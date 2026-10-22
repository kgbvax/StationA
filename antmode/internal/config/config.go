// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads antmode's configuration.
//
// Precedence (highest wins): explicit CLI flag > config-file value > built-in
// default. The file carries the MQTT password, so on the target it must be
// 0600. See docs/conventions/config-and-secrets.md.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// MQTT holds the broker settings and this slot's own address.
type MQTT struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	Site     string `toml:"site"`
	Station  string `toml:"station"`
	Slot     string `toml:"slot"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

// Slots are the sibling slots antmode reads (and, for antenna power,
// commands). Site-relative "<station>/<slot>": antenna power lives under
// muehle/power, the rest under muehle/hf.
type Slots struct {
	PowerSeq     string `toml:"power_seq"`
	Rotator      string `toml:"rotator"`
	AntCtrl      string `toml:"ant_ctrl"`
	AntSwitch    string `toml:"ant_switch"`
	Radio        string `toml:"radio"`
	AntennaPower string `toml:"antenna_power"`
}

// NINA is the warning feed (BBK NINA API, DWD warnings by district).
type NINA struct {
	BaseURL string `toml:"base_url"`
	// ARS is the Amtlicher Regionalschlüssel (12 digits). NINA serves the
	// district level only, so the last 7 digits are zeroed before the query.
	// Empty disables the warning poll (warnings stay unknown).
	ARS      string   `toml:"ars"`
	PollS    int      `toml:"poll_s"`
	TimeoutS int      `toml:"timeout_s"`
	IICodes  []string `toml:"ii_codes"`
}

// Wind is the DWD station forecast used for the storm heading.
type Wind struct {
	BaseURL string `toml:"base_url"`
	// DWDStation is the DWD station id (MOSMIX "Stationskennung", e.g.
	// "10637" or "G005"). Empty disables the poll (wind always stale →
	// storm.fallback_az).
	DWDStation   string `toml:"dwd_station"`
	PollS        int    `toml:"poll_s"`
	TimeoutS     int    `toml:"timeout_s"`
	StaleMinutes int    `toml:"stale_minutes"`
}

// Storm is the storm-mode policy.
type Storm struct {
	// FallbackAz is the park azimuth when the wind is stale.
	FallbackAz int `toml:"fallback_az"`
	// MaxAz is the highest rotator target. 360 until the G-450 overlap
	// (360..450) readback is verified on the hardware.
	MaxAz int `toml:"max_az"`
	// AutoEnter: enter storm automatically on a qualifying warning while the
	// station is unmanned. false = observe only (suggestions).
	AutoEnter          bool `toml:"auto_enter"`
	ExitHoldoffMinutes int  `toml:"exit_holdoff_minutes"`
}

// Maintenance is the maintenance-mode preset.
type Maintenance struct {
	ParkAz int `toml:"park_az"`
}

// Park holds the lock-sequencing timeouts and tolerances.
type Park struct {
	GroundTimeoutS  int `toml:"ground_timeout_s"`
	RetractTimeoutS int `toml:"retract_timeout_s"`
	RotateTimeoutS  int `toml:"rotate_timeout_s"`
	ToleranceDeg    int `toml:"tolerance_deg"`
	DriftWarnDeg    int `toml:"drift_warn_deg"`
}

// Power is the antenna-power borrow.
type Power struct {
	Borrow          bool `toml:"borrow"`
	DevicesTimeoutS int  `toml:"devices_timeout_s"`
}

// State is the persistence location.
type State struct {
	Path string `toml:"path"`
}

// Config is the full runtime configuration.
type Config struct {
	Location string `toml:"location"`
	Host     string `toml:"host"`

	MQTT        MQTT        `toml:"mqtt"`
	Slots       Slots       `toml:"slots"`
	NINA        NINA        `toml:"nina"`
	Wind        Wind        `toml:"wind"`
	Storm       Storm       `toml:"storm"`
	Maintenance Maintenance `toml:"maintenance"`
	Park        Park        `toml:"park"`
	Power       Power       `toml:"power"`
	State       State       `toml:"state"`
}

// DefaultIICodes is the qualifying DWD event list: wind level 1 "WINDBÖEN"
// and up, plus every thunderstorm warning that carries gusts. Source: DWD
// "Warnungen (WW/WU)" code table (warning_codes_pdf.pdf, Stand 01.02.2024),
// https://www.dwd.de/DE/leistungen/opendata/help/warnungen/warning_codes_pdf.pdf
//
//	31 GEWITTER (auch mit Windböen)                       level 1
//	33 STARKES GEWITTER (Sturmböen / schwere Sturmböen)    level 2
//	36, 38 STARKES GEWITTER (… Sturmböen … und Starkregen) level 2
//	40 SCHWERES GEWITTER mit ORKANBÖEN                     level 3
//	41 SCHWERES GEWITTER mit EXTREMEN ORKANBÖEN            level 4
//	42 SCHWERES GEWITTER mit HEFTIGEM STARKREGEN           level 3 (schwere Sturmböen)
//	44, 45, 46, 48, 49 SCHWERES GEWITTER (… Böen …)        level 3/4
//	51 WINDBÖEN, 52 STURMBÖEN, 53 SCHWERE STURMBÖEN        level 1/2/2
//	54 ORKANARTIGE BÖEN, 55 ORKANBÖEN, 56 EXTREME ORKANBÖEN level 3/3/4
//	95 SCHWERES GEWITTER mit EXTREM HEFTIGEM STARKREGEN und HAGEL (Sturmböen) level 4
//	96 EXTREMES GEWITTER mit ORKANBÖEN, …                  level 4
//
// Left out: 34 (thunderstorm with heavy rain only), 90–93 (standard product
// only, not in the CAP feed), 57/58 (Bodensee/Bavarian lakes), 11–16 (coast
// and high sea), 98/99 (tests).
var DefaultIICodes = []string{
	"31", "33", "36", "38",
	"40", "41", "42", "44", "45", "46", "48", "49",
	"51", "52", "53", "54", "55", "56",
	"95", "96",
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		MQTT: MQTT{Slot: "ant-mode"},
		Slots: Slots{
			PowerSeq:     "hf/power-seq",
			Rotator:      "hf/rotator",
			AntCtrl:      "hf/ant-ctrl",
			AntSwitch:    "hf/ant-switch",
			Radio:        "hf/radio",
			AntennaPower: "power/antennas",
		},
		NINA: NINA{
			BaseURL:  "https://warnung.bund.de/api31",
			PollS:    300,
			TimeoutS: 20,
			IICodes:  append([]string(nil), DefaultIICodes...),
		},
		Wind: Wind{
			BaseURL:      "https://app-prod-ws.warnwetter.de/v30",
			PollS:        1800,
			TimeoutS:     20,
			StaleMinutes: 180,
		},
		Storm: Storm{
			FallbackAz:         0,
			MaxAz:              360,
			AutoEnter:          false,
			ExitHoldoffMinutes: 60,
		},
		Park: Park{
			GroundTimeoutS:  60,
			RetractTimeoutS: 120,
			RotateTimeoutS:  300,
			ToleranceDeg:    3,
			DriftWarnDeg:    10,
		},
		Power: Power{Borrow: false, DevicesTimeoutS: 240},
		State: State{Path: "/var/lib/antmode/state.json"},
	}
}

// Load reads the TOML file at path over the defaults. A missing file is
// returned as an error wrapping fs.ErrNotExist. Unknown keys are an error: a
// misspelt safety setting must not silently fall back to its default.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	dec := toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		var sme *toml.StrictMissingError
		if errors.As(err, &sme) {
			return Default(), fmt.Errorf("parse config %s: unknown key(s):\n%s", path, sme.String())
		}
		return Default(), fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

var (
	arsRE  = regexp.MustCompile(`^[0-9]{12}$`)
	iiRE   = regexp.MustCompile(`^[0-9]{1,3}$`)
	slotRE = regexp.MustCompile(`^[a-z0-9_-]+/[a-z0-9_-]+$`)
)

// Validate returns all problems joined.
func (c Config) Validate() error {
	var p []string
	if c.MQTT.Site == "" || c.MQTT.Station == "" || c.MQTT.Slot == "" {
		p = append(p, "mqtt.site, mqtt.station and mqtt.slot are required")
	}
	if c.Location == "" || c.Host == "" {
		p = append(p, "location and host are required (published in /meta)")
	}
	for k, v := range map[string]string{
		"power_seq": c.Slots.PowerSeq, "rotator": c.Slots.Rotator, "ant_ctrl": c.Slots.AntCtrl,
		"ant_switch": c.Slots.AntSwitch, "radio": c.Slots.Radio, "antenna_power": c.Slots.AntennaPower,
	} {
		if !slotRE.MatchString(v) {
			p = append(p, fmt.Sprintf("slots.%s must be site-relative \"<station>/<slot>\" (got %q)", k, v))
		}
	}
	if c.NINA.ARS != "" && !arsRE.MatchString(c.NINA.ARS) {
		p = append(p, "nina.ars must be the 12-digit Regionalschlüssel")
	}
	if c.NINA.BaseURL == "" || c.Wind.BaseURL == "" {
		p = append(p, "nina.base_url and wind.base_url are required")
	}
	if c.NINA.PollS < 60 || c.Wind.PollS < 60 {
		p = append(p, "nina.poll_s and wind.poll_s must be >= 60 (public services)")
	}
	if c.NINA.TimeoutS <= 0 || c.Wind.TimeoutS <= 0 {
		p = append(p, "nina.timeout_s and wind.timeout_s must be > 0")
	}
	if len(c.NINA.IICodes) == 0 {
		p = append(p, "nina.ii_codes must not be empty")
	}
	for _, s := range c.NINA.IICodes {
		if !iiRE.MatchString(s) {
			p = append(p, fmt.Sprintf("nina.ii_codes: %q is not a DWD II code", s))
		}
	}
	if c.Wind.StaleMinutes <= 0 {
		p = append(p, "wind.stale_minutes must be > 0")
	}
	if c.Storm.MaxAz < 360 || c.Storm.MaxAz > 450 {
		p = append(p, "storm.max_az must be in [360, 450]")
	}
	if c.Storm.FallbackAz < 0 || c.Storm.FallbackAz > c.Storm.MaxAz {
		p = append(p, "storm.fallback_az must be in [0, storm.max_az]")
	}
	if c.Maintenance.ParkAz < 0 || c.Maintenance.ParkAz > c.Storm.MaxAz {
		p = append(p, "maintenance.park_az must be in [0, storm.max_az]")
	}
	if c.Storm.ExitHoldoffMinutes <= 0 {
		p = append(p, "storm.exit_holdoff_minutes must be > 0")
	}
	if c.Park.GroundTimeoutS <= 0 || c.Park.RetractTimeoutS <= 0 || c.Park.RotateTimeoutS <= 0 {
		p = append(p, "park.ground_timeout_s, park.retract_timeout_s and park.rotate_timeout_s must be > 0")
	}
	if c.Park.ToleranceDeg < 1 || c.Park.ToleranceDeg > 20 {
		p = append(p, "park.tolerance_deg must be in [1, 20]")
	}
	if c.Park.DriftWarnDeg <= 0 {
		p = append(p, "park.drift_warn_deg must be > 0")
	}
	if c.Power.DevicesTimeoutS < 180 {
		p = append(p, "power.devices_timeout_s must be >= 180 (the RCU-06 and WRC need minutes after power-on)")
	}
	if c.State.Path == "" {
		p = append(p, "state.path is required")
	}
	if len(p) > 0 {
		return fmt.Errorf("config: %s", strings.Join(p, "; "))
	}
	return nil
}

// SplitSlot splits a validated site-relative "<station>/<slot>".
func SplitSlot(rel string) (station, slot string) {
	station, slot, _ = strings.Cut(rel, "/")
	return station, slot
}
