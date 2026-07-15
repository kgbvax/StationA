package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFileIsErrNotExist(t *testing.T) {
	_, err := Load("/nonexistent/path/config.toml")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist, got %v", err)
	}
}

func TestLoadOverlaysDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(`
http_addr = "0.0.0.0:9999"
site = "beta"
[mqtt]
broker = "tcp://example:1883"
user = "u"
password = "secret"
`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != "0.0.0.0:9999" {
		t.Errorf("http_addr=%q", cfg.HTTPAddr)
	}
	if cfg.Site != "beta" {
		t.Errorf("site=%q", cfg.Site)
	}
	if cfg.MQTT.Broker != "tcp://example:1883" {
		t.Errorf("broker=%q", cfg.MQTT.Broker)
	}
	if cfg.MQTT.Password != "secret" {
		t.Errorf("password not loaded")
	}
	// Untouched defaults preserved.
	if cfg.MQTT.ClientID != "testui" {
		t.Errorf("client_id default lost: %q", cfg.MQTT.ClientID)
	}
}

func TestEnvPasswordOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("[mqtt]\npassword = \"fromfile\"\n"), 0600)
	t.Setenv("TESTUI_MQTT_PASSWORD", "fromenv")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Password != "fromenv" {
		t.Errorf("env did not override file password: %q", cfg.MQTT.Password)
	}
}

func TestLoadMalformedIsFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("this is not = = valid toml [[[ "), 0600)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected parse error")
	}
}