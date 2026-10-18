// Package shelly speaks the Shelly Gen2+ native MQTT protocol for one plug.
//
// A Gen2+ Shelly (Plus / Mini / Pro …) is itself an MQTT client to the same
// broker. It publishes its relay state and accepts RPCs over MQTT. This package
// is the wire-level adapter the shelly-power-bridge uses to (a) parse telemetry
// the Shelly publishes and (b) build the RPC command to switch the relay. It
// holds no MQTT client of its own — main wires it to the per-slot paho client.
//
// Reference: Shelly Gen2+ "MqttReference" (RPC-over-MQTT). The relay state is
// announced on "<id>/status/switch:0"; commands go to "<id>/rpc".
package shelly

import (
	"encoding/json"
	"fmt"
)

// StatusTopic is the native topic a Gen2+ Shelly publishes its switch-0 state
// to. `<id>` is the device id (e.g. "shellyplus1pm-aabbccddeeff").
func StatusTopic(shellyID string) string {
	return shellyID + "/status/switch:0"
}

// RPCTopic is the native topic a Gen2+ Shelly accepts RPC requests on.
func RPCTopic(shellyID string) string {
	return shellyID + "/rpc"
}

// SwitchStatus is the subset of the "<id>/status/switch:0" payload the bridge
// cares about: the relay `output` boolean (true = on) and the human `apower`
// (W) for diagnostics. Gen2 publishes `output` as the actual relay position.
type SwitchStatus struct {
	Output  bool    `json:"output"`
	Apower  float64 `json:"apower,omitempty"`
	Aenergy Energy  `json:"aenergy,omitempty"`
	Voltage float64 `json:"voltage,omitempty"`
	Current float64 `json:"current,omitempty"`
}

// Energy is the switch's `aenergy` object. Real firmware sends an object
// ({"total":…,"by_minute":[…],"minute_ts":…}), not a number.
type Energy struct {
	Total float64 `json:"total"` // Wh
}

// ParseStatus decodes a "<id>/status/switch:0" payload into a SwitchStatus.
// Returns the canonical power string ("on"|"off") derived from `output`.
// A payload without `output` is an error, not "off": reading a missing field
// as false would report the relay off when it is on.
func ParseStatus(payload []byte) (SwitchStatus, string, error) {
	var raw struct {
		SwitchStatus
		Output *bool `json:"output"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return SwitchStatus{}, "", fmt.Errorf("decode switch:0: %w", err)
	}
	if raw.Output == nil {
		return SwitchStatus{}, "", fmt.Errorf("decode switch:0: no output field")
	}
	s := raw.SwitchStatus
	s.Output = *raw.Output
	if s.Output {
		return s, "on", nil
	}
	return s, "off", nil
}

// SwitchSet builds the Gen2+ RPC-over-MQTT payload to turn switch 0 on or off.
// Published to RPCTopic. The Shelly's response arrives on "<id>/rpc/rb"; the
// bridge does not need it — the resulting "<id>/status/switch:0" announce is
// the confirmation (fire-and-observe, integration model §8).
//
// {"id":1,"src":"shelly-power-bridge","method":"Switch.Set","params":{"id":0,"on":true}}
func SwitchSet(on bool) []byte {
	type params struct {
		ID int  `json:"id"`
		On bool `json:"on"`
	}
	type rpc struct {
		ID     int    `json:"id"`
		Src    string `json:"src"`
		Method string `json:"method"`
		Params params `json:"params"`
	}
	p := rpc{
		ID:     1,
		Src:    "shelly-power-bridge",
		Method: "Switch.Set",
		Params: params{ID: 0, On: on},
	}
	b, err := json.Marshal(p)
	if err != nil {
		// Cannot fail for this fixed struct; guard anyway.
		return nil
	}
	return b
}
