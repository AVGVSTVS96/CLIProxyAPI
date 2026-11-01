package deck

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Seat is one paid subscription seat on a plan.
type Seat struct {
	Label      string
	Tier       string
	MonthlyUSD float64
	Since      float64
}

// Plan is a subscription that covers its providers' usage.
type Plan struct {
	Name      string
	Providers []string
	Seats     []Seat
}

var claudeTiers = map[string]struct {
	tier    string
	monthly float64
}{
	"default_claude_max_20x": {"Max 20x", 200},
	"default_claude_max_5x":  {"Max 5x", 100},
	"default_claude_ai":      {"Pro", 20},
}

var tierNames = map[string]string{"max 20x": "default_claude_max_20x", "max 5x": "default_claude_max_5x", "pro": "default_claude_ai"}

func readJSON(path string, out any) bool {
	data, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(data, out) == nil
}

// readPlans lists the pooled Claude accounts' plans, from the pool's account
// list and the plan each profile reported, then the configured plans.
func readPlans(accountsFile, stateDir string, configured []config.UsagePlan) []Plan {
	var list struct {
		Accounts []struct {
			ID    string  `json:"id"`
			Email string  `json:"email"`
			Tier  *string `json:"tier"`
		} `json:"accounts"`
	}
	if accountsFile != "" {
		readJSON(accountsFile, &list)
	}
	var seats []Seat
	for _, account := range list.Accounts {
		var saved struct {
			Tier  *string  `json:"tier"`
			Since *float64 `json:"since"`
		}
		if stateDir != "" {
			readJSON(filepath.Join(stateDir, "plans", account.ID+".json"), &saved)
		}
		tier := ""
		if saved.Tier != nil {
			tier = *saved.Tier
		} else if account.Tier != nil {
			tier = tierNames[*account.Tier]
		}
		price, ok := claudeTiers[tier]
		if ok && saved.Since != nil && *saved.Since != 0 {
			seats = append(seats, Seat{Label: account.Email, Tier: price.tier, MonthlyUSD: price.monthly, Since: *saved.Since})
		}
	}
	var plans []Plan
	if len(seats) > 0 {
		plans = append(plans, Plan{Name: "Claude", Providers: []string{"anthropic"}, Seats: seats})
	}
	for _, plan := range configured {
		plans = append(plans, Plan{Name: plan.Name, Providers: plan.Providers, Seats: []Seat{{
			Label: plan.Name, Tier: plan.Name, MonthlyUSD: plan.MonthlyUSD, Since: parseMillis(plan.Since),
		}}})
	}
	return plans
}

type month struct {
	key        string
	start, end float64
}

// planPricing spreads each plan's monthly fees over the API value its usage had
// that calendar month, so a request's plan $ is its API $ times that month's rate.
type planPricing struct {
	active []Plan
	covers []float64
	months []month
	rate   [][]float64
	now    float64
}

func overlap(start, end, from, to float64) float64 {
	return math.Max(0, math.Min(end, to)-math.Max(start, from))
}

// A seat accrues its monthly price evenly across each calendar month it was active.
func seatFees(seat Seat, m month, from, to float64) float64 {
	return seat.MonthlyUSD * overlap(m.start, m.end, math.Max(from, seat.Since), to) / (m.end - m.start)
}

func newPlanPricing(plans []Plan, events []prepared, zone *time.Location, now time.Time) *planPricing {
	p := &planPricing{now: float64(now.UnixMilli())}
	for _, plan := range plans {
		if len(plan.Seats) == 0 {
			continue
		}
		cover := math.Inf(1)
		for _, seat := range plan.Seats {
			cover = math.Min(cover, seat.Since)
		}
		p.active = append(p.active, plan)
		p.covers = append(p.covers, cover)
	}
	from := p.now
	for _, cover := range p.covers {
		from = math.Min(from, cover)
	}
	if !math.IsNaN(from) {
		start := time.UnixMilli(int64(from)).In(zone)
		for cursor := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, zone); float64(cursor.UnixMilli()) <= p.now; {
			next := time.Date(cursor.Year(), cursor.Month()+1, 1, 0, 0, 0, 0, zone)
			p.months = append(p.months, month{key: fmt.Sprintf("%04d-%02d", cursor.Year(), cursor.Month()), start: float64(cursor.UnixMilli()), end: float64(next.UnixMilli())})
			cursor = next
		}
	}
	api := make([][]float64, len(p.active))
	for i := range api {
		api[i] = make([]float64, len(p.months))
	}
	for i := range events {
		event := &events[i]
		if !event.CostKnown || event.CostUSD <= 0 {
			continue
		}
		plan, m := p.planOf(event.provider, event.millis), p.monthOf(event.millis)
		if plan >= 0 && m >= 0 {
			api[plan][m] += event.CostUSD
		}
	}
	p.rate = make([][]float64, len(p.active))
	for i, plan := range p.active {
		p.rate[i] = make([]float64, len(p.months))
		for j, m := range p.months {
			fees := 0.0
			for _, seat := range plan.Seats {
				fees += seatFees(seat, m, m.start, p.now)
			}
			if api[i][j] > 0 {
				p.rate[i][j] = fees / api[i][j]
			}
		}
	}
	return p
}

func (p *planPricing) monthOf(millis float64) int {
	low, high := 0, len(p.months)-1
	for low <= high {
		i := (low + high) / 2
		switch {
		case millis < p.months[i].start:
			high = i - 1
		case millis >= p.months[i].end:
			low = i + 1
		default:
			return i
		}
	}
	if millis >= p.now {
		return len(p.months) - 1
	}
	return -1
}

func (p *planPricing) planOf(eventProvider string, millis float64) int {
	for i, plan := range p.active {
		if slices.Contains(plan.Providers, eventProvider) && millis >= p.covers[i] {
			return i
		}
	}
	return -1
}

func (p *planPricing) price(event *prepared) float64 {
	plan, m := p.planOf(event.provider, event.millis), p.monthOf(event.millis)
	if plan < 0 || m < 0 {
		return event.CostUSD
	}
	return event.CostUSD * p.rate[plan][m]
}
