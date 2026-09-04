// Package bridge owns the canonical `power` slot state model and the
// /meta /state /cmd surfaces for one Shelly plug. shelly-power-bridge runs one
// SlotBridge per configured [[slot]], each with its own paho client + LWT, so a
// process death takes every fronted slot offline with no stale-online gap.
package bridge

import (
	"encoding/json"
	"sync"
	"time"

	schema "codeberg.org/kgbvax/stationa/shared/schema"
)

// Commander is the relay-control surface the bridge drives from /cmd. The
// shelly command path implements it; tests use a fake.
type Commander interface {
	// SetPower asserts (on=true) or releases (on=false) the Shelly's relay 0.
	// Fire-and-observe: the resulting native status announce is the
	// confirmation, not the return of this call.
	SetPower(on bool) error
}

// Config is the subset of config one SlotBridge needs.
type Config struct {
	Site            string // e.g. "muehle"
	Station         string // e.g. "power"
	Slot            string // e.g. "master" | "psu-13v8"
	Location        string // physical location label, published in /meta
	Host            string // compute node, published in /meta
	DiscoveryPrefix string // hadiscovery (model §9)
	AvailTopic      string // <site>/<station>/<slot>/status (LWT topic)
	DeviceModel     string
	DeviceSerial    string
	FailSafe        string   // "on" | "off" — published in capabilities
	Feeds           []string // downstream slots this supply powers (psu-13v8 only)

	// Commander executes /cmd intent. May be nil (read-only deploy).
	Commander Commander
}

// Logger is the minimal logging surface the bridge uses.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

// powerState is the canonical `power` slot state (integration model §7.0).
type powerState struct {
	Power        string `json:"power"` // on | off (actual, read back from the Shelly)
	DeviceOnline bool   `json:"device_online"`
	Error        string `json:"error,omitempty"`
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
	Model  string `json:"model"`
	Serial string `json:"serial"`
}

type metaCapabilities struct {
	FailSafe string   `json:"fail_safe"` // "on" | "off"
	Feeds    []string `json:"feeds,omitempty"`
}

// metaExpose is the consumer-neutral field surface (model §3.1, Appendix C).
type metaExpose struct {
	Device metaExposeDevice  `json:"device"`
	Fields []metaExposeField `json:"fields"`
}

type metaExposeDevice struct {
	Name         string `json:"name,omitempty"`
	Model        string `json:"model,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
}

type metaExposeField struct {
	Key      string       `json:"key"`
	Name     string       `json:"name"`
	Type     string       `json:"type"`
	Options  []string     `json:"options,omitempty"`
	Writable bool         `json:"writable,omitempty"`
	Command  *metaCommand `json:"command,omitempty"`
}

type metaCommand struct {
	Action    string `json:"action,omitempty"`
	ValueKey  string `json:"value_key,omitempty"`
	ValueType string `json:"value_type,omitempty"`
}

// SlotBridge owns one canonical power slot and publishes it to MQTT.
type SlotBridge struct {
	cfg Config
	pub Publisher
	log Logger

	mu     sync.RWMutex
	state  powerState
	last   powerState // last published snapshot (for change dedup)
	online bool       // bridge (this slot's client) connected; gates state publishing
}

// New constructs a SlotBridge.
func New(cfg Config, pub Publisher, log Logger) *SlotBridge {
	return &SlotBridge{cfg: cfg, pub: pub, log: log}
}

// SetConnected marks whether this slot's paho client is connected. While
// disconnected, telemetry is not published (the LWT already shows /status
// offline). State from the Shelly arriving between connects is held and
// published on the next connect via PublishMeta+replay.
func (b *SlotBridge) SetConnected(connected bool) {
	b.mu.Lock()
	b.online = connected
	b.mu.Unlock()
}

// PublishMeta publishes the retained birth certificate. Called on (re)connect.
func (b *SlotBridge) PublishMeta() {
	p := metaPayload{
		Schema: "1.0",
		Role:   "power",
		Device: metaDevice{
			Model:  b.cfg.DeviceModel,
			Serial: b.cfg.DeviceSerial,
		},
		Link:     "wifi",
		Location: b.cfg.Location,
		Host:     b.cfg.Host,
		Capabilities: metaCapabilities{
			FailSafe: b.cfg.FailSafe,
			Feeds:    b.cfg.Feeds,
		},
		Expose: &metaExpose{
			Device: metaExposeDevice{
				Name:         b.cfg.DeviceModel,
				Model:        b.cfg.DeviceModel,
				Manufacturer: "Shelly",
			},
			Fields: []metaExposeField{
				{
					Key:      "power",
					Name:     "Power",
					Type:     "enum",
					Options:  []string{"on", "off"},
					Writable: true,
					Command:  &metaCommand{Action: "set_power", ValueKey: "value", ValueType: "string"},
				},
			},
		},
	}
	data, err := json.Marshal(p)
	if err != nil {
		b.log.Warnf("marshal meta: %v", err)
		return
	}
	_ = b.pub.Publish(b.metaTopic(), true, data)
}

// HandleTelemetry applies a Shelly native power observation ("on"|"off") and
// publishes the retained /state snapshot if it changed. device_online is set
// true: a native status announce means the Shelly is reachable. The native
// status announce IS the power observation, so this is the single entry point.
func (b *SlotBridge) HandleTelemetry(power string) {
	if power != "on" && power != "off" {
		return
	}
	b.mu.Lock()
	st := b.state
	wasOffline := !st.DeviceOnline
	prevErr := st.Error
	st.Power = power
	st.DeviceOnline = true
	st.Error = ""
	snap, changed := b.snapshotLocked(st)
	b.mu.Unlock()
	if changed {
		// Degraded → recovered transition (clears a MarkDeviceOffline error):
		// Warn per the logging convention §3 so the episode is greppable at
		// warning level alongside the offline transition. First-ever telemetry
		// (no prior error) is just normal operation → Info.
		if wasOffline && prevErr != "" {
			b.log.Warnf("shelly reachable again: device_online=true (cleared: %s)", prevErr)
		}
		b.log.Infof("state published: power=%s device_online=true", st.Power)
		b.publishState(snap)
	}
}

// MarkDeviceOffline sets the Shelly unreachable (no native announce within the
// staleness window) and publishes a snapshot. Called by the staleness watcher.
func (b *SlotBridge) MarkDeviceOffline(reason string) {
	b.mu.Lock()
	st := b.state
	st.DeviceOnline = false
	st.Error = reason
	snap, changed := b.snapshotLocked(st)
	b.mu.Unlock()
	if changed {
		// device_online=false is the degraded transition — Warn per the logging
		// convention §3 (change-dedup keeps repeat watcher ticks silent).
		b.log.Warnf("device offline: %s", reason)
		b.publishState(snap)
	}
}

// HandleCommand parses a /cmd JSON payload and dispatches it to the Commander.
// /cmd is retained (self-healing steady-state, model §8); on reconnect the
// broker replays the last command and the bridge re-applies it.
func (b *SlotBridge) HandleCommand(payload []byte) {
	var c schema.CmdPayload
	if err := json.Unmarshal(payload, &c); err != nil {
		b.log.Warnf("cmd: parse: %v", err)
		return
	}
	if b.cfg.Commander == nil {
		b.log.Warnf("cmd: no commander configured")
		return
	}
	switch c.Action {
	case "set_power":
		var on bool
		switch c.Value {
		case "on":
			on = true
		case "off":
			on = false
		default:
			b.log.Warnf("cmd set_power: unknown value %q (want on|off)", c.Value)
			return
		}
		if err := b.cfg.Commander.SetPower(on); err != nil {
			b.log.Warnf("cmd set_power: %v", err)
		}
	default:
		b.log.Warnf("cmd: unknown action %q", c.Action)
	}
}

// snapshotLocked stores the candidate state, dedups against last, and returns
// the snapshot plus whether it changed. Caller holds mu.
func (b *SlotBridge) snapshotLocked(st powerState) (powerState, bool) {
	if !b.online {
		// Not connected: hold state, do not publish (LWT covers liveness).
		b.state = st
		return st, false
	}
	changed := st != b.last
	if changed {
		b.state = st
		b.last = st
	}
	return st, changed
}

// publishState marshals the snapshot with an RFC3339 ts and publishes it
// retained to /state.
func (b *SlotBridge) publishState(st powerState) {
	p := stateEnvelope{TS: time.Now().UTC().Format(time.RFC3339)}
	p.powerState = st
	data, err := json.Marshal(p)
	if err != nil {
		b.log.Warnf("marshal state: %v", err)
		return
	}
	_ = b.pub.Publish(b.stateTopic(), true, data)
}

// stateEnvelope is the published /state shape: an RFC3339 ts plus the canonical
// power state fields (embedded).
type stateEnvelope struct {
	TS         string `json:"ts"`
	powerState        // embedded → top-level fields
}

// ------------------------------------------------------------------
// Topic helpers
// ------------------------------------------------------------------

func (b *SlotBridge) metaTopic() string {
	return schema.MetaTopic(b.cfg.Site, b.cfg.Station, b.cfg.Slot)
}
func (b *SlotBridge) stateTopic() string {
	return schema.StateTopic(b.cfg.Site, b.cfg.Station, b.cfg.Slot)
}

// CmdTopic returns the /cmd topic (exported for main to subscribe).
func (b *SlotBridge) CmdTopic() string { return schema.CmdTopic(b.cfg.Site, b.cfg.Station, b.cfg.Slot) }
