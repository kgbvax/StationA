package mqtt

import (
	"encoding/json"
	"math"
	"sync"
	"testing"

	"pelcobridge2/internal/control"
)

// nilLogger swallows log output.
type nilLogger struct{}

func (nilLogger) Infof(string, ...any)  {}
func (nilLogger) Warnf(string, ...any)  {}
func (nilLogger) Debugf(string, ...any) {}

func testConfig() Config {
	return Config{
		Enabled: true, Broker: "tcp://localhost:1883",
		Site: "muehle", Station: "uhf", Slot: "rotator",
		DeviceModel: "PTS-303Z/3050DZ", DeviceName: "UHF Rotator", DeviceLink: "rs485", Host: "shack-pc",
	}
}

// recordingSubmit records submitted intents.
type recordingSubmit struct {
	mu    sync.Mutex
	calls []control.Intent
	err   error
}

func (r *recordingSubmit) submit(it control.Intent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, it)
	return r.err
}

func (r *recordingSubmit) intents() []control.Intent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]control.Intent(nil), r.calls...)
}

func TestMetaShape(t *testing.T) {
	memo := NewMemoPublisher()
	slot := NewSlot(testConfig(), memo, nilLogger{}, nil, nil)

	slot.PublishMeta()

	msg := memo.Last(testConfig().MetaTopic())
	if msg == nil {
		t.Fatal("meta never published")
	}
	if !msg.Retained {
		t.Error("meta must be retained")
	}
	var m struct {
		Schema string `json:"schema"`
		Role   string `json:"role"`
		Expose struct {
			Fields []struct {
				Key string `json:"key"`
			} `json:"fields"`
			Actions []struct {
				Key string `json:"key"`
			} `json:"actions"`
		} `json:"expose"`
	}
	if err := json.Unmarshal(msg.Payload, &m); err != nil {
		t.Fatalf("meta JSON: %v", err)
	}
	if m.Schema == "" || m.Role != "rotator" {
		t.Errorf("meta schema/role = %q/%q", m.Schema, m.Role)
	}

	// Exactly one action, and it is stop — MQTT has no motion path.
	if len(m.Expose.Actions) != 1 || m.Expose.Actions[0].Key != "stop" {
		t.Fatalf("meta actions = %+v, want exactly [stop]", m.Expose.Actions)
	}
	want := map[string]bool{"az": false, "el": false, "target_az": false, "target_el": false, "moving": false, "armed": false, "device_online": false}
	for _, f := range m.Expose.Fields {
		if _, ok := want[f.Key]; ok {
			want[f.Key] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("meta field %q missing", k)
		}
	}
}

func TestStatePublishAndDedup(t *testing.T) {
	memo := NewMemoPublisher()
	slot := NewSlot(testConfig(), memo, nilLogger{}, nil, func() int { return 2 })

	snap := &control.Snapshot{
		Az: 123.456, El: 30, PhysAz: 130.456, PhysEl: 30,
		ReadbackValid: true, Armed: true, Offset: 6.544, JogSpeed: 0x12,
		DeviceOnline: true, SetStatus: "converged",
	}
	slot.PublishState(snap)
	msg := memo.Last(testConfig().StateTopic())
	if msg == nil {
		t.Fatal("state never published")
	}
	if !msg.Retained {
		t.Error("state must be retained")
	}
	var p statePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		t.Fatalf("state JSON: %v", err)
	}
	if p.Az == nil || *p.Az != 123.46 {
		t.Errorf("az = %v, want 123.46 (rounded to 0.01°)", p.Az)
	}
	if p.PhysAz == nil || *p.PhysAz != 130.46 {
		t.Errorf("phys_az = %v, want 130.46", p.PhysAz)
	}
	if p.AzOffsetDeg == nil || *p.AzOffsetDeg != 6.54 {
		t.Errorf("az_offset_deg = %v, want 6.54", p.AzOffsetDeg)
	}
	if p.Link != "ok" {
		t.Errorf("link = %q with DeviceOnline=true, want ok", p.Link)
	}
	if p.JogSpeed != 0x12 {
		t.Errorf("jog_speed = %d, want 18", p.JogSpeed)
	}
	if p.RotctldClients != 2 {
		t.Errorf("rotctld_clients = %d, want 2", p.RotctldClients)
	}
	if p.Ts == "" {
		t.Error("ts missing")
	}

	// Same state again: deduped, no publish.
	n := len(memo.Messages())
	slot.PublishState(snap)
	if got := len(memo.Messages()); got != n {
		t.Fatalf("identical snapshot republished: %d msgs, want %d", got, n)
	}

	// Changed state: published.
	snap2 := *snap
	snap2.Az = 200
	slot.PublishState(&snap2)
	if got := len(memo.Messages()); got != n+1 {
		t.Fatalf("changed snapshot not published: %d msgs, want %d", got, n+1)
	}

	// NaN positions become JSON null.
	empty := &control.Snapshot{Az: nan(), El: nan(), PhysAz: nan(), PhysEl: nan(),
		TargetAz: nan(), TargetEl: nan()}
	slot.PublishState(empty)
	msg = memo.Last(testConfig().StateTopic())
	var raw map[string]any
	if err := json.Unmarshal(msg.Payload, &raw); err != nil {
		t.Fatalf("empty snapshot JSON: %v", err)
	}
	for _, k := range []string{"az", "el", "phys_az", "phys_el"} {
		if v, ok := raw[k]; !ok || v != nil {
			t.Errorf("%s = %v, want null", k, v)
		}
	}
	if raw["link"] != "down" {
		t.Errorf("link = %v with DeviceOnline=false, want down", raw["link"])
	}
}

func TestCmdStopOnly(t *testing.T) {
	memo := NewMemoPublisher()
	rec := &recordingSubmit{}
	slot := NewSlot(testConfig(), memo, nilLogger{}, rec.submit, nil)

	// The one accepted action.
	slot.HandleCmd([]byte(`{"action":"stop"}`))
	if ints := rec.intents(); len(ints) != 1 {
		t.Fatalf("stop not submitted: %v", ints)
	}

	// Everything else is logged and ignored — especially motion and arming.
	for _, payload := range []string{
		`{"action":"set_pos","value":"100"}`,
		`{"action":"arm","value":"30"}`,
		`{"action":"goto"}`,
		`{"action":""}`,
		`{garbage}`,
		``,
	} {
		slot.HandleCmd([]byte(payload))
	}
	if ints := rec.intents(); len(ints) != 1 {
		t.Fatalf("non-stop commands submitted: %v", ints)
	}
}

func nan() float64 { return math.NaN() }
