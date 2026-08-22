// Package config loads the hf-mqtt-capture service configuration.
//
// Precedence (highest wins): explicit CLI flag > config-file value > built-in default.
// The config file is a single TOML document that also carries the MQTT password, so on
// the target machine it must be 0600. See docs/conventions/config-and-secrets.md.
package config

import (
	"fmt"
	"os"

	toml "github.com/pelletier/go-toml/v2"
)

// Config is the full runtime configuration for the capture service.
type Config struct {
	Broker         string `toml:"broker"`
	User           string `toml:"user"`
	Password       string `toml:"password"`
	Site           string `toml:"site"`
	Station        string `toml:"station"`
	LogDir         string `toml:"log_dir"`
	RetentionHours int    `toml:"retention_hours"`
}

// Default returns built-in defaults.
func Default() Config {
	return Config{
		Broker:         "tcp://192.168.1.50:1883",
		User:           "hf",
		Site:           "muehle",
		Station:        "hf",
		LogDir:         "/var/log/hf-mqtt-capture",
		RetentionHours: 72,
	}
}

// Load reads the TOML file at path and overlays its values onto the built-in defaults.
// A missing file is returned as an error wrapping fs.ErrNotExist so the caller can
// distinguish "no file" from "malformed file".
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return Default(), fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the config is usable. Returns a descriptive error for problems.
func (c Config) Validate() error {
	if c.Broker == "" {
		return fmt.Errorf("config: broker is required")
	}
	if c.Site == "" || c.Station == "" {
		return fmt.Errorf("config: site and station are required")
	}
	if c.User == "" {
		return fmt.Errorf("config: user is required")
	}
	if c.LogDir == "" {
		return fmt.Errorf("config: log_dir is required")
	}
	if c.RetentionHours <= 0 {
		return fmt.Errorf("config: retention_hours must be positive")
	}
	return nil
}
