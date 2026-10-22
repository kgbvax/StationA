// Package lock enforces the station antenna mode (slot hf/ant-mode, contract
// antmode/docs/ant-mode-mqtt-api.md) on the HF rotator.
//
// Gate wraps the WRC device and is handed to every inbound control path (MQTT
// /cmd, GS-232, PstRotator) in place of the device, so one decision covers
// them all. While the mode is storm or maintenance:
//
//   - set_az and fwd/rev are refused with rotor.ErrLocked;
//   - stop is always passed through (an e-stop is never refused);
//   - a set_az tagged with the maintenance override is allowed when the lock
//     carries `nudge` (console-only intent tagging, not security);
//   - the park itself (lock.park_az) is executed here, through the inner
//     device, once per lock seq: after the device is online and the azimuth
//     readback has been stable for StableFor. It is re-sent only when it never
//     arrived and the device came back online (reconnect, power cycle) — never
//     because of drift (the operator decided: park once, no re-aim). An
//     operator stop during the park cancels it until a new seq (repark).
//
// Concurrency: Gate methods are called from the /cmd worker, GS-232 and
// PstRotator goroutines, the WRC read loop (Observe) and the lock loop (Step).
// g.mu guards Gate state only; it is never held across an inner device call
// (a WRC write may take up to 3 s).
package lock

import (
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"codeberg.org/kgbvax/stationa/shared/antmode"
	"codeberg.org/kgbvax/stationa/shared/logging"

	"wrc-rotator-bridge/internal/rotor"
)

const (
	// StableFor is how long the azimuth readback must hold still after the
	// device comes online before the park is sent (the readback can jump right
	// after a power-on).
	StableFor = 3 * time.Second
	// stableBand is the readback wobble tolerated as "still".
	stableBand = 1.0
	// ArriveTol is how close to park_az counts as parked.
	ArriveTol = 3.0
)

// Device is what the gate wraps: the commander plus the position readback.
type Device interface {
	rotor.Commander
	CurrentAz() float64
}

// Gate is a rotor.Commander that enforces the antenna-mode lock.
type Gate struct {
	inner Device
	log   *slog.Logger
	now   func() time.Time

	mu      sync.Mutex
	st      antmode.State
	refused logging.Streak

	// Startup hold: until the first ant-mode state arrives (or holdUntil
	// passes), motion is refused so a command cannot slip in ahead of the
	// retained lock right after (re)start.
	seen      bool
	holdUntil time.Time

	// Device observation.
	online      bool
	moving      bool
	az          float64
	stableSince time.Time // zero while offline or moving

	// Park bookkeeping for the current lock seq.
	park park
}

type park struct {
	seq       uint64
	az        *int
	sent      bool
	arrived   bool
	cancelled bool // operator stop while the park was underway
}

// New wraps dev. now may be nil (time.Now). hold is the startup window in
// which motion is refused until the first ant-mode state arrives (0 = none,
// e.g. when the lock is disabled by config).
func New(dev Device, log *slog.Logger, now func() time.Time, hold time.Duration) *Gate {
	if now == nil {
		now = time.Now
	}
	return &Gate{inner: dev, log: log, now: now, st: antmode.NormalState, holdUntil: now().Add(hold)}
}

// Apply ingests a hf/ant-mode/state payload. It reports whether the published
// lock label changed. A malformed payload keeps the last known state.
func (g *Gate) Apply(payload []byte) (labelChanged bool) {
	st, err := antmode.Parse(payload)
	if err != nil {
		g.log.Error("ant-mode state rejected, keeping last known lock", "err", err)
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	before := g.st.Label()
	g.st = st
	g.seen = true
	if !st.Locked() {
		g.park = park{}
		g.refused.Reset()
	} else {
		l := st.L()
		if l.Seq != g.park.seq || !sameAz(l.ParkAz, g.park.az) {
			g.park = park{seq: l.Seq, az: l.ParkAz}
			if l.ParkAz != nil {
				g.log.Info("antenna mode park requested", "mode", st.Mode, "seq", l.Seq, "park_az", *l.ParkAz)
			}
		}
	}
	if st.Label() != before {
		g.log.Info("antenna mode lock changed", "lock", st.Label(), "was", before)
		return true
	}
	return false
}

// Label is the /state `lock` value ("" when unlocked).
func (g *Gate) Label() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.st.Label()
}

// Observe ingests a device telemetry/online change.
func (g *Gate) Observe(st rotor.State) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if !st.DeviceOnline {
		if g.online && g.park.sent && !g.park.arrived {
			// Lost the device mid-park: send again once it is back and still.
			g.park.sent = false
		}
		g.online = false
		g.stableSince = time.Time{}
		return
	}
	switch {
	case st.Moving:
		g.stableSince = time.Time{}
	case !g.online || g.stableSince.IsZero() || math.Abs(st.Az-g.az) > stableBand:
		g.stableSince = now
	}
	g.online, g.moving, g.az = true, st.Moving, st.Az
	if g.park.sent && !g.park.arrived && g.park.az != nil && !st.Moving &&
		math.Abs(st.Az-float64(*g.park.az)) <= ArriveTol {
		g.park.arrived = true
		g.log.Info("antenna mode park arrived", "seq", g.park.seq, "az", st.Az)
	}
}

// Step sends a pending park when its preconditions hold. Call it from a
// single loop goroutine (ticker + kicks); it may block on the device write.
func (g *Gate) Step() {
	g.mu.Lock()
	p := g.park
	ready := g.st.Locked() && p.az != nil && !p.sent && !p.arrived && !p.cancelled &&
		g.online && !g.stableSince.IsZero() && g.now().Sub(g.stableSince) >= StableFor
	if !ready {
		g.mu.Unlock()
		return
	}
	if math.Abs(g.az-float64(*p.az)) <= ArriveTol {
		g.park.sent, g.park.arrived = true, true
		g.mu.Unlock()
		g.log.Info("antenna mode park: already at park azimuth", "seq", p.seq, "az", *p.az)
		return
	}
	g.park.sent = true
	g.mu.Unlock()

	g.log.Info("antenna mode park: rotating", "seq", p.seq, "park_az", *p.az)
	if err := g.inner.SetAz(float64(*p.az)); err != nil {
		g.log.Warn("antenna mode park: set_az failed, will retry", "err", err)
		g.mu.Lock()
		if g.park.seq == p.seq {
			g.park.sent = false
		}
		g.mu.Unlock()
	}
}

// SetAz implements rotor.Commander: refused while locked.
func (g *Gate) SetAz(az float64) error {
	if err := g.refuse("set_az", "az", az); err != nil {
		return err
	}
	return g.inner.SetAz(az)
}

// SetAzOverride is a set_az carrying an explicit override tag. Allowed while
// locked only for the maintenance nudge; otherwise identical to SetAz.
func (g *Gate) SetAzOverride(az float64, override string) error {
	g.mu.Lock()
	nudge := g.st.Mode == antmode.Maintenance && g.st.L().Nudge && override == antmode.NudgeOverride
	g.mu.Unlock()
	if nudge {
		g.log.Info("maintenance nudge accepted", "az", az)
		return g.inner.SetAz(az)
	}
	return g.SetAz(az)
}

// Jog implements rotor.Commander: refused while locked (open-ended motion).
func (g *Gate) Jog(dir string) error {
	if err := g.refuse("jog", "dir", dir); err != nil {
		return err
	}
	return g.inner.Jog(dir)
}

// Stop implements rotor.Commander: never refused. A stop during an underway
// park cancels it until the next seq.
func (g *Gate) Stop() error {
	g.mu.Lock()
	if g.st.Locked() && g.park.sent && !g.park.arrived && !g.park.cancelled {
		g.park.cancelled = true
		g.log.Warn("antenna mode park cancelled by stop", "seq", g.park.seq)
	}
	g.mu.Unlock()
	return g.inner.Stop()
}

// CurrentAz passes the readback through (GS-232 / PstRotator queries).
func (g *Gate) CurrentAz() float64 { return g.inner.CurrentAz() }

// refuse returns rotor.ErrLocked (logging one Warn per refusal streak) while
// locked, nil otherwise.
func (g *Gate) refuse(what string, args ...any) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.seen && g.now().Before(g.holdUntil) {
		g.log.Warn("rotator motion refused: waiting for antenna mode state", "cmd", what)
		return rotor.ErrLocked
	}
	if !g.st.Locked() {
		return nil
	}
	g.refused.Fail(g.log, "rotator motion refused: antenna mode lock",
		append([]any{"lock", g.st.Label(), "cmd", what}, args...)...)
	return rotor.ErrLocked
}

// IsLocked reports whether err is a lock refusal.
func IsLocked(err error) bool { return errors.Is(err, rotor.ErrLocked) }

func sameAz(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
