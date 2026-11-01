package usage

import (
	"bytes"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

type grokSession struct {
	sessionID, project, model, parentID, forkedAt, kind, signature string
	ledger, updates                                                string
	parsed                                                         *parsedFile
}

// grokCache keeps parsed Grok accounting files by path and signature.
type grokCache struct {
	mu    sync.Mutex
	files map[string]cachedFile
}

func newGrokCache() *grokCache { return &grokCache{files: map[string]cachedFile{}} }

func grokTurnID(session string, turn int64, model string) string {
	return "grok:turn:" + encodeURIComponent(session) + ":" + strconv.FormatInt(turn, 10) + ":" + encodeURIComponent(model)
}

func grokTurnNumber(e *Event) int64 {
	parts := strings.Split(e.ID, ":")
	if len(parts) < 4 {
		return 0
	}
	n, _ := strconv.ParseInt(parts[3], 10, 64)
	return n
}

func tokenCount(v gjson.Result) bool {
	return v.Type == gjson.Number && v.Num >= 0 && v.Num <= 9007199254740991 && v.Num == math.Trunc(v.Num)
}

func count(v gjson.Result) int64 {
	if tokenCount(v) {
		return int64(v.Num)
	}
	return 0
}

func incompleteFlag(v gjson.Result) bool { return v.Exists() && v.Type != gjson.False }

func grokTimestamp(v gjson.Result) string {
	switch v.Type {
	case gjson.Number:
		if v.Num > 1e11 {
			return isoMillis(v.Num)
		}
		return isoMillis(v.Num * 1000)
	case gjson.String:
		return isoMillis(parseDate(v.Str))
	}
	return ""
}

func appendGrokUsage(parsed *parsedFile, usage gjson.Result, s *grokSession, turn int64, stamp, fallbackModel string) {
	type row struct {
		model string
		value gjson.Result
	}
	var rows []row
	modelUsage := usage.Get("modelUsage")
	if modelUsage.IsObject() {
		modelUsage.ForEach(func(key, value gjson.Result) bool {
			rows = append(rows, row{strings.Clone(key.Str), value})
			return true
		})
	}
	if len(rows) == 0 {
		model := text(usage.Get("primaryModelId"))
		if model == "" {
			model = fallbackModel
		}
		if model == "" {
			model = "unknown"
		}
		rows = []row{{model, usage}}
	}
	for _, r := range rows {
		f := fields(r.value, "inputTokens", "outputTokens", "cachedReadTokens", "cacheCreationTokens", "reasoningTokens",
			"modelCalls", "costUsdTicks", "costIsPartial", "usageIsIncomplete")
		if !tokenCount(f[0]) || !tokenCount(f[1]) {
			parsed.Malformed++
			continue
		}
		invalid := false
		for _, optional := range f[2:6] {
			if optional.Exists() && !tokenCount(optional) {
				invalid = true
			}
		}
		if invalid {
			parsed.Malformed++
			continue
		}
		input, output := count(f[0]), count(f[1])
		cacheRead := min(input, count(f[2]))
		cacheWrite := min(input-cacheRead, count(f[3]))
		if input == 0 && output == 0 {
			continue
		}
		model := r.model
		if model == "" {
			model = "unknown"
		}
		e := Event{
			ID: grokTurnID(s.sessionID, turn, model), Timestamp: stamp, Harness: "grok", SessionID: s.sessionID,
			Model: model, Project: s.project, InputTokens: input - cacheRead - cacheWrite, OutputTokens: output,
			CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite, ReasoningTokens: min(output, count(f[4])),
		}
		if !incompleteFlag(f[7]) && !incompleteFlag(f[8]) && !incompleteFlag(usage.Get("usageIsIncomplete")) &&
			tokenCount(f[6]) && f[6].Num > 0 {
			cost := f[6].Num / 1e10
			e.CostUSD, e.CostKnown, e.ReportedCostUSD = cost, true, &cost
		}
		if tokenCount(f[5]) {
			calls := count(f[5])
			e.Requests = &calls
		}
		parsed.Events = append(parsed.Events, &candidate{Event: e})
	}
}

func parseGrokLedger(data []byte, s *grokSession) *parsedFile {
	parsed := &parsedFile{SessionID: s.sessionID}
	ledger := gjson.ParseBytes(data)
	turns := ledger.Get("turns")
	if !ledger.IsObject() || !turns.IsArray() || (text(ledger.Get("sessionId")) != "" && text(ledger.Get("sessionId")) != s.sessionID) {
		parsed.Malformed++
		return parsed
	}
	for _, row := range turns.Array() {
		turn := count(row.Get("turnNumber"))
		stamp := grokTimestamp(row.Get("endedAt"))
		if turn == 0 || stamp == "" {
			parsed.Malformed++
			continue
		}
		appendGrokUsage(parsed, row, s, turn, stamp, s.model)
	}
	return parsed
}

func parseGrokUpdates(data []byte, s *grokSession) *parsedFile {
	parsed := &parsedFile{SessionID: s.sessionID}
	var turn int64
	model := s.model
	completed := map[string]int64{}
	_ = readLines(bytes.NewReader(data), func(raw []byte, _ int, complete bool) {
		raw = validUTF8(raw)
		if len(bytes.TrimFunc(raw, jsSpace)) == 0 {
			return
		}
		if !gjson.ValidBytes(raw) {
			if complete {
				parsed.Malformed++
			}
			return
		}
		record := gjson.ParseBytes(raw)
		params := record.Get("params")
		update := params.Get("update")
		if !record.IsObject() || !params.IsObject() || !update.IsObject() {
			update = gjson.Result{}
		}
		if m := text(update.Get("_meta").Get("modelId")); m != "" {
			model = m
		}
		if !isString(update.Get("sessionUpdate"), "turn_completed") {
			return
		}
		nativeID := text(params.Get("_meta").Get("eventId"))
		existing, seen := completed[nativeID]
		if nativeID == "" {
			seen = false
		}
		ordinal := existing
		if !seen {
			turn++
			ordinal = turn
		}
		if nativeID != "" {
			completed[nativeID] = ordinal
		}
		usage := update.Get("usage")
		if !usage.Exists() {
			if seen {
				parsed.Malformed++
			}
			return
		}
		stamp := grokTimestamp(params.Get("_meta").Get("agentTimestampMs"))
		if stamp == "" {
			stamp = grokTimestamp(record.Get("timestamp"))
		}
		if stamp == "" {
			parsed.Malformed++
			return
		}
		if !usage.IsObject() {
			usage = gjson.Parse("{}")
		}
		replacement := &parsedFile{}
		appendGrokUsage(replacement, usage, s, ordinal, stamp, model)
		parsed.Malformed += replacement.Malformed
		if !seen || replacement.Malformed == 0 {
			if seen {
				kept := parsed.Events[:0]
				for _, entry := range parsed.Events {
					if grokTurnNumber(&entry.Event) != ordinal {
						kept = append(kept, entry)
					}
				}
				parsed.Events = kept
			}
			parsed.Events = append(parsed.Events, replacement.Events...)
		}
	}, func() { parsed.Malformed++ })
	return parsed
}

func regularFile(file string) bool {
	info, err := os.Lstat(file)
	return err == nil && info.Mode().IsRegular()
}

func subdirectories(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if entry.Type().IsDir() {
			out = append(out, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func readBounded(file string, limit int64) ([]byte, string, bool) {
	info, err := os.Stat(file)
	if err != nil || info.Size() > limit {
		return nil, "", false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "", false
	}
	return data, fileSignature(info), true
}

// collect returns Grok's parsed sessions. Parent turns already include their subagents,
// so subagent sessions only prove ownership; forks keep inherited turns on the parent.
func (g *grokCache) collect(roots []string) ([]*parsedFile, []string) {
	var sessions []*grokSession
	parents := map[string]string{}
	var warnings []string
	for _, root := range roots {
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		for _, group := range subdirectories(real) {
			groupProject, groupSignature := "", ""
			if decoded, err := url.PathUnescape(filepath.Base(group)); err == nil && filepath.IsAbs(decoded) {
				groupProject = decoded
			}
			if groupProject == "" && regularFile(filepath.Join(group, ".cwd")) {
				if data, signature, ok := readBounded(filepath.Join(group, ".cwd"), 16*1024); ok {
					groupProject, groupSignature = jsTrim(string(data)), signature
				}
			}
			for _, directory := range subdirectories(group) {
				summary := gjson.Result{}
				summarySignature := "missing"
				if file := filepath.Join(directory, "summary.json"); regularFile(file) {
					if data, signature, ok := readBounded(file, 1<<20); ok && gjson.ValidBytes(data) {
						summary, summarySignature = gjson.ParseBytes(data), signature
					}
				}
				s := &grokSession{
					sessionID: text(summary.Get("info.id")),
					project:   text(summary.Get("info.cwd")),
					model:     text(summary.Get("current_model_id")),
					parentID:  text(summary.Get("parent_session_id")),
					kind:      text(summary.Get("session_kind")),
					signature: summarySignature + ":" + groupSignature,
				}
				if s.sessionID == "" {
					s.sessionID = filepath.Base(directory)
				}
				if s.project == "" {
					s.project = groupProject
				}
				if s.project == "" {
					s.project = "Unknown project"
				}
				if s.forkedAt = grokTimestamp(summary.Get("forked_at")); s.forkedAt == "" {
					s.forkedAt = grokTimestamp(summary.Get("created_at"))
				}
				if file := filepath.Join(directory, "usage.json"); regularFile(file) {
					s.ledger = file
				}
				if file := filepath.Join(directory, "updates.jsonl"); regularFile(file) {
					s.updates = file
				}
				sessions = append(sessions, s)
				subagents := filepath.Join(directory, "subagents")
				if info, err := os.Lstat(subagents); err != nil || !info.IsDir() {
					continue
				}
				for _, child := range subdirectories(subagents) {
					file := filepath.Join(child, "meta.json")
					if !regularFile(file) {
						continue
					}
					data, _, ok := readBounded(file, 1<<20)
					if !ok || !gjson.ValidBytes(data) {
						continue
					}
					meta := gjson.ParseBytes(data)
					childID := text(meta.Get("child_session_id"))
					parentID := text(meta.Get("parent_session_id"))
					if parentID == "" {
						parentID = s.sessionID
					}
					if childID != "" && childID != parentID {
						parents[childID] = parentID
					}
				}
			}
		}
	}
	subagent := func(s *grokSession) bool { return strings.HasPrefix(s.kind, "subagent") || parents[s.sessionID] != "" }
	retained := map[string]cachedFile{}
	for _, s := range sessions {
		if (s.ledger == "" && s.updates == "") || subagent(s) {
			continue
		}
		ledgerMalformed := 0
		if s.ledger != "" {
			if parsed := g.parse(s, s.ledger, true, retained); parsed != nil {
				s.parsed, ledgerMalformed = parsed, parsed.Malformed
			} else {
				ledgerMalformed = 1
			}
		}
		if (s.parsed == nil || ledgerMalformed > 0) && s.updates != "" {
			if parsed := g.parse(s, s.updates, false, retained); parsed != nil {
				s.parsed = parsed
			}
		}
	}
	g.mu.Lock()
	g.files = retained
	g.mu.Unlock()

	byID := map[string]*grokSession{}
	nativeTurns := map[string]map[string]map[int64]bool{}
	for _, s := range sessions {
		byID[s.sessionID] = s
		turns := nativeTurns[s.sessionID]
		if turns == nil {
			turns = map[string]map[int64]bool{}
			nativeTurns[s.sessionID] = turns
		}
		if s.parsed == nil {
			continue
		}
		for _, entry := range s.parsed.Events {
			key := fmt.Sprint(grokTurnNumber(&entry.Event), ":", entry.Event.Model)
			if turns[key] == nil {
				turns[key] = map[int64]bool{}
			}
			turns[key][int64(parseDate(entry.Event.Timestamp))] = true
		}
	}
	orphans := 0
	for _, s := range sessions {
		if !subagent(s) {
			continue
		}
		parentID := parents[s.sessionID]
		if parentID == "" {
			parentID = s.parentID
		}
		parent := byID[parentID]
		visited := map[string]bool{s.sessionID: true}
		for parent != nil && subagent(parent) && !visited[parent.sessionID] {
			visited[parent.sessionID] = true
			if parentID = parents[parent.sessionID]; parentID == "" {
				parentID = parent.parentID
			}
			parent = byID[parentID]
		}
		if parent == nil || (parent.ledger == "" && parent.updates == "") || parent.parsed == nil {
			orphans++
		}
	}
	if orphans > 0 {
		warnings = append(warnings, fmt.Sprintf("grok: %d subagent sessions have no readable parent accounting.", orphans))
	}
	var files []*parsedFile
	for _, s := range sessions {
		if s.parsed == nil {
			continue
		}
		file := *s.parsed
		file.Events = make([]*candidate, len(s.parsed.Events))
		for i, original := range s.parsed.Events {
			entry := *original
			file.Events[i] = &entry
			if s.parentID == "" {
				continue
			}
			e := &entry.Event
			owner := s
			visited := map[string]bool{}
			for owner.parentID != "" && !visited[owner.sessionID] {
				parent := byID[owner.parentID]
				stamps := nativeTurns[owner.parentID][fmt.Sprint(grokTurnNumber(e), ":", e.Model)]
				millis := int64(parseDate(e.Timestamp))
				inherited := false
				for offset := int64(-2); offset <= 2; offset++ {
					inherited = inherited || stamps[millis+offset]
				}
				beforeCopy := owner.forkedAt != "" && e.Timestamp < owner.forkedAt
				absentParentTie := parent == nil && owner.forkedAt != "" && e.Timestamp == owner.forkedAt
				if !inherited && !beforeCopy && !absentParentTie {
					break
				}
				visited[owner.sessionID] = true
				e.SessionID = owner.parentID
				if parent == nil {
					break
				}
				owner = parent
			}
			e.ID = grokTurnID(e.SessionID, grokTurnNumber(e), e.Model)
			entry.Sidechain = e.SessionID != s.sessionID
		}
		files = append(files, &file)
	}
	return files, warnings
}

func (g *grokCache) parse(s *grokSession, file string, ledger bool, retained map[string]cachedFile) *parsedFile {
	limit := int64(math.MaxInt64)
	if ledger {
		limit = 64 << 20
	}
	data, signature, ok := readBounded(file, limit)
	if !ok {
		return nil
	}
	signature = "grok-v1:" + signature + ":" + s.signature
	g.mu.Lock()
	cached, hit := g.files[file]
	g.mu.Unlock()
	if hit && cached.signature == signature {
		retained[file] = cached
		return cached.parsed
	}
	var parsed *parsedFile
	if ledger {
		if !gjson.ValidBytes(data) {
			return nil
		}
		parsed = parseGrokLedger(data, s)
	} else {
		parsed = parseGrokUpdates(data, s)
	}
	retained[file] = cachedFile{signature, parsed}
	return parsed
}
