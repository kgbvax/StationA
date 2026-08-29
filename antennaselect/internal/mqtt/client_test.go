package mqtt

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"

	"antennaselect/internal/config"
	"antennaselect/internal/reconcile"
)

// okToken is a non-blocking token: Wait() returns immediately (the recording fake's
// Publish never touches a broker). Used by the PA band-follow publish test.
type okToken struct{}

func (okToken) Wait() bool                     { return true }
func (okToken) WaitTimeout(time.Duration) bool { return true }
func (okToken) Done() <-chan struct{}          { return make(chan struct{}) }
func (okToken) Error() error                   { return nil }

// recordedMsg captures one Publish call.
type recordedMsg struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

// fakePaho embeds a nil paho.Client so only Publish is exercised; the test never calls any
// other paho method (no Connect/Subscribe/IsConnectionOpen on this path).
type fakePaho struct {
	paho.Client
	pub func(topic string, qos byte, retained bool, payload any) paho.Token
}

func (f fakePaho) Publish(topic string, qos byte, retained bool, payload any) paho.Token {
	return f.pub(topic, qos, retained, payload)
}

// blockingToken blocks Wait() until `release` is closed. It stands in for a slow broker:
// its Wait() blocks indefinitely until released, the exact condition that deadlocked paho's
// inline dispatch before the worker fix (update publishes synchronously; an inline
// onRadioState would block paho's matchAndDispatch goroutine waiting for the PUBACK the
// stalled read loop can never deliver).
type blockingToken struct{ release <-chan struct{} }

func (t blockingToken) Wait() bool { <-t.release; return true }
func (t blockingToken) WaitTimeout(d time.Duration) bool {
	select {
	case <-t.release:
		return true
	case <-time.After(d):
		return false
	}
}
func (t blockingToken) Done() <-chan struct{} { return make(chan struct{}) }
func (t blockingToken) Error() error          { return nil }

// fakeMessage is a minimal paho.Message for handler tests that never touch a real client.
type fakeMessage struct {
	topic   string
	payload []byte
}

func (m fakeMessage) Duplicate() bool   { return false }
func (m fakeMessage) Qos() byte         { return 0 }
func (m fakeMessage) Retained() bool    { return false }
func (m fakeMessage) Topic() string     { return m.topic }
func (m fakeMessage) MessageID() uint16 { return 0 }
func (m fakeMessage) Payload() []byte   { return m.payload }
func (m fakeMessage) Ack()              {}

// TestOnRadioStateDefersReconcile is the regression guard for the paho-handler deadlock.
// onRadioState runs on paho's matchAndDispatch goroutine (OrderMatters is the default) and
// must NOT run update() inline, because update() publishes synchronously and that blocks
// dispatch after the first message. Against the pre-fix code (`c.update(...)` called
// directly inside onRadioState) this test hangs at the first select and fails on timeout;
// with the worker it returns immediately and the deferred reconcile/publish later runs
// (and blocks) on the worker goroutine, not paho's.
func TestOnRadioStateDefersReconcile(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	var once sync.Once
	var calls int32
	fake := fakePaho{pub: func(string, byte, bool, any) paho.Token {
		atomic.AddInt32(&calls, 1)
		once.Do(func() { close(reached) })
		return blockingToken{release: release}
	}}

	// Minimal reconciler: update() reaches publishJSON on the first call regardless of the
	// decision (haveDecision is false), so the publish — and thus the block — is hit.
	rec := reconcile.New(config.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		client: fake,
		rec:    rec,
		site:   "s", station: "st", slot: "sl",
		jobs:   make(chan func(), 256),
		ctx:    ctx,
		cancel: cancel,
	}
	// The worker is started only after the Phase-1 calls==0 check below, so that
	// assertion is deterministic — no goroutine is draining the queue while
	// onRadioState runs. Starting the worker here would race the check: the worker
	// could reach the (blocking) publish and increment `calls` before the assertion,
	// producing a false "work ran inline" failure. The deadlock teeth come from the
	// Phase-1 timeout, which fires against inline code whether or not a worker exists.

	msg := fakeMessage{
		topic:   "s/st/radio/state",
		payload: []byte(`{"band":"20m","freq_hz":14000000,"tx":"rx"}`),
	}

	// Phase 1 — onRadioState must return without waiting for the (blocked) publish.
	returned := make(chan struct{})
	go func() {
		c.onRadioState(nil, msg)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("onRadioState blocked: reconcile/publish ran inline on the caller (paho-dispatch) goroutine")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("publish happened during onRadioState (%d call(s)); work ran inline, not deferred", got)
	}

	// Phase 2 — the worker runs the deferred update and reaches the blocking publish.
	go sharedmqtt.RunJobs(ctx, c.jobs)
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("worker never reached the deferred publish")
	}
	close(release) // let the publish complete
	cancel()       // tell the worker to exit (cancels its ctx)
}

// TestPABandFollowPublishesSetBandNotRetained drives update() with a SetBand-producing
// input and asserts the PA /cmd emit is QoS 1 and NOT retained (the pa-mqtt-api contract),
// dedups an unchanged band, and advances on a band change — in contrast to the ant-switch
// select emit, which IS retained.
func TestPABandFollowPublishesSetBandNotRetained(t *testing.T) {
	var (
		mu     sync.Mutex
		record []recordedMsg
	)
	cfg := config.Config{
		Location: "bauwagen", Host: "shari",
		MQTT: config.MQTT{Site: "muehle", Station: "hf", Slot: "antenna-select"},
		WiringMap: map[string]string{
			"port1": "dummy-load", "port3": "ultrabeam", "port6": "fan-dipole", "off": "grounded",
		},
		BandPolicy: config.BandPolicy{
			Bands:    map[string][]string{"ultrabeam": {"20m"}, "fan-dipole": {"40m"}},
			Fallback: "fan-dipole",
		},
		BandFollow: config.BandFollow{Resource: "ultrabeam", Slot: "ant-ctrl"},
		PAFollow:   config.PAFollow{Enabled: true, Slot: "pa"},
	}
	fake := fakePaho{pub: func(topic string, qos byte, retained bool, payload any) paho.Token {
		// publishJSON marshals to []byte before calling Publish, so payload is already
		// the on-the-wire bytes — record it as-is (re-marshalling []byte base64-encodes it).
		b, _ := payload.([]byte)
		mu.Lock()
		record = append(record, recordedMsg{topic, qos, retained, b})
		mu.Unlock()
		return okToken{}
	}}
	c := &Client{
		client: fake,
		cfg:    cfg,
		rec:    reconcile.New(cfg),
		site:   "muehle", station: "hf", slot: "antenna-select",
		jobs: make(chan func(), 256),
	}

	paCmds := func() []recordedMsg {
		mu.Lock()
		defer mu.Unlock()
		var out []recordedMsg
		for _, m := range record {
			if m.topic == "muehle/hf/pa/cmd" {
				out = append(out, m)
			}
		}
		return out
	}

	// First update: radio online on 20m -> set_band 20m published, NOT retained.
	c.update(func(in *reconcile.Inputs) {
		in.RadioOnline = true
		in.RadioBand = "20m"
		in.RadioTX = reconcile.TXReceive
		in.StationActivity = "active"
	})
	if got := paCmds(); len(got) != 1 {
		t.Fatalf("after 20m: expected 1 pa/cmd publish, got %d", len(got))
	} else {
		m := got[0]
		if m.retained {
			t.Error("pa/cmd must be NOT retained (pa-mqtt-api contract)")
		}
		if m.qos != 1 {
			t.Errorf("pa/cmd qos = %d, want 1", m.qos)
		}
		var p struct {
			Action string `json:"action"`
			Value  string `json:"value"`
		}
		if err := json.Unmarshal(m.payload, &p); err != nil {
			t.Fatalf("unmarshal pa/cmd: %v", err)
		}
		if p.Action != "set_band" || p.Value != "20m" {
			t.Errorf("pa/cmd payload = %+v, want action=set_band value=20m", p)
		}
	}

	// Same band again: deduped — no new pa/cmd.
	c.update(func(in *reconcile.Inputs) {
		in.RadioOnline = true
		in.RadioBand = "20m"
		in.RadioTX = reconcile.TXReceive
		in.StationActivity = "active"
	})
	if got := paCmds(); len(got) != 1 {
		t.Errorf("after repeat 20m: expected still 1 pa/cmd (dedup), got %d", len(got))
	}

	// Band changes to 40m: a new pa/cmd, still not retained.
	c.update(func(in *reconcile.Inputs) {
		in.RadioOnline = true
		in.RadioBand = "40m"
		in.RadioTX = reconcile.TXReceive
		in.StationActivity = "active"
	})
	if got := paCmds(); len(got) != 2 {
		t.Fatalf("after 40m: expected 2 pa/cmd publishes, got %d", len(got))
	}
	if got := paCmds()[1]; got.retained {
		t.Error("second pa/cmd must also be NOT retained")
	}

	// Contrast: the ant-switch select emit (and any frequency emit) IS retained. The
	// first update resolved target port3 against an unknown switch selection, so a select
	// was published retained to ant-switch/cmd.
	mu.Lock()
	var antSwitch *recordedMsg
	for i := range record {
		if record[i].topic == "muehle/hf/ant-switch/cmd" {
			antSwitch = &record[i]
			break
		}
	}
	mu.Unlock()
	if antSwitch == nil {
		t.Fatal("expected a retained ant-switch/cmd select (contrast with not-retained pa/cmd)")
	}
	if !antSwitch.retained {
		t.Error("ant-switch/cmd select must be retained — contrast with the not-retained pa/cmd emit")
	}
}

// TestTunerFollowPublishesSetInlineNotRetained drives update() with a SetInline-producing
// input and asserts the tuner /cmd emit is QoS 1, NOT retained (mirrors the PA /cmd contract),
// carries {"action":"set_inline","value":<bool>}, dedups an unchanged value, and advances on
// a change (engage -> bypass).
func TestTunerFollowPublishesSetInlineNotRetained(t *testing.T) {
	var (
		mu     sync.Mutex
		record []recordedMsg
	)
	cfg := config.Config{
		Location: "bauwagen", Host: "shari",
		MQTT: config.MQTT{Site: "muehle", Station: "hf", Slot: "antenna-select"},
		WiringMap: map[string]string{
			"port1": "dummy-load", "port3": "ultrabeam", "port6": "fan-dipole", "off": "grounded",
		},
		BandPolicy: config.BandPolicy{
			Bands:    map[string][]string{"ultrabeam": {"20m"}, "fan-dipole": {"30m", "40m"}},
			Fallback: "fan-dipole",
		},
		BandFollow:  config.BandFollow{Resource: "ultrabeam", Slot: "ant-ctrl"},
		TunerFollow: config.TunerFollow{Enabled: true, Slot: "tuner", Resource: "fan-dipole", ATUBands: []string{"30m"}},
	}
	fake := fakePaho{pub: func(topic string, qos byte, retained bool, payload any) paho.Token {
		b, _ := payload.([]byte)
		mu.Lock()
		record = append(record, recordedMsg{topic, qos, retained, b})
		mu.Unlock()
		return okToken{}
	}}
	c := &Client{
		client: fake,
		cfg:    cfg,
		rec:    reconcile.New(cfg),
		site:   "muehle", station: "hf", slot: "antenna-select",
		jobs: make(chan func(), 256),
	}

	tunerCmds := func() []recordedMsg {
		mu.Lock()
		defer mu.Unlock()
		var out []recordedMsg
		for _, m := range record {
			if m.topic == "muehle/hf/tuner/cmd" {
				out = append(out, m)
			}
		}
		return out
	}

	// First update: radio online on 30m, fan-dipole (port6) selected -> set_inline true, not retained.
	c.update(func(in *reconcile.Inputs) {
		in.RadioOnline = true
		in.RadioBand = "30m"
		in.RadioTX = reconcile.TXReceive
		in.StationActivity = "active"
		in.SwitchSelected = "port6"
	})
	if got := tunerCmds(); len(got) != 1 {
		t.Fatalf("after 30m: expected 1 tuner/cmd publish, got %d", len(got))
	} else {
		m := got[0]
		if m.retained {
			t.Error("tuner/cmd must be NOT retained")
		}
		if m.qos != 1 {
			t.Errorf("tuner/cmd qos = %d, want 1", m.qos)
		}
		var p struct {
			Action string `json:"action"`
			Value  bool   `json:"value"`
		}
		if err := json.Unmarshal(m.payload, &p); err != nil {
			t.Fatalf("unmarshal tuner/cmd: %v", err)
		}
		if p.Action != "set_inline" || !p.Value {
			t.Errorf("tuner/cmd payload = %+v, want action=set_inline value=true", p)
		}
	}

	// Same state again: deduped — no new tuner/cmd.
	c.update(func(in *reconcile.Inputs) {
		in.RadioOnline = true
		in.RadioBand = "30m"
		in.RadioTX = reconcile.TXReceive
		in.StationActivity = "active"
		in.SwitchSelected = "port6"
	})
	if got := tunerCmds(); len(got) != 1 {
		t.Errorf("after repeat 30m: expected still 1 tuner/cmd (dedup), got %d", len(got))
	}

	// Switch to 40m (resonant on fan-dipole, not in atu_bands): set_inline false, not retained.
	c.update(func(in *reconcile.Inputs) {
		in.RadioOnline = true
		in.RadioBand = "40m"
		in.RadioTX = reconcile.TXReceive
		in.StationActivity = "active"
		in.SwitchSelected = "port6"
	})
	if got := tunerCmds(); len(got) != 2 {
		t.Fatalf("after 40m: expected 2 tuner/cmd publishes, got %d", len(got))
	} else {
		m := got[1]
		if m.retained {
			t.Error("second tuner/cmd must also be NOT retained")
		}
		var p struct {
			Action string `json:"action"`
			Value  bool   `json:"value"`
		}
		if err := json.Unmarshal(m.payload, &p); err != nil {
			t.Fatalf("unmarshal second tuner/cmd: %v", err)
		}
		if p.Action != "set_inline" || p.Value {
			t.Errorf("second tuner/cmd payload = %+v, want action=set_inline value=false", p)
		}
	}
}

// TestTunerFollowDisabledEmitsNothing asserts that with TunerFollow disabled (the default),
// no tuner/cmd is published even on a non-resonant band — the reconciler returns nil SetInline.
func TestTunerFollowDisabledEmitsNothing(t *testing.T) {
	var (
		mu     sync.Mutex
		record []recordedMsg
	)
	cfg := config.Config{
		Location: "bauwagen", Host: "shari",
		MQTT: config.MQTT{Site: "muehle", Station: "hf", Slot: "antenna-select"},
		WiringMap: map[string]string{
			"port1": "dummy-load", "port3": "ultrabeam", "port6": "fan-dipole", "off": "grounded",
		},
		BandPolicy: config.BandPolicy{
			Bands:    map[string][]string{"fan-dipole": {"30m", "40m"}},
			Fallback: "fan-dipole",
		},
		// TunerFollow left at its zero value (disabled).
	}
	fake := fakePaho{pub: func(topic string, qos byte, retained bool, payload any) paho.Token {
		b, _ := payload.([]byte)
		mu.Lock()
		record = append(record, recordedMsg{topic, qos, retained, b})
		mu.Unlock()
		return okToken{}
	}}
	c := &Client{
		client: fake,
		cfg:    cfg,
		rec:    reconcile.New(cfg),
		site:   "muehle", station: "hf", slot: "antenna-select",
		jobs: make(chan func(), 256),
	}
	c.update(func(in *reconcile.Inputs) {
		in.RadioOnline = true
		in.RadioBand = "30m"
		in.RadioTX = reconcile.TXReceive
		in.StationActivity = "active"
		in.SwitchSelected = "port6"
	})
	mu.Lock()
	defer mu.Unlock()
	for _, m := range record {
		if m.topic == "muehle/hf/tuner/cmd" {
			t.Errorf("tuner disabled: did not expect a tuner/cmd publish, got %s", m.payload)
		}
	}
}

// TestNoAntSwitchCmdOnFrequencyChangeWithinSameBand is a regression guard for the
// reported symptom: changing VFO within the same band must not command the 1:6 relay
// switch. The reconciler resolves the target from band only; the switch is already on
// that target; no ant-switch/cmd should be emitted.
func TestNoAntSwitchCmdOnFrequencyChangeWithinSameBand(t *testing.T) {
	var (
		mu     sync.Mutex
		record []recordedMsg
	)
	cfg := config.Config{
		Location: "bauwagen", Host: "shari",
		MQTT: config.MQTT{Site: "muehle", Station: "hf", Slot: "antenna-select"},
		WiringMap: map[string]string{
			"port1": "dummy-load", "port3": "ultrabeam", "port6": "fan-dipole", "off": "grounded",
		},
		BandPolicy: config.BandPolicy{
			Bands:    map[string][]string{"ultrabeam": {"20m"}, "fan-dipole": {"40m"}},
			Fallback: "fan-dipole",
		},
	}
	fake := fakePaho{pub: func(topic string, qos byte, retained bool, payload any) paho.Token {
		b, _ := payload.([]byte)
		mu.Lock()
		record = append(record, recordedMsg{topic, qos, retained, b})
		mu.Unlock()
		return okToken{}
	}}
	c := &Client{
		client: fake, cfg: cfg, rec: reconcile.New(cfg),
		site: "muehle", station: "hf", slot: "antenna-select",
		jobs: make(chan func(), 256),
	}

	// Radio on 20m and switch already reports port3 (ultrabeam).
	c.update(func(in *reconcile.Inputs) {
		in.RadioOnline = true
		in.RadioBand = "20m"
		in.RadioFreqHz = 14_000_000
		in.RadioTX = reconcile.TXReceive
		in.StationActivity = "active"
		in.SwitchSelected = "port3"
	})
	mu.Lock()
	baseline := len(record)
	mu.Unlock()

	// New radio/state: different frequency, still 20m.
	c.update(func(in *reconcile.Inputs) {
		in.RadioFreqHz = 14_200_000
	})

	mu.Lock()
	defer mu.Unlock()
	for _, m := range record[baseline:] {
		if m.topic == "muehle/hf/ant-switch/cmd" {
			t.Errorf("expected no ant-switch/cmd on frequency-only change, got %s", m.payload)
		}
	}
}

// TestReassertionAfterManualOverride verifies that if the switch is manually moved away
// from the reconciler's target, a new radio change re-issues the select. Previously
// lastSelect dedup suppressed this, leaving the station on the wrong antenna.
func TestReassertionAfterManualOverride(t *testing.T) {
	var (
		mu     sync.Mutex
		record []recordedMsg
	)
	cfg := config.Config{
		Location: "bauwagen", Host: "shari",
		MQTT: config.MQTT{Site: "muehle", Station: "hf", Slot: "antenna-select"},
		WiringMap: map[string]string{
			"port1": "dummy-load", "port3": "ultrabeam", "port6": "fan-dipole", "off": "grounded",
		},
		BandPolicy: config.BandPolicy{
			Bands:    map[string][]string{"ultrabeam": {"20m"}, "fan-dipole": {"40m"}},
			Fallback: "fan-dipole",
		},
	}
	fake := fakePaho{pub: func(topic string, qos byte, retained bool, payload any) paho.Token {
		b, _ := payload.([]byte)
		mu.Lock()
		record = append(record, recordedMsg{topic, qos, retained, b})
		mu.Unlock()
		return okToken{}
	}}
	c := &Client{
		client: fake, cfg: cfg, rec: reconcile.New(cfg),
		site: "muehle", station: "hf", slot: "antenna-select",
		jobs: make(chan func(), 256),
	}

	antSwitchCmds := func() []recordedMsg {
		mu.Lock()
		defer mu.Unlock()
		var out []recordedMsg
		for _, m := range record {
			if m.topic == "muehle/hf/ant-switch/cmd" {
				out = append(out, m)
			}
		}
		return out
	}

	// 1. Startup: target port3, switch reports port1 -> emit select port3.
	c.update(func(in *reconcile.Inputs) {
		in.RadioOnline = true
		in.RadioBand = "20m"
		in.RadioFreqHz = 14_000_000
		in.RadioTX = reconcile.TXReceive
		in.StationActivity = "active"
		in.SwitchSelected = "port1"
	})
	if got := antSwitchCmds(); len(got) != 1 {
		t.Fatalf("expected initial select, got %d cmds", len(got))
	}

	// 2. Switch now reports port3 (command took effect).
	c.update(func(in *reconcile.Inputs) {
		in.SwitchSelected = "port3"
	})
	if got := antSwitchCmds(); len(got) != 1 {
		t.Fatalf("expected no new cmd when switch reaches target, got %d", len(got))
	}

	// 3. Manual override: switch moved back to port1 while target is still port3.
	c.update(func(in *reconcile.Inputs) {
		in.SwitchSelected = "port1"
	})
	if got := antSwitchCmds(); len(got) != 2 {
		t.Fatalf("expected reassertion select after manual override, got %d cmds", len(got))
	}

	// 4. A frequency change within the same band should also reassert.
	c.update(func(in *reconcile.Inputs) {
		in.RadioFreqHz = 14_200_000
	})
	if got := antSwitchCmds(); len(got) != 3 {
		t.Fatalf("expected reassertion on frequency change after manual override, got %d cmds", len(got))
	}
}

// radioOnlineClient builds a Client backed by a recording fake and a running worker, plus a
// drain helper that waits for all queued handler jobs to complete (FIFO over c.jobs). Used
// by the RadioOnline gate tests below, which drive the real onRadioState/onRadioStatus
// handlers (the AND of /status bridge liveness and /state.device_online radio-link liveness)
// rather than setting in.RadioOnline directly.
func radioOnlineClient(t *testing.T) (c *Client, antSwitchCmds func() []recordedMsg, drain func(), cancel context.CancelFunc) {
	t.Helper()
	var (
		mu     sync.Mutex
		record []recordedMsg
	)
	cfg := config.Config{
		Location: "bauwagen", Host: "shari",
		MQTT: config.MQTT{Site: "muehle", Station: "hf", Slot: "antenna-select"},
		WiringMap: map[string]string{
			"port1": "dummy-load", "port3": "ultrabeam", "port6": "fan-dipole", "off": "grounded",
		},
		BandPolicy: config.BandPolicy{
			Bands:    map[string][]string{"ultrabeam": {"20m"}, "fan-dipole": {"40m"}},
			Fallback: "fan-dipole",
		},
	}
	fake := fakePaho{pub: func(topic string, qos byte, retained bool, payload any) paho.Token {
		b, _ := payload.([]byte)
		mu.Lock()
		record = append(record, recordedMsg{topic, qos, retained, b})
		mu.Unlock()
		return okToken{}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	c = &Client{
		client: fake, cfg: cfg, rec: reconcile.New(cfg),
		site: "muehle", station: "hf", slot: "antenna-select",
		jobs: make(chan func(), 256), ctx: ctx, cancel: cancel,
	}
	go sharedmqtt.RunJobs(ctx, c.jobs)
	antSwitchCmds = func() []recordedMsg {
		mu.Lock()
		defer mu.Unlock()
		var out []recordedMsg
		for _, m := range record {
			if m.topic == "muehle/hf/ant-switch/cmd" {
				out = append(out, m)
			}
		}
		return out
	}
	drain = func() {
		done := make(chan struct{})
		sharedmqtt.Enqueue(c.jobs, func() { close(done) })
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not drain queued handler jobs in time")
		}
	}
	return c, antSwitchCmds, drain, cancel
}

func radioStateMsg(deviceOnline bool, band string) fakeMessage {
	return fakeMessage{
		topic:   "muehle/hf/radio/state",
		payload: []byte(`{"band":"` + band + `","freq_hz":14000000,"tx":"rx","device_online":` + boolStr(deviceOnline) + `}`),
	}
}

func radioStatusMsg(online bool) fakeMessage {
	v := "offline"
	if online {
		v = "online"
	}
	return fakeMessage{topic: "muehle/hf/radio/status", payload: []byte(v)}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestRadioOnlineRequiresDeviceOnline is the regression guard for the antennaselect half of
// the flexbridge frequency-chatter audit. /status is the broker LWT (bridge process
// liveness); it stays online while the radio link is down. The old code keyed RadioOnline
// on /status alone, so a bridge-up-but-radio-down state (flexbridge reconnecting, publishing
// /state with device_online=false and band="") trusted the stale/empty radio state and
// chattered the antenna to the fallback. RadioOnline must be the AND of bridge liveness and
// device_online (radio-link liveness): only when both are up may radio-derived fields be
// trusted (§10).
func TestRadioOnlineRequiresDeviceOnline(t *testing.T) {
	t.Run("bridge_up_radio_down_holds", func(t *testing.T) {
		c, antSwitchCmds, drain, cancel := radioOnlineClient(t)
		defer cancel()
		// Bridge online, but the radio link is down (device_online=false), band=20m.
		// Must NOT select: hold the last selection.
		c.onRadioStatus(nil, radioStatusMsg(true))
		c.onRadioState(nil, radioStateMsg(false, "20m"))
		drain()
		if got := antSwitchCmds(); len(got) != 0 {
			t.Errorf("bridge up + radio down: expected no ant-switch select (hold), got %d: %v", len(got), got)
		}
	})

	t.Run("bridge_up_radio_up_selects", func(t *testing.T) {
		c, antSwitchCmds, drain, cancel := radioOnlineClient(t)
		defer cancel()
		c.onRadioStatus(nil, radioStatusMsg(true))
		c.onRadioState(nil, radioStateMsg(true, "20m"))
		drain()
		got := antSwitchCmds()
		if len(got) != 1 {
			t.Fatalf("bridge up + radio up: expected 1 ant-switch select (20m->port3), got %d", len(got))
		}
		var p struct {
			Select string `json:"select"`
		}
		if err := json.Unmarshal(got[0].payload, &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if p.Select != "port3" {
			t.Errorf("select=%q, want port3", p.Select)
		}
	})

	t.Run("bridge_down_radio_up_holds", func(t *testing.T) {
		c, antSwitchCmds, drain, cancel := radioOnlineClient(t)
		defer cancel()
		// Radio link is up but the bridge LWT says offline (frozen retained snapshot).
		// Must hold — /status is the freshness gate (§10).
		c.onRadioStatus(nil, radioStatusMsg(false))
		c.onRadioState(nil, radioStateMsg(true, "20m"))
		drain()
		if got := antSwitchCmds(); len(got) != 0 {
			t.Errorf("bridge down + radio up: expected no select (hold), got %d", len(got))
		}
	})

	t.Run("empty_band_holds_even_when_online", func(t *testing.T) {
		c, antSwitchCmds, drain, cancel := radioOnlineClient(t)
		defer cancel()
		// Both live, but band is empty (flexbridge reconnect Reset, no slice yet).
		// Must hold — empty band is transient, not a fallback intent.
		c.onRadioStatus(nil, radioStatusMsg(true))
		c.onRadioState(nil, radioStateMsg(true, ""))
		drain()
		if got := antSwitchCmds(); len(got) != 0 {
			t.Errorf("empty band while online: expected no select (hold), got %d", len(got))
		}
	})

	t.Run("state_before_status_settles", func(t *testing.T) {
		// Retained delivery order is not guaranteed: /state may arrive before /status.
		// The intermediate (device online, bridge unknown) must hold, and the final
		// state after /status=online must select exactly once.
		c, antSwitchCmds, drain, cancel := radioOnlineClient(t)
		defer cancel()
		c.onRadioState(nil, radioStateMsg(true, "20m"))
		drain()
		if got := antSwitchCmds(); len(got) != 0 {
			t.Fatalf("after /state alone: expected hold (bridge liveness unknown), got %d", len(got))
		}
		c.onRadioStatus(nil, radioStatusMsg(true))
		drain()
		if got := antSwitchCmds(); len(got) != 1 {
			t.Errorf("after /status=online: expected 1 select total, got %d", len(got))
		}
	})

	t.Run("radio_drop_after_up_releases_to_hold", func(t *testing.T) {
		// Online and selected; then the radio link drops (device_online=false) while the
		// bridge stays up. RadioOnline must go false → no new select, and a subsequent
		// band change must NOT chatter while the radio is down.
		c, antSwitchCmds, drain, cancel := radioOnlineClient(t)
		defer cancel()
		c.onRadioStatus(nil, radioStatusMsg(true))
		c.onRadioState(nil, radioStateMsg(true, "20m"))
		drain()
		if got := antSwitchCmds(); len(got) != 1 {
			t.Fatalf("setup: expected 1 select, got %d", len(got))
		}
		// Radio link drops; switch still on port3 (matches last target), band unchanged.
		c.onRadioState(nil, radioStateMsg(false, "20m"))
		drain()
		if got := antSwitchCmds(); len(got) != 1 {
			t.Errorf("after radio drop: expected no new select, got %d", len(got))
		}
	})
}

// --- idle timeout (walk-away safety, §10) tests -----------------------------

// idleClient builds a client wired like radioOnlineClient but with an explicit idle
// timeout and a controllable lastActivity, so the idle-timeout logic can be exercised
// deterministically.
func idleClient(t *testing.T, timeout time.Duration) (c *Client, antSwitchCmds func() []recordedMsg, drain func(), cancel context.CancelFunc) {
	t.Helper()
	var (
		mu     sync.Mutex
		record []recordedMsg
	)
	cfg := config.Config{
		Location: "bauwagen", Host: "shari",
		MQTT: config.MQTT{Site: "muehle", Station: "hf", Slot: "antenna-select"},
		WiringMap: map[string]string{
			"port1": "dummy-load", "port3": "ultrabeam", "port6": "fan-dipole", "off": "grounded",
		},
		BandPolicy: config.BandPolicy{
			Bands:    map[string][]string{"ultrabeam": {"20m"}, "fan-dipole": {"40m"}},
			Fallback: "fan-dipole",
		},
		Idle: config.Idle{TimeoutMinutes: int(timeout / time.Minute)},
	}
	fake := fakePaho{pub: func(topic string, qos byte, retained bool, payload any) paho.Token {
		b, _ := payload.([]byte)
		mu.Lock()
		record = append(record, recordedMsg{topic, qos, retained, b})
		mu.Unlock()
		return okToken{}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	c = &Client{
		client: fake, cfg: cfg, rec: reconcile.New(cfg),
		site: "muehle", station: "hf", slot: "antenna-select",
		jobs: make(chan func(), 256), ctx: ctx, cancel: cancel,
		lastActivity: time.Now(),
	}
	go sharedmqtt.RunJobs(ctx, c.jobs)
	antSwitchCmds = func() []recordedMsg {
		mu.Lock()
		defer mu.Unlock()
		var out []recordedMsg
		for _, m := range record {
			if m.topic == "muehle/hf/ant-switch/cmd" {
				out = append(out, m)
			}
		}
		return out
	}
	drain = func() {
		done := make(chan struct{})
		sharedmqtt.Enqueue(c.jobs, func() { close(done) })
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not drain queued handler jobs in time")
		}
	}
	return c, antSwitchCmds, drain, cancel
}

// radioStateMsgFreq is like radioStateMsg but with an explicit frequency, so a VFO change
// can be simulated.
func radioStateMsgFreq(deviceOnline bool, band string, freqHz int64) fakeMessage {
	return fakeMessage{
		topic:   "muehle/hf/radio/state",
		payload: []byte("{\"band\":\"" + band + "\",\"freq_hz\":" + strconv.FormatInt(freqHz, 10) + ",\"tx\":\"rx\",\"device_online\":" + boolStr(deviceOnline) + "}"),
	}
}

func TestIdleTimeoutGroundsAntenna(t *testing.T) {
	c, antSwitchCmds, drain, cancel := idleClient(t, time.Hour)
	defer cancel()
	// Radio online on 20m -> selects port3.
	c.onRadioStatus(nil, radioStatusMsg(true))
	c.onRadioState(nil, radioStateMsg(true, "20m"))
	drain()
	if got := antSwitchCmds(); len(got) != 1 {
		t.Fatalf("setup: expected 1 select, got %d", len(got))
	}
	// Simulate the idle timeout elapsing: lastActivity is now stale.
	c.lastActivity = time.Now().Add(-2 * time.Hour)
	c.checkIdle()
	drain()
	cmds := antSwitchCmds()
	if len(cmds) != 2 {
		t.Fatalf("expected 2 selects (port3 then off), got %d", len(cmds))
	}
	var p struct {
		Select string `json:"select"`
	}
	if err := json.Unmarshal(cmds[len(cmds)-1].payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Select != "off" {
		t.Errorf("last select=%q, want off", p.Select)
	}
	if c.in.StationActivity != "inactive" {
		t.Errorf("StationActivity=%q, want inactive", c.in.StationActivity)
	}
}

func TestIdleTimeoutNotElapsedKeepsActive(t *testing.T) {
	c, _, drain, cancel := idleClient(t, time.Hour)
	defer cancel()
	c.onRadioStatus(nil, radioStatusMsg(true))
	c.onRadioState(nil, radioStateMsg(true, "20m"))
	drain()
	// lastActivity is recent (set by the activity detection above), so checkIdle must
	// NOT mark inactive.
	c.checkIdle()
	drain()
	if c.in.StationActivity == "inactive" {
		t.Errorf("StationActivity became inactive despite recent activity")
	}
}

func TestVFOChangeMarksActive(t *testing.T) {
	c, _, drain, cancel := idleClient(t, time.Hour)
	defer cancel()
	c.onRadioStatus(nil, radioStatusMsg(true))
	c.onRadioState(nil, radioStateMsg(true, "20m"))
	drain()
	// Force idle.
	c.lastActivity = time.Now().Add(-2 * time.Hour)
	c.checkIdle()
	drain()
	if c.in.StationActivity != "inactive" {
		t.Fatalf("setup: expected inactive, got %q", c.in.StationActivity)
	}
	// A VFO change (14.0 -> 14.1 MHz) marks active again.
	c.onRadioState(nil, radioStateMsgFreq(true, "20m", 14100000))
	drain()
	if c.in.StationActivity != "active" {
		t.Errorf("StationActivity=%q, want active after VFO change", c.in.StationActivity)
	}
}

func TestTXMarksActive(t *testing.T) {
	c, _, drain, cancel := idleClient(t, time.Hour)
	defer cancel()
	c.onRadioStatus(nil, radioStatusMsg(true))
	c.onRadioState(nil, radioStateMsg(true, "20m"))
	drain()
	c.lastActivity = time.Now().Add(-2 * time.Hour)
	c.checkIdle()
	drain()
	if c.in.StationActivity != "inactive" {
		t.Fatalf("setup: expected inactive, got %q", c.in.StationActivity)
	}
	// A transmit (tx="tx") marks active again, even with the same frequency.
	c.onRadioState(nil, fakeMessage{
		topic:   "muehle/hf/radio/state",
		payload: []byte("{\"band\":\"20m\",\"freq_hz\":14000000,\"tx\":\"tx\",\"device_online\":true}"),
	})
	drain()
	if c.in.StationActivity != "active" {
		t.Errorf("StationActivity=%q, want active after TX", c.in.StationActivity)
	}
}

func TestActivityAfterIdleReselects(t *testing.T) {
	c, antSwitchCmds, drain, cancel := idleClient(t, time.Hour)
	defer cancel()
	c.onRadioStatus(nil, radioStatusMsg(true))
	c.onRadioState(nil, radioStateMsg(true, "20m"))
	drain()
	// Idle -> off.
	c.lastActivity = time.Now().Add(-2 * time.Hour)
	c.checkIdle()
	drain()
	// Activity (VFO change) -> re-select port3.
	c.onRadioState(nil, radioStateMsgFreq(true, "20m", 14100000))
	drain()
	cmds := antSwitchCmds()
	if len(cmds) < 3 {
		t.Fatalf("expected at least 3 selects (port3, off, port3), got %d", len(cmds))
	}
	var p struct {
		Select string `json:"select"`
	}
	if err := json.Unmarshal(cmds[len(cmds)-1].payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Select != "port3" {
		t.Errorf("last select=%q, want port3", p.Select)
	}
}

// operatorHoldMsg builds an operator /cmd message like a console or HA would
// publish to muehle/hf/antenna-select/cmd.
func operatorHoldMsg(request string) fakeMessage {
	return fakeMessage{
		topic:   "muehle/hf/antenna-select/cmd",
		payload: []byte(`{"request":"` + request + `"}`),
	}
}

// TestOperatorHoldMarksActiveAndReselects: after the idle timeout grounds the
// antenna, an operator hold must work as a manual re-arm — even with the radio
// link down, where previously only a radio/state change could mark activity
// and Tier 1 (idle) silently overrode every operator command. The hold is
// evidence of presence; a later idle timeout re-grounds (walk-away safety).
func TestOperatorHoldMarksActiveAndReselects(t *testing.T) {
	c, antSwitchCmds, drain, cancel := idleClient(t, time.Hour)
	defer cancel()
	// Ground first: radio on 20m selects port3, then the idle timeout fires.
	c.onRadioStatus(nil, radioStatusMsg(true))
	c.onRadioState(nil, radioStateMsg(true, "20m"))
	drain()
	c.lastActivity = time.Now().Add(-2 * time.Hour)
	c.checkIdle()
	drain()
	if got := antSwitchCmds(); len(got) != 2 {
		t.Fatalf("setup: expected 2 selects (port3, off), got %d", len(got))
	}
	// Radio bridge drops — the only other activity source is now dead.
	c.onRadioStatus(nil, radioStatusMsg(false))
	drain()

	// The operator asks for port6. Tier 2 (operator) must win over Tier 1.
	c.onOperatorCmd(nil, operatorHoldMsg("port6"))
	drain()
	if c.in.StationActivity != "active" {
		t.Errorf("StationActivity=%q, want active after operator hold", c.in.StationActivity)
	}
	if c.in.OperatorRequest != "port6" {
		t.Errorf("OperatorRequest=%q, want port6", c.in.OperatorRequest)
	}
	if !c.lastActivity.After(time.Now().Add(-time.Minute)) {
		t.Error("operator hold did not reset lastActivity (idle clock)")
	}
	cmds := antSwitchCmds()
	if len(cmds) != 3 {
		t.Fatalf("expected 3rd select (port6) from the hold, got %d", len(cmds))
	}
	var p struct {
		Select string `json:"select"`
	}
	if err := json.Unmarshal(cmds[len(cmds)-1].payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Select != "port6" {
		t.Errorf("select=%q, want port6", p.Select)
	}

	// The hold also re-arms the idle clock: an immediate checkIdle must not
	// re-ground (walk-away timeout restarts from the hold).
	c.checkIdle()
	drain()
	if c.in.StationActivity != "active" {
		t.Errorf("checkIdle marked inactive right after an operator hold")
	}
	if got := antSwitchCmds(); len(got) != 3 {
		t.Errorf("checkIdle after hold emitted extra selects (grounding): %d", len(got))
	}
}

// TestOperatorReleaseDoesNotMarkActive: the "auto" release withdraws the hold
// but is not evidence of presence — it must not reset the idle clock or
// re-activate a grounded station.
func TestOperatorReleaseDoesNotMarkActive(t *testing.T) {
	c, antSwitchCmds, drain, cancel := idleClient(t, time.Hour)
	defer cancel()
	c.onRadioStatus(nil, radioStatusMsg(true))
	c.onRadioState(nil, radioStateMsg(true, "20m"))
	drain()
	c.lastActivity = time.Now().Add(-2 * time.Hour)
	c.checkIdle()
	drain()
	if c.in.StationActivity != "inactive" {
		t.Fatalf("setup: expected inactive, got %q", c.in.StationActivity)
	}
	c.onOperatorCmd(nil, operatorHoldMsg("auto"))
	drain()
	// "auto" is stored verbatim (pre-existing behavior); holdActive treats it
	// as no hold, which is what the decision then reflects.
	if hold := c.in.OperatorRequest != "" && c.in.OperatorRequest != "auto"; hold {
		t.Errorf("OperatorRequest=%q, want \"auto\" (a release) after \"auto\" cmd", c.in.OperatorRequest)
	}
	if c.in.StationActivity != "inactive" {
		t.Errorf("StationActivity=%q, want still inactive after release", c.in.StationActivity)
	}
	if !c.lastActivity.Before(time.Now().Add(-time.Hour)) {
		t.Error("release reset lastActivity (should be untouched)")
	}
	if got := antSwitchCmds(); len(got) != 2 {
		t.Errorf("release emitted extra selects, got %d want 2", len(got))
	}
}
