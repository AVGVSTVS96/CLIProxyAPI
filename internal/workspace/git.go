package workspace

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// branchPrefix names the branches worktrees edit on; branch.<name>.editbase records what each started from.
const branchPrefix = "edit/"

// RepoSummary is a repo, its live checkout's branch and its worktrees.
type RepoSummary struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	About      string      `json:"about"`
	Path       string      `json:"path"`
	Review     bool        `json:"review"`
	Branch     *string     `json:"branch"`
	Unpushed   *int        `json:"unpushed"`
	Workspaces []Workspace `json:"workspaces"`
}

// Workspace is a repo's live checkout (Name nil, read-only here) or one of its worktrees.
type Workspace struct {
	ID     string  `json:"id"`
	Repo   string  `json:"repo"`
	Name   *string `json:"name"`
	Path   string  `json:"path"`
	Branch *string `json:"branch"`
	Base   *string `json:"base"`
}

// Change is a file that differs from the workspace's base.
type Change struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// Changes are a workspace's files against its base, and how far its branch is from the base.
type Changes struct {
	Files  []Change `json:"files"`
	Dirty  bool     `json:"dirty"`
	Ahead  int      `json:"ahead"`
	Behind int      `json:"behind"`
}

// FileBody is a file's text and its text at the workspace's base.
type FileBody struct {
	Path     string  `json:"path"`
	Contents *string `json:"contents"`
	Original *string `json:"original"`
	Binary   bool    `json:"binary"`
	TooLarge bool    `json:"tooLarge"`
}

// Shipped is what a ship did: the live branch it fast-forwarded, or the pull request it opened.
type Shipped struct {
	Branch  string `json:"branch"`
	Commits int    `json:"commits"`
	Head    string `json:"head,omitempty"`
	PR      string `json:"pr,omitempty"`
}

// run runs a command with extra environment; a failure becomes a refusal carrying the command's own words.
func run(ctx context.Context, dir string, env []string, stdin []byte, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"), env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(errOut.String())
		if message == "" {
			message = strings.TrimSpace(out.String())
		}
		if message == "" {
			message = name + " " + args[0] + ": " + err.Error()
		}
		return out.String(), refuse("%s", message)
	}
	return out.String(), nil
}

func (w *Service) git(dir string, args ...string) (string, error) {
	return run(w.ctx, dir, nil, nil, "git", args...)
}

// try runs git and reports only whether it succeeded.
func (w *Service) try(dir string, args ...string) (string, bool) {
	out, err := w.git(dir, args...)
	return out, err == nil
}

func split(out string) []string {
	return slices.DeleteFunc(strings.Split(out, "\x00"), func(s string) bool { return s == "" })
}

func (w *Service) repo(id string) (repo, error) {
	for _, r := range w.current().repos {
		if r.ID == id {
			return r, nil
		}
	}
	return repo{}, notFound("No repo named %s", id)
}

func (w *Service) home(r repo) string {
	return filepath.Join(w.current().worktrees, r.ID)
}

func (w *Service) liveBranch(r repo) *string {
	out, ok := w.try(r.Path, "symbolic-ref", "--short", "-q", "HEAD")
	if branch := strings.TrimSpace(out); ok && branch != "" {
		return &branch
	}
	return nil
}

// workspaces lists the live checkout first, then the worktrees this workspace made.
func (w *Service) workspaces(r repo) ([]Workspace, error) {
	live := Workspace{ID: r.ID, Repo: r.ID, Path: r.Path, Branch: w.liveBranch(r)}
	out, err := w.git(r.Path, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	home := w.home(r)
	list := []Workspace{live}
	for _, block := range strings.Split(out, "\x00\x00") {
		fields := map[string]string{}
		for _, line := range split(block) {
			key, value, _ := strings.Cut(line, " ")
			fields[key] = value
		}
		path, ref := fields["worktree"], fields["branch"]
		if filepath.Dir(path) != home || ref == "" {
			continue
		}
		name := filepath.Base(path)
		branch := strings.TrimPrefix(ref, "refs/heads/")
		base := live.Branch
		if out, ok := w.try(r.Path, "config", "--get", "branch."+branch+".editbase"); ok && strings.TrimSpace(out) != "" {
			recorded := strings.TrimSpace(out)
			base = &recorded
		}
		list = append(list, Workspace{ID: r.ID + "~" + name, Repo: r.ID, Name: &name, Path: path, Branch: &branch, Base: base})
	}
	return list, nil
}

// Repos summarizes every configured repo.
func (w *Service) Repos() ([]RepoSummary, error) {
	repos := w.current().repos
	out := make([]RepoSummary, len(repos))
	errs := make([]error, len(repos))
	var wg sync.WaitGroup
	for i, r := range repos {
		wg.Go(func() { out[i], errs[i] = w.summary(r) })
	}
	wg.Wait()
	return out, errors.Join(errs...)
}

func (w *Service) summary(r repo) (RepoSummary, error) {
	list, err := w.workspaces(r)
	if err != nil {
		return RepoSummary{}, err
	}
	summary := RepoSummary{ID: r.ID, Name: r.Name, About: r.About, Path: r.Path, Review: r.Review, Branch: list[0].Branch, Workspaces: list}
	if out, ok := w.try(r.Path, "rev-list", "--count", "@{upstream}..HEAD"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(out)); err == nil {
			summary.Unpushed = &n
		}
	}
	return summary, nil
}

func (w *Service) workspace(id string) (repo, Workspace, error) {
	repoID, _, _ := strings.Cut(id, "~")
	r, err := w.repo(repoID)
	if err != nil {
		return repo{}, Workspace{}, err
	}
	list, err := w.workspaces(r)
	if err != nil {
		return repo{}, Workspace{}, err
	}
	for _, ws := range list {
		if ws.ID == id {
			return r, ws, nil
		}
	}
	return repo{}, Workspace{}, notFound("No workspace %s", id)
}

// writable finds a worktree; the live checkout is read-only here.
func (w *Service) writable(id string) (repo, Workspace, error) {
	r, ws, err := w.workspace(id)
	if err == nil && ws.Name == nil {
		err = refuse("The live checkout is read-only here. Start a worktree to edit.")
	}
	return r, ws, err
}

var notSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(name string) string {
	s := strings.Trim(notSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 48 {
		s = strings.TrimRight(s[:48], "-")
	}
	return s
}

// Create starts a worktree on a new edit/ branch from the live checkout's branch.
func (w *Service) Create(repoID, name string) (string, error) {
	r, err := w.repo(repoID)
	if err != nil {
		return "", err
	}
	defer w.lock(r.ID)()
	home := w.home(r)
	stem := slug(name)
	if stem == "" {
		stem = "edit"
	}
	clean := stem
	for n := 2; ; n++ {
		_, taken := w.try(r.Path, "show-ref", "--verify", "--quiet", "refs/heads/"+branchPrefix+clean)
		if !taken && !exists(filepath.Join(home, clean)) {
			break
		}
		clean = stem + "-" + strconv.Itoa(n)
	}
	start := ""
	if live := w.liveBranch(r); live != nil {
		start = *live
	} else {
		out, err := w.git(r.Path, "rev-parse", "HEAD")
		if err != nil {
			return "", err
		}
		start = strings.TrimSpace(out)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return "", err
	}
	branch := branchPrefix + clean
	if _, err := w.git(r.Path, "worktree", "add", "-b", branch, filepath.Join(home, clean), start); err != nil {
		return "", err
	}
	if _, err := w.git(r.Path, "config", "branch."+branch+".editbase", start); err != nil {
		return "", err
	}
	w.resync()
	return r.ID + "~" + clean, nil
}

// Discard removes a worktree, and its branch once the base has every commit on it.
func (w *Service) Discard(id string) (*string, error) {
	r, ws, err := w.writable(id)
	if err != nil {
		return nil, err
	}
	defer w.lock(r.ID)()
	if _, err := w.git(r.Path, "worktree", "remove", "--force", ws.Path); err != nil {
		return nil, err
	}
	w.resync()
	if ws.Base != nil {
		if _, merged := w.try(r.Path, "merge-base", "--is-ancestor", *ws.Branch, *ws.Base); merged {
			if _, err := w.git(r.Path, "branch", "-D", *ws.Branch); err != nil {
				return nil, err
			}
			return nil, nil
		}
	}
	return ws.Branch, nil
}

// fork is what a workspace's changes are against: where its branch left the base, or HEAD for the live checkout.
func (w *Service) fork(ws Workspace) (string, error) {
	if ws.Name == nil || ws.Base == nil {
		return "HEAD", nil
	}
	out, err := w.git(ws.Path, "merge-base", "HEAD", *ws.Base)
	return strings.TrimSpace(out), err
}

// untracked lists files git doesn't track or ignore; a nested repo shows as a folder and is left out.
func (w *Service) untracked(ws Workspace) ([]string, error) {
	out, err := w.git(ws.Path, "ls-files", "-z", "-o", "--exclude-standard")
	return slices.DeleteFunc(split(out), func(p string) bool { return strings.HasSuffix(p, "/") }), err
}

// Files lists tracked files, minus submodules, and untracked ones.
func (w *Service) Files(id string) ([]string, error) {
	_, ws, err := w.workspace(id)
	if err != nil {
		return nil, err
	}
	tracked, err := w.git(ws.Path, "ls-files", "-z", "--stage")
	if err != nil {
		return nil, err
	}
	fresh, err := w.untracked(ws)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range split(tracked) {
		meta, path, _ := strings.Cut(entry, "\t")
		if !strings.HasPrefix(meta, "160000") {
			paths = append(paths, path)
		}
	}
	paths = append(paths, fresh...)
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

var statusNames = map[string]string{"A": "added", "D": "deleted"}

// Changes lists what differs from the fork point, with line counts, and the branch's distance from its base.
func (w *Service) Changes(id string) (Changes, error) {
	_, ws, err := w.workspace(id)
	if err != nil {
		return Changes{}, err
	}
	return w.changes(ws)
}

func (w *Service) changes(ws Workspace) (Changes, error) {
	fork, err := w.fork(ws)
	if err != nil {
		return Changes{}, err
	}
	diff := []string{"diff", "-z", "--no-renames", "--ignore-submodules", "--no-ext-diff", "--no-textconv"}
	numstat, err := w.git(ws.Path, append(diff, "--numstat", fork)...)
	if err != nil {
		return Changes{}, err
	}
	names, err := w.git(ws.Path, append(diff, "--name-status", fork)...)
	if err != nil {
		return Changes{}, err
	}
	porcelain, err := w.git(ws.Path, "status", "--porcelain", "--ignore-submodules")
	if err != nil {
		return Changes{}, err
	}
	fresh, err := w.untracked(ws)
	if err != nil {
		return Changes{}, err
	}

	statuses := map[string]string{}
	for pairs := split(names); len(pairs) >= 2; pairs = pairs[2:] {
		statuses[pairs[1]] = pairs[0]
	}
	files := []Change{}
	for _, line := range split(numstat) {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		removed, _ := strconv.Atoi(parts[1])
		status := statusNames[statuses[parts[2]]]
		if status == "" {
			status = "modified"
		}
		files = append(files, Change{Path: parts[2], Status: status, Added: added, Removed: removed})
	}
	for _, path := range fresh {
		files = append(files, Change{Path: path, Status: "untracked", Added: w.lines.count(filepath.Join(ws.Path, path))})
	}
	slices.SortFunc(files, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })

	changes := Changes{Files: files, Dirty: strings.TrimSpace(porcelain) != ""}
	if ws.Name != nil && ws.Base != nil {
		out, err := w.git(ws.Path, "rev-list", "--left-right", "--count", *ws.Base+"...HEAD")
		if err != nil {
			return Changes{}, err
		}
		if counts := strings.Fields(out); len(counts) == 2 {
			changes.Behind, _ = strconv.Atoi(counts[0])
			changes.Ahead, _ = strconv.Atoi(counts[1])
		}
	}
	return changes, nil
}

// clean checks a workspace-relative path: inside the workspace and never in .git.
func clean(path string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(path))
	if path == "" || filepath.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		slices.Contains(strings.Split(rel, string(filepath.Separator)), ".git") {
		return "", refuse("Path outside the workspace: %s", path)
	}
	return rel, nil
}

// isBinary is git's test: a NUL byte in the first 8000.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0
}

// Read returns a file and its text at the fork point.
func (w *Service) Read(id, path string) (FileBody, error) {
	_, ws, err := w.workspace(id)
	if err != nil {
		return FileBody{}, err
	}
	rel, err := clean(path)
	if err != nil {
		return FileBody{}, err
	}
	body := FileBody{Path: path}
	root, err := os.OpenRoot(ws.Path)
	if err != nil {
		return FileBody{}, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return FileBody{}, refuse("Can't read %s: %v", path, err)
	case info.IsDir():
		return FileBody{}, refuse("%s is a folder", path)
	case info.Size() > maxBytes:
		body.TooLarge = true
		return body, nil
	default:
		data, err := root.ReadFile(rel)
		if err != nil {
			return FileBody{}, refuse("Can't read %s: %v", path, err)
		}
		if isBinary(data) {
			body.Binary = true
			return body, nil
		}
		text := string(data)
		body.Contents = &text
	}
	fork, err := w.fork(ws)
	if err != nil {
		return FileBody{}, err
	}
	if out, ok := w.try(ws.Path, "cat-file", "blob", fork+":"+filepath.ToSlash(rel)); ok && len(out) <= maxBytes && !isBinary([]byte(out)) {
		body.Original = &out
	}
	return body, nil
}

// Write saves a file in a worktree.
func (w *Service) Write(id, path string, contents []byte) error {
	_, ws, err := w.writable(id)
	if err != nil {
		return err
	}
	rel, err := clean(path)
	if err != nil {
		return err
	}
	if len(contents) > maxBytes {
		return refuse("%s is over 2 MB, too large to save here", path)
	}
	root, err := os.OpenRoot(ws.Path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return refuse("Can't save %s: %v", path, err)
		}
	}
	if err := root.WriteFile(rel, contents, 0o644); err != nil {
		return refuse("Can't save %s: %v", path, err)
	}
	return nil
}

// Commit commits every edit in a worktree, signed.
func (w *Service) Commit(id, message string) (sha string, signed bool, err error) {
	r, ws, err := w.writable(id)
	if err != nil {
		return "", false, err
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return "", false, refuse("Write a commit message first.")
	}
	defer w.lock(r.ID)()
	if _, err := w.git(ws.Path, "add", "-A"); err != nil {
		return "", false, err
	}
	if _, same := w.try(ws.Path, "diff", "--cached", "--quiet"); same {
		return "", false, refuse("Nothing to commit.")
	}
	if _, err := run(w.ctx, ws.Path, nil, []byte(message), "git", "commit", "-S", "-F", "-"); err != nil {
		return "", false, err
	}
	out, err := w.git(ws.Path, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	sha = strings.TrimSpace(out)
	return sha, w.signed(ws.Path, sha), nil
}

// signed reports whether a commit object carries a signature.
func (w *Service) signed(dir, sha string) bool {
	out, ok := w.try(dir, "cat-file", "commit", sha)
	header, _, _ := strings.Cut(out, "\n\n")
	return ok && strings.Contains(header, "\ngpgsig")
}

// signedRange lists the commits in base..HEAD, refusing when any is unsigned.
func (w *Service) signedRange(dir, base string) (int, error) {
	out, err := w.git(dir, "rev-list", base+"..HEAD")
	if err != nil {
		return 0, err
	}
	commits := strings.Fields(out)
	var unsigned []string
	for _, sha := range commits {
		if !w.signed(dir, sha) {
			unsigned = append(unsigned, sha[:min(len(sha), 12)])
		}
	}
	if len(unsigned) > 0 {
		return 0, refuse("Unsigned commits, not shipped: %s", strings.Join(unsigned, ", "))
	}
	return len(commits), nil
}

// Ship lands a worktree's commits: a review repo gets a pull request; any other
// fast-forwards its live branch, rebasing the worktree first if the live branch moved.
func (w *Service) Ship(id string) (Shipped, error) {
	r, ws, err := w.writable(id)
	if err != nil {
		return Shipped{}, err
	}
	defer w.lock(r.ID)()
	changes, err := w.changes(ws)
	switch {
	case err != nil:
		return Shipped{}, err
	case changes.Dirty:
		return Shipped{}, refuse("Commit your edits before shipping.")
	case changes.Ahead == 0:
		return Shipped{}, refuse("Nothing to ship yet.")
	case ws.Base == nil:
		return Shipped{}, refuse("This worktree has no base branch to ship to.")
	}
	base := *ws.Base
	commits, err := w.signedRange(ws.Path, base)
	if err != nil {
		return Shipped{}, err
	}
	if r.Review {
		return w.propose(ws, base, commits)
	}
	live := w.liveBranch(r)
	if live == nil || *live != base {
		on := "a detached HEAD"
		if live != nil {
			on = *live
		}
		return Shipped{}, refuse("The live checkout is on %s, not %s. Ship it by hand.", on, base)
	}
	if changes.Behind > 0 {
		// --gpg-sign re-signs the rebased commits, which a plain rebase would leave unsigned.
		if _, err := w.git(ws.Path, "rebase", "--gpg-sign", base); err != nil {
			_, _ = w.try(ws.Path, "rebase", "--abort")
			return Shipped{}, refuse("%s moved and the rebase hit a conflict, so nothing shipped. %s", base, err.Error())
		}
	}
	if changes.Behind > 0 {
		if commits, err = w.signedRange(ws.Path, base); err != nil {
			return Shipped{}, err
		}
	}
	if _, err := w.git(r.Path, "merge", "--ff-only", *ws.Branch); err != nil {
		return Shipped{}, err
	}
	head, err := w.git(r.Path, "rev-parse", "--short", "HEAD")
	if err != nil {
		return Shipped{}, err
	}
	return Shipped{Branch: base, Commits: commits, Head: strings.TrimSpace(head)}, nil
}

// propose pushes a worktree's branch and returns its open pull request, opening one if none is.
func (w *Service) propose(ws Workspace, base string, commits int) (Shipped, error) {
	branch := *ws.Branch
	if _, err := w.git(ws.Path, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return Shipped{}, err
	}
	out, err := run(w.ctx, ws.Path, nil, nil, "gh", "pr", "list", "--head", branch, "--state", "open", "--json", "url", "--jq", ".[0].url // empty")
	if err != nil {
		return Shipped{}, err
	}
	pr := strings.TrimSpace(out)
	if pr == "" {
		out, err := run(w.ctx, ws.Path, nil, nil, "gh", "pr", "create", "--base", base, "--head", branch, "--fill")
		if err != nil {
			return Shipped{}, err
		}
		lines := strings.Fields(out)
		if len(lines) > 0 {
			pr = lines[len(lines)-1]
		}
	}
	return Shipped{Branch: base, Commits: commits, PR: pr}, nil
}

// Push pushes a repo's live branch; review repos change only through pull requests.
func (w *Service) Push(repoID string) error {
	r, err := w.repo(repoID)
	if err != nil {
		return err
	}
	if r.Review {
		return refuse("%s changes go through pull requests. Ship a worktree instead.", r.Name)
	}
	defer w.lock(r.ID)()
	_, err = w.git(r.Path, "push")
	return err
}

// lineCache counts untracked files' lines, remembering each by size and modification time.
type lineCache struct {
	mu    sync.Mutex
	known map[string]lineCount
}

type lineCount struct {
	size  int64
	mtime time.Time
	lines int
}

func (c *lineCache) count(path string) int {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return 0
	}
	c.mu.Lock()
	known, ok := c.known[path]
	c.mu.Unlock()
	if ok && known.size == info.Size() && known.mtime.Equal(info.ModTime()) {
		return known.lines
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	lines := countLines(data)
	c.mu.Lock()
	if c.known == nil {
		c.known = map[string]lineCount{}
	}
	c.known[path] = lineCount{size: info.Size(), mtime: info.ModTime(), lines: lines}
	c.mu.Unlock()
	return lines
}

// countLines counts lines as git's numstat does: a binary file has none, and a last line without a newline still counts.
func countLines(data []byte) int {
	if len(data) == 0 || isBinary(data) {
		return 0
	}
	n := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}
