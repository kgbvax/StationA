package reconcile

import (
	"testing"

	"antennaselect/internal/config"
)

func testConfig() config.Config {
	return config.Config{
		Location: "bauwagen",
		Host:     "shari",
		MQTT:     config.MQTT{Site: "muehle", Station: "hf", Slot: "antenna-select"},
		WiringMap: map[string]string{
			"p1":  "dummy-load",
			"p2":  "fan-dipole",
			"p3":  "ultrabeam",
			"off": "grounded",
		},
		BandPolicy: config.BandPolicy{
			Bands: map[string][]string{
				"ultrabeam":  {"6m", "10m", "12m", "15m", "17m", "20m"},
				"fan-dipole": {"30m", "40m", "60m", "80m"},
			},
			Fallback: "fan-dipole",
		},
		BandFollow: config.BandFollow{Resource: "ultrabeam", Slot: "ant-ctrl"},
	}
}

func TestResolveAutoBandPolicy(t *testing.T) {
	r := New(testConfig())
	cases := []struct {
		band string
		want string
	}{
		{"20m", "p3"}, // ultrabeam
		{"17m", "p3"},
		{"6m", "p3"},
		{"40m", "p2"}, // fan-dipole
		{"80m", "p2"},
		{"160m", "p2"}, // unmatched -> fallback fan-dipole
		{"", "p2"},     // unknown band -> fallback
	}
	for _, tc := range cases {
		d := r.Resolve(Inputs{RadioOnline: true, RadioBand: tc.band, StationActivity: "active"})
		if d.Target != tc.want || d.Source != SourceAuto {
			t.Errorf("band %q: got target=%q source=%q, want target=%q source=auto", tc.band, d.Target, d.Source, tc.want)
		}
		if d.Mode != ModeAuto {
			t.Errorf("band %q: mode=%q, want auto", tc.band, d.Mode)
		}
	}
}

func TestResolveOperatorHold(t *testing.T) {
	r := New(testConfig())
	// An operator hold on p1 wins over the auto band policy (which would pick p3 for 20m).
	d := r.Resolve(Inputs{RadioOnline: true, RadioBand: "20m", StationActivity: "active", OperatorRequest: "p1"})
	if d.Target != "p1" || d.Source != SourceOperator || d.Mode != ModeManual {
		t.Errorf("operator hold: got %+v, want target=p1 source=operator mode=manual", d)
	}
	// "auto" releases the hold and returns to band policy.
	d = r.Resolve(Inputs{RadioOnline: true, RadioBand: "20m", StationActivity: "active", OperatorRequest: "auto"})
	if d.Target != "p3" || d.Source != SourceAuto || d.Mode != ModeAuto {
		t.Errorf("hold released: got %+v, want target=p3 source=auto mode=auto", d)
	}
}

func TestResolveIdleOverridesOperator(t *testing.T) {
	r := New(testConfig())
	// Station inactive forces off and overrides an operator hold (walk-away safety, §10).
	d := r.Resolve(Inputs{RadioOnline: true, RadioBand: "20m", StationActivity: "inactive", OperatorRequest: "p3"})
	if d.Target != PortOff || d.Source != SourceIdle {
		t.Errorf("idle override: got %+v, want target=off source=idle", d)
	}
	// Mode still reflects that a hold is active (manual), even though idle won the target.
	if d.Mode != ModeManual {
		t.Errorf("idle with hold: mode=%q, want manual", d.Mode)
	}
}

func TestResolveUnknownActivityIsActive(t *testing.T) {
	r := New(testConfig())
	d := r.Resolve(Inputs{RadioOnline: true, RadioBand: "40m", StationActivity: ""})
	if d.Target != "p2" || d.Source != SourceAuto {
		t.Errorf("unknown activity should behave as active: got %+v", d)
	}
}

func TestResolveRadioOfflineHoldsLast(t *testing.T) {
	r := New(testConfig())
	// Radio offline: auto cannot resolve, target empty -> hold last selection (§10).
	d := r.Resolve(Inputs{RadioOnline: false, RadioBand: "20m", StationActivity: "active"})
	if d.Target != "" {
		t.Errorf("radio offline: target=%q, want empty (hold last)", d.Target)
	}
	// But an operator hold still asserts even with the radio offline.
	d = r.Resolve(Inputs{RadioOnline: false, StationActivity: "active", OperatorRequest: "p1"})
	if d.Target != "p1" || d.Source != SourceOperator {
		t.Errorf("offline + hold: got %+v, want target=p1 source=operator", d)
	}
}

func TestNextEmitsSelectInRX(t *testing.T) {
	r := New(testConfig())
	act := r.Next(Inputs{RadioOnline: true, RadioBand: "20m", StationActivity: "active", RadioTX: TXReceive, SwitchSelected: "p1"})
	if act.SelectPort != "p3" {
		t.Errorf("expected select p3 in RX, got %q", act.SelectPort)
	}
	if act.DeferredForTX {
		t.Error("should not be deferred in RX")
	}
}

func TestNextDefersSelectDuringTX(t *testing.T) {
	r := New(testConfig())
	act := r.Next(Inputs{RadioOnline: true, RadioBand: "20m", StationActivity: "active", RadioTX: TXTransmit, SwitchSelected: "p1"})
	if act.SelectPort != "" {
		t.Errorf("must not emit select during TX (cold-switch), got %q", act.SelectPort)
	}
	if !act.DeferredForTX {
		t.Error("expected DeferredForTX during TX")
	}
}

func TestNextNoSelectWhenAlreadyOnTarget(t *testing.T) {
	r := New(testConfig())
	act := r.Next(Inputs{RadioOnline: true, RadioBand: "20m", StationActivity: "active", RadioTX: TXReceive, SwitchSelected: "p3"})
	if act.SelectPort != "" {
		t.Errorf("no select expected when already on target, got %q", act.SelectPort)
	}
}

func TestNextBandFollowOnlyWhenFollowedResourceSelected(t *testing.T) {
	r := New(testConfig())
	// Followed resource (ultrabeam, 20m -> p3) selected: band-follow pushes the frequency.
	act := r.Next(Inputs{RadioOnline: true, RadioBand: "20m", RadioFreqHz: 14175000, StationActivity: "active", RadioTX: TXReceive, SwitchSelected: "p3"})
	if act.FollowFreqHz != 14175000 {
		t.Errorf("expected band-follow freq 14175000, got %d", act.FollowFreqHz)
	}
	// Fan-dipole selected (40m -> p2): no band-follow.
	act = r.Next(Inputs{RadioOnline: true, RadioBand: "40m", RadioFreqHz: 7100000, StationActivity: "active", RadioTX: TXReceive, SwitchSelected: "p2"})
	if act.FollowFreqHz != 0 {
		t.Errorf("no band-follow expected when fan-dipole selected, got %d", act.FollowFreqHz)
	}
}

func TestNextBandFollowDisabledWhenNoResourceConfigured(t *testing.T) {
	cfg := testConfig()
	cfg.BandFollow = config.BandFollow{Slot: "ant-ctrl"} // no resource -> disabled
	r := New(cfg)
	act := r.Next(Inputs{RadioOnline: true, RadioBand: "20m", RadioFreqHz: 14175000, StationActivity: "active", RadioTX: TXReceive, SwitchSelected: "p3"})
	if act.FollowFreqHz != 0 {
		t.Errorf("band-follow disabled: expected no follow, got %d", act.FollowFreqHz)
	}
}

func TestNextNoActionsWhenUnresolvable(t *testing.T) {
	r := New(testConfig())
	act := r.Next(Inputs{RadioOnline: false, StationActivity: "active", RadioTX: TXReceive, SwitchSelected: "p1"})
	if act.SelectPort != "" || act.DeferredForTX || act.FollowFreqHz != 0 {
		t.Errorf("unresolvable state should yield no actions, got %+v", act)
	}
}

// paFollowCfg is testConfig with the PA band-follow binding enabled (slot pa). testConfig
// alone leaves PAFollow disabled, so existing tests don't suddenly emit SetBand.
func paFollowCfg() config.Config {
	cfg := testConfig()
	cfg.PAFollow = config.PAFollow{Enabled: true, Slot: "pa"}
	return cfg
}

func TestNextPAFollowEmitsBandWhenRadioOnline(t *testing.T) {
	r := New(paFollowCfg())
	// PA follow is NOT gated on antenna selection: it fires for any selected port (here
	// the fan-dipole on p2, not the followed ultrabeam) while the radio is online + band known.
	for _, selected := range []string{"p1", "p2", "p3", "off"} {
		act := r.Next(Inputs{RadioOnline: true, RadioBand: "20m", StationActivity: "active",
			RadioTX: TXReceive, SwitchSelected: selected})
		if act.SetBand != "20m" {
			t.Errorf("selected=%s: expected SetBand=20m, got %q", selected, act.SetBand)
		}
	}
}

func TestNextPAFollowGatesOnRadioOnline(t *testing.T) {
	r := New(paFollowCfg())
	// Radio offline: never trust radio state (§10) — no SetBand, even with a band present.
	act := r.Next(Inputs{RadioOnline: false, RadioBand: "20m", StationActivity: "active", RadioTX: TXReceive, SwitchSelected: "p3"})
	if act.SetBand != "" {
		t.Errorf("radio offline: expected no SetBand, got %q", act.SetBand)
	}
}

func TestNextPAFollowGatesOnBandKnown(t *testing.T) {
	r := New(paFollowCfg())
	// Band unknown/empty: nothing to push.
	act := r.Next(Inputs{RadioOnline: true, RadioBand: "", StationActivity: "active", RadioTX: TXReceive, SwitchSelected: "p3"})
	if act.SetBand != "" {
		t.Errorf("empty band: expected no SetBand, got %q", act.SetBand)
	}
}

func TestNextPAFollowDisabled(t *testing.T) {
	// testConfig leaves PAFollow disabled — the default. No SetBand even when online + known.
	r := New(testConfig())
	act := r.Next(Inputs{RadioOnline: true, RadioBand: "20m", StationActivity: "active", RadioTX: TXReceive, SwitchSelected: "p3"})
	if act.SetBand != "" {
		t.Errorf("PAFollow disabled: expected no SetBand, got %q", act.SetBand)
	}
}

func TestNextPAFollowNoTXGate(t *testing.T) {
	r := New(paFollowCfg())
	// PA band-follow has NO TX gate (hot-switch protection is hardware): it still emits
	// during TX, unlike the ant-switch SelectPort which defers (cold-switch, §6). The
	// switch is off p3 (the 20m target) so a select IS pending and defers; the PA band
	// emits regardless.
	act := r.Next(Inputs{RadioOnline: true, RadioBand: "20m", StationActivity: "active", RadioTX: TXTransmit, SwitchSelected: "p1"})
	if act.SetBand != "20m" {
		t.Errorf("PA follow during TX: expected SetBand=20m (no TX gate), got %q", act.SetBand)
	}
	if !act.DeferredForTX {
		t.Error("ant-switch select should still defer during TX (cold-switch)")
	}
}

// tunerFollowCfg is testConfig with the tuner in-line binding enabled (resource fan-dipole,
// atu_bands 30/60/160m, slot tuner). testConfig alone leaves TunerFollow disabled.
func tunerFollowCfg() config.Config {
	cfg := testConfig()
	cfg.TunerFollow = config.TunerFollow{
		Enabled:  true,
		Slot:     "tuner",
		Resource: "fan-dipole",
		ATUBands: []string{"30m", "60m", "160m"},
	}
	return cfg
}

func TestNextTunerFollowEngagesOnNonResonantBand(t *testing.T) {
	r := New(tunerFollowCfg())
	// Fan-dipole selected (p2) on a non-resonant ATU band -> ATU in line.
	for _, band := range []string{"30m", "60m", "160m"} {
		act := r.Next(Inputs{RadioOnline: true, RadioBand: band, StationActivity: "active",
			RadioTX: TXReceive, SwitchSelected: "p2"})
		if act.SetInline == nil || *act.SetInline != true {
			t.Errorf("band %s: expected SetInline=true, got %v", band, act.SetInline)
		}
	}
}

func TestNextTunerFollowBypassesOnResonantBand(t *testing.T) {
	r := New(tunerFollowCfg())
	// Fan-dipole selected (p2) on a resonant band (40m is wired, not in atu_bands) -> bypass.
	act := r.Next(Inputs{RadioOnline: true, RadioBand: "40m", StationActivity: "active",
		RadioTX: TXReceive, SwitchSelected: "p2"})
	if act.SetInline == nil || *act.SetInline != false {
		t.Errorf("40m: expected SetInline=false (bypass), got %v", act.SetInline)
	}
	// 80m is resonant (wired to fan-dipole, not in atu_bands) -> bypass too.
	act = r.Next(Inputs{RadioOnline: true, RadioBand: "80m", StationActivity: "active",
		RadioTX: TXReceive, SwitchSelected: "p2"})
	if act.SetInline == nil || *act.SetInline != false {
		t.Errorf("80m: expected SetInline=false (bypass), got %v", act.SetInline)
	}
}

func TestNextTunerFollowBypassesWhenOtherResourceSelected(t *testing.T) {
	r := New(tunerFollowCfg())
	// Ultrabeam (p3) selected on 20m — not the tuner's resource. ATU must drop out of line
	// (bypass), so leaving a non-resonant band doesn't leave the ATU engaged.
	act := r.Next(Inputs{RadioOnline: true, RadioBand: "20m", StationActivity: "active",
		RadioTX: TXReceive, SwitchSelected: "p3"})
	if act.SetInline == nil || *act.SetInline != false {
		t.Errorf("ultrabeam selected: expected SetInline=false (bypass), got %v", act.SetInline)
	}
	// Even a non-resonant band routes to the fallback fan-dipole when unmatched — but the
	// ultrabeam is the selected resource here, so the ATU stays bypassed.
	act = r.Next(Inputs{RadioOnline: true, RadioBand: "20m", StationActivity: "active",
		RadioTX: TXReceive, SwitchSelected: "p3"})
	if act.SetInline == nil || *act.SetInline != false {
		t.Errorf("ultrabeam selected on resonant band: expected bypass, got %v", act.SetInline)
	}
}

func TestNextTunerFollowGatesOnRadioOnline(t *testing.T) {
	r := New(tunerFollowCfg())
	// Radio offline: never trust radio state (§10) — no SetInline, even with a band present.
	act := r.Next(Inputs{RadioOnline: false, RadioBand: "30m", StationActivity: "active",
		RadioTX: TXReceive, SwitchSelected: "p2"})
	if act.SetInline != nil {
		t.Errorf("radio offline: expected no SetInline, got %v", *act.SetInline)
	}
}

func TestNextTunerFollowGatesOnBandKnown(t *testing.T) {
	r := New(tunerFollowCfg())
	// Band unknown/empty: nothing to push.
	act := r.Next(Inputs{RadioOnline: true, RadioBand: "", StationActivity: "active",
		RadioTX: TXReceive, SwitchSelected: "p2"})
	if act.SetInline != nil {
		t.Errorf("empty band: expected no SetInline, got %v", *act.SetInline)
	}
}

func TestNextTunerFollowDisabled(t *testing.T) {
	// testConfig leaves TunerFollow disabled — the default. No SetInline even when on a
	// non-resonant band with the resource selected.
	r := New(testConfig())
	act := r.Next(Inputs{RadioOnline: true, RadioBand: "30m", StationActivity: "active",
		RadioTX: TXReceive, SwitchSelected: "p2"})
	if act.SetInline != nil {
		t.Errorf("TunerFollow disabled: expected no SetInline, got %v", *act.SetInline)
	}
}

func TestNextTunerFollowDedupStable(t *testing.T) {
	r := New(tunerFollowCfg())
	// Re-resolving the same state yields the same intent value (the dedup is the mqtt
	// layer's job, but the reconciler must keep emitting the same bool, not flip-flop).
	first := r.Next(Inputs{RadioOnline: true, RadioBand: "30m", StationActivity: "active",
		RadioTX: TXReceive, SwitchSelected: "p2"})
	second := r.Next(Inputs{RadioOnline: true, RadioBand: "30m", StationActivity: "active",
		RadioTX: TXReceive, SwitchSelected: "p2"})
	if first.SetInline == nil || second.SetInline == nil {
		t.Fatal("expected non-nil SetInline on both resolves")
	}
	if *first.SetInline != *second.SetInline {
		t.Errorf("SetInline flipped across identical resolves: %v -> %v", *first.SetInline, *second.SetInline)
	}
}
