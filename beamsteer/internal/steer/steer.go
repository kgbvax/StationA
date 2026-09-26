// SPDX-License-Identifier: AGPL-3.0-or-later

// Package steer is the pure smart-rotation decision: given the bearing the
// contest logger asked for, the rotator position and the Ultrabeam direction,
// it picks the cheapest way to put a lobe on the station. No I/O.
//
//   - forward/reverse: the lobe is ±Lobe around the boom heading (front) and
//     the heading+180 (back). Station in the front lobe → forward; in the back
//     lobe → reverse. Both are a direction change only, no rotation.
//     Otherwise rotate, to the lobe that needs the least rotator travel
//     (forward on b, or reverse on b+180).
//   - bidirectional: both lobes are live and ±BidirLobe wide. Never switch
//     direction. Station in either lobe → nothing; otherwise rotate to the
//     nearer of b / b+180.
//   - 6m: the Ultrabeam has no reverse on 6m, so only forward candidates.
//
// Travel is linear (|target − az|), not circular: the G-450 cannot rotate
// through its end stop, so 350° from 10° is 340° of travel, not 20°.
package steer

import (
	"fmt"
	"math"
)

// Ultrabeam directions (ultrabridge /state.direction vocabulary).
const (
	Forward       = "forward"
	Reverse       = "reverse"
	Bidirectional = "bidirectional"
)

// Inputs is everything one decision needs.
type Inputs struct {
	Bearing   float64 // requested great-circle bearing, degrees true
	Az        float64 // rotator position (boom heading), raw rotator degrees
	Direction string  // current Ultrabeam direction; "" = unknown
	Band      string  // radio band ("6m" disables reverse)

	Lobe      float64 // half-width of a forward/reverse lobe, degrees
	BidirLobe float64 // half-width of each bidirectional lobe, degrees
	MaxAz     float64 // highest rotator target allowed (360, or 450 with overlap)
}

// Decision is what to do. A nil RotateTo means do not rotate; an empty
// Direction means do not change direction.
type Decision struct {
	RotateTo  *float64
	Direction string
	Reason    string
}

// Norm maps any angle to [0, 360).
func Norm(a float64) float64 {
	a = math.Mod(a, 360)
	if a < 0 {
		a += 360
	}
	return a
}

// Diff is the unsigned smallest angle between a and b, in [0, 180].
func Diff(a, b float64) float64 {
	d := math.Abs(Norm(a) - Norm(b))
	if d > 180 {
		d = 360 - d
	}
	return d
}

// EffectiveHeading is where the main lobe points: the boom heading, or the
// boom heading +180 when the beam is reversed.
func EffectiveHeading(az float64, direction string) float64 {
	if direction == Reverse {
		return Norm(az + 180)
	}
	return Norm(az)
}

// Decide picks the action for one logger rotate request.
func Decide(in Inputs) Decision {
	b := Norm(in.Bearing)
	on6m := in.Band == "6m"

	switch in.Direction {
	case Bidirectional:
		if Diff(b, in.Az) <= in.BidirLobe {
			return Decision{Reason: "in bidirectional front lobe"}
		}
		if Diff(b, in.Az+180) <= in.BidirLobe {
			return Decision{Reason: "in bidirectional back lobe"}
		}
		t := nearest(in, b, Norm(b+180))
		return Decision{RotateTo: &t, Reason: fmt.Sprintf("bidirectional: rotate to %.0f", t)}

	case Forward, Reverse:
		if Diff(b, in.Az) <= in.Lobe {
			if in.Direction == Forward {
				return Decision{Reason: "in front lobe"}
			}
			return Decision{Direction: Forward, Reason: "in front lobe: switch to forward"}
		}
		if !on6m && Diff(b, in.Az+180) <= in.Lobe {
			if in.Direction == Reverse {
				return Decision{Reason: "in back lobe"}
			}
			return Decision{Direction: Reverse, Reason: "behind: switch to 180°"}
		}
		fwd, fwdCost := best(in, b)
		if on6m {
			return rotate(in, fwd, Forward, "6m")
		}
		rev, revCost := best(in, Norm(b+180))
		if revCost < fwdCost {
			return rotate(in, rev, Reverse, "")
		}
		return rotate(in, fwd, Forward, "")

	default:
		// Direction unknown (ant-ctrl offline): plain rotation, like the
		// logger would get without beamsteer.
		t, _ := best(in, b)
		return Decision{RotateTo: &t, Reason: fmt.Sprintf("direction unknown: rotate to %.0f", t)}
	}
}

func rotate(in Inputs, target float64, dir, note string) Decision {
	d := Decision{RotateTo: &target}
	if dir != in.Direction {
		d.Direction = dir
	}
	label := "fwd"
	if dir == Reverse {
		label = "180°"
	}
	d.Reason = fmt.Sprintf("rotate to %.0f %s", target, label)
	if note != "" {
		d.Reason += " (" + note + ")"
	}
	return d
}

// nearest returns whichever of the two targets needs the least travel.
func nearest(in Inputs, t1, t2 float64) float64 {
	a, ca := best(in, t1)
	b, cb := best(in, t2)
	if cb < ca {
		return b
	}
	return a
}

// best returns the rotator target for heading t (0..360) with the least
// travel from the current position, using the overlap (t+360) when MaxAz
// allows it. Targets are whole degrees.
func best(in Inputs, t float64) (float64, float64) {
	t = math.Round(t)
	if t >= 360 {
		t -= 360
	}
	target, cost := t, math.Abs(t-in.Az)
	if alt := t + 360; alt <= in.MaxAz {
		if c := math.Abs(alt - in.Az); c < cost {
			target, cost = alt, c
		}
	}
	return target, cost
}
