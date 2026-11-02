package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnvPasswordWinsOverToml(t *testing.T) {
	t.Setenv("CHASEQ_UI_MQTT_PASSWORD", "from-env")
	c, err := Load(write(t, "[mqtt]\npassword = \"from-toml\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.MQTT.Password != "from-env" {
		t.Errorf("password = %q, want from-env", c.MQTT.Password)
	}
}

func TestTomlPasswordFallbackAndNone(t *testing.T) {
	t.Setenv("CHASEQ_UI_MQTT_PASSWORD", "")
	c, _ := Load(write(t, "[mqtt]\npassword = \"from-toml\"\n"))
	if c.MQTT.Password != "from-toml" {
		t.Errorf("password = %q, want from-toml", c.MQTT.Password)
	}
	c, _ = Load("")
	if c.MQTT.Password != "" {
		t.Errorf("password = %q, want empty", c.MQTT.Password)
	}
}

func TestStringNeverShowsPassword(t *testing.T) {
	t.Setenv("CHASEQ_UI_MQTT_PASSWORD", "s3cret-pw")
	c, _ := Load("")
	if strings.Contains(c.String(), "s3cret-pw") {
		t.Errorf("String() leaks the password: %s", c.String())
	}
}

func TestTopicsAndResolve(t *testing.T) {
	p := write(t, "")
	c, _ := Load(p)
	if got := c.ChaseCmd(); got != "muehle/hf/chase/cmd" {
		t.Errorf("ChaseCmd = %q", got)
	}
	if got := c.RadioState(); got != "muehle/hf/radio/state" {
		t.Errorf("RadioState = %q", got)
	}
	if got := c.Resolve("filters.json"); got != filepath.Join(filepath.Dir(p), "filters.json") {
		t.Errorf("Resolve = %q", got)
	}
}
