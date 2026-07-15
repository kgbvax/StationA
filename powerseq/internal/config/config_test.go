package config

import (
	"testing"
)

// exampleStartup/exampleShutdown reproduce the integration-model §7.1 sequence
// (the default shipped in config.example.toml). Used to build a valid Config for
// tests that exercise the transport/timing defaults and the validate rules.
func exampleStartup() []Step {
	return []Step{
		{Name: "master-on", Kind: KindCmd, Slot: "power/master", Action: "set_power", Value: "on"},
		{Name: "network-delay", Kind: KindDelay, Duration: "network"},
		{Name: "psu-on", Kind: KindCmd, Slot: "power/psu-13v8", Action: "set_power", Value: "on"},
		{Name: "wait-controllers-online", Kind: KindWaitStatus, Slots: []string{"hf/switch", "hf/pa-arm", "hf/ant-switch"}},
		{Name: "trx-on", Kind: KindCmd, Slot: "hf/switch", Action: "set_trx", Value: "on"},
		{Name: "wait-radio-online", Kind: KindWaitStatus, Slots: []string{"hf/radio"}},
		{Name: "pa-on", Kind: KindCmd, Slot: "hf/switch", Action: "set_pa", Value: "on"},
		{Name: "wait-pa-power-on", Kind: KindWaitState, Slot: "hf/pa", Field: "power", Value: "on"},
		{Name: "pa-arm-enable", Kind: KindCmd, Slot: "hf/pa-arm", Action: "set_enabled", Value: "true"},
	}
}

func exampleShutdown() []Step {
	return []Step{
		{Name: "pa-arm-disable", Kind: KindCmd, Slot: "hf/pa-arm", Action: "set_enabled", Value: "false"},
		{Name: "stagger-1", Kind: KindDelay, Duration: "stagger"},
		{Name: "pa-off", Kind: KindCmd, Slot: "hf/switch", Action: "set_pa", Value: "off"},
		{Name: "stagger-2", Kind: KindDelay, Duration: "stagger"},
		{Name: "trx-off", Kind: KindCmd, Slot: "hf/switch", Action: "set_trx", Value: "off"},
		{Name: "stagger-3", Kind: KindDelay, Duration: "stagger"},
		{Name: "psu-off", Kind: KindCmd, Slot: "power/psu-13v8", Action: "set_power", Value: "off"},
		{Name: "stagger-4", Kind: KindDelay, Duration: "stagger"},
		{Name: "master-off", Kind: KindCmd, Slot: "power/master", Action: "set_power", Value: "off"},
	}
}

func validConfig() Config {
	cfg := Defaults()
	cfg.Startup = exampleStartup()
	cfg.Shutdown = exampleShutdown()
	return cfg
}

func TestDefaultsTransportAndTiming(t *testing.T) {
	cfg := Defaults()
	if cfg.MQTT.Site != "muehle" || cfg.MQTT.Slot != "power-seq" {
		t.Errorf("defaults = %+v", cfg.MQTT)
	}
	if cfg.Timing.NetworkDelayS != 30 || cfg.Timing.StepTimeoutS != 120 {
		t.Errorf("timing defaults = %+v", cfg.Timing)
	}
	// The sequence is config-driven: bare defaults have NO sequence, so a bare
	// Defaults() must fail Validate (empty step lists).
	if err := cfg.Validate(); err == nil {
		t.Error("bare Defaults() should fail Validate (no sequence)")
	}
}

func TestValidateRequiresSiteAndSlot(t *testing.T) {
	checks := func(mutate func(*Config)) {
		t.Helper()
		cfg := validConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Error("Validate should reject this config")
		}
	}
	checks(func(c *Config) { c.MQTT.Site = "" })
	checks(func(c *Config) { c.MQTT.Station = "" })
	checks(func(c *Config) { c.MQTT.Broker = "" })
}

func TestValidateExampleSequence(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("example sequence invalid: %v", err)
	}
}

func TestValidateSequenceRules(t *testing.T) {
	cfg := validConfig()

	// Empty startup rejected.
	bad := cfg
	bad.Startup = nil
	if err := bad.Validate(); err == nil {
		t.Error("empty startup should fail")
	}

	// Duplicate step name rejected.
	bad = cfg
	bad.Startup = append([]Step{{Name: "master-on", Kind: KindCmd, Slot: "x", Action: "y", Value: "z"}}, cfg.Startup...)
	if err := bad.Validate(); err == nil {
		t.Error("duplicate step name should fail")
	}

	// cmd missing value rejected.
	bad = cfg
	bad.Startup = []Step{{Name: "s", Kind: KindCmd, Slot: "power/master", Action: "set_power"}}
	if err := bad.Validate(); err == nil {
		t.Error("cmd without value should fail")
	}

	// wait_status without slots rejected.
	bad = cfg
	bad.Startup = []Step{{Name: "s", Kind: KindWaitStatus}}
	if err := bad.Validate(); err == nil {
		t.Error("wait_status without slots should fail")
	}

	// wait_status bad state rejected.
	bad = cfg
	bad.Startup = []Step{{Name: "s", Kind: KindWaitStatus, Slots: []string{"hf/radio"}, State: "maybe"}}
	if err := bad.Validate(); err == nil {
		t.Error("wait_status bad state should fail")
	}

	// wait_state missing field rejected.
	bad = cfg
	bad.Startup = []Step{{Name: "s", Kind: KindWaitState, Slot: "hf/pa", Value: "on"}}
	if err := bad.Validate(); err == nil {
		t.Error("wait_state without field should fail")
	}

	// delay with neither/both duration_s and duration rejected.
	bad = cfg
	bad.Startup = []Step{{Name: "s", Kind: KindDelay}}
	if err := bad.Validate(); err == nil {
		t.Error("delay with no duration should fail")
	}
	bad = cfg
	bad.Startup = []Step{{Name: "s", Kind: KindDelay, DurationS: intPtr(5), Duration: "network"}}
	if err := bad.Validate(); err == nil {
		t.Error("delay with both duration_s and duration should fail")
	}

	// delay with unknown symbolic duration rejected (fail fast).
	bad = cfg
	bad.Startup = []Step{{Name: "s", Kind: KindDelay, Duration: "bogus"}}
	if err := bad.Validate(); err == nil {
		t.Error("delay with unknown symbolic duration should fail")
	}

	// unknown kind rejected.
	bad = cfg
	bad.Startup = []Step{{Name: "s", Kind: "wait_a_minute"}}
	if err := bad.Validate(); err == nil {
		t.Error("unknown step kind should fail")
	}
}

func intPtr(v int) *int { return &v }

// TestValidateStepEdgeCases covers the load-time validation the adversarial
// review added: non-positive timeout_s / duration_s, an empty entry in a
// wait_status slots list, and that wait_state value="" is now ALLOWED
// (waiting for a field to clear is legitimate).
func TestValidateStepEdgeCases(t *testing.T) {
	mustReject := func(name string, mutate func(*Config)) {
		t.Helper()
		cfg := validConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate should reject", name)
		}
	}
	mustAccept := func(name string, mutate func(*Config)) {
		t.Helper()
		cfg := validConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: Validate should accept: %v", name, err)
		}
	}

	// timeout_s <= 0 rejected on wait_status and wait_state.
	zero := 0
	neg := -1
	mustReject("wait_status timeout_s=0", func(c *Config) {
		c.Startup = []Step{{Name: "s", Kind: KindWaitStatus, Slots: []string{"hf/radio"}, TimeoutS: &zero}}
	})
	mustReject("wait_state timeout_s=-1", func(c *Config) {
		c.Startup = []Step{{Name: "s", Kind: KindWaitState, Slot: "hf/pa", Field: "power", Value: "on", TimeoutS: &neg}}
	})

	// duration_s <= 0 rejected on delay.
	mustReject("delay duration_s=0", func(c *Config) {
		c.Startup = []Step{{Name: "s", Kind: KindDelay, DurationS: &zero}}
	})
	mustReject("delay duration_s=-5", func(c *Config) {
		c.Startup = []Step{{Name: "s", Kind: KindDelay, DurationS: &neg}}
	})

	// An empty entry in wait_status slots is rejected (a stray "" makes the AND
	// unpassable — fail fast at load, not at a confusing timeout).
	mustReject("wait_status empty slot entry", func(c *Config) {
		c.Startup = []Step{{Name: "s", Kind: KindWaitStatus, Slots: []string{"hf/radio", ""}}}
	})

	// wait_state value="" is now ACCEPTED (wait for a field to clear).
	mustAccept("wait_state value empty", func(c *Config) {
		c.Startup = []Step{{Name: "wait-fault-clear", Kind: KindWaitState, Slot: "hf/pa", Field: "fault", Value: ""}}
	})
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("POWERSEQ_MQTT_BROKER", "tcp://10.0.0.1:1883")
	t.Setenv("POWERSEQ_MQTT_PASSWORD", "secret")
	t.Setenv("POWERSEQ_MQTT_SITE", "testsite")
	f := &Flags{ConfigPath: "/nonexistent-powerseq-config.toml"}
	cfg, err := Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MQTT.Broker != "tcp://10.0.0.1:1883" {
		t.Errorf("broker = %s", cfg.MQTT.Broker)
	}
	if cfg.MQTT.Password != "secret" {
		t.Errorf("password not overridden")
	}
	if cfg.MQTT.Site != "testsite" {
		t.Errorf("site = %s", cfg.MQTT.Site)
	}
}

func TestMissingDefaultConfigIsOK(t *testing.T) {
	f := &Flags{ConfigPath: "/nonexistent-powerseq-config.toml"}
	cfg, err := Load(f)
	if err != nil {
		t.Fatalf("missing default config should not error: %v", err)
	}
	if cfg.MQTT.Slot != "power-seq" {
		t.Errorf("expected default slot, got %s", cfg.MQTT.Slot)
	}
}

// TestExampleFileDecodesAndValidates round-trips the shipped config.example.toml
// (the model §7.1 default sequence) through Load + Validate, guarding the
// documented schema against drift.
func TestExampleFileDecodesAndValidates(t *testing.T) {
	f := &Flags{ConfigPath: "../../config.example.toml"}
	cfg, err := Load(f)
	if err != nil {
		t.Fatalf("Load example: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate example: %v", err)
	}
	if len(cfg.Startup) != 9 || len(cfg.Shutdown) != 9 {
		t.Errorf("example sequence = %d/%d steps, want 9/9 (§7.1)", len(cfg.Startup), len(cfg.Shutdown))
	}
	if cfg.Startup[0].Name != "master-on" || cfg.Startup[0].Slot != "power/master" {
		t.Errorf("startup[0] = %+v, want master-on/power/master", cfg.Startup[0])
	}
	if cfg.Shutdown[0].Name != "pa-arm-disable" {
		t.Errorf("shutdown[0] = %+v, want pa-arm-disable", cfg.Shutdown[0])
	}
}
