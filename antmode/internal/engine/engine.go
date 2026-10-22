// SPDX-License-Identifier: AGPL-3.0-or-later

// Package engine is antmode's runtime: it owns the mode (through
// internal/policy), builds the lock in safe order, borrows antenna power when
// needed, persists everything, and publishes /meta and /state.
//
// Lock sequencing, per entry or repark (seq changes each time):
//
//	ground  → lock{ground}, phase grounding
//	        → hf/ant-switch reports selected "off" (or the ground timeout passes
//	          while hf/radio is not transmitting)
//	retract → lock.retract, phase retracting
//	        → hf/ant-ctrl live, retracted and not moving (or the retract timeout)
//	park    → lock.park_az (storm: wind ± 90° nearer the rotator; maintenance:
//	          the preset), phase rotating
//	        → hf/rotator live, within tolerance, not moving → parked
//	          (rotate timeout → failed + fault; a late arrival still parks)
//
// antmode never commands the rotator or the Ultrabeam: the bridges read the
// lock and act. The only command antmode sends is power/antennas set_power
// for the power borrow (and the clear of its own retained /cmd).
//
// Every method runs on the single jobs worker, so there is no lock and
// nothing is published from a paho handler.
package engine

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"codeberg.org/kgbvax/stationa/shared/antmode"
	"codeberg.org/kgbvax/stationa/shared/logging"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"antmode/internal/config"
	"antmode/internal/dwd"
	"antmode/internal/heading"
	"antmode/internal/nina"
	"antmode/internal/policy"
)

// Publisher publishes one MQTT message and fails fast while the link is down.
type Publisher interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
}

// Saver persists the engine state (internal/store.File).
type Saver interface {
	Save(v any) error
}

// Phase is the lock sequencing phase.
type Phase string

const (
	Idle            Phase = "idle"
	Grounding       Phase = "grounding"
	Retracting      Phase = "retracting"
	AwaitingPower   Phase = "awaiting_power"
	AwaitingDevices Phase = "awaiting_devices"
	Rotating        Phase = "rotating"
	Parked          Phase = "parked"
	Failed          Phase = "failed"
)

var phases = []Phase{Idle, Grounding, Retracting, AwaitingPower, AwaitingDevices, Rotating, Parked, Failed}

func (p Phase) valid() bool {
	for _, v := range phases {
		if p == v {
			return true
		}
	}
	return false
}

const (
	// StartupHold: after the first broker connect, wait this long for the
	// retained replays (own /state when there is no state file, powerseq,
	// the siblings) before deciding or publishing /state.
	StartupHold = 3 * time.Second
	// cmdMaxAge / cmdMaxFuture gate a /cmd carrying ts.
	cmdMaxAge    = 30 * time.Second
	cmdMaxFuture = 5 * time.Second
	// stateVersion is the persisted file format.
	stateVersion = 1
)

// Borrow is an antenna-power borrow in progress (persisted).
type Borrow struct {
	// RestoreTo is the power state to put back ("off").
	RestoreTo string `json:"restore_to"`
	// PowerSeqPhase is powerseq's phase when the borrow started; the restore
	// only happens while it is unchanged (else powerseq owns the power now).
	PowerSeqPhase string    `json:"powerseq_phase"`
	Since         time.Time `json:"since"`
	// Sent: set_power on went out.
	Sent bool `json:"sent"`
	// Restore: the restore is due (mode left, maintenance ended).
	Restore bool `json:"restore"`
}

// Persisted is the state file content.
type Persisted struct {
	Version int           `json:"version"`
	Policy  policy.State  `json:"policy"`
	Lock    *antmode.Lock `json:"lock,omitempty"`
	Phase   Phase         `json:"phase"`
	Fault   string        `json:"fault,omitempty"`
	Borrow  *Borrow       `json:"borrow,omitempty"`
}

// sibling is the two-layer liveness of one slot plus its parsed /state.
type sibling struct {
	status  bool // /status online
	device  bool // /state.device_online (absent = true)
	seen    bool // a /state arrived
	az      float64
	azKnown bool
	moving  bool
	// ant-ctrl
	retracted bool
	// ant-switch
	selected string
	// radio
	tx bool
	// antenna power
	power string
}

func (s *sibling) live() bool { return s.status && s.device }

// Engine is the antmode state machine.
type Engine struct {
	cfg   config.Config
	pub   Publisher
	saver Saver
	log   *slog.Logger
	now   func() time.Time

	self     string
	cmdTopic string
	powerCmd string

	pol    *policy.Policy
	lock   *antmode.Lock
	phase  Phase
	fault  string
	borrow *Borrow

	phaseSince  time.Time
	borrowClock time.Time // in-memory start of the awaiting_* timeout
	groundTXLog bool      // "ground timeout held by TX" logged for this phase
	drift       *int
	driftWarned bool

	connected    bool
	firstConnect time.Time
	ready        bool
	adopt        bool // no state file: adopt the retained own /state
	pendingCmds  [][]byte

	seqStatus bool
	seqPhase  string
	rot       sibling
	ant       sibling
	sw        sibling
	radio     sibling
	pwr       sibling

	warnings []nina.Warning // last successful poll (all qualifying)
	warnOK   bool           // the last poll succeeded
	wind     *dwd.Wind
	windAt   time.Time

	lastState     string
	lastPersisted string
	persistFail   logging.Streak
	pubFail       logging.Streak
}

// New builds the engine over a loaded state (nil when there is no state
// file: the engine then adopts the retained own /state during the startup
// hold, so a lost file never unlocks the station). now may be nil.
func New(cfg config.Config, pub Publisher, saver Saver, loaded *Persisted, log *slog.Logger, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	m := cfg.MQTT
	st, sl := config.SplitSlot(cfg.Slots.AntennaPower)
	e := &Engine{
		cfg:      cfg,
		pub:      pub,
		saver:    saver,
		log:      log,
		now:      now,
		self:     schema.SlotBase(m.Site, m.Station, m.Slot),
		cmdTopic: schema.CmdTopic(m.Site, m.Station, m.Slot),
		powerCmd: schema.CmdTopic(m.Site, st, sl),
		phase:    Idle,
	}
	pcfg := policy.Config{AutoEnter: cfg.Storm.AutoEnter, ExitHoldoff: time.Duration(cfg.Storm.ExitHoldoffMinutes) * time.Minute}
	if loaded == nil {
		e.adopt = true
		// Seq is opaque to the bridges (equality only); seeding a fresh state
		// from the clock keeps it from repeating an old value.
		e.pol = policy.New(pcfg, policy.State{Seq: uint64(now().Unix())})
	} else {
		e.pol = policy.New(pcfg, loaded.Policy)
		e.lock, e.phase, e.fault, e.borrow = loaded.Lock, loaded.Phase, loaded.Fault, loaded.Borrow
		e.normalizeLoaded()
	}
	return e
}

// normalizeLoaded repairs an inconsistent loaded state toward safety: a
// locked mode always has a lock (ground at least), normal never has one.
func (e *Engine) normalizeLoaded() {
	mode := e.pol.S.Mode
	if !mode.Valid() {
		e.log.Error("state file has an unknown ant_mode; keeping normal", "ant_mode", mode)
		e.pol.S.Mode = antmode.Normal
		mode = antmode.Normal
	}
	if mode == antmode.Normal {
		e.lock, e.phase = nil, Idle
		if e.borrow != nil {
			e.borrow.Restore = true // left normal mid-borrow before the crash
		}
		return
	}
	if e.lock == nil {
		e.lock = &antmode.Lock{Seq: e.pol.S.Seq}
	}
	e.lock.Ground = true
	e.lock.Nudge = mode == antmode.Maintenance
	if !e.phase.valid() || e.phase == Idle {
		e.phase = Grounding
	}
	if e.borrow != nil {
		e.borrowClock = e.now()
	}
	e.log.Info("restored locked state", "ant_mode", mode, "seq", e.lock.Seq, "phase", e.phase,
		"borrow", e.borrow != nil)
}

// ---------------------------------------------------------------------------
// connection and startup
// ---------------------------------------------------------------------------

// SetConnected tracks the broker link.
func (e *Engine) SetConnected(on bool) {
	e.connected = on
	if on && e.firstConnect.IsZero() {
		e.firstConnect = e.now()
	}
}

// Ready reports whether the startup hold is over.
func (e *Engine) Ready() bool { return e.ready }

func (e *Engine) becomeReady() {
	e.ready = true
	e.adopt = false
	e.phaseSince = e.now()
	if e.borrow != nil {
		e.borrowClock = e.now()
	}
	e.log.Info("startup hold over", "ant_mode", e.pol.S.Mode, "phase", e.phase,
		"presence", e.presence(), "pending_cmds", len(e.pendingCmds))
	cmds := e.pendingCmds
	e.pendingCmds = nil
	for _, c := range cmds {
		e.applyCmd(c)
	}
	e.step()
	e.persist()
	e.publishState(true)
}

// OwnState is the retained own /state replay. Read only while adopting (no
// state file at start): a locked snapshot is taken over so that losing the
// file never unlocks the station.
func (e *Engine) OwnState(p []byte) {
	if !e.adopt || e.ready {
		return
	}
	st, err := antmode.Parse(p)
	if err != nil {
		e.log.Error("own retained /state is malformed; not adopted", "err", err)
		return
	}
	if !st.Locked() {
		return
	}
	var extra struct {
		Source string `json:"source"`
		Since  string `json:"since"`
		Phase  Phase  `json:"phase"`
	}
	_ = json.Unmarshal(p, &extra)
	l := st.L()
	e.pol.S.Mode, e.pol.S.Seq = st.Mode, l.Seq
	e.pol.S.Source = policy.Manual
	if extra.Source == string(policy.Auto) {
		e.pol.S.Source = policy.Auto
	}
	if t, err := time.Parse(time.RFC3339, extra.Since); err == nil {
		e.pol.S.Since = t
	}
	e.pol.S.Reason = "adopted from the retained /state (no state file)"
	e.lock = &l
	e.phase = extra.Phase
	e.normalizeLoaded()
	e.log.Warn("no state file: adopted the retained lock from the broker",
		"ant_mode", st.Mode, "seq", l.Seq, "phase", e.phase)
}

// ---------------------------------------------------------------------------
// bus inputs
// ---------------------------------------------------------------------------

func online(p []byte) bool { return strings.EqualFold(strings.TrimSpace(string(p)), "online") }

// deviceOnline reads /state.device_online; a slot that omits it is judged by
// /status alone.
func deviceOnline(v *bool) bool { return v == nil || *v }

func (e *Engine) PowerSeqStatus(p []byte) { e.seqStatus = online(p); e.inputChanged() }

func (e *Engine) PowerSeqState(p []byte) {
	var s struct {
		Phase string `json:"phase"`
	}
	if len(strings.TrimSpace(string(p))) == 0 {
		e.seqPhase = ""
	} else if err := json.Unmarshal(p, &s); err != nil {
		e.log.Warn("bad power-seq state", "err", err)
		return
	} else {
		e.seqPhase = s.Phase
	}
	e.inputChanged()
}

func (e *Engine) RotatorStatus(p []byte) { e.rot.status = online(p); e.inputChanged() }

func (e *Engine) RotatorState(p []byte) {
	var s struct {
		Az           *float64 `json:"az"`
		Moving       bool     `json:"moving"`
		DeviceOnline *bool    `json:"device_online"`
	}
	if err := json.Unmarshal(p, &s); err != nil {
		e.log.Warn("bad rotator state", "err", err)
		return
	}
	if s.Az != nil {
		e.rot.az, e.rot.azKnown = *s.Az, true
	}
	e.rot.moving, e.rot.device, e.rot.seen = s.Moving, deviceOnline(s.DeviceOnline), true
	e.inputChanged()
}

func (e *Engine) AntCtrlStatus(p []byte) { e.ant.status = online(p); e.inputChanged() }

func (e *Engine) AntCtrlState(p []byte) {
	var s struct {
		Retracted    bool  `json:"retracted"`
		Moving       bool  `json:"moving"`
		DeviceOnline *bool `json:"device_online"`
	}
	if err := json.Unmarshal(p, &s); err != nil {
		e.log.Warn("bad ant-ctrl state", "err", err)
		return
	}
	e.ant.retracted, e.ant.moving, e.ant.device, e.ant.seen = s.Retracted, s.Moving, deviceOnline(s.DeviceOnline), true
	e.inputChanged()
}

func (e *Engine) AntSwitchStatus(p []byte) { e.sw.status = online(p); e.inputChanged() }

func (e *Engine) AntSwitchState(p []byte) {
	var s struct {
		Selected     string `json:"selected"`
		DeviceOnline *bool  `json:"device_online"`
	}
	if err := json.Unmarshal(p, &s); err != nil {
		e.log.Warn("bad ant-switch state", "err", err)
		return
	}
	e.sw.selected, e.sw.device, e.sw.seen = s.Selected, deviceOnline(s.DeviceOnline), true
	e.inputChanged()
}

func (e *Engine) RadioStatus(p []byte) { e.radio.status = online(p); e.inputChanged() }

func (e *Engine) RadioState(p []byte) {
	var s struct {
		TX           string `json:"tx"`
		DeviceOnline *bool  `json:"device_online"`
	}
	if err := json.Unmarshal(p, &s); err != nil {
		e.log.Warn("bad radio state", "err", err)
		return
	}
	e.radio.tx, e.radio.device, e.radio.seen = s.TX == "tx", deviceOnline(s.DeviceOnline), true
	e.inputChanged()
}

func (e *Engine) PowerStatus(p []byte) { e.pwr.status = online(p); e.inputChanged() }

func (e *Engine) PowerState(p []byte) {
	var s struct {
		Power        string `json:"power"`
		DeviceOnline *bool  `json:"device_online"`
	}
	if err := json.Unmarshal(p, &s); err != nil {
		e.log.Warn("bad antenna power state", "err", err)
		return
	}
	e.pwr.power, e.pwr.device, e.pwr.seen = s.Power, deviceOnline(s.DeviceOnline), true
	e.inputChanged()
}

func (e *Engine) inputChanged() {
	if !e.ready {
		return
	}
	e.step()
	e.persist()
	e.publishState(false)
}

// NinaResult records one warning poll (err != nil: the poll failed, the
// warnings are unknown — the last known list is kept, nothing counts).
func (e *Engine) NinaResult(ws []nina.Warning, err error) {
	now := e.now()
	if !e.ready {
		// Before the startup hold the mode may still be adopted; keep the
		// result (step uses it for entry) but leave the quiet-time
		// accounting to the next poll.
		if err == nil {
			e.warnings = ws
		}
		e.warnOK = err == nil
		return
	}
	if err != nil {
		e.warnOK = false
		e.pol.Poll(now, false, nil)
	} else {
		e.warnings, e.warnOK = ws, true
		r := e.pol.Poll(now, true, nina.Active(ws, now))
		if r.ModeChanged {
			e.log.Info("ant_mode changed", "from", r.From, "to", e.pol.S.Mode, "source", e.pol.S.Source,
				"reason", e.pol.S.Reason)
			e.modeChanged(r.From)
		}
	}
	e.inputChanged()
}

// WindResult records one wind poll (err != nil: keep the last value; it
// goes stale on its own).
func (e *Engine) WindResult(w dwd.Wind, err error) {
	if err == nil {
		e.wind, e.windAt = &w, e.now()
	}
	e.inputChanged()
}

// Tick runs once a second: the startup hold, timeouts, warnings becoming
// active or expiring by the clock, retries of failed publishes.
func (e *Engine) Tick() {
	if !e.ready {
		if e.connected && !e.firstConnect.IsZero() && !e.now().Before(e.firstConnect.Add(StartupHold)) {
			e.becomeReady()
		}
		return
	}
	e.step()
	e.persist()
	e.publishState(false)
}

// ---------------------------------------------------------------------------
// /cmd
// ---------------------------------------------------------------------------

// Cmd handles the own /cmd. Every non-empty payload is one-shot: it is
// applied (or dropped) and then the retained topic is cleared, so Home
// Assistant's retained select never replays a stale mode on restart. The
// empty payload of the clear comes back and is ignored here.
func (e *Engine) Cmd(p []byte) {
	if len(strings.TrimSpace(string(p))) == 0 {
		return
	}
	if err := e.pub.Publish(e.cmdTopic, 1, true, []byte{}); err != nil {
		e.log.Warn("retained /cmd not cleared", "err", err)
	}
	if !e.ready {
		// Before the startup hold ends the mode may still be adopted from
		// the broker; queue so the cmd applies on top of it.
		if len(e.pendingCmds) < 8 {
			e.pendingCmds = append(e.pendingCmds, append([]byte(nil), p...))
		}
		return
	}
	e.applyCmd(p)
	e.step() // a phase may already be satisfied (repark with the elements in)
	e.persist()
	e.publishState(false)
}

func (e *Engine) applyCmd(p []byte) {
	var c struct {
		Action string          `json:"action"`
		Value  json.RawMessage `json:"value"`
		TS     string          `json:"ts"`
	}
	if err := json.Unmarshal(p, &c); err != nil {
		e.log.Warn("bad /cmd", "err", err)
		return
	}
	now := e.now()
	if c.TS != "" {
		ts, err := time.Parse(time.RFC3339, c.TS)
		switch {
		case err != nil:
			e.log.Warn("/cmd dropped: bad ts", "ts", c.TS, "action", c.Action)
			return
		case now.Sub(ts) > cmdMaxAge:
			e.log.Warn("/cmd dropped: stale", "ts", c.TS, "action", c.Action)
			return
		case ts.Sub(now) > cmdMaxFuture:
			e.log.Warn("/cmd dropped: ts in the future", "ts", c.TS, "action", c.Action)
			return
		}
	}
	active := nina.Active(e.warnings, now)
	switch c.Action {
	case "set_ant_mode":
		var v string
		if err := json.Unmarshal(c.Value, &v); err != nil {
			e.log.Warn("set_ant_mode: value must be a string", "value", string(c.Value))
			return
		}
		m := antmode.Mode(strings.ToLower(strings.TrimSpace(v)))
		r, err := e.pol.SetMode(m, now, active)
		if err != nil {
			e.log.Warn("set_ant_mode refused", "err", err)
			return
		}
		if !r.ModeChanged {
			e.log.Info("set_ant_mode: already in this mode (use repark to park again)", "ant_mode", m)
			return
		}
		e.log.Info("ant_mode changed", "from", r.From, "to", m, "source", policy.Manual, "reason", e.pol.S.Reason)
		e.modeChanged(r.From)
	case "repark":
		if r := e.pol.Repark(); !r.Reparked {
			e.log.Info("repark ignored in normal")
			return
		}
		e.log.Info("repark", "ant_mode", e.pol.S.Mode, "seq", e.pol.S.Seq)
		e.restartPark()
	case "dismiss_suggestion":
		if r := e.pol.Dismiss(active); r.Changed {
			e.log.Info("storm suggestion dismissed", "warnings", len(active))
		}
	default:
		e.log.Warn("unknown /cmd action", "action", c.Action)
	}
}

// ---------------------------------------------------------------------------
// mode and lock sequencing
// ---------------------------------------------------------------------------

func (e *Engine) presence() policy.Presence { return policy.PresenceOf(e.seqStatus, e.seqPhase) }

func (e *Engine) transmitting() bool { return e.radio.live() && e.radio.tx }

func (e *Engine) setPhase(p Phase) {
	if p == e.phase {
		return
	}
	e.log.Info("phase", "from", e.phase, "to", p, "ant_mode", e.pol.S.Mode)
	e.phase, e.phaseSince, e.groundTXLog = p, e.now(), false
}

func (e *Engine) fail(fault string) {
	e.fault = fault
	e.log.Warn("park failed", "ant_mode", e.pol.S.Mode, "fault", fault)
	e.setPhase(Failed)
}

// modeChanged applies a policy mode change to the lock.
func (e *Engine) modeChanged(from antmode.Mode) {
	to := e.pol.S.Mode
	e.drift, e.driftWarned, e.fault = nil, false, ""
	switch {
	case to == antmode.Normal:
		e.lock = nil
		e.setPhase(Idle)
		if e.borrow != nil {
			e.borrow.Restore = true // abort (mid-borrow) or maintenance ended
		}
	case from == antmode.Normal:
		e.lock = &antmode.Lock{Seq: e.pol.S.Seq, Ground: true, Nudge: to == antmode.Maintenance}
		e.setPhase(Grounding)
		e.startBorrowIfNeeded()
	default:
		e.lock.Nudge = to == antmode.Maintenance
		e.restartPark()
	}
}

// restartPark re-runs the park on the current lock with the new seq (repark,
// or a switch between storm and maintenance). Ground and retract stay in
// force; the park azimuth is computed again.
func (e *Engine) restartPark() {
	e.lock.Seq = e.pol.S.Seq
	e.lock.ParkAz = nil
	e.drift, e.driftWarned, e.fault = nil, false, ""
	if e.lock.Retract {
		e.setPhase(Retracting)
	} else {
		e.setPhase(Grounding)
	}
	e.phaseSince = e.now()
	if e.borrow != nil {
		e.borrow.Restore = false // the power is needed again
		if !e.pwrOn() {
			e.borrow.Sent = false // switched off meanwhile: send on again
			e.setPhase(AwaitingPower)
			e.borrowClock = e.now()
		}
		return
	}
	e.startBorrowIfNeeded()
}

func (e *Engine) pwrOn() bool { return e.pwr.live() && e.pwr.power == "on" }

// startBorrowIfNeeded switches antenna power on when it is off at entry, in a
// stable powerseq phase only. restore_to is persisted BEFORE the retained
// set_power on goes out, and /state carries power_hold before it, so powerseq
// never reads the power-up as activity and a crash still restores.
func (e *Engine) startBorrowIfNeeded() {
	if !e.cfg.Power.Borrow || e.borrow != nil {
		return
	}
	if !e.pwr.live() || e.pwr.power != "off" {
		return // already on, or unknown: nothing to borrow
	}
	if !e.seqStatus {
		e.log.Info("antenna power off, but powerseq is offline: not borrowing")
		return
	}
	switch e.seqPhase {
	case "idle", "standby", "running":
	default:
		e.log.Info("antenna power off, but powerseq is in a transition: not borrowing", "powerseq_phase", e.seqPhase)
		return
	}
	prev := e.phase
	e.borrow = &Borrow{RestoreTo: "off", PowerSeqPhase: e.seqPhase, Since: e.now()}
	e.setPhase(AwaitingPower)
	if err := e.saver.Save(e.snapshot()); err != nil {
		e.log.Error("not borrowing antenna power: restore_to could not be persisted", "err", err)
		e.borrow = nil
		e.setPhase(prev)
		return
	}
	e.lastPersisted = ""
	e.borrowClock = e.now()
	e.log.Info("borrowing antenna power", "powerseq_phase", e.seqPhase)
	e.publishState(false)
	e.sendPowerOn()
}

func (e *Engine) sendPowerOn() {
	if err := e.publishJSON(e.powerCmd, 1, true, map[string]string{"action": "set_power", "value": "on"}); err != nil {
		e.pubFail.Fail(e.log, "antenna power on not sent; retrying", "err", err)
		return
	}
	e.pubFail.Reset()
	e.borrow.Sent = true
}

// step advances the lock sequencing and the power borrow; it also runs the
// automatic storm entry (presence or the clock may have changed).
func (e *Engine) step() {
	now := e.now()
	if r := e.pol.Evaluate(now, nina.Active(e.warnings, now), e.presence(), false); r.ModeChanged {
		e.log.Info("ant_mode changed", "from", r.From, "to", e.pol.S.Mode, "source", e.pol.S.Source,
			"reason", e.pol.S.Reason)
		e.modeChanged(r.From)
	}
	if e.lock != nil {
		e.stepLock(now)
	}
	e.stepBorrow()
}

func (e *Engine) stepLock(now time.Time) {
	p := e.cfg.Park
	elapsed := now.Sub(e.phaseSince)
	switch e.phase {
	case AwaitingPower:
		if e.borrow == nil {
			e.setPhase(Grounding)
			return
		}
		if !e.borrow.Sent {
			e.sendPowerOn()
		}
		if e.pwrOn() {
			e.setPhase(AwaitingDevices)
		} else if e.borrowTimedOut(now) {
			// Go on building the lock anyway: if the gear comes up later the
			// bridges must find retract and park_az already there. The
			// remaining timeouts end in failed (and the storm restore).
			e.log.Warn("antenna power did not come on; continuing the lock without it")
			e.setPhase(Grounding)
		}
	case AwaitingDevices:
		if e.rot.live() && e.ant.live() {
			e.setPhase(Grounding)
		} else if e.borrowTimedOut(now) {
			e.log.Warn("rotator/ant-ctrl not online after antenna power on; continuing the lock",
				"timeout_s", e.cfg.Power.DevicesTimeoutS, "rotator_live", e.rot.live(), "ant_ctrl_live", e.ant.live())
			e.setPhase(Grounding)
		}
	case Grounding:
		if e.sw.live() && e.sw.selected == "off" {
			e.lock.Retract = true
			e.setPhase(Retracting)
			return
		}
		if elapsed < time.Duration(p.GroundTimeoutS)*time.Second {
			return
		}
		if e.transmitting() {
			if !e.groundTXLog {
				e.log.Warn("ground not confirmed and the radio is transmitting: holding the retract")
				e.groundTXLog = true
			}
			return
		}
		e.log.Warn("ant-switch did not report off in time; retracting anyway",
			"switch_live", e.sw.live(), "selected", e.sw.selected)
		e.lock.Retract = true
		e.setPhase(Retracting)
	case Retracting:
		if e.ant.live() && e.ant.retracted && !e.ant.moving {
			e.computePark()
			return
		}
		if elapsed >= time.Duration(p.RetractTimeoutS)*time.Second {
			e.log.Warn("ant-ctrl did not confirm retracted in time; parking anyway",
				"ant_ctrl_live", e.ant.live(), "retracted", e.ant.retracted, "moving", e.ant.moving)
			e.computePark()
		}
	case Rotating:
		if e.atPark() {
			e.parked()
		} else if elapsed >= time.Duration(p.RotateTimeoutS)*time.Second {
			e.fail(fmt.Sprintf("rotator did not reach park_az %d within %ds (%s)",
				*e.lock.ParkAz, p.RotateTimeoutS, e.rotDesc()))
		}
	case Parked:
		e.updateDrift()
	case Failed:
		if e.lock.ParkAz != nil && e.atPark() {
			e.log.Info("rotator reached park_az after the timeout")
			e.parked()
		}
	}
}

func (e *Engine) borrowTimedOut(now time.Time) bool {
	return now.Sub(e.borrowClock) >= time.Duration(e.cfg.Power.DevicesTimeoutS)*time.Second
}

func (e *Engine) rotDesc() string {
	if !e.rot.live() || !e.rot.azKnown {
		return "rotator offline"
	}
	return fmt.Sprintf("az %.0f, moving %v", e.rot.az, e.rot.moving)
}

// computePark sets lock.park_az: storm → the wind heading, maintenance →
// the preset.
func (e *Engine) computePark() {
	var az int
	if e.pol.S.Mode == antmode.Storm {
		in := heading.Inputs{
			WindOK: e.windFresh(), Az: e.rot.az, AzOK: e.rot.live() && e.rot.azKnown,
			MaxAz: e.cfg.Storm.MaxAz, FallbackAz: e.cfg.Storm.FallbackAz,
		}
		if e.wind != nil {
			in.WindDeg = e.wind.DirDeg
		}
		az = heading.Storm(in)
		e.log.Info("storm park", "park_az", az, "wind_ok", in.WindOK, "wind_deg", in.WindDeg,
			"az", in.Az, "az_ok", in.AzOK)
	} else {
		az = e.cfg.Maintenance.ParkAz
		e.log.Info("maintenance park", "park_az", az)
	}
	e.lock.Retract = true
	e.lock.ParkAz = &az
	e.setPhase(Rotating)
}

func (e *Engine) atPark() bool {
	if e.lock == nil || e.lock.ParkAz == nil || !e.rot.live() || !e.rot.azKnown || e.rot.moving {
		return false
	}
	return math.Abs(e.rot.az-float64(*e.lock.ParkAz)) <= float64(e.cfg.Park.ToleranceDeg)
}

func (e *Engine) parked() {
	e.fault = ""
	e.setPhase(Parked)
	e.updateDrift()
}

// updateDrift records the readback offset while parked (no re-aim: the
// G-450 has no brake, turning in gusts is the worst case). Warn once per
// excursion beyond the threshold.
func (e *Engine) updateDrift() {
	if e.lock.ParkAz == nil || !e.rot.live() || !e.rot.azKnown {
		e.drift = nil
		return
	}
	d := int(math.Round(e.rot.az - float64(*e.lock.ParkAz)))
	e.drift = &d
	over := abs(d) > e.cfg.Park.DriftWarnDeg
	if over && !e.driftWarned {
		e.log.Warn("parked rotator drifted (not re-aiming)", "drift_deg", d, "park_az", *e.lock.ParkAz, "az", e.rot.az)
	}
	e.driftWarned = over
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// stepBorrow restores borrowed antenna power when it is due: storm once
// parked or failed; maintenance and an aborted borrow once the mode is left.
// Only while powerseq's phase is unchanged since the borrow started —
// otherwise powerseq has taken over the power and it is left alone.
func (e *Engine) stepBorrow() {
	b := e.borrow
	if b == nil {
		return
	}
	due := b.Restore || (e.pol.S.Mode == antmode.Storm && (e.phase == Parked || e.phase == Failed))
	if !due {
		return
	}
	if !e.seqStatus || e.seqPhase != b.PowerSeqPhase {
		e.log.Warn("powerseq phase changed during the antenna power borrow; leaving the power as it is",
			"borrow_phase", b.PowerSeqPhase, "powerseq_phase", e.seqPhase, "powerseq_online", e.seqStatus)
		e.borrow = nil
		return
	}
	// Sent even when the readback already shows restore_to: shelly-power-bridge
	// keeps the retained /cmd as its intent and replays it on every reconnect,
	// so our retained set_power on must be overwritten (an on still in flight,
	// a lost Shelly RPC, a Shelly that rebooted off after a power cut). Also
	// when on was never confirmed sent: a timed-out publish may still arrive.
	if err := e.publishJSON(e.powerCmd, 1, true, map[string]string{"action": "set_power", "value": b.RestoreTo}); err != nil {
		e.pubFail.Fail(e.log, "antenna power restore not sent; retrying", "err", err)
		return
	}
	e.pubFail.Reset()
	e.log.Info("antenna power borrow ended", "restored_to", b.RestoreTo, "ant_mode", e.pol.S.Mode)
	e.borrow = nil
}

// ---------------------------------------------------------------------------
// persistence and outputs
// ---------------------------------------------------------------------------

func (e *Engine) snapshot() Persisted {
	return Persisted{Version: stateVersion, Policy: e.pol.S, Lock: e.lock, Phase: e.phase, Fault: e.fault, Borrow: e.borrow}
}

// persist saves the state file when it changed.
func (e *Engine) persist() {
	b, err := json.Marshal(e.snapshot())
	if err != nil {
		e.log.Error("marshal state", "err", err)
		return
	}
	if string(b) == e.lastPersisted {
		return
	}
	if err := e.saver.Save(e.snapshot()); err != nil {
		e.persistFail.Fail(e.log, "state file not written", "err", err)
		return
	}
	e.persistFail.Reset()
	e.lastPersisted = string(b)
}

func (e *Engine) windFresh() bool {
	return e.wind != nil && e.now().Sub(e.windAt) <= time.Duration(e.cfg.Wind.StaleMinutes)*time.Minute
}

// Wind is the /state wind block.
type Wind struct {
	DirDeg  *float64 `json:"dir_deg,omitempty"`
	GustKmh *float64 `json:"gust_kmh,omitempty"`
	TS      string   `json:"ts,omitempty"`
	Stale   bool     `json:"stale"`
}

// Warning is one /state warnings[] entry.
type Warning struct {
	ID      string `json:"id"`
	Event   string `json:"event"`
	II      string `json:"ii"`
	Onset   string `json:"onset"`
	Expires string `json:"expires,omitempty"`
}

// State is the /state snapshot (the contract in docs/ant-mode-mqtt-api.md).
type State struct {
	TS             string        `json:"ts"`
	AntMode        antmode.Mode  `json:"ant_mode"`
	Source         string        `json:"source"`
	Since          string        `json:"since,omitempty"`
	Lock           *antmode.Lock `json:"lock,omitempty"`
	Phase          Phase         `json:"phase"`
	Fault          string        `json:"fault,omitempty"`
	PowerHold      bool          `json:"power_hold"`
	Presence       string        `json:"presence"`
	Wind           Wind          `json:"wind"`
	Warnings       []Warning     `json:"warnings"`
	WarningsOK     bool          `json:"warnings_ok"`
	StormSuggested bool          `json:"storm_suggested"`
	Reason         string        `json:"reason,omitempty"`
	AutoExitAt     string        `json:"auto_exit_at,omitempty"`
	DriftDeg       *int          `json:"drift_deg,omitempty"`
}

func rfc(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// CurrentState builds the /state snapshot (without ts).
func (e *Engine) CurrentState() State {
	now := e.now()
	active := nina.Active(e.warnings, now)
	pres := e.presence()
	st := State{
		AntMode:    e.pol.S.Mode,
		Source:     string(e.pol.S.Source),
		Since:      rfc(e.pol.S.Since),
		Phase:      e.phase,
		Fault:      e.fault,
		PowerHold:  e.borrow != nil,
		Presence:   string(pres),
		Warnings:   []Warning{},
		WarningsOK: e.warnOK,
		Reason:     e.pol.S.Reason,
		AutoExitAt: rfc(e.pol.AutoExitAt()),
	}
	if e.lock != nil {
		l := *e.lock
		st.Lock = &l
		st.DriftDeg = e.drift
	}
	st.Wind.Stale = !e.windFresh()
	if e.wind != nil {
		d, g := e.wind.DirDeg, e.wind.GustKmh
		st.Wind.DirDeg, st.Wind.GustKmh, st.Wind.TS = &d, &g, rfc(e.windAt)
	}
	for _, w := range active {
		st.Warnings = append(st.Warnings, Warning{ID: w.ID, Event: w.Event, II: w.II, Onset: rfc(w.Onset), Expires: rfc(w.Expires)})
	}
	if e.pol.Suggested(active, pres) {
		st.StormSuggested = true
		st.Reason = policy.SuggestReason(active, pres)
	}
	return st
}

// publishState publishes /state when it changed (force: on every reconnect).
// Nothing is published before the startup hold ends: an early normal could
// unlock the bridges ahead of an adopted lock.
func (e *Engine) publishState(force bool) {
	if !e.ready || !e.connected {
		return
	}
	st := e.CurrentState()
	key, err := json.Marshal(st)
	if err != nil {
		e.log.Error("marshal /state", "err", err)
		return
	}
	if !force && string(key) == e.lastState {
		return
	}
	st.TS = rfc(e.now())
	if err := e.publishJSON(e.self+"/state", 1, true, st); err != nil {
		e.log.Debug("state not published", "err", err)
		return
	}
	e.lastState = string(key)
}

// Republish re-sends /meta and /state (on every broker (re)connect).
func (e *Engine) Republish() {
	e.publishMeta()
	e.publishState(true)
}

func (e *Engine) publishMeta() {
	modes := make([]string, 0, len(antmode.Modes))
	for _, m := range antmode.Modes {
		modes = append(modes, string(m))
	}
	phaseOpts := make([]string, 0, len(phases))
	for _, p := range phases {
		phaseOpts = append(phaseOpts, string(p))
	}
	s := e.cfg.Slots
	meta := map[string]any{
		"schema":   "1.0",
		"role":     "ant-mode",
		"link":     "none",
		"location": e.cfg.Location,
		"host":     e.cfg.Host,
		"capabilities": map[string]any{
			"modes":                modes,
			"reads":                []string{s.PowerSeq, s.Rotator, s.AntCtrl, s.AntSwitch, s.Radio, s.AntennaPower},
			"locks":                []string{s.Rotator, s.AntCtrl, s.AntSwitch},
			"actions":              []string{"set_ant_mode", "repark", "dismiss_suggestion"},
			"auto_enter":           e.cfg.Storm.AutoEnter,
			"power_borrow":         e.cfg.Power.Borrow,
			"ii_codes":             e.cfg.NINA.IICodes,
			"max_az":               e.cfg.Storm.MaxAz,
			"fallback_az":          e.cfg.Storm.FallbackAz,
			"maintenance_az":       e.cfg.Maintenance.ParkAz,
			"exit_holdoff_minutes": e.cfg.Storm.ExitHoldoffMinutes,
		},
		"expose": map[string]any{
			"device": map[string]any{"name": "Antenna mode"},
			"fields": []map[string]any{
				{"key": "ant_mode", "name": "Antenna mode", "type": "enum", "options": modes, "writable": true,
					"command": map[string]any{"action": "set_ant_mode", "value_key": "value", "value_type": "string"}},
				{"key": "storm_suggested", "name": "Storm suggested", "type": "boolean"},
				{"key": "phase", "name": "Park phase", "type": "enum", "options": phaseOpts},
				{"key": "source", "name": "Mode source", "type": "enum", "options": []string{"manual", "auto"}},
				{"key": "presence", "name": "Presence", "type": "enum", "options": []string{"manned", "unmanned"}},
			},
			"actions": []map[string]any{
				{"key": "repark", "name": "Re-park", "command": map[string]any{"action": "repark"}},
				{"key": "dismiss_suggestion", "name": "Dismiss storm suggestion", "command": map[string]any{"action": "dismiss_suggestion"}},
			},
		},
	}
	if err := e.publishJSON(e.self+"/meta", 1, true, meta); err != nil {
		e.log.Debug("meta not published", "err", err)
	}
}

func (e *Engine) publishJSON(topic string, qos byte, retained bool, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		e.log.Error("marshal", "topic", topic, "err", err)
		return err
	}
	return e.pub.Publish(topic, qos, retained, b)
}
