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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"station-log/internal/working/geo"
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
	DupWindow string      `toml:"dup_window"`
	Slot      SlotConfig  `toml:"slot"`
	MQTT      MQTTConfig  `toml:"mqtt"`
	HTTP      HTTPConfig  `toml:"http"`
	Log4OM    Log4OMUDP   `toml:"log4om"`
	Watch     WatchConfig `toml:"watch"`
	Working   Working     `toml:"working"`
	Wavelog   Wavelog     `toml:"wavelog"`
	Log       LogConfig   `toml:"log"`
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

// WatchConfig is the ADIF drop-directory feeder; off unless Dir is set.
type WatchConfig struct {
	Dir             string `toml:"dir"`
	IntervalSeconds int    `toml:"interval_seconds"`
	Source          string `toml:"source"`
}

// Working is the "currently working station" feed (retained topic
// <slot>/working): UDP listeners for the shack loggers, resolved against the
// station locator and, optionally, QRZ.com. Off while no listener is set.
type Working struct {
	// StationLocator is the shack QTH (Maidenhead), the same QTH the loggers
	// are configured with: it turns DXLog's azimuth+distance into a map
	// position and a locator into a beam bearing.
	StationLocator string `toml:"station_locator"`
	// StaleAfter is the UDP-silence window after which device_online drops.
	StaleAfter string            `toml:"stale_after"`
	Listeners  []WorkingListener `toml:"listener"`
	QRZ        WorkingQRZ        `toml:"qrz"`
}

// WorkingListener is one UDP listener decoding one logger's stream.
type WorkingListener struct {
	// Name is the source in logs and in selected.source ("dxlog", "log4om").
	Name string `toml:"name"`
	// Kind selects the decoder: "n1mm" (DXLog / N1MM lookupinfo) or "log4om"
	// (Log4OM outbound CALLSIGN, the bare call).
	Kind      string   `toml:"kind"`
	Listen    string   `toml:"listen"`
	AllowFrom []string `toml:"allow_from"`
}

// WorkingQRZ enables the QRZ.com lookup (a paid XML subscription) for name,
// country and city, and for a position the logger and the own log lack.
// The password is env-only (STATION_LOG_QRZ_PASSWORD). Off while User is empty.
type WorkingQRZ struct {
	User            string `toml:"user"`
	Password        string `toml:"-"`
	CacheDays       int    `toml:"cache_days"`
	NegativeMinutes int    `toml:"negative_minutes"`
}

// Enabled reports whether the working feed runs.
func (w Working) Enabled() bool { return len(w.Listeners) > 0 }

// Stale is the parsed StaleAfter.
func (w Working) Stale() time.Duration {
	d, _ := time.ParseDuration(w.StaleAfter)
	return d
}

// Wavelog is the two-way sync with a Wavelog instance (off by default). The
// API key is env-only (STATION_LOG_WAVELOG_KEY).
type Wavelog struct {
	// Direction: "off" (default), "pull", "push" or "both".
	Direction string `toml:"direction"`
	// URL is the instance base URL without /index.php.
	URL             string `toml:"url"`
	IntervalSeconds int    `toml:"interval_seconds"`
	FullResyncHours int    `toml:"full_resync_hours"`
	BatchSize       int    `toml:"batch_size"`
	PullLimit       int    `toml:"pull_limit"`
	// PushSources: source-name prefixes eligible for push.
	PushSources []string `toml:"push_sources"`
	// PushSince (RFC 3339 date or time): only QSOs at or after it are pushed.
	// Empty = the time sync was first enabled, so history is not bulk-pushed.
	PushSince     string `toml:"push_since"`
	RepushChanges bool   `toml:"repush_changes"`
	// CAFile is an extra CA bundle for a private CA.
	CAFile   string           `toml:"ca_file"`
	Key      string           `toml:"-"` // env only: STATION_LOG_WAVELOG_KEY
	Profiles []WavelogProfile `toml:"profile"`
}

// WavelogProfile maps an own callsign to a Wavelog station profile id.
type WavelogProfile struct {
	OwnCall          string `toml:"own_call"`
	StationProfileID string `toml:"station_profile_id"`
}

// Enabled reports whether any Wavelog sync runs.
func (w Wavelog) Enabled() bool { return w.Direction != "" && w.Direction != "off" }

// Pull and Push report which directions run.
func (w Wavelog) Pull() bool { return w.Direction == "pull" || w.Direction == "both" }
func (w Wavelog) Push() bool { return w.Direction == "push" || w.Direction == "both" }

// PushSinceTime is the parsed PushSince (zero when unset).
func (w Wavelog) PushSinceTime() time.Time {
	t, _ := parseSince(w.PushSince)
	return t
}

func parseSince(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", s)
	return t.UTC(), err
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
		Watch:     WatchConfig{IntervalSeconds: 30, Source: "adif-drop"},
		Working:   Working{StaleAfter: "10m", QRZ: WorkingQRZ{CacheDays: 30, NegativeMinutes: 10}},
		Wavelog: Wavelog{Direction: "off", IntervalSeconds: 300, FullResyncHours: 24, BatchSize: 50, PullLimit: 500,
			PushSources: []string{"log4om", "adif"}},
		Log: LogConfig{Level: "info"},
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
	if v := os.Getenv("STATION_LOG_QRZ_PASSWORD"); v != "" {
		cfg.Working.QRZ.Password = v
	}
	if v := os.Getenv("STATION_LOG_WAVELOG_KEY"); v != "" {
		cfg.Wavelog.Key = v
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
	if c.Watch.Dir != "" {
		if c.Watch.IntervalSeconds < 1 {
			return errors.New("watch.interval_seconds must be at least 1")
		}
		if strings.TrimSpace(c.Watch.Source) == "" {
			return errors.New("watch.source must not be empty")
		}
		// a directory that does not exist (yet) is only a warning at run time
	}
	if err := c.validateWorking(); err != nil {
		return err
	}
	return c.validateWavelog()
}

func (c Config) validateWavelog() error {
	w := c.Wavelog
	switch w.Direction {
	case "", "off":
		return nil
	case "pull", "push", "both":
	default:
		return fmt.Errorf("wavelog.direction %q: want off, pull, push or both", w.Direction)
	}
	u, err := url.Parse(w.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("wavelog.url %q: want an https:// base URL", w.URL)
	}
	if u.Scheme == "http" && !privateHost(u.Hostname()) {
		return fmt.Errorf("wavelog.url %q: plain http is only allowed for loopback or private hosts (the API key travels in the request body)", w.URL)
	}
	if w.Key == "" {
		return errors.New("wavelog needs STATION_LOG_WAVELOG_KEY in the environment (never in the TOML)")
	}
	if len(w.Profiles) == 0 {
		return errors.New("wavelog needs at least one [[wavelog.profile]] (own_call + station_profile_id)")
	}
	seen := map[string]bool{}
	for i, p := range w.Profiles {
		if strings.TrimSpace(p.OwnCall) == "" || strings.TrimSpace(p.StationProfileID) == "" {
			return fmt.Errorf("wavelog.profile[%d]: own_call and station_profile_id are required", i)
		}
		k := strings.ToUpper(strings.TrimSpace(p.OwnCall))
		if seen[k] {
			return fmt.Errorf("wavelog.profile[%d]: duplicate own_call %q", i, p.OwnCall)
		}
		seen[k] = true
	}
	if w.IntervalSeconds < 60 {
		return errors.New("wavelog.interval_seconds must be at least 60")
	}
	if w.FullResyncHours < 1 || w.BatchSize < 1 || w.PullLimit < 1 {
		return errors.New("wavelog.full_resync_hours, batch_size and pull_limit must be at least 1")
	}
	if _, err := parseSince(w.PushSince); err != nil {
		return fmt.Errorf("wavelog.push_since %q: want an RFC 3339 date or time", w.PushSince)
	}
	return nil
}

// privateHost reports a loopback or RFC 1918 / ULA / link-local host (a name
// counts only for "localhost").
func privateHost(h string) bool {
	if h == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && (a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast())
}

// WavelogCheck describes the Wavelog sync for -check; the key is never shown.
func (c Config) WavelogCheck() string {
	w := c.Wavelog
	if !w.Enabled() {
		return "wavelog sync off"
	}
	return fmt.Sprintf("wavelog sync %s  url %s  key set  %d profile(s)  interval %ds  push_since %q",
		w.Direction, w.URL, len(w.Profiles), w.IntervalSeconds, w.PushSince)
}

func (c Config) validateWorking() error {
	w := c.Working
	if w.StationLocator != "" {
		if _, ok := geo.LocatorToLatLng(w.StationLocator); !ok {
			return fmt.Errorf("working.station_locator: malformed Maidenhead locator %q", w.StationLocator)
		}
	}
	if d, err := time.ParseDuration(w.StaleAfter); err != nil || d < time.Second {
		return fmt.Errorf("working.stale_after %q: want a duration of at least 1s", w.StaleAfter)
	}
	if w.QRZ.User != "" {
		if w.QRZ.Password == "" {
			return errors.New("working.qrz.user needs STATION_LOG_QRZ_PASSWORD in the environment")
		}
		if w.QRZ.CacheDays < 1 || w.QRZ.NegativeMinutes < 1 {
			return errors.New("working.qrz.cache_days and negative_minutes must be at least 1")
		}
	}
	names, addrs := map[string]bool{}, map[string]bool{}
	if c.Log4OM.Listen != "" {
		addrs[c.Log4OM.Listen] = true
	}
	for i, l := range w.Listeners {
		id := fmt.Sprintf("working.listener[%d]", i)
		switch l.Kind {
		case "n1mm", "log4om":
		default:
			return fmt.Errorf("%s: kind must be \"n1mm\" or \"log4om\" (got %q)", id, l.Kind)
		}
		if l.Name == "" {
			return fmt.Errorf("%s: name is required (it becomes selected.source)", id)
		}
		if names[l.Name] {
			return fmt.Errorf("%s: duplicate name %q", id, l.Name)
		}
		names[l.Name] = true
		if _, port, err := net.SplitHostPort(l.Listen); err != nil || port == "" || port == "0" {
			return fmt.Errorf("%s %q: want host:port", id, l.Listen)
		}
		if addrs[l.Listen] {
			return fmt.Errorf("%s: %q is already used by another listener", id, l.Listen)
		}
		addrs[l.Listen] = true
		if len(l.AllowFrom) == 0 {
			return fmt.Errorf("%s: allow_from is required (the listeners are LAN-facing)", id)
		}
		if err := checkAllow(id+".allow_from", l.AllowFrom); err != nil {
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

// QRZCachePath is the QRZ lookup cache file (under data_dir; personal-ish data, never in git).
func (c Config) QRZCachePath() string { return filepath.Join(c.DataDir, "qrz-cache.json") }

// WorkingCheck describes the working feed for -check; secrets are redacted.
func (c Config) WorkingCheck() []string {
	w := c.Working
	if !w.Enabled() {
		return []string{"working feed off (no [[working.listener]])"}
	}
	out := []string{fmt.Sprintf("working feed on  topic <slot>/working  station_locator %q  stale_after %s", w.StationLocator, w.StaleAfter)}
	for _, l := range w.Listeners {
		out = append(out, fmt.Sprintf("  listener %s kind %s listen %s allow_from %v", l.Name, l.Kind, l.Listen, l.AllowFrom))
	}
	if w.QRZ.User != "" {
		out = append(out, fmt.Sprintf("  qrz user %q password set cache %s (%d d, negative %d min)", w.QRZ.User, c.QRZCachePath(), w.QRZ.CacheDays, w.QRZ.NegativeMinutes))
	} else {
		out = append(out, "  qrz off (own-log grid only)")
	}
	return out
}
