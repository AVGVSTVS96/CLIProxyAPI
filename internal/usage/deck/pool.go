package deck

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// LimitWindow is one of an account's limits now.
type LimitWindow struct {
	Used      *float64 `json:"used"`
	Cutoff    float64  `json:"cutoff"`
	ResetsAt  *int64   `json:"resetsAt"`
	SampledAt *int64   `json:"sampledAt"`
	// Share of the window already passed, 0 to 1, so pace reads at a glance.
	Elapsed *float64 `json:"elapsed"`
}

// HeldReset is a limit reset an account holds.
type HeldReset struct {
	Kind      string `json:"kind"`
	Left      int    `json:"left"`
	ExpiresAt *int64 `json:"expiresAt"`
}

// Account is one pooled Claude account and its standing for new work.
type Account struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Plan  string `json:"plan"`
	// When the account's current plan started, from its profile; not when it joined the pool.
	Since       *int64      `json:"since"`
	SignInUntil *int64      `json:"signInUntil"`
	Rank        *int        `json:"rank"`
	Standing    string      `json:"standing"`
	Reasons     []string    `json:"reasons"`
	FiveHour    LimitWindow `json:"fiveHour"`
	Week        LimitWindow `json:"week"`
	Working     int         `json:"working"`
	Idle        int         `json:"idle"`
	HeldResets  []HeldReset `json:"heldResets"`
	AtLimit     any         `json:"atLimit"`
}

// Policy is the pool's cutoffs.
type Policy struct {
	SessionCutoff    float64 `json:"sessionCutoff"`
	WeeklyCutoff     float64 `json:"weeklyCutoff"`
	AgentsPerAccount int     `json:"agentsPerAccount"`
}

// Pool is where new sessions go and why.
type Pool struct {
	Next     *string  `json:"next"`
	Decision string   `json:"decision"`
	Order    []string `json:"order"`
	Policy   Policy   `json:"policy"`
	Revision string   `json:"revision"`
	Notes    []string `json:"notes"`
}

// PolicyResult answers a policy preview or save.
type PolicyResult struct {
	Pool     Pool `json:"pool"`
	Saved    bool `json:"saved"`
	Conflict bool `json:"conflict"`
}

type seat struct{ session, account string }

type poolState struct {
	accounts []*Account
	pool     Pool
	seats    []seat
}

const (
	fiveHours = 5 * 3_600_000
	weekMs    = 7 * 86_400_000
	dayMs     = 86_400_000
)

var planNames = []struct {
	pattern *regexp.Regexp
	name    string
}{
	{regexp.MustCompile(`max.?20x`), "Max 20x"},
	{regexp.MustCompile(`max.?5x`), "Max 5x"},
	{regexp.MustCompile(`max`), "Max"},
	{regexp.MustCompile(`pro`), "Pro"},
}

func planOf(tier *string) string {
	if tier == nil || *tier == "" {
		return ""
	}
	for _, plan := range planNames {
		if plan.pattern.MatchString(*tier) {
			return plan.name
		}
	}
	return *tier
}

func windowOf(w *coreauth.PoolWindow, sampledAt int64, length, cutoff float64) LimitWindow {
	if w == nil {
		return LimitWindow{Cutoff: cutoff}
	}
	out := LimitWindow{Used: ptr(w.Used), Cutoff: cutoff, SampledAt: &sampledAt}
	if w.ResetsAt != 0 {
		out.ResetsAt = ptr(w.ResetsAt)
		elapsed := math.Round(math.Min(1, math.Max(0, 1-float64(w.ResetsAt-sampledAt)/length))*1000) / 1000
		out.Elapsed = &elapsed
	}
	return out
}

func reasonsOf(a coreauth.PoolRow, snap coreauth.PoolSnapshot) []string {
	if a.NeedsReauth {
		return []string{"Signed out: sign in again."}
	}
	if a.Disabled {
		return []string{"Turned off."}
	}
	if !a.Usable {
		five := 0.0
		if a.Reading.FiveHour != nil {
			five = a.Reading.FiveHour.Used
		}
		if five >= snap.SessionCutoff {
			return []string{fmt.Sprintf("Past the %s%% five-hour cutoff.", number(snap.SessionCutoff))}
		}
		return []string{fmt.Sprintf("Past the %s%% weekly cutoff.", number(snap.WeeklyCutoff))}
	}
	var reasons []string
	if a.HoldsReset {
		reasons = append(reasons, "Holds a limit reset, so its week gets used first.")
	}
	if a.Comfortable {
		return append(reasons, "Has room for another working session.")
	}
	return append(reasons, "Crowded for its five-hour room.")
}

// number prints a float the way JavaScript does in a template string.
func number(v float64) string { return string(encode(v)) }

func standingOf(a coreauth.PoolRow, rank int) string {
	switch {
	case a.NeedsReauth || a.Disabled:
		return "signed-out"
	case rank == 0 && a.Usable:
		return "next"
	case a.Usable && a.Comfortable:
		return "open"
	case a.Usable || a.Open:
		return "tight"
	}
	return "limited"
}

func poolOf(snap coreauth.PoolSnapshot, revision string) Pool {
	pool := Pool{Decision: snap.Decision, Order: []string{}, Revision: revision, Notes: []string{},
		Policy: Policy{SessionCutoff: snap.SessionCutoff, WeeklyCutoff: snap.WeeklyCutoff, AgentsPerAccount: snap.AgentsPerAccount}}
	if len(snap.Accounts) > 0 && snap.Accounts[0].Usable {
		pool.Next = ptr(snap.Accounts[0].Account)
	}
	for _, a := range snap.Accounts {
		pool.Order = append(pool.Order, a.Account)
	}
	return pool
}

func parsePool(snap coreauth.PoolSnapshot, revision string, seats []seat) *poolState {
	state := &poolState{pool: poolOf(snap, revision), seats: seats}
	for rank, a := range snap.Accounts {
		account := &Account{
			ID: a.Account, Label: a.Email, Rank: ptr(rank + 1), Standing: standingOf(a, rank), Reasons: reasonsOf(a, snap),
			FiveHour:   windowOf(a.Reading.FiveHour, a.Reading.At, fiveHours, snap.SessionCutoff),
			Week:       windowOf(a.Reading.Week, a.Reading.At, weekMs, snap.WeeklyCutoff),
			Working:    a.Working,
			HeldResets: []HeldReset{},
			AtLimit:    a.AtLimit,
		}
		if a.Plan != nil {
			account.Plan = planOf(a.Plan.Tier)
			account.Since = a.Plan.Since
		}
		if a.SignInExpires != "" {
			if at := parseMillis(a.SignInExpires); !math.IsNaN(at) && at != 0 {
				account.SignInUntil = ptr(int64(at))
			}
		}
		seated := 0
		for _, s := range seats {
			if s.account == a.Account {
				seated++
			}
		}
		account.Idle = seated - a.Working
		for _, r := range a.Resets {
			account.HeldResets = append(account.HeldResets, HeldReset{Kind: r.Kind, Left: r.Left, ExpiresAt: r.ExpiresAt})
		}
		state.accounts = append(state.accounts, account)
	}
	return state
}

// seatsOf is each session's latest account, for sessions the proxy saw in the last day.
func seatsOf(rows []Binding, now time.Time) []seat {
	since := float64(now.UnixMilli() - dayMs)
	var seats []seat
	at := map[string]int{}
	for _, row := range rows {
		if row.AccountID == nil || *row.AccountID == "" || row.millis < since {
			continue
		}
		if i, ok := at[row.SessionID]; ok {
			seats[i].account = *row.AccountID
			continue
		}
		at[row.SessionID] = len(seats)
		seats = append(seats, seat{session: row.SessionID, account: *row.AccountID})
	}
	return seats
}

// Move is a session starting on, or moving to, an account.
type Move struct {
	At      float64 `json:"at"`
	Session string  `json:"session"`
	Label   *string `json:"label"`
	From    *string `json:"from"`
	To      string  `json:"to"`
	Cause   string  `json:"cause"`
}

// HistoryLine is one account's readings of one limit.
type HistoryLine struct {
	Account string       `json:"account"`
	Kind    string       `json:"kind"`
	Points  [][2]float64 `json:"points"`
}

// Host is a machine whose sessions the pool serves.
type Host struct {
	Host   string `json:"host"`
	SeenAt *int64 `json:"seenAt"`
	Seats  int    `json:"seats"`
}

// AccountsView is the Accounts page: the last week of readings and the last day of moves.
type AccountsView struct {
	History []HistoryLine `json:"history"`
	Moves   []Move        `json:"moves"`
	Hosts   []Host        `json:"hosts"`
}

func historyOf(readings []LimitReading, now time.Time) []HistoryLine {
	from := float64(now.UnixMilli() - weekMs)
	lines := []HistoryLine{}
	at := map[[2]string]int{}
	for _, r := range readings {
		if r.Account == "" || r.At < from {
			continue
		}
		for _, kind := range limitKinds {
			used := r.value(kind)
			if used == nil {
				continue
			}
			key := [2]string{r.Account, kind}
			i, ok := at[key]
			if !ok {
				i = len(lines)
				at[key] = i
				lines = append(lines, HistoryLine{Account: r.Account, Kind: kind})
			}
			lines[i].Points = append(lines[i].Points, [2]float64{r.At, *used})
		}
	}
	return lines
}

// movesOf lists starts and moves of the last day, newest first, from the pool's per-session account log.
func movesOf(rows []Binding, now time.Time) []Move {
	since := float64(now.UnixMilli() - dayMs)
	moves := []Move{}
	last := map[string]string{}
	for _, row := range rows {
		if row.AccountID == nil || *row.AccountID == "" {
			continue
		}
		before, had := last[row.SessionID]
		last[row.SessionID] = *row.AccountID
		if (had && before == *row.AccountID) || row.millis < since {
			continue
		}
		move := Move{At: row.millis, Session: row.SessionID, To: *row.AccountID, Cause: "start"}
		if had {
			move.From = ptr(before)
			move.Cause = row.Cause
			if move.Cause == "" {
				move.Cause = "limit"
			}
		}
		moves = append(moves, move)
	}
	sort.SliceStable(moves, func(i, j int) bool { return moves[i].At > moves[j].At })
	return moves
}

var errAgents = errors.New("Agents per account must be a whole number from 1 to 500.")

func validPolicy(p Policy) error {
	for _, v := range []float64{p.SessionCutoff, p.WeeklyCutoff} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 100 {
			return fmt.Errorf("Cutoffs must be above 0 and at most 100.")
		}
	}
	if p.AgentsPerAccount < 1 || p.AgentsPerAccount > 500 {
		return errAgents
	}
	return nil
}
