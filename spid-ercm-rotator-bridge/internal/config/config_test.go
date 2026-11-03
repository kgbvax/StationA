package config

import (
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// examplePath is the repo's checked-in example config, two directories up
// from this package.
func examplePath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "config.example.toml")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("example config: %v", err)
	}
	return p
}

func loadFile(t *testing.T, path string) (Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags := RegisterFlags(fs)
	_ = fs.Parse([]string{"-config", path})
	return Load(flags)
}

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Host != "shari" {
		t.Errorf("default host = %q, want shari", cfg.Host)
	}
	if cfg.MQTT.Broker != "tcp://192.168.1.178:1883" {
		t.Errorf("default broker = %q, want tcp://192.168.1.178:1883", cfg.MQTT.Broker)
	}
	if cfg.MQTT.Site != "muehle" || cfg.MQTT.Station != "uhf" {
		t.Errorf("default site/station = %q/%q, want muehle/uhf", cfg.MQTT.Site, cfg.MQTT.Station)
	}
	if cfg.MQTT.User != "hf" {
		t.Errorf("default mqtt user = %q, want hf", cfg.MQTT.User)
	}
	if cfg.Rotctld.Bind != "0.0.0.0" || cfg.Rotctld.Port != 4534 {
		t.Errorf("default rotctld = %q:%d, want 0.0.0.0:4534", cfg.Rotctld.Bind, cfg.Rotctld.Port)
	}
	if cfg.PstRotator.Bind != "0.0.0.0" || cfg.PstRotator.Port != 12041 {
		t.Errorf("default pstrotator = %q:%d, want 0.0.0.0:12041", cfg.PstRotator.Bind, cfg.PstRotator.Port)
	}
	if cfg.Control.PollInterval != "1s" || cfg.Control.PollIntervalDur.String() != "1s" {
		t.Errorf("default poll interval = %q (%s), want 1s", cfg.Control.PollInterval, cfg.Control.PollIntervalDur)
	}
	if cfg.Control.ReopenCooldown != "2s" || cfg.Control.ReopenCooldownDur.String() != "2s" {
		t.Errorf("default reopen cooldown = %q (%s), want 2s", cfg.Control.ReopenCooldown, cfg.Control.ReopenCooldownDur)
	}
	for _, ax := range []struct {
		name  string
		ctrl  AxisControl
		min   float64
		max   float64
		park  float64
		band  float64
		baud  int
		label string
	}{
		{"az", cfg.Control.AZ, 0, 360, 0, 4, 1200, "SPID Rotor (Rot1Prog)"},
		{"el", cfg.Control.EL, 0, 90, 0, 1, 9600, "ERC-M / GS-500"},
	} {
		if ax.ctrl.Min != ax.min || ax.ctrl.Max != ax.max {
			t.Errorf("default %s travel limits = [%v, %v], want [%v, %v]", ax.name, ax.ctrl.Min, ax.ctrl.Max, ax.min, ax.max)
		}
		if ax.ctrl.Park != ax.park {
			t.Errorf("default %s park = %v, want %v", ax.name, ax.ctrl.Park, ax.park)
		}
		if ax.ctrl.Deadband != ax.band {
			t.Errorf("default %s deadband = %v, want %v", ax.name, ax.ctrl.Deadband, ax.band)
		}
	}
}

func TestExampleConfigParses(t *testing.T) {
	cfg, err := loadFile(t, examplePath(t))
	if err != nil {
		t.Fatalf("example config should parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("example config should validate: %v", err)
	}
	if len(cfg.Slots) != 2 {
		t.Fatalf("example config has %d slots, want 2", len(cfg.Slots))
	}
	var az, el *SlotConfig
	for i := range cfg.Slots {
		switch cfg.Slots[i].Axis {
		case "az":
			az = &cfg.Slots[i]
		case "el":
			el = &cfg.Slots[i]
		}
	}
	if az == nil || el == nil {
		t.Fatalf("example config must carry one az and one el slot, got az=%v el=%v", az != nil, el != nil)
	}
	if az.Slot != "az-rotator" || el.Slot != "el-rotator" {
		t.Errorf("slot names = %q/%q, want az-rotator/el-rotator", az.Slot, el.Slot)
	}
	if az.DeviceModel == "" || el.DeviceModel == "" {
		t.Errorf("device models must be set, got %q/%q", az.DeviceModel, el.DeviceModel)
	}
	if az.DeviceLink != "serial" || el.DeviceLink != "serial" {
		t.Errorf("links = %q/%q, want serial/serial", az.DeviceLink, el.DeviceLink)
	}
	if az.Serial.Baud != 1200 {
		t.Errorf("az baud = %d, want 1200 (Rot1Prog)", az.Serial.Baud)
	}
	if el.Serial.Baud != 9600 {
		t.Errorf("el baud = %d, want 9600 (ERC-M GS-232B)", el.Serial.Baud)
	}
	// The example ships empty serial ports so the whole stack runs in mock
	// mode bench- and CI-side without hardware (KTD7).
	if !az.Mock() || !el.Mock() {
		t.Errorf("example config ports = %q/%q, want empty (mock mode)", az.Serial.Port, el.Serial.Port)
	}
	if cfg.MQTT.Station != "uhf" || cfg.MQTT.Site != "muehle" {
		t.Errorf("slot addressing = %q/%q, want muehle/uhf", cfg.MQTT.Site, cfg.MQTT.Station)
	}
}

func TestExplicitMissingConfigIsFatal(t *testing.T) {
	// -config passed explicitly (loadFile parses "-config <path>") pointing at
	// a file that does not exist: the operator asked for that file, so Load
	// must fail instead of silently running defaults (config-and-secrets §2).
	_, err := loadFile(t, "/nonexistent/spid-ercm-rotator-bridge.toml")
	if err == nil {
		t.Fatal("an explicitly-passed -config path that is missing must be an error — silence would hide the operator's typo")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing explicit config must wrap fs.ErrNotExist, got: %v", err)
	}
}

func TestAbsentDefaultConfigUsesDefaults(t *testing.T) {
	// No FlagSet behind the flags (hand-built, like no -config passed) and the
	// default path simply absent: run on the built-in defaults — the go-run
	// bench mode with mock ports must keep working without a seeded /etc file.
	flags := &Flags{ConfigPath: "/nonexistent/spid-ercm-rotator-bridge.toml"}
	cfg, err := Load(flags)
	if err != nil {
		t.Fatalf("absent default config must not error: %v", err)
	}
	if cfg.Control.AZ.Deadband != 4.0 {
		t.Errorf("az deadband = %v, want default 4.0", cfg.Control.AZ.Deadband)
	}
	if cfg.MQTT.Broker != "tcp://192.168.1.178:1883" || cfg.Host != "shari" {
		t.Errorf("defaults not applied: broker %q, host %q", cfg.MQTT.Broker, cfg.Host)
	}
}

func TestLoadMalformedFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(path, []byte("not = valid = toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFile(t, path); err == nil {
		t.Fatal("malformed file should error")
	}
}

func TestLoadMinimalAppliesDefaults(t *testing.T) {
	// Only the mandatory shape: one az + one el slot with device models and
	// empty ports. Everything else must fall back to the built-in defaults.
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
[[slot]]
axis         = "az"
device_model = "SPID Rotor (Rot1Prog)"

[[slot]]
axis         = "el"
device_model = "ERC-M / GS-500"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFile(t, path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("minimal config should validate: %v", err)
	}
	if cfg.Slots[0].Slot != "az-rotator" || cfg.Slots[1].Slot != "el-rotator" {
		t.Errorf("default slot names = %q/%q, want az-rotator/el-rotator", cfg.Slots[0].Slot, cfg.Slots[1].Slot)
	}
	if cfg.Slots[0].DeviceLink != "serial" || cfg.Slots[1].DeviceLink != "serial" {
		t.Errorf("default links = %q/%q, want serial", cfg.Slots[0].DeviceLink, cfg.Slots[1].DeviceLink)
	}
	if cfg.Slots[0].Serial.Baud != 1200 {
		t.Errorf("az default baud = %d, want 1200", cfg.Slots[0].Serial.Baud)
	}
	if cfg.Slots[1].Serial.Baud != 9600 {
		t.Errorf("el default baud = %d, want 9600", cfg.Slots[1].Serial.Baud)
	}
	if cfg.Control.AZ.Max != 360 || cfg.Control.EL.Max != 90 {
		t.Errorf("default travel max = %v/%v, want 360/90", cfg.Control.AZ.Max, cfg.Control.EL.Max)
	}
	if cfg.Control.PollIntervalDur.String() != "1s" {
		t.Errorf("default poll interval = %s, want 1s", cfg.Control.PollIntervalDur)
	}
	if cfg.Control.ReopenCooldownDur.String() != "2s" {
		t.Errorf("default reopen cooldown = %s, want 2s", cfg.Control.ReopenCooldownDur)
	}
}

func TestEmptyPortSelectsMockMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
[[slot]]
axis         = "az"
device_model = "SPID Rotor (Rot1Prog)"
[slot.serial]
port = ""

[[slot]]
axis         = "el"
device_model = "ERC-M / GS-500"
[slot.serial]
port = "/dev/serial/by-id/usb-FTDI_FT232R_USB_UART_A50285BI-if00-port0"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFile(t, path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("mock az + real el should validate: %v", err)
	}
	if !cfg.Slots[0].Mock() {
		t.Errorf("empty port must select mock mode (KTD7)")
	}
	if cfg.Slots[1].Mock() {
		t.Errorf("configured port %q must not be mock mode", cfg.Slots[1].Serial.Port)
	}
}

func TestMissingSlotRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
[[slot]]
axis         = "az"
device_model = "SPID Rotor (Rot1Prog)"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFile(t, path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a config without the el slot must be rejected")
	}

	content2 := `
[[slot]]
axis         = "el"
device_model = "ERC-M / GS-500"
`
	path2 := filepath.Join(t.TempDir(), "config2.toml")
	if err := os.WriteFile(path2, []byte(content2), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2, err := loadFile(t, path2)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg2.Validate(); err == nil {
		t.Fatal("a config without the az slot must be rejected")
	}
}

func TestUnknownOrDuplicateAxisRejected(t *testing.T) {
	base := func(axis string) string {
		return `
[[slot]]
axis         = "` + axis + `"
device_model = "X"
`
	}
	for _, axis := range []string{"az2", "AZ", "foo", ""} {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(base(axis)), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadFile(t, path)
		if err != nil {
			t.Fatalf("load (%q): %v", axis, err)
		}
		if err := cfg.Validate(); err == nil {
			t.Errorf("axis %q must be rejected", axis)
		}
	}

	dup := `
[[slot]]
axis         = "az"
device_model = "X"

[[slot]]
axis         = "az"
device_model = "Y"
`
	path := filepath.Join(t.TempDir(), "dup.toml")
	if err := os.WriteFile(path, []byte(dup), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFile(t, path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Error("duplicate az axis must be rejected")
	}
}

func TestPortMustBeByIdPath(t *testing.T) {
	content := `
[[slot]]
axis         = "az"
device_model = "SPID Rotor (Rot1Prog)"
[slot.serial]
port = "/dev/ttyUSB0"

[[slot]]
axis         = "el"
device_model = "ERC-M / GS-500"
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFile(t, path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a raw /dev/ttyUSB* port must be rejected — self-heal reopens the stable by-id path (KTD7)")
	}
}

func TestControlValidation(t *testing.T) {
	// Inverted travel limits.
	cfg := Defaults()
	cfg.Control.AZ.Min = 360
	cfg.Control.AZ.Max = 0
	if err := cfg.Validate(); err == nil {
		t.Error("inverted az travel limits must be rejected")
	}

	// Park position outside travel limits.
	cfg = Defaults()
	cfg.Control.EL.Park = 120
	if err := cfg.Validate(); err == nil {
		t.Error("el park outside travel limits must be rejected")
	}

	// Negative deadband.
	cfg = Defaults()
	cfg.Control.AZ.Deadband = -0.5
	if err := cfg.Validate(); err == nil {
		t.Error("negative deadband must be rejected")
	}
}

func TestBadDurationsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
[control]
poll_interval = "not-a-duration"

[[slot]]
axis         = "az"
device_model = "SPID Rotor (Rot1Prog)"

[[slot]]
axis         = "el"
device_model = "ERC-M / GS-500"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFile(t, path); err == nil {
		t.Fatal("a non-parseable poll_interval must be rejected at load")
	}
}

// --- MQTT password: env only, never TOML, never flags -------------------------

func TestPasswordInTOMLRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
[mqtt]
password = "s3cret-in-toml"

[[slot]]
axis         = "az"
device_model = "SPID Rotor (Rot1Prog)"

[[slot]]
axis         = "el"
device_model = "ERC-M / GS-500"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadFile(t, path)
	if err == nil {
		t.Fatal("a password key in the TOML must be rejected — the secret lives only in the EnvironmentFile")
	}
	if !strings.Contains(err.Error(), "SPID_ERCM_ROTATOR_BRIDGE_MQTT_PASSWORD") {
		t.Errorf("rejection should point at the env var, got: %v", err)
	}
}

func TestExampleHasNoPasswordKey(t *testing.T) {
	data, err := os.ReadFile(examplePath(t))
	if err != nil {
		t.Fatal(err)
	}
	// A TOML key named password anywhere (mqtt.password included) is a bug;
	// comments mentioning the env var name are fine.
	re := regexp.MustCompile(`(?m)^\s*password\s*=`)
	if re.Match(data) {
		t.Errorf("config.example.toml must not contain a `password =` key:\n%s", re.Find(data))
	}
}

func TestPasswordFromEnvOnly(t *testing.T) {
	t.Setenv("SPID_ERCM_ROTATOR_BRIDGE_MQTT_PASSWORD", "s3cret")
	t.Setenv("SPID_ERCM_ROTATOR_BRIDGE_MQTT_BROKER", "tcp://10.9.8.7:1883")
	t.Setenv("SPID_ERCM_ROTATOR_BRIDGE_MQTT_SITE", "other")
	t.Setenv("SPID_ERCM_ROTATOR_BRIDGE_MQTT_STATION", "test")

	cfg, err := loadFile(t, examplePath(t))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MQTT.Password != "s3cret" {
		t.Errorf("password = %q, want s3cret (from SPID_ERCM_ROTATOR_BRIDGE_MQTT_PASSWORD)", cfg.MQTT.Password)
	}
	if cfg.MQTT.Broker != "tcp://10.9.8.7:1883" {
		t.Errorf("broker = %q, want env override", cfg.MQTT.Broker)
	}
	if cfg.MQTT.Site != "other" || cfg.MQTT.Station != "test" {
		t.Errorf("site/station = %q/%q, want env overrides other/test", cfg.MQTT.Site, cfg.MQTT.Station)
	}
}

func TestNoPasswordFlag(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterFlags(fs)
	fs.VisitAll(func(f *flag.Flag) {
		if strings.Contains(f.Name, "password") {
			t.Errorf("flag -%s must not exist — the password never rides a command line", f.Name)
		}
	})
}

func TestFlagLogLevelOverrides(t *testing.T) {
	// Hand-built Flags with an absent path (no -config flag): the level flag
	// alone must override the config-derived level.
	flags := &Flags{
		ConfigPath: filepath.Join(t.TempDir(), "absent.toml"),
		LogLevel:   "debug",
	}
	cfg, err := Load(flags)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log level = %q, want debug (flag)", cfg.Log.Level)
	}
}
