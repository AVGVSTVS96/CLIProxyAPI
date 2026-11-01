package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// t3Metadata is what T3 Code knows about a harness session: its thread, title and project.
type t3Metadata struct {
	SessionTitle    string
	ProjectName     string
	T3ThreadID      string
	T3ThreadURL     string
	RepositoryPaths []string
}

const t3MaximumRows = 20_000

// readT3Metadata reads T3 Code's state databases, keyed "harness:nativeSessionId", through
// the sqlite3 CLI in read-only mode. Only allowlisted columns and JSON properties leave
// SQLite. Any failure means no metadata, never a failed collection.
func readT3Metadata(ctx context.Context, directory, baseURL string) map[string]t3Metadata {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	metadata := map[string]t3Metadata{}
	environmentID := ""
	if info, err := os.Stat(filepath.Join(directory, "environment-id")); err == nil && info.Mode().IsRegular() && info.Size() <= 512 {
		if data, err := os.ReadFile(filepath.Join(directory, "environment-id")); err == nil {
			environmentID = t3Text(string(data), 512)
		}
	}
	deleted := map[string]bool{}
	for _, name := range []string{"statev2.sqlite", "state.sqlite"} {
		file := filepath.Join(directory, name)
		info, err := os.Stat(file)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 2<<30 {
			continue
		}
		result, err := readT3Database(ctx, file, environmentID, baseURL, deleted)
		if err != nil {
			continue
		}
		for key, value := range result.values {
			if _, ok := metadata[key]; !ok {
				metadata[key] = value
			}
		}
	}
	return metadata
}

func t3Text(value any, maximum int) string {
	s, ok := value.(string)
	if !ok || jsLength(s) > maximum {
		return ""
	}
	return jsTrim(s)
}

func sqliteQuery(ctx context.Context, file, query string) ([]map[string]any, error) {
	cmd := exec.CommandContext(ctx, "sqlite3", "-readonly", "-json", "-cmd", ".timeout 100", file, query)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var rows []map[string]any
	if strings.TrimSpace(out.String()) == "" {
		return rows, nil
	}
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func t3Harness(driver any) string {
	switch driver {
	case "claudeAgent", "claude":
		return "claude"
	case "codex", "grok", "pi", "muse":
		return driver.(string)
	}
	return ""
}

func encodeURIComponent(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// t3ThreadURL is the shared web route of a thread in the environment that owns it.
func t3ThreadURL(base, environmentID, threadID string) string {
	if environmentID == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.HasSuffix(base, "?") || strings.HasSuffix(base, "#") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/") + "/" +
		encodeURIComponent(environmentID) + "/" + encodeURIComponent(threadID)
}

type t3Result struct{ values map[string]t3Metadata }

func readT3Database(ctx context.Context, file, environmentID, baseURL string, deleted map[string]bool) (t3Result, error) {
	result := t3Result{values: map[string]t3Metadata{}}
	tables := []string{
		"orchestration_v2_projection_provider_threads", "orchestration_v2_projection_threads", "projection_projects",
		"projection_threads", "projection_thread_sessions", "provider_session_runtime",
	}
	schema, err := sqliteQuery(ctx, file, `SELECT m.name AS t, p.name AS c FROM sqlite_master m, pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name IN ('`+strings.Join(tables, "','")+`')`)
	if err != nil {
		return result, err
	}
	columns := map[string]map[string]bool{}
	for _, row := range schema {
		table, _ := row["t"].(string)
		column, _ := row["c"].(string)
		if columns[table] == nil {
			columns[table] = map[string]bool{}
		}
		columns[table][column] = true
	}
	has := func(table string, required ...string) bool {
		for _, column := range required {
			if !columns[table][column] {
				return false
			}
		}
		return true
	}
	query := func(q string) []map[string]any {
		rows, err := sqliteQuery(ctx, file, q)
		if err != nil {
			return nil
		}
		return rows
	}
	limit := fmt.Sprint(t3MaximumRows)
	if has("orchestration_v2_projection_threads", "thread_id", "deleted_at") {
		for _, row := range query(`SELECT thread_id AS threadId FROM orchestration_v2_projection_threads
			WHERE deleted_at IS NOT NULL LIMIT ` + limit) {
			if id := t3Text(row["threadId"], 512); id != "" {
				deleted[id] = true
			}
		}
	}
	insert := func(row map[string]any, legacy bool) {
		provider := t3Harness(row["driver"])
		nativeID := t3Text(row["nativeId"], 512)
		threadID := t3Text(row["threadId"], 512)
		if provider == "" || nativeID == "" || threadID == "" {
			return
		}
		key := provider + ":" + nativeID
		if _, ok := result.values[key]; ok {
			return
		}
		if value, ok := row["deletedAt"]; ok && value != nil {
			deleted[threadID] = true
		}
		if deleted[threadID] {
			result.values[key] = t3Metadata{T3ThreadID: threadID}
			return
		}
		if legacy && len(deleted) >= t3MaximumRows {
			return
		}
		m := t3Metadata{SessionTitle: t3Text(row["title"], 1024), T3ThreadID: threadID,
			T3ThreadURL: t3ThreadURL(baseURL, environmentID, threadID)}
		if project := t3Text(row["projectName"], 1024); project != "No project" {
			m.ProjectName = project
		}
		for _, path := range []string{t3Text(row["worktreePath"], 4096), t3Text(row["workspaceRoot"], 4096)} {
			if path != "" && (len(m.RepositoryPaths) == 0 || m.RepositoryPaths[0] != path) {
				m.RepositoryPaths = append(m.RepositoryPaths, path)
			}
		}
		result.values[key] = m
	}
	projectsAvailable := has("projection_projects", "project_id", "title", "workspace_root")
	projectFields, projectJoin := "NULL AS projectName, NULL AS workspaceRoot", ""
	if projectsAvailable {
		projectFields = "p.title AS projectName, p.workspace_root AS workspaceRoot"
		projectJoin = "LEFT JOIN projection_projects p ON p.project_id = t.project_id"
	}
	if has("orchestration_v2_projection_provider_threads", "thread_id", "payload_json") &&
		has("orchestration_v2_projection_threads", "thread_id", "project_id", "title", "payload_json") {
		deletedField := "NULL AS deletedAt"
		if columns["orchestration_v2_projection_threads"]["deleted_at"] {
			deletedField = "t.deleted_at AS deletedAt"
		}
		ordering, inner := "", ""
		if columns["orchestration_v2_projection_provider_threads"]["updated_at"] {
			ordering, inner = "ORDER BY pt.updated_at DESC", ", updated_at"
		}
		for _, row := range query(`SELECT substr(json_extract(pt.payload_json, '$.nativeThreadRef.nativeId'), 1, 513) AS nativeId,
			json_extract(pt.payload_json, '$.nativeThreadRef.driver') AS driver,
			t.thread_id AS threadId, t.title AS title, ` + deletedField + `, ` + projectFields + `,
			CASE WHEN length(t.payload_json) <= 262144 AND json_valid(t.payload_json)
				THEN substr(json_extract(t.payload_json, '$.worktreePath'), 1, 4097) END AS worktreePath
			FROM (SELECT thread_id, payload_json ` + inner + `
				FROM orchestration_v2_projection_provider_threads ` + strings.Replace(ordering, "pt.", "", 1) + `
				LIMIT ` + limit + `) pt
			JOIN orchestration_v2_projection_threads t ON t.thread_id = pt.thread_id
			` + projectJoin + `
			WHERE length(pt.payload_json) <= 262144 AND json_valid(pt.payload_json)
			` + ordering + ` LIMIT ` + limit) {
			if id := t3Text(row["threadId"], 512); id != "" && row["deletedAt"] != nil {
				deleted[id] = true
			}
			insert(row, false)
		}
	}
	if has("projection_threads", "thread_id", "project_id", "title") {
		tombstones := "NULL"
		if len(deleted) >= t3MaximumRows {
			ids := make([]string, 0, len(deleted))
			for id := range deleted {
				ids = append(ids, id)
			}
			data, _ := json.Marshal(ids)
			tombstones = "'" + strings.ReplaceAll(string(data), "'", "''") + "'"
		}
		legacyDeleted, deletedField, worktree := "0", "NULL AS deletedAt", "NULL"
		if columns["projection_threads"]["deleted_at"] {
			legacyDeleted, deletedField = "t.deleted_at IS NOT NULL", "t.deleted_at AS deletedAt"
		}
		if columns["projection_threads"]["worktree_path"] {
			worktree = "t.worktree_path"
		}
		common := "t.thread_id AS threadId, t.title AS title, " + deletedField + ", " + projectFields + ", " + worktree + " AS worktreePath"
		filter := "(" + tombstones + " IS NULL OR t.thread_id IN (SELECT value FROM json_each(" + tombstones + ")) OR " + legacyDeleted + ")"
		if has("provider_session_runtime", "thread_id", "provider_name", "resume_cursor_json") {
			for _, row := range query(`SELECT COALESCE(json_extract(r.resume_cursor_json, '$.threadId'), json_extract(r.resume_cursor_json, '$.sessionId')) AS nativeId,
				r.provider_name AS driver, ` + common + `
				FROM provider_session_runtime r JOIN projection_threads t ON t.thread_id = r.thread_id ` + projectJoin + `
				WHERE json_valid(r.resume_cursor_json) AND ` + filter + ` LIMIT ` + limit) {
				insert(row, true)
			}
		}
		if has("projection_thread_sessions", "thread_id", "provider_name", "provider_thread_id", "provider_session_id") {
			for _, row := range query(`SELECT COALESCE(s.provider_thread_id, s.provider_session_id) AS nativeId, s.provider_name AS driver, ` + common + `
				FROM projection_thread_sessions s JOIN projection_threads t ON t.thread_id = s.thread_id ` + projectJoin + `
				WHERE COALESCE(s.provider_thread_id, s.provider_session_id) IS NOT NULL AND ` + filter + ` LIMIT ` + limit) {
				insert(row, true)
			}
		}
	}
	return result, nil
}
