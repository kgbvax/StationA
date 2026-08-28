package config

import (
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml/v2"
)

// State is the content of state.toml: the last entered azimuth offset, kept as
// a TUI PREFILL only. Arming always requires the operator to confirm or correct
// it — the offset is never loaded into the engine automatically, and armed
// state is never persisted.
type State struct {
	LastOffsetDeg float64 `toml:"last_offset_deg"`
}

// LoadState reads state.toml next to the config file (or the given path).
// Missing file → zero state, no error (first run).
func LoadState(path string) State {
	var st State
	if path == "" {
		return st
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	_ = toml.Unmarshal(data, &st)
	return st
}

// SaveState writes state.toml (0600) next to the config file.
func SaveState(path string, st State) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	data, err := toml.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
