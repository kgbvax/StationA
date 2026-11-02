package store

import (
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "cq.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReopenKeepsData(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cq.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("k", "v"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, ok, _ := s.Setting("k"); !ok || v != "v" {
		t.Fatalf("got %q %v", v, ok)
	}
}

func TestWorkedSetAcrossSources(t *testing.T) {
	s := open(t)
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if err := s.AddLogQSOs([]LogQSO{{Source: SourceLog4OM, Call: "EA8XYZ", Band: "20m", Mode: "SSB", At: at}, {Source: SourceLog4OM, Call: "EA8XYZ", Band: "20m", Mode: "SSB", At: at}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceLogQSOs(SourceClubLog, []LogQSO{{Call: "VP8LP", Band: "40m", Mode: "CW", At: at}}); err != nil {
		t.Fatal(err)
	}
	w, err := s.WorkedSet()
	if err != nil {
		t.Fatal(err)
	}
	if !w[WorkedKey{"EA8XYZ", "20m"}] || !w[WorkedKey{"VP8LP", "40m"}] || len(w) != 2 {
		t.Fatalf("%v", w)
	}
	// a second Club Log download replaces only Club Log's QSOs
	if err := s.ReplaceLogQSOs(SourceClubLog, nil); err != nil {
		t.Fatal(err)
	}
	if w, _ := s.WorkedSet(); len(w) != 1 {
		t.Fatalf("%v", w)
	}
}

func TestDXSkipsExpire(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
	if err := s.SetDXSkip(DXSkip{"EA8XYZ", "20m", now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.DXSkips(now); len(k) != 1 {
		t.Fatalf("%v", k)
	}
	if k, _ := s.DXSkips(now.Add(time.Hour)); len(k) != 0 {
		t.Fatalf("%v", k)
	}
}

func TestConfirmationsReplaced(t *testing.T) {
	s := open(t)
	if err := s.ReplaceConfirmations([]Confirmation{{29, "20m"}, {29, "40m"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceConfirmations([]Confirmation{{230, "20m"}}); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Confirmations(); len(c) != 1 || c[0].DXCC != 230 {
		t.Fatalf("%v", c)
	}
}
