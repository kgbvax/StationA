package model

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"chaseq-ui/internal/bus"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newModel() (*Model, *clock) {
	c := &clock{t: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)}
	return New(c.now), c
}

func rk(id int64, call, band string, needed bool) bus.Ranked {
	return bus.Ranked{RowID: id, Call: call, Band: band, Needed: needed}
}

func chase(r ...bus.Ranked) bus.ChaseState { return bus.ChaseState{Activity: "dx", Ranked: r} }

func order(v View) []int64 {
	var o []int64
	for _, r := range v.Rows {
		o = append(o, r.RowID)
	}
	return o
}

func eq(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// AE5: the highlight stays on its station while the list re-ranks, and moves
// to the first row when that station leaves.
func TestHighlightFollowsRowThenFallsBack(t *testing.T) {
	m, _ := newModel()
	m.SetChase(chase(rk(1, "A1", "20m", true), rk(2, "B2", "20m", true), rk(7, "EA8XYZ", "20m", true)))
	m.Select(7)
	m.SetChase(chase(rk(9, "NEW", "20m", true), rk(1, "A1", "20m", true), rk(2, "B2", "20m", true), rk(7, "EA8XYZ", "20m", true)))
	v := m.Snapshot()
	if v.Highlight != 7 || !eq(order(v), []int64{9, 1, 2, 7}) {
		t.Fatalf("highlight=%d order=%v, want 7 on row 4", v.Highlight, order(v))
	}
	m.SetChase(chase(rk(9, "NEW", "20m", true), rk(1, "A1", "20m", true)))
	if v := m.Snapshot(); v.Highlight != 9 {
		t.Errorf("highlight=%d, want 9 (first row)", v.Highlight)
	}
}

// AE1: arrow keys move within the list and clamp.
func TestMoveClamps(t *testing.T) {
	m, _ := newModel()
	m.SetChase(chase(rk(1, "A", "20m", true), rk(2, "B", "20m", true), rk(3, "C", "20m", true), rk(4, "D", "20m", true)))
	m.Snapshot()
	for i := 0; i < 3; i++ {
		m.Move(1)
	}
	if v := m.Snapshot(); v.Highlight != 4 {
		t.Fatalf("after 3 downs highlight=%d, want 4", v.Highlight)
	}
	m.Move(1)
	if v := m.Snapshot(); v.Highlight != 4 {
		t.Errorf("past the end highlight=%d, want 4", v.Highlight)
	}
	m.Move(-9)
	if v := m.Snapshot(); v.Highlight != 1 {
		t.Errorf("past the top highlight=%d, want 1", v.Highlight)
	}
}

// AE7: band + needed-only filters, chase-queue order kept.
func TestFilters(t *testing.T) {
	m, _ := newModel()
	m.SetChase(chase(rk(1, "A", "20m", true), rk(2, "B", "40m", true), rk(3, "C", "20m", false), rk(4, "D", "20m", true)))
	m.SetFilters(Filters{Bands: []string{"20m"}, NeededOnly: true})
	v := m.Snapshot()
	if !eq(order(v), []int64{1, 4}) || v.Hidden != 2 {
		t.Errorf("order=%v hidden=%d, want [1 4] hidden 2", order(v), v.Hidden)
	}
}

func TestFilteredToEmptyReportsHidden(t *testing.T) {
	m, _ := newModel()
	m.SetChase(chase(rk(1, "A", "40m", true), rk(2, "B", "40m", true)))
	m.SetFilters(Filters{Bands: []string{"20m"}})
	v := m.Snapshot()
	if len(v.Rows) != 0 || v.Hidden != 2 || v.Highlight != 0 {
		t.Errorf("rows=%d hidden=%d highlight=%d", len(v.Rows), v.Hidden, v.Highlight)
	}
}

func TestRowFrequencyModeAndWorked(t *testing.T) {
	m, _ := newModel()
	heard := rk(7, "EA8XYZ", "20m", true)
	heard.FreqHz, heard.Mode, heard.Source = 14231500, "usb", bus.SourceBmrec
	heard.WorkedBefore = &bus.WorkedBefore{QSOs: 2, Bands: []string{"20m", "40m", "15m"}, ThisBand: false}
	old := rk(8, "NOFREQ", "20m", true) // older chase-queue: no freq_hz
	cl := bus.Ranked{RowID: -3, Call: "DL1ABC", Band: "20m", Needed: true, Source: bus.SourceCluster, Spot: &bus.ClusterSpot{FreqHz: 14025000}}
	dupe := rk(9, "W1AW", "20m", false)
	dupe.WorkedBefore = &bus.WorkedBefore{QSOs: 1, Bands: []string{"20m"}, ThisBand: true}
	m.SetChase(chase(heard, old, cl, dupe))
	v := m.Snapshot()
	r := v.Rows[0]
	if r.FreqHz != 14231500 || !r.Tunable() || r.Mode != "usb" || !r.Worked || r.WorkedThisBand ||
		len(r.WorkedOther) != 2 || r.WorkedOther[0] != "40m" || r.WorkedOther[1] != "15m" {
		t.Errorf("heard row = %+v", r)
	}
	if v.Rows[1].Tunable() || v.Rows[1].Worked {
		t.Errorf("row without freq_hz must not be tunable: %+v", v.Rows[1])
	}
	if v.Rows[2].FreqHz != 14025000 || !v.Rows[2].Cluster {
		t.Errorf("cluster row freq = %d", v.Rows[2].FreqHz)
	}
	if d := v.Rows[3]; !d.Worked || !d.WorkedThisBand || len(d.WorkedOther) != 0 {
		t.Errorf("dupe row = %+v", d)
	}
}

// AE6: offline / stale disable actions; a fresh state re-enables them.
func TestOfflineAndStale(t *testing.T) {
	m, c := newModel()
	if v := m.Snapshot(); v.ActionsEnabled || v.Offline == "" {
		t.Errorf("before any state: enabled=%v offline=%q", v.ActionsEnabled, v.Offline)
	}
	m.SetChase(chase(rk(1, "A", "20m", true)))
	if v := m.Snapshot(); !v.ActionsEnabled {
		t.Errorf("fresh state should enable actions")
	}
	m.SetStatus(false)
	if v := m.Snapshot(); v.ActionsEnabled || v.Offline != "chase-queue is offline" {
		t.Errorf("offline: enabled=%v line=%q", v.ActionsEnabled, v.Offline)
	}
	m.SetStatus(true)
	c.t = c.t.Add(76 * time.Second)
	if v := m.Snapshot(); v.ActionsEnabled {
		t.Errorf("76 s silence should disable actions")
	}
	m.SetChase(chase(rk(1, "A", "20m", true)))
	if v := m.Snapshot(); !v.ActionsEnabled {
		t.Errorf("a fresh state should re-enable actions")
	}
}

func TestEmptyRankedKeepsProblems(t *testing.T) {
	m, _ := newModel()
	m.SetChase(bus.ChaseState{Activity: "dx", Problems: []string{"Club Log confirmations not loaded yet"}})
	v := m.Snapshot()
	if len(v.Rows) != 0 || len(v.Problems) != 1 {
		t.Errorf("rows=%d problems=%v", len(v.Rows), v.Problems)
	}
}

func TestFiltersRoundTripAndCorrupt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "filters.json")
	f := Filters{NeededOnly: true}.ToggleBand("40m").ToggleBand("20m")
	if err := SaveFilters(p, f); err != nil {
		t.Fatal(err)
	}
	got := LoadFilters(p)
	if !got.NeededOnly || len(got.Bands) != 2 || got.Bands[0] != "20m" {
		t.Errorf("round trip = %+v", got)
	}
	if got := got.ToggleBand("20m"); len(got.Bands) != 1 {
		t.Errorf("toggle off = %+v", got)
	}
	if g := LoadFilters(filepath.Join(t.TempDir(), "missing.json")); g.Active() {
		t.Errorf("missing file = %+v", g)
	}
}

func TestCorruptFiltersFileMeansNoFilter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "filters.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f := LoadFilters(p); f.Active() {
		t.Errorf("corrupt file gave %+v", f)
	}
}
