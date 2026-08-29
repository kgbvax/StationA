package simhead

import (
	"testing"
	"time"

	"pelcobridge2/internal/pelco"
)

// The simulator is the stand-in for the bench head: every engine test leans on
// it, so each quirk the bench documented is pinned here — textbook decode,
// D/P adaptivity, quiet-line-only sets, garbage-while-moving, self-test
// re-home. Timing-tolerant: polls with deadlines rather than sleeping fixed
// amounts wherever an assertion depends on motion.

// readOne blocks for exactly one reply frame with a timeout.
func readOne(t *testing.T, h *Head) pelco.RxFrame {
	t.Helper()
	ch := make(chan pelco.RxFrame, 1)
	go func() {
		buf := make([]byte, 32)
		n, err := h.Read(buf)
		if err != nil {
			return
		}
		if rx, ok := DecodeAny(buf[:n]); ok {
			ch <- rx
		}
	}()
	select {
	case rx := <-ch:
		return rx
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a reply")
		return pelco.RxFrame{}
	}
}

func mustWrite(t *testing.T, h *Head, b any) {
	t.Helper()
	var wire []byte
	switch v := b.(type) {
	case pelco.Frame:
		wire = v[:]
	case []byte:
		wire = v
	default:
		t.Fatalf("mustWrite: unsupported type %T", b)
	}
	if err := h.Write(wire); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestQueryPanTextbookDecode(t *testing.T) {
	h := New(Options{Addr: 1, PanDeg: 123.45, TiltDeg: 10})
	defer h.Close()

	mustWrite(t, h, pelco.QueryFrame(1, pelco.OpQueryPan))
	rx := readOne(t, h)
	if rx.Op() != pelco.OpRspPan {
		t.Fatalf("wrong reply: %+v", rx)
	}
	if got := pelco.WordToDeg(rx.Word()); got != 123.45 {
		t.Fatalf("pan readback %.2f, want 123.45 (textbook degrees×100)", got)
	}
}

func TestQueryTiltTextbookDecode(t *testing.T) {
	h := New(Options{Addr: 1, PanDeg: 200, TiltDeg: 45.5})
	defer h.Close()

	mustWrite(t, h, pelco.QueryFrame(1, pelco.OpQueryTilt))
	rx := readOne(t, h)
	if rx.Op() != pelco.OpRspTilt {
		t.Fatalf("wrong reply: %+v", rx)
	}
	if got := pelco.WordToDeg(rx.Word()); got != 45.5 {
		t.Fatalf("tilt readback %.2f, want 45.5", got)
	}
}

func TestWrongAddrIgnored(t *testing.T) {
	h := New(Options{Addr: 1, PanDeg: 10})
	defer h.Close()

	mustWrite(t, h, pelco.QueryFrame(2, pelco.OpQueryPan))
	// No reply must come: wait a moment and confirm the replies channel is
	// empty by issuing an addressed query and expecting THAT reply first.
	mustWrite(t, h, pelco.QueryFrame(1, pelco.OpQueryTilt))
	rx := readOne(t, h)
	if rx.Op() != pelco.OpRspTilt {
		t.Fatalf("first reply must be the addressed query's, got op %02X", rx.Op())
	}
}

func TestSetRejectedOnBusyLine(t *testing.T) {
	h := New(Options{Addr: 1, PanDeg: 10, TiltDeg: 5, SilenceRequired: 500 * time.Millisecond})
	defer h.Close()

	// Back-to-back traffic: the set lands well inside the silence window, so
	// the head ignores it (bench fact: sets only work on a quiet line).
	mustWrite(t, h, pelco.QueryFrame(1, pelco.OpQueryPan))
	readOne(t, h)
	mustWrite(t, h, pelco.SetPanFrame(1, 200))

	deadline := time.Now().Add(700 * time.Millisecond) // longer than the silence window
	for time.Now().Before(deadline) {
		if h.PanDeg() != 10 {
			t.Fatalf("set applied on a busy line: pan=%.2f", h.PanDeg())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSetAppliesWhenQuiet(t *testing.T) {
	h := New(Options{Addr: 1, PanDeg: 10, TiltDeg: 5, SilenceRequired: 30 * time.Millisecond,
		RateAzDegPerS: 200}) // fast: the default 4°/s needs 45 s to cross 190°
	defer h.Close()

	time.Sleep(60 * time.Millisecond) // quiet line
	mustWrite(t, h, pelco.SetPanFrame(1, 200))

	if !waitFor(t, 3*time.Second, func() bool { return h.PanDeg() == 200 }) {
		t.Fatalf("set pan did not converge: pan=%.2f", h.PanDeg())
	}
}

func TestGarbageWhileMoving(t *testing.T) {
	h := New(Options{Addr: 1, PanDeg: 10, TiltDeg: 5, SilenceRequired: 30 * time.Millisecond})
	defer h.Close()

	mustWrite(t, h, pelco.JogFrame(1, pelco.OpRight, pelco.DefaultJogSpeed))
	mustWrite(t, h, pelco.QueryFrame(1, pelco.OpQueryPan))
	rx := readOne(t, h)
	cur := uint16(h.PanDeg() * 100)
	if rx.Word() == cur {
		t.Fatalf("readback while moving must be garbage, matched real position %04X", cur)
	}

	// After a stop, the readback is textbook again.
	mustWrite(t, h, pelco.StopFrame(1))
	mustWrite(t, h, pelco.QueryFrame(1, pelco.OpQueryPan))
	rx = readOne(t, h)
	if pelco.WordToDeg(rx.Word()) != h.PanDeg() {
		t.Fatalf("post-stop readback %.2f != position %.2f",
			pelco.WordToDeg(rx.Word()), h.PanDeg())
	}
}

func TestStopHaltsJog(t *testing.T) {
	h := New(Options{Addr: 1, PanDeg: 10, TiltDeg: 5})
	defer h.Close()

	mustWrite(t, h, pelco.JogFrame(1, pelco.OpRight, pelco.DefaultJogSpeed))
	if !waitFor(t, 2*time.Second, func() bool { return h.PanDeg() > 12 }) {
		t.Fatal("jog never moved the head")
	}

	mustWrite(t, h, pelco.StopFrame(1))
	stopped := h.PanDeg()
	if !waitFor(t, 500*time.Millisecond, func() bool { return !h.Moving() }) {
		t.Fatal("head still moving after stop")
	}
	time.Sleep(100 * time.Millisecond)
	if h.PanDeg() != stopped {
		t.Fatalf("pan drifted after stop: %.2f → %.2f", stopped, h.PanDeg())
	}
}

func TestSetTiltClampedToTravel(t *testing.T) {
	h := New(Options{Addr: 1, PanDeg: 10, TiltDeg: 5, SilenceRequired: 30 * time.Millisecond,
		RateElDegPerS: 100})
	defer h.Close()

	time.Sleep(60 * time.Millisecond)
	mustWrite(t, h, pelco.SetTiltFrame(1, 120)) // beyond the 90° bracket

	if !waitFor(t, 3*time.Second, func() bool { return h.TiltDeg() == 90 }) {
		t.Fatalf("tilt did not stop at the 90° bracket: %.2f", h.TiltDeg())
	}
}

func TestSelfTestRehomes(t *testing.T) {
	h := New(Options{Addr: 1, PanDeg: 200, TiltDeg: 45, SilenceRequired: 30 * time.Millisecond,
		RateAzDegPerS: 200, RateElDegPerS: 100})
	defer h.Close()

	time.Sleep(60 * time.Millisecond)
	mustWrite(t, h, pelco.SelfTestFrame(1)) // preset call 125: DANGEROUS re-home

	if !waitFor(t, 5*time.Second, func() bool {
		return h.PanDeg() == 0 && h.TiltDeg() == 0
	}) {
		t.Fatalf("self-test did not re-home to 0/0: pan=%.2f tilt=%.2f", h.PanDeg(), h.TiltDeg())
	}
}

// Preset set/call 105 flips the periodic self-check; the self-test's factory
// restore re-enables it.
func TestSelfCheckToggle(t *testing.T) {
	h := New(Options{Addr: 1})
	defer h.Close()

	if !h.SelfCheck() {
		t.Fatal("factory default: the self-check must start enabled")
	}
	mustWrite(t, h, pelco.SelfCheckDisableFrame(1)) // preset set 105
	if h.SelfCheck() {
		t.Error("preset set 105 did not disable the self-check")
	}
	mustWrite(t, h, pelco.SelfCheckEnableFrame(1)) // preset call 105
	if !h.SelfCheck() {
		t.Error("preset call 105 did not re-enable the self-check")
	}
	mustWrite(t, h, pelco.SelfCheckDisableFrame(1))

	// Factory defaults (self-test) restore the self-check along with the rest.
	mustWrite(t, h, pelco.SelfTestFrame(1))
	if !h.SelfCheck() {
		t.Error("self-test did not restore the factory self-check")
	}
}

func TestOtherPresetCallsIgnored(t *testing.T) {
	h := New(Options{Addr: 1, PanDeg: 200, TiltDeg: 45, SilenceRequired: 30 * time.Millisecond})
	defer h.Close()

	time.Sleep(60 * time.Millisecond)
	// Any preset call that is NOT 125 must not move the head.
	mustWrite(t, h, pelco.PresetCallFrame(1, 3))
	time.Sleep(300 * time.Millisecond)
	if h.PanDeg() != 200 || h.TiltDeg() != 45 {
		t.Fatalf("ordinary preset call moved the head: pan=%.2f tilt=%.2f", h.PanDeg(), h.TiltDeg())
	}
}

func TestCloseUnblocksRead(t *testing.T) {
	h := New(Options{Addr: 1})
	done := make(chan error, 1)
	go func() {
		_, err := h.Read(make([]byte, 16))
		if err != nil {
			done <- err
		}
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	h.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not unblock Read")
	}
}

// waitFor polls cond every 20 ms until it holds or the deadline passes.
// Returns whether it held — call sites use it to fail with their own context.
func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// DecodeAny is the simulator's front door; a frame must parse and a bad
// checksum must be refused, mirroring the head's silence on garbage.
func TestDecodeAnyEnvelopes(t *testing.T) {
	d := pelco.QueryFrame(1, pelco.OpQueryPan)
	rx, ok := DecodeAny(d[:])
	if !ok || rx.Op() != pelco.OpQueryPan {
		t.Fatalf("D frame: %+v ok=%v", rx, ok)
	}

	bad := d
	bad[6] ^= 0xFF
	if _, ok := DecodeAny(bad[:]); ok {
		t.Fatal("bad checksum must be refused")
	}
	if _, ok := DecodeAny([]byte{0xFF, 0x01}); ok {
		t.Fatal("short buffer must be refused")
	}
}
