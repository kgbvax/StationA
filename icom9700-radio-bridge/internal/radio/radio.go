// Package radio is the U1 skeleton of the IC-9700 control plane: the Manager
// seam main.go's wiring compiles against. U2 (RS-BA1 UDP transport), U3
// (CI-V codec), U4 (session manager) and U5 (bus surface) replace the stubs —
// the interface is the contract those units fill, not a placeholder to grow.
//
// The stubs are obviously marked and minimal on purpose: the U1 scaffold must
// not pretend to speak protocol.
package radio

import (
	"context"
	"errors"
	"log/slog"
)

// ErrNotImplemented marks every U1 stub body. The bridge's radio loop treats
// it like any other connect/session failure — log, back off, retry — so the
// scaffold's behavior (MQTT online, radio side politely idle) is exactly what
// the wired units inherit.
var ErrNotImplemented = errors.New("radio session stack not implemented (U1 scaffold; U2-U5 land transport/codec/session)")

// Manager is the radio-side surface the bridge main loop drives. The radio's
// single LAN session is on-demand (KTD-2): Connect performs the full RS-BA1
// handshake or fails without stealing a session held by another client (R2);
// Disconnect tears a live session down.
type Manager interface {
	// Connect opens the control session to the configured radio host. It
	// blocks until the handshake completes or fails (U4).
	Connect(ctx context.Context) error
	// Disconnect closes the live session, if any. Safe with no session.
	Disconnect()
	// Execute applies one /cmd payload (stationa value-key convention) to
	// the radio; it fails when no live session is held (U5 wires the bus
	// actions onto it).
	Execute(ctx context.Context, payload []byte) error
	// Poll refreshes the radio-measured state for the bridge's /state
	// assembly while a session is live (U4/U5).
	Poll(ctx context.Context) error
}

// Stub is the obviously-marked U1 placeholder Manager. Connect always fails
// with ErrNotImplemented, so the scaffold's radio loop backs off forever and
// every bus cmd is logged and rejected — exactly the "not deployed yet"
// posture; the bridge only goes live when U2-U5 land.
type Stub struct {
	Log *slog.Logger
}

// Compile-time proof the stub satisfies the seam U2-U5 fill.
var _ Manager = (*Stub)(nil)

// Connect returns ErrNotImplemented until U4 lands the session manager.
func (s *Stub) Connect(_ context.Context) error {
	return ErrNotImplemented
}

// Disconnect is a no-op on the stub (there is never a session to drop).
func (s *Stub) Disconnect() {}

// Execute logs the rejected cmd at Warn (visible to `journalctl -p warning`,
// logging convention §3) and returns ErrNotImplemented. U5 replaces this
// with the real one-shot dispatch (ts gate, clear-after-execute-or-reject).
func (s *Stub) Execute(_ context.Context, payload []byte) error {
	s.Log.Warn("cmd received but the radio session stack is not implemented (U1 scaffold)",
		"payload_bytes", len(payload))
	return ErrNotImplemented
}

// Poll returns ErrNotImplemented until U4 lands the telemetry loop.
func (s *Stub) Poll(_ context.Context) error {
	return ErrNotImplemented
}
