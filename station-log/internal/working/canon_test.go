package working

import "testing"

func TestBandOfHz(t *testing.T) {
	cases := []struct {
		freq int64
		want string
	}{
		{1_800_000, "160m"}, {3_600_000, "80m"}, {7_074_000, "40m"}, {10_118_000, "30m"},
		{14_023_000, "20m"}, {14_350_000, "20m"}, {14_360_000, "gen"}, // HF general-coverage gap
		{18_100_000, "17m"}, {21_200_000, "15m"}, {24_950_000, "12m"}, {28_400_000, "10m"},
		{50_313_000, "6m"}, {144_300_000, "2m"},
		{3_200_000, "gen"}, {1_500_000, ""}, {0, ""}, {-100, ""},
	}
	for _, c := range cases {
		if got := BandOfHz(c.freq); got != c.want {
			t.Errorf("BandOfHz(%d) = %q, want %q", c.freq, got, c.want)
		}
	}
}

func TestCanonicalMode(t *testing.T) {
	cases := []struct {
		raw  string
		freq int64
		want string
	}{
		{"CW", 0, "cw"}, {"cw", 0, "cw"}, {"USB", 0, "usb"}, {"LSB", 0, "lsb"},
		{"USB-D", 0, "usb"}, {"LSB-D", 0, "lsb"}, {"FM-D", 0, "fm"}, // reference exception: carrier, not data
		{"SSB", 14_100_000, "usb"}, {"SSB", 7_100_000, "lsb"}, {"AM", 0, "am"}, {"FM", 0, "fm"},
		{"RTTY", 0, "data"}, {"DIGI", 0, "data"}, {"FT8", 0, "data"}, {"DIGITAL", 0, "data"},
		{"", 0, ""}, {"warez", 0, "data"},
	}
	for _, c := range cases {
		if got := CanonicalMode(c.raw, c.freq); got != c.want {
			t.Errorf("CanonicalMode(%q, %d) = %q, want %q", c.raw, c.freq, got, c.want)
		}
	}
}

func TestNormalizeBand(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"20m", "20m"}, {" 40M ", "40m"}, {"14.0", ""}, {"14", ""}, {"khz", ""}, {"", ""}, {"6m", "6m"},
	}
	for _, c := range cases {
		if got := NormalizeBand(c.raw); got != c.want {
			t.Errorf("NormalizeBand(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}
