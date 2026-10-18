package bridge

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type pubMsg struct {
	topic    string
	retained bool
	payload  []byte
}

type fakePub struct {
	msgs []pubMsg
	err  error
}

func (f *fakePub) Publish(topic string, retained bool, payload []byte) error {
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, pubMsg{topic, retained, append([]byte(nil), payload...)})
	return nil
}

func (f *fakePub) lastState(t *testing.T) State {
	t.Helper()
	for i := len(f.msgs) - 1; i >= 0; i-- {
		if f.msgs[i].topic == "muehle/power/shack-pc/state" {
			var st State
			if err := json.Unmarshal(f.msgs[i].payload, &st); err != nil {
				t.Fatal(err)
			}
			return st
		}
	}
	t.Fatal("no /state published")
	return State{}
}

type harness struct {
	b        *Bridge
	pub      *fakePub
	idle     time.Duration
	idleErr  error
	suspends int
	suspErr  error
}

func newHarness() *harness {
	h := &harness{pub: &fakePub{}}
	h.b = New(Config{
		Site: "muehle", Station: "power", Slot: "shack-pc",
		Location: "bauwagen", Host: "bwpc",
		DeviceModel: "Windows PC", DeviceSerial: "bwpc", Link: "ethernet",
		MinUserIdle: 30 * time.Minute,
	}, h.pub,
		func() (time.Duration, error) { return h.idle, h.idleErr },
		func() error { h.suspends++; return h.suspErr },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h
}

var sleepCmd = []byte(`{"action":"sleep"}`)

func TestDecide(t *testing.T) {
	min := 30 * time.Minute
	if err := Decide(29*time.Minute+59*time.Second, min); !errors.Is(err, ErrUserActive) {
		t.Errorf("idle < min: got %v, want ErrUserActive", err)
	}
	if err := Decide(min, min); err != nil {
		t.Errorf("idle == min: got %v, want nil", err)
	}
	if err := Decide(2*time.Hour, min); err != nil {
		t.Errorf("idle > min: got %v, want nil", err)
	}
}

func TestSleepRefusedWhenUserActive(t *testing.T) {
	h := newHarness()
	h.idle = 5 * time.Minute
	h.b.HandleCommand(false, sleepCmd)
	if h.suspends != 0 {
		t.Fatalf("suspend called %d times, want 0", h.suspends)
	}
	st := h.pub.lastState(t)
	if st.Error != "user active, sleep refused" {
		t.Errorf("state.error = %q", st.Error)
	}
	if st.UserIdleS != 300 || st.Power != "awake" || !st.DeviceOnline {
		t.Errorf("state = %+v", st)
	}
}

func TestSleepWhenIdle(t *testing.T) {
	h := newHarness()
	h.idle = 31 * time.Minute
	h.b.HandleCommand(false, sleepCmd)
	if h.suspends != 1 {
		t.Fatalf("suspend called %d times, want 1", h.suspends)
	}
	if st := h.pub.lastState(t); st.Error != "" {
		t.Errorf("state.error = %q, want empty", st.Error)
	}
}

func TestRetainedCmdIgnored(t *testing.T) {
	h := newHarness()
	h.idle = 10 * time.Hour
	h.b.HandleCommand(true, sleepCmd)
	if h.suspends != 0 {
		t.Fatalf("retained cmd triggered suspend")
	}
	if len(h.pub.msgs) != 0 {
		t.Errorf("retained cmd published %d messages", len(h.pub.msgs))
	}
}

func TestUnknownActionIgnored(t *testing.T) {
	h := newHarness()
	h.idle = 10 * time.Hour
	for _, p := range []string{`{"action":"shutdown"}`, `{"action":""}`, `not json`, ``} {
		h.b.HandleCommand(false, []byte(p))
	}
	if h.suspends != 0 {
		t.Fatalf("suspend called for a non-sleep cmd")
	}
	if len(h.pub.msgs) != 0 {
		t.Errorf("non-sleep cmd published %d messages", len(h.pub.msgs))
	}
}

func TestErrorClearedAfterSuccess(t *testing.T) {
	h := newHarness()
	h.idle = time.Minute
	h.b.HandleCommand(false, sleepCmd)
	if st := h.pub.lastState(t); st.Error == "" {
		t.Fatal("refusal did not set state.error")
	}
	h.idle = time.Hour
	h.b.HandleCommand(false, sleepCmd)
	if h.suspends != 1 {
		t.Fatalf("suspend called %d times, want 1", h.suspends)
	}
	if st := h.pub.lastState(t); st.Error != "" {
		t.Errorf("state.error = %q after successful sleep, want empty", st.Error)
	}
}

func TestRefusalErrorClearedOnNextRefresh(t *testing.T) {
	h := newHarness()
	h.idle = time.Minute
	h.b.HandleCommand(false, sleepCmd)
	h.b.PublishState()
	if st := h.pub.lastState(t); st.Error != "" {
		t.Errorf("state.error = %q on the refresh after a refusal, want empty", st.Error)
	}
}

func TestRefusalErrorKeptWhilePublishFails(t *testing.T) {
	h := newHarness()
	h.idle = time.Minute
	h.pub.err = ErrNotConnected
	h.b.HandleCommand(false, sleepCmd)
	h.pub.err = nil
	h.b.PublishState()
	if st := h.pub.lastState(t); st.Error != "user active, sleep refused" {
		t.Errorf("state.error = %q, want the refusal carried to the first successful publish", st.Error)
	}
}

func TestSuspendFailureSetsError(t *testing.T) {
	h := newHarness()
	h.idle = time.Hour
	h.suspErr = errors.New("no S3")
	h.b.HandleCommand(false, sleepCmd)
	if st := h.pub.lastState(t); st.Error != "sleep failed: no S3" {
		t.Errorf("state.error = %q", st.Error)
	}
}

func TestIdleSourceErrorRefuses(t *testing.T) {
	h := newHarness()
	h.idleErr = errors.New("boom")
	h.b.HandleCommand(false, sleepCmd)
	if h.suspends != 0 {
		t.Fatal("suspend called without a known idle time")
	}
	if st := h.pub.lastState(t); st.Error == "" {
		t.Error("state.error empty")
	}
}

func TestStateWireShape(t *testing.T) {
	h := newHarness()
	h.idle = 90*time.Second + 400*time.Millisecond
	h.b.PublishState()
	m := h.pub.msgs[len(h.pub.msgs)-1]
	if !m.retained {
		t.Error("/state not retained")
	}
	var raw map[string]any
	if err := json.Unmarshal(m.payload, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ts", "power", "user_idle_s", "device_online"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing key %q in %s", k, m.payload)
		}
	}
	if _, ok := raw["error"]; ok {
		t.Errorf("error key present without an error: %s", m.payload)
	}
	if raw["user_idle_s"] != float64(90) {
		t.Errorf("user_idle_s = %v, want 90", raw["user_idle_s"])
	}
}

func TestMeta(t *testing.T) {
	h := newHarness()
	h.b.PublishMeta()
	m := h.pub.msgs[0]
	if m.topic != "muehle/power/shack-pc/meta" || !m.retained {
		t.Fatalf("meta topic/retain = %s/%v", m.topic, m.retained)
	}
	var meta struct {
		Role         string `json:"role"`
		Host         string `json:"host"`
		Capabilities struct {
			Actions []string `json:"actions"`
		} `json:"capabilities"`
		Expose struct {
			Actions []struct {
				Command map[string]string `json:"command"`
			} `json:"actions"`
		} `json:"expose"`
	}
	if err := json.Unmarshal(m.payload, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Role != "pc-power" || meta.Host != "bwpc" {
		t.Errorf("role/host = %q/%q", meta.Role, meta.Host)
	}
	if len(meta.Capabilities.Actions) != 1 || meta.Capabilities.Actions[0] != "sleep" {
		t.Errorf("capabilities.actions = %v", meta.Capabilities.Actions)
	}
	if len(meta.Expose.Actions) != 1 || meta.Expose.Actions[0].Command["action"] != "sleep" {
		t.Errorf("expose.actions = %+v", meta.Expose.Actions)
	}
}
