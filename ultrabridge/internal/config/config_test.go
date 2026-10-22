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
		// The retract latch is persisted in the systemd StateDirectory.
		StateDir: "/var/lib/ultrabridge",
		MQTT: MQTT{
			// ClientID empty: derived from the slot address by the MQTT client
			// (model §8). Slot is the canonical ant-ctrl role (model §4).
			ClientID:        "",
			Slot:            "ant-ctrl",
			DiscoveryPrefix: "homeassistant",
			// The antenna-mode lock is on by default; with no retained
			// ant-mode state the Ultrabeam is unlocked (rollout-safe).
			AntModeSlot: "ant-mode",
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
location    = "bauwagen"
host        = "shari"

[mqtt]
broker           = "tcp://broker.local:1883"
client_id        = "custom-id"
site             = "muehle"
station          = "hf"
slot             = "ant-ctrl"
discovery_prefix = "homeassistant"
user             = "ham"
password         = "s3cr3t"
`)

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	want := Config{
		HTTPAddr:   "0.0.0.0:9090",
		SerialPort: "/dev/ttyUSB0",
		Baud:       38400,
		Location:   "bauwagen",
		Host:       "shari",
		StateDir:   "/var/lib/ultrabridge", // key absent → default
		MQTT: MQTT{
			Broker:          "tcp://broker.local:1883",
			ClientID:        "custom-id",
			Site:            "muehle",
			Station:         "hf",
			Slot:            "ant-ctrl",
			DiscoveryPrefix: "homeassistant",
			User:            "ham",
			Password:        "s3cr3t",
			AntModeSlot:     "ant-mode", // key absent → default
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
	if got.MQTT.ClientID != "" || got.MQTT.Slot != "ant-ctrl" {
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

// TestLoadAntModeSlotDisable: an explicit empty ant_mode_slot disables the
// antenna-mode lock (no subscription, no startup hold).
func TestLoadAntModeSlotDisable(t *testing.T) {
	path := writeTemp(t, `
[mqtt]
ant_mode_slot = ""
`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.MQTT.AntModeSlot != "" {
		t.Errorf("AntModeSlot = %q, want empty (disabled)", got.MQTT.AntModeSlot)
	}
}

// TestLoadStateDirDisable: state_dir is read from the file; "" keeps the
// retract latch in memory only.
func TestLoadStateDirDisable(t *testing.T) {
	path := writeTemp(t, "state_dir = \"\"\n")
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.StateDir != "" {
		t.Errorf("StateDir = %q, want empty (disabled)", got.StateDir)
	}
}
