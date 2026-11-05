package working

import (
	"context"
	"testing"

	"station-log/internal/working/callsign"
	"station-log/internal/working/geo"
	"station-log/internal/working/n1mm"
	"station-log/internal/working/qrz"
)

func stationResolver() Resolver {
	jo31, _ := geo.LocatorToLatLng("JO31") // (50.5, -13.0)
	return Resolver{Station: jo31, HasStation: true}
}

func n1mmDXLog(call string) n1mm.LookupInfo {
	return n1mm.LookupInfo{
		Call: call, FreqTx10: 1402476, Band: "20", Mode: "CW",
		Azimuth: 62.4, DistanceKm: 15420.3,
		CountryPrefix: "VK9", WPXPrefix: "VK9X", Reason: "SpaceOrTab", Logger: "DXLog",
	}
}

func TestFromN1MMResolutionMatrix(t *testing.T) {
	r := stationResolver()

	// DXLog shape: no grid, az+dist present → the direct problem places the
	// pin; logger azimuth/distance stand; band derived from freq (MHz form
	// "20" rejected by NormalizeBand).
	sel := r.FromN1MM(n1mmDXLog("VK9XY"), "dxlog")
	if sel == nil {
		t.Fatal("nil selection")
	}
	if sel.Call != "VK9XY" || sel.FreqHz != 14024760 {
		t.Fatalf("call/freq = %q/%d (10 Hz units ×10)", sel.Call, sel.FreqHz)
	}
	if sel.Band != "20m" || sel.Mode != "cw" {
		t.Fatalf("band/mode = %q/%q", sel.Band, sel.Mode)
	}
	if sel.Lat == 0 || sel.Lng == 0 {
		t.Fatal("direct problem produced no pin")
	}
	if sel.Azimuth != 62.4 || sel.DistanceKm != 15420.3 {
		t.Fatalf("az/dist = (%v, %v), want logger values verbatim", sel.Azimuth, sel.DistanceKm)
	}
	if sel.Locator != "" {
		t.Fatalf("Locator = %q, want empty", sel.Locator)
	}

	// N1MM shape: grid present, no az/dist → coordinates from the locator,
	// bearing/distance from the inverse problem.
	li := n1mm.LookupInfo{Call: "G4ABC", FreqTx10: 707400, Band: "40m", Mode: "LSB", Grid: "IO91"}
	sel2 := r.FromN1MM(li, "n1mm")
	if sel2.Locator != "IO91" || sel2.Lat == 0 {
		t.Fatalf("locator path: %+v", sel2)
	}
	if sel2.Azimuth <= 0 || sel2.DistanceKm <= 0 {
		t.Fatalf("inverse problem did not fill az/dist: %+v", sel2)
	}
	// 40m LSB stays LSB (below 10 MHz), freq 707400×10 = 7074000.
	if sel2.FreqHz != 7074000 || sel2.Mode != "lsb" {
		t.Fatalf("freq/mode = (%d, %q)", sel2.FreqHz, sel2.Mode)
	}

	// Neither grid nor az/dist (and station known): call+RF only.
	li3 := n1mm.LookupInfo{Call: "AB1CDE", FreqTx10: 1402476, Mode: "CW"}
	sel3 := r.FromN1MM(li3, "dxlog")
	if sel3.Lat != 0 || sel3.Azimuth != 0 {
		t.Fatalf("bare record should carry no position: %+v", sel3)
	}

	// No station configured: az/dist still carried (compass ray), no pin.
	rNone := Resolver{}
	sel4 := rNone.FromN1MM(n1mmDXLog("VK9XY"), "dxlog")
	if sel4.Lat != 0 {
		t.Fatal("pin placed without a station locator")
	}
	if sel4.Azimuth != 62.4 {
		t.Fatalf("logger azimuth lost: %+v", sel4)
	}
}

func TestFromN1MMEmptyCallClears(t *testing.T) {
	r := stationResolver()
	if sel := r.FromN1MM(n1mmDXLog(""), "dxlog"); sel != nil {
		t.Fatalf("empty call should clear (nil), got %+v", sel)
	}
}

func TestFromCallsign(t *testing.T) {
	r := stationResolver()
	sel := r.FromCallsign(callsign.Callsign{
		Call: "OK2XYZ", FreqHz: 7074000, Mode: "CW", Locator: "JO70",
	}, "log4om")
	if sel == nil {
		t.Fatal("nil selection")
	}
	if sel.Band != "40m" || sel.Mode != "cw" {
		t.Fatalf("band/mode = %q/%q", sel.Band, sel.Mode)
	}
	if sel.Locator != "JO70" || sel.Lat == 0 {
		t.Fatalf("locator not resolved: %+v", sel)
	}
	if sel.Azimuth <= 0 || sel.DistanceKm <= 0 {
		t.Fatalf("inverse problem did not run: %+v", sel)
	}

	if sel := r.FromCallsign(callsign.Callsign{Call: ""}, "log4om"); sel != nil {
		t.Fatal("empty log4om call should clear")
	}
}

// fakeLookup scripts one answer and records every call it sees.
type fakeLookup struct {
	rec   qrz.Record
	err   error
	calls []string
}

func (f *fakeLookup) Lookup(_ context.Context, call string) (qrz.Record, error) {
	f.calls = append(f.calls, call)
	return f.rec, f.err
}

// A Log4OM selection (bare call, no position) is the QRZ gap-fill's whole
// reason: the grid places the pin, finish answers the beam, name/country/qth
// ride along.
func TestEnrichFillsMissingPosition(t *testing.T) {
	r := stationResolver()
	lk := &fakeLookup{rec: qrz.Record{
		Call: "VK9XY", Grid: "QH42wp", Country: "Australia", Qth: "Cairns", Name: "Fred Nerk",
	}}
	r.QRZ = lk

	sel := r.FromCallsign(callsign.Callsign{Call: "VK9XY"}, "log4om")
	sel, err := r.Enrich(context.Background(), sel)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(lk.calls) != 1 || lk.calls[0] != "VK9XY" {
		t.Fatalf("lookups = %v, want [VK9XY]", lk.calls)
	}
	if sel.Locator != "QH42wp" {
		t.Errorf("locator = %q, want the QRZ grid", sel.Locator)
	}
	if sel.Lat == 0 && sel.Lng == 0 {
		t.Error("QRZ grid did not resolve to coordinates")
	}
	if sel.Azimuth <= 0 || sel.DistanceKm <= 0 {
		t.Errorf("beam answer not derived: az=%v dist=%v", sel.Azimuth, sel.DistanceKm)
	}
	if sel.Country != "Australia" || sel.QTH != "Cairns" {
		t.Errorf("country/qth = %q/%q", sel.Country, sel.QTH)
	}
	if sel.Name != "Fred Nerk" {
		t.Errorf("name = %q, want the QRZ operator name", sel.Name)
	}
}

// QRZ without a grid but with coordinates still places the pin (finish's
// inverse problem answers the beam from raw lat/lng).
func TestEnrichFromCoordinatesWithoutGrid(t *testing.T) {
	r := stationResolver()
	r.QRZ = &fakeLookup{rec: qrz.Record{Call: "VK9XY", Lat: -16.92, Lon: 145.77}}

	sel := r.FromCallsign(callsign.Callsign{Call: "VK9XY"}, "log4om")
	sel, _ = r.Enrich(context.Background(), sel)
	if sel.Locator != "" {
		t.Errorf("locator = %q, want empty (no grid in the record)", sel.Locator)
	}
	if sel.Lat == 0 || sel.Lng == 0 {
		t.Error("coordinates not applied")
	}
	if sel.Azimuth <= 0 || sel.DistanceKm <= 0 {
		t.Errorf("beam answer not derived from coordinates: az=%v dist=%v", sel.Azimuth, sel.DistanceKm)
	}
}

// Logger-provided positions are authoritative: the lookup still runs (the
// console's read-out wants the operator name) but locator, coordinates and
// the beam answer are never overwritten.
func TestEnrichKeepsLoggerPositionFillsIdentity(t *testing.T) {
	r := stationResolver()
	lk := &fakeLookup{rec: qrz.Record{
		Call: "VK9XY", Grid: "QH42wp", Country: "Australia", Qth: "Cairns", Name: "Fred Nerk",
	}}
	r.QRZ = lk

	// DXLog az+dist → direct problem, no grid.
	sel := r.FromN1MM(n1mmDXLog("VK9XY"), "dxlog")
	before := *sel
	if _, err := r.Enrich(context.Background(), sel); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(lk.calls) != 1 || lk.calls[0] != "VK9XY" {
		t.Fatalf("lookups = %v, want [VK9XY]", lk.calls)
	}
	if sel.Locator != before.Locator || sel.Lat != before.Lat || sel.Lng != before.Lng ||
		sel.Azimuth != before.Azimuth || sel.DistanceKm != before.DistanceKm {
		t.Errorf("QRZ position leaked into a logger-placed record: %+v → %+v", &before, sel)
	}
	if sel.Name != "Fred Nerk" || sel.Country != "Australia" || sel.QTH != "Cairns" {
		t.Errorf("identity not filled: name/country/qth = %q/%q/%q", sel.Name, sel.Country, sel.QTH)
	}

	// N1MM grid → locator path: same contract.
	gridSel := r.FromN1MM(n1mm.LookupInfo{
		Call: "VK9XY", FreqTx10: 1402476, Band: "20", Mode: "CW", Grid: "QH42",
	}, "n1mm")
	gridBefore := *gridSel
	if _, err := r.Enrich(context.Background(), gridSel); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(lk.calls) != 2 {
		t.Fatalf("lookups = %v, want one per selection", lk.calls)
	}
	if gridSel.Locator != gridBefore.Locator || gridSel.Azimuth != gridBefore.Azimuth {
		t.Errorf("QRZ position leaked into a logger-located record: %+v → %+v", &gridBefore, gridSel)
	}
	if gridSel.Name != "Fred Nerk" {
		t.Errorf("name not filled on the locator path: %q", gridSel.Name)
	}
}

func TestEnrichNilLookupAndNilSelection(t *testing.T) {
	r := stationResolver() // QRZ nil → disabled

	sel := r.FromCallsign(callsign.Callsign{Call: "VK9XY"}, "log4om")
	got, err := r.Enrich(context.Background(), sel)
	if err != nil || got != sel {
		t.Fatalf("disabled enrich changed behavior: %+v, %v", got, err)
	}
	if got, err := r.Enrich(context.Background(), nil); got != nil || err != nil {
		t.Fatalf("nil selection not passed through: %+v, %v", got, err)
	}
}

// A failed lookup publishes the record as-is (call+RF only) — the same
// shape the bridge produced before QRZ existed.
func TestEnrichLookupErrorLeavesSelectionUnchanged(t *testing.T) {
	r := stationResolver()
	r.QRZ = &fakeLookup{err: qrz.ErrNotFound}

	sel := r.FromCallsign(callsign.Callsign{Call: "XX9XX"}, "log4om")
	before := *sel
	got, err := r.Enrich(context.Background(), sel)
	if err == nil {
		t.Fatal("expected the lookup error to surface")
	}
	if got != sel || *got != before {
		t.Errorf("selection changed on error: %+v → %+v", &before, got)
	}
}

// Without a configured station_locator the QRZ position still places the
// pin; only the beam answer stays absent.
func TestEnrichWithoutStationLocator(t *testing.T) {
	r := Resolver{} // no station
	lk := &fakeLookup{rec: qrz.Record{Call: "VK9XY", Grid: "QH42wp", Country: "Australia"}}
	r.QRZ = lk

	sel := r.FromCallsign(callsign.Callsign{Call: "VK9XY"}, "log4om")
	sel, err := r.Enrich(context.Background(), sel)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if sel.Lat == 0 && sel.Lng == 0 {
		t.Error("QRZ grid did not resolve to coordinates")
	}
	if sel.Azimuth != 0 || sel.DistanceKm != 0 {
		t.Errorf("beam answer computed without a station: az=%v dist=%v", sel.Azimuth, sel.DistanceKm)
	}
}

// A call worked before gives the pin from station-log's own store; QRZ
// still supplies the identity and never moves the position.
func TestEnrichUsesOwnStoreGridBeforeQRZ(t *testing.T) {
	r := stationResolver()
	r.Grid = func(call string) (string, bool) {
		if call == "VK9XY" {
			return "QH42", true
		}
		return "", false
	}
	lk := &fakeLookup{rec: qrz.Record{Call: "VK9XY", Grid: "PM95", Country: "Australia", Name: "Fred Nerk"}}
	r.QRZ = lk

	sel := r.FromCallsign(callsign.Callsign{Call: "VK9XY"}, "log4om")
	sel, err := r.Enrich(context.Background(), sel)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Locator != "QH42" || sel.Azimuth <= 0 {
		t.Fatalf("own-store grid not used: %+v", sel)
	}
	if sel.Name != "Fred Nerk" || sel.Country != "Australia" {
		t.Errorf("identity not filled from QRZ: %+v", sel)
	}

	// Own store alone (no QRZ) still places the call.
	r.QRZ = nil
	sel = r.FromCallsign(callsign.Callsign{Call: "VK9XY"}, "log4om")
	if sel, _ = r.Enrich(context.Background(), sel); sel.Locator != "QH42" {
		t.Fatalf("own-store grid without QRZ: %+v", sel)
	}

	// An unknown call stays call-only.
	sel = r.FromCallsign(callsign.Callsign{Call: "XX9XX"}, "log4om")
	if sel, _ = r.Enrich(context.Background(), sel); sel.Lat != 0 || sel.Locator != "" {
		t.Fatalf("unknown call got a position: %+v", sel)
	}
}

// A logger-placed record never asks the store.
func TestEnrichOwnStoreSkippedWhenPlaced(t *testing.T) {
	r := stationResolver()
	asked := false
	r.Grid = func(string) (string, bool) { asked = true; return "QH42", true }
	sel := r.FromN1MM(n1mmDXLog("VK9XY"), "dxlog")
	r.Enrich(context.Background(), sel)
	if asked {
		t.Fatal("own store consulted for a logger-placed record")
	}
}

func n1mmDXLogNoPos() n1mm.LookupInfo {
	return n1mm.LookupInfo{Call: "AB1CDE", FreqTx10: 1402476, Mode: "CW"}
}
