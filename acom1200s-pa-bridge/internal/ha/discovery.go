// Package ha builds Home Assistant MQTT discovery payloads and topic names for
// the ACOM amplifier slot.
//
// The amplifier becomes one HA "device" grouping all its entities. Discovery
// uses the standard layout:
//
//	<discovery_prefix>/<component>/<node_id>/<object_id>/config
//
// node_id is "acom-<serial>". This legacy embedded discovery is gated behind
// config PublishHADiscovery (model §9); the default path renders discovery
// from /meta.expose via the standalone hadiscovery consumer.
package ha

import (
	"fmt"
	"strings"
)

// Components.
const (
	ComponentSensor       = "sensor"
	ComponentBinarySensor = "binary_sensor"
)

// Device info shared by every entity of one amplifier.
type Device struct {
	Serial   string
	Model    string // e.g. "ACOM 1200S"
	Name     string // e.g. "ACOM 1200S"
	Firmware string // empty if unknown (the ACOM serial protocol reports none)
}

// DiscoveryConfig is a Home Assistant MQTT discovery payload. Field names use
// the lower-case keys HA expects in JSON. Hand-rolled to keep the payload
// minimal and the dependency list empty.
type DiscoveryConfig struct {
	UniqueID          string     `json:"unique_id"`
	Name              string     `json:"name"`
	StateTopic        string     `json:"state_topic"`
	ValueTemplate     string     `json:"value_template,omitempty"`
	UnitOfMeasurement string     `json:"unit_of_measurement,omitempty"`
	DeviceClass       string     `json:"device_class,omitempty"`
	StateClass        string     `json:"state_class,omitempty"`
	EntityType        string     `json:"entity_category,omitempty"`
	PayloadOn         string     `json:"payload_on,omitempty"`
	PayloadOff        string     `json:"payload_off,omitempty"`
	CommandTopic      string     `json:"command_topic,omitempty"`
	CommandTemplate   string     `json:"command_template,omitempty"`
	Options           []string   `json:"options,omitempty"`
	Device            DeviceInfo `json:"device"`
	AvailabilityTopic string     `json:"availability_topic,omitempty"`
	AvailabilityMode  string     `json:"availability_mode,omitempty"`
}

// DeviceInfo is the HA "device" block.
type DeviceInfo struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	SWVersion    string   `json:"sw_version,omitempty"`
}

// NodeID returns the discovery node id for an amplifier ("acom-<serial>").
func NodeID(serial string) string {
	return "acom-" + sanitize(serial)
}

// ConfigTopic returns the discovery config topic for an entity.
//
//	<discovery_prefix>/<component>/<node_id>/<object_id>/config
func ConfigTopic(discoveryPrefix, component, nodeID, objectID string) string {
	return fmt.Sprintf("%s/%s/%s/%s/config", discoveryPrefix, component, nodeID, objectID)
}

// DeviceInfoFor builds the HA device block for an amplifier.
func DeviceInfoFor(d Device) DeviceInfo {
	model := d.Model
	if model == "" {
		model = "ACOM"
	}
	name := d.Name
	if name == "" {
		name = "ACOM " + d.Serial
	}
	return DeviceInfo{
		Identifiers:  []string{d.Serial},
		Name:         name,
		Manufacturer: "ACOM",
		Model:        model,
		SWVersion:    d.Firmware,
	}
}

// SensorEntity builds the discovery config for a numeric/string sensor.
// valueTemplate extracts the field from the JSON state snapshot
// (e.g. "{{ value_json.fwd_power_w }}"). unit may be empty for string sensors.
func SensorEntity(name, objectID, stateTopic, unit, valueTemplate string, d Device, availTopic string) (DiscoveryConfig, string) {
	cfg := DiscoveryConfig{
		UniqueID:          fmt.Sprintf("%s_%s", NodeID(d.Serial), objectID),
		Name:              name,
		StateTopic:        stateTopic,
		ValueTemplate:     valueTemplate,
		UnitOfMeasurement: unit,
		DeviceClass:       deviceClassFor(unit),
		Device:            DeviceInfoFor(d),
		AvailabilityTopic: availTopic,
		AvailabilityMode:  "all",
	}
	if unit != "" {
		cfg.StateClass = "measurement"
	}
	return cfg, ComponentSensor
}

// BinaryEntity builds the discovery config for a binary_sensor.
func BinaryEntity(name, objectID, stateTopic, onPayload, offPayload, valueTemplate string, d Device, availTopic string) (DiscoveryConfig, string) {
	cfg := DiscoveryConfig{
		UniqueID:          fmt.Sprintf("%s_%s", NodeID(d.Serial), objectID),
		Name:              name,
		StateTopic:        stateTopic,
		ValueTemplate:     valueTemplate,
		DeviceClass:       "running",
		PayloadOn:         onPayload,
		PayloadOff:        offPayload,
		Device:            DeviceInfoFor(d),
		AvailabilityTopic: availTopic,
		AvailabilityMode:  "all",
	}
	return cfg, ComponentBinarySensor
}

// SelectEntity builds the discovery config for a writable enum select, with a
// command topic. cmdTemplate wraps HA's selected option into the /cmd JSON the
// bridge expects (e.g. `{"action":"set_mode","value":"{{ value }}"}`).
func SelectEntity(name, objectID, stateTopic, cmdTopic, valueTemplate, cmdTemplate string, options []string, d Device, availTopic string) (DiscoveryConfig, string) {
	cfg := DiscoveryConfig{
		UniqueID:          fmt.Sprintf("%s_%s", NodeID(d.Serial), objectID),
		Name:              name,
		StateTopic:        stateTopic,
		CommandTopic:      cmdTopic,
		ValueTemplate:     valueTemplate,
		CommandTemplate:   cmdTemplate,
		Options:           options,
		Device:            DeviceInfoFor(d),
		AvailabilityTopic: availTopic,
		AvailabilityMode:  "all",
	}
	return cfg, "select"
}

// deviceClassFor maps a publish unit to the HA device class where one is valid.
func deviceClassFor(unit string) string {
	switch unit {
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
	case "Hz":
		return "frequency"
	default:
		return ""
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
