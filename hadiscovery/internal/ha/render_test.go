package ha

import (
	"encoding/json"
	"strings"
	"testing"

	"hadiscovery/internal/expose"
)

// metaFixture builds a SlotMeta for "muehle/hf/radio" with the given fields, an expose
// device block, and a capabilities map (for options_ref resolution).
func metaFixture(fields []expose.Field, actions []expose.Action, caps map[string]any) expose.SlotMeta {
	return expose.SlotMeta{
		Addr:         "muehle/hf/radio",
		Site:         "muehle",
		Station:      "hf",
		Slot:         "radio",
		Schema:       "1.0",
		Role:         "radio",
		Capabilities: caps,
		Expose: &expose.Expose{
			Device: &expose.DeviceBlock{
				Name:         "FLEX-8400",
				Model:        "FLEX-8400",
				Manufacturer: "FlexRadio Systems",
				SWVersion:    "3.8.19",
				Area:         "Radio shack",
			},
			Fields:  fields,
			Actions: actions,
		},
	}
}

// decode unmarshals an Entity's discovery payload into the same struct the renderer uses,
// so tests assert on the actual on-the-wire JSON keys, not internal fields.
func decode(t *testing.T, e Entity) discoveryPayload {
	t.Helper()
	var p discoveryPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("unmarshal %s: %v", e.Topic, err)
	}
	return p
}

func TestNodeID(t *testing.T) {
	got := NodeID(expose.SlotMeta{Site: "muehle", Station: "hf", Slot: "radio"})
	if got != "muehle-hf-radio" {
		t.Fatalf("NodeID = %q, want muehle-hf-radio", got)
	}
}

func TestConfigTopic(t *testing.T) {
	got := ConfigTopic("homeassistant", "sensor", "muehle-hf-radio", "freq_hz")
	want := "homeassistant/sensor/muehle-hf-radio/freq_hz/config"
	if got != want {
		t.Fatalf("ConfigTopic = %q, want %q", got, want)
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"freq_hz":  "freq_hz",
		"Freq.Hz":  "freq_hz",
		"ant-ctrl": "ant-ctrl",
		"a/b c":    "a_b_c",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNumberReadOnly renders a read-only number field (sensor) and asserts the unit-derived
// device_class, state_class, value_template, state_topic, and the shared envelope.
func TestNumberReadOnly(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "freq_hz", Name: "Frequency", Type: "number", Unit: "Hz",
		Class: "frequency", StateClass: "measurement",
	}}, nil, map[string]any{"bands": []string{"40m"}}))
	if len(ents) != 1 {
		t.Fatalf("got %d entities, want 1", len(ents))
	}
	e := ents[0]
	if e.Component != "sensor" {
		t.Errorf("component = %q, want sensor", e.Component)
	}
	if e.Topic != "homeassistant/sensor/muehle-hf-radio/freq_hz/config" {
		t.Errorf("topic = %q", e.Topic)
	}
	p := decode(t, e)
	if p.StateTopic != "muehle/hf/radio/state" {
		t.Errorf("state_topic = %q", p.StateTopic)
	}
	if p.ValueTemplate != "{{ value_json.freq_hz }}" {
		t.Errorf("value_template = %q", p.ValueTemplate)
	}
	if p.UnitOfMeasurement != "Hz" {
		t.Errorf("unit = %q", p.UnitOfMeasurement)
	}
	if p.DeviceClass != "frequency" {
		t.Errorf("device_class = %q", p.DeviceClass)
	}
	if p.StateClass != "measurement" {
		t.Errorf("state_class = %q", p.StateClass)
	}
	if p.CommandTopic != "" {
		t.Errorf("read-only sensor must have no command_topic, got %q", p.CommandTopic)
	}
	assertEnvelope(t, p, "muehle-hf-radio_freq_hz", "Frequency")
}

// TestNumberWritable renders a writable number (HA number component) with min/max/step and
// a command_template rendered from the command descriptor.
func TestNumberWritable(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "freq_hz", Name: "Frequency", Type: "number", Unit: "Hz", Class: "frequency",
		Writable: true, Min: 1800000, Max: 54000000, Step: 1000,
		Command: &expose.Command{Action: "frequency", ValueKey: "freq_hz", ValueType: "int"},
	}}, nil, nil))
	if len(ents) != 1 {
		t.Fatalf("got %d entities, want 1", len(ents))
	}
	e := ents[0]
	if e.Component != "number" {
		t.Fatalf("component = %q, want number", e.Component)
	}
	p := decode(t, e)
	if p.CommandTopic != "muehle/hf/radio/cmd" {
		t.Errorf("command_topic = %q", p.CommandTopic)
	}
	wantTpl := `{"action":"frequency","freq_hz":{{ value | int }}}`
	if p.CommandTemplate != wantTpl {
		t.Errorf("command_template = %q, want %q", p.CommandTemplate, wantTpl)
	}
	if p.Min != float64(1800000) || p.Max != float64(54000000) || p.Step != float64(1000) {
		t.Errorf("min/max/step = %v/%v/%v", p.Min, p.Max, p.Step)
	}
	if p.NumberMode != "box" {
		t.Errorf("mode = %q, want box", p.NumberMode)
	}
	if !p.Retain {
		t.Errorf("writable number must be retained (keeps the retained /cmd tracking the latest operator intent — see render.go)")
	}
}

// TestEnumReadOnly renders a read-only enum as a sensor.
func TestEnumReadOnly(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "band", Name: "Band", Type: "enum", OptionsRef: "bands",
	}}, nil, map[string]any{"bands": []string{"40m", "20m"}}))
	if len(ents) != 1 || ents[0].Component != "sensor" {
		t.Fatalf("got %+v, want one sensor", ents)
	}
	p := decode(t, ents[0])
	if len(p.Options) != 0 {
		t.Errorf("read-only enum sensor must have no options, got %v", p.Options)
	}
}

// TestEnumWritable renders a writable enum as a select, resolving options_ref against the
// capabilities map, and rendering the command_template.
func TestEnumWritable(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "mode", Name: "Mode", Type: "enum", OptionsRef: "modes", Writable: true,
		Command: &expose.Command{Action: "mode", ValueKey: "value", ValueType: "string"},
	}}, nil, map[string]any{"modes": []string{"cw", "usb", "lsb"}}))
	if len(ents) != 1 || ents[0].Component != "select" {
		t.Fatalf("got %+v, want one select", ents)
	}
	p := decode(t, ents[0])
	wantOpts := []string{"cw", "usb", "lsb"}
	if len(p.Options) != len(wantOpts) {
		t.Fatalf("options = %v, want %v", p.Options, wantOpts)
	}
	for i, o := range wantOpts {
		if p.Options[i] != o {
			t.Errorf("options[%d] = %q, want %q", i, p.Options[i], o)
		}
	}
	wantTpl := `{"action":"mode","value":"{{ value }}"}`
	if p.CommandTemplate != wantTpl {
		t.Errorf("command_template = %q, want %q", p.CommandTemplate, wantTpl)
	}
	if !p.Retain {
		t.Errorf("writable select must be retained (keeps the retained /cmd tracking the latest operator intent — see render.go)")
	}
}

// TestEnumWritableInlineOptions uses inline options (no options_ref).
func TestEnumWritableInlineOptions(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "selected", Name: "Selected", Type: "enum",
		Options: []string{"off", "p1", "p2"}, Writable: true,
		Command: &expose.Command{ValueKey: "select", ValueType: "string"},
	}}, nil, nil))
	if len(ents) != 1 || ents[0].Component != "select" {
		t.Fatalf("got %+v, want one select", ents)
	}
	p := decode(t, ents[0])
	wantTpl := `{"select":"{{ value }}"}`
	if p.CommandTemplate != wantTpl {
		t.Errorf("command_template = %q, want %q (no action, value_key only)", p.CommandTemplate, wantTpl)
	}
	if len(p.Options) != 3 || p.Options[1] != "p1" {
		t.Errorf("options = %v", p.Options)
	}
}

// TestEnumWritableMissingCommand is unrenderable (writable enum with no command) and must
// be skipped (empty component), not emitted as a broken select.
func TestEnumWritableMissingCommand(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "mode", Name: "Mode", Type: "enum", OptionsRef: "modes", Writable: true,
	}}, nil, map[string]any{"modes": []string{"cw"}}))
	if len(ents) != 0 {
		t.Fatalf("writable enum without command must be skipped, got %+v", ents)
	}
}

// TestEnumWritableEmptyOptions is unrenderable: a writable enum whose options resolve to
// empty (options_ref points at a missing/empty capabilities key, no inline options) must be
// skipped. Emitting it would publish a select with no `options` key, which HA rejects
// (CONF_OPTIONS is vol.Required in the mqtt select discovery schema).
func TestEnumWritableEmptyOptions(t *testing.T) {
	// options_ref "modes" but capabilities has no "modes" key.
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "mode", Name: "Mode", Type: "enum", OptionsRef: "modes", Writable: true,
		Command: &expose.Command{Action: "mode", ValueKey: "value", ValueType: "string"},
	}}, nil, map[string]any{"bands": []string{"40m"}}))
	if len(ents) != 0 {
		t.Fatalf("writable enum with empty options must be skipped, got %+v", ents)
	}
}

// TestEnumWritableEmptyOptionsList covers the empty-list case: options_ref resolves to an
// empty slice (present but empty capabilities key). Still no valid select.
func TestEnumWritableEmptyOptionsList(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "mode", Name: "Mode", Type: "enum", OptionsRef: "modes", Writable: true,
		Command: &expose.Command{Action: "mode", ValueKey: "value", ValueType: "string"},
	}}, nil, map[string]any{"modes": []string{}}))
	if len(ents) != 0 {
		t.Fatalf("writable enum with empty options list must be skipped, got %+v", ents)
	}
}

// TestBooleanDefault renders a boolean whose state holds a real bool (no on/off payloads):
// value_template maps truthiness to ON/OFF.
func TestBooleanDefault(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "tuning", Name: "Tuning", Type: "boolean",
	}}, nil, nil))
	if len(ents) != 1 || ents[0].Component != "binary_sensor" {
		t.Fatalf("got %+v, want one binary_sensor", ents)
	}
	p := decode(t, ents[0])
	if p.ValueTemplate != "{{ 'ON' if value_json.tuning else 'OFF' }}" {
		t.Errorf("value_template = %q", p.ValueTemplate)
	}
	if p.PayloadOn != "ON" || p.PayloadOff != "OFF" {
		t.Errorf("payload_on/off = %q/%q", p.PayloadOn, p.PayloadOff)
	}
}

// TestBooleanCustomPayloads renders a boolean whose state holds string payloads (tx/rx):
// value_template passes the value through, payload_on/off are the state strings.
func TestBooleanCustomPayloads(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "tx", Name: "Transmitting", Type: "boolean", On: "tx", Off: "rx",
	}}, nil, nil))
	if len(ents) != 1 || ents[0].Component != "binary_sensor" {
		t.Fatalf("got %+v, want one binary_sensor", ents)
	}
	p := decode(t, ents[0])
	if p.ValueTemplate != "{{ value_json.tx }}" {
		t.Errorf("value_template = %q, want pass-through", p.ValueTemplate)
	}
	if p.PayloadOn != "tx" || p.PayloadOff != "rx" {
		t.Errorf("payload_on/off = %q/%q, want tx/rx", p.PayloadOn, p.PayloadOff)
	}
}

// TestAction renders a one-shot button: payload_press is the static command JSON, no
// value_template, command_topic set.
func TestAction(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture(nil, []expose.Action{{
		Key: "retract", Name: "Retract", Command: &expose.Command{Action: "retract"},
	}}, nil))
	if len(ents) != 1 || ents[0].Component != "button" {
		t.Fatalf("got %+v, want one button", ents)
	}
	e := ents[0]
	if e.Topic != "homeassistant/button/muehle-hf-radio/retract/config" {
		t.Errorf("topic = %q", e.Topic)
	}
	p := decode(t, e)
	if p.PayloadPress != `{"action":"retract"}` {
		t.Errorf("payload_press = %q", p.PayloadPress)
	}
	if p.ValueTemplate != "" {
		t.Errorf("button must have no value_template, got %q", p.ValueTemplate)
	}
	if p.CommandTopic != "muehle/hf/radio/cmd" {
		t.Errorf("command_topic = %q", p.CommandTopic)
	}
}

// TestNilExpose returns nil (engine handles the no-expose diagnostic).
func TestNilExpose(t *testing.T) {
	ents := Render("homeassistant", "", expose.SlotMeta{Addr: "muehle/hf/radio", Role: "radio"})
	if ents != nil {
		t.Fatalf("got %+v, want nil for no expose", ents)
	}
}

// TestUnitToDeviceClass locks the unit→device_class map.
func TestUnitToDeviceClass(t *testing.T) {
	cases := map[string]string{
		"Hz":   "frequency",
		"°C":   "temperature",
		"degC": "temperature",
		"W":    "power",
		"V":    "voltage",
		"A":    "current",
		"dBm":  "signal_strength",
		"%":    "", // no mapping
	}
	for unit, want := range cases {
		if got := unitToDeviceClass(unit); got != want {
			t.Errorf("unitToDeviceClass(%q) = %q, want %q", unit, got, want)
		}
	}
}

// TestDeviceBlockFallback verifies expose.device falls back to meta.device when fields are
// absent, and that a logic slot (no device) gets a role+addr name.
func TestDeviceBlockFallback(t *testing.T) {
	t.Run("expose_device_wins", func(t *testing.T) {
		m := metaFixture(nil, nil, nil)
		m.Device = expose.MetaDevice{Model: "OtherModel", Firmware: "1.0"}
		d := deviceBlock(m, "muehle-hf-radio", "")
		if d.Model != "FLEX-8400" {
			t.Errorf("model = %q, want expose.device FLEX-8400", d.Model)
		}
		if d.SWVersion != "3.8.19" {
			t.Errorf("sw_version = %q", d.SWVersion)
		}
	})
	t.Run("logic_slot", func(t *testing.T) {
		m := expose.SlotMeta{Addr: "muehle/hf/discovery", Role: "discovery"}
		d := deviceBlock(m, "muehle-hf-discovery", "")
		if d.Name != "discovery muehle/hf/discovery" {
			t.Errorf("name = %q", d.Name)
		}
		if len(d.Identifiers) != 1 || d.Identifiers[0] != "muehle-hf-discovery" {
			t.Errorf("identifiers = %v", d.Identifiers)
		}
	})
}

// TestDeviceBlockArea covers the HA `suggested_area` fallback: a slot's own expose.device.area
// wins; when it is unset, the deployment-wide default area fills in; an empty default
// suppresses the field entirely (it is omitempty, so HA never sees it).
func TestDeviceBlockArea(t *testing.T) {
	t.Run("per_slot_area_wins", func(t *testing.T) {
		m := metaFixture(nil, nil, nil) // expose.device.area == "Radio shack"
		d := deviceBlock(m, "muehle-hf-radio", "Bauwagen")
		if d.SuggestedArea != "Radio shack" {
			t.Errorf("suggested_area = %q, want per-slot \"Radio shack\"", d.SuggestedArea)
		}
	})
	t.Run("default_fills_when_unset", func(t *testing.T) {
		m := metaFixture(nil, nil, nil)
		m.Expose.Device.Area = "" // slot names no area
		d := deviceBlock(m, "muehle-hf-radio", "Bauwagen")
		if d.SuggestedArea != "Bauwagen" {
			t.Errorf("suggested_area = %q, want deployment default \"Bauwagen\"", d.SuggestedArea)
		}
	})
	t.Run("logic_slot_gets_default", func(t *testing.T) {
		m := expose.SlotMeta{Addr: "muehle/hf/discovery", Role: "discovery"} // no device
		d := deviceBlock(m, "muehle-hf-discovery", "Bauwagen")
		if d.SuggestedArea != "Bauwagen" {
			t.Errorf("suggested_area = %q, want \"Bauwagen\" for a deviceless slot", d.SuggestedArea)
		}
	})
	t.Run("empty_default_suppresses", func(t *testing.T) {
		m := metaFixture(nil, nil, nil)
		m.Expose.Device.Area = ""
		d := deviceBlock(m, "muehle-hf-radio", "")
		if d.SuggestedArea != "" {
			t.Errorf("suggested_area = %q, want empty (omitempty -> omitted)", d.SuggestedArea)
		}
	})
}

// TestRenderAreaOnWire asserts the deployment default area reaches the discovery payload as
// `suggested_area` for a slot that does not name its own, and that a per-slot area overrides it.
func TestRenderAreaOnWire(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		m := metaFixture([]expose.Field{{Key: "freq_hz", Name: "Frequency", Type: "number"}}, nil, nil)
		m.Expose.Device.Area = "" // no per-slot area -> default applies
		ents := Render("homeassistant", "Bauwagen", m)
		p := decode(t, ents[0])
		if p.Device.SuggestedArea != "Bauwagen" {
			t.Errorf("device.suggested_area = %q, want \"Bauwagen\"", p.Device.SuggestedArea)
		}
	})
	t.Run("per_slot_override", func(t *testing.T) {
		m := metaFixture([]expose.Field{{Key: "freq_hz", Name: "Frequency", Type: "number"}}, nil, nil)
		// metaFixture sets area "Radio shack"; default must not override it.
		ents := Render("homeassistant", "Bauwagen", m)
		p := decode(t, ents[0])
		if p.Device.SuggestedArea != "Radio shack" {
			t.Errorf("device.suggested_area = %q, want per-slot \"Radio shack\"", p.Device.SuggestedArea)
		}
	})
}

// assertEnvelope checks the fields every entity shares: unique_id, name, availability,
// availability_mode, device identifiers, and origin.
func assertEnvelope(t *testing.T, p discoveryPayload, wantUniqueID, wantName string) {
	t.Helper()
	if p.UniqueID != wantUniqueID {
		t.Errorf("unique_id = %q, want %q", p.UniqueID, wantUniqueID)
	}
	if p.Name != wantName {
		t.Errorf("name = %q, want %q", p.Name, wantName)
	}
	if len(p.Availability) != 1 {
		t.Fatalf("availability len = %d, want 1", len(p.Availability))
	}
	a := p.Availability[0]
	if a.Topic != "muehle/hf/radio/status" || a.PayloadAvailable != "online" || a.PayloadNotAvailable != "offline" {
		t.Errorf("availability = %+v", a)
	}
	if p.AvailabilityMode != "all" {
		t.Errorf("availability_mode = %q, want all", p.AvailabilityMode)
	}
	if len(p.Device.Identifiers) != 1 || p.Device.Identifiers[0] != "muehle-hf-radio" {
		t.Errorf("device.identifiers = %v", p.Device.Identifiers)
	}
	if p.Origin.Name != "hadiscovery" {
		t.Errorf("origin.name = %q", p.Origin.Name)
	}
}

// TestValuePlaceholder locks the value_type coercion used in command_template.
func TestValuePlaceholder(t *testing.T) {
	cases := map[string]string{
		"int":    "{{ value | int }}",
		"float":  "{{ value | float }}",
		"string": `"{{ value }}"`,
		"":       `"{{ value }}"`,
	}
	for vt, want := range cases {
		if got := valuePlaceholder(vt); got != want {
			t.Errorf("valuePlaceholder(%q) = %q, want %q", vt, got, want)
		}
	}
}

// TestRenderFieldOrder verifies fields render before actions, in declared order (the engine
// relies on deterministic order for idempotent byte comparison).
func TestRenderFieldOrder(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture(
		[]expose.Field{
			{Key: "freq_hz", Name: "Frequency", Type: "number", Unit: "Hz"},
			{Key: "tx", Name: "TX", Type: "boolean"},
		},
		[]expose.Action{{Key: "retract", Name: "Retract", Command: &expose.Command{Action: "retract"}}},
		nil))
	if len(ents) != 3 {
		t.Fatalf("got %d entities, want 3", len(ents))
	}
	if ents[0].ObjectID != "freq_hz" || ents[1].ObjectID != "tx" || ents[2].ObjectID != "retract" {
		var order []string
		for _, e := range ents {
			order = append(order, e.ObjectID)
		}
		t.Errorf("order = %v, want [freq_hz tx retract]", order)
	}
}

// TestStringField renders a plain string field as a sensor with no unit/class.
func TestStringField(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "target", Name: "Target", Type: "string",
	}}, nil, nil))
	if len(ents) != 1 || ents[0].Component != "sensor" {
		t.Fatalf("got %+v, want one sensor", ents)
	}
	p := decode(t, ents[0])
	if p.UnitOfMeasurement != "" || p.DeviceClass != "" {
		t.Errorf("string sensor must have no unit/class, got %q/%q", p.UnitOfMeasurement, p.DeviceClass)
	}
	if !strings.Contains(p.ValueTemplate, "value_json.target") {
		t.Errorf("value_template = %q", p.ValueTemplate)
	}
}

// TestWritableNumberNoStateClass asserts that a writable number (HA number component) does
// not carry state_class, which HA's MQTT number discovery schema rejects.
func TestWritableNumberNoStateClass(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "freq_hz", Name: "Frequency", Type: "number", Unit: "Hz", Class: "frequency",
		Writable: true, Min: 1800000, Max: 54000000, Step: 1000,
		Command: &expose.Command{Action: "frequency", ValueKey: "freq_hz", ValueType: "int"},
	}}, nil, nil))
	if len(ents) != 1 || ents[0].Component != "number" {
		t.Fatalf("got %+v, want one number", ents)
	}
	p := decode(t, ents[0])
	if p.StateClass != "" {
		t.Errorf("writable number must not emit state_class, got %q", p.StateClass)
	}
}

// TestWritableNumberMissingCommand asserts that a writable number without a valid command
// descriptor is skipped rather than emitting a broken number entity.
func TestWritableNumberMissingCommand(t *testing.T) {
	cases := []struct {
		name string
		cmd  *expose.Command
	}{
		{"nil command", nil},
		{"action-only command", &expose.Command{Action: "frequency"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ents := Render("homeassistant", "", metaFixture([]expose.Field{{
				Key: "freq_hz", Name: "Frequency", Type: "number", Unit: "Hz",
				Writable: true, Min: 0, Max: 100, Step: 1,
				Command: c.cmd,
			}}, nil, nil))
			if len(ents) != 0 {
				t.Errorf("got %+v, want no entities for invalid writable number command", ents)
			}
		})
	}
}

// TestWritableEnumActionOnlyCommand asserts that a writable enum whose command lacks a
// value_key is skipped, because action-only commands are valid only for buttons.
func TestWritableEnumActionOnlyCommand(t *testing.T) {
	ents := Render("homeassistant", "", metaFixture([]expose.Field{{
		Key: "mode", Name: "Mode", Type: "enum", OptionsRef: "modes", Writable: true,
		Command: &expose.Command{Action: "mode"}, // missing value_key
	}}, nil, map[string]any{"modes": []string{"cw", "usb"}}))
	if len(ents) != 0 {
		t.Errorf("got %+v, want no entities for action-only enum command", ents)
	}
}

// TestActionMissingCommand asserts that an action without a valid action-only command is
// skipped rather than rendering a broken button.
func TestActionMissingCommand(t *testing.T) {
	cases := []struct {
		name string
		cmd  *expose.Command
	}{
		{"nil command", nil},
		{"empty action", &expose.Command{ValueKey: "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ents := Render("homeassistant", "", metaFixture(nil, []expose.Action{{
				Key: "retract", Name: "Retract", Command: c.cmd,
			}}, nil))
			if len(ents) != 0 {
				t.Errorf("got %+v, want no entities for invalid action command", ents)
			}
		})
	}
}
