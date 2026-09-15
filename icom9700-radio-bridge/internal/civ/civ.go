package civ

// CI-V layer vocabulary: framing bytes, the command table this bridge uses,
// the operating-mode byte map, the canonical band table with per-VFO range
// validation, and the typed error values.
//
// Every byte here is pinned by docs/civ-research-brief.md in this module
// (see the "U3 codec pin" section there): the official IC-9700 CI-V
// Reference Guide command table, cross-checked against wfview/kappanhang.
// Where the U1 brief and the official manual disagreed, the official manual
// won and the correction is recorded in the brief — notably data mode
// (1A 06, not 06) and the attenuator (11 00/11 10, not 16 11).

import (
	"errors"
	"fmt"
)

// CI-V framing (official guide, "About the data format"): both directions
// are FE FE <dest> <src> <cmd> [<sub>] [<data>...] FD. The radio answers
// OK with ...FB FD and rejects with ...FA FD (NG carries NO echo of the
// rejected command — the caller attaches it, see NGError).
const (
	Preamble  = 0xfe
	Postamble = 0xfd
	// AddrRadio is the IC-9700's CI-V address (A2; radio-side prerequisite,
	// deploy gate 3) and AddrCtrl the controller's (E0).
	AddrRadio = 0xa2
	AddrCtrl  = 0xe0
)

// CI-V command codes of this bridge's table (brief, "Command table").
const (
	CmdFreqTransceive = 0x00 // radio -> controller broadcast, MAIN and SUB changes
	CmdModeTransceive = 0x01 // radio -> controller broadcast
	CmdReadFreq       = 0x03
	CmdReadMode       = 0x04
	CmdSetFreq        = 0x05
	CmdSetMode        = 0x06
	CmdSelectBand     = 0x07 // MAIN/SUB band select + read selected
	CmdAttenuator     = 0x11 // 11 00 ATT off / 11 10 ATT 10 dB (U3 correction: not 16 11)
	CmdRFPower        = 0x14 // 14 0A RF output power
	CmdReadMeter      = 0x15
	CmdFunction       = 0x16 // 16 02 preamp, 16 5A satellite mode
	CmdTransceiverID  = 0x19 // 19 00 read the transceiver ID
	CmdSetItem        = 0x1a // 1A 06 data mode, 1A 05 0127 CI-V transceive
	CmdStatus         = 0x1c // 1C 00 transceiver status (PTT)
)

// Sub-command bytes of the table above.
const (
	BandSelMain          = 0xd0 // select the main band
	BandSelSub           = 0xd1 // select the sub band
	BandSelRead          = 0xd2 // send/read main band selection (data 00=main, 01=sub)
	AttenuatorOff        = 0x00 // 11 00
	Attenuator10         = 0x10 // 11 10 (10 dB)
	PowerRF              = 0x0a // 14 0A
	MeterSubS            = 0x02 // 15 02 S-meter (0-255, S9=120)
	MeterSubSWR          = 0x12 // 15 12
	MeterSubALC          = 0x13 // 15 13
	MeterSubCOMP         = 0x14 // 15 14
	FuncPreamp           = 0x02 // 16 02 (0-3, P.AMP x EXT-P.AMP)
	FuncSatellite        = 0x5a // 16 5A satellite mode on/off/read
	SetItemDataMode      = 0x06 // 1A 06 data mode with filter (U3 correction: not 06)
	SetItemCIVTransceive = 0x27 // 1A 05 0127 CI-V transceive on/off
	StatusSubPTT         = 0x00 // 1C 00 (00=RX, 01=TX)
)

// Reply codes.
const (
	ReplyOK = 0xfb
	ReplyNG = 0xfa
)

// Operating-mode bytes (official guide, "Operating mode", commands 01/04/06).
const (
	ModeByteLSB   = 0x00
	ModeByteUSB   = 0x01
	ModeByteAM    = 0x02
	ModeByteCW    = 0x03
	ModeByteRTTY  = 0x04 // never published raw (R7)
	ModeByteFM    = 0x05
	ModeByteCWR   = 0x07 // reported as cw — the reverse-filter bit is not representable
	ModeByteRTTYR = 0x08 // never published raw (R7)
	ModeByteDV    = 0x17 // never published raw (R7)
	ModeByteDD    = 0x22 // never published raw (R7)
)

// IF filter bytes ("Filter setting": 01=FIL1, 02=FIL2, 03=FIL3). Commands 01
// and 06 may omit the filter byte (then FIL1 / the mode default applies).
const (
	FilterOmit = 0x00
	Filter1    = 0x01
	Filter2    = 0x02
	Filter3    = 0x03
)

// Canonical bus modes (docs/conventions/band-mode-reference.md). ModeData is
// reachable only through the 1A 06 data-mode modifier — there is no operating
// mode byte for it.
const (
	ModeCW   = "cw"
	ModeUSB  = "usb"
	ModeLSB  = "lsb"
	ModeAM   = "am"
	ModeFM   = "fm"
	ModeData = "data"
)

// modeCanonical maps every mode byte the IC-9700 can report. ok=false bytes
// (RTTY both filters, DV, DD) are recognized but NEVER published raw: the
// transceive path omits the mode field instead (R7 — one defined behavior
// for every mode byte). CW-R collapses onto cw per the brief.
func modeCanonical(b byte) (mode string, ok bool) {
	switch b {
	case ModeByteLSB:
		return ModeLSB, true
	case ModeByteUSB:
		return ModeUSB, true
	case ModeByteAM:
		return ModeAM, true
	case ModeByteCW, ModeByteCWR:
		return ModeCW, true
	case ModeByteFM:
		return ModeFM, true
	default:
		// ModeByteRTTY, ModeByteRTTYR, ModeByteDV, ModeByteDD and anything unknown.
		return "", false
	}
}

// modeByte maps a canonical bus mode onto the operating-mode byte for
// commands 06. ModeData has no byte here on purpose: it is the 1A 06
// modifier (BuildSetDataMode).
func modeByte(mode string) (byte, bool) {
	switch mode {
	case ModeLSB:
		return ModeByteLSB, true
	case ModeUSB:
		return ModeByteUSB, true
	case ModeAM:
		return ModeByteAM, true
	case ModeCW:
		return ModeByteCW, true
	case ModeFM:
		return ModeByteFM, true
	default:
		return 0, false
	}
}

// VFO selects one of the radio's two bands. In satellite mode the radio
// transmits on SUB and receives on MAIN (brief, "Satellite semantics").
type VFO uint8

const (
	VfoMain VFO = iota
	VfoSub
)

func (v VFO) String() string {
	switch v {
	case VfoMain:
		return "main"
	case VfoSub:
		return "sub"
	default:
		return fmt.Sprintf("vfo(%d)", uint8(v))
	}
}

// BandRange is one row of the canonical VHF/UHF band table
// (docs/conventions/band-mode-reference.md); bounds are inclusive Hz.
type BandRange struct {
	Name         string
	MinHz, MaxHz uint32
}

// BandTable is the canonical 2m/70cm/23cm vocabulary with its frequency
// ranges — the single source this codec validates against (R7).
var BandTable = []BandRange{
	{Name: "2m", MinHz: 144_000_000, MaxHz: 146_000_000},
	{Name: "70cm", MinHz: 430_000_000, MaxHz: 440_000_000},
	{Name: "23cm", MinHz: 1_240_000_000, MaxHz: 1_300_000_000},
}

// BandForFreq derives the canonical band name for a frequency (R7: bands
// derive from the canonical table). ok=false for frequencies outside it.
func BandForFreq(hz uint32) (name string, ok bool) {
	for _, b := range BandTable {
		if hz >= b.MinHz && hz <= b.MaxHz {
			return b.Name, true
		}
	}
	return "", false
}

// AllowsBand reports whether the VFO carries the band. The SUB band has no
// 23cm receiver (plan R7/U3: per-VFO validation — SUB-VFO 23cm is a rejection).
func (v VFO) AllowsBand(name string) bool {
	switch v {
	case VfoMain:
		return name == "2m" || name == "70cm" || name == "23cm"
	case VfoSub:
		return name == "2m" || name == "70cm"
	default:
		return false
	}
}

// Typed errors of the CI-V vocabulary. Parse drops count into Codec.Dropped;
// the NG typed rejection is NGError (ErrNG sentinel).
var (
	// ErrFrameMalformed: the frame is not a well-formed CI-V frame (length,
	// preamble, postamble or data-area shape wrong for the command).
	ErrFrameMalformed = errors.New("civ: malformed CI-V frame")
	// ErrForeignFrame: the frame is not addressed to this controller from
	// this radio (CI-V buses can carry third-party traffic; LAN does not).
	ErrForeignFrame = errors.New("civ: CI-V frame not between this radio and controller")
	// ErrUnknownCommand: the command byte is outside this bridge's table.
	ErrUnknownCommand = errors.New("civ: CI-V command outside the bridge table")
	// ErrNG is the sentinel every *NGError unwraps to.
	ErrNG = errors.New("civ: radio rejected the command (NG)")
	// ErrFreqRange: the frequency is outside the canonical band table.
	ErrFreqRange = errors.New("civ: frequency outside the station band table (2m/70cm/23cm)")
	// ErrBandNotOnSub: the band does not exist on the SUB VFO (23cm is
	// MAIN-only).
	ErrBandNotOnSub = errors.New("civ: band not available on the SUB VFO (23cm is MAIN-only)")
	// ErrBadModeName: the mode name is not canonical, or is ModeData where
	// an operating-mode byte is required (data mode is the 1A 06 modifier).
	ErrBadModeName = errors.New("civ: unsupported canonical mode name")
	// ErrBadFilter: the filter byte is not 0 (omit) or one of FIL1-FIL3.
	ErrBadFilter = errors.New("civ: filter byte must be 0 (omit) or 1-3")
	// ErrBadLevel: a numeric argument is outside its command's range.
	ErrBadLevel = errors.New("civ: value out of range for the command")
)

// ValidateFreq checks a frequency against the per-VFO band vocabulary:
// it must land in a canonical band the VFO carries (plan U3: band-plan
// validation lives in the codec — a SUB-VFO 23cm set is a rejection).
func ValidateFreq(v VFO, hz uint32) error {
	name, ok := BandForFreq(hz)
	if !ok {
		return fmt.Errorf("%w: %d Hz", ErrFreqRange, hz)
	}
	if !v.AllowsBand(name) {
		return fmt.Errorf("%w: %s on %s", ErrBandNotOnSub, name, v)
	}
	return nil
}

// Meter selects one of the read-only meter commands (15 xx). Values are one
// byte 0-255; the S-meter reads S0=0, S9=120, S9+60dB=241.
type Meter byte

const (
	MeterS    Meter = MeterSubS
	MeterSWR  Meter = MeterSubSWR
	MeterALC  Meter = MeterSubALC
	MeterCOMP Meter = MeterSubCOMP
)

func (m Meter) sub() byte { return byte(m) }
