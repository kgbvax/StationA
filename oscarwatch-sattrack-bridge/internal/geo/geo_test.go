package geo

import (
	"math"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// lngDiff is the shortest angular distance between two longitudes.
func lngDiff(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360)
	if d > 180 {
		d = 360 - d
	}
	return d
}

// JO32WE cell centre — the station locator the deploy seeds.
var shack = Observer{Lat: 52.1875, Lng: 7.875, AltM: 30}

func TestRoundTrip(t *testing.T) {
	sats := []Point{
		{Lat: 55.0, Lng: 10.0, AltKm: 420},     // ISS overhead-ish
		{Lat: 40.0, Lng: -20.0, AltKm: 790},    // SO-50 low in the WSW
		{Lat: 75.0, Lng: 60.0, AltKm: 800},     // high-latitude NE pass
		{Lat: 30.0, Lng: 30.0, AltKm: 1450},    // AO-7-ish, far SE
		{Lat: 52.1875, Lng: 7.875, AltKm: 500}, // straight up
		{Lat: 0.0, Lng: 13.0, AltKm: 35786.0},  // geostationary QO-100
	}
	for _, want := range sats {
		az, el, r := LookAngles(shack, want)
		got := SubPoint(shack, az, el, r)
		if !near(got.Lat, want.Lat, 1e-6) || lngDiff(got.Lng, want.Lng) > 1e-6 || !near(got.AltKm, want.AltKm, 1e-6) {
			t.Errorf("round trip %+v → az %.3f el %.3f r %.1f → %+v", want, az, el, r, got)
		}
	}
}

func TestZenith(t *testing.T) {
	p := SubPoint(shack, 123.0, 90.0, 400.0) // azimuth is irrelevant at zenith
	if !near(p.Lat, shack.Lat, 1e-6) || !near(p.Lng, shack.Lng, 1e-6) {
		t.Fatalf("zenith sub-point %+v, want observer position", p)
	}
	if !near(p.AltKm, 400.03, 1e-6) {
		t.Fatalf("zenith alt = %.6f, want 400.03 (range + observer height)", p.AltKm)
	}
}

// TestEquatorEastHorizon checks SubPoint against a closed form that does not
// share the ENU rotation: an observer at (0°, 0°, 0 m) sits at ECEF (a, 0, 0);
// a target due east on the horizon at slant range r is at ECEF (a, r, 0).
func TestEquatorEastHorizon(t *testing.T) {
	const r = 2000.0
	p := SubPoint(Observer{}, 90, 0, r)
	wantLng := deg(math.Atan2(r, wgs84A))
	wantAlt := math.Hypot(wgs84A, r) - wgs84A
	if !near(p.Lat, 0, 1e-9) || !near(p.Lng, wantLng, 1e-9) || !near(p.AltKm, wantAlt, 1e-6) {
		t.Fatalf("got %+v, want lat 0 lng %.6f alt %.3f", p, wantLng, wantAlt)
	}
}

// TestGeostationaryLookAngle checks LookAngles against the textbook spherical
// result: from 51.5°N on the satellite's meridian a GEO bird sits due south at
// el = atan((cosγ − R/(R+h)) / sinγ) ≈ 31°. The ellipsoid shifts it by a
// fraction of a degree.
func TestGeostationaryLookAngle(t *testing.T) {
	az, el, _ := LookAngles(Observer{Lat: 51.5, Lng: 0}, Point{Lat: 0, Lng: 0, AltKm: 35786})
	g := rad(51.5)
	re := 6378.137
	want := deg(math.Atan((math.Cos(g) - re/(re+35786)) / math.Sin(g)))
	if !near(az, 180, 1e-6) {
		t.Fatalf("az = %.6f, want 180", az)
	}
	if !near(el, want, 0.5) {
		t.Fatalf("el = %.3f, want ≈ %.3f", el, want)
	}
}

func TestSubPointDirection(t *testing.T) {
	// A LEO bird low in the east lies east of the station; one low in the
	// north lies north of it.
	east := SubPoint(shack, 90, 5, 2000)
	if east.Lng <= shack.Lng || !near(east.Lat, shack.Lat, 5) {
		t.Fatalf("east pass sub-point %+v", east)
	}
	north := SubPoint(shack, 0, 5, 2000)
	if north.Lat <= shack.Lat || !near(north.Lng, shack.Lng, 1e-6) {
		t.Fatalf("north pass sub-point %+v", north)
	}
	if east.AltKm < 200 || east.AltKm > 900 {
		t.Fatalf("2000 km slant at 5° el should be LEO height, got %.1f km", east.AltKm)
	}
}

func TestLngWrap(t *testing.T) {
	obs := Observer{Lat: 60, Lng: 179.5}
	p := SubPoint(obs, 90, 10, 1500)
	if p.Lng > 180 || p.Lng <= -180 {
		t.Fatalf("lng %.3f out of (-180, 180]", p.Lng)
	}
	if p.Lng > 0 {
		t.Fatalf("east of the antimeridian should wrap negative, got %.3f", p.Lng)
	}
}

func TestLocatorToLatLng(t *testing.T) {
	ll, ok := LocatorToLatLng("JO32we")
	if !ok || !near(ll.Lat, 52.1875, 1e-9) || !near(ll.Lng, 7.875, 1e-9) {
		t.Fatalf("JO32WE → %+v ok=%v", ll, ok)
	}
	ll, ok = LocatorToLatLng("JO32")
	if !ok || !near(ll.Lat, 52.5, 1e-9) || !near(ll.Lng, 7, 1e-9) {
		t.Fatalf("JO32 → %+v ok=%v", ll, ok)
	}
	for _, bad := range []string{"", "J", "JO3", "ZZ32", "JO3A", "JO32ZZ", "JO32WE12"} {
		if _, ok := LocatorToLatLng(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}
