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

	"beamsteer/internal/config"
)

type msg struct {
	topic    string
	retained bool
	body     map[string]any
}

type fakePub struct {
	msgs []msg
	fail bool // simulate a down link: every publish errors, nothing recorded
}

func (f *fakePub) Publish(topic string, qos byte, retained bool, payload []byte) error {
	if f.fail {
		return errors.New("link down")
	}
	var m map[string]any
	_ = json.Unmarshal(payload, &m)
	f.msgs = append(f.msgs, msg{topic, retained, m})
	return nil
}

// cmds returns the /cmd messages for one topic since the last reset.
func (f *fakePub) cmds(topic string) []msg {
	var out []msg
	for _, m := range f.msgs {
		if m.topic == topic {
			out = append(out, m)
		}
	}
	return out
}

const (
	rotCmd = "muehle/hf/rotator/cmd"
	antCmd = "muehle/hf/ant-ctrl/cmd"
	self   = "muehle/hf/beam-steer"
)

func newEngine(t *testing.T) (*Engine, *fakePub) {
	e, pub, _ := newEngineClock(t)
	return e, pub
}

// newEngineClock also returns the engine's clock, for expiry tests.
func newEngineClock(t *testing.T) (*Engine, *fakePub, *time.Time) {
	t.Helper()
	cfg := config.Default()
	cfg.MQTT.Site, cfg.MQTT.Station = "muehle", "hf"
	cfg.Location, cfg.Host = "bauwagen", "shari"
	pub := &fakePub{}
	clock := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	e := New(cfg, pub, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return clock })
	e.SetConnected(true)
	e.AntModeState(nil) // no retained ant-mode state: normal, no startup hold
	return e, pub, &clock
}

// lastState is the most recent /state body.
func lastState(t *testing.T, pub *fakePub) map[string]any {
	t.Helper()
	st := pub.cmds(self + "/state")
	if len(st) == 0 {
		t.Fatal("no /state published")
	}
	return st[len(st)-1].body
}

func antState(e *Engine, dir, band string, moving bool) {
	b, _ := json.Marshal(map[string]any{"direction": dir, "band": band, "moving": moving, "device_online": true})
	e.AntState(b)
}

func dirs(pub *fakePub) []any {
	var out []any
	for _, m := range pub.cmds(antCmd) {
		out = append(out, m.body["value"])
	}
	return out
}

// live brings all three siblings online: rotator at az, ant-ctrl in dir.
func live(e *Engine, az float64, dir string) {
	e.RotatorStatus([]byte("online"))
	e.RotatorState([]byte(`{"az":` + ftoa(az) + `,"moving":false,"device_online":true}`))
	e.AntStatus([]byte("online"))
	e.AntState([]byte(`{"direction":"` + dir + `","moving":false,"device_online":true}`))
	e.RadioStatus([]byte("online"))
	e.RadioState([]byte(`{"band":"20m","tx":"rx","device_online":true}`))
}

func ftoa(f float64) string { b, _ := json.Marshal(f); return string(b) }

func TestDisabledIsPassthrough(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.Goto(260.4)
	r := pub.cmds(rotCmd)
	if len(r) != 1 || r[0].body["action"] != "set_az" || r[0].body["az"] != 260.0 || r[0].retained {
		t.Fatalf("rotator cmds = %+v", r)
	}
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Errorf("ant-ctrl cmds = %+v, want none", a)
	}
}

func TestBehindFlipsWithoutRotation(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(265)
	if r := pub.cmds(rotCmd); len(r) != 0 {
		t.Errorf("rotator cmds = %+v, want none", r)
	}
	a := pub.cmds(antCmd)
	if len(a) != 1 || a[0].body["value"] != "reverse" || a[0].body["action"] != "direction" || !a[0].retained {
		t.Fatalf("ant-ctrl cmds = %+v", a)
	}
	if a[0].body["ts"] != "2026-09-26T12:00:00Z" {
		t.Errorf("ts = %v", a[0].body["ts"])
	}
}

func TestOutsideRotatesToCheaperLobe(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(330) // reverse on 150 (60) beats forward on 330 (240)
	r := pub.cmds(rotCmd)
	if len(r) != 1 || r[0].body["az"] != 150.0 {
		t.Fatalf("rotator cmds = %+v", r)
	}
	if a := pub.cmds(antCmd); len(a) != 1 || a[0].body["value"] != "reverse" {
		t.Fatalf("ant-ctrl cmds = %+v", a)
	}
}

func TestBidirNeverSwitches(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "bidirectional")
	e.SetEnabled(true)
	e.Goto(230) // back lobe ±45
	e.Goto(340) // outside → rotate to 160
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Errorf("ant-ctrl cmds = %+v, want none", a)
	}
	if r := pub.cmds(rotCmd); len(r) != 1 || r[0].body["az"] != 160.0 {
		t.Errorf("rotator cmds = %+v", r)
	}
}

func TestDirectionHeldWhileTransmittingOrMoving(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.RadioState([]byte(`{"band":"20m","tx":"tx","device_online":true}`))
	e.Goto(270)
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Fatalf("sent during TX: %+v", a)
	}
	// Elements moving after TX ends: still held.
	e.AntState([]byte(`{"direction":"forward","moving":true,"device_online":true}`))
	e.RadioState([]byte(`{"band":"20m","tx":"rx","device_online":true}`))
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Fatalf("sent while moving: %+v", a)
	}
	e.AntState([]byte(`{"direction":"forward","moving":false,"device_online":true}`))
	if a := pub.cmds(antCmd); len(a) != 1 || a[0].body["value"] != "reverse" {
		t.Fatalf("after clear: %+v", a)
	}
}

func TestNewerRequestSupersedesPending(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.RadioState([]byte(`{"band":"20m","tx":"tx","device_online":true}`))
	e.Goto(270) // would reverse, held
	e.Goto(95)  // front lobe: nothing to do
	e.RadioState([]byte(`{"band":"20m","tx":"rx","device_online":true}`))
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Errorf("stale pending sent: %+v", a)
	}
}

func TestStaleTXFromDeadRadioDoesNotHold(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.RadioState([]byte(`{"band":"20m","tx":"tx","device_online":true}`))
	e.RadioStatus([]byte("offline"))
	e.Goto(270)
	if a := pub.cmds(antCmd); len(a) != 1 {
		t.Errorf("ant-ctrl cmds = %+v, want reverse", a)
	}
}

func TestRotatorOfflineIgnored(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.RotatorStatus([]byte("offline"))
	e.SetEnabled(true)
	e.Goto(150)
	if r := pub.cmds(rotCmd); len(r) != 0 {
		t.Errorf("rotator cmds = %+v", r)
	}
	if _, ok := e.Readback(); ok {
		t.Error("readback valid with rotator offline")
	}
}

func TestAntCtrlOfflinePlainRotation(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.AntStatus([]byte("offline"))
	e.SetEnabled(true)
	e.Goto(265)
	if r := pub.cmds(rotCmd); len(r) != 1 || r[0].body["az"] != 265.0 {
		t.Errorf("rotator cmds = %+v", r)
	}
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Errorf("ant-ctrl cmds = %+v", a)
	}
}

func TestSixMetreNeverReverses(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.RadioState([]byte(`{"band":"6m","tx":"rx","device_online":true}`))
	e.SetEnabled(true)
	e.Goto(270)
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Errorf("ant-ctrl cmds = %+v", a)
	}
	if r := pub.cmds(rotCmd); len(r) != 1 || r[0].body["az"] != 270.0 {
		t.Errorf("rotator cmds = %+v", r)
	}
}

func TestReadbackIsEffectiveHeading(t *testing.T) {
	e, _ := newEngine(t)
	live(e, 90, "reverse")
	if h, ok := e.Readback(); !ok || h != 270 {
		t.Errorf("readback = %v %v, want 270", h, ok)
	}
}

func TestStateAndStop(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(265)
	e.Stop()
	r := pub.cmds(rotCmd)
	if len(r) != 1 || r[0].body["action"] != "stop" {
		t.Errorf("stop = %+v", r)
	}
	st := pub.cmds(self + "/state")
	last := st[len(st)-1]
	if !last.retained || last.body["enabled"] != true {
		t.Fatalf("state = %+v", last)
	}
	l := last.body["last"].(map[string]any)
	if l["bearing"] != 265.0 || l["direction"] != "reverse" || l["reason"] == "" {
		t.Errorf("last = %+v", l)
	}
}

// --- 2026-10-01 review fixes ---------------------------------------------------

// ultrabridge republishes /state only on its 2 s poll: a request right after
// a flip must decide from the commanded direction, not the stale /state.
func TestFlipThenBehindBeforeAntStateCatchesUp(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 165, "reverse")
	e.SetEnabled(true)
	e.Goto(170) // front lobe while reversed → forward
	e.Goto(350) // now behind (ant-ctrl /state still says reverse)
	if got := dirs(pub); len(got) != 1 || got[0] != "forward" {
		t.Fatalf("before confirm: dirs = %v, want [forward] (reverse held)", got)
	}
	if lastState(t, pub)["pending"] != "reverse" {
		t.Fatalf("pending = %v, want reverse", lastState(t, pub)["pending"])
	}
	antState(e, "forward", "20m", false) // controller confirms the first flip
	if got := dirs(pub); len(got) != 2 || got[1] != "reverse" {
		t.Fatalf("after confirm: dirs = %v, want [forward reverse]", got)
	}
}

func TestDuplicateRequestDoesNotResendFlip(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(270)
	e.Goto(272)
	if got := dirs(pub); len(got) != 1 {
		t.Fatalf("dirs = %v, want one reverse", got)
	}
}

func TestSecondRequestDecidesFromCommandedTarget(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(150) // rotate to 150 fwd
	e.Goto(155) // rotator /state still says 90: must judge from 150 → front lobe
	if r := pub.cmds(rotCmd); len(r) != 1 {
		t.Fatalf("rotator cmds = %+v, want one", r)
	}
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Fatalf("ant cmds = %+v, want none", a)
	}
}

func TestUnconfirmedFlipExpires(t *testing.T) {
	e, pub, clock := newEngineClock(t)
	live(e, 165, "reverse")
	e.SetEnabled(true)
	e.Goto(170) // forward sent, never confirmed
	e.Goto(350) // reverse held behind it
	*clock = clock.Add(dirSettle + time.Second)
	e.Tick()
	// The window lapsed: decide from the reported direction again. It still
	// reads reverse, so the held reverse is already satisfied — nothing more.
	if got := dirs(pub); len(got) != 1 {
		t.Fatalf("dirs = %v, want only the first forward", got)
	}
	if _, held := lastState(t, pub)["pending"]; held {
		t.Fatal("pending still set after expiry")
	}
}

func TestSixMetreUsesUltrabeamBand(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward") // radio says 20m
	antState(e, "forward", "6m", false)
	e.SetEnabled(true)
	e.Goto(270)
	if got := dirs(pub); len(got) != 0 {
		t.Fatalf("dirs = %v, want none on 6m", got)
	}
}

// A held 180° that can no longer go out (now on 6m) is decided again: the
// boom must turn to the forward target instead of staying 180° off.
func TestHeldReverseRedecidedAfterQSYTo6m(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.RadioState([]byte(`{"band":"20m","tx":"tx","device_online":true}`))
	e.Goto(270) // behind: reverse, held during TX
	antState(e, "forward", "6m", false)
	e.RadioState([]byte(`{"band":"6m","tx":"rx","device_online":true}`))
	if got := dirs(pub); len(got) != 0 {
		t.Fatalf("dirs = %v, want none (no reverse on 6m)", got)
	}
	r := pub.cmds(rotCmd)
	if len(r) != 1 || r[0].body["az"] != 270.0 {
		t.Fatalf("rotator cmds = %+v, want set_az 270 (forward on 6m)", r)
	}
	if l := lastState(t, pub)["last"].(map[string]any); !strings.Contains(l["reason"].(string), "after hold") {
		t.Fatalf("last.reason = %v", l["reason"])
	}
}

func TestArrivalAndStopRetireCommandedTarget(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(150)
	e.RotatorState([]byte(`{"az":150,"moving":false,"device_online":true}`))
	if e.cmdAz != nil {
		t.Fatal("cmdAz not retired on arrival")
	}
	e.Goto(250) // rotate again
	e.Stop()
	if e.cmdAz != nil {
		t.Fatal("cmdAz not retired by stop")
	}
	_ = pub
}

func TestRetargetedElsewhereRetiresCommandedTarget(t *testing.T) {
	e, _ := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(150)
	e.RotatorState([]byte(`{"az":100,"target_az":150,"moving":true,"device_online":true}`)) // ack
	e.RotatorState([]byte(`{"az":110,"target_az":20,"moving":true,"device_online":true}`))  // console aimed elsewhere
	if e.cmdAz != nil {
		t.Fatal("cmdAz kept after another client retargeted the rotator")
	}
	if got := e.effectiveAz(); got != 20 {
		t.Fatalf("effectiveAz = %v, want the new target 20", got)
	}
}

func TestUnacknowledgedRotateExpiresQuickly(t *testing.T) {
	e, _, clock := newEngineClock(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(150)
	*clock = clock.Add(rotAckWindow + time.Second)
	e.Tick()
	if got := e.effectiveAz(); got != 90 {
		t.Fatalf("effectiveAz = %v, want 90 (set_az never acknowledged)", got)
	}
}

func TestStateCarriesInputs(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	in := lastState(t, pub)["inputs"].(map[string]any)
	if in["rotator"] != true || in["ant_ctrl"] != true || in["radio"] != true {
		t.Fatalf("inputs = %v", in)
	}
	e.RotatorStatus([]byte("offline"))
	in = lastState(t, pub)["inputs"].(map[string]any)
	if in["rotator"] != false {
		t.Fatalf("inputs after rotator offline = %v", in)
	}
}

func TestToggleClearsLastDecision(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(265)
	e.SetEnabled(false)
	if _, ok := lastState(t, pub)["last"]; ok {
		t.Fatal("last kept across the toggle")
	}
}

// --- second review pass -----------------------------------------------------

// A WRC frame emitted before our set_az applied (old target, already
// moving) must neither acknowledge nor retire our target.
func TestStaleMovingFrameKeepsCommandedTarget(t *testing.T) {
	e, _ := newEngine(t)
	live(e, 90, "forward")
	e.RotatorState([]byte(`{"az":100,"target_az":150,"moving":true,"device_online":true}`))
	e.SetEnabled(true)
	e.Goto(220)                                                                             // moving toward 150 already; beyond the front lobe of 150
	e.RotatorState([]byte(`{"az":101,"target_az":150,"moving":true,"device_online":true}`)) // stale
	if got := e.effectiveAz(); got != 220 {
		t.Fatalf("effectiveAz = %v, want 220 (stale frame retired our target)", got)
	}
	e.RotatorState([]byte(`{"az":102,"target_az":220,"moving":true,"device_online":true}`)) // ours
	e.RotatorState([]byte(`{"az":110,"target_az":30,"moving":true,"device_online":true}`))  // someone else
	if got := e.effectiveAz(); got != 30 {
		t.Fatalf("effectiveAz = %v, want 30 after a real retarget", got)
	}
}

// A STOP from the console or M5 dial goes straight to the rotator: once it
// rests short of our target, its own az is the truth.
func TestExternalStopShortRetiresTarget(t *testing.T) {
	for _, tgt := range []string{`,"target_az":150`, ``} { // tdeg kept, or omitted
		e, pub := newEngine(t)
		live(e, 90, "forward")
		e.SetEnabled(true)
		e.Goto(150)
		e.RotatorState([]byte(`{"az":95` + tgt + `,"moving":true,"device_online":true}`))
		e.RotatorState([]byte(`{"az":110` + tgt + `,"moving":false,"device_online":true}`))
		if got := e.effectiveAz(); got != 110 {
			t.Fatalf("target%q: effectiveAz = %v, want 110", tgt, got)
		}
		e.Goto(150) // the same station again: must rotate again
		if r := pub.cmds(rotCmd); len(r) != 2 {
			t.Fatalf("target%q: rotator cmds = %+v, want a second set_az", tgt, r)
		}
	}
}

// The radio QSYs first; the Ultrabeam follows seconds later. 6m from
// either live source forbids reverse.
func TestRadioOn6mBeforeUltrabeamFollows(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	antState(e, "forward", "20m", false)
	e.RadioState([]byte(`{"band":"6m","tx":"rx","device_online":true}`))
	e.SetEnabled(true)
	e.Goto(330)
	if got := dirs(pub); len(got) != 0 {
		t.Fatalf("dirs = %v, want none", got)
	}
	if r := pub.cmds(rotCmd); len(r) != 1 || r[0].body["az"] != 330.0 {
		t.Fatalf("rotator cmds = %+v, want set_az 330 forward", r)
	}
}

// The operator sets BI while our flip is in flight: the flip is dropped and
// a held change is decided again against BI — no override.
func TestOperatorBIDuringInFlightFlip(t *testing.T) {
	e, pub, clock := newEngineClock(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.Goto(270) // reverse sent
	e.Goto(95)  // forward held behind the in-flight reverse
	antState(e, "bidirectional", "20m", false)
	*clock = clock.Add(dirSettle + time.Second)
	e.Tick()
	if got := dirs(pub); len(got) != 1 || got[0] != "reverse" {
		t.Fatalf("dirs = %v, want only the first reverse (BI kept)", got)
	}
}

// A down link must not leave phantom in-flight state behind.
func TestPublishFailureLeavesNothingInFlight(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	pub.fail = true
	e.Goto(150)
	if e.cmdAz != nil {
		t.Fatal("cmdAz set although set_az was not sent")
	}
	e.Goto(270) // flip only
	if e.cmdDir != "" || e.pending == nil {
		t.Fatalf("cmdDir=%q pending=%v; want no cmdDir and the flip kept for retry", e.cmdDir, e.pending)
	}
	pub.fail = false
	e.Tick()
	if got := dirs(pub); len(got) != 1 || got[0] != "reverse" {
		t.Fatalf("dirs after recovery = %v, want reverse", got)
	}
}

func TestNotConnectedIgnoresRequests(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetConnected(false)
	e.Goto(150)
	if r := pub.cmds(rotCmd); len(r) != 0 {
		t.Fatalf("rotator cmds = %+v, want none", r)
	}
	if e.last == nil || !strings.Contains(e.last.Reason, "not connected") {
		t.Fatalf("last = %+v", e.last)
	}
}

// --- antenna-mode lock (hf/ant-mode) -------------------------------------------

// setAntMode delivers a hf/ant-mode/state payload the way the mqtt layer
// does: record (paho goroutine), then apply (worker).
func setAntMode(e *Engine, payload string) {
	e.AntModeState([]byte(payload))
	e.SyncAntMode()
}

const stormLock = `{"ant_mode":"storm","lock":{"seq":1,"ground":true,"retract":true,"park_az":175}}`

func TestLockIgnoresRequestsAndPublishesNothing(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		e, pub := newEngine(t)
		live(e, 90, "forward")
		e.SetEnabled(enabled)
		setAntMode(e, stormLock)
		e.Goto(265) // behind: would flip
		e.Goto(150) // outside: would rotate (or plain rotation when disabled)
		if r := pub.cmds(rotCmd); len(r) != 0 {
			t.Errorf("enabled=%v: rotator cmds = %+v, want none", enabled, r)
		}
		if a := pub.cmds(antCmd); len(a) != 0 {
			t.Errorf("enabled=%v: ant-ctrl cmds = %+v, want none", enabled, a)
		}
		st := lastState(t, pub)
		if st["lock"] != "storm" {
			t.Errorf("enabled=%v: state lock = %v, want storm", enabled, st["lock"])
		}
		l := st["last"].(map[string]any)
		if l["bearing"] != 150.0 || l["reason"] != "antenna mode storm: ignored" || l["rotate_to"] != nil {
			t.Errorf("enabled=%v: last = %+v", enabled, l)
		}
	}
}

func TestLockDropsPendingFlip(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.RadioState([]byte(`{"band":"20m","tx":"tx","device_online":true}`))
	e.Goto(265) // behind, held while transmitting
	if lastState(t, pub)["pending"] != "reverse" {
		t.Fatalf("pending = %v, want reverse", lastState(t, pub)["pending"])
	}
	setAntMode(e, stormLock)
	st := lastState(t, pub)
	if _, ok := st["pending"]; ok || st["lock"] != "storm" {
		t.Fatalf("state after lock = %+v, want no pending, lock storm", st)
	}
	e.RadioState([]byte(`{"band":"20m","tx":"rx","device_online":true}`))
	e.Tick()
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Fatalf("ant-ctrl cmds = %+v, want none", a)
	}
	// Unlock: the dropped flip is not replayed.
	setAntMode(e, `{"ant_mode":"normal"}`)
	e.Tick()
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Fatalf("ant-ctrl cmds after unlock = %+v, want none (no replay)", a)
	}
	if _, ok := lastState(t, pub)["lock"]; ok {
		t.Errorf("state after unlock = %+v, want no lock", lastState(t, pub))
	}
}

// A lock recorded by the paho handler whose apply job was lost (full jobs
// queue) still blocks: every motion path applies the recorded lock first.
func TestRecordedLockBlocksWithoutApplyJob(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	e.AntModeState([]byte(stormLock)) // no SyncAntMode
	e.Goto(150)
	if r := pub.cmds(rotCmd); len(r) != 0 {
		t.Fatalf("rotator cmds = %+v, want none", r)
	}
	if lastState(t, pub)["lock"] != "storm" {
		t.Fatalf("state = %+v", lastState(t, pub))
	}
}

func TestLockStopNotForwarded(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	setAntMode(e, `{"ant_mode":"maintenance","lock":{"seq":2,"ground":true,"nudge":true}}`)
	e.Stop()
	if r := pub.cmds(rotCmd); len(r) != 0 {
		t.Fatalf("rotator cmds = %+v, want none (logger STOP must not cancel the park)", r)
	}
	if lastState(t, pub)["lock"] != "maintenance" {
		t.Errorf("state lock = %v", lastState(t, pub)["lock"])
	}
	setAntMode(e, `{"ant_mode":"normal"}`)
	e.Stop()
	if r := pub.cmds(rotCmd); len(r) != 1 || r[0].body["action"] != "stop" {
		t.Fatalf("rotator cmds after unlock = %+v, want one stop", r)
	}
}

func TestUnlockResumes(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	e.SetEnabled(true)
	setAntMode(e, stormLock)
	e.Goto(265)
	setAntMode(e, ``) // retained state cleared → normal
	if a := pub.cmds(antCmd); len(a) != 0 {
		t.Fatalf("ant-ctrl cmds = %+v, want none (no replay on unlock)", a)
	}
	e.Goto(265)
	if got := dirs(pub); len(got) != 1 || got[0] != "reverse" {
		t.Fatalf("dirs after unlock = %v, want [reverse]", got)
	}
}

func TestMalformedAntModeKeepsLastKnown(t *testing.T) {
	e, pub := newEngine(t)
	live(e, 90, "forward")
	setAntMode(e, stormLock)
	setAntMode(e, `{"ant_mode":"hurricane"}`)
	setAntMode(e, `not json`)
	e.Goto(150)
	if r := pub.cmds(rotCmd); len(r) != 0 {
		t.Fatalf("rotator cmds = %+v, want none (malformed must not unlock)", r)
	}
	if lastState(t, pub)["lock"] != "storm" {
		t.Errorf("state lock = %v", lastState(t, pub)["lock"])
	}
}

func TestReadbackWhileLocked(t *testing.T) {
	e, _ := newEngine(t)
	live(e, 90, "reverse")
	setAntMode(e, stormLock)
	e.RotatorState([]byte(`{"az":175,"moving":false,"device_online":true}`))
	if h, ok := e.Readback(); !ok || h != 355 {
		t.Errorf("readback = %v %v, want 355", h, ok)
	}
}

func newEngineNoAntMode(t *testing.T, slot string) (*Engine, *fakePub, *time.Time) {
	t.Helper()
	cfg := config.Default()
	cfg.MQTT.Site, cfg.MQTT.Station = "muehle", "hf"
	cfg.Location, cfg.Host = "bauwagen", "scmino"
	cfg.Steer.AntModeSlot = slot
	pub := &fakePub{}
	clock := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	e := New(cfg, pub, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return clock })
	return e, pub, &clock
}

// Until the retained ant-mode state lands (or the hold lapses with none),
// nothing is sent: a request must not slip in ahead of a storm lock.
func TestStartupHold(t *testing.T) {
	e, pub, clock := newEngineNoAntMode(t, "ant-mode")
	e.SetConnected(true)
	e.Goto(150) // smart rotation off: plain rotation needs no sibling data
	e.Stop()
	if r := pub.cmds(rotCmd); len(r) != 0 {
		t.Fatalf("rotator cmds during hold = %+v, want none", r)
	}
	if e.last == nil || e.last.Reason != "antenna mode not yet known: ignored" {
		t.Fatalf("last = %+v", e.last)
	}
	*clock = clock.Add(antModeHold)
	e.Goto(150)
	if r := pub.cmds(rotCmd); len(r) != 1 || r[0].body["az"] != 150.0 {
		t.Fatalf("rotator cmds after hold = %+v, want set_az 150", r)
	}

	// A state that lands inside the hold ends it at once.
	e, pub, _ = newEngineNoAntMode(t, "ant-mode")
	e.SetConnected(true)
	setAntMode(e, `{"ant_mode":"normal"}`)
	e.Goto(150)
	if r := pub.cmds(rotCmd); len(r) != 1 {
		t.Fatalf("rotator cmds = %+v, want set_az", r)
	}
}

func TestAntModeDisabledByConfig(t *testing.T) {
	e, pub, _ := newEngineNoAntMode(t, "")
	e.SetConnected(true)
	e.Goto(150)
	if r := pub.cmds(rotCmd); len(r) != 1 {
		t.Fatalf("rotator cmds = %+v, want set_az (no hold without an ant-mode slot)", r)
	}
}

func TestMetaExposesLock(t *testing.T) {
	e, pub := newEngine(t)
	e.Republish()
	m := pub.cmds(self + "/meta")
	if len(m) != 1 {
		t.Fatalf("meta = %+v", m)
	}
	fields := m[0].body["expose"].(map[string]any)["fields"].([]any)
	found := false
	for _, f := range fields {
		if f.(map[string]any)["key"] == "lock" {
			found = true
		}
	}
	if !found {
		t.Errorf("expose fields = %+v, want lock", fields)
	}
}

// After a broker outage the pre-outage lock is not authoritative until the
// post-reconnect retained replay lands (or the hold lapses): antmode may have
// entered storm while beamsteer was cut off.
func TestReconnectHoldsUntilReplay(t *testing.T) {
	e, pub, clock := newEngineNoAntMode(t, "ant-mode")
	e.SetConnected(true)
	setAntMode(e, `{"ant_mode":"normal"}`)
	live(e, 90, "forward")
	e.Goto(150)
	if r := pub.cmds(rotCmd); len(r) != 1 {
		t.Fatalf("rotator cmds before outage = %+v, want one set_az", r)
	}

	// Outage; storm entered meanwhile. Reconnect: the request queued ahead
	// of the retained replay must not go out on the stale "normal".
	e.SetConnected(false)
	e.AntModeConnecting() // OnConnect, before the subscriptions
	e.SetConnected(true)
	e.Goto(160)
	if r := pub.cmds(rotCmd); len(r) != 1 {
		t.Fatalf("rotator cmds before replay = %+v, want none new", r)
	}
	if e.last == nil || e.last.Reason != "antenna mode not yet known: ignored" {
		t.Fatalf("last = %+v", e.last)
	}
	setAntMode(e, stormLock) // the replay
	e.Goto(160)
	if r := pub.cmds(rotCmd); len(r) != 1 {
		t.Fatalf("rotator cmds after storm replay = %+v, want none new", r)
	}
	if e.last.Reason != "antenna mode storm: ignored" {
		t.Fatalf("last = %+v", e.last)
	}

	// Another outage, no replay at all (retained state lost): the last known
	// lock stays enforced after the hold lapses — never unlock by default.
	e.SetConnected(false)
	e.AntModeConnecting()
	e.SetConnected(true)
	*clock = clock.Add(antModeHold)
	e.Goto(160)
	if r := pub.cmds(rotCmd); len(r) != 1 {
		t.Fatalf("rotator cmds after hold, no replay = %+v, want none new", r)
	}
	if e.last.Reason != "antenna mode storm: ignored" {
		t.Fatalf("last = %+v", e.last)
	}

	// A replay that already arrived when the worker runs SetConnected (the
	// worker was busy) is not discarded: no needless hold.
	setAntMode(e, `{"ant_mode":"normal"}`)
	e.SetConnected(false)
	e.AntModeConnecting()
	e.AntModeState([]byte(`{"ant_mode":"normal"}`)) // handler ran first
	e.SetConnected(true)
	e.Goto(170)
	if r := pub.cmds(rotCmd); len(r) != 2 || r[1].body["az"] != 170.0 {
		t.Fatalf("rotator cmds after fresh replay = %+v, want set_az 170", r)
	}
}

// Every new refusal reason (startup hold → storm, storm → maintenance) warns
// afresh, so `journalctl -p warning` shows the lock that is refusing.
func TestIgnoredWarnPerLockReason(t *testing.T) {
	var buf strings.Builder
	cfg := config.Default()
	cfg.MQTT.Site, cfg.MQTT.Station = "muehle", "hf"
	clock := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	e := New(cfg, &fakePub{}, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
		func() time.Time { return clock })
	e.SetConnected(true)
	warns := func() int { return strings.Count(buf.String(), "rotate request ignored: antenna locked") }

	e.Goto(150) // startup hold
	e.Goto(150)
	if n := warns(); n != 1 {
		t.Fatalf("warns during hold = %d, want 1", n)
	}
	setAntMode(e, stormLock)
	e.Goto(150)
	e.Goto(150)
	if n := warns(); n != 2 || !strings.Contains(buf.String(), "lock=\"antenna mode storm\"") {
		t.Fatalf("warns after storm = %d, want 2 (one for the storm)\n%s", n, buf.String())
	}
	setAntMode(e, `{"ant_mode":"maintenance","lock":{"seq":2,"ground":true}}`)
	e.Goto(150)
	if n := warns(); n != 3 {
		t.Fatalf("warns after maintenance = %d, want 3", n)
	}
	setAntMode(e, `{"ant_mode":"maintenance","lock":{"seq":2,"ground":true}}`) // same lock again
	e.Goto(150)
	if n := warns(); n != 3 {
		t.Fatalf("warns after repeated maintenance = %d, want 3", n)
	}
}
