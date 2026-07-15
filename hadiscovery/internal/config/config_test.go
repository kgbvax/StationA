package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultArea locks the headline behavior: the built-in default area is "Bauwagen".
func TestDefaultArea(t *testing.T) {
	if got := Default().Area; got != "Bauwagen" {
		t.Fatalf("Default().Area = %q, want \"Bauwagen\"", got)
	}
}

// TestLoadAreaMissingKeepsDefault asserts a config file that omits `area` leaves the
// built-in "Bauwagen" default in place — this is the live-deploy case (the on-device config
// was seeded before the field existed, so it has no `area` line and must still get the default).
func TestLoadAreaMissingKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// Minimal valid config: no `area` key at all.
	if err := os.WriteFile(path, []byte(`
location = "bauwagen"
host     = "shari"
[mqtt]
broker  = "tcp://192.168.1.50:1883"
site    = "muehle"
station = "hf"
slot    = "discovery"
`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Area != "Bauwagen" {
		t.Errorf("Area = %q, want default \"Bauwagen\" when file omits area", cfg.Area)
	}
}

// TestLoadAreaOverride asserts an explicit `area` in the file overrides the default.
func TestLoadAreaOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`
location = "bauwagen"
host     = "shari"
area     = "Radio shack"
[mqtt]
broker  = "tcp://192.168.1.50:1883"
site    = "muehle"
station = "hf"
slot    = "discovery"
`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Area != "Radio shack" {
		t.Errorf("Area = %q, want file value \"Radio shack\"", cfg.Area)
	}
}

// TestLoadAreaEmptySuppresses asserts `area = ""` is honored as "no suggested_area" rather
// than being forced back to the default — the escape hatch for deployments that want none.
func TestLoadAreaEmptySuppresses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`
location = "bauwagen"
host     = "shari"
area     = ""
[mqtt]
broker  = "tcp://192.168.1.50:1883"
site    = "muehle"
station = "hf"
slot    = "discovery"
`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Area != "" {
		t.Errorf("Area = %q, want \"\" (explicitly suppressed)", cfg.Area)
	}
}
