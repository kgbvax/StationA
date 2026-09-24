// SPDX-License-Identifier: AGPL-3.0-or-later

// Package spid implements the azimuth rotator driver for a SPID controller
// speaking the Rot1Prog binary protocol (plan KTD5, Appendix A): 13-byte
// command packets, a 5-byte raw-digit status reply, a poll loop on the
// configured tick, writes paced ≥300 ms.
//
// COMMUNICATION MODEL (2026-09-24 rework, ultracode-reviewed): ONE goroutine
// — the RunPoll loop — owns ALL port I/O. SetTarget/Stop never touch the
// wire; they enqueue a command and the owner performs it between exchanges,
// reporting the write result back to the caller. The per-generation reader
// goroutine decodes the RX byte stream into frames and hands them to the
// owner, which is the single consumer. Consequences, each traced to a live
// 2026-09-23 incident or a review-confirmed defect:
//
//   - request/reply pairing is structural (wire order = owner's write order),
//     not a timing heuristic — two writer kinds feeding one untagged FIFO
//     could pair a stop reply with the wrong tick;
//   - the all-zero stop/ack reply is attributed by an OUTSTANDING-COMMAND
//     counter, not frame shape alone: a genuine status reply whose wrapped
//     register rests exactly on count 0 decodes as az 0 instead of freezing
//     the readback behind an ever-reset watchdog;
//   - frames carry their reader generation and stale ones are discarded, so
//     a pre-outage reply can never be committed after a reopen;
//   - there is no ioMu dance: only the owner writes, so a write can only
//     stall the owner — and the write watchdog closes a stalled handle
//     through the link, feeding the same self-heal path without a blind
//     resend racing its own completion.
//
// The driver self-heals indefinitely (KTD7): every read/write fault takes the
// axis down (clearing the cached-readback validity so the U4 deadband can
// never silently no-op against a pre-outage position), and the owner
// re-resolves the stable /dev/serial/by-id/ path after the configured
// cooldown, forever. After every successful reopen the readback starts
// unknown again until the first fresh status reply — Rot1Prog has no init
// command sequence, so the status request on the next tick IS the re-init.
//
// A controller that stays SILENT is also a dead link: three consecutive poll
// read timeouts take the axis down (the internal/ercm timeout contract — a
// powered-off rotor behind a live USB adapter must not keep a frozen but
// valid readback online forever).
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
	// RunPoll runs the readback poll until ctx is done; the poll loop IS the
	// I/O owner (see the package comment), so the consumer spawns it on its
	// own goroutine. SetTarget/Stop dispatch into it and block until the
	// owner reports the write outcome — bounded by dispatchBound.
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
	// WriteTimeout bounds one port write. go.bug.st/serial exposes no write
	// deadline (its unix Write is a plain blocking write), so a write stalled
	// on a wedged fd is closed out by a watchdog after this long and feeds
	// the reopen/self-heal path. Tests shrink it; real deployments take the
	// 3 s default.
	WriteTimeout time.Duration
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
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = DefaultWriteTimeout
	}
	return o
}

// maxConsecutiveTimeouts is the poll read-timeout bound: a controller that
// stays silent for this many consecutive ticks is a dead device behind a live
// adapter (powered-off rotor), not a busy one, and the link must go down —
// the internal/ercm exchange-timeout contract applied to the poll loop.
const maxConsecutiveTimeouts = 3

// dispatchBound bounds one SetTarget/Stop round-trip through the owner. The
// old per-write watchdog bound, kept: a caller must never block forever on a
// wedged or already-dead owner.
const dispatchBound = 3 * time.Second

// command is one queued wire write: SetTarget/Stop hand it to the owner and
// wait for the write outcome on reply (buffered, so the owner never blocks
// delivering to a caller that timed out and walked away).
type command struct {
	frame []byte
	reply chan error
}

// readerEvent is one frame (or the terminal read error) from a reader
// generation. The generation lets the owner discard everything a replaced
// reader still had in flight — structurally, not by timing.
type readerEvent struct {
	gen   int
	frame []byte
	err   error
}

// link guards the swappable port handle. The owner swaps it on reopen and
// teardown; the swap Closes the old handle, which unblocks that generation's
// reader (its Read fails, it reports once, and the stale generation is
// discarded by the owner).
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
// likely already gone — that is why we are swapping). The Close runs OUTSIDE
// link.mu: go.bug.st's Close waits for the port's readers to drain, and a
// reader parked on a silent link must not hold every future snapshot/swap
// hostage while it drains.
func (l *link) swap(rw io.ReadWriteCloser) {
	l.mu.Lock()
	old := l.rw
	l.rw = rw
	l.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// Driver talks Rot1Prog to one azimuth controller over a byte-oriented port.
// One goroutine (RunPoll) owns every byte on the wire; everyone else talks to
// that owner through channels or reads the cached state under mu.
type Driver struct {
	opener func() (io.ReadWriteCloser, error)
	opts   Opts
	log    *slog.Logger

	// Owner-wired channels: cmdCh carries queued wire writes into the owner
	// (SetTarget/Stop dispatch), frameCh carries decoded frames and terminal
	// read errors out of the reader generations. Both are small on purpose:
	// a full cmdCh means the owner is wedged and callers must hear that, and
	// frameCh overflow drops the OLDEST event so the newest wire state wins.
	cmdCh   chan command
	frameCh chan readerEvent

	// mu guards ONLY the cached surfaces (readback, liveness, error string) —
	// the owner writes them, anyone reads them, and it is never held across
	// port I/O, so Online()/Readback() stay answerable even while the owner
	// is wedged on a dead fd.
	mu     sync.Mutex
	az     float64
	valid  bool
	online bool
	errStr string

	// Everything below is OWNER-PRIVATE: only the RunPoll goroutine touches
	// it, so none of it needs a lock. This is the point of the model —
	// sequencing lives in one place.
	lnk         *link     // current port handle (nil = down)
	gen         int       // reader generation; frames/events carry it
	outstanding int       // commands written whose (zero) reply is still expected
	timeouts    int       // consecutive silent polls
	lastWrite   time.Time // last successful write, for the pace gate
	lastFault   time.Time // last down transition or failed open, for the cooldown
	everUp      bool      // a Warn on reopen is recovery news; the first open is Info
}

// NewDriver builds the driver over an opener closure. The opener re-resolves
// the stable /dev/serial/by-id/ path on every call so a USB re-enumeration
// heals instead of wedging on a deleted device node (KTD7, the ultrabridge
// model). A nil log silences the driver (tests).
func NewDriver(opener func() (io.ReadWriteCloser, error), opts Opts, log *slog.Logger) *Driver {
	return &Driver{
		opener:  opener,
		opts:    opts.withDefaults(),
		log:     logOrDiscard(log),
		lnk:     &link{},
		cmdCh:   make(chan command, 4),
		frameCh: make(chan readerEvent, 4),
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
	return NewDriver(serialOpener(slot.Serial.Port, slot.Serial.Baud, opts.ReadTimeout), opts, log), nil
}

// --- Axis contract ------------------------------------------------------------

// SetTarget enqueues one set frame; the owner writes it and reports the
// outcome. The error reports the LINK, not device acceptance — Rot1Prog set
// commands are documented silent (rot2proG spec).
func (d *Driver) SetTarget(az float64) error {
	if math.IsNaN(az) || math.IsInf(az, 0) {
		return fmt.Errorf("azimuth %v is not finite", az)
	}
	return d.dispatch(encodeSet(az))
}

// Stop enqueues the stop frame; see SetTarget for the dispatch semantics.
func (d *Driver) Stop() error { return d.dispatch(encodeStop()) }

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

// dispatch hands one frame to the owner and waits, bounded, for the write
// outcome. A full queue means the owner is wedged or already gone — the
// caller hears that immediately instead of blocking forever.
func (d *Driver) dispatch(frame []byte) error {
	c := command{frame: frame, reply: make(chan error, 1)}
	select {
	case d.cmdCh <- c:
	default:
		return fmt.Errorf("command queue full — link owner wedged or stopped")
	}
	select {
	case err := <-c.reply:
		return err
	case <-time.After(dispatchBound):
		return fmt.Errorf("command dispatch timed out after %s", dispatchBound)
	}
}

// --- the owner loop -----------------------------------------------------------

// RunPoll is the I/O owner: it opens the port, then services queued commands,
// reader events and the poll tick, in that order, forever. Every byte on the
// wire is written here; every frame is consumed here. Returns when ctx is
// done, after closing the port.
func (d *Driver) RunPoll(ctx context.Context) {
	// Initial open. A controller absent at boot must not wedge anything: the
	// loop retries via the write/poll heal path.
	_ = d.ensureLink()

	ticker := time.NewTicker(d.opts.PollInterval)
	defer ticker.Stop()

	for {
		// Commands first, bounded: a queue that built up behind a poll
		// exchange drains before the next wait. Beyond the bound, the rest
		// waits for the next iteration — the wire pace gates them anyway.
		for i := 0; i < len(d.cmdCh); i++ {
			d.execCommand(<-d.cmdCh)
		}

		select {
		case <-ctx.Done():
			d.shutdown()
			return
		case c := <-d.cmdCh:
			d.execCommand(c)
		case ev := <-d.frameCh:
			d.handleFrameEvent(ev)
		case <-ticker.C:
			d.pollExchange(ctx)
		}
	}
}

// execCommand performs one queued wire write and reports the outcome. Set
// and stop frames expect an (all-zero) reply; crediting the outstanding
// counter is what makes the next all-zero frame attributable structurally.
func (d *Driver) execCommand(c command) {
	err := d.writeWithHeal(c.frame)
	if err == nil && c.frame[11] != kStatus {
		d.outstanding++
	}
	c.reply <- err
}

// pollExchange writes one status request and consumes replies until the
// status reply arrives. Stale-generation events are discarded; all-zero
// frames consume outstanding-command credits (a live status of register
// count exactly 0 decodes as az 0 — finding 5 of the 2026-09-24 review).
func (d *Driver) pollExchange(ctx context.Context) {
	if err := d.writeWithHeal(encodeStatus()); err != nil {
		return // writeWithHeal already marked the link down and logged
	}
	for {
		select {
		case ev := <-d.frameCh:
			if ev.gen != d.gen {
				continue // a replaced reader's leftovers: never commit
			}
			if ev.err != nil {
				d.handleReaderErr(ev)
				return
			}
			if isCommandAck(ev.frame) && d.outstanding > 0 {
				d.outstanding--
				d.noteLive()
				continue
			}
			az, err := decodeStatusReply(ev.frame)
			if err != nil {
				// A malformed reply proves the link but must never be misread
				// as a position — ASCII digits in a reply are the Rot1Prog
				// desync trap and are rejected by the codec, not interpreted.
				d.log.Warn("malformed status reply", "err", err, "frame", fmt.Sprintf("% X", ev.frame))
				d.noteLive()
				return
			}
			d.commitPosition(az)
			return
		case <-time.After(d.opts.ReadTimeout):
			// No reply this tick: the controller may just be slow or busy —
			// one missed tick is not news. But a controller that stays
			// silent for maxConsecutiveTimeouts ticks is a DEAD device
			// behind a live adapter (powered-off rotor), and the cached
			// state must not stay frozen-online forever.
			d.timeoutStrike()
			return
		case <-ctx.Done():
			return
		}
	}
}

// handleFrameEvent consumes a reader event OUTSIDE an exchange: a stop reply
// that landed after its command already returned, or a terminal read error.
// Same attribution rules as the exchange loop — one code path per rule.
func (d *Driver) handleFrameEvent(ev readerEvent) {
	if ev.gen != d.gen {
		return // a replaced reader's leftovers
	}
	if ev.err != nil {
		d.handleReaderErr(ev)
		return
	}
	if isCommandAck(ev.frame) && d.outstanding > 0 {
		d.outstanding--
		d.noteLive()
		return
	}
	az, err := decodeStatusReply(ev.frame)
	if err != nil {
		d.log.Warn("malformed status reply", "err", err, "frame", fmt.Sprintf("% X", ev.frame))
		d.noteLive()
		return
	}
	// A fresh position outside an exchange is still a fresh position.
	d.commitPosition(az)
}

// timeoutStrike counts one silent poll and takes the link down when the
// consecutive-silence bound trips. Owner-only.
func (d *Driver) timeoutStrike() (tripped bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.timeouts++
	if d.timeouts < maxConsecutiveTimeouts {
		return false
	}
	d.markDownLocked(fmt.Errorf(
		"no status reply after %d consecutive polls (read timeout %s each)",
		maxConsecutiveTimeouts, d.opts.ReadTimeout))
	return true
}

// handleReaderErr takes the link down for a CURRENT-generation transport
// fault; errors from replaced readers are dropped (their handle is already
// closed and a fresh generation owns the link).
func (d *Driver) handleReaderErr(ev readerEvent) {
	if ev.gen != d.gen {
		return
	}
	d.mu.Lock()
	d.markDownLocked(ev.err)
	d.mu.Unlock()
}

// noteLive records any frame as proof of a live device, however empty or
// malformed, restarting the consecutive-silence count.
func (d *Driver) noteLive() {
	d.mu.Lock()
	d.timeouts = 0
	d.mu.Unlock()
}

// commitPosition caches one decoded status reply (owner-only write).
func (d *Driver) commitPosition(az float64) {
	d.mu.Lock()
	d.az = az
	d.valid = true // KTD9: the first status reply clears the unknown flag
	d.online = true
	d.errStr = ""
	d.timeouts = 0
	d.mu.Unlock()
}

// shutdown closes the port on ctx done and reports failure to every command
// still queued — no caller may hang on an owner that is going away.
func (d *Driver) shutdown() {
	for {
		select {
		case c := <-d.cmdCh:
			c.reply <- fmt.Errorf("link owner stopped")
		default:
			d.lnk.swap(nil) // close the handle; the reader dies on the stale handle
			return
		}
	}
}

// --- writes ---------------------------------------------------------------------

// writeWithHeal performs one paced, watchdog-bounded write. On a write fault
// it takes the link down and re-sends ONCE on a freshly reopened handle —
// the ultrabridge model: never retry onto the same wedged fd, never spin.
func (d *Driver) writeWithHeal(frame []byte) error {
	if err := d.ensureLink(); err != nil {
		return err
	}
	if err := d.pacedWrite(frame); err != nil {
		d.markDown(err)
		if !d.reopenDue() {
			return fmt.Errorf("write: %w", err)
		}
		// One reopen+retry per call: the stale handle is replaced and the
		// frame re-sent once; a persistently broken link surfaces the error
		// and the next poll tick retries.
		if rErr := d.reopen(); rErr != nil {
			return fmt.Errorf("write: %w (reopen: %v)", err, rErr)
		}
		d.log.Warn("serial write fault, port reopened", "err", err)
		if err := d.pacedWrite(frame); err != nil {
			d.markDown(err)
			return fmt.Errorf("write after reopen: %w", err)
		}
	}
	d.lastWrite = time.Now()
	return nil
}

// pacedWrite gates the write pace (Rot1Prog ≥300 ms between any two writes)
// and hands the frame to the watchdog. Owner-only: no lock needed for
// lastWrite — the sleep merely delays this one goroutine, and the cached
// state stays answerable throughout.
func (d *Driver) pacedWrite(frame []byte) error {
	if !d.lastWrite.IsZero() {
		if w := d.opts.WritePace - time.Since(d.lastWrite); w > 0 {
			time.Sleep(w)
		}
	}
	rw := d.lnk.snapshot()
	if rw == nil {
		return io.ErrClosedPipe
	}
	if err := d.watchdogWrite(rw, frame); err != nil {
		return err
	}
	return nil
}

// watchdogWrite writes one frame through the current port handle, bounded by
// Opts.WriteTimeout. go.bug.st/serial exposes no write deadline (its unix
// Write is a plain blocking write), so a wedged tty fd would park here
// forever stalling the whole owner; instead the watchdog closes the handle on
// stall — closing the fd converts the parked Write into an error, which
// feeds the markDown/reopen self-heal path. The double select prefers a
// completion that raced the timer: only a genuinely stalled write is closed
// out, never one that already landed.
func (d *Driver) watchdogWrite(rw io.ReadWriteCloser, frame []byte) error {
	done := make(chan error, 1) // buffered: a late parked Write never leaks its goroutine
	go func() {
		_, err := rw.Write(frame)
		done <- err
	}()
	timer := time.NewTimer(d.opts.WriteTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		select {
		case err := <-done:
			return err
		default:
		}
		_ = rw.Close()
		return fmt.Errorf("write stalled for %s — port handle closed by watchdog", d.opts.WriteTimeout)
	}
}

// --- link state -------------------------------------------------------------------

// ensureLink opens or reopens the port when it is down — cooldown-gated, so
// a flapping adapter cannot spin open/write/close in a tight loop (KTD7:
// indefinite, one attempt per window). Owner-only.
func (d *Driver) ensureLink() error {
	d.mu.Lock()
	down := d.lnk.snapshot() == nil || !d.online
	errStr := d.errStr
	d.mu.Unlock()
	if !down {
		return nil
	}
	if !d.reopenDue() {
		return fmt.Errorf("serial link down: %s", errStr)
	}
	return d.reopen()
}

// markDown takes the link down and invalidates the cached readback: after a
// Down transition the deadband must never trust a pre-outage position (KTD7).
func (d *Driver) markDown(err error) {
	d.mu.Lock()
	d.markDownLocked(err)
	d.mu.Unlock()
}

// markDownLocked is markDown with mu held (used inside exchange critical
// sections that already hold the lock). Only the healthy→down transition
// stamps lastFault and Warns — the repeated failures of a persistently dead
// link are symptoms, not new events, and must not push the reopen cooldown
// out forever.
func (d *Driver) markDownLocked(err error) {
	wasDown := !d.online
	d.online = false
	d.valid = false
	d.errStr = err.Error()
	d.timeouts = 0 // any link transition restarts the consecutive-timeout count
	if !wasDown {
		d.lastFault = time.Now()
		d.log.Warn("serial link down", "err", err)
	}
}

// reopenDue reports whether the cooldown since the last fault has elapsed.
// The zero lastFault (never faulted) is always due — the first open is
// immediate. Owner-only.
func (d *Driver) reopenDue() bool {
	return time.Since(d.lastFault) >= d.opts.ReopenCooldown
}

// reopen replaces the port handle: it closes the stale one (which unblocks
// the old reader — its terminal event arrives stale-tagged and is dropped),
// calls the opener (which re-resolves the by-id path), bumps the reader
// generation, drops outstanding-command credits (their replies died with the
// old handle), and re-initializes the axis state (KTD7). Reopening must be
// preceded by reopenDue; a failed attempt stamps lastFault so the next retry
// waits out the cooldown — that is the indefinite, paced retry.
//
// Owner-only: the port lifecycle is serialized by the model itself. mu is
// taken only around cached-state transitions, so even a slow device-node
// open cannot freeze Online()/Readback().
func (d *Driver) reopen() error {
	d.gen++
	gen := d.gen
	// swap (which Closes the stale handle) runs OUTSIDE mu: Close waits for
	// the port's readers to drain, and a reader parked on a silent link must
	// never stall the cached surfaces — the observed az-slot freeze of
	// 2026-09-17 is exactly that shape.
	d.lnk.swap(nil)
	rw, err := d.opener()
	if err != nil {
		d.mu.Lock()
		d.online = false
		d.valid = false
		d.errStr = "open serial: " + err.Error()
		d.lastFault = time.Now()
		d.mu.Unlock()
		return err
	}
	d.lnk.swap(rw)
	d.mu.Lock()
	d.online = true
	// Re-init after reopen (KTD7): Rot1Prog has no init command sequence —
	// the status request on the next tick IS the re-init, and until its
	// reply the readback is unknown, so the deadband always writes.
	d.valid = false
	d.errStr = ""
	d.timeouts = 0
	d.mu.Unlock()
	d.outstanding = 0 // the old handle took its un-ACKed commands with it
	go d.readFrames(gen, rw)
	wasUp := d.everUp
	d.everUp = true
	if wasUp {
		d.log.Warn("serial port reopened after fault", "gen", gen)
	} else {
		d.log.Info("serial port opened", "gen", gen)
	}
	return nil
}

// --- the reader -----------------------------------------------------------------

// readFrames is one reader generation: it owns the handle it was started
// with — it NEVER re-snapshots the link, so a reopen can only kill it (the
// swap closed its handle), never let it read the new handle alongside the
// fresh reader. Frames and the terminal error are handed to the owner
// generation-tagged; the owner drops whatever does not match.
func (d *Driver) readFrames(gen int, rw io.ReadWriteCloser) {
	buf := make([]byte, 64)
	var pending []byte
	for {
		n, err := rw.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			pending = d.emitFrames(gen, pending)
		}
		if err != nil {
			d.sendEvent(readerEvent{gen: gen, err: err})
			return
		}
	}
}

// emitFrames strips whole 0x57…END frames off the head of pending and hands
// them to the owner (the single consumer). Returns the remainder.
func (d *Driver) emitFrames(gen int, pending []byte) []byte {
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
		d.sendEvent(readerEvent{gen: gen, frame: frame})
	}
	// Bound the buffer against an endless stream with no END byte.
	if len(pending) > 64 {
		pending = pending[len(pending)-64:]
	}
	return pending
}

// sendEvent hands one event to the owner, dropping the OLDEST queued event
// on overflow: the newest wire state must win, and a full channel means the
// owner is stalled — its next exchange will drain and re-sync by generation.
func (d *Driver) sendEvent(ev readerEvent) {
	select {
	case d.frameCh <- ev:
		return
	default:
	}
	select {
	case <-d.frameCh:
	default:
	}
	select {
	case d.frameCh <- ev:
	default:
	}
}

// --- serial port opener --------------------------------------------------------

// serialOpener returns the opener closure the driver self-heals through: it
// re-resolves the stable /dev/serial/by-id/ symlink on every call, so a USB
// re-enumeration heals instead of wedging on a deleted device node. The port
// read timeout is BOUNDED: without it a reader parks in a blocking Read on a
// silent link, and the library's Close (which waits for readers to drain)
// then hangs every reopen — the 2026-09-17 az-slot freeze.
func serialOpener(path string, baud int, readTimeout time.Duration) func() (io.ReadWriteCloser, error) {
	if readTimeout <= 0 {
		readTimeout = DefaultReadTimeout
	}
	return func() (io.ReadWriteCloser, error) {
		p, err := serial.Open(path, &serial.Mode{BaudRate: baud})
		if err != nil {
			return nil, fmt.Errorf("open serial %s @ %d baud: %w", path, baud, err)
		}
		if err := p.SetReadTimeout(readTimeout); err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("open serial %s: set read timeout: %w", path, err)
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
