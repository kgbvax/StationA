package bridge

// The /state and /meta shapes (plan U5, R5–R8, KTD-3): one slot
// muehle/uhf/radio, one retained JSON snapshot carrying the hybrid VFO shape —
// top-level active-TX fields mirroring the TX VFO (SUB in satellite mode — SUB
// is the uplink/TX side; MAIN otherwise) plus main/sub detail objects — and
// the retained birth certificate with a read-only expose block.
//
// Per-state payload rules (R6): while a CI-V session is live every known
// field publishes; while idle/connecting/error the radio-measured fields are
// OMITTED (never zeroed, never frozen) and only ts, device_online:false,
// session_state, armed, error and the bridge-held selected_vfo remain.

import (
	"codeberg.org/kgbvax/stationa/shared/schema"

	"icom9700-radio-bridge/internal/civ"
	"icom9700-radio-bridge/internal/radio"
)

// deviceModel is the /meta.device identity. The bridge fronts exactly one
// device family, so the model is a constant (the model name lives in the
// bridge, not the config — the same posture as the role constant). No
// firmware read exists in v1 (bench pin), so /meta is published once at
// startup and never clobbered (the flexbridge placeholder lesson: never
// publish a partial identity over a good one — here there is nothing to
// read, so the startup publish is the final one).
const deviceModel = "Icom IC-9700"

// vfoCache is one VFO's last-known radio state. hasFreq/modeOK/hasDataMode
// gate the published fields (omitted, never zeroed); preamp/attenuator are
// bridge-held cmd echoes only — the per-band read scoping of 16 02 / 11 is a
// bench pin (codec.go), so v1 never polls them.
type vfoCache struct {
	hasFreq    bool
	freqHz     uint32
	band       string // derived from freqHz via the canonical table; "" when outside it
	mode       string // canonical; valid only when modeOK
	modeOK     bool   // false when the radio reports RTTY/DV/DD (never published raw, R7)
	hasData    bool
	dataMode   bool
	preamp     *int  // set via set_preamp; nil = unknown
	attenuator *bool // set via set_attenuator; nil = unknown
	power      *int  // 0-255 RF power read (14 0A applies to the selected band's VFO)
}

// meterCache is the last-known meter read. Each value is the raw 0-255 CI-V
// level (S-meter: S0=0, S9=120); ok=false omits the field.
type meterCache struct {
	sOK   bool
	s     uint8
	swrOK bool
	swr   uint8
	alcOK bool
	alc   uint8
}

// radioCache is the bridge-held mirror of the radio-measured state. It is
// cleared as a whole on session loss (OnSessionDown) so the next publish
// omits the fields instead of freezing stale telemetry (flexbridge Reset
// lesson, R6); selectedVFO deliberately survives (bridge-held, R6).
type radioCache struct {
	vfo       [2]vfoCache // [civ.VfoMain, civ.VfoSub]
	selected  civ.VFO     // bridge-held: survives session loss (R6)
	satellite bool        // 16 5A
	tx        bool        // 1C 00 (transceive or read reply)
	meters    meterCache
}

// clear drops every radio-measured value (R6: omitted, not zeroed-frozen).
func (c *radioCache) clear() {
	c.vfo = [2]vfoCache{}
	c.satellite = false
	c.tx = false
	c.meters = meterCache{}
}

// txVFO returns the VFO the top-level fields mirror: the radio's SELECTED
// band — the band PTT keys — with satellite mode forcing SUB (the uplink/TX
// side there; the inversion gpredict #181 documents). A front-panel
// selected-VFO change therefore flips the top-level mirror with it (R6).
func (c *radioCache) txVFO() civ.VFO {
	if c.satellite {
		return civ.VfoSub
	}
	return c.selected
}

// stateSnapshot is the comparable dedup form of /state (the spid snap
// pattern): dedup compares this, never the serialized JSON. ts is stamped at
// publish time and the freshness heartbeat keeps it fresh while unchanged.
// Pointer/flag fields distinguish "omitted" from zero (R6).
type stateSnapshot struct {
	live         bool
	sessionState radio.SessionState
	armed        bool
	err          string

	freqHz    uint32
	hasFreq   bool
	band      string
	mode      string // "" when the TX VFO's mode is unknown or not canonical (R7)
	hasMode   bool
	tx        string // "tx"|"rx" when live
	hasTX     bool
	satellite bool
	hasSat    bool

	main     vfoSnapshot
	sub      vfoSnapshot
	selected civ.VFO // bridge-held; always published (R6)

	sMeter *int
	swr    *int
	alc    *int
	power  *int
}

// vfoSnapshot is the comparable dedup form of one VFO detail object.
type vfoSnapshot struct {
	band    string
	freqHz  uint32
	hasFreq bool
	mode    string
	hasMode bool
	data    bool
	hasData bool
	preamp  *int
	att     *bool
}

// equal compares two stateSnapshot values (pointers by target).
func (s stateSnapshot) equal(o stateSnapshot) bool {
	return s.live == o.live &&
		s.sessionState == o.sessionState &&
		s.armed == o.armed &&
		s.err == o.err &&
		s.freqHz == o.freqHz && s.hasFreq == o.hasFreq &&
		s.band == o.band && s.mode == o.mode && s.hasMode == o.hasMode &&
		s.tx == o.tx && s.hasTX == o.hasTX &&
		s.satellite == o.satellite && s.hasSat == o.hasSat &&
		s.main.equal(o.main) && s.sub.equal(o.sub) && s.selected == o.selected &&
		intPtrEqual(s.sMeter, o.sMeter) && intPtrEqual(s.swr, o.swr) &&
		intPtrEqual(s.alc, o.alc) && intPtrEqual(s.power, o.power)
}

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// equal compares two vfoSnapshot values by value — the echo stores allocate
// fresh pointers per cmd, so == (pointer identity) would report spurious
// changes for unchanged preamp/attenuator.
func (v vfoSnapshot) equal(o vfoSnapshot) bool {
	return v.band == o.band &&
		v.freqHz == o.freqHz && v.hasFreq == o.hasFreq &&
		v.mode == o.mode && v.hasMode == o.hasMode &&
		v.data == o.data && v.hasData == o.hasData &&
		intPtrEqual(v.preamp, o.preamp) && boolPtrEqual(v.att, o.att)
}

// statePayload is the wire shape of /state. The JSON keys here are the U7
// console fixture contract: ts, freq_hz, band, mode, tx, main, sub,
// selected_vfo, satellite, session_state, device_online, armed, s_meter,
// tx_power, swr, alc, error.
type statePayload struct {
	TS           string      `json:"ts"`
	FreqHz       *uint32     `json:"freq_hz,omitempty"`
	Band         string      `json:"band,omitempty"`
	Mode         string      `json:"mode,omitempty"`
	TX           string      `json:"tx,omitempty"` // "tx"|"rx" — omitted without a live session (radio-measured)
	Main         *vfoPayload `json:"main,omitempty"`
	Sub          *vfoPayload `json:"sub,omitempty"`
	SelectedVFO  string      `json:"selected_vfo"`
	Satellite    *bool       `json:"satellite,omitempty"`
	SessionState string      `json:"session_state"`
	DeviceOnline bool        `json:"device_online"` // CI-V control-session liveness (R16: healthy idle = false)
	Armed        bool        `json:"armed"`
	SMeter       *int        `json:"s_meter,omitempty"`
	TXPower      *int        `json:"tx_power,omitempty"`
	SWR          *int        `json:"swr,omitempty"`
	ALC          *int        `json:"alc,omitempty"`
	Error        string      `json:"error,omitempty"`
}

// vfoPayload is one VFO detail object of /state.
type vfoPayload struct {
	Band       string  `json:"band,omitempty"`
	FreqHz     *uint32 `json:"freq_hz,omitempty"`
	Mode       string  `json:"mode,omitempty"`
	DataMode   *bool   `json:"data_mode,omitempty"`
	Preamp     *int    `json:"preamp,omitempty"`
	Attenuator *bool   `json:"attenuator,omitempty"`
}

// buildSnapshot folds the bridge-held state into the dedup form. Caller
// holds b.mu. Everything radio-measured is gated on live (R6); selected_vfo,
// session_state, armed and the error fact always publish.
func (b *Bridge) buildSnapshot(snap radio.Snapshot, live bool) stateSnapshot {
	s := stateSnapshot{
		sessionState: snap.State,
		armed:        snap.Armed,
		err:          b.errorFact(snap),
	}
	if live {
		s.live = true
		s.hasTX = true
		s.tx = txString(b.radio.tx)
		s.hasSat = true
		s.satellite = b.radio.satellite
		tx := b.radio.txVFO()
		tv := b.radio.vfo[tx]
		s.freqHz, s.hasFreq = tv.freqHz, tv.hasFreq
		s.band = tv.band
		if tv.modeOK {
			s.mode, s.hasMode = tv.mode, true
		}
		m := b.radio.meters
		if m.sOK {
			v := int(m.s)
			s.sMeter = &v
		}
		if m.swrOK {
			v := int(m.swr)
			s.swr = &v
		}
		if m.alcOK {
			v := int(m.alc)
			s.alc = &v
		}
		if tv.power != nil {
			v := *tv.power
			s.power = &v
		}
	}
	s.main = vfoSnapshotOf(b.radio.vfo[civ.VfoMain], live)
	s.sub = vfoSnapshotOf(b.radio.vfo[civ.VfoSub], live)
	s.selected = b.radio.selected
	return s
}

// vfoSnapshotOf folds one VFO cache; the detail objects are radio-measured
// state, so they publish only while live (R6 — otherwise omitted whole).
func vfoSnapshotOf(v vfoCache, live bool) vfoSnapshot {
	if !live {
		return vfoSnapshot{}
	}
	s := vfoSnapshot{
		band:    v.band,
		freqHz:  v.freqHz,
		hasFreq: v.hasFreq,
		mode:    v.mode,
		hasMode: v.modeOK,
		data:    v.dataMode,
		hasData: v.hasData,
		preamp:  v.preamp,
		att:     v.attenuator,
	}
	return s
}

func txString(on bool) string {
	if on {
		return "tx"
	}
	return "rx"
}

// errorFact merges the session's observed fact with the last /cmd rejection:
// the session fact wins while present (it clears itself via operator ack /
// decay — safety-class facts persist by the session's own ErrorSafety
// handling), otherwise the cmd rejection persists until the next admitted
// intent (the spid cmdErr pattern).
func (b *Bridge) errorFact(snap radio.Snapshot) string {
	if snap.Error != "" {
		return snap.Error
	}
	return b.cmdErr
}

// payload renders the wire JSON from the dedup form.
func statePayloadOf(s stateSnapshot) statePayload {
	p := statePayload{
		TS:           nowRFC3339(),
		SessionState: string(s.sessionState),
		DeviceOnline: s.live,
		Armed:        s.armed,
		Error:        s.err,
	}
	if s.hasFreq {
		f := s.freqHz
		p.FreqHz = &f
		p.Band = s.band
	}
	if s.hasMode {
		p.Mode = s.mode
	}
	if s.hasTX {
		p.TX = s.tx
	}
	if s.hasSat {
		v := s.satellite
		p.Satellite = &v
	}
	if s.main.hasAny() {
		v := vfoPayloadOf(s.main)
		p.Main = &v
	}
	if s.sub.hasAny() {
		v := vfoPayloadOf(s.sub)
		p.Sub = &v
	}
	p.SelectedVFO = s.selected.String()
	p.SMeter, p.SWR, p.ALC, p.TXPower = s.sMeter, s.swr, s.alc, s.power
	return p
}

func vfoPayloadOf(v vfoSnapshot) vfoPayload {
	p := vfoPayload{Band: v.band}
	if v.hasFreq {
		f := v.freqHz
		p.FreqHz = &f
	}
	if v.hasMode {
		p.Mode = v.mode
	}
	if v.hasData {
		d := v.data
		p.DataMode = &d
	}
	p.Preamp, p.Attenuator = v.preamp, v.att
	return p
}

// hasAny reports whether the VFO detail object carries anything at all (an
// empty object is omitted rather than published as {}).
func (v vfoSnapshot) hasAny() bool {
	return v.hasFreq || v.hasMode || v.hasData || v.preamp != nil || v.att != nil || v.band != ""
}

// metaPayload builds the retained /meta birth certificate (R8): structured
// capabilities per the model's Appendix-A radio example and a READ-ONLY
// expose block — state fields only, no writable setpoints, no actions, no
// PTT/arm widgets (the sat-rotator posture; the action set lives in
// docs/mqtt-api.md). The mode enum resolves via options_ref against
// capabilities.modes.
func metaPayload(site, station, slot, location string) map[string]any {
	return map[string]any{
		"schema": "1.0",
		"role":   "radio",
		"device": map[string]any{
			"model": deviceModel,
		},
		"link":     "ethernet",
		"location": location,
		"capabilities": map[string]any{
			"bands":     []string{"2m", "70cm", "23cm"},
			"modes":     []string{civ.ModeCW, civ.ModeUSB, civ.ModeLSB, civ.ModeAM, civ.ModeFM, civ.ModeData},
			"bias_t":    true, // informational: set via the radio menu per deploy gate 3 — no bus action in v1
			"satellite": true,
			"vfos":      []string{"main", "sub"},
		},
		"expose": map[string]any{
			"device": map[string]any{
				"name":  schema.SlotBase(site, station, slot),
				"model": deviceModel,
			},
			"fields": []map[string]any{
				{"key": "freq_hz", "name": "Frequency", "type": "number", "unit": "Hz", "class": "frequency", "state_class": "measurement"},
				{"key": "band", "name": "Band", "type": "enum", "options_ref": "bands"},
				{"key": "mode", "name": "Mode", "type": "enum", "options_ref": "modes"},
				{"key": "tx", "name": "Transmitting", "type": "boolean", "on": "tx", "off": "rx"},
				{"key": "selected_vfo", "name": "Selected VFO", "type": "string"},
				{"key": "satellite", "name": "Satellite mode", "type": "boolean"},
				{"key": "session_state", "name": "Session state", "type": "string"},
				{"key": "device_online", "name": "Device online", "type": "boolean"},
				{"key": "armed", "name": "Armed", "type": "boolean"},
				{"key": "s_meter", "name": "S-meter", "type": "number", "state_class": "measurement"},
				{"key": "tx_power", "name": "TX power", "type": "number", "state_class": "measurement"},
				{"key": "swr", "name": "SWR", "type": "number", "state_class": "measurement"},
				{"key": "alc", "name": "ALC", "type": "number", "state_class": "measurement"},
				{"key": "error", "name": "Last error", "type": "string"},
			},
		},
	}
}
