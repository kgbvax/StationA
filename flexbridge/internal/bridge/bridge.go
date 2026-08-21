package bridge

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"flexbridge/internal/flexradio"
	"flexbridge/internal/ha"

	schema "codeberg.org/kgbvax/stationa/shared/schema"
)

// bandTransitionHold is the window after a set_band command during which
// slice-derived band changes are suppressed unless they match the target band.
// SmartSDR changes the panadapter band immediately, but the slice retunes
// asynchronously and emits intermediate status frames carrying the old
// frequency (now outside the target band). Without the hold, flexbridge
// derives a transient wrong band (e.g. 40m) and publishes it, causing
// antenna-select to chatter to the fallback antenna for a few milliseconds.
var bandTransitionHold = 750 * time.Millisecond

type bandTransition struct {
	target    string    // canonical band label we are waiting for, e.g. "15m"
	deadline  time.Time // hold expires here even if the pan never confirms
	confirmed bool      // true once a tracked pan reports band==target
}

// Bridge owns the radio state model and translates radio events (TCP
// status frames) into MQTT publishes following the station integration model.
// All shared state is guarded by mu.
type Bridge struct {
	cfg Config
	pub Publisher
	log Logger

	mu          sync.RWMutex
	device      ha.Device
	commander   Commander // current radio command surface (nil while radio offline)
	interlock   flexradio.InterlockStatus
	slices      map[int]flexradio.SliceStatus
	sliceBand   map[int]string                 // per-slice last-derived band, used as the hysteresis prev for that slice
	pans        map[string]flexradio.PanStatus // panadapters tracked via `sub pan all`, keyed by stream-id handle
	micProfiles map[string]struct{}            // mic profile names from `profile mic info` → `profile mic list=` status frames
	tuStatus    flexradio.ATUStatus
	state       radioState
	bandTx      *bandTransition // nil when no band transition is in progress

	discoDone bool
}

// Commander is the radio control surface the bridge drives from /cmd. The
// *flexradio.Client implements it; tests use a fake. Aliased here so the
// bridge package owns the /cmd dispatch without re-declaring the interface
// (same pattern as acom1200s-pa-bridge's `type Commander = acom.Commander`).
type Commander = flexradio.Commander

// cmdPayload is the /cmd JSON the bridge accepts, aliased to the shared
// convention (the argument rides under the `value` key, never under a key
// named after the action). Same pattern as acom1200s-pa-bridge.
type cmdPayload = schema.CmdPayload

// radioState is the mutable radio state held under Bridge.mu.
type radioState struct {
	freqHz         int64
	band           string
	mode           string   // canonical (NormalizeMode applied)
	txing          bool     // true while interlock.Transmitting
	tuning         bool     // true while ATU or radio is in tuning state
	drive          int      // 0-100 transmit drive level
	deviceOnline   bool     // true while the radio TCP link is up (handshake done)
	dvkStatus      string   // DVK: idle|recording|preview|playback|disabled (omitempty via statePayload)
	dvkID          int      // DVK: active memory id (cleared on idle/disabled)
	micProfile     string   // mic profile currently loaded (active name; omitempty)
	micProfileList []string // sorted available mic profile names (omitempty)
}

// statePayload is the JSON shape published to <slot>/state (retained).
// band and mode are omitted (not published raw or as placeholders) when the
// frequency is unknown or the firmware mode has no canonical mapping.
// device_online is always present (true/false) so consumers can distinguish a
// live radio from a frozen snapshot left over from a disconnect — /status is
// the MQTT/LWT bridge liveness, not the radio link.
type statePayload struct {
	TS           string   `json:"ts"`
	FreqHz       int64    `json:"freq_hz"`
	Band         string   `json:"band,omitempty"`
	Mode         string   `json:"mode,omitempty"`
	TX           string   `json:"tx"` // "rx" | "tx"
	Tuning       bool     `json:"tuning"`
	Drive        int      `json:"drive"`                  // 0-100
	DeviceOnline bool     `json:"device_online"`          // radio link liveness
	DVKStatus    string   `json:"dvk_status,omitempty"`   // DVK operation (SmartSDR v4+)
	DVKID        int      `json:"dvk_id,omitempty"`       // active DVK memory id
	MicProfile   string   `json:"mic_profile,omitempty"`  // active mic profile name (SmartSDR native profile)
	MicProfiles  []string `json:"mic_profiles,omitempty"` // available mic profile names (dynamic; /state only)
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
// primitives. Radio tuning state is read-only except for band, which is a writable
// setpoint (set_band drives SmartSDR native band-stacking; /state.band stays derived
// from freq_hz). DVK playback is exposed as one-shot actions (buttons).
type metaExpose struct {
	Device  metaExposeDevice   `json:"device"`
	Fields  []metaExposeField  `json:"fields"`
	Actions []metaExposeAction `json:"actions,omitempty"`
}

// metaExposeAction describes a one-shot button (integration model Appendix C).
// Most actions are action-only: pressing the button publishes
// {"action":"<Action>"}. A value-carrying action adds a value_key so the
// consumer sends {"action":"<Action>","value":"<value>"}.
type metaExposeAction struct {
	Key     string       `json:"key"`
	Name    string       `json:"name"`
	Command *metaCommand `json:"command"`
}

// metaCommand describes how a write is encoded on /cmd (Appendix C). For DVK
// actions the command is action-only (no value_key): the memory index is
// encoded in the action name itself (dvk_play_1 .. dvk_play_12).
type metaCommand struct {
	Action    string `json:"action,omitempty"`
	ValueKey  string `json:"value_key,omitempty"`
	ValueType string `json:"value_type,omitempty"`
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
// writable + command mark a field as a setpoint: a consumer renders a control (e.g. a
// select for an enum) that publishes command on /cmd. The field's /state value may
// still be derived — e.g. band is writable (set_band triggers native band-stacking)
// yet /state.band remains derived from freq_hz (model §4 invariant).
type metaExposeField struct {
	Key        string       `json:"key"`
	Name       string       `json:"name"`
	Type       string       `json:"type"`
	Unit       string       `json:"unit,omitempty"`
	Class      string       `json:"class,omitempty"`
	StateClass string       `json:"state_class,omitempty"`
	OptionsRef string       `json:"options_ref,omitempty"`
	On         string       `json:"on,omitempty"`
	Off        string       `json:"off,omitempty"`
	Writable   bool         `json:"writable,omitempty"`
	Command    *metaCommand `json:"command,omitempty"`
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
		cfg:         cfg,
		pub:         pub,
		log:         log,
		slices:      make(map[int]flexradio.SliceStatus),
		sliceBand:   make(map[int]string),
		pans:        make(map[string]flexradio.PanStatus),
		micProfiles: make(map[string]struct{}),
	}
}

// SetCommander installs the radio command surface used to dispatch /cmd
// intent. Called from runOnce after a successful handshake (the *Client is
// per-connect-cycle, so the Commander is injected rather than passed in
// Config). Pass nil when the radio link is down so HandleCommand no-ops.
func (b *Bridge) SetCommander(c Commander) {
	b.mu.Lock()
	b.commander = c
	b.mu.Unlock()
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
	b.sliceBand = make(map[int]string)
	b.pans = make(map[string]flexradio.PanStatus)
	b.micProfiles = make(map[string]struct{})
	b.interlock = flexradio.InterlockStatus{}
	b.tuStatus = flexradio.ATUStatus{}
	b.state = radioState{} // deviceOnline defaults false
	b.commander = nil      // radio link down: /cmd must no-op
	b.bandTx = nil         // any pending band transition is abandoned
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
	case "display": // SmartSDR panadapter status topic is the two-word "display pan"
		b.handlePan(f)
	case "atu":
		b.handleATU(f)
	case "radio":
		b.handleRadio(f)
	case "dvk":
		b.handleDVK(f)
	case "profile": // SmartSDR mic profile list/active (reply to `profile mic info`)
		b.handleProfile(f)
	}
}

// handleProfile tracks mic profiles from "profile" status frames. SmartSDR does
// not broadcast profiles via subscriptions; the bridge queries the list by
// sending `profile mic info` (in the handshake and after a save), and the radio
// replies with a `profile mic list=Name^Other Name^…` status frame — an
// authoritative full snapshot of the available mic profiles (caret-delimited;
// names may contain spaces). A `profile mic current=<name>` frame would carry
// the active profile, but SmartSDR does not emit one for mic profiles (mic
// profiles are load-only presets with no "current" pointer, unlike global
// profiles), so the active mic name is tracked client-side as "last loaded via
// set_mic_profile" (see HandleCommand). Non-mic profile types (global/transmit)
// and the importing=/exporting= transfer flags are ignored.
func (b *Bridge) handleProfile(f flexradio.Frame) {
	ps := flexradio.ParseProfile(f.RawBody)
	if ps.Type != "mic" {
		return // only mic profiles are tracked
	}
	if ps.IsList {
		// Full-snapshot replacement: rebuild the known set from the list.
		newSet := make(map[string]struct{}, len(ps.Names))
		for _, n := range ps.Names {
			newSet[n] = struct{}{}
		}
		newList := sortedKeys(newSet)
		b.mu.Lock()
		listChanged := !stringSliceEqual(b.state.micProfileList, newList)
		// If the client-tracked active name is no longer in the list, drop it.
		activeChanged := false
		if b.state.micProfile != "" {
			if _, ok := newSet[b.state.micProfile]; !ok {
				b.state.micProfile = ""
				activeChanged = true
			}
		}
		b.micProfiles = newSet
		b.state.micProfileList = newList
		snap := b.state
		b.mu.Unlock()
		if listChanged || activeChanged {
			b.publishStateSnapshot(snap)
		}
		return
	}
	if ps.IsCurrent {
		// Defensive: mic does not emit current= on current firmware, but honor
		// it if a revision does, so the active name follows the radio.
		b.mu.Lock()
		changed := b.state.micProfile != ps.Current
		if changed {
			b.state.micProfile = ps.Current
		}
		snap := b.state
		b.mu.Unlock()
		if changed {
			b.publishStateSnapshot(snap)
		}
	}
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// handleDVK updates the DVK state from a "dvk" status frame (SmartSDR v4+,
// subscribed via `sub dvk all`). Only status= frames carry state; added/deleted
// memory-library frames are ignored. idle/disabled clears the active id.
func (b *Bridge) handleDVK(f flexradio.Frame) {
	ds := flexradio.ParseDVK(joinArgsFields(f))
	if !ds.HasStatus {
		b.log.Debugf("dvk non-status frame: %s", joinArgsFields(f))
		return
	}
	b.mu.Lock()
	// idle/disabled ⇒ no active memory; clear the id even if the frame carries one.
	newID := ds.ID
	if ds.Status == "idle" || ds.Status == "disabled" {
		newID = 0
	}
	changed := b.state.dvkStatus != ds.Status || b.state.dvkID != newID
	if changed {
		b.state.dvkStatus = ds.Status
		b.state.dvkID = newID
	}
	snap := b.state
	b.mu.Unlock()

	if changed {
		b.publishStateSnapshot(snap)
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

	raw := fieldsString(f)
	s, err := flexradio.ParseSlice(f.TopicArgs, raw, prev)
	if err != nil {
		b.log.Warnf("parse slice: %v", err)
		return
	}

	b.mu.Lock()
	if isSliceRemoval(f) {
		// SmartSDR signals slice removal either as a bare "removed" topic-arg
		// (S|slice <n> <r> removed) or via in_use=0 / removed=1. The bridge must
		// delete the entry; otherwise a stale slice with TX=true or Active=true
		// lingers forever (the map is only cleared on Reset), and
		// resolveActiveSlice would keep selecting a phantom slice — publishing
		// its frozen frequency and jumping the bus between the phantom and the
		// real slice on every frame (Go map iteration order is randomized).
		delete(b.slices, idx)
		delete(b.sliceBand, idx)
	} else {
		b.slices[s.Index] = s
		// Track each slice's band with its OWN previous band as hysteresis prev,
		// updated on every frame for that slice (active or not). Using the
		// single global b.state.band as the hysteresis prev meant switching the
		// active slice between two slices clobbered the held band of an
		// edge-dwelling slice — re-exposing the band-edge chatter that
		// BandEdgeHysteresisHz exists to prevent.
		b.sliceBand[s.Index] = flexradio.BandForFreqWithPrev(s.FreqHz, b.sliceBand[s.Index])
	}
	changed := b.updateActiveSliceState()
	snap := b.state
	b.mu.Unlock()

	if changed {
		b.log.Debugf("slice update idx=%d raw=%q -> freq_hz=%d band=%q mode=%q tx=%v",
			idx, raw, snap.freqHz, snap.band, snap.mode, snap.txing)
		b.publishStateSnapshot(snap)
	}
}

// isSliceRemoval reports whether a slice status frame signals that the slice
// has been removed/torn down. SmartSDR encodings handled (confirm the live
// format with a capture; both are covered):
//   - a bare "removed" trailing topic-arg:  S|slice <n> <r> removed
//   - in_use=0 (slice no longer in use):   S|slice <n> <r> in_use=0 ...
//   - removed=1 (explicit flag):           S|slice <n> <r> removed=1
func isSliceRemoval(f flexradio.Frame) bool {
	for _, a := range strings.Fields(f.TopicArgs) {
		if a == "removed" {
			return true
		}
	}
	if v, ok := f.Fields["in_use"]; ok && v == "0" {
		return true
	}
	if v, ok := f.Fields["removed"]; ok && v == "1" {
		return true
	}
	return false
}

// handlePan tracks panadapters from "pan" status frames (subscribed via
// `sub pan all`). The pan stream-id handle is the leading topic arg and is
// kept as a raw hex string — `display pan s <handle> band=N` takes it verbatim.
// The bridge needs the handle to drive band changes; band/center are tracked
// for observability and to confirm a band change took effect. Pan removal is
// signalled the same way as slice removal (bare "removed" arg or in_use=0).
func (b *Bridge) handlePan(f flexradio.Frame) {
	// The "display" topic covers pan/panafall/panf; only "pan" carries the
	// panadapter handle we need for band changes.
	args := strings.Fields(f.TopicArgs)
	if len(args) == 0 || args[0] != "pan" {
		return
	}
	if isPanRemoval(f) {
		handle := panHandleArg(f)
		if handle == "" {
			return
		}
		b.mu.Lock()
		_, existed := b.pans[handle]
		delete(b.pans, handle)
		b.mu.Unlock()
		if existed {
			b.log.Infof("panadapter removed handle=%s", handle)
		}
		return
	}
	p := flexradio.ParsePan(f.TopicArgs, fieldsString(f))
	if p.Handle == "" {
		return
	}
	b.mu.Lock()
	_, existed := b.pans[p.Handle]
	b.pans[p.Handle] = p
	// A tracked panadapter reporting the target band confirms a commanded band
	// change and releases the transition hold early. This prevents the hold from
	// suppressing legitimate retunes once the new band is reached.
	if b.bandTx != nil && p.Band != 0 {
		if bandLabel, ok := flexradio.BandLabelForNumber(p.Band); ok && bandLabel == b.bandTx.target {
			b.bandTx.confirmed = true
		}
	}
	b.mu.Unlock()
	if !existed {
		// Info-level so a default (info) log confirms the pan subscription is
		// delivering without bumping log.level to debug.
		b.log.Infof("panadapter tracked handle=%s center_hz=%d", p.Handle, p.CenterHz)
	} else {
		b.log.Debugf("pan update handle=%s center_hz=%d band=%d", p.Handle, p.CenterHz, p.Band)
	}
}

// panHandleArg returns the pan stream-id handle from a "display pan" frame's
// topic args ("pan <stream_id> ..."), or "" if absent.
func panHandleArg(f flexradio.Frame) string {
	args := strings.Fields(f.TopicArgs)
	if len(args) >= 2 && args[0] == "pan" {
		return args[1]
	}
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// isPanRemoval reports whether a pan status frame signals the panadapter has
// been torn down. Same encodings as slice removal (isSliceRemoval).
func isPanRemoval(f flexradio.Frame) bool {
	for _, a := range strings.Fields(f.TopicArgs) {
		if a == "removed" {
			return true
		}
	}
	if v, ok := f.Fields["in_use"]; ok && v == "0" {
		return true
	}
	if v, ok := f.Fields["removed"]; ok && v == "1" {
		return true
	}
	return false
}

// updateActiveSliceState updates b.state from the current TX/active slice.
// Must be called with b.mu held. Returns true if state changed.
func (b *Bridge) updateActiveSliceState() bool {
	active, ok := resolveActiveSlice(b.slices)
	if !ok {
		return false
	}
	newFreq := active.FreqHz
	// The hysteresis prev is this active slice's own tracked band, not the
	// global published band — see handleSlice for why.
	newBand := b.sliceBand[active.Index]
	newMode := flexradio.NormalizeMode(active.Mode)

	// Band-transition hold: after a set_band command, suppress slice-derived
	// band changes that don't match the target until the panadapter confirms the
	// new band or the hold times out. We still publish frequency/mode updates
	// that are inside the target band, and we always publish once the hold ends.
	if tx := b.bandTx; tx != nil {
		now := time.Now()
		if !tx.confirmed && now.Before(tx.deadline) {
			if newBand != tx.target {
				// Keep the previous band so downstream consumers do not act on the
				// transient old frequency. Frequency and mode are still allowed to
				// update if they belong to the target band.
				newBand = b.state.band
			}
		} else {
			// Hold expired or confirmed: clear it and accept the derived band.
			b.bandTx = nil
		}
	}

	if b.state.freqHz == newFreq && b.state.mode == newMode && b.state.band == newBand {
		return false
	}
	// A nonzero frequency that is reported as "gen" or "unknown" after hysteresis
	// means the radio is genuinely outside ham allocations (or sent a transient
	// invalid value). Warn so bad raw data is visible.
	if newFreq > 0 && (newBand == "unknown" || newBand == "gen") {
		b.log.Warnf("out-of-band frequency from radio: freq_hz=%d band=%q", newFreq, newBand)
	}
	b.state.freqHz = newFreq
	b.state.band = newBand
	b.state.mode = newMode
	return true
}

// resolveActiveSlice returns the slice that drives the radio state.
// Prefers the TX slice; falls back to the Active slice.
//
// Selection is DETERMINISTIC: among TX slices the lowest Index wins, and if
// none, among Active slices the lowest Index wins. Go map iteration order is
// randomized, so returning the "first" match found while ranging the map
// would make freq_hz/band/mode a coin flip per frame whenever two slices match
// the predicate — e.g. two RX panadapters both active=1 with no TX slice, or a
// stale TX slice left in the map (see isSliceRemoval). The lowest-index
// tiebreaker keeps the published state stable across reads.
func resolveActiveSlice(slices map[int]flexradio.SliceStatus) (flexradio.SliceStatus, bool) {
	var best flexradio.SliceStatus
	found := false
	for _, s := range slices {
		if s.TX && (!found || s.Index < best.Index) {
			best = s
			found = true
		}
	}
	if found {
		return best, true
	}
	for _, s := range slices {
		if s.Active && (!found || s.Index < best.Index) {
			best = s
			found = true
		}
	}
	if found {
		return best, true
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
// Command handling (/cmd → radio)
// ------------------------------------------------------------------

// HandleCommand parses a /cmd JSON payload and dispatches it to the radio
// via the Commander. Unknown/invalid actions are logged and dropped. No ack
// is published — consumers observe the result on /state (dvk_status/dvk_id,
// freq_hz/band/mode), per the stationa fire-and-observe plane discipline.
//
// Accepted actions (muehle/hf/radio/cmd, NOT retained — one-shot intents):
//
//	{"action":"dvk_play_<N>"}   play DVK memory N (1-12); keys TX
//	{"action":"dvk_play","value":"N"}  same, value form (scripts/Node-RED)
//	{"action":"dvk_stop","value":"N"}   stop memory N
//	{"action":"dvk_stop"}              stop the currently-active memory (from /state)
//	{"action":"set_band","value":"20m"} native band-stacking: radio restores the
//	                                   last-used freq/mode for that band
//	{"action":"set_mic_profile","value":"<name>"} load mic profile <name> (SmartSDR
//	                                   native profile mic load)
func (b *Bridge) HandleCommand(payload []byte) {
	var c cmdPayload
	if err := json.Unmarshal(payload, &c); err != nil {
		b.log.Warnf("cmd: parse: %v", err)
		return
	}
	b.mu.RLock()
	cmd := b.commander
	mode := b.state.mode
	activeID := b.state.dvkID
	b.mu.RUnlock()
	if cmd == nil {
		b.log.Warnf("cmd: radio offline (no commander)")
		return
	}

	switch {
	case c.Action == "dvk_play":
		id := parseDVKID(c.Value, 1, 12)
		if id == 0 {
			b.log.Warnf("cmd dvk_play: bad memory %q", c.Value)
			return
		}
		if !isVoiceMode(mode) {
			b.log.Debugf("cmd dvk_play: TX mode %q is not a voice mode; radio may refuse", mode)
		}
		if err := cmd.DVKPlay(id); err != nil {
			b.log.Warnf("cmd dvk_play: %v", err)
		}

	case strings.HasPrefix(c.Action, "dvk_play_"):
		id := parseDVKID(strings.TrimPrefix(c.Action, "dvk_play_"), 1, 12)
		if id == 0 {
			b.log.Warnf("cmd %s: bad memory", c.Action)
			return
		}
		if err := cmd.DVKPlay(id); err != nil {
			b.log.Warnf("cmd %s: %v", c.Action, err)
		}

	case c.Action == "dvk_stop":
		id := 0
		if c.Value != "" {
			id = parseDVKID(c.Value, 1, 12)
			if id == 0 {
				b.log.Warnf("cmd dvk_stop: bad memory %q", c.Value)
				return
			}
		} else {
			id = activeID // stop whatever is currently playing/recording
		}
		if id == 0 {
			b.log.Warnf("cmd dvk_stop: no memory given and none active")
			return
		}
		if err := cmd.DVKStop(id); err != nil {
			b.log.Warnf("cmd dvk_stop: %v", err)
		}

	case c.Action == "set_band":
		// Native SmartSDR band-stacking: change the band on a panadapter and the
		// radio restores the last-used frequency/mode for that band. The result
		// is observed on the slice/pan status stream (/state stays freq-derived).
		bandLabel := strings.ToLower(strings.TrimSpace(c.Value))
		bandNum, ok := flexradio.BandNumberFor(bandLabel)
		if !ok {
			b.log.Warnf("cmd set_band: unknown/unsupported band %q", c.Value)
			return
		}
		b.mu.RLock()
		handle, hasPan := b.targetPanHandle()
		b.mu.RUnlock()
		if !hasPan {
			b.log.Warnf("cmd set_band: no panadapter tracked (open a panadapter first)")
			return
		}
		b.log.Infof("cmd set_band: band=%s (n=%d) pan=%s", bandLabel, bandNum, handle)
		if err := cmd.SetBand(handle, bandNum); err != nil {
			b.log.Warnf("cmd set_band: %v", err)
			return
		}
		// Arm the band-transition hold. While active, slice-derived band changes
		// that do not match the target are suppressed so antennaselect does not
		// see the transient old frequency as a tuning intent. The hold is released
		// when a tracked panadapter reports the requested band, or when the
		// deadline expires (whichever comes first).
		b.mu.Lock()
		b.bandTx = &bandTransition{target: bandLabel, deadline: time.Now().Add(bandTransitionHold)}
		b.mu.Unlock()

	case c.Action == "set_mic_profile":
		// Native SmartSDR mic-profile load. The name is double-quoted on the
		// wire, so reject names containing quotes/control chars. If the tracked
		// mic-profile list is populated and the name isn't in it, drop as a
		// likely typo (an empty list — before the first `profile mic info`
		// response — does NOT block, since the list may simply not have arrived
		// yet).
		name := strings.TrimSpace(c.Value)
		if !validProfileName(name) {
			b.log.Warnf("cmd set_mic_profile: invalid name %q", c.Value)
			return
		}
		b.mu.RLock()
		_, known := b.micProfiles[name]
		listPopulated := len(b.micProfiles) > 0
		b.mu.RUnlock()
		if listPopulated && !known {
			b.log.Warnf("cmd set_mic_profile: %q is not a known mic profile", name)
			return
		}
		b.log.Infof("cmd set_mic_profile: %s", name)
		if err := cmd.SetMicProfile(name); err != nil {
			b.log.Warnf("cmd set_mic_profile: %v", err)
			return
		}
		// SmartSDR does not report the active mic profile, so track it
		// client-side as the name we just loaded (best-effort: assumes the load
		// succeeded; the known-name guard above makes a wrong load unlikely).
		b.mu.Lock()
		changed := b.state.micProfile != name
		if changed {
			b.state.micProfile = name
		}
		snap := b.state
		b.mu.Unlock()
		if changed {
			b.publishStateSnapshot(snap)
		}

	default:
		b.log.Warnf("cmd: unknown action %q", c.Action)
	}
}

// targetPanHandle resolves which panadapter a band change should target. It
// prefers the panadapter the active slice is on (when the slice status carried
// a pan handle), then the single tracked pan, then the lowest handle for
// determinism (Go map iteration is randomized). Returns ok=false when no
// panadapter is tracked (none open — the operator must open one via the GUI).
// Must be called with b.mu (at least RLock) held.
func (b *Bridge) targetPanHandle() (string, bool) {
	if active, ok := resolveActiveSlice(b.slices); ok && active.PanHandle != "" {
		if _, tracked := b.pans[active.PanHandle]; tracked {
			return active.PanHandle, true
		}
	}
	var lowest string
	for h := range b.pans {
		if lowest == "" || h < lowest {
			lowest = h
		}
	}
	return lowest, lowest != ""
}

// parseDVKID parses a DVK memory id string, returning 0 if it is empty or out
// of [min,max]. Used for both the value form ("3") and the per-memory action
// suffix form (dvk_play_3).
func parseDVKID(s string, min, max int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < min || n > max {
		return 0
	}
	return n
}

// isVoiceMode reports whether a canonical mode can carry voice (DVK is live
// only in voice modes; CW and data are refused by the radio). The bridge
// stays dumb — this is advisory only; the radio is authoritative.
func isVoiceMode(canonical string) bool {
	switch canonical {
	case "usb", "lsb", "am", "fm":
		return true
	}
	return false
}

// validProfileName reports whether s is an acceptable mic-profile name to send
// on the wire. The bridge wraps the name in double quotes (`profile mic load
// "<name>"`), so embedded quotes/backslashes and control characters are
// rejected to keep the wire command well-formed and avoid injection. Empty and
// over-long names are rejected too.
func validProfileName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r == '"' || r == '\\' || r < 0x20 {
			return false
		}
	}
	return true
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
		DVKStatus:    st.dvkStatus,
		DVKID:        st.dvkID,
		MicProfile:   st.micProfile,
		MicProfiles:  st.micProfileList,
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
		// Consumer-neutral field surface (model §3.1). Radio tuning state is
		// read-only except band and mic_profile, which are writable setpoints
		// (set_band → native band-stacking; set_mic_profile → native profile mic
		// load); every other field renders as a sensor/binary_sensor. The enum
		// options resolve via options_ref against the capabilities above
		// (bands/modes). The dynamic mic-profile list lives on /state
		// (mic_profiles) only — expose.fields has no array type, so it is not
		// declared here. HA-specific rendering lives in hadiscovery, not here.
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
				{Key: "band", Name: "Band", Type: "enum", OptionsRef: "bands", Writable: true,
					Command: &metaCommand{Action: "set_band", ValueKey: "value", ValueType: "string"}},
				{Key: "mode", Name: "Mode", Type: "enum", OptionsRef: "modes"},
				{Key: "drive", Name: "Drive", Type: "number", Unit: "%"},
				{Key: "tx", Name: "Transmitting", Type: "boolean", On: "tx", Off: "rx"},
				{Key: "tuning", Name: "Tuning", Type: "boolean"},
				{Key: "dvk_status", Name: "DVK Status", Type: "string"},
				{Key: "dvk_id", Name: "DVK Memory", Type: "number"},
				{Key: "mic_profile", Name: "Mic Profile", Type: "string", Writable: true,
					Command: &metaCommand{Action: "set_mic_profile", ValueKey: "value", ValueType: "string"}},
			},
			Actions: dvkExposeActions(),
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

// dvkExposeActions builds the expose action surface for the DVK trigger: one
// action per memory (dvk_play_1 .. dvk_play_12) plus dvk_stop. Each play action
// is action-only — pressing it publishes {"action":"dvk_play_<N>"} — so a
// consumer (hadiscovery → HA button) needs no value injection. dvk_stop with
// no value stops the currently-active memory (resolved by the bridge from
// /state at dispatch time).
func dvkExposeActions() []metaExposeAction {
	acts := make([]metaExposeAction, 0, 13)
	for n := 1; n <= 12; n++ {
		key := "dvk_play_" + strconv.Itoa(n)
		acts = append(acts, metaExposeAction{
			Key:     key,
			Name:    "DVK Play " + strconv.Itoa(n),
			Command: &metaCommand{Action: key},
		})
	}
	acts = append(acts, metaExposeAction{
		Key:     "dvk_stop",
		Name:    "DVK Stop",
		Command: &metaCommand{Action: "dvk_stop"},
	})
	return acts
}
