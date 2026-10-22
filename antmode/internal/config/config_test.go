// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func valid() Config {
	c := Default()
	c.Location, c.Host = "bauwagen", "scmino"
	c.MQTT.Site, c.MQTT.Station = "muehle", "hf"
	return c
}

func TestDefaults(t *testing.T) {
	c := valid()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Storm.AutoEnter || c.Power.Borrow {
		t.Fatal("auto_enter and power borrow must default off (observe-only rollout)")
	}
	if c.Storm.ExitHoldoffMinutes != 60 || c.Storm.MaxAz != 360 || c.MQTT.Slot != "ant-mode" ||
		c.State.Path != "/var/lib/antmode/state.json" || c.NINA.BaseURL != "https://warnung.bund.de/api31" {
		t.Fatalf("defaults = %+v", c)
	}
	has := map[string]bool{}
	for _, s := range c.NINA.IICodes {
		has[s] = true
	}
	for _, s := range []string{"51", "52", "53", "54", "55", "56", "31", "33", "40", "96"} {
		if !has[s] {
			t.Errorf("default ii_codes miss %s", s)
		}
	}
	for _, s := range []string{"34", "59", "61", "90", "98", "99"} {
		if has[s] {
			t.Errorf("default ii_codes include non-gust code %s", s)
		}
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*Config){
		"no site":            func(c *Config) { c.MQTT.Site = "" },
		"no host":            func(c *Config) { c.Host = "" },
		"bad ars":            func(c *Config) { c.NINA.ARS = "09162" },
		"bad slot":           func(c *Config) { c.Slots.AntennaPower = "antennas" },
		"fast poll":          func(c *Config) { c.NINA.PollS = 10 },
		"empty codes":        func(c *Config) { c.NINA.IICodes = nil },
		"bad code":           func(c *Config) { c.NINA.IICodes = []string{"WINDBÖEN"} },
		"max_az":             func(c *Config) { c.Storm.MaxAz = 500 },
		"fallback > max":     func(c *Config) { c.Storm.FallbackAz = 400 },
		"maintenance < 0":    func(c *Config) { c.Maintenance.ParkAz = -1 },
		"holdoff":            func(c *Config) { c.Storm.ExitHoldoffMinutes = 0 },
		"tolerance":          func(c *Config) { c.Park.ToleranceDeg = 0 },
		"devices timeout":    func(c *Config) { c.Power.DevicesTimeoutS = 60 },
		"no state path":      func(c *Config) { c.State.Path = "" },
		"stale minutes":      func(c *Config) { c.Wind.StaleMinutes = 0 },
		"retract timeout 0":  func(c *Config) { c.Park.RetractTimeoutS = 0 },
		"tx quiet too short": func(c *Config) { c.Park.TXQuietS = 5 },
	}
	for name, mut := range cases {
		c := valid()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := valid()
	c.NINA.ARS = "091620000000"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "missing.toml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	p := filepath.Join(dir, "c.toml")
	if err := os.WriteFile(p, []byte(`
location = "bauwagen"
host = "scmino"
[mqtt]
site = "muehle"
station = "hf"
[nina]
ars = "091620000000"
ii_codes = ["52"]
[storm]
auto_enter = true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Storm.AutoEnter || c.Storm.ExitHoldoffMinutes != 60 || len(c.NINA.IICodes) != 1 || c.Slots.Rotator != "hf/rotator" {
		t.Fatalf("load = %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	// A misspelt key is an error, not a silent default.
	if err := os.WriteFile(p, []byte("[storm]\nauto_entr = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "auto_entr") {
		t.Fatalf("unknown key: %v", err)
	}
}

// config.example.toml must load and validate as shipped.
func TestExampleConfig(t *testing.T) {
	c, err := Load("../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Storm.AutoEnter || c.Power.Borrow {
		t.Fatal("example must ship observe-only")
	}
	d := Default()
	if strings.Join(c.NINA.IICodes, ",") != strings.Join(d.NINA.IICodes, ",") {
		t.Fatalf("example ii_codes %v != defaults %v", c.NINA.IICodes, d.NINA.IICodes)
	}
}

func TestSplitSlot(t *testing.T) {
	if st, sl := SplitSlot("power/antennas"); st != "power" || sl != "antennas" {
		t.Fatal(st, sl)
	}
}
