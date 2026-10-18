package shelly

import "fmt"

// Generations the bridge speaks. Gen2+ (Plus / Pro / Gen3 …) uses the
// "<id>/status/switch:0" + "<id>/rpc" protocol; Gen1 (Plug S, Shelly 1 …)
// uses "shellies/<id>/relay/0" with plain on/off commands.
const (
	Gen1 = "gen1"
	Gen2 = "gen2"
)

// Wire is one Shelly's native MQTT protocol: the topics to subscribe and
// publish, the relay-state parser and the command payload.
type Wire struct {
	StatusTopic string // relay state announce
	OnlineTopic string // "true" on connect, "false" = the Shelly's last will
	CmdTopic    string // where SetPower publishes
	// ParseStatus returns "on" | "off" from a StatusTopic payload.
	ParseStatus func(payload []byte) (string, error)
	// Command builds the CmdTopic payload for the wanted relay position.
	Command func(on bool) []byte
}

// WireFor returns the native protocol for a Shelly of the given generation
// ("" = gen2). shellyID is the device's MQTT id, e.g. "shellyplugsg3-…" or,
// for Gen1, "shellyplug-s-A71704".
func WireFor(generation, shellyID string) (Wire, error) {
	switch generation {
	case "", Gen2:
		return Wire{
			StatusTopic: StatusTopic(shellyID),
			OnlineTopic: shellyID + "/online",
			CmdTopic:    RPCTopic(shellyID),
			ParseStatus: func(p []byte) (string, error) {
				_, power, err := ParseStatus(p)
				return power, err
			},
			Command: SwitchSet,
		}, nil
	case Gen1:
		base := "shellies/" + shellyID
		return Wire{
			StatusTopic: base + "/relay/0",
			OnlineTopic: base + "/online",
			CmdTopic:    base + "/relay/0/command",
			ParseStatus: ParseGen1Relay,
			Command:     Gen1Command,
		}, nil
	default:
		return Wire{}, fmt.Errorf("unknown shelly generation %q (want gen1|gen2)", generation)
	}
}

// ParseGen1Relay decodes a Gen1 "shellies/<id>/relay/0" payload. "overpower"
// means the plug tripped its power limit and opened the relay — off.
func ParseGen1Relay(payload []byte) (string, error) {
	switch string(payload) {
	case "on":
		return "on", nil
	case "off", "overpower":
		return "off", nil
	default:
		return "", fmt.Errorf("decode gen1 relay/0: unexpected payload %q", payload)
	}
}

// Gen1Command is the "shellies/<id>/relay/0/command" payload.
func Gen1Command(on bool) []byte {
	if on {
		return []byte("on")
	}
	return []byte("off")
}
