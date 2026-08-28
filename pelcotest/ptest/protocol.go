package main

import (
	"fmt"
	"strings"
)

// Pelco-D frame: FF addr cmd1 cmd2 data1 data2 checksum (7 bytes).
// cmd1 is 0x00 for everything this rotor documents; the action lives in cmd2.
// checksum = (addr + cmd1 + cmd2 + data1 + data2) & 0xFF.
const frameLen = 7

// Pelco-P carries the SAME logical fields in an 8-byte envelope:
// A0 addr cmd1 cmd2 data1 data2 AF checksum, where the checksum is the XOR of
// the first seven bytes (STX and ETX included). The manual calls this unit
// "Pelco-D/Pelco-P adaptive" — it answers in whichever protocol a frame
// arrived in, so the RX side accepts both and only TX framing is a mode.
const (
	frameLenP = 8
	pelcoSTX  = 0xA0
	pelcoETX  = 0xAF
)

// maxLogCol bounds every line Decode emits. The log pane is only
// (terminal width - menuWidth - 2) columns and the viewport hard-truncates
// with no wrap and no ellipsis, so a long line silently loses its tail — which
// used to hide the position word and the checksum verdict on an 80-column
// terminal, i.e. exactly the two fields the operator is reading the log for.
const maxLogCol = 50

type Frame [frameLen]byte

// Build assembles a Pelco-D frame and computes the checksum.
func Build(addr, cmd1, cmd2, d1, d2 byte) Frame {
	f := Frame{0xFF, addr, cmd1, cmd2, d1, d2}
	f[6] = byte(uint16(f[1]) + uint16(f[2]) + uint16(f[3]) + uint16(f[4]) + uint16(f[5]))
	return f
}

// Hex renders the frame as space-separated upper-case hex.
func (f Frame) Hex() string {
	parts := make([]string, frameLen)
	for i, b := range f {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, " ")
}

// ChkOK reports whether the stored checksum matches the summed bytes.
func (f Frame) ChkOK() bool {
	return f.checksum() == f[6]
}

func (f Frame) checksum() byte {
	return byte(uint16(f[1]) + uint16(f[2]) + uint16(f[3]) + uint16(f[4]) + uint16(f[5]))
}

// WrapP re-wraps a logically-built Pelco-D frame as an 8-byte Pelco-P wire
// frame: the address/cmd/data bytes shift right by one to make room for ETX,
// and the checksum becomes the XOR of bytes 1..7 (STX and ETX included). The
// same address byte is used in both protocols — the unit matches a single DIP
// address code regardless of protocol (strict Pelco-P gear is zero-indexed;
// this unit is assumed not to be, which is UNVERIFIED on real hardware — see
// the README's Pelco-P section and use raw hex entry to test addr-1 vs addr).
func WrapP(f Frame) []byte {
	w := []byte{pelcoSTX, f[1], f[2], f[3], f[4], f[5], pelcoETX, 0}
	w[7] = pXor(w)
	return w
}

// pXor is the Pelco-P checksum: XOR of the seven bytes before it.
func pXor(w []byte) byte {
	c := byte(0)
	for _, b := range w[:7] {
		c ^= b
	}
	return c
}

// pChkOK reports whether an 8-byte buffer is a well-formed Pelco-P frame.
func pChkOK(w []byte) bool {
	if len(w) != frameLenP || w[0] != pelcoSTX || w[6] != pelcoETX {
		return false
	}
	return pXor(w) == w[7]
}

// Word is data1..data2 read as a big-endian 16-bit value.
func (f Frame) Word() uint16 { return uint16(f[4])<<8 | uint16(f[5]) }

// TiltCal is an OPERATOR-SUPPLIED HYPOTHESIS about the tilt readback, not a
// known fact: the linear map raw = Raw0 + scale*elev, scale = (Raw90 - Raw0)/90.
//
// What is actually established about the tilt position response (cmd2 0x5B) on
// the 303Z/3050DZ is only negative:
//
//   - The manual's claim that it carries hundredths of a degree is FALSE. Every
//     real reading renders as an angle outside the head's 0..90° travel.
//   - The linear raw-encoder-count model is ALSO NOT established. It was fitted
//     to bench observations and then contradicted on the bench: re-checked
//     2026-08-27, elevation does not appear in the tilt word at all. Whatever
//     0x5B carries, it is not a function of elevation alone.
//
// So ptest asserts NOTHING about elevation by default. Pass -tilt-cal to try a
// map you want to test; the log then labels it "hyp:" so it can never be
// mistaken for a measurement. Pan (cmd2 0x59) really is Pelco-standard
// hundredths and is treated as such.
type TiltCal struct {
	Raw0, Raw90 float64
}

// Valid reports whether the operator supplied a hypothesis to test.
func (c TiltCal) Valid() bool { return c.Raw90 != c.Raw0 }

// Scale is raw counts per degree under the supplied hypothesis.
func (c TiltCal) Scale() float64 { return (c.Raw90 - c.Raw0) / 90 }

// Deg maps a raw tilt readback word to degrees under the supplied hypothesis.
func (c TiltCal) Deg(word uint16) float64 { return (float64(word) - c.Raw0) / c.Scale() }

// ParseHex parses user-entered hex ("FF 01 00 53 00 00 54", any separators)
// into a Frame.
func ParseHex(s string) (Frame, error) {
	w, err := parseHexBytes(s)
	if err != nil {
		return Frame{}, err
	}
	if len(w) != frameLen {
		return Frame{}, fmt.Errorf("need %d bytes, got %d", frameLen, len(w))
	}
	var f Frame
	copy(f[:], w)
	return f, nil
}

// ParseWireHex parses user-entered raw hex of either frame length (7 for D,
// 8 for P) and returns the exact bytes to put on the wire — raw entry is sent
// as typed, never re-framed.
func ParseWireHex(s string) ([]byte, error) {
	w, err := parseHexBytes(s)
	if err != nil {
		return nil, err
	}
	if len(w) != frameLen && len(w) != frameLenP {
		return nil, fmt.Errorf("need %d or %d bytes, got %d", frameLen, frameLenP, len(w))
	}
	return w, nil
}

// parseHexBytes splits on any non-hex-digit run and requires each field to be
// exactly two hex digits. A one-nibble field is rejected rather than
// zero-extended: "54" mistyped as "5" used to be sent silently as 0x05, which
// then reads back as a checksum error the operator chases for real.
func parseHexBytes(s string) ([]byte, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F')
	})
	w := make([]byte, len(fields))
	for i, fl := range fields {
		if len(fl) != 2 {
			return nil, fmt.Errorf("byte %d (%q): need exactly two hex digits", i+1, fl)
		}
		var b int
		if _, err := fmt.Sscanf(fl, "%x", &b); err != nil {
			return nil, fmt.Errorf("byte %d (%q): not a hex byte", i+1, fl)
		}
		w[i] = byte(b)
	}
	return w, nil
}

// ParamKind describes what user input a command needs before sending.
type ParamKind int

const (
	ParamNone    ParamKind = iota
	ParamDegrees           // 42/44: angle in degrees, sent as degrees*100 in data1..2
	ParamPreset            // preset number 0-255, sent in data2
	ParamSpeeds            // move: two hex speed bytes, default 20 20
	ParamHex               // raw frame entry
)

// Command is one entry of the command menu, straight from the doc sheet.
type Command struct {
	Name    string // menu label
	Desc    string // what the doc says it does
	Cmd2    byte   // command-2 byte (cmd1 is always 0x00 here)
	D1, D2  byte   // fixed data bytes (params may override)
	Param   ParamKind
	Confirm bool // ask y/n before sending (40 wipes device settings)
}

// Commands is the doc's command sheet. The angle commands (doc SN 40-44) are
// listed first; the rest follow in sheet order.
var Commands = []Command{
	{Name: "41 tilt query", Desc: "vertical angle query", Cmd2: 0x53},
	{Name: "43 pan query", Desc: "horizontal angle query", Cmd2: 0x51},
	{Name: "42 tilt set", Desc: "set vertical angle (IGNORED by 303Z)", Cmd2: 0x4D, Param: ParamDegrees},
	{Name: "44 pan set", Desc: "set horizontal angle (IGNORED by 303Z)", Cmd2: 0x4B, Param: ParamDegrees},
	{Name: "40 defaults+selftest", Desc: "restore defaults, start PTZ self-test", Cmd2: 0x07, D2: 0x7D, Confirm: true},

	{Name: "stop", Desc: "stop all movement", Cmd2: 0x00},
	{Name: "up", Desc: "move up (tilt speed)", Cmd2: 0x08, Param: ParamSpeeds},
	{Name: "down", Desc: "move down (tilt speed)", Cmd2: 0x10, Param: ParamSpeeds},
	{Name: "left", Desc: "move left (pan speed)", Cmd2: 0x04, Param: ParamSpeeds},
	{Name: "right", Desc: "move right (pan speed)", Cmd2: 0x02, Param: ParamSpeeds},

	{Name: "preset set N", Desc: "set preset point N", Cmd2: 0x03, Param: ParamPreset},
	{Name: "preset call N", Desc: "go to preset point N", Cmd2: 0x07, Param: ParamPreset},

	{Name: "105 set", Desc: "disable PTZ self-check", Cmd2: 0x03, D2: 0x69},
	{Name: "105 call", Desc: "enable PTZ self-check", Cmd2: 0x07, D2: 0x69},
	{Name: "17 set", Desc: "set limit-scan left start point", Cmd2: 0x03, D2: 0x11},
	{Name: "18 set", Desc: "set limit-scan right start point", Cmd2: 0x03, D2: 0x12},
	{Name: "19 call", Desc: "start limit scan", Cmd2: 0x07, D2: 0x13},
	{Name: "22 set", Desc: "set line-scan speed", Cmd2: 0x03, D2: 0x16},
	{Name: "70 call", Desc: "cruise stop time 5 s", Cmd2: 0x07, D2: 0x46},
	{Name: "71 call", Desc: "cruise stop time 10 s", Cmd2: 0x07, D2: 0x47},
	{Name: "72 call", Desc: "cruise stop time 15 s", Cmd2: 0x07, D2: 0x48},
	{Name: "83 call", Desc: "start cruise line 1 (presets 1-16)", Cmd2: 0x07, D2: 0x53},
	{Name: "84 call", Desc: "start cruise line 2 (presets 33-48)", Cmd2: 0x07, D2: 0x54},
	{Name: "89 set", Desc: "start max-angle line (set)", Cmd2: 0x03, D2: 0x59},
	{Name: "89 call", Desc: "start max-angle line scan", Cmd2: 0x07, D2: 0x59},
	{Name: "AUX1 on", Desc: "aux on (doc sends aux number 0)", Cmd2: 0x09},
	{Name: "AUX2 off", Desc: "aux off (doc sends aux number 0)", Cmd2: 0x0B},
	{Name: "110 set", Desc: "guard position on (preset 1)", Cmd2: 0x03, D2: 0x6E},
	{Name: "110 call", Desc: "guard position off", Cmd2: 0x07, D2: 0x6E},
	// The doc's frame for SN 111 is FF 01 00 07 00 6F 77 — data1 is 00 and
	// data2 is the 0x6F selector, so the "29<N<251 s" note in the sheet is NOT
	// an in-frame parameter. ptest used to treat it as one and wrote the
	// seconds over data2, which destroyed the selector for every value except
	// 111 (0x6F == 111 decimal, the coincidence its own test baked in).
	{Name: "111 call", Desc: "guard return time (no in-frame parameter)", Cmd2: 0x07, D2: 0x6F},
	{Name: "120 call", Desc: "clear all preset points", Cmd2: 0x07, D2: 0x78, Confirm: true},

	{Name: "raw frame", Desc: "send 7 bytes entered as hex", Param: ParamHex},
}

// dangerousPresets maps a preset-call number to why calling it is destructive.
// Every "NN call" entry in the doc sheet is really preset-call NN, so the plain
// "preset call N" menu entry can reach the confirm-gated commands by number
// unless it is checked at send time.
var dangerousPresets = map[byte]string{
	0x7D: "restores factory defaults and starts the PTZ self-test — a self-test re-home invalidates the tilt calibration",
	0x78: "clears all preset points",
}

// DangerousReason returns why sending this command with this input is
// destructive, or "" if it is not.
func DangerousReason(c Command, input string) string {
	if c.Confirm {
		return "wipes device settings"
	}
	if c.Cmd2 != 0x07 {
		return ""
	}
	f, err := BuildFrame(1, c, input)
	if err != nil {
		return ""
	}
	if f[4] != 0x00 {
		return ""
	}
	return dangerousPresets[f[5]]
}

// BuildWire builds the exact bytes to put on the wire for a command. When
// useP is set the logically D-built frame is re-wrapped as an 8-byte Pelco-P
// envelope (WrapP); raw hex entry is always sent as typed (7 or 8 bytes),
// never re-framed. The returned Frame is the logical D-fields view of what is
// sent, for the log in both protocols.
func BuildWire(addr byte, c Command, input string, useP bool) ([]byte, Frame, error) {
	if c.Param == ParamHex {
		w, err := ParseWireHex(input)
		if err != nil {
			return nil, Frame{}, fmt.Errorf("raw frame: %v", err)
		}
		if len(w) == frameLenP {
			return w, Frame{w[0], w[1], w[2], w[3], w[4], w[5], w[7]}, nil
		}
		var f Frame
		copy(f[:], w)
		return w, f, nil
	}
	f, err := BuildFrame(addr, c, input)
	if err != nil {
		return nil, Frame{}, err
	}
	if useP {
		return WrapP(f), f, nil
	}
	return f[:], f, nil
}

// BuildFrame turns a selected command plus optional user input into a frame.
// input is whatever is currently in the parameter line ("" when unused).
func BuildFrame(addr byte, c Command, input string) (Frame, error) {
	switch c.Param {
	case ParamHex:
		f, err := ParseHex(input)
		if err != nil {
			return Frame{}, fmt.Errorf("raw frame: %v", err)
		}
		return f, nil
	case ParamDegrees:
		deg, err := parseFloatStrict(input)
		if err != nil {
			return Frame{}, fmt.Errorf("degrees: %v", err)
		}
		centi := int(deg*100 + 0.5)
		if centi < 0 || centi > 0xFFFF {
			return Frame{}, fmt.Errorf("degrees %.2f out of range 0..655.35", deg)
		}
		return Build(addr, 0x00, c.Cmd2, byte(centi>>8), byte(centi)), nil
	case ParamPreset:
		n, err := parseIntStrict(input)
		if err != nil || n < 0 || n > 255 {
			return Frame{}, fmt.Errorf("preset: %q is not 0..255", input)
		}
		return Build(addr, 0x00, c.Cmd2, 0x00, byte(n)), nil
	case ParamSpeeds:
		pan, tilt := byte(0x20), byte(0x20)
		if input != "" {
			fields := strings.Fields(input)
			if len(fields) != 2 {
				return Frame{}, fmt.Errorf("speeds: %q is not two hex bytes (e.g. \"20 20\")", input)
			}
			var vals [2]byte
			for i, fl := range fields {
				var v int
				if _, err := fmt.Sscanf(fl, "%x", &v); err != nil || len(fl) > 2 {
					return Frame{}, fmt.Errorf("speeds: %q is not a hex byte", fl)
				}
				if v < 0 || v > 0x3F {
					return Frame{}, fmt.Errorf("speeds must be hex 00..3F")
				}
				vals[i] = byte(v)
			}
			pan, tilt = vals[0], vals[1]
		}
		// Up/down ride on the tilt byte (data2), left/right on the pan byte
		// (data1) — the other byte stays 0 so only one axis moves, matching
		// the doc's example frames. Only the byte for the selected axis is
		// used; the other value is ignored, which is why the prompt names
		// which one applies.
		d1, d2 := byte(0), byte(0)
		switch c.Cmd2 {
		case 0x08, 0x10: // up, down
			d2 = tilt
		case 0x04, 0x02: // left, right
			d1 = pan
		}
		return Build(addr, 0x00, c.Cmd2, d1, d2), nil
	default:
		return Build(addr, 0x00, c.Cmd2, c.D1, c.D2), nil
	}
}

// parseFloatStrict rejects trailing junk. fmt.Sscanf("%f") stops at the first
// bad rune and reports no error, so "45,5" used to send 45.00° and "9 0" 9.00°
// — a decimal comma is a realistic typo and the frame went out silently wrong.
func parseFloatStrict(s string) (float64, error) {
	var v float64
	var rest string
	n, _ := fmt.Sscanf(s, "%f%s", &v, &rest)
	if n == 0 {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if rest != "" {
		return 0, fmt.Errorf("%q has trailing junk (%q) — use a decimal point, not a comma", s, rest)
	}
	if v != v { // NaN passed the old range check and sent 0.00°
		return 0, fmt.Errorf("%q is not a finite number", s)
	}
	return v, nil
}

// parseIntStrict rejects trailing junk, so "0x7D" no longer becomes preset 0.
func parseIntStrict(s string) (int, error) {
	var v int
	var rest string
	n, _ := fmt.Sscanf(s, "%d%s", &v, &rest)
	if n == 0 || rest != "" {
		return 0, fmt.Errorf("%q is not a plain decimal number", s)
	}
	return v, nil
}

// RxFrame is one assembled frame off the wire: a 7-byte Pelco-D frame
// (P == false) or an 8-byte Pelco-P frame (P == true). Frame holds the
// normalized logical fields — for P frames [0]=STX, [1..5] the addr/cmd/data
// bytes as they sit on the wire, [6] the XOR checksum. Hex/ChkOK work per
// protocol; Word() decodes identically in both (the position word occupies
// the same logical fields).
type RxFrame struct {
	Frame
	P    bool
	Wire []byte
}

// Hex renders the exact wire bytes. It is driven by len(Wire), not by the
// protocol flag: an 8-byte raw entry that does not start with 0xA0 is not a
// Pelco-P frame but all eight bytes still went out, and the log has to show
// what was sent.
func (r RxFrame) Hex() string {
	w := r.Wire
	if len(w) == 0 {
		w = r.Frame[:]
	}
	parts := make([]string, len(w))
	for i, b := range w {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, " ")
}

// ChkOK reports whether the frame's checksum validates in its own protocol.
func (r RxFrame) ChkOK() bool {
	if r.P {
		return pChkOK(r.Wire)
	}
	if len(r.Wire) == frameLenP {
		return false // 8 bytes that are not a P envelope: no checksum rule applies
	}
	return r.Frame.ChkOK()
}

// chkByte is the checksum byte actually on the wire. For a Pelco-P frame that
// is wire[7] (the XOR), not Frame[6]: on the TX path Frame is the logical
// D-fields view, whose [6] holds the additive D sum, so the log used to print
// chk=54 next to a wire that carried 5D.
func (r RxFrame) chkByte() byte {
	if r.P && len(r.Wire) >= frameLenP {
		return r.Wire[7]
	}
	if len(r.Wire) == frameLen {
		return r.Wire[6]
	}
	return r.Frame[6]
}

// Noise is a run of bytes that could not be assembled into a valid frame.
type Noise []byte

// badCap chunks a very long resync run inside a single Feed so one huge read of
// garbage (a wrong-baud link) is reported in readable pieces. Rejected bytes
// are no longer withheld beyond the Feed that rejected them.
const badCap = 256

// Event is one thing that came off the wire, in wire order. Feed returns a
// single ordered slice rather than separate frame and noise slices: the UI used
// to log every noise run in a read before every frame in the same read, so the
// transcript contradicted the wire for exactly the corrupted traffic the
// operator is trying to read.
type Event struct {
	Frame   RxFrame
	Noise   Noise // non-nil ⇒ this event is an unframeable run, Frame unset
	Partial bool  // a stalled incomplete frame flushed after a receive gap
}

// IsNoise reports whether the event is an unframeable run rather than a frame.
func (e Event) IsNoise() bool { return e.Noise != nil }

// Assembler reassembles a raw byte stream into frames. It syncs on the start
// byte (0xFF → 7-byte Pelco-D, 0xA0 → 8-byte Pelco-P) and validates the
// matching checksum (additive sum vs XOR); on a bad checksum the leading byte
// is dropped and scanning resumes. Both protocols are always accepted on RX —
// the unit is D/P adaptive and answers in the protocol it was addressed in.
//
// An incomplete frame is held only until the next receive gap. Without that
// bound a truncated reply stayed at the head of the buffer and merged with the
// next reply, whose 0xFF start byte landed where the lost checksum byte had
// been — producing a checksum-VALID frame carrying a fabricated position word
// while the genuine reply was walked off byte by byte. Call FlushIdle after a
// receive gap of ~1.5 frame times to close that window.
type Assembler struct {
	buf []byte
	bad []byte // bytes consumed while resyncing, reported as noise
}

// Feed consumes new bytes and returns the frames and noise runs it can, in
// wire order.
func (a *Assembler) Feed(data []byte) []Event {
	a.buf = append(a.buf, data...)
	var ev []Event
	for len(a.buf) > 0 {
		isP := a.buf[0] == pelcoSTX
		need := frameLen
		if isP {
			need = frameLenP
		} else if a.buf[0] != 0xFF {
			a.drop()
			ev = a.capBad(ev)
			continue
		}
		if len(a.buf) < need {
			break // wait for the rest of the frame (or for FlushIdle)
		}
		wire := make([]byte, need)
		copy(wire, a.buf[:need])
		var f Frame
		ok := false
		if isP {
			ok = pChkOK(wire)
			f = Frame{wire[0], wire[1], wire[2], wire[3], wire[4], wire[5], wire[7]}
		} else {
			copy(f[:], wire)
			ok = f.ChkOK()
		}
		if !ok {
			a.drop()
			ev = a.capBad(ev)
			continue
		}
		ev = a.flushBad(ev) // noise precedes the frame that follows it
		ev = append(ev, Event{Frame: RxFrame{Frame: f, P: isP, Wire: wire}})
		a.buf = a.buf[need:]
	}
	// Bytes that have already been definitively rejected are never withheld:
	// they used to be buffered until 256 accumulated, so a single corrupted
	// reply produced no log output at all and looked like "no answer".
	return a.flushBad(ev)
}

// FlushIdle reports an incomplete frame that has been sitting in the assembler
// with no further traffic. The UI calls it after a receive gap so a truncated
// reply is surfaced as a partial instead of merging with the next one.
func (a *Assembler) FlushIdle() []Event {
	ev := a.flushBad(nil)
	if len(a.buf) > 0 {
		ev = append(ev, Event{Noise: Noise(append([]byte(nil), a.buf...)), Partial: true})
		a.buf = nil
	}
	return ev
}

// Pending reports whether the assembler is holding anything that a receive gap
// should flush.
func (a *Assembler) Pending() bool { return len(a.buf) > 0 || len(a.bad) > 0 }

func (a *Assembler) drop() {
	a.bad = append(a.bad, a.buf[0])
	a.buf = a.buf[1:]
}

// flushBad emits the accumulated resync bytes as one noise run, if any.
func (a *Assembler) flushBad(ev []Event) []Event {
	if len(a.bad) == 0 {
		return ev
	}
	ev = append(ev, Event{Noise: Noise(a.bad)})
	a.bad = nil
	return ev
}

// capBad chunks a resync run that grows past badCap within a single Feed.
func (a *Assembler) capBad(ev []Event) []Event {
	if len(a.bad) < badCap {
		return ev
	}
	return a.flushBad(ev)
}

// docHint returns the doc's (untrusted) interpretation of a frame plus, for the
// tilt response, the calibrated raw-encoder-count reading. It returns nil if
// the doc says nothing about the frame. Everything the doc claims is tentative:
// the whole point of ptest is to check it against real device traffic.
func docHint(f Frame, cal TiltCal) []string {
	word := f.Word()
	switch f[3] {
	case 0x5B:
		// The doc claims hundredths; that is disproved. No replacement model is
		// established either — elevation does not appear in this word. State the
		// doc's claim, flag it when impossible, and otherwise assert nothing.
		lines := []string{fmt.Sprintf("doc: tilt resp, word/100 = %.2f°", float64(word)/100)}
		if d := float64(word) / 100; d < 0 || d > 90 {
			lines = append(lines, "     └ impossible: head travels 0..90°")
		}
		lines = append(lines, "     meaning of this word is UNKNOWN")
		if cal.Valid() {
			lines = append(lines,
				fmt.Sprintf("hyp: raw %d → %.2f° (-tilt-cal)", word, cal.Deg(word)),
				fmt.Sprintf("     UNVERIFIED: %.3f counts/°", cal.Scale()))
		}
		return lines
	case 0x59:
		return []string{fmt.Sprintf("doc: pan resp, word/100 = %.2f° (tentative)", float64(word)/100)}
	case 0x53:
		return []string{"doc: tilt angle query"}
	case 0x51:
		return []string{"doc: pan angle query"}
	case 0x4D:
		return []string{
			fmt.Sprintf("doc: set tilt %.2f° (deg*100)", float64(word)/100),
			"     └ 303Z ignores 0x4D (confirmed live)",
		}
	case 0x4B:
		return []string{
			fmt.Sprintf("doc: set pan %.2f° (deg*100)", float64(word)/100),
			"     └ 303Z ignores 0x4B (confirmed live)",
		}
	}
	return nil
}

// Decode renders a frame for the log: raw hex first, then a raw field
// breakdown, then the doc hint (if any). Decoding is never presented as fact.
// Both protocols share the logical field layout; a P envelope is labeled as
// such. Every line stays within maxLogCol so nothing is truncated off the
// right edge of the log pane on a narrow terminal.
func Decode(r RxFrame, dir string, cal TiltCal) []string {
	lines := []string{
		fmt.Sprintf("%s %s", dir, r.Hex()),
		fmt.Sprintf("   raw: addr=%02X cmd1=%02X cmd2=%02X d1=%02X d2=%02X",
			r.Frame[1], r.Frame[2], r.Frame[3], r.Frame[4], r.Frame[5]),
		fmt.Sprintf("   word=%04X (%d)  d1=%d d2=%d  chk=%02X %s",
			r.Word(), r.Word(), r.Frame[4], r.Frame[5], r.chkByte(), chkStr(r)),
	}
	for _, h := range docHint(r.Frame, cal) {
		lines = append(lines, "   "+h)
	}
	switch {
	case r.P:
		lines = append(lines, "   pelco-p: A0/AF envelope, XOR chk")
	case len(r.Wire) == frameLenP:
		lines = append(lines, "   8 bytes, not an A0/AF envelope")
	default:
		// Only the standard pan/tilt/zoom command uses cmd2 as a bitfield.
		// Extended commands (preset, aux, scan, position) are opcodes and are
		// identified by bit 0 being set, so decoding them as direction bits
		// printed nonsense: "preset call" as "right,left", "AUX off" as
		// "right,up".
		if r.Frame[3] < 0x30 && r.Frame[3]&0x01 == 0 {
			if bits := decodeBits(r.Frame[3]); bits != "" {
				lines = append(lines, "   pelco-d bits: "+bits)
			}
		}
	}
	return lines
}

func chkStr(r RxFrame) string {
	if r.ChkOK() {
		return "ok"
	}
	if r.P {
		if r.Wire[6] != pelcoETX {
			return fmt.Sprintf("BAD (byte 7 is %02X, not ETX AF)", r.Wire[6])
		}
		return fmt.Sprintf("BAD (want %02X)", pXor(r.Wire))
	}
	if len(r.Wire) == frameLenP {
		return "unverifiable (8 bytes, no A0 STX)"
	}
	return fmt.Sprintf("BAD (want %02X)", r.checksum())
}

func decodeBits(cmd2 byte) string {
	var parts []string
	if cmd2 == 0 {
		parts = append(parts, "stop/all-clear")
	}
	if cmd2&0x02 != 0 {
		parts = append(parts, "right")
	}
	if cmd2&0x04 != 0 {
		parts = append(parts, "left")
	}
	if cmd2&0x08 != 0 {
		parts = append(parts, "up")
	}
	if cmd2&0x10 != 0 {
		parts = append(parts, "down")
	}
	if cmd2&0x20 != 0 {
		parts = append(parts, "zoom-tele")
	}
	return strings.Join(parts, ",")
}
