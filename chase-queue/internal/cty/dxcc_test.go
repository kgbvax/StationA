package cty

import (
	"os"
	"path/filepath"
	"testing"
)

func waeDB(t *testing.T, withCSV bool) *DB {
	t.Helper()
	db, err := LoadFile(filepath.Join("testdata", "wae-cty.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if withCSV {
		if err := db.LoadNumbers(filepath.Join("testdata", "mini-cty.csv")); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestWAEFlag(t *testing.T) {
	db := waeDB(t, false)
	e, ok := db.Lookup("IT9ABC")
	if !ok || !e.WAE || e.Primary != "IT9" {
		t.Fatalf("IT9ABC = %+v, %v; want WAE entity IT9", e, ok)
	}
	if e, _ := db.Lookup("I1ABC"); e.WAE {
		t.Errorf("I1ABC flagged WAE: %+v", e)
	}
}

func TestDXCCFoldsWAE(t *testing.T) {
	for _, csv := range []bool{false, true} {
		db := waeDB(t, csv)
		for _, c := range []struct{ call, want string }{
			{"IT9ABC", "I"},
			{"I1ABC", "I"},
			{"GM3ZZZ", "GM"}, // exact entry of Shetland folds into Scotland
			{"TA1ABC", "TA"},
			{"EA8/DL1ABC", "EA8"}, // Canary Islands is a DXCC entity of its own
		} {
			e, ok := db.DXCC(c.call)
			if !ok || e.Primary != c.want || e.WAE {
				t.Errorf("csv=%v DXCC(%q) = %+v, %v; want primary %q", csv, c.call, e, ok, c.want)
			}
		}
	}
}

func TestADIFNumbers(t *testing.T) {
	db := waeDB(t, true)
	cases := []struct {
		call string
		adif int
	}{{"DL1ABC", 230}, {"IT9ABC", 248}, {"EA8/DL1ABC", 29}, {"KH6XYZ", 110}}
	for _, c := range cases {
		e, ok := db.DXCC(c.call)
		if !ok || e.ADIF != c.adif {
			t.Errorf("DXCC(%q).ADIF = %d (ok=%v); want %d", c.call, e.ADIF, ok, c.adif)
		}
	}
	if e, _ := waeDB(t, false).DXCC("DL1ABC"); e.ADIF != 0 {
		t.Errorf("without cty.csv the ADIF number is %d, want 0", e.ADIF)
	}
}

func TestOpenIgnoresCSV(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, filepath.Join("testdata", "wae-cty.dat"), filepath.Join(dir, "cty.dat"))
	copyFile(t, filepath.Join("testdata", "mini-cty.csv"), filepath.Join(dir, "cty.csv"))
	db, err := Open(dir)
	if err != nil || db == nil {
		t.Fatalf("Open: %v, %v", db, err)
	}
	if e, _ := db.Lookup("DL1ABC"); e.ADIF != 0 {
		t.Errorf("Open loaded cty.csv numbers (%d); the board's DB must stay cty.dat only", e.ADIF)
	}
	cdb, err := OpenContest(dir)
	if err != nil || cdb == nil {
		t.Fatalf("OpenContest: %v, %v", cdb, err)
	}
	if e, _ := cdb.DXCC("DL1ABC"); e.ADIF != 230 {
		t.Errorf("OpenContest DL1ABC ADIF = %d, want 230", e.ADIF)
	}
	if missing, err := OpenContest(t.TempDir()); missing != nil || err != nil {
		t.Errorf("OpenContest on empty dir = %v, %v; want nil, nil", missing, err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
