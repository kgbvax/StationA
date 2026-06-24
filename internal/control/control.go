// Package control implements the optional inbound network-control servers that
// let external tracking software command the rotator. Two text protocols are
// supported — Hamlib rotctld and Yaesu GS-232 — both translated to a common
// Command that the engine executes. Position queries are answered from a
// thread-safe Pos snapshot the engine publishes.
//
// These servers move the rotator on remote request: they are the explicit,
// opt-in counterpart to the TUI's local hold-to-move controls.
package control

import "sync"

// Kind enumerates the rotator actions a network client can request.
type Kind int

const (
	// KindStop halts all motion.
	KindStop Kind = iota
	// KindSetPos commands an absolute move to (Az, El) degrees.
	KindSetPos
	// KindJog requests continuous motion in the Pan/Tilt direction
	// (each -1, 0, or +1). Used by GS-232 R/L/U/D.
	KindJog
)

// Command is a decoded, protocol-independent rotator action.
type Command struct {
	Kind   Kind
	Az, El float64 // KindSetPos: target degrees (azimuth/elevation)
	Pan    int     // KindJog: -1 CCW/left, +1 CW/right
	Tilt   int     // KindJog: -1 down, +1 up
}

// Submit hands a decoded Command to the engine for execution.
type Submit func(Command)

// Pos is a thread-safe holder for the latest known rotator position, written by
// the engine and read by the servers to answer queries.
type Pos struct {
	mu     sync.Mutex
	az, el float64
	valid  bool
}

// Set records the latest position (degrees).
func (p *Pos) Set(az, el float64) {
	p.mu.Lock()
	p.az, p.el, p.valid = az, el, true
	p.mu.Unlock()
}

// Get returns the latest position and whether any readback has arrived yet.
func (p *Pos) Get() (az, el float64, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.az, p.el, p.valid
}
