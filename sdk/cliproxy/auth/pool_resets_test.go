package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
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
	for _, plan := range claudePool.planResetsLocked(claudePool.rankLocked(auths, now, 1, ""), now) {
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

func TestResetSpendsThoughAnotherWeekComesBackSoon(t *testing.T) {
	resetPool(t)
	holdReset("holder", 40*24*time.Hour)
	if plan := planFor(poolAuth("holder", 0, 100, 60*time.Hour), poolAuth("b", 0, 99, 3*time.Hour)); plan.why == "" {
		t.Fatalf("plan = %+v, want it spent: the pool is dry and the holder's week has 60 hours to use it", plan)
	}
}

func TestResetWaitsWhenItsOwnWeekIsAlmostOver(t *testing.T) {
	resetPool(t)
	holdReset("holder", 40*24*time.Hour)
	if plan := planFor(poolAuth("holder", 0, 100, 3*time.Hour), poolAuth("b", 0, 99, 60*time.Hour)); plan.why != "" {
		t.Fatalf("plan = %+v, want it kept: the holder's week comes back in 3 hours", plan)
	}
}

func TestResetIsSpentOnAWeekAlmostOverInItsLastWeek(t *testing.T) {
	resetPool(t)
	holdReset("holder", 5*24*time.Hour)
	if plan := planFor(poolAuth("holder", 0, 100, 3*time.Hour), poolAuth("b", 0, 99, 60*time.Hour)); plan.why == "" {
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
	rows := claudePool.rankLocked([]*Auth{holder, b}, time.Now(), 1, "")
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
		if line, _ := os.ReadFile(told); strings.HasPrefix(string(line), "Spent holder@example.com's weekly reset") && strings.HasSuffix(string(line), "1 weekly reset is still held, a fresh week on holder@example.com.") {
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

func holdFableReset(id string, fableUsed float64) {
	expires := time.Now().Add(120 * 24 * time.Hour).UnixMilli()
	claudePool.account(id).resets = []PoolReset{{ID: "launch-fable", Kind: "fable", Left: 1, ExpiresAt: &expires, Fable: true, FableUsed: fableUsed, UsableNow: true}}
}

func TestFableResetSpendsWhenThePoolRunsDryOnTheOneGivingBackMost(t *testing.T) {
	resetPool(t)
	holdFableReset("little", 10)
	holdFableReset("most", 50)
	plans := plansFor(poolAuth("little", 0, 100, 120*time.Hour), poolAuth("most", 0, 100, 60*time.Hour), poolAuth("b", 0, 99, 30*time.Hour))
	if plans["most"].why == "" || plans["little"].why != "" {
		t.Fatalf("plans = %+v, want only most's Fable reset spent", plans)
	}
	if !strings.Contains(plans["little"].plan, "gives back ~5 weekly points") {
		t.Fatalf("plan = %q, want what it gives back", plans["little"].plan)
	}
}

func TestFableResetWaitsWhileAnotherAccountHasWeekLeft(t *testing.T) {
	resetPool(t)
	holdFableReset("holder", 50)
	if plan := planFor(poolAuth("holder", 0, 100, 60*time.Hour), poolAuth("b", 0, 40, 62*time.Hour)); plan.why != "" {
		t.Fatalf("plan = %+v, want it kept until b runs out", plan)
	}
}

func TestFableResetWithNoFableUseIsKept(t *testing.T) {
	resetPool(t)
	holdFableReset("holder", 0)
	if plan := planFor(poolAuth("holder", 0, 100, 60*time.Hour), poolAuth("b", 0, 99, 62*time.Hour)); plan.row != nil {
		t.Fatalf("plan = %+v, want nothing planned: there is no Fable use to give back", plan)
	}
}

func TestFableHolderIsNotPreferredForNewWork(t *testing.T) {
	resetPool(t)
	holdFableReset("holder", 50)
	claudePool.mu.Lock()
	rows := claudePool.rankLocked([]*Auth{poolAuth("holder", 0, 10, 60*time.Hour)}, time.Now(), 1, "")
	claudePool.mu.Unlock()
	if rows[0].HoldsReset {
		t.Fatal("a Fable reset doesn't clear a whole week, so its holder shouldn't go first")
	}
}

func TestGrantReadsAFableReset(t *testing.T) {
	g := grantReset(map[string]any{"id": "launch-2610-fable", "clears": []any{"five_hour", "seven_day_overage_included"}, "gives_back_weekly": true,
		"use_requires_limit": false, "percent_used": map[string]any{"seven_day_overage_included": 44.0, "seven_day": 83.0}}, 1)
	if !g.Fable || g.Weekly || g.FableUsed != 44 || g.GivesBack(83) != g.FableUsed*poolFableShare {
		t.Fatalf("grant = %+v, want a Fable reset with 44%% Fable used", g)
	}
}

func TestStaleUsageAfterAFableSpendKeepsTheOtherResets(t *testing.T) {
	api := &resetAPI{usage: `{"seven_day":{"utilization":100},"cedar_ember":{"grants":[{"id":"launch-fable","resets_left":1,"gives_back_weekly":true,` +
		`"clears":["seven_day_overage_included"],"usable_now":true,"use_requires_limit":false,"type":"full","ends_at":"2030-02-01T00:00:00Z",` +
		`"percent_used":{"seven_day_overage_included":50,"seven_day":100}}]}}`}
	resetPool(t)
	claudePool.client = &http.Client{Transport: api}
	holder, other := poolAuth("holder", 0, 100, 60*time.Hour), poolAuth("b", 0, 100, 62*time.Hour)
	for _, a := range []*Auth{holder, other} {
		a.Metadata["access_token"], a.Metadata["organization_uuid"] = "token", "org"
	}
	holdFableReset("holder", 50)
	holdFableReset("b", 40)
	now := time.Now()
	claudePool.spendReset(context.Background(), nil, []*Auth{holder, other}, now)
	for minute := 1; minute <= 30; minute++ {
		at := now.Add(time.Duration(minute) * time.Minute)
		claudePool.readUsage(context.Background(), "holder", "token", at)
		claudePool.spendReset(context.Background(), nil, []*Auth{holder, other}, at)
	}
	if len(api.claims) != 1 {
		t.Fatalf("claims = %v, want b's reset kept until holder's giveback shows", api.claims)
	}
}

func fableAt(a *Auth, used float64) *Auth {
	a.Quota.Signals["Anthropic-Ratelimit-Unified-7d_oi-Utilization"] = strconv.FormatFloat(used/100, 'f', -1, 64)
	a.Quota.Signals["Anthropic-Ratelimit-Unified-7d_oi-Reset"] = a.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"]
	return a
}

func pickModel(t *testing.T, model string, auths ...*Auth) string {
	t.Helper()
	got, err := (&QuotaSelector{}).Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatal(err)
	}
	return got.ID
}

func TestFableWorkSkipsAnAccountWithNoFableLeft(t *testing.T) {
	resetPool(t)
	spent, other := fableAt(poolAuth("spent", 10, 40, 10*time.Hour), 99), fableAt(poolAuth("other", 10, 40, 120*time.Hour), 30)
	if got := pickModel(t, "claude-fable-5-2", spent, other); got != "other" {
		t.Fatalf("Fable went to %s, want other, the account with Fable left", got)
	}
	if got := pickModel(t, "claude-opus-5-5", spent, other); got != "spent" {
		t.Fatalf("Opus went to %s, want spent, whose week runs out first", got)
	}
}

func TestFableReadingOutlivesTheNextReplyOfAnotherModel(t *testing.T) {
	resetPool(t)
	spent, other := fableAt(poolAuth("spent", 10, 40, 10*time.Hour), 99), poolAuth("other", 10, 40, 120*time.Hour)
	claudePool.observeFable(spent)
	spent.Quota.Signals = poolAuth("spent", 10, 40, 10*time.Hour).Quota.Signals
	if got := pickModel(t, "claude-fable-5-2[1m]", spent, other); got != "other" {
		t.Fatalf("Fable went to %s after an Opus reply dropped the Fable headers", got)
	}
}

func TestFableSessionLeavesAnAccountRunningOutOfFable(t *testing.T) {
	resetPool(t)
	hot, calm := fableAt(poolAuth("hot", 10, 30, 48*time.Hour), 96), fableAt(poolAuth("calm", 10, 30, 48*time.Hour), 20)
	if rest, level := claudePool.moveAway(hot, []*Auth{hot, calm}, nil, "claude-fable-5-2"); len(rest) != 1 || level != "early" {
		t.Fatalf("kept a Fable session 2 points from Fable's cutoff: rest = %v", rest)
	}
	if rest, _ := claudePool.moveAway(hot, []*Auth{hot, calm}, nil, "claude-opus-5-5"); rest != nil {
		t.Fatalf("moved an Opus session over Fable's allowance: %v", rest)
	}
}

func TestUsageReadsFablesAllowanceAndItsShareOfTheWeek(t *testing.T) {
	resetPool(t)
	claudePool.client = &http.Client{Transport: &resetAPI{usage: `{"seven_day":{"utilization":82},` +
		`"limits":[{"kind":"weekly_all","percent":82},{"kind":"weekly_scoped","percent":51,"resets_at":"2030-01-01T00:00:00Z","scope":{"model":{"display_name":"Fable"}}}],` +
		`"weekly_scoped_shares":[{"limit_index":1,"used_percent_of_weekly":26,"allowance_percent_of_weekly":50}],` +
		`"cedar_ember":{"grants":[{"id":"launch-fable","resets_left":1,"gives_back_weekly":true,"clears":["seven_day_overage_included"],` +
		`"usable_now":true,"use_requires_limit":false,"ends_at":"2030-02-01T00:00:00Z","percent_used":{"seven_day_overage_included":51,"seven_day":82}}]}}`}}
	claudePool.readUsage(context.Background(), "a", "token", time.Now())
	claudePool.mu.Lock()
	rows := claudePool.rankLocked([]*Auth{poolAuth("a", 0, 82, 48*time.Hour)}, time.Now(), 1, "claude-fable-5-2")
	claudePool.mu.Unlock()
	r := rows[0]
	if r.Fable == nil || r.Fable.Used != 51 || r.FableShare != 0.5 || r.Resets[0].GivesBack(82) != 25.5 {
		t.Fatalf("row = %+v, want Fable at 51%%, half the week, giving back 25.5 points", r)
	}
}

func TestFableResetSpendsOnceNoAccountHasFableLeft(t *testing.T) {
	resetPool(t)
	holdFableReset("holder", 100)
	holder, b := fableAt(poolAuth("holder", 0, 70, 60*time.Hour), 100), fableAt(poolAuth("b", 0, 40, 62*time.Hour), 99)
	if plan := planFor(holder, b); plan.why == "" || !strings.Contains(plan.why, "Fable allowance") {
		t.Fatalf("plan = %+v, want it spent: Fable work has nowhere left to go", plan)
	}
}

func TestFableResetWaitsWhileAnotherAccountHasFableLeft(t *testing.T) {
	resetPool(t)
	holdFableReset("holder", 100)
	holder, b := fableAt(poolAuth("holder", 0, 70, 60*time.Hour), 100), fableAt(poolAuth("b", 0, 40, 62*time.Hour), 60)
	if plan := planFor(holder, b); plan.why != "" || !strings.Contains(plan.plan, "b@example.com has used its week or Fable allowance") {
		t.Fatalf("plan = %+v, want it kept while b has Fable left", plan)
	}
}
