package deck

import (
	"bytes"
	"encoding/json"
	"sync"
)

// stream is one SSE stream per tab: a snapshot on connect, then only the live
// keys whose JSON changed, and `stale` events naming resources to refetch.
type stream struct {
	mu      sync.Mutex
	clients map[chan []byte]bool
	keys    []string
	live    map[string]json.RawMessage
}

func newStream() *stream {
	return &stream{clients: map[chan []byte]bool{}, live: map[string]json.RawMessage{}}
}

func (s *stream) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

// object writes keys and values as a JSON object in order; a nil value is null.
func object(keys []string, values map[string]json.RawMessage) []byte {
	var out bytes.Buffer
	out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		name, _ := json.Marshal(key)
		out.Write(name)
		out.WriteByte(':')
		if value := values[key]; value != nil {
			out.Write(value)
		} else {
			out.WriteString("null")
		}
	}
	out.WriteByte('}')
	return out.Bytes()
}

func frame(kind string, body []byte) []byte {
	return []byte(`data: {"type":"` + kind + `","live":` + string(body) + "}\n\n")
}

// subscribe returns a client's channel, which starts with the snapshot.
func (s *stream) subscribe() chan []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	client := make(chan []byte, 64)
	client <- frame("snapshot", object(s.keys, s.live))
	s.clients[client] = true
	return client
}

func (s *stream) unsubscribe(client chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, client)
}

func (s *stream) publish(entries []liveEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := map[string]json.RawMessage{}
	var keys, changed []string
	patch := map[string]json.RawMessage{}
	for _, entry := range entries {
		text := encode(entry.value)
		next[entry.key] = text
		keys = append(keys, entry.key)
		if previous, ok := s.live[entry.key]; !ok || !bytes.Equal(previous, text) {
			changed = append(changed, entry.key)
			patch[entry.key] = text
		}
	}
	// A changed key keeps its place, so the client's order matches a fresh snapshot.
	var order []string
	for _, key := range s.keys {
		if _, ok := next[key]; ok {
			order = append(order, key)
		} else {
			changed = append(changed, key)
		}
	}
	for _, key := range keys {
		if _, ok := s.live[key]; !ok {
			order = append(order, key)
		}
	}
	s.keys, s.live = order, next
	if len(changed) > 0 {
		s.sendLocked(frame("patch", object(changed, patch)))
	}
}

func (s *stream) stale(tags ...string) {
	if len(tags) == 0 {
		return
	}
	body, _ := json.Marshal(struct {
		Type string   `json:"type"`
		Tags []string `json:"tags"`
	}{"stale", tags})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendLocked([]byte("data: " + string(body) + "\n\n"))
}

func (s *stream) heartbeat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendLocked([]byte(": \n\n"))
}

// sendLocked drops a client too slow to keep up; its tab reconnects and takes a fresh snapshot.
func (s *stream) sendLocked(data []byte) {
	for client := range s.clients {
		select {
		case client <- data:
		default:
			delete(s.clients, client)
			close(client)
		}
	}
}
