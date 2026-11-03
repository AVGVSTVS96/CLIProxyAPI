package deck

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

// Amount is tokens, requests and dollars summed over requests.
type Amount struct {
	Tokens         float64 `json:"tokens"`
	Input          float64 `json:"input"`
	Output         float64 `json:"output"`
	CacheRead      float64 `json:"cacheRead"`
	CacheWrite     float64 `json:"cacheWrite"`
	Reasoning      float64 `json:"reasoning"`
	Requests       float64 `json:"requests"`
	APIUSD         float64 `json:"apiUsd"`
	PlanUSD        float64 `json:"planUsd"`
	UnpricedTokens float64 `json:"unpricedTokens"`
}

func (a *Amount) addEvent(event *prepared, planUSD float64) {
	a.Tokens += event.tokens
	a.Input += float64(event.InputTokens)
	a.Output += float64(event.OutputTokens)
	a.CacheRead += float64(event.CacheReadTokens)
	a.CacheWrite += float64(event.CacheWriteTokens)
	a.Reasoning += float64(event.ReasoningTokens)
	if event.Requests != nil {
		a.Requests += float64(*event.Requests)
	} else {
		a.Requests++
	}
	a.APIUSD += event.CostUSD
	a.PlanUSD += planUSD
	if !event.CostKnown {
		a.UnpricedTokens += event.tokens
	}
}

// Amounts is a record of amounts that keeps the order its keys were first added in.
type Amounts struct {
	keys   []string
	values map[string]*Amount
}

func (r *Amounts) at(key string) *Amount {
	if value, ok := r.values[key]; ok {
		return value
	}
	if r.values == nil {
		r.values = map[string]*Amount{}
	}
	value := &Amount{}
	r.keys = append(r.keys, key)
	r.values[key] = value
	return value
}

// Keys lists the record's keys in insertion order.
func (r *Amounts) Keys() []string { return r.keys }

// Get returns a key's amount, nil when absent.
func (r *Amounts) Get(key string) *Amount { return r.values[key] }

// MarshalJSON writes the record as an object in insertion order.
func (r Amounts) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for i, key := range r.keys {
		if i > 0 {
			out.WriteByte(',')
		}
		name, _ := json.Marshal(key)
		value, _ := json.Marshal(r.values[key])
		out.Write(name)
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// SessionLimit is the limit points one session was charged on one account.
type SessionLimit struct {
	Account string  `json:"account"`
	Kind    string  `json:"kind"`
	Points  float64 `json:"points"`
}

// Session is one session's usage in a range.
type Session struct {
	Key        string  `json:"key"`
	DeviceID   string  `json:"deviceId"`
	Harness    string  `json:"harness"`
	SessionID  string  `json:"sessionId"`
	Project    string  `json:"project"`
	Repository *string `json:"repository"`
	Cwd        string  `json:"cwd"`
	Title      *string `json:"title"`
	// Entrypoint is how its Claude Code process was started (cli, sdk-cli, sdk-ts, t3-code...); "" when unknown.
	Entrypoint string  `json:"entrypoint"`
	FirstAt    float64 `json:"firstAt"`
	LastAt     float64 `json:"lastAt"`
	Amount     Amount  `json:"amount"`
	Models     Amounts `json:"models"`
	// By the account the session was bound to at each request's time; "none" when unbound.
	Accounts Amounts `json:"accounts"`
	// Sparse: [bucket index, tokens, API $, plan $, five-hour points, week points].
	Series [][6]float64    `json:"series"`
	Limits []*SessionLimit `json:"limits"`
}

// WindowUsage is the points of one account's limit window that fell in the
// range: assigned to the range's requests, or unassigned when no bound request
// explains the rise.
type WindowUsage struct {
	Account    string       `json:"account"`
	Kind       string       `json:"kind"`
	ResetsAt   float64      `json:"resetsAt"`
	Since      float64      `json:"since"`
	Sampled    float64      `json:"sampled"`
	Assigned   float64      `json:"assigned"`
	Unassigned float64      `json:"unassigned"`
	Rises      [][2]float64 `json:"rises"`
}

// Period is a range's bounds in unix ms and the zone its days are counted in.
type Period struct {
	From     int64  `json:"from"`
	To       int64  `json:"to"`
	Timezone string `json:"timezone"`
}

// Previous is the period before the range, summed.
type Previous struct {
	Amount
	Sessions int `json:"sessions"`
}

// SessionUsage is token-tracker's GetSessionUsage answer for one range.
type SessionUsage struct {
	GeneratedAt int64          `json:"generatedAt"`
	Period      Period         `json:"period"`
	Bucket      string         `json:"bucket"`
	Buckets     []int64        `json:"buckets"`
	Sessions    []*Session     `json:"sessions"`
	Windows     []WindowUsage  `json:"windows"`
	Previous    Previous       `json:"previous"`
	Devices     []usage.Device `json:"devices"`
	// SDKFactor is the five-hour weight Agent SDK requests were charged at.
	SDKFactor float64 `json:"sdkFactor"`
}

// prepared is an event with what every view derives from it.
type prepared struct {
	*usage.Event
	millis   float64
	provider string
	tokens   float64
}

func prepare(events []usage.Event) []prepared {
	out := make([]prepared, len(events))
	providers := map[string]string{}
	for i := range events {
		event := &events[i]
		name, ok := providers[event.Model]
		if !ok {
			name = provider(event.Model)
			providers[event.Model] = name
		}
		out[i] = prepared{Event: event, millis: parseMillis(event.Timestamp), provider: name,
			tokens: float64(event.InputTokens + event.OutputTokens + event.CacheReadTokens + event.CacheWriteTokens)}
	}
	return out
}

func deviceOf(event *prepared) string {
	if event.DeviceID == "" {
		return "local"
	}
	return event.DeviceID
}

type limitInputs struct {
	readings  []LimitReading
	history   []Binding
	sdkFactor float64
}

type sessionRow struct {
	*Session
	series   map[int]*[6]float64
	limits   map[string]*SessionLimit
	titledAt float64
}

// sessionUsage lists every session active in the range, with the same records,
// deduplication and prices as the totals, so the sessions' sums equal them.
func sessionUsage(events []prepared, r Range, zone *time.Location, now time.Time, plans *planPricing, limits *limitInputs, devices []usage.Device) *SessionUsage {
	frame := usageTimeframe(r, zone, now)
	nowMillis := float64(now.UnixMilli())
	startMillis, previousMillis, endMillis := float64(frame.start.UnixMilli()), float64(frame.previousStart.UnixMilli()), float64(frame.end.UnixMilli())
	size, starts := bucketStarts(r, frame)
	bucketOf := func(millis float64) int {
		return max(0, sort.Search(len(starts), func(i int) bool { return float64(starts[i]) > millis })-1)
	}

	accountOf := func(*prepared) string { return "" }
	var windows []*limitWindow
	var charges []*limitCharge
	if limits != nil {
		accountOf = accountLookup(limits.history)
		windows, charges = attributeLimits(limits.readings, accountOf, events, limits.sdkFactor)
	}
	assignedInRange := map[*limitWindow]float64{}
	previous := Amount{}
	previousSessions := map[string]bool{}
	sessions := map[string]*sessionRow{}
	var order []*sessionRow
	for i := range events {
		event := &events[i]
		millis := event.millis
		if math.IsNaN(millis) || millis > nowMillis || millis < previousMillis || millis >= endMillis {
			continue
		}
		device := deviceOf(event)
		key := device + ":" + event.Harness + ":" + event.SessionID
		planUSD := event.CostUSD
		if plans != nil {
			planUSD = plans.price(event)
		}
		if millis < startMillis {
			previous.addEvent(event, planUSD)
			previousSessions[key] = true
			continue
		}
		row := sessions[key]
		if row == nil {
			project := event.Project
			if event.Repository != nil {
				project = *event.Repository
			}
			row = &sessionRow{Session: &Session{
				Key: key, DeviceID: device, Harness: event.Harness, SessionID: event.SessionID,
				Project: project, Repository: event.Repository, Cwd: event.Project, FirstAt: millis, LastAt: millis,
			}, series: map[int]*[6]float64{}, limits: map[string]*SessionLimit{}, titledAt: math.Inf(-1)}
			sessions[key] = row
			order = append(order, row)
		}
		row.FirstAt = math.Min(row.FirstAt, millis)
		row.LastAt = math.Max(row.LastAt, millis)
		if title := strings.TrimSpace(event.SessionTitle); title != "" && millis >= row.titledAt {
			row.Title = &title
			row.titledAt = millis
		}
		if event.Entrypoint != "" {
			row.Entrypoint = event.Entrypoint
		}
		row.Amount.addEvent(event, planUSD)
		row.Models.at(event.Model).addEvent(event, planUSD)
		account := accountOf(event)
		if account == "" {
			account = noAccount
		}
		row.Accounts.at(account).addEvent(event, planUSD)
		bucket := bucketOf(millis)
		point := row.series[bucket]
		if point == nil {
			point = &[6]float64{float64(bucket)}
			row.series[bucket] = point
		}
		point[1] += event.tokens
		point[2] += event.CostUSD
		point[3] += planUSD
		charge := charges[i:min(i+1, len(charges))]
		if len(charge) == 0 || charge[0] == nil {
			continue
		}
		point[4] += charge[0].points[0]
		point[5] += charge[0].points[1]
		for k, kind := range limitKinds {
			window := charge[0].window[k]
			if window == nil {
				continue
			}
			assignedInRange[window] += charge[0].points[k]
			limitKey := charge[0].account + ":" + kind
			limit := row.limits[limitKey]
			if limit == nil {
				limit = &SessionLimit{Account: charge[0].account, Kind: kind}
				row.limits[limitKey] = limit
				row.Limits = append(row.Limits, limit)
			}
			limit.Points += charge[0].points[k]
		}
	}

	rangeEnd := math.Min(endMillis, nowMillis)
	usages := []WindowUsage{}
	for _, window := range windows {
		if window.until < startMillis || window.since > rangeEnd {
			continue
		}
		assigned := assignedInRange[window]
		rises := [][2]float64{}
		unassigned := 0.0
		for _, r := range window.unassigned {
			if r.at >= startMillis && r.at <= rangeEnd {
				rises = append(rises, [2]float64{r.at, r.points})
			}
		}
		for _, r := range rises {
			unassigned += r[1]
		}
		usages = append(usages, WindowUsage{Account: window.account, Kind: window.kind, ResetsAt: window.resetsAt, Since: window.since,
			Sampled: assigned + unassigned, Assigned: assigned, Unassigned: unassigned, Rises: rises})
	}

	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.Amount.APIUSD != b.Amount.APIUSD {
			return a.Amount.APIUSD > b.Amount.APIUSD
		}
		return localeCompare(a.Key, b.Key) < 0
	})
	out := make([]*Session, len(order))
	for i, row := range order {
		row.Series = make([][6]float64, 0, len(row.series))
		for _, point := range row.series {
			row.Series = append(row.Series, *point)
		}
		sort.Slice(row.Series, func(a, b int) bool { return row.Series[a][0] < row.Series[b][0] })
		if row.Limits == nil {
			row.Limits = []*SessionLimit{}
		}
		out[i] = row.Session
	}
	return &SessionUsage{
		GeneratedAt: now.UnixMilli(),
		Period:      Period{From: frame.start.UnixMilli(), To: frame.end.UnixMilli(), Timezone: zone.String()},
		Bucket:      size,
		Buckets:     starts,
		Sessions:    out,
		Windows:     usages,
		Previous:    Previous{Amount: previous, Sessions: len(previousSessions)},
		Devices:     devices,
		SDKFactor:   sdkFactorOf(limits),
	}
}

func sdkFactorOf(limits *limitInputs) float64 {
	if limits == nil || limits.sdkFactor < 1 {
		return 1
	}
	return limits.sdkFactor
}
