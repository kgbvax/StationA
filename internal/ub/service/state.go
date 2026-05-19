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
}
