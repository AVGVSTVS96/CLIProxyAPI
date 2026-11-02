package usage

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// GitHub Actions runs that call Claude upload their accounting as claude-usage-* artifacts
// (autofork's .github/actions/usage step). The GitHub reader copies new ones into the
// store, one device per repository, from every repository the gh login owns.

const githubInterval = 5 * time.Minute

type githubReader struct {
	api    string
	token  func(context.Context) (string, error)
	client *http.Client
	store  *FileStore
	policy func() *PricingPolicy
	path   string

	mu    sync.Mutex
	state githubState
}

// githubState remembers, per repository, the listing a pass read in full (its ETag, so an
// unchanged listing costs GitHub nothing) and the artifacts already copied.
type githubState struct {
	ETags map[string]string  `json:"etags"`
	Read  map[string][]int64 `json:"read"`
}

type githubArtifact struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Expired   bool   `json:"expired"`
	CreatedAt string `json:"created_at"`
}

type githubUsage struct {
	Version    int     `json:"version"`
	Repository string  `json:"repository"`
	Events     []Event `json:"events"`
}

func ghToken(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func newGitHubReader(stateDir string, store *FileStore) *githubReader {
	r := &githubReader{
		api: "https://api.github.com", token: ghToken, client: &http.Client{Timeout: time.Minute},
		store: store, policy: store.Pricing().Policy, path: filepath.Join(stateDir, "github.json"),
	}
	if data, err := os.ReadFile(r.path); err == nil {
		_ = json.Unmarshal(data, &r.state)
	}
	if r.state.ETags == nil {
		r.state.ETags = map[string]string{}
	}
	if r.state.Read == nil {
		r.state.Read = map[string][]int64{}
	}
	return r
}

// run reads on start and every five minutes. GitHub only announces finished runs through
// webhooks, which would need a public address into this server.
func (r *githubReader) run(ctx context.Context) {
	var reported string
	for {
		problem := ""
		if err := r.pass(ctx); err != nil {
			problem = err.Error()
		}
		if problem != reported && ctx.Err() == nil {
			if problem != "" {
				log.Warnf("usage: GitHub Actions records: %s", problem)
			} else {
				log.Info("usage: GitHub Actions records are current again")
			}
			reported = problem
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(githubInterval):
		}
	}
}

func (r *githubReader) pass(ctx context.Context) error {
	token, err := r.token(ctx)
	if err != nil {
		return err
	}
	var repos []string
	_, err = r.pages(ctx, token, "/user/repos?affiliation=owner&per_page=100", "", func(body []byte) error {
		var page []struct {
			FullName string `json:"full_name"`
			Archived bool   `json:"archived"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		for _, repo := range page {
			if !repo.Archived {
				repos = append(repos, repo.FullName)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	jobs := make(chan string)
	var failed []string
	var failedMu sync.Mutex
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for repo := range jobs {
				if err := r.readRepo(ctx, token, repo); err != nil && ctx.Err() == nil {
					failedMu.Lock()
					failed = append(failed, fmt.Sprintf("%s (%v)", repo, err))
					failedMu.Unlock()
				}
			}
		}()
	}
	for _, repo := range repos {
		jobs <- repo
	}
	close(jobs)
	wg.Wait()
	r.mu.Lock()
	data, _ := json.Marshal(r.state)
	r.mu.Unlock()
	if err := writeFileAtomic(r.path, data); err != nil {
		return err
	}
	if len(failed) > 0 {
		slices.Sort(failed)
		return errors.New("could not read " + strings.Join(failed, ", "))
	}
	return nil
}

// readRepo copies a repository's unread usage artifacts, oldest first. Its listing's ETag
// is kept only once every artifact in it has been read.
func (r *githubReader) readRepo(ctx context.Context, token, repo string) error {
	r.mu.Lock()
	etag := r.state.ETags[repo]
	read := map[int64]bool{}
	for _, id := range r.state.Read[repo] {
		read[id] = true
	}
	r.mu.Unlock()
	var artifacts []githubArtifact
	listed, err := r.pages(ctx, token, "/repos/"+repo+"/actions/artifacts?per_page=100", etag, func(body []byte) error {
		var page struct {
			Artifacts []githubArtifact `json:"artifacts"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		for _, a := range page.Artifacts {
			if strings.HasPrefix(a.Name, "claude-usage-") && !a.Expired {
				artifacts = append(artifacts, a)
			}
		}
		return nil
	})
	if errors.Is(err, errNotModified) {
		return nil
	}
	if err != nil {
		return err
	}
	slices.SortFunc(artifacts, func(a, b githubArtifact) int { return strings.Compare(a.CreatedAt, b.CreatedAt) })
	var kept []int64
	var problem error
	for _, a := range artifacts {
		if !read[a.ID] {
			if err := r.copyArtifact(ctx, token, repo, a); err != nil {
				problem = err
				continue
			}
		}
		kept = append(kept, a.ID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(kept) > 0 {
		r.state.Read[repo] = kept
	} else {
		delete(r.state.Read, repo)
	}
	if problem == nil {
		r.state.ETags[repo] = listed
	} else {
		delete(r.state.ETags, repo)
	}
	return problem
}

func (r *githubReader) copyArtifact(ctx context.Context, token, repo string, a githubArtifact) error {
	archive, err := r.get(ctx, token, fmt.Sprintf("%s/repos/%s/actions/artifacts/%d/zip", r.api, repo, a.ID), "")
	if err != nil {
		return err
	}
	z, err := zip.NewReader(bytes.NewReader(archive.body), int64(len(archive.body)))
	if err != nil {
		return err
	}
	file, err := z.Open("claude-usage.json")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	var doc githubUsage
	if err = json.NewDecoder(io.LimitReader(file, 256<<20)).Decode(&doc); err != nil {
		return err
	}
	if doc.Version != 1 || !strings.EqualFold(doc.Repository, repo) {
		return fmt.Errorf("artifact %d is not %s's version 1 usage", a.ID, repo)
	}
	if len(doc.Events) == 0 {
		return nil
	}
	policy := r.policy()
	for i := range doc.Events {
		e := &doc.Events[i]
		if e.ServiceTier == nil && e.ReportedCostUSD == nil {
			standard := "standard"
			e.ServiceTier = &standard
		}
		*e, _ = policy.Reprice(*e)
	}
	device := Device{ID: "github:" + repo, Label: "GitHub " + repo}
	if created, err := time.Parse(time.RFC3339, a.CreatedAt); err == nil {
		ms := created.UnixMilli()
		device.SyncedAt = &ms
	}
	_, err = r.store.Apply(Batch{Device: device, Events: doc.Events}, true)
	return err
}

var (
	errNotModified = errors.New("not modified")
	nextPage       = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)
)

// pages calls each with every page of a listing and returns the first page's ETag. Given
// that page's earlier ETag, an unchanged listing returns errNotModified.
func (r *githubReader) pages(ctx context.Context, token, path, etag string, each func([]byte) error) (string, error) {
	url, listed := r.api+path, ""
	for first := true; url != ""; first = false {
		sent := ""
		if first {
			sent = etag
		}
		page, err := r.get(ctx, token, url, sent)
		if err != nil {
			return "", err
		}
		if page.status == http.StatusNotModified {
			return "", errNotModified
		}
		if first {
			listed = page.etag
		}
		if err = each(page.body); err != nil {
			return "", err
		}
		url = ""
		if m := nextPage.FindStringSubmatch(page.link); m != nil {
			url = m[1]
		}
	}
	return listed, nil
}

type githubResponse struct {
	status     int
	body       []byte
	etag, link string
}

func (r *githubReader) get(ctx context.Context, token, url, etag string) (githubResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return githubResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return githubResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return githubResponse{}, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotModified {
		return githubResponse{}, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	return githubResponse{resp.StatusCode, body, resp.Header.Get("ETag"), resp.Header.Get("Link")}, nil
}
