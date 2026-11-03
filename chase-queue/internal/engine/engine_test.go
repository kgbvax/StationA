package engine

import (
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"chase-queue/internal/bus"
	"chase-queue/internal/contest"
	"chase-queue/internal/cty"
	"chase-queue/internal/store"
)

type memStore struct {
	mu      sync.Mutex
	handled map[string]string
	skips   map[string]time.Time
}

func (m *memStore) MarkContestHandled(_ int64, call, band, how string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handled[call+"|"+band] = how
	return nil
}
func (m *memStore) ClearContestHandled(_ int64, call, band string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.handled, call+"|"+band)
	return nil
}
func (m *memStore) SetDXSkip(k store.DXSkip) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.skips[k.Call+"|"+k.Band] = k.Until
	return nil
}
func (m *memStore) ClearDXSkip(call, band string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.skips, call+"|"+band)
	return nil
}

type fixture struct {
	e   *Engine
	st  *memStore
	now time.Time
	db  *cty.DB
}

var ranks = map[int]int{29: 50, 230: 200} // EA8 rarer than DL; G unranked

func newDX(t *testing.T) *fixture {
	t.Helper()
	db, err := cty.LoadFile("testdata/cty.dat")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.LoadNumbers("testdata/cty.csv"); err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: &memStore{handled: map[string]string{}, skips: map[string]time.Time{}},
		now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), db: db}
	f.e = New(Config{Store: f.st, CTY: db, Now: func() time.Time { return f.now },
		Rank: func(a int) (int, bool) { r, ok := ranks[a]; return r, ok }})
	return f
}

func newContest(t *testing.T, qsos ...store.ContestQSO) *fixture {
	t.Helper()
	f := newDX(t)
	fh, err := os.Open("testdata/dxcc-only.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	def, err := contest.ParseDefinition(fh)
	if err != nil {
		t.Fatal(err)
	}
	f.e.ActivateContest(store.Contest{ID: 1, Name: "SOMEDX"}, &contest.Model{Def: def, CTY: f.db}, nil, qsos)
	return f
}

func cand(id int64, call, band string, trusted bool) bus.Candidate {
	return bus.Candidate{RowID: id, Call: call, Band: band, Trusted: trusted, FreqHz: 14.2e6 + float64(id)*1000,
		FirstUTC: time.Date(2026, 11, 1, 11, 0, int(id), 0, time.UTC)}
}

func (f *fixture) rank(seq int64, rows []bus.Candidate, spots ...bus.Spot) bus.ChaseState {
	f.e.SetCandidates(bus.CQFinderState{Seq: seq, Rows: rows, Spotted: spots})
	return f.e.Compute()
}

func ids(st bus.ChaseState) []int64 {
	out := []int64{}
	for _, r := range st.Ranked {
		out = append(out, r.RowID)
	}
	return out
}

func logq(call, band string) store.ContestQSO {
	return store.ContestQSO{StationID: "OP1", QSOID: call + band, Call: call, Band: band, Mode: "SSB"}
}

func want(t *testing.T, st bus.ChaseState, ws ...int64) {
	t.Helper()
	if got := ids(st); !slices.Equal(got, append([]int64{}, ws...)) {
		t.Fatalf("order %v, want %v", got, ws)
	}
}

func TestContestWorkedHiddenOtherBandListed(t *testing.T) {
	f := newContest(t, logq("EA8ABC", "20m"))
	st := f.rank(1, []bus.Candidate{cand(1, "EA8ABC", "20m", true), cand(2, "EA8ABC", "40m", true)})
	want(t, st, 2)
	if !st.Ranked[0].Needed || st.Ranked[0].Tag != bus.TagMult {
		t.Fatalf("%+v", st.Ranked[0])
	}
}

func TestContestNewMultFirstThenOthers(t *testing.T) {
	f := newContest(t, logq("DL1AAA", "20m"))
	st := f.rank(1, []bus.Candidate{cand(1, "DL2BBB", "20m", true), cand(2, "", "20m", false), cand(3, "EA8XYZ", "20m", true)})
	want(t, st, 3, 1, 2)
	if st.Ranked[1].Needed || st.Ranked[1].Tag != "" {
		t.Fatalf("%+v", st.Ranked[1])
	}
}

func TestSpottedLeavesAtOnceAndAgesOut(t *testing.T) {
	// EA8 is a worked multiplier (not needed): a spot bmrec relays hides it
	f := newContest(t, logq("EA8AAA", "20m"))
	rows := []bus.Candidate{cand(1, "EA8XYZ", "20m", true), cand(2, "EA8/DL1ABC", "20m", true)}
	want(t, f.rank(1, rows, bus.Spot{Call: "EA8XYZ", Band: "20m", At: f.now.Add(-time.Minute)}), 2)
	want(t, f.rank(2, rows, bus.Spot{Call: "DL1ABC", Band: "20m", At: f.now}), 1, 2)
	f.now = f.now.Add(31 * time.Minute)
	want(t, f.rank(3, rows, bus.Spot{Call: "EA8XYZ", Band: "20m", At: f.now.Add(-31 * time.Minute)}), 1, 2)
}

func TestBmrecOnlySpotNeverHidesANeededStation(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 230, Band: "20m"}}) // DL confirmed on 20m
	rows := []bus.Candidate{cand(1, "EA8XYZ", "20m", true), cand(2, "DL3CCC", "20m", true)}
	spots := []bus.Spot{{Call: "EA8XYZ", Band: "20m", At: f.now, Source: "own"}, {Call: "DL3CCC", Band: "20m", At: f.now, Source: "cluster"}}
	// no cluster row stands for EA8XYZ, so the needed heard row stays; DL3CCC is not needed and leaves
	st := f.rank(1, rows, spots...)
	want(t, st, 1)
	if st.Ranked[0].Tag != bus.TagNeed {
		t.Fatalf("%+v", st.Ranked[0])
	}
	// once our own cluster login has the spot, its cluster row replaces the heard row
	f.e.AddClusterSpot(ClusterSpot{Call: "EA8XYZ", Band: "20m", FreqHz: 14025000, At: f.now})
	st = f.e.Compute()
	if len(st.Ranked) != 1 || st.Ranked[0].Source != bus.SourceCluster || st.Ranked[0].Call != "EA8XYZ" {
		t.Fatalf("%+v", st.Ranked)
	}
}

func TestRestartRebuildsSpotsFromState(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 29, Band: "20m"}}) // EA8 confirmed: not needed, so the spot hides it
	st := f.rank(7, []bus.Candidate{cand(1, "EA8XYZ", "20m", true)}, bus.Spot{Call: "EA8XYZ", Band: "20m", At: f.now.Add(-10 * time.Minute), Source: "own"})
	want(t, st)
	if st.Seq != 7 {
		t.Fatal(st.Seq)
	}
}

func TestStaleSeqIgnored(t *testing.T) {
	f := newDX(t)
	f.rank(5, []bus.Candidate{cand(1, "EA8XYZ", "20m", true)})
	if f.e.SetCandidates(bus.CQFinderState{Seq: 4}) {
		t.Fatal("older state taken")
	}
	if st := f.e.Compute(); st.Seq != 5 || len(st.Ranked) != 1 {
		t.Fatalf("%+v", st)
	}
}

func TestContestSkipReturnsWhenConfirmed(t *testing.T) {
	f := newContest(t)
	f.rank(1, []bus.Candidate{cand(1, "EA8XYZ", "20m", false)})
	if err := f.e.Skip(bus.Target{RowID: 1, Band: "20m"}); err != nil {
		t.Fatal(err)
	}
	want(t, f.rank(2, []bus.Candidate{cand(1, "EA8XYZ", "20m", false)}))
	want(t, f.rank(3, []bus.Candidate{cand(1, "EA8XYZ", "20m", true)}), 1)
}

func TestActionOnUnlistedRowRefused(t *testing.T) {
	f := newContest(t)
	f.rank(1, []bus.Candidate{cand(1, "EA8XYZ", "20m", true)})
	if err := f.e.Skip(bus.Target{RowID: 1, Band: "20m"}); err != nil {
		t.Fatal(err)
	}
	if err := f.e.Handled(bus.Target{RowID: 1, Band: "20m", How: "spot"}); !errors.Is(err, ErrNotListed) {
		t.Fatalf("got %v", err)
	}
}

func TestContestHandledPersistsAndUndo(t *testing.T) {
	f := newContest(t)
	rows := []bus.Candidate{cand(1, "EA8XYZ", "20m", true)}
	f.rank(1, rows)
	if err := f.e.Handled(bus.Target{RowID: 1, Band: "20m", Call: "ea8xyz", How: "spot"}); err != nil {
		t.Fatal(err)
	}
	if f.st.handled["EA8XYZ|20m"] != "spot" {
		t.Fatalf("%v", f.st.handled)
	}
	st := f.rank(2, rows)
	want(t, st)
	if st.Undo != "EA8XYZ" {
		t.Fatal(st.Undo)
	}
	if err := f.e.Undo(); err != nil {
		t.Fatal(err)
	}
	want(t, f.rank(3, rows), 1)
	if len(f.st.handled) != 0 {
		t.Fatal(f.st.handled)
	}
	if err := f.e.Undo(); !errors.Is(err, ErrNothingToUndo) {
		t.Fatal(err)
	}
}

func TestDXOrderNeverConfirmedFirstThenRarest(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 29, Band: "40m"}, {DXCC: 291, Band: "20m"}})
	st := f.rank(1, []bus.Candidate{cand(1, "EA8XYZ", "20m", true), cand(2, "DL1ABC", "20m", true),
		cand(3, "K1ABC", "20m", true), cand(4, "G4ABC", "20m", true)})
	// DL (never confirmed, ranked), G (never confirmed, unranked), EA8 (new on band), K (confirmed: not needed)
	want(t, st, 2, 4, 1, 3)
	for i, needed := range []bool{true, true, true, false} {
		r := st.Ranked[i]
		if r.Needed != needed || (needed && r.Tag != bus.TagNeed) || (!needed && r.Tag != "") {
			t.Fatalf("%d: %+v", i, r)
		}
	}
}

func TestDXWorkedHidesButEntityStaysNeeded(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.AddDXWorked("EA8XYZ", "20m")
	st := f.rank(1, []bus.Candidate{cand(1, "EA8XYZ", "20m", true), cand(2, "EA8ABC", "20m", true)})
	want(t, st, 2)
	if !st.Ranked[0].Needed {
		t.Fatal("entity should stay needed")
	}
}

func TestDXUnknownUntilConfirmationsLoad(t *testing.T) {
	f := newDX(t)
	st := f.rank(1, []bus.Candidate{cand(1, "EA8XYZ", "20m", true)})
	if len(st.Ranked) != 1 || st.Ranked[0].Needed || st.Ranked[0].Tag != "" {
		t.Fatalf("%+v", st)
	}
}

func TestDXQSYKeepsStation(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	rows := []bus.Candidate{cand(1, "EA8XYZ", "20m", true)}
	f.rank(1, rows)
	if err := f.e.Handled(bus.Target{RowID: 1, Band: "20m", How: "qsy"}); err != nil {
		t.Fatal(err)
	}
	want(t, f.rank(2, rows), 1)
}

func TestDXSkipUntilUTCMidnight(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.now = time.Date(2026, 11, 1, 23, 30, 0, 0, time.UTC)
	rows := []bus.Candidate{cand(1, "EA8XYZ", "20m", true)}
	f.rank(1, rows)
	if err := f.e.Skip(bus.Target{RowID: 1, Band: "20m"}); err != nil {
		t.Fatal(err)
	}
	if got := f.st.skips["EA8XYZ|20m"]; !got.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("until %v", got)
	}
	want(t, f.rank(2, rows))
	f.now = time.Date(2026, 11, 2, 0, 0, 1, 0, time.UTC)
	want(t, f.rank(3, rows), 1)
}

func TestUndoDoesNotCrossActivities(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.rank(1, []bus.Candidate{cand(1, "EA8XYZ", "20m", true)})
	if err := f.e.Skip(bus.Target{RowID: 1, Band: "20m"}); err != nil {
		t.Fatal(err)
	}
	f.e.ActivateDX()
	if err := f.e.Undo(); !errors.Is(err, ErrNothingToUndo) {
		t.Fatalf("got %v", err)
	}
}

func TestSeqRestartAcceptedWhenNewer(t *testing.T) {
	f := newDX(t)
	f.e.SetCandidates(bus.CQFinderState{Seq: 5000, TS: f.now, Rows: []bus.Candidate{cand(1, "EA8XYZ", "20m", true)}})
	// bmrec restarted: its counter starts again, but its clock moved on
	if !f.e.SetCandidates(bus.CQFinderState{Seq: 1, TS: f.now.Add(time.Minute), Rows: []bus.Candidate{cand(2, "DL1ABC", "20m", true)}}) {
		t.Fatal("restarted bmrec ignored")
	}
	if st := f.e.Compute(); len(st.Ranked) != 1 || st.Ranked[0].RowID != 2 {
		t.Fatalf("%+v", st)
	}
	// a lower seq with an older clock is still stale
	f.e.SetCandidates(bus.CQFinderState{Seq: 7, TS: f.now.Add(2 * time.Minute)})
	if f.e.SetCandidates(bus.CQFinderState{Seq: 3, TS: f.now, Rows: []bus.Candidate{cand(9, "G4ABC", "20m", true)}}) {
		t.Fatal("stale state taken")
	}
}

func TestActionUsesTheListedCallNotThePayloadCall(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.rank(1, []bus.Candidate{cand(1, "EA8XYZ", "20m", true)})
	if err := f.e.Skip(bus.Target{RowID: 1, Band: "20m", Call: "K1ABC"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.st.skips["EA8XYZ|20m"]; !ok || len(f.st.skips) != 1 {
		t.Fatalf("skipped %v", f.st.skips)
	}
}

func TestTypedCallForCalllessRowIsValidated(t *testing.T) {
	f := newContest(t)
	f.rank(1, []bus.Candidate{cand(1, "", "20m", false)})
	if err := f.e.Handled(bus.Target{RowID: 1, Band: "20m", Call: "x'; DROP TABLE", How: "spot"}); !errors.Is(err, errBadCall) {
		t.Fatalf("got %v", err)
	}
	if err := f.e.Handled(bus.Target{RowID: 1, Band: "20m", Call: "ea8xyz", How: "spot"}); err != nil || f.st.handled["EA8XYZ|20m"] != "spot" {
		t.Fatalf("%v %v", err, f.st.handled)
	}
}
