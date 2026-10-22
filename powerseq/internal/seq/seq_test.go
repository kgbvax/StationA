package seq

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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

func (l *testLogger) Infof(f string, a ...any)  { l.t.Logf("INFO: "+f, a...) }
func (l *testLogger) Warnf(f string, a ...any)  { l.t.Logf("WARN: "+f, a...) }
func (l *testLogger) Errorf(f string, a ...any) { l.t.Logf("ERROR: "+f, a...) }
func (l *testLogger) Debugf(string, ...any)     {}

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

	// stop while idle (no fault) → honored: idempotent teardown (the fault
	// latch is in-memory; a restart wipes it, so stop must not depend on it).
	s.Stop()
	runUntil(t, s, func() bool { return len(pub.cmds()) == 5 }, time.Second)
	if !phaseIs(s, PhaseIdle) {
		t.Errorf("stop from plain idle ended in phase=%s, want idle", mustPhase(s))
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

// A process restart wipes the in-memory fault latch (fresh Sequencer,
// fault=""): stop must still tear the station down — it is the only rollback
// path for slots a faulted sequence left energized (review: "boot wipes fault
// → stop ignored after restart").
func TestRestartAfterFaultStopStillHonored(t *testing.T) {
	// First life: a startup faults mid-sequence; slots stay energized
	// (no rollback by design).
	s1, _ := newTestSeq(t)
	s1.markOnline("power/master")
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go s1.Run(ctx1)
	s1.Start()
	runUntil(t, s1, func() bool {
		ph, _, fault := s1.Phase()
		return ph == PhaseIdle && fault != ""
	}, time.Second)

	// Second life: a fresh process — fault latch gone, phase=idle.
	s2, pub2 := newTestSeq(t)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go s2.Run(ctx2)
	runUntil(t, s2, func() bool { return phaseIs(s2, PhaseIdle) }, time.Second)

	s2.Stop()
	runUntil(t, s2, func() bool { return len(pub2.cmds()) == 5 }, time.Second)
	cmds := pub2.cmds()
	if len(cmds) != 5 {
		t.Fatalf("post-restart stop emitted %+v, want 5 shutdown cmds", cmds)
	}
	last := cmds[len(cmds)-1]
	if last.topic != abs("power/master")+"/cmd" || last.value != "off" {
		t.Errorf("last shutdown cmd = %+v, want master off", last)
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
	// wait_state slots + every wait_status online slot (device_online gate).
	wantState := []string{
		abs("hf/ant-switch"), abs("hf/pa"), abs("hf/pa-arm"), abs("hf/radio"), abs("hf/switch"),
	}
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

func TestWaitStatusOnlineRequiresDeviceOnline(t *testing.T) {
	// 2026-10-18: flexbridge /status was online (bridge up) while the FLEX was
	// still booting (device_online=false); wait-radio-online passed at once.
	// state="online" must be two-layer: LWT online AND device_online not false.
	up := []Step{
		{Name: "wait-radio-online", Kind: KindWaitStatus, Slots: []string{"hf/radio"}},
	}
	s, _ := newTestSeqWith(t, up, exampleShutdown())
	s.SetBrokerOnline(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.SetStatus(abs("hf/radio"), true)
	s.SetState(abs("hf/radio"), []byte(`{"freq_hz":0,"device_online":false}`))
	s.Start()
	time.Sleep(StepTimeoutInTest() / 2)
	if phaseIs(s, PhaseRunning) {
		t.Fatal("passed with device_online=false behind an online bridge")
	}
	s.SetState(abs("hf/radio"), []byte(`{"freq_hz":14074000,"device_online":true}`))
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

// ----- idle standby / wake ---------------------------------------------------

func standbySteps() []Step {
	f := false
	return []Step{
		{Name: "antennas-off", Kind: KindCmd, Slot: "power/antennas", Action: "set_power", Value: "off"},
		{Name: "pc-sleep", Kind: KindCmd, Slot: "power/shack-pc", Action: "sleep", Value: "now", Retain: &f},
	}
}

func wakeSteps() []Step {
	return []Step{
		{Name: "pc-wake", Kind: KindWOL, MAC: "aa:bb:cc:dd:ee:ff", Broadcast: "192.168.1.255:9"},
		{Name: "antennas-on", Kind: KindCmd, Slot: "power/antennas", Action: "set_power", Value: "on"},
	}
}

type wolRec struct {
	mu    sync.Mutex
	calls []string
}

func (w *wolRec) send(mac net.HardwareAddr, addr string) error {
	w.mu.Lock()
	w.calls = append(w.calls, mac.String()+"@"+addr)
	w.mu.Unlock()
	return nil
}

func (w *wolRec) n() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

const testIdleTimeout = 80 * time.Millisecond

// newStandbySeq builds a sequencer with idle standby configured, started and
// in phase=running (startup = one cmd step so it reaches running at once).
func newStandbySeq(t *testing.T) (*Sequencer, *recPublisher, *wolRec, context.CancelFunc) {
	t.Helper()
	return newStandbySeqWith(t, nil)
}

// newStandbySeqWith is newStandbySeq with a config hook applied before New.
func newStandbySeqWith(t *testing.T, mut func(*Config)) (*Sequencer, *recPublisher, *wolRec, context.CancelFunc) {
	t.Helper()
	pub := &recPublisher{}
	wol := &wolRec{}
	cfg := baseConfig(
		[]Step{{Name: "master-on", Kind: KindCmd, Slot: "power/master", Action: "set_power", Value: "on"}},
		exampleShutdown())
	cfg.Standby = standbySteps()
	cfg.Wake = wakeSteps()
	cfg.ActivitySlot = "hf/antenna-select"
	cfg.IdleTimeout = testIdleTimeout
	cfg.IdleCheck = 2 * time.Millisecond
	cfg.Watches = []Watch{
		{Slot: "uhf/radio", Field: "freq_hz"},
		{Slot: "uhf/el-rotator", Field: "el", MinDelta: 2},
	}
	cfg.WOL = wol.send
	if mut != nil {
		mut(&cfg)
	}
	s, err := New(cfg, pub, &testLogger{t})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetBrokerOnline(true)
	s.markOnline("hf/antenna-select", "uhf/radio", "uhf/el-rotator")
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	s.Start()
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
	return s, pub, wol, cancel
}

func (s *Sequencer) setActivity(activity string, last time.Time) {
	m := map[string]any{"activity": activity}
	if !last.IsZero() {
		m["last_activity"] = last.UTC().Format(time.RFC3339Nano)
	}
	b, _ := json.Marshal(m)
	s.SetState(abs("hf/antenna-select"), b)
}

func cmdsTo(pub *recPublisher, rel string) []cmdTuple {
	var out []cmdTuple
	for _, c := range pub.cmds() {
		if c.topic == abs(rel)+"/cmd" {
			out = append(out, c)
		}
	}
	return out
}

func TestStandbyAfterIdleAndWakeOnActivity(t *testing.T) {
	s, pub, wol, cancel := newStandbySeq(t)
	defer cancel()

	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)

	ant := cmdsTo(pub, "power/antennas")
	if len(ant) != 1 || ant[0].value != "off" || !ant[0].retained {
		t.Fatalf("antennas cmds = %+v, want one retained off", ant)
	}
	pc := cmdsTo(pub, "power/shack-pc")
	if len(pc) != 1 || pc[0].action != "sleep" || pc[0].retained {
		t.Fatalf("shack-pc cmds = %+v, want one NOT retained sleep", pc)
	}

	// Fresh VFO activity: antennaselect reports a last_activity after standby.
	s.setActivity("active", time.Now())
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
	if wol.n() != 1 || wol.calls[0] != "aa:bb:cc:dd:ee:ff@192.168.1.255:9" {
		t.Errorf("wol calls = %v", wol.calls)
	}
	ant = cmdsTo(pub, "power/antennas")
	if len(ant) != 2 || ant[1].value != "on" {
		t.Errorf("antennas cmds after wake = %+v", ant)
	}
}

func TestNoStandbyBeforeRunningForTimeout(t *testing.T) {
	s, _, _, cancel := newStandbySeq(t)
	defer cancel()
	// antenna-select was already inactive at start: standby must still wait a
	// full idle timeout after phase=running.
	s.setActivity("inactive", time.Time{})
	time.Sleep(testIdleTimeout / 2)
	if !phaseIs(s, PhaseRunning) {
		t.Fatalf("standby before the idle timeout: phase=%s", mustPhase(s))
	}
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
}

func TestActiveStationNeverStandsBy(t *testing.T) {
	s, _, _, cancel := newStandbySeq(t)
	defer cancel()
	s.setActivity("active", time.Now())
	time.Sleep(3 * testIdleTimeout)
	if !phaseIs(s, PhaseRunning) {
		t.Fatalf("phase=%s, want running while active", mustPhase(s))
	}
}

func TestWatchActivityDefersStandby(t *testing.T) {
	s, _, _, cancel := newStandbySeq(t)
	defer cancel()
	s.setActivity("inactive", time.Time{})
	s.SetState(abs("uhf/radio"), []byte(`{"freq_hz":145800000,"device_online":true}`))
	// Keep the UHF radio moving (doppler) past the idle timeout.
	deadline := time.Now().Add(2 * testIdleTimeout)
	f := 145800000
	for time.Now().Before(deadline) {
		f += 50
		s.SetState(abs("uhf/radio"), []byte(fmt.Sprintf(`{"freq_hz":%d,"device_online":true}`, f)))
		if !phaseIs(s, PhaseRunning) {
			t.Fatalf("standby while the UHF radio is in use: phase=%s", mustPhase(s))
		}
		time.Sleep(testIdleTimeout / 8)
	}
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
}

func TestWatchDeadbandAndPowerLossAreNotActivity(t *testing.T) {
	s, _, wol, cancel := newStandbySeq(t)
	defer cancel()
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":0,"device_online":true}`))
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)

	// el dithers 0↔1 (ADC noise) — below the 2° deadband.
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":1,"device_online":true}`))
	// Antenna power off: the device drops, the position vanishes.
	s.SetState(abs("uhf/el-rotator"), []byte(`{"device_online":false}`))
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":37,"device_online":false}`))
	time.Sleep(20 * time.Millisecond)
	if !phaseIs(s, PhaseStandby) || wol.n() != 0 {
		t.Fatalf("woke on noise/power loss: phase=%s wol=%d", mustPhase(s), wol.n())
	}

	// A real move (≥ 2°) while fit wakes the station.
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":5,"device_online":true}`))
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
}

func TestAntennaselectRestartDoesNotWake(t *testing.T) {
	s, _, wol, cancel := newStandbySeq(t)
	defer cancel()
	s.setActivity("inactive", time.Now().Add(-time.Hour))
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)

	// antennaselect restarts: offline, online, activity=active, no last_activity.
	s.markOffline("hf/antenna-select")
	s.markOnline("hf/antenna-select")
	s.setActivity("active", time.Time{})
	time.Sleep(20 * time.Millisecond)
	if !phaseIs(s, PhaseStandby) || wol.n() != 0 {
		t.Fatalf("restart woke the station: phase=%s wol=%d", mustPhase(s), wol.n())
	}
}

func TestStartFromStandbyWakesWithoutInstantRestandby(t *testing.T) {
	s, pub, wol, cancel := newStandbySeq(t)
	defer cancel()
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)

	s.Start() // operator START in standby = wake (not the full startup)
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
	if wol.n() != 1 {
		t.Errorf("wol calls = %d, want 1", wol.n())
	}
	if n := len(cmdsTo(pub, "power/master")); n != 1 {
		t.Errorf("start from standby re-ran startup: %d master cmds", n)
	}
	// antenna-select still says inactive; standby must wait a fresh timeout.
	time.Sleep(testIdleTimeout / 2)
	if !phaseIs(s, PhaseRunning) {
		t.Fatalf("instant re-standby after a START wake: phase=%s", mustPhase(s))
	}
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
}

func TestStopFromStandby(t *testing.T) {
	s, _, _, cancel := newStandbySeq(t)
	defer cancel()
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
	s.Stop()
	runUntil(t, s, func() bool { return phaseIs(s, PhaseIdle) }, time.Second)
	// A stopped station never stands by or wakes.
	s.setActivity("active", time.Now())
	time.Sleep(20 * time.Millisecond)
	if !phaseIs(s, PhaseIdle) {
		t.Fatalf("phase=%s after stop, want idle", mustPhase(s))
	}
}

func TestStandbyGatedOnBroker(t *testing.T) {
	s, _, _, cancel := newStandbySeq(t)
	defer cancel()
	s.SetBrokerOnline(false)
	s.setActivity("inactive", time.Time{})
	time.Sleep(2 * testIdleTimeout)
	if !phaseIs(s, PhaseRunning) {
		t.Fatalf("standby with the broker down: phase=%s", mustPhase(s))
	}
}

func TestStandbyRequiresWake(t *testing.T) {
	cfg := baseConfig(exampleStartup(), exampleShutdown())
	cfg.Standby = standbySteps()
	cfg.ActivitySlot = "hf/antenna-select"
	cfg.IdleTimeout = time.Minute
	if _, err := New(cfg, &recPublisher{}, &testLogger{t}); err == nil {
		t.Fatal("standby without wake accepted")
	}
}

func TestStandbySubscriptionsAndMeta(t *testing.T) {
	s, _, _, cancel := newStandbySeq(t)
	defer cancel()
	status, state := s.Subscriptions()
	for _, want := range []string{"hf/antenna-select", "uhf/radio", "uhf/el-rotator", "power/antennas", "power/shack-pc"} {
		if !contains(status, abs(want)) {
			t.Errorf("status subscriptions miss %s: %v", want, status)
		}
	}
	for _, want := range []string{"hf/antenna-select", "uhf/radio", "uhf/el-rotator"} {
		if !contains(state, abs(want)) {
			t.Errorf("state subscriptions miss %s: %v", want, state)
		}
	}
	var meta struct {
		Capabilities struct{ Controls []string } `json:"capabilities"`
	}
	if err := json.Unmarshal(s.MetaPayload(), &meta); err != nil {
		t.Fatal(err)
	}
	if !contains(meta.Capabilities.Controls, abs("power/shack-pc")) {
		t.Errorf("meta controls miss power/shack-pc: %v", meta.Capabilities.Controls)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestMagicPacket(t *testing.T) {
	mac, _ := net.ParseMAC("01:23:45:67:89:ab")
	p := MagicPacket(mac)
	if len(p) != 102 {
		t.Fatalf("len = %d, want 102", len(p))
	}
	for i := 0; i < 6; i++ {
		if p[i] != 0xFF {
			t.Fatalf("header byte %d = %x", i, p[i])
		}
	}
	for i := 0; i < 16; i++ {
		if string(p[6+i*6:12+i*6]) != string(mac) {
			t.Fatalf("repetition %d wrong", i)
		}
	}
}

func TestWaitTimeoutReportsObserved(t *testing.T) {
	// The fault must say what the sequencer saw, not just "timeout".
	up := []Step{
		{Name: "wait-radio-online", Kind: KindWaitStatus, Slots: []string{"hf/radio", "hf/switch"}},
	}
	s, _ := newTestSeqWith(t, up, exampleShutdown())
	s.SetBrokerOnline(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	s.SetStatus(abs("hf/radio"), true)
	s.SetState(abs("hf/radio"), []byte(`{"device_online":false}`))
	s.Start()
	runUntil(t, s, func() bool {
		ph, _, fault := s.Phase()
		return ph == PhaseIdle && fault != ""
	}, time.Second)

	_, _, fault := s.Phase()
	for _, want := range []string{
		"wait-radio-online: timeout (",
		"hf/radio status=online device_online=false",
		"hf/switch status=none state=none",
	} {
		if !strings.Contains(fault, want) {
			t.Errorf("fault = %q, want it to contain %q", fault, want)
		}
	}
}

// ----- idle shutdown ---------------------------------------------------------

const testShutdownTimeout = 300 * time.Millisecond

func idleShutdownSteps() []Step {
	f := false
	return append(exampleShutdown(),
		Step{Name: "pc-sleep", Kind: KindCmd, Slot: "power/shack-pc", Action: "sleep", Value: "now", Retain: &f})
}

func newIdleShutdownSeq(t *testing.T) (*Sequencer, *recPublisher, *wolRec, context.CancelFunc) {
	t.Helper()
	return newStandbySeqWith(t, func(c *Config) {
		c.IdleShutdown = idleShutdownSteps()
		c.ShutdownTimeout = testShutdownTimeout
	})
}

func lastState(pub *recPublisher) statePayload {
	st := pub.states()
	if len(st) == 0 {
		return statePayload{}
	}
	return st[len(st)-1]
}

func TestIdleShutdownAfterTimeoutThenStart(t *testing.T) {
	s, pub, _, cancel := newIdleShutdownSeq(t)
	defer cancel()
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
	runUntil(t, s, func() bool { return phaseIs(s, PhaseIdle) }, 2*time.Second)

	if _, _, fault := s.Phase(); fault != "" {
		t.Fatalf("idle shutdown faulted: %s", fault)
	}
	if got := lastState(pub).StopReason; got != "idle_shutdown" {
		t.Errorf("stop_reason = %q, want idle_shutdown", got)
	}
	if sw := cmdsTo(pub, "hf/switch"); len(sw) != 2 || sw[0].value != "off" || sw[1].value != "off" {
		t.Errorf("hf/switch cmds = %+v, want pa off + trx off", sw)
	}
	if arm := cmdsTo(pub, "hf/pa-arm"); len(arm) != 1 || arm[0].value != "false" {
		t.Errorf("pa-arm cmds = %+v, want one disable", arm)
	}
	if pc := cmdsTo(pub, "power/shack-pc"); len(pc) != 2 || pc[1].retained {
		t.Errorf("shack-pc cmds = %+v, want standby + idle-shutdown sleep (not retained)", pc)
	}

	// A start after an idle shutdown is a full cold start and clears the reason.
	s.setActivity("active", time.Now())
	s.Start()
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
	if m := cmdsTo(pub, "power/master"); m[len(m)-1].value != "on" {
		t.Errorf("power/master cmds = %+v, want the restart's on last", m)
	}
	if got := lastState(pub).StopReason; got != "" {
		t.Errorf("stop_reason after start = %q, want empty", got)
	}
}

func TestIdleShutdownClockFollowsLastActivity(t *testing.T) {
	s, _, _, cancel := newIdleShutdownSeq(t)
	defer cancel()
	// Activity 150 ms into running, then idle: the shutdown clock starts at
	// that last_activity, not at the running entry.
	time.Sleep(150 * time.Millisecond)
	last := time.Now()
	s.setActivity("inactive", last)
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
	time.Sleep(time.Until(last.Add(testShutdownTimeout - 70*time.Millisecond)))
	if !phaseIs(s, PhaseStandby) {
		t.Fatalf("idle shutdown before shutdown timeout after last_activity: phase=%s", mustPhase(s))
	}
	runUntil(t, s, func() bool { return phaseIs(s, PhaseIdle) }, time.Second)
}

func TestIdleShutdownSurvivesAntennaselectRestart(t *testing.T) {
	s, pub, wol, cancel := newIdleShutdownSeq(t)
	defer cancel()
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
	// Restart publishes no last_activity: neither a wake nor a clock reset.
	s.markOffline("hf/antenna-select")
	s.markOnline("hf/antenna-select")
	s.setActivity("active", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseIdle) }, 2*time.Second)
	if wol.n() != 0 || lastState(pub).StopReason != "idle_shutdown" {
		t.Errorf("wol=%d stop_reason=%q, want 0 / idle_shutdown", wol.n(), lastState(pub).StopReason)
	}
}

func TestWatchInStandbyWakesInsteadOfShutdown(t *testing.T) {
	s, _, wol, cancel := newIdleShutdownSeq(t)
	defer cancel()
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":10,"device_online":true}`))
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":30,"device_online":true}`))
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
	if wol.n() != 1 {
		t.Errorf("wol calls = %d, want 1", wol.n())
	}
}

func TestIdleShutdownGatedOnBroker(t *testing.T) {
	s, _, _, cancel := newIdleShutdownSeq(t)
	defer cancel()
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
	s.SetBrokerOnline(false)
	time.Sleep(testShutdownTimeout + 100*time.Millisecond)
	if !phaseIs(s, PhaseStandby) {
		t.Fatalf("idle shutdown with the broker down: phase=%s", mustPhase(s))
	}
}

func TestNoIdleShutdownWhenUnconfigured(t *testing.T) {
	s, _, _, cancel := newStandbySeq(t)
	defer cancel()
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
	time.Sleep(testShutdownTimeout + 100*time.Millisecond)
	if !phaseIs(s, PhaseStandby) {
		t.Fatalf("phase=%s, want standby (no idle shutdown configured)", mustPhase(s))
	}
}

func TestOperatorStopSetsStopReason(t *testing.T) {
	s, pub, _, cancel := newStandbySeq(t)
	defer cancel()
	s.Stop()
	runUntil(t, s, func() bool { return phaseIs(s, PhaseIdle) }, time.Second)
	if got := lastState(pub).StopReason; got != "operator" {
		t.Errorf("stop_reason = %q, want operator", got)
	}
}

func TestIdleShutdownConfigRejected(t *testing.T) {
	cases := map[string]func(*Config){
		"without standby": func(c *Config) {
			c.Standby, c.Wake = nil, nil
			c.IdleShutdown = idleShutdownSteps()
			c.ShutdownTimeout = time.Hour
		},
		"timeout not above idle timeout": func(c *Config) {
			c.IdleShutdown = idleShutdownSteps()
			c.ShutdownTimeout = c.IdleTimeout
		},
	}
	for name, mut := range cases {
		cfg := baseConfig(exampleStartup(), exampleShutdown())
		cfg.Standby = standbySteps()
		cfg.Wake = wakeSteps()
		cfg.ActivitySlot = "hf/antenna-select"
		cfg.IdleTimeout = time.Minute
		mut(&cfg)
		if _, err := New(cfg, &recPublisher{}, &testLogger{t}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Regression 2026-10-19: standby switches the antenna power off; the UHF
// rotator controllers keep their link up (device_online=true) but report a
// stepped position. Counted as a watch move, that woke every standby ~2 s
// after entry, so the station cycled running→standby→running every 30 min and
// the idle shutdown could never fire. A change within the settle window after
// a phase change re-baselines the watch; a later real move still wakes.
func TestPowerStepInSettleWindowDoesNotWake(t *testing.T) {
	const settle = 60 * time.Millisecond
	s, _, wol, cancel := newStandbySeqWith(t, func(c *Config) { c.WatchSettle = settle })
	defer cancel()
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":3,"device_online":true}`))
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)

	// Antenna supply off: readback steps 3 → 0 with the link still up.
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":0,"device_online":true}`))
	time.Sleep(settle + 20*time.Millisecond)
	// Heartbeat of the new (unpowered) value after the window: not a move.
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":0,"device_online":true}`))
	time.Sleep(20 * time.Millisecond)
	if !phaseIs(s, PhaseStandby) || wol.n() != 0 {
		t.Fatalf("woke on the standby power step: phase=%s wol=%d", mustPhase(s), wol.n())
	}

	// A real move after the window still wakes the station.
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":10,"device_online":true}`))
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)

	// Wake powers the antennas on again: the readback steps back within the
	// window. That is not activity, so standby follows one idle timeout after
	// the wake instead of being deferred by the power step.
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":3,"device_online":true}`))
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
}

// ----- antenna-mode power hold ----------------------------------------------

// newHoldSeq is newStandbySeq with the ant-mode slot read.
func newHoldSeq(t *testing.T, mut func(*Config)) (*Sequencer, *recPublisher, *wolRec, context.CancelFunc) {
	t.Helper()
	return newStandbySeqWith(t, func(c *Config) {
		c.AntModeSlot = "ant-mode"
		c.AntModeHold = time.Millisecond // no reconnect wait unless a test sets one
		if mut != nil {
			mut(c)
		}
	})
}

func (s *Sequencer) setHold(hold bool) {
	s.SetAntMode([]byte(fmt.Sprintf(`{"ant_mode":"storm","lock":{"seq":1,"ground":true,"retract":true},"power_hold":%t}`, hold)))
}

func (s *Sequencer) holding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.powerHold
}

func TestAntModeTopicAndMeta(t *testing.T) {
	s, _, _, cancel := newHoldSeq(t, nil)
	defer cancel()
	want := testSite + "/" + testStation + "/ant-mode"
	if got := s.AntModeStateTopic(); got != want+"/state" {
		t.Errorf("AntModeStateTopic = %q, want %q", got, want+"/state")
	}
	var meta struct {
		Capabilities struct{ Watches []string } `json:"capabilities"`
	}
	if err := json.Unmarshal(s.MetaPayload(), &meta); err != nil {
		t.Fatal(err)
	}
	if !contains(meta.Capabilities.Watches, want) {
		t.Errorf("meta watches miss %s: %v", want, meta.Capabilities.Watches)
	}

	// Not configured, or no idle standby: not read.
	s2, _, _, cancel2 := newStandbySeq(t)
	defer cancel2()
	if got := s2.AntModeStateTopic(); got != "" {
		t.Errorf("unconfigured AntModeStateTopic = %q", got)
	}
	cfg := baseConfig(exampleStartup(), exampleShutdown())
	cfg.AntModeSlot = "ant-mode"
	s3, err := New(cfg, &recPublisher{}, &testLogger{t})
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.AntModeStateTopic(); got != "" {
		t.Errorf("AntModeStateTopic without standby = %q", got)
	}
}

// The station is idle and in running: while antmode borrows the antenna
// power, no standby is entered; once the hold ends, standby follows.
func TestPowerHoldBlocksStandby(t *testing.T) {
	s, _, _, cancel := newHoldSeq(t, nil)
	defer cancel()
	s.setHold(true)
	s.setActivity("inactive", time.Time{})
	time.Sleep(3 * testIdleTimeout)
	if !phaseIs(s, PhaseRunning) {
		t.Fatalf("standby during the power hold: phase=%s", mustPhase(s))
	}
	s.setHold(false)
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
}

// Regression guard for the borrow (plan phase 5): station in standby, antmode
// switches the antenna power on to park and off again. The rotator readbacks
// step both times with the link up (and the UHF watches may move meanwhile);
// none of it wakes the station. A real move after the settle window does.
func TestPowerHoldWatchChangesDoNotWake(t *testing.T) {
	const settle = 60 * time.Millisecond
	s, _, wol, cancel := newHoldSeq(t, func(c *Config) { c.WatchSettle = settle })
	defer cancel()
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":3,"device_online":true}`))
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
	time.Sleep(settle + 20*time.Millisecond) // past the standby-entry window

	s.setHold(true)
	// Borrowed power on: readback steps, then moves while the HF park runs.
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":0,"device_online":true}`))
	time.Sleep(settle + 20*time.Millisecond) // well outside any phase window
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":20,"device_online":true}`))
	s.SetState(abs("uhf/radio"), []byte(`{"freq_hz":145800000,"device_online":true}`))
	s.SetState(abs("uhf/radio"), []byte(`{"freq_hz":145900000,"device_online":true}`))
	time.Sleep(20 * time.Millisecond)
	if !phaseIs(s, PhaseStandby) || wol.n() != 0 {
		t.Fatalf("woke during the power hold: phase=%s wol=%d", mustPhase(s), wol.n())
	}

	// Power restored to off, hold cleared: the readback step right after is
	// inside the fresh settle window and only re-baselines.
	s.setHold(false)
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":3,"device_online":true}`))
	time.Sleep(settle + 20*time.Millisecond)
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":3,"device_online":true}`))
	time.Sleep(20 * time.Millisecond)
	if !phaseIs(s, PhaseStandby) || wol.n() != 0 {
		t.Fatalf("woke on the power-restore step: phase=%s wol=%d", mustPhase(s), wol.n())
	}

	// After the window a real move wakes the station again.
	s.SetState(abs("uhf/el-rotator"), []byte(`{"el":15,"device_online":true}`))
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
}

// A last_activity newer than the standby entry would wake the station; during
// the hold no wake is started (the borrow is not cut short by a wake).
func TestPowerHoldDefersWake(t *testing.T) {
	s, _, wol, cancel := newHoldSeq(t, nil)
	defer cancel()
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
	s.setHold(true)
	s.setActivity("active", time.Now())
	time.Sleep(30 * time.Millisecond)
	if !phaseIs(s, PhaseStandby) || wol.n() != 0 {
		t.Fatalf("wake during the power hold: phase=%s wol=%d", mustPhase(s), wol.n())
	}
	s.setHold(false)
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
}

func TestPowerHoldBlocksIdleShutdown(t *testing.T) {
	s, _, _, cancel := newHoldSeq(t, func(c *Config) {
		c.IdleShutdown = idleShutdownSteps()
		c.ShutdownTimeout = testShutdownTimeout
	})
	defer cancel()
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
	s.setHold(true)
	time.Sleep(testShutdownTimeout + 100*time.Millisecond)
	if !phaseIs(s, PhaseStandby) {
		t.Fatalf("idle shutdown during the power hold: phase=%s", mustPhase(s))
	}
	// The hold end restarts the clock: the shutdown follows ShutdownTimeout
	// later (TestLongPowerHoldRestartsIdleShutdownClock).
	s.setHold(false)
	runUntil(t, s, func() bool { return phaseIs(s, PhaseIdle) }, time.Second)
}

func TestAntModeReadingRules(t *testing.T) {
	s, _, _, cancel := newHoldSeq(t, nil)
	defer cancel()
	if s.holding() {
		t.Fatal("hold before any ant-mode state")
	}
	s.setHold(true)
	// Malformed payloads keep the last known hold.
	for _, bad := range []string{`{`, `{"ant_mode":"hurricane","power_hold":false}`, `{"ant_mode":"storm","lock":{"park_az":999}}`} {
		s.SetAntMode([]byte(bad))
		if !s.holding() {
			t.Fatalf("malformed %q dropped the hold", bad)
		}
	}
	// Empty / cleared retained state = normal = no hold.
	s.SetAntMode(nil)
	if s.holding() {
		t.Fatal("empty ant-mode state kept the hold")
	}
	s.setHold(true)
	s.SetAntMode([]byte(`{"ant_mode":"normal"}`))
	if s.holding() {
		t.Fatal("normal state without power_hold kept the hold")
	}
	// A hold is honored in any mode it is published with.
	s.SetAntMode([]byte(`{"ant_mode":"maintenance","power_hold":true}`))
	if !s.holding() {
		t.Fatal("maintenance power_hold not honored")
	}
	// Malformed while not holding keeps "no hold".
	s.SetAntMode([]byte(`{"ant_mode":"normal"}`))
	s.SetAntMode([]byte(`garbage`))
	if s.holding() {
		t.Fatal("malformed state set a hold")
	}
}

// Reconnect order (OnConnect sets the broker online before the ant-mode
// /state is re-delivered): a borrow that started while powerseq was
// disconnected must not be cut by an overdue idle shutdown in that gap.
func TestReconnectWaitsForAntModeState(t *testing.T) {
	s, _, _, cancel := newHoldSeq(t, func(c *Config) {
		c.IdleShutdown = idleShutdownSteps()
		c.ShutdownTimeout = testShutdownTimeout
		c.AntModeHold = 150 * time.Millisecond
	})
	defer cancel()
	s.setHold(false)
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
	s.SetBrokerOnline(false)
	time.Sleep(testShutdownTimeout + 50*time.Millisecond) // overdue while down
	s.SetBrokerOnline(true)
	time.Sleep(40 * time.Millisecond) // retained {power_hold:true} arrives late
	s.setHold(true)
	time.Sleep(200 * time.Millisecond)
	if !phaseIs(s, PhaseStandby) {
		t.Fatalf("idle shutdown before the ant-mode state was re-delivered: phase=%s", mustPhase(s))
	}
	// No retained ant-mode state after a reconnect = no hold, once the
	// window has passed.
	s.setHold(false)
	s.SetBrokerOnline(false)
	s.SetBrokerOnline(true)
	runUntil(t, s, func() bool { return phaseIs(s, PhaseIdle) }, time.Second)
}

// A hold longer than the idle-shutdown timeout (maintenance) restarts the
// idle-shutdown clock when it ends: the station is not powered down on the
// next tick while the operator is still there.
func TestLongPowerHoldRestartsIdleShutdownClock(t *testing.T) {
	s, _, _, cancel := newHoldSeq(t, func(c *Config) {
		c.IdleShutdown = idleShutdownSteps()
		c.ShutdownTimeout = testShutdownTimeout
	})
	defer cancel()
	s.setActivity("inactive", time.Time{})
	runUntil(t, s, func() bool { return phaseIs(s, PhaseStandby) }, time.Second)
	s.setHold(true)
	time.Sleep(2 * testShutdownTimeout)
	s.setHold(false)
	ended := time.Now()
	time.Sleep(time.Until(ended.Add(testShutdownTimeout - 70*time.Millisecond)))
	if !phaseIs(s, PhaseStandby) {
		t.Fatalf("idle shutdown right after the hold ended: phase=%s", mustPhase(s))
	}
	runUntil(t, s, func() bool { return phaseIs(s, PhaseIdle) }, time.Second)
}
