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
	TransportSim    = "sim" // in-memory emulator, never touches hardware
)

// Wire protocol names (protocol:). Both carry the same command set; they
// differ only in the wire envelope. The 303Z/3050DZ is "Pelco-D/Pelco-P
// adaptive": it detects the protocol per received frame and answers in kind,
// so the configured protocol only chooses which envelope WE send.
const (
	// ProtocolD is Pelco-D (default): 7-byte 0xFF-framed frames, additive
	// checksum. The head's documented native mode at 2400 baud.
	ProtocolD = "d"
	// ProtocolP is Pelco-P: 8-byte 0xA0/0xAF-framed frames, XOR checksum.
	// The address byte sent is the same DIP address code as in D — strict
	// Pelco-P gear is zero-indexed, but this unit matches its single DIP
	// address code in either protocol, so do NOT subtract one.
	ProtocolP = "p"
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
	Transport  string          `yaml:"transport"` // serial | tcp | sim
	Serial     SerialConfig    `yaml:"serial"`
	TCP        TCPConfig       `yaml:"tcp"`
	Sim        SimConfig       `yaml:"sim"`
	SelfCheck  SelfCheckConfig `yaml:"self_check"`
	Addr       byte            `yaml:"addr"`        // Pelco-D camera address (1-255)
	Protocol   string          `yaml:"protocol"`    // wire protocol sent to the PTZ: d (Pelco-D, default) | p (Pelco-P)
	Log        string          `yaml:"log"`         // TX/RX trace file ("" disables)
	LogLevel   LogLevel        `yaml:"log_level"`   // error | warn | info | debug | trace
	AzOffset   float64         `yaml:"az_offset"`   // azimuth zero offset: physical azimuth that reads as 0° (degrees)
	TiltInvert bool            `yaml:"tilt_invert"` // invert elevation: unit mounted upside down (logical = 90 - physical)
	TiltCal    TiltCalConfig   `yaml:"tilt_cal"`    // tilt readback calibration (raw encoder counts vs Pelco-standard hundredths)
	Control    ControlConfig   `yaml:"control"`
	Wrap       WrapConfig      `yaml:"wrap"`
	Goto       GotoConfig      `yaml:"goto"`
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

// SimConfig holds the in-memory emulator parameters (transport: sim). The
// emulator never opens a device: it answers position queries and applies
// commanded moves in memory, so the control servers can be exercised without
// a rotator attached.
type SimConfig struct {
	StartPan  float64 `yaml:"start_pan"`  // initial azimuth, degrees (default 0)
	StartTilt float64 `yaml:"start_tilt"` // initial elevation, degrees (default 0)
	JogStep   float64 `yaml:"jog_step"`   // degrees of travel per jog frame (default 5)
	// WildTiltWhileMoving reproduces the 303Z/3050DZ failure mode in the
	// simulator: while the tilt motor runs, QueryTilt answers with
	// tilt+this instead of the true position (a constant valid-checksum
	// garbage stream, never the true position); idle readback is clean.
	// >0 enables the injector; 0 (default) is a clean readback. Used by the
	// open-loop-slew goto test to exercise the halt-and-confirm path without
	// the hardware. The value is an offset in degrees (e.g. 190 lands far
	// past the 90° physical limit, plainly garbage).
	WildTiltWhileMoving float64 `yaml:"wild_tilt_while_moving"`
}

// SelfCheckConfig controls whether the bridge disables the PTZ self-check on
// connect. The 303Z/3050DZ runs a self-check sweep on power-up (and after a
// factory reset); set Disable true to send the "disable self-check" command
// (set preset 105) once per successful connect so it does not run. The setting
// is persistent on the unit, so subsequent power-ups stay disabled after the
// first. In sim mode the command is a harmless no-op (the emulator ignores
// presets), so it is sent regardless.
type SelfCheckConfig struct {
	Disable bool `yaml:"disable"` // send set-preset-105 on connect to disable the PTZ self-check (default true)
}

// ControlConfig configures the optional inbound network-control server.
type ControlConfig struct {
	Bind    string       `yaml:"bind"` // listen address for the inbound server
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

// TiltCalConfig calibrates the tilt (elevation) readback decode for heads whose
// tilt readback is a raw encoder count rather than the Pelco-standard hundredths
// of a degree. The 303Z/3050DZ is such a head: its tilt readback is the linear
// count raw = raw_at_0 + scale*elev, where scale = (raw_at_90 - raw_at_0)/90
// (~355.878 counts/deg, a ~3.559:1 gear ratio — the gear ratio is a physical
// constant and does not change when the rotator is re-homed; only the zero
// offset shifts). Calibrated live: raw_at_0 = 22456, raw_at_90 = 54485 (the
// head was re-homed after an earlier calibration that read 13408/45437; the
// offset shifted by +9048 counts, scale unchanged). Pan readback stays
// Pelco-standard hundredths.
//
// Two calibration points define the linear map. When raw_at_90 is zero (unset)
// the decode falls back to the Pelco standard (hundredths of a degree: offset 0,
// scale 100), so the in-memory simulator and any standard Pelco head keep
// working without calibration. The same offset/scale is reused to ENCODE the
// absolute SetTilt word when goto commands an absolute tilt on a calibrated head.
type TiltCalConfig struct {
	RawAt0  float64 `yaml:"raw_at_0"`  // raw readback at 0° elevation (offset)
	RawAt90 float64 `yaml:"raw_at_90"` // raw readback at 90° elevation (defines scale); 0 = use Pelco-standard hundredths
}

// GotoConfig tunes the goto. Absolute positioning on the 303Z/3050DZ is
// achieved by closed-loop jog (the device ignores the Pelco-D absolute
// SetPan/SetTilt opcodes 0x4B/0x4D), so goto convergence is readback-driven.
// These knobs are exposed because the device's slew rate is only knowable from
// a live run; defaults are generous.
type GotoConfig struct {
	TimeoutSec      float64 `yaml:"timeout_sec"`       // safety timeout: stop if a goto has not converged (default 60)
	TiltSlewRate    float64 `yaml:"tilt_slew_rate"`    // tilt open-loop slew rate in deg/s at jog speed; >0 enables the open-loop-slew-then-halt-and-confirm path for the 303Z/3050DZ, whose tilt readback is a constant valid-checksum garbage stream while the motor runs (never the true position) — readback is trustworthy only once the motor halts. 0 = legacy closed-loop (works for the sim and any head with clean motion readback). Calibrate live by timing a sweep: travel_deg / measured_seconds; the correction loop tolerates a mis-calibrated rate, so an approximate value still converges.
	TiltMaxConfirms int     `yaml:"tilt_max_confirms"` // post-halt correction pulses before the goto timeout gives up (default 8); each correction is one open-loop pulse (~1–2s on real hw), so 8 stays within the 60s timeout and tolerates a roughly-calibrated slew rate
}

// Default returns the built-in defaults (mirrors the original flag defaults).
// Inbound servers are disabled and bound to localhost for least-privilege.
func Default() Config {
	return Config{
		Transport:  TransportSerial,
		Serial:     SerialConfig{Port: "/dev/tty.usbmodem5AF50020681", Baud: 2400},
		TCP:        TCPConfig{Address: "127.0.0.1:4001"},
		Sim:        SimConfig{StartPan: 0, StartTilt: 0, JogStep: 5},
		SelfCheck:  SelfCheckConfig{Disable: true},
		Addr:       1,
		Protocol:   ProtocolD,
		Log:        "pelcots.log",
		LogLevel:   LogInfo,
		AzOffset:   0,
		TiltInvert: false,
		Control: ControlConfig{
			Bind:    "127.0.0.1",
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
