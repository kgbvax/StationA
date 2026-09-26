package civserial

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"icom9700-radio-bridge/internal/civ"
)

// fakePort is an in-process serial port: writes are recorded, the test
// feeds inbound bytes via feed().
type fakePort struct {
	mu      sync.Mutex
	written [][]byte
	closed  bool
	inbound chan []byte

	// failAfter: when > 0, Read fails after that many reads total (the
	// port "vanishes" — the radio power-cycled).
	failAfter int
	reads     int
}

func newFakePort() *fakePort {
	return &fakePort{inbound: make(chan []byte, 64)}
}

func (p *fakePort) Read(b []byte) (int, error) {
	p.mu.Lock()
	p.reads++
	fail := p.failAfter > 0 && p.reads > p.failAfter
	p.mu.Unlock()
	if fail {
		return 0, errorsNew("port vanished")
	}
	select {
	case data := <-p.inbound:
		n := copy(b, data)
		return n, nil
	case <-time.After(2 * time.Millisecond):
		return 0, nil
	}
}

func (p *fakePort) Write(d []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.written = append(p.written, append([]byte(nil), d...))
	return len(d), nil
}

func (p *fakePort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

// feed injects one radio -> controller frame into the read path.
func (p *fakePort) feed(frame []byte) {
	p.inbound <- append([]byte(nil), frame...)
}

func (p *fakePort) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *fakePort) wroteFrames() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.written))
	copy(out, p.written)
	return out
}

func errorsNew(s string) error { return &staticErr{s} }

type staticErr struct{ s string }

func (e *staticErr) Error() string { return e.s }

// harness drives a Monitor over a manufactured port factory.
type sharness struct {
	t      *testing.T
	m      *Monitor
	ports  []*fakePort
	openMu sync.Mutex
	openN  int
	// failReadsAfter applies to every port (0 = never).
	failReadsAfter int
}

func newSH(t *testing.T, failReadsAfter int) *sharness {
	h := &sharness{t: t, failReadsAfter: failReadsAfter}
	m, err := New(Options{
		Device:        "/dev/serial/by-id/test",
		Baud:          115200,
		MeterInterval: 20 * time.Millisecond,
		ReplyWait:     120 * time.Millisecond,
		ReconnectSpacing: 20 * time.Millisecond,
		PowerFrame:    []byte{0x1a, 0x05, 0x02, 0x01},
		Open: func() (Port, error) {
			h.openMu.Lock()
			h.openN++
			p := newFakePort()
			p.failAfter = h.failReadsAfter
			h.ports = append(h.ports, p)
			h.openMu.Unlock()
			return p, nil
		},
		Logger: slog.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.m = m
	t.Cleanup(m.Close)
	return h
}

func (h *sharness) openCount() int {
	h.openMu.Lock()
	defer h.openMu.Unlock()
	return h.openN
}

func (h *sharness) lastPort() *fakePort {
	h.openMu.Lock()
	defer h.openMu.Unlock()
	if len(h.ports) == 0 {
		return nil
	}
	return h.ports[len(h.ports)-1]
}

func waitSH(t *testing.T, what string, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s not observed within %s", what, d)
}

// monitorOn toggles the monitor and waits for the port to come up.
func (h *sharness) monitorOn() *fakePort {
	if err := h.m.SetMonitor(true); err != nil {
		h.t.Fatalf("SetMonitor(true): %v", err)
	}
	waitSH(h.t, "serial port open", 2*time.Second, func() bool {
		p := h.lastPort()
		return p != nil && len(p.wroteFrames()) > 0
	})
	return h.lastPort()
}

// radioFrame assembles an inbound TRANSCEIVE BROADCAST (dest 00 — the
// radio addresses all controllers; ParseFrame must NOT read it as a
// direct reply).
func radioFrame(cmd byte, sub []byte) []byte {
	p := append([]byte{0xFE, 0xFE, 0x00, 0xA2, cmd}, sub...)
	return append(p, 0xFD)
}

// radioReply assembles an inbound DIRECT REPLY to one of our polls
// (addressed E0 A2; the FB ack rides BEFORE the FD end marker).
func radioReply(cmd byte, sub []byte) []byte {
	p := append([]byte{0xFE, 0xFE, 0xE0, 0xA2, cmd}, sub...)
	return append(p, 0xFB, 0xFD)
}

func hasFrame(written [][]byte, want []byte) bool {
	for _, w := range written {
		if string(w) == string(want) {
			return true
		}
	}
	return false
}

// monitor_on opens the port and immediately sends the transceive enable +
// the initial freq/mode/satellite reads — the first /state after monitor_on
// is complete without waiting for the operator to touch the dial.
func TestMonitorOnSendsInitialReads(t *testing.T) {
	h := newSH(t, 0)
	p := h.monitorOn()

	waitSH(t, "transceive enable sent", time.Second, func() bool {
		return hasFrame(p.wroteFrames(), civ.CmdSetTransceive(true))
	})
	for _, want := range [][]byte{civ.CmdReadFreq(), civ.CmdReadMode(), civ.CmdReadSatelliteMode()} {
		if !hasFrame(p.wroteFrames(), want) {
			t.Errorf("initial read % X missing (written %d frames)", want, len(p.wroteFrames()))
		}
	}
}

// A freq transceive broadcast lands in the state (the on-change feed).
func TestFreqBroadcastUpdatesState(t *testing.T) {
	h := newSH(t, 0)
	p := h.monitorOn()

	hz, _ := civ.BCD10Encode(145800000)
	p.feed(radioFrame(0x00, hz))
	waitSH(t, "freq broadcast folded", time.Second, func() bool {
		st := h.m.Snapshot()
		return st.FreqHz == 145800000 && st.Band == "2m"
	})
}

// Mode broadcast lands likewise.
func TestModeBroadcastUpdatesState(t *testing.T) {
	h := newSH(t, 0)
	p := h.monitorOn()

	p.feed(radioFrame(0x01, []byte{0x05, 0x01})) // FM, filter 1
	waitSH(t, "mode broadcast folded", time.Second, func() bool {
		return h.m.Snapshot().Mode == "fm"
	})
}

// The meter poll gets its echo-sub reply parsed into the state, and an
// unchanged re-read does not re-nudge (the on-change cadence).
func TestMeterPollReply(t *testing.T) {
	h := newSH(t, 0)
	p := h.monitorOn()

	// Reply to the first S-meter poll (15 02): BCD "0120" = 120 (S9). Real
	// firmware repeats the sub bytes — the reply carries them (bench).
	waitSH(t, "s-meter poll sent", time.Second, func() bool {
		return hasFrame(p.wroteFrames(), civ.CmdReadSMeter())
	})
	p.feed(radioReply(0x15, []byte{0x02, 0x01, 0x20}))
	waitSH(t, "s-meter folded", time.Second, func() bool {
		st := h.m.Snapshot()
		return st.SMeter != nil && *st.SMeter == 120
	})

	// Same value again: no update nudge (dedupe).
	drain(h.m.Updates())
	p.feed(radioReply(0x15, []byte{0x02, 0x01, 0x20}))
	time.Sleep(120 * time.Millisecond)
	select {
	case <-h.m.Updates():
		t.Error("unchanged meter re-read nudged an update")
	default:
	}
}

// A port that vanishes (radio power-cycle) reconnects and replays the
// initial reads on the fresh port.
func TestReconnectAfterPortError(t *testing.T) {
	h := newSH(t, 3) // every port dies after 3 reads
	p1 := h.monitorOn()
	h.m.SetMonitor(false) // toggle off/on is not required — run() reconnects alone
	_ = p1
	h.m.SetMonitor(true)

	waitSH(t, "port re-opened after the error", 3*time.Second, func() bool {
		return h.openCount() >= 2
	})
	p2 := h.lastPort()
	waitSH(t, "initial reads replayed on the fresh port", 2*time.Second, func() bool {
		return hasFrame(p2.wroteFrames(), civ.CmdSetTransceive(true))
	})
}

// Wake with the monitor off transient-opens a port, sends the power-on
// frame blind, and closes it again.
func TestWakeTransientOpen(t *testing.T) {
	h := newSH(t, 0)

	if err := h.m.Wake(testCtx()); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	p := h.lastPort()
	if p == nil {
		t.Fatal("Wake never opened a port")
	}
	if !hasFrame(p.wroteFrames(), []byte{0x1a, 0x05, 0x02, 0x01}) {
		t.Error("power-on frame not sent")
	}
	waitSH(t, "transient port closed", time.Second, func() bool { return p.isClosed() })
}

// SetMonitor without a configured device is the observed fact, not a
// guess.
func TestSetMonitorNoDevice(t *testing.T) {
	m, err := New(Options{Open: func() (Port, error) { return &fakePort{}, nil }, Logger: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.SetMonitor(true); err != ErrNoDevice {
		t.Fatalf("err = %v, want ErrNoDevice", err)
	}
}

// A deaf radio (polls unanswered) flips radio_responding off after the
// hysteresis; one answer resets it instantly.
func TestDeafHysteresis(t *testing.T) {
	h := newSH(t, 0)
	h.monitorOn()

	// No radio traffic at all: two unanswered cycles clear responding.
	waitSH(t, "responding cleared after deaf cycles", 2*time.Second, func() bool {
		return !h.m.Snapshot().Responding
	})

	// Any inbound answer restores it instantly.
	h.lastPort().feed(radioFrame(0x00, mustBCD(145800000)))
	waitSH(t, "responding restored on answer", time.Second, func() bool {
		return h.m.Snapshot().Responding
	})
}

func mustBCD(hz uint64) []byte {
	b, err := civ.BCD10Encode(hz)
	if err != nil {
		panic(err)
	}
	return b
}

func drain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func testCtx() context.Context { return context.Background() }
