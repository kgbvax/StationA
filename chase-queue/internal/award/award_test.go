package award

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirMissingGivesBuiltinDXCC(t *testing.T) {
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "nope")} {
		c, err := LoadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		d, ok := c.Get("DXCC")
		if !ok || c.Len() != 1 || d.Kind != KindDXCC || d.Rule != RuleConfirmed || !d.perBand() {
			t.Fatalf("%+v", d)
		}
	}
}

func TestLoadDirExpandsConfigs(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "was.toml", `
[[award]]
code = "WAS"
name = "Worked All States"
kind = "field"
field = "State"
dxcc = [6, 291, 110]
items = ["al", "ak", "ca"]

  [[award.config]]
  name = "20-CW"
  bands = ["20m"]
  modes = ["cw"]

  [[award.config]]
  name = "Mixed"
`)
	write(t, dir, "notes.txt", "not toml")
	c, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Codes(), " "); got != "dxcc WAS WAS-20-CW WAS-Mixed" {
		t.Fatal(got)
	}
	d, _ := c.Get("was-20-cw")
	if d.Field != "STATE" || d.Items[0] != "AL" || len(d.Bands) != 1 || d.Modes[0] != "cw" || d.Rule != RuleConfirmed || d.Configs != nil {
		t.Fatalf("%+v", d)
	}
	if m, _ := c.Get("WAS-Mixed"); len(m.Bands) != 0 || len(m.Items) != 3 {
		t.Fatalf("%+v", m)
	}
}

func TestLoadDirRejects(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"reference":  {"[[award]]\ncode='SOTA'\nkind='reference'\n", "deferred"},
		"contest":    {"[[award]]\ncode='X'\nkind='contest'\n", "built in"},
		"nokind":     {"[[award]]\ncode='X'\n", "kind is required"},
		"badfield":   {"[[award]]\ncode='X'\nkind='field'\nfield='GRID'\n", "only STATE"},
		"badrule":    {"[[award]]\ncode='X'\nkind='dxcc'\nrule='maybe'\n", "worked or confirmed"},
		"badmode":    {"[[award]]\ncode='X'\nkind='dxcc'\nmodes=['ssb']\n", "cw, phone or data"},
		"badband":    {"[[award]]\ncode='X'\nkind='dxcc'\nbands=['twenty']\n", "20m"},
		"reserved":   {"[[award]]\ncode='Contest'\nkind='dxcc'\n", "reserved"},
		"dup":        {"[[award]]\ncode='X'\nkind='dxcc'\n[[award]]\ncode='x'\nkind='dxcc'\n", "twice"},
		"unknownkey": {"[[award]]\ncode='X'\nkind='dxcc'\nbogus=1\n", "unknown key"},
		"dxccfields": {"[[award]]\ncode='X'\nkind='dxcc'\nitems=['A']\n", "belong to kind field"},
	} {
		dir := t.TempDir()
		write(t, dir, "a.toml", tc.body)
		if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestFileMayReplaceBuiltinDXCCOnce(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.toml", "[[award]]\ncode='DXCC'\nkind='dxcc'\nrule='worked'\nper_band=false\n")
	c, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := c.Get("dxcc"); d.Rule != RuleWorked || d.perBand() || c.Len() != 1 || len(c.Overrode) != 1 {
		t.Fatalf("%+v", d)
	}
	write(t, dir, "b.toml", "[[award]]\ncode='dxcc'\nkind='dxcc'\n")
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("a second dxcc is a duplicate")
	}
}

func TestAddConfigNamesAlreadyPrefixed(t *testing.T) {
	c := NewCatalog()
	d := Definition{Code: "WAS", Kind: KindField, Rule: RuleConfirmed, Field: "STATE", Items: []string{"NY"},
		Configs: []Config{{Name: "WAS"}, {Name: "WAS-10-CW", Bands: []string{"10m"}, Modes: []string{"cw"}}, {Name: "Mixed"}}}
	if err := c.Add(d); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"WAS", "WAS-10-CW", "WAS-Mixed"} {
		if _, ok := c.Get(code); !ok {
			t.Errorf("%s missing", code)
		}
	}
	if _, ok := c.Get("WAS-WAS"); ok {
		t.Error("WAS-WAS must not exist")
	}
	if _, ok := c.Get("WAS-WAS-10-CW"); ok {
		t.Error("WAS-WAS-10-CW must not exist")
	}
}
