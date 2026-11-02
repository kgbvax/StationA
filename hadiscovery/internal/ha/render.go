// Package ha renders Home Assistant MQTT discovery from a slot's consumer-neutral `expose`
// block (see package expose and model §3.1 / Appendix C).
//
// This is the *only* place HA-specific knowledge lives. It is a small, finite, deterministic
// mapper from neutral primitives (number/enum/boolean/action + writable + command) to HA
// discovery components (sensor/number/select/binary_sensor/button), injecting the topic
// planes (state/cmd/status derived from the slot address), the device block, availability,
// and unique ids. The bridges contain no HA knowledge; this package is the single renderer.
package ha

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"hadiscovery/internal/expose"
)

// Version is the hadiscovery build version, stamped into the HA `origin` block. Overridable
// via -ldflags at build time; "dev" by default.
var Version = "dev"

// Entity is one rendered HA discovery message: the HA component, the object id, the
// discovery config topic, and the JSON payload to publish there (retained).
type Entity struct {
	Component string
	ObjectID  string
	Topic     string
	Payload   []byte
}

// availability is one entry in the HA availability list. Every entity of a slot shares the
// slot's own /status LWT as its availability topic; a device-backed slot (one that exposes
// a boolean device_online field) adds its /state device_online as a second entry, so HA
// follows the station's two-layer liveness rule (bridge up AND device link up).
type availability struct {
	Topic               string `json:"topic"`
	PayloadAvailable    string `json:"payload_available"`
	PayloadNotAvailable string `json:"payload_not_available"`
	ValueTemplate       string `json:"value_template,omitempty"`
}

// deviceOnlineKey is the /state field that carries the device-link layer of the station's
// two-layer liveness (integration model: /status = bridge LWT, device_online = device link).
const deviceOnlineKey = "device_online"

type devicePayload struct {
	Identifiers   []string `json:"identifiers"`
	Name          string   `json:"name"`
	Manufacturer  string   `json:"manufacturer,omitempty"`
	Model         string   `json:"model,omitempty"`
	SWVersion     string   `json:"sw_version,omitempty"`
	SuggestedArea string   `json:"suggested_area,omitempty"`
}

type originPayload struct {
	Name      string `json:"name"`
	SWVersion string `json:"sw_version,omitempty"`
}

// discoveryPayload is the HA discovery config JSON. Field order is deliberate (golden tests
// depend on it). Unknown-to-HA keys are not emitted.
type discoveryPayload struct {
	UniqueID          string         `json:"unique_id"`
	Name              string         `json:"name"`
	StateTopic        string         `json:"state_topic,omitempty"`
	CommandTopic      string         `json:"command_topic,omitempty"`
	CommandTemplate   string         `json:"command_template,omitempty"`
	ValueTemplate     string         `json:"value_template,omitempty"`
	UnitOfMeasurement string         `json:"unit_of_measurement,omitempty"`
	DeviceClass       string         `json:"device_class,omitempty"`
	StateClass        string         `json:"state_class,omitempty"`
	PayloadOn         string         `json:"payload_on,omitempty"`
	PayloadOff        string         `json:"payload_off,omitempty"`
	PayloadPress      string         `json:"payload_press,omitempty"`
	Options           []string       `json:"options,omitempty"`
	Min               any            `json:"min,omitempty"`
	Max               any            `json:"max,omitempty"`
	Step              any            `json:"step,omitempty"`
	NumberMode        string         `json:"mode,omitempty"`
	Retain            bool           `json:"retain,omitempty"`
	EntityCategory    string         `json:"entity_category,omitempty"`
	Availability      []availability `json:"availability"`
	AvailabilityMode  string         `json:"availability_mode,omitempty"`
	Device            devicePayload  `json:"device"`
	Origin            originPayload  `json:"origin"`
}

// Render maps a slot's parsed meta (with its expose block) to the HA discovery entities for
// that slot. area is the deployment-wide HA area used as `suggested_area` when the slot's
// own expose.device.area does not name one; pass "" to emit no suggested_area fallback. If
// the slot has no expose block, it returns nil (the engine decides what to do for
// undiscoverable slots — typically one diagnostic sensor).
func Render(prefix, area string, m expose.SlotMeta) []Entity {
	if m.Expose == nil {
		return nil
	}
	nodeID := NodeID(m)
	dev := deviceBlock(m, nodeID, area)
	stateTopic := m.Addr + "/state"
	cmdTopic := m.Addr + "/cmd"
	bridgeAvail := []availability{{
		Topic:               m.Addr + "/status",
		PayloadAvailable:    "online",
		PayloadNotAvailable: "offline",
	}}
	avail := bridgeAvail
	if exposesDeviceOnline(m.Expose) {
		avail = append(append([]availability(nil), bridgeAvail...), availability{
			Topic:               stateTopic,
			PayloadAvailable:    "online",
			PayloadNotAvailable: "offline",
			ValueTemplate:       "{{ 'online' if value_json." + deviceOnlineKey + " | default(false) else 'offline' }}",
		})
	}

	var ents []Entity
	for _, f := range m.Expose.Fields {
		// The device_online entity itself follows only the bridge layer: gating it on
		// device_online would make it go unavailable exactly when it should read "off".
		fa := avail
		if f.Key == deviceOnlineKey {
			fa = bridgeAvail
		}
		comp, p := fieldEntity(f, stateTopic, cmdTopic, nodeID, m.Capabilities, dev, fa)
		if comp == "" {
			continue
		}
		ents = append(ents, Entity{
			Component: comp,
			ObjectID:  sanitize(f.Key),
			Topic:     ConfigTopic(prefix, comp, nodeID, sanitize(f.Key)),
			Payload:   p,
		})
	}
	for _, a := range m.Expose.Actions {
		for _, b := range actionButtons(a, m.Capabilities) {
			oid := sanitize(b.key)
			ents = append(ents, Entity{
				Component: "button",
				ObjectID:  oid,
				Topic:     ConfigTopic(prefix, "button", nodeID, oid),
				Payload:   buttonPayload(b, cmdTopic, nodeID, dev, avail),
			})
		}
	}
	return ents
}

// exposesDeviceOnline reports whether the slot declares the device-link liveness field.
func exposesDeviceOnline(ex *expose.Expose) bool {
	for _, f := range ex.Fields {
		if f.Key == deviceOnlineKey && f.Type == "boolean" {
			return true
		}
	}
	return false
}

// fieldEntity renders one expose field to (HA component, payload JSON). Returns "" component
// for an unrenderable field (unknown type, writable without a command, enum without options).
// caps is the slot's capabilities map, used to resolve `options_ref`.
func fieldEntity(f expose.Field, stateTopic, cmdTopic, nodeID string, caps map[string]any, dev devicePayload, avail []availability) (string, []byte) {
	oid := sanitize(f.Key)
	base := discoveryPayload{
		UniqueID:         nodeID + "_" + oid,
		Name:             f.Name,
		StateTopic:       stateTopic,
		ValueTemplate:    "{{ value_json." + f.Key + " }}",
		Availability:     avail,
		AvailabilityMode: "all",
		Device:           dev,
		Origin:           originPayload{Name: "hadiscovery", SWVersion: Version},
	}

	switch f.Type {
	case "number":
		base.UnitOfMeasurement = f.Unit
		if f.Writable {
			base.DeviceClass = deviceClassFor(f, numberDeviceClasses)
		} else {
			base.DeviceClass = deviceClassFor(f, sensorDeviceClasses)
		}
		if f.Writable {
			if !writableCommandValid(f.Command) {
				return "", nil
			}
			base.CommandTopic = cmdTopic
			base.CommandTemplate = commandTemplate(f.Command)
			base.Min = f.Min
			base.Max = f.Max
			base.Step = f.Step
			base.NumberMode = "box"
			// Retained on purpose (model §8 exception). HA must
			// keep the retained /cmd tracking the latest operator intent: for a
			// steady-state slot (power, switch, pa-arm) the retained value is what the
			// target re-applies on its own reconnect — a non-retained HA write would
			// desync it (powerseq retains "off" at shutdown, the operator switches on via
			// HA, a bridge restart replays "off" and cuts the rail). A one-shot consumer
			// must not re-execute a retained replay (model §8 rule 1): it either clears
			// the topic after executing (ultrabridge) or drops m.Retained() deliveries
			// (acombridge, flexbridge, wrc-rotator-bridge). Retaining here is safe only
			// because every one-shot consumer does one of the two.
			// A writable number reads its current value from the same state field.
			base.Retain = true
			return "number", marshal(base)
		}
		base.StateClass = f.StateClass
		return "sensor", marshal(base)

	case "enum":
		opts := f.Options
		if len(opts) == 0 && f.OptionsRef != "" {
			opts = expose.CapStringList(caps, f.OptionsRef)
		}
		if f.Writable {
			// A HA select requires a non-empty `options` list (CONF_OPTIONS is
			// vol.Required in the mqtt select discovery schema). An enum whose options
			// resolve to empty — inline options missing and options_ref points at a
			// missing/empty capabilities key — would be published as a select with no
			// `options` key (the omitempty tag drops nil), which HA rejects. Skip it
			// instead, matching this function's "enum without options → unrenderable"
			// contract (see docstring above).
			if !writableCommandValid(f.Command) || len(opts) == 0 {
				return "", nil
			}
			base.CommandTopic = cmdTopic
			base.CommandTemplate = commandTemplate(f.Command)
			base.Options = opts
			// Retained on purpose — see the writable-number comment above.
			base.Retain = true
			return "select", marshal(base)
		}
		// A read-only enum renders as a plain sensor (its value_template just reads
		// the state field); an empty option set is harmless here, so no guard needed.
		return "sensor", marshal(base)

	case "boolean":
		if f.On != "" || f.Off != "" {
			base.PayloadOn = f.On
			base.PayloadOff = f.Off
			if base.PayloadOn == "" {
				base.PayloadOn = "ON"
			}
			if base.PayloadOff == "" {
				base.PayloadOff = "OFF"
			}
		} else {
			base.ValueTemplate = fmt.Sprintf("{{ 'ON' if value_json.%s else 'OFF' }}", f.Key)
			base.PayloadOn = "ON"
			base.PayloadOff = "OFF"
		}
		return "binary_sensor", marshal(base)

	case "string":
		return "sensor", marshal(base)
	}
	return "", nil
}

// button is one rendered press target: its object key, display name and static payload.
type button struct {
	key, name, payload string
}

// actionButtons expands one expose action into HA buttons. Actions require a command with a
// non-empty action name. An action-only command is one button. An action whose command
// carries a value_key gets one button per option (inline options, else options_ref into
// capabilities), keyed "<key>_<option>" — a HA button sends a fixed payload, so the argument
// must be fixed per button. An action with a value_key but no resolvable options renders
// nothing: an empty argument is never a valid command (the tuner rejects tune "").
func actionButtons(a expose.Action, caps map[string]any) []button {
	c := a.Command
	if c == nil || c.Action == "" {
		return nil
	}
	if c.ValueKey == "" {
		return []button{{key: a.Key, name: a.Name, payload: fmt.Sprintf(`{"action":%s}`, jsonString(c.Action))}}
	}
	opts := a.Options
	if len(opts) == 0 && a.OptionsRef != "" {
		opts = expose.CapStringList(caps, a.OptionsRef)
	}
	out := make([]button, 0, len(opts))
	for _, o := range opts {
		out = append(out, button{
			key:     a.Key + "_" + o,
			name:    a.Name + " " + o,
			payload: fmt.Sprintf(`{"action":%s,%s:%s}`, jsonString(c.Action), jsonString(c.ValueKey), literalValue(o, c.ValueType)),
		})
	}
	return out
}

// buttonPayload renders one button's discovery config. Buttons carry no entity_category:
// they are the slot's primary controls (DVK, rotate, retract, tune), and HA hides
// categorised entities from its auto-generated dashboard.
func buttonPayload(b button, cmdTopic, nodeID string, dev devicePayload, avail []availability) []byte {
	oid := sanitize(b.key)
	return marshal(discoveryPayload{
		UniqueID:         nodeID + "_" + oid,
		Name:             b.name,
		CommandTopic:     cmdTopic,
		PayloadPress:     b.payload,
		Availability:     avail,
		AvailabilityMode: "all",
		Device:           dev,
		Origin:           originPayload{Name: "hadiscovery", SWVersion: Version},
	})
}

// literalValue renders a fixed argument as a JSON literal per value_type: numbers and
// bools unquoted when they parse, everything else a JSON string.
func literalValue(v, valueType string) string {
	switch valueType {
	case "int":
		if _, err := strconv.ParseInt(v, 10, 64); err == nil {
			return v
		}
	case "float":
		if _, err := strconv.ParseFloat(v, 64); err == nil {
			return v
		}
	case "bool":
		if b, err := strconv.ParseBool(v); err == nil {
			return strconv.FormatBool(b)
		}
	}
	return jsonString(v)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// deviceBlock builds the HA device block for a slot: identifiers are the nodeID (one device
// per slot); name/model/manufacturer/sw_version/area come from expose.device, falling back
// to meta.device. The HA `suggested_area` is expose.device.area, or — when the slot does
// not name one — the deployment-wide default area (so every discovered device lands in the
// configured area unless a slot opts out). area == "" suppresses the fallback.
func deviceBlock(m expose.SlotMeta, nodeID, area string) devicePayload {
	d := devicePayload{Identifiers: []string{nodeID}}
	var name, model, sw string
	if m.Expose != nil && m.Expose.Device != nil {
		ed := m.Expose.Device
		name, model, sw = ed.Name, ed.Model, ed.SWVersion
		d.Manufacturer = ed.Manufacturer
		d.SuggestedArea = ed.Area
	}
	if d.SuggestedArea == "" {
		d.SuggestedArea = area
	}
	if model == "" {
		model = m.Device.Model
	}
	if sw == "" {
		sw = m.Device.Firmware
	}
	if name == "" {
		if model != "" {
			name = model
		} else {
			name = m.Role + " " + m.Addr
		}
	}
	d.Name = name
	d.Model = model
	d.SWVersion = sw
	return d
}

// writableCommandValid reports whether c is a valid command descriptor for a writable
// field. It requires a non-nil command with a non-empty value_key. Action-only commands
// (action with no value_key) are valid only for buttons.
func writableCommandValid(c *expose.Command) bool {
	return c != nil && c.ValueKey != ""
}

// commandTemplate renders a writable field's command descriptor into the HA command_template
// string (Jinja). The descriptor carries the role/field-specific knowledge (action name,
// value key, value coercion); this function owns the HA template syntax.
func commandTemplate(c *expose.Command) string {
	if c == nil {
		return ""
	}
	ph := valuePlaceholder(c.ValueType)
	switch {
	case c.Action != "" && c.ValueKey != "":
		return fmt.Sprintf(`{"action":"%s","%s":%s}`, c.Action, c.ValueKey, ph)
	case c.Action == "" && c.ValueKey != "":
		return fmt.Sprintf(`{"%s":%s}`, c.ValueKey, ph)
	default:
		return ""
	}
}

// valuePlaceholder returns the HA Jinja placeholder for the user-supplied value, coerced per
// value_type.
func valuePlaceholder(valueType string) string {
	switch valueType {
	case "int":
		return "{{ value | int }}"
	case "float":
		return "{{ value | float }}"
	default: // "string" or unset
		return `"{{ value }}"`
	}
}

// deviceClassFor maps a neutral unit/class to the HA device_class for one component. The
// neutral `class` is a station semantic hint, not HA vocabulary: it is passed through only
// when HA knows it for that component and the field's unit is one HA accepts for it.
// Otherwise the unit-derived class is tried the same way, and failing both no device_class
// is emitted. HA rejects the whole discovery config on an unknown device_class (or a unit
// that does not fit it), so an unvalidated pass-through loses the entity entirely — the
// station's `azimuth` and `swr` hints did exactly that.
func deviceClassFor(f expose.Field, classes map[string][]string) string {
	for _, dc := range []string{f.Class, unitToDeviceClass(f.Unit)} {
		if dc == "" {
			continue
		}
		units, ok := classes[dc]
		if !ok {
			continue
		}
		if len(units) == 0 || slices.Contains(units, f.Unit) {
			return dc
		}
	}
	return ""
}

// sensorDeviceClasses lists the HA sensor device classes hadiscovery may emit, each with the
// units HA accepts for it (nil = no unit check). Deliberately a subset of HA's
// SensorDeviceClass: the classes a station field can plausibly carry. A class not listed
// here is dropped, never guessed.
var sensorDeviceClasses = map[string][]string{
	"frequency":       {"Hz", "kHz", "MHz", "GHz"},
	"power":           {"mW", "W", "kW", "MW"},
	"apparent_power":  {"VA"},
	"voltage":         {"µV", "mV", "V", "kV"},
	"current":         {"mA", "A"},
	"temperature":     {"°C", "°F", "K"},
	"signal_strength": {"dB", "dBm"},
	"sound_pressure":  {"dB", "dBA"},
	"distance":        {"mm", "cm", "m", "km", "in", "ft", "yd", "mi", "nmi"},
	"speed":           {"m/s", "km/h", "mph", "kn", "ft/s"},
	"wind_speed":      {"m/s", "km/h", "mph", "kn", "ft/s", "Beaufort"},
	"duration":        {"µs", "ms", "s", "min", "h", "d"},
	"humidity":        {"%"},
	"battery":         {"%"},
	"energy":          {"Wh", "kWh", "MWh"},
	"data_rate":       {"bit/s", "kbit/s", "Mbit/s", "B/s", "kB/s", "MB/s"},
	"timestamp":       nil,
}

// numberDeviceClasses is the subset valid for a HA number (NumberDeviceClass has no
// timestamp).
var numberDeviceClasses = func() map[string][]string {
	m := make(map[string][]string, len(sensorDeviceClasses))
	for k, v := range sensorDeviceClasses {
		if k != "timestamp" {
			m[k] = v
		}
	}
	return m
}()

// unitToDeviceClass maps a unit string to the HA device_class, where a valid one exists.
// Mirrors flexbridge/internal/ha/deviceClassFor.
func unitToDeviceClass(unit string) string {
	switch unit {
	case "Hz":
		return "frequency"
	case "°C", "degC":
		return "temperature"
	case "W", "Watts":
		return "power"
	case "V", "Volts":
		return "voltage"
	case "A", "Amps":
		return "current"
	case "dBm":
		return "signal_strength"
	}
	return ""
}

// NodeID returns the sanitized HA node id for a slot: "<site>-<station>-<slot>".
func NodeID(m expose.SlotMeta) string {
	return sanitize(m.Site + "-" + m.Station + "-" + m.Slot)
}

// ConfigTopic returns the discovery config topic for an entity:
//
//	<prefix>/<component>/<nodeID>/<objectID>/config
func ConfigTopic(prefix, component, nodeID, objectID string) string {
	return fmt.Sprintf("%s/%s/%s/%s/config", prefix, component, nodeID, objectID)
}

// sanitize lowercases and replaces characters that aren't valid in HA node/object ids
// ([a-zA-Z0-9_-]) with "_". Matches flexbridge/internal/ha.sanitize.
func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func marshal(p discoveryPayload) []byte {
	b, err := json.Marshal(p)
	if err != nil {
		// discoveryPayload contains only primitives, slices, and concrete types; marshal
		// cannot fail in practice. If it ever does, emit a minimal valid payload.
		return []byte(`{}`)
	}
	return b
}
