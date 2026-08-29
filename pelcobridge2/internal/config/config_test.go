package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"pelcobridge2/internal/pelco"
)

func TestLoadOverridesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
[serial]
port = "COM3"
baud = 9600
addr = 2

[rotctld]
enabled = false
bind = "127.0.0.1"

[control]
jog_speed = 5
settle_ms = 1500
set_attempts = 4
set_tolerance_deg = 0.5
jog_hold_ms = 300

[mqtt]
enabled = true
broker = "tcp://broker:1883"
user = "hf"
site = "muehle"
station = "uhf"
slot = "rotator"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Serial.Port != "COM3" || cfg.Serial.Baud != 9600 || cfg.Serial.Addr != 2 {
		t.Errorf("serial = %+v", cfg.Serial)
	}
	if cfg.Rotctld.Enabled || cfg.Rotctld.Bind != "127.0.0.1" {
		t.Errorf("rotctld = %+v", cfg.Rotctld)
	}
	if cfg.Control.JogSpeed != 5 || cfg.Control.SetToleranceDeg != 0.5 || cfg.Control.JogHoldMS != 300 {
		t.Errorf("control = %+v", cfg.Control)
	}
	if !cfg.MQTT.Enabled || cfg.MQTT.User != "hf" {
		t.Errorf("mqtt = %+v", cfg.MQTT)
	}

	eng := cfg.EngineConfig()
	if eng.Settle != 1500*time.Millisecond || eng.JogSpeed != 5 || eng.SetAttempts != 4 || eng.Addr != 2 {
		t.Errorf("engine mapping = %+v", eng)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	// No path at all: pure defaults (seed-once deploy before the first seed).
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("empty path must not error: %v", err)
	}
	if cfg.Control.SettleMS != 2000 || cfg.Rotctld.Port != 4533 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

// An EXPLICIT path (flag / PELCOBRIDGE2_CONFIG) that does not exist must fail
// loudly — silent fallback to defaults once masked a mistyped -config flag.
func TestLoadExplicitMissingFileErrors(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err == nil {
		t.Fatal("explicit missing config must error, not fall back")
	}
}

func TestDefaultJogSpeedIsPelcoDefault(t *testing.T) {
	if got := Default().Control.JogSpeed; got != int(pelco.DefaultJogSpeed) {
		t.Errorf("default jog speed = %#x, want 0x12", got)
	}
}

func TestResolvePathPrecedence(t *testing.T) {
	if got := ResolvePath("/x/y.toml", "/e"); got != "/x/y.toml" {
		t.Errorf("flag path ignored: %q", got)
	}
	// No flag, no env, no candidate files anywhere: empty.
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if got := ResolvePath("", ""); got != "" {
		t.Errorf("empty candidates resolved to %q", got)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.toml")
	if err := SaveState(path, State{LastOffsetDeg: 42.5}); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("state.toml mode = %v, want 0600", info.Mode())
	}
	st := LoadState(path)
	if st.LastOffsetDeg != 42.5 {
		t.Errorf("last_offset_deg = %v, want 42.5", st.LastOffsetDeg)
	}
}

func TestLoadStateMissingIsZero(t *testing.T) {
	st := LoadState(filepath.Join(t.TempDir(), "absent.toml"))
	if st.LastOffsetDeg != 0 {
		t.Errorf("missing state = %+v, want zero", st)
	}
}
