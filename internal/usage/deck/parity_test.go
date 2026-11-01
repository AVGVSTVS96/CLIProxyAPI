package deck

import (
	"bufio"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
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
		answers = append(answers, answer{query, withLive(usageView(tracked[q.Range], parts[q.Range], labels, q), nil, 0)})
	}
	t.Logf("%d views %v", len(queries), time.Since(started))
	writeOut(t, filepath.Join(out, "go-views.json"), answers)
}
