// Package config loads pelcobridge2's on-disk configuration.
//
// The config file is a single TOML document that also carries the MQTT user,
// so on the target machine it must be stored with restrictive (0600)
// permissions. The password itself is NOT in the file: it comes from the
// environment (PELCOBRIDGE2_MQTT_PASSWORD). See the stationa config-and-secrets
// convention.
//
// Precedence (highest wins): explicit CLI flag > environment > config file >
// built-in default. The config file is optional (seed-once deploy).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"pelcobridge2/internal/control"
	"pelcobridge2/internal/mqtt"
	"pelcobridge2/internal/pelco"
)

// Serial holds the RS-485 link settings.
type Serial struct {
	Port   string `toml:"port"`    // e.g. COM3, /dev/serial/by-id/...
	Baud   int    `toml:"baud"`    // 2400 on the bench link
	Addr   byte   `toml:"addr"`    // head's DIP address
	PelcoP bool   `toml:"pelco_p"` // TX envelope; RX is always adaptive
}

// Rotctld holds the hamlib rotctld TCP server settings.
type Rotctld struct {
	Enabled bool   `toml:"enabled"`
	Bind    string `toml:"bind"` // e.g. "0.0.0.0" or "127.0.0.1"
	Port    int    `toml:"port"` // default 4533
}

// Control holds the engine windows. Zero values mean "use the bench default".
type Control struct {
	JogSpeed           int     `toml:"jog_speed"`         // 0x00–0x3F, default 0x12
	SettleMS           int     `toml:"settle_ms"`         // quiet window around absolute sets
	SetAttempts        int     `toml:"set_attempts"`      // verification-ladder re-sends
	SetToleranceDeg    float64 `toml:"set_tolerance_deg"` // degrees
	ArmMaxReadbackAgeS float64 `toml:"arm_max_readback_age_s"`
	JogHoldMS          int     `toml:"jog_hold_ms"` // TUI hold-to-move window
	MinAz              float64 `toml:"min_az"`
	MaxAz              float64 `toml:"max_az"`
	MinEl              float64 `toml:"min_el"`
	MaxEl              float64 `toml:"max_el"`
}

// Log holds optional file logging.
type Log struct {
	File string `toml:"file"` // empty disables the file log
}

// MQTTSection is the [mqtt] TOML block; mapped onto mqtt.Config at wiring time.
type MQTT struct {
	Enabled  bool   `toml:"enabled"`
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	User     string `toml:"user"`
	Password string `toml:"password"` // prefer PELCOBRIDGE2_MQTT_PASSWORD
	Site     string `toml:"site"`
	Station  string `toml:"station"`
	Slot     string `toml:"slot"`

	DeviceModel string `toml:"device_model"`
	DeviceName  string `toml:"device_name"`
	DeviceLink  string `toml:"device_link"`
	Host        string `toml:"host"`
}

// Config is the full runtime configuration.
type Config struct {
	Serial  Serial  `toml:"serial"`
	Rotctld Rotctld `toml:"rotctld"`
	Control Control `toml:"control"`
	MQTT    MQTT    `toml:"mqtt"`
	Log     Log     `toml:"log"`
}

// Default returns the built-in defaults, matching the plan's tables.
func Default() Config {
	return Config{
		Serial:  Serial{Port: "", Baud: 2400, Addr: 1},
		Rotctld: Rotctld{Enabled: true, Bind: "0.0.0.0", Port: 4533},
		Control: Control{
			JogSpeed:           int(pelco.DefaultJogSpeed), // 0x12 = "12"
			SettleMS:           2000,
			SetAttempts:        3,
			SetToleranceDeg:    0.3,
			ArmMaxReadbackAgeS: 10,
			JogHoldMS:          250,
			MinAz:              0, MaxAz: 360, MinEl: 0, MaxEl: 90,
		},
		MQTT: MQTT{
			Enabled: false,
			Broker:  "tcp://192.168.1.50:1883",
			Site:    "muehle", Station: "uhf", Slot: "rotator",
			DeviceModel: "PTS-303Z/3050DZ",
			DeviceName:  "UHF Rotator",
			DeviceLink:  "rs485",
		},
	}
}

// EngineConfig maps the TOML control section onto the engine's Config. Out-of-
// range values are clamped here, once, rather than becoming wrong wire bytes:
// jog_speed 300 truncated to a byte is 0x2C — a silent speed change.
func (c Config) EngineConfig() control.Config {
	jog := c.Control.JogSpeed
	if jog < 0 || jog > int(pelco.MaxSpeed) {
		jog = int(pelco.DefaultJogSpeed)
	}
	settleMS := c.Control.SettleMS
	if settleMS < 0 {
		settleMS = Default().Control.SettleMS
	}
	attempts := c.Control.SetAttempts
	if attempts < 1 {
		attempts = 1
	}
	ageS := c.Control.ArmMaxReadbackAgeS
	if ageS < 0 {
		ageS = Default().Control.ArmMaxReadbackAgeS
	}
	return control.Config{
		Addr:              c.Serial.Addr,
		Baud:              c.Serial.Baud,
		PelcoP:            c.Serial.PelcoP,
		JogSpeed:          byte(jog),
		Settle:            time.Duration(settleMS) * time.Millisecond,
		SetAttempts:       attempts,
		SetTolerance:      c.Control.SetToleranceDeg,
		ArmMaxReadbackAge: time.Duration(ageS * float64(time.Second)),
	}
}

// MQTTConfig maps the TOML section onto the slot config. The password comes
// from the environment (PELCOBRIDGE2_MQTT_PASSWORD); the TOML [mqtt] password
// is the fallback for hosts with no process environment to speak of (a
// double-clicked Windows exe) — the file itself is 0600.
func (c Config) MQTTConfig(password string) mqtt.Config {
	if password == "" {
		password = c.MQTT.Password
	}
	return mqtt.Config{
		Enabled:     c.MQTT.Enabled,
		Broker:      c.MQTT.Broker,
		ClientID:    c.MQTT.ClientID,
		User:        c.MQTT.User,
		Password:    password,
		Site:        c.MQTT.Site,
		Station:     c.MQTT.Station,
		Slot:        c.MQTT.Slot,
		DeviceModel: c.MQTT.DeviceModel,
		DeviceName:  c.MQTT.DeviceName,
		DeviceLink:  c.MQTT.DeviceLink,
		Host:        c.MQTT.Host,
	}
}

// RotctldAddr is the listen address for the rotctld server.
func (c Config) RotctldAddr() string {
	return fmt.Sprintf("%s:%d", c.Rotctld.Bind, c.Rotctld.Port)
}

// Load reads the TOML file at path over the defaults. The file is optional
// (seed-once deploy): ResolvePath only returns paths it has verified to exist,
// so a NOT-FOUND here means the operator explicitly asked for a file (flag or
// PELCOBRIDGE2_CONFIG) that is not there — that must fail loudly, not silently
// fall back to defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, fmt.Errorf("config file %s not found", path)
		}
		return cfg, err
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// ResolvePath picks the config path: explicit flag > env > exe-dir/config.toml
// (Windows double-click friendly) > ./config.toml. Empty means no file found.
func ResolvePath(flagPath, envPath string) string {
	if flagPath != "" {
		return flagPath
	}
	if envPath != "" {
		return envPath
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config.toml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if _, err := os.Stat("config.toml"); err == nil {
		return "config.toml"
	}
	return ""
}
