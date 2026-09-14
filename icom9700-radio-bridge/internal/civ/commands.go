package civ

// The reply side of the command table: a strict dispatcher on the command
// bytes, typed extractors for the payload each command answers with, and
// the typed NG rejection.
//
// Parse contract (plan U3): reply dispatch on command bytes; malformed or
// foreign or unknown frames are counted (Codec.Dropped) and dropped — never
// panicked on, never partially interpreted. Framing itself is already the
// transport's datalen-based job (never FD-scanning, KTD-10); Parse receives
// exactly one datalen-delimited CI-V frame.

import (
	"errors"
	"fmt"
)

// ReplyKind classifies a parsed frame.
type ReplyKind uint8

const (
	KindAck  ReplyKind = iota // FB — the radio accepted the previous command
	KindNG                    // FA — typed rejection, see NGError
	KindData                  // a read reply or a transceive broadcast
)

// Reply is one parsed radio frame. Data holds the bytes after cmd (and sub
// when the command has one); the extractors below interpret it per command.
type Reply struct {
	Kind ReplyKind
	Cmd  byte
	Sub  byte // meaningful only for the sub-command families (07/11/14/15/16/19/1a/1c)
	Data []byte
}

// replySpec is the shape one command's frames must have. A frame matching
// the command byte but not the shape is malformed (counted and dropped).
type replySpec struct {
	name string
	// hasSub: the byte after cmd is a sub command, not data.
	hasSub bool
	// subOptional: the radio may omit the sub (19 answers with the bare
	// legacy form 19 <id> as well as 19 00 <id>); a single byte after cmd
	// is then data, not sub.
	subOptional bool
	// subLens maps sub command -> accepted data-area lengths.
	subLens map[byte][]int
	// lens is the accepted data-area lengths when the command has no sub.
	lens []int
}

// replySpecs dispatches on the command byte. Commands absent here (06 set
// mode echoes, 25/26, scope data, ...) are unknown: counted and dropped —
// the bridge reads the mode via 04 and never opens the scope stream.
var replySpecs = map[byte]replySpec{
	CmdFreqTransceive: {name: "00 frequency transceive", lens: []int{5}},
	CmdModeTransceive: {name: "01 mode transceive", lens: []int{1, 2}}, // filter byte may be omitted
	CmdReadFreq:       {name: "03 read frequency", lens: []int{5}},
	CmdReadMode:       {name: "04 read mode", lens: []int{2}},
	CmdSelectBand: {name: "07 band select", hasSub: true, subLens: map[byte][]int{
		BandSelRead: {1}, // reply 07 D2 <00 main | 01 sub>
	}},
	CmdAttenuator: {name: "11 attenuator", hasSub: true, subLens: map[byte][]int{
		AttenuatorOff: {0}, // state rides the sub: 11 00 off / 11 10 on
		Attenuator10:  {0},
	}},
	CmdRFPower: {name: "14 0A RF power", hasSub: true, subLens: map[byte][]int{
		PowerRF: {1},
	}},
	CmdReadMeter: {name: "15 meter", hasSub: true, subLens: map[byte][]int{
		MeterSubS:    {1},
		MeterSubSWR:  {1},
		MeterSubALC:  {1},
		MeterSubCOMP: {1},
	}},
	CmdFunction: {name: "16 function", hasSub: true, subLens: map[byte][]int{
		FuncPreamp:    {1}, // 16 02 <0-3>
		FuncSatellite: {1}, // 16 5A <00/01>
	}},
	CmdTransceiverID: {name: "19 transceiver ID", hasSub: true, subOptional: true,
		subLens: map[byte][]int{
			0x00: {1}, // 19 00 <id>
		},
		lens: []int{1}, // bare legacy form 19 <id>
	},
	CmdSetItem: {name: "1a set item", hasSub: true, subLens: map[byte][]int{
		SetItemDataMode: {2}, // 1A 06 <00/01> <filter>
	}},
	CmdStatus: {name: "1c 00 status (PTT)", hasSub: true, subLens: map[byte][]int{
		StatusSubPTT: {1}, // 1C 00 <00 RX | 01 TX>
	}},
}

// NGError is the typed rejection for an FA frame. The NG reply carries no
// echo of the rejected command, so the pending command's identity is
// attached by the caller at correlation time (WithCommand); the zero value
// still satisfies errors.Is(err, ErrNG).
type NGError struct {
	Cmd    byte
	Sub    byte
	HasSub bool
}

// ErrNG sentinel: NGError unwraps to it, so callers can test
// errors.Is(err, civ.ErrNG) without the type assertion.
func (e *NGError) Error() string {
	s := "command"
	if e != nil {
		s = commandName(e.Cmd)
		if e.HasSub {
			s = fmt.Sprintf("%02x %02x (%s)", e.Cmd, e.Sub, specName(e.Cmd))
		}
	}
	return ErrNG.Error() + ": radio answered NG to " + s
}

// Unwrap makes NGError satisfy errors.Is(err, ErrNG).
func (e *NGError) Unwrap() error { return ErrNG }

// WithCommand attaches the pending command's identity (the FA frame does
// not carry it) and returns the same error for chaining.
func (e *NGError) WithCommand(cmd byte, subs ...byte) *NGError {
	e.Cmd = cmd
	if len(subs) > 0 {
		e.Sub = subs[0]
		e.HasSub = true
	}
	return e
}

// AsNG recovers the typed rejection from an error, ok=false for anything
// else.
func AsNG(err error) (*NGError, bool) {
	var ng *NGError
	if errors.As(err, &ng) {
		return ng, true
	}
	return nil, false
}

func specName(cmd byte) string {
	if s, ok := replySpecs[cmd]; ok {
		return s.name
	}
	return ""
}

// commandName renders a command byte with its table name when known.
func commandName(cmd byte) string {
	if n := specName(cmd); n != "" {
		return fmt.Sprintf("%02x %s", cmd, n)
	}
	return fmt.Sprintf("%02x", cmd)
}

// Parse strictly decodes one datalen-delimited CI-V frame from the radio.
// Any deviation — length, preamble/postamble, addressing, unknown command,
// data area not matching the command's shape — is an error and increments
// Codec.Dropped. An NG frame parses as KindNG with an *NGError (command
// still to be attached by the caller).
func (c *Codec) Parse(frame []byte) (Reply, error) {
	fail := func(err error) (Reply, error) {
		c.dropped.Add(1)
		return Reply{}, err
	}
	if len(frame) < 6 {
		return fail(fmt.Errorf("%w: %d bytes, minimum is 6", ErrFrameMalformed, len(frame)))
	}
	if frame[0] != Preamble || frame[1] != Preamble {
		return fail(fmt.Errorf("%w: missing preamble in % x", ErrFrameMalformed, frame[:min(2, len(frame))]))
	}
	if frame[len(frame)-1] != Postamble {
		return fail(fmt.Errorf("%w: missing postamble", ErrFrameMalformed))
	}
	dst, src, cmd := frame[2], frame[3], frame[4]
	if dst != c.ctrl || src != c.radio {
		return fail(fmt.Errorf("%w: %02x -> %02x", ErrForeignFrame, src, dst))
	}
	body := frame[5 : len(frame)-1] // after cmd, before postamble

	if cmd == ReplyOK {
		if len(body) != 0 {
			return fail(fmt.Errorf("%w: OK frame with a %d-byte data area", ErrFrameMalformed, len(body)))
		}
		return Reply{Kind: KindAck, Cmd: cmd}, nil
	}
	if cmd == ReplyNG {
		if len(body) != 0 {
			return fail(fmt.Errorf("%w: NG frame with a %d-byte data area", ErrFrameMalformed, len(body)))
		}
		return Reply{Kind: KindNG, Cmd: cmd}, &NGError{}
	}

	spec, known := replySpecs[cmd]
	if !known {
		return fail(fmt.Errorf("%w: %02x", ErrUnknownCommand, cmd))
	}

	r := Reply{Kind: KindData, Cmd: cmd}
	switch {
	case spec.hasSub && len(body) >= 1 && (len(body) >= 2 || !spec.subOptional):
		r.Sub = body[0]
		lens, ok := spec.subLens[r.Sub]
		if !ok {
			return fail(fmt.Errorf("%w: %s with unexpected sub %02x", ErrFrameMalformed, spec.name, r.Sub))
		}
		r.Data = body[1:]
		if !acceptLen(lens, len(r.Data)) {
			return fail(fmt.Errorf("%w: %s sub %02x data area %d bytes, accept %v",
				ErrFrameMalformed, spec.name, r.Sub, len(r.Data), lens))
		}
	case spec.hasSub && spec.subOptional:
		// Bare form: the byte after cmd is data, there is no sub.
		r.Data = body
		if !acceptLen(spec.lens, len(r.Data)) {
			return fail(fmt.Errorf("%w: %s data area %d bytes, accept %v",
				ErrFrameMalformed, spec.name, len(r.Data), spec.lens))
		}
	case spec.hasSub:
		return fail(fmt.Errorf("%w: %s without its sub command", ErrFrameMalformed, spec.name))
	default:
		r.Data = body
		if !acceptLen(spec.lens, len(r.Data)) {
			return fail(fmt.Errorf("%w: %s data area %d bytes, accept %v",
				ErrFrameMalformed, spec.name, len(r.Data), spec.lens))
		}
	}
	return r, nil
}

func acceptLen(lens []int, n int) bool {
	for _, l := range lens {
		if l == n {
			return true
		}
	}
	return false
}

// --- payload extractors ---------------------------------------------------------------

// FreqHz decodes a frequency data area (commands 00/03).
func (r Reply) FreqHz() (uint32, error) {
	return decodeBCDFreq(r.Data)
}

// Mode decodes a mode data area (commands 01/04): the canonical mode and
// the filter byte (0 when the radio omitted it). ok=false means the radio
// reported a mode the bus never publishes raw (RTTY/DV/DD, R7) — the
// consumer omits the mode field instead.
func (r Reply) Mode() (mode string, filter byte, ok bool, err error) {
	if len(r.Data) < 1 {
		return "", 0, false, fmt.Errorf("%w: mode data area is empty", ErrFrameMalformed)
	}
	filter = 0
	if len(r.Data) >= 2 {
		filter = r.Data[1]
	}
	mode, ok = modeCanonical(r.Data[0])
	return mode, filter, ok, nil
}

// OnOff decodes a 00/01 data area (16 5A satellite, 1C 00 PTT).
func (r Reply) OnOff() (bool, error) {
	if len(r.Data) != 1 {
		return false, fmt.Errorf("%w: on/off data area is %d bytes", ErrFrameMalformed, len(r.Data))
	}
	switch r.Data[0] {
	case 0x00:
		return false, nil
	case 0x01:
		return true, nil
	default:
		return false, fmt.Errorf("%w: on/off value 0x%02x", ErrFrameMalformed, r.Data[0])
	}
}

// SelectedVFO decodes the band-read data area (07 D2): 00=main, 01=sub.
func (r Reply) SelectedVFO() (VFO, error) {
	if len(r.Data) != 1 {
		return 0, fmt.Errorf("%w: band selection data area is %d bytes", ErrFrameMalformed, len(r.Data))
	}
	switch r.Data[0] {
	case 0x00:
		return VfoMain, nil
	case 0x01:
		return VfoSub, nil
	default:
		return 0, fmt.Errorf("%w: band selection value 0x%02x", ErrFrameMalformed, r.Data[0])
	}
}

// MeterLevel decodes a one-byte meter value (15 02/12/13/14): 0-255, with
// the S-meter reading S0=0 and S9=120.
func (r Reply) MeterLevel() (byte, error) {
	if len(r.Data) != 1 {
		return 0, fmt.Errorf("%w: meter data area is %d bytes", ErrFrameMalformed, len(r.Data))
	}
	return r.Data[0], nil
}

// PowerLevel decodes the RF power data area (14 0A): 0-255.
func (r Reply) PowerLevel() (byte, error) {
	if len(r.Data) != 1 {
		return 0, fmt.Errorf("%w: power data area is %d bytes", ErrFrameMalformed, len(r.Data))
	}
	return r.Data[0], nil
}

// PreampLevel decodes the preamp data area (16 02): the documented 0-3
// value (00 both off, 01 P.AMP, 02 EXT-P.AMP, 03 both).
func (r Reply) PreampLevel() (byte, error) {
	if len(r.Data) != 1 {
		return 0, fmt.Errorf("%w: preamp data area is %d bytes", ErrFrameMalformed, len(r.Data))
	}
	if v := r.Data[0]; v <= 0x03 {
		return v, nil
	}
	return 0, fmt.Errorf("%w: preamp value 0x%02x outside 0-3", ErrFrameMalformed, r.Data[0])
}

// Attenuator decodes the attenuator state from the sub command (11 00 off /
// 11 10 on); the data area is empty by definition.
func (r Reply) Attenuator() (bool, error) {
	if len(r.Data) != 0 {
		return false, fmt.Errorf("%w: attenuator frame with a %d-byte data area", ErrFrameMalformed, len(r.Data))
	}
	switch r.Sub {
	case AttenuatorOff:
		return false, nil
	case Attenuator10:
		return true, nil
	default:
		return false, fmt.Errorf("%w: attenuator sub 0x%02x", ErrFrameMalformed, r.Sub)
	}
}

// RadioID decodes the ID reply (19): the transceiver's CI-V address byte,
// tolerating both the documented (19 00 <id>) and the bare legacy
// (19 <id>) shape.
func (r Reply) RadioID() (byte, error) {
	if len(r.Data) != 1 {
		return 0, fmt.Errorf("%w: ID data area is %d bytes", ErrFrameMalformed, len(r.Data))
	}
	return r.Data[0], nil
}

// DataMode decodes the data-mode reply (1A 06 <q> <w>): on/off and the
// filter (0 when off — the radio stores 00 there per the official table).
func (r Reply) DataMode() (on bool, filter byte, err error) {
	if len(r.Data) != 2 {
		return false, 0, fmt.Errorf("%w: data-mode data area is %d bytes", ErrFrameMalformed, len(r.Data))
	}
	switch r.Data[0] {
	case 0x00:
		return false, r.Data[1], nil
	case 0x01:
		return true, r.Data[1], nil
	default:
		return false, 0, fmt.Errorf("%w: data-mode value 0x%02x", ErrFrameMalformed, r.Data[0])
	}
}
