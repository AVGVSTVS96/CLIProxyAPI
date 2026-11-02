// Package workspace serves the dashboard's Files, Plugins and Executor pages:
// configured git repos browsed read-only and edited through worktrees, drafts,
// hex channels, hex mods and Claude Code plugins, and Executor apps.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// maxBytes is the largest file read, written or counted.
const maxBytes = 2_000_000

// Options are what the workspace reads and how it announces changes.
type Options struct {
	Config config.Config
	// Stale names resources the dashboard should refetch, like "plugins" or "repo:hex".
	Stale func(tags ...string)
}

// Service serves the pages. Its settings follow config reloads.
type Service struct {
	ctx    context.Context
	cancel context.CancelFunc
	stale  func(tags ...string)
	lines  lineCache
	watch  *watcher

	mu       sync.RWMutex
	settings settings
	executor *executor
	locks    map[string]*sync.Mutex
}

type repo struct {
	ID     string
	Name   string
	Path   string
	About  string
	Review bool
}

type settings struct {
	hex, claude   string
	repos         []repo
	worktrees     string
	drafts        string
	draftTargets  map[string]string
	executorApps  string
	executorToken string
	executorAPI   string
}

func settingsOf(cfg config.Config) settings {
	w := cfg.Workspace
	s := settings{
		hex:           expand(cfg.Usage.Hex, "~/hex"),
		claude:        expand(cfg.Usage.Claude, "~/.claude"),
		worktrees:     expand(w.Worktrees, "~/.local/share/cliproxyapi/worktrees"),
		drafts:        expand(w.Drafts.Dir, ""),
		draftTargets:  w.Drafts.Targets,
		executorApps:  expand(w.Executor.Apps, ""),
		executorToken: expand(w.Executor.Token, ""),
		executorAPI:   strings.TrimRight(w.Executor.API, "/"),
	}
	seen := map[string]bool{}
	for _, r := range w.Repos {
		path := expand(r.Path, "")
		switch {
		case r.ID == "" || strings.ContainsAny(r.ID, "~/") || seen[r.ID]:
			log.Warnf("workspace: skipping repo %q: its id must be unique, without ~ or /", r.ID)
			continue
		case path == "" || !exists(filepath.Join(path, ".git")):
			log.Warnf("workspace: skipping repo %s: no git checkout at %q", r.ID, path)
			continue
		}
		seen[r.ID] = true
		name := r.Name
		if name == "" {
			name = r.ID
		}
		s.repos = append(s.repos, repo{ID: r.ID, Name: name, Path: path, About: r.About, Review: r.Review})
	}
	return s
}

// New reads the settings, starts watching every repo, worktree and settings file, and loads Executor.
func New(opts Options) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	stale := opts.Stale
	if stale == nil {
		stale = func(...string) {}
	}
	w := &Service{ctx: ctx, cancel: cancel, stale: stale, locks: map[string]*sync.Mutex{}}
	w.watch = newWatcher(ctx, stale)
	w.SetConfig(opts.Config)
	return w
}

// SetConfig applies a reloaded config.
func (w *Service) SetConfig(cfg config.Config) {
	next := settingsOf(cfg)
	w.mu.Lock()
	previous := w.settings
	w.settings = next
	executorChanged := previous.executorAPI != next.executorAPI ||
		previous.executorToken != next.executorToken || previous.executorApps != next.executorApps
	if executorChanged {
		w.executor = nil
		if next.executorAPI != "" {
			w.executor = newExecutor(w.ctx, next.executorAPI, next.executorToken, next.executorApps, func() { w.stale("executor") })
		}
	}
	executor := w.executor
	w.mu.Unlock()
	if executorChanged && executor != nil {
		executor.overview.refresh()
	}
	w.resync()
	if !reflect.DeepEqual(previous, next) {
		w.stale("repos", "drafts", "plugins", "executor")
	}
}

// Close stops watching.
func (w *Service) Close() { w.cancel() }

func (w *Service) current() settings {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.settings
}

// lock serializes the changes to one repo, so two never fight over its index.
func (w *Service) lock(repoID string) func() {
	w.mu.Lock()
	l := w.locks[repoID]
	if l == nil {
		l = &sync.Mutex{}
		w.locks[repoID] = l
	}
	w.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Error is an answer the dashboard shows as is: a refused action or a missing thing.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func refuse(format string, args ...any) error {
	return &Error{Status: http.StatusConflict, Message: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &Error{Status: http.StatusNotFound, Message: fmt.Sprintf(format, args...)}
}

func statusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return http.StatusInternalServerError
}

func expand(path, fallback string) string {
	if path == "" {
		path = fallback
	}
	if path == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(path, "~"); ok && (rest == "" || rest[0] == '/') {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, rest)
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
