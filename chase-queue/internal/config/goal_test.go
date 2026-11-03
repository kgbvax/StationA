package config

import "testing"

func TestGoalsAndAwardsDir(t *testing.T) {
	cfg, err := Load(write(t, "data_dir = \"/d\"\n[[goal]]\naward = \"WAS-20-CW\"\npriority = 70\n[[goal]]\naward = \"dxcc\"\nenabled = false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Goals) != 2 || !cfg.Goals[0].IsEnabled() || cfg.Goals[0].Priority != 70 || cfg.Goals[1].IsEnabled() {
		t.Fatalf("%+v", cfg.Goals)
	}
	if cfg.AwardsDir() != "/d/awards" {
		t.Fatal(cfg.AwardsDir())
	}
	cfg, err = Load(write(t, "[awards]\ndir = \"/etc/x\"\n"))
	if err != nil || cfg.AwardsDir() != "/etc/x" || len(cfg.Goals) != 0 {
		t.Fatalf("%+v %v", cfg, err)
	}
	for name, body := range map[string]string{
		"no award": "[[goal]]\npriority = 5\n",
		"twice":    "[[goal]]\naward = \"a\"\n[[goal]]\naward = \"A\"\n",
		"priority": "[[goal]]\naward = \"a\"\npriority = 5000\n",
		"negative": "[[goal]]\naward = \"a\"\npriority = -1\n",
	} {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
