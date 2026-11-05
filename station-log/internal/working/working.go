// Package working is station-log's "currently working station" feed: the
// station the operator has keyed in the shack logger (DXLog, Log4OM),
// resolved to a map position and a beam bearing, published as the retained
// document on muehle/hf/station-log/working. It absorbed logger-spot-bridge
// (slot muehle/hf/spots); the document shape is unchanged, so hf_console
// reads it as it read /state of the old slot.
//
// The decoders (n1mm, callsign), geo, qrz are subpackages. The Resolver is
// pure apart from the optional lookups; Feed owns the listeners' side
// (liveness, the publish path on the jobs worker).
package working

import (
	"context"
	"time"

	"station-log/internal/band"
	"station-log/internal/working/callsign"
	"station-log/internal/working/geo"
	"station-log/internal/working/n1mm"
	"station-log/internal/working/qrz"
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

	// Name/Country/QTH come only from the QRZ enrichment (the loggers don't
	// send them); the name is the QRZ record's fname + name.
	Name    string `json:"name,omitempty"`
	Country string `json:"country,omitempty"`
	QTH     string `json:"qth,omitempty"`

	// Source names the logger the selection came from (the listener's name:
	// "dxlog", "log4om", ...) and TS is when the selection arrived.
	Source string `json:"source"`
	TS     string `json:"ts"`
}

// State is the retained document on the working topic. device_online carries
// the logger-feed layer of the two-layer liveness ("a logger was heard on
// UDP within stale_after"); the bridge layer is station-log's /status.
// Selected omitted = cleared.
type State struct {
	TS           string    `json:"ts"`
	DeviceOnline bool      `json:"device_online"`
	Selected     *Selected `json:"selected,omitempty"`
}

// CallLookup is the QRZ seam (qrz.Service); a nil value keeps the resolver pure.
type CallLookup interface {
	Lookup(ctx context.Context, call string) (qrz.Record, error)
}

// GridLookup returns a locator station-log already holds for a call (from a
// worked QSO). It is asked before QRZ for a position.
type GridLookup func(call string) (grid string, ok bool)

// Resolver turns decoder output into Selected records. Station is the shack
// QTH, the same QTH the loggers are configured with, which is what makes
// DXLog's azimuth/distance reusable for the pin and the beam ray.
type Resolver struct {
	Station    geo.LatLng
	HasStation bool
	QRZ        CallLookup // optional
	Grid       GridLookup // optional: station-log's own store
	Now        func() time.Time
}

func (r Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r Resolver) ts() string { return r.now().UTC().Format(time.RFC3339) }

// FromN1MM resolves an N1MM/DXLog lookupinfo datagram. It returns nil for an
// empty call: the operator cleared the entry window, the documented clear.
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
		Mode:          CanonicalMode(li.Mode, freqHz),
		Azimuth:       li.Azimuth,
		DistanceKm:    li.DistanceKm,
		CountryPrefix: li.CountryPrefix,
		WPXPrefix:     li.WPXPrefix,
		Source:        source,
		TS:            r.ts(),
	}
	r.finish(sel, li.Grid)
	return sel
}

// FromCallsign resolves a Log4OM CALLSIGN datagram. Same contract as
// FromN1MM. Log4OM sends the bare call, so position comes from enrichment.
func (r Resolver) FromCallsign(c callsign.Callsign, source string) *Selected {
	if c.Call == "" {
		return nil
	}
	sel := &Selected{
		Call:   c.Call,
		FreqHz: c.FreqHz,
		Band:   bandFor(c.Band, c.FreqHz),
		Mode:   CanonicalMode(c.Mode, c.FreqHz),
		Source: source,
		TS:     r.ts(),
	}
	r.finish(sel, c.Locator)
	return sel
}

// finish resolves locator/azimuth/distance into a consistent record:
// coordinates from the locator (or DXLog's direct problem), then any missing
// bearing/distance from the inverse problem. Logger azimuth wins (CTY
// precise); coordinates fill the map. (0,0) is the no-coordinates sentinel.
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
		ll := geo.Destination(r.Station, sel.Azimuth, sel.DistanceKm)
		sel.Lat, sel.Lng = ll.Lat, ll.Lng
	case !azGiven && hasCoords && r.HasStation:
		sel.Azimuth, sel.DistanceKm = geo.BearingDistance(r.Station, geo.LatLng{Lat: sel.Lat, Lng: sel.Lng})
	}
}

func placed(sel *Selected) bool {
	return sel.Locator != "" || sel.Lat != 0 || sel.Lng != 0 || sel.Azimuth > 0 || sel.DistanceKm > 0
}

// Enrich fills what the logger did not send. A record the logger left
// without any position first asks station-log's own store for a worked
// locator of the call, then QRZ; identity (name, country, city) comes from
// QRZ for every keyed call. Position stays logger-authoritative: DXLog's
// azimuth+distance is station-relative and live while a QRZ grid can be
// stale. A lookup error returns the selection unchanged (call+RF only) with
// the error; publication never depends on it. Runs on the jobs worker; ctx
// bounds the HTTP work.
func (r Resolver) Enrich(ctx context.Context, sel *Selected) (*Selected, error) {
	if sel == nil {
		return sel, nil
	}
	if r.Grid != nil && !placed(sel) {
		if g, ok := r.Grid(sel.Call); ok {
			r.finish(sel, g)
		}
	}
	if r.QRZ == nil {
		return sel, nil
	}
	rec, err := r.QRZ.Lookup(ctx, sel.Call)
	if err != nil {
		return sel, err
	}
	sel.Name = rec.Name
	sel.Country = rec.Country
	sel.QTH = rec.Qth
	if placed(sel) {
		return sel, nil
	}
	switch {
	case rec.Grid != "":
		r.finish(sel, rec.Grid)
	case rec.Lat != 0 || rec.Lon != 0:
		sel.Lat, sel.Lng = rec.Lat, rec.Lon
		r.finish(sel, "")
	}
	return sel, nil
}

// bandFor trusts a canonical-looking band label ("20m"), else derives the
// band from the frequency: the ADIF band containing it, "gen" for the HF
// general-coverage gap, "" outside (omitted from the document). DXLog's
// MHz-style labels ("14.0") fall through to the frequency.
func bandFor(raw string, freqHz int64) string {
	if b := NormalizeBand(raw); b != "" {
		return b
	}
	return BandOfHz(freqHz)
}

// BandOfHz resolves a frequency (Hz) to the band label of the working document.
func BandOfHz(freqHz int64) string {
	if freqHz <= 0 {
		return ""
	}
	if b, ok := band.Of(float64(freqHz)); ok {
		return b.Name
	}
	if freqHz >= 1_800_000 && freqHz <= 30_000_000 {
		return "gen"
	}
	return ""
}
