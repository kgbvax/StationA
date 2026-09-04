package bridge

import (
	"encoding/json"
	"testing"

	"atr1k-tuner-bridge/internal/tuner"
)

// fakeCommander records the commands the bridge issued.
type fakeCommander struct {
	inlines []bool
	tunes   []bool
	err     error
}

func (f *fakeCommander) SetInline(inline bool) error {
	f.inlines = append(f.inlines, inline)
	return f.err
}
func (f *fakeCommander) Tune(full bool) error {
	f.tunes = append(f.tunes, full)
	return f.err
}

func newTestBridge(t *testing.T) (*Bridge, *MemoPublisher, *fakeCommander) {
	t.Helper()
	pub := NewMemoPublisher()
	cmd := &fakeCommander{}
	b := New(Config{
		Site:        "muehle",
		Station:     "hf",
		Slot:        "tuner",
		Location:    "bauwagen",
		Host:        "shari",
		DeviceModel: "ATR-1000",
		DeviceLink:  "wifi",
		Commander:   cmd,
	}, pub, &testLogger{t})
	return b, pub, cmd
}

type testLogger struct{ t *testing.T }

func (l *testLogger) Infof(format string, args ...any)  { l.t.Logf("INFO: "+format, args...) }
func (l *testLogger) Warnf(format string, args ...any)  { l.t.Logf("WARN: "+format, args...) }
func (l *testLogger) Errorf(format string, args ...any) { l.t.Logf("ERROR: "+format, args...) }
func (l *testLogger) Debugf(format string, args ...any) { l.t.Logf("DEBUG: "+format, args...) }

func TestPublishMeta(t *testing.T) {
	b, pub, _ := newTestBridge(t)
	b.PublishMeta()

	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 meta publish, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Topic != "muehle/hf/tuner/meta" {
		t.Errorf("meta topic = %q, want muehle/hf/tuner/meta", m.Topic)
	}
	if !m.Retained {
		t.Error("meta must be retained")
	}
	var meta map[string]any
	if err := json.Unmarshal(m.Payload, &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta["schema"] != "1.0" {
		t.Errorf("schema = %v, want 1.0", meta["schema"])
	}
	if meta["role"] != "tuner" {
		t.Errorf("role = %v, want tuner", meta["role"])
	}
	if meta["link"] != "wifi" {
		t.Errorf("link = %v, want wifi", meta["link"])
	}
	if meta["location"] != "bauwagen" {
		t.Errorf("location = %v, want bauwagen", meta["location"])
	}
	if meta["host"] != "shari" {
		t.Errorf("host = %v, want shari", meta["host"])
	}
	caps, _ := meta["capabilities"].(map[string]any)
	if caps["inline"] != true {
		t.Errorf("capabilities.inline = %v, want true", caps["inline"])
	}
	modes, _ := caps["tune_modes"].([]any)
	if len(modes) != 2 || modes[0] != "mem" || modes[1] != "full" {
		t.Errorf("capabilities.tune_modes = %v, want [mem full]", modes)
	}
	expose, _ := meta["expose"].(map[string]any)
	if expose == nil {
		t.Fatal("expose block missing")
	}
	fields, _ := expose["fields"].([]any)
	// Verify the inline field is writable with the set_inline command descriptor.
	var inlineField map[string]any
	for _, f := range fields {
		fm := f.(map[string]any)
		if fm["key"] == "inline" {
			inlineField = fm
		}
	}
	if inlineField == nil {
		t.Fatal("inline field missing from expose.fields")
	}
	if inlineField["writable"] != true {
		t.Error("inline field must be writable")
	}
	cmd, _ := inlineField["command"].(map[string]any)
	if cmd["action"] != "set_inline" || cmd["value_key"] != "value" || cmd["value_type"] != "bool" {
		t.Errorf("inline command = %v, want action=set_inline value_key=value value_type=bool", cmd)
	}
	actions, _ := expose["actions"].([]any)
	if len(actions) != 1 {
		t.Errorf("expose.actions len = %d, want 1 (tune)", len(actions))
	} else {
		act := actions[0].(map[string]any)
		if act["options_ref"] != "tune_modes" {
			t.Errorf("tune options_ref = %v, want tune_modes (so consumers resolve capabilities.tune_modes)", act["options_ref"])
		}
		actCmd, _ := act["command"].(map[string]any)
		if actCmd["action"] != "tune" || actCmd["value_key"] != "value" || actCmd["value_type"] != "enum" {
			t.Errorf("tune command = %v, want action=tune value_key=value value_type=enum", actCmd)
		}
	}
}

func TestHandleCommand(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		check   func(*fakeCommander)
	}{
		{"set_inline_true", `{"action":"set_inline","value":true}`, func(c *fakeCommander) {
			if len(c.inlines) != 1 || c.inlines[0] != true {
				t.Errorf("inlines = %v, want [true]", c.inlines)
			}
		}},
		{"set_inline_false", `{"action":"set_inline","value":false}`, func(c *fakeCommander) {
			if len(c.inlines) != 1 || c.inlines[0] != false {
				t.Errorf("inlines = %v, want [false]", c.inlines)
			}
		}},
		{"tune_mem", `{"action":"tune","value":"mem"}`, func(c *fakeCommander) {
			if len(c.tunes) != 1 || c.tunes[0] != false {
				t.Errorf("tunes = %v, want [false]", c.tunes)
			}
		}},
		{"tune_full", `{"action":"tune","value":"full"}`, func(c *fakeCommander) {
			if len(c.tunes) != 1 || c.tunes[0] != true {
				t.Errorf("tunes = %v, want [true]", c.tunes)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _, cmd := newTestBridge(t)
			b.HandleCommand([]byte(tc.payload))
			tc.check(cmd)
		})
	}
}

func TestHandleCommandUnknown(t *testing.T) {
	b, _, cmd := newTestBridge(t)
	b.HandleCommand([]byte(`{"action":"bypass"}`))
	if len(cmd.inlines) != 0 || len(cmd.tunes) != 0 {
		t.Error("unknown action must not touch the commander")
	}
}

func TestHandleCommandTuneBadMode(t *testing.T) {
	b, _, cmd := newTestBridge(t)
	// The argument rides under the `value` key (not `mode`); a value that is not
	// a recognized tune mode (mem|full) must be rejected without calling Tune.
	b.HandleCommand([]byte(`{"action":"tune","value":"fine"}`))
	if len(cmd.tunes) != 0 {
		t.Error("unknown tune mode must not call Tune")
	}
}

// TestHandleCommandTuneNullValue guards the live testui bug: an action carrying
// value_key but a JSON-null value unmarshals to the zero string "" with no error,
// which must NOT be accepted as a tune (it would log "unknown mode \"\"" and drop
// the command — the symptom the testui produced before it rendered the enum).
func TestHandleCommandTuneNullValue(t *testing.T) {
	b, _, cmd := newTestBridge(t)
	b.HandleCommand([]byte(`{"action":"tune","value":null}`))
	if len(cmd.tunes) != 0 {
		t.Error("null tune value must not call Tune")
	}
}

func TestHandleCommandSetInlineMissingValue(t *testing.T) {
	b, _, cmd := newTestBridge(t)
	b.HandleCommand([]byte(`{"action":"set_inline"}`))
	if len(cmd.inlines) != 0 {
		t.Error("set_inline without inline must not call SetInline")
	}
}

func TestHandleCommandNoCommander(t *testing.T) {
	pub := NewMemoPublisher()
	b := New(Config{Site: "muehle", Station: "hf", Slot: "tuner"}, pub, &testLogger{t})
	// No commander configured: must not panic.
	b.HandleCommand([]byte(`{"action":"tune","mode":"full"}`))
}

func TestHandleTelemetryDedup(t *testing.T) {
	b, pub, _ := newTestBridge(t)
	st := tuner.State{SWR: 2.0, Fwd: 100, Inline: true, DeviceOnline: true}

	b.HandleTelemetry(st)
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("first telemetry: got %d publishes, want 1", n)
	}
	// Identical telemetry must NOT re-publish (dedup).
	b.HandleTelemetry(st)
	if n := len(pub.Messages()); n != 1 {
		t.Errorf("duplicate telemetry: got %d publishes, want 1 (dedup)", n)
	}
	// A changed SWR re-publishes.
	b.HandleTelemetry(tuner.State{SWR: 2.5, Fwd: 100, Inline: true, DeviceOnline: true})
	if n := len(pub.Messages()); n != 2 {
		t.Errorf("changed telemetry: got %d publishes, want 2", n)
	}
	// The published snapshot carries the SWR and an RFC3339 ts.
	var snap map[string]any
	if err := json.Unmarshal(pub.Messages()[1].Payload, &snap); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if snap["swr"] != 2.5 {
		t.Errorf("state.swr = %v, want 2.5", snap["swr"])
	}
	if ts, _ := snap["ts"].(string); ts == "" {
		t.Error("state.ts missing")
	}
}

func TestSetDeviceOnlinePublishesOffline(t *testing.T) {
	b, pub, _ := newTestBridge(t)
	// Seed a known state via telemetry so the snapshot has content.
	b.HandleTelemetry(tuner.State{SWR: 1.8, Fwd: 50, Inline: true, DeviceOnline: true})
	pub.Reset()

	b.SetDeviceOnline(false, "atr1k: connection reset")
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d publishes, want 1", len(msgs))
	}
	var snap map[string]any
	if err := json.Unmarshal(msgs[0].Payload, &snap); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if snap["device_online"] != false {
		t.Errorf("device_online = %v, want false", snap["device_online"])
	}
	if snap["error"] != "atr1k: connection reset" {
		t.Errorf("error = %v, want the message", snap["error"])
	}
	if snap["swr"] != 1.8 {
		t.Errorf("swr = %v, want last-known 1.8 preserved", snap["swr"])
	}
}

func TestStateTopics(t *testing.T) {
	b, _, _ := newTestBridge(t)
	if got := b.metaTopic(); got != "muehle/hf/tuner/meta" {
		t.Errorf("metaTopic = %q", got)
	}
	if got := b.stateTopic(); got != "muehle/hf/tuner/state" {
		t.Errorf("stateTopic = %q", got)
	}
	if got := b.CmdTopic(); got != "muehle/hf/tuner/cmd" {
		t.Errorf("CmdTopic = %q", got)
	}
}
