package usage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func storeEvent(id string, output int64) Event {
	tier, write1h := "standard", int64(0)
	e := testEvent("claude", "claude-opus-4-5-20251101", 100, output, 0, 0)
	e.ID, e.ServiceTier, e.CacheWrite1hTokens = id, &tier, &write1h
	return e
}

func TestStoreAppliesPersistsAndReprices(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir, "server", "server", NewPricing(dir))
	if err != nil {
		t.Fatal(err)
	}
	notified := make(chan struct{}, 8)
	cancel := store.Subscribe(func() { notified <- struct{}{} })
	defer cancel()

	titled := storeEvent("a", 10)
	titled.SessionTitle, titled.T3ThreadID, titled.T3ThreadURL = "title", "t1", "https://app.t3.codes/e/t1"
	mac := Device{ID: "mac", Label: "mac"}
	if result, _ := store.Apply(Batch{Device: mac, Events: []Event{titled, storeEvent("b", 5)}}, false); result.Accepted != 2 {
		t.Fatalf("accepted %+v", result)
	}
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("subscriber not called")
	}
	corrected := storeEvent("a", 20)
	result, _ := store.Apply(Batch{Device: mac, Events: []Event{corrected, storeEvent("b", 5)}, DeletedIDs: []string{"missing"}}, false)
	if result.Updated != 1 || result.Accepted != 0 || result.Deleted != 0 {
		t.Fatalf("a correction replaces, an unchanged record is skipped: %+v", result)
	}
	if _, err = store.Apply(Batch{Device: mac, DeletedIDs: []string{"b"}}, false); err != nil {
		t.Fatal(err)
	}
	if result, _ = store.Apply(Batch{Device: mac, Events: []Event{storeEvent("a", 1), storeEvent("c", 1)}}, true); result.Accepted != 1 {
		t.Fatalf("an import only fills missing records: %+v", result)
	}

	reopened, err := OpenStore(dir, "server", "server", NewPricing(dir))
	if err != nil {
		t.Fatal(err)
	}
	events := reopened.DeviceEvents("mac")
	if len(events) != 2 {
		t.Fatalf("journal replay: %d records", len(events))
	}
	for _, e := range events {
		if e.ID == "a" && (e.OutputTokens != 20 || e.SessionTitle != "title" || e.T3ThreadURL == "") {
			t.Fatalf("metadata a collector couldn't read this time is kept: %+v", e)
		}
	}
	if devices := reopened.Devices(); devices[0].ID != "server" || devices[1].ID != "mac" || devices[1].SyncedAt == nil {
		t.Fatalf("devices %+v", devices)
	}

	before := reopened.Revision()
	priced := reopened.Events()
	if len(priced) != 2 || !priced[0].CostKnown {
		t.Fatalf("events %+v", priced)
	}
	if err = reopened.Pricing().SetRule(PricingRule{Model: "claude-opus-4-5-20251101", Kind: "free"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for reopened.Revision() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := reopened.Events(); after[0].CostUSD != 0 || !after[0].CostKnown {
		t.Fatalf("a pricing rule reprices stored records: %+v", after[0])
	}
}

func TestStoreKeepsOneCopyAcrossDevices(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenStore(dir, "server", "server", NewPricing(""))
	_, _ = store.Apply(Batch{Device: Device{ID: "server"}, Events: []Event{storeEvent("x", 5)}}, false)
	_, _ = store.Apply(Batch{Device: Device{ID: "mac"}, Events: []Event{storeEvent("x", 5), storeEvent("y", 1)}}, false)
	_, _ = store.Apply(Batch{Device: Device{ID: "zz"}, Events: []Event{storeEvent("y", 9)}}, false)
	events := store.Events()
	owners := map[string]string{}
	for _, e := range events {
		owners[e.ID] = e.DeviceID
	}
	if len(events) != 2 || owners["x"] != "server" || owners["y"] != "zz" {
		t.Fatalf("the larger copy wins, then the server's own: %v", owners)
	}
}

func TestSnapshotServesReadersUntilItAges(t *testing.T) {
	store, _ := OpenStore(t.TempDir(), "server", "server", NewPricing(""))
	store.SnapshotAge = time.Hour
	_, _ = store.Apply(Batch{Device: Device{ID: "server"}, Events: []Event{storeEvent("x", 5)}}, false)
	first, revision := store.Events(), store.Revision()
	_, _ = store.Apply(Batch{Device: Device{ID: "server"}, Events: []Event{storeEvent("y", 1)}}, false)
	if len(store.Events()) != len(first) || store.Revision() != revision {
		t.Fatal("a young snapshot answers both Events and Revision")
	}
	store.SnapshotAge = 0
	if len(store.Events()) != 2 || store.Revision() == revision {
		t.Fatal("an aged snapshot is rebuilt with the new revision")
	}
}

func TestPushIntoIngestEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	store, _ := OpenStore(dir, "server", "server", NewPricing(""))
	shared.Store(store)
	defer shared.Store(nil)
	engine := gin.New()
	engine.POST(IngestPath, IngestHandler)
	server := httptest.NewServer(engine)
	defer server.Close()

	events := make([]Event, pushBatchSize+3)
	for i := range events {
		events[i] = storeEvent(string(rune('a'+i%26))+time.Duration(i).String(), 1)
	}
	var acknowledged int
	err := sendBatches(context.Background(), server.URL+IngestPath, "key", Device{ID: "mac", Label: "mac"}, events, []string{"gone"},
		func(batch []Event, _ []string) { acknowledged += len(batch) })
	if err != nil || acknowledged != len(events) {
		t.Fatalf("err %v, acknowledged %d", err, acknowledged)
	}
	if got := len(store.DeviceEvents("mac")); got != len(events) {
		t.Fatalf("stored %d", got)
	}
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	_ = json.NewEncoder(zw).Encode(Batch{Device: Device{ID: "server"}})
	_ = zw.Close()
	response, err := http.Post(server.URL+IngestPath, "application/json", &body)
	if err == nil && response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a push may not write the server's own device: %d", response.StatusCode)
	}
}
