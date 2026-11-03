package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportAwardXML(t *testing.T) {
	var out, errb bytes.Buffer
	if code := importAwardXML("../../internal/award/testdata/award_WAS.xml", &out, &errb); code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"# imported from award_WAS.xml", "# note: config 10-FT8", "[[award]]", `code = "WAS"`, `kind = "field"`, "[[award.config]]", `name = "20-CW"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

func TestImportAwardXMLReferenceIsDeferred(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sota.xml")
	os.WriteFile(p, []byte("<A><AwardName>SOTA</AwardName><AwardType>REFERENCE</AwardType></A>"), 0o600)
	var out, errb bytes.Buffer
	if code := importAwardXML(p, &out, &errb); code != 3 || out.Len() != 0 || !strings.Contains(errb.String(), "deferred") {
		t.Fatalf("%d %q %q", code, out.String(), errb.String())
	}
	if code := importAwardXML(filepath.Join(t.TempDir(), "none.xml"), &out, &errb); code != 1 {
		t.Fatal(code)
	}
}
