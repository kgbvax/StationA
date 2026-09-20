package spid

import (
	"bytes"
	"math"
	"testing"
)

// wantSet123 pins the Appendix A byte table (docs/plans/
// 2026-09-12-001-feat-sat-ops-rotators-plan.md, lines 466-479) for a
// set-position command carrying azimuth 123°:
//
//	u = 360 + 123 = 483 → H1 H2 H3 = '4' '8' '3', H4 = '0'
//	Rot1Prog zeroes every el/PH/PV field; command digits are ASCII (0x30+d)
//	(while the STATUS REPLY digits are raw 0-9 — the trap pinned below);
//	K = 0x2F set position, END = 0x20.
var wantSet123 = []byte{
	0x57,                   // S
	0x34, 0x38, 0x33, 0x30, // H1 H2 H3 H4 = '4' '8' '3' '0' (483 = 360+123)
	0x30,                   // PH = '0'
	0x30, 0x30, 0x30, 0x30, // V1..V4 = '0' (Rot1Prog zeroes the el fields)
	0x30, // PV = '0'
	0x2F, // K = set position
	0x20, // END
}

func TestEncodeSetAz123MatchesAppendixByteTable(t *testing.T) {
	got := encodeSet(123)
	if !bytes.Equal(got, wantSet123) {
		t.Fatalf("set frame for az 123 does not match the Appendix A byte table:\n got %s\nwant %s",
			hex(got), hex(wantSet123))
	}
}

func TestEncodeStatusRequest(t *testing.T) {
	got := encodeStatus()
	want := []byte{
		0x57,
		0x30, 0x30, 0x30, 0x30, // H1..H4 = '0' — status packets zero position fields
		0x30,                   // PH
		0x30, 0x30, 0x30, 0x30, // V1..V4
		0x30, // PV
		0x1F, // K = status request
		0x20, // END
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("status request frame:\n got %s\nwant %s", hex(got), hex(want))
	}
}

func TestEncodeStopUsesK0F(t *testing.T) {
	got := encodeStop()
	want := []byte{
		0x57,
		0x30, 0x30, 0x30, 0x30,
		0x30,
		0x30, 0x30, 0x30, 0x30,
		0x30,
		0x0F, // K = stop
		0x20,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stop frame:\n got %s\nwant %s", hex(got), hex(want))
	}
}

// TestDecodeStatusReplyRawDigits pins THE Rot1Prog desync trap byte-exactly:
// the 5-byte status reply carries its digit fields as RAW byte values 0-9,
// never ASCII. az = H1*100 + H2*10 + H3 - 360.
func TestDecodeStatusReplyRawDigits(t *testing.T) {
	// Position 123°: u = 483 → raw digits 4, 8, 3 (0x04 0x08 0x03, NOT '4' '8' '3').
	az, err := decodeStatusReply([]byte{0x57, 0x04, 0x08, 0x03, 0x20})
	if err != nil {
		t.Fatalf("decode raw-digit reply: %v", err)
	}
	if az != 123 {
		t.Fatalf("decoded az = %v, want 123", az)
	}

	// The same position spelled as ASCII digits — what a naive encoder or a
	// Rot2Prog-width misframe produces — must be REJECTED, not misread as
	// 0x34*100+0x38*10+0x33 degrees of garbage.
	if _, err := decodeStatusReply([]byte{0x57, 0x34, 0x38, 0x33, 0x20}); err == nil {
		t.Fatal("ASCII digit bytes in a status reply must be rejected — Rot1Prog reply digits are raw 0-9 (Appendix A)")
	}

	// Full digit span: az 0 → u 360 → raw 3,6,0; az 360 ≡ 0 → u 720 → raw
	// 7,2,0 (normalized into [0, 360) — same contract as the past-north wrap
	// pinned by TestDecodeStatusReplyWrapsPastNorth).
	for _, tc := range []struct {
		az   float64
		raw  []byte
		want float64
	}{
		{0, []byte{0x57, 0x03, 0x06, 0x00, 0x20}, 0},
		{360, []byte{0x57, 0x07, 0x02, 0x00, 0x20}, 0},
	} {
		got, err := decodeStatusReply(tc.raw)
		if err != nil {
			t.Fatalf("decode az %v reply: %v", tc.az, err)
		}
		if got != tc.want {
			t.Errorf("az %v reply decoded to %v, want %v", tc.az, got, tc.want)
		}
	}
}

func TestDecodeStatusReplyRejectsBadFrames(t *testing.T) {
	for name, b := range map[string][]byte{
		"wrong start byte": {0x00, 0x04, 0x08, 0x03, 0x20},
		"wrong end byte":   {0x57, 0x04, 0x08, 0x03, 0x00},
		"short":            {0x57, 0x04, 0x08, 0x20},
		"long":             {0x57, 0x04, 0x08, 0x03, 0x20, 0x00},
		"empty":            {},
		// u = 100 < the 360 offset would decode to az −260 — no physical
		// rotor reports a negative azimuth; this is an encoding violation.
		"u below the 360 offset": {0x57, 0x01, 0x00, 0x00, 0x20},
	} {
		if _, err := decodeStatusReply(b); err == nil {
			t.Errorf("%s: expected decode error, got none", name)
		}
	}
}

// TestDecodeStatusReplyWrapsPastNorth pins the live observation of
// 2026-09-20: a shortest-path slew across north (271°→89°) left the
// controller's continuous position register at 449; the bridge must
// normalize it into [0, 360) or the deadband compares targets against a
// readback a full turn away and never suppresses.
func TestDecodeStatusReplyWrapsPastNorth(t *testing.T) {
	reply := encodeStatusReply(449) // u = 809 → raw digits 8, 0, 9
	got, err := decodeStatusReply(reply)
	if err != nil {
		t.Fatalf("decode past-north reply %s: %v", hex(reply), err)
	}
	if got != 89 {
		t.Errorf("past-north report 449 decoded to %v, want 89", got)
	}
}

// TestCommandReplyRoundTrip walks the whole encode/decode pair: a set command's
// ASCII digits and the device's raw-digit status reply for the same azimuth
// must both resolve back to the same whole degrees, fractions rounding. (360
// is absent by design: the reply side normalizes it to 0 — see
// TestDecodeStatusReplyRawDigits.)
func TestCommandReplyRoundTrip(t *testing.T) {
	for _, az := range []float64{0, 1, 45, 123, 180, 359, 123.4, 359.6} {
		want := math.Round(az)
		cmd := encodeSet(az)
		if got := decodeCommandAz(cmd); got != want {
			t.Errorf("az %v: command digits decode to %v, want %v (%s)", az, got, want, hex(cmd))
		}
		reply := encodeStatusReply(az)
		got, err := decodeStatusReply(reply)
		if err != nil {
			t.Fatalf("az %v: decode reply %s: %v", az, hex(reply), err)
		}
		// The reply side normalizes into [0, 360): a whole-degree want of 360
		// (359.6 rounded) reads back as 0 — the same position.
		if want == 360 {
			want = 0
		}
		if got != want {
			t.Errorf("az %v: reply %s decodes to %v, want %v", az, hex(reply), got, want)
		}
	}
}

// hex renders a byte slice as two-digit hex pairs for failure messages.
func hex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*3)
	for i, v := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, digits[v>>4], digits[v&0x0f])
	}
	return string(out)
}
