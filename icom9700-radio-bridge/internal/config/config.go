// Package config holds runtime configuration for icom9700-radio-bridge.
//
// icom9700-radio-bridge fronts the Icom IC-9700 as the canonical `radio` slot
// muehle/uhf/radio on the station bus, controlling it over CI-V via Icom's
// RS-BA1-style LAN protocol (UDP :50001 control / :50002 CI-V data — the
// protocol has no discovery, so radio_host is mandatory). This package defines
// the configuration shape, defaults and loading (TOML file + flags +
// ICOM9700_* env overrides).
//
// The two secrets — the MQTT password and the CI-V login password — are
// deliberately NOT TOML keys: both are read from the environment (systemd
// EnvironmentFile) and a `password` key anywhere in the TOML is a hard parse
// error, so neither secret can silently leak into the config file.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// EnvPrefix is the ICOM9700_* env override prefix. The feature plan pins these
// exact names (shorter than the dir-name-derived ICOM9700_RADIO_BRIDGE_* the
// naming convention would produce); the load-bearing ones are the two secrets,
// ICOM9700_MQTT_PASSWORD and ICOM9700_CIV_PASSWORD.
const EnvPrefix = "ICOM9700"

// Config is the top-level configuration.
type Config struct {
	// RadioHost is the radio's LAN address (IC-9700 remote-control UDP
	// endpoint, e.g. 9700.kgbvax.net). The RS-BA1 protocol has no
	// broadcast/discovery — the client must know the address.
	RadioHost string `toml:"radio_host"`

	MQTT    MQTTConfig    `toml:"mqtt"`
	CIV     CIVConfig     `toml:"civ"`
	Session SessionConfig `toml:"session"`
	Radio   RadioConfig   `toml:"radio"`
	Log     LogConfig     `toml:"log"`
}

// MQTTConfig holds broker connection settings and station-model addressing.
// The password is env-only (toml:"-" so a `password` key in the TOML stays
// undecoded and is rejected at load).
type MQTTConfig struct {
	Broker          string `toml:"broker"`
	ClientID        string `toml:"client_id"`
	User            string `toml:"user"`
	DiscoveryPrefix string `toml:"discovery_prefix"`

	// Station-model slot addressing: <site>/<station>/<slot>
	Site     string `toml:"site"`     // e.g. "muehle"
	Station  string `toml:"station"`  // e.g. "uhf"
	Slot     string `toml:"slot"`     // e.g. "radio"
	Location string `toml:"location"` // physical location label, published in /meta

	// Password comes only from ICOM9700_MQTT_PASSWORD.
	Password string `toml:"-"`
}

// CIVConfig holds the radio's remote-control login credentials (SET > Network
// > Remote Control on the radio; deploy gate 3 in the feature plan). The
// password is env-only.
type CIVConfig struct {
	Username string `toml:"username"`

	// Password comes only from ICOM9700_CIV_PASSWORD.
	Password string `toml:"-"`
}

// SessionConfig holds the on-demand session policy (plan R1/R2, KTD-2): the
// bridge connects only on demand and never auto-steals the radio's single LAN
// session, so the retry parameters are deliberately conservative.
type SessionConfig struct {
	// IdleTimeout is how long a live session is held with no radio-side work
	// before the bridge disconnects (a duration string, "120s"). Radio
	// keepalives and meter frames do not count as work; an armed permit
	// blocks idle-disconnect (R11). Parsed into IdleTimeoutDur at load.
	IdleTimeout string `toml:"idle_timeout"`
	// TXWatchdog bounds a keyed PTT while a session is live or can be
	// re-established (a duration string, "180s"; plan KTD-5/R12). Parsed
	// into TXWatchdogDur at load.
	TXWatchdog string `toml:"tx_watchdog"`
	// MaxAttempts bounds the login attempt series per connect demand
	// (cmd-driven, armed-held or safety-driven — R2).
	MaxAttempts int `toml:"max_attempts"`
	// AttemptSpacing is the minimum spacing between login attempts (a
	// duration string, "30s" — pending the login-lockout bench pin, plan
	// deploy gate 5). Parsed into AttemptSpacingDur at load.
	AttemptSpacing string `toml:"attempt_spacing"`
	// ErrorDecay is how long the session state machine holds `error` before
	// decaying to `idle` (a duration string, "60s"; plan R3). Parsed into
	// ErrorDecayDur at load.
	ErrorDecay string `toml:"error_decay"`

	IdleTimeoutDur    time.Duration `toml:"-"`
	TXWatchdogDur     time.Duration `toml:"-"`
	AttemptSpacingDur time.Duration `toml:"-"`
	ErrorDecayDur     time.Duration `toml:"-"`
}

// RadioConfig holds the live-session telemetry cadence.
type RadioConfig struct {
	// PollInterval is the /state refresh cadence while a session is live
	// (a duration string, "1s"; meters dedup'd to <=1 Hz into the retained
	// snapshot, KTD-8). Parsed into PollIntervalDur at load.
	PollInterval string `toml:"poll_interval"`

	PollIntervalDur time.Duration `toml:"-"`
}

// LogConfig controls logging verbosity.
type LogConfig struct {
	Level string `toml:"level"`
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		RadioHost: "9700.kgbvax.net",
		MQTT: MQTTConfig{
			// KTD-7: the broker is addressed via bwbroker DNS indirection,
			// not a hardcoded LAN address — the user repoints the DNS entry
			// as the station's broker migrates.
			Broker:          "tcp://bwbroker:1883",
			ClientID:        "",
			User:            "hf",
			DiscoveryPrefix: "homeassistant",
			Site:            "muehle",
			Station:         "uhf",
			Slot:            "radio",
			Location:        "bauwagen",
		},
		Session: SessionConfig{
			IdleTimeout:       "120s",
			TXWatchdog:        "180s",
			MaxAttempts:       3,
			AttemptSpacing:    "30s",
			ErrorDecay:        "60s",
			IdleTimeoutDur:    120 * time.Second,
			TXWatchdogDur:     180 * time.Second,
			AttemptSpacingDur: 30 * time.Second,
			ErrorDecayDur:     60 * time.Second,
		},
		Radio: RadioConfig{
			PollInterval:    "1s",
			PollIntervalDur: time.Second,
		},
		Log: LogConfig{Level: "info"},
	}
}

// Flags describes the command-line flags icom9700-radio-bridge understands.
// There is deliberately no password flag — no secret ever rides a command
// line (docs/conventions/config-and-secrets.md).
type Flags struct {
	ConfigPath string
	LogLevel   string

	// fs is the FlagSet the flags were registered on (set by RegisterFlags);
	// Load uses it to tell an explicitly-passed -config from the registered
	// default path. A hand-built Flags (tests) has no FlagSet and is never
	// explicit.
	fs *flag.FlagSet
}

// RegisterFlags wires icom9700-radio-bridge's flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	f.fs = fs
	fs.StringVar(&f.ConfigPath, "config", "/etc/icom9700-radio-bridge/config.toml", "path to config file")
	fs.StringVar(&f.LogLevel, "log.level", "", "log level (debug|info|warn|error); overrides config")
	return &f
}

// explicitConfig reports whether -config was actually passed on the command
// line (flag.Visit — even -config set to its default value counts as
// explicit). It drives the missing-file policy: an absent DEFAULT config path
// runs on defaults; an absent EXPLICIT -config path is fatal — the operator
// asked for that file, and silently running defaults would hide the typo
// (docs/conventions/config-and-secrets.md §2).
func (f *Flags) explicitConfig() bool {
	if f.fs == nil {
		return false
	}
	explicit := false
	f.fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "config" {
			explicit = true
		}
	})
	return explicit
}

// Load reads the TOML config file (if present), applies defaults and env
// overrides, and parses the duration keys. A config file absent at the
// DEFAULT path is not an error: defaults are used (bench/go-run keeps working
// without a seeded /etc file). An EXPLICITLY-passed -config path that is
// missing or unreadable IS an error wrapping fs.ErrNotExist (main exits
// non-zero) — see docs/conventions/config-and-secrets.md §2. A `password`
// key anywhere in the TOML is a hard error — both secrets are env-only.
func Load(f *Flags) (Config, error) {
	cfg := Defaults()

	data, err := os.ReadFile(f.ConfigPath)
	switch {
	case err == nil:
		md, derr := toml.Decode(string(data), &cfg)
		if derr != nil {
			return Config{}, fmt.Errorf("decode %s: %w", f.ConfigPath, derr)
		}
		for _, key := range md.Undecoded() {
			// Neither secret may sit in the TOML: the MQTT password and the
			// CI-V login password both live only in the environment. Neither
			// has a struct key, so any password-ish key (`password`,
			// `civ_password`, ...) shows up here as undecoded — reject it
			// instead of silently ignoring it like other unknown keys.
			if last := key[len(key)-1]; strings.Contains(strings.ToLower(last), "password") {
				return Config{}, fmt.Errorf(
					"%s: %v must not appear in the config file — secrets are loaded from the %s_MQTT_PASSWORD and %s_CIV_PASSWORD environment variables (EnvironmentFile)",
					f.ConfigPath, key, EnvPrefix, EnvPrefix)
			}
		}
	case errors.Is(err, fs.ErrNotExist) && !f.explicitConfig():
		// Default path simply absent: run on the built-in defaults.
	default:
		// Missing-but-explicit, or unreadable for any other reason: fatal.
		// errors.Is(err, fs.ErrNotExist) distinguishes the two for callers.
		return Config{}, fmt.Errorf("read %s: %w", f.ConfigPath, err)
	}

	applyEnv(&cfg)

	if f.LogLevel != "" {
		cfg.Log.Level = strings.ToLower(f.LogLevel)
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}

	// Parse the duration strings once; every later unit gets a Duration.
	d, err := time.ParseDuration(cfg.Session.IdleTimeout)
	if err != nil {
		return Config{}, fmt.Errorf("session.idle_timeout %q: %w", cfg.Session.IdleTimeout, err)
	}
	cfg.Session.IdleTimeoutDur = d
	d, err = time.ParseDuration(cfg.Session.TXWatchdog)
	if err != nil {
		return Config{}, fmt.Errorf("session.tx_watchdog %q: %w", cfg.Session.TXWatchdog, err)
	}
	cfg.Session.TXWatchdogDur = d
	d, err = time.ParseDuration(cfg.Session.AttemptSpacing)
	if err != nil {
		return Config{}, fmt.Errorf("session.attempt_spacing %q: %w", cfg.Session.AttemptSpacing, err)
	}
	cfg.Session.AttemptSpacingDur = d
	d, err = time.ParseDuration(cfg.Session.ErrorDecay)
	if err != nil {
		return Config{}, fmt.Errorf("session.error_decay %q: %w", cfg.Session.ErrorDecay, err)
	}
	cfg.Session.ErrorDecayDur = d
	d, err = time.ParseDuration(cfg.Radio.PollInterval)
	if err != nil {
		return Config{}, fmt.Errorf("radio.poll_interval %q: %w", cfg.Radio.PollInterval, err)
	}
	cfg.Radio.PollIntervalDur = d

	return cfg, nil
}

// Validate checks that the config is usable. Station-model addressing is
// mandatory (model §2/§8.1) and the session/radio cadences must be coherent.
func (c Config) Validate() error {
	if c.RadioHost == "" {
		return fmt.Errorf("radio_host must be configured — the RS-BA1 protocol has no discovery")
	}
	if c.MQTT.Site == "" || c.MQTT.Station == "" {
		return fmt.Errorf("mqtt site and station must be configured for station-model addressing")
	}
	if c.MQTT.Broker == "" {
		return fmt.Errorf("mqtt broker must be configured")
	}
	if c.Session.IdleTimeoutDur <= 0 {
		return fmt.Errorf("session.idle_timeout must be > 0 (got %s)", c.Session.IdleTimeoutDur)
	}
	if c.Session.TXWatchdogDur <= 0 {
		return fmt.Errorf("session.tx_watchdog must be > 0 (got %s)", c.Session.TXWatchdogDur)
	}
	if c.Session.MaxAttempts <= 0 {
		return fmt.Errorf("session.max_attempts must be > 0 (got %d)", c.Session.MaxAttempts)
	}
	if c.Session.AttemptSpacingDur <= 0 {
		return fmt.Errorf("session.attempt_spacing must be > 0 (got %s)", c.Session.AttemptSpacingDur)
	}
	if c.Session.ErrorDecayDur <= 0 {
		return fmt.Errorf("session.error_decay must be > 0 (got %s)", c.Session.ErrorDecayDur)
	}
	if c.Radio.PollIntervalDur <= 0 {
		return fmt.Errorf("radio.poll_interval must be > 0 (got %s)", c.Radio.PollIntervalDur)
	}
	return nil
}

// applyEnv overlays ICOM9700_* env vars on top of cfg, used for the systemd
// EnvironmentFile workflow where the secrets aren't in the TOML. The two
// password vars are load-bearing; the rest exist for prodding the bridge
// without a config file.
func applyEnv(cfg *Config) {
	if v := os.Getenv(EnvPrefix + "_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv(EnvPrefix + "_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTT.ClientID = v
	}
	if v := os.Getenv(EnvPrefix + "_MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv(EnvPrefix + "_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv(EnvPrefix + "_CIV_USERNAME"); v != "" {
		cfg.CIV.Username = v
	}
	if v := os.Getenv(EnvPrefix + "_CIV_PASSWORD"); v != "" {
		cfg.CIV.Password = v
	}
	if v := os.Getenv(EnvPrefix + "_RADIO_HOST"); v != "" {
		cfg.RadioHost = v
	}
	if v := os.Getenv(EnvPrefix + "_SITE"); v != "" {
		cfg.MQTT.Site = v
	}
	if v := os.Getenv(EnvPrefix + "_STATION"); v != "" {
		cfg.MQTT.Station = v
	}
	if v := os.Getenv(EnvPrefix + "_SLOT"); v != "" {
		cfg.MQTT.Slot = v
	}
}
