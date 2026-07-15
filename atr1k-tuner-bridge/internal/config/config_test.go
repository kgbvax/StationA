package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.MQTT.Slot != "tuner" {
		t.Errorf("default slot = %q, want tuner", cfg.MQTT.Slot)
	}
	if cfg.MQTT.Site != "muehle" || cfg.MQTT.Station != "hf" {
		t.Errorf("default site/station = %q/%q, want muehle/hf", cfg.MQTT.Site, cfg.MQTT.Station)
	}
	if cfg.Tuner.URL != "ws://192.168.1.20:60001" {
		t.Errorf("default tuner url = %q", cfg.Tuner.URL)
	}
	if cfg.Host != "shari" {
		t.Errorf("default host = %q, want shari", cfg.Host)
	}
	if cfg.Device.Model != "ATR-1000" {
		t.Errorf("default model = %q", cfg.Device.Model)
	}
	if cfg.Device.Link != "wifi" {
		t.Errorf("default link = %q, want wifi", cfg.Device.Link)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags := RegisterFlags(fs)
	_ = fs.Parse([]string{"-config", "/nonexistent/atr1k-tuner-bridge.toml"})
	cfg, err := Load(flags)
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if cfg.MQTT.Slot != "tuner" {
		t.Errorf("slot = %q, want default tuner", cfg.MQTT.Slot)
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
[tuner]
url = "ws://10.0.0.5:60001"
[mqtt]
broker = "tcp://1.2.3.4:1883"
site = "other"
station = "uhf"
slot = "tuner-2"
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
	if cfg.Tuner.URL != "ws://10.0.0.5:60001" {
		t.Errorf("tuner url = %q", cfg.Tuner.URL)
	}
	if cfg.MQTT.Broker != "tcp://1.2.3.4:1883" {
		t.Errorf("broker = %q", cfg.MQTT.Broker)
	}
	if cfg.MQTT.Site != "other" || cfg.MQTT.Station != "uhf" || cfg.MQTT.Slot != "tuner-2" {
		t.Errorf("slot addressing = %q/%q/%q", cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("ATR1K_TUNER_BRIDGE_MQTT_PASSWORD", "s3cret")
	t.Setenv("ATR1K_TUNER_BRIDGE_MQTT_SLOT", "tuner-9")
	t.Setenv("ATR1K_TUNER_BRIDGE_TUNER_URL", "ws://env.example:60001")

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
	if cfg.MQTT.Slot != "tuner-9" {
		t.Errorf("slot = %q, want tuner-9 (env)", cfg.MQTT.Slot)
	}
	if cfg.Tuner.URL != "ws://env.example:60001" {
		t.Errorf("tuner url = %q, want env override", cfg.Tuner.URL)
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
	cfg3.Tuner.URL = ""
	if err := cfg3.Validate(); err == nil {
		t.Error("missing tuner url should fail validation")
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
