// Package config holds runtime configuration for atr1k-tuner-bridge.
//
// atr1k-tuner-bridge bridges the ATR-1000 ATU (BTR-1000 / N7DDC family) to MQTT
// using the station integration model (slot muehle/hf/tuner). It reads the
// tuner's binary WebSocket status stream and publishes a canonical tuner state
// snapshot. This package defines the configuration shape, defaults and loading
// (TOML file + flags + env overrides for the [mqtt] and [tuner] sections).
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
	Tuner  TunerConfig  `toml:"tuner"`
	MQTT   MQTTConfig   `toml:"mqtt"`
	Log    LogConfig    `toml:"log"`
	Device DeviceConfig `toml:"device"`
	// Host is the compute node the adapter runs on (model §3, §8.1 item 5),
	// published in /meta. Defaults to "shari".
	Host string `toml:"host"`
}

// TunerConfig holds the ATR-1000 WebSocket endpoint.
type TunerConfig struct {
	// URL is the ATR-1000 binary WebSocket endpoint, e.g. "ws://192.168.1.20:60001".
	URL string `toml:"url"`
}

// MQTTConfig holds broker connection settings and station-model addressing.
type MQTTConfig struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	User     string `toml:"user"`
	Password string `toml:"password"`

	// Station-model slot addressing: <site>/<station>/<slot>
	Site     string `toml:"site"`     // e.g. "muehle"
	Station  string `toml:"station"`  // e.g. "hf"
	Slot     string `toml:"slot"`     // e.g. "tuner"
	Location string `toml:"location"` // physical location label, e.g. "bauwagen"
}

// LogConfig controls logging verbosity.
type LogConfig struct {
	Level string `toml:"level"`
}

// DeviceConfig describes the fronted tuner for /meta.
type DeviceConfig struct {
	Model string `toml:"model"` // e.g. "ATR-1000"
	Link  string `toml:"link"`  // transport to the tuner, e.g. "wifi"
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Host: "shari",
		Tuner: TunerConfig{
			URL: "ws://192.168.1.20:60001",
		},
		MQTT: MQTTConfig{
			Broker:   "tcp://192.168.1.50:1883",
			ClientID: "",
			User:     "hf",
			Site:     "muehle",
			Station:  "hf",
			Slot:     "tuner",
			Location: "bauwagen",
		},
		Log:    LogConfig{Level: "info"},
		Device: DeviceConfig{Model: "ATR-1000", Link: "wifi"},
	}
}

// Flags describes the command-line flags atr1k-tuner-bridge understands.
type Flags struct {
	ConfigPath string
	LogLevel   string
	Debug      bool
}

// RegisterFlags wires atr1k-tuner-bridge's flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	fs.StringVar(&f.ConfigPath, "config", "/etc/atr1k-tuner-bridge/config.toml", "path to config file")
	fs.StringVar(&f.LogLevel, "log.level", "", "log level (debug|info|warn|error); overrides config")
	fs.BoolVar(&f.Debug, "debug", false, "log ATR-1000 WebSocket I/O")
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
	if cfg.MQTT.Slot == "" {
		cfg.MQTT.Slot = "tuner"
	}
	if cfg.Device.Model == "" {
		cfg.Device.Model = "ATR-1000"
	}
	if cfg.Device.Link == "" {
		cfg.Device.Link = "wifi"
	}
	if cfg.Host == "" {
		cfg.Host = "shari"
	}

	return cfg, nil
}

// Validate checks that the config is usable. Station-model addressing is
// mandatory (model §2/§8.1): without site and station the slot topics would be
// malformed. The tuner endpoint is mandatory.
func (c Config) Validate() error {
	if c.MQTT.Site == "" || c.MQTT.Station == "" {
		return fmt.Errorf("mqtt site and station must be configured for station-model addressing")
	}
	if c.Tuner.URL == "" {
		return fmt.Errorf("tuner url must be configured")
	}
	return nil
}

// applyEnv overlays ATR1K_TUNER_BRIDGE_* env vars on top of cfg, used for the
// systemd EnvironmentFile workflow where the secret isn't in the TOML. The
// prefix is the dir name uppercased with hyphens → underscores (see
// docs/conventions/naming.md).
func applyEnv(cfg *Config) {
	if v := os.Getenv("ATR1K_TUNER_BRIDGE_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv("ATR1K_TUNER_BRIDGE_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTT.ClientID = v
	}
	if v := os.Getenv("ATR1K_TUNER_BRIDGE_MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv("ATR1K_TUNER_BRIDGE_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv("ATR1K_TUNER_BRIDGE_MQTT_SITE"); v != "" {
		cfg.MQTT.Site = v
	}
	if v := os.Getenv("ATR1K_TUNER_BRIDGE_MQTT_STATION"); v != "" {
		cfg.MQTT.Station = v
	}
	if v := os.Getenv("ATR1K_TUNER_BRIDGE_MQTT_SLOT"); v != "" {
		cfg.MQTT.Slot = v
	}
	if v := os.Getenv("ATR1K_TUNER_BRIDGE_TUNER_URL"); v != "" {
		cfg.Tuner.URL = v
	}
}
