package deck

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// LiveSession is a Claude session running now.
type LiveSession struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// The Usage row key this session ranks under with by=agent.
	Row   string  `json:"row"`
	App   *string `json:"app"`
	State string  `json:"state"`
	// The account the pool has the session on.
	Account *string `json:"account"`
	Host    string  `json:"host"`
	Cwd     string  `json:"cwd"`
	Since   float64 `json:"since"`
}

type running struct {
	sessionID, cwd string
	name           *string
	busy           bool
	startedAt      float64
}

type blocked struct {
	sessionID string
	name      *string
	cwd       string
	since     float64
}

type processes struct {
	running []running
	blocked []blocked
	apps    []appState
}

type appState struct {
	name      string
	connected bool
}

var pidFile = regexp.MustCompile(`^\d+\.json$`)

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readProcesses lists running Claude sessions, blocked jobs and which hex apps
// are connected.
func readProcesses(claude, hex string) processes {
	var found processes
	byID := map[string]running{}
	entries, _ := os.ReadDir(filepath.Join(claude, "sessions"))
	for _, entry := range entries {
		if !pidFile.MatchString(entry.Name()) {
			continue
		}
		var s struct {
			SessionID string   `json:"sessionId"`
			PID       *float64 `json:"pid"`
			Cwd       string   `json:"cwd"`
			Name      *string  `json:"name"`
			Status    string   `json:"status"`
			StartedAt float64  `json:"startedAt"`
		}
		if !readJSON(filepath.Join(claude, "sessions", entry.Name()), &s) || s.SessionID == "" || s.PID == nil ||
			!exists("/proc/"+strconv.FormatFloat(*s.PID, 'f', -1, 64)) {
			continue
		}
		byID[s.SessionID] = running{sessionID: s.SessionID, cwd: s.Cwd, name: s.Name, busy: s.Status == "busy", startedAt: s.StartedAt}
	}
	for _, r := range byID {
		found.running = append(found.running, r)
	}
	sort.Slice(found.running, func(i, j int) bool { return localeCompare(found.running[i].sessionID, found.running[j].sessionID) < 0 })

	jobs, _ := os.ReadDir(filepath.Join(claude, "jobs"))
	for _, job := range jobs {
		var state struct {
			State     string  `json:"state"`
			SessionID string  `json:"sessionId"`
			Name      *string `json:"name"`
			Cwd       string  `json:"cwd"`
			UpdatedAt string  `json:"updatedAt"`
		}
		if !readJSON(filepath.Join(claude, "jobs", job.Name(), "state.json"), &state) || state.State != "blocked" || state.SessionID == "" {
			continue
		}
		since := parseMillis(state.UpdatedAt)
		if math.IsNaN(since) {
			since = 0
		}
		found.blocked = append(found.blocked, blocked{sessionID: state.SessionID, name: state.Name, cwd: state.Cwd, since: since})
	}
	sort.Slice(found.blocked, func(i, j int) bool { return localeCompare(found.blocked[i].sessionID, found.blocked[j].sessionID) < 0 })

	apps, _ := os.ReadDir(filepath.Join(hex, "state"))
	for _, app := range apps {
		dir := filepath.Join(hex, "state", app.Name())
		if exists(filepath.Join(dir, "sessions")) {
			found.apps = append(found.apps, appState{name: app.Name(), connected: exists(filepath.Join(dir, "hub.sock"))})
		}
	}
	return found
}

// liveEntry is one key of the live map and its JSON.
type liveEntry struct {
	key   string
	value any
}

// liveState is what is true right now, as a flat list of keys the stream diffs:
// `s:<session>`, `a:<account>`, `pool`, `app:<name>`. Only fields that change
// when something happens enter it, never clocks or sample ages.
type liveState struct {
	sessions map[string]*LiveSession
	order    []*LiveSession
	entries  []liveEntry
}

// liveMap builds the live state. firstSeen holds when each session was first
// seen, for those with no start time of their own.
func liveMap(found processes, pool *poolState, names *Names, tracked *SessionUsage, firstSeen map[string]float64, now time.Time) *liveState {
	state := &liveState{sessions: map[string]*LiveSession{}}
	seats := map[string]string{}
	if pool != nil {
		for _, seat := range pool.seats {
			seats[seat.session] = seat.account
		}
	}
	known := map[string]*Session{}
	projects := map[string]string{}
	if tracked != nil {
		for _, s := range tracked.Sessions {
			known[s.SessionID] = s
			projects[s.Cwd] = s.Project
		}
	}
	blockedIDs := map[string]bool{}
	for _, job := range found.blocked {
		blockedIDs[job.sessionID] = true
	}

	session := func(id string, name *string, cwd, live string, since float64, host string) *LiveSession {
		record := known[id]
		seen, ok := firstSeen[id]
		if !ok {
			seen = float64(now.UnixMilli())
			firstSeen[id] = seen
		}
		project := cwd
		if record != nil {
			project = record.Project
		} else if p, ok := projects[cwd]; ok {
			project = p
		}
		who := func(title *string) Who {
			if names == nil {
				return Who{Kind: "project", Project: project}
			}
			return whoOf(names, sessionRef{sessionID: id, title: title, project: project})
		}
		var title *string
		if record != nil {
			title = record.Title
		}
		ranked := who(title)
		shown := ranked
		if name != nil && *name != "" && title == nil {
			shown = who(name)
		}
		key := id
		if record != nil {
			key = record.Key
			if cwd == "" {
				cwd = record.Cwd
			}
			if since == 0 {
				since = record.FirstAt
			}
		}
		if since == 0 {
			since = seen
		}
		label, _ := labelOf(shown)
		s := &LiveSession{ID: id, Label: label, Row: rowOf(ranked, key), State: live, Host: host, Cwd: cwd, Since: since}
		if ranked.Kind == "thread" {
			s.App = ptr(ranked.App)
		}
		if account, ok := seats[id]; ok {
			s.Account = &account
		}
		return s
	}
	add := func(s *LiveSession) {
		state.order = append(state.order, s)
		state.sessions[s.ID] = s
	}
	for _, r := range found.running {
		live := "idle"
		if blockedIDs[r.sessionID] {
			live = "blocked"
		} else if r.busy {
			live = "working"
		}
		add(session(r.sessionID, r.name, r.cwd, live, r.startedAt, "server"))
	}
	for _, job := range found.blocked {
		if _, ok := state.sessions[job.sessionID]; !ok {
			add(session(job.sessionID, job.name, job.cwd, "blocked", job.since, "server"))
		}
	}
	for id := range firstSeen {
		if _, ok := state.sessions[id]; !ok {
			delete(firstSeen, id)
		}
	}

	for _, s := range state.order {
		state.entries = append(state.entries, liveEntry{"s:" + s.ID, s})
	}
	if pool != nil {
		for _, account := range pool.accounts {
			state.entries = append(state.entries, liveEntry{"a:" + account.ID, account})
		}
		state.entries = append(state.entries, liveEntry{"pool", pool.pool})
	}
	for _, app := range found.apps {
		state.entries = append(state.entries, liveEntry{"app:" + app.name, map[string]bool{"connected": app.connected}})
	}
	return state
}

func encode(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
