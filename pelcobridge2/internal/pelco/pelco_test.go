package pelco

import (
	"math"
	"reflect"
	"testing"
)

func TestBuildChecksum(t *testing.T) {
	// The vendor doc's canonical tilt query: FF 01 00 53 00 00 54.
	f := Build(1, 0x00, 0x53, 0x00, 0x00)
	if got, want := f.Hex(), "FF 01 00 53 00 00 54"; got != want {
		t.Fatalf("Build tilt query: got %s want %s", got, want)
	}
	if !f.ChkOK() {
		t.Fatal("built frame fails its own checksum")
	}
}

func TestBuildGoldenFrames(t *testing.T) {
	cases := []struct {
		name string
		f    Frame
		want string
	}{
		{"stop", StopFrame(1), "FF 01 00 00 00 00 01"},
		{"jog up 0x12", JogFrame(1, OpUp, DefaultJogSpeed), "FF 01 00 08 00 12 1B"},
		{"jog down", JogFrame(1, OpDown, 0x30), "FF 01 00 10 00 30 41"},
		{"jog right", JogFrame(1, OpRight, 0x20), "FF 01 00 02 20 00 23"},
		{"jog left", JogFrame(1, OpLeft, 0x20), "FF 01 00 04 20 00 25"},
		{"query pan", QueryFrame(1, OpQueryPan), "FF 01 00 51 00 00 52"},
		{"query tilt", QueryFrame(1, OpQueryTilt), "FF 01 00 53 00 00 54"},
		{"set pan 123.45", SetPanFrame(1, 123.45), "FF 01 00 4B 30 39 B5"},
		{"set pan wraps", SetPanFrame(1, 361.0), "FF 01 00 4B 00 64 B0"},
		{"set tilt 45", SetTiltFrame(1, 45), "FF 01 00 4D 11 94 F3"},
		{"set tilt clamp", SetTiltFrame(1, 120), "FF 01 00 4D 23 28 99"},
		{"self test", SelfTestFrame(1), "FF 01 00 07 00 7D 85"},
		{"self check disable", SelfCheckDisableFrame(1), "FF 01 00 03 00 69 6D"},
	}
	for _, c := range cases {
		if got := c.f.Hex(); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
		if !c.f.ChkOK() {
			t.Errorf("%s: checksum not ok", c.name)
		}
	}
}

func TestJogSpeedClamp(t *testing.T) {
	if ClampSpeed(0xFF) != MaxSpeed {
		t.Fatalf("ClampSpeed(0xFF) = %02X, want 0x3F", ClampSpeed(0xFF))
	}
	if ClampSpeed(0x12) != 0x12 {
		t.Fatal("in-range speed must pass through")
	}
}

func TestDegWordRoundTrip(t *testing.T) {
	for _, d := range []float64{0, 0.01, 45, 123.45, 359.99, 655.35} {
		w, err := DegToWord(d)
		if err != nil {
			t.Fatalf("DegToWord(%.2f): %v", d, err)
		}
		if got := WordToDeg(w); math.Abs(got-d) > 0.005 {
			t.Fatalf("round trip %.2f → %d → %.4f", d, w, got)
		}
	}
	if _, err := DegToWord(-0.01); err == nil {
		t.Fatal("negative degrees must error, not wrap")
	}
	if _, err := DegToWord(656); err == nil {
		t.Fatal("degrees above word range must error")
	}
	if _, err := DegToWord(math.NaN()); err == nil {
		t.Fatal("NaN must error")
	}
}

func TestNorm360(t *testing.T) {
	cases := map[float64]float64{0: 0, 359.99: 359.99, 360: 0, -12: 348, 720: 0, -361: 359}
	for in, want := range cases {
		if got := Norm360(in); math.Abs(got-want) > 1e-9 {
			t.Fatalf("Norm360(%v) = %v, want %v", in, got, want)
		}
	}
}

// The assembler invariants, each pinned by a bench failure that ptest recorded.

func TestAssemblerFrameAndFields(t *testing.T) {
	a := &Assembler{}
	q := Build(1, 0x00, OpQueryTilt, 0x00, 0x00)
	ev := a.Feed(q[:])
	// A checksum-valid 7-byte frame assembles as a frame — the assembler does
	// not know TX from RX; callers attribute direction.
	if len(ev) != 1 || ev[0].IsNoise() || ev[0].Frame.Op() != OpQueryTilt {
		t.Fatalf("query frame must assemble: %+v", ev)
	}
	r := Build(1, 0x00, OpRspTilt, 0x11, 0x94) // 45.00°
	ev = a.Feed(r[:])
	if len(ev) != 1 || ev[0].IsNoise() {
		t.Fatalf("want one frame, got %+v", ev)
	}
	rf := ev[0].Frame
	if rf.Op() != OpRspTilt || rf.Addr() != 1 {
		t.Fatalf("frame fields: %+v", rf)
	}
	if WordToDeg(rf.Word()) != 45.00 {
		t.Fatalf("tilt readback decode: %.2f", WordToDeg(rf.Word()))
	}
}

func TestAssemblerNoiseBeforeFrame(t *testing.T) {
	a := &Assembler{}
	// One rejected byte, then a valid tilt response: noise must be logged
	// before the frame, matching wire order.
	resp := Frame{0xFF, 1, 0, OpRspTilt, 0x11, 0x94, 0}
	resp[6] = checksum(resp)
	ev := a.Feed(append([]byte{0x42}, resp[:]...))
	if len(ev) != 2 {
		t.Fatalf("want noise + frame, got %d events", len(ev))
	}
	if !ev[0].IsNoise() || len(ev[0].Noise) != 1 || ev[0].Noise[0] != 0x42 {
		t.Fatalf("first event must be the noise run: %+v", ev[0])
	}
	if ev[1].IsNoise() || ev[1].Frame.Word() != 0x1194 {
		t.Fatalf("second event must be the frame: %+v", ev[1])
	}
}

func TestAssemblerBadChecksumRecovers(t *testing.T) {
	a := &Assembler{}
	resp := Frame{0xFF, 1, 0x00, OpRspTilt, 0x11, 0x94, 0xEE} // wrong checksum
	ev := a.Feed(resp[:])
	// The corrupted frame's bytes are surfaced as noise, never silently dropped.
	if len(ev) != 1 || !ev[0].IsNoise() {
		t.Fatalf("bad checksum must surface as noise: %+v", ev)
	}
	// A following valid frame still assembles.
	good := QueryFrame(1, OpQueryPan)
	ev = a.Feed(good[:])
	if len(ev) != 1 || ev[0].IsNoise() {
		t.Fatalf("valid frame after garbage lost: %+v", ev)
	}
}

func TestAssemblerFlushIdlePreventsFabricatedWord(t *testing.T) {
	a := &Assembler{}
	// A truncated reply (lost checksum byte) must not sit in the buffer and
	// merge with the next reply. The failure mode ptest documented: the lost
	// checksum byte was 0xFF, and the NEXT reply's 0xFF start byte lands
	// exactly there — producing a checksum-VALID frame carrying a fabricated
	// position word while the genuine reply is walked off byte by byte.
	// (Genuine reply word 0x574C; its checksum 0xFF was lost on the wire.)
	truncated := []byte{0xFF, 0x01, 0x00, OpRspTilt, 0x57, 0x4C}
	ev := a.Feed(truncated)
	if len(ev) != 0 {
		t.Fatalf("incomplete frame must be held: %+v", ev)
	}
	if !a.Pending() {
		t.Fatal("assembler must report pending")
	}

	// Without an idle flush, the next reply's 0xFF start byte completes the
	// stalled frame as checksum-VALID with the fabricated word 0x574C —
	// demonstrating the failure the gap-flush exists to close.
	merged := a.Feed([]byte{0xFF})
	if len(merged) != 1 || merged[0].IsNoise() || merged[0].Frame.Word() != 0x574C {
		t.Fatalf("without FlushIdle the merge fabricates a valid frame: %+v", merged)
	}

	// The disciplined path: a receive gap flushes the partial FIRST.
	a2 := &Assembler{}
	ev2 := a2.Feed(truncated)
	if len(ev2) != 0 {
		t.Fatalf("incomplete frame must be held: %+v", ev2)
	}
	ev2 = a2.FlushIdle()
	if len(ev2) != 1 || !ev2[0].Partial || !ev2[0].IsNoise() {
		t.Fatalf("want one partial event, got %+v", ev2)
	}

	// The genuine reply then assembles untouched.
	next := QueryFrame(1, OpQueryPan)
	ev2 = a2.Feed(next[:])
	if len(ev2) != 1 || ev2[0].IsNoise() || ev2[0].Frame.Op() != OpQueryPan {
		t.Fatalf("genuine reply corrupted by previous truncation: %+v", ev2)
	}
}

func TestAssemblerWrongBaudGarbageCapped(t *testing.T) {
	a := &Assembler{}
	data := make([]byte, badCap+50)
	for i := range data {
		data[i] = byte(i * 7)
	}
	// One big read of garbage must surface in readable chunks, not one blob.
	count := 0
	for _, e := range a.Feed(data) {
		if e.IsNoise() && !e.Partial {
			count++
		}
	}
	if count == 0 {
		t.Fatal("garbage produced no noise events")
	}
}

func TestDecodeWithinMaxLogCol(t *testing.T) {
	f := Build(1, 0x00, OpRspTilt, 0x11, 0x94)
	for _, l := range Decode(RxFrame{Frame: f}, "RX") {
		if len(l) > maxLogCol {
			t.Fatalf("line exceeds maxLogCol(%d): %q", maxLogCol, l)
		}
	}
}

// Frame equality helper for future tests.
func framesEqual(a, b Frame) bool { return reflect.DeepEqual(a, b) }
