// Package seq owns the station startup/shutdown state machine for the powerseq
// sequencer (integration model `sequencer` role, logic slot muehle/hf/power-seq).
//
// The sequence is CONFIG-DRIVEN: a pair of ordered step lists (startup/shutdown)
// supplied to New. Each step is one of four kinds —
//
//	cmd          emit a retained /cmd {action, value} to a slot
//	wait_status  wait until N slots' /status == online|offline
//	             (state="online" is two-layer: each slot's /state.device_online
//	             must also not be "false" when present — a bridge whose LWT is
//	             online in front of a still-booting device does not pass)
//	wait_state   wait until a slot's /state top-level field == value
//	             (implicit two-layer liveness precondition: the slot's /status
//	             must be online AND its /state.device_online must not be "false"
//	             when present, so neither a dead bridge whose LWT fired nor a
//	             dead device behind a live bridge can pass on a stale retained
//	             /state)
//	delay        sleep a fixed duration (a literal duration_s or a symbolic
//	             "network"/"stagger" ref into [timing])
//	wol          send a Wake-on-LAN magic packet (UDP broadcast)
//
// Besides start/stop the sequencer has an optional idle standby: from
// phase=running, when the activity slot (antenna-select) reports the station
// inactive and no watched /state field moved for the idle timeout, it runs the
// standby list (antenna power off, shack PC to sleep) → phase=standby. Fresh
// activity (a last_activity newer than the standby entry, a watch moving) or a
// `start` runs the wake list → phase=running.
//
// The runner is a single goroutine that executes one sequence at a time;
// observations arrive from the mqtt layer and update a mutex-guarded input map.
// Waits poll that map with a deadline (step timeout → fault, no rollback).
// Blocking the runner is safe — it is never a paho message-handler goroutine.
//
// The sequencer is ONE WRITER of the controlled slots but does not lock them —
// any channel stays directly toggleable for troubleshooting while the
// sequencer is idle (model §7.1).
package seq

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	schema "codeberg.org/kgbvax/stationa/shared/schema"
)

// Phase is the sequencer state.
const (
	PhaseIdle     = "idle"
	PhaseStarting = "starting"
	PhaseRunning  = "running"
	PhaseStopping = "stopping"
	// Idle standby (optional, configured with [[standby]]/[[wake]]).
	PhaseEnteringStandby = "entering_standby"
	PhaseStandby         = "standby"
	PhaseWaking          = "waking"
)

// Step kinds.
const (
	KindCmd        = "cmd"
	KindWaitStatus = "wait_status"
	KindWaitState  = "wait_state"
	KindDelay      = "delay"
	KindWOL        = "wol"
)

// Logger is the minimal logging surface the sequencer uses. Errorf is for
// lost state/data (publish or marshal failed, malformed payload dropped);
// Warnf is for degraded-but-recovering events (logging.md §3).
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Publisher is the MQTT surface the sequencer emits /cmd and its own /state on.
// Publish must not block indefinitely — a broker outage must surface as an error
// (the mqtt layer bounds paho's Wait with a timeout) so the runner never stalls.
type Publisher interface {
	Publish(topic string, retained bool, payload []byte) error
}

// Step is one config-driven sequence step. Slot addresses are SITE-RELATIVE
// (e.g. "power/master", "hf/switch"); New resolves them to <site>/<slot>.
//
// value is always a string — the stationa /cmd value-key convention carries the
// argument under `value` as a string (model value_type is string|int|float, no
// bool), matching the live wire format (set_power "on", set_enabled "true").
type Step struct {
	Name string
	Kind string // KindCmd | KindWaitStatus | KindWaitState | KindDelay

	// cmd
	Slot   string // site-relative
	Action string
	Value  string
	Retain *bool // nil → true (controlled /cmd retained, model §8)

	// wait_status
	Slots []string // site-relative, logical AND
	State string   // "online" (default) | "offline"

	// wait_state
	Field string // top-level JSON key in the /state snapshot

	// waits (wait_status + wait_state)
	HoldMs   *int // debounce: condition must hold this long; nil → DefaultHold, explicit 0 = edge-triggered
	TimeoutS *int // per-step override; nil → StepTimeout

	// delay
	DurationS *int   // literal seconds
	Duration  string // "network" | "stagger" (symbolic ref into [timing])

	// wol
	MAC       string // target NIC MAC
	Broadcast string // host:port; "" → DefaultWOLBroadcast
}

// DefaultWOLBroadcast is the wol destination when a step omits broadcast.
const DefaultWOLBroadcast = "255.255.255.255:9"

// Watch is one extra idle-activity source: a change of a /state field.
type Watch struct {
	Slot     string  // site-relative
	Field    string  // top-level /state key
	MinDelta float64 // numeric deadband (0 = any change counts)
}

// WOLSender sends a magic packet for mac to addr (host:port). Injected so tests
// do not touch the network.
type WOLSender func(mac net.HardwareAddr, addr string) error

// Config is the subset of config the sequencer needs.
type Config struct {
	Site            string
	Station         string
	Slot            string
	Location        string
	Host            string
	DiscoveryPrefix string

	Startup  []Step
	Shutdown []Step
	Standby  []Step // optional idle power-down
	Wake     []Step // optional revive (required with Standby)

	// Idle standby triggers (used only when Standby/Wake are configured).
	ActivitySlot string        // site-relative; /state.activity + last_activity
	IdleTimeout  time.Duration // watch quiet window + minimum running time
	Watches      []Watch
	IdleCheck    time.Duration // idle evaluation cadence (0 → 1 s)

	// WOL sends wol steps; nil → SendMagicPacket.
	WOL WOLSender

	NetworkDelay    time.Duration // delay "network"
	StepTimeout     time.Duration // default wait deadline
	ShutdownStagger time.Duration // delay "stagger"
	PollInterval    time.Duration // wait loop cadence
	DefaultHold     time.Duration // default hold_ms (0 = edge-triggered)
}

// resolvedStep is a Step resolved to absolute slot addresses + time.Durations.
type resolvedStep struct {
	Name     string
	Kind     string
	Slot     string // absolute (cmd / wait_state)
	Action   string
	Value    string
	Retain   bool
	Slots    []string // absolute (wait_status)
	State    string   // "online" | "offline"
	Field    string
	Hold     time.Duration
	Timeout  time.Duration
	Duration time.Duration // delay
	MAC      net.HardwareAddr
	Bcast    string
}

// resolvedWatch is a Watch resolved to an absolute slot, plus its change state.
type resolvedWatch struct {
	Slot     string
	Field    string
	MinDelta float64
	anchor   string // last counted value ("" = not seen yet)
}

// Sequencer is the station startup/shutdown state machine.
type Sequencer struct {
	cfg  Config
	pub  Publisher
	log  Logger
	self string // <site>/<station>/<slot>

	startup  []resolvedStep
	shutdown []resolvedStep
	standby  []resolvedStep
	wake     []resolvedStep

	activitySlot string // absolute; "" = idle standby disabled
	watches      []*resolvedWatch
	wol          WOLSender

	mu     sync.Mutex
	status map[string]string // absolute slot → "online"|"offline" (absent = unseen)
	state  map[string]any    // absolute slot → parsed /state JSON (map[string]any)
	online bool              // broker connection state (set by the mqtt layer)

	phase string
	step  string
	fault string

	runningSince time.Time // last entry into phase=running
	standbySince time.Time // last entry into phase=standby
	lastWatch    time.Time // last counted watch change

	cmdCh       chan string   // "start" | "stop" → runner
	republishCh chan struct{} // mqtt layer → runner: re-publish retained /state
}

// New constructs a Sequencer, resolving the step lists to absolute addresses
// and validating self-waits (a wait_state on the sequencer's own slot is a
// config error: the sequencer's /state is an output, not an input).
func New(cfg Config, pub Publisher, log Logger) (*Sequencer, error) {
	self := schema.SlotBase(cfg.Site, cfg.Station, cfg.Slot)
	startup, err := resolve(cfg.Startup, cfg, self, "startup")
	if err != nil {
		return nil, err
	}
	shutdown, err := resolve(cfg.Shutdown, cfg, self, "shutdown")
	if err != nil {
		return nil, err
	}
	standby, err := resolve(cfg.Standby, cfg, self, "standby")
	if err != nil {
		return nil, err
	}
	wake, err := resolve(cfg.Wake, cfg, self, "wake")
	if err != nil {
		return nil, err
	}
	if (len(standby) == 0) != (len(wake) == 0) {
		return nil, fmt.Errorf("standby and wake must both be configured or both absent")
	}
	s := &Sequencer{
		cfg:         cfg,
		pub:         pub,
		log:         log,
		self:        self,
		startup:     startup,
		shutdown:    shutdown,
		standby:     standby,
		wake:        wake,
		wol:         cfg.WOL,
		status:      map[string]string{},
		state:       map[string]any{},
		phase:       PhaseIdle,
		cmdCh:       make(chan string, 4),
		republishCh: make(chan struct{}, 1),
	}
	if s.wol == nil {
		s.wol = SendMagicPacket
	}
	if len(standby) > 0 {
		if cfg.ActivitySlot == "" {
			return nil, fmt.Errorf("standby/wake need an activity slot")
		}
		if cfg.IdleTimeout <= 0 {
			return nil, fmt.Errorf("standby/wake need an idle timeout > 0")
		}
		s.activitySlot = cfg.Site + "/" + cfg.ActivitySlot
		for _, w := range cfg.Watches {
			s.watches = append(s.watches, &resolvedWatch{Slot: cfg.Site + "/" + w.Slot, Field: w.Field, MinDelta: w.MinDelta})
		}
	}
	return s, nil
}

// standbyEnabled reports whether the idle standby feature is configured.
func (s *Sequencer) standbyEnabled() bool { return s.activitySlot != "" }

func resolve(steps []Step, cfg Config, self, list string) ([]resolvedStep, error) {
	out := make([]resolvedStep, 0, len(steps))
	for _, st := range steps {
		r := resolvedStep{
			Name:    st.Name,
			Kind:    st.Kind,
			Action:  st.Action,
			Value:   st.Value,
			Retain:  true,
			Slots:   make([]string, len(st.Slots)),
			State:   st.State,
			Field:   st.Field,
			Hold:    cfg.DefaultHold, // nil hold_ms → the global default (0 = edge-triggered)
			Timeout: cfg.StepTimeout,
		}
		if r.State == "" {
			r.State = "online"
		}
		if st.Retain != nil {
			r.Retain = *st.Retain
		}
		if st.TimeoutS != nil {
			r.Timeout = time.Duration(*st.TimeoutS) * time.Second
		}
		// An explicit hold_ms (including 0 = edge-triggered) overrides the default,
		// distinguishing "omitted" from "explicitly 0" — the *int mirrors Retain/TimeoutS.
		if st.HoldMs != nil {
			r.Hold = time.Duration(*st.HoldMs) * time.Millisecond
		}
		switch st.Kind {
		case KindCmd, KindWaitState:
			r.Slot = cfg.Site + "/" + st.Slot
			if st.Kind == KindWaitState && r.Slot == self {
				return nil, fmt.Errorf("%s step %q: wait_state on the sequencer's own slot (%s) is not allowed (own /state is an output)", list, st.Name, self)
			}
		case KindWaitStatus:
			for j, s := range st.Slots {
				r.Slots[j] = cfg.Site + "/" + s
			}
		case KindDelay:
			d, err := resolveDelay(st, cfg)
			if err != nil {
				return nil, fmt.Errorf("%s step %q: %w", list, st.Name, err)
			}
			r.Duration = d
		case KindWOL:
			mac, err := net.ParseMAC(st.MAC)
			if err != nil {
				return nil, fmt.Errorf("%s step %q: wol mac: %w", list, st.Name, err)
			}
			r.MAC = mac
			r.Bcast = st.Broadcast
			if r.Bcast == "" {
				r.Bcast = DefaultWOLBroadcast
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func resolveDelay(st Step, cfg Config) (time.Duration, error) {
	if st.DurationS != nil {
		return time.Duration(*st.DurationS) * time.Second, nil
	}
	switch st.Duration {
	case "network":
		return cfg.NetworkDelay, nil
	case "stagger":
		return cfg.ShutdownStagger, nil
	default:
		return 0, fmt.Errorf("delay: unknown symbolic duration %q", st.Duration)
	}
}

// SelfBase returns the sequencer's own slot base (for the mqtt layer to publish
// meta/state/status/cmd).
func (s *Sequencer) SelfBase() string { return s.self }

// CmdTopic returns the sequencer's own /cmd topic (NOT retained, one-shot).
func (s *Sequencer) CmdTopic() string { return s.self + "/cmd" }

// Subscriptions returns the slot addresses the mqtt layer should subscribe,
// derived from the configured sequence (union of every slot referenced by any
// cmd, wait_status, or wait_state step). The /status of every such slot is
// subscribed (so the sequencer observes outage/liveness of every slot it
// touches, not only the ones it waits on — model §7.1); the /state of every
// wait_state slot and every wait_status state="online" slot (for its
// device_online) is subscribed. With idle standby configured, the activity
// slot and every watch slot are subscribed too (/status + /state).
func (s *Sequencer) Subscriptions() (statusSlots, stateSlots []string) {
	status := map[string]struct{}{}
	state := map[string]struct{}{}
	note := func(slot string) { status[slot] = struct{}{} }
	if s.standbyEnabled() {
		note(s.activitySlot)
		state[s.activitySlot] = struct{}{}
		for _, w := range s.watches {
			note(w.Slot)
			state[w.Slot] = struct{}{}
		}
	}
	for _, st := range s.allSteps() {
		switch st.Kind {
		case KindCmd:
			note(st.Slot)
		case KindWaitStatus:
			for _, slot := range st.Slots {
				note(slot)
				if st.State == "online" {
					state[slot] = struct{}{}
				}
			}
		case KindWaitState:
			note(st.Slot)
			state[st.Slot] = struct{}{}
		}
	}
	return sortedKeys(status), sortedKeys(state)
}

// allSteps returns every configured step across all lists.
func (s *Sequencer) allSteps() []resolvedStep {
	out := append([]resolvedStep{}, s.startup...)
	out = append(out, s.shutdown...)
	out = append(out, s.standby...)
	return append(out, s.wake...)
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SetStatus records a liveness observation from a slot's /status. Called from
// the mqtt layer (a /status publish — retained or live).
func (s *Sequencer) SetStatus(slot string, online bool) {
	s.mu.Lock()
	if online {
		s.status[slot] = "online"
	} else {
		s.status[slot] = "offline"
	}
	s.mu.Unlock()
}

// SetState records a /state snapshot from a subscribed /state slot. Called from the
// mqtt layer. A malformed payload drops the slot's prior snapshot (logged) so a
// good→malformed transition does NOT keep the stale prior value — the wait then
// sees no field and yields false for that poll, not a poisoned map.
func (s *Sequencer) SetState(slot string, payload []byte) {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		// Malformed payload dropped (the prior snapshot is deleted with it).
		s.log.Errorf("bad %s/state: %v", slot, err)
		s.mu.Lock()
		delete(s.state, slot)
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.state[slot] = m
	s.noteWatchesLocked(slot, m)
	s.mu.Unlock()
}

// noteWatchesLocked records a counted change for every watch on slot whose
// field moved by at least its deadband since the last counted value. A change
// counts only while the slot is fit (bridge online, device_online not false)
// and only between two real values: a field appearing or vanishing (device
// powered down, invalid readback) is not activity — otherwise switching the
// antenna power off in standby would read as activity and wake the station.
func (s *Sequencer) noteWatchesLocked(slot string, m map[string]any) {
	fit := s.status[slot] == "online" && coerceToString(m["device_online"]) != "false"
	for _, w := range s.watches {
		if w.Slot != slot {
			continue
		}
		v := coerceToString(m[w.Field])
		if v == "" || !fit {
			continue
		}
		if w.anchor == "" {
			w.anchor = v // first real value is the baseline, not activity
			continue
		}
		if watchMoved(w.anchor, v, w.MinDelta) {
			w.anchor = v
			s.lastWatch = time.Now()
		}
	}
}

// watchMoved reports whether cur differs from anchor by at least minDelta
// (numeric fields) or at all (non-numeric, or minDelta 0).
func watchMoved(anchor, cur string, minDelta float64) bool {
	if anchor == cur {
		return false
	}
	if minDelta <= 0 {
		return true
	}
	a, errA := strconv.ParseFloat(anchor, 64)
	c, errC := strconv.ParseFloat(cur, 64)
	if errA != nil || errC != nil {
		return true
	}
	return math.Abs(c-a) >= minDelta
}

// SetBrokerOnline records the broker connection state. A wait or cmd step
// faults when the broker is down rather than stalling on stale/buffered data.
func (s *Sequencer) SetBrokerOnline(online bool) {
	s.mu.Lock()
	s.online = online
	s.mu.Unlock()
}

// Start requests a startup sequence. Non-blocking; honored when phase=idle
// (retries/clears a prior fault) or phase=standby (= wake); dropped (logged)
// when busy.
func (s *Sequencer) Start() { s.request("start") }

// Stop requests a shutdown sequence. Non-blocking; honored when phase=running,
// phase=standby or phase=idle — the teardown is idempotent and can only de-energize, and the
// fault latch is in-memory (a process restart wipes it), so stop-from-idle
// must not depend on remembering why it is idle. Dropped (logged) mid-sequence.
func (s *Sequencer) Stop() { s.request("stop") }

func (s *Sequencer) request(cmd string) {
	s.mu.Lock()
	phase, fault := s.phase, s.fault
	allow := false
	switch cmd {
	case "start":
		allow = phase == PhaseIdle || phase == PhaseStandby
	case "stop":
		// Idle is honored regardless of fault: the in-memory fault latch dies
		// with the process, so after a restart a stop is the ONLY teardown
		// path for the slots a faulted sequence left energized — and a
		// shutdown re-states "off", it never energizes.
		allow = phase == PhaseRunning || phase == PhaseIdle || phase == PhaseStandby
	}
	s.mu.Unlock()
	if !allow {
		s.log.Warnf("cmd %s ignored: phase=%s fault=%q", cmd, phase, fault)
		return
	}
	select {
	case s.cmdCh <- cmd:
	default:
		s.log.Warnf("cmd %s dropped: runner queue full", cmd)
	}
}

// Phase returns the current phase/step/fault (for tests + status publishing).
func (s *Sequencer) Phase() (phase, step, fault string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase, s.step, s.fault
}

// Run is the runner goroutine. It executes one sequence at a time and exits on
// ctx cancel. Publishes the initial idle state on entry — overwriting any stale
// retained /state from a prior crash so the bus sees phase=idle on boot (own
// /state is an output; the sequencer never reads it back).
func (s *Sequencer) Run(ctx context.Context) {
	s.publishState()
	var tick <-chan time.Time
	if s.standbyEnabled() {
		every := s.cfg.IdleCheck
		if every <= 0 {
			every = time.Second
		}
		t := time.NewTicker(every)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			// Idle standby / wake run on the runner goroutine like start/stop,
			// so they serialize with operator commands through begin().
			cmd := s.idleDecision(time.Now())
			if cmd == "" {
				continue
			}
			steps, endPhase, ok := s.begin(cmd)
			if !ok {
				continue
			}
			s.runSequence(ctx, steps, endPhase)
		case cmd := <-s.cmdCh:
			// begin() is the AUTHORITATIVE busy guard: it re-checks phase and
			// transitions it atomically under mu before running. request()'s
			// phase check is only a fast-path drop — two rapid same-type commands
			// can both pass request() before the runner transitions phase, but
			// only the first passes begin() (the second sees the already-moved
			// phase), closing the TOCTOU window.
			steps, endPhase, ok := s.begin(cmd)
			if !ok {
				s.log.Warnf("cmd %s dropped: busy guard re-check failed (already in progress)", cmd)
				continue
			}
			s.runSequence(ctx, steps, endPhase)
		case <-s.republishCh:
			s.publishState()
		}
	}
}

// RequestRepublish asks the runner to re-publish the sequencer's retained
// /state on its next loop. Non-blocking (buffered 1, coalescing). Used by the
// mqtt layer on (re)connect so the publish happens on the runner goroutine —
// never the paho callback (the stationa memory: a paho handler must not call a
// blocking Publish). After a broker wipe that drops retained messages this
// restores an idle sequencer's /state instead of leaving it absent.
func (s *Sequencer) RequestRepublish() {
	select {
	case s.republishCh <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// sequences
// ---------------------------------------------------------------------------

// idleDecision evaluates the idle standby triggers at now and returns
// "standby", "wake" or "" (nothing to do). Evaluated only with a live broker
// (stale inputs must not power anything down or up).
//
//   - standby (from running): the activity slot is fit and reports
//     activity=inactive, phase=running has lasted the idle timeout (no standby
//     right after a start or wake, even while antenna-select still says
//     inactive), and no watch moved within the idle timeout.
//   - wake (from standby): the activity slot reports a last_activity newer than
//     the standby entry (a real VFO change / TX / operator hold — an
//     antennaselect restart publishes no last_activity, so it never wakes), or
//     a watch moved after the standby entry.
func (s *Sequencer) idleDecision(now time.Time) string {
	if !s.standbyEnabled() {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.online {
		return ""
	}
	m, _ := s.state[s.activitySlot].(map[string]any)
	switch s.phase {
	case PhaseRunning:
		fit := s.status[s.activitySlot] == "online" && m != nil && coerceToString(m["device_online"]) != "false"
		if !fit || coerceToString(m["activity"]) != "inactive" {
			return ""
		}
		if now.Sub(s.runningSince) < s.cfg.IdleTimeout || now.Sub(s.lastWatch) < s.cfg.IdleTimeout {
			return ""
		}
		return "standby"
	case PhaseStandby:
		if s.lastWatch.After(s.standbySince) {
			return "wake"
		}
		if m == nil {
			return ""
		}
		if ts, err := time.Parse(time.RFC3339Nano, coerceToString(m["last_activity"])); err == nil && ts.After(s.standbySince) {
			return "wake"
		}
	}
	return ""
}

// begin atomically re-checks the busy guard and transitions phase to the
// sequence's mid-phase, returning the resolved step list + end phase. ok=false
// if the command is no longer allowed (the runner drops it).
func (s *Sequencer) begin(cmd string) (steps []resolvedStep, endPhase string, ok bool) {
	s.mu.Lock()
	switch cmd {
	case "start":
		switch s.phase {
		case PhaseIdle:
			s.phase = PhaseStarting
			s.step = ""
			s.fault = ""
			steps, endPhase, ok = s.startup, PhaseRunning, true
		case PhaseStandby:
			// start from standby = wake (the station is energized already).
			s.phase = PhaseWaking
			s.step = ""
			s.fault = ""
			steps, endPhase, ok = s.wake, PhaseRunning, true
		}
	case "stop":
		// Same idle rule as request(): stop must not depend on the in-memory
		// fault latch surviving a restart (idempotent teardown, de-energize
		// only).
		if s.phase == PhaseRunning || s.phase == PhaseIdle || s.phase == PhaseStandby {
			s.phase = PhaseStopping
			s.step = ""
			s.fault = ""
			steps, endPhase, ok = s.shutdown, PhaseIdle, true
		}
	case "standby":
		if s.phase == PhaseRunning && s.standbyEnabled() {
			s.phase = PhaseEnteringStandby
			s.step = ""
			s.fault = ""
			steps, endPhase, ok = s.standby, PhaseStandby, true
		}
	case "wake":
		if s.phase == PhaseStandby && s.standbyEnabled() {
			s.phase = PhaseWaking
			s.step = ""
			s.fault = ""
			steps, endPhase, ok = s.wake, PhaseRunning, true
		}
	}
	s.mu.Unlock()
	if ok {
		s.publishState()
	}
	return steps, endPhase, ok
}

func (s *Sequencer) runSequence(ctx context.Context, steps []resolvedStep, endPhase string) {
	for _, st := range steps {
		if ctx.Err() != nil {
			s.abort(st.Name + ": interrupted")
			return
		}
		s.setStep(st.Name)
		reason, ok := s.execStep(ctx, &st)
		if !ok {
			s.abort(st.Name + ": " + reason)
			return
		}
	}
	s.setPhase(endPhase, "", "")
}

// execStep executes one resolved step, returning (reason, ok). ok=false aborts
// the sequence; reason is appended to the step name in the fault string.
func (s *Sequencer) execStep(ctx context.Context, st *resolvedStep) (string, bool) {
	switch st.Kind {
	case KindCmd:
		if !s.brokerOnline() {
			return "broker disconnected", false
		}
		payload := cmdPayload{Action: st.Action, Value: st.Value}
		b, _ := json.Marshal(payload)
		if err := s.pub.Publish(st.Slot+"/cmd", st.Retain, b); err != nil {
			return "publish failed: " + err.Error(), false
		}
		return "", true
	case KindDelay:
		if !s.sleepCtx(ctx, st.Duration) {
			return "interrupted", false
		}
		return "", true
	case KindWOL:
		if err := s.wol(st.MAC, st.Bcast); err != nil {
			return "wol failed: " + err.Error(), false
		}
		return "", true
	case KindWaitStatus:
		reason, ok := s.wait(ctx, st, func() bool {
			for _, slot := range st.Slots {
				if st.State == "online" {
					// Two-layer liveness: bridge LWT AND device_online. The
					// bridge LWT alone is online while the device behind it
					// is still booting (flexbridge on scmino, 2026-10-18).
					if !s.isOnline(slot) {
						return false
					}
				} else if s.statusOf(slot) != st.State {
					return false
				}
			}
			return true
		})
		return s.withObserved(st.Slots, "", reason, ok)
	case KindWaitState:
		reason, ok := s.wait(ctx, st, func() bool {
			if !s.isOnline(st.Slot) {
				return false // implicit liveness precondition
			}
			return s.stateField(st.Slot, st.Field) == st.Value
		})
		return s.withObserved([]string{st.Slot}, st.Field, reason, ok)
	default:
		return "unknown step kind " + st.Kind, false
	}
}

// withObserved appends what the sequencer last saw for slots to a wait
// timeout, so the fault on the bus and in the journal says WHY the condition
// never held (2026-10-18: "wait-radio-online: timeout" alone did not tell a
// stale device_online from a missing /state).
func (s *Sequencer) withObserved(slots []string, field, reason string, ok bool) (string, bool) {
	if ok || reason != "timeout" {
		return reason, ok
	}
	parts := make([]string, 0, len(slots))
	for _, slot := range slots {
		parts = append(parts, s.observed(slot, field))
	}
	return reason + " (" + strings.Join(parts, "; ") + ")", false
}

// observed renders a slot's last /status and /state view, e.g.
// "hf/radio status=online device_online=false".
func (s *Sequencer) observed(slot, field string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.status[slot]
	if status == "" {
		status = "none"
	}
	out := strings.TrimPrefix(slot, s.cfg.Site+"/") + " status=" + status
	m, ok := s.state[slot].(map[string]any)
	if !ok {
		return out + " state=none"
	}
	if v, present := m["device_online"]; present {
		out += " device_online=" + coerceToString(v)
	}
	if field != "" && field != "device_online" {
		out += " " + field + "=" + coerceToString(m[field])
	}
	return out
}

// wait polls cond with a deadline + optional hold (debounce). It faults on ctx
// cancel, broker disconnect, or timeout. Returns (reason, ok).
func (s *Sequencer) wait(ctx context.Context, st *resolvedStep, cond func() bool) (string, bool) {
	deadline := time.Now().Add(st.Timeout)
	hold := st.Hold
	var heldSince time.Time
	for {
		if ctx.Err() != nil {
			return "interrupted", false
		}
		if !s.brokerOnline() {
			return "broker disconnected", false
		}
		if time.Now().After(deadline) {
			return "timeout", false
		}
		// cond's readers (statusOf/isOnline/stateField) each take s.mu for their
		// own short read; we do NOT hold it here — that would self-deadlock.
		ok := cond()
		if ok {
			if heldSince.IsZero() {
				heldSince = time.Now()
			}
			if hold <= 0 || time.Since(heldSince) >= hold {
				return "", true
			}
		} else {
			heldSince = time.Time{} // condition broke → restart the hold window
		}
		if !s.sleepCtx(ctx, s.cfg.PollInterval) {
			return "interrupted", false
		}
	}
}

// sleepCtx sleeps for d, returning false if ctx cancels first.
func (s *Sequencer) sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// abort records a fault and returns the sequencer to idle. The slots driven so
// far remain in whatever state the retained /cmd last set them (self-healing,
// model §8) — NO rollback.
func (s *Sequencer) abort(reason string) {
	s.log.Warnf("sequence aborted: %s", reason)
	s.setPhase(PhaseIdle, "", reason)
}

// ---------------------------------------------------------------------------
// observed inputs (mu-guarded)
// ---------------------------------------------------------------------------

func (s *Sequencer) brokerOnline() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.online
}

// statusOf returns the last /status observation: "online", "offline", or "" if
// the slot has never published (absence is distinct from an explicit offline —
// a wait_status state="offline" requires an actual offline payload, never an
// absent one).
func (s *Sequencer) statusOf(slot string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status[slot]
}

// isOnline reports whether a slot is fit to act on: the bridge /status LWT says
// "online" AND, when the /state doc carries the field, the fronted device is reachable
// (device_online, model §3). Gating on the LWT alone let a dead device pass a wait_state
// on its stale retained /state: bridge up, serial link dead, pre-outage power:"on"
// retained — the sequencer would proceed to arm on an unreachable PA (fixed 2026-09-03).
// An absent device_online field (slots that do not report it) does not block.
func (s *Sequencer) isOnline(slot string) bool {
	if s.statusOf(slot) != "online" {
		return false
	}
	if v := s.stateField(slot, "device_online"); v == "false" {
		return false
	}
	return true
}

// stateField returns a /state top-level field coerced to a string for
// comparison against the configured value. Absent/nil → "".
func (s *Sequencer) stateField(slot, field string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.state[slot].(map[string]any)
	if !ok {
		return ""
	}
	return coerceToString(m[field])
}

// coerceToString maps a JSON-decoded value to a string for value comparison:
// bool → "true"/"false", number → decimal string, string → as-is, nil → "".
func coerceToString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case bool:
		if x {
			return "true"
		}
		return "false"
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// ---------------------------------------------------------------------------
// phase + publish
// ---------------------------------------------------------------------------

func (s *Sequencer) setPhase(phase, step, fault string) {
	s.mu.Lock()
	switch {
	case phase == PhaseRunning && s.phase != PhaseRunning:
		s.runningSince = time.Now()
	case phase == PhaseStandby && s.phase != PhaseStandby:
		s.standbySince = time.Now()
	}
	prev := s.phase
	s.phase = phase
	s.step = step
	s.fault = fault
	s.mu.Unlock()
	if prev != phase {
		s.log.Infof("phase %s -> %s", prev, phase)
	}
	s.publishState()
}

func (s *Sequencer) setStep(step string) {
	s.mu.Lock()
	s.step = step
	s.fault = ""
	s.mu.Unlock()
	s.publishState()
}

type statePayload struct {
	TS    string `json:"ts"`
	Phase string `json:"phase"`
	Step  string `json:"step"` // always present (model §7.1; only fault is omitempty)
	Fault string `json:"fault,omitempty"`
}

// publishState publishes the retained sequencer /state snapshot. A publish
// failure (broker down) is logged, not fatal — the runner keeps going.
func (s *Sequencer) publishState() {
	s.mu.Lock()
	p := statePayload{
		TS:    time.Now().UTC().Format(time.RFC3339),
		Phase: s.phase,
		Step:  s.step,
		Fault: s.fault,
	}
	s.mu.Unlock()
	b, err := json.Marshal(p)
	if err != nil {
		s.log.Errorf("marshal state: %v", err)
		return
	}
	if err := s.pub.Publish(s.self+"/state", true, b); err != nil {
		// Publish failure lost state — Error, not Warn (logging.md §3).
		s.log.Errorf("publish %s/state: %v", s.self, err)
	}
}

// ---------------------------------------------------------------------------
// command emitters (retained steady-state /cmd; value-key convention)
// ---------------------------------------------------------------------------

type cmdPayload struct {
	Action string `json:"action"`
	Value  string `json:"value"`
}

// MetaPayload returns the sequencer /meta birth certificate as raw JSON. The
// mqtt layer publishes it (retained) on (re)connect. controls/watches are
// derived from the configured sequence (model §7.1): controls = slots the
// sequencer emits /cmd to; watches = slots it observes (cmd + wait targets).
func (s *Sequencer) MetaPayload() []byte {
	controlSet, watchSet := map[string]struct{}{}, map[string]struct{}{}
	if s.standbyEnabled() {
		watchSet[s.activitySlot] = struct{}{}
		for _, w := range s.watches {
			watchSet[w.Slot] = struct{}{}
		}
	}
	for _, st := range s.allSteps() {
		switch st.Kind {
		case KindCmd:
			controlSet[st.Slot] = struct{}{}
			watchSet[st.Slot] = struct{}{}
		case KindWaitStatus:
			for _, slot := range st.Slots {
				watchSet[slot] = struct{}{}
			}
		case KindWaitState:
			watchSet[st.Slot] = struct{}{}
		}
	}
	meta := map[string]any{
		"schema":   "1.0",
		"role":     "sequencer",
		"link":     "none",
		"location": s.cfg.Location,
		"host":     s.cfg.Host,
		"capabilities": map[string]any{
			// The ordered slots this sequencer drives (model §7.1). It is one
			// writer of these but does not lock them — they stay directly
			// toggleable for troubleshooting while the sequencer is idle.
			"controls": sortedKeys(controlSet),
			"watches":  sortedKeys(watchSet),
		},
		// Logic slot: it reacts to state and emits intent (model §1). phase is
		// an enum (the sequencer state); step/fault are free strings. No
		// writable fields — the operator surface is the one-shot /cmd
		// (start|stop), which is not retained and so not an exposed field.
		"expose": map[string]any{
			"device": map[string]any{"name": "Station power sequencer"},
			"fields": []map[string]any{
				{"key": "phase", "name": "Phase", "type": "enum",
					"options": []string{PhaseIdle, PhaseStarting, PhaseRunning, PhaseStopping,
						PhaseEnteringStandby, PhaseStandby, PhaseWaking}},
				{"key": "step", "name": "Step", "type": "string"},
				{"key": "fault", "name": "Fault", "type": "string"},
			},
		},
	}
	b, _ := json.Marshal(meta)
	return b
}

// String returns a short debug representation.
func (s *Sequencer) String() string {
	phase, step, fault := s.Phase()
	if fault != "" {
		return fmt.Sprintf("powerseq{phase=%s step=%s fault=%s}", phase, step, fault)
	}
	return fmt.Sprintf("powerseq{phase=%s step=%s}", phase, step)
}

// SendMagicPacket sends a Wake-on-LAN magic packet (6×0xFF + 16×MAC) for mac to
// addr (host:port, usually a broadcast address) over UDP.
func SendMagicPacket(mac net.HardwareAddr, addr string) error {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(MagicPacket(mac))
	return err
}

// MagicPacket builds the Wake-on-LAN payload for mac.
func MagicPacket(mac net.HardwareAddr) []byte {
	p := make([]byte, 0, 6+16*len(mac))
	for i := 0; i < 6; i++ {
		p = append(p, 0xFF)
	}
	for i := 0; i < 16; i++ {
		p = append(p, mac...)
	}
	return p
}
