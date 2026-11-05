package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const claudeIdentityVerificationBody = `{"type":"error","error":{"type":"invalid_request_error","message":"Identity verification is required to continue."}}`

func TestClassifyClaudeUpstreamError_IdentityVerificationIsAccountRefusal(t *testing.T) {
	err := classifyClaudeUpstreamError(http.StatusBadRequest, nil, []byte(claudeIdentityVerificationBody))
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", err)
	}
	if got := gjson.Get(err.Error(), "error.type").String(); got != "permission_error" {
		t.Fatalf("error.type = %q, want permission_error", got)
	}
	if got := gjson.Get(err.Error(), "error.message").String(); got != "Identity verification is required to continue." {
		t.Fatalf("error.message = %q, want upstream message kept", got)
	}

	plain := classifyClaudeUpstreamError(http.StatusBadRequest, nil, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages: field required"}}`))
	if status, ok := plain.(interface{ StatusCode() int }); !ok || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("ordinary 400 = %v, want it left as 400", plain)
	}
}

func TestClaudeExecutor_AuthManager_IdentityVerificationFailsOverAndCoolsAccount(t *testing.T) {
	var blockedAttempts, healthyAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("X-Api-Key")+r.Header.Get("Authorization"), "blocked") {
			blockedAttempts.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(claudeIdentityVerificationBody))
			return
		}
		healthyAttempts.Add(1)
		_, _ = w.Write([]byte(`{"id":"msg-ok","type":"message","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer server.Close()

	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(NewClaudeExecutor(&config.Config{}))

	reg := registry.GetGlobalRegistry()
	blockedID := uuid.NewString() + "-identity-blocked"
	for _, auth := range []*cliproxyauth.Auth{
		{ID: blockedID, Provider: "claude", Attributes: map[string]string{"api_key": "blocked-key", "base_url": server.URL}},
		{ID: uuid.NewString() + "-identity-healthy", Provider: "claude", Attributes: map[string]string{"api_key": "healthy-key", "base_url": server.URL}},
	} {
		reg.RegisterClient(auth.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5"}})
		t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth: %v", err)
		}
	}

	payload := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":[{"type":"text","text":"test"}]}]}`)
	for i := range 3 {
		_, err := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{
			Model:   "claude-opus-5",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
		if err != nil {
			t.Fatalf("request %d: %v, want failover to the healthy account", i, err)
		}
	}
	if got := blockedAttempts.Load(); got != 1 {
		t.Fatalf("blocked account attempts = %d, want 1 before it leaves rotation", got)
	}
	if got := healthyAttempts.Load(); got != 3 {
		t.Fatalf("healthy account attempts = %d, want 3", got)
	}

	blocked, ok := manager.GetByID(blockedID)
	if !ok || blocked == nil || blocked.ModelStates["claude-opus-5"] == nil {
		t.Fatal("blocked account model state not found")
	}
	if wait := time.Until(blocked.ModelStates["claude-opus-5"].NextRetryAfter); wait < 25*time.Minute {
		t.Fatalf("blocked account cooldown = %v, want ~30m", wait)
	}
}
