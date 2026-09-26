package civ

import (
	"errors"
	"strings"
	"testing"
)

func hx(b []byte) string { return hexOf(b) }

// BCD round-trips for the three satellite bands (plan U3 scenario).
func TestBCD10RoundTrip(t *testing.T) {
	for _, hz := range []uint64{144_500_000, 432_100_000, 1_296_100_000} {
		bcd, err := BCD10Encode(hz)
		if err != nil {
			t.Fatalf("encode %d: %v", hz, err)
		}
		if len(bcd) != 5 {
			t.Fatalf("encode %d: got %d bytes, want 5", hz, len(bcd))
		}
		back, err := BCD10Decode(bcd)
		if err != nil {
			t.Fatalf("decode %d: %v", hz, err)
		}
		if back != hz {
			t.Errorf("round-trip %d = %d", hz, back)
		}
	}
	// 144.500.000 -> 00 00 50 44 01 (digits LSB-first, low nibble = lower
	// digit, hamlib to_bcd convention). The research brief's "00 00 50 41
	// 01" carries a 41-vs-44 typo — it would encode 141.5 MHz; the real
	// reference implementations (hamlib to_bcd, wfview) produce 44 here.
	bcd, err := BCD10Encode(144_500_000)
	if err != nil {
		t.Fatal(err)
	}
	if got := hx(bcd); got != "0000504401" {
		t.Errorf("BCD(144500000) = %s, want 0000504401", got)
	}
}

func TestBCD10RejectsGarbage(t *testing.T) {
	if _, err := BCD10Encode(10_000_000_000); err == nil {
		t.Error("encode above the 10-digit ceiling must fail")
	}
	// Nibble > 9 is a hard error — garbage never masquerades as a frequency.
	if _, err := BCD10Decode([]byte{0xFA, 0x00, 0x50, 0x41, 0x01}); err == nil {
		t.Error("decode of a non-BCD nibble must fail")
	}
}

func TestModeByteMap(t *testing.T) {
	want := map[byte]string{
		0x00: "lsb",
		0x01: "usb",
		0x02: "am",
		0x03: "cw",
		0x07: "cw", // CW-R reports as cw — reverse filter not representable
		0x05: "fm",
	}
	for b, m := range want {
		got, ok := ModeFromByte(b)
		if !ok || got != m {
			t.Errorf("ModeFromByte(%02x) = %q,%v; want %q,true", b, got, ok, m)
		}
	}
	// DV/DD/RTTY are recognized but unsupported — omitted, never raw (R7).
	for _, b := range []byte{0x04, 0x08, 0x17, 0x22} {
		if _, ok := ModeFromByte(b); ok {
			t.Errorf("ModeFromByte(%02x) must be unsupported", b)
		}
	}
	// Round-trip for every settable canonical mode.
	for _, m := range []string{"lsb", "usb", "am", "cw", "fm"} {
		b, ok := ModeToByte(m)
		if !ok {
			t.Fatalf("ModeToByte(%q) missing", m)
		}
		back, ok := ModeFromByte(b)
		if !ok || back != m {
			t.Errorf("mode round-trip %q via %02x = %q,%v", m, b, back, ok)
		}
	}
	if _, ok := ModeToByte("data"); ok {
		t.Error("data mode has no set-mode byte — it is the separate 06 modifier")
	}
}

func TestSetModeFrame(t *testing.T) {
	f, err := CmdSetMode("usb")
	if err != nil {
		t.Fatal(err)
	}
	if got := hx(f); got != "fefea2e0060101fd" {
		t.Errorf("set mode usb = %s, want fefea2e0060101fd", got)
	}
}

// Data-mode on/off frames match the command table byte-for-byte
// (`06 <00/01> <filter>`; plan U3 scenario).
func TestSetDataModeFrames(t *testing.T) {
	if got := hx(CmdSetDataMode(true)); got != "fefea2e0060101fd" {
		t.Errorf("data on = %s, want fefea2e0060101fd", got)
	}
	if got := hx(CmdSetDataMode(false)); got != "fefea2e0060001fd" {
		t.Errorf("data off = %s, want fefea2e0060001fd", got)
	}
}

// RF output power carries `14 0A` — and NOTHING in the codec emits the
// out-of-scope main-power `18 0x` (plan U3 scenario; KTD-10).
func TestRFPowerFrameAndNoMainPower(t *testing.T) {
	f := CmdSetRFPower(128)
	if got := hx(f); got != "fefea2e0140a80fd" {
		t.Errorf("set power = %s, want fefea2e0140a80fd", got)
	}

	// Walk every builder; none may produce an 18 command byte.
	selMain, err := CmdSelectVFO("main")
	if err != nil {
		t.Fatal(err)
	}
	selSub, err := CmdSelectVFO("sub")
	if err != nil {
		t.Fatal(err)
	}
	setFreq, err := CmdSetFreq(432_100_000)
	if err != nil {
		t.Fatal(err)
	}
	setMode, err := CmdSetMode("fm")
	if err != nil {
		t.Fatal(err)
	}
	builders := map[string][]byte{
		"readFreq":     CmdReadFreq(),
		"readMode":     CmdReadMode(),
		"selMain":      selMain,
		"selSub":       selSub,
		"readSel":      CmdReadSelectedVFO(),
		"satOn":        CmdSatelliteMode(true),
		"satOff":       CmdSatelliteMode(false),
		"satRead":      CmdReadSatelliteMode(),
		"pttOn":        CmdPTT(true),
		"pttOff":       CmdPTT(false),
		"id":           CmdTransceiverID(),
		"smeter":       CmdReadSMeter(),
		"swr":          CmdReadSWR(),
		"alc":          CmdReadALC(),
		"preamp":       CmdReadPreamp(),
		"att":          CmdReadAttenuator(),
		"power":        CmdSetRFPower(255),
		"transceiveOn": CmdSetTransceive(true),
		"dataOn":       CmdSetDataMode(true),
		"dataOff":      CmdSetDataMode(false),
		"setFreq":      setFreq,
		"setMode":      setMode,
	}
	for name, f := range builders {
		if len(f) >= 5 && f[4] == 0x18 {
			t.Errorf("%s emitted the out-of-scope main-power command 18", name)
		}
	}
}

func TestVFOAndPTTFrames(t *testing.T) {
	selMain, err := CmdSelectVFO("main")
	if err != nil {
		t.Fatal(err)
	}
	if got := hx(selMain); got != "fefea2e007d0fd" {
		t.Errorf("select main = %s", got)
	}
	selSub, err := CmdSelectVFO("sub")
	if err != nil {
		t.Fatal(err)
	}
	if got := hx(selSub); got != "fefea2e007d1fd" {
		t.Errorf("select sub = %s", got)
	}
	if got := hx(CmdReadSelectedVFO()); got != "fefea2e007d200fd" {
		t.Errorf("read selected = %s", got)
	}
	if got := hx(CmdPTT(true)); got != "fefea2e01c0001fd" {
		t.Errorf("ptt on = %s", got)
	}
	if got := hx(CmdPTT(false)); got != "fefea2e01c0000fd" {
		t.Errorf("ptt off = %s", got)
	}
	vfo, err := ParseVFOReply([]byte{0x01})
	if err != nil || vfo != "sub" {
		t.Errorf("vfo reply 01 = %q,%v", vfo, err)
	}
}

func TestSatelliteFrames(t *testing.T) {
	if got := hx(CmdSatelliteMode(true)); got != "fefea2e0165a01fd" {
		t.Errorf("sat on = %s", got)
	}
	if got := hx(CmdReadSatelliteMode()); got != "fefea2e0165afd" {
		t.Errorf("sat read = %s", got)
	}
	on, err := ParseSatelliteReply([]byte{0x01})
	if err != nil || !on {
		t.Errorf("sat reply 01 = %v,%v", on, err)
	}
}

// NG reply -> typed rejection carrying the command (plan U3 scenario).
func TestNGTypedRejection(t *testing.T) {
	// Radio rejects a freq set (e.g. satellite mode): FE FE E0 A2 05 FA FD.
	f, err := ParseFrame([]byte{0xFE, 0xFE, 0xE0, 0xA2, 0x05, 0xFA, 0xFD})
	if err != nil {
		t.Fatal(err)
	}
	if !f.IsNG() {
		t.Fatal("frame must parse as NG")
	}
	ngErr := f.AsNG()
	var typed *NGError
	if !errors.As(ngErr, &typed) {
		t.Fatalf("AsNG() = %T, want *NGError", ngErr)
	}
	if typed.Cmd != 0x05 {
		t.Errorf("NG cmd = %02x, want 05", typed.Cmd)
	}
	if !strings.Contains(typed.Error(), "05") || !strings.Contains(typed.Error(), "NG") {
		t.Errorf("NG error text = %q", typed.Error())
	}
}

// Transceive broadcast parse: freq, mode, tx — plus the FB reply variant
// (plan U3 scenario).
func TestTransceiveParse(t *testing.T) {
	bcd, err := BCD10Encode(435_500_000)
	if err != nil {
		t.Fatal(err)
	}

	// Broadcast (FD) with the all-controllers destination: FE FE 00 A2 00
	// <bcd> FD — no VFO tag; attribution is the session layer's.
	raw := append([]byte{0xFE, 0xFE, 0x00, 0xA2, 0x00}, bcd...)
	raw = append(raw, 0xFD)
	f, err := ParseFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok, err := ParseTransceive(f)
	if err != nil || !ok {
		t.Fatalf("freq broadcast: ok=%v err=%v", ok, err)
	}
	if tr.Kind != "freq" || tr.FreqHz != 435_500_000 || tr.IsReply {
		t.Errorf("freq transceive = %+v", tr)
	}

	// Reply (FB) to our read freq: same command byte, IsReply set.
	raw = append([]byte{0xFE, 0xFE, 0xE0, 0xA2, 0x00}, bcd...)
	raw = append(raw, 0xFB, 0xFD)
	f, err = ParseFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok, err = ParseTransceive(f)
	if err != nil || !ok || !tr.IsReply || tr.FreqHz != 435_500_000 {
		t.Errorf("freq reply = %+v ok=%v err=%v", tr, ok, err)
	}

	// Mode broadcast.
	f, err = ParseFrame([]byte{0xFE, 0xFE, 0xE0, 0xA2, 0x01, 0x05, 0x01, 0xFD})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok, err = ParseTransceive(f)
	if err != nil || !ok || tr.Mode != "fm" || tr.Filter != 0x01 {
		t.Errorf("mode transceive = %+v ok=%v err=%v", tr, ok, err)
	}

	// TX broadcast: 1C 00 01 = tx on.
	f, err = ParseFrame([]byte{0xFE, 0xFE, 0xE0, 0xA2, 0x1C, 0x00, 0x01, 0xFD})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok, err = ParseTransceive(f)
	if err != nil || !ok || tr.Kind != "tx" || tr.TX != "tx" {
		t.Errorf("tx transceive = %+v ok=%v err=%v", tr, ok, err)
	}

	// DV mode byte: consumed, reported unsupported, never raw (R7).
	f, err = ParseFrame([]byte{0xFE, 0xFE, 0xE0, 0xA2, 0x01, 0x17, 0x01, 0xFD})
	if err != nil {
		t.Fatal(err)
	}
	_, ok, err = ParseTransceive(f)
	if ok || !errors.Is(err, ErrUnsupportedMode) {
		t.Errorf("DV transceive = ok=%v err=%v, want unsupported", ok, err)
	}

	// A bare ack (05 FB) is not a transceive event.
	f, err = ParseFrame([]byte{0xFE, 0xFE, 0xE0, 0xA2, 0x05, 0xFB, 0xFD})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ = ParseTransceive(f); ok {
		t.Error("bare ack misparsed as transceive")
	}
}

// A frame whose DATA contains an FD-like byte parses by the payload extent,
// never by scanning for FD (plan U3 scenario; KTD-10).
func TestEmbeddedFDInData(t *testing.T) {
	// A 5-byte freq "value" whose 4th byte is 0xFD. Invalid BCD on decode —
	// but the FRAME must parse with the full data intact, not truncate at
	// the embedded FD.
	raw := []byte{0xFE, 0xFE, 0xE0, 0xA2, 0x00, 0x11, 0x11, 0xFD, 0x41, 0x01, 0xFB, 0xFD}
	f, err := ParseFrame(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Cmd != 0x00 {
		t.Errorf("cmd = %02x", f.Cmd)
	}
	if len(f.Sub) != 5 || f.Sub[2] != 0xFD {
		t.Fatalf("data = % x, want the embedded FD preserved", f.Sub)
	}
	if f.Terminator != TerminatorOK {
		t.Errorf("terminator = %02x, want FB", f.Terminator)
	}
	// And the value is rejected on decode — garbage is an error, not a guess.
	if _, err := ParseFreqReply(f.Sub); err == nil {
		t.Error("non-BCD frequency data must fail to decode")
	}
}

// Per-VFO band validation (plan U3 scenario): SUB has no 23cm.
func TestValidateFreqPerVFO(t *testing.T) {
	cases := []struct {
		vfo  string
		hz   uint64
		want bool
	}{
		{"main", 144_500_000, true},
		{"main", 432_100_000, true},
		{"main", 1_296_100_000, true},
		{"sub", 144_500_000, true},
		{"sub", 432_100_000, true},
		{"sub", 1_296_100_000, false}, // SUB has no 23cm
		{"main", 14_000_000, false},   // HF is not this radio
		{"sub", 500_000_000, false},   // outside every window
	}
	for _, c := range cases {
		err := ValidateFreq(c.vfo, c.hz)
		if (err == nil) != c.want {
			t.Errorf("ValidateFreq(%s, %d) = %v, want ok=%v", c.vfo, c.hz, err, c.want)
		}
	}
	// The rejection names the fact (the /state.error taxonomy carries it).
	if err := ValidateFreq("sub", 1_296_100_000); !strings.Contains(err.Error(), "sub") {
		t.Errorf("SUB-23cm rejection text = %q", err)
	}
	// Band labels.
	if b, _ := BandForFreq(435_500_000); b != "70cm" {
		t.Errorf("band(435.5M) = %q, want 70cm", b)
	}
}

// Frame parse strictness: garbage is an error, never a panic.
func TestParseFrameStrict(t *testing.T) {
	for _, raw := range [][]byte{
		{0xFE},                                     // too short
		{0x01, 0x02, 0x03, 0x04, 0x05, 0x06},       // no preamble
		{0xFE, 0xFE, 0xA2, 0xE0, 0x03, 0x00},       // no FD end
		{0xFE, 0xFE, 0x12, 0x34, 0x03, 0xFB, 0xFD}, // wrong addressing
	} {
		if _, err := ParseFrame(raw); err == nil {
			t.Errorf("ParseFrame(% x) must fail", raw)
		}
	}
	// Minimum valid frame: bare read with a bare ack.
	if _, err := ParseFrame([]byte{0xFE, 0xFE, 0xE0, 0xA2, 0x19, 0xFB, 0xFD}); err != nil {
		t.Errorf("ID ack parse: %v", err)
	}
}

// Meter replies are four BCD digits "0000".."0255": the S9 reading `01 20`
// is 120, not 0x0120 = 288.
func TestParseMeterBCD(t *testing.T) {
	cases := []struct {
		in   []byte
		want int
	}{
		{[]byte{0x00, 0x00}, 0},
		{[]byte{0x00, 0x42}, 42},
		{[]byte{0x01, 0x20}, 120},
		{[]byte{0x02, 0x41}, 241},
		{[]byte{0x02, 0x55}, 255},
		{[]byte{0x78}, 0x78}, // fake's single-byte legacy form: plain value
	}
	for _, c := range cases {
		got, err := ParseMeter(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseMeter(% x) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	if _, err := ParseMeter([]byte{0x01, 0x2a}); err == nil {
		t.Error("ParseMeter accepted a non-BCD nibble")
	}
}
