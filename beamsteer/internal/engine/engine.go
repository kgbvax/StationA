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

// Publisher publishes one MQTT message (QoS 1). Implementations may block
// briefly; the engine only calls it from the jobs worker.
type Publisher interface {
	Publish(topic string, retained bool, payload []byte) error
}

// Last is the most recent logger request and what beamsteer did with it.
type Last struct {
	Bearing   float64  `json:"bearing"`
	RotateTo  *float64 `json:"rotate_to,omitempty"`
	Direction string   `json:"direction,omitempty"`
	Reason    string   `json:"reason"`
	TS        string   `json:"ts"`
}

// Engine is the beamsteer state machine.
type Engine struct {
	cfg config.Config
	pub Publisher
	log *slog.Logger
	now func() time.Time

	self    string
	rotCmd  string
	antCmd  string
	enabled bool

	rotStatus, rotDevice bool
	az                   float64
	azKnown              bool
	targetAz             *float64
	rotMoving            bool

	antStatus, antDevice bool
	direction            string
	antMoving            bool

	radioStatus, radioDevice bool
	band                     string
	tx                       bool

	pending string // direction held until ant-ctrl stops moving and the radio is in RX
	last    *Last

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

// SetEnabled applies the operator toggle (own /cmd).
func (e *Engine) SetEnabled(on bool) {
	if e.enabled == on {
		return
	}
	e.enabled = on
	if !on {
		e.pending = ""
	}
	e.log.Info("smart rotation", "enabled", on)
	e.publishState()
}

func online(p []byte) bool { return strings.EqualFold(strings.TrimSpace(string(p)), "online") }

// deviceOnline reads /state.device_online; a bridge that omits it is judged
// by /status alone.
func deviceOnline(v *bool) bool { return v == nil || *v }

func (e *Engine) RotatorStatus(p []byte) { e.rotStatus = online(p); e.updateHeading() }

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
	e.updateHeading()
}

func (e *Engine) AntStatus(p []byte) {
	e.antStatus = online(p)
	e.updateHeading()
	e.retryPending()
}

func (e *Engine) AntState(p []byte) {
	var s struct {
		Direction    string `json:"direction"`
		Moving       bool   `json:"moving"`
		DeviceOnline *bool  `json:"device_online"`
	}
	if err := json.Unmarshal(p, &s); err != nil {
		e.log.Warn("bad ant-ctrl state", "err", err)
		return
	}
	e.direction, e.antMoving, e.antDevice = s.Direction, s.Moving, deviceOnline(s.DeviceOnline)
	e.updateHeading()
	e.retryPending()
}

func (e *Engine) RadioStatus(p []byte) { e.radioStatus = online(p); e.retryPending() }

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
	e.retryPending()
}

func (e *Engine) rotLive() bool   { return e.rotStatus && e.rotDevice }
func (e *Engine) antLive() bool   { return e.antStatus && e.antDevice }
func (e *Engine) radioLive() bool { return e.radioStatus && e.radioDevice }

// transmitting only trusts a live radio: a frozen retained "tx" from a dead
// flexbridge must not hold a direction change forever.
func (e *Engine) transmitting() bool { return e.radioLive() && e.tx }

// ---------------------------------------------------------------------------
// logger (PstRotator) inputs
// ---------------------------------------------------------------------------

// Goto handles a logger rotate request for bearing b.
func (e *Engine) Goto(b float64) {
	b = steer.Norm(b)
	last := &Last{Bearing: b, TS: e.ts()}
	e.last = last
	e.pending = "" // a newer request supersedes a held direction change

	if !e.enabled {
		t := math.Round(b)
		last.RotateTo, last.Reason = &t, "smart rotation off: plain rotation"
		e.sendRotate(t)
		e.publishState()
		return
	}
	if !e.rotLive() || !e.azKnown {
		last.Reason = "rotator offline: request ignored"
		e.log.Warn("logger rotate request ignored: rotator offline", "bearing", b)
		e.publishState()
		return
	}

	az := e.az
	if e.rotMoving && e.targetAz != nil {
		// Decide from where the rotator is going, not from mid-travel.
		az = *e.targetAz
	}
	dir := ""
	if e.antLive() {
		dir = e.direction
	}
	d := steer.Decide(steer.Inputs{
		Bearing:   b,
		Az:        az,
		Direction: dir,
		Band:      e.band,
		Lobe:      e.cfg.Steer.LobeDeg,
		BidirLobe: e.cfg.Steer.BidirLobeDeg,
		MaxAz:     e.cfg.Steer.MaxAz,
	})
	last.RotateTo, last.Direction, last.Reason = d.RotateTo, d.Direction, d.Reason
	e.log.Info("smart rotation", "bearing", b, "az", az, "direction", dir, "band", e.band,
		"rotate_to", fmtPtr(d.RotateTo), "new_direction", d.Direction, "reason", d.Reason)

	if d.RotateTo != nil {
		e.sendRotate(*d.RotateTo)
	}
	if d.Direction != "" {
		e.pending = d.Direction
		e.flushPending()
	}
	e.publishState()
}

// Stop halts the rotator. Always passed through, toggle or not.
func (e *Engine) Stop() {
	e.publish(e.rotCmd, false, map[string]any{"action": "stop"})
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

// flushPending sends a held direction change once it is safe. It reports
// whether pending changed, so bus-input callers can republish /state.
func (e *Engine) flushPending() bool {
	if e.pending == "" {
		return false
	}
	if !e.antLive() {
		e.log.Warn("direction change dropped: ant-ctrl offline", "direction", e.pending)
		e.pending = ""
		return true
	}
	if e.antMoving || e.transmitting() {
		return false // held; the next ant-ctrl/radio update retries
	}
	dir := e.pending
	e.pending = ""
	e.publish(e.antCmd, true, map[string]any{"action": "direction", "value": dir, "ts": e.ts()})
	return true
}

// retryPending is flushPending for bus inputs: republish /state on change.
func (e *Engine) retryPending() {
	if e.flushPending() {
		e.publishState()
	}
}

func (e *Engine) sendRotate(az float64) {
	e.publish(e.rotCmd, false, map[string]any{"action": "set_az", "az": az})
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
	st := map[string]any{"ts": e.ts(), "enabled": e.enabled}
	if e.pending != "" {
		st["pending"] = e.pending
	}
	if e.last != nil {
		st["last"] = e.last
	}
	e.publish(e.self+"/state", true, st)
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
	e.publish(e.self+"/meta", true, meta)
}

func (e *Engine) publish(topic string, retained bool, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		e.log.Error("marshal", "topic", topic, "err", err)
		return
	}
	if err := e.pub.Publish(topic, retained, b); err != nil {
		e.log.Warn("publish failed", "topic", topic, "err", err)
	}
}

func (e *Engine) ts() string { return e.now().UTC().Format(time.RFC3339) }

func fmtPtr(p *float64) any {
	if p == nil {
		return "-"
	}
	return *p
}
