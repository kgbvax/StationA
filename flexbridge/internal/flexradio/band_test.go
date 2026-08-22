package flexradio

import "testing"

func TestBandForFreq(t *testing.T) {
	cases := []struct {
		hz   int64
		want string
	}{
		{1_810_000, "160m"},
		{3_600_000, "80m"},
		{7_100_000, "40m"},
		{10_120_000, "30m"},
		{14_100_000, "20m"},
		{14_000_000, "20m"}, // band edge
		{14_350_000, "20m"},
		{18_110_000, "17m"},
		{21_200_000, "15m"},
		{24_920_000, "12m"},
		{28_500_000, "10m"},
		{50_000_000, "6m"}, // 6m band edge
		{50_150_000, "6m"},
		{52_000_000, "6m"},
		{54_000_000, "6m"},      // 6m band edge (inclusive)
		{54_100_000, "unknown"}, // just above 6m, below 4m -> unknown
		{145_500_000, "2m"},
		{433_500_000, "70cm"},
		{1_296_000_000, "23cm"},
		{0, "unknown"},
		{-1, "unknown"},
		{5_355_000, "60m"}, // 60m allocation (5351.5–5366.5 kHz)
		{5_060_000, "gen"}, // 5.06 MHz — HF, outside the 60m allocation → gen
		{5_250_000, "gen"}, // 5.25 MHz — HF, outside the 60m allocation → gen
	}
	for _, c := range cases {
		if got := BandForFreq(c.hz); got != c.want {
			t.Errorf("BandForFreq(%d) = %q, want %q", c.hz, got, c.want)
		}
	}
}

func TestBandForFreq_OutsideAllocations(t *testing.T) {
	// A shortwave-broadcast-ish frequency (no ham allocation) resolves to
	// "gen" (general coverage) on HF, not "unknown".
	if got := BandForFreq(9_900_000); got != "gen" {
		t.Errorf("BandForFreq(9_900_000) = %q, want gen", got)
	}
	// Something clearly out of range.
	if got := BandForFreq(100_000_000); got != "unknown" {
		t.Errorf("BandForFreq(100 MHz) = %q, want unknown", got)
	}
}

func TestBandForFreqWithPrev_Hysteresis(t *testing.T) {
	// 17m band is 18_068_000 – 18_168_000. VFO dither at 18_166–18_168 must stay 17m.
	cases := []struct {
		prev string
		hz   int64
		want string
	}{
		{"", 18_166_000, "17m"},    // inside 17m, no prev
		{"", 18_169_000, "gen"},    // just above 17m, no prev → gen
		{"17m", 18_169_000, "17m"}, // just above 17m, prev 17m, within hysteresis
		{"17m", 18_171_000, "gen"}, // above hysteresis window
		{"17m", 18_167_000, "17m"}, // still inside 17m
		{"20m", 18_169_000, "gen"}, // just above 17m, prev 20m → gen (not sticky to a different band)
		{"20m", 14_001_000, "20m"}, // just below 20m low edge, prev 20m → sticky
		{"20m", 13_997_000, "gen"}, // below hysteresis window
		{"gen", 18_169_000, "gen"}, // prev gen, just above 17m → gen
		{"gen", 18_167_000, "17m"}, // prev gen, inside 17m → 17m
	}
	for _, c := range cases {
		if got := BandForFreqWithPrev(c.hz, c.prev); got != c.want {
			t.Errorf("BandForFreqWithPrev(%d, %q) = %q, want %q", c.hz, c.prev, got, c.want)
		}
	}
}

func TestBandForFreqWithPrev_InvalidPrev(t *testing.T) {
	// Unknown previous band is treated as no previous band.
	if got := BandForFreqWithPrev(18_169_000, "notaband"); got != "gen" {
		t.Errorf("BandForFreqWithPrev(18_169_000, notaband) = %q, want gen", got)
	}
}

func TestBandIsValid(t *testing.T) {
	valid := []string{"20m", "80m", "70cm", "gen", "GEN", "  20m  "}
	for _, b := range valid {
		if !BandIsValid(b) {
			t.Errorf("BandIsValid(%q) = false, want true", b)
		}
	}
	if BandIsValid("99m") {
		t.Error("BandIsValid(\"99m\") = true, want false")
	}
}

func TestBandNumberFor(t *testing.T) {
	cases := []struct {
		label string
		want  int
		ok    bool
	}{
		{"20m", 20, true},
		{"160m", 160, true},
		{"6m", 6, true},
		{"60m", 60, true},
		{"17m", 17, true},
		{"10m", 10, true},
		{"  40m ", 40, true}, // trimmed
		{"80M", 80, true},    // case-insensitive
		// Out of scope: synthetic labels and XVTR (VHF/UHF) bands.
		{"gen", 0, false},
		{"unknown", 0, false},
		{"2m", 0, false},
		{"70cm", 0, false},
		{"23cm", 0, false},
		{"99m", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := BandNumberFor(c.label)
		if got != c.want || ok != c.ok {
			t.Errorf("BandNumberFor(%q) = (%d, %v), want (%d, %v)", c.label, got, ok, c.want, c.ok)
		}
	}
}
