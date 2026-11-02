package workspace

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
)

// settle is how long a burst of file events gathers before one stale event names everything it touched.
const settle = 150 * time.Millisecond

// target is a folder to watch and the stale tags each changed path in it names.
type target struct {
	dir       string
	recursive bool
	// skip leaves out a subfolder of a recursive target, by its path inside the target.
	skip func(rel string) bool
	tags func(rel string) []string
}

// watcher follows every target with one fsnotify watcher and sends their tags, a burst at a time.
type watcher struct {
	ctx    context.Context
	stale  func(tags ...string)
	notify *fsnotify.Watcher

	mu      sync.Mutex
	targets []target
	dirs    map[string]bool
	pending map[string]bool
	timer   *time.Timer
}

func newWatcher(ctx context.Context, stale func(tags ...string)) *watcher {
	w := &watcher{ctx: ctx, stale: stale, dirs: map[string]bool{}, pending: map[string]bool{}}
	notify, err := fsnotify.NewWatcher()
	if err != nil {
		log.Warnf("workspace: watch: %v; the dashboard won't hear about file changes", err)
		return w
	}
	w.notify = notify
	go w.loop()
	return w
}

// set replaces the targets, watching new folders and letting go of ones no target wants.
func (w *watcher) set(targets []target) {
	if w.notify == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.targets = targets
	wanted := map[string]bool{}
	for _, t := range targets {
		if !t.recursive {
			wanted[t.dir] = true
			continue
		}
		for _, dir := range folders(t, t.dir) {
			wanted[dir] = true
		}
	}
	for dir := range w.dirs {
		if !wanted[dir] {
			_ = w.notify.Remove(dir)
			delete(w.dirs, dir)
		}
	}
	for dir := range wanted {
		w.addLocked(dir)
	}
}

func (w *watcher) addLocked(dir string) {
	if w.dirs[dir] {
		return
	}
	if err := w.notify.Add(dir); err != nil {
		if !os.IsNotExist(err) {
			log.Debugf("workspace: watch %s: %v", dir, err)
		}
		return
	}
	w.dirs[dir] = true
}

// folders lists from, a folder in a recursive target, and every subfolder under it the target doesn't skip.
func folders(t target, from string) []string {
	var dirs []string
	_ = filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return nil
		}
		if rel, _ := filepath.Rel(t.dir, path); rel != "." && t.skip != nil && t.skip(filepath.ToSlash(rel)) {
			return filepath.SkipDir
		}
		dirs = append(dirs, path)
		return nil
	})
	return dirs
}

func (w *watcher) loop() {
	defer func() { _ = w.notify.Close() }()
	for {
		select {
		case <-w.ctx.Done():
			return
		case event, ok := <-w.notify.Events:
			if !ok {
				return
			}
			w.handle(event)
		case _, ok := <-w.notify.Errors:
			if !ok {
				return
			}
		}
	}
}

func (w *watcher) handle(event fsnotify.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, t := range w.targets {
		rel, err := filepath.Rel(t.dir, event.Name)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		if !t.recursive && strings.ContainsRune(rel, filepath.Separator) {
			continue
		}
		if t.recursive && event.Has(fsnotify.Create) {
			if info, err := os.Stat(event.Name); err == nil && info.IsDir() && (t.skip == nil || !t.skip(filepath.ToSlash(rel))) {
				for _, dir := range folders(t, event.Name) {
					w.addLocked(dir)
				}
			}
		}
		for _, tag := range t.tags(filepath.ToSlash(rel)) {
			w.pending[tag] = true
		}
	}
	if len(w.pending) > 0 && w.timer == nil {
		w.timer = time.AfterFunc(settle, w.flush)
	}
}

func (w *watcher) flush() {
	w.mu.Lock()
	tags := make([]string, 0, len(w.pending))
	for tag := range w.pending {
		tags = append(tags, tag)
	}
	w.pending, w.timer = map[string]bool{}, nil
	w.mu.Unlock()
	if len(tags) > 0 && w.ctx.Err() == nil {
		slices.Sort(tags)
		w.stale(tags...)
	}
}

// gitDir is a checkout's git folder, following a .git file to a linked one.
func gitDir(checkout string) string {
	dot := filepath.Join(checkout, ".git")
	data, err := os.ReadFile(dot)
	if err != nil {
		return dot
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return dot
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(checkout, dir)
	}
	return filepath.Clean(dir)
}

var gitState = []string{"index", "HEAD", "ORIG_HEAD", "COMMIT_EDITMSG", "packed-refs"}

// resync points the watcher at the settings files, the drafts, every repo's git state and every worktree's files.
func (w *Service) resync() {
	s := w.current()
	only := func(tag string, names ...string) func(string) []string {
		return func(rel string) []string {
			if slices.Contains(names, rel) {
				return []string{tag}
			}
			return nil
		}
	}
	targets := []target{
		{dir: s.claude, tags: only("plugins", "settings.json")},
		{dir: filepath.Join(s.claude, "plugins"), tags: only("plugins", "installed_plugins.json", "known_marketplaces.json")},
	}
	if s.drafts != "" {
		targets = append(targets, target{dir: s.drafts, tags: func(rel string) []string {
			if strings.HasPrefix(rel, ".") {
				return nil
			}
			return []string{"drafts"}
		}})
	}
	for _, r := range s.repos {
		repoTag := "repo:" + r.ID
		dir := gitDir(r.Path)
		targets = append(targets,
			target{dir: dir, tags: only(repoTag, gitState...)},
			target{dir: filepath.Join(dir, "refs"), recursive: true, skip: func(rel string) bool { return rel == "t3" },
				tags: func(rel string) []string {
					if strings.HasSuffix(rel, ".lock") || strings.HasPrefix(rel, "t3/") {
						return nil
					}
					// A worktree's branch moving changes how far it is from its base.
					if name, ok := strings.CutPrefix(rel, "heads/"+branchPrefix); ok {
						return []string{repoTag, "ws:" + r.ID + "~" + name}
					}
					return []string{repoTag}
				}})
		list, err := w.workspaces(r)
		if err != nil {
			continue
		}
		for _, ws := range list[1:] {
			tag := "ws:" + ws.ID
			targets = append(targets, target{dir: ws.Path, recursive: true,
				skip: func(rel string) bool { name := filepath.Base(rel); return name == ".git" || name == "node_modules" },
				tags: func(rel string) []string {
					if rel == ".git" {
						return nil
					}
					return []string{tag}
				}})
		}
	}
	w.watch.set(targets)
}
