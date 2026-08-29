// Package config holds runtime configuration for flexbridge.
//
// flexbridge observes a FlexRadio 6000-series radio over the network and
// mirrors its state to MQTT using the station integration model. This
// package defines the configuration shape, defaults and loading (TOML file
// + flags + env overrides for the [mqtt] section).
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration.
type Config struct {
	// RadioHost is the radio hostname/IP. Empty -> use UDP discovery.
	RadioHost string `toml:"radio_host"`
	// RadioSerial, when non-empty, selects a specific discovered radio by
	// serial number. Empty -> first discovered radio.
	RadioSerial string `toml:"radio_serial"`

	MQTT MQTTConfig `toml:"mqtt"`
	Log  LogConfig  `toml:"log"`
}

// MQTTConfig holds broker connection settings and station-model addressing.
type MQTTConfig struct {
	Broker          string `toml:"broker"`
	ClientID        string `toml:"client_id"`
	User            string `toml:"user"`
	Password        string `toml:"password"`
	DiscoveryPrefix string `toml:"discovery_prefix"`
	// PublishHADiscovery gates the legacy embedded HA discovery. It defaults to
	// false now that a standalone consumer (hadiscovery) renders discovery from
	// this bridge's consumer-neutral `expose` block in /meta (integration model
	// §9). Set true only to fall back to the embedded discovery during migration.
	PublishHADiscovery bool `toml:"publish_ha_discovery"`

	// Station-model slot addressing: <site>/<station>/<slot>
	Site     string `toml:"site"`     // e.g. "muehle"
	Station  string `toml:"station"`  // e.g. "hf"
	Slot     string `toml:"slot"`     // e.g. "radio"
	Location string `toml:"location"` // physical location label, e.g. "bauwagen"
}

// LogConfig controls logging verbosity.
type LogConfig struct {
	Level string `toml:"level"`
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		RadioHost:   "",
		RadioSerial: "",
		MQTT: MQTTConfig{
			Broker:          "tcp://127.0.0.1:1883",
			ClientID:        "flexbridge",
			DiscoveryPrefix: "homeassistant",
			Slot:            "radio",
		},
		Log: LogConfig{Level: "info"},
	}
}

// Flags describes the command-line flags flexbridge understands.
type Flags struct {
	ConfigPath string
	LogLevel   string
}

// RegisterFlags wires flexbridge's flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	fs.StringVar(&f.ConfigPath, "config", "/etc/flexbridge/config.toml", "path to config file")
	fs.StringVar(&f.LogLevel, "log.level", "", "log level (debug|info|warn|error); overrides config")
	return &f
}

// Load reads the TOML config file (if present), applies defaults and env
// overrides, and applies the flag overrides from f. An empty/missing
// config file is not an error: defaults are used.
func Load(f *Flags) (Config, error) {
	cfg := Defaults()

	if data, err := os.ReadFile(f.ConfigPath); err == nil {
		md, err := toml.Decode(string(data), &cfg)
		if err != nil {
			return Config{}, fmt.Errorf("decode %s: %w", f.ConfigPath, err)
		}
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			_ = undecoded // intentionally not fatal
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("read %s: %w", f.ConfigPath, err)
	}

	applyEnv(&cfg)

	if f.LogLevel != "" {
		cfg.Log.Level = strings.ToLower(f.LogLevel)
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.MQTT.DiscoveryPrefix == "" {
		cfg.MQTT.DiscoveryPrefix = "homeassistant"
	}
	if cfg.MQTT.Slot == "" {
		cfg.MQTT.Slot = "radio"
	}

	return cfg, nil
}

// applyEnv overlays FLEXBRIDGE_* env vars on top of cfg, used for the
// systemd EnvironmentFile workflow where secrets aren't in the TOML.
func applyEnv(cfg *Config) {
	if v := os.Getenv("FLEXBRIDGE_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv("FLEXBRIDGE_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTT.ClientID = v
	}
	if v := os.Getenv("FLEXBRIDGE_MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv("FLEXBRIDGE_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv("FLEXBRIDGE_RADIO_HOST"); v != "" {
		cfg.RadioHost = v
	}
	if v := os.Getenv("FLEXBRIDGE_RADIO_SERIAL"); v != "" {
		cfg.RadioSerial = v
	}
	if v := os.Getenv("FLEXBRIDGE_SITE"); v != "" {
		cfg.MQTT.Site = v
	}
	if v := os.Getenv("FLEXBRIDGE_STATION"); v != "" {
		cfg.MQTT.Station = v
	}
	if v := os.Getenv("FLEXBRIDGE_SLOT"); v != "" {
		cfg.MQTT.Slot = v
	}
}
