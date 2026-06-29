package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	got := Default()
	want := Config{
		HTTPAddr:   "127.0.0.1:8080",
		SerialPort: "",
		Baud:       19200,
		MQTT: MQTT{
			ClientID: "ubctrl",
			Prefix:   "ubctrl",
		},
	}
	if got != want {
		t.Fatalf("Default() = %+v, want %+v", got, want)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadCompleteFile(t *testing.T) {
	path := writeTemp(t, `
http_addr   = "0.0.0.0:9090"
serial_port = "/dev/ttyUSB0"
baud        = 38400

[mqtt]
broker    = "tcp://broker.local:1883"
client_id = "shari"
prefix    = "antenna"
user      = "ham"
password  = "s3cr3t"
`)

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	want := Config{
		HTTPAddr:   "0.0.0.0:9090",
		SerialPort: "/dev/ttyUSB0",
		Baud:       38400,
		MQTT: MQTT{
			Broker:   "tcp://broker.local:1883",
			ClientID: "shari",
			Prefix:   "antenna",
			User:     "ham",
			Password: "s3cr3t",
		},
	}
	if got != want {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

func TestLoadPartialKeepsDefaults(t *testing.T) {
	// Only override the broker; everything else must stay at defaults.
	path := writeTemp(t, `
[mqtt]
broker = "tcp://broker.local:1883"
`)

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.HTTPAddr != "127.0.0.1:8080" {
		t.Errorf("HTTPAddr = %q, want default 127.0.0.1:8080", got.HTTPAddr)
	}
	if got.Baud != 19200 {
		t.Errorf("Baud = %d, want default 19200", got.Baud)
	}
	if got.MQTT.ClientID != "ubctrl" || got.MQTT.Prefix != "ubctrl" {
		t.Errorf("MQTT defaults lost: %+v", got.MQTT)
	}
	if got.MQTT.Broker != "tcp://broker.local:1883" {
		t.Errorf("Broker = %q, want overridden value", got.MQTT.Broker)
	}
}

func TestLoadMalformed(t *testing.T) {
	path := writeTemp(t, "http_addr = \"unterminated")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() on malformed file: want error, got nil")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("malformed file misreported as not-exist: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.toml")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() on missing file: want error, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: want fs.ErrNotExist, got %v", err)
	}
}
