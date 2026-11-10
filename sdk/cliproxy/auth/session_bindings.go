package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// sessionBindingsFile holds the session-to-account bindings between two runs of the proxy,
// so a restart keeps each session on the account that holds its prompt cache.
const sessionBindingsFile = "session-bindings.json"

func (m *Manager) sessionCache() *SessionCache {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if affinity, ok := m.selector.(*SessionAffinitySelector); ok && affinity != nil {
		return affinity.cache
	}
	return nil
}

func sessionBindingsPath() string {
	claudePool.mu.Lock()
	defer claudePool.mu.Unlock()
	if claudePool.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(claudePool.cfg.StateDir, sessionBindingsFile)
}

// SaveSessionBindings writes the live session bindings to the pool's state directory.
func (m *Manager) SaveSessionBindings() (int, error) {
	cache, path := m.sessionCache(), sessionBindingsPath()
	if cache == nil || path == "" {
		return 0, nil
	}
	saved := cache.Snapshot()
	data, errMarshal := json.Marshal(saved)
	if errMarshal != nil {
		return 0, errMarshal
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return 0, errMkdir
	}
	tmp := path + ".tmp"
	if errWrite := os.WriteFile(tmp, data, 0o600); errWrite != nil {
		return 0, errWrite
	}
	if errRename := os.Rename(tmp, path); errRename != nil {
		return 0, errRename
	}
	return len(saved), nil
}

// RestoreSessionBindings loads what the last run saved and removes the file, so a crash
// later never brings back bindings older than the ones this run makes.
func (m *Manager) RestoreSessionBindings() (int, error) {
	cache, path := m.sessionCache(), sessionBindingsPath()
	if cache == nil || path == "" {
		return 0, nil
	}
	data, errRead := os.ReadFile(path)
	if errors.Is(errRead, fs.ErrNotExist) {
		return 0, nil
	}
	if errRead != nil {
		return 0, errRead
	}
	if errRemove := os.Remove(path); errRemove != nil {
		return 0, errRemove
	}
	var saved []SavedSessionBinding
	if errUnmarshal := json.Unmarshal(data, &saved); errUnmarshal != nil {
		return 0, fmt.Errorf("%s: %w", path, errUnmarshal)
	}
	return cache.Restore(saved), nil
}
