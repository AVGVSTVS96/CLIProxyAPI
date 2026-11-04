package auth

import (
	"context"
	"testing"
	"time"
)

func TestClaudeRuntimeUsesCreditUntilAPlanHasRoom(t *testing.T) {
	resetPool(t)
	m := NewManager(nil, &QuotaSelector{}, nil)
	key := &Auth{ID: "credit", Provider: "claude", Prefix: "credit", Attributes: map[string]string{"api_key": "test-key"}}
	if _, err := m.Register(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if runtime, _ := m.ClaudeRuntime("credit"); runtime != "sdk" {
		t.Fatalf("runtime = %s", runtime)
	}
	plan := poolAuth("plan", 0, 99, 24*time.Hour)
	if _, err := m.Register(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if runtime, _ := m.ClaudeRuntime("credit"); runtime != "sdk" {
		t.Fatalf("exhausted plan selected: %s", runtime)
	}
	plan = poolAuth("plan", 0, 0, 7*24*time.Hour)
	if _, err := m.Update(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if runtime, _ := m.ClaudeRuntime("credit"); runtime != "interactive" {
		t.Fatalf("reset plan should require interactive, got %s", runtime)
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
