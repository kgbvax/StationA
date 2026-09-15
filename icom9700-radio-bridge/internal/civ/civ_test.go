package civ

// CI-V vocabulary and codec integration tests: the band table against the
// station convention, and every command-table entry exercised against the
// fake radio through the U2 transport (build frame -> SendCIV -> fake
// replies -> parse), including the transceive broadcasts, the typed NG
// rejection and the datalen framing under an embedded FD byte.

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestBandTableMatchesConvention(t *testing.T) {
	// docs/conventions/band-mode-reference.md, "VHF/UHF band frequency
	// ranges" — the canonical table this bridge derives bands from (R7).
	want := []BandRange{
		{Name: "2m", MinHz: 144_000_000, MaxHz: 146_000_000},
		{Name: "70cm", MinHz: 430_000_000, MaxHz: 440_000_000},
		{Name: "23cm", MinHz: 1_240_000_000, MaxHz: 1_300_000_000},
	}
	if len(BandTable) != len(want) {
		t.Fatalf("BandTable has %d rows, want %d", len(BandTable), len(want))
	}
	for i, b := range BandTable {
		if b != want[i] {
			t.Errorf("BandTable[%d] = %+v, want %+v", i, b, want[i])
		}
	}
	// Per-VFO vocabulary: MAIN carries all three, SUB has no 23cm.
	if !VfoMain.AllowsBand("2m") || !VfoMain.AllowsBand("70cm") || !VfoMain.AllowsBand("23cm") {
		t.Error("MAIN must carry 2m/70cm/23cm")
	}
	if !VfoSub.AllowsBand("2m") || !VfoSub.AllowsBand("70cm") {
		t.Error("SUB must carry 2m/70cm")
	}
	if VfoSub.AllowsBand("23cm") {
		t.Error("SUB must not carry 23cm (plan R7/U3)")
	}
}

// civSim is a minimal scripted IC-9700 CI-V state machine for the fake
// radio's responder hook: it answers each command of the bridge's table the
// way the real radio would, keeping per-band state. It runs on the fake's
// serve goroutine (under its mutex), so it touches nothing else.
type civSim struct {
	freqs     [2]uint32 // per selected-band index (0=main, 1=sub)
	mode      byte
	filter    byte
	band      byte // 00 main / 01 sub selected
	satellite bool
	dataMode  bool
	dmFilter  byte
	ptt       bool
	preamp    byte
	att       bool
	meters    map[byte]byte
	rejectNG  map[byte]bool // answer NG once per command byte (scripted)
}

func newCIVSim() *civSim {
	return &civSim{
		freqs:  [2]uint32{432_100_000, 144_500_000},
		mode:   ModeByteFM,
		filter: Filter1,
		meters: map[byte]byte{
			MeterSubS:    120, // S9
			MeterSubSWR:  42,
			MeterSubALC:  7,
			MeterSubCOMP: 200,
		},
		rejectNG: map[byte]bool{},
	}
}

// reply helpers (radio->controller direction).
func (s *civSim) rb(cmd byte, rest ...byte) [][]byte {
	return [][]byte{radioFrame(cmd, rest...)}
}

func (s *civSim) ack() [][]byte { return s.rb(ReplyOK) }

func (s *civSim) bcd(hz uint32) []byte {
	b, err := encodeBCDFreq(hz)
	if err != nil {
		panic(err) // simulator state is always in-vocabulary
	}
	return b[:]
}

// respond answers one client frame; the shape mirrors the official table.
func (s *civSim) respond(frame []byte) [][]byte {
	if len(frame) < 6 || frame[0] != Preamble || frame[1] != Preamble ||
		frame[2] != AddrRadio || frame[3] != AddrCtrl {
		return nil
	}
	cmd := frame[4]
	body := frame[5 : len(frame)-1]
	sub := byte(0)
	if len(body) >= 1 {
		sub = body[0]
	}
	if s.rejectNG[cmd] {
		delete(s.rejectNG, cmd)
		return s.rb(ReplyNG)
	}
	switch cmd {
	case CmdReadFreq: // 03: the selected band's operating frequency
		return s.rb(CmdReadFreq, s.bcd(s.freqs[s.band])...)
	case CmdSetFreq: // 05: set the selected band's frequency, answer OK
		hz, err := decodeBCDFreq(body)
		if err != nil {
			return s.rb(ReplyNG)
		}
		s.freqs[s.band] = hz
		return s.ack()
	case CmdReadMode: // 04
		return s.rb(CmdReadMode, s.mode, s.filter)
	case CmdSetMode: // 06 <mode> [<filter>]
		s.mode, s.filter = body[0], Filter1
		if len(body) >= 2 {
			s.filter = body[1]
		}
		return s.ack()
	case CmdSelectBand: // 07 D0/D1 select, D2 00 read selected
		switch sub {
		case BandSelMain:
			s.band = 0x00
			return s.ack()
		case BandSelSub:
			s.band = 0x01
			return s.ack()
		case BandSelRead:
			return s.rb(CmdSelectBand, BandSelRead, s.band)
		}
		return nil
	case CmdAttenuator: // 11: state rides the sub; 11 10 unambiguously sets on
		switch sub {
		case Attenuator10:
			s.att = true
			return s.ack()
		case AttenuatorOff:
			// 11 00 is BOTH the set-off frame and the read probe (they are
			// byte-identical — the bench pin in the brief). This simulator
			// models the codec's assumed contract: probe = state echo, no
			// side effect. A real radio may instead apply set-off here.
			return s.rb(CmdAttenuator, s.attState())
		}
		return nil
	case CmdRFPower: // 14 0A <level>
		return s.ack()
	case CmdReadMeter: // 15 <sub> <value>
		v, ok := s.meters[sub]
		if !ok {
			return nil
		}
		return s.rb(CmdReadMeter, sub, v)
	case CmdFunction: // 16 02 preamp set/read, 16 5A satellite set/read
		switch sub {
		case FuncPreamp:
			if len(body) >= 2 {
				s.preamp = body[1]
				return s.ack()
			}
			return s.rb(CmdFunction, FuncPreamp, s.preamp)
		case FuncSatellite:
			if len(body) >= 2 {
				s.satellite = body[1] == 0x01
				return s.ack()
			}
			v := byte(0x00)
			if s.satellite {
				v = 0x01
			}
			return s.rb(CmdFunction, FuncSatellite, v)
		}
		return nil
	case CmdTransceiverID: // 19 00 -> 19 00 <address>
		return s.rb(CmdTransceiverID, 0x00, AddrRadio)
	case CmdSetItem: // 1A 06 data mode set/read; 1A 05 0127 transceive on/off
		switch sub {
		case SetItemDataMode:
			if len(body) >= 3 {
				s.dataMode = body[1] == 0x01
				s.dmFilter = body[2]
				return s.ack()
			}
			on := byte(0x00)
			if s.dataMode {
				on = 0x01
			}
			return s.rb(CmdSetItem, SetItemDataMode, on, s.dmFilter)
		case 0x05:
			return s.ack()
		}
		return nil
	case CmdStatus: // 1C 00 PTT set/read
		if len(body) >= 2 {
			s.ptt = body[1] == 0x01
			return s.ack()
		}
		v := byte(0x00)
		if s.ptt {
			v = 0x01
		}
		return s.rb(CmdStatus, StatusSubPTT, v)
	}
	return nil
}

func (s *civSim) attState() byte {
	if s.att {
		return Attenuator10
	}
	return AttenuatorOff
}

// roundTrip sends one frame through the live transport and returns every
// reply the codec parsed since. The sim answers synchronously, so a bounded
// poll on the collector suffices.
func roundTrip(t *testing.T, tr *Transport, fc *frameCollector, c *Codec, frame []byte) []Reply {
	t.Helper()
	base := fc.count()
	if err := tr.SendCIV(frame); err != nil {
		t.Fatalf("SendCIV(% x): %v", frame, err)
	}
	waitFor(t, time.Second, "reply to "+string(frame), func() bool { return fc.count() > base })
	var out []Reply
	for _, chunk := range fc.all()[base:] {
		r, err := c.Parse(chunk)
		if err != nil {
			t.Fatalf("parse reply % x: %v", chunk, err)
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		t.Fatalf("no reply parsed for % x", frame)
	}
	return out
}

func TestEveryCommandThroughFakeRadioTransport(t *testing.T) {
	fr := newFakeRadio(t)
	fc := &frameCollector{}
	c := NewCodec()
	sim := newCIVSim()
	fr.respondCIV(sim.respond)
	tr := dialLive(t, fr, func(o *Opts) { o.OnCIVFrame = fc.add })

	// 03 read frequency: the sim's MAIN band starts at 432.1 MHz.
	r := roundTrip(t, tr, fc, c, c.BuildReadFreq())
	if hz, err := r[0].FreqHz(); err != nil || hz != 432_100_000 {
		t.Errorf("read freq = (%d, %v), want 432.1M", hz, err)
	}

	// 07 D0 select MAIN, 07 D2 00 read selected.
	r = roundTrip(t, tr, fc, c, c.BuildSelectVFO(VfoMain))
	if r[0].Kind != KindAck {
		t.Errorf("select main = %v, want ack", r[0].Kind)
	}
	r = roundTrip(t, tr, fc, c, c.BuildReadSelectedVFO())
	if v, err := r[0].SelectedVFO(); err != nil || v != VfoMain {
		t.Errorf("read selected = (%s, %v)", v, err)
	}

	// 05 set frequency on the selected band, then read it back.
	f, err := c.BuildSetFreq(VfoMain, 144_500_000)
	if err != nil {
		t.Fatalf("build set freq: %v", err)
	}
	if r = roundTrip(t, tr, fc, c, f); r[0].Kind != KindAck {
		t.Errorf("set freq = %v, want ack", r[0].Kind)
	}
	r = roundTrip(t, tr, fc, c, c.BuildReadFreq())
	if hz, err := r[0].FreqHz(); err != nil || hz != 144_500_000 {
		t.Errorf("freq after set = (%d, %v), want 144.5M", hz, err)
	}

	// 04 read mode / 06 set mode (the sim answers FIL1 default after a set).
	r = roundTrip(t, tr, fc, c, c.BuildReadMode())
	if mode, filter, ok, err := r[0].Mode(); err != nil || mode != ModeFM || filter != Filter1 || !ok {
		t.Errorf("read mode = (%q, %d, %v, %v)", mode, filter, ok, err)
	}
	f, err = c.BuildSetMode(ModeCW, Filter2)
	if err != nil {
		t.Fatalf("build set mode: %v", err)
	}
	_ = roundTrip(t, tr, fc, c, f)
	r = roundTrip(t, tr, fc, c, c.BuildReadMode())
	if mode, filter, ok, err := r[0].Mode(); err != nil || mode != ModeCW || filter != Filter2 || !ok {
		t.Errorf("mode after set = (%q, %d, %v, %v)", mode, filter, ok, err)
	}

	// 1A 06 data mode on/off + read (official form — NOT the 06 modifier).
	f, err = c.BuildSetDataMode(true, Filter1)
	if err != nil {
		t.Fatalf("build data mode on: %v", err)
	}
	if r = roundTrip(t, tr, fc, c, f); r[0].Kind != KindAck {
		t.Errorf("data mode on = %v, want ack", r[0].Kind)
	}
	r = roundTrip(t, tr, fc, c, c.BuildReadDataMode())
	if on, filter, err := r[0].DataMode(); err != nil || !on || filter != Filter1 {
		t.Errorf("data mode read = (%v, %d, %v)", on, filter, err)
	}
	f, _ = c.BuildSetDataMode(false, 0)
	_ = roundTrip(t, tr, fc, c, f)
	r = roundTrip(t, tr, fc, c, c.BuildReadDataMode())
	if on, _, err := r[0].DataMode(); err != nil || on {
		t.Errorf("data mode read after off = (%v, %v)", on, err)
	}

	// 07 D1 select SUB (2m/70cm only), read back, set its frequency.
	_ = roundTrip(t, tr, fc, c, c.BuildSelectVFO(VfoSub))
	r = roundTrip(t, tr, fc, c, c.BuildReadSelectedVFO())
	if v, err := r[0].SelectedVFO(); err != nil || v != VfoSub {
		t.Errorf("read selected sub = (%s, %v)", v, err)
	}
	f, err = c.BuildSetFreq(VfoSub, 432_100_000)
	if err != nil {
		t.Fatalf("build sub freq: %v", err)
	}
	_ = roundTrip(t, tr, fc, c, f)
	r = roundTrip(t, tr, fc, c, c.BuildReadFreq())
	if hz, err := r[0].FreqHz(); err != nil || hz != 432_100_000 {
		t.Errorf("sub freq after set = (%d, %v)", hz, err)
	}
	// SUB-VFO 23cm is a codec-level rejection: no frame exists to send.
	before := len(fr.civFramesReceived())
	if _, err := c.BuildSetFreq(VfoSub, 1_296_100_000); !errors.Is(err, ErrBandNotOnSub) {
		t.Errorf("sub 23cm set err = %v, want ErrBandNotOnSub", err)
	}
	if got := len(fr.civFramesReceived()); got != before {
		t.Errorf("sub 23cm set leaked %d frames to the wire", got-before)
	}
	_ = roundTrip(t, tr, fc, c, c.BuildSelectVFO(VfoMain))

	// 16 5A satellite mode on/off + read.
	if r = roundTrip(t, tr, fc, c, c.BuildSetSatellite(true)); r[0].Kind != KindAck {
		t.Errorf("satellite on = %v, want ack", r[0].Kind)
	}
	r = roundTrip(t, tr, fc, c, c.BuildReadSatellite())
	if on, err := r[0].OnOff(); err != nil || !on {
		t.Errorf("satellite read = (%v, %v)", on, err)
	}
	_ = roundTrip(t, tr, fc, c, c.BuildSetSatellite(false))
	r = roundTrip(t, tr, fc, c, c.BuildReadSatellite())
	if on, err := r[0].OnOff(); err != nil || on {
		t.Errorf("satellite read after off = (%v, %v)", on, err)
	}

	// 1C 00 PTT on/off + read (framing only — the arm gate lives above).
	if r = roundTrip(t, tr, fc, c, c.BuildPTT(true)); r[0].Kind != KindAck {
		t.Errorf("ptt on = %v, want ack", r[0].Kind)
	}
	r = roundTrip(t, tr, fc, c, c.BuildReadPTT())
	if on, err := r[0].OnOff(); err != nil || !on {
		t.Errorf("ptt read = (%v, %v)", on, err)
	}
	_ = roundTrip(t, tr, fc, c, c.BuildPTT(false))
	r = roundTrip(t, tr, fc, c, c.BuildReadPTT())
	if on, err := r[0].OnOff(); err != nil || on {
		t.Errorf("ptt read after off = (%v, %v)", on, err)
	}

	// 19 00 transceiver ID.
	r = roundTrip(t, tr, fc, c, c.BuildReadID())
	if id, err := r[0].RadioID(); err != nil || id != AddrRadio {
		t.Errorf("ID = (0x%02x, %v), want 0xa2", id, err)
	}

	// 15 02/12/13/14 meters.
	meters := map[Meter]byte{MeterS: 120, MeterSWR: 42, MeterALC: 7, MeterCOMP: 200}
	for m, wantV := range meters {
		r = roundTrip(t, tr, fc, c, c.BuildReadMeter(m))
		if v, err := r[0].MeterLevel(); err != nil || v != wantV {
			t.Errorf("meter %d = (%d, %v), want %d", m, v, err, wantV)
		}
	}

	// 16 02 preamp set + read.
	f, err = c.BuildSetPreamp(0x01)
	if err != nil {
		t.Fatalf("build preamp: %v", err)
	}
	if r = roundTrip(t, tr, fc, c, f); r[0].Kind != KindAck {
		t.Errorf("preamp set = %v, want ack", r[0].Kind)
	}
	r = roundTrip(t, tr, fc, c, c.BuildReadPreamp())
	if v, err := r[0].PreampLevel(); err != nil || v != 1 {
		t.Errorf("preamp read = (%d, %v)", v, err)
	}

	// 11 attenuator: set on (11 10 -> FB), then read via the 11 00 probe.
	// On the wire the probe is byte-identical to the set-off frame — the
	// bench pin in the brief — so the sim (modeling the codec's assumed
	// read contract) answers the probe with a state echo and the set-off
	// direction stays byte-asserted in codec_test instead.
	_ = roundTrip(t, tr, fc, c, c.BuildSetAttenuator(true))
	r = roundTrip(t, tr, fc, c, c.BuildReadAttenuator())
	if on, err := r[0].Attenuator(); err != nil || !on {
		t.Errorf("att read = (%v, %v)", on, err)
	}

	// 14 0A RF output power (selected-band scoped; MAIN is selected here).
	f, err = c.BuildSetPower(128)
	if err != nil {
		t.Fatalf("build power: %v", err)
	}
	if r = roundTrip(t, tr, fc, c, f); r[0].Kind != KindAck {
		t.Errorf("power set = %v, want ack", r[0].Kind)
	}

	// 1A 05 0127 CI-V Transceive on/off.
	if r = roundTrip(t, tr, fc, c, c.BuildSetCIVTransceive(true)); r[0].Kind != KindAck {
		t.Errorf("civ transceive on = %v, want ack", r[0].Kind)
	}
	_ = roundTrip(t, tr, fc, c, c.BuildSetCIVTransceive(false))
}

func TestTransceiveBroadcastsEndToEnd(t *testing.T) {
	fr := newFakeRadio(t)
	fc := &frameCollector{}
	c := NewCodec()
	dialLive(t, fr, func(o *Opts) { o.OnCIVFrame = fc.add })

	// The radio pushes transceive broadcasts unsolicited; each is one
	// datalen-framed CI-V payload on the data stream.
	inject := func(payload []byte) {
		before := fc.count()
		fr.injectCIV(payload)
		waitFor(t, time.Second, "broadcast", func() bool { return fc.count() > before })
	}

	expect := []struct {
		name  string
		frame []byte
		check func(*testing.T, Transceive)
	}{
		{"freq 70cm", radioFrame(CmdFreqTransceive, mustBCD(t, 432_100_000)...), func(t *testing.T, ev Transceive) {
			if ev.Kind != TransceiveFreq || ev.FreqHz != 432_100_000 || ev.Band != "70cm" || !ev.BandOK {
				t.Errorf("event = %+v", ev)
			}
		}},
		{"freq 23cm", radioFrame(CmdFreqTransceive, mustBCD(t, 1_296_100_000)...), func(t *testing.T, ev Transceive) {
			if ev.Kind != TransceiveFreq || ev.FreqHz != 1_296_100_000 || ev.Band != "23cm" || !ev.BandOK {
				t.Errorf("event = %+v", ev)
			}
		}},
		{"mode fm fil2", radioFrame(CmdModeTransceive, ModeByteFM, Filter2), func(t *testing.T, ev Transceive) {
			if ev.Kind != TransceiveMode || ev.Mode != ModeFM || !ev.ModeOK || ev.Filter != Filter2 {
				t.Errorf("event = %+v", ev)
			}
		}},
		{"mode DV omitted", radioFrame(CmdModeTransceive, ModeByteDV), func(t *testing.T, ev Transceive) {
			if ev.Kind != TransceiveMode || ev.ModeOK || ev.Mode != "" {
				t.Errorf("DV published raw: %+v", ev)
			}
		}},
		{"mode DD omitted", radioFrame(CmdModeTransceive, ModeByteDD, Filter1), func(t *testing.T, ev Transceive) {
			if ev.Kind != TransceiveMode || ev.ModeOK || ev.Mode != "" {
				t.Errorf("DD published raw: %+v", ev)
			}
		}},
		{"mode RTTY omitted", radioFrame(CmdModeTransceive, ModeByteRTTY), func(t *testing.T, ev Transceive) {
			if ev.Kind != TransceiveMode || ev.ModeOK || ev.Mode != "" {
				t.Errorf("RTTY published raw: %+v", ev)
			}
		}},
		{"ptt on", radioFrame(CmdStatus, StatusSubPTT, 0x01), func(t *testing.T, ev Transceive) {
			if ev.Kind != TransceivePTT || !ev.PTTOn {
				t.Errorf("event = %+v", ev)
			}
		}},
		{"ptt off", radioFrame(CmdStatus, StatusSubPTT, 0x00), func(t *testing.T, ev Transceive) {
			if ev.Kind != TransceivePTT || ev.PTTOn {
				t.Errorf("event = %+v", ev)
			}
		}},
		{"s-meter with embedded FD byte", radioFrame(CmdReadMeter, MeterSubS, 0xfd), func(t *testing.T, ev Transceive) {
			// The datalen framing (never FD scanning) delivered the full
			// value byte: this is a meter frame, not a transceive — the
			// refusal itself proves the payload arrived intact, 0xFD inside.
			if ev.Kind == TransceiveFreq || ev.Kind == TransceivePTT {
				t.Errorf("meter frame misread as %v", ev.Kind)
			}
		}},
	}
	for _, tc := range expect {
		t.Run(tc.name, func(t *testing.T) {
			inject(tc.frame)
			chunks := fc.all()
			r, err := c.Parse(chunks[len(chunks)-1])
			if err != nil {
				t.Fatalf("parse % x: %v", chunks[len(chunks)-1], err)
			}
			if tc.name == "s-meter with embedded FD byte" {
				// The value byte must have survived the transport whole.
				if v, err := r.MeterLevel(); err != nil || v != 253 {
					t.Fatalf("embedded-FD meter value = (%d, %v), want 253", v, err)
				}
				return
			}
			ev, err := ParseTransceive(r)
			if err != nil {
				t.Fatalf("transceive: %v", err)
			}
			tc.check(t, ev)
		})
	}

	// A garbage broadcast arrives intact through the transport, is counted
	// by the codec's parser and dropped; parsing keeps working.
	before := c.Dropped()
	fr.injectCIV([]byte{0xfe, 0xfe, 0xe0, 0xa2, 0xde, 0xad, 0xfd})
	inject(radioFrame(CmdStatus, StatusSubPTT, 0x01))
	if _, err := c.Parse([]byte{0xfe, 0xfe, 0xe0, 0xa2, 0xde, 0xad, 0xfd}); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("garbage parse err = %v, want ErrUnknownCommand", err)
	}
	if c.Dropped() != before+1 {
		t.Errorf("garbage frame not counted: dropped %d -> %d", before, c.Dropped())
	}
}

func TestNGRejectionTypedEndToEnd(t *testing.T) {
	fr := newFakeRadio(t)
	fc := &frameCollector{}
	c := NewCodec()
	sim := newCIVSim()
	fr.respondCIV(sim.respond)
	tr := dialLive(t, fr, func(o *Opts) { o.OnCIVFrame = fc.add })

	// Script the radio to NG the next 05 set-frequency (the real radio does
	// this in satellite or memory mode). Under the fake's lock, because the
	// responder reads this map on the serve goroutine.
	fr.script(func() { sim.rejectNG[CmdSetFreq] = true })
	f, err := c.BuildSetFreq(VfoMain, 144_500_000)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	base := fc.count()
	if err := tr.SendCIV(f); err != nil {
		t.Fatalf("SendCIV: %v", err)
	}
	waitFor(t, time.Second, "NG reply", func() bool { return fc.count() > base })
	r, err := c.Parse(fc.all()[base])
	if r.Kind != KindNG {
		t.Errorf("reply kind = %v, want KindNG", r.Kind)
	}
	if !errors.Is(err, ErrNG) {
		t.Fatalf("parse err = %v, want ErrNG", err)
	}
	// The caller attaches the pending command (the FA frame carries none).
	ng, ok := AsNG(err)
	if !ok {
		t.Fatalf("AsNG failed on %v", err)
	}
	_ = ng.WithCommand(CmdSetFreq)
	if got := err.Error(); got == "" {
		t.Error("empty NG error text")
	}
	t.Log("typed rejection:", err)

	// The radio recovers: the next set answers OK again.
	f, _ = c.BuildSetFreq(VfoMain, 144_500_000)
	r2 := roundTrip(t, tr, fc, c, f)
	if r2[0].Kind != KindAck {
		t.Errorf("post-NG set = %v, want ack", r2[0].Kind)
	}
}

func TestCodecConcurrentParseUnderRace(t *testing.T) {
	// Parse is stateless except the dropped counter; the -race build proves
	// concurrent cmd workers and the read goroutine cannot corrupt it.
	c := NewCodec()
	frames := [][]byte{
		radioFrame(ReplyOK),
		radioFrame(CmdReadFreq, 0x00, 0x00, 0x50, 0x44, 0x01),
		radioFrame(CmdModeTransceive, ModeByteFM, Filter1),
		{0xfe, 0xfe, 0xe0, 0xa2, 0x03}, // malformed
		radioFrame(CmdStatus, StatusSubPTT, 0x01),
	}
	const perFrame = 300
	var wg sync.WaitGroup
	for _, f := range frames {
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(f []byte) {
				defer wg.Done()
				for j := 0; j < perFrame; j++ {
					_, _ = c.Parse(f)
				}
			}(f)
		}
	}
	wg.Wait()
	// One malformed frame shape at 4 goroutines x 300 passes.
	if got, want := c.Dropped(), uint64(4*perFrame); got != want {
		t.Errorf("dropped = %d, want %d", got, want)
	}
}

// mustBCD is the test-side frequency encoder used by broadcast scenarios.
func mustBCD(t *testing.T, hz uint32) []byte {
	t.Helper()
	b, err := encodeBCDFreq(hz)
	if err != nil {
		t.Fatalf("bcd(%d): %v", hz, err)
	}
	return b[:]
}
