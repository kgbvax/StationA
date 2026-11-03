package importer

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"station-log/internal/adif"
	"station-log/internal/store"
)

func newImporter(t *testing.T) (*Importer, *store.Store, *[]store.Event) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var evs []store.Event
	im := &Importer{Store: st, Window: 5 * time.Minute, DefaultOwn: "DL9ET", Sink: func(e []store.Event) { evs = append(evs, e...) }}
	return im, st, &evs
}

func imp(t *testing.T, im *Importer, text string, opt Options) Report {
	t.Helper()
	r, err := im.Import(context.Background(), text, opt)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func rec(call, tm, extra string) string {
	return "<CALL:" + itoa(len(call)) + ">" + call + "<QSO_DATE:8>20261101<TIME_ON:6>" + tm + "<BAND:3>20m<MODE:3>SSB<STATION_CALLSIGN:5>DL9ET" + extra + "<EOR>\n"
}

func itoa(n int) string { return string(rune('0'+n/10)) + string(rune('0'+n%10)) }

func TestImportFixtureAndIdempotent(t *testing.T) {
	im, st, evs := newImporter(t)
	b, err := os.ReadFile("testdata/log4om.adi")
	if err != nil {
		t.Fatal(err)
	}
	r := imp(t, im, string(b), Options{Source: "log4om-file"})
	if r.Records != 6 || r.Added != 4 || r.Merged != 0 || r.Unchanged != 0 || r.SkippedInvalid != 2 || len(r.Errors) != 2 {
		t.Fatalf("first import: %+v", r)
	}
	if len(*evs) != 4 || (*evs)[0].Action != "added" || (*evs)[0].ID != (*evs)[0].QSO.ID {
		t.Fatalf("events: %+v", *evs)
	}
	r = imp(t, im, string(b), Options{Source: "log4om-file"})
	if r.Added != 0 || r.Merged != 0 || r.Unchanged != 4 || r.SkippedInvalid != 2 {
		t.Fatalf("re-import: %+v", r)
	}
	if len(*evs) != 4 {
		t.Fatalf("a no-op import emitted events: %d", len(*evs))
	}
	if n, _ := st.Count(); n != 4 {
		t.Fatalf("count %d", n)
	}

	qs, _ := st.ByCall("K1ABC", "")
	if len(qs) != 1 {
		t.Fatal(qs)
	}
	q := qs[0]
	if q.OwnCall != "DL9ET" || q.Band != "20m" || q.Mode != "SSB" || q.ModeClass != "phone" || q.FreqHz != 14_205_000 || q.DXCC != 291 ||
		q.State != "MA" || q.CQZone != 5 || q.ITUZone != 8 || q.Grid != "FN42AB" || q.Confirmed || q.Source != "log4om-file" ||
		!q.At.Equal(time.Date(2026, 11, 1, 10, 15, 0, 0, time.UTC)) {
		t.Fatalf("%+v", q)
	}
	// trailing blank of the call is trimmed, QSL_RCVD=Y confirms, 4-digit TIME_ON
	qs, _ = st.ByCall("W1AW", "")
	if len(qs) != 1 || !qs[0].QSLRcvd || !qs[0].Confirmed || qs[0].ModeClass != "cw" || qs[0].At.Hour() != 12 {
		t.Fatalf("%+v", qs)
	}
	// OPERATOR fallback, band from FREQ, LoTW "V" counts, FT8 is data
	qs, _ = st.ByCall("JA1XYZ", "")
	if len(qs) != 1 || qs[0].OwnCall != "DL9ET" || qs[0].Band != "15m" || !qs[0].LoTWRcvd || qs[0].ModeClass != "data" || qs[0].Submode != "FT8" {
		t.Fatalf("%+v", qs)
	}
	qs, _ = st.ByCall("VK2ABC", "")
	if len(qs) != 1 || qs[0].SOTARef != "VK2/HU-001" || qs[0].WWFFRef != "VKFF-0001" {
		t.Fatalf("%+v", qs)
	}
}

func TestMergeLaterConfirmation(t *testing.T) {
	im, st, evs := newImporter(t)
	imp(t, im, rec("K1ABC", "101500", "<STATE:2>MA"), Options{Source: "a"})
	// the LoTW export arrives later: confirmation + SIG_INFO, no STATE, time 2 min off
	r := imp(t, im, rec("K1ABC", "101300", "<LOTW_QSL_RCVD:1>Y<SIG:4>POTA<SIG_INFO:6>K-1234<GRIDSQUARE:4>FN42"), Options{Source: "lotw"})
	if r.Added != 0 || r.Merged != 1 || r.Unchanged != 0 {
		t.Fatalf("%+v", r)
	}
	qs, _ := st.ByCall("K1ABC", "")
	q := qs[0]
	if len(qs) != 1 || !q.LoTWRcvd || !q.Confirmed || q.State != "MA" || q.SigInfo != "K-1234" || q.Sig != "POTA" || q.Grid != "FN42" ||
		q.Source != "a" || q.At.Minute() != 13 {
		t.Fatalf("merged: %+v", q)
	}
	if last := (*evs)[len(*evs)-1]; last.Action != "merged" || last.ID != q.ID || !last.QSO.Confirmed {
		t.Fatalf("event %+v", last)
	}
	// the old, unconfirmed export again: nothing changes, flags stay true, empty never overwrites
	r = imp(t, im, rec("K1ABC", "101500", ""), Options{Source: "a"})
	if r.Merged != 0 || r.Unchanged != 1 {
		t.Fatalf("%+v", r)
	}
	qs, _ = st.ByCall("K1ABC", "")
	if !qs[0].LoTWRcvd || qs[0].State != "MA" || qs[0].At.Minute() != 13 {
		t.Fatalf("%+v", qs[0])
	}
	// a corrected non-empty STATE overwrites
	imp(t, im, rec("K1ABC", "101500", "<STATE:2>NH"), Options{})
	qs, _ = st.ByCall("K1ABC", "")
	if qs[0].State != "NH" {
		t.Fatalf("%+v", qs[0])
	}
}

func TestWindowEdges(t *testing.T) {
	for _, c := range []struct {
		tm   string
		want string // "merged" or "added"
	}{
		{"101500", "merged"}, {"102000", "merged"}, {"101000", "merged"}, {"102001", "added"}, {"100959", "added"},
	} {
		im, st, _ := newImporter(t)
		imp(t, im, rec("K1ABC", "101500", ""), Options{})
		r := imp(t, im, rec("K1ABC", c.tm, "<STATE:2>MA"), Options{})
		got := "?"
		switch {
		case r.Added == 1:
			got = "added"
		case r.Merged == 1:
			got = "merged"
		}
		if got != c.want {
			t.Errorf("%s: %s, want %s (%+v)", c.tm, got, c.want, r)
		}
		n, _ := st.Count()
		if (c.want == "added") != (n == 2) {
			t.Errorf("%s: count %d", c.tm, n)
		}
	}
	// earliest time kept
	im, st, _ := newImporter(t)
	imp(t, im, rec("K1ABC", "101500", ""), Options{})
	imp(t, im, rec("K1ABC", "101200", ""), Options{})
	qs, _ := st.ByCall("K1ABC", "")
	if len(qs) != 1 || qs[0].At.Minute() != 12 {
		t.Fatalf("%+v", qs)
	}
}

func TestNotMerged(t *testing.T) {
	im, st, _ := newImporter(t)
	imp(t, im, rec("K1ABC", "101500", ""), Options{})
	// another own call, same QSO otherwise
	other := strings.Replace(rec("K1ABC", "101500", ""), "DL9ET", "DK0XYZ", 1)
	// another mode class
	cw := strings.Replace(rec("K1ABC", "101500", ""), "<MODE:3>SSB", "<MODE:2>CW", 1)
	// another band
	b40 := strings.Replace(rec("K1ABC", "101500", ""), "20m", "40m", 1)
	// another call
	r := imp(t, im, other+cw+b40+rec("K1ABD", "101500", ""), Options{})
	if r.Added != 4 || r.Merged != 0 {
		t.Fatalf("%+v", r)
	}
	if n, _ := st.Count(); n != 5 {
		t.Fatal(n)
	}
	// SSB, USB and LSB are one mode class
	r = imp(t, im, strings.Replace(rec("K1ABC", "101530", ""), "<MODE:3>SSB", "<MODE:3>USB", 1), Options{})
	if r.Added != 0 || r.Unchanged != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestReplaceSource(t *testing.T) {
	im, st, evs := newImporter(t)
	imp(t, im, rec("K1AAA", "100000", "")+rec("K1BBB", "110000", "")+rec("K1CCC", "120000", ""), Options{Source: "log4om"})
	imp(t, im, rec("K1DDD", "130000", ""), Options{Source: "other"})
	// a QSO first stored by "other", later also in log4om's file: stays other's
	imp(t, im, rec("K1AAA", "100100", "<STATE:2>MA"), Options{Source: "other"})

	// not requested: a plain import never deletes
	r := imp(t, im, rec("K1AAA", "100000", ""), Options{Source: "log4om"})
	if r.Deleted != 0 {
		t.Fatalf("%+v", r)
	}
	*evs = nil
	r = imp(t, im, rec("K1AAA", "100000", "")+rec("K1CCC", "120000", ""), Options{Source: "log4om", ReplaceSource: true})
	if r.Deleted != 1 || r.Added != 0 {
		t.Fatalf("%+v", r)
	}
	if len(*evs) != 1 || (*evs)[0].Action != "deleted" || (*evs)[0].QSO.Call != "K1BBB" {
		t.Fatalf("%+v", *evs)
	}
	if qs, _ := st.ByCall("K1BBB", ""); len(qs) != 0 {
		t.Fatal("K1BBB should be gone")
	}
	for _, c := range []string{"K1AAA", "K1CCC", "K1DDD"} {
		if qs, _ := st.ByCall(c, ""); len(qs) != 1 {
			t.Fatalf("%s: %v", c, qs)
		}
	}
	// a file without a valid record never wipes a source
	n0, _ := st.Count()
	if _, err := im.Import(context.Background(), "<EOH>garbage", Options{Source: "log4om", ReplaceSource: true}); err != ErrNoRecords {
		t.Fatalf("err %v", err)
	}
	if n, _ := st.Count(); n != n0 {
		t.Fatalf("count %d -> %d", n0, n)
	}
}

func TestLastImport(t *testing.T) {
	im, _, _ := newImporter(t)
	if _, ok := im.Last(); ok {
		t.Fatal("no import yet")
	}
	imp(t, im, rec("K1ABC", "101500", ""), Options{Source: "x"})
	l, ok := im.Last()
	if !ok || l.Source != "x" || l.Added != 1 || l.At.IsZero() {
		t.Fatalf("%+v", l)
	}
}

func TestFromRecord(t *testing.T) {
	for _, c := range []struct {
		name string
		r    adif.Record
		own  string
		err  string
		chk  func(store.QSO) bool
	}{
		{"freq band", adif.Record{"CALL": "dl1abc", "MODE": "ft8", "FREQ": "50.313", "QSO_DATE": "20261101", "TIME_ON": "1010"}, "dl9et", "",
			func(q store.QSO) bool {
				return q.Band == "6m" && q.Call == "DL1ABC" && q.OwnCall == "DL9ET" && q.ModeClass == "data"
			}},
		{"unknown BAND falls back to FREQ", adif.Record{"CALL": "A", "MODE": "CW", "BAND": "99m", "FREQ": "7.01", "QSO_DATE": "20261101", "TIME_ON": "101000"}, "X", "",
			func(q store.QSO) bool { return q.Band == "40m" }},
		{"no own", adif.Record{"CALL": "A", "MODE": "CW", "BAND": "20m", "QSO_DATE": "20261101", "TIME_ON": "101000"}, "", "no STATION_CALLSIGN", nil},
		{"no mode", adif.Record{"CALL": "A", "BAND": "20m", "QSO_DATE": "20261101", "TIME_ON": "101000"}, "X", "no MODE", nil},
		{"no band", adif.Record{"CALL": "A", "MODE": "CW", "QSO_DATE": "20261101", "TIME_ON": "101000"}, "X", "no BAND", nil},
		{"bad time", adif.Record{"CALL": "A", "MODE": "CW", "BAND": "20m", "QSO_DATE": "2026", "TIME_ON": "101000"}, "X", "QSO_DATE", nil},
		{"no time", adif.Record{"CALL": "A", "MODE": "CW", "BAND": "20m", "QSO_DATE": "20261101"}, "X", "QSO_DATE", nil},
	} {
		q, err := FromRecord(c.r, c.own, "s")
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err %v, want %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil || !c.chk(q) {
			t.Errorf("%s: %v %+v", c.name, err, q)
		}
	}
}

func TestModeClass(t *testing.T) {
	for mode, want := range map[string]string{"CW": "cw", "ssb": "phone", "USB": "phone", "FM": "phone", "AM": "phone", "RTTY": "data",
		"FT8": "data", "MFSK": "data", "PSK31": "data", "DIGITALVOICE": "phone", "": ""} {
		if got := ModeClass(mode, ""); got != want {
			t.Errorf("%q: %q, want %q", mode, got, want)
		}
	}
	if ModeClass("SSB-ISH", "LSB") != "phone" {
		t.Error("submode LSB")
	}
}

func TestReplaceSourceGuards(t *testing.T) {
	im, st, _ := newImporter(t)
	imp(t, im, rec("K1AAA", "100000", "")+rec("K1BBB", "110000", "")+rec("K1CCC", "120000", ""), Options{Source: "log4om"})
	n0, _ := st.Count()
	ctx := context.Background()
	// a cut-off file, an invalid record, and a delete of most of the source are refused
	cut := rec("K1AAA", "100000", "") + "<CALL:9>K1"
	if _, err := im.Import(ctx, cut, Options{Source: "log4om", ReplaceSource: true}); err != ErrIncomplete {
		t.Fatalf("err %v", err)
	}
	bad := rec("K1AAA", "100000", "") + "<CALL:5>K1DDD<EOR>"
	if _, err := im.Import(ctx, bad, Options{Source: "log4om", ReplaceSource: true}); err != ErrIncomplete {
		t.Fatalf("err %v", err)
	}
	one := rec("K1AAA", "100000", "")
	if _, err := im.Import(ctx, one, Options{Source: "log4om", ReplaceSource: true}); err != ErrMassDelete {
		t.Fatalf("err %v", err)
	}
	if n, _ := st.Count(); n != n0 {
		t.Fatalf("count %d -> %d", n0, n)
	}
	// a QSO another source also contributed to survives the replace
	imp(t, im, rec("K1BBB", "110000", "<LOTW_QSL_RCVD:1>Y"), Options{Source: "lotw"})
	r, err := im.Import(ctx, one, Options{Source: "log4om", ReplaceSource: true, Force: true})
	if err != nil || r.Deleted != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if qs, _ := st.ByCall("K1BBB", ""); len(qs) != 1 || !qs[0].LoTWRcvd {
		t.Fatalf("K1BBB: %v", qs)
	}
	if qs, _ := st.ByCall("K1CCC", ""); len(qs) != 0 {
		t.Fatal("K1CCC should be gone")
	}
}
