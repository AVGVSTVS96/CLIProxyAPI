package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestProxyCapturesTranscriptlessAndFailedRequests(t *testing.T) {
	ctx := internallogging.WithClientRequestMetadata(context.Background(), internallogging.ClientRequestMetadata{
		UserAgent: "claude-cli/2.1.0 (external, microscope)", SessionID: "review-session",
	})
	at := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	event := proxyEvent(ctx, coreusage.Record{
		RequestID: "execution", ResponseID: "msg_review", AccountID: "account", Provider: "claude",
		Model: "claude-opus-4-5-20251101", RequestedAt: at, Failed: true,
		ResponseHeaders: http.Header{"Request-Id": {"req_review"}},
		Detail:          coreusage.Detail{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 300, CacheCreationTokens: 50},
	})
	if event.Entrypoint != "microscope" || event.Project != "microscope" || event.SessionID != "review-session" || !event.Failed {
		t.Fatalf("caller/partial failure missing: %+v", event)
	}
	if event.InputTokens != 100 || event.OutputTokens != 20 || event.CacheReadTokens != 300 || event.CacheWriteTokens != 50 {
		t.Fatalf("independent Claude buckets changed: %+v", event)
	}
	if event.ResponseID != "msg_review" || event.UpstreamRequestID != "req_review" || event.AccountID != "account" {
		t.Fatalf("identity missing: %+v", event)
	}
	codex := proxyEvent(ctx, coreusage.Record{RequestID: "codex", Provider: "codex", Detail: coreusage.Detail{
		InputTokens: 100, OutputTokens: 20, CacheReadTokens: 60, ReasoningTokens: 5,
	}})
	if codex.InputTokens != 40 || codex.CacheReadTokens != 60 || codex.OutputTokens != 20 || codex.ReasoningTokens != 5 {
		t.Fatalf("subset buckets counted twice: %+v", codex)
	}
	zero := proxyEvent(context.Background(), coreusage.Record{RequestID: "zero", Failed: true})
	if tokenTotal(&zero) != 0 || zero.SessionID == "" || zero.ID == "" {
		t.Fatalf("zero-usage attempt lost: %+v", zero)
	}
}

func TestProxyTranscriptReconciliationPersistsBothSources(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir, "server", "server", NewPricing(dir))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseClaude(strings.NewReader(`{"type":"assistant","timestamp":"2026-11-03T00:00:01Z","sessionId":"session","requestId":"req_1","cwd":"/work/project","message":{"id":"msg_1","model":"claude-opus-4-5-20251101","usage":{"input_tokens":100,"output_tokens":10}}}`), "/work/session.jsonl")
	if err != nil || len(parsed.Events) != 1 {
		t.Fatalf("parse: %+v %v", parsed, err)
	}
	transcript := parsed.Events[0].Event
	transcript.SessionTitle = "Code review"
	wire := Event{Source: "proxy", ID: "proxy:one", ResponseID: "msg_1", UpstreamRequestID: "req_1", AccountID: "actual-account",
		Timestamp: "2026-11-03T00:00:00Z", Harness: "claude", Model: transcript.Model, SessionID: "session",
		Project: "microscope", Entrypoint: "microscope", InputTokens: 100, OutputTokens: 20}
	historical := transcript
	historical.ID = "claude:message:msg_earlier:req_earlier"
	historical.OutputTokens = 5
	if _, err = store.Apply(Batch{Device: Device{ID: "server"}, Events: []Event{wire}}, false); err != nil {
		t.Fatal(err)
	}
	if got := store.Events(); len(got) != 1 || got[0].Project != "microscope" {
		t.Fatalf("transcriptless: %+v", got)
	}
	if _, err = store.Apply(Batch{Device: Device{ID: "mac"}, Events: []Event{transcript, historical}}, false); err != nil {
		t.Fatal(err)
	}
	check := func(s *FileStore) {
		t.Helper()
		got := s.Events()
		if len(got) != 2 {
			t.Fatalf("matched response counted twice, or earlier history hidden: %+v", got)
		}
		var seen bool
		for _, e := range got {
			if e.Source == "proxy" {
				seen = true
				if e.OutputTokens != 20 || e.AccountID != "actual-account" || e.Project != "/work/project" || e.SessionTitle != "Code review" || e.DeviceID != "mac" || e.Entrypoint != "microscope" {
					t.Fatalf("wrong reconciliation: %+v", e)
				}
			}
		}
		if !seen {
			t.Fatal("wire accounting lost")
		}
	}
	check(store)
	reopened, err := OpenStore(dir, "server", "server", NewPricing(dir))
	if err != nil {
		t.Fatal(err)
	}
	check(reopened)
	// Raw journals retain both records; reconciliation is reversible when a transcript changes.
	if len(reopened.DeviceEvents("server")) != 1 || len(reopened.DeviceEvents("mac")) != 2 {
		t.Fatal("raw evidence discarded")
	}
}

func TestProxyReconcilesExactRequestIdentityOnly(t *testing.T) {
	tests := []struct {
		name             string
		wire, transcript Event
		want             int
	}{
		{"header fallback", Event{Source: "proxy", Harness: "claude", Model: "opus", UpstreamRequestID: "req_1"}, Event{Harness: "claude", Model: "opus", ID: "claude:message:msg_1:req_1"}, 1},
		{"codex native", Event{Source: "proxy", Harness: "codex", ResponseID: "resp_1"}, Event{Harness: "codex", ID: "codex:response:resp_1"}, 1},
		{"different response", Event{Source: "proxy", Harness: "claude", ResponseID: "msg_1"}, Event{Harness: "claude", ID: "claude:message:msg_2:req_2"}, 2},
		{"no identity", Event{Source: "proxy", Harness: "claude", SessionID: "same"}, Event{Harness: "claude", SessionID: "same"}, 2},
		{"advisor retained", Event{Source: "proxy", Harness: "claude", ResponseID: "msg_1"}, Event{Harness: "claude", ID: "claude:advisor:hash"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reconcileProxy([]Event{tt.wire, tt.transcript}); len(got) != tt.want {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestProxyDispatcherDrainsIntoJournal(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir, "server", "server", NewPricing(dir))
	if err != nil {
		t.Fatal(err)
	}
	previous := shared.Swap(store)
	defer shared.Store(previous)
	manager := coreusage.NewManager(1)
	manager.Register(proxySink{})
	ctx := internallogging.WithClientRequestMetadata(context.Background(), internallogging.ClientRequestMetadata{UserAgent: "claude-cli/2 (external, microscope)", SessionID: "session"})
	manager.Publish(ctx, coreusage.Record{RequestID: "persisted", Provider: "claude", Model: "claude-opus-4-5-20251101", Detail: coreusage.Detail{InputTokens: 100, OutputTokens: 20}})
	manager.Stop()
	reopened, err := OpenStore(dir, "server", "server", NewPricing(dir))
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Events()
	if len(got) != 1 || got[0].Source != "proxy" || got[0].Project != "microscope" || !got[0].CostKnown || got[0].CostUSD <= 0 {
		t.Fatalf("drain did not persist priced caller: %+v", got)
	}
}

func TestProxyAdvisorUsageReconcilesWithTranscript(t *testing.T) {
	raw := `{"type":"assistant","timestamp":"2026-11-03T00:00:00Z","sessionId":"s","requestId":"req_1","message":{"id":"msg_1","model":"claude-sonnet-4-5","usage":{"input_tokens":100,"output_tokens":20,"iterations":[{"type":"message","model":"claude-sonnet-4-5","input_tokens":100,"output_tokens":20},{"type":"advisor_message","model":"claude-opus-4-5","input_tokens":30,"output_tokens":5}]}}}`
	parsed, err := parseClaude(strings.NewReader(raw), "s.jsonl")
	if err != nil || len(parsed.Events) != 2 {
		t.Fatalf("parse %+v %v", parsed, err)
	}
	iterations := []coreusage.IterationUsage{{Type: "message", Model: "claude-sonnet-4-5"}, {Type: "advisor_message", Model: "claude-opus-4-5", InputTokens: coreusage.Int64Ptr(30), OutputTokens: coreusage.Int64Ptr(5)}}
	events := proxyEvents(context.Background(), coreusage.Record{RequestID: "run", ResponseID: "msg_1", SessionID: "s", Provider: "claude", Model: "claude-sonnet-4-5", ResponseHeaders: http.Header{"Request-Id": {"req_1"}}, Detail: coreusage.Detail{InputTokens: 100, OutputTokens: 20, Iterations: &iterations}})
	if len(events) != 2 || events[1].Requests == nil || *events[1].Requests != 0 {
		t.Fatalf("advisor missing or counted as wire request: %+v", events)
	}
	for _, entry := range parsed.Events {
		events = append(events, entry.Event)
	}
	got := reconcileProxy(events)
	total := int64(0)
	for _, e := range got {
		total += tokenTotal(&e)
	}
	if len(got) != 2 || total != 155 {
		t.Fatalf("advisor lost or doubled: %+v total=%d", got, total)
	}
}

func TestProxyPreservesUnclassifiedUsage(t *testing.T) {
	e := proxyEvent(context.Background(), coreusage.Record{RequestID: "unknown", Provider: "custom-provider", Model: "claude-opus-4-5-20251101", Detail: coreusage.Detail{TotalTokens: 123}})
	if tokenTotal(&e) != 123 || e.UnclassifiedTokens != 123 {
		t.Fatalf("unclassified usage dropped: %+v", e)
	}
	priced, _ := NewPricing(t.TempDir()).Policy().Reprice(e)
	if priced.CostKnown {
		t.Fatal("unclassified tokens presented as known zero cost")
	}
}

func TestProxyReceiptsWithoutStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts.ndjson")
	previous := shared.Swap(nil)
	defer shared.Store(previous)
	SetReceipts(path)
	defer SetReceipts("")
	manager := coreusage.NewManager(1)
	manager.Register(proxySink{})
	iterations := []coreusage.IterationUsage{{Type: "message", Model: "claude-sonnet-4-5"}, {Type: "advisor_message", Model: "claude-opus-4-5", InputTokens: coreusage.Int64Ptr(30), OutputTokens: coreusage.Int64Ptr(5)}}
	record := coreusage.Record{RequestID: "run", ResponseID: "msg_1", SessionID: "s", Provider: "claude", Model: "claude-sonnet-4-5",
		RequestedAt: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), ResponseHeaders: http.Header{"Request-Id": {"req_1"}},
		Detail: coreusage.Detail{InputTokens: 100, OutputTokens: 20, Iterations: &iterations}}
	second := coreusage.Record{RequestID: "second", Provider: "codex", Model: "gpt-5", RequestedAt: record.RequestedAt, Detail: coreusage.Detail{InputTokens: 7}}
	manager.Publish(context.Background(), record)
	manager.Publish(context.Background(), second)
	manager.Stop()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for _, e := range append(proxyEvents(context.Background(), record), proxyEvent(context.Background(), second)) {
		line, _ := json.Marshal(e)
		want = append(append(want, line...), '\n')
	}
	if string(data) != string(want) {
		t.Fatalf("receipts:\n%s\nwant:\n%s", data, want)
	}
}
