// Package config holds runtime configuration for acom1200s-pa-bridge.
//
// acom1200s-pa-bridge bridges an ACOM 600S/1200S linear amplifier to MQTT using the
// station integration model (slot muehle/hf/pa). It reads a proprietary serial
// protocol over a USB-serial adapter and publishes a canonical PA state
// snapshot. This package defines the configuration shape, defaults and loading
// (TOML file + flags + env overrides for the [mqtt] section).
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
	Serial SerialConfig `toml:"serial"`
	MQTT   MQTTConfig   `toml:"mqtt"`
	Log    LogConfig    `toml:"log"`
	// Device describes the fronted hardware for /meta (model/serial/link).
	// The ACOM serial protocol does not report a serial number, so Device.Serial
	// is a stable configured identifier (defaulted in publishMeta when empty).
	Device DeviceConfig `toml:"device"`
	// Host is the compute node the adapter runs on (model §3, §8.1 item 5),
	// published in /meta. Defaults to "shari".
	Host string `toml:"host"`
}

// SerialConfig holds the serial-port settings.
type SerialConfig struct {
	// Port is the device path, preferably the /dev/serial/by-id/... symlink so
	// it is stable across replugs.
	Port string `toml:"port"`
	// AvgTimeMs is the forward-power moving-average window in milliseconds.
	// 1 (the minimum) effectively disables averaging, publishing the raw
	// per-frame forward power.
	AvgTimeMs int `toml:"avg_time_ms"`
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
	Slot     string `toml:"slot"`     // e.g. "pa"
	Location string `toml:"location"` // physical location label, e.g. "bauwagen"
}

// LogConfig controls logging verbosity.
type LogConfig struct {
	Level string `toml:"level"`
}

// DeviceConfig describes the fronted amplifier for /meta.
type DeviceConfig struct {
	Model  string `toml:"model"`  // e.g. "ACOM 1200S"
	Serial string `toml:"serial"` // stable id; protocol reports none
	Link   string `toml:"link"`   // transport, e.g. "serial"
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Host: "shari",
		Serial: SerialConfig{
			Port:      "/dev/serial/by-id/usb-Prolific_Technology_Inc._USB-Serial_Controller_D-if00-port0",
			AvgTimeMs: 1,
		},
		MQTT: MQTTConfig{
			Broker:          "tcp://192.168.1.50:1883",
			ClientID:        "",
			User:            "hf",
			DiscoveryPrefix: "homeassistant",
			Site:            "muehle",
			Station:         "hf",
			Slot:            "pa",
			Location:        "bauwagen",
		},
		Log:    LogConfig{Level: "info"},
		Device: DeviceConfig{Model: "ACOM 1200S", Serial: "", Link: "serial"},
	}
}

// Flags describes the command-line flags acom1200s-pa-bridge understands.
type Flags struct {
	ConfigPath string
	LogLevel   string
	Debug      bool
}

// RegisterFlags wires acom1200s-pa-bridge's flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	fs.StringVar(&f.ConfigPath, "config", "/etc/acom1200s-pa-bridge/config.toml", "path to config file")
	fs.StringVar(&f.LogLevel, "log.level", "", "log level (debug|info|warn|error); overrides config")
	fs.BoolVar(&f.Debug, "debug", false, "hex-dump serial I/O")
	return &f
}

// Load reads the TOML config file (if present), applies defaults and env
// overrides, and applies the flag overrides from f. An empty/missing config
// file is not an error: defaults are used.
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
		cfg.MQTT.Slot = "pa"
	}
	if cfg.Device.Model == "" {
		cfg.Device.Model = "ACOM 1200S"
	}
	if cfg.Device.Link == "" {
		cfg.Device.Link = "serial"
	}
	if cfg.Host == "" {
		cfg.Host = "shari"
	}

	return cfg, nil
}

// Validate checks that the config is usable. Station-model addressing is
// mandatory (model §2/§8.1): without site and station the slot topics would be
// malformed.
func (c Config) Validate() error {
	if c.MQTT.Site == "" || c.MQTT.Station == "" {
		return fmt.Errorf("mqtt site and station must be configured for station-model addressing")
	}
	if c.Serial.Port == "" {
		return fmt.Errorf("serial port must be configured")
	}
	return nil
}

// applyEnv overlays ACOM1200S_PA_BRIDGE_* env vars on top of cfg, used for the
// systemd EnvironmentFile workflow where the secret isn't in the TOML.
func applyEnv(cfg *Config) {
	if v := os.Getenv("ACOM1200S_PA_BRIDGE_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv("ACOM1200S_PA_BRIDGE_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTT.ClientID = v
	}
	if v := os.Getenv("ACOM1200S_PA_BRIDGE_MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv("ACOM1200S_PA_BRIDGE_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv("ACOM1200S_PA_BRIDGE_MQTT_SITE"); v != "" {
		cfg.MQTT.Site = v
	}
	if v := os.Getenv("ACOM1200S_PA_BRIDGE_MQTT_STATION"); v != "" {
		cfg.MQTT.Station = v
	}
	if v := os.Getenv("ACOM1200S_PA_BRIDGE_MQTT_SLOT"); v != "" {
		cfg.MQTT.Slot = v
	}
	if v := os.Getenv("ACOM1200S_PA_BRIDGE_SERIAL_PORT"); v != "" {
		cfg.Serial.Port = v
	}
}
