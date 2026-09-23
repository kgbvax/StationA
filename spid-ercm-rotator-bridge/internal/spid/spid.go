// SPDX-License-Identifier: AGPL-3.0-or-later

// Package spid implements the azimuth rotator driver for a SPID controller
// speaking the Rot1Prog binary protocol (plan KTD5, Appendix A): 13-byte
// command packets, a 5-byte raw-digit status reply, set commands with no
// reply, a poll loop on the configured tick, writes paced ≥300 ms.
//
// COMMUNICATION MODEL (single owner, re-architected 2026-09-23 after the
// ultracode review of the live incidents): ONE goroutine — the RunPoll owner
// — performs every byte of port I/O, as synchronous write→read EXCHANGES.
// Commands (set/stop) arrive from other goroutines on a channel and are
// written by the owner between polls; each exchange owns its reply by
// structure (half-duplex FIFO: the first frame after a request IS its
// reply), so there is no reply-to-request pairing heuristic, no shared
// frame channel, no reader goroutine, and no generation tagging — the
// entire class of "stale frame resurfaces after a reopen" and "ACK misread
// as a position" defects (both observed live 2026-09-23) is unrepresentable.
// The one content heuristic left is deliberate and command-accounted: the
// spec makes SET silent, but flood hardware was observed answering with the
// all-zero frame, so the owner remembers it wrote a set and lets the next
// status exchange swallow one all-zero frame instead of caching it as az 0.
//
// The driver self-heals indefinitely (KTD7): every fault takes the axis
// down (clearing the cached-readback validity so the U4 deadband can never
// silently no-op against a pre-outage position), and the owner re-resolves
// the stable /dev/serial/by-id/ path after the configured cooldown, forever.
// After every reopen the readback starts unknown until the first fresh
// status reply — Rot1Prog has no init sequence, so the next tick's status
// request IS the re-init.
//
// A controller that stays SILENT is also a dead link: three consecutive
// status exchanges with no reply take the axis down (the internal/ercm
// timeout contract — a powered-off rotor behind a live USB adapter must not
// keep a frozen but valid readback online forever). Only a decoded status
// reply resets the silence count; a command ACK proves nothing about the
// status path (a 1 Hz stop flood ACKed for hours on 2026-09-23 while the
// status path was dead). Writes are watchdog-bounded because go.bug.st/serial
// exposes no write deadline: the owner arms a timer per write; a write
// stalled on a wedged fd has its handle closed out from under it, which
// converts the parked Write into an error and feeds the same self-heal path.
// The single owner makes the watchdog trivially safe — it can only ever
// close the one handle the owner is writing through, and a late timer is
// neutralized by the exchange sequence number. The cached liveness state
// locks separately, so Online/Readback stay answerable while the owner is
// wedged or exchanging.
//
// An empty configured serial path selects the in-process mock device (KTD7)
// so the whole stack runs bench- and CI-side without hardware.
package spid

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
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
	// RunPoll runs the owner loop until ctx is done; it owns every byte of
	// port I/O and the self-heal cadence, so the consumer spawns it on its
	// own goroutine.
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
	// ReadTimeout bounds the wait for a reply inside one exchange.
	ReadTimeout time.Duration
	// WriteTimeout bounds one port write. go.bug.st/serial exposes no write
	// deadline (its unix Write is a plain blocking write), so a write stalled
	// on a wedged fd is closed out by the owner's watchdog after this long
	// and feeds the reopen/self-heal path. Tests shrink it; real deployments
	// take the 3 s default.
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

// maxConsecutiveTimeouts is the status-exchange silence bound: a controller
// that answers nothing for this many consecutive polls is a dead device
// behind a live adapter (powered-off rotor), not a busy one, and the link
// must go down — the internal/ercm exchange-timeout contract applied to the
// owner loop.
const maxConsecutiveTimeouts = 3

// readTimeoutSetter is the optional port capability the owner configures
// after open: bounded reads (real serial ports and the test fakes both
// honor it; a read then returns (0, nil) once the timeout lapses).
type readTimeoutSetter interface {
	SetReadTimeout(time.Duration) error
}

// replyKind tells the exchange how to treat a command's reply phase.
type replyKind int

const (
	// noReply: SET is documented silent — write only. If flood firmware
	// answers it anyway, the all-zero frame is swallowed by the next status
	// exchange via ackPending (command accounting, not content guessing).
	noReply replyKind = iota
	// ackReply: STOP answers — consume one reply frame, whatever it carries
	// (the spec's "approximate stopped position"; zeros in ROT1 captures).
	ackReply
	// wantStatus: the exchange is a status poll; the reply is the position.
	wantStatus
)

// command is one queued set/stop with its synchronous result. The caller
// blocks on ack until the owner has put the frame on the wire (or failed
// to) — the Axis contract reports write errors to the mount, which turns
// them into refusals rather than retries.
type command struct {
	frame []byte
	kind  replyKind
	ack   chan error
}

// linkState is the owner's private wire state: the current handle, the
// partial-frame buffer, and whether a SET went out whose (spec-silent,
// flood-observed) zero reply may still arrive. Owner-goroutine-only.
type linkState struct {
	rw         io.ReadWriteCloser
	pending    []byte
	ackPending bool
}

// Driver talks Rot1Prog to one azimuth controller over a byte-oriented port.
// Exactly one goroutine — RunPoll — ever touches the port; everyone else
// talks to it through cmdCh and the mutex-published cached state.
type Driver struct {
	opener func() (io.ReadWriteCloser, error)
	opts   Opts
	log    *slog.Logger

	cmdCh chan command
	done  chan struct{} // closed when the owner loop has exited

	// writeSeq increments on every owner exchange; the write watchdog
	// captures it and only closes the port if the exchange it armed for is
	// still the current one (a late timer must never fault a healthy,
	// already-completed write — the blind-resend race of the old design).
	writeSeq atomic.Int64

	// paceMu guards lastWriteAt. Only the owner writes today; the mutex
	// keeps pace() correct even if a second write path ever appears.
	paceMu      sync.Mutex
	lastWriteAt time.Time

	// strikes is the consecutive-silent-status-exchange count (owner-only).
	strikes int

	// stateMu guards ONLY the published cache below; the owner takes it for
	// the brief transitions, never across port I/O, so Readback/Online/Err
	// stay answerable no matter what the owner is doing.
	stateMu sync.Mutex
	az      float64
	valid   bool
	online  bool
	errStr  string
}

func NewDriver(opener func() (io.ReadWriteCloser, error), opts Opts, log *slog.Logger) *Driver {
	return &Driver{
		opener: opener,
		opts:   opts.withDefaults(),
		log:    logOrDiscard(log),
		cmdCh:  make(chan command, 8),
		done:   make(chan struct{}),
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

func (d *Driver) SetTarget(az float64) error {
	if math.IsNaN(az) || math.IsInf(az, 0) {
		return fmt.Errorf("azimuth %v is not finite", az)
	}
	return d.submit(command{frame: encodeSet(az), kind: noReply})
}

func (d *Driver) Stop() error {
	return d.submit(command{frame: encodeStop(), kind: ackReply})
}

// submit queues one command frame to the owner and waits for its wire
// result. The bound is generous — the owner answers every command, even on
// error — and expiring here means the owner is wedged in a syscall the
// watchdog has not yet reclaimed, which is a dead link worth reporting.
func (d *Driver) submit(c command) error {
	c.ack = make(chan error, 1)
	select {
	case d.cmdCh <- c:
	case <-d.done:
		return fmt.Errorf("driver stopped")
	}
	bound := 3 * (d.opts.WriteTimeout + d.opts.ReadTimeout)
	select {
	case err := <-c.ack:
		return err
	case <-time.After(bound):
		return fmt.Errorf("command not written within %s — link wedged", bound)
	case <-d.done:
		return fmt.Errorf("driver stopped")
	}
}

func (d *Driver) Readback() (float64, bool) {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	return d.az, d.valid
}

func (d *Driver) Online() bool {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	return d.online
}

func (d *Driver) Err() string {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	return d.errStr
}

// --- the owner loop -------------------------------------------------------------

// RunPoll is the single owner: it opens the port, then loops forever over
// command submissions, poll ticks, and self-heal, performing every byte of
// I/O itself. Returns when ctx is done, after closing the port.
func (d *Driver) RunPoll(ctx context.Context) {
	defer close(d.done)
	defer d.publishOffline("driver stopped")

	st := &linkState{}

	heal := func(why string, immediate bool) {
		// Fault path: close the stale handle, publish the down state, and
		// re-open after the cooldown; the opener re-resolves the by-id path
		// (KTD7). A failed attempt waits out the cooldown and retries —
		// indefinitely, paced. The initial open (immediate) skips the
		// cooldown exactly like the old zero-lastFault always-due rule, and
		// publishes nothing — the axis simply starts offline-until-first-poll.
		faulted := st.rw != nil
		if faulted {
			_ = st.rw.Close()
			st.rw = nil
		}
		st.pending = nil
		st.ackPending = false
		if faulted {
			d.publishOffline(why)
		}
		for attempt := 0; ; attempt++ {
			if !(immediate && attempt == 0) {
				deadline := time.Now().Add(d.opts.ReopenCooldown)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Until(deadline)):
				}
			}
			var err error
			st.rw, err = d.opener()
			if err == nil {
				if rts, ok := st.rw.(readTimeoutSetter); ok {
					_ = rts.SetReadTimeout(d.opts.ReadTimeout)
				}
				d.publishUp()
				d.log.Info("serial port opened")
				return
			}
			d.publishOffline("open serial: " + err.Error())
		}
	}

	heal("", true)

	ticker := time.NewTicker(d.opts.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if st.rw != nil {
				_ = st.rw.Close()
				st.rw = nil
			}
			return

		case c := <-d.cmdCh:
			_, _, err := d.exchange(st, c.frame, c.kind)
			if st.rw == nil {
				heal(err.Error(), false) // the exchange faulted the link; heal re-opens
			}
			if c.kind == noReply && err == nil {
				st.ackPending = true
			}
			c.ack <- err

		case <-ticker.C:
			frame, silent, err := d.exchange(st, encodeStatus(), wantStatus)
			if st.rw == nil {
				heal(err.Error(), false)
				continue
			}
			if silent {
				// No reply at all: a strike. Three consecutive ones are a
				// dead device behind a live adapter — take the link down
				// (the ercm timeout contract) and heal.
				d.strikes++
				if d.strikes >= maxConsecutiveTimeouts {
					why := fmt.Sprintf(
						"no status reply after %d consecutive polls (read timeout %s each)",
						d.strikes, d.opts.ReadTimeout)
					d.strikes = 0
					heal(why, false)
					continue
				}
				continue
			}
			if err != nil {
				d.strikes = 0
				continue // read fault already healed via st.rw == nil above
			}
			if frame == nil {
				continue // stray frames only; nothing usable this tick
			}
			az, derr := decodeStatusReply(frame)
			if derr != nil {
				// A malformed reply proves the link but must never be misread
				// as a position — ASCII digits in a reply are the Rot1Prog
				// desync trap and are rejected by the codec, not interpreted.
				d.log.Warn("malformed status reply", "err", derr, "frame", fmt.Sprintf("% X", frame))
				d.strikes = 0
				continue
			}
			d.commit(az)
			d.strikes = 0
			st.ackPending = false
		}
	}
}

// exchange performs one paced write→(reply) cycle. Owner-only. On a wire
// fault it closes and NILS st.rw, clears the frame buffer, publishes the
// down state, and returns the error — the caller runs heal. Semantics by
// kind:
//
//	noReply    (SET): write only, no reply phase; (nil, false) on success.
//	ackReply  (STOP): write, then consume one reply frame; silence is
//
// tolerated (a slow controller — the status path counts real silence).
//
//	wantStatus: write, then the FIRST frame is the reply — half-duplex FIFO,
//
// by structure. (nil, true) when nothing arrived in time.
//
// The ackPending exception: when the previous SET may still emit its
// non-spec zero reply, an all-zero frame is that command's ACK, never a
// position — swallow it and read once more (command accounting, per the
// ultracode review; a register resting exactly on count 0 costs one dropped
// tick here, while misreading the ACK cost a bogus az 0 live).
func (d *Driver) exchange(st *linkState, frame []byte, kind replyKind) (reply []byte, silent bool, err error) {
	if st.rw == nil {
		return nil, false, errors.New("serial link down")
	}
	if err := d.pacedWrite(st.rw, frame); err != nil {
		d.fault(st, err)
		return nil, false, err
	}

	switch kind {
	case noReply:
		return nil, false, nil

	case ackReply:
		b, had, err := d.readFrame(st)
		if err != nil {
			return nil, false, err
		}
		if had {
			d.log.Debug("consumed stop reply frame", "frame", fmt.Sprintf("% X", b))
		}
		return nil, false, nil

	default: // wantStatus
		b, had, err := d.readFrame(st)
		if err != nil {
			return nil, false, err
		}
		if !had {
			return nil, true, nil
		}
		if st.ackPending && isCommandAck(b) {
			// Ambiguous only because of the SET-silent spec vs the flood
			// reality; command accounting decides, not frame shape alone.
			d.log.Debug("consumed command ack frame", "frame", fmt.Sprintf("% X", b))
			st.ackPending = false
			b, had, err = d.readFrame(st)
			if err != nil {
				return nil, false, err
			}
			if !had {
				return nil, true, nil
			}
		}
		return b, false, nil
	}
}

// fault closes and nils the handle, drops partial frame bytes, and publishes
// the down state — the exchange's single fault exit.
func (d *Driver) fault(st *linkState, err error) {
	if st.rw != nil {
		_ = st.rw.Close()
		st.rw = nil
	}
	st.pending = nil
	d.publishOffline(err.Error())
}

// readFrame waits up to ReadTimeout for the next complete frame. Returns
// (frame, true, nil) on a frame, (nil, false, nil) on a silent timeout, and
// the fault via d.fault + error on a port read error.
func (d *Driver) readFrame(st *linkState) ([]byte, bool, error) {
	deadline := time.Now().Add(d.opts.ReadTimeout)
	for {
		// Emit a frame if one is complete in the buffer (resync-scanned).
		for len(st.pending) >= replyLen {
			i := 0
			for i < len(st.pending) && st.pending[i] != startByte {
				i++
			}
			st.pending = st.pending[i:]
			if len(st.pending) < replyLen {
				break
			}
			if st.pending[replyLen-1] != endByte {
				st.pending = st.pending[1:] // not a frame after all; slide one byte
				continue
			}
			frame := make([]byte, replyLen)
			copy(frame, st.pending[:replyLen])
			st.pending = st.pending[replyLen:]
			return frame, true, nil
		}
		if time.Now().After(deadline) {
			return nil, false, nil
		}
		buf := make([]byte, 64)
		n, err := st.rw.Read(buf)
		if n > 0 {
			st.pending = append(st.pending, buf[:n]...)
			if len(st.pending) > 64 { // endless stream with no END byte
				st.pending = st.pending[len(st.pending)-64:]
			}
		}
		if err != nil {
			d.fault(st, fmt.Errorf("read: %w", err))
			return nil, false, err
		}
	}
}

// pacedWrite enforces the ≥300 ms inter-write spacing (Appendix A) and then
// writes through the watchdog. Owner-only; the mutex stays so pace() remains
// correct even if a second write path ever appears.
func (d *Driver) pacedWrite(rw io.ReadWriteCloser, frame []byte) error {
	d.paceMu.Lock()
	if wait := d.opts.WritePace - time.Since(d.lastWriteAt); wait > 0 {
		time.Sleep(wait)
	}
	d.lastWriteAt = time.Now()
	d.paceMu.Unlock()
	return d.watchedWrite(rw, frame)
}

// watchedWrite writes one frame, bounded by WriteTimeout. go.bug.st/serial
// exposes no write deadline, so the owner arms a timer that closes THIS
// handle on stall — closing the fd converts the parked Write into an error.
// The writeSeq check makes a late timer harmless once the exchange moved on.
func (d *Driver) watchedWrite(rw io.ReadWriteCloser, frame []byte) error {
	seq := d.writeSeq.Add(1)
	done := make(chan error, 1) // buffered: a late parked Write never leaks
	go func() {
		_, err := rw.Write(frame)
		done <- err
	}()
	timer := time.AfterFunc(d.opts.WriteTimeout, func() {
		if d.writeSeq.Load() == seq {
			_ = rw.Close()
		}
	})
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-time.After(d.opts.WriteTimeout + 250*time.Millisecond):
		return fmt.Errorf("write stalled for %s — port handle closed by watchdog", d.opts.WriteTimeout)
	}
}

// --- published state ------------------------------------------------------------

func (d *Driver) commit(az float64) {
	d.stateMu.Lock()
	d.az = az
	d.valid = true
	d.online = true
	d.errStr = ""
	d.stateMu.Unlock()
}

func (d *Driver) publishUp() {
	d.stateMu.Lock()
	d.online = true
	d.errStr = ""
	d.stateMu.Unlock()
}

func (d *Driver) publishOffline(err string) {
	d.stateMu.Lock()
	d.online = false
	d.valid = false
	d.errStr = err
	d.stateMu.Unlock()
	if err != "" && err != "driver stopped" {
		d.log.Warn("serial link down", "err", err)
	}
}

// --- serial port opener ---------------------------------------------------------

// serialOpener returns the opener closure the driver self-heals through: it
// re-resolves the stable /dev/serial/by-id/ symlink on every call, so a USB
// re-enumeration heals instead of wedging on a deleted device node. The port
// read timeout is BOUNDED: the owner's exchanges rely on reads returning
// once the timeout lapses — without it a reply-less link parks the owner in
// a blocking Read forever (the 2026-09-17 az-slot freeze).
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
