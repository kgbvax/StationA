package serialio

import (
	"io"
	"math"
	"sync"

	"pelcots/internal/pelco"
)

// SimOptions configures an in-memory simulated Pelco-D rotator link.
type SimOptions struct {
	// Addr is the Pelco-D camera address the emulator answers for; it should
	// match the engine's addr (default 1). Zero accepts any address.
	Addr byte
	// StartPan is the initial azimuth in degrees (wrapped to 0–360).
	StartPan float64
	// StartTilt is the initial elevation in degrees (clamped to 0–90).
	StartTilt float64
	// JogStep is the degrees of travel applied per jog frame received. The
	// engine re-sends jog keepalive every poll (100–400 ms), so this sets the
	// simulated jog slew rate. Defaults to 5° per frame.
	JogStep float64
	// WildTiltWhileMoving, when >0, reproduces the 303Z/3050DZ readback failure
	// in the simulator: while the tilt motor is running the emulator answers
	// QueryTilt with tilt+this (a constant valid-checksum garbage stream, far
	// from the true position) instead of the true position; once the motor
	// halts, QueryTilt answers the true position. Idle readback is always
	// clean. 0 (default) = clean readback at all times. Degrees.
	WildTiltWhileMoving float64
}

// OpenSim returns a Port backed by an in-memory Pelco-D rotator emulator.
// Nothing is opened on the host: writes are decoded as Pelco-D commands and
// answered with position-response frames on the read side, so the engine's
// polling, arrival detection, and cable-wrap protection behave as they would
// against a real rotator. Used to exercise the inbound control server
// (rotctld) and the sat-tracking integration without any hardware attached.
//
// Motion model: an absolute SetPan/SetTilt snaps straight to the target (the
// unit is "instant"); a Jog moves the position by JogStep in the jog
// direction on each frame so the closed-loop unwrap path accumulates
// observed travel; Stop holds the current position. QueryPan/QueryTilt reply
// with the current position. There is never any I/O against a device.
func OpenSim(opts SimOptions) *Port {
	if opts.JogStep <= 0 {
		opts.JogStep = 5
	}
	s := &simLink{
		addr:                opts.Addr,
		pan:                 wrap360(opts.StartPan),
		tilt:                clampTilt(opts.StartTilt),
		jogStep:             opts.JogStep,
		wildTiltWhileMoving: opts.WildTiltWhileMoving,
		out:                 make(chan []byte, 64),
		done:                make(chan struct{}),
	}
	return newPort(s)
}

// simLink is an io.ReadWriteCloser that emulates a Pelco-D / Pelco-P PTZ in
// memory. The serialio framing reader (Port.read) consumes response bytes from
// out; the engine writes command frames via Write. Commands are accepted in
// either protocol (dispatched on the 0xFF / 0xA0 start byte) and each response
// is framed in the protocol the query arrived in, like the adaptive real head.
// All framing/checksumming is reused through newPort, so this is a drop-in
// transport just like Open/Dial.
type simLink struct {
	mu      sync.Mutex
	addr    byte
	pan     float64 // current azimuth, degrees, 0–<360
	tilt    float64 // current elevation, degrees, 0–90
	jogStep float64
	// wildTiltWhileMoving >0: while tiltMoving, QueryTilt answers tilt+this
	// (garbage) instead of the true position; reproduces the 303Z/3050DZ.
	wildTiltWhileMoving float64
	tiltMoving          bool   // tilt motor is running (last Jog had a tilt component)
	wbuf                []byte // inbound command accumulation (writes are whole frames)
	out                 chan []byte
	done                chan struct{}
	once                sync.Once
}

func (s *simLink) Read(p []byte) (int, error) {
	select {
	case b, ok := <-s.out:
		if !ok {
			return 0, io.EOF
		}
		return copy(p, b), nil
	case <-s.done:
		return 0, io.EOF
	}
}

func (s *simLink) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.wbuf = append(s.wbuf, p...)
	for {
		f, need, err := pelco.ParseAny(s.wbuf)
		if err == pelco.ErrLength {
			break // wait for the rest of the frame
		}
		if err != nil {
			s.wbuf = s.wbuf[1:] // false start — resync on the next 0xFF/0xA0
			continue
		}
		s.wbuf = s.wbuf[need:]
		s.handle(f)
	}
	s.mu.Unlock()
	return len(p), nil
}

func (s *simLink) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

// handle applies one decoded inbound command. Called under s.mu. Absolute
// moves snap; jogs step; queries enqueue a response frame for the reader.
func (s *simLink) handle(f pelco.Frame) {
	if s.addr != 0 && f.Addr != s.addr {
		return // not addressed to us
	}
	switch {
	case f.IsSetPan():
		s.pan = wrap360(pelco.HundredthsToDeg(f.Word()))
	case f.IsSetTilt():
		s.tilt = clampTilt(pelco.HundredthsToDeg(f.Word()))
		s.tiltMoving = false // absolute set snaps; the motor is not running afterward
	case f.IsSetPreset(), f.IsGoPreset(), f.IsClearPreset():
		// Presets (including the self-check disable, set preset 105) have no
		// position effect in the simulator; they are accepted and ignored so
		// the engine's on-connect disable-self-check frame is a harmless no-op.
	case f.IsQueryPan():
		r := pelco.PanResponse(s.addrOr(f.Addr), s.pan)
		r.Proto = f.Proto // answer in the protocol the query arrived in
		s.enqueue(r)
	case f.IsQueryTilt():
		t := s.tilt
		// 303Z/3050DZ failure mode: while the tilt motor runs the head answers
		// with a constant garbage stream (tilt+offset), never the true
		// position; once halted, readback is clean. tiltMoving is driven by the
		// motion frames below (a Jog with a tilt component runs the motor; a
		// Jog with no tilt component, a Stop, or a SetTilt halts it).
		if s.tiltMoving && s.wildTiltWhileMoving > 0 {
			t = s.tilt + s.wildTiltWhileMoving
		}
		r := pelco.TiltResponse(s.addrOr(f.Addr), t)
		r.Proto = f.Proto // answer in the protocol the query arrived in
		s.enqueue(r)
	case f.Cmd1 == 0 && f.Cmd2 == 0:
		// Stop (and a zero-direction jog, which is byte-identical: Cmd1=0
		// Cmd2=0): all motors halt. The tilt motor stops, so the next
		// QueryTilt answers the true position — this is how the open-loop
		// goto's halt (the tilt bit dropped) ends the garbage stream.
		s.tiltMoving = false
	case f.IsJog():
		pan, tilt := f.JogDir()
		s.pan = wrap360(s.pan + float64(pan)*s.jogStep)
		s.tilt = clampTilt(s.tilt + float64(tilt)*s.jogStep)
		s.tiltMoving = tilt != 0
	}
}

// addrOr returns the configured address, or the request's address when the
// emulator was configured to accept any (addr == 0), so responses carry a
// well-formed Pelco-D address byte in either case.
func (s *simLink) addrOr(req byte) byte {
	if s.addr != 0 {
		return s.addr
	}
	if req == 0 {
		return 1
	}
	return req
}

func (s *simLink) enqueue(f pelco.Frame) {
	select {
	case s.out <- f.Bytes():
	case <-s.done:
	default: // drop if the reader isn't draining (matches the real port's emit)
	}
}

func wrap360(deg float64) float64 {
	deg = math.Mod(deg, 360)
	if deg < 0 {
		deg += 360
	}
	return deg
}

func clampTilt(deg float64) float64 {
	if deg < 0 {
		return 0
	}
	if deg > 90 {
		return 90
	}
	return deg
}
