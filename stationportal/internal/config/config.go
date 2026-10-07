// Package config loads stationportal's on-disk configuration.
//
// stationportal is the station landing page: a passive bus consumer (no slot,
// no publishes, no LWT) that serves an HTTP overview of the station's services,
// slots, hosts and software. Precedence: config-file value > built-in default.
// The MQTT password comes from STATIONPORTAL_MQTT_PASSWORD (EnvironmentFile,
// 0600) so it never sits in the TOML, the unit file or the command line.
package config

import (
	"fmt"
	"os"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// PasswordEnv is the environment variable carrying the MQTT password.
const PasswordEnv = "STATIONPORTAL_MQTT_PASSWORD"

// MQTT holds the broker connection settings.
type MQTT struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

// Config is the full runtime configuration.
type Config struct {
	HTTPAddr string `toml:"http_addr"`
	// Site is the MQTT site prefix; the portal subscribes <site>/#.
	Site string `toml:"site"`
	// Inventory optionally overrides the inventory built into the binary
	// (a TOML file with the same shape as internal/inventory/inventory.toml).
	Inventory string `toml:"inventory"`
	// ProbeIntervalS is how often the service links are probed.
	ProbeIntervalS int `toml:"probe_interval_s"`
	// ProbeTimeoutS bounds one probe.
	ProbeTimeoutS int    `toml:"probe_timeout_s"`
	LogLevel      string `toml:"log_level"`
	MQTT          MQTT   `toml:"mqtt"`
}

// Default returns the built-in defaults. The broker is the live station broker
// on hassio (.50), not the scmino mirror on 127.0.0.1 — no app uses the mirror
// yet. ClientID is fixed and distinct from any slot-derived client ID.
func Default() Config {
	return Config{
		HTTPAddr:       ":80",
		Site:           "muehle",
		ProbeIntervalS: 30,
		ProbeTimeoutS:  3,
		LogLevel:       "info",
		MQTT: MQTT{
			Broker:   "tcp://192.168.1.50:1883",
			ClientID: "stationportal",
			User:     "hf",
		},
	}
}

// ProbeInterval returns the probe cadence, never below 5 s.
func (c Config) ProbeInterval() time.Duration {
	return time.Duration(max(c.ProbeIntervalS, 5)) * time.Second
}

// ProbeTimeout returns the per-probe bound, between 1 s and the interval.
func (c Config) ProbeTimeout() time.Duration {
	t := time.Duration(max(c.ProbeTimeoutS, 1)) * time.Second
	return min(t, c.ProbeInterval())
}

// Load reads the TOML file at path over the defaults. A missing file is
// returned as an error wrapping fs.ErrNotExist so the caller can tolerate it.
// The password env var wins over a password in the file.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		cfg.applyEnv()
		return cfg, err
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return Default(), fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyEnv()
	return cfg, nil
}

func (c *Config) applyEnv() {
	if v := os.Getenv(PasswordEnv); v != "" {
		c.MQTT.Password = v
	}
}
