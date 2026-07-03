package flexradio

import (
	"encoding/binary"
	"errors"
	"testing"
)

// buildMeterPacket constructs a synthetic VITA-49 meter datagram with the
// given readings, for testing. It sets the class-id bit and writes the
// 0x8002 discriminator so MeterReadings() accepts it.
func buildMeterPacket(readings []MeterReading) []byte {
	const (
		bitClass = 29
	)
	w0 := uint32(0)
	w0 |= 0 << 30       // packet type = data
	w0 |= 1 << bitClass // class id present
	// no trailer, no timestamp
	w1 := uint32(0)               // OUI / reserved
	w2 := uint32(meterClassIDLow) // info low 16 bits = 0x8002

	out := make([]byte, 0, 12+4*len(readings))
	var hb [4]byte
	binary.BigEndian.PutUint32(hb[:], w0)
	out = append(out, hb[:]...)
	binary.BigEndian.PutUint32(hb[:], w1)
	out = append(out, hb[:]...)
	binary.BigEndian.PutUint32(hb[:], w2)
	out = append(out, hb[:]...)

	for _, r := range readings {
		binary.BigEndian.PutUint16(hb[:2], r.Index)
		binary.BigEndian.PutUint16(hb[2:], uint16(r.Raw))
		out = append(out, hb[:]...)
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
	// Build a packet with a non-meter class id (0x8003 = spectrum).
	const spectrumClassIDLow uint16 = 0x8003
	w0 := uint32(1 << 29) // class present
	w1 := uint32(0)
	w2 := uint32(spectrumClassIDLow)
	out := make([]byte, 12)
	binary.BigEndian.PutUint32(out[0:4], w0)
	binary.BigEndian.PutUint32(out[4:8], w1)
	binary.BigEndian.PutUint32(out[8:12], w2)

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
	out := make([]byte, 12+6) // 6 payload bytes - not divisible by 4
	binary.BigEndian.PutUint32(out[0:4], w0)
	binary.BigEndian.PutUint32(out[4:8], 0)
	binary.BigEndian.PutUint32(out[8:12], uint32(meterClassIDLow))
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
	w1 := uint32(0)
	w2 := uint32(meterClassIDLow)
	// payload: one meter pair (4 bytes), then a 4-byte trailer
	out := make([]byte, 12+4+4)
	binary.BigEndian.PutUint32(out[0:4], w0)
	binary.BigEndian.PutUint32(out[4:8], w1)
	binary.BigEndian.PutUint32(out[8:12], w2)
	binary.BigEndian.PutUint16(out[12:14], 5)          // index
	binary.BigEndian.PutUint16(out[14:16], 6400)       // raw
	binary.BigEndian.PutUint32(out[16:20], 0xDEADBEEF) // trailer

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
	// class + integer ts + fractional ts.
	w0 := uint32(0)
	w0 |= 1 << 29    // class
	w0 |= 0b01 << 26 // TSI = 01 (integer ts present)
	w0 |= 0b10 << 24 // TSF = 10 (fractional ts present)
	w1 := uint32(0)
	w2 := uint32(meterClassIDLow)
	out := make([]byte, 12+4+8) // +4 int ts, +8 frac ts, no payload
	binary.BigEndian.PutUint32(out[0:4], w0)
	binary.BigEndian.PutUint32(out[4:8], w1)
	binary.BigEndian.PutUint32(out[8:12], w2)
	binary.BigEndian.PutUint32(out[12:16], 0x12345678)
	binary.BigEndian.PutUint64(out[16:24], 0x9ABCDEF012345678)

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
