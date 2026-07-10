// Package config loads ubctrl's persistent on-disk configuration.
//
// Configuration is layered with the following precedence (highest wins):
//
//	explicit CLI flag > config-file value > built-in default
//
// The config file is a single TOML document that also carries the MQTT
// password, so on the target machine it must be stored with restrictive
// (0600) permissions. See docs for the deployment contract.
package config

import (
	"fmt"
	"os"

	toml "github.com/pelletier/go-toml/v2"
)

// MQTT holds the MQTT bridge settings. Password is a secret; the file that
// contains it must be 0600 on the target machine.
type MQTT struct {
	Broker          string `toml:"broker"`
	ClientID        string `toml:"client_id"`
	Site            string `toml:"site"`
	Station         string `toml:"station"`
	Slot            string `toml:"slot"`
	DiscoveryPrefix string `toml:"discovery_prefix"`
	// PublishHADiscovery gates the legacy embedded HA discovery. It defaults to
	// false now that a standalone consumer (hadiscovery) renders discovery from
	// this bridge's consumer-neutral `expose` block in /meta (integration model
	// §9). Set true only to fall back to the embedded discovery during migration.
	PublishHADiscovery bool   `toml:"publish_ha_discovery"`
	User               string `toml:"user"`
	Password           string `toml:"password"`
}

// Config is the full runtime configuration for ubctrl.
type Config struct {
	HTTPAddr   string `toml:"http_addr"`
	SerialPort string `toml:"serial_port"`
	Baud       int    `toml:"baud"`
	// Location and Host are deployment facts published in /meta (integration
	// model §3): the building the device sits in and the compute node this
	// bridge runs on. They come from config, never from code.
	Location string `toml:"location"`
	Host     string `toml:"host"`
	MQTT     MQTT   `toml:"mqtt"`
}

// Default returns the built-in defaults. These match the historical flag
// defaults so behaviour is unchanged when no config file is present.
// ClientID is left empty so the MQTT client derives it from the slot address
// (model §8); Slot defaults to the canonical ant-ctrl role (model §4).
func Default() Config {
	return Config{
		HTTPAddr:   "127.0.0.1:8080",
		SerialPort: "",
		Baud:       19200,
		MQTT: MQTT{
			Broker:          "",
			ClientID:        "",
			Slot:            "ant-ctrl",
			DiscoveryPrefix: "homeassistant",
			User:            "",
			Password:        "",
		},
	}
}

// Load reads the TOML file at path and overlays its values onto the built-in
// defaults. Keys omitted from the file keep their default value.
//
// A missing file is reported as an error wrapping fs.ErrNotExist so callers
// can distinguish "no file" (often tolerable) from a malformed file (always
// fatal) using errors.Is(err, fs.ErrNotExist).
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		// Preserve fs.ErrNotExist (and any other os error) for the caller.
		return cfg, err
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return Default(), fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}
