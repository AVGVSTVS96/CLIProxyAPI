package usage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Sources says where a machine's agent logs live. Empty fields use the same defaults
// and environment variables (CLAUDE_CONFIG_DIR, CODEX_HOME, PI_CODING_AGENT_DIR, GROK_HOME,
// MUSE_HOME) token-tracker uses.
type Sources struct {
	Home       string
	Claude     []string
	Codex      []string
	Pi         []string
	Grok       []string
	Muse       []string
	T3Data     string
	T3URL      string
	DeviceID   string
	MaxWorkers int
}

type source struct {
	harness string
	path    string
}

type cachedFile struct {
	signature string
	parsed    *parsedFile
}

// Collector reads one machine's logs. Each Collect re-parses only files whose signature
// changed since the last one, keeping what deduplication needs from each file in memory.
type Collector struct {
	opts    Sources
	sources []source
	pricing *Pricing

	mu    sync.Mutex
	files map[string]cachedFile
	grok  *grokCache
}

// Collection is one pass's result: every current record, priced, sorted by time and id,
// and the ids proved to be replays that earlier passes may have reported.
type Collection struct {
	Events       []Event
	RetractedIDs []string
	Warnings     []string
}

func envDirs(name string) []string {
	var dirs []string
	for _, value := range strings.Split(os.Getenv(name), ",") {
		if value = strings.TrimSpace(value); value != "" {
			dirs = append(dirs, value)
		}
	}
	return dirs
}

// NewCollector resolves the source directories once; directories created later are
// still found because each collection walks them again.
func NewCollector(opts Sources, pricing *Pricing) *Collector {
	if opts.Home == "" {
		opts.Home, _ = os.UserHomeDir()
	}
	home := opts.Home
	expand := func(root string) string {
		switch {
		case root == "~":
			root = home
		case strings.HasPrefix(root, "~/"):
			root = filepath.Join(home, root[2:])
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return root
		}
		return abs
	}
	pick := func(configured []string, env string, defaults ...string) []string {
		if len(configured) > 0 {
			return configured
		}
		if dirs := envDirs(env); len(dirs) > 0 {
			return dirs
		}
		return defaults
	}
	claude := opts.Claude
	if len(claude) == 0 {
		claude = envDirs("CLAUDE_CONFIG_DIR")
		if len(claude) == 0 && strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")) == "" {
			claude = []string{filepath.Join(home, ".claude")}
			xdg := os.Getenv("XDG_CONFIG_HOME")
			if xdg == "" {
				xdg = filepath.Join(home, ".config")
			}
			if info, err := os.Stat(filepath.Join(xdg, "claude", "projects")); err == nil && info.IsDir() {
				claude = append(claude, filepath.Join(xdg, "claude"))
			}
		}
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	c := &Collector{opts: opts, pricing: pricing, files: map[string]cachedFile{}, grok: newGrokCache()}
	seen := map[string]bool{}
	add := func(harness, root string) {
		expanded := expand(root)
		if key := harness + ":" + expanded; !seen[key] {
			seen[key] = true
			c.sources = append(c.sources, source{harness, expanded})
		}
	}
	under := func(root, leaf string) string {
		if filepath.Base(strings.TrimSuffix(root, "/")) == leaf {
			return root
		}
		return filepath.Join(root, leaf)
	}
	for _, root := range claude {
		add("claude", under(root, "projects"))
	}
	for _, root := range pick(opts.Codex, "CODEX_HOME", filepath.Join(home, ".codex")) {
		if base := filepath.Base(strings.TrimSuffix(root, "/")); base == "sessions" || base == "archived_sessions" {
			add("codex", root)
		} else {
			add("codex", filepath.Join(root, "sessions"))
			add("codex", filepath.Join(root, "archived_sessions"))
		}
	}
	for _, root := range pick(opts.Pi, "PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent")) {
		add("pi", under(root, "sessions"))
	}
	for _, root := range pick(opts.Grok, "GROK_HOME", filepath.Join(home, ".grok")) {
		add("grok", under(root, "sessions"))
	}
	for _, root := range pick(opts.Muse, "MUSE_HOME", filepath.Join(dataHome, "muse")) {
		add("muse", under(root, "sessions"))
	}
	if c.opts.T3Data == "" {
		c.opts.T3Data = filepath.Join(home, ".t3", "userdata")
	}
	c.opts.T3Data = expand(c.opts.T3Data)
	if c.opts.T3URL == "" {
		c.opts.T3URL = "https://app.t3.codes"
	}
	return c
}

// Roots are the directories whose changes call for a new collection.
func (c *Collector) Roots() []string {
	roots := make([]string, 0, len(c.sources))
	for _, s := range c.sources {
		roots = append(roots, s.path)
	}
	return roots
}

func jsonlName(name string) bool {
	ext := filepath.Ext(name)
	return ext != name && strings.EqualFold(ext, ".jsonl")
}

// listLogs walks a source root like token-tracker: the root may be a symlink, nested
// symlinks are skipped, and paths come back sorted.
func listLogs(root string) (paths []string, problems int, err error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return nil, 0, errors.New("usage source is not a readable directory")
	}
	pending := []string{real}
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, err := os.ReadDir(dir)
		if err != nil {
			problems++
			continue
		}
		for _, entry := range entries {
			file := filepath.Join(dir, entry.Name())
			switch t := entry.Type(); {
			case t&fs.ModeSymlink != 0:
			case t.IsDir():
				pending = append(pending, file)
			case t.IsRegular() && jsonlName(entry.Name()):
				paths = append(paths, file)
			}
		}
	}
	sort.Strings(paths)
	return paths, problems, nil
}

func parserFor(harness string) func(r *os.File, file string) (*parsedFile, error) {
	switch harness {
	case "claude":
		return func(r *os.File, file string) (*parsedFile, error) { return parseClaude(r, file) }
	case "codex":
		return func(r *os.File, file string) (*parsedFile, error) { return parseCodex(r, file) }
	case "muse":
		return func(r *os.File, file string) (*parsedFile, error) { return parseMuse(r, file) }
	}
	return func(r *os.File, file string) (*parsedFile, error) { return parsePi(r, file) }
}

type logFile struct {
	harness, path, signature string
}

// parseFile reads one log, returning nil when it could not be read whole.
func parseFile(f logFile) *parsedFile {
	file, err := os.Open(f.path)
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	parsed, err := parserFor(f.harness)(file, f.path)
	if err != nil {
		return nil
	}
	return parsed
}

// scan lists every log and returns its parsed contents in token-tracker's order,
// parsing only what changed.
func (c *Collector) scan() ([]*parsedFile, []string) {
	var warnings []string
	var order []logFile
	for _, s := range c.sources {
		if s.harness == "grok" {
			continue
		}
		paths, problems, err := listLogs(s.path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				warnings = append(warnings, s.harness+": Usage directory could not be read.")
			}
			continue
		}
		if problems > 0 {
			warnings = append(warnings, s.harness+": some directories could not be read.")
		}
		for _, path := range paths {
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			order = append(order, logFile{s.harness, path, fileSignature(info)})
		}
	}
	c.mu.Lock()
	previous := c.files
	c.mu.Unlock()
	var stale []int
	parsed := make([]*parsedFile, len(order))
	for i, f := range order {
		if cached, ok := previous[f.harness+":"+f.path]; ok && cached.signature == f.signature {
			parsed[i] = cached.parsed
		} else {
			stale = append(stale, i)
		}
	}
	workers := c.opts.MaxWorkers
	if workers <= 0 {
		workers = max(1, runtime.GOMAXPROCS(0)/2)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				parsed[i] = parseFile(order[i])
			}
		}()
	}
	for _, i := range stale {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	next := make(map[string]cachedFile, len(order))
	files := make([]*parsedFile, 0, len(order))
	for i, f := range order {
		if parsed[i] == nil {
			continue
		}
		next[f.harness+":"+f.path] = cachedFile{f.signature, parsed[i]}
		files = append(files, parsed[i])
	}
	c.mu.Lock()
	c.files = next
	c.mu.Unlock()
	return files, warnings
}

// Collect reads the machine's logs and returns its records as token-tracker's
// collectUsage does.
func (c *Collector) Collect(ctx context.Context) Collection {
	files, warnings := c.scan()
	var grokSources []string
	for _, s := range c.sources {
		if s.harness == "grok" {
			grokSources = append(grokSources, s.path)
		}
	}
	grokFiles, grokWarnings := c.grok.collect(grokSources)
	files = append(files, grokFiles...)
	warnings = append(warnings, grokWarnings...)

	titles := map[string]string{}
	for _, file := range files {
		if file.SessionTitle != "" {
			for _, entry := range file.Events {
				titles[entry.Event.SessionID] = file.SessionTitle
			}
		}
	}
	views := newViews(files)
	entries, retained := deduplicate(views)
	var retracted []string
	seenRetracted := map[string]bool{}
	retract := func(id string) {
		if _, kept := retained[id]; !kept && !seenRetracted[id] {
			seenRetracted[id] = true
			retracted = append(retracted, id)
		}
	}
	for _, file := range files {
		for _, entry := range file.Events {
			retract(entry.Event.ID)
			if entry.LegacyEventID != "" {
				retract(entry.LegacyEventID)
			}
		}
	}
	for _, view := range views {
		for _, id := range view.Retracted {
			retract(id)
		}
	}
	sort.Strings(retracted)

	resolver := newRepositoryResolver(c.opts.Home)
	repositories := map[string]*string{}
	repository := func(path string) *string {
		if r, ok := repositories[path]; ok {
			return r
		}
		r := resolver.resolve(path)
		repositories[path] = r
		return r
	}
	isRemote := func(r *string) bool { return r != nil && !strings.HasPrefix(*r, "local:") }
	type sessionKey struct{ harness, session string }
	t3 := map[sessionKey]t3Metadata{}
	for key, metadata := range readT3Metadata(ctx, c.opts.T3Data, c.opts.T3URL) {
		harness, session, _ := strings.Cut(key, ":")
		t3[sessionKey{harness, session}] = metadata
	}
	sessionRepositories := map[sessionKey]*string{}
	attempted := map[sessionKey]bool{}
	for _, entry := range entries {
		key := sessionKey{entry.Event.Harness, entry.Event.SessionID}
		metadata, ok := t3[key]
		if !ok || attempted[key] || isRemote(repository(entry.Event.Project)) {
			continue
		}
		attempted[key] = true
		for _, cwd := range metadata.RepositoryPaths {
			if attached := repository(cwd); isRemote(attached) {
				sessionRepositories[key] = attached
				break
			}
		}
	}

	policy := c.pricing.Policy()
	unknown := map[string]bool{}
	events := make([]Event, 0, len(entries))
	for _, entry := range entries {
		e := entry.Event
		key := sessionKey{e.Harness, e.SessionID}
		metadata, hasMetadata := t3[key]
		if title, ok := titles[e.SessionID]; ok {
			e.SessionTitle = title
		} else if hasMetadata {
			e.SessionTitle = metadata.SessionTitle
		}
		if hasMetadata {
			e.ProjectName = metadata.ProjectName
			e.T3ThreadID = metadata.T3ThreadID
			e.T3ThreadURL = metadata.T3ThreadURL
		}
		cost, known := policy.estimateCost(&e, entry.CacheWrite1h, entry.Tier)
		if !known {
			unknown[e.Model] = true
		}
		e.CostUSD, e.CostKnown = cost, known
		e.RawModel = e.Model
		e.Model = policy.DisplayModel(e.Model)
		tier := entry.Tier
		e.ServiceTier = &tier
		write1h := entry.CacheWrite1h
		e.CacheWrite1hTokens = &write1h
		r := repository(e.Project)
		switch {
		case isRemote(r):
			e.Repository = r
		case sessionRepositories[key] != nil:
			e.Repository = sessionRepositories[key]
		default:
			e.Repository = r
		}
		if c.opts.DeviceID != "" {
			e.DeviceID = c.opts.DeviceID
		}
		events = append(events, e)
	}
	sortEvents(events)
	if len(unknown) > 0 {
		models := make([]string, 0, len(unknown))
		for model := range unknown {
			models = append(models, model)
		}
		sort.Strings(models)
		warnings = append(warnings, "No reliable API price for: "+strings.Join(models, ", ")+
			". These tokens are included, but their costs are unavailable.")
	}
	return Collection{Events: events, RetractedIDs: retracted, Warnings: warnings}
}

func sortEvents(events []Event) {
	slices.SortFunc(events, func(a, b Event) int {
		if c := strings.Compare(a.Timestamp, b.Timestamp); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
}
