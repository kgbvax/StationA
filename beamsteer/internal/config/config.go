// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads beamsteer's configuration.
//
// Precedence (highest wins): explicit CLI flag > config-file value > built-in
// default. The file carries the MQTT password, so on the target it must be
// 0600. See docs/conventions/config-and-secrets.md.
package config

import (
	"fmt"
	"os"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// MQTT holds the broker settings.
type MQTT struct {
	Broker   string `toml:"broker"`
	ClientID string `toml:"client_id"`
	Site     string `toml:"site"`
	Station  string `toml:"station"`
	Slot     string `toml:"slot"`
	User     string `toml:"user"`
	Password string `toml:"password"`
}

// PstRotator is the UDP listener the contest logger talks to.
type PstRotator struct {
	Bind string `toml:"bind"`
	Port int    `toml:"port"`
	// Reply is the AZ? reply shape: "manual" ("AZ:xxx.x\r", the PstRotator
	// manual) or "xml" ("<PST><AZIMUTH>nnn</AZIMUTH></PST>", what
	// wrc-rotator-bridge answers).
	Reply string `toml:"reply"`
}

// Steer holds the decision parameters and the sibling slots.
type Steer struct {
	LobeDeg      float64 `toml:"lobe_deg"`       // forward/reverse lobe half-width
	BidirLobeDeg float64 `toml:"bidir_lobe_deg"` // bidirectional lobe half-width
	// MaxAz is the highest rotator target. 360 until the G-450 overlap
	// (360..450) readback is verified on the hardware.
	MaxAz       float64 `toml:"max_az"`
	RotatorSlot string  `toml:"rotator_slot"`
	AntCtrlSlot string  `toml:"ant_ctrl_slot"`
	RadioSlot   string  `toml:"radio_slot"`
}

// Config is the full runtime configuration.
type Config struct {
	Location string `toml:"location"`
	Host     string `toml:"host"`

	MQTT       MQTT       `toml:"mqtt"`
	PstRotator PstRotator `toml:"pstrotator"`
	Steer      Steer      `toml:"steer"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		MQTT:       MQTT{Slot: "beam-steer"},
		PstRotator: PstRotator{Bind: "0.0.0.0", Port: 12050, Reply: "manual"},
		Steer: Steer{
			LobeDeg:      30,
			BidirLobeDeg: 45,
			MaxAz:        360,
			RotatorSlot:  "rotator",
			AntCtrlSlot:  "ant-ctrl",
			RadioSlot:    "radio",
		},
	}
}

// Load reads the TOML file at path over the defaults. A missing file is
// returned as an error wrapping fs.ErrNotExist.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return Default(), fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate returns all problems joined.
func (c Config) Validate() error {
	var p []string
	if c.MQTT.Site == "" || c.MQTT.Station == "" || c.MQTT.Slot == "" {
		p = append(p, "mqtt.site, mqtt.station and mqtt.slot are required")
	}
	if c.Location == "" || c.Host == "" {
		p = append(p, "location and host are required (published in /meta)")
	}
	if c.PstRotator.Port <= 0 || c.PstRotator.Port > 65534 {
		p = append(p, "pstrotator.port must be 1..65534 (replies go to port+1)")
	}
	if c.PstRotator.Reply != "manual" && c.PstRotator.Reply != "xml" {
		p = append(p, `pstrotator.reply must be "manual" or "xml"`)
	}
	if c.Steer.LobeDeg <= 0 || c.Steer.LobeDeg >= 90 {
		p = append(p, "steer.lobe_deg must be in (0, 90)")
	}
	if c.Steer.BidirLobeDeg <= 0 || c.Steer.BidirLobeDeg >= 90 {
		p = append(p, "steer.bidir_lobe_deg must be in (0, 90)")
	}
	if c.Steer.MaxAz < 360 || c.Steer.MaxAz > 450 {
		p = append(p, "steer.max_az must be in [360, 450]")
	}
	if c.Steer.RotatorSlot == "" || c.Steer.AntCtrlSlot == "" || c.Steer.RadioSlot == "" {
		p = append(p, "steer.rotator_slot, steer.ant_ctrl_slot and steer.radio_slot are required")
	}
	if len(p) > 0 {
		return fmt.Errorf("config: %s", strings.Join(p, "; "))
	}
	return nil
}
