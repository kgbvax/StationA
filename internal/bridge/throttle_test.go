package bridge

import (
	"math"
	"testing"
	"time"

	"flex2mqtt/internal/flexradio"
)

// fakeClock is a controllable clock for tests.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time      { return f.t }
func (f *fakeClock) add(d time.Duration) { f.t = f.t.Add(d) }

func newGate() (*Gate, *fakeClock) {
	cl := &fakeClock{t: time.Unix(1000, 0)}
	g := NewGate(map[flexradio.MeterGroup]time.Duration{
		flexradio.GroupTX:    500 * time.Millisecond,
		flexradio.GroupRX:    1000 * time.Millisecond,
		flexradio.GroupAudio: 500 * time.Millisecond,
		flexradio.GroupHW:    10 * time.Second,
	})
	g.SetNow(cl.now)
	return g, cl
}

func TestGate_FirstPublishAlwaysAllowed(t *testing.T) {
	g, _ := newGate()
	if !g.Allow("tx/swr", flexradio.GroupTX, "SWR", 1.5) {
		t.Error("first publish should be allowed")
	}
}

func TestGate_DuplicateWithinDeadbandSuppressed(t *testing.T) {
	g, cl := newGate()
	g.Allow("tx/swr", flexradio.GroupTX, "SWR", 1.500)
	cl.add(1 * time.Second) // past interval
	// 1.501 rounds to 1.50 at 0.01 deadband -> duplicate
	if g.Allow("tx/swr", flexradio.GroupTX, "SWR", 1.501) {
		t.Error("value within deadband should be suppressed")
	}
}

func TestGate_ChangedValueAfterIntervalAllowed(t *testing.T) {
	g, cl := newGate()
	g.Allow("tx/swr", flexradio.GroupTX, "SWR", 1.50)
	cl.add(600 * time.Millisecond) // past 500ms interval
	if !g.Allow("tx/swr", flexradio.GroupTX, "SWR", 2.00) {
		t.Error("changed value after interval should be allowed")
	}
}

func TestGate_ChangedValueBeforeIntervalSuppressed(t *testing.T) {
	g, cl := newGate()
	g.Allow("tx/swr", flexradio.GroupTX, "SWR", 1.50)
	cl.add(100 * time.Millisecond) // before 500ms interval
	if g.Allow("tx/swr", flexradio.GroupTX, "SWR", 3.00) {
		t.Error("changed value before interval should be suppressed")
	}
	// Now advance past interval; a packet that still differs should pass.
	cl.add(500 * time.Millisecond)
	if !g.Allow("tx/swr", flexradio.GroupTX, "SWR", 3.00) {
		t.Error("after interval, changed value should be allowed")
	}
}

func TestGate_PerKeyIndependence(t *testing.T) {
	g, cl := newGate()
	g.Allow("tx/swr", flexradio.GroupTX, "SWR", 1.50)
	// Different key, same instant: allowed.
	if !g.Allow("tx/fwd", flexradio.GroupTX, "W", 100.0) {
		t.Error("different key should be independent")
	}
	_ = cl
}

func TestGate_ResetForcesRepublish(t *testing.T) {
	g, cl := newGate()
	g.Allow("tx/swr", flexradio.GroupTX, "SWR", 1.50)
	cl.add(1 * time.Millisecond) // short interval, same value would be dup
	g.Reset()
	if !g.Allow("tx/swr", flexradio.GroupTX, "SWR", 1.50) {
		t.Error("after Reset, same value should be republished")
	}
}

func TestGate_HWSlowInterval(t *testing.T) {
	g, cl := newGate()
	g.Allow("hw/temp", flexradio.GroupHW, "degC", 45.0)
	// 5 seconds later, different temp: still suppressed (10s interval)
	cl.add(5 * time.Second)
	if g.Allow("hw/temp", flexradio.GroupHW, "degC", 46.0) {
		t.Error("HW change before 10s should be suppressed")
	}
	cl.add(6 * time.Second) // total 11s
	if !g.Allow("hw/temp", flexradio.GroupHW, "degC", 46.0) {
		t.Error("HW change after 10s should be allowed")
	}
}

func TestRoundTo(t *testing.T) {
	cases := []struct {
		v, step, want float64
	}{
		{1.234, 0.1, 1.2},
		{1.266, 0.1, 1.3},
		{1.234, 0.01, 1.23},
		{1.235, 0.01, 1.24}, // half rounds away from zero
		{0, 0.1, 0},
	}
	for _, c := range cases {
		got := roundTo(c.v, c.step)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("roundTo(%v,%v) = %v, want %v", c.v, c.step, got, c.want)
		}
	}
}
