package helps

import (
	"context"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestUsageReporterCapturesUpstreamIdentity(t *testing.T) {
	for _, tt := range []struct{ provider, payload, id string }{
		{"claude", `data: {"type":"message_start","message":{"id":"msg_1","model":"claude-opus-4-5"}}`, "msg_1"},
		{"claude", `{"type":"message","id":"msg_2","model":"claude-opus-4-5"}`, "msg_2"},
		{"codex", `data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5","status":"completed"}}`, "resp_1"},
	} {
		t.Run(tt.id, func(t *testing.T) {
			reporter := NewUsageReporter(context.Background(), tt.provider, "requested", &cliproxyauth.Auth{ID: "credential", Metadata: map[string]any{"account_uuid": "account"}})
			reporter.ObserveResponseModel([]byte(tt.payload))
			if reporter.responseID != tt.id || reporter.accountID != "account" {
				t.Fatalf("response=%q account=%q", reporter.responseID, reporter.accountID)
			}
			reporter.ObserveResponseModel([]byte(`data: {"type":"message_stop"}`))
			if reporter.responseID != tt.id {
				t.Fatal("terminal frame cleared identity")
			}
		})
	}
}
