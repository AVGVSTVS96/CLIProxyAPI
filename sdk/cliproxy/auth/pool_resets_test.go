package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func holdReset(id string, expiresIn time.Duration) {
	expires := time.Now().Add(expiresIn).UnixMilli()
	claudePool.account(id).resets = []PoolReset{{ID: "free_reset", Kind: "weekly", Left: 1, ExpiresAt: &expires, Weekly: true, UsableNow: true}}
}

func plansFor(auths ...*Auth) map[string]poolSpend {
	now := time.Now()
	claudePool.mu.Lock()
	defer claudePool.mu.Unlock()
	out := map[string]poolSpend{}
	for _, plan := range claudePool.planResetsLocked(claudePool.rankLocked(auths, now, 1), now) {
		out[plan.row.Account] = plan
	}
	return out
}

func planFor(auths ...*Auth) poolSpend {
	return plansFor(auths...)["holder"]
}

func TestResetWaitsWhileAnotherAccountHasWeekLeft(t *testing.T) {
	resetPool(t)
	holdReset("holder", 40*24*time.Hour)
	plan := planFor(poolAuth("holder", 0, 100, 110*time.Hour), poolAuth("b", 0, 42, 112*time.Hour), poolAuth("c", 0, 36, 124*time.Hour))
	if plan.why != "" || !strings.Contains(plan.plan, "b@example.com and c@example.com have used their week") {
		t.Fatalf("plan = %+v, want it kept until b and c run out", plan)
	}
}

func TestResetSpendsTheMomentThePoolRunsDry(t *testing.T) {
	resetPool(t)
	holdReset("holder", 40*24*time.Hour)
	plan := planFor(poolAuth("holder", 0, 100, 60*time.Hour), poolAuth("b", 0, 98, 62*time.Hour), poolAuth("c", 0, 99, 74*time.Hour))
	if plan.why == "" || plan.row.Account != "holder" {
		t.Fatalf("plan = %+v, want the holder's reset spent", plan)
	}
}

func TestResetWaitsOutAShortDrySpell(t *testing.T) {
	resetPool(t)
	holdReset("holder", 40*24*time.Hour)
	if plan := planFor(poolAuth("holder", 0, 100, 60*time.Hour), poolAuth("b", 0, 99, 3*time.Hour)); plan.why != "" {
		t.Fatalf("plan = %+v, want it kept: b's week comes back in 3 hours", plan)
	}
}

func TestResetTakesAShortDrySpellInItsLastWeek(t *testing.T) {
	resetPool(t)
	holdReset("holder", 5*24*time.Hour)
	if plan := planFor(poolAuth("holder", 0, 100, 60*time.Hour), poolAuth("b", 0, 99, 3*time.Hour)); plan.why == "" {
		t.Fatalf("plan = %+v, want it spent before it expires", plan)
	}
}

func TestResetIsSpentInItsLastHour(t *testing.T) {
	resetPool(t)
	holdReset("holder", 30*time.Minute)
	if plan := planFor(poolAuth("holder", 0, 40, 60*time.Hour), poolAuth("b", 0, 10, 60*time.Hour)); plan.why == "" {
		t.Fatalf("plan = %+v, want it spent rather than lost", plan)
	}
}

func TestEveryHeldResetGetsItsLastHour(t *testing.T) {
	resetPool(t)
	holdReset("late", 40*24*time.Hour)
	holdReset("expiring", 30*time.Minute)
	plans := plansFor(poolAuth("late", 0, 100, 120*time.Hour), poolAuth("expiring", 0, 40, 20*time.Hour), poolAuth("b", 0, 10, 60*time.Hour))
	if plans["expiring"].why == "" || plans["late"].why != "" {
		t.Fatalf("plans = %+v, want only the expiring reset spent", plans)
	}
}

func TestOneResetPerDrySpellOnTheWeekThatEndsLast(t *testing.T) {
	resetPool(t)
	holdReset("soon", 40*24*time.Hour)
	holdReset("late", 40*24*time.Hour)
	plans := plansFor(poolAuth("soon", 0, 100, 20*time.Hour), poolAuth("late", 0, 100, 120*time.Hour), poolAuth("b", 0, 99, 30*time.Hour))
	if plans["late"].why == "" || plans["soon"].why != "" {
		t.Fatalf("plans = %+v, want only late's reset spent", plans)
	}
}

func TestResetThatNeedsTheLimitRunsItsHolderToTheLimit(t *testing.T) {
	resetPool(t)
	holdReset("holder", 30*time.Minute)
	claudePool.account("holder").resets[0].NeedsLimit = true
	holder, b := poolAuth("holder", 0, 99, 60*time.Hour), poolAuth("b", 0, 10, 60*time.Hour)
	claudePool.mu.Lock()
	rows := claudePool.rankLocked([]*Auth{holder, b}, time.Now(), 1)
	claudePool.mu.Unlock()
	if rows[0].Account != "holder" || !rows[0].Usable {
		t.Fatalf("rows = %+v, want the holder first and still taking work at 99%%", rows)
	}
	if plan := planFor(holder, b); plan.why != "" || !strings.Contains(plan.plan, "weekly limit") {
		t.Fatalf("plan = %+v, want it kept until the holder reaches its limit", plan)
	}
	if plan := planFor(poolAuth("holder", 0, 100, 60*time.Hour), b); plan.why == "" {
		t.Fatalf("plan = %+v, want it spent at the limit", plan)
	}
}

func TestGrantNeedsTheLimitUnlessItSaysOtherwise(t *testing.T) {
	if !grantReset(map[string]any{"id": "g", "clears": []any{"seven_day"}}, 1).NeedsLimit {
		t.Fatal("a grant without use_requires_limit should need the limit, as Claude Code reads it")
	}
	if grantReset(map[string]any{"id": "g", "use_requires_limit": false}, 1).NeedsLimit {
		t.Fatal("use_requires_limit false should not need the limit")
	}
}

// resetAPI answers usage reads with usage and reset claims with the next of answers,
// a reset once they run out.
type resetAPI struct {
	mu       sync.Mutex
	usage    string
	answers  []int
	claims   []map[string]string
	requests int
}

const staleUsage = `{"seven_day":{"utilization":100},"cedar_ember":{"grants":[{"id":"free_reset","resets_left":2,` +
	`"clears":["seven_day"],"usable_now":true,"use_requires_limit":false,"type":"weekly","ends_at":"2030-02-01T00:00:00Z"}]}}`

func (f *resetAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	status, body := http.StatusOK, `{}`
	switch {
	case strings.HasSuffix(req.URL.Path, "/reset_rate_limits"):
		var claim map[string]string
		_ = json.NewDecoder(req.Body).Decode(&claim)
		f.claims = append(f.claims, claim)
		if len(f.answers) > 0 {
			status, f.answers = f.answers[0], f.answers[1:]
		}
		body = `{"result":"reset","resets_left":1,"cleared":["seven_day"],"weekly_resets_at":"2030-01-01T00:00:00Z"}`
	case req.URL.Path == "/api/oauth/usage":
		body = f.usage
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func dryPool(t *testing.T, api *resetAPI) (string, []*Auth) {
	dir := resetPool(t)
	claudePool.client = &http.Client{Transport: api}
	holder, other := poolAuth("holder", 0, 100, 60*time.Hour), poolAuth("b", 0, 99, 62*time.Hour)
	holder.Metadata["access_token"], holder.Metadata["organization_uuid"] = "token", "org"
	holdReset("holder", 40*24*time.Hour)
	claudePool.account("holder").resets[0].Left = 2
	return dir, []*Auth{holder, other}
}

func TestSpendResetClaimsOnceAndTellsTheCommand(t *testing.T) {
	api := &resetAPI{usage: staleUsage}
	dir, auths := dryPool(t, api)
	told := filepath.Join(dir, "told")
	ConfigurePool(config.PoolConfig{StateDir: dir, ResetCommand: "printf %s >" + told})

	claudePool.spendReset(context.Background(), nil, auths, time.Now())
	if spent := claudePool.spentLocked()["holder"]; len(spent) != 1 {
		t.Fatalf("spent = %v, want one reset recorded", spent)
	}
	for start := time.Now(); time.Since(start) < 5*time.Second; time.Sleep(20 * time.Millisecond) {
		if line, _ := os.ReadFile(told); strings.HasPrefix(string(line), "Spent holder@example.com's weekly reset") {
			return
		}
	}
	t.Fatal("reset-command never told about the spend")
}

func TestStaleUsageAfterASpendKeepsTheWeekAtZero(t *testing.T) {
	api := &resetAPI{usage: staleUsage}
	_, auths := dryPool(t, api)
	now := time.Now()
	claudePool.spendReset(context.Background(), nil, auths, now)
	for minute := 1; minute <= 30; minute++ {
		at := now.Add(time.Duration(minute) * time.Minute)
		claudePool.readUsage(context.Background(), "holder", "token", at)
		claudePool.spendReset(context.Background(), nil, auths, at)
	}
	if len(api.claims) != 1 {
		t.Fatalf("claims = %v, want the second reset kept", api.claims)
	}
	claudePool.mu.Lock()
	week := claudePool.readingLocked(auths[0], now.Add(30*time.Minute)).Week
	left := claudePool.account("holder").resets
	claudePool.mu.Unlock()
	if week == nil || week.Used != 0 || len(left) != 1 || left[0].Left != 1 {
		t.Fatalf("week = %+v, resets = %+v, want 0%% and one reset left", week, left)
	}
}

func TestClaimKeepsItsRequestIDAcrossARestart(t *testing.T) {
	api := &resetAPI{usage: staleUsage, answers: []int{http.StatusServiceUnavailable}}
	dir, auths := dryPool(t, api)
	claudePool.spendReset(context.Background(), nil, auths, time.Now())

	claudePool = &claudePoolState{accounts: map[string]*poolAccount{}, sessions: map[string]*poolSeat{}, client: &http.Client{Transport: api}}
	ConfigurePool(config.PoolConfig{StateDir: dir})
	claudePool.readUsage(context.Background(), "holder", "token", time.Now())
	claudePool.spendReset(context.Background(), nil, auths, time.Now())

	if len(api.claims) != 2 || api.claims[0]["request_id"] != api.claims[1]["request_id"] {
		t.Fatalf("claims = %v, want the retry under the first request id", api.claims)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, poolClaimsFile)); strings.TrimSpace(string(data)) != "{}" {
		t.Fatalf("%s = %s, want it empty once settled", poolClaimsFile, data)
	}
}

func TestNoUsageReadWhileBackingOff(t *testing.T) {
	api := &resetAPI{usage: staleUsage}
	_, auths := dryPool(t, api)
	claudePool.account("holder").retryAt = time.Now().Add(10 * time.Minute).UnixMilli()
	claudePool.spendReset(context.Background(), nil, auths, time.Now())
	if api.requests != 0 {
		t.Fatalf("requests = %d, want none while the account backs off a 429", api.requests)
	}
}
