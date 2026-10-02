package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsTargetLiveBrokerOnPort80(t *testing.T) {
	d := Default()
	if d.HTTPAddr != ":80" || d.MQTT.Broker != "tcp://192.168.1.50:1883" || d.MQTT.ClientID != "stationportal" {
		t.Fatalf("unexpected defaults: %+v", d)
	}
}

func TestLoadOverlaysFileAndEnvPassword(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte("http_addr = \":8099\"\n[mqtt]\npassword = \"file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PasswordEnv, "env")
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":8099" || c.MQTT.Password != "env" || c.MQTT.User != "hf" {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadMissingFileKeepsDefaultsAndEnv(t *testing.T) {
	t.Setenv(PasswordEnv, "env")
	c, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
	if c.MQTT.Password != "env" || c.HTTPAddr != ":80" {
		t.Fatalf("got %+v", c)
	}
}

func TestProbeBounds(t *testing.T) {
	c := Config{ProbeIntervalS: 1, ProbeTimeoutS: 60}
	if c.ProbeInterval() != 5*time.Second || c.ProbeTimeout() != 5*time.Second {
		t.Fatalf("interval %v timeout %v", c.ProbeInterval(), c.ProbeTimeout())
	}
}
