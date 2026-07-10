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
