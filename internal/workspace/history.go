package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// maxPatch caps a patch the dashboard renders.
const maxPatch = 4_000_000

// Commit is one commit on a workspace's HEAD.
type Commit struct {
	SHA     string `json:"sha"`
	Short   string `json:"short"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
	At      int64  `json:"at"`
	Signed  bool   `json:"signed"`
}

// CommitDetail is a commit with its message body and patch.
type CommitDetail struct {
	Commit
	Body      string `json:"body"`
	Patch     string `json:"patch"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Diff is a workspace's whole change set as one git-style patch, and the paths left out of it.
type Diff struct {
	Patch   string   `json:"patch"`
	Omitted []string `json:"omitted"`
}

// parseRaw reads one commit printed by `git log --pretty=raw`.
func parseRaw(record string) (CommitDetail, bool) {
	header, message, _ := strings.Cut(strings.TrimLeft(record, "\n"), "\n\n")
	var c CommitDetail
	for _, line := range strings.Split(header, "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "commit":
			c.SHA, _, _ = strings.Cut(value, " ")
		case "author":
			if open := strings.LastIndex(value, " <"); open >= 0 {
				c.Author = value[:open]
			}
			if closed := strings.LastIndex(value, "> "); closed >= 0 {
				seconds, _ := strconv.ParseInt(strings.Fields(value[closed+2:])[0], 10, 64)
				c.At = seconds * 1000
			}
		case "gpgsig", "gpgsig-sha256":
			c.Signed = true
		}
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(message, "\n"), "\n") {
		lines = append(lines, strings.TrimPrefix(line, "    "))
	}
	text := strings.Join(lines, "\n")
	subject, body, _ := strings.Cut(text, "\n")
	c.Subject, c.Body = subject, strings.Trim(body, "\n")
	c.Short = c.SHA[:min(len(c.SHA), 7)]
	return c, c.SHA != ""
}

// Commits lists the newest commits on a workspace's HEAD.
func (w *Service) Commits(id string, limit int) ([]Commit, error) {
	_, ws, err := w.workspace(id)
	if err != nil {
		return nil, err
	}
	out, err := w.git(ws.Path, "log", "-z", "--pretty=raw", "--no-color", "-n", strconv.Itoa(limit), "HEAD")
	if err != nil {
		return nil, err
	}
	commits := []Commit{}
	for _, record := range split(out) {
		if c, ok := parseRaw(record); ok {
			commits = append(commits, c.Commit)
		}
	}
	return commits, nil
}

var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{4,64}$`)

// CommitDetail returns one commit with its patch; a merge shows its change against its first parent.
func (w *Service) CommitDetail(id, sha string) (CommitDetail, error) {
	_, ws, err := w.workspace(id)
	if err != nil {
		return CommitDetail{}, err
	}
	if !shaPattern.MatchString(sha) {
		return CommitDetail{}, notFound("No commit %s", sha)
	}
	out, err := w.git(ws.Path, "log", "-1", "--pretty=raw", "--no-color", sha+"^{commit}", "--")
	if err != nil {
		return CommitDetail{}, notFound("No commit %s", sha)
	}
	c, ok := parseRaw(out)
	if !ok {
		return CommitDetail{}, notFound("No commit %s", sha)
	}
	patch, err := w.git(ws.Path, "show", "--format=", "--patch", "--no-color", "--no-ext-diff", "--no-textconv",
		"--diff-merges=first-parent", "--src-prefix=a/", "--dst-prefix=b/", c.SHA, "--")
	if err != nil {
		return CommitDetail{}, err
	}
	if len(patch) > maxBytes {
		patch, c.Truncated = cutPatch(patch, maxBytes), true
	}
	c.Patch = patch
	return c, nil
}

// cutPatch keeps whole files of a patch up to limit bytes.
func cutPatch(patch string, limit int) string {
	var kept strings.Builder
	for _, file := range splitPatch(patch) {
		if kept.Len()+len(file) > limit {
			break
		}
		kept.WriteString(file)
	}
	return kept.String()
}

// splitPatch cuts a patch into its files, each starting at its "diff --git" line.
func splitPatch(patch string) []string {
	var files []string
	for len(patch) > 0 {
		next := strings.Index(patch[1:], "\ndiff --git ")
		if next < 0 {
			files = append(files, patch)
			break
		}
		files = append(files, patch[:next+2])
		patch = patch[next+2:]
	}
	return files
}

// Diff is the workspace's change set against its fork point, untracked files
// included as new files, up to 4 MB; binary or over-2 MB untracked files and
// whatever doesn't fit are named in Omitted.
func (w *Service) Diff(id string) (Diff, error) {
	_, ws, err := w.workspace(id)
	if err != nil {
		return Diff{}, err
	}
	fork, err := w.fork(ws)
	if err != nil {
		return Diff{}, err
	}
	options := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "--ignore-submodules", "--src-prefix=a/", "--dst-prefix=b/"}
	tracked, err := w.git(ws.Path, append(options, fork, "--")...)
	if err != nil {
		return Diff{}, err
	}
	result := Diff{Omitted: []string{}}
	var patch strings.Builder
	if len(tracked) <= maxPatch {
		patch.WriteString(tracked)
	} else {
		names, err := w.git(ws.Path, append(options, "--name-only", "-z", fork, "--")...)
		if err != nil {
			return Diff{}, err
		}
		paths, files := split(names), splitPatch(tracked)
		if len(paths) != len(files) {
			return Diff{}, fmt.Errorf("diff listed %d files but printed %d", len(paths), len(files))
		}
		for i, file := range files {
			if patch.Len()+len(file) > maxPatch {
				result.Omitted = append(result.Omitted, paths[i])
				continue
			}
			patch.WriteString(file)
		}
	}
	fresh, err := w.untracked(ws)
	if err != nil {
		return Diff{}, err
	}
	full := false
	for _, path := range fresh {
		if !full {
			file, ok := newFileDiff(ws.Path, path)
			full = ok && patch.Len()+len(file) > maxPatch
			if ok && !full {
				patch.WriteString(file)
				continue
			}
		}
		result.Omitted = append(result.Omitted, path)
	}
	result.Patch = patch.String()
	return result, nil
}

// newFileDiff writes an untracked file as git would show it added; false for a binary, over-2 MB or unreadable one.
func newFileDiff(dir, path string) (string, bool) {
	full := filepath.Join(dir, path)
	info, err := os.Lstat(full)
	if err != nil {
		return "", false
	}
	var data []byte
	mode := "100644"
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return "", false
		}
		data, mode = []byte(target), "120000"
	case !info.Mode().IsRegular() || info.Size() > maxBytes:
		return "", false
	default:
		if data, err = os.ReadFile(full); err != nil {
			return "", false
		}
		if info.Mode()&0o111 != 0 {
			mode = "100755"
		}
	}
	if isBinary(data) {
		return "", false
	}
	a, b := quoteName("a/"+path), quoteName("b/"+path)
	var out strings.Builder
	fmt.Fprintf(&out, "diff --git %s %s\nnew file mode %s\n", a, b, mode)
	if len(data) == 0 {
		return out.String(), true
	}
	tab := ""
	if strings.Contains(b, " ") {
		tab = "\t"
	}
	text := string(data)
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	count := ""
	if len(lines) != 1 {
		count = "," + strconv.Itoa(len(lines))
	}
	fmt.Fprintf(&out, "--- /dev/null\n+++ %s%s\n@@ -0,0 +1%s @@\n", b, tab, count)
	for _, line := range lines {
		out.WriteString("+" + line + "\n")
	}
	if !strings.HasSuffix(text, "\n") {
		out.WriteString("\\ No newline at end of file\n")
	}
	return out.String(), true
}

// quoteName quotes a path the way git's headers do when it holds a quote, backslash, control or non-ASCII byte.
func quoteName(name string) string {
	needs := false
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x20 || c >= 0x7f || c == '"' || c == '\\' {
			needs = true
			break
		}
	}
	if !needs {
		return name
	}
	escapes := map[byte]string{'\a': `\a`, '\b': `\b`, '\t': `\t`, '\n': `\n`, '\v': `\v`, '\f': `\f`, '\r': `\r`, '"': `\"`, '\\': `\\`}
	var out strings.Builder
	out.WriteByte('"')
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case escapes[c] != "":
			out.WriteString(escapes[c])
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&out, `\%03o`, c)
		default:
			out.WriteByte(c)
		}
	}
	out.WriteByte('"')
	return out.String()
}
