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
// slot's own /status LWT as its availability topic.
type availability struct {
	Topic               string `json:"topic"`
	PayloadAvailable    string `json:"payload_available"`
	PayloadNotAvailable string `json:"payload_not_available"`
}

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
// that slot. If the slot has no expose block, it returns nil (the engine decides what to do
// for undiscoverable slots — typically one diagnostic sensor).
func Render(prefix string, m expose.SlotMeta) []Entity {
	if m.Expose == nil {
		return nil
	}
	nodeID := NodeID(m)
	dev := deviceBlock(m, nodeID)
	avail := []availability{{
		Topic:               m.Addr + "/status",
		PayloadAvailable:    "online",
		PayloadNotAvailable: "offline",
	}}
	stateTopic := m.Addr + "/state"
	cmdTopic := m.Addr + "/cmd"

	var ents []Entity
	for _, f := range m.Expose.Fields {
		comp, p := fieldEntity(f, stateTopic, cmdTopic, nodeID, m.Capabilities, dev, avail)
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
		p := actionEntity(a, cmdTopic, nodeID, dev, avail)
		ents = append(ents, Entity{
			Component: "button",
			ObjectID:  sanitize(a.Key),
			Topic:     ConfigTopic(prefix, "button", nodeID, sanitize(a.Key)),
			Payload:   p,
		})
	}
	return ents
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
		base.DeviceClass = deviceClassFor(f)
		base.StateClass = f.StateClass
		if f.Writable {
			base.CommandTopic = cmdTopic
			base.CommandTemplate = commandTemplate(f.Command)
			base.Min = f.Min
			base.Max = f.Max
			base.Step = f.Step
			base.NumberMode = "box"
			base.Retain = true
			// A writable number reads its current value from the same state field.
			return "number", marshal(base)
		}
		return "sensor", marshal(base)

	case "enum":
		opts := f.Options
		if len(opts) == 0 && f.OptionsRef != "" {
			opts = expose.CapStringList(caps, f.OptionsRef)
		}
		if f.Writable {
			if f.Command == nil {
				return "", nil
			}
			base.CommandTopic = cmdTopic
			base.CommandTemplate = commandTemplate(f.Command)
			base.Options = opts
			base.Retain = true
			return "select", marshal(base)
		}
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

// actionEntity renders a one-shot button.
func actionEntity(a expose.Action, cmdTopic, nodeID string, dev devicePayload, avail []availability) []byte {
	oid := sanitize(a.Key)
	p := discoveryPayload{
		UniqueID:         nodeID + "_" + oid,
		Name:             a.Name,
		CommandTopic:     cmdTopic,
		PayloadPress:     commandPayload(a.Command),
		EntityCategory:   "config",
		Availability:     avail,
		AvailabilityMode: "all",
		Device:           dev,
		Origin:           originPayload{Name: "hadiscovery", SWVersion: Version},
	}
	return marshal(p)
}

// deviceBlock builds the HA device block for a slot: identifiers are the nodeID (one device
// per slot); name/model/manufacturer/sw_version/area come from expose.device, falling back
// to meta.device.
func deviceBlock(m expose.SlotMeta, nodeID string) devicePayload {
	d := devicePayload{Identifiers: []string{nodeID}}
	var name, model, sw string
	if m.Expose != nil && m.Expose.Device != nil {
		ed := m.Expose.Device
		name, model, sw = ed.Name, ed.Model, ed.SWVersion
		d.Manufacturer = ed.Manufacturer
		d.SuggestedArea = ed.Area
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

// commandPayload renders a button's command descriptor into the static payload_press JSON
// (no template — a button sends a fixed payload).
func commandPayload(c *expose.Command) string {
	if c == nil {
		return ""
	}
	if c.Action != "" && c.ValueKey == "" {
		return fmt.Sprintf(`{"action":"%s"}`, c.Action)
	}
	if c.Action == "" && c.ValueKey != "" {
		// A button with a value key is unusual; emit the key with an empty value.
		return fmt.Sprintf(`{"%s":""}`, c.ValueKey)
	}
	if c.Action != "" && c.ValueKey != "" {
		return fmt.Sprintf(`{"action":"%s","%s":""}`, c.Action, c.ValueKey)
	}
	return ""
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

// deviceClassFor maps a neutral unit/class to the HA device_class. If the field carries an
// explicit `class`, that wins (it is already a generic semantic hint the consumer may pass
// through where HA accepts it); otherwise the unit is mapped via unitToDeviceClass.
func deviceClassFor(f expose.Field) string {
	if f.Class != "" {
		return f.Class
	}
	return unitToDeviceClass(f.Unit)
}

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
