// Package bridge owns the canonical rotator state model and translates decoded
// WRC status (internal/rotor.State) into MQTT publishes following the station
// integration model (slot muehle/hf/rotator). It also dispatches /cmd intent
// (set_az, stop, fwd, rev) to the rotator via a Commander.
package bridge

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"

	schema "codeberg.org/kgbvax/stationa/shared/schema"

	"wrc-rotator-bridge/internal/rotor"
)

// Commander is the rotator control surface the bridge drives from /cmd. The
// rotor device (internal/rotor.Device) implements it; tests use a fake.
type Commander = rotor.Commander

// OverrideCommander is implemented by a Commander that accepts a set_az
// carrying an `override` tag (the antenna-mode lock gate: a console nudge in
// maintenance). A plain device does not implement it; the tag is then ignored.
type OverrideCommander interface {
	SetAzOverride(az float64, override string) error
}

// Config is the subset of config the bridge needs.
type Config struct {
	Site     string // e.g. "muehle"
	Station  string // e.g. "hf"
	Slot     string // e.g. "rotator"
	Location string // physical location label, e.g. "bauwagen"
	Host     string // compute node, published in /meta

	// Device identity for /meta.
	DeviceModel string
	DeviceLink  string

	// Commander executes /cmd intent. May be nil (read-only deploy).
	Commander Commander
}

// Logger is the minimal logging surface the bridge uses.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Bridge owns the canonical rotator state and publishes it to MQTT.
type Bridge struct {
	cfg Config
	pub Publisher
	log Logger

	mu       sync.RWMutex
	state    rotor.State
	last     rotor.State // last published snapshot (for change dedup)
	lock     string      // antenna-mode lock label ("" = unlocked)
	lastLock string      // lock label in the last published snapshot
}

// metaPayload is the JSON shape published to <slot>/meta (retained birth cert).
type metaPayload struct {
	Schema       string           `json:"schema"`
	Role         string           `json:"role"`
	Device       metaDevice       `json:"device"`
	Link         string           `json:"link,omitempty"`
	Location     string           `json:"location,omitempty"`
	Host         string           `json:"host,omitempty"`
	Capabilities metaCapabilities `json:"capabilities"`
	Expose       *metaExpose      `json:"expose,omitempty"`
}

type metaDevice struct {
	Model string `json:"model"`
}

// metaCapabilities is the §7.1 rotator capability contract: axes [az] for the
// HF Yaesu G-450DC (azimuth-only).
type metaCapabilities struct {
	Axes []string `json:"axes"`
}

// metaExpose is the consumer-neutral field surface (model §3.1, Appendix C).
// It carries NO consumer vocabulary; hadiscovery renders HA entities from it.
type metaExpose struct {
	Device  metaExposeDevice   `json:"device"`
	Fields  []metaExposeField  `json:"fields"`
	Actions []metaExposeAction `json:"actions,omitempty"`
}

type metaExposeDevice struct {
	Name         string `json:"name,omitempty"`
	Model        string `json:"model,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Area         string `json:"area,omitempty"`
}

// metaExposeField describes one /state field. type is number|enum|boolean|string.
// writable + a command descriptor make it a setpoint; the consumer renders the
// command into its own /cmd syntax.
type metaExposeField struct {
	Key        string       `json:"key"`
	Name       string       `json:"name"`
	Type       string       `json:"type"`
	Unit       string       `json:"unit,omitempty"`
	Class      string       `json:"class,omitempty"`
	StateClass string       `json:"state_class,omitempty"`
	Min        *float64     `json:"min,omitempty"`
	Max        *float64     `json:"max,omitempty"`
	Step       *float64     `json:"step,omitempty"`
	Writable   bool         `json:"writable,omitempty"`
	Command    *metaCommand `json:"command,omitempty"`
}

type metaExposeAction struct {
	Key     string       `json:"key"`
	Name    string       `json:"name"`
	Command *metaCommand `json:"command"`
}

// metaCommand describes how a write is encoded on /cmd (Appendix C).
type metaCommand struct {
	Action    string `json:"action,omitempty"`
	ValueKey  string `json:"value_key,omitempty"`
	ValueType string `json:"value_type,omitempty"`
}

// cmdPayload is the /cmd JSON the bridge accepts. Az carries the set_az value;
// stop/fwd/rev carry only the action.
type cmdPayload struct {
	Action string   `json:"action"`
	Az     *float64 `json:"az,omitempty"`
	// Override tags a set_az as an explicit operator nudge allowed during an
	// antenna-mode maintenance lock ("maintenance"); see internal/lock.
	Override string `json:"override,omitempty"`
}

// New constructs a Bridge.
func New(cfg Config, pub Publisher, log Logger) *Bridge {
	return &Bridge{cfg: cfg, pub: pub, log: log}
}

// PublishMeta publishes the retained birth certificate via the bridge's own
// publisher (used in tests and anywhere the publisher is already wired).
func (b *Bridge) PublishMeta() {
	b.publishMetaWith(b.pub)
}

// PublishMetaVia publishes the retained birth certificate using the given paho
// client. Used from the paho OnConnect callback, where the bridge's
// PahoPublisher.Client has not yet been assigned — OnConnect fires during the
// initial Connect(), before connectMQTT returns and wires pub.Client.
func (b *Bridge) PublishMetaVia(c pahomqtt.Client) {
	b.publishMetaWith(&PahoPublisher{Client: c})
}

// publishMetaWith builds and publishes the retained /meta birth certificate
// through pub. Called on (re)connect so the certificate is always fresh.
func (b *Bridge) publishMetaWith(pub Publisher) {
	b.mu.RLock()
	loc := b.cfg.Location
	host := b.cfg.Host
	b.mu.RUnlock()

	azMin, azMax, azStep := 0.0, 450.0, 1.0

	p := metaPayload{
		Schema: "1.0",
		Role:   "rotator",
		Device: metaDevice{
			Model: b.cfg.DeviceModel,
		},
		Link:     b.cfg.DeviceLink,
		Location: loc,
		Host:     host,
		Capabilities: metaCapabilities{
			Axes: []string{"az"},
		},
		Expose: &metaExpose{
			Device: metaExposeDevice{
				Name:         "HF Rotator",
				Model:        b.cfg.DeviceModel,
				Manufacturer: "Yaesu",
				// No Area: hadiscovery supplies the deployment-wide default HA area
				// for slots that do not name one (integration model §9 — the bridge
				// carries no HA/area knowledge). `loc` is still published above as
				// the bus-identity location (model §3).
			},
			Fields: []metaExposeField{
				{Key: "az", Name: "Azimuth", Type: "number", Unit: "°", Class: "azimuth",
					StateClass: "measurement", Writable: true, Min: &azMin, Max: &azMax, Step: &azStep,
					Command: &metaCommand{Action: "set_az", ValueKey: "az", ValueType: "float"}},
				{Key: "target_az", Name: "Target Azimuth", Type: "number", Unit: "°", Class: "azimuth",
					StateClass: "measurement"},
				{Key: "moving", Name: "Moving", Type: "boolean"},
				{Key: "rotor_state", Name: "Rotor State", Type: "string"},
				{Key: "device_online", Name: "Device Online", Type: "boolean"},
				{Key: "lock", Name: "Antenna Mode Lock", Type: "string"},
			},
			Actions: []metaExposeAction{
				{Key: "stop", Name: "Stop", Command: &metaCommand{Action: "stop"}},
				{Key: "fwd", Name: "Rotate CW", Command: &metaCommand{Action: "fwd"}},
				{Key: "rev", Name: "Rotate CCW", Command: &metaCommand{Action: "rev"}},
			},
		},
	}
	data, err := json.Marshal(p)
	if err != nil {
		b.log.Errorf("marshal meta: %v", err)
		return
	}
	_ = pub.Publish(b.metaTopic(), true, data)
}

// HandleTelemetry updates the cached state and publishes the retained /state
// snapshot, but only when a published field changed since the last snapshot.
// The WRC streams status frequently; dedup keeps the bus quiet while the
// rotator sits still (model §8 — state is the live snapshot, not a firehose).
func (b *Bridge) HandleTelemetry(st rotor.State) {
	b.mu.Lock()
	b.state = st
	changed := stateChanged(b.last, st) || b.lock != b.lastLock
	lock := b.lock
	if changed {
		b.last, b.lastLock = st, lock
	}
	b.mu.Unlock()
	if changed {
		b.publishState(st, lock)
	}
}

// SetLock records the antenna-mode lock label ("" = unlocked) and publishes a
// snapshot when it changed.
func (b *Bridge) SetLock(label string) {
	b.mu.Lock()
	b.lock = label
	changed := b.lock != b.lastLock
	snap := b.state
	if changed {
		b.last, b.lastLock = snap, label
	}
	b.mu.Unlock()
	if changed {
		b.publishState(snap, label)
	}
}

// SetDeviceOnline updates the device_online/error fields and publishes a
// snapshot. Called when the WebSocket is lost or regained; /status itself (the
// bridge LWT) is unaffected — the bridge is still up.
func (b *Bridge) SetDeviceOnline(online bool, errMsg string) {
	b.mu.Lock()
	b.state.DeviceOnline = online
	b.state.Error = errMsg
	snap := b.state
	lock := b.lock
	changed := stateChanged(b.last, snap) || lock != b.lastLock
	if changed {
		b.last, b.lastLock = snap, lock
	}
	b.mu.Unlock()
	if changed {
		b.publishState(snap, lock)
	}
}

// stateChanged reports whether two snapshots differ in any published field.
// Comparing the float64 azimuths with == is fine: we keep the raw WRC value
// unchanged across identical reports, so a stationary rotator produces no
// churn. When the rotator moves the azimuth changes every frame, as expected.
func stateChanged(a, b rotor.State) bool {
	return a.Az != b.Az ||
		a.TargetAz != b.TargetAz ||
		a.Moving != b.Moving ||
		a.RotorState != b.RotorState ||
		a.DeviceOnline != b.DeviceOnline ||
		a.Error != b.Error
}

// HandleCommand parses a /cmd JSON payload and dispatches it to the Commander.
// Unknown actions are logged and ignored.
func (b *Bridge) HandleCommand(payload []byte) {
	var c cmdPayload
	if err := json.Unmarshal(payload, &c); err != nil {
		b.log.Warnf("cmd: parse: %v", err)
		return
	}
	if b.cfg.Commander == nil {
		b.log.Warnf("cmd: no commander configured")
		return
	}
	switch c.Action {
	case "set_az":
		if c.Az == nil {
			b.log.Warnf("cmd set_az: missing az")
			return
		}
		var err error
		if oc, ok := b.cfg.Commander.(OverrideCommander); ok && c.Override != "" {
			err = oc.SetAzOverride(*c.Az, c.Override)
		} else {
			err = b.cfg.Commander.SetAz(*c.Az)
		}
		b.cmdErr("set_az", err)
	case "stop":
		b.cmdErr("stop", b.cfg.Commander.Stop())
	case "fwd":
		b.cmdErr("fwd", b.cfg.Commander.Jog("fwd"))
	case "rev":
		b.cmdErr("rev", b.cfg.Commander.Jog("rev"))
	default:
		b.log.Warnf("cmd: unknown action %q", c.Action)
	}
}

// cmdErr logs a failed command. A lock refusal is not logged here: the lock
// gate already logs one Warn per refusal streak.
func (b *Bridge) cmdErr(action string, err error) {
	if err != nil && !errors.Is(err, rotor.ErrLocked) {
		b.log.Warnf("cmd %s: %v", action, err)
	}
}

// publishState marshals the snapshot with an RFC3339 ts and publishes it
// retained to /state.
func (b *Bridge) publishState(st rotor.State, lock string) {
	p := stateEnvelope{
		TS:    time.Now().UTC().Format(time.RFC3339),
		State: st,
		Lock:  lock,
	}
	data, err := json.Marshal(p)
	if err != nil {
		b.log.Errorf("marshal state: %v", err)
		return
	}
	_ = b.pub.Publish(b.stateTopic(), true, data)
}

// stateEnvelope is the published /state shape: an RFC3339 ts plus the canonical
// rotator state.
type stateEnvelope struct {
	TS string `json:"ts"`
	rotor.State
	// Lock is the antenna-mode lock (storm|maintenance), omitted when unlocked.
	Lock string `json:"lock,omitempty"`
}

// ------------------------------------------------------------------
// Topic helpers
// ------------------------------------------------------------------

func (b *Bridge) metaTopic() string {
	return schema.MetaTopic(b.cfg.Site, b.cfg.Station, b.cfg.Slot)
}
func (b *Bridge) stateTopic() string {
	return schema.StateTopic(b.cfg.Site, b.cfg.Station, b.cfg.Slot)
}
func (b *Bridge) cmdTopic() string {
	return schema.CmdTopic(b.cfg.Site, b.cfg.Station, b.cfg.Slot)
}

// CmdTopic returns the /cmd topic (exported for main to subscribe).
func (b *Bridge) CmdTopic() string { return b.cmdTopic() }
