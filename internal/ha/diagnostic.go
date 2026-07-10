package ha

import (
	"hadiscovery/internal/expose"
)

// Diagnostic renders a single diagnostic binary_sensor for a slot that publishes /meta
// without an `expose` block, so the slot is at least visible in HA as a device with a
// liveness entity. The entity reads the slot's own /status plane (plain "online"/"offline"
// string) and is tagged entity_category "diagnostic". Generic — not per-role.
func Diagnostic(prefix string, m expose.SlotMeta) Entity {
	nodeID := NodeID(m)
	const oid = "online"
	p := discoveryPayload{
		UniqueID:       nodeID + "_" + oid,
		Name:           m.Role,
		StateTopic:     m.Addr + "/status",
		PayloadOn:      "online",
		PayloadOff:     "offline",
		EntityCategory: "diagnostic",
		Availability: []availability{{
			Topic:               m.Addr + "/status",
			PayloadAvailable:    "online",
			PayloadNotAvailable: "offline",
		}},
		AvailabilityMode: "all",
		Device:           deviceBlock(m, nodeID),
		Origin:           originPayload{Name: "hadiscovery", SWVersion: Version},
	}
	return Entity{
		Component: "binary_sensor",
		ObjectID:  oid,
		Topic:     ConfigTopic(prefix, "binary_sensor", nodeID, oid),
		Payload:   marshal(p),
	}
}
