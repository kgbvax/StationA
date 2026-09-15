package canon

import "testing"

func TestBandFor(t *testing.T) {
	cases := []struct {
		freq int64
		want string
	}{
		{1_800_000, "160m"},
		{2_000_000, "160m"},
		{3_600_000, "80m"},
		{5_357_000, "60m"},
		{7_074_000, "40m"},
		{10_118_000, "30m"},
		{14_023_000, "20m"},
		{14_350_000, "20m"},
		{14_360_000, "gen"}, // HF general-coverage gap
		{18_100_000, "17m"},
		{21_200_000, "15m"},
		{24_950_000, "12m"},
		{28_400_000, "10m"},
		{50_313_000, "6m"},
		{1_500_000, ""},    // below 160m
		{3_200_000, "gen"}, // inside 1.8–30, outside allocations
		{70_000_000, ""},   // outside HF general coverage
		{0, ""},
		{-100, ""},
	}
	for _, c := range cases {
		if got := BandFor(c.freq); got != c.want {
			t.Errorf("BandFor(%d) = %q, want %q", c.freq, got, c.want)
		}
	}
}

func TestCanonicalMode(t *testing.T) {
	cases := []struct {
		raw  string
		freq int64
		want string
	}{
		{"CW", 0, "cw"},
		{"cw", 0, "cw"},
		{"USB", 0, "usb"},
		{"LSB", 0, "lsb"},
		{"SSB", 14_100_000, "usb"}, // ≥ 10 MHz → usb
		{"SSB", 7_100_000, "lsb"},
		{"AM", 0, "am"},
		{"FM", 0, "fm"},
		{"RTTY", 0, "data"},
		{"DIGI", 0, "data"},
		{"FT8", 0, "data"},
		{"DIGITAL", 0, "data"},
		{"", 0, ""},
		{"warez", 0, "data"}, // unrecognized non-empty collapses to the data family
	}
	for _, c := range cases {
		if got := CanonicalMode(c.raw, c.freq); got != c.want {
			t.Errorf("CanonicalMode(%q, %d) = %q, want %q", c.raw, c.freq, got, c.want)
		}
	}
}

func TestNormalizeBand(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"20m", "20m"},
		{" 40M ", "40m"},
		{"14.0", ""}, // DXLog MHz form: rejected, derive from freq instead
		{"14", ""},   // no unit
		{"khz", ""},  // non-numeric
		{"", ""},
		{"6m", "6m"},
	}
	for _, c := range cases {
		if got := NormalizeBand(c.raw); got != c.want {
			t.Errorf("NormalizeBand(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}
