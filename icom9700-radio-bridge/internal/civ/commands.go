package civ

import (
	"fmt"
)

// Command builders + reply parsers for the KTD-10 command table. Every
// builder returns the raw CI-V frame for SendCIV; the companion Parse
// functions decode the radio's answer. Parsers are strict and typed —
// garbage is an error, never a guessed value (R4.4's bus-side sibling).

// CmdReadFreq builds `03`.
func CmdReadFreq() []byte { return BuildFrame(CivCmdReadFreq, nil) }

// CmdSetFreq builds `05 <10 BCD>`.
func CmdSetFreq(hz uint64) ([]byte, error) {
	bcd, err := BCD10Encode(hz)
	if err != nil {
		return nil, err
	}
	return BuildFrame(CivCmdSetFreq, bcd), nil
}

// ParseFreqReply decodes a frequency value (a `00` reply's or broadcast's
// 5-byte BCD).
func ParseFreqReply(data []byte) (uint64, error) { return BCD10Decode(data) }

// CmdReadMode builds `04`.
func CmdReadMode() []byte { return BuildFrame(CivCmdReadMode, nil) }

// CmdSetMode builds `06 <mode> <filter>`; data-mode is the separate
// CmdSetDataMode modifier.
func CmdSetMode(mode string) ([]byte, error) {
	mb, ok := ModeToByte(mode)
	if !ok {
		return nil, fmt.Errorf("civ: unsupported mode %q", mode)
	}
	return BuildFrame(CivCmdSetMode, []byte{mb, DefaultFilter}), nil
}

// ParseModeReply decodes a mode+filter reply/broadcast (2 bytes). An
// unsupported mode byte returns ok=false — the field is omitted upstream,
// never published raw (R7).
func ParseModeReply(data []byte) (mode string, filter byte, ok bool, err error) {
	if len(data) != 2 {
		return "", 0, false, fmt.Errorf("civ: mode reply must be 2 bytes, got %d", len(data))
	}
	mode, ok = ModeFromByte(data[0])
	if !ok {
		return "", data[1], false, nil
	}
	return mode, data[1], true, nil
}

// CmdSetDataMode builds the data-mode on/off modifier `06 <00/01> <filter>`
// (KTD-10 command table).
func CmdSetDataMode(on bool) []byte {
	v := byte(0x00)
	if on {
		v = 0x01
	}
	return BuildFrame(CivCmdSetMode, []byte{v, DefaultFilter})
}

// CmdSelectVFO builds `07 D0` (main) / `07 D1` (sub).
func CmdSelectVFO(vfo string) ([]byte, error) {
	switch vfo {
	case "main":
		return BuildFrame(CivCmdSelectVFO, []byte{SubSelectMain}), nil
	case "sub":
		return BuildFrame(CivCmdSelectVFO, []byte{SubSelectSub}), nil
	default:
		return nil, fmt.Errorf("civ: unknown vfo %q", vfo)
	}
}

// CmdReadSelectedVFO builds `07 D2 00`.
func CmdReadSelectedVFO() []byte {
	return BuildFrame(CivCmdSelectVFO, []byte{SubReadSelectedVFO, 0x00})
}

// ParseVFOReply decodes the selected-VFO read answer (`00` main, `01` sub).
func ParseVFOReply(data []byte) (string, error) {
	if len(data) != 1 {
		return "", fmt.Errorf("civ: vfo reply must be 1 byte, got %d", len(data))
	}
	switch data[0] {
	case 0x00:
		return "main", nil
	case 0x01:
		return "sub", nil
	default:
		return "", fmt.Errorf("civ: unknown vfo byte %02x", data[0])
	}
}

// CmdSatelliteMode builds `16 5A 00/01` (set) — no data = read.
func CmdSatelliteMode(on bool) []byte {
	v := byte(0x00)
	if on {
		v = 0x01
	}
	return BuildFrame(CivCmdPreamp, []byte{SubSatelliteMode, v})
}

// CmdReadSatelliteMode builds `16 5A` (no data).
func CmdReadSatelliteMode() []byte {
	return BuildFrame(CivCmdPreamp, []byte{SubSatelliteMode})
}

// ParseSatelliteReply decodes the satellite-mode read answer.
func ParseSatelliteReply(data []byte) (bool, error) {
	if len(data) != 1 {
		return false, fmt.Errorf("civ: satellite reply must be 1 byte, got %d", len(data))
	}
	switch data[0] {
	case 0x00:
		return false, nil
	case 0x01:
		return true, nil
	default:
		return false, fmt.Errorf("civ: unknown satellite byte %02x", data[0])
	}
}

// CmdPTT builds `1C 00 01/00` (on/off). The bus-side arm gate lives in the
// bridge's safety core (U6) — this is the raw wire builder.
func CmdPTT(on bool) []byte {
	v := byte(0x00)
	if on {
		v = 0x01
	}
	return BuildFrame(CivCmdPTT, []byte{SubPTT, v})
}

// CmdTransceiverID builds `19` (liveness/identity probe).
func CmdTransceiverID() []byte { return BuildFrame(CivCmdReadID, nil) }

// CmdReadSMeter / CmdReadSWR / CmdReadALC build `15 02/12/13`.
func CmdReadSMeter() []byte { return BuildFrame(CivCmdReadMeter, []byte{SubSMeter}) }
func CmdReadSWR() []byte    { return BuildFrame(CivCmdReadMeter, []byte{SubSWR}) }
func CmdReadALC() []byte    { return BuildFrame(CivCmdReadMeter, []byte{SubALC}) }

// ParseMeter decodes an 0-255 meter value (S-meter: S9 = 120). Real
// firmware answers `15 <sub> <hi> <lo>` (two bytes, big-endian, bench
// 2026-09-20); the fake's single-byte legacy form is still accepted.
func ParseMeter(data []byte) (int, error) {
	switch len(data) {
	case 1:
		return int(data[0]), nil
	case 2:
		return int(data[0])<<8 | int(data[1]), nil
	default:
		return 0, fmt.Errorf("civ: meter reply must be 1-2 bytes, got %d", len(data))
	}
}

// CmdReadPreamp / CmdReadAttenuator build `16 02` / `16 11`.
func CmdReadPreamp() []byte     { return BuildFrame(CivCmdPreamp, []byte{SubPreamp}) }
func CmdReadAttenuator() []byte { return BuildFrame(CivCmdPreamp, []byte{SubAttenuator}) }

// ParsePreamp decodes the preamp answer: 00 off, 01/02 = P.AMP on.
func ParsePreamp(data []byte) (bool, error) {
	if len(data) != 1 {
		return false, fmt.Errorf("civ: preamp reply must be 1 byte, got %d", len(data))
	}
	return data[0] != 0x00, nil
}

// ParseAttenuator decodes the attenuator answer: 00 off, 01/02 = ATT on.
func ParseAttenuator(data []byte) (bool, error) {
	if len(data) != 1 {
		return false, fmt.Errorf("civ: attenuator reply must be 1 byte, got %d", len(data))
	}
	return data[0] != 0x00, nil
}

// CmdSetRFPower builds `14 0A <0-255>` — RF output power for the currently
// selected band's VFO. OUT OF SCOPE GUARD: the 14-family sub 0A has nothing
// to do with the main-power commands `18 01/00`, which this codec NEVER
// emits (a U3 test pins that); the plan's Go/No-Go posture depends on it.
func CmdSetRFPower(level byte) []byte {
	return BuildFrame(CivCmdSetRFPower, []byte{SubRFPower, level})
}

// CmdSetTransceive builds `1A 05 0127` / `1A 05 0126` (on/off) — enables
// the radio's unsolicited transceive broadcasts so /state updates without
// polling (deploy gate 3's CI-V Transceive prerequisite, settable in the
// radio menu; the bridge sets it once per session as belt-and-braces).
func CmdSetTransceive(on bool) []byte {
	v := byte(0x26)
	if on {
		v = 0x27
	}
	return BuildFrame(CivCmdSetTransceive, []byte{SubTransceiveToggle, 0x01, v})
}

// CmdPowerOn builds the IC-9700 remote wake `1A 05 02 01` (standby -> ON).
// Sent blind: a standby radio answers no CI-V ack, so there is no
// round-trip. The frame bytes are configurable (audio.power_on_frame) —
// this builder is the documented default.
func CmdPowerOn() []byte { return BuildFrame(CivCmdPower, []byte{0x05, 0x02, 0x01}) }
