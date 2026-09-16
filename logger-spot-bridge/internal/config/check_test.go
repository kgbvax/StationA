package config

import (
	"strings"
	"testing"
)

// okConfig is the deployed shape: hassio broker, hf user, DXLog listener.
func okConfig() Config {
	cfg := Defaults()
	cfg.MQTT.Broker = "tcp://hassio.kgbvax.net:1883"
	cfg.MQTT.User = "hf"
	cfg.StationLocator = "JO31"
	return cfg
}

func TestCheckPasswordViaEnv(t *testing.T) {
	t.Setenv("LOGGER_SPOT_BRIDGE_MQTT_PASSWORD", "secret")
	lines, err := Check(okConfig(), false)
	if err != nil {
		t.Fatalf("expected OK, got %v", err)
	}
	if !hasLine(lines, "password set (env") {
		t.Fatalf("env password not reported: %v", lines)
	}
	// The summary is printed to the operator — the secret itself must never
	// appear in it.
	for _, l := range lines {
		if strings.Contains(l, "secret") {
			t.Fatalf("summary leaks the password: %q", l)
		}
	}
}

func TestCheckPasswordViaToml(t *testing.T) {
	t.Setenv("LOGGER_SPOT_BRIDGE_MQTT_PASSWORD", "")
	cfg := okConfig()
	cfg.MQTT.Password = "from-toml"
	lines, err := Check(cfg, false)
	if err != nil {
		t.Fatalf("expected OK, got %v", err)
	}
	if !hasLine(lines, "password set ([mqtt]") {
		t.Fatalf("toml password not reported: %v", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "from-toml") {
			t.Fatalf("summary leaks the password: %q", l)
		}
	}
}

func TestCheckMissingPassword(t *testing.T) {
	t.Setenv("LOGGER_SPOT_BRIDGE_MQTT_PASSWORD", "")
	_, err := Check(okConfig(), false)
	if err == nil {
		t.Fatal("expected failure without any password")
	}
	if !strings.Contains(err.Error(), "no MQTT password") {
		t.Fatalf("wrong problem reported: %v", err)
	}
}

func TestCheckUnparseableBroker(t *testing.T) {
	t.Setenv("LOGGER_SPOT_BRIDGE_MQTT_PASSWORD", "x")
	cfg := okConfig()
	cfg.MQTT.Broker = "::::"
	if _, err := Check(cfg, false); err == nil {
		t.Fatal("expected failure for an unparseable broker URL")
	}
}

func TestCheckBrokerWithoutHost(t *testing.T) {
	t.Setenv("LOGGER_SPOT_BRIDGE_MQTT_PASSWORD", "x")
	cfg := okConfig()
	cfg.MQTT.Broker = "tcp://:1883"
	if _, err := Check(cfg, false); err == nil {
		t.Fatal("expected failure for a broker without a host")
	}
}

// resolve=true exercises the DNS path against the resolver; a name that
// cannot exist must surface as a problem, not a silent pass.
func TestCheckResolveFailure(t *testing.T) {
	t.Setenv("LOGGER_SPOT_BRIDGE_MQTT_PASSWORD", "x")
	cfg := okConfig()
	// .invalid is reserved by RFC 2606 and never resolves.
	cfg.MQTT.Broker = "tcp://bwbroker.invalid:1883"
	if _, err := Check(cfg, true); err == nil {
		t.Fatal("expected DNS-resolution failure for a nonexistent broker host")
	}
}

func hasLine(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func TestCheckPlaceholderPasswordRejected(t *testing.T) {
	// The seeded-but-unedited wrapper: env present, so a naive check would
	// pass — but env beats TOML in applyEnv, and this value is garbage.
	t.Setenv("LOGGER_SPOT_BRIDGE_MQTT_PASSWORD", mqttPasswordPlaceholder)
	cfg := okConfig()
	cfg.MQTT.Password = "real-one-in-toml"
	_, err := Check(cfg, false)
	if err == nil {
		t.Fatal("expected failure while the wrapper placeholder is in the environment")
	}
	if !strings.Contains(err.Error(), "PASTE_MQTT_PASSWORD_HERE") {
		t.Fatalf("placeholder not named as the problem: %v", err)
	}
}
