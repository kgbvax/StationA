package acom

import "testing"

func TestParseTelemetryOutOfRangeBandIsUnknown(t *testing.T) {
	// Regression: a band byte of 0x0B (low nibble 11) is "UNK" per decodeBand,
	// but BandIndex must also be 0 (unknown) so SetBand's current-band guard
	// rejects navigation from a garbage index instead of walking the amp.
	data := make([]byte, 72)
	data[3] = 0x60  // OPR/RX
	data[66] = 0xFF // no fault
	data[69] = 0x0B // out-of-range band nibble
	obs, ok := ParseTelemetry(data, nil)
	if !ok {
		t.Fatal("72-byte frame should parse")
	}
	if obs.BandIndex != 0 {
		t.Errorf("BandIndex = %d, want 0 (unknown) for out-of-range band byte", obs.BandIndex)
	}
	if obs.BandName != "UNK" {
		t.Errorf("BandName = %q, want UNK", obs.BandName)
	}
}

func TestParseTelemetryValidBandIndex(t *testing.T) {
	data := make([]byte, 72)
	data[3] = 0x60
	data[66] = 0xFF
	data[69] = 0x05 // 20m = index 5
	obs, ok := ParseTelemetry(data, nil)
	if !ok {
		t.Fatal("72-byte frame should parse")
	}
	if obs.BandIndex != 5 || obs.BandName != "20m" {
		t.Errorf("band = %d/%q, want 5/20m", obs.BandIndex, obs.BandName)
	}
}

func TestParseTelemetryShortFrameRejected(t *testing.T) {
	if _, ok := ParseTelemetry(make([]byte, 71), nil); ok {
		t.Error("71-byte frame should be rejected (want exactly 72)")
	}
}
