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
		{5_060_000, "60m"}, // UK 5MHz allocation low edge
		{5_250_000, "60m"}, // UK 5MHz allocation mid
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
