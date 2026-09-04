package bridge

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"flexbridge/internal/flexradio"
	"flexbridge/internal/ha"
)

// testLogger records nothing but satisfies Logger.
type testLogger struct{ t *testing.T }

func (l *testLogger) Infof(f string, a ...any)  { l.t.Logf("INFO: "+f, a...) }
func (l *testLogger) Warnf(f string, a ...any)  { l.t.Logf("WARN: "+f, a...) }
func (l *testLogger) Errorf(f string, a ...any) { l.t.Logf("ERROR: "+f, a...) }
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
	// The consumer-neutral expose block (model §3.1) must be present. Radio
	// tuning state is read-only except band and mic_profile (writable setpoints);
	// DVK playback is exposed as one-shot actions.
	expose, ok := meta["expose"].(map[string]any)
	if !ok {
		t.Fatalf("meta missing expose block; got %v", meta["expose"])
	}
	fields, _ := expose["fields"].([]any)
	if len(fields) != 10 {
		t.Errorf("expose.fields len = %d, want 10 (device_online,freq_hz,band,mode,drive,tx,tuning,dvk_status,dvk_id,mic_profile)", len(fields))
	}
	// Radio tuning state is read-only except band and mic_profile, which are
	// writable setpoints (set_band → native band-stacking; set_mic_profile →
	// native profile mic load). Every other field must not be writable; DVK is
	// driven via one-shot actions, not setpoint fields.
	for i, f := range fields {
		fm, _ := f.(map[string]any)
		if fm["key"] == nil {
			t.Errorf("expose.fields[%d] missing key", i)
			continue
		}
		key, _ := fm["key"].(string)
		if _, w := fm["writable"]; w && key != "band" && key != "mic_profile" {
			t.Errorf("expose field %v must not be writable", key)
		}
	}
	// band is the writable setpoint with a set_band command descriptor.
	var bandField map[string]any
	for _, f := range fields {
		fm, _ := f.(map[string]any)
		if k, _ := fm["key"].(string); k == "band" {
			bandField = fm
		}
	}
	if bandField == nil {
		t.Fatal("expose.fields missing band field")
	}
	if _, w := bandField["writable"]; !w {
		t.Error("band field must be writable (set_band)")
	}
	cmd, _ := bandField["command"].(map[string]any)
	if cmd == nil || cmd["action"] != "set_band" || cmd["value_key"] != "value" {
		t.Errorf("band command = %v, want {action:set_band value_key:value}", cmd)
	}
	// DVK is the one read-write surface: expose.actions carries 12 play buttons +
	// stop (action-only one-shot actions).
	actions, hasActions := expose["actions"].([]any)
	if !hasActions {
		t.Fatal("expose.actions missing; DVK actions must be advertised")
	}
	if len(actions) != 13 {
		t.Errorf("expose.actions len = %d, want 13 (dvk_play_1..12 + dvk_stop)", len(actions))
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

// ------------------------------------------------------------------
// DVK (Digital Voice Keyer, SmartSDR v4+)
// ------------------------------------------------------------------

// fakeCommander records the DVK, band-change, and mic-profile calls the bridge makes.
type fakeCommander struct {
	mu             sync.Mutex
	playCall       []int
	stopCall       []int
	bandCall       []bandCall
	micProfileCall []string
}

type bandCall struct {
	Handle string
	Band   int
}

func (f *fakeCommander) DVKPlay(id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.playCall = append(f.playCall, id)
	return nil
}

func (f *fakeCommander) DVKStop(id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCall = append(f.stopCall, id)
	return nil
}

func (f *fakeCommander) SetBand(handle string, band int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bandCall = append(f.bandCall, bandCall{handle, band})
	return nil
}

func (f *fakeCommander) SetMicProfile(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.micProfileCall = append(f.micProfileCall, name)
	return nil
}

func (f *fakeCommander) plays() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.playCall...)
}

func (f *fakeCommander) stops() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.stopCall...)
}

func (f *fakeCommander) bands() []bandCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bandCall(nil), f.bandCall...)
}

func (f *fakeCommander) micProfiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.micProfileCall...)
}

// cmdJSON builds a /cmd payload the way the shared value-key convention does:
// the argument rides under "value", not under a key named after the action.
func cmdJSON(action, value string) []byte {
	if value == "" {
		return []byte(`{"action":"` + action + `"}`)
	}
	return []byte(`{"action":"` + action + `","value":"` + value + `"}`)
}

// TestHandleCommand_DVK exercises the /cmd dispatch surface: the per-memory
// action form, the value form, bad-value rejection, dvk_stop (explicit and
// active-id), unknown actions, and the nil-commander no-op.
func TestHandleCommand_DVK(t *testing.T) {
	t.Run("dvk_play_3 per-memory action", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		b.HandleCommand(cmdJSON("dvk_play_3", ""))
		if got := fc.plays(); len(got) != 1 || got[0] != 3 {
			t.Errorf("plays = %v, want [3]", got)
		}
		if got := fc.stops(); len(got) != 0 {
			t.Errorf("stops = %v, want none", got)
		}
	})

	t.Run("dvk_play value form", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		b.HandleCommand(cmdJSON("dvk_play", "3"))
		if got := fc.plays(); len(got) != 1 || got[0] != 3 {
			t.Errorf("plays = %v, want [3]", got)
		}
	})

	t.Run("dvk_play bad value rejected", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		b.HandleCommand(cmdJSON("dvk_play", "99")) // out of range 1-12
		b.HandleCommand(cmdJSON("dvk_play", "foo"))
		b.HandleCommand(cmdJSON("dvk_play", ""))
		if got := fc.plays(); len(got) != 0 {
			t.Errorf("plays = %v, want none (all bad values rejected)", got)
		}
	})

	t.Run("dvk_play_13 out of range rejected", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		b.HandleCommand(cmdJSON("dvk_play_13", ""))
		if got := fc.plays(); len(got) != 0 {
			t.Errorf("plays = %v, want none (13 is out of range)", got)
		}
	})

	t.Run("dvk_stop explicit value", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		b.HandleCommand(cmdJSON("dvk_stop", "3"))
		if got := fc.stops(); len(got) != 1 || got[0] != 3 {
			t.Errorf("stops = %v, want [3]", got)
		}
	})

	t.Run("dvk_stop no value uses active id from state", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		// Seed an active DVK memory via the status stream (dvk status=playback id=5).
		f, _ := flexradio.ParseFrame("S0|dvk status=playback id=5")
		b.HandleStatus(f)
		b.HandleCommand(cmdJSON("dvk_stop", ""))
		if got := fc.stops(); len(got) != 1 || got[0] != 5 {
			t.Errorf("stops = %v, want [5] (active id from state)", got)
		}
	})

	t.Run("dvk_stop no value and none active is a no-op", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		b.HandleCommand(cmdJSON("dvk_stop", ""))
		if got := fc.stops(); len(got) != 0 {
			t.Errorf("stops = %v, want none (no active memory)", got)
		}
	})

	t.Run("unknown action is a no-op", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		b.HandleCommand(cmdJSON("set_frequency", "14.025"))
		if got := fc.plays(); len(got) != 0 {
			t.Errorf("plays = %v, want none", got)
		}
	})

	t.Run("nil commander no-ops (radio offline)", func(t *testing.T) {
		// A fresh bridge has no commander installed until runOnce calls SetCommander.
		b, _ := newTestBridge(t)
		b.HandleCommand(cmdJSON("dvk_play_3", "")) // must not panic
		b.HandleCommand(cmdJSON("dvk_stop", ""))
	})
}

// seedPan sends a "display pan" status frame so the bridge tracks a panadapter
// handle, which set_band needs to target. SmartSDR's panadapter status topic is
// the two-word "display pan" — the frame arrives as Topic="display",
// TopicArgs="pan <stream_id> ...".
func seedPan(t *testing.T, b *Bridge, handle string, band int) {
	t.Helper()
	f, _ := flexradio.ParseFrame("S0|display pan " + handle + " band=" + strconv.Itoa(band) + " center=14.175 in_use=1")
	b.HandleStatus(f)
}

// TestHandleCommand_SetBand exercises the native band-stacking /cmd dispatch:
// a valid band resolves to a wavelength number and targets a tracked pan;
// unknown bands and a missing panadapter are rejected without panicking.
func TestHandleCommand_SetBand(t *testing.T) {
	t.Run("valid band targets the tracked pan", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		seedPan(t, b, "0x40000000", 20)

		b.HandleCommand(cmdJSON("set_band", "40m"))
		got := fc.bands()
		if len(got) != 1 || got[0].Handle != "0x40000000" || got[0].Band != 40 {
			t.Errorf("set_band calls = %v, want [{0x40000000 40}]", got)
		}
	})

	t.Run("unknown band is a no-op", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		seedPan(t, b, "0x40000000", 20)

		b.HandleCommand(cmdJSON("set_band", "2m")) // XVTR, out of scope
		b.HandleCommand(cmdJSON("set_band", "gen"))
		b.HandleCommand(cmdJSON("set_band", "99m"))
		if got := fc.bands(); len(got) != 0 {
			t.Errorf("set_band calls = %v, want none (all unsupported)", got)
		}
	})

	t.Run("no panadapter tracked is a no-op", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		// No pan status seeded → no pan tracked.
		b.HandleCommand(cmdJSON("set_band", "20m"))
		if got := fc.bands(); len(got) != 0 {
			t.Errorf("set_band calls = %v, want none (no panadapter)", got)
		}
	})

	t.Run("nil commander no-ops (radio offline)", func(t *testing.T) {
		b, _ := newTestBridge(t)
		seedPan(t, b, "0x40000000", 20)
		b.HandleCommand(cmdJSON("set_band", "20m")) // must not panic
	})
}

// TestHandleCommand_SetBand_SuppressesTransientBand verifies that after a
// set_band command, intermediate slice status frames carrying the old frequency
// do not publish a transient wrong band to the bus. The band is held at the
// previous value until the panadapter reports the target band.
func TestHandleCommand_SetBand_SuppressesTransientBand(t *testing.T) {
	b, pub := newTestBridge(t)
	fc := &fakeCommander{}
	b.SetCommander(fc)

	// Start on 10m.
	seedPan(t, b, "0x40000000", 10)
	f10, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=28.300000 mode=USB active=1")
	b.HandleStatus(f10)
	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok || snap.Band != "10m" {
		t.Fatalf("baseline band = %q, want 10m", snap.Band)
	}

	// Command a change to 15m.
	pub.Reset()
	b.HandleCommand(cmdJSON("set_band", "15m"))

	// SmartSDR sends a slice update with the old 10m frequency before the slice
	// has retuned to 15m. Without the transition hold this would derive 10m (or
	// gen) and republish, which antennaselect would interpret as a routing intent.
	fTransient, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=28.300000 mode=USB active=1")
	b.HandleStatus(fTransient)
	if _, ok := lastState(pub.Messages(), testStateTopic); ok {
		t.Fatalf("transient slice should not publish /state during band transition")
	}

	// The panadapter confirms 15m; subsequent slice updates are accepted.
	pub.Reset()
	fPan15, _ := flexradio.ParseFrame("S0|display pan 0x40000000 band=15 center=21.225 in_use=1")
	b.HandleStatus(fPan15)
	f15, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=21.225000 mode=USB active=1")
	b.HandleStatus(f15)
	snap, ok = lastState(pub.Messages(), testStateTopic)
	if !ok || snap.Band != "15m" || snap.FreqHz != 21_225_000 {
		t.Fatalf("after pan confirm: got band=%q freq_hz=%d, want 15m/21225000", snap.Band, snap.FreqHz)
	}
}

// TestHandleCommand_SetBand_HoldExpires verifies that the band-transition hold
// releases after its deadline even if the panadapter never reports the target
// band, so a stuck transition cannot suppress real band changes forever.
func TestHandleCommand_SetBand_HoldExpires(t *testing.T) {
	// Use a very short hold so the test does not sleep for long.
	origHold := bandTransitionHold
	bandTransitionHold = 5 * time.Millisecond
	defer func() { bandTransitionHold = origHold }()

	b, pub := newTestBridge(t)
	fc := &fakeCommander{}
	b.SetCommander(fc)

	seedPan(t, b, "0x40000000", 10)
	f10, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=28.300000 mode=USB active=1")
	b.HandleStatus(f10)

	b.HandleCommand(cmdJSON("set_band", "40m"))
	time.Sleep(10 * time.Millisecond) // wait for hold to expire

	pub.Reset()
	f40, _ := flexradio.ParseFrame("S0|slice 0 0 RF_frequency=7.100000 mode=LSB active=1")
	b.HandleStatus(f40)
	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok || snap.Band != "40m" {
		t.Fatalf("after hold expiry: got band=%q, want 40m", snap.Band)
	}
}

// TestHandleDVK_State asserts the dvk status stream flows to the retained
// /state snapshot: playback sets dvk_status + dvk_id; idle clears the id.
func TestHandleDVK_State(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// Playback of memory 3 starts.
	f, _ := flexradio.ParseFrame("S0|dvk status=playback id=3")
	b.HandleStatus(f)
	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no /state published after dvk status=playback")
	}
	if snap.DVKStatus != "playback" {
		t.Errorf("dvk_status = %q, want playback", snap.DVKStatus)
	}
	if snap.DVKID != 3 {
		t.Errorf("dvk_id = %d, want 3", snap.DVKID)
	}

	// Idle clears the active memory id; the status itself becomes "idle".
	pub.Reset()
	f2, _ := flexradio.ParseFrame("S0|dvk status=idle id=3")
	b.HandleStatus(f2)
	snap, ok = lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no /state published after dvk status=idle")
	}
	if snap.DVKStatus != "idle" {
		t.Errorf("dvk_status = %q, want idle", snap.DVKStatus)
	}
	if snap.DVKID != 0 {
		t.Errorf("dvk_id = %d, want 0 (cleared on idle)", snap.DVKID)
	}
}

// TestHandleDVK_NonStatusFrameNoState asserts added/deleted memory-library
// frames do not perturb the /state plane (only status= frames carry state).
func TestHandleDVK_NonStatusFrameNoState(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// A "dvk added" memory-library frame must not publish state.
	f, _ := flexradio.ParseFrame(`S0|dvk added id=1 name="CQ" duration=5000`)
	b.HandleStatus(f)
	if _, ok := lastState(pub.Messages(), testStateTopic); ok {
		t.Error("dvk added frame published /state; only status= frames should")
	}
}

// TestBridge_MetaExposesDVK asserts the expose actions are exactly the 12
// per-memory play buttons plus dvk_stop, and that the dvk_status/dvk_id
// state fields are advertised.
func TestBridge_MetaExposesDVK(t *testing.T) {
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
	var meta map[string]any
	_ = json.Unmarshal(metaMsg.Payload, &meta)
	expose, _ := meta["expose"].(map[string]any)

	// Fields: dvk_status and dvk_id must be present.
	fields, _ := expose["fields"].([]any)
	haveField := map[string]bool{}
	for _, f := range fields {
		fm, _ := f.(map[string]any)
		haveField[fm["key"].(string)] = true
	}
	for _, k := range []string{"dvk_status", "dvk_id"} {
		if !haveField[k] {
			t.Errorf("expose.fields missing %q", k)
		}
	}

	// Actions: 12 dvk_play_<N> + dvk_stop (action-only; the memory index is in
	// the action name, so none of these carry a value_key).
	actions, _ := expose["actions"].([]any)
	seen := map[string]bool{}
	for _, a := range actions {
		am, _ := a.(map[string]any)
		key, _ := am["key"].(string)
		seen[key] = true
		cmd, _ := am["command"].(map[string]any)
		if cmd == nil {
			t.Errorf("action %q missing command", key)
			continue
		}
		if cmd["action"] != key {
			t.Errorf("action %q command.action = %v, want %q", key, cmd["action"], key)
		}
		// All DVK actions are action-only — no value_key.
		if _, hasVK := cmd["value_key"]; hasVK {
			t.Errorf("action %q should be action-only (no value_key)", key)
		}
	}
	for n := 1; n <= 12; n++ {
		if !seen["dvk_play_"+strconv.Itoa(n)] {
			t.Errorf("missing action dvk_play_%d", n)
		}
	}
	if !seen["dvk_stop"] {
		t.Error("missing action dvk_stop")
	}
	if len(seen) != 13 {
		t.Errorf("action count = %d, want 13", len(seen))
	}
}

// ------------------------------------------------------------------
// Mic profiles (SmartSDR native profile mic load)
// ------------------------------------------------------------------

// seedMicProfileList sends a "profile mic list=…" status frame (the reply to
// the one-shot `profile mic info` command) so the bridge tracks a set of mic
// profiles. The frame is an authoritative full snapshot; names may contain
// spaces and are caret-delimited with a trailing caret, exactly as the radio
// emits them.
func seedMicProfileList(t *testing.T, b *Bridge, names ...string) {
	t.Helper()
	f, _ := flexradio.ParseFrame("S0|profile mic list=" + strings.Join(names, "^") + "^")
	b.HandleStatus(f)
}

// seedMicProfileCurrent sends a "profile mic current=<name>" status frame
// (defensive: SmartSDR does not emit this for mic profiles, but the bridge
// honors it should a firmware revision do so).
func seedMicProfileCurrent(t *testing.T, b *Bridge, name string) {
	t.Helper()
	f, _ := flexradio.ParseFrame("S0|profile mic current=" + name)
	b.HandleStatus(f)
}

// TestHandleProfile_State asserts the mic-profile status stream flows to the
// retained /state snapshot: a list= frame populates mic_profiles (sorted, full
// snapshot replacement); a non-mic profile type is ignored; a current= frame
// sets mic_profile; and a list snapshot that no longer contains the active
// name clears mic_profile.
func TestHandleProfile_State(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()

	// A list frame with two profiles (names contain spaces). Sorted on /state.
	seedMicProfileList(t, b, "Ragchew", "Default ProSet HC6")

	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no /state published after mic profile list frame")
	}
	if snap.MicProfile != "" {
		t.Errorf("mic_profile = %q, want empty (list frame carries no active)", snap.MicProfile)
	}
	if len(snap.MicProfiles) != 2 {
		t.Fatalf("mic_profiles = %v, want 2 entries", snap.MicProfiles)
	}
	// Sorted: "Default ProSet HC6" < "Ragchew".
	if snap.MicProfiles[0] != "Default ProSet HC6" || snap.MicProfiles[1] != "Ragchew" {
		t.Errorf("mic_profiles = %v, want [Default ProSet HC6 Ragchew] (sorted)", snap.MicProfiles)
	}

	// A non-mic profile type (global) must not perturb the mic-profile state.
	pub.Reset()
	f, _ := flexradio.ParseFrame("S0|profile global list=Global^SO2RDefault^")
	b.HandleStatus(f)
	if _, ok := lastState(pub.Messages(), testStateTopic); ok {
		t.Error("non-mic profile frame published /state; only mic profiles should")
	}

	// A current= frame sets the active mic profile (defensive path).
	pub.Reset()
	seedMicProfileCurrent(t, b, "Default ProSet HC6")
	snap, ok = lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no /state published on current= frame")
	}
	if snap.MicProfile != "Default ProSet HC6" {
		t.Errorf("mic_profile = %q, want Default ProSet HC6", snap.MicProfile)
	}

	// A later list snapshot that omits the active name clears mic_profile and
	// replaces the set (full-snapshot semantics, not incremental).
	pub.Reset()
	seedMicProfileList(t, b, "Ragchew")
	snap, ok = lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no /state published on replacement list frame")
	}
	if snap.MicProfile != "" {
		t.Errorf("mic_profile = %q, want empty (active name dropped from list)", snap.MicProfile)
	}
	if len(snap.MicProfiles) != 1 || snap.MicProfiles[0] != "Ragchew" {
		t.Errorf("mic_profiles = %v, want [Ragchew] (full-snapshot replacement)", snap.MicProfiles)
	}

	// An identical list frame (no change) must not republish /state.
	pub.Reset()
	seedMicProfileList(t, b, "Ragchew")
	if _, ok := lastState(pub.Messages(), testStateTopic); ok {
		t.Error("unchanged list frame republished /state; should be a no-op")
	}
}

// TestHandleCommand_SetMicProfile exercises the /cmd dispatch for loading a
// mic profile: a valid name fires SetMicProfile AND tracks the active name
// client-side on /state.mic_profile (SmartSDR reports no active mic profile);
// an unknown name is dropped only when the tracked list is populated (an empty
// list — before the first profile mic info response — does not block);
// invalid names and a missing commander are no-ops.
func TestHandleCommand_SetMicProfile(t *testing.T) {
	t.Run("valid name fires SetMicProfile + sets active (list empty, not blocked)", func(t *testing.T) {
		b, pub := newTestBridge(t)
		pub.Reset()
		fc := &fakeCommander{}
		b.SetCommander(fc)
		b.HandleCommand(cmdJSON("set_mic_profile", "Default ProSet HC6"))
		if got := fc.micProfiles(); len(got) != 1 || got[0] != "Default ProSet HC6" {
			t.Errorf("SetMicProfile calls = %v, want [Default ProSet HC6]", got)
		}
		snap, ok := lastState(pub.Messages(), testStateTopic)
		if !ok {
			t.Fatal("no /state published after set_mic_profile")
		}
		if snap.MicProfile != "Default ProSet HC6" {
			t.Errorf("mic_profile = %q, want Default ProSet HC6 (client-side active)", snap.MicProfile)
		}
	})

	t.Run("known name when list populated fires SetMicProfile", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		seedMicProfileList(t, b, "Default ProSet HC6", "Ragchew")
		b.HandleCommand(cmdJSON("set_mic_profile", "Default ProSet HC6"))
		if got := fc.micProfiles(); len(got) != 1 || got[0] != "Default ProSet HC6" {
			t.Errorf("SetMicProfile calls = %v, want [Default ProSet HC6]", got)
		}
	})

	t.Run("unknown name when list populated is dropped", func(t *testing.T) {
		b, pub := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		seedMicProfileList(t, b, "Default ProSet HC6", "Ragchew")
		pub.Reset() // consume the seed's own /state publish before the dropped command
		b.HandleCommand(cmdJSON("set_mic_profile", "Nope"))
		if got := fc.micProfiles(); len(got) != 0 {
			t.Errorf("SetMicProfile calls = %v, want none (unknown name)", got)
		}
		if _, ok := lastState(pub.Messages(), testStateTopic); ok {
			t.Error("unknown-name load published /state; should be a no-op")
		}
	})

	t.Run("invalid name (embedded quote) is dropped", func(t *testing.T) {
		b, _ := newTestBridge(t)
		fc := &fakeCommander{}
		b.SetCommander(fc)
		b.HandleCommand(cmdJSON("set_mic_profile", `evil"name`))
		if got := fc.micProfiles(); len(got) != 0 {
			t.Errorf("SetMicProfile calls = %v, want none (invalid name)", got)
		}
	})

	t.Run("nil commander no-ops (radio offline)", func(t *testing.T) {
		b, _ := newTestBridge(t)
		b.HandleCommand(cmdJSON("set_mic_profile", "Default ProSet HC6")) // must not panic
	})
}

// TestBridge_StateHeartbeat pins the recency contract the m5stamp pa-arm's 10 s
// radio heartbeat depends on: while the radio link is live the snapshot is
// republished on the ticker even with zero radio activity (nothing else in
// flexbridge publishes /state on a change-only basis), and it stops — without
// stopping the loop — as soon as device_online goes false.
func TestBridge_StateHeartbeat(t *testing.T) {
	b, pub := newTestBridge(t) // SetDevice marks device_online=true
	seedSlice(t, b, "14.025000")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { b.StateHeartbeat(ctx, 10*time.Millisecond); close(done) }()

	// Live link: heartbeats flow (~interval each).
	deadline := time.Now().Add(2 * time.Second)
	for countFrom(pub.Messages()) < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if n := countFrom(pub.Messages()); n < 2 {
		t.Fatalf("heartbeat while live: %d state publishes, want >=2", n)
	}

	// Link down: ticker keeps running but must not publish.
	pub.Reset()
	b.Reset()
	nAfterReset := countFrom(pub.Messages())
	deadline = time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if countFrom(pub.Messages()) > nAfterReset {
			t.Fatal("heartbeat published while device_online=false (should skip)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// countFrom counts state-topic publishes in the memo publisher.
func countFrom(msgs []MemoMsg) int {
	n := 0
	for _, m := range msgs {
		if m.Topic == testStateTopic {
			n++
		}
	}
	return n
}
