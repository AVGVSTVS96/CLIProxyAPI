package workspace

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// sandbox gives git a config of its own with a fresh SSH signing key, and returns a temp folder.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "signing-key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	gitconfig := filepath.Join(dir, "gitconfig")
	settings := "[user]\n\tname = Test\n\temail = test@example.com\n\tsigningkey = " + key + ".pub\n" +
		"[gpg]\n\tformat = ssh\n[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n"
	if err := os.WriteFile(gitconfig, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("HOME", dir)
	return dir
}

func sh(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setup makes a live checkout with one commit, cloned from a bare origin, and a service over it.
func setup(t *testing.T, review bool) (*Service, string) {
	t.Helper()
	dir := sandbox(t)
	origin := filepath.Join(dir, "origin.git")
	live := filepath.Join(dir, "live")
	sh(t, dir, "init", "-q", "--bare", origin)
	sh(t, dir, "clone", "-q", origin, live)
	write(t, filepath.Join(live, "a.txt"), "one\ntwo\nthree\n")
	sh(t, live, "add", "-A")
	sh(t, live, "commit", "-q", "-m", "first")
	sh(t, live, "push", "-q", "origin", "main")

	var cfg config.Config
	cfg.Usage.Hex = filepath.Join(dir, "hex")
	cfg.Usage.Claude = filepath.Join(dir, "claude")
	cfg.Workspace.Repos = []config.WorkspaceRepo{{ID: "hex", Path: live, Review: review}, {ID: "gone", Path: filepath.Join(dir, "nothing")}}
	cfg.Workspace.Worktrees = filepath.Join(dir, "worktrees")
	w := New(Options{Config: cfg})
	t.Cleanup(w.Close)
	return w, live
}

func status(err error) int {
	if err == nil {
		return http.StatusOK
	}
	return statusOf(err)
}

func TestEditCommitShip(t *testing.T) {
	w, live := setup(t, false)

	repos, err := w.Repos()
	if err != nil || len(repos) != 1 || *repos[0].Branch != "main" || *repos[0].Unpushed != 0 {
		t.Fatalf("repos = %+v, %v (a repo without .git must be skipped)", repos, err)
	}
	id, err := w.Create("hex", "My Edit!")
	if err != nil || id != "hex~my-edit" {
		t.Fatalf("create = %q, %v", id, err)
	}
	if again, _ := w.Create("hex", "my edit"); again != "hex~my-edit-2" {
		t.Fatalf("second create = %q", again)
	}
	repos, _ = w.Repos()
	ws := repos[0].Workspaces
	if len(ws) != 3 || ws[0].Name != nil || ws[1].ID != id || *ws[1].Branch != "edit/my-edit" || *ws[1].Base != "main" {
		t.Fatalf("workspaces = %+v", ws)
	}

	if err := w.Write("hex", "a.txt", []byte("x")); status(err) != http.StatusConflict {
		t.Fatalf("writing the live checkout: %v", err)
	}
	for _, bad := range []string{"../x", ".git/config", "/etc/passwd", "sub/../../x", "", "."} {
		if err := w.Write(id, bad, []byte("x")); status(err) != http.StatusConflict {
			t.Fatalf("write %q: %v", bad, err)
		}
		if _, err := w.Read(id, bad); status(err) != http.StatusConflict {
			t.Fatalf("read %q: %v", bad, err)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws[1].Path, "link")); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(id, "link/escape.txt", []byte("x")); status(err) != http.StatusConflict || exists(filepath.Join(outside, "escape.txt")) {
		t.Fatalf("write through a symlink out of the worktree: %v", err)
	}
	if err := os.Remove(filepath.Join(ws[1].Path, "link")); err != nil {
		t.Fatal(err)
	}

	if err := w.Write(id, "a.txt", []byte("one\n2\nthree\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(id, "b/new.txt", []byte("x\ny")); err != nil {
		t.Fatal(err)
	}
	body, err := w.Read(id, "a.txt")
	if err != nil || *body.Contents != "one\n2\nthree\n" || *body.Original != "one\ntwo\nthree\n" {
		t.Fatalf("read = %+v, %v", body, err)
	}
	if body, _ := w.Read(id, "b/new.txt"); body.Original != nil {
		t.Fatal("a new file has no original")
	}
	changes, err := w.Changes(id)
	want := []Change{{Path: "a.txt", Status: "modified", Added: 1, Removed: 1}, {Path: "b/new.txt", Status: "untracked", Added: 2}}
	if err != nil || !changes.Dirty || len(changes.Files) != 2 || changes.Files[0] != want[0] || changes.Files[1] != want[1] {
		t.Fatalf("changes = %+v, %v", changes, err)
	}
	files, _ := w.Files(id)
	if strings.Join(files, ",") != "a.txt,b/new.txt" {
		t.Fatalf("files = %v", files)
	}

	if _, err := w.Ship(id); status(err) != http.StatusConflict || !strings.Contains(err.Error(), "Commit") {
		t.Fatalf("shipping dirty: %v", err)
	}
	if _, _, err := w.Commit(id, "  "); status(err) != http.StatusConflict {
		t.Fatalf("empty message: %v", err)
	}
	sha, signed, err := w.Commit(id, "edit a\n\nwith a body")
	if err != nil || !signed {
		t.Fatalf("commit = %s %v %v", sha, signed, err)
	}
	if _, _, err := w.Commit(id, "again"); status(err) != http.StatusConflict {
		t.Fatalf("nothing to commit: %v", err)
	}
	changes, _ = w.Changes(id)
	if changes.Dirty || changes.Ahead != 1 || changes.Behind != 0 || len(changes.Files) != 2 || changes.Files[1].Status != "added" {
		t.Fatalf("after commit = %+v", changes)
	}

	commits, err := w.Commits(id, 10, "")
	if err != nil || len(commits) != 2 || commits[0].SHA != sha || commits[0].Subject != "edit a" || !commits[0].Signed || commits[1].Signed ||
		commits[0].Author != "Test" || time.Since(time.UnixMilli(commits[0].At)) > time.Hour {
		t.Fatalf("commits = %+v, %v", commits, err)
	}
	detail, err := w.CommitDetail(id, commits[0].Short)
	if err != nil || detail.SHA != sha || detail.Body != "with a body" || !strings.Contains(detail.Patch, "diff --git a/a.txt b/a.txt") || !strings.Contains(detail.Patch, "+2") {
		t.Fatalf("detail = %+v, %v", detail, err)
	}
	if _, err := w.CommitDetail(id, "--output=/tmp/x"); status(err) != http.StatusNotFound {
		t.Fatalf("bad sha: %v", err)
	}

	shipped, err := w.Ship(id)
	if err != nil || shipped.Branch != "main" || shipped.Commits != 1 || shipped.Head == "" {
		t.Fatalf("ship = %+v, %v", shipped, err)
	}
	if sh(t, live, "rev-parse", "HEAD") != sha {
		t.Fatal("live branch didn't fast-forward to the shipped commit")
	}
	if _, err := w.Ship(id); status(err) != http.StatusConflict {
		t.Fatalf("nothing to ship: %v", err)
	}
	if err := w.Push("hex"); err != nil {
		t.Fatal(err)
	}
	if repos, _ := w.Repos(); *repos[0].Unpushed != 0 {
		t.Fatal("push left commits unpushed")
	}
	if kept, err := w.Discard(id); err != nil || kept != nil || sh(t, live, "branch", "--list", "edit/my-edit") != "" {
		t.Fatalf("discarding a shipped worktree kept %v, %v", kept, err)
	}
	if _, err := w.Changes(id); status(err) != http.StatusNotFound {
		t.Fatalf("discarded workspace: %v", err)
	}
}

func TestShipRebasesWhenLiveMoved(t *testing.T) {
	w, live := setup(t, false)
	id, _ := w.Create("hex", "rebase")
	repos, _ := w.Repos()
	path := repos[0].Workspaces[1].Path

	write(t, filepath.Join(path, "b.txt"), "b\n")
	if _, _, err := w.Commit(id, "add b"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(live, "c.txt"), "c\n")
	sh(t, live, "add", "-A")
	sh(t, live, "commit", "-q", "-m", "live moved")
	if changes, _ := w.Changes(id); changes.Ahead != 1 || changes.Behind != 1 {
		t.Fatalf("changes = %+v", changes)
	}
	shipped, err := w.Ship(id)
	if err != nil || shipped.Commits != 1 {
		t.Fatalf("ship = %+v, %v", shipped, err)
	}
	if sh(t, live, "log", "--format=%s", "-2") != "add b\nlive moved" || !w.signed(live, "HEAD") {
		t.Fatal("the rebased commit didn't land signed on top of live")
	}

	// A conflicting move aborts the rebase and ships nothing.
	write(t, filepath.Join(path, "a.txt"), "mine\n")
	if _, _, err := w.Commit(id, "mine"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(live, "a.txt"), "theirs\n")
	sh(t, live, "commit", "-q", "-am", "theirs")
	before := sh(t, live, "rev-parse", "HEAD")
	if _, err := w.Ship(id); status(err) != http.StatusConflict || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("conflicting ship: %v", err)
	}
	if sh(t, live, "rev-parse", "HEAD") != before || exists(filepath.Join(gitDir(path), "rebase-merge")) {
		t.Fatal("a conflicting ship changed live or left the rebase running")
	}
}

func TestShipRefusesUnsigned(t *testing.T) {
	w, _ := setup(t, false)
	id, _ := w.Create("hex", "unsigned")
	repos, _ := w.Repos()
	path := repos[0].Workspaces[1].Path
	write(t, filepath.Join(path, "b.txt"), "b\n")
	sh(t, path, "add", "-A")
	sh(t, path, "commit", "-q", "--no-gpg-sign", "-m", "unsigned")
	if _, err := w.Ship(id); status(err) != http.StatusConflict || !strings.Contains(err.Error(), "Unsigned") {
		t.Fatalf("shipping an unsigned commit: %v", err)
	}
}

func TestReviewRepoRefusesPush(t *testing.T) {
	w, _ := setup(t, true)
	if err := w.Push("hex"); status(err) != http.StatusConflict {
		t.Fatalf("pushing a review repo: %v", err)
	}
	if err := w.Push("nope"); status(err) != http.StatusNotFound {
		t.Fatalf("unknown repo: %v", err)
	}
}

func TestDiscardKeepsUnmergedBranch(t *testing.T) {
	w, live := setup(t, false)
	id, _ := w.Create("hex", "keep")
	repos, _ := w.Repos()
	write(t, filepath.Join(repos[0].Workspaces[1].Path, "b.txt"), "b\n")
	if _, _, err := w.Commit(id, "unmerged"); err != nil {
		t.Fatal(err)
	}
	kept, err := w.Discard(id)
	if err != nil || kept == nil || *kept != "edit/keep" || sh(t, live, "branch", "--list", "edit/keep") == "" {
		t.Fatalf("discard kept %v, %v", kept, err)
	}
	if _, err := w.Discard("hex"); status(err) != http.StatusConflict {
		t.Fatalf("discarding the live checkout: %v", err)
	}

	empty, _ := w.Create("hex", "empty")
	if kept, err := w.Discard(empty); err != nil || kept != nil || sh(t, live, "branch", "--list", "edit/empty") != "" {
		t.Fatalf("discarding an empty worktree kept %v, %v", kept, err)
	}
}

func TestDiff(t *testing.T) {
	w, live := setup(t, false)
	write(t, filepath.Join(live, "a.txt"), "one\n2\nthree\n")
	write(t, filepath.Join(live, "new file.txt"), "x\ny")
	write(t, filepath.Join(live, "one.txt"), "solo\n")
	write(t, filepath.Join(live, "empty.txt"), "")
	write(t, filepath.Join(live, "image.bin"), "PNG\x00\x01")

	diff, err := w.Diff("hex")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(diff.Omitted, ",") != "image.bin" {
		t.Fatalf("omitted = %v", diff.Omitted)
	}
	files := splitPatch(diff.Patch)
	if len(files) != 4 || !strings.HasPrefix(files[0], "diff --git a/a.txt b/a.txt\n") || !strings.Contains(files[0], "-two\n+2\n") {
		t.Fatalf("patch:\n%s", diff.Patch)
	}
	// Each untracked file reads exactly as git shows a new file, less the blob index line.
	for _, name := range []string{"new file.txt", "one.txt", "empty.txt"} {
		cmd := exec.Command("git", "diff", "--no-index", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", "/dev/null", name)
		cmd.Dir = live
		out, _ := cmd.Output()
		var lines []string
		for _, line := range strings.SplitAfter(string(out), "\n") {
			if !strings.HasPrefix(line, "index ") {
				lines = append(lines, line)
			}
		}
		got, _ := newFileDiff(live, name)
		if got != strings.Join(lines, "") {
			t.Fatalf("%s:\ngot:\n%s\ngit:\n%s", name, got, strings.Join(lines, ""))
		}
		if !strings.Contains(diff.Patch, got) {
			t.Fatalf("%s missing from the patch", name)
		}
	}
	if quoteName(`a/"é\`) != `"a/\"\303\251\\"` {
		t.Fatalf("quoteName = %s", quoteName(`a/"é\`))
	}
}

func TestCountLines(t *testing.T) {
	for text, want := range map[string]int{"": 0, "a": 1, "a\n": 1, "a\nb": 2, "a\nb\n": 2, "\x00a\n": 0} {
		if got := countLines([]byte(text)); got != want {
			t.Errorf("countLines(%q) = %d, want %d", text, got, want)
		}
	}
}

func TestDraftsAndPlugins(t *testing.T) {
	dir := t.TempDir()
	hex := filepath.Join(dir, "hex")
	claude := filepath.Join(dir, "claude")
	drafts := filepath.Join(dir, "drafts")
	write(t, filepath.Join(drafts, "AGENTS.md"), "new rules")
	write(t, filepath.Join(drafts, "loose.md"), "x")
	write(t, filepath.Join(drafts, ".hidden"), "x")
	write(t, filepath.Join(hex, ".hex", ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(hex, ".hex", "channels", "telegram", ".claude-plugin", "plugin.json"), `{"version":"0.2.0","description":"Telegram channel"}`)
	write(t, filepath.Join(hex, "state", "telegram", "hub.sock"), "")
	write(t, filepath.Join(hex, ".hex", "mods", "panel", "README.md"), "# panel\n\nA side panel.\n")
	write(t, filepath.Join(claude, "settings.json"), `{"enabledPlugins":{"tool@market":true},
		"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"guard.sh"}]}]}}`)
	write(t, filepath.Join(hex, ".mcp.json"), `{"mcpServers":{"t3":{"type":"http","url":"http://t3/mcp"},"off":{"command":"off-server"}}}`)
	write(t, filepath.Join(hex, ".claude", "settings.json"), `{"enabledMcpjsonServers":["t3","off"],"deniedMcpServers":[{"serverName":"off"}]}`)
	write(t, filepath.Join(claude, "skills", "x", "SKILL.md"), "---\nname: x\ndescription: >\n  Read X\n  as he sees it.\n---\n# x\n")
	write(t, filepath.Join(claude, "skills", "notes", "README.md"), "no skill here")
	write(t, filepath.Join(claude, "plugins", "cache", "tool", ".claude-plugin", "plugin.json"), `{"description":"A tool"}`)
	write(t, filepath.Join(claude, "plugins", "installed_plugins.json"), `{"version":2,"plugins":{
		"telegram@hex":[{"scope":"user","version":"0.1.0","installPath":"/nowhere"}],
		"tool@market":[{"scope":"user","version":"1.2.3","installPath":"`+filepath.Join(claude, "plugins", "cache", "tool")+`"}],
		"off@market":[{"scope":"project","version":"1.0.0","installPath":"/nowhere"}]}}`)

	var cfg config.Config
	cfg.Usage.Hex, cfg.Usage.Claude = hex, claude
	cfg.Workspace.Repos = []config.WorkspaceRepo{{ID: "core", Path: filepath.Join(hex, ".hex")}}
	cfg.Workspace.Drafts = config.WorkspaceDrafts{Dir: drafts, Targets: map[string]string{"AGENTS.md": "hex:AGENTS.md"}}
	w := New(Options{Config: cfg})
	t.Cleanup(w.Close)

	list := w.Drafts()
	if len(list) != 2 || list[0].Name != "AGENTS.md" || *list[0].Target != (Target{Repo: "hex", Path: "AGENTS.md"}) || list[1].Target != nil {
		t.Fatalf("drafts = %+v", list)
	}
	if contents, err := w.DraftContents("AGENTS.md"); err != nil || contents != "new rules" {
		t.Fatalf("draft = %q, %v", contents, err)
	}
	if _, err := w.DraftContents("../drafts/AGENTS.md"); status(err) != http.StatusNotFound {
		t.Fatalf("draft outside the list: %v", err)
	}

	plugins := w.Plugins()
	if len(plugins) != 8 {
		t.Fatalf("plugins = %+v", plugins)
	}
	channel, mod, off, tool := plugins[0], plugins[1], plugins[2], plugins[3]
	mcpOff, t3, skill, hook := plugins[4], plugins[5], plugins[6], plugins[7]
	if t3.ID != "mcp:hex:t3" || t3.Kind != "mcp" || !*t3.Enabled || t3.Description != "http://t3/mcp" || *t3.State != "http" ||
		mcpOff.Name != "off" || *mcpOff.Enabled || *mcpOff.State != "stdio" || mcpOff.Description != "off-server" {
		t.Fatalf("mcp servers = %+v %+v", t3, mcpOff)
	}
	if skill.Kind != "skill" || skill.Name != "x" || skill.Description != "Read X as he sees it." || *skill.Scope != "user" {
		t.Fatalf("skill = %+v", skill)
	}
	if hook.Kind != "hook" || hook.Name != "PreToolUse" || hook.Description != "guard.sh" || *hook.Scope != "user" || *hook.State != "Bash" {
		t.Fatalf("hook = %+v", hook)
	}
	if channel.Kind != "channel" || *channel.Version != "0.2.0" || channel.Description != "Telegram channel" || *channel.State != "connected" ||
		*channel.Source != (Target{Repo: "core", Path: "channels/telegram"}) || channel.Enabled != nil {
		t.Fatalf("channel = %+v", channel)
	}
	if mod.ID != "mod:panel" || mod.Kind != "mod" || mod.Description != "A side panel." || *mod.Source != (Target{Repo: "core", Path: "mods/panel"}) {
		t.Fatalf("mod = %+v", mod)
	}
	if tool.Kind != "claude" || !*tool.Enabled || tool.Description != "A tool" || *tool.Version != "1.2.3" || *tool.State != "market" ||
		off.ID != "off@market" || *off.Enabled || *off.Scope != "project" {
		t.Fatalf("claude plugins = %+v %+v", tool, off)
	}
	if err := w.SetPlugin("telegram@hex", false); status(err) != http.StatusNotFound {
		t.Fatalf("switching a channel: %v", err)
	}
}

func TestWatchNamesChangedWorkspace(t *testing.T) {
	dir := sandbox(t)
	live := filepath.Join(dir, "live")
	sh(t, dir, "init", "-q", live)
	write(t, filepath.Join(live, "a.txt"), "a\n")
	sh(t, live, "add", "-A")
	sh(t, live, "commit", "-q", "-m", "first")
	var cfg config.Config
	cfg.Usage.Claude = filepath.Join(dir, "claude")
	cfg.Workspace.Repos = []config.WorkspaceRepo{{ID: "hex", Path: live}}
	cfg.Workspace.Worktrees = filepath.Join(dir, "worktrees")
	stale := make(chan []string, 64)
	w := New(Options{Config: cfg, Stale: func(tags ...string) { stale <- tags }})
	t.Cleanup(w.Close)
	id, err := w.Create("hex", "watch")
	if err != nil {
		t.Fatal(err)
	}
	wait := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case tags := <-stale:
				for _, tag := range tags {
					if tag == want {
						return
					}
				}
			case <-deadline:
				t.Fatalf("no stale %s", want)
			}
		}
	}
	if err := w.Write(id, "deep/new.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	wait("ws:" + id)
	if err := w.Write(id, "deep/again.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	wait("ws:" + id)
	if _, _, err := w.Commit(id, "watched"); err != nil {
		t.Fatal(err)
	}
	wait("repo:hex")
}

func TestErrorsCarryStatus(t *testing.T) {
	if statusOf(errors.New("boom")) != http.StatusInternalServerError || statusOf(refuse("no")) != http.StatusConflict || statusOf(notFound("no")) != http.StatusNotFound {
		t.Fatal("wrong statuses")
	}
}

func TestSetConfigFollowsReloads(t *testing.T) {
	w, live := setup(t, false)
	if _, err := w.executorOrMissing(); status(err) != http.StatusNotFound {
		t.Fatalf("executor without config: %v", err)
	}
	var cfg config.Config
	cfg.Workspace.Repos = []config.WorkspaceRepo{{ID: "renamed", Path: live}}
	cfg.Workspace.Executor = config.WorkspaceExecutor{API: "http://127.0.0.1:1", Token: "/nowhere", Apps: t.TempDir()}
	w.SetConfig(cfg)
	if _, err := w.executorOrMissing(); err != nil {
		t.Fatalf("executor after reload: %v", err)
	}
	if repos, _ := w.Repos(); len(repos) != 1 || repos[0].ID != "renamed" {
		t.Fatalf("repos after reload = %+v", repos)
	}
	cfg.Workspace.Executor = config.WorkspaceExecutor{}
	w.SetConfig(cfg)
	if _, err := w.executorOrMissing(); status(err) != http.StatusNotFound {
		t.Fatalf("executor after removing it: %v", err)
	}
}

func TestReadHistoryAndRaw(t *testing.T) {
	w, live := setup(t, false)
	write(t, filepath.Join(live, "b.txt"), "bee\n")
	write(t, filepath.Join(live, "pic.png"), "\x89PNG\r\n\x1a\n\x00data")
	sh(t, live, "add", "-A")
	sh(t, live, "commit", "-q", "-m", "add b and a picture")

	body, err := w.Read("hex", "a.txt")
	if err != nil || body.Size != 14 || body.Last == nil || body.Last.Subject != "first" {
		t.Fatalf("read a.txt = %+v %+v, %v", body, body.Last, err)
	}
	if body, _ := w.Read("hex", "pic.png"); !body.Binary || body.Last == nil || body.Last.Subject != "add b and a picture" {
		t.Fatalf("read pic.png = %+v", body)
	}
	write(t, filepath.Join(live, "new.txt"), "fresh\n")
	if body, _ := w.Read("hex", "new.txt"); body.Last != nil || body.Size != 6 {
		t.Fatalf("an uncommitted file has no last commit: %+v", body)
	}

	all, _ := w.Commits("hex", 10, "")
	onlyA, err := w.Commits("hex", 10, "a.txt")
	if err != nil || len(all) != 2 || len(onlyA) != 1 || onlyA[0].Subject != "first" {
		t.Fatalf("commits all=%d a.txt=%+v, %v", len(all), onlyA, err)
	}
	if _, err := w.Commits("hex", 10, "../x"); status(err) != http.StatusConflict {
		t.Fatalf("path outside: %v", err)
	}

	data, kind, err := w.Raw("hex", "pic.png")
	if err != nil || kind != "image/png" || !strings.HasPrefix(string(data), "\x89PNG") {
		t.Fatalf("raw = %q %s, %v", data, kind, err)
	}
	if _, _, err := w.Raw("hex", "a.txt"); status(err) != http.StatusConflict {
		t.Fatalf("raw text file: %v", err)
	}
}
