// Package civ implements the Icom RS-BA1-style LAN protocol transport for the
// IC-9700: UDP :50001 control (are-you-there handshake, login with
// substitution-table-obfuscated credentials, token renewal, stream request)
// and UDP :50002 CI-V data (raw `FE FE..FD` frames inside a 5-byte data
// sub-header), with sequence tracking and retransmit on both streams.
//
// The wire behavior is a faithful Go port of kappanhang (github.com/nonoo/
// kappanhang, the newest known-good client against current firmware) cross-
// checked against wfview's icomudp* sources; the byte-level facts follow
// docs/civ-research-brief.md. Deviations are marked in comments where they
// exist (ping cadence, the CI-V silence watchdog).
//
// SECURITY: the login/auth packet payload carries the username and password
// under a public 1-byte substitution table — treat those bytes as
// plaintext-equivalent. Credential-bearing buffers are never logged at any
// level; callers must not feed them to t.Log or slog.
package civ

import (
	"encoding/binary"
	"errors"
)

// Radio-side UDP ports. The protocol has no discovery or broadcast: the
// client must know the radio address, and the data ports are fixed by the
// protocol (kappanhang binds its local sockets to the SAME port numbers it
// dials, so all streams are port-50001/50002 to port-50001/50002).
const (
	ControlPort = 50001
	CIVDataPort = 50002

	// AudioPort is announced in the request-stream packet (the wire field
	// exists), but v1 never opens the audio socket: audio datagrams the
	// radio may send there find no listener, which is harmless — the radio
	// does not require audio acks (wfview/kappanhang open it only to play
	// audio, which this bridge never consumes; KTD-9 leaves the stream
	// concept open for a future sidecar).
	AudioPort = 50003
)

// headerLen is the fixed 16-byte packet header shared by every stream:
//
//	[0]     length byte (payload semantics per packet family — for tracked
//	        data packets it is 0x15+len(data); for fixed families it equals
//	        the total datagram size)
//	[1..5]  family signature (dispatch prefix, little-endian type field)
//	[6..7]  stream sequence number, little-endian — meaningful only on
//	        tracked packets; handshake one-shots leave it 0
//	[8..11] local session ID (client), big-endian
//	[12..15] remote session ID (radio), big-endian
const headerLen = 16

// Family signatures (bytes [1..5], with [0] and [6..] noted).
var (
	// sigIdle is the untracked control idle (keepalive) packet: 16 bytes,
	// seq 0.
	sigIdle = []byte{0x10, 0x00, 0x00, 0x00, 0x00, 0x00}
	// sigRetransmitSingle requests retransmission of one packet; the seq
	// follows at [6..7] little-endian.
	sigRetransmitSingle = []byte{0x10, 0x00, 0x00, 0x00, 0x01, 0x00}
	// sigRetransmitRange requests a range list; pairs of LE start/end seqs
	// start at [16].
	sigRetransmitRange = []byte{0x18, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00}
	// sigAreYouThere / sigIAmHere / sigDisconnect / sigReady are the
	// handshake one-shots (pkt3/pkt4/pkt5/pkt6); [6..7] is 00 00 except
	// sigReady which carries 01 00.
	sigAreYouThere = []byte{0x10, 0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00}
	sigIAmHere     = []byte{0x10, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00}
	sigDisconnect  = []byte{0x10, 0x00, 0x00, 0x00, 0x05, 0x00, 0x00, 0x00}
	sigReady       = []byte{0x10, 0x00, 0x00, 0x00, 0x06, 0x00, 0x01, 0x00}
)

// Data-stream framing: a CI-V payload rides behind a 5-byte sub-header at
// offset 16 — reply flag 0xc1, payload length, 0x00, and the sender's inner
// sequence number big-endian — putting the raw CI-V bytes at offset 21
// (0x15). The length byte of a data packet is 0x15+len(payload), and the
// radio's datalen field is authoritative for parsing: CI-V payloads can
// embed the 0xFD end marker, so frames are NEVER found by scanning (KTD-10).
const (
	dataReplyFlag    = 0xc1
	dataSubHeaderLen = 5
	dataHeaderLen    = headerLen + dataSubHeaderLen // 21
)

// Ping (pkt7): 21 bytes — [4] family 0x07, [6..7] seq LE, [16] reply flag
// (0x00 request / 0x01 reply), [17..20] 4-byte ping ID echoed on reply. The
// length byte is 0x15 from the client; the radio's requests sometimes carry
// 0x00 there, so dispatch ignores byte [0] (kappanhang pkt7.isPkt7).
const (
	pingLen      = 21
	pingFamily   = 0x07
	pingRequest  = 0x00
	pingReplyFlg = 0x01
)

// Auth-family datagrams (login 0x80, auth 0x40, request-stream 0x90 and
// their answers 0x60/0xa8/0x50) dispatch on (length byte, first bytes).
var (
	sigLoginAnswer = []byte{0x60, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00}
	sigAuthAnswer  = []byte{0x40, 0x00, 0x00, 0x00, 0x00, 0x00}
	// sigRequestAnswer matches the client's 0x90 stream-request datagram.
	sigRequestAnswer = []byte{0x90, 0x00, 0x00, 0x00, 0x00, 0x00}
	// sigStatus is the 0x50 status packet: the stream-request answer
	// (error 0 = granted) and, mid-session, refusal/disconnect reports.
	sigStatus  = []byte{0x50, 0x00, 0x00, 0x00, 0x00, 0x00}
	sigA8Reply = []byte{0xa8, 0x00, 0x00, 0x00, 0x00, 0x00}
	// sigBusyReject matches a 20-byte control packet real IC-9700 firmware
	// answered to our kappanhang-shaped login on every attempt (bench
	// 2026-09-20: len 0x14, type 0x0001, body 81 ff ff ff) — a stale-
	// tracked-packet rejection, since the wire format now follows wfview
	// exactly. Undocumented in wfview/kappanhang — their type-0x0001 forms
	// are the 0x10 single / 0x18 range retransmit requests, never 0x14.
	sigBusyReject = []byte{0x14, 0x00, 0x00, 0x00, 0x01, 0x00}
	// sigBusyBody is the reject body at offset 0x10 (LE 0xffffff81).
	sigBusyBody = []byte{0x81, 0xff, 0xff, 0xff}
)

// Auth-family field offsets (wfview packettypes.h). The auth family
// follows wfview's wire format — see the authState comment in token.go.
const (
	// authSeqInit is wfview's starting inner sequence for the login/auth
	// family (uint16_t authSeq = 0x30), sent big-endian at 0x16.
	authSeqInit = 0x30
	// loginErrOffset holds ff ff ff fe on a login answer when the
	// username/password is wrong; on the 0x50 status answer 0 means
	// granted and 0xffffffff refused.
	loginErrOffset = 48
	// radioDisconnectedOffset (0x40) carries 0x01 when the radio says the
	// session went away (0x50, error 0).
	radioDisconnectedOffset = 64
	// tokRequestOffset / tokenOffset on the login answer (and the same
	// fields in every auth-family request): the 2-byte tokrequest echo and
	// the 4-byte session token.
	tokRequestOffset = 0x1a
	tokenOffset      = 0x1c
	tokenLen         = 4
	// authResponseOffset on the 0x40 auth answer carries the 4-byte
	// response code (0 = ok, 0xffffffff = rejected).
	authResponseOffset = 48
	// requestOKFlag is 0x01 on a successful 144-byte 0x90 stream-request
	// answer (kappanhang-style firmware; the 0x50 status is the wfview form).
	requestOKFlag = 96
	// a8NameOffset carries the first radio name in the volunteered 0xa8
	// capabilities packet (0x42 header + name at +0x10 in each 0x66 entry).
	a8NameOffset = 0x42 + 0x10
)

// Errors surfaced by the handshake. All are observed wire facts (the plan's
// error taxonomy carries them verbatim into /state.error upstream).
var (
	// ErrLoginRejected is the radio's explicit ff ff ff fe login answer —
	// wrong username or password.
	ErrLoginRejected = errors.New("civ: login refused (invalid username/password)")
	// ErrLoginBusy is the 20-byte 81 ff ff ff packet the radio answers to a
	// login while its single LAN session is held (manual wfview, or a
	// stale session left by a crashed client until the radio's reaper
	// clears it — bench 2026-09-20). Surfaced verbatim, never retried
	// silently.
	ErrLoginBusy = errors.New("civ: login refused — radio LAN session held (wfview or stale session)")
	// ErrConnectionRefused is the 0x50 ff ff ff answer: the radio refused
	// the session (a stale or other client's session may need a radio
	// reboot to clear — research brief, deploy gate 5).
	ErrConnectionRefused = errors.New("civ: connection refused by radio")
	// ErrHandshakeTimeout is the per-step expect timeout: the radio is
	// unreachable, off, or not answering.
	ErrHandshakeTimeout = errors.New("civ: handshake timeout")
)

// header builds the 16-byte header for sig (6-byte family prefix + the two
// [6..7] bytes already part of sig for the handshake families).
func header(sig []byte, localSID, remoteSID uint32) []byte {
	p := make([]byte, headerLen)
	copy(p, sig)
	binary.BigEndian.PutUint32(p[8:12], localSID)
	binary.BigEndian.PutUint32(p[12:16], remoteSID)
	return p
}

// isIdle reports whether r is a 16-byte idle (keepalive) packet.
func isIdle(r []byte) bool {
	return len(r) == headerLen && prefixEqual(r, sigIdle)
}

// isPing reports whether r is a ping packet (pkt7). The length byte varies
// on radio-originated requests, so it is ignored.
func isPing(r []byte) bool {
	return len(r) == pingLen && r[4] == pingFamily && r[5] == 0x00 &&
		r[1] == 0x00 && r[2] == 0x00 && r[3] == 0x00
}

// isData reports whether r is a tracked CI-V data packet: the length byte is
// 0x15+datalen and the sub-header reply flag is set. Real IC-9700 frames
// occasionally claim one byte more datalen than the datagram carries (bench
// 2026-09-20: datalen 8, payload 7 on the wire) — wfview clamps the payload
// to the datagram, and so do we, rather than dropping the frame (a strict
// length equality silently discarded EVERY radio frame in the first live
// bench). Idle packets on the data stream are NOT data (they still enter
// the receive reorder buffer to keep sequence continuity, like kappanhang).
func isData(r []byte) bool {
	return len(r) > dataHeaderLen && r[16] == dataReplyFlag
}

// dataSeq returns the packet's stream sequence number ([6..7] LE).
func dataSeq(r []byte) uint16 {
	if len(r) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint16(r[6:8])
}

// dataPayload strips the header + sub-header, returning the raw CI-V bytes.
// The datalen field is authoritative but clamped to the datagram (real
// firmware over-reports by one now and then — see isData).
// Caller guarantees isData.
func dataPayload(r []byte) []byte {
	n := int(r[17])
	if n > len(r)-dataHeaderLen {
		n = len(r) - dataHeaderLen
	}
	return r[dataHeaderLen : dataHeaderLen+n]
}

// buildData assembles a tracked CI-V data packet around payload with the
// stream's inner send sequence number.
func buildData(localSID, remoteSID uint32, innerSeq uint16, payload []byte) []byte {
	l := len(payload)
	p := make([]byte, dataHeaderLen+l)
	p[0] = byte(dataHeaderLen + l)
	binary.BigEndian.PutUint32(p[8:12], localSID)
	binary.BigEndian.PutUint32(p[12:16], remoteSID)
	p[16] = dataReplyFlag
	p[17] = byte(l)
	// [18] stays 0x00; inner sequence big-endian at [19..20].
	binary.BigEndian.PutUint16(p[19:21], innerSeq)
	copy(p[dataHeaderLen:], payload)
	return p
}

// buildPing assembles a ping. replyID==nil builds a request with a fresh
// 4-byte ID (counter + family tag, kappanhang's scheme); non-nil builds the
// reply echoing the radio's ID.
func buildPing(localSID, remoteSID uint32, seq uint16, replyID []byte) []byte {
	p := make([]byte, pingLen)
	p[0] = 0x15
	p[4] = pingFamily
	binary.LittleEndian.PutUint16(p[6:8], seq)
	binary.BigEndian.PutUint32(p[8:12], localSID)
	binary.BigEndian.PutUint32(p[12:16], remoteSID)
	if replyID == nil {
		p[16] = pingRequest
		// [17..20]: 4-byte ping ID; filled by the caller's counter.
	} else {
		p[16] = pingReplyFlg
		copy(p[17:21], replyID)
	}
	return p
}

// parsePingRequest extracts the reply ID from a radio ping request.
func parsePingRequest(r []byte) (seq uint16, replyID []byte) {
	return binary.LittleEndian.Uint16(r[6:8]), r[17:21]
}

// prefixEqual compares the leading len(sig) bytes.
func prefixEqual(r, sig []byte) bool {
	if len(r) < len(sig) {
		return false
	}
	for i, b := range sig {
		if r[i] != b {
			return false
		}
	}
	return true
}
