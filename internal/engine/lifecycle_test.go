package engine

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"hadiscovery/internal/expose"
	"hadiscovery/internal/ha"
)

// fakePub records every publish call in order.
type fakePub struct {
	calls []pubCall
}

type pubCall struct {
	topic    string
	retained bool
	payload  []byte
}

func (f *fakePub) Publish(topic string, _ byte, retained bool, payload []byte) error {
	f.calls = append(f.calls, pubCall{topic: topic, retained: retained, payload: append([]byte(nil), payload...)})
	return nil
}

// callsTo returns the publish calls whose topic has the given suffix (e.g. "/config" or
// "/status"), in order.
func (f *fakePub) callsTo(suffix string) []pubCall {
	var out []pubCall
	for _, c := range f.calls {
		if strings.HasSuffix(c.topic, suffix) {
			out = append(out, c)
		}
	}
	return out
}

// metaFor builds a minimal SlotMeta JSON payload with the given expose fields.
func metaFor(slot string, fields []expose.Field) ([]byte, string) {
	topic := "muehle/hf/" + slot + "/meta"
	if fields == nil {
		return []byte(`{"schema":"1.0","role":"` + slot + `","capabilities":{"modes":["cw","usb"]}}`), topic
	}
	// Build a tiny JSON with one field of each requested type; reuse a hand-rolled string
	// to avoid pulling encoding/json into fixtures. Tests only need Render to emit >0
	// entities, so a single number field suffices.
	return []byte(`{"schema":"1.0","role":"radio","capabilities":{"bands":["40m"]},"expose":{"device":{"name":"D","model":"D"},"fields":[{"key":"freq_hz","name":"Frequency","type":"number","unit":"Hz","class":"frequency","state_class":"measurement"}]}}`), topic
}

// TestOnMetaPublishes runs a slot meta through the engine and asserts retained discovery
// config messages land on the right topics.
func TestOnMetaPublishes(t *testing.T) {
	eng := NewEngine("homeassistant", "")
	pub := &fakePub{}
	eng.SetPub(pub)

	payload, topic := metaFor("radio", []expose.Field{{Key: "freq_hz", Type: "number"}})
	eng.OnMeta(topic, payload)

	cfgs := pub.callsTo("/config")
	if len(cfgs) != 1 {
		t.Fatalf("config publishes = %d, want 1: %+v", len(cfgs), pub.calls)
	}
	if cfgs[0].topic != "homeassistant/sensor/muehle-hf-radio/freq_hz/config" {
		t.Errorf("topic = %q", cfgs[0].topic)
	}
	if !cfgs[0].retained {
		t.Errorf("discovery must be retained")
	}
	if len(cfgs[0].payload) == 0 || cfgs[0].payload[0] != '{' {
		t.Errorf("payload not JSON: %q", cfgs[0].payload)
	}
	if len(eng.KnownAddrs()) != 1 || eng.KnownAddrs()[0] != "muehle/hf/radio" {
		t.Errorf("known = %v", eng.KnownAddrs())
	}
}

// TestOnMetaIdempotent asserts a byte-identical re-delivery of a retained /meta does not
// re-publish (the engine's idempotency guard).
func TestOnMetaIdempotent(t *testing.T) {
	eng := NewEngine("homeassistant", "")
	pub := &fakePub{}
	eng.SetPub(pub)

	payload, topic := metaFor("radio", []expose.Field{{Key: "freq_hz", Type: "number"}})
	eng.OnMeta(topic, payload)
	first := len(pub.calls)
	eng.OnMeta(topic, payload) // identical retained re-delivery
	if len(pub.calls) != first {
		t.Errorf("re-delivery published: before=%d after=%d (want no new publishes)", first, len(pub.calls))
	}
}

// TestOnMetaChangedRepublishes asserts a changed meta (different field set) republishes
// the new config and clears the dropped entity's topic.
func TestOnMetaChangedRepublishes(t *testing.T) {
	eng := NewEngine("homeassistant", "")
	pub := &fakePub{}
	eng.SetPub(pub)

	// Two distinct metas for the same slot addr so the field set changes between them.
	first := []byte(`{"schema":"1.0","role":"radio","capabilities":{},"expose":{"device":{"name":"D"},"fields":[{"key":"freq_hz","name":"Frequency","type":"number","unit":"Hz"},{"key":"tx","name":"TX","type":"boolean"}]}}`)
	second := []byte(`{"schema":"1.0","role":"radio","capabilities":{},"expose":{"device":{"name":"D"},"fields":[{"key":"freq_hz","name":"Frequency","type":"number","unit":"Hz"}]}}`)
	topic := "muehle/hf/radio/meta"

	eng.OnMeta(topic, first)
	// Now drop the tx field: freq_hz republished, tx topic cleared.
	eng.OnMeta(topic, second)

	cfgs := pub.callsTo("/config")
	// Expect: first round 2 publishes, second round 1 publish + 1 clear(empty retained).
	// Collect non-empty config publishes and empty clears.
	var nonEmpty, empties []pubCall
	for _, c := range cfgs {
		if len(c.payload) == 0 {
			empties = append(empties, c)
		} else {
			nonEmpty = append(nonEmpty, c)
		}
	}
	if len(nonEmpty) != 3 { // 2 first round + 1 second round
		t.Errorf("non-empty config publishes = %d, want 3", len(nonEmpty))
	}
	if len(empties) != 1 {
		t.Fatalf("clear (empty) publishes = %d, want 1 (the dropped tx entity)", len(empties))
	}
	if empties[0].topic != "homeassistant/binary_sensor/muehle-hf-radio/tx/config" {
		t.Errorf("cleared topic = %q, want the dropped tx entity", empties[0].topic)
	}
}

// TestOnHAStatusRepublish asserts an "online" HA birth re-publishes all known discovery.
func TestOnHAStatusRepublish(t *testing.T) {
	eng := NewEngine("homeassistant", "")
	pub := &fakePub{}
	eng.SetPub(pub)

	payload, topic := metaFor("radio", []expose.Field{{Key: "freq_hz", Type: "number"}})
	eng.OnMeta(topic, payload)
	pub.calls = nil // clear the initial publish record

	eng.OnHAStatus("online")
	cfgs := pub.callsTo("/config")
	if len(cfgs) != 1 {
		t.Errorf("republish on HA online = %d config publishes, want 1", len(cfgs))
	}

	// Non-"online" payloads must do nothing.
	pub.calls = nil
	eng.OnHAStatus("offline")
	if len(pub.calls) != 0 {
		t.Errorf("HA status offline should be a no-op, got %d publishes", len(pub.calls))
	}
}

// TestOnMetaCleared asserts a zero-length meta payload clears that slot's discovery topics
// and drops the slot from known.
func TestOnMetaCleared(t *testing.T) {
	eng := NewEngine("homeassistant", "")
	pub := &fakePub{}
	eng.SetPub(pub)

	payload, topic := metaFor("radio", []expose.Field{{Key: "freq_hz", Type: "number"}})
	eng.OnMeta(topic, payload)
	pub.calls = nil

	// Zero-length payload => clear.
	eng.OnMeta(topic, []byte(""))

	cfgs := pub.callsTo("/config")
	if len(cfgs) != 1 {
		t.Fatalf("clear publishes = %d, want 1", len(cfgs))
	}
	if len(cfgs[0].payload) != 0 {
		t.Errorf("clear payload must be empty, got %q", cfgs[0].payload)
	}
	if !cfgs[0].retained {
		t.Errorf("clear must be retained so HA removes the entity")
	}
	if len(eng.KnownAddrs()) != 0 {
		t.Errorf("known = %v, want empty after clear", eng.KnownAddrs())
	}
}

// TestOnMetaClearedUnknownSlot asserts clearing a slot we never saw is a silent no-op.
func TestOnMetaClearedUnknownSlot(t *testing.T) {
	eng := NewEngine("homeassistant", "")
	pub := &fakePub{}
	eng.SetPub(pub)
	eng.OnMeta("muehle/hf/pa/meta", []byte{})
	if len(pub.calls) != 0 {
		t.Errorf("clear of unknown slot published: %+v", pub.calls)
	}
}

// TestOnMetaNoExposeDiagnostic asserts a slot with no expose block gets exactly one
// diagnostic binary_sensor.
func TestOnMetaNoExposeDiagnostic(t *testing.T) {
	eng := NewEngine("homeassistant", "")
	pub := &fakePub{}
	eng.SetPub(pub)

	payload, topic := metaFor("pa", nil) // no expose
	eng.OnMeta(topic, payload)

	cfgs := pub.callsTo("/config")
	if len(cfgs) != 1 {
		t.Fatalf("config publishes = %d, want 1 diagnostic", len(cfgs))
	}
	if !strings.HasPrefix(cfgs[0].topic, "homeassistant/binary_sensor/muehle-hf-pa/") {
		t.Errorf("diagnostic topic = %q, want a binary_sensor under muehle-hf-pa", cfgs[0].topic)
	}
	if !strings.Contains(cfgs[0].topic, "/online/config") {
		t.Errorf("diagnostic object id should be 'online': %q", cfgs[0].topic)
	}
}

// TestOnMetaDefaultArea asserts the engine's deployment-wide area reaches the published
// discovery payload as `suggested_area` for a slot that does not name its own. A no-expose
// slot (diagnostic) has no device.area, so the default fills in.
func TestOnMetaDefaultArea(t *testing.T) {
	eng := NewEngine("homeassistant", "Bauwagen")
	pub := &fakePub{}
	eng.SetPub(pub)

	payload, topic := metaFor("pa", nil) // no expose -> diagnostic, no device.area
	eng.OnMeta(topic, payload)

	cfgs := pub.callsTo("/config")
	if len(cfgs) != 1 {
		t.Fatalf("config publishes = %d, want 1", len(cfgs))
	}
	var p struct {
		Device struct {
			SuggestedArea string `json:"suggested_area"`
		} `json:"device"`
	}
	if err := json.Unmarshal(cfgs[0].payload, &p); err != nil {
		t.Fatalf("unmarshal diagnostic: %v", err)
	}
	if p.Device.SuggestedArea != "Bauwagen" {
		t.Errorf("device.suggested_area = %q, want \"Bauwagen\"", p.Device.SuggestedArea)
	}
}

// TestOnMetaNoExposeIdempotent asserts a byte-identical re-delivery of a no-expose /meta
// is a no-op: the diagnostic is published once on first sight and never re-published. A
// slot that republishes its /meta on a heartbeat must not churn the bus (Diagnostic is
// deterministic, so the idempotency guard short-circuits) nor spam the log.
func TestOnMetaNoExposeIdempotent(t *testing.T) {
	eng := NewEngine("homeassistant", "")
	pub := &fakePub{}
	eng.SetPub(pub)

	payload, topic := metaFor("pa", nil) // no expose
	eng.OnMeta(topic, payload)
	first := len(pub.calls)
	eng.OnMeta(topic, payload) // identical retained re-delivery
	if len(pub.calls) != first {
		t.Errorf("no-expose re-delivery published: before=%d after=%d (want no new publishes)", first, len(pub.calls))
	}
}

// TestOnMetaParseErrorSkipped asserts a malformed meta is logged+skipped, not stored, and
// clears nothing.
func TestOnMetaParseErrorSkipped(t *testing.T) {
	eng := NewEngine("homeassistant", "")
	pub := &fakePub{}
	eng.SetPub(pub)
	eng.OnMeta("muehle/hf/radio/meta", []byte("{not json"))
	if len(pub.calls) != 0 {
		t.Errorf("malformed meta should publish nothing, got %+v", pub.calls)
	}
	if len(eng.KnownAddrs()) != 0 {
		t.Errorf("known = %v, want empty", eng.KnownAddrs())
	}
}

// TestNoopPubUntilSetPub asserts the engine does not panic before SetPub is called and
// silently drops publishes (used in the brief window before the bus client is wired).
func TestNoopPubUntilSetPub(t *testing.T) {
	eng := NewEngine("homeassistant", "")
	// No SetPub — should not panic.
	payload, topic := metaFor("radio", []expose.Field{{Key: "freq_hz", Type: "number"}})
	eng.OnMeta(topic, payload)
	eng.OnHAStatus("online")
	eng.OnMeta(topic, []byte{})
}

// TestSetPubNilRevertsToNoop asserts SetPub(nil) is safe and reverts to the noop publisher.
func TestSetPubNilRevertsToNoop(t *testing.T) {
	eng := NewEngine("homeassistant", "")
	pub := &fakePub{}
	eng.SetPub(pub)
	eng.SetPub(nil)
	payload, topic := metaFor("radio", []expose.Field{{Key: "freq_hz", Type: "number"}})
	eng.OnMeta(topic, payload)
	if len(pub.calls) != 0 {
		t.Errorf("after SetPub(nil) the noop publisher should drop, got %+v", pub.calls)
	}
}

// TestEntitiesEqual guards the idempotency comparator.
func TestEntitiesEqual(t *testing.T) {
	a := []ha.Entity{{Topic: "t1", Payload: []byte("p1")}, {Topic: "t2", Payload: []byte("p2")}}
	if !entitiesEqual(a, a) {
		t.Error("identical slices should be equal")
	}
	if entitiesEqual(a, a[:1]) {
		t.Error("different lengths should be unequal")
	}
	b := []ha.Entity{{Topic: "t1", Payload: []byte("p1")}, {Topic: "t2", Payload: []byte("DIFF")}}
	if entitiesEqual(a, b) {
		t.Error("different payloads should be unequal")
	}
	if entitiesEqual(a, []ha.Entity{{Topic: "t1", Payload: []byte("p1")}, {Topic: "OTHER", Payload: []byte("p2")}}) {
		t.Error("different topics should be unequal")
	}
	// byte-level: identical bytes via copy must compare equal.
	c := []ha.Entity{{Topic: "t1", Payload: bytes.Repeat([]byte("x"), 10)}}
	d := []ha.Entity{{Topic: "t1", Payload: bytes.Repeat([]byte("x"), 10)}}
	if !entitiesEqual(c, d) {
		t.Error("equal-content payloads should be equal")
	}
}
