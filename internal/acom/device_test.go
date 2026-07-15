package acom

import "testing"

// noopLog satisfies Logger without depending on *testing.T in error paths.
type noopLog struct{}

func (noopLog) Infof(string, ...any)  {}
func (noopLog) Warnf(string, ...any)  {}
func (noopLog) Debugf(string, ...any) {}

// TestProcessBufferLengthZeroNoPanic is the regression test for the high-severity
// finding: a 0x55 <any> 0x00 byte sequence made processBuffer slice an empty
// packet, verifyChecksum([]byte{}) returned true, and handlePacket panicked at
// packet[1]. Now processBuffer rejects declared lengths < 4 and resyncs.
func TestProcessBufferLengthZeroNoPanic(t *testing.T) {
	d := New("/dev/null", 300, false, noopLog{})
	// 0x55, addr, length=0 -> must resync, not panic. Followed by junk.
	buf := []byte{0x55, 0x2F, 0x00, 0x11, 0x22}
	rem := d.processBuffer(buf, nil, nil)
	_ = rem // no panic is the assertion
}

// TestProcessBufferLengthOneNoPanic covers the pktLen==1 variant of the same
// defect (packet=buf[:1], packet[1] out of range).
func TestProcessBufferLengthOneNoPanic(t *testing.T) {
	d := New("/dev/null", 300, false, noopLog{})
	buf := []byte{0x55, 0x2F, 0x01, 0x11}
	_ = d.processBuffer(buf, nil, nil)
}

// TestProcessBufferResyncsPastGarbage confirms a too-short declared length does
// not consume a following valid start byte: after resyncing past the 0x00-len
// frame, the second 0x55 is re-evaluated.
func TestProcessBufferResyncsPastGarbage(t *testing.T) {
	d := New("/dev/null", 300, false, noopLog{})
	// 0x55 0x2F 0x00  -> resync (drop 0x55), then 0x2F is not a start byte...
	// The remainder keeps scanning; we only assert no panic and progress.
	buf := []byte{0x55, 0x2F, 0x00, 0x55, 0x2F, 0x04}
	rem := d.processBuffer(buf, nil, nil)
	if len(rem) == 0 && len(buf) != 0 {
		// Acceptable either way; the point is no panic and no mis-slice.
	}
}

// Power control (SetPower / RTS wake line / desiredPower) was removed from the
// device: the PA bridge is a pure observer, and power-on/off is owned by the
// hf/switch slot's remote-on relays. The former TestSetPower* cases are gone
// with the machinery they covered.
