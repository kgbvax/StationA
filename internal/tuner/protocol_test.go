package tuner

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestBuildFrame(t *testing.T) {
	// Full tune: 0xFF, cmd 4, len 1, payload 2.
	if got := buildFrame(scmdTuneMode, tuneModeFull); !bytes.Equal(got, []byte{0xFF, 4, 1, 2}) {
		t.Fatalf("frame = % X", got)
	}
	if got := buildFrame(scmdSync); !bytes.Equal(got, []byte{0xFF, 1, 0}) {
		t.Fatalf("sync = % X", got)
	}
}

func TestParseFrame(t *testing.T) {
	cmd, payload, ok := parseFrame([]byte{0xFF, scmdMeter, 8, 0, 0, 0xD9, 0, 0xEE, 2})
	if !ok || cmd != scmdMeter || len(payload) != 6 {
		t.Fatalf("parse = %d %v %v", cmd, payload, ok)
	}
	if _, _, ok := parseFrame([]byte{0x00, 1, 0}); ok {
		t.Fatal("bad flag should not parse")
	}
}

func TestMeterAndRelay(t *testing.T) {
	// SWR raw 217 → 2.17; fwd 750W. Offsets 4..8.
	m := make([]byte, 10)
	m[0], m[1] = 0xFF, scmdMeter
	binary.LittleEndian.PutUint16(m[4:6], 217)
	binary.LittleEndian.PutUint16(m[6:8], 750)
	if swr, fwd, ok := meter(m); !ok || swr != 2.17 || fwd != 750 {
		t.Fatalf("meter = %v %v %v", swr, fwd, ok)
	}

	// L raw 1234 → 12.34 µH; C 56 pF. Offsets 6..10.
	r := make([]byte, 10)
	r[0], r[1] = 0xFF, scmdRelay
	binary.LittleEndian.PutUint16(r[6:8], 1234)
	binary.LittleEndian.PutUint16(r[8:10], 56)
	if l, c, ok := relay(r); !ok || l != 12.34 || c != 56 {
		t.Fatalf("relay = %v %v %v", l, c, ok)
	}
}
