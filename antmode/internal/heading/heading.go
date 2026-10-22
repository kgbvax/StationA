// SPDX-License-Identifier: AGPL-3.0-or-later

// Package heading computes the storm park azimuth: the Ultrabeam boom
// broadside to the wind, so the elements point into (and away from) it. On
// the 3 el 6–20 that is about 30 % less wind force than elements broadside,
// and it takes the bending load off the fibreglass element roots.
//
// Pure: no I/O, no clock. The table tests are the spec.
package heading

import "math"

// Inputs for Storm.
type Inputs struct {
	// WindDeg is the direction the wind comes from (meteorological, 0..360).
	WindDeg float64
	// WindOK is false when the wind is stale or missing → FallbackAz.
	WindOK bool
	// Az is the current rotator readback; AzOK false when unknown.
	Az   float64
	AzOK bool
	// MaxAz is the highest rotator target (360, or up to 450 once the G-450
	// overlap readback is verified).
	MaxAz int
	// FallbackAz is used when the wind is stale.
	FallbackAz int
}

// Storm returns the park azimuth in integer degrees: wind ± 90° (mod 360),
// plus the +360 variants that fit under MaxAz, whichever is nearest the
// current azimuth (the G-450 travels linearly and cannot cross its stop, so
// "nearest" is plain distance, not around the circle). Unknown azimuth →
// the lowest candidate. Stale wind → FallbackAz.
func Storm(in Inputs) int {
	if !in.WindOK || math.IsNaN(in.WindDeg) || math.IsInf(in.WindDeg, 0) {
		return in.FallbackAz
	}
	maxAz := in.MaxAz
	if maxAz < 360 {
		maxAz = 360
	}
	w := Norm(int(math.Round(in.WindDeg)))
	// A candidate of 0 also yields 360: on a 0..360 rotator both stops are
	// real targets for the same heading.
	var cands []int
	for _, c := range []int{Norm(w + 90), Norm(w + 270)} {
		cands = append(cands, c)
		if c+360 <= maxAz {
			cands = append(cands, c+360)
		}
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if !in.AzOK {
			if c < best {
				best = c
			}
			continue
		}
		db, dc := math.Abs(float64(best)-in.Az), math.Abs(float64(c)-in.Az)
		if dc < db || (dc == db && c < best) {
			best = c
		}
	}
	return best
}

// Norm folds a heading into [0, 360).
func Norm(d int) int {
	d %= 360
	if d < 0 {
		d += 360
	}
	return d
}
