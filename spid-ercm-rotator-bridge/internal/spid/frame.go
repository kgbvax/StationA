// SPDX-License-Identifier: AGPL-3.0-or-later

// Rot1Prog wire framing, pinned byte-exact from the plan's Appendix A
// (docs/plans/2026-09-12-001-feat-sat-ops-rotators-plan.md, lines 466-479;
// primary sources: hamlib rotators/spid/spid.c and the LA6YKA protocol spec).
//
// One 13-byte command packet:
//
//	S(0x57) | H1-H4 az digits | PH | V1-V4 el digits | PV | K | END(0x20)
//
// Command digit fields are ASCII (0x30+d); Rot1Prog zeroes every el/PH/PV
// field and keeps H4 = 0x30. The 5-byte STATUS REPLY carries its digit fields
// as RAW byte values 0-9, NOT ASCII — the classic desync trap, pinned by the
// byte-exact fixtures in frame_test.go. K: 0x2F set position, 0x1F status
// request, 0x0F stop. Set commands expect NO reply; stop/status packets zero
// the position fields. Azimuth encoding: u = 360 + az (whole degrees).
package spid

import (
	"fmt"
	"math"
	"time"
)

// Rot1Prog framing constants (Appendix A).
const (
	startByte = 0x57 // S
	endByte   = 0x20 // END
	asciiZero = 0x30 // command digits are ASCII 0x30+d

	kSet    = 0x2F // set position
	kStatus = 0x1F // status request
	kStop   = 0x0F // stop

	cmdLen   = 13
	replyLen = 5

	// DefaultWritePace is the minimum spacing between any two writes to the
	// controller: hamlib's 300 ms post_write_delay (Appendix A) — the
	// Rot1Prog needs it to keep up, and MD-0x-class firmware below 1.2507
	// drops fast commands outright.
	DefaultWritePace = 300 * time.Millisecond

	// DefaultReadTimeout bounds the wait for a status reply after the poll
	// writes the request. At 1200 baud a 5-byte reply takes ~40 ms; a
	// silent controller must not stall the poll tick.
	DefaultReadTimeout = 500 * time.Millisecond

	// DefaultWriteTimeout bounds one port write: go.bug.st/serial has no
	// write deadline, so a write stalled on a wedged fd is closed out by the
	// driver's watchdog after this long and feeds the reopen path. 13 bytes
	// at 1200 baud take ~110 ms — anything near this bound is a dead fd, not
	// a slow controller.
	DefaultWriteTimeout = 3 * time.Second
)

// blankCommand builds the 13-byte packet with every digit field '0' (Rot1Prog
// zeroes all el/PH/PV fields; status and stop packets also zero the az fields)
// and the given K byte.
func blankCommand(k byte) []byte {
	b := make([]byte, cmdLen)
	b[0] = startByte
	for i := 1; i <= 10; i++ {
		b[i] = asciiZero
	}
	b[11] = k
	b[12] = endByte
	return b
}

// encodeSet builds the set-position command for a whole-degree azimuth:
// u = 360 + az spells into the ASCII digit fields H1..H3, H4 stays '0'.
func encodeSet(az float64) []byte {
	u := int(math.Round(az)) + 360
	b := blankCommand(kSet)
	b[1] = asciiZero + byte(u/100)
	b[2] = asciiZero + byte((u/10)%10)
	b[3] = asciiZero + byte(u%10)
	return b
}

// encodeStatus builds the status request: position fields zeroed, K = 0x1F.
func encodeStatus() []byte { return blankCommand(kStatus) }

// encodeStop builds the stop packet: position fields zeroed, K = 0x0F.
func encodeStop() []byte { return blankCommand(kStop) }

// encodeStatusReply builds the 5-byte status reply a Rot1Prog controller sends
// for az: u = 360 + az into RAW digit bytes (0-9, not ASCII — the asymmetry
// with encodeSet is deliberate and pinned by test fixtures).
func encodeStatusReply(az float64) []byte {
	u := int(math.Round(az)) + 360
	return []byte{
		startByte,
		byte(u / 100),
		byte((u / 10) % 10),
		byte(u % 10),
		endByte,
	}
}

// decodeStatusReply decodes the 5-byte status reply. The digit bytes must be
// raw values 0-9; ASCII digits (a Rot2Prog-width misframe or a naive peer)
// are rejected, never misread.
func decodeStatusReply(b []byte) (float64, error) {
	if len(b) != replyLen {
		return 0, fmt.Errorf("status reply: %d bytes, want %d", len(b), replyLen)
	}
	if b[0] != startByte {
		return 0, fmt.Errorf("status reply: start byte 0x%02X, want 0x%02X", b[0], startByte)
	}
	if b[replyLen-1] != endByte {
		return 0, fmt.Errorf("status reply: end byte 0x%02X, want 0x%02X", b[replyLen-1], endByte)
	}
	for i := 1; i <= 3; i++ {
		if b[i] > 9 {
			return 0, fmt.Errorf("status reply digit byte %d is 0x%02X — Rot1Prog reply digits are RAW 0-9, not ASCII (Appendix A)", i, b[i])
		}
	}
	u := int(b[1])*100 + int(b[2])*10 + int(b[3])
	if u < 360 {
		// az is u − 360; a u below the 360 offset claims a negative azimuth,
		// which no physical rotor reports — treat it as the encoding
		// violation it is, never as a position (the codec's never-misread
		// posture).
		return 0, fmt.Errorf("status reply az u=%d below the 360 offset — encoding violation", u)
	}
	az := float64(u - 360)
	if az >= 360 {
		// Controllers with a continuous position register keep counting past
		// north when a shortest-path slew crosses 0° (observed live
		// 2026-09-20: 271°→89° through north reported as 449). The geometry
		// is right, only the report is unwrapped — normalize into [0, 360)
		// so the deadband/moving math and /state stay in degrees.
		az = math.Mod(az, 360)
	}
	return az, nil
}

// decodeCommandAz reads the azimuth back out of a set-position command
// (ASCII digit fields). It is the device-side counterpart of encodeSet and
// exists for the in-process mock controller and the frame round-trip tests.
func decodeCommandAz(b []byte) float64 {
	if len(b) != cmdLen {
		return math.NaN()
	}
	u := (int(b[1]-asciiZero))*100 + (int(b[2]-asciiZero))*10 + int(b[3]-asciiZero)
	return float64(u - 360)
}
