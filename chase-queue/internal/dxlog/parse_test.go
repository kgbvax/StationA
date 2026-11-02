package dxlog

import (
	"os"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseContactInfo(t *testing.T) {
	k, q, err := Parse(fixture(t, "contactinfo.xml"))
	if err != nil || k != KindQSO {
		t.Fatalf("kind=%v err=%v", k, err)
	}
	if q.Call != "G3XYZ" || q.Band != "20m" || q.Mode != "SSB" || q.StationID != "OP1" || q.QSOID != "9001" {
		t.Fatalf("%+v", q)
	}
	if q.ContestName != "UKEIDXSSB" || q.Exch[2] != "NK" || q.Mults[0] != "NK" || q.Dupe || q.Invalid {
		t.Fatalf("%+v", q)
	}
	if q.At.Year() != 2026 || q.At.Hour() != 13 {
		t.Fatalf("time %v", q.At)
	}
}

func TestParseKinds(t *testing.T) {
	if k, _, err := Parse(fixture(t, "contactreplace.xml")); err != nil || k != KindReplace {
		t.Fatalf("replace: %v %v", k, err)
	}
	if k, _, err := Parse(fixture(t, "contactdelete.xml")); err != nil || k != KindDelete {
		t.Fatalf("delete: %v %v", k, err)
	}
	if k, _, _ := Parse(fixture(t, "radioinfo.xml")); k != KindIgnored {
		t.Fatalf("radioinfo: %v", k)
	}
	if k, _, _ := Parse([]byte("not xml")); k != KindIgnored {
		t.Fatalf("garbage: %v", k)
	}
}

func TestBandName(t *testing.T) {
	for in, want := range map[string]string{"14,0": "20m", "14.0": "20m", "7,0": "40m", "3,5": "80m", "28": "10m"} {
		if got := bandName(in); got != want {
			t.Errorf("bandName(%q)=%q want %q", in, got, want)
		}
	}
}
