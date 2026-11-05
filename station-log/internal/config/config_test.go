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

func TestWatchValidation(t *testing.T) {
	c := Defaults()
	if c.Watch.Dir != "" || c.Watch.IntervalSeconds != 30 || c.Watch.Source != "adif-drop" {
		t.Fatalf("%+v", c.Watch)
	}
	c.Watch.Dir = "/does/not/exist/yet" // missing dir is not fatal
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Watch.IntervalSeconds = 0
	if err := c.Validate(); err == nil {
		t.Fatal("interval 0 must fail")
	}
}

const workingOK = `
[working]
station_locator = "JO32WE"
stale_after = "5m"
[[working.listener]]
name = "dxlog"
kind = "n1mm"
listen = "0.0.0.0:12060"
allow_from = ["192.168.1.197"]
[[working.listener]]
name = "log4om"
kind = "log4om"
listen = "0.0.0.0:2249"
allow_from = ["192.168.1.0/24"]
[working.qrz]
user = "DL9ET"
`

func TestWorkingConfig(t *testing.T) {
	t.Setenv("STATION_LOG_QRZ_PASSWORD", "qrz-secret")
	c, err := load(t, workingOK)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Working.Enabled() || c.Working.Stale() != 5*time.Minute || len(c.Working.Listeners) != 2 ||
		c.Working.QRZ.Password != "qrz-secret" || c.Working.QRZ.CacheDays != 30 {
		t.Fatalf("%+v", c.Working)
	}
	if !strings.HasSuffix(c.QRZCachePath(), "qrz-cache.json") || !strings.HasPrefix(c.QRZCachePath(), c.DataDir) {
		t.Fatalf("cache path %q not under data_dir", c.QRZCachePath())
	}
	for _, l := range c.WorkingCheck() {
		if strings.Contains(l, "qrz-secret") {
			t.Fatalf("secret in -check output: %s", l)
		}
	}
	if d, err := load(t, `default_own_call = "DL9ET"`); err != nil || d.Working.Enabled() {
		t.Fatalf("working must be off by default: %+v %v", d.Working, err)
	}
}

func TestWorkingValidate(t *testing.T) {
	t.Setenv("STATION_LOG_QRZ_PASSWORD", "x")
	rep := func(old, new string) string { return strings.Replace(workingOK, old, new, 1) }
	for toml, want := range map[string]string{
		rep(`kind = "n1mm"`, `kind = "wsjtx"`):                          "kind",
		rep(`allow_from = ["192.168.1.197"]`, ``):                       "allow_from",
		rep(`allow_from = ["192.168.1.197"]`, `allow_from = ["nope"]`):  "allow_from",
		rep(`0.0.0.0:2249`, `0.0.0.0:12060`):                            "already used",
		rep(`0.0.0.0:12060`, `12060`):                                   "host:port",
		rep(`name = "log4om"`, `name = "dxlog"`):                        "duplicate",
		rep(`station_locator = "JO32WE"`, `station_locator = "ZZ99ZZ"`): "station_locator",
		rep(`stale_after = "5m"`, `stale_after = "soon"`):               "stale_after",
	} {
		if _, err := load(t, toml); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %q, got %v", want, err)
		}
	}
	t.Setenv("STATION_LOG_QRZ_PASSWORD", "")
	if _, err := load(t, workingOK); err == nil || !strings.Contains(err.Error(), "STATION_LOG_QRZ_PASSWORD") {
		t.Errorf("qrz user without password accepted: %v", err)
	}
}

func TestWavelogConfig(t *testing.T) {
	c, err := load(t, "")
	if err != nil || c.Wavelog.Enabled() {
		t.Fatalf("off by default: %+v %v", c.Wavelog, err)
	}
	prof := "\n[[wavelog.profile]]\nown_call = \"DL9ET\"\nstation_profile_id = \"1\"\n"
	for toml, want := range map[string]string{
		"[wavelog]\ndirection = \"sideways\"":                                      "direction",
		"[wavelog]\ndirection = \"both\"\nurl = \"http://wl.example.org\"" + prof:  "plain http",
		"[wavelog]\ndirection = \"both\"\nurl = \"https://wl.example.org\"" + prof: "STATION_LOG_WAVELOG_KEY",
		"[wavelog]\ndirection = \"both\"\nurl = \"https://wl.example.org\"":        "STATION_LOG_WAVELOG_KEY",
	} {
		t.Setenv("STATION_LOG_WAVELOG_KEY", "")
		if _, err := load(t, toml); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", toml, err, want)
		}
	}
	t.Setenv("STATION_LOG_WAVELOG_KEY", "k")
	if _, err := load(t, "[wavelog]\ndirection = \"both\"\nurl = \"https://wl.example.org\""); err == nil || !strings.Contains(err.Error(), "profile") {
		t.Errorf("profile required: %v", err)
	}
	c, err = load(t, "[wavelog]\ndirection = \"both\"\nurl = \"http://192.168.1.5\"\npush_since = \"2026-11-01\""+prof)
	if err != nil || !c.Wavelog.Push() || !c.Wavelog.Pull() || c.Wavelog.Key != "k" || c.Wavelog.PushSinceTime().Day() != 1 {
		t.Fatalf("%+v %v", c.Wavelog, err)
	}
	if _, err := load(t, "[wavelog]\ndirection = \"pull\"\nurl = \"https://wl.example.org\"\ninterval_seconds = 10"+prof); err == nil || !strings.Contains(err.Error(), "interval_seconds") {
		t.Errorf("interval: %v", err)
	}
	// the key can never come from the TOML
	c, _ = load(t, "[wavelog]\nkey = \"fromfile\"")
	if c.Wavelog.Key == "fromfile" {
		t.Fatal("key read from TOML")
	}
	if strings.Contains(c.WavelogCheck(), "k") && strings.Contains(c.WavelogCheck(), "fromfile") {
		t.Fatal("check leaks key")
	}
}
