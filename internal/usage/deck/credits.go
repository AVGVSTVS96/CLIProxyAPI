package deck

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"
)

// APICredit is a recorded grant, never an inferred Anthropic billing balance.
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

type CreditUsage struct {
	APICredit
	ProxyUSD      float64 `json:"proxyUsd"`
	Requests      int     `json:"requests"`
	Unpriced      int     `json:"unpriced"`
	LastRequestAt *int64  `json:"lastRequestAt"`
}

type CreditsView struct {
	API []CreditUsage `json:"api"`
}

// Credits reads observations on demand and counts only matching proxy receipts
// during the grant. Logged API value does not prove the balance debited.
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
	for _, g := range grants {
		if g.Account == "" || g.Organization == "" || g.CredentialID == "" || g.StartsAt <= 0 || g.StartsAt > g.ObservedAt || g.ObservedAt <= 0 || g.ObservedAt > now || g.ExpiresAt <= g.ObservedAt || g.Remaining < 0 || g.Allowance <= 0 || g.Remaining > g.Allowance || g.MonthlyCap < 0 {
			return view, fmt.Errorf("Credit observations are invalid.")
		}
		item := CreditUsage{APICredit: g}
		for _, e := range d.opts.Store.Events() {
			at := parseMillis(e.Timestamp)
			if e.Source != "proxy" || e.AccountID != g.CredentialID || math.IsNaN(at) || at < float64(g.StartsAt) || at >= float64(g.ExpiresAt) || at > float64(now) {
				continue
			}
			item.Requests++
			if e.CostKnown {
				item.ProxyUSD += e.CostUSD
			} else {
				item.Unpriced++
			}
			if item.LastRequestAt == nil || int64(at) > *item.LastRequestAt {
				item.LastRequestAt = ptr(int64(at))
			}
		}
		view.API = append(view.API, item)
	}
	return view, nil
}
