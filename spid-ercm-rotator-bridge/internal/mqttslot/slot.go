// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mqttslot is the two-slot MQTT surface (plan U5): one paho client per
// rotator slot (KTD2 — the shelly-power-bridge per-slot shape), each publishing
// the station integration model's four planes for its axis:
//
//	/meta    retained birth certificate (role `rotator`, axes + travel limits,
//	         firmware once the driver has read it, read-only expose per KTD4)
//	/state   retained snapshot {ts, az|el, target, moving, link, device_online,
//	         error} — poll-tick cadence with dedup (KTD14)
//	/status  retained online|offline LWT, self-published offline on clean exit
//	/cmd     one-shot goto/stop intent (KTD13)
//
// The /cmd posture is one-shot (KTD13, the 2026-09-03 ultrabridge incident
// pattern): QoS-0 subscription so no broker offline backlog can queue motion
// behind the bridge, clear-after-execute-or-reject (the empty-payload echo
// guard keeps the clear from re-triggering), and a ts staleness gate for
// stamped producers — unstamped payloads are tolerated.
//
// The /state cadence follows KTD14 (the ultrabridge pattern): a fixed
// poll-tick reads the mount façade — Readback/Target/Moving/Online — dedups
// the snapshot, and republishes on change or edge with a fresh ts. Because
// every control path (MQTT /cmd, the U6 rotctld server, the U7 PstRotator
// listener) funnels through the same façade, protocol-driven motion surfaces
// in /state exactly like bus-driven motion (R4).
//
// Concurrency (runtime-library REQ-RT): paho handlers only Enqueue onto the
// per-slot jobs channel; RunJobs (shared/mqtt) executes and publishes. The
// poll tick runs on its own goroutine and publishes directly — it is not a
// paho handler.
package mqttslot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	sharedmqtt "codeberg.org/kgbvax/stationa/shared/mqtt"
	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"spid-ercm-rotator-bridge/internal/config"
	"spid-ercm-rotator-bridge/internal/mount"
)

// Mount is the dispatch-façade surface one slot consumes (KTD12: servers never
// re-implement cross-axis semantics — they call the façade). It is exactly
// the subset of *mount.Mount the slot needs; the façade satisfies it as
// written, and tests substitute a fake.
type Mount interface {
	// Goto admits one mount-level position intent (KTD8/R8–R12).
	Goto(t mount.Target) []mount.Refusal
	// Stop is the atomic all-stop from any control path (KTD8).
	Stop() []mount.AxisError
	// Readback returns the axis's cached position and its validity.
	Readback(ax mount.Axis) (float64, bool)
	// Online reports the axis's device-link liveness (the device_online layer).
	Online(ax mount.Axis) bool
	// Target returns the axis's last admitted target; cleared by Stop.
	Target(ax mount.Axis) (float64, bool)
	// Moving reports the axis's inferred motion (KTD14).
	Moving(ax mount.Axis) bool
}

// Options wires one slot. Every address and identity field comes from the
// bridge config — no site, station, host or location constant lives in this
// package (§8.1 item 6).
type Options struct {
	Broker   string
	ClientID string // optional per-bridge base; the slot derives its own client id
	User     string
	Password string

	Site    string // address prefix, e.g. "muehle" (config)
	Station string // e.g. "uhf" (config)
	Slot    string // slot address segment, e.g. "az-rotator" (config)
	Axis    string // config.AxisAZ / config.AxisEL — this slot's mount axis

	// Location / Host are the /meta identity fields, from config.
	Location string
	Host     string

	// DeviceModel / DeviceLink are the /meta device identity, from the
	// per-axis [[slot]] config.
	DeviceModel string
	DeviceLink  string

	// Firmware returns the controller firmware string for /meta.device
	// (the ERC-M rFMW). Nil (or "") omits the key — SPID's Rot1Prog has
	// none to report.
	Firmware func() string

	// Limits is this axis's travel envelope; it publishes into /meta
	// capabilities and nowhere else — refusals are the façade's job.
	Limits config.AxisControl

	// Mount is the dispatch façade this slot drives from /cmd.
	Mount Mount

	// Err returns the axis driver's last link-error string for
	// /state.error ("" while healthy). The az slot wires the SPID driver's
	// Err (which self-clears on recovery); the el slot gates the ERC-M
	// driver's LastError on Online (which does not self-clear).
	Err func() string

	// PollInterval is the /state snapshot tick (config control.poll_interval).
	PollInterval time.Duration
}

// Slot is one rotator slot's MQTT client: one paho connection with its own
// retained-LWT /status, its own /cmd subscription, and its own jobs worker.
type Slot struct {
	opts Options
	axis mount.Axis
	mnt  Mount
	log  *slog.Logger

	client paho.Client

	ctx    context.Context
	cancel context.CancelFunc
	jobs   chan func()

	// mu guards the dedup snapshot, the rejection error and the firmware
	// tracking. Held only briefly; never across a publish.
	mu      sync.Mutex
	hasLast bool
	last    snap
	cmdErr  string // last /cmd rejection, surfaced in /state.error; cleared by the next admitted intent
	lastFw  string // last firmware string folded into /meta

	statusTopic string
	metaTopic   string
	stateTopic  string
	cmdTopic    string
}

// newSlot builds the slot without a broker connection (tests wire a fake
// paho client; New wires a real one).
func newSlot(o Options, log *slog.Logger) (*Slot, error) {
	if o.Site == "" || o.Station == "" || o.Slot == "" {
		return nil, fmt.Errorf("mqttslot: site, station and slot must be set for station-model addressing")
	}
	if o.Mount == nil {
		return nil, fmt.Errorf("mqttslot: no mount façade wired")
	}
	if o.PollInterval <= 0 {
		return nil, fmt.Errorf("mqttslot: poll interval must be > 0")
	}
	var axis mount.Axis
	switch o.Axis {
	case config.AxisAZ:
		axis = mount.AZ
	case config.AxisEL:
		axis = mount.EL
	default:
		return nil, fmt.Errorf("mqttslot: axis must be %q or %q (got %q)", config.AxisAZ, config.AxisEL, o.Axis)
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Slot{
		opts: o,
		axis: axis,
		mnt:  o.Mount,
		log:  log,
		jobs: make(chan func(), 256),

		statusTopic: schema.StatusTopic(o.Site, o.Station, o.Slot),
		metaTopic:   schema.MetaTopic(o.Site, o.Station, o.Slot),
		stateTopic:  schema.StateTopic(o.Site, o.Station, o.Slot),
		cmdTopic:    schema.CmdTopic(o.Site, o.Station, o.Slot),
	}, nil
}

// New builds the slot's paho client and connects. The initial connect is
// fatal by design (§8.1 item 10): an unreachable broker returns an error so
// main exits non-zero and systemd crash-loops the unit — the bridge must
// never run with its MQTT plane silently disabled.
func New(ctx context.Context, o Options, log *slog.Logger) (*Slot, error) {
	s, err := newSlot(o, log)
	if err != nil {
		return nil, err
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	go sharedmqtt.RunJobs(s.ctx, s.jobs)

	clientID := s.deriveClientID()
	opts := paho.NewClientOptions().
		AddBroker(o.Broker).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetCleanSession(false).
		SetWill(s.statusTopic, "offline", 1, true)
	if o.User != "" {
		opts.SetUsername(o.User)
	}
	if o.Password != "" {
		opts.SetPassword(o.Password)
	}
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		// Degraded but recovering: paho auto-reconnects (logging convention §3).
		s.log.Warn("mqtt connection lost", "err", err)
	})
	opts.SetOnConnectHandler(func(cl paho.Client) { s.onConnect(cl) })

	s.client = paho.NewClient(opts)
	s.log.Info("mqtt connecting", "broker", o.Broker, "client_id", clientID)
	// Context-aware connect: paho's Connect().Wait() ignores ctx, so a SIGTERM
	// while the broker is unreachable cannot interrupt it (shared/mqtt bridges
	// the wait through a goroutine + select on ctx.Done).
	if err := sharedmqtt.Connect(s.ctx, s.client); err != nil {
		s.cancel()
		return nil, err
	}
	return s, nil
}

// deriveClientID defaults to a derivation of the slot address (§8.1 item 6) so
// a duplicate connection is diagnosable on the broker; a configured base is
// extended per slot.
func (s *Slot) deriveClientID() string {
	if s.opts.ClientID == "" {
		return s.opts.Site + "-" + s.opts.Station + "-" + s.opts.Slot
	}
	return s.opts.ClientID + "-" + s.opts.Slot
}

// Run drives the /state poll tick until the slot's context is done: snapshot
// the façade, dedup, republish on change or edge (KTD14). The caller owns the
// goroutine.
func (s *Slot) Run() {
	s.tick() // publish an immediate first snapshot so consumers never see a stateless slot
	ticker := time.NewTicker(s.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// Close is the clean shutdown (powerseq pattern): the LWT does not fire on a
// clean exit, so the slot self-publishes its retained offline /status before
// disconnecting. Also stops the jobs worker.
func (s *Slot) Close() {
	if s == nil {
		return
	}
	// Stop the worker first so no queued cmd/clear work races the shutdown.
	if s.cancel != nil {
		s.cancel()
	}
	if s.client != nil && s.client.IsConnectionOpen() {
		s.publishStringVia(s.client, s.statusTopic, "offline", 1, true)
		s.client.Disconnect(250)
	}
}

// ---------------------------------------------------------------------------
// connect ritual
// ---------------------------------------------------------------------------

// onConnect runs on every (re)connect (paho OnConnectHandler): /status
// online, the retained birth certificate, the QoS-0 /cmd subscription, and a
// forced /state restore — a broker wipe that dropped retained messages must
// not leave the slot stateless. It runs on a paho goroutine; the blocking
// publishes follow the ultrabridge onConnect precedent, and the /state
// restore is enqueued onto the jobs worker (REQ-RT).
func (s *Slot) onConnect(cl paho.Client) {
	s.log.Info("mqtt connected", "broker", s.opts.Broker, "client_id", s.deriveClientID())
	s.publishStringVia(cl, s.statusTopic, "online", 1, true)
	s.publishJSONVia(cl, s.metaTopic, s.metaPayload(), 1, true)
	s.subscribeCmd(cl)
	s.resetDedup()
	sharedmqtt.Enqueue(s.jobs, func() { s.publishState(true) })
}

// subscribeCmd subscribes /cmd at QoS 0 ON PURPOSE (KTD13): with a persistent
// session (CleanSession=false) a QoS-1 subscription lets the broker QUEUE
// every /cmd published while this bridge is offline and replay the whole
// backlog on reconnect — with real antennas behind it. QoS 0 stops offline
// queueing; retained delivery still works at any subscription QoS, and the
// clear after every handled cmd means nothing stale lingers.
func (s *Slot) subscribeCmd(cl paho.Client) {
	if token := cl.Subscribe(s.cmdTopic, 0, s.onCmd); token.Wait() && token.Error() != nil {
		s.log.Error("cmd subscribe failed", "topic", s.cmdTopic, "err", token.Error())
		return
	}
	s.log.Info("subscribed", "topic", s.cmdTopic, "qos", 0)
}

// ---------------------------------------------------------------------------
// /cmd one-shot handling (KTD13)
// ---------------------------------------------------------------------------

// cmdMaxAge bounds how old a stamped /cmd may be (the staleness gate, §8 rule
// 3). Producers SHOULD stamp commands; unstamped payloads are tolerated
// (existing console publishers stamp none) — the QoS-0 subscription +
// clear-after-execute defenses cover them.
const cmdMaxAge = 30 * time.Second

// cmdMsg is the local /cmd payload extension (the ultrabridge precedent):
// shared/schema.CmdPayload stays the Action/Value-only convention type; the
// optional ts rides this struct so the gate never leaks into the shared shape.
type cmdMsg struct {
	Action string `json:"action"`
	Value  string `json:"value,omitempty"`
	// Ts is the optional producer timestamp (RFC3339). Present but stale,
	// future, or unparseable ⇒ the cmd is rejected before dispatch.
	Ts string `json:"ts,omitempty"`
}

// onCmd is the /cmd handler. It runs inline on paho's dispatch goroutine and
// must never block: parsing and the gates happen here (cheap), and everything
// that dispatches, publishes or clears is enqueued onto the jobs worker.
//
// One-shot posture: whatever the payload, the retained topic is cleared after
// the worker has acted on it (executed or rejected), so nothing re-fires on
// the next (re)connect.
func (s *Slot) onCmd(_ paho.Client, msg paho.Message) {
	if len(msg.Payload()) == 0 {
		// Empty payload = the retained-clear marker (our own clear echo — we
		// are subscribed to the topic we clear). Not a command; do not re-clear
		// (that would echo another empty payload and loop forever).
		return
	}

	var cmd cmdMsg
	if err := json.Unmarshal(msg.Payload(), &cmd); err != nil || cmd.Action == "" {
		s.log.Error("rx invalid cmd", "topic", msg.Topic(), "payload", string(msg.Payload()))
		s.rejectAsync(fmt.Sprintf("invalid /cmd payload: %q", string(msg.Payload())))
		return
	}

	if cmd.Ts != "" {
		ts, err := time.Parse(time.RFC3339, cmd.Ts)
		if err != nil {
			s.log.Error("rx cmd with bad ts", "ts", cmd.Ts, "err", err)
			s.rejectAsync(fmt.Sprintf("cmd ts unparseable: %q", cmd.Ts))
			return
		}
		if age := time.Since(ts); age > cmdMaxAge || age < -cmdMaxAge {
			s.log.Warn("dropping stale cmd", "action", cmd.Action, "age", age.Round(time.Second), "max", cmdMaxAge)
			s.rejectAsync(fmt.Sprintf("stale cmd (age %s, bound %s)", age.Round(time.Second), cmdMaxAge))
			return
		}
	}

	switch cmd.Action {
	case "goto":
		deg, err := parseDegrees(cmd.Value)
		if err != nil {
			s.log.Warn("invalid goto value", "value", cmd.Value, "err", err)
			s.rejectAsync(fmt.Sprintf("invalid goto value %q: %v", cmd.Value, err))
			return
		}
		s.log.Info("rx cmd", "action", cmd.Action, "value", cmd.Value)
		sharedmqtt.Enqueue(s.jobs, func() { s.executeGoto(deg) })
	case "stop":
		s.log.Info("rx cmd", "action", cmd.Action)
		sharedmqtt.Enqueue(s.jobs, s.executeStop)
	default:
		s.log.Warn("unknown cmd action", "action", cmd.Action)
		s.rejectAsync(fmt.Sprintf("unknown cmd action %q", cmd.Action))
	}
}

// parseDegrees parses the value-key payload into a finite position. The
// strconv trap: ParseFloat happily returns NaN/Inf, which would sail through
// comparisons — refuse them here, up front.
func parseDegrees(v string) (float64, error) {
	deg, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, fmt.Errorf("not a number")
	}
	if math.IsNaN(deg) || math.IsInf(deg, 0) {
		return 0, fmt.Errorf("not a finite position")
	}
	return deg, nil
}

// executeGoto runs on the jobs worker: dispatch through the façade (the same
// pipeline every control path feeds), surface any refusal in /state.error
// (cleared by the next admitted intent), then clear the retained cmd —
// rejection still clears (one-shot: execute-or-reject).
func (s *Slot) executeGoto(deg float64) {
	t := mount.Target{HasAZ: s.axis == mount.AZ, HasEL: s.axis == mount.EL}
	if s.axis == mount.AZ {
		t.AZ = deg
	} else {
		t.EL = deg
	}
	refs := s.mnt.Goto(t)
	if len(refs) > 0 {
		s.setCmdErr(refs[0].Error())
	} else {
		s.setCmdErr("")
	}
	s.publishState(false)
	s.clearCmd()
}

// executeStop runs on the jobs worker: the atomic all-stop (KTD8 — it halts
// BOTH axes and clears every pending target on both, which is why a stop on
// either slot's /cmd is a full-mount stop).
func (s *Slot) executeStop() {
	errs := s.mnt.Stop()
	if len(errs) > 0 {
		s.setCmdErr(errs[0].Error())
	} else {
		s.setCmdErr("")
	}
	s.publishState(false)
	s.clearCmd()
}

// rejectAsync enqueues the rejection path onto the jobs worker: record the
// error into /state.error, publish, and STILL clear the retained cmd.
func (s *Slot) rejectAsync(errMsg string) {
	sharedmqtt.Enqueue(s.jobs, func() { s.reject(errMsg) })
}

func (s *Slot) reject(errMsg string) {
	s.setCmdErr(errMsg)
	s.publishState(false)
	s.clearCmd()
}

// clearCmd publishes an empty retained payload so no stale /cmd can re-fire
// on the next (re)connect. Runs on the jobs worker (blocking QoS-1 publish —
// never inline on paho's dispatch goroutine).
func (s *Slot) clearCmd() {
	s.publishBytesVia(s.client, s.cmdTopic, 1, true, []byte{})
}

func (s *Slot) setCmdErr(msg string) {
	s.mu.Lock()
	s.cmdErr = msg
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// /state cadence (KTD14)
// ---------------------------------------------------------------------------

// snap is the comparable /state snapshot: dedup compares this, never the
// serialized form (ts is always fresh).
type snap struct {
	pos    float64
	hasPos bool
	tgt    float64
	hasTgt bool
	moving bool
	online bool
	err    string
}

// tick is one poll tick: fold the façade state into a snapshot, dedup, and
// pick up a late-arriving firmware string for /meta.
func (s *Slot) tick() {
	s.maybeRepublishMetaForFirmware()
	s.publishState(false)
}

// maybeRepublishMetaForFirmware republishes the birth certificate once the
// driver's firmware read lands (the ERC-M rFMW is read on the first link
// open, which may be after the initial connect published /meta).
func (s *Slot) maybeRepublishMetaForFirmware() {
	if s.opts.Firmware == nil {
		return
	}
	fw := s.opts.Firmware()
	s.mu.Lock()
	changed := fw != s.lastFw
	if changed {
		s.lastFw = fw
	}
	s.mu.Unlock()
	if changed {
		s.publishMeta()
	}
}

// snapshot reads the façade: position + validity, target, moving (the façade
// owns the KTD14 inference), device liveness, and the merged error — the last
// /cmd rejection, else the axis driver's link error.
func (s *Slot) snapshot() snap {
	pos, valid := s.mnt.Readback(s.axis)
	tgt, hasTgt := s.mnt.Target(s.axis)
	s.mu.Lock()
	cmdErr := s.cmdErr
	s.mu.Unlock()
	sn := snap{
		pos:    pos,
		hasPos: valid,
		tgt:    tgt,
		hasTgt: hasTgt,
		moving: s.mnt.Moving(s.axis),
		online: s.mnt.Online(s.axis),
	}
	if cmdErr != "" {
		sn.err = cmdErr
	} else if s.opts.Err != nil {
		sn.err = s.opts.Err()
	}
	return sn
}

// publishState dedups the snapshot (force bypasses the dedup for the
// reconnect restore) and publishes the retained JSON with a fresh RFC3339 ts.
func (s *Slot) publishState(force bool) {
	sn := s.snapshot()
	s.mu.Lock()
	if !force && s.hasLast && s.last == sn {
		s.mu.Unlock()
		return
	}
	s.last = sn
	s.hasLast = true
	s.mu.Unlock()
	s.publishJSONVia(s.client, s.stateTopic, s.statePayload(sn), 1, true)
}

// resetDedup makes the next publishState republish regardless of change, so
// the reconnect restore re-lands the retained /state even when idle.
func (s *Slot) resetDedup() {
	s.mu.Lock()
	s.hasLast = false
	s.mu.Unlock()
}

// statePayload is the wire shape: {ts, az|el, target, moving, link,
// device_online, error}. The position key is the axis name (az / el) — one
// position per slot; an invalid readback omits the position rather than
// fabricating one (KTD9's never-a-fabricated-position rule, bus side).
type statePayload struct {
	TS           string   `json:"ts"`
	AZ           *float64 `json:"az,omitempty"`
	EL           *float64 `json:"el,omitempty"`
	Target       *float64 `json:"target,omitempty"`
	Moving       bool     `json:"moving"`
	Link         string   `json:"link"`
	DeviceOnline bool     `json:"device_online"`
	Error        string   `json:"error,omitempty"`
}

func (s *Slot) statePayload(sn snap) statePayload {
	p := statePayload{
		TS:           time.Now().UTC().Format(time.RFC3339),
		Moving:       sn.moving,
		Link:         s.opts.DeviceLink,
		DeviceOnline: sn.online,
		Error:        sn.err,
	}
	if sn.hasPos {
		pos := sn.pos
		if s.axis == mount.AZ {
			p.AZ = &pos
		} else {
			p.EL = &pos
		}
	}
	if sn.hasTgt {
		tgt := sn.tgt
		p.Target = &tgt
	}
	return p
}

// ---------------------------------------------------------------------------
// /meta birth certificate
// ---------------------------------------------------------------------------

// metaPayload builds the retained /meta birth certificate as a map so the
// optional firmware key (and only it) can be truly absent, never an empty
// string. Everything is composed from Options — the §8.1 item 6 rule.
func (s *Slot) metaPayload() map[string]any {
	meta := map[string]any{
		"schema": "1.0",
		"role":   "rotator", // canonical role (§4) — the device name lives in device
		"device": map[string]any{
			"model": s.opts.DeviceModel,
		},
		"link":     s.opts.DeviceLink,
		"location": s.opts.Location,
		"host":     s.opts.Host,
		"capabilities": map[string]any{
			"axes": []string{s.opts.Axis},
			"limits": map[string]float64{
				"min":  s.opts.Limits.Min,
				"max":  s.opts.Limits.Max,
				"park": s.opts.Limits.Park,
			},
			"deadband": s.opts.Limits.Deadband,
		},
		// Read-only expose (KTD4): state fields only — no writable setpoints,
		// no command widgets, no actions. hadiscovery renders state; HA motion
		// controls deliberately do not exist for a rotator slot (the no-arming
		// posture's motion authority lives on /cmd and the protocol listeners).
		"expose": map[string]any{
			"device": map[string]any{
				"name":  s.opts.Slot,
				"model": s.opts.DeviceModel,
				// No "area": hadiscovery supplies the deployment-wide default
				// HA area (integration model §9).
			},
			"fields": []map[string]any{
				{
					"key": s.opts.Axis, "name": axisName(s.opts.Axis), "type": "number",
					"unit": "°", "class": axisClass(s.opts.Axis), "state_class": "measurement",
				},
				{"key": "target", "name": axisName(s.opts.Axis) + " target", "type": "number", "unit": "°"},
				{"key": "moving", "name": "Moving", "type": "boolean"},
				{"key": "device_online", "name": "Device online", "type": "boolean"},
				{"key": "error", "name": "Last error", "type": "string"},
			},
		},
	}
	if fw := s.opts.firmware(); fw != "" {
		dev := meta["device"].(map[string]any)
		dev["firmware"] = fw
	}
	return meta
}

func (o Options) firmware() string {
	if o.Firmware == nil {
		return ""
	}
	return o.Firmware()
}

func axisName(axis string) string {
	if axis == config.AxisEL {
		return "Elevation"
	}
	return "Azimuth"
}

func axisClass(axis string) string {
	if axis == config.AxisEL {
		return "" // no canonical device class for elevation
	}
	return "azimuth"
}

// publishMeta (re)publishes the retained birth certificate via the slot's
// own client — the reconnect ritual and the late-firmware fold both land here.
func (s *Slot) publishMeta() {
	s.publishJSONVia(s.client, s.metaTopic, s.metaPayload(), 1, true)
}

// ---------------------------------------------------------------------------
// publish helpers
// ---------------------------------------------------------------------------

// The publish helpers block on the publish token, so they run only on the
// jobs worker, the poll tick, Close, or the connect ritual — never inside a
// message handler. The client is a parameter because OnConnect fires before
// New returns and assigns Slot.client (the wrc PublishMetaVia precedent).
func (s *Slot) publishJSONVia(cl paho.Client, topic string, v any, qos byte, retained bool) {
	b, err := json.Marshal(v)
	if err != nil {
		s.log.Error("marshal publish payload", "topic", topic, "err", err)
		return
	}
	s.publishBytesVia(cl, topic, qos, retained, b)
}

func (s *Slot) publishStringVia(cl paho.Client, topic, payload string, qos byte, retained bool) {
	s.publishBytesVia(cl, topic, qos, retained, []byte(payload))
}

func (s *Slot) publishBytesVia(cl paho.Client, topic string, qos byte, retained bool, b []byte) {
	token := cl.Publish(topic, qos, retained, b)
	if token.Wait() && token.Error() != nil {
		s.log.Error("publish failed", "topic", topic, "err", token.Error())
		return
	}
	s.log.Debug("tx", "topic", topic, "qos", qos, "retained", retained, "payload", string(b))
}
