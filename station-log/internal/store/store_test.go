package store

import (
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func q(call string, at time.Time) QSO {
	return QSO{OwnCall: "DL9ET", Call: call, Band: "20m", Mode: "SSB", ModeClass: ClassPhone, At: at, Source: "t"}
}

var t0 = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

func TestUpsertAndMergeRules(t *testing.T) {
	s := open(t)
	a := q("K1ABC", t0)
	a.State = "MA"
	a.FreqHz = 14_205_000
	r, err := s.Upsert(a, 5*time.Minute)
	if err != nil || r.Action != ActionAdded || r.QSO.ID == "" {
		t.Fatalf("%+v %v", r, err)
	}
	in := q("K1ABC", t0.Add(-time.Minute))
	in.Mode, in.FreqHz, in.LoTWRcvd, in.DXCC = "USB", 14_206_000, true, 291
	r, _ = s.Upsert(in, 5*time.Minute)
	if r.Action != ActionMerged {
		t.Fatalf("%+v", r)
	}
	g := r.QSO
	// mode and frequency are only filled in; state is kept (incoming empty); time is the earliest
	if g.Mode != "SSB" || g.FreqHz != 14_205_000 || g.State != "MA" || g.DXCC != 291 || !g.Confirmed || !g.At.Equal(t0.Add(-time.Minute)) {
		t.Fatalf("%+v", g)
	}
	r, _ = s.Upsert(in, 5*time.Minute)
	if r.Action != "" {
		t.Fatalf("repeat: %+v", r)
	}
	n, _ := s.Count()
	if n != 1 {
		t.Fatal(n)
	}
}

func TestClosestMatchWins(t *testing.T) {
	s := open(t)
	s.Upsert(q("K1ABC", t0), time.Minute)
	s.Upsert(q("K1ABC", t0.Add(4*time.Minute)), time.Minute)
	r, _ := s.Upsert(func() QSO { x := q("K1ABC", t0.Add(3*time.Minute+30*time.Second)); x.State = "NY"; return x }(), 5*time.Minute)
	if r.Action != ActionMerged || r.QSO.ID != "2" {
		t.Fatalf("%+v", r)
	}
}

func TestValidate(t *testing.T) {
	s := open(t)
	if _, err := s.Upsert(QSO{Call: "A"}, time.Minute); err == nil {
		t.Fatal("expected a validation error")
	}
}

func TestWorkedViewAndStats(t *testing.T) {
	s := open(t)
	now := t0
	s.Now = func() time.Time { return now }
	a := q("K1ABC", t0)
	a.DXCC = 291
	s.Upsert(a, time.Minute)
	b := q("K1ABC", t0.Add(24*time.Hour))
	b.Mode, b.State, b.LoTWRcvd = "USB", "MA", true
	s.Upsert(b, time.Minute)
	c := q("K1ABC", t0)
	c.ModeClass, c.Mode = ClassCW, "CW"
	s.Upsert(c, time.Minute)
	now = t0.Add(48 * time.Hour)
	d := q("W1AW", t0)
	d.OwnCall = "DK0XYZ"
	s.Upsert(d, time.Minute)

	w, err := s.WorkedView("DL9ET", time.Time{})
	if err != nil || len(w) != 2 {
		t.Fatalf("%+v %v", w, err)
	}
	var ph Worked
	for _, x := range w {
		if x.ModeClass == ClassPhone {
			ph = x
		}
	}
	if ph.Mode != "USB" || !ph.Confirmed || ph.DXCC != 291 || ph.State != "MA" || !ph.At.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("%+v", ph)
	}
	if all, _ := s.WorkedView("", time.Time{}); len(all) != 3 {
		t.Fatalf("%d", len(all))
	}
	if delta, _ := s.WorkedView("", t0.Add(time.Hour)); len(delta) != 1 || delta[0].Call != "W1AW" {
		t.Fatalf("%+v", delta)
	}
	st, err := s.Stats()
	if err != nil || st.QSOCount != 4 || st.ConfirmedCount != 1 || len(st.OwnCalls) != 2 || st.Sources["t"] != 4 || st.FirstAt == nil {
		t.Fatalf("%+v %v", st, err)
	}
}

// Two log entries of one QSO with different locator precision must not make
// every re-import look like a change (seen with a real Log4OM export).
func TestMergeGridPrefersPreciseAndIsStable(t *testing.T) {
	q := QSO{Grid: "LK08pw"}
	if q.Merge(QSO{Grid: "LK08"}) || q.Grid != "LK08pw" {
		t.Fatalf("shorter grid changed %q", q.Grid)
	}
	if !q.Merge(QSO{Grid: "LK08pw44"}) || q.Grid != "LK08pw44" {
		t.Fatalf("longer grid not taken %q", q.Grid)
	}
	e := QSO{}
	if !e.Merge(QSO{Grid: "KM64lo"}) || e.Merge(QSO{Grid: "KM65"}) || e.Grid != "KM64lo" {
		t.Fatalf("grid %q", e.Grid)
	}
}

func TestMergeSameSourceOnlyFillsOtherSourceCorrects(t *testing.T) {
	q := QSO{Source: "log4om-file", CQZone: 4, State: "MI"}
	if q.Merge(QSO{Source: "log4om-file", CQZone: 5, State: "OH"}) || q.CQZone != 4 || q.State != "MI" {
		t.Fatalf("same source changed %+v", q)
	}
	if !q.Merge(QSO{Source: "log4om-file", ITUZone: 8}) || q.ITUZone != 8 {
		t.Fatalf("gap not filled %+v", q)
	}
	if !q.Merge(QSO{Source: "other", CQZone: 5}) || q.CQZone != 5 {
		t.Fatalf("other source did not correct %+v", q)
	}
}

func TestGridForCall(t *testing.T) {
	s := open(t)
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	a := q("K1ABC", t0)
	a.Grid = "FN42"
	s.Upsert(a, time.Minute)
	b := q("K1ABC", t0.Add(time.Hour))
	b.Grid = "FN42AB"
	s.Upsert(b, time.Minute)
	s.Upsert(q("W2XYZ", t0), time.Minute)
	if g, err := s.GridForCall(" k1abc "); err != nil || g != "FN42AB" {
		t.Fatalf("%q %v", g, err)
	}
	if g, err := s.GridForCall("W2XYZ"); err != nil || g != "" {
		t.Fatalf("no grid: %q %v", g, err)
	}
	if g, err := s.GridForCall("NOBODY"); err != nil || g != "" {
		t.Fatalf("unknown: %q %v", g, err)
	}
}

func TestWeakSourceFillsButNeverOverwrites(t *testing.T) {
	s := open(t)
	a := q("K1ABC", t0)
	a.DXCC, a.State, a.CQZone = 291, "MA", 5
	if _, err := s.Upsert(a, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	in := q("K1ABC", t0)
	in.Source, in.DXCC, in.State, in.CQZone, in.ITUZone, in.LoTWRcvd, in.Grid = "wavelog", 110, "NY", 4, 8, true, "FN42aa"
	r, err := s.Upsert(in, 5*time.Minute)
	if err != nil || r.Action != ActionMerged {
		t.Fatalf("%+v %v", r, err)
	}
	g := r.QSO
	if g.DXCC != 291 || g.State != "MA" || g.CQZone != 5 {
		t.Fatalf("wavelog overwrote logger values: %+v", g)
	}
	if g.ITUZone != 8 || !g.LoTWRcvd || g.Grid != "FN42aa" {
		t.Fatalf("gaps, flags and a longer grid still count: %+v", g)
	}
	// a logger value still corrects a wavelog-first QSO
	b := q("K2XYZ", t0)
	b.Source, b.DXCC = "wavelog", 110
	s.Upsert(b, 5*time.Minute)
	c := q("K2XYZ", t0)
	c.DXCC = 291
	r, _ = s.Upsert(c, 5*time.Minute)
	if r.QSO.DXCC != 291 {
		t.Fatalf("%+v", r.QSO)
	}
}

func TestPushLedgerCascadesOnDelete(t *testing.T) {
	s := open(t)
	r, _ := s.Upsert(q("K1ABC", t0), 5*time.Minute)
	if err := s.MarkPushed(r.QSO.ID, "1", "fp", ""); err != nil {
		t.Fatal(err)
	}
	tx, _ := s.Begin()
	tx.SaveRaw(r.QSO.ID, "t", "<CALL:5>K1ABC<EOR>")
	if _, err := tx.Delete(r.QSO.ID); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	if n, _ := s.PushedCount("1"); n != 0 {
		t.Fatalf("ledger rows left: %d", n)
	}
	var raw int
	s.db.QueryRow(`SELECT COUNT(*) FROM qso_raw`).Scan(&raw)
	if raw != 0 {
		t.Fatal("raw left")
	}
}
