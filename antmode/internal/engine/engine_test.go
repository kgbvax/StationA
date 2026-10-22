// SPDX-License-Identifier: AGPL-3.0-or-later

package engine

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"codeberg.org/kgbvax/stationa/shared/antmode"

	"antmode/internal/config"
	"antmode/internal/dwd"
	"antmode/internal/nina"
	"antmode/internal/policy"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

type msg struct {
	topic    string
	qos      byte
	retained bool
	payload  string
}

// journal records publishes and saves in order (the borrow must persist
// before it publishes).
type journal struct{ ev []string }

type fakePub struct {
	j    *journal
	msgs []msg
	fail bool
}

func (p *fakePub) Publish(topic string, qos byte, retained bool, payload []byte) error {
	if p.fail {
		return errors.New("link down")
	}
	p.msgs = append(p.msgs, msg{topic, qos, retained, string(payload)})
	p.j.ev = append(p.j.ev, "pub "+topic+" "+string(payload))
	return nil
}

type fakeSaver struct {
	j    *journal
	last *Persisted
	n    int
	fail bool
}

func (s *fakeSaver) Save(v any) error {
	if s.fail {
		return errors.New("disk full")
	}
	p := v.(Persisted)
	b, _ := json.Marshal(p) // deep copy
	var c Persisted
	_ = json.Unmarshal(b, &c)
	s.last = &c
	s.n++
	s.j.ev = append(s.j.ev, "save")
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type harness struct {
	t   *testing.T
	e   *Engine
	pub *fakePub
	sv  *fakeSaver
	clk *clock
	j   *journal
}

var t0 = time.Date(2026, 10, 22, 16, 0, 0, 0, time.UTC)

func testCfg() config.Config {
	cfg := config.Default()
	cfg.Location, cfg.Host = "bauwagen", "scmino"
	cfg.MQTT.Site, cfg.MQTT.Station = "muehle", "hf"
	cfg.Storm.FallbackAz = 222
	cfg.Maintenance.ParkAz = 135
	return cfg
}

func newH(t *testing.T, mut func(*config.Config), loaded *Persisted) *harness {
	t.Helper()
	cfg := testCfg()
	if mut != nil {
		mut(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	j := &journal{}
	h := &harness{t: t, pub: &fakePub{j: j}, sv: &fakeSaver{j: j}, clk: &clock{t: t0}, j: j}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.e = New(cfg, h.pub, h.sv, loaded, log, h.clk.now)
	return h
}

// start connects and runs out the startup hold.
func (h *harness) start() *harness {
	h.e.SetConnected(true)
	h.e.Republish()
	h.clk.advance(StartupHold)
	h.e.Tick()
	if !h.e.Ready() {
		h.t.Fatal("not ready after the startup hold")
	}
	return h
}

func (h *harness) tick(d time.Duration) {
	h.clk.advance(d)
	h.e.Tick()
}

func js(v any) []byte { b, _ := json.Marshal(v); return b }

// station feeds a healthy station: powerseq in phase, everything online,
// switch on port3, elements out, rotator at az, radio rx, antenna power on.
func (h *harness) station(phase string, az float64) {
	e := h.e
	e.PowerSeqStatus([]byte("online"))
	e.PowerSeqState(js(map[string]any{"phase": phase}))
	e.RotatorStatus([]byte("online"))
	e.RotatorState(js(map[string]any{"az": az, "moving": false, "device_online": true}))
	e.AntCtrlStatus([]byte("online"))
	e.AntCtrlState(js(map[string]any{"retracted": false, "moving": false, "device_online": true}))
	e.AntSwitchStatus([]byte("online"))
	e.AntSwitchState(js(map[string]any{"selected": "port3", "settled": true}))
	e.RadioStatus([]byte("online"))
	e.RadioState(js(map[string]any{"tx": "rx", "device_online": true}))
	e.PowerStatus([]byte("online"))
	e.PowerState(js(map[string]any{"power": "on", "device_online": true}))
}

func (h *harness) cmd(v map[string]any) { h.e.Cmd(js(v)) }

// lastState returns the last published /state.
func (h *harness) lastState() map[string]any {
	h.t.Helper()
	for i := len(h.pub.msgs) - 1; i >= 0; i-- {
		if h.pub.msgs[i].topic == "muehle/hf/ant-mode/state" {
			var m map[string]any
			if err := json.Unmarshal([]byte(h.pub.msgs[i].payload), &m); err != nil {
				h.t.Fatal(err)
			}
			return m
		}
	}
	h.t.Fatal("no /state published")
	return nil
}

// parsed reads the last /state through the shared consumer parser.
func (h *harness) parsed() antmode.State {
	h.t.Helper()
	for i := len(h.pub.msgs) - 1; i >= 0; i-- {
		if h.pub.msgs[i].topic == "muehle/hf/ant-mode/state" {
			st, err := antmode.Parse([]byte(h.pub.msgs[i].payload))
			if err != nil {
				h.t.Fatal(err)
			}
			return st
		}
	}
	h.t.Fatal("no /state published")
	return antmode.State{}
}

func (h *harness) powerCmds() []string {
	var out []string
	for _, m := range h.pub.msgs {
		if m.topic == "muehle/power/antennas/cmd" {
			if !m.retained || m.qos != 1 {
				h.t.Fatalf("power cmd not retained QoS 1: %+v", m)
			}
			var c map[string]string
			_ = json.Unmarshal([]byte(m.payload), &c)
			if c["action"] != "set_power" {
				h.t.Fatalf("power cmd action = %q", c["action"])
			}
			out = append(out, c["value"])
		}
	}
	return out
}

func (h *harness) phase() Phase { return h.e.phase }

func (h *harness) switchOff() {
	h.e.AntSwitchState(js(map[string]any{"selected": "off", "settled": true}))
}

func (h *harness) retracted() {
	h.e.AntCtrlState(js(map[string]any{"retracted": true, "moving": false, "device_online": true}))
}

func (h *harness) rotator(az float64, moving bool) {
	h.e.RotatorState(js(map[string]any{"az": az, "moving": moving, "device_online": true}))
}

// ---------------------------------------------------------------------------
// /cmd
// ---------------------------------------------------------------------------

func TestCmdOneShotClearAndTSGate(t *testing.T) {
	h := newH(t, nil, nil).start()
	h.station("running", 100)
	clears := func() int {
		n := 0
		for _, m := range h.pub.msgs {
			if m.topic == "muehle/hf/ant-mode/cmd" {
				if m.payload != "" || !m.retained {
					t.Fatalf("clear is not an empty retained publish: %+v", m)
				}
				n++
			}
		}
		return n
	}

	h.e.Cmd(nil)
	h.e.Cmd([]byte("  "))
	if clears() != 0 {
		t.Fatal("empty payload (our own clear echo) was cleared again")
	}

	cases := []struct {
		name string
		p    string
	}{
		{"malformed", `{"action":`},
		{"stale ts", `{"action":"set_ant_mode","value":"storm","ts":"2026-10-22T15:59:00Z"}`},
		{"future ts", `{"action":"set_ant_mode","value":"storm","ts":"2026-10-22T16:01:00Z"}`},
		{"bad ts", `{"action":"set_ant_mode","value":"storm","ts":"yesterday"}`},
		{"unknown mode", `{"action":"set_ant_mode","value":"hurricane"}`},
		{"number value", `{"action":"set_ant_mode","value":3}`},
		{"unknown action", `{"action":"explode"}`},
		{"repark in normal", `{"action":"repark"}`},
	}
	for i, c := range cases {
		h.e.Cmd([]byte(c.p))
		if h.e.pol.S.Mode != antmode.Normal {
			t.Fatalf("%s changed the mode", c.name)
		}
		if clears() != i+1 {
			t.Fatalf("%s: retained cmd not cleared", c.name)
		}
	}
	// A fresh ts within 30 s is applied; no ts is applied too.
	h.e.Cmd([]byte(`{"action":"set_ant_mode","value":"storm","ts":"2026-10-22T15:59:40Z"}`))
	if h.e.pol.S.Mode != antmode.Storm {
		t.Fatal("fresh cmd not applied")
	}
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "Normal"})
	if h.e.pol.S.Mode != antmode.Normal {
		t.Fatal("cmd without ts not applied")
	}
	if clears() != len(cases)+2 {
		t.Fatal("applied cmds not cleared")
	}
}

func TestCmdBeforeReadyIsQueuedAndNothingPublished(t *testing.T) {
	h := newH(t, nil, nil)
	h.e.SetConnected(true)
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "maintenance"})
	if h.e.pol.S.Mode != antmode.Normal {
		t.Fatal("cmd applied before the startup hold")
	}
	h.tick(time.Second)
	for _, m := range h.pub.msgs {
		if strings.HasSuffix(m.topic, "/state") {
			t.Fatalf("/state published during the startup hold: %+v", m)
		}
	}
	h.tick(StartupHold)
	if h.e.pol.S.Mode != antmode.Maintenance || !h.parsed().Locked() {
		t.Fatal("queued cmd not applied at ready")
	}
}

func TestSameModeIsNoOp(t *testing.T) {
	h := newH(t, nil, nil).start()
	h.station("running", 100)
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	seq := h.e.lock.Seq
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	if h.e.lock.Seq != seq || h.phase() != Grounding {
		t.Fatal("same-mode set re-parked")
	}
}

// ---------------------------------------------------------------------------
// lock sequencing
// ---------------------------------------------------------------------------

func TestStormSequence(t *testing.T) {
	h := newH(t, nil, nil).start()
	h.station("running", 170)
	h.e.WindResult(dwd.Wind{DirDeg: 265, GustKmh: 72, Day: "2026-10-22"}, nil)

	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	st := h.parsed()
	if !st.Locked() || !st.L().Ground || st.L().Retract || st.L().ParkAz != nil || st.L().Nudge {
		t.Fatalf("entry lock = %+v", st.L())
	}
	seq := st.L().Seq
	if h.phase() != Grounding {
		t.Fatalf("phase %s", h.phase())
	}

	h.switchOff()
	if l := h.parsed().L(); !l.Retract || l.ParkAz != nil || h.phase() != Retracting {
		t.Fatalf("after ground: %+v %s", l, h.phase())
	}

	// Elements still moving in → not yet.
	h.e.AntCtrlState(js(map[string]any{"retracted": true, "moving": true, "device_online": true}))
	if h.phase() != Retracting {
		t.Fatal("parked while the elements were moving")
	}
	h.retracted()
	l := h.parsed().L()
	if l.ParkAz == nil || *l.ParkAz != 175 || h.phase() != Rotating || l.Seq != seq {
		t.Fatalf("park: %+v %s", l, h.phase())
	}

	h.rotator(176, true)
	if h.phase() != Rotating {
		t.Fatal("parked while moving")
	}
	h.rotator(176, false)
	if h.phase() != Parked {
		t.Fatalf("phase %s", h.phase())
	}
	s := h.lastState()
	if s["drift_deg"] != 1.0 || s["phase"] != "parked" || s["source"] != "manual" || s["ant_mode"] != "storm" {
		t.Fatalf("parked state = %v", s)
	}
	if w := s["wind"].(map[string]any); w["dir_deg"] != 265.0 || w["gust_kmh"] != 72.0 || w["stale"] != false {
		t.Fatalf("wind = %v", w)
	}

	// Drift: reported, never re-aimed.
	n := len(h.pub.msgs)
	h.rotator(190, false)
	if h.lastState()["drift_deg"] != 15.0 || h.phase() != Parked || *h.e.lock.ParkAz != 175 {
		t.Fatal("drift not reported / re-aimed")
	}
	for _, m := range h.pub.msgs[n:] {
		if strings.Contains(m.topic, "rotator") {
			t.Fatalf("antmode commanded the rotator: %+v", m)
		}
	}

	// Unlock: lock gone, idle.
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "normal"})
	if h.parsed().Locked() || h.lastState()["lock"] != nil || h.phase() != Idle {
		t.Fatalf("unlock: %v", h.lastState())
	}
}

func TestGroundTimeoutHeldWhileTransmitting(t *testing.T) {
	h := newH(t, nil, nil).start()
	h.station("running", 100)
	h.e.RadioState(js(map[string]any{"tx": "tx", "device_online": true}))
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	h.tick(61 * time.Second)
	if h.phase() != Grounding || h.e.lock.Retract {
		t.Fatal("retracted while transmitting without a confirmed ground")
	}
	h.e.RadioState(js(map[string]any{"tx": "rx", "device_online": true}))
	if h.phase() != Retracting || !h.e.lock.Retract {
		t.Fatalf("not retracting after TX ended: %s", h.phase())
	}
}

func TestGroundTimeoutRadioOffline(t *testing.T) {
	h := newH(t, nil, nil).start()
	h.station("running", 100)
	h.e.RadioState(js(map[string]any{"tx": "tx", "device_online": true}))
	h.e.RadioStatus([]byte("offline")) // a dead radio's frozen "tx" does not hold
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	h.tick(59 * time.Second)
	if h.phase() != Grounding {
		t.Fatal("left grounding before the timeout")
	}
	h.tick(2 * time.Second)
	if h.phase() != Retracting {
		t.Fatalf("phase %s", h.phase())
	}
}

func TestRetractTimeoutParksAnyway(t *testing.T) {
	h := newH(t, nil, nil).start()
	h.station("running", 100)
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "maintenance"})
	h.switchOff()
	h.e.AntCtrlStatus([]byte("offline"))
	h.tick(119 * time.Second)
	if h.phase() != Retracting {
		t.Fatal("parked before the retract timeout")
	}
	h.tick(2 * time.Second)
	l := h.parsed().L()
	if h.phase() != Rotating || l.ParkAz == nil || *l.ParkAz != 135 || !l.Nudge {
		t.Fatalf("maintenance park: %+v %s", l, h.phase())
	}
}

func TestRotateTimeoutFailsThenLateArrival(t *testing.T) {
	h := newH(t, nil, nil).start()
	h.station("running", 10)
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "maintenance"})
	h.switchOff()
	h.retracted()
	h.tick(301 * time.Second)
	if h.phase() != Failed || h.lastState()["fault"] == nil {
		t.Fatalf("no failure: %s %v", h.phase(), h.lastState())
	}
	if !h.parsed().Locked() || h.parsed().L().ParkAz == nil {
		t.Fatal("failure dropped the lock")
	}
	h.rotator(134, false)
	if h.phase() != Parked || h.lastState()["fault"] != nil {
		t.Fatalf("late arrival not parked: %s %v", h.phase(), h.lastState())
	}
}

func TestStaleWindUsesFallback(t *testing.T) {
	h := newH(t, nil, nil).start()
	h.station("running", 100)
	h.e.WindResult(dwd.Wind{DirDeg: 0}, nil) // north → 90 would be chosen
	h.tick(181 * time.Minute)                // older than stale_minutes
	if h.lastState()["wind"].(map[string]any)["stale"] != true {
		t.Fatal("wind not stale")
	}
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	h.switchOff()
	h.retracted()
	if az := *h.e.lock.ParkAz; az != 222 {
		t.Fatalf("park_az = %d, want fallback 222", az)
	}
	// A failed wind poll keeps the old value (still stale).
	h.e.WindResult(dwd.Wind{}, errors.New("HTTP 500"))
	if h.e.wind == nil || h.e.windFresh() {
		t.Fatal("failed poll replaced the wind")
	}
}

func TestReparkRecomputesWithNewSeq(t *testing.T) {
	h := newH(t, nil, nil).start()
	h.station("running", 170)
	h.e.WindResult(dwd.Wind{DirDeg: 265}, nil)
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	h.switchOff()
	h.retracted()
	h.rotator(175, false)
	seq := h.e.lock.Seq
	h.e.WindResult(dwd.Wind{DirDeg: 300}, nil) // veer: no re-aim by itself
	if *h.e.lock.ParkAz != 175 || h.phase() != Parked {
		t.Fatal("re-aimed on a wind veer")
	}
	h.cmd(map[string]any{"action": "repark"})
	l := h.parsed().L()
	if l.Seq != seq+1 || !l.Ground || !l.Retract {
		t.Fatalf("repark lock: %+v", l)
	}
	if *h.e.lock.ParkAz != 210 || h.phase() != Rotating { // 300±90 → 30/210, nearer 175
		t.Fatalf("repark park_az %d phase %s", *h.e.lock.ParkAz, h.phase())
	}
}

func TestStormToMaintenanceSwitch(t *testing.T) {
	h := newH(t, nil, nil).start()
	h.station("running", 170)
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	h.switchOff()
	h.retracted()
	seq := h.e.lock.Seq
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "maintenance"})
	l := h.parsed().L()
	if l.Seq != seq+1 || !l.Nudge || !l.Ground || !l.Retract || *l.ParkAz != 135 {
		t.Fatalf("switch lock: %+v", l)
	}
}

// ---------------------------------------------------------------------------
// warnings, presence, policy through the engine
// ---------------------------------------------------------------------------

func warn(id string, onset, expires time.Time) nina.Warning {
	return nina.Warning{ID: id, Event: "STURMBÖEN", II: "52", Onset: onset, Expires: expires}
}

func TestAutoEnterAndExit(t *testing.T) {
	h := newH(t, func(c *config.Config) { c.Storm.AutoEnter = true }, nil).start()
	h.station("standby", 100)
	ws := []nina.Warning{warn("dwd.1", t0.Add(-time.Hour), t0.Add(time.Hour))}
	h.e.NinaResult(ws, nil)
	s := h.lastState()
	if s["ant_mode"] != "storm" || s["source"] != "auto" || s["storm_suggested"] != false {
		t.Fatalf("auto entry: %v", s)
	}
	if len(s["warnings"].([]any)) != 1 || s["warnings_ok"] != true {
		t.Fatalf("warnings: %v", s)
	}
	// Warning expires by the clock; quiet polls; a NINA outage in between.
	h.clk.t = t0.Add(time.Hour)
	h.e.NinaResult(ws, nil) // expired: not active any more
	h.tick(10 * time.Minute)
	h.e.NinaResult(nil, nil)
	h.tick(10 * time.Minute)
	h.e.NinaResult(nil, errors.New("timeout"))
	if h.lastState()["warnings_ok"] != false {
		t.Fatal("failed poll not shown")
	}
	h.tick(60 * time.Minute)
	h.e.NinaResult(nil, nil)
	if h.e.pol.S.Mode != antmode.Storm {
		t.Fatal("outage time counted towards the auto exit")
	}
	if h.lastState()["auto_exit_at"] == nil {
		t.Fatal("auto_exit_at not shown")
	}
	for i := 0; i < 10 && h.e.pol.S.Mode == antmode.Storm; i++ {
		h.tick(10 * time.Minute)
		h.e.NinaResult(nil, nil)
	}
	if h.e.pol.S.Mode != antmode.Normal || h.parsed().Locked() {
		t.Fatal("auto storm did not exit")
	}
}

func TestMannedSuggestsOnly(t *testing.T) {
	h := newH(t, func(c *config.Config) { c.Storm.AutoEnter = true }, nil).start()
	h.station("running", 100)
	ws := []nina.Warning{warn("dwd.1", t0.Add(-time.Hour), t0.Add(time.Hour))}
	h.e.NinaResult(ws, nil)
	s := h.lastState()
	if s["ant_mode"] != "normal" || s["storm_suggested"] != true || s["presence"] != "manned" {
		t.Fatalf("manned: %v", s)
	}
	h.cmd(map[string]any{"action": "dismiss_suggestion"})
	if h.lastState()["storm_suggested"] != false {
		t.Fatal("dismiss did not clear the suggestion")
	}
	// Operator leaves: powerseq goes to standby → auto storm (dismissed ≠ acknowledged).
	h.e.PowerSeqState(js(map[string]any{"phase": "entering_standby"}))
	if h.e.pol.S.Mode != antmode.Storm || h.e.pol.S.Source != "auto" {
		t.Fatal("no auto storm once unmanned")
	}
}

func TestPowerSeqOfflineIsUnmanned(t *testing.T) {
	h := newH(t, func(c *config.Config) { c.Storm.AutoEnter = true }, nil).start()
	h.station("running", 100)
	h.e.PowerSeqStatus([]byte("offline"))
	h.e.NinaResult([]nina.Warning{warn("dwd.1", t0, time.Time{})}, nil)
	if h.e.pol.S.Mode != antmode.Storm || h.lastState()["presence"] != "unmanned" {
		t.Fatal("powerseq offline not treated as unmanned")
	}
}

func TestWarningBecomesActiveByClock(t *testing.T) {
	h := newH(t, func(c *config.Config) { c.Storm.AutoEnter = true }, nil).start()
	h.station("idle", 100)
	h.e.NinaResult([]nina.Warning{warn("dwd.1", t0.Add(30*time.Minute), t0.Add(2*time.Hour))}, nil)
	if h.e.pol.S.Mode != antmode.Normal {
		t.Fatal("entered before onset (no lead time)")
	}
	h.tick(30 * time.Minute)
	if h.e.pol.S.Mode != antmode.Storm {
		t.Fatal("did not enter at onset")
	}
}

func TestManualNormalAcksThroughEngine(t *testing.T) {
	h := newH(t, func(c *config.Config) { c.Storm.AutoEnter = true }, nil).start()
	h.station("idle", 100)
	ws := []nina.Warning{warn("dwd.1", t0, t0.Add(3*time.Hour))}
	h.e.NinaResult(ws, nil)
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "normal"})
	h.tick(time.Minute)
	h.e.NinaResult(ws, nil)
	if h.e.pol.S.Mode != antmode.Normal {
		t.Fatal("acknowledged warning re-entered storm")
	}
	h.e.NinaResult(append(ws, warn("dwd.2", t0, t0.Add(3*time.Hour))), nil)
	if h.e.pol.S.Mode != antmode.Storm {
		t.Fatal("new warning id did not enter storm")
	}
}

func TestNoAutoEntryDuringStartupHold(t *testing.T) {
	h := newH(t, func(c *config.Config) { c.Storm.AutoEnter = true }, nil)
	h.e.SetConnected(true)
	h.e.NinaResult([]nina.Warning{warn("dwd.1", t0.Add(-time.Hour), t0.Add(time.Hour))}, nil)
	// powerseq replay arrives inside the hold: manned.
	h.station("running", 100)
	if h.e.pol.S.Mode != antmode.Normal {
		t.Fatal("decided before the startup hold")
	}
	h.tick(StartupHold)
	if h.e.pol.S.Mode != antmode.Normal || h.lastState()["storm_suggested"] != true {
		t.Fatalf("after hold: %v", h.lastState())
	}
}

// ---------------------------------------------------------------------------
// power borrow
// ---------------------------------------------------------------------------

func borrowCfg(c *config.Config) { c.Power.Borrow = true }

func (h *harness) powerOff() {
	h.e.PowerState(js(map[string]any{"power": "off", "device_online": true}))
	h.e.RotatorStatus([]byte("offline"))
	h.e.AntCtrlStatus([]byte("offline"))
}

func (h *harness) powerOn() {
	h.e.PowerState(js(map[string]any{"power": "on", "device_online": true}))
	h.e.RotatorStatus([]byte("online"))
	h.e.AntCtrlStatus([]byte("online"))
}

func TestBorrowStorm(t *testing.T) {
	h := newH(t, borrowCfg, nil).start()
	h.station("standby", 170)
	h.powerOff()
	h.j.ev = nil
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})

	// Order: persist (restore_to), /state with power_hold, then power on.
	saveAt, holdAt, onAt := -1, -1, -1
	for i, ev := range h.j.ev {
		switch {
		case ev == "save" && saveAt < 0:
			saveAt = i
		case strings.HasPrefix(ev, "pub muehle/hf/ant-mode/state") && strings.Contains(ev, `"power_hold":true`) && holdAt < 0:
			holdAt = i
		case strings.HasPrefix(ev, "pub muehle/power/antennas/cmd") && onAt < 0:
			onAt = i
		}
	}
	if saveAt < 0 || holdAt < 0 || onAt < 0 || !(saveAt < holdAt && holdAt < onAt) {
		t.Fatalf("borrow order save=%d hold=%d on=%d: %v", saveAt, holdAt, onAt, h.j.ev)
	}
	if h.sv.last.Borrow == nil || h.sv.last.Borrow.RestoreTo != "off" || h.sv.last.Borrow.PowerSeqPhase != "standby" {
		t.Fatalf("persisted borrow = %+v", h.sv.last.Borrow)
	}
	if got := h.powerCmds(); len(got) != 1 || got[0] != "on" {
		t.Fatalf("power cmds %v", got)
	}
	if h.phase() != AwaitingPower {
		t.Fatalf("phase %s", h.phase())
	}

	h.e.PowerState(js(map[string]any{"power": "on", "device_online": true}))
	if h.phase() != AwaitingDevices {
		t.Fatalf("phase %s", h.phase())
	}
	h.e.RotatorStatus([]byte("online"))
	h.e.AntCtrlStatus([]byte("online"))
	if h.phase() != Grounding {
		t.Fatalf("phase %s", h.phase())
	}
	h.switchOff()
	h.retracted()
	h.rotator(float64(*h.e.lock.ParkAz), false)
	if h.phase() != Parked {
		t.Fatalf("phase %s", h.phase())
	}
	if got := h.powerCmds(); len(got) != 2 || got[1] != "off" {
		t.Fatalf("storm restore: %v", got)
	}
	if h.lastState()["power_hold"] != false || h.sv.last.Borrow != nil {
		t.Fatal("power_hold not released")
	}
	if !h.parsed().Locked() {
		t.Fatal("restore unlocked")
	}
}

func TestBorrowLeavesPowerWhenPowerSeqMoved(t *testing.T) {
	h := newH(t, borrowCfg, nil).start()
	h.station("standby", 170)
	h.powerOff()
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	h.powerOn()
	h.e.PowerSeqState(js(map[string]any{"phase": "waking"})) // operator came back
	h.e.PowerSeqState(js(map[string]any{"phase": "running"}))
	h.switchOff()
	h.retracted()
	h.rotator(float64(*h.e.lock.ParkAz), false)
	if got := h.powerCmds(); len(got) != 1 {
		t.Fatalf("restored although powerseq moved on: %v", got)
	}
	if h.lastState()["power_hold"] != false {
		t.Fatal("hold not released")
	}
}

func TestNoBorrowInTransitionOrDisabled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		borrow bool
		phase  string
	}{
		{"disabled", false, "standby"},
		{"starting", true, "starting"},
		{"waking", true, "waking"},
		{"stopping", true, "stopping"},
		{"entering_standby", true, "entering_standby"},
	} {
		h := newH(t, func(c *config.Config) { c.Power.Borrow = tc.borrow }, nil).start()
		h.station(tc.phase, 100)
		h.powerOff()
		h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
		if len(h.powerCmds()) != 0 || h.phase() != Grounding {
			t.Errorf("%s: borrowed (%v, %s)", tc.name, h.powerCmds(), h.phase())
		}
	}
	// Power already on: nothing to borrow.
	h := newH(t, borrowCfg, nil).start()
	h.station("standby", 100)
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	if len(h.powerCmds()) != 0 {
		t.Fatal("borrowed power that was on")
	}
}

func TestBorrowMaintenanceKeepsPowerUntilEnd(t *testing.T) {
	h := newH(t, borrowCfg, nil).start()
	h.station("idle", 100)
	h.powerOff()
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "maintenance"})
	h.powerOn()
	h.switchOff()
	h.retracted()
	h.rotator(135, false)
	if h.phase() != Parked || len(h.powerCmds()) != 1 || h.lastState()["power_hold"] != true {
		t.Fatalf("maintenance released power at park: %v %s", h.powerCmds(), h.phase())
	}
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "normal"})
	if got := h.powerCmds(); len(got) != 2 || got[1] != "off" || h.lastState()["power_hold"] != false {
		t.Fatalf("maintenance end: %v", got)
	}
}

func TestBorrowAbortMidway(t *testing.T) {
	h := newH(t, borrowCfg, nil).start()
	h.station("standby", 100)
	h.powerOff()
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	h.e.PowerState(js(map[string]any{"power": "on", "device_online": true}))
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "normal"})
	if got := h.powerCmds(); len(got) != 2 || got[1] != "off" {
		t.Fatalf("abort: %v", got)
	}
}

func TestBorrowDevicesTimeout(t *testing.T) {
	h := newH(t, borrowCfg, nil).start()
	h.station("standby", 100)
	h.powerOff()
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	h.e.PowerState(js(map[string]any{"power": "on", "device_online": true}))
	h.tick(239 * time.Second)
	if h.phase() != AwaitingDevices {
		t.Fatalf("phase %s", h.phase())
	}
	h.tick(2 * time.Second)
	// The lock is still built in full, so gear that comes up later finds it.
	if h.phase() != Grounding || len(h.powerCmds()) != 1 {
		t.Fatalf("after devices timeout: %s %v", h.phase(), h.powerCmds())
	}
	h.tick(61 * time.Second)  // ground timeout (radio rx)
	h.tick(121 * time.Second) // retract timeout
	if l := h.parsed().L(); !l.Retract || l.ParkAz == nil || h.phase() != Rotating {
		t.Fatalf("lock not completed: %+v %s", l, h.phase())
	}
	h.tick(301 * time.Second)
	if h.phase() != Failed || h.lastState()["fault"] == nil {
		t.Fatal("no failure")
	}
	if got := h.powerCmds(); len(got) != 2 || got[1] != "off" {
		t.Fatalf("storm failure did not restore: %v", got)
	}
}

func TestBorrowSaveFailureDoesNotBorrow(t *testing.T) {
	h := newH(t, borrowCfg, nil).start()
	h.station("standby", 100)
	h.powerOff()
	h.sv.fail = true
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	if len(h.powerCmds()) != 0 || h.e.borrow != nil || h.phase() != Grounding {
		t.Fatal("borrowed without a persisted restore")
	}
}

func TestBorrowPublishRetry(t *testing.T) {
	h := newH(t, borrowCfg, nil).start()
	h.station("standby", 100)
	h.powerOff()
	h.pub.fail = true
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	if h.e.borrow == nil || h.e.borrow.Sent {
		t.Fatal("borrow state wrong after a failed publish")
	}
	h.pub.fail = false
	h.tick(time.Second)
	if got := h.powerCmds(); len(got) != 1 || got[0] != "on" || !h.e.borrow.Sent {
		t.Fatalf("not retried: %v", got)
	}
}

func TestReparkAfterRestoreBorrowsAgain(t *testing.T) {
	h := newH(t, borrowCfg, nil).start()
	h.station("standby", 100)
	h.powerOff()
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	h.powerOn()
	h.switchOff()
	h.retracted()
	h.rotator(float64(*h.e.lock.ParkAz), false)
	h.powerOff() // restored
	h.cmd(map[string]any{"action": "repark"})
	if got := h.powerCmds(); len(got) != 3 || got[2] != "on" || h.phase() != AwaitingPower {
		t.Fatalf("repark did not borrow again: %v %s", got, h.phase())
	}
}

// ---------------------------------------------------------------------------
// persistence and restart
// ---------------------------------------------------------------------------

func TestPersistRoundTripAndRestart(t *testing.T) {
	h := newH(t, nil, nil).start()
	h.station("running", 170)
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	h.switchOff()
	h.retracted()
	saved := h.sv.last
	if saved == nil || saved.Policy.Mode != antmode.Storm || saved.Lock == nil || saved.Phase != Rotating {
		t.Fatalf("saved = %+v", saved)
	}
	b, _ := json.Marshal(saved)
	var loaded Persisted
	if err := json.Unmarshal(b, &loaded); err != nil {
		t.Fatal(err)
	}
	h2 := newH(t, nil, &loaded).start()
	st := h2.parsed()
	if !st.Locked() || st.L().Seq != saved.Lock.Seq || st.L().ParkAz == nil || *st.L().ParkAz != *saved.Lock.ParkAz {
		t.Fatalf("restart lock = %+v", st.L())
	}
	if h2.phase() != Rotating {
		t.Fatalf("restart phase %s", h2.phase())
	}
	// Next entry continues the seq.
	h2.cmd(map[string]any{"action": "set_ant_mode", "value": "normal"})
	h2.cmd(map[string]any{"action": "set_ant_mode", "value": "maintenance"})
	if h2.e.lock.Seq != saved.Lock.Seq+1 {
		t.Fatalf("seq after restart = %d", h2.e.lock.Seq)
	}
}

func TestCrashRestoreCompletesPendingRestore(t *testing.T) {
	loaded := &Persisted{
		Version: stateVersion,
		Policy:  policyState(antmode.Normal),
		Phase:   Idle,
		Borrow:  &Borrow{RestoreTo: "off", PowerSeqPhase: "standby", Since: t0.Add(-time.Hour), Sent: true},
	}
	h := newH(t, borrowCfg, loaded)
	h.e.SetConnected(true)
	h.station("standby", 100)
	h.tick(StartupHold)
	if got := h.powerCmds(); len(got) != 1 || got[0] != "off" {
		t.Fatalf("pending restore not completed: %v", got)
	}
	if h.sv.last.Borrow != nil {
		t.Fatal("borrow still persisted")
	}
}

func TestCrashMidStormBorrowResumes(t *testing.T) {
	loaded := &Persisted{
		Version: stateVersion,
		Policy:  policyState(antmode.Storm),
		Lock:    &antmode.Lock{Seq: 9, Ground: true},
		Phase:   AwaitingPower,
		Borrow:  &Borrow{RestoreTo: "off", PowerSeqPhase: "standby", Since: t0.Add(-time.Hour), Sent: false},
	}
	h := newH(t, borrowCfg, loaded)
	h.e.SetConnected(true)
	h.station("standby", 100)
	h.powerOff()
	h.tick(StartupHold)
	if got := h.powerCmds(); len(got) != 1 || got[0] != "on" {
		t.Fatalf("unsent power on not sent after restart: %v", got)
	}
	if h.phase() != AwaitingPower || h.parsed().L().Seq != 9 {
		t.Fatalf("phase %s", h.phase())
	}
}

func policyState(m antmode.Mode) (s policy.State) {
	s.Mode, s.Source, s.Since, s.Seq = m, "manual", t0.Add(-time.Hour), 9
	return s
}

func TestAdoptRetainedWhenNoStateFile(t *testing.T) {
	h := newH(t, nil, nil)
	h.e.SetConnected(true)
	h.e.OwnState([]byte(`{"ant_mode":"storm","source":"auto","since":"2026-10-22T10:00:00Z",` +
		`"lock":{"seq":41,"ground":true,"retract":true,"park_az":175,"nudge":false},"phase":"parked"}`))
	h.station("running", 175)
	h.tick(StartupHold)
	st := h.parsed()
	if !st.Locked() || st.L().Seq != 41 || *st.L().ParkAz != 175 || h.e.pol.S.Source != "auto" {
		t.Fatalf("adopted = %+v source %s", st.L(), h.e.pol.S.Source)
	}
	if h.sv.last == nil || h.sv.last.Lock == nil || h.sv.last.Lock.Seq != 41 {
		t.Fatal("adopted state not persisted")
	}
	// After ready the echo of our own /state is ignored.
	h.e.OwnState([]byte(`{"ant_mode":"normal"}`))
	if !h.e.pol.S.Mode.Valid() || h.e.pol.S.Mode != antmode.Storm {
		t.Fatal("own echo changed the mode")
	}
}

func TestAdoptIgnoresMalformedAndNormal(t *testing.T) {
	h := newH(t, nil, nil)
	h.e.SetConnected(true)
	h.e.OwnState([]byte(`{"ant_mode":"tornado"}`))
	h.e.OwnState([]byte(`{"ant_mode":"normal"}`))
	h.tick(StartupHold)
	if h.parsed().Locked() {
		t.Fatal("adopted garbage")
	}
	// Fresh seq is clock-seeded (opaque): the next entry does not reuse a small value.
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	if h.e.lock.Seq != uint64(t0.Unix())+1 {
		t.Fatalf("seq = %d", h.e.lock.Seq)
	}
}

func TestLoadedInconsistentStateIsRepaired(t *testing.T) {
	loaded := &Persisted{Version: stateVersion, Policy: policyState(antmode.Maintenance), Phase: "bogus"}
	h := newH(t, nil, loaded).start()
	l := h.parsed().L()
	if !h.parsed().Locked() || !l.Ground || !l.Nudge || h.phase() != Grounding {
		t.Fatalf("repaired = %+v %s", l, h.phase())
	}
}

func TestStateRepublishedOnReconnectAndDeduped(t *testing.T) {
	h := newH(t, nil, nil).start()
	h.station("running", 100)
	count := func() int {
		n := 0
		for _, m := range h.pub.msgs {
			if m.topic == "muehle/hf/ant-mode/state" {
				n++
			}
		}
		return n
	}
	n := count()
	h.tick(time.Second)
	h.tick(time.Second)
	if count() != n {
		t.Fatal("unchanged /state republished")
	}
	h.e.SetConnected(false)
	h.e.SetConnected(true)
	h.e.Republish()
	if count() != n+1 {
		t.Fatal("/state not republished on reconnect")
	}
	var meta map[string]any
	for _, m := range h.pub.msgs {
		if m.topic == "muehle/hf/ant-mode/meta" {
			_ = json.Unmarshal([]byte(m.payload), &meta)
		}
	}
	if meta["role"] != "ant-mode" {
		t.Fatalf("meta = %v", meta)
	}
	fields := meta["expose"].(map[string]any)["fields"].([]any)
	f0 := fields[0].(map[string]any)
	cmd := f0["command"].(map[string]any)
	if f0["key"] != "ant_mode" || f0["writable"] != true || cmd["action"] != "set_ant_mode" || cmd["value_key"] != "value" {
		t.Fatalf("expose ant_mode = %v", f0)
	}
}

// An abort before the Shelly reports on must still overwrite the retained
// set_power on: shelly-power-bridge replays its retained /cmd intent on every
// reconnect, so a skipped restore leaves the antennas powered for good.
func TestBorrowAbortBeforePowerOnRestores(t *testing.T) {
	h := newH(t, borrowCfg, nil).start()
	h.station("standby", 100)
	h.powerOff()
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "storm"})
	h.cmd(map[string]any{"action": "set_ant_mode", "value": "normal"})
	if got := h.powerCmds(); len(got) != 2 || got[0] != "on" || got[1] != "off" {
		t.Fatalf("abort before power on: %v", got)
	}
	if h.e.borrow != nil || h.lastState()["power_hold"] != false {
		t.Fatal("borrow not ended")
	}
}

// After a site power cut mid-borrow the Shelly boots off and antmode
// restarts: the retained on must still be overwritten.
func TestCrashRestoreWithPowerAlreadyOff(t *testing.T) {
	loaded := &Persisted{
		Version: stateVersion,
		Policy:  policyState(antmode.Normal),
		Phase:   Idle,
		Borrow:  &Borrow{RestoreTo: "off", PowerSeqPhase: "standby", Since: t0.Add(-time.Hour), Sent: true},
	}
	h := newH(t, borrowCfg, loaded)
	h.e.SetConnected(true)
	h.station("standby", 100)
	h.powerOff()
	h.tick(StartupHold)
	if got := h.powerCmds(); len(got) != 1 || got[0] != "off" {
		t.Fatalf("retained on not overwritten: %v", got)
	}
}

// auto_exit_at is a fixed instant between polls: idle Ticks during a quiet
// auto storm must not republish the retained /state.
func TestAutoExitAtStableBetweenPolls(t *testing.T) {
	h := newH(t, func(c *config.Config) { c.Storm.AutoEnter = true }, nil).start()
	h.station("standby", 100)
	ws := []nina.Warning{warn("dwd.1", t0.Add(-time.Hour), t0.Add(time.Minute))}
	h.e.NinaResult(ws, nil)
	h.tick(5 * time.Minute)
	h.e.NinaResult(nil, nil)
	h.tick(5 * time.Minute)
	h.e.NinaResult(nil, nil)
	lastPoll := h.clk.t
	want := h.lastState()["auto_exit_at"]
	if want == nil {
		t.Fatal("auto_exit_at not shown")
	}
	n := 0
	for _, m := range h.pub.msgs {
		if m.topic == "muehle/hf/ant-mode/state" {
			n++
		}
	}
	for i := 0; i < 10; i++ {
		h.tick(time.Second)
	}
	for _, m := range h.pub.msgs {
		if m.topic == "muehle/hf/ant-mode/state" {
			n--
		}
	}
	if n != 0 {
		t.Fatalf("%d /state republishes on idle Ticks", -n)
	}
	// holdoff (60 min) - 5 min quiet, counted from the last clean poll.
	if want != rfcTest(lastPoll.Add(55*time.Minute)) {
		t.Fatalf("auto_exit_at = %v", want)
	}
}

func rfcTest(t time.Time) string { return t.UTC().Format(time.RFC3339) }
