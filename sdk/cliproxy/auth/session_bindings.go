package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
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
	dir := claudePool.cfg.RunState()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, sessionBindingsFile)
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
	if errWrite := util.ReplaceFile(path, data); errWrite != nil {
		return 0, errWrite
	}
	return len(saved), nil
}

// RestoreSessionBindings loads what the last run saved.
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
	var saved []SavedSessionBinding
	if errUnmarshal := json.Unmarshal(data, &saved); errUnmarshal != nil {
		return 0, fmt.Errorf("%s: %w", path, errUnmarshal)
	}
	return cache.Restore(saved), nil
}
