// Package radio holds the IC-9700's on-demand session lifecycle: the
// Session state machine (idle/connecting/live/error) that decides when the
// bridge's single radio connection exists, behind the Manager seam the
// process wiring drives.
package radio

import (
	"context"
)

// Manager is the radio-side surface the bridge main loop drives. The radio's
// single LAN session is on-demand (KTD-2): Connect is a deliberate no-op —
// login attempts happen only cmd-driven, arm-driven or safety-driven (R2) —
// while Execute/Poll/Disconnect operate the session the bus demands.
type Manager interface {
	// Connect opens the control session to the configured radio host. It
	// blocks until the handshake completes or fails.
	Connect(ctx context.Context) error
	// Disconnect closes the live session, if any. Safe with no session.
	Disconnect()
	// Execute applies one /cmd payload (stationa value-key convention) to
	// the radio; it fails when no live session is held.
	Execute(ctx context.Context, payload []byte) error
	// Poll refreshes the radio-measured state for the bridge's /state
	// assembly while a session is live.
	Poll(ctx context.Context) error
}
