// SPDX-License-Identifier: AGPL-3.0-or-later

// Package spid implements the azimuth rotator driver for a SPID controller
// speaking the Rot1Prog binary protocol (plan KTD5, Appendix A): 13-byte
// command packets, a 5-byte raw-digit status reply, set commands with no
// reply, a poll loop on the configured tick, writes paced ≥300 ms.
//
// The driver self-heals indefinitely (KTD7): every read/write fault takes the
// axis down (clearing the cached-readback validity so the U4 deadband can
// never silently no-op against a pre-outage position), and the poll loop
// re-resolves the stable /dev/serial/by-id/ path after the configured
// cooldown, forever. Reader generations are tagged so a late error from a
// reader that has already been replaced by a reopen is ignored (the
// pelcobridge2 pattern). After every successful reopen the readback starts
// unknown again until the first fresh status reply — Rot1Prog has no init
// command sequence, so the status request on the next tick IS the re-init.
//
// An empty configured serial path selects the in-process mock device (KTD7)
// so the whole stack runs bench- and CI-side without hardware.
package spid

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"time"

	serial "go.bug.st/serial"

	"spid-ercm-rotator-bridge/internal/config"
)

// Axis is the per-axis driver contract the mount dispatch core (U4) and the
// MQTT surface consume. It is deliberately narrow — set-target, stop, cached
// readback plus its validity, and the device-link liveness/error state — so
// the serial driver and the in-process mock satisfy it identically and U4 can
// adapt it onto its own Controller interface.
type Axis interface {
	// SetTarget commands the axis to a whole-degree azimuth. The Rot1Prog
	// expects no reply; the error reports a dead link, not device acceptance.
	SetTarget(az float64) error
	// Stop halts the axis.
	Stop() error
	// Readback returns the cached position and whether it is valid — false
	// until the first status reply after startup or a reopen (KTD7/KTD9: an
	// unknown readback never skips a write and position queries refuse).
	Readback() (az float64, valid bool)
	// Online reports the device link (serial port) liveness — the
	// device_online layer of the two-liveness rule, not the bridge status.
	Online() bool
	// Err returns the last link error, "" while healthy (feeds /state.error).
	Err() string
}

// PollRunner runs the readback poll until ctx is done. It is the driver's own
// goroutine to spawn: the poll loop owns the self-heal cadence.
type PollRunner interface {
	RunPoll(ctx context.Context)
}

// Opts is the driver cadence. Real deployments take these from the config
// ([control] poll_interval / reopen_cooldown); tests shrink them.
type Opts struct {
	// PollInterval is the readback poll period.
	PollInterval time.Duration
	// ReopenCooldown gates every reopen attempt after a fault: a flapping
	// USB adapter must not spin open/write/close in a tight loop.
	ReopenCooldown time.Duration
	// WritePace is the minimum spacing between any two writes (Rot1Prog
	// needs ≥300 ms, Appendix A / hamlib post_write_delay).
	WritePace time.Duration
	// ReadTimeout bounds the wait for a status reply after the poll writes
	// the request.
	ReadTimeout time.Duration
}

func (o Opts) withDefaults() Opts {
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.ReopenCooldown <= 0 {
		o.ReopenCooldown = 2 * time.Second
	}
	if o.WritePace <= 0 {
		o.WritePace = DefaultWritePace
	}
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = DefaultReadTimeout
	}
	return o
}

// readErr tags a transport read failure with the generation of the reader that
// produced it, so an error from a reader already replaced by a reopen does
// not tear down the freshly reopened link.
type readErr struct {
	gen int
	err error
}

// link guards the swappable port handle. The reader goroutine snapshots the
// handle for each Read; a concurrent reopen (swap) closes the old handle,
// which unblocks the old reader with an error — the stale-generation tag then
// discards it.
type link struct {
	mu sync.Mutex
	rw io.ReadWriteCloser
}

func (l *link) snapshot() io.ReadWriteCloser {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rw
}

// swap replaces the handle and closes the previous one best-effort (it is
// likely already gone — that is why we are swapping).
func (l *link) swap(rw io.ReadWriteCloser) {
	l.mu.Lock()
	defer l.mu.Unlock()
	old := l.rw
	l.rw = rw
	if old != nil {
		_ = old.Close()
	}
}

// Driver talks Rot1Prog to one azimuth controller over a byte-oriented port.
// All writes (goto, stop, status request) serialize on one mutex and respect
// the write pace, so concurrent callers can never interleave bytes on the
// wire.
type Driver struct {
	opener func() (io.ReadWriteCloser, error)
	opts   Opts
	log    *slog.Logger

	// mu guards the link, the reader generation, the cached readback and the
	// liveness/error state. It is held across the write-pace sleep (bounded
	// by WritePace) — that is what serializes writers with the poll loop.
	mu        sync.Mutex
	lnk       *link
	gen       int
	rxCh      chan []byte
	readErrCh chan readErr

	az        float64
	valid     bool
	online    bool
	errStr    string
	everUp    bool // a Warn on reopen is recovery news; the first open is Info
	lastWrite time.Time
	// lastFault is when the link was last observed going down (or when the
	// last open attempt failed); every reopen waits out the cooldown from it.
	lastFault time.Time
}

// NewDriver builds a driver over an opener closure. The opener re-resolves
// the stable /dev/serial/by-id/ path on every call so a USB re-enumeration
// heals instead of wedging on a deleted device node (KTD7, the ultrabridge
// model). A nil log silences the driver (tests).
func NewDriver(opener func() (io.ReadWriteCloser, error), opts Opts, log *slog.Logger) *Driver {
	return &Driver{
		opener:    opener,
		opts:      opts.withDefaults(),
		log:       logOrDiscard(log),
		lnk:       &link{},
		rxCh:      make(chan []byte, 8),
		readErrCh: make(chan readErr, 4),
	}
}

// New builds the azimuth Axis from the per-axis slot config: an empty serial
// port selects the in-process mock device (KTD7), a configured by-id port the
// serial driver. This is the constructor U5 wires; it refuses the el axis.
func New(slot config.SlotConfig, control config.ControlConfig, log *slog.Logger) (Axis, error) {
	if slot.Axis != config.AxisAZ {
		return nil, fmt.Errorf("spid: axis %q: this driver fronts the azimuth axis only", slot.Axis)
	}
	opts := Opts{
		PollInterval:   control.PollIntervalDur,
		ReopenCooldown: control.ReopenCooldownDur,
		WritePace:      DefaultWritePace,
		ReadTimeout:    DefaultReadTimeout,
	}
	if slot.Mock() {
		return NewMock(opts), nil
	}
	return NewDriver(serialOpener(slot.Serial.Port, slot.Serial.Baud), opts, log), nil
}

// --- Axis contract ------------------------------------------------------------

func (d *Driver) SetTarget(az float64) error {
	if math.IsNaN(az) || math.IsInf(az, 0) {
		return fmt.Errorf("azimuth %v is not finite", az)
	}
	return d.writeFrame(encodeSet(az))
}

func (d *Driver) Stop() error { return d.writeFrame(encodeStop()) }

func (d *Driver) Readback() (float64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.az, d.valid
}

func (d *Driver) Online() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.online
}

func (d *Driver) Err() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.errStr
}

// --- poll + self-heal loop ------------------------------------------------------

// RunPoll opens the port and then, on every tick, writes one status request
// and consumes the reply — the bridge owns the poll cadence (Appendix A).
// It never gives up: a link that is down is retried every ReopenCooldown,
// indefinitely (KTD7). Returns when ctx is done, after closing the port.
func (d *Driver) RunPoll(ctx context.Context) {
	d.mu.Lock()
	if d.lnk.snapshot() == nil {
		// Initial open. A controller absent at boot must not wedge anything:
		// the loop retries via writeFrame's reopen path.
		_ = d.reopenLocked()
	}
	d.mu.Unlock()

	ticker := time.NewTicker(d.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.lnk.swap(nil) // close the port; the reader dies on the stale handle
			return
		case re := <-d.readErrCh:
			d.onReadErr(re)
		case <-ticker.C:
			d.pollOnce(ctx)
		}
	}
}

// pollOnce writes the status request and consumes one reply.
func (d *Driver) pollOnce(ctx context.Context) {
	d.mu.Lock()
	err := d.writeFrameLocked(encodeStatus())
	d.mu.Unlock()
	if err != nil {
		return // writeFrame already marked the link down and logged
	}

	select {
	case frame := <-d.rxCh:
		az, err := decodeStatusReply(frame)
		if err != nil {
			// A malformed reply proves the link but must never be misread as
			// a position — ASCII digits in a reply are the Rot1Prog desync
			// trap and are rejected by the codec, not interpreted.
			d.log.Warn("malformed status reply", "err", err, "frame", fmt.Sprintf("% X", frame))
			return
		}
		d.mu.Lock()
		d.az = az
		d.valid = true // KTD9: the first status reply clears the unknown flag
		d.online = true
		d.errStr = ""
		d.mu.Unlock()
	case re := <-d.readErrCh:
		d.onReadErr(re)
	case <-time.After(d.opts.ReadTimeout):
		// No reply this tick: the controller may just be slow or busy. Keep
		// the last cached state; port-level faults arrive via readErrCh.
	case <-ctx.Done():
	}
}

// onReadErr handles a transport read failure: mark the link down (clearing the
// cached readback validity — KTD7) and let the next tick's write path heal.
// Errors from a stale reader generation are dropped here.
func (d *Driver) onReadErr(re readErr) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if re.gen != d.gen {
		return // stale reader; a fresh generation already owns the link
	}
	d.markDownLocked(re.err)
}

// --- writes ---------------------------------------------------------------------

// writeFrame puts one command packet on the wire, paced and self-healing.
func (d *Driver) writeFrame(frame []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeFrameLocked(frame)
}

// writeFrameLocked requires d.mu held. The lock is held across the pace sleep
// so concurrent writers and the poll loop serialize — a torn frame on a
// Rot1Prog is indistinguishable from a position command.
func (d *Driver) writeFrameLocked(frame []byte) error {
	if d.lnk.snapshot() == nil || !d.online {
		// Link down (never opened, or after a fault): heal on the caller's
		// behalf, cooldown-gated so a flapping adapter cannot spin
		// open/write/close (KTD7: indefinite, one attempt per window).
		if !d.reopenDueLocked() {
			return fmt.Errorf("serial link down: %s", d.errStr)
		}
		if err := d.reopenLocked(); err != nil {
			return err
		}
	}

	// Rot1Prog write pacing (Appendix A): ≥300 ms between any two writes.
	if !d.lastWrite.IsZero() {
		if wait := d.opts.WritePace - time.Since(d.lastWrite); wait > 0 {
			time.Sleep(wait)
		}
	}

	if _, err := d.lnk.snapshot().Write(frame); err != nil {
		d.markDownLocked(err)
		if !d.reopenDueLocked() {
			return fmt.Errorf("write: %w", err)
		}
		// One reopen+retry per call (the ultrabridge model): the stale handle
		// is replaced and the frame re-sent once; a persistently broken link
		// surfaces the error and the next poll tick retries.
		if rErr := d.reopenLocked(); rErr != nil {
			return fmt.Errorf("write: %w (reopen: %v)", err, rErr)
		}
		d.log.Warn("serial write fault, port reopened", "err", err)
		if _, err = d.lnk.snapshot().Write(frame); err != nil {
			d.markDownLocked(err)
			return fmt.Errorf("write after reopen: %w", err)
		}
	}
	d.lastWrite = time.Now()
	return nil
}

// --- link state -------------------------------------------------------------------

// markDownLocked takes the link down and invalidates the cached readback:
// after a Down transition the deadband must never trust a pre-outage position
// (KTD7). Only the healthy→down transition stamps lastFault and Warns — the
// repeated write failures of a persistently dead link are symptoms, not new
// events, and must not push the reopen cooldown out forever.
func (d *Driver) markDownLocked(err error) {
	wasDown := !d.online
	d.online = false
	d.valid = false
	d.errStr = err.Error()
	if !wasDown {
		d.lastFault = time.Now()
		d.log.Warn("serial link down", "err", err)
	}
}

// reopenDueLocked reports whether the cooldown since the last fault has
// elapsed. The zero lastFault (never faulted) is always due — the first open
// is immediate.
func (d *Driver) reopenDueLocked() bool {
	return time.Since(d.lastFault) >= d.opts.ReopenCooldown
}

// reopenLocked replaces the port handle: it closes the stale one (which also
// unblocks the old reader — its late error is discarded by generation tag),
// calls the opener (which re-resolves the by-id path), bumps the reader
// generation, and re-initializes the axis state (KTD7). Reopening must be
// preceded by reopenDueLocked; a failed attempt stamps lastFault so the next
// retry waits out the cooldown — that is the indefinite, paced retry.
func (d *Driver) reopenLocked() error {
	d.gen++
	gen := d.gen
	d.lnk.swap(nil)
	rw, err := d.opener()
	if err != nil {
		d.online = false
		d.valid = false
		d.errStr = "open serial: " + err.Error()
		d.lastFault = time.Now()
		return err
	}
	d.lnk.swap(rw)
	d.online = true
	// Re-init after reopen (KTD7): Rot1Prog has no init command sequence —
	// the status request on the next tick IS the re-init, and until its
	// reply the readback is unknown, so the deadband always writes.
	d.valid = false
	d.errStr = ""
	// Drop any frames the dead link staged before the fault: a stale
	// pre-outage reply must never be consumed as a fresh readback.
	for {
		select {
		case <-d.rxCh:
			continue
		default:
		}
		break
	}
	d.startReader(gen)
	if d.everUp {
		d.log.Warn("serial port reopened after fault", "gen", gen)
	} else {
		d.everUp = true
		d.log.Info("serial port opened", "gen", gen)
	}
	return nil
}

// startReader spawns the reader generation gen: it assembles 0x57…0x20 frames
// from the raw byte stream (resync-scanning, never fixed offsets) and reports
// any read error tagged with its generation.
func (d *Driver) startReader(gen int) {
	go func() {
		buf := make([]byte, 64)
		var pending []byte
		for {
			rw := d.lnk.snapshot()
			if rw == nil {
				return // link torn down (shutdown); late errors are moot
			}
			n, err := rw.Read(buf)
			if n > 0 {
				pending = append(pending, buf[:n]...)
				pending = d.emitFrames(pending)
			}
			if err != nil {
				select {
				case d.readErrCh <- readErr{gen: gen, err: err}:
				default: // shutdown: nobody is reading anymore
				}
				return
			}
		}
	}()
}

// emitFrames strips whole 0x57…END frames off the head of pending and pushes
// them to rxCh (the poll loop is the only consumer; overflow drops rather
// than blocks the reader). Returns the remainder.
func (d *Driver) emitFrames(pending []byte) []byte {
	for len(pending) >= replyLen {
		// Resync: discard noise before the start byte.
		i := 0
		for i < len(pending) && pending[i] != startByte {
			i++
		}
		pending = pending[i:]
		if len(pending) < replyLen {
			break
		}
		if pending[replyLen-1] != endByte {
			pending = pending[1:] // not a frame after all; slide one byte
			continue
		}
		frame := make([]byte, replyLen)
		copy(frame, pending[:replyLen])
		pending = pending[replyLen:]
		select {
		case d.rxCh <- frame:
		default:
		}
	}
	// Bound the buffer against an endless stream with no END byte.
	if len(pending) > 64 {
		pending = pending[len(pending)-64:]
	}
	return pending
}

// --- serial port opener --------------------------------------------------------

// serialOpener returns the opener closure the driver self-heals through: it
// re-resolves the stable /dev/serial/by-id/ symlink on every call, so a USB
// re-enumeration heals instead of wedging on a deleted device node.
func serialOpener(path string, baud int) func() (io.ReadWriteCloser, error) {
	return func() (io.ReadWriteCloser, error) {
		p, err := serial.Open(path, &serial.Mode{BaudRate: baud})
		if err != nil {
			return nil, fmt.Errorf("open serial %s @ %d baud: %w", path, baud, err)
		}
		return p, nil
	}
}

func logOrDiscard(log *slog.Logger) *slog.Logger {
	if log != nil {
		return log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
