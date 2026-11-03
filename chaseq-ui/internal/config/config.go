// Package config holds chaseq-ui's runtime configuration: a TOML file next to
// the executable, MQTT password included, as for qslotter.
package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

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
	Site    string `toml:"site"`
	Station string `toml:"station"`
	Chase   string `toml:"chase"` // chase-queue slot
	Radio   string `toml:"radio"` // flexbridge slot
}

// HTTPConfig is the loopback server the window loads.
type HTTPConfig struct {
	Listen string `toml:"listen"`
}

// LoggersConfig names where the callsign is sent: the [[loggers.sender]]
// entries, or, when there are none, the single Log4OM given by the legacy
// log4om_* keys.
type LoggersConfig struct {
	Log4OMAddr    string         `toml:"log4om_addr"`    // legacy: Remote Control Interface inbound UDP
	Log4OMTimeout string         `toml:"log4om_timeout"` // legacy: reply wait, Go duration
	Sender        []SenderConfig `toml:"sender"`
}

// SenderConfig is one [[loggers.sender]] entry.
type SenderConfig struct {
	Kind    string `toml:"kind"`    // "log4om"
	Addr    string `toml:"addr"`    // kind-specific address (log4om: inbound UDP host:port)
	Timeout string `toml:"timeout"` // reply wait, Go duration; "" = the kind's default
	Enabled *bool  `toml:"enabled"` // nil = enabled
}

// On reports whether the entry is enabled (the default).
func (s SenderConfig) On() bool { return s.Enabled == nil || *s.Enabled }

// Senders returns the configured logger senders. With no [[loggers.sender]]
// the legacy log4om_addr/log4om_timeout pair stands for one Log4OM entry.
func (l LoggersConfig) Senders() []SenderConfig {
	if len(l.Sender) > 0 {
		return l.Sender
	}
	if l.Log4OMAddr == "" {
		return nil
	}
	// the legacy timeout was parsed leniently: a bad one meant the default
	timeout := l.Log4OMTimeout
	if _, err := time.ParseDuration(timeout); err != nil {
		timeout = ""
	}
	return []SenderConfig{{Kind: "log4om", Addr: l.Log4OMAddr, Timeout: timeout}}
}

// LogConfig is file logging (the windowsgui exe has no stderr).
type LogConfig struct {
	File string `toml:"file"`
}

// Default returns the defaults.
func Default() Config {
	return Config{
		MQTT:   MQTTConfig{Broker: "tcp://192.168.1.178:1883", User: "hf", ClientID: "muehle-chaseq-ui"},
		Topics: TopicsConfig{Site: "muehle", Station: "hf", Chase: "chase", Radio: "radio"},
		HTTP:   HTTPConfig{Listen: "127.0.0.1:8480"},
		Loggers: LoggersConfig{
			Log4OMAddr: "127.0.0.1:2241", Log4OMTimeout: "500ms",
		},
		FiltersFile: "filters.json",
	}
}

// Load reads path (TOML) over the defaults,
// A missing path is not an error: the defaults are used.
func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		if _, err := toml.DecodeFile(path, &c); err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
		c.dir = filepath.Dir(path)
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
func (c Config) ChaseState() string  { return c.slot(c.Topics.Chase, "state") }
func (c Config) ChaseStatus() string { return c.slot(c.Topics.Chase, "status") }
func (c Config) ChaseCmd() string    { return c.slot(c.Topics.Chase, "cmd") }
func (c Config) RadioState() string  { return c.slot(c.Topics.Radio, "state") }
func (c Config) RadioCmd() string    { return c.slot(c.Topics.Radio, "cmd") }
