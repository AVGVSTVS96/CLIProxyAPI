package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func poolAuth(id string, five, week float64, weekResetIn time.Duration) *Auth {
	now := time.Now()
	return &Auth{ID: id, Provider: "claude", Metadata: map[string]any{"account_uuid": id, "email": id + "@example.com"},
		Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": strconv.FormatFloat(five/100, 'f', -1, 64),
			"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
			"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(week/100, 'f', -1, 64),
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(weekResetIn).Unix(), 10),
		}}}
}

type offline struct{}

func (offline) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Header: http.Header{}}, nil
}

func resetPool(t *testing.T) string {
	dir := t.TempDir()
	claudePool = &claudePoolState{accounts: map[string]*poolAccount{}, sessions: map[string]*poolSeat{}, client: &http.Client{Transport: offline{}}}
	ConfigurePool(config.PoolConfig{StateDir: dir})
	return dir
}

func pickQuota(t *testing.T, auths ...*Auth) string {
	t.Helper()
	got, err := (&QuotaSelector{}).Pick(context.Background(), "claude", "claude-sonnet-5-5", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatal(err)
	}
	return got.ID
}

func TestQuotaSelectorSkipsAccountsPastACutoff(t *testing.T) {
	resetPool(t)
	if got := pickQuota(t, poolAuth("a", 95, 10, 48*time.Hour), poolAuth("b", 20, 99, 48*time.Hour), poolAuth("c", 50, 50, 96*time.Hour)); got != "c" {
		t.Fatalf("picked %s, want c, the only account under both cutoffs", got)
	}
}

func TestQuotaSelectorSpendsTheWeekThatRunsOutSoonest(t *testing.T) {
	resetPool(t)
	if got := pickQuota(t, poolAuth("later", 10, 40, 120*time.Hour), poolAuth("soon", 10, 40, 10*time.Hour)); got != "soon" {
		t.Fatalf("picked %s, want soon, whose weekly room resets first", got)
	}
}

func TestQuotaSelectorPrefersAnAccountHoldingAReset(t *testing.T) {
	resetPool(t)
	expires := time.Now().Add(24 * time.Hour).UnixMilli()
	claudePool.account("holder").resets = []PoolReset{{Kind: "weekly", Left: 1, ExpiresAt: &expires}}
	if got := pickQuota(t, poolAuth("soon", 10, 40, 10*time.Hour), poolAuth("holder", 10, 40, 120*time.Hour)); got != "holder" {
		t.Fatalf("picked %s, want holder", got)
	}
}

func TestMoveAwayLeavesAnAccountNearItsCutoffOnlyForAClearOne(t *testing.T) {
	resetPool(t)
	hot, calm := poolAuth("hot", 85, 30, 48*time.Hour), poolAuth("calm", 10, 30, 48*time.Hour)
	if rest, level := claudePool.moveAway(hot, []*Auth{hot, calm}); len(rest) != 1 || rest[0].ID != "calm" || level != "early" {
		t.Fatalf("rest = %v, want only calm", rest)
	}
	warm := poolAuth("warm", 82, 30, 48*time.Hour)
	if rest, _ := claudePool.moveAway(hot, []*Auth{hot, warm}); rest != nil {
		t.Fatalf("moved to an account that is no clearer: %v", rest)
	}
	if rest, _ := claudePool.moveAway(calm, []*Auth{hot, calm}); rest != nil {
		t.Fatalf("moved a session off a calm account: %v", rest)
	}
}

func TestSeatRecordsEachSessionsAccountOncePerChange(t *testing.T) {
	dir := resetPool(t)
	headers := http.Header{"X-Claude-Code-Session-Id": []string{"51933568-42b8-4c8e-bc44-2bed00731a66"}}
	a, b := poolAuth("a", 1, 1, time.Hour), poolAuth("b", 1, 1, time.Hour)
	claudePool.seat(headers, a, "limit")
	claudePool.seat(headers, a, "limit")
	claudePool.seat(headers, b, "early")
	claudePool.seat(http.Header{"X-Claude-Code-Session-Id": []string{"not-a-session"}}, a, "limit")
	data, _ := os.ReadFile(filepath.Join(dir, "attribution.ndjson"))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"accountId":"a"`) || !strings.Contains(lines[1], `"accountId":"b"`) || !strings.Contains(lines[1], `"event":"failure"`) || !strings.Contains(lines[1], `"cause":"early"`) {
		t.Fatalf("attribution = %q", lines)
	}
}

func TestSnapshotNeverCarriesATokenAndHistoryLogsMoves(t *testing.T) {
	dir := resetPool(t)
	a := poolAuth("a", 30, 40, 48*time.Hour)
	a.Metadata["access_token"] = "secret-token"
	m := NewManager(nil, &QuotaSelector{}, nil)
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	claudePool.sample(context.Background(), m.List())
	snap := m.PoolSnapshot(nil)
	if len(snap.Accounts) != 1 || snap.Accounts[0].Reading.Week == nil || snap.Accounts[0].Reading.Week.Used != 40 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if body, _ := json.Marshal(snap); strings.Contains(string(body), "secret-token") {
		t.Fatal("snapshot leaked a token")
	}
	history, _ := os.ReadFile(filepath.Join(dir, "usage-history.ndjson"))
	if !strings.Contains(string(history), `"account":"a"`) || !strings.Contains(string(history), `"week":40`) {
		t.Fatalf("history = %s", history)
	}
}
