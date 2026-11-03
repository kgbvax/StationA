package award

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func TestImportQSOFieldsAward(t *testing.T) {
	f, err := os.Open("testdata/award_WAS.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	def, notes, err := ImportXML(f)
	if err != nil {
		t.Fatal(err)
	}
	if def.Code != "WAS" || def.Kind != KindField || def.Field != "STATE" || def.Rule != RuleConfirmed ||
		fmt.Sprint(def.DXCC) != "[6 291 110]" || fmt.Sprint(def.Items) != "[AL AK CA NY]" || len(def.Configs) != 3 {
		t.Fatalf("%+v", def)
	}
	c := def.Configs
	if c[0].Name != "20-CW" || fmt.Sprint(c[0].Bands, c[0].Modes) != "[20m] [cw]" ||
		c[1].Name != "10-FT8" || fmt.Sprint(c[1].Modes) != "[data]" || c[2].Name != "Mixed-SSB" || fmt.Sprint(c[2].Modes) != "[phone]" {
		t.Fatalf("%+v", c)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "10-FT8") || !strings.Contains(notes[0], "continent") {
		t.Fatalf("%v", notes)
	}
	// the output is a definition file the loader accepts, with the configs expanded
	out, err := EncodeTOML(def)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write(t, dir, "was.toml", string(out))
	cat, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := strings.Join(cat.Codes(), " "); got != "dxcc WAS WAS-20-CW WAS-10-FT8 WAS-Mixed-SSB" {
		t.Fatal(got)
	}
	if d, _ := cat.Get("was-10-ft8"); fmt.Sprint(d.Modes, d.Bands, d.Items) != "[data] [10m] [AL AK CA NY]" {
		t.Fatalf("%+v", d)
	}
}

func TestImportReferenceAwardIsDeferredWithoutReadingItems(t *testing.T) {
	head, err := os.ReadFile("testdata/award_SOTA_head.xml")
	if err != nil {
		t.Fatal(err)
	}
	// 200 000 items stream past the importer, which keeps none of them
	items := io.MultiReader(bytes.NewReader(head), &repeat{n: 200_000}, strings.NewReader("</Items></AwardDefinition>"))
	_, _, err = ImportXML(items)
	if !errors.Is(err, ErrDeferred) || !strings.Contains(err.Error(), "SOTA") {
		t.Fatalf("%v", err)
	}
}

type repeat struct {
	n   int
	buf []byte
}

func (r *repeat) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		if r.n == 0 {
			return 0, io.EOF
		}
		r.n--
		r.buf = []byte(fmt.Sprintf("<Item><Code>W6/CT-%06d</Code><Name>Summit %d</Name></Item>", r.n, r.n))
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func TestImportRejects(t *testing.T) {
	for name, tc := range map[string]struct{ xml, want string }{
		"empty":    {"", "empty document"},
		"notaward": {"<a><b>1</b></a>", "no AwardType"},
		"field":    {"<A><AwardName>X</AwardName><AwardType>QSOFIELDS</AwardType><QsoField>Grid</QsoField></A>", "only STATE"},
		"type":     {"<A><AwardName>X</AwardName><AwardType>Foo</AwardType></A>", "unknown AwardType"},
		"dxcc":     {"<A><AwardName>X</AwardName><AwardType>QSOFIELDS</AwardType><QsoField>State</QsoField><AllowedDxcc>US</AllowedDxcc></A>", "ADIF entity number"},
		"noname":   {"<A><AwardType>QSOFIELDS</AwardType></A>", "no code or name"},
		"toolong": {"<A><AwardName>X</AwardName><AwardType>QSOFIELDS</AwardType><QsoField>State</QsoField><Items>" +
			strings.Repeat("<Item>AB</Item>", maxKids+1) + "</Items></A>", "too long"},
	} {
		if _, _, err := ImportXML(strings.NewReader(tc.xml)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestImportWorkedWhenNoConfirmation(t *testing.T) {
	x := "<A><AwardName>X</AwardName><AwardType>QSOFIELDS</AwardType><QsoField>State</QsoField><ConfirmationMethod>NONE</ConfirmationMethod><AllowedDxcc>291, 6</AllowedDxcc></A>"
	d, _, err := ImportXML(strings.NewReader(x))
	if err != nil || d.Rule != RuleWorked || fmt.Sprint(d.DXCC) != "[291 6]" {
		t.Fatalf("%+v %v", d, err)
	}
}
