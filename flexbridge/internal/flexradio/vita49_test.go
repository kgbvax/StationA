package flexradio

import (
	"encoding/binary"
	"errors"
	"testing"
)

// buildMeterPacket constructs a synthetic VITA-49 meter datagram matching
// the real FlexRadio wire format: header + 3-word class id with the 0x8002
// discriminator in word3's low 16 bits, plus the meter readings as
// (uint16 index, int16 raw) pairs.
func buildMeterPacket(readings []MeterReading) []byte {
	w0 := uint32(0)
	w0 |= 0 << 30 // packet type = data
	w0 |= 1 << 29 // class id present
	// no trailer, no timestamp

	out := make([]byte, 0, 16+4*len(readings))
	var hb [4]byte
	// word0: header
	binary.BigEndian.PutUint32(hb[:], w0)
	out = append(out, hb[:]...)
	// word1: class id (OUI / packet count)
	binary.BigEndian.PutUint32(hb[:], 0x00000700)
	out = append(out, hb[:]...)
	// word2: class id (info high)
	binary.BigEndian.PutUint32(hb[:], 0x00001c2d)
	out = append(out, hb[:]...)
	// word3: class id (info low) -- low 16 bits = 0x8002 (meter stream)
	binary.BigEndian.PutUint32(hb[:], 0x534c8002)
	out = append(out, hb[:]...)

	for _, r := range readings {
		binary.BigEndian.PutUint16(hb[:2], r.Index)
		binary.BigEndian.PutUint16(hb[2:], uint16(r.Raw))
		out = append(out, hb[:2]...)
		out = append(out, hb[2:]...)
	}
	return out
}

func TestParseVITA49_MeterPacket(t *testing.T) {
	readings := []MeterReading{
		{Index: 5, Raw: 6400},   // FWDPWR ~ 100 W
		{Index: 7, Raw: 256},    // SWR 2.0
		{Index: 10, Raw: -7968}, // S-meter -62.25 dBm
	}
	b := buildMeterPacket(readings)

	p, err := ParseVITA49(b)
	if err != nil {
		t.Fatalf("ParseVITA49: %v", err)
	}
	if !p.HasClass {
		t.Error("HasClass = false, want true")
	}
	if p.ClassIDLow != meterClassIDLow {
		t.Errorf("ClassIDLow = %#x, want %#x", p.ClassIDLow, meterClassIDLow)
	}
	if p.HasTrailer {
		t.Error("HasTrailer = true, want false")
	}

	got, err := p.MeterReadings()
	if err != nil {
		t.Fatalf("MeterReadings: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	for i, want := range readings {
		if got[i] != want {
			t.Errorf("got[%d] = %+v, want %+v", i, got[i], want)
		}
	}
}

func TestParseVITA49_ShortPacket(t *testing.T) {
	_, err := ParseVITA49([]byte{0x00, 0x01, 0x02})
	if !errors.Is(err, ErrShortPacket) {
		t.Errorf("err = %v, want ErrShortPacket", err)
	}
}

func TestParseVITA49_NonMeterClassRejected(t *testing.T) {
	// Build a packet with a non-meter class id (0x8003 = spectrum) in word3.
	w0 := uint32(1 << 29)     // class present
	out := make([]byte, 16+4) // header + 3-word class id + dummy payload
	binary.BigEndian.PutUint32(out[0:4], w0)
	binary.BigEndian.PutUint32(out[4:8], 0x00000700)   // word1 (OUI)
	binary.BigEndian.PutUint32(out[8:12], 0x00001c2d)  // word2 (info hi)
	binary.BigEndian.PutUint32(out[12:16], 0x534c8003) // word3 (info low: 0x8003 = spectrum)
	binary.BigEndian.PutUint32(out[16:20], 0)          // a dummy payload word

	p, err := ParseVITA49(out)
	if err != nil {
		t.Fatalf("ParseVITA49: %v", err)
	}
	_, err = p.MeterReadings()
	if !errors.Is(err, ErrNotMeterPacket) {
		t.Errorf("err = %v, want ErrNotMeterPacket", err)
	}
}

func TestMeterReadings_OddPayloadRejected(t *testing.T) {
	// Class present, class id = 0x8002, but payload length not %4.
	w0 := uint32(1 << 29)
	out := make([]byte, 16+6) // header + 3-word class id + 6 payload bytes
	binary.BigEndian.PutUint32(out[0:4], w0)
	binary.BigEndian.PutUint32(out[4:8], 0x00000700)   // word1
	binary.BigEndian.PutUint32(out[8:12], 0x00001c2d)  // word2
	binary.BigEndian.PutUint32(out[12:16], 0x534c8002) // word3 (meter)
	// 6 junk payload bytes
	p, err := ParseVITA49(out)
	if err != nil {
		t.Fatalf("ParseVITA49: %v", err)
	}
	if _, err := p.MeterReadings(); err == nil {
		t.Error("MeterReadings err = nil, want error for odd payload")
	}
}

func TestParseVITA49_EmptyPayload(t *testing.T) {
	b := buildMeterPacket(nil)
	p, err := ParseVITA49(b)
	if err != nil {
		t.Fatalf("ParseVITA49: %v", err)
	}
	got, err := p.MeterReadings()
	if err != nil {
		t.Fatalf("MeterReadings: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestParseVITA49_WithTrailer(t *testing.T) {
	// class + trailer, no timestamp. Verify trailer is peeled off and not
	// treated as payload.
	w0 := uint32(0)
	w0 |= 1 << 29 // class
	w0 |= 1 << 28 // trailer
	// payload: one meter pair (4 bytes), then a 4-byte trailer
	out := make([]byte, 16+4+4) // header + 3-word class id + payload + trailer
	binary.BigEndian.PutUint32(out[0:4], w0)
	binary.BigEndian.PutUint32(out[4:8], 0x00000700)   // word1
	binary.BigEndian.PutUint32(out[8:12], 0x00001c2d)  // word2
	binary.BigEndian.PutUint32(out[12:16], 0x534c8002) // word3 (meter)
	binary.BigEndian.PutUint16(out[16:18], 5)          // index
	binary.BigEndian.PutUint16(out[18:20], 6400)       // raw
	binary.BigEndian.PutUint32(out[20:24], 0xDEADBEEF) // trailer

	p, err := ParseVITA49(out)
	if err != nil {
		t.Fatalf("ParseVITA49: %v", err)
	}
	if !p.HasTrailer {
		t.Error("HasTrailer = false, want true")
	}
	if p.Trailer != 0xDEADBEEF {
		t.Errorf("Trailer = %#x, want 0xDEADBEEF", p.Trailer)
	}
	if len(p.Payload) != 4 {
		t.Fatalf("len(Payload) = %d, want 4 (trailer peeled)", len(p.Payload))
	}
	got, err := p.MeterReadings()
	if err != nil {
		t.Fatalf("MeterReadings: %v", err)
	}
	if len(got) != 1 || got[0].Index != 5 || got[0].Raw != 6400 {
		t.Errorf("got = %+v, want [{5 6400}]", got)
	}
}

func TestParseVITA49_WithTimestamps(t *testing.T) {
	// class + integer ts + fractional ts. FlexRadio's TSF field consumes 12
	// bytes on the wire (8-byte fractional ts + 4-byte sample word).
	w0 := uint32(0)
	w0 |= 1 << 29                // class
	w0 |= 0b01 << 26             // TSI = 01 (integer ts present)
	w0 |= 0b10 << 24             // TSF = 10 (fractional ts present)
	out := make([]byte, 16+4+12) // header + 3-word class id + int ts + frac ts(12)
	binary.BigEndian.PutUint32(out[0:4], w0)
	binary.BigEndian.PutUint32(out[4:8], 0x00000700)           // word1
	binary.BigEndian.PutUint32(out[8:12], 0x00001c2d)          // word2
	binary.BigEndian.PutUint32(out[12:16], 0x534c8002)         // word3 (meter)
	binary.BigEndian.PutUint32(out[16:20], 0x12345678)         // integer ts
	binary.BigEndian.PutUint64(out[20:28], 0x9ABCDEF012345678) // fractional ts (hi)
	binary.BigEndian.PutUint32(out[28:32], 0)                  // sample word

	p, err := ParseVITA49(out)
	if err != nil {
		t.Fatalf("ParseVITA49: %v", err)
	}
	if !p.HasTS {
		t.Error("HasTS = false, want true")
	}
	wantInt := uint32(0x12345678)
	wantFrac := uint64(0x9ABCDEF012345678)
	if p.IntegerTS != wantInt {
		t.Errorf("IntegerTS = %#x, want %#x", p.IntegerTS, wantInt)
	}
	if p.FractionalTS != wantFrac {
		t.Errorf("FractionalTS = %#x, want %#x", p.FractionalTS, wantFrac)
	}
	if len(p.Payload) != 0 {
		t.Errorf("len(Payload) = %d, want 0", len(p.Payload))
	}
}

// TestParseVITA49_RealFlexPacket locks in the wire format observed on a live
// FLEX-8400 (SmartSDR v4.2.20). These are reconstructed from a captured
// meter datagram (after the IP/UDP headers), so any future regression in
// the class-id offset / bit parsing will trip this test.
func TestParseVITA49_RealFlexPacket(t *testing.T) {
	// Layout (word0 = 0x33530010 -> T=00,C=1,Tr=1,TSI=00,TSF=11):
	//   word0..word3: header + 3-word class id (0x8002 = meter stream)
	//   12-byte fractional timestamp (TSF=11: 8-byte frac + 4-byte sample word)
	//   meter pairs (uint16 idx, int16 raw)
	//   4-byte trailer (Tr=1)
	// 0x33 = 0011 0011: C=1,Tr=1,TSI=00,TSF=11
	raw := []byte{
		0x33, 0x53, 0x00, 0x10, // word0: T=00,C=1,Tr=1,TSI=00,TSF=11
		0x00, 0x00, 0x07, 0x00, // word1 (OUI)
		0x00, 0x00, 0x1c, 0x2d, // word2 (info hi)
		0x53, 0x4c, 0x80, 0x02, // word3 (info lo: 0x8002 = meter stream)
		0x6a, 0x48, 0x28, 0x7e, // fractional ts (bytes 0-7)
		0x00, 0x00, 0x00, 0x00, // fractional ts (bytes 4-7)
		0x00, 0x00, 0x00, 0x00, // sample/sequence word (bytes 8-11)
		// meter pairs
		0x00, 0x01, 0xc4, 0x00, // idx 1, raw 0xc400
		0x00, 0x02, 0xc4, 0x00, // idx 2
		0x00, 0x03, 0x00, 0x00, // idx 3
		0x00, 0x08, 0x00, 0x00, // idx 8
		0x00, 0x09, 0x00, 0x00, // idx 9
		0x00, 0x0a, 0x00, 0x80, // idx 10
		0x00, 0x1b, 0xb5, 0x00, // idx 27
		0x00, 0x00, 0x00, 0x00, // trailer
	}
	p, err := ParseVITA49(raw)
	if err != nil {
		t.Fatalf("ParseVITA49: %v", err)
	}
	if !p.HasClass {
		t.Fatal("HasClass = false, want true")
	}
	if p.ClassIDLow != meterClassIDLow {
		t.Errorf("ClassIDLow = %#x, want %#x (0x8002)", p.ClassIDLow, meterClassIDLow)
	}
	if !p.HasTrailer {
		t.Error("HasTrailer = false, want true (this packet has Tr=1)")
	}
	readings, err := p.MeterReadings()
	if err != nil {
		t.Fatalf("MeterReadings: %v", err)
	}
	if len(readings) == 0 {
		t.Fatal("got 0 readings; payload was likely mis-parsed")
	}
	// First meter index should be 1.
	if readings[0].Index != 1 {
		t.Errorf("readings[0].Index = %d, want 1", readings[0].Index)
	}
	// idx 1 raw 0xc400 = -15360 signed -> -120 dBm (codec mic peak, ~quiet).
	if readings[0].Raw != -15360 {
		t.Errorf("readings[0].Raw = %d, want -15360", readings[0].Raw)
	}
}
