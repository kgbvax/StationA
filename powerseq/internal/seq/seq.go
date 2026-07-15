// Package seq owns the station startup/shutdown state machine for the powerseq
// sequencer (integration model `sequencer` role, logic slot muehle/hf/power-seq).
//
// The sequence is CONFIG-DRIVEN: a pair of ordered step lists (startup/shutdown)
// supplied to New. Each step is one of four kinds —
//
//	cmd          emit a retained /cmd {action, value} to a slot
//	wait_status  wait until N slots' /status == online|offline
//	wait_state   wait until a slot's /state top-level field == value
//	             (implicit precondition: the slot's /status must be online, so
//	             a dead device whose LWT fired cannot pass on a stale retained
//	             /state — the fix for the dead-device masking case)
//	delay        sleep a fixed duration (a literal duration_s or a symbolic
//	             "network"/"stagger" ref into [timing])
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
	"sort"
	"strconv"
	"sync"
	"time"
)

// Phase is the sequencer state.
const (
	PhaseIdle     = "idle"
	PhaseStarting = "starting"
	PhaseRunning  = "running"
	PhaseStopping = "stopping"
)

// Step kinds.
const (
	KindCmd        = "cmd"
	KindWaitStatus = "wait_status"
	KindWaitState  = "wait_state"
	KindDelay      = "delay"
)

// Logger is the minimal logging surface the sequencer uses.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
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
}

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
}

// Sequencer is the station startup/shutdown state machine.
type Sequencer struct {
	cfg  Config
	pub  Publisher
	log  Logger
	self string // <site>/<station>/<slot>

	startup  []resolvedStep
	shutdown []resolvedStep

	mu     sync.Mutex
	status map[string]string // absolute slot → "online"|"offline" (absent = unseen)
	state  map[string]any    // absolute slot → parsed /state JSON (map[string]any)
	online bool              // broker connection state (set by the mqtt layer)

	phase string
	step  string
	fault string

	cmdCh       chan string   // "start" | "stop" → runner
	republishCh chan struct{} // mqtt layer → runner: re-publish retained /state
}

// New constructs a Sequencer, resolving the step lists to absolute addresses
// and validating self-waits (a wait_state on the sequencer's own slot is a
// config error: the sequencer's /state is an output, not an input).
func New(cfg Config, pub Publisher, log Logger) (*Sequencer, error) {
	self := cfg.Site + "/" + cfg.Station + "/" + cfg.Slot
	startup, err := resolve(cfg.Startup, cfg, self, "startup")
	if err != nil {
		return nil, err
	}
	shutdown, err := resolve(cfg.Shutdown, cfg, self, "shutdown")
	if err != nil {
		return nil, err
	}
	return &Sequencer{
		cfg:         cfg,
		pub:         pub,
		log:         log,
		self:        self,
		startup:     startup,
		shutdown:    shutdown,
		status:      map[string]string{},
		state:       map[string]any{},
		phase:       PhaseIdle,
		cmdCh:       make(chan string, 4),
		republishCh: make(chan struct{}, 1),
	}, nil
}

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
// wait_state slot is subscribed.
func (s *Sequencer) Subscriptions() (statusSlots, stateSlots []string) {
	status := map[string]struct{}{}
	state := map[string]struct{}{}
	note := func(slot string) { status[slot] = struct{}{} }
	for _, st := range append(append([]resolvedStep{}, s.startup...), s.shutdown...) {
		switch st.Kind {
		case KindCmd:
			note(st.Slot)
		case KindWaitStatus:
			for _, slot := range st.Slots {
				note(slot)
			}
		case KindWaitState:
			note(st.Slot)
			state[st.Slot] = struct{}{}
		}
	}
	return sortedKeys(status), sortedKeys(state)
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

// SetState records a /state snapshot from a wait_state slot. Called from the
// mqtt layer. A malformed payload drops the slot's prior snapshot (logged) so a
// good→malformed transition does NOT keep the stale prior value — the wait then
// sees no field and yields false for that poll, not a poisoned map.
func (s *Sequencer) SetState(slot string, payload []byte) {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		s.log.Warnf("bad %s/state: %v", slot, err)
		s.mu.Lock()
		delete(s.state, slot)
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.state[slot] = m
	s.mu.Unlock()
}

// SetBrokerOnline records the broker connection state. A wait or cmd step
// faults when the broker is down rather than stalling on stale/buffered data.
func (s *Sequencer) SetBrokerOnline(online bool) {
	s.mu.Lock()
	s.online = online
	s.mu.Unlock()
}

// Start requests a startup sequence. Non-blocking; honored only when phase=idle
// (retries/clears a prior fault); dropped (logged) when busy.
func (s *Sequencer) Start() { s.request("start") }

// Stop requests a shutdown sequence. Non-blocking; honored when phase=running
// OR phase=idle with a fault (resume an interrupted shutdown — idempotent),
// dropped (logged) otherwise.
func (s *Sequencer) Stop() { s.request("stop") }

func (s *Sequencer) request(cmd string) {
	s.mu.Lock()
	phase, fault := s.phase, s.fault
	allow := false
	switch cmd {
	case "start":
		allow = phase == PhaseIdle
	case "stop":
		allow = phase == PhaseRunning || (phase == PhaseIdle && fault != "")
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
	for {
		select {
		case <-ctx.Done():
			return
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

// begin atomically re-checks the busy guard and transitions phase to the
// sequence's mid-phase, returning the resolved step list + end phase. ok=false
// if the command is no longer allowed (the runner drops it).
func (s *Sequencer) begin(cmd string) (steps []resolvedStep, endPhase string, ok bool) {
	s.mu.Lock()
	switch cmd {
	case "start":
		if s.phase == PhaseIdle {
			s.phase = PhaseStarting
			s.step = ""
			s.fault = ""
			steps, endPhase, ok = s.startup, PhaseRunning, true
		}
	case "stop":
		if s.phase == PhaseRunning || (s.phase == PhaseIdle && s.fault != "") {
			s.phase = PhaseStopping
			s.step = ""
			s.fault = ""
			steps, endPhase, ok = s.shutdown, PhaseIdle, true
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
	case KindWaitStatus:
		return s.wait(ctx, st, func() bool {
			for _, slot := range st.Slots {
				if s.statusOf(slot) != st.State {
					return false
				}
			}
			return true
		})
	case KindWaitState:
		return s.wait(ctx, st, func() bool {
			if !s.isOnline(st.Slot) {
				return false // implicit liveness precondition
			}
			return s.stateField(st.Slot, st.Field) == st.Value
		})
	default:
		return "unknown step kind " + st.Kind, false
	}
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

func (s *Sequencer) isOnline(slot string) bool { return s.statusOf(slot) == "online" }

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
	s.phase = phase
	s.step = step
	s.fault = fault
	s.mu.Unlock()
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
		s.log.Warnf("marshal state: %v", err)
		return
	}
	if err := s.pub.Publish(s.self+"/state", true, b); err != nil {
		s.log.Warnf("publish %s/state: %v", s.self, err)
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
	for _, st := range append(append([]resolvedStep{}, s.startup...), s.shutdown...) {
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
					"options": []string{PhaseIdle, PhaseStarting, PhaseRunning, PhaseStopping}},
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
