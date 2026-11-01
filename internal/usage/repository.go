package usage

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/text/unicode/norm"
)

var errProtected = errors.New("repository path unavailable")

// repositoryResolver finds each project's git origin. On macOS it never touches the
// folders that ask the user for access (Documents, Desktop, Downloads, iCloud), resolving
// symlinks one component at a time so a link can't lead there either.
type repositoryResolver struct {
	darwin   bool
	roots    []string
	nonLinks map[string]bool
	deadline time.Time
}

var protectedNames = []string{"Documents", "Desktop", "Downloads", "Library/Mobile Documents", "Library/CloudStorage"}

func canonicalPath(file string) string {
	abs, _ := filepath.Abs(file)
	lower := strings.ToLower(norm.NFD.String(abs))
	if lower == "/system/volumes/data" || strings.HasPrefix(lower, "/system/volumes/data/") {
		lower = lower[len("/system/volumes/data"):]
	}
	return lower
}

func newRepositoryResolver(home string) *repositoryResolver {
	r := &repositoryResolver{darwin: runtime.GOOS == "darwin", nonLinks: map[string]bool{}, deadline: time.Now().Add(5 * time.Second)}
	if !r.darwin {
		return r
	}
	for _, name := range protectedNames {
		r.roots = append(r.roots, canonicalPath(filepath.Join(home, name)))
	}
	if physical, err := r.safePath(home); err == nil {
		for _, name := range protectedNames {
			r.roots = append(r.roots, canonicalPath(filepath.Join(physical, name)))
		}
	}
	return r
}

func (r *repositoryResolver) blocked(file string) bool {
	c := canonicalPath(file)
	for _, root := range r.roots {
		if c == root || strings.HasPrefix(c, root+"/") {
			return true
		}
	}
	return false
}

func (r *repositoryResolver) safePath(file string) (string, error) {
	resolved, _ := filepath.Abs(file)
	if !r.darwin {
		return resolved, nil
	}
	for links := 0; links < 40; links++ {
		if r.blocked(resolved) {
			return "", errProtected
		}
		parts := strings.Split(strings.TrimPrefix(resolved, "/"), "/")
		component := "/"
		followed := false
		for i, part := range parts {
			if part == "" {
				continue
			}
			component = filepath.Join(component, part)
			if r.blocked(component) {
				return "", errProtected
			}
			if r.nonLinks[component] {
				continue
			}
			target, err := os.Readlink(component)
			if err == nil {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(component), target)
				}
				resolved = filepath.Join(append([]string{target}, parts[i+1:]...)...)
				followed = true
				break
			}
			if errors.Is(err, os.ErrNotExist) {
				return resolved, nil
			}
			var pathErr *os.PathError
			if errors.As(err, &pathErr) && isNotLink(pathErr) {
				r.nonLinks[component] = true
				continue
			}
			return "", errProtected
		}
		if !followed {
			return resolved, nil
		}
	}
	return "", errProtected
}

// canonicalRepository strips transport and credentials so SSH and HTTPS remotes of one
// repository match: "github.com/owner/repo".
func canonicalRepository(remote string) string {
	trimmed := strings.TrimSpace(remote)
	host, path := "", ""
	if m := scpRemote.FindStringSubmatch(trimmed); m != nil && !strings.Contains(trimmed, "://") {
		host, path = m[1], m[2]
	} else {
		u, err := url.Parse(trimmed)
		if err != nil {
			return ""
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git":
		default:
			return ""
		}
		host, path = u.Hostname(), u.EscapedPath()
	}
	path = strings.Trim(path, "/")
	if strings.HasSuffix(strings.ToLower(path), ".git") {
		path = path[:len(path)-4]
	}
	if host == "" || path == "" {
		return ""
	}
	host = strings.ToLower(host)
	switch host {
	case "github.com", "gitlab.com", "bitbucket.org":
		path = strings.ToLower(path)
	}
	return host + "/" + path
}

var (
	scpRemote     = regexp.MustCompile(`^(?:[^@/]+@)?([^/:]+):([^/].*)$`)
	remoteSection = regexp.MustCompile(`^\s*\[\s*remote\s+"([^"]+)"\s*\]`)
	anySection    = regexp.MustCompile(`^\s*\[`)
	urlLine       = regexp.MustCompile(`^\s*url\s*=\s*(.*)$`)
	gitdirLine    = regexp.MustCompile(`(?m)^gitdir:\s*(.+)$`)
)

func originRemote(config string) string {
	origin := false
	for _, line := range strings.Split(config, "\n") {
		if m := remoteSection.FindStringSubmatch(line); m != nil {
			origin = m[1] == "origin"
			continue
		}
		if anySection.MatchString(line) {
			origin = false
		}
		if origin {
			if m := urlLine.FindStringSubmatch(line); m != nil {
				return canonicalRepository(m[1])
			}
		}
	}
	return ""
}

func readMetadata(file string) (string, bool) {
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	return string(data), true
}

// resolve returns the repository a working directory belongs to: its origin remote,
// "local:<dir>" for a repository without one, or nil outside git.
func (r *repositoryResolver) resolve(cwd string) *string {
	if cwd == "" || cwd == "Unknown project" || time.Now().After(r.deadline) {
		return nil
	}
	if r.darwin && r.blocked(cwd) {
		return nil
	}
	directory, err := r.safePath(cwd)
	if err != nil {
		return nil
	}
	for depth := 0; depth < 100; depth++ {
		git, err := r.safePath(filepath.Join(directory, ".git"))
		if err != nil {
			return nil
		}
		if info, statErr := os.Stat(git); statErr == nil {
			gitDir := git
			if !info.IsDir() {
				if !info.Mode().IsRegular() {
					return nil
				}
				pointer, ok := readMetadata(git)
				if !ok {
					return nil
				}
				m := gitdirLine.FindStringSubmatch(pointer)
				if m == nil {
					return nil
				}
				target := strings.TrimSpace(m[1])
				if !filepath.IsAbs(target) {
					target = filepath.Join(directory, target)
				}
				if gitDir, err = r.safePath(target); err != nil {
					return nil
				}
			}
			commondir, err := r.safePath(filepath.Join(gitDir, "commondir"))
			if err != nil {
				return nil
			}
			if common, ok := readMetadata(commondir); ok {
				target := strings.TrimSpace(common)
				if !filepath.IsAbs(target) {
					target = filepath.Join(gitDir, target)
				}
				if gitDir, err = r.safePath(target); err != nil {
					return nil
				}
			}
			configPath, err := r.safePath(filepath.Join(gitDir, "config"))
			if err != nil {
				return nil
			}
			identity := "local:" + filepath.Dir(gitDir)
			if config, ok := readMetadata(configPath); ok {
				if origin := originRemote(config); origin != "" {
					identity = origin
				}
			}
			return &identity
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return nil
		}
		directory = parent
	}
	return nil
}

func isNotLink(err *os.PathError) bool { return errors.Is(err.Err, syscall.EINVAL) }
