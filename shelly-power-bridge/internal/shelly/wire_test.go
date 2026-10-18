package shelly

import "testing"

func TestWireForGen1(t *testing.T) {
	w, err := WireFor(Gen1, "shellyplug-s-A71704")
	if err != nil {
		t.Fatal(err)
	}
	if w.StatusTopic != "shellies/shellyplug-s-A71704/relay/0" ||
		w.OnlineTopic != "shellies/shellyplug-s-A71704/online" ||
		w.CmdTopic != "shellies/shellyplug-s-A71704/relay/0/command" {
		t.Errorf("gen1 topics = %+v", w)
	}
	for payload, want := range map[string]string{"on": "on", "off": "off", "overpower": "off"} {
		got, err := w.ParseStatus([]byte(payload))
		if err != nil || got != want {
			t.Errorf("parse %q = %q, %v; want %q", payload, got, err, want)
		}
	}
	if _, err := w.ParseStatus([]byte(`{"ison":true}`)); err == nil {
		t.Error("gen1 parse must reject an unexpected payload")
	}
	if string(w.Command(true)) != "on" || string(w.Command(false)) != "off" {
		t.Error("gen1 command payloads must be on/off")
	}
}

func TestWireForGen2Default(t *testing.T) {
	w, err := WireFor("", "shellyplugsg3-e4b063d86c98")
	if err != nil {
		t.Fatal(err)
	}
	if w.StatusTopic != "shellyplugsg3-e4b063d86c98/status/switch:0" ||
		w.OnlineTopic != "shellyplugsg3-e4b063d86c98/online" ||
		w.CmdTopic != "shellyplugsg3-e4b063d86c98/rpc" {
		t.Errorf("gen2 topics = %+v", w)
	}
	if got, err := w.ParseStatus([]byte(`{"output":false}`)); err != nil || got != "off" {
		t.Errorf("gen2 parse = %q, %v", got, err)
	}
}

func TestWireForUnknown(t *testing.T) {
	if _, err := WireFor("gen9", "x"); err == nil {
		t.Error("unknown generation must be an error")
	}
}
