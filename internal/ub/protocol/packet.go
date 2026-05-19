package protocol

import (
	"errors"
)

const (
	STX byte = 0xF5
	DLE byte = 0xF6
	ETX byte = 0xFA

	ChecksumSeed byte = 0x55
)

var (
	ErrPacketTooShort   = errors.New("packet too short")
	ErrChecksumMismatch = errors.New("packet checksum mismatch")
)

type Packet struct {
	Seq  byte
	Com  byte
	Data []byte
}

func EncodePacket(pkt Packet) []byte {
	payload := make([]byte, 0, 2+len(pkt.Data)+1)
	payload = append(payload, pkt.Seq, pkt.Com)
	payload = append(payload, pkt.Data...)
	payload = append(payload, checksum(payload))

	out := make([]byte, 0, 2+len(payload)*2)
	out = append(out, STX)
	for _, b := range payload {
		out = appendQuoted(out, b)
	}
	out = append(out, ETX)
	return out
}

func DecodeDecodedPacket(decoded []byte) (Packet, error) {
	if len(decoded) < 3 {
		return Packet{}, ErrPacketTooShort
	}

	calc := checksum(decoded[:len(decoded)-1])
	if calc != decoded[len(decoded)-1] {
		return Packet{}, ErrChecksumMismatch
	}

	pkt := Packet{
		Seq: decoded[0],
		Com: decoded[1],
	}
	if len(decoded) > 3 {
		pkt.Data = append([]byte(nil), decoded[2:len(decoded)-1]...)
	}
	return pkt, nil
}

func DecodeFramedBytes(raw []byte) (Packet, error) {
	decoded := make([]byte, 0, len(raw))
	escaped := false
	for _, b := range raw {
		if escaped {
			decoded = append(decoded, b|0x80)
			escaped = false
			continue
		}
		if b == DLE {
			escaped = true
			continue
		}
		decoded = append(decoded, b)
	}
	return DecodeDecodedPacket(decoded)
}

func appendQuoted(dst []byte, b byte) []byte {
	if b == STX || b == ETX || b == DLE {
		dst = append(dst, DLE)
		dst = append(dst, b&0x7F)
		return dst
	}
	return append(dst, b)
}

func checksum(data []byte) byte {
	chk := byte(ChecksumSeed)
	for _, b := range data {
		chk ^= b
		chk++
	}
	return chk
}
