// Package config loads the hadiscovery service's persistent configuration.
//
// hadiscovery is a passive consumer of the station bus: it subscribes to slot /meta
// announcements (the `expose` block) and renders Home Assistant discovery. It owns no
// device. Precedence (highest wins): explicit CLI flag > config-file value > built-in
// default. The config file is a single TOML document that also carries the MQTT password,
// so on the target machine it must be 0600. See docs/conventions/config-and-secrets.md.
package config

import (
	"fmt"
	"os"

	toml "github.com/pelletier/go-toml/v2"
)

// MQTT holds the broker settings. Password is a secret; the file that contains it must
// be 0600 on the target machine.
type MQTT struct {
	Broker          string `toml:"broker"`
	ClientID        string `toml:"client_id"`
	Site            string `toml:"site"`
	Station         string `toml:"station"`
	Slot            string `toml:"slot"`
	User            string `toml:"user"`
	Password        string `toml:"password"`
	DiscoveryPrefix string `toml:"discovery_prefix"` // default "homeassistant"
	MetaFilter      string `toml:"meta_filter"`      // default "<site>/+/+/meta"
}

// Config is the full runtime configuration for hadiscovery.
type Config struct {
	// Location and Host are deployment facts published in this service's own /meta
	// (integration model §3). hadiscovery is a logic slot (role "discovery", link "none").
	Location string `toml:"location"`
	Host     string `toml:"host"`

	// Area is the Home Assistant area every discovered device is suggested into when the
	// slot's own expose.device.area does not name one. It maps to HA's device-level
	// `suggested_area` (a hint applied at device creation; HA will not override a manual UI
	// assignment). Default "Bauwagen"; set to "" to emit no suggested_area at all.
	Area string `toml:"area"`

	MQTT MQTT `toml:"mqtt"`
}

// Default returns the built-in defaults. Site/station are left empty (a usable deployment
// must supply them via the config file); slot defaults to "discovery".
func Default() Config {
	return Config{
		Area: "Bauwagen",
		MQTT: MQTT{
			Slot:            "discovery",
			DiscoveryPrefix: "homeassistant",
		},
	}
}

// Load reads the TOML file at path and overlays its values onto the built-in defaults.
// A missing file is returned as an error wrapping fs.ErrNotExist so the caller can
// distinguish "no file" from a malformed file. After load, defaults that depend on
// configured values (MetaFilter) are filled if the file did not set them.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return Default(), fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.ApplyDerivedDefaults()
	return cfg, nil
}

// ApplyDerivedDefaults fills fields whose defaults depend on other configured values.
// MetaFilter defaults to "<site>/+/+/meta" (scoped to the configured site) when not set.
func (c *Config) ApplyDerivedDefaults() {
	if c.MQTT.DiscoveryPrefix == "" {
		c.MQTT.DiscoveryPrefix = "homeassistant"
	}
	if c.MQTT.MetaFilter == "" {
		if c.MQTT.Site != "" {
			c.MQTT.MetaFilter = c.MQTT.Site + "/+/+/meta"
		} else {
			c.MQTT.MetaFilter = "+/+/+/meta"
		}
	}
}

// Validate checks the required fields: site/station identify the bus this consumer
// watches, and location/host are required because hadiscovery publishes its own /meta.
func (c Config) Validate() error {
	if c.MQTT.Site == "" || c.MQTT.Station == "" {
		return fmt.Errorf("config: mqtt.site and mqtt.station are required")
	}
	if c.MQTT.Slot == "" {
		return fmt.Errorf("config: mqtt.slot is required")
	}
	if c.Location == "" || c.Host == "" {
		return fmt.Errorf("config: location and host are required (deployment facts published in /meta, model §3)")
	}
	if c.MQTT.MetaFilter == "" {
		return fmt.Errorf("config: mqtt.meta_filter is required")
	}
	return nil
}
