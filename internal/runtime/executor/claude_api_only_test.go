package executor

import (
	"context"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"net/http"
	"testing"
)

func TestClaudeAPIOnlyNeverExecutesOnOAuth(t *testing.T) {
	executor := NewClaudeExecutor(nil)
	auth := &cliproxyauth.Auth{Provider: "claude", Metadata: map[string]any{"access_token": "sk-ant-oat-test"}}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Proxy-Api-Only": []string{"1"}}}
	req := cliproxyexecutor.Request{Model: "claude-haiku-5-5"}
	_, a := executor.Execute(context.Background(), auth, req, opts)
	_, b := executor.ExecuteStream(context.Background(), auth, req, opts)
	_, c := executor.CountTokens(context.Background(), auth, req, opts)
	for _, err := range []error{a, b, c} {
		if got, ok := err.(statusErr); !ok || got.code != http.StatusConflict {
			t.Fatalf("OAuth inference was not blocked: %v", err)
		}
	}
	if err := requireClaudeAPIKey(&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-ant-api-test"}}, opts); err != nil {
		t.Fatalf("API key rejected: %v", err)
	}
}
