package workspace

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The rest of what a hex session loads besides plugins: MCP servers, skills and hooks, each as a
// Plugin of its own kind, with Scope "hex" for hex's project settings and "user" for the user's.

type mcpServer struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	Command string `json:"command"`
}

type claudeSettings struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
	EnabledMcpjsonServers []string `json:"enabledMcpjsonServers"`
	DeniedMcpServers      []struct {
		ServerName string `json:"serverName"`
	} `json:"deniedMcpServers"`
}

// userConfig is Claude Code's own state file, which holds the user's MCP servers.
func (s settings) userConfig() string {
	if home, err := os.UserHomeDir(); err == nil && s.claude == filepath.Join(home, ".claude") {
		return filepath.Join(home, ".claude.json")
	}
	return filepath.Join(s.claude, ".claude.json")
}

func (s settings) setup() []Plugin {
	var user, hex claudeSettings
	readJSON(filepath.Join(s.claude, "settings.json"), &user)
	readJSON(filepath.Join(s.hex, ".claude", "settings.json"), &hex)
	list := s.mcpServers(hex)
	list = append(list, s.skills(filepath.Join(s.hex, ".claude", "skills"), "hex")...)
	list = append(list, s.skills(filepath.Join(s.claude, "skills"), "user")...)
	list = append(list, hooks(hex, "hex", filepath.Join(s.hex, ".claude", "settings.json"), s)...)
	return append(list, hooks(user, "user", filepath.Join(s.claude, "settings.json"), s)...)
}

func (s settings) mcpServers(hex claudeSettings) []Plugin {
	var project struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}
	var global struct {
		MCPServers map[string]mcpServer `json:"mcpServers"`
	}
	projectFile := filepath.Join(s.hex, ".mcp.json")
	readJSON(projectFile, &project)
	readJSON(s.userConfig(), &global)
	denied := map[string]bool{}
	for _, d := range hex.DeniedMcpServers {
		denied[d.ServerName] = true
	}
	var list []Plugin
	add := func(servers map[string]mcpServer, scope string, source *Target, approved func(string) bool) {
		names := make([]string, 0, len(servers))
		for name := range servers {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			server := servers[name]
			where := server.URL
			if where == "" {
				where = server.Command
			}
			kind := server.Type
			if kind == "" {
				kind = "stdio"
			}
			on := approved(name) && !denied[name]
			list = append(list, Plugin{ID: "mcp:" + scope + ":" + name, Name: name, Kind: "mcp", Description: where,
				Enabled: &on, Scope: text(scope), Source: source, State: text(kind)})
		}
	}
	add(project.MCPServers, "hex", s.sourceOf(projectFile), func(name string) bool {
		return slices.Contains(hex.EnabledMcpjsonServers, name)
	})
	add(global.MCPServers, "user", nil, func(string) bool { return true })
	return list
}

// skills lists a skills folder, each with the description its SKILL.md front matter gives.
func (s settings) skills(dir, scope string) []Plugin {
	entries, _ := os.ReadDir(dir)
	var list []Plugin
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		file := filepath.Join(path, "SKILL.md")
		if !exists(file) {
			continue
		}
		p := Plugin{ID: "skill:" + scope + ":" + entry.Name(), Name: entry.Name(), Kind: "skill",
			Description: frontMatter(file, "description"), Scope: text(scope)}
		if real, err := filepath.EvalSymlinks(file); err == nil {
			p.Source = s.sourceOf(real)
		}
		list = append(list, p)
	}
	return list
}

// frontMatter reads one key from a Markdown file's leading YAML block, folded to one line.
func frontMatter(path, key string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return ""
	}
	var value []string
	inValue := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		if inValue {
			if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				value = append(value, strings.TrimSpace(line))
				continue
			}
			break
		}
		if rest, ok := strings.CutPrefix(line, key+":"); ok {
			rest = strings.TrimSpace(rest)
			inValue = true
			if rest != "" && rest != "|" && rest != ">" && rest != "|-" && rest != ">-" {
				value = append(value, strings.Trim(rest, `"'`))
			}
		}
	}
	return strings.Join(value, " ")
}

// hooks lists each hook event with the commands it runs.
func hooks(from claudeSettings, scope, file string, s settings) []Plugin {
	events := make([]string, 0, len(from.Hooks))
	for event := range from.Hooks {
		events = append(events, event)
	}
	slices.Sort(events)
	source := s.sourceOf(file)
	if real, err := filepath.EvalSymlinks(file); err == nil && source == nil {
		source = s.sourceOf(real)
	}
	var list []Plugin
	for _, event := range events {
		var commands, matchers []string
		for _, group := range from.Hooks[event] {
			if group.Matcher != "" {
				matchers = append(matchers, group.Matcher)
			}
			for _, hook := range group.Hooks {
				commands = append(commands, hook.Command)
			}
		}
		list = append(list, Plugin{ID: "hook:" + scope + ":" + event, Name: event, Kind: "hook",
			Description: strings.Join(commands, "\n"), Scope: text(scope), State: text(strings.Join(matchers, " · ")), Source: source})
	}
	return list
}
