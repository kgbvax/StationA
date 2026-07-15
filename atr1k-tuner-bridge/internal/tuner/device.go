// Package tuner talks to the ATR-1000 ATU (BTR-1000 / N7DDC family) over its binary
// WebSocket (default :60001). It owns the wire protocol (protocol.go), the read
// loop, and the command surface; the bridge (internal/bridge) translates between
// this and the canonical station-model state.
package tuner

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeTimeout = 5 * time.Second
	tuneTimeout  = 12 * time.Second
)

// Commander is the tuner control surface the bridge drives from /cmd. *Device
// implements it.
type Commander interface {
	SetInline(inline bool) error // TuneStatus: true = in line, false = bypass
	Tune(full bool) error        // TuneMode: full=true Full Tune, false Mem recall
}

// Logger is the minimal logging surface the device uses.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

// State is the canonical tuner state the bridge publishes (integration model §7.1,
// slot muehle/hf/tuner).
type State struct {
	Inline       bool    `json:"inline"`          // in line vs bypass
	SWR          float64 `json:"swr"`             // SWR ratio (raw/100 when ≥100)
	Fwd          uint16  `json:"fwd"`             // forward watts
	LUH          float64 `json:"l_uh"`            // inductance µH
	CPF          uint16  `json:"c_pf"`            // capacitance pF
	Settling     bool    `json:"settling"`        // a tune cycle is in progress
	Fault        string  `json:"fault,omitempty"` // "tune timeout" etc.
	DeviceOnline bool    `json:"device_online"`   // the ATR is reachable while the bridge is up
	Error        string  `json:"error,omitempty"` // human-readable device fault
}

// Device owns the ATR-1000 WebSocket connection. It runs a read loop that decodes
// meter/relay frames into canonical State and forwards them to a telemetry
// callback, and exposes a mutex-guarded write path for commands. The tune cycle
// is tracked with a timer: Settling holds while tuning and clears when the relays
// update (tune settled) or the timer fires (tune timeout → fault).
type Device struct {
	url   string
	debug bool
	log   Logger

	writeMu sync.Mutex // serializes WebSocket writes (commands)
	conn    *websocket.Conn

	mu          sync.Mutex // guards state + timer
	state       State
	timer       *time.Timer
	onTelemetry func(State) // set in Run; read-only afterward
}

// New constructs a Device for the given ATR-1000 WebSocket URL.
func New(url string, debug bool, log Logger) *Device {
	return &Device{url: url, debug: debug, log: log}
}

// Snapshot returns a thread-safe copy of the current canonical state.
func (d *Device) Snapshot() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// Run dials the ATR-1000, publishes the initial device-online state, and reads
// binary frames until the connection closes or ctx is cancelled. onTelemetry is
// called for every parsed frame (it publishes the /state snapshot). Returns the
// error that ended the run so the caller's reconnect loop can back off.
func (d *Device) Run(ctx context.Context, onTelemetry func(State)) error {
	d.mu.Lock()
	d.onTelemetry = onTelemetry
	d.mu.Unlock()

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, d.url, http.Header{})
	if err != nil {
		return fmt.Errorf("dial atr1k: %w", err)
	}
	d.writeMu.Lock()
	d.conn = conn
	d.writeMu.Unlock()
	defer d.closeConn()

	d.log.Infof("ATR-1000 WebSocket connected: %s", d.url)
	d.setOnline(true, "")
	// Request a full state snapshot from the tuner.
	_ = d.sendFrame(buildFrame(scmdSync))

	// If ctx is cancelled while blocked in ReadMessage, nudge the conn closed so
	// the read unblocks. The defer above also closes on return.
	go func() {
		<-ctx.Done()
		d.closeConn()
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("atr1k read: %w", err)
		}
		if d.debug {
			d.log.Debugf("RX from ATR: % x", data)
		}
		d.handleFrame(data)
	}
}

// SetInline puts the tuner in line (true) or bypass (false).
func (d *Device) SetInline(inline bool) error {
	var b byte
	if inline {
		b = 1
	}
	if err := d.sendFrame(buildFrame(scmdTuneStatus, b)); err != nil {
		return err
	}
	d.mu.Lock()
	d.state.Inline = inline
	snap := d.state
	d.mu.Unlock()
	d.push(snap)
	return nil
}

// Tune starts a tuning cycle (Full or Mem recall). Settling holds until the
// relays update (tune settled) or the tune timeout fires.
func (d *Device) Tune(full bool) error {
	mode := tuneModeMem
	if full {
		mode = tuneModeFull
	}
	if err := d.sendFrame(buildFrame(scmdTuneMode, mode)); err != nil {
		return err
	}
	d.mu.Lock()
	d.state.Inline = true
	d.state.Settling = true
	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer = time.AfterFunc(tuneTimeout, d.onTuneTimeout)
	snap := d.state
	d.mu.Unlock()
	d.push(snap)
	return nil
}

// handleFrame decodes one inbound binary frame and updates state.
func (d *Device) handleFrame(data []byte) {
	cmd, _, ok := parseFrame(data)
	if !ok {
		return
	}
	switch cmd {
	case scmdMeter:
		if swr, fwd, ok := meter(data); ok {
			d.mu.Lock()
			d.state.SWR = swr
			d.state.Fwd = fwd
			snap := d.state
			d.mu.Unlock()
			d.push(snap)
		}
	case scmdRelay:
		if l, c, ok := relay(data); ok {
			d.mu.Lock()
			d.state.LUH = l
			d.state.CPF = c
			d.state.Settling = false // relays updated → tune settled
			d.state.Fault = ""
			if d.timer != nil {
				d.timer.Stop()
				d.timer = nil
			}
			snap := d.state
			d.mu.Unlock()
			d.push(snap)
		}
	}
}

// onTuneTimeout fires when a tune cycle did not settle in time.
func (d *Device) onTuneTimeout() {
	d.mu.Lock()
	if d.state.Settling {
		d.state.Settling = false
		d.state.Fault = "tune timeout"
		d.log.Warnf("tuner tune timeout")
	}
	snap := d.state
	d.mu.Unlock()
	d.push(snap)
}

// sendFrame writes a command frame under the write mutex. The mutex is held
// across the write so concurrent commands (and the ctx-cancel close) serialize
// on the connection — gorilla's WriteMessage is not safe for concurrent use.
func (d *Device) sendFrame(b []byte) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	conn := d.conn
	if conn == nil {
		return fmt.Errorf("atr1k: websocket not connected")
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := conn.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return fmt.Errorf("atr1k write: %w", err)
	}
	if d.debug {
		d.log.Debugf("TX to ATR: % x", b)
	}
	return nil
}

// setOnline updates the cached device-online/error fields, preserving the last
// known measurements across a reconnect. Called when the WebSocket is lost or
// regained; the /status LWT (the bridge, not the ATR) is unaffected.
func (d *Device) setOnline(online bool, errMsg string) {
	d.mu.Lock()
	prev := d.state
	d.state = State{
		Inline:       prev.Inline,
		SWR:          prev.SWR,
		Fwd:          prev.Fwd,
		LUH:          prev.LUH,
		CPF:          prev.CPF,
		DeviceOnline: online,
		Error:        errMsg,
	}
	if !online {
		// A dropped connection clears a pending tune: it will not settle.
		if d.timer != nil {
			d.timer.Stop()
			d.timer = nil
		}
		d.state.Settling = false
	}
	snap := d.state
	d.mu.Unlock()
	d.push(snap)
}

func (d *Device) closeConn() {
	d.writeMu.Lock()
	conn := d.conn
	d.conn = nil
	d.writeMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// push forwards the snapshot to the telemetry callback (the bridge), if set.
func (d *Device) push(snap State) {
	d.mu.Lock()
	cb := d.onTelemetry
	d.mu.Unlock()
	if cb != nil {
		cb(snap)
	}
}
