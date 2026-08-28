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

// Pelco-P vectors. The all-stop one is CommFront's worked checksum example
// transposed to address 1; the rest are the same logical commands as their
// Pelco-D twins above, re-wrapped as 8-byte 0xA0/0xAF frames with an XOR
// checksum over bytes 1..7.
func TestPelcoPEncoding(t *testing.T) {
	cases := []struct {
		name string
		got  []byte
		want []byte
	}{
		{"stop addr 1", Frame{Addr: 0x01, Proto: ProtocolP}.Bytes(),
			[]byte{0xA0, 0x01, 0x00, 0x00, 0x00, 0x00, 0xAF, 0x0E}},
		{"pan-left speed 0x20 (CommFront worked example, addr 0)", Frame{Cmd2: 0x04, Data1: 0x20, Proto: ProtocolP}.Bytes(),
			[]byte{0xA0, 0x00, 0x00, 0x04, 0x20, 0x00, 0xAF, 0x2B}},
		{"query pan", QueryPan(0x01).BytesIn(ProtocolP),
			[]byte{0xA0, 0x01, 0x00, 0x51, 0x00, 0x00, 0xAF, 0x5F}},
		{"set pan 300°", SetPan(0x01, 300).BytesIn(ProtocolP),
			[]byte{0xA0, 0x01, 0x00, 0x4B, 0x75, 0x30, 0xAF, 0x00}},
		{"pan response 299.99° (doc example word 0x752F)", PanResponse(0x01, 299.99).BytesIn(ProtocolP),
			[]byte{0xA0, 0x01, 0x00, 0x59, 0x75, 0x2F, 0xAF, 0x0D}},
	}
	for _, c := range cases {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s: got % X want % X", c.name, c.got, c.want)
		}
	}
}

func TestParsePRoundTrip(t *testing.T) {
	orig := SetTilt(0x01, 45)
	pb := orig.BytesIn(ProtocolP)
	f, err := ParseP(pb)
	if err != nil {
		t.Fatalf("ParseP: %v", err)
	}
	if f.Addr != orig.Addr || f.Cmd1 != orig.Cmd1 || f.Cmd2 != orig.Cmd2 ||
		f.Data1 != orig.Data1 || f.Data2 != orig.Data2 {
		t.Fatalf("field mismatch: got %+v want %+v", f, orig)
	}
	if f.Proto != ProtocolP {
		t.Fatalf("proto tag = %v want p", f.Proto)
	}
	// Decoded P frame carries the same logical Word as its D twin.
	if f.Word() != orig.Word() {
		t.Fatalf("word mismatch: %d != %d", f.Word(), orig.Word())
	}
}

func TestParsePErrors(t *testing.T) {
	good := QueryPan(1).BytesIn(ProtocolP)
	if _, err := ParseP(good[:7]); err != ErrLength {
		t.Errorf("short frame: got %v want ErrLength", err)
	}
	bad := append([]byte(nil), good...)
	bad[0] = 0xFF // D sync, not P STX
	if _, err := ParseP(bad); err != ErrSync {
		t.Errorf("bad sync: got %v want ErrSync", err)
	}
	bad = append([]byte(nil), good...)
	bad[7] ^= 0xFF
	if _, err := ParseP(bad); err == nil {
		t.Errorf("bad checksum: expected error")
	}
	// Byte 6 must be ETX: without this check, a stray 0xA0 landing just before
	// a response whose payload XORs to zero passes the checksum (the two STX
	// bytes cancel) and swallows the real frame.
	bad = append([]byte(nil), good...)
	bad[6] = 0x00
	if _, err := ParseP(bad); err != ErrSync {
		t.Errorf("bad ETX: got %v want ErrSync", err)
	}
	// The same stream through ParseAny: the bogus window is rejected, one byte
	// is dropped, and the real frame assembles. The attack needs a frame whose
	// addr^cmd1^cmd2^data1 == 0 (the stray and real STX cancel in the checksum):
	// pan response 225.28° → word 0x5800.
	resp := PanResponse(1, 225.28).BytesIn(ProtocolP)
	noisy := append([]byte{STX}, resp...)
	f, _, err := ParseAny(noisy)
	if err == nil {
		t.Fatalf("stray 0xA0 before P response: accepted as %+v", f)
	}
	f, n, err := ParseAny(noisy[1:])
	if err != nil || n != PFrameLen || f.Proto != ProtocolP || f.Word() != 22528 {
		t.Errorf("real response after stray 0xA0: frame=%+v n=%d err=%v", f, n, err)
	}
}

// ParseAny dispatches on the lead byte and reports how many bytes to consume.
func TestParseAny(t *testing.T) {
	d := QueryPan(1)  // FF 01 00 51 00 00 52
	p := QueryTilt(1) // A0 01 00 53 00 00 AF <xor>
	if _, _, err := ParseAny(d.Bytes()); err != nil {
		t.Fatalf("ParseAny(D): %v", err)
	}
	f, _, err := ParseAny(p.BytesIn(ProtocolP))
	if err != nil {
		t.Fatalf("ParseAny(P): %v", err)
	}
	if f.Proto != ProtocolP || f.Cmd2 != cmdQueryTlt {
		t.Fatalf("ParseAny(P) decoded %+v", f)
	}

	// A mixed D+P stream decodes both frames in order.
	mix := append(append([]byte{}, d.Bytes()...), p.BytesIn(ProtocolP)...)
	f1, n1, err := ParseAny(mix)
	if err != nil || n1 != FrameLen {
		t.Fatalf("mix[0]: frame=%+v n=%d err=%v", f1, n1, err)
	}
	f2, n2, err := ParseAny(mix[n1:])
	if err != nil || n2 != PFrameLen || f2.Proto != ProtocolP {
		t.Fatalf("mix[1]: frame=%+v n=%d err=%v", f2, n2, err)
	}

	// Too-short tails ask for more bytes (ErrLength + need), they are not
	// framing errors.
	if _, need, err := ParseAny(d.Bytes()[:3]); err != ErrLength || need != FrameLen {
		t.Errorf("short D tail: need=%d err=%v", need, err)
	}
	if _, need, err := ParseAny(p.BytesIn(ProtocolP)[:5]); err != ErrLength || need != PFrameLen {
		t.Errorf("short P tail: need=%d err=%v", need, err)
	}

	// A byte that is neither sync nor STX is a false start (ErrSync once
	// enough bytes are buffered), so a framing loop drops one byte and
	// rescans. With fewer bytes than a whole frame the verdict is deferred
	// (ErrLength — wait).
	if _, _, err := ParseAny([]byte{0x42, 0xFF, 0x01}); err != ErrLength {
		t.Errorf("undersized false start: err=%v want ErrLength", err)
	}
	if _, _, err := ParseAny([]byte{0x42, 0xFF, 0x01, 0x00, 0x51, 0x00, 0x00, 0x52}); err != ErrSync {
		t.Errorf("false start: err=%v want ErrSync", err)
	}
}
