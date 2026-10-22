package service

import "time"

type State struct {
	FrequencyKHz uint16    `json:"frequency_khz"`
	BandName     string    `json:"band_name"`
	BandIndex    byte      `json:"band_index"`
	ModeName     string    `json:"mode_name"`
	MotorsMoving bool      `json:"motors_moving"`
	MotorBits    byte      `json:"motor_bits"`
	UpdatedAt    time.Time `json:"updated_at"`
	Offline      bool      `json:"offline"`
	LastError    string    `json:"last_error,omitempty"`
	// Retracted latches true when a retract command succeeds and clears when a
	// change-frequency command (new frequency or direction) is accepted. The RCU-06
	// status reply carries no retract flag, so this is bridge-side knowledge: it is
	// lost on a bridge restart (re-established by EnsureRetract while an
	// antenna-mode lock with retract is active). The PA arm (PLC #1) blocks on it.
	Retracted bool `json:"retracted"`
	// Lock is the antenna-mode lock label: "" when unlocked, else the mode
	// name (storm|maintenance). Filled by Controller.State from the hf/ant-mode
	// state, not by the device; while set, extends are refused.
	Lock string `json:"lock,omitempty"`
}
