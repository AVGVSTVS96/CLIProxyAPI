package usage

import (
	"math"
	"testing"
)

// Expected values come from token-tracker's own pricing code.

func TestPricingRevisionMatchesTokenTracker(t *testing.T) {
	if got := pricingRevision(defaultStored()); got != "b6d7dedba6e2b99bec30330f941cba7d8bfc98c6cbbc3a02ed9ede3c86128713" {
		t.Fatalf("bundled revision %s", got)
	}
	stored := defaultStored()
	stored.Rules = testRules()
	if got := pricingRevision(stored); got != "bdfc8da71746171d07ab564de94505ff037a5816eb8787fb9998cbe7ee5bbaaf" {
		t.Fatalf("rules revision %s", got)
	}
}

func testRules() []PricingRule {
	cacheRead := 0.1
	return []PricingRule{
		{Model: "my-model", Kind: "alias", Target: "gpt-5"},
		{Model: "cheap", Kind: "rates", Nickname: "Cheap", Rates: &PricingRates{InputPerMillion: 1, OutputPerMillion: 2, CacheReadPerMillion: &cacheRead}},
	}
}

func testEvent(harness, model string, input, output, cacheRead, cacheWrite int64) Event {
	return Event{ID: "a", Timestamp: "2026-01-01T00:00:00.000Z", Harness: harness, Model: model, Project: "p", SessionID: "s",
		InputTokens: input, OutputTokens: output, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestEstimateCost(t *testing.T) {
	policy := NewPricing("").Policy()
	opus := testEvent("claude", "claude-opus-4-5-20251101", 1000, 2000, 300000, 5000)
	if cost, known := policy.estimateCost(&opus, 2000, "standard"); !known || !near(cost, 0.24374999999999997) {
		t.Fatalf("opus standard %v %v", cost, known)
	}
	if _, known := policy.estimateCost(&opus, 0, "priority"); known {
		t.Fatal("a catalog price never borrows standard rates for another tier")
	}
	gpt := testEvent("codex", "gpt-5", 250000, 1000, 30000, 0)
	if cost, _ := policy.estimateCost(&gpt, 0, ""); !near(cost, 0.32625) {
		t.Fatalf("gpt-5 above 272k %v", cost)
	}
	if cost, _ := policy.estimateCost(&gpt, 0, "flex"); !near(cost, 0.163125) {
		t.Fatalf("gpt-5 flex %v", cost)
	}
}

func TestRepriceRulesAndLegacy(t *testing.T) {
	stored := defaultStored()
	stored.Rules = testRules()
	policy := &PricingPolicy{Catalog: stored.Catalog, Rules: stored.Rules}
	tier, write1h := "", int64(0)
	alias := testEvent("codex", "my-model", 250000, 1000, 30000, 0)
	alias.ServiceTier, alias.CacheWrite1hTokens = &tier, &write1h
	if got, _ := policy.Reprice(alias); got.Model != "gpt-5" || got.RawModel != "my-model" || !near(got.CostUSD, 0.32625) {
		t.Fatalf("alias %+v", got)
	}
	custom := alias
	custom.Model = "cheap"
	if got, _ := policy.Reprice(custom); got.Model != "Cheap" || !near(got.CostUSD, 0.255) {
		t.Fatalf("custom %+v", got)
	}
	legacy := testEvent("claude", "claude-opus-4-5-20251101", 1000, 2000, 300000, 5000)
	legacy.CostUSD, legacy.CostKnown = 1.25, true
	if got, _ := policy.Reprice(legacy); got.CostUSD != 1.25 || !got.CostKnown {
		t.Fatalf("a record without tier metadata keeps its reported cost: %+v", got)
	}
	if invalid := ValidateRules(stored.Catalog, append(testRules(), PricingRule{Model: "gpt-5", Kind: "alias", Target: "my-model"})); invalid == "" {
		t.Fatal("alias cycle accepted")
	}
}
