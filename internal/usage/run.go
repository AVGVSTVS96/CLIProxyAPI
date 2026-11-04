package usage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

var shared atomic.Pointer[FileStore]

// Shared is the running server's store, or nil before Run has opened it.
func Shared() *FileStore { return shared.Load() }

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path[1:], "/"))
		}
	}
	return path
}

// identity returns this machine's device id and label from config, else StateDir/device-id
// (created once) and the hostname.
func identity(cfg config.UsageConfig, stateDir string) (string, string, error) {
	id := strings.TrimSpace(cfg.DeviceID)
	if id == "" {
		path := filepath.Join(stateDir, "device-id")
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
			id = strings.TrimSpace(string(data))
		} else {
			id = uuid.NewString()
			if err = writeFileAtomic(path, []byte(id+"\n")); err != nil {
				return "", "", err
			}
		}
	}
	label := strings.TrimSpace(cfg.DeviceLabel)
	if label == "" {
		label, _ = os.Hostname()
	}
	return id, label, nil
}

type machine struct {
	stateDir  string
	github    bool
	id, label string
	pricing   *Pricing
	collector *Collector
}

func openMachine(cfg config.UsageConfig) (*machine, error) {
	stateDir := expandHome(strings.TrimSpace(cfg.StateDir))
	if stateDir == "" {
		return nil, errors.New("usage.state-dir is not set")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	id, label, err := identity(cfg, stateDir)
	if err != nil {
		return nil, err
	}
	pricing := NewPricing(stateDir)
	collector := NewCollector(Sources{
		DeviceID: id,
		T3Data:   expandHome(cfg.T3DataDir), T3URL: cfg.T3URL,
	}, pricing)
	return &machine{stateDir: stateDir, github: cfg.GitHubActions, id: id, label: label, pricing: pricing, collector: collector}, nil
}

// Local is the server's usage: the store of every device's records, and the collector
// that keeps this machine's own records in it current.
type Local struct {
	*FileStore
	machine   *machine
	unlock    func()
	closeOnce sync.Once
}

// Open opens the store under cfg.StateDir with everything already saved in it. Run then
// keeps this machine's records current; the state directory stays locked until Close.
func Open(cfg config.UsageConfig) (*Local, error) {
	m, err := openMachine(cfg)
	if err != nil {
		return nil, err
	}
	unlock, err := lockState(m.stateDir)
	if err != nil {
		return nil, err
	}
	store, err := OpenStore(m.stateDir, m.id, m.label, m.pricing)
	if err != nil {
		unlock()
		return nil, err
	}
	shared.Store(store)
	return &Local{FileStore: store, machine: m, unlock: unlock}, nil
}

// Close releases the store after the collector and request dispatcher have stopped.
func (l *Local) Close() {
	l.closeOnce.Do(func() {
		shared.CompareAndSwap(l.FileStore, nil)
		l.mu.Lock()
		for _, device := range l.devices {
			if device.journal != nil {
				if err := device.journal.Sync(); err != nil {
					log.WithError(err).Error("usage: sync journal")
				}
				if err := device.journal.Close(); err != nil {
					log.WithError(err).Error("usage: close journal")
				}
				device.journal = nil
			}
		}
		l.mu.Unlock()
		l.unlock()
	})
}

// Run collects this machine's logs into the store, then follows them and refreshes the
// pricing catalog daily, until ctx ends. With usage.github-actions it also reads GitHub
// Actions records.
func (l *Local) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	m := l.machine
	workers.Add(1)
	go func() { defer workers.Done(); m.pricing.RunRefresh(ctx) }()
	if m.github {
		workers.Add(1)
		go func() { defer workers.Done(); newGitHubReader(m.stateDir, l.FileStore).run(ctx) }()
	}
	pass := func() {
		started := time.Now()
		collection := m.collector.Collect(ctx)
		result, err := l.Apply(Batch{Device: Device{ID: m.id, Label: m.label}, Events: collection.Events,
			DeletedIDs: collection.RetractedIDs}, false)
		if err != nil {
			log.WithError(err).Warn("usage: could not save this machine's records")
		}
		log.Debugf("usage: collected %d records in %s (%d new, %d updated, %d withdrawn)",
			len(collection.Events), time.Since(started).Round(time.Millisecond), result.Accepted, result.Updated, result.Deleted)
	}
	pass()
	debug.FreeOSMemory()
	log.Infof("usage: collecting %s's logs into %s", m.label, m.stateDir)
	return follow(ctx, m.collector.Roots(), pass)
}

// follow runs pass after files under roots change. A burst of writes shares one pass:
// it starts once the logs are quiet for a second, or five seconds into a steady stream.
// Roots that don't exist yet are picked up when their parent directory creates them.
func follow(ctx context.Context, roots []string, pass func()) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()
	watched := map[string]bool{}
	addTree := func(root string) {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.Type()&fs.ModeSymlink != 0 && path != root {
				return nil
			}
			if entry.IsDir() || path == root {
				if !watched[path] && watcher.Add(path) == nil {
					watched[path] = true
				}
			}
			return nil
		})
	}
	ensure := func() {
		for _, root := range roots {
			if info, err := os.Stat(root); err == nil && info.IsDir() {
				addTree(root)
				continue
			}
			for dir := filepath.Dir(root); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
				if info, err := os.Stat(dir); err == nil && info.IsDir() {
					if !watched[dir] && watcher.Add(dir) == nil {
						watched[dir] = true
					}
					break
				}
			}
		}
	}
	inRoots := func(path string) bool {
		for _, root := range roots {
			if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	ancestorOfRoot := func(path string) bool {
		for _, root := range roots {
			if strings.HasPrefix(root, path+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	ensure()
	var quiet, deadline <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-watcher.Errors:
			log.WithError(err).Debug("usage: file watch error")
		case event := <-watcher.Events:
			if event.Op == fsnotify.Chmod {
				continue
			}
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					switch {
					case inRoots(event.Name):
						addTree(event.Name)
					case ancestorOfRoot(event.Name):
						ensure()
					}
				}
			}
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				delete(watched, event.Name)
			}
			if !inRoots(event.Name) {
				continue
			}
			quiet = time.After(time.Second)
			if deadline == nil {
				deadline = time.After(5 * time.Second)
			}
		case <-quiet:
			quiet, deadline = nil, nil
			pass()
		case <-deadline:
			quiet, deadline = nil, nil
			pass()
		}
	}
}
