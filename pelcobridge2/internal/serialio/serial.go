package serialio

import (
	"fmt"
	"time"

	"go.bug.st/serial"
)

// SerialPort wraps the real serial link. 8N1; baud is configurable because the
// 303Z/3050DZ family is documented for 1200–9600 baud (the head defaults are
// unit-specific and third-party controllers of the same family default 9600).
type SerialPort struct {
	port serial.Port
	name string
	baud int
}

// ListPorts enumerates host serial ports (works on windows/darwin/linux via
// go.bug.st/serial's enumerator).
func ListPorts() ([]string, error) { return serial.GetPortsList() }

func mode(baud int) *serial.Mode {
	return &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
}

// OpenPort opens name at baud, 8N1.
func OpenPort(name string, baud int) (*SerialPort, error) {
	p, err := serial.Open(name, mode(baud))
	if err != nil {
		return nil, err
	}
	return &SerialPort{port: p, name: name, baud: baud}, nil
}

func (s *SerialPort) Name() string { return s.name }
func (s *SerialPort) Baud() int    { return s.baud }

// Read reads available bytes. A timeout (when configured) returns (0, nil).
func (s *SerialPort) Read(p []byte) (int, error) {
	if s.port == nil {
		return 0, fmt.Errorf("port is closed")
	}
	return s.port.Read(p)
}

// Reopen closes and reopens the port under the same name. A USB-serial adapter
// that drops and re-enumerates leaves a stale descriptor whose reads fail
// forever; without this the RX side stayed dead for the rest of the session
// (bench failure recorded in ptest).
func (s *SerialPort) Reopen() error {
	if s.port != nil {
		_ = s.port.Close()
	}
	p, err := serial.Open(s.name, mode(s.baud))
	if err != nil {
		s.port = nil
		return err
	}
	s.port = p
	return nil
}

// Write sends one frame. Nothing else ever writes. A short write is an error:
// a partially-transmitted frame silently logged as sent is a bench failure
// recorded in ptest.
func (s *SerialPort) Write(b []byte) error {
	if s.port == nil {
		return fmt.Errorf("port is closed")
	}
	n, err := s.port.Write(b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return fmt.Errorf("short write: %d of %d bytes", n, len(b))
	}
	return nil
}

// Close closes the port.
func (s *SerialPort) Close() error {
	if s.port == nil {
		return nil
	}
	return s.port.Close()
}

// ReadErr carries which reader generation failed, so an error from a reader
// that has already been replaced does not tear down the live one.
type ReadErr struct {
	Gen int
	Err error
}

// ReadLoop blocks reading the port and pushes every chunk on ch; on error it
// reports once on errCh and exits. Purely event-driven — no timers, no
// polling, nothing is ever sent from here. The generation counter lets the
// caller ignore a stale loop's error after a Reopen started a fresh one.
func (s *SerialPort) ReadLoop(gen int, ch chan<- []byte, errCh chan<- ReadErr) {
	buf := make([]byte, 256)
	for {
		p := s.port
		if p == nil {
			errCh <- ReadErr{Gen: gen, Err: fmt.Errorf("port is closed")}
			return
		}
		n, err := p.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			ch <- chunk
		}
		if err != nil {
			errCh <- ReadErr{Gen: gen, Err: err}
			return
		}
	}
}

// idleGap is ~1.5 frame times at the given baud: long enough that a frame
// split across reads is never mistaken for a truncated one, short enough that
// a truncated reply is flushed well before the next reply arrives. Exported
// because the engine and the TUI idle-flush both derive their gap from it.
func IdleGap(baud int) time.Duration {
	if baud <= 0 {
		baud = 2400
	}
	d := time.Duration(float64(7*10) / float64(baud) * 1.5 * float64(time.Second))
	if d < 20*time.Millisecond {
		d = 20 * time.Millisecond
	}
	return d
}
