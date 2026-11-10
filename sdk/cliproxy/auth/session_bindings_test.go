package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionCacheRestoreKeepsLiveBindingsAndOrder(t *testing.T) {
	old := NewSessionCache(time.Hour)
	defer old.Stop()
	old.setSessionAliases("auth-a", "claude", "s1", "claude::s1::opus")
	old.setSessionAliases("auth-b", "claude", "s1", "claude::s1::sonnet")
	old.Set("s2", "auth-c")

	saved := old.Snapshot()
	saved = append(saved, SavedSessionBinding{AuthID: "auth-x", ExpiresAt: time.Now().Add(-time.Second), Aliases: []string{"gone"}})

	next := NewSessionCache(time.Hour)
	defer next.Stop()
	next.Set("s2", "auth-live")
	if restored := next.Restore(saved); restored != 2 {
		t.Fatalf("restored = %d, want 2 (the expired one and the session bound since are skipped)", restored)
	}
	for alias, want := range map[string]string{"claude::s1::opus": "auth-a", "claude::s1::sonnet": "auth-b", "s2": "auth-live"} {
		if got, _ := next.Get(alias); got != want {
			t.Fatalf("%s bound to %q, want %q", alias, got, want)
		}
	}
	if _, ok := next.Get("gone"); ok {
		t.Fatal("expired binding was restored")
	}
	if got := next.latestSessionAuth("s1", map[string]struct{}{"claude": {}}); got != "auth-b" {
		t.Fatalf("latest selection for s1 = %q, want auth-b", got)
	}
}

func TestSetSelectorCarriesSessionBindings(t *testing.T) {
	first := NewSessionAffinitySelector(&RoundRobinSelector{})
	m := NewManager(nil, first, nil)
	first.cache.Set("claude::s1::opus", "auth-a")

	m.SetSelector(NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: &QuotaSelector{}, TTL: 2 * time.Hour}))
	if got, _ := m.sessionCache().Get("claude::s1::opus"); got != "auth-a" {
		t.Fatalf("binding after config change = %q, want auth-a", got)
	}
}

func TestSessionBindingsSurviveRestart(t *testing.T) {
	dir := resetPool(t)

	before := NewManager(nil, NewSessionAffinitySelector(&RoundRobinSelector{}), nil)
	before.sessionCache().Set("claude::voice::sonnet", "auth-a")
	if saved, err := before.SaveSessionBindings(); err != nil || saved != 1 {
		t.Fatalf("SaveSessionBindings = %d, %v", saved, err)
	}

	after := NewManager(nil, NewSessionAffinitySelector(&RoundRobinSelector{}), nil)
	if restored, err := after.RestoreSessionBindings(); err != nil || restored != 1 {
		t.Fatalf("RestoreSessionBindings = %d, %v", restored, err)
	}
	if got, _ := after.sessionCache().Get("claude::voice::sonnet"); got != "auth-a" {
		t.Fatalf("binding after restart = %q, want auth-a", got)
	}
	if _, err := os.Stat(filepath.Join(dir, sessionBindingsFile)); !os.IsNotExist(err) {
		t.Fatalf("bindings file still present after restore: %v", err)
	}
}
