// SPDX-License-Identifier: AGPL-3.0-or-later

// Package engine holds beamsteer's runtime state and turns logger rotate
// requests into rotator and Ultrabeam commands.
//
// Every method except Readback runs on the single jobs worker (the mqtt
// layer and the UDP handler Enqueue onto it), so the engine state needs no
// lock and publishing never happens on a paho dispatch goroutine. Readback is
// called straight from the UDP read loop, so the heading it reports is kept
// under its own mutex.
package engine

import (
	"encoding/json"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"beamsteer/internal/config"
	"beamsteer/internal/steer"
)

// Publisher publishes one MQTT message. It must fail fast (not queue) while
// the broker link is down: a sibling /cmd replayed after a reconnect would
// move the mast to a stale target, possibly after the operator's STOP.
type Publisher interface {
	Publish(topic string, qos byte, retained bool, payload []byte) error
}

// How long a command beamsteer sent is trusted over the sibling's /state.
// ultrabridge republishes /state only on its 2 s poll, after a serial
// exchange of up to 5 s; the WRC reports a new target within a frame or two.
// Until the sibling confirms (or the window lapses), decisions use what was
// commanded — not the stale /state — and a second flip is held.
const (
	dirSettle       = 15 * time.Second  // direction cmd → ant-ctrl confirms
	rotAckWindow    = 10 * time.Second  // set_az → rotator shows our target or starts moving
	rotTravelWindow = 120 * time.Second // acknowledged set_az → arrival (G-450 full sweep ~60 s)
	arriveTolerance = 3.0               // degrees: rotator "at" the commanded target
)

// Last is the most recent logger request and what beamsteer did with it.
type Last struct {
	Bearing   float64  `json:"bearing"`
	RotateTo  *float64 `json:"rotate_to,omitempty"`
	Direction string   `json:"direction,omitempty"`
	Reason    string   `json:"reason"`
	TS        string   `json:"ts"`
}

// heldFlip is a direction change waiting for TX, element travel or an
// earlier flip to finish — with the bearing it was decided for, so it is
// decided again (not replayed blindly) when it is free to go.
type heldFlip struct {
	dir     string
	bearing float64
}

// Engine is the beamsteer state machine.
type Engine struct {
	cfg config.Config
	pub Publisher
	log *slog.Logger
	now func() time.Time

	self      string
	rotCmd    string
	antCmd    string
	enabled   bool
	connected bool

	rotStatus, rotDevice bool
	az                   float64
	azKnown              bool
	targetAz             *float64
	rotMoving            bool

	antStatus, antDevice bool
	direction            string
	antMoving            bool
	antBand              string

	radioStatus, radioDevice bool
	band                     string
	tx                       bool

	// Direction command in flight (see dirSettle): trusted until ant-ctrl
	// reports it at rest, reports some third direction at rest (set by
	// someone else), or the window lapses.
	cmdDir      string
	cmdDirFrom  string
	cmdDirUntil time.Time

	// set_az in flight. Acknowledged when the rotator shows our target, or
	// starts moving from rest after the command. Retired on arrival, when the
	// rotator stops after moving (short of the target: a STOP from anywhere,
	// a limit — its /state az is the truth then), on a retarget by someone
	// else, on our own Stop, or when the window lapses.
	cmdAz           *float64
	cmdAzUntil      time.Time
	cmdAzAcked      bool
	cmdAzMoved      bool
	cmdAzFromMoving bool

	pending *heldFlip
	last    *Last

	// Input liveness last published, so a change republishes /state.
	lastInputs inputs

	hmu       sync.Mutex
	heading   float64
	headingOK bool
}

// New builds an engine. now may be nil (time.Now).
func New(cfg config.Config, pub Publisher, log *slog.Logger, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	m := cfg.MQTT
	return &Engine{
		cfg:    cfg,
		pub:    pub,
		log:    log,
		now:    now,
		self:   schema.SlotBase(m.Site, m.Station, m.Slot),
		rotCmd: schema.CmdTopic(m.Site, m.Station, cfg.Steer.RotatorSlot),
		antCmd: schema.CmdTopic(m.Site, m.Station, cfg.Steer.AntCtrlSlot),
	}
}

// SelfBase is this slot's address.
func (e *Engine) SelfBase() string { return e.self }

// ---------------------------------------------------------------------------
// bus inputs
// ---------------------------------------------------------------------------

// SetConnected tracks the broker link (OnConnect / OnConnectionLost).
// Requests while it is down are dropped with a reason, not "sent".
func (e *Engine) SetConnected(on bool) { e.connected = on }

// SetEnabled applies the operator toggle (own /cmd).
func (e *Engine) SetEnabled(on bool) {
	if e.enabled == on {
		return
	}
	e.enabled = on
	e.pending = nil
	e.last = nil // a decision from before the toggle would read as current
	e.log.Info("smart rotation", "enabled", on)
	e.publishState()
}

func online(p []byte) bool { return strings.EqualFold(strings.TrimSpace(string(p)), "online") }

// deviceOnline reads /state.device_online; a bridge that omits it is judged
// by /status alone.
func deviceOnline(v *bool) bool { return v == nil || *v }

func (e *Engine) RotatorStatus(p []byte) { e.rotStatus = online(p); e.inputsChanged() }

func (e *Engine) RotatorState(p []byte) {
	var s struct {
		Az           *float64 `json:"az"`
		TargetAz     *float64 `json:"target_az"`
		Moving       bool     `json:"moving"`
		DeviceOnline *bool    `json:"device_online"`
	}
	if err := json.Unmarshal(p, &s); err != nil {
		e.log.Warn("bad rotator state", "err", err)
		return
	}
	if s.Az != nil {
		e.az, e.azKnown = *s.Az, true
	}
	e.targetAz, e.rotMoving, e.rotDevice = s.TargetAz, s.Moving, deviceOnline(s.DeviceOnline)
	e.settleRotator()
	e.inputsChanged()
}

// settleRotator retires the in-flight set_az once the rotator's own /state
// can be trusted again. Our target is "seen" only when target_az matches it
// or motion starts from rest — a frame the WRC emitted before applying our
// command (old target, already moving) neither acknowledges nor retargets.
// The WRC omits target_az for 0°, so a missing target is "unknown", never a
// retarget.
func (e *Engine) settleRotator() {
	if e.cmdAz == nil {
		return
	}
	t := *e.cmdAz
	targetIsOurs := e.targetAz != nil && math.Abs(*e.targetAz-t) <= 1
	if !e.cmdAzAcked && (targetIsOurs || (!e.cmdAzFromMoving && e.rotMoving)) {
		e.cmdAzAcked = true
		e.cmdAzUntil = e.now().Add(rotTravelWindow)
	}
	if e.cmdAzAcked && e.rotMoving {
		e.cmdAzMoved = true
	}
	switch {
	case !e.rotMoving && e.azKnown && math.Abs(e.az-t) <= arriveTolerance:
		e.cmdAz = nil // arrived
	case e.cmdAzMoved && !e.rotMoving:
		e.cmdAz = nil // stopped short (STOP from the console or M5 dial, a limit)
	case e.cmdAzAcked && e.targetAz != nil && !targetIsOurs:
		e.cmdAz = nil // retargeted by someone else (console, M5 dial, wrc listener)
	}
}

func (e *Engine) AntStatus(p []byte) {
	e.antStatus = online(p)
	e.inputsChanged()
	e.retryPending()
}

func (e *Engine) AntState(p []byte) {
	var s struct {
		Direction    string `json:"direction"`
		Band         string `json:"band"`
		Moving       bool   `json:"moving"`
		DeviceOnline *bool  `json:"device_online"`
	}
	if err := json.Unmarshal(p, &s); err != nil {
		e.log.Warn("bad ant-ctrl state", "err", err)
		return
	}
	e.direction, e.antBand, e.antMoving, e.antDevice = s.Direction, s.Band, s.Moving, deviceOnline(s.DeviceOnline)
	if e.cmdDir != "" && !s.Moving {
		switch s.Direction {
		case e.cmdDir:
			e.cmdDir = "" // the controller is there and at rest
		case e.cmdDirFrom:
			// Not applied yet (a poll from before the cmd ran): keep trusting the cmd.
		default:
			e.log.Info("ant-ctrl direction set elsewhere; dropping the in-flight flip",
				"commanded", e.cmdDir, "reported", s.Direction)
			e.cmdDir = ""
		}
	}
	e.inputsChanged()
	e.retryPending()
}

func (e *Engine) RadioStatus(p []byte) {
	e.radioStatus = online(p)
	e.inputsChanged()
	e.retryPending()
}

func (e *Engine) RadioState(p []byte) {
	var s struct {
		Band         string `json:"band"`
		TX           string `json:"tx"`
		DeviceOnline *bool  `json:"device_online"`
	}
	if err := json.Unmarshal(p, &s); err != nil {
		e.log.Warn("bad radio state", "err", err)
		return
	}
	e.band, e.tx, e.radioDevice = s.Band, s.TX == "tx", deviceOnline(s.DeviceOnline)
	e.inputsChanged()
	e.retryPending()
}

func (e *Engine) rotLive() bool   { return e.rotStatus && e.rotDevice }
func (e *Engine) antLive() bool   { return e.antStatus && e.antDevice }
func (e *Engine) radioLive() bool { return e.radioStatus && e.radioDevice }

// transmitting only trusts a live radio: a frozen retained "tx" from a dead
// flexbridge must not hold a direction change forever.
func (e *Engine) transmitting() bool { return e.radioLive() && e.tx }

// decisionBand is the band for the decision. 6m wins if EITHER live source
// says so: the radio is first after a QSY (ant-ctrl follows via antennaselect
// band-follow and a 2 s poll), the controller is the authority on what the
// elements are tuned to. Choosing reverse on 6m is the one wrong answer.
func (e *Engine) decisionBand() string {
	antBand, radioBand := "", ""
	if e.antLive() {
		antBand = e.antBand
	}
	if e.radioLive() {
		radioBand = e.band
	}
	if antBand == "6m" || radioBand == "6m" {
		return "6m"
	}
	if antBand != "" {
		return antBand
	}
	return radioBand
}

func (e *Engine) dirInFlight() bool { return e.cmdDir != "" && e.now().Before(e.cmdDirUntil) }

// effectiveDirection is the direction to decide from: a flip beamsteer just
// sent wins over ant-ctrl's not-yet-updated /state.
func (e *Engine) effectiveDirection() string {
	if !e.antLive() {
		return ""
	}
	if e.dirInFlight() {
		return e.cmdDir
	}
	return e.direction
}

// effectiveAz is the boom heading to decide from: where beamsteer just sent
// the rotator, else where it is going, else where it is.
func (e *Engine) effectiveAz() float64 {
	if e.cmdAz != nil && e.now().Before(e.cmdAzUntil) {
		return *e.cmdAz
	}
	if e.rotMoving && e.targetAz != nil {
		return *e.targetAz
	}
	return e.az
}

// ---------------------------------------------------------------------------
// logger (PstRotator) and console (aim) inputs
// ---------------------------------------------------------------------------

// Goto handles a rotate request for bearing b.
func (e *Engine) Goto(b float64) {
	b = steer.Norm(b)
	last := &Last{Bearing: b, TS: e.ts()}
	e.last = last
	e.pending = nil // a newer request supersedes a held direction change

	switch {
	case !e.connected:
		last.Reason = "not connected to the broker: request ignored"
		e.log.Warn("rotate request ignored: not connected to the broker", "bearing", b)
		return // nothing can be published, /state included
	case !e.enabled:
		t := math.Round(b)
		if e.sendRotate(t) {
			last.RotateTo, last.Reason = &t, "smart rotation off: plain rotation"
		} else {
			last.Reason = "smart rotation off: rotate cmd failed"
		}
		e.publishState()
		return
	case !e.rotLive() || !e.azKnown:
		last.Reason = "rotator offline: request ignored"
		e.log.Warn("rotate request ignored: rotator offline", "bearing", b)
		e.publishState()
		return
	}
	e.decideAndApply(b, last, "")
	e.publishState()
}

// decideAndApply decides for bearing b against the effective state and
// sends what is needed. A direction change that may not go now is held with
// its bearing. note is appended to the reason ("after hold").
func (e *Engine) decideAndApply(b float64, last *Last, note string) {
	az, dir, band := e.effectiveAz(), e.effectiveDirection(), e.decisionBand()
	d := steer.Decide(steer.Inputs{
		Bearing:   b,
		Az:        az,
		Direction: dir,
		Band:      band,
		Lobe:      e.cfg.Steer.LobeDeg,
		BidirLobe: e.cfg.Steer.BidirLobeDeg,
		MaxAz:     e.cfg.Steer.MaxAz,
	})
	reason := d.Reason
	if note != "" {
		reason += " (" + note + ")"
	}
	e.log.Info("smart rotation", "bearing", b, "az", az, "direction", dir, "band", band,
		"rotate_to", fmtPtr(d.RotateTo), "new_direction", d.Direction, "reason", reason)

	last.RotateTo, last.Direction, last.Reason = nil, d.Direction, reason
	if d.RotateTo != nil {
		if !e.sendRotate(*d.RotateTo) {
			// Without the rotation the flip would point the lobe wrong; drop
			// both (and never re-decide into another failing rotate).
			last.Direction, last.Reason = "", reason+"; rotate cmd failed"
			return
		}
		last.RotateTo = d.RotateTo
	}
	if d.Direction != "" {
		e.pending = &heldFlip{dir: d.Direction, bearing: b}
		e.flushPending()
	}
}

// Stop halts the rotator. Always passed through, toggle or not.
func (e *Engine) Stop() {
	e.cmdAz = nil // the commanded target will not be reached
	if err := e.publish(e.rotCmd, 0, false, map[string]any{"action": "stop"}); err != nil {
		e.log.Warn("stop not sent", "err", err)
	}
}

// Tick runs once a second on the jobs worker: lapsed in-flight windows stop
// blocking, and a held flip is retried.
func (e *Engine) Tick() {
	if e.cmdDir != "" && !e.dirInFlight() {
		e.log.Warn("ant-ctrl did not confirm direction in time; deciding from its /state again",
			"commanded", e.cmdDir, "reported", e.direction)
		e.cmdDir = ""
	}
	if e.cmdAz != nil && !e.now().Before(e.cmdAzUntil) {
		e.cmdAz = nil
	}
	e.retryPending()
}

// Readback is the heading reported to the logger's AZ? query: where the main
// lobe points (boom heading, +180 when reversed). Safe from any goroutine.
func (e *Engine) Readback() (float64, bool) {
	e.hmu.Lock()
	defer e.hmu.Unlock()
	return e.heading, e.headingOK
}

// ---------------------------------------------------------------------------
// outputs
// ---------------------------------------------------------------------------

// flushPending sends a held flip once it is safe — after deciding again for
// its bearing against the current state (the band may now be 6m, the
// operator may have set BI, an earlier flip may already cover it). It
// reports whether anything changed, so bus-input callers can republish.
func (e *Engine) flushPending() bool {
	p := e.pending
	if p == nil {
		return false
	}
	if !e.antLive() {
		e.log.Warn("held direction change dropped: ant-ctrl offline", "direction", p.dir)
		e.pending = nil
		return true
	}
	if !e.connected || e.antMoving || e.transmitting() || e.dirInFlight() {
		return false // held; the next ant-ctrl/radio update or Tick retries
	}
	e.pending = nil
	az, dir, band := e.effectiveAz(), e.effectiveDirection(), e.decisionBand()
	d := steer.Decide(steer.Inputs{
		Bearing: p.bearing, Az: az, Direction: dir, Band: band,
		Lobe: e.cfg.Steer.LobeDeg, BidirLobe: e.cfg.Steer.BidirLobeDeg, MaxAz: e.cfg.Steer.MaxAz,
	})
	if d.RotateTo != nil {
		// The world changed under the hold (e.g. QSY to 6m: no reverse, so
		// the boom must turn to the forward target instead).
		if e.last != nil && e.last.Bearing == p.bearing {
			e.decideAndApply(p.bearing, e.last, "after hold")
		}
		return true
	}
	if d.Direction == "" {
		return true // nothing left to do
	}
	if err := e.publish(e.antCmd, 1, true, map[string]any{"action": "direction", "value": d.Direction, "ts": e.ts()}); err != nil {
		e.log.Warn("direction cmd not sent; retrying", "direction", d.Direction, "err", err)
		e.pending = &heldFlip{dir: d.Direction, bearing: p.bearing}
		return false
	}
	e.cmdDir, e.cmdDirFrom, e.cmdDirUntil = d.Direction, e.direction, e.now().Add(dirSettle)
	return true
}

// retryPending is flushPending for bus inputs: republish /state on change.
func (e *Engine) retryPending() {
	if e.flushPending() {
		e.publishState()
	}
}

// sendRotate commands the rotator and records the target in flight — only
// if the cmd actually went out.
func (e *Engine) sendRotate(az float64) bool {
	if err := e.publish(e.rotCmd, 0, false, map[string]any{"action": "set_az", "az": az}); err != nil {
		e.log.Warn("set_az not sent", "az", az, "err", err)
		return false
	}
	t := az
	e.cmdAz, e.cmdAzUntil = &t, e.now().Add(rotAckWindow)
	e.cmdAzAcked, e.cmdAzMoved, e.cmdAzFromMoving = false, false, e.rotMoving
	return true
}

// inputs is the sibling liveness published in /state, so the console can
// show that beamsteer is blind rather than an AUTO that silently does
// nothing (2026-10-01: a poisoned broker session left it without rotator
// data for hours while /state read enabled).
type inputs struct {
	Rotator bool `json:"rotator"`
	AntCtrl bool `json:"ant_ctrl"`
	Radio   bool `json:"radio"`
}

func (e *Engine) currentInputs() inputs {
	return inputs{Rotator: e.rotLive() && e.azKnown, AntCtrl: e.antLive(), Radio: e.radioLive()}
}

// inputsChanged refreshes the AZ? heading and republishes /state when a
// sibling's liveness flips (logged, so the journal shows when beamsteer
// gains or loses its eyes).
func (e *Engine) inputsChanged() {
	e.updateHeading()
	in := e.currentInputs()
	if in == e.lastInputs {
		return
	}
	if in.Rotator != e.lastInputs.Rotator {
		if in.Rotator {
			e.log.Info("rotator readback live", "az", e.az)
		} else {
			e.log.Warn("rotator readback lost: rotate requests will be ignored")
		}
	}
	e.lastInputs = in
	e.publishState()
}

func (e *Engine) updateHeading() {
	e.hmu.Lock()
	defer e.hmu.Unlock()
	e.headingOK = e.rotLive() && e.azKnown
	dir := ""
	if e.antLive() {
		dir = e.direction
	}
	e.heading = steer.EffectiveHeading(e.az, dir)
}

// Republish re-sends /meta and /state (on every broker (re)connect).
func (e *Engine) Republish() {
	e.publishMeta()
	e.publishState()
}

func (e *Engine) publishState() {
	st := map[string]any{"ts": e.ts(), "enabled": e.enabled, "inputs": e.currentInputs()}
	if e.pending != nil {
		st["pending"] = e.pending.dir
	}
	if e.last != nil {
		st["last"] = e.last
	}
	if err := e.publish(e.self+"/state", 1, true, st); err != nil {
		e.log.Debug("state not published", "err", err)
	}
}

func (e *Engine) publishMeta() {
	s := e.cfg.Steer
	meta := map[string]any{
		"schema":   "1.0",
		"role":     "steering",
		"link":     "none",
		"location": e.cfg.Location,
		"host":     e.cfg.Host,
		"capabilities": map[string]any{
			"controls":        []string{s.RotatorSlot, s.AntCtrlSlot},
			"input":           "pstrotator-udp",
			"actions":         []string{"enable", "disable", "aim"},
			"pstrotator_port": e.cfg.PstRotator.Port,
			"lobe_deg":        s.LobeDeg,
			"bidir_lobe_deg":  s.BidirLobeDeg,
			"max_az":          s.MaxAz,
		},
		"expose": map[string]any{
			"device": map[string]any{"name": "Beam steering"},
			"fields": []map[string]any{
				{"key": "enabled", "name": "Smart rotation", "type": "boolean"},
			},
		},
	}
	if err := e.publish(e.self+"/meta", 1, true, meta); err != nil {
		e.log.Debug("meta not published", "err", err)
	}
}

func (e *Engine) publish(topic string, qos byte, retained bool, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		e.log.Error("marshal", "topic", topic, "err", err)
		return err
	}
	return e.pub.Publish(topic, qos, retained, b)
}

func (e *Engine) ts() string { return e.now().UTC().Format(time.RFC3339) }

func fmtPtr(p *float64) any {
	if p == nil {
		return "-"
	}
	return *p
}
