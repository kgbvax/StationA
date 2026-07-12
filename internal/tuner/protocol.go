package tuner

import "encoding/binary"

// ATR-1000 binary WebSocket protocol (reverse-engineered). Frame layout:
//
//	[0] 0xFF (flag) | [1] cmd | [2] payload length | [3..] payload   (uint16 LE)

const (
	scmdFlag       byte = 0xFF
	scmdSync       byte = 1
	scmdMeter      byte = 2 // swr/fwd/maxfwd
	scmdTuneStatus byte = 3 // [3]=0 bypass / 1 tuned
	scmdTuneMode   byte = 4 // [3]=0 Reset / 1 Mem / 2 Full / 3 Fine
	scmdRelay      byte = 5 // sw/ind/cap, L, C

	tuneModeMem  byte = 1
	tuneModeFull byte = 2
)

// buildFrame assembles a command frame.
func buildFrame(cmd byte, payload ...byte) []byte {
	return append([]byte{scmdFlag, cmd, byte(len(payload))}, payload...)
}

// parseFrame validates and splits an inbound frame into cmd + payload (the bytes
// from offset 3 on). ok is false for malformed frames.
func parseFrame(data []byte) (cmd byte, payload []byte, ok bool) {
	if len(data) < 3 || data[0] != scmdFlag {
		return 0, nil, false
	}
	return data[1], data[3:], true
}

// meter decodes a METER_STATUS frame: SWR (raw/100 when ≥100) and forward watts.
func meter(data []byte) (swr float64, fwd uint16, ok bool) {
	if len(data) < 8 {
		return 0, 0, false
	}
	raw := binary.LittleEndian.Uint16(data[4:6])
	swr = float64(raw)
	if raw >= 100 {
		swr = float64(raw) / 100
	}
	return swr, binary.LittleEndian.Uint16(data[6:8]), true
}

// relay decodes a RELAY_STATUS frame: inductance µH (raw/100) and capacitance pF.
func relay(data []byte) (lUH float64, cPF uint16, ok bool) {
	if len(data) < 10 {
		return 0, 0, false
	}
	return float64(binary.LittleEndian.Uint16(data[6:8])) / 100, binary.LittleEndian.Uint16(data[8:10]), true
}
