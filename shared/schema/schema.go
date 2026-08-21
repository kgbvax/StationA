// Package schema holds the shared station-integration-model MQTT address helpers
// and the /cmd payload convention used by every stationa bridge and logic
// component.
//
// The canonical slot address is <site>/<station>/<slot>, with the four planes
// meta | state | status | cmd appended as suffixes (see the station integration
// model, `docs/station-integration-model.md`). These helpers build those topics
// once so no module hand-concatenates them.
//
// CmdPayload encodes the /cmd convention: the argument rides under the `value`
// key (NOT under a key named after the action). atr1k-tuner-bridge got this
// wrong live; centralizing it here makes the convention structural.
package schema

// SlotBase returns the canonical slot address <site>/<station>/<slot>.
func SlotBase(site, station, slot string) string {
	return site + "/" + station + "/" + slot
}

// MetaTopic returns <slot>/meta (retained birth certificate).
func MetaTopic(site, station, slot string) string { return SlotBase(site, station, slot) + "/meta" }

// StateTopic returns <slot>/state (retained live snapshot).
func StateTopic(site, station, slot string) string { return SlotBase(site, station, slot) + "/state" }

// StatusTopic returns <slot>/status (retained online|offline, LWT).
func StatusTopic(site, station, slot string) string { return SlotBase(site, station, slot) + "/status" }

// CmdTopic returns <slot>/cmd (operator → bridge intent).
func CmdTopic(site, station, slot string) string { return SlotBase(site, station, slot) + "/cmd" }

// SiblingTopic returns <site>/<station>/<slot>/<suffix> for an arbitrary slot
// address — used by consumer/logic components (antennaselect, powerseq,
// hadiscovery) that subscribe to sibling slots.
func SiblingTopic(site, station, slot, suffix string) string {
	return SlotBase(site, station, slot) + "/" + suffix
}

// CmdPayload is the /cmd JSON every bridge accepts. The argument rides under the
// `value` key (the stationa /cmd convention), never under a key named after the
// action. Actions are slot-specific ("set_power", "tune", "set_band", …); the
// carrier key is always `value`.
type CmdPayload struct {
	Action string `json:"action"`
	Value  string `json:"value"`
}
