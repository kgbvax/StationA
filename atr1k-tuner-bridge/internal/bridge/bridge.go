// Package bridge owns the canonical tuner state model and translates decoded
// ATR-1000 frames (internal/tuner.State) into MQTT publishes following the
// station integration model (slot muehle/hf/tuner). It also dispatches /cmd
// intent (set_inline, tune) to the tuner via a Commander.
package bridge

import (
	"encoding/json"
	"sync"
	"time"

	"atr1k-tuner-bridge/internal/tuner"

	schema "codeberg.org/kgbvax/stationa/shared/schema"
)

// Commander is the tuner control surface the bridge drives from /cmd. The
// tuner device (internal/tuner.Device) implements it; tests use a fake.
type Commander = tuner.Commander

// Config is the subset of config the bridge needs.
type Config struct {
	Site     string // e.g. "muehle"
	Station  string // e.g. "hf"
	Slot     string // e.g. "tuner"
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
	Debugf(format string, args ...any)
}

// Bridge owns the canonical tuner state and publishes it to MQTT.
type Bridge struct {
	cfg Config
	pub Publisher
	log Logger

	mu    sync.RWMutex
	state tuner.State
	last  tuner.State // last published snapshot (for change dedup)
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

// metaCapabilities is the §7.1 tuner capability contract: the ATU can be put
// in line/bypass and tunes in mem or full mode.
type metaCapabilities struct {
	Inline    bool     `json:"inline"`
	TuneModes []string `json:"tune_modes"`
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
	Options    []string     `json:"options,omitempty"`
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

// cmdPayload is the /cmd JSON the bridge accepts. Both actions carry their
// argument under the conventional `value` key (matching acombridge's
// set_band: {"action":"set_band","value":"20m"}) — set_inline a bool, tune a
// mode ("mem"|"full").
type cmdPayload struct {
	Action string          `json:"action"`
	Value  json.RawMessage `json:"value,omitempty"`
}

// New constructs a Bridge.
func New(cfg Config, pub Publisher, log Logger) *Bridge {
	return &Bridge{cfg: cfg, pub: pub, log: log}
}

// PublishMeta publishes the retained birth certificate. Called on (re)connect.
func (b *Bridge) PublishMeta() {
	b.mu.RLock()
	loc := b.cfg.Location
	host := b.cfg.Host
	b.mu.RUnlock()

	p := metaPayload{
		Schema: "1.0",
		Role:   "tuner",
		Device: metaDevice{
			Model: b.cfg.DeviceModel,
		},
		Link:     b.cfg.DeviceLink,
		Location: loc,
		Host:     host,
		Capabilities: metaCapabilities{
			Inline:    true,
			TuneModes: []string{"mem", "full"},
		},
		Expose: &metaExpose{
			Device: metaExposeDevice{
				Name:  "HF ATU",
				Model: b.cfg.DeviceModel,
				// No Manufacturer: the ATR-1000 / N7DDC family lineage is device
				// detail; the bridge carries no HA/area knowledge (model §9).
			},
			Fields: []metaExposeField{
				{Key: "swr", Name: "SWR", Type: "number", Unit: "ratio",
					Class: "swr", StateClass: "measurement"},
				{Key: "fwd", Name: "Forward Power", Type: "number", Unit: "W",
					Class: "power", StateClass: "measurement"},
				{Key: "inline", Name: "In Line", Type: "boolean", Writable: true,
					Command: &metaCommand{Action: "set_inline", ValueKey: "value", ValueType: "bool"}},
				{Key: "l_uh", Name: "Inductance", Type: "number", Unit: "µH"},
				{Key: "c_pf", Name: "Capacitance", Type: "number", Unit: "pF"},
				{Key: "settling", Name: "Tuning", Type: "boolean"},
				{Key: "fault", Name: "Fault", Type: "string"},
				{Key: "device_online", Name: "Device Online", Type: "boolean"},
			},
			Actions: []metaExposeAction{
				{Key: "tune", Name: "Tune", Command: &metaCommand{Action: "tune", ValueKey: "value", ValueType: "enum"}},
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

// HandleTelemetry updates the cached state and publishes the retained /state
// snapshot, but only when a published field changed since the last snapshot.
// The ATR streams meter frames frequently; dedup keeps the bus quiet while the
// SWR sits steady (model §8 — state is the live snapshot, not a firehose).
func (b *Bridge) HandleTelemetry(st tuner.State) {
	b.mu.Lock()
	b.state = st
	changed := stateChanged(b.last, st)
	if changed {
		b.last = st
	}
	b.mu.Unlock()
	if changed {
		b.publishState(st)
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
	changed := stateChanged(b.last, snap)
	if changed {
		b.last = snap
	}
	b.mu.Unlock()
	if changed {
		b.publishState(snap)
	}
}

// stateChanged reports whether two snapshots differ in any published field.
// SWR/Fwd are floats compared with ==: the raw ATR value is kept unchanged across
// identical frames, so a steady SWR produces no churn. When the SWR moves the
// value changes every frame, as expected.
func stateChanged(a, b tuner.State) bool {
	return a.Inline != b.Inline ||
		a.SWR != b.SWR ||
		a.Fwd != b.Fwd ||
		a.LUH != b.LUH ||
		a.CPF != b.CPF ||
		a.Settling != b.Settling ||
		a.Fault != b.Fault ||
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
	case "set_inline":
		if len(c.Value) == 0 {
			b.log.Warnf("cmd set_inline: missing value")
			return
		}
		var v bool
		if err := json.Unmarshal(c.Value, &v); err != nil {
			b.log.Warnf("cmd set_inline: bad value: %v", err)
			return
		}
		if err := b.cfg.Commander.SetInline(v); err != nil {
			b.log.Warnf("cmd set_inline: %v", err)
		}
	case "tune":
		if len(c.Value) == 0 {
			b.log.Warnf("cmd tune: missing value")
			return
		}
		var mode string
		if err := json.Unmarshal(c.Value, &mode); err != nil {
			b.log.Warnf("cmd tune: bad value: %v", err)
			return
		}
		switch mode {
		case "mem":
			if err := b.cfg.Commander.Tune(false); err != nil {
				b.log.Warnf("cmd tune mem: %v", err)
			}
		case "full":
			if err := b.cfg.Commander.Tune(true); err != nil {
				b.log.Warnf("cmd tune full: %v", err)
			}
		default:
			b.log.Warnf("cmd tune: unknown mode %q", mode)
		}
	default:
		b.log.Warnf("cmd: unknown action %q", c.Action)
	}
}

// publishState marshals the snapshot with an RFC3339 ts and publishes it
// retained to /state.
func (b *Bridge) publishState(st tuner.State) {
	p := stateEnvelope{
		TS:    time.Now().UTC().Format(time.RFC3339),
		State: st,
	}
	data, err := json.Marshal(p)
	if err != nil {
		b.log.Warnf("marshal state: %v", err)
		return
	}
	_ = b.pub.Publish(b.stateTopic(), true, data)
}

// stateEnvelope is the published /state shape: an RFC3339 ts plus the canonical
// tuner state.
type stateEnvelope struct {
	TS string `json:"ts"`
	tuner.State
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
