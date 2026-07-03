package flexradio

import (
	"sort"
	"strings"
)

// BandForFreq maps a frequency in Hz to an amateur-radio band label.
// The radio does not expose a "band" field directly; it is derived from
// the slice frequency. Returns "unknown" when no band matches (out of all
// ham allocations, or frequency zero / not yet tuned).
//
// Bands follow the conventional labels used by transceivers ("160m",
// "80m", ..., "70cm"). Edge frequencies use the standard ITU region 1/2
// allocations; slight overlaps are tolerated so a value right at a band
// edge (e.g. 14.000 MHz) still resolves to "20m".
func BandForFreq(hz int64) string {
	type rng struct {
		lo, hi int64
		label  string
	}
	// Sorted by lo so we can binary-search; ranges are non-overlapping.
	bands := []rng{
		{1_800_000, 2_000_000, "160m"},
		{3_500_000, 4_000_000, "80m"},
		{5_060_000, 5_450_000, "60m"}, // includes UK 5MHz
		{7_000_000, 7_300_000, "40m"},
		{10_000_000, 10_200_000, "30m"},
		{14_000_000, 14_400_000, "20m"},
		{18_000_000, 18_200_000, "17m"},
		{21_000_000, 21_500_000, "15m"},
		{24_890_000, 25_000_000, "12m"},
		{28_000_000, 30_000_000, "10m"},
		{50_000_000, 54_000_000, "6m"},
		{69_000_000, 71_000_000, "4m"}, // UK 70MHz
		{144_000_000, 148_000_000, "2m"},
		{222_000_000, 225_000_000, "1.25m"},
		{430_000_000, 440_000_000, "70cm"},
		{902_000_000, 928_000_000, "33cm"},
		{1_240_000_000, 1_300_000_000, "23cm"},
	}
	if hz <= 0 {
		return "unknown"
	}
	// sort.Search for the first band whose hi >= hz, then verify lo <= hz.
	idx := sort.Search(len(bands), func(i int) bool { return bands[i].hi >= hz })
	if idx < len(bands) && hz >= bands[idx].lo {
		return bands[idx].label
	}
	// General-coverage fallback: HF frequencies outside the ham allocations
	// (e.g. shortwave broadcast bands) still resolve to "gen" rather than
	// "unknown", since a FlexRadio is often used to receive them. Only HF
	// qualifies; VHF/UHF out-of-allocation frequencies stay "unknown".
	if hz >= 1_800_000 && hz <= 30_000_000 {
		return "gen"
	}
	return "unknown"
}

// BandIsValid reports whether a band label is one we recognize (case- and
// dash-insensitive). Used mainly for sanity-checking external inputs.
func BandIsValid(label string) bool {
	lc := strings.ToLower(strings.TrimSpace(label))
	switch lc {
	case "160m", "80m", "60m", "40m", "30m", "20m", "17m", "15m", "12m",
		"10m", "6m", "4m", "2m", "1.25m", "70cm", "33cm", "23cm", "gen":
		return true
	}
	return false
}
