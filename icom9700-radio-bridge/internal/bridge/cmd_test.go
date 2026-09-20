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

// bharness: fake radio + real manager + real bridge over a recording
// client, all timers shrunk.
type bharness struct {
	t   *testing.T
	f   *civ.FakeRadio
	b   *Bridge
	cli *recClient
	ctx context.Context
}

func newBH(t *testing.T) *bharness {
	t.Helper()
	return newBHWithTimings(t, 0, 10*time.Second)
}

// newBHWithTimings shrinks the safety bounds: txWatchdog feeds the bridge's
// max-TX bound (0 = the 180 s default), lossWatchdog the session's
// silence-detection (so tests can force a session loss via SetSilent).
func newBHWithTimings(t *testing.T, txWatchdog, lossWatchdog time.Duration) *bharness {
	t.Helper()
	f := civ.NewFakeRadio(t)
	mgr := radio.NewManager(radio.Config{
		Host:            "127.0.0.1",
		Username:        "bridge",
		Password:        "hunter2",
		IdleTimeout:     10 * time.Second, // long: tests drive demand explicitly
		MaxAttempts:     2,
		AttemptSpacing:  30 * time.Millisecond,
		CmdWait:         time.Second,
		ControlPort:     f.Addr().Port,
		CIVPort:         f.CIVPort(),
		AreYouThere:     25 * time.Millisecond,
		HandshakeBudget: 1500 * time.Millisecond,
		LossWatchdog:    lossWatchdog,
		Logger:          slog.Default(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Run(ctx) }()

	b, err := New(Options{
		Site: "muehle", Station: "uhf", Slot: "radio",
		Location: "bauwagen", Host: "9700.kgbvax.net",
		DeviceModel:  "Icom IC-9700",
		Manager:      mgr,
		PollInterval: 100 * time.Millisecond,
		TXWatchdog:   txWatchdog,
		Logger:       slog.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cli := &recClient{}
	b.wireClient(cli)
	t.Cleanup(b.Close)
	return &bharness{t: t, f: f, b: b, cli: cli, ctx: ctx}
}

// armAndLive arms through the real cmd path and waits for the session.
func (h *bharness) armAndLive() {
	h.t.Helper()
	h.cli.sendCmd([]byte(`{"action":"arm"}`))
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		m := h.cli.lastState(h.t)
		if armed, _ := m["armed"].(bool); armed {
			if ss, _ := m["session_state"].(string); ss == radio.StateLive {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("armed+live not reached")
}

// pollNow runs one poll tick synchronously.
func (h *bharness) pollNow() { h.b.poll() }

// PTT while unarmed: the exact taxonomy rejection, and the radio frame is
// NEVER sent (plan U5 scenario).
func TestPttUnarmedRejected(t *testing.T) {
	h := newBH(t)
	h.cli.sendCmd([]byte(`{"action":"ptt","value":"on"}`))
	h.cli.waitCmdErr(t, errPttNotArmed)
	if h.f.PTT() {
		t.Error("PTT frame reached the radio while unarmed")
	}
	// One-shot: the cmd was cleared after the rejection.
	waitCmdCleared(t, h.cli)
}

// PTT while armed+live keys the radio; PTT off clears (plan U5 scenarios).
func TestPttArmedLive(t *testing.T) {
	h := newBH(t)
	h.armAndLive()

	h.cli.sendCmd([]byte(`{"action":"ptt","value":"on"}`))
	waitTrue(t, "fake PTT keyed", 3*time.Second, func() bool { return h.f.PTT() })
	waitCmdErr(t, h.cli, "")
	waitTrue(t, "tx:tx in /state", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		tx, _ := m["tx"].(string)
		return tx == "tx"
	})

	h.cli.sendCmd([]byte(`{"action":"ptt","value":"off"}`))
	waitTrue(t, "fake PTT released", 3*time.Second, func() bool { return !h.f.PTT() })
	waitTrue(t, "tx:rx back in /state", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		tx, _ := m["tx"].(string)
		return tx == "rx"
	})
}

// sat_mode while tx is on → rejected (R10 gate).
func TestSatModeRejectedWhileTx(t *testing.T) {
	h := newBH(t)
	h.armAndLive()

	h.cli.sendCmd([]byte(`{"action":"ptt","value":"on"}`))
	waitTrue(t, "fake PTT keyed", 3*time.Second, func() bool { return h.f.PTT() })

	h.cli.sendCmd([]byte(`{"action":"sat_mode","value":"on"}`))
	waitCmdErr(t, h.cli, "sat_mode rejected: tx is on or armed")
	if h.f.SelectedVFO() == "" {
		t.Error("sanity")
	}
}

// set_freq on SUB above the SUB band windows → the band rejection.
func TestSetFreqSub23cmRejected(t *testing.T) {
	h := newBH(t)
	h.armAndLive()

	h.cli.sendCmd([]byte(`{"action":"set_freq","value":"1296100000","vfo":"sub"}`))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e := h.cli.stateErr(t); strings.Contains(e, "out of band for sub") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("/state.error = %q, want the sub-band rejection", h.cli.stateErr(t))
}

// DV mode set → rejected as unsupported (R7).
func TestSetModeDVRejected(t *testing.T) {
	h := newBH(t)
	h.armAndLive()

	h.cli.sendCmd([]byte(`{"action":"set_mode","value":"dv","vfo":"sub"}`))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e := h.cli.stateErr(t); strings.Contains(e, "unsupported mode") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("/state.error = %q, want the unsupported-mode rejection", h.cli.stateErr(t))
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
	h.cli.sendCmd([]byte(`{"action":"set_freq","value":"432100000","vfo":"main","ts":"` + old + `"}`))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e := h.cli.stateErr(t); strings.Contains(e, "stale cmd") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("/state.error = %q, want the stale-cmd rejection", h.cli.stateErr(t))
}

// An unknown action is rejected (warn + drop semantics), not dispatched.
func TestUnknownActionRejected(t *testing.T) {
	h := newBH(t)
	h.cli.sendCmd([]byte(`{"action":"self_destruct"}`))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e := h.cli.stateErr(t); strings.Contains(e, "unknown cmd action") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("/state.error = %q, want the unknown-action rejection", h.cli.stateErr(t))
}

// set_freq main updates the /state top-level freq (via the poll, since the
// fake emits no transceives — plan U5's "via transceive or poll").
func TestSetFreqReflectsInState(t *testing.T) {
	h := newBH(t)
	h.armAndLive()

	h.cli.sendCmd([]byte(`{"action":"set_freq","value":"432650000","vfo":"main"}`))
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		h.pollNow()
		h.b.publishState(false)
		m := h.cli.lastState(t)
		mainIface, ok := m["main"].(map[string]any)
		if ok {
			if fh, _ := mainIface["freq_hz"].(float64); fh == 432650000 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("set_freq never reflected in /state.main.freq_hz")
}

// Satellite-mode state assembly: with satellite on, the top-level fields
// mirror SUB (the uplink) even with MAIN selected (plan U5 scenario).
func TestSatelliteMirrorsSub(t *testing.T) {
	h := newBH(t)
	h.armAndLive()

	h.cli.sendCmd([]byte(`{"action":"set_freq","value":"432650000","vfo":"main"}`))
	// Let the cmd land, then switch satellite mode on (radio idle → the
	// R10 gate passes).
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		h.pollNow()
		m := h.cli.lastState(t)
		if mainIface, ok := m["main"].(map[string]any); ok {
			if fh, _ := mainIface["freq_hz"].(float64); fh == 432650000 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	// R10: sat_mode is rejected while armed or keyed — disarm first (the
	// radio stays live via the demand that follows).
	h.cli.sendCmd([]byte(`{"action":"disarm"}`))
	waitTrue(t, "disarmed", 3*time.Second, func() bool {
		m := h.cli.lastState(t)
		armed, _ := m["armed"].(bool)
		return !armed
	})

	h.cli.sendCmd([]byte(`{"action":"sat_mode","value":"on"}`))
	waitTrue(t, "satellite mode in /state", 4*time.Second, func() bool {
		h.pollNow()
		h.b.publishState(false)
		m := h.cli.lastState(t)
		sat, _ := m["satellite"].(bool)
		if !sat {
			t.Logf("poll state: sat=%v err=%v sel=%v", m["satellite"], m["error"], m["selected_vfo"])
		}
		return sat
	})
	// Select MAIN explicitly; the top level must STILL mirror SUB.
	h.cli.sendCmd([]byte(`{"action":"set_freq","value":"144120000","vfo":"main"}`))
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < 10; i++ {
		h.pollNow()
		h.b.publishState(false)
		m := h.cli.lastState(t)
		fh, _ := m["freq_hz"].(float64)
		if fh == 145800000 { // SUB's frequency, not MAIN's 144.12 MHz
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("top-level freq_hz = %v, want the SUB uplink (145800000)", func() any {
		m := h.cli.lastState(t)
		return m["freq_hz"]
	}())
}

// Meters appear while live and dedup-suppress while unchanged (KTD-8).
func TestMetersPollAndDedup(t *testing.T) {
	h := newBH(t)
	h.armAndLive()
	h.f.SetSMeter(120)

	h.pollNow()
	h.cli.sendCmd([]byte(`{"action":"arm"}`)) // no-op republish trigger
	waitTrue(t, "s_meter published", 2*time.Second, func() bool {
		m := h.cli.lastState(t)
		v, _ := m["s_meter"].(float64)
		return v == 120
	})

	// Unchanged state: publishState dedups — count /state publishes over a
	// few forced calls.
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

	// A meter change republishes.
	h.f.SetSMeter(200)
	h.pollNow()
	h.b.publishState(false)
	waitTrue(t, "new s_meter published", time.Second, func() bool {
		m := h.cli.lastState(t)
		v, _ := m["s_meter"].(float64)
		return v == 200
	})
}

// Retained /meta never regresses: the birth certificate carries the
// capabilities and the READ-ONLY expose (no actions), published at the
// connect ritual before any radio identity (plan U5 scenario).
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
	if caps["bias_t"] != true || caps["satellite"] != true {
		t.Errorf("capabilities = %v", caps)
	}
	expose := meta["expose"].(map[string]any)
	if _, hasActions := expose["actions"]; hasActions {
		t.Error("expose carries actions — the v1 posture is read-only")
	}
}

// --- helpers shared with bridge_test.go -------------------------------------

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
