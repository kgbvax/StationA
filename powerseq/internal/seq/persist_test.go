package seq

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// capLogger records Warn/Error lines and forwards everything to a testLogger.
type capLogger struct {
	*testLogger
	mu    sync.Mutex
	errs  []string
	warns []string
}

func newCapLogger(t *testing.T) *capLogger { return &capLogger{testLogger: newTestLogger(t)} }

func (l *capLogger) Warnf(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	l.mu.Lock()
	l.warns = append(l.warns, msg)
	l.mu.Unlock()
	l.logf("WARN: %s", msg)
}

func (l *capLogger) Errorf(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	l.mu.Lock()
	l.errs = append(l.errs, msg)
	l.mu.Unlock()
	l.logf("ERROR: %s", msg)
}

func (l *capLogger) errors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.errs...)
}

func (l *capLogger) warnings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.warns...)
}

// runSeq starts the runner; stop cancels it and waits until Run returned (a
// process exit after SIGTERM, with main waiting for the runner).
func runSeq(s *Sequencer) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

func readSaved(t *testing.T, path string) savedState {
	t.Helper()
	st, err := readStateFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	return st
}

// newSeqWithFile builds a sequencer on cfg with the given state file and a
// connected broker (restore runs inside New).
func newSeqWithFile(t *testing.T, cfg Config, path string) (*Sequencer, *recPublisher, *capLogger) {
	t.Helper()
	cfg.StateFile = path
	pub := &recPublisher{}
	log := newCapLogger(t)
	s, err := New(cfg, pub, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.SetBrokerOnline(true)
	return s, pub, log
}

func (s *Sequencer) clocks() (runningSince, standbySince, lastWatch time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runningSince, s.standbySince, s.lastWatch
}

// A deploy while running used to come back idle, so the station never entered
// standby again. A restart in running restores running (no /cmd at boot) with
// the original running entry, and the idle logic carries on.
func TestRestartInRunningStaysRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s1, _, _ := newSeqWithFile(t, standbyTestConfig(&wolRec{}), path)
	stop1 := runSeq(s1)
	s1.Start()
	runUntil(t, s1, func() bool { return phaseIs(s1, PhaseRunning) }, time.Second)
	stop1()
	since, _, _ := s1.clocks()
	if st := readSaved(t, path); st.Phase != PhaseRunning || !st.RunningSince.Equal(since) {
		t.Fatalf("state file = %+v, want running since %s", st, since)
	}

	wol2 := &wolRec{}
	s2, pub2, log2 := newSeqWithFile(t, standbyTestConfig(wol2), path)
	if ph, step, fault := s2.Phase(); ph != PhaseRunning || step != "" || fault != "" {
		t.Fatalf("restored phase=%s step=%q fault=%q, want running", ph, step, fault)
	}
	if got, _, _ := s2.clocks(); !got.Equal(since) {
		t.Errorf("restored runningSince = %s, want %s", got, since)
	}
	if errs := log2.errors(); len(errs) != 0 {
		t.Errorf("restore logged errors: %v", errs)
	}
	s2.markOnline("hf/antenna-select")
	stop2 := runSeq(s2)
	defer stop2()
	runUntil(t, s2, func() bool { return len(pub2.states()) > 0 }, time.Second)
	if st := pub2.states()[0]; st.Phase != PhaseRunning || st.Fault != "" {
		t.Errorf("boot /state = %+v, want running", st)
	}
	if n := len(pub2.cmds()); n != 0 || wol2.n() != 0 {
		t.Fatalf("restart emitted %d cmds / %d wol, want none", n, wol2.n())
	}

	// The station can stand by again after the restart.
	s2.setActivity("inactive", time.Time{})
	runUntil(t, s2, func() bool { return phaseIs(s2, PhaseStandby) }, time.Second)
}

// A restart in standby restores standby with the idle-shutdown clock of the
// first life: the shutdown is due ShutdownTimeout after the real last
// activity, not after the restart.
func TestRestartInStandbyKeepsIdleShutdownClock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := standbyTestConfig(&wolRec{})
	cfg.IdleShutdown = idleShutdownSteps()
	cfg.ShutdownTimeout = time.Hour

	s1, _, _ := newSeqWithFile(t, cfg, path)
	s1.markOnline("hf/antenna-select")
	stop1 := runSeq(s1)
	s1.Start()
	runUntil(t, s1, func() bool { return phaseIs(s1, PhaseRunning) }, time.Second)
	s1.setActivity("inactive", time.Time{})
	runUntil(t, s1, func() bool { return phaseIs(s1, PhaseStandby) }, time.Second)
	stop1()
	since, standbySince, _ := s1.clocks()

	wol2 := &wolRec{}
	cfg2 := cfg
	cfg2.WOL = wol2.send
	s2, pub2, _ := newSeqWithFile(t, cfg2, path)
	if ph := mustPhase(s2); ph != PhaseStandby {
		t.Fatalf("restored phase=%s, want standby", ph)
	}
	gotRun, gotStandby, _ := s2.clocks()
	if !gotRun.Equal(since) || !gotStandby.Equal(standbySince) {
		t.Errorf("restored clocks running=%s standby=%s, want %s / %s", gotRun, gotStandby, since, standbySince)
	}
	s2.markOnline("hf/antenna-select")
	stop2 := runSeq(s2)
	defer stop2()
	runUntil(t, s2, func() bool { return len(pub2.states()) > 0 }, time.Second)
	if st := pub2.states()[0]; st.Phase != PhaseStandby {
		t.Errorf("boot /state = %+v, want standby", st)
	}

	// No idle shutdown before the activity slot's retained /state arrived:
	// its last_activity is part of the clock.
	if got := s2.idleDecision(since.Add(10 * time.Hour)); got != "" {
		t.Errorf("idle decision before the activity /state = %q, want none", got)
	}
	s2.setActivity("inactive", time.Time{})
	// Clock preserved: due one ShutdownTimeout after the first life's running
	// entry. A clock reset to the restart (≥ one idle timeout later) would
	// still say "" at since+1h+1ms.
	if got := s2.idleDecision(since.Add(time.Hour - time.Millisecond)); got != "" {
		t.Errorf("idle decision before the deadline = %q, want none", got)
	}
	if got := s2.idleDecision(since.Add(time.Hour + time.Millisecond)); got != "idle_shutdown" {
		t.Errorf("idle decision after the deadline = %q, want idle_shutdown (clock reset by the restart?)", got)
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(pub2.cmds()); n != 0 || wol2.n() != 0 || !phaseIs(s2, PhaseStandby) {
		t.Fatalf("restart switched something: %d cmds, %d wol, phase=%s", n, wol2.n(), mustPhase(s2))
	}
}

// A sequence in flight at a crash is not re-run: the restart lands in idle
// with a fault naming the interrupted step, switches nothing, and keeps the
// stop_reason.
func TestRestartMidTransitionIsIdleWithFault(t *testing.T) {
	for _, phase := range []string{PhaseStarting, PhaseStopping, PhaseEnteringStandby, PhaseWaking} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			crash := savedState{Version: stateFileVersion, Phase: phase, Step: "antennas-on", SavedAt: time.Now()}
			if phase == PhaseStopping {
				crash.StopReason = "idle_shutdown"
			}
			if err := writeStateFile(path, crash); err != nil {
				t.Fatal(err)
			}
			wol := &wolRec{}
			s, pub, log := newSeqWithFile(t, standbyTestConfig(wol), path)
			want := "antennas-on: interrupted by a powerseq restart (" + phase + ")"
			if ph, step, fault := s.Phase(); ph != PhaseIdle || step != "" || fault != want {
				t.Fatalf("restored phase=%s step=%q fault=%q, want idle / %q", ph, step, fault, want)
			}
			if len(log.warnings()) == 0 {
				t.Error("interrupted sequence not logged as a warning")
			}
			// Inputs that would satisfy every wait and the idle logic.
			s.allObserved()
			s.markOnline("hf/antenna-select", "power/antennas")
			s.setActivity("active", time.Now())
			stop := runSeq(s)
			runUntil(t, s, func() bool { return len(pub.states()) > 0 }, time.Second)
			time.Sleep(20 * time.Millisecond)
			stop()
			if n := len(pub.cmds()); n != 0 || wol.n() != 0 {
				t.Fatalf("restart re-ran the sequence: %+v, wol=%d", pub.cmds(), wol.n())
			}
			st := pub.states()[0]
			if st.Phase != PhaseIdle || st.Fault != want || st.StopReason != crash.StopReason {
				t.Errorf("boot /state = %+v, want idle, fault %q, stop_reason %q", st, want, crash.StopReason)
			}
			if f := readSaved(t, path); f.Phase != PhaseIdle || f.Fault != want {
				t.Errorf("state file after restore = %+v, want idle with the fault", f)
			}
		})
	}
}

// The transitional phase is on disk while the sequence runs (a SIGKILL there
// restores as interrupted); a graceful stop mid-sequence aborts it and records
// idle + fault itself. Either way start re-runs the startup as usual.
func TestCrashAndStopMidStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	cfg := baseConfig(exampleStartup(), exampleShutdown())
	cfg.StepTimeout = 10 * time.Second // blocks at wait-controllers
	s1, _, _ := newSeqWithFile(t, cfg, path)
	stop1 := runSeq(s1)
	s1.Start()
	deadline := time.Now().Add(time.Second)
	var snap []byte
	for {
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), `"step": "wait-controllers"`) {
			snap = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("state file never showed the running step: %s", b)
		}
		time.Sleep(time.Millisecond)
	}
	crashPath := filepath.Join(dir, "crash.json")
	if err := os.WriteFile(crashPath, snap, 0o600); err != nil {
		t.Fatal(err)
	}
	stop1() // SIGTERM: the runner aborts the sequence
	if f := readSaved(t, path); f.Phase != PhaseIdle || f.Fault != "wait-controllers: interrupted" {
		t.Errorf("state file after a graceful stop = %+v, want idle / wait-controllers: interrupted", f)
	}

	// Restart from the SIGKILL snapshot.
	s2, pub2, _ := newSeqWithFile(t, cfg, crashPath)
	want := "wait-controllers: interrupted by a powerseq restart (starting)"
	if ph, _, fault := s2.Phase(); ph != PhaseIdle || fault != want {
		t.Fatalf("restored phase=%s fault=%q, want idle / %q", ph, fault, want)
	}
	s2.allObserved()
	stop2 := runSeq(s2)
	defer stop2()
	time.Sleep(20 * time.Millisecond)
	if n := len(pub2.cmds()); n != 0 {
		t.Fatalf("restart re-ran the startup: %+v", pub2.cmds())
	}
	// Recovery is the operator's start (a full startup, the slots are idempotent).
	s2.Start()
	runUntil(t, s2, func() bool { return phaseIs(s2, PhaseRunning) }, time.Second)
	if _, _, fault := s2.Phase(); fault != "" {
		t.Errorf("fault after start = %q, want cleared", fault)
	}

	// Restart from the graceful-stop file.
	s3, _, _ := newSeqWithFile(t, cfg, path)
	if ph, _, fault := s3.Phase(); ph != PhaseIdle || fault != "wait-controllers: interrupted" {
		t.Errorf("restored phase=%s fault=%q after a graceful stop", ph, fault)
	}
}

func TestCorruptOrMissingStateFileStartsIdle(t *testing.T) {
	cases := map[string]*string{
		"missing":       nil,
		"empty":         ptr(""),
		"garbage":       ptr("{not json"),
		"wrong version": ptr(`{"version":2,"phase":"running"}`),
		"unknown phase": ptr(`{"version":1,"phase":"flying"}`),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if content != nil {
				if err := os.WriteFile(path, []byte(*content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			s, pub, log := newSeqWithFile(t, standbyTestConfig(&wolRec{}), path)
			if ph, _, fault := s.Phase(); ph != PhaseIdle || fault != "" {
				t.Fatalf("phase=%s fault=%q, want plain idle", ph, fault)
			}
			errs := log.errors()
			if len(errs) == 0 || !strings.Contains(errs[0], path) {
				t.Fatalf("errors = %v, want one naming %s", errs, path)
			}
			stop := runSeq(s)
			runUntil(t, s, func() bool { return len(pub.states()) > 0 }, time.Second)
			stop()
			if f := readSaved(t, path); f.Phase != PhaseIdle {
				t.Errorf("state file not rewritten as idle: %+v", f)
			}
			if n := len(pub.cmds()); n != 0 {
				t.Errorf("boot emitted %d cmds", n)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// An idle restore keeps the fault latch and the stop_reason.
func TestRestoredIdleKeepsFaultAndStopReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeStateFile(path, savedState{Version: stateFileVersion, Phase: PhaseIdle,
		Fault: "wait-pa-power: timeout", StopReason: "idle_shutdown", SavedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s, pub, _ := newSeqWithFile(t, baseConfig(exampleStartup(), exampleShutdown()), path)
	stop := runSeq(s)
	defer stop()
	runUntil(t, s, func() bool { return len(pub.states()) > 0 }, time.Second)
	if st := pub.states()[0]; st.Phase != PhaseIdle || st.Fault != "wait-pa-power: timeout" || st.StopReason != "idle_shutdown" {
		t.Errorf("boot /state = %+v", st)
	}
}

func TestRestoredStandbyWithoutStandbyConfigIsIdleFault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeStateFile(path, savedState{Version: stateFileVersion, Phase: PhaseStandby, SavedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s, _, _ := newSeqWithFile(t, baseConfig(exampleStartup(), exampleShutdown()), path)
	if ph, _, fault := s.Phase(); ph != PhaseIdle || !strings.Contains(fault, "no longer configured") {
		t.Errorf("phase=%s fault=%q, want idle with a config fault", ph, fault)
	}
}

// Clock-only changes (here a watch move) are written by the idle tick no more
// often than PersistEvery, and flushed when the runner exits.
func TestClockOnlyChangesThrottledAndFlushed(t *testing.T) {
	moveWatch := func(s *Sequencer) {
		s.SetState(abs("uhf/radio"), []byte(`{"freq_hz":145800000,"device_online":true}`))
		s.SetState(abs("uhf/radio"), []byte(`{"freq_hz":145800500,"device_online":true}`))
	}
	t.Run("throttled then flushed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		cfg := standbyTestConfig(&wolRec{})
		cfg.PersistEvery = time.Hour
		s, _, _ := newSeqWithFile(t, cfg, path)
		s.markOnline("uhf/radio")
		stop := runSeq(s)
		s.Start()
		runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
		moveWatch(s)
		_, _, moved := s.clocks()
		if moved.IsZero() {
			t.Fatal("watch move not counted")
		}
		time.Sleep(20 * time.Millisecond) // several idle ticks
		if f := readSaved(t, path); !f.LastWatch.IsZero() {
			t.Errorf("clock-only change written before PersistEvery: %+v", f)
		}
		stop()
		if f := readSaved(t, path); !f.LastWatch.Equal(moved) || f.Phase != PhaseRunning {
			t.Errorf("state file after exit = %+v, want running with last_watch %s", f, moved)
		}
	})
	t.Run("written by the tick", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		cfg := standbyTestConfig(&wolRec{})
		cfg.PersistEvery = 5 * time.Millisecond
		s, _, _ := newSeqWithFile(t, cfg, path)
		s.markOnline("uhf/radio")
		stop := runSeq(s)
		defer stop()
		s.Start()
		runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
		moveWatch(s)
		_, _, moved := s.clocks()
		runUntil(t, s, func() bool {
			f, err := readStateFile(path)
			return err == nil && f.LastWatch.Equal(moved)
		}, time.Second)
	})
}

// A state file that cannot be written is an Error, never a stall.
func TestStateFileWriteFailureKeepsRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "state.json")
	s, _, log := newSeqWithFile(t, standbyTestConfig(&wolRec{}), path)
	stop := runSeq(s)
	defer stop()
	s.Start()
	runUntil(t, s, func() bool { return phaseIs(s, PhaseRunning) }, time.Second)
	var writeErr bool
	for _, e := range log.errors() {
		if strings.Contains(e, "write failed") {
			writeErr = true
		}
	}
	if !writeErr {
		t.Errorf("errors = %v, want a write failure", log.errors())
	}
}

// Without a state file every start is idle (the pre-persistence behaviour).
func TestNoStateFileStartsIdle(t *testing.T) {
	s, pub := newTestSeq(t)
	stop := runSeq(s)
	s.Start()
	runUntil(t, s, func() bool { return len(pub.cmds()) > 0 }, time.Second)
	stop()
	s2, _ := newTestSeq(t)
	if ph := mustPhase(s2); ph != PhaseIdle {
		t.Errorf("phase=%s without a state file, want idle", ph)
	}
}
