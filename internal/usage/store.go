package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store holds every device's events, deduplicated and priced with the current pricing.
type Store interface {
	Events() []Event         // read-only to callers; a new slice after each change
	Devices() []Device       // the server itself first, syncedAt = now for it
	Revision() uint64        // bumps on any change to events or prices
	Subscribe(func()) func() // called after each change; returns cancel
}

type deviceLog struct {
	device  Device
	events  []Event
	index   map[string]int
	journal *os.File
	lines   int
}

// FileStore keeps each device's events in dir/events/<device>.ndjson, an append-only
// journal of upserts and withdrawals that is rewritten when it grows past twice its
// live size, and the device list in dir/devices.json.
type FileStore struct {
	dir     string
	localID string
	label   string
	pricing *Pricing

	mu       sync.Mutex
	devices  map[string]*deviceLog
	revision uint64
	strs     map[string]string
	ptrs     map[string]*string

	snapshotMu       sync.Mutex
	snapshot         []Event
	snapshotRevision uint64
	snapshotPolicy   *PricingPolicy

	subsMu  sync.Mutex
	subs    map[int]func()
	nextSub int
	wake    chan struct{}
}

type journalLine struct {
	Event   *Event `json:"e,omitempty"`
	Deleted string `json:"d,omitempty"`
}

// OpenStore loads the store in dir for the local device localID, labeled label.
func OpenStore(dir, localID, label string, pricing *Pricing) (*FileStore, error) {
	s := &FileStore{
		dir: dir, localID: localID, label: label, pricing: pricing,
		devices: map[string]*deviceLog{}, subs: map[int]func(){}, wake: make(chan struct{}, 1),
		strs: map[string]string{}, ptrs: map[string]*string{},
	}
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o700); err != nil {
		return nil, err
	}
	var devices []Device
	if data, err := os.ReadFile(filepath.Join(dir, "devices.json")); err == nil {
		if err = json.Unmarshal(data, &devices); err != nil {
			return nil, err
		}
	}
	for _, device := range devices {
		log := &deviceLog{device: device, index: map[string]int{}}
		if err := s.replay(log); err != nil {
			return nil, err
		}
		s.devices[device.ID] = log
	}
	if s.devices[localID] == nil {
		s.devices[localID] = &deviceLog{device: Device{ID: localID, Label: label}, index: map[string]int{}}
	}
	s.devices[localID].device.Label = label
	pricing.onChange = s.changed
	go s.notifyLoop()
	return s, nil
}

func (s *FileStore) journalPath(id string) string {
	return filepath.Join(s.dir, "events", url.PathEscape(id)+".ndjson")
}

func (s *FileStore) replay(log *deviceLog) error {
	file, err := os.Open(s.journalPath(log.device.ID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		var line journalLine
		if json.Unmarshal(scanner.Bytes(), &line) != nil {
			continue
		}
		log.lines++
		if line.Event != nil {
			s.intern(line.Event)
			log.put(*line.Event)
		} else if line.Deleted != "" {
			log.remove(line.Deleted)
		}
	}
	return scanner.Err()
}

// intern shares the strings many records repeat (projects, models, sessions, titles).
func (s *FileStore) intern(e *Event) {
	for _, field := range []*string{&e.Harness, &e.Model, &e.RawModel, &e.Project, &e.SessionID, &e.DeviceID,
		&e.SessionTitle, &e.ProjectName, &e.T3ThreadID, &e.T3ThreadURL, &e.Entrypoint} {
		if v, ok := s.strs[*field]; ok {
			*field = v
		} else {
			s.strs[*field] = *field
		}
	}
	for _, field := range []**string{&e.Repository, &e.ServiceTier} {
		if *field == nil {
			continue
		}
		if v, ok := s.ptrs[**field]; ok {
			*field = v
		} else {
			s.ptrs[**field] = *field
		}
	}
}

func (l *deviceLog) put(e Event) {
	if i, ok := l.index[e.ID]; ok {
		l.events[i] = e
		return
	}
	l.index[e.ID] = len(l.events)
	l.events = append(l.events, e)
}

func (l *deviceLog) remove(id string) bool {
	i, ok := l.index[id]
	if !ok {
		return false
	}
	last := len(l.events) - 1
	if i != last {
		l.events[i] = l.events[last]
		l.index[l.events[i].ID] = i
	}
	l.events[last] = Event{}
	l.events = l.events[:last]
	delete(l.index, id)
	return true
}

func (s *FileStore) append(log *deviceLog, lines []journalLine) error {
	if len(lines) == 0 {
		return nil
	}
	if log.journal == nil {
		file, err := os.OpenFile(s.journalPath(log.device.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		log.journal = file
	}
	w := bufio.NewWriterSize(log.journal, 1<<16)
	for _, line := range lines {
		data, err := json.Marshal(line)
		if err != nil {
			return err
		}
		_, _ = w.Write(append(data, '\n'))
	}
	log.lines += len(lines)
	if err := w.Flush(); err != nil {
		return err
	}
	if log.lines > 2*len(log.events)+10_000 {
		return s.compact(log)
	}
	return nil
}

func (s *FileStore) compact(log *deviceLog) error {
	temp, err := os.CreateTemp(filepath.Join(s.dir, "events"), "compact-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	w := bufio.NewWriterSize(temp, 1<<20)
	for i := range log.events {
		data, _ := json.Marshal(journalLine{Event: &log.events[i]})
		_, _ = w.Write(append(data, '\n'))
	}
	if err = w.Flush(); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp.Name(), s.journalPath(log.device.ID)); err != nil {
		return err
	}
	if log.journal != nil {
		_ = log.journal.Close()
		log.journal = nil
	}
	log.lines = len(log.events)
	return nil
}

func (s *FileStore) saveDevicesLocked() error {
	devices := make([]Device, 0, len(s.devices))
	for _, log := range s.devices {
		devices = append(devices, log.device)
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	data, err := json.MarshalIndent(devices, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, "devices.json"), append(data, '\n'))
}

// Batch is one device's changes: records to add or correct, and ids proved to be replays.
type Batch struct {
	Device     Device   `json:"device"`
	Events     []Event  `json:"events"`
	DeletedIDs []string `json:"deletedIds,omitempty"`
}

// Result counts what a batch changed.
type Result struct {
	Accepted int `json:"accepted"`
	Updated  int `json:"updated"`
	Deleted  int `json:"deleted"`
}

// Apply upserts a device's records and withdraws its retracted ids. Optional session
// metadata a collector couldn't read this time keeps its previous value. When fill is
// set (history import) existing records are kept and nothing is withdrawn.
func (s *FileStore) Apply(batch Batch, fill bool) (Result, error) {
	var result Result
	id := batch.Device.ID
	if strings.TrimSpace(id) == "" || len(id) > 200 {
		return result, errors.New("a valid device id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	log := s.devices[id]
	created := log == nil
	if created {
		log = &deviceLog{device: Device{ID: id}, index: map[string]int{}}
		s.devices[id] = log
	}
	if label := strings.TrimSpace(batch.Device.Label); label != "" && id != s.localID {
		log.device.Label = label
	}
	if batch.Device.SyncedAt != nil {
		log.device.SyncedAt = batch.Device.SyncedAt
	} else if !fill {
		now := time.Now().UnixMilli()
		log.device.SyncedAt = &now
	}
	var lines []journalLine
	for _, incoming := range batch.Events {
		if incoming.ID == "" {
			continue
		}
		i, exists := log.index[incoming.ID]
		if exists && fill {
			continue
		}
		var previous *Event
		if exists {
			previous = &log.events[i]
		}
		normalized := mergeEvent(incoming, previous, id)
		if previous != nil && eventsEqual(&normalized, previous) {
			continue
		}
		saved := new(Event)
		*saved = normalized
		s.intern(saved)
		if exists {
			result.Updated++
		} else {
			result.Accepted++
		}
		log.put(*saved)
		lines = append(lines, journalLine{Event: saved})
	}
	if !fill {
		for _, deleted := range batch.DeletedIDs {
			if log.remove(deleted) {
				result.Deleted++
				lines = append(lines, journalLine{Deleted: deleted})
			}
		}
	}
	err := s.append(log, lines)
	if saveErr := s.saveDevicesLocked(); err == nil {
		err = saveErr
	}
	if len(lines) > 0 || created {
		s.revision++
		s.signal()
	}
	return result, err
}

func isRemoteRepository(r *string) bool {
	return r != nil && *r != "" && !strings.HasPrefix(*r, "local:")
}

// mergeEvent is token-tracker's normalization of an uploaded record. A record that names
// its T3 thread is authoritative for the thread's link (an empty link clears it).
func mergeEvent(incoming Event, previous *Event, deviceID string) Event {
	e := incoming
	e.DeviceID = deviceID
	if !e.CostKnown {
		e.CostUSD = 0
	}
	if previous == nil {
		return e
	}
	if e.SessionTitle == "" {
		e.SessionTitle = previous.SessionTitle
	}
	if e.ProjectName == "" {
		e.ProjectName = previous.ProjectName
	}
	if e.Entrypoint == "" {
		e.Entrypoint = previous.Entrypoint
	}
	if incoming.T3ThreadID == "" && incoming.T3ThreadURL == "" {
		e.T3ThreadID, e.T3ThreadURL = previous.T3ThreadID, previous.T3ThreadURL
		if previous.T3ThreadID != "" && isRemoteRepository(previous.Repository) && !isRemoteRepository(incoming.Repository) &&
			incoming.Project == previous.Project && incoming.Harness == previous.Harness && incoming.SessionID == previous.SessionID {
			e.Repository = previous.Repository
		}
	}
	return e
}

func eventsEqual(a, b *Event) bool {
	strEq := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	intEq := func(x, y *int64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	floatEq := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.ID == b.ID && a.Timestamp == b.Timestamp && a.Harness == b.Harness && a.Model == b.Model &&
		a.RawModel == b.RawModel && a.Project == b.Project && strEq(a.Repository, b.Repository) &&
		a.SessionID == b.SessionID && a.InputTokens == b.InputTokens && a.OutputTokens == b.OutputTokens &&
		a.CacheReadTokens == b.CacheReadTokens && a.CacheWriteTokens == b.CacheWriteTokens &&
		a.ReasoningTokens == b.ReasoningTokens && a.UnclassifiedTokens == b.UnclassifiedTokens && a.CostUSD == b.CostUSD && floatEq(a.ReportedCostUSD, b.ReportedCostUSD) &&
		a.CostKnown == b.CostKnown && strEq(a.ServiceTier, b.ServiceTier) && intEq(a.CacheWrite1hTokens, b.CacheWrite1hTokens) &&
		intEq(a.Requests, b.Requests) && a.DeviceID == b.DeviceID && a.SessionTitle == b.SessionTitle &&
		a.ProjectName == b.ProjectName && a.T3ThreadID == b.T3ThreadID && a.T3ThreadURL == b.T3ThreadURL &&
		a.Entrypoint == b.Entrypoint && a.Source == b.Source && a.ResponseID == b.ResponseID &&
		a.UpstreamRequestID == b.UpstreamRequestID && a.AccountID == b.AccountID && a.Failed == b.Failed
}

// Events is every device's records, one per harness and id across devices (the larger
// token total, then the server's own copy, then the lower device id), repriced with the
// current policy and sorted by time and id.
func (s *FileStore) Events() []Event {
	policy := s.pricing.Policy()
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	s.mu.Lock()
	revision := s.revision
	if s.snapshot != nil && s.snapshotRevision == revision && s.snapshotPolicy == policy {
		s.mu.Unlock()
		return s.snapshot
	}
	type key struct{ harness, id string }
	total := 0
	for _, log := range s.devices {
		total += len(log.events)
	}
	index := make(map[key]int, total)
	events := make([]Event, 0, total)
	ids := make([]string, 0, len(s.devices))
	for id := range s.devices {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if (ids[i] == s.localID) != (ids[j] == s.localID) {
			return ids[j] == s.localID
		}
		return ids[i] < ids[j]
	})
	for _, deviceID := range ids {
		for _, incoming := range s.devices[deviceID].events {
			k := key{incoming.Harness, incoming.ID}
			i, ok := index[k]
			if !ok {
				index[k] = len(events)
				events = append(events, incoming)
				continue
			}
			existing := &events[i]
			preferred := incoming.DeviceID == s.localID ||
				(existing.DeviceID != s.localID && incoming.DeviceID < existing.DeviceID)
			if tokenTotal(&incoming) > tokenTotal(existing) || (tokenTotal(&incoming) == tokenTotal(existing) && preferred) {
				*existing = incoming
			}
		}
	}
	s.mu.Unlock()
	events = reconcileProxy(events)
	for i := range events {
		events[i], _ = policy.Reprice(events[i])
	}
	sortEvents(events)
	s.snapshot, s.snapshotRevision, s.snapshotPolicy = events, revision, policy
	return events
}

// Devices lists the server first, synced now, then other devices by label.
func (s *FileStore) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	out := []Device{{ID: s.localID, Label: s.label, SyncedAt: &now}}
	var others []Device
	for id, log := range s.devices {
		if id != s.localID {
			others = append(others, log.device)
		}
	}
	sort.Slice(others, func(i, j int) bool {
		if others[i].Label != others[j].Label {
			return others[i].Label < others[j].Label
		}
		return others[i].ID < others[j].ID
	})
	return append(out, others...)
}

// Revision bumps on any change to events or prices.
func (s *FileStore) Revision() uint64 {
	s.pricing.Policy()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

// Subscribe calls fn after changes; bursts of changes may share one call.
func (s *FileStore) Subscribe(fn func()) func() {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	id := s.nextSub
	s.nextSub++
	s.subs[id] = fn
	return func() {
		s.subsMu.Lock()
		defer s.subsMu.Unlock()
		delete(s.subs, id)
	}
}

func (s *FileStore) changed() {
	s.mu.Lock()
	s.revision++
	s.mu.Unlock()
	s.signal()
}

func (s *FileStore) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *FileStore) notifyLoop() {
	for range s.wake {
		s.subsMu.Lock()
		subs := make([]func(), 0, len(s.subs))
		for _, fn := range s.subs {
			subs = append(subs, fn)
		}
		s.subsMu.Unlock()
		for _, fn := range subs {
			fn()
		}
	}
}

// DeviceEvents returns a copy of one device's stored records.
func (s *FileStore) DeviceEvents(id string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	log := s.devices[id]
	if log == nil {
		return nil
	}
	return append([]Event(nil), log.events...)
}

// Pricing is the policy the store prices with.
func (s *FileStore) Pricing() *Pricing { return s.pricing }
