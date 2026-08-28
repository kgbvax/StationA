package main

import (
	"fmt"
	"time"

	"go.bug.st/serial"
)

// SerialPort wraps the serial link. 8N1; the baud rate is a flag because the
// 303Z/3050DZ family is documented for 1200–9600 baud (the PTS-3050DZ-Y product
// page) and third-party controllers of the same head default to 9600. ptest
// used to hardcode 2400 with no override, so a head running at another rate
// looked dead rather than mis-framed — and the assembler's own resync comment
// already blamed "a wrong-baud link" it gave no way to correct.
type SerialPort struct {
	port serial.Port
	name string
	baud int
}

func ListPorts() ([]string, error) {
	return serial.GetPortsList()
}

func mode(baud int) *serial.Mode {
	return &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
}

func OpenPort(name string, baud int) (*SerialPort, error) {
	p, err := serial.Open(name, mode(baud))
	if err != nil {
		return nil, err
	}
	return &SerialPort{port: p, name: name, baud: baud}, nil
}

func (s *SerialPort) Name() string { return s.name }
func (s *SerialPort) Baud() int    { return s.baud }

// SetReadTimeout bounds Read so a caller doing synchronous request/response
// (the sweep recorder) cannot block forever when the head does not answer. The
// TUI leaves it unset and reads blocking in its own goroutine.
func (s *SerialPort) SetReadTimeout(d time.Duration) error {
	if s.port == nil {
		return fmt.Errorf("port is closed")
	}
	return s.port.SetReadTimeout(d)
}

// Read reads available bytes, honouring any read timeout. A timeout returns
// (0, nil) rather than an error.
func (s *SerialPort) Read(p []byte) (int, error) {
	if s.port == nil {
		return 0, fmt.Errorf("port is closed")
	}
	return s.port.Read(p)
}

// Reopen closes and reopens the port under the same name. A USB-serial adapter
// that drops and re-enumerates leaves a stale descriptor whose reads fail
// forever; without this the RX side stayed dead for the rest of the session.
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

// Write sends one frame. Nothing else in the program ever writes. A short write
// is an error: it used to be discarded, so a partially-transmitted frame was
// logged as if the whole thing had gone out.
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

// ReadLoop blocks reading the port and pushes every chunk on ch; on error it
// reports once on errCh and exits. Purely event-driven — no timers, no
// polling, nothing is ever sent from here. The generation counter lets the UI
// ignore a stale loop's error after a Reopen has started a fresh one.
func (s *SerialPort) ReadLoop(gen int, ch chan<- []byte, errCh chan<- readErr) {
	buf := make([]byte, 256)
	for {
		p := s.port
		if p == nil {
			errCh <- readErr{gen: gen, err: fmt.Errorf("port is closed")}
			return
		}
		n, err := p.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			ch <- chunk
		}
		if err != nil {
			errCh <- readErr{gen: gen, err: err}
			return
		}
	}
}

// readErr carries which reader generation failed, so an error from a reader
// that has already been replaced does not tear down the live one.
type readErr struct {
	gen int
	err error
}

func (s *SerialPort) Close() error {
	if s.port == nil {
		return nil
	}
	return s.port.Close()
}
