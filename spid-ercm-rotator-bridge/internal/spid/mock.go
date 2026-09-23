// SPDX-License-Identifier: AGPL-3.0-or-later

package spid

import (
	"context"
	"io"
	"sync"
	"time"
)

// Mock is the in-process SPID Rot1Prog controller plus the very same Driver
// that fronts a real controller, connected over an in-memory wire. It
// satisfies the exact Axis contract of the serial driver (compile-time
// pinned in spid_test.go), and it is what an empty configured serial port
// selects (KTD7) — so mock mode runs the whole stack: protocol listeners →
// dispatch → driver → Rot1Prog framing → device, with no hardware.
//
// The scripting surface exists for U4+ tests: canned status replies (SetAz),
// fault injection (InjectError), and a device-side write log for byte-exact
// frame assertions (Writes / WriteTimes / Target / Connections).
type Mock struct {
	drv *Driver
	dev *mockController
}

// NewMock builds the in-process mock device and its driver.
func NewMock(opts Opts) *Mock {
	dev := newMockController()
	opener := func() (io.ReadWriteCloser, error) {
		driverEnd, deviceEnd := newMemWire()
		dev.connect(deviceEnd)
		return driverEnd, nil
	}
	return &Mock{drv: NewDriver(opener, opts, nil), dev: dev}
}

// --- Axis contract (delegates to the very driver the real port uses) ------------

func (m *Mock) SetTarget(az float64) error  { return m.drv.SetTarget(az) }
func (m *Mock) Stop() error                 { return m.drv.Stop() }
func (m *Mock) Readback() (float64, bool)   { return m.drv.Readback() }
func (m *Mock) Online() bool                { return m.drv.Online() }
func (m *Mock) Err() string                 { return m.drv.Err() }
func (m *Mock) RunPoll(ctx context.Context) { m.drv.RunPoll(ctx) }

// Close tears the mock's controller goroutine down (test hygiene; the driver
// side is torn down by cancelling the RunPoll ctx).
func (m *Mock) Close() { m.dev.close() }

// --- scripting surface -------------------------------------------------------------

// SetAz sets the canned position the controller answers status requests with.
func (m *Mock) SetAz(az float64) { m.dev.setAz(az) }

// Target returns the last azimuth the device was commanded to.
func (m *Mock) Target() float64 { return m.dev.commandedAz() }

// Writes returns every command frame the controller received, in order.
func (m *Mock) Writes() [][]byte { return m.dev.writes() }

// WriteTimes returns the receive time of each logged write, aligned with
// Writes (write-pace assertions).
func (m *Mock) WriteTimes() []time.Time { return m.dev.writeTimes() }

// Connections returns how many wires the controller accepted — one per
// initial open and per successful reopen.
func (m *Mock) Connections() int { return m.dev.connections() }

// InjectError poisons the current wire: both ends fail with err, so the
// driver's next exchange takes the link down and the self-heal path reopens a
// fresh wire (the USB-unplug rehearsal).
func (m *Mock) InjectError(err error) { m.dev.injectError(err) }

// --- the fake Rot1Prog controller ---------------------------------------------------

// mockController models the controller AS THE DRIVER MEETS IT: it decodes
// real 13-byte command frames off a wire, parks instantly at commanded
// positions, answers status requests with real raw-digit replies, and logs
// every received frame. One serve goroutine; one wire in service at a time.
type mockController struct {
	mu      sync.Mutex
	az      float64 // canned position (SetAz / last commanded)
	target  float64
	frames  [][]byte
	times   []time.Time
	conns   int
	current *memPort
	closed  bool

	acceptCh chan *memPort
}

func newMockController() *mockController {
	c := &mockController{acceptCh: make(chan *memPort, 4)}
	go c.serve()
	return c
}

func (c *mockController) serve() {
	for p := range c.acceptCh {
		c.serveConn(p)
	}
}

func (c *mockController) serveConn(p *memPort) {
	defer p.close()
	buf := make([]byte, 64)
	var pending []byte
	for {
		n, err := p.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			pending = c.handleFrames(p, pending)
		}
		if err != nil {
			return // wire dropped (unplugged) or fault injected
		}
	}
}

// handleFrames consumes whole command frames off the head of pending, logs
// them, and answers status requests. Returns the remainder.
func (c *mockController) handleFrames(p *memPort, pending []byte) []byte {
	for len(pending) >= cmdLen {
		i := 0
		for i < len(pending) && pending[i] != startByte {
			i++
		}
		pending = pending[i:]
		if len(pending) < cmdLen {
			break
		}
		if pending[cmdLen-1] != endByte {
			pending = pending[1:] // resync like the driver's reader
			continue
		}
		frame := make([]byte, cmdLen)
		copy(frame, pending)
		pending = pending[cmdLen:]

		var reply []byte
		c.mu.Lock()
		c.frames = append(c.frames, frame)
		c.times = append(c.times, time.Now())
		switch frame[11] {
		case kSet:
			az := decodeCommandAz(frame)
			c.target = az
			c.az = az // instant slew: the mock parks at the commanded position
			// SET is documented silent (rot2proG spec) — no reply.
		case kStatus:
			reply = encodeStatusReply(c.az)
		case kStop:
			// Position stays wherever it is.
			reply = encodeAck() // spec: STOP answers (zeros in ROT1 mode)
		}
		c.mu.Unlock()

		if reply != nil {
			if _, err := p.Write(reply); err != nil {
				return pending // wire died answering; the serve loop exits on the next Read
			}
		}
	}
	return pending
}

func (c *mockController) connect(devEnd *memPort) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		devEnd.fail(io.ErrClosedPipe)
		return
	}
	c.conns++
	c.current = devEnd
	c.mu.Unlock()
	c.acceptCh <- devEnd
}

func (c *mockController) setAz(az float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.az = az
}

func (c *mockController) commandedAz() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.target
}

func (c *mockController) writes() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.frames))
	copy(out, c.frames)
	return out
}

func (c *mockController) writeTimes() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Time, len(c.times))
	copy(out, c.times)
	return out
}

func (c *mockController) connections() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conns
}

func (c *mockController) injectError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current != nil {
		c.current.fail(err)
	}
}

func (c *mockController) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.current != nil {
		c.current.close()
	}
	close(c.acceptCh)
}

// --- in-memory wire ------------------------------------------------------------------

// memWire is an in-memory full-duplex byte link: writes on one end appear on
// the other. Faults and closure are observed atomically by both ends, so a
// blocked reader wakes with the error instead of parking forever — the same
// semantics a USB-serial fd has when the adapter is unplugged.
type memWire struct {
	mu      sync.Mutex
	failErr error
	closed  bool
	changed chan struct{} // closed on every state change, then replaced
	aToB    chan []byte
	bToA    chan []byte
}

// memPort is one end of a memWire.
type memPort struct {
	w           *memWire
	in          <-chan []byte
	out         chan []byte
	readTimeout time.Duration
}

func newMemWire() (driverEnd, deviceEnd *memPort) {
	w := &memWire{
		changed: make(chan struct{}),
		aToB:    make(chan []byte, 64),
		bToA:    make(chan []byte, 64),
	}
	return &memPort{w: w, in: w.bToA, out: w.aToB},
		&memPort{w: w, in: w.aToB, out: w.bToA}
}

// SetReadTimeout gives the wire the bounded-read capability the driver owner
// configures on real serial ports (readTimeoutSetter): a read with no data
// once the timeout lapsed returns (0, nil), exactly like go.bug.st/serial.
func (p *memPort) SetReadTimeout(d time.Duration) error {
	p.readTimeout = d
	return nil
}

func (p *memPort) Read(b []byte) (int, error) {
	var deadline <-chan time.Time
	if p.readTimeout > 0 {
		timer := time.NewTimer(p.readTimeout)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		p.w.mu.Lock()
		if p.w.failErr != nil {
			err := p.w.failErr
			p.w.mu.Unlock()
			return 0, err
		}
		if p.w.closed {
			p.w.mu.Unlock()
			return 0, io.EOF
		}
		in, changed := p.in, p.w.changed
		p.w.mu.Unlock()
		select {
		case data := <-in:
			return copy(b, data), nil
		case <-changed:
		case <-deadline:
			return 0, nil
		}
	}
}

func (p *memPort) Write(b []byte) (int, error) {
	for {
		p.w.mu.Lock()
		if p.w.failErr != nil {
			err := p.w.failErr
			p.w.mu.Unlock()
			return 0, err
		}
		if p.w.closed {
			p.w.mu.Unlock()
			return 0, io.ErrClosedPipe
		}
		out, changed := p.out, p.w.changed
		p.w.mu.Unlock()
		data := make([]byte, len(b))
		copy(data, b)
		select {
		case out <- data:
			return len(b), nil
		case <-changed:
		}
	}
}

// Close marks the wire closed (graceful end) and wakes blocked ends. Both
// ends and the controller may close concurrently; it is idempotent.
func (p *memPort) Close() error {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if p.w.closed {
		return nil
	}
	p.w.closed = true
	p.w.wake()
	return nil
}

// fail faults the wire with err and wakes every blocked reader and writer —
// the injected "adapter unplugged".
func (p *memPort) fail(err error) {
	p.w.mu.Lock()
	defer p.w.mu.Unlock()
	if err == nil {
		err = io.ErrClosedPipe
	}
	p.w.failErr = err
	p.w.wake()
}

// close is Close without the result (internal callers).
func (p *memPort) close() { _ = p.Close() }

// wake closes the change channel (unblocking every parked Read/Write) and
// installs a fresh one. Caller holds w.mu.
func (w *memWire) wake() {
	close(w.changed)
	w.changed = make(chan struct{})
}
