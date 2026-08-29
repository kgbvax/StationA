package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	c := Defaults()
	if c.MQTT.Site != "muehle" || c.Host != "shari" {
		t.Errorf("defaults = site=%q host=%q, want muehle/shari", c.MQTT.Site, c.Host)
	}
	if c.MQTT.Broker != "tcp://127.0.0.1:1883" {
		t.Errorf("default broker = %q", c.MQTT.Broker)
	}
	if c.Log.Level != "info" {
		t.Errorf("default log level = %q, want info", c.Log.Level)
	}
}

func TestLoadMissingFileNonFatal(t *testing.T) {
	t.Setenv("SHELLY_POWER_BRIDGE_MQTT_PASSWORD", "")
	c, err := Load(&Flags{ConfigPath: filepath.Join(t.TempDir(), "none.toml")})
	if err != nil {
		t.Fatalf("missing config file should be non-fatal, got %v", err)
	}
	if c.MQTT.Site != "muehle" {
		t.Errorf("defaults not applied for missing file: site=%q", c.MQTT.Site)
	}
}

func TestLoadParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(path, []byte("[mqtt\nbroker = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(&Flags{ConfigPath: path}); err == nil {
		t.Fatal("expected parse error for malformed TOML, got nil")
	}
}

func TestLoadSlots(t *testing.T) {
	t.Setenv("SHELLY_POWER_BRIDGE_MQTT_PASSWORD", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.toml")
	toml := `
host = "shari"
[[slot]]
station = "power"
slot = "master"
shelly_id = "shellyplus1pm-aaaa"
device_model = "Shelly Plus 1PM"
device_serial = "shellyplus1pm-aaaa"
[[slot]]
station = "power"
slot = "psu-13v8"
shelly_id = "shellyplus1pm-bbbb"
device_model = "Shelly Plus 1PM"
device_serial = "shellyplus1pm-bbbb"
feeds = ["hf/radio", "uhf/radio"]
`
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(&Flags{ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Slots) != 2 {
		t.Fatalf("slots = %d, want 2", len(c.Slots))
	}
	if c.Slots[1].FailSafe != "off" {
		t.Errorf("fail_safe default = %q, want off", c.Slots[1].FailSafe)
	}
	if len(c.Slots[1].Feeds) != 2 {
		t.Errorf("feeds = %v, want 2 entries", c.Slots[1].Feeds)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("valid config failed Validate: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	t.Setenv("SHELLY_POWER_BRIDGE_MQTT_PASSWORD", "")
	c := Defaults()
	if err := c.Validate(); err == nil {
		t.Error("Validate should reject zero slots")
	}

	c = Defaults()
	c.Slots = []SlotConfig{{Station: "power", Slot: "master"}}
	if err := c.Validate(); err == nil {
		t.Error("Validate should reject slot without shelly_id")
	}

	c = Defaults()
	c.Slots = []SlotConfig{
		{Station: "power", Slot: "master", ShellyID: "a", DeviceSerial: "a"},
		{Station: "power", Slot: "master", ShellyID: "b", DeviceSerial: "b"},
	}
	if err := c.Validate(); err == nil {
		t.Error("Validate should reject duplicate slot addresses")
	}

	c = Defaults()
	c.Slots = []SlotConfig{{Station: "power", Slot: "master", ShellyID: "a", DeviceSerial: "a", FailSafe: "sleep"}}
	if err := c.Validate(); err == nil {
		t.Error("Validate should reject fail_safe other than on|off")
	}
}

func TestLoadAppliesEnvPassword(t *testing.T) {
	t.Setenv("SHELLY_POWER_BRIDGE_MQTT_PASSWORD", "s3cr3t")
	c, err := Load(&Flags{ConfigPath: filepath.Join(t.TempDir(), "none.toml")})
	if err != nil {
		t.Fatal(err)
	}
	if c.MQTT.Password != "s3cr3t" {
		t.Errorf("password from env = %q, want s3cr3t", c.MQTT.Password)
	}
}
