package civ

import (
	"fmt"
)

// Canonical bus vocabulary (R7): bands and modes are normalized here, at
// the codec, so no raw radio byte ever reaches the bus. DV/DD and RTTY
// mode bytes are RECOGNIZED but reported unsupported — the field is
// omitted upstream, never published raw (R7).

// ModeFromByte maps a CI-V mode byte to the canonical bus vocabulary.
// Second return false = unsupported (omit the field, never a raw guess).
// 07 (CW-R) reports as `cw` — the reverse-filter bit is not representable
// on the bus. 04/08 (RTTY) and 17/22 (DV/DD) are unsupported per R7.
func ModeFromByte(b byte) (string, bool) {
	switch b {
	case 0x00:
		return "lsb", true
	case 0x01:
		return "usb", true
	case 0x02:
		return "am", true
	case 0x03, 0x07:
		return "cw", true
	case 0x05:
		return "fm", true
	default:
		// 0x04/0x08 RTTY, 0x17 DV, 0x22 DD and anything unknown.
		return "", false
	}
}

// ModeToByte maps a canonical bus mode to its set byte (with the default
// filter). Data mode is a separate command (CmdSetDataMode).
func ModeToByte(mode string) (byte, bool) {
	switch mode {
	case "lsb":
		return 0x00, true
	case "usb":
		return 0x01, true
	case "am":
		return 0x02, true
	case "cw":
		return 0x03, true
	case "fm":
		return 0x05, true
	default:
		return 0, false
	}
}

// DefaultFilter is the filter byte paired with mode sets (01 = the radio's
// default filter selection; the bus does not model filter breadth in v1).
const DefaultFilter byte = 0x01

// Frequency bands (R7): the canonical vocabulary is 2m/70cm/23cm with
// per-VFO range validation. Windows are the 9700's amateur segments,
// generous enough for satellite downlinks/uplinks incl. the 2 m LO side
// of many transverters.
type bandDef struct {
	name   string
	minHz  uint64
	maxHz  uint64 // exclusive
	hasSub bool   // usable on the SUB VFO (SUB has no 23cm)
}

var bands = []bandDef{
	{"2m", 140_000_000, 150_000_000, true},
	{"70cm", 420_000_000, 460_000_000, true},
	{"23cm", 1_240_000_000, 1_320_000_000, false},
}

// BandForFreq returns the canonical band label for a frequency, or an
// error when it lies outside every band window.
func BandForFreq(hz uint64) (string, error) {
	for _, b := range bands {
		if hz >= b.minHz && hz < b.maxHz {
			return b.name, nil
		}
	}
	return "", fmt.Errorf("civ: frequency %d Hz outside the 2m/70cm/23cm band windows", hz)
}

// ValidateFreq checks a frequency against the per-VFO band windows: MAIN
// carries all three bands; SUB has no 23cm (R7/KTD-10) — a SUB 23cm set is
// a rejection, not a silent passthrough.
func ValidateFreq(vfo string, hz uint64) error {
	if vfo != "main" && vfo != "sub" {
		return fmt.Errorf("civ: unknown vfo %q", vfo)
	}
	band, err := BandForFreq(hz)
	if err != nil {
		return err
	}
	if vfo == "sub" && band == "23cm" {
		return fmt.Errorf("civ: out of band for sub (23cm is main-only)")
	}
	return nil
}

// BCD10Encode renders a frequency in Hz as the radio's 10-digit BCD
// little-endian form (digit 0 = the 1 Hz place): 144500000 ->
// 00 00 50 41 01. Errors above the 10-digit ceiling (~9.99 GHz).
func BCD10Encode(hz uint64) ([]byte, error) {
	if hz > 9_999_999_999 {
		return nil, fmt.Errorf("civ: frequency %d Hz exceeds the 10-digit BCD ceiling", hz)
	}
	out := make([]byte, 5)
	for i := 0; i < 10; i++ {
		d := hz % 10
		hz /= 10
		if i%2 == 0 {
			out[i/2] |= byte(d) // low nibble = the lower-order digit
		} else {
			out[i/2] |= byte(d) << 4
		}
	}
	return out, nil
}

// BCD10Decode parses 10-digit BCD little-endian into Hz. A nibble > 9 is a
// hard error (garbage never masquerades as a frequency).
func BCD10Decode(b []byte) (uint64, error) {
	if len(b) != 5 {
		return 0, fmt.Errorf("civ: frequency BCD must be 5 bytes, got %d", len(b))
	}
	var hz uint64
	var mult uint64 = 1
	for i := 0; i < 10; i++ {
		by := b[i/2]
		var d uint64
		if i%2 == 0 {
			d = uint64(by & 0x0F)
		} else {
			d = uint64(by >> 4)
		}
		if d > 9 {
			return 0, fmt.Errorf("civ: invalid BCD digit %x at position %d", d, i)
		}
		hz += d * mult
		mult *= 10
	}
	return hz, nil
}
