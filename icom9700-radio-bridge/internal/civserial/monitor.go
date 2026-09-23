package civserial

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"icom9700-radio-bridge/internal/civ"
)

// State is the latest observed telemetry: only values actually read or
// received, never extrapolated. Pointers stay nil until first observed.
type State struct {
	Responding bool
	FreqHz     uint64
	Band       string
	Mode       string
	Satellite  bool
	SMeter     *int
	SWR        *int
	ALC        *int
}

// equalState compares by VALUE (the meter pointers are per-read
// allocations — a structural compare would report a change on every read
// and defeat the on-change cadence).
func equalState(a, b State) bool {
	if a.Responding != b.Responding || a.FreqHz != b.FreqHz ||
		a.Band != b.Band || a.Mode != b.Mode || a.Satellite != b.Satellite {
		return false
	}
	return equalPtr(a.SMeter, b.SMeter) && equalPtr(a.SWR, b.SWR) &&
		equalPtr(a.ALC, b.ALC)
}

func equalPtr(a, b *int) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

// Options configure the monitor.
type Options struct {
	// Device is a stable /dev/serial/by-id/ path (ttyACM numbers move on
	// USB re-enumeration). Empty = SetMonitor/Wake reject with ErrNoDevice.
	Device string
	// Baud is the CI-V USB baud (the radio menu must match; 115200).
	Baud int
	// MeterInterval is the s-meter/SWR/ALC read cadence while on (0 = 500
	// ms). Values publish only on change.
	MeterInterval time.Duration
	// PowerFrame is the CI-V remote-wake frame (blind send — a standby
	// radio answers no ack). Empty = Wake rejected.
	PowerFrame []byte
	// Open is the port factory (SerialOpen in production). Required.
	Open OpenFunc
	// ReconnectSpacing is the delay between open attempts after a lost
	// port (0 = 3 s).
	ReconnectSpacing time.Duration
	// ReplyWait bounds one meter poll's reply wait (0 = 1 s).
	ReplyWait time.Duration
	Logger    *slog.Logger
}

// Monitor is the serial CI-V telemetry reader. All methods are safe from
// any goroutine.
type Monitor struct {
	opts Options
	log  *slog.Logger

	mu         sync.Mutex
	on         bool
	stopped    bool
	st         State
	port       Port   // set while a session is connected
	poll       *poll  // in-flight meter read
	answerSeen bool   // inbound CI-V evidence since the last tick
	deafStreak int
	deafWarnAt time.Time

	wmu     sync.Mutex // serializes port writes (reader vs Wake vs polls)
	updates chan struct{}
}

// New builds the monitor.
func New(opts Options) (*Monitor, error) {
	if opts.Open == nil {
		return nil, errors.New("civserial: no port open func")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MeterInterval <= 0 {
		opts.MeterInterval = 500 * time.Millisecond
	}
	if opts.ReconnectSpacing <= 0 {
		opts.ReconnectSpacing = 3 * time.Second
	}
	if opts.ReplyWait <= 0 {
		opts.ReplyWait = time.Second
	}
	return &Monitor{
		opts:    opts,
		log:     opts.Logger.With("component", "civserial"),
		updates: make(chan struct{}, 1),
	}, nil
}

// SetMonitor toggles the telemetry reader. Sticky — no TTL: the serial
// wire is dedicated to this process, there is nothing to release. Turning
// it on starts the connect/read/poll loop; turning it off stops it and
// closes the port. The last observed State survives (the /state assembly
// omits telemetry while the monitor is off anyway).
func (m *Monitor) SetMonitor(on bool) error {
	if on && m.opts.Device == "" {
		return ErrNoDevice
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return errors.New("civserial: monitor stopped")
	}
	if m.on == on {
		m.mu.Unlock()
		return nil
	}
	m.on = on
	if on {
		m.deafStreak = 0 // a stale standby signature must not survive a toggle
	}
	m.mu.Unlock()
	if on {
		go m.run()
	} else {
		m.nudge() // /state drops the telemetry fields immediately
	}
	return nil
}

// Wake sends the CI-V remote-wake frame. With the monitor on it rides the
// open port; off, the port is transiently opened, the frame goes out
// blind, and the port closes again after a short linger (a standby radio
// answers no ack — nothing to wait for).
func (m *Monitor) Wake(ctx context.Context) error {
	if len(m.opts.PowerFrame) == 0 {
		return errors.New("civserial: power_on not configured (serial.power_on_frame empty)")
	}
	m.mu.Lock()
	on := m.on
	m.mu.Unlock()
	if on {
		var port Port
		deadline := time.After(5 * time.Second)
		for {
			m.mu.Lock()
			port = m.port
			m.mu.Unlock()
			if port != nil {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline:
				return errors.New("civserial: serial port not open")
			case <-time.After(50 * time.Millisecond):
			}
		}
		m.wmu.Lock()
		defer m.wmu.Unlock()
		m.log.Info("sending remote power-on frame (serial)")
		_, err := port.Write(m.opts.PowerFrame)
		return err
	}
	// Monitor off: transient open, blind send, linger, close.
	port, err := m.opts.Open()
	if err != nil {
		return fmt.Errorf("civserial: open for wake: %w", err)
	}
	m.log.Info("sending remote power-on frame (serial, transient open)")
	_, werr := port.Write(m.opts.PowerFrame)
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}
	port.Close()
	return werr
}

// Snapshot returns the latest observed telemetry.
func (m *Monitor) Snapshot() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st
}

// Updates nudges on every telemetry change (coalesced).
func (m *Monitor) Updates() <-chan struct{} { return m.updates }

// Close stops the monitor. The reader goroutine observes on==false at its
// next loop boundary and exits; the port closes with the session.
func (m *Monitor) Close() {
	m.mu.Lock()
	m.stopped = true
	m.on = false
	m.mu.Unlock()
	m.nudge()
}

// nudge coalesces an update notification.
func (m *Monitor) nudge() {
	select {
	case m.updates <- struct{}{}:
	default:
	}
}

// setState folds one observed value and nudges on change.
func (m *Monitor) setState(f func(*State)) {
	m.mu.Lock()
	prev := m.st
	f(&m.st)
	st := m.st
	m.mu.Unlock()
	if !equalState(st, prev) {
		m.nudge()
	}
}

// run is the monitor loop: while on, connect (with backoff), run one
// session, repeat. Exits when turned off or Close.
func (m *Monitor) run() {
	for {
		m.mu.Lock()
		on, stopped, port := m.on, m.stopped, m.port
		m.mu.Unlock()
		if stopped || !on {
			if port != nil {
				m.mu.Lock()
				m.port = nil
				m.mu.Unlock()
				port.Close()
			}
			return
		}

		port, err := m.opts.Open()
		if err != nil {
			m.log.Warn("serial open failed", "device", m.opts.Device, "err", err)
			m.markDeaf()
			m.sleep(m.opts.ReconnectSpacing)
			continue
		}
		m.mu.Lock()
		m.port = port
		m.mu.Unlock()
		m.log.Info("serial port open", "device", m.opts.Device)

		m.session(port)

		m.mu.Lock()
		m.port = nil
		m.mu.Unlock()
		port.Close()
		m.markDeaf()
		m.sleep(m.opts.ReconnectSpacing)
	}
}

// sleep waits d, returning early once the monitor is off or stopped.
func (m *Monitor) sleep(d time.Duration) {
	deadline := time.After(d)
	for {
		m.mu.Lock()
		on, stopped := m.on, m.stopped
		m.mu.Unlock()
		if stopped || !on {
			return
		}
		select {
		case <-deadline:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// session runs one connected period: enable transceive, read the initial
// state, start the read pump, then poll meters on the tick. Returns when
// the port errors or the monitor is turned off.
func (m *Monitor) session(port Port) {
	// Belt-and-braces: the radio menu's CI-V Transceive=ON is the
	// documented prerequisite; this re-asserts it each connect.
	if err := m.write(port, civ.CmdSetTransceive(true)); err != nil {
		m.log.Warn("transceive enable failed", "err", err)
		return
	}
	// Initial reads so the first /state after monitor_on is complete
	// without waiting for the operator to touch the dial.
	for _, frame := range [][]byte{civ.CmdReadFreq(), civ.CmdReadMode(), civ.CmdReadSatelliteMode()} {
		if err := m.write(port, frame); err != nil {
			m.log.Warn("initial read failed", "err", err)
			return
		}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.readPump(port, stop)
	}()
	defer close(stop)

	meters := []struct {
		frame []byte
		apply func(*State, int)
	}{
		{civ.CmdReadSMeter(), func(st *State, v int) { st.SMeter = &v }},
		{civ.CmdReadSWR(), func(st *State, v int) { st.SWR = &v }},
		{civ.CmdReadALC(), func(st *State, v int) { st.ALC = &v }},
	}

	meterIdx := 0
	var inflight *poll
	tick := time.NewTicker(m.opts.MeterInterval)
	defer tick.Stop()

	for {
		// Off switch: observed at every cycle boundary.
		m.mu.Lock()
		on, stopped := m.on, m.stopped
		m.mu.Unlock()
		if stopped || !on {
			return
		}

		select {
		case <-done:
			return // port errored; run() reconnects
		case <-tick.C:
		}

		// Settle the previous cycle's poll.
		if inflight != nil {
			select {
			case <-inflight.done:
				if inflight.err == nil && inflight.reply != nil {
					if v, err := civ.ParseMeter(inflight.reply); err == nil {
						apply := meters[(meterIdx-1+len(meters))%len(meters)].apply
						m.setState(func(st *State) { apply(st, v) })
					}
				}
				inflight = nil
			case <-time.After(m.opts.ReplyWait):
				// Unanswered: the deaf counter already counted it.
				inflight = nil
			case <-done:
				return
			}
		} else if !m.takeAnswered() {
			// No poll in flight and no inbound answer this tick.
			m.markDeaf()
		}

		// One meter poll per tick (round-robin).
		mt := meters[meterIdx%len(meters)]
		meterIdx++
		p := &poll{frame: mt.frame, done: make(chan struct{})}
		m.mu.Lock()
		m.poll = p
		m.mu.Unlock()
		inflight = p
		if err := m.write(port, mt.frame); err != nil {
			m.log.Warn("poll write failed", "err", err)
			return // port is gone; run() reconnects
		}
	}
}

// readPump reads the port until stop or error, feeding complete frames to
// handle.
func (m *Monitor) readPump(port Port, stop <-chan struct{}) {
	buf := make([]byte, 0, 512)
	var tmp [256]byte
	for {
		n, err := port.Read(tmp[:])
		if err != nil {
			return // port gone; run() reconnects
		}
		if n == 0 {
			// A 0-read with no error would spin — wait briefly.
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		buf = append(buf, tmp[:n]...)
		if len(buf) > 4096 {
			buf = buf[len(buf)-512:] // garbage guard: keep a tail only
		}
		for {
			raw, ok := nextFrame(&buf)
			if !ok {
				break
			}
			m.handle(raw)
		}
		select {
		case <-stop:
			return
		default:
		}
	}
}

// handle dispatches one complete CI-V frame: broadcasts fold into the
// state, direct replies resolve the in-flight poll.
func (m *Monitor) handle(raw []byte) {
	f, err := civ.ParseFrame(raw)
	if err != nil {
		m.log.Debug("dropping unparseable frame", "err", err)
		return
	}
	if !f.Direct {
		// Unsolicited broadcast: the radio's own front-panel change — the
		// on-change feed.
		tr, ok, err := civ.ParseTransceive(f)
		if err != nil || !ok {
			return
		}
		m.sawAnswer()
		switch tr.Kind {
		case "freq":
			band, _ := civ.BandForFreq(tr.FreqHz)
			m.setState(func(st *State) { st.FreqHz = tr.FreqHz; st.Band = band })
		case "mode":
			m.setState(func(st *State) { st.Mode = tr.Mode })
		case "tx":
			// TX broadcasts are consumed, not published — the console has
			// no TX surface (receive-only posture).
		}
		return
	}
	// A direct reply: does it answer the in-flight poll?
	m.mu.Lock()
	p := m.poll
	resolved := false
	if p != nil {
		isAck := f.Cmd == civ.TerminatorOK || f.Cmd == civ.TerminatorNG
		wantSub := p.frame[5 : len(p.frame)-1]
		matches := f.Cmd == p.frame[4] &&
			(len(wantSub) == 0 || bytes.HasPrefix(f.Sub, wantSub[:1]))
		if matches || isAck {
			if f.IsNG() {
				p.err = f.AsNG()
			} else {
				// Real firmware repeats the sub bytes in replies to
				// sub-command queries — strip that echo before parsing
				// (bench 2026-09-20).
				sub := f.Sub
				if len(sub) > 1 {
					sub = sub[1:]
				}
				p.reply = sub
			}
			close(p.done)
			m.poll = nil
			resolved = true
		}
	}
	m.mu.Unlock()
	if resolved {
		m.sawAnswer()
	}
}

// write sends one frame under the write mutex.
func (m *Monitor) write(port Port, frame []byte) error {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	_, err := port.Write(frame)
	return err
}

// sawAnswer records inbound CI-V evidence (broadcast or answered poll).
func (m *Monitor) sawAnswer() {
	m.mu.Lock()
	m.answerSeen = true
	m.mu.Unlock()
	m.clearDeaf()
}

// takeAnswered consumes the answer flag (one tick's worth).
func (m *Monitor) takeAnswered() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.answerSeen
	m.answerSeen = false
	return v
}

// clearDeaf resets the standby hysteresis (an answer arrived).
func (m *Monitor) clearDeaf() {
	m.mu.Lock()
	changed := !m.st.Responding
	m.deafStreak = 0
	m.st.Responding = true
	m.mu.Unlock()
	if changed {
		m.nudge()
	}
}

// markDeaf counts one unanswered poll cycle; deafPollsToClear consecutive
// ones clear radio_responding (hysteresis — a single CI-V collision must
// not flash the standby signature).
func (m *Monitor) markDeaf() {
	m.mu.Lock()
	m.deafStreak++
	clear := m.deafStreak >= deafPollsToClear && m.st.Responding
	warn := m.deafStreak >= deafPollsToClear && time.Since(m.deafWarnAt) >= time.Minute
	if clear {
		m.st.Responding = false
	}
	if warn {
		m.deafWarnAt = time.Now()
	}
	m.mu.Unlock()
	if clear {
		m.log.Warn("serial ci-v polls unanswered (radio in standby?)")
		m.nudge()
	}
}

// deafPollsToClear is the standby hysteresis in unanswered poll cycles.
const deafPollsToClear = 2

// poll is one in-flight meter read.
type poll struct {
	frame []byte
	reply []byte
	err   error
	done  chan struct{}
}

// nextFrame extracts the next complete CI-V frame from buf, resyncing past
// garbage (scan to the next 0xFD terminator, validate, drop one byte on a
// parse failure). Returns ok=false when no complete frame is available.
func nextFrame(buf *[]byte) ([]byte, bool) {
	b := *buf
	for {
		fd := bytes.IndexByte(b, 0xFD)
		if fd < 0 {
			// No terminator: keep a bounded partial-frame tail.
			if len(b) > 64 {
				b = b[len(b)-32:]
				*buf = b
			}
			return nil, false
		}
		candidate := b[:fd+1]
		if _, err := civ.ParseFrame(candidate); err != nil {
			b = b[1:] // garbage: resync one byte
			continue
		}
		*buf = b[fd+1:]
		return candidate, true
	}
}
