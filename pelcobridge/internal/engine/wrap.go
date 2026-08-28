package engine

import "math"

// MoveKind classifies how an absolute azimuth move must be carried out under
// cable-wrap protection.
type MoveKind int

const (
	// MoveNone means the azimuth is already at the target (no pan motion).
	MoveNone MoveKind = iota
	// MoveShort means the shortest path stays within the wrap limit — the unit
	// may take it directly.
	MoveShort
	// MoveUnwrap means the shortest path would exceed the limit, so the move is
	// driven the long way round (a full-rotation unwrap) to relieve the cable.
	MoveUnwrap
	// MoveBlock means no representation of the target lies within the limit:
	// the move is refused.
	MoveBlock
)

// MovePlan is the decision for one absolute azimuth move.
type MovePlan struct {
	Kind    MoveKind
	Dir     int     // pan travel direction: -1 CCW, 0 none, +1 CW
	Travel  float64 // signed degrees to travel (NewWrap - wrap)
	NewWrap float64 // resulting signed accumulator after the move completes
}

// norm360 wraps an angle in degrees into [0, 360).
func norm360(deg float64) float64 {
	deg = math.Mod(deg, 360)
	if deg < 0 {
		deg += 360
	}
	return deg
}

// shortestDelta returns the signed shortest angular travel (in (-180, 180]) to
// rotate from cur to tgt degrees. Antipodal targets resolve to +180.
func shortestDelta(cur, tgt float64) float64 {
	d := math.Mod(tgt-cur, 360)
	if d > 180 {
		d -= 360
	}
	if d <= -180 {
		d += 360
	}
	return d
}

func sign(x float64) int {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}

// planMove decides how to reach azimuth tgt from current azimuth cur given the
// signed cable-wind accumulator wrap and a ± wrap limit. It picks the target
// representation (tgt + k·360 in accumulator space) of least travel whose
// absolute wind stays within the limit; if the nearest (shortest-path)
// representation is within the limit it is Short, otherwise a farther one is
// forced (Unwrap), and if none is reachable it is Block.
func planMove(wrap, cur, tgt, limit float64) MovePlan {
	short := shortestDelta(cur, tgt)
	if short == 0 {
		return MovePlan{Kind: MoveNone, Dir: 0, NewWrap: wrap}
	}
	base := wrap + short // shortest-path representation in accumulator space
	if math.Abs(base) <= limit {
		return MovePlan{Kind: MoveShort, Dir: sign(short), Travel: short, NewWrap: base}
	}
	// Shortest path over-wraps; search neighbouring full-turn representations
	// for the one of least travel that respects the limit. The accumulator can
	// be far from zero (a stale persisted wind, a limit lowered via SetWrap, or
	// net wind accumulated while wrap was disabled), so centre the search on the
	// representation nearest zero rather than a fixed k=-4..4 window that would
	// miss valid reps and wrongly return MoveBlock for reachable targets.
	k0 := int(math.Round(-base / 360))
	best, found := 0.0, false
	for k := k0 - 4; k <= k0+4; k++ {
		w := base + float64(k)*360
		if math.Abs(w) <= limit && (!found || math.Abs(w-wrap) < math.Abs(best-wrap)) {
			best, found = w, true
		}
	}
	if !found {
		return MovePlan{Kind: MoveBlock, Dir: 0, NewWrap: wrap}
	}
	travel := best - wrap
	return MovePlan{Kind: MoveUnwrap, Dir: sign(travel), Travel: travel, NewWrap: best}
}
