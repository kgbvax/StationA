// Package config holds runtime configuration for spid-ercm-rotator-bridge.
//
// spid-ercm-rotator-bridge fronts two satellite-tracking rotator axes as two
// canonical `rotator` slots (muehle/uhf/az-rotator, SPID Rot1Prog azimuth;
// muehle/uhf/el-rotator, ERC-M/GS-500 elevation) plus a rotctld TCP server and
// a PstRotator UDP listener. This package defines the configuration shape,
// defaults and loading (TOML file + flags + SPID_ERCM_ROTATOR_BRIDGE_* env
// overrides for the [mqtt] section).
//
// The MQTT password is deliberately NOT a TOML key: it is read from the
// SPID_ERCM_ROTATOR_BRIDGE_MQTT_PASSWORD environment variable (systemd
// EnvironmentFile) and a `password` key anywhere in the TOML is a hard parse
// error, so the secret can never silently leak into the config file.
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

// Axes this bridge fronts. Exactly one slot per axis is required.
const (
	AxisAZ = "az"
	AxisEL = "el"
)

// EnvPrefix is the SPID_ERCM_ROTATOR_BRIDGE_* env override prefix — the module
// dir name uppercased with hyphens → underscores (docs/conventions/naming.md).
const EnvPrefix = "SPID_ERCM_ROTATOR_BRIDGE"

// byIDPrefix is the only accepted serial port spelling. The serial self-heal
// re-resolves this stable symlink after a USB re-enumeration (KTD7), so a raw
// /dev/ttyUSB* path would defeat the heal.
const byIDPrefix = "/dev/serial/by-id/"

// Config is the top-level configuration.
type Config struct {
	// Host is the compute node the adapter runs on (model §3, §8.1 item 5),
	// published in /meta. Defaults to "shari".
	Host string `toml:"host"`
	// MQTT holds broker connection settings and the site/station address
	// prefix shared by both rotator slots.
	MQTT MQTTConfig `toml:"mqtt"`
	// Log controls logging verbosity.
	Log LogConfig `toml:"log"`
	// Rotctld is the rotctld-compatible TCP server endpoint (KTD10).
	Rotctld RotctldConfig `toml:"rotctld"`
	// PstRotator is the PstRotator native UDP listener endpoint (KTD11).
	PstRotator PstRotatorConfig `toml:"pstrotator"`
	// GS232 is the legacy GS-232B TCP server endpoint (the PstRotator
	// integration path — PstRotator's own UDP control makes IT the listener,
	// so a networked PstRotator speaks GS-232 to us instead).
	GS232 GS232Config `toml:"gs232"`
	// Control holds the mount-wide cadence and per-axis travel envelope,
	// deadband and park positions.
	Control ControlConfig `toml:"control"`
	// Slots is the set of rotator axes this bridge fronts. Each becomes one
	// canonical `rotator` slot with its own /meta /state /status /cmd and its
	// own paho client + LWT, so a dead serial port degrades only its own slot
	// (KTD2).
	Slots []SlotConfig `toml:"slot"`
}

// MQTTConfig holds broker connection settings. The password is env-only
// (toml:"-" so a `password` key in the TOML stays undecoded and is rejected
// at load).
type MQTTConfig struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	User     string `toml:"user"`
	Site     string `toml:"site"`     // e.g. "muehle"
	Station  string `toml:"station"`  // e.g. "uhf"
	Location string `toml:"location"` // physical location label, published in /meta

	// Password comes only from SPID_ERCM_ROTATOR_BRIDGE_MQTT_PASSWORD.
	Password string `toml:"-"`
}

// LogConfig controls logging verbosity.
type LogConfig struct {
	Level string `toml:"level"`
}

// RotctldConfig is the rotctld TCP server bind endpoint.
type RotctldConfig struct {
	Bind string `toml:"bind"`
	Port int    `toml:"port"`
}

// PstRotatorConfig is the PstRotator UDP listener bind endpoint.
type PstRotatorConfig struct {
	Bind string `toml:"bind"`
	Port int    `toml:"port"`
}

// GS232Config is the legacy GS-232B TCP server endpoint. Optional control
// path for rotator-control software such as PSTRotator/N1MM: it drives the
// same mount façade the bus does, and the resulting motion still surfaces in
// /state (the wrc-rotator-bridge precedent, with elevation added).
type GS232Config struct {
	Enabled bool   `toml:"enabled"`
	Bind    string `toml:"bind"` // bind address, e.g. "0.0.0.0"
	Port    int    `toml:"port"` // listen port, e.g. 4533
}

// SlotConfig describes one rotator axis → one canonical `rotator` slot.
type SlotConfig struct {
	// Axis is the mount axis this slot drives: "az" or "el".
	Axis string `toml:"axis"`
	// Slot is the slot address segment; the full address is
	// <mqtt.site>/<mqtt.station>/<slot>. Defaults to "az-rotator"/"el-rotator".
	Slot string `toml:"slot"`
	// DeviceModel / DeviceLink are the identity published in /meta.
	DeviceModel string `toml:"device_model"`
	DeviceLink  string `toml:"link"`
	// Serial is the per-axis serial port table.
	Serial SerialConfig `toml:"serial"`
}

// SerialConfig is one axis's serial link to its controller.
type SerialConfig struct {
	// Port is the stable /dev/serial/by-id/... symlink. An EMPTY port selects
	// the in-process mock device (KTD7) so the stack runs without hardware.
	Port string `toml:"port"`
	// Baud is the controller baud rate. Defaults: az 1200 (Rot1Prog, KTD5),
	// el 9600 (ERC-M GS-232B, KTD6).
	Baud int `toml:"baud"`
}

// Mock reports whether this slot runs the in-process mock device instead of a
// real serial port — true iff the configured port is empty (KTD7).
func (s SlotConfig) Mock() bool { return s.Serial.Port == "" }

// ControlConfig holds the mount-wide poll/reopen cadence and the per-axis
// travel envelope, deadband and park position.
type ControlConfig struct {
	// PollInterval is the per-axis readback poll period, a Go duration string
	// ("1s"). Parsed into PollIntervalDur at load.
	PollInterval string `toml:"poll_interval"`
	// ReopenCooldown is the wait before re-resolving the by-id path after a
	// serial error, a Go duration string ("2s"). Parsed into
	// ReopenCooldownDur at load.
	ReopenCooldown string `toml:"reopen_cooldown"`

	PollIntervalDur   time.Duration `toml:"-"`
	ReopenCooldownDur time.Duration `toml:"-"`

	// AZ / EL are the per-axis control envelopes, keyed by axis name.
	AZ AxisControl `toml:"az"`
	EL AxisControl `toml:"el"`
}

// AxisControl is one axis's travel envelope, no-op deadband and park position.
type AxisControl struct {
	// Min / Max are the configured travel limits; targets outside are refused
	// on every control path before any serial write (R10).
	Min float64 `toml:"min"`
	Max float64 `toml:"max"`
	// Deadband is the no-op skip distance: a target within this distance of
	// the cached readback skips the serial write (R12). Defaults: az 4°
	// (micro-corrections from tracking clients must not jerk the rotor),
	// el 1°.
	Deadband float64 `toml:"deadband"`
	// Park is the axis park position dispatched by the PstRotator PARK
	// command (R7).
	Park float64 `toml:"park"`
}

// Axis returns the per-axis control envelope for "az" or "el".
func (c ControlConfig) Axis(axis string) (AxisControl, error) {
	switch axis {
	case AxisAZ:
		return c.AZ, nil
	case AxisEL:
		return c.EL, nil
	default:
		return AxisControl{}, fmt.Errorf("unknown axis %q", axis)
	}
}

// Defaults returns a Config with sensible default values.
func Defaults() Config {
	return Config{
		Host: "shari",
		MQTT: MQTTConfig{
			Broker:   "tcp://127.0.0.1:1883",
			ClientID: "",
			User:     "hf",
			Site:     "muehle",
			Station:  "uhf",
			Location: "bauwagen",
		},
		Log:     LogConfig{Level: "info"},
		Rotctld: RotctldConfig{Bind: "0.0.0.0", Port: 4534},
		PstRotator: PstRotatorConfig{
			Bind: "0.0.0.0",
			Port: 12041,
		},
		GS232: GS232Config{
			Enabled: true,
			Bind:    "0.0.0.0",
			Port:    4533,
		},
		Control: ControlConfig{
			PollInterval:      "1s",
			ReopenCooldown:    "2s",
			PollIntervalDur:   time.Second,
			ReopenCooldownDur: 2 * time.Second,
			AZ:                AxisControl{Min: 0, Max: 360, Deadband: 4, Park: 0},
			EL:                AxisControl{Min: 0, Max: 90, Deadband: 1, Park: 0},
		},
	}
}

// Flags describes the command-line flags spid-ercm-rotator-bridge understands.
// There is deliberately no password flag — the secret never rides a command
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

// RegisterFlags wires spid-ercm-rotator-bridge's flags onto fs.
func RegisterFlags(fs *flag.FlagSet) *Flags {
	var f Flags
	f.fs = fs
	fs.StringVar(&f.ConfigPath, "config", "/etc/spid-ercm-rotator-bridge/config.toml", "path to config file")
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
// overrides, and applies the flag overrides from f. A config file absent at
// the DEFAULT path is not an error: defaults are used (the go-run/bench mock
// mode keeps working without a seeded /etc file). An EXPLICITLY-passed
// -config path that is missing or unreadable IS an error (wrapping
// fs.ErrNotExist for a missing file; main exits non-zero) — see
// docs/conventions/config-and-secrets.md §2. A `password` key anywhere in the
// TOML is a hard error — the MQTT password is env-only.
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
			// The password must never sit in the TOML. It has no struct key,
			// so any `password = ...` shows up here as undecoded — reject it
			// instead of silently ignoring it like other unknown keys.
			if last := key[len(key)-1]; strings.EqualFold(last, "password") {
				return Config{}, fmt.Errorf(
					"%s: %v must not appear in the config file — the MQTT password is loaded from the %s_MQTT_PASSWORD environment variable (EnvironmentFile)",
					f.ConfigPath, key, EnvPrefix)
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
	if cfg.Host == "" {
		cfg.Host = "shari"
	}

	// Per-slot defaults: slot name, link and the per-axis baud rate.
	for i := range cfg.Slots {
		s := &cfg.Slots[i]
		s.Axis = strings.ToLower(strings.TrimSpace(s.Axis))
		switch s.Axis {
		case AxisAZ:
			if s.Slot == "" {
				s.Slot = "az-rotator"
			}
			if s.Serial.Baud == 0 {
				s.Serial.Baud = 1200 // Rot1Prog is fixed at 1200 (KTD5)
			}
		case AxisEL:
			if s.Slot == "" {
				s.Slot = "el-rotator"
			}
			if s.Serial.Baud == 0 {
				s.Serial.Baud = 9600 // ERC-M GS-232B typical (KTD6)
			}
		}
		if s.DeviceLink == "" {
			s.DeviceLink = "serial"
		}
	}

	// Parse the cadence strings once; every later unit gets a Duration.
	d, err := time.ParseDuration(cfg.Control.PollInterval)
	if err != nil {
		return Config{}, fmt.Errorf("control.poll_interval %q: %w", cfg.Control.PollInterval, err)
	}
	cfg.Control.PollIntervalDur = d
	d, err = time.ParseDuration(cfg.Control.ReopenCooldown)
	if err != nil {
		return Config{}, fmt.Errorf("control.reopen_cooldown %q: %w", cfg.Control.ReopenCooldown, err)
	}
	cfg.Control.ReopenCooldownDur = d

	return cfg, nil
}

// Validate checks that the config is usable. Station-model addressing is
// mandatory (model §2/§8.1), both axes must be configured exactly once, every
// non-mock port must be a /dev/serial/by-id/ path (the self-heal re-resolves
// that symlink, KTD7), and each axis's travel envelope must be coherent.
func (c Config) Validate() error {
	if c.MQTT.Site == "" || c.MQTT.Station == "" {
		return fmt.Errorf("mqtt site and station must be configured for station-model addressing")
	}
	if c.MQTT.Broker == "" {
		return fmt.Errorf("mqtt broker must be configured")
	}
	if c.Rotctld.Port <= 0 {
		return fmt.Errorf("rotctld port must be > 0 (got %d)", c.Rotctld.Port)
	}
	if c.PstRotator.Port <= 0 {
		return fmt.Errorf("pstrotator port must be > 0 (got %d)", c.PstRotator.Port)
	}
	if c.Control.PollIntervalDur <= 0 {
		return fmt.Errorf("control.poll_interval must be > 0 (got %s)", c.Control.PollIntervalDur)
	}
	if c.Control.ReopenCooldownDur <= 0 {
		return fmt.Errorf("control.reopen_cooldown must be > 0 (got %s)", c.Control.ReopenCooldownDur)
	}

	seen := map[string]bool{}
	for i, s := range c.Slots {
		switch s.Axis {
		case AxisAZ, AxisEL:
		default:
			return fmt.Errorf("slot[%d]: axis must be %q or %q (got %q)", i, AxisAZ, AxisEL, s.Axis)
		}
		if s.Slot == "" {
			return fmt.Errorf("slot[%d]: slot name must be set", i)
		}
		if s.DeviceModel == "" {
			return fmt.Errorf("slot[%d]: device_model must be set", i)
		}
		if s.DeviceLink == "" {
			return fmt.Errorf("slot[%d]: link must be set", i)
		}
		if !s.Mock() {
			if !strings.HasPrefix(s.Serial.Port, byIDPrefix) {
				return fmt.Errorf("slot[%d]: serial port must be a %s... path (got %q) — the self-heal reopen re-resolves the stable by-id symlink after a USB re-enumeration",
					i, byIDPrefix, s.Serial.Port)
			}
			if s.Serial.Baud <= 0 {
				return fmt.Errorf("slot[%d]: serial baud must be > 0 for a real port", i)
			}
		}
		if seen[s.Axis] {
			return fmt.Errorf("slot[%d]: duplicate axis %q — exactly one [[slot]] per axis", i, s.Axis)
		}
		seen[s.Axis] = true
	}
	for _, axis := range []string{AxisAZ, AxisEL} {
		if !seen[axis] {
			return fmt.Errorf("missing %q [[slot]] — this bridge fronts exactly one az and one el slot", axis)
		}
	}

	// Per-axis travel envelopes.
	for name, ax := range map[string]AxisControl{
		AxisAZ: c.Control.AZ,
		AxisEL: c.Control.EL,
	} {
		if ax.Min >= ax.Max {
			return fmt.Errorf("control.%s: travel limits must satisfy min < max (got %v, %v)", name, ax.Min, ax.Max)
		}
		if ax.Deadband < 0 {
			return fmt.Errorf("control.%s: deadband must be >= 0 (got %v)", name, ax.Deadband)
		}
		if ax.Park < ax.Min || ax.Park > ax.Max {
			return fmt.Errorf("control.%s: park %v outside travel limits [%v, %v]", name, ax.Park, ax.Min, ax.Max)
		}
	}
	return nil
}

// Slot returns the configured slot for the given axis ("az"/"el").
func (c Config) Slot(axis string) (SlotConfig, error) {
	for _, s := range c.Slots {
		if s.Axis == axis {
			return s, nil
		}
	}
	return SlotConfig{}, fmt.Errorf("no [[slot]] configured for axis %q", axis)
}

// applyEnv overlays SPID_ERCM_ROTATOR_BRIDGE_* env vars on top of cfg, used for
// the systemd EnvironmentFile workflow where the secret isn't in the TOML.
// Only the shared [mqtt] settings are env-overridable; per-slot values (axis,
// slot name, device identity, serial) and [control] come from the TOML only.
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
	if v := os.Getenv(EnvPrefix + "_MQTT_SITE"); v != "" {
		cfg.MQTT.Site = v
	}
	if v := os.Getenv(EnvPrefix + "_MQTT_STATION"); v != "" {
		cfg.MQTT.Station = v
	}
}
