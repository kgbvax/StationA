package config

import "testing"

func TestLegacySingleClusterTable(t *testing.T) {
	cfg, err := Load(write(t, "[cluster]\naddress = \"192.168.1.197:23\"\ncall = \"DL9ET\"\nskimmer = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Cluster) != 1 {
		t.Fatalf("%+v", cfg.Cluster)
	}
	if c := cfg.Cluster[0]; c.Name != "cluster" || c.Address != "192.168.1.197:23" || c.Call != "DL9ET" || !c.Skimmer || !c.IsEnabled() {
		t.Fatalf("%+v", c)
	}
	if len(cfg.Cluster.Active()) != 1 || cfg.Cluster.OwnCall() != "DL9ET" {
		t.Fatalf("%+v", cfg.Cluster)
	}
}

func TestClusterList(t *testing.T) {
	cfg, err := Load(write(t, `
[[cluster]]
name = "log4om"
address = "192.168.1.197:23"
call = "DL9ET"

[[cluster]]
name = "rbn"
address = "telnet.reversebeacon.net:7000"
call = "DL9ET"
skimmer = true

[[cluster]]
name = "off"
address = "x.example:23"
call = "DL9ET"
enabled = false
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Cluster) != 3 || cfg.Cluster[1].Name != "rbn" || !cfg.Cluster[1].Skimmer || cfg.Cluster[0].Skimmer {
		t.Fatalf("%+v", cfg.Cluster)
	}
	act := cfg.Cluster.Active()
	if len(act) != 2 || act[0].Name != "log4om" || act[1].Name != "rbn" {
		t.Fatalf("%+v", act)
	}
}

func TestClusterValidation(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate name":     "[[cluster]]\nname=\"a\"\naddress=\"h:23\"\ncall=\"X\"\n[[cluster]]\nname=\"A\"\naddress=\"i:23\"\ncall=\"X\"\n",
		"unnamed among many": "[[cluster]]\naddress=\"h:23\"\ncall=\"X\"\n[[cluster]]\nname=\"b\"\naddress=\"i:23\"\ncall=\"X\"\n",
		"bad address":        "[[cluster]]\nname=\"a\"\naddress=\"nohostport\"\ncall=\"X\"\n",
		"no call":            "[[cluster]]\nname=\"a\"\naddress=\"h:23\"\n",
		"unknown key":        "[[cluster]]\nname=\"a\"\naddres=\"h:23\"\n",
		"wrong type":         "[[cluster]]\nname=\"a\"\nskimmer=\"yes\"\n",
	} {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestBmrecGate(t *testing.T) {
	cfg, err := Load(write(t, ""))
	if err != nil || cfg.Bmrec.Gate != GateTrusted {
		t.Fatalf("default gate: %v %q", err, cfg.Bmrec.Gate)
	}
	for _, g := range []string{"off", "trusted", "qualified"} {
		cfg, err := Load(write(t, "[bmrec]\ngate = \""+g+"\"\n"))
		if err != nil || cfg.Bmrec.Gate != g {
			t.Errorf("%s: %v %q", g, err, cfg.Bmrec.Gate)
		}
	}
	if _, err := Load(write(t, "[bmrec]\ngate = \"strict\"\n")); err == nil {
		t.Error("unknown gate accepted")
	}
}

func TestBuddies(t *testing.T) {
	cfg, err := Load(write(t, "[[buddy]]\ncall = \"dl1abc\"\nnote = \"Hans\"\nboost = 5\n[[buddy]]\ncall = \"EA8/DL2XYZ\"\n"))
	if err != nil || len(cfg.Buddies) != 2 || cfg.Buddies[0].Boost != 5 || cfg.Buddies[0].Note != "Hans" {
		t.Fatalf("%v %+v", err, cfg.Buddies)
	}
	for name, body := range map[string]string{
		"no call":   "[[buddy]]\nnote = \"x\"\n",
		"duplicate": "[[buddy]]\ncall=\"DL1ABC\"\n[[buddy]]\ncall=\"dl1abc\"\n",
		"boost":     "[[buddy]]\ncall=\"DL1ABC\"\nboost = 5000\n",
	} {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLegacyClusterTableIgnoresUnknownKeys(t *testing.T) {
	cfg, err := Load(write(t, "[cluster]\naddress = \"h:23\"\ncall = \"DL9ET\"\nnode = \"old\"\nport = 23\n"))
	if err != nil || len(cfg.Cluster) != 1 || cfg.Cluster[0].Address != "h:23" {
		t.Fatalf("%v %+v", err, cfg.Cluster)
	}
}
