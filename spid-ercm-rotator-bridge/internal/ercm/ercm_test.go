package ercm

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

const testFirmware = "ERC-M GS232B V2.1-test"

// testConfig builds a fast-ticking driver Config wired to the given opener.
func testConfig(opener func() (io.ReadWriteCloser, error)) Config {
	return Config{
		PollInterval:   5 * time.Millisecond,
		ReopenCooldown: 5 * time.Millisecond,
		ReplyTimeout:   time.Second,
		Opener:         opener,
	}
}

// startDriver runs d in the background and cancels it at cleanup.
func startDriver(t *testing.T, cfg Config) *Driver {
	t.Helper()
	d := New(cfg, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("driver Run did not exit after ctx cancel")
		}
	})
	return d
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func hasLineWithPrefix(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// --- parsing -------------------------------------------------------------------

func TestParsePosition(t *testing.T) {
	cases := []struct {
		name         string
		line         string
		az, el       float64
		hasAz, hasEl bool
	}{
		{"B-mode both (Appendix spelling)", "AZ=123  EL=45", 123, 45, true, true},
		{"B-mode both single space", "AZ=123 EL=7", 123, 7, true, true},
		{"B-mode el only", "EL=45", 0, 45, false, true},
		{"B-mode az only", "AZ=123", 123, 0, true, false},
		{"B-mode padded", "AZ=001  EL=090", 1, 90, true, true},
		{"A-mode both", "+0123+0045", 123, 45, true, true},
		{"A-mode el-only fallback", "+0045", 0, 45, false, true},
		{"unknown reply", "?>", 0, 0, false, false},
		{"empty", "", 0, 0, false, false},
	}
	for _, c := range cases {
		az, el, hasAz, hasEl := parsePosition(c.line)
		if hasAz != c.hasAz || hasEl != c.hasEl {
			t.Errorf("%s: has flags = (%v, %v), want (%v, %v)", c.name, hasAz, hasEl, c.hasAz, c.hasEl)
			continue
		}
		if hasAz && az != c.az {
			t.Errorf("%s: az = %v, want %v", c.name, az, c.az)
		}
		if hasEl && el != c.el {
			t.Errorf("%s: el = %v, want %v", c.name, el, c.el)
		}
	}
}

// --- W command bytes (KTD6, Appendix B) -----------------------------------------

func TestGotoWCommandBytes(t *testing.T) {
	m := NewMock()
	m.SetAZ(123)
	m.SetEL(10)
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))

	// Elevation rides the az channel: the readback el is the AZ digits.
	eventually(t, "valid readback from the az digits", func() bool {
		el, ok := d.Readback()
		return ok && el == 123
	})

	if err := d.SetTarget(45); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	eventually(t, "W on the wire with el as the az operand", func() bool {
		return hasLine(m.Writes(), "W045 000")
	})
	eventually(t, "mock az channel at the target", func() bool { return m.AZ() == 45 })
	eventually(t, "fresh readback after move", func() bool {
		el, ok := d.Readback()
		return ok && el == 45
	})
}

// The el-on-az-channel mapping needs NO readback first: a goto goes straight
// to the wire (the old KTD6 az-deferral would stall every goto until a C2
// reply, on a controller whose az channel IS the elevation).
func TestGotoWrittenImmediatelyWithoutReadback(t *testing.T) {
	m := NewMock()
	m.HoldReadback(true) // NO C2 reply at all
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))

	eventually(t, "a poll tick ran", func() bool { return hasLine(m.Writes(), "C2") })

	if err := d.SetTarget(30); err != nil {
		t.Fatalf("SetTarget without readback: %v", err)
	}
	if !hasLine(m.Writes(), "W030 000") {
		t.Fatalf("goto not written immediately without any readback: %q", m.Writes())
	}
	eventually(t, "mock az channel at the target", func() bool { return m.AZ() == 30 })
}

// Readback validity starts false and lands only with the first C2 reply (KTD7).
func TestReadbackValidityLifecycle(t *testing.T) {
	m := NewMock()
	m.SetAZ(123)
	m.SetEL(10)
	m.HoldReadback(true)
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))

	eventually(t, "a poll tick ran", func() bool { return hasLine(m.Writes(), "C2") })
	if _, ok := d.Readback(); ok {
		t.Fatal("readback reported valid before the first C2 reply")
	}

	m.HoldReadback(false)
	m.StageReadback()
	eventually(t, "valid readback after first reply", func() bool {
		el, ok := d.Readback()
		return ok && el == 123 // the AZ digits ARE the elevation (wiring swap)
	})
}

// --- stop (S/E) ------------------------------------------------------------------

func TestStopWritesE(t *testing.T) {
	m := NewMock()
	m.SetAZ(123)
	m.SetEL(10)
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))

	eventually(t, "a poll tick ran", func() bool { return hasLine(m.Writes(), "C2") })

	// A goto goes straight to the wire (el-on-az mapping); a following stop
	// writes E and re-sends nothing — motion is written or refused, never
	// parked for later.
	if err := d.SetTarget(30); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !hasLine(m.Writes(), "E") {
		t.Fatalf("stop did not write E, writes: %q", m.Writes())
	}

	time.Sleep(60 * time.Millisecond) // let a poll tick pass
	ws := 0
	for _, l := range m.Writes() {
		if strings.HasPrefix(l, "W") {
			ws++
			if l != "W030 000" {
				t.Errorf("unexpected W spelling %q", l)
			}
		}
	}
	if ws != 1 {
		t.Errorf("W count after goto + stop = %d, want exactly 1", ws)
	}

	// The S form is the same stop command on the wire — the mock must accept
	// both spellings (plan "S/E stop").
	before := m.StopCount()
	if _, err := m.Write([]byte("S\r")); err != nil {
		t.Fatalf("direct S write: %v", err)
	}
	if got := m.StopCount(); got != before+1 {
		t.Errorf("mock StopCount after S = %d, want %d", got, before+1)
	}
	if !hasLine(m.Writes(), "S") {
		t.Errorf("S missing from mock write log: %q", m.Writes())
	}
}

// An OFFLINE stop refuses with ErrOffline and writes nothing; when the link
// heals, no motion may reach the wire on its behalf — motion is written or
// refused, never parked for later (the uncommanded-post-recovery-motion
// trap).
func TestOfflineStopRefusesAndHealsToNoMotion(t *testing.T) {
	m1 := NewMock()
	m2 := NewMock()
	m2.SetAZ(75)
	m2.SetEL(20)

	// The heal is gated: until the test says so, reopens after the fault fail
	// fast — the down state must be DURABLE while Stop runs, not the ~1 ms
	// transient a fast self-heal would make of it.
	heal := make(chan struct{})
	opened := 0
	opener := func() (io.ReadWriteCloser, error) {
		select {
		case <-heal:
			return m2.Port(), nil
		default:
		}
		n := opened
		opened++
		if n == 0 {
			return m1.Port(), nil
		}
		return nil, errors.New("adapter absent (scripted)")
	}
	cfg := testConfig(opener)
	d := startDriver(t, cfg)

	eventually(t, "first poll on the initial link", func() bool { return hasLine(m1.Writes(), "C2") })

	// Fault the link (the FailReads seam): the in-flight poll takes it Down,
	// and the gated opener keeps it down.
	m1.FailReads(1)
	eventually(t, "link down and staying down", func() bool { return !d.Online() })
	time.Sleep(20 * time.Millisecond) // a reopen attempt or two must keep failing
	if d.Online() {
		t.Fatal("link healed before the gate opened")
	}

	if err := d.Stop(); !errors.Is(err, ErrOffline) {
		t.Fatalf("Stop on a down link = %v, want ErrOffline", err)
	}
	if hasLineWithPrefix(m1.Writes(), "W") || hasLine(m1.Writes(), "E") {
		t.Fatalf("offline stop wrote motion/stop bytes: %q", m1.Writes())
	}

	// Heal: the fresh link resumes polling — and must write no motion.
	close(heal)
	eventually(t, "healed link readback", func() bool {
		el, ok := d.Readback()
		return d.Online() && ok && el == 75
	})
	time.Sleep(60 * time.Millisecond) // several live poll cycles
	for i, m := range []*MockDevice{m1, m2} {
		if hasLineWithPrefix(m.Writes(), "W") || hasLine(m.Writes(), "E") {
			t.Errorf("mock %d: motion bytes reached the wire around an offline stop: %q", i+1, m.Writes())
		}
	}
}

// --- self-heal (KTD7) --------------------------------------------------------------

func TestReopenAfterScriptedError(t *testing.T) {
	m1 := NewMock()
	m1.FailReads(1) // the first read (the first C2 poll) faults
	m2 := NewMock()
	m2.SetAZ(60)
	m2.SetEL(20)

	opened := 0
	opener := func() (io.ReadWriteCloser, error) {
		opened++
		if opened == 1 {
			return m1, nil
		}
		return m2, nil
	}
	d := startDriver(t, testConfig(opener))

	eventually(t, "recovery on a later tick", func() bool {
		el, ok := d.Readback()
		return d.Online() && ok && el == 60 // the AZ digits ARE the elevation
	})
	if !hasLine(m2.Writes(), "C2") {
		t.Errorf("no polls on the reopened port: %q", m2.Writes())
	}
	// The Down transition cleared readback validity; the healed link is
	// valid again only via the fresh reply — asserted above through ok==true.
}

// A goto whose write faults must return the error to the caller — the
// intent never hit the wire, and nothing is silently re-sent later
// (re-sending a motion command with no operator behind it is the
// ultrabridge stale-cmd pattern). The link self-heals; polling resumes; the
// failed motion stays failed.
func TestFailedGotoReturnsErrorToCaller(t *testing.T) {
	m := NewMock()
	m.SetAZ(123)
	m.SetEL(10)
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))

	eventually(t, "first poll on the wire", func() bool { return hasLine(m.Writes(), "C2") })

	m.FailNextW()
	if err := d.SetTarget(30); err == nil {
		t.Fatal("SetTarget with a scripted W write fault returned nil — the caller must learn the goto failed")
	}

	// The link self-heals (the fault took it Down through the reopen path)
	// and polling resumes — but the goto is NOT re-sent.
	eventually(t, "link healed after the write fault", func() bool {
		el, ok := d.Readback()
		return d.Online() && ok && el == 123
	})
	time.Sleep(60 * time.Millisecond) // several healed poll cycles
	for _, l := range m.Writes() {
		if strings.HasPrefix(l, "W") {
			t.Errorf("motion reached the wire after a failed goto: %q", l)
		}
	}
}

// blockingRW is a port whose Write and Read park until Close — the stand-in
// for a wedged tty fd (the same fake the SPID driver's watchdog test uses).
type blockingRW struct {
	mu     sync.Mutex
	closed bool
	parked chan struct{}
}

func newBlockingRW() *blockingRW { return &blockingRW{parked: make(chan struct{})} }

func (w *blockingRW) Write(p []byte) (int, error) {
	<-w.parked
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func (w *blockingRW) Read(p []byte) (int, error) {
	<-w.parked
	return 0, io.ErrClosedPipe
}

func (w *blockingRW) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	close(w.parked)
	return nil
}

// TestWriteStallWatchdogKeepsStateAnswerableAndHeals pins the wedged-fd case:
// the init write parked inside a dead fd must not freeze Online()/Readback()
// (the state mutex is never held across port I/O), the watchdog must close
// the stalled handle, and the existing reopen self-heal must recover through
// a fresh open.
func TestWriteStallWatchdogKeepsStateAnswerableAndHeals(t *testing.T) {
	stuck := newBlockingRW()
	m := NewMock()
	m.SetAZ(75)
	m.SetEL(20)
	opens := 0
	opener := func() (io.ReadWriteCloser, error) {
		opens++
		if opens == 1 {
			return stuck, nil
		}
		return m.Port(), nil
	}
	cfg := testConfig(opener)
	cfg.WriteTimeout = 30 * time.Millisecond
	d := startDriver(t, cfg)

	// Wait for the initial open, then give the init write a moment to wedge
	// inside the stuck fd (the watchdog arms for WriteTimeout, so the write
	// is still parked at this point).
	eventually(t, "initial open", func() bool { return d.Online() })
	time.Sleep(10 * time.Millisecond)

	// While the write is wedged, the state reads must answer promptly: port
	// I/O holds ioMu, never d.mu.
	answerable := make(chan struct{}, 2)
	go func() { _ = d.Online(); answerable <- struct{}{} }()
	go func() { _, _ = d.Readback(); answerable <- struct{}{} }()
	for i := 0; i < 2; i++ {
		select {
		case <-answerable:
		case <-time.After(time.Second):
			t.Fatal("state read blocked while a port write was wedged — d.mu must not serialize behind port I/O")
		}
	}

	// The watchdog fires: the stalled handle is closed, the link goes Down,
	// and the reopen path engages the healthy port — the poll recovers a
	// fresh valid readback through it.
	eventually(t, "watchdog stall converted into a heal", func() bool {
		el, ok := d.Readback()
		return d.Online() && ok && el == 75 // the AZ digits ARE the elevation
	})
	if opens < 2 {
		t.Errorf("opener attempts = %d, want >= 2 (the watchdog must feed the reopen path)", opens)
	}
}

func TestOpenRetriesIndefinitely(t *testing.T) {
	m := NewMock()
	m.SetAZ(5)
	m.SetEL(5)
	attempts := 0
	opener := func() (io.ReadWriteCloser, error) {
		attempts++
		if attempts <= 2 {
			return nil, errors.New("device absent (USB re-enumeration window)")
		}
		return m, nil
	}
	d := startDriver(t, testConfig(opener))

	eventually(t, "online after retries", func() bool {
		_, ok := d.Readback()
		return d.Online() && ok
	})
	if attempts < 3 {
		t.Errorf("opener attempts = %d, want >= 3", attempts)
	}
}

// --- boot path ----------------------------------------------------------------

// The boot path must issue NOTHING before the first C2 poll: the live bench
// ERC-M goes unresponsive to subsequent input after the unknown rFMW, so a
// boot-time firmware probe poisons the link (and every cooldown reopen
// re-poisons it). The driver therefore never writes rFMW at all and
// /meta.device.firmware stays empty.
func TestBootIssuesNoFirmwareProbe(t *testing.T) {
	m := NewMock()
	m.SetFirmware(testFirmware) // the mock WOULD answer rFMW — the driver must not ask
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))

	eventually(t, "online with a valid readback", func() bool {
		_, ok := d.Readback()
		return d.Online() && ok
	})
	if fw := d.Firmware(); fw != "" {
		t.Errorf("Firmware() = %q, want empty (no boot probe)", fw)
	}
	if hasLine(m.Writes(), "rFMW") {
		t.Errorf("boot path probed rFMW — this poisons the live ERC-M link: %q", m.Writes())
	}
}

// --- mock parity (scriptable, same contract as U2's) --------------------------------

// The mock must be scriptable: a staged raw reply overrides the computed one,
// which lets U4+ tests drive A-mode shapes and odd vendor spellings.
func TestMockScriptedAModeReply(t *testing.T) {
	m := NewMock()
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))
	eventually(t, "driver polling", func() bool { return hasLine(m.Writes(), "C2") })

	m.Script("+0123+0045") // GS-232A shape must still parse (KTD6 tolerance)
	eventually(t, "A-mode reply parsed: elevation from the az digits", func() bool {
		el, ok := d.Readback()
		return ok && el == 123
	})

	if err := d.SetTarget(50); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	eventually(t, "W with el as the az operand", func() bool {
		return hasLine(m.Writes(), "W050 000")
	})
}

// An empty configured port selects the in-process mock device (KTD7) — the
// whole driver runs without hardware, and a goto still moves the mock axis.
func TestMockModeEmptyPort(t *testing.T) {
	cfg := Config{
		Port:           "", // mock mode
		PollInterval:   5 * time.Millisecond,
		ReopenCooldown: 5 * time.Millisecond,
		ReplyTimeout:   time.Second,
	}
	d := startDriver(t, cfg)

	eventually(t, "mock-mode readback valid", func() bool {
		el, ok := d.Readback()
		return ok && el == 0
	})
	if err := d.SetTarget(45); err != nil {
		t.Fatalf("SetTarget in mock mode: %v", err)
	}
	eventually(t, "mock axis moved to target", func() bool {
		el, ok := d.Readback()
		return ok && el == 45
	})
}

// Concurrent SetTarget/Stop/Readback while the poll loop runs must serialize
// (run with -race).
func TestConcurrentWriteAndPollSerialize(t *testing.T) {
	m := NewMock()
	m.SetAZ(100)
	m.SetEL(10)
	cfg := Config{
		PollInterval:   2 * time.Millisecond,
		ReopenCooldown: 2 * time.Millisecond,
		ReplyTimeout:   time.Second,
		Opener:         func() (io.ReadWriteCloser, error) { return m.Port(), nil },
	}
	d := startDriver(t, cfg)
	eventually(t, "valid readback", func() bool { _, ok := d.Readback(); return ok })

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < 200; i++ {
				switch r.Intn(3) {
				case 0:
					_ = d.SetTarget(float64(r.Intn(91)))
				case 1:
					_ = d.Stop()
				default:
					_, _ = d.Readback()
					_ = d.Online()
					_ = d.Firmware()
				}
			}
		}(int64(g))
	}
	wg.Wait()
	if !d.Online() {
		t.Error("driver went offline under concurrent access")
	}
}
