package n1mm

import "testing"

// DXLog lookupinfo shape per the DXLog wiki ("Additional Information",
// broadcast type 4): lowercase root, txfreq in 10 Hz units, azimuth+distance
// from the CTY database.
const dxlogLookup = `<?xml version="1.0" encoding="utf-8"?>
<lookupinfo>
 <logger>DXLog</logger>
 <contestname>CQ-WW-CW</contestname>
 <mycall>DQ0M</mycall>
 <band>20</band>
 <txfreq>1402476</txfreq>
 <operator>DJ1XYZ</operator>
 <mode>CW</mode>
 <call>VK9XY</call>
 <countryprefix>VK9</countryprefix>
 <wpxprefix>VK9X</wpxprefix>
 <azimuth>62.4</azimuth>
 <distance>15420.3</distance>
 <stationid>1</stationid>
 <stationtype>Run</stationtype>
 <period>1</period>
 <reason>SpaceOrTab</reason>
</lookupinfo>`

// N1MM lookupinfo is contactinfo-shaped (the N1MM doc: "structure identical to
// the ContactInfo packet"): app instead of logger, rxfreq present, gridsquare
// usually empty, no azimuth/distance.
const n1mmLookup = `<lookupinfo>
 <app>N1MM</app>
 <contestname>CWOPS</contestname>
 <contestnr>73</contestnr>
 <timestamp>2020-01-17 16:43:38</timestamp>
 <mycall>W2XYZ</mycall>
 <band>20m</band>
 <rxfreq>14025000</rxfreq>
 <txfreq>14025000</txfreq>
 <operator>W2XYZ</operator>
 <mode>USB</mode>
 <call>K1ABC</call>
 <countryprefix>K</countryprefix>
 <wpxprefix>K1</wpxprefix>
 <gridsquare></gridsquare>
 <zone>5</zone>
</lookupinfo>`

const radioInfo = `<RadioInfo><app>DXLog</app><Freq>1402476</Freq><Mode>CW</Mode></RadioInfo>`
const contactInfo = `<contactinfo><call>VK9XY</call></contactinfo>`

func TestClassify(t *testing.T) {
	cases := map[string]RootKind{
		"lookupinfo":  KindLookupInfo,
		"LookupInfo":  KindLookupInfo,
		"contactinfo": KindContactInfo,
		"RadioInfo":   KindRadioInfo,
		"spot":        KindSpot,
		"Radio":       KindOther,
		"":            KindOther,
	}
	for root, want := range cases {
		if got := Classify(root); got != want {
			t.Errorf("Classify(%q) = %v, want %v", root, got, want)
		}
	}
}

func TestDecodeLookupInfoDXLog(t *testing.T) {
	li, err := DecodeLookupInfo([]byte(dxlogLookup))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if li.Call != "VK9XY" {
		t.Errorf("Call = %q", li.Call)
	}
	if li.FreqTx10 != 1402476 {
		t.Errorf("FreqTx10 = %d, want 1402476 (10 Hz units, verbatim)", li.FreqTx10)
	}
	if li.Band != "20" {
		t.Errorf("Band = %q (raw DXLog MHz form; resolver derives from freq)", li.Band)
	}
	if li.Mode != "CW" {
		t.Errorf("Mode = %q", li.Mode)
	}
	if li.Azimuth != 62.4 || li.DistanceKm != 15420.3 {
		t.Errorf("Az/Dist = (%v, %v)", li.Azimuth, li.DistanceKm)
	}
	if li.CountryPrefix != "VK9" || li.WPXPrefix != "VK9X" {
		t.Errorf("prefixes = (%q, %q)", li.CountryPrefix, li.WPXPrefix)
	}
	if li.Reason != "SpaceOrTab" {
		t.Errorf("Reason = %q", li.Reason)
	}
	if li.Logger != "DXLog" {
		t.Errorf("Logger = %q", li.Logger)
	}
	if li.Grid != "" {
		t.Errorf("Grid = %q, want empty (DXLog lookupinfo has no grid)", li.Grid)
	}
}

func TestDecodeLookupInfoN1MM(t *testing.T) {
	li, err := DecodeLookupInfo([]byte(n1mmLookup))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if li.Call != "K1ABC" {
		t.Errorf("Call = %q", li.Call)
	}
	if li.FreqTx10 != 14025000 || li.FreqRx10 != 14025000 {
		t.Errorf("Freq = (%d, %d)", li.FreqTx10, li.FreqRx10)
	}
	if li.Band != "20m" {
		t.Errorf("Band = %q", li.Band)
	}
	if li.Mode != "USB" {
		t.Errorf("Mode = %q", li.Mode)
	}
	if li.Azimuth != 0 || li.DistanceKm != 0 {
		t.Errorf("Az/Dist = (%v, %v), want absent", li.Azimuth, li.DistanceKm)
	}
	if li.Logger != "N1MM" {
		t.Errorf("Logger = %q (app fallback)", li.Logger)
	}
}

func TestDecodeLookupInfoRejectsOtherRoots(t *testing.T) {
	if _, err := DecodeLookupInfo([]byte(radioInfo)); err == nil {
		t.Fatal("RadioInfo accepted as lookupinfo")
	}
	if _, err := DecodeLookupInfo([]byte("not xml at all")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestDecodeLookupInfoEmptyCallIsMeaningful(t *testing.T) {
	// The operator wiped the entry window: call empty, decode must SUCCEED so
	// the resolver's nil (clear) path runs.
	datagram := `<lookupinfo><app>N1MM</app><call></call><txfreq>14025000</txfreq></lookupinfo>`
	li, err := DecodeLookupInfo([]byte(datagram))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if li.Call != "" {
		t.Fatalf("Call = %q, want empty", li.Call)
	}
}

func TestParseFreq10Garbage(t *testing.T) {
	datagram := `<lookupinfo><call>AB1CDE</call><txfreq>14.025.00</txfreq></lookupinfo>`
	li, err := DecodeLookupInfo([]byte(datagram))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if li.FreqTx10 != 0 {
		t.Fatalf("garbage freq surfaced as %d, want 0", li.FreqTx10)
	}
}
