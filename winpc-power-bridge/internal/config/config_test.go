package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestTailscaleDefaultsAndFile(t *testing.T) {
	d := Defaults()
	if !d.Tailscale.PingAfterResume || d.Tailscale.Path != DefaultTailscalePath {
		t.Errorf("defaults = %+v", d.Tailscale)
	}
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte("[tailscale]\nping_after_resume = false\npath = ''\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(&Flags{ConfigPath: p})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tailscale.PingAfterResume {
		t.Error("ping_after_resume = false was not kept")
	}
	if cfg.Tailscale.Path != DefaultTailscalePath {
		t.Errorf("empty path = %q, want the default", cfg.Tailscale.Path)
	}
}

const programsTOML = `
[[program]]
name    = "wsjtx"
exe     = 'C:\Program Files\FlexRadio Systems\WSJT\wsjtx\bin\wsjtx.exe'
check   = "process"
restart_owned = true

[[program]]
name          = "aethersdr"
exe           = 'C:\AetherSDR\AetherSDR.exe'
args          = ["--rigctl"]
workdir       = 'C:\AetherSDR'
process       = "aethersdr.exe"
check         = "tcp"
check_address = "127.0.0.1:4532"
probe         = "f"
reply_pattern = '^[0-9]+$'
settle_s      = 2
check_timeout_s = 90
`

func TestProgramsLoadWithDefaults(t *testing.T) {
	cfg, err := Load(&Flags{ConfigPath: writeConfig(t, programsTOML)})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Programs) != 2 {
		t.Fatalf("programs = %+v", cfg.Programs)
	}
	wsjtx, aeth := cfg.Programs[0], cfg.Programs[1]
	if wsjtx.Process != "wsjtx.exe" {
		t.Errorf("wsjtx process = %q, want the exe base name", wsjtx.Process)
	}
	if wsjtx.SettleS != 5 || wsjtx.CheckTimeoutS != 60 || !wsjtx.RestartOwned {
		t.Errorf("wsjtx = %+v", wsjtx)
	}
	if aeth.Process != "aethersdr.exe" || aeth.SettleS != 2 || aeth.CheckTimeoutS != 90 ||
		aeth.CheckAddress != "127.0.0.1:4532" || aeth.Probe != "f" || len(aeth.Args) != 1 {
		t.Errorf("aethersdr = %+v", aeth)
	}
}

func TestProgramsValidate(t *testing.T) {
	for name, body := range map[string]string{
		"no name":       "[[program]]\nexe = 'a.exe'\ncheck = \"none\"\n",
		"bad name":      "[[program]]\nname = \"Wsjtx-1\"\nexe = 'a.exe'\ncheck = \"none\"\n",
		"no exe":        "[[program]]\nname = \"wsjtx\"\ncheck = \"none\"\n",
		"bad check":     "[[program]]\nname = \"wsjtx\"\nexe = 'a.exe'\ncheck = \"udp\"\n",
		"no check":      "[[program]]\nname = \"wsjtx\"\nexe = 'a.exe'\n",
		"duplicate":     "[[program]]\nname = \"wsjtx\"\nexe = 'a.exe'\ncheck = \"none\"\n[[program]]\nname = \"wsjtx\"\nexe = 'b.exe'\ncheck = \"none\"\n",
		"tcp no addr":   "[[program]]\nname = \"a\"\nexe = 'a.exe'\ncheck = \"tcp\"\nprobe = \"f\"\nreply_pattern = '.'\n",
		"tcp no probe":  "[[program]]\nname = \"a\"\nexe = 'a.exe'\ncheck = \"tcp\"\ncheck_address = \"127.0.0.1:1\"\nreply_pattern = '.'\n",
		"tcp no reply":  "[[program]]\nname = \"a\"\nexe = 'a.exe'\ncheck = \"tcp\"\ncheck_address = \"127.0.0.1:1\"\nprobe = \"f\"\n",
		"tcp bad regex": "[[program]]\nname = \"a\"\nexe = 'a.exe'\ncheck = \"tcp\"\ncheck_address = \"127.0.0.1:1\"\nprobe = \"f\"\nreply_pattern = '('\n",
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

func TestOwnedFileNextToConfig(t *testing.T) {
	p := writeConfig(t, "")
	cfg, err := Load(&Flags{ConfigPath: p})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(p), OwnedFileName)
	if got := cfg.OwnedFilePath(); got != want {
		t.Errorf("owned file = %q, want %q", got, want)
	}
}

// TestExampleProgramsValid uncomments the [[program]] example block of the
// example config and checks that it validates.
func TestExampleProgramsValid(t *testing.T) {
	data, err := os.ReadFile("../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	in := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "# [[program]]") {
			in = true
		}
		if in && strings.HasPrefix(line, "# ---") {
			break
		}
		if in {
			b.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "#"), " ") + "\n")
		}
	}
	cfg, err := Load(&Flags{ConfigPath: writeConfig(t, b.String())})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Programs) != 3 || cfg.Programs[1].Process != "wsjtx.exe" {
		t.Errorf("programs = %+v", cfg.Programs)
	}
}
