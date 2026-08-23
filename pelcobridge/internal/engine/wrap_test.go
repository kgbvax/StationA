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

func TestPlanMoveUnwrap(t *testing.T) {
	// Wind already at +260 (limit 270). A short move to azimuth that adds +30
	// (cur 250 → tgt 280≡-80... use explicit): cur=250°, tgt=280° short=+30 →
	// would reach +290 > 270, so unwrap the long way (-330) to +... check.
	p := planMove(260, 250, 280, 270)
	if p.Kind != MoveUnwrap {
		t.Fatalf("expected unwrap, got %+v", p)
	}
	if p.Dir != -1 {
		t.Fatalf("expected to unwind negative (long way), got dir %d (%+v)", p.Dir, p)
	}
	if math.Abs(p.NewWrap) > 270+1e-9 {
		t.Fatalf("unwrap result %g exceeds limit", p.NewWrap)
	}
	// New wind must correspond to the same azimuth (mod 360) as the short path.
	short := 260 + shortestDelta(250, 280)
	if d := math.Mod(math.Abs(p.NewWrap-short), 360); d > 1e-6 && math.Abs(d-360) > 1e-6 {
		t.Fatalf("unwrap target %g not a full-turn image of %g", p.NewWrap, short)
	}
}

func TestPlanMoveBlock(t *testing.T) {
	// With a tight ±60 limit, azimuth 180 from cur 0 has no representation
	// within the limit (±180, ±... all exceed 60) → blocked.
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
	// A wind accumulator far above the limit (stale persisted value, a limit
	// lowered via SetWrap, or net wind accumulated while wrap was disabled).
	// The target is still reachable by unwinding the long way; the search must
	// not give up and return MoveBlock for a reachable target.
	p := planMove(1800, 0, 180, 270)
	if p.Kind != MoveUnwrap {
		t.Fatalf("expected unwrap for reachable target under large wind, got %+v", p)
	}
	if math.Abs(p.NewWrap) > 270+1e-9 {
		t.Fatalf("unwrap result %g exceeds limit", p.NewWrap)
	}
	// The resulting wind must be a full-turn image of the shortest-path result.
	short := 1800 + shortestDelta(0, 180)
	if d := math.Mod(math.Abs(p.NewWrap-short), 360); d > 1e-6 && math.Abs(d-360) > 1e-6 {
		t.Fatalf("unwrap target %g not a full-turn image of %g", p.NewWrap, short)
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
