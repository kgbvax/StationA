package cty

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mini(t *testing.T) *DB {
	t.Helper()
	db, err := LoadFile(filepath.Join("testdata", "mini-cty.dat"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestLookup(t *testing.T) {
	db := mini(t)
	cases := []struct {
		call string
		name string // "" = no country
		itu  int    // 0 = not checked
		cq   int
		cont string
	}{
		{"OR4TN", "Antarctica", 74, 13, "SA"},
		{"MR6TMS", "Scotland", 27, 14, "EU"},
		{"LW7DQQ/Y", "Argentina", 16, 13, "SA"}, // ITU override [16]
		{"LW7DQQ", "Argentina", 14, 13, "SA"},   // not the exact entry: defaults
		{"UI9XA", "European Russia", 29, 16, "EU"},
		{"UA9ABC", "Asiatic Russia", 30, 17, "AS"}, // longer prefix wins over UA
		{"UA3ABC", "European Russia", 29, 16, "EU"},
		{"UA1ZZZ", "European Russia", 29, 17, "EU"}, // CQ override (17)
		{"ZS85SARL", "South Africa", 57, 38, "AF"},
		{"ZS1ABC", "South Africa", 57, 38, "AF"},
		{"DL1ABC", "Fed. Rep. of Germany", 28, 14, "EU"},
		{"dl1abc", "Fed. Rep. of Germany", 0, 0, ""}, // case-insensitive
		{" DL1ABC ", "Fed. Rep. of Germany", 0, 0, ""},
		{"DL1ABC/P", "Fed. Rep. of Germany", 0, 0, ""},
		{"DL1ABC/M", "Fed. Rep. of Germany", 0, 0, ""},
		{"DL1ABC/QRP", "Fed. Rep. of Germany", 0, 0, ""},
		{"DL1ABC/A", "Fed. Rep. of Germany", 0, 0, ""},
		{"DL1ABC/P/QRP", "Fed. Rep. of Germany", 0, 0, ""},
		{"DL1ABC-7", "Fed. Rep. of Germany", 0, 0, ""},
		{"DL1ABC-#", "Fed. Rep. of Germany", 0, 0, ""},
		{"OE/DL1ABC", "Austria", 28, 15, "EU"},
		{"OE/DL1ABC/P", "Austria", 0, 0, ""},
		{"DL1ABC/OE", "Austria", 0, 0, ""},
		{"OE3ABC/DL", "Fed. Rep. of Germany", 0, 0, ""},
		{"W1ABC/4", "United States", 8, 5, "NA"}, // call-area digit dropped
		{"KH6ABC", "United States", 61, 31, "NA"},
		{"DL1ABC/XX9", "Fed. Rep. of Germany", 0, 0, ""}, // unknown prefix part: the call decides
		{"DL1ABC/MM", "", 0, 0, ""},
		{"DL1ABC/AM", "", 0, 0, ""},
		{"HELLO", "", 0, 0, ""}, // no digit, no callsign
		{"", "", 0, 0, ""},
		{"QQ1ABC", "", 0, 0, ""},
		{"1", "", 0, 0, ""},
		// exact-only entries never match a truncated or extended string
		{"OR4T", "", 0, 0, ""},
		{"OR4TNX", "", 0, 0, ""},
		{"MR6TMSX", "", 0, 0, ""},
		{"MR6TM", "", 0, 0, ""},
		{"ZS85SAR", "South Africa", 0, 0, ""}, // but ZS is a real prefix
	}
	for _, c := range cases {
		e, ok := db.Lookup(c.call)
		if (c.name != "") != ok {
			t.Errorf("Lookup(%q) ok = %v, want %v (entity %q)", c.call, ok, !ok, e.Name)
			continue
		}
		if !ok {
			if e != (Entity{}) {
				t.Errorf("Lookup(%q) = %+v on failure, want zero", c.call, e)
			}
			if db.Valid(c.call) {
				t.Errorf("Valid(%q) = true, want false", c.call)
			}
			continue
		}
		if e.Name != c.name {
			t.Errorf("Lookup(%q) = %q, want %q", c.call, e.Name, c.name)
		}
		if c.itu != 0 && e.ITUZone != c.itu {
			t.Errorf("Lookup(%q) ITU = %d, want %d", c.call, e.ITUZone, c.itu)
		}
		if c.cq != 0 && e.CQZone != c.cq {
			t.Errorf("Lookup(%q) CQ = %d, want %d", c.call, e.CQZone, c.cq)
		}
		if c.cont != "" && e.Continent != c.cont {
			t.Errorf("Lookup(%q) continent = %q, want %q", c.call, e.Continent, c.cont)
		}
		if !db.Valid(c.call) {
			t.Errorf("Valid(%q) = false, want true", c.call)
		}
	}
}

func TestPrimaryPrefix(t *testing.T) {
	e, ok := mini(t).Lookup("DL1ABC")
	if !ok || e.Primary != "DL" {
		t.Errorf("DL1ABC: %+v %v", e, ok)
	}
}

func TestNilDB(t *testing.T) {
	var db *DB
	if !db.Valid("ANYTHING") || !db.Valid("") {
		t.Error("nil DB must accept everything")
	}
	if e, ok := db.Lookup("DL1ABC"); ok || e != (Entity{}) {
		t.Errorf("nil DB Lookup = %+v, %v", e, ok)
	}
}

func TestOpenMissing(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{dir, filepath.Join(dir, "does-not-exist")} {
		db, err := Open(d)
		if db != nil || err != nil {
			t.Errorf("Open(%q) = %v, %v; want nil, nil", d, db, err)
		}
	}
	if _, err := LoadFile(filepath.Join(dir, "cty.dat")); err == nil {
		t.Error("LoadFile of a missing file must fail")
	}
}

func TestOpenFile(t *testing.T) {
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join("testdata", "mini-cty.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cty.dat"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil || db == nil {
		t.Fatalf("Open = %v, %v", db, err)
	}
	if !db.Valid("DL1ABC") || db.Valid("QQ1ABC") {
		t.Error("loaded DB does not judge")
	}
}

func TestOpenCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cty.dat"), []byte("this is not cty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(dir); err == nil || db != nil {
		t.Errorf("Open(corrupt) = %v, %v; want an error", db, err)
	}
}

func parse(t *testing.T, s string) *DB {
	t.Helper()
	db, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestFirstOccurrenceWins(t *testing.T) {
	db := parse(t, `First:  1: 2: EU: 0: 0: 0: AA:
    AA,BB,=XX1X;
Second: 3: 4: AS: 0: 0: 0: BB:
    BB,CC,=XX1X;
`)
	for call, want := range map[string]string{"AA1X": "First", "BB1X": "First", "CC1X": "Second", "XX1X": "First"} {
		if e, ok := db.Lookup(call); !ok || e.Name != want {
			t.Errorf("Lookup(%q) = %q, %v; want %q", call, e.Name, ok, want)
		}
	}
}

func TestParseEdgeCases(t *testing.T) {
	// CRLF, BOM, WAE entity, multi-line aliases, all override markers
	src := "\ufeffSicily:  15:  28:  EU:  37.50:  -14.00:  -1.0:  *IT9:\r\n" +
		"    IB9,ID9,\r\n" +
		"    IT9(14)[27]{AF}<12.5/-3.5>~-2.0~,\r\n" +
		"    =IT9ABC[29],=IT9XYZ{AS}(20)\r\n" +
		"    ,IZ9;\r\n" +
		"\r\n" +
		"Nowhere: 1: 1: NA: 0: 0: 0: NW:\r\n" +
		"    NW;\r\n"
	db := parse(t, src)
	cases := []struct {
		call      string
		cq, itu   int
		continent string
	}{
		{"IB9ABC", 15, 28, "EU"},
		{"IT9QRS", 14, 27, "AF"}, // overrides on a prefix alias; lat/long and UTC ignored
		{"IT9ABC", 15, 29, "EU"},
		{"IT9XYZ", 20, 28, "AS"},
		{"IZ9AAA", 15, 28, "EU"}, // alias after a line break inside the list
	}
	for _, c := range cases {
		e, ok := db.Lookup(c.call)
		if !ok || e.CQZone != c.cq || e.ITUZone != c.itu || e.Continent != c.continent {
			t.Errorf("Lookup(%q) = %+v, %v; want CQ %d ITU %d %s", c.call, e, ok, c.cq, c.itu, c.continent)
		}
		if ok && (e.Name != "Sicily" || e.Primary != "IT9") {
			t.Errorf("Lookup(%q): name %q primary %q (WAE '*' must be stripped)", c.call, e.Name, e.Primary)
		}
	}
	if e, ok := db.Lookup("NW1A"); !ok || e.Name != "Nowhere" {
		t.Errorf("entity after blank line: %+v %v", e, ok)
	}
}

func TestParseMissingTerminator(t *testing.T) {
	// the last entity without its closing ';' still counts
	db := parse(t, "Solo: 1: 2: EU: 0: 0: 0: SO:\n    SO,=SO1X\n")
	if !db.Valid("SO1AB") || !db.Valid("SO1X") {
		t.Error("unterminated final entity lost")
	}
}

func TestParseErrors(t *testing.T) {
	for name, src := range map[string]string{
		"empty":           "",
		"alias first":     "    AA,BB;\n",
		"short header":    "Bad: 1: 2: EU:\n",
		"bad zone":        "Bad: x: 2: EU: 0: 0: 0: AA:\n    AA;\n",
		"only whitespace": "\n  \n",
	} {
		if db, err := Parse(strings.NewReader(src)); err == nil {
			t.Errorf("%s: Parse = %v, nil; want an error", name, db)
		}
	}
}
