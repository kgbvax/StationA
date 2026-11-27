package seq

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ----- auto_start chain harness ----------------------------------------------

// hookPublisher records like recPublisher and hands every publish to hook
// (the fake bwpc actuator).
type hookPublisher struct {
	recPublisher
	hmu  sync.Mutex
	hook func(topic string, payload []byte)
}

func (h *hookPublisher) Publish(topic string, retained bool, payload []byte) error {
	_ = h.recPublisher.Publish(topic, retained, payload)
	h.hmu.Lock()
	f := h.hook
	h.hmu.Unlock()
	if f != nil {
		f(topic, payload)
	}
	return nil
}

// fakePC answers ensure_program on power/shack-pc/cmd with a /state that
// carries program_<name>=starting and then the reply (after a short delay,
// like a real round trip).
type fakePC struct {
	s      *Sequencer
	mu     sync.Mutex
	fields map[string]any
	reply  map[string]string // program → reply value; "" = stays starting; absent = "ready"
	errs   map[string]string // program → program_<name>_error
}

func (f *fakePC) onPublish(topic string, payload []byte) {
	if topic != abs("power/shack-pc")+"/cmd" {
		return
	}
	var c cmdPayload
	if json.Unmarshal(payload, &c) != nil || c.Action != "ensure_program" {
		return
	}
	f.mu.Lock()
	r, ok := f.reply[c.Value]
	if !ok {
		r = "ready"
	}
	// Like the real actuator: starting first, then the result.
	f.fields["program_"+c.Value] = "starting"
	starting, _ := json.Marshal(f.fields)
	var result []byte
	if r != "" {
		f.fields["program_"+c.Value] = r
		if e := f.errs[c.Value]; e != "" {
			f.fields["program_"+c.Value+"_error"] = e
		}
		result, _ = json.Marshal(f.fields)
	}
	f.mu.Unlock()
	go func() {
		time.Sleep(3 * time.Millisecond)
		f.s.SetState(abs("power/shack-pc"), starting)
		if result != nil {
			time.Sleep(time.Millisecond)
			f.s.SetState(abs("power/shack-pc"), result)
		}
	}()
}

func (f *fakePC) set(program, reply, errText string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply[program] = reply
	if errText != "" {
		f.errs[program] = errText
	}
}

var chainPrograms = []string{"aethersdr", "dax", "wsjtx", "75auto"}

// chainSteps is the example [[auto_start]] list in seq.Step form.
func chainSteps() []Step {
	f := false
	steps := []Step{
		{Name: "bwpc", Kind: KindWOL, MAC: "aa:bb:cc:dd:ee:01", Broadcast: "192.168.1.255:9", Poke: "192.168.1.197:3389"},
		{Name: "bwpc", Kind: KindWaitStatus, Slots: []string{"power/shack-pc"}},
	}
	for _, p := range chainPrograms {
		steps = append(steps,
			Step{Name: p, Kind: KindCmd, Slot: "power/shack-pc", Action: "ensure_program", Value: p, Retain: &f},
			Step{Name: p, Kind: KindWaitState, Slot: "power/shack-pc", Field: "program_" + p, Value: "ready", Fresh: true, FailValue: "failed"})
	}
	return append(steps, Step{Name: "75auto", Kind: KindWaitStatus, Slots: []string{"hf/auto-op"}})
}

const (
	testBeatAge    = 60 * time.Millisecond
	testReadyHold  = 150 * time.Millisecond
	testChainSlack = 2 * time.Second
)

func chainTestConfig(wol *wolRec) Config {
	cfg := standbyTestConfig(wol)
	cfg.AutoStart = chainSteps()
	cfg.AutoOpSlot = "auto-op"
	cfg.PASlot = "pa"
	cfg.AntModeSlot = "ant-mode"
	cfg.AntModeHold = time.Millisecond
	cfg.AutoOpBeatAge = testBeatAge
	cfg.ChainReadyHold = testReadyHold
	return cfg
}

type chainRig struct {
	s   *Sequencer
	pub *hookPublisher
	wol *wolRec
	pc  *fakePC
}

func (r *chainRig) rec() *recPublisher { return &r.pub.recPublisher }

// newChainSeq builds a sequencer with idle standby and an [[auto_start]]
// list; with start it is driven to phase=running first.
func newChainSeq(t *testing.T, start bool, mut func(*Config)) *chainRig {
	t.Helper()
	r := newChainRig(t, mut)
	startRun(t, r.s, context.Background())
	if start {
		r.s.Start()
		runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseRunning) }, time.Second)
	}
	return r
}

// newChainRig builds the sequencer of newChainSeq without starting the runner.
func newChainRig(t *testing.T, mut func(*Config)) *chainRig {
	t.Helper()
	pub := &hookPublisher{}
	wol := &wolRec{}
	cfg := chainTestConfig(wol)
	if mut != nil {
		mut(&cfg)
	}
	s, err := New(cfg, pub, newTestLogger(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pc := &fakePC{s: s, fields: map[string]any{}, reply: map[string]string{}, errs: map[string]string{}}
	pub.hook = pc.onPublish
	s.SetBrokerOnline(true)
	s.markOnline("hf/antenna-select", "uhf/radio", "uhf/el-rotator", "power/shack-pc", "hf/auto-op")
	return &chainRig{s: s, pub: pub, wol: wol, pc: pc}
}

// onPublish adds f to the publish hook (after the fake actuator). f runs on
// the publishing goroutine, mostly the runner.
func (r *chainRig) onPublish(f func(topic string, payload []byte)) {
	r.pub.hmu.Lock()
	defer r.pub.hmu.Unlock()
	prev := r.pub.hook
	r.pub.hook = func(topic string, payload []byte) {
		prev(topic, payload)
		f(topic, payload)
	}
}

// onFirstState calls f once, on the first own /state publish for which match
// returns true.
func (r *chainRig) onFirstState(match func(statePayload) bool, f func()) {
	var once sync.Once
	r.onPublish(func(topic string, payload []byte) {
		if topic != testSelfBase+"/state" {
			return
		}
		var st statePayload
		if json.Unmarshal(payload, &st) == nil && match(st) {
			once.Do(f)
		}
	})
}

// lastChain returns the chain of the last published /state (nil = none).
func lastChain(pub *recPublisher) *chainPayload {
	st := pub.states()
	if len(st) == 0 {
		return nil
	}
	return st[len(st)-1].Chain
}

func chainIs(pub *recPublisher, state string) bool {
	c := lastChain(pub)
	return c != nil && c.State == state
}

func waitChain(t *testing.T, r *chainRig, state string) *chainPayload {
	t.Helper()
	runUntil(t, r.s, func() bool { return chainIs(r.rec(), state) }, testChainSlack)
	return lastChain(r.rec())
}

// ensureCmds lists the ensure_program values sent to the actuator.
func ensureCmds(pub *recPublisher) []string {
	var out []string
	for _, c := range cmdsTo(pub, "power/shack-pc") {
		if c.action == "ensure_program" {
			out = append(out, c.value)
		}
	}
	return out
}

// liveSession publishes a live hf/auto-op /state with session.state and
// beats every 10 ms until the returned stop is called (or the test ends).
func liveSession(t *testing.T, s *Sequencer, state string) (stop func()) {
	t.Helper()
	return session(t, s, state, "live")
}

// session is liveSession with a session.source.
func session(t *testing.T, s *Sequencer, state, source string) (stop func()) {
	t.Helper()
	s.SetAutoOpState([]byte(fmt.Sprintf(`{"session":{"state":%q,"source":%q}}`, state, source)), false)
	done := make(chan struct{})
	var once sync.Once
	stop = func() { once.Do(func() { close(done) }) }
	go func() {
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-tk.C:
				s.AutoOpBeat()
			}
		}
	}()
	t.Cleanup(stop)
	return stop
}

// ----- the chain --------------------------------------------------------------

// From idle: the startup list runs, then the chain; the chain ends ready and
// the station is running.
func TestAutoStartFromIdle(t *testing.T) {
	r := newChainSeq(t, false, nil)
	r.s.AutoStart("tablet-1")
	c := waitChain(t, r, ChainReady)
	if c.Requester != "tablet-1" || c.Fault != "" {
		t.Fatalf("chain = %+v", c)
	}
	if !phaseIs(r.s, PhaseRunning) {
		t.Fatalf("phase = %s, want running", mustPhase(r.s))
	}
	if got := cmdsTo(r.rec(), "power/master"); len(got) != 1 {
		t.Fatalf("startup cmds = %+v, want the startup list once", got)
	}
	if got := ensureCmds(r.rec()); strings.Join(got, ",") != strings.Join(chainPrograms, ",") {
		t.Fatalf("ensure_program order = %v", got)
	}
	for _, cmd := range cmdsTo(r.rec(), "power/shack-pc") {
		if cmd.retained {
			t.Fatalf("actuator cmd retained: %+v", cmd)
		}
	}
	if r.wol.n() != 1 {
		t.Fatalf("wol = %d, want 1 (the bwpc step)", r.wol.n())
	}
	// The station step came first: the startup ran under chain step "station".
	sawStation := false
	for _, st := range r.rec().states() {
		if st.Chain != nil && st.Chain.Step == "station" && st.Phase == PhaseStarting {
			sawStation = true
		}
	}
	if !sawStation {
		t.Fatal("no /state with chain.step=station during the startup")
	}
}

func TestAutoStartFromStandbyWakesFirst(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.s.setActivity("inactive", time.Time{})
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseStandby) }, time.Second)
	r.s.AutoStart("tablet-1")
	waitChain(t, r, ChainReady)
	if !phaseIs(r.s, PhaseRunning) {
		t.Fatalf("phase = %s", mustPhase(r.s))
	}
	if r.wol.n() != 2 { // wake list + bwpc step
		t.Fatalf("wol = %d, want 2", r.wol.n())
	}
	ant := cmdsTo(r.rec(), "power/antennas")
	if len(ant) != 2 || ant[1].value != "on" {
		t.Fatalf("antennas cmds = %+v, want the wake list's on", ant)
	}
	if got := cmdsTo(r.rec(), "power/master"); len(got) != 1 {
		t.Fatalf("startup ran again: %+v", got)
	}
}

func TestAutoStartInRunningRunsChainOnly(t *testing.T) {
	r := newChainSeq(t, true, nil)
	before := len(r.rec().states())
	r.s.AutoStart("tablet-1")
	waitChain(t, r, ChainReady)
	if got := cmdsTo(r.rec(), "power/master"); len(got) != 1 {
		t.Fatalf("startup ran again: %+v", got)
	}
	for _, st := range r.rec().states()[before:] {
		if st.Phase != PhaseRunning {
			t.Fatalf("phase left running during the chain: %+v", st)
		}
	}
}

func TestAutoStartRefusedInTransitionalPhase(t *testing.T) {
	r := newChainSeq(t, false, func(c *Config) {
		c.Startup = []Step{{Name: "wait-never", Kind: KindWaitStatus, Slots: []string{"hf/never"}, TimeoutS: intPtr(5)}}
	})
	r.s.Start()
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseStarting) }, time.Second)
	r.s.AutoStart("tablet-1")
	c := waitChain(t, r, ChainFailed)
	if c.Fault != "busy: starting" || c.Requester != "tablet-1" {
		t.Fatalf("chain = %+v, want failed busy: starting", c)
	}
	r.s.Stop() // ends nothing: stop is refused while starting, the wait times out later
	time.Sleep(20 * time.Millisecond)
	if n := len(ensureCmds(r.rec())); n != 0 || r.wol.n() != 0 {
		t.Fatalf("chain ran: ensure=%d wol=%d", n, r.wol.n())
	}
}

func TestAutoStartWakesOfflinePC(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.s.markOffline("power/shack-pc")
	r.s.AutoStart("tablet-1")
	runUntil(t, r.s, func() bool {
		r.wol.mu.Lock()
		defer r.wol.mu.Unlock()
		return len(r.wol.pokes) == 1
	}, time.Second)
	if c := lastChain(r.rec()); c == nil || c.State != ChainRunning || c.Step != "bwpc" {
		t.Fatalf("chain = %+v, want running at bwpc", c)
	}
	r.s.markOnline("power/shack-pc")
	waitChain(t, r, ChainReady)
}

func TestAutoStartRefusedWhileSessionActive(t *testing.T) {
	r := newChainSeq(t, true, nil)
	liveSession(t, r.s, "running")
	r.s.AutoStart("tablet-1")
	c := waitChain(t, r, ChainFailed)
	if c.Fault != "busy: session active" {
		t.Fatalf("chain = %+v", c)
	}
	if n := len(ensureCmds(r.rec())); n != 0 {
		t.Fatalf("chain ran: %d ensure_program", n)
	}
}

// A retained program_dax=ready from the last chain does not pass a fresh wait.
func TestFreshWaitIgnoresOldState(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.s.SetState(abs("power/shack-pc"), []byte(`{"program_dax":"ready"}`))
	r.pc.set("dax", "", "")
	r.s.AutoStart("tablet-1")
	c := waitChain(t, r, ChainFailed)
	if c.Step != "dax" || !strings.Contains(c.Fault, "timeout") {
		t.Fatalf("chain = %+v, want dax timeout", c)
	}
}

func TestFailValueFailsAtOnceWithReason(t *testing.T) {
	r := newChainSeq(t, true, func(c *Config) { c.StepTimeout = 5 * time.Second })
	r.pc.set("aethersdr", "failed", "not started by the chain")
	t0 := time.Now()
	r.s.AutoStart("tablet-1")
	c := waitChain(t, r, ChainFailed)
	if time.Since(t0) > time.Second {
		t.Fatalf("fail_value waited for the timeout (%s)", time.Since(t0))
	}
	if c.Step != "aethersdr" || !strings.Contains(c.Fault, "not started by the chain") {
		t.Fatalf("chain = %+v", c)
	}
	if got := ensureCmds(r.rec()); len(got) != 1 {
		t.Fatalf("later programs started: %v", got)
	}
	if !phaseIs(r.s, PhaseRunning) {
		t.Fatalf("phase = %s, want running", mustPhase(r.s))
	}
}

// A wait_state timeout carries the <field>_error sibling.
func TestWaitStateTimeoutCarriesErrorSibling(t *testing.T) {
	s, pub := newTestSeqWith(t,
		[]Step{{Name: "wait-x", Kind: KindWaitState, Slot: "power/shack-pc", Field: "program_dax", Value: "ready"}},
		exampleShutdown())
	s.markOnline("power/shack-pc")
	s.SetState(abs("power/shack-pc"), []byte(`{"program_dax":"starting","program_dax_error":"no window"}`))
	startRun(t, s, context.Background())
	s.Start()
	runUntil(t, s, func() bool { _, _, f := s.Phase(); return f != "" }, time.Second)
	_, _, fault := s.Phase()
	if !strings.Contains(fault, "program_dax_error=no window") {
		t.Fatalf("fault = %q", fault)
	}
	_ = pub
}

// A chain timeout in running leaves the phase running; standby still follows.
func TestChainTimeoutKeepsRunningAndStandby(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.pc.set("aethersdr", "", "")
	r.s.setActivity("inactive", time.Time{})
	r.s.AutoStart("tablet-1")
	c := waitChain(t, r, ChainFailed)
	if c.Step != "aethersdr" || !strings.Contains(c.Fault, "timeout") {
		t.Fatalf("chain = %+v", c)
	}
	if ph := mustPhase(r.s); ph != PhaseRunning {
		t.Fatalf("phase = %s, want running", ph)
	}
	if n := len(ensureCmds(r.rec())); n != 1 {
		t.Fatalf("later programs started after the failure: %d", n)
	}
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseStandby) }, time.Second)
}

func TestAutoStartRefusedInStorm(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.s.SetAntMode([]byte(`{"ant_mode":"storm","lock":{"seq":1,"ground":true}}`))
	r.s.AutoStart("tablet-1")
	c := waitChain(t, r, ChainFailed)
	if c.Fault != "antenna mode storm" {
		t.Fatalf("chain = %+v", c)
	}
	r.s.SetAntMode([]byte(`{"ant_mode":"maintenance","lock":{"seq":2}}`))
	r.s.AutoStart("tablet-1")
	runUntil(t, r.s, func() bool {
		c := lastChain(r.rec())
		return c != nil && c.Fault == "antenna mode maintenance"
	}, time.Second)
	if n := len(ensureCmds(r.rec())); n != 0 || r.wol.n() != 0 {
		t.Fatalf("chain ran: ensure=%d wol=%d", n, r.wol.n())
	}
}

// The station stop during a chain: the chain ends failed before the shutdown
// runs, and no ready follows.
func TestStopDuringChain(t *testing.T) {
	r := newChainSeq(t, true, func(c *Config) { c.StepTimeout = 5 * time.Second })
	r.pc.set("dax", "", "")
	r.s.AutoStart("tablet-1")
	runUntil(t, r.s, func() bool {
		c := lastChain(r.rec())
		return c != nil && c.Step == "dax"
	}, time.Second)
	r.s.Stop()
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseIdle) }, 2*time.Second)

	failedAt, firstShutdown := -1, -1
	r.rec().mu.Lock()
	for i, m := range r.rec().msgs {
		if m.Topic == testSelfBase+"/state" && failedAt < 0 {
			var st statePayload
			if json.Unmarshal(m.Payload, &st) == nil && st.Chain != nil && st.Chain.State == ChainFailed {
				failedAt = i
				if st.Chain.Fault != "stopped by the operator" {
					t.Errorf("fault = %q", st.Chain.Fault)
				}
			}
		}
		if m.Topic == abs("hf/pa-arm")+"/cmd" && firstShutdown < 0 {
			firstShutdown = i
		}
	}
	r.rec().mu.Unlock()
	if failedAt < 0 || firstShutdown < 0 || failedAt > firstShutdown {
		t.Fatalf("chain failed at msg %d, first shutdown cmd at %d", failedAt, firstShutdown)
	}
	for _, st := range r.rec().states() {
		if st.Chain != nil && st.Chain.State == ChainReady {
			t.Fatal("chain.state=ready after a stop")
		}
	}
}

func TestTwoAutoStartsSecondRefused(t *testing.T) {
	r := newChainSeq(t, true, func(c *Config) { c.StepTimeout = 5 * time.Second })
	r.pc.set("aethersdr", "", "")
	r.s.AutoStart("first")
	r.s.AutoStart("second")
	runUntil(t, r.s, func() bool {
		c := lastChain(r.rec())
		return c != nil && c.Step == "aethersdr"
	}, time.Second)
	time.Sleep(30 * time.Millisecond)
	for _, st := range r.rec().states() {
		if st.Chain != nil && (st.Chain.Requester != "first" || st.Chain.State != ChainRunning) {
			t.Fatalf("chain changed by the second auto_start: %+v", st.Chain)
		}
	}
	// The first chain ends; the second was not queued behind it.
	r.s.SetState(abs("power/shack-pc"), []byte(`{"program_aethersdr":"ready"}`))
	waitChain(t, r, ChainReady)
	time.Sleep(50 * time.Millisecond)
	if got := ensureCmds(r.rec()); len(got) != len(chainPrograms) {
		t.Fatalf("ensure_program = %v, want one chain", got)
	}
	for _, st := range r.rec().states() {
		if st.Chain != nil && st.Chain.Requester != "first" {
			t.Fatalf("second chain ran: %+v", st.Chain)
		}
	}
}

// ----- session hold ----------------------------------------------------------

// Idle while a chain runs, and within the ready window: no standby.
func TestChainHoldsStandby(t *testing.T) {
	r := newChainSeq(t, true, func(c *Config) { c.StepTimeout = 5 * time.Second })
	r.s.setActivity("inactive", time.Time{})
	r.pc.set("dax", "", "")
	r.s.AutoStart("tablet-1")
	time.Sleep(3 * testIdleTimeout)
	if !phaseIs(r.s, PhaseRunning) || !chainIs(r.rec(), ChainRunning) {
		t.Fatalf("standby under a running chain: phase=%s", mustPhase(r.s))
	}
	// Let it pass: the program comes up, the chain turns ready.
	r.s.SetState(abs("power/shack-pc"), []byte(`{"program_dax":"ready"}`))
	waitChain(t, r, ChainReady)
	readyAt := time.Now()
	time.Sleep(testReadyHold - 40*time.Millisecond)
	if !phaseIs(r.s, PhaseRunning) {
		t.Fatalf("standby inside the ready window: phase=%s", mustPhase(r.s))
	}
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseStandby) }, time.Second)
	if d := time.Since(readyAt); d < testReadyHold+testIdleTimeout-20*time.Millisecond {
		t.Fatalf("standby %s after ready, want the window plus a full idle timeout", d)
	}
}

// A session that ends: standby waits a full timeout from the session end.
func TestSessionHoldThenFullTimeout(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.s.setActivity("inactive", time.Time{})
	stop := liveSession(t, r.s, "running")
	time.Sleep(3 * testIdleTimeout)
	if !phaseIs(r.s, PhaseRunning) {
		t.Fatalf("standby during a live session: phase=%s", mustPhase(r.s))
	}
	r.s.SetAutoOpState([]byte(`{"session":{"state":"ended"}}`), false)
	stop()
	ended := time.Now()
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseStandby) }, time.Second)
	if d := time.Since(ended); d < testIdleTimeout-10*time.Millisecond {
		t.Fatalf("standby %s after the session end, want >= %s", d, testIdleTimeout)
	}
}

// A session with fresh beats holds the standby and so the idle shutdown; its
// end restarts the idle-shutdown clock.
func TestSessionHoldRestartsIdleShutdownClock(t *testing.T) {
	r := newChainSeq(t, true, func(c *Config) {
		c.IdleShutdown = idleShutdownSteps()
		c.ShutdownTimeout = testShutdownTimeout
	})
	r.s.setActivity("inactive", time.Time{})
	stop := liveSession(t, r.s, "running")
	time.Sleep(2 * testShutdownTimeout)
	if !phaseIs(r.s, PhaseRunning) {
		t.Fatalf("standby or idle shutdown during a live session: phase=%s", mustPhase(r.s))
	}
	r.s.SetAutoOpState([]byte(`{"session":{"state":"ended"}}`), false)
	stop()
	ended := time.Now()
	time.Sleep(time.Until(ended.Add(testShutdownTimeout - 70*time.Millisecond)))
	if phaseIs(r.s, PhaseIdle) {
		t.Fatalf("idle shutdown right after the session ended: phase=%s", mustPhase(r.s))
	}
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseIdle) }, time.Second)
}

// A running session whose beats stopped (75auto crashed) does not hold.
func TestStaleBeatEndsHold(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.s.setActivity("inactive", time.Time{})
	stop := liveSession(t, r.s, "running")
	time.Sleep(2 * testIdleTimeout)
	stop() // session.state stays running, beats stop
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseStandby) }, time.Second)
}

// ----- PA standby on the session start edge ----------------------------------

func paCmds(pub *recPublisher) []cmdTuple { return cmdsTo(pub, "hf/pa") }

func TestPAStandbyOnSessionStartEdge(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.s.SetAutoOpState([]byte(`{"session":{"state":"not-started"}}`), false)
	r.s.SetAutoOpState([]byte(`{"session":{"state":"connecting","source":"live"}}`), false)
	runUntil(t, r.s, func() bool { return len(paCmds(r.rec())) == 1 }, time.Second)
	c := paCmds(r.rec())[0]
	if c.action != "set_mode" || c.value != "standby" || c.retained {
		t.Fatalf("pa cmd = %+v, want one not retained set_mode standby", c)
	}
	// The same session goes on (the operator puts the PA in operate): no
	// second standby.
	r.s.SetAutoOpState([]byte(`{"session":{"state":"running","source":"live"}}`), false)
	r.s.SetAutoOpState([]byte(`{"session":{"state":"running","source":"live"}}`), false)
	time.Sleep(30 * time.Millisecond)
	if n := len(paCmds(r.rec())); n != 1 {
		t.Fatalf("pa cmds = %d during one session, want 1", n)
	}
	// A new session after an ended one: a new standby.
	r.s.SetAutoOpState([]byte(`{"session":{"state":"ended"}}`), false)
	r.s.SetAutoOpState([]byte(`{"session":{"state":"connecting","source":"live"}}`), false)
	runUntil(t, r.s, func() bool { return len(paCmds(r.rec())) == 2 }, time.Second)
}

// powerseq restarts during a session: the retained /state only re-baselines.
func TestPAStandbyNotSentOnRetainedState(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.s.SetAutoOpState([]byte(`{"session":{"state":"running","source":"live"}}`), true)
	r.s.SetAutoOpState([]byte(`{"session":{"state":"running","source":"live"}}`), false)
	time.Sleep(30 * time.Millisecond)
	if n := len(paCmds(r.rec())); n != 0 {
		t.Fatalf("pa cmds = %d after a restart during a session, want 0", n)
	}
}

// ----- persistence -----------------------------------------------------------

func TestRestartDuringChainRestoresFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	wol := &wolRec{}
	cfg := chainTestConfig(wol)
	cfg.StateFile = path
	cfg.StepTimeout = 5 * time.Second
	pub := &hookPublisher{}
	s, err := New(cfg, pub, newTestLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	s.SetBrokerOnline(true)
	s.markOnline("hf/antenna-select", "power/shack-pc", "hf/auto-op")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	s.Start()
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
	s.AutoStart("tablet-1") // no actuator reply: the chain waits at aethersdr
	runUntil(t, s, func() bool { c := lastChain(&pub.recPublisher); return c != nil && c.Step == "aethersdr" }, time.Second)
	// A kill: the file keeps the running chain (no final flush of a failure).
	saved := readSaved(t, path)
	if saved.Chain == nil || saved.Chain.State != ChainRunning {
		cancel()
		<-done
		t.Fatalf("saved chain = %+v, want running", saved.Chain)
	}
	cancel()
	<-done
	if err := writeStateFile(path, saved); err != nil {
		t.Fatal(err)
	}

	pub2 := &recPublisher{}
	s2, err := New(cfg, pub2, newTestLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	s2.SetBrokerOnline(true)
	startRun(t, s2, context.Background())
	runUntil(t, s2, func() bool { return chainIs(pub2, ChainFailed) }, time.Second)
	c := lastChain(pub2)
	if c.Fault != "interrupted by a powerseq restart" || c.Requester != "tablet-1" {
		t.Fatalf("restored chain = %+v", c)
	}
	if !phaseIs(s2, PhaseRunning) {
		t.Fatalf("phase = %s", mustPhase(s2))
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(pub2.cmds()); n != 0 {
		t.Fatalf("restart sent %d /cmd", n)
	}
}

// ----- stop and the chain ----------------------------------------------------

// A stop right after the chain record turns running, before its first step:
// the chain fails "stopped by the operator" and sends no chain step.
func TestStopRightAfterChainRunning(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.onFirstState(func(st statePayload) bool {
		return st.Chain != nil && st.Chain.State == ChainRunning
	}, r.s.Stop)
	r.s.AutoStart("tablet-1")
	c := waitChain(t, r, ChainFailed)
	if c.Fault != "stopped by the operator" {
		t.Fatalf("chain = %+v, want failed stopped by the operator", c)
	}
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseIdle) }, 2*time.Second)
	if n := len(ensureCmds(r.rec())); n != 0 || r.wol.n() != 0 {
		t.Fatalf("chain steps sent after the stop: ensure=%d wol=%d", n, r.wol.n())
	}
}

// A stop while the startup of an auto_start from idle runs: the chain fails
// "stopped by the operator" and the shutdown runs.
func TestStopDuringAutoStartStartup(t *testing.T) {
	r := newChainSeq(t, false, func(c *Config) {
		c.Startup = []Step{{Name: "wait-never", Kind: KindWaitStatus, Slots: []string{"hf/never"}, TimeoutS: intPtr(5)}}
	})
	r.s.AutoStart("tablet-1")
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseStarting) && chainIs(r.rec(), ChainRunning) }, time.Second)
	r.s.Stop()
	c := waitChain(t, r, ChainFailed)
	if c.Fault != "stopped by the operator" {
		t.Fatalf("chain = %+v", c)
	}
	runUntil(t, r.s, func() bool { return len(cmdsTo(r.rec(), "hf/pa-arm")) > 0 && phaseIs(r.s, PhaseIdle) }, 2*time.Second)
	if n := len(ensureCmds(r.rec())); n != 0 || r.wol.n() != 0 {
		t.Fatalf("chain ran: ensure=%d wol=%d", n, r.wol.n())
	}
}

// AutoStart then Stop before the runner takes the auto_start: nothing of the
// chain is sent, and the next auto_start is accepted.
func TestStopBeforeQueuedAutoStart(t *testing.T) {
	r := newChainRig(t, nil)
	r.s.AutoStart("tablet-1")
	r.s.Stop()
	startRun(t, r.s, context.Background())
	c := waitChain(t, r, ChainFailed)
	if c.Fault != "stopped by the operator" {
		t.Fatalf("chain = %+v", c)
	}
	runUntil(t, r.s, func() bool { return len(cmdsTo(r.rec(), "hf/pa-arm")) > 0 && phaseIs(r.s, PhaseIdle) }, 2*time.Second)
	for _, c := range cmdsTo(r.rec(), "power/master") {
		if c.value == "on" {
			t.Fatalf("startup ran: %+v", c)
		}
	}
	if n := len(ensureCmds(r.rec())); n != 0 || r.wol.n() != 0 {
		t.Fatalf("chain ran: ensure=%d wol=%d", n, r.wol.n())
	}
	r.s.AutoStart("tablet-1")
	waitChain(t, r, ChainReady)
}

// A stop right after ready: the chain turns failed "stopped by the operator"
// (a waiting console must not start a session on a stopped station), and the
// next auto_start is accepted, not refused with a stale stop.
func TestStopRightAfterReadyThenAutoStart(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.onFirstState(func(st statePayload) bool {
		return st.Chain != nil && st.Chain.State == ChainReady
	}, r.s.Stop)
	r.s.AutoStart("tablet-1")
	runUntil(t, r.s, func() bool {
		c := lastChain(r.rec())
		return phaseIs(r.s, PhaseIdle) && c != nil && c.State == ChainFailed
	}, 2*time.Second)
	if c := lastChain(r.rec()); c.Fault != "stopped by the operator" {
		t.Fatalf("chain = %+v", c)
	}
	r.s.AutoStart("tablet-2")
	c := waitChain(t, r, ChainReady)
	if c.Requester != "tablet-2" {
		t.Fatalf("chain = %+v", c)
	}
}

// A station stop while the chain is ready turns it failed.
func TestStopAfterReadyFailsChain(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.s.AutoStart("tablet-1")
	waitChain(t, r, ChainReady)
	r.s.Stop()
	c := waitChain(t, r, ChainFailed)
	if c.Fault != "stopped by the operator" || c.Requester != "tablet-1" {
		t.Fatalf("chain = %+v", c)
	}
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseIdle) }, 2*time.Second)
}

// A startup step that fails inside an auto_start from idle: the chain fails
// with the startup fault, the station is idle with the fault, no chain step
// is sent and the session hold ends.
func TestAutoStartStartupFailure(t *testing.T) {
	r := newChainSeq(t, false, func(c *Config) {
		c.Startup = []Step{{Name: "wait-never", Kind: KindWaitStatus, Slots: []string{"hf/never"}}}
		c.StepTimeout = 50 * time.Millisecond
	})
	r.s.AutoStart("tablet-1")
	c := waitChain(t, r, ChainFailed)
	if !strings.HasPrefix(c.Fault, "wait-never: timeout") || c.Step != "station" {
		t.Fatalf("chain = %+v, want the startup fault at step station", c)
	}
	ph, _, fault := r.s.Phase()
	if ph != PhaseIdle || !strings.HasPrefix(fault, "wait-never: timeout") {
		t.Fatalf("phase=%s fault=%q, want idle + the startup fault", ph, fault)
	}
	if n := len(ensureCmds(r.rec())); n != 0 || r.wol.n() != 0 {
		t.Fatalf("chain ran: ensure=%d wol=%d", n, r.wol.n())
	}
	r.s.mu.Lock()
	held := r.s.sessionHoldLocked(time.Now())
	r.s.mu.Unlock()
	if held {
		t.Fatal("session hold still active after the failed chain")
	}
}

// ----- sessions in standby and replay ----------------------------------------

// A live session in standby is activity: it wakes the station.
func TestLiveSessionInStandbyWakes(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.s.setActivity("inactive", time.Time{})
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseStandby) }, time.Second)
	liveSession(t, r.s, "running")
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseRunning) }, time.Second)
	// The session then holds the standby.
	time.Sleep(3 * testIdleTimeout)
	if !phaseIs(r.s, PhaseRunning) {
		t.Fatalf("standby during the live session: phase=%s", mustPhase(r.s))
	}
}

// A replay session is not a station session: no hold, no PA standby, no
// busy refusal.
func TestReplaySessionNoHold(t *testing.T) {
	r := newChainSeq(t, true, nil)
	r.s.setActivity("inactive", time.Time{})
	session(t, r.s, "connecting", "replay")
	session(t, r.s, "running", "replay")
	runUntil(t, r.s, func() bool { return phaseIs(r.s, PhaseStandby) }, time.Second)
	if n := len(paCmds(r.rec())); n != 0 {
		t.Fatalf("pa cmds = %d for a replay session, want 0", n)
	}
	r.s.mu.Lock()
	reason := r.s.autoStartRefusalLocked(time.Now())
	r.s.mu.Unlock()
	if reason != "" {
		t.Fatalf("auto_start refused during a replay: %q", reason)
	}
}

// ----- fresh wait_state ------------------------------------------------------

// A fresh wait needs a non-terminal value first: an old ready queued before
// the ensure does not pass; starting, then ready does.
func TestFreshWaitNeedsNonTerminalFirst(t *testing.T) {
	f := false
	startup := []Step{
		{Name: "dax", Kind: KindCmd, Slot: "power/shack-pc", Action: "ensure_program", Value: "dax", Retain: &f},
		{Name: "dax", Kind: KindWaitState, Slot: "power/shack-pc", Field: "program_dax", Value: "ready", Fresh: true, FailValue: "failed"},
	}
	cfg := baseConfig(startup, exampleShutdown())
	cfg.StepTimeout = 2 * time.Second
	pub := &recPublisher{}
	s, err := New(cfg, pub, newTestLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	s.SetBrokerOnline(true)
	s.markOnline("power/shack-pc")
	startRun(t, s, context.Background())
	s.Start()
	runUntil(t, s, func() bool { _, st, _ := s.Phase(); return len(cmdsTo(pub, "power/shack-pc")) == 1 && st == "dax" }, time.Second)
	s.setState("power/shack-pc", "program_dax", "ready") // the old tick
	time.Sleep(30 * time.Millisecond)
	if !phaseIs(s, PhaseStarting) {
		t.Fatalf("old ready passed the fresh wait: phase=%s", mustPhase(s))
	}
	s.setState("power/shack-pc", "program_dax", "starting")
	s.setState("power/shack-pc", "program_dax", "ready")
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
}

// The actuator's starting can arrive before the wait step begins (between the
// ensure and the wait): it still counts.
func TestFreshWaitCountsStartingBeforeWaitStep(t *testing.T) {
	f := false
	startup := []Step{
		{Name: "dax", Kind: KindCmd, Slot: "power/shack-pc", Action: "ensure_program", Value: "dax", Retain: &f},
		{Name: "dax", Kind: KindWaitState, Slot: "power/shack-pc", Field: "program_dax", Value: "ready", Fresh: true, FailValue: "failed"},
	}
	cfg := baseConfig(startup, exampleShutdown())
	cfg.StepTimeout = 2 * time.Second
	pub := &hookPublisher{}
	s, err := New(cfg, pub, newTestLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	pub.hook = func(topic string, _ []byte) {
		if topic == abs("power/shack-pc")+"/cmd" {
			s.setState("power/shack-pc", "program_dax", "starting") // inside the cmd step
		}
	}
	s.SetBrokerOnline(true)
	s.markOnline("power/shack-pc")
	startRun(t, s, context.Background())
	s.Start()
	runUntil(t, s, func() bool { _, st, _ := s.Phase(); return len(pub.cmds()) > 0 && st == "dax" }, time.Second)
	time.Sleep(20 * time.Millisecond)
	s.setState("power/shack-pc", "program_dax", "ready")
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
}
