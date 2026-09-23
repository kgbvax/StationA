package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"icom9700-radio-bridge/internal/civ"
	"icom9700-radio-bridge/internal/radio"
)

// recClient records every publish and routes the /cmd subscription.
type recClient struct {
	mu   sync.Mutex
	pubs []recPub
	h    func(payload []byte)
}

type recPub struct {
	topic   string
	retain  bool
	payload []byte
}

func (c *recClient) publish(topic string, _ byte, retain bool, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pubs = append(c.pubs, recPub{topic: topic, retain: retain, payload: append([]byte(nil), payload...)})
}
func (c *recClient) subscribe(_ string, _ byte, h func(payload []byte)) { c.h = h }
func (c *recClient) isConnected() bool                                  { return true }
func (c *recClient) disconnect(_ uint)                                  {}

func (c *recClient) sendCmd(payload []byte) {
	if c.h != nil {
		c.h(payload)
	}
}

// lastState decodes the most recent /state publish.
func (c *recClient) lastState(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.pubs) - 1; i >= 0; i-- {
		if strings.HasSuffix(c.pubs[i].topic, "/state") {
			var m map[string]any
			if err := json.Unmarshal(c.pubs[i].payload, &m); err != nil {
				t.Fatalf("state payload: %v", err)
			}
			return m
		}
	}
	t.Fatal("no /state publish recorded")
	return nil
}

func (c *recClient) stateErr(t *testing.T) string {
	t.Helper()
	m := c.lastState(t)
	e, _ := m["error"].(string)
	return e
}

// waitCmdErr polls until /state.error equals want (the jobs worker is
// asynchronous).
func (c *recClient) waitCmdErr(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.stateErr(t) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("/state.error = %q, want %q", c.stateErr(t), want)
}

// fakeMonitor is the in-process Monitor stand-in: records the toggle/wake
// calls and lets tests push telemetry snapshots.
type fakeMonitor struct {
	mu      sync.Mutex
	on      bool
	woke    int
	st      RadioState
	updates chan struct{}
}

func newFakeMonitor() *fakeMonitor {
	return &fakeMonitor{updates: make(chan struct{}, 1)}
}

func (f *fakeMonitor) SetMonitor(on bool) error {
	f.mu.Lock()
	f.on = on
	f.mu.Unlock()
	f.nudge()
	return nil
}

func (f *fakeMonitor) Wake(_ context.Context) error {
	f.mu.Lock()
	f.woke++
	f.mu.Unlock()
	return nil
}

func (f *fakeMonitor) Snapshot() RadioState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeMonitor) Updates() <-chan struct{} { return f.updates }

func (f *fakeMonitor) Close() {}

func (f *fakeMonitor) isOn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.on
}

func (f *fakeMonitor) woken() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.woke
}

func (f *fakeMonitor) push(st RadioState) {
	f.mu.Lock()
	f.st = st
	f.mu.Unlock()
	f.nudge()
}

func (f *fakeMonitor) nudge() {
	select {
	case f.updates <- struct{}{}:
	default:
	}
}

// bharness: fake radio + real manager + real bridge over a recording
// client, all timers shrunk.
type bharness struct {
	t   *testing.T
	f   *civ.FakeRadio
	b   *Bridge
	cli *recClient
	mon *fakeMonitor // nil unless newBHWithMonitor
	ctx context.Context
}

func newBH(t *testing.T) *bharness {
	return newBHWithMonitor(t, false)
}

// newBHMon wires the bridge with the fake serial monitor.
func newBHMon(t *testing.T) *bharness {
	return newBHWithMonitor(t, true)
}

func newBHWithMonitor(t *testing.T, withMonitor bool) *bharness {
	t.Helper()
	f := civ.NewFakeRadio(t)
	mgr := radio.NewManager(radio.Config{
		Host:            "127.0.0.1",
		Username:        "bridge",
		Password:        "hunter2",
		IdleTimeout:     10 * time.Second, // long: tests drive demand explicitly
		MaxAttempts:     2,
		AttemptSpacing:  30 * time.Millisecond,
		HandshakeTO:     time.Second,
		ControlPort:     f.Addr().Port,
		CIVPort:         f.CIVPort(),
		AreYouThere:     25 * time.Millisecond,
		HandshakeBudget: 1500 * time.Millisecond,
		LossWatchdog:    10 * time.Second,
		Logger:          slog.Default(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Run(ctx) }()

	var mon *fakeMonitor
	var monIface Monitor
	if withMonitor {
		mon = newFakeMonitor()
		monIface = mon
	}

	b, err := New(Options{
		Site: "muehle", Station: "uhf", Slot: "radio",
		Location: "bauwagen", Host: "9700.kgbvax.net",
		DeviceModel: "Icom IC-9700",
		Manager:     mgr,
		Monitor:     monIface,
		Logger:      slog.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cli := &recClient{}
	b.wireClient(cli)
	if monIface != nil {
		// Run() starts followMonitor — the on-change path the telemetry
		// tests exercise (wireClient alone leaves the updates undrained).
		go b.Run()
	}
	t.Cleanup(b.Close)
	return &bharness{t: t, f: f, b: b, cli: cli, mon: mon, ctx: ctx}
}

// captureLive drives audio_on through the real cmd path and waits for the
// capture session to go live.
func (h *bharness) captureLive() {
	h.t.Helper()
	h.cli.sendCmd([]byte(`{"action":"audio_on"}`))
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		m := h.cli.lastState(h.t)
		if dem, _ := m["audio_demand"].(bool); dem {
			if ss, _ := m["session_state"].(string); ss == radio.StateLive {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("audio_demand+live not reached")
}

// audio_on connects the capture session; /state carries audio_demand and
// the live capture state (the session's only hold source).
func TestAudioOnOffDispatch(t *testing.T) {
	h := newBH(t)
	h.captureLive()

	h.cli.sendCmd([]byte(`{"action":"audio_off"}`))
	waitTrue(t, "audio_demand cleared in /state", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		dem, _ := m["audio_demand"].(bool)
		return !dem
	})
	// One-shot: the cmd was cleared after execution.
	waitCmdCleared(t, h.cli)
}

// monitor_on without a configured serial device is rejected with the
// observed fact (not a guess), and the cmd still clears.
func TestMonitorRejectionWithoutSerial(t *testing.T) {
	h := newBH(t)
	h.cli.sendCmd([]byte(`{"action":"monitor_on"}`))
	h.cli.waitCmdErr(t, "monitor unavailable: serial.device not configured")
	waitCmdCleared(t, h.cli)
}

// power_on without a configured serial device is rejected likewise.
func TestPowerOnRejectionWithoutSerial(t *testing.T) {
	h := newBH(t)
	h.cli.sendCmd([]byte(`{"action":"power_on"}`))
	h.cli.waitCmdErr(t, "power_on not configured (serial.device empty)")
	waitCmdCleared(t, h.cli)
}

// monitor_on/off toggle the monitor and the /state monitor flag; pushed
// telemetry flows into /state (on change).
func TestMonitorToggleAndTelemetry(t *testing.T) {
	h := newBHMon(t)

	h.cli.sendCmd([]byte(`{"action":"monitor_on"}`))
	waitTrue(t, "monitor flag in /state", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		on, _ := m["monitor"].(bool)
		return on
	})
	if !h.mon.isOn() {
		t.Error("monitor.SetMonitor(true) never reached the monitor")
	}

	// Telemetry lands in /state (responding radio).
	h.mon.push(RadioState{
		Responding: true,
		FreqHz:     432650000,
		Band:       "70cm",
		Mode:       "fm",
		SMeter:     intPtr(120),
	})
	waitTrue(t, "telemetry in /state", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		fh, _ := m["freq_hz"].(float64)
		return fh == 432650000
	})
	m := h.cli.lastState(t)
	if v, _ := m["s_meter"].(float64); v != 120 {
		t.Errorf("s_meter = %v", m["s_meter"])
	}
	if m["radio_responding"] != true {
		t.Errorf("radio_responding = %v", m["radio_responding"])
	}

	// Deaf radio: measured fields omitted (never zeroed, never frozen),
	// radio_responding flips false.
	h.mon.push(RadioState{Responding: false})
	waitTrue(t, "deaf gate hides telemetry", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		if rr, ok := m["radio_responding"].(bool); ok && !rr {
			if _, has := m["freq_hz"]; has {
				t.Logf("freq_hz still present while deaf")
				return false
			}
			return true
		}
		return false
	})

	// monitor_off drops the telemetry immediately.
	h.cli.sendCmd([]byte(`{"action":"monitor_off"}`))
	waitTrue(t, "monitor flag cleared", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		on, _ := m["monitor"].(bool)
		return !on
	})
	if _, has := h.cli.lastState(t)["freq_hz"]; has {
		t.Error("telemetry present while the monitor is off")
	}
}

// power_on with a configured monitor wakes it exactly once, cleanly.
func TestPowerOnDispatch(t *testing.T) {
	h := newBHMon(t)
	h.cli.sendCmd([]byte(`{"action":"power_on"}`))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.mon.woken() >= 1 {
			waitCmdErr(t, h.cli, "")
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("power_on never reached the monitor")
}

// An oversized payload gets the FIXED rejection (no attacker bytes in the
// retained error) and no echo (plan U5 scenario).
func TestOversizedCmdFixedRejection(t *testing.T) {
	h := newBH(t)
	big := make([]byte, cmdMaxBytes+100)
	for i := range big {
		big[i] = 'A'
	}
	h.cli.sendCmd(big)
	h.cli.waitCmdErr(t, cmdErrMsgTooLarge)

	// No attacker-chosen bytes rode into any publish.
	h.cli.mu.Lock()
	defer h.cli.mu.Unlock()
	for _, p := range h.cli.pubs {
		if strings.Contains(string(p.payload), "AAAA") {
			t.Error("oversized payload bytes echoed into a publish")
		}
	}
}

// A stamped-but-stale cmd is dropped with the staleness rejection.
func TestStaleCmdDropped(t *testing.T) {
	h := newBH(t)
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	h.cli.sendCmd([]byte(`{"action":"audio_on","ts":"` + old + `"}`))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e := h.cli.stateErr(t); strings.Contains(e, "stale cmd") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("/state.error = %q, want the stale-cmd rejection", h.cli.stateErr(t))
}

// An unknown action is rejected (warn + drop semantics), not dispatched —
// and control actions are unknown now: the whole LAN command path is gone.
func TestControlActionsRejected(t *testing.T) {
	h := newBH(t)
	for _, action := range []string{"ptt", "arm", "disarm", "set_freq", "set_mode", "sat_mode", "set_power"} {
		h.cli.sendCmd([]byte(`{"action":"` + action + `"}`))
		deadline := time.Now().Add(3 * time.Second)
		ok := false
		for time.Now().Before(deadline) {
			if e := h.cli.stateErr(t); strings.Contains(e, "unknown cmd action") {
				ok = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !ok {
			t.Fatalf("/state.error = %q, want the unknown-action rejection for %q", h.cli.stateErr(t), action)
		}
	}
	if h.f.PTT() {
		t.Error("a PTT frame reached the radio through some path")
	}
}

// Unchanged snapshots do not republish; a changed one does (the dedup the
// on-change cadence relies on).
func TestStateDedupe(t *testing.T) {
	h := newBHMon(t)
	h.cli.sendCmd([]byte(`{"action":"monitor_on"}`))
	waitTrue(t, "monitor flag in /state", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		on, _ := m["monitor"].(bool)
		return on
	})

	h.cli.mu.Lock()
	before := len(h.cli.pubs)
	h.cli.mu.Unlock()
	h.b.publishState(false)
	h.b.publishState(false)
	h.cli.mu.Lock()
	after := len(h.cli.pubs)
	h.cli.mu.Unlock()
	if after != before {
		t.Errorf("unchanged snapshot republished (%d -> %d publishes)", before, after)
	}

	// A telemetry change republishes.
	h.mon.push(RadioState{Responding: true, FreqHz: 144500000, Band: "2m", Mode: "usb"})
	waitTrue(t, "changed snapshot republished", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		fh, _ := m["freq_hz"].(float64)
		return fh == 144500000
	})
}

// Retained /meta never regresses: the birth certificate carries the
// capabilities and the READ-ONLY, control-free expose (no tx, no armed, no
// actions), published at the connect ritual.
func TestMetaReadOnlyExpose(t *testing.T) {
	h := newBH(t)
	h.cli.mu.Lock()
	defer h.cli.mu.Unlock()
	var meta map[string]any
	for i := len(h.cli.pubs) - 1; i >= 0; i-- {
		if strings.HasSuffix(h.cli.pubs[i].topic, "/meta") {
			if err := json.Unmarshal(h.cli.pubs[i].payload, &meta); err != nil {
				t.Fatalf("meta payload: %v", err)
			}
			break
		}
	}
	if meta == nil {
		t.Fatal("no /meta publish recorded")
	}
	if meta["role"] != "radio" {
		t.Errorf("role = %v", meta["role"])
	}
	caps := meta["capabilities"].(map[string]any)
	if _, has := caps["vfos"]; has {
		t.Error("capabilities still advertise vfos — dual-VFO control is gone")
	}
	expose := meta["expose"].(map[string]any)
	if _, hasActions := expose["actions"]; hasActions {
		t.Error("expose carries actions — the posture is read-only")
	}
	fields := expose["fields"].([]any) //nolint:forcetypeassert
	for _, fi := range fields {
		f := fi.(map[string]any) //nolint:forcetypeassert
		key := f["key"].(string) //nolint:forcetypeassert
		if key == "tx" || key == "armed" {
			t.Errorf("expose carries control field %q — the pivot removed it", key)
		}
	}
}

// --- helpers shared across bridge tests --------------------------------------

func intPtr(v int) *int { return &v }

func waitCmdCleared(t *testing.T, cli *recClient) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cli.mu.Lock()
		found := false
		for i := len(cli.pubs) - 1; i >= 0; i-- {
			if strings.HasSuffix(cli.pubs[i].topic, "/cmd") {
				found = len(cli.pubs[i].payload) == 0
				break
			}
		}
		cli.mu.Unlock()
		if found {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("retained /cmd never cleared after the rejection")
}

func waitCmdErr(t *testing.T, cli *recClient, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cli.stateErr(t) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("/state.error = %q, want %q", cli.stateErr(t), want)
}

func waitTrue(t *testing.T, what string, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s not observed within %s", what, d)
}
