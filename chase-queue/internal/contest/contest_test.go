package contest

import (
	"os"
	"strings"
	"testing"

	"chase-queue/internal/cty"
)

func load(t *testing.T, defFile string, withPrefill bool) *Model {
	t.Helper()
	f, err := os.Open("testdata/" + defFile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	def, err := ParseDefinition(f)
	if err != nil {
		t.Fatal(err)
	}
	db, err := cty.LoadFile("testdata/cty.dat")
	if err != nil {
		t.Fatal(err)
	}
	m := &Model{Def: def, CTY: db}
	if withPrefill {
		pf, err := os.Open("testdata/prefill.txt")
		if err != nil {
			t.Fatal(err)
		}
		defer pf.Close()
		col := 1
		if len(def.DBFiles) > 0 {
			col = def.DBFiles[0].RCVDCol
		}
		if m.Prefill, err = ParsePrefill(pf, col); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func qso(call, band, mode string, exch ...string) QSOIn {
	q := QSOIn{Call: call, Band: band, Mode: mode}
	copy(q.Exch[:], exch)
	return q
}

func TestParseDefinition(t *testing.T) {
	m := load(t, "ukei-like.txt", false)
	d := m.Def
	if d.Name != "UKEIDXSSB" || d.DoubleQSO != "PER_BAND" || len(d.Slots) != 1 {
		t.Fatalf("definition = %+v", d)
	}
	if d.List["NR"] != "NK" || d.List["NK"] != "NK" || d.List["NL"] != "NL" {
		t.Errorf("list aliases = %v", d.List)
	}
	if len(d.DBFiles) != 1 || d.DBFiles[0].Name != "Ukei.txt" || d.DBFiles[0].RCVDCol != 1 {
		t.Errorf("db files = %+v", d.DBFiles)
	}
	if len(d.Slots[0].Exceptions) != 1 || d.CountsWAE {
		t.Errorf("slot = %+v wae=%v", d.Slots[0], d.CountsWAE)
	}
	if !load(t, "cqww-like.txt", false).Def.CountsWAE {
		t.Error("CQ-WW-SSB should count the WAE list")
	}
}

func TestParseDefinitionWithoutSlots(t *testing.T) {
	d, err := ParseDefinition(strings.NewReader("CONTESTNAME=X\nBANDS=20\n"))
	if err != nil || len(d.Slots) != 0 || len(d.Notes) == 0 {
		t.Fatalf("d=%+v err=%v", d, err)
	}
	if _, err := ParseDefinition(strings.NewReader("no keys here\n")); err == nil {
		t.Error("a file without KEY=VALUE lines should be an error")
	}
}

// AE2: UK/EI stations count their district from the prefill, others their DXCC entity.
func TestValueUKEIDistricts(t *testing.T) {
	m := load(t, "ukei-like.txt", true)
	h := m.HeldFrom([]QSOIn{
		qso("G4AAA", "40m", "SSB", "59", "001", "NN"),
		qso("G5CCC", "40m", "SSB", "59", "002", "NL"),
	})
	if v := m.Value("G3XYZ", "40m", h); v.Class != ClassNew || v.Slots[0].Name != "NK" || v.NewDXCC != nil {
		t.Errorf("G3XYZ (prefill NK) = %+v", v)
	}
	if v := m.Value("G4AAA", "40m", h); v.Class != ClassNone || v.Slots[0].State != StateHeld {
		t.Errorf("G4AAA (NN held on 40m) = %+v", v)
	}
	if v := m.Value("G4AAA", "20m", h); v.Class != ClassNew {
		t.Errorf("NN is new on 20m: %+v", v)
	}
	if v := m.Value("M0ABC", "40m", h); v.Class != ClassUnknown || v.Slots[0].State != StateUnknown {
		t.Errorf("M0ABC without prefill = %+v, want unknown", v)
	}
	if v := m.Value("G6DDD", "40m", h); v.Class != ClassUnknown {
		t.Errorf("a prefill value not on the list is unknown, got %+v", v)
	}
	v := m.Value("EA8ABC", "40m", h)
	if v.Class != ClassNew || v.Slots[0].Name != "EA8" || v.NewDXCC == nil || v.NewDXCC.Primary != "EA8" {
		t.Errorf("EA8ABC is valued by DXCC, got %+v", v)
	}
}

func TestAliasCountsAsHeld(t *testing.T) {
	m := load(t, "ukei-like.txt", true)
	h := m.HeldFrom([]QSOIn{qso("G3XYZ", "20m", "SSB", "59", "001", "NR")})
	if v := m.Value("G3XYZ", "20m", h); v.Class != ClassNone {
		t.Errorf("NR logged, prefill says NK (same multiplier): %+v", v)
	}
}

// AE7: a slot bmrec cannot evaluate is unknown for every call; the other slot works.
func TestUnsupportedSlotIsUnknown(t *testing.T) {
	m := load(t, "fx-slot.txt", false)
	if got := m.Def.UnsupportedSlots(); len(got) != 1 || !strings.HasPrefix(got[0], "slot 2") {
		t.Errorf("unsupported = %v", got)
	}
	v := m.Value("DL1ABC", "20m", m.HeldFrom(nil))
	if v.Slots[0].State != StateNew || v.Slots[1].State != StateUnknown || v.Class != ClassNew {
		t.Errorf("value = %+v", v)
	}
	held := m.HeldFrom([]QSOIn{qso("DL2XYZ", "20m", "SSB")})
	if v := m.Value("DL1ABC", "20m", held); v.Class != ClassUnknown {
		t.Errorf("DXCC held, FX unknown: want unknown, got %+v", v)
	}
}

func TestTwoSlotsNew(t *testing.T) {
	m := load(t, "cqww-like.txt", false)
	h := m.HeldFrom([]QSOIn{qso("DL1ABC", "20m", "SSB", "59", "14")})
	v := m.Value("W1AW", "20m", h)
	if v.NewSlots != 2 || v.Class != ClassNew {
		t.Errorf("W1AW new entity and new zone: %+v", v)
	}
	if v := m.Value("DL2XYZ", "20m", h); v.Class != ClassNone {
		t.Errorf("same entity and zone held: %+v", v)
	}
	if v := m.Value("DL2XYZ", "15m", h); v.NewSlots != 2 {
		t.Errorf("nothing held on 15m: %+v", v)
	}
}

func TestHeldSkipsInvalidAndNonSSB(t *testing.T) {
	m := load(t, "dxcc-only.txt", false)
	h := m.HeldFrom([]QSOIn{
		{Call: "DL1ABC", Band: "20m", Mode: "SSB", Invalid: true},
		{Call: "OE1ABC", Band: "20m", Mode: "CW"},
	})
	if h.Size() != 0 {
		t.Errorf("held %d names from invalid and CW QSOs", h.Size())
	}
}

func TestWAEFoldedOnlyWhereTheContestCountsIt(t *testing.T) {
	dxccOnly := load(t, "dxcc-only.txt", false)
	h := dxccOnly.HeldFrom([]QSOIn{qso("I1ABC", "20m", "SSB")})
	if v := dxccOnly.Value("IT9ABC", "20m", h); v.Class != ClassNone {
		t.Errorf("IT9ABC is Italy in a DXCC-only contest: %+v", v)
	}
	cqww := load(t, "cqww-like.txt", false)
	h = cqww.HeldFrom([]QSOIn{qso("I1ABC", "20m", "SSB", "59", "15")})
	if v := cqww.Value("IT9ABC", "20m", h); v.Slots[0].State != StateNew || v.Slots[0].Name != "IT9" {
		t.Errorf("IT9ABC is a multiplier of its own in CQ WW: %+v", v)
	}
}

func TestWorked(t *testing.T) {
	m := load(t, "ukei-like.txt", false)
	w := m.NewWorked([]QSOIn{
		qso("EA8ABC", "20m", "SSB"),
		qso("DL1ABC/P", "20m", "SSB"),
		{Call: "OE1ABC", Band: "20m", Mode: "SSB", Invalid: true},
	})
	cases := []struct {
		call, band string
		want       bool
	}{
		{"EA8ABC", "20m", true},
		{"EA8ABC", "40m", false},
		{"DL1ABC", "20m", true},      // logged as DL1ABC/P
		{"EA8/DL1ABC", "20m", false}, // a prefix form is another call
		{"OE1ABC", "20m", false},     // invalid QSO
	}
	for _, c := range cases {
		if got := w.Has(c.call, c.band); got != c.want {
			t.Errorf("Has(%q,%q) = %v, want %v", c.call, c.band, got, c.want)
		}
	}
	m.Def.DoubleQSO = "PER_MODE"
	w = m.NewWorked([]QSOIn{qso("EA8ABC", "20m", "SSB"), qso("OE1ABC", "20m", "CW")})
	if !w.Has("EA8ABC", "40m") || w.Has("OE1ABC", "20m") {
		t.Error("PER_MODE: an SSB QSO is worked on every band, a CW QSO is not worked for SSB")
	}
}

func TestNormalizeCall(t *testing.T) {
	for in, want := range map[string]string{
		"dl1abc": "DL1ABC", "DL1ABC/P": "DL1ABC", "DL1ABC/QRP": "DL1ABC", "EA8/DL1ABC/P": "EA8/DL1ABC",
		"DL1ABC-7": "DL1ABC", "EA8/DL1ABC": "EA8/DL1ABC", "W1ABC/4": "W1ABC/4",
	} {
		if got := NormalizeCall(in); got != want {
			t.Errorf("NormalizeCall(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrefill(t *testing.T) {
	p, err := ParsePrefill(strings.NewReader("# c\n!!Order!!,Call\nDL1ABC,NK,x\nEA8ABC=NN\nSHORT\nG1AA,\n"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := p.Lookup("dl1abc/p"); !ok || v != "NK" {
		t.Errorf("DL1ABC = %q %v", v, ok)
	}
	if _, ok := p.Lookup("G1AA"); ok {
		t.Error("an empty column is no value")
	}
	if p.Len() != 2 {
		t.Errorf("len = %d, want 2 (comment, order line, short and empty lines skipped)", p.Len())
	}
	if (*Prefill)(nil).Len() != 0 {
		t.Error("nil prefill has entries")
	}
}

func TestNoPrefillIsUnknownNotWrong(t *testing.T) {
	m := load(t, "ukei-like.txt", false)
	if v := m.Value("G3XYZ", "20m", m.HeldFrom(nil)); v.Class != ClassUnknown {
		t.Errorf("no prefill: %+v", v)
	}
	m.CTY = nil
	if v := m.Value("DL1ABC", "20m", m.HeldFrom(nil)); v.Class != ClassUnknown {
		t.Errorf("no cty with an exception slot: %+v", v)
	}
}
