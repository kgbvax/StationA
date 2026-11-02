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
	if !eq(ids(r), []int64{2}) {
		t.Fatalf("queue %v", ids(r))
	}
	reasons := map[int64]string{}
	for _, h := range r.Hidden {
		reasons[h.RowID] = h.Reason
	}
	if reasons[1] != HiddenWorked || reasons[3] != HiddenSpotted || reasons[4] != HiddenSkipped || reasons[5] != HiddenSkipped {
		t.Fatalf("%v", reasons)
	}
	// a skipped station returns when something new is known
	r = Order(cands[3:4], Verdicts{Skipped: func(Candidate) (bool, bool) { return true, true }})
	if len(r.Queue) != 1 {
		t.Fatal("did not return")
	}
}
