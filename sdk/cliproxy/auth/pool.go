package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/entrypoint"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// The pool spends every Claude account's weekly limit before it resets without letting
// any one hit its five-hour limit. It ranks accounts for new sessions (QuotaSelector),
// moves a bound session off an account nearing a cutoff, samples each account's limits,
// held resets and plan from Anthropic, spends a held weekly reset when the pool needs it
// (pool_resets.go), and records which account each Claude Code session runs on.
// Agent SDK and `claude -p` sessions drain the five-hour limit faster than interactive
// ones, so they weigh routing.pool.agent-sdk-five-hour-factor sessions each there.

const (
	poolStaleAfter    = 20 * time.Minute
	poolCloudEvery    = 5 * time.Minute
	poolResetsEvery   = time.Hour
	poolPlanEvery     = 24 * time.Hour
	poolLogEvery      = 15 * time.Minute
	poolWorkingWindow = 2 * time.Minute
	poolMargin        = 10.0
	poolWeek          = 7 * 24 * time.Hour
	anthropicAPI      = "https://api.anthropic.com"
)

var sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// PoolWindow is one limit reading: percent used and when the window resets (unix ms, 0 unknown).
type PoolWindow struct {
	Used     float64 `json:"used"`
	ResetsAt int64   `json:"resetsAt"`
}

// PoolReading is an account's latest five-hour and weekly readings. A nil window has not started.
type PoolReading struct {
	FiveHour *PoolWindow `json:"fiveHour"`
	Week     *PoolWindow `json:"week"`
	At       int64       `json:"at"`
}

// PoolCredit is an upstream dollar balance, sampled independently of response-header limits.
type PoolCredit struct {
	Limit     *float64 `json:"limit"`
	Used      *float64 `json:"used"`
	Remaining *float64 `json:"remaining"`
	ExpiresAt *int64   `json:"expiresAt"`
	SampledAt int64    `json:"sampledAt"`
}

// PoolReset is a limit reset the account holds.
type PoolReset struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Left      int    `json:"left"`
	ExpiresAt *int64 `json:"expiresAt"`
	Paused    bool   `json:"paused"`
	UsableNow bool   `json:"usableNow"`
	// NeedsLimit is set when Anthropic lets the reset be used only at a limit it refills.
	NeedsLimit bool `json:"needsLimit"`
	Weekly     bool `json:"weekly"`
}

// PoolPlan is the account's subscription, from its profile.
type PoolPlan struct {
	Tier   *string `json:"tier"`
	Status *string `json:"status"`
	Since  *int64  `json:"since"`
	At     int64   `json:"at"`
}

type poolAccount struct {
	cloud       *PoolCredit
	reading     PoolReading
	logged      PoolReading
	resets      []PoolReset
	atLimit     any
	resetsAt    int64
	plan        *PoolPlan
	retryAt     int64
	needsReauth bool
	claim       *poolClaim
	spent       *poolSpent
}

type poolSeat struct {
	account    string
	entrypoint string
	seen       time.Time
}

type claudePoolState struct {
	mu         sync.Mutex
	cfg        config.PoolConfig
	accounts   map[string]*poolAccount
	sessions   map[string]*poolSeat
	listed     string
	client     *http.Client
	claimsRead bool
}

var claudePool = &claudePoolState{
	cfg:      normalizePoolConfig(config.PoolConfig{}),
	accounts: map[string]*poolAccount{},
	sessions: map[string]*poolSeat{},
	client:   &http.Client{Timeout: 15 * time.Second},
}

func normalizePoolConfig(cfg config.PoolConfig) config.PoolConfig {
	if cfg.SessionCutoff <= 0 || cfg.SessionCutoff > 100 {
		cfg.SessionCutoff = 90
	}
	if cfg.WeeklyCutoff <= 0 || cfg.WeeklyCutoff > 100 {
		cfg.WeeklyCutoff = 98
	}
	if cfg.AgentsPerAccount <= 0 {
		cfg.AgentsPerAccount = 100
	}
	cfg.StateDir = expandHome(cfg.StateDir)
	cfg.AccountsFile = expandHome(cfg.AccountsFile)
	return cfg
}

func expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

// ConfigurePool applies the pool settings from routing.pool.
func ConfigurePool(cfg config.PoolConfig) {
	claudePool.mu.Lock()
	claudePool.cfg = normalizePoolConfig(cfg)
	claudePool.mu.Unlock()
}

func isClaudeAuth(auth *Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "claude")
}

func metaString(auth *Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	value, _ := auth.Metadata[key].(string)
	return strings.TrimSpace(value)
}

// poolAccountID is the Claude account UUID, falling back to the auth ID before the profile is read.
func poolAccountID(auth *Auth) string {
	if id := metaString(auth, "account_uuid"); id != "" {
		return id
	}
	return auth.ID
}

func (p *claudePoolState) account(id string) *poolAccount {
	a := p.accounts[id]
	if a == nil {
		a = &poolAccount{}
		p.accounts[id] = a
	}
	return a
}

// headerReading turns the last response's anthropic-ratelimit-unified-* headers into a reading.
func headerReading(auth *Auth) PoolReading {
	signals := auth.Quota.Signals
	window := func(name string) *PoolWindow {
		used, err := strconv.ParseFloat(signals["Anthropic-Ratelimit-Unified-"+name+"-Utilization"], 64)
		if err != nil || math.IsNaN(used) {
			return nil
		}
		reset, _ := strconv.ParseInt(signals["Anthropic-Ratelimit-Unified-"+name+"-Reset"], 10, 64)
		return &PoolWindow{Used: used * 100, ResetsAt: reset * 1000}
	}
	r := PoolReading{FiveHour: window("5h"), Week: window("7d")}
	if r.FiveHour != nil || r.Week != nil {
		r.At = auth.Quota.ObservedAt.UnixMilli()
	}
	return r
}

// readingLocked is the freshest of the sampled and header readings, with windows that have
// since reset counted as empty.
func (p *claudePoolState) readingLocked(auth *Auth, now time.Time) PoolReading {
	a := p.account(poolAccountID(auth))
	r := a.reading
	if h := headerReading(auth); h.At > r.At {
		r = h
	}
	r, _ = a.settled(r, now)
	live := func(w *PoolWindow) *PoolWindow {
		if w != nil && w.ResetsAt > 0 && w.ResetsAt <= now.UnixMilli() {
			return &PoolWindow{Used: 0}
		}
		return w
	}
	r.FiveHour, r.Week = live(r.FiveHour), live(r.Week)
	return r
}

// PoolRow is one account's standing for new work, in rank order.
type PoolRow struct {
	Cloud         *PoolCredit `json:"cloud"`
	AuthID        string      `json:"authId"`
	Account       string      `json:"account"`
	Email         string      `json:"email"`
	Reading       PoolReading `json:"reading"`
	Usable        bool        `json:"usable"`
	Open          bool        `json:"open"`
	Known         bool        `json:"known"`
	Comfortable   bool        `json:"comfortable"`
	HoldsReset    bool        `json:"holdsReset"`
	Working       int         `json:"working"`
	Room          float64     `json:"room"`
	Urgency       float64     `json:"urgency"`
	WeekResetsAt  int64       `json:"weekResetsAt"`
	FreeAt        int64       `json:"freeAt"`
	Resets        []PoolReset `json:"resets"`
	AtLimit       any         `json:"atLimit"`
	Plan          *PoolPlan   `json:"plan"`
	NeedsReauth   bool        `json:"needsReauth"`
	Disabled      bool        `json:"disabled"`
	SignInExpires string      `json:"signInExpires"`
	ResetPlan     string      `json:"resetPlan,omitempty"`
	ResetsSpent   []int64     `json:"resetsSpent,omitempty"`
	auth          *Auth
	five, week    float64
	weekCutoff    float64
}

// workingLocked counts the account's working sessions, and their five-hour load with
// Agent SDK sessions weighted.
func (p *claudePoolState) workingLocked(account string, now time.Time) (int, float64) {
	n, load := 0, 0.0
	for _, seat := range p.sessions {
		if seat.account == account && now.Sub(seat.seen) <= poolWorkingWindow {
			n++
			load += entrypoint.Weight(seat.entrypoint, p.cfg.AgentSDKFactor())
		}
	}
	return n, load
}

// sessionWeight is the five-hour weight of the session a request belongs to.
func (p *claudePoolState) sessionWeight(headers http.Header) float64 {
	return entrypoint.Weight(entrypoint.FromUserAgent(headers.Get("User-Agent")), p.cfg.AgentSDKFactor())
}

func nextReset(at int64, now time.Time, period time.Duration) int64 {
	if at <= 0 {
		return 0
	}
	ms, step := now.UnixMilli(), period.Milliseconds()
	for at <= ms {
		at += step
	}
	return at
}

// rankLocked orders Claude accounts the way tokenhub did: accounts under both cutoffs first;
// among them one holding a reset, so its week runs out first and the reset clears a whole
// one, then those with their share of five-hour room per working session, then sampled ones; then the account whose remaining week runs out soonest, then
// the most five-hour room per working session. With nothing open, the most real room wins.
// Room is shared by load, the weighted working sessions plus the incoming one's weight; an
// account whose plan has no five-hour limit, like a verified Ultra one, counts sessions alike.
func (p *claudePoolState) rankLocked(auths []*Auth, now time.Time, weight float64) []PoolRow {
	cfg := p.cfg
	rows := make([]PoolRow, 0, len(auths))
	for _, auth := range auths {
		if !isClaudeAuth(auth) {
			continue
		}
		id := poolAccountID(auth)
		a := p.account(id)
		r := p.readingLocked(auth, now)
		incoming := weight
		row := PoolRow{AuthID: auth.ID, Account: id, Email: metaString(auth, "email"), Reading: r, auth: auth,
			Cloud: a.cloud, Resets: a.resets, AtLimit: a.atLimit, Plan: a.plan, NeedsReauth: a.needsReauth, Disabled: auth.Disabled,
			SignInExpires: metaString(auth, "refresh_expired")}
		var load float64
		row.Working, load = p.workingLocked(id, now)
		if r.FiveHour == nil && r.At > 0 {
			load, incoming = float64(row.Working), 1
		}
		if r.FiveHour != nil {
			row.five = r.FiveHour.Used
		}
		if r.Week != nil {
			row.week = r.Week.Used
		}
		row.Known = r.At > 0
		for _, reset := range a.resets {
			if reset.Left > 0 && !reset.Paused && (reset.ExpiresAt == nil || *reset.ExpiresAt > now.UnixMilli()) {
				row.HoldsReset = true
			}
		}
		row.weekCutoff = cfg.WeeklyCutoff
		for _, reset := range a.resets {
			if reset.NeedsLimit && reset.live(now) {
				row.weekCutoff = 100
			}
		}
		fiveRoom, weekRoom := cfg.SessionCutoff-row.five, row.weekCutoff-row.week
		row.Usable = fiveRoom > 0 && weekRoom > 0 && !a.needsReauth
		row.Room = fiveRoom / (load + incoming)
		var weekReset int64
		if r.Week != nil {
			weekReset = nextReset(r.Week.ResetsAt, now, poolWeek)
		}
		if weekReset == 0 {
			weekReset = now.Add(poolWeek).UnixMilli()
		}
		row.WeekResetsAt = weekReset
		row.Urgency = weekRoom / math.Max(1, float64(weekReset-now.UnixMilli())/3.6e6)
		row.Comfortable = row.Room >= cfg.SessionCutoff/float64(cfg.AgentsPerAccount) && weekRoom >= 5
		row.Open = row.five < 100 && row.week < 100 && !a.needsReauth
		if fiveRoom <= 0 && r.FiveHour != nil {
			row.FreeAt = nextReset(r.FiveHour.ResetsAt, now, 5*time.Hour)
		}
		if weekRoom <= 0 && weekReset > row.FreeAt {
			row.FreeAt = weekReset
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case a.Usable != b.Usable:
			return a.Usable
		case !a.Usable && a.Open != b.Open:
			return a.Open
		case !a.Usable && a.Open:
			return a.five < b.five
		case !a.Usable:
			return a.FreeAt < b.FreeAt
		case a.HoldsReset != b.HoldsReset:
			return a.HoldsReset
		case a.Comfortable != b.Comfortable:
			return a.Comfortable
		case a.Known != b.Known:
			return a.Known
		case a.Comfortable && a.Urgency != b.Urgency:
			return a.Urgency > b.Urgency
		case a.Room != b.Room:
			return a.Room > b.Room
		default:
			return a.Urgency > b.Urgency
		}
	})
	return rows
}

// QuotaSelector picks the Claude account tokenhub's policy ranks first. It decides cold
// bindings and rebinds when used under session affinity.
type QuotaSelector struct{}

// Pick implements Selector.
func (s *QuotaSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, time.Now())
	if err != nil {
		return nil, err
	}
	claudePool.mu.Lock()
	rows := claudePool.rankLocked(available, time.Now(), claudePool.sessionWeight(opts.Headers))
	claudePool.mu.Unlock()
	if len(rows) > 0 {
		return rows[0].auth, nil
	}
	return available[0], nil
}

// poolAfterPick moves a session off an account nearing a cutoff when another account is
// clear, then records which account the session runs on. pick reruns the affinity
// selector, which rebinds the session once its bound account is left out.
func poolAfterPick(opts cliproxyexecutor.Options, auths []*Auth, auth *Auth, quota bool, pick func([]*Auth) (*Auth, error)) *Auth {
	if !isClaudeAuth(auth) {
		return auth
	}
	cause := "limit"
	if quota {
		if rest, level := claudePool.moveAway(auth, auths, opts.Headers); rest != nil {
			if moved, err := pick(rest); err == nil && moved != nil {
				log.Infof("pool: moving session off %s, nearing its limits, to %s", metaString(auth, "email"), metaString(moved, "email"))
				auth, cause = moved, level
			}
		}
	}
	claudePool.seat(opts.Headers, auth, cause)
	return auth
}

// moveAway returns the other accounts and "over" (past a cutoff) or "early" (nearing one)
// when the session should leave auth for a clearer account. An Agent SDK session nears the
// five-hour cutoff faster, so its margin before the cutoff is wider by the same factor.
func (p *claudePoolState) moveAway(auth *Auth, auths []*Auth, headers http.Header) ([]*Auth, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg, now, weight := p.cfg, time.Now(), p.sessionWeight(headers)
	rows := p.rankLocked(auths, now, weight)
	calm := func(r PoolRow) bool {
		return r.Usable && r.Comfortable && r.five < cfg.SessionCutoff-poolMargin && r.weekCutoff-r.week >= 5
	}
	var mine *PoolRow
	for i := range rows {
		if rows[i].AuthID == auth.ID {
			mine = &rows[i]
		}
	}
	if mine == nil {
		return nil, ""
	}
	urgent := !mine.Usable
	soon := mine.five >= cfg.SessionCutoff-poolMargin*weight || mine.weekCutoff-mine.week < 5
	if !urgent && !soon {
		return nil, ""
	}
	better := false
	for _, r := range rows {
		if r.AuthID != auth.ID && (calm(r) || (urgent && r.Usable)) {
			better = true
		}
	}
	if !better {
		return nil, ""
	}
	rest := make([]*Auth, 0, len(auths))
	for _, a := range auths {
		if a != nil && a.ID != auth.ID {
			rest = append(rest, a)
		}
	}
	if urgent {
		return rest, "over"
	}
	return rest, "early"
}

// seat records the account a Claude Code session runs on: "start" for its first binding,
// "failure" when it changes, with why (limit, early, over), and the session's entrypoint.
func (p *claudePoolState) seat(headers http.Header, auth *Auth, cause string) {
	sessionID := strings.ToLower(strings.TrimSpace(headers.Get("X-Claude-Code-Session-Id")))
	if !sessionIDPattern.MatchString(sessionID) {
		return
	}
	account := poolAccountID(auth)
	via := entrypoint.FromUserAgent(headers.Get("User-Agent"))
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	seat := p.sessions[sessionID]
	if seat == nil || seat.account != account {
		row := map[string]any{"at": now.UTC().Format(time.RFC3339Nano), "sessionId": sessionID, "accountId": account, "event": "start"}
		if via != "" {
			row["entrypoint"] = via
		}
		if seat != nil {
			row["event"], row["from"], row["cause"] = "failure", seat.account, cause
		}
		p.appendLocked("attribution.ndjson", row)
		seat = &poolSeat{account: account, entrypoint: via}
		p.sessions[sessionID] = seat
	}
	seat.seen = now
	for id, s := range p.sessions {
		if now.Sub(s.seen) > 24*time.Hour {
			delete(p.sessions, id)
		}
	}
}

func (p *claudePoolState) appendLocked(name string, row any) {
	if p.cfg.StateDir == "" {
		return
	}
	line, _ := json.Marshal(row)
	if err := os.MkdirAll(p.cfg.StateDir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(p.cfg.StateDir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Warnf("pool: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func writeJSONFile(path string, value any) {
	if path == "" {
		return
	}
	data, _ := json.MarshalIndent(value, "", " ")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, path)
	}
}

// logReadingLocked appends a usage-history line when either percentage moves, and at least
// every 15 minutes while readings keep arriving.
func (p *claudePoolState) logReadingLocked(account string, r PoolReading) {
	a := p.account(account)
	pct := func(w *PoolWindow) *float64 {
		if w == nil {
			return nil
		}
		return &w.Used
	}
	reset := func(w *PoolWindow) *int64 {
		if w == nil || w.ResetsAt == 0 {
			return nil
		}
		return &w.ResetsAt
	}
	same := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || math.Abs(*x-*y) < 0.5) }
	if r.At <= a.logged.At || (same(pct(r.FiveHour), pct(a.logged.FiveHour)) && same(pct(r.Week), pct(a.logged.Week)) && r.At-a.logged.At < poolLogEvery.Milliseconds()) {
		return
	}
	a.logged = r
	p.appendLocked("usage-history.ndjson", map[string]any{"account": account, "at": r.At, "fiveHour": pct(r.FiveHour), "week": pct(r.Week), "fiveHourResetsAt": reset(r.FiveHour), "weekResetsAt": reset(r.Week)})
}

// StartPoolSampler reads each Claude account's limits, held resets and plan, every minute
// from response headers and, when those are stale or a cloud credit's balance is five
// minutes old, from Anthropic's usage endpoint.
func StartPoolSampler(ctx context.Context, m *Manager) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			claudePool.sample(ctx, m, m.List())
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (p *claudePoolState) sample(ctx context.Context, m *Manager, auths []*Auth) {
	now := time.Now()
	type job struct {
		auth                *Auth
		account             string
		usage, resets, plan bool
	}
	var jobs []job
	p.mu.Lock()
	enabled := p.cfg.StateDir != "" || p.cfg.AccountsFile != ""
	for _, auth := range auths {
		if !isClaudeAuth(auth) || auth.Disabled {
			continue
		}
		id := poolAccountID(auth)
		a := p.account(id)
		if h := headerReading(auth); h.At > a.reading.At {
			a.reading, _ = a.settled(h, now)
		}
		p.logReadingLocked(id, a.reading)
		if a.retryAt > now.UnixMilli() {
			continue
		}
		j := job{auth: auth, account: id,
			usage: now.UnixMilli()-a.reading.At > poolStaleAfter.Milliseconds() ||
				a.cloud != nil && now.UnixMilli()-a.cloud.SampledAt > poolCloudEvery.Milliseconds(),
			resets: now.UnixMilli()-a.resetsAt > poolResetsEvery.Milliseconds(),
			plan:   a.plan == nil || now.UnixMilli()-a.plan.At > poolPlanEvery.Milliseconds()}
		if enabled && (j.usage || j.resets || j.plan) {
			jobs = append(jobs, j)
		}
	}
	p.mu.Unlock()
	for _, j := range jobs {
		token := metaString(j.auth, "access_token")
		if token == "" {
			continue
		}
		if j.plan {
			var profile struct {
				Organization struct {
					Tier    *string `json:"rate_limit_tier"`
					Status  *string `json:"subscription_status"`
					Created *string `json:"subscription_created_at"`
				} `json:"organization"`
			}
			if status, _ := p.get(ctx, token, "/api/oauth/profile", &profile); status == http.StatusOK {
				o := profile.Organization
				plan := &PoolPlan{Tier: o.Tier, Status: o.Status, Since: isoMillis(o.Created), At: now.UnixMilli()}
				p.mu.Lock()
				p.account(j.account).plan = plan
				p.mu.Unlock()
				p.writeState(filepath.Join("plans", j.account+".json"), plan)
			}
		}
		if j.usage || j.resets {
			p.readUsage(ctx, j.account, token, now)
		}
	}
	p.spendReset(ctx, m, auths, now)
	p.writeAccounts(auths)
}

// readUsage reads an account's limits and held resets from Anthropic's usage endpoint.
func (p *claudePoolState) readUsage(ctx context.Context, account, token string, now time.Time) {
	var body struct {
		Cloud *struct {
			Limit     *float64 `json:"limit_dollars"`
			Used      *float64 `json:"used_dollars"`
			Remaining *float64 `json:"remaining_dollars"`
			Expires   *string  `json:"resets_at"`
		} `json:"iguana_necktie"`
		FiveHour *usageWindow `json:"five_hour"`
		SevenDay *usageWindow `json:"seven_day"`
		Cedar    *struct {
			Grants []map[string]any `json:"grants"`
		} `json:"cedar_ember"`
		Juniper *struct {
			Eligible  bool    `json:"eligible"`
			Available bool    `json:"available"`
			NextAt    *string `json:"next_available_at"`
		} `json:"juniper_tide"`
	}
	status, retry := p.get(ctx, token, "/api/oauth/usage?cedar_ember=1&at_wall=1&skip_spend=1", &body)
	p.mu.Lock()
	a := p.account(account)
	a.needsReauth = status == http.StatusUnauthorized
	switch {
	case status == http.StatusTooManyRequests:
		a.retryAt = now.Add(retry).UnixMilli()
	case status == http.StatusOK:
		a.cloud = nil
		if body.Cloud != nil {
			a.cloud = &PoolCredit{Limit: body.Cloud.Limit, Used: body.Cloud.Used, Remaining: body.Cloud.Remaining, ExpiresAt: isoMillis(body.Cloud.Expires), SampledAt: now.UnixMilli()}
		}
		var stale bool
		a.reading, stale = a.settled(PoolReading{FiveHour: body.FiveHour.window(), Week: body.SevenDay.window(), At: now.UnixMilli()}, now)
		p.logReadingLocked(account, a.reading)
		if !stale {
			a.resets = []PoolReset{}
			if body.Cedar != nil {
				for _, g := range body.Cedar.Grants {
					if left, _ := g["resets_left"].(float64); left > 0 {
						a.resets = append(a.resets, grantReset(g, int(left)))
					}
				}
			}
		}
		a.atLimit = nil
		if body.Juniper != nil && body.Juniper.Eligible {
			a.atLimit = map[string]any{"available": body.Juniper.Available, "nextAt": isoMillis(body.Juniper.NextAt)}
		}
		a.resetsAt = now.UnixMilli()
	}
	resets := map[string]any{"held": a.resets, "atLimit": a.atLimit, "at": now.UnixMilli()}
	p.mu.Unlock()
	if status == http.StatusOK {
		p.writeState(filepath.Join("resets", account+".json"), resets)
	}
}

func grantReset(g map[string]any, left int) PoolReset {
	id, _ := g["id"].(string)
	ends, _ := g["ends_at"].(string)
	paused, _ := g["paused"].(bool)
	usable, _ := g["usable_now"].(bool)
	needsLimit, given := g["use_requires_limit"].(bool)
	clears, _ := g["clears"].([]any)
	weekly := false
	for _, c := range clears {
		weekly = weekly || c == "seven_day"
	}
	return PoolReset{ID: id, Kind: resetKind(g), Left: left, ExpiresAt: isoMillis(&ends), Paused: paused, UsableNow: usable, NeedsLimit: needsLimit || !given, Weekly: weekly}
}

type usageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

func (w *usageWindow) window() *PoolWindow {
	if w == nil || w.Utilization == nil {
		return nil
	}
	out := &PoolWindow{Used: *w.Utilization}
	if at := isoMillis(w.ResetsAt); at != nil {
		out.ResetsAt = *at
	}
	return out
}

func resetKind(g map[string]any) string {
	if back, _ := g["gives_back_weekly"].(bool); back {
		return "fable"
	}
	if kind, _ := g["type"].(string); kind != "" {
		return kind
	}
	clears, _ := g["clears"].([]any)
	session, other := false, false
	for _, c := range clears {
		if c == "five_hour" {
			session = true
		} else {
			other = true
		}
	}
	switch {
	case session && !other:
		return "session"
	case other && !session:
		return "weekly"
	default:
		return "full"
	}
}

func isoMillis(s *string) *int64 {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return nil
	}
	ms := t.UnixMilli()
	return &ms
}

func (p *claudePoolState) get(ctx context.Context, token, path string, out any) (int, time.Duration) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicAPI+path, nil)
	if err != nil {
		return 0, 0
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "claude-cli/2.1.316 (external, cli)")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()
	retry := 15 * time.Minute
	if seconds, errAtoi := strconv.Atoi(resp.Header.Get("Retry-After")); errAtoi == nil && seconds > 0 {
		retry = time.Duration(seconds) * time.Second
	}
	if resp.StatusCode == http.StatusOK {
		if errDecode := json.NewDecoder(resp.Body).Decode(out); errDecode != nil {
			return 0, retry
		}
	}
	return resp.StatusCode, retry
}

func (p *claudePoolState) writeState(name string, value any) {
	p.mu.Lock()
	dir := p.cfg.StateDir
	p.mu.Unlock()
	if dir != "" {
		writeJSONFile(filepath.Join(dir, name), value)
	}
}

// writeAccounts keeps the account list current for tools that name accounts by ID.
func (p *claudePoolState) writeAccounts(auths []*Auth) {
	type entry struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		Tier        string `json:"tier"`
		NeedsReauth bool   `json:"needsReauth"`
	}
	p.mu.Lock()
	path := p.cfg.AccountsFile
	list := []entry{}
	for _, auth := range auths {
		if !isClaudeAuth(auth) || metaString(auth, "account_uuid") == "" {
			continue
		}
		a := p.account(poolAccountID(auth))
		tier := ""
		if a.plan != nil && a.plan.Tier != nil {
			tier = strings.ReplaceAll(strings.TrimPrefix(*a.plan.Tier, "default_claude_"), "_", " ")
		}
		list = append(list, entry{ID: poolAccountID(auth), Email: metaString(auth, "email"), Tier: tier, NeedsReauth: a.needsReauth || auth.Disabled})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Email < list[j].Email })
	data, _ := json.Marshal(list)
	changed := string(data) != p.listed && path != "" && len(list) > 0
	if changed {
		p.listed = string(data)
	}
	p.mu.Unlock()
	if changed {
		writeJSONFile(path, map[string]any{"accounts": list})
	}
}

// PoolSnapshot is what the dashboard shows: the policy and every Claude account in rank order.
type PoolSnapshot struct {
	SessionCutoff    float64   `json:"sessionCutoff"`
	WeeklyCutoff     float64   `json:"weeklyCutoff"`
	AgentsPerAccount int       `json:"agentsPerAccount"`
	Accounts         []PoolRow `json:"accounts"`
	Decision         string    `json:"decision"`
}

// PoolSnapshot ranks the manager's Claude accounts for the dashboard. A non-nil preview
// ranks them under proposed settings without applying them.
func (m *Manager) PoolSnapshot(preview *config.PoolConfig) PoolSnapshot {
	auths := m.List()
	claudePool.mu.Lock()
	defer claudePool.mu.Unlock()
	if preview != nil {
		saved := claudePool.cfg
		next := saved
		next.SessionCutoff, next.WeeklyCutoff, next.AgentsPerAccount = preview.SessionCutoff, preview.WeeklyCutoff, preview.AgentsPerAccount
		claudePool.cfg = normalizePoolConfig(next)
		defer func() { claudePool.cfg = saved }()
	}
	cfg := claudePool.cfg
	enabled := make([]*Auth, 0, len(auths))
	for _, a := range auths {
		if !a.Disabled {
			enabled = append(enabled, a)
		}
	}
	now := time.Now()
	rows := claudePool.rankLocked(enabled, now, 1)
	for _, plan := range claudePool.planResetsLocked(rows, now) {
		plan.row.ResetPlan = plan.plan
	}
	for _, a := range auths {
		if a.Disabled && isClaudeAuth(a) {
			rows = append(rows, claudePool.rankLocked([]*Auth{a}, now, 1)...)
		}
	}
	spent := claudePool.spentLocked()
	for i := range rows {
		rows[i].ResetsSpent = spent[rows[i].Account]
	}
	decision := "No Claude account can take a new session."
	if len(rows) > 0 && rows[0].Usable {
		decision = fmt.Sprintf("New sessions go to %s.", rows[0].Email)
	} else if len(rows) > 0 && rows[0].Open {
		decision = fmt.Sprintf("Every account is past a cutoff; new sessions go to %s, which has the most room left.", rows[0].Email)
	}
	return PoolSnapshot{SessionCutoff: cfg.SessionCutoff, WeeklyCutoff: cfg.WeeklyCutoff, AgentsPerAccount: cfg.AgentsPerAccount, Accounts: rows, Decision: decision}
}

// PoolHistory returns the usage-history lines newer than since (unix ms).
func PoolHistory(since int64) []json.RawMessage {
	claudePool.mu.Lock()
	dir := claudePool.cfg.StateDir
	claudePool.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(dir, "usage-history.ndjson"))
	if dir == "" || err != nil {
		return []json.RawMessage{}
	}
	out := []json.RawMessage{}
	for _, line := range strings.Split(string(data), "\n") {
		var row struct {
			At int64 `json:"at"`
		}
		if json.Unmarshal([]byte(line), &row) == nil && row.At >= since {
			out = append(out, json.RawMessage(line))
		}
	}
	return out
}
