package engine

import (
	"math"
	"testing"

	"pelcots/internal/config"
)

// TestTiltDecodeCalibrated verifies the linear tilt readback decode for a head
// whose tilt readback is a raw encoder count rather than Pelco-standard
// hundredths. The 303Z/3050DZ was calibrated live: raw = 22456 + 355.878*elev,
// i.e. raw_at_0 = 22456 and raw_at_90 = 54485. (The head was re-homed after an
// earlier calibration that read 13408/45437; the zero offset shifted by +9048
// counts, the gear-ratio scale is unchanged.) The engine derives scale and
// offset from the two calibration points and decodes raw counts to degrees.
func TestTiltDecodeCalibrated(t *testing.T) {
	eng := New(Options{
		Transport: config.TransportSim,
		Addr:      1,
		TiltCal:   config.TiltCalConfig{RawAt0: 22456, RawAt90: 54485},
	})
	if eng.tiltOffset != 22456 {
		t.Fatalf("tiltOffset = %v, want 22456", eng.tiltOffset)
	}
	wantScale := (54485 - 22456) / 90.0 // 355.8777...
	if math.Abs(eng.tiltScale-wantScale) > 1e-6 {
		t.Fatalf("tiltScale = %v, want %v", eng.tiltScale, wantScale)
	}
	cases := []struct {
		raw  uint16
		elev float64
	}{
		{22456, 0},  // horizon (explicit calibration point — the re-homed zero)
		{38470, 45}, // midpoint, derived from the linear map
		{54485, 90}, // vertical (explicit calibration point)
	}
	for _, c := range cases {
		got := eng.decodeTilt(c.raw)
		if math.Abs(got-c.elev) > 0.1 {
			t.Errorf("decodeTilt(%d) = %.3f, want %.3f", c.raw, got, c.elev)
		}
	}
}

// TestTiltDecodeStandard verifies that without tilt calibration the decode falls
// back to the Pelco standard (hundredths of a degree), so the in-memory simulator
// and any standard Pelco head keep working unchanged.
func TestTiltDecodeStandard(t *testing.T) {
	eng := New(Options{Transport: config.TransportSim, Addr: 1})
	if eng.tiltOffset != 0 || eng.tiltScale != 100 {
		t.Fatalf("uncalibrated tilt = offset %v scale %v, want 0/100", eng.tiltOffset, eng.tiltScale)
	}
	cases := []struct {
		raw  uint16
		elev float64
	}{
		{0, 0},
		{4500, 45},
		{9000, 90},
		{9869, 98.69},
	}
	for _, c := range cases {
		if got := eng.decodeTilt(c.raw); math.Abs(got-c.elev) > 1e-9 {
			t.Errorf("decodeTilt(%d) = %.3f, want %.3f", c.raw, got, c.elev)
		}
	}
}
