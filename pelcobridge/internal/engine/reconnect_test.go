package engine

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"pelcots/internal/config"
	"pelcots/internal/control"
	"pelcots/internal/pelco"
)

// waitFor polls cond up to timeout, returning whether it became true.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestAutoReconnect verifies the engine re-establishes a dropped link on its
// own: it connects to a TCP bridge, survives the peer closing the connection
// mid-session, and reconnects when the bridge accepts again — all without any
// explicit Reconnect call.
func TestAutoReconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var (
		mu       sync.Mutex
		accepted int
		conns    []net.Conn
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			mu.Lock()
			accepted++
			conns = append(conns, c)
			mu.Unlock()
			go func(c net.Conn) { // drain inbound polls until the conn drops
				buf := make([]byte, 64)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	eng := New(Options{
		Transport: config.TransportTCP,
		TCPAddr:   ln.Addr().String(),
		Addr:      1,
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool { return eng.Snapshot().Connected }) {
		t.Fatal("engine did not connect initially")
	}

	// Drop the first accepted connection to simulate a device/bridge failure.
	mu.Lock()
	first := conns[0]
	mu.Unlock()
	_ = first.Close()

	// The engine should notice the drop and enter the reconnecting state.
	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return !s.Connected || s.Reconnecting
	}) {
		t.Fatal("engine did not observe the dropped link")
	}

	// And then automatically re-establish the link (a second accept).
	if !waitFor(2*time.Second, func() bool {
		mu.Lock()
		n := accepted
		mu.Unlock()
		return n >= 2 && eng.Snapshot().Connected
	}) {
		t.Fatal("engine did not auto-reconnect after the drop")
	}
}

// TestReconnectWhileDeviceAbsent verifies the engine keeps retrying when the
// target is initially unreachable and connects once it appears, without a
// manual Reconnect.
func TestReconnectWhileDeviceAbsent(t *testing.T) {
	// Reserve a port, then close the listener so the address is (briefly) free.
	ln0, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln0.Addr().String()
	_ = ln0.Close()

	eng := New(Options{Transport: config.TransportTCP, TCPAddr: addr, Addr: 1})
	eng.Start()
	defer eng.Close()

	// Initial connect should fail and the engine should be retrying.
	if !waitFor(time.Second, func() bool { return eng.Snapshot().Reconnecting }) {
		t.Fatal("engine should be retrying while target is absent")
	}

	// Bring the target up; the retry loop should connect on its own.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("could not re-bind %s (race): %v", addr, err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 64)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	if !waitFor(2*time.Second, func() bool { return eng.Snapshot().Connected }) {
		t.Fatal("engine did not connect once the target appeared")
	}
}

// pelcoResponder is a minimal Pelco-D TCP "bridge" for the reconnect tests.
// It answers QueryPan/QueryTilt from a fixed position and classifies every
// received frame per connection (so the test can assert which frames landed on
// which connection). It deliberately does NOT advance position on a SetPan, so
// an in-progress goto never converges — the exact "mid-move" state a dropped
// link must catch.
type pelcoResponder struct {
	mu    sync.Mutex
	conns []*pelcoConnStats
	live  []net.Conn // accepted conn handles, in accept order (for dropping)
	addr  byte
	ln    net.Listener
}

type pelcoConnStats struct {
	mu                sync.Mutex
	stops, jogs, sets int // stops, jog keepalives, absolute SetPan/SetTilt
	misc              int
}

func (s *pelcoConnStats) record(f pelco.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case f.IsSetPan(), f.IsSetTilt():
		s.sets++
	case f.IsJog():
		s.jogs++
	case f.Cmd1 == 0 && f.Cmd2 == 0 && f.Data1 == 0 && f.Data2 == 0:
		s.stops++
	default:
		s.misc++
	}
}

func (s *pelcoConnStats) snapshot() (stops, jogs, sets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stops, s.jogs, s.sets
}

func (r *pelcoResponder) close() { _ = r.ln.Close() }

func (r *pelcoResponder) nth(n int) *pelcoConnStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n < 0 || n >= len(r.conns) {
		return nil
	}
	return r.conns[n]
}

func (r *pelcoResponder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

func newPelcoResponder(addr byte) *pelcoResponder {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	r := &pelcoResponder{addr: addr, ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			st := &pelcoConnStats{}
			r.mu.Lock()
			r.conns = append(r.conns, st)
			r.live = append(r.live, c)
			r.mu.Unlock()
			go r.serve(c, st)
		}
	}()
	return r
}

func (r *pelcoResponder) serve(c net.Conn, st *pelcoConnStats) {
	defer c.Close()
	buf := make([]byte, pelco.FrameLen)
	for {
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		f, err := pelco.Parse(buf)
		if err != nil {
			continue
		}
		st.record(f)
		// Answer position queries from a fixed 0°/0° position; never advance on
		// SetPan, so a goto stays in progress until the link is dropped.
		if f.IsQueryPan() {
			_, _ = c.Write(pelco.PanResponse(r.addr, 0).Bytes())
		} else if f.IsQueryTilt() {
			_, _ = c.Write(pelco.TiltResponse(r.addr, 0).Bytes())
		}
	}
}

// TestSetGotoDroppedLinkNotResumed is the regression test for the dropped-link
// motion bug: when the link drops mid-goto, linkFailed must clear the in-flight
// motion state (so the goto is not resumed) and connect must issue a Stop on
// the fresh link (so the unit, still physically slewing from before the drop,
// halts). Before the fix, mvActive stayed set after a drop and poll() kept
// driving the move on reconnect against a position computed pre-disconnect.
func TestSetGotoDroppedLinkNotResumed(t *testing.T) {
	r := newPelcoResponder(1)
	defer r.close()

	eng := New(Options{
		Transport:   config.TransportTCP,
		TCPAddr:     r.ln.Addr().String(),
		Addr:        1,
		WrapEnabled: true,
		WrapLimit:   270,
		WrapAccum:   260,  // cable wound near the +270 limit
		AutoArm:     true, // the move arrives over the network (Submit), so arm first
		LogLevel:    config.LogInfo,
	})
	eng.Start()
	defer eng.Close()

	if !waitFor(2*time.Second, func() bool { return eng.Snapshot().Connected }) {
		t.Fatal("engine did not connect")
	}
	// The goto needs both axes' readback before it can arm.
	if !waitFor(2*time.Second, func() bool {
		s := eng.Snapshot()
		return s.HavePan && s.HaveTilt
	}) {
		t.Fatal("engine never got pan/tilt readback")
	}

	// Goto −20°: shortest path (−20°) moves the wind from +260° to +240°, within
	// the ±270 limit, so the move is accepted and a SetPan goes out. The
	// responder never advances position, so the goto stays in progress.
	eng.Submit(control.Command{Kind: control.KindSetPos, Az: -20, El: 0})
	if !waitFor(2*time.Second, func() bool { return eng.Snapshot().Gotoing }) {
		t.Fatalf("engine never started the goto: status=%q", eng.Snapshot().Status)
	}
	first := r.nth(0)
	if first == nil {
		t.Fatal("no first connection recorded")
	}
	if _, _, sets := first.snapshot(); sets == 0 {
		t.Fatal("expected SetPan frames on the first connection while seeking")
	}

	// Drop the first connection mid-goto — the unit is physically still slewing.
	r.dropFirst(t)

	// State must be cleared: the goto is abandoned, not left to resume.
	if !waitFor(2*time.Second, func() bool { return !eng.Snapshot().Gotoing }) {
		s := eng.Snapshot()
		t.Fatalf("motion state not cleared after drop: gotoing=%v", s.Gotoing)
	}

	// The engine auto-reconnects; a second connection is accepted.
	if !waitFor(2*time.Second, func() bool { return r.count() >= 2 && eng.Snapshot().Connected }) {
		t.Fatalf("engine did not auto-reconnect (conns=%d)", r.count())
	}

	// Give the reconnect a moment to send its on-connect frames + a poll or two.
	time.Sleep(300 * time.Millisecond)

	second := r.nth(1)
	if second == nil {
		t.Fatal("no second connection recorded")
	}
	stops, jogs, sets := second.snapshot()

	if stops == 0 {
		t.Error("reconnect did not send Stop on the fresh link — unit may keep moving")
	}
	if sets != 0 {
		t.Errorf("reconnect resumed the goto (set frames on new link): sets=%d — stale goto not abandoned", sets)
	}
	if jogs != 0 {
		t.Errorf("unexpected jog frames on the new link: jogs=%d", jogs)
	}
}

// dropFirst closes the first accepted connection, forcing the engine down the
// link-failed path mid-goto.
func (r *pelcoResponder) dropFirst(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	c := r.live[0]
	r.mu.Unlock()
	if c == nil {
		t.Fatal("no first connection to drop")
	}
	_ = c.Close()
}
