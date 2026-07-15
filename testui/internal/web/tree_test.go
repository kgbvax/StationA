package web

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"testui/internal/mqtt"
)

func TestSplitTopic(t *testing.T) {
	cases := []struct {
		topic   string
		address string
		plane   plane
	}{
		{"muehle/hf/radio/state", "muehle/hf/radio", planeState},
		{"muehle/hf/radio/meta", "muehle/hf/radio", planeMeta},
		{"muehle/hf/radio/status", "muehle/hf/radio", planeStatus},
		{"muehle/hf/radio/cmd", "muehle/hf/radio", planeCmd},
		// Sub-topic (multi-receiver) still resolves by deepest plane suffix.
		{"muehle/hf/radio/receiver/1/state", "muehle/hf/radio/receiver/1", planeState},
		// Station node published directly: no plane suffix.
		{"muehle/hf", "muehle/hf", ""},
		// Unknown suffix: whole topic is the address.
		{"muehle/hf/radio/meter/smeter", "muehle/hf/radio/meter/smeter", ""},
	}
	for _, c := range cases {
		addr, pl := splitTopic(c.topic)
		if addr != c.address || pl != c.plane {
			t.Errorf("splitTopic(%q) = (%q,%q); want (%q,%q)", c.topic, addr, pl, c.address, c.plane)
		}
	}
}

func TestUpdateStoresAndClears(t *testing.T) {
	tr := NewTree()
	tr.Update(mqtt.Message{Topic: "muehle/hf/radio/state", Payload: []byte(`{"freq_hz":14025000}`), Retained: true})
	tr.Update(mqtt.Message{Topic: "muehle/hf/radio/status", Payload: []byte("online"), Retained: true})

	slots, order := tr.Snapshot()
	if len(order) != 1 || order[0] != "muehle/hf/radio" {
		t.Fatalf("order=%v", order)
	}
	s := slots[0]
	if s.State == nil || string(s.State.Payload) != `{"freq_hz":14025000}` {
		t.Errorf("state not stored: %+v", s.State)
	}
	if s.Status == nil || string(s.Status.Payload) != `"online"` {
		t.Errorf("status not stored: %+v", s.Status)
	}

	// Empty retained payload clears the plane (broker semantics: a retained clear).
	tr.Update(mqtt.Message{Topic: "muehle/hf/radio/state", Payload: []byte{}, Retained: true})
	slots, _ = tr.Snapshot()
	if slots[0].State != nil {
		t.Errorf("state should be cleared, got %+v", slots[0].State)
	}
}

func TestSnapshotOrderIsInsertionOrder(t *testing.T) {
	tr := NewTree()
	tr.Update(mqtt.Message{Topic: "muehle/hf/pa/state", Payload: []byte(`{}`)})
	tr.Update(mqtt.Message{Topic: "muehle/hf/radio/state", Payload: []byte(`{}`)})
	tr.Update(mqtt.Message{Topic: "muehle/uhf/rotator/state", Payload: []byte(`{}`)})
	_, order := tr.Snapshot()
	want := []string{"muehle/hf/pa", "muehle/hf/radio", "muehle/uhf/rotator"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order=%v want=%v", order, want)
	}
}

// TestBroadcastEventShape is the regression test for the critical SSE bug: live updates
// must be a tagged Event (lowercase keys, json.RawMessage payload, decoded object), NOT
// a raw mqtt.Message (no JSON tags, []byte -> base64). The browser reads m.topic /
// m.payload (lowercase) and the object view directly.
func TestBroadcastEventShape(t *testing.T) {
	tr := NewTree()
	ch, cancel := tr.Subscribe()
	defer cancel()

	tr.Update(mqtt.Message{Topic: "muehle/hf/radio/state", Payload: []byte(`{"freq_hz":14025000}`), Retained: true})
	ev := <-ch
	if ev.Address != "muehle/hf/radio" || ev.Plane != "state" {
		t.Fatalf("addr=%q plane=%q", ev.Address, ev.Plane)
	}
	if ev.Topic != "muehle/hf/radio/state" {
		t.Errorf("topic=%q", ev.Topic)
	}
	if string(ev.Payload) != `{"freq_hz":14025000}` {
		t.Errorf("payload not raw JSON object (base64?): %q", ev.Payload)
	}
	if ev.Object == nil || ev.Object["freq_hz"] != float64(14025000) {
		t.Errorf("object not decoded: %v", ev.Object)
	}
	// Wire shape must use lowercase keys, not mqtt.Message's capitalized ones.
	data, _ := json.Marshal(ev)
	if !strings.Contains(string(data), `"topic":"muehle/hf/radio/state"`) {
		t.Errorf("event JSON not lowercase-tagged: %s", data)
	}
	if strings.Contains(string(data), `"Topic":`) || strings.Contains(string(data), `"Payload":`) {
		t.Errorf("event JSON looks like raw mqtt.Message: %s", data)
	}

	// A bare /status string is wrapped as a JSON string and carries no object view.
	tr.Update(mqtt.Message{Topic: "muehle/hf/radio/status", Payload: []byte("online"), Retained: true})
	ev2 := <-ch
	if string(ev2.Payload) != `"online"` {
		t.Errorf("status payload not JSON-quoted string: %q", ev2.Payload)
	}
	if ev2.Object != nil {
		t.Errorf("status should have no object view: %v", ev2.Object)
	}

	// An empty retained payload is a clear: the Event signals it.
	tr.Update(mqtt.Message{Topic: "muehle/hf/radio/state", Payload: []byte{}, Retained: true})
	ev3 := <-ch
	if !ev3.Cleared {
		t.Errorf("expected cleared event, got %+v", ev3)
	}

	// A JSON-falsy but non-empty payload (null/false/0/"") is a real stored value, NOT a
	// clear. The server is the sole authority for clearing; the client must not infer a
	// clear from a falsy payload. Guards the F1 client/server-divergence regression.
	for _, raw := range []string{`null`, `false`, `0`, `""`} {
		tr.Update(mqtt.Message{Topic: "muehle/hf/radio/state", Payload: []byte(raw), Retained: true})
		ev := <-ch
		if ev.Cleared {
			t.Errorf("payload %q must not set Cleared (server is sole clear authority)", raw)
		}
		if string(ev.Payload) != raw {
			t.Errorf("payload %q not passed through: %q", raw, ev.Payload)
		}
	}

	// A bare scalar /status (out-of-spec but reachable) passes through unwrapped and
	// must not crash; the UI renders it via String(). Locks the json.Valid behavior.
	tr.Update(mqtt.Message{Topic: "muehle/hf/radio/status", Payload: []byte("5"), Retained: true})
	ev4 := <-ch
	if ev4.Cleared || string(ev4.Payload) != "5" {
		t.Errorf("bare scalar /status mishandled: cleared=%v payload=%q", ev4.Cleared, ev4.Payload)
	}
}