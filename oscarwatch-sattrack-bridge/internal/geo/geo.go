// Package geo turns a tracker's look angle (azimuth, elevation, slant range
// from the station) into the satellite's sub-satellite point — the lat/lng a
// map pins and the altitude a footprint circle needs. No orbital elements are
// involved: the look angle plus the observer's position fixes the satellite
// in space, and WGS84 turns that back into geodetic coordinates.
//
// Chain: observer geodetic → ECEF; the local East-North-Up vector
// (e = ρ·cosEl·sinAz, n = ρ·cosEl·cosAz, u = ρ·sinEl) rotated into ECEF and
// added; satellite ECEF → geodetic. LookAngles is the inverse (used by the
// tests to build ground truth, and handy for sanity checks).
//
// Also carries the Maidenhead decoder (a copy of logger-spot-bridge's — Go's
// internal/ rule forbids importing it across modules). Pure math, no I/O.
package geo

import (
	"math"
	"strings"
)

// WGS84 ellipsoid, kilometres.
const (
	wgs84A  = 6378.137
	wgs84F  = 1 / 298.257223563
	wgs84E2 = wgs84F * (2 - wgs84F)
)

// LatLng is a geographic point in degrees (lng East-positive), the stationa
// convention (matches the console's `LatLng` typedef).
type LatLng struct {
	Lat float64
	Lng float64
}

// Observer is the station position the tracker computes look angles from.
type Observer struct {
	Lat  float64 // degrees, North-positive
	Lng  float64 // degrees, East-positive
	AltM float64 // metres above the WGS84 ellipsoid (MSL is close enough)
}

// Point is a geodetic position: the sub-satellite point plus height.
type Point struct {
	Lat   float64 // degrees
	Lng   float64 // degrees, (-180, 180]
	AltKm float64 // km above the ellipsoid
}

type ecef struct{ x, y, z float64 }

// SubPoint returns the geodetic position of a target seen from obs at the
// given azimuth (degrees, 0 = north, clockwise), elevation (degrees above the
// horizon) and slant range (km).
func SubPoint(obs Observer, azDeg, elDeg, rangeKm float64) Point {
	o := toECEF(rad(obs.Lat), rad(obs.Lng), obs.AltM/1000)

	az, el := rad(azDeg), rad(elDeg)
	e := rangeKm * math.Cos(el) * math.Sin(az)
	n := rangeKm * math.Cos(el) * math.Cos(az)
	u := rangeKm * math.Sin(el)

	sinPhi, cosPhi := math.Sincos(rad(obs.Lat))
	sinLam, cosLam := math.Sincos(rad(obs.Lng))
	sat := ecef{
		x: o.x - sinLam*e - sinPhi*cosLam*n + cosPhi*cosLam*u,
		y: o.y + cosLam*e - sinPhi*sinLam*n + cosPhi*sinLam*u,
		z: o.z + cosPhi*n + sinPhi*u,
	}
	lat, lng, h := fromECEF(sat)
	return Point{Lat: deg(lat), Lng: normalizeLng(deg(lng)), AltKm: h}
}

// LookAngles is the inverse of SubPoint: azimuth (0..360), elevation and slant
// range from obs to p.
func LookAngles(obs Observer, p Point) (azDeg, elDeg, rangeKm float64) {
	o := toECEF(rad(obs.Lat), rad(obs.Lng), obs.AltM/1000)
	t := toECEF(rad(p.Lat), rad(p.Lng), p.AltKm)
	dx, dy, dz := t.x-o.x, t.y-o.y, t.z-o.z

	sinPhi, cosPhi := math.Sincos(rad(obs.Lat))
	sinLam, cosLam := math.Sincos(rad(obs.Lng))
	e := -sinLam*dx + cosLam*dy
	n := -sinPhi*cosLam*dx - sinPhi*sinLam*dy + cosPhi*dz
	u := cosPhi*cosLam*dx + cosPhi*sinLam*dy + sinPhi*dz

	rangeKm = math.Sqrt(dx*dx + dy*dy + dz*dz)
	elDeg = deg(math.Asin(u / rangeKm))
	azDeg = deg(math.Atan2(e, n))
	if azDeg < 0 {
		azDeg += 360
	}
	return azDeg, elDeg, rangeKm
}

func toECEF(lat, lng, hKm float64) ecef {
	sinPhi, cosPhi := math.Sincos(lat)
	sinLam, cosLam := math.Sincos(lng)
	nu := wgs84A / math.Sqrt(1-wgs84E2*sinPhi*sinPhi)
	return ecef{
		x: (nu + hKm) * cosPhi * cosLam,
		y: (nu + hKm) * cosPhi * sinLam,
		z: (nu*(1-wgs84E2) + hKm) * sinPhi,
	}
}

// fromECEF converts to geodetic by fixed-point iteration on latitude. For
// points well above the surface (every satellite) it converges to sub-mm in a
// handful of rounds; the height formula switches near the poles, where
// p/cosφ is ill-conditioned.
func fromECEF(c ecef) (lat, lng, hKm float64) {
	lng = math.Atan2(c.y, c.x)
	p := math.Hypot(c.x, c.y)
	lat = math.Atan2(c.z, p*(1-wgs84E2))
	for range 10 {
		sinPhi, cosPhi := math.Sincos(lat)
		nu := wgs84A / math.Sqrt(1-wgs84E2*sinPhi*sinPhi)
		if math.Abs(cosPhi) > 1e-6 {
			hKm = p/cosPhi - nu
		} else {
			hKm = math.Abs(c.z) - nu*(1-wgs84E2)
		}
		next := math.Atan2(c.z, p*(1-wgs84E2*nu/(nu+hKm)))
		if math.Abs(next-lat) < 1e-12 {
			lat = next
			break
		}
		lat = next
	}
	return lat, lng, hKm
}

// LocatorToLatLng decodes a Maidenhead grid locator (2, 4 or 6 characters) to
// the center of its cell. Returns ok=false for anything shorter than 2
// characters or with malformed pairs.
func LocatorToLatLng(locator string) (LatLng, bool) {
	s := strings.ToUpper(strings.TrimSpace(locator))
	if len(s) < 2 || len(s)%2 != 0 || len(s) > 6 {
		return LatLng{}, false
	}
	if s[0] < 'A' || s[0] > 'R' || s[1] < 'A' || s[1] > 'R' {
		return LatLng{}, false
	}
	lng := float64(s[0]-'A')*20.0 - 180.0
	lat := float64(s[1]-'A')*10.0 - 90.0

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
