// Package engine owns the discovery lifecycle: it watches slot /meta announcements,
// renders HA discovery via package ha, publishes it (retained), re-publishes on Home
// Assistant birth, and removes discovery when a slot's /meta is cleared. It is
// idempotent — a byte-identical re-delivery of a retained /meta does not churn the bus.
//
// Because the broker, not this process, holds the discovery tree, each connect runs a
// sync pass (BeginSync … Sweep): it records which slot /meta the broker still carries and
// which retained discovery configs hadiscovery published earlier, then clears every
// config no current /meta renders — slots removed while hadiscovery was down, fields
// dropped across a restart, renamed object ids. The in-memory known map alone cannot see
// those (it starts empty on every start).
package engine

import (
	"bytes"
	"encoding/json"
	"log/slog"
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

	// Sync pass state (see package doc). Only the jobs worker touches these, but they sit
	// under mu with known for simplicity.
	syncGen    uint64              // incremented by BeginSync; a stale Sweep is ignored
	syncing    bool                // between BeginSync and its Sweep
	seen       map[string]bool     // slot addrs whose /meta arrived during this pass
	candidates map[string]struct{} // retained hadiscovery-origin config topics on the broker
	rendered   map[string]struct{} // topics rendered this pass, even if a publish failed
}

// originName is the `origin.name` hadiscovery stamps on every config (package ha). The sync
// pass only ever clears configs that carry it.
const originName = "hadiscovery"

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
		slog.Error("[hadiscovery] skip meta; dropping malformed announcement", "topic", metaTopic, "err", err)
		if addr, addrErr := expose.AddrFromMetaTopic(metaTopic); addrErr == nil {
			e.clearAddr(addr, "meta parse error")
		}
		return
	}
	e.markSeen(m.Addr)
	ents := ha.Render(e.prefix, e.area, m)
	noExpose := len(ents) == 0
	if noExpose {
		ents = []ha.Entity{ha.Diagnostic(e.prefix, e.area, m)}
	}

	e.mu.Lock()
	prev := e.known[m.Addr]
	if e.syncing {
		// A slot whose publish fails below keeps its previous known set; its current
		// topics must still count as live, or the sweep would clear valid configs.
		for _, ent := range ents {
			e.rendered[ent.Topic] = struct{}{}
		}
	}
	e.mu.Unlock()

	if entitiesEqual(prev, ents) {
		return // idempotent: retained re-delivery, nothing changed
	}
	// Log the no-expose fallback only when the rendered set actually changed (first
	// sighting or a transition), not on every byte-identical meta re-delivery — a slot
	// that republishes its /meta on a heartbeat would otherwise spam journald forever
	// even though the bus publish is correctly a no-op (Diagnostic is deterministic).
	if noExpose {
		slog.Info("[hadiscovery] slot has no expose block; emitting diagnostic only", "slot", m.Addr, "role", m.Role)
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
			slog.Error("[hadiscovery] clear failed", "topic", topic, "err", err)
			allOK = false
		}
	}

	for _, ent := range ents {
		if err := pub.Publish(ent.Topic, 1, true, ent.Payload); err != nil {
			slog.Error("[hadiscovery] publish failed", "topic", ent.Topic, "err", err)
			allOK = false
		}
	}

	if allOK {
		e.mu.Lock()
		e.known[m.Addr] = ents
		e.mu.Unlock()
		change := "updated"
		if prev == nil {
			change = "published"
		}
		slog.Info("[hadiscovery] discovery "+change, "slot", m.Addr, "entities", len(ents), "cleared", len(prevTopics))
	}
}

// BeginSync starts a sync pass. The bus layer calls it (on the engine's worker) on every
// connect, before subscribing, and schedules Sweep with the returned generation once the
// broker's retained /meta and discovery configs have been delivered.
func (e *Engine) BeginSync() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.syncGen++
	e.syncing = true
	e.seen = map[string]bool{}
	e.candidates = map[string]struct{}{}
	e.rendered = map[string]struct{}{}
	return e.syncGen
}

func (e *Engine) markSeen(addr string) {
	e.mu.Lock()
	if e.syncing {
		e.seen[addr] = true
	}
	e.mu.Unlock()
}

// OnConfig is called for discovery config messages under this deployment's node ids during
// a sync pass. A non-empty config published by hadiscovery becomes a sweep candidate; an
// empty payload (a clear, including the echo of our own) withdraws it. Configs from other
// publishers are never touched. Outside a sync pass it is a no-op.
func (e *Engine) OnConfig(topic string, payload []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.syncing {
		return
	}
	if len(payload) == 0 {
		delete(e.candidates, topic)
		return
	}
	var p struct {
		Origin struct {
			Name string `json:"name"`
		} `json:"origin"`
	}
	if json.Unmarshal(payload, &p) == nil && p.Origin.Name == originName {
		e.candidates[topic] = struct{}{}
	}
}

// SweepResult reports what a sync pass changed (for logging and tests).
type SweepResult struct {
	Slots, Entities, SlotsRemoved, OrphansCleared int
}

// Sweep ends the sync pass started by BeginSync(gen). It removes the discovery of every known
// slot whose /meta the broker no longer carries (cleared while hadiscovery was disconnected),
// then clears every recorded hadiscovery config that no current slot renders. It logs one
// summary line, so the journal shows the service in sync after every connect. A Sweep for an
// older generation (a reconnect started a new pass meanwhile) is ignored.
func (e *Engine) Sweep(gen uint64) (SweepResult, bool) {
	e.mu.Lock()
	if !e.syncing || gen != e.syncGen {
		e.mu.Unlock()
		return SweepResult{}, false
	}
	e.syncing = false
	var gone []string
	for addr := range e.known {
		if !e.seen[addr] {
			gone = append(gone, addr)
		}
	}
	candidates, rendered := e.candidates, e.rendered
	e.seen, e.candidates, e.rendered = nil, nil, nil
	e.mu.Unlock()

	var res SweepResult
	for _, addr := range gone {
		e.clearAddr(addr, "meta no longer on the broker")
		res.SlotsRemoved++
	}

	e.mu.Lock()
	live := map[string]struct{}{}
	for topic := range rendered {
		live[topic] = struct{}{}
	}
	for _, ents := range e.known {
		for _, ent := range ents {
			live[ent.Topic] = struct{}{}
		}
	}
	res.Slots = len(e.known)
	for _, ents := range e.known {
		res.Entities += len(ents)
	}
	e.mu.Unlock()

	pub := e.pubSnap()
	for topic := range candidates {
		if _, ok := live[topic]; ok {
			continue
		}
		if err := pub.Publish(topic, 1, true, []byte("")); err != nil {
			slog.Error("[hadiscovery] orphan clear failed", "topic", topic, "err", err)
			continue
		}
		slog.Info("[hadiscovery] cleared orphaned discovery config", "topic", topic)
		res.OrphansCleared++
	}
	slog.Info("[hadiscovery] discovery in sync", "slots", res.Slots, "entities", res.Entities,
		"slots_removed", res.SlotsRemoved, "orphans_cleared", res.OrphansCleared)
	return res, true
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
		slog.Info("[hadiscovery] HA online; no known slots to re-publish")
		return
	}
	slog.Info("[hadiscovery] HA online; re-publishing discovery", "slots", len(snapshot))
	pub := e.pubSnap()
	for _, ents := range snapshot {
		for _, ent := range ents {
			if err := pub.Publish(ent.Topic, 1, true, ent.Payload); err != nil {
				slog.Error("[hadiscovery] re-publish failed", "topic", ent.Topic, "err", err)
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
		slog.Error("[hadiscovery] skip meta-clear; malformed topic", "topic", metaTopic, "err", err)
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
	slog.Info("[hadiscovery] removing discovery entities", "reason", reason, "slot", addr, "entities", len(prev))
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
			slog.Error("[hadiscovery] clear failed", "reason", logPrefix, "topic", ent.Topic, "err", err)
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
