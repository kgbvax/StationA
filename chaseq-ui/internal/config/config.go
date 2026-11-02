// Package config holds chaseq-ui's runtime configuration: a TOML file next to
// the executable, with the MQTT password taken from the environment
// (CHASEQ_UI_MQTT_PASSWORD) and the TOML value only as a fallback, as for
// winpc-power-bridge on the same host.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// EnvPrefix is the env-override prefix.
const EnvPrefix = "CHASEQ_UI_"

// Config is the top-level configuration.
type Config struct {
	MQTT    MQTTConfig    `toml:"mqtt"`
	Topics  TopicsConfig  `toml:"topics"`
	HTTP    HTTPConfig    `toml:"http"`
	Loggers LoggersConfig `toml:"loggers"`
	// FiltersFile holds the view filters (JSON); relative to the config dir.
	FiltersFile string    `toml:"filters_file"`
	Log         LogConfig `toml:"log"`

	dir string
}

// MQTTConfig is the broker connection.
type MQTTConfig struct {
	Broker   string `toml:"broker"`
	User     string `toml:"user"`
	Password string `toml:"password"`
	ClientID string `toml:"client_id"`
}

// TopicsConfig names the slots chaseq-ui talks to.
type TopicsConfig struct {
	Site     string `toml:"site"`
	Station  string `toml:"station"`
	Chase    string `toml:"chase"`    // chase-queue slot
	CQFinder string `toml:"cqfinder"` // bmrec candidates slot
	Radio    string `toml:"radio"`    // flexbridge slot
}

// HTTPConfig is the loopback server the window loads.
type HTTPConfig struct {
	Listen string `toml:"listen"`
}

// LoggersConfig names where the callsign is sent.
type LoggersConfig struct {
	Log4OMAddr   string `toml:"log4om_addr"`    // Remote Control Interface inbound UDP
	DXLogAddr    string `toml:"dxlog_addr"`     // chaseq-ui DXLog script listener
	Log4OMTimout string `toml:"log4om_timeout"` // reply wait, Go duration
}

// LogConfig is file logging (the windowsgui exe has no stderr).
type LogConfig struct {
	File string `toml:"file"`
}

// Default returns the defaults.
func Default() Config {
	return Config{
		MQTT:   MQTTConfig{Broker: "tcp://192.168.1.178:1883", User: "hf", ClientID: "muehle-chaseq-ui"},
		Topics: TopicsConfig{Site: "muehle", Station: "hf", Chase: "chase", CQFinder: "cqfinder", Radio: "radio"},
		HTTP:   HTTPConfig{Listen: "127.0.0.1:8480"},
		Loggers: LoggersConfig{
			Log4OMAddr: "127.0.0.1:2241", DXLogAddr: "127.0.0.1:9889", Log4OMTimout: "500ms",
		},
		FiltersFile: "filters.json",
	}
}

// Load reads path (TOML) over the defaults, then applies env overrides.
// A missing path is not an error: the defaults are used.
func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		if _, err := toml.DecodeFile(path, &c); err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
		c.dir = filepath.Dir(path)
	}
	if v := os.Getenv(EnvPrefix + "MQTT_PASSWORD"); v != "" {
		c.MQTT.Password = v
	}
	if v := os.Getenv(EnvPrefix + "MQTT_BROKER"); v != "" {
		c.MQTT.Broker = v
	}
	if v := os.Getenv(EnvPrefix + "HTTP_LISTEN"); v != "" {
		c.HTTP.Listen = v
	}
	return c, nil
}

// Resolve returns p relative to the config directory when it is relative.
func (c Config) Resolve(p string) string {
	if p == "" || filepath.IsAbs(p) || c.dir == "" {
		return p
	}
	return filepath.Join(c.dir, p)
}

// String redacts the password.
func (c Config) String() string {
	c.MQTT.Password = redact(c.MQTT.Password)
	return fmt.Sprintf("%+v", struct {
		MQTT    MQTTConfig
		Topics  TopicsConfig
		HTTP    HTTPConfig
		Loggers LoggersConfig
	}{c.MQTT, c.Topics, c.HTTP, c.Loggers})
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	return strings.Repeat("*", 8)
}

// Topic builders.

func (c Config) slot(slot, suffix string) string {
	return c.Topics.Site + "/" + c.Topics.Station + "/" + slot + "/" + suffix
}

// ChaseState etc. return the full topic names.
func (c Config) ChaseState() string    { return c.slot(c.Topics.Chase, "state") }
func (c Config) ChaseStatus() string   { return c.slot(c.Topics.Chase, "status") }
func (c Config) ChaseCmd() string      { return c.slot(c.Topics.Chase, "cmd") }
func (c Config) CQFinderState() string { return c.slot(c.Topics.CQFinder, "state") }
func (c Config) RadioState() string    { return c.slot(c.Topics.Radio, "state") }
func (c Config) RadioCmd() string      { return c.slot(c.Topics.Radio, "cmd") }
