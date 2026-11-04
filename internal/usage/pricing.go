package usage

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
)

// PriceSnapshot is a LiteLLM-derived catalog: per-token USD rates by model.
type PriceSnapshot struct {
	UpdatedAt string                        `json:"updatedAt"`
	Source    string                        `json:"source"`
	Models    map[string]map[string]float64 `json:"models"`
}

// PricingRates are USD per million tokens. A nil cache rate is unavailable; zero is free.
type PricingRates struct {
	InputPerMillion        float64  `json:"inputPerMillion"`
	OutputPerMillion       float64  `json:"outputPerMillion"`
	CacheReadPerMillion    *float64 `json:"cacheReadPerMillion,omitempty"`
	CacheWritePerMillion   *float64 `json:"cacheWritePerMillion,omitempty"`
	CacheWrite1hPerMillion *float64 `json:"cacheWrite1hPerMillion,omitempty"`
}

// PricingRule is a saved override: kind "alias" (Target), "rates" (Rates, Nickname) or "free".
type PricingRule struct {
	Model    string        `json:"model"`
	Kind     string        `json:"kind"`
	Target   string        `json:"target,omitempty"`
	Nickname string        `json:"nickname,omitempty"`
	Rates    *PricingRates `json:"rates,omitempty"`
}

// PricingPolicy is an immutable catalog plus rules; Revision hashes both.
type PricingPolicy struct {
	Catalog  PriceSnapshot `json:"catalog"`
	Rules    []PricingRule `json:"rules"`
	Revision string        `json:"revision"`

	once     sync.Once
	rules    map[string]PricingRule
	mu       sync.Mutex
	resolved map[string]*resolvedPrice
}

//go:embed pricing.json
var bundledPricingJSON []byte

var bundledCatalog = sync.OnceValue(func() PriceSnapshot {
	var snapshot PriceSnapshot
	if err := json.Unmarshal(bundledPricingJSON, &snapshot); err != nil || snapshot.Models == nil {
		return PriceSnapshot{UpdatedAt: "unavailable", Models: map[string]map[string]float64{}}
	}
	return snapshot
})

const (
	priceFree = iota + 1
	priceCustom
	priceCatalog
)

type resolvedPrice struct {
	model string
	kind  int
	rates map[string]float64

	mu       sync.Mutex
	variants map[string]*[5]float64
}

func (p *PricingPolicy) ruleMap() map[string]PricingRule {
	p.once.Do(func() {
		p.rules = make(map[string]PricingRule, len(p.Rules))
		for _, rule := range p.Rules {
			if _, ok := p.rules[rule.Model]; !ok {
				p.rules[rule.Model] = rule
			}
		}
		p.resolved = map[string]*resolvedPrice{}
	})
	return p.rules
}

func (p *PricingPolicy) catalogPrice(model string) map[string]float64 {
	lookup := func(name string) map[string]float64 { return p.Catalog.Models[name] }
	if direct := lookup(model); direct != nil {
		return direct
	}
	for _, prefix := range []string{"openai/", "anthropic/", "xai/", "meta/"} {
		if namespaced := lookup(prefix + model); namespaced != nil {
			return namespaced
		}
		if strings.HasPrefix(model, prefix) {
			if bare := lookup(model[len(prefix):]); bare != nil {
				return bare
			}
		}
	}
	if strings.HasPrefix(model, "claude-") && len(model) > 9 && isDigits(model[len(model)-8:]) && model[len(model)-9] == '-' {
		return lookup(model[:len(model)-9])
	}
	return nil
}

func isDigits(s string) bool {
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func customRates(rates *PricingRates) map[string]float64 {
	fields := map[string]float64{
		"input_cost_per_token":  rates.InputPerMillion / 1_000_000,
		"output_cost_per_token": rates.OutputPerMillion / 1_000_000,
	}
	for field, rate := range map[string]*float64{
		"cache_read_input_token_cost":               rates.CacheReadPerMillion,
		"cache_creation_input_token_cost":           rates.CacheWritePerMillion,
		"cache_creation_input_token_cost_above_1hr": rates.CacheWrite1hPerMillion,
	} {
		if rate != nil {
			fields[field] = *rate / 1_000_000
		}
	}
	return fields
}

// resolve follows alias chains once per model per policy, caching unknown models too.
func (p *PricingPolicy) resolve(model string) *resolvedPrice {
	rules := p.ruleMap()
	p.mu.Lock()
	defer p.mu.Unlock()
	if cached, ok := p.resolved[model]; ok {
		return cached
	}
	visited := []string{}
	seen := map[string]bool{}
	current := model
	var resolved *resolvedPrice
	for !seen[current] {
		if cached, ok := p.resolved[current]; ok {
			resolved = cached
			break
		}
		seen[current] = true
		visited = append(visited, current)
		rule, ok := rules[current]
		if ok && rule.Kind == "alias" {
			current = rule.Target
			continue
		}
		switch {
		case ok && rule.Kind == "free":
			resolved = &resolvedPrice{model: current, kind: priceFree}
		case ok && rule.Kind == "rates" && rule.Rates != nil:
			name := current
			if rule.Nickname != "" {
				name = rule.Nickname
			}
			resolved = &resolvedPrice{model: name, kind: priceCustom, rates: customRates(rule.Rates)}
		default:
			if rates := p.catalogPrice(current); rates != nil {
				resolved = &resolvedPrice{model: current, kind: priceCatalog, rates: rates}
			}
		}
		break
	}
	if len(p.resolved) > 4096 {
		p.resolved = map[string]*resolvedPrice{}
	}
	for _, name := range visited {
		p.resolved[name] = resolved
	}
	return resolved
}

// DisplayModel is the name a raw model is shown under after aliases and nicknames.
func (p *PricingPolicy) DisplayModel(raw string) string {
	if resolved := p.resolve(raw); resolved != nil {
		return resolved.model
	}
	return raw
}

// ValidateRules returns why rules can't be saved together, or "".
func ValidateRules(catalog PriceSnapshot, rules []PricingRule) string {
	seen := map[string]bool{}
	for _, rule := range rules {
		if seen[rule.Model] {
			return "Only one pricing rule can be saved for each model."
		}
		seen[rule.Model] = true
	}
	policy := &PricingPolicy{Catalog: catalog, Rules: rules}
	byModel := policy.ruleMap()
	validated := map[string]bool{}
	for _, rule := range rules {
		if rule.Kind != "alias" {
			continue
		}
		visited := map[string]bool{}
		current := rule.Model
		for !validated[current] {
			if visited[current] {
				return fmt.Sprintf("The alias for %s would create a cycle.", rule.Model)
			}
			visited[current] = true
			next, ok := byModel[current]
			if !ok || next.Kind != "alias" {
				break
			}
			current = next.Target
		}
		if policy.resolve(rule.Model) == nil {
			return fmt.Sprintf("The alias target for %s has no known catalog or custom pricing.", rule.Model)
		}
		for model := range visited {
			validated[model] = true
		}
	}
	return ""
}

var tierSuffixes = map[string]string{
	"": "", "default": "", "standard": "", "priority": "_priority", "fast": "_priority",
	"ultrafast": "_ultrafast", "flex": "_flex", "batch": "_batches", "batches": "_batches",
}

var costFields = [5]string{
	"input_cost_per_token",
	"output_cost_per_token",
	"cache_read_input_token_cost",
	"cache_creation_input_token_cost",
	"cache_creation_input_token_cost_above_1hr",
}

// A NaN rate marks it unavailable.
func (r *resolvedPrice) ratesFor(contextSuffix, tierSuffix string) *[5]float64 {
	suffix := contextSuffix + tierSuffix
	r.mu.Lock()
	defer r.mu.Unlock()
	if cached, ok := r.variants[suffix]; ok {
		return cached
	}
	var rates [5]float64
	for i, field := range costFields {
		rate, ok := r.rates[field+suffix]
		if !ok && contextSuffix != "" {
			rate, ok = r.rates[field+tierSuffix]
		}
		if !ok || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
			rate = math.NaN()
		}
		rates[i] = rate
	}
	if r.variants == nil {
		r.variants = map[string]*[5]float64{}
	}
	r.variants[suffix] = &rates
	return &rates
}

func (r *resolvedPrice) ratesForEvent(e *Event, tier string) *[5]float64 {
	input := e.InputTokens + e.CacheReadTokens + e.CacheWriteTokens
	contextSuffix := ""
	if _, ok := r.rates["input_cost_per_token_above_200k_tokens"]; ok && input > 200_000 {
		contextSuffix = "_above_200k_tokens"
	}
	if _, ok := r.rates["input_cost_per_token_above_272k_tokens"]; ok && input > 272_000 {
		contextSuffix = "_above_272k_tokens"
	}
	tierSuffix := ""
	if r.kind != priceCustom {
		suffix, ok := tierSuffixes[strings.ToLower(jsTrim(tier))]
		if !ok {
			return nil
		}
		tierSuffix = suffix
	}
	return r.ratesFor(contextSuffix, tierSuffix)
}

func missingRates(e *Event, write, write1h int64, rates *[5]float64) bool {
	return (e.InputTokens > 0 && math.IsNaN(rates[0])) ||
		(e.OutputTokens > 0 && math.IsNaN(rates[1])) ||
		(e.CacheReadTokens > 0 && math.IsNaN(rates[2])) ||
		(write > 0 && math.IsNaN(rates[3])) ||
		(write1h > 0 && math.IsNaN(rates[4]))
}

func zeroNaN(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	return f
}

func hasPricingMetadata(e *Event, resolved *resolvedPrice) bool {
	return e.UnclassifiedTokens == 0 && ((resolved != nil && resolved.kind == priceCustom) || e.ServiceTier != nil) &&
		(e.CacheWriteTokens == 0 || e.CacheWrite1hTokens != nil)
}

func estimateResolved(e *Event, cacheWrite1h int64, tier string, resolved *resolvedPrice) (float64, bool) {
	if resolved == nil {
		return 0, false
	}
	if resolved.kind == priceFree {
		return 0, true
	}
	rates := resolved.ratesForEvent(e, tier)
	if rates == nil {
		return 0, false
	}
	write1h := min(max(0, cacheWrite1h), e.CacheWriteTokens)
	write := e.CacheWriteTokens - write1h
	if missingRates(e, write, write1h, rates) {
		return 0, false
	}
	cost := float64(e.InputTokens)*zeroNaN(rates[0]) +
		float64(e.OutputTokens)*zeroNaN(rates[1]) +
		float64(e.CacheReadTokens)*zeroNaN(rates[2]) +
		float64(write)*zeroNaN(rates[3]) +
		float64(write1h)*zeroNaN(rates[4])
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0, false
	}
	return cost, true
}

func (p *PricingPolicy) usesReportedGrokCost(e *Event) bool {
	if e.Harness != "grok" {
		return false
	}
	_, ruled := p.ruleMap()[rawModel(e)]
	return !ruled
}

func reportedGrokCost(e *Event) (float64, bool) {
	if e.ReportedCostUSD != nil && *e.ReportedCostUSD > 0 && !math.IsInf(*e.ReportedCostUSD, 0) {
		return *e.ReportedCostUSD, true
	}
	return 0, false
}

func rawModel(e *Event) string {
	if e.RawModel != "" {
		return e.RawModel
	}
	return e.Model
}

// estimateCost prices a freshly collected record.
func (p *PricingPolicy) estimateCost(e *Event, cacheWrite1h int64, tier string) (float64, bool) {
	if p.usesReportedGrokCost(e) {
		return reportedGrokCost(e)
	}
	return estimateResolved(e, cacheWrite1h, tier, p.resolve(rawModel(e)))
}

// Reprice applies the policy to a stored record. Records without tier or cache-duration
// metadata (from older collectors) keep the cost their machine reported. It reports
// whether anything changed, so unchanged records can be shared.
func (p *PricingPolicy) Reprice(e Event) (Event, bool) {
	raw := rawModel(&e)
	resolved := p.resolve(raw)
	model := raw
	if resolved != nil {
		model = resolved.model
	}
	var cost float64
	var known bool
	switch {
	case p.usesReportedGrokCost(&e):
		cost, known = reportedGrokCost(&e)
	case hasPricingMetadata(&e, resolved) || (resolved != nil && resolved.kind == priceFree):
		var write1h int64
		if e.CacheWrite1hTokens != nil {
			write1h = *e.CacheWrite1hTokens
		}
		tier := ""
		if e.ServiceTier != nil {
			tier = *e.ServiceTier
		}
		cost, known = estimateResolved(&e, write1h, tier, resolved)
	default:
		known = e.CostKnown
		if known {
			cost = e.CostUSD
		}
	}
	if e.RawModel == raw && e.Model == model && e.CostUSD == cost && e.CostKnown == known {
		return e, false
	}
	e.RawModel, e.Model, e.CostUSD, e.CostKnown = raw, model, cost, known
	return e, true
}
