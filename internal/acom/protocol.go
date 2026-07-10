// Package acom implements the ACOM 600S/1200S serial monitor protocol.
//
// The amplifier speaks a framed, checksummed binary protocol over a USB-serial
// adapter (Prolific, 9600 8N1). This package owns the wire layer: framing,
// checksums, the fixed 72-byte telemetry decoder, the band/mode/fault tables,
// and the outbound command frames (enable-telemetry, set-mode, band-change).
//
// It exposes a Device that main runs; decoded telemetry is emitted as an
// Observation for the bridge to canonicalize and publish. The package holds
// only protocol-level state (current raw mode for the watchdog, current band
// index for relative band navigation); the canonical published state lives in
// the bridge.
package acom

// Constants defined in the ACOM serial protocol documentation.
const (
	StartByte     byte = 0x55 // frame start byte
	MsgEnableAuto byte = 0x92 // enable automatic telemetry
	MsgTelemetry  byte = 0x2F // combined status/telemetry message
	MsgAck        byte = 0x86 // acknowledge
	MsgAmpMgmt    byte = 0x81 // amplifier management commands

	// Sub-commands for MsgAmpMgmt (frame byte 3).
	CmdModeChange byte = 0x02 // request amplifier mode change
	CmdBandChange byte = 0x09 // manual band/antenna change

	// Modes for CmdModeChange (frame byte 5).
	ModeSTB   byte = 0x05 // standby
	ModeOPRRX byte = 0x06 // operate / receive

	// Directions for CmdBandChange (frame byte 5).
	BandNext byte = 0x80 // next band
	BandPrev byte = 0x40 // previous band
)

// verifyChecksum returns true if every byte of the frame sums to 0. A frame
// shorter than the minimum (start + addr + len + at least one more byte) cannot
// be a valid ACOM frame; an empty slice sums to 0 trivially, so it is rejected
// here rather than accepted as a false positive.
func verifyChecksum(packet []byte) bool {
	if len(packet) < 4 {
		return false
	}
	var sum byte
	for _, b := range packet {
		sum += b
	}
	return sum == 0
}

// calculateChecksum returns the byte that makes the frame sum to 0.
func calculateChecksum(data []byte) byte {
	var sum byte
	for _, b := range data {
		sum += b
	}
	return byte(0 - int8(sum))
}
