package engine

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"pelcots/internal/config"
	"pelcots/internal/control"
)

// simPollInterval overrides the engine's 500 ms poll cadence for the in-memory
// simulator tests: the sim has no baud-rate constraint, so a short tick lets a
// goto converge in well under a second instead of tens of seconds.
const simPollInterval = 20 * time.Millisecond

// gotoTol is the convergence window the tests assert. The engine ends a goto
// when readback is within moveTolerance (3°) of the target, so the rest
// position is within 3° by construction; 3.5° leaves a float slack.
const gotoTol = 3.5

func closeEnough(got, want float64) bool { return math.Abs(got-want) < gotoTol }

// TestSimModeGoto starts the engine against the in-memory simulator (no
// hardware), commands an absolute move through the same Submit path the
// inbound rotctld server uses, and asserts the engine reports the commanded
// position once the absolute-set goto converges on readback.
func TestSimModeGoto(t *testing.T) {
	eng := New(Options{
		Transport:    config.TransportSim,
		Addr:         1,
		Sim:          config.SimConfig{StartPan: 0, StartTilt: 0, JogStep: 2},
		AutoArm:      true, // the move arrives over the network (Submit)
		PollInterval: simPollInterval,
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool { return eng.Snapshot().Connected }) {
		t.Fatal("engine did not connect to the simulator")
	}
	// The closed loop is readback-driven, so both axes need a current position
	// before the goto can arm (startGoto blocks until havePan && haveTilt).
	if !waitFor(time.Second, func() bool {
		s := eng.Snapshot()
		return s.HavePan && s.HaveTilt
	}) {
		t.Fatalf("no readback before goto: %+v", eng.Snapshot())
	}

	// Command an absolute move exactly as gs232 "W 180 45" would.
	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 180, El: 45})

	if !waitFor(5*time.Second, func() bool {
		s := eng.Snapshot()
		return s.HavePan && s.HaveTilt && !s.Gotoing &&
			closeEnough(s.CurPan, 180) && closeEnough(s.CurTilt, 45)
	}) {
		s := eng.Snapshot()
		t.Fatalf("did not settle at 180/45: have=%v/%v pan=%.2f tilt=%.2f gotoing=%v status=%q",
			s.HavePan, s.HaveTilt, s.CurPan, s.CurTilt, s.Gotoing, s.Status)
	}
}

// TestSimModeZeroAzimuth verifies the azimuth zero-offset: after zeroing at a
// known physical azimuth, the engine reports that direction as 0° (in both the
// snapshot and the Pos the servers read), and a subsequent goto is interpreted
// in the zeroed frame.
func TestSimModeZeroAzimuth(t *testing.T) {
	eng := New(Options{
		Transport:    config.TransportSim,
		Addr:         1,
		Sim:          config.SimConfig{StartPan: 90, StartTilt: 0, JogStep: 2},
		AutoArm:      true, // the move arrives over the network (Submit)
		PollInterval: simPollInterval,
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return s.Connected && s.HavePan && s.HaveTilt && s.CurPan == 90
	}) {
		t.Fatalf("did not reach start 90°: %+v", eng.Snapshot())
	}

	eng.ZeroAzimuth()

	if !waitFor(time.Second, func() bool {
		s := eng.Snapshot()
		return s.AzOffset == 90 && s.CurPan == 0
	}) {
		s := eng.Snapshot()
		t.Fatalf("zero-az did not apply: offset=%.2f pan=%.2f status=%q", s.AzOffset, s.CurPan, s.Status)
	}

	// The Pos the servers read must also reflect the zeroed frame (updated on
	// the next pan readback, not synchronously with ZeroAzimuth).
	if !waitFor(time.Second, func() bool {
		az, _, ok := eng.Pos().Get()
		return ok && az == 0
	}) {
		az, _, ok := eng.Pos().Get()
		t.Fatalf("Pos not zeroed: az=%.2f ok=%v", az, ok)
	}

	// A goto in the zeroed frame: logical 30° = physical 120°. Only the pan
	// axis moves (SetPan); tilt is already at target.
	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 30, El: 0})
	if !waitFor(5*time.Second, func() bool {
		s := eng.Snapshot()
		return s.HavePan && !s.Gotoing && closeEnough(s.CurPan, 30)
	}) {
		s := eng.Snapshot()
		t.Fatalf("goto in zeroed frame failed: pan=%.2f gotoing=%v status=%q", s.CurPan, s.Gotoing, s.Status)
	}
}

// TestSimModeTiltInvert verifies the elevation inversion for an upside-down
// mount: the readback is mirrored (physical 0° reads as 90°) and a goto is
// interpreted in the inverted frame.
func TestSimModeTiltInvert(t *testing.T) {
	eng := New(Options{
		Transport:    config.TransportSim,
		Addr:         1,
		Sim:          config.SimConfig{StartPan: 0, StartTilt: 0, JogStep: 2},
		TiltInvert:   true,
		AutoArm:      true, // the move arrives over the network (Submit)
		PollInterval: simPollInterval,
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return s.Connected && s.HavePan && s.HaveTilt && s.CurTilt == 90
	}) {
		t.Fatalf("inverted readback not 90°: %+v", eng.Snapshot())
	}

	// Goto logical elevation 30° = physical tilt 60°. Only the tilt axis moves
	// (SetTilt, deferred one tick after the no-op pan decision).
	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 0, El: 30})
	if !waitFor(5*time.Second, func() bool {
		s := eng.Snapshot()
		return s.HaveTilt && !s.Gotoing && closeEnough(s.CurTilt, 30)
	}) {
		s := eng.Snapshot()
		t.Fatalf("goto in inverted frame failed: tilt=%.2f gotoing=%v status=%q", s.CurTilt, s.Gotoing, s.Status)
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

// TestNetworkCommandLoggedWithSource verifies that motion commands arriving
// via the inbound control protocols are logged with their source tag — so a
// rotctld setpos and a pstrotator stop are distinguishable from each other and
// from local moves in the trace. This is the contract the sat-tracking /
// PstRotator and rotctld integrations rely on for debugging: each CMD line in
// the log ring carries [rotctld]/[gs232]/[pstrotator], not bare hex.
func TestNetworkCommandLoggedWithSource(t *testing.T) {
	eng := New(Options{
		Transport: config.TransportSim,
		Addr:      1,
		Sim:       config.SimConfig{StartPan: 0, StartTilt: 0, JogStep: 5},
		LogLevel:  config.LogInfo,
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool { return eng.Snapshot().Connected }) {
		t.Fatal("engine did not connect to the simulator")
	}

	// A setpos exactly as the rotctld "P 180 45" handler submits it.
	eng.Submit(control.Command{
		Kind: control.KindSetPos, Az: 180, El: 45,
		Source: "rotctld", Raw: "P 180 45",
	})
	// A stop exactly as the PstRotator "<STOP>" handler submits it.
	eng.Submit(control.Command{
		Kind:   control.KindStop,
		Source: "rotctld", Raw: "S",
	})

	if !waitFor(time.Second, func() bool {
		rotc, pst := false, false
		for _, line := range eng.Snapshot().Log {
			if strings.Contains(line, "CMD [rotctld] setpos") {
				rotc = true
			}
			if strings.Contains(line, "CMD [rotctld] stop") {
				pst = true
			}
		}
		return rotc && pst
	}) {
		t.Fatalf("source-tagged CMD lines missing\nlog=%v", eng.Snapshot().Log)
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

// TestPollOneQueryPerTick is the regression guard for the readback-collapse fix.
// The 303Z/3050DZ at 2400 baud cannot service a back-to-back QueryPan+QueryTilt
// pair within one poll: the pan reply collides with the outgoing tilt query and
// readback collapses to occasional random replies (observed live ~0–1%). The
// fix is to send exactly ONE position query per tick, alternating pan/tilt.
//
// This test asserts the invariant directly from the TX trace: grouping query
// frames (Pelco-D pan-query 0x51 / tilt-query 0x53) by the millisecond they
// were sent, no group may contain BOTH a pan and a tilt query — i.e. the two
// queries are never sent in the same poll. Ticks are 500 ms apart, so queries
// from different ticks are far apart in time; queries within one poll are
// sub-millisecond apart (sim Send is synchronous). A 50 ms grouping threshold
// cleanly separates the two.
func TestPollOneQueryPerTick(t *testing.T) {
	eng := New(Options{
		Transport: config.TransportSim,
		Addr:      1,
		Sim:       config.SimConfig{StartPan: 0, StartTilt: 0, JogStep: 5},
		LogLevel:  config.LogDebug, // TX lines are logged at debug
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool { return eng.Snapshot().Connected }) {
		t.Fatal("engine did not connect to the simulator")
	}
	// ~5 ticks at 500 ms → enough pan and tilt queries to prove alternation.
	time.Sleep(2800 * time.Millisecond)

	type qf struct {
		ms  int64 // milliseconds-of-day from the log timestamp
		cmd byte  // 0x51 pan query, 0x53 tilt query
	}
	var qs []qf
	for _, line := range eng.Snapshot().Log {
		// TX lines look like: "14:30:00.123 DEBUG TX  FF 01 00 51 00 00 52"
		if !strings.Contains(line, "TX  FF ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		cmdByte, err := strconv.ParseInt(fields[6], 16, 64) // fields: ts lvl TX FF 01 00 <cmd> ...
		if err != nil || (cmdByte != 0x51 && cmdByte != 0x53) {
			continue
		}
		ts := fields[0] // HH:MM:SS.mmm
		parts := strings.Split(ts, ":")
		if len(parts) != 3 {
			continue
		}
		secParts := strings.SplitN(parts[2], ".", 2)
		if len(secParts) != 2 {
			continue
		}
		h, _ := strconv.ParseInt(parts[0], 10, 64)
		m, _ := strconv.ParseInt(parts[1], 10, 64)
		s, _ := strconv.ParseInt(secParts[0], 10, 64)
		msm, _ := strconv.ParseInt(secParts[1], 10, 64)
		ms := h*3600000 + m*60000 + s*1000 + msm
		qs = append(qs, qf{ms: ms, cmd: byte(cmdByte)})
	}
	if len(qs) < 4 {
		t.Fatalf("too few query frames captured (%d); log=%v", len(qs), eng.Snapshot().Log)
	}

	// Group consecutive queries within 50 ms of each other (same poll); ticks
	// are 500 ms apart so cross-tick gaps are far larger than the threshold.
	const sameTickMs = 50
	var panCount, tiltCount int
	groupPan, groupTilt := false, false
	flush := func() {
		if groupPan && groupTilt {
			t.Fatalf("a single poll sent both pan and tilt queries (readback-collapse bug): log=%v", eng.Snapshot().Log)
		}
		if groupPan {
			panCount++
		}
		if groupTilt {
			tiltCount++
		}
		groupPan, groupTilt = false, false
	}
	for i, q := range qs {
		if i > 0 && q.ms-qs[i-1].ms > sameTickMs {
			flush()
		}
		switch q.cmd {
		case 0x51:
			groupPan = true
		case 0x53:
			groupTilt = true
		}
	}
	flush()

	if panCount < 2 || tiltCount < 2 {
		t.Fatalf("expected alternation of both axes (pan=%d tilt=%d ticks): log=%v", panCount, tiltCount, eng.Snapshot().Log)
	}
}

// TestSimModePelcoPTx asserts the wire-protocol option end to end: with
// Protocol "p" every TX frame the engine emits is 8-byte 0xA0/0xAF Pelco-P,
// readback keeps flowing (the RX side is adaptive), State.Protocol publishes
// "p", and an absolute-set goto still converges — the sim answers in the
// protocol the query arrived in, exactly like the adaptive head.
func TestSimModePelcoPTx(t *testing.T) {
	eng := New(Options{
		Transport:    config.TransportSim,
		Addr:         1,
		Protocol:     config.ProtocolP,
		Sim:          config.SimConfig{StartPan: 0, StartTilt: 0, JogStep: 2},
		AutoArm:      true, // the move arrives over the network (Submit)
		PollInterval: simPollInterval,
		LogLevel:     config.LogDebug, // per-frame TX lines
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return s.Connected && s.HavePan && s.HaveTilt
	}) {
		t.Fatalf("no readback in P mode: %+v", eng.Snapshot())
	}
	if s := eng.Snapshot(); s.Protocol != "p" {
		t.Fatalf("State.Protocol = %q want p", s.Protocol)
	}
	// Every TX frame in the trace must carry the 0xA0 Pelco-P sync byte.
	var sawTx bool
	for _, l := range eng.Snapshot().Log {
		// lines look like "15:04:05.000 DEBUG TX  A0 01 00 53 00 00 AF 5D"
		fields := strings.Fields(l)
		tx := -1
		for i, w := range fields {
			if w == "TX" {
				tx = i
				break
			}
		}
		if tx < 0 || tx+2 > len(fields) {
			continue
		}
		sawTx = true
		if fields[tx+1] != "A0" {
			t.Fatalf("TX frame in P mode is not Pelco-P: %q", l)
		}
	}
	if !sawTx {
		t.Fatal("no TX frame in the trace (log_level debug expected)")
	}

	// The full control loop works in P too.
	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 180, El: 45})
	if !waitFor(5*time.Second, func() bool {
		s := eng.Snapshot()
		return !s.Gotoing && closeEnough(s.CurPan, 180) && closeEnough(s.CurTilt, 45)
	}) {
		s := eng.Snapshot()
		t.Fatalf("P-mode goto did not settle at 180/45: pan=%.2f tilt=%.2f gotoing=%v",
			s.CurPan, s.CurTilt, s.Gotoing)
	}
}
