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

// A held weekly reset zeroes an account's week. The pool spends one the moment every Claude
// account, the holder included, has used its week: before that, the room it gives would only
// stand in for room other accounts already have, which then goes unused. It waits out a dry
// spell shorter than poolDrySpell, when a week comes back on its own soon, unless the reset
// expires within poolLastWeek. In the reset's last poolLastHour it is spent whatever the pool
// holds, so it is never lost.

const (
	poolDrySpell     = 12 * time.Hour
	poolLastWeek     = 7 * 24 * time.Hour
	poolLastHour     = time.Hour
	poolFreshResets  = 2 * time.Minute
	poolClaimBackoff = 5 * time.Minute
	poolResetsFile   = "resets-spent.ndjson"
)

// poolClaim is a claim Anthropic has not settled, retried under the same request id.
type poolClaim struct {
	grant, requestID string
	retryAt          int64
	told             bool
}

// poolSpend is the reset the pool would spend now, or why it keeps them.
type poolSpend struct {
	row   *PoolRow
	grant PoolReset
	why   string
	plan  string
}

func (r PoolReset) live(now time.Time) bool {
	return r.Left > 0 && !r.Paused && r.Weekly && (r.ExpiresAt == nil || *r.ExpiresAt > now.UnixMilli())
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

// planResetLocked decides from ranked rows whether to spend a weekly reset now.
func (p *claudePoolState) planResetLocked(rows []PoolRow, now time.Time) poolSpend {
	cutoff := p.cfg.WeeklyCutoff
	var holders, others []*PoolRow
	for i := range rows {
		r := &rows[i]
		if r.Disabled || r.NeedsReauth {
			continue
		}
		held := false
		for _, g := range r.Resets {
			held = held || g.live(now)
		}
		if held {
			holders = append(holders, r)
		} else {
			others = append(others, r)
		}
	}
	if len(holders) == 0 {
		return poolSpend{}
	}
	sort.SliceStable(holders, func(i, j int) bool { return holders[i].WeekResetsAt > holders[j].WeekResetsAt })
	holder := holders[0]
	var grant PoolReset
	for _, g := range holder.Resets {
		if g.live(now) && (grant.ID == "" || g.ExpiresAt != nil && (grant.ExpiresAt == nil || *g.ExpiresAt < *grant.ExpiresAt)) {
			grant = g
		}
	}
	spend := func(why string) poolSpend {
		return poolSpend{row: holder, grant: grant, why: why, plan: "Spending it now: " + why + "."}
	}
	keep := func(plan string) poolSpend { return poolSpend{row: holder, grant: grant, plan: plan} }
	left := time.Duration(math.MaxInt64)
	if grant.ExpiresAt != nil {
		left = time.Duration(*grant.ExpiresAt-now.UnixMilli()) * time.Millisecond
	}
	if left < poolLastHour && holder.week > 0 {
		return spend(fmt.Sprintf("the reset expires %s", when(*grant.ExpiresAt)))
	}

	var busy []string
	if !holder.Known || holder.week < cutoff {
		busy = append(busy, holder.label())
	}
	dryUntil := holder.WeekResetsAt
	for _, r := range append(others, holders[1:]...) {
		if !r.Known || r.week < cutoff {
			busy = append(busy, r.label())
		}
		dryUntil = min(dryUntil, r.WeekResetsAt)
	}
	if len(busy) > 0 {
		return keep(fmt.Sprintf("Spends itself once %s %s used %s week.", strings.Join(busy, " and "),
			map[bool]string{true: "has", false: "have"}[len(busy) == 1], map[bool]string{true: "its", false: "their"}[len(busy) == 1]))
	}
	dry := time.Duration(dryUntil-now.UnixMilli()) * time.Millisecond
	switch {
	case dry >= poolDrySpell:
		return spend(fmt.Sprintf("every account had used its week and none came back before %s", when(dryUntil)))
	case left < poolLastWeek:
		return spend(fmt.Sprintf("every account had used its week and the reset expires %s", when(*grant.ExpiresAt)))
	default:
		return keep(fmt.Sprintf("Every account has used its week, but one comes back %s, so the reset waits for a longer gap.", when(dryUntil)))
	}
}

// spendReset spends a held weekly reset when planResetLocked says so, after rereading the
// holder's grants to make sure Anthropic still offers it.
func (p *claudePoolState) spendReset(ctx context.Context, auths []*Auth, now time.Time) {
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
	plan := p.planResetLocked(p.rankLocked(enabled, now, 1), now)
	if plan.why == "" {
		p.mu.Unlock()
		return
	}
	auth, account := plan.row.auth, plan.row.Account
	a := p.account(account)
	fresh := now.UnixMilli()-a.resetsAt <= poolFreshResets.Milliseconds()
	if a.claim != nil && a.claim.retryAt > now.UnixMilli() {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	token, org := metaString(auth, "access_token"), metaString(auth, "organization_uuid")
	if token == "" || org == "" {
		return
	}
	if !fresh {
		p.readUsage(ctx, account, token, now)
		p.mu.Lock()
		plan = p.planResetLocked(p.rankLocked(enabled, now, 1), now)
		p.mu.Unlock()
		if plan.why == "" || plan.row.Account != account {
			return
		}
	}
	p.mu.Lock()
	if a.claim == nil || a.claim.grant != plan.grant.ID {
		a.claim = &poolClaim{grant: plan.grant.ID, requestID: uuid.NewString()}
	}
	claim := a.claim
	command := p.cfg.ResetCommand
	p.mu.Unlock()
	tell := func(line string) {
		log.Info("pool: " + line)
		if command != "" {
			go runResetCommand(command, line)
		}
	}
	if !plan.grant.UsableNow {
		p.mu.Lock()
		told := claim.told
		claim.retryAt, claim.told = now.Add(poolClaimBackoff).UnixMilli(), true
		p.mu.Unlock()
		if !told {
			tell(fmt.Sprintf("Wanted to spend %s's weekly reset (%s), but Anthropic says it can't be used right now. Retrying every few minutes.", plan.row.label(), plan.why))
		}
		return
	}

	out, status := p.claimReset(ctx, token, org, plan.grant.ID, claim.requestID)
	settled := out.Result != "unavailable" && out.Result != "" && status != http.StatusTooManyRequests && status < 500
	p.mu.Lock()
	p.appendLocked(poolResetsFile, map[string]any{"at": now.UTC().Format(time.RFC3339Nano), "account": account, "email": plan.row.Email,
		"grant": plan.grant.ID, "kind": plan.grant.Kind, "status": status, "result": out.Result, "reason": out.Reason,
		"resetsLeft": out.ResetsLeft, "cleared": out.Cleared, "weeklyResetsAt": out.WeeklyResetsAt, "why": plan.why})
	retry := now.Add(poolClaimBackoff)
	if at := isoMillis(out.CooldownUntil); at != nil && *at > retry.UnixMilli() {
		retry = time.UnixMilli(*at)
	}
	told, weekEnds := claim.told, plan.row.WeekResetsAt
	switch {
	case out.Result == "reset":
		if at := isoMillis(out.WeeklyResetsAt); at != nil {
			weekEnds = *at
		}
		a.reading = PoolReading{FiveHour: plan.row.Reading.FiveHour, Week: &PoolWindow{Used: 0, ResetsAt: weekEnds}, At: now.UnixMilli()}
		a.resets = slices.DeleteFunc(slices.Clone(a.resets), func(r PoolReset) bool { return r.ID == plan.grant.ID })
		a.resetsAt, a.claim = 0, nil
	case settled:
		a.resetsAt = 0
		a.claim = &poolClaim{grant: plan.grant.ID, requestID: uuid.NewString(), retryAt: retry.UnixMilli(), told: true}
	default:
		claim.retryAt, claim.told = retry.UnixMilli(), true
	}
	p.mu.Unlock()

	switch {
	case out.Result == "reset":
		tell(fmt.Sprintf("Spent %s's weekly reset: %s. Its week is back to 0%%; next weekly reset %s.", plan.row.label(), plan.why, when(weekEnds)))
	case !told:
		tell(fmt.Sprintf("Tried to spend %s's weekly reset (%s); Anthropic answered %q (%s, HTTP %d). Retrying every few minutes.", plan.row.label(), plan.why, out.Result, out.Reason, status))
	default:
		log.Warnf("pool: reset claim for %s answered %q (%s), status %d", plan.row.Email, out.Result, out.Reason, status)
	}
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
		}
		if json.Unmarshal([]byte(line), &row) == nil && row.Result == "reset" {
			out[row.Account] = append(out[row.Account], row.At.UnixMilli())
		}
	}
	return out
}
