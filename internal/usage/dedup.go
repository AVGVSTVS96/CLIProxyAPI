package usage

import "strings"

// fileView is one parsed log during a deduplication pass. Filters replace Events and
// copy any candidate they change, so cached parsed files are never modified.
type fileView struct {
	*parsedFile
	Events    []*candidate
	Retracted []string
}

func newViews(files []*parsedFile) []*fileView {
	views := make([]*fileView, len(files))
	for i, file := range files {
		views[i] = &fileView{parsedFile: file, Events: file.Events, Retracted: file.RetractedIDs}
	}
	return views
}

func (v *fileView) harness() string {
	if len(v.Events) == 0 {
		return ""
	}
	return v.Events[0].Event.Harness
}

type replayKey struct{ native, session string }

func filterClaudeReplays(files []*fileView) {
	originals := map[replayKey]map[string]bool{}
	billed := map[replayKey]bool{}
	remember := func(key replayKey, id string) {
		if originals[key] == nil {
			originals[key] = map[string]bool{}
		}
		originals[key][id] = true
	}
	for _, file := range files {
		for _, request := range file.EmptyClaudeRequests {
			remember(replayKey{request.NativeMessageID, request.SessionID}, request.ID)
		}
		for _, entry := range file.Events {
			if entry.Event.Harness != "claude" || entry.Sidechain || entry.NativeMessageID == "" {
				continue
			}
			key := replayKey{entry.NativeMessageID, entry.Event.SessionID}
			owner := entry.ParentEventID
			if owner == "" {
				owner = entry.Event.ID
			}
			remember(key, owner)
			if tokenTotal(&entry.Event) > 0 {
				billed[key] = true
			}
		}
	}
	for _, file := range files {
		retracted := map[string]bool{}
		order := append([]string(nil), file.Retracted...)
		for _, id := range order {
			retracted[id] = true
		}
		kept := file.Events[:0:0]
		for _, entry := range file.Events {
			if entry.Event.Harness != "claude" || !entry.Sidechain || entry.NativeMessageID == "" {
				kept = append(kept, entry)
				continue
			}
			key := replayKey{entry.NativeMessageID, entry.Event.SessionID}
			identities := originals[key]
			if identities == nil || len(identities) != 1 || !billed[key] {
				kept = append(kept, entry)
				continue
			}
			owner := entry.ParentEventID
			if owner == "" {
				owner = entry.Event.ID
			}
			if !identities[owner] && !retracted[entry.Event.ID] {
				retracted[entry.Event.ID] = true
				order = append(order, entry.Event.ID)
			}
		}
		if len(kept) != len(file.Events) {
			file.Events = kept
		}
		if len(order) > 0 {
			file.Retracted = order
		}
	}
}

func filterPiReplays(files []*fileView) {
	var sessions []*fileView
	for _, file := range files {
		for _, entry := range file.Events {
			if entry.Event.Harness == "pi" {
				sessions = append(sessions, file)
				break
			}
		}
	}
	parents := map[string]*fileView{}
	native := map[string]map[string]bool{}
	for _, file := range sessions {
		parents[file.SessionID] = file
		set := map[string]bool{}
		prefix := len("pi:" + file.SessionID + ":")
		for _, entry := range file.Events {
			id := entry.Event.ID
			if len(id) >= prefix {
				id = id[prefix:]
			} else {
				id = ""
			}
			set[id+":"+entry.Event.Timestamp] = true
		}
		native[file.SessionID] = set
	}
	for _, file := range sessions {
		if file.ParentID == "" || file.ParentID == file.SessionID {
			continue
		}
		prefix := "pi:" + file.SessionID + ":"
		var events []*candidate
		for i, entry := range file.Events {
			if !strings.HasPrefix(entry.Event.ID, prefix) {
				continue
			}
			nativeID := entry.Event.ID[len(prefix):]
			oldID := entry.Event.ID
			session, id := entry.Event.SessionID, entry.Event.ID
			owner := file
			visited := map[string]bool{file.SessionID: true}
			for owner.ParentID != "" && !visited[owner.ParentID] {
				parent := parents[owner.ParentID]
				inherited := native[owner.ParentID][nativeID+":"+entry.Event.Timestamp]
				beforeCopy := owner.ForkedAt != "" && entry.Event.Timestamp < owner.ForkedAt
				if !inherited && !beforeCopy {
					break
				}
				session = owner.ParentID
				id = "pi:" + session + ":" + nativeID
				visited[owner.ParentID] = true
				if parent == nil {
					break
				}
				owner = parent
			}
			if id != oldID {
				if events == nil {
					events = append([]*candidate(nil), file.Events...)
				}
				moved := *entry
				moved.Event.SessionID, moved.Event.ID, moved.Sidechain = session, id, true
				events[i] = &moved
				file.Retracted = append(file.Retracted[:len(file.Retracted):len(file.Retracted)], oldID)
			}
		}
		if events != nil {
			file.Events = events
		}
	}
}

type codexReplay struct {
	timestamp                                       string
	input, output, cacheRead, cacheWrite, reasoning int64
}

func filterCodexReplays(files []*fileView) {
	parents := map[string]*fileView{}
	for _, file := range files {
		if file.harness() == "codex" {
			parents[file.SessionID] = file
		}
	}
	key := func(e *Event) codexReplay {
		return codexReplay{e.Timestamp, e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens}
	}
	parentReplays := map[*fileView]map[codexReplay]bool{}
	for _, child := range files {
		if child.harness() != "codex" || child.ParentID == "" || child.ParentID == child.SessionID {
			continue
		}
		parent := parents[child.ParentID]
		kept := child.Events[:0:0]
		for _, replay := range child.Events {
			if child.ForkedAt != "" && replay.Event.Timestamp < child.ForkedAt {
				continue
			}
			response := strings.TrimPrefix(replay.Event.ID, "codex:response:")
			if parent != nil && child.CompactionIDs[response] && parent.CompactionIDs[response] {
				continue
			}
			if parent == nil || parent == child || !strings.HasPrefix(replay.Event.ID, "codex:total:") {
				kept = append(kept, replay)
				continue
			}
			replays := parentReplays[parent]
			if replays == nil {
				replays = map[codexReplay]bool{}
				for _, original := range parent.Events {
					replays[key(&original.Event)] = true
				}
				parentReplays[parent] = replays
			}
			if !replays[key(&replay.Event)] {
				kept = append(kept, replay)
			}
		}
		child.Events = kept
		delete(parentReplays, child)
	}
}

// applyMuseParentProjects gives child Muse sessions their enclosing session's project.
func applyMuseParentProjects(files []*fileView) {
	var sessions []*fileView
	for _, file := range files {
		if file.WorkspaceRoot != "" {
			sessions = append(sessions, file)
			continue
		}
		for _, entry := range file.Events {
			if entry.Event.Harness == "muse" {
				sessions = append(sessions, file)
				break
			}
		}
	}
	if len(sessions) == 0 {
		return
	}
	projects := map[string]string{}
	parents := map[string]string{}
	for _, file := range sessions {
		if file.ParentID != "" {
			parents[file.SessionID] = file.ParentID
		}
		known := ""
		for _, entry := range file.Events {
			if entry.Event.Project != "Unknown project" {
				known = entry.Event.Project
				break
			}
		}
		if known == "" && file.WorkspaceRoot != "" {
			known = projectText(file.WorkspaceRoot)
		}
		if _, ok := projects[file.SessionID]; known != "" && !ok {
			projects[file.SessionID] = known
		}
	}
	for _, file := range files {
		if _, ok := parents[file.SessionID]; file.SessionID != "" && file.ParentID != "" && !ok {
			parents[file.SessionID] = file.ParentID
		}
	}
	for _, file := range sessions {
		project := projects[file.SessionID]
		visited := map[string]bool{file.SessionID: true}
		parent := parents[file.SessionID]
		for project == "" && parent != "" && !visited[parent] {
			visited[parent] = true
			project = projects[parent]
			parent = parents[parent]
		}
		if project == "" {
			continue
		}
		var events []*candidate
		for i, entry := range file.Events {
			if entry.Event.Project == "Unknown project" {
				if events == nil {
					events = append([]*candidate(nil), file.Events...)
				}
				moved := *entry
				moved.Event.Project = project
				events[i] = &moved
			}
		}
		if events != nil {
			file.Events = events
		}
	}
}

// deduplicate keeps one candidate per id: a primary record over a sidechain copy, then
// the larger token total. Order is each id's first appearance.
func deduplicate(files []*fileView) []*candidate {
	filterClaudeReplays(files)
	filterCodexReplays(files)
	filterPiReplays(files)
	applyMuseParentProjects(files)
	index := map[string]int{}
	var entries []*candidate
	for _, file := range files {
		for _, incoming := range file.Events {
			i, ok := index[incoming.Event.ID]
			if !ok {
				index[incoming.Event.ID] = len(entries)
				entries = append(entries, incoming)
				continue
			}
			existing := entries[i]
			if (existing.Sidechain && !incoming.Sidechain) ||
				(existing.Sidechain == incoming.Sidechain && tokenTotal(&incoming.Event) > tokenTotal(&existing.Event)) {
				entries[i] = incoming
			}
		}
	}
	return entries
}
