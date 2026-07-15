// Package rotor talks to the AF6SA WRC (Web Remote Control) controller that
// steers the HF rotator. The controller exposes a WebSocket at a configured URL
// (default ws://192.168.1.108/wsrotor). It streams rotor status as JSON and
// accepts azimuth commands as JSON. This package owns the wire structs, the
// read loop, and the command surface; the bridge (internal/bridge) translates
// between this and the canonical station-model state.
package rotor

import "strings"

// RotorStatus is the JSON status document streamed by the WRC controller.
type RotorStatus struct {
	State string  `json:"state"`          // e.g. "stopped", "rotating"
	Name  string  `json:"name,omitempty"` // rotor name label
	Az    float64 `json:"az"`             // current azimuth, degrees
	Lim1  float64 `json:"lim1,omitempty"` // CCW limit
	Lim2  float64 `json:"lim2,omitempty"` // CW limit
	TDeg  float64 `json:"tdeg,omitempty"` // target (target degrees)
	FMsg  string  `json:"fmsg,omitempty"` // fault message
}

// RotorCommand is the JSON command document sent to the WRC. Az is either a
// number (rotate to that azimuth) or one of the jog strings "stop", "fwd",
// "rev".
type RotorCommand struct {
	Az any `json:"az"`
}

// IsMoving reports whether a WRC state string indicates the rotator is turning.
// The controller has been observed to emit "rotating" while moving and
// "stopped"/"idle" at rest; treat any state mentioning "rotat" or "moving" as
// moving so a renamed-but-similar firmware string still maps correctly.
func IsMoving(state string) bool {
	s := strings.ToLower(state)
	if s == "" {
		return false
	}
	return strings.Contains(s, "rotat") || strings.Contains(s, "moving")
}

// State is the canonical rotator state the bridge publishes (integration model
// §7.1, slot muehle/hf/rotator). Azimuth-only: capabilities axes [az].
type State struct {
	Az           float64 `json:"az"`                    // current azimuth, degrees
	TargetAz     float64 `json:"target_az,omitempty"`   // commanded target azimuth (tdeg)
	Moving       bool    `json:"moving"`                // rotator is currently turning
	RotorState   string  `json:"rotor_state,omitempty"` // raw WRC state string (diagnostic)
	DeviceOnline bool    `json:"device_online"`         // the WRC is reachable while the bridge is up
	Error        string  `json:"error,omitempty"`       // human-readable WRC fault
}

// FromStatus canonicalizes a WRC RotorStatus into a State. online is set by the
// caller (the read loop marks the device online while the WebSocket is up).
func FromStatus(s RotorStatus, online bool) State {
	st := State{
		Az:           s.Az,
		Moving:       IsMoving(s.State),
		RotorState:   s.State,
		DeviceOnline: online,
		Error:        s.FMsg,
	}
	if s.TDeg != 0 {
		st.TargetAz = s.TDeg
	}
	return st
}
