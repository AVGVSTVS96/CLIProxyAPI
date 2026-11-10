package api

import (
	"fmt"
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

	engine, upstream, _ := newCacheWarmEngine("")
	return engine, upstream
}

func newCacheWarmEngine(stateDir string) (*gin.Engine, *cacheWarmUpstream, *cacheWarmer) {
	engine := gin.New()
	upstream := &cacheWarmUpstream{}
	warmer := newCacheWarmer(engine, stateDir)
	warmer.after = 30 * time.Millisecond
	engine.POST("/v1/messages", warmer.record, upstream.handle)
	return engine, upstream, warmer
}

func writeCacheWarmSession(t *testing.T, pid int, session, name, status string) {
	t.Helper()
	state := fmt.Sprintf(`{"pid":%d,"sessionId":%q,"name":%q,"status":%q}`, pid, session, name, status)
	file := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "sessions", fmt.Sprintf("%s.json", session))
	if err := os.WriteFile(file, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sendCacheWarmTest(engine *gin.Engine, agent string, tools ...string) {
	if len(tools) == 0 {
		tools = []string{`[{"name":"a"},{"name":"b"}]`}
	}
	body := `{"model":"m","max_tokens":64000,"stream":true,"tools":` + tools[0] + `,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
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

func TestCacheWarmKeepsTheMainConversationOverASideQuery(t *testing.T) {
	engine, upstream := newCacheWarmTest(t, "busy")
	sendCacheWarmTest(engine, "")
	sendCacheWarmTest(engine, "", `[]`)
	time.Sleep(60 * time.Millisecond)

	bodies := upstream.seen()
	if len(bodies) < 3 || gjson.Get(bodies[2], "tools.#").Int() != 2 {
		t.Fatalf("the touch should replay the main conversation, not the side query")
	}
}

func TestCacheWarmTouchesSessionWhileAnAgentItStartedRuns(t *testing.T) {
	engine, upstream := newCacheWarmTest(t, "idle")
	writeCacheWarmSession(t, 1, "s1-session", "BROADCOM", "idle")
	writeCacheWarmSession(t, os.Getpid(), "agent-session", "BROADCOM/keep-warm", "busy")
	sendCacheWarmTest(engine, "")
	time.Sleep(80 * time.Millisecond)

	if n := len(upstream.seen()); n < 2 {
		t.Fatalf("a session whose agent job still runs should be touched, got %d requests", n)
	}
}

func TestCacheWarmLetsSessionCoolOnceItsAgentHasExited(t *testing.T) {
	engine, upstream := newCacheWarmTest(t, "idle")
	writeCacheWarmSession(t, 1, "s1-session", "BROADCOM", "idle")
	writeCacheWarmSession(t, 0, "agent-session", "BROADCOM/keep-warm", "busy")
	writeCacheWarmSession(t, os.Getpid(), "other-session", "BROADCOMX/agent", "busy")
	sendCacheWarmTest(engine, "")
	time.Sleep(80 * time.Millisecond)

	if n := len(upstream.seen()); n != 1 {
		t.Fatalf("a session with no live agent of its own should cool, got %d requests", n)
	}
}

func TestCacheWarmFindsTheNameOfAStoppedSessionInItsJob(t *testing.T) {
	engine, upstream := newCacheWarmTest(t, "idle", "stopped")
	stopped := `{"sessionId":"s1-session","state":"stopped","name":"BROADCOM"}`
	file := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "jobs", "s1-sessi", "state.json")
	if err := os.WriteFile(file, []byte(stopped), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "sessions", "1.json")); err != nil {
		t.Fatal(err)
	}
	writeCacheWarmSession(t, os.Getpid(), "agent-session", "BROADCOM/keep-warm", "busy")
	time.Sleep(20 * time.Millisecond)
	sendCacheWarmTest(engine, "")
	time.Sleep(80 * time.Millisecond)

	if n := len(upstream.seen()); n < 2 {
		t.Fatalf("a stopped session whose agent still runs should be touched, got %d requests", n)
	}
}

func TestCacheWarmSessionsSurviveARestart(t *testing.T) {
	_, _ = newCacheWarmTest(t, "busy")
	state := t.TempDir()
	before, beforeUpstream, beforeWarmer := newCacheWarmEngine(state)
	beforeWarmer.after = time.Hour
	sendCacheWarmTest(before, "")
	if saved, err := beforeWarmer.save(); err != nil || saved != 1 {
		t.Fatalf("save = %d, %v", saved, err)
	}

	_, afterUpstream, afterWarmer := newCacheWarmEngine(state)
	if restored, err := afterWarmer.restore(); err != nil || restored != 1 {
		t.Fatalf("restore = %d, %v", restored, err)
	}
	if _, err := os.Stat(filepath.Join(state, cacheWarmFile)); !os.IsNotExist(err) {
		t.Fatalf("restore should remove the saved file, got %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	if n := len(afterUpstream.seen()); n < 1 {
		t.Fatalf("the next run should touch the saved session, got %d touches", n)
	}
	if gjson.Get(afterUpstream.seen()[0], "max_tokens").Int() != 1 {
		t.Fatalf("the next run should replay the saved conversation for one token")
	}
	sendCacheWarmTest(before, "")
	if n := len(beforeUpstream.seen()); n != 2 || len(beforeWarmer.entries) != 1 {
		t.Fatalf("the old run should stop keeping sessions once it saved them")
	}
}

func TestCacheWarmLeavesAnExpiredCacheBehindAfterARestart(t *testing.T) {
	_, _ = newCacheWarmTest(t, "busy")
	state := t.TempDir()
	before, _, beforeWarmer := newCacheWarmEngine(state)
	beforeWarmer.after = time.Hour
	sendCacheWarmTest(before, "")
	for _, entry := range beforeWarmer.entries {
		entry.SentAt = time.Now().Add(-time.Hour)
	}
	if _, err := beforeWarmer.save(); err != nil {
		t.Fatal(err)
	}

	_, upstream, afterWarmer := newCacheWarmEngine(state)
	if restored, err := afterWarmer.restore(); err != nil || restored != 0 {
		t.Fatalf("a cache past its hour should not come back, restore = %d, %v", restored, err)
	}
	time.Sleep(60 * time.Millisecond)
	if n := len(upstream.seen()); n != 0 {
		t.Fatalf("an expired cache should get no touch, got %d", n)
	}
}
