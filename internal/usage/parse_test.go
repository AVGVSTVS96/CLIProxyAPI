package usage

import (
	"strings"
	"testing"
)

func lines(records ...string) *strings.Reader { return strings.NewReader(strings.Join(records, "\n") + "\n") }

func TestClaudeRepeatsAndSidechainReplays(t *testing.T) {
	main, err := parseClaude(lines(
		`{"type":"custom-title","customTitle":"fix the bug"}`,
		`{"type":"assistant","sessionId":"s1","requestId":"r1","timestamp":"2026-01-01T00:00:00.000Z","cwd":"/w",`+
			`"message":{"id":"m1","model":"claude-opus-4-5","usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"assistant","sessionId":"s1","requestId":"r1","timestamp":"2026-01-01T00:00:01.000Z","cwd":"/w",`+
			`"message":{"id":"m1","model":"claude-opus-4-5","usage":{"input_tokens":10,"output_tokens":5,"service_tier":"standard"}}}`,
		`not json`,
	), "/p/s1.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(main.Events) != 1 || main.Events[0].Event.OutputTokens != 5 || main.Events[0].Tier != "standard" {
		t.Fatalf("repeated message lines collapse to the largest: %+v", main.Events)
	}
	if main.Malformed != 1 || main.SessionTitle != "fix the bug" {
		t.Fatalf("malformed %d title %q", main.Malformed, main.SessionTitle)
	}
	replay, _ := parseClaude(lines(
		`{"type":"assistant","isSidechain":true,"sessionId":"s1","requestId":"r9","timestamp":"2026-01-01T00:01:00.000Z",`+
			`"message":{"id":"m1","model":"claude-opus-4-5","usage":{"input_tokens":10,"cache_read_input_tokens":9000,"output_tokens":5}}}`,
	), "/p/s1/subagents/agent.jsonl")
	views := newViews([]*parsedFile{main, replay})
	entries, _ := deduplicate(views)
	if len(entries) != 1 || entries[0].Event.ID != "claude:message:m1:r1" {
		t.Fatalf("a /btw replay of the parent's message is dropped: %+v", entries)
	}
	if got := views[1].Retracted; len(got) != 1 || got[0] != "claude:message:m1:r9" {
		t.Fatalf("the replay's identity is retracted: %v", got)
	}
	if len(replay.Events) != 1 {
		t.Fatal("deduplication must not modify cached parsed files")
	}
}

func TestCodexCountersAndForks(t *testing.T) {
	parent, _ := parseCodex(lines(
		`{"type":"session_meta","timestamp":"2026-01-01T00:00:00.000Z","payload":{"id":"p1","cwd":"/w","model":"gpt-5"}}`,
		`{"type":"event_msg","timestamp":"2026-01-01T00:00:10.000Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10,"total_tokens":110}}}}`,
		`{"type":"event_msg","timestamp":"2026-01-01T00:00:20.000Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10,"total_tokens":110}}}}`,
		`{"type":"event_msg","timestamp":"2026-01-01T00:00:30.000Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":250,"cached_input_tokens":100,"output_tokens":30,"total_tokens":280}}}}`,
	), "/s/rollout-p1.jsonl")
	if len(parent.Events) != 2 {
		t.Fatalf("an unchanged total adds nothing: %d events", len(parent.Events))
	}
	second := parent.Events[1].Event
	if second.InputTokens != 90 || second.CacheReadTokens != 60 || second.OutputTokens != 20 || second.Project != "/w" {
		t.Fatalf("counters become per-request deltas: %+v", second)
	}
	child, _ := parseCodex(lines(
		`{"type":"session_meta","timestamp":"2026-01-01T00:00:25.000Z","payload":{"id":"c1","forked_from_id":"p1","cwd":"/w"}}`,
		`{"type":"event_msg","timestamp":"2026-01-01T00:00:10.000Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10,"total_tokens":110}}}}`,
		`{"type":"event_msg","timestamp":"2026-01-01T00:01:00.000Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":7,"output_tokens":3}}}}`,
	), "/s/rollout-c1.jsonl")
	entries, _ := deduplicate(newViews([]*parsedFile{parent, child}))
	if len(entries) != 3 {
		t.Fatalf("a fork keeps only its own requests: %d entries", len(entries))
	}
}

func TestPiForkOwnership(t *testing.T) {
	parent, _ := parsePi(lines(
		`{"type":"session","id":"aaa","cwd":"/w"}`,
		`{"type":"message","id":"e1","timestamp":"2026-01-01T00:00:00.000Z","message":{"role":"assistant","model":"m","usage":{"input":5,"output":1}}}`,
	), "/pi/2026_aaa.jsonl")
	fork, _ := parsePi(lines(
		`{"type":"session","id":"bbb","cwd":"/w","parentSession":"/pi/2026_aaa.jsonl","timestamp":"2026-01-01T00:05:00.000Z"}`,
		`{"type":"message","id":"e1","timestamp":"2026-01-01T00:00:00.000Z","message":{"role":"assistant","model":"m","usage":{"input":5,"output":1}}}`,
		`{"type":"message","id":"e2","timestamp":"2026-01-01T00:06:00.000Z","message":{"role":"assistant","model":"m","usage":{"input":2,"output":1}}}`,
	), "/pi/2026_bbb.jsonl")
	views := newViews([]*parsedFile{parent, fork})
	entries, _ := deduplicate(views)
	ids := map[string]bool{}
	for _, entry := range entries {
		ids[entry.Event.ID] = true
	}
	if len(entries) != 2 || !ids["pi:aaa:e1"] || !ids["pi:bbb:e2"] {
		t.Fatalf("inherited entries belong to the parent session: %v", ids)
	}
	if fork.Events[0].Event.ID != "pi:bbb:e1" {
		t.Fatal("deduplication must not modify cached parsed files")
	}
}
