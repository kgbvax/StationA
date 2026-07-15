package bridge

import (
	"encoding/json"
	"strings"
	"testing"

	"flexbridge/internal/flexradio"
	"flexbridge/internal/ha"
)

// testLogger records nothing but satisfies Logger.
type testLogger struct{ t *testing.T }

func (l *testLogger) Infof(f string, a ...any)  { l.t.Logf("INFO: "+f, a...) }
func (l *testLogger) Warnf(f string, a ...any)  { l.t.Logf("WARN: "+f, a...) }
func (l *testLogger) Debugf(f string, a ...any) {}

const testStateTopic = "test/hf/radio/state"

func newTestBridge(t *testing.T) (*Bridge, *MemoPublisher) {
	t.Helper()
	pub := NewMemoPublisher()
	cfg := Config{
		Site:               "test",
		Station:            "hf",
		Slot:               "radio",
		DiscoveryPrefix:    "homeassistant",
		AvailTopic:         "test/hf/radio/status",
		PublishHADiscovery: true, // legacy embedded discovery is gated off by default (model §9); enable here to keep testing that path.
	}
	b := New(cfg, pub, &testLogger{t})
	b.SetDevice(ha.Device{Serial: "TESTSERIAL", Model: "FLEX-8400", Name: "FlexRadio 8400"})
	return b, pub
}

// lastState finds the last published state snapshot at the given topic.
func lastState(msgs []MemoMsg, topic string) (statePayload, bool) {
	var snap statePayload
	found := false
	for _, m := range msgs {
		if m.Topic == topic {
			if err := json.Unmarshal(m.Payload, &snap); err == nil {
				found = true
			}
		}
	}
	return snap, found
}

// seedSlice sends a slice status so state has a non-zero base before testing
// other status types.
func seedSlice(t *testing.T, b *Bridge, freqMHz string) {
	t.Helper()
	f, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=" + freqMHz + " mode=USB active=1")
	b.HandleStatus(f)
}

func TestBridge_TransmittingState(t *testing.T) {
	b, pub := newTestBridge(t)
	seedSlice(t, b, "14.025000")
	pub.Reset()

	f, _ := flexradio.ParseFrame("S0|interlock state=TRANSMITTING")
	b.HandleStatus(f)

	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published after TRANSMITTING")
	}
	if snap.TX != "tx" {
		t.Errorf("TX = %q, want \"tx\"", snap.TX)
	}
	if !pub.Messages()[0].Retained {
		t.Error("state should be retained")
	}

	// Back to RX.
	pub.Reset()
	f2, _ := flexradio.ParseFrame("S0|interlock state=RECEIVING")
	b.HandleStatus(f2)
	snap, ok = lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published after RECEIVING")
	}
	if snap.TX != "rx" {
		t.Errorf("TX = %q, want \"rx\"", snap.TX)
	}
}

func TestBridge_SliceFreqAndMode(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	f, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=14.025000 mode=USB active=1")
	b.HandleStatus(f)

	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published for new slice")
	}
	if snap.FreqHz != 14_025_000 {
		t.Errorf("FreqHz = %d, want 14025000", snap.FreqHz)
	}
	if snap.Band != "20m" {
		t.Errorf("Band = %q, want 20m", snap.Band)
	}
	if snap.Mode != "usb" {
		t.Errorf("Mode = %q, want usb (canonical)", snap.Mode)
	}
}

func TestBridge_ModeNormalization(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	cases := []struct {
		firmware  string
		canonical string
	}{
		{"CW-U", "cw"},
		{"LSB", "lsb"},
		{"DIGU", "data"},
		{"FM", "fm"},
	}
	for _, c := range cases {
		pub.Reset()
		f, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=14.025000 mode=" + c.firmware + " active=1")
		b.HandleStatus(f)
		snap, ok := lastState(pub.Messages(), testStateTopic)
		if !ok {
			t.Fatalf("no state after mode=%s", c.firmware)
		}
		if snap.Mode != c.canonical {
			t.Errorf("mode %s -> %q, want %q", c.firmware, snap.Mode, c.canonical)
		}
	}
}

func TestBridge_ActiveSliceFollowsActiveFlag(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// Slice 0 becomes active on 20m.
	f0, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=14.100000 mode=USB active=1")
	b.HandleStatus(f0)
	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok || snap.Band != "20m" {
		t.Fatalf("band = %q, want 20m", snap.Band)
	}

	// Slice 0 tunes to 40m.
	pub.Reset()
	f1, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=7.100000 mode=LSB active=1")
	b.HandleStatus(f1)
	snap, ok = lastState(pub.Messages(), testStateTopic)
	if !ok || snap.Band != "40m" {
		t.Fatalf("band after retune = %q, want 40m", snap.Band)
	}

	// Slice 1 appears inactive — state unchanged.
	pub.Reset()
	f2, _ := flexradio.ParseFrame("S0|slice 1 0 RF_frequency=3.600000 mode=LSB active=0")
	b.HandleStatus(f2)
	if _, ok := lastState(pub.Messages(), testStateTopic); ok {
		t.Error("state should not publish when inactive slice changes")
	}

	// Slice 1 becomes the TX slice — state follows it.
	pub.Reset()
	f3, _ := flexradio.ParseFrame("S0|slice 1 0 RF_frequency=3.600000 mode=LSB tx=1")
	b.HandleStatus(f3)
	snap, ok = lastState(pub.Messages(), testStateTopic)
	if !ok || snap.Band != "80m" {
		t.Fatalf("band after TX slice switch = %q, want 80m", snap.Band)
	}
}

func TestBridge_NoDuplicatePublish(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// Start on 20m.
	f1, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=14.100000 mode=USB active=1")
	b.HandleStatus(f1)

	// Same frequency, same mode — no new publish.
	pub.Reset()
	f2, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=14.100000 mode=USB active=1")
	b.HandleStatus(f2)
	if len(pub.Messages()) != 0 {
		t.Errorf("unchanged state republished %d times, want 0", len(pub.Messages()))
	}

	// Frequency changes within 20m — publishes.
	pub.Reset()
	f3, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=14.200000 mode=USB active=1")
	b.HandleStatus(f3)
	if _, ok := lastState(pub.Messages(), testStateTopic); !ok {
		t.Error("expected state publish after frequency change")
	}
}

func TestBridge_TuningStateFromATU(t *testing.T) {
	b, pub := newTestBridge(t)
	seedSlice(t, b, "14.025000")
	pub.Reset()

	// ATU starts tuning.
	f1, _ := flexradio.ParseFrame("S0|atu status=Tuning active=1")
	b.HandleStatus(f1)
	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published on tuning start")
	}
	if !snap.Tuning {
		t.Error("tuning should be true during ATU Tuning state")
	}

	// ATU finishes.
	pub.Reset()
	f2, _ := flexradio.ParseFrame("S0|atu status=Tuned active=0")
	b.HandleStatus(f2)
	snap, ok = lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published on tuning end")
	}
	if snap.Tuning {
		t.Error("tuning should be false after ATU Tuned state")
	}
}

func TestBridge_DriveStatus(t *testing.T) {
	b, pub := newTestBridge(t)
	seedSlice(t, b, "14.025000")
	pub.Reset()

	f, _ := flexradio.ParseFrame("S0|radio drive=75 status=Available")
	b.HandleStatus(f)
	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published after drive update")
	}
	if snap.Drive != 75 {
		t.Errorf("Drive = %d, want 75", snap.Drive)
	}
}

// TestBridge_DeviceOnlineOnSetDevice: a successful handshake (SetDevice) must
// publish a /state snapshot with device_online=true so the bus sees the radio
// link come up — /status is MQTT/LWT bridge liveness, not the radio link.
func TestBridge_DeviceOnlineOnSetDevice(t *testing.T) {
	pub := NewMemoPublisher()
	cfg := Config{Site: "test", Station: "hf", Slot: "radio", DiscoveryPrefix: "homeassistant", AvailTopic: "test/hf/radio/status"}
	b := New(cfg, pub, &testLogger{t})

	// No SetDevice yet — no state published.
	if _, ok := lastState(pub.Messages(), testStateTopic); ok {
		t.Fatal("state published before SetDevice")
	}

	b.SetDevice(ha.Device{Serial: "1126-1213-8400-7992", Model: "FLEX-8400", Name: "FlexRadio 8400"})
	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published on SetDevice (handshake)")
	}
	if !snap.DeviceOnline {
		t.Errorf("device_online = %v, want true after handshake", snap.DeviceOnline)
	}
}

// TestBridge_DeviceOfflineOnReset: on connection lost (Reset) the bridge must
// republish /state with device_online=false and zeroed radio values, so a
// consumer watching only /state sees the radio go offline. Without this,
// /state freezes on the last live values (on-change-only publishing).
func TestBridge_DeviceOfflineOnReset(t *testing.T) {
	b, pub := newTestBridge(t)
	seedSlice(t, b, "14.025000")

	// Live state: device_online=true, real freq.
	snap, _ := lastState(pub.Messages(), testStateTopic)
	if !snap.DeviceOnline || snap.FreqHz != 14_025_000 {
		t.Fatalf("pre-reset state = %+v, want device_online=true freq=14025000", snap)
	}

	pub.Reset()
	b.Reset()

	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published on Reset (disconnect)")
	}
	if snap.DeviceOnline {
		t.Errorf("device_online = true after Reset, want false (radio link down)")
	}
	if snap.FreqHz != 0 {
		t.Errorf("freq_hz = %d after Reset, want 0 (stale values cleared)", snap.FreqHz)
	}
}

// TestBridge_MetaExposesDeviceOnline: the consumer-neutral expose block must
// advertise device_online so hadiscovery/testui render a radio-link pill.
func TestBridge_MetaExposesDeviceOnline(t *testing.T) {
	_, pub := newTestBridge(t)
	var metaMsg *MemoMsg
	for i := range pub.Messages() {
		m := pub.Messages()[i]
		if m.Topic == "test/hf/radio/meta" {
			metaMsg = &m
		}
	}
	var meta map[string]any
	_ = json.Unmarshal(metaMsg.Payload, &meta)
	expose, _ := meta["expose"].(map[string]any)
	fields, _ := expose["fields"].([]any)
	for _, f := range fields {
		fm, _ := f.(map[string]any)
		if fm["key"] == "device_online" {
			if fm["type"] != "boolean" {
				t.Errorf("device_online type = %v, want boolean", fm["type"])
			}
			return
		}
	}
	t.Error("expose.fields missing device_online")
}

func TestBridge_StateTopicIsRetained(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	f, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=14.025000 mode=USB active=1")
	b.HandleStatus(f)

	for _, m := range pub.Messages() {
		if m.Topic == testStateTopic && !m.Retained {
			t.Error("state topic must be retained")
		}
	}
}

func TestBridge_Discovery_PublishedOnce(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// Re-calling PublishDiscovery must be a no-op (already done in setup).
	b.PublishDiscovery()
	configCount := 0
	for _, m := range pub.Messages() {
		if strings.HasSuffix(m.Topic, "/config") {
			configCount++
		}
	}
	if configCount != 0 {
		t.Errorf("discovery re-published %d times on no-op; want 0", configCount)
	}
}

func TestBridge_Discovery_ContainsEntities(t *testing.T) {
	_, pub := newTestBridge(t)
	joined := ""
	for _, m := range pub.Messages() {
		if strings.HasSuffix(m.Topic, "/config") {
			joined += m.Topic + " " + string(m.Payload) + "\n"
		}
	}
	for _, want := range []string{"frequency", "band", "mode", "drive", "transmitting", "tuning"} {
		if !strings.Contains(joined, want) {
			t.Errorf("discovery missing entity %q", want)
		}
	}
}

func TestBridge_Discovery_UsesValueTemplate(t *testing.T) {
	_, pub := newTestBridge(t)
	for _, m := range pub.Messages() {
		if !strings.HasSuffix(m.Topic, "/config") {
			continue
		}
		if !strings.Contains(string(m.Payload), "value_template") {
			t.Errorf("discovery entity %q missing value_template", m.Topic)
		}
	}
}

func TestBridge_MetaTopic(t *testing.T) {
	_, pub := newTestBridge(t)
	var metaMsg *MemoMsg
	for i := range pub.Messages() {
		m := pub.Messages()[i]
		if m.Topic == "test/hf/radio/meta" {
			metaMsg = &m
		}
	}
	if metaMsg == nil {
		t.Fatal("meta topic not published")
	}
	if !metaMsg.Retained {
		t.Error("meta should be retained")
	}
	var meta map[string]any
	if err := json.Unmarshal(metaMsg.Payload, &meta); err != nil {
		t.Fatalf("meta not valid JSON: %v", err)
	}
	if meta["role"] != "radio" {
		t.Errorf("meta role = %v, want \"radio\"", meta["role"])
	}
	if meta["schema"] != "1.0" {
		t.Errorf("meta schema = %v, want \"1.0\"", meta["schema"])
	}
	// The consumer-neutral expose block (model §3.1) must be present and describe
	// the read-only radio field surface. flexbridge is read-only: no writable
	// fields, no actions.
	expose, ok := meta["expose"].(map[string]any)
	if !ok {
		t.Fatalf("meta missing expose block; got %v", meta["expose"])
	}
	fields, _ := expose["fields"].([]any)
	if len(fields) != 7 {
		t.Errorf("expose.fields len = %d, want 7 (device_online,freq_hz,band,mode,drive,tx,tuning)", len(fields))
	}
	// No field should declare itself writable (read-only bridge).
	for i, f := range fields {
		fm, _ := f.(map[string]any)
		if fm["key"] == nil {
			t.Errorf("expose.fields[%d] missing key", i)
		}
		if _, w := fm["writable"]; w {
			t.Errorf("expose field %v must not be writable (read-only bridge)", fm["key"])
		}
	}
	if _, hasActions := expose["actions"]; hasActions {
		t.Error("read-only bridge must expose no actions")
	}
	// enum options resolve via options_ref into capabilities, not inline options.
	if dev, _ := expose["device"].(map[string]any); dev == nil || dev["manufacturer"] != "FlexRadio Systems" {
		t.Errorf("expose.device = %v, want manufacturer FlexRadio Systems", dev)
	}
}

func TestBridge_StateTopicAddress(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	f, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=14.025000 mode=USB active=1")
	b.HandleStatus(f)

	for _, m := range pub.Messages() {
		if m.Topic == testStateTopic {
			return
		}
	}
	t.Errorf("expected state publish at %q, got topics: %v", testStateTopic, topicList(pub.Messages()))
}

func topicList(msgs []MemoMsg) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Topic)
	}
	return out
}
