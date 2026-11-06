package config

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func load(t *testing.T, toml string) Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.toml")
	if toml != "" {
		if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	f := RegisterFlags(fs)
	f.ConfigPath = p
	c, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaults(t *testing.T) {
	c := load(t, "")
	if c.Tuner.Address != "192.168.1.145:9010" || c.MQTT.Slot != "tuner" || c.Device.Link != "ethernet" {
		t.Fatalf("%+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if d, _ := c.PollDuration(); d != 250*time.Millisecond {
		t.Fatalf("poll %v", d)
	}
}

func TestFileAndEnv(t *testing.T) {
	t.Setenv("TGXL_TUNER_BRIDGE_MQTT_PASSWORD", "s3cret")
	t.Setenv("TGXL_TUNER_BRIDGE_TUNER_ADDRESS", "10.0.0.9:9010")
	c := load(t, "[tuner]\npoll_interval = \"500ms\"\nauth_code = \"123456\"\n")
	if c.MQTT.Password != "s3cret" || c.Tuner.Address != "10.0.0.9:9010" || c.Tuner.AuthCode != "123456" {
		t.Fatalf("%+v", c)
	}
	if d, _ := c.PollDuration(); d != 500*time.Millisecond {
		t.Fatalf("poll %v", d)
	}
}

func TestValidate(t *testing.T) {
	c := Defaults()
	c.MQTT.Site = ""
	if c.Validate() == nil {
		t.Fatal("empty site must fail")
	}
	c = Defaults()
	c.Tuner.PollInterval = "5ms"
	if c.Validate() == nil {
		t.Fatal("too fast poll must fail")
	}
	c.Tuner.PollInterval = "abc"
	if c.Validate() == nil {
		t.Fatal("garbage poll must fail")
	}
	c = Defaults()
	c.Tuner.TxPollInterval = "1ms"
	if c.Validate() == nil {
		t.Fatal("too fast tx poll must fail")
	}
}
