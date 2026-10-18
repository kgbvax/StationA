package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaults(t *testing.T) {
	t.Setenv(EnvPrefix+"MQTT_PASSWORD", "")
	p := writeConfig(t, "")
	cfg, err := Load(&Flags{ConfigPath: p})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Site != "muehle" || cfg.MQTT.Station != "power" || cfg.MQTT.Slot != "shack-pc" {
		t.Errorf("slot address = %s/%s/%s", cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot)
	}
	if cfg.MQTT.ClientID != "muehle-power-shack-pc" {
		t.Errorf("client id = %q", cfg.MQTT.ClientID)
	}
	if cfg.Sleep.MinUserIdleMinutes != 30 {
		t.Errorf("min idle = %d, want 30", cfg.Sleep.MinUserIdleMinutes)
	}
	if cfg.State.IntervalS != 30 {
		t.Errorf("interval = %d, want 30", cfg.State.IntervalS)
	}
	if cfg.Host != "bwpc" || cfg.Device.Serial != "bwpc" {
		t.Errorf("host = %q serial = %q", cfg.Host, cfg.Device.Serial)
	}
}

func TestExplicitMissingConfigIsError(t *testing.T) {
	_, err := Load(&Flags{ConfigPath: filepath.Join(t.TempDir(), "nope.toml")})
	if err == nil {
		t.Fatal("want error for an explicitly named missing config")
	}
}

func TestPasswordEnvWinsOverFile(t *testing.T) {
	p := writeConfig(t, "[mqtt]\npassword = \"fromfile\"\n")

	t.Setenv(EnvPrefix+"MQTT_PASSWORD", "")
	cfg, err := Load(&Flags{ConfigPath: p})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Password != "fromfile" {
		t.Errorf("file fallback: password = %q", cfg.MQTT.Password)
	}

	t.Setenv(EnvPrefix+"MQTT_PASSWORD", "fromenv")
	cfg, err = Load(&Flags{ConfigPath: p})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Password != "fromenv" {
		t.Errorf("env: password = %q", cfg.MQTT.Password)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	for name, body := range map[string]string{
		"zero idle":      "[sleep]\nmin_user_idle_minutes = 0\n",
		"short interval": "[state]\ninterval_s = 1\n",
		"bad level":      "[log]\nlevel = \"loud\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(&Flags{ConfigPath: writeConfig(t, body)})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Validate() == nil {
				t.Error("want validation error")
			}
		})
	}
}

func TestLogFileRelativeToConfig(t *testing.T) {
	p := writeConfig(t, "[log]\nfile = \"agent.log\"\n")
	cfg, err := Load(&Flags{ConfigPath: p})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(p), "agent.log")
	if got := cfg.LogFilePath(); got != want {
		t.Errorf("log file = %q, want %q", got, want)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	t.Setenv(EnvPrefix+"MQTT_PASSWORD", "")
	cfg, err := Load(&Flags{ConfigPath: "../../config.example.toml"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Slot != "shack-pc" || cfg.Sleep.MinUserIdleMinutes != 30 || cfg.Log.File == "" {
		t.Errorf("example config = %+v", cfg)
	}
}
