package flexradio

import (
	"math"
	"math/rand"
	"testing"
)

func TestConvertSource(t *testing.T) {
	cases := []struct {
		unit string
		raw  int16
		want float64
	}{
		{"dBm", 0, 0},
		{"dBm", 128, 1},
		{"dBm", -128, -1},
		{"dBFS", 256, 2},
		{"SWR", 1280, 10}, // SWR=10.0
		{"Volts", 2560, 10},
		{"Amps", 512, 2},
		{"degC", 640, 10},
		{"degF", -640, -10},
		{"Watts", 100, 100},   // unscaled
		{"Percent", 42, 42},   // unscaled (unknown->identity)
		{"unknownunit", 7, 7}, // unknown -> identity
	}
	for _, c := range cases {
		got := convertSource(c.unit, c.raw)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("convertSource(%q,%d) = %g, want %g", c.unit, c.raw, got, c.want)
		}
	}
}

func TestConvertRaw_dBmIdentity(t *testing.T) {
	// S-meter: source dBm, published dBm -> identity.
	def := MeterDef{Unit: "dBm", PublishUnit: "dBm"}
	got, unit := ConvertRaw("dBm", -7968, def) // -7968/128 = -62.25 dBm
	if unit != "dBm" {
		t.Errorf("unit = %q, want dBm", unit)
	}
	if math.Abs(got-(-62.25)) > 1e-9 {
		t.Errorf("got %g, want -62.25", got)
	}
}

func TestConvertRaw_FWDPWR_dBmToWatts(t *testing.T) {
	// FWDPWR reading 100 W = +50 dBm. raw = 50*128 = 6400.
	def := MeterDef{Unit: "dBm", PublishUnit: "W"}
	got, unit := ConvertRaw("dBm", 6400, def)
	if unit != "W" {
		t.Errorf("unit = %q, want W", unit)
	}
	if math.Abs(got-100) > 0.01 {
		t.Errorf("got %g W, want ~100 W", got)
	}
}

func TestConvertRaw_FWDPWR_lowPowerClampsToZero(t *testing.T) {
	// Very low dBm (< -60, i.e. sub-nanowatt) should clamp to 0 W to avoid
	// publishing meaningless 1e-30 dust when receiving.
	def := MeterDef{Unit: "dBm", PublishUnit: "W"}
	got, _ := ConvertRaw("dBm", -15808, def) // -123.5 dBm
	if got != 0 {
		t.Errorf("got %g W, want 0 (clamped)", got)
	}
}

func TestConvertRaw_SWR(t *testing.T) {
	// SWR raw 0x0100 (256) -> 256/128 = 2.0
	def := MeterDef{Unit: "SWR", PublishUnit: "SWR"}
	got, unit := ConvertRaw("SWR", 256, def)
	if unit != "SWR" {
		t.Errorf("unit = %q, want SWR", unit)
	}
	if math.Abs(got-2.0) > 1e-9 {
		t.Errorf("got %g, want 2.0", got)
	}
}

func TestDeadband(t *testing.T) {
	cases := map[string]float64{
		"dBm": 0.1, "dBFS": 0.1, "dB": 0.1,
		"SWR": 0.01, "Volts": 0.01, "Amps": 0.01,
		"degC": 0.1, "W": 0.1, "Watts": 0.1,
		"unknown": 0.1,
	}
	for unit, want := range cases {
		if got := Deadband(unit); got != want {
			t.Errorf("Deadband(%q) = %g, want %g", unit, got, want)
		}
	}
}

func TestMeterRegistry_RegisterWantedOnly(t *testing.T) {
	r := NewMeterRegistry()
	// Wanted meter: TX-/FWDPWR (firmware uses "TX-" source)
	if !r.Register(5, "TX-", 0, "FWDPWR") {
		t.Error("Register(TX-/FWDPWR) = false, want true")
	}
	// Unwanted meter: RAD/PACURRENT (excluded deliberately)
	if r.Register(6, "RAD", 0, "PACURRENT") {
		t.Error("Register(RAD/PACURRENT) = true, want false (excluded)")
	}
	if r.Count() != 1 {
		t.Errorf("Count = %d, want 1", r.Count())
	}

	rm, ok := r.LookupIndex(5)
	if !ok {
		t.Fatal("LookupIndex(5) not found")
	}
	if rm.Definition().Name != "FWDPWR" {
		t.Errorf("def name = %q, want FWDPWR", rm.Definition().Name)
	}
}

func TestMeterRegistry_Reset(t *testing.T) {
	r := NewMeterRegistry()
	r.Register(1, "TX-", 0, "FWDPWR")
	r.Register(2, "TX-", 0, "SWR")
	if r.Count() != 2 {
		t.Fatalf("Count = %d, want 2", r.Count())
	}
	r.Reset()
	if r.Count() != 0 {
		t.Errorf("after Reset, Count = %d, want 0", r.Count())
	}
}

func TestMeterRegistry_PerSliceMeters(t *testing.T) {
	// SLC/LEVEL appears once per slice; all should be registered.
	r := NewMeterRegistry()
	r.Register(10, "SLC", 0, "LEVEL")
	r.Register(11, "SLC", 1, "LEVEL")
	r.Register(12, "SLC", 2, "LEVEL")
	if r.Count() != 3 {
		t.Errorf("Count = %d, want 3 (one per slice)", r.Count())
	}
	for _, idx := range []uint16{10, 11, 12} {
		if _, ok := r.LookupIndex(idx); !ok {
			t.Errorf("LookupIndex(%d) not found", idx)
		}
	}
}

func TestWantedMeterKeys(t *testing.T) {
	keys := WantedMeterKeys()
	// 11 wanted meters (PATEMP removed; never streamed via VITA-49).
	want := 11
	if len(keys) != want {
		t.Errorf("len(keys) = %d, want %d", len(keys), want)
	}
}

func TestConvertSource_signedTwoComplement(t *testing.T) {
	// -16000 as int16 should yield -125.0 dBm after /128.
	got := convertSource("dBm", -16000)
	if math.Abs(got-(-125.0)) > 1e-9 {
		t.Errorf("got %g, want -125.0", got)
	}
}

// Randomized differential check: ConvertRaw must equal convertSource when
// source==publish unit.
func TestConvertRaw_identityFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, unit := range []string{"dBm", "dBFS", "SWR", "Volts", "Amps", "degC"} {
		for i := 0; i < 100; i++ {
			raw := int16(rng.Intn(1 << 16))
			def := MeterDef{Unit: unit, PublishUnit: unit}
			got, _ := ConvertRaw(unit, raw, def)
			want := convertSource(unit, raw)
			if math.Abs(got-want) > 1e-9 {
				t.Fatalf("unit=%s raw=%d got=%g want=%g", unit, raw, got, want)
			}
		}
	}
}
