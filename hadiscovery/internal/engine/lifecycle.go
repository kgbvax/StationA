// Package engine owns the discovery lifecycle: it watches slot /meta announcements,
// renders HA discovery via package ha, publishes it (retained), re-publishes on Home
// Assistant birth, and removes discovery when a slot's /meta is cleared. It is
// idempotent — a byte-identical re-delivery of a retained /meta does not churn the bus.
package engine

import (
	"bytes"
	"log"
	"strings"
	"sync"

	"hadiscovery/internal/expose"
	"hadiscovery/internal/ha"
)

// Pub is the narrow publish interface the engine needs, so lifecycle tests can use a fake.
type Pub interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
}

// Engine tracks the slots it has rendered discovery for and drives (re)publish/clear.
type Engine struct {
	prefix string
	area   string
	pub    Pub

	mu    sync.Mutex
	known map[string][]ha.Entity // key = slot addr
}

// NewEngine returns an engine that publishes discovery under prefix and suggests area as
// the HA `suggested_area` for any device whose own expose.device.area is unset. It starts
// with a noop publisher; call SetPub once the bus client exists so renders reach the broker.
func NewEngine(prefix, area string) *Engine {
	if prefix == "" {
		prefix = "homeassistant"
	}
	return &Engine{prefix: prefix, area: area, pub: noopPub{}, known: map[string][]ha.Entity{}}
}

// SetPub replaces the publisher. Called once after the bus client is constructed but
// before it connects (so OnConnect-delivered retained /meta reaches the real publisher).
func (e *Engine) SetPub(pub Pub) {
	if pub == nil {
		pub = noopPub{}
	}
	e.mu.Lock()
	e.pub = pub
	e.mu.Unlock()
}

// noopPub drops every publish; used until SetPub wires the real bus client.
type noopPub struct{}

func (noopPub) Publish(string, byte, bool, []byte) error { return nil }

// pubSnap returns the current publisher under lock so render callbacks (which run on the
// paho goroutine) race-safely read the value SetPub wrote on the main goroutine.
func (e *Engine) pubSnap() Pub {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pub
}

// OnMeta is called for every /meta message. A zero-length payload is treated as a clear
// (slot decommissioned). A parse failure is logged and any previously rendered discovery for
// that slot is cleared so stale HA entities do not persist. A slot with no `expose` block
// gets a single diagnostic sensor. Otherwise its discovery entities are published retained;
// a byte-identical re-delivery is a no-op. When the rendered set shrinks, the dropped
// entities' discovery topics are cleared (empty retained payload) so HA removes them.
func (e *Engine) OnMeta(metaTopic string, payload []byte) {
	if len(payload) == 0 {
		e.OnMetaCleared(metaTopic)
		return
	}
	m, err := expose.Parse(metaTopic, payload)
	if err != nil {
		log.Printf("[hadiscovery] skip meta %s: %v", metaTopic, err)
		if addr, addrErr := expose.AddrFromMetaTopic(metaTopic); addrErr == nil {
			e.clearAddr(addr, "meta parse error")
		}
		return
	}
	ents := ha.Render(e.prefix, e.area, m)
	noExpose := len(ents) == 0
	if noExpose {
		ents = []ha.Entity{ha.Diagnostic(e.prefix, e.area, m)}
	}

	e.mu.Lock()
	prev := e.known[m.Addr]
	e.mu.Unlock()

	if entitiesEqual(prev, ents) {
		return // idempotent: retained re-delivery, nothing changed
	}
	// Log the no-expose fallback only when the rendered set actually changed (first
	// sighting or a transition), not on every byte-identical meta re-delivery — a slot
	// that republishes its /meta on a heartbeat would otherwise spam journald forever
	// even though the bus publish is correctly a no-op (Diagnostic is deterministic).
	if noExpose {
		log.Printf("[hadiscovery] slot %s role=%s has no expose block; emitting diagnostic only", m.Addr, m.Role)
	}
	pub := e.pubSnap()

	// Clear discovery topics for entities that no longer exist (field/action removed),
	// then publish the current entity set. Only update known when every publish succeeds,
	// so a broker/ACK failure leaves the previous state in place and the next /meta
	// delivery retries.
	prevTopics := topicSet(prev)
	for _, ent := range ents {
		delete(prevTopics, ent.Topic)
	}
	allOK := true
	for topic := range prevTopics {
		if err := pub.Publish(topic, 1, true, []byte("")); err != nil {
			log.Printf("[hadiscovery] clear %s: %v", topic, err)
			allOK = false
		}
	}

	for _, ent := range ents {
		if err := pub.Publish(ent.Topic, 1, true, ent.Payload); err != nil {
			log.Printf("[hadiscovery] publish %s: %v", ent.Topic, err)
			allOK = false
		}
	}

	if allOK {
		e.mu.Lock()
		e.known[m.Addr] = ents
		e.mu.Unlock()
	}
}

// OnHAStatus is called for homeassistant/status messages. On payload "online" (HA just
// restarted and lost its discovered entities) it re-publishes every known slot's discovery.
func (e *Engine) OnHAStatus(payload string) {
	if strings.TrimSpace(payload) != "online" {
		return
	}
	e.mu.Lock()
	snapshot := make(map[string][]ha.Entity, len(e.known))
	for k, v := range e.known {
		snapshot[k] = v
	}
	e.mu.Unlock()
	if len(snapshot) == 0 {
		log.Printf("[hadiscovery] HA online; no known slots to re-publish")
		return
	}
	log.Printf("[hadiscovery] HA online; re-publishing discovery for %d slot(s)", len(snapshot))
	pub := e.pubSnap()
	for _, ents := range snapshot {
		for _, ent := range ents {
			if err := pub.Publish(ent.Topic, 1, true, ent.Payload); err != nil {
				log.Printf("[hadiscovery] re-publish %s: %v", ent.Topic, err)
			}
		}
	}
}

// OnMetaCleared is called when a retained /meta topic is received with a zero-length
// payload (slot decommissioned). It publishes an empty retained payload to each of that
// slot's discovery config topics so HA removes the entities, then forgets the slot. A
// clear for a slot we never saw is a no-op.
func (e *Engine) OnMetaCleared(metaTopic string) {
	addr, err := expose.AddrFromMetaTopic(metaTopic)
	if err != nil {
		log.Printf("[hadiscovery] skip meta-clear %s: %v", metaTopic, err)
		return
	}
	e.clearAddr(addr, "meta cleared")
}

// clearAddr publishes empty retained payloads to every discovery topic for addr and deletes
// the slot from known only if all publishes succeed. reason is used only in log messages.
func (e *Engine) clearAddr(addr, reason string) {
	e.mu.Lock()
	prev := e.known[addr]
	e.mu.Unlock()
	if len(prev) == 0 {
		return
	}
	log.Printf("[hadiscovery] %s for %s; removing %d discovery entity(ies)", reason, addr, len(prev))
	pub := e.pubSnap()
	if e.clearEntities(pub, prev, "clear") {
		e.mu.Lock()
		delete(e.known, addr)
		e.mu.Unlock()
	}
}

// clearEntities publishes an empty retained payload to each entity's discovery topic.
// It returns true only if every publish succeeds.
func (e *Engine) clearEntities(pub Pub, ents []ha.Entity, logPrefix string) bool {
	allOK := true
	for _, ent := range ents {
		if err := pub.Publish(ent.Topic, 1, true, []byte("")); err != nil {
			log.Printf("[hadiscovery] %s %s: %v", logPrefix, ent.Topic, err)
			allOK = false
		}
	}
	return allOK
}

// KnownAddrs returns the addresses of the slots the engine currently has discovery for
// (for diagnostics/tests).
func (e *Engine) KnownAddrs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.known))
	for a := range e.known {
		out = append(out, a)
	}
	return out
}

func topicSet(ents []ha.Entity) map[string]struct{} {
	s := make(map[string]struct{}, len(ents))
	for _, ent := range ents {
		s[ent.Topic] = struct{}{}
	}
	return s
}

func entitiesEqual(a, b []ha.Entity) bool {
	if len(a) != len(b) {
		return false
	}
	// Order is deterministic (Render walks fields/actions in order), so a direct compare
	// is stable. Compare topic + payload.
	for i := range a {
		if a[i].Topic != b[i].Topic || !bytes.Equal(a[i].Payload, b[i].Payload) {
			return false
		}
	}
	return true
}
