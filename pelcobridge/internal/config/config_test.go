package config

import (
	"path/filepath"
	"testing"
)

func TestLoadMissingReturnsDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if c.Transport != TransportSerial || c.Addr != 1 {
		t.Fatalf("missing file did not return defaults: %+v", c)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pelcots.yaml")
	want := Default()
	want.Transport = TransportTCP
	want.TCP.Address = "10.0.0.5:4001"
	want.Addr = 7
	want.Control.Rotctld = ServerConfig{Enabled: true, Port: 4533}
	want.Control.PstRotator = ServerConfig{Enabled: true, Port: 12000}
	want.Wrap = WrapConfig{Enabled: true, Limit: 270, Accumulated: -182.5}

	if err := Save(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Transport != want.Transport || got.TCP.Address != want.TCP.Address ||
		got.Addr != want.Addr || got.Control.Rotctld != want.Control.Rotctld ||
		got.Control.PstRotator != want.Control.PstRotator ||
		got.Wrap != want.Wrap {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}
