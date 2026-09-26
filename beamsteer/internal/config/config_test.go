// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import "testing"

func TestExampleConfigValid(t *testing.T) {
	cfg, err := Load("../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.PstRotator.Port != 12050 || cfg.Steer.BidirLobeDeg != 45 {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestValidateRejects(t *testing.T) {
	cfg := Default()
	cfg.MQTT.Site, cfg.MQTT.Station, cfg.Location, cfg.Host = "muehle", "hf", "x", "y"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	cfg.Steer.MaxAz = 500
	cfg.PstRotator.Reply = "json"
	if err := cfg.Validate(); err == nil {
		t.Error("want error")
	}
}
