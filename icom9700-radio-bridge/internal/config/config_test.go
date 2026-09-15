package config

import (
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// loadFile parses "-config <path>" explicitly (like an operator invocation).
func loadFile(t *testing.T, path string) (Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags := RegisterFlags(fs)
	_ = fs.Parse([]string{"-config", path})
	return Load(flags)
}

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.RadioHost != "9700.kgbvax.net" {
		t.Errorf("default radio_host = %q, want 9700.kgbvax.net", cfg.RadioHost)
	}
	// KTD-7: the broker default is the bwbroker DNS indirection, never a
	// hardcoded broker LAN address.
	if cfg.MQTT.Broker != "tcp://bwbroker:1883" {
		t.Errorf("default broker = %q, want tcp://bwbroker:1883", cfg.MQTT.Broker)
	}
	if cfg.MQTT.Site != "muehle" || cfg.MQTT.Station != "uhf" || cfg.MQTT.Slot != "radio" {
		t.Errorf("default station-model address = %q/%q/%q, want muehle/uhf/radio",
			cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot)
	}
	if cfg.MQTT.DiscoveryPrefix != "homeassistant" {
		t.Errorf("default discovery_prefix = %q, want homeassistant", cfg.MQTT.DiscoveryPrefix)
	}
	if cfg.Session.IdleTimeoutDur != 120*time.Second {
		t.Errorf("default idle_timeout = %s, want 120s", cfg.Session.IdleTimeoutDur)
	}
	if cfg.Session.TXWatchdogDur != 180*time.Second {
		t.Errorf("default tx_watchdog = %s, want 180s", cfg.Session.TXWatchdogDur)
	}
	if cfg.Session.MaxAttempts != 3 {
		t.Errorf("default max_attempts = %d, want 3", cfg.Session.MaxAttempts)
	}
	if cfg.Session.AttemptSpacingDur != 30*time.Second {
		t.Errorf("default attempt_spacing = %s, want 30s", cfg.Session.AttemptSpacingDur)
	}
	if cfg.Radio.PollIntervalDur != time.Second {
		t.Errorf("default poll_interval = %s, want 1s", cfg.Radio.PollIntervalDur)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("default log level = %q, want info", cfg.Log.Level)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestExampleConfigParses(t *testing.T) {
	// Deterministic empty secrets regardless of the invoking shell's exports
	// (applyEnv treats "" as unset).
	t.Setenv(EnvPrefix+"_MQTT_PASSWORD", "")
	t.Setenv(EnvPrefix+"_CIV_PASSWORD", "")

	cfg, err := loadFile(t, examplePath(t))
	if err != nil {
		t.Fatalf("example config should parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("example config should validate: %v", err)
	}
	if cfg.RadioHost != "9700.kgbvax.net" {
		t.Errorf("radio_host = %q, want 9700.kgbvax.net", cfg.RadioHost)
	}
	if cfg.MQTT.Broker != "tcp://bwbroker:1883" {
		t.Errorf("broker = %q, want tcp://bwbroker:1883", cfg.MQTT.Broker)
	}
	if cfg.MQTT.Site != "muehle" || cfg.MQTT.Station != "uhf" || cfg.MQTT.Slot != "radio" {
		t.Errorf("station-model address = %q/%q/%q, want muehle/uhf/radio",
			cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot)
	}
	if cfg.MQTT.User != "hf" {
		t.Errorf("mqtt user = %q, want hf", cfg.MQTT.User)
	}
	if cfg.Session.IdleTimeoutDur != 120*time.Second ||
		cfg.Session.TXWatchdogDur != 180*time.Second ||
		cfg.Session.MaxAttempts != 3 ||
		cfg.Session.AttemptSpacingDur != 30*time.Second {
		t.Errorf("session policy = %s/%s/%d/%s, want 120s/180s/3/30s",
			cfg.Session.IdleTimeoutDur, cfg.Session.TXWatchdogDur,
			cfg.Session.MaxAttempts, cfg.Session.AttemptSpacingDur)
	}
	if cfg.Radio.PollIntervalDur != time.Second {
		t.Errorf("poll_interval = %s, want 1s", cfg.Radio.PollIntervalDur)
	}
	if cfg.MQTT.Password != "" || cfg.CIV.Password != "" {
		t.Errorf("passwords must never come from the TOML, got mqtt=%q civ=%q",
			cfg.MQTT.Password, cfg.CIV.Password)
	}
}

func TestExplicitMissingConfigIsFatal(t *testing.T) {
	// -config passed explicitly (loadFile parses "-config <path>") pointing at
	// a file that does not exist: the operator asked for that file, so Load
	// must fail instead of silently running defaults (config-and-secrets §2).
	_, err := loadFile(t, "/nonexistent/icom9700-radio-bridge.toml")
	if err == nil {
		t.Fatal("an explicitly-passed -config path that is missing must be an error — silence would hide the operator's typo")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing explicit config must wrap fs.ErrNotExist, got: %v", err)
	}
}

func TestAbsentDefaultConfigUsesDefaults(t *testing.T) {
	// No FlagSet behind the flags (hand-built, like no -config passed) and the
	// default path simply absent: run on the built-in defaults — bench runs
	// must keep working without a seeded /etc file.
	flags := &Flags{ConfigPath: "/nonexistent/icom9700-radio-bridge.toml"}
	cfg, err := Load(flags)
	if err != nil {
		t.Fatalf("absent default config must not error: %v", err)
	}
	if cfg.MQTT.Broker != "tcp://bwbroker:1883" {
		t.Errorf("broker = %q, want default tcp://bwbroker:1883", cfg.MQTT.Broker)
	}
	if cfg.MQTT.Site != "muehle" || cfg.MQTT.Station != "uhf" || cfg.MQTT.Slot != "radio" {
		t.Errorf("station-model address = %q/%q/%q, want muehle/uhf/radio",
			cfg.MQTT.Site, cfg.MQTT.Station, cfg.MQTT.Slot)
	}
	if cfg.RadioHost != "9700.kgbvax.net" {
		t.Errorf("radio_host = %q, want default 9700.kgbvax.net", cfg.RadioHost)
	}
	if cfg.Session.IdleTimeoutDur != 120*time.Second {
		t.Errorf("idle_timeout = %s, want default 120s", cfg.Session.IdleTimeoutDur)
	}
}

func TestEnvOverridesReachConfig(t *testing.T) {
	tests := []struct {
		name string
		env  string
		set  string
		want func(Config) string
	}{
		{
			name: "civ password",
			env:  EnvPrefix + "_CIV_PASSWORD",
			set:  "civ-secret",
			want: func(c Config) string { return c.CIV.Password },
		},
		{
			name: "mqtt password",
			env:  EnvPrefix + "_MQTT_PASSWORD",
			set:  "mqtt-secret",
			want: func(c Config) string { return c.MQTT.Password },
		},
		{
			name: "civ username",
			env:  EnvPrefix + "_CIV_USERNAME",
			set:  "operator",
			want: func(c Config) string { return c.CIV.Username },
		},
		{
			name: "radio host",
			env:  EnvPrefix + "_RADIO_HOST",
			set:  "192.168.1.40",
			want: func(c Config) string { return c.RadioHost },
		},
		{
			name: "mqtt broker",
			env:  EnvPrefix + "_MQTT_BROKER",
			set:  "tcp://192.168.1.139:1883",
			want: func(c Config) string { return c.MQTT.Broker },
		},
		{
			name: "site",
			env:  EnvPrefix + "_SITE",
			set:  "other",
			want: func(c Config) string { return c.MQTT.Site },
		},
		{
			name: "slot",
			env:  EnvPrefix + "_SLOT",
			set:  "radio2",
			want: func(c Config) string { return c.MQTT.Slot },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.env, tt.set)
			cfg, err := loadFile(t, examplePath(t))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := tt.want(cfg); got != tt.set {
				t.Errorf("env %s did not reach config: got %q, want %q", tt.env, got, tt.set)
			}
		})
	}
}

func TestPasswordKeyInTOMLRejected(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "mqtt password key",
			content: "[mqtt]\npassword = \"secret\"\n",
		},
		{
			name:    "civ password key",
			content: "[civ]\nusername = \"op\"\nciv_password = \"secret\"\n",
		},
		{
			name:    "bare top-level password key",
			content: "password = \"secret\"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadFile(t, path)
			if err == nil {
				t.Fatal("a password key in the TOML must be a hard error — the secrets are env-only")
			}
			if !strings.Contains(err.Error(), "environment") {
				t.Errorf("error should point at the env vars, got: %v", err)
			}
		})
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

func TestInvalidDurationRejected(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"idle_timeout", "session.idle_timeout", "fast"},
		{"tx_watchdog", "session.tx_watchdog", "180"},
		{"attempt_spacing", "session.attempt_spacing", "soon"},
		{"poll_interval", "radio.poll_interval", "1 h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			content := "[" + strings.SplitN(tt.key, ".", 2)[0] + "]\n" +
				strings.SplitN(tt.key, ".", 2)[1] + " = \"" + tt.value + "\"\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadFile(t, path)
			if err == nil {
				t.Fatalf("invalid duration %q for %s should error", tt.value, tt.key)
			}
			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error should name the key %s, got: %v", tt.key, err)
			}
		})
	}
}

func TestLogLevelFlagOverride(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	flags := RegisterFlags(fs)
	_ = fs.Parse([]string{"-config", examplePath(t), "-log.level", "debug"})
	cfg, err := Load(flags)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log level = %q, want debug (flag overrides config)", cfg.Log.Level)
	}
}

func TestValidateRejectsEmptyAddressing(t *testing.T) {
	cfg := Defaults()
	cfg.MQTT.Site = ""
	if err := cfg.Validate(); err == nil {
		t.Error("empty site must be rejected — station-model addressing is mandatory")
	}
	cfg = Defaults()
	cfg.MQTT.Broker = ""
	if err := cfg.Validate(); err == nil {
		t.Error("empty broker must be rejected")
	}
	cfg = Defaults()
	cfg.RadioHost = ""
	if err := cfg.Validate(); err == nil {
		t.Error("empty radio_host must be rejected — the protocol has no discovery")
	}
}

// The RS-BA1 substitution table's domain is printable ASCII; a byte outside
// 32..126 must be rejected at load, not crash passcode() at first dial
// (review fix).
func TestValidateRejectsNonASCIICredentials(t *testing.T) {
	cfg := Defaults()
	cfg.CIV.Username = "operator1"
	cfg.CIV.Password = "s3crét" // é = 0xC3 0xA9, outside 32..126
	if err := cfg.Validate(); err == nil {
		t.Error("non-ASCII civ password must be rejected — it would panic the substitution table")
	}
	cfg = Defaults()
	cfg.CIV.Username = "operátor"
	cfg.CIV.Password = "s3cret"
	if err := cfg.Validate(); err == nil {
		t.Error("non-ASCII civ username must be rejected")
	}
}
