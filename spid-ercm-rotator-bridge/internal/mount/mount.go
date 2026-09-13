// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mount is the mount dispatch core (plan U4): the shared per-axis
// pipeline every control path — rotctld (U6), PstRotator (U7) and the per-slot
// MQTT /cmd surface (U5) — feeds, per the two-layer control boundary of
// KTD12: the serial drivers (internal/spid, internal/ercm) satisfy the narrow
// per-axis Controller interface below, and the Mount façade above them owns
// every cross-axis semantic — atomic stop with the bounded epoch (KTD8),
// two-axis refusal aggregation (KTD9) and park dispatch (KTD11). Servers
// consume the façade and never re-implement these semantics.
//
// Pipeline semantics implemented here:
//
//   - R8:  an intent structurally omitting an axis never constructs an intent
//     for it (a terrestrial az-only client leaves elevation untouched).
//   - R9:  motion toward an axis whose device link is down is refused with a
//     liveness reason, never silently queued or dropped; per-axis, so a dead
//     el axis refuses while az proceeds (AE3).
//   - R10: targets outside the axis's configured travel limits (including
//     non-finite values) are refused before any serial write.
//   - R11: latest-wins coalescing — one in-flight serial motion per axis, a
//     newer target supersedes any queued one (AE5).
//   - R12: a target within the configured deadband of the axis's cached
//     readback skips the serial write; a readback of unknown validity never
//     skips (KTD7/KTD8 always-write rule). The boundary at exactly the
//     configured deadband counts as within.
//
// The package is pure: no I/O, no MQTT, no serial — the controllers are
// injected, and the in-process driver mocks stand in for them in tests.
package mount

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"

	"spid-ercm-rotator-bridge/internal/config"
)

// Axis identifies one of the mount's two rotator axes.
type Axis string

// The axis names, from the config vocabulary (config.AxisAZ/AxisEL).
const (
	AZ Axis = config.AxisAZ
	EL Axis = config.AxisEL
)

// Controller is the per-axis driver contract (KTD12, the wrc narrow-interface
// shape): set-target, stop, cached readback plus its validity, and the
// device-link liveness. The SPID driver (internal/spid — Driver, Mock and the
// Axis interface they share) and the ERC-M driver (internal/ercm.Driver)
// satisfy it as written; no adapters are needed.
type Controller interface {
	// SetTarget commands the axis to a position. A non-nil error reports a
	// dead link, not device acceptance.
	SetTarget(deg float64) error
	// Stop halts the axis.
	Stop() error
	// Readback returns the cached position and whether it is valid — false
	// until the first status reply after startup or a reopen (KTD7).
	Readback() (deg float64, valid bool)
	// Online reports the device link (the device_online layer of the
	// two-liveness rule, not the bridge /status layer).
	Online() bool
}

// Target is one mount-level position intent. The Has flags are a NaN-free
// "is this axis carried" selector: an axis with a false flag is structurally
// omitted and never constructs an intent (R8) — there is no magic-value
// target that would silently move an axis the wire message never mentioned.
type Target struct {
	AZ, EL       float64
	HasAZ, HasEL bool
}

// RefusalReason distinguishes why an axis refused a target, so U6 can map it
// onto the rotctld RPRT vocabulary (liveness → -9, limit → -1) and U7 can log
// it (KTD9).
type RefusalReason int

const (
	// RefusalLimit: the target is outside the axis's travel envelope (R10).
	RefusalLimit RefusalReason = iota + 1
	// RefusalLiveness: the axis's device link is down (R9).
	RefusalLiveness
)

// String returns the canonical reason name used in logs and error strings.
func (r RefusalReason) String() string {
	switch r {
	case RefusalLimit:
		return "limit"
	case RefusalLiveness:
		return "liveness"
	default:
		return "unknown"
	}
}

// Refusal is one axis's refusal of one target. It implements error so a
// single refusal flows into error paths unchanged.
type Refusal struct {
	Axis   Axis
	Reason RefusalReason
	Target float64
	Detail string
}

// Error implements error.
func (r Refusal) Error() string {
	return fmt.Sprintf("%s axis refused (target %v): %s: %s",
		r.Axis, r.Target, r.Reason, r.Detail)
}

// AxisError reports a controller-level fault on one axis (a stop frame that
// could not be written). Stop is always attempted on both axes regardless;
// the caller decides how a fault maps onto its reply.
type AxisError struct {
	Axis Axis
	Err  error
}

// Error implements error.
func (e AxisError) Error() string {
	return fmt.Sprintf("%s axis: %v", e.Axis, e.Err)
}

// Mount is the two-axis dispatch façade. It owns the bounded stop epoch
// (KTD8) and one per-axis pipeline worker; all serial motion and stop frames
// flow through the workers' writeMu, one in flight per axis (R11).
type Mount struct {
	ctl config.ControlConfig
	log *slog.Logger

	// mu guards stopSeq and — through each axis's own mutex — the admission
	// window: a target is admitted and epoch-stamped atomically with respect
	// to any concurrent Stop, which is what bounds the stop's suppression to
	// intents admitted before it.
	mu      sync.Mutex
	stopSeq uint64 // the bounded stop epoch (KTD8)

	axes  map[Axis]*axis
	order []*axis // fixed az→el order for deterministic stops and reports
}

// New builds the façade over the two axis controllers. Run must be called
// (typically in its own goroutine) to start the per-axis workers; until then
// admitted intents simply queue.
func New(az, el Controller, ctl config.ControlConfig, log *slog.Logger) *Mount {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := &Mount{ctl: ctl, log: log}
	m.axes = map[Axis]*axis{
		AZ: newAxis(m, AZ, az, ctl.AZ),
		EL: newAxis(m, EL, el, ctl.EL),
	}
	m.order = []*axis{m.axes[AZ], m.axes[EL]}
	return m
}

// Run drives the per-axis dispatch workers until ctx is done — the same
// caller-owns-the-goroutine shape as the drivers' RunPoll/Run.
func (m *Mount) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, a := range m.order {
		wg.Add(1)
		go func(a *axis) {
			defer wg.Done()
			a.run(ctx)
		}(a)
	}
	wg.Wait()
}

// stopSeqNow reads the bounded stop epoch (the worker re-checks it inside
// the serial section before every write).
func (m *Mount) stopSeqNow() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopSeq
}

// Goto admits one mount-level position intent and returns the refusals, one
// per refused axis (empty = fully admitted, including deadband no-ops).
// Checks run per axis in the pipeline order: limit (R10) before liveness (R9)
// — input validity does not depend on device state — then admission to the
// latest-wins coalescer. A structurally omitted axis (R8) is never touched.
// The deadband skip (R12) is decided at write time against the freshest
// cached readback, so a superseding burst can never be answered with a
// position the axis has already left behind.
func (m *Mount) Goto(t Target) []Refusal {
	var refs []Refusal
	m.mu.Lock()
	seq := m.stopSeq
	if t.HasAZ {
		if r, ok := m.axes[AZ].admit(t.AZ, seq); !ok {
			refs = append(refs, r)
		}
	}
	if t.HasEL {
		if r, ok := m.axes[EL].admit(t.EL, seq); !ok {
			refs = append(refs, r)
		}
	}
	m.mu.Unlock()
	return refs
}

// Stop is the atomic all-stop from any control path (KTD8): it clears every
// pending target on BOTH axes, cancels intents claimed but not yet on the
// wire, and halts both axes. In-flight serial writes finish first (the stop
// frame follows them on the wire — a byte already sent cannot be unsent).
// The suppression is bounded by the epoch: it covers only intents admitted
// before this stop; intents arriving after it are admitted normally, so the
// pipeline is never jammed. Per-axis controller faults are returned for the
// caller's reply mapping; the halt is attempted on both axes regardless.
func (m *Mount) Stop() []AxisError {
	m.mu.Lock()
	m.stopSeq++
	for _, a := range m.order {
		a.beginHalt()
	}
	m.mu.Unlock()

	var errs []AxisError
	for _, a := range m.order {
		if ae := a.halt(); ae.Err != nil {
			errs = append(errs, ae)
		}
	}
	return errs
}

// Park is the atomic mount-level park intent (KTD11): its stop phase cancels
// every queued target and halts both axes, then both configured park targets
// dispatch through the normal Goto paths — the epoch bump bounds the stop's
// suppression to intents admitted before it, so the park slew is never
// suppressed by its own stop phase, and an already-at-park axis is a
// deadband no-op (R7). Park-phase stop faults are logged here; refusals of
// the park targets themselves are returned like Goto's.
func (m *Mount) Park() []Refusal {
	for _, ae := range m.Stop() {
		m.log.Warn("park: stop-phase halt fault", "axis", string(ae.Axis), "err", ae.Err)
	}
	return m.Goto(Target{
		AZ:    m.ctl.AZ.Park,
		EL:    m.ctl.EL.Park,
		HasAZ: true,
		HasEL: true,
	})
}

// --- per-axis telemetry for the surfaces above ---------------------------------------

// Readback returns the axis's cached position and its validity (U6: an
// invalid readback answers RPRT -11, never a fabricated position).
func (m *Mount) Readback(ax Axis) (float64, bool) {
	a := m.axes[ax]
	if a == nil {
		return 0, false
	}
	return a.ctrl.Readback()
}

// Online reports the axis's device-link liveness (U5: /state.device_online).
func (m *Mount) Online(ax Axis) bool {
	a := m.axes[ax]
	return a != nil && a.ctrl.Online()
}

// Target returns the axis's last admitted target. Cleared by Stop (KTD14:
// moving is cleared on stop), so ok is false until the next admitted intent.
func (m *Mount) Target(ax Axis) (float64, bool) {
	a := m.axes[ax]
	if a == nil {
		return 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.target == nil {
		return 0, false
	}
	return *a.target, true
}

// Moving reports the axis's inferred motion (KTD14): |target − readback| >
// deadband, false while readback validity is unknown, false after a stop.
func (m *Mount) Moving(ax Axis) bool {
	a := m.axes[ax]
	if a == nil {
		return false
	}
	deg, ok := m.Target(ax)
	if !ok {
		return false
	}
	rb, valid := a.ctrl.Readback()
	return valid && math.Abs(deg-rb) > a.ctl.Deadband
}
