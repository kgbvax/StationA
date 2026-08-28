// Package serialio connects to the PTZ — over a directly attached serial port
// or a TCP serial bridge — and bridges raw bytes to/from decoded Pelco-D
// frames. The transport is any io.ReadWriteCloser, so the framing reader is
// identical for serial and TCP.
package serialio

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"go.bug.st/serial"

	"pelcots/internal/pelco"
)

// errClosed is returned by Send after the port has been closed.
var errClosed = errors.New("serialio: port closed")

// writeTimeout bounds a single frame write. The actor goroutine is the only
// writer and writes synchronously, so without a deadline a wedged TCP bridge
// (full send buffer) would block polling, command execution, and the whole
// engine indefinitely. Only applied to transports that support deadlines
// (net.Conn); direct serial writes are left as-is.
const writeTimeout = 2 * time.Second

// dialTimeout bounds one connection attempt to a TCP serial bridge. The engine
// dials from its single actor goroutine, so an unbounded net.Dial to a host that
// silently drops SYNs — bridge powered off, firewall, wrong subnet — parked the
// whole engine for the OS connect timeout (75 s or more on macOS/Linux). Nothing
// polled, no command executed, no all-stop could be sent to a unit that was
// still slewing, and Close waited it out. Failing fast keeps the reconnect loop
// responsive instead.
const dialTimeout = 3 * time.Second

// Event is something observed on the serial link, delivered on the Frames
// channel. Exactly one of Raw, a decoded Frame, or Err is meaningful:
//   - Raw set: the exact bytes just read, before any framing (for diagnostics).
//   - Err set: a read or frame-parse error.
//   - otherwise: a successfully decoded frame (Pelco-D or Pelco-P — see
//     pelco.Frame.Proto for which envelope it arrived in).
type Event struct {
	Raw   []byte
	Frame pelco.Frame
	Err   error
}

// Port is an open connection to the PTZ (serial or TCP) with a background
// reader that emits decoded Pelco-D frames.
type Port struct {
	rwc    io.ReadWriteCloser
	frames chan Event

	mu     sync.Mutex
	closed bool
}

// newPort wraps an open transport and starts the framing reader goroutine.
func newPort(rwc io.ReadWriteCloser) *Port {
	port := &Port{rwc: rwc, frames: make(chan Event, 32)}
	go port.read()
	return port
}

// Open opens the named serial device at the given baud rate, 8N1, and starts
// the framing reader goroutine.
func Open(name string, baud int) (*Port, error) {
	mode := &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	sp, err := serial.Open(name, mode)
	if err != nil {
		return nil, err
	}
	return newPort(sp), nil
}

// Dial connects to a serial-to-TCP bridge (e.g. ser2net) at address
// "host:port" and starts the framing reader goroutine. The bridge owns the real
// serial parameters; raw Pelco-D bytes flow over the socket unchanged. The
// attempt is bounded by dialTimeout — see there for why that matters.
func Dial(address string) (*Port, error) {
	conn, err := net.DialTimeout("tcp", address, dialTimeout)
	if err != nil {
		return nil, err
	}
	return newPort(conn), nil
}

// Frames returns the channel of inbound decoded frames / read errors.
func (p *Port) Frames() <-chan Event { return p.frames }

// Send writes a single Pelco-D frame to the port. For deadline-capable
// transports (TCP bridges) the write is bounded by writeTimeout so a stalled
// link surfaces as an error instead of blocking the engine.
func (p *Port) Send(f pelco.Frame) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errClosed
	}
	if c, ok := p.rwc.(net.Conn); ok {
		_ = c.SetWriteDeadline(time.Now().Add(writeTimeout))
	}
	_, err := p.rwc.Write(f.Bytes())
	return err
}

// Close stops the reader and closes the underlying port.
func (p *Port) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	return p.rwc.Close() // unblocks the reader, which then closes p.frames
}

// read continuously frames the inbound byte stream and publishes each decoded
// frame on p.frames. It exits when the port is closed.
func (p *Port) read() {
	defer close(p.frames)

	var buf []byte
	chunk := make([]byte, 64)
	for {
		n, err := p.rwc.Read(chunk)
		if n > 0 {
			raw := make([]byte, n)
			copy(raw, chunk[:n])
			p.emit(Event{Raw: raw})
			buf = p.extract(append(buf, chunk[:n]...))
		}
		if err != nil {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if !closed {
				p.emit(Event{Err: err})
			}
			return
		}
	}
}

// extract pulls every valid frame out of buf and returns the unconsumed tail.
// It accepts both wire protocols (adaptive, like the unit itself): 0xA0 begins
// an 8-byte Pelco-P frame, 0xFF a 7-byte Pelco-D frame; which one we SEND is
// the engine's configured protocol, but the read side answers either. It
// self-heals from stray/extra bytes: leading non-start bytes are discarded,
// and a window that fails its checksum (a false start — 0xA0/0xAF can appear
// inside D frames as data, 0xFF inside P frames) drops a single byte and
// re-hunts rather than throwing away a frame that is merely misaligned by one
// byte. A candidate shorter than its protocol's frame length is kept in the
// tail until more bytes arrive.
func (p *Port) extract(buf []byte) []byte {
	for {
		i := 0
		for i < len(buf) && buf[i] != pelco.Sync && buf[i] != pelco.STX {
			i++
		}
		buf = buf[i:]
		f, need, err := pelco.ParseAny(buf)
		if err == pelco.ErrLength {
			return buf // wait for the rest of the frame
		}
		if err != nil {
			buf = buf[1:] // false start — resync on the next 0xFF/0xA0
			continue
		}
		p.emit(Event{Frame: f})
		buf = buf[need:]
	}
}

func (p *Port) emit(e Event) {
	select {
	case p.frames <- e:
	default: // drop if the engine actor is not draining fast enough (e.g. blocked in Send)
	}
}
