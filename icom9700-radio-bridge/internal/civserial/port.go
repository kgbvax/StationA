// Package civserial is the read-only serial CI-V telemetry monitor
// (2026-09 pivot): freq/mode arrive as unsolicited transceive broadcasts,
// meters are polled at a fixed cadence, and every change is surfaced as an
// update event — the bridge broadcasts it on MQTT on change. The serial
// wire is the ONLY CI-V command path left (receive-only posture: no LAN
// CI-V other than the audio session carrier); the reader never sends a
// state-changing frame — reads, transceive-enable and the power-on wake
// excepted.
//
// Concurrency: one reader goroutine owns the port while the monitor is on;
// Wake transient-opens the port when the monitor is off. Callers interact
// via SetMonitor/Wake/Snapshot/Updates.
package civserial

import (
	"errors"

	"go.bug.st/serial"
)

// Port is the serial byte transport. Production adapts go.bug.st/serial;
// tests substitute an in-process fake (go.bug.st ships no fake).
type Port interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
}

// OpenFunc opens the configured serial port. Invoked by the reader loop
// (and Wake) on every (re)connect — the radio power-cycling re-enumerates
// the tty device, so the port is always re-opened fresh.
type OpenFunc func() (Port, error)

// ErrNoDevice is returned by SetMonitor/Wake when no serial device is
// configured (serial.device empty): the observed fact, not a guess.
var ErrNoDevice = errors.New("civserial: no serial device configured")

// SerialOpen adapts go.bug.st/serial into an OpenFunc. The device must be
// a stable /dev/serial/by-id/ path — ttyACM numbers move on USB
// re-enumeration.
func SerialOpen(device string, baud int) OpenFunc {
	return func() (Port, error) {
		return serial.Open(device, &serial.Mode{
			BaudRate: baud,
			DataBits: 8,
			Parity:   serial.NoParity,
			StopBits: serial.OneStopBit,
		})
	}
}
