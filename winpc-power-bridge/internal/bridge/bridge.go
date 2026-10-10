// Package bridge owns the canonical muehle/power/shack-pc slot: the /meta
// birth certificate, the retained /state snapshot and the /cmd "sleep" action
// with its user-idle guard.
//
// The package has no MQTT client and no Windows dependency. The idle-time
// source and the suspend sequence are injected, so the guard and the command
// handling are unit-tested on any platform.
package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	schema "codeberg.org/kgbvax/stationa/shared/schema"
)

// Role is the canonical role of the slot, published in /meta.
const Role = "pc-power"

// PowerAwake is the only /state.power value the agent can publish: a sleeping
// PC publishes nothing (its /status is "offline").
const PowerAwake = "awake"

// ActionSleep is the one /cmd action.
const ActionSleep = "sleep"

// ErrUserActive is the guard refusal: input in the user session is newer than
// the configured minimum idle time.
var ErrUserActive = errors.New("user active, sleep refused")

// ErrNotConnected is returned by a Publisher while the MQTT client is down. The
// bridge logs it at Debug: the LWT already shows /status offline.
var ErrNotConnected = errors.New("mqtt not connected")

// IdleFunc returns the time since the last keyboard/mouse input in the
// interactive user session.
type IdleFunc func() (time.Duration, error)

// SuspendFunc runs the whole sleep sequence (bus offline, disconnect, suspend,
// reconnect) and returns after resume. A non-nil error means the PC did not
// sleep, or the agent could not reconnect.
type SuspendFunc func() error

// Publisher is the minimal MQTT surface the bridge uses. All bridge topics are
// retained.
type Publisher interface {
	Publish(topic string, retained bool, payload []byte) error
}

// Config is what the bridge needs from the runtime config.
type Config struct {
	Site, Station, Slot string
	Location, Host      string
	DeviceModel         string
	DeviceSerial        string
	Link                string
	// MinUserIdle is the sleep guard threshold.
	MinUserIdle time.Duration
}

// Decide is the sleep guard: nil when the session has been idle for at least
// min, ErrUserActive otherwise.
func Decide(idle, min time.Duration) error {
	if idle < min {
		return ErrUserActive
	}
	return nil
}

// Bridge is the slot model. It is not safe for concurrent use: every call runs
// on the single jobs worker (shared/mqtt RunJobs), never on a paho handler.
type Bridge struct {
	cfg     Config
	pub     Publisher
	idle    IdleFunc
	suspend SuspendFunc
	log     *slog.Logger

	// cmdErr is the outcome of the last /cmd (refusal or suspend failure). It
	// is carried in the next /state publish only, then cleared: a refusal is
	// an event, not a lasting condition. A successful sleep clears it at once.
	cmdErr string
}

// New constructs a Bridge.
func New(cfg Config, pub Publisher, idle IdleFunc, suspend SuspendFunc, log *slog.Logger) *Bridge {
	return &Bridge{cfg: cfg, pub: pub, idle: idle, suspend: suspend, log: log}
}

// State is the /state JSON document.
type State struct {
	TS           string `json:"ts"`
	Power        string `json:"power"`
	UserIdleS    int64  `json:"user_idle_s"`
	DeviceOnline bool   `json:"device_online"`
	Error        string `json:"error,omitempty"`
}

// PublishState publishes the retained /state snapshot with a fresh
// user_idle_s. Called on connect, every state interval and after a /cmd.
func (b *Bridge) PublishState() {
	st := State{
		TS:           time.Now().UTC().Format(time.RFC3339),
		Power:        PowerAwake,
		DeviceOnline: true,
	}
	idle, err := b.idle()
	if err != nil {
		st.Error = "user idle time unavailable: " + err.Error()
	} else {
		st.UserIdleS = int64(idle / time.Second)
	}
	if b.cmdErr != "" {
		st.Error = b.cmdErr
	}
	data, err := json.Marshal(st)
	if err != nil {
		b.log.Error("marshal state", "err", err)
		return
	}
	if b.publish(b.topic("state"), data) {
		// The command outcome reached the bus once; the next refresh clears it.
		b.cmdErr = ""
	}
}

// HandleCommand handles one /cmd message. retained is the MQTT retain flag of
// the delivery: a retained /cmd is a broker replay of an old message (on
// subscribe after start or resume), never a live request, and is dropped — a
// stale retained "sleep" must never put the PC to sleep at boot or resume.
func (b *Bridge) HandleCommand(retained bool, payload []byte) {
	if len(payload) == 0 {
		return // a retained-clear (empty payload); nothing to do
	}
	if retained {
		b.log.Warn("cmd: dropping retained /cmd replay (send sleep non-retained)", "payload", string(payload))
		return
	}
	var c schema.CmdPayload
	if err := json.Unmarshal(payload, &c); err != nil {
		b.log.Error("cmd: malformed JSON dropped", "err", err)
		return
	}
	switch c.Action {
	case ActionSleep:
		b.handleSleep()
	default:
		b.log.Warn("cmd: unknown action ignored", "action", c.Action)
	}
}

func (b *Bridge) handleSleep() {
	idle, err := b.idle()
	if err != nil {
		b.cmdErr = "user idle time unavailable, sleep refused"
		b.log.Warn("sleep refused: user idle time unavailable", "err", err)
		b.PublishState()
		return
	}
	if err := Decide(idle, b.cfg.MinUserIdle); err != nil {
		b.cmdErr = err.Error()
		b.log.Warn("sleep refused: user active",
			"user_idle", idle.Round(time.Second), "min_user_idle", b.cfg.MinUserIdle)
		b.PublishState()
		return
	}

	b.log.Info("sleep: going offline and suspending", "user_idle", idle.Round(time.Second))
	start := time.Now()
	if err := b.suspend(); err != nil {
		b.cmdErr = "sleep failed: " + err.Error()
		b.log.Error("sleep failed", "err", err)
	} else {
		b.cmdErr = ""
		b.log.Info("resumed from sleep", "slept", time.Since(start).Round(time.Second))
	}
	b.PublishState()
}

// PublishMeta publishes the retained /meta birth certificate.
func (b *Bridge) PublishMeta() {
	data, err := json.Marshal(b.meta())
	if err != nil {
		b.log.Error("marshal meta", "err", err)
		return
	}
	b.publish(b.topic("meta"), data)
}

func (b *Bridge) publish(topic string, data []byte) bool {
	if err := b.pub.Publish(topic, true, data); err != nil {
		if errors.Is(err, ErrNotConnected) {
			b.log.Debug("publish skipped: not connected", "topic", topic)
		} else {
			b.log.Error("publish failed", "topic", topic, "err", err)
		}
		return false
	}
	return true
}

func (b *Bridge) topic(plane string) string {
	return schema.SiblingTopic(b.cfg.Site, b.cfg.Station, b.cfg.Slot, plane)
}

// ------------------------------------------------------------------
// /meta
// ------------------------------------------------------------------

type metaPayload struct {
	Schema       string           `json:"schema"`
	Role         string           `json:"role"`
	Device       metaDevice       `json:"device"`
	Link         string           `json:"link,omitempty"`
	Location     string           `json:"location,omitempty"`
	Host         string           `json:"host,omitempty"`
	Capabilities metaCapabilities `json:"capabilities"`
	Expose       metaExpose       `json:"expose"`
}

type metaDevice struct {
	Model  string `json:"model"`
	Serial string `json:"serial"`
}

type metaCapabilities struct {
	Actions            []string `json:"actions"`
	MinUserIdleMinutes int      `json:"min_user_idle_minutes"`
}

type metaExpose struct {
	Device  exposeDevice   `json:"device"`
	Fields  []exposeField  `json:"fields"`
	Actions []exposeAction `json:"actions"`
}

type exposeDevice struct {
	Name         string `json:"name"`
	Model        string `json:"model,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
}

type exposeField struct {
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Unit       string   `json:"unit,omitempty"`
	Class      string   `json:"class,omitempty"`
	StateClass string   `json:"state_class,omitempty"`
	Options    []string `json:"options,omitempty"`
}

type exposeAction struct {
	Key     string        `json:"key"`
	Name    string        `json:"name"`
	Command exposeCommand `json:"command"`
}

type exposeCommand struct {
	Action string `json:"action"`
}

func (b *Bridge) meta() metaPayload {
	return metaPayload{
		Schema:   "1.0",
		Role:     Role,
		Device:   metaDevice{Model: b.cfg.DeviceModel, Serial: b.cfg.DeviceSerial},
		Link:     b.cfg.Link,
		Location: b.cfg.Location,
		Host:     b.cfg.Host,
		Capabilities: metaCapabilities{
			Actions:            []string{ActionSleep},
			MinUserIdleMinutes: int(b.cfg.MinUserIdle / time.Minute),
		},
		Expose: metaExpose{
			Device: exposeDevice{
				Name:  fmt.Sprintf("Shack PC (%s)", b.cfg.Host),
				Model: b.cfg.DeviceModel,
			},
			Fields: []exposeField{
				{Key: "power", Name: "Power", Type: "enum", Options: []string{PowerAwake}},
				{Key: "user_idle_s", Name: "User idle", Type: "number", Unit: "s", Class: "duration", StateClass: "measurement"},
			},
			Actions: []exposeAction{
				{Key: ActionSleep, Name: "Sleep", Command: exposeCommand{Action: ActionSleep}},
			},
		},
	}
}
