package serialio

import (
	"testing"

	"pelcots/internal/pelco"
)

// collect runs the framer over a sequence of read chunks and returns the
// decoded frames, mirroring how read() feeds extract().
func collect(t *testing.T, chunks ...[]byte) []pelco.Frame {
	t.Helper()
	p := &Port{frames: make(chan Event, 64)}
	var buf []byte
	for _, c := range chunks {
		buf = p.extract(append(buf, c...))
	}
	close(p.frames)
	var got []pelco.Frame
	for ev := range p.frames {
		if ev.Err == nil && ev.Raw == nil {
			got = append(got, ev.Frame)
		}
	}
	return got
}

// TestResyncStrayByte reproduces the exact case seen on the wire: a stray
// leading 0xFF arrives in one read, then the real tilt response arrives in the
// next. The framer must recover the valid frame instead of misaligning.
func TestResyncStrayByte(t *testing.T) {
	got := collect(t,
		[]byte{0xFF}, // stray sync byte
		[]byte{0xFF, 0x01, 0x00, 0x5B, 0x08, 0xBF, 0x23}, // real tilt response, 22.39°
	)
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1: %+v", len(got), got)
	}
	if !got[0].IsTiltResponse() || got[0].Word() != 0x08BF {
		t.Fatalf("recovered frame wrong: %+v", got[0])
	}
}

// TestSplitFrameAcrossReads ensures a frame split mid-way across two reads is
// still assembled.
func TestSplitFrameAcrossReads(t *testing.T) {
	got := collect(t,
		[]byte{0xFF, 0x01, 0x00},
		[]byte{0x59, 0x75, 0x2F, 0xFE}, // pan ~300° (29999)
	)
	if len(got) != 1 || !got[0].IsPanResponse() || got[0].Word() != 29999 {
		t.Fatalf("split frame not assembled: %+v", got)
	}
}

// TestLeadingGarbageThenFrame drops non-sync noise before a valid frame.
func TestLeadingGarbageThenFrame(t *testing.T) {
	got := collect(t,
		[]byte{0x12, 0x34, 0xAB}, // garbage, no sync
		[]byte{0xFF, 0x01, 0x00, 0x59, 0x00, 0x00, 0x5A},
	)
	if len(got) != 1 || !got[0].IsPanResponse() {
		t.Fatalf("did not recover after garbage: %+v", got)
	}
}
