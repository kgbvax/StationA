package pelco

import "math"

// Extended command opcodes (carried in CMD2 with CMD1 = 0x00).
const (
	cmdSetPan    = 0x4B // set pan (absolute) position
	cmdSetTilt   = 0x4D // set tilt (absolute) position
	cmdQueryPan  = 0x51 // query pan position
	cmdRespPan   = 0x59 // pan position response
	cmdQueryTlt  = 0x53 // query tilt position
	cmdRespTilt  = 0x5B // tilt position response
	cmdSetPreset = 0x03 // set / store preset N (preset number in DATA2)
	cmdClrPreset = 0x05 // clear preset N (preset number in DATA2)
	cmdGoPreset  = 0x07 // go to / call preset N (preset number in DATA2)
)

// Standard command (CMD2) direction bits, combinable for diagonal motion.
const (
	dirRight = 0x02
	dirLeft  = 0x04
	dirUp    = 0x08
	dirDown  = 0x10
)

// MaxSpeed is the maximum normal pan/tilt speed value used for jog commands.
const MaxSpeed = 0x3F

// TurboSpeed is the special pan-speed value that requests the unit's maximum
// "turbo" slew rate (Pelco-D reserves 0xFF for this).
const TurboSpeed = 0xFF

// clampSpeed bounds a jog speed to the valid range: 0x00–0x3F, or the special
// 0xFF turbo value passed through unchanged.
func clampSpeed(s int) byte {
	switch {
	case s >= TurboSpeed:
		return TurboSpeed
	case s < 0:
		return 0
	case s > MaxSpeed:
		return MaxSpeed
	}
	return byte(s)
}

// panHundredths converts a pan angle in degrees to hundredths, wrapping into
// the 0–359.99° range (0–35999). The modulo is taken on the float first so that
// an un-clamped inbound azimuth (e.g. a huge rotctld set_pos value) cannot
// overflow the int conversion before the wrap — which would yield a garbage
// angle and send the unit to a random azimuth.
func panHundredths(deg float64) uint16 {
	h := int(math.Round(math.Mod(deg*100, 36000)))
	if h < 0 {
		h += 36000
	}
	return uint16(h)
}

// tiltHundredths converts a tilt/elevation angle in degrees to hundredths,
// clamped to the unit's 0–90° travel.
func tiltHundredths(deg float64) uint16 {
	if deg < 0 {
		deg = 0
	}
	if deg > 90 {
		deg = 90
	}
	return uint16(math.Round(deg * 100))
}

// HundredthsToDeg converts a hundredths-of-a-degree word to degrees.
func HundredthsToDeg(h uint16) float64 { return float64(h) / 100 }

func extended(addr, cmd2 byte, word uint16) Frame {
	return Frame{Addr: addr, Cmd1: 0x00, Cmd2: cmd2, Data1: byte(word >> 8), Data2: byte(word)}
}

// SetPan builds an absolute pan-position command for the given degrees.
func SetPan(addr byte, deg float64) Frame {
	return extended(addr, cmdSetPan, panHundredths(deg))
}

// SetTilt builds an absolute tilt-position command for the given degrees.
func SetTilt(addr byte, deg float64) Frame {
	return extended(addr, cmdSetTilt, tiltHundredths(deg))
}

// QueryPan builds a pan-position query. The unit replies with a IsPanResponse frame.
func QueryPan(addr byte) Frame { return extended(addr, cmdQueryPan, 0) }

// QueryTilt builds a tilt-position query. The unit replies with a IsTiltResponse frame.
func QueryTilt(addr byte) Frame { return extended(addr, cmdQueryTlt, 0) }

// PanResponse builds a pan-position query response carrying the given degrees
// (wrapped to 0–359.99° by panHundredths). Used by the in-memory simulator.
func PanResponse(addr byte, deg float64) Frame { return extended(addr, cmdRespPan, panHundredths(deg)) }

// TiltResponse builds a tilt-position query response carrying the given degrees
// (clamped to 0–90° by tiltHundredths). Used by the in-memory simulator.
func TiltResponse(addr byte, deg float64) Frame {
	return extended(addr, cmdRespTilt, tiltHundredths(deg))
}

// Preset commands. Standard Pelco-D carries the preset number in DATA2 with
// DATA1 == 0 (unlike the position commands, which use the big-endian DATA1/DATA2
// word), so these have dedicated builders rather than reusing extended().

// SetPreset stores the current position as preset n (1–255). CMD2 = 0x03.
func SetPreset(addr byte, n byte) Frame {
	return Frame{Addr: addr, Cmd1: 0x00, Cmd2: cmdSetPreset, Data1: 0x00, Data2: n}
}

// GoToPreset moves the unit to preset n. CMD2 = 0x07.
func GoToPreset(addr byte, n byte) Frame {
	return Frame{Addr: addr, Cmd1: 0x00, Cmd2: cmdGoPreset, Data1: 0x00, Data2: n}
}

// ClearPreset clears preset n. CMD2 = 0x05.
func ClearPreset(addr byte, n byte) Frame {
	return Frame{Addr: addr, Cmd1: 0x00, Cmd2: cmdClrPreset, Data1: 0x00, Data2: n}
}

// Preset numbers with reserved meanings on the 303Z/3050DZ PTZ (from the
// serial-command manual). These are device conventions layered on top of the
// standard Pelco-D preset mechanism, not part of the base protocol.
const (
	// SelfCheckPreset gates the PTZ self-check: SetPreset(105) disables it,
	// GoToPreset(105) re-enables it. Persistent on the unit.
	SelfCheckPreset byte = 105
	// FactoryResetPreset: GoToPreset(125) restores default parameters AND starts
	// the self-test (so re-enabling self-check is a side effect of a reset).
	FactoryResetPreset byte = 125
)

// DisableSelfCheck builds the command that turns off the PTZ self-check
// function (set preset 105 on the 303Z/3050DZ). The setting persists on the
// unit until re-enabled (go-to preset 105) or cleared by a factory reset
// (go-to preset 125). Wire: FF <addr> 00 03 00 69 <chk>.
func DisableSelfCheck(addr byte) Frame { return SetPreset(addr, SelfCheckPreset) }

// EnableSelfCheck builds the command that re-enables the PTZ self-check
// function (go-to preset 105).
func EnableSelfCheck(addr byte) Frame { return GoToPreset(addr, SelfCheckPreset) }

// IsQueryPan reports whether f is a pan-position query.
func (f Frame) IsQueryPan() bool { return f.Cmd1 == 0x00 && f.Cmd2 == cmdQueryPan }

// IsQueryTilt reports whether f is a tilt-position query.
func (f Frame) IsQueryTilt() bool { return f.Cmd1 == 0x00 && f.Cmd2 == cmdQueryTlt }

// IsSetPan reports whether f is an absolute pan-position set command.
func (f Frame) IsSetPan() bool { return f.Cmd1 == 0x00 && f.Cmd2 == cmdSetPan }

// IsSetTilt reports whether f is an absolute tilt-position set command.
func (f Frame) IsSetTilt() bool { return f.Cmd1 == 0x00 && f.Cmd2 == cmdSetTilt }

// IsSetPreset reports whether f is a preset-set command (CMD2 == 0x03).
func (f Frame) IsSetPreset() bool { return f.Cmd1 == 0x00 && f.Cmd2 == cmdSetPreset }

// IsGoPreset reports whether f is a preset-call / go-to-preset command (CMD2 == 0x07).
func (f Frame) IsGoPreset() bool { return f.Cmd1 == 0x00 && f.Cmd2 == cmdGoPreset }

// IsClearPreset reports whether f is a preset-clear command (CMD2 == 0x05).
func (f Frame) IsClearPreset() bool { return f.Cmd1 == 0x00 && f.Cmd2 == cmdClrPreset }

// IsJog reports whether f is a continuous-motion jog command — an extended
// command (CMD1 == 0x00) whose CMD2 carries *only* the direction bits
// (0x02/0x04/0x08/0x10) and nothing else.
//
// The "no bits outside the motion mask" test is what makes this correct: the
// Pelco-D preset opcodes (0x03 set, 0x05 clear, 0x07 go) and the aux opcodes
// (0x09 on, 0x0B off) all share the low direction bits — 0x03 = 0x02|0x01,
// 0x07 = 0x06|0x01, 0x09 = 0x08|0x01, 0x0B = 0x0A|0x01 — so a naive
// "any direction bit set" test misclassifies them as jogs. Each also sets bit
// 0x01 (outside the motion mask 0x1E), which excludes them. The high extended
// position opcodes (0x4B/0x4D/0x51/0x53/0x59/0x5B) set bits in 0x20/0x40 and are
// excluded the same way. A Stop frame (CMD2 == 0) is not a jog.
func (f Frame) IsJog() bool {
	const motion = dirRight | dirLeft | dirUp | dirDown // 0x1E
	return f.Cmd1 == 0x00 && f.Cmd2&motion != 0 && f.Cmd2&^motion == 0
}

// JogDir decodes a jog command's pan/tilt direction as -1, 0, or +1 each. Pan
// is +1 right / -1 left; tilt is +1 up / -1 down. Call only when IsJog is true.
func (f Frame) JogDir() (pan, tilt int) {
	switch {
	case f.Cmd2&dirRight != 0:
		pan = 1
	case f.Cmd2&dirLeft != 0:
		pan = -1
	}
	switch {
	case f.Cmd2&dirUp != 0:
		tilt = 1
	case f.Cmd2&dirDown != 0:
		tilt = -1
	}
	return
}

// Jog builds a continuous-motion command. panDir/tiltDir are direction bits
// (use the Dir* helpers via the exported Pan/Tilt direction constants below);
// speeds are clamped to 0x3F.
func Jog(addr byte, cmd2 byte, panSpeed, tiltSpeed int) Frame {
	return Frame{Addr: addr, Cmd1: 0x00, Cmd2: cmd2, Data1: clampSpeed(panSpeed), Data2: clampSpeed(tiltSpeed)}
}

// Stop builds an all-stop command.
func Stop(addr byte) Frame { return Frame{Addr: addr} }

// Direction describes a jog direction as a combination of pan and tilt motion.
type Direction struct {
	Pan  int // -1 left, 0 none, +1 right
	Tilt int // -1 down, 0 none, +1 up
}

// Cmd2 returns the Pelco-D command byte encoding this direction.
func (d Direction) Cmd2() byte {
	var c byte
	switch {
	case d.Pan > 0:
		c |= dirRight
	case d.Pan < 0:
		c |= dirLeft
	}
	switch {
	case d.Tilt > 0:
		c |= dirUp
	case d.Tilt < 0:
		c |= dirDown
	}
	return c
}

// IsPanResponse reports whether f is a pan-position query response.
func (f Frame) IsPanResponse() bool { return f.Cmd1 == 0x00 && f.Cmd2 == cmdRespPan }

// IsTiltResponse reports whether f is a tilt-position query response.
func (f Frame) IsTiltResponse() bool { return f.Cmd1 == 0x00 && f.Cmd2 == cmdRespTilt }
