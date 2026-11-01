package deck

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"sort"
	"sync"
)

// Binding is one row of the pool's per-session account log (attribution.ndjson).
type Binding struct {
	At        string  `json:"at"`
	SessionID string  `json:"sessionId"`
	AccountID *string `json:"accountId"`
	Event     string  `json:"event"`
	Cause     string  `json:"cause,omitempty"`
	millis    float64
}

// LimitReading is one sample of an account's limits (usage-history.ndjson).
type LimitReading struct {
	Account          string   `json:"account"`
	At               float64  `json:"at"`
	FiveHour         *float64 `json:"fiveHour"`
	Week             *float64 `json:"week"`
	FiveHourResetsAt *float64 `json:"fiveHourResetsAt"`
	WeekResetsAt     *float64 `json:"weekResetsAt"`
}

func (r LimitReading) value(kind string) *float64 {
	if kind == fiveHour {
		return r.FiveHour
	}
	return r.Week
}

func (r LimitReading) resetsAt(kind string) *float64 {
	if kind == fiveHour {
		return r.FiveHourResetsAt
	}
	return r.WeekResetsAt
}

// appendLog follows an append-only ndjson file, parsing only the bytes added
// since the last read; a file that shrank is read again from the start.
type appendLog[T any] struct {
	mu    sync.Mutex
	path  string
	parse func([]byte) (T, bool)
	size  int64
	rest  []byte
	rows  []T
}

func (l *appendLog[T]) read() ([]T, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	info, err := os.Stat(l.path)
	size := int64(0)
	if err == nil {
		size = info.Size()
	}
	if size < l.size {
		l.size, l.rest, l.rows = 0, nil, nil
	}
	if size == l.size {
		return l.rows, false
	}
	f, err := os.Open(l.path)
	if err != nil {
		return l.rows, false
	}
	defer f.Close()
	added := make([]byte, size-l.size)
	n, _ := f.ReadAt(added, l.size)
	text := append(l.rest, added[:n]...)
	cut := bytes.LastIndexByte(text, '\n')
	var rows []T
	for _, line := range bytes.Split(text[:max(cut, 0)], []byte{'\n'}) {
		if row, ok := l.parse(line); ok {
			rows = append(rows, row)
		}
	}
	l.rest = append([]byte(nil), text[cut+1:]...)
	l.size += int64(n)
	next := make([]T, 0, len(l.rows)+len(rows))
	l.rows = append(append(next, l.rows...), rows...)
	return l.rows, true
}

func parseReading(line []byte) (LimitReading, bool) {
	var probe struct {
		Account *string  `json:"account"`
		At      *float64 `json:"at"`
	}
	var row LimitReading
	if json.Unmarshal(line, &probe) != nil || probe.Account == nil || probe.At == nil || json.Unmarshal(line, &row) != nil {
		return row, false
	}
	return row, true
}

func parseBinding(line []byte) (Binding, bool) {
	var row struct {
		At        any `json:"at"`
		SessionID any `json:"sessionId"`
		AccountID any `json:"accountId"`
		Event     any `json:"event"`
		Cause     any `json:"cause"`
	}
	if json.Unmarshal(line, &row) != nil {
		return Binding{}, false
	}
	at, _ := row.At.(string)
	session, _ := row.SessionID.(string)
	event, _ := row.Event.(string)
	millis := parseMillis(at)
	if at == "" || math.IsNaN(millis) || session == "" {
		return Binding{}, false
	}
	if event != "start" && event != "failure" && event != "observe-live" {
		return Binding{}, false
	}
	binding := Binding{At: at, SessionID: session, Event: event, millis: millis}
	binding.Cause, _ = row.Cause.(string)
	switch account := row.AccountID.(type) {
	case string:
		binding.AccountID = &account
	case nil:
		if event != "observe-live" {
			return Binding{}, false
		}
	default:
		return Binding{}, false
	}
	return binding, true
}

// sortedReadings returns readings in time order, keeping file order for ties.
func sortedReadings(rows []LimitReading) []LimitReading {
	out := append([]LimitReading(nil), rows...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// bindingHistory is token-tracker's attribution history: account-bound hooks,
// plus process scans for sessions no hook named, where a scan that saw several
// accounts within a second binds the session to none.
func bindingHistory(all []Binding) []Binding {
	rows := append([]Binding(nil), all...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].millis < rows[j].millis })
	authoritative := map[string]bool{}
	var history []Binding
	for _, row := range rows {
		if row.Event != "observe-live" {
			if row.AccountID != nil && *row.AccountID != "" {
				authoritative[row.SessionID] = true
			}
			history = append(history, row)
		}
	}
	var order []string
	observed := map[string][]Binding{}
	for _, row := range rows {
		if row.Event != "observe-live" || authoritative[row.SessionID] {
			continue
		}
		if _, seen := observed[row.SessionID]; !seen {
			order = append(order, row.SessionID)
		}
		observed[row.SessionID] = append(observed[row.SessionID], row)
	}
	for _, session := range order {
		observations := observed[session]
		for i := 0; i < len(observations); {
			first := observations[i]
			accounts := map[string]bool{accountKey(first.AccountID): true}
			for i++; i < len(observations) && observations[i].millis-first.millis <= 1000; i++ {
				accounts[accountKey(observations[i].AccountID)] = true
			}
			if len(accounts) != 1 {
				first.AccountID = nil
			}
			history = append(history, first)
		}
	}
	sort.SliceStable(history, func(i, j int) bool { return history[i].millis < history[j].millis })
	return history
}

func accountKey(account *string) string {
	if account == nil {
		return "\x00null"
	}
	return *account
}

// accountLookup finds the account a Claude request's session was bound to at
// the request's own time.
func accountLookup(history []Binding) func(event *prepared) string {
	bySession := map[string][]Binding{}
	for _, row := range history {
		bySession[row.SessionID] = append(bySession[row.SessionID], row)
	}
	return func(event *prepared) string {
		list := bySession[event.SessionID]
		if len(list) == 0 || event.Harness != "claude" || math.IsNaN(event.millis) {
			return ""
		}
		at := sort.Search(len(list), func(i int) bool { return list[i].millis > event.millis }) - 1
		if at < 0 || list[at].AccountID == nil {
			return ""
		}
		return *list[at].AccountID
	}
}
