// SPDX-License-Identifier: AGPL-3.0-or-later

package mount

// The per-axis pipeline behind the Mount façade (plan U4): one worker
// goroutine per axis, latest-wins coalescing (R11), the bounded stop epoch
// (KTD8) and the claim-time deadband rule (R12). The pelcobridge2 one-intent
// discipline, adapted: at most one intent is in flight per axis, everyone
// else's intents supersede whatever is still queued, and an all-stop cancels
// every queued intent on both axes without ever jamming the pipeline.

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"

	"spid-ercm-rotator-bridge/internal/config"
)

// axis is one axis's pipeline state plus its controller.
//
// Locking model (all orders cycle-free):
//
//	m.mu    the epoch; held during admission and Stop's clear phase, and may
//	        nest axis.mu inside it (m.mu → axis.mu).
//	a.mu    the pipeline state (pending, pendSeq, target, inFlight, halting).
//	a.writeMu the serial section: held across one controller write plus the
//	        epoch re-check before it. Stop's halt frame takes it too, so a
//	        stop frame and a set frame can never reorder on the wire.
//
// Nobody holds a.mu while acquiring writeMu or m.mu, and m.mu is never held
// across a controller call, so no ordering inversion exists between the
// admission path, the workers and Stop.
type axis struct {
	name  Axis
	ctrl  Controller
	ctl   config.AxisControl
	log   *slog.Logger
	mount *Mount

	mu       sync.Mutex // guards the fields below
	pending  *float64   // latest admitted target not yet claimed by the worker
	pendSeq  uint64     // the stop epoch at the pending target's admission
	target   *float64   // last admitted target (Mount.Target/Moving); nil after Stop
	inFlight int        // claimed intents currently inside the serial section
	halting  bool       // a Stop is mid-flight: it owns the wire next

	writeMu sync.Mutex    // the serial section (see the locking model)
	notify  chan struct{} // cap-1 wakeups for the worker

	// dirty is guarded by writeMu alone: the wire owes this axis a stop
	// frame. It starts true — at process birth the wire state is unknown (a
	// pre-restart set frame may still be driving the controller), so the
	// first halt is always real. A written set frame keeps it true, a written
	// stop frame clears it. The stop-debounce (see halt) must not skip a halt
	// while it is set, so it is not foldable into the a.mu state.
	dirty bool
}

func newAxis(m *Mount, name Axis, ctrl Controller, ctl config.AxisControl) *axis {
	return &axis{
		name:   name,
		ctrl:   ctrl,
		ctl:    ctl,
		log:    m.log,
		mount:  m,
		notify: make(chan struct{}, 1),
		dirty:  true,
	}
}

// admit validates one target and admits it to the coalescer. Called with
// m.mu held, so the epoch stamp is atomic with the pending slot: a concurrent
// Stop either fully precedes this admission (the target survives with the new
// epoch) or fully follows it (its clear phase drops the target). The online
// flag is the R9 liveness view the caller sampled BEFORE m.mu was taken —
// admit performs no controller call, keeping the m.mu section pure. Returns
// the refusal when the target is not admitted.
func (a *axis) admit(deg float64, seq uint64, online bool) (Refusal, bool) {
	// R10: limit refusal before any serial write. Non-finite targets are
	// limit refusals too — the comparison against a NaN is true for nothing.
	if math.IsNaN(deg) || math.IsInf(deg, 0) || deg < a.ctl.Min || deg > a.ctl.Max {
		return Refusal{
			Axis:   a.name,
			Reason: RefusalLimit,
			Target: deg,
			Detail: fmt.Sprintf("target outside travel limits [%.1f, %.1f]", a.ctl.Min, a.ctl.Max),
		}, false
	}
	// R9/KTD9: the local two-layer liveness view — a dead device link
	// refuses the axis; the intent is never queued or dropped silently.
	if !online {
		return Refusal{
			Axis:   a.name,
			Reason: RefusalLiveness,
			Target: deg,
			Detail: "axis device link offline",
		}, false
	}

	d := deg
	a.mu.Lock()
	a.pending = &d // latest-wins: a superseded queued target is overwritten
	a.pendSeq = seq
	a.target = &d
	a.mu.Unlock()
	select {
	case a.notify <- struct{}{}:
	default: // a wakeup is already staged; the worker drains to the newest
	}
	return Refusal{}, true
}

// beginHalt is Stop's queue-cancel phase on one axis: drop the queued target,
// clear the recorded target (KTD14: moving is cleared on stop) and raise the
// halt flag that owns the wire next. Called with m.mu held.
func (a *axis) beginHalt() {
	a.mu.Lock()
	a.pending = nil
	a.target = nil
	a.halting = true
	a.mu.Unlock()
}

// halt writes the stop frame — unless stopOwed says the wire is already in
// the halted state this process last imposed. It waits for the serial
// section, so an in-flight set frame finishes first and the stop frame
// follows it — the only ordering a byte already on the wire admits. Clearing
// the halt flag inside the serial section guarantees no worker can observe a
// stale flag while holding writeMu.
func (a *axis) halt() AxisError {
	a.writeMu.Lock()
	var err error
	if a.stopOwed() {
		err = a.ctrl.Stop()
		if err == nil {
			a.dirty = false
		}
	} else {
		// Stop-debounce: every set frame has been followed by a stop frame,
		// and no intent is anywhere in the pipeline — a stop frame would only
		// pulse the controller's stop relay. Observed live 2026-09-23: a
		// rotctld client S-flooding at ~1 Hz clattered the ERC-M relay
		// continuously without ever moving anything. The command answer is
		// unaffected (the façade's Stop still succeeds), and anything that
		// moves — or anything written by an earlier process incarnation —
		// sees dirty true and halts for real.
	}
	a.mu.Lock()
	a.halting = false
	a.mu.Unlock()
	a.writeMu.Unlock()
	// Wake the worker: a claim may have yielded to the halt and re-queued.
	select {
	case a.notify <- struct{}{}:
	default:
	}
	return AxisError{Axis: a.name, Err: err}
}

// stopOwed reports whether a stop frame owes the wire a write: any intent in
// the pipeline (queued, recorded, or claimed), or a set frame / process
// restart that leaves the wire state unknown. The intent fields are read
// under a.mu; dirty is only touched under writeMu, which the caller (halt)
// holds — so the worker cannot slip a claim or a write past this check.
func (a *axis) stopOwed() bool {
	a.mu.Lock()
	busy := a.pending != nil || a.target != nil || a.inFlight > 0
	a.mu.Unlock()
	return busy || a.dirty
}

// run is the per-axis worker: park on the notify channel, drain the pipeline
// after every wakeup, one serial write in flight at a time (R11).
func (a *axis) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.notify:
			a.drain()
		}
	}
}

// outOfDeadband reports whether a target is outside the axis's no-op
// deadband of a cached readback (the R12 comparison). For the az axis the
// distance is circular (the 359°↔1° seam counts as 2°, not 358°) — a linear
// comparison over-fires at north and, against a readback that ever lands a
// full turn off, neuters the deadband entirely; el is linear (0–180 does not
// wrap).
func (a *axis) outOfDeadband(deg, rb float64) bool {
	d := math.Abs(deg - rb)
	if a.name == AZ && d > 180 {
		d = 360 - d
	}
	return d > a.ctl.Deadband
}

// drain claims the latest pending target and writes it, repeatedly, until
// the queue is empty. Between claims a newer target may have superseded the
// last one — claiming again picks it up, so the axis always moves toward the
// newest admitted target.
func (a *axis) drain() {
	for {
		a.mu.Lock()
		if a.pending == nil {
			a.mu.Unlock()
			return
		}
		deg := *a.pending
		seq := a.pendSeq
		a.pending = nil
		a.inFlight++
		a.mu.Unlock()

		a.writeMu.Lock()
		if a.yieldIfHalting(deg, seq) {
			// The stop owns the wire next and the worker yielded — release
			// the serial section too, or the worker's own next drain and
			// Stop's halt deadlock on the leaked lock.
			a.writeMu.Unlock()
			return
		}
		if seq != a.mount.stopSeqNow() {
			// KTD8 bounded epoch: this intent was admitted before a stop
			// that intervened between admission and claim. Drop it — the
			// suppression covers exactly the intents admitted before the
			// stop, and nothing beyond them.
		} else if rb, valid := a.ctrl.Readback(); !valid || a.outOfDeadband(deg, rb) {
			// R12: within the configured deadband of a VALID cached
			// readback the write is skipped; an unknown readback never
			// skips (KTD7 always-write). The boundary at exactly the
			// deadband counts as within (<=).
			if err := a.ctrl.SetTarget(deg); err != nil {
				// A failed write is reported, never retried: re-sending a
				// motion command with no operator behind it is the
				// ultrabridge stale-cmd pattern. The next poll marks the
				// link down and the control path re-issues.
				a.log.Warn("serial write failed", "axis", string(a.name), "target", deg, "err", err)
			} else {
				// A set frame may now be on the wire: the next halt owes it a
				// stop frame, however idle the pipeline looks by then.
				a.dirty = true
			}
		}
		a.mu.Lock()
		a.inFlight--
		a.mu.Unlock()
		a.writeMu.Unlock()
	}
}

// yieldIfHalting handles a claim that raced a stop: the stop owns the wire
// next, so the claimed intent goes back to the pending slot (any newer
// pending wins — latest-wins) and the worker yields. The re-queue is
// load-bearing, not a deferred no-op: a pre-stop intent carries its old
// epoch and is dropped at the next claim, but a post-stop intent admitted
// during the halt window carries the new epoch and survives it, writing
// after the stop frame — so the wire order is always
// set-before-stop-before-next-set. Only called with writeMu held; reports
// whether the worker yielded (drain returns).
func (a *axis) yieldIfHalting(deg float64, seq uint64) bool {
	a.mu.Lock()
	halting := a.halting
	if halting {
		if a.pending == nil {
			d := deg
			a.pending = &d
			a.pendSeq = seq
		}
		a.inFlight--
	}
	a.mu.Unlock()
	return halting
}
