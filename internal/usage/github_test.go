package usage

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func usageZip(t *testing.T, doc any) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create("claude-usage.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.NewEncoder(f).Encode(doc); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestGitHubReaderCopiesNewArtifactsOnce(t *testing.T) {
	const repo = "me/fork"
	fresh := testEvent("claude", "claude-opus-4-5-20251101", 100, 10, 0, 0)
	fresh.ID = "github:new"
	stored := storeEvent("github:old", 99)
	downloads := map[string]int{}
	var mux http.ServeMux
	mux.HandleFunc("/user/repos", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"full_name":"me/fork"},{"full_name":"me/old","archived":true}]`))
	})
	mux.HandleFunc("/repos/me/fork/actions/artifacts", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(`{"artifacts":[
			{"id":3,"name":"claude-usage-3-1-sync","created_at":"2026-11-01T10:00:00Z"},
			{"id":2,"name":"claude-usage-2-1-sync","created_at":"2026-10-31T10:00:00Z"},
			{"id":4,"name":"claude-usage-4-1-sync","expired":true},
			{"id":5,"name":"build-output"}]}`))
	})
	mux.HandleFunc("/repos/me/fork/actions/artifacts/", func(w http.ResponseWriter, r *http.Request) {
		downloads[r.URL.Path]++
		events := []Event{}
		if strings.Contains(r.URL.Path, "/3/") {
			events = []Event{fresh, stored}
		}
		_, _ = w.Write(usageZip(t, map[string]any{"version": 1, "repository": repo, "events": events}))
	})
	server := httptest.NewServer(&mux)
	defer server.Close()

	dir := t.TempDir()
	store, err := OpenStore(dir, "server", "server", NewPricing(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Apply(Batch{Device: Device{ID: "github:" + repo}, Events: []Event{stored}}, true); err != nil {
		t.Fatal(err)
	}
	reader := newGitHubReader(dir, store)
	reader.api = server.URL
	reader.token = func(context.Context) (string, error) { return "token", nil }
	for range 2 {
		if err = reader.pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(downloads) != 2 || downloads["/repos/me/fork/actions/artifacts/3/zip"] != 1 {
		t.Fatalf("downloads %v", downloads)
	}
	events := store.DeviceEvents("github:" + repo)
	if len(events) != 2 {
		t.Fatalf("events %+v", events)
	}
	for _, e := range events {
		if e.ID == "github:new" && (e.ServiceTier == nil || !e.CostKnown || e.CostUSD == 0) {
			t.Fatalf("new record not priced: %+v", e)
		}
		if e.ID == "github:old" && e.OutputTokens != 99 {
			t.Fatalf("stored record was replaced: %+v", e)
		}
	}
	if devices := store.Devices(); devices[1].Label != "GitHub me/fork" || devices[1].SyncedAt == nil {
		t.Fatalf("devices %+v", devices)
	}
}
