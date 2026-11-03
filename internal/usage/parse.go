package usage

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/tidwall/gjson"
)

// candidate is one parsed usage record before cross-file deduplication.
type candidate struct {
	Event           Event
	Sidechain       bool
	CacheWrite1h    int64
	Tier            string
	NativeMessageID string
	ParentEventID   string
	LegacyEventID   string
}

type emptyRequest struct {
	ID, NativeMessageID, SessionID string
}

// parsedFile is everything deduplication needs from one log, and nothing else:
// prompts and replies are dropped as each record is read.
type parsedFile struct {
	Events              []*candidate
	Malformed           int
	SessionID           string
	ParentID            string
	ForkedAt            string
	WorkspaceRoot       string
	CompactionIDs       map[string]bool
	RetractedIDs        []string
	EmptyClaudeRequests []emptyRequest
	SessionTitle        string
}

const maximumLineLength = 32 * 1024 * 1024

var readers = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 1<<20) }}

// readLines calls visit for each line, as token-tracker's record parser splits a log:
// every newline ends a complete line, and a final unterminated line is incomplete.
// Lines over 32 MiB count as malformed without being parsed.
func readLines(r io.Reader, visit func(line []byte, number int, complete bool), oversized func()) error {
	reader := readers.Get().(*bufio.Reader)
	reader.Reset(r)
	defer func() {
		reader.Reset(nil)
		readers.Put(reader)
	}()
	var long []byte
	accumulating, tooLong := false, false
	number := 0
	for {
		chunk, err := reader.ReadSlice('\n')
		complete := err == nil
		if errors.Is(err, bufio.ErrBufferFull) {
			if !tooLong {
				long = append(long, chunk...)
				accumulating = true
				if len(long) > maximumLineLength*3 || (len(long) > maximumLineLength && jsLength(string(long)) > maximumLineLength) {
					long, tooLong = long[:0], true
				}
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if complete {
			chunk = chunk[:len(chunk)-1]
		}
		line := chunk
		if accumulating || tooLong {
			long = append(long, chunk...)
			line = long
		}
		if complete || len(line) > 0 || tooLong {
			number++
			switch {
			case tooLong || (len(line) > maximumLineLength && jsLength(string(line)) > maximumLineLength):
				oversized()
			default:
				visit(line, number, complete)
			}
		}
		long, accumulating, tooLong = long[:0], false, false
		if !complete {
			return nil
		}
	}
}

// parseRecords handles one log: each non-blank line must be a JSON object the visitor
// accepts, or (when complete) it counts as malformed.
func parseRecords(r io.Reader, parsed *parsedFile, visit func(record gjson.Result, line int) bool) error {
	return readLines(r, func(raw []byte, number int, complete bool) {
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
		record := gjson.Parse(unsafe.String(unsafe.SliceData(raw), len(raw)))
		if (!record.IsObject() || !visit(record, number)) && complete {
			parsed.Malformed++
		}
	}, func() { parsed.Malformed++ })
}

// fields returns an object's members by name; a repeated key keeps its last value.
func fields(r gjson.Result, names ...string) (out [12]gjson.Result) {
	if !r.IsObject() {
		return out
	}
	for m := (members{raw: r.Raw}); ; {
		key, value, ok := m.next()
		if !ok {
			return out
		}
		for i, name := range names {
			if key == name {
				out[i] = gjson.Parse(value)
			}
		}
	}
}

// members walks a valid JSON object's members without allocating.
type members struct {
	raw     string
	i       int
	started bool
}

func (m *members) skipSpace() {
	for m.i < len(m.raw) && (m.raw[m.i] <= ' ' || m.raw[m.i] == ',' || m.raw[m.i] == ':') {
		m.i++
	}
}

// next returns the next member's key and raw value.
func (m *members) next() (string, string, bool) {
	if !m.started {
		m.started = true
		m.i = strings.IndexByte(m.raw, '{') + 1
		if m.i == 0 {
			return "", "", false
		}
	}
	m.skipSpace()
	if m.i >= len(m.raw) || m.raw[m.i] != '"' {
		return "", "", false
	}
	end, escaped := stringEnd(m.raw, m.i)
	key := m.raw[m.i+1 : end-1]
	if escaped {
		key = gjson.Parse(m.raw[m.i:end]).Str
	}
	m.i = end
	m.skipSpace()
	start := m.i
	m.i = valueEnd(m.raw, m.i)
	return key, m.raw[start:m.i], true
}

// stringEnd returns the index just past the string starting at s[i] == '"'.
func stringEnd(s string, i int) (int, bool) {
	escaped := false
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			escaped = true
			j++
		case '"':
			return j + 1, escaped
		}
	}
	return len(s), escaped
}

// valueEnd returns the index just past the JSON value starting at s[i].
func valueEnd(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch c := s[j]; c {
		case '"':
			j, _ = stringEnd(s, j)
			j--
			if depth == 0 {
				return j + 1
			}
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return j
			}
			depth--
			if depth == 0 {
				return j + 1
			}
		case ',':
			if depth == 0 {
				return j
			}
		default:
			if depth == 0 && c <= ' ' {
				return j
			}
		}
	}
	return len(s)
}

func isString(v gjson.Result, s string) bool { return v.Type == gjson.String && v.Str == s }

// text is a trimmed string value, copied out of the line buffer.
func text(v gjson.Result) string {
	if v.Type != gjson.String {
		return ""
	}
	return strings.Clone(jsTrim(v.Str))
}

func hasNumber(v gjson.Result) bool {
	return v.Type == gjson.Number && !math.IsInf(v.Num, 0) && !math.IsNaN(v.Num)
}

func number(v gjson.Result) int64 {
	if !hasNumber(v) || v.Num <= 0 {
		return 0
	}
	if v.Num >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v.Num)
}

func modelName(v gjson.Result) string {
	if s := text(v); s != "" && s != "<synthetic>" {
		return s
	}
	return "unknown"
}

func modelText(s string) string {
	if s != "" && s != "<synthetic>" {
		return s
	}
	return "unknown"
}

func projectText(s string) string {
	if s == "" {
		return "Unknown project"
	}
	return s
}

func fileSession(file string) string {
	name := file
	if i := strings.LastIndexAny(file, `\/`); i >= 0 {
		name = file[i+1:]
	}
	if len(name) >= 6 && strings.EqualFold(name[len(name)-6:], ".jsonl") {
		name = name[:len(name)-6]
	}
	if name == "" {
		return "unknown"
	}
	return name
}

func tokenTotal(e *Event) int64 {
	return e.InputTokens + e.OutputTokens + e.CacheReadTokens + e.CacheWriteTokens
}

// ordinalValue is the JSON value fallback ids hash: a numeric or non-empty string
// ordinal, else the line number.
func ordinalValue(ordinal gjson.Result, line int) any {
	if hasNumber(ordinal) {
		return ordinal.Num
	}
	if ordinal.Type == gjson.String && ordinal.Str != "" {
		return ordinal.Str
	}
	return line
}

func fallbackID(harness, session, stamp string, ordinal any) string {
	return harness + ":metadata:" + sha256Hex(jsonArray(session, stamp, ordinal))
}

func timestamp(v gjson.Result) string {
	switch v.Type {
	case gjson.Number:
		if v.Num > 1e11 {
			return isoMillis(v.Num)
		}
		return isoMillis(v.Num * 1000)
	case gjson.String:
		return isoMillis(parseDate(jsTrim(v.Str)))
	}
	return ""
}

func newEvent(harness, stamp, session, model, project string) Event {
	return Event{Timestamp: stamp, Harness: harness, SessionID: session, Model: model, Project: projectText(project)}
}

// interner shares the strings many records of one log repeat.
type interner map[string]string

func (in interner) get(s string) string {
	if v, ok := in[s]; ok {
		return v
	}
	in[s] = s
	return s
}

func parseClaude(r io.Reader, file string) (*parsedFile, error) {
	parsed := &parsedFile{}
	strs := interner{}
	represented := map[string]bool{}
	empty := map[string]emptyRequest{}
	var emptyOrder []string
	title := ""
	err := parseRecords(r, parsed, func(record gjson.Result, line int) bool {
		f := fields(record, "type", "customTitle", "agentName", "message", "timestamp", "sessionId", "requestId",
			"uuid", "ordinal", "cwd", "isSidechain", "entrypoint")
		kind, message := f[0], f[3]
		if isString(kind, "custom-title") || isString(kind, "agent-name") {
			value := f[1]
			if isString(kind, "agent-name") {
				value = f[2]
			}
			if t := text(value); t != "" {
				title = t
			}
			return true
		}
		m := fields(message, "usage", "model", "id")
		usage, model, messageID := m[0], m[1], m[2]
		if !isString(kind, "assistant") || !usage.IsObject() || isString(model, "<synthetic>") {
			return true
		}
		stamp := timestamp(f[4])
		if stamp == "" {
			return false
		}
		session := text(f[5])
		if session == "" {
			session = fileSession(file)
		}
		session = strs.get(session)
		nativeID := text(messageID)
		var id string
		switch {
		case nativeID != "":
			id = "claude:message:" + nativeID + ":" + text(f[6])
		case text(f[7]) != "":
			id = "claude:uuid:" + text(f[7])
		default:
			id = fallbackID("claude", session, stamp, ordinalValue(f[8], line))
		}
		sidechain := f[10].Type == gjson.True
		cwd := strs.get(text(f[9]))
		via := strs.get(text(f[11]))
		parentTier := tierOf(usage, "")
		firstEntry := len(parsed.Events)
		appendUsage := func(usage gjson.Result, model gjson.Result, usageID string, advisor bool) {
			u := fields(usage, "cache_creation", "output_tokens_details", "input_tokens", "output_tokens",
				"cache_read_input_tokens", "cache_creation_input_tokens")
			cache := fields(u[0], "ephemeral_1h_input_tokens", "ephemeral_5m_input_tokens")
			details := fields(u[1], "thinking_tokens", "reasoning_tokens")
			e := newEvent("claude", stamp, session, strs.get(modelName(model)), cwd)
			e.ID = usageID
			e.Entrypoint = via
			e.InputTokens = number(u[2])
			e.OutputTokens = number(u[3])
			e.CacheReadTokens = number(u[4])
			e.CacheWriteTokens = max(number(u[5]), number(cache[0])+number(cache[1]))
			e.ReasoningTokens = min(e.OutputTokens, max(number(details[0]), number(details[1])))
			if tokenTotal(&e) == 0 {
				return
			}
			c := &candidate{
				Event:        e,
				Tier:         strs.get(tierOf(usage, parentTier)),
				CacheWrite1h: min(e.CacheWriteTokens, number(cache[0])),
				Sidechain:    sidechain,
			}
			if nativeID != "" {
				c.NativeMessageID = nativeID
			}
			if advisor {
				c.ParentEventID = id
			}
			parsed.Events = append(parsed.Events, c)
		}
		appendUsage(usage, model, id, false)
		if iterations := usage.Get("iterations"); iterations.IsArray() {
			advisorIndex := 0
			for index, value := range iterations.Array() {
				it := fields(value, "type", "model")
				if !isString(it[0], "advisor_message") {
					continue
				}
				ordinal := advisorIndex
				advisorIndex++
				if t := text(it[1]); t == "" || t == "<synthetic>" {
					continue
				}
				advisorID := "claude:advisor:" + sha256Hex(jsonArray(id, ordinal))
				before := len(parsed.Events)
				appendUsage(value, it[1], advisorID, true)
				if index != ordinal && len(parsed.Events) > before {
					parsed.Events[len(parsed.Events)-1].LegacyEventID = "claude:advisor:" + sha256Hex(jsonArray(id, index))
				}
			}
		}
		if nativeID != "" && !sidechain {
			if len(parsed.Events) > firstEntry {
				represented[id] = true
				delete(empty, id)
			} else if !represented[id] {
				if _, ok := empty[id]; !ok {
					emptyOrder = append(emptyOrder, id)
				}
				empty[id] = emptyRequest{ID: id, NativeMessageID: nativeID, SessionID: session}
			}
		}
		return true
	})
	for _, id := range emptyOrder {
		if request, ok := empty[id]; ok {
			parsed.EmptyClaudeRequests = append(parsed.EmptyClaudeRequests, request)
			delete(empty, id)
		}
	}
	parsed.SessionTitle = title
	parsed.Events = collapseRepeats(parsed.Events)
	return parsed, err
}

// collapseRepeats keeps one candidate per id and sidechain flag within a file: Claude
// writes a message's usage once per content block. The kept one is the first with the
// largest total, which is what deduplication would pick, at its first position.
func collapseRepeats(events []*candidate) []*candidate {
	type key struct {
		id        string
		sidechain bool
	}
	index := make(map[key]int, len(events))
	kept := events[:0]
	for _, entry := range events {
		k := key{entry.Event.ID, entry.Sidechain}
		i, ok := index[k]
		if !ok {
			index[k] = len(kept)
			kept = append(kept, entry)
			continue
		}
		if tokenTotal(&entry.Event) > tokenTotal(&kept[i].Event) {
			if entry.LegacyEventID == "" {
				entry.LegacyEventID = kept[i].LegacyEventID
			}
			kept[i] = entry
		}
	}
	clear(events[len(kept):])
	return slices.Clip(kept)
}

// tierOf is a Claude usage block's service tier; speed "fast" is priority.
func tierOf(usage gjson.Result, fallback string) string {
	u := fields(usage, "speed", "service_tier")
	if isString(u[0], "fast") {
		return "priority"
	}
	if t := text(u[1]); t != "" {
		return t
	}
	return fallback
}

const (
	tkInput = iota
	tkCached
	tkCacheWrite
	tkCacheCreation
	tkOutput
	tkReasoning
	tkTotal
)

// codexTokens are Codex's seven counters, already passed through number().
type codexTokens [7]int64

func readCodexTokens(v gjson.Result) codexTokens {
	f := fields(v, "input_tokens", "cached_input_tokens", "cache_write_input_tokens", "cache_creation_input_tokens",
		"output_tokens", "reasoning_output_tokens", "total_tokens")
	var t codexTokens
	for i := range t {
		t[i] = number(f[i])
	}
	return t
}

func tokenDelta(current codexTokens, previous *codexTokens) codexTokens {
	if previous == nil || current[tkInput] < previous[tkInput] || current[tkOutput] < previous[tkOutput] ||
		(current[tkTotal] > 0 && previous[tkTotal] > 0 && current[tkTotal] < previous[tkTotal]) {
		return current
	}
	var delta codexTokens
	for i := range delta {
		delta[i] = max(0, current[i]-previous[i])
	}
	return delta
}

func codexModel(v gjson.Result) string {
	f := fields(v, "model", "model_name", "metadata")
	if s := text(f[0]); s != "" {
		return s
	}
	if s := text(f[1]); s != "" {
		return s
	}
	return text(f[2].Get("model"))
}

type codexContext struct{ model, cwd, tier string }

var uuidV7 = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func codexTaskStart(payload gjson.Result) (millis float64, precision float64, ok bool) {
	f := fields(payload, "started_at", "turn_id")
	started := timestamp(f[0])
	turnID := text(f[1])
	if uuidV7.MatchString(turnID) {
		turn, _ := strconv.ParseInt(turnID[:8]+turnID[9:13], 16, 64)
		if started == "" || math.Floor(float64(turn)/1000) == math.Floor(parseDate(started)/1000) {
			return float64(turn), 1, true
		}
	}
	precision = 1
	if hasNumber(f[0]) && f[0].Num == math.Trunc(f[0].Num) && f[0].Num <= 1e11 {
		precision = 1000
	}
	if started == "" {
		return 0, 0, false
	}
	return parseDate(started), precision, true
}

func codexEvent(stamp, session string, ctx codexContext, tokens codexTokens) Event {
	e := newEvent("codex", stamp, session, ctx.model, ctx.cwd)
	input := tokens[tkInput]
	e.CacheReadTokens = min(tokens[tkCached], input)
	e.CacheWriteTokens = min(max(tokens[tkCacheWrite], tokens[tkCacheCreation]), input-e.CacheReadTokens)
	e.InputTokens = input - e.CacheReadTokens - e.CacheWriteTokens
	e.OutputTokens = tokens[tkOutput]
	e.ReasoningTokens = min(tokens[tkReasoning], e.OutputTokens)
	return e
}

type codexPrecise struct {
	entry         *candidate
	turnID        string
	explicitModel string
	threadID      string
	responseID    string
	line          int
	tokens        codexTokens
	threadTokens  *codexTokens
}

type codexSnapshot struct {
	entry  *candidate
	line   int
	tokens codexTokens
	total  *codexTokens
}

func parseCodex(r io.Reader, file string) (*parsedFile, error) {
	parsed := &parsedFile{SessionID: fileSession(file)}
	strs := interner{}
	ctx := codexContext{model: "unknown"}
	contexts := map[string]codexContext{}
	var previous *codexTokens
	var precise []codexPrecise
	var snapshots []codexSnapshot
	compactionMarkers := map[string]int{}
	firstMetadata := true
	var historyStart *float64
	copiedAncestry, inheritedTask := false, false
	nativeStartLine := -1
	beforeHistory := func(ordinal gjson.Result) bool {
		return historyStart != nil && hasNumber(ordinal) && ordinal.Num < *historyStart
	}
	err := parseRecords(r, parsed, func(record gjson.Result, line int) bool {
		f := fields(record, "payload", "type", "timestamp", "ordinal")
		payload, ordinal := f[0], f[3]
		kind := text(f[1])
		switch kind {
		case "session_meta":
			p := fields(payload, "id", "session_id", "subagent_history_start_ordinal", "source", "forked_from_id",
				"parent_thread_id", "timestamp", "cwd")
			if !firstMetadata {
				if text(p[0]) == parsed.ParentID {
					copiedAncestry = true
				}
				return true
			}
			firstMetadata = false
			if hasNumber(p[2]) {
				start := p[2].Num
				historyStart = &start
			}
			if id := text(p[0]); id != "" {
				parsed.SessionID = id
			} else if id := text(p[1]); id != "" {
				parsed.SessionID = id
			}
			parsed.ParentID = text(p[4])
			if parsed.ParentID == "" {
				parsed.ParentID = text(p[5])
			}
			if parsed.ParentID == "" {
				parsed.ParentID = text(p[3].Get("subagent").Get("thread_spawn").Get("parent_thread_id"))
			}
			parsed.ForkedAt = timestamp(p[6])
			if parsed.ForkedAt == "" {
				parsed.ForkedAt = timestamp(f[2])
			}
			ctx.cwd = strs.get(text(p[7]))
			if m := codexModel(payload); m != "" {
				ctx.model = strs.get(modelText(m))
			}
			return true
		case "turn_context":
			p := fields(payload, "cwd", "service_tier", "turn_id")
			if m := codexModel(payload); m != "" {
				ctx.model = strs.get(modelText(m))
			}
			if cwd := text(p[0]); cwd != "" {
				ctx.cwd = strs.get(cwd)
			}
			if p[1].Type == gjson.String {
				ctx.tier = strs.get(text(p[1]))
			}
			if id := text(p[2]); id != "" {
				contexts[id] = ctx
			}
			return true
		case "compacted":
			if id := text(payload.Get("compaction_response_id")); id != "" {
				if _, ok := compactionMarkers[id]; !ok {
					compactionMarkers[id] = line
				}
			}
			return true
		case "event_msg":
			p := fields(payload, "type", "info", "thread_settings")
			if isString(p[0], "task_started") && parsed.ParentID != "" && parsed.ForkedAt != "" && nativeStartLine < 0 {
				if millis, precision, ok := codexTaskStart(payload); ok {
					creation := parseDate(parsed.ForkedAt)
					if math.Floor(millis/precision) < math.Floor(creation/precision) {
						inheritedTask = true
					} else {
						nativeStartLine = line
					}
				}
				return true
			}
			if isString(p[0], "thread_settings_applied") {
				if tier := p[2].Get("service_tier"); tier.Type == gjson.String {
					ctx.tier = strs.get(text(tier))
				}
				return true
			}
			if !isString(p[0], "token_count") || !p[1].IsObject() {
				return true
			}
			stamp := timestamp(f[2])
			if stamp == "" {
				return false
			}
			info := fields(p[1], "total_token_usage", "last_token_usage")
			var tokens *codexTokens
			var total *codexTokens
			if info[0].IsObject() {
				totals := readCodexTokens(info[0])
				total = &totals
				if previous == nil || totals != *previous {
					var t codexTokens
					if info[1].IsObject() {
						t = readCodexTokens(info[1])
					} else {
						t = tokenDelta(totals, previous)
					}
					tokens = &t
				}
				previous = &totals
			} else if info[1].IsObject() {
				t := readCodexTokens(info[1])
				tokens = &t
			}
			if tokens == nil || beforeHistory(ordinal) {
				return true
			}
			current := ctx
			if m := codexModel(payload); m != "" {
				current.model = strs.get(m)
			} else if m := codexModel(p[1]); m != "" {
				current.model = strs.get(m)
			}
			e := codexEvent(stamp, parsed.SessionID, current, *tokens)
			if tokenTotal(&e) > 0 {
				e.ID = "codex:total:" + sha256Hex(jsonArray(parsed.SessionID, stamp, e.InputTokens, e.OutputTokens,
					e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens))
				snapshots = append(snapshots, codexSnapshot{
					entry: &candidate{Event: e, Tier: current.tier}, line: line, tokens: *tokens, total: total,
				})
			}
			return true
		case "token_usage_record":
		default:
			return true
		}
		p := fields(payload, "usage", "thread_id", "response_id", "service_tier", "turn_id", "thread_token_usage")
		if !p[0].IsObject() || beforeHistory(ordinal) {
			return true
		}
		stamp := timestamp(f[2])
		if stamp == "" {
			return false
		}
		threadID := text(p[1])
		if threadID != "" && threadID != parsed.SessionID {
			return true
		}
		tokens := readCodexTokens(p[0])
		e := codexEvent(stamp, parsed.SessionID, ctx, tokens)
		if tokenTotal(&e) == 0 {
			return true
		}
		responseID := text(p[2])
		if responseID != "" {
			e.ID = "codex:response:" + responseID
		} else {
			e.ID = fallbackID("codex", parsed.SessionID, stamp, ordinalValue(ordinal, line))
		}
		tier := text(p[3])
		if tier == "" {
			tier = ctx.tier
		}
		pending := codexPrecise{
			entry: &candidate{Event: e, Tier: strs.get(tier)}, turnID: text(p[4]), explicitModel: codexModel(payload),
			threadID: threadID, responseID: responseID, line: line, tokens: tokens,
		}
		if p[5].IsObject() {
			t := readCodexTokens(p[5])
			pending.threadTokens = &t
		}
		precise = append(precise, pending)
		return true
	})
	firstRecord := "￿"
	for _, pending := range precise {
		if _, compacted := compactionMarkers[pending.responseID]; pending.threadID != "" && !compacted &&
			pending.entry.Event.Timestamp < firstRecord {
			firstRecord = pending.entry.Event.Timestamp
		}
	}
	replayBoundary, hasBoundary := 0, false
	if parsed.ParentID != "" && (copiedAncestry || inheritedTask) {
		switch {
		case nativeStartLine >= 0:
			replayBoundary, hasBoundary = nativeStartLine, true
		case inheritedTask:
			replayBoundary, hasBoundary = math.MaxInt, true
		}
	}
	for _, snapshot := range snapshots {
		if snapshot.entry.Event.Timestamp >= firstRecord {
			continue
		}
		if hasBoundary && snapshot.line < replayBoundary {
			parsed.RetractedIDs = append(parsed.RetractedIDs, snapshot.entry.Event.ID)
			continue
		}
		parsed.Events = append(parsed.Events, snapshot.entry)
	}
	parsed.CompactionIDs = map[string]bool{}
	for _, pending := range precise {
		if _, ok := compactionMarkers[pending.responseID]; ok {
			parsed.CompactionIDs[pending.responseID] = true
		}
	}
	for index, pending := range precise {
		markerLine, marked := compactionMarkers[pending.responseID]
		if pending.threadID == "" && !marked {
			continue
		}
		if pending.threadID == "" && hasBoundary && pending.line < replayBoundary {
			parsed.RetractedIDs = append(parsed.RetractedIDs, pending.entry.Event.ID)
			continue
		}
		if marked {
			low, high := 0, len(snapshots)
			for low < high {
				middle := int(uint(low+high) >> 1)
				if snapshots[middle].line <= pending.line {
					low = middle + 1
				} else {
					high = middle
				}
			}
			boundary := markerLine
			if index+1 < len(precise) && precise[index+1].line < boundary {
				boundary = precise[index+1].line
			}
			var covered *codexSnapshot
			for i := low; i < len(snapshots); i++ {
				snapshot := &snapshots[i]
				if snapshot.line >= boundary {
					break
				}
				if snapshot.entry.Event.Timestamp < firstRecord && (snapshot.tokens == pending.tokens ||
					(pending.threadTokens != nil && snapshot.total != nil && *pending.threadTokens == *snapshot.total)) {
					covered = snapshot
					break
				}
			}
			if covered != nil {
				covered.entry.Event.ID = pending.entry.Event.ID
				continue
			}
		}
		if turn, ok := contexts[pending.turnID]; ok {
			pending.entry.Event.Model = turn.model
			pending.entry.Event.Project = projectText(turn.cwd)
			if pending.entry.Tier == "" {
				pending.entry.Tier = turn.tier
			}
		}
		if pending.explicitModel != "" {
			pending.entry.Event.Model = strs.get(modelText(pending.explicitModel))
		}
		parsed.Events = append(parsed.Events, pending.entry)
	}
	return parsed, err
}

func parsePi(r io.Reader, file string) (*parsedFile, error) {
	parsed := &parsedFile{SessionID: fileSession(file)}
	strs := interner{}
	cwd, model := "", "unknown"
	err := parseRecords(r, parsed, func(record gjson.Result, line int) bool {
		f := fields(record, "type", "id", "parentSession", "timestamp", "cwd", "modelId", "message", "ordinal")
		switch {
		case isString(f[0], "session"):
			if id := text(f[1]); id != "" {
				parsed.SessionID = id
			}
			parent := text(f[2])
			parsed.ParentID, parsed.ForkedAt = "", ""
			if parent != "" {
				parts := strings.Split(fileSession(parent), "_")
				parsed.ParentID = parts[len(parts)-1]
				parsed.ForkedAt = timestamp(f[3])
			}
			cwd = strs.get(text(f[4]))
			return true
		case isString(f[0], "model_change"):
			model = strs.get(modelName(f[5]))
			return true
		}
		m := fields(f[6], "role", "usage", "model")
		if !isString(f[0], "message") || !isString(m[0], "assistant") || !m[1].IsObject() {
			return true
		}
		stamp := timestamp(f[3])
		if stamp == "" {
			return false
		}
		if text(m[2]) != "" {
			model = strs.get(modelName(m[2]))
		}
		u := fields(m[1], "input", "output", "cacheRead", "cacheWrite", "reasoning", "cacheWrite1h")
		e := newEvent("pi", stamp, parsed.SessionID, model, cwd)
		e.InputTokens = number(u[0])
		e.OutputTokens = number(u[1])
		e.CacheReadTokens = number(u[2])
		e.CacheWriteTokens = number(u[3])
		e.ReasoningTokens = min(number(u[4]), e.OutputTokens)
		if tokenTotal(&e) == 0 {
			return true
		}
		if id := text(f[1]); id != "" {
			e.ID = "pi:" + parsed.SessionID + ":" + id
		} else {
			e.ID = fallbackID("pi", parsed.SessionID, stamp, ordinalValue(f[7], line))
		}
		parsed.Events = append(parsed.Events, &candidate{Event: e, CacheWrite1h: min(e.CacheWriteTokens, number(u[5]))})
		return true
	})
	return parsed, err
}

// museTimestamp reads Muse's microsecond clock; other scales are normalized.
func museTimestamp(v gjson.Result) string {
	if !hasNumber(v) {
		return timestamp(v)
	}
	value := v.Num
	switch {
	case value > 1e17:
		return isoMillis(value / 1e6)
	case value > 1e14:
		return isoMillis(value / 1000)
	case value > 1e11:
		return isoMillis(value)
	}
	return isoMillis(value * 1000)
}

// musePathIDs reads <date>/<session>/session.jsonl and grouped child sessions such as
// <session>/subagent/<child>/session.jsonl.
func musePathIDs(file string) (session, parent string) {
	var parts []string
	for _, part := range strings.FieldsFunc(file, func(r rune) bool { return r == '/' || r == '\\' }) {
		parts = append(parts, part)
	}
	at := func(fromEnd int) string {
		if len(parts) >= fromEnd {
			return parts[len(parts)-fromEnd]
		}
		return ""
	}
	group := func(s string) bool { return s == "subagent" || s == "approval-review" || s == "workflows" }
	session = fileSession(file)
	if session == "session" {
		session = at(2)
		if session == "" {
			session = "unknown"
		}
	}
	switch {
	case group(at(3)):
		parent = at(4)
	case group(at(2)):
		parent = at(3)
	}
	if parent == session {
		parent = ""
	}
	return session, parent
}

func parseMuse(r io.Reader, file string) (*parsedFile, error) {
	parsed := &parsedFile{}
	parsed.SessionID, parsed.ParentID = musePathIDs(file)
	strs := interner{}
	cwd, model := "", "unknown"
	var visit func(record gjson.Result, line int, childPath string) bool
	visit = func(record gjson.Result, line int, childPath string) bool {
		f := fields(record, "children", "stream", "payload", "payload_type", "recorded_at", "id", "sequence")
		if f[0].IsArray() {
			for index, child := range f[0].Array() {
				c := fields(child, "record_json", "child_index")
				raw := text(c[0])
				if raw == "" {
					continue
				}
				childOrdinal := jsNumber(float64(index))
				if hasNumber(c[1]) {
					childOrdinal = jsNumber(c[1].Num)
				}
				next := childOrdinal
				if childPath != "" {
					next = childPath + ":" + childOrdinal
				}
				if !gjson.Valid(raw) {
					parsed.Malformed++
					continue
				}
				inner := gjson.Parse(raw)
				if !inner.IsObject() || !visit(inner, line, next) {
					parsed.Malformed++
				}
			}
			return true
		}
		stream := fields(f[1], "kind", "id")
		streamKind := text(stream[0])
		if streamKind != "" && streamKind != "session" {
			return true
		}
		if streamKind == "session" && text(stream[1]) != "" {
			parsed.SessionID = text(stream[1])
		}
		payload := f[2]
		switch text(f[3]) {
		case "runtime.session.metadata", "session.workspace_branch.observed", "run.model.configured":
			info := fields(payload.Get("record"), "workspace_root", "model_id")
			if root := text(info[0]); root != "" {
				cwd = strs.get(root)
				if parsed.WorkspaceRoot == "" {
					parsed.WorkspaceRoot = cwd
				}
			}
			if text(info[1]) != "" {
				model = strs.get(modelName(info[1]))
			}
			return true
		}
		p := fields(payload, "event", "source_run_record_id")
		ev := fields(p[0], "kind", "usage", "model")
		if !isString(ev[0], "model_completed") || !ev[1].IsObject() {
			return true
		}
		stamp := museTimestamp(f[4])
		if stamp == "" {
			return false
		}
		u := fields(ev[1], "input_tokens", "cache_read_tokens", "cached_tokens", "cache_write_tokens", "output_tokens",
			"reasoning_tokens")
		eventModel := text(ev[2])
		if eventModel == "" {
			eventModel = model
		}
		e := newEvent("muse", stamp, parsed.SessionID, strs.get(modelText(eventModel)), cwd)
		input := number(u[0])
		e.CacheReadTokens = min(input, max(number(u[1]), number(u[2])))
		e.CacheWriteTokens = min(input-e.CacheReadTokens, number(u[3]))
		e.InputTokens = input - e.CacheReadTokens - e.CacheWriteTokens
		e.OutputTokens = number(u[4])
		e.ReasoningTokens = min(e.OutputTokens, number(u[5]))
		if tokenTotal(&e) == 0 {
			return true
		}
		nativeID := text(p[1])
		if nativeID == "" {
			nativeID = text(f[5])
		}
		if nativeID != "" {
			e.ID = "muse:record:" + nativeID
		} else {
			var ordinal any = f[6].Num
			if !hasNumber(f[6]) {
				if childPath != "" {
					ordinal = strconv.Itoa(line) + ":" + childPath
				} else {
					ordinal = museSequence(f[6], line)
				}
			}
			e.ID = fallbackID("muse", parsed.SessionID, stamp, ordinal)
		}
		parsed.Events = append(parsed.Events, &candidate{Event: e})
		return true
	}
	err := parseRecords(r, parsed, func(record gjson.Result, line int) bool { return visit(record, line, "") })
	return parsed, err
}

// museSequence is fallbackId's ordinal when a top-level record has a non-numeric sequence.
func museSequence(sequence gjson.Result, line int) any {
	if sequence.Type == gjson.String && sequence.Str != "" {
		return sequence.Str
	}
	return line
}
