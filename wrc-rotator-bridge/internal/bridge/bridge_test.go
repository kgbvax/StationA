package bridge

import (
	"encoding/json"
	"testing"

	"wrc-rotator-bridge/internal/rotor"
)

// fakeCommander records the commands the bridge issued.
type fakeCommander struct {
	sets  []float64
	stops int
	jogs  []string
	err   error
}

func (f *fakeCommander) SetAz(az float64) error { f.sets = append(f.sets, az); return f.err }
func (f *fakeCommander) Stop() error            { f.stops++; return f.err }
func (f *fakeCommander) Jog(dir string) error   { f.jogs = append(f.jogs, dir); return f.err }

func newTestBridge(t *testing.T) (*Bridge, *MemoPublisher, *fakeCommander) {
	t.Helper()
	pub := NewMemoPublisher()
	cmd := &fakeCommander{}
	b := New(Config{
		Site:        "muehle",
		Station:     "hf",
		Slot:        "rotator",
		Location:    "bauwagen",
		Host:        "shari",
		DeviceModel: "Yaesu G-450DC",
		DeviceLink:  "ethernet",
		Commander:   cmd,
	}, pub, &testLogger{t})
	return b, pub, cmd
}

type testLogger struct{ t *testing.T }

func (l *testLogger) Infof(format string, args ...any)  { l.t.Logf("INFO: "+format, args...) }
func (l *testLogger) Warnf(format string, args ...any)  { l.t.Logf("WARN: "+format, args...) }
func (l *testLogger) Debugf(format string, args ...any) { l.t.Logf("DEBUG: "+format, args...) }

func TestPublishMeta(t *testing.T) {
	b, pub, _ := newTestBridge(t)
	b.PublishMeta()

	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 meta publish, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Topic != "muehle/hf/rotator/meta" {
		t.Errorf("meta topic = %q, want muehle/hf/rotator/meta", m.Topic)
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
	if meta["role"] != "rotator" {
		t.Errorf("role = %v, want rotator", meta["role"])
	}
	if meta["link"] != "ethernet" {
		t.Errorf("link = %v, want ethernet", meta["link"])
	}
	if meta["location"] != "bauwagen" {
		t.Errorf("location = %v, want bauwagen", meta["location"])
	}
	if meta["host"] != "shari" {
		t.Errorf("host = %v, want shari", meta["host"])
	}
	caps, _ := meta["capabilities"].(map[string]any)
	axes, _ := caps["axes"].([]any)
	if len(axes) != 1 || axes[0] != "az" {
		t.Errorf("capabilities.axes = %v, want [az]", axes)
	}
	expose, _ := meta["expose"].(map[string]any)
	if expose == nil {
		t.Fatal("expose block missing")
	}
	fields, _ := expose["fields"].([]any)
	// Verify the az field is writable with the set_az command descriptor.
	var azField map[string]any
	for _, f := range fields {
		fm := f.(map[string]any)
		if fm["key"] == "az" {
			azField = fm
		}
	}
	if azField == nil {
		t.Fatal("az field missing from expose.fields")
	}
	if azField["writable"] != true {
		t.Error("az field must be writable")
	}
	cmd, _ := azField["command"].(map[string]any)
	if cmd["action"] != "set_az" || cmd["value_key"] != "az" || cmd["value_type"] != "float" {
		t.Errorf("az command = %v, want action=set_az value_key=az value_type=float", cmd)
	}
	actions, _ := expose["actions"].([]any)
	if len(actions) != 3 {
		t.Errorf("expose.actions len = %d, want 3 (stop/fwd/rev)", len(actions))
	}
}

func TestHandleCommand(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		check   func(*fakeCommander)
	}{
		{"set_az", `{"action":"set_az","az":180}`, func(c *fakeCommander) {
			if len(c.sets) != 1 || c.sets[0] != 180 {
				t.Errorf("sets = %v, want [180]", c.sets)
			}
		}},
		{"stop", `{"action":"stop"}`, func(c *fakeCommander) {
			if c.stops != 1 {
				t.Errorf("stops = %d, want 1", c.stops)
			}
		}},
		{"fwd", `{"action":"fwd"}`, func(c *fakeCommander) {
			if len(c.jogs) != 1 || c.jogs[0] != "fwd" {
				t.Errorf("jogs = %v, want [fwd]", c.jogs)
			}
		}},
		{"rev", `{"action":"rev"}`, func(c *fakeCommander) {
			if len(c.jogs) != 1 || c.jogs[0] != "rev" {
				t.Errorf("jogs = %v, want [rev]", c.jogs)
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
	b.HandleCommand([]byte(`{"action":"park"}`))
	if len(cmd.sets) != 0 || cmd.stops != 0 || len(cmd.jogs) != 0 {
		t.Error("unknown action must not touch the commander")
	}
}

func TestHandleCommandSetAzMissingValue(t *testing.T) {
	b, _, cmd := newTestBridge(t)
	b.HandleCommand([]byte(`{"action":"set_az"}`))
	if len(cmd.sets) != 0 {
		t.Error("set_az without az must not call SetAz")
	}
}

func TestHandleCommandNoCommander(t *testing.T) {
	pub := NewMemoPublisher()
	b := New(Config{Site: "muehle", Station: "hf", Slot: "rotator"}, pub, &testLogger{t})
	// No commander configured: must not panic.
	b.HandleCommand([]byte(`{"action":"stop"}`))
}

func TestHandleTelemetryDedup(t *testing.T) {
	b, pub, _ := newTestBridge(t)
	st := rotor.State{Az: 90, Moving: false, DeviceOnline: true}

	b.HandleTelemetry(st)
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("first telemetry: got %d publishes, want 1", n)
	}
	// Identical telemetry must NOT re-publish (dedup).
	b.HandleTelemetry(st)
	if n := len(pub.Messages()); n != 1 {
		t.Errorf("duplicate telemetry: got %d publishes, want 1 (dedup)", n)
	}
	// A changed azimuth re-publishes.
	b.HandleTelemetry(rotor.State{Az: 91, Moving: true, DeviceOnline: true})
	if n := len(pub.Messages()); n != 2 {
		t.Errorf("changed telemetry: got %d publishes, want 2", n)
	}
	// The published snapshot carries the azimuth and an RFC3339 ts.
	var snap map[string]any
	if err := json.Unmarshal(pub.Messages()[1].Payload, &snap); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if snap["az"] != 91.0 {
		t.Errorf("state.az = %v, want 91", snap["az"])
	}
	if ts, _ := snap["ts"].(string); ts == "" {
		t.Error("state.ts missing")
	}
}

func TestSetDeviceOnlinePublishesOffline(t *testing.T) {
	b, pub, _ := newTestBridge(t)
	// Seed a known state via telemetry so the snapshot has content.
	b.HandleTelemetry(rotor.State{Az: 120, DeviceOnline: true})
	pub.Reset()

	b.SetDeviceOnline(false, "wrc: connection reset")
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
	if snap["error"] != "wrc: connection reset" {
		t.Errorf("error = %v, want the message", snap["error"])
	}
	if snap["az"] != 120.0 {
		t.Errorf("az = %v, want last-known 120 preserved", snap["az"])
	}
}

func TestStateTopics(t *testing.T) {
	b, _, _ := newTestBridge(t)
	if got := b.metaTopic(); got != "muehle/hf/rotator/meta" {
		t.Errorf("metaTopic = %q", got)
	}
	if got := b.stateTopic(); got != "muehle/hf/rotator/state" {
		t.Errorf("stateTopic = %q", got)
	}
	if got := b.CmdTopic(); got != "muehle/hf/rotator/cmd" {
		t.Errorf("CmdTopic = %q", got)
	}
}
