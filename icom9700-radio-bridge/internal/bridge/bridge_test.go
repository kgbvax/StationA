package bridge

// Bridge slot tests (plan U5): the four-plane surface driven end-to-end over
// the fake IC-9700 (real loopback UDP, the civ package's FakeRadio) with a
// recording paho fake standing in for the broker — the spid-ercm mqttslot
// test shape. Every policy figure (session retries, transport cadences,
// poll tick) is shrunk for test speed; production figures come from config
// at wiring time.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"icom9700-radio-bridge/internal/civ"
	"icom9700-radio-bridge/internal/radio"
)

// ---------------------------------------------------------------------------
// paho fake (the spid slot_test pattern: record Publish/Subscribe, instant
// tokens; the unimplemented paho.Client surface stays nil and is never hit)
// ---------------------------------------------------------------------------

type recPub struct {
	topic    string
	qos      byte
	retained bool
	payload  string
}

type instantToken struct{ err error }

func (t instantToken) Wait() bool                     { return true }
func (t instantToken) WaitTimeout(time.Duration) bool { return t.err == nil }
func (t instantToken) Done() <-chan struct{}          { return make(chan struct{}) }
func (t instantToken) Error() error                   { return t.err }

func (m fakeMsg) Duplicate() bool   { return false }
func (m fakeMsg) Qos() byte         { return 0 }
func (m fakeMsg) Retained() bool    { return false }
func (m fakeMsg) Topic() string     { return m.topic }
func (m fakeMsg) MessageID() uint16 { return 0 }
func (m fakeMsg) Payload() []byte   { return m.payload }
func (m fakeMsg) Ack()              {}

type fakeMsg struct {
	topic   string
	payload []byte
}

type recordingPaho struct {
	paho.Client
	mu   sync.Mutex
	pubs []recPub
	open bool
}

func (f *recordingPaho) Publish(topic string, qos byte, retained bool, payload any) paho.Token {
	b, ok := payload.([]byte)
	if !ok {
		b = []byte(fmt.Sprintf("%v", payload))
	}
	f.mu.Lock()
	f.pubs = append(f.pubs, recPub{topic: topic, qos: qos, retained: retained, payload: string(b)})
	f.mu.Unlock()
	return instantToken{}
}

func (f *recordingPaho) Subscribe(topic string, qos byte, _ paho.MessageHandler) paho.Token {
	f.mu.Lock()
	_ = topic
	_ = qos
	f.mu.Unlock()
	return instantToken{}
}

func (f *recordingPaho) IsConnectionOpen() bool { return true }

func (f *recordingPaho) recorded() []recPub {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recPub(nil), f.pubs...)
}

// ---------------------------------------------------------------------------
// the scripted IC-9700: a small radio model behind the fake's CI-V responder.
// respond() runs on the fake's serve goroutine WITH fr.mu held — it takes
// only the model's own mutex and returns reply payloads (the fake injects
// them); test-side mutations go through the Set*/Key* methods, which take
// model.mu but never call fake-radio methods (no lock nesting).
// ---------------------------------------------------------------------------

type radioModel struct {
	mu     sync.Mutex
	freq   [2]uint32
	mode   [2]byte // operating-mode byte (civ.ModeByte*)
	data   [2]bool
	sel    byte // 0 = main, 1 = sub
	sat    bool
	ptt    bool
	preamp byte
	att    byte // 0x00 off, 0x10 on
	power  byte
	sMeter byte
	swr    byte
	alc    byte
	comp   byte
}

// mustFakeRadio binds the fake radio wired into t (the session_test helper,
// duplicated here — different package).
func mustFakeRadio(t *testing.T) *civ.FakeRadio {
	t.Helper()
	fr, err := civ.NewFakeRadio()
	if err != nil {
		t.Fatalf("fake radio bind: %v", err)
	}
	t.Cleanup(fr.Close)
	return fr
}

// cmdCleared reports whether the retained /cmd was cleared (empty retained
// payload — the one-shot posture).
func cmdCleared(fake *recordingPaho, topic string) bool {
	for _, p := range fake.recorded() {
		if p.topic == topic && p.payload == "" && p.retained {
			return true
		}
	}
	return false
}

func newRadioModel() *radioModel {
	return &radioModel{
		freq:  [2]uint32{433_400_000, 145_200_000},
		mode:  [2]byte{civ.ModeByteFM, civ.ModeByteFM},
		power: 100,
	}
}

// attach scripts the model as the fake's CI-V responder.
func (m *radioModel) attach(fr *civ.FakeRadio) {
	fr.RespondCIV(m.respond)
}

func (m *radioModel) respond(frame []byte) [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(frame) < 6 || frame[len(frame)-1] != civ.Postamble {
		return nil
	}
	cmd := frame[4]
	body := frame[5 : len(frame)-1]

	reply := func(cmd byte, data ...byte) []byte {
		out := []byte{civ.Preamble, civ.Preamble, civ.AddrCtrl, civ.AddrRadio, cmd}
		out = append(out, data...)
		return append(out, civ.Postamble)
	}
	bcd := func(hz uint32) []byte {
		b := make([]byte, 5)
		for i := 0; i < 5; i++ {
			d0 := (hz / pow10t(2*i)) % 10
			d1 := (hz / pow10t(2*i+1)) % 10
			b[i] = byte(d1<<4 | d0)
		}
		return b
	}

	switch cmd {
	case civ.CmdFreqTransceive: // 00: not client-sent
		return nil
	case civ.CmdReadFreq: // 03
		return [][]byte{reply(cmd, bcd(m.freq[m.sel])...)}
	case civ.CmdReadMode: // 04
		return [][]byte{reply(cmd, m.mode[m.sel], 0x01)}
	case civ.CmdSetFreq: // 05 <5 bcd>
		if len(body) == 5 {
			m.freq[m.sel] = bcdDecode(body)
			// The real radio transceives the change back (CI-V Transceive ON).
			return [][]byte{
				reply(civ.ReplyOK),
				reply(civ.CmdFreqTransceive, bcd(m.freq[m.sel])...),
			}
		}
	case civ.CmdSetMode: // 06 <mode> [<filter>]
		if len(body) >= 1 {
			m.mode[m.sel] = body[0]
			return [][]byte{
				reply(civ.ReplyOK),
				reply(civ.CmdModeTransceive, m.mode[m.sel], 0x01),
			}
		}
	case civ.CmdSelectBand: // 07 D0/D1 select (len 1), 07 D2 00 read (len 2)
		if len(body) == 2 && body[0] == civ.BandSelRead {
			return [][]byte{reply(cmd, body[0], m.sel)}
		}
		if len(body) == 1 && (body[0] == civ.BandSelMain || body[0] == civ.BandSelSub) {
			m.sel = body[0] - civ.BandSelMain
			return [][]byte{reply(civ.ReplyOK)}
		}
	case civ.CmdAttenuator: // 11 00/10
		if len(body) == 1 {
			m.att = body[0]
			return [][]byte{reply(cmd, body[0])}
		}
	case civ.CmdRFPower: // 14 0A: set is 14 0A <lvl> (len 2), read is 14 0A (len 1)
		if len(body) == 2 {
			m.power = body[1]
			return [][]byte{reply(civ.ReplyOK)}
		}
		if len(body) == 1 {
			return [][]byte{reply(cmd, civ.PowerRF, m.power)}
		}
	case civ.CmdReadMeter: // 15 xx
		if len(body) == 1 {
			switch body[0] {
			case civ.MeterSubS:
				return [][]byte{reply(cmd, body[0], m.sMeter)}
			case civ.MeterSubSWR:
				return [][]byte{reply(cmd, body[0], m.swr)}
			case civ.MeterSubALC:
				return [][]byte{reply(cmd, body[0], m.alc)}
			case civ.MeterSubCOMP:
				return [][]byte{reply(cmd, body[0], m.comp)}
			}
		}
	case civ.CmdFunction: // 16 02 preamp, 16 5A satellite
		if len(body) == 1 && body[0] == civ.FuncPreamp {
			return [][]byte{reply(cmd, body[0], m.preamp)}
		}
		if len(body) == 1 && body[0] == civ.FuncSatellite {
			v := byte(0)
			if m.sat {
				v = 1
			}
			return [][]byte{reply(cmd, body[0], v)}
		}
		if len(body) == 2 && body[0] == civ.FuncSatellite {
			m.sat = body[1] == 1
			return [][]byte{reply(civ.ReplyOK)}
		}
		if len(body) == 2 && body[0] == civ.FuncPreamp {
			m.preamp = body[1]
			return [][]byte{reply(civ.ReplyOK)}
		}
	case civ.CmdSetItem: // 1A 06 data mode
		if len(body) == 3 && body[0] == civ.SetItemDataMode {
			m.data[m.sel] = body[1] == 1
			return [][]byte{reply(civ.ReplyOK)}
		}
		if len(body) == 1 && body[0] == civ.SetItemDataMode {
			v := byte(0)
			if m.data[m.sel] {
				v = 1
			}
			f := byte(1)
			if m.data[m.sel] {
				f = 3
			}
			return [][]byte{reply(cmd, body[0], v, f)}
		}
	case civ.CmdStatus: // 1C 00: set is 1C 00 <v> (len 2), read is 1C 00 (len 1)
		if len(body) == 2 {
			m.ptt = body[1] == 1
			return [][]byte{reply(civ.ReplyOK)}
		}
		if len(body) == 1 {
			v := byte(0)
			if m.ptt {
				v = 1
			}
			// The read reply shares the transceive shape (transceive.go).
			return [][]byte{reply(cmd, civ.StatusSubPTT, v)}
		}
	}
	return nil
}

// --- test-side model mutations ---------------------------------------------

func (m *radioModel) setSel(sel byte) { m.mu.Lock(); m.sel = sel; m.mu.Unlock() }
func (m *radioModel) setSat(on bool)  { m.mu.Lock(); m.sat = on; m.mu.Unlock() }
func (m *radioModel) setMeter(b byte) { m.mu.Lock(); m.sMeter = b; m.mu.Unlock() }
func (m *radioModel) setFreqAt(vfo int, hz uint32) {
	m.mu.Lock()
	m.freq[vfo] = hz
	m.mu.Unlock()
}

func pow10t(n int) uint32 {
	p := uint32(1)
	for i := 0; i < n; i++ {
		p *= 10
	}
	return p
}

func bcdDecode(b []byte) uint32 {
	var hz uint64
	for i := 0; i < 5; i++ {
		hz += uint64(b[i]&0x0f) * uint64(pow10t(2*i))
		hz += uint64(b[i]>>4) * uint64(pow10t(2*i+1))
	}
	return uint32(hz)
}

// --- transceive broadcast builders (radio-initiated change simulation) -----

func freqTransFrame(hz uint32) []byte {
	out := []byte{civ.Preamble, civ.Preamble, civ.AddrCtrl, civ.AddrRadio, civ.CmdFreqTransceive}
	for i := 0; i < 5; i++ {
		d0 := byte((hz / pow10t(2*i)) % 10)
		d1 := byte((hz / pow10t(2*i+1)) % 10)
		out = append(out, d1<<4|d0)
	}
	return append(out, civ.Postamble)
}

func pttTransFrame(on bool) []byte {
	v := byte(0)
	if on {
		v = 1
	}
	return []byte{civ.Preamble, civ.Preamble, civ.AddrCtrl, civ.AddrRadio, civ.CmdStatus, civ.StatusSubPTT, v, civ.Postamble}
}

// ---------------------------------------------------------------------------
// bridge fixture
// ---------------------------------------------------------------------------

// testOpts shrinks every cadence for test speed (the session_test pattern).
func testOpts(fr *civ.FakeRadio) Options {
	return Options{
		Site:     "tsite",
		Station:  "tstation",
		Slot:     "tradio",
		Location: "tlocation",

		PollInterval: 25 * time.Millisecond,
		ArmTimeout:   3 * time.Second,

		RadioHost:      "127.0.0.1",
		ControlPort:    fr.CtrlPort(),
		Username:       "operator1",
		Password:       "s3cret!",
		IdleTimeout:    10 * time.Second, // no mid-test idle drop
		MaxAttempts:    3,
		AttemptSpacing: 30 * time.Millisecond,
		ErrorDecay:     120 * time.Millisecond,
		TransportOpts: func(o *civ.Opts) {
			o.AreYouTherePeriod = 20 * time.Millisecond
			o.HandshakeTimeout = 600 * time.Millisecond
			o.IdlePeriod = 10 * time.Millisecond
			o.PingPeriod = 40 * time.Millisecond
			o.RetransmitPeriod = 10 * time.Millisecond
			o.TokenRenewal = 200 * time.Millisecond
			o.RenewalTimeout = 300 * time.Millisecond
			o.CivSilence = 300 * time.Millisecond
			o.StartDataPeriod = 30 * time.Millisecond
			o.SessionTimeout = 400 * time.Millisecond
		},
	}
}

// newTestBridge wires the bridge over the fake radio with the recording
// paho fake attached (the OnMQTTConnect ritual included). The radio model
// answers reads with a populated two-VFO world.
func newTestBridge(t *testing.T, fr *civ.FakeRadio, model *radioModel) (*Bridge, *recordingPaho) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	opts := testOpts(fr)
	b := New(ctx, opts)
	t.Cleanup(b.Close)
	// The telemetry ticker, exactly as main.go's pollLoop drives it.
	go func() {
		tick := time.NewTicker(opts.PollInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				b.Poll(ctx)
			}
		}
	}()
	fake := &recordingPaho{}
	b.OnMQTTConnect(fake)
	if model != nil {
		model.attach(fr)
	}
	return b, fake
}

// runOnWorker runs f on the bridge's jobs worker and waits for it — the
// deterministic way to drive Execute from a test (production funnels it the
// same way through the paho handler).
func runOnWorker(b *Bridge, f func()) {
	done := make(chan struct{})
	b.Jobs() <- func() { f(); close(done) }
	<-done
}

// deliverCmd publishes one /cmd payload through the real path.
func deliverCmd(b *Bridge, payload string) {
	runOnWorker(b, func() {
		_ = b.Execute(context.Background(), []byte(payload))
	})
}

// statePubs returns every recorded /state publish as generic maps.
func statePubs(t *testing.T, fake *recordingPaho, topic string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, p := range fake.recorded() {
		if p.topic != topic {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(p.payload), &m); err != nil {
			t.Fatalf("state payload not JSON: %v (%q)", err, p.payload)
		}
		out = append(out, m)
	}
	return out
}

// lastState returns the most recent /state publish.
func lastState(t *testing.T, fake *recordingPaho, topic string) map[string]any {
	t.Helper()
	pubs := statePubs(t, fake, topic)
	if len(pubs) == 0 {
		t.Fatalf("no /state publishes on %s", topic)
	}
	return pubs[len(pubs)-1]
}

// tryState returns the most recent /state publish, ok=false before the first
// one landed (for waitFor predicates — they must not Fatal).
func tryState(fake *recordingPaho, topic string) (map[string]any, bool) {
	pubs := statePubsNoFatal(fake, topic)
	if len(pubs) == 0 {
		return nil, false
	}
	return pubs[len(pubs)-1], true
}

func statePubsNoFatal(fake *recordingPaho, topic string) []map[string]any {
	var out []map[string]any
	for _, p := range fake.recorded() {
		if p.topic != topic {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(p.payload), &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// waitFor polls fn until true or the timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, what)
}

// waitLive waits for the session to reach live over the fake radio.
func waitLive(t *testing.T, b *Bridge) {
	t.Helper()
	waitFor(t, 3*time.Second, "session live", func() bool {
		return b.sess.SessionState() == radio.StateLive
	})
}

// waitBothVFOsKnown waits until the first full two-VFO reconcile landed (the
// sub detail object appears). Until then the sub cache is legitimately
// omitted from /state (radio-measured fields publish only once read, R6).
func waitBothVFOsKnown(t *testing.T, fake *recordingPaho, st string) {
	t.Helper()
	waitFor(t, 3*time.Second, "sub VFO populated by the full reconcile", func() bool {
		m := lastState(t, fake, st)
		_, ok := m["sub"].(map[string]any)
		return ok
	})
}

// lastError returns the error field of the latest /state ("" when absent).
func lastError(t *testing.T, fake *recordingPaho, topic string) string {
	m := lastState(t, fake, topic)
	e, _ := m["error"].(string)
	return e
}

// hasKey reports whether the latest /state carries the key at all.
func hasKey(t *testing.T, fake *recordingPaho, topic, key string) bool {
	m := lastState(t, fake, topic)
	_, ok := m[key]
	return ok
}

// frameSent reports whether the radio received a frame with the exact bytes.
func frameSent(fr *civ.FakeRadio, want []byte) bool {
	for _, f := range fr.CivFramesReceived() {
		if string(f) == string(want) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// /meta birth certificate (R8)
// ---------------------------------------------------------------------------

func TestMetaPublishedAtStartupReadOnly(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	meta := topicMeta(b.opts)

	var pub recPub
	waitFor(t, 2*time.Second, "meta publish", func() bool {
		for _, p := range fake.recorded() {
			if p.topic == meta {
				pub = p
				return true
			}
		}
		return false
	})
	if !pub.retained {
		t.Error("/meta must be retained")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(pub.payload), &m); err != nil {
		t.Fatalf("meta not JSON: %v", err)
	}
	if m["schema"] != "1.0" || m["role"] != "radio" {
		t.Errorf("schema/role = %v/%v, want 1.0/radio", m["schema"], m["role"])
	}
	if m["location"] != "tlocation" {
		t.Errorf("location = %v, want the configured value", m["location"])
	}
	dev, _ := m["device"].(map[string]any)
	if dev["model"] != deviceModel {
		t.Errorf("device.model = %v, want %q (startup publish, no firmware read in v1)", dev["model"], deviceModel)
	}
	if _, has := dev["firmware"]; has {
		t.Error("device.firmware must be absent in v1 (no identity read; never clobber /meta)")
	}
	caps, _ := m["capabilities"].(map[string]any)
	for _, k := range []string{"bands", "modes", "bias_t", "satellite", "vfos"} {
		if _, has := caps[k]; !has {
			t.Errorf("capabilities missing %q: %v", k, caps)
		}
	}
	if bands, _ := caps["bands"].([]any); fmt.Sprint(bands) != "[2m 70cm 23cm]" {
		t.Errorf("capabilities.bands = %v, want [2m 70cm 23cm]", bands)
	}
	if vfos, _ := caps["vfos"].([]any); fmt.Sprint(vfos) != "[main sub]" {
		t.Errorf("capabilities.vfos = %v, want [main sub]", vfos)
	}
	if caps["bias_t"] != true || caps["satellite"] != true {
		t.Errorf("bias_t/satellite = %v/%v, want true/true", caps["bias_t"], caps["satellite"])
	}

	// Read-only expose: state fields only — no actions, no writable fields,
	// no command widgets, no PTT/arm keys (the sat-rotator posture).
	expose, _ := m["expose"].(map[string]any)
	fields, _ := expose["fields"].([]any)
	if len(fields) == 0 {
		t.Fatal("expose.fields empty")
	}
	gotKeys := map[string]map[string]any{}
	for _, f := range fields {
		fm := f.(map[string]any)
		key, _ := fm["key"].(string)
		gotKeys[key] = fm
		if w, _ := fm["writable"].(bool); w {
			t.Errorf("expose field %q writable — read-only expose (R8)", key)
		}
		if fm["command"] != nil {
			t.Errorf("expose field %q carries a command — read-only expose (R8)", key)
		}
	}
	for _, key := range []string{"freq_hz", "band", "mode", "tx", "selected_vfo", "satellite",
		"session_state", "device_online", "armed", "s_meter", "swr", "alc", "tx_power", "error"} {
		if _, has := gotKeys[key]; !has {
			t.Errorf("expose.fields missing %q", key)
		}
	}
	if gotKeys["mode"]["options_ref"] != "modes" {
		t.Errorf("mode options_ref = %v, want modes", gotKeys["mode"]["options_ref"])
	}
	if gotKeys["band"]["options_ref"] != "bands" {
		t.Errorf("band options_ref = %v, want bands", gotKeys["band"]["options_ref"])
	}
	tx := gotKeys["tx"]
	if tx["type"] != "boolean" || tx["on"] != "tx" || tx["off"] != "rx" {
		t.Errorf("tx expose = %v, want boolean on=tx off=rx (the flexbridge shape)", tx)
	}
	if _, has := expose["actions"]; has {
		t.Error("expose.actions present — read-only expose declares no actions (R8)")
	}
	for _, key := range []string{"ptt", "arm", "disarm"} {
		if _, has := gotKeys[key]; has {
			t.Errorf("expose field %q present — no PTT/arm widgets in v1 (R8)", key)
		}
	}
}

// ---------------------------------------------------------------------------
// initial /state and the per-state payload rules (R6)
// ---------------------------------------------------------------------------

func TestInitialStateIdleOmitsRadioFields(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newTestBridge(t, fr, newRadioModel())
	st := topicState(b.opts)

	var m map[string]any
	waitFor(t, 2*time.Second, "initial state publish", func() bool {
		pubs := statePubs(t, fake, st)
		if len(pubs) == 0 {
			return false
		}
		m = pubs[0]
		return true
	})
	if m["session_state"] != "idle" {
		t.Errorf("session_state = %v, want idle", m["session_state"])
	}
	if m["device_online"] != false {
		t.Errorf("device_online = %v, want false at healthy idle (R16)", m["device_online"])
	}
	if m["armed"] != false {
		t.Errorf("armed = %v, want false", m["armed"])
	}
	if m["selected_vfo"] != "main" {
		t.Errorf("selected_vfo = %v, want main (bridge-held default)", m["selected_vfo"])
	}
	if _, ok := m["ts"].(string); !ok || m["ts"] == "" {
		t.Errorf("ts = %v, want an RFC3339 stamp", m["ts"])
	}
	for _, key := range []string{"freq_hz", "band", "mode", "tx", "main", "sub",
		"satellite", "s_meter", "swr", "alc", "tx_power", "error"} {
		if _, has := m[key]; has {
			t.Errorf("idle /state carries radio-measured field %q — must be omitted (R6)", key)
		}
	}
}

// ---------------------------------------------------------------------------
// happy path: set_freq over the fake radio
// ---------------------------------------------------------------------------

func TestSetFreqMainUpdatesState(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"set_freq","vfo":"main","value":"432100000"}`)
	waitLive(t, b)

	waitFor(t, 3*time.Second, "state freq_hz = 432100000", func() bool {
		m := lastState(t, fake, st)
		f, _ := m["freq_hz"].(float64)
		return f == 432100000
	})
	m := lastState(t, fake, st)
	if m["band"] != "70cm" {
		t.Errorf("band = %v, want 70cm (derived)", m["band"])
	}
	if m["device_online"] != true {
		t.Errorf("device_online = %v, want true while live", m["device_online"])
	}
	if m["session_state"] != "live" {
		t.Errorf("session_state = %v, want live", m["session_state"])
	}
	if m["tx"] != "rx" {
		t.Errorf("tx = %v, want rx (the string enum)", m["tx"])
	}
	main, _ := m["main"].(map[string]any)
	if main == nil || main["freq_hz"] != float64(432100000) {
		t.Errorf("main detail = %v, want freq_hz 432100000", m["main"])
	}
	// The set frame really went out (05 + BCD).
	set, err := b.codec.BuildSetFreq(civ.VfoMain, 432100000)
	if err != nil {
		t.Fatalf("BuildSetFreq: %v", err)
	}
	if !frameSent(fr, set) {
		t.Error("radio never received the 05 set-frequency frame")
	}
	// The retained /cmd is cleared on the SUCCESS path too (KTD-6 one-shot).
	waitFor(t, 2*time.Second, "retained cmd cleared after successful admit", func() bool {
		return cmdCleared(fake, topicCmd(b.opts))
	})
}

// ---------------------------------------------------------------------------
// PTT (R10): exact rejections, then the live arm-gated key-up
// ---------------------------------------------------------------------------

func TestPTTUnarmedRejectedWithExactStringNoRadioTraffic(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newTestBridge(t, fr, newRadioModel())
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"ptt","value":"on"}`)
	if got := lastError(t, fake, st); got != ErrPTTNotArmed {
		t.Fatalf("error = %q, want exactly %q", got, ErrPTTNotArmed)
	}
	if len(fr.LoginAttempts()) != 0 {
		t.Errorf("rejected ptt drove %d login attempts — must gate before any radio traffic", len(fr.LoginAttempts()))
	}
	for _, f := range fr.CivFramesReceived() {
		if len(f) >= 5 && f[4] == civ.CmdStatus {
			t.Fatalf("PTT frame reached the radio: % x", f)
		}
	}
	// The retained cmd still cleared (one-shot, execute-or-reject).
	waitFor(t, time.Second, "cmd cleared", func() bool { return cmdCleared(fake, topicCmd(b.opts)) })
}

func TestPTTLiveSendsFrameAndReflectsTX(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitFor(t, time.Second, "armed true", func() bool {
		return lastState(t, fake, st)["armed"] == true
	})

	deliverCmd(b, `{"action":"ptt","value":"on"}`)
	on := b.codec.BuildPTT(true)
	waitFor(t, 2*time.Second, "PTT-on frame at the radio", func() bool {
		return frameSent(fr, on)
	})
	waitFor(t, 2*time.Second, "state tx = tx", func() bool {
		return lastState(t, fake, st)["tx"] == "tx"
	})

	deliverCmd(b, `{"action":"ptt","value":"off"}`)
	off := b.codec.BuildPTT(false)
	waitFor(t, 2*time.Second, "PTT-off frame at the radio", func() bool {
		return frameSent(fr, off)
	})
	waitFor(t, 2*time.Second, "state tx = rx", func() bool {
		return lastState(t, fake, st)["tx"] == "rx"
	})
}

// ---------------------------------------------------------------------------
// satellite mode (R10 gates + the SUB-uplink mirror)
// ---------------------------------------------------------------------------

func TestSatModeRejectionsAndToggle(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	// Live first, then key PTT: sat_mode while tx is on must reject.
	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitBothVFOsKnown(t, fake, st)
	deliverCmd(b, `{"action":"ptt","value":"on"}`)
	waitFor(t, 2*time.Second, "tx on", func() bool {
		return lastState(t, fake, st)["tx"] == "tx"
	})
	deliverCmd(b, `{"action":"sat_mode"}`)
	if got := lastError(t, fake, st); got != ErrSatModeTX {
		t.Fatalf("error = %q, want exactly %q", got, ErrSatModeTX)
	}

	// PTT off, but still armed: sat_mode rejects on the permit.
	deliverCmd(b, `{"action":"ptt","value":"off"}`)
	waitFor(t, 2*time.Second, "tx off", func() bool {
		return lastState(t, fake, st)["tx"] == "rx"
	})
	deliverCmd(b, `{"action":"sat_mode"}`)
	if got := lastError(t, fake, st); got != ErrSatModeArm {
		t.Fatalf("error = %q, want exactly %q", got, ErrSatModeArm)
	}

	// Disarm, then the toggle lands: state reflects it, the frames went out.
	deliverCmd(b, `{"action":"disarm"}`)
	waitFor(t, time.Second, "disarmed", func() bool {
		return lastState(t, fake, st)["armed"] == false
	})
	deliverCmd(b, `{"action":"sat_mode"}`)
	waitFor(t, 2*time.Second, "satellite true in state", func() bool {
		return lastState(t, fake, st)["satellite"] == true
	})
	if !frameSent(fr, b.codec.BuildSetSatellite(true)) {
		t.Error("radio never received the 16 5A satellite-on frame")
	}
	// Top-level mirror flips to SUB while satellite mode is on.
	m := lastState(t, fake, st)
	sub, _ := m["sub"].(map[string]any)
	if sub == nil || m["freq_hz"] != sub["freq_hz"] {
		t.Errorf("satellite-mode top-level = %v, must mirror SUB %v (uplink)", m["freq_hz"], sub["freq_hz"])
	}
}

func TestSetFreqMainDuringSatelliteMirrorsSub(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitBothVFOsKnown(t, fake, st)
	deliverCmd(b, `{"action":"disarm"}`)
	deliverCmd(b, `{"action":"sat_mode"}`)
	waitFor(t, 2*time.Second, "satellite on", func() bool {
		return lastState(t, fake, st)["satellite"] == true
	})

	// Tune MAIN (the downlink) while satellite mode routes the mirror to SUB.
	deliverCmd(b, `{"action":"set_freq","vfo":"main","value":"145800000"}`)
	waitFor(t, 3*time.Second, "main retuned", func() bool {
		m := lastState(t, fake, st)
		main, _ := m["main"].(map[string]any)
		return main != nil && main["freq_hz"] == float64(145800000)
	})
	m := lastState(t, fake, st)
	if m["freq_hz"] == float64(145800000) {
		t.Errorf("top-level freq_hz = %v followed MAIN; in satellite mode it must mirror SUB (uplink)", m["freq_hz"])
	}
	sub, _ := m["sub"].(map[string]any)
	if sub == nil || m["freq_hz"] != sub["freq_hz"] {
		t.Errorf("top-level freq_hz = %v, want the SUB value %v", m["freq_hz"], sub["freq_hz"])
	}
}

// ---------------------------------------------------------------------------
// per-VFO band validation + unsupported modes (R7/R10)
// ---------------------------------------------------------------------------

func TestSetFreqSub23cmRejectedExactString(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newTestBridge(t, fr, newRadioModel())
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"set_freq","vfo":"sub","value":"1296100000"}`)
	if got := lastError(t, fake, st); got != "freq rejected: out of band for sub" {
		t.Fatalf("error = %q, want exactly %q", got, "freq rejected: out of band for sub")
	}
	for _, f := range fr.CivFramesReceived() {
		if len(f) >= 5 && f[4] == civ.CmdSetFreq {
			t.Fatalf("a frequency frame reached the radio despite the local band rejection: % x", f)
		}
	}
	// MAIN carries 23cm: same value on the main VFO tunes.
	deliverCmd(b, `{"action":"set_freq","vfo":"main","value":"1296100000"}`)
	waitFor(t, 3*time.Second, "main on 23cm", func() bool {
		m := lastState(t, fake, st)
		return m["band"] == "23cm" && m["freq_hz"] == float64(1296100000)
	})
}

func TestSetModeUnsupportedRejected(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newTestBridge(t, fr, newRadioModel())
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"set_mode","value":"dv"}`)
	if got := lastError(t, fake, st); !strings.HasPrefix(got, "mode rejected: unsupported") {
		t.Fatalf("error = %q, want a mode-unsupported rejection", got)
	}
	// "data" is the 1A 06 modifier — pointed at set_data, never set as a mode.
	deliverCmd(b, `{"action":"set_mode","value":"data"}`)
	if got := lastError(t, fake, st); !strings.HasPrefix(got, "mode rejected:") {
		t.Fatalf("error = %q, want a mode rejection for data", got)
	}
	for _, f := range fr.CivFramesReceived() {
		if len(f) >= 5 && (f[4] == civ.CmdSetMode || f[4] == civ.CmdSetItem) {
			t.Fatalf("a mode frame reached the radio despite the rejection: % x", f)
		}
	}
	// A canonical mode still tunes (on the selected VFO).
	deliverCmd(b, `{"action":"set_mode","value":"usb"}`)
	waitLive(t, b)
	waitFor(t, 3*time.Second, "mode usb published", func() bool {
		return lastState(t, fake, st)["mode"] == "usb"
	})
}

// ---------------------------------------------------------------------------
// cmd gates: oversized, stale, unknown (KTD-6 one-shot posture)
// ---------------------------------------------------------------------------

func TestOversizedPayloadFixedRejectionNoEcho(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newTestBridge(t, fr, newRadioModel())
	st := topicState(b.opts)

	marker := "SECRETMARKERDONOTECHO"
	big := `{"action":"set_freq","value":"` + strings.Repeat(marker, 300) + `"}`
	deliverCmd(b, big)

	if got := lastError(t, fake, st); got != cmdErrMsgTooLarge {
		t.Fatalf("error = %q, want exactly the fixed %q", got, cmdErrMsgTooLarge)
	}
	for _, p := range fake.recorded() {
		if strings.Contains(p.payload, marker) {
			t.Fatalf("producer bytes echoed into a retained publish on %s", p.topic)
		}
	}
	waitFor(t, time.Second, "cmd cleared", func() bool { return cmdCleared(fake, topicCmd(b.opts)) })
}

func TestStaleTsDropped(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newTestBridge(t, fr, newRadioModel())
	st := topicState(b.opts)

	old := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	deliverCmd(b, fmt.Sprintf(`{"action":"set_freq","value":"433400000","ts":%q}`, old))
	if got := lastError(t, fake, st); !strings.HasPrefix(got, "stale cmd") {
		t.Fatalf("error = %q, want a stale-cmd rejection", got)
	}
	if len(fr.LoginAttempts()) != 0 {
		t.Errorf("stale cmd drove login attempts — must drop before any radio demand")
	}
	waitFor(t, time.Second, "cmd cleared", func() bool { return cmdCleared(fake, topicCmd(b.opts)) })
}

func TestUnknownActionWarnAndDrop(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newTestBridge(t, fr, newRadioModel())
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"frobnicate","value":"1"}`)
	if got := lastError(t, fake, st); got != "" {
		t.Fatalf("unknown action published error %q — must warn + drop", got)
	}
	waitFor(t, time.Second, "cmd cleared", func() bool { return cmdCleared(fake, topicCmd(b.opts)) })
}

// ---------------------------------------------------------------------------
// session loss and the per-state payload rules (R6) + safety-fact persistence
// ---------------------------------------------------------------------------

func TestSessionLossOmitsRadioFieldsAndRepublishes(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitBothVFOsKnown(t, fake, st)
	model.setMeter(120)
	time.Sleep(50 * time.Millisecond) // let the meter land in a published snapshot

	// The radio vanishes mid-session (reboot/network drop).
	fr.StopAnswering()
	waitFor(t, 3*time.Second, "device_online false after session loss", func() bool {
		return lastState(t, fake, st)["device_online"] == false
	})
	m := lastState(t, fake, st)
	for _, key := range []string{"freq_hz", "band", "mode", "tx", "main", "sub",
		"satellite", "s_meter", "swr", "alc", "tx_power"} {
		if _, has := m[key]; has {
			t.Errorf("post-loss /state carries radio-measured %q — must be omitted, not frozen (R6)", key)
		}
	}
	if m["session_state"] != "error" && m["session_state"] != "idle" {
		t.Errorf("session_state = %v, want error (or decayed idle)", m["session_state"])
	}
	if m["selected_vfo"] != "main" {
		t.Errorf("selected_vfo = %v, want the bridge-held value (survives sessions, R6)", m["selected_vfo"])
	}
	if _, ok := m["ts"].(string); !ok || m["ts"] == "" {
		t.Error("stamped fields must stay fresh after the loss republish")
	}
}

func TestSafetyErrorFactPersistsAcrossDecay(t *testing.T) {
	fr := mustFakeRadio(t)
	b, fake := newTestBridge(t, fr, newRadioModel())
	st := topicState(b.opts)

	// An undeliverable safety PTT-off is a safety-class fact: the session
	// state decays error -> idle, the fact persists until operator ack.
	fr.StopAnswering()
	b.sess.RequestPTTOff()
	waitFor(t, 3*time.Second, "terminal safety fact", func() bool {
		m, ok := tryState(fake, st)
		return ok && m["error"] == radio.FactPTTOffUndeliverable
	})
	waitFor(t, 2*time.Second, "session_state decayed to idle", func() bool {
		return lastState(t, fake, st)["session_state"] == "idle"
	})
	if got := lastError(t, fake, st); got != radio.FactPTTOffUndeliverable {
		t.Fatalf("error = %q after decay — the safety fact must persist (only session_state decays)", got)
	}
	// The next successful operator activity acks it.
	fr.Reboot(0) // the radio comes back (fresh session ID — full re-login, R3)
	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitFor(t, 2*time.Second, "error cleared by the successful arm", func() bool {
		return lastError(t, fake, st) == ""
	})
}

func TestNonSafetyErrorDecaysAndClears(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	// Live, then the radio vanishes: "session lost" is not safety-class and
	// decays away with the error state.
	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	fr.StopAnswering()
	waitFor(t, 3*time.Second, "session lost fact", func() bool {
		return lastError(t, fake, st) == "session lost"
	})
	waitFor(t, 3*time.Second, "error decayed and fact cleared", func() bool {
		return lastState(t, fake, st)["session_state"] == "idle" && lastError(t, fake, st) == ""
	})
}

// ---------------------------------------------------------------------------
// meters (KTD-8): <=1 Hz, dedup-suppressed, radio changes surface
// ---------------------------------------------------------------------------

func TestMetersAppearWhileLiveAndDedupSuppress(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	model.setMeter(87)
	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitFor(t, 3*time.Second, "s_meter 87 published", func() bool {
		return lastState(t, fake, st)["s_meter"] == float64(87)
	})

	// With the values unchanged, the retained snapshot dedups: no new
	// /state publishes over ~10 poll ticks (the heartbeat is 60 ticks).
	time.Sleep(50 * time.Millisecond)
	pubs := len(statePubs(t, fake, st))
	time.Sleep(300 * time.Millisecond)
	if got := len(statePubs(t, fake, st)); got != pubs {
		t.Errorf("%d extra /state publishes with an unchanged snapshot — dedup failed (KTD-8)", got-pubs)
	}

	// A meter change surfaces within a tick.
	model.setMeter(120)
	waitFor(t, 2*time.Second, "s_meter 120 published", func() bool {
		return lastState(t, fake, st)["s_meter"] == float64(120)
	})
}

func TestTSPowerReadFromRadio(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitFor(t, 3*time.Second, "tx_power published from the 14 0A read", func() bool {
		return lastState(t, fake, st)["tx_power"] == float64(100)
	})
}

// ---------------------------------------------------------------------------
// radio-initiated changes (R6 reconciliation): front-panel/wfview simulation
// ---------------------------------------------------------------------------

func TestRadioInitiatedSelectedVFOChangeSurfaces(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitBothVFOsKnown(t, fake, st)

	// The operator flips the front-panel band selection to SUB.
	model.setSel(1)
	waitFor(t, 3*time.Second, "selected_vfo flipped to sub by the next poll tick", func() bool {
		return lastState(t, fake, st)["selected_vfo"] == "sub"
	})
	m := lastState(t, fake, st)
	if m["freq_hz"] != float64(145200000) {
		t.Errorf("top-level freq_hz = %v, want the SUB-band value 145200000 (the SEL-marker source and the mirror flip together)", m["freq_hz"])
	}
}

func TestRadioInitiatedSatelliteAndTXChangesSurfaces(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)

	// wfview turns satellite mode on at the radio.
	model.setSat(true)
	waitFor(t, 3*time.Second, "satellite true from the 16 5A poll read", func() bool {
		return lastState(t, fake, st)["satellite"] == true
	})

	// The operator keys the mic at the radio: the 1C 00 transceive broadcast.
	fr.InjectCIV(pttTransFrame(true))
	waitFor(t, 3*time.Second, "tx = tx from the transceive broadcast", func() bool {
		return lastState(t, fake, st)["tx"] == "tx"
	})
}

func TestTransceiveFreqBroadcastUpdatesState(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)

	// A wfview-side tune on the selected band broadcasts cmd 00.
	fr.InjectCIV(freqTransFrame(433_500_000))
	waitFor(t, 3*time.Second, "state freq updated by the broadcast", func() bool {
		return lastState(t, fake, st)["freq_hz"] == float64(433500000)
	})
	m := lastState(t, fake, st)
	if m["band"] != "70cm" {
		t.Errorf("band = %v, want 70cm (derived from the broadcast frequency)", m["band"])
	}
}

func TestSelectedVFOBridgeHeldAcrossSessionLoss(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitBothVFOsKnown(t, fake, st)
	model.setSel(1)
	waitFor(t, 3*time.Second, "selected_vfo = sub", func() bool {
		return lastState(t, fake, st)["selected_vfo"] == "sub"
	})

	fr.StopAnswering()
	waitFor(t, 3*time.Second, "session down", func() bool {
		return lastState(t, fake, st)["device_online"] == false
	})
	if got := lastState(t, fake, st)["selected_vfo"]; got != "sub" {
		t.Errorf("selected_vfo = %v after session loss, want sub (bridge-held, R6)", got)
	}
}

// ---------------------------------------------------------------------------
// per-VFO detail actions: set_data / set_preamp / set_attenuator
// ---------------------------------------------------------------------------

func TestSetDataPreampAttenuator(t *testing.T) {
	fr := mustFakeRadio(t)
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"arm"}`)
	waitLive(t, b)
	waitBothVFOsKnown(t, fake, st)

	deliverCmd(b, `{"action":"set_data","value":"on"}`)
	waitFor(t, 2*time.Second, "data_mode true published", func() bool {
		m := lastState(t, fake, st)
		main, _ := m["main"].(map[string]any)
		return main != nil && main["data_mode"] == true
	})
	deliverCmd(b, `{"action":"set_preamp","value":"2"}`)
	waitFor(t, 2*time.Second, "preamp 2 published", func() bool {
		m := lastState(t, fake, st)
		main, _ := m["main"].(map[string]any)
		return main != nil && main["preamp"] == float64(2)
	})
	deliverCmd(b, `{"action":"set_attenuator","value":"on"}`)
	waitFor(t, 2*time.Second, "attenuator true published", func() bool {
		m := lastState(t, fake, st)
		main, _ := m["main"].(map[string]any)
		return main != nil && main["attenuator"] == true
	})
}

// A failed admit must leave the per-VFO echo cache untouched (review fix):
// v1 never polls preamp/attenuator, so an echo recorded on a failed set
// would persist as wrong state.
func TestPreampEchoOnlyOnDeliveredSet(t *testing.T) {
	fr := mustFakeRadio(t)
	fr.StopAnswering()
	model := newRadioModel()
	b, fake := newTestBridge(t, fr, model)
	st := topicState(b.opts)

	deliverCmd(b, `{"action":"set_preamp","vfo":"main","value":"1"}`)
	waitFor(t, 3*time.Second, "the set_preamp rejection", func() bool {
		return lastError(t, fake, st) != ""
	})
	m := lastState(t, fake, st)
	main, _ := m["main"].(map[string]any)
	if main != nil {
		if _, ok := main["preamp"]; ok {
			t.Fatalf("preamp echo recorded on a failed set: %v", main)
		}
	}
	// The retained /cmd is cleared on the rejection path too.
	waitFor(t, 2*time.Second, "retained cmd cleared after reject", func() bool {
		return cmdCleared(fake, topicCmd(b.opts))
	})
}
