package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	c := Defaults()
	if c.MQTT.Site != "muehle" || c.MQTT.Station != "hf" || c.MQTT.Slot != "pa" {
		t.Errorf("defaults slot = %s/%s/%s, want muehle/hf/pa", c.MQTT.Site, c.MQTT.Station, c.MQTT.Slot)
	}
	if c.MQTT.Broker != "tcp://192.168.1.50:1883" {
		t.Errorf("default broker = %q, want tcp://192.168.1.50:1883", c.MQTT.Broker)
	}
	if c.MQTT.PublishHADiscovery {
		t.Error("publish_ha_discovery must default to false (model §9 gate)")
	}
	if c.Serial.Port == "" || c.Serial.AvgTimeMs != 300 {
		t.Errorf("default serial = %+v, want non-empty port + avg 300", c.Serial)
	}
	if c.Device.Model != "ACOM 1200S" || c.Device.Link != "serial" {
		t.Errorf("default device = %+v", c.Device)
	}
	if c.Host != "shari" {
		t.Errorf("default host = %q, want shari", c.Host)
	}
}

func TestLoadMissingFileNonFatal(t *testing.T) {
	t.Setenv("ACOMBRIDGE_MQTT_PASSWORD", "") // ensure env doesn't taint
	c, err := Load(&Flags{ConfigPath: filepath.Join(t.TempDir(), "does-not-exist.toml")})
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

func TestLoadAppliesEnvPassword(t *testing.T) {
	// The password must come from the env (EnvironmentFile), not the TOML.
	t.Setenv("ACOMBRIDGE_MQTT_PASSWORD", "s3cr3t")
	t.Setenv("ACOMBRIDGE_MQTT_SITE", "envsite")
	t.Setenv("ACOMBRIDGE_MQTT_SLOT", "envpa")
	t.Setenv("ACOMBRIDGE_SERIAL_PORT", "/dev/env-port")

	c, err := Load(&Flags{ConfigPath: filepath.Join(t.TempDir(), "none.toml")})
	if err != nil {
		t.Fatal(err)
	}
	if c.MQTT.Password != "s3cr3t" {
		t.Errorf("password from env = %q, want s3cr3t", c.MQTT.Password)
	}
	if c.MQTT.Site != "envsite" {
		t.Errorf("site from env = %q, want envsite", c.MQTT.Site)
	}
	if c.MQTT.Slot != "envpa" {
		t.Errorf("slot from env = %q, want envpa", c.MQTT.Slot)
	}
	if c.Serial.Port != "/dev/env-port" {
		t.Errorf("serial port from env = %q, want /dev/env-port", c.Serial.Port)
	}
}

func TestLoadFlagLogLevelOverrides(t *testing.T) {
	c, err := Load(&Flags{ConfigPath: filepath.Join(t.TempDir(), "none.toml"), LogLevel: "debug"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Log.Level != "debug" {
		t.Errorf("log level = %q, want debug (flag override)", c.Log.Level)
	}
}

func TestLoadTomlOverridesDefaults(t *testing.T) {
	// Guard against ambient secrets: never let a real ACOMBRIDGE_MQTT_PASSWORD
	// taint the config under test (and never dump the whole MQTTConfig — it
	// carries the Password field — in an error message).
	t.Setenv("ACOMBRIDGE_MQTT_PASSWORD", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.toml")
	toml := `
host = "myhost"
[serial]
port = "/dev/myport"
avg_time_ms = 500
[mqtt]
site = "over"
station = "ride"
slot = "pa"
`
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(&Flags{ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if c.Serial.Port != "/dev/myport" || c.Serial.AvgTimeMs != 500 {
		t.Errorf("serial not overridden: port=%q avg=%d", c.Serial.Port, c.Serial.AvgTimeMs)
	}
	if c.MQTT.Site != "over" || c.MQTT.Station != "ride" {
		t.Errorf("mqtt not overridden: site=%q station=%q", c.MQTT.Site, c.MQTT.Station)
	}
	if c.Host != "myhost" {
		t.Errorf("host not overridden: %q", c.Host)
	}
}

func TestValidate(t *testing.T) {
	c := Defaults()
	if err := c.Validate(); err != nil {
		t.Errorf("valid defaults failed Validate: %v", err)
	}

	bad := Defaults()
	bad.MQTT.Site = ""
	if err := bad.Validate(); err == nil {
		t.Error("Validate should reject empty site")
	}

	bad2 := Defaults()
	bad2.Serial.Port = ""
	if err := bad2.Validate(); err == nil {
		t.Error("Validate should reject empty serial port")
	}
}
