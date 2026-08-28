package engine

import (
	"math"
	"testing"
)

func TestShortestDelta(t *testing.T) {
	cases := []struct {
		cur, tgt, want float64
	}{
		{0, 10, 10},
		{10, 0, -10},
		{350, 10, 20},  // forward across 0
		{10, 350, -20}, // backward across 0
		{0, 180, 180},  // antipodal resolves to +180
		{0, 190, -170}, // shorter to go backward
		{180, 0, 180},  // antipodal from the other side
		{0, 0, 0},
	}
	for _, c := range cases {
		if got := shortestDelta(c.cur, c.tgt); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("shortestDelta(%g,%g) = %g, want %g", c.cur, c.tgt, got, c.want)
		}
	}
}

func TestPlanMoveShort(t *testing.T) {
	// Well within ±270: take the short path.
	p := planMove(0, 0, 90, 270)
	if p.Kind != MoveShort || p.Dir != 1 || math.Abs(p.NewWrap-90) > 1e-9 {
		t.Fatalf("short move: %+v", p)
	}
}

func TestPlanMoveOverWindBlocked(t *testing.T) {
	// Wind already at +260 (limit 270). A short move to azimuth that adds +30
	// (cur 250 → tgt 280) would reach +290 > 270. SetPan only ever takes the
	// shortest physical path and the long way round is never commanded, so the
	// move is REFUSED — over-winding is relieved manually (jog + zero-wrap).
	p := planMove(260, 250, 280, 270)
	if p.Kind != MoveBlock {
		t.Fatalf("expected block, got %+v", p)
	}
	if p.Dir != 0 || p.NewWrap != 260 {
		t.Fatalf("blocked move must leave the plan and accumulator untouched: %+v", p)
	}
}

func TestPlanMoveBlock(t *testing.T) {
	// With a tight ±60 limit, azimuth 180 from cur 0 has a short path of +180,
	// which exceeds the limit → blocked.
	p := planMove(0, 0, 180, 60)
	if p.Kind != MoveBlock {
		t.Fatalf("expected block, got %+v", p)
	}
}

func TestPlanMoveNone(t *testing.T) {
	if p := planMove(123, 45, 45, 270); p.Kind != MoveNone {
		t.Fatalf("expected none, got %+v", p)
	}
}

func TestPlanMoveLargeWrap(t *testing.T) {
	// A wind accumulator far above the limit (a stale persisted value, a limit
	// lowered via SetWrap, or net wind accumulated while wrap was disabled):
	// with unwrap gone there is no representation to reach for, so every move
	// is refused until the operator manually unwinds and re-zeros the wind.
	p := planMove(1800, 0, 180, 270)
	if p.Kind != MoveBlock {
		t.Fatalf("expected block for a move under a large wind, got %+v", p)
	}
}

// TestAccumulatorAcrossZero verifies the readback-integration rule sums
// shortest deltas correctly across the 0/360 boundary.
func TestAccumulatorAcrossZero(t *testing.T) {
	wind := 0.0
	samples := []float64{350, 355, 359, 3, 8} // crossing 360→0 going CW
	for i := 1; i < len(samples); i++ {
		wind += shortestDelta(samples[i-1], samples[i])
	}
	if math.Abs(wind-18) > 1e-9 { // 350 → 8 forward = +18°
		t.Fatalf("accumulated %g, want 18", wind)
	}
}
