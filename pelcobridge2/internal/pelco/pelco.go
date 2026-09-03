// Package pelco implements the Pelco-D wire protocol as spoken by the
// PTS-303Z/3050DZ pan/tilt head: frame construction, checksums, an RX
// assembler, and opcode builders.
//
// Bench facts this package is built on (bench 2026-08-28, authoritative):
//
//   - Pan (0x59) AND tilt (0x5B) position responses carry degrees×100 as a
//     big-endian 16-bit word. The earlier "raw encoder counts" tilt model and
//     ptest's "meaning unknown" conclusion were superseded.
//   - The TILT scale is INVERTED relative to antenna elevation (bench
//     2026-08-30): the head's native tilt word runs opposite to elevation over
//     the 0..90 travel — native 0 is the antenna at zenith (the mechanical
//     home), native 90 the horizon; elevation = 90° − native tilt. Set words,
//     readback words, and the jog opcodes all speak the NATIVE scale (OpUp
//     raises native tilt, i.e. lowers the antenna), so callers convert at the
//     wire boundary: TiltToEl / ElToTilt.
//   - Absolute sets (0x4B SetPan / 0x4D SetTilt) work, but only on a quiet
//     line — callers must hold other traffic around them (the engine's gate).
package pelco

import (
	"fmt"
	"math"
	"strings"
)

// Frame is the wire layout: FF addr cmd1 cmd2 d1 d2 sum. cmd1 is 0x00 for
// everything this head documents; the action lives in cmd2.
// checksum = (addr + cmd1 + cmd2 + d1 + d2) & 0xFF.
const FrameLen = 7

// Frame is one 7-byte Pelco-D wire frame.
type Frame [FrameLen]byte

// Build assembles a Pelco-D frame and computes the checksum.
func Build(addr, cmd1, cmd2, d1, d2 byte) Frame {
	f := Frame{0xFF, addr, cmd1, cmd2, d1, d2}
	f[6] = checksum(f)
	return f
}

func checksum(f Frame) byte {
	return byte(uint16(f[1]) + uint16(f[2]) + uint16(f[3]) + uint16(f[4]) + uint16(f[5]))
}

// Hex renders the frame as space-separated upper-case hex.
func (f Frame) Hex() string {
	parts := make([]string, FrameLen)
	for i, b := range f {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, " ")
}

// hexSpaced renders arbitrary bytes as space-separated upper-case hex.
func hexSpaced(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02X", v)
	}
	return strings.Join(parts, " ")
}

// ChkOK reports whether the stored checksum matches the summed bytes.
func (f Frame) ChkOK() bool { return checksum(f) == f[6] }

// Word is d1..d2 read as a big-endian 16-bit value.
func (f Frame) Word() uint16 { return uint16(f[4])<<8 | uint16(f[5]) }

// RxFrame is one frame off the wire: the fields plus the exact wire bytes.
type RxFrame struct {
	Frame
	Wire []byte
}

// Hex renders the exact wire bytes.
func (r RxFrame) Hex() string {
	w := r.Wire
	if len(w) == 0 {
		w = r.Frame[:]
	}
	parts := make([]string, len(w))
	for i, b := range w {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, " ")
}

// ChkOK validates the frame's checksum.
func (r RxFrame) ChkOK() bool {
	return r.Frame.ChkOK()
}

// Op is the cmd2 opcode: the action lives there; cmd1 is always 0x00 here.
func (r RxFrame) Op() byte { return r.Frame[3] }

// Addr is the address byte.
func (r RxFrame) Addr() byte { return r.Frame[1] }

// WordToDeg converts a position readback word to degrees. The bench-confirmed
// encoding is plain hundredths of a degree for BOTH pan (0x59) and tilt (0x5B).
func WordToDeg(w uint16) float64 { return float64(w) / 100 }

// DegToWord converts degrees to the wire's degrees×100 word, rounding to the
// nearest hundredth. Errors rather than silently clamping: a mistyped target
// must fail loudly, not move the head to a rounded corner.
func DegToWord(deg float64) (uint16, error) {
	if math.IsNaN(deg) || math.IsInf(deg, 0) {
		return 0, fmt.Errorf("degrees %v is not finite", deg)
	}
	centi := int(math.Round(deg * 100))
	if centi < 0 || centi > math.MaxUint16 {
		return 0, fmt.Errorf("degrees %.2f out of word range 0..655.35", deg)
	}
	return uint16(centi), nil
}

// TiltToEl converts the head's native tilt word-degrees to antenna elevation.
// The tilt scale is inverted (bench 2026-08-30): elevation = 90° − native tilt
// over the 0..90 travel, so native 0 is zenith and native 90 the horizon.
func TiltToEl(tilt float64) float64 { return 90 - tilt }

// ElToTilt converts antenna elevation to the head's native tilt word-degrees
// (the mirror of TiltToEl; the conversion is its own inverse).
func ElToTilt(el float64) float64 { return 90 - el }

// Norm360 wraps an angle into [0, 360).
func Norm360(d float64) float64 {
	d = math.Mod(d, 360)
	if d < 0 {
		d += 360
	}
	return d
}
