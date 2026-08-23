package engine

import (
	"strings"
	"testing"
	"time"

	"pelcots/internal/config"
	"pelcots/internal/control"
)

// TestSimModeGoto starts the engine against the in-memory simulator (no
// hardware), commands an absolute move through the same Submit path the
// inbound control servers use, and asserts the engine reports the commanded
// position once its poll/readback loop converges. This is the loop the
// GS-232 / rotctld / PstRotator integrations exercise.
func TestSimModeGoto(t *testing.T) {
	eng := New(Options{
		Transport: config.TransportSim,
		Addr:      1,
		Sim:       config.SimConfig{StartPan: 0, StartTilt: 0, JogStep: 5},
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool { return eng.Snapshot().Connected }) {
		t.Fatal("engine did not connect to the simulator")
	}

	// Command an absolute move exactly as gs232 "W 180 45" would.
	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 180, El: 45})

	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return s.HavePan && s.HaveTilt && !s.Gotoing &&
			s.CurPan == 180 && s.CurTilt == 45
	}) {
		s := eng.Snapshot()
		t.Fatalf("did not settle at 180/45: have=%v/%v pan=%.2f tilt=%.2f gotoing=%v status=%q",
			s.HavePan, s.HaveTilt, s.CurPan, s.CurTilt, s.Gotoing, s.Status)
	}
}

// TestSimModePosPublished verifies the thread-safe Pos the inbound servers read
// from carries the simulator's position, so query answers match what was
// commanded — the contract the sat-tracking / PstRotator clients depend on.
func TestSimModePosPublished(t *testing.T) {
	eng := New(Options{
		Transport: config.TransportSim,
		Addr:      1,
		Sim:       config.SimConfig{StartPan: 90, StartTilt: 30, JogStep: 5},
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool {
		az, el, ok := eng.Pos().Get()
		return ok && az == 90 && el == 30
	}) {
		az, el, ok := eng.Pos().Get()
		t.Fatalf("Pos not at start 90/30: az=%.2f el=%.2f ok=%v", az, el, ok)
	}
}

// TestSimModeDisableSelfCheckOnConnect verifies the engine sends the
// disable-self-check frame once on connect when self_check.disable is set
// (the default for the 303Z/3050DZ), as a line in the published log ring.
func TestSimModeDisableSelfCheckOnConnect(t *testing.T) {
	eng := New(Options{
		Transport: config.TransportSim,
		Addr:      1,
		Sim:       config.SimConfig{StartPan: 0, StartTilt: 0, JogStep: 5},
		SelfCheck: config.SelfCheckConfig{Disable: true},
		LogLevel:  config.LogInfo, // the disable-self-check line is logged at info
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool { return eng.Snapshot().Connected }) {
		t.Fatal("engine did not connect to the simulator")
	}
	if !waitFor(time.Second, func() bool {
		for _, line := range eng.Snapshot().Log {
			if strings.Contains(line, "disable-self-check") {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("disable-self-check not logged on connect: %v", eng.Snapshot().Log)
	}
}

// TestSimModeSelfCheckOptOut verifies that with self_check.disable == false the
// engine does NOT send the disable-self-check frame on connect.
func TestSimModeSelfCheckOptOut(t *testing.T) {
	eng := New(Options{
		Transport: config.TransportSim,
		Addr:      1,
		Sim:       config.SimConfig{StartPan: 0, StartTilt: 0, JogStep: 5},
		SelfCheck: config.SelfCheckConfig{Disable: false},
		LogLevel:  config.LogInfo,
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool { return eng.Snapshot().Connected }) {
		t.Fatal("engine did not connect to the simulator")
	}
	// Give it a moment to be sure, then assert no disable-self-check line.
	time.Sleep(100 * time.Millisecond)
	for _, line := range eng.Snapshot().Log {
		if strings.Contains(line, "disable-self-check") {
			t.Fatalf("disable-self-check sent despite opt-out: %v", eng.Snapshot().Log)
		}
	}
}
