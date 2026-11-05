package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type usageStoreStub struct{ events []usage.Event }

func (s usageStoreStub) Events() []usage.Event { return s.events }
func (s usageStoreStub) Devices() []usage.Device {
	return []usage.Device{{ID: "server", Label: "server"}}
}
func (s usageStoreStub) Revision() uint64        { return 1 }
func (s usageStoreStub) Subscribe(func()) func() { return func() {} }

// TestUsageDeckRoutes serves the deck views through the real server: behind the
// management key on v8 and v0, the live stream through response compression,
// and policy saves through the v8 config API's If-Match check.
func TestUsageDeckRoutes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	dir := t.TempDir()
	hex := filepath.Join(dir, "hex")
	claude := filepath.Join(dir, "claude")
	for _, folder := range []string{filepath.Join(hex, ".hex", "bin"), filepath.Join(claude, "sessions"), filepath.Join(dir, "state")} {
		if err := os.MkdirAll(folder, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\ncase \"$1\" in\nthreads) echo '[{\"app\":\"telegram\",\"thread\":\"-1001:5\",\"name\":\"Usage work\",\"session\":\"11111111-1111-1111-1111-111111111111\"}]';;\nlog) echo '[]';;\nwatch) exec sleep 600;;\nesac\n"
	if err := os.WriteFile(filepath.Join(hex, ".hex", "bin", "hex"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	store := usageStoreStub{events: []usage.Event{{
		ID: "a", Timestamp: now.Add(-time.Minute).Format(time.RFC3339Nano), Harness: "claude", Model: "claude-opus-4-5",
		Project: "/home/me/hex", SessionID: "11111111-1111-1111-1111-111111111111", InputTokens: 10, OutputTokens: 20,
		CostUSD: 0.5, CostKnown: true, DeviceID: "server",
	}}}

	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}}}
	cfg.Usage = proxyconfig.UsageConfig{Timezone: "UTC", Hex: hex, Claude: claude}
	cfg.Routing.Pool.StateDir = filepath.Join(dir, "state")
	server := newTestServerWithConfig(t, cfg, WithUsageStore(store))
	t.Cleanup(func() { _ = server.Stop(t.Context()) })
	if err := os.WriteFile(server.configFilePath, []byte("config-version: 8\nrouting:\n  pool:\n    session-cutoff: 90\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := proxyconfig.LoadConfig(server.configFilePath)
	if err != nil {
		t.Fatal(err)
	}
	server.mgmt.SetConfig(loaded)

	do := func(method, path, body string, key bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if key {
			req.Header.Set("Authorization", "Bearer test-management-key")
		}
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		return recorder
	}
	if got := do(http.MethodGet, "/v8/management/deck/usage?range=today", "", false).Code; got != http.StatusUnauthorized {
		t.Fatalf("usage without the key answered %d", got)
	}
	var view struct {
		Rows []struct {
			Label string `json:"label"`
		} `json:"rows"`
		Totals struct {
			APIUSD float64 `json:"apiUsd"`
		} `json:"totals"`
	}
	answer := do(http.MethodGet, "/v8/management/deck/usage?range=today", "", true)
	if answer.Code != http.StatusOK || json.Unmarshal(answer.Body.Bytes(), &view) != nil {
		t.Fatalf("usage: %d %s", answer.Code, answer.Body.String())
	}
	if len(view.Rows) != 1 || view.Rows[0].Label != "Usage work" || view.Totals.APIUSD != 0.5 {
		t.Fatalf("usage view = %+v", view)
	}
	if got := do(http.MethodGet, "/v0/management/deck/accounts", "", true); got.Code != http.StatusOK {
		t.Fatalf("accounts: %d %s", got.Code, got.Body.String())
	}

	live := httptest.NewServer(server.engine)
	t.Cleanup(live.Close)
	req, _ := http.NewRequest(http.MethodGet, live.URL+"/v8/management/deck/events", nil)
	req.Header.Set("Authorization", "Bearer test-management-key")
	response, err := live.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	frames := bufio.NewReader(response.Body)
	frame := func() map[string]any {
		t.Helper()
		for {
			line, err := frames.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				var event map[string]any
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					t.Fatal(err)
				}
				return event
			}
		}
	}
	snapshot := frame()
	pool, _ := snapshot["live"].(map[string]any)["pool"].(map[string]any)
	if snapshot["type"] != "snapshot" || pool == nil {
		t.Fatalf("first frame = %v", snapshot)
	}
	running := `{"sessionId":"11111111-1111-1111-1111-111111111111","pid":` + strconv.Itoa(os.Getpid()) + `,"cwd":"/home/me/hex","status":"busy","startedAt":1}`
	if err := os.WriteFile(filepath.Join(claude, "sessions", "1.json"), []byte(running), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := frame()
	session, _ := patch["live"].(map[string]any)["s:11111111-1111-1111-1111-111111111111"].(map[string]any)
	if patch["type"] != "patch" || session["label"] != "Usage work" || session["state"] != "working" {
		t.Fatalf("patch = %v", patch)
	}

	policy := func(revision string) map[string]any {
		t.Helper()
		body := `{"policy":{"sessionCutoff":80,"weeklyCutoff":98,"agentsPerAccount":100},"revision":` + strconv.Quote(revision) + `,"save":true}`
		answer := do(http.MethodPost, "/v8/management/deck/accounts/policy", body, true)
		var result map[string]any
		if answer.Code != http.StatusOK || json.Unmarshal(answer.Body.Bytes(), &result) != nil {
			t.Fatalf("policy: %d %s", answer.Code, answer.Body.String())
		}
		return result
	}
	stale := policy(`"stale"`)
	if stale["conflict"] != true || stale["saved"] != false {
		t.Fatalf("stale save = %v", stale)
	}
	pool = stale["pool"].(map[string]any)
	result := policy(pool["revision"].(string))
	saved, _ := os.ReadFile(server.configFilePath)
	if result["saved"] != true || !strings.Contains(string(saved), "session-cutoff: 80") {
		t.Fatalf("save = %v, config:\n%s", result, saved)
	}
	if next := result["pool"].(map[string]any); next["revision"] == pool["revision"] || next["policy"].(map[string]any)["sessionCutoff"] != 80.0 {
		t.Fatalf("saved pool = %v", next)
	}
}

// TestUsageDeckWithoutStore serves the pool's accounts and live keys when usage lives
// outside the proxy, and leaves the usage views to that service.
func TestUsageDeckWithoutStore(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	dir := t.TempDir()
	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}}}
	cfg.Usage = proxyconfig.UsageConfig{Timezone: "UTC", Hex: filepath.Join(dir, "hex"), Claude: filepath.Join(dir, "claude")}
	cfg.Routing.Pool.StateDir = filepath.Join(dir, "state")
	server := newTestServerWithConfig(t, cfg)
	t.Cleanup(func() { _ = server.Stop(t.Context()) })

	get := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer test-management-key")
		recorder := httptest.NewRecorder()
		server.engine.ServeHTTP(recorder, req)
		return recorder.Code
	}
	if got := get("/v8/management/deck/accounts"); got != http.StatusOK {
		t.Fatalf("accounts answered %d", got)
	}
	for _, path := range []string{"/v8/management/deck/usage", "/v8/management/deck/sessions", "/v8/management/deck/credits"} {
		if got := get(path); got != http.StatusNotFound {
			t.Fatalf("%s answered %d", path, got)
		}
	}

	live := httptest.NewServer(server.engine)
	t.Cleanup(live.Close)
	req, _ := http.NewRequest(http.MethodGet, live.URL+"/v8/management/deck/events", nil)
	req.Header.Set("Authorization", "Bearer test-management-key")
	response, err := live.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	frames := bufio.NewReader(response.Body)
	for {
		line, err := frames.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var snapshot map[string]any
			if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
				t.Fatal(err)
			}
			if pool, _ := snapshot["live"].(map[string]any)["pool"].(map[string]any); snapshot["type"] != "snapshot" || pool == nil {
				t.Fatalf("first frame = %v", snapshot)
			}
			return
		}
	}
}
