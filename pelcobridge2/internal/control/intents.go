package control

import "pelcobridge2/internal/pelco"

// Intent is one thing the engine can be asked to do. Concrete types keep the
// source gating readable: the engine switches on type, then on Request.From.
type Intent interface{ intent() }

type (
	// QueryPanIntent / QueryTiltIntent ask the head for one position readback.
	// Always allowed (arming needs them); subject to the one-outstanding-query
	// rule and refused while moving.
	QueryPanIntent  struct{}
	QueryTiltIntent struct{}

	// JogIntent starts motion on one axis at the engine's jog speed. Armed
	// only. Motion runs until a Stop (TUI hold-to-move supplies it).
	JogIntent struct{ Dir Dir }

	// StopIntent is the all-stop. Always allowed, from every source, even
	// disarmed — it also cancels any in-flight set ladder.
	StopIntent struct{}

	// SetPanIntent / SetTiltIntent drive the verification ladder toward a
	// TRUE target (the offset is applied to a physical target internally).
	// Armed only.
	SetPanIntent  struct{ Deg float64 }
	SetTiltIntent struct{ Deg float64 }

	// GotoPhysZeroIntent drives both axes to PHYSICAL zero (the head's
	// mechanical home). Armed only; the offset is never applied.
	GotoPhysZeroIntent struct{}

	// ArmIntent arms the rotator for rotctl use. TUI-only, enforced twice
	// (source check here, and no code path from MQTT/rotctld builds it).
	// Requires a fresh pan readback and a stationary head.
	ArmIntent struct{ TrueAz float64 }

	// DisarmIntent drops the armed state. TUI-only.
	DisarmIntent struct{}

	// SelfTestIntent fires the preset-call-125 self-test (re-homes the head,
	// can rip cables). TUI-only and refused while armed.
	SelfTestIntent struct{}

	// SelfCheckIntent toggles the head's periodic self-check (preset
	// set/call 105). While enabled the head re-homes itself UNPROMPTED —
	// maintenance only. TUI-only; enabling is additionally disarmed-only.
	SelfCheckIntent struct{ Enable bool }

	// JogSpeedIntent sets the jog speed byte (0x00–0x3F). TUI-only.
	JogSpeedIntent struct{ Speed byte }

	// ReopenIntent re-opens the serial transport (USB re-enumeration heal).
	ReopenIntent struct{}
)

func (QueryPanIntent) intent()     {}
func (QueryTiltIntent) intent()    {}
func (JogIntent) intent()          {}
func (StopIntent) intent()         {}
func (SetPanIntent) intent()       {}
func (SetTiltIntent) intent()      {}
func (GotoPhysZeroIntent) intent() {}
func (ArmIntent) intent()          {}
func (DisarmIntent) intent()       {}
func (SelfTestIntent) intent()     {}
func (SelfCheckIntent) intent()    {}
func (JogSpeedIntent) intent()     {}
func (ReopenIntent) intent()       {}

// Dir is a jog direction; it maps onto the Pelco-D jog opcodes.
type Dir int

const (
	DirUp Dir = iota
	DirDown
	DirLeft
	DirRight
)

func (d Dir) String() string {
	switch d {
	case DirUp:
		return "up"
	case DirDown:
		return "down"
	case DirLeft:
		return "left"
	case DirRight:
		return "right"
	}
	return "?"
}

func (d Dir) opcode() byte {
	switch d {
	case DirUp:
		return pelco.OpUp
	case DirDown:
		return pelco.OpDown
	case DirLeft:
		return pelco.OpLeft
	case DirRight:
		return pelco.OpRight
	}
	return pelco.OpStop
}
