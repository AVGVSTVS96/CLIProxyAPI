package auth

import (
	"context"
	"testing"
)

func TestStandbyNeitherSavesNorRefreshesCredentials(t *testing.T) {
	store := &reloadAuthStore{records: map[string]*Auth{}}
	m := NewManager(store, nil, nil)
	auth := &Auth{ID: "a", Provider: "claude", Metadata: map[string]any{"refresh_token": "old"}}
	m.SetStandby(true)
	if err := m.persist(context.Background(), auth); err != nil || len(store.records) != 0 {
		t.Fatalf("persist in standby: err %v, saved %d, want nothing written", err, len(store.records))
	}
	if _, err := m.refreshAuthForRequest(context.Background(), "a", ""); err == nil {
		t.Fatal("refresh in standby succeeded, want it refused")
	}
	m.SetStandby(false)
	if err := m.persist(context.Background(), auth); err != nil || len(store.records) != 1 {
		t.Fatalf("persist after standby: err %v, saved %d, want the auth written", err, len(store.records))
	}
}
