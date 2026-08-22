// Package config holds runtime configuration for wrc-rotator-bridge.
//
// wrc-rotator-bridge bridges the HF antenna rotator (Yaesu G-450DC steered via an
// AF6SA WRC controller) to MQTT using the station integration model (slot
// muehle/hf/rotator). It reads the WRC's WebSocket JSON status stream and
// publishes a canonical rotator state snapshot. This package defines the
// configuration shape, defaults and loading (TOML file + flags + env overrides
// for the [mqtt] and [rotor] sections).
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
	Rotor      RotorConfig      `toml:"rotor"`
	GS232      GS232Config      `toml:"gs232"`
	PSTRotator PSTRotatorConfig `toml:"pstrotator"`
	MQTT       MQTTConfig       `toml:"mqtt"`
	Log        LogConfig        `toml:"log"`
	Device     DeviceConfig     `toml:"device"`
	// Host is the compute node the adapter runs on (model §3, §8.1 item 5),
	// published in /meta. Defaults to "shari".
	Host string `toml:"host"`
}

// RotorConfig holds the WRC WebSocket endpoint.
type RotorConfig struct {
	// URL is the WRC WebSocket endpoint, e.g. "ws://192.168.1.108/wsrotor".
	URL string `toml:"url"`
}

// GS232Config controls the legacy GS-232B inbound TCP server (optional control
// path for rotator-control software such as PSTRotator/N1MM/rotctld). It is
// orthogonal to the MQTT three-plane contract: it drives the same device the
// bridge does, and the resulting motion still surfaces in /state.
type GS232Config struct {
	Enabled bool   `toml:"enabled"`
	Bind    string `toml:"bind"` // bind address, e.g. "0.0.0.0"
	Port    int    `toml:"port"` // listen port, e.g. 7373
}

// PSTRotatorConfig controls the PSTRotator-compatible inbound UDP listener
// (optional control path for PSTRotator). It is orthogonal to the MQTT
// three-plane contract: it drives the same device the bridge does, and the
// resulting motion still surfaces in /state.
type PSTRotatorConfig struct {
	Enabled bool   `toml:"enabled"`
	Bind    string `toml:"bind"` // bind address, e.g. "0.0.0.0"
	Port    int    `toml:"port"` // listen port, e.g. 12040
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
	// §9). Retained only as a migration fallback; new components omit it.
	PublishHADiscovery bool `toml:"publish_ha_discovery"`

	// Station-model slot addressing: <site>/<station>/<slot>
	Site     string `toml:"site"`     // e.g. "muehle"
	Station  string `toml:"station"`  // e.g. "hf"
	Slot     string `toml:"slot"`     // e.g. "rotator"
	Location string `toml:"location"` // physical location label, e.g. "bauwagen"
}

// LogConfig controls logging verbosity.
type LogConfig struct {
	Level string `toml:"level"`
}

// DeviceConfig describes the fronted rotator for /meta.
type DeviceConfig struct {
	Model string `toml:"model"` // e.g. "Yaesu G-450DC"
	Link  string `toml:"link"`  // transport to the controller, e.g. "ethernet"
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Host: "shari",
		Rotor: RotorConfig{
			URL: "ws://192.168.1.108/wsrotor",
		},
		GS232: GS232Config{
			Enabled: true,
			Bind:    "0.0.0.0",
			Port:    7373,
		},
		PSTRotator: PSTRotatorConfig{
			Enabled: true,
			Bind:    "0.0.0.0",
			Port:    12040,
		},
		MQTT: MQTTConfig{
			Broker:          "tcp://192.168.1.50:1883",
			ClientID:        "",
			User:            "hf",
			DiscoveryPrefix: "homeassistant",
			Site:            "muehle",
			Station:         "hf",
			Slot:            "rotator",
			Location:        "bauwagen",
		},
		Log:    LogConfig{Level: "info"},
		Device: DeviceConfig{Model: "Yaesu G-450DC", Link: "ethernet"},
	}
}

// Flags describes the command-line flags wrc-rotator-bridge understands.
type Flags struct {
	ConfigPath string
	LogLevel   string
	Debug      bool
}

// RegisterFlags wires wrc-rotator-bridge's flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	fs.StringVar(&f.ConfigPath, "config", "/etc/wrc-rotator-bridge/config.toml", "path to config file")
	fs.StringVar(&f.LogLevel, "log.level", "", "log level (debug|info|warn|error); overrides config")
	fs.BoolVar(&f.Debug, "debug", false, "log WRC WebSocket I/O")
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
		cfg.MQTT.Slot = "rotator"
	}
	if cfg.Device.Model == "" {
		cfg.Device.Model = "Yaesu G-450DC"
	}
	if cfg.Device.Link == "" {
		cfg.Device.Link = "ethernet"
	}
	if cfg.Host == "" {
		cfg.Host = "shari"
	}

	return cfg, nil
}

// Validate checks that the config is usable. Station-model addressing is
// mandatory (model §2/§8.1): without site and station the slot topics would be
// malformed. The WRC endpoint is mandatory.
func (c Config) Validate() error {
	if c.MQTT.Site == "" || c.MQTT.Station == "" {
		return fmt.Errorf("mqtt site and station must be configured for station-model addressing")
	}
	if c.Rotor.URL == "" {
		return fmt.Errorf("rotor url must be configured")
	}
	if c.GS232.Enabled && c.GS232.Port == 0 {
		return fmt.Errorf("gs232 port must be configured when gs232 is enabled")
	}
	if c.PSTRotator.Enabled && c.PSTRotator.Port == 0 {
		return fmt.Errorf("pstrotator port must be configured when pstrotator is enabled")
	}
	return nil
}

// applyEnv overlays WRC_ROTATOR_BRIDGE_* env vars on top of cfg, used for the
// systemd EnvironmentFile workflow where the secret isn't in the TOML.
func applyEnv(cfg *Config) {
	if v := os.Getenv("WRC_ROTATOR_BRIDGE_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv("WRC_ROTATOR_BRIDGE_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTT.ClientID = v
	}
	if v := os.Getenv("WRC_ROTATOR_BRIDGE_MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv("WRC_ROTATOR_BRIDGE_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv("WRC_ROTATOR_BRIDGE_MQTT_SITE"); v != "" {
		cfg.MQTT.Site = v
	}
	if v := os.Getenv("WRC_ROTATOR_BRIDGE_MQTT_STATION"); v != "" {
		cfg.MQTT.Station = v
	}
	if v := os.Getenv("WRC_ROTATOR_BRIDGE_MQTT_SLOT"); v != "" {
		cfg.MQTT.Slot = v
	}
	if v := os.Getenv("WRC_ROTATOR_BRIDGE_ROTOR_URL"); v != "" {
		cfg.Rotor.URL = v
	}
}
