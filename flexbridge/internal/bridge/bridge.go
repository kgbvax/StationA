package bridge

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"flexbridge/internal/flexradio"
	"flexbridge/internal/ha"
)

// Bridge owns the radio state model and translates radio events (TCP
// status frames) into MQTT publishes following the station integration model.
// All shared state is guarded by mu.
type Bridge struct {
	cfg Config
	pub Publisher
	log Logger

	mu        sync.RWMutex
	device    ha.Device
	interlock flexradio.InterlockStatus
	slices    map[int]flexradio.SliceStatus
	tuStatus  flexradio.ATUStatus
	state     radioState

	discoDone bool
}

// radioState is the mutable radio state held under Bridge.mu.
type radioState struct {
	freqHz       int64
	band         string
	mode         string // canonical (NormalizeMode applied)
	txing        bool   // true while interlock.Transmitting
	tuning       bool   // true while ATU or radio is in tuning state
	drive        int    // 0-100 transmit drive level
	deviceOnline bool   // true while the radio TCP link is up (handshake done)
}

// statePayload is the JSON shape published to <slot>/state (retained).
// band and mode are omitted (not published raw or as placeholders) when the
// frequency is unknown or the firmware mode has no canonical mapping.
// device_online is always present (true/false) so consumers can distinguish a
// live radio from a frozen snapshot left over from a disconnect — /status is
// the MQTT/LWT bridge liveness, not the radio link.
type statePayload struct {
	TS           string `json:"ts"`
	FreqHz       int64  `json:"freq_hz"`
	Band         string `json:"band,omitempty"`
	Mode         string `json:"mode,omitempty"`
	TX           string `json:"tx"` // "rx" | "tx"
	Tuning       bool   `json:"tuning"`
	Drive        int    `json:"drive"`        // 0-100
	DeviceOnline bool   `json:"device_online"` // radio link liveness
}

// metaPayload is the JSON shape published to <slot>/meta (retained birth cert).
type metaPayload struct {
	Schema       string           `json:"schema"`
	Role         string           `json:"role"`
	Device       metaDevice       `json:"device"`
	Link         string           `json:"link,omitempty"`
	Location     string           `json:"location,omitempty"`
	Capabilities metaCapabilities `json:"capabilities"`
	Expose       *metaExpose      `json:"expose,omitempty"`
}

type metaDevice struct {
	Model    string `json:"model"`
	Serial   string `json:"serial"`
	Firmware string `json:"firmware,omitempty"`
}

// metaExpose is the consumer-neutral field surface (integration model §3.1, Appendix C).
// It carries NO consumer vocabulary (no device_class strings, no Jinja, no payload_on/off);
// consumers such as hadiscovery render their own representation from these neutral
// primitives. flexbridge is read-only, so none of its fields are writable and it exposes no
// actions — every field is a sensor/binary_sensor.
type metaExpose struct {
	Device metaExposeDevice  `json:"device"`
	Fields []metaExposeField `json:"fields"`
}

type metaExposeDevice struct {
	Name         string `json:"name,omitempty"`
	Model        string `json:"model,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	SWVersion    string `json:"sw_version,omitempty"`
	Area         string `json:"area,omitempty"`
}

// metaExposeField describes one /state field. type is number|enum|boolean|string.
// options_ref names a key in capabilities (e.g. "bands") that holds the enum options.
// on/off carry the boolean's state payload strings when the state holds strings, not a bool.
type metaExposeField struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Unit       string `json:"unit,omitempty"`
	Class      string `json:"class,omitempty"`
	StateClass string `json:"state_class,omitempty"`
	OptionsRef string `json:"options_ref,omitempty"`
	On         string `json:"on,omitempty"`
	Off        string `json:"off,omitempty"`
}

type metaCapabilities struct {
	Bands     []string `json:"bands"`
	Modes     []string `json:"modes"`
	Receivers int      `json:"receivers"`
	Diversity bool     `json:"diversity"`
	AmpKey    bool     `json:"amp_key"`
	Tune      bool     `json:"tune"`
	BiasT     bool     `json:"bias_t"`
	RxInputs  []string `json:"rx_inputs,omitempty"`
	TxOutputs []string `json:"tx_outputs,omitempty"`
}

// Config is the subset of config the bridge needs.
type Config struct {
	Site               string // e.g. "muehle"
	Station            string // e.g. "hf"
	Slot               string // e.g. "radio"
	Location           string // physical location label, e.g. "bauwagen"
	DiscoveryPrefix    string // e.g. "homeassistant"
	AvailTopic         string // <site>/<station>/<slot>/status  (LWT topic)
	PublishHADiscovery bool   // gate the legacy embedded HA discovery (model §9); default false
}

// Logger is the minimal logging surface the bridge uses.
type Logger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
}

// New constructs a Bridge.
func New(cfg Config, pub Publisher, log Logger) *Bridge {
	return &Bridge{
		cfg:    cfg,
		pub:    pub,
		log:    log,
		slices: make(map[int]flexradio.SliceStatus),
	}
}

// SetDevice records the radio identity (called after a successful handshake).
// Triggers meta and HA discovery publication, and marks the radio link online
// in /state so consumers can distinguish a live radio from a frozen snapshot.
func (b *Bridge) SetDevice(d ha.Device) {
	b.mu.Lock()
	b.device = d
	b.discoDone = false
	b.state.deviceOnline = true
	snap := b.state
	b.mu.Unlock()
	b.publishMeta()
	b.publishStateSnapshot(snap)
	if b.cfg.PublishHADiscovery {
		b.PublishDiscovery()
	}
}

// Reset clears runtime radio state on disconnect/reconnect so stale values
// aren't carried over and all fields are republished on reconnect. It also
// publishes a /state snapshot with device_online=false (and zeroed radio
// values) so the bus sees the radio link go down — /state is otherwise
// on-change-only and would freeze on the last live values, hiding the
// disconnect from consumers that only watch /state.
func (b *Bridge) Reset() {
	b.mu.Lock()
	b.slices = make(map[int]flexradio.SliceStatus)
	b.interlock = flexradio.InterlockStatus{}
	b.tuStatus = flexradio.ATUStatus{}
	b.state = radioState{} // deviceOnline defaults false
	b.discoDone = false
	snap := b.state
	b.mu.Unlock()
	b.publishStateSnapshot(snap)
}

// ------------------------------------------------------------------
// Status handling (TCP API)
// ------------------------------------------------------------------

// HandleStatus routes a parsed SmartSDR status frame.
func (b *Bridge) HandleStatus(f flexradio.Frame) {
	switch f.Topic {
	case "interlock":
		b.handleInterlock(f)
	case "slice":
		b.handleSlice(f)
	case "atu":
		b.handleATU(f)
	case "radio":
		b.handleRadio(f)
	}
}

// handleInterlock updates the TX state in the radio snapshot.
func (b *Bridge) handleInterlock(f flexradio.Frame) {
	is := flexradio.ParseInterlock(joinArgsFields(f))
	b.mu.Lock()
	prev := b.interlock
	b.interlock = is
	changed := prev.Transmitting != is.Transmitting
	if changed {
		b.state.txing = is.Transmitting
	}
	snap := b.state
	b.mu.Unlock()

	if changed {
		b.publishStateSnapshot(snap)
	}
}

// handleSlice updates per-slice state and, if the active/TX slice changed,
// republishes the radio state snapshot.
func (b *Bridge) handleSlice(f flexradio.Frame) {
	var idx int
	if args := strings.Fields(f.TopicArgs); len(args) > 0 {
		idx, _ = strconv.Atoi(args[0])
	}
	b.mu.RLock()
	prev := b.slices[idx]
	b.mu.RUnlock()

	s, err := flexradio.ParseSlice(f.TopicArgs, fieldsString(f), prev)
	if err != nil {
		b.log.Warnf("parse slice: %v", err)
		return
	}
	b.mu.Lock()
	b.slices[s.Index] = s
	changed := b.updateActiveSliceState()
	snap := b.state
	b.mu.Unlock()

	if changed {
		b.publishStateSnapshot(snap)
	}
}

// updateActiveSliceState updates b.state from the current TX/active slice.
// Must be called with b.mu held. Returns true if state changed.
func (b *Bridge) updateActiveSliceState() bool {
	active, ok := resolveActiveSlice(b.slices)
	if !ok {
		return false
	}
	newFreq := active.FreqHz
	newBand := flexradio.BandForFreq(newFreq)
	newMode := flexradio.NormalizeMode(active.Mode)
	if b.state.freqHz == newFreq && b.state.mode == newMode {
		return false
	}
	b.state.freqHz = newFreq
	b.state.band = newBand
	b.state.mode = newMode
	return true
}

// resolveActiveSlice returns the slice that drives the radio state.
// Prefers the TX slice; falls back to the Active slice.
func resolveActiveSlice(slices map[int]flexradio.SliceStatus) (flexradio.SliceStatus, bool) {
	for _, s := range slices {
		if s.TX {
			return s, true
		}
	}
	for _, s := range slices {
		if s.Active {
			return s, true
		}
	}
	return flexradio.SliceStatus{}, false
}

// handleATU updates the tuning flag when the ATU transitions into or out of
// active tuning.
func (b *Bridge) handleATU(f flexradio.Frame) {
	st := flexradio.ParseATU(joinArgsFields(f))
	b.mu.Lock()
	b.tuStatus = st
	newTuning := st.Status == "tuning"
	changed := b.state.tuning != newTuning
	if changed {
		b.state.tuning = newTuning
	}
	snap := b.state
	b.mu.Unlock()

	if changed {
		b.publishStateSnapshot(snap)
	}
}

// handleRadio updates drive level and radio-level tuning flag.
func (b *Bridge) handleRadio(f flexradio.Frame) {
	fs := fieldsString(f)
	b.mu.Lock()
	changed := false
	if drive, ok := flexradio.ParseDrive(fs); ok && b.state.drive != drive {
		b.state.drive = drive
		changed = true
	}
	if tuning, ok := flexradio.ParseRadioTuning(fs); ok && b.state.tuning != tuning {
		b.state.tuning = tuning
		changed = true
	}
	snap := b.state
	b.mu.Unlock()

	if changed {
		b.publishStateSnapshot(snap)
	}
}

// ------------------------------------------------------------------
// Publishing helpers
// ------------------------------------------------------------------

// publishStateSnapshot marshals the current state to JSON and publishes it
// to the retained state topic.
func (b *Bridge) publishStateSnapshot(st radioState) {
	tx := "rx"
	if st.txing {
		tx = "tx"
	}
	p := statePayload{
		TS:           time.Now().UTC().Format(time.RFC3339),
		FreqHz:       st.freqHz,
		Band:         st.band,
		Mode:         st.mode,
		TX:           tx,
		Tuning:       st.tuning,
		Drive:        st.drive,
		DeviceOnline: st.deviceOnline,
	}
	data, err := json.Marshal(p)
	if err != nil {
		b.log.Warnf("marshal state: %v", err)
		return
	}
	_ = b.pub.Publish(b.stateTopic(), true, data)
}

// publishMeta publishes the retained birth-certificate topic with device
// identity and capabilities. Called once per connect cycle from SetDevice.
func (b *Bridge) publishMeta() {
	b.mu.RLock()
	d := b.device
	loc := b.cfg.Location
	b.mu.RUnlock()

	if d.Serial == "" {
		return
	}

	// Capabilities are static knowledge for the FLEX-8400.
	p := metaPayload{
		Schema: "1.0",
		Role:   "radio",
		Device: metaDevice{
			Model:    d.Model,
			Serial:   d.Serial,
			Firmware: d.Firmware,
		},
		Link:     "ethernet",
		Location: loc,
		Capabilities: metaCapabilities{
			Bands:     []string{"160m", "80m", "60m", "40m", "30m", "20m", "17m", "15m", "12m", "10m", "6m"},
			Modes:     []string{"cw", "usb", "lsb", "am", "fm", "data"},
			Receivers: 1,
			Diversity: false,
			AmpKey:    true,
			Tune:      true,
			BiasT:     false,
			RxInputs:  []string{"ant1", "ant2", "rx_a"},
			TxOutputs: []string{"ant1", "ant2"},
		},
		// Consumer-neutral field surface (model §3.1). flexbridge is read-only:
		// no field is writable and there are no actions — every field renders as a
		// sensor/binary_sensor. The enum options resolve via options_ref against
		// the capabilities above (bands/modes). HA-specific rendering lives in
		// hadiscovery, not here.
		Expose: &metaExpose{
			Device: metaExposeDevice{
				Name:         d.Name,
				Model:        d.Model,
				Manufacturer: "FlexRadio Systems",
				SWVersion:    d.Firmware,
				Area:         loc,
			},
			Fields: []metaExposeField{
				{Key: "device_online", Name: "Device online", Type: "boolean"},
				{Key: "freq_hz", Name: "Frequency", Type: "number", Unit: "Hz", Class: "frequency", StateClass: "measurement"},
				{Key: "band", Name: "Band", Type: "enum", OptionsRef: "bands"},
				{Key: "mode", Name: "Mode", Type: "enum", OptionsRef: "modes"},
				{Key: "drive", Name: "Drive", Type: "number", Unit: "%"},
				{Key: "tx", Name: "Transmitting", Type: "boolean", On: "tx", Off: "rx"},
				{Key: "tuning", Name: "Tuning", Type: "boolean"},
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

// PublishDiscovery emits HA discovery for all radio entities. Safe to call
// repeatedly; only emits once per connect cycle.
func (b *Bridge) PublishDiscovery() {
	b.mu.Lock()
	if b.discoDone || b.device.Serial == "" {
		b.mu.Unlock()
		return
	}
	b.discoDone = true
	d := b.device
	b.mu.Unlock()

	nodeID := ha.NodeID(d.Serial)
	st := b.stateTopic()
	avail := b.cfg.AvailTopic

	entities := []struct {
		name, objectID, template, unit string
		binary                         bool
		onPayload, offPayload          string
	}{
		{
			name: "Frequency", objectID: "frequency",
			template: "{{ value_json.freq_hz }}", unit: "Hz",
		},
		{
			name: "Band", objectID: "band",
			template: "{{ value_json.band }}", unit: "",
		},
		{
			name: "Mode", objectID: "mode",
			template: "{{ value_json.mode }}", unit: "",
		},
		{
			name: "Drive", objectID: "drive",
			template: "{{ value_json.drive }}", unit: "%",
		},
		{
			name: "Transmitting", objectID: "transmitting", binary: true,
			template: "{{ value_json.tx }}", onPayload: "tx", offPayload: "rx",
		},
		{
			name: "Tuning", objectID: "tuning", binary: true,
			template: "{{ value_json.tuning | lower }}", onPayload: "true", offPayload: "false",
		},
		{
			name: "Device online", objectID: "device_online", binary: true,
			template: "{{ value_json.device_online }}", onPayload: "true", offPayload: "false",
		},
	}

	for _, e := range entities {
		var cfg ha.DiscoveryConfig
		var comp string
		if e.binary {
			cfg, comp = ha.BinaryEntity(e.name, e.objectID, st, e.onPayload, e.offPayload, e.template, d, avail)
		} else {
			cfg, comp = ha.StatusEntity(e.name, e.objectID, st, e.unit, e.template, d, avail)
		}
		topic := ha.ConfigTopic(b.cfg.DiscoveryPrefix, comp, nodeID, e.objectID)
		_ = publishDiscovery(b.pub, topic, cfg)
	}
}

// ------------------------------------------------------------------
// Topic helpers
// ------------------------------------------------------------------

func (b *Bridge) slotBase() string {
	return b.cfg.Site + "/" + b.cfg.Station + "/" + b.cfg.Slot
}

func (b *Bridge) metaTopic() string  { return b.slotBase() + "/meta" }
func (b *Bridge) stateTopic() string { return b.slotBase() + "/state" }

// ------------------------------------------------------------------
// Internal helpers (free functions)
// ------------------------------------------------------------------

func joinArgsFields(f flexradio.Frame) string {
	return f.TopicArgs + " " + fieldsString(f)
}

func fieldsString(f flexradio.Frame) string {
	out := ""
	for k, v := range f.Fields {
		if out != "" {
			out += " "
		}
		out += k + "=" + v
	}
	return out
}
