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
	"pelcots/internal/serialio"
)

// TestGotoSendsAbsoluteSets asserts the goto drive model end to end: a network
// setpos produces absolute SetPan/SetTilt opcodes (0x4B/0x4D) on the wire and
// NO jog frames — the unit slews itself after the absolute sets; the engine
// only polls readback. (The device honors 0x4B/0x4D — confirmed on the bench
// 2026-08-28; the earlier closed-loop-jog goto was built on a misread. The
// responder answers from a fixed position, so the goto never converges and
// stays "seeking" until the safety timeout — long enough to watch the frames.)
func TestGotoSendsAbsoluteSets(t *testing.T) {
	r := newPelcoResponder(1)
	defer r.close()

	eng := New(Options{
		Transport:    config.TransportTCP,
		TCPAddr:      r.ln.Addr().String(),
		Addr:         1,
		PollInterval: 30 * time.Millisecond,
		Goto:         config.GotoConfig{TimeoutSec: 0.5},
		AutoArm:      true, // the move arrives over the network (Submit)
	})
	eng.Start()
	defer eng.Close()
	connectedWithReadback(t, eng)

	st := r.nth(0)
	if st == nil {
		t.Fatal("no connection recorded")
	}
	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 100, El: 20})
	if !waitFor(time.Second, func() bool { return eng.Snapshot().Gotoing }) {
		t.Fatalf("goto never armed: %+v", eng.Snapshot())
	}
	if !waitFor(time.Second, func() bool { _, _, sets := st.snapshot(); return sets >= 1 }) {
		t.Fatalf("no SetPan/SetTilt frames reached the unit: status=%q", eng.Snapshot().Status)
	}
	// Let the never-converging goto run past a few more poll ticks.
	time.Sleep(200 * time.Millisecond)
	_, jogs, sets := st.snapshot()
	if sets < 1 {
		t.Errorf("goto sent no absolute set frames")
	}
	if jogs != 0 {
		t.Errorf("goto drove the unit with jog frames (%d) — absolute sets are the only goto path", jogs)
	}
}

// TestGotoSetQuietWindowAndResend pins the wire discipline around absolute
// sets: (a) after a SetPan the next frame is the deferred SetTilt — no query
// intervenes (the line is held silent around absolute sets, which this head
// ignores when they land in the query stream); (b) when the unit never moves,
// the lost sets are re-sent (bounded), instead of the goto silently riding to
// its timeout.
func TestGotoSetQuietWindowAndResend(t *testing.T) {
	r := newSeqResponder(1)
	defer r.close()

	eng := New(Options{
		Transport:    config.TransportTCP,
		TCPAddr:      r.ln.Addr().String(),
		Addr:         1,
		PollInterval: 30 * time.Millisecond,            // quiet = 3 ticks, re-send after 6
		Goto:         config.GotoConfig{TimeoutSec: 0}, // default 60 s: the goto must not expire mid-test
		AutoArm:      true,
	})
	eng.Start()
	defer eng.Close()
	connectedWithReadback(t, eng)

	eng.Submit(control.Command{Kind: control.KindSetPos, Az: 100, El: 20})
	// 2 sets per send round (pan+tilt) + up to maxSetResends re-sends.
	if !waitFor(3*time.Second, func() bool { return r.count("set") >= 2+2*maxSetResends }) {
		t.Fatalf("sets were not re-sent after no travel: log=%v", r.kinds())
	}
	// No query may land between the first SetPan and the deferred SetTilt: the
	// quiet window must keep the line free while the head latches the set.
	log := r.frames()
	first := -1
	for i, f := range log {
		if f.kind == "set" {
			first = i
			break
		}
	}
	if first < 0 || first+1 >= len(log) {
		t.Fatalf("no set frame pair in the log: %v", log)
	}
	if k := log[first+1].kind; k != "set" {
		t.Errorf("frame after the first SetPan was %q, want the deferred SetTilt (quiet window)", k)
	}
	if r.count("query") == 0 {
		t.Errorf("polling never resumed after the quiet window")
	}
}

// TestTiltDecodeIsTextbook pins the tilt readback decode: a Pelco-D tilt
// response word is elevation in hundredths of a degree, decoded exactly like
// pan (bench-confirmed 2026-08-28; the earlier raw-encoder calibration model
// was a misread and has been deleted).
func TestTiltDecodeIsTextbook(t *testing.T) {
	eng := New(Options{Transport: config.TransportSim, Addr: 1})
	// Feed a tilt response frame directly (never started: no actor loop needed;
	// handleFrame only touches readback state, pos, and logging).
	eng.handleFrame(serialio.Event{Frame: pelco.TiltResponse(1, 45)}, true)
	if !eng.haveTilt || eng.curTilt != 45 {
		t.Fatalf("tilt decode: haveTilt=%v curTilt=%.2f, want exactly 45", eng.haveTilt, eng.curTilt)
	}
	eng.handleFrame(serialio.Event{Frame: pelco.TiltResponse(1, 12.34)}, true)
	if eng.curTilt != 12.34 {
		t.Fatalf("tilt decode: curTilt=%.2f, want 12.34 (hundredths of a degree)", eng.curTilt)
	}
}

// seqResponder is a Pelco-D TCP "bridge" that records every received frame as
// a (kind, arrival-order) sequence while answering position queries from a
// fixed position — used to pin the absolute-set wire discipline (quiet window
// around the sets, re-send on no travel).
type seqResponder struct {
	mu  sync.Mutex
	log []seqFrame
	ln  net.Listener
}

type seqFrame struct {
	kind string // "set" | "query" | "stop" | "jog" | "misc"
}

func newSeqResponder(addr byte) *seqResponder {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	r := &seqResponder{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.serve(c)
		}
	}()
	return r
}

func (r *seqResponder) serve(c net.Conn) {
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
		kind := "misc"
		switch {
		case f.IsSetPan(), f.IsSetTilt():
			kind = "set"
		case f.IsQueryPan(), f.IsQueryTilt():
			kind = "query"
			// Answer from a fixed 0°/0° position; never advance on a set.
			resp := pelco.PanResponse(1, 0)
			if f.IsQueryTilt() {
				resp = pelco.TiltResponse(1, 0)
			}
			_, _ = c.Write(resp.Bytes())
		case f.Cmd1 == 0 && f.Cmd2 == 0 && f.Data1 == 0 && f.Data2 == 0:
			kind = "stop"
		}
		r.mu.Lock()
		r.log = append(r.log, seqFrame{kind: kind})
		r.mu.Unlock()
	}
}

// count returns how many frames of the given kind have arrived.
func (r *seqResponder) count(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, f := range r.log {
		if f.kind == kind {
			n++
		}
	}
	return n
}

// kinds returns the frame-kind sequence (test-failure diagnostics).
func (r *seqResponder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.log))
	for i, f := range r.log {
		out[i] = f.kind
	}
	return out
}

// frames returns a copy of the recorded sequence.
func (r *seqResponder) frames() []seqFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]seqFrame, len(r.log))
	copy(out, r.log)
	return out
}

func (r *seqResponder) close() { _ = r.ln.Close() }
