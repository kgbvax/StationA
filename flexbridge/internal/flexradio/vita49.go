package flexradio

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// VITA-49 (VRT — VITA Radio Transport) context / stream packets.
//
// FlexRadio 6000-series radios stream real-time meter data as VITA-49
// UDP datagrams once a client has told the radio (via the TCP API) which
// UDP port to stream to. The packet header is a fixed set of 32-bit words;
// for the meter stream the payload is a sequence of (uint16 meter_id,
// int16 raw_value) pairs.
//
// Reference: VITA 49.0-2007 plus FlexRadio API wiki "Metering Protocol".
// The conversion math for the raw_value field lives in meters.go.
//
// Word0 bit layout (big-endian):
//
//	bits 31..30  T   (packet type: 0=data, ...)
//	bit  29      C   (class id present)
//	bit  28      Tr  (trailer present)
//	bits 27..26  TSI (timestamp integer mode)
//	bits 25..24  TSF (timestamp fractional mode)
//	bits 23..0   packet count

const (
	vitaTypeIFData        uint32 = 0x0 // IF data without context (meter stream)
	vitaTypeContextPacket uint32 = 0x3
)

// Class id used by the FlexRadio meter stream. The lower 32 bits of the
// second class-id word reduce (after masking) to 0x8002 for meter packets.
const meterClassIDLow uint16 = 0x8002

// VITAPacket is a decoded VITA-49 datagram.
type VITAPacket struct {
	Word0        uint32 // raw header word0 (diagnostics)
	PacketType   uint32 // word0 bits 30..31
	ClassIDLow   uint16 // second class-id word's low 16 bits (0x8002 = meters)
	HasClass     bool
	HasTrailer   bool
	HasTS        bool
	IntegerTS    uint32 // integer (coarse) timestamp, when HasTS & TSI!=0
	FractionalTS uint64 // fractional timestamp (64-bit), when HasTS & TSF!=0
	Payload      []byte // bytes between header/ts and optional trailer
	Trailer      uint32
}

// MeterReading is a single decoded (index, value) pair from a meter packet.
type MeterReading struct {
	Index uint16
	Raw   int16
}

// ErrShortPacket is returned when a datagram is too small for what its
// header claims.
var ErrShortPacket = errors.New("flexradio: VITA-49 packet too short")

// ErrNotMeterPacket is returned by MeterReadings when the packet is not a
// FlexRadio meter stream packet (wrong class id).
var ErrNotMeterPacket = errors.New("flexradio: not a FlexRadio meter packet")

// ParseVITA49 decodes a VITA-49 datagram into its structural parts.
//
// FlexRadio's meter/spectrum packets use a 3-word class identifier after the
// header word (observed on SmartSDR v3/v4):
//
//	word0: header (T/C/Tr/TSI/TSF bits)
//	word1: class id word 1 (OUI / packet count)
//	word2: class id word 2 (info high)
//	word3: class id word 3 (info low)  -- low 16 bits = stream discriminator
//	         0x8002 = meter stream, 0x8003 = spectrum/display, ...
//	[timestamp: TSI=00 -> none; TSF=11 -> 64-bit sample timestamp (8 bytes)]
//	[payload]
//	[trailer: 4 bytes if Tr=1]
//
// Note: word1..word3 are 3 words (12 bytes), so the discriminator is at
// b[12:16] (low 16 bits). Earlier revisions read only 2 class-id words and
// so mis-read the class id, rejecting every real meter packet.
func ParseVITA49(b []byte) (VITAPacket, error) {
	if len(b) < 4 { // at least the header word
		return VITAPacket{}, ErrShortPacket
	}
	w0 := binary.BigEndian.Uint32(b[0:4])

	p := VITAPacket{
		Word0:      w0,
		PacketType: (w0 >> 30) & 0x3,
		HasClass:   (w0>>29)&0x1 == 1,
		HasTrailer: (w0>>28)&0x1 == 1,
	}
	tsi := (w0 >> 26) & 0x3
	tsf := (w0 >> 24) & 0x3
	p.HasTS = tsi != 0 || tsf != 0

	off := 4
	if p.HasClass {
		// 3-word class id: word1 (OUI) + word2 (info hi) + word3 (info lo).
		// word3's low 16 bits are the stream discriminator (0x8002 = meters).
		if off+12 > len(b) {
			return p, ErrShortPacket
		}
		p.ClassIDLow = uint16(binary.BigEndian.Uint32(b[off+8 : off+12]))
		off += 12
	}

	if p.HasTS {
		if tsi != 0 {
			if off+4 > len(b) {
				return p, ErrShortPacket
			}
			p.IntegerTS = binary.BigEndian.Uint32(b[off : off+4])
			off += 4
		}
		if tsf != 0 {
			// FlexRadio sets TSF=11 (sample timestamp). On the wire (observed
			// on SmartSDR v3/v4) this occupies 12 bytes, not the 8 the VITA-49
			// spec describes: a 64-bit fractional timestamp followed by a
			// 32-bit sample/sequence word. We consume all 12 so the meter
			// payload starts at the right offset.
			if off+12 > len(b) {
				return p, ErrShortPacket
			}
			p.FractionalTS = binary.BigEndian.Uint64(b[off : off+8])
			off += 12
		}
	}

	// Remaining bytes are payload (+ optional trailer at the end).
	rest := b[off:]
	if p.HasTrailer {
		if len(rest) < 4 {
			return p, ErrShortPacket
		}
		p.Trailer = binary.BigEndian.Uint32(rest[len(rest)-4:])
		rest = rest[:len(rest)-4]
	}
	p.Payload = rest
	return p, nil
}

// MeterReadings decodes the meter payload of a FlexRadio VITA-49 packet.
// Returns ErrNotMeterPacket if this datagram is not a meter stream packet.
//
// Payload format (per AetherSDR MeterModel.h, sourced from FlexLib):
// a sequence of N pairs of (uint16 meter_id, int16 raw_value) in
// big-endian byte order. N = len(payload)/4.
func (p VITAPacket) MeterReadings() ([]MeterReading, error) {
	// Gate primarily on the class id. Some firmware variants send slightly
	// different packet types, so we trust the class id discriminator.
	if p.HasClass && p.ClassIDLow != meterClassIDLow {
		return nil, ErrNotMeterPacket
	}
	if len(p.Payload)%4 != 0 {
		return nil, fmt.Errorf("flexradio: meter payload length %d not a multiple of 4", len(p.Payload))
	}
	n := len(p.Payload) / 4
	out := make([]MeterReading, n)
	for i := 0; i < n; i++ {
		o := i * 4
		out[i] = MeterReading{
			Index: binary.BigEndian.Uint16(p.Payload[o : o+2]),
			Raw:   int16(binary.BigEndian.Uint16(p.Payload[o+2 : o+4])),
		}
	}
	return out, nil
}
