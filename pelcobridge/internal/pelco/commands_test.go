package pelco

import (
	"bytes"
	"testing"
)

// TestResponseBuildersRoundTrip verifies the simulator's response builders
// produce frames the existing response predicates recognize, with the angle
// carried through Word() correctly.
func TestResponseBuildersRoundTrip(t *testing.T) {
	pr := PanResponse(1, 180)
	if !pr.IsPanResponse() {
		t.Fatalf("PanResponse not recognized as a pan response: %+v", pr)
	}
	if got, want := HundredthsToDeg(pr.Word()), 180.0; got != want {
		t.Fatalf("pan word = %.2f, want %.2f", got, want)
	}

	tr := TiltResponse(1, 45)
	if !tr.IsTiltResponse() {
		t.Fatalf("TiltResponse not recognized as a tilt response: %+v", tr)
	}
	if got, want := HundredthsToDeg(tr.Word()), 45.0; got != want {
		t.Fatalf("tilt word = %.2f, want %.2f", got, want)
	}
}

// TestCommandClassification verifies the set/query/jog predicates discriminate
// the extended commands the engine emits, so the simulator can dispatch on them.
func TestCommandClassification(t *testing.T) {
	cases := []struct {
		name    string
		f       Frame
		setPan  bool
		setTilt bool
		qPan    bool
		qTilt   bool
		jog     bool
	}{
		{"SetPan", SetPan(1, 180), true, false, false, false, false},
		{"SetTilt", SetTilt(1, 45), false, true, false, false, false},
		{"QueryPan", QueryPan(1), false, false, true, false, false},
		{"QueryTilt", QueryTilt(1), false, false, false, true, false},
		{"JogRight", Jog(1, Direction{Pan: 1}.Cmd2(), 0x20, 0), false, false, false, false, true},
		{"Stop", Stop(1), false, false, false, false, false},
		// Preset opcodes must NOT be misclassified as jogs even though they
		// share the low direction bits (0x03=0x02|0x01, 0x07=0x06|0x01).
		{"SetPreset", SetPreset(1, 105), false, false, false, false, false},
		{"GoPreset", GoToPreset(1, 105), false, false, false, false, false},
		{"ClearPreset", ClearPreset(1, 105), false, false, false, false, false},
		{"DisableSelfCheck", DisableSelfCheck(1), false, false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.f.IsSetPan() != c.setPan {
				t.Errorf("IsSetPan = %v, want %v", c.f.IsSetPan(), c.setPan)
			}
			if c.f.IsSetTilt() != c.setTilt {
				t.Errorf("IsSetTilt = %v, want %v", c.f.IsSetTilt(), c.setTilt)
			}
			if c.f.IsQueryPan() != c.qPan {
				t.Errorf("IsQueryPan = %v, want %v", c.f.IsQueryPan(), c.qPan)
			}
			if c.f.IsQueryTilt() != c.qTilt {
				t.Errorf("IsQueryTilt = %v, want %v", c.f.IsQueryTilt(), c.qTilt)
			}
			if c.f.IsJog() != c.jog {
				t.Errorf("IsJog = %v, want %v", c.f.IsJog(), c.jog)
			}
		})
	}
}

// TestJogDirDecoding verifies JogDir extracts pan/tilt direction from CMD2.
func TestJogDirDecoding(t *testing.T) {
	cases := []struct {
		d       Direction
		wantPan int
		wantTlt int
	}{
		{Direction{Pan: 1}, 1, 0},            // right
		{Direction{Pan: -1}, -1, 0},          // left
		{Direction{Tilt: 1}, 0, 1},           // up
		{Direction{Tilt: -1}, 0, -1},         // down
		{Direction{Pan: 1, Tilt: -1}, 1, -1}, // right + down (diagonal)
	}
	for _, c := range cases {
		f := Jog(1, c.d.Cmd2(), 0x20, 0x20)
		pan, tilt := f.JogDir()
		if pan != c.wantPan || tilt != c.wantTlt {
			t.Errorf("Jog(%+v) → pan=%d tilt=%d; want pan=%d tilt=%d", c.d, pan, tilt, c.wantPan, c.wantTlt)
		}
	}
}

// TestPresetWireBytes verifies the preset/self-check builders produce exactly
// the frames documented in the 303Z/3050DZ serial-command manual (Pelco-D,
// address 0x01):
//
//	disable self-check (set preset 105)   = FF 01 00 03 00 69 6D
//	enable  self-check (go-to preset 105) = FF 01 00 07 00 69 71
//	factory reset + self-test (call 125) = FF 01 00 07 00 7D 85
//
// Checksum = (ADDR+CMD1+CMD2+DATA1+DATA2) mod 256; preset number in DATA2,
// DATA1 == 0 (unlike the big-endian position-word commands).
func TestPresetWireBytes(t *testing.T) {
	cases := []struct {
		name string
		f    Frame
		want []byte
	}{
		{"DisableSelfCheck", DisableSelfCheck(1), []byte{0xFF, 0x01, 0x00, 0x03, 0x00, 0x69, 0x6D}},
		{"EnableSelfCheck", EnableSelfCheck(1), []byte{0xFF, 0x01, 0x00, 0x07, 0x00, 0x69, 0x71}},
		{"FactoryReset", GoToPreset(1, FactoryResetPreset), []byte{0xFF, 0x01, 0x00, 0x07, 0x00, 0x7D, 0x85}},
		{"SetPreset8", SetPreset(1, 8), []byte{0xFF, 0x01, 0x00, 0x03, 0x00, 0x08, 0x0C}},
		{"ClearPreset8", ClearPreset(1, 8), []byte{0xFF, 0x01, 0x00, 0x05, 0x00, 0x08, 0x0E}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.f.Bytes(); !bytes.Equal(got, c.want) {
				t.Fatalf("%s bytes = % X, want % X", c.name, got, c.want)
			}
		})
	}
}

// TestPresetPredicates verifies the preset predicates discriminate the three
// preset opcodes (0x03/0x05/0x07) and reject the position opcodes.
func TestPresetPredicates(t *testing.T) {
	if !SetPreset(1, 5).IsSetPreset() {
		t.Error("SetPreset not recognized by IsSetPreset")
	}
	if !GoToPreset(1, 5).IsGoPreset() {
		t.Error("GoToPreset not recognized by IsGoPreset")
	}
	if !ClearPreset(1, 5).IsClearPreset() {
		t.Error("ClearPreset not recognized by IsClearPreset")
	}
	// Cross-discrimination: a set is not a go-to, etc.
	if SetPreset(1, 5).IsGoPreset() || SetPreset(1, 5).IsClearPreset() {
		t.Error("SetPreset misclassified as go/clear")
	}
	if GoToPreset(1, 5).IsSetPreset() || GoToPreset(1, 5).IsClearPreset() {
		t.Error("GoToPreset misclassified as set/clear")
	}
	// Position commands are not presets.
	if SetPan(1, 0).IsSetPreset() || QueryPan(1).IsGoPreset() {
		t.Error("position command misclassified as a preset")
	}
}
