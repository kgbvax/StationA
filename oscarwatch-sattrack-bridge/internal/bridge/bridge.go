// Package bridge holds the canonical sat-track state model for the
// muehle/uhf/sat-track slot: the mapping from an OscarWatch `satelliteStatus`
// frame (plus the link state) to the retained /state snapshot, the /meta birth
// certificate with its `expose` block, and the change-dedup that keeps an
// idle tracker off the wire.
//
// The slot answers "which satellite is the station tracking, and where is it"
// — the sat-ops analogue of muehle/hf/spots. It is read-only (no /cmd).
package bridge

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"codeberg.org/kgbvax/stationa/shared/schema"

	"oscarwatch-sattrack-bridge/internal/geo"
	"oscarwatch-sattrack-bridge/internal/oscarwatch"
)

// Input is everything the /state snapshot is derived from: the WebSocket link
// state and the latest `satelliteStatus` frame on the current connection. The
// source side replaces it wholesale on every event, so the state worker only
// ever needs the newest one (latest-wins; see cmd main).
type Input struct {
	// Online is true while the WebSocket to OscarWatch is open.
	Online bool
	// Err describes why the link is down ("" while online).
	Err string
	// Status is the newest status frame since the link came up; nil until
	// OscarWatch's on-connect snapshot arrives, and after a disconnect.
	Status *oscarwatch.Status
}

// State is the retained /state snapshot. The satellite keys are pointers
// without omitempty: with nothing tracked (or the feed down) they are JSON
// null, so every consumer sees a stable key set and Home Assistant renders the
// sensors as "unknown" instead of logging template errors on missing keys.
type State struct {
	TS           string `json:"ts"`
	DeviceOnline bool   `json:"device_online"`
	// Tracking: OscarWatch has a satellite focused (and, with its "only
	// broadcast when above horizon" option on, above the horizon).
	Tracking bool `json:"tracking"`
	// InRange is OscarWatch's inRange flag, passed through.
	InRange bool `json:"in_range"`

	SatName  *string `json:"sat_name"`
	NoradID  *string `json:"norad_id"`
	ModeType *string `json:"mode_type"`

	Az           *float64 `json:"az"`
	El           *float64 `json:"el"`
	RangeKm      *float64 `json:"range_km"`
	RangeRateKmS *float64 `json:"range_rate_km_s"`
	Sunlit       *bool    `json:"sunlit"`

	SubLat *float64 `json:"sub_lat"`
	SubLng *float64 `json:"sub_lng"`
	AltKm  *float64 `json:"alt_km"`

	UplinkHz     *int64  `json:"uplink_hz"`
	DownlinkHz   *int64  `json:"downlink_hz"`
	UplinkMode   *string `json:"uplink_mode"`
	DownlinkMode *string `json:"downlink_mode"`
	UplinkBand   *string `json:"uplink_band"`
	DownlinkBand *string `json:"downlink_band"`
	BeaconOnly   *bool   `json:"beacon_only"`
	Doppler      *string `json:"doppler"`

	Error string `json:"error,omitempty"`
}

// Build derives the /state snapshot from the input. obs is the station
// position for the sub-satellite point; nil leaves sub_lat/sub_lng/alt_km
// null. now stamps ts.
func Build(in Input, obs *geo.Observer, now time.Time) State {
	st := State{TS: now.UTC().Format(time.RFC3339), DeviceOnline: in.Online}
	if !in.Online {
		st.Error = in.Err
		return st // a frozen satellite position is worse than none
	}
	s := in.Status
	if s == nil || s.Satellite == nil {
		return st
	}
	st.Tracking = true
	st.InRange = s.InRange
	st.SatName = str(s.Satellite.Name)
	st.NoradID = str(string(s.Satellite.NoradID))
	st.ModeType = str(s.Satellite.ModeType)

	if t := s.Tracking; t != nil {
		st.Az = round(t.AzimuthDeg, 1)
		st.El = round(t.ElevationDeg, 1)
		st.RangeKm = round(t.RangeKm, 1)
		st.RangeRateKmS = round(t.RangeRateKmPerSec, 3)
		st.Sunlit = t.IsSunlit
		if obs != nil && t.AzimuthDeg != nil && t.ElevationDeg != nil && t.RangeKm != nil && *t.RangeKm > 0 {
			// From the unrounded source values; rounding happens on output.
			p := geo.SubPoint(*obs, *t.AzimuthDeg, *t.ElevationDeg, *t.RangeKm)
			st.SubLat = round(&p.Lat, 3)
			st.SubLng = round(&p.Lng, 3)
			st.AltKm = round(&p.AltKm, 1)
		}
	}
	if f := s.Frequencies; f != nil {
		st.UplinkHz = hz(f.UplinkHz)
		st.DownlinkHz = hz(f.DownlinkHz)
		st.UplinkMode = CanonicalMode(f.UplinkMode)
		st.DownlinkMode = CanonicalMode(f.DownlinkMode)
		beacon := f.IsBeaconOnly
		st.BeaconOnly = &beacon
	}
	if b := s.Bands; b != nil {
		st.UplinkBand = str(strings.ToLower(strings.TrimSpace(b.TX)))
		st.DownlinkBand = str(strings.ToLower(strings.TrimSpace(b.RX)))
	}
	st.Doppler = doppler(s.DopplerStrategy)
	return st
}

// CanonicalMode normalizes an OscarWatch uplink/downlink mode to the station's
// canonical vocabulary (cw, usb, lsb, am, fm, data — band-mode-reference.md).
// Anything it cannot place — including a bare "SSB", which does not say which
// sideband — returns nil: adapters publish a canonical mode or nothing.
func CanonicalMode(raw string) *string {
	m := strings.ToUpper(strings.TrimSpace(raw))
	var c string
	switch m {
	case "FM", "FMN", "NFM", "FM-N", "WFM", "FMW":
		c = "fm"
	case "USB":
		c = "usb"
	case "LSB":
		c = "lsb"
	case "CW", "CWR", "CW-R", "CW-U", "CW-L":
		c = "cw"
	case "AM":
		c = "am"
	case "DATA", "DIGI", "DIG", "PKT", "PACKET", "AFSK", "FSK", "GFSK", "GMSK", "MSK",
		"BPSK", "QPSK", "RTTY", "DATA-USB", "DATA-LSB", "DATA-FM", "USB-D", "LSB-D", "FM-D":
		c = "data"
	default:
		return nil
	}
	return &c
}

// doppler maps OscarWatch's camelCase strategy to the bus's snake_case; an
// unknown future value passes through rather than vanishing.
func doppler(raw string) *string {
	switch raw {
	case "":
		return nil
	case "full":
		return str("full")
	case "downlinkOnly":
		return str("downlink_only")
	case "uplinkOnly":
		return str("uplink_only")
	}
	return str(raw)
}

func str(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func hz(v int64) *int64 {
	if v <= 0 {
		return nil // OscarWatch sends 0 for "none" (e.g. a beacon-only uplink)
	}
	return &v
}

func round(v *float64, places int) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	p := math.Pow(10, float64(places))
	r := math.Round(*v*p) / p
	if r == 0 {
		r = 0 // no "-0" on the wire
	}
	return &r
}

// Topics bundles the plane addresses for the slot.
type Topics struct {
	Meta, State, Status string
}

// TopicsFor builds the plane addresses with the shared schema helpers. There
// is no /cmd: the slot is read-only.
func TopicsFor(site, station, slot string) Topics {
	return Topics{
		Meta:   schema.MetaTopic(site, station, slot),
		State:  schema.StateTopic(site, station, slot),
		Status: schema.StatusTopic(site, station, slot),
	}
}

// MetaOptions carries the config-sourced identity for /meta (§8.1 item 6: no
// site, host or location constants in code).
type MetaOptions struct {
	Slot     string // slot segment, used as the HA device name
	Location string
	Host     string
}

// Meta builds the retained /meta birth certificate. The expose block is
// read-only (fields only) and keyed 1:1 to State's JSON keys, so hadiscovery
// renders HA sensors and testui a field panel without any registration.
func Meta(o MetaOptions) map[string]any {
	return map[string]any{
		"schema": "1.0",
		"role":   "sat-track",
		"device": map[string]any{
			"name":  "OscarWatch Satellite link",
			"model": "OscarWatch",
		},
		"link":     "websocket",
		"location": o.Location,
		"host":     o.Host,
		"capabilities": map[string]any{
			"source":   "oscarwatch",
			"protocol": oscarwatch.ProtocolVersion,
		},
		"expose": map[string]any{
			"device": map[string]any{"name": o.Slot, "model": "OscarWatch"},
			"fields": exposeFields,
		},
	}
}

// exposeFields is the read-only field surface. Classes are only set where a
// real HA device class fits the unit (km → distance, Hz → frequency via the
// unit fallback); km/s has no HA speed unit, so range rate carries none.
var exposeFields = []map[string]any{
	{"key": "sat_name", "name": "Satellite", "type": "string"},
	{"key": "norad_id", "name": "NORAD ID", "type": "string"},
	{"key": "mode_type", "name": "Transponder mode", "type": "string"},
	{"key": "tracking", "name": "Tracking", "type": "boolean"},
	{"key": "in_range", "name": "In range", "type": "boolean"},
	{"key": "az", "name": "Azimuth", "type": "number", "unit": "°", "state_class": "measurement"},
	{"key": "el", "name": "Elevation", "type": "number", "unit": "°", "state_class": "measurement"},
	{"key": "range_km", "name": "Range", "type": "number", "unit": "km", "class": "distance", "state_class": "measurement"},
	{"key": "range_rate_km_s", "name": "Range rate", "type": "number", "unit": "km/s", "state_class": "measurement"},
	{"key": "sunlit", "name": "Sunlit", "type": "boolean"},
	{"key": "sub_lat", "name": "Sub-satellite latitude", "type": "number", "unit": "°"},
	{"key": "sub_lng", "name": "Sub-satellite longitude", "type": "number", "unit": "°"},
	{"key": "alt_km", "name": "Altitude", "type": "number", "unit": "km", "class": "distance", "state_class": "measurement"},
	{"key": "uplink_hz", "name": "Uplink frequency", "type": "number", "unit": "Hz"},
	{"key": "downlink_hz", "name": "Downlink frequency", "type": "number", "unit": "Hz"},
	{"key": "uplink_mode", "name": "Uplink mode", "type": "string"},
	{"key": "downlink_mode", "name": "Downlink mode", "type": "string"},
	{"key": "doppler", "name": "Doppler strategy", "type": "string"},
	{"key": "device_online", "name": "Tracker online", "type": "boolean"},
	{"key": "error", "name": "Last error", "type": "string"},
}

// SlotBridge owns the last published snapshot. It is used from the single
// state worker only; that serialization is the only lock it needs.
type SlotBridge struct {
	meta     map[string]any
	lastKey  string // dedup key: the snapshot with ts blanked
	lastJSON string // last published /state document
}

// New builds the slot bridge around a /meta document.
func New(meta map[string]any) *SlotBridge {
	return &SlotBridge{meta: meta}
}

// MetaPayload returns the retained /meta JSON.
func (b *SlotBridge) MetaPayload() ([]byte, error) {
	return json.Marshal(b.meta)
}

// Update records st and returns its payload; changed is false when st equals
// the last snapshot apart from ts. This is what keeps an idle tracker off the
// wire: OscarWatch re-sends its unchanged frame about once a second (only its
// timestampUtc moves), and with nothing focused every one of those frames
// maps to the same snapshot.
func (b *SlotBridge) Update(st State) (payload []byte, changed bool, err error) {
	keyed := st
	keyed.TS = ""
	k, err := json.Marshal(keyed)
	if err != nil {
		return nil, false, err
	}
	if string(k) == b.lastKey {
		return nil, false, nil
	}
	data, err := json.Marshal(st)
	if err != nil {
		return nil, false, err
	}
	b.lastKey = string(k)
	b.lastJSON = string(data)
	return data, true, nil
}

// LastJSON returns the last published /state document ("" before the first).
// The on-connect birth republishes it verbatim so a subscriber arriving after
// a broker restart sees state even when nothing changes afterwards.
func (b *SlotBridge) LastJSON() string { return b.lastJSON }
