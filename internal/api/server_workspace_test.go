package api

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// TestWorkspaceRoutes serves the Files pages beside deck's views: behind the
// management key on v8, with file changes announced on deck's stream.
func TestWorkspaceRoutes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-management-key")
	dir := t.TempDir()
	gitconfig := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	live := filepath.Join(dir, "live")
	for _, args := range [][]string{{"init", "-q", live}, {"-C", live, "commit", "-q", "--allow-empty", "-m", "first"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "claude", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &proxyconfig.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{"test-key"}}}
	cfg.Usage = proxyconfig.UsageConfig{Timezone: "UTC", Hex: filepath.Join(dir, "hex"), Claude: filepath.Join(dir, "claude")}
	cfg.Workspace.Repos = []proxyconfig.WorkspaceRepo{{ID: "hex", Path: live}}
	cfg.Workspace.Worktrees = filepath.Join(dir, "worktrees")
	server := newTestServerWithConfig(t, cfg, WithUsageStore(usageStoreStub{}))
	t.Cleanup(func() { _ = server.Stop(t.Context()) })

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
	const base = "/v8/management/deck"
	if got := do(http.MethodGet, base+"/repos", "", false).Code; got != http.StatusUnauthorized {
		t.Fatalf("repos without the key answered %d", got)
	}
	repos := do(http.MethodGet, base+"/repos", "", true)
	if repos.Code != http.StatusOK || repos.Header().Get("Cache-Control") != "no-store" || !strings.Contains(repos.Body.String(), `"id":"hex"`) {
		t.Fatalf("repos: %d %s", repos.Code, repos.Body.String())
	}
	created := do(http.MethodPost, base+"/repos/hex/workspaces", `{"name":"api"}`, true)
	if created.Code != http.StatusOK || strings.TrimSpace(created.Body.String()) != `{"id":"hex~api"}` {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	if got := do(http.MethodPut, base+"/ws/hex/file?path=a.txt", "x", true); got.Code != http.StatusConflict {
		t.Fatalf("writing the live checkout: %d", got.Code)
	}
	if got := do(http.MethodGet, base+"/ws/hex~nope/changes", "", true); got.Code != http.StatusNotFound {
		t.Fatalf("unknown workspace: %d", got.Code)
	}

	stream := httptest.NewServer(server.engine)
	t.Cleanup(stream.Close)
	req, _ := http.NewRequest(http.MethodGet, stream.URL+base+"/events", nil)
	req.Header.Set("Authorization", "Bearer test-management-key")
	client := stream.Client()
	client.Timeout = 10 * time.Second
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	if got := do(http.MethodPut, base+"/ws/hex~api/file?path=notes/a.txt", "hello\n", true); got.Code != http.StatusOK {
		t.Fatalf("write: %d %s", got.Code, got.Body.String())
	}
	var file struct {
		Contents *string `json:"contents"`
		Original *string `json:"original"`
	}
	read := do(http.MethodGet, base+"/ws/hex~api/file?path=notes/a.txt", "", true)
	if err := json.Unmarshal(read.Body.Bytes(), &file); err != nil || file.Contents == nil || *file.Contents != "hello\n" || file.Original != nil {
		t.Fatalf("read: %s", read.Body.String())
	}
	changes := do(http.MethodGet, base+"/ws/hex~api/changes", "", true)
	if !strings.Contains(changes.Body.String(), `{"path":"notes/a.txt","status":"untracked","added":1,"removed":0}`) {
		t.Fatalf("changes: %s", changes.Body.String())
	}

	frames := bufio.NewReader(response.Body)
	for {
		line, err := frames.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, `data: {"type":"stale"`) && strings.Contains(line, `"ws:hex~api"`) {
			break
		}
	}
}
