package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	cfg, err := Load(&Flags{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Host != "shack-pc" || cfg.StaleAfter != 10*time.Minute {
		t.Fatalf("defaults: %+v", cfg)
	}
	if cfg.Slot.Station != "hf" || cfg.Slot.Slot != "spots" {
		t.Fatalf("slot defaults: %+v", cfg.Slot)
	}
	if len(cfg.Listeners) != 1 || cfg.Listeners[0].Kind != "n1mm" || cfg.Listeners[0].Port != 12060 {
		t.Fatalf("listener defaults: %+v", cfg.Listeners)
	}
	if cfg.MQTT.Broker != "tcp://hassio.kgbvax.net:1883" || cfg.MQTT.User != "hf" || cfg.MQTT.Site != "muehle" {
		t.Fatalf("mqtt defaults: %+v", cfg.MQTT)
	}
}

func TestLoadTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
host = "shack-pc"
station_locator = "JO31"
stale_after = "5m"

[slot]
station = "hf"
slot    = "spots"

[[listener]]
name = "dxlog"
kind = "n1mm"
port = 12060

[[listener]]
name = "log4om"
kind = "log4om"
port = 2249

[mqtt]
broker = "tcp://hassio.kgbvax.net:1883"
user   = "hf"
site   = "muehle"
`
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(&Flags{ConfigPath: path})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.StaleAfter != 5*time.Minute {
		t.Fatalf("stale_after = %v", cfg.StaleAfter)
	}
	if len(cfg.Listeners) != 2 || cfg.Listeners[1].Kind != "log4om" || cfg.Listeners[1].Port != 2249 {
		t.Fatalf("listeners: %+v", cfg.Listeners)
	}
	if cfg.StationLocator != "JO31" {
		t.Fatalf("station_locator = %q", cfg.StationLocator)
	}
}

func TestExplicitConfigMustExist(t *testing.T) {
	if _, err := Load(&Flags{ConfigPath: "/nonexistent/config.toml"}); err == nil {
		t.Fatal("explicit missing config accepted")
	}
	// Implicit (default path resolution) missing config falls back to defaults.
	if _, err := Load(&Flags{}); err != nil {
		t.Fatalf("implicit missing config should default: %v", err)
	}
}

func TestBadListenerKind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := `
[[listener]]
name = "x"
kind = "telnet"
port = 1
`
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(&Flags{ConfigPath: path}); err == nil {
		t.Fatal("bad listener kind accepted")
	}
}

func TestBadStationLocator(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("station_locator = \"ZZ99\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(&Flags{ConfigPath: path}); err == nil {
		t.Fatal("malformed station_locator accepted")
	}
}

func TestEnvPasswordOverride(t *testing.T) {
	t.Setenv("LOGGER_SPOT_BRIDGE_MQTT_PASSWORD", "sekret")
	cfg, err := Load(&Flags{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Password != "sekret" {
		t.Fatalf("password env not applied: %q", cfg.MQTT.Password)
	}
}
