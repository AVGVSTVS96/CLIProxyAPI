package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type cacheWarmUpstream struct {
	mu      sync.Mutex
	bodies  []string
	written int
}

func (u *cacheWarmUpstream) handle(c *gin.Context) {
	body, _ := io.ReadAll(c.Request.Body)
	u.mu.Lock()
	u.bodies = append(u.bodies, string(body))
	written := u.written
	u.mu.Unlock()
	c.String(http.StatusOK, `{"usage":{"cache_read_input_tokens":1000,"cache_creation_input_tokens":%d}}`, written)
}

func (u *cacheWarmUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.bodies...)
}

func newCacheWarmTest(t *testing.T, status string, job ...string) (*gin.Engine, *cacheWarmUpstream) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := `{"pid":1,"sessionId":"s1-session","status":"` + status + `"}`
	if err := os.WriteFile(filepath.Join(dir, "sessions", "1.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(job) > 0 {
		if err := os.MkdirAll(filepath.Join(dir, "jobs", "s1-sessi"), 0o700); err != nil {
			t.Fatal(err)
		}
		state := `{"sessionId":"s1-session","state":"` + job[0] + `"}`
		if err := os.WriteFile(filepath.Join(dir, "jobs", "s1-sessi", "state.json"), []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	engine := gin.New()
	upstream := &cacheWarmUpstream{}
	warmer := newCacheWarmer(engine)
	warmer.after = 30 * time.Millisecond
	engine.POST("/v1/messages", warmer.record, upstream.handle)
	return engine, upstream
}

func sendCacheWarmTest(engine *gin.Engine, agent string) {
	body := `{"model":"m","max_tokens":64000,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("X-Claude-Code-Session-Id", "s1-session")
	if agent != "" {
		request.Header.Set("X-Claude-Code-Agent-Id", agent)
	}
	engine.ServeHTTP(httptest.NewRecorder(), request)
}

func TestCacheWarmTouchesBusySessionForOneToken(t *testing.T) {
	engine, upstream := newCacheWarmTest(t, "busy")
	sendCacheWarmTest(engine, "")
	time.Sleep(100 * time.Millisecond)

	bodies := upstream.seen()
	if len(bodies) < 3 {
		t.Fatalf("want the request and repeated touches, got %d requests", len(bodies))
	}
	touch := bodies[1]
	if gjson.Get(touch, "max_tokens").Int() != 1 || gjson.Get(touch, "stream").Bool() {
		t.Fatalf("touch should ask for one token without streaming: %s", touch)
	}
	if gjson.Get(touch, "messages").Raw != gjson.Get(bodies[0], "messages").Raw {
		t.Fatalf("touch should replay the conversation unchanged")
	}
}

func TestCacheWarmLetsIdleSessionCool(t *testing.T) {
	engine, upstream := newCacheWarmTest(t, "idle")
	sendCacheWarmTest(engine, "")
	time.Sleep(80 * time.Millisecond)

	if n := len(upstream.seen()); n != 1 {
		t.Fatalf("an idle session should get no touch, got %d requests", n)
	}
}

func TestCacheWarmSkipsSubagents(t *testing.T) {
	engine, upstream := newCacheWarmTest(t, "busy")
	sendCacheWarmTest(engine, "agent-1")
	time.Sleep(80 * time.Millisecond)

	if n := len(upstream.seen()); n != 1 {
		t.Fatalf("a subagent's conversation should get no touch, got %d requests", n)
	}
}

func TestCacheWarmStopsWhenTouchMissesTheCache(t *testing.T) {
	engine, upstream := newCacheWarmTest(t, "busy")
	upstream.written = 900
	sendCacheWarmTest(engine, "")
	time.Sleep(120 * time.Millisecond)

	if n := len(upstream.seen()); n != 2 {
		t.Fatalf("a touch that re-saves the conversation should be the last, got %d requests", n)
	}
}

func TestCacheWarmTouchesIdleSessionWhoseJobIsWorking(t *testing.T) {
	engine, upstream := newCacheWarmTest(t, "idle", "working")
	sendCacheWarmTest(engine, "")
	time.Sleep(80 * time.Millisecond)

	if n := len(upstream.seen()); n < 2 {
		t.Fatalf("a session still working on its job should be touched, got %d requests", n)
	}
}

func TestCacheWarmLetsBlockedJobCool(t *testing.T) {
	engine, upstream := newCacheWarmTest(t, "idle", "blocked")
	sendCacheWarmTest(engine, "")
	time.Sleep(80 * time.Millisecond)

	if n := len(upstream.seen()); n != 1 {
		t.Fatalf("a session waiting on its owner should get no touch, got %d requests", n)
	}
}

func TestCacheWarmKeepsWorkingSessionWarmAfterItIsStopped(t *testing.T) {
	engine, upstream := newCacheWarmTest(t, "idle", "working")
	stopped := `{"sessionId":"s1-session","state":"stopped"}`
	file := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "jobs", "s1-sessi", "state.json")
	if err := os.WriteFile(file, []byte(stopped), 0o600); err != nil {
		t.Fatal(err)
	}
	sendCacheWarmTest(engine, "")
	time.Sleep(80 * time.Millisecond)

	if n := len(upstream.seen()); n < 2 {
		t.Fatalf("a session stopped while working should stay warm, got %d requests", n)
	}
}

func TestCacheWarmSeesAJobThatTurnsBlocked(t *testing.T) {
	engine, upstream := newCacheWarmTest(t, "idle", "working")
	blocked := `{"sessionId":"s1-session","state":"blocked"}`
	file := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "jobs", "s1-sessi", "state.json")
	if err := os.WriteFile(file, []byte(blocked), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	sendCacheWarmTest(engine, "")
	time.Sleep(80 * time.Millisecond)

	if n := len(upstream.seen()); n != 1 {
		t.Fatalf("a session that turned to waiting on its owner should cool, got %d requests", n)
	}
}
