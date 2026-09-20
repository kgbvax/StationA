// Package radio owns the IC-9700's on-demand session lifecycle (U4): the
// state machine that connects only on work (a pending command or the armed
// hold), spaces login attempts, decays error states, and keeps the radio's
// single LAN session available for manual wfview use (KTD-2).
//
// Work definition (R1): a pending command demand or the armed hold. Radio
// keepalives, ping replies and meter polls are never work. While the hold
// is set the session is kept open (R11); the hold is bridge-held and drops
// on any session loss (fail-disarmed, KTD-6).
//
// Concurrency model: one Run goroutine owns the state machine and the
// live client; callers interact through channels (Demand/SetHold) so the
// state can never be observed mid-transition. The single-command mutex
// inside Demand serializes frame round-trips.
package radio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"icom9700-radio-bridge/internal/civ"
)

// SessionState values surfaced in Snapshot (the /state.session_state
// vocabulary, R5/R16).
const (
	StateIdle       = "idle"
	StateConnecting = "connecting"
	StateLive       = "live"
	StateError      = "error"
)

// SessionOptions configure the lifecycle. All timers are shrinkable for
// tests against the fake radio.
type SessionOptions struct {
	Host           string
	Username       string
	Password       string
	IdleTimeout    time.Duration // live without work -> disconnect (R1)
	MaxAttempts    int           // login attempts per connect demand (R2)
	AttemptSpacing time.Duration // minimum spacing between attempts (R2)
	DecayTimeout   time.Duration // error state decays to idle after this (plan state machine)
	HandshakeTO    time.Duration // per-command reply wait
	// Passthroughs to the transport (tests shrink them; production takes
	// the civ defaults).
	ControlPort     int
	CIVPort         int
	AreYouThere     time.Duration
	HandshakeBudget time.Duration
	LossWatchdog    time.Duration
	Logger          *slog.Logger
}

// Snapshot is the lifecycle truth for the /state assembly (U5): the
// session state, the observed failure fact, and the hold (armed) flag.
type Snapshot struct {
	SessionState string
	Err          string
	Hold         bool
	RadioName    string
	// ConnectAttempts is the cumulative count of Dial attempts since the
	// session was created (diagnostics; the attempt-policy tests pin it).
	ConnectAttempts int
}

// ErrSessionStopped is returned by demands arriving after the manager
// stopped — never a silent drop, never a guessed state.
var ErrSessionStopped = errors.New("radio: session manager stopped")

// demand is one unit of work: run fn against a live client; the errCh
// receives fn's error (or the connect failure).
type demand struct {
	fn    func(*civ.Client) error
	errCh chan error
}

// Session is the on-demand state machine. Create with NewSession, run Run,
// drive it with Demand/SetHold.
type Session struct {
	opts SessionOptions
	log  *slog.Logger

	demands chan demand
	holdCh  chan holdReq
	stopped chan struct{}
	runDone chan struct{}

	mu       sync.Mutex
	snap     Snapshot
	hold     bool
	onLive   func(ctx context.Context, c *civ.Client) error
	onLoss   func(err error)
	client   *civ.Client
	routeCh  chan struct{} // closed when the routed client is torn down
	notifyCh chan struct{}
	transCh  chan civ.Transceive
	replyCh  chan civ.Frame // read replies, consumed by roundTrip
}

type holdReq struct {
	on  bool
	err chan error
}

// NewSession builds the state machine.
func NewSession(opts SessionOptions) *Session {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 120 * time.Second
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 3
	}
	if opts.AttemptSpacing <= 0 {
		opts.AttemptSpacing = 30 * time.Second
	}
	if opts.DecayTimeout <= 0 {
		opts.DecayTimeout = opts.IdleTimeout
	}
	if opts.HandshakeTO <= 0 {
		opts.HandshakeTO = 2 * time.Second
	}
	return &Session{
		opts:     opts,
		log:      opts.Logger.With("component", "radio-session"),
		demands:  make(chan demand, 16),
		holdCh:   make(chan holdReq, 1),
		stopped:  make(chan struct{}),
		runDone:  make(chan struct{}),
		notifyCh: make(chan struct{}, 1),
		transCh:  make(chan civ.Transceive, 64),
		replyCh:  make(chan civ.Frame, 8),
	}
}

// OnLive registers the post-reconnect hook (the safety core reissues an
// outstanding PTT-off here — plan state machine, live -> connecting with
// work pending). One hook; last registration wins.
func (s *Session) OnLive(fn func(ctx context.Context, c *civ.Client) error) {
	s.mu.Lock()
	s.onLive = fn
	s.mu.Unlock()
}

// OnLoss registers the session-loss hook (the safety core disarms and
// schedules the safety-driven PTT-off delivery here, R2/R12).
func (s *Session) OnLoss(fn func(err error)) {
	s.mu.Lock()
	s.onLoss = fn
	s.mu.Unlock()
}

// Notify returns a channel that receives a nudge on every state change
// (coalesced — the /state assembly republishes on each).
func (s *Session) Notify() <-chan struct{} { return s.notifyCh }

// Transceives returns the parsed inbound transceive stream (freq/mode/tx
// events from the radio; unsupported-mode events dropped per R7).
func (s *Session) Transceives() <-chan civ.Transceive { return s.transCh }

// Snapshot returns the current lifecycle truth.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

// SetHold sets/clears the armed hold (R11): setting it is a connect demand
// (arm-while-idle is the connect trigger); clearing it releases the
// session to the idle timer. Returns the connect failure when the hold's
// connect demand fails (the arm cmd is rejected, hold stays false — the
// operator re-arms, R2).
func (s *Session) SetHold(ctx context.Context, on bool) error {
	select {
	case <-s.stopped:
		return ErrSessionStopped
	default:
	}
	if on {
		_, err := s.connectAndRun(ctx, func(*civ.Client) error { return nil })
		if err != nil {
			return err
		}
	}
	done := make(chan error, 1)
	select {
	case s.holdCh <- holdReq{on: on, err: done}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stopped:
		return ErrSessionStopped
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Demand ensures a live session and runs fn against it (the work-demand
// primitive everything above the session goes through). A connect failure
// rejects the demand with the observed fact (R2).
func (s *Session) Demand(ctx context.Context, fn func(*civ.Client) error) error {
	_, err := s.connectAndRun(ctx, fn)
	return err
}

// connectAndRun submits a work demand, driving idle/error -> connecting if
// needed, and waits for it to execute or fail.
func (s *Session) connectAndRun(ctx context.Context, fn func(*civ.Client) error) (*civ.Client, error) {
	d := demand{fn: fn, errCh: make(chan error, 1)}
	select {
	case s.demands <- d:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.stopped:
		return nil, ErrSessionStopped
	}
	select {
	case err := <-d.errCh:
		s.mu.Lock()
		c := s.client
		s.mu.Unlock()
		return c, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// setState records a state change, notifies watchers, and logs.
func (s *Session) setState(state, errText string) {
	s.mu.Lock()
	prev := s.snap.SessionState
	s.snap.SessionState = state
	s.snap.Err = errText
	s.mu.Unlock()
	if prev != state {
		s.log.Info("session state", "from", prev, "to", state, "err", errText)
	}
	select {
	case s.notifyCh <- struct{}{}:
	default:
	}
}

func (s *Session) setRadioName(name string) {
	s.mu.Lock()
	s.snap.RadioName = name
	s.mu.Unlock()
}

// Run drives the state machine until ctx is cancelled. It returns the
// ctx error.
func (s *Session) Run(ctx context.Context) error {
	defer close(s.runDone)
	defer s.closeClient()

	// Derived timers, owned by this goroutine.
	var idleTimer *time.Timer
	var idleC <-chan time.Time
	var decayTimer *time.Timer
	var decayC <-chan time.Time
	stopIdle := func() {
		if idleTimer != nil {
			idleTimer.Stop()
			idleTimer = nil
			idleC = nil
		}
	}
	stopDecay := func() {
		if decayTimer != nil {
			decayTimer.Stop()
			decayTimer = nil
			decayC = nil
		}
	}
	defer stopIdle()
	defer stopDecay()

	s.setState(StateIdle, "")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case d := <-s.demands:
			s.mu.Lock()
			live := s.snap.SessionState == StateLive && s.client != nil
			s.mu.Unlock()
			if live {
				// Already live: run now, restart the idle clock (this was
				// work; keepalives never reach this path).
				s.runDemand(d)
				stopIdle()
				if !s.held() {
					idleTimer = time.NewTimer(s.opts.IdleTimeout)
					idleC = idleTimer.C
				}
				continue
			}
			// idle or error: connect on demand. A pending decay stops —
			// the next demand IS the operator's retry (>= spacing held).
			stopDecay()
			err := s.connectWithRetries(ctx)
			if err != nil {
				d.errCh <- err // reject with the observed fact (R2)
				continue
			}
			s.runDemand(d)
			stopIdle()
			if !s.held() {
				idleTimer = time.NewTimer(s.opts.IdleTimeout)
				idleC = idleTimer.C
			}

		case h := <-s.holdCh:
			s.mu.Lock()
			s.hold = h.on
			s.snap.Hold = h.on
			s.mu.Unlock()
			s.notify()
			h.err <- nil
			if h.on {
				stopIdle() // armed holds the session open (R11)
			} else if s.live() {
				// Disarm with no pending traffic starts the idle clock.
				stopIdle()
				idleTimer = time.NewTimer(s.opts.IdleTimeout)
				idleC = idleTimer.C
			}

		case <-idleC:
			// Live without work and not held: disconnect politely (R1) —
			// the radio's session goes back to manual wfview use.
			s.log.Info("idle timeout, disconnecting (session free for wfview)")
			s.closeClient()
			stopIdle()
			s.setState(StateIdle, "")

		case <-decayC:
			// An error state with no retrying operator decays to idle so a
			// stale fault never reads as current (plan state machine).
			stopDecay()
			if s.snapshot().SessionState == StateError {
				s.setState(StateIdle, "")
			}

		case err := <-s.lostC():
			// Involuntary session loss (radio reboot, wfview eviction,
			// network): fields-omitted republish is the /state assembly's
			// job on the notify below; the hold drops (fail-disarmed, R11)
			// and the full re-login happens on the next demand.
			s.closeClient()
			stopIdle()
			held := s.held()
			s.mu.Lock()
			s.hold = false
			s.snap.Hold = false
			s.mu.Unlock()
			s.setState(StateError, err.Error())
			decayTimer = time.NewTimer(s.opts.DecayTimeout)
			decayC = decayTimer.C
			s.mu.Lock()
			fn := s.onLoss
			s.mu.Unlock()
			if fn != nil {
				fn(err)
			}
			if held {
				s.log.Warn("session hold dropped on loss (fail-disarmed)", "err", err)
			}
		}
	}
}

// lostC wraps the client's Lost channel for the Run select. The client is
// only replaced inside this goroutine (connect/close), so reading it here
// is race-free.
func (s *Session) lostC() <-chan error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil // nil channel blocks forever — no live session to lose
	}
	return s.client.Lost
}

func (s *Session) live() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap.SessionState == StateLive && s.client != nil
}

func (s *Session) held() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hold
}

func (s *Session) snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

func (s *Session) notify() {
	select {
	case s.notifyCh <- struct{}{}:
	default:
	}
}

// connectWithRetries performs the full Dial with the attempt policy
// (MaxAttempts attempts, >= AttemptSpacing apart — no login storm under
// wfview contention, R2). Every attempt is a fresh handshake (R3).
func (s *Session) connectWithRetries(ctx context.Context) error {
	s.setState(StateConnecting, "")
	var lastErr error
	for attempt := 1; attempt <= s.opts.MaxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(s.opts.AttemptSpacing):
			case <-ctx.Done():
				s.setState(StateError, ctx.Err().Error())
				return ctx.Err()
			}
		}
		s.mu.Lock()
		s.snap.ConnectAttempts++
		s.mu.Unlock()
		c, err := civ.Dial(ctx, civ.Options{
			Host:            s.opts.Host,
			ControlPort:     s.opts.ControlPort,
			CIVPort:         s.opts.CIVPort,
			Username:        s.opts.Username,
			Password:        s.opts.Password,
			AreYouThere:     s.opts.AreYouThere,
			HandshakeTO:     s.opts.HandshakeTO,
			HandshakeBudget: s.opts.HandshakeBudget,
			LossWatchdog:    s.opts.LossWatchdog,
			Logger:          s.log,
		})
		if err != nil {
			lastErr = err
			if errors.Is(err, civ.ErrLoginBusy) {
				// The radio explicitly refused: its LAN session is held.
				// Retrying into a held session is a login storm — fail
				// now; the next demand retries the series.
				s.log.Warn("login refused: radio LAN session is held",
					"attempt", attempt, "of", s.opts.MaxAttempts)
				break
			}
			s.log.Warn("connect attempt failed",
				"attempt", attempt, "of", s.opts.MaxAttempts, "err", err)
			continue
		}
		s.mu.Lock()
		s.client = c
		s.routeCh = make(chan struct{})
		routeCh := s.routeCh
		s.mu.Unlock()
		s.setState(StateLive, "")
		s.setRadioName(c.RadioName())
		go s.routeFrames(c, routeCh)
		s.fireOnLive(ctx, c)
		return nil
	}
	s.setState(StateError, lastErr.Error())
	return lastErr
}

// fireOnLive runs the safety hook (PTT-off first on reconnect) after a
// successful handshake, before any pending demand executes.
func (s *Session) fireOnLive(ctx context.Context, c *civ.Client) {
	s.mu.Lock()
	fn := s.onLive
	s.mu.Unlock()
	if fn != nil {
		if err := fn(ctx, c); err != nil {
			s.log.Warn("on-live hook failed", "err", err)
		}
	}
}

// runDemand executes one demand against the live client. Replies from the
// previous command are drained first — the demand path is serialized, so
// the single reply slot always belongs to the current command.
func (s *Session) runDemand(d demand) {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c == nil {
		d.errCh <- errors.New("radio: no live session")
		return
	}
	for {
		select {
		case <-s.replyCh:
		default:
		}
		break
	}
	d.errCh <- d.fn(c)
}

// routeFrames reads the live client's frames until the session ends:
// read replies go to the round-trip waiter, transceive events to the
// bus-facing stream (unsupported modes dropped, R7).
func (s *Session) routeFrames(c *civ.Client, done chan struct{}) {
	for {
		select {
		case <-done:
			return
		case raw, ok := <-c.Frames():
			if !ok {
				return
			}
			f, err := civ.ParseFrame(raw)
			if err != nil {
				s.log.Debug("dropping unparseable frame", "err", err)
				continue
			}
			if f.Direct {
				// A direct reply (E0 A2) routes to the (serialized) command
				// waiter regardless of terminator — real firmware answers
				// reads with FD-terminated frames (e.g. FE FE E0 A2 15 02
				// <hi> <lo> FD) and sets with the bare FB/FA acknowledge
				// (bench 2026-09-20); keying routing on the terminator
				// dropped every read reply.
				select {
				case s.replyCh <- f:
				default:
					s.log.Warn("reply slot full, dropping reply", "cmd", f.Cmd)
				}
				continue
			}
			tr, ok, err := civ.ParseTransceive(f)
			if err != nil {
				// Unsupported-mode events: consumed, omitted (R7).
				continue
			}
			if !ok {
				continue // unrelated frames
			}
			select {
			case s.transCh <- tr:
			default:
				s.log.Warn("transceive stream full, dropping oldest event")
				select {
				case <-s.transCh:
				default:
				}
				select {
				case s.transCh <- tr:
				default:
				}
			}
		}
	}
}

// RoundTrip sends one command frame and waits for its reply (FB/FA), which
// the frame router delivers on replyCh. NG becomes the typed rejection.
// Callers serialize through the demand path.
func (s *Session) RoundTrip(ctx context.Context, c *civ.Client, frame []byte, wait time.Duration) (civ.Frame, error) {
	if err := c.SendCIV(frame); err != nil {
		return civ.Frame{}, fmt.Errorf("radio: send: %w", err)
	}
	deadline := time.After(wait)
	// Sub-command queries sharing a command byte (15 02 / 15 12 / 15 13)
	// are told apart by the reply's repeated FIRST sub byte — the real
	// radio echoes it (bench 2026-09-20). A trailing read-placeholder byte
	// (07 d2 00 → 07 d2 <value>) is NOT echoed verbatim, so only the first
	// sub byte is matched.
	wantSub := frame[5 : len(frame)-1]
	for {
		select {
		case f := <-s.replyCh:
			// Calls are serialized, so a bare FB/FA acknowledge (the real
			// radio answers sets with FE FE E0 A2 FB — no command echo)
			// necessarily answers the pending command. Everything else must
			// carry the sent command byte and the discriminating sub byte.
			isAck := f.Cmd == civ.TerminatorOK || f.Cmd == civ.TerminatorNG
			matches := f.Cmd == frame[4] &&
				(len(wantSub) == 0 || bytes.HasPrefix(f.Sub, wantSub[:1]))
			if !matches && !isAck {
				continue // a late reply to an earlier command
			}
			if f.IsNG() {
				return civ.Frame{}, f.AsNG()
			}
			return f, nil
		case <-deadline:
			return civ.Frame{}, fmt.Errorf("radio: no reply to command %02x within %s", frame[4], wait)
		case <-ctx.Done():
			return civ.Frame{}, ctx.Err()
		case <-c.Lost:
			return civ.Frame{}, errors.New("radio: session lost awaiting reply")
		}
	}
}

// closeClient tears the live client down (idempotent) and stops its frame
// router. Run is the only reader of client.Lost.
func (s *Session) closeClient() {
	s.mu.Lock()
	c := s.client
	s.client = nil
	routeCh := s.routeCh
	s.routeCh = nil
	s.mu.Unlock()
	if routeCh != nil {
		close(routeCh)
	}
	if c != nil {
		c.Close()
	}
}
