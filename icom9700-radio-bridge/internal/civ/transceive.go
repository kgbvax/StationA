package civ

import (
	"errors"
	"fmt"
)

// Transceive interpretation (R7/KTD-10): the radio pushes unsolicited
// frames when CI-V Transceive is on — frequency changes (00), mode changes
// (01), PTT changes (1C 00). The same command bytes arrive as READ replies
// (terminator FB). One parser serves both; the IsReply flag lets the
// session layer attribute answers to outstanding reads.

// ErrUnsupportedMode marks a recognized-but-unpublishable mode byte (DV/DD/
// RTTY — R7): the event is consumed, the field omitted, never published raw.
var ErrUnsupportedMode = errors.New("civ: unsupported mode byte (omitted)")

type Transceive struct {
	Kind    string // "freq" | "mode" | "tx"
	IsReply bool   // true when the frame answers one of our reads (FB)
	FreqHz  uint64
	Mode    string
	Filter  byte
	TX      string // "tx" | "rx"
}

// ParseTransceive interprets one parsed frame. The second return is false
// when the frame is not a transceive event (other command bytes, NG
// replies, bare acks).
//
// VFO attribution: the 9700's frequency/mode transceives do not carry a
// VFO tag — the session layer attributes them (selected VFO; SUB while
// satellite mode is on, where SUB is the uplink and MAIN the downlink,
// KTD-10 satellite semantics).
func ParseTransceive(f Frame) (Transceive, bool, error) {
	if f.IsNG() {
		return Transceive{}, false, nil
	}
	isReply := f.Terminator == TerminatorOK
	switch f.Cmd {
	case CivCmdFreqBroadcast:
		hz, err := BCD10Decode(f.Sub)
		if err != nil {
			return Transceive{}, false, fmt.Errorf("civ: freq transceive: %w", err)
		}
		return Transceive{Kind: "freq", IsReply: isReply, FreqHz: hz}, true, nil

	case CivCmdModeBroadcast:
		mode, filter, ok, err := ParseModeReply(f.Sub)
		if err != nil {
			return Transceive{}, false, fmt.Errorf("civ: mode transceive: %w", err)
		}
		if !ok {
			// DV/DD/RTTY: consumed, mode omitted (R7).
			return Transceive{}, false, ErrUnsupportedMode
		}
		return Transceive{Kind: "mode", IsReply: isReply, Mode: mode, Filter: filter}, true, nil

	case CivCmdPTT:
		if len(f.Sub) != 2 || f.Sub[0] != SubPTT {
			return Transceive{}, false, nil
		}
		tx := "rx"
		if f.Sub[1] == 0x01 {
			tx = "tx"
		}
		return Transceive{Kind: "tx", IsReply: isReply, TX: tx}, true, nil
	}
	return Transceive{}, false, nil
}
