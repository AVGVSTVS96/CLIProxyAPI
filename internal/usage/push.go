package usage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"math"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// IngestPath is the management endpoint other machines push their records to.
const IngestPath = "/v0/management/usage/events"

const pushBatchSize = 2000

// Push sends this machine's records to the server at serverURL, then keeps sending new
// and corrected ones as its logs change, until ctx ends. The server keeps whatever it
// received, so removing local logs never removes history.
func Push(ctx context.Context, cfg config.UsageConfig, serverURL string) error {
	key := strings.TrimSpace(cfg.PushKey)
	if key == "" {
		return errors.New("usage.push-key is not set")
	}
	m, err := openMachine(cfg)
	if err != nil {
		return err
	}
	unlock, err := lockState(m.stateDir)
	if err != nil {
		return err
	}
	defer unlock()
	endpoint := strings.TrimRight(serverURL, "/") + IngestPath
	device := Device{ID: m.id, Label: m.label}
	sent := map[string]uint64{}
	withdrawn := map[string]bool{}
	pending := func(collection Collection) (changed []Event, deleted []string) {
		for _, e := range collection.Events {
			if digest, ok := sent[e.ID]; !ok || digest != eventDigest(&e) {
				changed = append(changed, e)
			}
		}
		for _, id := range collection.RetractedIDs {
			if !withdrawn[id] {
				deleted = append(deleted, id)
			}
		}
		return changed, deleted
	}
	pass := func() {
		collection := m.collector.Collect(ctx)
		for backoff := time.Second; ; backoff = min(2*backoff, time.Minute) {
			changed, deleted := pending(collection)
			err := sendBatches(ctx, endpoint, key, device, changed, deleted, func(events []Event, ids []string) {
				for i := range events {
					sent[events[i].ID] = eventDigest(&events[i])
				}
				for _, id := range ids {
					withdrawn[id] = true
				}
			})
			if err == nil {
				log.Debugf("usage: pushed %d records and %d withdrawals", len(changed), len(deleted))
				return
			}
			log.WithError(err).Warn("usage: push failed; retrying")
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}
	pass()
	debug.FreeOSMemory()
	log.Infof("usage: pushing %s (%s) to %s", m.label, m.id, serverURL)
	return follow(ctx, m.collector.Roots(), pass)
}

var digestSeed = maphash.MakeSeed()

// eventDigest fingerprints every field of a record, to tell which ones changed since sent.
func eventDigest(e *Event) uint64 {
	var h maphash.Hash
	h.SetSeed(digestSeed)
	str := func(s string) { _, _ = h.WriteString(s); _ = h.WriteByte(0) }
	num := func(n int64) { _, _ = h.WriteString(strconv.FormatInt(n, 36)); _ = h.WriteByte(0) }
	for _, s := range []string{e.ID, e.Timestamp, e.Harness, e.Model, e.RawModel, e.Project, e.SessionID, e.DeviceID,
		e.SessionTitle, e.ProjectName, e.T3ThreadID, e.T3ThreadURL} {
		str(s)
	}
	for _, p := range []*string{e.Repository, e.ServiceTier} {
		if p == nil {
			str("\x01nil")
		} else {
			str(*p)
		}
	}
	for _, n := range []int64{e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens,
		int64(math.Float64bits(e.CostUSD))} {
		num(n)
	}
	for _, p := range []*int64{e.CacheWrite1hTokens, e.Requests} {
		if p == nil {
			str("\x01nil")
		} else {
			num(*p)
		}
	}
	if e.ReportedCostUSD == nil {
		str("\x01nil")
	} else {
		num(int64(math.Float64bits(*e.ReportedCostUSD)))
	}
	if e.CostKnown {
		str("known")
	}
	return h.Sum64()
}

// sendBatches posts records and withdrawals in bounded batches, reporting each
// acknowledged batch. An empty change still posts once, so the server sees the device.
func sendBatches(ctx context.Context, endpoint, key string, device Device, events []Event, deleted []string,
	acknowledged func([]Event, []string)) error {
	for first := true; first || len(events) > 0 || len(deleted) > 0; first = false {
		n := min(len(events), pushBatchSize)
		d := min(len(deleted), pushBatchSize-n)
		batch := Batch{Device: device, Events: events[:n], DeletedIDs: deleted[:d]}
		if err := postBatch(ctx, endpoint, key, batch); err != nil {
			return err
		}
		acknowledged(batch.Events, batch.DeletedIDs)
		events, deleted = events[n:], deleted[d:]
	}
	return nil
}

func postBatch(ctx context.Context, endpoint, key string, batch Batch) error {
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	if err := json.NewEncoder(zw).Encode(batch); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	return nil
}

// IngestHandler stores a batch another machine pushed. It sits behind management auth.
func IngestHandler(c *gin.Context) {
	store := Shared()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage is not enabled on this server"})
		return
	}
	body := http.MaxBytesReader(c.Writer, c.Request.Body, 256<<20)
	var reader io.Reader = body
	if strings.EqualFold(c.GetHeader("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid gzip body"})
			return
		}
		defer func() { _ = zr.Close() }()
		reader = io.LimitReader(zr, 1<<30)
	}
	var batch Batch
	if err := json.NewDecoder(reader).Decode(&batch); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage batch"})
		return
	}
	if message := validateBatch(batch, store.localID); message != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": message})
		return
	}
	result, err := store.Apply(batch, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not save usage"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accepted": result.Accepted, "updated": result.Updated, "deleted": result.Deleted,
		"receivedAt": isoNow()})
}

func validateBatch(batch Batch, localID string) string {
	id := strings.TrimSpace(batch.Device.ID)
	switch {
	case id == "" || len(id) > 200 || len(batch.Device.Label) > 200:
		return "A valid device id is required."
	case id == localID:
		return "The server collects its own logs."
	case len(batch.Events)+len(batch.DeletedIDs) > 10_000:
		return "A batch can contain at most 10,000 records."
	}
	for i := range batch.Events {
		e := &batch.Events[i]
		values := []float64{float64(e.InputTokens), float64(e.OutputTokens), float64(e.CacheReadTokens),
			float64(e.CacheWriteTokens), float64(e.ReasoningTokens), e.CostUSD}
		if e.ReportedCostUSD != nil {
			values = append(values, *e.ReportedCostUSD)
		}
		if e.Requests != nil {
			values = append(values, float64(*e.Requests))
		}
		if e.ID == "" || math.IsNaN(parseDate(e.Timestamp)) {
			return "Usage records need a stable id and a valid timestamp."
		}
		for _, v := range values {
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				return "Usage records need nonnegative token counts and cost."
			}
		}
	}
	return ""
}
