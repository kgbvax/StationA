package bridge

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"tgxl-tuner-bridge/internal/tgxl"
)

type nopLog struct{}

func (nopLog) Infof(string, ...any)  {}
func (nopLog) Warnf(string, ...any)  {}
func (nopLog) Errorf(string, ...any) {}
func (nopLog) Debugf(string, ...any) {}

type fakeCmd struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeCmd) add(s string) error {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
	return nil
}
func (f *fakeCmd) SetInline(v bool) error {
	if v {
		return f.add("inline:true")
	}
	return f.add("inline:false")
}
func (f *fakeCmd) SetOperate(v bool) error {
	if v {
		return f.add("operate:true")
	}
	return f.add("operate:false")
}
func (f *fakeCmd) SetAntenna(n int) error { return f.add("ant:" + string(rune('0'+n))) }
func (f *fakeCmd) Tune() error            { return f.add("tune") }

func newBridge(c Commander) (*Bridge, *MemoPublisher) {
	p := NewMemoPublisher()
	return New(Config{Site: "muehle", Station: "hf", Slot: "tuner", DeviceModel: "Tuner Genius XL",
		DeviceLink: "ethernet", Commander: c}, p, nopLog{}), p
}

func TestHandleCommand(t *testing.T) {
	f := &fakeCmd{}
	b, _ := newBridge(f)
	for _, p := range []string{
		`{"action":"set_inline","value":true}`,
		`{"action":"set_inline","value":false}`,
		`{"action":"set_operate","value":false}`,
		`{"action":"set_antenna","value":3}`,
		`{"action":"tune","value":"auto"}`,
		`{"action":"tune","value":"mem"}`,
		`{"action":"tune","value":"full"}`,
		// rejected:
		`{"action":"set_inline","value":"yes"}`,
		`{"action":"set_inline"}`,
		`{"action":"set_antenna","value":4}`,
		`{"action":"set_antenna","value":0}`,
		`{"action":"tune","value":"bogus"}`,
		`{"action":"nope","value":1}`,
		`not json`,
	} {
		b.HandleCommand([]byte(p))
	}
	got := strings.Join(f.calls, ",")
	want := "inline:true,inline:false,operate:false,ant:3,tune,tune,tune"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestNilCommanderIgnored(t *testing.T) {
	b, _ := newBridge(nil)
	b.HandleCommand([]byte(`{"action":"tune","value":"auto"}`))
}

func TestMetaShape(t *testing.T) {
	b, p := newBridge(nil)
	b.SetDeviceInfo(tgxl.Info{Serial: "210387-1", Version: "1.1.8"})
	msgs := p.Messages()
	if len(msgs) != 1 || !msgs[0].Retained || msgs[0].Topic != "muehle/hf/tuner/meta" {
		t.Fatalf("%+v", msgs)
	}
	var m map[string]any
	if err := json.Unmarshal(msgs[0].Payload, &m); err != nil {
		t.Fatal(err)
	}
	if m["role"] != "tuner" {
		t.Fatalf("role %v", m["role"])
	}
	dev := m["device"].(map[string]any)
	if dev["model"] != "Tuner Genius XL" || dev["serial"] != "210387-1" || dev["firmware"] != "1.1.8" {
		t.Fatalf("device %v", dev)
	}
	caps := m["capabilities"].(map[string]any)
	if caps["antennas"].(float64) != 3 || caps["tune_modes"].([]any)[0] != "auto" {
		t.Fatalf("caps %v", caps)
	}
	exp := m["expose"].(map[string]any)
	keys := map[string]bool{}
	for _, f := range exp["fields"].([]any) {
		keys[f.(map[string]any)["key"].(string)] = true
	}
	// the /state fields the consumers read must be advertised
	for _, k := range []string{"inline", "swr", "fwd", "settling", "fault", "device_online"} {
		if !keys[k] {
			t.Errorf("expose misses %q", k)
		}
	}
	// same info again: no republish
	b.SetDeviceInfo(tgxl.Info{Serial: "210387-1", Version: "1.1.8"})
	if len(p.Messages()) != 1 {
		t.Fatal("unchanged info must not republish meta")
	}
}

func TestStateDedupAndFields(t *testing.T) {
	b, p := newBridge(nil)
	st := tgxl.State{DeviceOnline: true, Operate: true, Inline: true, Antenna: 2, SWR: 1.2}
	b.HandleTelemetry(st)
	b.HandleTelemetry(st)
	if n := len(p.Messages()); n != 1 {
		t.Fatalf("dedup: %d publishes", n)
	}
	st.Fwd = 50
	b.HandleTelemetry(st)
	msgs := p.Messages()
	if len(msgs) != 2 || msgs[1].Topic != "muehle/hf/tuner/state" || !msgs[1].Retained {
		t.Fatalf("%+v", msgs)
	}
	var m map[string]any
	_ = json.Unmarshal(msgs[1].Payload, &m)
	// settling and device_online are read by the PLC firmware: never omitted
	for _, k := range []string{"settling", "device_online", "inline", "swr", "fwd", "ts"} {
		if _, ok := m[k]; !ok {
			t.Errorf("state lacks %q: %s", k, msgs[1].Payload)
		}
	}
	if _, ok := m["l_uh"]; ok {
		t.Error("l_uh must be gone")
	}
}

func TestRepublishBeforeFirstTelemetryIsNoop(t *testing.T) {
	b, p := newBridge(nil)
	b.RepublishState()
	if len(p.Messages()) != 0 {
		t.Fatal("must not publish an all-zero snapshot")
	}
	b.HandleTelemetry(tgxl.State{DeviceOnline: true})
	p.Reset()
	b.RepublishState()
	if len(p.Messages()) != 1 {
		t.Fatal("want republish after first telemetry")
	}
}

func TestSetDeviceOnlineFalseClearsSettling(t *testing.T) {
	b, p := newBridge(nil)
	b.HandleTelemetry(tgxl.State{DeviceOnline: true, Settling: true})
	b.SetDeviceOnline(false, "tgxl: gone")
	msgs := p.Messages()
	var m map[string]any
	_ = json.Unmarshal(msgs[len(msgs)-1].Payload, &m)
	if m["device_online"] != false || m["settling"] != false || m["error"] != "tgxl: gone" {
		t.Fatalf("%v", m)
	}
}
