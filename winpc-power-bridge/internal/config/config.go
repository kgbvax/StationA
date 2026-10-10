// Package config holds runtime configuration for winpc-power-bridge.
//
// winpc-power-bridge is a small agent on the Windows shack PC (bwpc) that
// publishes the station integration-model slot muehle/power/shack-pc and puts
// the PC to sleep on a /cmd {"action":"sleep"}. This package defines the
// configuration shape, defaults and loading (TOML file + flags + env overrides).
//
// Secrets: the MQTT password comes from the environment variable
// WINPC_POWER_BRIDGE_MQTT_PASSWORD. The TOML [mqtt] password is a fallback for
// the Windows host, where the bridge runs as a hidden Task Scheduler task and
// an EnvironmentFile does not exist (same rule as pelcobridge2, the other
// shack-PC component). The config file lives in the user's profile, which only
// the user, SYSTEM and Administrators can read.
//
// Precedence (highest wins): CLI flag > environment > config file > default.
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// EnvPrefix is the env-override prefix (naming convention: the directory name
// in upper case, hyphens to underscores).
const EnvPrefix = "WINPC_POWER_BRIDGE_"

// DefaultConfigName is the config file name next to the executable, used when
// no -config flag is given.
const DefaultConfigName = "config.toml"

// Config is the top-level configuration.
type Config struct {
	// Host is the compute node the agent runs on, published in /meta.
	Host string `toml:"host"`
	// Location is the physical location label, published in /meta.
	Location string `toml:"location"`

	MQTT   MQTTConfig   `toml:"mqtt"`
	Device DeviceConfig `toml:"device"`
	Sleep  SleepConfig  `toml:"sleep"`
	State  StateConfig  `toml:"state"`
	Log    LogConfig    `toml:"log"`

	// path is the config file that was loaded ("" if none). Relative paths in
	// the config (log file) resolve against its directory.
	path string
}

// MQTTConfig holds broker connection settings and the slot address.
type MQTTConfig struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	User     string `toml:"user"`
	// Password is the fallback only; WINPC_POWER_BRIDGE_MQTT_PASSWORD wins.
	Password string `toml:"password"`
	Site     string `toml:"site"`
	Station  string `toml:"station"`
	Slot     string `toml:"slot"`
}

// DeviceConfig is the identity published in /meta.device.
type DeviceConfig struct {
	Model  string `toml:"model"`
	Serial string `toml:"serial"` // default: Host
	Link   string `toml:"link"`
}

// SleepConfig holds the sleep guard.
type SleepConfig struct {
	// MinUserIdleMinutes: a sleep command is refused when the last keyboard or
	// mouse input in the user's session is newer than this.
	MinUserIdleMinutes int `toml:"min_user_idle_minutes"`
}

// StateConfig controls the periodic /state refresh.
type StateConfig struct {
	IntervalS int `toml:"interval_s"`
}

// LogConfig controls logging.
type LogConfig struct {
	Level string `toml:"level"`
	// File is an optional log file. Windows exception to the logging
	// convention: the agent runs as a hidden GUI-subsystem task, so stderr
	// goes nowhere. Empty = stderr only. A relative path resolves against the
	// config file directory.
	File string `toml:"file"`
}

// Defaults returns a Config with the built-in defaults.
func Defaults() Config {
	return Config{
		Host:     "bwpc",
		Location: "bauwagen",
		MQTT: MQTTConfig{
			Broker:  "tcp://192.168.1.50:1883",
			User:    "hf",
			Site:    "muehle",
			Station: "power",
			Slot:    "shack-pc",
		},
		Device: DeviceConfig{
			Model: "Windows PC",
			Link:  "ethernet",
		},
		Sleep: SleepConfig{MinUserIdleMinutes: 30},
		State: StateConfig{IntervalS: 30},
		Log:   LogConfig{Level: "info"},
	}
}

// Flags describes the command-line flags.
type Flags struct {
	ConfigPath string
	LogLevel   string
}

// RegisterFlags wires the flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	fs.StringVar(&f.ConfigPath, "config", "", "path to config file (default: "+DefaultConfigName+" next to the executable)")
	fs.StringVar(&f.LogLevel, "log.level", "", "log level (debug|info|warn|error); overrides config")
	return &f
}

// Load reads the config file, applies env and flag overrides and fills empty
// keys with defaults. An explicitly named config file (-config) that does not
// exist is an error; the implicit default next to the executable may be
// missing (then the built-in defaults apply).
func Load(f *Flags) (Config, error) {
	cfg := Defaults()

	path := f.ConfigPath
	explicit := path != ""
	if !explicit {
		path = defaultConfigPath()
	}
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if _, err := toml.Decode(string(data), &cfg); err != nil {
				return Config{}, fmt.Errorf("decode %s: %w", path, err)
			}
			cfg.path = path
		case os.IsNotExist(err) && !explicit:
			// No implicit config: defaults.
		default:
			return Config{}, fmt.Errorf("read %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if f.LogLevel != "" {
		cfg.Log.Level = f.LogLevel
	}
	fillDefaults(&cfg)
	return cfg, nil
}

// fillDefaults restores defaults for keys a config file set to "".
func fillDefaults(cfg *Config) {
	d := Defaults()
	cfg.Log.Level = strings.ToLower(cfg.Log.Level)
	if cfg.Log.Level == "" {
		cfg.Log.Level = d.Log.Level
	}
	if cfg.Host == "" {
		cfg.Host = d.Host
	}
	if cfg.Location == "" {
		cfg.Location = d.Location
	}
	if cfg.MQTT.Site == "" {
		cfg.MQTT.Site = d.MQTT.Site
	}
	if cfg.MQTT.Station == "" {
		cfg.MQTT.Station = d.MQTT.Station
	}
	if cfg.MQTT.Slot == "" {
		cfg.MQTT.Slot = d.MQTT.Slot
	}
	if cfg.MQTT.ClientID == "" {
		cfg.MQTT.ClientID = cfg.MQTT.Site + "-" + cfg.MQTT.Station + "-" + cfg.MQTT.Slot
	}
	if cfg.Device.Model == "" {
		cfg.Device.Model = d.Device.Model
	}
	if cfg.Device.Link == "" {
		cfg.Device.Link = d.Device.Link
	}
	if cfg.Device.Serial == "" {
		cfg.Device.Serial = cfg.Host
	}
	if cfg.State.IntervalS == 0 {
		cfg.State.IntervalS = d.State.IntervalS
	}
}

// Validate checks that the config is usable.
func (c Config) Validate() error {
	if c.MQTT.Broker == "" {
		return fmt.Errorf("mqtt broker must be configured")
	}
	if c.MQTT.Site == "" || c.MQTT.Station == "" || c.MQTT.Slot == "" {
		return fmt.Errorf("mqtt site, station and slot must be configured")
	}
	if c.Sleep.MinUserIdleMinutes < 1 {
		return fmt.Errorf("sleep.min_user_idle_minutes must be >= 1 (got %d)", c.Sleep.MinUserIdleMinutes)
	}
	if c.State.IntervalS < 5 {
		return fmt.Errorf("state.interval_s must be >= 5 (got %d)", c.State.IntervalS)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level must be debug|info|warn|error (got %q)", c.Log.Level)
	}
	return nil
}

// Path returns the config file that was loaded, or "" if none.
func (c Config) Path() string { return c.path }

// LogFilePath returns the log file path with a relative path resolved against
// the config file directory (or the executable directory if no config file was
// loaded). Returns "" when no log file is configured.
func (c Config) LogFilePath() string {
	if c.Log.File == "" || filepath.IsAbs(c.Log.File) {
		return c.Log.File
	}
	base := ""
	if c.path != "" {
		base = filepath.Dir(c.path)
	} else if exe, err := os.Executable(); err == nil {
		base = filepath.Dir(exe)
	}
	return filepath.Join(base, c.Log.File)
}

// defaultConfigPath is config.toml next to the executable.
func defaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), DefaultConfigName)
}

// applyEnv overlays WINPC_POWER_BRIDGE_MQTT_* env vars on cfg.
func applyEnv(cfg *Config) {
	if v := os.Getenv(EnvPrefix + "MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv(EnvPrefix + "MQTT_CLIENT_ID"); v != "" {
		cfg.MQTT.ClientID = v
	}
	if v := os.Getenv(EnvPrefix + "MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv(EnvPrefix + "MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv(EnvPrefix + "MQTT_SITE"); v != "" {
		cfg.MQTT.Site = v
	}
}
