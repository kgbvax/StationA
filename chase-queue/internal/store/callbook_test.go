package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestWorkedBefore(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	if err := s.AddLogQSOs([]LogQSO{
		{Source: SourceLog4OM, Call: "EA8XYZ", Band: "20m", Mode: "SSB", At: t0},
		{Source: SourceClubLog, Call: "EA8XYZ", Band: "20m", Mode: "USB", At: t0.Add(20 * time.Second)}, // same QSO, other source
		{Source: SourceLog4OM, Call: "EA8XYZ", Band: "40m", Mode: "CW", At: t0.Add(time.Hour)},
		{Source: SourceLog4OM, Call: "K1ABC", Band: "15m", Mode: "FT8", At: t0},
	}); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateContest("TEST", "2026-11-01", 0, 0, t0)
	if err != nil {
		t.Fatal(err)
	}
	last := t0.Add(48 * time.Hour)
	if err := s.UpsertContestQSOs(c.ID, []ContestQSO{
		{StationID: "a", QSOID: "1", Call: "EA8XYZ", Band: "80m", Mode: "CW", At: last},
		{StationID: "a", QSOID: "2", Call: "EA8XYZ", Band: "10m", Mode: "CW", At: last.Add(time.Hour), Invalid: true},
	}, t0); err != nil {
		t.Fatal(err)
	}
	got, err := s.WorkedBefore([]string{"EA8XYZ", "DL1NEW"})
	if err != nil {
		t.Fatal(err)
	}
	w := got["EA8XYZ"]
	if w == nil || w.QSOs != 3 || !slices.Equal(w.Bands, []string{"80m", "40m", "20m"}) || !w.Last.Equal(last) || len(w.Modes) == 0 {
		t.Fatalf("%+v", w)
	}
	if _, ok := got["DL1NEW"]; ok || got["K1ABC"] != nil {
		t.Fatalf("%v", got)
	}
}

func TestCallbookCache(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.PutCallbook(CallbookEntry{Call: "DL1ABC", Found: true, Data: `{"name":"Hans"}`, Fetched: now}); err != nil {
		t.Fatal(err)
	}
	e, ok, err := s.Callbook("DL1ABC")
	if err != nil || !ok || !e.Found || e.Data == "" || !e.Fetched.Equal(now) {
		t.Fatalf("%+v %v %v", e, ok, err)
	}
	if err := s.PruneCallbook(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Callbook("DL1ABC"); ok {
		t.Fatal("not pruned")
	}
}
