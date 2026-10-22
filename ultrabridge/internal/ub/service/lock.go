package service

// Antenna-mode lock (slot hf/ant-mode, contract antmode/docs/ant-mode-mqtt-api.md).
//
// While the station antenna mode is storm or maintenance the Ultrabeam elements
// stay in:
//
//   - SetFrequency and SetMode (every element extend: frequency, band and
//     direction changes) return ErrLocked — one decision for the MQTT /cmd
//     path and the HTTP API alike;
//   - Retract is never refused;
//   - EnsureRetract (called from the 2 s poll loop) retracts the elements
//     itself when the lock carries `retract`: on a new lock seq, when the
//     RCU-06 comes back online (a power cycle may re-extend — also mid-retract;
//     the latch is cleared until the re-issued retract is accepted), and at bridge
//     start once the lock is known — then keeps retrying on the poll until the
//     latch reads retracted and the motors have stopped. This also restores
//     the retract latch the bridge loses on a restart. A lock with
//     retract=false (antmode grounds the antenna first) only refuses extends.
//
// Reading rules: no retained ant-mode state = normal (rollout-safe); a valid
// locked snapshot is enforced even while hf/ant-mode/status is offline; a
// malformed payload keeps the last known state (Error logged). ts is not
// checked.
//
// Concurrency: ApplyAntMode runs on paho's dispatch goroutine and only parses
// and records under c.lmu (never I/O); the extend gate runs on the /cmd worker
// and HTTP goroutines; EnsureRetract runs on the poll goroutine. c.lmu is never
// held across a device exchange.

import (
	"context"
	"errors"
	"time"

	"codeberg.org/kgbvax/stationa/shared/antmode"
	"codeberg.org/kgbvax/stationa/shared/logging"
)

// ErrLocked is returned by SetFrequency and SetMode while the antenna-mode
// lock (or the startup hold) refuses element extends. Callers map it to their
// own refusal (HTTP 423) and skip their own Warn: the controller already logs
// one Warn per refusal streak.
var ErrLocked = errors.New("antenna mode lock: element extend refused")

// lockState is the controller's view of hf/ant-mode. Guarded by Controller.lmu.
type lockState struct {
	st antmode.State

	// Startup hold: until the first ant-mode state arrives (or holdUntil
	// passes), extends are refused so a retained /cmd cannot slip in ahead of
	// the retained lock right after (re)start.
	seen      bool
	holdUntil time.Time

	// Seq bookkeeping for the ensure-retract triggers.
	haveSeq bool
	seq     uint64
	retract bool

	// Ensure-retract: pending means a retract must be (re)issued; sent means
	// one was accepted since and the poll now waits for retracted && !moving.
	pending bool
	sent    bool

	refused     logging.Streak // extend refusals; reset on unlock
	retractFail logging.Streak // ensure-retract failures; reset on success
}

// SetLockHold arms the startup hold: extends are refused until the first
// ant-mode state is applied or d has passed. Call once at startup, only when
// the lock is enabled (ant_mode_slot configured); 0 disables the hold.
func (c *Controller) SetLockHold(d time.Duration) {
	c.lmu.Lock()
	c.lk.holdUntil = c.now().Add(d)
	c.lmu.Unlock()
}

// LockKick delivers a token when an applied ant-mode state changed the lock
// label or requested a retract, so the poll loop runs EnsureRetract and
// publishes the label at once instead of at the next 2 s tick. Republished
// snapshots that change nothing here (antmode's wind/phase updates) do not
// kick: each kick costs a poll's serial round-trips.
func (c *Controller) LockKick() <-chan struct{} { return c.kick }

// ApplyAntMode ingests a hf/ant-mode/state payload. It only parses and records
// (safe on paho's dispatch goroutine) and reports whether the lock label
// changed. A malformed payload keeps the last known state.
func (c *Controller) ApplyAntMode(payload []byte) (labelChanged bool) {
	st, err := antmode.Parse(payload)
	if err != nil {
		c.log.Error("ant-mode state rejected, keeping last known lock", "err", err)
		return false
	}
	c.lmu.Lock()
	before := c.lk.st.Label()
	retractNow := false
	c.lk.st = st
	c.lk.seen = true
	if !st.Locked() {
		c.lk.haveSeq, c.lk.seq, c.lk.retract = false, 0, false
		c.lk.pending, c.lk.sent = false, false
		c.lk.refused.Reset()
		c.lk.retractFail.Reset()
	} else {
		l := st.L()
		newSeq := !c.lk.haveSeq || l.Seq != c.lk.seq
		// A new seq, or retract appearing on the current one (antmode builds
		// the lock up: ground first, retract once the switch reports off).
		if l.Retract && (newSeq || !c.lk.retract) {
			c.lk.pending, c.lk.sent, retractNow = true, false, true
			c.log.Info("antenna mode retract requested", "mode", st.Mode, "seq", l.Seq)
		}
		c.lk.haveSeq, c.lk.seq, c.lk.retract = true, l.Seq, l.Retract
	}
	after := st.Label()
	c.lmu.Unlock()

	if after != before || retractNow {
		select {
		case c.kick <- struct{}{}:
		default:
		}
	}
	if after != before {
		c.log.Info("antenna mode lock changed", "lock", after, "was", before)
		return true
	}
	return false
}

// LockLabel is the /state `lock` value: "" when unlocked, else the mode name.
func (c *Controller) LockLabel() string {
	c.lmu.Lock()
	defer c.lmu.Unlock()
	return c.lk.st.Label()
}

// checkExtend returns ErrLocked (logging) while the lock or the startup hold
// refuses an element extend, nil otherwise.
func (c *Controller) checkExtend(what string, args ...any) error {
	c.lmu.Lock()
	defer c.lmu.Unlock()
	if !c.lk.seen && c.now().Before(c.lk.holdUntil) {
		c.log.Warn("ultrabeam extend refused: waiting for antenna mode state",
			append([]any{"cmd", what}, args...)...)
		return ErrLocked
	}
	if !c.lk.st.Locked() {
		return nil
	}
	c.lk.refused.Fail(c.log, "ultrabeam extend refused: antenna mode lock",
		append([]any{"lock", c.lk.st.Label(), "cmd", what}, args...)...)
	return ErrLocked
}

// deviceBackOnline re-arms the ensure-retract on an offline→online edge: an
// RCU-06 power cycle may have driven the elements back out. It re-arms even
// while a retract is in flight (sent, waiting for the motors): that retract
// may not have finished before the outage. The latch is cleared too, so /state
// does not claim retracted until the re-issued retract is accepted.
func (c *Controller) deviceBackOnline() {
	c.lmu.Lock()
	rearm := c.lk.st.Locked() && c.lk.st.L().Retract
	if rearm {
		c.lk.pending, c.lk.sent = true, false
		c.log.Info("antenna mode retract re-armed: device back online", "seq", c.lk.seq)
	}
	c.lmu.Unlock()
	if rearm {
		c.setRetracted(false) // c.mu after c.lmu is released: never nested
	}
}

// EnsureRetract drives the elements in while the lock carries `retract`. Call
// it from the poll loop after Refresh/PollMotorStatus (it reads their state);
// it may block on a retract exchange. A no-op when unlocked, when the lock has
// retract=false, or while the device is offline (the online edge re-arms).
func (c *Controller) EnsureRetract(ctx context.Context) {
	s := c.State()
	c.lmu.Lock()
	if !c.lk.st.Locked() || !c.lk.st.L().Retract {
		c.lmu.Unlock()
		return
	}
	// Belt and braces: an extend that passed the gate just before the lock
	// landed can still clear the latch after our retract.
	if !s.Retracted && !s.Offline && !c.lk.pending {
		c.lk.pending = true
		c.log.Info("antenna mode retract re-armed: elements not retracted", "seq", c.lk.seq)
	}
	if c.lk.sent && !s.Retracted {
		c.lk.sent = false
	}
	pending, sent, seq, label := c.lk.pending, c.lk.sent, c.lk.seq, c.lk.st.Label()
	c.lmu.Unlock()

	if !pending || s.Offline {
		return
	}
	if sent {
		if s.Retracted && !s.MotorsMoving {
			c.lmu.Lock()
			if c.lk.pending && c.lk.sent && c.lk.seq == seq {
				c.lk.pending, c.lk.sent = false, false
				c.lk.retractFail.Reset()
				c.log.Info("antenna mode: elements retracted", "lock", label, "seq", seq)
			}
			c.lmu.Unlock()
		}
		return
	}

	c.log.Info("antenna mode: retracting elements", "lock", label, "seq", seq)
	err := c.Retract(ctx)
	c.lmu.Lock()
	defer c.lmu.Unlock()
	if err != nil {
		c.lk.retractFail.Fail(c.log, "antenna mode retract failed, retrying on next poll",
			"lock", label, "seq", seq, "err", err)
		return
	}
	if c.lk.pending && c.lk.seq == seq {
		c.lk.sent = true
	}
}
