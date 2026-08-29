package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
broker   = "tcp://127.0.0.1:1883"
user     = "hf"
password = "secret"
site     = "muehle"
station  = "hf"
log_dir  = "/var/log/hf-mqtt-capture"
retention_hours = 72
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Broker != "tcp://127.0.0.1:1883" {
		t.Errorf("broker = %q, want tcp://127.0.0.1:1883", cfg.Broker)
	}
	if cfg.RetentionHours != 72 {
		t.Errorf("retention_hours = %d, want 72", cfg.RetentionHours)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestValidateMissingBroker(t *testing.T) {
	cfg := Default()
	cfg.Broker = ""
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for missing broker")
	}
}

func TestValidateMissingSiteStation(t *testing.T) {
	cfg := Default()
	cfg.Site = ""
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for missing site/station")
	}
}
