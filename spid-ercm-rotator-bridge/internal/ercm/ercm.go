// Package ercm drives the elevation axis of the satellite rotator mount: a
// GS-500 elevation rotor behind an Ing.-Büro Alba (DF9GR) ERC-M controller,
// speaking the GS-232B dialect over USB-serial (plan KTD6, Appendix B).
//
// Driver contract — the same shape as U2's SPID driver (internal/spid), so
// U4's mount dispatch can consume either axis behind one per-axis Controller
// interface (set-target, stop, cached readback + validity, online):
//
//	SetTarget(el)   elevation goto; DEFERRED (accepted, not written) while no
//	                azimuth has been cached yet — see the deferral rule below
//	Stop()          stop the elevation axis (E command); clears a deferred target
//	Readback()      (el, valid) — cached from the last C2 reply; valid is false
//	                until the first C2 reply after start or after any Down
//	                transition (KTD7 always-write rule)
//	Online()        the serial link health (device_online, not /status)
//	Firmware()      the rFMW string, read once per (re)open for /meta
//	Run(ctx)        the poll loop: open, init, C2 every tick, self-heal
//
// Wire dialect (Appendix B, CR-terminated commands):
//
//	Waaa eee   goto: az AND el in one command (single space separator). The
//	           elevation axis needs the CURRENT az as an operand, which this
//	           bridge does not own (azimuth lives on the SPID driver) — the
//	           driver caches the az each C2 reply reports alongside el.
//	C2         readback: B-mode "AZ=aaa  EL=eee"; the A-mode "+0aaa+0eee"
//	           shape and elevation-only "EL=eee" replies are tolerated.
//	E / S      stop elevation / stop both. The driver sends E; the mock
//	           accepts both spellings.
//	rFMW       firmware version string, read once at startup for /meta.
//
// The az-deferral rule (KTD6, distinct from U4's deadband skip): a W arriving
// with NO cached az — post-start/post-reopen, pre-first-readback — is DEFERRED
// until the next poll tick supplies one. Never is a W written with a
// fabricated az operand. Stop clears a deferred target.
//
// Concurrency model: one mutex (d.mu) guards the port handle, the reader
// generation and the caches; the port WRITE phases of every path run under
// it, so writes serialize. The reply WAIT of the poll exchange deliberately
// runs WITHOUT d.mu — a slow or silent controller parks the poll loop, never
// the control paths (SetTarget/Stop/Readback stay live). Only Run exchanges
// (waits for replies), so reply lines have exactly one consumer.
//
// Bench-only settings — NEVER issued at boot by this driver (the boot path
// must not mutate or even probe controller configuration; these are recorded
// here so the bench bring-up knows where they belong):
//
//	bench: rBAU    baud-rate query — 9600 is typical, the default is
//	               undocumented (bench-probe; the configured baud rides
//	               [slot.serial].baud / config.SerialConfig.Baud)
//	bench: rPRO    protocol mode query — must read 1 (GS-232B); the DCU-1
//	               mode is azimuth-only and cannot carry the GS-500 axis
//	bench: rAR2/rAL2/rCR2/rCL2  elevation calibration (multi-point, every 5°)
//	               — Service-Tool work; the bridge only ever reads positions
//
// Self-heal (KTD7): reopen via the opener closure re-resolving the stable
// /dev/serial/by-id symlink, retry indefinitely after a cooldown, tag every
// reader goroutine with a generation so late errors from a replaced port are
// ignored, and re-init (re-read rFMW) after every successful reopen. Every
// Down transition clears readback validity and the cached az, so post-reopen
// behavior is always-write until the first fresh C2 reply.
package ercm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	serial "go.bug.st/serial"
)

// Errors surfaced by the driver contract.
var (
	// ErrClosed is returned by every method after Close.
	ErrClosed = errors.New("ercm driver closed")
	// ErrOffline is returned by motion paths while the serial link is down
	// (U4 layers its liveness refusal on top of this local view).
	ErrOffline = errors.New("ercm controller offline")
	// ErrOutOfRange refuses a target the 3-digit W operands cannot carry.
	ErrOutOfRange = errors.New("target out of GS-232B operand range")
)

// GS-232B command spellings (Appendix B, CR-terminated).
const (
	cmdReadback = "C2"   // az+el readback (B-mode "AZ=aaa  EL=eee")
	cmdGoto     = "W"    // az AND el in one command, "W%03d %03d"
	cmdStop     = "E"    // stop elevation (S = stop both; mock accepts either)
	cmdFirmware = "rFMW" // firmware version, read once per (re)open for /meta
)

// Config wires one driver instance to its serial port. It is the ercm-side
// shape of the bridge config: main (U5) maps a config.SlotConfig's serial
// table plus the control cadence into this struct.
type Config struct {
	// Port is the stable /dev/serial/by-id/... symlink. An EMPTY port
	// selects the in-process mock device (KTD7) so the whole stack runs
	// bench- and CI-side without hardware.
	Port string
	// Baud is the controller line speed (ERC-M GS-232B typically 9600,
	// bench-probe rBAU). Recorded for /meta and the real-port opener; the
	// default plain-file opener cannot program the UART line speed — see
	// New for the opener-injection seam.
	Baud int
	// PollInterval is the readback poll period (config-bounded ~1 s).
	PollInterval time.Duration
	// ReopenCooldown bounds reopen attempts after a serial fault so a
	// flapping adapter cannot spin open/read/close in a tight loop.
	ReopenCooldown time.Duration
	// ReplyTimeout bounds each exchange's wait for a reply line.
	ReplyTimeout time.Duration
	// Opener opens a fresh port handle. Tests (and any deployment needing
	// real termios line-speed programming) inject their own; nil selects the
	// default: mock for an empty Port, plain file I/O otherwise. Every
	// reopen calls it again, re-resolving the by-id symlink — the KTD7
	// self-heal model borrowed from ultrabridge's opener closure.
	Opener func() (io.ReadWriteCloser, error)
}

// defaults applied by New for unset cadence fields.
const (
	defaultPollInterval   = time.Second
	defaultReopenCooldown = 2 * time.Second
	defaultReplyTimeout   = 2 * time.Second
)

// genErr tags a reader-goroutine error with the generation of the reader that
// produced it, so an error from a reader that has already been replaced (a
// reopen started a fresh one) does not tear down the live link (pelcobridge2's
// generation-tagging pattern).
type genErr struct {
	gen int
	err error
}

// Driver is the elevation-axis serial driver for the ERC-M/GS-500. Run owns
// the poll loop; every other method is a thread-safe entry point for the
// control paths (U4's façade, later the servers).
type Driver struct {
	cfg    Config
	log    *slog.Logger
	opener func() (io.ReadWriteCloser, error)

	mu sync.Mutex

	closed          bool
	port            io.ReadWriteCloser
	gen             int // current reader generation
	online          bool
	lastOpenAttempt time.Time

	// readback cache: valid is false until the first C2 reply after start
	// or after any Down transition (KTD7); cachedAz feeds the W operand.
	valid    bool
	cachedAz float64
	haveAz   bool
	cachedEl float64

	pendingEl *float64 // deferred target awaiting a first az (KTD6)
	firmware  string   // rFMW reply, read once per (re)open
	lastErr   string

	rx    chan string // reply lines from the current reader generation
	rxErr chan genErr // reader errors, tagged with their generation
}

// New constructs a Driver. With no injected Opener, an empty Port selects the
// in-process mock (a fresh MockDevice per open, mirroring a device
// power-cycle); a non-empty Port opens plain file I/O on the by-id path.
//
// NOTE on baud: the default plain-file opener cannot program the UART line
// speed (this module deliberately carries no serial library). The FTDI
// virtual COM powering the ERC-M enumerates at the OS default, and the bench
// bring-up pins rBAU against it; if the deployed link needs explicit termios
// programming, U5's wiring injects the opener here and the driver does not
// change.
func New(cfg Config, log *slog.Logger) *Driver {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.ReopenCooldown <= 0 {
		cfg.ReopenCooldown = defaultReopenCooldown
	}
	if cfg.ReplyTimeout <= 0 {
		cfg.ReplyTimeout = defaultReplyTimeout
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	opener := cfg.Opener
	if opener == nil {
		if cfg.Port == "" {
			// KTD7: empty port ⇒ in-process mock, fresh device per (re)open.
			opener = func() (io.ReadWriteCloser, error) { return NewMock().Port(), nil }
		} else {
			port := cfg.Port
			baud := cfg.Baud
			opener = func() (io.ReadWriteCloser, error) {
				p, err := serial.Open(port, &serial.Mode{BaudRate: baud})
				if err != nil {
					return nil, fmt.Errorf("open serial %s @ %d baud: %w", port, baud, err)
				}
				return p, nil
			}
		}
	}

	return &Driver{
		cfg:    cfg,
		log:    log,
		opener: opener,
		rx:     make(chan string, 16),
		rxErr:  make(chan genErr, 4),
	}
}

// SetTarget commands the elevation axis to deg. While no azimuth has been
// cached (post-start/post-reopen, pre-first-readback) the intent is DEFERRED:
// it is accepted, stored, and written as a W carrying the az the next C2
// reply supplies — never with a fabricated az operand (KTD6). A deferred
// target is cleared by Stop and superseded by a later SetTarget.
func (d *Driver) SetTarget(deg float64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrClosed
	}
	if !d.online {
		return ErrOffline
	}
	if !inOperandRange(deg) {
		return ErrOutOfRange
	}
	if !d.haveAz {
		el := deg
		d.pendingEl = &el
		d.log.Debug("elevation goto deferred until first readback supplies az", "el", deg)
		return nil
	}
	return d.writeGotoLocked(d.cachedAz, deg)
}

// Stop halts the elevation axis (E command) and clears any deferred target:
// a stop must never be followed by a stale parked intent moving the axis
// (KTD8's queue-cancel discipline, applied to this driver's deferral).
func (d *Driver) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrClosed
	}
	if !d.online {
		return ErrOffline
	}
	d.pendingEl = nil
	if _, err := io.WriteString(d.port, cmdStop+"\r"); err != nil {
		d.linkDownLocked(fmt.Errorf("write stop: %w", err))
		return fmt.Errorf("write stop: %w", err)
	}
	return nil
}

// Readback returns the cached elevation and whether it is valid. valid is
// false until the first C2 reply after start or after any Down transition,
// feeding KTD8's deadband rule (unknown ⇒ always write) and KTD9's -11.
func (d *Driver) Readback() (el float64, valid bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cachedEl, d.valid
}

// Online reports the serial link health — the device_online layer, not the
// bridge /status layer.
func (d *Driver) Online() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.online
}

// Firmware returns the rFMW string read at the last successful open, for
// /meta.device.firmware.
func (d *Driver) Firmware() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.firmware
}

// LastError returns the diagnostic string of the most recent link fault, ""
// when the link never faulted (or after a clean reopen).
func (d *Driver) LastError() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastErr
}

// Close tears the link down; Run returns shortly after.
func (d *Driver) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	if d.port != nil {
		_ = d.port.Close()
		d.port = nil
	}
	d.online = false
	d.valid = false
	d.haveAz = false
}

// Run drives the poll loop until ctx is cancelled or the driver is Closed:
// open (retrying indefinitely, cooldown-bounded), init (rFMW), then one C2
// readback exchange per tick. Errors never end the loop — the link goes Down
// and a later tick reopens (KTD7).
func (d *Driver) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.mu.Lock()
		closed := d.closed
		online := d.online
		d.mu.Unlock()
		if closed {
			return ErrClosed
		}
		if !online {
			if err := d.tryOpen(ctx); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if !sleepCtx(ctx, d.cfg.ReopenCooldown) {
					return ctx.Err()
				}
				continue
			}
		}

		// One readback tick. The reply wait runs without d.mu, so a slow or
		// silent controller parks the loop, never the control paths.
		line, err := d.exchange(ctx, cmdReadback)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue // the exchange already took the link Down
		}
		d.applyReadback(line)

		if !sleepCtx(ctx, d.cfg.PollInterval) {
			return ctx.Err()
		}
	}
}

// tryOpen brings the link up: opener (cooldown-gated), a fresh reader
// generation, then re-init (rFMW) — the controller may have power-cycled in
// every gap the link was down, so the init re-runs after EVERY successful
// (re)open, not just the first (KTD7).
func (d *Driver) tryOpen(ctx context.Context) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return ErrClosed
	}
	if !d.lastOpenAttempt.IsZero() && time.Since(d.lastOpenAttempt) < d.cfg.ReopenCooldown {
		d.mu.Unlock()
		return fmt.Errorf("reopen cooldown (%s) not elapsed", d.cfg.ReopenCooldown)
	}
	d.lastOpenAttempt = time.Now()

	rw, err := d.opener()
	if err != nil {
		d.lastErr = fmt.Sprintf("open serial: %v", err)
		d.mu.Unlock()
		return fmt.Errorf("open serial: %w", err)
	}
	d.port = rw
	d.online = true
	d.startReaderLocked()
	d.mu.Unlock()

	// Re-init: read the firmware once. A reply that is not a position line
	// lands in /meta's firmware field; position-shaped lines (a stale reply
	// draining out of the previous reader) are ignored.
	line, err := d.exchange(ctx, cmdFirmware)
	if err != nil {
		if ctx.Err() == nil {
			d.mu.Lock()
			d.linkDownLocked(err)
			d.mu.Unlock()
		}
		return err
	}
	if _, _, hasAz, hasEl := parsePosition(line); !hasAz && !hasEl {
		if fw := strings.TrimSpace(line); fw != "" {
			d.mu.Lock()
			d.firmware = fw
			d.mu.Unlock()
		}
	}
	return nil
}

// applyReadback folds one C2 reply into the cache and — if a goto was
// deferred for want of an az — writes it now with the az this reply supplied.
func (d *Driver) applyReadback(line string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	az, el, hasAz, hasEl := parsePosition(line)
	if hasAz {
		d.cachedAz = az
		d.haveAz = true
	}
	if hasEl {
		d.cachedEl = el
		d.valid = true
	}
	if d.pendingEl != nil && d.haveAz {
		pending := *d.pendingEl
		d.pendingEl = nil
		_ = d.writeGotoLocked(d.cachedAz, pending)
	}
}

// writeGotoLocked puts one Waaa eee on the wire: the current az operand plus
// the target el. A write fault takes the link Down; the intent is NOT retried
// on reopen — re-sending a motion command with no operator behind it is the
// ultrabridge stale-cmd pattern. The control path re-issues. Callers must
// hold d.mu.
func (d *Driver) writeGotoLocked(az, el float64) error {
	if !inOperandRange(az) || !inOperandRange(el) {
		return ErrOutOfRange
	}
	cmd := fmt.Sprintf("%s%03d %03d\r", cmdGoto, roundDeg(az), roundDeg(el))
	if _, err := io.WriteString(d.port, cmd); err != nil {
		err = fmt.Errorf("write goto: %w", err)
		d.linkDownLocked(err)
		return err
	}
	return nil
}

// exchange writes one command (under d.mu, serializing with every other
// port write) and waits for its reply line WITHOUT holding d.mu, bounded by
// ReplyTimeout. Only Run exchanges, so reply lines have exactly one
// consumer. A reader error or a timeout from the CURRENT generation is a
// link fault: the link goes Down and self-heals on a later tick (KTD7).
func (d *Driver) exchange(ctx context.Context, cmd string) (string, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return "", ErrClosed
	}
	if !d.online || d.port == nil {
		d.mu.Unlock()
		return "", ErrOffline
	}
	port := d.port
	gen := d.gen
	if _, err := io.WriteString(port, cmd+"\r"); err != nil {
		err = fmt.Errorf("write %q: %w", cmd, err)
		d.linkDownLocked(err)
		d.mu.Unlock()
		return "", err
	}
	d.mu.Unlock()

	timeout := time.NewTimer(d.cfg.ReplyTimeout)
	defer timeout.Stop()
	for {
		select {
		case line := <-d.rx:
			return line, nil
		case ge := <-d.rxErr:
			if ge.gen != gen {
				continue // stale reader; the current generation owns the line
			}
			d.mu.Lock()
			d.linkDownLocked(ge.err)
			d.mu.Unlock()
			return "", ge.err
		case <-timeout.C:
			err := fmt.Errorf("timeout waiting for reply to %q", cmd)
			d.mu.Lock()
			d.linkDownLocked(err)
			d.mu.Unlock()
			return "", err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// startReaderLocked spawns a fresh reader goroutine for the current port and
// bumps the generation, so late errors from the reader of a replaced port are
// tagged stale and ignored. Any reply lines buffered from the previous
// generation are drained. Callers must hold d.mu.
func (d *Driver) startReaderLocked() {
	d.gen++
	gen := d.gen
	port := d.port

	// Drain lines the old generation buffered: a late reply must not be
	// mistaken for the answer to this generation's first command.
	for {
		select {
		case <-d.rx:
			continue
		default:
		}
		break
	}

	go func() {
		r := bufio.NewReader(port)
		for {
			line, err := readLine(r)
			if err != nil {
				select {
				case d.rxErr <- genErr{gen: gen, err: err}:
				default: // nobody is exchanging; the fault surfaces on next use
				}
				return
			}
			if line == "" {
				continue
			}
			select {
			case d.rx <- line:
			default: // no exchange is running; drop unsolicited chatter
			}
		}
	}()
}

// linkDownLocked handles a link fault: close the stale handle (which unblocks
// the reader goroutine of that generation), mark offline, and clear the
// cached readback + az so post-reopen behavior is always-write until the
// first fresh C2 reply (KTD7). A deferred target SURVIVES: it is an admitted
// intent that never hit the wire, and it still defers until the healed link
// supplies an az. Idempotent. Callers must hold d.mu.
func (d *Driver) linkDownLocked(err error) {
	if d.port != nil {
		_ = d.port.Close()
	}
	d.port = nil
	d.online = false
	d.valid = false
	d.haveAz = false
	d.lastErr = err.Error()
	d.log.Warn("ercm link down, will reopen", "err", err)
}

// readLine reads one CR- or LF-terminated line. A terminator with nothing
// accumulated (the LF of a CRLF pair) keeps reading — it is not an empty
// reply.
func readLine(r *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\r' || b == '\n' {
			if sb.Len() == 0 {
				continue
			}
			return sb.String(), nil
		}
		sb.WriteByte(b)
	}
}

// sleepCtx sleeps for d unless ctx ends first; false means ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// inOperandRange guards the 3-digit W fields (az 0..360 and el 0..90 both fit;
// anything beyond 999 would corrupt the fixed-width spelling).
func inOperandRange(deg float64) bool {
	return deg >= 0 && deg <= 999 && !math.IsNaN(deg) && !math.IsInf(deg, 0)
}

// roundDeg formats a float position into the GS-232B integer-degree fields.
func roundDeg(deg float64) int {
	return int(math.Round(deg))
}

// GS-232B readback shapes (Appendix B; wrc-rotator-bridge's tolerant-parsing
// style — match the token, never the whole line, so spacing and spelling
// variants from vendor firmware revisions still parse):
//
//	B-mode: "AZ=aaa  EL=eee" (also "AZ=aaa" or "EL=eee" alone)
//	A-mode: "+0aaa+0eee" (also a bare "+0eee" elevation-only fallback)
var (
	reBModeAz   = regexp.MustCompile(`(?i)AZ\s*=\s*(\d+(?:\.\d+)?)`)
	reBModeEl   = regexp.MustCompile(`(?i)EL\s*=\s*(\d+(?:\.\d+)?)`)
	reAModeBoth = regexp.MustCompile(`^\+0*(\d{3,4})\+0*(\d{3,4})$`)
	reAModeEl   = regexp.MustCompile(`^\+0*(\d{3,4})$`)
)

// parsePosition tolerantly extracts azimuth/elevation from one controller
// reply line. It reports what the line carried: a B-mode el-only reply sets
// hasEl alone, an A-mode line sets both. Lines that match no known shape are
// ignored as chatter (the caller keeps its previous cache).
func parsePosition(line string) (az, el float64, hasAz, hasEl bool) {
	s := strings.TrimSpace(line)
	if s == "" {
		return 0, 0, false, false
	}
	if m := reBModeAz.FindStringSubmatch(s); m != nil {
		az, _ = strconv.ParseFloat(m[1], 64)
		hasAz = true
	}
	if m := reBModeEl.FindStringSubmatch(s); m != nil {
		el, _ = strconv.ParseFloat(m[1], 64)
		hasEl = true
	}
	if hasAz || hasEl {
		return az, el, hasAz, hasEl
	}
	if m := reAModeBoth.FindStringSubmatch(s); m != nil {
		az, _ = strconv.ParseFloat(m[1], 64)
		el, _ = strconv.ParseFloat(m[2], 64)
		return az, el, true, true
	}
	if m := reAModeEl.FindStringSubmatch(s); m != nil {
		el, _ = strconv.ParseFloat(m[1], 64)
		return 0, el, false, true
	}
	return 0, 0, false, false
}
