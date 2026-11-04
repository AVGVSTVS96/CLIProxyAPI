package auth

import (
	"context"
	"testing"
	"time"
)

func TestClaudeRuntimeUsesCreditBeforeAnyPlan(t *testing.T) {
	resetPool(t)
	m := NewManager(nil, &QuotaSelector{}, nil)
	key := &Auth{ID: "credit", Provider: "claude", Prefix: "credit", Attributes: map[string]string{"api_key": "test-key"}}
	plan := poolAuth("plan", 0, 0, 7*24*time.Hour)
	for _, a := range []*Auth{key, plan} {
		if _, err := m.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	if runtime, _ := m.ClaudeRuntime("credit"); runtime != "sdk" {
		t.Fatalf("a plan with room was chosen over a credit with balance: %s", runtime)
	}
}

func TestClaudeRuntimeExhaustedCreditRequiresInteractive(t *testing.T) {
	resetPool(t)
	m := NewManager(nil, &QuotaSelector{}, nil)
	key := &Auth{ID: "credit", Provider: "claude", Prefix: "credit", Attributes: map[string]string{"api_key": "test-key"}, LastError: &Error{Message: "Your credit balance is too low to access the Anthropic API."}}
	if _, err := m.Register(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if runtime, _ := m.ClaudeRuntime("credit"); runtime != "interactive" {
		t.Fatalf("runtime = %s", runtime)
	}
	if runtime, _ := m.ClaudeRuntime(""); runtime != "interactive" {
		t.Fatalf("empty prefix = %s", runtime)
	}
}
