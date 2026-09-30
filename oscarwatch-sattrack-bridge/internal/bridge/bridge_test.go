package bridge

import (
	"encoding/json"
	"testing"
	"time"

	"oscarwatch-sattrack-bridge/internal/geo"
	"oscarwatch-sattrack-bridge/internal/oscarwatch"
)

var t0 = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func so50(t *testing.T) *oscarwatch.Status {
	t.Helper()
	s, err := oscarwatch.DecodeStatus([]byte(`{
	  "type":"satelliteStatus","version":1,"timestampUtc":"2026-07-07T11:04:00.000Z","inRange":true,
	  "satellite":{"name":"SO-50","noradId":"27607","modeType":"FM VOICE"},
	  "frequencies":{"uplinkHz":435300000,"downlinkHz":145850000,"uplinkMode":"FM","downlinkMode":"FM","isBeaconOnly":false},
	  "bands":{"tx":"70cm","rx":"2m"},
	  "tracking":{"azimuthDeg":91.7,"elevationDeg":1.9,"rangeKm":2100.5,"rangeRateKmPerSec":-4.92,"isSunlit":true},
	  "dopplerStrategy":"downlinkOnly"}`))
	if err != nil {
		t.Fatal(err)
	}
	return &s
}

// decode round-trips a State through JSON into a generic map, the way every
// consumer sees it.
func decode(t *testing.T, st State) map[string]any {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuildTracking(t *testing.T) {
	obs := &geo.Observer{Lat: 52.1875, Lng: 7.875}
	m := decode(t, Build(Input{Online: true, Status: so50(t)}, obs, t0))

	want := map[string]any{
		"ts": "2026-09-30T12:00:00Z", "device_online": true, "tracking": true, "in_range": true,
		"sat_name": "SO-50", "norad_id": "27607", "mode_type": "FM VOICE",
		"az": 91.7, "el": 1.9, "range_km": 2100.5, "range_rate_km_s": -4.92, "sunlit": true,
		"uplink_hz": 435300000.0, "downlink_hz": 145850000.0,
		"uplink_mode": "fm", "downlink_mode": "fm", "uplink_band": "70cm", "downlink_band": "2m",
		"beacon_only": false, "doppler": "downlink_only",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %#v, want %#v", k, m[k], v)
		}
	}
	if _, ok := m["error"]; ok {
		t.Error("error key present while online")
	}
	// Sub-point: east of the station (az 91.7), LEO height (the doc fixture
	// geometry works out to ~400 km), and consistent
	// with the look angle it came from.
	lat, lng, alt := m["sub_lat"].(float64), m["sub_lng"].(float64), m["alt_km"].(float64)
	if lng <= obs.Lng || alt < 300 || alt > 1000 {
		t.Fatalf("sub-point %.3f,%.3f alt %.1f", lat, lng, alt)
	}
	az, el, r := geo.LookAngles(*obs, geo.Point{Lat: lat, Lng: lng, AltKm: alt})
	if abs(az-91.7) > 0.05 || abs(el-1.9) > 0.05 || abs(r-2100.5) > 0.5 {
		t.Fatalf("published sub-point re-projects to az %.3f el %.3f r %.1f", az, el, r)
	}
}

func TestBuildNoObserver(t *testing.T) {
	m := decode(t, Build(Input{Online: true, Status: so50(t)}, nil, t0))
	for _, k := range []string{"sub_lat", "sub_lng", "alt_km"} {
		if v, ok := m[k]; !ok || v != nil {
			t.Errorf("%s = %#v (present=%v), want null", k, v, ok)
		}
	}
	if m["az"] != 91.7 {
		t.Fatal("look angle lost without observer")
	}
}

// satelliteKeys are the keys that must be null when nothing is tracked.
var satelliteKeys = []string{
	"sat_name", "norad_id", "mode_type", "az", "el", "range_km", "range_rate_km_s", "sunlit",
	"sub_lat", "sub_lng", "alt_km", "uplink_hz", "downlink_hz", "uplink_mode", "downlink_mode",
	"uplink_band", "downlink_band", "beacon_only", "doppler",
}

func assertCleared(t *testing.T, m map[string]any) {
	t.Helper()
	if m["tracking"] != false || m["in_range"] != false {
		t.Errorf("tracking=%v in_range=%v, want false", m["tracking"], m["in_range"])
	}
	for _, k := range satelliteKeys {
		if v, ok := m[k]; !ok || v != nil {
			t.Errorf("%s = %#v (present=%v), want null", k, v, ok)
		}
	}
}

func TestBuildNoSatellite(t *testing.T) {
	s, err := oscarwatch.DecodeStatus([]byte(`{"type":"satelliteStatus","version":1,"inRange":false,"satellite":null,"wispDde":"** NO SATELLITE **"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, Build(Input{Online: true, Status: &s}, &geo.Observer{}, t0))
	assertCleared(t, m)
	if m["device_online"] != true {
		t.Fatal("device_online should stay true")
	}
}

func TestBuildBeforeSnapshot(t *testing.T) {
	assertCleared(t, decode(t, Build(Input{Online: true}, nil, t0)))
}

func TestBuildOfflineClearsSatellite(t *testing.T) {
	// Even with a stale frame still in hand, an offline input publishes no
	// satellite: consumers must not pin a frozen position.
	m := decode(t, Build(Input{Online: false, Err: "oscarwatch: connection refused", Status: so50(t)}, nil, t0))
	assertCleared(t, m)
	if m["device_online"] != false || m["error"] != "oscarwatch: connection refused" {
		t.Fatalf("device_online=%v error=%v", m["device_online"], m["error"])
	}
}

func TestBuildBelowHorizon(t *testing.T) {
	// "Only broadcast when above horizon" off: a focused bird below the
	// horizon is tracked but not in range, with a negative elevation.
	s := so50(t)
	s.InRange = false
	el := -12.3
	s.Tracking.ElevationDeg = &el
	m := decode(t, Build(Input{Online: true, Status: s}, nil, t0))
	if m["tracking"] != true || m["in_range"] != false || m["el"] != -12.3 {
		t.Fatalf("tracking=%v in_range=%v el=%v", m["tracking"], m["in_range"], m["el"])
	}
}

func TestBuildZeroFrequencyIsNull(t *testing.T) {
	s := so50(t)
	s.Frequencies.UplinkHz = 0
	s.Frequencies.IsBeaconOnly = true
	m := decode(t, Build(Input{Online: true, Status: s}, nil, t0))
	if m["uplink_hz"] != nil || m["beacon_only"] != true {
		t.Fatalf("uplink_hz=%v beacon_only=%v", m["uplink_hz"], m["beacon_only"])
	}
}

func TestCanonicalMode(t *testing.T) {
	cases := map[string]string{
		"FM": "fm", "fmn": "fm", " NFM ": "fm", "USB": "usb", "LSB": "lsb", "CW": "cw",
		"AM": "am", "BPSK": "data", "PKT": "data", "DATA": "data", "FSK": "data",
		"SSB": "", "": "", "SSTV": "", "FM VOICE": "",
	}
	for in, want := range cases {
		got := CanonicalMode(in)
		switch {
		case want == "" && got != nil:
			t.Errorf("CanonicalMode(%q) = %q, want nil", in, *got)
		case want != "" && (got == nil || *got != want):
			t.Errorf("CanonicalMode(%q) = %v, want %q", in, got, want)
		}
	}
}

func TestDoppler(t *testing.T) {
	for in, want := range map[string]string{"full": "full", "downlinkOnly": "downlink_only", "uplinkOnly": "uplink_only", "someFuture": "someFuture"} {
		if got := doppler(in); got == nil || *got != want {
			t.Errorf("doppler(%q) = %v, want %q", in, got, want)
		}
	}
	if doppler("") != nil {
		t.Error("empty doppler not nil")
	}
}

func TestUpdateDedupIgnoresTS(t *testing.T) {
	b := New(Meta(MetaOptions{}))
	in := Input{Online: true, Status: so50(t)}

	p1, changed, err := b.Update(Build(in, nil, t0))
	if err != nil || !changed || len(p1) == 0 {
		t.Fatalf("first update: changed=%v err=%v", changed, err)
	}
	if _, changed, _ := b.Update(Build(in, nil, t0.Add(5*time.Second))); changed {
		t.Fatal("same content with a later ts republished")
	}
	if b.LastJSON() != string(p1) {
		t.Fatal("LastJSON is not the last published payload")
	}

	moved := so50(t)
	az := 92.0
	moved.Tracking.AzimuthDeg = &az
	p2, changed, _ := b.Update(Build(Input{Online: true, Status: moved}, nil, t0.Add(time.Second)))
	if !changed || b.LastJSON() != string(p2) {
		t.Fatal("changed azimuth not published")
	}

	if _, changed, _ := b.Update(Build(Input{Online: false, Err: "x"}, nil, t0)); !changed {
		t.Fatal("going offline not published")
	}
}

func TestMetaPayload(t *testing.T) {
	b := New(Meta(MetaOptions{Slot: "sat-track", Location: "bauwagen", Host: "scmino"}))
	raw, err := b.MetaPayload()
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Schema   string         `json:"schema"`
		Role     string         `json:"role"`
		Location string         `json:"location"`
		Host     string         `json:"host"`
		Link     string         `json:"link"`
		Caps     map[string]any `json:"capabilities"`
		Expose   struct {
			Device map[string]any   `json:"device"`
			Fields []map[string]any `json:"fields"`
		} `json:"expose"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Schema != "1.0" || m.Role != "sat-track" || m.Location != "bauwagen" || m.Host != "scmino" || m.Link != "websocket" {
		t.Fatalf("meta identity: %+v", m)
	}
	if m.Caps["source"] != "oscarwatch" || m.Caps["protocol"] != 1.0 {
		t.Fatalf("capabilities: %+v", m.Caps)
	}
	if m.Expose.Device["name"] != "sat-track" || len(m.Expose.Fields) == 0 {
		t.Fatalf("expose: %+v", m.Expose)
	}

	// Every exposed key must exist in /state (a typo would render an HA
	// sensor that is forever unknown).
	stateKeys := decode(t, Build(Input{Online: true, Status: so50(t)}, &geo.Observer{}, t0))
	stateKeys["error"] = nil // omitempty; present whenever the link is down
	for _, f := range m.Expose.Fields {
		k := f["key"].(string)
		if _, ok := stateKeys[k]; !ok {
			t.Errorf("expose key %q not in /state", k)
		}
		switch f["type"] {
		case "number", "string", "boolean":
		default:
			t.Errorf("expose %q has type %v", k, f["type"])
		}
	}
}

func TestTopicsFor(t *testing.T) {
	tp := TopicsFor("muehle", "uhf", "sat-track")
	if tp.Meta != "muehle/uhf/sat-track/meta" || tp.State != "muehle/uhf/sat-track/state" || tp.Status != "muehle/uhf/sat-track/status" {
		t.Fatalf("topics: %+v", tp)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
