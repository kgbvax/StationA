// Package pelco implements the Pelco-D and Pelco-P protocols used to control
// PTZ units. Both carry the same command set in different envelopes; a frame's
// Protocol says which envelope it uses on the wire.
//
// A Pelco-D message is a fixed 7-byte frame:
//
//	FF | ADDR | CMD1 | CMD2 | DATA1 | DATA2 | CHECKSUM
//
// where CHECKSUM = (ADDR + CMD1 + CMD2 + DATA1 + DATA2) mod 256.
//
// A Pelco-P message is a fixed 8-byte frame carrying the same logical fields:
//
//	STX | ADDR | CMD1 | CMD2 | DATA1 | DATA2 | ETX | CHECKSUM
//
// where STX = 0xA0, ETX = 0xAF and CHECKSUM = XOR of bytes 1..7 (STX and ETX
// included). The command and data bytes occupy the same logical positions as
// in Pelco-D (shifted right by one on the wire to make room for ETX), so the
// position opcodes (0x4B/0x4D set, 0x51/0x53 query, 0x59/0x5B response) decode
// identically in both protocols.
//
// Addressing: this package sends the same address byte in both protocols — the
// 303Z/3050DZ matches a single DIP "address code" (0–64) regardless of which
// protocol a frame arrived in, and its D frame works with the same byte.
// (Strict Pelco-P gear is zero-indexed while Pelco-D is one-indexed; that
// convention does NOT apply here — do not subtract one.)
package pelco

import (
	"errors"
	"fmt"
)

// Protocol selects the wire envelope for a frame.
type Protocol byte

const (
	// ProtocolD is Pelco-D: 7-byte 0xFF-framed, additive checksum.
	ProtocolD Protocol = iota
	// ProtocolP is Pelco-P: 8-byte 0xA0/0xAF-framed, XOR checksum.
	ProtocolP
)

// String returns the protocol's lowercase name ("d" / "p").
func (p Protocol) String() string {
	if p == ProtocolP {
		return "p"
	}
	return "d"
}

// Sync is the byte that begins every Pelco-D frame.
const Sync = 0xFF

// STX and ETX are the bytes that begin and end every Pelco-P frame.
const (
	STX = 0xA0
	ETX = 0xAF
)

// FrameLen is the fixed length of a Pelco-D frame in bytes.
const FrameLen = 7

// PFrameLen is the fixed length of a Pelco-P frame in bytes.
const PFrameLen = 8

// Frame is a decoded Pelco-D or Pelco-P message (without the framing bytes or
// checksum, which are derived). Proto says which envelope the frame uses (or
// should use) on the wire; the zero value is Pelco-D. The zero Frame is not a
// valid command; use a command builder.
type Frame struct {
	Addr  byte
	Cmd1  byte
	Cmd2  byte
	Data1 byte
	Data2 byte
	Proto Protocol
}

// checksumD returns the Pelco-D checksum for the frame.
func (f Frame) checksumD() byte {
	return f.Addr + f.Cmd1 + f.Cmd2 + f.Data1 + f.Data2
}

// checksumP returns the Pelco-P checksum for the frame: XOR of the seven
// payload bytes (STX, ADDR, CMD1, CMD2, DATA1, DATA2, ETX).
func (f Frame) checksumP() byte {
	return STX ^ f.Addr ^ f.Cmd1 ^ f.Cmd2 ^ f.Data1 ^ f.Data2 ^ ETX
}

// BytesIn encodes the frame into its wire representation in the given
// protocol, regardless of the frame's Proto tag.
func (f Frame) BytesIn(p Protocol) []byte {
	if p == ProtocolP {
		return []byte{STX, f.Addr, f.Cmd1, f.Cmd2, f.Data1, f.Data2, ETX, f.checksumP()}
	}
	return []byte{Sync, f.Addr, f.Cmd1, f.Cmd2, f.Data1, f.Data2, f.checksumD()}
}

// Bytes encodes the frame into its wire representation using the frame's own
// Proto (Pelco-D when unset). Builders default to Pelco-D; Parse/ParseP tag
// decoded frames with the protocol they arrived in.
func (f Frame) Bytes() []byte { return f.BytesIn(f.Proto) }

// Word returns the 16-bit big-endian value carried in DATA1/DATA2. For
// position commands and query responses this is the angle in hundredths of a
// degree — in both protocols the word sits in the same logical fields.
func (f Frame) Word() uint16 {
	return uint16(f.Data1)<<8 | uint16(f.Data2)
}

var (
	// ErrLength is returned when a buffer does not hold a whole frame for the
	// protocol implied by its first byte (7 bytes for D, 8 for P).
	ErrLength = errors.New("pelco: wrong frame length")
	// ErrSync is returned when a framing byte is wrong: the first byte is not a
	// recognized start byte (0xFF for Pelco-D, 0xA0 for Pelco-P), or a Pelco-P
	// frame's byte 6 is not ETX. Callers treat it as a false start and rescan
	// from the next byte.
	ErrSync = errors.New("pelco: bad frame sync")
	// ErrChecksum is returned when the trailing checksum does not match.
	ErrChecksum = errors.New("pelco: checksum mismatch")
)

// Parse validates and decodes a single 7-byte Pelco-D frame.
func Parse(b []byte) (Frame, error) {
	if len(b) != FrameLen {
		return Frame{}, ErrLength
	}
	if b[0] != Sync {
		return Frame{}, ErrSync
	}
	f := Frame{Addr: b[1], Cmd1: b[2], Cmd2: b[3], Data1: b[4], Data2: b[5], Proto: ProtocolD}
	if got := f.checksumD(); got != b[6] {
		return Frame{}, fmt.Errorf("%w: got %#02x want %#02x", ErrChecksum, b[6], got)
	}
	return f, nil
}

// ParseP validates and decodes a single 8-byte Pelco-P frame. Both framing
// bytes are checked: byte 6 must be ETX, not just byte 0 STX — the checksum is
// computed from the ETX constant, so without the wire-byte check a stray 0xA0
// landing just before a response whose payload XORs to zero would pass
// validation and swallow the real frame.
func ParseP(b []byte) (Frame, error) {
	if len(b) != PFrameLen {
		return Frame{}, ErrLength
	}
	if b[0] != STX || b[6] != ETX {
		return Frame{}, ErrSync
	}
	f := Frame{Addr: b[1], Cmd1: b[2], Cmd2: b[3], Data1: b[4], Data2: b[5], Proto: ProtocolP}
	if got := f.checksumP(); got != b[7] {
		return Frame{}, fmt.Errorf("%w: got %#02x want %#02x", ErrChecksum, b[7], got)
	}
	return f, nil
}

// ParseAny decodes one frame from the head of b, dispatching on the first
// byte: 0xA0 → Pelco-P (8 bytes), anything else → Pelco-D (7 bytes, checked
// against Sync). It returns the number of bytes the frame occupies so a
// framing loop can consume exactly that much on success. When b holds fewer
// bytes than the protocol's frame length, it returns (need, ErrLength) with
// need = the number of bytes required — a stream reader should wait for more
// bytes rather than treat this as a framing error. Any other error means the
// bytes are a false start; the caller should drop one byte and rescan.
func ParseAny(b []byte) (f Frame, need int, err error) {
	if len(b) == 0 {
		return Frame{}, FrameLen, ErrLength
	}
	if b[0] == STX {
		if len(b) < PFrameLen {
			return Frame{}, PFrameLen, ErrLength
		}
		f, err = ParseP(b[:PFrameLen])
		return f, PFrameLen, err
	}
	if len(b) < FrameLen {
		return Frame{}, FrameLen, ErrLength
	}
	f, err = Parse(b[:FrameLen])
	return f, FrameLen, err
}
