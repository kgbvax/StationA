// Package config holds runtime configuration for oscarwatch-sattrack-bridge:
// the TOML shape, defaults, flags and the OSCARWATCH_SATTRACK_BRIDGE_* env
// overlay (the MQTT password is env-only by convention — see
// ../docs/conventions/config-and-secrets.md).
package config

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"oscarwatch-sattrack-bridge/internal/geo"
)

// EnvPrefix is the directory name uppercased, hyphens → underscores (naming
// convention).
const EnvPrefix = "OSCARWATCH_SATTRACK_BRIDGE_"

// Config is the top-level configuration.
type Config struct {
	// Host is the compute node the bridge runs on, published in /meta.
	Host string `toml:"host"`
	// Location is the physical-location label published in /meta.
	Location string        `toml:"location"`
	Slot     SlotConfig    `toml:"slot"`
	Source   SourceConfig  `toml:"source"`
	Station  StationConfig `toml:"station"`
	MQTT     MQTTConfig    `toml:"mqtt"`
	Log      LogConfig     `toml:"log"`
}

// SlotConfig is the canonical slot address under the site.
type SlotConfig struct {
	Station string `toml:"station"`
	Slot    string `toml:"slot"`
}

// SourceConfig addresses OscarWatch's Satellite-link WebSocket server.
type SourceConfig struct {
	// URL is ws://<OscarWatch PC>:<port>/ (OscarWatch default port 7373;
	// "Allow connections from local network" must be on for a LAN client).
	URL string `toml:"url"`
	// PingInterval is the WebSocket keepalive cadence; the link counts as
	// dead after 2.5× without a pong or frame. 0 disables keepalive.
	PingInterval time.Duration `toml:"ping_interval"`
}

// StationConfig is the observer position the sub-satellite point is derived
// from. Explicit lat/lon win; otherwise the locator's cell centre is used
// (a 6-character locator is within ~3 km — fine for a map pin). Neither set
// → the bridge publishes look angles without a sub-point.
type StationConfig struct {
	Locator string   `toml:"locator"`
	Lat     *float64 `toml:"lat"`
	Lon     *float64 `toml:"lon"`
	AltM    float64  `toml:"alt_m"`
}

// MQTTConfig holds broker connection settings.
type MQTTConfig struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	User     string `toml:"user"`
	Password string `toml:"password"`
	Site     string `toml:"site"`
}

// LogConfig controls logging verbosity.
type LogConfig struct {
	Level string `toml:"level"`
}

// Defaults returns the station defaults: slot uhf/sat-track, OscarWatch on
// BWPC, the live hassio broker.
func Defaults() Config {
	return Config{
		Host:     "scmino",
		Location: "bauwagen",
		Slot:     SlotConfig{Station: "uhf", Slot: "sat-track"},
		Source:   SourceConfig{URL: "ws://192.168.1.197:7373/", PingInterval: 30 * time.Second},
		MQTT: MQTTConfig{
			Broker: "tcp://192.168.1.50:1883",
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

// RegisterFlags wires the flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	fs.StringVar(&f.ConfigPath, "config", "", "path to config.toml (default: next to the executable)")
	fs.StringVar(&f.LogLevel, "log.level", "", "log level (debug|info|warn|error); overrides config")
	return &f
}

// Load reads the TOML config (explicit -config → next to the executable →
// none), overlays the env and validates. A missing implicit file is fine
// (defaults carry the bridge); an explicitly named unreadable file is fatal.
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
			return Config{}, fmt.Errorf("read %s: %w", path, err)
		}
	}

	applyEnv(&cfg)
	if f.LogLevel != "" {
		cfg.Log.Level = f.LogLevel
	}
	fillDefaults(&cfg)
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// fillDefaults restores defaults a config file blanked out.
func fillDefaults(cfg *Config) {
	d := Defaults()
	cfg.Log.Level = strings.ToLower(cfg.Log.Level)
	if cfg.Log.Level == "" {
		cfg.Log.Level = d.Log.Level
	}
	if cfg.MQTT.Site == "" {
		cfg.MQTT.Site = d.MQTT.Site
	}
	if cfg.MQTT.Broker == "" {
		cfg.MQTT.Broker = d.MQTT.Broker
	}
	if cfg.Host == "" {
		cfg.Host = d.Host
	}
	if cfg.Location == "" {
		cfg.Location = d.Location
	}
	if cfg.Slot.Station == "" {
		cfg.Slot.Station = d.Slot.Station
	}
	if cfg.Slot.Slot == "" {
		cfg.Slot.Slot = d.Slot.Slot
	}
	if cfg.Source.URL == "" {
		cfg.Source.URL = d.Source.URL
	}
	if cfg.MQTT.ClientID == "" {
		cfg.MQTT.ClientID = cfg.MQTT.Site + "-" + cfg.Slot.Station + "-" + cfg.Slot.Slot
	}
}

func validate(cfg Config) error {
	u, err := url.Parse(cfg.Source.URL)
	if err != nil {
		return fmt.Errorf("source.url %q: %w", cfg.Source.URL, err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return fmt.Errorf("source.url %q: scheme must be ws or wss", cfg.Source.URL)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("source.url %q has no host", cfg.Source.URL)
	}
	if cfg.Source.PingInterval < 0 {
		return fmt.Errorf("source.ping_interval must not be negative")
	}
	if cfg.Source.PingInterval > 0 && cfg.Source.PingInterval < time.Second {
		return fmt.Errorf("source.ping_interval %v is too short (min 1s, 0 disables)", cfg.Source.PingInterval)
	}
	st := cfg.Station
	if (st.Lat == nil) != (st.Lon == nil) {
		return fmt.Errorf("station: set both lat and lon, or neither")
	}
	if st.Lat != nil {
		if *st.Lat < -90 || *st.Lat > 90 {
			return fmt.Errorf("station.lat %v out of range", *st.Lat)
		}
		if *st.Lon < -180 || *st.Lon > 180 {
			return fmt.Errorf("station.lon %v out of range", *st.Lon)
		}
	}
	if st.Locator != "" {
		// A wrong QTH silently misplaces every sub-satellite point.
		if _, ok := geo.LocatorToLatLng(st.Locator); !ok {
			return fmt.Errorf("station.locator: malformed Maidenhead locator %q", st.Locator)
		}
	}
	switch cfg.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q: want debug, info, warn or error", cfg.Log.Level)
	}
	return nil
}

// Observer resolves the station position for the sub-satellite point, and
// describes where it came from (for the startup log and -check). nil when
// neither lat/lon nor a locator is configured.
func (c Config) Observer() (*geo.Observer, string) {
	st := c.Station
	if st.Lat != nil && st.Lon != nil {
		return &geo.Observer{Lat: *st.Lat, Lng: *st.Lon, AltM: st.AltM},
			fmt.Sprintf("lat/lon %.5f,%.5f alt %.0f m", *st.Lat, *st.Lon, st.AltM)
	}
	if ll, ok := geo.LocatorToLatLng(st.Locator); ok {
		return &geo.Observer{Lat: ll.Lat, Lng: ll.Lng, AltM: st.AltM},
			fmt.Sprintf("locator %s (%.4f,%.4f)", strings.ToUpper(st.Locator), ll.Lat, ll.Lng)
	}
	return nil, "unset — no sub-satellite point"
}

// applyEnv overlays OSCARWATCH_SATTRACK_BRIDGE_* on cfg (seed-once: the
// password never lives in the TOML).
func applyEnv(cfg *Config) {
	for key, dst := range map[string]*string{
		"MQTT_BROKER":    &cfg.MQTT.Broker,
		"MQTT_CLIENT_ID": &cfg.MQTT.ClientID,
		"MQTT_USER":      &cfg.MQTT.User,
		"MQTT_PASSWORD":  &cfg.MQTT.Password,
		"MQTT_SITE":      &cfg.MQTT.Site,
		"SOURCE_URL":     &cfg.Source.URL,
	} {
		if v := os.Getenv(EnvPrefix + key); v != "" {
			*dst = v
		}
	}
}
