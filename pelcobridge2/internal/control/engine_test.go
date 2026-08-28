package control

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"pelcobridge2/internal/pelco"
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
		Settle: 100 * time.Millisecond, SetAttempts: 2, SetTolerance: 0.5,
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
	h := &harness{t: t, tr: head, req: make(chan Request, 32)}
	evCh := make(chan Event, 256)
	h.eng = New(cfg, head, nil, h.req, evCh)
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

// arm queries pan once, then arms with the given true azimuth.
func (h *harness) arm(trueAz float64) {
	if r := h.call(SrcTUI, QueryPanIntent{}); r.Err != nil {
		h.t.Fatalf("arm precondition (query): %v", r.Err)
	}
	if r := h.call(SrcTUI, ArmIntent{TrueAz: trueAz}); r.Err != nil {
		h.t.Fatalf("arm: %v", r.Err)
	}
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

func TestQueryTiltReturnsTextbookDegrees(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, TiltDeg: 45.5}), testCfg())

	r := h.call(SrcTUI, QueryTiltIntent{})
	if r.Err != nil {
		t.Fatalf("query tilt: %v", r.Err)
	}
	if r.Deg != 45.5 {
		t.Fatalf("query tilt = %.2f, want 45.5", r.Deg)
	}
}

// --- arming -------------------------------------------------------------------

func TestDisarmedRefusals(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1}), testCfg())

	for _, it := range []Intent{
		JogIntent{Dir: DirUp},
		SetPanIntent{Deg: 100},
		SetTiltIntent{Deg: 30},
		GotoPhysZeroIntent{},
	} {
		if r := h.call(SrcTUI, it); r.Err != ErrDisarmed {
			t.Fatalf("%T disarmed: %v, want ErrDisarmed", it, r.Err)
		}
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

func TestSetLadderFailsAfterRetries(t *testing.T) {
	// A head whose silence window outlasts everything ignores every set, so
	// the ladder must exhaust its tries and report failure.
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1, PanDeg: 10,
		SilenceRequired: 10 * time.Second}), testCfg())
	h.arm(10)

	h.eng.Call(context.Background(), SrcTUI, SetPanIntent{Deg: 200})
	if !h.waitFor(6*time.Second, func(s *Snapshot) bool { return s.SetStatus == "failed" }) {
		t.Fatalf("ladder should fail, status=%q", h.lastSnap().SetStatus)
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

func TestJogSpeedClamped(t *testing.T) {
	h := startHarness(t, simhead.New(simhead.Options{Addr: 1}), testCfg())

	h.call(SrcTUI, JogSpeedIntent{Speed: 0xFF})
	if !h.waitFor(2*time.Second, func(s *Snapshot) bool { return s.JogSpeed == pelco.MaxSpeed }) {
		t.Fatalf("jog speed not clamped, snap=%+v", h.lastSnap())
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

	// The TUI can — and the (simulated) head re-homes to 0/0.
	h.eng.Call(context.Background(), SrcTUI, SelfTestIntent{})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && h.tr.PanDeg() != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if h.tr.PanDeg() != 0 {
		t.Fatalf("self-test did not re-home: %.2f", h.tr.PanDeg())
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
