package civ

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"time"
)

// Auth and token management for the control stream (brief, connection steps
// 3-5): login with substitution-table-obfuscated credentials, the token
// confirm + renewal exchange, the 60 s renewal cycle with its timeout, and
// the stream request that carries the client's chosen local CI-V port.
//
// NEVER-LOG PIN: nothing here logs packet content — the substitution table
// is public, so captured bytes ARE the password (package doc).

// renewalState tracks the 60 s token renewal cycle: the deadline for the
// next renewal send and whether one is outstanding (with its timeout).
// Guarded by the transport mutex.
type renewalState struct {
	nextDue     time.Time
	deadline    time.Time
	outstanding bool
}

// newTokReq draws the random u16 token-request nonce the login response must
// echo (wfview matches on it before accepting the token).
func newTokReq() uint16 {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0x1c1c // crypto/rand failure is not a session concern
	}
	return binary.LittleEndian.Uint16(b[:])
}

// sendLogin builds and tracked-sends the login packet. Returns the token
// request nonce to match against the response. Caller holds ctrl.mu.
func (s *stream) sendLogin(username, password, clientName [16]byte, now time.Time) uint16 {
	s.tokReq = newTokReq()
	p := loginPacket(username, password, clientName, s.tokReq, s.authSeq, s.myID, s.remoteID)
	s.authSeq++
	_ = s.sendTracked(p, now)
	return s.tokReq
}

// sendToken sends the 0x40 token packet (confirm right after login, renewal
// every 60 s, removal on clean disconnect). Caller holds ctrl.mu.
func (s *stream) sendToken(reqType byte, now time.Time) {
	p := tokenPacket(reqType, s.tokReq, s.token, s.authSeq, s.myID, s.remoteID)
	s.authSeq++
	_ = s.sendTracked(p, now)
}

// sendStreamRequest sends the 0x90 stream-request packet carrying the
// client's chosen local CI-V and audio ports big-endian (the radio answers
// with the status packet naming its own ports).
func (s *stream) sendStreamRequest(civLocalPort, audioLocalPort int, username [16]byte, now time.Time) {
	p := streamRequestPacket(civLocalPort, audioLocalPort, s.tokReq, s.token, s.authSeq, username, s.myID, s.remoteID)
	s.authSeq++
	_ = s.sendTracked(p, now)
}

// parseLoginResponse validates a 0x60 login response against the token
// request we sent. Returns the session token. The error field 0xfffffffe
// (LE bytes fe ff ff ff) is the radio's invalid-username/password verdict.
func parseLoginResponse(b []byte, ourTokReq uint16) (token uint32, err error) {
	if got := binary.LittleEndian.Uint16(b[tokReqOff:]); got != ourTokReq {
		return 0, errTokenMismatch
	}
	if e := binary.LittleEndian.Uint32(b[errOff:]); e == errAuthBad {
		return 0, ErrAuthFailed
	}
	return binary.LittleEndian.Uint32(b[tokenOff:]), nil
}

// parseTokenResponse validates a 0x40 token response (requestreply 0x02,
// requesttype 0x05): response 0 = renewal accepted, 0xffffffff = rejected
// (session stale/other client).
func parseTokenResponse(b []byte) (response uint32, ok bool) {
	if b[reqReplyOff] != reqReplyReply || b[reqTypeOff] != reqTypeRenewal {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b[errOff:]), true
}

// errTokenMismatch: the login/token response echoed a different token
// request nonce — not our session's answer.
var errTokenMismatch = errors.New("token request mismatch in auth response")

// renewNow sends a token renewal (requesttype 0x05) and arms the renewal
// timeout. Caller holds ctrl.mu and the transport mutex.
func (t *Transport) renewNow(now time.Time) {
	t.ctrl.mu.Lock()
	t.ctrl.sendToken(reqTypeRenewal, now)
	t.ctrl.mu.Unlock()
	t.renewal.outstanding = true
	t.renewal.deadline = now.Add(t.o.RenewalTimeout)
}

// renewalOK records a successful renewal response: disarm the timeout, lay
// in the next 60 s renewal (wfview restarts TOKEN_RENEWAL on every success).
// Caller holds the transport mutex.
func (t *Transport) renewalOK(now time.Time) {
	t.renewal.outstanding = false
	t.renewal.nextDue = now.Add(t.o.TokenRenewal)
}

// maintainRenewal is the maintenance tick for the renewal cycle.
// Caller holds the transport mutex.
func (t *Transport) maintainRenewal(now time.Time) {
	if !t.renewal.outstanding && now.After(t.renewal.nextDue) {
		t.renewNow(now)
	}
}

// authBytes assembles the credential packet fields once per dial. The
// username rides substituted in both the login and the stream request (wfview
// substitutes it in both); the client name travels plain.
type authBytes struct {
	username   [16]byte
	password   [16]byte
	clientName [16]byte
}

func newAuthBytes(username, password, clientName string) authBytes {
	return authBytes{
		username:   passcode(username),
		password:   passcode(password),
		clientName: field16(clientName),
	}
}
