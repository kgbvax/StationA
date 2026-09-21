package radio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"icom9700-radio-bridge/internal/civ"
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
	CmdWait        time.Duration // per-command reply wait

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
// Run owns the lifecycle goroutine, Execute applies one /cmd payload with an
// on-demand connect, SetHold is the arm/disarm demand, Snapshot/Notify feed
// the /state assembly.
type Manager struct {
	sess *Session
	cfg  Config
	log  *slog.Logger

	mu     sync.Mutex // serializes payload dispatch (frame round-trips)
	closed bool
}

// NewManager builds the manager over a new session.
func NewManager(cfg Config) *Manager {
	if cfg.CmdWait <= 0 {
		cfg.CmdWait = 2 * time.Second
	}
	sess := NewSession(SessionOptions{
		Host:            cfg.Host,
		Username:        cfg.Username,
		Password:        cfg.Password,
		IdleTimeout:     cfg.IdleTimeout,
		MaxAttempts:     cfg.MaxAttempts,
		AttemptSpacing:  cfg.AttemptSpacing,
		DecayTimeout:    cfg.DecayTimeout,
		HandshakeTO:     cfg.CmdWait,
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
	return &Manager{sess: sess, cfg: cfg, log: cfg.Logger.With("component", "radio-manager")}
}

// Session exposes the state machine for the hooks (OnLive/OnLoss) and
// tests.
func (m *Manager) Session() *Session { return m.sess }

// Run drives the session state machine until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) error { return m.sess.Run(ctx) }

// Snapshot returns the lifecycle truth for the /state assembly.
func (m *Manager) Snapshot() Snapshot { return m.sess.Snapshot() }

// Notify receives a nudge on every state change.
func (m *Manager) Notify() <-chan struct{} { return m.sess.Notify() }

// Transceives receives parsed inbound transceive events (freq/mode/tx).
func (m *Manager) Transceives() <-chan civ.Transceive { return m.sess.Transceives() }

// OnLive registers the post-reconnect hook (safety core: PTT-off first).
func (m *Manager) OnLive(fn func(ctx context.Context, c *civ.Client) error) {
	m.sess.OnLive(fn)
}

// OnLoss registers the session-loss hook (safety core: disarm + safety
// PTT-off delivery, R2/R12).
func (m *Manager) OnLoss(fn func(err error)) { m.sess.OnLoss(fn) }

// SetHold is the arm/disarm demand (R11): arming connects on demand and
// holds the session open; the hold drops on session loss (fail-disarmed).
func (m *Manager) SetHold(ctx context.Context, on bool) error {
	return m.sess.SetHold(ctx, on)
}

// SetAudioDemand is the audio_on/audio_off demand: on = connect (when idle)
// and open the audio receive stream, TTL-bounded so a dead consumer never
// pins the radio session (KTD-2). Receive-only — no arm gate applies.
func (m *Manager) SetAudioDemand(ctx context.Context, on bool) error {
	return m.sess.SetAudioDemand(ctx, on)
}

// busCmd is the stationa value-key payload shape: {"action": ..., "value":
// ...[, "vfo": ...]}. The value stays raw — string and number forms both
// arrive depending on the publisher.
type busCmd struct {
	Action string          `json:"action"`
	Value  json.RawMessage `json:"value"`
	Vfo    string          `json:"vfo"`
}

func (b *busCmd) stringValue() (string, error) {
	var s string
	if err := json.Unmarshal(b.Value, &s); err == nil {
		return s, nil
	}
	// Numeric forms arrive from some publishers; normalize to string.
	var n json.Number
	if err := json.Unmarshal(b.Value, &n); err == nil {
		return n.String(), nil
	}
	if b.Value == nil {
		return "", nil
	}
	return "", fmt.Errorf("radio: unsupported value form %s", string(b.Value))
}

func (b *busCmd) boolValue() (bool, error) {
	s, err := b.stringValue()
	if err != nil {
		return false, err
	}
	switch s {
	case "on", "true", "1":
		return true, nil
	case "off", "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("radio: unsupported bool value %q", s)
}

// Execute applies one /cmd payload to the radio: parse, validate, connect
// on demand, dispatch over the CI-V stream, wait for the radio's ack.
// PTT is deliberately NOT dispatched here — it is the safety core's gate
// (U6 lands `arm ∧ live` around it); arm/disarm arrive as SetHold.
func (m *Manager) Execute(ctx context.Context, payload []byte) error {
	var cmd busCmd
	if err := json.Unmarshal(payload, &cmd); err != nil {
		return fmt.Errorf("radio: bad cmd payload: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch cmd.Action {
	case "set_freq":
		vfo := cmd.Vfo
		if vfo == "" {
			return errors.New("radio: set_freq requires a vfo")
		}
		s, err := cmd.stringValue()
		if err != nil {
			return err
		}
		hz, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return fmt.Errorf("radio: set_freq value %q is not a Hz integer", s)
		}
		if err := civ.ValidateFreq(vfo, hz); err != nil {
			return err
		}
		return m.sess.Demand(ctx, func(c *civ.Client) error {
			sel, err := civ.CmdSelectVFO(vfo)
			if err != nil {
				return err
			}
			if _, err := m.sess.RoundTrip(ctx, c, sel, m.cfg.CmdWait); err != nil {
				return err
			}
			set, err := civ.CmdSetFreq(hz)
			if err != nil {
				return err
			}
			_, err = m.sess.RoundTrip(ctx, c, set, m.cfg.CmdWait)
			return err
		})

	case "set_mode":
		if cmd.Vfo == "" {
			return errors.New("radio: set_mode requires a vfo")
		}
		mode, err := cmd.stringValue()
		if err != nil {
			return err
		}
		set, err := civ.CmdSetMode(mode)
		if err != nil {
			return err
		}
		return m.sess.Demand(ctx, func(c *civ.Client) error {
			sel, err := civ.CmdSelectVFO(cmd.Vfo)
			if err != nil {
				return err
			}
			if _, err := m.sess.RoundTrip(ctx, c, sel, m.cfg.CmdWait); err != nil {
				return err
			}
			_, err = m.sess.RoundTrip(ctx, c, set, m.cfg.CmdWait)
			return err
		})

	case "set_data":
		if cmd.Vfo == "" {
			return errors.New("radio: set_data requires a vfo")
		}
		on, err := cmd.boolValue()
		if err != nil {
			return err
		}
		return m.sess.Demand(ctx, func(c *civ.Client) error {
			sel, err := civ.CmdSelectVFO(cmd.Vfo)
			if err != nil {
				return err
			}
			if _, err := m.sess.RoundTrip(ctx, c, sel, m.cfg.CmdWait); err != nil {
				return err
			}
			_, err = m.sess.RoundTrip(ctx, c, civ.CmdSetDataMode(on), m.cfg.CmdWait)
			return err
		})

	case "sat_mode":
		on, err := cmd.boolValue()
		if err != nil {
			return err
		}
		return m.sess.Demand(ctx, func(c *civ.Client) error {
			_, err := m.sess.RoundTrip(ctx, c, civ.CmdSatelliteMode(on), m.cfg.CmdWait)
			return err
		})

	case "set_power":
		s, err := cmd.stringValue()
		if err != nil {
			return err
		}
		lvl, err := strconv.ParseUint(s, 10, 8)
		if err != nil {
			return fmt.Errorf("radio: set_power value %q is not a 0-255 level", s)
		}
		return m.sess.Demand(ctx, func(c *civ.Client) error {
			_, err := m.sess.RoundTrip(ctx, c, civ.CmdSetRFPower(byte(lvl)), m.cfg.CmdWait)
			return err
		})

	case "arm":
		return m.SetHold(ctx, true)
	case "disarm":
		return m.SetHold(ctx, false)

	case "audio_on":
		return m.SetAudioDemand(ctx, true)
	case "audio_off":
		return m.SetAudioDemand(ctx, false)

	case "ptt":
		// The safety core (U6) owns the arm gate; until it lands the
		// rejection is explicit rather than ungated (fail-closed).
		return errors.New("radio: ptt rejected: not armed")

	default:
		return fmt.Errorf("radio: unknown cmd action %q", cmd.Action)
	}
}
