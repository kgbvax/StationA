package engine

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"pelcots/internal/config"
	"pelcots/internal/control"
)

// These are the regression tests for the runaway-motion class of bug: a path
// that leaves the engine's motion flags saying "stopped" (or lets them stay
// "moving") without the matching Pelco-D frame reaching the unit. Pelco-D Jog
// latches — there is no auto-stop — so every such path leaves a physical
// rotator slewing until it hits a mechanical stop or winds the cable off.
//
// They all drive the engine against pelcoResponder (reconnect_test.go), which
// answers position queries from a FIXED 0°/0° and never advances on Jog, and
// which counts the Stop and Jog frames that landed on each connection.

// waitForStops polls until the connection has recorded at least want Stop
// frames, so the assertions do not race the actor goroutine's write.
func waitForStops(st *pelcoConnStats, want int, timeout time.Duration) (stops, jogs int) {
	waitFor(timeout, func() bool {
		s, _ := st.snapshot()
		return s >= want
	})
	return st.snapshot()
}

// connectedWithReadback brings an engine up against r and waits until both axes
// have readback (the closed-loop goto refuses to arm without it).
func connectedWithReadback(t *testing.T, eng *Engine) {
	t.Helper()
	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return s.Connected && s.HavePan && s.HaveTilt
	}) {
		t.Fatalf("engine never got pan/tilt readback: %+v", eng.Snapshot())
	}
}

// TestAtTargetGotoStopsLatchedMotion covers beginGoto's "already at the target
// on both axes" exit. It is reached with motion latched — here a jog, but
// equally a goto that a tracking update retargets onto the position the unit
// has just reached — and before the fix it cleared jogging/gotoing and returned
// without sending Stop, so poll() stopped sending the keepalive, no goto
// remained to time out, and the unit slewed on indefinitely.
func TestAtTargetGotoStopsLatchedMotion(t *testing.T) {
	r := newPelcoResponder(1)
	defer r.close()

	eng := New(Options{
		Transport:    config.TransportTCP,
		TCPAddr:      r.ln.Addr().String(),
		Addr:         1,
		PollInterval: 40 * time.Millisecond,
		LogLevel:     config.LogInfo,
	})
	eng.Start()
	defer eng.Close()
	connectedWithReadback(t, eng)

	st := r.nth(0)
	if st == nil {
		t.Fatal("no connection recorded")
	}
	// Latch motion, and confirm the unit really is being driven.
	eng.Jog(1, 0, false) // continuous jog (the local path; the unit latches)
	if !waitFor(time.Second, func() bool { return eng.Snapshot().Jogging }) {
		t.Fatal("jog was not latched")
	}
	if !waitFor(time.Second, func() bool { _, jogs := st.snapshot(); return jogs > 0 }) {
		t.Fatal("no jog frames reached the unit")
	}
	stopsBefore, _ := st.snapshot()

	// A target the unit is already at: the responder reads a fixed 0°/0°, so
	// both axes are inside their deadband and no new move is needed.
	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 0, El: 0, Source: "rotctld"})

	if !waitFor(time.Second, func() bool {
		s := eng.Snapshot()
		return !s.Jogging && !s.Gotoing
	}) {
		t.Fatalf("motion flags not cleared: %+v", eng.Snapshot())
	}
	stopsAfter, _ := waitForStops(st, stopsBefore+1, time.Second)
	if stopsAfter <= stopsBefore {
		t.Errorf("at-target goto cleared the motion flags without sending Stop "+
			"(stops %d -> %d): the unit is still slewing", stopsBefore, stopsAfter)
	}
}

// TestStreamingTargetsDoNotDefeatGotoTimeout covers the goto stall watchdog.
// gotoDeadline is the only backstop against a jog that never converges, and it
// used to be re-armed by every beginGoto — so a tracker streaming a new target
// every second or two (the normal case during a pass) rolled it forward
// forever. With readback collapsed, as here, the unit jogged for the whole pass.
// The deadline is now armed at motion start and pushed out only by OBSERVED
// travel, which this responder never produces.
func TestStreamingTargetsDoNotDefeatGotoTimeout(t *testing.T) {
	r := newPelcoResponder(1)
	defer r.close()

	eng := New(Options{
		Transport:    config.TransportTCP,
		TCPAddr:      r.ln.Addr().String(),
		Addr:         1,
		PollInterval: 30 * time.Millisecond,
		Goto:         config.GotoConfig{TimeoutSec: 0.4},
		LogLevel:     config.LogInfo,
	})
	eng.Start()
	defer eng.Close()
	connectedWithReadback(t, eng)

	st := r.nth(0)
	if st == nil {
		t.Fatal("no connection recorded")
	}
	// Arm the first move and confirm the unit really is being driven. The
	// responder never advances on Jog, so nothing ever converges: this stands in
	// for a jammed axis or the readback collapse the 303Z/3050DZ exhibits.
	var submitted atomic.Int64
	az := 100.0
	eng.Submit(control.Command{Kind: control.KindSetPos, Az: az, El: 0, Source: "rotctld"})
	submitted.Add(1)
	if !waitFor(time.Second, func() bool { return eng.Snapshot().Gotoing }) {
		t.Fatalf("goto never armed: %+v", eng.Snapshot())
	}
	if !waitFor(time.Second, func() bool { _, jogs := st.snapshot(); return jogs > 0 }) {
		t.Fatal("no jog frames reached the unit (test setup broken)")
	}
	// Every expiry sends an all-stop, and on this responder expireGoto is the
	// ONLY source of one (no axis ever converges, no target is ever already
	// reached), so counting Stop frames counts timeouts. Polling Gotoing instead
	// would race the immediate re-arm by the next streamed target.
	stopsBefore, _ := st.snapshot()
	deadline := time.Now().Add(1500 * time.Millisecond) // ~3.5x the goto timeout
	for time.Now().Before(deadline) {
		az += 0.5 // a drifting target, as a tracker produces
		eng.Submit(control.Command{Kind: control.KindSetPos, Az: az, El: 0, Source: "rotctld"})
		submitted.Add(1)
		time.Sleep(60 * time.Millisecond)
	}
	stopsAfter, _ := st.snapshot()
	if stopsAfter-stopsBefore < 2 {
		t.Errorf("goto expired %d times across %d streamed targets over %v (timeout %v; expected ~3): "+
			"the safety backstop is defeated and the unit jogs for the whole pass",
			stopsAfter-stopsBefore, submitted.Load(), 1500*time.Millisecond, 400*time.Millisecond)
	}
}

// TestStalledGotoStillExpires is the companion guard: the watchdog change must
// not have made the timeout unreachable for a single non-converging goto.
func TestStalledGotoStillExpires(t *testing.T) {
	r := newPelcoResponder(1)
	defer r.close()

	eng := New(Options{
		Transport:    config.TransportTCP,
		TCPAddr:      r.ln.Addr().String(),
		Addr:         1,
		PollInterval: 30 * time.Millisecond,
		Goto:         config.GotoConfig{TimeoutSec: 0.3},
	})
	eng.Start()
	defer eng.Close()
	connectedWithReadback(t, eng)

	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 100, El: 0})
	if !waitFor(time.Second, func() bool { return eng.Snapshot().Gotoing }) {
		t.Fatalf("goto never armed: %+v", eng.Snapshot())
	}
	if !waitFor(2*time.Second, func() bool { return !eng.Snapshot().Gotoing }) {
		t.Fatal("a stalled goto never expired — the safety timeout is unreachable")
	}
	if s := eng.Snapshot(); s.Status == "" {
		t.Error("expected a timeout status after expiry")
	}
}

// TestGotoArmedWhileDownDoesNotResumeOnReconnect covers the other way a stale
// closed-loop reached the wire. linkFailed clears motion state when a live link
// dies (TestUnwrapDroppedLinkNotResumed), but a *failed connect* — an explicit
// Reconnect to a target that is down, or a first connect that never succeeded —
// left havePan/haveTilt set. startGoto's readback gate therefore let an inbound
// move arm a full closed-loop goto while disconnected, whose frames were
// dropped by send(); when the link returned, poll() resumed the jog keepalive
// and drove the unit toward a target derived from a stale position.
func TestGotoArmedWhileDownDoesNotResumeOnReconnect(t *testing.T) {
	r := newPelcoResponder(1)
	defer r.close()
	live := r.ln.Addr().String()

	// A dead target: bind a port, learn its address, release it.
	ln0, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	dead := ln0.Addr().String()
	_ = ln0.Close()

	eng := New(Options{
		Transport:    config.TransportTCP,
		TCPAddr:      live,
		Addr:         1,
		PollInterval: 40 * time.Millisecond,
		Goto:         config.GotoConfig{TimeoutSec: 60},
		LogLevel:     config.LogInfo,
	})
	eng.Start()
	defer eng.Close()
	connectedWithReadback(t, eng)

	// Explicit reconnect (TUI 'r'/'m') to a target that is down.
	eng.Reconnect(ConnSpec{Transport: config.TransportTCP, TCPAddr: dead})
	if !waitFor(2*time.Second, func() bool { return !eng.Snapshot().Connected }) {
		t.Fatalf("still connected after reconnecting to a dead address: %+v", eng.Snapshot())
	}
	// The position is no longer known, so an inbound move must be refused.
	if s := eng.Snapshot(); s.HavePan || s.HaveTilt {
		t.Errorf("readback still considered valid while disconnected: HavePan=%v HaveTilt=%v",
			s.HavePan, s.HaveTilt)
	}
	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 200, El: 30, Source: "rotctld"})
	time.Sleep(150 * time.Millisecond)
	if eng.Snapshot().Gotoing {
		t.Error("a goto was armed against a stale position while the link was down")
	}

	// Back to the live bridge: nothing may resume on the fresh connection.
	before := r.count()
	eng.Reconnect(ConnSpec{Transport: config.TransportTCP, TCPAddr: live})
	if !waitFor(2*time.Second, func() bool {
		return eng.Snapshot().Connected && r.count() > before
	}) {
		t.Fatalf("did not reconnect to the live bridge (conns=%d)", r.count())
	}
	st := r.nth(before)
	if st == nil {
		t.Fatal("no stats for the fresh connection")
	}
	stops, _ := waitForStops(st, 1, time.Second)
	time.Sleep(200 * time.Millisecond) // let a few polls run
	stops, jogs := st.snapshot()
	if stops == 0 {
		t.Error("no Stop on the fresh link — a unit still slewing from before is not halted")
	}
	if jogs != 0 {
		t.Errorf("stale goto resumed on the fresh link (jogs=%d): the unit is being driven "+
			"toward a target computed from a position read before the outage", jogs)
	}
}

// TestCloseSendsAllStopBeforeReturning covers the shutdown race. Close used to
// queue the cleanup and return immediately, so main could exit the process
// before the actor goroutine wrote the all-stop — on SIGTERM mid-move that left
// the rotator slewing with nothing left running to halt it.
func TestCloseSendsAllStopBeforeReturning(t *testing.T) {
	r := newPelcoResponder(1)
	defer r.close()

	eng := New(Options{
		Transport:    config.TransportTCP,
		TCPAddr:      r.ln.Addr().String(),
		Addr:         1,
		PollInterval: 40 * time.Millisecond,
		LogLevel:     config.LogInfo,
	})
	eng.Start()
	connectedWithReadback(t, eng)

	st := r.nth(0)
	if st == nil {
		t.Fatal("no connection recorded")
	}
	eng.Jog(1, 0, false) // continuous jog (the local path; the unit latches)
	if !waitFor(time.Second, func() bool { _, jogs := st.snapshot(); return jogs > 0 }) {
		t.Fatal("no jog frames reached the unit")
	}
	stopsBefore, _ := st.snapshot()

	_ = eng.Close() // must not return until the all-stop is on the wire

	// shutdown() publishes as its last act, so a snapshot that still says
	// "connected" proves Close returned while the teardown — and therefore the
	// all-stop — was still pending. That is the ordering guarantee; the frame
	// then landing on the responder is asynchronous, hence the bounded wait.
	if eng.Snapshot().Connected {
		t.Error("Close returned before the actor tore the link down: on SIGTERM the process " +
			"would exit with the all-stop unsent and the unit still slewing")
	}
	stopsAfter, _ := waitForStops(st, stopsBefore+1, time.Second)
	if stopsAfter <= stopsBefore {
		t.Errorf("no shutdown all-stop reached the port (stops %d -> %d): "+
			"a process exit here leaves the unit slewing", stopsBefore, stopsAfter)
	}
	_ = eng.Close() // idempotent, and must not block
}

// TestCloseDoesNotBlockOnFullRequestQueue covers the other half of the shutdown
// fix. Close used to signal shutdown by queueing a cleanup request, so once the
// 64-deep queue filled behind a wedged actor, Close blocked forever and SIGTERM
// hung until systemd's SIGKILL — with no all-stop ever sent. An engine that is
// constructed but never Started stands in for a wedged actor: nothing drains
// reqs.
func TestCloseDoesNotBlockOnFullRequestQueue(t *testing.T) {
	eng := New(Options{Transport: config.TransportSim, Addr: 1})
	for i := 0; i < 128; i++ { // overfill the 64-slot queue
		go eng.Submit(control.Command{Kind: control.KindSetPos, Az: float64(i % 360), El: 0})
	}
	time.Sleep(100 * time.Millisecond) // let the queue fill

	done := make(chan struct{})
	go func() {
		_ = eng.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a full request queue — SIGTERM would hang with no all-stop sent")
	}
}
