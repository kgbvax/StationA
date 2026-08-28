package main

import (
	"fmt"
	"strings"
	"testing"
)

// Frames taken verbatim from the doc sheet
// (pelcotest/docs/更新云台说明书串口指令.20240327154146056-2.xls).
func TestBuildFramesMatchDoc(t *testing.T) {
	tests := []struct {
		name  string
		addr  byte
		cmd   Command
		input string
		want  string
	}{
		{"up", 1, Command{Cmd2: 0x08, Param: ParamSpeeds}, "20 20", "FF 01 00 08 00 20 29"},
		{"down", 1, Command{Cmd2: 0x10, Param: ParamSpeeds}, "20 20", "FF 01 00 10 00 20 31"},
		{"left", 1, Command{Cmd2: 0x04, Param: ParamSpeeds}, "20 20", "FF 01 00 04 20 00 25"},
		{"right", 1, Command{Cmd2: 0x02, Param: ParamSpeeds}, "20 20", "FF 01 00 02 20 00 23"},
		{"stop", 1, Command{Cmd2: 0x00}, "", "FF 01 00 00 00 00 01"},
		{"preset1 set", 1, Command{Cmd2: 0x03, Param: ParamPreset}, "1", "FF 01 00 03 00 01 05"},
		{"preset1 call", 1, Command{Cmd2: 0x07, Param: ParamPreset}, "1", "FF 01 00 07 00 01 09"},
		{"105 set", 1, Command{Cmd2: 0x03, D2: 0x69}, "", "FF 01 00 03 00 69 6D"},
		{"105 call", 1, Command{Cmd2: 0x07, D2: 0x69}, "", "FF 01 00 07 00 69 71"},
		{"aux on", 1, Command{Cmd2: 0x09}, "", "FF 01 00 09 00 00 0A"},
		{"aux off", 1, Command{Cmd2: 0x0B}, "", "FF 01 00 0B 00 00 0C"},
		{"110 set", 1, Command{Cmd2: 0x03, D2: 0x6E}, "", "FF 01 00 03 00 6E 72"},
		{"110 call", 1, Command{Cmd2: 0x07, D2: 0x6E}, "", "FF 01 00 07 00 6E 76"},
		{"120 call", 1, Command{Cmd2: 0x07, D2: 0x78}, "", "FF 01 00 07 00 78 80"},
		{"40 defaults", 1, Command{Cmd2: 0x07, D2: 0x7D}, "", "FF 01 00 07 00 7D 85"},
		{"41 tilt query", 1, Command{Cmd2: 0x53}, "", "FF 01 00 53 00 00 54"},
		{"43 pan query", 1, Command{Cmd2: 0x51}, "", "FF 01 00 51 00 00 52"},
		{"42 tilt set 90", 1, Command{Cmd2: 0x4D, Param: ParamDegrees}, "90", "FF 01 00 4D 23 28 99"},
		{"44 pan set 300", 1, Command{Cmd2: 0x4B, Param: ParamDegrees}, "300", "FF 01 00 4B 75 30 F1"},
		// The doc's frame for SN 111 carries no parameter: d1=00, d2=6F.
		{"111 call", 1, Command{Cmd2: 0x07, D2: 0x6F}, "", "FF 01 00 07 00 6F 77"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildFrame(tt.addr, tt.cmd, tt.input)
			if err != nil {
				t.Fatalf("BuildFrame: %v", err)
			}
			if got.Hex() != tt.want {
				t.Errorf("got %s, want %s", got.Hex(), tt.want)
			}
			if !got.ChkOK() {
				t.Errorf("frame %s has bad checksum", got.Hex())
			}
		})
	}
}

// The menu entry for SN 111 must emit the doc's frame with no parameter path.
// It used to be ParamSeconds, which wrote the seconds over data2 and destroyed
// the 0x6F selector for every value except 111 (0x6F == 111 decimal — the
// coincidence the old test baked in).
func TestGuardReturnTimeHasNoParameter(t *testing.T) {
	c := cmdByName(t, "111 call")
	if c.Param != ParamNone {
		t.Fatalf("111 call must take no parameter, got Param=%v", c.Param)
	}
	f, err := BuildFrame(1, c, "")
	if err != nil {
		t.Fatal(err)
	}
	if f.Hex() != "FF 01 00 07 00 6F 77" {
		t.Errorf("got %s, want the doc's FF 01 00 07 00 6F 77", f.Hex())
	}
}

func cmdByName(t *testing.T, name string) Command {
	t.Helper()
	for _, c := range Commands {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no command named %q", name)
	return Command{}
}

func TestParseHexRoundTrip(t *testing.T) {
	in := "FF 01 00 5B 1F 3F BA"
	f, err := ParseHex(in)
	if err != nil {
		t.Fatalf("ParseHex: %v", err)
	}
	if f.Hex() != in {
		t.Errorf("got %s, want %s", f.Hex(), in)
	}
	if !f.ChkOK() {
		t.Error("doc response frame should have valid checksum")
	}
	if f.Word() != 0x1F3F {
		t.Errorf("word = %04X, want 1F3F", f.Word())
	}
	if _, err := ParseHex("FF 01 00 53"); err == nil {
		t.Error("short input should error")
	}
	// A one-nibble field used to be zero-extended: "54" mistyped as "5" went
	// out as 0x05 and produced a checksum error the operator then chased.
	if _, err := ParseWireHex("FF 01 00 53 00 00 5"); err == nil {
		t.Error("one-nibble byte should be rejected, not read as 0x05")
	}
}

func TestAssembler(t *testing.T) {
	good, _ := ParseHex("FF 01 00 5B 1F 3F BA")
	good2, _ := ParseHex("FF 01 00 59 75 2F FE")

	// Leading garbage, then two back-to-back frames.
	stream := append([]byte{0x12, 0xFF, 0x00}, good[:]...)
	stream = append(stream, good2[:]...)

	a := &Assembler{}
	frames := framesOf(a.Feed(stream))
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	if frames[0].Hex() != good.Hex() || frames[1].Hex() != good2.Hex() {
		t.Errorf("frames mismatch: %s / %s", frames[0].Hex(), frames[1].Hex())
	}

	// Split delivery across feeds must still reassemble.
	a = &Assembler{}
	if got := framesOf(a.Feed(good[:3])); len(got) != 0 {
		t.Fatalf("partial frame should not emit")
	}
	frames = framesOf(a.Feed(good[3:]))
	if len(frames) != 1 || frames[0].Hex() != good.Hex() {
		t.Fatalf("split frame not reassembled")
	}

	// Bad checksum resyncs instead of emitting.
	bad := append([]byte(nil), good[:]...)
	bad[6] ^= 0xFF
	a = &Assembler{}
	a.Feed(bad)
	frames = framesOf(a.Feed(good[:]))
	if len(frames) != 1 || frames[0].Hex() != good.Hex() {
		t.Fatalf("resync after bad checksum failed: %v", frames)
	}
}

func framesOf(events []Event) []RxFrame {
	var out []RxFrame
	for _, e := range events {
		if !e.IsNoise() {
			out = append(out, e.Frame)
		}
	}
	return out
}

func noisesOf(events []Event) []Noise {
	var out []Noise
	for _, e := range events {
		if e.IsNoise() {
			out = append(out, e.Noise)
		}
	}
	return out
}

func tiltFrame(raw uint16) []byte {
	f := Build(1, 0x00, 0x5B, byte(raw>>8), byte(raw))
	return f[:]
}

// A truncated reply must never merge with the next one. The next frame's 0xFF
// start byte lands exactly where the lost checksum byte was, and for ~0.33% of
// the head's travel the merged window passes the additive checksum — so ptest
// reported a fabricated position word as "chk ok" and silently discarded the
// genuine reply. FlushIdle after a receive gap closes the window.
func TestTruncatedReplyDoesNotFabricateAFrame(t *testing.T) {
	trunc := tiltFrame(163)  // FF 01 00 5B 00 A3 FF — its checksum byte is 0xFF
	real := tiltFrame(22456) // the genuine next reply: 0° elevation

	// Without a gap flush the two merge into one valid-looking frame.
	a := &Assembler{}
	merged := framesOf(a.Feed(append(append([]byte{}, trunc[:6]...), real...)))
	if len(merged) == 1 && merged[0].Word() == 163 {
		t.Logf("baseline (no gap): fabricated word=%d as expected", merged[0].Word())
	}

	// With a gap flush between the two reads, the partial is reported as
	// partial and the genuine reply survives intact.
	a = &Assembler{}
	ev := a.Feed(trunc[:6])
	if len(framesOf(ev)) != 0 {
		t.Fatalf("a 6-byte partial must not emit a frame")
	}
	if !a.Pending() {
		t.Fatal("assembler should report the partial as pending")
	}
	ev = a.FlushIdle()
	ns := noisesOf(ev)
	if len(ns) != 1 || len(ns[0]) != 6 {
		t.Fatalf("FlushIdle should surface the 6 stalled bytes, got %v", ns)
	}
	if !ev[0].Partial {
		t.Error("the stalled run should be marked Partial")
	}
	frames := framesOf(a.Feed(real))
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want the genuine reply", len(frames))
	}
	if frames[0].Word() != 22456 {
		t.Errorf("word = %d, want the genuine 22456", frames[0].Word())
	}
}

// A corrupted reply used to produce zero output: rejected bytes were withheld
// until 256 accumulated, which at 2400 baud and one manual query at a time
// never happens. The RX pane was indistinguishable from "no answer".
func TestCorruptedReplyIsReported(t *testing.T) {
	bad := tiltFrame(22456)
	bad[6] ^= 0x01 // corrupt the checksum
	a := &Assembler{}
	ev := a.Feed(bad)
	if len(framesOf(ev)) != 0 {
		t.Fatal("a corrupted frame must not be emitted as a frame")
	}
	total := 0
	for _, n := range noisesOf(ev) {
		total += len(n)
	}
	if total == 0 {
		t.Error("a corrupted reply must be reported as noise, not swallowed")
	}
}

// Feed must return frames and noise in wire order; they used to come back as
// two separate slices and were logged noise-first.
func TestEventsAreInWireOrder(t *testing.T) {
	f1 := tiltFrame(26014)
	f2 := tiltFrame(29573)
	stream := append([]byte{}, f1...)
	for i := 0; i < 10; i++ {
		stream = append(stream, 0x7E)
	}
	stream = append(stream, f2...)

	ev := (&Assembler{}).Feed(stream)
	var order []string
	for _, e := range ev {
		if e.IsNoise() {
			order = append(order, fmt.Sprintf("noise(%d)", len(e.Noise)))
		} else {
			order = append(order, fmt.Sprintf("frame(%d)", e.Frame.Word()))
		}
	}
	want := "frame(26014) noise(10) frame(29573)"
	if got := strings.Join(order, " "); got != want {
		t.Errorf("event order = %q, want %q", got, want)
	}
}

// TestWrapP checks the D→P re-wrap: same logical fields, 8-byte A0/AF
// envelope, XOR checksum. The tilt-query vector is the P transpose of the
// doc's FF 01 00 53 00 00 54; the all-stop matches CommFront's worked
// example transposed to address 1.
func TestWrapP(t *testing.T) {
	tests := []struct {
		name string
		log  string // logical D frame hex
		want string // expected P wire hex
	}{
		{"tilt query", "FF 01 00 53 00 00 54", "A0 01 00 53 00 00 AF 5D"},
		{"stop", "FF 01 00 00 00 00 01", "A0 01 00 00 00 00 AF 0E"},
		{"pan set 300", "FF 01 00 4B 75 30 F1", "A0 01 00 4B 75 30 AF 00"},
	}
	for _, tt := range tests {
		f, err := ParseHex(tt.log)
		if err != nil {
			t.Fatalf("%s: ParseHex: %v", tt.name, err)
		}
		got := WrapP(f)
		if hexBytes(got) != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, hexBytes(got), tt.want)
		}
		if !pChkOK(got) {
			t.Errorf("%s: wrapped frame fails P checksum", tt.name)
		}
	}
}

func hexBytes(b []byte) string {
	out := make([]string, len(b))
	for i, v := range b {
		out[i] = fmt.Sprintf("%02X", v)
	}
	return strings.Join(out, " ")
}

// TestAssemblerP verifies the RX side accepts Pelco-P envelopes.
func TestAssemblerP(t *testing.T) {
	pTilt := []byte{0xA0, 0x01, 0x00, 0x5B, 0x1F, 0x3F, 0xAF, 0x00}
	pTilt[7] = pXor(pTilt)

	dTilt, _ := ParseHex("FF 01 00 5B 1F 3F BA")
	stream := append(append([]byte{}, pTilt...), dTilt[:]...)

	a := &Assembler{}
	frames := framesOf(a.Feed(stream))
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2: %+v", len(frames), frames)
	}
	if !frames[0].P || !frames[0].ChkOK() || frames[0].Word() != 0x1F3F {
		t.Errorf("P frame wrong: %+v", frames[0])
	}
	if frames[0].Frame[3] != 0x5B {
		t.Errorf("P cmd2 = %02X, want 5B (logical field layout)", frames[0].Frame[3])
	}
	if frames[1].P || frames[1].Hex() != dTilt.Hex() {
		t.Errorf("D frame after P wrong: %+v", frames[1])
	}

	// A corrupted P checksum drops bytes and resyncs; the D frame survives.
	badP := append([]byte(nil), pTilt...)
	badP[7] ^= 0xFF
	a = &Assembler{}
	frames = framesOf(a.Feed(append(badP, dTilt[:]...)))
	if len(frames) != 1 || frames[0].P {
		t.Fatalf("bad P checksum should resync to the D frame: %+v", frames)
	}
}

// TestBuildWireModes verifies the TX path honors the mode flag.
func TestBuildWireModes(t *testing.T) {
	dq := Command{Cmd2: 0x53} // tilt query
	wD, fD, err := BuildWire(1, dq, "", false)
	if err != nil {
		t.Fatalf("BuildWire D: %v", err)
	}
	if hexBytes(wD) != "FF 01 00 53 00 00 54" || fD.Hex() != "FF 01 00 53 00 00 54" {
		t.Errorf("D wire/log wrong: %s / %s", hexBytes(wD), fD.Hex())
	}
	wP, fP, err := BuildWire(1, dq, "", true)
	if err != nil {
		t.Fatalf("BuildWire P: %v", err)
	}
	if hexBytes(wP) != "A0 01 00 53 00 00 AF 5D" {
		t.Errorf("P wire wrong: %s", hexBytes(wP))
	}
	if fP.Hex() != "FF 01 00 53 00 00 54" {
		t.Errorf("P logical view wrong: %s", fP.Hex())
	}

	// Raw entry: sent as typed in both lengths.
	w, _, err := BuildWire(1, Command{Param: ParamHex}, "FF 01 00 53 00 00 54", true)
	if err != nil || hexBytes(w) != "FF 01 00 53 00 00 54" {
		t.Errorf("raw 7-byte entry must be sent as typed: %s err=%v", hexBytes(w), err)
	}
	w, _, err = BuildWire(1, Command{Param: ParamHex}, "A0 01 00 53 00 00 AF 5D", false)
	if err != nil || hexBytes(w) != "A0 01 00 53 00 00 AF 5D" {
		t.Errorf("raw 8-byte entry must be sent as typed: %s err=%v", hexBytes(w), err)
	}
}

// A TX Pelco-P frame must log the XOR byte that is on the wire, not the
// additive Pelco-D sum from the logical field view. The log used to print
// chk=54 next to a wire carrying 5D — and the README documented 5D.
func TestTxPelcoPLogsWireChecksum(t *testing.T) {
	wire, f, err := BuildWire(1, Command{Cmd2: 0x53}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	rf := RxFrame{Frame: f, Wire: wire, P: true}
	if rf.chkByte() != wire[7] {
		t.Errorf("logged chk = %02X, wire carries %02X", rf.chkByte(), wire[7])
	}
	out := strings.Join(Decode(rf, "TX", hypTiltCal), "\n")
	if !strings.Contains(out, "chk=5D ok") {
		t.Errorf("expected chk=5D ok in:\n%s", out)
	}
}

// All eight bytes of an 8-byte raw entry must appear in the log even when it is
// not a Pelco-P envelope; the 8th byte used to vanish and byte 7 was reported
// as a bad checksum.
func TestRawEightByteNonPLogsAllBytes(t *testing.T) {
	wire, f, err := BuildWire(1, Command{Param: ParamHex}, "FF 01 00 53 00 00 54 99", false)
	if err != nil {
		t.Fatal(err)
	}
	rf := RxFrame{Frame: f, Wire: wire, P: false}
	if got := rf.Hex(); got != "FF 01 00 53 00 00 54 99" {
		t.Errorf("Hex() = %q, want all 8 bytes", got)
	}
	out := strings.Join(Decode(rf, "TX", hypTiltCal), "\n")
	if !strings.Contains(out, "unverifiable") {
		t.Errorf("an 8-byte non-P frame has no checksum rule; want 'unverifiable' in:\n%s", out)
	}
}

// cmd2 values with bit 0 set are extended opcodes, not direction bitfields.
// They used to be rendered as movement: preset-call as "right,left", aux-off as
// "right,up".
func TestExtendedOpcodesAreNotDecodedAsBits(t *testing.T) {
	for _, cmd2 := range []byte{0x03, 0x07, 0x09, 0x0B} {
		f := Build(1, 0x00, cmd2, 0x00, 0x01)
		out := strings.Join(Decode(RxFrame{Frame: f, Wire: f[:]}, "TX", hypTiltCal), "\n")
		if strings.Contains(out, "pelco-d bits") {
			t.Errorf("cmd2=%02X is an extended opcode but was decoded as direction bits:\n%s", cmd2, out)
		}
	}
	// A genuine jog frame still gets its bits.
	f := Build(1, 0x00, 0x08, 0x00, 0x20)
	out := strings.Join(Decode(RxFrame{Frame: f, Wire: f[:]}, "TX", hypTiltCal), "\n")
	if !strings.Contains(out, "pelco-d bits: up") {
		t.Errorf("jog frame lost its bit decode:\n%s", out)
	}
}

// hypTiltCal is a hypothesis used only to exercise the -tilt-cal code path. It
// is NOT a calibration: elevation does not appear in the 0x5B word (re-checked
// on the bench 2026-08-27), so nothing here asserts a real decode.
var hypTiltCal = TiltCal{Raw0: 22456, Raw90: 54485}

// By DEFAULT the tilt hint must assert nothing about elevation: it states the
// manual's claim, flags it as impossible, and says the meaning is unknown.
func TestTiltHintAssertsNothingByDefault(t *testing.T) {
	for _, raw := range []uint16{22456, 38470, 54485} {
		f := Build(1, 0x00, 0x5B, byte(raw>>8), byte(raw))
		out := strings.Join(Decode(RxFrame{Frame: f, Wire: f[:]}, "RX", TiltCal{}), "\n")
		if !strings.Contains(out, "UNKNOWN") {
			t.Errorf("raw %d: the hint must say the meaning is unknown:\n%s", raw, out)
		}
		if !strings.Contains(out, "impossible") {
			t.Errorf("raw %d: word/100 = %.2f° is outside 0..90 and must be flagged:\n%s",
				raw, float64(raw)/100, out)
		}
		if strings.Contains(out, "° el") || strings.Contains(out, "hyp:") {
			t.Errorf("raw %d: no elevation may be asserted without -tilt-cal:\n%s", raw, out)
		}
	}
}

// With -tilt-cal the reading appears, but only ever labelled as an unverified
// hypothesis so it cannot be mistaken for a measurement.
func TestTiltHypothesisIsLabelledUnverified(t *testing.T) {
	f := Build(1, 0x00, 0x5B, 0x96, 0x46) // raw 38470
	out := strings.Join(Decode(RxFrame{Frame: f, Wire: f[:]}, "RX", hypTiltCal), "\n")
	if !strings.Contains(out, "hyp: raw 38470 → 45.00°") {
		t.Errorf("expected the hypothesis reading:\n%s", out)
	}
	if !strings.Contains(out, "UNVERIFIED") {
		t.Errorf("the hypothesis must be marked UNVERIFIED:\n%s", out)
	}
	if !strings.Contains(out, "UNKNOWN") {
		t.Errorf("the unknown-meaning caveat must survive -tilt-cal:\n%s", out)
	}
}

// Every line Decode emits must fit the log pane on an 80-column terminal
// (80 - menuWidth - 2 = 52), or the position word and checksum are truncated
// off the right edge with no way to scroll to them.
func TestDecodeLinesFitNarrowPane(t *testing.T) {
	cases := [][3]byte{{0x5B, 0xD4, 0xD5}, {0x59, 0x75, 0x2F}, {0x4D, 0x23, 0x28},
		{0x4B, 0x75, 0x30}, {0x53, 0, 0}, {0x51, 0, 0}, {0x08, 0, 0x20}}
	for _, c := range cases {
		f := Build(1, 0x00, c[0], c[1], c[2])
		for _, l := range Decode(RxFrame{Frame: f, Wire: f[:]}, "RX", hypTiltCal) {
			if n := len([]rune(l)); n > maxLogCol {
				t.Errorf("cmd2=%02X line is %d cols (max %d): %q", c[0], n, maxLogCol, l)
			}
		}
	}
}

// Every "NN call" doc command is really preset-call NN, so the plain
// "preset call N" entry must be gated when N names a destructive command.
func TestDestructivePresetCallsAreGated(t *testing.T) {
	presetCall := cmdByName(t, "preset call N")
	defaults40 := cmdByName(t, "40 defaults+selftest")

	got, _ := BuildFrame(1, presetCall, "125")
	want, _ := BuildFrame(1, defaults40, "")
	if got.Hex() != want.Hex() {
		t.Fatalf("expected preset call 125 to equal command 40: %s vs %s", got.Hex(), want.Hex())
	}
	if DangerousReason(presetCall, "125") == "" {
		t.Error("preset call 125 is the factory reset and must require confirmation")
	}
	if DangerousReason(presetCall, "120") == "" {
		t.Error("preset call 120 clears all presets and must require confirmation")
	}
	if DangerousReason(presetCall, "1") != "" {
		t.Error("preset call 1 is harmless and must not prompt")
	}
	if DangerousReason(cmdByName(t, "120 call"), "") == "" {
		t.Error("the 120 call menu entry must require confirmation")
	}
	if DangerousReason(cmdByName(t, "41 tilt query"), "") != "" {
		t.Error("a tilt query must never prompt")
	}
}

// Numeric parameters must reject trailing junk instead of silently sending a
// different value. A decimal comma is a realistic typo.
func TestParametersRejectTrailingJunk(t *testing.T) {
	deg := Command{Cmd2: 0x4D, Param: ParamDegrees}
	for _, in := range []string{"45,5", "45xyz", "4 5", "NaN", "+45x"} {
		if f, err := BuildFrame(1, deg, in); err == nil {
			t.Errorf("degrees %q should be rejected, produced %s", in, f.Hex())
		}
	}
	for _, in := range []string{"45", "45.5", "0", "90"} {
		if _, err := BuildFrame(1, deg, in); err != nil {
			t.Errorf("degrees %q should be accepted: %v", in, err)
		}
	}
	pre := Command{Cmd2: 0x07, Param: ParamPreset}
	if f, err := BuildFrame(1, pre, "0x7D"); err == nil {
		t.Errorf("preset %q should be rejected, produced %s", "0x7D", f.Hex())
	}
	sp := Command{Cmd2: 0x08, Param: ParamSpeeds}
	if f, err := BuildFrame(1, sp, "20 20 20"); err == nil {
		t.Errorf("three speed bytes should be rejected, produced %s", f.Hex())
	}
}
