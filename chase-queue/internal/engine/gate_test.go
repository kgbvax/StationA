package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"chase-queue/internal/bus"
	"chase-queue/internal/store"
)

func boolp(b bool) *bool { return &b }

func gateRows() []bus.Candidate {
	trusted := cand(1, "DL1AAA", "20m", true)
	trusted.CallSource = "owner"
	qualified := cand(2, "DL2BBB", "20m", true)
	qualified.Qualified = boolp(true)
	unqualified := cand(3, "DL3CCC", "20m", true)
	unqualified.Qualified = boolp(false)
	inferred := cand(4, "DL4DDD", "20m", false) // untrusted call
	inferred.CallSource = "inferred"
	callless := cand(5, "", "20m", false)
	byConfirm := cand(6, "DL6FFF", "20m", false) // call_source says confirmed, flag not set
	byConfirm.CallSource = "confirmed"
	return []bus.Candidate{trusted, qualified, unqualified, inferred, callless, byConfirm}
}

func TestBmrecGate(t *testing.T) {
	for _, c := range []struct {
		gate string
		want []int64
		hid  int
	}{
		{"", []int64{1, 2, 3, 4, 5, 6}, 0},
		{GateOff, []int64{1, 2, 3, 4, 5, 6}, 0},
		{GateTrusted, []int64{1, 2, 3, 6}, 2},
		{GateQualified, []int64{2}, 5},
	} {
		f := newDX(t)
		f.e.SetConfirmations(nil)
		f.e.SetBmrecGate(c.gate)
		st := f.rank(1, gateRows())
		got := ids(st)
		slices.Sort(got)
		if !slices.Equal(got, c.want) || st.BmrecUnqualified != c.hid {
			t.Errorf("gate %q: rows %v hidden %d, want %v %d", c.gate, got, st.BmrecUnqualified, c.want, c.hid)
		}
	}
}

func TestQualifiedGateProblemWhenBmrecSendsNoFlag(t *testing.T) {
	f := newDX(t)
	f.e.SetBmrecGate(GateQualified)
	if p := f.e.GateProblem(); p != "" {
		t.Fatalf("no rows yet: %q", p)
	}
	f.rank(1, []bus.Candidate{cand(1, "DL1AAA", "20m", true)}) // an older bmrec
	if p := f.e.GateProblem(); !strings.Contains(p, "no qualified flag") {
		t.Fatalf("%q", p)
	}
	f.rank(2, []bus.Candidate{func() bus.Candidate { c := cand(1, "DL1AAA", "20m", true); c.Qualified = boolp(false); return c }()})
	if p := f.e.GateProblem(); p != "" {
		t.Fatalf("flag present (false): %q", p)
	}
	f.e.SetBmrecGate(GateTrusted)
	f.rank(3, []bus.Candidate{cand(1, "DL1AAA", "20m", true)})
	if p := f.e.GateProblem(); p != "" {
		t.Fatalf("other gates never complain: %q", p)
	}
}

func TestBuddyListedWhenNotNeededAndMarked(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 230, Band: "20m"}}) // DL confirmed on 20m
	f.e.SetBuddies([]Buddy{{Call: "dl2bbb", Note: "Hans", Boost: 3}, {Call: "EA8XYZ"}})
	f.e.AddClusterSpot(ClusterSpot{Call: "DL2BBB/P", Band: "20m", FreqHz: 14_250_000, Spotter: "X", Cluster: "log4om", At: f.now}) // buddy, not needed
	f.e.AddClusterSpot(ClusterSpot{Call: "DL9ZZZ", Band: "20m", FreqHz: 14_250_000, At: f.now})                                    // not needed, no buddy: not listed
	f.e.AddClusterSpot(ClusterSpot{Call: "EA8XYZ", Band: "20m", FreqHz: 14_025_000, At: f.now})                                    // needed buddy
	f.e.AddClusterSpot(ClusterSpot{Call: "G4AAA", Band: "20m", FreqHz: 14_025_000, At: f.now})                                     // needed, no buddy
	heard := cand(1, "DL3CCC", "20m", true)
	st := f.rank(1, []bus.Candidate{heard})
	var calls []string
	for _, r := range st.Ranked {
		calls = append(calls, r.Call)
	}
	// needed block: buddy EA8XYZ first although G4AAA arrived with equal priority; then the not-needed buddy; then the rest
	if strings.Join(calls, ",") != "EA8XYZ,G4AAA,DL2BBB,DL3CCC" {
		t.Fatalf("%v", calls)
	}
	ea8, dl2 := st.Ranked[0], st.Ranked[2]
	if !ea8.Buddy || !ea8.Needed || ea8.BuddyNote != "" {
		t.Fatalf("%+v", ea8)
	}
	if !dl2.Buddy || dl2.Needed || dl2.Tag != "" || dl2.BuddyNote != "Hans" || dl2.Source != bus.SourceCluster || dl2.ClusterName != "log4om" {
		t.Fatalf("%+v", dl2)
	}
	if st.Ranked[1].Buddy || st.Ranked[3].Buddy {
		t.Fatalf("%+v", st.Ranked)
	}
}

func TestBuddyDoesNotCountAgainstClusterCap(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.SetBuddies([]Buddy{{Call: "DL1BUD"}})
	for i := range MaxClusterRows + 5 {
		f.e.AddClusterSpot(ClusterSpot{Call: "EA8A" + string(rune('A'+i%26)) + string(rune('A'+i/26)), Band: "20m", At: f.now})
	}
	f.e.AddClusterSpot(ClusterSpot{Call: "DL1BUD", Band: "20m", At: f.now})
	st := f.e.Compute()
	if len(st.Ranked) != MaxClusterRows+1 {
		t.Fatalf("%d rows", len(st.Ranked))
	}
	found := false
	for _, r := range st.Ranked {
		found = found || r.Buddy
	}
	if !found {
		t.Fatal("buddy missing")
	}
}

func TestHumanSpotOutranksSkimmerOfTheSameStation(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.AddClusterSpot(ClusterSpot{Call: "EA8XYZ", Band: "20m", FreqHz: 14_025_000, Spotter: "SP5ABC", Comment: "UP 2 QSL via buero", Cluster: "log4om", At: f.now.Add(-time.Minute)})
	f.e.AddClusterSpot(ClusterSpot{Call: "EA8XYZ", Band: "20m", FreqHz: 14_026_000, Spotter: "DL1ABC-#", Comment: "CW 23 dB", Cluster: "rbn", RBN: true, At: f.now})
	f.e.AddClusterSpot(ClusterSpot{Call: "EA8XYZ", Band: "20m", FreqHz: 14_020_000, Spotter: "OK1ABC", Cluster: "log4om", At: f.now.Add(-5 * time.Minute)}) // older human: ignored
	st := f.e.Compute()
	if len(st.Ranked) != 1 {
		t.Fatalf("one row per call and band: %+v", st.Ranked)
	}
	r := st.Ranked[0]
	if r.Source != bus.SourceCluster || r.ClusterName != "log4om" || r.Spot == nil || r.Spot.RBN || r.Spot.FreqHz != 14_025_000 ||
		r.FreqHz != 14_025_000 || r.Spot.Comment != "UP 2 QSL via buero" || !r.Spot.At.Equal(f.now) {
		t.Fatalf("%+v %+v", r, r.Spot)
	}
	// a fresher human spot takes the row over
	f.e.AddClusterSpot(ClusterSpot{Call: "EA8XYZ", Band: "20m", FreqHz: 14_027_000, Spotter: "SP5ABC", Cluster: "log4om", At: f.now.Add(time.Minute)})
	if r = f.e.Compute().Ranked[0]; r.Spot.FreqHz != 14_027_000 || r.Spot.RBN {
		t.Fatalf("%+v %+v", r, r.Spot)
	}
	// a skimmer alone makes an rbn row; a human spot older than the horizon no longer shields it
	f.e.AddClusterSpot(ClusterSpot{Call: "G4AAA", Band: "20m", FreqHz: 14_030_000, Cluster: "rbn", RBN: true, At: f.now})
	f.e.AddClusterSpot(ClusterSpot{Call: "G4AAA", Band: "20m", FreqHz: 14_031_000, Cluster: "rbn", RBN: true, At: f.now.Add(time.Minute)})
	for _, r := range f.e.Compute().Ranked {
		if r.Call == "G4AAA" && (!r.Spot.RBN || r.Spot.FreqHz != 14_031_000) {
			t.Fatalf("%+v %+v", r, r.Spot)
		}
	}
}

func TestVHFBandsRank(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 230, Band: "2m"}})
	f.e.AddClusterSpot(ClusterSpot{Call: "EA8XYZ", Band: "2m", FreqHz: 144_300_000, At: f.now})
	f.e.AddClusterSpot(ClusterSpot{Call: "DL2BBB", Band: "70cm", FreqHz: 432_200_000, At: f.now})
	st := f.e.Compute()
	if len(st.Ranked) != 2 {
		t.Fatalf("%+v", st.Ranked)
	}
	for _, r := range st.Ranked {
		if r.Mode != "" {
			t.Fatalf("unknown VHF mode must stay empty: %+v", r)
		}
	}
}

func TestHeardRowStaysWhenItsClusterRowIsNotListed(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 29, Band: "20m"}}) // EA8 done: its cluster row is not listed
	f.e.AddClusterSpot(ClusterSpot{Call: "EA8XYZ", Band: "20m", FreqHz: 14_025_000, Cluster: "log4om", At: f.now})
	st := f.rank(1, []bus.Candidate{cand(1, "EA8XYZ", "20m", true)})
	if len(st.Ranked) != 1 || st.Ranked[0].Source != bus.SourceBmrec {
		t.Fatalf("the heard row must stay when the cluster row is not listed: %+v", st.Ranked)
	}
	// needed: the cluster row stands for it
	f.e.SetConfirmations(nil)
	st = f.rank(2, []bus.Candidate{cand(1, "EA8XYZ", "20m", true)})
	if len(st.Ranked) != 1 || st.Ranked[0].Source != bus.SourceCluster {
		t.Fatalf("%+v", st.Ranked)
	}
}

func TestTraceExplainsEveryRow(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.SetBmrecGate(GateQualified)
	f.rank(1, gateRows())
	tr := f.e.Trace()
	if tr.Gate != GateQualified || tr.Candidates != 6 || !tr.QualifiedFlag || tr.GateKept != 5 {
		t.Fatalf("header = %+v", tr)
	}
	byCall := map[string]RowTrace{}
	for _, r := range tr.Rows {
		byCall[r.Call+"#"+r.Outcome] = r
	}
	if r, ok := byCall["DL2BBB#listed"]; !ok || r.Source != bus.SourceBmrec {
		t.Errorf("the qualified row must be traced as listed: %+v", tr.Rows)
	}
	if r, ok := byCall["DL3CCC#gate"]; !ok || r.Detail != "not qualified in bmrec" {
		t.Errorf("an unqualified row says why: %+v", r)
	}
	if r, ok := byCall["DL1AAA#gate"]; !ok || r.Detail != "bmrec sent no qualified flag for this row" {
		t.Errorf("a row without the flag says so: %+v", r)
	}
	if r, ok := byCall["DL4DDD#gate"]; !ok || !strings.Contains(r.Detail, "not trusted") {
		t.Errorf("an untrusted call says so: %+v", r)
	}
	if len(tr.Rows) != 6 {
		t.Errorf("one trace per candidate, got %d: %+v", len(tr.Rows), tr.Rows)
	}
}

func TestTraceRemembersRowsListedEarlier(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.SetBmrecGate(GateQualified)
	f.rank(1, gateRows())
	f.rank(2, nil) // the qualified row closed: it is gone from bmrec's state
	tr := f.e.Trace()
	var found bool
	for _, v := range tr.RecentlyListed {
		found = found || (v.Call == "DL2BBB" && v.Source == bus.SourceBmrec)
	}
	if !found {
		t.Fatalf("a row listed earlier stays in recently_listed: %+v", tr.RecentlyListed)
	}
	for _, r := range tr.Rows {
		if r.Call == "DL2BBB" {
			t.Errorf("the closed row is no longer a candidate: %+v", r)
		}
	}
}

func TestBmrecRowLingersAfterBmrecClosesIt(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.SetBmrecGate(GateQualified)
	f.e.SetBmrecLinger(10 * time.Minute)
	qualified := cand(2, "DL2BBB", "20m", true)
	qualified.Qualified = boolp(true)
	unqualified := cand(3, "DL3CCC", "20m", true)
	unqualified.Qualified = boolp(false)
	if got := ids(f.rank(1, []bus.Candidate{qualified, unqualified})); !slices.Equal(got, []int64{2}) {
		t.Fatalf("before the close: %v", got)
	}
	// bmrec closes both rows: only the one that passed the gate stays, for the linger time
	f.now = f.now.Add(3 * time.Minute)
	if got := ids(f.rank(2, nil)); !slices.Equal(got, []int64{2}) {
		t.Errorf("a gate-passing row stays listed after bmrec closed it: %v", got)
	}
	if tr := f.e.Trace(); len(tr.Rows) != 1 || !strings.Contains(tr.Rows[0].Detail, "kept listed") {
		t.Errorf("the trace says it is kept: %+v", tr.Rows)
	}
	f.now = f.now.Add(8 * time.Minute) // 11 min after the last time bmrec offered it
	if got := ids(f.rank(3, nil)); len(got) != 0 {
		t.Errorf("gone after the linger time: %v", got)
	}
}

func TestLingerOffDropsAClosedRowAtOnce(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.SetBmrecGate(GateQualified)
	qualified := cand(2, "DL2BBB", "20m", true)
	qualified.Qualified = boolp(true)
	f.rank(1, []bus.Candidate{qualified})
	if got := ids(f.rank(2, nil)); len(got) != 0 {
		t.Errorf("no linger configured: %v", got)
	}
}

func TestRankedCarriesWhenTheStationWasLastHeard(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	c := cand(2, "DL2BBB", "20m", true)
	heard := f.now.Add(-4 * time.Minute)
	c.LastUTC = heard
	st := f.rank(1, []bus.Candidate{c})
	if len(st.Ranked) != 1 || !st.Ranked[0].HeardAt.Equal(heard) {
		t.Errorf("heard_at = %+v", st.Ranked)
	}
}

func TestBmrecRowCarriesATuneMode(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.SetBmrecGate(GateQualified)
	var rows []bus.Candidate
	for i, hz := range []float64{14_221_800, 14_254_600, 7_158_000, 14_030_000, 14_080_000, 21_300_000, 3_750_000} {
		c := cand(int64(i+1), "DL"+string(rune('A'+i))+"AAA", "20m", true)
		c.FreqHz, c.Qualified = hz, boolp(true)
		rows = append(rows, c)
	}
	st := f.rank(1, rows)
	want := map[int64]string{1: "usb", 2: "usb", 3: "lsb", 4: "cw", 5: "data", 6: "usb", 7: "lsb"}
	for _, r := range st.Ranked {
		if r.Mode != want[r.RowID] {
			t.Errorf("row %d at %d Hz: mode %q, want %q", r.RowID, r.FreqHz, r.Mode, want[r.RowID])
		}
	}
	if len(st.Ranked) != len(rows) {
		t.Errorf("ranked %d of %d rows", len(st.Ranked), len(rows))
	}
}
