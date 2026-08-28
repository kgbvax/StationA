package engine

import (
	"testing"
	"time"

	"pelcots/internal/config"
	"pelcots/internal/control"
	"pelcots/internal/pelco"
	"pelcots/internal/serialio"
)

// TestGotoSendsAbsoluteSets asserts the goto drive model end to end: a network
// setpos produces absolute SetPan/SetTilt opcodes (0x4B/0x4D) on the wire and
// NO jog frames — the unit slews itself after the absolute sets; the engine
// only polls readback. (The device honors 0x4B/0x4D — confirmed on the bench
// 2026-08-28; the earlier closed-loop-jog goto was built on a misread. The
// responder answers from a fixed position, so the goto never converges and
// stays "seeking" until the safety timeout — long enough to watch the frames.)
func TestGotoSendsAbsoluteSets(t *testing.T) {
	r := newPelcoResponder(1)
	defer r.close()

	eng := New(Options{
		Transport:    config.TransportTCP,
		TCPAddr:      r.ln.Addr().String(),
		Addr:         1,
		PollInterval: 30 * time.Millisecond,
		Goto:         config.GotoConfig{TimeoutSec: 0.5},
		AutoArm:      true, // the move arrives over the network (Submit)
	})
	eng.Start()
	defer eng.Close()
	connectedWithReadback(t, eng)

	st := r.nth(0)
	if st == nil {
		t.Fatal("no connection recorded")
	}
	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 100, El: 20})
	if !waitFor(time.Second, func() bool { return eng.Snapshot().Gotoing }) {
		t.Fatalf("goto never armed: %+v", eng.Snapshot())
	}
	if !waitFor(time.Second, func() bool { _, _, sets := st.snapshot(); return sets >= 1 }) {
		t.Fatalf("no SetPan/SetTilt frames reached the unit: status=%q", eng.Snapshot().Status)
	}
	// Let the never-converging goto run past a few more poll ticks.
	time.Sleep(200 * time.Millisecond)
	_, jogs, sets := st.snapshot()
	if sets < 1 {
		t.Errorf("goto sent no absolute set frames")
	}
	if jogs != 0 {
		t.Errorf("goto drove the unit with jog frames (%d) — absolute sets are the only goto path", jogs)
	}
}

// TestTiltDecodeIsTextbook pins the tilt readback decode: a Pelco-D tilt
// response word is elevation in hundredths of a degree, decoded exactly like
// pan (bench-confirmed 2026-08-28; the earlier raw-encoder calibration model
// was a misread and has been deleted).
func TestTiltDecodeIsTextbook(t *testing.T) {
	eng := New(Options{Transport: config.TransportSim, Addr: 1})
	// Feed a tilt response frame directly (never started: no actor loop needed;
	// handleFrame only touches readback state, pos, and logging).
	eng.handleFrame(serialio.Event{Frame: pelco.TiltResponse(1, 45)}, true)
	if !eng.haveTilt || eng.curTilt != 45 {
		t.Fatalf("tilt decode: haveTilt=%v curTilt=%.2f, want exactly 45", eng.haveTilt, eng.curTilt)
	}
	eng.handleFrame(serialio.Event{Frame: pelco.TiltResponse(1, 12.34)}, true)
	if eng.curTilt != 12.34 {
		t.Fatalf("tilt decode: curTilt=%.2f, want 12.34 (hundredths of a degree)", eng.curTilt)
	}
}
