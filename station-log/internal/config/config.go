// Package config holds station-log's runtime configuration: one TOML file
// (0600, seed-once) overlaid by environment variables for secrets, with
// flags for the file path and log level (flag > file > default).
package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration.
type Config struct {
	// Host is the compute node, published in /meta.
	Host string `toml:"host"`
	// DataDir holds the SQLite database.
	DataDir string `toml:"data_dir"`
	// DefaultOwnCall is the own callsign of a record that carries neither
	// STATION_CALLSIGN nor OPERATOR.
	DefaultOwnCall string `toml:"default_own_call"`
	// DupWindow is the +- time window of the duplicate merge (default 5m).
	DupWindow string     `toml:"dup_window"`
	Slot      SlotConfig `toml:"slot"`
	MQTT      MQTTConfig `toml:"mqtt"`
	HTTP      HTTPConfig `toml:"http"`
	Log4OM    Log4OMUDP  `toml:"log4om"`
	Log       LogConfig  `toml:"log"`
}

// SlotConfig is station-log's own slot under the site.
type SlotConfig struct {
	Station string `toml:"station"`
	Slot    string `toml:"slot"`
}

// MQTTConfig holds broker connection settings.
type MQTTConfig struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	User     string `toml:"user"`
	Password string `toml:"-"` // env only: STATION_LOG_MQTT_PASSWORD
	Site     string `toml:"site"`
}

// HTTPConfig is the HTTP API (import, read API, health).
type HTTPConfig struct {
	Listen string `toml:"listen"`
	// AllowFrom limits the clients (IPs or CIDRs); required when Listen is
	// not loopback, because POST /import has no auth.
	AllowFrom []string `toml:"allow_from"`
}

// Log4OMUDP is the Log4OM ADIF_MESSAGE feeder; off unless Listen is set.
type Log4OMUDP struct {
	Listen    string   `toml:"listen"`
	AllowFrom []string `toml:"allow_from"`
}

// LogConfig controls logging.
type LogConfig struct {
	Level string `toml:"level"`
}

// Defaults returns the default configuration.
func Defaults() Config {
	return Config{
		Host:      "scmino",
		DataDir:   "/var/lib/station-log",
		DupWindow: "5m",
		Slot:      SlotConfig{Station: "hf", Slot: "station-log"},
		MQTT:      MQTTConfig{Broker: "tcp://127.0.0.1:1883", Site: "muehle"},
		HTTP:      HTTPConfig{Listen: "127.0.0.1:8096"},
		Log:       LogConfig{Level: "info"},
	}
}

// Flags are the command-line flags.
type Flags struct {
	ConfigPath string
	LogLevel   string
}

// RegisterFlags wires the flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	fs.StringVar(&f.ConfigPath, "config", "/etc/station-log/config.toml", "path to config.toml")
	fs.StringVar(&f.LogLevel, "log.level", "", "log level (debug|info|warn|error); overrides config")
	return &f
}

// Load reads the file (a missing default path is not an error), overlays the
// environment and validates.
func Load(f *Flags) (Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(f.ConfigPath)
	switch {
	case err == nil:
		if _, err := toml.Decode(string(data), &cfg); err != nil {
			return Config{}, fmt.Errorf("decode %s: %w", f.ConfigPath, err)
		}
	case !errors.Is(err, os.ErrNotExist) || f.ConfigPath != "/etc/station-log/config.toml":
		return Config{}, fmt.Errorf("read %s: %w", f.ConfigPath, err)
	}
	if v := os.Getenv("STATION_LOG_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv("STATION_LOG_MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv("STATION_LOG_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if f.LogLevel != "" {
		cfg.Log.Level = strings.ToLower(f.LogLevel)
	}
	return cfg, cfg.Validate()
}

// Window is the parsed duplicate window.
func (c Config) Window() time.Duration {
	d, _ := time.ParseDuration(c.DupWindow)
	return d
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if c.DataDir == "" {
		return errors.New("data_dir is required")
	}
	if c.MQTT.Broker == "" || c.MQTT.Site == "" || c.Slot.Station == "" || c.Slot.Slot == "" {
		return errors.New("mqtt.broker, mqtt.site and slot must be set")
	}
	d, err := time.ParseDuration(c.DupWindow)
	if err != nil || d < 0 || d > time.Hour {
		return fmt.Errorf("dup_window %q: want a duration from 0 to 1h", c.DupWindow)
	}
	if _, _, err := net.SplitHostPort(c.HTTP.Listen); err != nil {
		return fmt.Errorf("http.listen %q: want host:port", c.HTTP.Listen)
	}
	if err := checkAllow("http.allow_from", c.HTTP.AllowFrom); err != nil {
		return err
	}
	if host, _, _ := net.SplitHostPort(c.HTTP.Listen); host != "127.0.0.1" && host != "::1" && host != "localhost" && len(c.HTTP.AllowFrom) == 0 {
		return fmt.Errorf("http.listen %q: a non-loopback listener needs http.allow_from (POST /import has no auth)", c.HTTP.Listen)
	}
	if c.Log4OM.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Log4OM.Listen); err != nil {
			return fmt.Errorf("log4om.listen %q: want host:port", c.Log4OM.Listen)
		}
		if len(c.Log4OM.AllowFrom) == 0 {
			return errors.New("log4om.allow_from is required with log4om.listen")
		}
		if err := checkAllow("log4om.allow_from", c.Log4OM.AllowFrom); err != nil {
			return err
		}
	}
	return nil
}

func checkAllow(name string, list []string) error {
	for _, a := range list {
		if _, err := ParseAllowFrom(a); err != nil {
			return fmt.Errorf("%s %q: %w", name, a, err)
		}
	}
	return nil
}

// ParseAllowFrom reads an address or a prefix.
func ParseAllowFrom(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
}

// Prefixes parses a validated allow_from list.
func Prefixes(list []string) []netip.Prefix {
	var out []netip.Prefix
	for _, a := range list {
		if p, err := ParseAllowFrom(a); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// DBPath is the SQLite file.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "station-log.db") }
