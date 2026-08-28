package engine

import (
	"math"
	"testing"
	"time"

	"pelcots/internal/config"
	"pelcots/internal/control"
	"pelcots/internal/pelco"
	"pelcots/internal/serialio"
)

// simSlewRate returns the in-memory simulator's effective tilt slew rate in
// deg/s: the sim steps JogStep degrees per jog frame, and the engine re-sends
// the jog keepalive once per poll, so the rate is JogStep / pollInterval. The
// open-loop slew deadline is planned against this rate so the blind halt
// lands near the target; the safety-bias undershoot and the correction loop
// close the rest, so an exact match is not required.
func simSlewRate(jogStep float64) float64 {
	return jogStep / simPollInterval.Seconds()
}

// TestGotoTiltWildFrameRecovery reproduces the live 303Z/3050DZ failure: while
// the tilt motor runs the head emits a constant valid-checksum garbage stream
// (never the true position), so a closed-loop tilt goto that watches readback
// while slewing never converges — the garbage is accepted as position and the
// loop derails (there is no longer a wild-frame filter to reject it; a filter
// was the wrong answer anyway, since it would also reject the first true
// post-halt readback and hang). The fix is open-loop slew (jog blind, timed by
// a configured rate, readback ignored while moving) then halt-and-confirm
// against the stable idle readback with a bounded correction loop. The
// simulator injects the garbage (WildTiltWhileMoving); the test sets
// tilt_slew_rate to the sim's discrete jog rate so the open-loop deadline
// predicts the slew, and asserts the tilt goto converges through the garbage.
//
// Tilt-only (azimuth already at target) so the poll queries tilt every tick
// for a fast confirm — the simplest path through the fix.
func TestGotoTiltWildFrameRecovery(t *testing.T) {
	const jogStep = 2.0
	eng := New(Options{
		Transport:    config.TransportSim,
		Addr:         1,
		Sim:          config.SimConfig{StartPan: 0, StartTilt: 0, JogStep: jogStep, WildTiltWhileMoving: 190},
		PollInterval: simPollInterval,
		Goto: config.GotoConfig{
			TiltSlewRate: simSlewRate(jogStep),
		},
		LogLevel: config.LogDebug,
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return s.Connected && s.HavePan && s.HaveTilt
	}) {
		t.Fatalf("no readback before goto: %+v", eng.Snapshot())
	}

	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 0, El: 45})

	if !waitFor(5*time.Second, func() bool {
		s := eng.Snapshot()
		return s.HaveTilt && !s.Gotoing && closeEnough(s.CurTilt, 45)
	}) {
		s := eng.Snapshot()
		t.Fatalf("open-loop tilt goto did not converge through the garbage stream: tilt=%.2f gotoing=%v status=%q\nlog=%v",
			s.CurTilt, s.Gotoing, s.Status, s.Log)
	}
}

// TestGotoTiltWildTwoAxis is the realistic sat-tracking scenario: a goto
// commands BOTH azimuth and elevation, with the tilt readback garbage while
// the tilt motor runs. Pan runs the legacy closed loop (clean pan readback —
// the garbage injector is tilt-only); tilt runs the open-loop slew. The two
// axes share the alternating pan/tilt query cadence while both move, then tilt
// gets every tick once pan settles. Asserts both axes reach the target — the
// path the PstRotator / sat-tracking integration drives on the real head.
func TestGotoTiltWildTwoAxis(t *testing.T) {
	const jogStep = 2.0
	eng := New(Options{
		Transport:    config.TransportSim,
		Addr:         1,
		Sim:          config.SimConfig{StartPan: 0, StartTilt: 0, JogStep: jogStep, WildTiltWhileMoving: 190},
		PollInterval: simPollInterval,
		Goto: config.GotoConfig{
			TiltSlewRate: simSlewRate(jogStep),
		},
		LogLevel: config.LogDebug,
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return s.Connected && s.HavePan && s.HaveTilt
	}) {
		t.Fatalf("no readback before goto: %+v", eng.Snapshot())
	}

	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 180, El: 45})

	if !waitFor(6*time.Second, func() bool {
		s := eng.Snapshot()
		return s.HavePan && s.HaveTilt && !s.Gotoing &&
			closeEnough(s.CurPan, 180) && closeEnough(s.CurTilt, 45)
	}) {
		s := eng.Snapshot()
		t.Fatalf("two-axis open-loop goto did not converge: pan=%.2f tilt=%.2f gotoing=%v status=%q\nlog=%v",
			s.CurPan, s.CurTilt, s.Gotoing, s.Status, s.Log)
	}
}

// TestGotoTiltOpenLoopClean sanity-checks that the open-loop slew path also
// converges when the readback is clean (no garbage injector) — e.g. if
// tilt_slew_rate is set on a head that does not emit the garbage stream. The
// readback is ignored during the slew either way; the confirm reads the clean
// idle position after the halt. Asserts the open-loop path is not specific to
// the garbage case.
func TestGotoTiltOpenLoopClean(t *testing.T) {
	const jogStep = 2.0
	eng := New(Options{
		Transport:    config.TransportSim,
		Addr:         1,
		Sim:          config.SimConfig{StartPan: 0, StartTilt: 0, JogStep: jogStep},
		PollInterval: simPollInterval,
		Goto: config.GotoConfig{
			TiltSlewRate: simSlewRate(jogStep),
		},
		LogLevel: config.LogDebug,
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return s.Connected && s.HavePan && s.HaveTilt
	}) {
		t.Fatalf("no readback before goto: %+v", eng.Snapshot())
	}

	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 0, El: 45})

	if !waitFor(5*time.Second, func() bool {
		s := eng.Snapshot()
		return s.HaveTilt && !s.Gotoing && closeEnough(s.CurTilt, 45)
	}) {
		s := eng.Snapshot()
		t.Fatalf("open-loop tilt goto did not converge on clean readback: tilt=%.2f gotoing=%v status=%q\nlog=%v",
			s.CurTilt, s.Gotoing, s.Status, s.Log)
	}
}

// TestNoWildFrameFilter asserts the wild-frame rejection filter has been
// removed: a tilt readback that jumps an arbitrary distance from the last
// accepted position is accepted, not rejected. The filter was never requested
// and mis-rejected good frames on the 303Z/3050DZ (notably: tilt_wild_deg: 0 in
// the config was silently raised to 30, so users who thought they had disabled
// it still saw "wild frame rejected" lines). The open-loop slew path handles
// the device's garbage-while-moving readback by ignoring it during motion; a
// gross-jump filter on idle readback is not wanted. A 90° jump is used because
// it exceeds the old 30° threshold, so it would have been rejected before the
// removal. handleFrame is called directly (the run goroutine is not started)
// so the assertion is single-threaded and deterministic.
func TestNoWildFrameFilter(t *testing.T) {
	eng := New(Options{Transport: config.TransportSim, Addr: 1}) // standard decode (offset 0, scale 100)
	eng.haveTilt = true
	eng.curTilt = 0

	// A tilt response encoding 90° — a 90° jump from 0. Before the filter's
	// removal (tiltWild defaulted to 30) this was rejected and curTilt froze.
	eng.handleFrame(serialio.Event{Frame: pelco.TiltResponse(1, 90)}, true)

	if math.Abs(eng.curTilt-90) > 1e-6 {
		t.Fatalf("a large tilt jump must be accepted now that the wild-frame filter is removed: curTilt=%.3f, want 90", eng.curTilt)
	}
}
