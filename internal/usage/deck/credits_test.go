package deck

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

type eventStore []usage.Event

func (s eventStore) Events() []usage.Event   { return s }
func (s eventStore) Devices() []usage.Device { return nil }
func (s eventStore) Revision() uint64        { return 0 }
func (s eventStore) Subscribe(func()) func() { return func() {} }

func TestCreditRemainingFollowsProxySpendAfterTheConsoleReading(t *testing.T) {
	dir := t.TempDir()
	start := time.Now().Add(-2 * time.Hour)
	observed := start.Add(time.Hour)
	at := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	grant := `[{"account":"a","organization":"o","credentialId":"key","remaining":150,"allowance":200,"monthlyCap":200,` +
		`"startsAt":` + strconv.FormatInt(start.UnixMilli(), 10) + `,"observedAt":` + strconv.FormatInt(observed.UnixMilli(), 10) + `,"expiresAt":` + strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10) + `}]`
	if err := os.WriteFile(filepath.Join(dir, "credits.json"), []byte(grant), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := func(when time.Time, cost float64, known, failed bool) usage.Event {
		return usage.Event{Source: "proxy", AccountID: "key", Timestamp: at(when), CostUSD: cost, CostKnown: known, Failed: failed}
	}
	d := &Deck{opts: Options{Store: eventStore{
		proxy(start.Add(time.Minute), 50, true, false),
		proxy(observed.Add(time.Minute), 30, true, false),
		proxy(observed.Add(2*time.Minute), 0, false, true),
		proxy(observed.Add(3*time.Minute), 0, false, false),
	}}}
	d.paths.stateDir = dir
	view, err := d.Credits()
	if err != nil {
		t.Fatal(err)
	}
	got := view.API[0]
	if got.Remaining != 120 || got.Recorded != 150 || got.ProxyUSD != 80 || got.Requests != 4 || got.Unpriced != 1 {
		t.Fatalf("got %+v", got)
	}
}
