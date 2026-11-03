package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"chase-queue/internal/award"
	"chase-queue/internal/bus"
	"chase-queue/internal/store"
)

func was(code string, rule award.Rule, bands, modes []string) award.Definition {
	return award.Definition{Code: code, Kind: award.KindField, Field: "STATE", Rule: rule, DXCC: []int{291},
		Items: []string{"NY", "CA", "TX"}, Bands: bands, Modes: modes}
}

func catalog(t *testing.T, defs ...award.Definition) *award.Catalog {
	t.Helper()
	c := award.NewCatalog()
	for _, d := range defs {
		if err := c.Add(d); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func (f *fixture) goals(t *testing.T, c *award.Catalog, gs ...Goal) {
	t.Helper()
	if err := f.e.SetAwards(c, gs); err != nil {
		t.Fatal(err)
	}
}

// states answers the callbook lookups of a test.
func (f *fixture) states(m map[string]string) {
	f.e.cfg.State = func(call string) string { return m[call] }
}

func needsOf(st bus.ChaseState, row int64) []bus.Need {
	for _, r := range st.Ranked {
		if r.RowID == row {
			return r.Needs
		}
	}
	return nil
}

func TestNeedsDXCCDefaultGoal(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	st := f.rank(1, []bus.Candidate{cand(1, "EA8XYZ", "20m", true)})
	r := st.Ranked[0]
	want := []bus.Need{{Award: "dxcc", Key: "EA8", Band: "20m", Status: "unworked"}}
	if !r.Needed || r.Tag != bus.TagNeed || fmt.Sprint(r.Needs) != fmt.Sprint(want) {
		t.Fatalf("%+v", r)
	}
	// worked in the log but not confirmed: the slot is still open, with its status
	f.e.SetLogEntries([]award.Entry{{Call: "EA8AAA", DXCC: 29, Band: "20m", ModeClass: "phone"}})
	st = f.rank(2, []bus.Candidate{cand(1, "EA8XYZ", "20m", true)})
	if n := st.Ranked[0].Needs; len(n) != 1 || n[0].Status != "worked-unconfirmed" {
		t.Fatalf("%+v", n)
	}
	// confirmed: not needed, no needs
	f.e.SetLogEntries([]award.Entry{{Call: "EA8AAA", DXCC: 29, Band: "20m", ModeClass: "phone", Confirmed: true}})
	st = f.rank(3, []bus.Candidate{cand(1, "EA8XYZ", "20m", true)})
	if r := st.Ranked[0]; r.Needed || r.Needs != nil || r.Tag != "" {
		t.Fatalf("%+v", r)
	}
}

func TestFieldAwardNeedsAndThreeValued(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 291, Band: "20m"}}) // DXCC for K is done
	f.goals(t, catalog(t, was("WAS-20-phone", award.RuleConfirmed, []string{"20m"}, []string{"phone"})),
		Goal{Award: "WAS-20-phone", Enabled: true})
	rows := []bus.Candidate{cand(1, "K1ABC", "20m", true)}
	// no log view and no state yet: unknown, never "not needed"
	st := f.rank(1, rows)
	if r := st.Ranked[0]; r.Needed || r.Needs != nil {
		t.Fatalf("%+v", r)
	}
	f.e.SetLogEntries(nil)
	st = f.rank(2, rows)
	if r := st.Ranked[0]; r.Needed {
		t.Fatalf("the state is not known: %+v", r)
	}
	// the call's state is known from the callbook
	f.states(map[string]string{"K1ABC": "ny"})
	st = f.rank(3, rows)
	want := bus.Need{Award: "WAS-20-phone", Key: "NY", Band: "20m", Mode: "phone", Status: "unworked"}
	if r := st.Ranked[0]; !r.Needed || len(r.Needs) != 1 || r.Needs[0] != want || r.Tag != bus.TagNeed {
		t.Fatalf("%+v", r)
	}
	// a state outside the items, a band outside the award: not part of it
	f.states(map[string]string{"K1ABC": "AK"})
	if r := f.rank(4, rows).Ranked[0]; r.Needed {
		t.Fatalf("AK is not an item: %+v", r)
	}
	f.states(map[string]string{"K1ABC": "NY"})
	for _, n := range f.rank(5, []bus.Candidate{cand(1, "K1ABC", "40m", true)}).Ranked[0].Needs {
		if n.Award == "WAS-20-phone" {
			t.Fatalf("40m is outside the award: %+v", n)
		}
	}
	// NY worked on 20m phone, not confirmed: still needed; confirmed: done
	f.e.SetLogEntries([]award.Entry{{Call: "K9XYZ", DXCC: 291, State: "NY", Band: "20m", ModeClass: "phone"}})
	if r := f.rank(6, rows).Ranked[0]; !r.Needed || r.Needs[0].Status != "worked-unconfirmed" {
		t.Fatalf("%+v", r)
	}
	f.e.SetLogEntries([]award.Entry{{Call: "K9XYZ", DXCC: 291, State: "NY", Band: "20m", ModeClass: "phone", Confirmed: true}})
	if r := f.rank(7, rows).Ranked[0]; r.Needed {
		t.Fatalf("%+v", r)
	}
	// the same state worked in CW does not fill the phone slot
	f.e.SetLogEntries([]award.Entry{{Call: "K9XYZ", DXCC: 291, State: "NY", Band: "20m", ModeClass: "cw", Confirmed: true}})
	if r := f.rank(8, rows).Ranked[0]; !r.Needed {
		t.Fatalf("%+v", r)
	}
}

func TestFieldAwardStateFromTheLogAndWorkedRule(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 291, Band: "20m"}})
	d := was("WAS", award.RuleWorked, nil, nil)
	allBands := false
	d.PerBand = &allBands
	f.goals(t, catalog(t, d), Goal{Award: "WAS", Enabled: true})
	// no callbook: the state of the call comes from the log's own QSO with it
	f.e.SetLogEntries([]award.Entry{{Call: "K1ABC", DXCC: 291, State: "NY", Band: "40m", ModeClass: "cw"}})
	rows := []bus.Candidate{cand(1, "K1ABC", "20m", true)}
	// rule worked: a QSO on any band and mode fills the slot
	if r := f.rank(1, rows).Ranked[0]; r.Needed {
		t.Fatalf("%+v", r)
	}
	// rule confirmed: the same QSO, not confirmed, leaves the slot open
	d.Rule = award.RuleConfirmed
	f.goals(t, catalog(t, d), Goal{Award: "WAS", Enabled: true})
	f.e.SetLogEntries([]award.Entry{{Call: "K1ABC", DXCC: 291, State: "CA", Band: "40m", ModeClass: "cw"}})
	r := f.rank(2, rows).Ranked[0]
	want := bus.Need{Award: "WAS", Key: "CA", Status: "worked-unconfirmed"}
	if !r.Needed || len(r.Needs) != 1 || r.Needs[0] != want {
		t.Fatalf("the log's own state for the call, over all bands and modes: %+v", r)
	}
}

func TestGoalsOnOff(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.SetLogEntries(nil)
	f.states(map[string]string{"K1ABC": "NY"})
	c := catalog(t, was("WAS", award.RuleConfirmed, nil, nil))
	rows := []bus.Candidate{cand(1, "K1ABC", "20m", true), cand(2, "EA8XYZ", "20m", true)}
	needed := func(st bus.ChaseState) [2]bool { return [2]bool{st.Ranked[0].Needed, st.Ranked[1].Needed} }
	byRow := func(st bus.ChaseState) map[int64][]bus.Need {
		m := map[int64][]bus.Need{}
		for _, r := range st.Ranked {
			m[r.RowID] = r.Needs
		}
		return m
	}
	// defaults: only DXCC is a goal
	got := byRow(f.rank(1, rows))
	if len(got[1]) != 1 || got[1][0].Award != "dxcc" || len(got[2]) != 1 {
		t.Fatalf("%+v", got)
	}
	// WAS on as well
	f.goals(t, c, Goal{Award: "WAS", Enabled: true})
	got = byRow(f.rank(2, rows))
	if len(got[1]) != 2 {
		t.Fatalf("%+v", got)
	}
	// DXCC off: only WAS is left, EA8 needs nothing
	f.goals(t, c, Goal{Award: "WAS", Enabled: true}, Goal{Award: "dxcc", Enabled: false})
	got = byRow(f.rank(3, rows))
	if len(got[1]) != 1 || got[1][0].Award != "WAS" || got[2] != nil {
		t.Fatalf("%+v", got)
	}
	// every goal off: nothing is needed
	f.goals(t, c, Goal{Award: "WAS", Enabled: false}, Goal{Award: "dxcc", Enabled: false})
	if st := f.rank(4, rows); needed(st) != [2]bool{} {
		t.Fatalf("%+v", st.Ranked)
	}
	if err := f.e.SetAwards(c, []Goal{{Award: "nosuch", Enabled: true}}); err == nil {
		t.Fatal("a goal for an unknown award is refused")
	}
}

func TestGoalPriorityThenAwardCountThenNewSlots(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 291, Band: "20m"}, {DXCC: 29, Band: "40m"}})
	f.e.SetLogEntries(nil)
	// DXCC: EA8 needs it (rarest, rank 50); K is done. WAS needs K.
	f.states(map[string]string{"K1ABC": "NY", "K2DEF": "CA"})
	c := catalog(t, was("WAS", award.RuleConfirmed, nil, nil))
	rows := []bus.Candidate{cand(1, "EA8XYZ", "20m", true), cand(2, "K1ABC", "20m", true), cand(3, "DL1ABC", "20m", true)}
	// DXCC 10 < WAS 50 by default: WAS first; then DXCC rows by Most Wanted/new slots (DL never confirmed = 2)
	f.goals(t, c, Goal{Award: "WAS", Enabled: true})
	want(t, f.rank(1, rows), 2, 3, 1)
	// DXCC raised above WAS
	f.goals(t, c, Goal{Award: "WAS", Enabled: true}, Goal{Award: "dxcc", Priority: 90, Enabled: true})
	want(t, f.rank(2, rows), 3, 1, 2)
	// a station both awards need goes ahead of one only the first needs
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 29, Band: "40m"}})
	rows = []bus.Candidate{cand(1, "EA8XYZ", "20m", true), cand(2, "K1ABC", "20m", true)}
	f.goals(t, c, Goal{Award: "WAS", Priority: 20, Enabled: true}, Goal{Award: "dxcc", Priority: 20, Enabled: true})
	st := f.rank(3, rows)
	want(t, st, 2, 1)
	if len(st.Ranked[0].Needs) != 2 {
		t.Fatalf("%+v", st.Ranked[0].Needs)
	}
}

func TestContestIsAnAward(t *testing.T) {
	f := newContest(t, logq("DL1AAA", "20m"))
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 230, Band: "20m"}, {DXCC: 29, Band: "40m"}})
	rows := []bus.Candidate{cand(1, "EA8XYZ", "20m", true), cand(2, "DL2BBB", "20m", true), cand(3, "G4ABC", "20m", true)}
	st := f.rank(1, rows)
	if st.Activity != bus.ActivityContest || st.Contest == nil {
		t.Fatalf("%+v", st)
	}
	// EA8 and G are new multipliers and new DXCC (contest and DXCC); DL held in both
	want(t, st, 1, 3, 2)
	r := st.Ranked[0]
	if r.Tag != bus.TagMult || len(r.Needs) != 2 || r.Needs[0].Award != "contest" || r.Needs[0].Key != "EA8" || r.Needs[0].Band != "20m" ||
		r.Needs[1].Award != "dxcc" {
		t.Fatalf("%+v", r)
	}
	// the contest goal off: the rows keep their DXCC needs and are tagged "need"
	f.goals(t, award.NewCatalog(), Goal{Award: "contest", Enabled: false})
	r = f.rank(2, rows).Ranked[0]
	if !r.Needed || r.Tag != bus.TagNeed || len(r.Needs) != 1 || r.Needs[0].Award != "dxcc" {
		t.Fatalf("%+v", r)
	}
	// DXCC off, contest on: the old contest-only behaviour
	f.goals(t, award.NewCatalog(), Goal{Award: "dxcc", Enabled: false})
	st = f.rank(3, rows)
	want(t, st, 1, 3, 2)
	if r := st.Ranked[0]; r.Tag != bus.TagMult || len(r.Needs) != 1 || r.Needs[0].Award != "contest" {
		t.Fatalf("%+v", r)
	}
	// leaving the contest leaves its award
	f.e.ActivateDX()
	if st := f.rank(4, rows); st.Activity != bus.ActivityDX || st.Contest != nil || st.Ranked[0].Needed {
		t.Fatalf("DXCC is off and no contest runs: %+v", st)
	}
}

func TestContestTieBreaksByMostWanted(t *testing.T) {
	f := newContest(t, logq("DL1AAA", "20m"))
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 230, Band: "20m"}})
	// G never confirmed (DXCC 2 new slots), EA8 first seen earlier; both mults
	rows := []bus.Candidate{cand(1, "EA8XYZ", "20m", true), cand(2, "G4ABC", "20m", true)}
	st := f.rank(1, rows)
	// equal priority and awards: NewSlots of the primary (contest: 1 each), then Most Wanted (EA8 rank 50)
	want(t, st, 1, 2)
}

func TestStateSizeStaysSmall(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations(nil)
	f.e.SetLogEntries(nil)
	states := map[string]string{}
	var rows []bus.Candidate
	for i := 1; i <= 40; i++ {
		call := fmt.Sprintf("K%dABC", i)
		states[call] = []string{"NY", "CA", "TX"}[i%3]
		rows = append(rows, cand(int64(i), call, "20m", true))
	}
	f.states(states)
	c := catalog(t, was("WAS-20", award.RuleConfirmed, []string{"20m"}, nil), was("WAS-phone", award.RuleConfirmed, nil, []string{"phone"}))
	f.goals(t, c, Goal{Award: "WAS-20", Enabled: true}, Goal{Award: "WAS-phone", Enabled: true})
	st := f.rank(1, rows)
	st.TS = time.Now()
	for _, r := range st.Ranked {
		if len(r.Needs) != 3 {
			t.Fatalf("%+v", r)
		}
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 64<<10/2 {
		t.Fatalf("state of 40 rows with 3 needs is %d bytes", len(b))
	}
	t.Logf("40 rows, 3 needs each: %d bytes", len(b))
}

func TestClusterRowCarriesModeClassNeed(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 291, Band: "20m"}})
	f.e.SetLogEntries(nil)
	f.states(map[string]string{"K1ABC": "NY"})
	f.goals(t, catalog(t, was("WAS-20-CW", award.RuleConfirmed, []string{"20m"}, []string{"cw"})), Goal{Award: "WAS-20-CW", Enabled: true})
	f.e.AddClusterSpot(ClusterSpot{Call: "K1ABC", Band: "20m", FreqHz: 14_030_000, Comment: "CW 599", Spotter: "DL1XYZ", At: f.now})
	st := f.rank(1, nil)
	if len(st.Ranked) != 1 || st.Ranked[0].Source != bus.SourceCluster {
		t.Fatalf("%+v", st.Ranked)
	}
	want := bus.Need{Award: "WAS-20-CW", Key: "NY", Band: "20m", Mode: "cw", Status: "unworked"}
	if n := st.Ranked[0].Needs; len(n) != 1 || n[0] != want {
		t.Fatalf("%+v", n)
	}
	// the same station in SSB is not part of the CW award
	f.e.AddClusterSpot(ClusterSpot{Call: "K1ABC", Band: "20m", FreqHz: 14_250_000, Comment: "SSB", Spotter: "DL1XYZ", At: f.now})
	if st := f.rank(2, nil); len(st.Ranked) != 0 {
		t.Fatalf("%+v", st.Ranked)
	}
}

func TestHeardRowValuedByBandPlanMode(t *testing.T) {
	f := newDX(t)
	f.e.SetConfirmations([]store.Confirmation{{DXCC: 291, Band: "20m"}})
	f.e.SetLogEntries(nil)
	f.states(map[string]string{"K1ABC": "NY"})
	cw := cand(1, "K1ABC", "20m", true)
	cw.FreqHz = 14_025_000
	f.goals(t, catalog(t, was("WAS-phone", award.RuleConfirmed, []string{"20m"}, []string{"phone"}),
		was("WAS-CW", award.RuleConfirmed, []string{"20m"}, []string{"cw"})),
		Goal{Award: "WAS-phone", Enabled: true}, Goal{Award: "WAS-CW", Enabled: true})
	n := needsOf(f.rank(1, []bus.Candidate{cw}), 1)
	if len(n) != 1 || n[0].Award != "WAS-CW" || n[0].Mode != "cw" {
		t.Fatalf("a CW CQ is a CW need, not a phone one: %+v", n)
	}
}

func TestAwardProblems(t *testing.T) {
	f := newDX(t)
	f.goals(t, catalog(t, was("WAS-20-phone", award.RuleConfirmed, []string{"20m"}, []string{"phone"})),
		Goal{Award: "WAS-20-phone", Enabled: true})
	if p := f.e.AwardProblems(false); len(p) != 1 || !strings.Contains(p[0], "WAS-20-phone") {
		t.Fatalf("%v", p)
	}
	if p := f.e.AwardProblems(true); len(p) != 2 {
		t.Fatalf("DXCC is on and has no confirmations: %v", p)
	}
	f.e.SetLogEntries(nil)
	if p := f.e.AwardProblems(true); len(p) != 0 {
		t.Fatalf("%v", p)
	}
}
