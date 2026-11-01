package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	pricingSource   = "https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json"
	pricingDownload = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	pricingFile     = "pricing-state.json"
)

// storedPricing is pricing-state.json, the same file token-tracker keeps.
type storedPricing struct {
	Catalog      PriceSnapshot `json:"catalog"`
	Rules        []PricingRule `json:"rules"`
	CheckedAt    *string       `json:"checkedAt"`
	RefreshError *string       `json:"refreshError"`
}

// PricingInfo describes the current policy for the dashboard.
type PricingInfo struct {
	Revision     string        `json:"revision"`
	UpdatedAt    string        `json:"updatedAt"`
	CheckedAt    *string       `json:"checkedAt"`
	Source       string        `json:"source"`
	Rules        []PricingRule `json:"rules"`
	Models       []string      `json:"models"`
	RefreshError *string       `json:"refreshError"`
}

// Pricing owns the saved catalog and rules in a directory and hands out immutable policies.
type Pricing struct {
	dir      string
	download string
	onChange func()

	mu          sync.Mutex
	fingerprint string
	stored      storedPricing
	policy      *PricingPolicy
}

// NewPricing reads dir/pricing-state.json, or the bundled catalog when it is missing.
// An empty dir keeps everything in memory.
func NewPricing(dir string) *Pricing {
	p := &Pricing{dir: dir, download: pricingDownload}
	p.reload()
	return p
}

func defaultStored() storedPricing {
	return storedPricing{Catalog: bundledCatalog(), Rules: []PricingRule{}}
}

func (p *Pricing) path() string { return filepath.Join(p.dir, pricingFile) }

func fileFingerprint(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fileSignature(info)
}

// reload rereads the file when another process replaced it.
func (p *Pricing) reload() {
	p.mu.Lock()
	defer p.mu.Unlock()
	fingerprint := "memory"
	if p.dir != "" {
		fingerprint = fileFingerprint(p.path())
	}
	if p.policy != nil && fingerprint == p.fingerprint {
		return
	}
	stored := defaultStored()
	if fingerprint != "" && fingerprint != "memory" {
		if decoded, err := readStoredPricing(p.path()); err == nil {
			stored = decoded
		} else {
			message := "The saved pricing catalog could not be read; bundled prices remain available."
			stored.RefreshError = &message
		}
	}
	p.fingerprint = fingerprint
	p.setLocked(stored)
}

func readStoredPricing(path string) (storedPricing, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return storedPricing{}, err
	}
	if len(data) > 20*1024*1024 {
		return storedPricing{}, errors.New("the saved pricing catalog is too large")
	}
	var stored storedPricing
	if err = json.Unmarshal(data, &stored); err != nil {
		return storedPricing{}, err
	}
	if stored.Catalog.Models == nil {
		return storedPricing{}, errors.New("the saved pricing catalog has no models")
	}
	if stored.Rules == nil {
		stored.Rules = []PricingRule{}
	}
	if invalid := checkRules(stored.Catalog, stored.Rules); invalid != "" {
		return storedPricing{}, errors.New(invalid)
	}
	return stored, nil
}

func checkRules(catalog PriceSnapshot, rules []PricingRule) string {
	finite := func(v *float64) bool { return v == nil || (*v >= 0 && !math.IsInf(*v, 0) && !math.IsNaN(*v)) }
	for _, rule := range rules {
		name := func(s string) bool { return s != "" && len(s) <= 200 && s == strings.TrimSpace(s) }
		ok := name(rule.Model)
		switch rule.Kind {
		case "alias":
			ok = ok && name(rule.Target)
		case "free":
		case "rates":
			r := rule.Rates
			ok = ok && r != nil && (rule.Nickname == "" || name(rule.Nickname)) &&
				finite(&r.InputPerMillion) && finite(&r.OutputPerMillion) &&
				finite(r.CacheReadPerMillion) && finite(r.CacheWritePerMillion) && finite(r.CacheWrite1hPerMillion)
		default:
			ok = false
		}
		if !ok {
			return "Enter a valid model name and finite, nonnegative rates."
		}
	}
	return ValidateRules(catalog, rules)
}

func (p *Pricing) setLocked(stored storedPricing) {
	p.stored = stored
	p.policy = &PricingPolicy{Catalog: stored.Catalog, Rules: stored.Rules, Revision: pricingRevision(stored)}
}

// Policy is the current immutable pricing policy.
func (p *Pricing) Policy() *PricingPolicy {
	p.reload()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.policy
}

// Info summarizes the current policy.
func (p *Pricing) Info() PricingInfo {
	p.reload()
	p.mu.Lock()
	defer p.mu.Unlock()
	models := make([]string, 0, len(p.stored.Catalog.Models)+len(p.stored.Rules))
	seen := map[string]bool{}
	for model := range p.stored.Catalog.Models {
		models = append(models, model)
		seen[model] = true
	}
	for _, rule := range p.stored.Rules {
		if !seen[rule.Model] {
			models = append(models, rule.Model)
		}
	}
	sort.Strings(models)
	return PricingInfo{
		Revision: p.policy.Revision, UpdatedAt: p.stored.Catalog.UpdatedAt, CheckedAt: p.stored.CheckedAt,
		Source: p.stored.Catalog.Source, Rules: p.stored.Rules, Models: models, RefreshError: p.stored.RefreshError,
	}
}

func (p *Pricing) persistLocked(stored storedPricing) error {
	if p.dir != "" {
		data, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		if err = writeFileAtomic(p.path(), append(data, '\n')); err != nil {
			return errors.New("could not save pricing settings")
		}
		p.fingerprint = fileFingerprint(p.path())
	}
	p.setLocked(stored)
	if p.onChange != nil {
		go p.onChange()
	}
	return nil
}

// SetRule saves or replaces the rule for rule.Model.
func (p *Pricing) SetRule(rule PricingRule) error {
	p.reload()
	p.mu.Lock()
	defer p.mu.Unlock()
	rules := []PricingRule{}
	for _, existing := range p.stored.Rules {
		if existing.Model != rule.Model {
			rules = append(rules, existing)
		}
	}
	rules = append(rules, rule)
	sort.Slice(rules, func(i, j int) bool { return rules[i].Model < rules[j].Model })
	if invalid := checkRules(p.stored.Catalog, rules); invalid != "" {
		return errors.New(invalid)
	}
	stored := p.stored
	stored.Rules = rules
	return p.persistLocked(stored)
}

// DeleteRule removes the rule for model.
func (p *Pricing) DeleteRule(model string) error {
	p.reload()
	p.mu.Lock()
	defer p.mu.Unlock()
	rules := []PricingRule{}
	for _, existing := range p.stored.Rules {
		if existing.Model != model {
			rules = append(rules, existing)
		}
	}
	if invalid := ValidateRules(p.stored.Catalog, rules); invalid != "" {
		return errors.New(invalid)
	}
	stored := p.stored
	stored.Rules = rules
	return p.persistLocked(stored)
}

// RefreshDue is true when the catalog was last checked a day or more ago.
func (p *Pricing) RefreshDue(now time.Time) bool {
	p.reload()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stored.CheckedAt == nil {
		return true
	}
	checked, err := time.Parse(time.RFC3339Nano, *p.stored.CheckedAt)
	return err != nil || now.Sub(checked) >= 24*time.Hour
}

// Refresh downloads LiteLLM's catalog and merges it over the bundled and saved models,
// keeping saved rules. A failed download keeps the previous catalog and records why.
func (p *Pricing) Refresh(ctx context.Context) error {
	catalog, downloadErr := downloadCatalog(ctx, p.download)
	p.reload()
	p.mu.Lock()
	defer p.mu.Unlock()
	checkedAt := isoNow()
	stored := p.stored
	stored.CheckedAt = &checkedAt
	if downloadErr != nil {
		message := "Could not refresh model prices. The previous catalog and your saved rules remain available."
		stored.RefreshError = &message
		_ = p.persistLocked(stored)
		return downloadErr
	}
	models := map[string]map[string]float64{}
	for _, source := range []map[string]map[string]float64{bundledCatalog().Models, stored.Catalog.Models, catalog.Models} {
		for model, rates := range source {
			models[model] = rates
		}
	}
	catalog.Models = models
	if invalid := ValidateRules(catalog, stored.Rules); invalid != "" {
		stored.RefreshError = &invalid
		return p.persistLocked(stored)
	}
	stored.Catalog = catalog
	stored.RefreshError = nil
	return p.persistLocked(stored)
}

// RunRefresh checks hourly whether the daily catalog refresh is due, until ctx ends.
func (p *Pricing) RunRefresh(ctx context.Context) {
	for {
		if p.RefreshDue(time.Now()) {
			_ = p.Refresh(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

var rateField = regexp.MustCompile(`^(?:input_cost_per_token|output_cost_per_token|cache_read_input_token_cost|cache_creation_input_token_cost)(?:_|$)`)

func downloadCatalog(ctx context.Context, url string) (PriceSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PriceSnapshot{}, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return PriceSnapshot{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return PriceSnapshot{}, fmt.Errorf("pricing download: %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return PriceSnapshot{}, err
	}
	return decodeUpstreamPrices(body)
}

func decodeUpstreamPrices(body []byte) (PriceSnapshot, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return PriceSnapshot{}, errors.New("the downloaded pricing catalog is invalid")
	}
	models := map[string]map[string]float64{}
	for model, value := range raw {
		var fields map[string]any
		if json.Unmarshal(value, &fields) != nil || fields == nil {
			continue
		}
		rates := map[string]float64{}
		for field, rate := range fields {
			if !rateField.MatchString(field) || rate == nil {
				continue
			}
			number, ok := rate.(float64)
			if !ok || number < 0 || math.IsInf(number, 0) {
				return PriceSnapshot{}, fmt.Errorf("the downloaded price for %s is invalid", model)
			}
			rates[field] = number
		}
		_, input := rates["input_cost_per_token"]
		_, output := rates["output_cost_per_token"]
		if input && output {
			models[model] = rates
		}
	}
	if len(models) < 100 {
		return PriceSnapshot{}, errors.New("the downloaded pricing catalog is unexpectedly incomplete")
	}
	return PriceSnapshot{Source: pricingSource, UpdatedAt: isoNow(), Models: models}, nil
}

func isoNow() string { return isoMillis(float64(time.Now().UnixMilli())) }

// pricingRevision is token-tracker's content hash of the catalog and rules.
func pricingRevision(stored storedPricing) string {
	var b strings.Builder
	b.WriteByte('[')
	stableJSON(&b, stored.Catalog)
	b.WriteByte(',')
	stableJSON(&b, stored.Rules)
	b.WriteByte(']')
	return sha256Hex(b.String())
}

func stableJSON(b *strings.Builder, value any) {
	data, _ := json.Marshal(value)
	var generic any
	_ = json.Unmarshal(data, &generic)
	writeStable(b, generic)
}

func writeStable(b *strings.Builder, value any) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			jsString(b, key)
			b.WriteByte(':')
			writeStable(b, v[key])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			writeStable(b, item)
		}
		b.WriteByte(']')
	case string:
		jsString(b, v)
	case float64:
		b.WriteString(jsNumber(v))
	case bool:
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	default:
		b.WriteString("null")
	}
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Chmod(temp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
