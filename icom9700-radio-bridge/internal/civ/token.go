package civ

import (
	"crypto/rand"
	"encoding/binary"
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

// auth state carried across the login/renewal exchange.
type authState struct {
	innerSendSeq uint16 // sequence of the auth-family inner packets
	authID       [6]byte
	gotAuthID    bool
	a8ReplyID    [a8ReplyIDLen]byte
}

// buildLogin assembles the 128-byte login datagram (kappanhang
// controlstream.sendPktLogin). authStartID is 2 random bytes per attempt.
// SECURITY: the returned buffer carries the obfuscated credentials — never
// log it.
func (a *authState) buildLogin(localSID, remoteSID uint32, username, password string) ([]byte, error) {
	var authStartID [2]byte
	if _, err := rand.Read(authStartID[:]); err != nil {
		return nil, err
	}
	user := passcode(username)
	pass := passcode(password)
	p := header([]byte{0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, localSID, remoteSID)
	p = append(p,
		0x00, 0x00, 0x00, 0x70, 0x01, 0x00, 0x00, byte(a.innerSendSeq),
		byte(a.innerSendSeq>>8), 0x00, authStartID[0], authStartID[1], 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	p = append(p, user...)
	p = append(p, pass...)
	p = append(p,
		0x69, 0x63, 0x6f, 0x6d, 0x2d, 0x70, 0x63, 0x00, // "icom-pc"
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	a.innerSendSeq++
	return p, nil
}

// buildAuth assembles the 64-byte auth datagram for the given magic
// (0x02 first auth after login, 0x05 second auth + renewals, 0x01 deauth on
// clean disconnect). Requires gotAuthID.
func (a *authState) buildAuth(localSID, remoteSID uint32, magic byte) []byte {
	p := header([]byte{0x40, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, localSID, remoteSID)
	p = append(p,
		0x00, 0x00, 0x00, 0x30, 0x01, magic, 0x00, byte(a.innerSendSeq),
		byte(a.innerSendSeq>>8), 0x00)
	p = append(p, a.authID[:]...)
	p = append(p, make([]byte, 32)...)
	a.innerSendSeq++
	return p
}

// buildRequestStream assembles the request-stream datagram (kappanhang
// sendRequestSerialAndAudio): asks the radio to open the CI-V data stream,
// announcing our local ports. RigName is the plain-text radio self-
// description echoed back in the answer ("IC-9700"); txBufMs is the
// transmit buffer length the radio assumes for us, in milliseconds.
// SECURITY: carries the obfuscated username — never log the buffer.
func (a *authState) buildRequestStream(localSID, remoteSID uint32, username, rigName string, civPort, audioPort int, txBufMs uint16) []byte {
	name := make([]byte, 8)
	copy(name, rigName)
	user := passcode(username)
	// audioSampleRate = 48000 (0xBB80), announced in both rate fields
	// exactly like the reference client even though v1 never opens audio.
	const srHi, srLo = 0xBB, 0x80
	p := header([]byte{0x90, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, localSID, remoteSID)
	p = append(p,
		0x00, 0x00, 0x00, 0x80, 0x01, 0x03, 0x00, byte(a.innerSendSeq),
		byte(a.innerSendSeq>>8), 0x00)
	p = append(p, a.authID[:]...)
	p = append(p, a.a8ReplyID[:]...)
	p = append(p, make([]byte, 8)...)
	p = append(p, name...)
	p = append(p, make([]byte, 40)...)
	p = append(p, user...)
	p = append(p,
		0x01, 0x01, 0x04, 0x04, 0x00, 0x00, srHi, srLo,
		0x00, 0x00, srHi, srLo,
		0x00, 0x00, byte(civPort>>8), byte(civPort),
		0x00, 0x00, byte(audioPort>>8), byte(audioPort), 0x00, 0x00,
		byte(txBufMs>>8), byte(txBufMs), 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
	a.innerSendSeq++
	return p
}

// parseLoginAnswer validates the 96-byte login answer. Returns ErrLoginRejected
// on the radio's explicit ff ff ff fe; otherwise it captures the session
// auth ID (token).
func (a *authState) parseLoginAnswer(r []byte) error {
	if !prefixEqual(r, sigLoginAnswer) {
		return ErrHandshakeTimeout
	}
	if len(r) > loginErrOffset+3 && r[loginErrOffset] == 0xff &&
		r[loginErrOffset+1] == 0xff && r[loginErrOffset+2] == 0xff && r[loginErrOffset+3] == 0xfe {
		return ErrLoginRejected
	}
	copy(a.authID[:], r[authIDOffset:authIDOffset+authIDLen])
	a.gotAuthID = true
	return nil
}

// authMagicRenewal reports whether the 64-byte auth answer acknowledges the
// 0x05 renewal magic.
func authMagicRenewal(r []byte) bool {
	return len(r) > offAuthMagic && r[offAuthMagic] == 0x05
}

// parseRequestAnswer validates the 144-byte request-stream answer: success
// flag, refreshed session IDs, refreshed auth ID, and the radio's
// self-description. The refreshed remote SID is returned to the caller.
func (a *authState) parseRequestAnswer(r []byte) (remoteSID uint32, devName string, ok bool) {
	if !prefixEqual(r, sigRequestAnswer) || len(r) <= requestOKFlag || r[requestOKFlag] != 0x01 {
		return 0, "", false
	}
	remoteSID = binary.BigEndian.Uint32(r[requestAnswerRemoteSID : requestAnswerRemoteSID+4])
	copy(a.authID[:], r[authIDOffset:authIDOffset+authIDLen])
	a.gotAuthID = true
	devName = parseNullTerminated(r[requestDevNameOffset:])
	return remoteSID, devName, true
}

// parseA8 captures the radio's volunteered 16-byte ID from an unprompted
// 0xa8 packet (echoed later in the request-stream packet).
func (a *authState) parseA8(r []byte) {
	if len(r) >= a8ReplyIDOffset+a8ReplyIDLen {
		copy(a.a8ReplyID[:], r[a8ReplyIDOffset:a8ReplyIDOffset+a8ReplyIDLen])
	}
}

// authFailKind classifies the 0x50 packet: 1 = refused (held/stale session),
// 2 = radio says disconnected, 0 = not an auth-fail packet.
func authFailKind(r []byte) int {
	if !prefixEqual(r, sigAuthFail) || len(r) <= radioDisconnectedOffset {
		return 0
	}
	switch {
	case r[48] == 0xff && r[49] == 0xff && r[50] == 0xff:
		return 1
	case r[48] == 0x00 && r[49] == 0x00 && r[50] == 0x00 && r[radioDisconnectedOffset] == 0x01:
		return 2
	}
	return 0
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
