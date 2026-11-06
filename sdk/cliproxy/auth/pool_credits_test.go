package auth

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Free credit runs by priority: an empty grant is skipped, then the one expiring first, then the next.
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
	for _, a := range []*Auth{credit("spent", "100"), credit("first", "300"), credit("second", "200")} {
		reg.RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: "claude-opus-5-5"}})
		t.Cleanup(func() { reg.UnregisterClient(a.ID) })
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	empty := map[string]bool{"spent": true}
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

	if got := send(); got != "first" {
		t.Fatalf("first spend went to %s (tried %v), want first", got, tried)
	}
	empty["first"] = true
	if got := send(); got != "second" {
		t.Fatalf("after first ran out the request went to %s (tried %v), want second", got, tried)
	}
	empty["second"] = true
	if got := send(); got != "error" {
		t.Fatalf("every credit is empty but the request was served by %s", got)
	}
}

// A bare model runs on a plan even while an API credit has balance: credit is reached only by
// asking for it under its prefix.
func TestBareModelUsesThePlanWhileCreditHasBalance(t *testing.T) {
	credit := &Auth{ID: "credit", Provider: "claude", Prefix: "included", Status: StatusActive,
		Attributes: map[string]string{"api_key": "credit-key", "priority": "300"}}
	plan := &Auth{ID: "plan", Provider: "claude", Status: StatusActive, Metadata: map[string]any{"email": "plan@example.com"}}
	m := NewManager(nil, nil, nil)
	reg := registry.GetGlobalRegistry()
	for _, a := range []*Auth{credit, plan} {
		model := "claude-opus-5-5"
		if a.Prefix != "" {
			model = a.Prefix + "/" + model
		}
		reg.RegisterClient(a.ID, "claude", []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { reg.UnregisterClient(a.ID) })
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	m.RegisterExecutor(&mockCustomErrorExecutor{identifier: "claude", executeFn: func(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{Payload: []byte(a.ID)}, nil
	}})
	resp, err := m.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: "claude-opus-5-5"}, cliproxyexecutor.Options{})
	if err != nil || string(resp.Payload) != "plan" {
		t.Fatalf("bare model went to %q (err %v), want the plan", resp.Payload, err)
	}
}
