package mqtt

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"

	"ultrabridge/internal/ub/service"
	"ultrabridge/internal/ub/transport"
)

func TestBandOptionsHaveCenters(t *testing.T) {
	for _, b := range bandOptions {
		khz, ok := bandCenterKHz[b]
		if !ok {
			t.Errorf("band option %q has no center frequency", b)
		}
		if khz == 0 {
			t.Errorf("band %q center is zero", b)
		}
	}
	if got := bandCenterKHz["6m"]; got != 51000 {
		t.Errorf("6m center = %d, want 51000", got)
	}
	if got := bandCenterKHz["20m"]; got != 14175 {
		t.Errorf("20m center = %d, want 14175", got)
	}
}

func TestStateSnapshotIgnoresUpdatedAt(t *testing.T) {
	s1 := service.State{
		FrequencyKHz: 14000,
		BandName:     "20m",
		BandIndex:    4,
		ModeName:     "forward",
		MotorsMoving: false,
		MotorBits:    0,
		Offline:      false,
		LastError:    "",
		UpdatedAt:    time.Date(2026, 6, 7, 10, 0, 0, 0, time.UTC),
	}
	s2 := s1
	s2.UpdatedAt = s1.UpdatedAt.Add(5 * time.Second)

	snap1 := stateSnapshot(s1)
	snap2 := stateSnapshot(s2)

	if snap1 != snap2 {
		t.Fatalf("snapshots should be equal when only updated_at changes: %#v vs %#v", snap1, snap2)
	}
}

func TestStateSnapshotFreqHz(t *testing.T) {
	s := service.State{FrequencyKHz: 14175}
	snap := stateSnapshot(s)
	if snap.FreqHz != 14175000 {
		t.Errorf("FreqHz = %d, want 14175000 (kHz * 1000)", snap.FreqHz)
	}
}

func TestShouldPublishStateDeduplicates(t *testing.T) {
	c := &Client{}
	base := publishedState{
		FreqHz:    14000000,
		Band:      "20m",
		Mode:      "forward",
		Moving:    false,
		Offline:   false,
		LastError: "",
	}

	if !c.shouldPublishState(base) {
		t.Fatal("first state should be published")
	}
	if c.shouldPublishState(base) {
		t.Fatal("identical state should not be published")
	}

	changed := base
	changed.Mode = "reverse"
	if !c.shouldPublishState(changed) {
		t.Fatal("changed state should be published")
	}
	if c.shouldPublishState(changed) {
		t.Fatal("same changed state should not be published again")
	}
}

func TestTopicHelpers(t *testing.T) {
	c := &Client{site: "muehle", station: "hf", slot: "ultrabeam-ctrl", discoveryPrefix: "homeassistant"}
	cases := []struct {
		got  string
		want string
	}{
		{c.stateTopic(), "muehle/hf/ultrabeam-ctrl/state"},
		{c.metaTopic(), "muehle/hf/ultrabeam-ctrl/meta"},
		{c.statusTopic(), "muehle/hf/ultrabeam-ctrl/status"},
		{c.cmdTopic(), "muehle/hf/ultrabeam-ctrl/cmd"},
		{c.discoveryTopic("sensor", "hf-ultrabeam-ctrl", "band"), "homeassistant/sensor/hf-ultrabeam-ctrl/band/config"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestMetaPayloadExpose locks the consumer-neutral expose block in /meta (integration
// model §3.1). ultrabridge is read-write: freq_hz/band/direction are writable setpoints, moving
// is a read-only boolean, retract is a one-shot action. Enum options resolve via
// options_ref into capabilities, so the keys must match.
func TestMetaPayloadExpose(t *testing.T) {
	c := &Client{site: "muehle", station: "hf", slot: "ant-ctrl", location: "bauwagen", host: "shari"}
	meta := c.metaPayload()

	if meta["role"] != "ant-ctrl" {
		t.Errorf("role = %v, want ant-ctrl", meta["role"])
	}
	caps, _ := meta["capabilities"].(map[string]any)
	expose, ok := meta["expose"].(map[string]any)
	if !ok {
		t.Fatal("meta missing expose block")
	}

	// device block
	dev, _ := expose["device"].(map[string]any)
	if dev["manufacturer"] != "Ultrabeam" || dev["model"] != "RCU-06" {
		t.Errorf("expose.device = %v, want Ultrabeam/RCU-06", dev)
	}

	// fields: every writable field must carry a command; every enum options_ref must
	// resolve to a non-empty list in capabilities.
	fields, _ := expose["fields"].([]map[string]any)
	if len(fields) != 6 {
		t.Fatalf("expose.fields len = %d, want 6", len(fields))
	}
	wantWritable := map[string]bool{
		"freq_hz": true, "band": true, "direction": true,
		"moving": false, "device_online": false, "error": false,
	}
	for _, f := range fields {
		key, _ := f["key"].(string)
		if f["name"] == nil {
			t.Errorf("field %q missing name", key)
		}
		writable, _ := f["writable"].(bool)
		if writable != wantWritable[key] {
			t.Errorf("field %q writable = %v, want %v", key, writable, wantWritable[key])
		}
		if writable {
			if f["command"] == nil {
				t.Errorf("writable field %q must have a command", key)
			}
		}
		if f["type"] == "enum" {
			ref, _ := f["options_ref"].(string)
			if ref == "" {
				t.Errorf("enum field %q must use options_ref", key)
			}
			if opts, _ := caps[ref].([]string); len(opts) == 0 {
				t.Errorf("enum field %q options_ref %q does not resolve in capabilities", key, ref)
			}
		}
	}
	// freq_hz min/max/step + int coercion must be present (matches the embedded
	// discovery command_template).
	for _, f := range fields {
		if f["key"] == "freq_hz" {
			if f["min"] == nil || f["max"] == nil || f["step"] == nil {
				t.Errorf("freq_hz missing min/max/step: %v", f)
			}
			cmd, _ := f["command"].(map[string]any)
			if cmd["value_type"] != "int" {
				t.Errorf("freq_hz command value_type = %v, want int", cmd["value_type"])
			}
		}
	}

	// actions: retract one-shot.
	actions, _ := expose["actions"].([]map[string]any)
	if len(actions) != 1 || actions[0]["key"] != "retract" {
		t.Fatalf("expose.actions = %v, want one retract", actions)
	}
	if cmd, _ := actions[0]["command"].(map[string]any); cmd["action"] != "retract" {
		t.Errorf("retract command = %v, want {action:retract}", cmd)
	}

	if meta["location"] != "bauwagen" || meta["host"] != "shari" {
		t.Errorf("meta location/host = %v/%v", meta["location"], meta["host"])
	}
}

// fakePaho embeds a nil paho.Client so only Publish is exercised; the test never calls any
// other paho method (no Connect/Subscribe/IsConnectionOpen on this path).
type fakePaho struct {
	paho.Client
	pub func(topic string, qos byte, retained bool, payload any) paho.Token
}

func (f fakePaho) Publish(topic string, qos byte, retained bool, payload any) paho.Token {
	return f.pub(topic, qos, retained, payload)
}

// blockingToken blocks Wait() until `release` is closed. It stands in for a slow broker:
// its Wait() blocks indefinitely until released, the exact condition that deadlocked paho's
// inline dispatch before the worker fix (the republish publishes synchronously; an inline
// onHAStatus would block paho's matchAndDispatch goroutine waiting for the PUBACK the
// stalled read loop can never deliver).
type blockingToken struct{ release <-chan struct{} }

func (t blockingToken) Wait() bool { <-t.release; return true }
func (t blockingToken) WaitTimeout(d time.Duration) bool {
	select {
	case <-t.release:
		return true
	case <-time.After(d):
		return false
	}
}
func (t blockingToken) Done() <-chan struct{} { return make(chan struct{}) }
func (t blockingToken) Error() error          { return nil }

// fakeMessage is a minimal paho.Message for handler tests that never touch a real client.
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

// instantToken completes immediately — stands in for a fast broker publish.
type instantToken struct{ err error }

func (t instantToken) Wait() bool                     { return true }
func (t instantToken) WaitTimeout(time.Duration) bool { return t.err == nil }
func (t instantToken) Done() <-chan struct{}          { return make(chan struct{}) }
func (t instantToken) Error() error                   { return t.err }

// recPub records one Publish call's arguments.
type recPub struct {
	topic    string
	qos      byte
	retained bool
	payload  string
}

// recordingPaho records every Publish and completes it instantly; only Publish is
// exercised on this path (no Connect/Subscribe, mirroring fakePaho above).
type recordingPaho struct {
	paho.Client
	mu   sync.Mutex
	pubs []recPub
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

func (f *recordingPaho) recorded() []recPub {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recPub(nil), f.pubs...)
}

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

// cmdTestClient builds a Client wired to the recording paho + mock controller, with a
// running jobs worker, for onCmd tests.
func cmdTestClient(t *testing.T) (*Client, *recordingPaho, *service.Controller) {
	t.Helper()
	ctrl := service.NewController(transport.NewMock())
	fake := &recordingPaho{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := &Client{
		client: fake,
		ctrl:   ctrl,
		site:   "muehle", station: "hf", slot: "ant-ctrl",
		discoveryPrefix: "homeassistant",
		jobs:            make(chan func(), 16),
		ctx:             ctx,
		cancel:          cancel,
	}
	go sharedmqtt.RunJobs(ctx, c.jobs)
	return c, fake, ctrl
}

// TestOnCmdExecutesThenClears locks the one-shot /cmd semantics (2026-09-03 stale-cmd
// replay fix): a frequency command must reach the controller AND clear the retained topic
// afterwards, so nothing re-fires on the next (re)connect.
func TestOnCmdExecutesThenClears(t *testing.T) {
	c, fake, ctrl := cmdTestClient(t)
	msg := fakeMessage{topic: "muehle/hf/ant-ctrl/cmd", payload: []byte(`{"action":"frequency","freq_hz":21225000}`)}
	c.onCmd(nil, msg)

	waitFor(t, 2*time.Second, "frequency cmd executed on controller", func() bool {
		return ctrl.State().FrequencyKHz == 21225
	})
	waitFor(t, 2*time.Second, "retained cmd cleared", func() bool {
		for _, p := range fake.recorded() {
			if p.topic == "muehle/hf/ant-ctrl/cmd" && p.retained && p.payload == "" {
				return true
			}
		}
		return false
	})
}

// TestOnCmdDropsStaleTs locks the staleness gate: a command older than cmdMaxAge (or from
// the future) must be dropped before it reaches the serial device — the guard that would
// have rejected the 18 h queued backlog replayed on reconnect on 2026-09-03. A fresh ts
// passes.
func TestOnCmdDropsStaleTs(t *testing.T) {
	cases := []struct {
		name    string
		ts      string
		wantRun bool
	}{
		{"stale past", time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339), false},
		{"far future", time.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339), false},
		{"fresh", time.Now().UTC().Format(time.RFC3339), true},
		{"bad ts", "not-a-timestamp", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, fake, ctrl := cmdTestClient(t)
			payload := fmt.Sprintf(`{"action":"frequency","freq_hz":21225000,"ts":%q}`, tc.ts)
			c.onCmd(nil, fakeMessage{topic: "muehle/hf/ant-ctrl/cmd", payload: []byte(payload)})

			waitFor(t, 2*time.Second, "retained cmd cleared", func() bool {
				for _, p := range fake.recorded() {
					if p.topic == "muehle/hf/ant-ctrl/cmd" && p.retained && p.payload == "" {
						return true
					}
				}
				return false
			})
			// Small grace so a (wrong) execution has time to surface before asserting.
			time.Sleep(100 * time.Millisecond)
			got := ctrl.State().FrequencyKHz
			if tc.wantRun && got != 21225 {
				t.Errorf("fresh cmd not executed: FrequencyKHz = %d, want 21225", got)
			}
			if !tc.wantRun && got != 0 {
				t.Errorf("stale cmd executed: FrequencyKHz = %d, want 0 (untouched)", got)
			}
		})
	}
}

// TestOnCmdEmptyPayloadIsClearMarker: the empty retained payload is the clear marker we
// publish ourselves; receiving it back (we subscribe to the topic we clear) must be a
// no-op — treating it as an invalid command would echo another clear and loop forever.
func TestOnCmdEmptyPayloadIsClearMarker(t *testing.T) {
	c, fake, _ := cmdTestClient(t)
	c.onCmd(nil, fakeMessage{topic: "muehle/hf/ant-ctrl/cmd", payload: []byte{}})
	time.Sleep(100 * time.Millisecond)
	if pubs := fake.recorded(); len(pubs) != 0 {
		t.Errorf("empty payload triggered %d publish(es), want none (clear loop): %v", len(pubs), pubs)
	}
}

// TestOnCmdInvalidPayloadCleared: an unknown-action payload must not reach the controller
// and must clear the retained topic so a typo'd retained cmd does not re-log on every reconnect.
func TestOnCmdInvalidPayloadCleared(t *testing.T) {
	c, fake, ctrl := cmdTestClient(t)
	msg := fakeMessage{topic: "muehle/hf/ant-ctrl/cmd", payload: []byte(`{"action":"frequncy"}`)}
	c.onCmd(nil, msg)

	waitFor(t, 2*time.Second, "retained cmd cleared", func() bool {
		for _, p := range fake.recorded() {
			if p.topic == "muehle/hf/ant-ctrl/cmd" && p.retained && p.payload == "" {
				return true
			}
		}
		return false
	})
	time.Sleep(50 * time.Millisecond) // let any (wrong) execution surface
	if got := ctrl.State().FrequencyKHz; got != 0 {
		t.Errorf("invalid cmd executed: FrequencyKHz = %d, want 0 (untouched)", got)
	}
}

// TestOnHAStatusDefersRepublish is the regression guard for the paho-handler deadlock.
// onHAStatus runs on paho's matchAndDispatch goroutine (OrderMatters is the default) and
// must NOT republish inline, because PublishDiscovery()/PublishState() publish synchronously
// and that blocks dispatch after the first message. Against the pre-fix code (the republish
// called directly inside the handler) this test hangs at the first select and fails on
// timeout; with the worker it returns immediately and the deferred republish later runs
// (and blocks) on the worker goroutine, not paho's.
//
// publishHADiscovery=true makes onHAStatus run PublishDiscovery() (eight synchronous
// publishes via c.client.Publish) before PublishState; the first publish blocks, which is
// what the test observes. A mock controller lets PublishState complete after release rather
// than nil-deref, so the worker exits cleanly.
func TestOnHAStatusDefersRepublish(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	var once sync.Once
	fake := fakePaho{pub: func(string, byte, bool, any) paho.Token {
		once.Do(func() { close(reached) })
		return blockingToken{release: release}
	}}

	ctrl := service.NewController(transport.NewMock())
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		client: fake,
		ctrl:   ctrl,
		site:   "muehle", station: "hf", slot: "ant-ctrl",
		discoveryPrefix:    "homeassistant",
		publishHADiscovery: true,
		jobs:               make(chan func(), 256),
		ctx:                ctx,
		cancel:             cancel,
	}
	go sharedmqtt.RunJobs(ctx, c.jobs)

	msg := fakeMessage{topic: "homeassistant/status", payload: []byte("online")}

	// Phase 1 — onHAStatus must return without waiting for the (blocked) republish.
	returned := make(chan struct{})
	go func() {
		c.onHAStatus(nil, msg)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("onHAStatus blocked: republish ran inline on the caller (paho-dispatch) goroutine")
	}

	// Phase 2 — the worker runs the deferred republish and reaches the blocking publish.
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("worker never reached the deferred republish")
	}
	close(release) // let every queued publish complete
	cancel()       // tell the worker to exit (cancels its ctx)
}
