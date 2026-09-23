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
	_ Axis = (*Driver)(nil)
	_ Axis = (*Mock)(nil)
)

func TestMockAndDriverSatisfyAxis(t *testing.T) {
	// The var _ assertions above are the real proof; this runtime smoke drives
	// the full mock stack (frames, wire, owner loop) once end-to-end.
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
// the minimum write pace, and a watchdog-fast write timeout (real deployments
// use the 300 ms Rot1Prog pace and the 3 s write default).
func fastOpts() Opts {
	return Opts{
		PollInterval:   2 * time.Millisecond,
		ReopenCooldown: 5 * time.Millisecond,
		WritePace:      time.Millisecond,
		ReadTimeout:    50 * time.Millisecond,
		WriteTimeout:   30 * time.Millisecond,
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

// --- the single-owner reply ownership (the 2026-09-23 incidents) ---------------

// TestStatusExchangeOwnsItsReply pins the structural property the old design
// lacked: the first frame after a status request IS its reply — including an
// all-zero frame, which is a legal position (register count exactly 0 after
// the mod-720 wrap), not an ACK to guess away. The stale set-ACK of the OLD
// design (the live "erroneous az 0") cannot pair wrongly because the owner
// consumes each exchange's reply itself.
func TestStatusExchangeOwnsItsReply(t *testing.T) {
	rw := newScriptedRW(0)
	st := &linkState{rw: rw}
	d := NewDriver(func() (io.ReadWriteCloser, error) { return rw, nil }, fastOpts(), nil)

	frame, silent, err := d.exchange(st, encodeStatus(), wantStatus)
	if err != nil || silent {
		t.Fatalf("status exchange = (%v, silent=%v, err=%v), want the zero frame", frame, silent, err)
	}
	az, derr := decodeStatusReply(frame)
	if derr != nil || az != 0 {
		t.Errorf("all-zero status decoded to (%v, %v), want (0, nil) — a register resting on count 0 is a position", az, derr)
	}
}

// TestSetAckSwallowedOnlyWhenCommandOutstanding pins the command-accounting
// fix: the all-zero flood ACK is swallowed after a SET (ackPending), but a
// device answering all-zero with NO set outstanding is a real position and
// must be returned. Frame shape alone cannot discriminate (the review's
// isCommandAck finding); the owner's own write history does.
func TestSetAckSwallowedOnlyWhenCommandOutstanding(t *testing.T) {
	// With a SET outstanding: the zero frame is its ACK; the status reply
	// behind it carries the position.
	rw := newScriptedRW(180)
	rw.stage(encodeAck())
	d := NewDriver(func() (io.ReadWriteCloser, error) { return rw, nil }, fastOpts(), nil)
	st := &linkState{rw: rw, ackPending: true}
	frame, silent, err := d.exchange(st, encodeStatus(), wantStatus)
	if err != nil || silent {
		t.Fatalf("exchange after set = (silent=%v, err=%v), want the status frame", silent, err)
	}
	if az, derr := decodeStatusReply(frame); derr != nil || az != 180 {
		t.Errorf("status behind a set ACK decoded to (%v, %v), want (180, nil)", az, derr)
	}
	if st.ackPending {
		t.Error("ackPending must clear once the ACK was consumed")
	}

	// Without a SET outstanding: the same zero frame is a real az 0.
	rw2 := newScriptedRW(0)
	d2 := NewDriver(func() (io.ReadWriteCloser, error) { return rw2, nil }, fastOpts(), nil)
	st2 := &linkState{rw: rw2}
	frame2, silent2, err2 := d2.exchange(st2, encodeStatus(), wantStatus)
	if err2 != nil || silent2 {
		t.Fatalf("exchange without set = (silent=%v, err=%v), want the zero frame", silent2, err2)
	}
	if az, derr := decodeStatusReply(frame2); derr != nil || az != 0 {
		t.Errorf("zero status without a set outstanding decoded to (%v, %v), want (0, nil)", az, derr)
	}
}

// TestMockStopAckDoesNotSurfaceAsZero runs the same contract end-to-end
// through the mock, which answers stop with the spec's zero reply (it long
// answered with nothing, which is why the stack never caught the misread).
// The tracker's 1 Hz stop flood is exactly this path under load.
func TestMockStopAckDoesNotSurfaceAsZero(t *testing.T) {
	m := NewMock(fastOpts())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.RunPoll(ctx)

	if !waitCond(2*time.Second, func() bool {
		_, valid := m.Readback()
		return valid
	}) {
		t.Fatal("mock never produced a first status reply")
	}
	m.SetAz(180)
	if !waitCond(2*time.Second, func() bool {
		az, valid := m.Readback()
		return valid && az == 180
	}) {
		az, valid := m.Readback()
		t.Fatalf("readback = (%v, %v), want (180, true)", az, valid)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Hold across several poll cycles: the stop reply must be drained, never
	// surface as az 0, and the position readout must hold at 180.
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		if az, _ := m.Readback(); az != 180 {
			t.Fatalf("readback regressed to %v — a stop reply leaked into the position cache", az)
		}
		time.Sleep(time.Millisecond)
	}
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
// received bytes against the Appendix A table. The owner loop runs, so poll
// frames interleave; the assertion is that the set frame appears byte-exact.
func TestSetTargetWritesAppendixFrame(t *testing.T) {
	m := NewMock(fastOpts())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.RunPoll(ctx)

	if err := m.SetTarget(123); err != nil {
		t.Fatalf("SetTarget(123): %v", err)
	}
	if !waitCond(2*time.Second, func() bool {
		for _, w := range m.Writes() {
			if string(w) == string(wantSet123) {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("device never saw the set frame %s; saw %d writes", hex(wantSet123), len(m.Writes()))
	}
	if m.Target() != 123 {
		t.Errorf("device-side commanded target = %v, want 123", m.Target())
	}
}

// TestStopFrameUsesK0F pins the stop packet byte-exactly: position fields
// zeroed, K = 0x0F, riding the wire after a set.
func TestStopFrameUsesK0F(t *testing.T) {
	m := NewMock(fastOpts())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.RunPoll(ctx)

	if err := m.SetTarget(90); err != nil {
		t.Fatalf("SetTarget(90): %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !waitCond(2*time.Second, func() bool {
		for _, w := range m.Writes() {
			if len(w) == 13 && w[11] == kStop {
				return true
			}
		}
		return false
	}) {
		t.Fatal("device never saw the stop frame")
	}
	for _, w := range m.Writes() {
		if len(w) == 13 && w[11] == kStop && string(w) != string(encodeStop()) {
			t.Fatalf("stop frame\n got %s\nwant %s", hex(w), hex(encodeStop()))
		}
	}
}

// --- self-heal (KTD7) ----------------------------------------------------------

// TestScriptedReadErrorReopenRecovers covers the live ultrabridge failure
// class: a port-level read fault mid-poll must take the axis down (clearing
// readback validity — the deadband may never trust a pre-outage position),
// then the owner must reopen the path and recover, all without anyone
// restarting the driver.
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

	// Recovery on a later tick: reopen, fresh owner state, valid readback.
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
// not wedge or panic — the owner keeps retrying and recovers.
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

// TestWriteFaultReopensAndRetries pins the write-side self-heal: a write
// erroring on a dead handle takes the link down, the owner heals onto the
// healthy handle, and the frame the caller asked for still lands — with no
// torn or duplicated bytes from the dead attempt (single owner: the retry
// is sequential, not concurrent with the fault).
func TestWriteFaultReopensAndRetries(t *testing.T) {
	dead := &scriptedRW{writeErr: errors.New("input/output error")}
	healthy := newScriptedRW(200)
	first := true
	opener := func() (io.ReadWriteCloser, error) {
		if first {
			first = false
			return dead, nil
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
		t.Fatalf("write-fault heal never recovered: readback (%v, %v), online %v, err %q", az, valid, d.Online(), d.Err())
	}
	// The link must be up afterwards — the fault belonged to the dead handle.
	if err := d.SetTarget(200); err != nil {
		t.Errorf("SetTarget on the healed link: %v", err)
	}
}

// --- silent-controller liveness (consecutive read timeouts) ----------------------

// TestConsecutiveReadTimeoutsTakeLinkDown pins the powered-off-rotor case: a
// live USB adapter whose controller never answers must NOT keep
// device_online=true with a frozen-but-valid readback forever (the deadband
// would then no-op real writes against the stale position). After
// maxConsecutiveTimeouts silent status exchanges the link must go down and
// the readback must be invalid — the internal/ercm exchange-timeout contract,
// applied to the owner loop.
func TestConsecutiveReadTimeoutsTakeLinkDown(t *testing.T) {
	rw := newScriptedRW(200)
	rw.setSwallow(1 << 30) // silent forever
	opens := 0
	opener := func() (io.ReadWriteCloser, error) {
		if opens == 0 {
			opens++
			return rw, nil
		}
		return nil, errors.New("no such file or directory")
	}
	d := NewDriver(opener, fastOpts(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.RunPoll(ctx)

	// Wait for the initial open, then for the bounded silence to take the
	// link down.
	if !waitCond(2*time.Second, func() bool { return d.Online() }) {
		t.Fatal("initial open never came up")
	}
	if !waitCond(2*time.Second, func() bool { return !d.Online() }) {
		t.Fatal("a permanently silent controller must take the link down after the consecutive-timeout bound — not stay online with a frozen readback")
	}
	if _, valid := d.Readback(); valid {
		t.Fatal("the frozen cached readback must be invalid after the timeout-driven link-down (the deadband must always write again)")
	}
	if d.Err() == "" {
		t.Error("the timeout-driven link-down must surface an error string (feeds /state.error)")
	}
}

// TestPollTimeoutsBelowBoundKeepLinkUp proves the other side of the bound:
// brief silence (maxConsecutiveTimeouts-1 missed replies) keeps the link up —
// the count resets on every reply — and only a permanently silent controller
// trips it afterwards.
func TestPollTimeoutsBelowBoundKeepLinkUp(t *testing.T) {
	rw := newScriptedRW(200)
	rw.setSwallow(maxConsecutiveTimeouts - 1) // the first N-1 status polls go unanswered
	opens := 0
	opener := func() (io.ReadWriteCloser, error) {
		if opens == 0 {
			opens++
			return rw, nil
		}
		return nil, errors.New("no such file or directory")
	}
	d := NewDriver(opener, fastOpts(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.RunPoll(ctx)

	// The first maxConsecutiveTimeouts-1 polls time out without tripping the
	// link, so the Nth reply must land while the axis is still online.
	if !waitCond(2*time.Second, func() bool {
		az, valid := d.Readback()
		return valid && az == 200
	}) {
		az, valid := d.Readback()
		t.Fatalf("brief silence below the bound must not take the link down: readback (%v, %v), online %v", az, valid, d.Online())
	}

	// Now silence the healthy controller for good: the count restarts from
	// the last reply, trips after maxConsecutiveTimeouts more ticks, and the
	// link goes down with the readback invalidated.
	rw.setSwallow(1 << 30)
	if !waitCond(2*time.Second, func() bool { return !d.Online() }) {
		t.Fatal("a controller going permanently silent must take the link down after the bounded consecutive timeouts")
	}
	if _, valid := d.Readback(); valid {
		t.Fatal("the timeout-driven link-down must clear readback validity (KTD7 deadband)")
	}
}

// --- write watchdog (wedged fd) ---------------------------------------------------

// blockingRW is an io.ReadWriteCloser whose Write and Read park until Close —
// the wedged tty fd the watchdog must convert into an error. Close unblocks
// every parked call (what closing a serial fd does to a blocked write) and is
// idempotent because the heal path closes the same handle again.
type blockingRW struct {
	mu     sync.Mutex
	closed bool
	parked chan struct{}
}

func newBlockingRW() *blockingRW {
	return &blockingRW{parked: make(chan struct{})}
}

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
// a status write parked inside a dead fd must not freeze Online()/Readback()
// (the cached state lock is never held across port I/O), the watchdog must
// close the stalled handle, and the self-heal path must reopen the healthy
// port and recover the readback.
func TestWriteStallWatchdogKeepsStateAnswerableAndHeals(t *testing.T) {
	stuck := newBlockingRW()
	healthy := newScriptedRW(200)
	opens := 0
	opener := func() (io.ReadWriteCloser, error) {
		if opens == 0 {
			opens++
			return stuck, nil
		}
		return healthy, nil
	}
	opts := fastOpts()
	opts.WriteTimeout = 30 * time.Millisecond
	d := NewDriver(opener, opts, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.RunPoll(ctx)

	// Wait for the initial open, then give the owner loop one tick to wedge
	// its first status write inside the stuck fd (the watchdog arms for
	// WriteTimeout, so the write is still parked at this point).
	if !waitCond(2*time.Second, func() bool { return d.Online() }) {
		t.Fatal("initial open never came up")
	}
	time.Sleep(10 * time.Millisecond)

	// While the write is wedged, Online() must answer promptly: the owner is
	// stuck in port I/O, but the published state never serializes behind it.
	onlineCh := make(chan bool, 1)
	go func() { onlineCh <- d.Online() }()
	select {
	case up := <-onlineCh:
		if !up {
			t.Fatal("the link must read online while the write is merely wedged")
		}
	case <-time.After(time.Second):
		t.Fatal("Online() blocked while a port write was wedged — the cached state must not serialize behind port I/O")
	}

	// The watchdog fires: the stalled handle is closed, the link marked down,
	// and the heal path engages the healthy port — the poll recovers a fresh
	// valid readback through it.
	if !waitCond(2*time.Second, func() bool {
		az, valid := d.Readback()
		return valid && az == 200 && d.Online()
	}) {
		az, valid := d.Readback()
		t.Fatalf("the watchdog never converted the stalled write into a heal: readback (%v, %v), online %v, err %q",
			az, valid, d.Online(), d.Err())
	}
	if d.Err() != "" {
		t.Errorf("recovered link still carries an error string: %q", d.Err())
	}
}

// --- write pacing and serialization --------------------------------------------

// TestWritePaceSeparatesWrites proves the ≥300 ms Rot1Prog write pacing (Appendix
// A: hamlib post_write_delay): back-to-back writes never beat the pace — now
// including commands queued through the owner channel between poll writes.
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
// plus the owner loop against one axis; every frame the fake controller
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

// TestCommandsFailAfterOwnerStops pins shutdown: once the owner loop exited,
// SetTarget/Stop report an error instead of blocking or silently queueing.
func TestCommandsFailAfterOwnerStops(t *testing.T) {
	m := NewMock(fastOpts())
	ctx, cancel := context.WithCancel(context.Background())
	go m.RunPoll(ctx)
	if !waitCond(2*time.Second, func() bool {
		_, valid := m.Readback()
		return valid
	}) {
		t.Fatal("mock never came up")
	}
	cancel()
	if !waitCond(2*time.Second, func() bool {
		select {
		case <-m.drv.done:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("owner loop never exited after ctx cancel")
	}
	if err := m.SetTarget(10); err == nil {
		t.Error("SetTarget after owner exit must error, not queue forever")
	}
	if err := m.Stop(); err == nil {
		t.Error("Stop after owner exit must error, not queue forever")
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

// --- scripted raw port (fault/liveness tests below the mock level) --------------

// scriptedRW is a minimal scripted io.ReadWriteCloser: writes either fail
// (writeErr) or are recorded and answered like a Rot1Prog controller (a
// status request stages the matching raw-digit reply); reads drain staged
// replies, honor SetReadTimeout (returning (0, nil) on lapse — the go.bug.st
// contract the owner's bounded exchanges rely on), or block until Close. It
// lets tests drive the driver's fault paths directly, independent of the
// in-memory mock wire. swallowStatus simulates a SILENT controller (a
// powered-off rotor behind a live adapter): the first N status requests get
// no reply at all.
type scriptedRW struct {
	mu       sync.Mutex
	writeErr error
	az       float64 // canned position answered to status requests
	closed   bool
	replies  [][]byte
	wake     chan struct{}

	// swallowStatus: the first N status requests stay unanswered (0 = answer
	// everything, the original behavior). setSwallow can raise it live.
	swallowStatus int
	statusCount   int // status requests seen so far

	readTimeout time.Duration
}

// setSwallow retargets the silence knob live (e.g. silence the controller
// after a healthy phase to prove the consecutive-timeout trip).
func (w *scriptedRW) setSwallow(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.swallowStatus = n
}

// SetReadTimeout implements the readTimeoutSetter the owner configures after
// open; a read with nothing staged returns (0, nil) once it lapses.
func (w *scriptedRW) SetReadTimeout(d time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.readTimeout = d
	return nil
}

// stage prepends raw reply frames ahead of the canned status flow — e.g. a
// command ACK already queued while the next status reply is still pending.
func (w *scriptedRW) stage(frames ...[]byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.replies = append(frames, w.replies...)
	w.wakeReaders()
}

func (w *scriptedRW) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if len(p) == 13 && p[0] == 0x57 && p[11] == kStatus {
		w.statusCount++
		if w.statusCount > w.swallowStatus {
			w.replies = append(w.replies, encodeStatusReply(w.az))
			w.wakeReaders()
		}
	}
	return len(p), nil
}

func (w *scriptedRW) Read(p []byte) (int, error) {
	var deadline <-chan time.Time
	w.mu.Lock()
	rt := w.readTimeout
	w.mu.Unlock()
	if rt > 0 {
		timer := time.NewTimer(rt)
		defer timer.Stop()
		deadline = timer.C
	}
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
		case <-deadline:
			return 0, nil
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
