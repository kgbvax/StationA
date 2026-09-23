// SPDX-License-Identifier: AGPL-3.0-or-later

package rotctld

// U6 wire tests: the rotctld TCP server over the REAL mount façade (U4) with
// scriptable per-axis fakes standing in for the drivers, byte-pinned in the
// pelcobridge2 TestWireTable style (plan KTD10 — keep the dialect
// byte-identical). The composition test at the bottom drives p/P/S through
// the real façade into U2's and U3's in-process mock devices (mock mode).

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"spid-ercm-rotator-bridge/internal/config"
	"spid-ercm-rotator-bridge/internal/ercm"
	"spid-ercm-rotator-bridge/internal/mount"
	"spid-ercm-rotator-bridge/internal/spid"
)

// The façade satisfies the server's Facade interface as-is (KTD12).
var _ Facade = (*mount.Mount)(nil)

// --- test scaffolding ---------------------------------------------------------------

// testControl is the wire-table travel envelope (the config defaults' shape).
func testControl() config.ControlConfig {
	return config.ControlConfig{
		AZ: config.AxisControl{Min: 0, Max: 360, Deadband: 1, Park: 0},
		EL: config.AxisControl{Min: 0, Max: 90, Deadband: 1, Park: 0},
	}
}

// fakeAxis is the scriptable per-axis Controller fake: canned readback +
// validity, a liveness switch, a SetTarget write log (the "did the serial
// write happen?" probe for refusal tests) and a stop log with an optional
// fault. The cross-axis semantics live in the real façade above it.
type fakeAxis struct {
	mu      sync.Mutex
	online  bool
	rb      float64
	valid   bool
	targets []float64
	stops   int
	stopErr error
}

func newFakeAxis() *fakeAxis { return &fakeAxis{online: true} }

// fakeAt: online with a valid canned readback.
func fakeAt(deg float64) *fakeAxis {
	f := newFakeAxis()
	f.setReadback(deg, true)
	return f
}

// fakeDark: online but pre-readback (valid=false) — the KTD9 -11 case.
func fakeDark() *fakeAxis { return newFakeAxis() }

// fakeDead: device link down, pre-readback — the KTD9 -9 case.
func fakeDead() *fakeAxis {
	f := newFakeAxis()
	f.setOnline(false)
	return f
}

// SetTarget parks instantly: the readback tracks the write, so a follow-up p
// observes the commanded position (the mock-device convention).
func (f *fakeAxis) SetTarget(deg float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = append(f.targets, deg)
	f.rb = deg
	f.valid = true
	return nil
}

func (f *fakeAxis) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return f.stopErr
}

func (f *fakeAxis) Readback() (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rb, f.valid
}

func (f *fakeAxis) Online() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online
}

func (f *fakeAxis) setReadback(deg float64, valid bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rb, f.valid = deg, valid
}

func (f *fakeAxis) setOnline(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.online = on
}

func (f *fakeAxis) setStopErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopErr = err
}

func (f *fakeAxis) written() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]float64, len(f.targets))
	copy(out, f.targets)
	return out
}

func (f *fakeAxis) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

// newTestServer builds the REAL façade over two fakes (workers running until
// the test ends) and the server over that façade.
func newTestServer(t *testing.T, ctl config.ControlConfig, az, el *fakeAxis) *Server {
	t.Helper()
	m := mount.New(az, el, ctl, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)
	return New(m, "mockmount", LimitsFromControl(ctl), nil)
}

// waitFor polls cond until it holds or a generous deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// --- wire table (byte-pinned, pelcobridge2 TestWireTable style) ----------------------

func TestWireTable(t *testing.T) {
	tests := []struct {
		name string
		az   *fakeAxis
		el   *fakeAxis
		in   string
		want string
	}{
		{"get_pos short", fakeAt(123.4), fakeAt(45), "p\n", "123.40\n45.00\n"},
		{"get_pos long", fakeAt(0), fakeAt(90), "\\get_pos\n", "0.00\n90.00\n"},
		{"get_pos extended", fakeAt(359.99), fakeAt(0.5), "+p\n", "RPRT 0\n359.99\n0.50\n"},

		// KTD9: pre-readback is RPRT -11 on either axis, never a fabricated
		// position.
		{"get_pos az pre-readback", fakeDark(), fakeAt(45), "p\n", "RPRT -11\n"},
		{"get_pos el pre-readback", fakeAt(10), fakeDark(), "p\n", "RPRT -11\n"},

		{"set_pos ok", fakeAt(0), fakeAt(0), "P 180.5 45.0\n", "RPRT 0\n"},
		{"set_pos long", fakeAt(0), fakeAt(0), "\\set_pos 359.5 0\n", "RPRT 0\n"},
		{"set_pos comma decimal", fakeAt(0), fakeAt(0), "P 12,5 3\n", "RPRT 0\n"},

		// ParseFloat accepts "nan"/"inf"; the façade would refuse them as
		// non-finite limit targets — but the wire layer must refuse up front
		// like pelcobridge2 (KTD10), before any dispatch.
		{"set_pos garbage az", fakeAt(0), fakeAt(0), "P abc 45\n", "RPRT -1\n"},
		{"set_pos nan az", fakeAt(0), fakeAt(0), "P nan 45\n", "RPRT -1\n"},
		{"set_pos inf az", fakeAt(0), fakeAt(0), "P inf 45\n", "RPRT -1\n"},
		{"set_pos nan el", fakeAt(0), fakeAt(0), "P 180 nan\n", "RPRT -1\n"},
		{"set_pos missing el", fakeAt(0), fakeAt(0), "P 180\n", "RPRT -1\n"},

		// R10: travel-limit targets → RPRT -1.
		{"set_pos az out of limits", fakeAt(0), fakeAt(0), "P 400 45\n", "RPRT -1\n"},
		{"set_pos el out of limits", fakeAt(0), fakeAt(0), "P 180 95\n", "RPRT -1\n"},

		// R9/KTD9 (AE3): a dead axis refuses with the liveness code while the
		// other axis proceeds (asserted in TestAE3AzProceedsWhileElRefused).
		{"set_pos dead el", fakeAt(0), fakeDead(), "P 90.0 30.0\n", "RPRT -9\n"},
		{"set_pos dead az", fakeDead(), fakeAt(0), "P 90.0 30.0\n", "RPRT -9\n"},

		// Mixed refusal precedence (pinned): liveness beats limit — the dead
		// axis is the operator-facing station fault, and a partially refused
		// command reads as failure either way (KTD9).
		{"set_pos mixed limit+liveness", fakeAt(0), fakeDead(), "P 400 30\n", "RPRT -9\n"},

		{"stop", fakeAt(0), fakeAt(0), "S\n", "RPRT 0\n"},
		{"stop long", fakeAt(0), fakeAt(0), "\\stop\n", "RPRT 0\n"},

		{"get_info", fakeAt(0), fakeAt(0), "_\n", "mockmount\n"},
		{"get_info long", fakeAt(0), fakeAt(0), "\\get_info\n", "mockmount\n"},

		{"dump_state", fakeAt(0), fakeAt(0), "\\dump_state\n",
			"1\n" +
				"rot_model=901\n" +
				"min_az=0.000000\n" +
				"max_az=360.000000\n" +
				"min_el=0.000000\n" +
				"max_el=90.000000\n" +
				"south_zero=0\n" +
				"rot_type=AzEl\n" +
				"done\n"},

		{"quit", fakeAt(0), fakeAt(0), "q\n", ""},
		{"quit upper", fakeAt(0), fakeAt(0), "Q\n", ""},

		{"comment ignored", fakeAt(0), fakeAt(0), "# a comment\n", ""},
		{"empty ignored", fakeAt(0), fakeAt(0), "   \n", ""},
		{"unknown", fakeAt(0), fakeAt(0), "x\n", "RPRT -4\n"},
		{"unknown long", fakeAt(0), fakeAt(0), "\\set_conf foo bar\n", "RPRT -4\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, testControl(), tc.az, tc.el)
			got, closeConn := s.Handle(tc.in)
			if got != tc.want {
				t.Fatalf("Handle(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if tc.name == "quit" || tc.name == "quit upper" {
				if !closeConn {
					t.Fatal("q must close the connection")
				}
			} else if closeConn {
				t.Fatalf("%s must not close the connection", tc.name)
			}
		})
	}
}

// TestDumpStateConfiguredLimits pins that the advertised envelope is the
// configured travel envelope (the one refusals enforce), not a constant.
func TestDumpStateConfiguredLimits(t *testing.T) {
	ctl := config.ControlConfig{
		AZ: config.AxisControl{Min: 10, Max: 350, Deadband: 1, Park: 0},
		EL: config.AxisControl{Min: -5, Max: 85, Deadband: 1, Park: 0},
	}
	s := newTestServer(t, ctl, newFakeAxis(), newFakeAxis())
	got, _ := s.Handle("\\dump_state")
	want := "1\n" +
		"rot_model=901\n" +
		"min_az=10.000000\n" +
		"max_az=350.000000\n" +
		"min_el=-5.000000\n" +
		"max_el=85.000000\n" +
		"south_zero=0\n" +
		"rot_type=AzEl\n" +
		"done\n"
	if got != want {
		t.Fatalf("dump_state = %q, want %q", got, want)
	}
}

// --- behavioral pins beyond the reply bytes -----------------------------------------

// TestAE1GotoThenGetPos: P 180.5 45.0 → RPRT 0, both axes dispatch through the
// real façade, and a following p reports both live positions.
func TestAE1GotoThenGetPos(t *testing.T) {
	az, el := newFakeAxis(), newFakeAxis()
	s := newTestServer(t, testControl(), az, el)

	if got, _ := s.Handle("P 180.5 45.0"); got != "RPRT 0\n" {
		t.Fatalf("P reply = %q, want RPRT 0\n", got)
	}
	waitFor(t, "az write 180.5", func() bool {
		w := az.written()
		return len(w) == 1 && w[0] == 180.5
	})
	waitFor(t, "el write 45", func() bool {
		w := el.written()
		return len(w) == 1 && w[0] == 45
	})

	got, _ := s.Handle("p")
	if want := "180.50\n45.00\n"; got != want {
		t.Fatalf("p = %q, want %q", got, want)
	}
}

// TestAE3AzProceedsWhileElRefused: a dead el axis answers RPRT -9 while the
// az dispatch still happens (AE3) — and the dead axis is never written.
func TestAE3AzProceedsWhileElRefused(t *testing.T) {
	az, el := fakeAt(0), fakeDead()
	s := newTestServer(t, testControl(), az, el)

	got, _ := s.Handle("P 90.0 30.0")
	if got != "RPRT -9\n" {
		t.Fatalf("P reply = %q, want RPRT -9\n", got)
	}
	waitFor(t, "az dispatch 90 despite dead el", func() bool {
		w := az.written()
		return len(w) == 1 && w[0] == 90
	})
	if w := el.written(); len(w) != 0 {
		t.Fatalf("dead el axis was written %v, want no dispatch", w)
	}
}

// TestMixedRefusalPrecedence pins the documented deterministic precedence:
// any liveness refusal wins the single RPRT over limit refusals. The az
// target is out of limits, the el axis is dead → RPRT -9, and neither axis
// dispatches.
func TestMixedRefusalPrecedence(t *testing.T) {
	az, el := fakeAt(0), fakeDead()
	s := newTestServer(t, testControl(), az, el)

	got, _ := s.Handle("P 400 30")
	if got != "RPRT -9\n" {
		t.Fatalf("P reply = %q, want RPRT -9 (liveness precedence)", got)
	}
	// Refusals happen at admission: a refused axis can never dispatch later.
	if w := az.written(); len(w) != 0 {
		t.Fatalf("limit-refused az was written %v, want no dispatch", w)
	}
	if w := el.written(); len(w) != 0 {
		t.Fatalf("liveness-refused el was written %v, want no dispatch", w)
	}
}

// TestLimitRefusalNoSerialWrite: both axes out of limits → RPRT -1 with NO
// serial write on either axis (R10: refused before any serial write).
func TestLimitRefusalNoSerialWrite(t *testing.T) {
	az, el := fakeAt(0), fakeAt(0)
	s := newTestServer(t, testControl(), az, el)

	got, _ := s.Handle("P 400 95")
	if got != "RPRT -1\n" {
		t.Fatalf("P reply = %q, want RPRT -1\n", got)
	}
	if w := az.written(); len(w) != 0 {
		t.Fatalf("limit-refused az was written %v, want no serial write", w)
	}
	if w := el.written(); len(w) != 0 {
		t.Fatalf("limit-refused el was written %v, want no serial write", w)
	}
}

// TestStopAlwaysZero: S answers RPRT 0 even when a stop frame faults —
// pelcobridge2 parity (KTD10). The fault is logged, never surfaced as an
// error code the dialect does not define for S.
func TestStopAlwaysZero(t *testing.T) {
	az, el := fakeAt(0), fakeAt(0)
	el.setStopErr(io.ErrClosedPipe)
	az.setStopErr(io.ErrClosedPipe)
	s := newTestServer(t, testControl(), az, el)

	got, _ := s.Handle("S")
	if got != "RPRT 0\n" {
		t.Fatalf("S reply = %q, want RPRT 0\n even on stop faults", got)
	}
	waitFor(t, "az stop frame", func() bool { return az.stopCount() == 1 })
	waitFor(t, "el stop frame", func() bool { return el.stopCount() == 1 })
}

// blockingFacade parks every façade call until released — it pins the 2 s
// per-call bound (KTD10) without a real wedged serial section.
type blockingFacade struct {
	release chan struct{}
}

func (b *blockingFacade) Goto(mount.Target) []mount.Refusal {
	<-b.release
	return nil
}

func (b *blockingFacade) Stop() []mount.AxisError {
	<-b.release
	return nil
}

func (b *blockingFacade) Readback(mount.Axis) (float64, bool) {
	<-b.release
	return 0, false
}

// TestPerCallTimeout: a façade call that outlives the 2 s bound answers
// RPRT -6 for P (the KTD10 vocabulary's timeout code) and S still answers
// RPRT 0 — the client is never left hanging on a silent station.
func TestPerCallTimeout(t *testing.T) {
	b := &blockingFacade{release: make(chan struct{})}
	t.Cleanup(func() { close(b.release) }) // release the parked goroutines
	s := New(b, "blocked", LimitsFromControl(testControl()), nil)

	start := time.Now()
	got, _ := s.Handle("P 180 45")
	if got != "RPRT -6\n" {
		t.Fatalf("timed-out P = %q, want RPRT -6\n", got)
	}
	if elapsed := time.Since(start); elapsed < 2*time.Second || elapsed > 6*time.Second {
		t.Fatalf("timed-out P returned after %v, want the 2 s per-call bound", elapsed)
	}

	got, _ = s.Handle("S")
	if got != "RPRT 0\n" {
		t.Fatalf("timed-out S = %q, want RPRT 0\n", got)
	}
}

// lockedBuffer is an io.Writer safe for the slog handler goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lateFacade is a façade whose Goto parks until released, then returns
// refusals — the abandoned-call probe for the F13 late-completion pin.
type lateFacade struct {
	release chan struct{}
	refs    []mount.Refusal
}

func (b *lateFacade) Goto(mount.Target) []mount.Refusal {
	<-b.release
	return b.refs
}

func (b *lateFacade) Stop() []mount.AxisError {
	<-b.release
	return nil
}

func (b *lateFacade) Readback(mount.Axis) (float64, bool) {
	<-b.release
	return 0, false
}

// TestLateGotoCompletionLogged (F13): when a Goto outlives the bound the
// client is answered RPRT -6, but the abandoned call still completes and
// admits the motion — its late completion AND its refusals must Warn-surface,
// never be silently discarded.
func TestLateGotoCompletionLogged(t *testing.T) {
	b := &lateFacade{
		release: make(chan struct{}),
		refs: []mount.Refusal{{
			Axis:   mount.AZ,
			Reason: mount.RefusalLimit,
			Target: 400,
			Detail: "outside [0,360]",
		}},
	}
	time.AfterFunc(callTimeout+250*time.Millisecond, func() { close(b.release) })

	sink := &lockedBuffer{}
	lg := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(b, "late", LimitsFromControl(testControl()), lg)

	if got, _ := s.Handle("P 180 45"); got != "RPRT -6\n" {
		t.Fatalf("timed-out P = %q, want RPRT -6\n", got)
	}

	// The waiter drains the abandoned call and logs the late completion
	// plus the refusal it carries. Wait for BOTH lines: the waiter emits
	// them back-to-back, and stopping at the first would race the second.
	waitFor(t, "late goto completion Warn with its refusal", func() bool {
		out := sink.String()
		return strings.Contains(out, "completed late") &&
			strings.Contains(out, "late goto refusal")
	})
	out := sink.String()
	if !strings.Contains(out, "late goto refusal") || !strings.Contains(out, "reason=limit") {
		t.Errorf("late completion must surface its refusals, got:\n%s", out)
	}
	if !strings.Contains(out, "RPRT -6") || !strings.Contains(out, "admitted after RPRT -6") {
		t.Errorf("late-completion Warn must tie back to the -6 already answered, got:\n%s", out)
	}
}

// --- over-the-wire (listener) -------------------------------------------------------

// serveTCP runs a server on an ephemeral loopback port and returns its address.
func serveTCP(t *testing.T, s *Server) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.ListenAndServe(ctx, "127.0.0.1:0")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.Addr(); a != nil {
			return a.String()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("listener never came up")
	return ""
}

// readLine reads one reply line off the client side of a session.
func readLine(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	if !sc.Scan() {
		t.Fatalf("connection closed early, want a reply line: %v", sc.Err())
	}
	return sc.Text()
}

func TestOverTheWire(t *testing.T) {
	s := newTestServer(t, testControl(), fakeAt(10), fakeAt(5))
	addr := serveTCP(t, s)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Multiple commands on one connection, then quit.
	if _, err := conn.Write([]byte("S\n\\get_pos\nq\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	sc := bufio.NewScanner(conn)
	for _, w := range []string{"RPRT 0", "10.00", "5.00"} {
		if got := readLine(t, sc); got != w {
			t.Fatalf("line = %q, want %q", got, w)
		}
	}
	if sc.Scan() {
		t.Fatalf("session still open after q, got %q", sc.Text())
	}
}

func TestClientCount(t *testing.T) {
	s := newTestServer(t, testControl(), fakeAt(0), fakeAt(0))
	addr := serveTCP(t, s)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("S\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.Clients() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("clients = %d, want 1", s.Clients())
		}
		time.Sleep(5 * time.Millisecond)
	}

	conn.Close()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.Clients() != 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Clients() != 0 {
		t.Fatalf("clients = %d after close, want 0", s.Clients())
	}
}

// --- mock-mode end-to-end: the real façade into the real mock devices ----------------

// TestMockModeEndToEnd drives p/P/S/\dump_state over TCP through the REAL
// mount façade into U2's real SPID mock (driver + Rot1Prog framing + device)
// and U3's real ERC-M mock (driver + GS-232B dialect + device) — the KTD7
// mock-mode stack, no hand fakes anywhere below the façade.
func TestMockModeEndToEnd(t *testing.T) {
	ctl := config.ControlConfig{
		PollIntervalDur:   25 * time.Millisecond,
		ReopenCooldownDur: 50 * time.Millisecond,
		AZ:                config.AxisControl{Min: 0, Max: 360, Deadband: 1, Park: 0},
		EL:                config.AxisControl{Min: 0, Max: 90, Deadband: 1, Park: 0},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	azMock := spid.NewMock(spid.Opts{
		PollInterval: 25 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
	})
	t.Cleanup(azMock.Close)
	elDev := ercm.NewMock()
	elDrv := ercm.New(ercm.Config{
		PollInterval:   25 * time.Millisecond,
		ReopenCooldown: 50 * time.Millisecond,
		ReplyTimeout:   200 * time.Millisecond,
		Opener:         func() (io.ReadWriteCloser, error) { return elDev.Port(), nil },
	}, nil)

	m := mount.New(azMock, elDrv, ctl, nil)
	go m.Run(ctx)
	go azMock.RunPoll(ctx)
	go func() { _ = elDrv.Run(ctx) }()

	// Live readbacks before any command: both drivers' first polls landed.
	waitFor(t, "az readback valid", func() bool {
		_, ok := m.Readback(mount.AZ)
		return ok
	})
	waitFor(t, "el readback valid", func() bool {
		_, ok := m.Readback(mount.EL)
		return ok
	})

	s := New(m, "mock mount", LimitsFromControl(ctl), nil)
	addr := serveTCP(t, s)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	sc := bufio.NewScanner(conn)

	send := func(line string) {
		t.Helper()
		if _, err := conn.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("send %q: %v", line, err)
		}
	}

	// P 180.5 45.0 → RPRT 0 (AE1 over the whole stack).
	send("P 180.5 45.0")
	if got := readLine(t, sc); got != "RPRT 0" {
		t.Fatalf("P reply = %q, want RPRT 0", got)
	}
	// The SPID encodes whole degrees (rounds 180.5 → 181); the ERC-M moves
	// instantly on the W command — and elevation lands on the mock's AZ
	// axis (el rides the az channel, wiring swap).
	waitFor(t, "SPID commanded to 181", func() bool { return azMock.Target() == 181 })
	waitFor(t, "GS-500 at 45", func() bool { return elDev.AZ() == 45 })
	// The driver caches refresh on the next poll tick after the devices park.
	waitFor(t, "az driver cache at 181", func() bool {
		deg, ok := m.Readback(mount.AZ)
		return ok && deg == 181
	})
	waitFor(t, "el driver cache at 45", func() bool {
		deg, ok := m.Readback(mount.EL)
		return ok && deg == 45
	})

	// p → both live positions off the driver caches (byte-pinned: whole-degree
	// devices report 181.00 / 45.00).
	send("p")
	if got := readLine(t, sc); got != "181.00" {
		t.Fatalf("p az = %q, want 181.00", got)
	}
	if got := readLine(t, sc); got != "45.00" {
		t.Fatalf("p el = %q, want 45.00", got)
	}

	// \dump_state over the wire, byte-exact from the configured envelope.
	send("\\dump_state")
	want := []string{
		"1", "rot_model=901",
		"min_az=0.000000", "max_az=360.000000",
		"min_el=0.000000", "max_el=90.000000",
		"south_zero=0", "rot_type=AzEl", "done",
	}
	for _, w := range want {
		if got := readLine(t, sc); got != w {
			t.Fatalf("dump_state line = %q, want %q", got, w)
		}
	}

	// S → RPRT 0 always, and the stop frames really reach both devices: the
	// ERC-M sees an E/S line, the SPID a 13-byte stop frame (K byte 0x0F at
	// index 11 — spid.encodeStop).
	send("S")
	if got := readLine(t, sc); got != "RPRT 0" {
		t.Fatalf("S reply = %q, want RPRT 0", got)
	}
	waitFor(t, "ERC-M stop command", func() bool { return elDev.StopCount() >= 1 })
	waitFor(t, "SPID stop frame", func() bool {
		for _, f := range azMock.Writes() {
			if len(f) == 13 && f[11] == 0x0F {
				return true
			}
		}
		return false
	})

	// q closes the session.
	send("q")
	if sc.Scan() {
		t.Fatalf("session still open after q, got %q", sc.Text())
	}
}

// TestGotoCommandedPositions pins that one P dispatches exactly one intent
// per axis through the façade (two writes total, one per axis).
func TestGotoCommandedPositions(t *testing.T) {
	az, el := fakeAt(0), fakeAt(0)
	s := newTestServer(t, testControl(), az, el)

	if got, _ := s.Handle("P 179.5 12.25"); got != "RPRT 0\n" {
		t.Fatalf("P reply = %q, want RPRT 0\n", got)
	}
	waitFor(t, "both axes dispatched", func() bool {
		return len(az.written()) == 1 && len(el.written()) == 1
	})
	if w := az.written(); w[0] != 179.5 {
		t.Fatalf("az dispatch = %v, want [179.5]", w)
	}
	if w := el.written(); w[0] != 12.25 {
		t.Fatalf("el dispatch = %v, want [12.25]", w)
	}
}

// --- flood resistance (F6) ----------------------------------------------------------

// TestTransientAcceptErrorClassification pins the F6 accept-error split:
// fd exhaustion and aborted handshakes are retryable, a closed or broken
// listener is not.
func TestTransientAcceptErrorClassification(t *testing.T) {
	transient := []error{
		&net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.EMFILE)},
		&net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.ENFILE)},
		&net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.ECONNABORTED)},
	}
	for _, err := range transient {
		if !transientAcceptError(err) {
			t.Errorf("transientAcceptError(%v) = false, want true", err)
		}
	}
	permanent := []error{
		errors.New("use of closed network connection"),
		&net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.EINVAL)},
	}
	for _, err := range permanent {
		if transientAcceptError(err) {
			t.Errorf("transientAcceptError(%v) = true, want false", err)
		}
	}
}

// flakyListener scripts Accept for the retry test through the server's
// listen seam: the queued transient errors first, then one real (pipe)
// connection, then block until Close.
type flakyListener struct {
	mu     sync.Mutex
	errs   []error
	conn   net.Conn
	block  chan struct{}
	closed bool
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if len(l.errs) > 0 {
		err := l.errs[0]
		l.errs = l.errs[1:]
		l.mu.Unlock()
		return nil, err
	}
	if l.conn != nil {
		c := l.conn
		l.conn = nil
		l.mu.Unlock()
		return c, nil
	}
	l.mu.Unlock()
	<-l.block
	return nil, errors.New("flaky listener closed")
}

func (l *flakyListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.block)
	}
	return nil
}

func (l *flakyListener) Addr() net.Addr { return &net.TCPAddr{} }

// TestAcceptRetriesTransientErrors (F6): two transient accept errors must
// not kill ListenAndServe — the loop pauses, retries, and still serves the
// next session; only ctx-done ends it.
func TestAcceptRetriesTransientErrors(t *testing.T) {
	serverEnd, clientEnd := net.Pipe()
	t.Cleanup(func() { _ = clientEnd.Close() })
	l := &flakyListener{
		errs: []error{
			&net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.EMFILE)},
			&net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.ECONNABORTED)},
		},
		conn:  serverEnd,
		block: make(chan struct{}),
	}

	s := newTestServer(t, testControl(), fakeAt(0), fakeAt(0))
	s.listen = func(string, string) (net.Listener, error) { return l, nil }
	s.acceptRetryPause = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- s.ListenAndServe(ctx, "unused") }()

	// The loop survived both transient errors and served the session anyway.
	waitFor(t, "session served despite transient accept errors", func() bool {
		return s.Clients() == 1
	})

	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("ListenAndServe returned %v, want the ctx error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ListenAndServe did not return after ctx cancel")
	}
}

// TestSessionCapRefusesFlood (F6): past the session cap a new connection is
// accepted then immediately closed, and neither the count nor the existing
// sessions are disturbed.
func TestSessionCapRefusesFlood(t *testing.T) {
	s := newTestServer(t, testControl(), fakeAt(0), fakeAt(0))
	addr := serveTCP(t, s)

	var held []net.Conn
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	for i := 0; i < s.maxClients; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		held = append(held, c)
	}
	waitFor(t, "all session slots taken", func() bool {
		return s.Clients() == s.maxClients
	})

	// The flood connection beyond the cap: the server closes it promptly.
	extra, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial extra: %v", err)
	}
	defer extra.Close()
	_ = extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(extra); err != nil {
		t.Fatalf("reading the refused connection: %v", err)
	}
	if got := s.Clients(); got != s.maxClients {
		t.Fatalf("clients = %d after refusing the extra connection, want %d", got, s.maxClients)
	}

	// The held sessions are untouched: one still gets its replies.
	if _, err := held[0].Write([]byte("_\n")); err != nil {
		t.Fatalf("send on held session: %v", err)
	}
	sc := bufio.NewScanner(held[0])
	if got := readLine(t, sc); got != "mockmount" {
		t.Fatalf("held session reply = %q, want %q", got, "mockmount")
	}
}

// TestIdleSessionReaped (F6): a connected client that never sends a command
// line is closed by the per-command read deadline instead of squatting on a
// session slot forever.
func TestIdleSessionReaped(t *testing.T) {
	s := newTestServer(t, testControl(), fakeAt(0), fakeAt(0))
	s.readIdleTimeout = 250 * time.Millisecond
	addr := serveTCP(t, s)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	waitFor(t, "idle client counted", func() bool { return s.Clients() == 1 })

	// The server side must close within the shrunk idle grace.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("idle session was not reaped: %v", err)
	}
	waitFor(t, "reaped client uncounted", func() bool { return s.Clients() == 0 })
}

// The 2026-09-23 incident: a rotctld client repositioned the mount with no
// Info trace at all. Connects and the motion commands (P/S) must log at
// Info, keyed to the remote; polls (`p`) must not — a 1 Hz gpredict poll
// loop would drown the journal.
func TestMotionCommandsLogAtInfo(t *testing.T) {
	sink := &lockedBuffer{}
	lg := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctl := testControl()
	m := mount.New(fakeAt(10), fakeAt(5), ctl, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go m.Run(ctx)
	s := New(m, "mockmount", LimitsFromControl(ctl), lg)
	addr := serveTCP(t, s)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("p\nP 180 45\nS\nq\n")); err != nil {
		t.Fatalf("send: %v", err)
	}
	sc := bufio.NewScanner(conn)
	for _, w := range []string{"10.00", "5.00", "RPRT 0", "RPRT 0"} {
		if got := readLine(t, sc); got != w {
			t.Fatalf("reply = %q, want %q", got, w)
		}
	}
	waitFor(t, "stop cmd logged", func() bool {
		return strings.Count(sink.String(), "cmd=stop") == 1
	})

	out := sink.String()
	if !strings.Contains(out, "rotctld: client connected") || !strings.Contains(out, "remote=") {
		t.Errorf("connect must log at Info with the remote, got:\n%s", out)
	}
	if !strings.Contains(out, "cmd=goto az=180 el=45") {
		t.Errorf("goto must log its targets at Info, got:\n%s", out)
	}
	if got := strings.Count(out, "rotctld: cmd"); got != 2 {
		t.Errorf("cmd log lines = %d, want 2 (goto+stop; the p poll must stay silent):\n%s", got, out)
	}
}
