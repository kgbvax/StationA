package pelco

import (
	"bytes"
	"testing"
)

func TestBytesChecksum(t *testing.T) {
	// Stop on address 1: FF 01 00 00 00 00 01
	got := Stop(0x01).Bytes()
	want := []byte{0xFF, 0x01, 0x00, 0x00, 0x00, 0x00, 0x01}
	if !bytes.Equal(got, want) {
		t.Fatalf("Stop: got % X want % X", got, want)
	}
}

func TestChecksumWraparound(t *testing.T) {
	// Sum exceeds 256 and must wrap mod 256.
	f := Frame{Addr: 0xFF, Cmd1: 0xFF, Cmd2: 0x02, Data1: 0x01, Data2: 0x00}
	// 0xFF+0xFF+0x02+0x01+0x00 = 0x201 -> 0x01
	if got := f.Bytes()[6]; got != 0x01 {
		t.Fatalf("checksum wrap: got %#02x want 0x01", got)
	}
}

func TestSetPanEncoding(t *testing.T) {
	// 359° -> 35900 -> 0x8C3C. Set Pan on addr 1.
	f := SetPan(0x01, 359)
	if f.Cmd2 != cmdSetPan {
		t.Fatalf("cmd2 = %#02x want %#02x", f.Cmd2, cmdSetPan)
	}
	if w := f.Word(); w != 35900 {
		t.Fatalf("word = %d want 35900", w)
	}
	want := []byte{0xFF, 0x01, 0x00, 0x4B, 0x8C, 0x3C, 0x14}
	if got := f.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("SetPan(359): got % X want % X", got, want)
	}
}

func TestPanWrap(t *testing.T) {
	cases := map[float64]uint16{0: 0, 360: 0, 359.99: 35999, -1: 35900, 720.5: 50}
	for deg, want := range cases {
		if got := panHundredths(deg); got != want {
			t.Errorf("panHundredths(%g) = %d want %d", deg, got, want)
		}
	}
}

func TestTiltClamp(t *testing.T) {
	cases := map[float64]uint16{-5: 0, 0: 0, 45: 4500, 90: 9000, 120: 9000}
	for deg, want := range cases {
		if got := tiltHundredths(deg); got != want {
			t.Errorf("tiltHundredths(%g) = %d want %d", deg, got, want)
		}
	}
}

func TestParseRoundTrip(t *testing.T) {
	orig := SetTilt(0x01, 45)
	f, err := Parse(orig.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f != orig {
		t.Fatalf("round trip mismatch: %+v != %+v", f, orig)
	}
}

func TestParseErrors(t *testing.T) {
	good := SetPan(1, 90).Bytes()
	if _, err := Parse(good[:6]); err != ErrLength {
		t.Errorf("short frame: got %v want ErrLength", err)
	}
	bad := append([]byte(nil), good...)
	bad[0] = 0x00
	if _, err := Parse(bad); err != ErrSync {
		t.Errorf("bad sync: got %v want ErrSync", err)
	}
	bad = append([]byte(nil), good...)
	bad[6] ^= 0xFF
	if _, err := Parse(bad); err == nil {
		t.Errorf("bad checksum: expected error")
	}
}

func TestDirectionCmd2(t *testing.T) {
	cases := map[Direction]byte{
		{Pan: 1}:            dirRight,
		{Pan: -1}:           dirLeft,
		{Tilt: 1}:           dirUp,
		{Tilt: -1}:          dirDown,
		{Pan: 1, Tilt: 1}:   dirRight | dirUp,
		{Pan: -1, Tilt: -1}: dirLeft | dirDown,
		{}:                  0,
	}
	for d, want := range cases {
		if got := d.Cmd2(); got != want {
			t.Errorf("Direction%+v.Cmd2() = %#02x want %#02x", d, got, want)
		}
	}
}

// Real frames observed from the unit / sent by the app, to keep the
// response-vs-command distinction explicit.
func TestObservedFrames(t *testing.T) {
	// Tilt query response at 0°: FF 01 00 5B 00 00 5C.
	resp0, err := Parse([]byte{0xFF, 0x01, 0x00, 0x5B, 0x00, 0x00, 0x5C})
	if err != nil {
		t.Fatalf("parse 0° tilt response: %v", err)
	}
	if !resp0.IsTiltResponse() || HundredthsToDeg(resp0.Word()) != 0 {
		t.Fatalf("0° tilt response decoded wrong: %+v", resp0)
	}

	// Tilt query response at 90° uses the SAME opcode (0x5B), value 9000.
	resp90, err := Parse([]byte{0xFF, 0x01, 0x00, 0x5B, 0x23, 0x28, 0xA7})
	if err != nil {
		t.Fatalf("parse 90° tilt response: %v", err)
	}
	if !resp90.IsTiltResponse() || HundredthsToDeg(resp90.Word()) != 90 {
		t.Fatalf("90° tilt response decoded wrong: %+v", resp90)
	}

	// Pan query responses: 0° → 59 00 00; ~300° → 59 75 2F = 29999 = 299.99°.
	pan0, _ := Parse([]byte{0xFF, 0x01, 0x00, 0x59, 0x00, 0x00, 0x5A})
	if !pan0.IsPanResponse() || HundredthsToDeg(pan0.Word()) != 0 {
		t.Fatalf("0° pan response decoded wrong: %+v", pan0)
	}
	pan300, err := Parse([]byte{0xFF, 0x01, 0x00, 0x59, 0x75, 0x2F, 0xFE})
	if err != nil {
		t.Fatalf("parse 300° pan response: %v", err)
	}
	if !pan300.IsPanResponse() || pan300.Word() != 29999 {
		t.Fatalf("~300° pan response decoded wrong: %.2f", HundredthsToDeg(pan300.Word()))
	}

	// FF 01 00 4D 23 28 99 is the Set-Tilt-90° COMMAND (what the app sends),
	// not a query response — it must NOT be treated as readback.
	cmd := SetTilt(0x01, 90)
	if got, want := cmd.Bytes(), []byte{0xFF, 0x01, 0x00, 0x4D, 0x23, 0x28, 0x99}; !bytes.Equal(got, want) {
		t.Fatalf("SetTilt(90) = % X want % X", got, want)
	}
	if cmd.IsTiltResponse() || cmd.IsPanResponse() {
		t.Fatal("Set-Tilt command must not be detected as a query response")
	}
}

func TestJogSpeedClamp(t *testing.T) {
	// Pan-right at turbo: pan speed must pass 0xFF through; tilt clamps to 0x3F.
	f := Jog(0x01, Direction{Pan: 1}.Cmd2(), TurboSpeed, MaxSpeed)
	if f.Data1 != 0xFF {
		t.Errorf("turbo pan speed = %#02x want 0xFF", f.Data1)
	}
	if f.Data2 != 0x3F {
		t.Errorf("max tilt speed = %#02x want 0x3F", f.Data2)
	}
	// Out-of-range non-turbo value clamps to MaxSpeed, not turbo.
	if got := Jog(1, 0x02, 0x50, 0).Data1; got != 0x3F {
		t.Errorf("over-range speed = %#02x want 0x3F", got)
	}
}

func TestResponseDetection(t *testing.T) {
	pan := Frame{Cmd2: cmdRespPan}
	if !pan.IsPanResponse() || pan.IsTiltResponse() {
		t.Error("pan response misdetected")
	}
	tilt := Frame{Cmd2: cmdRespTilt}
	if !tilt.IsTiltResponse() || tilt.IsPanResponse() {
		t.Error("tilt response misdetected")
	}
}
