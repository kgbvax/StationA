package control

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"pelcobridge2/internal/pelco"
	"pelcobridge2/internal/serialio"
	"pelcobridge2/internal/simhead"
)

// The engine is exercised against the simulated head with every window
// shortened (testCfg): same frames, same quirks, seconds instead of bench
// minutes. Every assertion that depends on motion or ladders polls with a
// deadline instead of sleeping fixed amounts.

// testCfg shortens every window so tests run in seconds, not bench minutes.
func testCfg() Config {
	return Config{
		Addr: 1, Baud: 2400, JogSpeed: pelco.DefaultJogSpeed,
		Settle: 100 * time.Millisecond, SetTolerance: 0.5,
		ArmMaxReadbackAge: 2 * time.Second, ReplyWait: 600 * time.Millisecond,
	}
}

// blackhole swallows writes and never answers — pins the one-outstanding-query
// and reply-wait behaviour without a device.
type blackhole struct {
	mu     sync.Mutex
	writes [][]byte
	closed chan struct{}
}

func (b *blackhole) Read(p []byte) (int, error) {
	<-b.closed
	return 0, errors.New("transport closed")
}
func (b *blackhole) Write(w []byte) error {
	b.mu.Lock()
	b.writes = append(b.writes, append([]byte(nil), w...))
	b.mu.Unlock()
	return nil
}
func (b *blackhole) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func (b *blackhole) writeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.writes)
}

func (b *blackhole) lastWrite() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.writes) == 0 {
		return nil
	}
	return b.writes[len(b.writes)-1]
}

// harness is one engine wired to one simulated head, with an event pump
// collecting snapshots and log lines for assertions.
type harness struct {
	t   *testing.T
	eng *Engine
	tr  *simhead.Head
	req chan Request

	mu    sync.Mutex
	snaps []*Snapshot
	logs  []string
}

func startHarness(t *testing.T, head *simhead.Head, cfg Config) *harness {
	t.Helper()
	return startHarnessTr(t, head, head, cfg)
}

// startHarnessTr wires the engine to an arbitrary transport (a blackhole, a
// write-failing wrapper) while keeping the simulated head for motion asserts.
func startHarnessTr(t *testing.T, head *simhead.Head, tr serialio.Transport, cfg Config) *harness {
	t.Helper()
	h := &harness{t: t, tr: head, req: make(chan Request, 32)}
	evCh := make(chan Event, 256)
	h.eng = New(cfg, tr, nil, h.req, evCh)
	ctx, cancel := context.WithCancel(context.Background())
	go h.eng.Run(ctx)
	t.Cleanup(func() {
		cancel()
		time.Sleep(50 * time.Millisecond) // let the shutdown all-stop land
	})

	go func() {
		for ev := range evCh {
			h.mu.Lock()
			if ev.Snap != nil {
				h.snaps = append(h.snaps, ev.Snap)
			}
			if ev.Log != "" {
				h.logs = append(h.logs, ev.Log)
			}
			h.mu.Unlock()
		}
	}()
	return h
}

func (h *harness) call(from Source, it Intent) Result {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return h.eng.Call(ctx, from, it)
}

// callNoBusy retries ErrBusy: a preset frame must not break a frame-gap or
// reply window, so the engine answers "retry" — tests do exactly that.
func (h *harness) callNoBusy(from Source, it Intent) Result {
	deadline := time.Now().Add(2 * time.Second)
	r := h.call(from, it)
	for r.Err == ErrBusy && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		r = h.call(from, it)
	}
	return r
}

// arm queries pan once, then arms with the given true azimuth.
func (h *harness) arm(trueAz float64) {
	if r := h.call(SrcTUI, QueryPanIntent{}); r.Err != nil {
		h.t.Fatalf("arm precondition (query): %v", r.Err)
	}
	if r := h.call(SrcTUI, ArmIntent{TrueAz: trueAz}); r.Err != nil {
		h.t.Fatalf("arm: %v", r.Err)
	}
}

// gotoAzEl teleports the simulated head so the ENGINE's user frame reads the
// requested TRUE az/el: the pan target crosses the arm offset
// (physical = Norm360(true + offset), exactly where a SetPanIntent would
// drive the head) and the elevation mirrors into the native tilt word. A
// test-setup primitive — no wire traffic, no ladder. The offset applied is
// whatever the engine currently holds (0 while disarmed).
func (h *harness) gotoAzEl(trueAz, el float64) {
	h.tr.SetAzEl(pelco.Norm360(trueAz+h.eng.offset), el)
}

func (h *harness) lastSnap() *Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.snaps) == 0 {
		return nil
	}
	return h.snaps[len(h.snaps)-1]
}

func (h *harness) waitFor(d time.Duration, cond func(*Snapshot) bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s := h.lastSnap(); s != nil && cond(s) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// logDump renders the collected log lines for failure diagnostics.
func (h *harness) logDump() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.logs, "\n")
}

// --- startup -------------------------------------------------------------------

// No polling means nothing else guarantees a snapshot: the engine must publish
// one BEFORE the loop, or the TUI sits on "waiting for engine…" forever.
func TestInitialSnapshotBeforeLoop(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1}), testCfg())

	if !h.waitFor(2*time.Second, func(s *Snapshot) bool { return s != nil }) {
		t.Fatal("no snapshot ever published")
	}
	s := h.lastSnap()
	if s.Armed || s.DeviceOnline {
		t.Fatalf("initial snapshot not pristine: armed=%v online=%v", s.Armed, s.DeviceOnline)
	}
}

// --- queries ------------------------------------------------------------------

func TestQueryPanReturnsTextbookDegrees(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 123.45}), testCfg())

	r := h.call(SrcTUI, QueryPanIntent{})
	if r.Err != nil {
		t.Fatalf("query pan: %v", r.Err)
	}
	if r.Deg != 123.45 {
		t.Fatalf("query pan = %.2f, want 123.45 (textbook decode)", r.Deg)
	}
}

// The tilt scale is inverted (bench 2026-08-30): the head's native tilt word
// mirrors elevation, so a query must answer in TRUE elevation, not the word.
func TestQueryTiltMirrorsInvertedScale(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, TiltDeg: 45.5}), testCfg())

	r := h.call(SrcTUI, QueryTiltIntent{})
	if r.Err != nil {
		t.Fatalf("query tilt: %v", r.Err)
	}
	if r.Deg != 44.5 { // el = 90 − native 45.5
		t.Fatalf("query tilt = %.2f, want 44.5 (el = 90 − native 45.5)", r.Deg)
	}
}

// --- arming -------------------------------------------------------------------

func TestDisarmedRefusals(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1}), testCfg())

	// Absolute sets stay arm-gated for EVERY source.
	for _, it := range []Intent{
		SetPanIntent{Deg: 100},
		SetTiltIntent{Deg: 30},
	} {
		if r := h.call(SrcTUI, it); r.Err != ErrDisarmed {
			t.Fatalf("%T disarmed: %v, want ErrDisarmed", it, r.Err)
		}
	}
	// TUI manual motion (jog, goto 0, goto az/el) is NOT gated by arming —
	// the operator positions the head with it BEFORE arming. A jog never
	// self-stops, so it gets its own line and an all-stop before the gotos:
	// execSet refuses under an active jog (ErrMoving, below).
	if r := h.call(SrcTUI, JogIntent{Dir: DirUp}); r.Err != nil {
		t.Fatalf("%T from TUI disarmed: %v, want allowed", JogIntent{Dir: DirUp}, r.Err)
	}
	for _, it := range []Intent{
		GotoPhysZeroIntent{},
		GotoAzElIntent{Az: 200, HasAz: true},
	} {
		if r := h.call(SrcTUI, it); r.Err != ErrMoving {
			t.Fatalf("%T under active jog: %v, want ErrMoving", it, r.Err)
		}
	}
	if r := h.call(SrcTUI, StopIntent{}); r.Err != nil {
		t.Fatalf("stop: %v", r.Err)
	}
	for _, it := range []Intent{
		GotoPhysZeroIntent{},
		GotoAzElIntent{Az: 200, HasAz: true},
	} {
		if r := h.call(SrcTUI, it); r.Err != nil {
			t.Fatalf("%T from TUI disarmed: %v, want allowed", it, r.Err)
		}
	}
	// Non-TUI goto az/el is still arm-gated.
	if r := h.call(SrcRotctld, GotoAzElIntent{Az: 200, HasAz: true}); r.Err != ErrDisarmed {
		t.Fatalf("GotoAzElIntent from rotctld disarmed: %v, want ErrDisarmed", r.Err)
	}
	// A goto that selects no axis, or a non-finite target, is refused outright.
	if r := h.call(SrcTUI, GotoAzElIntent{}); r.Err == nil {
		t.Fatal("goto with no axis was accepted")
	}
	if r := h.call(SrcTUI, GotoAzElIntent{Az: math.NaN(), HasAz: true}); r.Err == nil {
		t.Fatal("goto with NaN azimuth was accepted")
	}
	// Non-TUI jog is still arm-gated.
	if r := h.call(SrcRotctld, JogIntent{Dir: DirUp}); r.Err != ErrDisarmed {
		t.Fatalf("rotctld jog disarmed: %v, want ErrDisarmed", r.Err)
	}
	// Stop is always allowed, disarmed or not.
	if r := h.call(SrcTUI, StopIntent{}); r.Err != nil {
		t.Fatalf("stop disarmed: %v", r.Err)
	}
}

func TestArmRequiresFreshReadback(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 10}), testCfg())

	if r := h.call(SrcTUI, ArmIntent{TrueAz: 0}); r.Err != ErrStale {
		t.Fatalf("arm without readback: %v, want ErrStale", r.Err)
	}

	h.call(SrcTUI, QueryPanIntent{})
	if r := h.call(SrcTUI, ArmIntent{TrueAz: 0}); r.Err != nil {
		t.Fatalf("arm after query: %v", r.Err)
	}
}

func TestArmStaleReadbackRefused(t *testing.T) {
	cfg := testCfg()
	cfg.ArmMaxReadbackAge = 50 * time.Millisecond
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 10}), cfg)

	h.call(SrcTUI, QueryPanIntent{})
	time.Sleep(150 * time.Millisecond) // readback ages past the window
	if r := h.call(SrcTUI, ArmIntent{TrueAz: 0}); r.Err != ErrStale {
		t.Fatalf("stale arm: %v, want ErrStale", r.Err)
	}
}

func TestArmFromRotctldOrMQTTRefused(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 10}), testCfg())
	h.call(SrcTUI, QueryPanIntent{})

	if r := h.call(SrcRotctld, ArmIntent{TrueAz: 0}); r.Err != ErrSource {
		t.Fatalf("arm from rotctld: %v, want ErrSource", r.Err)
	}
	if r := h.call(SrcMQTT, ArmIntent{TrueAz: 0}); r.Err != ErrSource {
		t.Fatalf("arm from mqtt: %v, want ErrSource", r.Err)
	}
}

func TestOffsetMath(t *testing.T) {
	// phys 100, told true az 30 → offset 70; snapshot az = phys − offset.
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 100}), testCfg())

	h.call(SrcTUI, QueryPanIntent{})
	h.call(SrcTUI, ArmIntent{TrueAz: 30})

	if !h.waitFor(2*time.Second, func(s *Snapshot) bool { return s.Armed }) {
		t.Fatal("never armed")
	}
	s := h.lastSnap()
	if diff := s.Offset - 70; diff > 0.01 || diff < -0.01 {
		t.Fatalf("offset = %.2f, want 70", s.Offset)
	}
	if s.Az != 30 {
		t.Fatalf("true az = %.2f, want 30", s.Az)
	}
}

// gotoAzEl positions the head in the ENGINE's user frame: the pan target
// crosses the arm offset, the elevation mirrors into the native tilt word.
// A query must read the requested true values back.
func TestGotoAzElAppliesOffsets(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 100, TiltDeg: 5,
		RateAzDegPerS: 200, RateElDegPerS: 100, SilenceRequired: 30 * time.Millisecond}), testCfg())
	h.arm(30) // phys 100 ↔ true 30 → offset 70

	h.gotoAzEl(200, 45)
	if got := h.tr.PanDeg(); got != 270 { // PHYSICAL: true 200 + offset 70
		t.Fatalf("head pan = %.2f, want 270 (true 200 + offset 70)", got)
	}
	if got := h.tr.TiltDeg(); got != 45 { // NATIVE: 90 − el 45
		t.Fatalf("head native tilt = %.2f, want 45 (el 45 mirrored)", got)
	}

	r := h.call(SrcTUI, QueryPanIntent{})
	if r.Err != nil || r.Deg != 200 {
		t.Fatalf("query pan = %.2f (err %v), want true 200", r.Deg, r.Err)
	}
	r = h.call(SrcTUI, QueryTiltIntent{})
	if r.Err != nil || r.Deg != 45 {
		t.Fatalf("query tilt = %.2f (err %v), want el 45", r.Deg, r.Err)
	}
}

// The TUI's goto prompt lands here: GotoAzElIntent drives the same
// verification ladder as set_pos, but from the TUI it works DISARMED —
// manual positioning like jog and goto-0, so with no arm the true target IS
// the physical target. Single-axis forms (HasAz/HasEl) get single-step
// ladders and leave the other axis untouched.
func TestGotoAzElIntentDrivesLadder(t *testing.T) {
	cfg := testCfg()
	cfg.Settle = 1500 * time.Millisecond // head must arrive before each verify (garbage while moving)
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 10, TiltDeg: 80,
		RateAzDegPerS: 200, RateElDegPerS: 100, SilenceRequired: 30 * time.Millisecond}), cfg)

	wait := func() {
		// Two settle windows per step (before the set, before its verify) at
		// 1.5 s each — allow for both steps plus travel.
		if !h.waitFor(10*time.Second, func(s *Snapshot) bool { return s.SetStatus == "converged" }) {
			t.Fatalf("goto ladder never converged: status=%q\nlogs:\n%s", h.lastSnap().SetStatus, h.logDump())
		}
	}
	if r := h.call(SrcTUI, GotoAzElIntent{Az: 200, El: 45, HasAz: true, HasEl: true}); r.Err != nil {
		t.Fatalf("goto az/el disarmed from TUI: %v", r.Err)
	}
	wait()
	if got := h.tr.PanDeg(); got != 200 { // offset 0 while disarmed
		t.Fatalf("head pan = %.2f, want 200", got)
	}
	if got := h.tr.TiltDeg(); got != 45 { // native = 90 − el 45
		t.Fatalf("head native tilt = %.2f, want 45 (el 45 mirrored)", got)
	}
	if s := h.lastSnap(); s.Az != 200 || s.El != 45 {
		t.Fatalf("snapshot az/el = %.2f/%.2f, want true 200/45", s.Az, s.El)
	}

	// El-only: pan must not move. Wait on the VALUE, not SetStatus — the
	// first goto's "converged" is still standing and would pass instantly.
	if r := h.call(SrcTUI, GotoAzElIntent{El: 30, HasEl: true}); r.Err != nil {
		t.Fatalf("goto el-only: %v", r.Err)
	}
	if !h.waitFor(10*time.Second, func(s *Snapshot) bool { return s.El == 30 && s.SetStatus == "converged" }) {
		t.Fatalf("el-only goto never converged: el=%.2f status=%q\nlogs:\n%s",
			h.lastSnap().El, h.lastSnap().SetStatus, h.logDump())
	}
	if got := h.tr.PanDeg(); got != 200 {
		t.Fatalf("el-only goto moved pan: %.2f", got)
	}
	if got := h.tr.TiltDeg(); got != 60 {
		t.Fatalf("head native tilt = %.2f, want 60 (el 30)", got)
	}
}

// Armed, the goto prompt's TRUE azimuth crosses the arm offset exactly like
// set_pos: physical = Norm360(true + offset).
func TestGotoAzElIntentCrossesArmOffset(t *testing.T) {
	cfg := testCfg()
	cfg.Settle = 1500 * time.Millisecond // head must arrive before the verify (garbage while moving)
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 100,
		RateAzDegPerS: 200, SilenceRequired: 30 * time.Millisecond}), cfg)
	h.arm(30) // phys 100 ↔ true 30 → offset 70

	if r := h.call(SrcTUI, GotoAzElIntent{Az: 200, HasAz: true}); r.Err != nil {
		t.Fatalf("goto az armed: %v", r.Err)
	}
	if !h.waitFor(4*time.Second, func(s *Snapshot) bool { return s.SetStatus == "converged" }) {
		t.Fatalf("goto ladder never converged: status=%q\nlogs:\n%s", h.lastSnap().SetStatus, h.logDump())
	}
	if got := h.tr.PanDeg(); got != 270 { // true 200 + offset 70
		t.Fatalf("head pan = %.2f, want 270 (true 200 + offset 70)", got)
	}
}

// --- one-outstanding-query discipline -----------------------------------------

func TestOneOutstandingQuery(t *testing.T) {
	// A transport that never answers pins the first query in flight.
	bh := &blackhole{closed: make(chan struct{})}
	reqCh := make(chan Request, 8)
	eng := New(testCfg(), bh, nil, reqCh, make(chan Event, 64))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Run(ctx)

	done := make(chan Result, 1)
	go func() {
		done <- eng.Call(context.Background(), SrcTUI, QueryPanIntent{})
	}()
	time.Sleep(100 * time.Millisecond) // first query TXed and sitting in flight

	// A second query while one is outstanding: refused as busy.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if r := eng.Call(ctx2, SrcTUI, QueryTiltIntent{}); r.Err != ErrBusy {
		t.Fatalf("second query: %v, want ErrBusy", r.Err)
	}

	select {
	case first := <-done:
		if first.Err != ErrNoFix {
			t.Fatalf("unanswered query: %v, want ErrNoFix", first.Err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reply wait never bounded the query")
	}
}

func TestQueryWhileMovingRefused(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 10,
		RateAzDegPerS: 20}), testCfg())
	h.arm(0)

	h.call(SrcTUI, JogIntent{Dir: DirRight})
	if r := h.call(SrcTUI, QueryPanIntent{}); r.Err != ErrMoving {
		t.Fatalf("query while moving: %v, want ErrMoving", r.Err)
	}
}

// --- the set ladder -------------------------------------------------------------

func TestSetLadderConverges(t *testing.T) {
	cfg := testCfg()
	cfg.Settle = 1500 * time.Millisecond // head must arrive before the verify (garbage while moving)
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 10, TiltDeg: 5,
		RateAzDegPerS: 200, RateElDegPerS: 100,
		SilenceRequired: 30 * time.Millisecond}), cfg)
	h.arm(0)

	if r := h.call(SrcTUI, SetPanIntent{Deg: 200}); r.Err != nil {
		t.Fatalf("set pan: %v", r.Err)
	}
	if !h.waitFor(8*time.Second, func(s *Snapshot) bool { return s.SetStatus == "converged" }) {
		t.Fatalf("ladder never converged: status=%q\nlogs:\n%s", h.lastSnap().SetStatus, h.logDump())
	}
	if got := h.tr.PanDeg(); got != 210 { // PHYSICAL: true 200 + offset 10
		t.Fatalf("head pan = %.2f, want 210", got)
	}
}

// A tilt set must cross the inverted scale: SetTiltIntent{30} (TRUE elevation)
// drives the head's NATIVE tilt word to 60; the snapshot reports both frames.
func TestSetTiltDrivesNativeWord(t *testing.T) {
	cfg := testCfg()
	cfg.Settle = 1500 * time.Millisecond // head must arrive before the verify (garbage while moving)
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 10, TiltDeg: 5,
		RateAzDegPerS: 200, RateElDegPerS: 100,
		SilenceRequired: 30 * time.Millisecond}), cfg)
	h.arm(0)

	if r := h.call(SrcTUI, SetTiltIntent{Deg: 30}); r.Err != nil {
		t.Fatalf("set tilt: %v", r.Err)
	}
	if !h.waitFor(8*time.Second, func(s *Snapshot) bool { return s.SetStatus == "converged" }) {
		t.Fatalf("tilt ladder never converged: status=%q\nlogs:\n%s", h.lastSnap().SetStatus, h.logDump())
	}
	if got := h.tr.TiltDeg(); got != 60 { // NATIVE word: true el 30 mirrored
		t.Fatalf("head native tilt = %.2f, want 60 (el 30 mirrored)", got)
	}
	s := h.lastSnap()
	if s.El != 30 {
		t.Fatalf("snapshot el = %.2f, want 30 (true elevation)", s.El)
	}
	if s.PhysEl != 60 {
		t.Fatalf("snapshot phys_el = %.2f, want 60 (native word)", s.PhysEl)
	}
}

func TestGotoPhysZeroIgnoresOffset(t *testing.T) {
	cfg := testCfg()
	cfg.Settle = 2500 * time.Millisecond // az travel is 200° at 100°/s — arrive before verify
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 200, TiltDeg: 45,
		RateAzDegPerS: 100, RateElDegPerS: 50, SilenceRequired: 30 * time.Millisecond}), cfg)
	h.arm(180) // offset 20

	h.eng.Call(context.Background(), SrcTUI, GotoPhysZeroIntent{})
	if !h.waitFor(12*time.Second, func(s *Snapshot) bool { return s.SetStatus == "converged" }) {
		t.Fatalf("goto 0 never converged: status=%q", h.lastSnap().SetStatus)
	}
	// PHYSICAL zero — the offset is never applied to this target.
	if h.tr.PanDeg() != 0 || h.tr.TiltDeg() != 0 {
		t.Fatalf("goto 0 landed at %.2f/%.2f, want physical 0/0", h.tr.PanDeg(), h.tr.TiltDeg())
	}
}

func TestSetLadderFailsWithoutResend(t *testing.T) {
	// A head whose silence window outlasts everything ignores every set. The
	// ladder must fail on the FIRST off-target verify — and, per the no-auto-
	// re-send decision (bench 2026-08-30), put exactly one set on the wire.
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 10,
		SilenceRequired: 10 * time.Second}), testCfg())
	h.arm(10)

	h.eng.Call(context.Background(), SrcTUI, SetPanIntent{Deg: 200})
	if !h.waitFor(6*time.Second, func(s *Snapshot) bool { return s.SetStatus == "failed" }) {
		t.Fatalf("ladder should fail, status=%q\nlogs:\n%s", h.lastSnap().SetStatus, h.logDump())
	}
	if n := strings.Count(h.logDump(), "set p="); n != 1 {
		t.Fatalf("ladder sent %d set frames, want 1 (no automatic re-send)\nlogs:\n%s", n, h.logDump())
	}
	if h.tr.PanDeg() != 10 {
		t.Fatalf("head moved on ignored sets: %.2f", h.tr.PanDeg())
	}
}

func TestStopCancelsLadder(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 10, TiltDeg: 5,
		SilenceRequired: 30 * time.Millisecond}), testCfg())
	h.arm(10)

	h.eng.Call(context.Background(), SrcTUI, SetPanIntent{Deg: 200})
	h.eng.Call(context.Background(), SrcTUI, StopIntent{}) // human wins

	if !h.waitFor(2*time.Second, func(s *Snapshot) bool {
		return !s.Moving && s.SetStatus != "setting"
	}) {
		t.Fatalf("ladder still running after stop: %q", h.lastSnap().SetStatus)
	}
	if got := h.tr.PanDeg(); got > 50 {
		t.Fatalf("head moved despite stop: %.2f", got)
	}
}

// --- crawl mode ------------------------------------------------------------------

// crawlCfg is testCfg plus crawl mode, scaled for the simulator: the simhead
// ignores the speed byte, so one burst's travel is rate × CrawlBurst — every
// crawl test picks rates where that is a sane fraction of the distance under
// test. CrawlMaxBursts keeps fillDefaults' 40 unless a test shrinks it.
func crawlCfg() Config {
	cfg := testCfg()
	cfg.Crawl = true
	cfg.CrawlSpeed = 0x04
	cfg.CrawlTol = 8.0
	cfg.CrawlBurst = 200 * time.Millisecond
	return cfg
}

// awaitMotion polls the head itself until its pan passes the threshold — no
// readbacks happen mid-motion, so snapshots carry stale positions.
func (h *harness) awaitMotion(minPan float64, what string) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.tr.PanDeg() < minPan {
		time.Sleep(10 * time.Millisecond)
	}
	if h.tr.PanDeg() < minPan {
		h.t.Fatalf("%s never moved the head\nlogs:\n%s", what, h.logDump())
	}
}

// Crawl converges both axes by jog bursts alone: no absolute-set frame may
// touch the wire, and each axis lands within the crawl tolerance.
func TestCrawlConvergesBothAxes(t *testing.T) {
	cfg := crawlCfg()
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 20, TiltDeg: 75,
		RateAzDegPerS: 100, RateElDegPerS: 50, SilenceRequired: 30 * time.Millisecond}), cfg)

	if r := h.call(SrcTUI, GotoAzElIntent{Az: 200, El: 45, HasAz: true, HasEl: true}); r.Err != nil {
		t.Fatalf("goto az/el disarmed from TUI: %v", r.Err)
	}
	if !h.waitFor(20*time.Second, func(s *Snapshot) bool { return s.SetStatus == "converged" }) {
		t.Fatalf("crawl never converged: status=%q\nlogs:\n%s", h.lastSnap().SetStatus, h.logDump())
	}
	dump := h.logDump()
	if n := strings.Count(dump, "set p=") + strings.Count(dump, "set t="); n != 0 {
		t.Fatalf("crawl put %d absolute-set frames on the wire\nlogs:\n%s", n, dump)
	}
	if d := math.Abs(h.tr.PanDeg() - 200); d > cfg.CrawlTol {
		t.Fatalf("head pan %.2f, want within %.1f° of 200", h.tr.PanDeg(), cfg.CrawlTol)
	}
	if d := math.Abs(h.tr.TiltDeg() - 45); d > cfg.CrawlTol {
		t.Fatalf("head native tilt %.2f, want within %.1f° of 45 (el 45 mirrored)", h.tr.TiltDeg(), cfg.CrawlTol)
	}
	if s := h.lastSnap(); math.Abs(s.Az-200) > cfg.CrawlTol || math.Abs(s.El-45) > cfg.CrawlTol {
		t.Fatalf("snapshot az/el %.2f/%.2f, want true 200/45", s.Az, s.El)
	}
	// The per-burst measurement line is the future learner's raw data — pin
	// its format: signed delta, burst length, speed byte (pan jogs right: +).
	if !strings.Contains(dump, "crawl p: +") || !strings.Contains(dump, "in 0.2s @ 0x04") {
		t.Fatalf("burst measurement line missing or malformed\nlogs:\n%s", dump)
	}
}

// Pan crawl takes the wraparound-shortest path: 350° → 10° is +20° (OpRight
// across zero), never the 340° sweep the other way round.
func TestCrawlWraparoundShortest(t *testing.T) {
	cfg := crawlCfg()
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 350,
		RateAzDegPerS: 100, SilenceRequired: 30 * time.Millisecond}), cfg)

	if r := h.call(SrcTUI, GotoAzElIntent{Az: 10, HasAz: true}); r.Err != nil {
		t.Fatalf("goto az: %v", r.Err)
	}
	if !h.waitFor(10*time.Second, func(s *Snapshot) bool { return s.SetStatus == "converged" }) {
		t.Fatalf("crawl never converged: status=%q\nlogs:\n%s", h.lastSnap().SetStatus, h.logDump())
	}
	dump := h.logDump()
	if !strings.Contains(dump, "TX FF 01 00 02") { // OpRight
		t.Fatalf("no right-jog burst for the +20° wraparound\nlogs:\n%s", dump)
	}
	if strings.Contains(dump, "TX FF 01 00 04") { // OpLeft
		t.Fatalf("crawl jogged left — the long way round\nlogs:\n%s", dump)
	}
	if d := pelco.Norm360(h.tr.PanDeg() - 10); d > cfg.CrawlTol && d < 360-cfg.CrawlTol {
		t.Fatalf("head pan %.2f, want within %.1f° of 10 across zero", h.tr.PanDeg(), cfg.CrawlTol)
	}
}

// The crawl's tilt bursts speak the NATIVE scale with no user-frame swap:
// toward a lower native word the engine sends OpDown, toward a higher one
// OpUp (OpUp raises native tilt, lowering the antenna). A wrongly swapped
// pair would drive both cases away from the target and never converge.
func TestCrawlTiltNativeDirection(t *testing.T) {
	cfg := crawlCfg()

	// Native 80 (el 10) → el 45 (native 45): target BELOW the current word.
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, TiltDeg: 80,
		RateElDegPerS: 50, SilenceRequired: 30 * time.Millisecond}), cfg)
	if r := h.call(SrcTUI, GotoAzElIntent{El: 45, HasEl: true}); r.Err != nil {
		t.Fatalf("goto el: %v", r.Err)
	}
	if !h.waitFor(10*time.Second, func(s *Snapshot) bool { return s.SetStatus == "converged" }) {
		t.Fatalf("crawl down never converged: status=%q\nlogs:\n%s", h.lastSnap().SetStatus, h.logDump())
	}
	dump := h.logDump()
	if !strings.Contains(dump, "TX FF 01 00 10") { // OpDown
		t.Fatalf("target below the native word did not jog DOWN\nlogs:\n%s", dump)
	}
	if strings.Contains(dump, "TX FF 01 00 08") { // OpUp
		t.Fatalf("target below the native word jogged UP — the tilt pair is swapped\nlogs:\n%s", dump)
	}
	if d := math.Abs(h.tr.TiltDeg() - 45); d > cfg.CrawlTol {
		t.Fatalf("head native tilt %.2f, want within %.1f° of 45", h.tr.TiltDeg(), cfg.CrawlTol)
	}

	// Native 5 (el 85) → el 45 (native 45): target ABOVE the current word.
	h2 := startHarness(t, simhead.New(simhead.Options{Addr: 1, TiltDeg: 5,
		RateElDegPerS: 50, SilenceRequired: 30 * time.Millisecond}), cfg)
	if r := h2.call(SrcTUI, GotoAzElIntent{El: 45, HasEl: true}); r.Err != nil {
		t.Fatalf("goto el: %v", r.Err)
	}
	if !h2.waitFor(10*time.Second, func(s *Snapshot) bool { return s.SetStatus == "converged" }) {
		t.Fatalf("crawl up never converged: status=%q\nlogs:\n%s", h2.lastSnap().SetStatus, h2.logDump())
	}
	dump2 := h2.logDump()
	if !strings.Contains(dump2, "TX FF 01 00 08") { // OpUp
		t.Fatalf("target above the native word did not jog UP\nlogs:\n%s", dump2)
	}
	if strings.Contains(dump2, "TX FF 01 00 10") { // OpDown
		t.Fatalf("target above the native word jogged DOWN — the tilt pair is swapped\nlogs:\n%s", dump2)
	}
}

// The burst cap fails the crawl instead of jogging forever: a target the
// bursts never reach puts exactly cap bursts on the wire, then "failed".
func TestCrawlCapFails(t *testing.T) {
	cfg := crawlCfg()
	cfg.CrawlMaxBursts = 2
	// 4°/s × 200 ms = 0.8° per burst: 200° is unreachable inside the cap.
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 0,
		RateAzDegPerS: 4, SilenceRequired: 30 * time.Millisecond}), cfg)

	if r := h.call(SrcTUI, GotoAzElIntent{Az: 200, HasAz: true}); r.Err != nil {
		t.Fatalf("goto az: %v", r.Err)
	}
	if !h.waitFor(10*time.Second, func(s *Snapshot) bool { return s.SetStatus == "failed" }) {
		t.Fatalf("crawl never failed: status=%q\nlogs:\n%s", h.lastSnap().SetStatus, h.logDump())
	}
	dump := h.logDump()
	if n := strings.Count(dump, "crawl p toward"); n != cfg.CrawlMaxBursts {
		t.Fatalf("crawl sent %d bursts, want exactly %d before failing\nlogs:\n%s", n, cfg.CrawlMaxBursts, dump)
	}
	if !strings.Contains(dump, "set FAILED: crawl: 2 bursts") {
		t.Fatalf("failure note missing the burst cap\nlogs:\n%s", dump)
	}
}

// An e-stop mid-burst halts the head, and nothing crawl-related may happen
// afterwards: no stop frame from a stale burst tick, no further burst.
func TestCrawlStopMidBurst(t *testing.T) {
	cfg := crawlCfg()
	cfg.CrawlBurst = 2 * time.Second // the e-stop lands well inside the burst
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 0,
		RateAzDegPerS: 20, SilenceRequired: 30 * time.Millisecond}), cfg)

	if r := h.call(SrcTUI, GotoAzElIntent{Az: 200, HasAz: true}); r.Err != nil {
		t.Fatalf("goto az: %v", r.Err)
	}
	h.awaitMotion(2, "crawl burst")
	h.call(SrcTUI, StopIntent{})

	if !h.waitFor(3*time.Second, func(s *Snapshot) bool { return !s.Moving }) {
		t.Fatalf("still moving after e-stop\nlogs:\n%s", h.logDump())
	}
	halted := h.tr.PanDeg()
	time.Sleep(2500 * time.Millisecond) // outlive the armed burst tick
	if got := h.tr.PanDeg(); got != halted {
		t.Fatalf("head moved after e-stop: %.2f → %.2f", halted, got)
	}
	dump := h.logDump()
	if strings.Contains(dump, "crawl burst end") {
		t.Fatalf("stale burst tick TXed after the e-stop\nlogs:\n%s", dump)
	}
	if n := strings.Count(dump, "crawl p toward"); n != 1 {
		t.Fatalf("%d crawl bursts total, want 1 (the e-stop won)\nlogs:\n%s", n, dump)
	}
}

// The crawl reads state BEFORE its first burst: the first crawl TX on the
// wire is the position query, and convergence needs no set frame.
func TestCrawlReadsBeforeFirstBurst(t *testing.T) {
	cfg := crawlCfg()
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 20,
		RateAzDegPerS: 100, SilenceRequired: 30 * time.Millisecond}), cfg)

	if r := h.call(SrcTUI, GotoAzElIntent{Az: 40, HasAz: true}); r.Err != nil {
		t.Fatalf("goto az: %v", r.Err)
	}
	if !h.waitFor(10*time.Second, func(s *Snapshot) bool { return s.SetStatus == "converged" }) {
		t.Fatalf("crawl never converged: status=%q\nlogs:\n%s", h.lastSnap().SetStatus, h.logDump())
	}
	dump := h.logDump()
	q, b := strings.Index(dump, "crawl query"), strings.Index(dump, "crawl p toward")
	if q < 0 || b < 0 {
		t.Fatalf("crawl log missing the query or burst lines\nlogs:\n%s", dump)
	}
	if q > b {
		t.Fatalf("first burst TX preceded the first read\nlogs:\n%s", dump)
	}
	if strings.Contains(dump, "set p=") {
		t.Fatalf("crawl used an absolute-set frame\nlogs:\n%s", dump)
	}
}

// rotctld's set_pos path crawls too: an armed set converges by bursts, with
// the arm offset crossed into the physical target exactly like the set ladder.
func TestCrawlRotctldSetPos(t *testing.T) {
	cfg := crawlCfg()
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 20,
		RateAzDegPerS: 100, SilenceRequired: 30 * time.Millisecond}), cfg)
	h.arm(0) // phys 20 ↔ true 0 → offset 20

	if r := h.call(SrcRotctld, SetPanIntent{Deg: 200}); r.Err != nil {
		t.Fatalf("rotctld set_pos: %v", r.Err)
	}
	if !h.waitFor(20*time.Second, func(s *Snapshot) bool { return s.SetStatus == "converged" }) {
		t.Fatalf("rotctld crawl never converged: status=%q\nlogs:\n%s", h.lastSnap().SetStatus, h.logDump())
	}
	if d := math.Abs(h.tr.PanDeg() - 220); d > cfg.CrawlTol { // true 200 + offset 20
		t.Fatalf("head pan %.2f, want within %.1f° of 220", h.tr.PanDeg(), cfg.CrawlTol)
	}
	if strings.Contains(h.logDump(), "set p=") {
		t.Fatalf("rotctld crawl used an absolute-set frame\nlogs:\n%s", h.logDump())
	}
}

// A goto under an active manual jog is refused with ErrMoving in BOTH modes:
// a jog never self-stops, and the old behaviour — silently clearing jogOp and
// checking against a moving head — guaranteed a garbage readback. The jog
// itself must survive the refusal untouched.
func TestGotoRefusedUnderManualJog(t *testing.T) {
	for name, crawl := range map[string]bool{"set ladder": false, "crawl": true} {
		t.Run(name, func(t *testing.T) {
			cfg := crawlCfg()
			cfg.Crawl = crawl
			h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 0,
				RateAzDegPerS: 20, SilenceRequired: 30 * time.Millisecond}), cfg)

			h.call(SrcTUI, JogIntent{Dir: DirRight})
			h.awaitMotion(2, "jog")

			if r := h.call(SrcTUI, GotoAzElIntent{Az: 200, HasAz: true}); r.Err != ErrMoving {
				t.Fatalf("goto under jog: %v, want ErrMoving", r.Err)
			}
			if s := h.lastSnap(); !s.Moving {
				t.Fatal("jog died on the refused goto — it must survive untouched")
			}
			if crawl && strings.Contains(h.logDump(), "crawl query") {
				t.Fatalf("crawl started under an active jog\nlogs:\n%s", h.logDump())
			}
			h.call(SrcTUI, StopIntent{})
			if !h.waitFor(3*time.Second, func(s *Snapshot) bool { return !s.Moving }) {
				t.Fatal("still moving after stop")
			}
		})
	}
}

// A manual jog queued mid-burst abandons the crawl at the next readback —
// without that escape hatch the jog would sit queued for the whole crawl
// (minutes at crawl speed) while the operator holds the key.
func TestCrawlQueuedJogWinsAtReadback(t *testing.T) {
	cfg := crawlCfg()
	cfg.CrawlBurst = 1 * time.Second // queue the jog well inside the burst
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 0,
		RateAzDegPerS: 20, SilenceRequired: 30 * time.Millisecond}), cfg)

	// Converge once first: the cancel below must CLEAR the status, not let a
	// fresh engine's "" hide a stale "converged" leaking through the cancel.
	if r := h.call(SrcTUI, GotoAzElIntent{Az: 0, HasAz: true}); r.Err != nil {
		t.Fatalf("pre-converge goto: %v", r.Err)
	}
	if !h.waitFor(5*time.Second, func(s *Snapshot) bool { return s.SetStatus == "converged" }) {
		t.Fatalf("pre-converge never converged: %q\nlogs:\n%s", h.lastSnap().SetStatus, h.logDump())
	}

	if r := h.call(SrcTUI, GotoAzElIntent{Az: 200, HasAz: true}); r.Err != nil {
		t.Fatalf("goto az: %v", r.Err)
	}
	h.awaitMotion(2, "crawl burst")
	if err := Submit(h.req, SrcTUI, JogIntent{Dir: DirRight}); err != nil {
		t.Fatalf("queue jog mid-burst: %v", err)
	}

	if !h.waitFor(5*time.Second, func(s *Snapshot) bool {
		return strings.Contains(h.logDump(), "crawl cancelled: manual jog")
	}) {
		t.Fatalf("queued jog never abandoned the crawl\nlogs:\n%s", h.logDump())
	}
	// The jog itself must now run — human wins — and the status must not
	// claim a set any more: neither the dead crawl's "setting" nor the FIRST
	// goto's stale "converged".
	if !h.waitFor(3*time.Second, func(s *Snapshot) bool {
		return strings.Contains(h.logDump(), "jog right") && s.SetStatus == ""
	}) {
		t.Fatalf("manual jog never ran (or the cancel leaked a stale status %q)\nlogs:\n%s",
			h.lastSnap().SetStatus, h.logDump())
	}
	h.call(SrcTUI, StopIntent{})
	if !h.waitFor(3*time.Second, func(s *Snapshot) bool { return !s.Moving }) {
		t.Fatal("still moving after stop")
	}
}

// A write failure at the crawl's first query unwinds exactly like the set
// ladder's: ErrWire to the caller, "failed" status, no wedged gate — and the
// engine keeps answering afterwards.
func TestWriteErrorUnwindsCrawl(t *testing.T) {
	w := &errWrite{block: make(chan struct{})}
	reqCh := make(chan Request, 8)
	evCh := make(chan Event, 64)
	eng := New(crawlCfg(), w, nil, reqCh, evCh)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Run(ctx)
	snaps := make(chan *Snapshot, 64)
	go func() {
		for ev := range evCh {
			if ev.Snap != nil {
				snaps <- ev.Snap
			}
		}
	}()
	var lastSnap *Snapshot
	last := func() *Snapshot {
		for {
			select {
			case s := <-snaps:
				lastSnap = s
			default:
				return lastSnap
			}
		}
	}

	qctx, qcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer qcancel()
	if r := eng.Call(qctx, SrcTUI, GotoAzElIntent{Az: 200, HasAz: true}); !errors.Is(r.Err, ErrWire) {
		t.Fatalf("goto on dead port: %v, want ErrWire", r.Err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var s *Snapshot
	for time.Now().Before(deadline) {
		if s = last(); s != nil && s.SetStatus == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s == nil || s.SetStatus != "failed" {
		t.Fatalf("crawl on dead port: status %q, want \"failed\"", s.SetStatus)
	}
	if s.Moving {
		t.Fatal("snapshot still moving after the unwind")
	}

	// No wedged gate behind the failed crawl: a query is answered promptly.
	start := time.Now()
	if r := eng.Call(qctx, SrcTUI, QueryPanIntent{}); !errors.Is(r.Err, ErrNoFix) {
		t.Fatalf("query after crawl unwind: %v, want ErrNoFix", r.Err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("query took %s after the unwind — gate wedged", time.Since(start))
	}
}

// A silent head fails the crawl through the same reply-wait path as the set
// ladder: the check query times out, the crawl fails "no readback", and NO
// burst ever left — nothing was known to move toward (read state FIRST).
func TestCrawlNoReadbackFails(t *testing.T) {
	bh := &blackhole{closed: make(chan struct{})}
	t.Cleanup(func() { bh.Close() })
	h := startHarnessTr(t, nil, bh, crawlCfg())

	if r := h.call(SrcTUI, GotoAzElIntent{Az: 200, HasAz: true}); r.Err != nil {
		t.Fatalf("goto az: %v", r.Err)
	}
	if !h.waitFor(4*time.Second, func(s *Snapshot) bool { return s.SetStatus == "failed" }) {
		t.Fatalf("silent head never failed the crawl: %q\nlogs:\n%s",
			h.lastSnap().SetStatus, h.logDump())
	}
	dump := h.logDump()
	if !strings.Contains(dump, "set FAILED: no readback") {
		t.Fatalf("failure note missing\nlogs:\n%s", dump)
	}
	if n := strings.Count(dump, "crawl p toward"); n != 0 {
		t.Fatalf("crawl jogged %d bursts without ever reading state\nlogs:\n%s", n, dump)
	}
	// Self-check disable at start + the one check query: nothing else may
	// have touched the wire.
	if n := bh.writeCount(); n != 2 {
		t.Fatalf("%d frames on the wire, want 2 (self-check disable + check query)", n)
	}
}

// The set ladder's first write failure must reach the caller as ErrWire —
// the same contract the crawl's first query already pins. It used to reply
// success for a set that never left the wire.
func TestSetWriteErrorRepliesErrWire(t *testing.T) {
	w := &errWrite{block: make(chan struct{})}
	reqCh := make(chan Request, 8)
	evCh := make(chan Event, 64)
	eng := New(testCfg(), w, nil, reqCh, evCh) // NON-crawl: the set ladder path
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Run(ctx)

	qctx, qcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer qcancel()
	if r := eng.Call(qctx, SrcTUI, GotoAzElIntent{Az: 200, HasAz: true}); !errors.Is(r.Err, ErrWire) {
		t.Fatalf("set-mode goto on dead port: %v, want ErrWire", r.Err)
	}
}

// Only a readback that ANSWERS the ladder's outstanding query drives it: a
// wrong-axis frame (the late reply to an expired user query) or a duplicate
// must not be mistaken for the check readback — the bogus error would jog
// the head off garbage data. White-box: no Run loop, the test IS the engine
// goroutine, so the interleaving is deterministic.
func TestCrawlIgnoresWrongAxisReadback(t *testing.T) {
	bh := &blackhole{closed: make(chan struct{})}
	t.Cleanup(func() { bh.Close() })
	eng := New(crawlCfg(), bh, nil, make(chan Request, 4), make(chan Event, 64))
	eng.cfg.fillDefaults()
	eng.timer = time.NewTimer(time.Hour)
	if !eng.timer.Stop() {
		<-eng.timer.C
	}

	// A pan crawl with its check query outstanding.
	eng.ladder = &ladderState{steps: []ladderStep{{'p', 200}}, phase: 3,
		crawl: true, lastDeg: math.NaN()}
	eng.inFlight = &query{op: pelco.OpRspPan}

	// A late TILT reply to some expired user query: wrong axis.
	eng.gotReadback('t', 45, pelco.RxFrame{Frame: pelco.Build(1, 0, pelco.OpRspTilt, 0x11, 0x58)})
	if n := bh.writeCount(); n != 0 {
		t.Fatalf("wrong-axis readback TXed %d frames, want 0", n)
	}
	if eng.ladder == nil || eng.ladder.phase != 3 {
		t.Fatalf("wrong-axis readback disturbed the ladder: %+v", eng.ladder)
	}
	if eng.inFlight == nil {
		t.Fatal("wrong-axis readback consumed the outstanding pan query")
	}

	// The matching PAN readback drives the crawl: exactly one jog burst.
	eng.gotReadback('p', 30, pelco.RxFrame{Frame: pelco.Build(1, 0, pelco.OpRspPan, 0x0B, 0xB8)})
	if n := bh.writeCount(); n != 1 {
		t.Fatalf("matching readback TXed %d frames, want 1 (the burst)", n)
	}
	if op := bh.lastWrite()[3]; op != pelco.OpRight { // 30° → 200°: right
		t.Fatalf("burst opcode = %#x, want OpRight", op)
	}

	// A duplicate pan reply must not fire a second burst (the query it
	// would answer is already consumed).
	eng.gotReadback('p', 31, pelco.RxFrame{Frame: pelco.Build(1, 0, pelco.OpRspPan, 0x0C, 0x24)})
	if n := bh.writeCount(); n != 1 {
		t.Fatalf("duplicate readback TXed %d frames, want still 1", n)
	}
}

// stopFailHead forwards to the simulated head but fails every STOP frame —
// the crawl's tickBurst stop TX against a freshly-dead adapter.
type stopFailHead struct {
	*simhead.Head
}

func (s *stopFailHead) Write(b []byte) error {
	if len(b) >= 4 && b[3] == pelco.OpStop {
		return errors.New("serial write: port gone")
	}
	return s.Head.Write(b)
}

// A stop-frame write failure mid-crawl unwinds (invariant 5) and strands
// nothing: status "failed", exactly one burst on the wire, and the engine
// still answers requests afterwards.
func TestCrawlStopFrameWriteErrorUnwinds(t *testing.T) {
	cfg := crawlCfg()
	head := simhead.New(simhead.Options{Addr: 1, PanDeg: 0,
		RateAzDegPerS: 20, SilenceRequired: 30 * time.Millisecond})
	h := startHarnessTr(t, head, &stopFailHead{Head: head}, cfg)

	if r := h.call(SrcTUI, GotoAzElIntent{Az: 200, HasAz: true}); r.Err != nil {
		t.Fatalf("goto az: %v", r.Err)
	}
	h.awaitMotion(2, "crawl burst")
	if !h.waitFor(6*time.Second, func(s *Snapshot) bool { return s.SetStatus == "failed" }) {
		t.Fatalf("stop-frame write failure never failed the crawl: %q\nlogs:\n%s",
			h.lastSnap().SetStatus, h.logDump())
	}
	dump := h.logDump()
	if n := strings.Count(dump, "crawl p toward"); n != 1 {
		t.Fatalf("%d bursts, want exactly 1 before the failed stop\nlogs:\n%s", n, dump)
	}
	if strings.Contains(dump, "crawl burst end") {
		t.Fatalf("the never-written stop frame was logged as TXed\nlogs:\n%s", dump)
	}
	// No wedge: a query is answered (or honestly refused) well inside the
	// caller timeout — the queue is not stranded on the dead write.
	start := time.Now()
	_ = h.call(SrcTUI, QueryPanIntent{})
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("query took %s after the unwind — gate wedged", time.Since(start))
	}
}

// Out-of-range elevation is refused at the intent boundary in BOTH modes:
// the set ladder clamps at the wire (SetTiltFrame), but a crawl would chase
// the unreachable native target with jog bursts straight into the
// mechanical limit until the burst cap.
func TestGotoRefusesOutOfRangeElevation(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1}), crawlCfg())
	h.arm(10)

	for name, it := range map[string]Intent{
		"goto el 120":  GotoAzElIntent{Az: 200, El: 120, HasAz: true, HasEl: true},
		"goto el -5":   GotoAzElIntent{El: -5, HasEl: true},
		"set tilt 120": SetTiltIntent{Deg: 120},
	} {
		if r := h.call(SrcTUI, it); r.Err == nil {
			t.Fatalf("%s accepted — the crawl would grind into the mechanical limit", name)
		}
	}
	dump := h.logDump()
	for _, bad := range []string{"crawl p toward", "crawl t toward", "crawl query", "set p=", "set t="} {
		if strings.Contains(dump, bad) {
			t.Fatalf("refused out-of-range target produced wire traffic (%q)\nlogs:\n%s", bad, dump)
		}
	}
}

// --- jog & speed ------------------------------------------------------------------

func TestJogMovesAndStops(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 10,
		RateAzDegPerS: 20, SilenceRequired: 30 * time.Millisecond}), testCfg())
	h.arm(10)

	h.call(SrcTUI, JogIntent{Dir: DirRight})
	if !h.waitFor(3*time.Second, func(s *Snapshot) bool { return s.Moving }) {
		t.Fatal("jog did not start motion")
	}
	// Let some real motion accrue before stopping (no readbacks happen during
	// a jog — snapshots carry the stale pan — so poll the head itself).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.tr.PanDeg() <= 12 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.tr.PanDeg(); got <= 12 {
		t.Fatalf("head never moved: %.2f", got)
	}
	h.call(SrcTUI, StopIntent{})
	if !h.waitFor(3*time.Second, func(s *Snapshot) bool { return !s.Moving }) {
		t.Fatal("still moving after stop")
	}
	if got := h.tr.PanDeg(); got <= 10 {
		t.Fatalf("head never moved: %.2f", got)
	}
}

// Jog "up" means ELEVATION up: the head's native tilt word must FALL, because
// the jog opcodes speak the inverted native scale (bench 2026-08-30).
func TestJogUpRaisesElevation(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, TiltDeg: 45,
		RateElDegPerS: 50, SilenceRequired: 30 * time.Millisecond}), testCfg())

	// TUI jog is allowed disarmed — no readbacks happen mid-jog, so poll the
	// head's native word directly.
	h.call(SrcTUI, JogIntent{Dir: DirUp})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.tr.TiltDeg() >= 43 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.tr.TiltDeg(); got >= 43 {
		t.Fatalf("jog up never lowered the native tilt: %.2f", got)
	}
	h.call(SrcTUI, StopIntent{})
	if !h.waitFor(3*time.Second, func(s *Snapshot) bool { return !s.Moving }) {
		t.Fatal("still moving after stop")
	}
}

func TestJogSpeedClamped(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1}), testCfg())

	h.call(SrcTUI, JogSpeedIntent{Speed: 0xFF})
	if !h.waitFor(2*time.Second, func(s *Snapshot) bool { return s.JogSpeed == pelco.MaxSpeed }) {
		t.Fatalf("jog speed not clamped, snap=%+v", h.lastSnap())
	}
}

// --- e-stop vs queued motion ----------------------------------------------------

// An e-stop must also kill motion that is still QUEUED (gate closed / query
// outstanding). If it only killed the active ladder, the next drain would
// replay a stale set seconds after the all-stop frame — motion with no
// operator input behind it.
func TestEstopCancelsPendingMotion(t *testing.T) {
	bh := &blackhole{closed: make(chan struct{})}
	reqCh := make(chan Request, 8)
	evCh := make(chan Event, 64)
	eng := New(testCfg(), bh, nil, reqCh, evCh)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Run(ctx)

	// A query that never gets an answer holds e.inFlight for ReplyWait; with
	// the query outstanding a following set is queued, not executed.
	qrep := make(chan Result, 1)
	reqCh <- Request{From: SrcTUI, Intent: QueryPanIntent{}, Reply: qrep}
	time.Sleep(100 * time.Millisecond) // query TXed, inFlight set, frame gap elapsed

	rep := make(chan Result, 1)
	reqCh <- Request{From: SrcTUI, Intent: GotoPhysZeroIntent{}, Reply: rep}
	time.Sleep(50 * time.Millisecond) // must sit in the queue, not execute

	bh.mu.Lock()
	nWrites := len(bh.writes)
	bh.mu.Unlock()
	if nWrites != 2 { // self-check disable + the query — and nothing else
		t.Fatalf("%d frames on the wire after queued goto (want 2)", nWrites)
	}

	if r := eng.Call(context.Background(), SrcTUI, StopIntent{}); r.Err != nil {
		t.Fatalf("estop: %v", r.Err)
	}

	select {
	case r := <-rep:
		if !errors.Is(r.Err, ErrCancelled) {
			t.Fatalf("queued goto answered %v, want ErrCancelled", r.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued goto never answered after e-stop")
	}

	// No set frame may have reached the wire — before or after the stop.
	bh.mu.Lock()
	defer bh.mu.Unlock()
	if len(bh.writes) != 3 { // + the all-stop frame
		t.Fatalf("%d frames after e-stop (want 3)", len(bh.writes))
	}
	for i, w := range bh.writes {
		if w[3] == pelco.OpSetPan || w[3] == pelco.OpSetTilt {
			t.Fatalf("frame %d is a set opcode — queued motion survived the e-stop", i)
		}
	}
	if bh.writes[2][3] != pelco.OpStop {
		t.Fatalf("last frame opcode %#x, want all-stop", bh.writes[2][3])
	}
}

// --- reader restart / auto-reopen ------------------------------------------------

// flakyTransport fails the first N reads, then serves frames from a channel.
// Writes are recorded so tests can wait for a frame to actually be TXed.
type flakyTransport struct {
	mu      sync.Mutex
	fails   int
	reopens int
	writes  [][]byte
	frames  chan []byte
}

func (f *flakyTransport) Read(p []byte) (int, error) {
	f.mu.Lock()
	if f.fails > 0 {
		f.fails--
		f.mu.Unlock()
		return 0, errors.New("simulated read error")
	}
	f.mu.Unlock()
	fr, ok := <-f.frames
	if !ok {
		return 0, errors.New("transport closed")
	}
	return copy(p, fr), nil
}

func (f *flakyTransport) Write(b []byte) error {
	f.mu.Lock()
	f.writes = append(f.writes, append([]byte(nil), b...))
	f.mu.Unlock()
	return nil
}
func (f *flakyTransport) Close() error { return nil }

// awaitWrite polls until a frame with the given opcode hit the wire.
func (f *flakyTransport) awaitWrite(t *testing.T, op byte) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, w := range f.writes {
			if w[3] == op {
				f.mu.Unlock()
				return
			}
		}
		f.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %#x frame on the wire within 3s", op)
}

func panRspFrame(addr byte, deg float64) []byte {
	w := uint16(deg * 100)
	f := pelco.Build(addr, 0x00, pelco.OpRspPan, byte(w>>8), byte(w))
	return f[:]
}

// A dead read (re-enumerated USB fd, dropped TCP mock) must not leave the
// engine deaf: it reopens the transport and starts a fresh reader, which picks
// up new frames and brings the head back online.
func TestReadLoopRestartsAfterReadError(t *testing.T) {
	tr := &flakyTransport{frames: make(chan []byte, 4)}
	tr.mu.Lock()
	tr.fails = 1 // the very first reader dies on its first read
	tr.mu.Unlock()

	reqCh := make(chan Request, 8)
	evCh := make(chan Event, 256)
	eng := New(testCfg(), tr, func() error { tr.mu.Lock(); tr.reopens++; tr.mu.Unlock(); return nil }, reqCh, evCh)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Run(ctx)
	go func() {
		for range evCh { // drain: slow-consumer drops must not wedge the test
		}
	}()

	// Feed a pan response for the fresh reader: the engine must mark the head
	// online. Only then start a query — and answer it only once its frame is
	// actually on the wire (a readback arriving before the query TX would
	// legitimately not match the in-flight query).
	tr.frames <- panRspFrame(1, 87.65)

	done := make(chan Result, 1)
	go func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer ccancel()
		done <- eng.Call(cctx, SrcTUI, QueryPanIntent{})
	}()
	tr.awaitWrite(t, pelco.OpQueryPan)
	tr.frames <- panRspFrame(1, 87.65)

	select {
	case r := <-done:
		if r.Err != nil {
			t.Fatalf("query after auto-reopen: %v", r.Err)
		}
		if r.Deg != 87.65 {
			t.Fatalf("query = %.2f, want 87.65", r.Deg)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("engine never recovered from the read error")
	}

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.reopens != 1 {
		t.Errorf("reopen called %d times, want 1", tr.reopens)
	}
}

// --- write-error unwinding ---------------------------------------------------------

// errWrite blocks reads forever (nothing ever comes back) and fails every
// write: a dead fd whose EIO surfaces on the TX side.
type errWrite struct {
	block chan struct{}
}

func (w *errWrite) Read(p []byte) (int, error) {
	<-w.block // never answered
	return 0, errors.New("transport closed")
}
func (w *errWrite) Write(b []byte) error { return errors.New("serial write: port gone") }
func (w *errWrite) Close() error         { return nil }

// A failed write must unwind the state machine (fail the in-flight query, kill
// the ladder) instead of wedging it — no timer is armed when nothing went out
// on the wire, so a half-set-up ladder would otherwise hang forever.
func TestWriteErrorUnwinds(t *testing.T) {
	w := &errWrite{block: make(chan struct{})}
	reqCh := make(chan Request, 8)
	evCh := make(chan Event, 64)
	eng := New(testCfg(), w, nil, reqCh, evCh)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Run(ctx)
	snaps := make(chan *Snapshot, 64)
	go func() {
		for ev := range evCh {
			if ev.Snap != nil {
				snaps <- ev.Snap
			}
		}
	}()
	var lastSnap *Snapshot
	last := func() *Snapshot {
		for {
			select {
			case s := <-snaps:
				lastSnap = s
			default:
				return lastSnap
			}
		}
	}

	// A query whose write fails is answered promptly with no-fix, not left
	// dangling until the reply-wait timeout.
	qctx, qcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer qcancel()
	start := time.Now()
	if r := eng.Call(qctx, SrcTUI, QueryPanIntent{}); !errors.Is(r.Err, ErrNoFix) {
		t.Fatalf("query on dead port: %v, want ErrNoFix", r.Err)
	} else if time.Since(start) > time.Second {
		t.Fatalf("query took %s on a dead port — not unwound", time.Since(start))
	}

	// A goto-0 whose first set write fails must report failure, not sit in
	// "setting" forever.
	go func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer ccancel()
		eng.Call(cctx, SrcTUI, GotoPhysZeroIntent{})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := last(); s != nil && s.SetStatus == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := last(); s == nil || s.SetStatus != "failed" {
		t.Fatalf("goto 0 on dead port: set status %q, want \"failed\"", s.SetStatus)
	}

	// A self-test / self-check toggle whose write fails is answered with
	// txfail, not success — and the self-check model must not claim a frame
	// that never left: it stays "unknown", never "off" or "on".
	if r := eng.Call(qctx, SrcTUI, SelfCheckIntent{Enable: false}); !errors.Is(r.Err, ErrTxFail) {
		t.Fatalf("self-check disable on dead port: %v, want ErrTxFail", r.Err)
	}
	if r := eng.Call(qctx, SrcTUI, SelfCheckIntent{Enable: true}); !errors.Is(r.Err, ErrTxFail) {
		t.Fatalf("self-check enable on dead port: %v, want ErrTxFail", r.Err)
	}
	if r := eng.Call(qctx, SrcTUI, SelfTestIntent{}); !errors.Is(r.Err, ErrTxFail) {
		t.Fatalf("self-test on dead port: %v, want ErrTxFail", r.Err)
	}
	if s := last(); s == nil || s.SelfCheck != "unknown" {
		t.Fatalf("self-check model = %q on a dead port, want unknown", s.SelfCheck)
	}
}

// --- true-az query replies ---------------------------------------------------------

// User-facing query replies (rotctld get_pos, TUI 'a'/'e') are answered in
// TRUE degrees — the same frame set_pos speaks — not raw physical readback.
func TestQueryReplyAppliesOffset(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 100}), testCfg())

	if r := h.call(SrcTUI, QueryPanIntent{}); r.Err != nil {
		t.Fatalf("pre-arm query: %v", r.Err)
	}
	if r := h.call(SrcTUI, ArmIntent{TrueAz: 30}); r.Err != nil {
		t.Fatalf("arm: %v", r.Err)
	}

	r := h.call(SrcTUI, QueryPanIntent{})
	if r.Err != nil {
		t.Fatalf("armed query: %v", r.Err)
	}
	if r.Deg != 30 {
		t.Fatalf("armed query = %.2f, want 30 (phys 100 − offset 70)", r.Deg)
	}
}

// --- source gating -----------------------------------------------------------------

func TestSelfTestOnlyFromTUI(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 200, TiltDeg: 45,
		RateAzDegPerS: 200, RateElDegPerS: 100, SilenceRequired: 30 * time.Millisecond}), testCfg())

	// MQTT/rotctld may never trigger the cable-ripper.
	if r := h.call(SrcMQTT, SelfTestIntent{}); r.Err != ErrSource {
		t.Fatalf("self-test from mqtt: %v, want ErrSource", r.Err)
	}
	if r := h.call(SrcRotctld, SelfTestIntent{}); r.Err != ErrSource {
		t.Fatalf("self-test from rotctld: %v, want ErrSource", r.Err)
	}

	// The TUI can — but the call may race the connect-disable's frame-gap
	// window: ErrBusy means "retry", never "refused".
	r := h.callNoBusy(SrcTUI, SelfTestIntent{})
	if r.Err != nil {
		t.Fatalf("self-test from TUI: %v", r.Err)
	}

	// The (simulated) head re-homes to 0/0.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && h.tr.PanDeg() != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if h.tr.PanDeg() != 0 {
		t.Fatalf("self-test did not re-home: %.2f", h.tr.PanDeg())
	}

	// Factory defaults restore the periodic self-check AND invalidate every
	// readback: the arm gate must demand a fresh position after a re-home.
	if !h.waitFor(2*time.Second, func(s *Snapshot) bool { return s.SelfCheck == "on" }) {
		t.Fatalf("snapshot self-check = %q, want on after self-test", h.lastSnap().SelfCheck)
	}
	if s := h.lastSnap(); s.ReadbackValid {
		t.Error("self-test must invalidate the readback (pre-re-home position is stale)")
	}
	if !h.tr.SelfCheck() {
		t.Error("self-test must restore the simulated head's factory self-check")
	}
}

// The self-check model is honest about proof. No reply follows a preset frame
// (RS-485 has no link ACK), so the connect-time disable leaves the model at
// "unknown" until the head proves it is alive with a frame — then the pending
// claim lands. The claim dies with the link, and the toggle keeps its guards.
func TestSelfCheckLifecycle(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 10, TiltDeg: 5,
		RateAzDegPerS: 200, RateElDegPerS: 100, SilenceRequired: 30 * time.Millisecond}), testCfg())

	// Before any RX frame the model reads "unknown" — never a premature "off",
	// even though the disable frame itself did reach the head.
	if !h.waitFor(2*time.Second, func(s *Snapshot) bool { return s.SelfCheck == "unknown" }) {
		t.Fatalf("snapshot self-check = %q before any reply, want unknown", h.lastSnap().SelfCheck)
	}
	if h.tr.SelfCheck() {
		t.Fatal("connect-time disable never reached the simulated head")
	}

	// Only the TUI may touch the self-check.
	if r := h.call(SrcMQTT, SelfCheckIntent{Enable: true}); r.Err != ErrSource {
		t.Fatalf("self-check enable from mqtt: %v, want ErrSource", r.Err)
	}
	if r := h.call(SrcRotctld, SelfCheckIntent{Enable: false}); r.Err != ErrSource {
		t.Fatalf("self-check disable from rotctld: %v, want ErrSource", r.Err)
	}

	// Proof of life: the arm precondition's query reply is the first RX frame,
	// and the pending "off" claim lands with it.
	h.arm(10)
	if !h.waitFor(2*time.Second, func(s *Snapshot) bool { return s.SelfCheck == "off" }) {
		t.Fatalf("snapshot self-check = %q after the head answered, want off", h.lastSnap().SelfCheck)
	}

	// Enabling under an armed rotator is the hazard the arm gate exists for.
	if r := h.call(SrcTUI, SelfCheckIntent{Enable: true}); r.Err == nil {
		t.Fatal("self-check enable accepted while armed")
	}
	if r := h.call(SrcTUI, SelfTestIntent{}); r.Err == nil {
		t.Fatal("self-test accepted while armed")
	}
	if r := h.call(SrcTUI, DisarmIntent{}); r.Err != nil {
		t.Fatalf("disarm: %v", r.Err)
	}

	// A moving rotator refuses the toggle, and the model does not flip on a
	// refusal.
	if r := h.call(SrcTUI, JogIntent{Dir: DirRight}); r.Err != nil {
		t.Fatalf("jog: %v", r.Err)
	}
	if r := h.call(SrcTUI, SelfCheckIntent{Enable: false}); r.Err != ErrMoving {
		t.Fatalf("self-check disable while moving: %v, want ErrMoving", r.Err)
	}
	if s := h.lastSnap(); s.SelfCheck != "off" {
		t.Fatalf("model = %q after a refused toggle, want off unchanged", s.SelfCheck)
	}
	if r := h.call(SrcTUI, StopIntent{}); r.Err != nil {
		t.Fatalf("stop: %v", r.Err)
	}

	// TUI enable, disarmed: the head flips and the snapshot follows.
	if r := h.callNoBusy(SrcTUI, SelfCheckIntent{Enable: true}); r.Err != nil {
		t.Fatalf("self-check enable: %v", r.Err)
	}
	if !h.waitFor(2*time.Second, func(s *Snapshot) bool { return s.SelfCheck == "on" }) {
		t.Fatalf("snapshot self-check = %q, want on", h.lastSnap().SelfCheck)
	}
	if !h.tr.SelfCheck() {
		t.Fatal("enable never reached the simulated head")
	}

	// And back off — the station default.
	if r := h.callNoBusy(SrcTUI, SelfCheckIntent{Enable: false}); r.Err != nil {
		t.Fatalf("self-check disable: %v", r.Err)
	}
	if !h.waitFor(2*time.Second, func(s *Snapshot) bool { return s.SelfCheck == "off" }) {
		t.Fatalf("snapshot self-check = %q, want off", h.lastSnap().SelfCheck)
	}
	if h.tr.SelfCheck() {
		t.Fatal("disable never reached the simulated head")
	}

	// Link death drops the claim: the model returns to "unknown" (honesty
	// over optimism). No reopener is wired in tests, so no re-send happens
	// either — that path is the reopen intents' job.
	h.tr.Close()
	if !h.waitFor(2*time.Second, func(s *Snapshot) bool { return s.SelfCheck == "unknown" }) {
		t.Fatalf("snapshot self-check = %q after link death, want unknown", h.lastSnap().SelfCheck)
	}
	if s := h.lastSnap(); s.DeviceOnline {
		t.Error("device still online after link death")
	}
}

func TestStopFromEverySource(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1}), testCfg())
	for _, src := range []Source{SrcTUI, SrcRotctld, SrcMQTT} {
		if r := h.call(src, StopIntent{}); r.Err != nil {
			t.Fatalf("stop from %s: %v", src, r.Err)
		}
	}
}
