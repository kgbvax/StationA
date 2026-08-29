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

	// open resolves portPath to a fresh serial.Port. Kept as a field so the
	// fault self-heal (reopen) can re-resolve the — preferably by-id — path
	// after the USB-serial adapter drops and re-enumerates under a new tty, and
	// so tests can inject a mock port factory.
	open func() (serial.Port, error)

	mu         sync.Mutex
	port       serial.Port
	closed     bool
	lastReopen time.Time // guarded by mu; rate-bounds the in-place self-heal

	stateMu sync.RWMutex
	mode    string // raw firmware mode (diagnostic)
	band    int    // amp band index 1..10 (0 = unknown), for navigation
	online  bool
}

// New constructs a Device for the given port path and averaging window.
func New(portPath string, avgMs int, debug bool, log Logger) *Device {
	d := &Device{
		portPath: portPath,
		avgMs:    avgMs,
		debug:    debug,
		log:      log,
	}
	d.open = func() (serial.Port, error) {
		mode := &serial.Mode{
			BaudRate: 9600,
			DataBits: 8,
			Parity:   serial.NoParity,
			StopBits: serial.OneStopBit,
		}
		return serial.Open(d.portPath, mode)
	}
	return d
}

// openPort dials the port via d.open and applies the standard setup (1 s read
// timeout, flushed buffers). It does not touch Device state; callers own the
// resulting handle.
func (d *Device) openPort() (serial.Port, error) {
	port, err := d.open()
	if err != nil {
		return nil, err
	}
	if err := port.SetReadTimeout(1 * time.Second); err != nil {
		d.log.Warnf("set read timeout: %v", err)
	}
	port.ResetInputBuffer()
	port.ResetOutputBuffer()
	return port, nil
}

// Open opens the serial port (9600 8N1), resets the buffers, marks the device
// online, and sends the initial enable-telemetry command.
//
// The bridge is a pure observer of the amplifier: it neither drives the amp's
// power state nor touches the host's RTS line. PA power-on/off is owned by the
// power-distribution layer (the hf/switch slot's remote-on relays); this slot
// only reports the resulting power state in telemetry (pa.power).
func (d *Device) Open() error {
	port, err := d.openPort()
	if err != nil {
		return err
	}

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
// unblocks the read. Port faults are self-healed in place when possible: see
// the port == nil branch below for the reopen-retry half.
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
			// A previous reopen failed — typically ENOENT because udev has not
			// yet recreated the by-id path after the adapter re-enumerated.
			// Retry the open in place, spaced by reopenMinInterval and bounded
			// by the silence watchdog, rather than tearing the run down: the
			// canonical drop + re-enumeration fault heals here without
			// flipping /state.device_online. (This is ultrabridge's "rw == nil,
			// next poll tick retries" half, adapted to a push-based loop; the
			// sleep is only a spin guard — the rate bound does the pacing.)
			if time.Since(lastDataTime) > silenceLimit {
				return fmt.Errorf("no data received for %s (port unrecoverable)", silenceLimit)
			}
			if d.selfHealAllowed() {
				if rerr := d.reopen(); rerr == nil {
					// Fresh link: partial frames from the old handle no longer
					// apply. (Silence timers DO carry over — a reopened port
					// that delivers nothing is still silent.)
					rxBuf = rxBuf[:0]
					lastRetryTime = time.Now() // reopen re-armed telemetry already
					continue
				} else {
					d.log.Warnf("reopen retry: %v", rerr)
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(reopenRetrySleep):
			}
			continue
		}

		n, err := port.Read(tmpBuf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// The silence watchdog applies on the fault path too: a link that
			// faults (rather than times out) without delivering data for
			// silenceLimit must not be held online by repeated reopens —
			// device_online must reflect a dead link. lastDataTime is only
			// ever reset by real data, never by a reopen.
			if time.Since(lastDataTime) > silenceLimit {
				return fmt.Errorf("no data received for %s (last fault: %w)", silenceLimit, err)
			}
			// Port-level fault — typically EIO after the USB-serial adapter
			// dropped and the kernel re-enumerated it under a new tty. Try one
			// in-place reopen (re-resolving the by-id path) before tearing the
			// run down: a transient glitch heals without flipping
			// device_online, while a persistently broken link surfaces the
			// error for the serial restart loop's backoff. See reopen.
			if d.selfHealAllowed() {
				if rerr := d.reopen(); rerr == nil {
					// Fresh link: partial frames from the old handle no longer
					// apply. (Silence timers DO carry over — see above.)
					rxBuf = rxBuf[:0]
					lastRetryTime = time.Now()
					continue
				} else {
					// The reopen itself failed — usually the by-id path is
					// simply not there yet (udev still recreating it). Loop
					// around to the in-place retry path above instead of
					// returning: an adapter that is coming back is exactly
					// what the in-place heal exists for.
					d.log.Warnf("reopen after fault: %v", rerr)
					continue
				}
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
		if time.Since(lastDataTime) > silenceLimit {
			return fmt.Errorf("no data received for %s, restarting monitor", silenceLimit)
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

// Timing knobs for the run loop's self-heal and watchdog. Vars (not consts) so
// tests can compress them; the defaults are the live values.
var (
	// reopenMinInterval bounds how often the in-place self-heal may retry after
	// a port fault. A transient USB re-enumeration heals within one reopen; a
	// link that faults again inside this window is treated as persistently
	// broken and falls back to the serial restart loop (whose backoff spaces
	// the retries). This mirrors ultrabridge's "one reopen per exchange, poll
	// tick spaces retries" bound, adapted to acom's push-based read loop.
	reopenMinInterval = 2 * time.Second

	// silenceLimit is how long the run loop tolerates a silent link (read
	// timeouts OR read faults healed by reopen) before giving up and letting
	// the serial restart loop mark the device offline. It applies to both the
	// timeout path and the fault path: a link that keeps faulting but never
	// delivers data is silent, however many times the port reopens.
	silenceLimit = 30 * time.Second

	// reopenRetrySleep spaces the idle loop between failed reopen attempts.
	// Purely a spin guard while waiting out the reopenMinInterval window —
	// the rate bound does the actual pacing.
	reopenRetrySleep = 100 * time.Millisecond
)

// selfHealAllowed reports whether an in-place reopen may be attempted now: the
// device must not be closed (shutdown) and the previous reopen attempt must be
// older than reopenMinInterval (no tight reopen loop against a flapping port).
func (d *Device) selfHealAllowed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.closed && time.Since(d.lastReopen) >= reopenMinInterval
}

// reopen swaps the stale serial handle for a fresh one, re-resolving the
// (preferably by-id) path so a USB-serial adapter that dropped and
// re-enumerated under a new tty is picked up without tearing the run loop —
// and without flipping /state.device_online, which a full serialLoop restart
// does. The stale handle is closed best-effort (it is likely already gone).
// On failure d.port is left nil and the caller falls back to the restart path.
func (d *Device) reopen() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return fmt.Errorf("device closed")
	}
	d.lastReopen = time.Now()
	stale := d.port
	d.port = nil
	d.mu.Unlock()

	if stale != nil {
		_ = stale.Close()
	}
	port, err := d.openPort()
	if err != nil {
		return err
	}

	d.mu.Lock()
	if d.closed { // Close() raced us during the open; don't leak the handle.
		d.mu.Unlock()
		port.Close()
		return fmt.Errorf("device closed")
	}
	d.port = port
	d.mu.Unlock()

	d.log.Infof("serial port reopened after fault (%s)", d.portPath)
	// The amp keeps streaming once enabled, so telemetry resumes on its own;
	// re-arm anyway in case the amp itself rebooted (PSU cycled) while the
	// adapter was down. Failure is non-fatal — data may still flow.
	if err := d.EnableTelemetry(); err != nil {
		d.log.Warnf("re-arm telemetry after reopen: %v", err)
	}
	return nil
}

// Online reports whether the serial port is currently open.
func (d *Device) Online() bool {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.online
}

// CurrentMode returns the last raw firmware mode (diagnostic).
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
