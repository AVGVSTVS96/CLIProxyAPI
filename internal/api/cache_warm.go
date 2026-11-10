package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Claude's one-hour prompt cache lives an hour from its last read, and a read renews it.
// A touch three minutes early leaves room for a slow status read and the request itself.
const cacheWarmAfter = 57 * time.Minute

// A touch reads the conversation at a tenth of the input price; letting it expire and
// re-saving it costs twice the input price. Past 18 touches without the session coming
// back, warming has cost what a re-save would.
const cacheWarmMaxTouches = 18

type cacheWarmTouchKey struct{}

// cacheWarmFile holds the sessions being kept warm between two runs of the proxy, so a
// restart or reload doesn't drop them until they send again.
const cacheWarmFile = "cache-warm.json"

type cacheWarmEntry struct {
	Header  http.Header     `json:"header"`
	Body    json.RawMessage `json:"body"`
	SentAt  time.Time       `json:"sentAt"`
	Touches int             `json:"touches"`
	timer   *time.Timer
}

// cacheWarmer keeps a Claude Code session's main conversation in the prompt cache while
// Claude Code reports the session busy, or its background job says it is still working,
// so a session waiting on its own agents and jobs finds its cache warm when they wake it.
type cacheWarmer struct {
	serve   http.Handler
	claude  string
	after   time.Duration
	mu      sync.Mutex
	entries map[string]*cacheWarmEntry
	// A session hex stops while it waits on its work reads "stopped", so the warmer
	// remembers the last state each session reported for itself.
	reported map[string]string
	// names maps a session to its name, which names the agents it starts "<name>/<agent>".
	names map[string]string
	// file is where entries outlive the process; empty keeps them in memory only.
	file string
	// saved stops this process warming once it has handed its entries to the next run.
	saved bool
}

func newCacheWarmer(serve http.Handler, stateDir string) *cacheWarmer {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude")
	}
	w := &cacheWarmer{
		serve:    serve,
		claude:   dir,
		after:    cacheWarmAfter,
		entries:  map[string]*cacheWarmEntry{},
		reported: map[string]string{},
		names:    map[string]string{},
	}
	if stateDir != "" {
		if rest, ok := strings.CutPrefix(stateDir, "~"); ok {
			home, _ := os.UserHomeDir()
			stateDir = filepath.Join(home, rest)
		}
		w.file = filepath.Join(stateDir, cacheWarmFile)
	}
	w.watchJobs()
	return w
}

func (w *cacheWarmer) watchJobs() {
	jobs := filepath.Join(w.claude, "jobs")
	watcher, err := fsnotify.NewWatcher()
	if err != nil || watcher.Add(jobs) != nil {
		log.Warnf("cache warm: cannot watch %s; only busy sessions stay warm", jobs)
		return
	}
	dirs, _ := os.ReadDir(jobs)
	for _, dir := range dirs {
		if dir.IsDir() {
			_ = watcher.Add(filepath.Join(jobs, dir.Name()))
			w.note(filepath.Join(jobs, dir.Name(), "state.json"))
		}
	}
	go func() {
		for event := range watcher.Events {
			if filepath.Dir(event.Name) == jobs {
				if event.Has(fsnotify.Create) {
					_ = watcher.Add(event.Name)
					w.note(filepath.Join(event.Name, "state.json"))
				}
				continue
			}
			if filepath.Base(event.Name) == "state.json" {
				w.note(event.Name)
			}
		}
	}()
}

func (w *cacheWarmer) note(file string) {
	job := claudeState(file)
	if job.SessionID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if job.Name != "" {
		w.names[job.SessionID] = job.Name
	}
	if job.State != "" && job.State != "stopped" {
		w.reported[job.SessionID] = job.State
	}
}

func (w *cacheWarmer) record(c *gin.Context) {
	session := c.GetHeader("X-Claude-Code-Session-Id")
	if session == "" ||
		c.GetHeader("X-Claude-Code-Agent-Id") != "" ||
		c.GetHeader("X-Claude-Code-Compaction") != "" ||
		c.Request.Context().Value(cacheWarmTouchKey{}) != nil {
		c.Next()
		return
	}

	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.Next()
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	body, header := raw, c.Request.Header.Clone()
	if strings.EqualFold(header.Get("Content-Encoding"), "gzip") {
		body = gunzip(raw)
		header.Del("Content-Encoding")
	}
	header.Del("Content-Length")
	sentAt := time.Now()

	c.Next()

	if body != nil && c.Writer.Status() == http.StatusOK && bytes.Contains(body, []byte(`"ttl":"1h"`)) {
		w.keep(session, header, body, sentAt)
	}
}

func gunzip(raw []byte) []byte {
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil
	}
	return body
}

func (w *cacheWarmer) keep(session string, header http.Header, body []byte, sentAt time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()

	entry := w.entries[session]
	if w.saved || (entry != nil && (entry.SentAt.After(sentAt) || sideQuery(body, entry.Body))) {
		return
	}
	if entry == nil {
		entry = &cacheWarmEntry{}
		w.entries[session] = entry
	} else if entry.timer != nil {
		entry.timer.Stop()
	}
	entry.Header, entry.Body, entry.SentAt, entry.Touches = header, body, sentAt, 0
	w.arm(session, entry)
}

// arm must be called with w.mu held.
func (w *cacheWarmer) arm(session string, entry *cacheWarmEntry) {
	due := entry.SentAt
	entry.timer = time.AfterFunc(time.Until(due.Add(w.after)), func() { w.touch(session, due) })
}

// Claude Code's side queries, like a title or a prompt suggestion, carry the session's
// header but few or none of its tools, and keep little of the conversation in the cache.
func sideQuery(body, main []byte) bool {
	return gjson.GetBytes(body, "tools.#").Int()*2 < gjson.GetBytes(main, "tools.#").Int()
}

func (w *cacheWarmer) touch(session string, due time.Time) {
	w.mu.Lock()
	entry := w.entries[session]
	if w.saved || entry == nil || !entry.SentAt.Equal(due) {
		w.mu.Unlock()
		return
	}
	if entry.Touches >= cacheWarmMaxTouches || !w.working(session) {
		delete(w.entries, session)
		w.mu.Unlock()
		log.Infof("cache warm: session %s left to cool after %d touches", session, entry.Touches)
		return
	}
	header, body := entry.Header, entry.Body
	w.mu.Unlock()

	read, written, ok := w.send(header, body)
	if !ok || read == 0 || written > read/10 {
		log.Infof("cache warm: stopped for session %s (read %d, written %d)", session, read, written)
		w.forget(session, due)
		return
	}
	log.Infof("cache warm: touched session %s (read %d, written %d)", session, read, written)

	w.mu.Lock()
	defer w.mu.Unlock()
	if entry := w.entries[session]; entry != nil && entry.SentAt.Equal(due) && !w.saved {
		entry.Touches++
		entry.SentAt = time.Now()
		w.arm(session, entry)
	}
}

func (w *cacheWarmer) forget(session string, due time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if entry := w.entries[session]; entry != nil && entry.SentAt.Equal(due) {
		delete(w.entries, session)
	}
}

// save writes the entries for the next run and stops this process warming, so a reload's
// old process, still finishing answers, never touches a session the new one now keeps.
func (w *cacheWarmer) save() (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == "" || w.saved {
		return 0, nil
	}
	w.saved = true
	for _, entry := range w.entries {
		entry.timer.Stop()
	}
	data, err := json.Marshal(w.entries)
	if err != nil {
		return 0, err
	}
	if err = os.MkdirAll(filepath.Dir(w.file), 0o700); err != nil {
		return 0, err
	}
	tmp := w.file + ".tmp"
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		return 0, err
	}
	return len(w.entries), os.Rename(tmp, w.file)
}

// restore loads what the last run saved and removes the file, so a crash later never
// brings back entries older than the ones this run makes. A cache past its hour is gone,
// and touching it would save it again, so its entry stays behind.
func (w *cacheWarmer) restore() (int, error) {
	if w.file == "" {
		return 0, nil
	}
	data, err := os.ReadFile(w.file)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if err = os.Remove(w.file); err != nil {
		return 0, err
	}
	var saved map[string]*cacheWarmEntry
	if err = json.Unmarshal(data, &saved); err != nil {
		return 0, fmt.Errorf("%s: %w", w.file, err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	restored := 0
	for session, entry := range saved {
		if current := w.entries[session]; current != nil || time.Since(entry.SentAt) > cacheWarmAfter+2*time.Minute {
			continue
		}
		w.entries[session] = entry
		w.arm(session, entry)
		restored++
	}
	return restored, nil
}

// send replays the conversation's last request for one output token, which reads its
// whole prefix from the cache and so renews it, on the account the session is bound to.
func (w *cacheWarmer) send(header http.Header, body []byte) (read, written int64, ok bool) {
	body, err := sjson.SetBytes(body, "max_tokens", 1)
	if err != nil {
		return 0, 0, false
	}
	if body, err = sjson.SetBytes(body, "stream", false); err != nil {
		return 0, 0, false
	}

	ctx := context.WithValue(context.Background(), cacheWarmTouchKey{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header = header.Clone()
	request.Header.Del("Accept-Encoding")
	response := httptest.NewRecorder()
	w.serve.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		return 0, 0, false
	}
	usage := gjson.GetBytes(response.Body.Bytes(), "usage")
	return usage.Get("cache_read_input_tokens").Int(), usage.Get("cache_creation_input_tokens").Int(), true
}

// working must be called with w.mu held. A session counts as working while Claude Code
// shows it busy, its background job last said it was working, or an agent it started
// (a live session named "<its name>/...") still runs.
func (w *cacheWarmer) working(session string) bool {
	if w.reported[session] == "working" {
		return true
	}
	files, _ := filepath.Glob(filepath.Join(w.claude, "sessions", "*.json"))
	running := make([]claudeSessionState, 0, len(files))
	for _, file := range files {
		if state := claudeState(file); alive(state.PID) {
			running = append(running, state)
		}
	}
	name := w.names[session]
	for _, state := range running {
		if state.SessionID == session {
			if state.Status == "busy" {
				return true
			}
			if state.Name != "" {
				name = state.Name
			}
		}
	}
	if name == "" {
		return false
	}
	for _, state := range running {
		if strings.HasPrefix(state.Name, name+"/") {
			return true
		}
	}
	return false
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

type claudeSessionState struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	State     string `json:"state"`
}

func claudeState(file string) (state claudeSessionState) {
	if data, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	return state
}

// SaveCacheWarm hands the sessions being kept warm to the next run and stops warming here.
func (s *Server) SaveCacheWarm() {
	if s == nil || s.cacheWarmer == nil {
		return
	}
	saved, err := s.cacheWarmer.save()
	if err != nil {
		log.Errorf("cache warm: could not save sessions for the next run: %v", err)
	} else if saved > 0 {
		log.Infof("cache warm: saved %d sessions for the next run", saved)
	}
}

// RestoreCacheWarm picks up the sessions the last run was keeping warm.
func (s *Server) RestoreCacheWarm() {
	if s == nil || s.cacheWarmer == nil {
		return
	}
	restored, err := s.cacheWarmer.restore()
	if err != nil {
		log.Warnf("cache warm: could not restore the last run's sessions: %v", err)
	} else if restored > 0 {
		log.Infof("cache warm: restored %d sessions from the last run", restored)
	}
}
