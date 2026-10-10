// Package config holds runtime configuration for powerseq, the station
// startup/shutdown sequencer (integration model `sequencer` role, logic slot
// `muehle/hf/power-seq`). This package defines the configuration shape, defaults
// and loading (TOML file + flags + env overrides for the [mqtt] section).
//
// The startup/shutdown sequence is CONFIG-DRIVEN: it is a pair of ordered step
// lists ([[startup]] / [[shutdown]], plus the optional idle [[standby]] /
// [[wake]]) in this config, not hard-coded in Go. Each
// step is one of five kinds — emit a retained /cmd, wait for N slots' /status,
// wait for a slot's /state field, a fixed delay, or a Wake-on-LAN packet. The sequencer resolves the
// site-relative slot addresses to absolute ones (model §7.1 is the default
// sequence shipped in config.example.toml).
package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration.
type Config struct {
	// Host is the compute node the sequencer runs on (model §3, §8.1 item 5),
	// published in /meta. Defaults to "shari".
	Host string `toml:"host"`
	// Location is a physical label published in /meta.
	Location string `toml:"location"`
	// MQTT holds broker connection settings and the sequencer's own slot address.
	MQTT MQTTConfig `toml:"mqtt"`
	// Timing holds the startup/shutdown cadence (delays + timeouts).
	Timing TimingConfig `toml:"timing"`
	// Startup is the ordered startup sequence (model §7.1). Run on `start`.
	Startup []Step `toml:"startup"`
	// Shutdown is the ordered shutdown sequence (reverse of startup, with
	// staggers for inrush). Run on `stop`.
	Shutdown []Step `toml:"shutdown"`
	// Standby is the ordered idle power-down sequence (optional). Run from
	// phase=running when the station has been idle (see Idle).
	Standby []Step `toml:"standby"`
	// Wake is the ordered revive sequence (optional, required with Standby).
	// Run from phase=standby on activity or on `start`.
	Wake []Step `toml:"wake"`
	// Idle configures when the station counts as idle (standby trigger) and
	// what counts as activity (wake trigger). Only used with Standby/Wake.
	Idle IdleConfig `toml:"idle"`
	// Log controls logging verbosity.
	Log LogConfig `toml:"log"`
}

// MQTTConfig holds broker connection settings and the sequencer's own address.
type MQTTConfig struct {
	Broker          string `toml:"broker"`
	ClientID        string `toml:"client_id"`
	User            string `toml:"user"`
	Password        string `toml:"password"`
	Site            string `toml:"site"`             // e.g. "muehle"
	Station         string `toml:"station"`          // e.g. "hf"
	Slot            string `toml:"slot"`             // e.g. "power-seq"
	DiscoveryPrefix string `toml:"discovery_prefix"` // hadiscovery (model §9)
}

// TimingConfig holds the sequencer cadence. Times are in seconds for TOML
// readability; the sequencer converts to time.Duration.
type TimingConfig struct {
	// NetworkDelayS is the pause referenced by delay steps with duration =
	// "network". ~30 s — let the network (broker, the Shelies' WiFi) come up.
	NetworkDelayS int `toml:"network_delay_s"`
	// StepTimeoutS is the DEFAULT deadline for every wait step (overridden by a
	// step's timeout_s). Max wait for a liveness confirmation before the step
	// (and the sequence) faults. Generous: a slow-booting device.
	StepTimeoutS int `toml:"step_timeout_s"`
	// ShutdownStaggerS is the pause referenced by delay steps with duration =
	// "stagger" — between shutdown steps for inrush control.
	ShutdownStaggerS int `toml:"shutdown_stagger_s"`
	// PollIntervalMs is how often the runner re-checks a wait condition.
	PollIntervalMs int `toml:"poll_interval_ms"`
	// DefaultHoldMs is the default hold (debounce) window for a wait step that
	// omits hold_ms — the condition must hold continuously this long. 0 = the
	// condition passes as soon as it is true (edge-triggered).
	DefaultHoldMs int `toml:"default_hold_ms"`
}

// IdleConfig configures the standby/wake triggers.
//
// The station is idle when the activity slot's /state.activity is "inactive"
// AND no watch moved within TimeoutMinutes AND phase=running has lasted at
// least TimeoutMinutes. It wakes when the activity slot reports a
// last_activity newer than the standby entry, or a watch moves.
type IdleConfig struct {
	// ActivitySlot is the site-relative slot whose /state carries
	// activity ("active"|"inactive") and last_activity (RFC3339), e.g.
	// "hf/antenna-select".
	ActivitySlot string `toml:"activity_slot"`
	// TimeoutMinutes is the quiet window for the watches and the minimum time
	// in phase=running before standby (no standby right after a start/wake).
	TimeoutMinutes int `toml:"timeout_minutes"`
	// Watches are extra activity sources: a change of the /state field counts
	// as activity (e.g. UHF radio frequency, UHF rotator position).
	Watches []Watch `toml:"watch"`
}

// Watch is one extra activity source.
type Watch struct {
	Slot  string `toml:"slot"`
	Field string `toml:"field"`
	// MinDelta is the numeric deadband: a numeric change smaller than this
	// (from the last counted value) is not activity. 0 = any change counts.
	MinDelta float64 `toml:"min_delta"`
}

// LogConfig controls logging verbosity.
type LogConfig struct {
	Level string `toml:"level"`
}

// Step is one step in the startup or shutdown sequence. The `kind` field is the
// discriminator; which other fields are required depends on it (validated in
// Validate). Slot addresses are site-relative (e.g. "power/master",
// "hf/switch"); the sequencer resolves them to <site>/<slot>.
//
//	kind = "cmd"          emit a retained /cmd.          slot, action, value [, retain]
//	kind = "wait_status"  wait for N slots' /status.     slots [, state] [, hold_ms] [, timeout_s]
//	kind = "wait_state"   wait for a slot's /state field. slot, field, value [, hold_ms] [, timeout_s]
//	kind = "delay"        sleep a fixed duration.        duration_s | duration("network"|"stagger")
//	kind = "wol"          send a Wake-on-LAN packet.     mac [, broadcast]
//
// value is ALWAYS a TOML string: the stationa /cmd value-key convention carries
// the argument under `value` as a string (model value_type is string|int|float,
// NO bool), matching the live wire format (set_power on/off, set_enabled
// "true"/"false").
type Step struct {
	Name string `toml:"name"`
	Kind string `toml:"kind"`

	// cmd
	Slot   string `toml:"slot"`
	Action string `toml:"action"`
	Value  string `toml:"value"`
	Retain *bool  `toml:"retain"`

	// wait_status
	Slots []string `toml:"slots"`
	State string   `toml:"state"`

	// wait_state
	Field string `toml:"field"`

	// waits (wait_status + wait_state)
	HoldMs   *int `toml:"hold_ms"`
	TimeoutS *int `toml:"timeout_s"`

	// delay
	DurationS *int   `toml:"duration_s"`
	Duration  string `toml:"duration"`

	// wol
	MAC       string `toml:"mac"`
	Broadcast string `toml:"broadcast"` // host:port, default 255.255.255.255:9
}

// The valid step kinds.
const (
	KindCmd        = "cmd"
	KindWaitStatus = "wait_status"
	KindWaitState  = "wait_state"
	KindDelay      = "delay"
	KindWOL        = "wol"
)

// Defaults returns a Config with sensible default values. The sequence itself
// has NO built-in default — it must be supplied in config (the
// config.example.toml ships the model §7.1 sequence). Only the transport,
// timing, and logging defaults are baked in.
func Defaults() Config {
	return Config{
		Host:     "shari",
		Location: "bauwagen",
		MQTT: MQTTConfig{
			Broker:          "tcp://127.0.0.1:1883",
			ClientID:        "",
			User:            "hf",
			Site:            "muehle",
			Station:         "hf",
			Slot:            "power-seq",
			DiscoveryPrefix: "homeassistant",
		},
		Timing: TimingConfig{
			NetworkDelayS:    30,
			StepTimeoutS:     120,
			ShutdownStaggerS: 2,
			PollIntervalMs:   200,
			DefaultHoldMs:    0,
		},
		Log: LogConfig{Level: "info"},
	}
}

// Flags describes the command-line flags powerseq understands.
type Flags struct {
	ConfigPath string
	LogLevel   string
}

// RegisterFlags wires powerseq's flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	fs.StringVar(&f.ConfigPath, "config", "/etc/powerseq/config.toml", "path to config file")
	fs.StringVar(&f.LogLevel, "log.level", "", "log level (debug|info|warn|error); overrides config")
	return &f
}

// Load reads the TOML config file (if present), applies defaults and env
// overrides, and applies flag overrides. An empty/missing DEFAULT-path file is
// not an error: defaults are used. An explicitly-given path that is missing or
// malformed is fatal (handled by the caller via the returned error).
func Load(f *Flags) (Config, error) {
	cfg := Defaults()

	if data, err := os.ReadFile(f.ConfigPath); err == nil {
		if _, err := toml.Decode(string(data), &cfg); err != nil {
			return Config{}, fmt.Errorf("decode %s: %w", f.ConfigPath, err)
		}
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("read %s: %w", f.ConfigPath, err)
	}

	applyEnv(&cfg)

	if f.LogLevel != "" {
		cfg.Log.Level = strings.ToLower(f.LogLevel)
	}
	if cfg.Log.Level == "" {
		cfg.Log.Level = "info"
	}
	if cfg.MQTT.DiscoveryPrefix == "" {
		cfg.MQTT.DiscoveryPrefix = "homeassistant"
	}
	if cfg.Host == "" {
		cfg.Host = "shari"
	}
	if cfg.Timing.NetworkDelayS < 0 {
		cfg.Timing.NetworkDelayS = 30
	}
	if cfg.Timing.StepTimeoutS <= 0 {
		cfg.Timing.StepTimeoutS = 120
	}
	if cfg.Timing.ShutdownStaggerS < 0 {
		cfg.Timing.ShutdownStaggerS = 2
	}
	if cfg.Timing.PollIntervalMs <= 0 {
		cfg.Timing.PollIntervalMs = 200
	}
	if cfg.Timing.DefaultHoldMs < 0 {
		cfg.Timing.DefaultHoldMs = 0
	}
	if cfg.Idle.TimeoutMinutes == 0 {
		cfg.Idle.TimeoutMinutes = 30
	}

	return cfg, nil
}

// Validate checks that the config is usable. The sequencer's own address
// (site/station/slot) is mandatory (model §2/§8.1), and both step lists must be
// present and well-formed (fail fast at load, never at runtime).
func (c Config) Validate() error {
	if c.MQTT.Site == "" {
		return fmt.Errorf("mqtt site must be configured for station-model addressing")
	}
	if c.MQTT.Station == "" || c.MQTT.Slot == "" {
		return fmt.Errorf("mqtt station and slot must be set (the sequencer's own address)")
	}
	if c.MQTT.Broker == "" {
		return fmt.Errorf("mqtt broker must be configured")
	}
	if err := validateSteps("startup", c.Startup); err != nil {
		return err
	}
	if err := validateSteps("shutdown", c.Shutdown); err != nil {
		return err
	}
	// Standby and wake come as a pair: a station that can power down on idle
	// must also be able to revive.
	if (len(c.Standby) == 0) != (len(c.Wake) == 0) {
		return fmt.Errorf("[[standby]] and [[wake]] must both be present or both absent")
	}
	if len(c.Standby) > 0 {
		if err := validateSteps("standby", c.Standby); err != nil {
			return err
		}
		if err := validateSteps("wake", c.Wake); err != nil {
			return err
		}
		if c.Idle.ActivitySlot == "" {
			return fmt.Errorf("[idle] activity_slot is required with [[standby]]/[[wake]]")
		}
		if c.Idle.TimeoutMinutes <= 0 {
			return fmt.Errorf("[idle] timeout_minutes must be > 0 (got %d)", c.Idle.TimeoutMinutes)
		}
		for i, w := range c.Idle.Watches {
			if w.Slot == "" || w.Field == "" {
				return fmt.Errorf("idle.watch[%d]: slot and field are required", i)
			}
			if w.MinDelta < 0 {
				return fmt.Errorf("idle.watch[%d]: min_delta must be >= 0", i)
			}
		}
	}
	return nil
}

func validateSteps(list string, steps []Step) error {
	if len(steps) == 0 {
		return fmt.Errorf("at least one [[%s]] step is required (the sequence is config-driven)", list)
	}
	seen := map[string]bool{}
	for i, st := range steps {
		prefix := fmt.Sprintf("%s[%d]", list, i)
		if st.Name == "" {
			return fmt.Errorf("%s: step name is required", prefix)
		}
		if seen[st.Name] {
			return fmt.Errorf("%s: duplicate step name %q (names must be unique within %s)", prefix, st.Name, list)
		}
		seen[st.Name] = true
		switch st.Kind {
		case KindCmd:
			if st.Slot == "" || st.Action == "" || st.Value == "" {
				return fmt.Errorf("%s (%q): cmd step requires slot, action, value", prefix, st.Name)
			}
		case KindWaitStatus:
			if len(st.Slots) == 0 {
				return fmt.Errorf("%s (%q): wait_status step requires slots", prefix, st.Name)
			}
			for _, sl := range st.Slots {
				if sl == "" {
					return fmt.Errorf("%s (%q): wait_status slots must not be empty (a stray \"\" entry makes the AND unpassable)", prefix, st.Name)
				}
			}
			if st.State != "" && st.State != "online" && st.State != "offline" {
				return fmt.Errorf("%s (%q): wait_status state must be online|offline, got %q", prefix, st.Name, st.State)
			}
			if st.TimeoutS != nil && *st.TimeoutS <= 0 {
				return fmt.Errorf("%s (%q): timeout_s must be > 0 (got %d; nil means use the default)", prefix, st.Name, *st.TimeoutS)
			}
		case KindWaitState:
			// value may be "" — waiting for a field to clear (become absent/nil)
			// is legitimate and the runtime supports it (absent → "" matches "").
			if st.Slot == "" || st.Field == "" {
				return fmt.Errorf("%s (%q): wait_state step requires slot, field (value may be empty to wait for clear)", prefix, st.Name)
			}
			if st.TimeoutS != nil && *st.TimeoutS <= 0 {
				return fmt.Errorf("%s (%q): timeout_s must be > 0 (got %d; nil means use the default)", prefix, st.Name, *st.TimeoutS)
			}
		case KindDelay:
			hasS := st.DurationS != nil
			hasSym := st.Duration != ""
			if hasS == hasSym { // exactly one required
				return fmt.Errorf("%s (%q): delay step requires exactly one of duration_s or duration", prefix, st.Name)
			}
			if hasS && *st.DurationS <= 0 {
				return fmt.Errorf("%s (%q): duration_s must be > 0 (got %d; a non-positive delay silently skips)", prefix, st.Name, *st.DurationS)
			}
			if hasSym && st.Duration != "network" && st.Duration != "stagger" {
				return fmt.Errorf("%s (%q): delay duration must be \"network\"|\"stagger\", got %q", prefix, st.Name, st.Duration)
			}
		case KindWOL:
			if _, err := net.ParseMAC(st.MAC); err != nil {
				return fmt.Errorf("%s (%q): wol step requires a valid mac: %v", prefix, st.Name, err)
			}
			if st.Broadcast != "" {
				if _, _, err := net.SplitHostPort(st.Broadcast); err != nil {
					return fmt.Errorf("%s (%q): wol broadcast must be host:port: %v", prefix, st.Name, err)
				}
			}
		default:
			return fmt.Errorf("%s (%q): unknown step kind %q (want cmd|wait_status|wait_state|delay|wol)", prefix, st.Name, st.Kind)
		}
	}
	return nil
}

// applyEnv overlays POWERSEQ_MQTT_* env vars on top of cfg, used for the systemd
// EnvironmentFile workflow where the secret isn't in the TOML.
func applyEnv(cfg *Config) {
	if v := os.Getenv("POWERSEQ_MQTT_BROKER"); v != "" {
		cfg.MQTT.Broker = v
	}
	if v := os.Getenv("POWERSEQ_MQTT_CLIENT_ID"); v != "" {
		cfg.MQTT.ClientID = v
	}
	if v := os.Getenv("POWERSEQ_MQTT_USER"); v != "" {
		cfg.MQTT.User = v
	}
	if v := os.Getenv("POWERSEQ_MQTT_PASSWORD"); v != "" {
		cfg.MQTT.Password = v
	}
	if v := os.Getenv("POWERSEQ_MQTT_SITE"); v != "" {
		cfg.MQTT.Site = v
	}
}
