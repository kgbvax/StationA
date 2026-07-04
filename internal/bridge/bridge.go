package bridge

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"flexbridge/internal/flexradio"
	"flexbridge/internal/ha"
)

// Bridge owns the radio state model and translates radio events (TCP
// status frames + VITA-49 meter packets) into MQTT publishes with
// Home Assistant discovery.
//
// It is concurrency-safe: the TCP reader goroutine calls HandleStatus,
// the UDP reader goroutine calls HandleMeterPacket, and discovery may be
// republished on reconnect. All shared state is guarded by mu.
type Bridge struct {
	cfg    Config
	pub    Publisher
	log    Logger
	gate   *Gate
	meters *flexradio.MeterRegistry

	mu        sync.RWMutex
	device    ha.Device // set on first successful connect
	interlock flexradio.InterlockStatus
	slices    map[int]flexradio.SliceStatus
	txPower   int
	tunePower int
	tuStatus  flexradio.ATUStatus

	// discoveredMeters tracks which HA discovery configs we've already
	// published, so we only emit them once per (re)connect cycle.
	discoDone   bool
	discoSlices map[int]bool // slice indices we've emitted per-slice discovery for
}

// Config is the subset of config the bridge needs.
type Config struct {
	Serial          string
	StatePrefix     string // e.g. "flexbridge"
	DiscoveryPrefix string // e.g. "homeassistant"
	AvailTopic      string // LWT topic for the bridge
	Rates           map[flexradio.MeterGroup]time.Duration
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
		cfg:         cfg,
		pub:         pub,
		log:         log,
		gate:        NewGate(cfg.Rates),
		meters:      flexradio.NewMeterRegistry(),
		slices:      make(map[int]flexradio.SliceStatus),
		discoSlices: make(map[int]bool),
	}
}

// SetDevice records the radio identity (called after discovery / handshake).
// Triggers HA discovery publication once.
func (b *Bridge) SetDevice(d ha.Device) {
	b.mu.Lock()
	b.device = d
	b.discoDone = false
	b.discoSlices = make(map[int]bool)
	b.mu.Unlock()
	b.PublishDiscovery()
}

// Reset clears runtime radio state on disconnect/reconnect so stale values
// aren't carried over and the gate forces republish.
func (b *Bridge) Reset() {
	b.mu.Lock()
	b.meters.Reset()
	b.slices = make(map[int]flexradio.SliceStatus)
	b.interlock = flexradio.InterlockStatus{}
	b.txPower = 0
	b.tunePower = 0
	b.tuStatus = flexradio.ATUStatus{}
	b.discoDone = false
	b.discoSlices = make(map[int]bool)
	b.mu.Unlock()
	b.gate.Reset()
}

// MaybePublishSliceDiscovery emits HA discovery for any slices we've learned
// about but not yet announced. Slices are dynamic, so discovery is published
// lazily as slices appear (driven by the "slice" status handler in main).
func (b *Bridge) MaybePublishSliceDiscovery() {
	b.mu.Lock()
	if b.device.Serial == "" {
		b.mu.Unlock()
		return
	}
	d := b.device
	pending := make([]int, 0)
	for idx := range b.slices {
		if !b.discoSlices[idx] {
			pending = append(pending, idx)
		}
	}
	b.mu.Unlock()

	nodeID := ha.NodeID(d.Serial)
	for _, idx := range pending {
		b.publishSliceDiscovery(d, nodeID, idx)
		b.mu.Lock()
		b.discoSlices[idx] = true
		b.mu.Unlock()
	}
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
	case "meter":
		b.handleMeterDef(f)
	}
}

// HandleReply routes a parsed SmartSDR reply frame (R<h>|<seq>|<body>).
// Currently only the "meter list" reply is handled: its body is a packed
// list of meter definitions which we use to populate the meter index map.
func (b *Bridge) HandleReply(f flexradio.Frame) {
	body := f.Body
	// Reply bodies look like "<seq>|<body>"; strip the leading seq field if
	// present so body starts at the actual content.
	if i := strings.IndexByte(body, '|'); i >= 0 {
		body = body[i+1:]
	}
	if !strings.Contains(body, "meter") || !strings.Contains(body, ".src=") {
		return // not the meter list reply
	}
	entries := flexradio.ParseMeterListReply(body)
	if len(entries) == 0 {
		return
	}
	b.mu.Lock()
	b.meters.Reset()
	registered := 0
	for _, e := range entries {
		if b.meters.Register(e.Index, e.Source, e.SourceNum, e.Name) {
			registered++
		}
	}
	b.mu.Unlock()
	b.log.Infof("meter list: %d entries, %d published", len(entries), registered)
}

// handleInterlock updates transmit state and publishes the binary_sensor.
func (b *Bridge) handleInterlock(f flexradio.Frame) {
	is := flexradio.ParseInterlock(joinArgsFields(f))
	b.mu.Lock()
	prev := b.interlock
	b.interlock = is
	b.mu.Unlock()

	if prev.Transmitting != is.Transmitting {
		b.publishTransmitting(is)
	}
}

// handleSlice updates per-slice state and publishes the changed status
// fields (frequency, mode, filter, agc).
func (b *Bridge) handleSlice(f flexradio.Frame) {
	s, err := flexradio.ParseSlice(f.TopicArgs, fieldsString(f))
	if err != nil {
		b.log.Warnf("parse slice: %v", err)
		return
	}
	b.mu.Lock()
	prev := b.slices[s.Index]
	b.slices[s.Index] = s
	b.mu.Unlock()

	b.publishSliceDiff(prev, s)
	b.maybePublishActiveSlice(prev, s)
}

// maybePublishActiveSlice updates the active/frequency and active/band topics
// when the active slice changes or its frequency moves.
func (b *Bridge) maybePublishActiveSlice(prev, cur flexradio.SliceStatus) {
	if cur.Active && (prev.FreqHz != cur.FreqHz || !prev.Active) {
		// This slice is active and its frequency changed, or it just became active.
		b.publishActiveFreqBand(cur)
		return
	}
	if prev.Active && !cur.Active {
		// This slice just became inactive; find the new active slice.
		b.mu.RLock()
		var active *flexradio.SliceStatus
		for _, s := range b.slices {
			if s.Active {
				sc := s
				active = &sc
				break
			}
		}
		b.mu.RUnlock()
		if active != nil {
			b.publishActiveFreqBand(*active)
		}
	}
}

// publishActiveFreqBand publishes the active-slice frequency and band summary.
func (b *Bridge) publishActiveFreqBand(s flexradio.SliceStatus) {
	mhz := float64(s.FreqHz) / 1e6
	_ = b.pub.Publish(b.statusTopic("active/frequency"), true, []byte(formatFloat(mhz)))
	_ = b.pub.Publish(b.statusTopic("active/band"), true, []byte(flexradio.BandForFreq(s.FreqHz)))
}

// handleATU updates the ATU status sensor.
func (b *Bridge) handleATU(f flexradio.Frame) {
	st := flexradio.ParseATU(joinArgsFields(f))
	b.mu.Lock()
	prev := b.tuStatus
	b.tuStatus = st
	b.mu.Unlock()

	if prev.Status != st.Status {
		topic := b.statusTopic("atu")
		_ = b.pub.Publish(topic, true, []byte(st.Status))
	}
}

// handleRadio updates radio-wide fields like tx_power.
func (b *Bridge) handleRadio(f flexradio.Frame) {
	fs := fieldsString(f)
	if power, ok := flexradio.ParseTxPower(fs); ok {
		b.mu.Lock()
		prev := b.txPower
		b.txPower = power
		b.mu.Unlock()
		if prev != power {
			topic := b.statusTopic("tx_power")
			_ = b.pub.Publish(topic, true, []byte(strconv.Itoa(power)))
		}
	}
	if power, ok := flexradio.ParseTunePower(fs); ok {
		b.mu.Lock()
		prev := b.tunePower
		b.tunePower = power
		b.mu.Unlock()
		if prev != power {
			topic := b.statusTopic("tune_power")
			_ = b.pub.Publish(topic, true, []byte(strconv.Itoa(power)))
		}
	}
}

// handleMeterDef rebuilds the meter index map from "meter N" status lines.
// The body looks like: "0 0 TX=FWDPWR" or "5 0 SLC=LEVEL num=0".
func (b *Bridge) handleMeterDef(f flexradio.Frame) {
	// Format: "<index> <num> <SOURCE>=<name>". TopicArgs holds "index num"
	// as bare words; the fields portion has "SOURCE=name" plus more.
	rawArgs := splitFields(f.TopicArgs)
	if len(rawArgs) < 2 {
		return
	}
	idx, err := strconv.Atoi(rawArgs[0])
	if err != nil {
		return
	}
	// Second positional word is the source-internal num: for SLC meters
	// this is the slice index; for others it's a manifest slot.
	sourceNum := 0
	if len(rawArgs) > 1 {
		if n, err := strconv.Atoi(rawArgs[1]); err == nil {
			sourceNum = n
		}
	}
	// Find the SOURCE=name token in fields.
	var source, name string
	for _, tok := range splitFields(fieldsString(f)) {
		for _, src := range []string{"SLC=", "TX-=", "RAD=", "COD-=", "AMP="} {
			if startsWith(tok, src) {
				source = src[:len(src)-1]
				name = tok[len(src):]
			}
		}
	}
	if source == "" || name == "" {
		return
	}
	b.mu.Lock()
	registered := b.meters.Register(uint16(idx), source, sourceNum, name)
	b.mu.Unlock()
	if registered {
		b.log.Debugf("registered meter %d = %s/%s", idx, source, name)
	}
}

// ------------------------------------------------------------------
// Meter packet handling (VITA-49 UDP)
// ------------------------------------------------------------------

// HandleMeterPacket decodes one VITA-49 datagram and publishes the meters
// it contains (subject to throttle/dedup and TX gating).
func (b *Bridge) HandleMeterPacket(data []byte) {
	p, err := flexradio.ParseVITA49(data)
	if err != nil {
		return // malformed; ignore quietly (non-meter or garbage)
	}
	readings, err := p.MeterReadings()
	if err != nil {
		return
	}
	b.mu.RLock()
	transmitting := b.interlock.Transmitting
	b.mu.RUnlock()

	for _, r := range readings {
		b.handleReading(r, transmitting)
	}
}

// handleReading converts, gates, throttles, and publishes one meter reading.
func (b *Bridge) handleReading(r flexradio.MeterReading, transmitting bool) {
	b.mu.RLock()
	rm, ok := b.meters.LookupIndex(r.Index)
	b.mu.RUnlock()
	if !ok {
		return // not a meter we publish
	}
	def := rm.Definition()

	// TX gating: TX-chain meters only make sense while actively transmitting.
	// Publishing them while receiving yields full-scale garbage.
	if def.Group == flexradio.GroupTX || def.Group == flexradio.GroupAudio {
		if !transmitting {
			return
		}
	}

	value, unit := flexradio.ConvertRaw(def.Unit, r.Raw, def)
	topic := b.meterTopic(def, rm.SourceNum())

	if !b.gate.Allow(topic, def.Group, unit, value) {
		return
	}
	_ = b.pub.Publish(topic, false, []byte(formatValue(value, unit)))
}

// ------------------------------------------------------------------
// Publishing helpers
// ------------------------------------------------------------------

// publishTransmitting emits the transmitting binary_sensor state.
func (b *Bridge) publishTransmitting(is flexradio.InterlockStatus) {
	topic := b.statusTopic("transmitting")
	payload := "RECEIVING"
	if is.Transmitting {
		payload = "TRANSMITTING"
	}
	_ = b.pub.Publish(topic, true, []byte(payload))
}

// publishSliceDiff publishes only the per-slice status fields that changed.
// Band is derived from frequency (the radio exposes no band field), so it is
// republished whenever the resolved band label changes.
func (b *Bridge) publishSliceDiff(prev, cur flexradio.SliceStatus) {
	base := fmt.Sprintf("slice/%d/", cur.Index)
	if prev.FreqHz != cur.FreqHz {
		mhz := float64(cur.FreqHz) / 1e6
		_ = b.pub.Publish(b.statusTopic(base+"frequency"), true, []byte(formatFloat(mhz)))
		// Band follows frequency; only republish when the band label actually
		// changes (not on every Hz of drift within a band).
		if flexradio.BandForFreq(prev.FreqHz) != flexradio.BandForFreq(cur.FreqHz) {
			_ = b.pub.Publish(b.statusTopic(base+"band"), true, []byte(flexradio.BandForFreq(cur.FreqHz)))
		}
	}
	if prev.Mode != cur.Mode {
		_ = b.pub.Publish(b.statusTopic(base+"mode"), true, []byte(cur.Mode))
	}
	if prev.AGCMode != cur.AGCMode {
		_ = b.pub.Publish(b.statusTopic(base+"agc"), true, []byte(cur.AGCMode))
	}
	if prev.FilterLow != cur.FilterLow {
		_ = b.pub.Publish(b.statusTopic(base+"filter_low"), true, []byte(strconv.Itoa(cur.FilterLow)))
	}
	if prev.FilterHigh != cur.FilterHigh {
		_ = b.pub.Publish(b.statusTopic(base+"filter_high"), true, []byte(strconv.Itoa(cur.FilterHigh)))
	}
	if prev.Active != cur.Active {
		_ = b.pub.Publish(b.statusTopic(base+"active"), true, []byte(boolStr(cur.Active)))
	}
}

// PublishDiscovery emits HA discovery for all entities. Safe to call
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

	// 1. Meter entities (one per wanted meter def).
	for _, def := range wantedMeters() {
		objectID := def.ObjectID
		// Per-slice meters get a per-slice object id at publish time; for
		// discovery we use a generic name. HA creates one entity that all
		// slices publish to? No — we need per-slice discovery. But slices
		// are dynamic. Compromise: emit discovery for slice 0..3 up front.
		// Simpler: emit a per-meter generic entity; per-slice topics embed
		// the slice index and we emit discovery lazily when a slice appears.
		if def.Source == flexradio.SourceSlice {
			// Defer slice-meter discovery to when slices are known.
			continue
		}
		topic := b.meterTopic(def, 0)
		cfg, comp := ha.MeterEntity(def, d, topic, objectID, b.cfg.AvailTopic)
		discoTopic := ha.ConfigTopic(b.cfg.DiscoveryPrefix, comp, nodeID, objectID)
		_ = publishDiscovery(b.pub, discoTopic, cfg)
	}

	// 2. Status entities (radio-wide).
	b.publishStatusDiscovery(d, nodeID)

	// Per-slice discovery is published lazily by MaybePublishSliceDiscovery
	// as slices appear (they're dynamic). Emit for any already known here.
	b.MaybePublishSliceDiscovery()
}

// publishStatusDiscovery emits the radio-wide status entities.
func (b *Bridge) publishStatusDiscovery(d ha.Device, nodeID string) {
	// Transmitting binary_sensor.
	cfg, comp := ha.BinaryEntity("Transmitting", "transmitting",
		b.statusTopic("transmitting"), "TRANSMITTING", "RECEIVING", d, b.cfg.AvailTopic)
	_ = publishDiscovery(b.pub, ha.ConfigTopic(b.cfg.DiscoveryPrefix, comp, nodeID, "transmitting"), cfg)

	// TX power.
	cfg, comp = ha.StatusEntity("TX Power", "tx_power", b.statusTopic("tx_power"), "W", d, b.cfg.AvailTopic)
	_ = publishDiscovery(b.pub, ha.ConfigTopic(b.cfg.DiscoveryPrefix, comp, nodeID, "tx_power"), cfg)

	// Tune power (carrier level during ATU/tune).
	cfg, comp = ha.StatusEntity("Tune Power", "tune_power", b.statusTopic("tune_power"), "W", d, b.cfg.AvailTopic)
	_ = publishDiscovery(b.pub, ha.ConfigTopic(b.cfg.DiscoveryPrefix, comp, nodeID, "tune_power"), cfg)

	// ATU status (string).
	cfg, comp = ha.StatusEntity("ATU Status", "atu", b.statusTopic("atu"), "", d, b.cfg.AvailTopic)
	_ = publishDiscovery(b.pub, ha.ConfigTopic(b.cfg.DiscoveryPrefix, comp, nodeID, "atu"), cfg)

	// Active slice summary: single frequency and band for antenna/PA control.
	cfg, comp = ha.StatusEntity("Active Frequency", "active_frequency", b.statusTopic("active/frequency"), "MHz", d, b.cfg.AvailTopic)
	_ = publishDiscovery(b.pub, ha.ConfigTopic(b.cfg.DiscoveryPrefix, comp, nodeID, "active_frequency"), cfg)

	cfg, comp = ha.StatusEntity("Active Band", "active_band", b.statusTopic("active/band"), "", d, b.cfg.AvailTopic)
	_ = publishDiscovery(b.pub, ha.ConfigTopic(b.cfg.DiscoveryPrefix, comp, nodeID, "active_band"), cfg)
}

// publishSliceDiscovery emits discovery for one slice's status fields and
// per-slice meters. Called when a slice first appears.
func (b *Bridge) publishSliceDiscovery(d ha.Device, nodeID string, sliceIdx int) {
	prefix := fmt.Sprintf("slice_%d_", sliceIdx)
	base := fmt.Sprintf("slice/%d/", sliceIdx)

	// Status fields.
	statusFields := []struct {
		suffix, name, unit string
	}{
		{"frequency", "Frequency", "MHz"},
		{"band", "Band", ""}, // derived from frequency by flexbridge
		{"mode", "Mode", ""},
		{"agc", "AGC Mode", ""},
		{"filter_low", "Filter Low", "Hz"},
		{"filter_high", "Filter High", "Hz"},
		{"active", "Active", ""},
	}
	for _, sf := range statusFields {
		cfg, comp := ha.StatusEntity(
			fmt.Sprintf("Slice %d %s", sliceIdx, sf.name),
			prefix+sf.suffix,
			b.statusTopic(base+sf.suffix),
			sf.unit, d, b.cfg.AvailTopic,
		)
		_ = publishDiscovery(b.pub, ha.ConfigTopic(b.cfg.DiscoveryPrefix, comp, nodeID, prefix+sf.suffix), cfg)
	}

	// Per-slice meters.
	for _, def := range wantedMeters() {
		if def.Source != flexradio.SourceSlice {
			continue
		}
		topic := b.meterTopic(def, sliceIdx)
		objectID := fmt.Sprintf("slice_%d_%s", sliceIdx, def.ObjectID)
		cfg, comp := ha.MeterEntity(def, d, topic, objectID, b.cfg.AvailTopic)
		_ = publishDiscovery(b.pub, ha.ConfigTopic(b.cfg.DiscoveryPrefix, comp, nodeID, objectID), cfg)
	}
}

// ------------------------------------------------------------------
// Topic helpers
// ------------------------------------------------------------------

func (b *Bridge) stateBase() string {
	return b.cfg.StatePrefix + "/" + b.device.Serial
}

// statusTopic builds a retained state topic: <state_prefix>/<serial>/state/<rel...>
func (b *Bridge) statusTopic(rel string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.stateBase() + "/state/" + rel
}

// meterTopic builds a non-retained meter topic. sourceNum disambiguates
// per-slice meters.
//
//	<state_prefix>/<serial>/meter/<bucket>/[<slice>/]<object_id>
func (b *Bridge) meterTopic(def flexradio.MeterDef, sourceNum int) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	bucket := string(def.Group)
	var topic string
	if def.Source == flexradio.SourceSlice {
		topic = fmt.Sprintf("%s/meter/%s/%d/%s", b.stateBase(), bucket, sourceNum, def.ObjectID)
	} else {
		topic = fmt.Sprintf("%s/meter/%s/%s", b.stateBase(), bucket, def.ObjectID)
	}
	return topic
}

// ------------------------------------------------------------------
// Internal helpers (free functions)
// ------------------------------------------------------------------

// wantedMeters returns the static wanted-meter list. Wrapped so tests can
// access it without import cycles.
func wantedMeters() []flexradio.MeterDef {
	// flexradio doesn't export wantedMeters; we list the defs here to keep
	// the bridge self-contained and to drive discovery. Keep in sync with
	// flexradio.wantedMeters.
	return flexradio.WantedMeterDefs()
}

// joinArgsFields reconstructs "args fields" for the parsers that take a
// single string.
func joinArgsFields(f flexradio.Frame) string {
	return f.TopicArgs + " " + fieldsString(f)
}

// fieldsString rebuilds the "key=value key=value" portion from f.Fields.
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

// splitFields tokenizes on whitespace.
func splitFields(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func formatValue(v float64, unit string) string {
	// SWR and ratio-like values: 2 decimals. dB: 1 decimal. Watts: 1 decimal.
	switch unit {
	case "SWR":
		return formatFloatPrec(v, 2)
	case "dBm", "dB", "dBFS":
		return formatFloatPrec(v, 1)
	case "W", "Watts":
		return formatFloatPrec(v, 1)
	case "V", "Volts", "A", "Amps":
		return formatFloatPrec(v, 2)
	case "°C", "degC":
		return formatFloatPrec(v, 1)
	default:
		return formatFloatPrec(v, 2)
	}
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

func formatFloatPrec(v float64, prec int) string {
	return strconv.FormatFloat(v, 'f', prec, 64)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
