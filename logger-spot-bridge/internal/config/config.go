// Package config holds runtime configuration for logger-spot-bridge.
//
// logger-spot-bridge listens on the shack PC next to the logging software
// (Log4OM, DXLog) for the loggers' operator-entered-callsign UDP broadcasts
// and publishes the selected station to the canonical
// <site>/<station>/<slot> spot slot. This package defines the configuration
// shape, defaults and loading (TOML file + flags + env overrides for the
// MQTT and QRZ passwords).
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"logger-spot-bridge/internal/geo"
)

// Config is the top-level configuration.
type Config struct {
	// Host is the compute node the adapter runs on (model §3), published in
	// /meta. The loggers run on the shack PC, so that is the default.
	Host string `toml:"host"`
	// StationLocator is the shack QTH as a Maidenhead locator — the same QTH
	// the loggers are configured with. DXLog's azimuth/distance are computed
	// from the logging PC's position; reusing the QTH here is what makes the
	// direct problem (az+dist → pin) land on the right spot on the map.
	StationLocator string `toml:"station_locator"`
	// Slot is the canonical slot address under the site.
	Slot SlotConfig `toml:"slot"`
	// Listeners is the set of UDP listeners, one per logger broadcast stream.
	Listeners []ListenerConfig `toml:"listener"`
	// StaleAfter is the UDP-silence window after which device_online drops
	// (the two-layer liveness model: /status is the bridge, device_online is
	// the logger feed). Loggers only broadcast when the operator types or
	// (RadioInfo) the radio moves, so the default is generous.
	StaleAfter time.Duration `toml:"stale_after"`
	QRZ        QRZConfig     `toml:"qrz"`
	MQTT       MQTTConfig    `toml:"mqtt"`
	Log        LogConfig     `toml:"log"`
}

// SlotConfig is the canonical slot address under the site.
type SlotConfig struct {
	Station string `toml:"station"`
	Slot    string `toml:"slot"`
}

// ListenerConfig is one UDP listener decoding one logger's broadcast stream.
type ListenerConfig struct {
	// Name identifies the source in logs and in the published `selected.source`
	// field ("dxlog", "log4om", …).
	Name string `toml:"name"`
	// Kind selects the decoder: "n1mm" (DXLog / N1MM-family XML) or "log4om"
	// (Log4OM outbound CALLSIGN).
	Kind string `toml:"kind"`
	// Port is the UDP port to bind. N1MM-family default 12060.
	Port int `toml:"port"`
}

// MQTTConfig holds broker connection settings.
type MQTTConfig struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	User     string `toml:"user"`
	Password string `toml:"password"`
	Site     string `toml:"site"`
}

// QRZConfig enables the QRZ.com callsign→position lookup that fills the
// selected-station record when the logger provides no location (Log4OM's
// outbound CALLSIGN datagram is the bare callsign — see internal/log4om).
// Requires a paid QRZ subscription with XML access. The QRZ password is
// env-only (LOGGER_SPOT_BRIDGE_QRZ_PASSWORD), per the seed-once convention.
type QRZConfig struct {
	// Enabled turns the lookup on. Without it the record is call+RF only
	// when the logger sends no position (previous behavior).
	Enabled bool `toml:"enabled"`
	// Username is the QRZ.com login (NOT secret; the password is the secret).
	Username string `toml:"username"`
	// Password is set only through the env overlay — never the TOML.
	Password string `toml:"-"`
	// CachePath is the lookup-cache file. Empty resolves to qrz-cache.json
	// next to the config file.
	CachePath string `toml:"cache_path"`
	// CacheDays is the positive-cache TTL in days (QRZ data moves at
	// renewal/relocation speed). 0 → 30.
	CacheDays int `toml:"cache_days"`
	// NegativeMinutes caches "not found" answers so a mistyped call does
	// not re-hit the API on every keystroke. 0 → 10.
	NegativeMinutes int `toml:"negative_minutes"`
}

// LogConfig controls logging verbosity.
type LogConfig struct {
	Level string `toml:"level"`
}

// Defaults returns a Config with sensible default values: one N1MM-family
// listener on :12060 (the N1MM default DXLog also offers), slot hf/spots.
func Defaults() Config {
	return Config{
		Host:       "shack-pc",
		StaleAfter: 10 * time.Minute,
		Slot:       SlotConfig{Station: "hf", Slot: "spots"},
		Listeners:  []ListenerConfig{{Name: "dxlog", Kind: "n1mm", Port: 12060}},
		QRZ:        QRZConfig{CacheDays: 30, NegativeMinutes: 10},
		MQTT: MQTTConfig{
			Broker: "tcp://hassio.kgbvax.net:1883",
			User:   "hf",
			Site:   "muehle",
		},
		Log: LogConfig{Level: "info"},
	}
}

// Flags describes the command-line flags.
type Flags struct {
	ConfigPath string
	LogLevel   string
}

// RegisterFlags wires the flags onto fs. The default config path is next to
// the exe (the shack PC runs it interactively from its own directory — no
// /etc on Windows); an explicit -config is honoured for tests.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	fs.StringVar(&f.ConfigPath, "config", "", "path to config.toml (default: next to the executable)")
	fs.StringVar(&f.LogLevel, "log.level", "", "log level (debug|info|warn|error); overrides config")
	return &f
}

// Load reads the TOML config file (path resolution: explicit flag → next to
// the executable → none), applies defaults and env overrides. A missing
// implicit config file is NOT an error (defaults carry the bridge); an
// explicitly named file that cannot be read IS (pelcobridge2's config rule).
func Load(f *Flags) (Config, error) {
	cfg := Defaults()

	path := f.ConfigPath
	if path == "" {
		if exe, err := os.Executable(); err == nil {
			path = filepath.Join(filepath.Dir(exe), "config.toml")
		}
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			if _, err := toml.Decode(string(data), &cfg); err != nil {
				return Config{}, fmt.Errorf("decode %s: %w", path, err)
			}
		} else if f.ConfigPath != "" {
			// Explicitly named: an unreadable config is fatal, not a fallback.
			return Config{}, fmt.Errorf("read %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if f.LogLevel != "" {
		cfg.Log.Level = strings.ToLower(f.LogLevel)
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.MQTT.Site == "" {
		cfg.MQTT.Site = "muehle"
	}
	if cfg.MQTT.Broker == "" {
		cfg.MQTT.Broker = "tcp://hassio.kgbvax.net:1883"
	}
	if cfg.Host == "" {
		cfg.Host = "shack-pc"
	}
	if cfg.Slot.Station == "" {
		cfg.Slot.Station = "hf"
	}
	if cfg.Slot.Slot == "" {
		cfg.Slot.Slot = "spots"
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = 10 * time.Minute
	}
	if cfg.QRZ.CacheDays <= 0 {
		cfg.QRZ.CacheDays = 30
	}
	if cfg.QRZ.NegativeMinutes <= 0 {
		cfg.QRZ.NegativeMinutes = 10
	}
	if cfg.QRZ.Enabled {
		// Resolve the cache default next to the config file (the shack PC
		// runs interactively from its own directory — no /var/lib).
		if cfg.QRZ.CachePath == "" {
			base := "config.toml"
			if path != "" {
				base = path
			}
			cfg.QRZ.CachePath = filepath.Join(filepath.Dir(base), "qrz-cache.json")
		}
		// Fail fast: credentials arrive via TOML username + env password
		// (seed-once). Launching "enabled" without them would only produce
		// auth-failure spam on the first keyed call.
		if cfg.QRZ.Username == "" || cfg.QRZ.Password == "" {
			return Config{}, fmt.Errorf("qrz.enabled requires qrz.username and LOGGER_SPOT_BRIDGE_QRZ_PASSWORD in the environment")
		}
	}
	if len(cfg.Listeners) == 0 {
		cfg.Listeners = []ListenerConfig{{Name: "dxlog", Kind: "n1mm", Port: 12060}}
	}
	for i := range cfg.Listeners {
		l := &cfg.Listeners[i]
		if l.Name == "" {
			l.Name = l.Kind
		}
		if l.Port == 0 {
			l.Port = 12060
		}
		switch l.Kind {
		case "n1mm", "log4om":
		default:
			return Config{}, fmt.Errorf("listener[%d] (%s): kind must be \"n1mm\" or \"log4om\" (got %q)", i, l.Name, l.Kind)
		}
	}
	if cfg.StationLocator != "" {
		// Fail fast on a malformed locator: a wrong shack position silently
		// misplaces every DXLog-derived pin on the map.
		if _, ok := geo.LocatorToLatLng(cfg.StationLocator); !ok {
			return Config{}, fmt.Errorf("station_locator: malformed Maidenhead locator %q", cfg.StationLocator)
		}
	}
	return cfg, nil
}

// applyEnv overlays LOGGER_SPOT_BRIDGE_* env vars on top of cfg (the seed-once
// workflow: the password never lives in the TOML).
func applyEnv(cfg *Config) {
	if v := os.Getenv("LOGGER_SPOT_BRIDGE_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv("LOGGER_SPOT_BRIDGE_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTT.ClientID = v
	}
	if v := os.Getenv("LOGGER_SPOT_BRIDGE_MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv("LOGGER_SPOT_BRIDGE_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv("LOGGER_SPOT_BRIDGE_MQTT_SITE"); v != "" {
		cfg.MQTT.Site = v
	}
	if v := os.Getenv("LOGGER_SPOT_BRIDGE_QRZ_USERNAME"); v != "" {
		cfg.QRZ.Username = v
	}
	if v := os.Getenv("LOGGER_SPOT_BRIDGE_QRZ_PASSWORD"); v != "" {
		cfg.QRZ.Password = v
	}
}
