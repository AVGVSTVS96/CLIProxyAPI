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

func planFor(auths ...*Auth) poolSpend {
	now := time.Now()
	claudePool.mu.Lock()
	defer claudePool.mu.Unlock()
	return claudePool.planResetLocked(claudePool.rankLocked(auths, now, 1), now)
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

type resetAPI struct {
	mu     sync.Mutex
	claims []map[string]string
}

func (f *resetAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := `{}`
	switch {
	case strings.HasSuffix(req.URL.Path, "/reset_rate_limits"):
		var claim map[string]string
		_ = json.NewDecoder(req.Body).Decode(&claim)
		f.claims = append(f.claims, claim)
		body = `{"result":"reset","resets_left":0,"cleared":["seven_day"],"weekly_resets_at":"2030-01-01T00:00:00Z"}`
	case req.URL.Path == "/api/oauth/usage":
		left := 1 - len(f.claims)
		body = `{"seven_day":{"utilization":100},"cedar_ember":{"grants":[{"id":"free_reset","resets_left":` + string(rune('0'+left)) +
			`,"clears":["seven_day"],"usable_now":true,"type":"weekly","ends_at":"2030-02-01T00:00:00Z"}]}}`
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func TestSpendResetClaimsOnceAndTellsTheCommand(t *testing.T) {
	dir := resetPool(t)
	api := &resetAPI{}
	claudePool.client = &http.Client{Transport: api}
	told := filepath.Join(dir, "told")
	ConfigurePool(config.PoolConfig{StateDir: dir, ResetCommand: "printf %s >" + told})
	holder, other := poolAuth("holder", 0, 100, 60*time.Hour), poolAuth("b", 0, 99, 62*time.Hour)
	holder.Metadata["access_token"], holder.Metadata["organization_uuid"] = "token", "org"
	holdReset("holder", 40*24*time.Hour)

	claudePool.spendReset(context.Background(), []*Auth{holder, other}, time.Now())
	claudePool.mu.Lock()
	claudePool.account("holder").resetsAt = time.Now().UnixMilli()
	claudePool.mu.Unlock()
	claudePool.spendReset(context.Background(), []*Auth{holder, other}, time.Now())

	if len(api.claims) != 1 || api.claims[0]["program"] != "cedar_ember" || api.claims[0]["grant_id"] != "free_reset" || api.claims[0]["request_id"] == "" {
		t.Fatalf("claims = %v, want one cedar_ember claim for free_reset", api.claims)
	}
	if spent := claudePool.spentLocked()["holder"]; len(spent) != 1 {
		t.Fatalf("spent = %v, want one reset recorded", spent)
	}
	if w := claudePool.account("holder").reading.Week; w == nil || w.Used != 0 {
		t.Fatalf("week = %+v, want 0 after the reset", w)
	}
	for start := time.Now(); time.Since(start) < 5*time.Second; time.Sleep(20 * time.Millisecond) {
		if line, _ := os.ReadFile(told); strings.HasPrefix(string(line), "Spent holder@example.com's weekly reset") {
			return
		}
	}
	t.Fatal("reset-command never told about the spend")
}
