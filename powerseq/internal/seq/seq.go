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
// `start` runs the wake list → phase=running. With an idle shutdown
// configured, a station that stays in standby until ShutdownTimeout has passed
// since the last activity runs the idle_shutdown list (full power-down) →
// phase=idle with stop_reason=idle_shutdown; only a `start` revives it.
//
// The antenna-mode slot (hf/ant-mode) can hold the idle logic through its
// /state.power_hold: while it borrows the antenna power to park the HF beam
// with the station in standby, for the whole maintenance mode (borrowed or
// not), and in storm until the park is parked or failed. While the hold is
// true, in any phase, the idle logic stands still: a watch change only
// re-baselines (borrowed power or work at the mast moves the rotator
// readbacks) and no standby, wake or idle shutdown is started; operator
// start/stop still work. When the hold ends a fresh settle window starts, so
// the readback step from switching the power back off is not activity either.
//
// The optional auto_start chain (Config.AutoStart) brings the station from any
// power state to a running 75auto session on bwpc: the startup list (from
// idle) or the wake list (from standby) first, then the chain list. Its
// progress is a chain sub-state on /state (state running|ready|failed, step,
// fault, requester, ts), separate from the station phase: a chain step that
// fails leaves the phase as it was. The chain runs under its own cancellable
// context, so a station stop ends it at once.
//
// A live 75auto session holds the standby and the idle shutdown: while the
// hf/auto-op /state reports session.source live, session.state connecting or
// running, and the
// last /beat or live /state is younger than AutoOpBeatAge, also while a chain
// runs and for ChainReadyHold after it is ready. In standby a live session
// wakes the station. When the hold ends, both idle clocks restart. On the
// session start edge powerseq sends the PA to standby once.
//
// The phase survives a process restart (Config.StateFile, see persist.go): a
// restart restores idle/running/standby with their idle clocks, and turns a
// sequence that was in flight into idle + fault without re-running it.
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

	"codeberg.org/kgbvax/stationa/shared/antmode"
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

// Chain states (the /state.chain.state of an auto_start).
const (
	ChainRunning = "running"
	ChainReady   = "ready"
	ChainFailed  = "failed"
)

// Defaults of the session hold.
const (
	DefaultAutoOpBeatAge  = 30 * time.Second
	DefaultChainReadyHold = 30 * time.Minute
)

// chainStepStation is the chain step name while the startup or wake list runs first.
const chainStepStation = "station"

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
	// Fresh: the step passes (or fails on FailValue) only after a /state
	// received after the step started showed the field at a non-terminal
	// value (neither Value nor FailValue, e.g. "starting"). A cmd step to the
	// same slot right before it counts as the step start.
	Fresh bool
	// FailValue: the field taking this value fails the step at once ("" = off).
	FailValue string

	// waits (wait_status + wait_state)
	HoldMs   *int // debounce: condition must hold this long; nil → DefaultHold, explicit 0 = edge-triggered
	TimeoutS *int // per-step override; nil → StepTimeout

	// delay
	DurationS *int   // literal seconds
	Duration  string // "network" | "stagger" (symbolic ref into [timing])

	// wol
	MAC       string // target NIC MAC
	Broadcast string // host:port; "" → DefaultWOLBroadcast
	Poke      string // host:port; "" → no TCP connect after the packet
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

// PokeSender opens one TCP connection to addr (host:port) and closes it. A PC
// in Modern Standby (S0 low-power idle) ignores a magic packet, but a TCP
// connect to it wakes it ("wake on pattern match"). The result does not
// matter: a refused or timed-out connect has woken the PC all the same.
// Injected so tests do not touch the network.
type PokeSender func(addr string)

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
	// IdleShutdown is the optional full power-down run from standby once
	// ShutdownTimeout has passed since the last activity (requires Standby).
	IdleShutdown []Step
	// AutoStart is the optional chain run by the auto_start command, after
	// the startup list (from idle) or the wake list (from standby).
	AutoStart []Step

	// AutoOpSlot is the 75auto slot (same site/station, e.g. "auto-op") whose
	// /state session.state and /beat hold the idle logic and refuse an
	// auto_start while a session is active. "" = not read.
	AutoOpSlot string
	// PASlot is the PA slot (same site/station, e.g. "pa") that gets one
	// set_mode standby on the session start edge. "" = none. Needs AutoOpSlot.
	PASlot string
	// AutoOpBeatAge is how old the last hf/auto-op /beat or live /state may
	// be for an active session to count (0 → DefaultAutoOpBeatAge).
	AutoOpBeatAge time.Duration
	// ChainReadyHold is how long after a chain is ready the idle logic still
	// holds (0 → DefaultChainReadyHold).
	ChainReadyHold time.Duration

	// Idle standby triggers (used only when Standby/Wake are configured).
	ActivitySlot string        // site-relative; /state.activity + last_activity
	IdleTimeout  time.Duration // watch quiet window + minimum running time
	Watches      []Watch
	IdleCheck    time.Duration // idle evaluation cadence (0 → 1 s)
	// ShutdownTimeout is the time since the last activity after which a
	// station in standby runs IdleShutdown (must exceed IdleTimeout).
	ShutdownTimeout time.Duration
	// WatchSettle is how long after a phase change a watch change is not
	// activity (the change re-baselines the watch instead). The sequencer's
	// own steps switch power: a rotator readback that steps when the antenna
	// supply goes off (device link still up) is the sequencer's doing, not an
	// operator's. 0 = no settle window.
	WatchSettle time.Duration
	// AntModeSlot is the antenna-mode logic slot (same site/station, e.g.
	// "ant-mode") whose /state power_hold suspends the idle logic (during a
	// borrow, a maintenance, a storm park). "" = not read. Used only with
	// idle standby.
	AntModeSlot string
	// AntModeHold is how long after a broker (re)connect the idle logic waits
	// for the ant-mode /state to be re-delivered (0 → 3 s). No state within
	// the window = no hold (no retained state means normal).
	AntModeHold time.Duration

	// StateFile is where the phase is persisted across restarts ("" = not
	// persisted: every start is idle). See persist.go.
	StateFile string
	// PersistEvery throttles state-file writes for clock-only changes
	// (0 → DefaultPersistEvery). Phase changes are written at once.
	PersistEvery time.Duration

	// WOL sends wol steps; nil → SendMagicPacket.
	WOL WOLSender

	// Poke sends the poke of a wol step; nil → SendPoke.
	Poke PokeSender

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
	Fresh    bool
	FailVal  string
	Hold     time.Duration
	Timeout  time.Duration
	Duration time.Duration // delay
	MAC      net.HardwareAddr
	Bcast    string
	Poke     string
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
	idleShut []resolvedStep // optional; run from standby after ShutdownTimeout
	autoStrt []resolvedStep // optional; the auto_start chain

	activitySlot string // absolute; "" = idle standby disabled
	antModeSlot  string // absolute; "" = power_hold not read
	autoOpSlot   string // absolute; "" = 75auto session not read
	paSlot       string // absolute; "" = no PA standby on the session start edge
	watches      []*resolvedWatch
	wol          WOLSender
	poke         PokeSender

	mu     sync.Mutex
	status map[string]string // absolute slot → "online"|"offline" (absent = unseen)
	state  map[string]any    // absolute slot → parsed /state JSON (map[string]any)
	// stateSeq counts the /state messages per slot (a fresh wait_state needs
	// one received after the step started).
	stateSeq map[string]uint64
	// stateHist keeps the last /state docs of every fresh wait_state slot, so
	// a fresh wait sees a non-terminal value that a newer /state replaced
	// between two polls.
	stateHist map[string][]stateRec
	// lastCmd marks the cmd step that ran right before the current step
	// (runner goroutine only): a fresh wait_state on its slot starts there.
	lastCmd cmdMark
	online  bool // broker connection state (set by the mqtt layer)

	phase      string
	step       string
	fault      string
	stopReason string // why the station last went to idle: "operator" | "idle_shutdown" ("" after a start)

	runningSince time.Time // last entry into phase=running
	standbySince time.Time // last entry into phase=standby
	lastWatch    time.Time // last counted watch change
	lastWatchWhy string    // which watch moved last ("slot.field a→b"), for the wake log
	settleUntil  time.Time // watch changes before this re-baseline (see WatchSettle)
	powerHold    bool      // ant-mode power_hold (borrow, maintenance, storm park): idle logic suspended
	holdEndedAt  time.Time // last end of a power hold: restarts the idle-shutdown clock
	// After a broker (re)connect the idle logic waits until the ant-mode
	// /state is re-delivered (antModeSeen) or antModeWaitUntil passes: a
	// borrow that started while powerseq was disconnected must not be cut by
	// a decision made on the pre-disconnect hold value.
	antModeSeen      bool
	antModeWaitUntil time.Time
	antMode          antmode.Mode // last parsed ant-mode (an auto_start is refused in storm/maintenance)

	// auto_start chain. chain is the /state.chain record ("" state = none).
	chain          chainState
	chainQueued    bool               // an auto_start is in cmdCh
	chainRequester string             // requester of the queued auto_start
	chainCancel    context.CancelFunc // non-nil while a chain runs
	chainStopped   bool               // a station stop ended (or pre-empted) the chain

	// 75auto session (hf/auto-op).
	autoOpSession      string    // session.state of the last /state
	autoOpLive         bool      // session.source of the last /state was "live"
	autoOpActive       bool      // a live session was connecting|running (edge baseline)
	autoOpBeat         time.Time // last /beat or live /state
	paStandbyPending   bool      // a PA set_mode standby waits for the runner
	sessionHeld        bool      // the session hold was active at the last idle check
	sessionHoldEndedAt time.Time // last end of the session hold: restarts both idle clocks

	// State file bookkeeping (runner goroutine only, after New).
	saved        *savedState // last snapshot written (or loaded); nil = none
	savedAt      time.Time   // when saved was written (clock-only throttle)
	persistEvery time.Duration

	cmdCh       chan string   // "start" | "stop" | "auto_start" → runner
	republishCh chan struct{} // → runner: re-publish /state, send a pending PA standby
}

// stateRec is one /state of a fresh wait_state slot.
type stateRec struct {
	seq uint64
	doc map[string]any
}

// stateHistMax bounds stateHist per slot.
const stateHistMax = 64

// cmdMark is a cmd step and the slot's /state count before it was sent.
type cmdMark struct {
	slot string
	seq  uint64
}

// chainState is the progress record of an auto_start (persisted and
// published as /state.chain).
type chainState struct {
	State     string    `json:"state"`
	Step      string    `json:"step,omitempty"`
	Fault     string    `json:"fault,omitempty"`
	Requester string    `json:"requester,omitempty"`
	TS        time.Time `json:"ts"`
}

func (a chainState) equal(b chainState) bool {
	return a.State == b.State && a.Step == b.Step && a.Fault == b.Fault && a.Requester == b.Requester && a.TS.Equal(b.TS)
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
	idleShut, err := resolve(cfg.IdleShutdown, cfg, self, "idle_shutdown")
	if err != nil {
		return nil, err
	}
	autoStrt, err := resolve(cfg.AutoStart, cfg, self, "auto_start")
	if err != nil {
		return nil, err
	}
	if len(idleShut) > 0 {
		if len(standby) == 0 {
			return nil, fmt.Errorf("idle_shutdown needs standby/wake (it runs from standby)")
		}
		if cfg.ShutdownTimeout <= cfg.IdleTimeout {
			return nil, fmt.Errorf("idle_shutdown needs a shutdown timeout > the idle timeout")
		}
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
		idleShut:    idleShut,
		autoStrt:    autoStrt,
		wol:         cfg.WOL,
		poke:        cfg.Poke,
		status:      map[string]string{},
		state:       map[string]any{},
		stateSeq:    map[string]uint64{},
		stateHist:   map[string][]stateRec{},
		phase:       PhaseIdle,
		cmdCh:       make(chan string, 4),
		republishCh: make(chan struct{}, 1),
	}
	for _, st := range s.allSteps() {
		if st.Kind == KindWaitState && st.Fresh {
			s.stateHist[st.Slot] = nil
		}
	}
	if s.wol == nil {
		s.wol = SendMagicPacket
	}
	if s.poke == nil {
		s.poke = SendPoke
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
	// The ant-mode slot holds the idle logic, and refuses an auto_start in
	// storm or maintenance.
	if cfg.AntModeSlot != "" && (len(standby) > 0 || len(autoStrt) > 0) {
		s.antModeSlot = schema.SlotBase(cfg.Site, cfg.Station, cfg.AntModeSlot)
	}
	if s.cfg.AntModeHold <= 0 {
		s.cfg.AntModeHold = 3 * time.Second
	}
	if cfg.AutoOpSlot != "" {
		s.autoOpSlot = schema.SlotBase(cfg.Site, cfg.Station, cfg.AutoOpSlot)
		if cfg.PASlot != "" {
			s.paSlot = schema.SlotBase(cfg.Site, cfg.Station, cfg.PASlot)
		}
	}
	if s.cfg.AutoOpBeatAge <= 0 {
		s.cfg.AutoOpBeatAge = DefaultAutoOpBeatAge
	}
	if s.cfg.ChainReadyHold <= 0 {
		s.cfg.ChainReadyHold = DefaultChainReadyHold
	}
	s.persistEvery = cfg.PersistEvery
	if s.persistEvery <= 0 {
		s.persistEvery = DefaultPersistEvery
	}
	if cfg.StateFile != "" {
		s.restore(time.Now())
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
			Fresh:   st.Fresh,
			FailVal: st.FailValue,
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
			r.Poke = st.Poke
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
	out = append(out, s.idleShut...)
	out = append(out, s.autoStrt...)
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

// AntModeStateTopic returns the antenna-mode slot's /state topic, or "" when
// it is not read (no ant-mode slot configured, or neither idle standby nor an
// auto_start chain configured). The mqtt layer subscribes it with
// SetAntMode as the handler.
func (s *Sequencer) AntModeStateTopic() string {
	if s.antModeSlot == "" {
		return ""
	}
	return s.antModeSlot + "/state"
}

// SetAntMode records a hf/ant-mode/state snapshot (called from the mqtt layer;
// parse + a mutex update only, never blocks). Reading rules of the slot
// contract: an empty payload (no retained state) is normal = no hold; a
// malformed payload keeps the last known hold (logged as Error). power_hold is
// not only a borrow flag: antmode also holds for a whole maintenance and in
// storm until the park is parked or failed. When the hold ends a fresh
// WatchSettle window starts: antmode may switch borrowed power back off, and
// the readback step that follows is the borrow's doing, not an operator's.
// The end of a hold also restarts the idle-shutdown clock: watch changes
// during the hold are not counted, and a maintenance hold can last hours with
// the operator on site.
func (s *Sequencer) SetAntMode(payload []byte) {
	st, err := antmode.Parse(payload)
	if err != nil {
		s.log.Errorf("ant-mode state rejected, keeping last known power_hold: %v", err)
		return
	}
	s.mu.Lock()
	s.antModeSeen = true
	s.antMode = st.Mode
	prev := s.powerHold
	s.powerHold = st.PowerHold
	if prev && !st.PowerHold {
		now := time.Now()
		if until := now.Add(s.cfg.WatchSettle); until.After(s.settleUntil) {
			s.settleUntil = until
		}
		s.holdEndedAt = now
	}
	phase := s.phase
	s.mu.Unlock()
	switch {
	case !prev && st.PowerHold:
		s.log.Infof("ant-mode power hold: idle logic suspended (phase=%s, ant_mode=%s)", phase, st.Mode)
	case prev && !st.PowerHold:
		s.log.Infof("ant-mode power hold ended: watches settle for %s, idle-shutdown clock restarted (phase=%s)", s.cfg.WatchSettle, phase)
	}
}

// AutoOpStateTopic and AutoOpBeatTopic return the 75auto slot's /state and
// /beat topics, or "" when the slot is not read. The mqtt layer subscribes
// them with SetAutoOpState and AutoOpBeat as the handlers.
func (s *Sequencer) AutoOpStateTopic() string {
	if s.autoOpSlot == "" {
		return ""
	}
	return s.autoOpSlot + "/state"
}

func (s *Sequencer) AutoOpBeatTopic() string {
	if s.autoOpSlot == "" {
		return ""
	}
	return s.autoOpSlot + "/beat"
}

// AutoOpSlot returns the absolute 75auto slot ("" = not read).
func (s *Sequencer) AutoOpSlot() string { return s.autoOpSlot }

// SetAutoOpState records the session.state of a hf/auto-op /state (called
// from the mqtt layer; parse + a mutex update only, never publishes). A live
// /state counts as a beat; a retained one can be old and does not. The PA
// standby is edge-triggered: a live /state that turns the session from not
// active to connecting or running asks the runner to send it once. A retained
// /state only re-baselines the edge, so a powerseq restart or a reconnect
// during a session sends nothing. Only a session with session.source "live"
// counts (a replay does not transmit; 75auto holds the station by the same
// rule). Empty payload = no session; malformed = keep the last known session
// state (Error).
func (s *Sequencer) SetAutoOpState(payload []byte, retained bool) {
	var doc struct {
		Session *struct {
			State  string `json:"state"`
			Source string `json:"source"`
		} `json:"session"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &doc); err != nil {
			s.log.Errorf("auto-op state rejected, keeping last known session state: %v", err)
			return
		}
	}
	state, live := "", false
	if doc.Session != nil {
		state = doc.Session.State
		live = doc.Session.Source == "live"
	}
	active := live && sessionStateActive(state)
	s.mu.Lock()
	s.autoOpSession = state
	s.autoOpLive = live
	if !retained {
		s.autoOpBeat = time.Now()
	}
	edge := !retained && active && !s.autoOpActive && s.paSlot != ""
	if edge {
		s.paStandbyPending = true
	}
	s.autoOpActive = active
	s.mu.Unlock()
	if edge {
		s.log.Infof("75auto session %s: PA to standby", state)
		s.RequestRepublish()
	}
}

// AutoOpBeat records a hf/auto-op /beat (called from the mqtt layer).
func (s *Sequencer) AutoOpBeat() {
	s.mu.Lock()
	s.autoOpBeat = time.Now()
	s.mu.Unlock()
}

func sessionStateActive(state string) bool { return state == "connecting" || state == "running" }

// sessionActiveLocked reports a live 75auto session that is connecting or
// running and still beats.
func (s *Sequencer) sessionActiveLocked(now time.Time) bool {
	return s.autoOpSlot != "" && s.autoOpLive && sessionStateActive(s.autoOpSession) &&
		!s.autoOpBeat.IsZero() && now.Sub(s.autoOpBeat) < s.cfg.AutoOpBeatAge
}

// sessionHoldLocked reports the session hold: an active session, a running
// chain, or a ready chain within ChainReadyHold (the operator just asked for a
// session; standby must not cut it before it starts).
func (s *Sequencer) sessionHoldLocked(now time.Time) bool {
	switch {
	case s.sessionActiveLocked(now), s.chain.State == ChainRunning:
		return true
	case s.chain.State == ChainReady:
		return now.Sub(s.chain.TS) < s.cfg.ChainReadyHold
	}
	return false
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
	s.stateSeq[slot]++
	if h, ok := s.stateHist[slot]; ok {
		h = append(h, stateRec{seq: s.stateSeq[slot], doc: m})
		if len(h) > stateHistMax {
			h = h[len(h)-stateHistMax:]
		}
		s.stateHist[slot] = h
	}
	s.noteWatchesLocked(slot, m)
	s.mu.Unlock()
}

// noteWatchesLocked records a counted change for every watch on slot whose
// field moved by at least its deadband since the last counted value. A change
// counts only while the slot is fit (bridge online, device_online not false)
// and only between two real values: a field appearing or vanishing (device
// powered down, invalid readback) is not activity — otherwise switching the
// antenna power off in standby would read as activity and wake the station.
//
// Nor does a change while a sequence runs or within WatchSettle of a phase
// change: the sequencer's own power steps move readbacks of devices whose link
// stays up (the UHF rotator controllers report a stepped position with
// device_online=true when the antenna supply goes off — 2026-10-19, every
// standby woke ~2 s later). Such a change re-baselines the watch instead.
// The same holds while the antenna-mode slot holds the idle logic
// (power_hold: a borrow, a maintenance, a storm park) and within WatchSettle
// after the hold ends.
func (s *Sequencer) noteWatchesLocked(slot string, m map[string]any) {
	fit := s.status[slot] == "online" && coerceToString(m["device_online"]) != "false"
	now := time.Now()
	settled := (s.phase == PhaseRunning || s.phase == PhaseStandby) && !now.Before(s.settleUntil) && !s.powerHold
	for _, w := range s.watches {
		if w.Slot != slot {
			continue
		}
		v := coerceToString(m[w.Field])
		if v == "" || !fit {
			continue
		}
		if w.anchor == "" || !settled {
			w.anchor = v // baseline: first real value, or a self-inflicted change
			continue
		}
		if watchMoved(w.anchor, v, w.MinDelta) {
			s.lastWatchWhy = fmt.Sprintf("%s.%s %s→%s", w.Slot, w.Field, w.anchor, v)
			w.anchor = v
			s.lastWatch = now
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
// On a (re)connect the ant-mode hold is unknown until its retained /state is
// re-delivered (or AntModeHold passes): the mqtt layer sets the broker online
// before it subscribes.
func (s *Sequencer) SetBrokerOnline(online bool) {
	s.mu.Lock()
	if online && !s.online && s.antModeSlot != "" {
		s.antModeSeen = false
		s.antModeWaitUntil = time.Now().Add(s.cfg.AntModeHold)
	}
	s.online = online
	s.mu.Unlock()
}

// Start requests a startup sequence. Non-blocking; honored when phase=idle
// (retries/clears a prior fault) or phase=standby (= wake); dropped (logged)
// when busy.
func (s *Sequencer) Start() { s.request("start") }

// Stop requests a shutdown sequence. Non-blocking; honored when phase=running,
// phase=standby or phase=idle — the teardown is idempotent and can only de-energize, and the
// fault latch is lost with a missing or corrupt state file (or without one),
// so stop-from-idle must not depend on remembering why it is idle. Dropped
// (logged) mid-sequence.
func (s *Sequencer) Stop() { s.request("stop") }

// AutoStart requests the auto_start chain for requester (the device id the
// page echoes from /state.chain). Non-blocking. While a chain runs (or one is
// queued) it is dropped and /state.chain stays as it is. Every other refusal
// writes chain.state=failed with the reason: no [[auto_start]], busy: <phase>
// (a sequence runs), antenna mode storm|maintenance, busy: session active.
func (s *Sequencer) AutoStart(requester string) {
	now := time.Now()
	s.mu.Lock()
	if s.chainQueued || s.chain.State == ChainRunning {
		s.mu.Unlock()
		s.log.Warnf("auto_start from %q refused: busy: chain running", requester)
		return
	}
	if reason := s.autoStartRefusalLocked(now); reason != "" {
		s.chain = chainState{State: ChainFailed, Fault: reason, Requester: requester, TS: now}
		s.mu.Unlock()
		s.log.Warnf("auto_start from %q refused: %s", requester, reason)
		s.RequestRepublish() // the runner publishes and persists it
		return
	}
	s.chainQueued = true
	s.chainRequester = requester
	s.mu.Unlock()
	select {
	case s.cmdCh <- "auto_start":
	default:
		s.mu.Lock()
		s.chainQueued = false
		s.mu.Unlock()
		s.log.Warnf("auto_start dropped: runner queue full")
	}
}

// autoStartRefusalLocked returns why an auto_start cannot run now ("" = it can).
func (s *Sequencer) autoStartRefusalLocked(now time.Time) string {
	switch {
	case len(s.autoStrt) == 0:
		return "auto_start not configured"
	case transitional(s.phase):
		return "busy: " + s.phase
	case s.antMode == antmode.Storm || s.antMode == antmode.Maintenance:
		return "antenna mode " + string(s.antMode)
	case s.sessionActiveLocked(now):
		return "busy: session active"
	}
	return ""
}

func (s *Sequencer) request(cmd string) {
	s.mu.Lock()
	phase, fault := s.phase, s.fault
	allow := false
	switch cmd {
	case "start":
		allow = phase == PhaseIdle || phase == PhaseStandby
	case "stop":
		// Idle is honored regardless of fault: the fault latch dies with a
		// missing or corrupt state file, so after such a restart a stop is the
		// ONLY teardown path for the slots a faulted sequence left energized —
		// and a shutdown re-states "off", it never energizes.
		allow = phase == PhaseRunning || phase == PhaseIdle || phase == PhaseStandby
		// A stop ends a running chain directly (not through the queue, the
		// runner is busy in it): the chain fails "stopped by the operator"
		// before the shutdown runs, also while its startup or wake runs. A
		// queued auto_start is refused the same way.
		if s.chainCancel != nil {
			s.chainStopped = true
			s.chainCancel()
			allow = true
		}
		if s.chainQueued {
			s.chainStopped = true
			s.chainQueued = false
		}
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
// ctx cancel. Publishes the initial state on entry — the phase restored from
// the state file (idle without one) — overwriting any stale retained /state
// from before the restart (own /state is an output; the sequencer never reads
// it back from the bus). On exit it flushes pending clock-only changes to the
// state file.
func (s *Sequencer) Run(ctx context.Context) {
	defer func() { s.persist(time.Now(), false) }()
	s.persist(time.Now(), false) // a resolved restore (or a first start) is written now
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
			// Clock-only changes (a watch moved, a power hold ended) arrive on
			// the paho goroutine; the runner writes them (throttled).
			s.persist(time.Now(), true)
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
			// phase), closing the TOCTOU window. auto_start has its own
			// re-check (beginAutoStart).
			if cmd == "auto_start" {
				s.runAutoStart(ctx)
				continue
			}
			steps, endPhase, ok := s.begin(cmd)
			if !ok {
				s.log.Warnf("cmd %s dropped: busy guard re-check failed (already in progress)", cmd)
				continue
			}
			s.runSequence(ctx, steps, endPhase)
		case <-s.republishCh:
			s.serviceRequests()
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

// serviceRequests is the runner's answer to RequestRepublish: send a pending
// PA standby, write and publish /state (a refused auto_start set the chain on
// the paho goroutine). It also runs while a sequence sleeps or waits
// (sleepCtx), so neither waits for the sequence to end.
func (s *Sequencer) serviceRequests() {
	s.flushPAStandby()
	s.persist(time.Now(), false)
	s.publishState()
}

// flushPAStandby sends the pending PA set_mode standby (not retained: a
// one-shot, it must not replay on a PA bridge restart and undo a manual
// operate later in the session). Runner goroutine only.
func (s *Sequencer) flushPAStandby() {
	s.mu.Lock()
	pending := s.paStandbyPending
	s.paStandbyPending = false
	online := s.online
	s.mu.Unlock()
	if !pending {
		return
	}
	if !online {
		s.log.Errorf("PA standby for the 75auto session not sent: broker disconnected")
		return
	}
	b, _ := json.Marshal(cmdPayload{Action: "set_mode", Value: "standby"})
	if err := s.pub.Publish(s.paSlot+"/cmd", false, b); err != nil {
		s.log.Errorf("PA standby for the 75auto session not sent: %v", err)
	}
}

// ---------------------------------------------------------------------------
// sequences
// ---------------------------------------------------------------------------

// idleDecision evaluates the idle standby triggers at now and returns
// "standby", "wake", "idle_shutdown" or "" (nothing to do). Evaluated only with a live broker
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
//   - idle_shutdown (from standby, when configured and no wake is due): the
//     last activity (see lastActivityLocked) is ShutdownTimeout ago. Not
//     before the activity slot's /state has been seen: its last_activity is
//     part of that clock, and after a restart into standby the retained
//     /state can arrive after the first idle check.
//
// The session hold (a live 75auto session or an auto_start chain,
// sessionHoldLocked) blocks the standby and the idle shutdown, not the wake:
// in standby a live session is activity and wakes the station (75auto may be
// started on bwpc without the chain, and it must not transmit with the
// antenna gear off). Nothing is decided while the antenna-mode slot holds the idle logic
// (power_hold): a borrow, a maintenance or a storm park must not be cut by a
// standby or idle shutdown, and is not a reason to wake. Nor right after a broker (re)connect until the
// ant-mode /state is re-delivered (or AntModeHold passes): a borrow may have
// started while powerseq was disconnected.
func (s *Sequencer) idleDecision(now time.Time) string {
	if !s.standbyEnabled() {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	held := s.sessionHoldLocked(now)
	if s.sessionHeld && !held {
		// Both idle clocks restart (sessionHoldEndedAt is in the standby
		// check and in lastActivityLocked): a session that ran for hours left
		// no counted activity behind.
		s.sessionHoldEndedAt = now
		s.log.Infof("session hold ended: idle clocks restarted (phase=%s)", s.phase)
	}
	s.sessionHeld = held
	if !s.online || s.powerHold {
		return ""
	}
	if s.antModeSlot != "" && !s.antModeSeen && now.Before(s.antModeWaitUntil) {
		return ""
	}
	m, _ := s.state[s.activitySlot].(map[string]any)
	switch s.phase {
	case PhaseRunning:
		if held {
			return ""
		}
		fit := s.status[s.activitySlot] == "online" && m != nil && coerceToString(m["device_online"]) != "false"
		if !fit || coerceToString(m["activity"]) != "inactive" {
			return ""
		}
		if now.Sub(s.runningSince) < s.cfg.IdleTimeout || now.Sub(s.lastWatch) < s.cfg.IdleTimeout ||
			now.Sub(s.sessionHoldEndedAt) < s.cfg.IdleTimeout {
			return ""
		}
		return "standby"
	case PhaseStandby:
		if s.lastWatch.After(s.standbySince) {
			s.log.Infof("wake: watch moved (%s)", s.lastWatchWhy)
			return "wake"
		}
		if m != nil {
			if ts, err := time.Parse(time.RFC3339Nano, coerceToString(m["last_activity"])); err == nil && ts.After(s.standbySince) {
				s.log.Infof("wake: %s last_activity %s", s.activitySlot, ts.Format(time.RFC3339))
				return "wake"
			}
		}
		if s.sessionActiveLocked(now) {
			s.log.Infof("wake: 75auto session %s", s.autoOpSession)
			return "wake"
		}
		if held {
			return ""
		}
		if len(s.idleShut) > 0 && m != nil && now.Sub(s.lastActivityLocked(m)) >= s.cfg.ShutdownTimeout {
			return "idle_shutdown"
		}
	}
	return ""
}

// lastActivityLocked is the idle-shutdown clock: the latest of the activity
// slot's last_activity, the last counted watch change, the last entry into
// phase=running (a start or a wake is activity too) and the last end of an
// ant-mode power hold or of the session hold (watch changes during a hold are
// not counted, and a session counts as activity). An
// antennaselect restart publishes no last_activity, so it never resets the
// clock.
func (s *Sequencer) lastActivityLocked(m map[string]any) time.Time {
	last := s.runningSince
	if s.lastWatch.After(last) {
		last = s.lastWatch
	}
	if s.holdEndedAt.After(last) {
		last = s.holdEndedAt
	}
	if s.sessionHoldEndedAt.After(last) {
		last = s.sessionHoldEndedAt
	}
	if m != nil {
		if ts, err := time.Parse(time.RFC3339Nano, coerceToString(m["last_activity"])); err == nil && ts.After(last) {
			last = ts
		}
	}
	return last
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
			s.stopReason = ""
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
			s.stopReason = "operator"
			steps, endPhase, ok = s.shutdown, PhaseIdle, true
			// A ready chain is over with the stop: a console that waits to
			// start the session must not start it on a stopped station.
			if s.chain.State == ChainReady {
				s.chain.State = ChainFailed
				s.chain.Fault = "stopped by the operator"
				s.chain.TS = time.Now()
				s.log.Infof("auto_start chain: ready ended by the stop")
			}
		}
	case "idle_shutdown":
		if s.phase == PhaseStandby && len(s.idleShut) > 0 {
			s.log.Infof("idle shutdown: no activity for %s", s.cfg.ShutdownTimeout)
			s.phase = PhaseStopping
			s.step = ""
			s.fault = ""
			s.stopReason = "idle_shutdown"
			steps, endPhase, ok = s.idleShut, PhaseIdle, true
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
		// On disk before the first step runs: a crash mid-sequence must
		// restore as an interrupted sequence, not as the phase before it.
		s.persist(time.Now(), false)
		s.publishState()
	}
	return steps, endPhase, ok
}

// runSequence runs steps and reports whether all passed (endPhase reached).
func (s *Sequencer) runSequence(ctx context.Context, steps []resolvedStep, endPhase string) bool {
	for _, st := range steps {
		if ctx.Err() != nil {
			s.abort(st.Name + ": interrupted")
			return false
		}
		s.setStep(st.Name)
		reason, ok := s.execStep(ctx, &st)
		if !ok {
			s.abort(st.Name + ": " + reason)
			return false
		}
	}
	s.setPhase(endPhase, "", "")
	return true
}

// beginAutoStart re-checks an auto_start on the runner (the authoritative
// guard, like begin) and returns the chain context (cancelled by a station
// stop) and the list to run before the chain: the startup list from idle, the
// wake list from standby, none from running. A refusal writes
// chain.state=failed with the reason.
func (s *Sequencer) beginAutoStart(ctx context.Context, now time.Time) (chainCtx context.Context, prefix []resolvedStep, prefixEnd string, ok bool) {
	s.mu.Lock()
	s.chainQueued = false
	requester := s.chainRequester
	reason := s.autoStartRefusalLocked(now)
	if s.chainStopped {
		reason = "stopped by the operator"
		s.chainStopped = false
	}
	if reason != "" {
		s.chain = chainState{State: ChainFailed, Fault: reason, Requester: requester, TS: now}
		s.mu.Unlock()
		s.log.Warnf("auto_start from %q refused: %s", requester, reason)
		s.persist(now, false)
		s.publishState()
		return nil, nil, "", false
	}
	s.chain = chainState{State: ChainRunning, Requester: requester, TS: now}
	// The cancel is in place before mu is released: a stop that sees the
	// chain running also finds the chain to cancel.
	chainCtx, s.chainCancel = context.WithCancel(ctx)
	switch s.phase {
	case PhaseIdle:
		s.phase = PhaseStarting
		s.step = ""
		s.fault = ""
		s.stopReason = ""
		s.chain.Step = chainStepStation
		prefix, prefixEnd = s.startup, PhaseRunning
	case PhaseStandby:
		s.phase = PhaseWaking
		s.step = ""
		s.fault = ""
		s.chain.Step = chainStepStation
		prefix, prefixEnd = s.wake, PhaseRunning
	}
	// An accepted auto_start is activity: the hold starts now, and its end
	// (ready window over, or a failed chain) restarts the idle clocks.
	s.sessionHeld = true
	s.sessionHoldEndedAt = now
	phase := s.phase
	s.mu.Unlock()
	s.log.Infof("auto_start from %q: chain running (phase=%s)", requester, phase)
	s.persist(now, false)
	s.publishState()
	return chainCtx, prefix, prefixEnd, true
}

// runAutoStart runs an auto_start: the startup or wake list when needed, then
// the chain list, under a context of its own that a station stop cancels
// (request). A chain step failure leaves the phase as it is; a startup or
// wake failure is a station fault as with start (phase idle).
func (s *Sequencer) runAutoStart(ctx context.Context) {
	chainCtx, prefix, prefixEnd, ok := s.beginAutoStart(ctx, time.Now())
	if !ok {
		return
	}
	if len(prefix) > 0 && !s.runSequence(chainCtx, prefix, prefixEnd) {
		_, _, fault := s.Phase()
		s.endChain(ChainFailed, fault)
		return
	}
	for _, st := range s.autoStrt {
		if chainCtx.Err() != nil {
			s.endChain(ChainFailed, "interrupted")
			return
		}
		s.setChainStep(st.Name)
		reason, ok := s.execStep(chainCtx, &st)
		if !ok {
			s.endChain(ChainFailed, reason)
			return
		}
	}
	s.endChain(ChainReady, "")
}

// setChainStep records the chain step (the station step and phase stay).
func (s *Sequencer) setChainStep(step string) {
	s.mu.Lock()
	s.chain.Step = step
	s.chain.Fault = ""
	s.mu.Unlock()
	s.persist(time.Now(), false)
	s.publishState()
}

// endChain records the end of a chain. After a station stop it is failed
// "stopped by the operator", also when the last step had already passed: no
// ready is published after a stop. The chain cancel and the stop latch are
// cleared under mu before the publish, so a stop from then on sees no chain
// to cancel (a stop on a ready chain fails it in begin) and leaves no latch
// for the next auto_start.
func (s *Sequencer) endChain(state, fault string) {
	now := time.Now()
	s.mu.Lock()
	if s.chainStopped {
		state, fault = ChainFailed, "stopped by the operator"
		s.chainStopped = false
	}
	if s.chainCancel != nil {
		s.chainCancel()
		s.chainCancel = nil
	}
	s.chain.State = state
	s.chain.Fault = fault
	s.chain.TS = now
	step := s.chain.Step
	s.mu.Unlock()
	if state == ChainFailed {
		s.log.Warnf("auto_start chain failed at %s: %s", step, fault)
	} else {
		s.log.Infof("auto_start chain ready")
	}
	s.persist(now, false)
	s.publishState()
}

// execStep executes one resolved step, returning (reason, ok). ok=false aborts
// the sequence; reason is appended to the step name in the fault string.
func (s *Sequencer) execStep(ctx context.Context, st *resolvedStep) (string, bool) {
	prevCmd := s.lastCmd
	s.lastCmd = cmdMark{}
	switch st.Kind {
	case KindCmd:
		s.lastCmd = cmdMark{slot: st.Slot, seq: s.stateSeqOf(st.Slot)}
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
		if st.Poke != "" {
			s.poke(st.Poke)
		}
		return "", true
	case KindWaitStatus:
		reason, ok := s.wait(ctx, st, func() (bool, string) {
			for _, slot := range st.Slots {
				if st.State == "online" {
					// Two-layer liveness: bridge LWT AND device_online. The
					// bridge LWT alone is online while the device behind it
					// is still booting (flexbridge on scmino, 2026-10-18).
					if !s.isOnline(slot) {
						return false, ""
					}
				} else if s.statusOf(slot) != st.State {
					return false, ""
				}
			}
			return true, ""
		})
		return s.withObserved(st.Slots, "", reason, ok)
	case KindWaitState:
		// fresh: the target (or fail) value counts only after a /state
		// received after the step started showed a non-terminal value. The
		// actuator publishes "starting" first, so neither a retained value
		// from an earlier run nor a /state tick queued before the cmd passes
		// it. The step starts at the cmd to the same slot right before it,
		// when there is one: the "starting" can arrive before the wait runs.
		var base uint64
		seen := false
		if st.Fresh {
			base = s.stateSeqOf(st.Slot)
			if prevCmd.slot == st.Slot {
				base = prevCmd.seq
			}
		}
		reason, ok := s.wait(ctx, st, func() (bool, string) {
			if !s.isOnline(st.Slot) {
				return false, "" // implicit liveness precondition
			}
			if st.Fresh && !seen {
				seen, base = s.nonTerminalSince(st, base)
				if !seen {
					return false, ""
				}
			}
			v := s.stateField(st.Slot, st.Field)
			if st.FailVal != "" && v == st.FailVal {
				msg := st.Field + "=" + v
				if e := s.stateField(st.Slot, st.Field+"_error"); e != "" {
					msg += " (" + st.Field + "_error=" + e + ")"
				}
				return false, msg
			}
			return v == st.Value, ""
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
		if e := coerceToString(m[field+"_error"]); e != "" {
			out += " " + field + "_error=" + e
		}
	}
	return out
}

// wait polls cond with a deadline + optional hold (debounce). It faults on ctx
// cancel, broker disconnect, timeout, or at once when cond returns a failure
// reason. Returns (reason, ok).
func (s *Sequencer) wait(ctx context.Context, st *resolvedStep, cond func() (bool, string)) (string, bool) {
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
		ok, fail := cond()
		if fail != "" {
			return fail, false
		}
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
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		case <-s.republishCh:
			// A refused auto_start or a PA standby must not wait for the
			// sequence to end.
			s.serviceRequests()
		}
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
func (s *Sequencer) stateSeqOf(slot string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateSeq[slot]
}

// nonTerminalSince reports whether a /state of st.Slot newer than seq showed
// st.Field at a value other than st.Value and st.FailVal, and returns the
// newest seq it looked at (the next scan starts there).
func (s *Sequencer) nonTerminalSince(st *resolvedStep, seq uint64) (bool, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := seq
	for _, r := range s.stateHist[st.Slot] {
		if r.seq <= seq {
			continue
		}
		last = r.seq
		v := coerceToString(r.doc[st.Field])
		if v != st.Value && (st.FailVal == "" || v != st.FailVal) {
			return true, last
		}
	}
	return false, last
}

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
	if prev != phase {
		s.settleUntil = time.Now().Add(s.cfg.WatchSettle)
	}
	s.phase = phase
	s.step = step
	s.fault = fault
	s.mu.Unlock()
	if prev != phase {
		s.log.Infof("phase %s -> %s", prev, phase)
	}
	s.persist(time.Now(), false)
	s.publishState()
}

func (s *Sequencer) setStep(step string) {
	s.mu.Lock()
	s.step = step
	s.fault = ""
	s.mu.Unlock()
	s.persist(time.Now(), false)
	s.publishState()
}

type statePayload struct {
	TS    string `json:"ts"`
	Phase string `json:"phase"`
	Step  string `json:"step"` // always present (model §7.1; only fault is omitempty)
	Fault string `json:"fault,omitempty"`
	// StopReason says why the station is idle: "operator" (a stop) or
	// "idle_shutdown" (no activity for the shutdown timeout). Cleared by start.
	StopReason string `json:"stop_reason,omitempty"`
	// Chain is the progress of the last auto_start (absent before the first).
	Chain *chainPayload `json:"chain,omitempty"`
}

// chainPayload is /state.chain.
type chainPayload struct {
	State     string `json:"state"`
	Step      string `json:"step"`
	Fault     string `json:"fault,omitempty"`
	Requester string `json:"requester"`
	TS        string `json:"ts"`
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

		StopReason: s.stopReason,
	}
	if s.chain.State != "" {
		p.Chain = &chainPayload{
			State:     s.chain.State,
			Step:      s.chain.Step,
			Fault:     s.chain.Fault,
			Requester: s.chain.Requester,
			TS:        s.chain.TS.UTC().Format(time.RFC3339),
		}
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
	if s.antModeSlot != "" {
		watchSet[s.antModeSlot] = struct{}{}
	}
	if s.autoOpSlot != "" {
		watchSet[s.autoOpSlot] = struct{}{}
	}
	if s.paSlot != "" {
		controlSet[s.paSlot] = struct{}{}
		watchSet[s.paSlot] = struct{}{}
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
				{"key": "stop_reason", "name": "Stop reason", "type": "string"},
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

// PokePeriod bounds the connect of SendPoke.
const PokePeriod = 3 * time.Second

// SendPoke opens one TCP connection to addr and closes it. See PokeSender.
func SendPoke(addr string) {
	c, err := net.DialTimeout("tcp", addr, PokePeriod)
	if err == nil {
		_ = c.Close()
	}
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
