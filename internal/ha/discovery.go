// Package ha builds Home Assistant MQTT discovery payloads and topic names.
//
// Each radio becomes one HA "device" (grouping all its entities). Meters
// and status fields become individual sensors under that device. We use the
// standard discovery layout:
//
//	<discovery_prefix>/<component>/<node_id>/<object_id>/config
//
// node_id is "flexradio-<serial>", so multiple radios coexist cleanly.
package ha

import (
	"fmt"
	"strings"

	"flexbridge/internal/flexradio"
)

// Components.
const (
	ComponentSensor       = "sensor"
	ComponentBinarySensor = "binary_sensor"
)

// Device info shared by every entity of one radio.
type Device struct {
	Serial string
	Model  string // e.g. "FLEX-8400"
	Name   string // e.g. "FlexRadio 8400"
}

// DiscoveryConfig is a Home Assistant MQTT discovery payload. Field names
// use the lower-case keys HA expects in JSON. We keep this hand-rolled
// (rather than importing a schema) so the payload stays minimal and the
// dependency list stays empty.
type DiscoveryConfig struct {
	// UniqueID is HA's stable id for this entity across restarts.
	UniqueID string `json:"unique_id"`
	// Name is the friendly entity name.
	Name string `json:"name"`
	// StateTopic is where the value is published.
	StateTopic string `json:"state_topic"`
	// ValueTemplate, if set, extracts the value from a JSON payload. We
	// publish plain values, so this is usually empty.
	ValueTemplate string `json:"value_template,omitempty"`
	// UnitOfMeasurement for numeric sensors.
	UnitOfMeasurement string `json:"unit_of_measurement,omitempty"`
	// DeviceClass (temperature, voltage, power, signal_strength, ...).
	DeviceClass string `json:"device_class,omitempty"`
	// StateClass (measurement / total). measurement for live telemetry.
	StateClass string `json:"state_class,omitempty"`
	// Category "diagnostic" or "config" to hide from the main UI.
	EntityType string `json:"entity_category,omitempty"`
	// For binary_sensors: the payload that maps to ON.
	PayloadOn string `json:"payload_on,omitempty"`
	// For binary_sensors: the payload that maps to OFF.
	PayloadOff string `json:"payload_off,omitempty"`
	// Device groups this entity under the radio.
	Device DeviceInfo `json:"device"`
	// AvailabilityTopic / Payload for the bridge LWT.
	AvailabilityTopic string `json:"availability_topic,omitempty"`
	AvailabilityMode  string `json:"availability_mode,omitempty"`
}

// DeviceInfo is the HA "device" block.
type DeviceInfo struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
}

// NodeID returns the discovery node id for a radio ("flexradio-<serial>").
func NodeID(serial string) string {
	return "flexradio-" + sanitize(serial)
}

// ConfigTopic returns the discovery config topic for an entity.
//
//	<discovery_prefix>/<component>/<node_id>/<object_id>/config
func ConfigTopic(discoveryPrefix, component, nodeID, objectID string) string {
	return fmt.Sprintf("%s/%s/%s/%s/config", discoveryPrefix, component, nodeID, objectID)
}

// DeviceInfoFor builds the HA device block for a radio.
func DeviceInfoFor(d Device) DeviceInfo {
	model := d.Model
	if model == "" {
		model = "FlexRadio"
	}
	name := d.Name
	if name == "" {
		name = "FlexRadio " + d.Serial
	}
	return DeviceInfo{
		Identifiers:  []string{d.Serial},
		Name:         name,
		Manufacturer: "FlexRadio",
		Model:        model,
	}
}

// MeterEntity builds the discovery config for a meter sensor.
// stateTopic is the full topic the bridge publishes the meter value to.
// availTopic is the bridge LWT topic (may be empty).
func MeterEntity(def flexradio.MeterDef, d Device, stateTopic, objectID, availTopic string) (DiscoveryConfig, string) {
	cfg := DiscoveryConfig{
		UniqueID:          fmt.Sprintf("%s_%s", NodeID(d.Serial), objectID),
		Name:              def.Label,
		StateTopic:        stateTopic,
		UnitOfMeasurement: publishUnitSymbol(def.PublishUnit),
		DeviceClass:       deviceClassFor(def.PublishUnit),
		StateClass:        "measurement",
		Device:            DeviceInfoFor(d),
		AvailabilityTopic: availTopic,
		AvailabilityMode:  "all",
	}
	if cfg.UnitOfMeasurement == "" {
		// dB / dBFS / SWR have no official device class unit; keep plain.
		cfg.UnitOfMeasurement = def.PublishUnit
	}
	component := ComponentSensor
	return cfg, component
}

// StatusEntity builds the discovery config for a status sensor.
// unit may be empty (e.g. for mode, a string state).
func StatusEntity(name, objectID, stateTopic, unit string, d Device, availTopic string) (DiscoveryConfig, string) {
	cfg := DiscoveryConfig{
		UniqueID:          fmt.Sprintf("%s_%s", NodeID(d.Serial), objectID),
		Name:              name,
		StateTopic:        stateTopic,
		UnitOfMeasurement: unit,
		DeviceClass:       deviceClassForUnit(unit),
		Device:            DeviceInfoFor(d),
		AvailabilityTopic: availTopic,
		AvailabilityMode:  "all",
	}
	if unit == "" {
		// String-valued state (mode, atu status): no state class, no unit.
	} else {
		cfg.StateClass = "measurement"
	}
	return cfg, ComponentSensor
}

// BinaryEntity builds the discovery config for a binary_sensor (e.g. the
// transmitting flag). on/off payloads default to "1"/"0" but can be set to
// "true"/"false" or "TRANSMITTING"/"RECEIVING".
func BinaryEntity(name, objectID, stateTopic, onPayload, offPayload string, d Device, availTopic string) (DiscoveryConfig, string) {
	cfg := DiscoveryConfig{
		UniqueID:          fmt.Sprintf("%s_%s", NodeID(d.Serial), objectID),
		Name:              name,
		StateTopic:        stateTopic,
		DeviceClass:       "running", // "transmitting" reads as a running/active state
		PayloadOn:         onPayload,
		PayloadOff:        offPayload,
		Device:            DeviceInfoFor(d),
		AvailabilityTopic: availTopic,
		AvailabilityMode:  "all",
	}
	return cfg, ComponentBinarySensor
}

// deviceClassFor maps a publish unit to the HA device class, where one is
// valid. Returns "" when there's no valid class (HA rejects invalid combos
// like unit=MHz + device_class=frequency, which expects Hz).
func deviceClassFor(publishUnit string) string {
	switch publishUnit {
	case "°C", "degC":
		return "temperature"
	case "V", "Volts":
		return "voltage"
	case "W", "Watts":
		return "power"
	case "A", "Amps":
		return "current"
	case "dBm":
		return "signal_strength"
	default:
		// dB, dBFS, SWR, MHz, plain strings: no device class.
		return ""
	}
}

// deviceClassForUnit handles status fields, where unit may be "MHz" (no
// class) or "W" (power).
func deviceClassForUnit(unit string) string {
	return deviceClassFor(unit)
}

// publishUnitSymbol normalizes a unit string to the symbol HA displays.
// "degC" -> "°C", "Volts" -> "V", etc.
func publishUnitSymbol(publishUnit string) string {
	switch publishUnit {
	case "°C", "degC":
		return "°C"
	case "V", "Volts":
		return "V"
	case "W", "Watts":
		return "W"
	case "A", "Amps":
		return "A"
	case "dBm":
		return "dBm"
	case "dB":
		return "dB"
	case "dBFS":
		return "dBFS"
	case "SWR":
		return "" // ratio, no unit symbol
	case "MHz":
		return "MHz"
	default:
		return publishUnit
	}
}

// sanitize lowercases and strips characters that aren't valid in HA object
// ids / topic segments.
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
