package ercm

import (
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MockDevice is the scriptable in-memory ERC-M controller: it speaks the same
// GS-232B dialect the Driver does (C2/B/C readbacks, W goto, E/S stop, rFMW
// firmware) and satisfies the same contract as U2's SPID mock, so U4+ (mount
// dispatch, servers) and the bridge's mock mode run the whole stack bench- and
// CI-side without hardware (plan KTD7).
//
// Scriptability:
//   - SetAZ/SetEL/SetFirmware preset the simulated controller state.
//   - HoldReadback(true) suppresses computed readback replies (a silent line)
//     so tests can pin the "no readback yet" window; StageReadback then
//     answers the in-flight poll from the current position.
//   - Script stages raw reply lines that the next reads return verbatim
//     before any computed reply — this is how a test pins an A-mode
//     "+0aaa+0eee" or any odd vendor spelling.
//   - FailWrites/FailReads inject scripted link faults for self-heal tests.
//
// A W command moves both axes instantly (no slew model): position and target
// converge at once, which is what the driver's next C2 observes. Mock parity
// with U2's mock is contract parity, not slew realism — tests that need a
// moving axis extend this type then.
//
// The zero value is not usable; construct with NewMock.
type MockDevice struct {
	mu   sync.Mutex
	cond *sync.Cond

	az       float64
	el       float64
	firmware string

	replies []string // staged reply lines, FIFO (scripted + computed)
	writes  []string // every command line received, in order
	stopN   int      // E/S commands seen
	hold    bool     // suppress computed readback replies
	failW   int      // remaining scripted write errors
	failR   int      // remaining scripted read errors
	closed  bool
}

// NewMock returns a MockDevice in a plausible power-on state (az/el 0,
// firmware string set).
func NewMock() *MockDevice {
	m := &MockDevice{
		az:       0,
		el:       0,
		firmware: "ERC-M GS232B V2.1",
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// SetAZ presets the simulated azimuth.
func (m *MockDevice) SetAZ(deg float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.az = deg
}

// SetEL presets the simulated elevation.
func (m *MockDevice) SetEL(deg float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.el = deg
}

// SetFirmware sets the string the rFMW query returns.
func (m *MockDevice) SetFirmware(fw string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.firmware = fw
}

// AZ returns the simulated azimuth.
func (m *MockDevice) AZ() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.az
}

// EL returns the simulated elevation.
func (m *MockDevice) EL() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.el
}

// Writes returns a copy of every command line the mock has received, in order
// — the wire-level write log tests assert exact spellings against.
func (m *MockDevice) Writes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.writes))
	copy(out, m.writes)
	return out
}

// StopCount returns how many E/S stop commands the mock has received.
func (m *MockDevice) StopCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopN
}

// HoldReadback toggles suppression of computed readback replies: with hold
// set, C2/B/C writes are logged but no reply is staged — the line goes quiet.
func (m *MockDevice) HoldReadback(hold bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hold = hold
	m.cond.Broadcast()
}

// StageReadback stages one C2-shaped reply from the current position and
// wakes the reader — the test-side hand-crank for a line held by
// HoldReadback(true).
func (m *MockDevice) StageReadback() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stageLocked(fmt.Sprintf("AZ=%03d  EL=%03d", mockRoundDeg(m.az), mockRoundDeg(m.el)))
}

// Script stages raw reply lines; the next reads return them verbatim, before
// any computed reply.
func (m *MockDevice) Script(lines ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range lines {
		m.stageLocked(l)
	}
}

// FailWrites scripts n upcoming Write calls to fail (a dead port).
func (m *MockDevice) FailWrites(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failW = n
}

// FailReads scripts n upcoming Read calls to fail (a dropped adapter).
func (m *MockDevice) FailReads(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failR = n
}

// stageLocked appends one reply line. Must hold m.mu.
func (m *MockDevice) stageLocked(line string) {
	m.replies = append(m.replies, line)
	m.cond.Broadcast()
}

// Write handles one command batch: it logs every CR/LF-separated line,
// dispatches the GS-232B semantics, and stages the reply the controller would
// send (none for motion commands — W/E/S get no reply, per the protocol).
func (m *MockDevice) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failW > 0 {
		m.failW--
		return 0, errors.New("scripted write error")
	}
	for _, line := range splitLines(p) {
		m.writes = append(m.writes, line)
		m.dispatchLocked(line)
	}
	return len(p), nil
}

// reWCmd matches the W goto command — "Waaa eee" with the single-space
// separator the wire dialect pins. It runs on the outer-trimmed line, NOT
// on upperTrim output, because the separator space is load-bearing.
var reWCmd = regexp.MustCompile(`^W\s*(\d{1,3})\s+(\d{1,3})$`)

// dispatchLocked applies one received command line. Must hold m.mu.
func (m *MockDevice) dispatchLocked(line string) {
	u := upperTrim(line)
	switch {
	case u == "C2":
		if !m.hold {
			m.stageLocked(fmt.Sprintf("AZ=%03d  EL=%03d", mockRoundDeg(m.az), mockRoundDeg(m.el)))
		}
	case u == "C": // azimuth request: az-only reply
		if !m.hold {
			m.stageLocked(fmt.Sprintf("AZ=%03d", mockRoundDeg(m.az)))
		}
	case u == "B": // elevation request: el-only reply
		if !m.hold {
			m.stageLocked(fmt.Sprintf("EL=%03d", mockRoundDeg(m.el)))
		}
	case u == "E" || u == "S": // el stop / both stop — same class of command
		m.stopN++
	case u == "RFMW":
		m.stageLocked(m.firmware)
	case u == "RBAU":
		m.stageLocked("9600")
	case u == "RPRO":
		m.stageLocked("1") // GS-232B protocol mode
	default:
		// Waaa eee: az AND el in one command (single space separator —
		// parse before any space normalization, the separator is syntax).
		up := strings.ToUpper(strings.TrimSpace(line))
		if mm := reWCmd.FindStringSubmatch(up); mm != nil {
			a, _ := strconv.Atoi(mm[1])
			e, _ := strconv.Atoi(mm[2])
			m.az, m.el = float64(a), float64(e) // instant move (no slew model)
		}
		// Anything else: real controllers answer "?>" or nothing; the
		// driver must tolerate silence, so stage nothing.
	}
}

// Read returns staged replies FIFO. With nothing staged it blocks until a
// reply is staged or the device is closed. This is the device-level read a
// test drives directly; the driver talks through a Port() handle instead.
func (m *MockDevice) Read(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		if m.failR > 0 {
			m.failR--
			return 0, errors.New("scripted read error")
		}
		if len(m.replies) > 0 {
			line := m.replies[0]
			m.replies = m.replies[1:]
			n := copy(p, line+"\r")
			return n, nil
		}
		if m.closed {
			return 0, io.ErrClosedPipe
		}
		m.cond.Wait()
	}
}

// Close marks the whole device closed and wakes a blocked reader.
func (m *MockDevice) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.cond.Broadcast()
	return nil
}

// Port returns a fresh handle onto the mock device. Closing the HANDLE does
// not close the device: it models the driver closing a serial fd (self-heal,
// link Down) while the controller lives on — the opener hands out another
// handle at the reopen. Tests that want one persistent controller across a
// reopen cycle return Port() from the opener.
func (m *MockDevice) Port() io.ReadWriteCloser { return &mockPort{dev: m} }

// mockPort is one handle onto a MockDevice. Its Read polls the device's
// staged-reply queue and aborts when the handle closes, so a reader blocked
// on a silent line (HoldReadback) wakes with an error exactly like a reader
// on a closed fd.
type mockPort struct {
	dev    *MockDevice
	mu     sync.Mutex
	closed bool
}

// pollInterval bounds the reply-queue poll loop.
const mockPollInterval = 500 * time.Microsecond

func (p *mockPort) Read(b []byte) (int, error) {
	for {
		line, err := p.dev.tryPopReply()
		if err != nil {
			return 0, err
		}
		if line != "" {
			return copy(b, line+"\r"), nil
		}
		p.mu.Lock()
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return 0, io.ErrClosedPipe
		}
		time.Sleep(mockPollInterval)
	}
}

func (p *mockPort) Write(b []byte) (int, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return 0, io.ErrClosedPipe
	}
	return p.dev.Write(b)
}

func (p *mockPort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

// tryPopReply pops one staged reply without blocking. A non-nil error is a
// scripted fault (FailReads); an empty line means nothing is staged yet.
func (m *MockDevice) tryPopReply() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failR > 0 {
		m.failR--
		return "", errors.New("scripted read error")
	}
	if len(m.replies) > 0 {
		line := m.replies[0]
		m.replies = m.replies[1:]
		return line, nil
	}
	return "", nil
}

// mockRoundDeg formats a position the way the GS-232B integer fields carry it.
func mockRoundDeg(deg float64) int {
	return int(math.Round(deg))
}

// splitLines splits a write buffer into command lines on CR/LF, dropping
// empty fragments (a trailing LF after CRLF must not look like a command).
func splitLines(p []byte) []string {
	s := string(p)
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\r' || s[i] == '\n' {
			if frag := s[start:i]; frag != "" {
				out = append(out, frag)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func upperTrim(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' {
			continue
		}
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
