// Package bus mirrors the slice of the station bus the landing page needs:
// per address, the retained /meta, the /status LWT and /state.device_online.
// Two-layer liveness (station model): /status is the bridge process, the
// snapshot's device_online is the device link — the page shows both.
package bus

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

// Slot is the live view of one bus address.
type Slot struct {
	Address string
	// Meta is the parsed retained /meta (nil when none or not a JSON object).
	Meta     map[string]any
	MetaAt   time.Time
	Status   string // "online" / "offline" / "" (never seen or cleared)
	StatusAt time.Time
	// DeviceOnline is /state.device_online; nil when the snapshot omits it.
	DeviceOnline *bool
	StateAt      time.Time
}

// Store holds the live slots. Safe for concurrent use.
type Store struct {
	site string

	mu          sync.RWMutex
	slots       map[string]*Slot
	connected   bool
	changedAt   time.Time
	lastMessage time.Time
}

// NewStore returns an empty store for <site>/# topics.
func NewStore(site string) *Store {
	return &Store{site: strings.Trim(site, "/"), slots: map[string]*Slot{}}
}

// AddressOf splits <address>/<plane> for the planes the portal tracks. It
// returns ok=false for other topics (/cmd, non-site topics, bare planes).
func AddressOf(site, topic string) (address, plane string, ok bool) {
	i := strings.LastIndexByte(topic, '/')
	if i <= 0 {
		return "", "", false
	}
	address, plane = topic[:i], topic[i+1:]
	switch plane {
	case "meta", "state", "status":
	default:
		return "", "", false
	}
	if !strings.HasPrefix(address, site+"/") {
		return "", "", false
	}
	return address, plane, true
}

// Update applies one inbound message. An empty payload is a retained clear
// and forgets that plane.
func (s *Store) Update(topic string, payload []byte, now time.Time) {
	address, plane, ok := AddressOf(s.site, topic)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastMessage = now
	sl := s.slots[address]
	if sl == nil {
		sl = &Slot{Address: address}
		s.slots[address] = sl
	}
	switch plane {
	case "meta":
		sl.Meta, sl.MetaAt = nil, now
		if len(payload) > 0 {
			var m map[string]any
			if json.Unmarshal(payload, &m) == nil {
				sl.Meta = m
			}
		}
	case "status":
		sl.Status, sl.StatusAt = strings.TrimSpace(string(payload)), now
	case "state":
		sl.StateAt = now
		sl.DeviceOnline = nil
		if len(payload) > 0 {
			var st struct {
				DeviceOnline *bool `json:"device_online"`
			}
			if json.Unmarshal(payload, &st) == nil {
				sl.DeviceOnline = st.DeviceOnline
			}
		}
	}
	if sl.Meta == nil && sl.Status == "" && sl.StateAt.IsZero() && sl.DeviceOnline == nil {
		delete(s.slots, address)
	}
}

// SetConnected records the MQTT link state.
func (s *Store) SetConnected(up bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connected != up {
		s.connected, s.changedAt = up, now
	}
}

// Link reports the MQTT link state, when it last changed, and when the last
// message arrived.
func (s *Store) Link() (connected bool, changedAt, lastMessage time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connected, s.changedAt, s.lastMessage
}

// Snapshot returns a copy of all slots sorted by address.
func (s *Store) Snapshot() []Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Slot, 0, len(s.slots))
	for _, sl := range s.slots {
		c := *sl
		if sl.DeviceOnline != nil {
			v := *sl.DeviceOnline
			c.DeviceOnline = &v
		}
		out = append(out, c) // Meta maps are never mutated after decode
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// Facts are the device facts a /meta carries, flattened for display.
type Facts struct {
	Role, Model, Manufacturer, Serial, Firmware, Link, Host, Location string
}

// MetaFacts extracts display facts from a /meta object. Bridges differ in
// where they put things (device.link vs link), so both are read.
func MetaFacts(m map[string]any) Facts {
	if m == nil {
		return Facts{}
	}
	f := Facts{
		Role:     str(m["role"]),
		Link:     str(m["link"]),
		Host:     str(m["host"]),
		Location: str(m["location"]),
	}
	if d, ok := m["device"].(map[string]any); ok {
		f.Model = str(d["model"])
		if f.Model == "" {
			f.Model = str(d["name"])
		}
		f.Manufacturer = str(d["manufacturer"])
		f.Serial = str(d["serial"])
		f.Firmware = firstNonEmpty(str(d["firmware"]), str(d["sw_version"]), str(d["version"]))
		if f.Link == "" {
			f.Link = str(d["link"])
		}
	}
	if f.Manufacturer == "" {
		if e, ok := m["expose"].(map[string]any); ok {
			if d, ok := e["device"].(map[string]any); ok {
				f.Manufacturer = str(d["manufacturer"])
				if f.Model == "" {
					f.Model = str(d["model"])
				}
			}
		}
	}
	return f
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
