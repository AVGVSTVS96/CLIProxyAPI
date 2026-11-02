package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// overviewFresh is how long the Executor overview is served before a request refreshes it behind itself.
const overviewFresh = 30 * time.Second

// ExecutorApp is an Executor app with its deployment, local source and linked accounts.
type ExecutorApp struct {
	ID            string            `json:"id"`
	Slug          string            `json:"slug"`
	Name          string            `json:"name"`
	ActiveVersion *string           `json:"activeVersion"`
	DeployedAt    *string           `json:"deployedAt"`
	Versions      int               `json:"versions"`
	Local         bool              `json:"local"`
	Pinned        *string           `json:"pinned"`
	Requirements  []string          `json:"requirements"`
	Accounts      []ExecutorAccount `json:"accounts"`
}

// ExecutorAccount is an account linked to one of an app's requirements.
type ExecutorAccount struct {
	Requirement string  `json:"requirement"`
	Label       string  `json:"label"`
	Health      *string `json:"health"`
}

// ExecutorOverview is the framework release the host runs and every app.
type ExecutorOverview struct {
	Framework *string       `json:"framework"`
	Apps      []ExecutorApp `json:"apps"`
	Console   string        `json:"console"`
}

// SourceFile is one app file as deployed and as saved locally.
type SourceFile struct {
	Path     string  `json:"path"`
	Deployed *string `json:"deployed"`
	Local    *string `json:"local"`
}

type executor struct {
	ctx      context.Context
	api      string
	token    string
	apps     string
	client   *http.Client
	overview *cached[ExecutorOverview]

	mu  sync.Mutex
	org string
}

func newExecutor(ctx context.Context, api, token, apps string, changed func()) *executor {
	e := &executor{ctx: ctx, api: api, token: token, apps: apps, client: &http.Client{Timeout: 30 * time.Second}}
	e.overview = &cached[ExecutorOverview]{ctx: ctx, load: e.load, changed: changed}
	return e
}

func (e *executor) call(path, method string, body any, into any) error {
	token, err := os.ReadFile(e.token)
	if err != nil {
		return fmt.Errorf("can't read the Executor token: %w", err)
	}
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(e.ctx, method, e.api+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cliproxyapi-workspace")
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var problem struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &problem) == nil && problem.Message != "" {
			return errors.New(problem.Message)
		}
		return fmt.Errorf("Executor answered %d", resp.StatusCode)
	}
	return json.Unmarshal(data, into)
}

// scoped calls an organization's API; the organization is looked up once it answers.
func (e *executor) scoped(path, method string, body any, into any) error {
	e.mu.Lock()
	org := e.org
	e.mu.Unlock()
	if org == "" {
		var context struct {
			Organization string `json:"organization"`
		}
		if err := e.call("/api/context", http.MethodGet, nil, &context); err != nil {
			return err
		}
		org = context.Organization
		e.mu.Lock()
		e.org = org
		e.mu.Unlock()
	}
	return e.call("/api/organizations/"+url.PathEscape(org)+path, method, body, into)
}

type inventoryApp struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	ActiveVersion string `json:"activeVersion"`
	Requirements  struct {
		Accounts map[string]json.RawMessage `json:"accounts"`
	} `json:"requirements"`
}

func (e *executor) inventory() ([]inventoryApp, error) {
	var answer struct {
		Apps []inventoryApp `json:"apps"`
	}
	err := e.scoped("/inventory", http.MethodGet, nil, &answer)
	return answer.Apps, err
}

func (e *executor) app(slug string) (inventoryApp, error) {
	apps, err := e.inventory()
	if err != nil {
		return inventoryApp{}, err
	}
	for _, app := range apps {
		if app.Slug == slug {
			return app, nil
		}
	}
	return inventoryApp{}, notFound("No Executor app %s", slug)
}

// localDir is an app's folder of local sources.
func (e *executor) localDir(slug string) (string, error) {
	if slug == "" || slug == "." || slug == ".." || slug != filepath.Base(slug) {
		return "", notFound("No Executor app %s", slug)
	}
	return filepath.Join(e.apps, slug), nil
}

func (e *executor) load() (ExecutorOverview, error) {
	apps, err := e.inventory()
	if err != nil {
		return ExecutorOverview{}, err
	}
	var release struct {
		Version string `json:"version"`
	}
	overview := ExecutorOverview{Apps: make([]ExecutorApp, len(apps)), Console: e.api}
	var wg sync.WaitGroup
	wg.Go(func() {
		if e.scoped("/framework/release", http.MethodGet, nil, &release) == nil {
			overview.Framework = text(release.Version)
		}
	})
	for i, app := range apps {
		wg.Go(func() { overview.Apps[i] = e.describe(app) })
	}
	wg.Wait()
	return overview, nil
}

func (e *executor) describe(app inventoryApp) ExecutorApp {
	var versions []struct {
		ID        string `json:"id"`
		CreatedAt string `json:"createdAt"`
	}
	var linked []struct {
		Requirement string `json:"requirement"`
		Account     struct {
			Label string `json:"label"`
		} `json:"account"`
		Health struct {
			Info struct {
				Email string `json:"email"`
			} `json:"info"`
			Apps []struct {
				Check struct {
					Status string `json:"status"`
				} `json:"check"`
			} `json:"apps"`
		} `json:"health"`
	}
	_ = e.scoped("/apps/"+url.PathEscape(app.ID)+"/versions", http.MethodGet, nil, &versions)
	_ = e.scoped("/apps/"+url.PathEscape(app.ID)+"/accounts/user", http.MethodGet, nil, &linked)

	out := ExecutorApp{ID: app.ID, Slug: app.Slug, Name: app.Name, ActiveVersion: text(app.ActiveVersion), Versions: len(versions),
		Requirements: []string{}, Accounts: []ExecutorAccount{}}
	for _, v := range versions {
		if v.ID == app.ActiveVersion {
			out.DeployedAt = text(v.CreatedAt)
		}
	}
	for requirement := range app.Requirements.Accounts {
		out.Requirements = append(out.Requirements, requirement)
	}
	slices.Sort(out.Requirements)
	for _, link := range linked {
		label := link.Health.Info.Email
		if label == "" {
			label = link.Account.Label
		}
		account := ExecutorAccount{Requirement: link.Requirement, Label: label}
		if len(link.Health.Apps) > 0 {
			account.Health = text(link.Health.Apps[0].Check.Status)
		}
		out.Accounts = append(out.Accounts, account)
	}
	if dir, err := e.localDir(app.Slug); err == nil && exists(filepath.Join(dir, "index.ts")) {
		out.Local = true
		var pkg struct {
			Dependencies map[string]string `json:"dependencies"`
		}
		readJSON(filepath.Join(dir, "package.json"), &pkg)
		out.Pinned = text(pkg.Dependencies["apps"])
	}
	return out
}

// Source lists an app's deployed files, plus index.ts and package.json, each deployed and local.
func (e *executor) Source(slug string) ([]SourceFile, error) {
	dir, err := e.localDir(slug)
	if err != nil {
		return nil, err
	}
	app, err := e.app(slug)
	if err != nil {
		return nil, err
	}
	var deployed struct {
		Files []struct {
			Path    string  `json:"path"`
			Content *string `json:"content"`
		} `json:"files"`
	}
	if err := e.scoped("/apps/"+url.PathEscape(app.ID)+"/source", http.MethodGet, nil, &deployed); err != nil {
		return nil, err
	}
	contents := map[string]*string{}
	paths := []string{"index.ts", "package.json"}
	for _, f := range deployed.Files {
		contents[f.Path] = f.Content
		paths = append(paths, f.Path)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	root, _ := os.OpenRoot(dir)
	if root != nil {
		defer func() { _ = root.Close() }()
	}
	files := make([]SourceFile, 0, len(paths))
	for _, path := range paths {
		file := SourceFile{Path: path, Deployed: contents[path]}
		if root != nil {
			if data, err := root.ReadFile(filepath.FromSlash(path)); err == nil {
				local := string(data)
				file.Local = &local
			}
		}
		files = append(files, file)
	}
	return files, nil
}

// Save writes a file into an app's local source.
func (e *executor) Save(slug, path string, contents []byte) error {
	dir, err := e.localDir(slug)
	if err != nil {
		return err
	}
	rel, err := clean(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if parent := filepath.Dir(rel); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return refuse("Can't save %s: %v", path, err)
		}
	}
	if err := root.WriteFile(rel, contents, 0o644); err != nil {
		return refuse("Can't save %s: %v", path, err)
	}
	return nil
}

// Deploy uploads an app's local files as a new version.
func (e *executor) Deploy(slug string) (*string, int, error) {
	app, err := e.app(slug)
	if err != nil {
		return nil, 0, err
	}
	files, err := e.Source(slug)
	if err != nil {
		return nil, 0, err
	}
	type upload struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	uploads := []upload{}
	for _, f := range files {
		if f.Local != nil {
			uploads = append(uploads, upload{Path: f.Path, Content: *f.Local})
		}
	}
	var result struct {
		Version *struct {
			ID string `json:"id"`
		} `json:"version"`
	}
	if err := e.scoped("/apps/"+url.PathEscape(app.ID)+"/deploy", http.MethodPost, map[string]any{"files": uploads}, &result); err != nil {
		return nil, 0, err
	}
	e.overview.refresh()
	if result.Version == nil {
		return nil, len(uploads), nil
	}
	return &result.Version.ID, len(uploads), nil
}

// Connect starts linking an account to one of an app's requirements and returns where to sign in.
func (e *executor) Connect(slug, requirement string) (string, error) {
	app, err := e.app(slug)
	if err != nil {
		return "", err
	}
	if _, ok := app.Requirements.Accounts[requirement]; !ok {
		return "", notFound("%s has no account requirement %s", slug, requirement)
	}
	var answer struct {
		URL string `json:"url"`
	}
	path := "/apps/" + url.PathEscape(app.ID) + "/accounts/user/" + url.PathEscape(requirement) + "/connections"
	if err := e.scoped(path, http.MethodPost, map[string]any{}, &answer); err != nil {
		return "", err
	}
	e.overview.refresh()
	return answer.URL, nil
}

// cached holds the last value a load returned, a failed load keeping it. One
// load runs at a time; a refresh asked for during a load runs once after it.
type cached[T any] struct {
	ctx     context.Context
	load    func() (T, error)
	changed func()

	mu      sync.Mutex
	value   *T
	encoded []byte
	at      time.Time
	err     error
	loading chan struct{}
	again   bool
}

// get returns the cached value, refreshing it behind the caller once stale; the first call waits for a value.
func (c *cached[T]) get() (T, error) {
	c.mu.Lock()
	if c.value != nil {
		value := *c.value
		if time.Since(c.at) > overviewFresh {
			c.startLocked()
		}
		c.mu.Unlock()
		return value, nil
	}
	loading := c.startLocked()
	c.mu.Unlock()
	select {
	case <-loading:
	case <-c.ctx.Done():
		var zero T
		return zero, c.ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value == nil {
		var zero T
		if c.err == nil {
			c.err = errors.New("Executor didn't answer")
		}
		return zero, c.err
	}
	return *c.value, nil
}

func (c *cached[T]) refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loading != nil {
		c.again = true
		return
	}
	c.startLocked()
}

func (c *cached[T]) startLocked() chan struct{} {
	if c.loading == nil {
		c.loading = make(chan struct{})
		go c.run(c.loading)
	}
	return c.loading
}

func (c *cached[T]) run(done chan struct{}) {
	for {
		value, err := c.load()
		var encoded []byte
		if err == nil {
			encoded, err = json.Marshal(value)
		}
		c.mu.Lock()
		changed := false
		if err != nil {
			c.err = err
			log.Warnf("workspace: executor: %v", err)
		} else {
			changed = c.value != nil && !bytes.Equal(encoded, c.encoded)
			c.value, c.encoded, c.at, c.err = &value, encoded, time.Now(), nil
		}
		again := c.again && c.ctx.Err() == nil
		c.again = false
		if !again {
			c.loading = nil
			close(done)
		}
		c.mu.Unlock()
		if changed {
			c.changed()
		}
		if !again {
			return
		}
	}
}
