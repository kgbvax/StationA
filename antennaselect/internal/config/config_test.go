package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoadAndValidate(t *testing.T) {
	path := writeTemp(t, `
location = "bauwagen"
host     = "shari"

[mqtt]
broker  = "tcp://broker.local:1883"
site    = "muehle"
station = "hf"

[wiring_map]
port1 = "dummy-load"
port3 = "ultrabeam"
port6 = "fan-dipole"
off   = "grounded"

[band_policy]
fallback = "fan-dipole"

[band_policy.bands]
ultrabeam  = ["20m", "15m"]
fan-dipole = ["40m", "80m"]

[band_follow]
resource = "ultrabeam"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MQTT.Slot != "antenna-select" {
		t.Errorf("slot default lost: %q", cfg.MQTT.Slot)
	}
	if cfg.BandFollow.Slot != "ant-ctrl" {
		t.Errorf("band_follow.slot default lost: %q", cfg.BandFollow.Slot)
	}
	if cfg.BandFollow.Resource != "ultrabeam" {
		t.Errorf("band_follow.resource = %q, want ultrabeam", cfg.BandFollow.Resource)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	if got := cfg.ResourceToPort()["ultrabeam"]; got != "port3" {
		t.Errorf("ResourceToPort[ultrabeam] = %q, want port3", got)
	}
	if _, ok := cfg.ResourceToPort()["grounded"]; ok {
		t.Error("off/grounded should be excluded from ResourceToPort")
	}
}

func TestValidateMissingFallback(t *testing.T) {
	cfg := Config{
		Location:  "bauwagen",
		Host:      "shari",
		MQTT:      MQTT{Site: "muehle", Station: "hf"},
		WiringMap: map[string]string{"port2": "fan-dipole"},
		BandPolicy: BandPolicy{
			Bands: map[string][]string{"fan-dipole": {"40m"}},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for missing fallback")
	}
}

func TestValidateUnwiredResource(t *testing.T) {
	cfg := Config{
		Location:  "bauwagen",
		Host:      "shari",
		MQTT:      MQTT{Site: "muehle", Station: "hf"},
		WiringMap: map[string]string{"port2": "fan-dipole"},
		BandPolicy: BandPolicy{
			Bands:    map[string][]string{"ultrabeam": {"20m"}}, // ultrabeam not wired
			Fallback: "fan-dipole",
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for band_policy resource not in wiring_map")
	}
}

func TestValidateUnwiredFollowResource(t *testing.T) {
	cfg := Config{
		Location:  "bauwagen",
		Host:      "shari",
		MQTT:      MQTT{Site: "muehle", Station: "hf"},
		WiringMap: map[string]string{"port2": "fan-dipole"},
		BandPolicy: BandPolicy{
			Bands:    map[string][]string{"fan-dipole": {"40m"}},
			Fallback: "fan-dipole",
		},
		BandFollow: BandFollow{Resource: "ultrabeam", Slot: "ant-ctrl"}, // not wired
	}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for band_follow.resource not in wiring_map")
	}
}

func TestValidateRequiresSiteStation(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err == nil {
		t.Error("expected error when site/station unset")
	}
}

func TestValidateOverlappingBandsToDifferentPorts(t *testing.T) {
	cfg := Config{
		Location: "bauwagen",
		Host:     "shari",
		MQTT:     MQTT{Site: "muehle", Station: "hf"},
		WiringMap: map[string]string{
			"port1": "dummy-load",
			"port3": "ultrabeam",
			"port6": "fan-dipole",
			"off":   "grounded",
		},
		BandPolicy: BandPolicy{
			Bands: map[string][]string{
				"ultrabeam":  {"20m", "15m"},
				"fan-dipole": {"20m", "40m"}, // 20m overlaps with ultrabeam -> different port
			},
			Fallback: "fan-dipole",
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error for overlapping bands mapping to different ports")
	}
	want := `band "20m" maps to multiple switch ports`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("expected error to contain %q, got %q", want, err.Error())
	}
}

