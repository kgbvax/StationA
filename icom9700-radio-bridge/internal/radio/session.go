// Package radio owns the IC-9700's on-demand capture-session lifecycle:
// the state machine that connects only on work (the audio demand), spaces
// login attempts, decays error states, and keeps the radio's single LAN
// session available for manual wfview use (KTD-2).
//
// Work definition (R1, receive-only posture 2026-09): the audio demand is
// the ONLY session hold. There are no LAN CI-V commands left — the LAN
// session exists solely to carry the :50003 receive-audio stream. While
// the audio demand is set the session is kept open; the demand is
// TTL-bounded and refreshed by the consumer's audio_on heartbeats, so a
// dead consumer never pins the radio (KTD-2). Telemetry flows over the
// dedicated serial CI-V monitor (internal/civserial), never over this
// session.
//
// Concurrency model: one Run goroutine owns the state machine and the
// live client; callers interact through channels (SetAudioDemand) so the
// state can never be observed mid-transition.
package radio

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"icom9700-radio-bridge/internal/civ"
)

// SessionState values surfaced in Snapshot (the /state.session_state
// vocabulary, R5/R16 — redefined 2026-09 as the CAPTURE-session state).
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
	HandshakeTO    time.Duration // per-handshake-step reply wait
	// Passthroughs to the transport (tests shrink them; production takes
	// the civ defaults).
	ControlPort     int
	CIVPort         int
	AudioPort       int
	AreYouThere     time.Duration
	HandshakeBudget time.Duration
	LossWatchdog    time.Duration
	// PingInterval passes through to the transport's keepalive cadence
	// (tests shrink it; it must stay ≪ LossWatchdog — see fastSessionOpts).
	PingInterval time.Duration
	// AudioDemandTTL bounds an audio demand without a refreshing audio_on
	// (a dead preview consumer must not pin the radio session). 0 = 60 s.
	AudioDemandTTL time.Duration
	// AudioSink receives the demodulated audio PCM chunks while the audio
	// stream is open (the main wiring publishes them to the preview host).
	// nil = audio demand connects but no PCM is delivered.
	AudioSink func([]byte)
	Logger    *slog.Logger
}

// Snapshot is the lifecycle truth for the /state assembly (U5): the
// capture-session state and the observed failure fact.
type Snapshot struct {
	SessionState string
	Err          string
	AudioDemand  bool
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

// Session is the on-demand capture-session state machine. Create with
// NewSession, run Run, drive it with SetAudioDemand.
type Session struct {
	opts SessionOptions
	log  *slog.Logger

	demands  chan demand
	audioCh  chan audioReq
	stopped  chan struct{}
	runDone  chan struct{}

	mu           sync.Mutex
	snap         Snapshot
	audioDemand  bool
	client       *civ.Client
	routeCh      chan struct{} // closed when the frame drainer is torn down
	audioRouteCh chan struct{} // same, for the audio PCM router
	notifyCh     chan struct{}
}

type audioReq struct {
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
	if opts.AudioDemandTTL <= 0 {
		opts.AudioDemandTTL = 60 * time.Second
	}
	return &Session{
		opts:     opts,
		log:      opts.Logger.With("component", "radio-session"),
		demands:  make(chan demand, 16),
		audioCh:  make(chan audioReq, 1),
		stopped:  make(chan struct{}),
		runDone:  make(chan struct{}),
		notifyCh: make(chan struct{}, 1),
	}
}

// Notify returns a channel that receives a nudge on every state change
// (coalesced — the /state assembly republishes on each).
func (s *Session) Notify() <-chan struct{} { return s.notifyCh }

// Snapshot returns the current lifecycle truth.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

// SetAudioDemand sets/clears the audio demand: on = connect (when idle) and
// open the audio receive stream; off = close it. The demand is TTL-bounded
// (AudioDemandTTL) — consumers refresh it with repeated audio_on — so a dead
// consumer never pins the radio session (KTD-2). A connect failure while
// setting the demand rejects the cmd; the demand itself is NOT set in that
// case (the operator retries). This is the session's only hold source: no
// LAN command path exists anymore (receive-only posture, 2026-09).
func (s *Session) SetAudioDemand(ctx context.Context, on bool) error {
	select {
	case <-s.stopped:
		return ErrSessionStopped
	default:
	}
	if on {
		if _, err := s.connectAndRun(ctx, func(*civ.Client) error { return nil }); err != nil {
			return err
		}
	}
	done := make(chan error, 1)
	select {
	case s.audioCh <- audioReq{on: on, err: done}:
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

// audioDemanded reports the audio demand under the state mutex.
func (s *Session) audioDemanded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioDemand
}

// ensureAudio opens the audio receive stream + PCM router on the live client
// when the demand is set (post-connect and post-audio_on). Failures are
// logged, never fatal: audio is auxiliary and the next heartbeat retries.
func (s *Session) ensureAudio() {
	if !s.audioDemanded() {
		return
	}
	s.mu.Lock()
	c := s.client
	sink := s.opts.AudioSink
	routeCh := s.audioRouteCh
	s.mu.Unlock()
	if c == nil {
		return
	}
	if err := c.OpenAudio(); err != nil {
		s.log.Warn("audio stream open failed", "err", err)
		return
	}
	if routeCh != nil {
		return // router already running for this client
	}
	s.mu.Lock()
	s.audioRouteCh = make(chan struct{})
	routeCh = s.audioRouteCh
	s.mu.Unlock()
	go s.routeAudio(c, routeCh, sink)
}

// routeAudio pumps PCM chunks from the live client's audio stream to the
// configured sink until the session or the audio stream ends.
func (s *Session) routeAudio(c *civ.Client, done chan struct{}, sink func([]byte)) {
	for {
		select {
		case <-done:
			return
		case pcm, ok := <-c.AudioFrames():
			if !ok {
				return // audio stream closed (demand off, loss, or shutdown)
			}
			if sink != nil {
				sink(pcm)
			}
		}
	}
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
	var audioTTLTimer *time.Timer
	var audioTTLC <-chan time.Time
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
	stopAudioTTL := func() {
		if audioTTLTimer != nil {
			audioTTLTimer.Stop()
			audioTTLTimer = nil
			audioTTLC = nil
		}
	}
	defer stopAudioTTL()

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
				s.runDemand(d)
				// This was work: restart the idle clock unless the audio
				// demand holds the session open.
				stopIdle()
				if !s.audioDemanded() {
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
				// A failed connect arms the decay: a stale fault must not
				// read as current when nobody retries (R3).
				decayTimer = time.NewTimer(s.opts.DecayTimeout)
				decayC = decayTimer.C
				continue
			}
			s.runDemand(d)
			stopIdle()
			if !s.audioDemanded() {
				idleTimer = time.NewTimer(s.opts.IdleTimeout)
				idleC = idleTimer.C
			}
			s.ensureAudio()

			case ar := <-s.audioCh:
				s.mu.Lock()
				s.audioDemand = ar.on
				s.snap.AudioDemand = ar.on
				s.mu.Unlock()
				s.notify()
				ar.err <- nil
				if ar.on {
					// The audio demand holds the session open; audio_on is
					// heartbeat-refreshed by the consumer and expires via the
					// TTL — a dead consumer never pins the radio (KTD-2).
					stopIdle()
					stopAudioTTL()
					audioTTLTimer = time.NewTimer(s.opts.AudioDemandTTL)
					audioTTLC = audioTTLTimer.C
					if s.live() {
						s.ensureAudio()
					}
				} else {
					stopAudioTTL()
					if s.live() {
						s.mu.Lock()
						c := s.client
						s.mu.Unlock()
						if c != nil {
							c.CloseAudio()
						}
					}
					if s.live() {
						stopIdle()
						idleTimer = time.NewTimer(s.opts.IdleTimeout)
						idleC = idleTimer.C
					}
				}

			case <-audioTTLC:
				// Audio demand expired without a refreshing audio_on: close
				// the audio stream and release the session to the idle clock.
				s.mu.Lock()
				s.audioDemand = false
				s.snap.AudioDemand = false
				c := s.client
				s.mu.Unlock()
				s.notify()
				if c != nil {
					c.CloseAudio()
				}
				if s.live() {
					stopIdle()
					idleTimer = time.NewTimer(s.opts.IdleTimeout)
					idleC = idleTimer.C
				}

		case <-idleC:
			// Live without work: disconnect politely (R1) — the radio's
			// session goes back to manual wfview use.
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
			// job on the notify below; the audio demand SURVIVES (the next
			// TTL refresh reconnects and re-opens the stream) and the full
			// re-login happens on that next demand.
			s.closeClient()
			stopIdle()
			s.setState(StateError, err.Error())
			decayTimer = time.NewTimer(s.opts.DecayTimeout)
			decayC = decayTimer.C
			if s.audioDemanded() {
				s.log.Warn("capture session lost; audio demand retained (reconnects on refresh)", "err", err)
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
			AudioPort:       s.opts.AudioPort,
			Username:        s.opts.Username,
			Password:        s.opts.Password,
			AreYouThere:     s.opts.AreYouThere,
			HandshakeTO:     s.opts.HandshakeTO,
			HandshakeBudget: s.opts.HandshakeBudget,
			LossWatchdog:    s.opts.LossWatchdog,
			PingInterval:    s.opts.PingInterval,
			NoCIVData:       true, // receive-only: no LAN CI-V at all (2026-09 pivot)
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
		go s.drainFrames(c, routeCh)
		return nil
	}
	s.setState(StateError, lastErr.Error())
	return lastErr
}

// runDemand executes one demand against the live client. The demand path
// is serialized (one Run goroutine), so no reply-slot bookkeeping is
// needed anymore — there are no command round-trips left.
func (s *Session) runDemand(d demand) {
	s.mu.Lock()
	c := s.client
	s.mu.Unlock()
	if c == nil {
		d.errCh <- errors.New("radio: no live session")
		return
	}
	d.errCh <- d.fn(c)
}

// drainFrames reads the live client's frame channel until the session
// ends. With LAN CI-V gone there is nothing to route — but the transport
// keeps streaming radio-side frames (transceive broadcasts, ping
// leftovers) and its buffers must stay drained or the client stalls.
func (s *Session) drainFrames(c *civ.Client, done chan struct{}) {
	for {
		select {
		case <-done:
			return
		case _, ok := <-c.Frames():
			if !ok {
				return
			}
		}
	}
}

// closeClient tears the live client down (idempotent) and stops its frame
// drainer. Run is the only reader of client.Lost.
func (s *Session) closeClient() {
	s.mu.Lock()
	c := s.client
	s.client = nil
	routeCh := s.routeCh
	s.routeCh = nil
	audioRouteCh := s.audioRouteCh
	s.audioRouteCh = nil
	s.mu.Unlock()
	if routeCh != nil {
		close(routeCh)
	}
	if audioRouteCh != nil {
		close(audioRouteCh)
	}
	if c != nil {
		c.Close()
	}
}
