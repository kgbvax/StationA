// Package config loads testui's persistent on-disk configuration.
//
// testui is a workstation-side MQTT relay + static UI server, not a deployed slot.
// It connects to the station broker as a passive consumer (subscribes <site>/#) and
// exposes the live tree to a browser over HTTP+SSE; the browser also publishes through
// it (to /cmd, to retained /state for simulation, and clears retained topics).
//
// Configuration is layered with the following precedence (highest wins):
//
//	explicit CLI flag > config-file value > built-in default
//
// The config file carries the MQTT password, so on disk it must be 0600. The password
// may also be supplied via the TESTUI_MQTT_PASSWORD environment variable (EnvironmentFile
// pattern, matching FLEXBRIDGE_MQTT_PASSWORD) so the TOML itself need not hold the secret.
// No -mqtt-password flag exists — the secret never reaches the command line.
package config

import (
	"fmt"
	"os"

	toml "github.com/pelletier/go-toml/v2"
)

// MQTT holds the broker connection settings. Password is a secret; the file that
// contains it must be 0600 on disk, or the password should come from the
// TESTUI_MQTT_PASSWORD env var instead.
type MQTT struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	Site     string `toml:"site"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

// Config is the full runtime configuration for testui.
type Config struct {
	HTTPAddr string `toml:"http_addr"`
	// Site is the MQTT site prefix to subscribe (<site>/#) and the guard prefix for
	// publish requests from the browser (the UI may only publish under <site>/).
	Site string `toml:"site"`
	MQTT MQTT  `toml:"mqtt"`
}

// Default returns the built-in defaults. ClientID is a fixed, distinct value (not
// derived from a slot address) so this passive consumer does not collide on the broker
// with any real slot's client ID.
func Default() Config {
	return Config{
		HTTPAddr: "127.0.0.1:8090",
		Site:     "muehle",
		MQTT: MQTT{
			Broker:   "tcp://192.168.1.50:1883",
			ClientID: "testui",
			Site:     "muehle",
			User:     "hf",
			Password: "",
		},
	}
}

// Load reads the TOML file at path and overlays its values onto the built-in defaults.
// Keys omitted from the file keep their default value.
//
// A missing file is reported as an error wrapping fs.ErrNotExist so callers can
// distinguish "no file" (often tolerable) from a malformed file (always fatal).
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err // preserve fs.ErrNotExist for the caller
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return Default(), fmt.Errorf("parse config %s: %w", path, err)
	}
	// Environment override for the secret (EnvironmentFile pattern). Never log this.
	if v := os.Getenv("TESTUI_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	return cfg, nil
}