package acom

import "testing"

func TestVerifyChecksumRejectsShortFrames(t *testing.T) {
	// An empty slice sums to 0 trivially; it must NOT pass as a valid frame.
	// (Regression: previously verifyChecksum([]byte{}) returned true, so a
	// length-byte-0 frame reached handlePacket and panicked at packet[1].)
	cases := [][]byte{
		nil,
		{},
		{0x55},
		{0x55, 0x2F},
		{0x55, 0x2F, 0x00},
	}
	for _, c := range cases {
		if verifyChecksum(c) {
			t.Errorf("verifyChecksum(%v) = true, want false (short/empty frame)", c)
		}
	}
}

func TestVerifyChecksumValidFrame(t *testing.T) {
	// {0x55, 0x2F, 0x04, 0x...}: build a 4-byte frame that sums to 0.
	frame := []byte{0x55, 0x2F, 0x04, 0x00}
	frame[3] = calculateChecksum(frame[:3])
	if !verifyChecksum(frame) {
		t.Error("a correctly checksummed 4-byte frame should pass")
	}
}

func TestCalculateChecksum(t *testing.T) {
	// The checksum byte makes all bytes sum to 0 (mod 256).
	frame := []byte{0x55, 0x92, 0x04, 0x15}
	// 0x55+0x92+0x04 = 0xEB; checksum = 0x15 -> sum 0x00. (MsgEnableAuto frame.)
	if got := calculateChecksum([]byte{0x55, 0x92, 0x04}); got != 0x15 {
		t.Errorf("calculateChecksum = 0x%02X, want 0x15", got)
	}
	_ = frame
}
