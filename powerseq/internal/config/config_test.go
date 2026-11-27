package config

import (
	"os"
	"path/filepath"
	"strings"
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
	if len(cfg.Startup) != 12 || len(cfg.Shutdown) != 9 {
		t.Errorf("example sequence = %d/%d steps, want 12/9", len(cfg.Startup), len(cfg.Shutdown))
	}
	if cfg.Idle.ShutdownMinutes != 120 || len(cfg.IdleShutdown) != 10 {
		t.Errorf("example idle shutdown = %d min / %d steps, want 120 / 10", cfg.Idle.ShutdownMinutes, len(cfg.IdleShutdown))
	}
	// Cold-start order: PC mains → PSU → 5 s → PC WOL → (PLC online) → TRX → 1 s → PA → antenna power.
	var order []string
	for _, st := range cfg.Startup {
		order = append(order, st.Name)
	}
	want := "pc-mains-on psu-on psu-settle pc-boot wait-plc-online trx-on trx-settle pa-on antennas-on wait-radio-online wait-pa-power-on pa-arm-enable"
	if got := strings.Join(order, " "); got != want {
		t.Errorf("startup order:\n got  %s\n want %s", got, want)
	}
	if len(cfg.Standby) != 2 || len(cfg.Wake) != 3 {
		t.Errorf("standby/wake = %d/%d steps, want 2/3", len(cfg.Standby), len(cfg.Wake))
	}
	for _, st := range cfg.Standby {
		if st.Slot == "power/shack-pc" && (st.Retain == nil || *st.Retain) {
			t.Errorf("pc-sleep must be retain=false (a retained sleep replays on wake)")
		}
	}
	// The shack PC plug is switched on at cold start and never off by the
	// sequencer: cutting mains hard-powers-off Windows.
	for name, steps := range map[string][]Step{"shutdown": cfg.Shutdown, "standby": cfg.Standby, "wake": cfg.Wake} {
		for _, st := range steps {
			if st.Slot == "power/shack-pc-mains" && st.Value == "off" {
				t.Errorf("%s step %q switches the shack PC mains off", name, st.Name)
			}
		}
	}
	if cfg.Idle.ActivitySlot != "hf/antenna-select" || len(cfg.Idle.Watches) != 3 {
		t.Errorf("idle = %+v", cfg.Idle)
	}
	// The site master mains is out of scope: nothing in the shipped sequence may
	// switch it (it also powers the Pis and the network).
	for _, steps := range [][]Step{cfg.Startup, cfg.Shutdown, cfg.Standby, cfg.Wake} {
		for _, st := range steps {
			if st.Slot == "power/master" {
				t.Errorf("step %q drives power/master", st.Name)
			}
		}
	}
	if cfg.Startup[0].Name != "pc-mains-on" {
		t.Errorf("startup[0] = %+v, want pc-mains-on", cfg.Startup[0])
	}
	if cfg.Shutdown[0].Name != "pa-arm-disable" {
		t.Errorf("shutdown[0] = %+v, want pa-arm-disable", cfg.Shutdown[0])
	}
}

func validWithStandby() Config {
	cfg := Defaults()
	cfg.Startup = exampleStartup()
	cfg.Shutdown = exampleShutdown()
	f := false
	cfg.Standby = []Step{
		{Name: "antennas-off", Kind: KindCmd, Slot: "power/antennas", Action: "set_power", Value: "off"},
		{Name: "pc-sleep", Kind: KindCmd, Slot: "power/shack-pc", Action: "sleep", Value: "now", Retain: &f},
	}
	cfg.Wake = []Step{
		{Name: "pc-wake", Kind: KindWOL, MAC: "aa:bb:cc:dd:ee:ff", Broadcast: "192.168.1.255:9", Poke: "192.168.1.197:3389"},
		{Name: "antennas-on", Kind: KindCmd, Slot: "power/antennas", Action: "set_power", Value: "on"},
	}
	cfg.Idle = IdleConfig{ActivitySlot: "hf/antenna-select", TimeoutMinutes: 30,
		Watches: []Watch{{Slot: "uhf/radio", Field: "freq_hz"}}}
	return cfg
}

func TestStandbyConfigValidates(t *testing.T) {
	if err := validWithStandby().Validate(); err != nil {
		t.Fatalf("valid standby config rejected: %v", err)
	}
}

func TestStandbyConfigRejects(t *testing.T) {
	cases := map[string]func(*Config){
		"standby without wake":   func(c *Config) { c.Wake = nil },
		"wake without standby":   func(c *Config) { c.Standby = nil },
		"no activity slot":       func(c *Config) { c.Idle.ActivitySlot = "" },
		"zero timeout":           func(c *Config) { c.Idle.TimeoutMinutes = 0 },
		"watch without field":    func(c *Config) { c.Idle.Watches[0].Field = "" },
		"negative deadband":      func(c *Config) { c.Idle.Watches[0].MinDelta = -1 },
		"bad mac":                func(c *Config) { c.Wake[0].MAC = "nope" },
		"broadcast without port": func(c *Config) { c.Wake[0].Broadcast = "192.168.1.255" },
		"poke without port":      func(c *Config) { c.Wake[0].Poke = "192.168.1.197" },
	}
	for name, mut := range cases {
		cfg := validWithStandby()
		mut(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestNoStandbyIsValid(t *testing.T) {
	cfg := Defaults()
	cfg.Startup = exampleStartup()
	cfg.Shutdown = exampleShutdown()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config without standby rejected: %v", err)
	}
}

func validWithIdleShutdown() Config {
	cfg := validWithStandby()
	cfg.Idle.ShutdownMinutes = 120
	cfg.IdleShutdown = exampleShutdown()
	return cfg
}

func TestIdleShutdownConfigValidates(t *testing.T) {
	if err := validWithIdleShutdown().Validate(); err != nil {
		t.Fatalf("valid idle-shutdown config rejected: %v", err)
	}
}

func TestIdleShutdownConfigRejects(t *testing.T) {
	cases := map[string]func(*Config){
		"minutes without steps": func(c *Config) { c.IdleShutdown = nil },
		"steps without minutes": func(c *Config) { c.Idle.ShutdownMinutes = 0 },
		"negative minutes":      func(c *Config) { c.Idle.ShutdownMinutes = -1 },
		"not above timeout":     func(c *Config) { c.Idle.ShutdownMinutes = c.Idle.TimeoutMinutes },
		"without standby":       func(c *Config) { c.Standby, c.Wake = nil, nil },
		"bad step":              func(c *Config) { c.IdleShutdown[0].Kind = "nope" },
	}
	for name, mut := range cases {
		cfg := validWithIdleShutdown()
		mut(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestAntModeSlotDefaultAndValidation(t *testing.T) {
	cfg := validConfig()
	if cfg.MQTT.AntModeSlot != "ant-mode" {
		t.Errorf("ant_mode_slot default = %q, want ant-mode", cfg.MQTT.AntModeSlot)
	}
	cfg.MQTT.AntModeSlot = "" // disabled
	if err := cfg.Validate(); err != nil {
		t.Errorf("empty ant_mode_slot rejected: %v", err)
	}
	for _, bad := range []string{"hf/ant-mode", "ant-mode/#", "+"} {
		cfg.MQTT.AntModeSlot = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("ant_mode_slot %q accepted", bad)
		}
	}
}

func TestStateFileDefaultOverrideAndValidation(t *testing.T) {
	cfg := validConfig()
	if cfg.StateFile != DefaultStateFile || DefaultStateFile != "/var/lib/powerseq/state.json" {
		t.Errorf("state_file default = %q", cfg.StateFile)
	}
	cfg.StateFile = "" // persistence off
	if err := cfg.Validate(); err != nil {
		t.Errorf("empty state_file rejected: %v", err)
	}
	cfg.StateFile = "state.json"
	if err := cfg.Validate(); err == nil {
		t.Error("relative state_file accepted")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("state_file = \"/run/powerseq/s.json\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(&Flags{ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if got.StateFile != "/run/powerseq/s.json" {
		t.Errorf("state_file from TOML = %q", got.StateFile)
	}
	if err := os.WriteFile(path, []byte("state_file = \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err = Load(&Flags{ConfigPath: path}); err != nil || got.StateFile != "" {
		t.Errorf("explicit empty state_file = %q (err %v), want disabled", got.StateFile, err)
	}
}

func validWithAutoStart() Config {
	cfg := validConfig()
	f := false
	cfg.AutoStart = []Step{
		{Name: "bwpc", Kind: KindWOL, MAC: "aa:bb:cc:dd:ee:ff", Poke: "192.168.1.197:3389"},
		{Name: "bwpc", Kind: KindWaitStatus, Slots: []string{"power/shack-pc"}},
		{Name: "dax", Kind: KindCmd, Slot: "power/shack-pc", Action: "ensure_program", Value: "dax", Retain: &f},
		{Name: "dax", Kind: KindWaitState, Slot: "power/shack-pc", Field: "program_dax", Value: "ready", Fresh: true, FailValue: "failed"},
	}
	return cfg
}

// [[auto_start]] is validated like the other lists, except that consecutive
// steps may share a name (the name is the progress row of the page).
func TestAutoStartConfigRules(t *testing.T) {
	if err := validWithAutoStart().Validate(); err != nil {
		t.Fatalf("valid auto_start rejected: %v", err)
	}
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("config without auto_start rejected: %v", err)
	}
	cases := map[string]func(*Config){
		"unknown kind":     func(c *Config) { c.AutoStart[2].Kind = "launch" },
		"cmd without slot": func(c *Config) { c.AutoStart[2].Slot = "" },
		"no name":          func(c *Config) { c.AutoStart[0].Name = "" },
		"bad mac":          func(c *Config) { c.AutoStart[0].MAC = "zz" },
		"bad auto_op_slot": func(c *Config) { c.MQTT.AutoOpSlot = "hf/auto-op" },
		"bad pa_slot":      func(c *Config) { c.MQTT.PASlot = "pa/#" },
		"beat age 0":       func(c *Config) { c.MQTT.AutoOpBeatS = 0 },
	}
	for name, mut := range cases {
		cfg := validWithAutoStart()
		mut(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestAutoOpDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.MQTT.AutoOpSlot != "auto-op" || cfg.MQTT.PASlot != "pa" || cfg.MQTT.AutoOpBeatS != 30 {
		t.Errorf("defaults = %q %q %d", cfg.MQTT.AutoOpSlot, cfg.MQTT.PASlot, cfg.MQTT.AutoOpBeatS)
	}
	cfg = validConfig()
	cfg.MQTT.AutoOpSlot = "" // disabled
	if err := cfg.Validate(); err != nil {
		t.Errorf("empty auto_op_slot rejected: %v", err)
	}
}

// The shipped example chain: the row-id step names in order, actuator
// commands not retained, every program wait fresh with fail_value.
func TestExampleAutoStart(t *testing.T) {
	cfg, err := Load(&Flags{ConfigPath: "../../config.example.toml"})
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, st := range cfg.AutoStart {
		if len(rows) == 0 || rows[len(rows)-1] != st.Name {
			rows = append(rows, st.Name)
		}
		switch st.Kind {
		case KindCmd:
			if st.Retain == nil || *st.Retain || st.Action != "ensure_program" || st.Value != st.Name {
				t.Errorf("cmd step %+v: want a not retained ensure_program of %s", st, st.Name)
			}
		case KindWaitState:
			if !st.Fresh || st.FailValue != "failed" || st.Field != "program_"+st.Name || st.Value != "ready" {
				t.Errorf("wait_state step %+v", st)
			}
		}
	}
	if got := strings.Join(rows, " "); got != "bwpc aethersdr dax wsjtx 75auto" {
		t.Errorf("auto_start rows = %s", got)
	}
	last := cfg.AutoStart[len(cfg.AutoStart)-1]
	if last.Kind != KindWaitStatus || len(last.Slots) != 1 || last.Slots[0] != "hf/auto-op" {
		t.Errorf("last auto_start step = %+v, want wait_status hf/auto-op", last)
	}
	if cfg.AutoStart[0].Kind != KindWOL || cfg.AutoStart[0].Poke == "" {
		t.Errorf("first auto_start step = %+v, want the bwpc wol with poke", cfg.AutoStart[0])
	}
}
