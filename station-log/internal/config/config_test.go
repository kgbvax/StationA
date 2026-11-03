package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, toml string) (Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(&Flags{ConfigPath: p})
}

func TestDefaultsAndEnv(t *testing.T) {
	t.Setenv("STATION_LOG_MQTT_PASSWORD", "s3cret")
	c, err := load(t, `default_own_call = "DL9ET"`)
	if err != nil || c.Window() != 5*time.Minute || c.HTTP.Listen != "127.0.0.1:8096" || c.MQTT.Password != "s3cret" || c.Slot.Slot != "station-log" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestValidate(t *testing.T) {
	for toml, want := range map[string]string{
		`dup_window = "soon"`:                                       "dup_window",
		`dup_window = "2h"`:                                         "dup_window",
		"[http]\nlisten = \"0.0.0.0:8096\"":                         "allow_from",
		"[log4om]\nlisten = \"0.0.0.0:2340\"":                       "allow_from",
		"[http]\nlisten = \"nope\"":                                 "host:port",
		"[log4om]\nlisten = \"0.0.0.0:2340\"\nallow_from = [\"x\"]": "allow_from",
	} {
		if _, err := load(t, toml); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", toml, err, want)
		}
	}
	if _, err := load(t, "[http]\nlisten = \"0.0.0.0:8096\"\nallow_from = [\"192.168.1.0/24\"]\n[log4om]\nlisten = \"0.0.0.0:2340\"\nallow_from = [\"192.168.1.197\"]"); err != nil {
		t.Error(err)
	}
}
