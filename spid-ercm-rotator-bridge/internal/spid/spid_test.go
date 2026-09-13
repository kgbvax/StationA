package spid

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"spid-ercm-rotator-bridge/internal/config"
)

// --- compile-time interface assertions ---------------------------------------
//
// The mock device satisfies the exact per-axis contract the real serial driver
// satisfies, so U4+ tests (and mock mode, KTD7) run against the same interface
// with no adapter layer.

var (
	_ Axis       = (*Driver)(nil)
	_ PollRunner = (*Driver)(nil)
	_ Axis       = (*Mock)(nil)
	_ PollRunner = (*Mock)(nil)
)

func TestMockAndDriverSatisfyAxis(t *testing.T) {
	// The var _ assertions above are the real proof; this runtime smoke drives
	// the full mock stack (frames, wire, poll) once end-to-end.
	m := NewMock(fastOpts())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.RunPoll(ctx)

	m.SetAz(217)
	if !waitCond(2*time.Second, func() bool {
		az, valid := m.Readback()
		return valid && az == 217
	}) {
		az, valid := m.Readback()
		t.Fatalf("mock end-to-end: readback = (%v, %v), want (217, true)", az, valid)
	}
	if !m.Online() {
		t.Fatal("mock end-to-end: axis must be online")
	}
	if err := m.SetTarget(217); err != nil {
		t.Fatalf("SetTarget over the mock: %v", err)
	}
}

// fastOpts keeps the driver tests fast: millisecond-scale poll/reopen cadence,
// and the minimum write pace (real deployments use the 300 ms Rot1Prog pace).
func fastOpts() Opts {
	return Opts{
		PollInterval:   2 * time.Millisecond,
		ReopenCooldown: 5 * time.Millisecond,
		WritePace:      time.Millisecond,
		ReadTimeout:    50 * time.Millisecond,
	}
}

func waitCond(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

// --- readback cache -----------------------------------------------------------

func TestPollPopulatesCachedReadbackAndClearsUnknown(t *testing.T) {
	m := NewMock(fastOpts())

	az, valid := m.Readback()
	if valid {
		t.Fatal("readback validity must start unknown (false) before the first status reply (KTD7/KTD9)")
	}
	if m.Online() {
		t.Fatal("axis must start offline before the first successful open")
	}

	m.SetAz(123)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.RunPoll(ctx)

	if !waitCond(2*time.Second, func() bool {
		az, valid = m.Readback()
		return valid && az == 123
	}) {
		t.Fatalf("poll never populated the cached readback: got (%v, %v), want (123, true)", az, valid)
	}
	if !m.Online() {
		t.Fatal("axis must be online after the first successful status exchange")
	}
}

// TestSetTargetWritesAppendixFrame drives a target through the whole stack
// (driver → framing → in-memory wire → fake controller) and pins the device's
// received bytes against the Appendix A table. No poll loop runs: the first
// write opens the link on demand, so the device sees exactly this one frame.
func TestSetTargetWritesAppendixFrame(t *testing.T) {
	m := NewMock(fastOpts())
	defer m.Close()

	if err := m.SetTarget(123); err != nil {
		t.Fatalf("SetTarget(123): %v", err)
	}
	if !waitCond(2*time.Second, func() bool { return len(m.Writes()) >= 1 }) {
		t.Fatal("device never saw the write")
	}
	writes := m.Writes()
	if len(writes) != 1 {
		t.Fatalf("device saw %d writes, want exactly the one set frame", len(writes))
	}
	if string(writes[0]) != string(wantSet123) {
		t.Fatalf("device received\n got %s\nwant %s", hex(writes[0]), hex(wantSet123))
	}
	if m.Target() != 123 {
		t.Errorf("device-side commanded target = %v, want 123", m.Target())
	}
}

// TestStopFrameUsesK0F pins the stop packet byte-exactly: position fields
// zeroed, K = 0x0F. SetTarget first so the two writes are also proof that
// stop goes out as its own frame after a set.
func TestStopFrameUsesK0F(t *testing.T) {
	m := NewMock(fastOpts())
	defer m.Close()

	if err := m.SetTarget(90); err != nil {
		t.Fatalf("SetTarget(90): %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !waitCond(2*time.Second, func() bool { return len(m.Writes()) >= 2 }) {
		t.Fatal("device never saw the set and stop writes")
	}
	writes := m.Writes()
	if len(writes) != 2 {
		t.Fatalf("device saw %d writes, want set + stop", len(writes))
	}
	last := writes[1]
	if last[11] != kStop {
		t.Fatalf("stop packet K byte = 0x%02X, want 0x0F (frame %s)", last[11], hex(last))
	}
	if string(last) != string(encodeStop()) {
		t.Fatalf("stop frame\n got %s\nwant %s", hex(last), hex(encodeStop()))
	}
}

// --- self-heal (KTD7) ----------------------------------------------------------

// TestScriptedReadErrorReopenRecovers covers the live ultrabridge failure
// class: a port-level read fault mid-poll must take the axis down (clearing
// readback validity — the deadband may never trust a pre-outage position),
// then the next ticks must reopen the by-id-style path and recover, all
// without anyone restarting the driver.
func TestScriptedReadErrorReopenRecovers(t *testing.T) {
	m := NewMock(fastOpts())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.RunPoll(ctx)

	m.SetAz(45)
	if !waitCond(2*time.Second, func() bool {
		az, valid := m.Readback()
		return valid && az == 45
	}) {
		t.Fatal("driver never became valid before the fault")
	}
	conns := m.Connections()

	m.InjectError(errors.New("input/output error"))

	// Down transition: device_online false and the cached readback invalid.
	if !waitCond(2*time.Second, func() bool {
		_, valid := m.Readback()
		return !valid && !m.Online()
	}) {
		t.Fatal("read fault never took the axis down / cleared readback validity")
	}

	// Recovery on a later tick: reopen, fresh reader, valid readback again.
	m.SetAz(46)
	if !waitCond(2*time.Second, func() bool {
		az, valid := m.Readback()
		return valid && az == 46
	}) {
		az, valid := m.Readback()
		t.Fatalf("driver never recovered after reopen: readback (%v, %v), online %v", az, valid, m.Online())
	}
	if got := m.Connections(); got < conns+1 {
		t.Errorf("reopen count = %d connections, want ≥ %d (a fresh open after the fault)", got, conns+1)
	}
	if m.Err() != "" {
		t.Errorf("recovered axis still carries an error string: %q", m.Err())
	}
}

// TestReopenRetriesIndefinitelyUntilPortReturns proves the never-give-up side
// of KTD7: an opener that fails twice before the adapter re-enumerates must
// not wedge or panic — the poll loop keeps retrying and recovers.
func TestReopenRetriesIndefinitelyUntilPortReturns(t *testing.T) {
	failures := 2
	healthy := newScriptedRW(200)
	opener := func() (io.ReadWriteCloser, error) {
		if failures > 0 {
			failures--
			return nil, errors.New("no such file or directory")
		}
		return healthy, nil
	}
	d := NewDriver(opener, fastOpts(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.RunPoll(ctx)

	if !waitCond(2*time.Second, func() bool {
		az, valid := d.Readback()
		return valid && az == 200
	}) {
		az, valid := d.Readback()
		t.Fatalf("driver never recovered after %d opener failures: readback (%v, %v), online %v", 2, az, valid, d.Online())
	}
}

// TestWriteErrorReopenIgnoresStaleReader pins the generation tagging: a write
// fault triggers an immediate reopen+retry, and the OLD reader generation's
// late error (it dies on the closed stale handle) must be ignored instead of
// tearing down the freshly reopened link.
func TestWriteErrorReopenIgnoresStaleReader(t *testing.T) {
	stale := &scriptedRW{writeErr: errors.New("input/output error")}
	healthy := newScriptedRW(200)
	first := true
	opener := func() (io.ReadWriteCloser, error) {
		if first {
			first = false
			return stale, nil
		}
		return healthy, nil
	}
	d := NewDriver(opener, fastOpts(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.RunPoll(ctx)

	// The very first status poll hits the dead port: write error → reopen
	// (healthy handle) → retry → reply from the healthy port. The stale
	// reader then errors on its closed handle; generation tagging must drop
	// that error and leave the link up.
	if !waitCond(2*time.Second, func() bool {
		az, valid := d.Readback()
		return valid && az == 200
	}) {
		az, valid := d.Readback()
		t.Fatalf("write-fault reopen never recovered: readback (%v, %v), online %v, err %q", az, valid, d.Online(), d.Err())
	}
	if !d.Online() {
		t.Fatal("stale reader error must not take the reopened link down")
	}
	if d.Err() != "" {
		t.Errorf("link carries an error from a stale reader generation: %q", d.Err())
	}
	// Give the stale reader's late error a moment to arrive and be dropped.
	time.Sleep(20 * time.Millisecond)
	if !d.Online() {
		t.Fatal("stale reader error arrived late and wrongly marked the link down")
	}
	az, valid := d.Readback()
	if !valid || az != 200 {
		t.Fatalf("readback after stale-error window = (%v, %v), want (200, true)", az, valid)
	}
}

// --- write pacing and serialization --------------------------------------------

// TestWritePaceSeparatesWrites proves the ≥300 ms Rot1Prog write pacing (Appendix
// A: hamlib post_write_delay): back-to-back writes never beat the pace.
func TestWritePaceSeparatesWrites(t *testing.T) {
	const pace = 25 * time.Millisecond
	opts := fastOpts()
	opts.WritePace = pace
	m := NewMock(opts)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.RunPoll(ctx)

	for _, az := range []float64{10, 20, 30} {
		if err := m.SetTarget(az); err != nil {
			t.Fatalf("SetTarget(%v): %v", az, err)
		}
	}
	if !waitCond(2*time.Second, func() bool { return len(m.WriteTimes()) >= 3 }) {
		t.Fatal("device never saw all three writes")
	}
	times := m.WriteTimes()
	if len(times) < 3 {
		t.Fatalf("device saw %d writes, want ≥ 3", len(times))
	}
	for i := 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < pace*9/10 {
			t.Errorf("writes %d→%d only %v apart, want ≥ ~%s (Rot1Prog pace)", i-1, i, gap, pace)
		}
	}
}

// TestConcurrentWriteAndPollSerialize runs the -race gauntlet: many writers
// plus the poll loop against one axis; every frame the fake controller
// receives must be exactly one well-formed 13-byte packet — a torn or
// interleaved write would fail the frame check, and unsynchronized driver
// state would fail the race detector.
func TestConcurrentWriteAndPollSerialize(t *testing.T) {
	m := NewMock(fastOpts())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.RunPoll(ctx)

	const writers, perWriter = 4, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < perWriter; i++ {
				if err := m.SetTarget(float64(r.Intn(361))); err != nil {
					t.Errorf("SetTarget: %v", err)
					return
				}
			}
		}(int64(w))
	}
	wg.Wait()
	cancel()

	writes := m.Writes()
	if len(writes) < writers*perWriter {
		t.Fatalf("device saw %d writes, want ≥ %d", len(writes), writers*perWriter)
	}
	for i, fr := range writes {
		if len(fr) != 13 || fr[0] != 0x57 || fr[12] != 0x20 {
			t.Fatalf("write %d is not one well-formed 13-byte frame: %s", i, hex(fr))
		}
		if fr[11] != 0x2F && fr[11] != 0x1F && fr[11] != 0x0F {
			t.Fatalf("write %d carries unknown K byte 0x%02X: %s", i, fr[11], hex(fr))
		}
	}
}

// --- construction / mock-mode selection ----------------------------------------

func TestNewSelectsMockForEmptyPort(t *testing.T) {
	azSlot := config.SlotConfig{
		Axis:        config.AxisAZ,
		Slot:        "az-rotator",
		DeviceModel: "SPID Rotor (Rot1Prog)",
	}
	ctrl := config.Defaults().Control

	a, err := New(azSlot, ctrl, nil)
	if err != nil {
		t.Fatalf("New (mock): %v", err)
	}
	if _, ok := a.(*Mock); !ok {
		t.Fatalf("empty serial port must select the in-process mock device (KTD7), got %T", a)
	}

	real := azSlot
	real.Serial = config.SerialConfig{
		Port: "/dev/serial/by-id/usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0",
		Baud: 1200,
	}
	a, err = New(real, ctrl, nil)
	if err != nil {
		t.Fatalf("New (serial): %v", err)
	}
	if _, ok := a.(*Driver); !ok {
		t.Fatalf("configured port must select the serial driver, got %T", a)
	}
}

func TestNewRejectsNonAzAxis(t *testing.T) {
	elSlot := config.SlotConfig{Axis: config.AxisEL, Slot: "el-rotator", DeviceModel: "GS-500"}
	if _, err := New(elSlot, config.Defaults().Control, nil); err == nil {
		t.Fatal("the SPID driver must refuse the el axis — it fronts azimuth only")
	}
}

// --- scripted raw port (generation/pacing tests below the mock level) ----------

// scriptedRW is a minimal scripted io.ReadWriteCloser: writes either fail
// (writeErr) or are recorded and answered like a Rot1Prog controller
// (a status request stages the matching raw-digit reply); reads drain staged
// replies or block until Close. It lets tests drive the driver's fault paths
// directly, independent of the in-memory mock wire.
type scriptedRW struct {
	mu       sync.Mutex
	writeErr error
	az       float64 // canned position answered to status requests
	closed   bool
	replies  [][]byte
	wake     chan struct{}
}

func (w *scriptedRW) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if len(p) == 13 && p[0] == 0x57 && p[11] == kStatus {
		w.replies = append(w.replies, encodeStatusReply(w.az))
		w.wakeReaders()
	}
	return len(p), nil
}

func (w *scriptedRW) Read(p []byte) (int, error) {
	for {
		w.mu.Lock()
		if w.closed {
			w.mu.Unlock()
			return 0, io.ErrClosedPipe
		}
		if len(w.replies) > 0 {
			r := w.replies[0]
			w.replies = w.replies[1:]
			w.mu.Unlock()
			return copy(p, r), nil
		}
		wake := w.wake
		w.mu.Unlock()
		select {
		case <-wake:
		case <-time.After(50 * time.Millisecond):
			// Re-check for close/closed without blocking forever in tests.
		}
	}
}

func (w *scriptedRW) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.wakeReaders()
	return nil
}

func (w *scriptedRW) wakeReaders() {
	if w.wake != nil {
		close(w.wake)
	}
	w.wake = make(chan struct{})
}

func newScriptedRW(az float64) *scriptedRW {
	return &scriptedRW{az: az, wake: make(chan struct{})}
}
