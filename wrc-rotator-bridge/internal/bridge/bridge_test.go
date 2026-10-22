package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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

// overrideCommander is a fakeCommander that also accepts the override tag
// (the shape of the antenna-mode lock gate).
type overrideCommander struct {
	fakeCommander
	overrides []string
}

func (o *overrideCommander) SetAzOverride(az float64, override string) error {
	o.sets = append(o.sets, az)
	o.overrides = append(o.overrides, override)
	return o.err
}

// recLogger counts Warn lines.
type recLogger struct {
	testLogger
	warns []string
}

func (l *recLogger) Warnf(format string, args ...any) {
	l.warns = append(l.warns, fmt.Sprintf(format, args...))
}

func lastState(t *testing.T, pub *MemoPublisher) map[string]any {
	t.Helper()
	msgs := pub.Messages()
	if len(msgs) == 0 {
		t.Fatal("no /state published")
	}
	m := msgs[len(msgs)-1]
	if m.Topic != "muehle/hf/rotator/state" || !m.Retained {
		t.Fatalf("last publish %q retained=%v, want retained /state", m.Topic, m.Retained)
	}
	var snap map[string]any
	if err := json.Unmarshal(m.Payload, &snap); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	return snap
}

func TestStateLockFieldPresentOnlyWhenLocked(t *testing.T) {
	b, pub, _ := newTestBridge(t)
	b.HandleTelemetry(rotor.State{Az: 90, DeviceOnline: true})
	if _, ok := lastState(t, pub)["lock"]; ok {
		t.Error("unlocked /state carries lock; want it omitted")
	}
	b.SetLock("storm")
	if got := lastState(t, pub)["lock"]; got != "storm" {
		t.Errorf("state.lock = %v, want storm", got)
	}
	if got := lastState(t, pub)["az"]; got != 90.0 {
		t.Errorf("lock snapshot az = %v, want the current 90", got)
	}
	b.SetLock("")
	if _, ok := lastState(t, pub)["lock"]; ok {
		t.Error("lock not omitted after unlock")
	}
}

func TestSetLockPublishesOnlyOnChange(t *testing.T) {
	b, pub, _ := newTestBridge(t)
	b.HandleTelemetry(rotor.State{Az: 90, DeviceOnline: true})
	pub.Reset()

	b.SetLock("") // unchanged (still unlocked)
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("SetLock(\"\") while unlocked: %d publishes, want 0", n)
	}
	b.SetLock("maintenance")
	b.SetLock("maintenance") // the lock loop calls this every second
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("repeated SetLock: %d publishes, want 1", n)
	}
	b.SetLock("storm")
	if n := len(pub.Messages()); n != 2 {
		t.Fatalf("lock change: %d publishes, want 2", n)
	}
}

func TestHandleTelemetryDedupIncludesLock(t *testing.T) {
	b, pub, _ := newTestBridge(t)
	st := rotor.State{Az: 90, DeviceOnline: true}
	b.HandleTelemetry(st)
	b.SetLock("storm")
	pub.Reset()

	// Same telemetry, same lock: deduped.
	b.HandleTelemetry(st)
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("unchanged telemetry under lock: %d publishes, want 0", n)
	}
	// Changed telemetry keeps carrying the lock.
	b.HandleTelemetry(rotor.State{Az: 91, Moving: true, DeviceOnline: true})
	if got := lastState(t, pub)["lock"]; got != "storm" {
		t.Errorf("telemetry snapshot lock = %v, want storm", got)
	}
	// The device-online path carries it too.
	b.SetDeviceOnline(false, "wrc: gone")
	if got := lastState(t, pub)["lock"]; got != "storm" {
		t.Errorf("offline snapshot lock = %v, want storm", got)
	}
}

func TestSetAzOverrideRouting(t *testing.T) {
	newBridge := func(c Commander) *Bridge {
		return New(Config{Site: "muehle", Station: "hf", Slot: "rotator", Commander: c},
			NewMemoPublisher(), &testLogger{t})
	}

	oc := &overrideCommander{}
	b := newBridge(oc)
	b.HandleCommand([]byte(`{"action":"set_az","az":120,"override":"maintenance"}`))
	if len(oc.overrides) != 1 || oc.overrides[0] != "maintenance" || oc.sets[0] != 120 {
		t.Fatalf("tagged set_az: overrides=%v sets=%v, want SetAzOverride(120, maintenance)", oc.overrides, oc.sets)
	}
	b.HandleCommand([]byte(`{"action":"set_az","az":130}`))
	if len(oc.overrides) != 1 || len(oc.sets) != 2 || oc.sets[1] != 130 {
		t.Fatalf("untagged set_az: overrides=%v sets=%v, want plain SetAz(130)", oc.overrides, oc.sets)
	}

	// A plain commander ignores the tag: plain SetAz.
	fc := &fakeCommander{}
	newBridge(fc).HandleCommand([]byte(`{"action":"set_az","az":140,"override":"maintenance"}`))
	if len(fc.sets) != 1 || fc.sets[0] != 140 {
		t.Fatalf("plain commander: sets=%v, want [140]", fc.sets)
	}
}

func TestLockRefusalNotLoggedAtWarn(t *testing.T) {
	log := &recLogger{testLogger: testLogger{t}}
	cmd := &fakeCommander{err: fmt.Errorf("gate: %w", rotor.ErrLocked)}
	b := New(Config{Site: "muehle", Station: "hf", Slot: "rotator", Commander: cmd},
		NewMemoPublisher(), log)
	for _, p := range []string{
		`{"action":"set_az","az":10}`, `{"action":"fwd"}`, `{"action":"rev"}`,
	} {
		b.HandleCommand([]byte(p))
	}
	if len(log.warns) != 0 {
		t.Errorf("lock refusals logged at Warn by the bridge: %v", log.warns)
	}

	// Any other failure still warns.
	cmd.err = errors.New("wrc: websocket not connected")
	b.HandleCommand([]byte(`{"action":"stop"}`))
	if len(log.warns) != 1 {
		t.Errorf("device failure: %d warns, want 1", len(log.warns))
	}
}

// blockingPublisher holds the first Publish until release is closed.
type blockingPublisher struct {
	*MemoPublisher
	once             sync.Once
	entered, release chan struct{}
}

func (p *blockingPublisher) Publish(topic string, retained bool, payload []byte) error {
	block := false
	p.once.Do(func() { block = true })
	if block {
		close(p.entered)
		<-p.release
	}
	return p.MemoPublisher.Publish(topic, retained, payload)
}

// The lock loop (SetLock) and the WRC read loop (HandleTelemetry) publish on
// different goroutines; the newer snapshot must be the one left retained.
func TestConcurrentPublishesStayOrdered(t *testing.T) {
	pub := &blockingPublisher{MemoPublisher: NewMemoPublisher(),
		entered: make(chan struct{}), release: make(chan struct{})}
	b := New(Config{Site: "muehle", Station: "hf", Slot: "rotator"}, pub, &testLogger{t})

	telDone := make(chan struct{})
	go func() { b.HandleTelemetry(rotor.State{Az: 91, DeviceOnline: true}); close(telDone) }()
	<-pub.entered // the lock-less snapshot is on its way to the broker
	lockDone := make(chan struct{})
	go func() { b.SetLock("storm"); close(lockDone) }()
	select {
	case <-lockDone:
	case <-time.After(50 * time.Millisecond):
	}
	close(pub.release)
	<-telDone
	<-lockDone

	if got := lastState(t, pub.MemoPublisher)["lock"]; got != "storm" {
		t.Fatalf("retained /state lock = %v, want storm (stale snapshot published last)", got)
	}
}
