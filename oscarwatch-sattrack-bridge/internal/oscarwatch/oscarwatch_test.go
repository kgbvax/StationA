package oscarwatch

import "testing"

// Fixtures are the frame shapes from the OscarWatch Satellite-link v1 help
// page (see docs/oscarwatch-sattrack-bridge-mqtt-api.md §9).
const inRangeSO50 = `{
  "type": "satelliteStatus",
  "version": 1,
  "timestampUtc": "2026-07-07T11:04:00.000Z",
  "inRange": true,
  "satellite": {"name": "SO-50", "noradId": "27607", "modeType": "FM VOICE"},
  "frequencies": {
    "uplinkHz": 435300000, "downlinkHz": 145850000,
    "uplinkMode": "FM", "downlinkMode": "FM",
    "nominalUplinkKHz": 435300, "nominalDownlinkKHz": 145850,
    "isBeaconOnly": false
  },
  "bands": {"tx": "70cm", "rx": "2m"},
  "tracking": {
    "azimuthDeg": 91.7, "elevationDeg": 1.9, "rangeKm": 2100.5,
    "rangeRateKmPerSec": -4.92, "isSunlit": true
  },
  "dopplerStrategy": "full",
  "wispDde": "SO-50 AZ91,7 EL1,9 UP435300000 UFM DN145850000 DFM MA0,0 RR-4,92"
}`

const noSatellite = `{
  "type": "satelliteStatus",
  "version": 1,
  "timestampUtc": "2026-07-07T11:20:00.000Z",
  "inRange": false,
  "satellite": null,
  "wispDde": "** NO SATELLITE **"
}`

const qsoLogged = `{
  "type": "qsoLogged", "version": 1, "timestampUtc": "2026-07-11T14:30:05.000Z",
  "logbook": {"id": 3, "name": "Field day", "myCallsign": "G0ABC", "myGridSquare": "IO91"},
  "qso": {"id": 42, "call": "DL1ABC", "satellite": {"name": "SO-50", "noradId": "27607"}},
  "adif": "<CALL:6>DL1ABC<EOR>"
}`

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		"satelliteStatus": KindStatus,
		"qsoLogged":       KindQSO,
		"qsoUpdated":      KindQSO,
		"qsoDeleted":      KindQSO,
		"passPredicted":   KindOther,
		"":                KindOther,
	}
	for typ, want := range cases {
		if got := Classify(typ); got != want {
			t.Errorf("Classify(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestPeekHeader(t *testing.T) {
	h, err := PeekHeader([]byte(qsoLogged))
	if err != nil {
		t.Fatal(err)
	}
	if h.Type != TypeQSOLogged || h.Version != 1 {
		t.Fatalf("header = %+v", h)
	}
	if _, err := PeekHeader([]byte(`{"version":1}`)); err == nil {
		t.Fatal("frame without type accepted")
	}
	if _, err := PeekHeader([]byte(`not json`)); err == nil {
		t.Fatal("malformed frame accepted")
	}
}

func TestDecodeStatusInRange(t *testing.T) {
	s, err := DecodeStatus([]byte(inRangeSO50))
	if err != nil {
		t.Fatal(err)
	}
	if !s.InRange || s.Version != 1 || s.TimestampUTC != "2026-07-07T11:04:00.000Z" {
		t.Fatalf("envelope: %+v", s)
	}
	if s.Satellite == nil || s.Satellite.Name != "SO-50" || s.Satellite.NoradID != "27607" || s.Satellite.ModeType != "FM VOICE" {
		t.Fatalf("satellite: %+v", s.Satellite)
	}
	f := s.Frequencies
	if f == nil || f.UplinkHz != 435300000 || f.DownlinkHz != 145850000 || f.UplinkMode != "FM" || f.DownlinkMode != "FM" || f.IsBeaconOnly {
		t.Fatalf("frequencies: %+v", f)
	}
	if s.Bands == nil || s.Bands.TX != "70cm" || s.Bands.RX != "2m" {
		t.Fatalf("bands: %+v", s.Bands)
	}
	tr := s.Tracking
	if tr == nil || *tr.AzimuthDeg != 91.7 || *tr.ElevationDeg != 1.9 || *tr.RangeKm != 2100.5 ||
		*tr.RangeRateKmPerSec != -4.92 || !*tr.IsSunlit {
		t.Fatalf("tracking: %+v", tr)
	}
	if s.DopplerStrategy != "full" {
		t.Fatalf("doppler = %q", s.DopplerStrategy)
	}
}

func TestDecodeStatusNoSatellite(t *testing.T) {
	s, err := DecodeStatus([]byte(noSatellite))
	if err != nil {
		t.Fatal(err)
	}
	if s.InRange || s.Satellite != nil || s.Tracking != nil || s.Frequencies != nil {
		t.Fatalf("no-satellite frame decoded as %+v", s)
	}
}

func TestDecodeStatusForwardCompatible(t *testing.T) {
	// Version 2 with unknown keys at every level still decodes the v1 fields.
	frame := `{"type":"satelliteStatus","version":2,"inRange":true,"newTopLevel":{"x":1},
	  "satellite":{"name":"AO-91","noradId":43017,"modeType":"FM VOICE","operator":"AMSAT"},
	  "tracking":{"azimuthDeg":10,"elevationDeg":45,"rangeKm":900,"rangeRateKmPerSec":0,"isSunlit":false,"dopplerHz":123}}`
	s, err := DecodeStatus([]byte(frame))
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != 2 || s.Satellite.Name != "AO-91" {
		t.Fatalf("decoded %+v", s)
	}
	if s.Satellite.NoradID != "43017" {
		t.Fatalf("numeric noradId = %q, want \"43017\"", s.Satellite.NoradID)
	}
	if s.Tracking.RangeRateKmPerSec == nil || *s.Tracking.RangeRateKmPerSec != 0 {
		t.Fatal("explicit 0 range rate lost")
	}
	if s.Tracking.IsSunlit == nil || *s.Tracking.IsSunlit {
		t.Fatal("explicit false sunlit lost")
	}
}

func TestDecodeStatusMissingTrackingKeys(t *testing.T) {
	frame := `{"type":"satelliteStatus","version":1,"inRange":true,
	  "satellite":{"name":"ISS","noradId":"25544","modeType":"APRS"},"tracking":{"azimuthDeg":200}}`
	s, err := DecodeStatus([]byte(frame))
	if err != nil {
		t.Fatal(err)
	}
	if s.Tracking.AzimuthDeg == nil || s.Tracking.ElevationDeg != nil || s.Tracking.IsSunlit != nil {
		t.Fatalf("missing keys not nil: %+v", s.Tracking)
	}
}

func TestDecodeStatusMalformed(t *testing.T) {
	for _, bad := range []string{
		`{"type":"satelliteStatus",`,
		`{"type":"satelliteStatus","satellite":{"noradId":true}}`,
		`{"type":"satelliteStatus","inRange":"yes"}`,
	} {
		if _, err := DecodeStatus([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestFlexStringNull(t *testing.T) {
	s, err := DecodeStatus([]byte(`{"type":"satelliteStatus","satellite":{"name":"X","noradId":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Satellite.NoradID != "" {
		t.Fatalf("null noradId = %q", s.Satellite.NoradID)
	}
}
