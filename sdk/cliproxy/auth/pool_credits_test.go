package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// The live grants: 101 is spent, 5n expires first and so has the higher priority, then shahidy.
func TestCreditsFallThroughByPriorityThenToPlans(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	const low = "Your credit balance is too low to access the Anthropic API."
	rules := []internalconfig.RequestScopedErrorRule{{Status: 400, Match: []string{low}, Action: "continue-and-cooldown"}}
	credit := func(id, priority string) *Auth {
		return &Auth{ID: id, Provider: "claude", Prefix: "included", Status: StatusActive,
			Attributes: map[string]string{"api_key": id + "-key", "priority": priority},
			Metadata:   map[string]any{"request_scoped_errors": rules}}
	}
	m := NewManager(nil, nil, nil)
	reg := registry.GetGlobalRegistry()
	for _, a := range []*Auth{credit("101", "100"), credit("5n", "300"), credit("shahidy", "200")} {
		reg.RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
		t.Cleanup(func() { reg.UnregisterClient(a.ID) })
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	empty := map[string]bool{"101": true}
	var tried []string
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "claude", executeFn: func(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		tried = append(tried, a.ID)
		if empty[a.ID] {
			return cliproxyexecutor.Response{}, customStatusError{code: 400, msg: `{"type":"error","error":{"type":"invalid_request_error","message":"` + low + `"}}`}
		}
		return cliproxyexecutor.Response{Payload: []byte(a.ID)}, nil
	}})
	send := func() string {
		tried = nil
		resp, err := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-5-5"}, cliproxyexecutor.Options{})
		if err != nil {
			return "error"
		}
		return string(resp.Payload)
	}

	if got := send(); got != "5n" {
		t.Fatalf("first spend went to %s (tried %v), want 5n", got, tried)
	}
	empty["5n"] = true
	if got := send(); got != "shahidy" {
		t.Fatalf("after 5n ran out the request went to %s (tried %v), want shahidy", got, tried)
	}
	if runtime, _ := m.ClaudeRuntime("included"); runtime != "sdk" {
		t.Fatalf("shahidy still has balance, runtime = %s", runtime)
	}
	empty["shahidy"] = true
	if got := send(); got != "error" {
		t.Fatalf("every credit is empty but the request was served by %s", got)
	}
	if runtime, _ := m.ClaudeRuntime("included"); runtime != "interactive" {
		t.Fatalf("every credit is empty, runtime = %s", runtime)
	}
}
