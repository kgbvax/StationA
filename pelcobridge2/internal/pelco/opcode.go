package pelco

// Opcodes are the cmd2 byte values this head documents. Named constants only;
// no magic bytes outside this file.
const (
	OpStop       = 0x00 // stop all movement
	OpRight      = 0x02 // jog right (pan speed in d1)
	OpLeft       = 0x04 // jog left  (pan speed in d1)
	OpUp         = 0x08 // jog up    (tilt speed in d2)
	OpDown       = 0x10 // jog down  (tilt speed in d2)
	OpSetPan     = 0x4B // absolute set pan — works only on a quiet line
	OpSetTilt    = 0x4D // absolute set tilt — same quiet-line requirement
	OpQueryPan   = 0x51 // pan angle query
	OpQueryTilt  = 0x53 // tilt angle query
	OpRspPan     = 0x59 // pan response, degrees×100
	OpRspTilt    = 0x5B // tilt response, degrees×100 (bench-confirmed 2026-08-28)
	OpPresetSet  = 0x03 // "set preset N" / extended selector set
	OpPresetCall = 0x07 // "call preset N" / extended selector call
)

// PresetSelfCheckOff is the "disable self-check" selector: preset SET 105
// disables the head's periodic self-check, preset CALL 105 re-enables it. The
// engine sends the disable once per connect (a periodic self-check re-homes
// the head unprompted); the call is reachable only from the TUI, as a manual
// maintenance toggle.
const PresetSelfCheckOff = 0x69

// Preset selectors ride in d2 of a preset set/call frame.
const (
	PresetSelfTest = 0x7D // preset call 125: factory defaults + self-test (re-homes the head — DANGEROUS)
)

// MaxSpeed is the top of the documented jog-speed range (0x00..0x3F).
const MaxSpeed = 0x3F

// DefaultJogSpeed is the TUI's initial low jog speed (user spec: "12").
const DefaultJogSpeed = 0x12

// ClampSpeed constrains a jog speed byte to the documented range.
func ClampSpeed(b byte) byte { return b & MaxSpeed }

// IsPanOp reports whether op acts on the pan axis — pan jogs (0x02/0x04),
// pan set (0x4B), pan query (0x51), pan response (0x59). The speed byte rides
// in d1 for pan ops and d2 for tilt ops.
func IsPanOp(op byte) bool {
	switch op {
	case OpRight, OpLeft, OpSetPan, OpQueryPan, OpRspPan:
		return true
	}
	return false
}

// JogFrame builds a one-axis jog frame at the given speed byte.
func JogFrame(addr, op, speed byte) Frame {
	speed = ClampSpeed(speed)
	d1, d2 := byte(0), byte(0)
	if IsPanOp(op) {
		d1 = speed
	} else {
		d2 = speed
	}
	return Build(addr, 0x00, op, d1, d2)
}

// StopFrame builds the all-stop frame. It is the safety path: valid disarmed,
// valid mid-motion, valid while anything else is gated.
func StopFrame(addr byte) Frame { return Build(addr, 0x00, OpStop, 0x00, 0x00) }

// QueryFrame builds a pan (0x51) or tilt (0x53) angle query.
func QueryFrame(addr, op byte) Frame { return Build(addr, 0x00, op, 0x00, 0x00) }

// SetPanFrame builds an absolute pan set in degrees. The angle wraps into
// [0,360): the head is circular, and the caller's offset math has already
// decided where the head should physically point.
func SetPanFrame(addr byte, deg float64) Frame {
	w, err := DegToWord(Norm360(deg))
	if err != nil {
		// Norm360 guarantees [0,360) so DegToWord cannot fail; this branch only
		// exists to keep SetPanFrame total for callers that pass NaN — in that
		// case park at zero rather than emit a garbage word.
		return Build(addr, 0x00, OpSetPan, 0x00, 0x00)
	}
	return Build(addr, 0x00, OpSetPan, byte(w>>8), byte(w))
}

// SetTiltFrame builds an absolute tilt set in degrees. Tilt travel is 0..90°;
// out-of-range values are clamped — a tilt set is a physical-position command
// and overshooting the head's travel is the dangerous direction.
func SetTiltFrame(addr byte, deg float64) Frame {
	if deg < 0 {
		deg = 0
	}
	if deg > 90 {
		deg = 90
	}
	w, _ := DegToWord(deg)
	return Build(addr, 0x00, OpSetTilt, byte(w>>8), byte(w))
}

// PresetCallFrame sends "call preset N" (extended selector call).
func PresetCallFrame(addr, n byte) Frame { return Build(addr, 0x00, OpPresetCall, 0x00, n) }

// PresetSetFrame sends "set preset N" (extended selector set).
func PresetSetFrame(addr, n byte) Frame { return Build(addr, 0x00, OpPresetSet, 0x00, n) }

// SelfTestFrame builds the dangerous self-test frame (preset call 125).
// A self-test restores factory defaults and re-homes the head: the head swings
// to its mechanical stops and whatever zero the readback references moves.
// The engine refuses it while armed; the TUI double-confirms it.
func SelfTestFrame(addr byte) Frame { return PresetCallFrame(addr, PresetSelfTest) }

// SelfCheckDisableFrame sends "set preset 105", disabling the head's periodic
// self-check. That self-check re-homes the head unprompted — unacceptable for
// an antenna rotor mid-contact — so the engine sends this once per connect.
func SelfCheckDisableFrame(addr byte) Frame { return PresetSetFrame(addr, PresetSelfCheckOff) }

// SelfCheckEnableFrame sends "call preset 105", re-enabling the head's periodic
// self-check. MAINTENANCE ONLY: while enabled, the head re-homes itself
// unprompted. TUI-only, disarmed-only, behind a y/n confirm.
func SelfCheckEnableFrame(addr byte) Frame { return PresetCallFrame(addr, PresetSelfCheckOff) }
