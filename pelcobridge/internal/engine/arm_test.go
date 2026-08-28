package engine

import (
	"strings"
	"testing"
	"time"

	"pelcots/internal/config"
	"pelcots/internal/control"
)

// The arm-gate tests cover the safety workflow: the engine always starts
// disarmed, network-originated motion (rotctld setpos) is refused until Arm,
// Stop always passes, and the calibration primitives (GotoZero to PHYSICAL 0°
// with a wind re-zero; SetAzimuthTrue offset entry) behave as documented. The
// local TUI path (Jog/Goto/StopMotion) is deliberately NOT gated.

// txLines returns the TX lines of the engine trace (log_level debug records
// one line per sent frame).
func txLines(eng *Engine) []string {
	var out []string
	for _, l := range eng.Snapshot().Log {
		if strings.Contains(l, "TX ") {
			out = append(out, l)
		}
	}
	return out
}

func hasTxLine(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// TestDisarmedRefusesNetworkMotion: while disarmed a rotctld setpos produces
// no motion frames and the goto never arms; a Stop still passes (it is always
// safe).
func TestDisarmedRefusesNetworkMotion(t *testing.T) {
	r := newPelcoResponder(1)
	defer r.close()

	eng := New(Options{
		Transport: config.TransportTCP,
		TCPAddr:   r.ln.Addr().String(),
		Addr:      1,
		LogLevel:  config.LogInfo,
	})
	eng.Start()
	defer eng.Close()
	connectedWithReadback(t, eng)

	if eng.Snapshot().Armed {
		t.Fatal("engine started armed — armed state must never persist or default on")
	}

	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 100, El: 20, Source: "rotctld"})
	time.Sleep(300 * time.Millisecond)
	if s := eng.Snapshot(); s.Gotoing {
		t.Fatal("disarmed setpos armed a goto")
	}
	if _, _, sets := r.nth(0).snapshot(); sets != 0 {
		t.Errorf("disarmed setpos sent %d set frames — network motion must be refused", sets)
	}

	// Stop passes the gate.
	eng.Submit(control.Command{Kind: control.KindStop, Source: "rotctld"})
	if stops, _, _ := waitForStops(r.nth(0), 1, time.Second); stops == 0 {
		t.Error("Stop was refused while disarmed — stop must always pass")
	}
}

// TestArmUnlocksNetworkMotion: after Arm, the same setpos moves the unit.
func TestArmUnlocksNetworkMotion(t *testing.T) {
	r := newPelcoResponder(1)
	defer r.close()

	eng := New(Options{
		Transport: config.TransportTCP,
		TCPAddr:   r.ln.Addr().String(),
		Addr:      1,
		LogLevel:  config.LogInfo,
	})
	eng.Start()
	defer eng.Close()
	connectedWithReadback(t, eng)

	eng.Arm()
	if !waitFor(time.Second, func() bool { return eng.Snapshot().Armed }) {
		t.Fatal("Arm did not take effect")
	}

	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 100, El: 20, Source: "rotctld"})
	if !waitFor(time.Second, func() bool { return eng.Snapshot().Gotoing }) {
		t.Fatalf("armed setpos did not arm a goto: status=%q", eng.Snapshot().Status)
	}
	if !waitFor(time.Second, func() bool { _, _, sets := r.nth(0).snapshot(); return sets >= 1 }) {
		t.Error("armed setpos sent no set frames")
	}
}

// TestAutoArmArmsAtStart covers the auto_arm option for headless/sim use.
func TestAutoArmArmsAtStart(t *testing.T) {
	eng := New(Options{Transport: config.TransportSim, Addr: 1, AutoArm: true})
	eng.Start()
	defer eng.Close()
	if !waitFor(time.Second, func() bool { return eng.Snapshot().Armed }) {
		t.Fatal("auto_arm did not arm the engine at start")
	}
}

// TestGotoZeroSendsPhysicalZeroAndRezerosWind covers the calibration goto-0:
// it targets the unit's PHYSICAL 0° (bypassing the azimuth offset — the frame
// carries word 0x0000, not the offset-shifted azimuth) and re-zeroes the
// cable-wind accumulator when the readback converges there.
func TestGotoZeroSendsPhysicalZeroAndRezerosWind(t *testing.T) {
	eng := New(Options{
		Transport:    config.TransportSim,
		Addr:         1,
		Sim:          config.SimConfig{StartPan: 100, StartTilt: 0},
		AzOffset:     50, // logical 0° = physical 50° — must NOT be applied by goto-0
		WrapEnabled:  true,
		WrapLimit:    270,
		WrapAccum:    200,
		PollInterval: 20 * time.Millisecond,
		LogLevel:     config.LogDebug, // per-frame TX lines
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return s.Connected && s.HavePan && s.HaveTilt
	}) {
		t.Fatalf("no readback: %+v", eng.Snapshot())
	}

	eng.GotoZero()
	// CurPan is ambiguous under the offset (logical 50° both before and after),
	// so settle on the RAW word: physical 0° reads back as word 0x0000, and the
	// wind accumulator must have been re-zeroed by the cal-zero arrival.
	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return s.CurPanRaw == 0 && !s.Gotoing && s.Wrap == 0
	}) {
		s := eng.Snapshot()
		t.Fatalf("goto-0 did not settle at physical 0° with a re-zeroed wind: raw=0x%04X pan=%.2f wrap=%+.1f gotoing=%v status=%q",
			s.CurPanRaw, s.CurPan, s.Wrap, s.Gotoing, s.Status)
	}

	// The wire must carry SetPan with word 0x0000 (physical zero) — and never
	// the offset-shifted azimuth 50° (word 0x1388).
	tx := txLines(eng)
	if !hasTxLine(tx, "TX  FF 01 00 4B 00 00 4C") {
		t.Errorf("no SetPan(0x0000) frame on the wire:\n%s", strings.Join(tx, "\n"))
	}
	if hasTxLine(tx, "TX  FF 01 00 4B 13 88") {
		t.Errorf("goto-0 sent the offset-shifted azimuth 50° (word 0x1388) — it must bypass azOffset:\n%s",
			strings.Join(tx, "\n"))
	}
}

// TestSetAzimuthTrueMapsGotos covers the offset-entry calibration: after
// telling the engine the antenna actually points at true azimuth 40° (while
// physically at 100°), a network goto to true 10° drives the unit to physical
// 70° and reports logical 10°.
func TestSetAzimuthTrueMapsGotos(t *testing.T) {
	eng := New(Options{
		Transport:    config.TransportSim,
		Addr:         1,
		Sim:          config.SimConfig{StartPan: 100, StartTilt: 0},
		AutoArm:      true, // the goto arrives over the network (Submit)
		PollInterval: 20 * time.Millisecond,
		LogLevel:     config.LogDebug, // per-frame TX lines
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return s.Connected && s.HavePan && s.HaveTilt && s.CurPan == 100
	}) {
		t.Fatalf("no readback at start 100°: %+v", eng.Snapshot())
	}

	eng.SetAzimuthTrue(40) // physical 100° now reads as logical 40°
	if !waitFor(time.Second, func() bool { return eng.Snapshot().AzOffset == 60 }) {
		s := eng.Snapshot()
		t.Fatalf("offset not set: az_offset=%.2f status=%q", s.AzOffset, s.Status)
	}

	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 10, El: 0})
	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return !s.Gotoing && s.CurPan == 10
	}) {
		s := eng.Snapshot()
		t.Fatalf("goto after offset entry failed: pan=%.2f gotoing=%v status=%q", s.CurPan, s.Gotoing, s.Status)
	}
	// The wire must have carried physical 70° (word 0x1B58), not logical 10°.
	if !hasTxLine(txLines(eng), "TX  FF 01 00 4B 1B 58") {
		t.Errorf("no SetPan(physical 70°) frame on the wire:\n%s", strings.Join(txLines(eng), "\n"))
	}
}
