package deck

import (
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

const (
	colors = 7
	// noAccount is the account of requests bound to no account, such as Codex or unpooled sessions.
	noAccount = "none"
)

var (
	groupings  = []string{"agent", "session", "project", "model", "harness", "device", "account"}
	measures   = []string{"tokens", "api", "plan", "fiveHour", "week"}
	dimensions = []string{"app", "harness", "model", "project", "device", "account"}
)

// Query is what the Usage page asks for.
type Query struct {
	Range   Range               `json:"range"`
	By      string              `json:"by"`
	Measure string              `json:"measure"`
	Where   map[string][]string `json:"where"`
	Search  string              `json:"search"`
	Limit   int                 `json:"limit"`
	// Only rows under this agent row key, so a row drills into its sessions.
	Within *string `json:"within"`
}

func pick[T ~string](value string, options []T, fallback T) T {
	for _, option := range options {
		if string(option) == value {
			return option
		}
	}
	return fallback
}

// ParseQuery reads a Usage query from the page's URL parameters.
func ParseQuery(params url.Values) Query {
	where := map[string][]string{}
	for _, dimension := range dimensions {
		var values []string
		for _, value := range strings.Split(params.Get("w."+dimension), ",") {
			if value != "" {
				values = append(values, value)
			}
		}
		if len(values) > 0 {
			sort.Strings(values)
			where[dimension] = values
		}
	}
	limit, err := strconv.Atoi(params.Get("limit"))
	if err != nil || limit <= 0 {
		limit = 50
	}
	q := Query{
		Range:   pick(params.Get("range"), Ranges, Week),
		By:      pick(params.Get("by"), groupings, "agent"),
		Measure: pick(params.Get("measure"), measures, "api"),
		Where:   where,
		Search:  strings.TrimSpace(params.Get("q")),
		Limit:   limit,
	}
	if within := params.Get("within"); within != "" {
		q.Within = &within
	}
	return q
}

// Points are percentage points of account limits, summed over accounts and
// windows. Null: nothing to estimate from.
type Points struct {
	FiveHour *float64 `json:"fiveHour"`
	Week     *float64 `json:"week"`
}

// AccountPoints are one account's points within a row.
type AccountPoints struct {
	Account string `json:"account"`
	Points
}

// Totals sum the rows in view.
type Totals struct {
	Amount
	Sessions     int     `json:"sessions"`
	CacheHitRate float64 `json:"cacheHitRate"`
	Points       Points  `json:"points"`
}

// Row is one ranked group.
type Row struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
	Who    *Who   `json:"who"`
	// Categorical slot 0 to 6 for the top rows by API value in the unfiltered range, 7 for the rest.
	Color        int             `json:"color"`
	Amount       Amount          `json:"amount"`
	Points       Points          `json:"points"`
	ByAccount    []AccountPoints `json:"byAccount"`
	Sessions     int             `json:"sessions"`
	LastActiveAt float64         `json:"lastActiveAt"`
	// The chosen measure per bucket of the view's buckets.
	Series     []float64 `json:"series"`
	Live       *string   `json:"live"`
	sessionIDs map[string]bool
}

// Bucket is one chart bucket's start and size.
type Bucket struct {
	At   int64  `json:"at"`
	Size string `json:"size"`
}

// Series is one line of the chart.
type Series struct {
	Key    string    `json:"key"`
	Label  string    `json:"label"`
	Color  int       `json:"color"`
	Values []float64 `json:"values"`
}

// Unassigned is limit points no bound request explains.
type Unassigned struct {
	Account string  `json:"account"`
	Kind    string  `json:"kind"`
	Points  float64 `json:"points"`
}

// Coverage says how much of the view is named and since when limits are known.
type Coverage struct {
	NamedAPIUSD float64      `json:"namedApiUsd"`
	TotalAPIUSD float64      `json:"totalApiUsd"`
	LimitsSince *float64     `json:"limitsSince"`
	Unassigned  []Unassigned `json:"unassigned"`
}

// Facet is one value of a dimension and how many sessions have it.
type Facet struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Sources says where the view's data came from and when.
type Sources struct {
	TrackerAt    *int64         `json:"trackerAt"`
	TrackerError *string        `json:"trackerError"`
	Devices      []usage.Device `json:"devices"`
}

// UsageView is the Usage page: totals, the ranked rows, the top-seven series
// and coverage for one query.
type UsageView struct {
	Query    Query              `json:"query"`
	Period   Period             `json:"period"`
	Totals   Totals             `json:"totals"`
	Previous *Totals            `json:"previous"`
	Buckets  []Bucket           `json:"buckets"`
	Series   []Series           `json:"series"`
	Rows     []Row              `json:"rows"`
	RowCount int                `json:"rowCount"`
	Coverage Coverage           `json:"coverage"`
	Facets   map[string][]Facet `json:"facets"`
	Sources  Sources            `json:"sources"`
}

// Labels are what people call accounts and devices.
type Labels struct {
	Accounts map[string]string
	Devices  map[string]string
}

// part is one session's share of one model on one account. Parts of a session
// sum back to the session exactly.
type part struct {
	session        *Session
	who            Who
	agent          string
	model, account string
	amount         Amount
	fiveHour, week float64
	// Multipliers from the session's series columns [tokens, api, plan, fiveHour, week] to this part's.
	scale [5]float64
}

func ratio(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return part / whole
}

func fields(a *Amount) [10]*float64 {
	return [10]*float64{&a.Tokens, &a.Input, &a.Output, &a.CacheRead, &a.CacheWrite, &a.Reasoning, &a.Requests, &a.APIUSD, &a.PlanUSD, &a.UnpricedTokens}
}

func (a *Amount) add(b Amount) {
	into, from := fields(a), fields(&b)
	for i := range into {
		*into[i] += *from[i]
	}
}

// overlapOf is, field by field, the part of whole that is both a and b,
// assuming they overlap in proportion.
func overlapOf(a, b, whole Amount) Amount {
	var out Amount
	o, x, y, w := fields(&out), fields(&a), fields(&b), fields(&whole)
	for i := range o {
		if *w[i] != 0 {
			*o[i] = *x[i] * *y[i] / *w[i]
		}
	}
	return out
}

// partsOf splits a session into model × account parts. The amount per model and
// per account are exact; a part is their overlap. Limit points belong to their
// account and split across models by API $.
func partsOf(s *Session, names *Names) []part {
	who := whoOf(names, sessionRef{sessionID: s.SessionID, title: s.Title, project: s.Project, repository: s.Repository})
	agent := rowOf(who, s.Key)
	amount := s.Amount
	models := s.Models
	if len(models.keys) == 0 {
		models = Amounts{}
		*models.at("unknown") = amount
	}
	amounts := s.Accounts
	if len(amounts.keys) == 0 {
		amounts = Amounts{}
		*amounts.at(noAccount) = amount
	}
	var kinds [2]float64
	charged := map[string]*[2]float64{}
	accounts := append([]string(nil), amounts.keys...)
	for _, limit := range s.Limits {
		entry := charged[limit.Account]
		if entry == nil {
			entry = &[2]float64{}
			charged[limit.Account] = entry
			if amounts.Get(limit.Account) == nil {
				accounts = append(accounts, limit.Account)
			}
		}
		entry[kindIndex(limit.Kind)] += limit.Points
		kinds[kindIndex(limit.Kind)] += limit.Points
	}
	parts := make([]part, 0, len(models.keys)*len(accounts))
	for _, model := range models.keys {
		modelAmount := *models.Get(model)
		api := ratio(modelAmount.Tokens, amount.Tokens)
		if amount.APIUSD != 0 {
			api = ratio(modelAmount.APIUSD, amount.APIUSD)
		}
		for _, account := range accounts {
			accountAmount := Amount{}
			if found := amounts.Get(account); found != nil {
				accountAmount = *found
			}
			points := [2]float64{}
			if found := charged[account]; found != nil {
				points = *found
			}
			parts = append(parts, part{
				session: s, who: who, agent: agent, model: model, account: account,
				amount:   overlapOf(modelAmount, accountAmount, amount),
				fiveHour: points[0] * api,
				week:     points[1] * api,
				scale: [5]float64{
					ratio(modelAmount.Tokens, amount.Tokens) * ratio(accountAmount.Tokens, amount.Tokens),
					ratio(modelAmount.APIUSD, amount.APIUSD) * ratio(accountAmount.APIUSD, amount.APIUSD),
					ratio(modelAmount.PlanUSD, amount.PlanUSD) * ratio(accountAmount.PlanUSD, amount.PlanUSD),
					ratio(points[0], kinds[0]) * api,
					ratio(points[1], kinds[1]) * api,
				},
			})
		}
	}
	return parts
}

func dimensionOf(dimension string, p *part) string {
	switch dimension {
	case "app":
		return appOf(p.who)
	case "harness":
		return p.session.Harness
	case "model":
		return p.model
	case "project":
		return p.session.Project
	case "device":
		return p.session.DeviceID
	}
	return p.account
}

// nameOf is what people call a dimension's value: account and device labels,
// "No account" for unbound requests.
func nameOf(dimension, id string, labels Labels) string {
	switch dimension {
	case "account":
		if id == noAccount {
			return "No account"
		}
		if label, ok := labels.Accounts[id]; ok {
			return label
		}
	case "device":
		if label, ok := labels.Devices[id]; ok {
			return label
		}
	}
	return id
}

var groupDimension = map[string]string{"model": "model", "harness": "harness", "device": "device", "account": "account"}

func groupKey(by string, p *part) string {
	switch by {
	case "agent":
		return p.agent
	case "session":
		return "s:" + p.session.Key
	case "project":
		return "p:" + p.session.Project
	case "model":
		return "m:" + p.model
	case "harness":
		return "h:" + p.session.Harness
	case "device":
		return "d:" + p.session.DeviceID
	}
	return "a:" + p.account
}

func describe(by string, p *part, labels Labels) (string, string, *Who) {
	switch by {
	case "agent":
		label, detail := labelOf(p.who)
		agent := agentOf(p.who)
		return label, detail, &agent
	case "session":
		label, detail := labelOf(p.who)
		if p.session.Title != nil {
			label = *p.session.Title
		}
		who := p.who
		if who.Kind == "action" {
			return label, who.Repo + " · " + detail, &who
		}
		id := p.session.SessionID
		if len(id) > 8 {
			id = id[:8]
		}
		return label, strings.Join([]string{basename(p.session.Project), p.session.Harness, id}, " · "), &who
	case "project":
		project := p.session.Project
		if name := basename(project); name != "" {
			return name, project, nil
		}
		return project, project, nil
	}
	dimension := groupDimension[by]
	return nameOf(dimension, dimensionOf(dimension, p), labels), "", nil
}

var seriesColumn = map[string]int{"tokens": 1, "api": 2, "plan": 3, "fiveHour": 4, "week": 5}

type accountShare struct{ apiUSD, fiveHour, week float64 }

type group struct {
	key, label, detail string
	who                *Who
	amount             Amount
	fiveHour, week     float64
	accounts           []string
	byAccount          map[string]*accountShare
	sessions           map[string]bool
	sessionIDs         map[string]bool
	lastActiveAt       float64
	series             []float64
}

func groupParts(parts []part, by, measure string, buckets int, labels Labels) []*group {
	index := map[string]*group{}
	var groups []*group
	column := seriesColumn[measure]
	for i := range parts {
		p := &parts[i]
		key := groupKey(by, p)
		g := index[key]
		if g == nil {
			label, detail, who := describe(by, p, labels)
			g = &group{key: key, label: label, detail: detail, who: who, byAccount: map[string]*accountShare{},
				sessions: map[string]bool{}, sessionIDs: map[string]bool{}, series: make([]float64, buckets)}
			index[key] = g
			groups = append(groups, g)
		}
		g.amount.add(p.amount)
		g.fiveHour += p.fiveHour
		g.week += p.week
		g.sessions[p.session.Key] = true
		g.sessionIDs[p.session.SessionID] = true
		g.lastActiveAt = max(g.lastActiveAt, p.session.LastAt)
		if p.amount.Tokens != 0 || p.fiveHour != 0 || p.week != 0 {
			share := g.byAccount[p.account]
			if share == nil {
				share = &accountShare{}
				g.byAccount[p.account] = share
				g.accounts = append(g.accounts, p.account)
			}
			share.apiUSD += p.amount.APIUSD
			share.fiveHour += p.fiveHour
			share.week += p.week
		}
		if buckets > 0 {
			for _, row := range p.session.Series {
				g.series[int(row[0])] += row[column] * p.scale[column-1]
			}
		}
	}
	return groups
}

func valueOf(g *group, measure string) float64 {
	switch measure {
	case "tokens":
		return g.amount.Tokens
	case "api":
		return g.amount.APIUSD
	case "plan":
		return g.amount.PlanUSD
	case "fiveHour":
		return g.fiveHour
	}
	return g.week
}

func usageView(tracked *SessionUsage, all []part, labels Labels, q Query) *UsageView {
	buckets := len(tracked.Buckets)
	var parts []part
	for i := range all {
		p := &all[i]
		keep := q.Within == nil || p.agent == *q.Within
		for dimension, values := range q.Where {
			keep = keep && slices.Contains(values, dimensionOf(dimension, p))
		}
		if keep {
			parts = append(parts, *p)
		}
	}

	byAPI := groupParts(all, q.By, "api", 0, labels)
	sort.SliceStable(byAPI, func(i, j int) bool { return byAPI[i].amount.APIUSD > byAPI[j].amount.APIUSD })
	color := map[string]int{}
	for i, g := range byAPI[:min(colors, len(byAPI))] {
		color[g.key] = i
	}

	groups := groupParts(parts, q.By, q.Measure, buckets, labels)
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := valueOf(groups[i], q.Measure), valueOf(groups[j], q.Measure)
		if a != b {
			return a > b
		}
		return groups[i].amount.APIUSD > groups[j].amount.APIUSD
	})

	shown := groups
	if needle := strings.ToLower(q.Search); needle != "" {
		shown = nil
		for _, g := range groups {
			if strings.Contains(strings.ToLower(g.label+"\n"+g.detail), needle) {
				shown = append(shown, g)
			}
		}
	}
	limits := len(tracked.Windows) > 0
	points := func(five, week float64) Points {
		if !limits {
			return Points{}
		}
		return Points{FiveHour: &five, Week: &week}
	}

	var total Amount
	totalSessions := map[string]bool{}
	var totalFive, totalWeek, named float64
	for i := range parts {
		total.add(parts[i].amount)
		totalSessions[parts[i].session.Key] = true
		totalFive += parts[i].fiveHour
		totalWeek += parts[i].week
		if parts[i].who.Kind != "project" {
			named += parts[i].amount.APIUSD
		}
	}
	hitRate := func(a Amount) float64 { return ratio(a.CacheRead, a.Input+a.CacheRead+a.CacheWrite) }
	view := &UsageView{
		Query:  q,
		Period: tracked.Period,
		Totals: Totals{Amount: total, Sessions: len(totalSessions), CacheHitRate: hitRate(total), Points: points(totalFive, totalWeek)},
	}
	if len(q.Where) == 0 && q.Within == nil {
		view.Previous = &Totals{Amount: tracked.Previous.Amount, Sessions: tracked.Previous.Sessions, CacheHitRate: hitRate(tracked.Previous.Amount)}
	}

	top := groups[:min(colors, len(groups))]
	taken := map[int]bool{}
	for _, g := range top {
		if slot, ok := color[g.key]; ok {
			taken[slot] = true
		}
	}
	var free []int
	for slot := range colors {
		if !taken[slot] {
			free = append(free, slot)
		}
	}
	for _, g := range top {
		if _, ok := color[g.key]; !ok {
			color[g.key] = colors
			if len(free) > 0 {
				color[g.key], free = free[0], free[1:]
			}
		}
	}
	colorOf := func(key string) int {
		if slot, ok := color[key]; ok {
			return slot
		}
		return colors
	}
	view.Series = make([]Series, 0, len(top)+1)
	for _, g := range top {
		view.Series = append(view.Series, Series{Key: g.key, Label: g.label, Color: colorOf(g.key), Values: g.series})
	}
	if rest := groups[len(top):]; len(rest) > 0 {
		values := make([]float64, buckets)
		for _, g := range rest {
			for i, v := range g.series {
				values[i] += v
			}
		}
		view.Series = append(view.Series, Series{Key: "rest", Label: "Everything else", Color: colors, Values: values})
	}

	view.Buckets = make([]Bucket, len(tracked.Buckets))
	for i, at := range tracked.Buckets {
		view.Buckets[i] = Bucket{At: at, Size: tracked.Bucket}
	}
	view.Rows = make([]Row, 0, min(q.Limit, len(shown)))
	for _, g := range shown[:min(q.Limit, len(shown))] {
		accounts := append([]string(nil), g.accounts...)
		sort.SliceStable(accounts, func(i, j int) bool { return g.byAccount[accounts[i]].apiUSD > g.byAccount[accounts[j]].apiUSD })
		byAccount := make([]AccountPoints, len(accounts))
		for i, account := range accounts {
			share := g.byAccount[account]
			byAccount[i] = AccountPoints{Account: account, Points: points(share.fiveHour, share.week)}
		}
		view.Rows = append(view.Rows, Row{Key: g.key, Label: g.label, Detail: g.detail, Who: g.who, Color: colorOf(g.key),
			Amount: g.amount, Points: points(g.fiveHour, g.week), ByAccount: byAccount, Sessions: len(g.sessions),
			LastActiveAt: g.lastActiveAt, Series: g.series, sessionIDs: g.sessionIDs})
	}
	view.RowCount = len(shown)

	var unassigned []Unassigned
	unassignedAt := map[[2]string]int{}
	var since *float64
	for _, w := range tracked.Windows {
		key := [2]string{w.Account, w.Kind}
		i, ok := unassignedAt[key]
		if !ok {
			i = len(unassigned)
			unassignedAt[key] = i
			unassigned = append(unassigned, Unassigned{Account: w.Account, Kind: w.Kind})
		}
		unassigned[i].Points += w.Unassigned
		if since == nil || w.Since < *since {
			since = ptr(w.Since)
		}
	}
	positive := []Unassigned{}
	for _, u := range unassigned {
		if u.Points > 0 {
			positive = append(positive, u)
		}
	}
	view.Coverage = Coverage{NamedAPIUSD: named, TotalAPIUSD: total.APIUSD, LimitsSince: since, Unassigned: positive}

	view.Facets = map[string][]Facet{}
	for _, dimension := range dimensions {
		var facets []Facet
		at := map[string]int{}
		sessions := []map[string]bool{}
		for i := range all {
			value := dimensionOf(dimension, &all[i])
			j, ok := at[value]
			if !ok {
				j = len(facets)
				at[value] = j
				facets = append(facets, Facet{ID: value, Label: nameOf(dimension, value, labels)})
				sessions = append(sessions, map[string]bool{})
			}
			sessions[j][all[i].session.Key] = true
		}
		for j := range facets {
			facets[j].Count = len(sessions[j])
		}
		sort.SliceStable(facets, func(i, j int) bool { return facets[i].Count > facets[j].Count })
		view.Facets[dimension] = append([]Facet{}, facets[:min(100, len(facets))]...)
	}

	view.Sources.Devices = tracked.Devices
	return view
}

// liveStrength is the browser's order too: a row with a blocked session reads
// blocked even while another works.
var liveStrength = map[string]int{"blocked": 3, "working": 2, "idle": 1}

// withLive lights each row from the live sessions; cheap, so it runs on every
// request over a cached view.
func withLive(view *UsageView, live []*LiveSession, trackerAt int64) UsageView {
	out := *view
	out.Rows = make([]Row, len(view.Rows))
	for i, row := range view.Rows {
		var best *string
		for _, s := range live {
			if !row.sessionIDs[s.ID] && s.Row != row.Key {
				continue
			}
			if best == nil || liveStrength[s.State] > liveStrength[*best] {
				best = ptr(s.State)
			}
		}
		row.Live = best
		out.Rows[i] = row
	}
	out.Sources.TrackerAt = &trackerAt
	return out
}
