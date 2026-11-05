package geo

import (
	"math"
	"testing"
)

func almostEq(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestLocatorToLatLngCenters(t *testing.T) {
	// JO31 (Ruhr area): J field → lng 0..20; second char O → lat 50..60;
	// digits 3,1 → +6 lng, +1 lat; 4-char center offset → (7.0, 51.5).
	got, ok := LocatorToLatLng("JO31")
	if !ok {
		t.Fatal("JO31 rejected")
	}
	if !almostEq(got.Lat, 51.5, 0.001) || !almostEq(got.Lng, 7.0, 0.001) {
		t.Fatalf("JO31 → (%v, %v), want (51.5, 7.0)", got.Lat, got.Lng)
	}

	// 6-char adds the subsquare center offset (5' lng, 2.5' lat per cell).
	got6, _ := LocatorToLatLng("JO31aa")
	if !almostEq(got6.Lat, 51.0208333, 0.0001) || !almostEq(got6.Lng, 6.0416667, 0.0001) {
		t.Fatalf("JO31aa → (%v, %v), want (51.0208, 6.0417)", got6.Lat, got6.Lng)
	}

	if _, ok := LocatorToLatLng(""); ok {
		t.Fatal("empty locator accepted")
	}
	if _, ok := LocatorToLatLng("ZZ99"); ok {
		t.Fatal("out-of-range locator accepted")
	}
}

func TestDestinationAndInverseRoundTrip(t *testing.T) {
	jo31, _ := LocatorToLatLng("JO31")

	// DXLog-style: bearing 62.4°, distance 15420 km.
	dest := Destination(jo31, 62.4, 15420.0)

	az, dist := BearingDistance(jo31, dest)
	if !almostEq(az, 62.4, 0.01) {
		t.Fatalf("bearing round-trip: got %v want 62.4", az)
	}
	if !almostEq(dist, 15420.0, 0.5) {
		t.Fatalf("distance round-trip: got %v want 15420", dist)
	}
}

func TestBearingCardinals(t *testing.T) {
	// Equator sanity: due east / north / west bearings between cardinal points.
	eastAz, eastDist := BearingDistance(LatLng{Lat: 0, Lng: 0}, LatLng{Lat: 0, Lng: 10})
	if !almostEq(eastAz, 90, 0.01) || !almostEq(eastDist, math.Pi/18*EarthRadiusKm, 1) {
		t.Fatalf("east bearing: got (%v, %v)", eastAz, eastDist)
	}
	northAz, _ := BearingDistance(LatLng{Lat: 0, Lng: 0}, LatLng{Lat: 10, Lng: 0})
	if !almostEq(northAz, 0, 0.01) {
		t.Fatalf("north bearing: got %v", northAz)
	}
}

func TestDestinationLngWrap(t *testing.T) {
	// Easting past the antimeridian wraps into (-180, 180]: 200 km east of
	// 179.5° is 181.3° → -178.7°.
	start := LatLng{Lat: 0, Lng: 179.5}
	dest := Destination(start, 90, 200.0)
	if dest.Lng >= 0 || dest.Lng < -180 {
		t.Fatalf("wrap: got %v, want just past the antimeridian (negative)", dest.Lng)
	}
	if !almostEq(dest.Lng, -178.70, 0.05) {
		t.Fatalf("wrap: got %v, want ≈ -178.70", dest.Lng)
	}
}
