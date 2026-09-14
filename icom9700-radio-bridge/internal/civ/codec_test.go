package civ

// Codec unit tests: BCD frequency codec, mode map, band validation, the
// byte-pinned frame builders and the strict reply parser — all without the
// network. The transport-level integration (build -> SendCIV -> fake radio
// -> parse) lives in civ_test.go.

import (
	"bytes"
	"errors"
	"math/rand"
	"strings"
	"testing"
)

// radioFrame builds a frame in the radio->controller direction the way the
// IC-9700 sends it (dest = controller E0, source = radio A2) — the shape
// Parse consumes and the fake radio's scripted replies use.
func radioFrame(cmd byte, rest ...byte) []byte {
	f := make([]byte, 0, 6+len(rest))
	f = append(f, Preamble, Preamble, AddrCtrl, AddrRadio, cmd)
	f = append(f, rest...)
	return append(f, Postamble)
}

func TestBCDFreqEncodeDecodeRoundTrip(t *testing.T) {
	// Exact wire bytes for the plan's three round-trip frequencies. Note
	// 144.500.000 encodes to 00 00 50 44 01 — the U1 brief's printed
	// example (00 00 50 41 01) is a transcription typo that decodes to
	// 141.500.000 (see the brief's U3 codec pin).
	cases := []struct {
		hz    uint32
		bytes [5]byte
	}{
		{144_500_000, [5]byte{0x00, 0x00, 0x50, 0x44, 0x01}},
		{432_100_000, [5]byte{0x00, 0x00, 0x10, 0x32, 0x04}},
		{1_296_100_000, [5]byte{0x00, 0x00, 0x10, 0x96, 0x12}},
	}
	for _, tc := range cases {
		got, err := encodeBCDFreq(tc.hz)
		if err != nil {
			t.Fatalf("encodeBCDFreq(%d): %v", tc.hz, err)
		}
		if got != tc.bytes {
			t.Errorf("encodeBCDFreq(%d) = % x, want % x", tc.hz, got, tc.bytes)
		}
		back, err := decodeBCDFreq(tc.bytes[:])
		if err != nil {
			t.Fatalf("decodeBCDFreq(% x): %v", tc.bytes, err)
		}
		if back != tc.hz {
			t.Errorf("decodeBCDFreq(% x) = %d, want %d", tc.bytes, back, tc.hz)
		}
	}

	// Band edges round-trip too (inclusive bounds of the canonical table).
	for _, hz := range []uint32{144_000_000, 146_000_000, 430_000_000, 440_000_000, 1_240_000_000, 1_300_000_000} {
		b, err := encodeBCDFreq(hz)
		if err != nil {
			t.Fatalf("encodeBCDFreq(%d): %v", hz, err)
		}
		if back, err := decodeBCDFreq(b[:]); err != nil || back != hz {
			t.Errorf("band edge %d: round trip gave %d, %v", hz, back, err)
		}
	}

	// The radio tunes in 10 Hz steps; anything finer is not representable.
	if _, err := encodeBCDFreq(144_500_005); !errors.Is(err, ErrBadLevel) {
		t.Errorf("encodeBCDFreq(144_500_005) err = %v, want ErrBadLevel", err)
	}
	// A non-decimal nibble is corruption the datalen framing cannot exclude.
	if _, err := decodeBCDFreq([]byte{0x00, 0x00, 0xfa, 0x44, 0x01}); !errors.Is(err, ErrFrameMalformed) {
		t.Errorf("decodeBCDFreq with nibble 0xa err = %v, want ErrFrameMalformed", err)
	}
}

func TestModeByteToCanonicalMap(t *testing.T) {
	// Every operating-mode byte the radio can report, with the canonical
	// bus mode (R7). CW-R collapses onto cw; RTTY (both filters), DV and DD
	// are recognized but NEVER published raw — ok=false means the consumer
	// omits the mode field.
	cases := []struct {
		b    byte
		mode string
		ok   bool
	}{
		{ModeByteLSB, ModeLSB, true},
		{ModeByteUSB, ModeUSB, true},
		{ModeByteAM, ModeAM, true},
		{ModeByteCW, ModeCW, true},
		{ModeByteFM, ModeFM, true},
		{ModeByteCWR, ModeCW, true}, // reverse-filter bit not representable
		{ModeByteRTTY, "", false},
		{ModeByteRTTYR, "", false},
		{ModeByteDV, "", false},
		{ModeByteDD, "", false},
		{0x99, "", false}, // unknown bytes fail closed
	}
	for _, tc := range cases {
		mode, ok := modeCanonical(tc.b)
		if mode != tc.mode || ok != tc.ok {
			t.Errorf("modeCanonical(0x%02x) = (%q, %v), want (%q, %v)", tc.b, mode, ok, tc.mode, tc.ok)
		}
	}
}

func TestCanonicalModeToByte(t *testing.T) {
	cases := []struct {
		mode string
		b    byte
		ok   bool
	}{
		{ModeLSB, ModeByteLSB, true},
		{ModeUSB, ModeByteUSB, true},
		{ModeAM, ModeByteAM, true},
		{ModeCW, ModeByteCW, true},
		{ModeFM, ModeByteFM, true},
		{ModeData, 0, false}, // data mode is the 1A 06 modifier, not a mode byte
		{"rtty", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		b, ok := modeByte(tc.mode)
		if b != tc.b || ok != tc.ok {
			t.Errorf("modeByte(%q) = (0x%02x, %v), want (0x%02x, %v)", tc.mode, b, ok, tc.b, tc.ok)
		}
	}
}

func TestSetModeFrameBytes(t *testing.T) {
	c := NewCodec()
	cases := []struct {
		mode   string
		filter byte
		want   []byte
	}{
		{ModeUSB, Filter1, []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdSetMode, ModeByteUSB, Filter1, Postamble}},
		{ModeCW, Filter2, []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdSetMode, ModeByteCW, Filter2, Postamble}},
		{ModeLSB, FilterOmit, []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdSetMode, ModeByteLSB, Postamble}},
	}
	for _, tc := range cases {
		got, err := c.BuildSetMode(tc.mode, tc.filter)
		if err != nil {
			t.Fatalf("BuildSetMode(%q, %d): %v", tc.mode, tc.filter, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("BuildSetMode(%q, %d) = % x, want % x", tc.mode, tc.filter, got, tc.want)
		}
	}
	if _, err := c.BuildSetMode(ModeData, Filter1); !errors.Is(err, ErrBadModeName) {
		t.Errorf("BuildSetMode(data) err = %v, want ErrBadModeName", err)
	}
	if _, err := c.BuildSetMode(ModeFM, 0x04); !errors.Is(err, ErrBadFilter) {
		t.Errorf("BuildSetMode(fm, 4) err = %v, want ErrBadFilter", err)
	}
}

func TestDataModeFrameBytes(t *testing.T) {
	// Official table "DATA mode with filter set" (1A 06): 01=on with
	// FIL1-FIL3; off is always 00 00 ("when 00 is set, also set 00 to w").
	// The U1 brief's `06 <00/01> <filter>` modifier is a transcription
	// error — 06 00/01 are the LSB/USB operating-mode bytes.
	c := NewCodec()
	cases := []struct {
		on     bool
		filter byte
		want   []byte
	}{
		{true, Filter1, []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdSetItem, SetItemDataMode, 0x01, Filter1, Postamble}},
		{true, Filter3, []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdSetItem, SetItemDataMode, 0x01, Filter3, Postamble}},
		{false, Filter1, []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdSetItem, SetItemDataMode, 0x00, 0x00, Postamble}},
		{false, FilterOmit, []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdSetItem, SetItemDataMode, 0x00, 0x00, Postamble}},
	}
	for _, tc := range cases {
		got, err := c.BuildSetDataMode(tc.on, tc.filter)
		if err != nil {
			t.Fatalf("BuildSetDataMode(%v, %d): %v", tc.on, tc.filter, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("BuildSetDataMode(%v, %d) = % x, want % x", tc.on, tc.filter, got, tc.want)
		}
	}
	if _, err := c.BuildSetDataMode(true, FilterOmit); !errors.Is(err, ErrBadFilter) {
		t.Errorf("BuildSetDataMode(true, 0) err = %v, want ErrBadFilter", err)
	}
	// Read form: 1A 06 with no data area.
	if got := c.BuildReadDataMode(); !bytes.Equal(got, []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdSetItem, SetItemDataMode, Postamble}) {
		t.Errorf("BuildReadDataMode = % x", got)
	}
}

func TestBuildSetFreqValidatesPerVFO(t *testing.T) {
	c := NewCodec()

	// MAIN carries all three canonical bands.
	for _, hz := range []uint32{144_500_000, 432_100_000, 1_296_100_000} {
		if err := ValidateFreq(VfoMain, hz); err != nil {
			t.Errorf("ValidateFreq(main, %d): %v", hz, err)
		}
	}
	// SUB has no 23cm — the plan's SUB-VFO 23cm set must be a rejection
	// before any frame exists.
	f, err := c.BuildSetFreq(VfoSub, 1_296_100_000)
	if f != nil {
		t.Errorf("SUB 23cm set produced a frame: % x", f)
	}
	if !errors.Is(err, ErrBandNotOnSub) {
		t.Errorf("SUB 23cm set err = %v, want ErrBandNotOnSub", err)
	}
	// SUB carries 2m/70cm.
	for _, hz := range []uint32{144_500_000, 432_100_000} {
		if err := ValidateFreq(VfoSub, hz); err != nil {
			t.Errorf("ValidateFreq(sub, %d): %v", hz, err)
		}
	}
	// Out-of-table frequencies are rejected for both VFOs (R7: bands derive
	// from the canonical table).
	for _, v := range []VFO{VfoMain, VfoSub} {
		for _, hz := range []uint32{143_999_999, 146_000_001, 429_999_999, 440_000_001, 1_239_999_999, 1_300_000_001, 7_100_000} {
			if err := ValidateFreq(v, hz); !errors.Is(err, ErrFreqRange) {
				t.Errorf("ValidateFreq(%s, %d) err = %v, want ErrFreqRange", v, hz, err)
			}
		}
	}
	// A valid MAIN set carries the exact BCD bytes after the command byte.
	f, err = c.BuildSetFreq(VfoMain, 1_296_100_000)
	if err != nil {
		t.Fatalf("BuildSetFreq(main, 23cm): %v", err)
	}
	want := []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdSetFreq, 0x00, 0x00, 0x10, 0x96, 0x12, Postamble}
	if !bytes.Equal(f, want) {
		t.Errorf("BuildSetFreq(main, 1296.1M) = % x, want % x", f, want)
	}
}

func TestRFPowerFrameBytes(t *testing.T) {
	c := NewCodec()
	cases := []struct {
		level int
		want  []byte
	}{
		{0, []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdRFPower, PowerRF, 0x00, Postamble}},
		{128, []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdRFPower, PowerRF, 0x80, Postamble}},
		{255, []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdRFPower, PowerRF, 0xff, Postamble}},
	}
	for _, tc := range cases {
		got, err := c.BuildSetPower(tc.level)
		if err != nil {
			t.Fatalf("BuildSetPower(%d): %v", tc.level, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("BuildSetPower(%d) = % x, want % x", tc.level, got, tc.want)
		}
	}
	for _, level := range []int{-1, 256} {
		if _, err := c.BuildSetPower(level); !errors.Is(err, ErrBadLevel) {
			t.Errorf("BuildSetPower(%d) err = %v, want ErrBadLevel", level, err)
		}
	}
}

// allBuilders maps one named builder per command-table entry so the
// LAN-power assertion below walks every frame the codec can emit.
func allBuilders(c *Codec) map[string]func() ([]byte, error) {
	return map[string]func() ([]byte, error){
		"read_freq":              func() ([]byte, error) { return c.BuildReadFreq(), nil },
		"set_freq_main":          func() ([]byte, error) { return c.BuildSetFreq(VfoMain, 144_500_000) },
		"set_freq_sub":           func() ([]byte, error) { return c.BuildSetFreq(VfoSub, 432_100_000) },
		"select_main":            func() ([]byte, error) { return c.BuildSelectVFO(VfoMain), nil },
		"select_sub":             func() ([]byte, error) { return c.BuildSelectVFO(VfoSub), nil },
		"read_selected":          func() ([]byte, error) { return c.BuildReadSelectedVFO(), nil },
		"read_mode":              func() ([]byte, error) { return c.BuildReadMode(), nil },
		"set_mode":               func() ([]byte, error) { return c.BuildSetMode(ModeUSB, Filter1) },
		"data_mode_on":           func() ([]byte, error) { return c.BuildSetDataMode(true, Filter1) },
		"data_mode_off":          func() ([]byte, error) { return c.BuildSetDataMode(false, 0) },
		"read_data_mode":         func() ([]byte, error) { return c.BuildReadDataMode(), nil },
		"sat_on":                 func() ([]byte, error) { return c.BuildSetSatellite(true), nil },
		"sat_off":                func() ([]byte, error) { return c.BuildSetSatellite(false), nil },
		"read_sat":               func() ([]byte, error) { return c.BuildReadSatellite(), nil },
		"ptt_on":                 func() ([]byte, error) { return c.BuildPTT(true), nil },
		"ptt_off":                func() ([]byte, error) { return c.BuildPTT(false), nil },
		"read_ptt":               func() ([]byte, error) { return c.BuildReadPTT(), nil },
		"read_id":                func() ([]byte, error) { return c.BuildReadID(), nil },
		"read_s_meter":           func() ([]byte, error) { return c.BuildReadMeter(MeterS), nil },
		"read_swr":               func() ([]byte, error) { return c.BuildReadMeter(MeterSWR), nil },
		"read_alc":               func() ([]byte, error) { return c.BuildReadMeter(MeterALC), nil },
		"read_comp":              func() ([]byte, error) { return c.BuildReadMeter(MeterCOMP), nil },
		"read_preamp":            func() ([]byte, error) { return c.BuildReadPreamp(), nil },
		"set_preamp":             func() ([]byte, error) { return c.BuildSetPreamp(0x01) },
		"att_on":                 func() ([]byte, error) { return c.BuildSetAttenuator(true), nil },
		"att_off":                func() ([]byte, error) { return c.BuildSetAttenuator(false), nil },
		"read_att":               func() ([]byte, error) { return c.BuildReadAttenuator(), nil },
		"set_power":              func() ([]byte, error) { return c.BuildSetPower(0x18) }, // data byte 0x18 on purpose
		"set_civ_transceive_on":  func() ([]byte, error) { return c.BuildSetCIVTransceive(true), nil },
		"set_civ_transceive_off": func() ([]byte, error) { return c.BuildSetCIVTransceive(false), nil },
	}
}

func TestNoBuilderEmitsLANPowerFrame(t *testing.T) {
	// KTD-10: the LAN main-power commands (18 01 on, 18 00 off) are OUT OF
	// SCOPE and never emitted. Dedicated assertion over EVERY builder: no
	// 0x18 command byte, and no embedded 18 00/18 01 pair anywhere — even
	// when a data byte legitimately is 0x18 (set_power above).
	c := NewCodec()
	for name, build := range allBuilders(c) {
		f, err := build()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if f[4] == 0x18 {
			t.Errorf("%s emits command byte 0x18 (LAN power): % x", name, f)
		}
		for i := 0; i+1 < len(f); i++ {
			if f[i] == 0x18 && (f[i+1] == 0x00 || f[i+1] == 0x01) {
				t.Errorf("%s carries an embedded 18 %02x pair: % x", name, f[i+1], f)
			}
		}
	}
}

func TestPreampAttenuatorFramingAndBandScoping(t *testing.T) {
	c := NewCodec()

	// Preamp: 16 02 carries the documented 0-3 value and no band byte (the
	// official table has no band-selector form — per-band scoping is the
	// caller's 07 D0/D1 job and a bench pin, see the brief's U3 pin).
	got, err := c.BuildSetPreamp(0x02)
	if err != nil {
		t.Fatalf("BuildSetPreamp(2): %v", err)
	}
	want := []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdFunction, FuncPreamp, 0x02, Postamble}
	if !bytes.Equal(got, want) {
		t.Errorf("BuildSetPreamp(2) = % x, want % x", got, want)
	}
	if _, err := c.BuildSetPreamp(0x04); !errors.Is(err, ErrBadLevel) {
		t.Errorf("BuildSetPreamp(4) err = %v, want ErrBadLevel", err)
	}
	readPreamp := []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdFunction, FuncPreamp, Postamble}
	if got := c.BuildReadPreamp(); !bytes.Equal(got, readPreamp) {
		t.Errorf("BuildReadPreamp = % x, want % x", got, readPreamp)
	}

	// Attenuator: officially command 11 with the state in the sub command
	// (11 00 off / 11 10 = 10 dB on).
	attOn := []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdAttenuator, Attenuator10, Postamble}
	attOff := []byte{Preamble, Preamble, AddrRadio, AddrCtrl, CmdAttenuator, AttenuatorOff, Postamble}
	if got := c.BuildSetAttenuator(true); !bytes.Equal(got, attOn) {
		t.Errorf("BuildSetAttenuator(true) = % x, want % x", got, attOn)
	}
	if got := c.BuildSetAttenuator(false); !bytes.Equal(got, attOff) {
		t.Errorf("BuildSetAttenuator(false) = % x, want % x", got, attOff)
	}
	if got := c.BuildReadAttenuator(); !bytes.Equal(got, attOff) {
		t.Errorf("BuildReadAttenuator = % x, want the 11 00 probe % x", got, attOff)
	}

	// The per-band get/set pattern R5's per-VFO /state needs: select the
	// band first, then read/set — asserted as a byte sequence.
	seq := [][]byte{
		c.BuildSelectVFO(VfoSub),
		c.BuildSetAttenuator(true),
		c.BuildSelectVFO(VfoMain),
		c.BuildReadPreamp(),
	}
	wantSeq := [][]byte{
		{Preamble, Preamble, AddrRadio, AddrCtrl, CmdSelectBand, BandSelSub, Postamble},
		attOn,
		{Preamble, Preamble, AddrRadio, AddrCtrl, CmdSelectBand, BandSelMain, Postamble},
		readPreamp,
	}
	for i := range seq {
		if !bytes.Equal(seq[i], wantSeq[i]) {
			t.Errorf("scoped frame %d = % x, want % x", i, seq[i], wantSeq[i])
		}
	}

	// Attenuator state parses from the sub command; the read probe (11 00)
	// is byte-identical to set-off — the ambiguity is a documented bench
	// pin, and the parser treats both as state reports.
	parseAtt := func(sub byte) (bool, error) {
		r, err := c.Parse(radioFrame(CmdAttenuator, sub))
		if err != nil {
			t.Fatalf("parse attenuator reply 0x%02x: %v", sub, err)
		}
		return r.Attenuator()
	}
	if on, err := parseAtt(Attenuator10); err != nil || !on {
		t.Errorf("attenuator sub 10 = (%v, %v)", on, err)
	}
	if off, err := parseAtt(AttenuatorOff); err != nil || off {
		t.Errorf("attenuator sub 00 = (%v, %v)", off, err)
	}
}

func TestParseAckAndTypedNGRejection(t *testing.T) {
	c := NewCodec()

	r, err := c.Parse(radioFrame(ReplyOK))
	if err != nil || r.Kind != KindAck {
		t.Errorf("FB parse = (%+v, %v), want KindAck, nil", r, err)
	}

	r, err = c.Parse(radioFrame(ReplyNG))
	if r.Kind != KindNG {
		t.Errorf("FA kind = %v, want KindNG", r.Kind)
	}
	if !errors.Is(err, ErrNG) {
		t.Errorf("FA err = %v, want ErrNG", err)
	}
	ng, ok := AsNG(err)
	if !ok {
		t.Fatalf("AsNG(FA) did not recover the typed rejection: %v", err)
	}
	// The FA frame carries no command echo — attach the pending command at
	// correlation time and render it.
	msg := ng.WithCommand(CmdSetFreq).Error()
	if !strings.Contains(msg, "05") || !strings.Contains(msg, "NG") {
		t.Errorf("NG message %q lacks the command identity", msg)
	}
	msg = ng.WithCommand(CmdFunction, FuncSatellite).Error()
	if !strings.Contains(msg, "16 5a") {
		t.Errorf("NG message %q lacks the sub-command identity", msg)
	}
	if !errors.Is(err, ErrNG) {
		t.Error("attached NG error no longer satisfies errors.Is(err, ErrNG)")
	}
}

func TestParseReplyDispatcher(t *testing.T) {
	c := NewCodec()
	bcd := func(hz uint32) []byte {
		b, err := encodeBCDFreq(hz)
		if err != nil {
			t.Fatalf("bcd(%d): %v", hz, err)
		}
		return b[:]
	}

	t.Run("freq reply", func(t *testing.T) {
		r, err := c.Parse(radioFrame(CmdReadFreq, bcd(432_100_000)...))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if hz, err := r.FreqHz(); err != nil || hz != 432_100_000 {
			t.Errorf("FreqHz = (%d, %v)", hz, err)
		}
	})

	t.Run("mode replies with and without filter", func(t *testing.T) {
		r, err := c.Parse(radioFrame(CmdReadMode, ModeByteFM, Filter1))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		mode, filter, ok, err := r.Mode()
		if err != nil || mode != ModeFM || filter != Filter1 || !ok {
			t.Errorf("Mode = (%q, %d, %v, %v)", mode, filter, ok, err)
		}
		// Mode transceive may omit the filter byte (official table note).
		r, err = c.Parse(radioFrame(CmdModeTransceive, ModeByteCW))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		mode, filter, ok, err = r.Mode()
		if err != nil || mode != ModeCW || filter != 0 || !ok {
			t.Errorf("bare Mode = (%q, %d, %v, %v)", mode, filter, ok, err)
		}
		// Never-published bytes parse but report ok=false (R7).
		for _, b := range []byte{ModeByteRTTY, ModeByteRTTYR, ModeByteDV, ModeByteDD} {
			r, err := c.Parse(radioFrame(CmdModeTransceive, b))
			if err != nil {
				t.Fatalf("parse mode byte 0x%02x: %v", b, err)
			}
			mode, _, ok, err := r.Mode()
			if err != nil {
				t.Fatalf("Mode(0x%02x): %v", b, err)
			}
			if ok || mode != "" {
				t.Errorf("mode byte 0x%02x published as (%q, %v); want omitted", b, mode, ok)
			}
		}
	})

	t.Run("selected vfo", func(t *testing.T) {
		r, _ := c.Parse(radioFrame(CmdSelectBand, BandSelRead, 0x00))
		if v, err := r.SelectedVFO(); err != nil || v != VfoMain {
			t.Errorf("SelectedVFO(00) = (%s, %v)", v, err)
		}
		r, _ = c.Parse(radioFrame(CmdSelectBand, BandSelRead, 0x01))
		if v, err := r.SelectedVFO(); err != nil || v != VfoSub {
			t.Errorf("SelectedVFO(01) = (%s, %v)", v, err)
		}
	})

	t.Run("satellite and PTT on/off", func(t *testing.T) {
		r, err := c.Parse(radioFrame(CmdFunction, FuncSatellite, 0x01))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if on, err := r.OnOff(); err != nil || !on {
			t.Errorf("satellite 01 = (%v, %v)", on, err)
		}
		r, _ = c.Parse(radioFrame(CmdStatus, StatusSubPTT, 0x00))
		if on, err := r.OnOff(); err != nil || on {
			t.Errorf("ptt 00 = (%v, %v)", on, err)
		}
	})

	t.Run("transceiver ID in both documented shapes", func(t *testing.T) {
		// Official table form: 19 00 <id>.
		r, err := c.Parse(radioFrame(CmdTransceiverID, 0x00, AddrRadio))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if id, err := r.RadioID(); err != nil || id != AddrRadio {
			t.Errorf("RadioID(19 00 A2) = (0x%02x, %v)", id, err)
		}
		// Bare legacy form: 19 <id>.
		r, err = c.Parse(radioFrame(CmdTransceiverID, AddrRadio))
		if err != nil {
			t.Fatalf("parse bare: %v", err)
		}
		if id, err := r.RadioID(); err != nil || id != AddrRadio {
			t.Errorf("RadioID(19 A2) = (0x%02x, %v)", id, err)
		}
	})

	t.Run("meters by datalen not FD scanning", func(t *testing.T) {
		// S9 = 120.
		r, err := c.Parse(radioFrame(CmdReadMeter, MeterSubS, 120))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if v, err := r.MeterLevel(); err != nil || v != 120 {
			t.Errorf("S-meter = (%d, %v)", v, err)
		}
		// A meter value of 253 IS byte 0xFD — a frame containing an
		// FD-like byte inside its payload must parse by the datalen framing
		// (never first-FD scanning), delivering the full value byte.
		r, err = c.Parse(radioFrame(CmdReadMeter, MeterSubS, 0xfd))
		if err != nil {
			t.Fatalf("parse embedded-FD frame: %v", err)
		}
		if v, err := r.MeterLevel(); err != nil || v != 253 {
			t.Errorf("S-meter 0xfd = (%d, %v), want 253", v, err)
		}
	})

	t.Run("power, preamp, data mode", func(t *testing.T) {
		r, _ := c.Parse(radioFrame(CmdRFPower, PowerRF, 180))
		if v, err := r.PowerLevel(); err != nil || v != 180 {
			t.Errorf("PowerLevel = (%d, %v)", v, err)
		}
		r, _ = c.Parse(radioFrame(CmdFunction, FuncPreamp, 0x03))
		if v, err := r.PreampLevel(); err != nil || v != 3 {
			t.Errorf("PreampLevel = (%d, %v)", v, err)
		}
		r, err := c.Parse(radioFrame(CmdFunction, FuncPreamp, 0x04))
		if err != nil {
			t.Fatalf("parse preamp 4: %v", err)
		}
		if _, err := r.PreampLevel(); !errors.Is(err, ErrFrameMalformed) {
			t.Errorf("PreampLevel(4) err = %v, want ErrFrameMalformed", err)
		}
		r, err = c.Parse(radioFrame(CmdSetItem, SetItemDataMode, 0x01, Filter2))
		if err != nil {
			t.Fatalf("parse data mode: %v", err)
		}
		on, filter, err := r.DataMode()
		if err != nil || !on || filter != Filter2 {
			t.Errorf("DataMode(01 02) = (%v, %d, %v)", on, filter, err)
		}
	})

	t.Run("transceive events", func(t *testing.T) {
		// Frequency transceive: the band derives from the canonical table.
		r, _ := c.Parse(radioFrame(CmdFreqTransceive, bcd(1_296_100_000)...))
		ev, err := ParseTransceive(r)
		if err != nil || ev.Kind != TransceiveFreq || ev.FreqHz != 1_296_100_000 || ev.Band != "23cm" || !ev.BandOK {
			t.Errorf("freq transceive = (%+v, %v)", ev, err)
		}
		// A frequency outside the table still parses; the band is marked
		// absent for the consumer to skip.
		r, _ = c.Parse(radioFrame(CmdFreqTransceive, bcd(50_100_000)...))
		ev, err = ParseTransceive(r)
		if err != nil || ev.BandOK || ev.Band != "" {
			t.Errorf("out-of-table transceive = (%+v, %v), want BandOK=false", ev, err)
		}
		// Mode transceive with a never-published byte stays an event with
		// ModeOK=false (R7: omitted, never raw).
		r, _ = c.Parse(radioFrame(CmdModeTransceive, ModeByteDV))
		ev, err = ParseTransceive(r)
		if err != nil || ev.Kind != TransceiveMode || ev.ModeOK || ev.Mode != "" {
			t.Errorf("DV transceive = (%+v, %v)", ev, err)
		}
		// PTT transceive.
		r, _ = c.Parse(radioFrame(CmdStatus, StatusSubPTT, 0x01))
		ev, err = ParseTransceive(r)
		if err != nil || ev.Kind != TransceivePTT || !ev.PTTOn {
			t.Errorf("ptt transceive = (%+v, %v)", ev, err)
		}
		// Non-transceive frames are refused as events.
		if _, err := ParseTransceive(Reply{Kind: KindData, Cmd: CmdReadMeter, Data: []byte{0}}); !errors.Is(err, errNotTransceive) {
			t.Errorf("meter as transceive err = %v, want errNotTransceive", err)
		}
		if _, err := ParseTransceive(Reply{Kind: KindAck, Cmd: CmdFreqTransceive}); !errors.Is(err, errNotTransceive) {
			t.Errorf("ack as transceive err = %v, want errNotTransceive", err)
		}
	})
}

func TestParseMalformedCountedAndDropped(t *testing.T) {
	c := NewCodec()
	bad := map[string][]byte{
		"empty":            {},
		"short":            {0xfe, 0xfe, 0xe0, 0xa2, 0x03},
		"only postamble":   {0xfd},
		"bad preamble":     {0xaa, 0xfe, 0xe0, 0xa2, 0x03, 0xfd},
		"bad postamble":    {0xfe, 0xfe, 0xe0, 0xa2, 0x03, 0x00},
		"not to us":        {0xfe, 0xfe, 0xa2, 0xa2, 0x03, 0xfd},       // dest = the radio itself
		"not from radio":   {0xfe, 0xfe, 0xe0, 0x94, 0x03, 0xfd},       // foreign source
		"unknown command":  {0xfe, 0xfe, 0xe0, 0xa2, 0x25, 0x00, 0xfd}, // 25 is real but not in our table
		"ack with data":    {0xfe, 0xfe, 0xe0, 0xa2, 0xfb, 0x00, 0xfd}, // OK must be bare
		"ng with data":     {0xfe, 0xfe, 0xe0, 0xa2, 0xfa, 0x00, 0xfd}, // NG must be bare
		"freq short":       {0xfe, 0xfe, 0xe0, 0xa2, 0x00, 0x00, 0x00, 0xfd},
		"meter wide":       {0xfe, 0xfe, 0xe0, 0xa2, 0x15, 0x02, 0x01, 0x02, 0xfd},
		"preamp no data":   {0xfe, 0xfe, 0xe0, 0xa2, 0x16, 0x02, 0xfd},
		"unknown 16 sub":   {0xfe, 0xfe, 0xe0, 0xa2, 0x16, 0x40, 0x00, 0xfd}, // outside our sub set
		"att unknown sub":  {0xfe, 0xfe, 0xe0, 0xa2, 0x11, 0x20, 0xfd},
		"band read naked":  {0xfe, 0xfe, 0xe0, 0xa2, 0x07, 0xd2, 0xfd},
		"ptt wide":         {0xfe, 0xfe, 0xe0, 0xa2, 0x1c, 0x00, 0x01, 0x00, 0xfd},
		"data mode narrow": {0xfe, 0xfe, 0xe0, 0xa2, 0x1a, 0x06, 0x01, 0xfd},
	}
	for name, frame := range bad {
		before := c.Dropped()
		if _, err := c.Parse(frame); err == nil {
			t.Errorf("%s: parsed without error, want a counted drop", name)
		}
		if c.Dropped() != before+1 {
			t.Errorf("%s: dropped counter went %d -> %d, want +1", name, before, c.Dropped())
		}
	}
	// Typed error surface: the three vocabulary errors are distinguishable.
	if _, err := c.Parse([]byte{0xfe}); !errors.Is(err, ErrFrameMalformed) {
		t.Errorf("short frame err = %v, want ErrFrameMalformed", err)
	}
	if _, err := c.Parse([]byte{0xfe, 0xfe, 0xa2, 0xa2, 0x03, 0xfd}); !errors.Is(err, ErrForeignFrame) {
		t.Errorf("foreign frame err = %v, want ErrForeignFrame", err)
	}
	if _, err := c.Parse([]byte{0xfe, 0xfe, 0xe0, 0xa2, 0x25, 0xfd}); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("unknown command err = %v, want ErrUnknownCommand", err)
	}
}

func TestParseNeverPanicsOnGarbage(t *testing.T) {
	c := NewCodec()
	rng := rand.New(rand.NewSource(9700)) // deterministic corpus
	corpus := [][]byte{
		radioFrame(ReplyOK),
		radioFrame(CmdReadFreq, 0x00, 0x00, 0x50, 0x44, 0x01),
		radioFrame(CmdModeTransceive, ModeByteDV),
		radioFrame(CmdFunction, FuncPreamp, 0x02),
	}
	for i := 0; i < 20_000; i++ {
		var f []byte
		switch i % 3 {
		case 0: // pure random bytes
			f = make([]byte, rng.Intn(24))
			_, _ = rng.Read(f)
		case 1: // a valid frame with one byte flipped
			f = append([]byte(nil), corpus[i%len(corpus)]...)
			f[rng.Intn(len(f))] = byte(rng.Intn(256))
		case 2: // a valid frame truncated or extended
			f = append([]byte(nil), corpus[i%len(corpus)]...)
			if rng.Intn(2) == 0 {
				f = f[:rng.Intn(len(f))]
			} else {
				f = append(f, byte(rng.Intn(256)))
			}
		}
		before := c.Dropped()
		// Must never panic. An error advances the drop counter — except the
		// NG rejection, which parses successfully (it is a valid frame).
		if _, err := c.Parse(f); err != nil && !errors.Is(err, ErrNG) && c.Dropped() == before {
			t.Fatalf("dropped counter did not advance on error for % x", f)
		}
	}
}
