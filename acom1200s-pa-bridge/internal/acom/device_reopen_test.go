package acom

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.bug.st/serial"
)

// mockPort is a scriptable serial.Port. Read/write behaviour comes from the
// fn hooks so a test can simulate a USB-serial adapter that drops mid-run and
// recovers on reopen (same pattern as ultrabridge's transport tests).
type mockPort struct {
	mu     sync.Mutex
	readFn func(p []byte) (int, error)
	writes [][]byte
	closed bool
}

func (m *mockPort) SetMode(*serial.Mode) error { return nil }
func (m *mockPort) Drain() error               { return nil }
func (m *mockPort) ResetInputBuffer() error    { return nil }
func (m *mockPort) ResetOutputBuffer() error   { return nil }
func (m *mockPort) SetDTR(bool) error          { return nil }
func (m *mockPort) SetRTS(bool) error          { return nil }
func (m *mockPort) GetModemStatusBits() (*serial.ModemStatusBits, error) {
	return &serial.ModemStatusBits{}, nil
}
func (m *mockPort) SetReadTimeout(time.Duration) error { return nil }
func (m *mockPort) Break(time.Duration) error          { return nil }

func (m *mockPort) Read(p []byte) (int, error) {
	m.mu.Lock()
	fn := m.readFn
	m.mu.Unlock()
	if fn == nil {
		return 0, nil
	}
	return fn(p)
}

func (m *mockPort) Write(p []byte) (int, error) {
	m.mu.Lock()
	m.writes = append(m.writes, append([]byte(nil), p...))
	m.mu.Unlock()
	return len(p), nil
}

func (m *mockPort) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockPort) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *mockPort) written() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.writes...)
}

// errPortRead is the fault a dropped USB-serial adapter shows: read on the
// stale handle returns EIO immediately.
func errPortRead([]byte) (int, error) {
	return 0, errors.New("input/output error")
}

// quietPortRead simulates a healthy but idle link: the 1 s read timeout fires
// with no data (Run's silence handling owns that case; here it just keeps the
// loop alive).
func quietPortRead(p []byte) (int, error) {
	time.Sleep(5 * time.Millisecond)
	return 0, nil
}

// TestRunReopensAfterReadFault pins the self-heal contract: a read fault (the
// live symptom of an adapter drop + re-enumeration) must be healed by one
// in-place reopen — stale handle closed, by-id path re-resolved, telemetry
// re-armed — WITHOUT Run returning, so /state.device_online never flaps and
// the serial restart loop's backoff is never entered for a transient glitch.
func TestRunReopensAfterReadFault(t *testing.T) {
	flaky := &mockPort{readFn: errPortRead}
	healthy := &mockPort{readFn: quietPortRead}

	d := New("/dev/serial/by-id/x", 300, false, noopLog{})
	var opens atomic.Int32
	d.open = func() (serial.Port, error) {
		if opens.Add(1) == 1 {
			return flaky, nil // initial handle: faults on first read
		}
		return healthy, nil // reopen: fresh handle, resolves the new tty
	}
	if err := d.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, nil) }()

	// Long enough for the first fault, the reopen, and a healthy read tick.
	select {
	case err := <-done:
		t.Fatalf("Run returned after a transient fault (self-heal failed): %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// The reopen itself: exactly one extra open, stale handle closed,
	// enable-telemetry re-armed on the fresh handle, device still online.
	if n := opens.Load(); n != 2 {
		t.Fatalf("expected exactly 2 opens (initial + one reopen), got %d", n)
	}
	if !flaky.isClosed() {
		t.Fatal("stale port handle was not closed on reopen")
	}
	writes := healthy.written()
	if len(writes) == 0 {
		t.Fatal("no telemetry re-arm sent on the fresh handle after reopen")
	} else if writes[0][0] != StartByte || writes[0][1] != MsgEnableAuto {
		t.Fatalf("first write after reopen = % X, want enable-telemetry frame", writes[0])
	}
	if !d.Online() {
		t.Fatal("deviceOnline flapped false during the in-place reopen")
	}

	// Clean shutdown: cancel must still end Run promptly.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestRunSurfacesErrorWhenReopenFails ensures a permanently broken link still
// surfaces an error (no infinite silent retry): when the by-id path can never
// be re-resolved, the in-place reopen retries continue at the rate-bound
// cadence until the silence watchdog ends the run for the serial restart
// loop's backoff — the same budget a connected-but-silent amp gets.
func TestRunSurfacesErrorWhenReopenFails(t *testing.T) {
	setKnobs(t, 20*time.Millisecond, 150*time.Millisecond)

	d := New("/dev/serial/by-id/x", 300, false, noopLog{})
	var opens atomic.Int32
	d.open = func() (serial.Port, error) {
		if opens.Add(1) == 1 {
			return &mockPort{readFn: errPortRead}, nil
		}
		return nil, errors.New("no such device") // adapter gone for good
	}
	if err := d.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}

	start := time.Now()
	err := d.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected Run to return when the adapter is gone for good, got nil")
	}
	if !strings.Contains(err.Error(), "no data received") {
		t.Fatalf("expected the silence watchdog to end the run, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Run took %v to give up on a missing adapter", elapsed)
	}
	if n := opens.Load(); n < 3 {
		t.Fatalf("expected the missing by-id path to be retried in place (opens=%d), not given up after one", n)
	}
}

// TestRunRetriesReopenAcrossReenumeration pins the canonical live fault this
// whole change exists for: the adapter drops (EIO on the stale handle) and the
// kernel re-enumerates it, so the by-id path is NOT yet resolvable when the
// first reopen fires — it appears only once udev recreates the symlink a
// moment later. The run must retry the open in place and heal, without Run
// returning and without /state.device_online flipping. The one-shot reopen
// (before the retry path existed) lost this race every time.
func TestRunRetriesReopenAcrossReenumeration(t *testing.T) {
	setKnobs(t, 30*time.Millisecond, 10*time.Second)

	start := time.Now()
	var healthy atomic.Pointer[mockPort]
	var opens atomic.Int32
	d := New("/dev/serial/by-id/x", 300, false, noopLog{})
	d.open = func() (serial.Port, error) {
		switch n := opens.Add(1); {
		case n == 1:
			return &mockPort{readFn: errPortRead}, nil // initial handle: EIO, adapter dropped
		case time.Since(start) < 120*time.Millisecond:
			// udev has not recreated the by-id symlink yet — the first reopen
			// (and any retry inside this window) hits ENOENT.
			return nil, errors.New("no such file or directory")
		default:
			p := &mockPort{readFn: quietPortRead}
			healthy.Store(p)
			return p, nil
		}
	}
	if err := d.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, nil) }()

	// The path comes back ~120 ms in; the retry loop picks it up within the
	// next rate window. Give it room, then require a stable healed run.
	select {
	case err := <-done:
		t.Fatalf("Run returned despite the adapter coming back (udev race lost): %v", err)
	case <-time.After(400 * time.Millisecond):
	}

	if p := healthy.Load(); p == nil {
		t.Fatal("the healed handle was never opened — reopen did not retry across re-enumeration")
	} else {
		writes := p.written()
		if len(writes) == 0 {
			t.Fatal("no telemetry re-arm sent on the healed handle")
		} else if writes[0][0] != StartByte || writes[0][1] != MsgEnableAuto {
			t.Fatalf("first write after heal = % X, want enable-telemetry frame", writes[0])
		}
	}
	if !d.Online() {
		t.Fatal("deviceOnline flapped false across the re-enumeration")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestRunRateBoundsReopen pins the tight-loop bound: a port that opens fine
// but faults again immediately (flapping link) must get exactly ONE in-place
// reopen, then fall back to returning the error for the restart loop instead
// of spinning reopen/Read forever.
func TestRunRateBoundsReopen(t *testing.T) {
	d := New("/dev/serial/by-id/x", 300, false, noopLog{})
	var opens atomic.Int32
	d.open = func() (serial.Port, error) {
		opens.Add(1)
		return &mockPort{readFn: errPortRead}, nil // always faults
	}
	if err := d.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}

	start := time.Now()
	err := d.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected Run to return on a flapping link, got nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("flapping link fell back after %v; rate bound not enforced", elapsed)
	}
	if n := opens.Load(); n != 2 {
		t.Fatalf("expected exactly 2 opens (initial + one rate-bounded reopen), got %d", n)
	}
}

// setKnobs compresses the self-heal timing knobs for a test and restores them
// on cleanup. The production values (2 s / 30 s) are far too slow to observe
// in a unit test.
func setKnobs(t *testing.T, reopenEvery, silence time.Duration) {
	t.Helper()
	oldReopen, oldSilence := reopenMinInterval, silenceLimit
	reopenMinInterval, silenceLimit = reopenEvery, silence
	t.Cleanup(func() { reopenMinInterval, silenceLimit = oldReopen, oldSilence })
}

// waitOpens blocks until the open factory has been called n times (or fails).
func waitOpens(t *testing.T, opens *atomic.Int32, n int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for opens.Load() < n && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := opens.Load(); got < n {
		t.Fatalf("timed out waiting for %d opens, saw %d", n, got)
	}
}

// TestRunRehealsAfterWindow pins the rate bound's recovery half, which a
// one-shot-per-Run heal would break: a SECOND fault, outside the
// reopenMinInterval window after a successful reopen, must heal again in place
// (Run stays up) rather than fall through to the restart loop. The bound
// exists to space reopens, not to ration them.
//
// The scenario is time-scripted on the ports themselves (each fresh handle
// mirrors a by-id re-resolve) rather than toggled from the test, so nothing
// races the read loop: handle #1 faults immediately; handle #2 is healthy for
// 100 ms (>> the 30 ms reopen window) then faults; handle #3 is healthy.
func TestRunRehealsAfterWindow(t *testing.T) {
	setKnobs(t, 30*time.Millisecond, 10*time.Second)

	mkPort := func(healthyFor time.Duration) *mockPort {
		born := time.Now()
		return &mockPort{readFn: func(p []byte) (int, error) {
			time.Sleep(2 * time.Millisecond) // reads take a moment, like a real port
			if time.Since(born) < healthyFor {
				return 0, nil // quiet, healthy link
			}
			return 0, errors.New("input/output error")
		}}
	}

	d := New("/dev/serial/by-id/x", 300, false, noopLog{})
	var opens atomic.Int32
	d.open = func() (serial.Port, error) {
		switch opens.Add(1) {
		case 1:
			return mkPort(0), nil // initial handle: faults from the first read
		case 2:
			return mkPort(100 * time.Millisecond), nil // heals, then faults again
		default:
			return mkPort(time.Hour), nil // stable from here on
		}
	}
	if err := d.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx, nil) }()

	// Both glitches heal in place; the second happens ~100 ms in, well outside
	// the 30 ms reopen window.
	waitOpens(t, &opens, 3)

	select {
	case err := <-done:
		t.Fatalf("Run returned after out-of-window faults (heal is not repeatable): %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	if n := opens.Load(); n != 3 {
		t.Fatalf("expected exactly 3 opens (initial + two reopens), got %d", n)
	}
	if !d.Online() {
		t.Fatal("device went offline during in-place reheals")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// TestRunSilenceWatchdogCoversFaultPath pins the two-layer-liveness contract:
// a link that keeps faulting (so reads error, never time out) while delivering
// NO data must not be held online by repeated successful reopens — Run must
// return within the silence budget so serialLoop flips /state.device_online,
// regardless of how often the by-id path resolves. A reopen that delivers
// nothing is still silence; only real data refreshes the watchdog.
func TestRunSilenceWatchdogCoversFaultPath(t *testing.T) {
	setKnobs(t, 20*time.Millisecond, 150*time.Millisecond)

	// Faults spaced just outside the reopen window (25 ms > 20 ms): each one
	// reopens successfully — the rate bound never trips — so only the silence
	// watchdog can end the run. This is the flapping-adapter shape that would
	// otherwise hold device_online true forever.
	errPortSpacedRead := func([]byte) (int, error) {
		time.Sleep(25 * time.Millisecond)
		return 0, errors.New("input/output error")
	}

	d := New("/dev/serial/by-id/x", 300, false, noopLog{})
	var opens atomic.Int32
	d.open = func() (serial.Port, error) {
		opens.Add(1)
		return &mockPort{readFn: errPortSpacedRead}, nil // opens fine, reads fault
	}
	if err := d.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}

	start := time.Now()
	err := d.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected Run to return on a faulting, data-less link, got nil")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("silence watchdog took %v on the fault path; reopens masked a dead link", elapsed)
	}
	if !strings.Contains(err.Error(), "no data received") {
		t.Fatalf("expected silence error, got: %v", err)
	}
	if n := opens.Load(); n < 2 {
		t.Fatalf("expected the watchdog to allow in-place reopens first (opens=%d), then give up", n)
	}
}
