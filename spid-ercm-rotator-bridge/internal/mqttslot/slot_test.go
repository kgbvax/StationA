// SPDX-License-Identifier: AGPL-3.0-or-later

package mqttslot

// Unit tests for the two-slot MQTT surface (plan U5). The suite pins:
//
//   - the retained /meta birth certificate shape (§8.1 items 5/6: canonical
//     role, capabilities with travel limits, everything composed from the
//     passed options — no site/station/host constants),
//   - the one-shot /cmd posture (KTD13: QoS-0 subscription, clear after
//     execute-or-reject, empty-payload echo guard, ts gate),
//   - the /state cadence (KTD14: dedup, change/edge republish, fresh ts,
//     device_online while /status stays online),
//   - moving inference per axis (KTD14, via the real mount façade over fake
//     controllers),
//   - protocol-driven motion surfacing in /state (R4: a control path that
//     bypasses /cmd still shows up on the bus),
//   - clean-shutdown liveness (the powerseq self-published offline pattern).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"

	"spid-ercm-rotator-bridge/internal/config"
	"spid-ercm-rotator-bridge/internal/mount"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// recPub records one Publish call.
type recPub struct {
	topic    string
	qos      byte
	retained bool
	payload  string
}

// recSub records one Subscribe call.
type recSub struct {
	topic string
	qos   byte
}

// instantToken completes immediately — stands in for a fast broker publish.
type instantToken struct{ err error }

func (t instantToken) Wait() bool                     { return true }
func (t instantToken) WaitTimeout(time.Duration) bool { return t.err == nil }
func (t instantToken) Done() <-chan struct{}          { return make(chan struct{}) }
func (t instantToken) Error() error                   { return t.err }

// fakeMessage is a minimal paho.Message for handler tests without a broker.
type fakeMessage struct {
	topic   string
	payload []byte
}

func (m fakeMessage) Duplicate() bool   { return false }
func (m fakeMessage) Qos() byte         { return 0 }
func (m fakeMessage) Retained() bool    { return false }
func (m fakeMessage) Topic() string     { return m.topic }
func (m fakeMessage) MessageID() uint16 { return 0 }
func (m fakeMessage) Payload() []byte   { return m.payload }
func (m fakeMessage) Ack()              {}

// recordingPaho records every Publish and Subscribe and completes each token
// instantly. The unimplemented paho.Client surface embeds a nil Client, the
// ultrabridge test pattern — only the methods overridden here are ever called
// on this path.
type recordingPaho struct {
	paho.Client
	mu       sync.Mutex
	pubs     []recPub
	subs     []recSub
	open     bool
	discOnce sync.Once
}

func (f *recordingPaho) Publish(topic string, qos byte, retained bool, payload any) paho.Token {
	b, ok := payload.([]byte)
	if !ok {
		b = []byte(fmt.Sprintf("%v", payload))
	}
	f.mu.Lock()
	f.pubs = append(f.pubs, recPub{topic: topic, qos: qos, retained: retained, payload: string(b)})
	f.mu.Unlock()
	return instantToken{}
}

func (f *recordingPaho) Subscribe(topic string, qos byte, _ paho.MessageHandler) paho.Token {
	f.mu.Lock()
	f.subs = append(f.subs, recSub{topic: topic, qos: qos})
	f.mu.Unlock()
	return instantToken{}
}

func (f *recordingPaho) IsConnectionOpen() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open
}

func (f *recordingPaho) Disconnect(quiesce uint) {
	f.discOnce.Do(func() {
		f.mu.Lock()
		f.open = false
		f.mu.Unlock()
	})
}

func (f *recordingPaho) recorded() []recPub {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recPub(nil), f.pubs...)
}

func (f *recordingPaho) recordedSubs() []recSub {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recSub(nil), f.subs...)
}

// fakeMount is the dispatch-façade stand-in: it records Goto/Stop intents and
// serves canned Readback/Target/Online/Moving answers.
type fakeMount struct {
	mu     sync.Mutex
	gotos  []mount.Target
	stops  int
	pos    float64
	posOK  bool
	tgt    float64
	hasTgt bool
	online bool
	moving bool
}

func (f *fakeMount) Goto(t mount.Target) []mount.Refusal {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotos = append(f.gotos, t)
	f.tgt = t.AZ // single-axis slots: the fake is wired for one axis at a time
	if !t.HasAZ {
		f.tgt = t.EL
	}
	f.hasTgt = true
	return nil
}

func (f *fakeMount) Stop() []mount.AxisError {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	f.hasTgt = false
	return nil
}

func (f *fakeMount) Readback(ax mount.Axis) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pos, f.posOK
}

func (f *fakeMount) Online(mount.Axis) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online
}

func (f *fakeMount) Target(mount.Axis) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tgt, f.hasTgt
}

func (f *fakeMount) Moving(mount.Axis) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.moving
}

// fakeCtrl is a mount.Controller fake: it instant-parks at every admitted
// target, exactly like the SPID/ERC-M in-process mock devices.
type fakeCtrl struct {
	mu     sync.Mutex
	online bool
	pos    float64
	valid  bool
}

func (f *fakeCtrl) SetTarget(deg float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.online {
		return errors.New("axis offline")
	}
	f.pos = deg
	f.valid = true
	return nil
}

func (f *fakeCtrl) Stop() error { return nil }

func (f *fakeCtrl) Readback() (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pos, f.valid
}

func (f *fakeCtrl) Online() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online
}

func (f *fakeCtrl) setPos(deg float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pos = deg
	f.valid = true
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// waitFor polls fn until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, what)
}

// testOptions returns one slot's Options with every field carrying a test
// value — the assertions that /meta echoes these exact values (never "shari",
// "muehle", "bauwagen" constants) are the §8.1 item 6 pin.
func testOptions(axis, slot string) Options {
	return Options{
		Broker:       "tcp://127.0.0.1:1",
		ClientID:     "test-bridge",
		User:         "hf",
		Site:         "tsite",
		Station:      "tstation",
		Slot:         slot,
		Axis:         axis,
		Location:     "tlocation",
		Host:         "thost",
		DeviceModel:  "SPID test model",
		DeviceLink:   "serial",
		Limits:       config.AxisControl{Min: 0, Max: 360, Deadband: 1, Park: 0},
		PollInterval: 20 * time.Millisecond,
	}
}

// newTestSlot wires a Slot against the recording paho fake and the jobs
// worker, without a broker connection (onConnect is invoked by hand).
func newTestSlot(t *testing.T, o Options, m Mount, fake *recordingPaho) *Slot {
	t.Helper()
	o.Mount = m
	s, err := newSlot(o, nil)
	if err != nil {
		t.Fatalf("newSlot: %v", err)
	}
	s.client = fake
	s.ctx, s.cancel = context.WithCancel(context.Background())
	go sharedmqtt.RunJobs(s.ctx, s.jobs)
	t.Cleanup(func() {
		s.cancel()
	})
	return s
}

// statePubs returns every recorded /state publish for the slot's own topic,
// unmarshalled as generic maps (wire-shape assertions, not struct echoes).
func statePubs(t *testing.T, fake *recordingPaho, topic string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, p := range fake.recorded() {
		if p.topic != topic {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(p.payload), &m); err != nil {
			t.Fatalf("state payload not JSON: %v (%q)", err, p.payload)
		}
		out = append(out, m)
	}
	return out
}

func findPub(fake *recordingPaho, topic, payload string) (recPub, bool) {
	for _, p := range fake.recorded() {
		if p.topic == topic && p.payload == payload {
			return p, true
		}
	}
	return recPub{}, false
}

func cmdCleared(fake *recordingPaho, topic string) bool {
	p, ok := findPub(fake, topic, "")
	return ok && p.retained
}

// ---------------------------------------------------------------------------
// /meta birth certificate
// ---------------------------------------------------------------------------

// TestMetaPayloadComposesFromConfig pins the §8.1 items 5/6 shape: canonical
// role, axes + travel limits in capabilities, device/link/location/host all
// composed from the passed options — no site, station, host or location
// constant anywhere in the payload.
func TestMetaPayloadComposesFromConfig(t *testing.T) {
	o := testOptions(config.AxisAZ, "az-rotator")
	o.Mount = &fakeMount{}
	s, err := newSlot(o, nil)
	if err != nil {
		t.Fatalf("newSlot: %v", err)
	}
	meta := s.metaPayload()

	if meta["schema"] != "1.0" {
		t.Errorf("schema = %v, want 1.0", meta["schema"])
	}
	if meta["role"] != "rotator" {
		t.Errorf("role = %v, want rotator", meta["role"])
	}
	if meta["link"] != "serial" {
		t.Errorf("link = %v, want serial", meta["link"])
	}
	if meta["location"] != "tlocation" || meta["host"] != "thost" {
		t.Errorf("location/host = %v/%v, want tlocation/thost (from options, not constants)", meta["location"], meta["host"])
	}
	dev, _ := meta["device"].(map[string]any)
	if dev["model"] != "SPID test model" {
		t.Errorf("device.model = %v, want the configured model", dev["model"])
	}
	if _, has := dev["firmware"]; has {
		t.Errorf("device.firmware must be omitted when the driver reports none (SPID has none)")
	}

	caps, _ := meta["capabilities"].(map[string]any)
	axes, _ := caps["axes"].([]string)
	if len(axes) != 1 || axes[0] != "az" {
		t.Errorf("capabilities.axes = %v, want [az]", caps["axes"])
	}
	lim, _ := caps["limits"].(map[string]float64)
	if lim["min"] != 0 || lim["max"] != 360 || lim["park"] != 0 {
		t.Errorf("capabilities.limits = %v, want the configured travel envelope {0 360 0}", caps["limits"])
	}

	// Read-only expose (KTD4): state fields only, no writable setpoints, no
	// command widgets, no one-shot actions — hadiscovery renders state, not
	// HA motion controls.
	expose, ok := meta["expose"].(map[string]any)
	if !ok {
		t.Fatal("meta missing expose block")
	}
	fields, _ := expose["fields"].([]map[string]any)
	if len(fields) == 0 {
		t.Fatal("expose.fields empty")
	}
	gotKeys := map[string]bool{}
	for _, f := range fields {
		key, _ := f["key"].(string)
		gotKeys[key] = true
		if w, _ := f["writable"].(bool); w {
			t.Errorf("field %q writable — rotator expose is read-only (KTD4)", key)
		}
		if f["command"] != nil {
			t.Errorf("field %q carries a command — read-only expose (KTD4)", key)
		}
	}
	for _, key := range []string{"az", "target", "moving", "device_online", "error"} {
		if !gotKeys[key] {
			t.Errorf("expose.fields missing %q: %v", key, gotKeys)
		}
	}
	if _, has := expose["actions"]; has {
		t.Error("expose.actions present — a read-only expose declares no actions (KTD4)")
	}
}

// TestMetaPayloadFirmwareFromDriver pins that the ERC-M rFMW string, once the
// driver has read it, rides /meta.device.firmware — and stays absent until
// then (the el slot's constructor wires a Firmware callback, the az slot none).
func TestMetaPayloadFirmwareFromDriver(t *testing.T) {
	o := testOptions(config.AxisEL, "el-rotator")
	o.Firmware = func() string { return "" }
	o.Mount = &fakeMount{}
	s, _ := newSlot(o, nil)
	meta := s.metaPayload()
	dev, _ := meta["device"].(map[string]any)
	if _, has := dev["firmware"]; has {
		t.Error("firmware must be absent while the driver has not read it yet")
	}

	s.opts.Firmware = func() string { return "ERC-M 2.34" }
	meta = s.metaPayload()
	dev, _ = meta["device"].(map[string]any)
	if dev["firmware"] != "ERC-M 2.34" {
		t.Errorf("firmware = %v, want the driver-reported string", dev["firmware"])
	}
}

// ---------------------------------------------------------------------------
// connect / reconnect
// ---------------------------------------------------------------------------

// TestOnConnectPublishesBirthAndSubscribesCmdQoS0 pins the connect ritual:
// retained /status "online" + retained /meta birth + the /cmd subscription at
// QoS 0 (KTD13 — a persistent-session QoS-1 subscription would let the broker
// queue a motion backlog for offline replay, the 2026-09-03 incident).
func TestOnConnectPublishesBirthAndSubscribesCmdQoS0(t *testing.T) {
	o := testOptions(config.AxisAZ, "az-rotator")
	fake := &recordingPaho{}
	s := newTestSlot(t, o, &fakeMount{}, fake)

	s.onConnect(fake)

	if p, ok := findPub(fake, s.statusTopic, "online"); !ok || !p.retained || p.qos != 1 {
		t.Errorf("status online publish = %+v, want retained QoS1 'online'", p)
	}
	var metaPub recPub
	found := false
	for _, p := range fake.recorded() {
		if p.topic == s.metaTopic && p.retained {
			metaPub, found = p, true
		}
	}
	if !found {
		t.Fatal("no retained /meta publish on connect")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(metaPub.payload), &m); err != nil {
		t.Fatalf("meta payload not JSON: %v", err)
	}
	if m["role"] != "rotator" {
		t.Errorf("published meta role = %v, want rotator", m["role"])
	}

	// The QoS-0 subscription pin (KTD13).
	sawCmdSub := false
	for _, sub := range fake.recordedSubs() {
		if sub.topic == s.cmdTopic {
			sawCmdSub = true
			if sub.qos != 0 {
				t.Errorf("cmd subscribed at QoS %d, want 0 (KTD13 queued-replay defense)", sub.qos)
			}
		}
	}
	if !sawCmdSub {
		t.Errorf("no /cmd subscription: %v", fake.recordedSubs())
	}
}

// TestReconnectRepublishesMetaAndState pins the reconnect ritual (§8.1 item 1
// spirit, powerseq pattern): every (re)connect re-publishes the retained
// birth certificate and restores /state even when nothing changed — a broker
// wipe that dropped retained messages must not leave the slot stateless.
func TestReconnectRepublishesMetaAndState(t *testing.T) {
	o := testOptions(config.AxisAZ, "az-rotator")
	fm := &fakeMount{online: true, pos: 42, posOK: true}
	fake := &recordingPaho{}
	s := newTestSlot(t, o, fm, fake)

	s.onConnect(fake)
	waitFor(t, 2*time.Second, "state republished after first connect", func() bool {
		return len(statePubs(t, fake, s.stateTopic)) > 0
	})
	firstMeta, firstState := len(fake.recorded()), len(statePubs(t, fake, s.stateTopic))

	s.onConnect(fake) // simulate a paho auto-reconnect
	waitFor(t, 2*time.Second, "meta + state republished after reconnect", func() bool {
		metas := 0
		for _, p := range fake.recorded() {
			if p.topic == s.metaTopic && p.retained {
				metas++
			}
		}
		return metas >= 2 && len(statePubs(t, fake, s.stateTopic)) > firstState
	})
	if len(fake.recorded()) == firstMeta {
		t.Error("reconnect republished nothing")
	}
}

// TestNewInitialConnectFailureReturnsError pins the fatal-exit convention
// (§8.1 item 10): an unreachable broker at boot must fail the constructor so
// main exits non-zero and systemd crash-loops the unit — never a silently
// MQTT-less bridge.
func TestNewInitialConnectFailureReturnsError(t *testing.T) {
	o := testOptions(config.AxisAZ, "az-rotator")
	o.Broker = "tcp://127.0.0.1:1" // nothing listens here; refused instantly
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := New(ctx, o, nil); err == nil {
		t.Fatal("New connected (or hung) against a closed port; want a returned error for the non-zero exit")
	}
}

// ---------------------------------------------------------------------------
// /cmd one-shot posture (KTD13)
// ---------------------------------------------------------------------------

// TestGotoPerAxisRoutesCorrectAxis pins the R3 payload shape ("goto routes to
// dispatch with the value key"): the degrees ride under the `value` key
// (shared/schema convention — never a key named after the action), parse into
// the axis's mount.Target, and the retained /cmd topic is cleared after
// execution. Per axis: an az-slot goto dispatches Target{HasAZ}, an el-slot
// goto dispatches Target{HasEL}.
func TestGotoPerAxisRoutesCorrectAxis(t *testing.T) {
	cases := []struct {
		axis, slot string
		want       mount.Target
	}{
		{config.AxisAZ, "az-rotator", mount.Target{AZ: 123.5, HasAZ: true}},
		{config.AxisEL, "el-rotator", mount.Target{EL: 45, HasEL: true}},
	}
	for _, tc := range cases {
		t.Run(tc.axis, func(t *testing.T) {
			o := testOptions(tc.axis, tc.slot)
			fm := &fakeMount{online: true}
			fake := &recordingPaho{}
			s := newTestSlot(t, o, fm, fake)

			payload := `{"action":"goto","value":"` + fmt.Sprint(tc.want.AZ+tc.want.EL) + `"}`
			s.onCmd(nil, fakeMessage{topic: s.cmdTopic, payload: []byte(payload)})

			waitFor(t, 2*time.Second, "goto dispatched to the mount", func() bool {
				fm.mu.Lock()
				defer fm.mu.Unlock()
				return len(fm.gotos) == 1
			})
			fm.mu.Lock()
			got := fm.gotos[0]
			fm.mu.Unlock()
			if got != tc.want {
				t.Errorf("dispatched target = %+v, want %+v", got, tc.want)
			}
			waitFor(t, 2*time.Second, "retained cmd cleared after execution", func() bool {
				return cmdCleared(fake, s.cmdTopic)
			})
		})
	}
}

// TestGotoInvalidValueRejectedAndStillCleared pins the rejection contract: a
// goto with a non-degrees value is refused before dispatch, the refusal
// surfaces as /state.error, AND the retained cmd is still cleared (one-shot:
// execute-or-reject, always clear).
func TestGotoInvalidValueRejectedAndStillCleared(t *testing.T) {
	for _, payload := range []string{
		`{"action":"goto","value":"abc"}`, // not a number
		`{"action":"goto"}`,               // missing value
		`{"action":"goto","value":""}`,    // empty value
		`{"action":"goto","value":"NaN"}`, // ParseFloat would take it; must be refused
		`{"action":"spin","value":"90"}`,  // unknown action
	} {
		t.Run(payload, func(t *testing.T) {
			o := testOptions(config.AxisAZ, "az-rotator")
			fm := &fakeMount{online: true}
			fake := &recordingPaho{}
			s := newTestSlot(t, o, fm, fake)

			s.onCmd(nil, fakeMessage{topic: s.cmdTopic, payload: []byte(payload)})

			waitFor(t, 2*time.Second, "retained cmd cleared despite rejection", func() bool {
				return cmdCleared(fake, s.cmdTopic)
			})
			waitFor(t, 2*time.Second, "rejection surfaced in /state.error", func() bool {
				pubs := statePubs(t, fake, s.stateTopic)
				if len(pubs) == 0 {
					return false
				}
				errStr, _ := pubs[len(pubs)-1]["error"].(string)
				return errStr != ""
			})
			fm.mu.Lock()
			gotos := len(fm.gotos)
			fm.mu.Unlock()
			if gotos != 0 {
				t.Errorf("rejected cmd still dispatched %d intent(s)", gotos)
			}
		})
	}
}

// TestTsGate pins the staleness bound (KTD13): a stamped cmd older than
// cmdMaxAge — or from the far future, or with an unparseable ts — is rejected
// before dispatch; an unstamped payload is tolerated (existing console
// publishers stamp none); a fresh stamp passes.
func TestTsGate(t *testing.T) {
	cases := []struct {
		name    string
		ts      string
		wantRun bool
	}{
		{"stale past", time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339), false},
		{"far future", time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339), false},
		{"bad ts", "not-a-timestamp", false},
		{"fresh", time.Now().UTC().Format(time.RFC3339), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := testOptions(config.AxisAZ, "az-rotator")
			fm := &fakeMount{online: true}
			fake := &recordingPaho{}
			s := newTestSlot(t, o, fm, fake)

			s.onCmd(nil, fakeMessage{topic: s.cmdTopic,
				payload: []byte(fmt.Sprintf(`{"action":"goto","value":"90","ts":%q}`, tc.ts))})

			waitFor(t, 2*time.Second, "cmd cleared (stale or not)", func() bool {
				return cmdCleared(fake, s.cmdTopic)
			})
			time.Sleep(100 * time.Millisecond) // let a (wrong) dispatch surface
			fm.mu.Lock()
			gotos := len(fm.gotos)
			fm.mu.Unlock()
			if tc.wantRun && gotos != 1 {
				t.Errorf("fresh stamped cmd not dispatched (gotos = %d)", gotos)
			}
			if !tc.wantRun && gotos != 0 {
				t.Errorf("stale/bad-ts cmd dispatched (gotos = %d)", gotos)
			}
		})
	}

	// Unstamped payloads are tolerated by design.
	t.Run("unstamped tolerated", func(t *testing.T) {
		o := testOptions(config.AxisAZ, "az-rotator")
		fm := &fakeMount{online: true}
		fake := &recordingPaho{}
		s := newTestSlot(t, o, fm, fake)
		s.onCmd(nil, fakeMessage{topic: s.cmdTopic, payload: []byte(`{"action":"goto","value":"90"}`)})
		waitFor(t, 2*time.Second, "unstamped goto dispatched", func() bool {
			fm.mu.Lock()
			defer fm.mu.Unlock()
			return len(fm.gotos) == 1
		})
	})
}

// TestEmptyPayloadIsClearMarker pins the echo guard: the empty retained
// payload is our own clear marker received back on the topic we clear —
// treating it as a command would republish another clear and loop forever.
func TestEmptyPayloadIsClearMarker(t *testing.T) {
	o := testOptions(config.AxisAZ, "az-rotator")
	fm := &fakeMount{online: true}
	fake := &recordingPaho{}
	s := newTestSlot(t, o, fm, fake)

	s.onCmd(nil, fakeMessage{topic: s.cmdTopic, payload: []byte{}})

	time.Sleep(150 * time.Millisecond) // let a (wrong) clear-echo surface
	if pubs := fake.recorded(); len(pubs) != 0 {
		t.Errorf("empty payload caused %d publish(es), want none (clear loop): %+v", len(pubs), pubs)
	}
}

// TestGotoLimitRefusalSurfacesErrorAndStillCleared pins R10's bus-side
// surfacing against the REAL façade: a target outside the configured travel
// envelope is refused before any serial write, the refusal lands in
// /state.error, and the retained cmd is still cleared.
func TestGotoLimitRefusalSurfacesErrorAndStillCleared(t *testing.T) {
	azc := &fakeCtrl{online: true}
	ctl := config.ControlConfig{AZ: config.AxisControl{Min: 0, Max: 360, Deadband: 1}}
	m := mount.New(azc, &fakeCtrl{online: true}, ctl, nil)

	o := testOptions(config.AxisAZ, "az-rotator")
	fake := &recordingPaho{}
	s := newTestSlot(t, o, m, fake)

	s.onCmd(nil, fakeMessage{topic: s.cmdTopic, payload: []byte(`{"action":"goto","value":"999"}`)})

	waitFor(t, 2*time.Second, "retained cmd cleared despite limit refusal", func() bool {
		return cmdCleared(fake, s.cmdTopic)
	})
	waitFor(t, 2*time.Second, "limit refusal surfaced in /state.error", func() bool {
		pubs := statePubs(t, fake, s.stateTopic)
		if len(pubs) == 0 {
			return false
		}
		errStr, _ := pubs[len(pubs)-1]["error"].(string)
		return errStr != ""
	})
	// Nothing was written to the axis controller (R10: refusal before any write).
	azc.mu.Lock()
	valid := azc.valid
	azc.mu.Unlock()
	if valid {
		t.Error("limit-refused target reached the controller (write must never happen)")
	}
}

// TestStopDispatchesStopAndClears pins the stop action: mount.Stop (the atomic
// all-stop of KTD8) plus the one-shot clear.
func TestStopDispatchesStopAndClears(t *testing.T) {
	o := testOptions(config.AxisAZ, "az-rotator")
	fm := &fakeMount{online: true}
	fake := &recordingPaho{}
	s := newTestSlot(t, o, fm, fake)

	s.onCmd(nil, fakeMessage{topic: s.cmdTopic, payload: []byte(`{"action":"stop"}`)})

	waitFor(t, 2*time.Second, "stop dispatched", func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return fm.stops == 1
	})
	waitFor(t, 2*time.Second, "cmd cleared after stop", func() bool {
		return cmdCleared(fake, s.cmdTopic)
	})
}

// ---------------------------------------------------------------------------
// /state cadence (KTD14)
// ---------------------------------------------------------------------------

// TestStateDedupFreshTSOnChange pins the cadence: an unchanged snapshot
// publishes nothing (no firehose during a pass), a changed snapshot publishes
// with a fresh RFC3339 ts.
func TestStateDedupFreshTSOnChange(t *testing.T) {
	o := testOptions(config.AxisAZ, "az-rotator")
	fm := &fakeMount{online: true, pos: 10, posOK: true, hasTgt: true, tgt: 10}
	fake := &recordingPaho{}
	s := newTestSlot(t, o, fm, fake)

	s.publishState(false)
	if n := len(statePubs(t, fake, s.stateTopic)); n != 1 {
		t.Fatalf("first publish: %d state pubs, want 1", n)
	}
	s.publishState(false)
	if n := len(statePubs(t, fake, s.stateTopic)); n != 1 {
		t.Fatalf("unchanged snapshot republished: %d state pubs, want 1 (dedup)", n)
	}

	fm.mu.Lock()
	fm.pos = 55.5
	fm.mu.Unlock()
	s.publishState(false)
	pubs := statePubs(t, fake, s.stateTopic)
	if len(pubs) != 2 {
		t.Fatalf("changed snapshot not republished: %d state pubs, want 2", len(pubs))
	}
	if pubs[1]["az"] != 55.5 {
		t.Errorf("published az = %v, want 55.5", pubs[1]["az"])
	}
	ts1, _ := pubs[0]["ts"].(string)
	ts2, _ := pubs[1]["ts"].(string)
	if ts1 == "" || ts2 == "" {
		t.Fatalf("state ts missing: %q %q", ts1, ts2)
	}
	if _, err := time.Parse(time.RFC3339, ts2); err != nil {
		t.Errorf("state ts %q is not RFC3339: %v", ts2, err)
	}
}

// TestDeviceOnlineFalseLeavesStatusOnline pins the two-layer liveness rule
// (wrc pattern): a dead device link flips /state.device_online (and sets
// error), while /status — the bridge LWT — stays online.
func TestDeviceOnlineFalseLeavesStatusOnline(t *testing.T) {
	o := testOptions(config.AxisAZ, "az-rotator")
	fm := &fakeMount{online: false, pos: 10, posOK: true}
	fake := &recordingPaho{}
	s := newTestSlot(t, o, fm, fake)

	s.onConnect(fake)
	waitFor(t, 2*time.Second, "state published with device_online false", func() bool {
		pubs := statePubs(t, fake, s.stateTopic)
		if len(pubs) == 0 {
			return false
		}
		return pubs[len(pubs)-1]["device_online"] == false
	})

	// /status must still be "online" — and nothing offline was published.
	if p, ok := findPub(fake, s.statusTopic, "online"); !ok || !p.retained {
		t.Error("status 'online' missing")
	}
	if p, ok := findPub(fake, s.statusTopic, "offline"); ok {
		t.Errorf("device link down published 'offline' on /status: %+v — device_online is a /state field", p)
	}
}

// ---------------------------------------------------------------------------
// moving inference (KTD14) via the real mount façade
// ---------------------------------------------------------------------------

// TestMovingInferencePerAxis pins KTD14 against the real mount façade over
// fake controllers: moving = |target − readback| > deadband, false while
// readback validity is unknown, false after a stop, per axis.
func TestMovingInferencePerAxis(t *testing.T) {
	azc := &fakeCtrl{online: true}
	elc := &fakeCtrl{online: true}
	ctl := config.ControlConfig{
		AZ: config.AxisControl{Min: 0, Max: 360, Deadband: 1, Park: 0},
		EL: config.AxisControl{Min: 0, Max: 90, Deadband: 1, Park: 0},
	}
	m := mount.New(azc, elc, ctl, nil)

	// No intent yet: not moving.
	if m.Moving(mount.AZ) {
		t.Error("moving with no target")
	}
	// Intent admitted but readback validity unknown: not moving (KTD7 rule).
	if refs := m.Goto(mount.Target{AZ: 120, HasAZ: true}); len(refs) != 0 {
		t.Fatalf("az goto refused: %v", refs)
	}
	if m.Moving(mount.AZ) {
		t.Error("moving while readback validity unknown")
	}
	// First readback arrives, target far away: moving.
	azc.setPos(100)
	if !m.Moving(mount.AZ) {
		t.Error("not moving with |120-100| = 20 > deadband 1")
	}
	// El axis mirrors the whole chain independently.
	if refs := m.Goto(mount.Target{EL: 45, HasEL: true}); len(refs) != 0 {
		t.Fatalf("el goto refused: %v", refs)
	}
	elc.setPos(30)
	if !m.Moving(mount.EL) {
		t.Error("el not moving with |45-30| = 15 > deadband 1")
	}
	if !m.Moving(mount.AZ) {
		t.Error("az lost moving state while el was exercised")
	}
	// Within deadband at exactly the boundary: not moving (<= counts as within).
	azc.setPos(119)
	if m.Moving(mount.AZ) {
		t.Error("moving at exactly the deadband boundary (R12: counts as within)")
	}
	// Stop clears target (KTD14: moving cleared on stop).
	if errs := m.Stop(); len(errs) != 0 {
		t.Fatalf("stop faulted: %v", errs)
	}
	if m.Moving(mount.AZ) || m.Moving(mount.EL) {
		t.Error("moving after stop")
	}
	if _, ok := m.Target(mount.EL); ok {
		t.Error("target survived stop")
	}
}

// TestMovingSurfacedInState pins that the moving flag rides /state (via the
// slot publisher reading the façade).
func TestMovingSurfacedInState(t *testing.T) {
	azc := &fakeCtrl{online: true}
	ctl := config.ControlConfig{AZ: config.AxisControl{Min: 0, Max: 360, Deadband: 1}}
	m := mount.New(azc, &fakeCtrl{online: true}, ctl, nil)

	o := testOptions(config.AxisAZ, "az-rotator")
	fake := &recordingPaho{}
	s := newTestSlot(t, o, m, fake)

	if refs := m.Goto(mount.Target{AZ: 120, HasAZ: true}); len(refs) != 0 {
		t.Fatalf("goto refused: %v", refs)
	}
	azc.setPos(100)
	s.publishState(false)
	pubs := statePubs(t, fake, s.stateTopic)
	if len(pubs) == 0 {
		t.Fatal("no state publish")
	}
	if pubs[0]["moving"] != true {
		t.Errorf("state moving = %v, want true", pubs[0]["moving"])
	}
}

// ---------------------------------------------------------------------------
// R4: protocol-driven motion surfaces in /state
// ---------------------------------------------------------------------------

// TestProtocolDrivenMotionSurfacesInState pins R4: a control path that never
// touches /cmd (rotctld in U6, PstRotator in U7) drives the mount façade
// directly — and the poll tick's /state snapshot carries the motion to the
// bus exactly like a bus-driven move.
func TestProtocolDrivenMotionSurfacesInState(t *testing.T) {
	azc := &fakeCtrl{online: true}
	azc.setPos(10)
	ctl := config.ControlConfig{AZ: config.AxisControl{Min: 0, Max: 360, Deadband: 1}}
	m := mount.New(azc, &fakeCtrl{online: true}, ctl, nil)

	o := testOptions(config.AxisAZ, "az-rotator")
	fake := &recordingPaho{}
	s := newTestSlot(t, o, m, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx) // per-axis dispatch workers: the write happens there
	go s.Run()    // the /state poll tick

	// The "protocol server" path: direct façade call, no /cmd publish at all.
	if refs := m.Goto(mount.Target{AZ: 90, HasAZ: true}); len(refs) != 0 {
		t.Fatalf("protocol-path goto refused: %v", refs)
	}

	waitFor(t, 3*time.Second, "protocol-driven move surfaced in /state", func() bool {
		for _, p := range statePubs(t, fake, s.stateTopic) {
			if az, ok := p["az"].(float64); ok && az == 90 {
				if tgt, ok := p["target"].(float64); ok && tgt == 90 {
					return true
				}
			}
		}
		return false
	})

	// No /cmd publish was involved (motion was protocol-driven).
	for _, p := range fake.recorded() {
		if p.topic == s.cmdTopic {
			t.Errorf("unexpected /cmd publish during protocol-driven motion: %+v", p)
		}
	}
}

// ---------------------------------------------------------------------------
// clean shutdown (powerseq pattern)
// ---------------------------------------------------------------------------

// TestCloseSelfPublishesRetainedOfflineForBothSlots pins the clean-shutdown
// liveness rule: the LWT does not fire on a clean exit, so the slot must
// publish its own retained "offline" before disconnecting — for BOTH slots.
func TestCloseSelfPublishesRetainedOfflineForBothSlots(t *testing.T) {
	for _, tc := range []struct {
		axis, slot string
	}{
		{config.AxisAZ, "az-rotator"},
		{config.AxisEL, "el-rotator"},
	} {
		t.Run(tc.axis, func(t *testing.T) {
			o := testOptions(tc.axis, tc.slot)
			fake := &recordingPaho{open: true}
			s := newTestSlot(t, o, &fakeMount{}, fake)

			s.Close()

			p, ok := findPub(fake, s.statusTopic, "offline")
			if !ok {
				t.Fatalf("no self-published offline on %s", s.statusTopic)
			}
			if !p.retained || p.qos != 1 {
				t.Errorf("offline publish = QoS %d retained %v, want QoS1 retained", p.qos, p.retained)
			}
			if fake.IsConnectionOpen() {
				t.Error("client not disconnected")
			}
		})
	}
}
