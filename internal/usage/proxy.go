package usage

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/entrypoint"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

func init() { coreusage.RegisterNamedPlugin("dashboard", proxySink{}) }

type proxySink struct{}

var receipts atomic.Pointer[string]

// SetReceipts makes every proxied request append its records to path, one per line; empty stops it.
func SetReceipts(path string) {
	path = expandHome(strings.TrimSpace(path))
	receipts.Store(&path)
}

func (proxySink) HandleUsage(ctx context.Context, record coreusage.Record) {
	store := Shared()
	var path string
	if p := receipts.Load(); p != nil {
		path = *p
	}
	if store == nil && path == "" {
		return
	}
	events := proxyEvents(ctx, record)
	if store != nil {
		if _, err := store.Apply(Batch{Device: Device{ID: store.localID, Label: store.label}, Events: events}, false); err != nil {
			log.WithError(err).Error("usage: could not persist proxy request")
		}
	}
	if path != "" {
		if err := appendReceipts(path, events); err != nil {
			log.WithError(err).Error("usage: could not write proxy receipt")
		}
	}
}

// appendReceipts writes events in one append, so readers never see half a request.
func appendReceipts(path string, events []Event) error {
	var lines []byte
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			return err
		}
		lines = append(append(lines, data...), '\n')
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(lines)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func proxyEvents(ctx context.Context, record coreusage.Record) []Event {
	primary := proxyEvent(ctx, record)
	events := []Event{primary}
	if record.Provider != "claude" {
		return events
	}
	ordinal := 0
	for _, iteration := range record.Detail.IterationSlice() {
		if iteration.Type != "advisor_message" {
			continue
		}
		index := ordinal
		ordinal++
		if iteration.Model == "" || iteration.Model == "<synthetic>" {
			continue
		}
		advisor := record
		advisor.Model, advisor.ResponseModel = iteration.Model, iteration.Model
		advisor.Detail = coreusage.Detail{InputTokens: intValue(iteration.InputTokens), OutputTokens: intValue(iteration.OutputTokens),
			CacheReadTokens: intValue(iteration.CacheReadInputTokens), CacheCreationTokens: intValue(iteration.CacheCreationInputTokens),
			ReasoningTokens: intValue(iteration.ReasoningTokens), CacheCreation1hTokens: iteration.CacheCreation1hTokens}
		event := proxyEvent(ctx, advisor)
		event.ID = primary.ID + ":advisor:" + strconv.Itoa(index)
		event.ResponseID, event.UpstreamRequestID = "", ""
		if primary.ResponseID != "" && primary.UpstreamRequestID != "" {
			original := "claude:message:" + primary.ResponseID + ":" + primary.UpstreamRequestID
			event.ResponseID = "advisor:" + sha256Hex(jsonArray(original, index))
		}
		if tokenTotal(&event) > 0 {
			zero := int64(0)
			event.Requests = &zero
			events = append(events, event)
		}
	}
	return events
}

func intValue(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func proxyEvent(ctx context.Context, record coreusage.Record) Event {
	metadata := internallogging.GetClientRequestMetadata(ctx)
	via := entrypoint.FromUserAgent(metadata.UserAgent)
	harness := strings.TrimSpace(record.Provider)
	if harness == "" {
		harness = "proxy"
	}
	session := record.SessionID
	if session == "" {
		session = metadata.SessionID
	}
	if session == "" {
		session = "proxy:" + record.RequestID
	}
	project := via
	if project == "" {
		project = "Proxy requests"
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	model := record.ResponseModel
	if model == "" {
		model = record.Model
	}
	detail := coreusage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	breakdown := detail.TokenBreakdown
	event := Event{
		Source: "proxy", ID: "proxy:" + record.RequestID + ":" + record.Model,
		ResponseID: record.ResponseID, UpstreamRequestID: record.ResponseHeaders.Get("request-id"),
		AccountID: record.AccountID, Failed: record.Failed,
		Timestamp: at.UTC().Format(time.RFC3339Nano), Harness: harness,
		Model: model, Project: project, SessionID: session, Entrypoint: via,
		InputTokens: breakdown.Input.UncachedTokens, OutputTokens: breakdown.Output.TotalTokens,
		CacheReadTokens: breakdown.Input.CacheReadTokens, CacheWriteTokens: breakdown.Input.CacheWriteTokens,
		ReasoningTokens: breakdown.Output.ReasoningTokens, UnclassifiedTokens: breakdown.UnclassifiedTokens, CacheWrite1hTokens: detail.CacheCreation1hTokens,
	}
	if event.UpstreamRequestID == "" {
		event.UpstreamRequestID = record.ResponseHeaders.Get("x-request-id")
	}
	tier := record.ResponseServiceTier
	if tier == "" {
		tier = record.EffectiveServiceTier
	}
	if tier == "" {
		tier = record.ServiceTier
	}
	event.ServiceTier = &tier
	if event.CacheWrite1hTokens == nil {
		zero := int64(0)
		event.CacheWrite1hTokens = &zero
	}
	return event
}

type proxyIdentity struct{ harness, kind, id, model string }

func responseIdentity(e Event) string {
	if e.ResponseID != "" {
		return e.ResponseID
	}
	if e.Harness == "claude" && strings.HasPrefix(e.ID, "claude:advisor:") {
		return "advisor:" + strings.TrimPrefix(e.ID, "claude:advisor:")
	}
	if e.Harness == "claude" && strings.HasPrefix(e.ID, "claude:message:") {
		id, _, _ := strings.Cut(strings.TrimPrefix(e.ID, "claude:message:"), ":")
		return id
	}
	if e.Harness == "codex" && strings.HasPrefix(e.ID, "codex:response:") {
		return strings.TrimPrefix(e.ID, "codex:response:")
	}
	return ""
}

func requestIdentity(e Event) string {
	if e.UpstreamRequestID != "" {
		return e.UpstreamRequestID
	}
	if e.Harness == "claude" && strings.HasPrefix(e.ID, "claude:message:") {
		_, id, _ := strings.Cut(strings.TrimPrefix(e.ID, "claude:message:"), ":")
		return id
	}
	return ""
}

func proxyIdentities(e Event) []proxyIdentity {
	var keys []proxyIdentity
	if id := responseIdentity(e); id != "" {
		keys = append(keys, proxyIdentity{harness: e.Harness, kind: "response", id: id})
	}
	if id := requestIdentity(e); id != "" {
		keys = append(keys, proxyIdentity{harness: e.Harness, kind: "request", id: id, model: e.Model})
	}
	return keys
}

// reconcileProxy keeps wire accounting and enriches it with transcript metadata.
// Only exact upstream identities join requests; a session never hides its history.
func reconcileProxy(events []Event) []Event {
	byIdentity := map[proxyIdentity]int{}
	type sessionKey struct{ harness, session string }
	sessions := map[sessionKey]Event{}
	for i, e := range events {
		if e.Source == "proxy" {
			for _, key := range proxyIdentities(e) {
				byIdentity[key] = i
			}
		} else {
			key := sessionKey{e.Harness, e.SessionID}
			previous, exists := sessions[key]
			if !exists || e.Timestamp > previous.Timestamp {
				sessions[key] = e
			}
		}
	}
	removed := make([]bool, len(events))
	for i, e := range events {
		if e.Source == "proxy" {
			continue
		}
		for _, key := range proxyIdentities(e) {
			if index, ok := byIdentity[key]; ok {
				events[index] = proxyMetadata(events[index], e)
				removed[i] = true
				break
			}
		}
	}
	out := events[:0]
	for i, e := range events {
		if removed[i] {
			continue
		}
		if e.Source == "proxy" {
			if metadata, ok := sessions[sessionKey{e.Harness, e.SessionID}]; ok {
				e = proxyMetadata(e, metadata)
			}
		}
		out = append(out, e)
	}
	return out
}

func proxyMetadata(wire, transcript Event) Event {
	wire.Project, wire.Repository = transcript.Project, transcript.Repository
	wire.DeviceID = transcript.DeviceID
	wire.SessionID = transcript.SessionID
	if transcript.SessionTitle != "" {
		wire.SessionTitle = transcript.SessionTitle
	}
	wire.ProjectName = transcript.ProjectName
	wire.T3ThreadID, wire.T3ThreadURL = transcript.T3ThreadID, transcript.T3ThreadURL
	if wire.Entrypoint == "" {
		wire.Entrypoint = transcript.Entrypoint
	}
	return wire
}
