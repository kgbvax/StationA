package bridge

import (
	"encoding/json"
	"testing"
)

// MemoMsg is one captured publish.
type MemoMsg struct {
	Topic    string
	Retained bool
	Payload  []byte
}

// MemoPublisher records every publish; implements Publisher for tests.
type MemoPublisher struct {
	Msgs []MemoMsg
}

func (m *MemoPublisher) Publish(topic string, retained bool, payload []byte) error {
	m.Msgs = append(m.Msgs, MemoMsg{Topic: topic, Retained: retained, Payload: payload})
	return nil
}
func (m *MemoPublisher) IsConnected() bool { return true }

// fakeCommander records /cmd dispatches without touching a Shelly.
type fakeCommander struct {
	calls []bool
	err   error
}

func (f *fakeCommander) SetPower(on bool) error {
	f.calls = append(f.calls, on)
	return f.err
}

type testLogger struct{ t *testing.T }

func (l *testLogger) Infof(f string, a ...any) { l.t.Logf("INFO: "+f, a...) }
func (l *testLogger) Warnf(f string, a ...any) { l.t.Logf("WARN: "+f, a...) }
func (l *testLogger) Debugf(string, ...any)    {}

func newTestBridge(t *testing.T, cmd Commander) (*SlotBridge, *MemoPublisher) {
	t.Helper()
	pub := &MemoPublisher{}
	cfg := Config{
		Site:            "muehle",
		Station:         "power",
		Slot:            "master",
		Location:        "bauwagen",
		Host:            "shari",
		DiscoveryPrefix: "homeassistant",
		AvailTopic:      "muehle/power/master/status",
		DeviceModel:     "Shelly Plus 1PM",
		DeviceSerial:    "shellyplus1pm-aa",
		FailSafe:        "off",
		Commander:       cmd,
	}
	b := New(cfg, pub, &testLogger{t})
	b.SetConnected(true) // tests assume the slot client is up
	return b, pub
}

func lastMsg(msgs []MemoMsg, topic string) (MemoMsg, bool) {
	var found MemoMsg
	ok := false
	for _, m := range msgs {
		if m.Topic == topic {
			found = m
			ok = true
		}
	}
	return found, ok
}

func TestPublishMeta(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{})
	b.PublishMeta()
	msg, ok := lastMsg(pub.Msgs, "muehle/power/master/meta")
	if !ok {
		t.Fatal("no /meta published")
	}
	if !msg.Retained {
		t.Error("/meta must be retained")
	}
	var m map[string]any
	if err := json.Unmarshal(msg.Payload, &m); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if m["role"] != "power" {
		t.Errorf("role = %v, want power", m["role"])
	}
	if m["link"] != "wifi" {
		t.Errorf("link = %v, want wifi", m["link"])
	}
	caps, _ := m["capabilities"].(map[string]any)
	if caps == nil || caps["fail_safe"] != "off" {
		t.Errorf("capabilities.fail_safe = %v, want off", caps)
	}
	expose, _ := m["expose"].(map[string]any)
	fields, _ := expose["fields"].([]any)
	f0, _ := fields[0].(map[string]any)
	if f0["key"] != "power" || f0["writable"] != true {
		t.Errorf("power field = %v, want writable power", f0)
	}
	cmd, _ := f0["command"].(map[string]any)
	if cmd["action"] != "set_power" || cmd["value_key"] != "value" {
		t.Errorf("power command = %v, want set_power/value", cmd)
	}
}

func TestHandleTelemetryStateSnapshot(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{})
	b.HandleTelemetry("on")
	msg, ok := lastMsg(pub.Msgs, "muehle/power/master/state")
	if !ok {
		t.Fatal("no /state published")
	}
	if !msg.Retained {
		t.Error("/state must be retained")
	}
	var snap struct {
		TS           string `json:"ts"`
		Power        string `json:"power"`
		DeviceOnline bool   `json:"device_online"`
	}
	if err := json.Unmarshal(msg.Payload, &snap); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if snap.Power != "on" || !snap.DeviceOnline {
		t.Errorf("state = %+v, want power=on device_online=true", snap)
	}
}

func TestHandleTelemetryDedup(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{})
	b.HandleTelemetry("on")
	b.HandleTelemetry("on") // unchanged → must not re-publish
	stateCount := 0
	for _, m := range pub.Msgs {
		if m.Topic == "muehle/power/master/state" {
			stateCount++
		}
	}
	if stateCount != 1 {
		t.Errorf("published /state %d times for unchanged telemetry, want 1", stateCount)
	}
}

func TestHandleCommandSetPower(t *testing.T) {
	cmd := &fakeCommander{}
	b, _ := newTestBridge(t, cmd)
	b.HandleCommand([]byte(`{"action":"set_power","value":"on"}`))
	b.HandleCommand([]byte(`{"action":"set_power","value":"off"}`))
	if len(cmd.calls) != 2 || !cmd.calls[0] || cmd.calls[1] {
		t.Errorf("SetPower calls = %v, want [true false]", cmd.calls)
	}
}

func TestHandleCommandRejectsUnknown(t *testing.T) {
	cmd := &fakeCommander{}
	b, _ := newTestBridge(t, cmd)
	b.HandleCommand([]byte(`{"action":"frobnicate","value":"x"}`))
	b.HandleCommand([]byte(`{"action":"set_power","value":"sleep"}`))
	if len(cmd.calls) != 0 {
		t.Errorf("unexpected SetPower calls = %v", cmd.calls)
	}
}

func TestDisconnectedDoesNotPublishState(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{})
	b.SetConnected(false)
	b.HandleTelemetry("on")
	for _, m := range pub.Msgs {
		if m.Topic == "muehle/power/master/state" {
			t.Error("must not publish /state while disconnected (LWT covers liveness)")
		}
	}
}
