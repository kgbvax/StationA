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
//     carries `nudge` (console-only intent tagging, not security) and its
//     target is within the nudge maximum (SetNudgeMax, default
//     DefaultNudgeMaxDeg) of the current readback — linear in rotor
//     coordinates, never wrapped mod 360; with the WRC offline (no readback)
//     the nudge is refused;
//   - the park itself (lock.park_az) is executed here, through the inner
//     device, once per lock seq: after the device is online and the azimuth
//     readback has been stable for StableFor. It is re-sent only when it never
//     arrived and the device came back online (reconnect, power cycle) — never
//     because of drift (the operator decided: park once, no re-aim). An
//     operator stop during the park cancels it until a new seq (repark); so
//     does an accepted maintenance nudge (the operator has taken the azimuth
//     over), also one accepted before park_az arrived (antmode still
//     grounding or retracting).
//   - a finished park (arrived, cancelled, nudged) is recorded in a small
//     state file (PersistTo), so a bridge restart does not park the same seq
//     again. A park that was underway at the restart is sent again.
//
// Concurrency: Gate methods are called from the /cmd worker, GS-232 and
// PstRotator goroutines, the WRC read loop (Observe) and the lock loop (Step).
// g.mu guards Gate state only; it is never held across an inner device call
// (a WRC write may take up to 3 s). g.moveMu orders the gate's own park write
// against the moves it passes through (a nudge, or a set_az right after
// unlock), so a park write in flight can never land after them; stop never
// takes it (an e-stop never waits).
package lock

import (
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
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
	// MinAz/MaxAz bound a maintenance nudge to the G-450's travel (the
	// range the bridge advertises in /meta).
	MinAz, MaxAz = 0.0, 450.0
	// ArriveTol is how close to park_az counts as parked.
	ArriveTol = 3.0
	// DefaultNudgeMaxDeg bounds a maintenance nudge: the farthest a tagged
	// set_az may be from the readback. The console steps ±5° from the
	// readback (rounded to whole degrees); anything farther is not a nudge.
	DefaultNudgeMaxDeg = 10.0
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

	moveMu sync.Mutex // see Concurrency above

	mu       sync.Mutex
	st       antmode.State
	refused  logging.Streak
	tooFar   logging.Streak // nudge refusals (out of bound / no readback)
	nudgeMax float64

	// Startup hold: until the first ant-mode state arrives, motion is refused
	// so a command cannot slip in ahead of the retained lock. The window is
	// open-ended until StartHold (MQTT connected, ant-mode subscribed next),
	// then lasts holdFor; with no retained ant-mode state at all it ends there.
	seen      bool
	holdFor   time.Duration
	holdUntil time.Time // zero until StartHold

	// Device observation.
	online      bool
	moving      bool
	az          float64
	stableSince time.Time // zero while offline or moving
	stableAz    float64   // readback when stableSince was (re)started

	// Park bookkeeping for the current lock seq.
	park park

	// Finished-park record (survives a restart; see PersistTo).
	statePath string
	saveMu    sync.Mutex // orders file writes; taken before g.mu
	finished  parkRecord // last finished park (zero = none)
	dirty     bool       // finished changed since the last write
}

type park struct {
	seq       uint64
	az        *int
	sent      bool
	arrived   bool
	cancelled bool // operator stop while the park was underway
	// done: nothing more to send for this seq (arrived, already there,
	// cancelled, nudged, or finished before a restart).
	done bool
}

// parkRecord is the persisted finished park. Mode, seq and park_az must all
// match the retained lock for it to count (antmode persists seq, so a seq is
// not reused unless antmode loses its own state file). A nudge ends the park
// for its seq whatever the park_az: its record matches on mode and seq, and
// has no park_az when the nudge came before park_az was published.
type parkRecord struct {
	Mode    antmode.Mode `json:"ant_mode"`
	Seq     uint64       `json:"seq"`
	ParkAz  *int         `json:"park_az,omitempty"`
	Outcome string       `json:"outcome"` // arrived | cancelled | nudged
}

// matches reports whether the record finishes the park of lock l in mode m.
func (r parkRecord) matches(m antmode.Mode, l antmode.Lock) bool {
	if r.Seq != l.Seq || r.Mode != m || l.ParkAz == nil {
		return false
	}
	return r.Outcome == outcomeNudged || (r.ParkAz != nil && *r.ParkAz == *l.ParkAz)
}

const outcomeNudged = "nudged"

// New wraps dev. now may be nil (time.Now). hold is the startup window in
// which motion is refused until the first ant-mode state arrives (0 = none,
// e.g. when the lock is disabled by config); it runs from StartHold, and
// motion is refused before StartHold too.
func New(dev Device, log *slog.Logger, now func() time.Time, hold time.Duration) *Gate {
	if now == nil {
		now = time.Now
	}
	return &Gate{inner: dev, log: log, now: now, st: antmode.NormalState, holdFor: hold,
		nudgeMax: DefaultNudgeMaxDeg}
}

// SetNudgeMax sets the maintenance nudge bound in degrees (config
// nudge_max_deg). A value <= 0 keeps the current bound. Call it before the
// gate is in use.
func (g *Gate) SetNudgeMax(deg float64) {
	if deg <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nudgeMax = deg
}

// StartHold starts the startup-hold window. Call it when the MQTT connection
// is up, before subscribing to the ant-mode state (paho OnConnect), so a slow
// broker cannot use the window up before the retained lock can arrive. A
// no-op once an ant-mode state has been seen.
func (g *Gate) StartHold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.seen && g.holdFor > 0 {
		g.holdUntil = g.now().Add(g.holdFor)
	}
}

// PersistTo loads the finished-park record from path and keeps it updated
// there. Call it once, before the first Apply. A missing file is a fresh
// start; an unreadable one is logged and ignored (the park then runs again,
// as before a restart).
func (g *Gate) PersistTo(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.statePath = path
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var rec parkRecord
	if err == nil {
		err = json.Unmarshal(data, &rec)
	}
	if err != nil {
		g.log.Warn("antenna mode park record unreadable, ignoring", "path", path, "err", err)
		return
	}
	g.finished = rec
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
	if !g.seen || st.Label() != before {
		// A new refusal reason (startup hold ended, lock entered or changed)
		// warns afresh.
		g.refused.Reset()
		g.tooFar.Reset()
	}
	g.st = st
	g.seen = true
	if !st.Locked() {
		g.park = park{}
	} else {
		l := st.L()
		if l.Seq != g.park.seq || !sameAz(l.ParkAz, g.park.az) {
			g.park = park{seq: l.Seq, az: l.ParkAz}
			switch f := g.finished; {
			case l.ParkAz == nil:
			case f.matches(st.Mode, l):
				// Finished before a bridge restart: park once means once.
				g.park.sent, g.park.done = true, true
				g.log.Info("antenna mode park already finished, not re-sent",
					"mode", st.Mode, "seq", l.Seq, "park_az", *l.ParkAz, "outcome", f.Outcome)
			default:
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
	if g.observe(st) {
		g.save()
	}
}

// observe does Observe under g.mu and reports whether the park finished.
func (g *Gate) observe(st rotor.State) (finished bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if !st.DeviceOnline {
		if g.online && g.park.sent && !g.park.done {
			// Lost the device mid-park: send again once it is back and still.
			g.park.sent = false
		}
		g.online = false
		g.stableSince = time.Time{}
		return false
	}
	switch {
	case st.Moving:
		g.stableSince = time.Time{}
	case !g.online || g.stableSince.IsZero() || math.Abs(st.Az-g.stableAz) > stableBand:
		// Measured against the azimuth the window started at, not the last
		// frame: a brakeless G-450 creeping in the wind with moving=false must
		// not count as still just because each step is small.
		g.stableSince, g.stableAz = now, st.Az
	}
	g.online, g.moving, g.az = true, st.Moving, st.Az
	if g.park.sent && !g.park.done && g.park.az != nil && !st.Moving &&
		math.Abs(st.Az-float64(*g.park.az)) <= ArriveTol {
		g.park.arrived = true
		g.finishLocked("arrived")
		g.log.Info("antenna mode park arrived", "seq", g.park.seq, "az", st.Az)
		return true
	}
	return false
}

// Step sends a pending park when its preconditions hold. Call it from a
// single loop goroutine (ticker + kicks); it may block on the device write.
func (g *Gate) Step() {
	g.moveMu.Lock()
	defer g.moveMu.Unlock()
	g.mu.Lock()
	p := g.park
	ready := g.st.Locked() && p.az != nil && !p.sent && !p.done &&
		g.online && !g.stableSince.IsZero() && g.now().Sub(g.stableSince) >= StableFor
	if !ready {
		g.mu.Unlock()
		return
	}
	if math.Abs(g.az-float64(*p.az)) <= ArriveTol {
		g.park.sent, g.park.arrived = true, true
		g.finishLocked("arrived")
		g.mu.Unlock()
		g.save()
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
		return
	}
	// An operator stop that raced the park write (it may have reached the
	// WRC first) must still win: stop again. A redundant stop is harmless.
	g.mu.Lock()
	restop := g.park.seq == p.seq && g.park.cancelled
	g.mu.Unlock()
	if restop {
		if err := g.inner.Stop(); err != nil {
			g.log.Warn("antenna mode park: re-stop after cancel failed", "err", err)
		}
	}
}

// SetAz implements rotor.Commander: refused while locked.
func (g *Gate) SetAz(az float64) error {
	if err := g.refuse("set_az", "az", az); err != nil {
		return err
	}
	g.moveMu.Lock()
	defer g.moveMu.Unlock()
	return g.inner.SetAz(az)
}

// SetAzOverride is a set_az carrying an explicit override tag. Allowed while
// locked only for the maintenance nudge; otherwise identical to SetAz. The
// nudge is bounded: a target more than the nudge maximum from the current
// readback (or any target while the WRC is offline, as there is no readback
// to bound it by) is refused with rotor.ErrLocked and ends nothing. An
// accepted nudge ends the park for the current seq, sent or not, and also
// before park_az is published (lock.nudge is set from maintenance entry, while
// antmode still grounds and retracts): the operator has taken the azimuth
// over, and neither a later park_az, a reconnect nor a restart may turn the
// boom back to the preset.
func (g *Gate) SetAzOverride(az float64, override string) error {
	g.moveMu.Lock()
	g.mu.Lock()
	nudge := g.st.Mode == antmode.Maintenance && g.st.L().Nudge && override == antmode.NudgeOverride
	if nudge {
		// Linear distance in rotor coordinates (0..450° with the overlap):
		// 358 → 3 is a 355° sweep, not 5°.
		switch {
		case !g.online:
			g.tooFar.Fail(g.log, "maintenance nudge refused: no rotator readback (WRC offline)", "az", az)
			g.mu.Unlock()
			g.moveMu.Unlock()
			return rotor.ErrLocked
		case math.IsNaN(az) || az < MinAz || az > MaxAz:
			// Inside the bound but past an end stop: the WRC's handling of an
			// out-of-travel target is undocumented (-6 might become 354).
			g.tooFar.Fail(g.log, "maintenance nudge refused: outside the rotor travel",
				"az", az, "min_az", MinAz, "max_az", MaxAz)
			g.mu.Unlock()
			g.moveMu.Unlock()
			return rotor.ErrLocked
		case math.Abs(az-g.az) > g.nudgeMax:
			g.tooFar.Fail(g.log, "maintenance nudge refused: too far from the readback",
				"az", az, "readback", g.az, "nudge_max_deg", g.nudgeMax)
			g.mu.Unlock()
			g.moveMu.Unlock()
			return rotor.ErrLocked
		}
		g.tooFar.Reset()
	}
	ended, seq, parkAzKnown, readback := false, g.park.seq, g.park.az != nil, g.az
	if nudge && !g.park.done {
		g.park.sent = true
		g.finishLocked(outcomeNudged)
		ended = true
	}
	g.mu.Unlock()
	if !nudge {
		g.moveMu.Unlock()
		return g.SetAz(az)
	}
	defer g.moveMu.Unlock()
	if ended {
		g.save()
		g.log.Info("antenna mode park ended by maintenance nudge", "seq", seq, "park_az_known", parkAzKnown)
	}
	g.log.Info("maintenance nudge accepted", "az", az, "readback", readback)
	return g.inner.SetAz(az)
}

// Jog implements rotor.Commander: refused while locked (open-ended motion).
func (g *Gate) Jog(dir string) error {
	if err := g.refuse("jog", "dir", dir); err != nil {
		return err
	}
	g.moveMu.Lock()
	defer g.moveMu.Unlock()
	return g.inner.Jog(dir)
}

// Stop implements rotor.Commander: never refused, never waits for a park
// write. A stop during an underway park cancels it until the next seq.
func (g *Gate) Stop() error {
	g.mu.Lock()
	cancelled := false
	if g.st.Locked() && g.park.sent && !g.park.done {
		g.park.cancelled = true
		g.finishLocked("cancelled")
		cancelled = true
		g.log.Warn("antenna mode park cancelled by stop", "seq", g.park.seq)
	}
	g.mu.Unlock()
	err := g.inner.Stop()
	if cancelled {
		g.save()
	}
	return err
}

// finishLocked marks the current park done and records it for PersistTo
// (park_az is nil only for a nudge before park_az; Apply then finds the
// record when park_az arrives). Caller holds g.mu and calls save after
// unlocking.
func (g *Gate) finishLocked(outcome string) {
	g.park.done = true
	var az *int
	if g.park.az != nil {
		v := *g.park.az
		az = &v
	}
	g.finished = parkRecord{Mode: g.st.Mode, Seq: g.park.seq, ParkAz: az, Outcome: outcome}
	g.dirty = true
}

// save writes the latest finished-park record when it changed (no-op without
// PersistTo). The write is atomic (temp file + rename); a failure is logged
// and the park would then run once more after a restart.
func (g *Gate) save() {
	g.saveMu.Lock()
	defer g.saveMu.Unlock()
	g.mu.Lock()
	rec, path, dirty := g.finished, g.statePath, g.dirty
	g.dirty = false
	g.mu.Unlock()
	if !dirty || path == "" {
		return
	}
	data, err := json.Marshal(rec)
	if err == nil {
		err = writeFileAtomic(path, data)
	}
	if err != nil {
		g.log.Warn("antenna mode park record not saved", "path", path, "err", err)
	}
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(data, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// CurrentAz passes the readback through (GS-232 / PstRotator queries).
func (g *Gate) CurrentAz() float64 { return g.inner.CurrentAz() }

// refuse returns rotor.ErrLocked (logging one Warn per refusal streak) while
// locked, nil otherwise.
func (g *Gate) refuse(what string, args ...any) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.seen && g.holdFor > 0 && (g.holdUntil.IsZero() || g.now().Before(g.holdUntil)) {
		g.refused.Fail(g.log, "rotator motion refused: waiting for antenna mode state",
			append([]any{"cmd", what}, args...)...)
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
