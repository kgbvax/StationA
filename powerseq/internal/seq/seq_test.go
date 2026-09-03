package seq

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ----- test publishers -------------------------------------------------------

// recPublisher records every publish in order. Implements Publisher.
type recPublisher struct {
	mu   sync.Mutex
	msgs []recMsg
}

type recMsg struct {
	Topic    string
	Retained bool
	Payload  []byte
}

func (r *recPublisher) Publish(topic string, retained bool, payload []byte) error {
	r.mu.Lock()
	r.msgs = append(r.msgs, recMsg{Topic: topic, Retained: retained, Payload: payload})
	r.mu.Unlock()
	return nil
}

func (r *recPublisher) cmds() []cmdTuple {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []cmdTuple
	for _, m := range r.msgs {
		if !strings.HasSuffix(m.Topic, "/cmd") {
			continue
		}
		var c cmdPayload
		if json.Unmarshal(m.Payload, &c) != nil {
			continue
		}
		out = append(out, cmdTuple{topic: m.Topic, action: c.Action, value: c.Value, retained: m.Retained})
	}
	return out
}

type cmdTuple struct {
	topic, action, value string
	retained             bool
}

// stateMsgs returns the recorded /state snapshots (the sequencer's own /state).
func (r *recPublisher) states() []statePayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []statePayload
	for _, m := range r.msgs {
		if !strings.HasSuffix(m.Topic, "/state") || strings.HasSuffix(m.Topic, "/cmd") {
			continue
		}
		var st statePayload
		if json.Unmarshal(m.Payload, &st) == nil {
			out = append(out, st)
		}
	}
	return out
}

// statesRaw returns the raw payloads of recorded /state snapshots (to assert
// key presence — e.g. that `step` is always emitted, never omitempty).
func (r *recPublisher) statesRaw() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]byte
	for _, m := range r.msgs {
		if !strings.HasSuffix(m.Topic, "/state") || strings.HasSuffix(m.Topic, "/cmd") {
			continue
		}
		out = append(out, m.Payload)
	}
	return out
}

// errPublisher fails every Publish (to test the cmd-publish-failure fault path).
type errPublisher struct{ recPublisher }

func (e *errPublisher) Publish(topic string, retained bool, payload []byte) error {
	return fmt.Errorf("simulated broker down")
}

// ----- test harness ----------------------------------------------------------

type testLogger struct{ t *testing.T }

func (l *testLogger) Infof(f string, a ...any) { l.t.Logf("INFO: "+f, a...) }
func (l *testLogger) Warnf(f string, a ...any) { l.t.Logf("WARN: "+f, a...) }
func (l *testLogger) Debugf(string, ...any)    {}

const (
	testSite     = "muehle"
	testStation  = "hf"
	testSlot     = "power-seq"
	testSelfBase = "muehle/hf/power-seq"
)

// exampleStartup/exampleShutdown reproduce the model §7.1 sequence (the default
// shipped in config.example.toml), in seq.Step (site-relative) form.
func exampleStartup() []Step {
	return []Step{
		{Name: "master-on", Kind: KindCmd, Slot: "power/master", Action: "set_power", Value: "on"},
		{Name: "network-delay", Kind: KindDelay, Duration: "network"},
		{Name: "psu-on", Kind: KindCmd, Slot: "power/psu-13v8", Action: "set_power", Value: "on"},
		{Name: "wait-controllers", Kind: KindWaitStatus, Slots: []string{"hf/switch", "hf/pa-arm", "hf/ant-switch"}},
		{Name: "trx-on", Kind: KindCmd, Slot: "hf/switch", Action: "set_trx", Value: "on"},
		{Name: "wait-radio", Kind: KindWaitStatus, Slots: []string{"hf/radio"}},
		{Name: "pa-on", Kind: KindCmd, Slot: "hf/switch", Action: "set_pa", Value: "on"},
		{Name: "wait-pa-power", Kind: KindWaitState, Slot: "hf/pa", Field: "power", Value: "on"},
		{Name: "pa-arm-enable", Kind: KindCmd, Slot: "hf/pa-arm", Action: "set_enabled", Value: "true"},
	}
}

func exampleShutdown() []Step {
	return []Step{
		{Name: "pa-arm-disable", Kind: KindCmd, Slot: "hf/pa-arm", Action: "set_enabled", Value: "false"},
		{Name: "stagger-1", Kind: KindDelay, Duration: "stagger"},
		{Name: "pa-off", Kind: KindCmd, Slot: "hf/switch", Action: "set_pa", Value: "off"},
		{Name: "stagger-2", Kind: KindDelay, Duration: "stagger"},
		{Name: "trx-off", Kind: KindCmd, Slot: "hf/switch", Action: "set_trx", Value: "off"},
		{Name: "stagger-3", Kind: KindDelay, Duration: "stagger"},
		{Name: "psu-off", Kind: KindCmd, Slot: "power/psu-13v8", Action: "set_power", Value: "off"},
		{Name: "stagger-4", Kind: KindDelay, Duration: "stagger"},
		{Name: "master-off", Kind: KindCmd, Slot: "power/master", Action: "set_power", Value: "off"},
	}
}

func baseConfig(startup, shutdown []Step) Config {
	return Config{
		Site: testSite, Station: testStation, Slot: testSlot,
		Location: "bauwagen", Host: "shari",
		Startup:      startup,
		Shutdown:     shutdown,
		NetworkDelay: 5 * time.Millisecond, StepTimeout: 200 * time.Millisecond,
		ShutdownStagger: 2 * time.Millisecond, PollInterval: 2 * time.Millisecond,
	}
}

func newTestSeq(t *testing.T) (*Sequencer, *recPublisher) {
	t.Helper()
	return newTestSeqWith(t, exampleStartup(), exampleShutdown())
}

func newTestSeqWith(t *testing.T, startup, shutdown []Step) (*Sequencer, *recPublisher) {
	t.Helper()
	pub := &recPublisher{}
	s, err := New(baseConfig(startup, shutdown), pub, &testLogger{t})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetBrokerOnline(true) // simulate a connected broker (mqtt.New sets this)
	return s, pub
}

// abs resolves a site-relative slot to its absolute address.
func abs(rel string) string { return testSite + "/" + rel }

// markOnline marks the given site-relative slots' /status online.
func (s *Sequencer) markOnline(rels ...string) {
	for _, r := range rels {
		s.SetStatus(abs(r), true)
	}
}

// markOffline marks the given site-relative slots' /status offline.
func (s *Sequencer) markOffline(rels ...string) {
	for _, r := range rels {
		s.SetStatus(abs(r), false)
	}
}

// setState sets a /state field on a site-relative slot (JSON-encoded).
func (s *Sequencer) setState(rel, field, value string) {
	s.SetState(abs(rel), []byte(fmt.Sprintf(`{%q:%q}`, field, value)))
}

// allObserved pre-satisfies every wait in the example startup.
func (s *Sequencer) allObserved() {
	s.markOnline("hf/switch", "hf/pa-arm", "hf/ant-switch", "hf/radio", "hf/pa")
	s.setState("hf/pa", "power", "on")
}

func runUntil(t *testing.T, s *Sequencer, done func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("sequencer did not reach expected state in time")
}

func phaseIs(s *Sequencer, want string) bool {
	ph, _, _ := s.Phase()
	return ph == want
}

// ----- sequence tests --------------------------------------------------------

func TestStartupOrder(t *testing.T) {
	s, pub := newTestSeq(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Start()

	// master-on (cmd) + network-delay (5ms) + psu-on (cmd) emit, then the runner
	// stalls at wait-controllers (no slots online yet).
	runUntil(t, s, func() bool {
		ph, step, _ := s.Phase()
		return ph == PhaseStarting && step == "wait-controllers"
	}, time.Second)

	// Inject controllers online → proceeds to wait-radio.
	s.markOnline("hf/switch", "hf/pa-arm", "hf/ant-switch")
	runUntil(t, s, func() bool {
		_, step, _ := s.Phase()
		return step == "wait-radio"
	}, time.Second)

	// Inject radio online → proceeds to wait-pa-power.
	s.markOnline("hf/radio")
	runUntil(t, s, func() bool {
		_, step, _ := s.Phase()
		return step == "wait-pa-power"
	}, time.Second)

	// Inject pa online + power on → running.
	s.markOnline("hf/pa")
	s.setState("hf/pa", "power", "on")
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)

	want := []cmdTuple{
		{abs("power/master") + "/cmd", "set_power", "on", true},
		{abs("power/psu-13v8") + "/cmd", "set_power", "on", true},
		{abs("hf/switch") + "/cmd", "set_trx", "on", true},
		{abs("hf/switch") + "/cmd", "set_pa", "on", true},
		{abs("hf/pa-arm") + "/cmd", "set_enabled", "true", true},
	}
	cmds := pub.cmds()
	if len(cmds) != len(want) {
		t.Fatalf("got %d cmds, want %d: %+v", len(cmds), len(want), cmds)
	}
	for i, w := range want {
		if cmds[i] != w {
			t.Errorf("cmd[%d] = %+v, want %+v", i, cmds[i], w)
		}
	}
}

func TestShutdownReverseOrder(t *testing.T) {
	s, pub := newTestSeq(t)
	s.allObserved()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Start()
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)

	pub.mu.Lock()
	pub.msgs = nil // drop startup cmds; inspect shutdown only
	pub.mu.Unlock()

	s.Stop()
	runUntil(t, s, func() bool { return phaseIs(s, PhaseIdle) }, time.Second)

	want := []cmdTuple{
		{abs("hf/pa-arm") + "/cmd", "set_enabled", "false", true},
		{abs("hf/switch") + "/cmd", "set_pa", "off", true},
		{abs("hf/switch") + "/cmd", "set_trx", "off", true},
		{abs("power/psu-13v8") + "/cmd", "set_power", "off", true},
		{abs("power/master") + "/cmd", "set_power", "off", true},
	}
	cmds := pub.cmds()
	if len(cmds) != len(want) {
		t.Fatalf("got %d shutdown cmds, want %d: %+v", len(cmds), len(want), cmds)
	}
	for i, w := range want {
		if cmds[i] != w {
			t.Errorf("shutdown cmd[%d] = %+v, want %+v", i, cmds[i], w)
		}
	}
}

func TestWaitTimeoutFaultNoRollback(t *testing.T) {
	s, pub := newTestSeq(t)
	// Only master online; leave switch/pa-arm/ant-switch offline so
	// wait-controllers times out. (StepTimeout is 200ms.)
	s.markOnline("power/master")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Start()
	runUntil(t, s, func() bool {
		ph, _, fault := s.Phase()
		return ph == PhaseIdle && fault != ""
	}, time.Second)

	_, _, fault := s.Phase()
	if !strings.Contains(fault, "wait-controllers") || !strings.Contains(fault, "timeout") {
		t.Errorf("fault = %q, want wait-controllers timeout", fault)
	}
	// master + psu set_power on emitted before the fault; no trx/pa/arm cmds.
	cmds := pub.cmds()
	if len(cmds) != 2 {
		t.Fatalf("pre-fault cmds = %+v, want master+psu set_power on", cmds)
	}
	for i, w := range []cmdTuple{
		{abs("power/master") + "/cmd", "set_power", "on", true},
		{abs("power/psu-13v8") + "/cmd", "set_power", "on", true},
	} {
		if cmds[i] != w {
			t.Errorf("pre-fault cmd[%d] = %+v, want %+v", i, cmds[i], w)
		}
	}
	// No rollback: no compensating off/false cmd to master or psu.
	for _, c := range cmds {
		if c.value == "off" || c.value == "false" {
			t.Errorf("rollback cmd emitted (no rollback expected): %+v", c)
		}
	}
}

func TestStaleStateIdempotentPass(t *testing.T) {
	s, pub := newTestSeq(t)
	s.allObserved() // every wait already true → fast-path
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	start := time.Now()
	s.Start()
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)

	// Should converge fast (well under StepTimeout); the only real wait is the
	// poll cadence on already-true conditions.
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("idempotent startup took %s; expected to fast-path", elapsed)
	}
	// Every cmd re-emits the same retained intent.
	if len(pub.cmds()) != 5 {
		t.Errorf("expected 5 startup cmds, got %d", len(pub.cmds()))
	}
}

func TestRestartNoSpuriousSequence(t *testing.T) {
	s, pub := newTestSeq(t)
	// Simulate a restart with the station hot: retained observations present.
	s.allObserved()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// On boot the runner publishes idle and emits NO /cmd until a start arrives.
	runUntil(t, s, func() bool {
		pub.mu.Lock()
		defer pub.mu.Unlock()
		for _, m := range pub.msgs {
			if strings.HasSuffix(m.Topic, "/cmd") {
				return false
			}
		}
		return phaseIs(s, PhaseIdle)
	}, time.Second)
	if got := len(pub.cmds()); got != 0 {
		t.Fatalf("boot emitted %d cmds, want 0 (no spurious sequence)", got)
	}
}

func TestBusyGuards(t *testing.T) {
	s, pub := newTestSeq(t)
	s.allObserved()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// stop while idle (no fault) → dropped.
	s.Stop()
	time.Sleep(20 * time.Millisecond)
	if !phaseIs(s, PhaseIdle) || len(pub.cmds()) != 0 {
		t.Errorf("stop while idle (no fault) should be dropped; phase=%s cmds=%d", mustPhase(s), len(pub.cmds()))
	}

	// start → running; start while running → dropped (no extra cmds).
	s.Start()
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
	before := len(pub.cmds())
	s.Start() // ignored (phase=running, not idle)
	time.Sleep(20 * time.Millisecond)
	if got := len(pub.cmds()); got != before {
		t.Errorf("start while running emitted extra cmds: %+v", pub.cmds()[before:])
	}
}

func TestStopFromIdleOnFaultResumes(t *testing.T) {
	s, pub := newTestSeq(t)
	// Shutdown has no waits, so force a startup fault to land in idle+fault.
	s.markOnline("power/master")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Start()
	runUntil(t, s, func() bool {
		ph, _, fault := s.Phase()
		return ph == PhaseIdle && fault != ""
	}, time.Second)

	pub.mu.Lock()
	pub.msgs = nil
	pub.mu.Unlock()

	// stop from idle+fault IS honored (resume/teardown the partial state). The
	// phase is already idle from the fault, so wait for the shutdown's 5 cmds
	// (not phase==idle, which would return instantly).
	s.Stop()
	runUntil(t, s, func() bool { return len(pub.cmds()) == 5 }, time.Second)
	// fault cleared after a completed shutdown.
	if _, _, fault := s.Phase(); fault != "" {
		t.Errorf("fault = %q after completed shutdown, want cleared", fault)
	}
	if got := len(pub.cmds()); got != 5 {
		t.Errorf("resume shutdown emitted %d cmds, want 5", got)
	}
}

func TestSubscriptionDerivation(t *testing.T) {
	s, _ := newTestSeq(t)
	statusSlots, stateSlots := s.Subscriptions()

	wantStatus := []string{
		abs("hf/ant-switch"), abs("hf/pa"), abs("hf/pa-arm"),
		abs("hf/radio"), abs("hf/switch"), abs("power/master"), abs("power/psu-13v8"),
	}
	if fmt.Sprint(statusSlots) != fmt.Sprint(wantStatus) {
		t.Errorf("statusSlots = %v, want %v (§7.1 — every referenced slot)", statusSlots, wantStatus)
	}
	wantState := []string{abs("hf/pa")}
	if fmt.Sprint(stateSlots) != fmt.Sprint(wantState) {
		t.Errorf("stateSlots = %v, want %v", stateSlots, wantState)
	}
}

func TestMetaDerivation(t *testing.T) {
	s, _ := newTestSeq(t)
	var m map[string]any
	if err := json.Unmarshal(s.MetaPayload(), &m); err != nil {
		t.Fatalf("meta not json: %v", err)
	}
	if m["role"] != "sequencer" {
		t.Errorf("role = %v, want sequencer", m["role"])
	}
	caps, _ := m["capabilities"].(map[string]any)
	controls, _ := caps["controls"].([]any)
	watches, _ := caps["watches"].([]any)

	wantControls := []any{abs("hf/pa-arm"), abs("hf/switch"), abs("power/master"), abs("power/psu-13v8")}
	if fmt.Sprint(controls) != fmt.Sprint(wantControls) {
		t.Errorf("controls = %v, want %v (cmd slots only)", controls, wantControls)
	}
	wantWatches := []any{
		abs("hf/ant-switch"), abs("hf/pa"), abs("hf/pa-arm"),
		abs("hf/radio"), abs("hf/switch"), abs("power/master"), abs("power/psu-13v8"),
	}
	if fmt.Sprint(watches) != fmt.Sprint(wantWatches) {
		t.Errorf("watches = %v, want %v (all referenced slots, §7.1)", watches, wantWatches)
	}
}

func TestWaitStateLivenessPrecondition(t *testing.T) {
	s, _ := newTestSeq(t)
	// Satisfy the earlier startup waits so the sequence reaches wait-pa-power.
	s.markOnline("hf/switch", "hf/pa-arm", "hf/ant-switch", "hf/radio")
	// The PA is OFFLINE (dead device, LWT never published an online), but its
	// retained /state.power says "on" (stale). The wait must NOT pass on the
	// stale /state alone.
	s.setState("hf/pa", "power", "on")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Start()
	// The sequence reaches wait-pa-power (pa-on cmd emitted), then the
	// liveness precondition (hf/pa /status online) is false → timeout fault.
	runUntil(t, s, func() bool {
		_, step, _ := s.Phase()
		return step == "wait-pa-power"
	}, time.Second)
	runUntil(t, s, func() bool {
		ph, _, fault := s.Phase()
		return ph == PhaseIdle && fault != ""
	}, time.Second)

	_, _, fault := s.Phase()
	if !strings.Contains(fault, "wait-pa-power") || !strings.Contains(fault, "timeout") {
		t.Errorf("fault = %q, want wait-pa-power timeout (dead device must not pass on stale /state)", fault)
	}
}

// TestWaitStateDeviceOnlinePrecondition: the bridge is up (/status online) but the
// fronted device is dead (/state.device_online false). The wait must NOT pass on the
// stale retained /state — the LWT alone is layer 1 of the two-layer liveness rule
// (model §3/§7.1); powerseq gated on the LWT only until 2026-09-03.
func TestWaitStateDeviceOnlinePrecondition(t *testing.T) {
	s, _ := newTestSeq(t)
	s.markOnline("hf/switch", "hf/pa-arm", "hf/ant-switch", "hf/radio", "hf/pa")
	// Bridge online, device link dead, retained /state.power says "on" (stale).
	s.SetState(abs("hf/pa"), []byte(`{"power":"on","device_online":false}`))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Start()
	runUntil(t, s, func() bool {
		_, step, _ := s.Phase()
		return step == "wait-pa-power"
	}, time.Second)
	runUntil(t, s, func() bool {
		ph, _, fault := s.Phase()
		return ph == PhaseIdle && fault != ""
	}, time.Second)

	_, _, fault := s.Phase()
	if !strings.Contains(fault, "wait-pa-power") || !strings.Contains(fault, "timeout") {
		t.Errorf("fault = %q, want wait-pa-power timeout (dead device_online must not pass on stale /state)", fault)
	}
}

func StepTimeoutInTest() time.Duration { return 200 * time.Millisecond }

func TestBrokerDisconnectGatesCmd(t *testing.T) {
	s, pub := newTestSeq(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.SetBrokerOnline(false) // broker drops before the first cmd
	s.Start()
	runUntil(t, s, func() bool {
		ph, _, fault := s.Phase()
		return ph == PhaseIdle && fault != ""
	}, time.Second)

	_, _, fault := s.Phase()
	if !strings.Contains(fault, "broker disconnected") {
		t.Errorf("fault = %q, want broker disconnected", fault)
	}
	if got := len(pub.cmds()); got != 0 {
		t.Errorf("emitted %d cmds with broker down, want 0 (gated)", got)
	}
}

func TestBrokerDisconnectGatesWait(t *testing.T) {
	s, _ := newTestSeq(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Start()
	runUntil(t, s, func() bool {
		_, step, _ := s.Phase()
		return step == "wait-controllers"
	}, time.Second)

	// Broker drops mid-wait → the wait faults (does not poll stale data).
	s.SetBrokerOnline(false)
	runUntil(t, s, func() bool {
		ph, _, fault := s.Phase()
		return ph == PhaseIdle && fault != ""
	}, time.Second)

	_, _, fault := s.Phase()
	if !strings.Contains(fault, "broker disconnected") {
		t.Errorf("fault = %q, want broker disconnected", fault)
	}
}

func TestHoldMsDebounce(t *testing.T) {
	up := []Step{
		{Name: "wait-flap", Kind: KindWaitStatus, Slots: []string{"hf/switch"}, HoldMs: intPtr(60)},
	}
	s, _ := newTestSeqWith(t, up, exampleShutdown())
	s.SetBrokerOnline(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Start()
	// Online then offline within the 60ms hold window → must NOT pass.
	s.markOnline("hf/switch")
	time.Sleep(20 * time.Millisecond)
	s.markOffline("hf/switch")
	time.Sleep(60 * time.Millisecond)
	ph, step, _ := s.Phase()
	if ph == PhaseRunning {
		t.Fatalf("passed without a stable hold; step=%s", step)
	}
	// Now stay online past the hold window → passes.
	s.markOnline("hf/switch")
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
}

func TestCmdValueKeyConvention(t *testing.T) {
	s, pub := newTestSeq(t)
	s.allObserved()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Start()
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)

	// Snapshot under the publisher lock (the runner may still be publishing).
	pub.mu.Lock()
	msgs := append([]recMsg(nil), pub.msgs...)
	pub.mu.Unlock()
	for _, m := range msgs {
		if !strings.HasSuffix(m.Topic, "/cmd") {
			continue
		}
		var c map[string]any
		if err := json.Unmarshal(m.Payload, &c); err != nil {
			t.Errorf("bad cmd payload %s: %v", m.Topic, err)
			continue
		}
		if _, ok := c["action"].(string); !ok {
			t.Errorf("cmd %s: action missing/not string", m.Topic)
		}
		v, ok := c["value"]
		if !ok {
			t.Errorf("cmd %s: value key missing (value-key convention)", m.Topic)
			continue
		}
		if _, isStr := v.(string); !isStr {
			t.Errorf("cmd %s: value = %T, want JSON string (value-key convention)", m.Topic, v)
		}
	}
}

func TestRetainDefaultAndOverride(t *testing.T) {
	no := false
	up := []Step{
		{Name: "retained", Kind: KindCmd, Slot: "hf/switch", Action: "set_pa", Value: "on"},
		{Name: "oneshot", Kind: KindCmd, Slot: "hf/switch", Action: "set_trx", Value: "on", Retain: &no},
	}
	s, pub := newTestSeqWith(t, up, exampleShutdown())
	s.SetBrokerOnline(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Start()
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, 2*time.Second)

	cmds := pub.cmds()
	if len(cmds) != 2 {
		t.Fatalf("got %d cmds, want 2", len(cmds))
	}
	if !cmds[0].retained {
		t.Error("default cmd not retained (want retained=true)")
	}
	if cmds[1].retained {
		t.Error("retain=false cmd was retained (want non-retained)")
	}
}

func TestSelfWaitConfigError(t *testing.T) {
	up := []Step{{Name: "self-wait", Kind: KindWaitState, Slot: "hf/power-seq", Field: "phase", Value: "running"}}
	if _, err := New(baseConfig(up, exampleShutdown()), &recPublisher{}, &testLogger{t}); err == nil {
		t.Error("wait_state on the sequencer's own slot should fail New")
	}
}

func TestUnknownSymbolicDuration(t *testing.T) {
	up := []Step{{Name: "bad", Kind: KindDelay, Duration: "bogus"}}
	if _, err := New(baseConfig(up, exampleShutdown()), &recPublisher{}, &testLogger{t}); err == nil {
		t.Error("unknown symbolic duration should fail New")
	}
}

func TestPerStepTimeoutOverride(t *testing.T) {
	to := 1
	up := []Step{{Name: "tight-wait", Kind: KindWaitStatus, Slots: []string{"hf/switch"}, TimeoutS: &to}}
	s, _ := newTestSeqWith(t, up, exampleShutdown())
	s.SetBrokerOnline(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	start := time.Now()
	s.Start()
	runUntil(t, s, func() bool {
		ph, _, fault := s.Phase()
		return ph == PhaseIdle && fault != ""
	}, 3*time.Second)
	elapsed := time.Since(start)
	// StepTimeout default is 200ms; the per-step override is 1s — the wait must
	// NOT fault in ~200ms, it must take ~1s.
	if elapsed < 800*time.Millisecond {
		t.Errorf("faulted after %s; per-step timeout_s=1 should hold ~1s, not the 200ms default", elapsed)
	}
}

func TestCtxCancelInterrupts(t *testing.T) {
	s, pub := newTestSeq(t)
	s.markOnline("power/master") // so wait-controllers is the blocking step
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)

	s.Start()
	runUntil(t, s, func() bool {
		_, step, _ := s.Phase()
		return step == "wait-controllers"
	}, time.Second)

	cancel()
	runUntil(t, s, func() bool {
		ph, _, fault := s.Phase()
		return ph == PhaseIdle && fault != ""
	}, time.Second)
	_, _, fault := s.Phase()
	if !strings.Contains(fault, "interrupted") {
		t.Errorf("fault = %q, want interrupted", fault)
	}
	// master + psu emitted before cancel; no rollback.
	if got := len(pub.cmds()); got != 2 {
		t.Errorf("cmds = %d, want 2 (master+psu, no rollback)", got)
	}
}

func TestDelayDurations(t *testing.T) {
	// network → NetworkDelay (5ms), stagger → ShutdownStagger (2ms),
	// duration_s → literal seconds. Verified via resolveDelay directly.
	for _, tc := range []struct {
		name string
		step Step
		want time.Duration
	}{
		{"network", Step{Kind: KindDelay, Duration: "network"}, 5 * time.Millisecond},
		{"stagger", Step{Kind: KindDelay, Duration: "stagger"}, 2 * time.Millisecond},
		{"literal", Step{Kind: KindDelay, DurationS: intPtr(3)}, 3 * time.Second},
	} {
		d, err := resolveDelay(tc.step, baseConfig(exampleStartup(), exampleShutdown()))
		if err != nil {
			t.Errorf("%s: resolveDelay: %v", tc.name, err)
			continue
		}
		if d != tc.want {
			t.Errorf("%s: duration = %s, want %s", tc.name, d, tc.want)
		}
	}
}

func intPtr(v int) *int { return &v }

func TestWaitStatusOffline(t *testing.T) {
	// wait_status state="offline" passes only on an actual offline payload,
	// not on absence (a slot that never published).
	up := []Step{
		{Name: "wait-down", Kind: KindWaitStatus, Slots: []string{"hf/radio"}, State: "offline"},
	}
	s, _ := newTestSeqWith(t, up, exampleShutdown())
	s.SetBrokerOnline(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Start()
	// Absence must not satisfy "offline": the wait should stay blocked.
	time.Sleep(StepTimeoutInTest() / 2)
	if phaseIs(s, PhaseRunning) {
		t.Fatal("passed on absence; state=offline requires an actual offline payload")
	}
	// An explicit offline payload satisfies it.
	s.markOffline("hf/radio")
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
}

func TestStateFieldCoercionAndMissing(t *testing.T) {
	// bool → "true", number → decimal string, string → as-is, missing → "".
	s, _ := newTestSeq(t)
	// Drive the coerceToString helper directly (it is the comparison core).
	for _, tc := range []struct {
		v    any
		want string
	}{
		{nil, ""},
		{true, "true"},
		{false, "false"},
		{float64(42), "42"},
		{"on", "on"},
	} {
		if got := coerceToString(tc.v); got != tc.want {
			t.Errorf("coerce(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}

	// A /state snapshot missing the field yields "" (does not panic).
	s.SetState(abs("hf/pa"), []byte(`{"something":"else"}`))
	if got := s.stateField(abs("hf/pa"), "power"); got != "" {
		t.Errorf("missing field = %q, want \"\"", got)
	}
	// A malformed /state payload drops the prior snapshot (no poisoning): the
	// field reads "" — and crucially a GOOD→malformed transition must not keep
	// the stale prior value.
	s.SetState(abs("hf/pa"), []byte(`{"power":"on"}`))
	if got := s.stateField(abs("hf/pa"), "power"); got != "on" {
		t.Errorf("good state field = %q, want on", got)
	}
	s.SetState(abs("hf/pa"), []byte(`not json`))
	if got := s.stateField(abs("hf/pa"), "power"); got != "" {
		t.Errorf("after malformed over a good snapshot, stale field = %q, want \"\" (cleared, not poisoned)", got)
	}
}

// TestSetStateMalformedClearsStale is the dedicated regression for the
// good→malformed transition the adversarial review flagged: a prior good
// snapshot must NOT survive a malformed publish.
func TestSetStateMalformedClearsStale(t *testing.T) {
	s, _ := newTestSeq(t)
	s.SetState(abs("hf/pa"), []byte(`{"power":"on"}`))
	if got := s.stateField(abs("hf/pa"), "power"); got != "on" {
		t.Fatalf("seed field = %q, want on", got)
	}
	s.SetState(abs("hf/pa"), []byte(`not json`))
	if got := s.stateField(abs("hf/pa"), "power"); got != "" {
		t.Fatalf("stale field after malformed = %q, want \"\" (snapshot must be cleared)", got)
	}
	// A subsequent good publish repopulates normally.
	s.SetState(abs("hf/pa"), []byte(`{"power":"off"}`))
	if got := s.stateField(abs("hf/pa"), "power"); got != "off" {
		t.Errorf("re-seeded field = %q, want off", got)
	}
}

// TestBusyGuardDoubleStart exercises the runner dequeue re-check (begin): two
// start commands enqueued while phase is still idle must run the startup only
// ONCE, not twice. Without the begin() re-check the second would replay the
// whole sequence (the TOCTOU the adversarial review flagged).
func TestBusyGuardDoubleStart(t *testing.T) {
	s, pub := newTestSeq(t)
	s.allObserved()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Enqueue two starts BEFORE the runner processes any, bypassing request()'s
	// fast-path guard so both sit in cmdCh while phase is still idle.
	s.cmdCh <- "start"
	s.cmdCh <- "start"
	go s.Run(ctx)
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)

	// Give the runner a moment to drain + drop the second command.
	runUntil(t, s, func() bool { return len(pub.cmds()) == 5 }, time.Second)
	if got := len(pub.cmds()); got != 5 {
		t.Errorf("double-tapped start ran %d cmds, want 5 (one startup; second dropped by begin re-check)", got)
	}
}

// TestStateAlwaysHasStepKey asserts the retained /state always carries the
// `step` key (model §7.1: only fault is omitempty), even when step is empty.
func TestStateAlwaysHasStepKey(t *testing.T) {
	s, pub := newTestSeq(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// On entry the runner publishes an idle /state with an empty step.
	runUntil(t, s, func() bool {
		for _, m := range pub.statesRaw() {
			if strings.Contains(string(m), `"phase":"idle"`) && strings.Contains(string(m), `"step"`) {
				return true
			}
		}
		return false
	}, time.Second)
}

func TestCmdPublishFailureFaults(t *testing.T) {
	pub := &errPublisher{recPublisher: recPublisher{}}
	s, err := New(baseConfig(exampleStartup(), exampleShutdown()), pub, &testLogger{t})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetBrokerOnline(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.Start()
	runUntil(t, s, func() bool {
		ph, _, fault := s.Phase()
		return ph == PhaseIdle && fault != ""
	}, time.Second)
	_, _, fault := s.Phase()
	if !strings.Contains(fault, "publish failed") {
		t.Errorf("fault = %q, want publish failed", fault)
	}
}

func TestStatePublishesPhase(t *testing.T) {
	s, pub := newTestSeq(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// Run() publishes an initial idle /state on entry.
	runUntil(t, s, func() bool {
		for _, st := range pub.states() {
			if st.Phase == PhaseIdle {
				return true
			}
		}
		return false
	}, time.Second)
}

func mustPhase(s *Sequencer) string {
	ph, _, _ := s.Phase()
	return ph
}
