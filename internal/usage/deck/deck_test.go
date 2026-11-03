package deck

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

func TestBucketsFollowTheZone(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	at := func(value string) time.Time {
		parsed, _ := time.ParseInLocation("2006-01-02 15:04", value, la)
		return parsed
	}
	cases := []struct {
		r       Range
		now     string
		size    string
		buckets int
		from    string
	}{
		{Today, "2026-11-01 08:00", "1h", 25, "2026-11-01 00:00"},
		{Today, "2026-03-08 12:00", "1h", 23, "2026-03-08 00:00"},
		{Yesterday, "2026-11-02 08:00", "1h", 25, "2026-11-01 00:00"},
		{Week, "2026-11-01 08:30", "4h", 49, "2026-10-25 08:30"},
		{Month, "2026-11-01 08:30", "1d", 31, "2026-10-02 08:30"},
		{HalfYear, "2026-08-30 10:00", "1w", 27, "2026-02-28 00:00"},
	}
	for _, c := range cases {
		frame := usageTimeframe(c.r, la, at(c.now))
		size, starts := bucketStarts(c.r, frame)
		if size != c.size || len(starts) != c.buckets || !frame.start.Equal(at(c.from)) {
			t.Errorf("%s at %s: %s × %d from %s, want %s × %d from %s", c.r, c.now, size, len(starts), frame.start, c.size, c.buckets, c.from)
		}
	}
}

func TestLocaleCompareMatchesJavaScript(t *testing.T) {
	// The order Bun's localeCompare gives.
	want := []string{"a", "A", "a b", "a_b", "a-b", "a:b", "a.b", "a/b", "a0", "b", "B", "github:1234:1", "github:12345:1"}
	got := append([]string(nil), want...)
	sort.Slice(got, func(i, j int) bool { return localeCompare(got[i], got[j]) < 0 })
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %q", got)
	}
}

func f(v float64) *float64 { return &v }

func preparedEvents(events ...usage.Event) []prepared {
	return prepare(events)
}

func TestLimitRisesSplitByCost(t *testing.T) {
	account := "acct"
	history := bindingHistory([]Binding{{SessionID: "s1", AccountID: &account, Event: "start", millis: 0}})
	events := preparedEvents(
		usage.Event{Harness: "claude", SessionID: "s1", Timestamp: "1970-01-01T00:01:40.000Z", CostUSD: 1},
		usage.Event{Harness: "claude", SessionID: "s1", Timestamp: "1970-01-01T00:02:30.000Z", CostUSD: 3},
		usage.Event{Harness: "codex", SessionID: "s1", Timestamp: "1970-01-01T00:02:40.000Z", CostUSD: 9},
	)
	reset := f(1e9)
	readings := []LimitReading{
		{Account: account, At: 50_000, FiveHour: f(2), FiveHourResetsAt: reset},
		{Account: account, At: 200_000, FiveHour: f(10), FiveHourResetsAt: reset},
		{Account: account, At: 300_000, FiveHour: f(13), FiveHourResetsAt: reset},
	}
	windows, charges := attributeLimits(readings, accountLookup(history), events, 1)
	if len(windows) != 1 {
		t.Fatalf("%d windows", len(windows))
	}
	w := windows[0]
	if charges[0].points[0] != 2 || charges[1].points[0] != 6 || charges[2] != nil {
		t.Fatalf("charges %v %v %v", charges[0], charges[1], charges[2])
	}
	// The baseline (2) and the last rise, which no bound request explains, stay unassigned.
	unassigned := 0.0
	for _, r := range w.unassigned {
		unassigned += r.points
	}
	if unassigned != 5 || w.sampled != 13 {
		t.Fatalf("sampled %v unassigned %v", w.sampled, unassigned)
	}
}

func TestManualResetNeedsALowerWeekAfter(t *testing.T) {
	rows := []LimitReading{
		{At: 1, FiveHour: f(40), Week: f(50)},
		{At: 2, FiveHour: f(0), Week: f(0)},
		{At: 3, FiveHour: f(5), Week: f(10)},
		{At: 4, FiveHour: f(0), Week: f(0)},
		{At: 5, FiveHour: f(1), Week: f(60)},
	}
	if got := manualResetsOf(rows); !got[2] || got[4] {
		t.Fatalf("manual resets %v", got)
	}
}

func TestScansThatSawTwoAccountsBindNone(t *testing.T) {
	a, b := "a", "b"
	history := bindingHistory([]Binding{
		{SessionID: "hooked", AccountID: &a, Event: "start", millis: 10},
		{SessionID: "hooked", AccountID: &b, Event: "observe-live", millis: 20},
		{SessionID: "scanned", AccountID: &a, Event: "observe-live", millis: 30},
		{SessionID: "scanned", AccountID: &b, Event: "observe-live", millis: 900},
		{SessionID: "scanned", AccountID: &b, Event: "observe-live", millis: 5000},
	})
	lookup := accountLookup(history)
	at := func(session string, ms float64) string {
		return lookup(&prepared{Event: &usage.Event{Harness: "claude", SessionID: session}, millis: ms})
	}
	if at("hooked", 30) != "a" || at("scanned", 100) != "" || at("scanned", 6000) != "b" || at("scanned", 10) != "" {
		t.Fatalf("hooked %q, scanned %q %q %q", at("hooked", 30), at("scanned", 100), at("scanned", 6000), at("scanned", 10))
	}
}

func TestAppendLogReadsOnlyWhatWasAdded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-history.ndjson")
	log := &appendLog[LimitReading]{path: path, parse: parseReading}
	write := func(text string, flag int) {
		file, err := os.OpenFile(path, flag|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.WriteString(text)
		_ = file.Close()
	}
	write(`{"account":"a","at":1,"fiveHour":1,"week":2}`+"\n"+`{"account":"a","at":2,`, os.O_TRUNC)
	if rows, _ := log.read(); len(rows) != 1 {
		t.Fatalf("%d rows before the line ends", len(rows))
	}
	write(`"fiveHour":null,"week":3}`+"\nnot json\n"+`{"at":3}`+"\n", os.O_APPEND)
	if rows, _ := log.read(); len(rows) != 2 || rows[1].FiveHour != nil || *rows[1].Week != 3 {
		t.Fatalf("rows %+v", rows)
	}
	write(`{"account":"b","at":9,"fiveHour":0,"week":0}`+"\n", os.O_TRUNC)
	if rows, _ := log.read(); len(rows) != 1 || rows[0].Account != "b" {
		t.Fatalf("after shrinking: %+v", rows)
	}
}

func TestStreamSendsOnlyChangedKeys(t *testing.T) {
	s := newStream()
	s.publish([]liveEntry{{"s:1", 1}, {"s:2", 2}, {"pool", "p"}})
	client := s.subscribe()
	if got := string(<-client); got != `data: {"type":"snapshot","live":{"s:1":1,"s:2":2,"pool":"p"}}`+"\n\n" {
		t.Fatalf("snapshot %s", got)
	}
	s.publish([]liveEntry{{"s:3", 3}, {"s:2", 20}, {"pool", "p"}})
	if got := string(<-client); got != `data: {"type":"patch","live":{"s:3":3,"s:2":20,"s:1":null}}`+"\n\n" {
		t.Fatalf("patch %s", got)
	}
	if strings.Join(s.keys, " ") != "s:2 pool s:3" {
		t.Fatalf("order %v", s.keys)
	}
	s.publish([]liveEntry{{"s:3", 3}, {"s:2", 20}, {"pool", "p"}})
	s.stale("usage")
	if got := string(<-client); got != `data: {"type":"stale","tags":["usage"]}`+"\n\n" {
		t.Fatalf("an unchanged publish sent %s", got)
	}
}

func TestPlanRateSpreadsMonthlyFees(t *testing.T) {
	utc := time.UTC
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, utc)
	plans := []Plan{{Name: "Claude", Providers: []string{"anthropic"}, Seats: []Seat{{MonthlyUSD: 310, Since: float64(time.Date(2026, 9, 1, 0, 0, 0, 0, utc).UnixMilli())}}}}
	events := preparedEvents(
		usage.Event{Model: "claude-opus-4-5", Timestamp: "2026-09-10T00:00:00Z", CostUSD: 600, CostKnown: true},
		usage.Event{Model: "claude-opus-4-5", Timestamp: "2026-10-10T00:00:00Z", CostUSD: 50, CostKnown: true},
		usage.Event{Model: "gpt-5", Timestamp: "2026-10-10T00:00:00Z", CostUSD: 70, CostKnown: true},
	)
	p := newPlanPricing(plans, events, utc, now)
	// September's whole fee over its API value; October's fees so far (15 of 31 days) over October's.
	if got := p.price(&events[0]); math.Abs(got-310) > 1e-9 {
		t.Fatalf("September plan $ %v", got)
	}
	if got := p.price(&events[1]); math.Abs(got-150) > 1e-9 {
		t.Fatalf("October plan $ %v", got)
	}
	if got := p.price(&events[2]); got != 70 {
		t.Fatalf("an uncovered provider's plan $ %v", got)
	}
}

func TestAgentSDKRequestsCarryTheFactorOnTheFiveHourLimitOnly(t *testing.T) {
	account := "acct"
	history := bindingHistory([]Binding{
		{SessionID: "sdk", AccountID: &account, Event: "start", millis: 0},
		{SessionID: "tui", AccountID: &account, Event: "start", millis: 0},
	})
	events := preparedEvents(
		usage.Event{Harness: "claude", SessionID: "sdk", Timestamp: "1970-01-01T00:01:40.000Z", CostUSD: 1, Entrypoint: "sdk-cli"},
		usage.Event{Harness: "claude", SessionID: "tui", Timestamp: "1970-01-01T00:01:50.000Z", CostUSD: 1, Entrypoint: "cli"},
	)
	reset, weekReset := f(1e9), f(2e9)
	readings := []LimitReading{
		{Account: account, At: 50_000, FiveHour: f(1), FiveHourResetsAt: reset, Week: f(1), WeekResetsAt: weekReset},
		{Account: account, At: 200_000, FiveHour: f(4), FiveHourResetsAt: reset, Week: f(3), WeekResetsAt: weekReset},
	}
	_, charges := attributeLimits(readings, accountLookup(history), events, 2)
	if charges[0].points[0] != 2 || charges[1].points[0] != 1 {
		t.Fatalf("five-hour charges %v and %v, want 2 and 1", charges[0].points[0], charges[1].points[0])
	}
	if charges[0].points[1] != 1 || charges[1].points[1] != 1 {
		t.Fatalf("week charges %v and %v, want 1 and 1", charges[0].points[1], charges[1].points[1])
	}
}
