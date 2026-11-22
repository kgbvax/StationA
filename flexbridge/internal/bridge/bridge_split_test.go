package bridge

import (
	"reflect"
	"testing"

	"flexbridge/internal/flexradio"
)

// Split detection: /state keeps following the TX slice; slice A (what CAT
// calls VFO A) is published as the RX side while a different slice holds the
// TX flag, and cross_band flags the two on different ham bands — the
// "worked split on 20 m, clicked a 17 m spot, VFO B still on 20 m" case.

func handleLine(t *testing.T, b *Bridge, line string) {
	t.Helper()
	f, err := flexradio.ParseFrame(line)
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	b.HandleStatus(f)
}

// splitOn20m puts slice A (idx 0) on 14.100 RX-only and slice B (idx 1) on
// 14.105 with the TX flag — SmartSDR's split, same band.
func splitOn20m(t *testing.T, b *Bridge) {
	t.Helper()
	handleLine(t, b, "S0|slice 0 0 RF_frequency=14.100000 mode=USB active=1 tx=1 index_letter=A")
	handleLine(t, b, "S0|slice 0 0 tx=0")
	handleLine(t, b, "S0|slice 1 0 RF_frequency=14.105000 mode=USB active=0 tx=1 index_letter=B")
}

func TestBridge_SplitSameBand(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()
	splitOn20m(t, b)

	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published")
	}
	if snap.FreqHz != 14_105_000 || snap.Band != "20m" {
		t.Errorf("/state follows the TX slice: freq=%d band=%q, want 14105000/20m", snap.FreqHz, snap.Band)
	}
	if !snap.Split {
		t.Error("split = false, want true (TX on slice B)")
	}
	if snap.RxFreqHz != 14_100_000 || snap.RxBand != "20m" {
		t.Errorf("rx = %d/%q, want 14100000/20m (slice A)", snap.RxFreqHz, snap.RxBand)
	}
	if snap.CrossBand {
		t.Error("cross_band = true on a same-band split")
	}
}

func TestBridge_SplitCrossBandWhenSliceARetunes(t *testing.T) {
	b, pub := newTestBridge(t)
	splitOn20m(t, b)
	pub.Reset()

	// The logger clicks a 17 m spot: VFO A = slice A retunes, TX stays on B.
	handleLine(t, b, "S0|slice 0 0 RF_frequency=18.100000")

	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published after slice A retune")
	}
	if snap.Band != "20m" || snap.FreqHz != 14_105_000 {
		t.Errorf("TX side moved: freq=%d band=%q, want 14105000/20m", snap.FreqHz, snap.Band)
	}
	if !snap.Split || snap.RxBand != "17m" || snap.RxFreqHz != 18_100_000 {
		t.Errorf("rx side = split=%v %d/%q, want split 18100000/17m", snap.Split, snap.RxFreqHz, snap.RxBand)
	}
	if !snap.CrossBand {
		t.Error("cross_band = false, want true (RX 17m, TX 20m)")
	}

	// Same frame again: nothing changed, nothing published.
	pub.Reset()
	handleLine(t, b, "S0|slice 0 0 RF_frequency=18.100000")
	if n := len(pub.Messages()); n != 0 {
		t.Errorf("unchanged split state republished %d times, want 0", n)
	}
}

func TestBridge_SplitOffClearsRxFields(t *testing.T) {
	b, pub := newTestBridge(t)
	splitOn20m(t, b)
	handleLine(t, b, "S0|slice 0 0 RF_frequency=18.100000")
	pub.Reset()

	// TX moves back to slice A (split off in SmartSDR).
	handleLine(t, b, "S0|slice 1 0 tx=0")
	handleLine(t, b, "S0|slice 0 0 tx=1")

	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published after split off")
	}
	if snap.Band != "17m" || snap.FreqHz != 18_100_000 {
		t.Errorf("/state = %d/%q, want 18100000/17m (slice A is the TX slice again)", snap.FreqHz, snap.Band)
	}
	if snap.Split || snap.CrossBand {
		t.Errorf("split=%v cross_band=%v, want false/false", snap.Split, snap.CrossBand)
	}
	if snap.RxFreqHz != 0 || snap.RxBand != "" {
		t.Errorf("rx fields = %d/%q, want omitted", snap.RxFreqHz, snap.RxBand)
	}
}

func TestBridge_SplitSliceRemovedClears(t *testing.T) {
	b, pub := newTestBridge(t)
	splitOn20m(t, b)
	handleLine(t, b, "S0|slice 0 0 RF_frequency=18.100000")
	pub.Reset()

	// Slice B torn down: slice A (active, no TX flag anywhere) drives /state.
	handleLine(t, b, "S0|slice 1 0 removed")

	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published after slice removal")
	}
	if snap.Band != "17m" || snap.Split || snap.CrossBand {
		t.Errorf("band=%q split=%v cross_band=%v, want 17m/false/false", snap.Band, snap.Split, snap.CrossBand)
	}
}

func TestBridge_SplitWithoutIndexLetterUsesLowestIndex(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()
	handleLine(t, b, "S0|slice 0 0 RF_frequency=14.100000 mode=USB active=1 tx=0")
	handleLine(t, b, "S0|slice 1 0 RF_frequency=18.100000 mode=USB active=0 tx=1")

	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published")
	}
	if !snap.Split || snap.RxBand != "20m" || snap.Band != "17m" || !snap.CrossBand {
		t.Errorf("split=%v rx=%q tx=%q cross=%v, want true/20m/17m/true", snap.Split, snap.RxBand, snap.Band, snap.CrossBand)
	}
}

func TestBridge_SplitLetterBeatsIndex(t *testing.T) {
	// Slice indices and letters need not agree (slice A can be re-created
	// at a higher index); the letter wins.
	b, pub := newTestBridge(t)
	pub.Reset()
	handleLine(t, b, "S0|slice 0 0 RF_frequency=18.100000 mode=USB active=0 tx=1 index_letter=B")
	handleLine(t, b, "S0|slice 1 0 RF_frequency=14.100000 mode=USB active=1 tx=0 index_letter=A")

	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published")
	}
	if !snap.Split || snap.RxBand != "20m" || snap.Band != "17m" || !snap.CrossBand {
		t.Errorf("split=%v rx=%q tx=%q cross=%v, want true/20m/17m/true", snap.Split, snap.RxBand, snap.Band, snap.CrossBand)
	}
}

func TestBridge_SplitNoInfoBandIsNotCrossBand(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()
	// Slice A parked on a broadcast frequency (gen): no mismatch claim.
	handleLine(t, b, "S0|slice 0 0 RF_frequency=9.900000 mode=AM active=1 tx=0 index_letter=A")
	handleLine(t, b, "S0|slice 1 0 RF_frequency=14.105000 mode=USB active=0 tx=1 index_letter=B")

	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published")
	}
	if !snap.Split || snap.RxBand != "gen" || snap.CrossBand {
		t.Errorf("split=%v rx=%q cross=%v, want true/gen/false", snap.Split, snap.RxBand, snap.CrossBand)
	}
}

func TestBridge_NoSplitWithoutTxFlag(t *testing.T) {
	// Two RX slices, no TX flag: /state is the active slice and there is no
	// second VFO to compare against.
	b, pub := newTestBridge(t)
	pub.Reset()
	handleLine(t, b, "S0|slice 0 0 RF_frequency=14.100000 mode=USB active=0 tx=0 index_letter=A")
	handleLine(t, b, "S0|slice 1 0 RF_frequency=18.100000 mode=USB active=1 tx=0 index_letter=B")

	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published")
	}
	if snap.Band != "17m" || snap.Split || snap.CrossBand || snap.RxBand != "" {
		t.Errorf("band=%q split=%v cross=%v rx=%q, want 17m/false/false/\"\"", snap.Band, snap.Split, snap.CrossBand, snap.RxBand)
	}
}

// Band change while split: slice A moves to the new band, takes the TX flag,
// and the split slice is removed (collapseSplit), unless the configuration
// allows a cross-band split.

func newSplitBridge(t *testing.T, allowCross bool) (*Bridge, *MemoPublisher, *fakeCommander) {
	t.Helper()
	b, pub := newTestBridge(t)
	b.cfg.AllowCrossBandSplit = allowCross
	fc := &fakeCommander{}
	b.SetCommander(fc)
	return b, pub, fc
}

func TestBridge_SplitBandChangeOnSliceACollapses(t *testing.T) {
	b, pub, fc := newSplitBridge(t, false)
	splitOn20m(t, b)
	if got := fc.sliceCmds(); len(got) != 0 {
		t.Fatalf("same-band split sent %v, want nothing", got)
	}

	// The logger clicks a 17 m spot: slice A retunes, slice B stays on 20 m.
	handleLine(t, b, "S0|slice 0 0 RF_frequency=18.100000")
	want := []string{"tx 0", "remove 1"}
	if got := fc.sliceCmds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("collapse = %v, want %v", got, want)
	}

	// The radio accepts both: the bridge's model follows (no status echo).
	pub.Reset()
	b.HandleReply(flexradio.Reply{Command: "slice set 0 tx=1"})
	b.HandleReply(flexradio.Reply{Command: "slice remove 1"})
	snap, ok := lastState(pub.Messages(), testStateTopic)
	if !ok {
		t.Fatal("no state published after the collapse")
	}
	if snap.FreqHz != 18_100_000 || snap.Band != "17m" || snap.Split || snap.CrossBand {
		t.Errorf("/state = %d/%q split=%v cross=%v, want 18100000/17m/false/false",
			snap.FreqHz, snap.Band, snap.Split, snap.CrossBand)
	}
	if got := fc.sliceCmds(); len(got) != 2 {
		t.Errorf("collapse repeated: %v", got)
	}
}

func TestBridge_SplitBandChangeViaSetFreqCollapses(t *testing.T) {
	b, _, fc := newSplitBridge(t, false)
	splitOn20m(t, b)

	b.HandleCommand(cmdJSON("set_freq", "18100000"))
	if got := fc.tunes(); len(got) != 1 || got[0].Slice != 0 {
		t.Fatalf("tunes = %v, want slice 0", got)
	}
	// The accepted tune is applied to the model; slice A left 20 m.
	b.HandleReply(flexradio.Reply{Command: "slice tune 0 18.100000 autopan=1"})
	if got, want := fc.sliceCmds(), []string{"tx 0", "remove 1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("collapse = %v, want %v", got, want)
	}
}

func TestBridge_SplitSameBandRetuneKeepsSplit(t *testing.T) {
	b, _, fc := newSplitBridge(t, false)
	splitOn20m(t, b)
	handleLine(t, b, "S0|slice 0 0 RF_frequency=14.200000")
	if got := fc.sliceCmds(); len(got) != 0 {
		t.Errorf("in-band retune sent %v, want nothing", got)
	}
}

func TestBridge_SplitTxSliceBandChangeKeepsSplit(t *testing.T) {
	// The operator tunes the TX slice to another band (60 m cross-band DX
	// style): a deliberate cross-band split, warn only.
	b, pub, fc := newSplitBridge(t, false)
	splitOn20m(t, b)
	pub.Reset()
	handleLine(t, b, "S0|slice 1 0 RF_frequency=7.150000")
	if got := fc.sliceCmds(); len(got) != 0 {
		t.Errorf("TX-slice band change sent %v, want nothing", got)
	}
	if snap, ok := lastState(pub.Messages(), testStateTopic); !ok || !snap.CrossBand {
		t.Errorf("cross_band not published: %+v", snap)
	}
}

func TestBridge_SplitAllowCrossBandKeepsSplit(t *testing.T) {
	b, _, fc := newSplitBridge(t, true)
	splitOn20m(t, b)
	handleLine(t, b, "S0|slice 0 0 RF_frequency=18.100000")
	if got := fc.sliceCmds(); len(got) != 0 {
		t.Errorf("allow_cross_band sent %v, want nothing", got)
	}
}

func TestHandleCommand_SetBandWhileSplitCollapsesFirst(t *testing.T) {
	b, _, fc := newSplitBridge(t, false)
	handleLine(t, b, "S0|display pan 0x40000000 band=20 center=14.175 in_use=1")
	handleLine(t, b, "S0|display pan 0x40000001 band=20 center=14.175 in_use=1")
	handleLine(t, b, "S0|slice 0 0 RF_frequency=14.100000 mode=USB active=1 tx=0 index_letter=A pan=0x40000001")
	handleLine(t, b, "S0|slice 1 0 RF_frequency=14.105000 mode=USB active=0 tx=1 index_letter=B pan=0x40000000")

	b.HandleCommand(cmdJSON("set_band", "17m"))
	if got, want := fc.sliceCmds(), []string{"tx 0", "remove 1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("collapse = %v, want %v", got, want)
	}
	// The band change targets slice A's panadapter, not the TX slice's.
	if got := fc.bands(); len(got) != 1 || got[0].Handle != "0x40000001" || got[0].Band != 17 {
		t.Errorf("set_band = %v, want [{0x40000001 17}]", got)
	}
}

func TestHandleCommand_SetBandWithoutSplitSendsNoCollapse(t *testing.T) {
	b, _, fc := newSplitBridge(t, false)
	handleLine(t, b, "S0|display pan 0x40000000 band=20 center=14.175 in_use=1")
	handleLine(t, b, "S0|slice 0 0 RF_frequency=14.100000 mode=USB active=1 tx=1 index_letter=A pan=0x40000000")
	b.HandleCommand(cmdJSON("set_band", "17m"))
	if got := fc.sliceCmds(); len(got) != 0 {
		t.Errorf("no split, sent %v", got)
	}
	if got := fc.bands(); len(got) != 1 {
		t.Errorf("set_band = %v, want one call", got)
	}
}
