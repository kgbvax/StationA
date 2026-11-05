// Package geo holds the great-circle and Maidenhead math the selected-station
// resolution needs: locator → lat/lng, the direct problem (start, bearing,
// distance → destination) that turns DXLog's azimuth+distance into a map
// position, and the inverse problem (two points → bearing + distance) that
// fills the bearing/distance read-out when only coordinates are known.
//
// Pure math, no I/O. Earth radius 6371 km matches the console's
// hf_console/lib/dxspot/projection.dart so both ends agree on distances.
package geo

import (
	"math"
	"strings"
)

// EarthRadiusKm is the mean Earth radius used across stationa components.
const EarthRadiusKm = 6371.0

// LatLng is a geographic point in degrees (lng East-positive), the stationa
// convention (matches the console's `LatLng` typedef).
type LatLng struct {
	Lat float64
	Lng float64
}

// LocatorToLatLng decodes a Maidenhead grid locator (2, 4 or 6 characters) to
// the center of its cell, ported from horstreporter's
// `spot.go:locatorToLatLng` (the console carries the same port). Returns
// ok=false for anything shorter than 2 characters or with malformed pairs.
func LocatorToLatLng(locator string) (LatLng, bool) {
	s := strings.ToUpper(strings.TrimSpace(locator))
	if len(s) < 2 || len(s)%2 != 0 {
		return LatLng{}, false
	}

	lng := float64(s[0]-'A')*20.0 - 180.0
	lat := float64(s[1]-'A')*10.0 - 90.0
	if s[0] < 'A' || s[0] > 'R' || s[1] < 'A' || s[1] > 'R' {
		return LatLng{}, false
	}

	if len(s) >= 4 {
		if s[2] < '0' || s[2] > '9' || s[3] < '0' || s[3] > '9' {
			return LatLng{}, false
		}
		lng += float64(s[2]-'0') * 2.0
		lat += float64(s[3]-'0') * 1.0
		if len(s) >= 6 {
			if s[4] < 'A' || s[4] > 'X' || s[5] < 'A' || s[5] > 'X' {
				return LatLng{}, false
			}
			lng += float64(s[4]-'A')*(5.0/60.0) + (5.0 / 120.0)
			lat += float64(s[5]-'A')*(2.5/60.0) + (2.5 / 120.0)
		} else {
			lng += 1.0
			lat += 0.5
		}
	} else {
		lng += 10.0
		lat += 5.0
	}
	return LatLng{Lat: lat, Lng: normalizeLng(lng)}, true
}

// Destination returns the point reached from start after travelling distanceKm
// along the great-circle initial bearing (degrees, 0 = north). This is the
// direct problem; it turns DXLog's azimuth+distance (computed from its CTY
// centroid) into the lat/lng the Mercator map pins.
func Destination(start LatLng, bearingDeg, distanceKm float64) LatLng {
	lat1 := rad(start.Lat)
	lng1 := rad(start.Lng)
	theta := rad(bearingDeg)
	delta := distanceKm / EarthRadiusKm

	sinLat2 := math.Sin(lat1)*math.Cos(delta) + math.Cos(lat1)*math.Sin(delta)*math.Cos(theta)
	// Clamp: a distance at the antipode can push the argument just past ±1.
	sinLat2 = math.Min(1.0, math.Max(-1.0, sinLat2))
	lat2 := math.Asin(sinLat2)

	lng2 := lng1 + math.Atan2(
		math.Sin(theta)*math.Sin(delta)*math.Cos(lat1),
		math.Cos(delta)-math.Sin(lat1)*sinLat2,
	)
	return LatLng{Lat: deg(lat2), Lng: normalizeLng(deg(lng2))}
}

// BearingDistance returns the initial great-circle bearing (degrees 0..360)
// from a to b and the great-circle distance in km. The inverse problem.
func BearingDistance(a, b LatLng) (bearingDeg, distanceKm float64) {
	lat1 := rad(a.Lat)
	lat2 := rad(b.Lat)
	dLng := rad(b.Lng - a.Lng)

	y := math.Sin(dLng) * math.Cos(lat2)
	x := math.Cos(lat1)*math.Sin(lat2) - math.Sin(lat1)*math.Cos(lat2)*math.Cos(dLng)
	theta := math.Atan2(y, x)

	dLng0 := rad(b.Lng) - rad(a.Lng)
	c := math.Acos(math.Min(1.0, math.Max(-1.0,
		math.Sin(lat1)*math.Sin(lat2)+math.Cos(lat1)*math.Cos(lat2)*math.Cos(dLng0))))

	bearing := deg(theta)
	if bearing < 0 {
		bearing += 360.0
	}
	return bearing, c * EarthRadiusKm
}

func rad(d float64) float64 { return d * math.Pi / 180.0 }
func deg(r float64) float64 { return r * 180.0 / math.Pi }

// normalizeLng wraps lng into (-180, 180].
func normalizeLng(lng float64) float64 {
	lng = math.Mod(lng, 360.0)
	switch {
	case lng > 180.0:
		lng -= 360.0
	case lng <= -180.0:
		lng += 360.0
	}
	return lng
}
