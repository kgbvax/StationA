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
	m.SetFirmware(testFirmware)
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))

	eventually(t, "firmware readback", func() bool { return d.Firmware() == testFirmware })
	eventually(t, "valid readback", func() bool {
		el, ok := d.Readback()
		return ok && el == 10
	})

	if err := d.SetTarget(45); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	eventually(t, "W on the wire", func() bool { return hasLine(m.Writes(), "W123 045") })
	if m.EL() != 45 {
		t.Errorf("mock el after W = %v, want 45", m.EL())
	}
	eventually(t, "fresh readback after move", func() bool {
		el, ok := d.Readback()
		return ok && el == 45
	})
}

// The core KTD6 deferral: a goto arriving with NO cached az is deferred until
// the next poll tick supplies one — never written with a fabricated az operand.
func TestGotoDeferredUntilFirstReadback(t *testing.T) {
	m := NewMock()
	m.SetAZ(123)
	m.SetEL(10)
	m.HoldReadback(true) // C2 replies suppressed until released
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))

	eventually(t, "init exchange (rFMW)", func() bool { return hasLine(m.Writes(), "rFMW") })
	eventually(t, "a poll tick ran", func() bool { return hasLine(m.Writes(), "C2") })

	if err := d.SetTarget(30); err != nil {
		t.Fatalf("SetTarget while deferred: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // several poll cycles' worth of quiet line
	if hasLineWithPrefix(m.Writes(), "W") {
		t.Fatalf("W written before any C2 reply supplied an az: %q", m.Writes())
	}

	m.HoldReadback(false)
	m.StageReadback() // the in-flight C2 finally answers: AZ=123  EL=010
	eventually(t, "deferred W after first readback", func() bool {
		return hasLine(m.Writes(), "W123 030")
	})
	for _, l := range m.Writes() {
		if strings.HasPrefix(l, "W") && l != "W123 030" {
			t.Errorf("unexpected W spelling %q, want exactly W123 030", l)
		}
	}
}

// Readback validity starts false and lands only with the first C2 reply (KTD7).
func TestReadbackValidityLifecycle(t *testing.T) {
	m := NewMock()
	m.SetAZ(123)
	m.SetEL(10)
	m.HoldReadback(true)
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))

	eventually(t, "init exchange", func() bool { return hasLine(m.Writes(), "rFMW") })
	if _, ok := d.Readback(); ok {
		t.Fatal("readback reported valid before the first C2 reply")
	}

	m.HoldReadback(false)
	m.StageReadback()
	eventually(t, "valid readback after first reply", func() bool {
		el, ok := d.Readback()
		return ok && el == 10
	})
}

// --- stop (S/E) ------------------------------------------------------------------

func TestStopWritesEAndClearsPendingTarget(t *testing.T) {
	m := NewMock()
	m.SetAZ(123)
	m.SetEL(10)
	m.HoldReadback(true)
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))

	eventually(t, "init exchange", func() bool { return hasLine(m.Writes(), "rFMW") })

	// Park a deferred target, then stop: the stop must clear it (KTD8 spirit).
	if err := d.SetTarget(30); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if err := d.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !hasLine(m.Writes(), "E") {
		t.Fatalf("stop did not write E, writes: %q", m.Writes())
	}

	m.HoldReadback(false)
	m.StageReadback()
	time.Sleep(60 * time.Millisecond) // let a poll tick process the readback
	if hasLineWithPrefix(m.Writes(), "W") {
		t.Fatalf("deferred target survived a stop: %q", m.Writes())
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

// --- self-heal (KTD7) --------------------------------------------------------------

func TestReopenAfterScriptedError(t *testing.T) {
	m1 := NewMock()
	m1.SetFirmware(testFirmware)
	m1.FailReads(1) // the first read (rFMW at init) faults
	m2 := NewMock()
	m2.SetAZ(60)
	m2.SetEL(20)
	m2.SetFirmware(testFirmware)

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
		return d.Online() && ok && el == 20
	})
	if !hasLine(m2.Writes(), "rFMW") {
		t.Errorf("re-init (rFMW) missing after reopen: %q", m2.Writes())
	}
	if !hasLine(m2.Writes(), "C2") {
		t.Errorf("no polls on the reopened port: %q", m2.Writes())
	}
	// The Down transition cleared readback validity; the healed link is
	// valid again only via the fresh reply — asserted above through ok==true.
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

// --- rFMW firmware ----------------------------------------------------------------

func TestFirmwareFromRFMW(t *testing.T) {
	m := NewMock()
	m.SetFirmware(testFirmware)
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))
	eventually(t, "rFMW reply parsed into firmware", func() bool { return d.Firmware() == testFirmware })
}

// --- mock parity (scriptable, same contract as U2's) --------------------------------

// The mock must be scriptable: a staged raw reply overrides the computed one,
// which lets U4+ tests drive A-mode shapes and odd vendor spellings.
func TestMockScriptedAModeReply(t *testing.T) {
	m := NewMock()
	m.SetFirmware(testFirmware)
	d := startDriver(t, testConfig(func() (io.ReadWriteCloser, error) { return m.Port(), nil }))
	eventually(t, "init exchange", func() bool { return d.Firmware() == testFirmware })

	m.Script("+0123+0045") // GS-232A shape must still parse (KTD6 tolerance)
	eventually(t, "A-mode reply parsed", func() bool {
		el, ok := d.Readback()
		return ok && el == 45
	})

	if err := d.SetTarget(50); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	eventually(t, "W with the A-mode-supplied az", func() bool {
		return hasLine(m.Writes(), "W123 050")
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
