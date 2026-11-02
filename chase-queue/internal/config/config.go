// Package config holds chase-queue's runtime configuration: one TOML file
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

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration.
type Config struct {
	// Host is the compute node, published in /meta.
	Host string `toml:"host"`
	// DataDir holds the SQLite database, uploaded contest files, cty.dat
	// and cty.csv.
	DataDir string      `toml:"data_dir"`
	Slot    SlotConfig  `toml:"slot"`
	MQTT    MQTTConfig  `toml:"mqtt"`
	HTTP    HTTPConfig  `toml:"http"`
	Finder  FinderSlot  `toml:"cqfinder"`
	Contest ContestUDP  `toml:"contest"`
	Log4OM  Log4OMUDP   `toml:"log4om"`
	ClubLog ClubLogConf `toml:"clublog"`
	QRZ     QRZConf     `toml:"qrz"`
	Log     LogConfig   `toml:"log"`
}

// SlotConfig is chase-queue's own slot under the site.
type SlotConfig struct {
	Station string `toml:"station"`
	Slot    string `toml:"slot"`
}

// FinderSlot is bmrec's candidate slot.
type FinderSlot struct {
	Station string `toml:"station"`
	Slot    string `toml:"slot"`
}

// MQTTConfig holds broker connection settings.
type MQTTConfig struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	User     string `toml:"user"`
	Password string `toml:"-"` // env only: CHASE_QUEUE_MQTT_PASSWORD
	Site     string `toml:"site"`
}

// HTTPConfig is the local HTTP API (contest files, health).
type HTTPConfig struct {
	Listen string `toml:"listen"`
}

// ContestUDP is the DXLog UDP feed (N1MM-style XML) used in contests.
type ContestUDP struct {
	Listen    string   `toml:"listen"`
	AllowFrom []string `toml:"allow_from"`
}

// Log4OMUDP is the Log4OM ADIF_MESSAGE feed used outside contests.
type Log4OMUDP struct {
	Listen    string   `toml:"listen"`
	AllowFrom []string `toml:"allow_from"`
}

// ClubLogConf is the Club Log source of confirmations.
type ClubLogConf struct {
	// Call is the logbook's callsign.
	Call string `toml:"call"`
	// Token is the personal API token; env only: CHASE_QUEUE_CLUBLOG_TOKEN.
	Token string `toml:"-"`
	// Confirmed lists the DXCC matrix status values that mean confirmed.
	Confirmed []int `toml:"confirmed_status"`
	// RefreshMinutes is how often the matrix is fetched (default 45).
	RefreshMinutes int `toml:"refresh_minutes"`
}

// QRZConf is the QRZ.com XML callbook login (name, QTH, locator, picture of
// the stations on the list). Full data needs a QRZ XML subscription; without
// one QRZ returns name and address only.
type QRZConf struct {
	// User is the QRZ login (a callsign).
	User string `toml:"user"`
	// Password is env only: CHASE_QUEUE_QRZ_PASSWORD.
	Password string `toml:"-"`
	// RefreshDays is how long a callbook entry is kept before it is looked
	// up again (default 30).
	RefreshDays int `toml:"refresh_days"`
}

// Enabled reports whether callbook lookups are configured.
func (q QRZConf) Enabled() bool { return q.User != "" && q.Password != "" }

// LogConfig controls logging.
type LogConfig struct {
	Level string `toml:"level"`
}

// Defaults returns the default configuration.
func Defaults() Config {
	return Config{
		Host:    "scmino",
		DataDir: "/var/lib/chase-queue",
		Slot:    SlotConfig{Station: "hf", Slot: "chase"},
		Finder:  FinderSlot{Station: "hf", Slot: "cqfinder"},
		MQTT:    MQTTConfig{Broker: "tcp://127.0.0.1:1883", Site: "muehle"},
		HTTP:    HTTPConfig{Listen: "127.0.0.1:8095"},
		ClubLog: ClubLogConf{Confirmed: []int{1, 3}, RefreshMinutes: 45},
		QRZ:     QRZConf{RefreshDays: 30},
		Log:     LogConfig{Level: "info"},
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
	fs.StringVar(&f.ConfigPath, "config", "/etc/chase-queue/config.toml", "path to config.toml")
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
	case !errors.Is(err, os.ErrNotExist) || f.ConfigPath != Defaults().configPath():
		return Config{}, fmt.Errorf("read %s: %w", f.ConfigPath, err)
	}
	if v := os.Getenv("CHASE_QUEUE_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv("CHASE_QUEUE_MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv("CHASE_QUEUE_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv("CHASE_QUEUE_CLUBLOG_TOKEN"); v != "" {
		cfg.ClubLog.Token = v
	}
	if v := os.Getenv("CHASE_QUEUE_QRZ_PASSWORD"); v != "" {
		cfg.QRZ.Password = v
	}
	if f.LogLevel != "" {
		cfg.Log.Level = strings.ToLower(f.LogLevel)
	}
	return cfg, cfg.Validate()
}

func (Config) configPath() string { return "/etc/chase-queue/config.toml" }

// Validate checks the configuration.
func (c Config) Validate() error {
	if c.DataDir == "" {
		return errors.New("data_dir is required")
	}
	if c.MQTT.Broker == "" || c.MQTT.Site == "" || c.Slot.Station == "" || c.Slot.Slot == "" || c.Finder.Station == "" || c.Finder.Slot == "" {
		return errors.New("mqtt.broker, mqtt.site, slot and cqfinder must be set")
	}
	if _, _, err := net.SplitHostPort(c.HTTP.Listen); err != nil {
		return fmt.Errorf("http.listen %q: want host:port", c.HTTP.Listen)
	}
	for name, u := range map[string]struct {
		listen string
		allow  []string
	}{"contest": {c.Contest.Listen, c.Contest.AllowFrom}, "log4om": {c.Log4OM.Listen, c.Log4OM.AllowFrom}} {
		if u.listen == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(u.listen); err != nil {
			return fmt.Errorf("%s.listen %q: want host:port", name, u.listen)
		}
		if len(u.allow) == 0 {
			return fmt.Errorf("%s.allow_from is required with %s.listen", name, name)
		}
		for _, a := range u.allow {
			if _, err := ParseAllowFrom(a); err != nil {
				return fmt.Errorf("%s.allow_from %q: %w", name, a, err)
			}
		}
	}
	if c.ClubLog.Token != "" && c.ClubLog.Call == "" {
		return errors.New("clublog.call is required with a Club Log token")
	}
	if c.QRZ.Password != "" && c.QRZ.User == "" {
		return errors.New("qrz.user is required with a QRZ password")
	}
	if c.QRZ.RefreshDays < 1 {
		return errors.New("qrz.refresh_days must be at least 1")
	}
	if len(c.ClubLog.Confirmed) == 0 {
		return errors.New("clublog.confirmed_status must not be empty")
	}
	if host, _, _ := net.SplitHostPort(c.HTTP.Listen); host != "127.0.0.1" && host != "::1" && host != "localhost" {
		return fmt.Errorf("http.listen %q: the upload API has no auth and must listen on loopback", c.HTTP.Listen)
	}
	if c.ClubLog.RefreshMinutes < 10 {
		return errors.New("clublog.refresh_minutes must be at least 10 (Club Log allows 1,000 requests a day)")
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
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "chase-queue.db") }
