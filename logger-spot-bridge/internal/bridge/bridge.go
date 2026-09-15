// Package bridge holds the canonical selected-station state model, the
// decoder→payload resolution, and the retained publishing logic for the
// muehle/hf/spots slot.
//
// The slot follows the logging-integration draft (docs/logging-integration-model.md
// §3/§6.3): …/spots with role `bandmap`. This bridge's contribution is the
// retained /state `selected` record — the station the operator has keyed in
// the shack logger (Log4OM / DXLog), resolved to a map position and a beam
// bearing. It is deliberately NOT the spot *stream* (…/spots/event); nothing
// on the bus consumes that yet.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"codeberg.org/kgbvax/stationa/shared/schema"

	"logger-spot-bridge/internal/canon"
	"logger-spot-bridge/internal/geo"
	"logger-spot-bridge/internal/log4om"
	"logger-spot-bridge/internal/n1mm"
	"logger-spot-bridge/internal/qrz"
)

// Selected is one operator-keyed station, fully resolved for the console:
// identity (call), RF context (freq_hz/band/mode), map position (lat/lng or
// the locator it came from) and the beam answer (azimuth + distance_km, from
// the logger's CTY calculation or derived from coordinates).
type Selected struct {
	Call string `json:"call"`

	FreqHz int64  `json:"freq_hz,omitempty"`
	Band   string `json:"band,omitempty"`
	Mode   string `json:"mode,omitempty"`

	Locator string  `json:"locator,omitempty"`
	Lat     float64 `json:"lat,omitempty"`
	Lng     float64 `json:"lng,omitempty"`

	Azimuth    float64 `json:"azimuth,omitempty"`
	DistanceKm float64 `json:"distance_km,omitempty"`

	CountryPrefix string `json:"country_prefix,omitempty"`
	WPXPrefix     string `json:"wpx_prefix,omitempty"`

	// Country/QTH come only from the QRZ gap-fill (the loggers don't send
	// them) — context for the console's read-out.
	Country string `json:"country,omitempty"`
	QTH     string `json:"qth,omitempty"`

	// Source names the logger the selection came from (the listener's name
	// config: "dxlog", "log4om", …) and TS is when the selection arrived.
	Source string `json:"source"`
	TS     string `json:"ts"`
}

// State is the retained /state snapshot: a full document per the single-
// retained-snapshot convention, with device_online carrying the two-layer
// liveness (the /status LWT covers the bridge process; device_online covers
// "a logger has been heard on UDP within the staleness window").
type State struct {
	TS           string    `json:"ts"`
	DeviceOnline bool      `json:"device_online"`
	Selected     *Selected `json:"selected,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// Meta is the /meta birth certificate (role `bandmap` per the logging layer).
type Meta struct {
	Schema string         `json:"schema"`
	Role   string         `json:"role"`
	Host   string         `json:"host,omitempty"`
	Device map[string]any `json:"device,omitempty"`
	Caps   map[string]any `json:"capabilities,omitempty"`
}

// Topics bundles the plane addresses for the slot.
type Topics struct {
	Meta, State, Status, Cmd string
}

// TopicsFor builds the plane addresses with the shared schema helpers.
func TopicsFor(site, station, slot string) Topics {
	return Topics{
		Meta:   schema.MetaTopic(site, station, slot),
		State:  schema.StateTopic(site, station, slot),
		Status: schema.StatusTopic(site, station, slot),
		Cmd:    schema.CmdTopic(site, station, slot),
	}
}

// CallLookup is the optional position gap-filler seam (filled by the QRZ
// service in internal/qrz; a nil QRZ field keeps the resolver pure).
type CallLookup interface {
	Lookup(ctx context.Context, call string) (qrz.Record, error)
}

// Resolver turns decoder output into Selected records. Station is the shack
// QTH — the same QTH the loggers are configured with, which is what makes
// DXLog's azimuth/distance (computed from the logging PC) reusable for the
// pin and the beam ray.
type Resolver struct {
	Station    geo.LatLng
	HasStation bool
	QRZ        CallLookup // optional; nil when the lookup is disabled
}

// FromN1MM resolves an N1MM/DXLog lookupinfo datagram. Returns nil for an
// empty call — the operator cleared the entry window, which is the documented
// clear signal (DXLog reason "CallChanged" with no call).
func (r Resolver) FromN1MM(li n1mm.LookupInfo, source string) *Selected {
	if li.Call == "" {
		return nil
	}

	freqHz := li.FreqTx10 * 10
	if freqHz == 0 {
		freqHz = li.FreqRx10 * 10
	}

	sel := &Selected{
		Call:          li.Call,
		FreqHz:        freqHz,
		Band:          bandFor(li.Band, freqHz),
		Mode:          canon.CanonicalMode(li.Mode, freqHz),
		Azimuth:       li.Azimuth,
		DistanceKm:    li.DistanceKm,
		CountryPrefix: li.CountryPrefix,
		WPXPrefix:     li.WPXPrefix,
		Source:        source,
		TS:            nowRFC3339(),
	}
	r.finish(sel, li.Grid)
	return sel
}

// FromLog4OM resolves a Log4OM CALLSIGN datagram. Same contract as FromN1MM;
// Log4OM (on the documented evidence) sends no azimuth/distance, so the
// bearing comes from the locator when one is present.
func (r Resolver) FromLog4OM(c log4om.Callsign, source string) *Selected {
	if c.Call == "" {
		return nil
	}

	sel := &Selected{
		Call:   c.Call,
		FreqHz: c.FreqHz,
		Band:   bandFor(c.Band, c.FreqHz),
		Mode:   canon.CanonicalMode(c.Mode, c.FreqHz),
		Source: source,
		TS:     nowRFC3339(),
	}
	r.finish(sel, c.Locator)
	return sel
}

// finish resolves locator/azimuth/distance into a consistent record:
// coordinates from the locator (or DXLog's direct problem), then any missing
// bearing/distance from the inverse problem. Logger-provided azimuth wins
// (it is CTY-precise); coordinates fill the map.
func (r Resolver) finish(sel *Selected, locator string) {
	if ll, ok := geo.LocatorToLatLng(locator); ok {
		sel.Locator = locator
		sel.Lat = ll.Lat
		sel.Lng = ll.Lng
	}

	hasCoords := sel.Lat != 0 || sel.Lng != 0
	azGiven := sel.Azimuth > 0 || sel.DistanceKm > 0

	switch {
	case azGiven && !hasCoords && r.HasStation:
		// DXLog azimuth+distance: the direct problem places the pin.
		ll := geo.Destination(r.Station, sel.Azimuth, sel.DistanceKm)
		sel.Lat = ll.Lat
		sel.Lng = ll.Lng
	case !azGiven && hasCoords && r.HasStation:
		// Coordinates only (locator path): the inverse problem answers the beam.
		az, dist := geo.BearingDistance(r.Station, geo.LatLng{Lat: sel.Lat, Lng: sel.Lng})
		sel.Azimuth = az
		sel.DistanceKm = dist
	}
	// Both present: keep the logger's azimuth/distance, coordinates stand.
	// Neither present: the record stays call+RF only.
}

// Enrich runs the QRZ gap-fill on a fresh selection: when the logger gave no
// position at all (Log4OM's CALLSIGN broadcast is the bare call), the QRZ
// lookup provides locator/coordinates and country/city, and finish computes
// the beam answer from them exactly as it would for logger-provided data.
//
// Logger-provided positions never trigger a lookup: DXLog's azimuth+distance
// is station-relative and live, while a QRZ grid can be stale. On lookup
// error the selection is returned unchanged — it publishes call+RF only,
// exactly as with the lookup disabled. Runs on the jobs worker; ctx bounds
// the HTTP work.
func (r Resolver) Enrich(ctx context.Context, sel *Selected) (*Selected, error) {
	if sel == nil || r.QRZ == nil {
		return sel, nil
	}
	// Already placeable (locator, coordinates, or a bearing ray): skip.
	if sel.Locator != "" || sel.Lat != 0 || sel.Lng != 0 || sel.Azimuth > 0 || sel.DistanceKm > 0 {
		return sel, nil
	}

	rec, err := r.QRZ.Lookup(ctx, sel.Call)
	if err != nil {
		return sel, err
	}
	sel.Country = rec.Country
	sel.QTH = rec.Qth
	switch {
	case rec.Grid != "":
		r.finish(sel, rec.Grid)
	case rec.Lat != 0 || rec.Lon != 0:
		// QRZ without a grid but with coordinates: place directly.
		sel.Lat, sel.Lng = rec.Lat, rec.Lon
		r.finish(sel, "")
	}
	return sel, nil
}

// bandFor trusts a canonical-looking band label ("20m"), else derives from
// the frequency. DXLog's MHz-style labels ("14.0") are rejected and fall
// through to the frequency, which is always present when the band matters.
func bandFor(raw string, freqHz int64) string {
	if b := canon.NormalizeBand(raw); b != "" {
		return b
	}
	return canon.BandFor(freqHz)
}

// SlotBridge owns the slot's canonical state and turns it into retained
// payloads. All methods run on the single jobs worker (the caller enqueues);
// the worker's serialization is the only lock the state needs.
type SlotBridge struct {
	meta     Meta
	lastKey  string // dedup key over (selected content, device_online) — TS excluded
	lastJSON string // last published /state document
}

// New builds the slot bridge.
func New(meta Meta) *SlotBridge {
	return &SlotBridge{meta: meta}
}

// MetaPayload returns the retained /meta JSON.
func (b *SlotBridge) MetaPayload() ([]byte, error) {
	return json.Marshal(b.meta)
}

// SetSelected records a new selection (nil clears it) and returns the /state
// payload, always changed=true: every datagram is an event (the operator
// re-keyed the entry window, possibly within the same second), so the
// selection path deliberately has NO dedup — the console re-brightens the
// marker off the fresh ts. Only the liveness path dedups.
func (b *SlotBridge) SetSelected(sel *Selected, deviceOnline bool) (payload []byte, changed bool, err error) {
	st := State{TS: nowRFC3339(), DeviceOnline: deviceOnline, Selected: sel}
	data, err := json.Marshal(st)
	if err != nil {
		return nil, false, err
	}
	// Keep the dedup key in step even though the selection path ignores it:
	// a subsequent SetDeviceOnline with this same content must not republish.
	if key, kerr := dedupKey(sel, deviceOnline); kerr == nil {
		b.lastKey = key
	}
	b.lastJSON = string(data)
	return data, true, nil
}

// SetDeviceOnline re-announces liveness without touching the selection
// (staleness watcher path). Dedup keeps a quiet bridge off the wire: an
// unchanged (selection, online) pair publishes nothing.
func (b *SlotBridge) SetDeviceOnline(online bool, sel *Selected) (payload []byte, changed bool, err error) {
	key, err := dedupKey(sel, online)
	if err != nil {
		return nil, false, err
	}
	if key == b.lastKey {
		return nil, false, nil
	}
	st := State{TS: nowRFC3339(), DeviceOnline: online, Selected: sel}
	data, err := json.Marshal(st)
	if err != nil {
		return nil, false, err
	}
	b.lastKey = key
	b.lastJSON = string(data)
	return data, true, nil
}

// LastJSON returns the last published /state document ("" before the first).
// The on-connect birth sequence republishes it verbatim — a fresh subscriber
// after a broker restart must see state even when nothing changes afterwards
// (REQ-RT-5) — and the liveness path reads the current selection out of it.
func (b *SlotBridge) LastJSON() string { return b.lastJSON }

func dedupKey(sel *Selected, online bool) (string, error) {
	if sel == nil {
		return fmt.Sprintf("cleared|%t", online), nil
	}
	s, err := json.Marshal(sel)
	if err != nil {
		return "", err
	}
	return string(s) + "|" + fmt.Sprintf("%t", online), nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
