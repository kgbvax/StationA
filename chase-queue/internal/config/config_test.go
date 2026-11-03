package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, s string) *Flags {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Flags{ConfigPath: p}
}

func TestDefaultsAndEnv(t *testing.T) {
	t.Setenv("CHASE_QUEUE_CLUBLOG_TOKEN", "cl_x")
	t.Setenv("CHASE_QUEUE_MQTT_PASSWORD", "pw")
	cfg, err := Load(write(t, "[clublog]\ncall = \"DL1ABC\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClubLog.Token != "cl_x" || cfg.MQTT.Password != "pw" || cfg.Slot.Slot != "chase" || cfg.HTTP.Listen != "127.0.0.1:8095" {
		t.Fatalf("%+v", cfg)
	}
}

func TestValidation(t *testing.T) {
	for name, body := range map[string]string{
		"listen without allow_from": "[log4om]\nlisten = \"0.0.0.0:2340\"\n",
		"bad allow_from":            "[log4om]\nlisten = \"0.0.0.0:2340\"\nallow_from = [\"nope\"]\n",
		"refresh too fast":          "[clublog]\nrefresh_minutes = 1\n",
	} {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	t.Setenv("CHASE_QUEUE_CLUBLOG_TOKEN", "cl_x")
	if _, err := Load(write(t, "")); err == nil {
		t.Error("token without call accepted")
	}
	if _, err := Load(write(t, "[log4om]\nlisten = \"0.0.0.0:2340\"\nallow_from = [\"192.168.1.0/24\"]\n[clublog]\ncall=\"X\"\n")); err != nil {
		t.Error(err)
	}
}

func TestTokenNotInTOML(t *testing.T) {
	cfg, err := Load(write(t, "[clublog]\ntoken = \"cl_in_file\"\ncall = \"X\"\n"))
	if err != nil || cfg.ClubLog.Token != "" {
		t.Fatalf("token read from the file: %v %q", err, cfg.ClubLog.Token)
	}
}

func TestQRZPasswordNeedsUser(t *testing.T) {
	c := Defaults()
	c.QRZ.Password = "x"
	if c.Validate() == nil {
		t.Fatal("password without user accepted")
	}
	c.QRZ.User = "DL1ABC"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestStationLogSection(t *testing.T) {
	cfg, err := Load(write(t, "[cluster]\naddress = \"h:23\"\ncall = \"DL9ET\"\n"))
	if err != nil || cfg.StationLog.Enabled() {
		t.Fatalf("absent section must leave it off: %v %+v", err, cfg.StationLog)
	}
	cfg, err = Load(write(t, "[cluster]\naddress = \"h:23\"\ncall = \"DL9ET\"\n[station_log]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.StationLog; s.URL != "http://127.0.0.1:8096" || s.OwnCall != "DL9ET" || s.Slot != "station-log" || s.Station != "hf" || s.ReloadMinutes != 5 {
		t.Fatalf("%+v", s)
	}
	cfg, err = Load(write(t, "[station_log]\nurl = \"http://scmino:8096\"\nown_call = \"DL1ABC\"\nreload_minutes = 2\n"))
	if err != nil || cfg.StationLog.URL != "http://scmino:8096" || cfg.StationLog.OwnCall != "DL1ABC" || cfg.StationLog.ReloadMinutes != 2 {
		t.Fatalf("%v %+v", err, cfg.StationLog)
	}
	for _, bad := range []string{"url = \"scmino:8096\"", "url = \"ftp://x/\"", "reload_minutes = -1"} {
		if _, err := Load(write(t, "[station_log]\n"+bad+"\n")); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
