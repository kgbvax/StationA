package contest

import (
	"testing"
	"time"

	"chase-queue/internal/cty"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newMult(id int64, call string, slots, rank int, age int) Candidate {
	v := Valuation{Class: ClassNew, NewSlots: slots}
	if rank != -1 {
		v.NewDXCC = &cty.Entity{}
	}
	return Candidate{RowID: id, Band: "20m", Call: call, Val: v, Rank: max(rank, 0), FirstSeen: t0.Add(-time.Duration(age) * time.Minute)}
}

func ids(r Result) []int64 {
	var o []int64
	for _, i := range r.Queue {
		o = append(o, i.RowID)
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

func TestOrderTiersAndRank(t *testing.T) {
	cands := []Candidate{
		{RowID: 1, Band: "20m", Call: "DL1AAA", FirstSeen: t0},                                     // unworked
		{RowID: 2, Band: "20m", FirstSeen: t0, Likelihood: 0.4},                                    // candidate
		{RowID: 3, Band: "20m", Call: "K1AAA", Val: Valuation{Class: ClassUnknown}, FirstSeen: t0}, // unknown
		newMult(4, "EA8A", 1, 200, 5),
		newMult(5, "VK9A", 1, 30, 1),
		newMult(6, "P5A", 2, 500, 1),
		newMult(7, "G3A", 1, -1, 50), // district only
		{RowID: 8, Band: "20m", FirstSeen: t0, Likelihood: 0.9},
	}
	got := ids(Order(cands, Verdicts{}))
	want := []int64{6, 5, 4, 7, 3, 1, 8, 2}
	if !eq(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestOrderHides(t *testing.T) {
	cands := []Candidate{
		{RowID: 1, Band: "20m", Call: "EA8ABC"},
		{RowID: 2, Band: "40m", Call: "EA8ABC"},
		{RowID: 3, Band: "20m", Call: "DL1A"},
		{RowID: 4, Band: "20m", Call: "DL2B"},
		{RowID: 5, Band: "20m", Skipped: true},
	}
	r := Order(cands, Verdicts{
		Worked:  func(c, b string) bool { return c == "EA8ABC" && b == "20m" },
		Spotted: func(c, b string) bool { return c == "DL1A" },
		Skipped: func(c Candidate) (bool, bool) { return c.Call == "DL2B", false },
	})
	// EA8ABC on 20m is worked: listed in the worked tier, after the unworked EA8ABC on 40m
	if !eq(ids(r), []int64{2, 1}) {
		t.Fatalf("queue %v", ids(r))
	}
	if r.Queue[1].Tier != TierWorked || r.Queue[0].Tier != TierUnworked {
		t.Fatalf("tiers %v %v", r.Queue[0].Tier, r.Queue[1].Tier)
	}
	reasons := map[int64]string{}
	for _, h := range r.Hidden {
		reasons[h.RowID] = h.Reason
	}
	if reasons[3] != HiddenSpotted || reasons[4] != HiddenSkipped || reasons[5] != HiddenSkipped {
		t.Fatalf("%v", reasons)
	}
	// a worked station that is also skipped or spotted stays hidden
	r = Order(cands[:1], Verdicts{Worked: func(string, string) bool { return true }, Spotted: func(string, string) bool { return true }})
	if len(r.Queue) != 0 {
		t.Fatalf("queue %v", ids(r))
	}
	// a worked station that is also skipped or handled stays hidden
	r = Order(cands[:1], Verdicts{Worked: func(string, string) bool { return true },
		Skipped: func(Candidate) (bool, bool) { return true, false }})
	if len(r.Queue) != 0 || len(r.Hidden) != 1 || r.Hidden[0].Reason != HiddenSkipped {
		t.Fatalf("skipped dupe: %v %v", ids(r), r.Hidden)
	}
	r = Order(cands[:1], Verdicts{Worked: func(string, string) bool { return true }, Handled: func(string, string) bool { return true }})
	if len(r.Queue) != 0 || len(r.Hidden) != 1 || r.Hidden[0].Reason != HiddenHandled {
		t.Fatalf("handled dupe: %v %v", ids(r), r.Hidden)
	}
	// a skipped station returns when something new is known
	r = Order(cands[3:4], Verdicts{Skipped: func(Candidate) (bool, bool) { return true, true }})
	if len(r.Queue) != 1 {
		t.Fatal("did not return")
	}
}

func TestOrderWorkedTierBeforeCandidatesAfterUnworked(t *testing.T) {
	cands := []Candidate{
		{RowID: 1, Band: "20m", Call: "DL1AAA", FirstSeen: t0},
		{RowID: 2, Band: "20m", FirstSeen: t0, Likelihood: 0.4},
		{RowID: 3, Band: "20m", Call: "K1AAA", FirstSeen: t0.Add(-time.Minute)}, // worked
		newMult(4, "EA8A", 1, 200, 5),                                           // worked, but the worked tier wins
	}
	got := ids(Order(cands, Verdicts{Worked: func(c, _ string) bool { return c == "K1AAA" || c == "EA8A" }}))
	if want := []int64{1, 4, 3, 2}; !eq(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
