package award

import "testing"

func TestDXCCPerBandSemantics(t *testing.T) {
	d := Builtin()
	club := map[int]map[string]bool{29: {"40m": true}}
	l := &Log{ClubLoaded: true, Club: club, Loaded: true, Entries: []Entry{
		{DXCC: 230, Band: "20m"},                  // worked, not confirmed
		{DXCC: 291, Band: "20m", Confirmed: true}, // confirmed in the log
	}}
	ix := d.Build(l)
	in := func(adif int, band string) Input { return Input{Call: "X", Band: band, ADIF: adif, Entity: "P"} }
	for _, tc := range []struct {
		in     Input
		status Status
		needed bool
		fresh  bool
	}{
		{in(29, "40m"), Confirmed, false, false},        // Club Log
		{in(29, "20m"), Unworked, true, false},          // confirmed elsewhere: one new slot
		{in(291, "20m"), Confirmed, false, false},       // the log
		{in(230, "20m"), WorkedUnconfirmed, true, true}, // worked is not confirmed; never confirmed: two slots
		{in(281, "20m"), Unworked, true, true},
	} {
		h, ok := ix.Evaluate(tc.in)
		if !ok || h.Status != tc.status || h.Needed != tc.needed || h.Fresh != tc.fresh || h.Band != tc.in.Band || h.Key != "P" {
			t.Errorf("%+v: %+v ok=%v", tc.in, h, ok)
		}
	}
	// no entity (cty missing): unknown
	if h, _ := ix.Evaluate(in(0, "20m")); h.Status != Unknown || h.Needed {
		t.Fatalf("%+v", h)
	}
	// nothing loaded yet: unknown, not "needed" and not "done"
	if h, _ := d.Build(&Log{}).Evaluate(in(29, "20m")); h.Status != Unknown || h.Needed {
		t.Fatalf("%+v", h)
	}
	// either source is enough to value a slot
	if h, _ := d.Build(&Log{Loaded: true}).Evaluate(in(29, "20m")); h.Status != Unworked || !h.Needed {
		t.Fatalf("%+v", h)
	}
	if h, _ := d.Build(&Log{ClubLoaded: true}).Evaluate(in(29, "20m")); h.Status != Unworked || !h.Needed {
		t.Fatalf("%+v", h)
	}
}

func TestDXCCModeSliceIgnoresClubLog(t *testing.T) {
	// Club Log's matrix has no mode: it cannot fill a CW slot
	d := Definition{Code: "DXCC-CW", Kind: KindDXCC, Rule: RuleConfirmed, Modes: []string{"cw"}, PerMode: true}
	d.normalize()
	l := &Log{Loaded: true, ClubLoaded: true, Club: map[int]map[string]bool{29: {"20m": true}}, Entries: []Entry{
		{DXCC: 230, Band: "20m", ModeClass: "cw", Confirmed: true},
		{DXCC: 281, Band: "20m", ModeClass: "phone", Confirmed: true},
		{DXCC: 206, Band: "20m", Confirmed: true}, // a legacy row without a mode
	}}
	ix := d.Build(l)
	for adif, want := range map[int]Status{29: Unworked, 230: Confirmed, 281: Unworked, 206: Unworked} {
		h, ok := ix.Evaluate(Input{Band: "20m", Mode: "cw", ADIF: adif})
		if !ok || h.Status != want || h.Mode != "cw" {
			t.Errorf("%d: %+v", adif, h)
		}
	}
	// SSB is outside the award; an unknown mode cannot be told
	if _, ok := ix.Evaluate(Input{Band: "20m", Mode: "usb", ADIF: 29}); ok {
		t.Fatal("phone is not part of a CW award")
	}
	if h, ok := ix.Evaluate(Input{Band: "20m", ADIF: 29}); !ok || h.Status != Unknown || h.Needed {
		t.Fatalf("%+v", h)
	}
}

func fieldDef() Definition {
	d := Definition{Code: "WAS", Kind: KindField, Field: "STATE", Rule: RuleConfirmed, DXCC: []int{6, 291, 110},
		Items: []string{"NY", "CA", "AK"}}
	d.normalize()
	return d
}

func TestFieldAwardKeysAndUnknown(t *testing.T) {
	d := fieldDef()
	l := &Log{Loaded: true, Entries: []Entry{
		{Call: "K9AAA", DXCC: 291, State: "NY", Band: "20m", ModeClass: "cw", Confirmed: true},
		{Call: "K9BBB", DXCC: 291, State: "CA", Band: "20m", ModeClass: "cw"},
		{Call: "VE3AA", DXCC: 1, State: "ON", Band: "20m", ModeClass: "cw", Confirmed: true}, // not an item, not an allowed entity
		{Call: "K9CCC", DXCC: 0, State: "AK", Band: "20m", ModeClass: "cw", Confirmed: true}, // log knows the state, entity not resolved
	}}
	ix := d.Build(l)
	ev := func(adif int, state, call string) (Hit, bool) {
		return ix.Evaluate(Input{Call: call, Band: "20m", Mode: "usb", ADIF: adif, State: state})
	}
	if h, _ := ev(291, "ny", "K1A"); h.Status != Confirmed || h.Needed || h.Key != "NY" || h.Band != "20m" {
		t.Fatalf("%+v", h)
	}
	if h, _ := ev(291, "CA", "K1A"); h.Status != WorkedUnconfirmed || !h.Needed {
		t.Fatalf("%+v", h)
	}
	if h, _ := ev(291, "AK", "K1A"); h.Status != Confirmed {
		t.Fatalf("the entry of unknown entity counts: %+v", h)
	}
	// the state of a call comes from the log when the callbook has none
	if h, _ := ev(291, "", "K9AAA"); h.Key != "NY" || h.Status != Confirmed {
		t.Fatalf("%+v", h)
	}
	// unknown stays unknown: no state, nothing loaded, no entity
	if h, ok := ev(291, "", "K1NEW"); !ok || h.Status != Unknown || h.Needed || h.Key != "" {
		t.Fatalf("%+v", h)
	}
	if h, _ := ev(0, "NY", "K1A"); h.Status != Unknown {
		t.Fatalf("%+v", h)
	}
	if h, _ := d.Build(&Log{}).Evaluate(Input{Band: "20m", ADIF: 291, State: "NY"}); h.Status != Unknown {
		t.Fatalf("%+v", h)
	}
	// not part of the award: other entity, state outside the items
	if _, ok := ev(1, "ON", "VE3X"); ok {
		t.Fatal("Canada is not an allowed entity")
	}
	if _, ok := ev(291, "TX", "K1A"); ok {
		t.Fatal("TX is not an item")
	}
}

func TestWorkedRuleAndSlices(t *testing.T) {
	d := fieldDef()
	d.Rule, d.Bands, d.PerMode = RuleWorked, []string{"20m", "40m"}, true
	l := &Log{Loaded: true, Entries: []Entry{
		{DXCC: 291, State: "NY", Band: "20m", ModeClass: "cw"},
		{DXCC: 291, State: "CA", Band: "15m", ModeClass: "cw"}, // band outside the award
	}}
	ix := d.Build(l)
	if h, _ := ix.Evaluate(Input{Band: "20m", Mode: "cw", ADIF: 291, State: "NY"}); h.Needed || h.Mode != "cw" {
		t.Fatalf("worked is enough: %+v", h)
	}
	if h, _ := ix.Evaluate(Input{Band: "20m", Mode: "usb", ADIF: 291, State: "NY"}); !h.Needed || h.Mode != "phone" {
		t.Fatalf("a slot per mode class: %+v", h)
	}
	if h, _ := ix.Evaluate(Input{Band: "40m", Mode: "cw", ADIF: 291, State: "NY"}); !h.Needed {
		t.Fatalf("a slot per band: %+v", h)
	}
	if _, ok := ix.Evaluate(Input{Band: "15m", Mode: "cw", ADIF: 291, State: "CA"}); ok {
		t.Fatal("15m is outside the award")
	}
}
