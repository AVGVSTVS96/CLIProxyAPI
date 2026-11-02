// Package deck serves the dashboard's Usage, Live and Accounts pages from the
// usage store and the account pool: per-session usage with limit attribution and
// plan prices, ranked views, the live sessions stream and the pool's accounts.
package deck

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Store is the collector's usage store, as the views read it.
type Store interface {
	Events() []usage.Event
	Devices() []usage.Device
	Revision() uint64
	Subscribe(func()) func()
}

// Options are what the views read.
type Options struct {
	Store Store
	// Pool ranks the Claude accounts, under proposed settings when preview is non-nil.
	Pool func(preview *config.PoolConfig) coreauth.PoolSnapshot
	// ConfigPath is the config file whose hash is the pool policy's revision.
	ConfigPath string
	// Engine serves the management API, which saves policy changes.
	Engine http.Handler
	Config config.Config
}

// paths are the files and folders the views read, from the usage and routing.pool config.
type paths struct {
	zone         *time.Location
	hex, claude  string
	stateDir     string
	accountsFile string
	plans        []config.UsagePlan
}

func pathsOf(cfg config.Config) paths {
	p := paths{
		zone:         time.Local,
		hex:          expand(cfg.Usage.Hex, "~/hex"),
		claude:       expand(cfg.Usage.Claude, "~/.claude"),
		stateDir:     expand(cfg.Routing.Pool.StateDir, ""),
		accountsFile: expand(cfg.Routing.Pool.AccountsFile, ""),
		plans:        cfg.Usage.Plans,
	}
	zone := cfg.Usage.Timezone
	if zone == "" {
		zone = localZone()
	}
	if loc, err := time.LoadLocation(zone); err == nil {
		p.zone = loc
	} else {
		log.Warnf("usage: timezone %q: %v; days count in UTC", zone, err)
		p.zone = time.UTC
	}
	return p
}

func expand(path, fallback string) string {
	if path == "" {
		path = fallback
	}
	if rest, ok := strings.CutPrefix(path, "~"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

// localZone is the machine's IANA zone name, which Go's time.Local doesn't carry.
func localZone() string {
	if zone := os.Getenv("TZ"); zone != "" {
		return strings.TrimPrefix(zone, ":")
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(link, "zoneinfo/"); ok {
			return zone
		}
	}
	return "UTC"
}

const (
	// A range older than this reloads behind the request that finds it.
	freshFor = 5 * time.Minute
	// Ranges asked for this recently reload when usage changes.
	viewedFor = 10 * time.Minute
	// Plan rates spread a month's fees, so they rebuild at most this often.
	plansFor  = 10 * time.Minute
	heartbeat = 20 * time.Second
)

type rangeCache struct {
	value  *SessionUsage
	parts  []part
	names  *Names
	news   string
	at     time.Time
	viewed time.Time
	// loading is open while a load runs and closes when it ends.
	loading chan struct{}
}

// Deck serves the views. Every input it reads is re-read when it changes.
type Deck struct {
	opts   Options
	ctx    context.Context
	cancel context.CancelFunc
	stream *stream

	poolChanged func()
	readings    *appendLog[LimitReading]
	bindings    *appendLog[Binding]

	mu        sync.Mutex
	paths     paths
	names     *Names
	namesErr  error
	past      map[string][2]string
	procs     processes
	pool      *poolState
	live      *liveState
	firstSeen map[string]float64
	events    []prepared
	eventsRev uint64
	plans     *planPricing
	plansKey  string
	plansAt   time.Time
	ranges    map[Range]*rangeCache
	views     map[string]*UsageView
	labels    string
}

// New starts the views: it reads every input once, then follows their changes until Close.
func New(opts Options) *Deck {
	ctx, cancel := context.WithCancel(context.Background())
	d := &Deck{opts: opts, ctx: ctx, cancel: cancel, stream: newStream(), paths: pathsOf(opts.Config),
		firstSeen: map[string]float64{}, ranges: map[Range]*rangeCache{}, views: map[string]*UsageView{}}
	for _, r := range Ranges {
		d.ranges[r] = &rangeCache{}
	}
	d.ranges[Week].viewed = time.Now()
	d.readings = &appendLog[LimitReading]{path: d.stateFile("usage-history.ndjson"), parse: parseReading}
	d.bindings = &appendLog[Binding]{path: d.stateFile("attribution.ndjson"), parse: parseBinding}

	d.refreshProcesses()
	d.refreshNames()
	d.refreshPool()
	d.publish()
	go d.warm(Week)

	usageChanged := burst(ctx, 2*time.Second, d.usageChanged)
	cancelStore := opts.Store.Subscribe(usageChanged)
	go func() { <-ctx.Done(); cancelStore() }()
	if exists(d.hexCommand()) {
		go d.follow([]string{d.hexCommand(), "watch"}, burst(ctx, 250*time.Millisecond, func() {
			d.refreshNames()
			d.publish()
			d.resetViews()
		}))
	}
	go d.watch(burst(ctx, 250*time.Millisecond, func() { d.refreshProcesses(); d.publish() }),
		watchSpec{dir: filepath.Join(d.paths.claude, "sessions"), match: func(rel string) bool { return pidFile.MatchString(rel) }},
		watchSpec{dir: filepath.Join(d.paths.claude, "jobs"), depth: 1, match: func(rel string) bool { return strings.HasSuffix(rel, "/state.json") }},
		watchSpec{dir: filepath.Join(d.paths.hex, "state"), depth: 1, match: func(rel string) bool {
			return strings.HasSuffix(rel, "/hub.sock") || strings.HasSuffix(rel, "/sessions")
		}})
	d.poolChanged = burst(ctx, time.Second, func() { d.refreshPool(); d.publish() })
	logsChanged := burst(ctx, 30*time.Second, func() { d.stream.stale("accounts") })
	if d.paths.stateDir != "" {
		go d.watch(d.poolChanged, watchSpec{dir: d.paths.stateDir, depth: 1, match: func(rel string) bool {
			if rel == "usage-history.ndjson" || rel == "attribution.ndjson" {
				logsChanged()
				usageChanged()
				return true
			}
			return strings.HasPrefix(rel, "resets/") || strings.HasPrefix(rel, "plans/")
		}})
	}
	go d.every(time.Minute, d.poolChanged)
	go d.every(freshFor, func() {
		if d.stream.size() > 0 {
			usageChanged()
		}
	})
	go d.every(heartbeat, d.stream.heartbeat)
	return d
}

// stateFile is a file in the pool's state folder; none without one.
func (d *Deck) stateFile(name string) string {
	if d.paths.stateDir == "" {
		return ""
	}
	return filepath.Join(d.paths.stateDir, name)
}

// Close stops following changes and ends every stream.
func (d *Deck) Close() { d.cancel() }

// Stale tells every open stream to refetch the resources tags name, like "plugins" or "repo:hex".
func (d *Deck) Stale(tags ...string) { d.stream.stale(tags...) }

// SetConfig applies a reloaded config's usage settings.
func (d *Deck) SetConfig(cfg config.Config) {
	d.mu.Lock()
	d.opts.Config = cfg
	d.paths.plans = pathsOf(cfg).plans
	d.plansKey = ""
	d.mu.Unlock()
	d.poolChanged()
	d.usageChanged()
}

func (d *Deck) every(interval time.Duration, fn func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			fn()
		}
	}
}

// burst calls fn once, wait after the first call in a burst; a steady stream of calls can't starve it.
func burst(ctx context.Context, wait time.Duration, fn func()) func() {
	var mu sync.Mutex
	var timer *time.Timer
	return func() {
		mu.Lock()
		defer mu.Unlock()
		if timer != nil || ctx.Err() != nil {
			return
		}
		timer = time.AfterFunc(wait, func() {
			mu.Lock()
			timer = nil
			mu.Unlock()
			if ctx.Err() == nil {
				fn()
			}
		})
	}
}

type watchSpec struct {
	dir   string
	depth int
	match func(rel string) bool
}

// watch calls changed for each file event a spec matches, following new
// subfolders up to the spec's depth.
func (d *Deck) watch(changed func(), specs ...watchSpec) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Warnf("usage: watch: %v", err)
		return
	}
	defer watcher.Close()
	add := func(dir string) {
		if err := watcher.Add(dir); err != nil && !os.IsNotExist(err) {
			log.Debugf("usage: watch %s: %v", dir, err)
		}
	}
	for _, s := range specs {
		add(s.dir)
		if s.depth > 0 {
			entries, _ := os.ReadDir(s.dir)
			for _, entry := range entries {
				if entry.IsDir() {
					add(filepath.Join(s.dir, entry.Name()))
				}
			}
		}
	}
	for {
		select {
		case <-d.ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			for _, s := range specs {
				rel, err := filepath.Rel(s.dir, event.Name)
				if err != nil || strings.HasPrefix(rel, "..") {
					continue
				}
				if s.depth > 0 && !strings.Contains(rel, "/") && event.Has(fsnotify.Create) {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						add(event.Name)
					}
				}
				if s.match(rel) {
					changed()
				}
			}
		case <-watcher.Errors:
		}
	}
}

// follow runs a command that prints a line per change, like `hex watch`, and
// calls changed for each; one that ends starts again.
func (d *Deck) follow(command []string, changed func()) {
	for d.ctx.Err() == nil {
		cmd := exec.CommandContext(d.ctx, command[0], command[1:]...)
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err == nil {
			for scanner := bufio.NewScanner(out); scanner.Scan(); {
				changed()
			}
			err = cmd.Wait()
		}
		if d.ctx.Err() == nil {
			log.Warnf("usage: %s exited (%v); starting it again", strings.Join(command, " "), err)
		}
		select {
		case <-d.ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}

func (d *Deck) refreshProcesses() {
	found := readProcesses(d.paths.claude, d.paths.hex)
	d.mu.Lock()
	d.procs = found
	d.mu.Unlock()
}

func (d *Deck) hexCommand() string { return filepath.Join(d.paths.hex, ".hex", "bin", "hex") }

// refreshNames reads hex's thread names; without hex, sessions go unnamed.
func (d *Deck) refreshNames() {
	if !exists(d.hexCommand()) {
		d.mu.Lock()
		d.names = &Names{}
		d.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, time.Minute)
	defer cancel()
	d.mu.Lock()
	past := d.past
	d.mu.Unlock()
	var err error
	if past == nil {
		if past, err = readPast(ctx, d.paths.hex); err != nil {
			log.Warnf("usage: %v", err)
		}
	}
	names, err := loadNames(ctx, d.paths.hex, past)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.past = past
	if err != nil {
		d.namesErr = err
		log.Warnf("usage: %v", err)
		return
	}
	d.names, d.namesErr = names, nil
}

// revision is the pool policy's revision: the config file's hash, as the v8
// config API's ETag gives it.
func (d *Deck) revision() string {
	data, err := os.ReadFile(d.opts.ConfigPath)
	if err != nil {
		return ""
	}
	return `"` + config.ComputeSourceHash(data) + `"`
}

func (d *Deck) refreshPool() {
	if d.opts.Pool == nil {
		return
	}
	bindings, _ := d.bindings.read()
	state := parsePool(d.opts.Pool(nil), d.revision(), seatsOf(bindings, time.Now()))
	d.mu.Lock()
	previous := d.labels
	d.pool = state
	labels := make(map[string]string, len(state.accounts))
	for _, a := range state.accounts {
		labels[a.ID] = a.Label
	}
	d.labels = string(encode(labels))
	d.mu.Unlock()
	if previous != "" && previous != d.labels {
		d.resetViews()
	}
}

// publish sends the live keys that changed.
func (d *Deck) publish() {
	d.mu.Lock()
	tracked := d.ranges[Week].value
	if tracked == nil {
		tracked = d.ranges[Today].value
	}
	d.live = liveMap(d.procs, d.pool, d.names, tracked, d.firstSeen, time.Now())
	entries := d.live.entries
	d.mu.Unlock()
	d.stream.publish(entries)
}

func (d *Deck) resetViews() {
	d.mu.Lock()
	d.views = map[string]*UsageView{}
	d.mu.Unlock()
	d.stream.stale("usage")
}

// usageChanged reloads the ranges in view after the store, limits or plans change.
func (d *Deck) usageChanged() {
	d.mu.Lock()
	var due []Range
	for _, r := range Ranges {
		c := d.ranges[r]
		if c.value != nil && (time.Since(c.viewed) < viewedFor || (r == Week && d.stream.size() > 0)) {
			due = append(due, r)
		}
	}
	d.mu.Unlock()
	for _, r := range due {
		go d.warm(r)
	}
}

// prepared returns the store's events with what each view derives from them, once per revision.
func (d *Deck) prepared() ([]prepared, uint64) {
	revision := d.opts.Store.Revision()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.events == nil || d.eventsRev != revision {
		d.events, d.eventsRev = prepare(d.opts.Store.Events()), revision
	}
	return d.events, d.eventsRev
}

func (d *Deck) planPricing(events []prepared, revision uint64, now time.Time) *planPricing {
	d.mu.Lock()
	p := d.paths
	d.mu.Unlock()
	plans := readPlans(p.accountsFile, p.stateDir, p.plans)
	key := fmt.Sprintf("%d %s", revision, encode(plans))
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.plans == nil || d.plansKey != key || now.Sub(d.plansAt) > plansFor {
		d.plans, d.plansKey, d.plansAt = newPlanPricing(plans, events, p.zone, now), key, now
	}
	return d.plans
}

// load computes one range's session usage.
func (d *Deck) load(r Range) *SessionUsage {
	events, revision := d.prepared()
	readings, _ := d.readings.read()
	bindings, _ := d.bindings.read()
	now := time.Now()
	plans := d.planPricing(events, revision, now)
	limits := &limitInputs{readings: sortedReadings(readings), history: bindingHistory(bindings)}
	return sessionUsage(events, r, d.paths.zone, now, plans, limits, d.opts.Store.Devices())
}

// warm reloads a range, one load at a time; listeners hear only when its
// sessions or limit windows changed, since every load moves the clock fields.
func (d *Deck) warm(r Range) {
	d.mu.Lock()
	c := d.ranges[r]
	if c.loading != nil {
		d.mu.Unlock()
		return
	}
	done := make(chan struct{})
	c.loading = done
	d.mu.Unlock()
	defer close(done)
	value := d.load(r)
	hash := sha256.New()
	_ = json.NewEncoder(hash).Encode([]any{value.Sessions, value.Windows})
	news := string(hash.Sum(nil))
	d.mu.Lock()
	changed := c.news != news
	c.value, c.news, c.at, c.loading = value, news, time.Now(), nil
	if changed {
		c.parts, c.names = nil, nil
		for k := range d.views {
			if strings.Contains(k, `"range":"`+string(r)+`"`) {
				delete(d.views, k)
			}
		}
	}
	d.mu.Unlock()
	if changed {
		if r == Week || r == Today {
			d.publish()
		}
		d.stream.stale("usage")
	}
}

// sessions returns a range's session usage: a request never waits on a loaded
// range, and one older than freshFor reloads behind it.
func (d *Deck) sessions(r Range) *rangeCache {
	for {
		d.mu.Lock()
		c := d.ranges[r]
		c.viewed = time.Now()
		loaded, loading, at := c.value != nil, c.loading, c.at
		d.mu.Unlock()
		switch {
		case loaded:
			if time.Since(at) > freshFor {
				go d.warm(r)
			}
			return c
		case loading != nil:
			<-loading
		default:
			d.warm(r)
		}
	}
}

func (d *Deck) labelsLocked() Labels {
	labels := Labels{Accounts: map[string]string{}, Devices: map[string]string{}}
	if d.pool != nil {
		for _, a := range d.pool.accounts {
			labels.Accounts[a.ID] = a.Label
		}
	}
	tracked := d.ranges[Week].value
	if tracked == nil {
		tracked = d.ranges[Today].value
	}
	if tracked != nil {
		for _, device := range tracked.Devices {
			labels.Devices[device.ID] = device.Label
		}
	}
	return labels
}

// Usage answers the Usage page's query.
func (d *Deck) Usage(q Query) (UsageView, error) {
	c := d.sessions(q.Range)
	d.mu.Lock()
	defer d.mu.Unlock()
	if c.value == nil {
		return UsageView{}, fmt.Errorf("Usage hasn't loaded yet.")
	}
	if d.names == nil {
		if d.namesErr != nil {
			return UsageView{}, d.namesErr
		}
		return UsageView{}, fmt.Errorf("hex's thread names haven't loaded yet.")
	}
	key := string(encode(q))
	view := d.views[key]
	if view == nil {
		if c.parts == nil || c.names != d.names {
			c.parts, c.names = nil, d.names
			for _, s := range c.value.Sessions {
				c.parts = append(c.parts, partsOf(s, d.names)...)
			}
		}
		if len(d.views) > 64 {
			d.views = map[string]*UsageView{}
		}
		view = usageView(c.value, c.parts, d.labelsLocked(), q)
		d.views[key] = view
	}
	return withLive(view, d.live.order), nil
}

// SessionUsage answers token-tracker's GetSessionUsage for one range.
func (d *Deck) SessionUsage(r Range) *SessionUsage {
	return d.sessions(r).value
}

func (d *Deck) sessionLabel(id string) *string {
	if s, ok := d.live.sessions[id]; ok {
		return ptr(s.Label)
	}
	if d.names == nil {
		return nil
	}
	var record *Session
	if tracked := d.ranges[Week].value; tracked != nil {
		for _, s := range tracked.Sessions {
			if s.SessionID == id {
				record = s
				break
			}
		}
	}
	ref := sessionRef{sessionID: id}
	if record != nil {
		ref = sessionRef{sessionID: id, title: record.Title, project: record.Project, repository: record.Repository}
	}
	who := whoOf(d.names, ref)
	if who.Kind == "project" && record == nil {
		return nil
	}
	label, _ := labelOf(who)
	return &label
}

// Accounts answers the Accounts page.
func (d *Deck) Accounts() (AccountsView, error) {
	if d.opts.Pool == nil {
		return AccountsView{}, fmt.Errorf("The account pool isn't running.")
	}
	readings, _ := d.readings.read()
	bindings, _ := d.bindings.read()
	now := time.Now()
	view := AccountsView{History: historyOf(readings, now), Moves: movesOf(bindings, now)}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := range view.Moves {
		view.Moves[i].Label = d.sessionLabel(view.Moves[i].Session)
	}
	seats := 0
	if d.pool != nil {
		seats = len(d.pool.seats)
	}
	view.Hosts = []Host{{Host: "server", Seats: seats}}
	return view, nil
}

// SetPolicy previews proposed cutoffs, or saves them when the config is still at revision.
func (d *Deck) SetPolicy(proposed Policy, revision string, save bool, authorization string) (PolicyResult, error) {
	if err := validPolicy(proposed); err != nil {
		return PolicyResult{}, err
	}
	if d.opts.Pool == nil {
		return PolicyResult{}, fmt.Errorf("The account pool isn't running.")
	}
	if !save {
		snap := d.opts.Pool(&config.PoolConfig{SessionCutoff: proposed.SessionCutoff, WeeklyCutoff: proposed.WeeklyCutoff, AgentsPerAccount: proposed.AgentsPerAccount})
		return PolicyResult{Pool: poolOf(snap, revision)}, nil
	}
	body, _ := json.Marshal(map[string]any{"session-cutoff": proposed.SessionCutoff, "weekly-cutoff": proposed.WeeklyCutoff, "agents-per-account": proposed.AgentsPerAccount})
	request := httptest.NewRequest(http.MethodPatch, "/v8/management/config/routing/pool", strings.NewReader(string(body)))
	request.RemoteAddr = "127.0.0.1:0"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", revision)
	request.Header.Set("Authorization", authorization)
	response := httptest.NewRecorder()
	d.opts.Engine.ServeHTTP(response, request)
	d.refreshPool()
	d.publish()
	d.mu.Lock()
	pool := d.pool.pool
	d.mu.Unlock()
	switch response.Code {
	case http.StatusOK:
		// The config reloads behind the save, so rank under what was saved.
		saved := poolOf(d.opts.Pool(&config.PoolConfig{SessionCutoff: proposed.SessionCutoff, WeeklyCutoff: proposed.WeeklyCutoff, AgentsPerAccount: proposed.AgentsPerAccount}), d.revision())
		return PolicyResult{Pool: saved, Saved: true}, nil
	case http.StatusPreconditionFailed:
		return PolicyResult{Pool: pool, Conflict: true}, nil
	}
	return PolicyResult{}, fmt.Errorf("The account pool refused the policy (%d).", response.Code)
}

// Register serves the views under group: usage, accounts, accounts/policy and events.
func (d *Deck) Register(group *gin.RouterGroup) {
	reply := func(c *gin.Context, value any, err error) {
		c.Header("Cache-Control", "no-store")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, value)
	}
	group.GET("/usage", func(c *gin.Context) {
		view, err := d.Usage(ParseQuery(c.Request.URL.Query()))
		reply(c, view, err)
	})
	group.GET("/sessions", func(c *gin.Context) {
		reply(c, d.SessionUsage(pick(c.Query("range"), Ranges, Week)), nil)
	})
	group.GET("/accounts", func(c *gin.Context) {
		view, err := d.Accounts()
		reply(c, view, err)
	})
	group.POST("/accounts/policy", func(c *gin.Context) {
		var body struct {
			Policy struct {
				SessionCutoff    float64 `json:"sessionCutoff"`
				WeeklyCutoff     float64 `json:"weeklyCutoff"`
				AgentsPerAccount float64 `json:"agentsPerAccount"`
			} `json:"policy"`
			Revision any  `json:"revision"`
			Save     bool `json:"save"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			reply(c, nil, err)
			return
		}
		if agents := body.Policy.AgentsPerAccount; agents != math.Trunc(agents) {
			reply(c, nil, errAgents)
			return
		}
		revision := ""
		if body.Revision != nil {
			revision = fmt.Sprint(body.Revision)
		}
		policy := Policy{SessionCutoff: body.Policy.SessionCutoff, WeeklyCutoff: body.Policy.WeeklyCutoff, AgentsPerAccount: int(body.Policy.AgentsPerAccount)}
		result, err := d.SetPolicy(policy, revision, body.Save, c.GetHeader("Authorization"))
		reply(c, result, err)
	})
	group.GET("/events", d.serveEvents)
}

func (d *Deck) serveEvents(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	client := d.stream.subscribe()
	defer d.stream.unsubscribe(client)
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-d.ctx.Done():
			return
		case data, ok := <-client:
			if !ok {
				return
			}
			if _, err := c.Writer.Write(data); err != nil {
				return
			}
			c.Writer.Flush()
		}
	}
}
