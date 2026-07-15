package flexradio

import (
	"fmt"
	"sort"
	"strings"
)

// BandForFreq maps a frequency in Hz to the canonical band label. The radio
// does not expose a "band" field directly; it is derived from the slice
// frequency.
//
// The table is the canonical band reference
// (../../docs/conventions/band-mode-reference.md, DL / IARU Region 1 edges);
// when that reference changes this table needs the corresponding update.
// Frequencies outside the table resolve to the canonical fallback
// "band-<N>" (N ≈ wavelength in metres — implementation-specific, do not
// rely on it downstream). Returns "" for zero/invalid frequencies so the
// caller can omit the band field.
func BandForFreq(hz int64) string {
	type rng struct {
		lo, hi int64
		label  string
	}
	// Sorted by lo so we can binary-search; ranges are non-overlapping.
	bands := []rng{
		{1_800_000, 1_999_999, "160m"},
		{3_500_000, 3_999_999, "80m"},
		{5_351_500, 5_366_500, "60m"},
		{7_000_000, 7_299_999, "40m"},
		{10_100_000, 10_149_999, "30m"},
		{14_000_000, 14_349_999, "20m"},
		{18_068_000, 18_167_999, "17m"},
		{21_000_000, 21_449_999, "15m"},
		{24_890_000, 24_989_999, "12m"},
		{28_000_000, 29_699_999, "10m"},
		{50_000_000, 53_999_999, "6m"},
		{144_000_000, 146_000_000, "2m"},
		{430_000_000, 440_000_000, "70cm"},
		{1_240_000_000, 1_300_000_000, "23cm"},
	}
	if hz <= 0 {
		return ""
	}
	// sort.Search for the first band whose hi >= hz, then verify lo <= hz.
	idx := sort.Search(len(bands), func(i int) bool { return bands[i].hi >= hz })
	if idx < len(bands) && hz >= bands[idx].lo {
		return bands[idx].label
	}
	// Canonical fallback: band-<N> with N the approximate wavelength in
	// metres. Covers general-coverage RX (shortwave broadcast) as well as
	// out-of-allocation VHF/UHF.
	return fmt.Sprintf("band-%d", 300_000_000/hz)
}

// BandIsValid reports whether a band label is one we recognize (case- and
// dash-insensitive), including the band-<N> fallback form. Used mainly for
// sanity-checking external inputs.
func BandIsValid(label string) bool {
	lc := strings.ToLower(strings.TrimSpace(label))
	switch lc {
	case "160m", "80m", "60m", "40m", "30m", "20m", "17m", "15m", "12m",
		"10m", "6m", "2m", "70cm", "23cm":
		return true
	}
	return strings.HasPrefix(lc, "band-")
}
