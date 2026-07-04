// Package config holds runtime configuration for flex2mqtt.
//
// flex2mqtt observes a FlexRadio 6000-series radio over the network and
// mirrors its state to MQTT for Home Assistant. This package defines the
// configuration shape, defaults and loading (TOML file + flags + env
// overrides for the [mqtt] section).
package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration.
type Config struct {
	// RadioHost is the radio hostname/IP. Empty -> use UDP discovery.
	RadioHost string `toml:"radio_host"`
	// RadioUDPPort is the local UDP port the radio will stream VITA-49
	// meters to (we tell the radio this port during the TCP handshake).
	RadioUDPPort int `toml:"radio_udp_port"`
	// RadioSerial, when non-empty, selects a specific discovered radio by
	// serial number. Empty -> first discovered radio.
	RadioSerial string `toml:"radio_serial"`

	MQTT  MQTTConfig  `toml:"mqtt"`
	Log   LogConfig   `toml:"log"`
	Rates RatesConfig `toml:"rates"`
}

// MQTTConfig holds broker connection settings.
type MQTTConfig struct {
	Broker          string `toml:"broker"`
	ClientID        string `toml:"client_id"`
	User            string `toml:"user"`
	Password        string `toml:"password"`
	DiscoveryPrefix string `toml:"discovery_prefix"`
	StatePrefix     string `toml:"state_prefix"`
}

// LogConfig controls logging verbosity.
type LogConfig struct {
	Level string `toml:"level"`
}

// RatesConfig sets the minimum publish interval per meter group, in seconds.
// A meter whose value changes is published only if at least this long has
// elapsed since its last publish (and the value differs by more than the
// unit deadband).
type RatesConfig struct {
	TX    float64 `toml:"tx"`
	Audio float64 `toml:"audio"`
	RX    float64 `toml:"rx"`
	HW    float64 `toml:"hw"`
}

// Rate returns the min-publish interval for a meter group as a Duration.
// Falls back to the default when zero or negative.
func (r RatesConfig) Rate(group string) time.Duration {
	var v float64
	switch group {
	case "tx":
		v = r.TX
	case "audio":
		v = r.Audio
	case "rx":
		v = r.RX
	case "hw":
		v = r.HW
	default:
		v = 1.0
	}
	if v <= 0 {
		v = 1.0
	}
	return time.Duration(v * float64(time.Second))
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		RadioHost:    "",
		RadioUDPPort: 4991,
		RadioSerial:  "",
		MQTT: MQTTConfig{
			Broker:          "tcp://homeassistant.local:1883",
			ClientID:        "flex2mqtt",
			DiscoveryPrefix: "homeassistant",
			StatePrefix:     "flex2mqtt",
		},
		Log: LogConfig{Level: "info"},
		Rates: RatesConfig{
			TX:    0.5,
			Audio: 0.5,
			RX:    1.0,
			HW:    10.0,
		},
	}
}

// Flags describes the command-line flags flex2mqtt understands.
type Flags struct {
	ConfigPath string
	LogLevel   string
}

// RegisterFlags wires flex2mqtt's flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	fs.StringVar(&f.ConfigPath, "config", "/etc/flex2mqtt/config.toml", "path to config file")
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
			// Surface typos but don't hard-fail; unknown keys are logged by caller.
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
	if cfg.MQTT.StatePrefix == "" {
		cfg.MQTT.StatePrefix = "flex2mqtt"
	}

	return cfg, nil
}

// applyEnv overlays FLEX2MQTT_* env vars on top of cfg, used for the
// systemd EnvironmentFile workflow where secrets aren't in the TOML.
func applyEnv(cfg *Config) {
	if v := os.Getenv("FLEX2MQTT_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv("FLEX2MQTT_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTT.ClientID = v
	}
	if v := os.Getenv("FLEX2MQTT_MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv("FLEX2MQTT_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv("FLEX2MQTT_RADIO_HOST"); v != "" {
		cfg.RadioHost = v
	}
	if v := os.Getenv("FLEX2MQTT_RADIO_SERIAL"); v != "" {
		cfg.RadioSerial = v
	}
	if v := os.Getenv("FLEX2MQTT_RADIO_UDP_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			cfg.RadioUDPPort = n
		}
	}
}
