package inventory

import (
	"strings"
	"testing"
)

func TestBuiltInInventoryIsValid(t *testing.T) {
	inv, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Links) == 0 || len(inv.Slots) == 0 || len(inv.Software) == 0 || len(inv.Hosts) == 0 {
		t.Fatalf("inventory sections missing: %d links %d slots %d software %d hosts",
			len(inv.Links), len(inv.Slots), len(inv.Software), len(inv.Hosts))
	}
	// Every component named by a slot must appear in the software list.
	sw := map[string]bool{}
	for _, s := range inv.Software {
		sw[s.Name] = true
	}
	for _, s := range inv.Slots {
		if !sw[s.Component] {
			t.Errorf("slot %s: component %q missing from [[software]]", s.Address, s.Component)
		}
	}
}

func TestStrictDecodeRejectsMisplacedKey(t *testing.T) {
	_, err := Parse([]byte("[[link]]\nname = \"x\"\nurl = \"http://h/\"\nlink = \"oops\"\n"))
	if err == nil {
		t.Fatal("unknown key in [[link]] must fail")
	}
}

func TestValidateRejectsDanglingSoftwareLinkAndDuplicateSlot(t *testing.T) {
	_, err := Parse([]byte(`
[[slot]]
address = "muehle/hf/a"
[[slot]]
address = "muehle/hf/a"
[[software]]
name = "s"
link = "nope"
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("got %v", err)
	}
}

func TestProbeTarget(t *testing.T) {
	cases := []struct {
		url, probe, kind, target string
		bad                      bool
	}{
		{"http://h:8080/x", "", "http", "http://h:8080/x", false},
		{"https://h/", "", "http", "https://h/", false},
		{"mqtt://h:1883", "", "tcp", "h:1883", false},
		{"ws://h", "", "tcp", "h:80", false},
		{"ws://h:60001", "", "tcp", "h:60001", false},
		{"tcp://h", "", "", "", true},
		{"ftp://h/", "", "", "", true},
		{"ftp://h/", "none", "none", "", false},
		{"not a url", "", "", "", true},
	}
	for _, c := range cases {
		k, tg, err := ProbeTarget(Link{URL: c.url, Probe: c.probe})
		if (err != nil) != c.bad || k != c.kind || tg != c.target {
			t.Errorf("%s/%s: got %q %q %v", c.url, c.probe, k, tg, err)
		}
	}
}
