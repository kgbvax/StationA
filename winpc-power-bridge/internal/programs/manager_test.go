package programs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"winpc-power-bridge/internal/config"
)

// fakeProc is an in-memory process table.
type fakeProc struct {
	procs   map[int]fakeEntry
	nextPID int
	clock   uint64
	starts  []string
	ends    []int
	// dieOnStart makes a started process exit at once (process check fails).
	dieOnStart map[string]bool
	startErr   error
	endErr     error
}

type fakeEntry struct {
	image string
	start uint64
}

func newFakeProc() *fakeProc {
	return &fakeProc{procs: map[int]fakeEntry{}, nextPID: 100, dieOnStart: map[string]bool{}}
}

// run adds a process that the operator started.
func (f *fakeProc) run(image string) Process {
	f.nextPID++
	f.clock++
	f.procs[f.nextPID] = fakeEntry{image, f.clock}
	return Process{PID: f.nextPID, Start: f.clock}
}

func (f *fakeProc) Find(image string) ([]Process, error) {
	var out []Process
	for pid, e := range f.procs {
		if strings.EqualFold(e.image, image) {
			out = append(out, Process{PID: pid, Start: e.start})
		}
	}
	return out, nil
}

func (f *fakeProc) Alive(p Process) bool {
	e, ok := f.procs[p.PID]
	return ok && e.start == p.Start
}

func (f *fakeProc) Start(s Spec) (Process, error) {
	if f.startErr != nil {
		return Process{}, f.startErr
	}
	f.starts = append(f.starts, s.Exe)
	image := s.Exe[strings.LastIndexAny(s.Exe, `\/`)+1:]
	p := f.run(image)
	if f.dieOnStart[image] {
		delete(f.procs, p.PID)
	}
	return p, nil
}

func (f *fakeProc) End(p Process) error {
	if f.endErr != nil {
		return f.endErr
	}
	f.ends = append(f.ends, p.PID)
	delete(f.procs, p.PID)
	return nil
}

// fakeClock advances on Sleep, so settle and retry waits take no real time.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.t = c.t.Add(d)
	return nil
}

type harness struct {
	m       *Manager
	proc    *fakeProc
	clock   *fakeClock
	session bool
	owned   string
	seen    []string // program states seen by the progress callback
}

var testPrograms = []config.ProgramConfig{
	{Name: "wsjtx", Exe: `C:\WSJTX\bin\wsjtx.exe`, Process: "wsjtx.exe", Check: config.CheckProcess, SettleS: 5, CheckTimeoutS: 60, RestartOwned: true},
	{Name: "aethersdr", Exe: `C:\AetherSDR\AetherSDR.exe`, Process: "AetherSDR.exe", Check: config.CheckTCP,
		CheckAddress: "127.0.0.1:1", Probe: "f", ReplyPattern: `^[0-9]+$`, SettleS: 5, CheckTimeoutS: 30},
	{Name: "75auto", Exe: `C:\75auto\75auto.exe`, Process: "75auto.exe", Check: config.CheckNone, SettleS: 5, CheckTimeoutS: 60},
}

func newHarness(t *testing.T, progs []config.ProgramConfig) *harness {
	t.Helper()
	h := &harness{proc: newFakeProc(), clock: &fakeClock{t: time.Unix(1_800_000_000, 0)}}
	h.owned = filepath.Join(t.TempDir(), "owned.json")
	h.m = h.newManager(progs)
	return h
}

func (h *harness) newManager(progs []config.ProgramConfig) *Manager {
	return New(context.Background(), Options{
		Programs:      progs,
		Proc:          h.proc,
		OwnedFile:     h.owned,
		SessionActive: func() bool { return h.session },
		Clock:         h.clock,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func (h *harness) ensure(t *testing.T, name string) {
	t.Helper()
	h.seen = nil
	if err := h.m.Ensure(name, func() { h.seen = append(h.seen, h.field(t, name).State) }); err != nil {
		t.Fatalf("ensure %s: %v", name, err)
	}
}

func (h *harness) field(t *testing.T, name string) Field {
	t.Helper()
	for _, f := range h.m.Fields() {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no field for %s", name)
	return Field{}
}

func (h *harness) wantState(t *testing.T, name, state string) Field {
	t.Helper()
	f := h.field(t, name)
	if f.State != state {
		t.Fatalf("%s state = %q (error %q), want %q", name, f.State, f.Error, state)
	}
	if len(h.seen) == 0 || h.seen[0] != StateStarting {
		t.Errorf("%s: first published state = %v, want starting first", name, h.seen)
	}
	return f
}

func TestEnsureStartsMissingProgram(t *testing.T) {
	h := newHarness(t, testPrograms)
	t0 := h.clock.Now()
	h.ensure(t, "wsjtx")
	h.wantState(t, "wsjtx", StateReady)
	if len(h.proc.starts) != 1 || h.proc.starts[0] != `C:\WSJTX\bin\wsjtx.exe` {
		t.Fatalf("starts = %v", h.proc.starts)
	}
	if !h.m.Owned("wsjtx") {
		t.Error("started program not recorded as owned")
	}
	if got := h.clock.Now().Sub(t0); got < 5*time.Second {
		t.Errorf("ready after %s, want after settle_s", got)
	}
}

func TestEnsureAdoptsOperatorProgram(t *testing.T) {
	h := newHarness(t, testPrograms)
	h.proc.run("wsjtx.exe")
	h.ensure(t, "wsjtx")
	h.wantState(t, "wsjtx", StateReady)
	if len(h.proc.starts) != 0 || len(h.proc.ends) != 0 {
		t.Errorf("starts = %v ends = %v, want none", h.proc.starts, h.proc.ends)
	}
	if h.m.Owned("wsjtx") {
		t.Error("operator program recorded as owned")
	}
}

func TestEnsureRestartsOwnedWithRestartOwned(t *testing.T) {
	h := newHarness(t, testPrograms)
	h.ensure(t, "wsjtx")
	first := h.proc.starts
	h.ensure(t, "wsjtx")
	h.wantState(t, "wsjtx", StateReady)
	if len(h.proc.ends) != 1 || len(h.proc.starts) != len(first)+1 {
		t.Errorf("ends = %v starts = %v, want one end and a new start", h.proc.ends, h.proc.starts)
	}
	if !h.m.Owned("wsjtx") {
		t.Error("restarted program not owned")
	}
}

func TestEnsureDoesNotRestartOperatorProgramWithRestartOwned(t *testing.T) {
	h := newHarness(t, testPrograms)
	h.proc.run("wsjtx.exe")
	h.ensure(t, "wsjtx")
	h.wantState(t, "wsjtx", StateReady)
	if len(h.proc.ends) != 0 || len(h.proc.starts) != 0 {
		t.Errorf("ends = %v starts = %v, want none", h.proc.ends, h.proc.starts)
	}
}

func TestEnsureRestartsOwnedProgramThatFailsItsCheck(t *testing.T) {
	ln := listen(t, "RPRT -1")
	progs := []config.ProgramConfig{testPrograms[1]}
	progs[0].CheckAddress = ln
	h := newHarness(t, progs)
	// First ensure: started and owned, but the check fails (no radio).
	h.ensure(t, "aethersdr")
	h.wantState(t, "aethersdr", StateFailed)
	if !h.m.Owned("aethersdr") {
		t.Fatal("started program not owned")
	}
	// Second ensure: the owned copy fails its check, so it is ended and
	// started again (no restart_owned on this program).
	h.ensure(t, "aethersdr")
	if len(h.proc.ends) != 1 || len(h.proc.starts) != 2 {
		t.Errorf("ends = %v starts = %v, want end + start", h.proc.ends, h.proc.starts)
	}
}

func TestEnsureRestartedOwnedBecomesReady(t *testing.T) {
	var bad atomic.Int32
	bad.Store(1 << 20) // every reply is an error reply until reset
	progs := []config.ProgramConfig{testPrograms[1]}
	progs[0].CheckAddress = listenFunc(t, func() string {
		if bad.Add(-1) >= 0 {
			return "RPRT -1"
		}
		return "14074000"
	})
	h := newHarness(t, progs)
	h.ensure(t, "aethersdr") // owned, check fails
	h.wantState(t, "aethersdr", StateFailed)
	bad.Store(1) // the check of the running owned copy fails once more
	h.ensure(t, "aethersdr")
	h.wantState(t, "aethersdr", StateReady)
	if len(h.proc.ends) != 1 || len(h.proc.starts) != 2 {
		t.Errorf("ends = %v starts = %v, want end + start", h.proc.ends, h.proc.starts)
	}
}

func TestEnsureRefusesForeignFailingProgram(t *testing.T) {
	progs := []config.ProgramConfig{testPrograms[1]}
	progs[0].CheckAddress = closedPort(t)
	h := newHarness(t, progs)
	h.proc.run("AetherSDR.exe")
	h.ensure(t, "aethersdr")
	f := h.wantState(t, "aethersdr", StateFailed)
	if !strings.HasPrefix(f.Error, "not started by the chain") {
		t.Errorf("error = %q", f.Error)
	}
	if len(h.proc.ends) != 0 || len(h.proc.starts) != 0 {
		t.Errorf("ends = %v starts = %v, want none", h.proc.ends, h.proc.starts)
	}
}

func TestTCPCheckListenerReady(t *testing.T) {
	progs := []config.ProgramConfig{testPrograms[1]}
	progs[0].CheckAddress = listen(t, "14074000")
	h := newHarness(t, progs)
	h.ensure(t, "aethersdr")
	h.wantState(t, "aethersdr", StateReady)
}

func TestTCPCheckClosedPortFailsAfterTimeout(t *testing.T) {
	progs := []config.ProgramConfig{testPrograms[1]}
	progs[0].CheckAddress = closedPort(t)
	h := newHarness(t, progs)
	t0 := h.clock.Now()
	h.ensure(t, "aethersdr")
	f := h.wantState(t, "aethersdr", StateFailed)
	if got := h.clock.Now().Sub(t0); got < 30*time.Second {
		t.Errorf("failed after %s, want after check_timeout_s", got)
	}
	if f.Error == "" {
		t.Error("no error text")
	}
}

func TestTCPCheckErrorReplyInError(t *testing.T) {
	progs := []config.ProgramConfig{testPrograms[1]}
	progs[0].CheckAddress = listen(t, "RPRT -1")
	h := newHarness(t, progs)
	h.ensure(t, "aethersdr")
	f := h.wantState(t, "aethersdr", StateFailed)
	if !strings.Contains(f.Error, "RPRT -1") {
		t.Errorf("error = %q, want the reply", f.Error)
	}
}

func TestProcessCheckFailsWhenProcessExits(t *testing.T) {
	h := newHarness(t, testPrograms)
	h.proc.dieOnStart["wsjtx.exe"] = true
	h.ensure(t, "wsjtx")
	f := h.wantState(t, "wsjtx", StateFailed)
	if !strings.Contains(f.Error, "exited") {
		t.Errorf("error = %q", f.Error)
	}
}

func TestStartErrorFails(t *testing.T) {
	h := newHarness(t, testPrograms)
	h.proc.startErr = errors.New("access denied")
	h.ensure(t, "wsjtx")
	f := h.wantState(t, "wsjtx", StateFailed)
	if !strings.Contains(f.Error, "access denied") {
		t.Errorf("error = %q", f.Error)
	}
	if h.m.Owned("wsjtx") {
		t.Error("owned without a start")
	}
}

func TestEnsureAlreadyReady(t *testing.T) {
	h := newHarness(t, testPrograms)
	h.proc.run("wsjtx.exe")
	h.ensure(t, "wsjtx")
	h.ensure(t, "wsjtx")
	h.wantState(t, "wsjtx", StateReady)
	if len(h.seen) != 1 || h.seen[0] != StateStarting {
		t.Errorf("progress states = %v, want [starting]", h.seen)
	}
}

func TestEnsureCheckNoneReadyAtOnce(t *testing.T) {
	h := newHarness(t, testPrograms)
	t0 := h.clock.Now()
	h.ensure(t, "75auto")
	h.wantState(t, "75auto", StateReady)
	if !h.m.Owned("75auto") || len(h.proc.starts) != 1 {
		t.Errorf("owned = %v starts = %v", h.m.Owned("75auto"), h.proc.starts)
	}
	if h.clock.Now() != t0 {
		t.Errorf("check none waited %s", h.clock.Now().Sub(t0))
	}
}

func TestEnsureUnknownProgram(t *testing.T) {
	h := newHarness(t, testPrograms)
	if err := h.m.Ensure("notepad", func() { t.Error("progress for an unknown program") }); !errors.Is(err, ErrUnknownProgram) {
		t.Fatalf("err = %v, want ErrUnknownProgram", err)
	}
	if len(h.proc.starts) != 0 {
		t.Error("unknown program started something")
	}
}

func TestStopForeignRefused(t *testing.T) {
	h := newHarness(t, testPrograms)
	p := h.proc.run("wsjtx.exe")
	if err := h.m.Stop("wsjtx"); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("err = %v, want ErrNotOwned", err)
	}
	if !h.proc.Alive(p) || len(h.proc.ends) != 0 {
		t.Error("foreign program ended")
	}
}

func TestStopOwned(t *testing.T) {
	h := newHarness(t, testPrograms)
	h.ensure(t, "wsjtx")
	if err := h.m.Stop("wsjtx"); err != nil {
		t.Fatal(err)
	}
	if len(h.proc.ends) != 1 || h.m.Owned("wsjtx") {
		t.Errorf("ends = %v owned = %v", h.proc.ends, h.m.Owned("wsjtx"))
	}
	if f := h.field(t, "wsjtx"); f.State != StateStopped {
		t.Errorf("state = %q", f.State)
	}
}

func TestStopRefusedDuringSession(t *testing.T) {
	h := newHarness(t, testPrograms)
	h.ensure(t, "wsjtx")
	h.session = true
	if err := h.m.Stop("wsjtx"); !errors.Is(err, ErrSessionActive) {
		t.Fatalf("err = %v, want ErrSessionActive", err)
	}
	if len(h.proc.ends) != 0 || !h.m.Owned("wsjtx") {
		t.Error("program ended during a session")
	}
}

func TestStopUnknown(t *testing.T) {
	h := newHarness(t, testPrograms)
	if err := h.m.Stop("notepad"); !errors.Is(err, ErrUnknownProgram) {
		t.Fatalf("err = %v", err)
	}
}

func TestOwnedRecordSurvivesRestart(t *testing.T) {
	h := newHarness(t, testPrograms)
	h.ensure(t, "wsjtx")
	h.ensure(t, "75auto")
	// The 75auto PID is reused by another process after the agent restart.
	var pid int
	for p, e := range h.proc.procs {
		if e.image == "75auto.exe" {
			pid = p
		}
	}
	h.proc.clock++
	h.proc.procs[pid] = fakeEntry{"75auto.exe", h.proc.clock}

	m2 := h.newManager(testPrograms)
	if !m2.Owned("wsjtx") {
		t.Error("wsjtx with matching PID and start time not owned after restart")
	}
	if m2.Owned("75auto") {
		t.Error("reused PID with another start time still owned")
	}
	h.m = m2
	if f := h.field(t, "wsjtx"); f.State != StateReady {
		t.Errorf("owned live program after restart: state %q", f.State)
	}
}

func TestRefreshMarksDeadProgramStopped(t *testing.T) {
	h := newHarness(t, testPrograms)
	h.ensure(t, "wsjtx")
	h.proc.procs = map[int]fakeEntry{}
	h.m.Refresh()
	if f := h.field(t, "wsjtx"); f.State != StateStopped {
		t.Errorf("state = %q, want stopped", f.State)
	}
	if h.m.Owned("wsjtx") {
		t.Error("dead program still owned")
	}
}

func TestFieldsInConfigOrder(t *testing.T) {
	h := newHarness(t, testPrograms)
	var names []string
	for _, f := range h.m.Fields() {
		names = append(names, f.Name)
		if f.State != StateStopped {
			t.Errorf("%s initial state %q", f.Name, f.State)
		}
	}
	if strings.Join(names, ",") != "wsjtx,aethersdr,75auto" {
		t.Errorf("names = %v", names)
	}
}

func TestStopNotRefusedDuringReplaySession(t *testing.T) {
	h := newHarness(t, testPrograms)
	act := &Activity{}
	now := time.Unix(1_800_000_000, 0)
	act.AutoOpState([]byte(`{"session":{"state":"running","source":"replay"}}`), false, now)
	act.AutoOpBeat(now)
	h.m.sessionActive = func() bool { return act.SessionActive(now) }
	h.ensure(t, "wsjtx")
	if err := h.m.Stop("wsjtx"); err != nil {
		t.Fatalf("stop during a replay session: %v", err)
	}
	if h.m.Owned("wsjtx") {
		t.Error("program still owned after stop")
	}
}

func TestRestartEndFailureFails(t *testing.T) {
	h := newHarness(t, testPrograms)
	h.ensure(t, "wsjtx")
	var old Process
	for pid, e := range h.proc.procs {
		old = Process{PID: pid, Start: e.start}
	}
	h.proc.endErr = errors.New("access denied")
	h.ensure(t, "wsjtx")
	f := h.wantState(t, "wsjtx", StateFailed)
	if !strings.Contains(f.Error, "end: access denied") {
		t.Errorf("error = %q, want the End reason", f.Error)
	}
	if len(h.proc.starts) != 1 {
		t.Errorf("starts = %v, want no second start", h.proc.starts)
	}
	m2 := h.newManager(testPrograms)
	if !m2.Owned("wsjtx") || m2.owned["wsjtx"] != old {
		t.Errorf("owned record = %v, want the old copy %v", m2.owned, old)
	}
}

func TestCorruptOwnedFileTreatedAsEmpty(t *testing.T) {
	h := newHarness(t, testPrograms)
	if err := os.WriteFile(h.owned, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	m := New(context.Background(), Options{
		Programs: testPrograms, Proc: h.proc, OwnedFile: h.owned, Clock: h.clock,
		Log: slog.New(slog.NewTextHandler(&buf, nil)),
	})
	for _, f := range m.Fields() {
		if m.Owned(f.Name) || f.State != StateStopped {
			t.Errorf("%s: owned %v state %q after a corrupt record", f.Name, m.Owned(f.Name), f.State)
		}
	}
	if !strings.Contains(buf.String(), "owned record malformed") {
		t.Errorf("no log line for the corrupt record: %q", buf.String())
	}
}

func TestRefreshOperatorCopyExited(t *testing.T) {
	h := newHarness(t, testPrograms)
	h.proc.run("wsjtx.exe")
	h.ensure(t, "wsjtx")
	h.wantState(t, "wsjtx", StateReady)
	h.proc.procs = map[int]fakeEntry{}
	h.m.Refresh()
	if f := h.field(t, "wsjtx"); f.State != StateStopped {
		t.Errorf("state = %q, want stopped", f.State)
	}
}

func TestEnsureReusesOwnedWithoutRestartOwned(t *testing.T) {
	progs := []config.ProgramConfig{
		{Name: "wsjtx", Exe: `C:\WSJTX\bin\wsjtx.exe`, Process: "wsjtx.exe", Check: config.CheckProcess, SettleS: 5, CheckTimeoutS: 60},
	}
	h := newHarness(t, progs)
	h.ensure(t, "wsjtx")
	h.ensure(t, "wsjtx")
	h.wantState(t, "wsjtx", StateReady)
	if len(h.proc.starts) != 1 || len(h.proc.ends) != 0 {
		t.Errorf("starts = %v ends = %v, want one start and no end", h.proc.starts, h.proc.ends)
	}
	if !h.m.Owned("wsjtx") {
		t.Error("reused copy no longer owned")
	}
}

// listen starts a TCP server that answers every line with reply.
func listen(t *testing.T, reply string) string {
	t.Helper()
	return listenFunc(t, func() string { return reply })
}

// listenFunc starts a TCP server that answers every line with reply().
func listenFunc(t *testing.T, reply func() string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					if _, err := r.ReadString('\n'); err != nil {
						return
					}
					if _, err := c.Write([]byte(reply() + "\n")); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// closedPort returns an address where nothing listens.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}
