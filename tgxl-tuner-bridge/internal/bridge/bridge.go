// Package bridge owns the canonical tuner state model and translates the
// Tuner Genius XL state (internal/tgxl.State) into MQTT publishes following the
// station integration model (slot muehle/hf/tuner). It also dispatches /cmd
// intent (set_inline, set_operate, set_antenna, tune) via a Commander.
package bridge

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"tgxl-tuner-bridge/internal/tgxl"

	schema "codeberg.org/kgbvax/stationa/shared/schema"
)

// Commander is the tuner control surface the bridge drives from /cmd.
type Commander = tgxl.Commander

// Config is the subset of config the bridge needs.
type Config struct {
	Site, Station, Slot string
	Location            string // physical location label, e.g. "bauwagen"
	Host                string // compute node, published in /meta

	DeviceModel string
	DeviceLink  string

	Commander Commander // may be nil (read-only)
}

// Logger is the minimal logging surface the bridge uses.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Bridge owns the canonical tuner state and publishes it to MQTT.
type Bridge struct {
	cfg Config
	pub Publisher
	log Logger

	mu       sync.RWMutex
	state    tgxl.State
	last     tgxl.State
	hasLast  bool // RepublishState is a no-op until a snapshot has published
	serial   string
	firmware string
}

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
	Model    string `json:"model"`
	Serial   string `json:"serial,omitempty"`
	Firmware string `json:"firmware,omitempty"`
}

// metaCapabilities is the §7.1 tuner capability contract.
type metaCapabilities struct {
	Inline    bool     `json:"inline"`
	TuneModes []string `json:"tune_modes"`
	Antennas  int      `json:"antennas"`
}

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
	OptionsRef string       `json:"options_ref,omitempty"`
	Writable   bool         `json:"writable,omitempty"`
	Command    *metaCommand `json:"command,omitempty"`
}

type metaExposeAction struct {
	Key        string       `json:"key"`
	Name       string       `json:"name"`
	Options    []string     `json:"options,omitempty"`
	OptionsRef string       `json:"options_ref,omitempty"`
	Command    *metaCommand `json:"command"`
}

type metaCommand struct {
	Action    string `json:"action,omitempty"`
	ValueKey  string `json:"value_key,omitempty"`
	ValueType string `json:"value_type,omitempty"`
}

// cmdPayload: every action carries its argument under the conventional `value`.
type cmdPayload struct {
	Action string          `json:"action"`
	Value  json.RawMessage `json:"value,omitempty"`
}

// New constructs a Bridge.
func New(cfg Config, pub Publisher, log Logger) *Bridge {
	return &Bridge{cfg: cfg, pub: pub, log: log}
}

func fptr(v float64) *float64 { return &v }

// Meta builds the birth certificate (exported for tests).
func (b *Bridge) Meta() metaPayload {
	b.mu.RLock()
	serial, fw := b.serial, b.firmware
	b.mu.RUnlock()
	return metaPayload{
		Schema: "1.0",
		Role:   "tuner",
		Device: metaDevice{Model: b.cfg.DeviceModel, Serial: serial, Firmware: fw},
		Link:   b.cfg.DeviceLink, Location: b.cfg.Location, Host: b.cfg.Host,
		Capabilities: metaCapabilities{Inline: true, TuneModes: []string{"auto"}, Antennas: 3},
		Expose: &metaExpose{
			Device: metaExposeDevice{Name: "HF ATU", Model: b.cfg.DeviceModel, Manufacturer: "FlexRadio"},
			Fields: []metaExposeField{
				{Key: "swr", Name: "SWR", Type: "number", Unit: "ratio", Class: "swr", StateClass: "measurement"},
				{Key: "fwd", Name: "Forward Power", Type: "number", Unit: "W", Class: "power", StateClass: "measurement"},
				{Key: "inline", Name: "In Line", Type: "boolean", Writable: true,
					Command: &metaCommand{Action: "set_inline", ValueKey: "value", ValueType: "bool"}},
				{Key: "operate", Name: "Operate", Type: "boolean", Writable: true,
					Command: &metaCommand{Action: "set_operate", ValueKey: "value", ValueType: "bool"}},
				{Key: "antenna", Name: "Antenna", Type: "number", Writable: true,
					Min: fptr(1), Max: fptr(3), Step: fptr(1),
					Command: &metaCommand{Action: "set_antenna", ValueKey: "value", ValueType: "number"}},
				{Key: "settling", Name: "Tuning", Type: "boolean"},
				{Key: "ptt", Name: "PTT", Type: "boolean"},
				{Key: "fault", Name: "Fault", Type: "string"},
				{Key: "message", Name: "Message", Type: "string"},
				{Key: "device_online", Name: "Device Online", Type: "boolean"},
			},
			Actions: []metaExposeAction{
				{Key: "tune", Name: "Tune", OptionsRef: "tune_modes",
					Command: &metaCommand{Action: "tune", ValueKey: "value", ValueType: "enum"}},
			},
		},
	}
}

// PublishMeta publishes the retained birth certificate. Called on (re)connect.
func (b *Bridge) PublishMeta() {
	data, err := json.Marshal(b.Meta())
	if err != nil {
		b.log.Errorf("marshal meta: %v", err)
		return
	}
	_ = b.pub.Publish(b.metaTopic(), true, data)
}

// SetDeviceInfo records serial/firmware from the TGXL `info` reply and
// republishes /meta when they changed.
func (b *Bridge) SetDeviceInfo(info tgxl.Info) {
	b.mu.Lock()
	changed := b.serial != info.Serial || b.firmware != info.Version
	b.serial, b.firmware = info.Serial, info.Version
	b.mu.Unlock()
	if changed {
		b.PublishMeta()
	}
}

// HandleTelemetry updates the cached state and publishes /state when a
// published field changed (change-dedup, model §8).
func (b *Bridge) HandleTelemetry(st tgxl.State) {
	b.mu.Lock()
	b.state = st
	changed := st != b.last
	if changed {
		b.last = st
		b.hasLast = true
	}
	b.mu.Unlock()
	if changed {
		b.publishState(st)
	}
}

// SetDeviceOnline updates device_online/error; /status (bridge LWT) is unaffected.
func (b *Bridge) SetDeviceOnline(online bool, errMsg string) {
	b.mu.Lock()
	b.state.DeviceOnline = online
	b.state.Error = errMsg
	if !online {
		b.state.Settling = false
	}
	snap := b.state
	changed := snap != b.last
	if changed {
		b.last = snap
		b.hasLast = true
	}
	b.mu.Unlock()
	if changed {
		b.publishState(snap)
	}
}

// RepublishState re-lands the retained /state after an MQTT (re)connect. No-op
// until the first snapshot has published (never an all-zero snapshot).
func (b *Bridge) RepublishState() {
	b.mu.Lock()
	if !b.hasLast {
		b.mu.Unlock()
		return
	}
	snap := b.state
	b.mu.Unlock()
	b.publishState(snap)
}

// HandleCommand parses a /cmd JSON payload and dispatches it.
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
	if len(c.Value) == 0 {
		b.log.Warnf("cmd %s: missing value", c.Action)
		return
	}
	switch c.Action {
	case "set_inline", "set_operate":
		var v bool
		if err := json.Unmarshal(c.Value, &v); err != nil {
			b.log.Warnf("cmd %s: bad value: %v", c.Action, err)
			return
		}
		var err error
		if c.Action == "set_inline" {
			err = b.cfg.Commander.SetInline(v)
		} else {
			err = b.cfg.Commander.SetOperate(v)
		}
		if err != nil {
			b.log.Warnf("cmd %s: %v", c.Action, err)
		}
	case "set_antenna":
		var n int
		if err := json.Unmarshal(c.Value, &n); err != nil || n < 1 || n > 3 {
			b.log.Warnf("cmd set_antenna: bad value %s (want 1..3)", string(c.Value))
			return
		}
		if err := b.cfg.Commander.SetAntenna(n); err != nil {
			b.log.Warnf("cmd set_antenna: %v", err)
		}
	case "tune":
		var mode string
		if err := json.Unmarshal(c.Value, &mode); err != nil {
			b.log.Warnf("cmd tune: bad value: %v", err)
			return
		}
		switch strings.ToLower(mode) {
		case "auto":
		case "mem", "full":
			// ATR-1000 legacy modes: the TGXL recalls memory solutions by itself.
			b.log.Debugf("cmd tune: legacy mode %q treated as auto", mode)
		default:
			b.log.Warnf("cmd tune: unknown mode %q", mode)
			return
		}
		if err := b.cfg.Commander.Tune(); err != nil {
			b.log.Warnf("cmd tune: %v", err)
		}
	default:
		b.log.Warnf("cmd: unknown action %q", c.Action)
	}
}

type stateEnvelope struct {
	TS string `json:"ts"`
	tgxl.State
}

func (b *Bridge) publishState(st tgxl.State) {
	data, err := json.Marshal(stateEnvelope{TS: time.Now().UTC().Format(time.RFC3339), State: st})
	if err != nil {
		b.log.Errorf("marshal state: %v", err)
		return
	}
	_ = b.pub.Publish(b.stateTopic(), true, data)
}

func (b *Bridge) metaTopic() string  { return schema.MetaTopic(b.cfg.Site, b.cfg.Station, b.cfg.Slot) }
func (b *Bridge) stateTopic() string { return schema.StateTopic(b.cfg.Site, b.cfg.Station, b.cfg.Slot) }

// CmdTopic returns the /cmd topic (exported for main to subscribe).
func (b *Bridge) CmdTopic() string { return schema.CmdTopic(b.cfg.Site, b.cfg.Station, b.cfg.Slot) }
