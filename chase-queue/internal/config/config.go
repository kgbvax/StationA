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
	"net/url"
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
	DataDir string     `toml:"data_dir"`
	Slot    SlotConfig `toml:"slot"`
	MQTT    MQTTConfig `toml:"mqtt"`
	HTTP    HTTPConfig `toml:"http"`
	Finder  FinderSlot `toml:"cqfinder"`
	Contest ContestUDP `toml:"contest"`
	Log4OM  Log4OMUDP  `toml:"log4om"`
	// StationLog is station-log's read API and event feed; the section being
	// absent keeps the Log4OM listener and the local log.
	StationLog StationLogConf `toml:"station_log"`
	ClubLog    ClubLogConf    `toml:"clublog"`
	QRZ        QRZConf        `toml:"qrz"`
	// Cluster is the DX clusters: [[cluster]] entries, or the deployed single
	// [cluster] table read as one entry named "cluster".
	Cluster ClusterList `toml:"cluster"`
	// Bmrec gates which of bmrec's rows are available stations.
	Bmrec BmrecConf `toml:"bmrec"`
	// Buddies are stations the operator wants to be told about.
	Buddies []BuddyConf `toml:"buddy"`
	// Awards locates the award definition files; Goals switch awards on.
	Awards AwardsConf `toml:"awards"`
	Goals  []GoalConf `toml:"goal"`
	Log    LogConfig  `toml:"log"`
}

// AwardsConf is where the award definitions (TOML files, see
// docs/chase-queue-mqtt-api.md) are read from, once at start.
type AwardsConf struct {
	// Dir holds *.toml definition files; default <data_dir>/awards. A
	// missing directory leaves only the built-in DXCC award.
	Dir string `toml:"dir"`
}

// GoalConf is a [[goal]]: it enables an award, or turns one off, and ranks it
// against the others. DXCC is a goal unless one turns it off; the running
// contest is a goal while an instance runs (award "contest"); any other
// award is a goal only when a [[goal]] enables it.
type GoalConf struct {
	// Award is a definition's code or "contest".
	Award string `toml:"award"`
	// Priority ranks the awards (higher first); 0 = the default: 100 for the
	// contest, 10 for DXCC, 50 for any other.
	Priority int `toml:"priority"`
	// Enabled defaults to true.
	Enabled *bool `toml:"enabled"`
}

// IsEnabled reports whether the goal is on.
func (g GoalConf) IsEnabled() bool { return g.Enabled == nil || *g.Enabled }

// AwardsDir is the directory of award definitions.
func (c Config) AwardsDir() string {
	if c.Awards.Dir != "" {
		return c.Awards.Dir
	}
	return filepath.Join(c.DataDir, "awards")
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

// StationLogConf is the station-log component (muehle/hf/station-log): the
// log view is loaded from its HTTP API and kept current from its events.
// Writing the section without url selects http://127.0.0.1:8096; leaving the
// section out turns the integration off.
type StationLogConf struct {
	URL string `toml:"url"`
	// OwnCall is the active callsign whose QSOs count; default cluster.call,
	// then clublog.call ("" folds every own call of the log).
	OwnCall string `toml:"own_call"`
	// Station and Slot address station-log's slot for its event topic.
	Station string `toml:"station"`
	Slot    string `toml:"slot"`
	// ReloadMinutes is the period of the full reload (default 5).
	ReloadMinutes int `toml:"reload_minutes"`
}

// Enabled reports whether the log view comes from station-log.
func (s StationLogConf) Enabled() bool { return s.URL != "" }

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

// ClusterConf is one DX-cluster login (telnet). Its spots hide the heard
// stations they name and list the needed ones as cluster rows.
type ClusterConf struct {
	// Name labels the cluster on the wire (cluster_name) and in problems;
	// unique. Default "cluster" when it is the only entry.
	Name string `toml:"name"`
	// Address is the node, host:port.
	Address string `toml:"address"`
	// Call is the login callsign; spots by this call (the operator's own)
	// are not listed.
	Call string `toml:"call"`
	// Skimmer keeps CW/RTTY skimmer (RBN) spots (spotter "-#"); off by default.
	Skimmer bool `toml:"skimmer"`
	// Enabled defaults to true; false keeps the entry in the file but does
	// not connect.
	Enabled *bool `toml:"enabled"`
}

// IsEnabled reports whether the cluster is connected.
func (c ClusterConf) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// DefaultClusterName names a lone entry without a name (and the legacy
// single [cluster] table).
const DefaultClusterName = "cluster"

// ClusterList is the DX clusters. It reads [[cluster]] (a list of tables) and,
// for deployed configs, the older single [cluster] table as a list of one.
type ClusterList []ClusterConf

// UnmarshalTOML implements toml.Unmarshaler.
func (l *ClusterList) UnmarshalTOML(data any) error {
	var tables []map[string]any
	legacy := false // the old single [cluster] table: unknown keys were ignored, as in every other table
	switch v := data.(type) {
	case map[string]any:
		tables, legacy = []map[string]any{v}, true
	case []map[string]any:
		tables = v
	case []any:
		for _, e := range v {
			m, ok := e.(map[string]any)
			if !ok {
				return errors.New("cluster: want [[cluster]] tables")
			}
			tables = append(tables, m)
		}
	default:
		return fmt.Errorf("cluster: want a [cluster] table or [[cluster]] tables, not %T", data)
	}
	out := make(ClusterList, 0, len(tables))
	for _, m := range tables {
		var c ClusterConf
		for k, v := range m {
			var ok bool
			switch k {
			case "name":
				c.Name, ok = v.(string)
			case "address":
				c.Address, ok = v.(string)
			case "call":
				c.Call, ok = v.(string)
			case "skimmer":
				c.Skimmer, ok = v.(bool)
			case "enabled":
				var b bool
				if b, ok = v.(bool); ok {
					c.Enabled = &b
				}
			default:
				if legacy {
					continue
				}
				return fmt.Errorf("cluster: unknown key %q", k)
			}
			if !ok {
				return fmt.Errorf("cluster.%s: wrong type %T", k, v)
			}
		}
		out = append(out, c)
	}
	if len(out) == 1 && strings.TrimSpace(out[0].Name) == "" {
		out[0].Name = DefaultClusterName
	}
	*l = out
	return nil
}

// Active lists the enabled clusters that have an address.
func (l ClusterList) Active() []ClusterConf {
	var out []ClusterConf
	for _, c := range l {
		if c.IsEnabled() && c.Address != "" {
			out = append(out, c)
		}
	}
	return out
}

// OwnCall is the first login callsign: the active station call when
// station_log.own_call is not set.
func (l ClusterList) OwnCall() string {
	for _, c := range l {
		if c.Call != "" {
			return c.Call
		}
	}
	return ""
}

// Gate values of [bmrec] gate.
const (
	GateOff       = "off"
	GateTrusted   = "trusted"
	GateQualified = "qualified"
)

// BmrecConf says which of bmrec's open CQ rows are available stations.
type BmrecConf struct {
	// Gate is "off" (every open row, no matter whether its call is known),
	// "trusted" (a callsign that is read back or typed by the operator;
	// the default) or "qualified" (trusted and the row's qualified flag).
	Gate string `toml:"gate"`
	// LingerMinutes keeps a bmrec row that passed the gate on the list this many
	// minutes after bmrec closed it (a CQ station going quiet for a few minutes
	// closes its row, but it is still there to be worked). 0 = off; default 10.
	LingerMinutes int `toml:"linger_minutes"`
}

// BuddyConf is a [[buddy]]: a station to be told about whenever it is on the
// list.
type BuddyConf struct {
	// Call is the buddy's callsign; portable forms (/P, EA8/...) match.
	Call string `toml:"call"`
	// Note is shown with the row.
	Note string `toml:"note"`
	// Boost orders buddies among themselves (higher first); default 0.
	Boost int `toml:"boost"`
}

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
		Bmrec:   BmrecConf{Gate: GateTrusted, LingerMinutes: 10},
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
	var md toml.MetaData
	switch {
	case err == nil:
		if md, err = toml.Decode(string(data), &cfg); err != nil {
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
	cfg.finishStationLog(md.IsDefined("station_log"))
	return cfg, cfg.Validate()
}

// finishStationLog fills the station_log defaults when the section is there.
func (c *Config) finishStationLog(present bool) {
	s := &c.StationLog
	if present && s.URL == "" {
		s.URL = "http://127.0.0.1:8096"
	}
	if s.Station == "" {
		s.Station = "hf"
	}
	if s.Slot == "" {
		s.Slot = "station-log"
	}
	if s.ReloadMinutes == 0 {
		s.ReloadMinutes = 5
	}
	if s.OwnCall == "" {
		s.OwnCall = c.Cluster.OwnCall()
	}
	if s.OwnCall == "" {
		s.OwnCall = c.ClubLog.Call
	}
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
	if c.StationLog.Enabled() {
		u, err := url.Parse(c.StationLog.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("station_log.url %q: want http(s)://host:port", c.StationLog.URL)
		}
		if c.StationLog.ReloadMinutes < 1 {
			return errors.New("station_log.reload_minutes must be at least 1")
		}
	}
	seen := map[string]bool{}
	for _, g := range c.Goals {
		k := strings.ToLower(strings.TrimSpace(g.Award))
		if k == "" {
			return errors.New("goal.award is required")
		}
		if seen[k] {
			return fmt.Errorf("goal %q is listed twice", g.Award)
		}
		seen[k] = true
		if g.Priority < 0 || g.Priority > 1000 {
			return fmt.Errorf("goal %q: priority %d: want 0 (default) to 1000", g.Award, g.Priority)
		}
	}
	if c.ClubLog.Token != "" && c.ClubLog.Call == "" {
		return errors.New("clublog.call is required with a Club Log token")
	}
	switch c.Bmrec.Gate {
	case GateOff, GateTrusted, GateQualified:
	default:
		return fmt.Errorf("bmrec.gate %q: want off, trusted or qualified", c.Bmrec.Gate)
	}
	if c.Bmrec.LingerMinutes < 0 || c.Bmrec.LingerMinutes > 120 {
		return fmt.Errorf("bmrec.linger_minutes %d: want 0 (off) to 120", c.Bmrec.LingerMinutes)
	}
	names := map[string]bool{}
	for i, cl := range c.Cluster {
		n := strings.TrimSpace(cl.Name)
		if n == "" || len(n) > 32 || strings.ContainsAny(n, " \t\r\n") {
			return fmt.Errorf("cluster #%d: name %q: want 1-32 characters without spaces", i+1, cl.Name)
		}
		if names[strings.ToLower(n)] {
			return fmt.Errorf("cluster name %q is used twice", n)
		}
		names[strings.ToLower(n)] = true
		if cl.Address == "" { // no login, as before
			continue
		}
		if _, _, err := net.SplitHostPort(cl.Address); err != nil {
			return fmt.Errorf("cluster %q: address %q: want host:port", n, cl.Address)
		}
		if cl.Call == "" {
			return fmt.Errorf("cluster %q: call is required with address", n)
		}
	}
	buddies := map[string]bool{}
	for _, b := range c.Buddies {
		k := strings.ToUpper(strings.TrimSpace(b.Call))
		if k == "" {
			return errors.New("buddy.call is required")
		}
		if buddies[k] {
			return fmt.Errorf("buddy %q is listed twice", b.Call)
		}
		buddies[k] = true
		if b.Boost < -1000 || b.Boost > 1000 {
			return fmt.Errorf("buddy %q: boost %d: want -1000 to 1000", b.Call, b.Boost)
		}
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
