// Package pelco implements the Pelco-D and Pelco-P wire protocols as spoken by
// the PTS-303Z/3050DZ pan/tilt head: frame construction, checksums, an RX
// assembler, and opcode builders.
//
// Bench facts this package is built on (bench 2026-08-28, authoritative):
//
//   - Pan (0x59) AND tilt (0x5B) position responses carry degrees×100 as a
//     big-endian 16-bit word. The earlier "raw encoder counts" tilt model and
//     ptest's "meaning unknown" conclusion were superseded.
//   - Absolute sets (0x4B SetPan / 0x4D SetTilt) work, but only on a quiet
//     line — callers must hold other traffic around them (the engine's gate).
//   - The unit is protocol-adaptive on RX: it answers in the envelope the
//     frame arrived in, so both envelopes are always accepted inbound.
package pelco

import (
	"fmt"
	"math"
	"strings"
)

// Frame is the logical Pelco-D field layout: FF addr cmd1 cmd2 d1 d2 sum.
// cmd1 is 0x00 for everything this head documents; the action lives in cmd2.
// checksum = (addr + cmd1 + cmd2 + d1 + d2) & 0xFF.
//
// Pelco-P carries the SAME logical fields in an 8-byte envelope:
// A0 addr cmd1 cmd2 d1 d2 AF xor, the checksum being the XOR of bytes 1..7
// (STX and ETX included). The same address byte is used in both protocols —
// this head matches one DIP address code regardless of protocol (strict
// Pelco-P gear is zero-indexed; this unit is not).
const (
	FrameLen  = 7
	FrameLenP = 8
	STX       = 0xA0
	ETX       = 0xAF
)

// Frame is the logical 7-byte Pelco-D view; Pelco-P frames are built from it
// via WrapP and normalized back into it on parse.
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

// WrapP re-wraps a logically-built Pelco-D frame as an 8-byte Pelco-P wire
// frame: the address/cmd/data bytes shift right by one to make room for ETX,
// and the checksum becomes the XOR of bytes 1..7 (STX and ETX included).
func WrapP(f Frame) []byte {
	w := []byte{STX, f[1], f[2], f[3], f[4], f[5], ETX, 0}
	w[7] = PXor(w)
	return w
}

// PXor is the Pelco-P checksum: XOR of the seven bytes before it.
func PXor(w []byte) byte {
	c := byte(0)
	for _, b := range w[:7] {
		c ^= b
	}
	return c
}

// PChkOK reports whether an 8-byte buffer is a well-formed Pelco-P frame.
func PChkOK(w []byte) bool {
	if len(w) != FrameLenP || w[0] != STX || w[6] != ETX {
		return false
	}
	return PXor(w) == w[7]
}

// RxFrame is one frame off the wire: the logical D-fields view plus the exact
// wire bytes and the envelope it arrived in.
type RxFrame struct {
	Frame
	P    bool
	Wire []byte
}

// Hex renders the exact wire bytes (7 for D, 8 for P).
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

// ChkOK validates the frame's checksum in its own protocol.
func (r RxFrame) ChkOK() bool {
	if r.P {
		return PChkOK(r.Wire)
	}
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

// Norm360 wraps an angle into [0, 360).
func Norm360(d float64) float64 {
	d = math.Mod(d, 360)
	if d < 0 {
		d += 360
	}
	return d
}
