// Package config holds runtime configuration for shelly-power-bridge.
//
// shelly-power-bridge fronts one or more Shelly Gen2+ smart plugs that speak
// MQTT natively, and translates their native topics into the station
// integration-model `power` slot (<site>/<station>/<slot>/{meta,state,status,cmd}).
// This package defines the configuration shape, defaults and loading (TOML file
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
	// Host is the compute node the adapter runs on (model §3, §8.1 item 5),
	// published in /meta. Defaults to "shari".
	Host string `toml:"host"`
	// MQTT holds broker connection settings and station-model addressing shared
	// by every slot this bridge fronts.
	MQTT MQTTConfig `toml:"mqtt"`
	// Log controls logging verbosity.
	Log LogConfig `toml:"log"`
	// Slots is the set of Shelly plugs this bridge fronts. Each becomes one
	// canonical `power` slot with its own /meta /state /status /cmd and its own
	// paho client (and LWT), so a bridge death takes all its slots offline with
	// no stale-online gap.
	Slots []SlotConfig `toml:"slot"`
}

// MQTTConfig holds broker connection settings. Per-slot addressing
// (station/slot) lives in each SlotConfig so one process can publish multiple
// site-level power slots.
type MQTTConfig struct {
	Broker          string `toml:"broker"`
	ClientID        string `toml:"client_id"`
	User            string `toml:"user"`
	Password        string `toml:"password"`
	Site            string `toml:"site"`             // e.g. "muehle"
	DiscoveryPrefix string `toml:"discovery_prefix"` // hadiscovery (model §9)
}

// LogConfig controls logging verbosity.
type LogConfig struct {
	Level string `toml:"level"`
}

// SlotConfig describes one Shelly plug → one canonical power slot.
type SlotConfig struct {
	// Station/slot form the canonical address <site>/<station>/<slot>. The site
	// comes from [mqtt].site. For the site-level power layer these are
	// station="power", slot="master"|"psu-13v8"|… (integration model §7.0).
	Station  string `toml:"station"`
	Slot     string `toml:"slot"`
	Location string `toml:"location"` // physical location label, published in /meta

	// DeviceModel / DeviceSerial are the Shelly identity published in /meta.
	// DeviceSerial should be the Shelly's stable id (the Gen2 device id, e.g.
	// "shellyplus1pm-<macsuffix>"), which is also the Shelly MQTT prefix.
	DeviceModel  string `toml:"device_model"`
	DeviceSerial string `toml:"device_serial"`

	// ShellyID is the Gen2+ MQTT prefix the Shelly publishes under, e.g.
	// "shellyplus1pm-aabbccddeeff". The bridge subscribes to
	// "<shelly_id>/status/switch:0" for telemetry and publishes the
	// "<shelly_id>/rpc" topic to command it.
	ShellyID string `toml:"shelly_id"`

	// FailSafe is the Shelly power-on default published in capabilities:
	// "off" means the plug restores OFF after a mains blip (a power blip drops
	// the station rather than re-energizing it unexpectedly). Default "off".
	FailSafe string `toml:"fail_safe"`

	// Feeds lists the downstream slot addresses this supply powers (model §4
	// `power` capability `feeds`). Empty for the station master mains; populated
	// for the 13.8 V PSU (hf/radio, uhf/radio, hf/tuner, …).
	Feeds []string `toml:"feeds"`
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Host: "shari",
		MQTT: MQTTConfig{
			Broker:          "tcp://192.168.1.50:1883",
			ClientID:        "",
			User:            "hf",
			Site:            "muehle",
			DiscoveryPrefix: "homeassistant",
		},
		Log: LogConfig{Level: "info"},
	}
}

// Flags describes the command-line flags shelly-power-bridge understands.
type Flags struct {
	ConfigPath string
	LogLevel   string
}

// RegisterFlags wires shelly-power-bridge's flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	fs.StringVar(&f.ConfigPath, "config", "/etc/shelly-power-bridge/config.toml", "path to config file")
	fs.StringVar(&f.LogLevel, "log.level", "", "log level (debug|info|warn|error); overrides config")
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
	if cfg.MQTT.Site == "" {
		cfg.MQTT.Site = "muehle"
	}
	if cfg.Host == "" {
		cfg.Host = "shari"
	}
	for i := range cfg.Slots {
		if cfg.Slots[i].FailSafe == "" {
			cfg.Slots[i].FailSafe = "off"
		}
	}

	return cfg, nil
}

// Validate checks that the config is usable. Station-model addressing (site) is
// mandatory, and at least one slot must be configured, each with a station,
// slot, device serial, and shelly id (the native MQTT prefix to subscribe and
// command).
func (c Config) Validate() error {
	if c.MQTT.Site == "" {
		return fmt.Errorf("mqtt site must be configured for station-model addressing")
	}
	if c.MQTT.Broker == "" {
		return fmt.Errorf("mqtt broker must be configured")
	}
	if len(c.Slots) == 0 {
		return fmt.Errorf("at least one [[slot]] must be configured")
	}
	seen := map[string]bool{}
	for i, s := range c.Slots {
		if s.Station == "" || s.Slot == "" {
			return fmt.Errorf("slot[%d]: station and slot must be set", i)
		}
		if s.ShellyID == "" {
			return fmt.Errorf("slot[%d]: shelly_id must be set (the Gen2 MQTT prefix)", i)
		}
		if s.DeviceSerial == "" {
			return fmt.Errorf("slot[%d]: device_serial must be set", i)
		}
		switch s.FailSafe {
		case "off", "on":
		default:
			return fmt.Errorf("slot[%d]: fail_safe must be \"on\" or \"off\" (got %q)", i, s.FailSafe)
		}
		addr := c.MQTT.Site + "/" + s.Station + "/" + s.Slot
		if seen[addr] {
			return fmt.Errorf("duplicate slot address %q", addr)
		}
		seen[addr] = true
	}
	return nil
}

// applyEnv overlays SHELLY_POWER_BRIDGE_* env vars on top of cfg, used for the
// systemd EnvironmentFile workflow where the secret isn't in the TOML. Per-slot
// values come from the TOML; only the shared broker/credentials/site are
// env-overridable.
func applyEnv(cfg *Config) {
	if v := os.Getenv("SHELLY_POWER_BRIDGE_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv("SHELLY_POWER_BRIDGE_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTT.ClientID = v
	}
	if v := os.Getenv("SHELLY_POWER_BRIDGE_MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv("SHELLY_POWER_BRIDGE_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv("SHELLY_POWER_BRIDGE_MQTT_SITE"); v != "" {
		cfg.MQTT.Site = v
	}
}
