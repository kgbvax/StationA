// SPDX-License-Identifier: AGPL-3.0-or-later

// Package antmode is the shared wire contract of the `muehle/hf/ant-mode` logic
// slot (component `antmode`): the subset of its retained /state that the
// enforcing bridges (wrc-rotator-bridge, ultrabridge) and the standing-down
// consumers (antennaselect, beamsteer, powerseq) read. It is a pure parser —
// no policy, no MQTT — so the five consumers cannot drift apart on how a lock
// is read (the same reason shared/pstrotator holds the PstRotator grammar).
//
// Reading rules (normative, see antmode/docs/ant-mode-mqtt-api.md):
//
//   - Empty payload (retained topic cleared / never published) → normal.
//     Rollout-safe: the station is usable before antmode exists.
//   - A valid snapshot with ant_mode ≠ normal is enforced even while the
//     ant-mode slot's /status is offline — a dead owner never unlocks.
//   - A malformed payload is an error; the caller keeps its last known
//     state (and logs an Error), it never falls back to normal.
//
// The lock is built up in safe order by antmode: ground first, then retract
// once the switch reports off, then park_az once the elements are in. Bridges
// act on each field as it appears. Seq changes on every mode entry and every
// explicit re-park; bridges compare it for equality only.
package antmode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Mode is the station antenna mode.
type Mode string

const (
	Normal      Mode = "normal"
	Storm       Mode = "storm"
	Maintenance Mode = "maintenance"
)

// Modes lists the valid modes in display order (the /meta enum options).
var Modes = []Mode{Normal, Storm, Maintenance}

// NudgeOverride is the `override` value an operator console puts on a rotator
// set_az to request a maintenance nudge. It is intent tagging (keeps
// automation out), not authentication.
const NudgeOverride = "maintenance"

// Valid reports whether m is one of the defined modes.
func (m Mode) Valid() bool {
	for _, v := range Modes {
		if m == v {
			return true
		}
	}
	return false
}

// Lock is what an enforcing bridge must do while the mode is not normal.
type Lock struct {
	// Seq identifies one park request; a new value means "park again".
	Seq uint64 `json:"seq"`
	// Ground: antennaselect holds the antenna switch at off.
	Ground bool `json:"ground"`
	// Retract: ultrabridge retracts the elements and keeps them in.
	Retract bool `json:"retract"`
	// ParkAz: wrc-rotator-bridge drives here once (nil = not yet).
	ParkAz *int `json:"park_az,omitempty"`
	// Nudge: an explicit console set_az tagged NudgeOverride is allowed.
	Nudge bool `json:"nudge"`
}

// State is the consumer-facing subset of hf/ant-mode/state.
type State struct {
	Mode Mode `json:"ant_mode"`
	// Lock is present whenever Mode ≠ normal (a missing lock on a locked mode
	// parses as a zero Lock: refuse motion, no actions yet).
	Lock *Lock `json:"lock,omitempty"`
	// PowerHold: antmode is borrowing antenna power to park; powerseq must
	// treat watch changes as a re-baseline, not activity.
	PowerHold bool `json:"power_hold,omitempty"`
}

// Locked reports whether motion must be refused.
func (s State) Locked() bool { return s.Mode != "" && s.Mode != Normal }

// L returns the lock, never nil (zero Lock when unlocked or not yet built).
func (s State) L() Lock {
	if s.Lock == nil {
		return Lock{}
	}
	return *s.Lock
}

// NormalState is the unlocked state.
var NormalState = State{Mode: Normal}

// ErrMalformed wraps every Parse failure.
var ErrMalformed = errors.New("antmode: malformed state")

// Parse decodes a hf/ant-mode/state payload per the reading rules above.
func Parse(payload []byte) (State, error) {
	if len(strings.TrimSpace(string(payload))) == 0 {
		return NormalState, nil
	}
	var s State
	if err := json.Unmarshal(payload, &s); err != nil {
		return State{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if !s.Mode.Valid() {
		return State{}, fmt.Errorf("%w: ant_mode %q", ErrMalformed, s.Mode)
	}
	if s.Mode == Normal {
		s.Lock = nil
	} else if s.Lock == nil {
		s.Lock = &Lock{}
	}
	if s.Lock != nil && s.Lock.ParkAz != nil && (*s.Lock.ParkAz < 0 || *s.Lock.ParkAz > 450) {
		return State{}, fmt.Errorf("%w: park_az %d out of range", ErrMalformed, *s.Lock.ParkAz)
	}
	return s, nil
}

// Label is the short human/state label of a lock: "" when unlocked, else the
// mode name. Bridges publish it as their /state `lock` field.
func (s State) Label() string {
	if !s.Locked() {
		return ""
	}
	return string(s.Mode)
}
