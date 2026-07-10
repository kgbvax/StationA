// Package bridge owns the canonical PA state model and translates decoded
// ACOM telemetry (internal/acom.Observation) into MQTT publishes following the
// station integration model (slot muehle/hf/pa). It also dispatches /cmd
// intent to the amplifier via a Commander.
package bridge

import (
	"encoding/json"
	"sync"
	"time"

	"acombridge/internal/acom"
	"acombridge/internal/ha"
)

// Commander is the amplifier control surface the bridge drives from /cmd. The
// device (internal/acom.Device) implements it; tests use a fake.
type Commander interface {
	SetMode(mode string) error // "operate" | "standby"
	SetBand(band string) error // canonical band label, e.g. "20m"
}

// Config is the subset of config the bridge needs.
type Config struct {
	Site               string // e.g. "muehle"
	Station            string // e.g. "hf"
	Slot               string // e.g. "pa"
	Location           string // physical location label, e.g. "bauwagen"
	Host               string // compute node, published in /meta
	DiscoveryPrefix    string // e.g. "homeassistant"
	AvailTopic         string // <site>/<station>/<slot>/status (LWT topic)
	PublishHADiscovery bool   // gate legacy embedded HA discovery (model §9); default false

	// Device identity for /meta. The ACOM protocol reports no serial, so
	// DeviceSerial is a stable configured identifier (defaulted when empty).
	DeviceModel  string
	DeviceSerial string
	DeviceLink   string

	// Commander executes /cmd intent. May be nil (read-only deploy).
	Commander Commander
}

// Logger is the minimal logging surface the bridge uses.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

// Bridge owns the canonical PA state and publishes it to MQTT.
type Bridge struct {
	cfg Config
	pub Publisher
	log Logger
	dev ha.Device

	mu    sync.RWMutex
	state paState
	disco bool
}

// paState is the canonical PA state (integration model §7.1) plus the raw
// diagnostic pa_state and the optional device_online/error fields (§3).
type paState struct {
	Mode         string  `json:"mode"` // operate | standby | bypass
	Band         string  `json:"band,omitempty"`
	Keyed        string  `json:"keyed"` // rx | tx | inhibited
	FwdPowerW    uint16  `json:"fwd_power_w"`
	RflPowerW    uint16  `json:"rfl_power_w"`
	TempC        float64 `json:"temp_c"`
	SWR          float64 `json:"swr"`
	Fault        string  `json:"fault"`    // none | swr | temp | reflected | other
	PaState      string  `json:"pa_state"` // raw firmware mode (diagnostic)
	DeviceOnline bool    `json:"device_online"`
	Error        string  `json:"error,omitempty"`
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
	Model    string `json:"model"`
	Serial   string `json:"serial"`
	Firmware string `json:"firmware,omitempty"`
}

// metaCapabilities is the §7.1 PA capability contract.
type metaCapabilities struct {
	Bands      []string `json:"bands"`
	MaxPowerW  int      `json:"max_power_w"`
	BandSource string   `json:"band_source"`
	RfSample   bool     `json:"rf_sample"`
	KeyInput   string   `json:"key_input"`
	AlcOut     bool     `json:"alc_out"`
	Modes      []string `json:"modes"`
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
// options_ref names a capabilities key holding the enum options. writable + a
// command descriptor make it a setpoint; the consumer renders the command into
// its own /cmd syntax.
type metaExposeField struct {
	Key        string       `json:"key"`
	Name       string       `json:"name"`
	Type       string       `json:"type"`
	Unit       string       `json:"unit,omitempty"`
	Class      string       `json:"class,omitempty"`
	StateClass string       `json:"state_class,omitempty"`
	Options    []string     `json:"options,omitempty"`
	OptionsRef string       `json:"options_ref,omitempty"`
	Writable   bool         `json:"writable,omitempty"`
	Command    *metaCommand `json:"command,omitempty"`
	On         string       `json:"on,omitempty"`
	Off        string       `json:"off,omitempty"`
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

// cmdPayload is the /cmd JSON the bridge accepts.
type cmdPayload struct {
	Action string `json:"action"`
	Value  string `json:"value"`
}

// New constructs a Bridge.
func New(cfg Config, pub Publisher, log Logger) *Bridge {
	serial := cfg.DeviceSerial
	if serial == "" {
		serial = "acom-1200s"
	}
	return &Bridge{
		cfg: cfg,
		pub: pub,
		log: log,
		dev: ha.Device{Serial: serial, Model: cfg.DeviceModel, Name: cfg.DeviceModel},
	}
}

// PublishMeta publishes the retained birth certificate. Called on (re)connect.
func (b *Bridge) PublishMeta() {
	b.mu.RLock()
	loc := b.cfg.Location
	host := b.cfg.Host
	b.mu.RUnlock()

	p := metaPayload{
		Schema: "1.0",
		Role:   "pa",
		Device: metaDevice{
			Model:  b.cfg.DeviceModel,
			Serial: b.dev.Serial,
		},
		Link:     b.cfg.DeviceLink,
		Location: loc,
		Host:     host,
		Capabilities: metaCapabilities{
			Bands:      acom.BandOptions,
			MaxPowerW:  1200,
			BandSource: "cat",
			RfSample:   false,
			KeyInput:   "hardware",
			AlcOut:     true,
			Modes:      []string{"operate", "standby"},
		},
		Expose: &metaExpose{
			Device: metaExposeDevice{
				Name:         b.cfg.DeviceModel,
				Model:        b.cfg.DeviceModel,
				Manufacturer: "ACOM",
				Area:         loc,
			},
			Fields: []metaExposeField{
				{Key: "mode", Name: "Mode", Type: "enum", OptionsRef: "modes", Writable: true,
					Command: &metaCommand{Action: "set_mode", ValueKey: "value", ValueType: "string"}},
				{Key: "band", Name: "Band", Type: "enum", OptionsRef: "bands", Writable: true,
					Command: &metaCommand{Action: "set_band", ValueKey: "value", ValueType: "string"}},
				{Key: "keyed", Name: "Keyed", Type: "enum", Options: []string{"rx", "tx", "inhibited"}},
				{Key: "fwd_power_w", Name: "Forward Power", Type: "number", Unit: "W", Class: "power", StateClass: "measurement"},
				{Key: "rfl_power_w", Name: "Reflected Power", Type: "number", Unit: "W", Class: "power", StateClass: "measurement"},
				{Key: "temp_c", Name: "Temperature", Type: "number", Unit: "°C", Class: "temperature", StateClass: "measurement"},
				{Key: "swr", Name: "SWR", Type: "number", StateClass: "measurement"},
				{Key: "fault", Name: "Fault", Type: "enum", Options: []string{"none", "swr", "temp", "reflected", "other"}},
				{Key: "pa_state", Name: "PA State", Type: "string"},
				{Key: "device_online", Name: "Device Online", Type: "boolean"},
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

// Reset clears runtime state on disconnect so stale values aren't carried over.
func (b *Bridge) Reset() {
	b.mu.Lock()
	b.state = paState{}
	b.disco = false
	b.mu.Unlock()
}

// HandleTelemetry canonicalizes a decoded frame, updates state, and publishes
// the retained /state snapshot. Every frame is published (PA telemetry is
// continuous; the snapshot is the live state, model §8).
func (b *Bridge) HandleTelemetry(obs acom.Observation) {
	b.mu.Lock()
	b.state = paState{
		Mode:         acom.CanonicalMode(obs.ModeRaw),
		Band:         obs.BandName,
		Keyed:        acom.CanonicalKeyed(obs.ModeRaw),
		FwdPowerW:    obs.ForwardPower,
		RflPowerW:    obs.ReflectedPower,
		TempC:        obs.Temperature,
		SWR:          obs.SWR,
		Fault:        acom.CanonicalFault(obs.ErrByte, obs.ErrMsg),
		PaState:      obs.ModeRaw,
		DeviceOnline: true,
		Error:        errMsgFor(obs.ErrByte, obs.ErrMsg),
	}
	snap := b.state
	b.mu.Unlock()
	b.publishState(snap)
}

// SetDeviceOnline updates the device_online/error fields and publishes a
// snapshot. Called when the serial port is lost or regained; /status itself
// (the bridge LWT) is unaffected — the bridge is still up.
func (b *Bridge) SetDeviceOnline(online bool, errMsg string) {
	b.mu.Lock()
	b.state.DeviceOnline = online
	b.state.Error = errMsg
	snap := b.state
	b.mu.Unlock()
	b.publishState(snap)
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
	case "set_mode":
		if err := b.cfg.Commander.SetMode(c.Value); err != nil {
			b.log.Warnf("cmd set_mode: %v", err)
		}
	case "set_band":
		if err := b.cfg.Commander.SetBand(c.Value); err != nil {
			b.log.Warnf("cmd set_band: %v", err)
		}
	default:
		b.log.Warnf("cmd: unknown action %q", c.Action)
	}
}

// PublishDiscovery emits the legacy embedded HA discovery, gated behind
// cfg.PublishHADiscovery (model §9). Default off — discovery is rendered by
// the standalone hadiscovery consumer from /meta.expose. Safe to call
// repeatedly; only emits once per connect cycle.
func (b *Bridge) PublishDiscovery() {
	if !b.cfg.PublishHADiscovery {
		return
	}
	b.mu.Lock()
	if b.disco {
		b.mu.Unlock()
		return
	}
	b.disco = true
	dev := b.dev
	b.mu.Unlock()

	nodeID := ha.NodeID(dev.Serial)
	st := b.stateTopic()
	cmd := b.cmdTopic()
	avail := b.cfg.AvailTopic

	// Writable selects (mode, band) point at /cmd; telemetry sensors read /state.
	// The command_template wraps HA's selected option into the /cmd action JSON.
	selects := []struct {
		name, objectID, template, cmdTemplate string
		options                               []string
	}{
		{name: "Mode", objectID: "mode", template: "{{ value_json.mode }}",
			cmdTemplate: `{"action":"set_mode","value":"{{ value }}"}`,
			options:     []string{"operate", "standby"}},
		{name: "Band", objectID: "band", template: "{{ value_json.band }}",
			cmdTemplate: `{"action":"set_band","value":"{{ value }}"}`,
			options:     acom.BandOptions},
	}
	for _, e := range selects {
		cfg, comp := ha.SelectEntity(e.name, e.objectID, st, cmd, e.template, e.cmdTemplate, e.options, dev, avail)
		topic := ha.ConfigTopic(b.cfg.DiscoveryPrefix, comp, nodeID, e.objectID)
		_ = publishDiscovery(b.pub, topic, cfg)
	}

	// Numeric/string telemetry sensors. HA sends the raw /cmd payload
	// "{{ value_json.<field> }}" extracts each field from the single state doc.
	sensors := []struct {
		name, objectID, unit, template string
	}{
		{name: "Forward Power", objectID: "fwd_power_w", unit: "W", template: "{{ value_json.fwd_power_w }}"},
		{name: "Reflected Power", objectID: "rfl_power_w", unit: "W", template: "{{ value_json.rfl_power_w }}"},
		{name: "Temperature", objectID: "temp_c", unit: "°C", template: "{{ value_json.temp_c }}"},
		{name: "SWR", objectID: "swr", unit: "", template: "{{ value_json.swr }}"},
		{name: "Keyed", objectID: "keyed", unit: "", template: "{{ value_json.keyed }}"},
		{name: "Fault", objectID: "fault", unit: "", template: "{{ value_json.fault }}"},
		{name: "PA State", objectID: "pa_state", unit: "", template: "{{ value_json.pa_state }}"},
	}
	for _, e := range sensors {
		cfg, comp := ha.SensorEntity(e.name, e.objectID, st, e.unit, e.template, dev, avail)
		topic := ha.ConfigTopic(b.cfg.DiscoveryPrefix, comp, nodeID, e.objectID)
		_ = publishDiscovery(b.pub, topic, cfg)
	}

	// device_online as a binary_sensor.
	onCfg, onComp := ha.BinaryEntity("Device Online", "device_online", st, "true", "false",
		"{{ value_json.device_online }}", dev, avail)
	_ = publishDiscovery(b.pub, ha.ConfigTopic(b.cfg.DiscoveryPrefix, onComp, nodeID, "device_online"), onCfg)
}

// publishState marshals the snapshot with an RFC3339 ts and publishes it
// retained to /state.
func (b *Bridge) publishState(st paState) {
	p := stateEnvelope{
		TS:      time.Now().UTC().Format(time.RFC3339),
		paState: st,
	}
	data, err := json.Marshal(p)
	if err != nil {
		b.log.Warnf("marshal state: %v", err)
		return
	}
	_ = b.pub.Publish(b.stateTopic(), true, data)
}

// stateEnvelope is the published /state shape: an RFC3339 ts plus the canonical
// PA state fields (embedded).
type stateEnvelope struct {
	TS string `json:"ts"`
	paState
}

// errMsgFor returns the human-readable fault string for /state.error, empty
// when there is no fault.
func errMsgFor(errByte byte, errMsg string) string {
	if errByte == 0xFF {
		return ""
	}
	return errMsg
}

// ------------------------------------------------------------------
// Topic helpers
// ------------------------------------------------------------------

func (b *Bridge) slotBase() string {
	return b.cfg.Site + "/" + b.cfg.Station + "/" + b.cfg.Slot
}

func (b *Bridge) metaTopic() string  { return b.slotBase() + "/meta" }
func (b *Bridge) stateTopic() string { return b.slotBase() + "/state" }
func (b *Bridge) cmdTopic() string   { return b.slotBase() + "/cmd" }

// CmdTopic returns the /cmd topic (exported for main to subscribe).
func (b *Bridge) CmdTopic() string { return b.cmdTopic() }
