package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.MQTT.Slot != "rotator" {
		t.Errorf("default slot = %q, want rotator", cfg.MQTT.Slot)
	}
	if cfg.MQTT.Site != "muehle" || cfg.MQTT.Station != "hf" {
		t.Errorf("default site/station = %q/%q, want muehle/hf", cfg.MQTT.Site, cfg.MQTT.Station)
	}
	if cfg.Rotor.URL != "ws://192.168.1.108/wsrotor" {
		t.Errorf("default rotor url = %q", cfg.Rotor.URL)
	}
	if !cfg.GS232.Enabled || cfg.GS232.Port != 7373 {
		t.Errorf("default gs232 = %+v, want enabled port 7373", cfg.GS232)
	}
	if cfg.Host != "shari" {
		t.Errorf("default host = %q, want shari", cfg.Host)
	}
	if cfg.Device.Model != "Yaesu G-450DC" {
		t.Errorf("default model = %q", cfg.Device.Model)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags := RegisterFlags(fs)
	_ = fs.Parse([]string{"-config", "/nonexistent/wrc-rotator-bridge.toml"})
	cfg, err := Load(flags)
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if cfg.MQTT.Slot != "rotator" {
		t.Errorf("slot = %q, want default rotator", cfg.MQTT.Slot)
	}
}

func TestLoadMalformedFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(path, []byte("not = valid = toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags := RegisterFlags(fs)
	_ = fs.Parse([]string{"-config", path})
	if _, err := Load(flags); err == nil {
		t.Fatal("malformed file should error")
	}
}

func TestLoadOverridesFromTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
host = "shack-pc"
[rotor]
url = "ws://10.0.0.5/wsrotor"
[gs232]
enabled = false
[mqtt]
broker = "tcp://1.2.3.4:1883"
site = "other"
station = "uhf"
slot = "rotator-2"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags := RegisterFlags(fs)
	_ = fs.Parse([]string{"-config", path})
	cfg, err := Load(flags)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Host != "shack-pc" {
		t.Errorf("host = %q, want shack-pc", cfg.Host)
	}
	if cfg.Rotor.URL != "ws://10.0.0.5/wsrotor" {
		t.Errorf("rotor url = %q", cfg.Rotor.URL)
	}
	if cfg.GS232.Enabled {
		t.Error("gs232 should be disabled by TOML")
	}
	if cfg.MQTT.Broker != "tcp://1.2.3.4:1883" {
		t.Errorf("broker = %q", cfg.MQTT.Broker)
	}
	if cfg.MQTT.Site != "other" || cfg.MQTT.Station != "uhf" || cfg.MQTT.Slot != "rotator-2" {
		t.Errorf("slot addressing = %q/%q/%q", cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("WRC_ROTATOR_BRIDGE_MQTT_PASSWORD", "s3cret")
	t.Setenv("WRC_ROTATOR_BRIDGE_MQTT_SLOT", "rotator-9")
	t.Setenv("WRC_ROTATOR_BRIDGE_ROTOR_URL", "ws://env.example/wsrotor")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags := RegisterFlags(fs)
	_ = fs.Parse([]string{"-config", "/nonexistent.toml"})
	cfg, err := Load(flags)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MQTT.Password != "s3cret" {
		t.Errorf("password = %q, want s3cret (env)", cfg.MQTT.Password)
	}
	if cfg.MQTT.Slot != "rotator-9" {
		t.Errorf("slot = %q, want rotator-9 (env)", cfg.MQTT.Slot)
	}
	if cfg.Rotor.URL != "ws://env.example/wsrotor" {
		t.Errorf("rotor url = %q, want env override", cfg.Rotor.URL)
	}
}

func TestValidate(t *testing.T) {
	cfg := Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}

	cfg2 := Defaults()
	cfg2.MQTT.Site = ""
	if err := cfg2.Validate(); err == nil {
		t.Error("missing site should fail validation")
	}

	cfg3 := Defaults()
	cfg3.Rotor.URL = ""
	if err := cfg3.Validate(); err == nil {
		t.Error("missing rotor url should fail validation")
	}

	cfg4 := Defaults()
	cfg4.GS232.Enabled = true
	cfg4.GS232.Port = 0
	if err := cfg4.Validate(); err == nil {
		t.Error("enabled gs232 with port 0 should fail validation")
	}
}

func TestFlagLogLevelOverrides(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags := RegisterFlags(fs)
	_ = fs.Parse([]string{"-config", "/nonexistent.toml", "-log.level", "debug"})
	cfg, err := Load(flags)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log level = %q, want debug (flag)", cfg.Log.Level)
	}
}
