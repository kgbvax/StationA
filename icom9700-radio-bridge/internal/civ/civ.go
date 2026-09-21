package civ

import (
	"encoding/hex"
	"fmt"
)

// CI-V layer (KTD-10): frames `FE FE <dest> <src> <cmd> [<sub>] [<data>] <term> FD`
// with radio address A2 and controller address E0. Replies terminate with FB
// (ack + data) or FA (NG rejection) before the FD end marker; unsolicited
// transceive broadcasts terminate with FD directly.
//
// Parsing NEVER scans for the FD end marker: the transport hands the codec
// exactly one datalen-delimited payload, and the frame's extent is the
// payload's extent — scope/keyer payloads can embed FD bytes inside data,
// and a scanner would silently truncate them (KTD-10).
const (
	CIVAddrRadio      = 0xA2
	CIVAddrController = 0xE0
)

// CI-V command bytes (this bridge's set; KTD-10 command table). The
// Civ* prefix distinguishes these byte constants from the builder
// functions of the same command in commands.go.
const (
	CivCmdFreqBroadcast byte = 0x00 // radio -> controller: freq changes AND read-freq replies
	CivCmdModeBroadcast byte = 0x01 // radio -> controller: mode changes AND read-mode replies
	CivCmdReadFreq      byte = 0x03
	CivCmdReadMode      byte = 0x04
	CivCmdSetFreq       byte = 0x05
	CivCmdSetMode       byte = 0x06 // mode+filter, and the data-mode on/off modifier
	CivCmdSelectVFO     byte = 0x07 // sub D0 main / D1 sub / D2 00 read-selected
	CivCmdSetRFPower    byte = 0x14 // sub 0A: RF output power 0-255
	CivCmdReadMeter     byte = 0x15 // sub 02 S-meter, 12 SWR, 13 ALC, 14 comp
	CivCmdPreamp        byte = 0x16 // sub 02 preamp; 11 attenuator; 5A satellite mode
	CivCmdReadID        byte = 0x19 // transceiver ID probe
	CivCmdPTT           byte = 0x1C // sub 00: on/off + transceive
	CivCmdSetTransceive byte = 0x1A // sub 05 01 27: CI-V Transceive on/off
	CivCmdPower         byte = 0x1A // sub 05 02 01: power ON from standby (IC-9700 remote wake; frame configurable — see audio.power_on_frame)

	// OUT OF SCOPE (KTD-10): main power on/off 18 01/00 is never emitted by
	// the bridge — a U3 test pins that nothing in the codec produces it.
	// Main/sub exchange 07 B0 and split 0F are unused in v1.
)

// Sub-command bytes.
const (
	SubSelectMain       byte = 0xD0
	SubSelectSub        byte = 0xD1
	SubReadSelectedVFO  byte = 0xD2
	SubSMeter           byte = 0x02
	SubSWR              byte = 0x12
	SubALC              byte = 0x13
	SubRFPower          byte = 0x0A
	SubPTT              byte = 0x00
	SubSatelliteMode    byte = 0x5A
	SubPreamp           byte = 0x02
	SubAttenuator       byte = 0x11
	SubTransceiveToggle byte = 0x05
)

// Terminator bytes.
const (
	TerminatorFrame byte = 0xFD
	TerminatorOK    byte = 0xFB
	TerminatorNG    byte = 0xFA
)

// Frame is one parsed CI-V frame: command, optional sub/command-dependent
// bytes, data, and the reply terminator.
type Frame struct {
	Cmd        byte
	Sub        []byte // everything between the command and the terminator
	Terminator byte   // FB (ok/reply), FA (rejection), FD (broadcast/command end)
	Direct     bool   // addressed to the controller (E0 A2) — a direct reply
}

// IsNG reports a radio rejection (FA).
func (f Frame) IsNG() bool { return f.Terminator == TerminatorNG }

// AsNG converts a rejected frame into the typed error the bridge surfaces
// into /state.error.
func (f Frame) AsNG() error {
	sub := ""
	if len(f.Sub) > 0 {
		sub = fmt.Sprintf(" % 02x", f.Sub)
	}
	return &NGError{Cmd: f.Cmd, Sub: sub}
}

// NGError is a typed radio rejection — the wire's FA answer.
type NGError struct {
	Cmd byte
	Sub string
}

func (e *NGError) Error() string {
	return fmt.Sprintf("civ: command %02x%s rejected (NG)", e.Cmd, e.Sub)
}

// BuildFrame assembles a controller -> radio command frame
// (FE FE A2 E0 <cmd> [<sub/data>] FD).
func BuildFrame(cmd byte, sub []byte) []byte {
	p := make([]byte, 0, 6+len(sub))
	p = append(p, 0xFE, 0xFE, CIVAddrRadio, CIVAddrController, cmd)
	p = append(p, sub...)
	return append(p, TerminatorFrame)
}

// ParseFrame parses one radio -> controller CI-V payload: a direct reply
// (FE FE E0 A2 ...) or a transceive broadcast (FE FE FE 00 ...). The
// payload's extent IS the frame's extent — embedded FD bytes in data parse
// correctly because nothing scans (see package framing rule).
func ParseFrame(p []byte) (Frame, error) {
	if len(p) < 6 {
		return Frame{}, fmt.Errorf("civ: frame too short (%d bytes)", len(p))
	}
	if p[0] != 0xFE || p[1] != 0xFE {
		return Frame{}, fmt.Errorf("civ: missing FE FE preamble (% x)", p[:2])
	}
	var cmdIdx int
	var direct bool
	switch {
	case p[2] == 0xFE: // broadcast: FE FE FE <00> <cmd> ...
		cmdIdx = 4
	case p[2] == CIVAddrController && p[3] == CIVAddrRadio: // direct reply
		cmdIdx = 4
		direct = true
	case p[2] == 0x00 && p[3] == CIVAddrRadio: // broadcast with explicit dest 00
		cmdIdx = 4
	default:
		return Frame{}, fmt.Errorf("civ: unexpected addressing % 02x", p[2:4])
	}
	if p[len(p)-1] != TerminatorFrame {
		return Frame{}, fmt.Errorf("civ: missing FD end marker")
	}
	f := Frame{Cmd: p[cmdIdx], Direct: direct}
	body := p[cmdIdx+1 : len(p)-1]
	if len(body) == 0 {
		// Bare acknowledge: the real IC-9700 answers a set/select command
		// with FE FE E0 A2 FB (or FA for NG) — the ack code rides in the
		// COMMAND slot with no body (bench 2026-09-20: 9x `fe fe e0 a2 fb
		// fd` for 24 commands; the fake's cmd-echo form masked this).
		switch f.Cmd {
		case TerminatorOK:
			f.Terminator = TerminatorOK
		case TerminatorNG:
			f.Terminator = TerminatorNG
		default:
			f.Terminator = TerminatorFrame
		}
		return f, nil
	}
	f.Terminator = body[len(body)-1]
	switch f.Terminator {
	case TerminatorOK, TerminatorNG:
		f.Sub = body[:len(body)-1]
	default:
		f.Terminator = TerminatorFrame
		f.Sub = body
	}
	return f, nil
}

// hexOf renders bytes for error messages.
func hexOf(b []byte) string { return hex.EncodeToString(b) }
