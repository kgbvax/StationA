package radio

// The on-demand radio session lifecycle (plan U4, KTD-2): the IC-9700's
// single LAN session stays freely available for manual wfview operating, so
// the bridge connects only on demand and disconnects when idle.
//
// State machine (the plan HTD diagram; /state.session_state drives every
// consumer):
//
//	idle       → connecting : a /cmd arrives, the arm permit is set, or the
//	                          safety PTT-off delivery demands a session
//	connecting → live      : full RS-BA1 handshake (login + token + stream)
//	connecting → error     : MaxAttempts dials failed — the triggering cmd is
//	                          rejected with the observed failure fact
//	error      → idle      : the ErrorDecay timer (safety-class facts keep
//	                          the fact; only session_state decays)
//	error      → connecting: the next cmd/arm demand, ≥AttemptSpacing after
//	                          the last dial
//	live       → idle      : idle timeout with no work and no arm permit —
//	                          keepalives and meter polls are NOT work
//	live       → connecting: involuntary session loss with work pending
//	                          (the outstanding PTT-off)
//	live       → error     : involuntary session loss, no work pending —
//	                          /state republished with radio fields omitted,
//	                          in-flight cmds rejected, disarmed
//
// Concurrency model: one manager goroutine owns all state transitions and
// runs them strictly serially — a connect series executes inline, so queued
// events drain after it and two series can never overlap. Callers post
// closures onto the event queue and wait on their own reply channels; the
// Snapshot/Armed/PTTOffPending accessors read a mutex-guarded mirror and
// stay answerable at all times (the spid cached-state shape).
//
// Everything here is safety-flavored plumbing: RequestPTTOff and the
// OnArmDrop hook are the edges the U6 safety core sits on.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"icom9700-radio-bridge/internal/civ"
)

// SessionState is the /state.session_state vocabulary (plan R5).
type SessionState string

const (
	StateIdle       SessionState = "idle"
	StateConnecting SessionState = "connecting"
	StateLive       SessionState = "live"
	StateError      SessionState = "error"
)

// FactPTTOffUndeliverable is the terminal safety-class fact (plan R2): the
// outstanding PTT-off could not be delivered within the MaxAttempts/
// AttemptSpacing bound of the safety-driven connect. Safety-class facts are
// exempt from clearing by the error-decay timer — session_state decays to
// idle, the fact persists in /state.error until operator ack.
const FactPTTOffUndeliverable = "ptt-off undeliverable: session unavailable"

// The observed-failure-fact vocabulary (plan R2: /state.error carries ONLY
// observed failure facts — no credentials, no protocol detail, nothing
// inferred).
const (
	factLoginRefused = "login refused"
	factConnRefused  = "connection refused"
	factHandshakeTO  = "handshake timeout"
	factCancelled    = "connect cancelled"
	factDialFailed   = "connect failed"
	factSessionLost  = "session lost"
)

// ErrEventQueueFull: the session event queue overflowed (bounded buffer
// rule, R18). Practically unreachable at the configured cap; callers reject
// the triggering cmd instead of blocking.
var ErrEventQueueFull = errors.New("radio: session event queue full")

// Snapshot is the bridge-visible session state: what U5 publishes as
// session_state/error/armed and what U6 gates PTT dispatch on.
type Snapshot struct {
	// State is the session_state value.
	State SessionState
	// Error is the last observed failure fact, "" when none.
	Error string
	// ErrorSafety marks a safety-class fact (watchdog trip, PTT-off
	// undeliverable): it survives the error→idle decay until operator ack,
	// while only SessionState decays (R17 never-dismiss posture).
	ErrorSafety bool
	// Armed mirrors the bridge-held arm permit (R11).
	Armed bool
}

// Config carries what the session manager needs from config.Config plus the
// test seams. The retry/idle/decay figures are read from config at wiring
// time — never hardcoded here (plan: figures finalize at deploy gate 5).
type Config struct {
	// Host is the radio's LAN address.
	Host string
	// ControlPort is the radio's control port; 0 uses the civ default (50001).
	ControlPort int
	// Username and Password are the radio's remote-control login. They are
	// never logged (the civ never-log pin).
	Username string
	Password string

	// IdleTimeout holds a live session with no work this long before a clean
	// disconnect. Keepalives and meter polls are not work (KTD-2).
	IdleTimeout time.Duration
	// MaxAttempts bounds the login attempt series per connect demand
	// (cmd-driven, arm-driven or safety-driven — R2).
	MaxAttempts int
	// AttemptSpacing is the minimum spacing between any two civ.Dial calls
	// (R2: no retry storm into a radio that refuses or holds the session).
	AttemptSpacing time.Duration
	// ErrorDecay holds the error state before it decays to idle (R3).
	ErrorDecay time.Duration

	// Opts mutates the civ transport options per dial — test cadences and
	// extra observation. Production wiring leaves it nil.
	Opts func(*civ.Opts)

	// Log receives lifecycle lines; it should carry the component/slot attrs
	// (logging convention). Nil falls back to the default logger.
	Log *slog.Logger
}

// Hooks are the outbound edges U5 (bus surface) and U6 (safety core)
// consume. Every hook runs on the manager goroutine and must not block —
// hand off to a worker (the stationa paho-handler rule).
type Hooks struct {
	// OnStateChange fires on every published snapshot change, once with the
	// initial idle snapshot right from NewSession. U5 republishes
	// /state.session_state, /state.error and armed from it.
	OnStateChange func(Snapshot)
	// OnSessionDown fires when an ESTABLISHED session ends involuntarily
	// (radio reboot, network drop, wfview eviction), before any reconnect
	// dialing. U5 republishes /state with the radio-measured fields omitted
	// so change-gating cannot freeze stale telemetry (flexbridge Reset
	// lesson, plan R3).
	OnSessionDown func(reason string)
	// OnArmDrop fires when the arm permit drops on its own: an involuntary
	// session loss, or an arm-driven connect series that failed (R2/R3/R11:
	// fail-disarmed, the operator re-arms). This is the U6 arm-permit edge.
	OnArmDrop func(reason string)
	// OnCIVFrame receives raw radio CI-V chunks while a session is live
	// (transceive broadcasts). It runs on the transport read goroutine: it
	// must not block and must not call back into the session.
	OnCIVFrame func(chunk []byte)
}

// demandKind classifies why a connect series runs — it decides the terminal
// error fact and whether a failure drops the arm permit.
type demandKind uint8

const (
	demandCmd    demandKind = iota // a pending /cmd drove the connect (R2)
	demandArm                      // the arm permit drove the connect
	demandSafety                   // the outstanding PTT-off drove the connect
)

// waiter is one queued work item: a cmd body (fn) or the arm demand itself
// (fn nil). reply is buffered so the manager goroutine never blocks on a
// caller that already left.
type waiter struct {
	fn    func(*civ.Transport) error
	reply chan error
	kind  demandKind
}

// eventQueueCap bounds the session event queue (R18: no unbounded growth).
const eventQueueCap = 64

// Session is the on-demand radio session lifecycle above the civ transport.
// Build with NewSession; Close on shutdown.
type Session struct {
	cfg   Config
	hooks Hooks
	log   *slog.Logger
	codec *civ.Codec

	ev     chan func() // event closures; the manager goroutine runs them serially
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	// mu guards the accessor mirror only — the state transitions themselves
	// are serialized by the manager goroutine.
	mu        sync.Mutex
	snap      Snapshot
	pttOffPub bool

	// --- manager-goroutine-owned state ---
	state      SessionState
	fact       string
	factSafety bool
	armed      bool
	pttOff     bool // an outstanding PTT-off awaits delivery
	tr         *civ.Transport
	gen        uint64 // bumped per transport; loss callbacks carry theirs
	lastDial   time.Time
	waiters    []*waiter
	idleTimer  *time.Timer
	decayTimer *time.Timer
}

// NewSession builds the session manager and starts its goroutine. The
// initial idle snapshot fires OnStateChange synchronously before it returns.
func NewSession(cfg Config, hooks Hooks) *Session {
	cfg.fill()
	log := cfg.Log
	if log == nil {
		log = slog.Default().With("component", "icom9700-radio-bridge")
	}
	log = log.With("radio", cfg.Host)

	s := &Session{
		cfg:        cfg,
		hooks:      hooks,
		log:        log,
		codec:      civ.NewCodec(),
		ev:         make(chan func(), eventQueueCap),
		done:       make(chan struct{}),
		state:      StateIdle,
		idleTimer:  newStoppedTimer(),
		decayTimer: newStoppedTimer(),
	}
	s.mu.Lock()
	s.snap = Snapshot{State: StateIdle}
	s.mu.Unlock()
	s.ctx, s.cancel = context.WithCancel(context.Background())
	go s.run()
	s.fireStateChange(Snapshot{State: StateIdle})
	return s
}

// fill zero-guards the policy figures (a hand-built Config — tests, benches —
// stays usable; production wiring passes config.Session's parsed values).
func (c *Config) fill() {
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 120 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.AttemptSpacing <= 0 {
		c.AttemptSpacing = 30 * time.Second
	}
	if c.ErrorDecay <= 0 {
		c.ErrorDecay = 60 * time.Second
	}
}

// --- accessors (safe from any goroutine) --------------------------------

// Snapshot returns the current session snapshot.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

// SessionState returns the current session_state value.
func (s *Session) SessionState() SessionState { return s.Snapshot().State }

// Armed reports the bridge-held arm permit (R11).
func (s *Session) Armed() bool { return s.Snapshot().Armed }

// PTTOffPending reports whether a PTT-off frame is still owed to the radio
// (U6 queries it after RequestPTTOff).
func (s *Session) PTTOffPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pttOffPub
}

// Live reports whether a session is currently established.
func (s *Session) Live() bool { return s.SessionState() == StateLive }

// --- exported mutators ---------------------------------------------------

// SetArmed sets the bridge-held arm permit (R11).
//
// Setting it while no session is held drives a connect demand (R2: armed is
// a connect trigger); the call blocks until the connect series concludes and
// returns its outcome — a failed arm-driven connect is the rejection of the
// arm: the permit drops again, OnArmDrop fires and the observed failure fact
// lands in /state.error (the operator re-arms). While live, arming only sets
// the permit (it blocks idle-disconnect from then on). Disarming while live
// starts the idle timer; disarm never fails. A ctx that dies before the
// series concludes leaves the permit in place — U6's MQTT-loss rule owns
// that case.
func (s *Session) SetArmed(ctx context.Context, on bool) error {
	if !on {
		reply := make(chan error, 1)
		ok := s.post(func() {
			s.armed = false
			if s.state == StateLive {
				s.resetIdleTimer() // disarm with no traffic starts the idle timer
			}
			s.log.Info("arm permit dropped")
			s.publish()
			reply <- nil
		})
		if !ok {
			return ErrEventQueueFull
		}
		select {
		case <-reply:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	w := &waiter{reply: make(chan error, 1), kind: demandArm}
	ok := s.post(func() { s.armLocked(w) })
	if !ok {
		return ErrEventQueueFull
	}
	select {
	case err := <-w.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RequestPTTOff asks for a PTT-off frame to be delivered to the radio — the
// safety-driven connect trigger (R2/KTD-5). U6 calls it when PTT must be
// forced off (watchdog trip, MQTT-loss rule, session-loss reissue).
//
// While live the frame goes out immediately. Otherwise it becomes the
// outstanding PTT-off: a safety-class connect demand runs, bounded by the
// same MaxAttempts/AttemptSpacing as every demand; a live session delivers
// the frame before anything else. If the bound exhausts, the terminal
// safety-class fact FactPTTOffUndeliverable lands in the error state and
// the demand concludes (no retry storm) — U6 re-requests if the safety
// condition persists. It never blocks.
func (s *Session) RequestPTTOff() {
	s.post(s.requestPTTOffLocked)
}

func (s *Session) requestPTTOffLocked() {
	s.pttOff = true
	s.publish()
	if s.state == StateLive {
		s.deliverPTTOff()
		return
	}
	err := s.runSeries(demandSafety)
	s.concludePTTOff(err)
}

// concludePTTOff settles a safety connect series for the outstanding
// PTT-off.
func (s *Session) concludePTTOff(err error) {
	switch {
	case err == nil:
		s.deliverPTTOff()
	case s.ctx.Err() != nil:
		// Shutting down: the error state is moot, teardown owns the rest.
		s.log.Warn("PTT-off undelivered, session stack shutting down")
	default:
		// Terminal: the bound is exhausted. Safety-class fact, exempt from
		// decay-clearing; the demand does not retry on its own.
		s.fact, s.factSafety = FactPTTOffUndeliverable, true
		s.enter(StateError)
		s.pttOff = false
		s.publish()
	}
}

// Do runs fn once on a live session — the work execution seam. It registers
// the work, drives the idle→connecting demand when no session is held (R2:
// cmd-driven connect), waits for the session and runs fn; the idle timer
// re-arms after the work. A failed series or an involuntary session loss
// rejects the work with the observed failure fact — a queued cmd is never
// silently dropped. Keepalive and telemetry traffic must NOT come through
// Do (it would hold the session open forever; KTD-2).
func (s *Session) Do(ctx context.Context, fn func(tr *civ.Transport) error) error {
	w := &waiter{fn: fn, reply: make(chan error, 1)}
	if !s.post(func() { s.enqueueWork(w) }) {
		return ErrEventQueueFull
	}
	select {
	case err := <-w.reply:
		return err
	case <-ctx.Done():
		// The caller left; the session still runs or rejects the work — the
		// reply lands in the buffered channel.
		return ctx.Err()
	}
}

// Disconnect tears a live session down politely (courtesy logout + token
// removal) without touching the arm permit. It blocks until the teardown
// ran or the session stack is shutting down.
func (s *Session) Disconnect() {
	reply := make(chan struct{}, 1)
	if !s.post(func() {
		s.closeTransport()
		if s.state == StateLive {
			s.enter(StateIdle)
		}
		reply <- struct{}{}
	}) {
		return
	}
	select {
	case <-reply:
	case <-s.ctx.Done():
	}
}

// Close shuts the session stack down (idempotent): any live transport is
// closed, queued work is rejected, the manager goroutine exits. Close blocks
// until the wind-down finished; fn bodies in flight must be bounded (the
// Do contract).
func (s *Session) Close() {
	s.cancel()
	<-s.done
}

// --- the manager goroutine ------------------------------------------------

func (s *Session) run() {
	defer s.once.Do(func() { close(s.done) })
	for {
		select {
		case <-s.ctx.Done():
			s.teardown()
			return
		case f := <-s.ev:
			f()
		case <-s.idleTimer.C:
			s.idleExpired()
		case <-s.decayTimer.C:
			s.decayExpired()
		}
	}
}

// teardown is the ctx-done wind-down: no courtesy states, no hooks beyond
// the final snapshot — the process is leaving.
func (s *Session) teardown() {
	s.closeTransport()
	stopTimer(s.idleTimer)
	stopTimer(s.decayTimer)
	s.rejectWaiters("radio: session stack shut down")
	s.state = StateIdle
	s.publish()
}

// post queues an event closure. It never blocks the caller (the stationa
// paho-handler rule); a full queue fails fast instead.
func (s *Session) post(f func()) bool {
	select {
	case s.ev <- f:
		return true
	default:
		s.log.Warn("session event queue full, event dropped")
		return false
	}
}

// postBlocking queues the session-loss callback: the loss event must not be
// droppable, and the transport runs the callback on its own goroutine, so a
// blocking send with the ctx escape is safe here.
func (s *Session) postBlocking(f func()) {
	select {
	case s.ev <- f:
	case <-s.ctx.Done():
	}
}

// enqueueWork settles one queued work item (Do's closure).
func (s *Session) enqueueWork(w *waiter) {
	switch s.state {
	case StateLive:
		s.runWork(w)
	case StateIdle, StateError:
		// cmd-driven connect demand (R2)
		s.waiters = append(s.waiters, w)
		s.concludeDemand(demandCmd, s.runSeries(demandCmd))
	case StateConnecting:
		// A series is concluding right now (closures never run mid-series);
		// the running demand resolves the waiter. Defensive only.
		s.log.Warn("work queued while connecting; waiting for the running demand")
		s.waiters = append(s.waiters, w)
	}
}

// armLocked settles SetArmed(true) (its closure).
func (s *Session) armLocked(w *waiter) {
	if s.armed && s.state == StateLive {
		w.reply <- nil // already holding the session open
		return
	}
	s.armed = true
	if s.state == StateLive {
		// Permit on a live session: held open from now on (R11).
		s.log.Info("arm permit set on a live session")
		s.resetIdleTimer()
		s.publish()
		w.reply <- nil
		return
	}
	// idle or error: the arm set is the connect demand (R2). No permit ever
	// dials on its own — without this demand there are no login attempts.
	s.log.Info("arm permit set, connecting on demand")
	s.waiters = append(s.waiters, w)
	s.concludeDemand(demandArm, s.runSeries(demandArm))
}

// runSeries performs one bounded connect demand (R2): up to MaxAttempts
// civ.Dial calls, every call ≥ AttemptSpacing after the previous one — the
// single spacing invariant that makes a retry storm impossible. It enters
// connecting and, on success, live. On exhaustion it returns the last dial
// error; the caller classifies it (concludeDemand / concludePTTOff).
func (s *Session) runSeries(kind demandKind) error {
	s.enter(StateConnecting)
	var lastErr error
	for attempt := 1; attempt <= s.cfg.MaxAttempts; attempt++ {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		if wait := s.spacingWait(); wait > 0 {
			s.log.Debug("connect attempt spaced", "attempt", attempt, "wait", wait.String())
			select {
			case <-s.ctx.Done():
				return s.ctx.Err()
			case <-time.After(wait):
			}
		}
		trGen := s.gen + 1
		s.lastDial = time.Now()
		tr, err := s.dial(trGen)
		if err == nil {
			s.gen = trGen
			s.tr = tr
			s.enter(StateLive)
			return nil
		}
		lastErr = err
		if s.ctx.Err() != nil {
			return s.ctx.Err()
		}
		s.log.Warn("radio connect attempt failed",
			"attempt", attempt, "max_attempts", s.cfg.MaxAttempts, "fact", observedFact(err))
	}
	return lastErr
}

// concludeDemand settles a finished cmd/arm connect series.
func (s *Session) concludeDemand(kind demandKind, err error) {
	switch {
	case err == nil:
		s.resolveWaiters(nil)
	case s.ctx.Err() != nil:
		s.rejectWaiters("radio: session stack shut down")
	default:
		fact := observedFact(err)
		s.fact, s.factSafety = fact, false
		s.enter(StateError)
		if kind == demandArm && s.armed {
			// The failed arm-triggered connect rejects the arm: the permit
			// drops (fail-disarmed), the safety core hears about it.
			s.armed = false
			s.log.Warn("arm permit dropped: connect failed", "fact", fact)
			if s.hooks.OnArmDrop != nil {
				s.hooks.OnArmDrop("connect failed: " + fact)
			}
		}
		s.publish()
		// The rejected callers wake only after the settled snapshot
		// (error state, dropped permit) is published — SetArmed's contract.
		s.rejectWaiters(fact)
	}
}

// resolveWaiters runs the queued work after a successful series (each item
// against the live transport) — or rejects every item after a failed one.
// The failure classification (error state, arm-permit drop) is
// concludeDemand's job; this only settles the callers.
func (s *Session) resolveWaiters(err error) {
	ws := s.waiters
	s.waiters = nil
	if err != nil {
		fact := observedFact(err)
		for _, w := range ws {
			w.reply <- fmt.Errorf("radio: %s", fact)
		}
		return
	}
	for _, w := range ws {
		s.runWork(w)
	}
}

// rejectWaiters rejects every queued work item with the given fact (involuntary
// session loss, shutdown) — never silently dropped (plan R3).
func (s *Session) rejectWaiters(fact string) {
	ws := s.waiters
	s.waiters = nil
	for _, w := range ws {
		w.reply <- fmt.Errorf("radio: %s", fact)
	}
}

// runWork executes one work item against the live transport.
func (s *Session) runWork(w *waiter) {
	if w.fn == nil {
		// The arm demand: being live is what it asked for. A successful arm
		// is operator activity — it acks the error fact (HTD: error → idle
		// "or operator ack via next successful activity").
		s.ackErrorFact()
		w.reply <- nil
		return
	}
	err := w.fn(s.tr)
	if err != nil {
		s.log.Warn("radio work failed", "err", err)
	} else {
		s.ackErrorFact()
	}
	w.reply <- err
	s.resetIdleTimer() // the work just done re-arms the idle window
	s.publish()
}

// onSessionLoss handles an involuntary session end (R3) — the transport's
// OnSessionLoss callback, filtered to the current generation.
func (s *Session) onSessionLoss(trGen uint64, err error) {
	if trGen != s.gen || s.tr == nil || s.state != StateLive {
		return // stale callback: that transport was already replaced/closed
	}
	s.tr = nil
	s.gen++
	s.log.Warn("radio session lost", "err", err)

	// In-flight cmds resolve as rejected (R3).
	s.rejectWaiters(factSessionLost)
	wasArmed := s.armed
	s.armed = false

	// Republish with the radio-measured fields omitted BEFORE any reconnect
	// dialing (flexbridge Reset lesson: change-gating must not freeze stale
	// telemetry).
	if s.hooks.OnSessionDown != nil {
		s.hooks.OnSessionDown(factSessionLost)
	}
	if wasArmed {
		// R3: an involuntary session loss disarms, always.
		s.log.Warn("arm permit dropped: session lost")
		if s.hooks.OnArmDrop != nil {
			s.hooks.OnArmDrop(factSessionLost)
		}
	}
	s.publish()

	if s.pttOff {
		// Work pending (the outstanding PTT-off): live → connecting, the
		// safety re-delivery demand.
		s.concludePTTOff(s.runSeries(demandSafety))
		return
	}
	// No work pending: live → error, decaying to idle per ErrorDecay.
	s.fact, s.factSafety = factSessionLost, false
	s.enter(StateError)
}

// deliverPTTOff sends the outstanding PTT-off frame. It runs only while a
// transport is installed.
func (s *Session) deliverPTTOff() {
	if s.tr == nil {
		return
	}
	if err := s.tr.SendCIV(s.codec.BuildPTT(false)); err != nil {
		// The session is dying or dead: the frame stays outstanding; the
		// session-loss path re-drives it via the safety demand.
		s.log.Warn("PTT-off send failed, frame stays outstanding", "err", err)
		s.publish()
		return
	}
	s.pttOff = false
	s.log.Warn("PTT-off delivered") // Warn: safety-relevant, journalctl -p warning
	s.publish()
}

// --- state transitions (manager goroutine only) ---------------------------

// enter switches the session state and services the timers that belong to
// the edges (idle window on live, decay on error).
func (s *Session) enter(st SessionState) {
	if s.state == st {
		return
	}
	s.state = st
	switch st {
	case StateLive:
		stopTimer(s.decayTimer)
		s.resetIdleTimer()
		s.log.Info("radio session live")
	case StateError:
		stopTimer(s.idleTimer)
		resetTimer(s.decayTimer, s.cfg.ErrorDecay)
		s.log.Warn("session error", "fact", s.fact, "decay", s.cfg.ErrorDecay.String())
	case StateConnecting:
		stopTimer(s.idleTimer)
		stopTimer(s.decayTimer)
		s.log.Debug("session connecting")
	case StateIdle:
		stopTimer(s.idleTimer)
		stopTimer(s.decayTimer)
		s.log.Debug("session idle")
	}
	s.publish()
}

// ackErrorFact clears the error fact on operator ack via the next successful
// activity (HTD). Safety-class facts clear here too — the ack is the
// operator's; only the DECAY timer may not clear them.
func (s *Session) ackErrorFact() {
	if s.fact == "" {
		return
	}
	s.fact, s.factSafety = "", false
	s.publish()
}

// idleExpired takes a live session down after IdleTimeout of no work — never
// while the arm permit holds it open (R11) or work is outstanding.
func (s *Session) idleExpired() {
	if s.state != StateLive || s.armed || s.pttOff || len(s.waiters) > 0 {
		return // armed holds open; pending work holds; not live: moot
	}
	s.log.Info("session idle timeout, disconnecting", "idle", s.cfg.IdleTimeout.String())
	s.closeTransport() // polite: courtesy logout + token removal
	s.enter(StateIdle)
}

// decayExpired decays the error state to idle. Safety-class facts are exempt
// from clearing by decay: the state decays, the fact persists until operator
// ack (R17 posture).
func (s *Session) decayExpired() {
	if s.state != StateError {
		return
	}
	if s.factSafety {
		s.log.Info("error state decayed to idle; safety fact persists until operator ack", "fact", s.fact)
		s.enter(StateIdle)
		return
	}
	s.log.Info("error state decayed to idle", "decay", s.cfg.ErrorDecay.String())
	s.fact, s.factSafety = "", false
	s.enter(StateIdle)
}

// publish mirrors the goroutine-owned state into the accessor snapshot and
// fires OnStateChange without any lock held (the hook may call back into the
// accessors).
func (s *Session) publish() {
	snap := Snapshot{State: s.state, Error: s.fact, ErrorSafety: s.factSafety, Armed: s.armed}
	s.mu.Lock()
	s.snap = snap
	s.pttOffPub = s.pttOff
	s.mu.Unlock()
	s.fireStateChange(snap)
}

func (s *Session) fireStateChange(snap Snapshot) {
	if s.hooks.OnStateChange != nil {
		s.hooks.OnStateChange(snap)
	}
}

// resetIdleTimer re-arms the idle window (live sessions only).
func (s *Session) resetIdleTimer() {
	if s.state != StateLive {
		return
	}
	resetTimer(s.idleTimer, s.cfg.IdleTimeout)
}

// closeTransport tears the current transport down (polite Close: courtesy
// logout, token removal) and bumps the generation so its loss callback —
// if one is already in flight — lands stale.
func (s *Session) closeTransport() {
	if s.tr != nil {
		s.gen++
		s.tr.Close()
		s.tr = nil
	}
}

// spacingWait reports how long the next dial must wait to keep the ≥
// AttemptSpacing invariant since the previous dial.
func (s *Session) spacingWait() time.Duration {
	if s.lastDial.IsZero() {
		return 0
	}
	d := s.cfg.AttemptSpacing - time.Since(s.lastDial)
	if d < 0 {
		return 0
	}
	return d
}

// dial builds the transport options and runs one handshake. trGen is the
// generation the transport will carry once installed (its loss callback is
// filtered against it).
func (s *Session) dial(trGen uint64) (*civ.Transport, error) {
	o := civ.Opts{
		Host:        s.cfg.Host,
		ControlPort: s.cfg.ControlPort,
		Username:    s.cfg.Username,
		Password:    s.cfg.Password,
		Log:         s.log,
		OnCIVFrame:  s.hooks.OnCIVFrame,
		OnSessionLoss: func(err error) {
			// Runs on its own goroutine (transport contract): the blocking
			// post is safe and cannot lose the event.
			s.postBlocking(func() { s.onSessionLoss(trGen, err) })
		},
	}
	if s.cfg.Opts != nil {
		s.cfg.Opts(&o)
	}
	return civ.Dial(s.ctx, o)
}

// observedFact reduces a dial failure to the short fact /state.error carries
// (R2: observed failure facts only).
func observedFact(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, civ.ErrAuthFailed):
		return factLoginRefused
	case errors.Is(err, civ.ErrRefused):
		return factConnRefused
	case errors.Is(err, civ.ErrTimeout):
		return factHandshakeTO
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return factCancelled
	default:
		return factDialFailed
	}
}

// --- small timer helpers (manager-goroutine-only state) -------------------

func newStoppedTimer() *time.Timer {
	t := time.NewTimer(time.Hour)
	if !t.Stop() {
		<-t.C
	}
	return t
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func stopTimer(t *time.Timer) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}
