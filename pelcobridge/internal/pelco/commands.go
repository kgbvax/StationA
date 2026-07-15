package pelco

import "math"

// Extended command opcodes (carried in CMD2 with CMD1 = 0x00).
const (
	cmdSetPan   = 0x4B // set pan (absolute) position
	cmdSetTilt  = 0x4D // set tilt (absolute) position
	cmdQueryPan = 0x51 // query pan position
	cmdRespPan  = 0x59 // pan position response
	cmdQueryTlt = 0x53 // query tilt position
	cmdRespTilt = 0x5B // tilt position response
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
// the 0–359.99° range (0–35999).
func panHundredths(deg float64) uint16 {
	h := int(math.Round(deg * 100))
	h %= 36000
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
