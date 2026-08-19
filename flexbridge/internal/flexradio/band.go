package flexradio

import (
	"sort"
	"strings"
)

// BandEdgeHysteresisHz is the frequency guard band applied around ham-band
// edges when a previous band is known. Once the radio is reporting a ham band,
// small excursions outside that band (up to this distance past the edge) keep the
// reported band stable. This prevents antenna-switch / PA-follow chatter when the
// VFO noise hovers right at a band edge (observed live on 17m near 18.168 MHz).
const BandEdgeHysteresisHz = 2_000

type bandRange struct {
	lo, hi int64
	label  string
}

// canonicalBands is the canonical band reference
// (../../docs/conventions/band-mode-reference.md, DL / IARU Region 1 edges).
// Sorted by lo; ranges are non-overlapping and band edges are inclusive.
var canonicalBands = []bandRange{
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
	bands := canonicalBands
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

// BandForFreqWithPrev is like BandForFreq but applies a small hysteresis around
// the previous band's edges. If the previous band was a ham allocation and the
// new frequency is just outside that allocation (within BandEdgeHysteresisHz),
// the previous band is returned. This keeps radio/state.band stable when the VFO
// dithers near a band edge, and downstream antenna/PA consumers do not chatter.
// Transitions from "gen"/"unknown" into a ham band switch immediately; only
// exits from a ham band are hysteresis-guarded.
func BandForFreqWithPrev(hz int64, prev string) string {
	candidate := BandForFreq(hz)
	// If we don't have a stable previous ham band, or the candidate is the same,
	// or the candidate is already the no-information case, return immediately.
	if prev == "" || prev == candidate || prev == "unknown" || candidate == "unknown" {
		return candidate
	}
	// If the candidate is another ham band (not gen/unknown), switch immediately.
	// Hysteresis only guards exits from a ham band into the general-coverage gap.
	if candidate != "gen" {
		return candidate
	}
	lo, hi, ok := bandEdges(prev)
	if !ok {
		return candidate
	}
	// Frequency just above the previous band's upper edge, still within hysteresis.
	if hz > hi && hz <= hi+BandEdgeHysteresisHz {
		return prev
	}
	// Frequency just below the previous band's lower edge, still within hysteresis.
	if hz < lo && hz >= lo-BandEdgeHysteresisHz {
		return prev
	}
	return candidate
}

// bandEdges returns the inclusive edges for a known ham-band label. ok is false
// for "gen", "unknown", or unrecognized labels.
func bandEdges(label string) (lo, hi int64, ok bool) {
	for _, b := range canonicalBands {
		if b.label == label {
			return b.lo, b.hi, true
		}
	}
	return 0, 0, false
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