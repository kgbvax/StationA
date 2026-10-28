package mqtt

import (
	"encoding/json"
	"testing"
	"time"

	"antennaselect/internal/reconcile"
)

// --- station hold ([idle].hold_slots) tests ---------------------------------
//
// bmrec's HF lab records from a panadapter for hours without a VFO change or a
// transmit. Its hold (muehle/hf/recorder) must keep the walk-away grounding off
// while it is live, and must stop doing so when bmrec pauses, stops, dies or
// hangs.

func holdStatusMsg(online bool) fakeMessage {
	v := "offline"
	if online {
		v = "online"
	}
	return fakeMessage{topic: "muehle/hf/recorder/status", payload: []byte(v)}
}

func holdStateMsg(held bool, until time.Time) fakeMessage {
	p := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339), "station_hold": held,
		"holder": "bmrec", "reason": "hf lab recording 40m"}
	if !until.IsZero() {
		p["hold_until"] = until.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(p)
	return fakeMessage{topic: "muehle/hf/recorder/state", payload: b}
}

// heldClient is an idle client on 20m (port3 selected) with hold slot
// "recorder", its idle clock already 2 h stale.
func heldClient(t *testing.T) (c *Client, lastSelect func() string, drain func(), cancel func()) {
	t.Helper()
	c, antSwitchCmds, drain, cancel := idleClient(t, time.Hour)
	c.cfg.Idle.HoldSlots = []string{"recorder"}
	c.onRadioStatus(nil, radioStatusMsg(true))
	c.onRadioState(nil, radioStateMsg(true, "20m"))
	drain()
	lastSelect = func() string {
		cmds := antSwitchCmds()
		if len(cmds) == 0 {
			return ""
		}
		var p struct {
			Select string `json:"select"`
		}
		if err := json.Unmarshal(cmds[len(cmds)-1].payload, &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return p.Select
	}
	if got := lastSelect(); got != "port3" {
		t.Fatalf("setup: select = %q, want port3", got)
	}
	return c, lastSelect, drain, cancel
}

// staleIdle ages the idle clock past the timeout and runs one idle tick.
func staleIdle(c *Client, drain func()) {
	done := make(chan struct{})
	enqueueAndWait(c, func() { c.lastActivity = time.Now().Add(-2 * time.Hour) }, done)
	c.checkIdle()
	drain()
}

func enqueueAndWait(c *Client, f func(), done chan struct{}) {
	c.jobs <- func() { c.mu.Lock(); f(); c.mu.Unlock(); close(done) }
	<-done
}

func TestStationHoldKeepsAntennaUngrounded(t *testing.T) {
	c, lastSelect, drain, cancel := heldClient(t)
	defer cancel()
	c.onHoldStatus("recorder")(nil, holdStatusMsg(true))
	c.onHoldState("recorder")(nil, holdStateMsg(true, time.Now().Add(time.Minute)))
	drain()
	staleIdle(c, drain)
	if got := lastSelect(); got != "port3" {
		t.Errorf("select = %q while held, want port3 (no grounding)", got)
	}
	if c.in.StationActivity != "active" {
		t.Errorf("StationActivity = %q while held, want active", c.in.StationActivity)
	}
	if time.Since(c.lastActivity) > time.Minute {
		t.Error("a live hold did not reset the idle clock")
	}
}

func TestStationHoldReleasedGroundsAfterTimeout(t *testing.T) {
	for name, msgs := range map[string][]fakeMessage{
		"paused":            {holdStatusMsg(true), holdStateMsg(false, time.Time{})},
		"expired":           {holdStatusMsg(true), holdStateMsg(true, time.Now().Add(-time.Second))},
		"publisher offline": {holdStateMsg(true, time.Now().Add(time.Minute)), holdStatusMsg(false)},
		"no hold_until":     {holdStatusMsg(true), holdStateMsg(true, time.Time{})},
		"malformed":         {holdStatusMsg(true), {topic: "muehle/hf/recorder/state", payload: []byte("{")}},
		"cleared retained":  {holdStatusMsg(true), {topic: "muehle/hf/recorder/state"}},
	} {
		t.Run(name, func(t *testing.T) {
			c, lastSelect, drain, cancel := heldClient(t)
			defer cancel()
			// A live hold first, so every case also proves the release.
			c.onHoldStatus("recorder")(nil, holdStatusMsg(true))
			c.onHoldState("recorder")(nil, holdStateMsg(true, time.Now().Add(time.Minute)))
			drain()
			for _, m := range msgs {
				if m.topic == "muehle/hf/recorder/status" {
					c.onHoldStatus("recorder")(nil, m)
				} else {
					c.onHoldState("recorder")(nil, m)
				}
			}
			drain()
			staleIdle(c, drain)
			if got := lastSelect(); got != reconcile.PortOff {
				t.Errorf("select = %q, want off (hold not live)", got)
			}
		})
	}
}

// A hold whose publisher never reported /status online is not live: the last
// will is what releases a dead publisher, so it must have been seen.
func TestStationHoldNeedsStatusOnline(t *testing.T) {
	c, lastSelect, drain, cancel := heldClient(t)
	defer cancel()
	c.onHoldState("recorder")(nil, holdStateMsg(true, time.Now().Add(time.Minute)))
	drain()
	staleIdle(c, drain)
	if got := lastSelect(); got != reconcile.PortOff {
		t.Errorf("select = %q, want off (no /status online seen)", got)
	}
}

// A hold_until far in the future is capped at maxHoldTTL from receipt: a
// publisher that stops refreshing cannot keep the antenna up for hours.
func TestStationHoldCappedFromReceipt(t *testing.T) {
	c, lastSelect, drain, cancel := heldClient(t)
	defer cancel()
	c.onHoldStatus("recorder")(nil, holdStatusMsg(true))
	c.onHoldState("recorder")(nil, holdStateMsg(true, time.Now().Add(24*time.Hour)))
	drain()
	done := make(chan struct{})
	enqueueAndWait(c, func() { c.holds["recorder"].received = time.Now().Add(-maxHoldTTL - time.Second) }, done)
	staleIdle(c, drain)
	if got := lastSelect(); got != reconcile.PortOff {
		t.Errorf("select = %q, want off (hold older than maxHoldTTL)", got)
	}
}

// The idle timeout runs from when the hold ends, not from when it began.
func TestStationHoldEndStartsTheTimeout(t *testing.T) {
	c, lastSelect, drain, cancel := heldClient(t)
	defer cancel()
	c.onHoldStatus("recorder")(nil, holdStatusMsg(true))
	c.onHoldState("recorder")(nil, holdStateMsg(true, time.Now().Add(time.Minute)))
	drain()
	staleIdle(c, drain) // held through a stale clock: refreshed to now
	c.onHoldState("recorder")(nil, holdStateMsg(false, time.Time{}))
	drain()
	c.checkIdle()
	drain()
	if got := lastSelect(); got != "port3" {
		t.Errorf("select = %q right after the release, want port3 (timeout not yet run)", got)
	}
	staleIdle(c, drain)
	if got := lastSelect(); got != reconcile.PortOff {
		t.Errorf("select = %q a timeout after the release, want off", got)
	}
}

// A hold arriving on a grounded station is presence: it re-selects and
// publishes activity=active with a fresh last_activity (powerseq's wake).
func TestStationHoldOnGroundedStationReselects(t *testing.T) {
	c, lastSelect, drain, cancel := heldClient(t)
	defer cancel()
	staleIdle(c, drain)
	if got := lastSelect(); got != reconcile.PortOff {
		t.Fatalf("setup: select = %q, want off", got)
	}
	c.onHoldStatus("recorder")(nil, holdStatusMsg(true))
	c.onHoldState("recorder")(nil, holdStateMsg(true, time.Now().Add(time.Minute)))
	drain()
	if got := lastSelect(); got != "port3" {
		t.Errorf("select = %q after the hold, want port3", got)
	}
	var st statePayload
	for _, m := range lastIdleRecord() {
		if m.topic == "muehle/hf/antenna-select/state" {
			_ = json.Unmarshal(m.payload, &st)
		}
	}
	if st.Activity != "active" || st.LastActivity == "" {
		t.Errorf("antenna-select/state = %+v, want activity active with last_activity", st)
	}
}

// The hold never outranks the antenna-mode lock: a storm grounds anyway.
func TestStationHoldDoesNotOverrideLockGround(t *testing.T) {
	c, lastSelect, drain, cancel := heldClient(t)
	defer cancel()
	c.onHoldStatus("recorder")(nil, holdStatusMsg(true))
	c.onHoldState("recorder")(nil, holdStateMsg(true, time.Now().Add(time.Minute)))
	drain()
	c.jobs <- func() {
		c.update(func(in *reconcile.Inputs) { in.Locked, in.LockGround = true, true })
	}
	drain()
	if got := lastSelect(); got != reconcile.PortOff {
		t.Errorf("select = %q under a grounding lock while held, want off", got)
	}
}

func TestParseHold(t *testing.T) {
	until := time.Date(2026, 10, 28, 12, 1, 0, 0, time.UTC)
	p, u, err := parseHold([]byte(`{"station_hold":true,"hold_until":"2026-10-28T12:01:00Z","holder":"bmrec"}`))
	if err != nil || !p.StationHold || !u.Equal(until) || p.Holder != "bmrec" {
		t.Errorf("parseHold = %+v %v %v", p, u, err)
	}
	if p, _, err := parseHold([]byte(`{"station_hold":false}`)); err != nil || p.StationHold {
		t.Errorf("released: %+v %v", p, err)
	}
	if _, _, err := parseHold([]byte(`{"station_hold":true,"hold_until":"soon"}`)); err == nil {
		t.Error("a hold with an unparsable hold_until was accepted")
	}
}
