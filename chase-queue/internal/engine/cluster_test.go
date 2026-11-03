package engine

import (
	"testing"
	"time"

	"chase-queue/internal/bus"
	"chase-queue/internal/store"
)

func TestClusterSpotsMergeIntoRanking(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 230, Band: "20m"}}) // DL confirmed on 20m
	spot := func(call string, at time.Time) {
		f.e.AddClusterSpot(ClusterSpot{Call: call, Band: "20m", Spotter: "SP5ABC", Comment: "cq", FreqHz: 14025000, At: at})
	}
	spot("EA8XYZ", f.now.Add(-5*time.Minute)) // needed: listed as a cluster row
	spot("DL2BBB", f.now)                     // not needed: not listed
	spot("G4AAA", f.now.Add(-time.Minute))    // needed, unranked
	// EA8XYZ is also heard: its heard row leaves, the cluster row stands for it
	st := f.rank(1, []bus.Candidate{cand(1, "EA8XYZ", "20m", true), cand(2, "DL3CCC", "20m", true)})
	if len(st.Ranked) != 3 {
		t.Fatalf("%+v", st.Ranked)
	}
	ea8, g4, dl3 := st.Ranked[0], st.Ranked[1], st.Ranked[2]
	if ea8.Call != "EA8XYZ" || ea8.Source != bus.SourceCluster || ea8.RowID >= 0 || ea8.Spot == nil || ea8.Spot.FreqHz != 14025000 || ea8.Tag != bus.TagNeed {
		t.Fatalf("%+v", ea8)
	}
	if g4.Call != "G4AAA" || g4.Source != bus.SourceCluster || dl3.RowID != 2 || dl3.Source != bus.SourceBmrec {
		t.Fatalf("%+v %+v", g4, dl3)
	}
	// self-contained rows: frequency and derived mode on both kinds
	if ea8.FreqHz != 14025000 || ea8.Mode != "cw" || dl3.FreqHz != 14.2e6+2000 || dl3.Mode != "usb" {
		t.Fatalf("%+v %+v", ea8, dl3)
	}
	// row ids are stable while the spot lasts
	spot("EA8XYZ", f.now)
	if again := f.e.Compute(); again.Ranked[0].RowID != ea8.RowID {
		t.Fatalf("%+v", again.Ranked[0])
	}
	// Skip works on a cluster row
	if err := f.e.Skip(bus.Target{RowID: ea8.RowID, Band: "20m"}); err != nil {
		t.Fatal(err)
	}
	if st := f.e.Compute(); st.Ranked[0].Call != "G4AAA" {
		t.Fatalf("%+v", st.Ranked)
	}
	// spots age out after the horizon; the heard row comes back
	f.now = f.now.Add(31 * time.Minute)
	if st := f.e.Compute(); len(st.Ranked) != 1 || st.Ranked[0].RowID != 2 {
		t.Fatalf("%+v", st.Ranked)
	}
}

func TestClusterRowsCapped(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	for i := range MaxClusterRows + 10 {
		f.e.AddClusterSpot(ClusterSpot{Call: "EA8A" + string(rune('A'+i%26)) + string(rune('A'+i/26)), Band: "20m", At: f.now})
	}
	if st := f.e.Compute(); len(st.Ranked) != MaxClusterRows {
		t.Fatalf("%d rows", len(st.Ranked))
	}
}

func TestClusterRowModeFromCommentThenBandPlan(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.AddClusterSpot(ClusterSpot{Call: "EA8AAA", Band: "20m", Comment: "FT8 -10", FreqHz: 14_250_000, At: f.now})
	f.e.AddClusterSpot(ClusterSpot{Call: "EA8BBB", Band: "20m", Comment: "up 2", FreqHz: 14_250_000, At: f.now})
	f.e.AddClusterSpot(ClusterSpot{Call: "EA8CCC", Band: "20m", Comment: "", FreqHz: 14_105_000, At: f.now})
	modes := map[string]string{}
	for _, r := range f.rank(1, nil).Ranked {
		modes[r.Call] = r.Mode
	}
	if modes["EA8AAA"] != "data" || modes["EA8BBB"] != "usb" || modes["EA8CCC"] != "" {
		t.Fatalf("%v", modes)
	}
}

func TestWorkedClusterStationNotListed(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.AddDXWorked("EA8XYZ", "20m")
	f.e.AddClusterSpot(ClusterSpot{Call: "EA8XYZ", Band: "20m", FreqHz: 14_025_000, At: f.now})
	if st := f.rank(1, nil); len(st.Ranked) != 0 {
		t.Fatalf("%+v", st.Ranked)
	}
}
