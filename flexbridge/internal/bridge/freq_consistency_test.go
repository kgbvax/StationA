package bridge

import (
	"testing"

	"flexbridge/internal/flexradio"
)

// These tests guard the frequency/band-consistency fixes: deterministic
// multi-slice selection (A2), slice-removal deletion (A1), per-slice band
// hysteresis (B), and malformed-RF_frequency partial-frame handling (C).
//
// The reported live symptom was freq_hz/band jumping between values across
// reads, especially with more than one slice.

func frameFor(t *testing.T, line string) flexradio.Frame {
	t.Helper()
	f, err := flexradio.ParseFrame(line)
	if err != nil {
		t.Fatalf("ParseFrame(%q): %v", line, err)
	}
	return f
}

// TestResolveActiveSlice_DeterministicTwoTX: with two TX slices, selection must
// always be the lowest index, never a per-call coin flip from Go's randomized
// map iteration. Under the old "return first match while ranging" code, this
// test fails within a few hundred iterations.
func TestResolveActiveSlice_DeterministicTwoTX(t *testing.T) {
	slices := map[int]flexradio.SliceStatus{
		0: {Index: 0, TX: true, FreqHz: 14_100_000},
		2: {Index: 2, TX: true, FreqHz: 7_100_000},
	}
	for i := 0; i < 500; i++ {
		got, ok := resolveActiveSlice(slices)
		if !ok || got.Index != 0 {
			t.Fatalf("iter %d: got idx=%d ok=%v, want idx=0", i, got.Index, ok)
		}
	}
}

// TestResolveActiveSlice_DeterministicTwoActive: the RX case — two panadapters
// both active=1, no TX slice. This is a normal single-operator FLEX-8400
// configuration and was the most direct cause of the reported flip-flop.
func TestResolveActiveSlice_DeterministicTwoActive(t *testing.T) {
	slices := map[int]flexradio.SliceStatus{
		0: {Index: 0, Active: true, FreqHz: 14_100_000},
		1: {Index: 1, Active: true, FreqHz: 7_100_000},
	}
	for i := 0; i < 500; i++ {
		got, ok := resolveActiveSlice(slices)
		if !ok || got.Index != 0 {
			t.Fatalf("iter %d: got idx=%d ok=%v, want idx=0", i, got.Index, ok)
		}
	}
}

// TestResolveActiveSlice_PrefersTXOverActive: a TX slice wins over an active one.
func TestResolveActiveSlice_PrefersTXOverActive(t *testing.T) {
	slices := map[int]flexradio.SliceStatus{
		0: {Index: 0, Active: true, FreqHz: 14_100_000},
		1: {Index: 1, TX: true, FreqHz: 7_100_000},
	}
	got, ok := resolveActiveSlice(slices)
	if !ok || !got.TX || got.Index != 1 {
		t.Fatalf("got idx=%d tx=%v, want idx=1 tx=true", got.Index, got.TX)
	}
}

// TestBridge_SliceRemovalDeletesPhantom: a removed slice must be deleted from
// b.slices, not left as a phantom with TX=true/Active=true that
// resolveActiveSlice would keep selecting (and flip-flop against a new slice).
// Covers both removal encodings: bare "removed" token and in_use=0.
func TestBridge_SliceRemovalDeletesPhantom(t *testing.T) {
	for _, enc := range []string{
		"S0|slice 0 0 removed",   // bare token encoding
		"S0|slice 0 0 in_use=0",  // in_use=0 encoding
		"S0|slice 0 0 removed=1", // explicit flag encoding
	} {
		t.Run(enc, func(t *testing.T) {
			b, pub := newTestBridge(t)
			// slice 0 is the TX slice on 20m
			b.HandleStatus(frameFor(t, "S0|slice 0 0 RF_frequency=14.100000 mode=USB active=1 tx=1"))
			// it is removed
			b.HandleStatus(frameFor(t, enc))

			b.mu.RLock()
			_, exists := b.slices[0]
			b.mu.RUnlock()
			if exists {
				t.Fatalf("removed slice 0 still in b.slices")
			}

			// open a new TX slice on 40m; state must follow it, not a frozen 20m phantom
			pub.Reset()
			b.HandleStatus(frameFor(t, "S0|slice 1 0 RF_frequency=7.100000 mode=LSB active=1 tx=1"))
			snap, ok := lastState(pub.Messages(), testStateTopic)
			if !ok {
				t.Fatalf("expected a publish for the new TX slice")
			}
			if snap.FreqHz != 7_100_000 || snap.Band != "40m" {
				t.Errorf("after removal: freq_hz=%d band=%q, want 7100000/40m", snap.FreqHz, snap.Band)
			}
		})
	}
}

// TestBridge_PerSliceHysteresisSurvivesSliceSwitch: the band held by hysteresis
// for an edge-dwelling slice must survive a round-trip through a different
// slice. With the old global-prev design, switching away and back clobbered the
// held band to "gen"; with per-slice prev it stays "20m".
func TestBridge_PerSliceHysteresisSurvivesSliceSwitch(t *testing.T) {
	b, pub := newTestBridge(t)

	// Seed slice 0 deep in 20m, then tune it just above the 20m edge so the
	// 2 kHz hysteresis holds it at "20m".
	b.HandleStatus(frameFor(t, "S0|slice 0 0 RF_frequency=14.300000 mode=USB active=1 tx=1"))
	pub.Reset()
	b.HandleStatus(frameFor(t, "S0|slice 0 0 RF_frequency=14.350500 mode=USB active=1 tx=1"))
	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok || snap.Band != "20m" {
		t.Fatalf("edge-dwelling slice: band=%q want 20m (hysteresis)", snap.Band)
	}

	// Switch TX to slice 1 on 40m.
	pub.Reset()
	b.HandleStatus(frameFor(t, "S0|slice 0 0 tx=0"))
	b.HandleStatus(frameFor(t, "S0|slice 1 0 RF_frequency=7.100000 mode=LSB active=1 tx=1"))
	if snap, ok := lastState(pub.Messages(), testStateTopic); !ok || snap.Band != "40m" {
		t.Fatalf("after TX switch: band=%q want 40m", snap.Band)
	}

	// Switch back to slice 0 (still at 14.350500). The held 20m must survive —
	// the old global-prev code published "gen" here.
	pub.Reset()
	b.HandleStatus(frameFor(t, "S0|slice 1 0 tx=0"))
	snap, ok = lastState(pub.Messages(), testStateTopic)
	if !ok || snap.Band != "20m" {
		t.Fatalf("after switch-back: band=%q want 20m (per-slice hysteresis held)", snap.Band)
	}
	if snap.FreqHz != 14_350_500 {
		t.Errorf("after switch-back: freq_hz=%d want 14350500", snap.FreqHz)
	}
}

// TestBridge_MalformedFreqKeepsOtherFields: a frame with a bad RF_frequency
// must not be dropped entirely — the freq is retained and the mode/active
// changes in the same incremental update are still applied.
func TestBridge_MalformedFreqKeepsOtherFields(t *testing.T) {
	b, pub := newTestBridge(t)
	// baseline: 20m, CW
	b.HandleStatus(frameFor(t, "S0|slice 0 0 RF_frequency=14.100000 mode=CW active=1"))

	// Malformed (empty) RF_frequency plus a mode change in the same frame.
	pub.Reset()
	b.HandleStatus(frameFor(t, "S0|slice 0 0 RF_frequency= mode=USB active=1"))
	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("expected a publish (mode changed despite bad RF_frequency)")
	}
	if snap.FreqHz != 14_100_000 {
		t.Errorf("FreqHz=%d, want retained 14100000 (bad freq skipped)", snap.FreqHz)
	}
	if snap.Mode != "usb" {
		t.Errorf("Mode=%q want usb (mode change must still apply)", snap.Mode)
	}
}

// TestBridge_MultiSliceStaysOnLowestActive: two active RX slices, no TX — the
// published state must stay on the lowest-index active slice and not flip to
// the other on every frame. (Integration-level companion to the direct
// resolveActiveSlice tests above.)
func TestBridge_MultiSliceStaysOnLowestActive(t *testing.T) {
	b, pub := newTestBridge(t)
	b.HandleStatus(frameFor(t, "S0|slice 0 0 RF_frequency=14.100000 mode=USB active=1"))
	b.HandleStatus(frameFor(t, "S0|slice 1 0 RF_frequency=7.100000 mode=LSB active=1"))

	// Re-deliver slice 1 many times; state must never flip to 40m.
	for i := 0; i < 50; i++ {
		pub.Reset()
		b.HandleStatus(frameFor(t, "S0|slice 1 0 RF_frequency=7.100000 mode=LSB active=1"))
		if msgs := pub.Messages(); len(msgs) != 0 {
			snap, _ := lastState(msgs, testStateTopic)
			t.Errorf("iter %d: unexpected publish flipping to freq_hz=%d band=%q (should stay on slice 0)", i, snap.FreqHz, snap.Band)
		}
	}

	// Sanity: the bridge still resolves to slice 0 (20m).
	b.mu.RLock()
	active, ok := resolveActiveSlice(b.slices)
	b.mu.RUnlock()
	if !ok || active.Index != 0 {
		t.Fatalf("active slice idx=%d ok=%v, want 0", active.Index, ok)
	}
}
