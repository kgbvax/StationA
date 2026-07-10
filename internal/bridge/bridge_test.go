package bridge

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"acombridge/internal/acom"
)

// testLogger satisfies Logger by routing to the test log.
type testLogger struct{ t *testing.T }

func (l *testLogger) Infof(f string, a ...any)  { l.t.Logf("INFO: "+f, a...) }
func (l *testLogger) Warnf(f string, a ...any)  { l.t.Logf("WARN: "+f, a...) }
func (l *testLogger) Debugf(f string, a ...any) {}

// fakeCommander records /cmd dispatches without touching serial.
type fakeCommander struct {
	modes []string
	bands []string
	err   error
}

func (f *fakeCommander) SetMode(m string) error { f.modes = append(f.modes, m); return f.err }
func (f *fakeCommander) SetBand(b string) error { f.bands = append(f.bands, b); return f.err }

func newTestBridge(t *testing.T, cmd Commander, publishHA bool) (*Bridge, *MemoPublisher) {
	t.Helper()
	pub := NewMemoPublisher()
	cfg := Config{
		Site:               "muehle",
		Station:            "hf",
		Slot:               "pa",
		Location:           "bauwagen",
		Host:               "shari",
		DiscoveryPrefix:    "homeassistant",
		AvailTopic:         "muehle/hf/pa/status",
		PublishHADiscovery: publishHA,
		DeviceModel:        "ACOM 1200S",
		DeviceSerial:       "acom-1200s",
		DeviceLink:         "serial",
		Commander:          cmd,
	}
	return New(cfg, pub, &testLogger{t}), pub
}

// lastMsg returns the last message published to topic.
func lastMsg(msgs []MemoMsg, topic string) (MemoMsg, bool) {
	var found MemoMsg
	ok := false
	for _, m := range msgs {
		if m.Topic == topic {
			found = m
			ok = true
		}
	}
	return found, ok
}

func TestHandleTelemetryStateSnapshot(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{}, false)
	obs := acom.Observation{
		ForwardPower:   600,
		ReflectedPower: 3,
		SWR:            1.25,
		Temperature:    42.1,
		BandIndex:      5,
		BandName:       "20m",
		ModeRaw:        "OPR/TX",
		ErrByte:        0xFF,
		ErrMsg:         "NONE",
	}
	b.HandleTelemetry(obs)

	msg, ok := lastMsg(pub.Messages(), "muehle/hf/pa/state")
	if !ok {
		t.Fatal("no /state published after telemetry")
	}
	if !msg.Retained {
		t.Error("/state must be retained")
	}

	// Golden: unmarshal into the published envelope shape and assert canonical
	// fields, raw pa_state, and the ts.
	var snap struct {
		TS           string  `json:"ts"`
		Mode         string  `json:"mode"`
		Band         string  `json:"band"`
		Keyed        string  `json:"keyed"`
		FwdPowerW    uint16  `json:"fwd_power_w"`
		RflPowerW    uint16  `json:"rfl_power_w"`
		TempC        float64 `json:"temp_c"`
		SWR          float64 `json:"swr"`
		Fault        string  `json:"fault"`
		PaState      string  `json:"pa_state"`
		DeviceOnline bool    `json:"device_online"`
		Error        string  `json:"error"`
	}
	if err := json.Unmarshal(msg.Payload, &snap); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if snap.Mode != "operate" {
		t.Errorf("mode = %q, want operate", snap.Mode)
	}
	if snap.Keyed != "tx" {
		t.Errorf("keyed = %q, want tx", snap.Keyed)
	}
	if snap.Fault != "none" {
		t.Errorf("fault = %q, want none", snap.Fault)
	}
	if snap.PaState != "OPR/TX" {
		t.Errorf("pa_state = %q, want OPR/TX (raw preserved)", snap.PaState)
	}
	if snap.FwdPowerW != 600 || snap.RflPowerW != 3 {
		t.Errorf("power fwd=%d rfl=%d, want 600/3", snap.FwdPowerW, snap.RflPowerW)
	}
	if !snap.DeviceOnline {
		t.Error("device_online should be true on telemetry")
	}
	if snap.Error != "" {
		t.Errorf("error = %q, want empty for fault=none", snap.Error)
	}
	if snap.TS == "" || !strings.Contains(snap.TS, "T") {
		t.Errorf("ts = %q, want RFC3339", snap.TS)
	}
	// temp_c is rounded to 0.1 °C; a clean input stays clean.
	if snap.TempC != 42.1 {
		t.Errorf("temp_c = %v, want 42.1 (rounded to 0.1°C)", snap.TempC)
	}
}

// TestTempCRoundedToTenth locks the bus-cleanliness fix: a Kelvin→Celsius
// conversion leaves float64 noise (e.g. 28.850000000000023); the bridge must
// publish only 28.9 (one decimal), not the noisy value.
func TestTempCRoundedToTenth(t *testing.T) {
	for _, c := range []float64{28.850000000000023, 29.850000000000023, 42.05, 0.049} {
		if got := roundTempC(c); got != math.Round(c*10)/10 {
			t.Errorf("roundTempC(%v) = %v, want %v", c, got, math.Round(c*10)/10)
		}
	}
	// Noisy 28.850000000000023 must publish as exactly 28.9.
	if got := roundTempC(28.850000000000023); fmt.Sprintf("%.1f", got) != "28.9" {
		t.Errorf("roundTempC noisy = %v, want 28.9", got)
	}
}

func TestHandleTelemetryFaultMapping(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{}, false)
	b.HandleTelemetry(acom.Observation{
		ModeRaw:  "OPR/RX",
		BandName: "40m",
		ErrByte:  0x1C, // PAM1 TEMP TOO HIGH -> temp
		ErrMsg:   "PAM1 TEMP TOO HIGH",
	})
	msg, _ := lastMsg(pub.Messages(), "muehle/hf/pa/state")
	var snap struct {
		Mode  string `json:"mode"`
		Keyed string `json:"keyed"`
		Fault string `json:"fault"`
		Error string `json:"error"`
	}
	json.Unmarshal(msg.Payload, &snap)
	if snap.Mode != "operate" || snap.Keyed != "rx" {
		t.Errorf("mode/keyed = %q/%q, want operate/rx", snap.Mode, snap.Keyed)
	}
	if snap.Fault != "temp" {
		t.Errorf("fault = %q, want temp", snap.Fault)
	}
	if snap.Error != "PAM1 TEMP TOO HIGH" {
		t.Errorf("error = %q, want verbatim fault message", snap.Error)
	}
}

func TestHandleTelemetryStandbyKeyedInhibited(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{}, false)
	b.HandleTelemetry(acom.Observation{ModeRaw: "STANDBY", ErrByte: 0xFF, ErrMsg: "NONE"})
	msg, _ := lastMsg(pub.Messages(), "muehle/hf/pa/state")
	var snap struct {
		Mode  string `json:"mode"`
		Keyed string `json:"keyed"`
	}
	json.Unmarshal(msg.Payload, &snap)
	if snap.Mode != "standby" || snap.Keyed != "inhibited" {
		t.Errorf("standby -> mode/keyed = %q/%q, want standby/inhibited", snap.Mode, snap.Keyed)
	}
}

func TestPublishMeta(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{}, false)
	b.PublishMeta()

	msg, ok := lastMsg(pub.Messages(), "muehle/hf/pa/meta")
	if !ok {
		t.Fatal("no /meta published")
	}
	if !msg.Retained {
		t.Error("/meta must be retained")
	}
	var m map[string]any
	if err := json.Unmarshal(msg.Payload, &m); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if m["role"] != "pa" {
		t.Errorf("role = %v, want pa", m["role"])
	}
	if m["link"] != "serial" {
		t.Errorf("link = %v, want serial", m["link"])
	}
	if m["host"] != "shari" {
		t.Errorf("host = %v, want shari", m["host"])
	}
	caps, _ := m["capabilities"].(map[string]any)
	if caps == nil {
		t.Fatal("missing capabilities")
	}
	if caps["max_power_w"] != float64(1200) {
		t.Errorf("max_power_w = %v, want 1200", caps["max_power_w"])
	}
	bands, _ := caps["bands"].([]any)
	if len(bands) != 10 {
		t.Errorf("bands len = %d, want 10 (no 60m)", len(bands))
	}
	modes, _ := caps["modes"].([]any)
	if len(modes) != 2 || modes[0] != "operate" || modes[1] != "standby" {
		t.Errorf("modes = %v, want [operate standby]", modes)
	}

	// expose: mode and band are writable with a command descriptor; keyed/fault
	// are enums; pa_state is a string; device_online is boolean.
	expose, _ := m["expose"].(map[string]any)
	if expose == nil {
		t.Fatal("missing expose block")
	}
	fields, _ := expose["fields"].([]any)
	fieldByKey := map[string]map[string]any{}
	for _, f := range fields {
		fm, _ := f.(map[string]any)
		fieldByKey[fm["key"].(string)] = fm
	}
	mode := fieldByKey["mode"]
	if mode["writable"] != true {
		t.Error("mode field must be writable")
	}
	cmd, _ := mode["command"].(map[string]any)
	if cmd == nil || cmd["action"] != "set_mode" {
		t.Error("mode field command must be {action:set_mode}")
	}
	band := fieldByKey["band"]
	if band["writable"] != true {
		t.Error("band field must be writable")
	}
	bcmd, _ := band["command"].(map[string]any)
	if bcmd == nil || bcmd["action"] != "set_band" {
		t.Error("band field command must be {action:set_band}")
	}
	if fieldByKey["keyed"]["type"] != "enum" {
		t.Error("keyed field must be enum")
	}
	if fieldByKey["fault"]["type"] != "enum" {
		t.Error("fault field must be enum")
	}
	if fieldByKey["pa_state"]["type"] != "string" {
		t.Error("pa_state field must be string")
	}
	if fieldByKey["device_online"]["type"] != "boolean" {
		t.Error("device_online field must be boolean")
	}
	// No freq field must leak into the PA state surface.
	if _, ok := fieldByKey["freq_hz"]; ok {
		t.Error("freq_hz must not be exposed on the PA slot")
	}
}

func TestCmdDispatchSetMode(t *testing.T) {
	cmd := &fakeCommander{}
	b, _ := newTestBridge(t, cmd, false)
	b.HandleCommand([]byte(`{"action":"set_mode","value":"standby"}`))
	if len(cmd.modes) != 1 || cmd.modes[0] != "standby" {
		t.Errorf("set_mode dispatch = %v, want [standby]", cmd.modes)
	}
}

func TestCmdDispatchSetBand(t *testing.T) {
	cmd := &fakeCommander{}
	b, _ := newTestBridge(t, cmd, false)
	b.HandleCommand([]byte(`{"action":"set_band","value":"20m"}`))
	if len(cmd.bands) != 1 || cmd.bands[0] != "20m" {
		t.Errorf("set_band dispatch = %v, want [20m]", cmd.bands)
	}
}

func TestCmdDispatchUnknownAction(t *testing.T) {
	cmd := &fakeCommander{}
	b, _ := newTestBridge(t, cmd, false)
	b.HandleCommand([]byte(`{"action":"frobnicate","value":"x"}`))
	if len(cmd.modes)+len(cmd.bands) != 0 {
		t.Error("unknown action must not dispatch to commander")
	}
}

func TestCmdDispatchMalformed(t *testing.T) {
	cmd := &fakeCommander{}
	b, _ := newTestBridge(t, cmd, false)
	b.HandleCommand([]byte(`{not json`))
	if len(cmd.modes)+len(cmd.bands) != 0 {
		t.Error("malformed cmd must not dispatch")
	}
}

func TestCmdNoCommander(t *testing.T) {
	// read-only deploy: Commander nil -> dispatch is a logged no-op, not a panic.
	b, _ := newTestBridge(t, nil, false)
	b.HandleCommand([]byte(`{"action":"set_mode","value":"operate"}`))
}

func TestHADiscoveryGateOffByDefault(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{}, false) // gate off
	b.PublishDiscovery()
	for _, m := range pub.Messages() {
		if strings.HasPrefix(m.Topic, "homeassistant/") {
			t.Errorf("legacy HA discovery must not publish when gate is off: %s", m.Topic)
		}
	}
}

func TestHADiscoveryGateOn(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{}, true) // gate on
	b.PublishDiscovery()
	found := false
	for _, m := range pub.Messages() {
		if strings.HasPrefix(m.Topic, "homeassistant/") {
			found = true
			if !m.Retained {
				t.Errorf("discovery config %s must be retained", m.Topic)
			}
		}
	}
	if !found {
		t.Fatal("gate on should publish homeassistant/ discovery configs")
	}
	// Idempotent: a second call must not double-publish.
	n := len(pub.Messages())
	b.PublishDiscovery()
	if len(pub.Messages()) != n {
		t.Errorf("PublishDiscovery should emit once per cycle; grew %d -> %d", n, len(pub.Messages()))
	}
}

func TestSetDeviceOnlineFalsePublishes(t *testing.T) {
	b, pub := newTestBridge(t, &fakeCommander{}, false)
	b.HandleTelemetry(acom.Observation{ModeRaw: "OPR/RX", ErrByte: 0xFF, ErrMsg: "NONE"})
	pub.Reset()
	b.SetDeviceOnline(false, "serial: no data for 30s")
	msg, ok := lastMsg(pub.Messages(), "muehle/hf/pa/state")
	if !ok {
		t.Fatal("SetDeviceOnline should republish /state")
	}
	var snap struct {
		DeviceOnline bool   `json:"device_online"`
		Error        string `json:"error"`
	}
	json.Unmarshal(msg.Payload, &snap)
	if snap.DeviceOnline {
		t.Error("device_online should be false after port loss")
	}
	if !strings.Contains(snap.Error, "no data") {
		t.Errorf("error = %q, want serial failure message", snap.Error)
	}
}
