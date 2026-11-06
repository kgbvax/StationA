// Package config holds runtime configuration for tgxl-tuner-bridge.
//
// tgxl-tuner-bridge bridges the FlexRadio Tuner Genius XL (1x3) to MQTT using
// the station integration model (slot muehle/hf/tuner). It polls the tuner's
// TCP API and publishes a canonical tuner state snapshot. This package defines the configuration shape, defaults and loading
// (TOML file + flags + env overrides for the [mqtt] and [tuner] sections).
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration.
type Config struct {
	Tuner  TunerConfig  `toml:"tuner"`
	MQTT   MQTTConfig   `toml:"mqtt"`
	Log    LogConfig    `toml:"log"`
	Device DeviceConfig `toml:"device"`
	// Host is the compute node the adapter runs on (model §3, §8.1 item 5),
	// published in /meta. Defaults to "scmino".
	Host string `toml:"host"`
}

// TunerConfig holds the Tuner Genius XL endpoint.
type TunerConfig struct {
	// Address is host:port of the TGXL API (port 9010). Empty = find the tuner
	// from its UDP 9010 discovery broadcast.
	Address string `toml:"address"`
	// AuthCode is only needed when connecting from outside the TGXL's LAN.
	AuthCode string `toml:"auth_code"`
	// PollInterval is how often `status` is requested (Go duration, e.g. "250ms").
	PollInterval string `toml:"poll_interval"`
	// TxPollInterval is the faster `status` interval while PTT is asserted (e.g. "50ms").
	TxPollInterval string `toml:"tx_poll_interval"`
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
	Model string `toml:"model"` // e.g. "Tuner Genius XL"
	Link  string `toml:"link"`  // transport to the tuner, e.g. "ethernet"
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Host: "scmino",
		Tuner: TunerConfig{
			Address:        "192.168.1.145:9010",
			PollInterval:   "250ms",
			TxPollInterval: "50ms",
		},
		MQTT: MQTTConfig{
			Broker:   "tcp://127.0.0.1:1883",
			ClientID: "",
			User:     "hf",
			Site:     "muehle",
			Station:  "hf",
			Slot:     "tuner",
			Location: "bauwagen",
		},
		Log:    LogConfig{Level: "info"},
		Device: DeviceConfig{Model: "Tuner Genius XL", Link: "ethernet"},
	}
}

// Flags describes the command-line flags tgxl-tuner-bridge understands.
type Flags struct {
	ConfigPath string
	LogLevel   string
	Debug      bool
}

// RegisterFlags wires tgxl-tuner-bridge's flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	fs.StringVar(&f.ConfigPath, "config", "/etc/tgxl-tuner-bridge/config.toml", "path to config file")
	fs.StringVar(&f.LogLevel, "log.level", "", "log level (debug|info|warn|error); overrides config")
	fs.BoolVar(&f.Debug, "debug", false, "log TGXL TCP I/O")
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
		cfg.Device.Model = "Tuner Genius XL"
	}
	if cfg.Device.Link == "" {
		cfg.Device.Link = "ethernet"
	}
	if cfg.Host == "" {
		cfg.Host = "scmino"
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
	if _, err := c.PollDuration(); err != nil {
		return err
	}
	if _, err := c.TxPollDuration(); err != nil {
		return err
	}
	return nil
}

// applyEnv overlays TGXL_TUNER_BRIDGE_* env vars on top of cfg, used for the
// systemd EnvironmentFile workflow where the secret isn't in the TOML. The
// prefix is the dir name uppercased with hyphens → underscores (see
// docs/conventions/naming.md).
func applyEnv(cfg *Config) {
	if v := os.Getenv("TGXL_TUNER_BRIDGE_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv("TGXL_TUNER_BRIDGE_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTT.ClientID = v
	}
	if v := os.Getenv("TGXL_TUNER_BRIDGE_MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv("TGXL_TUNER_BRIDGE_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv("TGXL_TUNER_BRIDGE_MQTT_SITE"); v != "" {
		cfg.MQTT.Site = v
	}
	if v := os.Getenv("TGXL_TUNER_BRIDGE_MQTT_STATION"); v != "" {
		cfg.MQTT.Station = v
	}
	if v := os.Getenv("TGXL_TUNER_BRIDGE_MQTT_SLOT"); v != "" {
		cfg.MQTT.Slot = v
	}
	if v := os.Getenv("TGXL_TUNER_BRIDGE_TUNER_ADDRESS"); v != "" {
		cfg.Tuner.Address = v
	}
	if v := os.Getenv("TGXL_TUNER_BRIDGE_TUNER_AUTH_CODE"); v != "" {
		cfg.Tuner.AuthCode = v
	}
}

// PollDuration parses tuner.poll_interval (default 250ms, minimum 50ms).
func (c Config) PollDuration() (time.Duration, error) {
	if c.Tuner.PollInterval == "" {
		return 250 * time.Millisecond, nil
	}
	d, err := time.ParseDuration(c.Tuner.PollInterval)
	if err != nil || d < 50*time.Millisecond {
		return 0, fmt.Errorf("tuner.poll_interval %q must be a duration >= 50ms", c.Tuner.PollInterval)
	}
	return d, nil
}

// TxPollDuration parses tuner.tx_poll_interval (default 50ms, minimum 10ms).
func (c Config) TxPollDuration() (time.Duration, error) {
	if c.Tuner.TxPollInterval == "" {
		return 50 * time.Millisecond, nil
	}
	d, err := time.ParseDuration(c.Tuner.TxPollInterval)
	if err != nil || d < 10*time.Millisecond {
		return 0, fmt.Errorf("tuner.tx_poll_interval %q must be a duration >= 10ms", c.Tuner.TxPollInterval)
	}
	return d, nil
}
