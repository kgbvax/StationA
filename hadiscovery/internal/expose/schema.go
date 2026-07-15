// Package expose defines and parses the consumer-neutral `expose` block carried on a
// slot's /meta birth certificate (integration model §3.1, Appendix C).
//
// `expose` describes a slot's observable/controllable field surface in a form no specific
// consumer owns: field type (number/enum/boolean/string), unit, semantic class, enum
// options (inline or by reference into `capabilities`), writability, a structured
// `command` descriptor, and one-shot `actions`. It carries NO consumer vocabulary — no
// device_class, no Jinja templates, no payload_on/off. Consumers (package ha, and any
// future non-HA consumer) render their own representation from these neutral primitives.
package expose

// Expose is the top-level `expose` block on /meta. All fields optional; a slot that omits
// the block entirely is simply not discovered.
type Expose struct {
	Device  *DeviceBlock `json:"device,omitempty"`
	Fields  []Field      `json:"fields,omitempty"`
	Actions []Action     `json:"actions,omitempty"`
}

// DeviceBlock is the consumer "device" registry block shared by all entities of one slot.
// It supplements meta.device for the consumer; logic slots may omit it.
type DeviceBlock struct {
	Name         string `json:"name,omitempty"`
	Model        string `json:"model,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	SWVersion    string `json:"sw_version,omitempty"`
	Area         string `json:"area,omitempty"`
}

// Field describes one state field to expose.
type Field struct {
	Key        string   `json:"key"`                   // the /state JSON key
	Name       string   `json:"name"`                  // display name
	Type       string   `json:"type"`                  // number | enum | boolean | string
	Unit       string   `json:"unit,omitempty"`        // for number
	Class      string   `json:"class,omitempty"`       // semantic hint (frequency, power, ...)
	StateClass string   `json:"state_class,omitempty"` // measurement | total | total_increasing
	Options    []string `json:"options,omitempty"`     // inline enum options
	OptionsRef string   `json:"options_ref,omitempty"` // key into capabilities for the options
	Writable   bool     `json:"writable,omitempty"`    // setpoint/command target
	Command    *Command `json:"command,omitempty"`     // required when Writable
	On         string   `json:"on,omitempty"`          // boolean: payload string for ON
	Off        string   `json:"off,omitempty"`         // boolean: payload string for OFF
	Min        any      `json:"min,omitempty"`         // writable number
	Max        any      `json:"max,omitempty"`         // writable number
	Step       any      `json:"step,omitempty"`        // writable number
}

// Action describes a one-shot button.
type Action struct {
	Key     string   `json:"key"`     // becomes the entity object id
	Name    string   `json:"name"`    // display name
	Command *Command `json:"command"` // the /cmd payload to send on press
}

// Command describes how a write is encoded on /cmd, in structured form (never a
// consumer-specific template string). The three shapes (see model Appendix C):
//   - Action + ValueKey: {"action":"<action>","<value_key>":<value>}
//   - ValueKey only:     {"<value_key>":<value>}
//   - Action only:       {"action":"<action>"}   (button)
type Command struct {
	Action    string `json:"action,omitempty"`
	ValueKey  string `json:"value_key,omitempty"`
	ValueType string `json:"value_type,omitempty"` // string | int | float
}
