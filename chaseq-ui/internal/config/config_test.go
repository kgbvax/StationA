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

func TestTomlPasswordAndNone(t *testing.T) {
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
	c, _ := Load(write(t, "[mqtt]\npassword = \"s3cret-pw\"\n"))
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

func TestSendersLegacyFallback(t *testing.T) {
	c, _ := Load(write(t, "[loggers]\nlog4om_addr = \"10.0.0.5:2241\"\nlog4om_timeout = \"300ms\"\n"))
	got := c.Loggers.Senders()
	if len(got) != 1 || got[0].Kind != "log4om" || got[0].Addr != "10.0.0.5:2241" || got[0].Timeout != "300ms" || !got[0].On() {
		t.Errorf("Senders = %+v", got)
	}
	if c, _ = Load(""); len(c.Loggers.Senders()) != 1 {
		t.Errorf("defaults: Senders = %+v", c.Loggers.Senders())
	}
}

func TestSendersListWinsOverLegacy(t *testing.T) {
	c, _ := Load(write(t, `[loggers]
log4om_addr = "10.0.0.5:2241"
[[loggers.sender]]
kind = "log4om"
addr = "127.0.0.1:2241"
timeout = "1s"
[[loggers.sender]]
kind = "log4om"
addr = "127.0.0.1:2251"
enabled = false
`))
	got := c.Loggers.Senders()
	if len(got) != 2 || got[0].Addr != "127.0.0.1:2241" || !got[0].On() || got[1].On() {
		t.Errorf("Senders = %+v", got)
	}
}

func TestLegacyBadTimeoutFallsBackToDefault(t *testing.T) {
	l := LoggersConfig{Log4OMAddr: "127.0.0.1:2241", Log4OMTimeout: "500"}
	if s := l.Senders(); len(s) != 1 || s[0].Timeout != "" {
		t.Fatalf("%+v", s)
	}
}
