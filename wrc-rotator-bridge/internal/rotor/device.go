package rotor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Commander is the rotator control surface the bridge drives from /cmd and the
// GS-232 server drives from its inbound connections. *Device implements it.
type Commander interface {
	SetAz(az float64) error // rotate to azimuth (degrees)
	Stop() error            // halt motion
	Jog(dir string) error   // "fwd" (CW) | "rev" (CCW)
}

// Logger is the minimal logging surface the device uses.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Device owns the WRC WebSocket connection. It runs a read loop that parses
// RotorStatus and forwards canonical State to a telemetry callback, and exposes
// a mutex-guarded write path for commands. The current State is kept under a
// separate read mutex so the GS-232 server can answer position queries without
// racing the read loop.
type Device struct {
	url   string
	debug bool
	log   Logger

	writeMu sync.Mutex // serializes WebSocket writes (commands)
	conn    *websocket.Conn

	stateMu sync.RWMutex
	state   State
}

// New constructs a Device for the given WRC WebSocket URL.
func New(url string, debug bool, log Logger) *Device {
	return &Device{url: url, debug: debug, log: log}
}

// Snapshot returns a thread-safe copy of the current canonical state. The
// GS-232 server uses it to answer position queries.
func (d *Device) Snapshot() State {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.state
}

// CurrentAz returns the last-known azimuth. It satisfies the gs232.Controller
// interface for position queries.
func (d *Device) CurrentAz() float64 {
	return d.Snapshot().Az
}

// Run dials the WRC, publishes the initial device-online state, and reads
// status messages until the connection closes or ctx is cancelled. onTelemetry
// is called for every parsed status (it publishes the /state snapshot). Returns
// the error that ended the run so the caller's reconnect loop can back off.
func (d *Device) Run(ctx context.Context, onTelemetry func(State)) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.DialContext(ctx, d.url, http.Header{})
	if err != nil {
		return fmt.Errorf("dial wrc: %w", err)
	}
	d.writeMu.Lock()
	d.conn = conn
	d.writeMu.Unlock()
	defer d.closeConn()

	d.log.Infof("WRC WebSocket connected: %s", d.url)
	d.setOnline(true, "")

	// If ctx is cancelled while blocked in ReadMessage, nudge the conn closed
	// so the read unblocks. The defer above also closes on return.
	go func() {
		<-ctx.Done()
		d.closeConn()
	}()

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("wrc read: %w", err)
		}
		if d.debug {
			d.log.Debugf("RX from WRC: %s", string(message))
		}
		var status RotorStatus
		if err := json.Unmarshal(message, &status); err != nil {
			// Malformed frame dropped: state was not updated from it.
			d.log.Errorf("wrc: parse status: %v", err)
			continue
		}
		st := FromStatus(status, true)
		d.stateMu.Lock()
		d.state = st
		d.stateMu.Unlock()
		if onTelemetry != nil {
			onTelemetry(st)
		}
	}
}

// SetAz rotates to the given azimuth (degrees). The WRC accepts absolute
// azimuth commands only when the value is sent as a quoted string; numeric
// values are ignored by the controller firmware.
func (d *Device) SetAz(az float64) error {
	return d.send(RotorCommand{Az: strconv.FormatFloat(az, 'f', 0, 64)})
}

// Stop halts motion.
func (d *Device) Stop() error {
	return d.send(RotorCommand{Az: "stop"})
}

// Jog issues a continuous-rotation command: "fwd" (CW) or "rev" (CCW).
func (d *Device) Jog(dir string) error {
	return d.send(RotorCommand{Az: dir})
}

// send writes a command under the write mutex. Safe to call from the /cmd
// worker and from GS-232 handler goroutines concurrently.
func (d *Device) send(cmd RotorCommand) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if d.conn == nil {
		return fmt.Errorf("wrc: websocket not connected")
	}
	if err := d.conn.WriteJSON(cmd); err != nil {
		return fmt.Errorf("wrc write: %w", err)
	}
	if d.debug {
		d.log.Debugf("TX to WRC: %+v", cmd)
	}
	return nil
}

// setOnline updates the cached device-online/error fields without a status
// message (used on connect). The read loop's telemetry carries the live values
// afterwards; this just clears a stale "offline" between connect cycles.
func (d *Device) setOnline(online bool, errMsg string) {
	d.stateMu.Lock()
	prev := d.state
	d.state = State{
		Az:           prev.Az, // keep last-known azimuth across a reconnect
		DeviceOnline: online,
		Error:        errMsg,
	}
	d.stateMu.Unlock()
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
