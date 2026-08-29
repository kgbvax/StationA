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

	// Absolute sets stay arm-gated for EVERY source.
	for _, it := range []Intent{
		SetPanIntent{Deg: 100},
		SetTiltIntent{Deg: 30},
	} {
		if r := h.call(SrcTUI, it); r.Err != ErrDisarmed {
			t.Fatalf("%T disarmed: %v, want ErrDisarmed", it, r.Err)
		}
	}
	// TUI manual motion (jog, goto 0) is NOT gated by arming — the operator
	// positions the head with it BEFORE arming.
	for _, it := range []Intent{
		JogIntent{Dir: DirUp},
		GotoPhysZeroIntent{},
	} {
		if r := h.call(SrcTUI, it); r.Err != nil {
			t.Fatalf("%T from TUI disarmed: %v, want allowed", it, r.Err)
		}
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
