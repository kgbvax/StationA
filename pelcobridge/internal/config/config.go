// Package config loads and persists pelcots settings as YAML. Missing files
// resolve to Default(), so first runs work without a config present.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Transport names the outbound link to the PTZ.
const (
	TransportSerial = "serial"
	TransportTCP    = "tcp"
)

// LogLevel selects how much detail is recorded in the trace/log. Ordered from
// quietest (LogError) to most verbose (LogTrace); a line is emitted when its
// level is at or below the configured level.
type LogLevel int8

const (
	LogError LogLevel = iota // failures that abort an operation
	LogWarn                  // recoverable problems (read errors, TX while disconnected)
	LogInfo                  // operational milestones (connect, server start/stop)
	LogDebug                 // per-frame TX and decoded RX readback
	LogTrace                 // raw bytes and unrecognized frames
)

var logLevelNames = [...]string{
	LogError: "error",
	LogWarn:  "warn",
	LogInfo:  "info",
	LogDebug: "debug",
	LogTrace: "trace",
}

// String returns the lowercase level name (also its YAML/flag representation).
func (l LogLevel) String() string {
	if l < 0 || int(l) >= len(logLevelNames) {
		return "info"
	}
	return logLevelNames[l]
}

// ParseLogLevel maps a name (case-insensitive) to a LogLevel.
func ParseLogLevel(s string) (LogLevel, error) {
	want := strings.ToLower(strings.TrimSpace(s))
	for i, name := range logLevelNames {
		if name == want {
			return LogLevel(i), nil
		}
	}
	return LogInfo, fmt.Errorf("invalid log level %q (want error|warn|info|debug|trace)", s)
}

// MarshalYAML writes the level as its name so the config stays human-readable.
func (l LogLevel) MarshalYAML() (any, error) { return l.String(), nil }

// UnmarshalYAML accepts the level name; an empty/missing value keeps the zero
// value (the caller seeds it from Default first).
func (l *LogLevel) UnmarshalYAML(n *yaml.Node) error {
	if n.Value == "" {
		return nil
	}
	lvl, err := ParseLogLevel(n.Value)
	if err != nil {
		return err
	}
	*l = lvl
	return nil
}

// Config is the full persisted application state.
type Config struct {
	Transport string        `yaml:"transport"` // serial | tcp
	Serial    SerialConfig  `yaml:"serial"`
	TCP       TCPConfig     `yaml:"tcp"`
	Addr      byte          `yaml:"addr"`      // Pelco-D camera address (1-255)
	Log       string        `yaml:"log"`       // TX/RX trace file ("" disables)
	LogLevel  LogLevel      `yaml:"log_level"` // error | warn | info | debug | trace
	Control   ControlConfig `yaml:"control"`
	Wrap      WrapConfig    `yaml:"wrap"`
}

// SerialConfig holds the directly attached serial parameters.
type SerialConfig struct {
	Port string `yaml:"port"`
	Baud int    `yaml:"baud"`
}

// TCPConfig holds the serial-to-TCP bridge endpoint ("host:port").
type TCPConfig struct {
	Address string `yaml:"address"`
}

// ControlConfig configures the optional inbound network-control servers.
type ControlConfig struct {
	Bind    string       `yaml:"bind"` // listen address for inbound servers
	GS232   ServerConfig `yaml:"gs232"`
	Rotctld ServerConfig `yaml:"rotctld"`
}

// ServerConfig is one inbound protocol server's settings.
type ServerConfig struct {
	Enabled bool `yaml:"enabled"`
	Port    int  `yaml:"port"`
}

// WrapConfig configures cable-wrap protection for infinite-azimuth rotators.
type WrapConfig struct {
	Enabled     bool    `yaml:"enabled"`
	Limit       float64 `yaml:"limit"`       // ± degrees of permitted wind
	Accumulated float64 `yaml:"accumulated"` // signed wind state, persisted across runs
}

// Default returns the built-in defaults (mirrors the original flag defaults).
// Inbound servers are disabled and bound to localhost for least-privilege.
func Default() Config {
	return Config{
		Transport: TransportSerial,
		Serial:    SerialConfig{Port: "/dev/tty.usbmodem5AF50020681", Baud: 2400},
		TCP:       TCPConfig{Address: "127.0.0.1:4001"},
		Addr:      1,
		Log:       "pelcots.log",
		LogLevel:  LogInfo,
		Control: ControlConfig{
			Bind:    "127.0.0.1",
			GS232:   ServerConfig{Enabled: false, Port: 4000},
			Rotctld: ServerConfig{Enabled: false, Port: 4533},
		},
		Wrap: WrapConfig{Enabled: false, Limit: 270, Accumulated: 0},
	}
}

// Load reads and parses the YAML config at path. A missing file is not an
// error: it returns Default() so a first run works out of the box.
func Load(path string) (Config, error) {
	c := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Default(), fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// Save writes c to path as YAML (0o644).
func Save(path string, c Config) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
