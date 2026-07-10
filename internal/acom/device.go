package acom

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"go.bug.st/serial"
)

// Logger is the minimal logging surface the device uses (same shape as
// bridge.Logger, so one adapter satisfies both).
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Device owns the serial port to the amplifier and the protocol-level state
// the wire layer needs (current raw mode for the watchdog, current band index
// for relative band navigation). Canonical published state lives in the bridge.
type Device struct {
	portPath string
	avgMs    int
	debug    bool
	log      Logger

	mu     sync.Mutex
	port   serial.Port
	closed bool

	stateMu sync.RWMutex
	mode    string // raw firmware mode, for the watchdog
	band    int    // amp band index 1..10 (0 = unknown), for navigation
	online  bool
}

// New constructs a Device for the given port path and averaging window.
func New(portPath string, avgMs int, debug bool, log Logger) *Device {
	return &Device{portPath: portPath, avgMs: avgMs, debug: debug, log: log}
}

// Open opens the serial port (9600 8N1), resets the buffers, marks the device
// online, and sends the initial enable-telemetry command.
func (d *Device) Open() error {
	mode := &serial.Mode{
		BaudRate: 9600,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(d.portPath, mode)
	if err != nil {
		return err
	}
	if err := port.SetReadTimeout(1 * time.Second); err != nil {
		d.log.Warnf("set read timeout: %v", err)
	}
	port.ResetInputBuffer()
	port.ResetOutputBuffer()

	d.mu.Lock()
	d.port = port
	d.closed = false
	d.mu.Unlock()

	d.setOnline(true)
	if err := d.EnableTelemetry(); err != nil {
		return fmt.Errorf("failed to enable telemetry: %w", err)
	}
	d.log.Infof("telemetry enable command sent on %s", d.portPath)
	return nil
}

// Run is the read loop. It blocks reading frames, ACKing each and emitting
// telemetry observations to onObs, until the port errors, 30 s of silence
// elapses, or ctx is cancelled. Closing the port (from Close or ctx cancel)
// unblocks the read.
func (d *Device) Run(ctx context.Context, onObs func(Observation)) error {
	// Arrange for ctx cancellation to close the port and unblock the read.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			d.Close()
		case <-stop:
		}
	}()

	fwdAvg := NewPowerAverager(d.avgMs)
	rxBuf := make([]byte, 0, 1024)
	tmpBuf := make([]byte, 128)
	lastDataTime := time.Now()
	lastRetryTime := time.Now()

	for {
		d.mu.Lock()
		port := d.port
		d.mu.Unlock()
		if port == nil {
			return fmt.Errorf("serial port closed")
		}

		n, err := port.Read(tmpBuf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}

		if n > 0 {
			lastDataTime = time.Now()
			rxBuf = append(rxBuf, tmpBuf[:n]...)
			rxBuf = d.processBuffer(rxBuf, fwdAvg, onObs)
			continue
		}

		// n == 0: read timeout.
		if time.Since(lastDataTime) > 30*time.Second {
			return fmt.Errorf("no data received for 30s, restarting monitor")
		}
		if time.Since(lastDataTime) > 5*time.Second && time.Since(lastRetryTime) > 5*time.Second {
			d.log.Infof("no data for 5s, re-sending enable telemetry")
			if err := d.EnableTelemetry(); err != nil {
				d.log.Warnf("re-send enable telemetry: %v", err)
			}
			lastRetryTime = time.Now()
		}
	}
}

// Close closes the serial port and marks the device offline. Safe to call
// repeatedly.
func (d *Device) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	port := d.port
	d.port = nil
	d.mu.Unlock()
	if port != nil {
		port.Close()
	}
	d.setOnline(false)
}

// Online reports whether the serial port is currently open.
func (d *Device) Online() bool {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.online
}

// CurrentMode returns the last raw firmware mode (for the watchdog).
func (d *Device) CurrentMode() string {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.mode
}

// CurrentBand returns the last amp band index 1..10 (0 = unknown).
func (d *Device) CurrentBand() int {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.band
}

func (d *Device) setOnline(v bool) {
	d.stateMu.Lock()
	d.online = v
	d.stateMu.Unlock()
}

// processBuffer scans buf for complete frames, ACKs each, and emits telemetry
// observations. It returns the unconsumed remainder of buf.
func (d *Device) processBuffer(buf []byte, fwdAvg *PowerAverager, onObs func(Observation)) []byte {
	for len(buf) >= 3 {
		if buf[0] != StartByte {
			buf = buf[1:]
			continue
		}
		pktLen := int(buf[2])
		// The length byte is the total frame length. The smallest valid ACOM
		// frame is 4 bytes (start + addr + len + data/checksum). A declared
		// length < 4 is noise/corruption: resync one byte rather than slicing
		// an empty/short packet (which would index out of range in
		// handlePacket, or pass verifyChecksum vacuously on an empty slice).
		if pktLen < 4 {
			buf = buf[1:]
			continue
		}
		if len(buf) < pktLen {
			return buf
		}
		packet := buf[:pktLen]
		if verifyChecksum(packet) {
			d.handlePacket(packet, fwdAvg, onObs)
			buf = buf[pktLen:]
		} else {
			buf = buf[1:]
		}
	}
	return buf
}

func (d *Device) handlePacket(packet []byte, fwdAvg *PowerAverager, onObs func(Observation)) {
	// Defensive: processBuffer enforces pktLen >= 4 and verifyChecksum rejects
	// short frames, but never trust a wire-derived length when indexing.
	if len(packet) < 4 {
		return
	}
	if d.debug {
		d.log.Debugf("RX PKT: %s", hex.EncodeToString(packet))
	}
	addr := packet[1]
	_ = d.sendAck(addr)
	if addr == MsgTelemetry {
		obs, ok := ParseTelemetry(packet, fwdAvg)
		if !ok {
			return
		}
		d.stateMu.Lock()
		d.mode = obs.ModeRaw
		d.band = obs.BandIndex
		d.stateMu.Unlock()
		if onObs != nil {
			onObs(obs)
		}
	}
}

// ------------------------------------------------------------------
// Outbound command frames
// ------------------------------------------------------------------

// EnableTelemetry sends the MsgEnableAuto (0x92) frame that arms automatic
// telemetry streaming.
func (d *Device) EnableTelemetry() error {
	cmd := []byte{StartByte, MsgEnableAuto, 0x04, 0x15}
	return d.write("TX ENABLE", cmd)
}

// SetMode sets the amplifier mode. mode is "operate" or "standby".
func (d *Device) SetMode(mode string) error {
	var modeByte byte
	switch mode {
	case "operate":
		modeByte = ModeOPRRX
	case "standby":
		modeByte = ModeSTB
	default:
		return fmt.Errorf("unknown mode %q (want operate|standby)", mode)
	}
	packet := []byte{StartByte, MsgAmpMgmt, 0x08, CmdModeChange, 0x00, modeByte, 0x00}
	if err := d.write("TX CMD", packet); err != nil {
		return err
	}
	d.log.Infof("sent set_mode %s (0x%02X)", mode, modeByte)
	return nil
}

// SetBand walks the amplifier from its current band to the named band using
// next/prev steps (the protocol exposes only relative band changes).
func (d *Device) SetBand(band string) error {
	target, ok := BandNameToIndex(band)
	if !ok {
		return fmt.Errorf("unknown band %q", band)
	}
	current := d.CurrentBand()
	if current == 0 {
		return fmt.Errorf("current band unknown, cannot navigate")
	}
	if current == target {
		return nil
	}
	direction, steps := BandNext, target-current
	if target < current {
		direction, steps = BandPrev, current-target
	}
	d.log.Infof("band navigation: %d -> %d (%d steps)", current, target, steps)
	for i := 0; i < steps; i++ {
		packet := []byte{StartByte, MsgAmpMgmt, 0x08, CmdBandChange, 0x00, direction, 0x00}
		if err := d.write("TX BAND", packet); err != nil {
			return fmt.Errorf("band step %d/%d: %w", i+1, steps, err)
		}
		time.Sleep(150 * time.Millisecond)
	}
	return nil
}

// sendAck ACKs a received frame.
func (d *Device) sendAck(msgReceivedAddr byte) error {
	packet := []byte{StartByte, MsgAck, 0x05, msgReceivedAddr}
	return d.write("TX ACK", packet)
}

// write sends a packet to the port under the device mutex. Returns an error if
// the port is not open.
func (d *Device) write(label string, packet []byte) error {
	// Frames carrying a checksum append it as the final byte.
	if packet[1] != MsgEnableAuto {
		packet = append(packet, calculateChecksum(packet))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.port == nil {
		return fmt.Errorf("serial port not active")
	}
	if d.debug {
		d.log.Debugf("%s: %s", label, hex.EncodeToString(packet))
	}
	_, err := d.port.Write(packet)
	return err
}
