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
	// MoveBlock means the shortest path would exceed the limit: the move is
	// refused. The absolute SetPan opcode always takes the shortest physical
	// path and the unit cannot be told to travel the long way round, so
	// over-winding can only be relieved manually (jog + zero-wrap).
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
// signed cable-wind accumulator wrap and a ± wrap limit. Only the shortest-path
// representation is ever commanded: the absolute SetPan opcode takes the
// shortest physical path and the unit cannot be told to travel the long way
// round, so if that path would exceed the limit the move is refused (Block) and
// over-winding must be relieved manually (TUI jog + zero-wrap).
func planMove(wrap, cur, tgt, limit float64) MovePlan {
	short := shortestDelta(cur, tgt)
	if short == 0 {
		return MovePlan{Kind: MoveNone, Dir: 0, NewWrap: wrap}
	}
	newWrap := wrap + short
	if math.Abs(newWrap) > limit {
		return MovePlan{Kind: MoveBlock, Dir: 0, NewWrap: wrap}
	}
	return MovePlan{Kind: MoveShort, Dir: sign(short), Travel: short, NewWrap: newWrap}
}
