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

// Event is something observed on the serial link, delivered on the Frames
// channel. Exactly one of Raw, a decoded Frame, or Err is meaningful:
//   - Raw set: the exact bytes just read, before any framing (for diagnostics).
//   - Err set: a read or frame-parse error.
//   - otherwise: a successfully decoded 7-byte Pelco-D frame.
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
// serial parameters; raw Pelco-D bytes flow over the socket unchanged.
func Dial(address string) (*Port, error) {
	conn, err := net.Dial("tcp", address)
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

// extract pulls every valid 7-byte frame out of buf and returns the unconsumed
// tail. It self-heals from stray/extra bytes: leading non-sync bytes are
// discarded, and a 7-byte window that fails its checksum (a false sync, e.g. a
// duplicated 0xFF) drops a single byte and re-hunts for the next 0xFF rather
// than throwing away a frame that is merely misaligned by one byte.
func (p *Port) extract(buf []byte) []byte {
	for {
		i := 0
		for i < len(buf) && buf[i] != pelco.Sync {
			i++
		}
		buf = buf[i:]
		if len(buf) < pelco.FrameLen {
			return buf
		}
		f, err := pelco.Parse(buf[:pelco.FrameLen])
		if err != nil {
			buf = buf[1:] // false sync — resync on the next 0xFF
			continue
		}
		p.emit(Event{Frame: f})
		buf = buf[pelco.FrameLen:]
	}
}

func (p *Port) emit(e Event) {
	select {
	case p.frames <- e:
	default: // drop if the engine actor is not draining fast enough (e.g. blocked in Send)
	}
}
