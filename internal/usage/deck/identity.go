package deck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Who is who a usage row belongs to. Threads are keyed by app and thread id; a
// name alone never joins.
type Who struct {
	Kind     string  `json:"kind"`
	App      string  `json:"app,omitempty"`
	Thread   string  `json:"thread,omitempty"`
	Name     string  `json:"name,omitempty"`
	Owner    *string `json:"owner,omitempty"`
	Unit     string  `json:"unit,omitempty"`
	Title    string  `json:"title,omitempty"`
	Project  string  `json:"project,omitempty"`
	Repo     string  `json:"repo,omitempty"`
	Workflow string  `json:"workflow,omitempty"`
	Run      *string `json:"run,omitempty"`
	Href     *string `json:"href,omitempty"`
}

// MarshalJSON writes exactly the fields each kind has, nulls included.
func (w Who) MarshalJSON() ([]byte, error) {
	switch w.Kind {
	case "thread":
		return json.Marshal(struct {
			Kind   string  `json:"kind"`
			App    string  `json:"app"`
			Thread string  `json:"thread"`
			Name   string  `json:"name"`
			Href   *string `json:"href"`
		}{w.Kind, w.App, w.Thread, w.Name, w.Href})
	case "job":
		return json.Marshal(struct {
			Kind  string  `json:"kind"`
			Owner *string `json:"owner"`
			Unit  string  `json:"unit"`
		}{w.Kind, w.Owner, w.Unit})
	case "session":
		return json.Marshal(struct {
			Kind    string  `json:"kind"`
			Title   string  `json:"title"`
			Project string  `json:"project"`
			Href    *string `json:"href"`
		}{w.Kind, w.Title, w.Project, w.Href})
	case "action":
		return json.Marshal(struct {
			Kind     string  `json:"kind"`
			Repo     string  `json:"repo"`
			Workflow string  `json:"workflow"`
			Run      *string `json:"run"`
			Href     *string `json:"href"`
		}{w.Kind, w.Repo, w.Workflow, w.Run, w.Href})
	}
	return json.Marshal(struct {
		Kind    string `json:"kind"`
		Project string `json:"project"`
	}{w.Kind, w.Project})
}

type thread struct{ App, Thread, Name string }

// Names are hex's threads, for naming sessions.
type Names struct {
	// Thread by its current Claude session, from `hex threads`.
	current map[string]thread
	// Thread by the first 8 characters of a past session id, from `hex log`
	// messages whose meta names the session's log.
	logged map[string]thread
	// Threads by name, across apps.
	named map[string][]thread
}

type sessionRef struct {
	sessionID  string
	title      *string
	project    string
	repository *string
}

var (
	jobTitle = regexp.MustCompile(`^(.+)/([\w.@-]+)$`)
	// tokenhub's GitHub import names each session `github:<run>:<attempt>:…` and titles it `<workflow> #<run>`.
	runSession  = regexp.MustCompile(`^github:(\d+):`)
	runNumber   = regexp.MustCompile(` #\d+$`)
	githubRepo  = regexp.MustCompile(`^https://github\.com/`)
	pastLogName = regexp.MustCompile(`-([0-9a-f]{8})\.md$`)
)

func ptr[T any](value T) *T { return &value }

func threadLink(app, id string) *string {
	switch app {
	case "t3":
		return ptr("t3-thread://v1/" + id)
	case "telegram":
		chat, topic, _ := strings.Cut(id, ":")
		internal := strings.TrimPrefix(chat, "-100")
		if topic != "" {
			return ptr("https://t.me/c/" + internal + "/" + topic)
		}
		return ptr("https://t.me/c/" + internal)
	}
	return nil
}

func projectOf(project string, repository *string) string {
	if repository != nil {
		return *repository
	}
	return project
}

func basename(p string) string {
	if p == "" {
		return ""
	}
	trimmed := strings.TrimRight(p, "/")
	if trimmed == "" {
		return ""
	}
	return path.Base(trimmed)
}

// whoOf names a session, in order: a GitHub Actions run, by tokenhub's session
// id; its thread's current session; a past session of a thread, by hex's log; a
// title that names exactly one thread; a bin/job title `<owner>/<unit>`; any
// other title; else its project. A name alone never joins two threads.
func whoOf(names *Names, s sessionRef) Who {
	project := projectOf(s.project, s.repository)
	if run := runSession.FindStringSubmatch(s.sessionID); run != nil {
		repo := githubRepo.ReplaceAllString(project, "")
		workflow := "workflow"
		if s.title != nil {
			if name := strings.TrimSpace(runNumber.ReplaceAllString(*s.title, "")); name != "" {
				workflow = name
			}
		}
		return Who{Kind: "action", Repo: repo, Workflow: workflow, Run: &run[1], Href: ptr("https://github.com/" + repo + "/actions/runs/" + run[1])}
	}
	title := ""
	if s.title != nil {
		title = strings.TrimSpace(*s.title)
	}
	found, ok := names.current[s.sessionID]
	if !ok {
		found, ok = names.logged[s.sessionID[:min(8, len(s.sessionID))]]
	}
	if !ok && title != "" && len(names.named[title]) == 1 {
		found, ok = names.named[title][0], true
	}
	if ok {
		return Who{Kind: "thread", App: found.App, Thread: found.Thread, Name: found.Name, Href: threadLink(found.App, found.Thread)}
	}
	if job := jobTitle.FindStringSubmatch(title); job != nil {
		return Who{Kind: "job", Owner: &job[1], Unit: job[2]}
	}
	if title != "" {
		return Who{Kind: "session", Title: title, Project: project}
	}
	return Who{Kind: "project", Project: project}
}

// rowOf is the agent row a session ranks under.
func rowOf(who Who, sessionKey string) string {
	switch who.Kind {
	case "thread":
		return "t:" + who.App + ":" + who.Thread
	case "job":
		owner := ""
		if who.Owner != nil {
			owner = *who.Owner
		}
		return "j:" + owner + "/" + who.Unit
	case "session":
		return "s:" + sessionKey
	case "action":
		return "g:" + who.Repo + ":" + who.Workflow
	}
	return "p:" + who.Project
}

// agentOf is the agent a session ranks under: an Actions run's agent is its
// workflow, which links to the repo's runs.
func agentOf(who Who) Who {
	if who.Kind == "action" {
		who.Run = nil
		who.Href = ptr("https://github.com/" + who.Repo + "/actions")
	}
	return who
}

func labelOf(who Who) (string, string) {
	switch who.Kind {
	case "thread":
		return who.Name, who.App
	case "job":
		if who.Owner != nil {
			return who.Unit, "job of " + *who.Owner
		}
		return who.Unit, "job"
	case "session":
		return who.Title, basename(who.Project)
	case "action":
		return basename(who.Repo) + " · " + who.Workflow, "GitHub Actions"
	}
	if name := basename(who.Project); name != "" {
		return name, "untitled sessions"
	}
	return who.Project, "untitled sessions"
}

func appOf(who Who) string {
	switch who.Kind {
	case "thread":
		return who.App
	case "job":
		return "job"
	case "action":
		return "actions"
	}
	return "other"
}

// hexJSON runs `hex <command> --json`, so the views never read hex.db's tables.
func hexJSON(ctx context.Context, hex, command string, out any) error {
	cmd := exec.CommandContext(ctx, filepath.Join(hex, ".hex", "bin", "hex"), command, "--json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return fmt.Errorf("hex %s: %s", command, message)
		}
		return fmt.Errorf("hex %s: %w", command, err)
	}
	return json.Unmarshal(data, out)
}

// readPast maps each past session's first 8 characters to its thread, from
// messages whose meta names the session's log. Only hex's old log import wrote
// those, so hex's whole log is read once, not at every change.
func readPast(ctx context.Context, hex string) (map[string][2]string, error) {
	var rows []struct {
		To   string `json:"to"`
		Meta struct {
			App string `json:"app"`
			Log string `json:"log"`
		} `json:"meta"`
	}
	if err := hexJSON(ctx, hex, "log", &rows); err != nil {
		return nil, err
	}
	past := map[string][2]string{}
	for _, row := range rows {
		if id := pastLogName.FindStringSubmatch(row.Meta.Log); id != nil && row.Meta.App != "" {
			past[id[1]] = [2]string{row.Meta.App, row.To}
		}
	}
	return past, nil
}

func loadNames(ctx context.Context, hex string, past map[string][2]string) (*Names, error) {
	var rows []struct {
		App     string `json:"app"`
		Thread  string `json:"thread"`
		Name    string `json:"name"`
		Session string `json:"session"`
	}
	if err := hexJSON(ctx, hex, "threads", &rows); err != nil {
		return nil, err
	}
	names := &Names{current: map[string]thread{}, logged: map[string]thread{}, named: map[string][]thread{}}
	threads := map[[2]string]thread{}
	var order [][2]string
	for _, row := range rows {
		t := thread{App: row.App, Thread: row.Thread, Name: row.Name}
		key := [2]string{row.App, row.Thread}
		if _, seen := threads[key]; !seen {
			order = append(order, key)
		}
		threads[key] = t
		if row.Session != "" {
			names.current[row.Session] = t
		}
	}
	for id, key := range past {
		if t, ok := threads[key]; ok {
			names.logged[id] = t
		}
	}
	for _, key := range order {
		t := threads[key]
		names.named[t.Name] = append(names.named[t.Name], t)
	}
	return names, nil
}
