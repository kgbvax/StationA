package serialio

import (
	"fmt"
	"sync"
	"time"

	"go.bug.st/serial"
)

// SerialPort wraps the real serial link. 8N1; baud is configurable because the
// 303Z/3050DZ family is documented for 1200–9600 baud (the head defaults are
// unit-specific and third-party controllers of the same family default 9600).
//
// The port field is mutex-guarded: Read runs on the engine's reader goroutine
// while Reopen runs on the engine loop (ctrl+r / auto-heal) — an unsynchronized
// swap is a data race under -race and a use-after-close on real hardware.
type SerialPort struct {
	mu   sync.Mutex
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

// current snapshots the live port handle.
func (s *SerialPort) current() serial.Port {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// Read reads available bytes. A timeout (when configured) returns (0, nil).
// Never hold the mutex across the blocking read — a concurrent Reopen must be
// able to swap the port (Close unblocks this Read with an error).
func (s *SerialPort) Read(p []byte) (int, error) {
	port := s.current()
	if port == nil {
		return 0, fmt.Errorf("port is closed")
	}
	return port.Read(p)
}

// Reopen closes and reopens the port under the same name. A USB-serial adapter
// that drops and re-enumerates leaves a stale descriptor whose reads fail
// forever; without this the RX side stayed dead for the rest of the session
// (bench failure recorded in ptest). The engine restarts its reader after a
// successful reopen.
func (s *SerialPort) Reopen() error {
	s.mu.Lock()
	old := s.port
	s.port = nil
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	p, err := serial.Open(s.name, mode(s.baud))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.port = p
	s.mu.Unlock()
	return nil
}

// Write sends one frame. Nothing else ever writes. A short write is an error:
// a partially-transmitted frame silently logged as sent is a bench failure
// recorded in ptest.
func (s *SerialPort) Write(b []byte) error {
	port := s.current()
	if port == nil {
		return fmt.Errorf("port is closed")
	}
	n, err := port.Write(b)
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
	s.mu.Lock()
	port := s.port
	s.port = nil
	s.mu.Unlock()
	if port == nil {
		return nil
	}
	return port.Close()
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
