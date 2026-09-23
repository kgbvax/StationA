package radio

import (
	"context"
	"log/slog"
	"time"
)

// Config carries the bridge config onto the session manager.
type Config struct {
	Host           string
	Username       string
	Password       string
	IdleTimeout    time.Duration
	MaxAttempts    int
	AttemptSpacing time.Duration
	DecayTimeout   time.Duration // 0 = the idle timeout
	HandshakeTO    time.Duration // per-handshake-step reply wait

	// Transport passthroughs (production leaves them zero for the civ
	// defaults; tests point the session at a fake radio).
	ControlPort     int
	CIVPort         int
	AudioPort       int
	AreYouThere     time.Duration
	HandshakeBudget time.Duration
	LossWatchdog    time.Duration
	PingInterval    time.Duration

	// AudioDemandTTL bounds an unrefreshed audio_on (0 = 60 s); AudioSink
	// receives the demodulated audio PCM chunks while the audio stream is
	// open (the main wiring publishes them to the preview host).
	AudioDemandTTL time.Duration
	AudioSink      func([]byte)

	Logger *slog.Logger
}

// Manager is the radio-side surface main.go and the bridge slot (U5) drive:
// Run owns the lifecycle goroutine, SetAudioDemand is the audio_on/audio_off
// demand (the only session hold — receive-only posture, 2026-09), and
// Snapshot/Notify feed the /state assembly. There is no command path: LAN
// CI-V control was removed with the 2026-09 pivot.
type Manager struct {
	sess *Session
	log  *slog.Logger
}

// NewManager builds the manager over a new session.
func NewManager(cfg Config) *Manager {
	if cfg.HandshakeTO <= 0 {
		cfg.HandshakeTO = 2 * time.Second
	}
	sess := NewSession(SessionOptions{
		Host:            cfg.Host,
		Username:        cfg.Username,
		Password:        cfg.Password,
		IdleTimeout:     cfg.IdleTimeout,
		MaxAttempts:     cfg.MaxAttempts,
		AttemptSpacing:  cfg.AttemptSpacing,
		DecayTimeout:    cfg.DecayTimeout,
		HandshakeTO:     cfg.HandshakeTO,
		ControlPort:     cfg.ControlPort,
		CIVPort:         cfg.CIVPort,
		AudioPort:       cfg.AudioPort,
		AreYouThere:     cfg.AreYouThere,
		HandshakeBudget: cfg.HandshakeBudget,
		LossWatchdog:    cfg.LossWatchdog,
		PingInterval:    cfg.PingInterval,
		AudioDemandTTL:  cfg.AudioDemandTTL,
		AudioSink:       cfg.AudioSink,
		Logger:          cfg.Logger,
	})
	return &Manager{sess: sess, log: cfg.Logger.With("component", "radio-manager")}
}

// Session exposes the state machine for tests.
func (m *Manager) Session() *Session { return m.sess }

// Run drives the session state machine until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error { return m.sess.Run(ctx) }

// Snapshot returns the lifecycle truth for the /state assembly.
func (m *Manager) Snapshot() Snapshot { return m.sess.Snapshot() }

// Notify receives a nudge on every state change.
func (m *Manager) Notify() <-chan struct{} { return m.sess.Notify() }

// SetAudioDemand is the audio_on/audio_off demand: on = connect (when idle)
// and open the audio receive stream, TTL-bounded so a dead consumer never
// pins the radio session (KTD-2). Receive-only — the only session hold.
func (m *Manager) SetAudioDemand(ctx context.Context, on bool) error {
	return m.sess.SetAudioDemand(ctx, on)
}
