// Package web serves the test UI and the HTTP API the browser drives.
//
// tree.go is the in-memory model of the station bus: a map of slot address -> per-plane
// last payload + timestamp, fed by mqtt.Client and snapshotted to SSE subscribers.
package web

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"testui/internal/mqtt"
)

// plane is one of the four topic suffixes per slot (integration model §8).
type plane string

const (
	planeMeta   plane = "meta"
	planeState  plane = "state"
	planeStatus plane = "status"
	planeCmd    plane = "cmd"
)

// Plane holds the last payload for one plane of a slot. It is the JSON shape the browser
// receives in BOTH the snapshot (embedded in Slot) and the live update stream (as an
// Event), so the two code paths share one schema: lowercase tags, a json.RawMessage
// payload (raw JSON for /state and /meta, a JSON-quoted string for /status), and a
// pre-decoded object view the UI renders directly without re-parsing.
type Plane struct {
	Topic    string          `json:"topic"`
	Payload  json.RawMessage `json:"payload"`
	Retained bool            `json:"retained"`
	TS       time.Time       `json:"ts"`
	// JSON-decoded view of Payload when it is a JSON object (nil for /status, which is a
	// bare string). Shipped to the browser so it need not JSON.parse the payload again —
	// valid-JSON object payloads arrive as JS objects, not strings.
	Object map[string]any `json:"object,omitempty"`
}

// Slot is the per-address model: meta/state/status/cmd planes.
type Slot struct {
	Address string `json:"address"`
	Meta    *Plane `json:"meta,omitempty"`
	State   *Plane `json:"state,omitempty"`
	Status  *Plane `json:"status,omitempty"`
	Cmd     *Plane `json:"cmd,omitempty"`
}

// Event is one SSE update: a plane change for a slot, fanned out to subscribers. Its JSON
// shape matches Plane (plus address/plane/cleared), so the browser applies snapshot and
// update events through the same path. Critically this is NOT mqtt.Message — that struct
// has no JSON tags and a []byte payload, which would serialize to capitalized keys and
// base64 and break the live stream.
type Event struct {
	Topic    string          `json:"topic"`             // full bus topic (e.g. muehle/hf/radio/state)
	Address  string          `json:"address"`           // slot address (topic minus the plane suffix)
	Plane    string          `json:"plane,omitempty"`   // meta|state|status|cmd; "" for non-plane topics
	Payload  json.RawMessage `json:"payload,omitempty"` // wrapped same way as Plane.Payload
	Object   map[string]any  `json:"object,omitempty"`  // decoded view, same as Plane.Object
	Retained bool            `json:"retained"`
	TS       time.Time       `json:"ts"`
	Cleared  bool            `json:"cleared,omitempty"` // empty retained payload cleared the plane
}

// Tree is the in-memory bus model. It implements mqtt.Store.
type Tree struct {
	mu     sync.RWMutex
	slots  map[string]*Slot
	orders []string // insertion order for stable rendering

	subsMu sync.RWMutex
	subs   map[chan *Event]struct{}
}

// NewTree creates an empty tree.
func NewTree() *Tree {
	return &Tree{
		slots: make(map[string]*Slot),
		subs:  make(map[chan *Event]struct{}),
	}
}

// Update implements mqtt.Store: routes an inbound message to the right slot+plane and
// fans an Event out to SSE subscribers. Called on the mqtt jobs worker.
func (t *Tree) Update(m mqtt.Message) {
	address, pl := splitTopic(m.Topic)
	planeName := string(pl)

	t.mu.Lock()
	s, ok := t.slots[address]
	if !ok {
		s = &Slot{Address: address}
		t.slots[address] = s
		t.orders = append(t.orders, address)
	}

	ev := &Event{
		Topic:    m.Topic,
		Address:  address,
		Plane:    planeName,
		Retained: m.Retained,
		TS:       m.TS,
	}

	// An empty payload on a retained topic is a clear: drop the plane so the UI reflects
	// "no retained value" rather than a stale one, and signal the clear to subscribers.
	if len(m.Payload) == 0 {
		switch pl {
		case planeMeta:
			s.Meta = nil
		case planeState:
			s.State = nil
		case planeStatus:
			s.Status = nil
		case planeCmd:
			s.Cmd = nil
		}
		ev.Cleared = true
		t.mu.Unlock()
		t.broadcast(ev)
		return
	}

	// Payload is json.RawMessage, which json.Marshal validates on output. Planes like
	// /status carry a bare string ("online") that is not valid JSON; wrap it as a JSON
	// string so the snapshot/update events always serialize (the UI strips the quotes).
	// /state and /meta are already JSON objects and pass through unchanged.
	payload := m.Payload
	if !json.Valid(payload) {
		if b, err := json.Marshal(string(payload)); err == nil {
			payload = b
		}
	}
	p := &Plane{
		Topic:    m.Topic,
		Payload:  json.RawMessage(payload),
		Retained: m.Retained,
		TS:       m.TS,
	}
	// Decode the object view the browser renders directly (avoids a client-side re-parse
	// that would fail on the already-parsed object payload).
	var obj map[string]any
	if json.Unmarshal(payload, &obj) == nil {
		p.Object = obj
	}

	switch pl {
	case planeMeta:
		s.Meta = p
	case planeState:
		s.State = p
	case planeStatus:
		s.Status = p
	case planeCmd:
		s.Cmd = p
	}
	t.mu.Unlock()

	ev.Payload = p.Payload
	ev.Object = p.Object
	t.broadcast(ev)
}

// splitTopic splits "<addr>/<plane>" into the slot address and the plane. Topics that do
// not end in one of the four plane suffixes (e.g. a station node "muehle/hf" published
// directly, or a sub-topic like radio/receiver/N/state) are handled gracefully: the
// deepest trailing segment that is a known plane is split off, and the rest is the
// address. If no plane matches, the whole topic is the address and the plane is "".
func splitTopic(topic string) (address string, pl plane) {
	segs := strings.Split(topic, "/")
	if len(segs) == 0 {
		return topic, ""
	}
	last := segs[len(segs)-1]
	switch plane(last) {
	case planeMeta, planeState, planeStatus, planeCmd:
		return strings.Join(segs[:len(segs)-1], "/"), plane(last)
	}
	return topic, ""
}

// Snapshot returns the current tree (all slots) plus the slot order, for the initial
// SSE burst and the /api/tree endpoint.
func (t *Tree) Snapshot() (slots []*Slot, order []string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, addr := range t.orders {
		if s := t.slots[addr]; s != nil {
			slots = append(slots, s)
		}
	}
	return slots, t.orders
}

// Subscribe registers a channel for live Event broadcasts. Returns an unsubscribe
// function. The channel is buffered; slow receivers are dropped (non-blocking send in
// broadcast) — for a state mirror dropping is preferable to blocking the bus dispatch.
func (t *Tree) Subscribe() (ch chan *Event, cancel func()) {
	ch = make(chan *Event, 64)
	t.subsMu.Lock()
	t.subs[ch] = struct{}{}
	t.subsMu.Unlock()
	cancel = func() {
		t.subsMu.Lock()
		delete(t.subs, ch)
		t.subsMu.Unlock()
		close(ch)
	}
	return ch, cancel
}

func (t *Tree) broadcast(ev *Event) {
	t.subsMu.RLock()
	defer t.subsMu.RUnlock()
	for ch := range t.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop rather than block the worker
		}
	}
}