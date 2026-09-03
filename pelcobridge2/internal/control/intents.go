package control

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
	// only. Motion runs until a Stop (TUI hold-to-move supplies it). DirUp/
	// DirDown mean ELEVATION up/down; the engine translates them to the
	// head's native jog opcodes, whose tilt scale is inverted.
	JogIntent struct{ Dir Dir }

	// StopIntent is the all-stop. Always allowed, from every source, even
	// disarmed — it also cancels any in-flight set ladder.
	StopIntent struct{}

	// SetPanIntent / SetTiltIntent drive the verification ladder toward a
	// TRUE target (pan: offset applied to a physical target internally;
	// tilt: elevation mirrored to the head's inverted native tilt).
	// Armed only.
	SetPanIntent  struct{ Deg float64 }
	SetTiltIntent struct{ Deg float64 }

	// GotoPhysZeroIntent drives both axes to PHYSICAL zero (the head's
	// mechanical home). The TUI may use it disarmed, like jog; the offset
	// is never applied.
	GotoPhysZeroIntent struct{}

	// GotoAzElIntent drives one or both axes to a TRUE az/el target in a
	// single ladder (pan crosses the arm offset, el mirrors to the native
	// tilt). Manual-positioning class like jog and goto-0: the TUI may use
	// it disarmed (offset is 0 then, so the target is physical); any other
	// source needs the arm gate. HasAz/HasEl pick the axes — a NaN-free way
	// to say "only one axis".
	GotoAzElIntent struct {
		Az, El       float64
		HasAz, HasEl bool
	}

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
func (GotoAzElIntent) intent()     {}
func (ArmIntent) intent()          {}
func (DisarmIntent) intent()       {}
func (SelfTestIntent) intent()     {}
func (SelfCheckIntent) intent()    {}
func (JogSpeedIntent) intent()     {}
func (ReopenIntent) intent()       {}

// Dir is a jog direction; the engine maps it onto the Pelco-D jog opcodes
// (dirOpcode in engine.go — the single mapping, since the tilt pair swaps).
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
