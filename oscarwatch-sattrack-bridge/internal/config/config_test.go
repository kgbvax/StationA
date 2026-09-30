package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(&Flags{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Slot.Station != "uhf" || cfg.Slot.Slot != "sat-track" || cfg.MQTT.Site != "muehle" {
		t.Fatalf("slot defaults: %+v %+v", cfg.Slot, cfg.MQTT)
	}
	if cfg.Source.URL != "ws://192.168.1.197:7373/" || cfg.Source.PingInterval != 30*time.Second {
		t.Fatalf("source defaults: %+v", cfg.Source)
	}
	if cfg.MQTT.ClientID != "muehle-uhf-sat-track" || cfg.Host != "scmino" || cfg.Location != "bauwagen" {
		t.Fatalf("identity defaults: client %q host %q location %q", cfg.MQTT.ClientID, cfg.Host, cfg.Location)
	}
	if obs, _ := cfg.Observer(); obs != nil {
		t.Fatalf("observer without station config: %+v", obs)
	}
}

func TestLoadTOML(t *testing.T) {
	path := writeConfig(t, `
host     = "scmino"
location = "bauwagen"

[slot]
station = "uhf"
slot    = "sat-track"

[source]
url           = "ws://bwpc:7373/"
ping_interval = "45s"

[station]
locator = "JO32WE"
alt_m   = 40

[mqtt]
broker = "tcp://192.168.1.50:1883"
user   = "hf"
site   = "muehle"

[log]
level = "DEBUG"
`)
	cfg, err := Load(&Flags{ConfigPath: path})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Source.URL != "ws://bwpc:7373/" || cfg.Source.PingInterval != 45*time.Second {
		t.Fatalf("source: %+v", cfg.Source)
	}
	if cfg.Log.Level != "debug" {
		t.Fatalf("log level %q not lowercased", cfg.Log.Level)
	}
	obs, desc := cfg.Observer()
	if obs == nil || obs.Lat != 52.1875 || obs.Lng != 7.875 || obs.AltM != 40 || !strings.Contains(desc, "JO32WE") {
		t.Fatalf("observer %+v (%s)", obs, desc)
	}
}

func TestLatLonBeatsLocator(t *testing.T) {
	path := writeConfig(t, "[station]\nlocator = \"JO32WE\"\nlat = 52.2\nlon = 7.9\n")
	cfg, err := Load(&Flags{ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if obs, _ := cfg.Observer(); obs == nil || obs.Lat != 52.2 || obs.Lng != 7.9 {
		t.Fatalf("observer %+v, want explicit lat/lon", obs)
	}
}

func TestZeroPingDisables(t *testing.T) {
	cfg, err := Load(&Flags{ConfigPath: writeConfig(t, "[source]\nping_interval = \"0s\"\n")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.PingInterval != 0 {
		t.Fatalf("ping_interval = %v, want 0", cfg.Source.PingInterval)
	}
}

func TestValidation(t *testing.T) {
	for name, body := range map[string]string{
		"http scheme":   "[source]\nurl = \"http://bwpc:7373/\"\n",
		"no host":       "[source]\nurl = \"ws:///\"\n",
		"ping too fast": "[source]\nping_interval = \"100ms\"\n",
		"lat only":      "[station]\nlat = 52.0\n",
		"lat range":     "[station]\nlat = 95.0\nlon = 7.0\n",
		"bad locator":   "[station]\nlocator = \"XX99\"\n",
		"bad level":     "[log]\nlevel = \"loud\"\n",
		"bad toml":      "[source\n",
	} {
		if _, err := Load(&Flags{ConfigPath: writeConfig(t, body)}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExplicitConfigMustExist(t *testing.T) {
	if _, err := Load(&Flags{ConfigPath: "/nonexistent/config.toml"}); err == nil {
		t.Fatal("explicit missing config accepted")
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv(EnvPrefix+"MQTT_PASSWORD", "s3cret")
	t.Setenv(EnvPrefix+"SOURCE_URL", "ws://10.0.0.5:7373/")
	cfg, err := Load(&Flags{ConfigPath: writeConfig(t, "[mqtt]\npassword = \"toml\"\n")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MQTT.Password != "s3cret" || cfg.Source.URL != "ws://10.0.0.5:7373/" {
		t.Fatalf("env not applied: password %q url %q", cfg.MQTT.Password, cfg.Source.URL)
	}
}

func TestCheck(t *testing.T) {
	cfg := Defaults()
	fillDefaults(&cfg)
	if _, err := Check(cfg, false); err == nil || !strings.Contains(err.Error(), "no MQTT password") {
		t.Fatalf("missing password not reported: %v", err)
	}

	t.Setenv(EnvPrefix+"MQTT_PASSWORD", "s3cret")
	cfg.MQTT.Password = "s3cret"
	lines, err := Check(cfg, false)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "s3cret") {
		t.Fatal("password leaked into the check summary")
	}
	for _, want := range []string{"muehle/uhf/sat-track", "ws://192.168.1.197:7373/", "ping 30s", "env " + EnvPrefix + "MQTT_PASSWORD"} {
		if !strings.Contains(joined, want) {
			t.Errorf("summary lacks %q:\n%s", want, joined)
		}
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load(&Flags{ConfigPath: "../../config.example.toml"})
	if err != nil {
		t.Fatalf("config.example.toml: %v", err)
	}
	if obs, _ := cfg.Observer(); obs == nil || cfg.Slot.Slot != "sat-track" {
		t.Fatalf("example config: observer %+v slot %q", obs, cfg.Slot.Slot)
	}
}
