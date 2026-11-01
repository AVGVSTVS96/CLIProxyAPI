package deck

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type fakeStore struct {
	events  []usage.Event
	devices []usage.Device
}

func (s *fakeStore) Events() []usage.Event   { return s.events }
func (s *fakeStore) Devices() []usage.Device { return s.devices }
func (s *fakeStore) Revision() uint64        { return 1 }
func (s *fakeStore) Subscribe(func()) func() { return func() {} }

func loadFixture(t testing.TB, dir string) *fakeStore {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	store := &fakeStore{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<26)
	for scanner.Scan() {
		var event usage.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		store.events = append(store.events, event)
	}
	if !readJSON(filepath.Join(dir, "devices.json"), &store.devices) {
		t.Fatal("devices.json")
	}
	return store
}

func writeOut(t testing.TB, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestParity writes this package's answers for the shared fixture next to the
// reference answers token-tracker's and deck's own code wrote (ref.ts), for
// compare.py. DECK_PARITY names the folder holding events.ndjson, devices.json
// and deck/{now,state,out}.
func TestParity(t *testing.T) {
	dir := os.Getenv("DECK_PARITY")
	if dir == "" {
		t.Skip("DECK_PARITY is unset")
	}
	nowText, _ := os.ReadFile(filepath.Join(dir, "deck", "now"))
	ms, err := strconv.ParseInt(string(nowText[:len(nowText)-1]), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(ms)
	state := filepath.Join(dir, "deck", "state")
	out := filepath.Join(dir, "deck", "out")
	zone, _ := time.LoadLocation("America/Los_Angeles")

	started := time.Now()
	store := loadFixture(t, dir)
	t.Logf("load %d events %v", len(store.events), time.Since(started))

	var configured struct {
		Plans []struct {
			Name       string   `json:"name"`
			Providers  []string `json:"providers"`
			MonthlyUSD float64  `json:"monthlyUSD"`
			Since      string   `json:"since"`
		} `json:"plans"`
	}
	readJSON(filepath.Join(state, "plans-config.json"), &configured)
	var plansConfig []config.UsagePlan
	for _, p := range configured.Plans {
		plansConfig = append(plansConfig, config.UsagePlan{Name: p.Name, Providers: p.Providers, MonthlyUSD: p.MonthlyUSD, Since: p.Since})
	}

	started = time.Now()
	events := prepare(store.events)
	t.Logf("prepare %v", time.Since(started))
	started = time.Now()
	plans := newPlanPricing(readPlans(filepath.Join(state, "accounts.json"), state, plansConfig), events, zone, now)
	t.Logf("plan pricing %v", time.Since(started))
	readings, _ := (&appendLog[LimitReading]{path: filepath.Join(state, "usage-history.ndjson"), parse: parseReading}).read()
	bindings, _ := (&appendLog[Binding]{path: filepath.Join(state, "attribution.ndjson"), parse: parseBinding}).read()
	limits := &limitInputs{readings: sortedReadings(readings), history: bindingHistory(bindings)}

	tracked := map[Range]*SessionUsage{}
	for _, r := range Ranges {
		started = time.Now()
		tracked[r] = sessionUsage(events, r, zone, now, plans, limits, store.devices)
		t.Logf("%s %v, %d sessions", r, time.Since(started), len(tracked[r].Sessions))
		writeOut(t, filepath.Join(out, "go-sessions-"+string(r)+".json"), tracked[r])
	}

	past, err := readPast(context.Background(), "/home/bassim/hex")
	if err != nil {
		t.Fatal(err)
	}
	names, err := loadNames(context.Background(), "/home/bassim/hex", past)
	if err != nil {
		t.Fatal(err)
	}
	labels := Labels{Accounts: map[string]string{}, Devices: map[string]string{}}
	var accounts struct {
		Accounts []struct{ ID, Email string } `json:"accounts"`
	}
	readJSON(filepath.Join(state, "accounts.json"), &accounts)
	for _, a := range accounts.Accounts {
		labels.Accounts[a.ID] = a.Email
	}
	for _, d := range tracked[Week].Devices {
		labels.Devices[d.ID] = d.Label
	}

	var queries []string
	if !readJSON(filepath.Join(out, "queries.json"), &queries) {
		t.Fatal("queries.json")
	}
	parts := map[Range][]part{}
	for r, value := range tracked {
		for _, s := range value.Sessions {
			parts[r] = append(parts[r], partsOf(s, names)...)
		}
	}
	type answer struct {
		Query string    `json:"query"`
		View  UsageView `json:"view"`
	}
	var answers []answer
	started = time.Now()
	for _, query := range queries {
		params, _ := url.ParseQuery(query)
		q := ParseQuery(params)
		answers = append(answers, answer{query, withLive(usageView(tracked[q.Range], parts[q.Range], labels, q), nil)})
	}
	t.Logf("%d views %v", len(queries), time.Since(started))
	writeOut(t, filepath.Join(out, "go-views.json"), answers)
}

// TestLive answers each range of token-tracker's live answers in deck/live with
// that answer's own clock, for comparing against the live service.
func TestLive(t *testing.T) {
	dir := os.Getenv("DECK_PARITY")
	if dir == "" {
		t.Skip("DECK_PARITY is unset")
	}
	state := filepath.Join(dir, "deck", "state")
	zone, _ := time.LoadLocation("America/Los_Angeles")
	store := loadFixture(t, dir)
	events := prepare(store.events)
	readings, _ := (&appendLog[LimitReading]{path: filepath.Join(state, "usage-history.ndjson"), parse: parseReading}).read()
	bindings, _ := (&appendLog[Binding]{path: filepath.Join(state, "attribution.ndjson"), parse: parseBinding}).read()
	limits := &limitInputs{readings: sortedReadings(readings), history: bindingHistory(bindings)}
	plansConfig := []config.UsagePlan{{Name: "ChatGPT Pro", Providers: []string{"openai"}, MonthlyUSD: 200, Since: "2026-04-24"}}
	for _, r := range Ranges {
		var live struct {
			GeneratedAt int64 `json:"generatedAt"`
		}
		if !readJSON(filepath.Join(dir, "deck", "live", "tt-"+string(r)+".json"), &live) {
			t.Fatal(r)
		}
		now := time.UnixMilli(live.GeneratedAt)
		plans := newPlanPricing(readPlans(filepath.Join(state, "accounts.json"), state, plansConfig), events, zone, now)
		writeOut(t, filepath.Join(dir, "deck", "live", "go-"+string(r)+".json"), sessionUsage(events, r, zone, now, plans, limits, store.devices))
	}
}

// UnmarshalJSON reads a record keeping its keys' order, as the views depend on it.
func (r *Amounts) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if _, err := decoder.Token(); err != nil {
		return err
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		if err := decoder.Decode(r.at(key.(string))); err != nil {
			return err
		}
	}
	return nil
}

// TestDeckLive builds the views over token-tracker's live answers in deck/live,
// for comparing against deck's live /api/usage answers to the same queries.
func TestDeckLive(t *testing.T) {
	dir := os.Getenv("DECK_PARITY")
	if dir == "" {
		t.Skip("DECK_PARITY is unset")
	}
	live := filepath.Join(dir, "deck", "live")
	tracked := map[Range]*SessionUsage{}
	for _, r := range Ranges {
		tracked[r] = &SessionUsage{}
		if !readJSON(filepath.Join(live, "tt-"+string(r)+".json"), tracked[r]) {
			t.Fatal(r)
		}
	}
	past, _ := readPast(context.Background(), "/home/bassim/hex")
	names, err := loadNames(context.Background(), "/home/bassim/hex", past)
	if err != nil {
		t.Fatal(err)
	}
	labels := Labels{Accounts: map[string]string{}, Devices: map[string]string{}}
	var accounts struct {
		Accounts []struct{ ID, Email string } `json:"accounts"`
	}
	readJSON(filepath.Join(dir, "deck", "state", "accounts.json"), &accounts)
	for _, a := range accounts.Accounts {
		labels.Accounts[a.ID] = a.Email
	}
	for _, d := range tracked[Week].Devices {
		labels.Devices[d.ID] = d.Label
	}
	var queries []string
	readJSON(filepath.Join(dir, "deck", "out", "queries.json"), &queries)
	type answer struct {
		Query string    `json:"query"`
		View  UsageView `json:"view"`
	}
	var answers []answer
	for _, query := range queries {
		params, _ := url.ParseQuery(query)
		q := ParseQuery(params)
		var parts []part
		for _, s := range tracked[q.Range].Sessions {
			parts = append(parts, partsOf(s, names)...)
		}
		answers = append(answers, answer{query, withLive(usageView(tracked[q.Range], parts, labels, q), nil)})
	}
	writeOut(t, filepath.Join(live, "go-views.json"), answers)
}

// TestLiveSnapshot runs the views on live inputs, read only: the fixture's
// events, hex, Claude Code and the pool's state files, and the running proxy's
// pool ranking saved in deck/live/pool.json. It writes the Accounts answer and
// the stream's snapshot for comparing against deck's.
func TestLiveSnapshot(t *testing.T) {
	dir := os.Getenv("DECK_PARITY")
	if dir == "" {
		t.Skip("DECK_PARITY is unset")
	}
	live := filepath.Join(dir, "deck", "live")
	var snap coreauth.PoolSnapshot
	if !readJSON(filepath.Join(live, "pool.json"), &snap) {
		t.Fatal("pool.json")
	}
	cfg := config.Config{}
	cfg.Routing.Pool.StateDir = "~/.local/state/tokenhub"
	cfg.Routing.Pool.AccountsFile = "~/.config/tokenhub/accounts.json"
	d := New(Options{
		Store:      loadFixture(t, dir),
		Pool:       func(*config.PoolConfig) coreauth.PoolSnapshot { return snap },
		ConfigPath: expand("~/.config/cliproxyapi/config.yaml", ""),
		Config:     cfg,
	})
	defer d.Close()
	d.sessions(Week)
	d.publish()
	accounts, err := d.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	writeOut(t, filepath.Join(live, "go-accounts.json"), accounts)
	client := d.stream.subscribe()
	first := <-client
	if err := os.WriteFile(filepath.Join(live, "go-events.txt"), first, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSpeed serves the routes through gin on the fixture and live inputs and
// reports each answer's size, gzipped size and time, cold and warm.
func TestSpeed(t *testing.T) {
	dir := os.Getenv("DECK_PARITY")
	if dir == "" {
		t.Skip("DECK_PARITY is unset")
	}
	var snap coreauth.PoolSnapshot
	readJSON(filepath.Join(dir, "deck", "live", "pool.json"), &snap)
	cfg := config.Config{}
	cfg.Routing.Pool.StateDir = "~/.local/state/tokenhub"
	cfg.Routing.Pool.AccountsFile = "~/.config/tokenhub/accounts.json"
	d := New(Options{Store: loadFixture(t, dir), Pool: func(*config.PoolConfig) coreauth.PoolSnapshot { return snap }, Config: cfg})
	defer d.Close()
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	d.Register(engine.Group("/deck"))
	get := func(path string) (int, int, time.Duration) {
		started := time.Now()
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		took := time.Since(started)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, recorder.Code, recorder.Body.String())
		}
		var zipped bytes.Buffer
		w := gzip.NewWriter(&zipped)
		_, _ = w.Write(recorder.Body.Bytes())
		_ = w.Close()
		return recorder.Body.Len(), zipped.Len(), took
	}
	for _, path := range []string{"/deck/usage?range=7d", "/deck/usage?range=7d", "/deck/usage?range=7d&by=session&measure=week",
		"/deck/usage?range=6m", "/deck/usage?range=6m", "/deck/usage?range=6m&by=session", "/deck/usage?range=6m&by=session",
		"/deck/usage?range=today", "/deck/accounts", "/deck/accounts", "/deck/sessions?range=7d"} {
		size, zipped, took := get(path)
		t.Logf("%-48s %7d B  gzip %6d B  %v", path, size, zipped, took)
	}
	t.Logf("events snapshot %d B", len(<-d.stream.subscribe()))
}
