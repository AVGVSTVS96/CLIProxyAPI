package deck

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sync"
	"time"
)

// APICredit is a grant and its last balance read off Console, which no API key can query.
// Operators keep these non-secret observations in the pool state directory.
type APICredit struct {
	StartsAt     int64   `json:"startsAt"`
	Account      string  `json:"account"`
	Organization string  `json:"organization"`
	CredentialID string  `json:"credentialId"`
	Remaining    float64 `json:"remaining"`
	Allowance    float64 `json:"allowance"`
	MonthlyCap   float64 `json:"monthlyCap"`
	ObservedAt   int64   `json:"observedAt"`
	ExpiresAt    int64   `json:"expiresAt"`
	RenewsAt     int64   `json:"renewsAt"`
}

// CreditUsage carries the grant forward from its Console reading: Remaining is that
// reading less what the proxy priced on the credential since.
type CreditUsage struct {
	APICredit
	Recorded      float64 `json:"recorded"`
	ProxyUSD      float64 `json:"proxyUsd"`
	Requests      int     `json:"requests"`
	Unpriced      int     `json:"unpriced"`
	LastRequestAt *int64  `json:"lastRequestAt"`
}

type CreditsView struct {
	API []CreditUsage `json:"api"`
}

// Credits reads observations on demand and counts only matching proxy receipts
// during the grant; failed requests are not billed.
func (d *Deck) Credits() (CreditsView, error) {
	view := CreditsView{API: []CreditUsage{}}
	data, err := os.ReadFile(d.stateFile("credits.json"))
	if os.IsNotExist(err) {
		return view, nil
	}
	if err != nil {
		return view, fmt.Errorf("Credit observations could not be read.")
	}
	var grants []APICredit
	if json.Unmarshal(data, &grants) != nil {
		return view, fmt.Errorf("Credit observations are invalid.")
	}
	now := time.Now().UnixMilli()
	events := d.opts.Store.Events()
	for _, g := range grants {
		if g.Account == "" || g.Organization == "" || g.CredentialID == "" || g.StartsAt <= 0 || g.StartsAt > g.ObservedAt || g.ObservedAt <= 0 || g.ObservedAt > now || g.ExpiresAt <= g.ObservedAt || g.Remaining < 0 || g.Allowance <= 0 || g.Remaining > g.Allowance || g.MonthlyCap < 0 {
			return view, fmt.Errorf("Credit observations are invalid.")
		}
		item := CreditUsage{APICredit: g, Recorded: g.Remaining}
		for _, e := range events {
			at := parseMillis(e.Timestamp)
			if e.Source != "proxy" || e.AccountID != g.CredentialID || math.IsNaN(at) || at < float64(g.StartsAt) || at >= float64(g.ExpiresAt) || at > float64(now) {
				continue
			}
			item.Requests++
			if e.CostKnown {
				item.ProxyUSD += e.CostUSD
				if at >= float64(g.ObservedAt) {
					item.Remaining -= e.CostUSD
				}
			} else if !e.Failed {
				item.Unpriced++
			}
			if item.LastRequestAt == nil || int64(at) > *item.LastRequestAt {
				item.LastRequestAt = ptr(int64(at))
			}
		}
		item.Remaining = math.Max(0, item.Remaining)
		view.API = append(view.API, item)
	}
	return view, nil
}

// creditsCache holds the last view, so a request never waits on a pass over every event.
type creditsCache struct {
	mu      sync.Mutex
	view    *CreditsView
	err     error
	hash    string
	loading bool
	again   bool
}

func (d *Deck) cachedCredits() (CreditsView, error) {
	d.credits.mu.Lock()
	view, err := d.credits.view, d.credits.err
	d.credits.mu.Unlock()
	if view == nil {
		return d.Credits()
	}
	return *view, err
}

// refreshCredits recomputes the view, one pass at a time, and tells listeners when it changed.
// A change during a pass runs one more.
func (d *Deck) refreshCredits() {
	c := &d.credits
	c.mu.Lock()
	if c.loading {
		c.again = true
		c.mu.Unlock()
		return
	}
	c.loading = true
	for {
		c.again = false
		c.mu.Unlock()
		view, err := d.Credits()
		hash := fmt.Sprint(string(encode(view)), err)
		c.mu.Lock()
		changed := hash != c.hash
		c.view, c.err, c.hash = &view, err, hash
		if changed {
			d.stream.stale("credits")
		}
		if !c.again {
			break
		}
	}
	c.loading = false
	c.mu.Unlock()
}
