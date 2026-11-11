package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// A held weekly reset zeroes an account's week and Fable's allowance in it; a Fable reset zeroes
// Fable's own allowance and gives back to the week only what Fable used in it. The pool spends a
// weekly reset the moment every Claude account has used its week, and a Fable reset, or a weekly
// one whose own week is used, the moment none has Fable left: before that, the room it gives
// would only stand in for room other accounts already have, which then goes unused. A reset
// refills the week without moving its end, and what is left at the end is lost, so one reset is
// spent per dry spell: the one that gives back the most, then the one whose account's week ends
// last. One whose own week ends within poolRunway is kept,
// unless it expires within poolLastWeek. A reset in its last poolLastHour is spent whatever the
// pool holds.
//
// Anthropic lets some resets be used only at the weekly limit. Their holder takes new work
// past the weekly cutoff, up to that limit, so the reset becomes usable; one whose holder
// never gets there before it expires is lost.
//
// Anthropic's usage endpoint can trail a spend, so for poolSettle after one, a reading that
// still shows the week as it was is stale. A claim keeps its request id, on disk in
// poolClaimsFile, until Anthropic settles it, so retrying it can't spend a second reset.

const (
	poolRunway       = 12 * time.Hour
	poolLastWeek     = 7 * 24 * time.Hour
	poolLastHour     = time.Hour
	poolFreshResets  = 2 * time.Minute
	poolClaimBackoff = 5 * time.Minute
	poolSettle       = time.Hour
	poolResetsFile   = "resets-spent.ndjson"
	poolClaimsFile   = "reset-claims.json"
	// WHY: Fable's allowance as a fraction of the week until the usage endpoint reports it
	// (weekly_scoped_shares); it reported 0.5 on all five Max 20x accounts on Nov 10 2026.
	poolFableShare = 0.5
)

// poolClaim is a claim Anthropic has not settled, retried under the same request id.
type poolClaim struct {
	Grant     string `json:"grant"`
	RequestID string `json:"requestId"`
	Sent      bool   `json:"sent"`
	retryAt   int64
	told      bool
}

// poolSpent is a reset the pool spent, which the usage endpoint may not show yet.
type poolSpent struct {
	week            float64
	weekEnds, until int64
	fable           bool
}

// poolSpend is a held reset the pool would spend now, or why it keeps it.
type poolSpend struct {
	row   *PoolRow
	grant PoolReset
	why   string
	plan  string
}

func (r PoolReset) live(now time.Time) bool {
	return r.Left > 0 && !r.Paused && (r.Weekly || r.Fable && r.FableUsed > 0) && (r.ExpiresAt == nil || *r.ExpiresAt > now.UnixMilli())
}

// GivesBack is how many weekly points spending r now would give back to a week at week used.
func (r PoolReset) GivesBack(week float64) float64 {
	if r.Fable {
		share := r.Share
		if share <= 0 {
			share = poolFableShare
		}
		return min(week, r.FableUsed*share)
	}
	return week
}

func (r PoolReset) left(now time.Time) time.Duration {
	if r.ExpiresAt == nil {
		return math.MaxInt64
	}
	return time.Duration(*r.ExpiresAt-now.UnixMilli()) * time.Millisecond
}

// nextReset is the account's live weekly reset that expires first.
func (r PoolRow) nextReset(now time.Time) (PoolReset, bool) {
	var next PoolReset
	for _, g := range r.Resets {
		if g.live(now) && (next.ID == "" || g.left(now) < next.left(now)) {
			next = g
		}
	}
	return next, next.ID != ""
}

func (r PoolRow) label() string {
	if r.Email != "" {
		return r.Email
	}
	return r.Account
}

func when(ms int64) string {
	return time.UnixMilli(ms).In(time.Local).Format("Mon Jan 2 3:04pm")
}

// settled is r with a week that still shows the usage from before a spent reset replaced by
// the week the reset started.
func (a *poolAccount) settled(r PoolReading, now time.Time) (PoolReading, bool) {
	if s := a.spent; s != nil && now.UnixMilli() < s.until && r.Week != nil && r.Week.Used >= s.week {
		if !s.fable {
			r.Week = &PoolWindow{Used: 0, ResetsAt: s.weekEnds}
		}
		return r, true
	}
	return r, false
}

// planResetsLocked decides from ranked rows, for every account holding a weekly reset,
// whether to spend it now.
func (p *claudePoolState) planResetsLocked(rows []PoolRow, now time.Time) []poolSpend {
	var holders []poolSpend
	var busy, fableBusy []string
	for i := range rows {
		r := &rows[i]
		if r.Disabled || r.NeedsReauth {
			continue
		}
		if !r.Known || r.week < r.weekCutoff || r.settling {
			busy = append(busy, r.label())
		}
		if !r.Known || r.week < r.weekCutoff && r.fable < p.cfg.WeeklyCutoff || r.settling {
			fableBusy = append(fableBusy, r.label())
		}
		if g, ok := r.nextReset(now); ok {
			holders = append(holders, poolSpend{row: r, grant: g})
		}
	}
	sort.SliceStable(holders, func(i, j int) bool {
		a, b := holders[i], holders[j]
		if back, other := a.grant.GivesBack(a.row.week), b.grant.GivesBack(b.row.week); back != other {
			return back > other
		}
		return a.row.WeekResetsAt > b.row.WeekResetsAt
	})
	spend := func(h *poolSpend, why string) { h.why, h.plan = why, "Spending it now: "+why+"." }

	spent := false
	for i := range holders {
		h := &holders[i]
		if h.grant.left(now) >= poolLastHour || h.row.week <= 0 {
			continue
		}
		expires := when(*h.grant.ExpiresAt)
		if h.grant.NeedsLimit && h.row.week < 100 {
			h.plan = fmt.Sprintf("Expires %s and only works at the weekly limit, so %s takes new sessions first to reach it.", expires, h.row.label())
			continue
		}
		spend(h, "the reset expires "+expires)
		spent = true
	}

	for i := range holders {
		h := &holders[i]
		left := h.grant.left(now)
		waiting, used := busy, "week"
		if len(busy) > 0 && (h.grant.Fable || h.row.week >= h.row.weekCutoff) {
			waiting, used = fableBusy, "week or Fable allowance"
		}
		switch {
		case h.plan != "":
		case len(waiting) > 0:
			one := len(waiting) == 1
			h.plan = fmt.Sprintf("Spends itself once %s %s used %s %s.", strings.Join(waiting, " and "),
				map[bool]string{true: "has", false: "have"}[one], map[bool]string{true: "its", false: "their"}[one], used)
		case spent:
			h.plan = fmt.Sprintf("Every account has used its %s, but another reset is being spent, so this one waits for the next time they all have.", used)
		case h.row.WeekResetsAt-now.UnixMilli() >= poolRunway.Milliseconds():
			spend(h, fmt.Sprintf("every account had used its %s", used))
			spent = true
		case left < poolLastWeek:
			spend(h, fmt.Sprintf("every account had used its %s and the reset expires %s", used, when(*h.grant.ExpiresAt)))
			spent = true
		default:
			h.plan = fmt.Sprintf("Every account has used its %s, but this one's week comes back %s, too soon to use what the reset gives.", used, when(h.row.WeekResetsAt))
		}
		if h.grant.Fable {
			h.plan = fmt.Sprintf("Fable reset, gives back ~%.0f weekly points (what Fable used this week). %s", h.grant.GivesBack(h.row.week), h.plan)
		}
	}
	return holders
}

// spendReset spends every held reset planResetsLocked says to, and lifts the
// manager's cooldown on an account whose reset it spent.
func (p *claudePoolState) spendReset(ctx context.Context, m *Manager, auths []*Auth, now time.Time) {
	enabled := make([]*Auth, 0, len(auths))
	for _, a := range auths {
		if isClaudeAuth(a) && !a.Disabled {
			enabled = append(enabled, a)
		}
	}
	p.mu.Lock()
	if p.cfg.StateDir == "" && p.cfg.AccountsFile == "" {
		p.mu.Unlock()
		return
	}
	p.loadClaimsLocked()
	plans := p.planResetsLocked(p.rankLocked(enabled, now, 1, ""), now)
	p.mu.Unlock()
	for _, plan := range plans {
		if plan.why != "" && p.spendOne(ctx, enabled, plan, now) && m != nil {
			if _, _, err := m.ResetQuota(ctx, plan.row.AuthID); err != nil {
				log.Warnf("pool: lifting %s's cooldown after its reset: %v", plan.row.label(), err)
			}
		}
	}
}

// replanLocked is the plan for one account's reset.
func (p *claudePoolState) replanLocked(auths []*Auth, account string, now time.Time) poolSpend {
	for _, plan := range p.planResetsLocked(p.rankLocked(auths, now, 1, ""), now) {
		if plan.row.Account == account {
			return plan
		}
	}
	return poolSpend{}
}

// spendOne claims plan's reset after rereading the account's grants to make sure Anthropic
// still offers it, and reports whether it was spent.
func (p *claudePoolState) spendOne(ctx context.Context, auths []*Auth, plan poolSpend, now time.Time) bool {
	auth, account := plan.row.auth, plan.row.Account
	token, org := metaString(auth, "access_token"), metaString(auth, "organization_uuid")
	if token == "" || org == "" {
		return false
	}
	p.mu.Lock()
	a := p.account(account)
	backingOff := a.retryAt > now.UnixMilli() || a.claim != nil && a.claim.retryAt > now.UnixMilli()
	fresh := now.UnixMilli()-a.resetsAt <= poolFreshResets.Milliseconds()
	p.mu.Unlock()
	if backingOff {
		return false
	}
	if !fresh {
		p.readUsage(ctx, account, token, now)
		p.mu.Lock()
		read := now.UnixMilli()-a.resetsAt <= poolFreshResets.Milliseconds()
		plan = p.replanLocked(auths, account, now)
		p.mu.Unlock()
		if !read || plan.why == "" {
			return false
		}
	}

	p.mu.Lock()
	if c := a.claim; c != nil && c.Sent && c.Grant != plan.grant.ID {
		for _, g := range a.resets {
			if g.ID == c.Grant && g.live(now) {
				plan.grant = g
			}
		}
	}
	if a.claim == nil || a.claim.Grant != plan.grant.ID {
		a.claim = &poolClaim{Grant: plan.grant.ID, RequestID: uuid.NewString()}
		p.saveClaimsLocked()
	}
	claim := a.claim
	command := p.cfg.ResetCommand
	tell := func(line string) {
		log.Info("pool: " + line)
		if command != "" {
			go runResetCommand(command, line)
		}
	}
	if !plan.grant.UsableNow {
		told := claim.told
		claim.retryAt, claim.told = now.Add(poolClaimBackoff).UnixMilli(), true
		p.mu.Unlock()
		if !told {
			tell(fmt.Sprintf("Wanted to spend %s's %s reset (%s), but Anthropic says it can't be used right now. Retrying every few minutes.", plan.row.label(), plan.grant.Kind, plan.why))
		}
		return false
	}
	resent := claim.Sent
	claim.Sent = true
	p.saveClaimsLocked()
	p.mu.Unlock()

	out, status := p.claimReset(ctx, token, org, plan.grant.ID, claim.RequestID)
	spent := out.Result == "reset" || out.Result == "already_used" && resent
	settled := out.Result != "unavailable" && out.Result != "" && status != http.StatusTooManyRequests && status < 500
	p.mu.Lock()
	p.appendLocked(poolResetsFile, map[string]any{"at": now.UTC().Format(time.RFC3339Nano), "account": account, "email": plan.row.Email,
		"grant": plan.grant.ID, "kind": plan.grant.Kind, "requestId": claim.RequestID, "status": status, "result": out.Result, "reason": out.Reason,
		"spent": spent, "resetsLeft": out.ResetsLeft, "cleared": out.Cleared, "weeklyResetsAt": out.WeeklyResetsAt, "why": plan.why,
		"week": plan.row.week, "fableUsed": plan.grant.FableUsed, "givesBack": plan.grant.GivesBack(plan.row.week)})
	retry := now.Add(poolClaimBackoff)
	if at := isoMillis(out.CooldownUntil); at != nil && *at > retry.UnixMilli() {
		retry = time.UnixMilli(*at)
	}
	told, weekEnds := claim.told, plan.row.WeekResetsAt
	switch {
	case spent:
		if at := isoMillis(out.WeeklyResetsAt); at != nil {
			weekEnds = *at
		}
		fableCleared := plan.grant.Fable || slices.Contains(out.Cleared, "seven_day_overage_included")
		if !plan.grant.Fable {
			a.reading = PoolReading{FiveHour: plan.row.Reading.FiveHour, Week: &PoolWindow{Used: 0, ResetsAt: weekEnds}, At: now.UnixMilli()}
		}
		if fableCleared && a.fable != nil {
			a.fable, a.fableAt = &PoolWindow{Used: 0, ResetsAt: a.fable.ResetsAt}, now.UnixMilli()
		}
		a.spent = &poolSpent{week: plan.row.week, weekEnds: weekEnds, until: now.Add(poolSettle).UnixMilli(), fable: plan.grant.Fable}
		resets := make([]PoolReset, 0, len(a.resets))
		for _, g := range a.resets {
			if g.ID == plan.grant.ID {
				g.Left--
				if out.ResetsLeft != nil {
					g.Left = *out.ResetsLeft
				}
			}
			if fableCleared && g.Fable {
				g.FableUsed = 0
			}
			if g.Left > 0 {
				resets = append(resets, g)
			}
		}
		a.resets, a.resetsAt, a.claim = resets, 0, nil
	case settled:
		a.resetsAt = 0
		a.claim = &poolClaim{Grant: plan.grant.ID, RequestID: uuid.NewString(), retryAt: retry.UnixMilli(), told: true}
	default:
		claim.retryAt, claim.told = retry.UnixMilli(), true
	}
	p.saveClaimsLocked()
	held := p.heldWeeklyLocked(auths, now)
	p.mu.Unlock()

	switch {
	case spent:
		if plan.grant.Fable {
			tell(fmt.Sprintf("Spent %s's Fable reset: %s. It gives back what Fable used this week, ~%.0f of its %.0f%% week; its week still resets %s.", plan.row.label(), plan.why, plan.grant.GivesBack(plan.row.week), plan.row.week, when(weekEnds)))
		} else {
			tell(fmt.Sprintf("Spent %s's weekly reset: %s. Its week is back to 0%%; next weekly reset %s. %s", plan.row.label(), plan.why, when(weekEnds), held))
		}
	case !told:
		tell(fmt.Sprintf("Tried to spend %s's %s reset (%s); Anthropic answered %q (%s, HTTP %d). Retrying every few minutes.", plan.row.label(), plan.grant.Kind, plan.why, out.Result, out.Reason, status))
	default:
		log.Warnf("pool: reset claim for %s answered %q (%s), status %d", plan.row.label(), out.Result, out.Reason, status)
	}
	return spent
}

// heldWeeklyLocked says how many weekly resets the pool still holds and who holds them, so a
// spend never reads as the pool running out while fresh weeks remain.
func (p *claudePoolState) heldWeeklyLocked(auths []*Auth, now time.Time) string {
	count := 0
	var holders []string
	for _, auth := range auths {
		held := 0
		for _, g := range p.account(poolAccountID(auth)).resets {
			if g.Weekly && g.live(now) {
				held += g.Left
			}
		}
		if held > 0 {
			count += held
			holders = append(holders, metaString(auth, "email"))
		}
	}
	switch count {
	case 0:
		return "No weekly resets are left."
	case 1:
		return "1 weekly reset is still held, a fresh week on " + holders[0] + "."
	}
	return fmt.Sprintf("%d weekly resets are still held, a fresh week each, on %s.", count, strings.Join(holders, ", "))
}

// loadClaimsLocked reads the claims a previous run sent and Anthropic hadn't settled.
func (p *claudePoolState) loadClaimsLocked() {
	if p.claimsRead || p.cfg.StateDir == "" {
		return
	}
	p.claimsRead = true
	data, err := os.ReadFile(filepath.Join(p.cfg.StateDir, poolClaimsFile))
	if err != nil {
		return
	}
	claims := map[string]*poolClaim{}
	if err := json.Unmarshal(data, &claims); err != nil {
		log.Warnf("pool: unreadable %s: %v", poolClaimsFile, err)
		return
	}
	for account, c := range claims {
		if c != nil && c.Grant != "" && c.RequestID != "" {
			p.account(account).claim = c
		}
	}
}

func (p *claudePoolState) saveClaimsLocked() {
	if p.cfg.StateDir == "" {
		return
	}
	claims := map[string]*poolClaim{}
	for account, a := range p.accounts {
		if a.claim != nil && a.claim.Sent {
			claims[account] = a.claim
		}
	}
	writeJSONFile(filepath.Join(p.cfg.StateDir, poolClaimsFile), claims)
}

type resetAnswer struct {
	Result         string   `json:"result"`
	Reason         string   `json:"reason"`
	ResetsLeft     *int     `json:"resets_left"`
	Cleared        []string `json:"cleared"`
	WeeklyResetsAt *string  `json:"weekly_resets_at"`
	CooldownUntil  *string  `json:"cooldown_until"`
}

// claimReset spends a reset the way Claude Code's /limit-reset does.
func (p *claudePoolState) claimReset(ctx context.Context, token, org, grant, requestID string) (resetAnswer, int) {
	body, _ := json.Marshal(map[string]string{"program": "cedar_ember", "grant_id": grant, "request_id": requestID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicAPI+"/api/organizations/"+org+"/reset_rate_limits", bytes.NewReader(body))
	if err != nil {
		return resetAnswer{}, 0
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.317 (external, cli)")
	resp, err := p.client.Do(req)
	if err != nil {
		return resetAnswer{}, 0
	}
	defer resp.Body.Close()
	var out resetAnswer
	if resp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return out, resp.StatusCode
}

func runResetCommand(command, line string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "sh", "-c", command+` "$1"`, "sh", line).CombinedOutput(); err != nil {
		log.Warnf("pool: reset-command failed: %v: %s", err, out)
	}
}

// spentLocked is when the pool spent each account's resets, oldest first (unix ms).
func (p *claudePoolState) spentLocked() map[string][]int64 {
	out := map[string][]int64{}
	if p.cfg.StateDir == "" {
		return out
	}
	data, _ := os.ReadFile(filepath.Join(p.cfg.StateDir, poolResetsFile))
	for _, line := range strings.Split(string(data), "\n") {
		var row struct {
			At      time.Time `json:"at"`
			Account string    `json:"account"`
			Result  string    `json:"result"`
			Spent   bool      `json:"spent"`
		}
		if json.Unmarshal([]byte(line), &row) == nil && (row.Result == "reset" || row.Spent) {
			out[row.Account] = append(out[row.Account], row.At.UnixMilli())
		}
	}
	return out
}
