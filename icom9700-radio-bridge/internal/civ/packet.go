// Package civ implements the IC-9700's RS-BA1-style CI-V-over-LAN transport:
// the 16-byte packet header layer, the are-you-there/login/token/stream
// handshake, per-stream sequence tracking with retransmit requests,
// keepalives and pings, the 60 s token renewal, the CI-V data watchdog and
// the clean disconnect.
//
// Protocol constants and the connection sequence are pinned by the committed
// protocol brief (docs/civ-research-brief.md in this module), which ports the
// wfview icomudpbase/icomudphandler/icomudpcivdata and kappanhang behavior.
// This implementation does not re-derive protocol facts.
//
// NEVER-LOG PIN (plan R17): credential-bearing packets — login, token
// confirm/renewal/removal and the stream request, which carries the token —
// are never logged at any level, in any form. The credential substitution
// table below is public (it ships in wfview's source), so captured wire
// bytes ARE the password; only packet types, lengths and counts may reach
// slog. transport_test.go mechanizes this pin against a debug-level capture.
package civ

import "encoding/binary"

// UDP ports of the radio's three streams (brief, "Ports and streams"). The
// audio port is never opened by this bridge (KTD-9: a future sidecar plugs
// into the same stream machinery).
const (
	ControlPort = 50001
	SerialPort  = 50002
	AudioPort   = 50003
)

// 16-byte packet header (brief, "Packet layer"): a u32 little-endian total
// length, a u16 little-endian type, a u16 little-endian sequence, then the
// 4-byte sent and received IDs. Header field offsets:
const (
	headerLen = 16
	lenOff    = 0 // u32 LE total packet length (header + payload)
	typeOff   = 4 // u16 LE packet type
	seqOff    = 6 // u16 LE tracked sequence number
	sentIDOff = 8 // 4 bytes, sentid
	rcvdIDOff = 12

	replyOff    = 0x10 // ping reply flag / civ sub-header reply byte
	pingTimeOff = 0x11 // u32 LE ping time (radio uptime in requests)
)

// Fixed packet lengths (wfview packettypes.h, matching the brief).
const (
	ctrlLen      = 0x10 // control: handshake, idle, disconnect, retransmit-single
	pingLen      = 0x15 // ping; also the CI-V sub-header size (civHeaderLen)
	openCloseLen = 0x16 // CI-V stream open/close
	bulkRetxBase = 0x10 // bulk retransmit request: base + 4 bytes per range pair
	tokenLen     = 0x40
	statusLen    = 0x50
	loginRespLen = 0x60
	loginLen     = 0x80
	streamReqLen = 0x90
)

// Packet types (u16 LE at typeOff). Auth-family packets (login, token,
// stream request and all their responses) ride as type 0x00 tracked packets
// distinguished by their length; 0x01 is a retransmit request on any stream.
const (
	ptIdle        = 0x00 // tracked idle/data
	ptRetransmit  = 0x01 // retransmit request (single or bulk)
	ptAreYouThere = 0x03
	ptIAmHere     = 0x04
	ptLogout      = 0x05 // disconnect
	ptAreYouReady = 0x06
	ptIAmReady    = 0x06 // same wire type; direction distinguishes
	ptPing        = 0x07
)

// Auth request/reply sub-types (byte at reqTypeOff / reqReplyOff).
const (
	reqReplyRequest = 0x01 // we ask
	reqReplyReply   = 0x02 // radio answers

	reqTypeLogin     = 0x00 // login packet
	reqTypeRemoval   = 0x01 // token removal (clean disconnect)
	reqTypeStreamReq = 0x03 // stream request
	reqTypeConfirm   = 0x02 // token confirm (right after login)
	reqTypeRenewal   = 0x05 // token renewal (every 60 s)
)

// Auth-family payload offsets (after the 16-byte header).
const (
	payloadOff        = 0x10 // u32 BE payload size
	reqReplyOff       = 0x14 // u8 requestreply
	reqTypeOff        = 0x15 // u8 requesttype
	innerSeqOff       = 0x16 // u16 BE auth inner sequence
	tokReqOff         = 0x1a // u16 LE token request (login echo-match)
	tokenOff          = 0x1c // u32 LE session token
	userOff           = 0x40 // login: substituted username[16]; stream request: username[16]
	passOff           = 0x50 // login: substituted password[16]
	nameOff           = 0x60 // login: client name[16]
	errOff            = 0x30 // u32 LE error/response field
	connTypeOff       = 0x40 // login response: connection type string ("FTTH")
	discOff           = 0x40 // status: radio-disconnected flag
	civPortOff        = 0x42 // status: u16 BE radio-side CI-V port (brief-pinned)
	audioPortOff      = 0x46 // status: u16 BE radio-side audio port (unused, v1)
	civLocalPortOff   = 0x7c // stream request: u32 BE chosen local CI-V port
	audioLocalPortOff = 0x80 // stream request: u32 BE chosen local audio port
	resetCapOff       = 0x24 // token packet: u16 BE capability 0x0798
)

// Openclose (CI-V stream open/close) fields.
const (
	dataFieldOff     = 0x10 // u16 LE 0x01c0 on open
	openCloseSeqOff  = 0x13 // u16 BE sub-header send sequence
	magicOff         = 0x15 // 0x04 open, 0x00 close
	openCloseMagic   = 0x04
	closeMagic       = 0x00
	opencloseDataVal = 0x01c0
)

// CI-V data sub-header (0x15 bytes; brief: reply=0xc1, datalen, big-endian
// send-seq, then raw CI-V). Frames are framed by datalen, never by scanning
// for 0xFD — payloads can embed it (KTD-10).
const (
	civHeaderLen  = 0x15
	civReplyOff   = 0x10
	civDatalenOff = 0x11 // u16 LE payload length
	civSendSeqOff = 0x13 // u16 BE send sequence
	civReplyByte  = 0xc1
)

// Error/response values carried in the u32 at errOff.
const (
	errRefused = 0xffffffff // status packet: connection refused (stale/other session)
	errAuthBad = 0xfffffffe // login response: invalid username/password
	respOK     = 0x00000000 // token renewal accepted
	discFlag   = 0x01       // status disc byte: radio disconnected us
)

// header is the parsed 16-byte packet header.
type header struct {
	len    uint32
	typ    uint16
	seq    uint16
	sentID uint32
	rcvdID uint32
}

func parseHeader(b []byte) header {
	return header{
		len:    binary.LittleEndian.Uint32(b),
		typ:    binary.LittleEndian.Uint16(b[typeOff:]),
		seq:    binary.LittleEndian.Uint16(b[seqOff:]),
		sentID: binary.BigEndian.Uint32(b[sentIDOff:]),
		rcvdID: binary.BigEndian.Uint32(b[rcvdIDOff:]),
	}
}

// putHeader writes h into the first 16 bytes of b. IDs travel big-endian
// (kappanhang/wfview byte order on the wire).
func putHeader(b []byte, h header) {
	binary.LittleEndian.PutUint32(b, h.len)
	binary.LittleEndian.PutUint16(b[typeOff:], h.typ)
	binary.LittleEndian.PutUint16(b[seqOff:], h.seq)
	binary.BigEndian.PutUint32(b[sentIDOff:], h.sentID)
	binary.BigEndian.PutUint32(b[rcvdIDOff:], h.rcvdID)
}

func putLE32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
func putBE32(b []byte, off int, v uint32) { binary.BigEndian.PutUint32(b[off:], v) }
func putBE16(b []byte, off int, v uint16) { binary.BigEndian.PutUint16(b[off:], v) }

// controlPacket builds a 16-byte control packet (handshake, idle,
// disconnect, single retransmit request).
func controlPacket(typ byte, seq uint16, sentID, rcvdID uint32) []byte {
	b := make([]byte, ctrlLen)
	putHeader(b, header{len: ctrlLen, typ: uint16(typ), seq: seq, sentID: sentID, rcvdID: rcvdID})
	return b
}

// pingPacket builds a 21-byte ping (type 0x07). reply is 0 for a request and
// 1 for an answer; timeMS is the uptime/time-of-day field either way.
func pingPacket(reply byte, seq uint16, timeMS uint32, sentID, rcvdID uint32) []byte {
	b := make([]byte, pingLen)
	putHeader(b, header{len: pingLen, typ: ptPing, seq: seq, sentID: sentID, rcvdID: rcvdID})
	b[replyOff] = reply
	binary.LittleEndian.PutUint32(b[pingTimeOff:], timeMS)
	return b
}

// openclosePacket builds the CI-V stream open/close packet (0x16 bytes,
// data 0x01c0, magic 0x04 open / 0x00 close; brief, connection step 6).
func openclosePacket(closeIt bool, subSeq uint16, sentID, rcvdID uint32) []byte {
	b := make([]byte, openCloseLen)
	putHeader(b, header{len: openCloseLen, sentID: sentID, rcvdID: rcvdID})
	binary.LittleEndian.PutUint16(b[dataFieldOff:], opencloseDataVal)
	binary.BigEndian.PutUint16(b[openCloseSeqOff:], subSeq)
	if closeIt {
		b[magicOff] = closeMagic
	} else {
		b[magicOff] = openCloseMagic
	}
	return b
}

// civDataPacket builds a CI-V data datagram: 16-byte header, 0x15-byte
// sub-header (reply 0xc1, u16 LE datalen, u16 BE send sequence) then the raw
// CI-V bytes. The tracked header sequence is stamped by sendTracked.
func civDataPacket(payload []byte, subSeq uint16, sentID, rcvdID uint32) []byte {
	b := make([]byte, civHeaderLen+len(payload))
	putHeader(b, header{len: uint32(len(b)), sentID: sentID, rcvdID: rcvdID})
	b[civReplyOff] = civReplyByte
	binary.LittleEndian.PutUint16(b[civDatalenOff:], uint16(len(payload)))
	binary.BigEndian.PutUint16(b[civSendSeqOff:], subSeq)
	copy(b[civHeaderLen:], payload)
	return b
}

// bulkRetransmitPacket builds a multi-packet retransmit request: a 16-byte
// header (type 0x01) followed by one (start,end) u16 LE pair per requested
// sequence — wfview duplicates each missing seq into a degenerate range.
func bulkRetransmitPacket(seqs []uint16, sentID, rcvdID uint32) []byte {
	b := make([]byte, bulkRetxBase+4*len(seqs))
	putHeader(b, header{len: uint32(len(b)), typ: ptRetransmit, sentID: sentID, rcvdID: rcvdID})
	for i, s := range seqs {
		off := bulkRetxBase + 4*i
		binary.LittleEndian.PutUint16(b[off:], s)
		binary.LittleEndian.PutUint16(b[off+2:], s)
	}
	return b
}

// loginPacket builds the 0x80 login packet: substituted credentials at
// 0x40/0x50, client name at 0x60 (brief, connection step 3). tokReq and
// innerSeq are the auth bookkeeping the radio echoes back; token is always 0
// here (no token exists yet).
func loginPacket(username, password, clientName [16]byte, tokReq uint16, innerSeq uint16, sentID, rcvdID uint32) []byte {
	b := make([]byte, loginLen)
	putHeader(b, header{len: loginLen, sentID: sentID, rcvdID: rcvdID})
	putBE32(b, payloadOff, loginLen-headerLen)
	b[reqReplyOff] = reqReplyRequest
	b[reqTypeOff] = reqTypeLogin
	putBE16(b, innerSeqOff, innerSeq)
	binary.LittleEndian.PutUint16(b[tokReqOff:], tokReq)
	copy(b[userOff:], username[:])
	copy(b[passOff:], password[:])
	copy(b[nameOff:], clientName[:])
	return b
}

// tokenPacket builds the 0x40 token packet. reqType is one of reqTypeConfirm
// (0x02, right after login), reqTypeRenewal (0x05, every 60 s) or
// reqTypeRemoval (0x01, clean disconnect).
func tokenPacket(reqType byte, tokReq uint16, token uint32, innerSeq uint16, sentID, rcvdID uint32) []byte {
	b := make([]byte, tokenLen)
	putHeader(b, header{len: tokenLen, sentID: sentID, rcvdID: rcvdID})
	putBE32(b, payloadOff, tokenLen-headerLen)
	b[reqReplyOff] = reqReplyRequest
	b[reqTypeOff] = reqType
	putBE16(b, innerSeqOff, innerSeq)
	binary.LittleEndian.PutUint16(b[tokReqOff:], tokReq)
	binary.LittleEndian.PutUint32(b[tokenOff:], token)
	putBE16(b, resetCapOff, 0x0798) // wfview's capability field
	return b
}

// streamRequestPacket builds the 0x90 stream-request packet carrying the
// client's chosen local CI-V (and audio) ports big-endian (brief, connection
// step 5). Control-only build: rx enabled, tx/audio disabled.
func streamRequestPacket(civLocalPort, audioLocalPort int, tokReq uint16, token uint32, innerSeq uint16, username [16]byte, sentID, rcvdID uint32) []byte {
	b := make([]byte, streamReqLen)
	putHeader(b, header{len: streamReqLen, sentID: sentID, rcvdID: rcvdID})
	putBE32(b, payloadOff, streamReqLen-headerLen)
	b[reqReplyOff] = reqReplyRequest
	b[reqTypeOff] = reqTypeStreamReq
	putBE16(b, innerSeqOff, innerSeq)
	binary.LittleEndian.PutUint16(b[tokReqOff:], tokReq)
	binary.LittleEndian.PutUint32(b[tokenOff:], token)
	copy(b[userOff:], username[:])
	b[0x70] = 0x01 // rx enable
	b[0x71] = 0x00 // tx enable — control-only v1 (KTD-9)
	b[0x72] = 0x04 // rx codec LPCM16
	b[0x73] = 0x00 // tx codec none
	putBE32(b, 0x74, 48000)
	putBE32(b, 0x78, 48000)
	putBE32(b, civLocalPortOff, uint32(civLocalPort))
	putBE32(b, audioLocalPortOff, uint32(audioLocalPort))
	putBE32(b, 0x84, 500) // tx buffer ms
	b[0x88] = 0x01        // convert
	return b
}

// status is the parsed 0x50 radio status packet (answer to the stream
// request): radio-side ports and the error field.
type status struct {
	civPort   int
	audioPort int
	err       uint32
	disc      bool
}

// parseStatus parses a 0x50 status packet. The CI-V port sits big-endian at
// 0x42, audio at 0x46 (brief-pinned); error 0xffffffff means the connection
// was refused (stale/other session).
func parseStatus(b []byte) status {
	return status{
		err:       binary.LittleEndian.Uint32(b[errOff:]),
		disc:      b[discOff] == discFlag,
		civPort:   int(binary.BigEndian.Uint16(b[civPortOff:])),
		audioPort: int(binary.BigEndian.Uint16(b[audioPortOff:])),
	}
}

// substitution table for login credentials — the public wfview/kappanhang
// table, indexed by (byte + position) wrapped into 32..126. Obfuscation
// only: treat substituted bytes as plaintext-equivalent (never-log pin).
var substitution = [127]byte{
	32: 0x47, 33: 0x5d, 34: 0x4c, 35: 0x42, 36: 0x66, 37: 0x20, 38: 0x23,
	39: 0x46, 40: 0x4e, 41: 0x57, 42: 0x45, 43: 0x3d, 44: 0x67, 45: 0x76,
	46: 0x60, 47: 0x41, 48: 0x62, 49: 0x39, 50: 0x59, 51: 0x2d, 52: 0x68,
	53: 0x7e, 54: 0x7c, 55: 0x65, 56: 0x7d, 57: 0x49, 58: 0x29, 59: 0x72,
	60: 0x73, 61: 0x78, 62: 0x21, 63: 0x6e, 64: 0x5a, 65: 0x5e, 66: 0x4a,
	67: 0x3e, 68: 0x71, 69: 0x2c, 70: 0x2a, 71: 0x54, 72: 0x3c, 73: 0x3a,
	74: 0x63, 75: 0x4f, 76: 0x43, 77: 0x75, 78: 0x27, 79: 0x79, 80: 0x5b,
	81: 0x35, 82: 0x70, 83: 0x48, 84: 0x6b, 85: 0x56, 86: 0x6f, 87: 0x34,
	88: 0x32, 89: 0x6c, 90: 0x30, 91: 0x61, 92: 0x6d, 93: 0x7b, 94: 0x2f,
	95: 0x4b, 96: 0x64, 97: 0x38, 98: 0x2b, 99: 0x2e, 100: 0x50, 101: 0x40,
	102: 0x3f, 103: 0x55, 104: 0x33, 105: 0x37, 106: 0x25, 107: 0x77, 108: 0x24,
	109: 0x26, 110: 0x74, 111: 0x6a, 112: 0x28, 113: 0x53, 114: 0x4d, 115: 0x69,
	116: 0x22, 117: 0x5c, 118: 0x44, 119: 0x31, 120: 0x36, 121: 0x58, 122: 0x3b,
	123: 0x7a, 124: 0x51, 125: 0x5f, 126: 0x52,
}

// passcode maps a credential into the 16-byte field the login packet
// carries: each byte is shifted by its position, wrapped into 32..126 and
// substituted through the public table; the remainder is zero-padded.
func passcode(s string) [16]byte {
	var out [16]byte
	for i := 0; i < len(s) && i < len(out); i++ {
		p := int(s[i]) + i
		if p > 126 {
			p = 32 + p%127
		}
		out[i] = substitution[p]
	}
	return out
}

// field16 copies s into a zero-padded 16-byte packet field.
func field16(s string) [16]byte {
	var out [16]byte
	copy(out[:], s)
	return out
}
