package expose

import (
	"testing"
)

// radioMeta is a representative /meta payload for the radio slot, carrying an expose block
// with number/enum(boolean)/enum-writable/boolean fields and an action.
const radioMeta = `{
  "schema": "1.0",
  "role": "radio",
  "link": "flexradio",
  "location": "bauwagen",
  "host": "shari",
  "device": {"model": "FLEX-8400", "serial": "SN123", "firmware": "3.8.19"},
  "capabilities": {
    "bands": ["6m","10m","20m","40m","80m"],
    "modes": ["cw","usb","lsb","am","fm","data"]
  },
  "expose": {
    "device": {"name": "FLEX-8400", "model": "FLEX-8400", "manufacturer": "FlexRadio Systems", "sw_version": "3.8.19", "area": "Radio shack"},
    "fields": [
      {"key": "freq_hz", "name": "Frequency", "type": "number", "unit": "Hz", "class": "frequency", "state_class": "measurement"},
      {"key": "band",    "name": "Band", "type": "enum", "options_ref": "bands"},
      {"key": "mode",    "name": "Mode", "type": "enum", "options_ref": "modes", "writable": true,
       "command": {"action": "mode", "value_key": "value", "value_type": "string"}},
      {"key": "tx",      "name": "Transmitting", "type": "boolean", "on": "tx", "off": "rx"}
    ],
    "actions": [
      {"key": "retract", "name": "Retract", "command": {"action": "retract"}}
    ]
  }
}`

func TestParseRadio(t *testing.T) {
	m, err := Parse("muehle/hf/radio/meta", []byte(radioMeta))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Addr != "muehle/hf/radio" {
		t.Errorf("addr = %q", m.Addr)
	}
	if m.Site != "muehle" || m.Station != "hf" || m.Slot != "radio" {
		t.Errorf("site/station/slot = %q/%q/%q", m.Site, m.Station, m.Slot)
	}
	if m.Role != "radio" || m.Schema != "1.0" {
		t.Errorf("role/schema = %q/%q", m.Role, m.Schema)
	}
	if m.Device.Model != "FLEX-8400" || m.Device.Serial != "SN123" {
		t.Errorf("device = %+v", m.Device)
	}
	if m.Expose == nil {
		t.Fatal("expose is nil")
	}
	if len(m.Expose.Fields) != 4 {
		t.Fatalf("fields = %d, want 4", len(m.Expose.Fields))
	}
	mode := m.Expose.Fields[2]
	if !mode.Writable || mode.Command == nil || mode.Command.Action != "mode" {
		t.Errorf("mode field = %+v", mode)
	}
	if len(m.Expose.Actions) != 1 || m.Expose.Actions[0].Key != "retract" {
		t.Errorf("actions = %+v", m.Expose.Actions)
	}
}

func TestParseNoExpose(t *testing.T) {
	const noExpose = `{"schema":"1.0","role":"pa","link":"acom","capabilities":{"temp":true}}`
	m, err := Parse("muehle/hf/pa/meta", []byte(noExpose))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Expose != nil {
		t.Errorf("expose = %+v, want nil", m.Expose)
	}
	if m.Role != "pa" {
		t.Errorf("role = %q", m.Role)
	}
}

func TestParseExplicitNullExpose(t *testing.T) {
	const nullExpose = `{"schema":"1.0","role":"pa","expose":null}`
	m, err := Parse("muehle/hf/pa/meta", []byte(nullExpose))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Expose != nil {
		t.Errorf("expose = %+v, want nil for explicit null", m.Expose)
	}
}

func TestParseBadSchema(t *testing.T) {
	const bad = `{"schema":"2.0","role":"radio"}`
	if _, err := Parse("muehle/hf/radio/meta", []byte(bad)); err == nil {
		t.Fatal("expected error for schema 2.0")
	}
}

func TestParseMissingRole(t *testing.T) {
	const bad = `{"schema":"1.0"}`
	if _, err := Parse("muehle/hf/radio/meta", []byte(bad)); err == nil {
		t.Fatal("expected error for missing role")
	}
}

func TestParseBadMetaTopic(t *testing.T) {
	if _, err := Parse("muehle/hf/radio", []byte(radioMeta)); err == nil {
		t.Fatal("expected error for topic without /meta")
	}
}

func TestParseMalformedJSON(t *testing.T) {
	if _, err := Parse("muehle/hf/radio/meta", []byte("{not json")); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestAddrFromMetaTopic(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"muehle/hf/radio/meta", "muehle/hf/radio", false},
		{"/muehle/hf/radio/meta", "muehle/hf/radio", false}, // leading slash tolerated
		{"muehle/hf/radio", "", true},                       // no /meta
		{"muehle/hf/radio/state", "", true},                 // wrong plane
		{"muehle/hf/meta", "", true},                        // too few segments
	}
	for _, c := range cases {
		got, err := AddrFromMetaTopic(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("AddrFromMetaTopic(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("AddrFromMetaTopic(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("AddrFromMetaTopic(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCapStringList(t *testing.T) {
	caps := map[string]any{
		"modes":   []string{"cw", "usb"},
		"ports":   []any{float64(1), float64(2), float64(3)},
		"scalars": "not-a-list",
	}
	if got := CapStringList(caps, "modes"); len(got) != 2 || got[0] != "cw" {
		t.Errorf("modes = %v", got)
	}
	if got := CapStringList(caps, "ports"); len(got) != 3 || got[1] != "2" {
		t.Errorf("ports = %v, want [1 2 3] as strings", got)
	}
	if got := CapStringList(caps, "scalars"); got != nil {
		t.Errorf("scalars = %v, want nil", got)
	}
	if got := CapStringList(caps, "missing"); got != nil {
		t.Errorf("missing = %v, want nil", got)
	}
	if got := CapStringList(nil, "modes"); got != nil {
		t.Errorf("nil caps = %v, want nil", got)
	}
}

func TestIsAddr(t *testing.T) {
	if !IsAddr("muehle/hf/radio") {
		t.Error("IsAddr should be true for 3-segment addr")
	}
	if IsAddr("muehle/hf/radio/meta") {
		t.Error("IsAddr should be false for 4-segment topic")
	}
}
