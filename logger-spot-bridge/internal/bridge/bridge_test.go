package bridge

import (
	"context"
	"encoding/json"
	"testing"

	"logger-spot-bridge/internal/geo"
	"logger-spot-bridge/internal/log4om"
	"logger-spot-bridge/internal/n1mm"
	"logger-spot-bridge/internal/qrz"
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

func TestFromLog4OM(t *testing.T) {
	r := stationResolver()
	sel := r.FromLog4OM(log4om.Callsign{
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

	if sel := r.FromLog4OM(log4om.Callsign{Call: ""}, "log4om"); sel != nil {
		t.Fatal("empty log4om call should clear")
	}
}

func TestSlotBridgePublishSemantics(t *testing.T) {
	b := New(Meta{Schema: "1.0", Role: "bandmap"})

	r := stationResolver()
	sel1 := r.FromN1MM(n1mmDXLog("VK9XY"), "dxlog")
	p1, changed, err := b.SetSelected(sel1, true)
	if err != nil || !changed || p1 == nil {
		t.Fatalf("first publish: changed=%t err=%v", changed, err)
	}

	// Every selection datagram is an event: even the identical selection
	// object republishes (fresh ts), and so does a re-key of the same call.
	// The console re-brightens off the fresh ts — do not dedup this path.
	if _, changed, _ := b.SetSelected(sel1, true); !changed {
		t.Fatal("selection path deduped — datagrams must stay events")
	}
	sel2 := r.FromN1MM(n1mmDXLog("VK9XY"), "dxlog")
	if _, changed, _ := b.SetSelected(sel2, true); !changed {
		t.Fatal("re-keyed same call did not republish")
	}

	// Clearing (the empty-call datagram) publishes too.
	p3, changed, _ := b.SetSelected(nil, true)
	if !changed || p3 == nil {
		t.Fatal("clear did not publish")
	}

	// The LIVENESS path is the deduped one: an unchanged (cleared, online)
	// pair publishes nothing; the flip does.
	if _, changed, _ := b.SetDeviceOnline(true, nil); changed {
		t.Fatal("unchanged liveness snapshot republished")
	}
	if _, changed, _ := b.SetDeviceOnline(false, nil); !changed {
		t.Fatal("liveness flip did not publish")
	}
	if _, changed, _ := b.SetDeviceOnline(false, nil); changed {
		t.Fatal("repeat liveness snapshot republished")
	}
}

func TestLastJSONRoundTrip(t *testing.T) {
	b := New(Meta{Schema: "1.0", Role: "bandmap"})
	if b.LastJSON() != "" {
		t.Fatal("LastJSON before first publish should be empty")
	}
	r := stationResolver()
	sel := r.FromN1MM(n1mmDXLog("VK9XY"), "dxlog")
	b.SetSelected(sel, true)

	var st State
	if err := json.Unmarshal([]byte(b.LastJSON()), &st); err != nil {
		t.Fatalf("LastJSON is not a State document: %v", err)
	}
	if st.Selected == nil || st.Selected.Call != "VK9XY" || !st.DeviceOnline {
		t.Fatalf("round-trip mismatch: %+v", st)
	}
}

func TestMetaPayload(t *testing.T) {
	b := New(Meta{Schema: "1.0", Role: "bandmap", Host: "shack-pc"})
	data, err := b.MetaPayload()
	if err != nil {
		t.Fatal(err)
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Role != "bandmap" || m.Host != "shack-pc" {
		t.Fatalf("meta mismatch: %+v", m)
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

	sel := r.FromLog4OM(log4om.Callsign{Call: "VK9XY"}, "log4om")
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

	sel := r.FromLog4OM(log4om.Callsign{Call: "VK9XY"}, "log4om")
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

	sel := r.FromLog4OM(log4om.Callsign{Call: "VK9XY"}, "log4om")
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

	sel := r.FromLog4OM(log4om.Callsign{Call: "XX9XX"}, "log4om")
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

	sel := r.FromLog4OM(log4om.Callsign{Call: "VK9XY"}, "log4om")
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
