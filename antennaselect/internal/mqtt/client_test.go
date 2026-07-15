package mqtt

import (
	"context"
	"encoding/json"
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
