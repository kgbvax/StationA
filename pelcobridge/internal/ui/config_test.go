package ui

import (
	"testing"

	"pelcots/internal/config"
	"pelcots/internal/engine"
)

// TestConfigPreservesNonEditableFields guards against the TUI quit-save
// resetting fields the TUI does not edit. Config() must start from the loaded
// config, not Default(), so self_check, sim, and log_level survive unchanged.
func TestConfigPreservesNonEditableFields(t *testing.T) {
	cfg := config.Default()
	cfg.LogLevel = config.LogDebug
	cfg.SelfCheck = config.SelfCheckConfig{Disable: false}
	cfg.Sim = config.SimConfig{StartPan: 10, StartTilt: 20, JogStep: 7}

	m := New(engine.New(engine.Options{}), cfg, cfg.Log)
	got := m.Config()

	if got.LogLevel != config.LogDebug {
		t.Errorf("log_level = %s, want debug", got.LogLevel)
	}
	if got.SelfCheck.Disable != false {
		t.Errorf("self_check.disable = %v, want false", got.SelfCheck.Disable)
	}
	if got.Sim != (config.SimConfig{StartPan: 10, StartTilt: 20, JogStep: 7}) {
		t.Errorf("sim = %+v, want {StartPan:10 StartTilt:20 JogStep:7}", got.Sim)
	}
}
