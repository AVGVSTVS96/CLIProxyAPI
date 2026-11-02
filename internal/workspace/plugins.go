package workspace

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Draft is a proposed file and the repo file it would replace.
type Draft struct {
	Name   string  `json:"name"`
	Target *Target `json:"target"`
}

// Target is a file in a configured repo.
type Target struct {
	Repo string `json:"repo"`
	Path string `json:"path"`
}

// Drafts lists the drafts folder's files with their targets.
func (w *Service) Drafts() []Draft {
	s := w.current()
	drafts := []Draft{}
	if s.drafts == "" {
		return drafts
	}
	entries, _ := os.ReadDir(s.drafts)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || !entry.Type().IsRegular() {
			continue
		}
		draft := Draft{Name: entry.Name()}
		if repo, path, ok := strings.Cut(s.draftTargets[entry.Name()], ":"); ok && repo != "" && path != "" {
			draft.Target = &Target{Repo: repo, Path: path}
		}
		drafts = append(drafts, draft)
	}
	return drafts
}

// DraftContents reads one draft by its listed name.
func (w *Service) DraftContents(name string) (string, error) {
	listed := slices.ContainsFunc(w.Drafts(), func(d Draft) bool { return d.Name == name })
	if !listed {
		return "", notFound("No draft %s", name)
	}
	data, err := os.ReadFile(filepath.Join(w.current().drafts, name))
	return string(data), err
}

// Plugin is a hex channel, a hex mod or a Claude Code plugin.
type Plugin struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Version     *string `json:"version"`
	Description string  `json:"description"`
	Enabled     *bool   `json:"enabled"`
	Scope       *string `json:"scope"`
	Source      *Target `json:"source"`
	State       *string `json:"state"`
}

type install struct {
	Scope       string `json:"scope"`
	Version     string `json:"version"`
	InstallPath string `json:"installPath"`
}

type manifest struct {
	Version     string `json:"version"`
	Description string `json:"description"`
}

func readJSON(path string, into any) {
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, into)
	}
}

func text(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// sourceOf names the configured repo file a folder lives in, preferring the innermost repo.
func (s settings) sourceOf(path string) *Target {
	var best *repo
	for i, r := range s.repos {
		if strings.HasPrefix(path, r.Path+string(filepath.Separator)) && (best == nil || len(r.Path) > len(best.Path)) {
			best = &s.repos[i]
		}
	}
	if best == nil {
		return nil
	}
	rel, _ := filepath.Rel(best.Path, path)
	return &Target{Repo: best.ID, Path: filepath.ToSlash(rel)}
}

// Plugins lists hex's channels, hex's mods, then the other Claude Code plugins.
func (w *Service) Plugins() []Plugin {
	s := w.current()
	var installed struct {
		Plugins map[string][]install `json:"plugins"`
	}
	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	var marketplaces map[string]struct {
		InstallLocation string `json:"installLocation"`
	}
	readJSON(filepath.Join(s.claude, "plugins", "installed_plugins.json"), &installed)
	readJSON(filepath.Join(s.claude, "settings.json"), &settings)
	readJSON(filepath.Join(s.claude, "plugins", "known_marketplaces.json"), &marketplaces)
	channels := marketplaces["hex"].InstallLocation
	if channels == "" {
		channels = filepath.Join(s.hex, ".hex", "channels")
	}

	ids := make([]string, 0, len(installed.Plugins))
	for id := range installed.Plugins {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	list := []Plugin{}
	var claude []Plugin
	for _, id := range ids {
		var first install
		if installs := installed.Plugins[id]; len(installs) > 0 {
			first = installs[0]
		}
		name, market, _ := strings.Cut(id, "@")
		if market == "hex" {
			dir := filepath.Join(channels, name)
			var m manifest
			readJSON(filepath.Join(dir, ".claude-plugin", "plugin.json"), &m)
			version := m.Version
			if version == "" {
				version = first.Version
			}
			state := "installed"
			if stateDir := filepath.Join(s.hex, "state", name); exists(filepath.Join(stateDir, "hub.sock")) {
				state = "connected"
			} else if exists(stateDir) {
				state = "set up"
			}
			p := Plugin{ID: id, Name: name, Kind: "channel", Version: text(version), Description: m.Description, State: &state}
			if exists(dir) {
				p.Source = s.sourceOf(dir)
			}
			list = append(list, p)
			continue
		}
		var m manifest
		if first.InstallPath != "" {
			readJSON(filepath.Join(first.InstallPath, ".claude-plugin", "plugin.json"), &m)
		}
		enabled := settings.EnabledPlugins[id]
		claude = append(claude, Plugin{ID: id, Name: name, Kind: "claude", Version: text(first.Version), Description: m.Description,
			Enabled: &enabled, Scope: text(first.Scope), State: text(market)})
	}

	modsDir := filepath.Join(s.hex, ".hex", "mods")
	entries, _ := os.ReadDir(modsDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(modsDir, entry.Name())
		version, description := describeMod(dir)
		list = append(list, Plugin{ID: "mod:" + entry.Name(), Name: entry.Name(), Kind: "mod", Version: text(version),
			Description: description, Source: s.sourceOf(dir)})
	}
	return append(list, claude...)
}

// describeMod reads a mod's version and description from its plugin manifest or package.json, or its README's first line.
func describeMod(dir string) (version, description string) {
	for _, path := range []string{filepath.Join(dir, ".claude-plugin", "plugin.json"), filepath.Join(dir, "package.json")} {
		var m manifest
		readJSON(path, &m)
		if version == "" {
			version = m.Version
		}
		if description == "" {
			description = m.Description
		}
	}
	if description != "" {
		return version, description
	}
	file, err := os.Open(filepath.Join(dir, "README.md"))
	if err != nil {
		return version, ""
	}
	defer func() { _ = file.Close() }()
	for scanner := bufio.NewScanner(file); scanner.Scan(); {
		if line := strings.TrimSpace(scanner.Text()); line != "" && !strings.HasPrefix(line, "#") {
			return version, line
		}
	}
	return version, ""
}

// SetPlugin turns an installed Claude Code plugin on or off for the user.
func (w *Service) SetPlugin(id string, enabled bool) error {
	s := w.current()
	if !slices.ContainsFunc(w.Plugins(), func(p Plugin) bool { return p.ID == id && p.Kind == "claude" }) {
		return notFound("No Claude Code plugin %s", id)
	}
	verb := "disable"
	if enabled {
		verb = "enable"
	}
	ctx, cancel := context.WithTimeout(w.ctx, time.Minute)
	defer cancel()
	var env []string
	if home, err := os.UserHomeDir(); err == nil && s.claude != filepath.Join(home, ".claude") {
		env = []string{"CLAUDE_CONFIG_DIR=" + s.claude}
	}
	_, err := run(ctx, "", env, nil, "claude", "plugin", verb, id, "--scope", "user", "--json")
	return err
}
