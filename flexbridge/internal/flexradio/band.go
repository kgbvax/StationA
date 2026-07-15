package flexradio

import (
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
// Band edges are inclusive on both ends.
//
// Return rules:
//   - A frequency inside a ham allocation → that band's canonical label
//     (e.g. 14.1 MHz → "20m").
//   - A frequency in the HF general-coverage range (1.8–30 MHz) that is not
//     inside a ham allocation → "gen" (e.g. 9.9 MHz shortwave broadcast).
//   - Anything else outside the allocations (VHF/UHF gaps, non-HF) → "unknown".
//   - Zero or negative frequencies → "unknown".
func BandForFreq(hz int64) string {
	type rng struct {
		lo, hi int64
		label  string
	}
	// Sorted by lo so we can binary-search; ranges are non-overlapping.
	bands := []rng{
		{1_800_000, 2_000_000, "160m"},
		{3_500_000, 4_000_000, "80m"},
		{5_351_500, 5_366_500, "60m"},
		{7_000_000, 7_300_000, "40m"},
		{10_100_000, 10_150_000, "30m"},
		{14_000_000, 14_350_000, "20m"},
		{18_068_000, 18_168_000, "17m"},
		{21_000_000, 21_450_000, "15m"},
		{24_890_000, 24_990_000, "12m"},
		{28_000_000, 29_700_000, "10m"},
		{50_000_000, 54_000_000, "6m"},
		{144_000_000, 146_000_000, "2m"},
		{430_000_000, 440_000_000, "70cm"},
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
	// HF general-coverage (no ham allocation): 1.8–30 MHz → "gen".
	if hz >= 1_800_000 && hz <= 30_000_000 {
		return "gen"
	}
	return "unknown"
}

// BandIsValid reports whether a band label is one we recognize (case- and
// dash-insensitive), including the "gen" general-coverage label. Used mainly
// for sanity-checking external inputs.
func BandIsValid(label string) bool {
	lc := strings.ToLower(strings.TrimSpace(label))
	switch lc {
	case "160m", "80m", "60m", "40m", "30m", "20m", "17m", "15m", "12m",
		"10m", "6m", "2m", "70cm", "23cm", "gen":
		return true
	}
	return false
}