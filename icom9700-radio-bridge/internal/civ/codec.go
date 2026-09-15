package civ

// Codec builds the CI-V frames of this bridge's command table and is the
// only place that produces them. Frame bytes are pinned per the committed
// brief (see its "U3 codec pin" section for the official-table corrections:
// data mode 1A 06, attenuator 11 00/11 10, ID 19 00). Band-plan validation
// happens here at build time, so an out-of-vocabulary frame can never reach
// the wire.
//
// Two lanes never emit anything: the LAN main-power commands (18 01/00) are
// out of scope per KTD-10, and there is deliberately no builder for them —
// the test suite asserts no builder can produce an 18 0x frame.

import (
	"fmt"
	"sync/atomic"
)

// Codec is the CI-V frame layer over one radio (addresses fixed at
// construction). It is safe for concurrent use: builders are pure and the
// only state is the dropped-frame counter.
type Codec struct {
	radio byte
	ctrl  byte

	dropped atomic.Uint64
}

// NewCodec returns a codec pinned to the IC-9700's addresses (radio A2,
// controller E0).
func NewCodec() *Codec {
	return &Codec{radio: AddrRadio, ctrl: AddrCtrl}
}

// Dropped reports how many inbound frames Parse refused as malformed,
// foreign or unknown — all counted and dropped, never panicking (plan U3).
func (c *Codec) Dropped() uint64 { return c.dropped.Load() }

// frame assembles FE FE <radio> <ctrl> <cmd> <rest...> FD — the
// controller-to-radio direction.
func (c *Codec) frame(cmd byte, rest ...byte) []byte {
	f := make([]byte, 0, 6+len(rest))
	f = append(f, Preamble, Preamble, c.radio, c.ctrl, cmd)
	f = append(f, rest...)
	return append(f, Postamble)
}

// --- BCD frequency ------------------------------------------------------------------

// encodeBCDFreq packs a frequency into the 5-byte little-endian BCD the
// frequency commands carry (official guide "Operating frequency", commands
// 00/03/05/1C 03): byte i holds digit(2i+1) in the high nibble and
// digit(2i) in the low nibble of the Hz value. 144.500.000 Hz ->
// 00 00 50 44 01. (The U1 brief's example bytes 00 00 50 41 01 are a
// transcription typo — they decode to 141.500.000; see the brief's U3 pin.)
func encodeBCDFreq(hz uint32) ([5]byte, error) {
	if hz%10 != 0 {
		return [5]byte{}, fmt.Errorf("%w: %d Hz is not a multiple of 10 Hz", ErrBadLevel, hz)
	}
	var out [5]byte
	for i := range out {
		d0 := byte((hz / pow10(2*i)) % 10)   // 10^(2i) digit: 1 Hz, 1 kHz, 100 kHz, 10 MHz, 1 GHz
		d1 := byte((hz / pow10(2*i+1)) % 10) // 10^(2i+1) digit: 10 Hz, 100 Hz, 1 MHz, 100 MHz, 10 GHz
		out[i] = d1<<4 | d0
	}
	return out, nil
}

// decodeBCDFreq unpacks the inverse. A non-decimal nibble is bit corruption
// the datalen framing cannot exclude, so it is an error, not a panic.
func decodeBCDFreq(b []byte) (uint32, error) {
	if len(b) != 5 {
		return 0, fmt.Errorf("%w: frequency data area is %d bytes, want 5", ErrFrameMalformed, len(b))
	}
	var hz uint64
	for i := 0; i < 5; i++ {
		d0, d1 := uint64(b[i]&0x0f), uint64(b[i]>>4)
		if d0 > 9 || d1 > 9 {
			return 0, fmt.Errorf("%w: non-BCD nibble in frequency byte %d (0x%02x)", ErrFrameMalformed, i, b[i])
		}
		hz += d0 * uint64(pow10(2*i))
		hz += d1 * uint64(pow10(2*i+1))
	}
	if hz > 0xffffffff {
		return 0, fmt.Errorf("%w: frequency %d Hz overflows uint32", ErrFrameMalformed, hz)
	}
	return uint32(hz), nil
}

func pow10(n int) uint32 {
	p := uint32(1)
	for i := 0; i < n; i++ {
		p *= 10
	}
	return p
}

// --- builders ------------------------------------------------------------------------

// BuildReadFreq builds the read-frequency frame (03).
func (c *Codec) BuildReadFreq() []byte {
	return c.frame(CmdReadFreq)
}

// BuildSetFreq builds the set-frequency frame (05 <10 BCD>) for one VFO.
// The frame itself carries no VFO — the caller selects the band first with
// BuildSelectVFO (the same scoping 14 0A power rides). Frequencies outside
// the VFO's canonical bands are rejected here, before any frame exists.
func (c *Codec) BuildSetFreq(v VFO, hz uint32) ([]byte, error) {
	if err := ValidateFreq(v, hz); err != nil {
		return nil, err
	}
	bcd, err := encodeBCDFreq(hz)
	if err != nil {
		return nil, err
	}
	return c.frame(CmdSetFreq, bcd[:]...), nil
}

// BuildSelectVFO builds the band-select frame (07 D0 main / 07 D1 sub).
func (c *Codec) BuildSelectVFO(v VFO) []byte {
	if v == VfoSub {
		return c.frame(CmdSelectBand, BandSelSub)
	}
	return c.frame(CmdSelectBand, BandSelMain)
}

// BuildReadSelectedVFO builds the read-selected-band frame (07 D2 00). The
// reply's data byte answers 00=main, 01=sub.
func (c *Codec) BuildReadSelectedVFO() []byte {
	return c.frame(CmdSelectBand, BandSelRead, 0x00)
}

// BuildReadMode builds the read-operating-mode frame (04).
func (c *Codec) BuildReadMode() []byte {
	return c.frame(CmdReadMode)
}

// BuildSetMode builds the set-mode frame (06 <mode> [<filter>]). mode must
// be canonical and have an operating-mode byte (cw/usb/lsb/am/fm — ModeData
// is the 1A 06 modifier, see BuildSetDataMode). filter 0 (FilterOmit) omits
// the byte — the radio then picks the mode's default filter; otherwise it
// must be FIL1-FIL3.
func (c *Codec) BuildSetMode(mode string, filter byte) ([]byte, error) {
	b, ok := modeByte(mode)
	if !ok {
		return nil, fmt.Errorf("%w: %q (data mode is the 1A 06 modifier, not an operating mode)", ErrBadModeName, mode)
	}
	if filter > Filter3 {
		return nil, fmt.Errorf("%w: 0x%02x", ErrBadFilter, filter)
	}
	if filter == FilterOmit {
		return c.frame(CmdSetMode, b), nil
	}
	return c.frame(CmdSetMode, b, filter), nil
}

// BuildSetDataMode builds the data-mode frame — officially 1A 06 <q> <w>
// ("DATA mode with filter set"): q 01=on / 00=off, w FIL1-FIL3 when on. The
// official table pins "when 00 is set, also set 00 to w", so off is always
// 1A 06 00 00 regardless of filter. (The U1 brief's `06 <00/01> <filter>`
// modifier is a transcription error — on the real radio 06 00/01 are the
// LSB/USB operating-mode bytes; see the brief's U3 pin.)
func (c *Codec) BuildSetDataMode(on bool, filter byte) ([]byte, error) {
	if on {
		if filter < Filter1 || filter > Filter3 {
			return nil, fmt.Errorf("%w: data mode on needs FIL1-FIL3, got 0x%02x", ErrBadFilter, filter)
		}
		return c.frame(CmdSetItem, SetItemDataMode, 0x01, filter), nil
	}
	return c.frame(CmdSetItem, SetItemDataMode, 0x00, 0x00), nil
}

// BuildReadDataMode builds the data-mode read frame (1A 06, no data area).
func (c *Codec) BuildReadDataMode() []byte {
	return c.frame(CmdSetItem, SetItemDataMode)
}

// BuildSetCIVTransceive builds the CI-V Transceive on/off frame
// (1A 05 0127 <00/01>). The radio-side menu must be ON for transceive
// broadcasts to flow at all (deploy gate 3); the builder exists so the
// bridge can verify/restore it on session.
func (c *Codec) BuildSetCIVTransceive(on bool) []byte {
	data := byte(0x00)
	if on {
		data = 0x01
	}
	// Sub 05 with the 2-byte BCD item number 01 27.
	return c.frame(CmdSetItem, 0x05, 0x01, SetItemCIVTransceive, data)
}

// BuildSetSatellite builds the satellite-mode frame (16 5A <00/01>).
func (c *Codec) BuildSetSatellite(on bool) []byte {
	if on {
		return c.frame(CmdFunction, FuncSatellite, 0x01)
	}
	return c.frame(CmdFunction, FuncSatellite, 0x00)
}

// BuildReadSatellite builds the satellite-mode read frame (16 5A, no data).
func (c *Codec) BuildReadSatellite() []byte {
	return c.frame(CmdFunction, FuncSatellite)
}

// BuildPTT builds the PTT frame (1C 00 <00/01>; official table: "send/read
// the transceiver's status", 00=RX 01=TX). Emitting this is the caller's
// safety decision (R10 arm gate, R12 watchdog) — the codec just frames it.
func (c *Codec) BuildPTT(on bool) []byte {
	if on {
		return c.frame(CmdStatus, StatusSubPTT, 0x01)
	}
	return c.frame(CmdStatus, StatusSubPTT, 0x00)
}

// BuildReadPTT builds the PTT read frame (1C 00, no data).
func (c *Codec) BuildReadPTT() []byte {
	return c.frame(CmdStatus, StatusSubPTT)
}

// BuildReadID builds the transceiver-ID probe (19 00; the official table
// documents the sub command). BENCH-ONLY today: the live handshake does NOT
// send it — radio identity is not verified at connect (a bench pin, see
// docs/civ-research-brief.md).
func (c *Codec) BuildReadID() []byte {
	return c.frame(CmdTransceiverID, 0x00)
}

// BuildReadMeter builds a meter read frame (15 <sub>): MeterS, MeterSWR,
// MeterALC or MeterCOMP.
func (c *Codec) BuildReadMeter(m Meter) []byte {
	return c.frame(CmdReadMeter, m.sub())
}

// BuildReadPreamp builds the preamp read frame (16 02, no data). The reply
// data byte carries the documented 0-3 value (00=P.AMP off/EXT-P.AMP off,
// 01=P.AMP on, 02=EXT-P.AMP on, 03=both on).
//
// BAND SCOPING — BENCH PIN: the official table carries no band-selector
// byte for 16 02 (the plan's carry-forward expected one), so per-band
// preamp reads must be scoped by selecting the band first (BuildSelectVFO).
// Whether 16 02 follows the selected band on the IC-9700 is UNVERIFIED —
// see the brief's U3 pin before trusting per-band /state values.
func (c *Codec) BuildReadPreamp() []byte {
	return c.frame(CmdFunction, FuncPreamp)
}

// BuildSetPreamp builds the preamp set frame (16 02 <0-3>): 00 both off,
// 01 P.AMP on, 02 EXT-P.AMP on, 03 both on (official table values).
func (c *Codec) BuildSetPreamp(level byte) ([]byte, error) {
	if level > 0x03 {
		return nil, fmt.Errorf("%w: preamp level %d, want 0-3", ErrBadLevel, level)
	}
	return c.frame(CmdFunction, FuncPreamp, level), nil
}

// BuildSetAttenuator builds the attenuator frame — officially command 11
// with the state in the sub command: 11 00 = ATT off, 11 10 = ATT 10 dB.
// (The U1 brief's `16 11` does not exist in the official table; KTD-10's
// "attenuator 16 11" is corrected per the brief's U3 pin.)
func (c *Codec) BuildSetAttenuator(on bool) []byte {
	if on {
		return c.frame(CmdAttenuator, Attenuator10)
	}
	return c.frame(CmdAttenuator, AttenuatorOff)
}

// BuildReadAttenuator builds the attenuator read probe. The official table
// marks the ATT pair send/read but shows no separate read form; by the CI-V
// convention (no data area = read) the probe is 11 00 and the radio is
// expected to answer with the current state (11 00 or 11 10).
//
// READ SEMANTICS — BENCH PIN: the state-echo reply is inferred from the
// send/read convention, not explicitly documented; and like 16 02, whether
// the probe scopes to the selected band is UNVERIFIED. See the brief's
// U3 pin.
func (c *Codec) BuildReadAttenuator() []byte {
	return c.frame(CmdAttenuator, AttenuatorOff)
}

// BuildSetPower builds the RF output power frame (14 0A <0-255>). The level
// applies to the currently SELECTED band's VFO — the caller selects with
// BuildSelectVFO first (KTD-10). There is deliberately no builder for the
// LAN main-power commands (18 01/00, out of scope per KTD-10).
func (c *Codec) BuildSetPower(level int) ([]byte, error) {
	if level < 0 || level > 255 {
		return nil, fmt.Errorf("%w: RF power %d, want 0-255", ErrBadLevel, level)
	}
	return c.frame(CmdRFPower, PowerRF, byte(level)), nil
}
