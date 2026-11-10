package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

type cacheWarmEntry struct {
	header  http.Header
	body    []byte
	sentAt  time.Time
	touches int
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
}

func newCacheWarmer(serve http.Handler) *cacheWarmer {
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
	if job.SessionID == "" || job.State == "" || job.State == "stopped" {
		return
	}
	w.mu.Lock()
	w.reported[job.SessionID] = job.State
	w.mu.Unlock()
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
	if entry != nil && entry.sentAt.After(sentAt) {
		return
	}
	if entry == nil {
		entry = &cacheWarmEntry{}
		w.entries[session] = entry
	} else if entry.timer != nil {
		entry.timer.Stop()
	}
	entry.header, entry.body, entry.sentAt, entry.touches = header, body, sentAt, 0
	entry.timer = time.AfterFunc(time.Until(sentAt.Add(w.after)), func() { w.touch(session, sentAt) })
}

func (w *cacheWarmer) touch(session string, due time.Time) {
	w.mu.Lock()
	entry := w.entries[session]
	if entry == nil || entry.sentAt != due {
		w.mu.Unlock()
		return
	}
	if entry.touches >= cacheWarmMaxTouches || !w.working(session) {
		delete(w.entries, session)
		w.mu.Unlock()
		log.Infof("cache warm: session %s left to cool after %d touches", session, entry.touches)
		return
	}
	header, body := entry.header, entry.body
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
	if entry := w.entries[session]; entry != nil && entry.sentAt == due {
		entry.touches++
		now := time.Now()
		entry.sentAt = now
		entry.timer = time.AfterFunc(w.after, func() { w.touch(session, now) })
	}
}

func (w *cacheWarmer) forget(session string, due time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if entry := w.entries[session]; entry != nil && entry.sentAt == due {
		delete(w.entries, session)
	}
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

func (w *cacheWarmer) working(session string) bool {
	if w.reported[session] == "working" {
		return true
	}
	files, _ := filepath.Glob(filepath.Join(w.claude, "sessions", "*.json"))
	for _, file := range files {
		if running := claudeState(file); running.SessionID == session && running.Status == "busy" {
			return true
		}
	}
	return false
}

type claudeSessionState struct {
	SessionID string `json:"sessionId"`
	Status    string `json:"status"`
	State     string `json:"state"`
}

func claudeState(file string) (state claudeSessionState) {
	if data, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	return state
}
