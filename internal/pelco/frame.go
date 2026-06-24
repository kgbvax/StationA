// Package pelco implements the Pelco-D protocol used to control PTZ units.
//
// A Pelco-D message is a fixed 7-byte frame:
//
//	FF | ADDR | CMD1 | CMD2 | DATA1 | DATA2 | CHECKSUM
//
// where CHECKSUM = (ADDR + CMD1 + CMD2 + DATA1 + DATA2) mod 256.
package pelco

import (
	"errors"
	"fmt"
)

// Sync is the byte that begins every Pelco-D frame.
const Sync = 0xFF

// FrameLen is the fixed length of a Pelco-D frame in bytes.
const FrameLen = 7

// Frame is a decoded Pelco-D message (without the sync byte or checksum, which
// are derived). The zero value is not a valid frame; use a command builder.
type Frame struct {
	Addr  byte
	Cmd1  byte
	Cmd2  byte
	Data1 byte
	Data2 byte
}

// checksum returns the Pelco-D checksum for the frame.
func (f Frame) checksum() byte {
	return f.Addr + f.Cmd1 + f.Cmd2 + f.Data1 + f.Data2
}

// Bytes encodes the frame into its 7-byte wire representation.
func (f Frame) Bytes() []byte {
	return []byte{Sync, f.Addr, f.Cmd1, f.Cmd2, f.Data1, f.Data2, f.checksum()}
}

// Word returns the 16-bit big-endian value carried in DATA1/DATA2. For
// position commands and query responses this is the angle in hundredths of a
// degree.
func (f Frame) Word() uint16 {
	return uint16(f.Data1)<<8 | uint16(f.Data2)
}

var (
	// ErrLength is returned when a buffer is not exactly FrameLen bytes.
	ErrLength = errors.New("pelco: frame must be 7 bytes")
	// ErrSync is returned when the first byte is not the sync byte.
	ErrSync = errors.New("pelco: missing 0xFF sync byte")
	// ErrChecksum is returned when the trailing checksum does not match.
	ErrChecksum = errors.New("pelco: checksum mismatch")
)

// Parse validates and decodes a single 7-byte frame.
func Parse(b []byte) (Frame, error) {
	if len(b) != FrameLen {
		return Frame{}, ErrLength
	}
	if b[0] != Sync {
		return Frame{}, ErrSync
	}
	f := Frame{Addr: b[1], Cmd1: b[2], Cmd2: b[3], Data1: b[4], Data2: b[5]}
	if got := f.checksum(); got != b[6] {
		return Frame{}, fmt.Errorf("%w: got %#02x want %#02x", ErrChecksum, b[6], got)
	}
	return f, nil
}
