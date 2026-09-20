package civ

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
)

// The credential substitution table (kappanhang passcode.go / wfview). It is
// PUBLIC — it provides no confidentiality; the obfuscated bytes are
// plaintext-equivalent and are never logged (package comment).
//
// Indexing: entry i maps the source byte (s[i]+i, wrapped into 32..126) to
// its wire byte. Credentials are zero-padded to 16 bytes on the wire.
var substTable = map[byte]byte{
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
	102: 0x3f, 103: 0x55, 104: 0x33, 105: 0x37, 106: 0x25, 107: 0x77,
	108: 0x24, 109: 0x26, 110: 0x74, 111: 0x6a, 112: 0x28, 113: 0x53,
	114: 0x4d, 115: 0x69, 116: 0x22, 117: 0x5c, 118: 0x44, 119: 0x31,
	120: 0x36, 121: 0x58, 122: 0x3b, 123: 0x7a, 124: 0x51, 125: 0x5f,
	126: 0x52,
}

// passcode encodes a credential into the 16-byte wire form. Bytes beyond the
// 16-byte field are dropped (kappanhang behavior; the radio's own fields are
// 16 wide).
func passcode(s string) []byte {
	res := make([]byte, 16)
	for i := 0; i < len(s) && i < len(res); i++ {
		p := int(s[i]) + i
		if p > 126 {
			p = 32 + p%127
		}
		res[i] = substTable[byte(p)]
	}
	return res
}

// The auth family follows WFVIEW's wire format, not kappanhang's. The bench
// against the real IC-9700 (2026-09-20) rejected the kappanhang-shaped login
// with a 20-byte `81 ff ff ff` packet on every attempt — on a freshly
// rebooted, provably idle radio with byte-verified-correct credentials —
// while wfview connects to the same radio. The differences that matter are
// all in the 0x10..0x1f window of the auth-family packets:
//
//   - the login/auth inner sequence is BIG-endian and starts at 0x30
//     (wfview authSeq = 0x30; kappanhang sends little-endian from 0);
//   - tokrequest rides at 0x1a (2 bytes; the radio echoes it) — kappanhang
//     puts random bytes at 0x19 instead;
//   - the session token (from the 0x60 answer at 0x1c) is echoed back in
//     the auth and request packets at 0x1c — kappanhang instead spreads a
//     6-byte authID taken from 0x1a across 0x19;
//   - the outer tracked sequence starts at 1 (wfview sendSeq = 1 — set on
//     the stream in dialStream; a seq-0 login reads as a stale packet).
//
// Only one immediate auth (magic 0x02) is sent after login; the 0x05
// renewal rides the periodic timer exactly like wfview's 60 s token timer.
type authState struct {
	authSeq    uint16 // auth-family inner sequence, BE, starts at 0x30
	tokRequest uint16 // random per attempt; the radio echoes it in answers
	token      uint32 // session token from the login answer (0x1c)
	gotToken   bool
	devName    string // radio self-description from the 0xa8 capabilities
}

func newAuthState() *authState {
	a := &authState{authSeq: authSeqInit}
	var b [2]byte
	if _, err := rand.Read(b[:]); err == nil {
		a.tokRequest = binary.LittleEndian.Uint16(b[:])
	}
	return a
}

// buildLogin assembles the 128-byte login datagram (wfview
// icomUdpHandler::sendLogin). SECURITY: the returned buffer carries the
// obfuscated credentials — never log it.
func (a *authState) buildLogin(localSID, remoteSID uint32, username, password string) ([]byte, error) {
	user := passcode(username)
	pass := passcode(password)
	p := header([]byte{0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, localSID, remoteSID)
	p = append(p,
		0x00, 0x00, 0x00, 0x70, // payloadsize (BE, wfview qToBigEndian)
		0x01,                    // requestreply
		0x00,                    // requesttype: login
		byte(a.authSeq>>8), byte(a.authSeq), // innerseq, BE
		0x00, 0x00, // unused
		byte(a.tokRequest), byte(a.tokRequest>>8)) // tokrequest, LE
	p = append(p, make([]byte, 36)...) // 0x1c..0x3f
	p = append(p, user...)             // 0x40
	p = append(p, pass...)             // 0x50
	p = append(p, clientName16()...)   // 0x60
	p = append(p, make([]byte, 16)...) // 0x70 padding
	a.authSeq++
	return p, nil
}

// buildAuth assembles the 64-byte token/auth datagram for the given magic
// (0x02 first auth after login, 0x05 renewal, 0x01 deauth on clean
// disconnect — wfview sendToken). Requires a parsed login answer (token).
func (a *authState) buildAuth(localSID, remoteSID uint32, magic byte) []byte {
	p := header([]byte{0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, localSID, remoteSID)
	p = append(p,
		0x00, 0x00, 0x00, 0x30, // payloadsize
		0x01,                    // requestreply
		magic,                   // requesttype: 0x02 first / 0x05 renewal / 0x01 deauth
		byte(a.authSeq>>8), byte(a.authSeq), // innerseq, BE
		0x00, 0x00,                      // unused
		byte(a.tokRequest), byte(a.tokRequest>>8), // tokrequest
		byte(a.token), byte(a.token>>8), byte(a.token>>16), byte(a.token>>24)) // token, LE
	p = append(p, make([]byte, 4)...)           // 0x20 authstartid (unused)
	p = append(p, 0x07, 0x98)                   // 0x24 resetcap (BE, wfview constant)
	p = append(p, make([]byte, 26)...)          // 0x26..0x3f
	a.authSeq++
	return p
}

// buildRequestStream assembles the 144-byte conninfo/stream-request datagram
// (wfview icomUdpHandler::sendRequestStream): asks the radio to open the
// CI-V data stream, announcing our local ports. txBufMs is the transmit
// buffer length the radio assumes for us, in milliseconds.
// SECURITY: carries the obfuscated username — never log the buffer.
func (a *authState) buildRequestStream(localSID, remoteSID uint32, username string, civPort, audioPort int, txBufMs uint16) []byte {
	p := header([]byte{0x90, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, localSID, remoteSID)
	p = append(p,
		0x00, 0x00, 0x00, 0x80, // payloadsize
		0x01, 0x03,              // requestreply / requesttype: stream request
		byte(a.authSeq>>8), byte(a.authSeq), // innerseq, BE
		0x00, 0x00,                // unused
		byte(a.tokRequest), byte(a.tokRequest>>8), // tokrequest
		byte(a.token), byte(a.token>>8), byte(a.token>>16), byte(a.token>>24)) // token, LE
	p = append(p, a.guidBytes()...)    // 0x20 client identity (GUID)
	p = append(p, make([]byte, 16)...) // 0x30
	p = append(p, clientName32()...)   // 0x40 client computer name
	p = append(p, passcode(username)...) // 0x60
	p = append(p,
		0x01,       // 0x70 rxenable
		0x01,       // 0x71 txenable
		0x04, 0x04, // 0x72 rxcodec / 0x73 txcodec
		0x00, 0x00, 0xBB, 0x80, // 0x74 rxsample (48000, BE)
		0x00, 0x00, 0xBB, 0x80, // 0x78 txsample (48000, BE)
		0x00, 0x00, byte(civPort>>8), byte(civPort), // 0x7c civport (BE u32)
		0x00, 0x00, byte(audioPort>>8), byte(audioPort), // 0x80 audioport (BE u32)
		0x00, 0x00, byte(txBufMs>>8), byte(txBufMs), // 0x84 txbuffer (BE u32)
		0x01) // 0x88 convert
	p = append(p, make([]byte, 7)...) // 0x89..0x8f
	a.authSeq++
	return p
}

// clientName16/32 are the fixed client computer-name fields ("icom-pc",
// kappanhang's constant — the radio only displays it).
func clientName16() []byte {
	b := make([]byte, 16)
	copy(b, "icom-pc")
	return b
}

func clientName32() []byte {
	b := make([]byte, 32)
	copy(b, "icom-pc")
	return b
}

// guidBytes is the stable 16-byte client identity. A constant (not random)
// so the radio's token pairing for this client survives restarts.
func (a *authState) guidBytes() []byte {
	b := make([]byte, 16)
	copy(b, "ICOM9700-BRIDGE1")
	return b
}

// parseLoginAnswer validates the 96-byte login answer: the tokrequest echo
// must match ours, ff ff ff fe at 0x30 is the explicit credential
// rejection, and the session token at 0x1c is captured for the auth family.
func (a *authState) parseLoginAnswer(r []byte) error {
	if !prefixEqual(r, sigLoginAnswer) {
		return ErrHandshakeTimeout
	}
	if len(r) > loginErrOffset+3 && r[loginErrOffset] == 0xff &&
		r[loginErrOffset+1] == 0xff && r[loginErrOffset+2] == 0xff && r[loginErrOffset+3] == 0xfe {
		return ErrLoginRejected
	}
	if len(r) < tokenOffset+tokenLen {
		return ErrHandshakeTimeout
	}
	if got := binary.LittleEndian.Uint16(r[tokRequestOffset : tokRequestOffset+2]); got != a.tokRequest {
		return fmt.Errorf("civ: login answer tokrequest mismatch (got 0x%04x, want 0x%04x)", got, a.tokRequest)
	}
	a.token = binary.LittleEndian.Uint32(r[tokenOffset : tokenOffset+tokenLen])
	a.gotToken = true
	return nil
}

// parseStatusAnswer classifies the 0x50 status answer: success (error 0),
// refused (0xffffffff), or unrecognized.
func parseStatusAnswer(r []byte) (ok, refused bool) {
	if !prefixEqual(r, sigStatus) || len(r) <= loginErrOffset+3 {
		return false, false
	}
	switch binary.LittleEndian.Uint32(r[loginErrOffset : loginErrOffset+4]) {
	case 0x00000000:
		return true, false
	case 0xffffffff:
		return false, true
	}
	return false, false
}

// statusRadiosDisconnected reports the 0x50 "radio disconnected" form
// (error 0 with the disc flag set).
func statusRadiosDisconnected(r []byte) bool {
	return len(r) > radioDisconnectedOffset &&
		binary.LittleEndian.Uint32(r[loginErrOffset:loginErrOffset+4]) == 0 &&
		r[radioDisconnectedOffset] == 0x01
}

// parseA8 captures the radio's self-description from its volunteered 0xa8
// capabilities packet: each 0x66-byte radio entry carries a 32-byte name at
// +0x10 after the 0x42-byte header.
func (a *authState) parseA8(r []byte) {
	if a.devName != "" {
		return
	}
	off := a8NameOffset
	if len(r) < off+32 {
		return
	}
	if name := parseNullTerminated(r[off : off+32]); name != "" {
		a.devName = name
	}
}

// authResponse extracts the 0x40 auth answer's response code (0x30).
func authResponse(r []byte) (uint32, bool) {
	if !prefixEqual(r, sigAuthAnswer) || len(r) < authResponseOffset+4 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(r[authResponseOffset : authResponseOffset+4]), true
}

func parseNullTerminated(b []byte) string {
	if i := indexZero(b); i >= 0 {
		return string(b[:i])
	}
	return strings.ToValidUTF8(string(b), "")
}

func indexZero(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}
