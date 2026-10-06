package bridge

import (
	"context"
	"reflect"
	"testing"
	"time"

	"flexbridge/internal/flexradio"
)

func TestPlanLimit(t *testing.T) {
	none := optInt{}
	obs := func(rf, tune, max optInt) txObs { return txObs{rf: rf, tune: tune, max: max} }
	tests := []struct {
		name  string
		known bool
		limit optInt
		obs   txObs
		mech  string
		want  []txCmd
	}{
		{"unknown ceiling plans nothing", false, some(40), obs(some(100), some(100), some(100)), MechanismBoth, nil},
		{"clamp lowers rfpower over the ceiling", true, some(40), obs(some(100), some(10), some(100)), MechanismClamp, []txCmd{{knobRF, 40}}},
		{"clamp lowers tunepower too", true, some(40), obs(some(30), some(50), none), MechanismClamp, []txCmd{{knobTune, 40}}},
		{"clamp leaves values at or under the ceiling", true, some(40), obs(some(40), some(10), none), MechanismClamp, nil},
		{"clamp never sends to an unknown value", true, some(40), obs(none, none, none), MechanismClamp, nil},
		{"clamp ignores max_power_level", true, some(40), obs(some(30), some(10), some(100)), MechanismClamp, nil},
		{"lifted clamp does nothing", true, none, obs(some(100), some(100), some(40)), MechanismClamp, nil},
		{"native sets the ceiling", true, some(40), obs(some(100), some(10), some(100)), MechanismNative, []txCmd{{knobMax, 40}}},
		{"native sets the ceiling even when max unknown", true, some(40), obs(none, none, none), MechanismNative, []txCmd{{knobMax, 40}}},
		{"native in line plans nothing", true, some(40), obs(some(100), some(10), some(40)), MechanismNative, nil},
		{"native lift restores 100", true, none, obs(some(40), some(10), some(40)), MechanismNative, []txCmd{{knobMax, 100}}},
		{"native lift with max unknown waits", true, none, obs(none, none, none), MechanismNative, nil},
		{"native lift at 100 plans nothing", true, none, obs(none, none, some(100)), MechanismNative, nil},
		{"both = native + clamp", true, some(40), obs(some(100), some(10), some(100)), MechanismBoth, []txCmd{{knobMax, 40}, {knobRF, 40}}},
		{"unrecognized mechanism falls back to clamp", true, some(40), obs(some(100), some(10), some(100)), "bogus", []txCmd{{knobRF, 40}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := planLimit(tc.known, tc.limit, tc.obs, tc.mech)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("planLimit = %v, want %v", got, tc.want)
			}
		})
	}
}

func newLimitBridge(t *testing.T, mech string) (*Bridge, *MemoPublisher, *fakeCommander) {
	t.Helper()
	b, pub := newTestBridge(t)
	b.cfg.PowerLimitMechanism = mech
	fc := &fakeCommander{}
	b.SetCommander(fc)
	return b, pub, fc
}

func feedTransmit(t *testing.T, b *Bridge, line string) {
	t.Helper()
	f, err := flexradio.ParseFrame(line)
	if err != nil {
		t.Fatal(err)
	}
	b.HandleStatus(f)
}

func TestSetRFPowerLimit_Values(t *testing.T) {
	tests := []struct {
		value string
		want  *int // nil = no ceiling on /state
		known bool
	}{
		{"40", ptrTo(40), true},
		{" 40 ", ptrTo(40), true},
		{"100", ptrTo(100), true},
		{"1", ptrTo(1), true},
		{"off", nil, true},
		{"OFF", nil, true},
		{"0", nil, false},
		{"101", nil, false},
		{"abc", nil, false},
		{"", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			b, pub := newTestBridge(t)
			pub.Reset()
			b.HandleCommand(cmdJSON("set_rf_power_limit", tc.value))
			b.mu.RLock()
			known, lim := b.limitKnown, b.state.rfPowerLimit
			b.mu.RUnlock()
			if known != tc.known {
				t.Fatalf("limitKnown = %v, want %v", known, tc.known)
			}
			if !tc.known {
				if len(pub.Messages()) != 0 {
					t.Errorf("rejected value published %d messages", len(pub.Messages()))
				}
				return
			}
			if !reflect.DeepEqual(lim.ptr(), tc.want) {
				t.Errorf("ceiling = %v, want %v", lim.ptr(), tc.want)
			}
			snap, ok := lastState(pub.Messages(), testStateTopic)
			if !ok {
				t.Fatal("no /state published")
			}
			if !reflect.DeepEqual(snap.RFLimitW, tc.want) {
				t.Errorf("/state.rf_power_limit_w = %v, want %v", snap.RFLimitW, tc.want)
			}
		})
	}
}

func ptrTo(v int) *int { return &v }

func TestSetRFPowerLimit_AcceptedWithRadioOffline(t *testing.T) {
	b, pub := newTestBridge(t) // no commander installed
	b.HandleCommand(cmdJSON("set_rf_power_limit", "40"))
	snap, _ := lastState(pub.Messages(), testStateTopic)
	if snap.RFLimitW == nil || *snap.RFLimitW != 40 {
		t.Fatalf("/state.rf_power_limit_w = %v, want 40 with the radio offline", snap.RFLimitW)
	}
	b.enforceLimit(time.Now()) // no commander: must not panic, nothing to send
}

func TestResetKeepsCeiling(t *testing.T) {
	b, pub, _ := newLimitBridge(t, MechanismClamp)
	feedTransmit(t, b, "S0|transmit rfpower=30 tunepower=10")
	b.HandleCommand(cmdJSON("set_rf_power_limit", "40"))
	pub.Reset()
	b.Reset()
	snap, _ := lastState(pub.Messages(), testStateTopic)
	if snap.DeviceOnline {
		t.Error("Reset snapshot must be device_online=false")
	}
	if snap.RFLimitW == nil || *snap.RFLimitW != 40 {
		t.Errorf("Reset dropped the ceiling: rf_power_limit_w = %v", snap.RFLimitW)
	}
	if snap.RFPowerW != nil {
		t.Errorf("Reset must clear radio-observed rf_power_w, got %v", *snap.RFPowerW)
	}
}

func TestTransmitStatusPublished(t *testing.T) {
	b, pub := newTestBridge(t)
	pub.Reset()
	feedTransmit(t, b, "S0|transmit tune=0 max_power_level=100")
	feedTransmit(t, b, "S0|transmit freq=14.213000 rfpower=37 tunepower=10 tx_slice_mode=USB")
	snap, _ := lastState(pub.Messages(), testStateTopic)
	if snap.RFPowerW == nil || *snap.RFPowerW != 37 || snap.TunePowerW == nil || *snap.TunePowerW != 10 ||
		snap.MaxPowerW == nil || *snap.MaxPowerW != 100 {
		t.Errorf("/state power = rf %v tune %v max %v, want 37/10/100", snap.RFPowerW, snap.TunePowerW, snap.MaxPowerW)
	}
	if snap.RFLimitW != nil {
		t.Errorf("no ceiling set, but rf_power_limit_w = %v", *snap.RFLimitW)
	}
	n := len(pub.Messages())
	feedTransmit(t, b, "S0|transmit rfpower=37") // unchanged: no publish
	if len(pub.Messages()) != n {
		t.Error("unchanged transmit frame republished /state")
	}
	feedTransmit(t, b, "S0|transmit band 3 rfpower=100") // topic args: not live state
	b.mu.RLock()
	rf := b.state.rfPower
	b.mu.RUnlock()
	if rf != some(37) {
		t.Errorf("transmit frame with topic args changed rfpower to %v", rf)
	}
}

func TestEnforce_ClampsOverCeiling(t *testing.T) {
	b, _, fc := newLimitBridge(t, MechanismClamp)
	b.HandleCommand(cmdJSON("set_rf_power_limit", "40"))
	feedTransmit(t, b, "S0|transmit rfpower=100 tunepower=10")
	b.enforceLimit(time.Now())
	if got := fc.txCmds(); !reflect.DeepEqual(got, []txCmd{{knobRF, 40}}) {
		t.Fatalf("sent %v, want [rfpower=40]", got)
	}
	// Radio confirms; operator drags below the ceiling: nothing more to send.
	feedTransmit(t, b, "S0|transmit rfpower=30")
	b.enforceLimit(time.Now().Add(2 * time.Second))
	if got := fc.txCmds(); len(got) != 1 {
		t.Errorf("sent %v after compliance, want no new command", got)
	}
}

func TestEnforce_RateLimitsResends(t *testing.T) {
	b, _, fc := newLimitBridge(t, MechanismClamp)
	b.HandleCommand(cmdJSON("set_rf_power_limit", "40"))
	feedTransmit(t, b, "S0|transmit rfpower=100")
	t0 := time.Now()
	b.enforceLimit(t0)
	b.enforceLimit(t0.Add(200 * time.Millisecond)) // radio has not confirmed yet
	if got := fc.txCmds(); len(got) != 1 {
		t.Fatalf("sent %d commands within the resend window, want 1", len(got))
	}
	b.enforceLimit(t0.Add(powerLimitResend + time.Millisecond))
	if got := fc.txCmds(); len(got) != 2 {
		t.Errorf("sent %d commands after the resend window, want 2", len(got))
	}
}

func TestEnforce_NoCeilingNoCommands(t *testing.T) {
	b, _, fc := newLimitBridge(t, MechanismBoth)
	feedTransmit(t, b, "S0|transmit rfpower=100 tunepower=100 max_power_level=40")
	b.enforceLimit(time.Now())
	if got := fc.txCmds(); len(got) != 0 {
		t.Errorf("ceiling unknown (no set_rf_power_limit yet) but sent %v; a persisted native ceiling must stay", got)
	}
}

func TestEnforce_NativeOwnsMaxPowerLevel(t *testing.T) {
	b, _, fc := newLimitBridge(t, MechanismNative)
	feedTransmit(t, b, "S0|transmit rfpower=100 max_power_level=100")
	b.HandleCommand(cmdJSON("set_rf_power_limit", "40"))
	t0 := time.Now()
	b.enforceLimit(t0)
	feedTransmit(t, b, "S0|transmit max_power_level=40")
	// Operator raises max_power_level in the GUI: re-asserted.
	feedTransmit(t, b, "S0|transmit max_power_level=80")
	b.enforceLimit(t0.Add(2 * time.Second))
	// Lift: back to 100, rfpower is left alone.
	b.HandleCommand(cmdJSON("set_rf_power_limit", "off"))
	b.enforceLimit(t0.Add(4 * time.Second))
	want := []txCmd{{knobMax, 40}, {knobMax, 40}, {knobMax, 100}}
	if got := fc.txCmds(); !reflect.DeepEqual(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
}

func TestRunPowerLimit_KickedByReconnect(t *testing.T) {
	b, _ := newTestBridge(t)
	b.HandleCommand(cmdJSON("set_rf_power_limit", "40"))
	// The transmit status arrives during the handshake, before the commander.
	feedTransmit(t, b, "S0|transmit rfpower=100")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.RunPowerLimit(ctx)

	fc := &fakeCommander{}
	b.SetCommander(fc)
	deadline := time.Now().Add(2 * time.Second)
	for len(fc.txCmds()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := fc.txCmds(); !reflect.DeepEqual(got, []txCmd{{knobRF, 40}}) {
		t.Errorf("after reconnect sent %v, want [rfpower=40]", got)
	}
}
