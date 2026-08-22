// projection.dart — azimuthal equidistant (AEQD) projection, ported from horstreporter.
//
// The compass overlay plots DX spots at their true bearing *and* true distance from
// the station QTH, matching horstreporter's azimuthal map. The math is ported verbatim
// from horstreporter's `static/azimuth-runtime.js` (`projectAeqdNormalized` +
// `projectToCanvas`) and `spot.go` (`locatorToLatLng`), so the console reproduces the
// exact same projection as the web tool — no server-side projection needed.
//
// Pure Dart (dart:math only) so it unit-tests without Flutter.

import 'dart:math' as math;

const double earthRadiusKm = 6371.0;
const double antipodeKm = math.pi * earthRadiusKm; // ≈ 20015 km
// Clipped just shy of π: at the antipode the AEQD scale factor k = c/sin(c) blows up
// (sin π = 0), so horstreporter clips the last 0.02 rad. We do the same.
const double maxVisibleC = math.pi - 0.02;

double _degToRad(double deg) => deg * math.pi / 180.0;

double _wrapPi(double rad) {
  while (rad > math.pi) {
    rad -= 2 * math.pi;
  }
  while (rad < -math.pi) {
    rad += 2 * math.pi;
  }
  return rad;
}

/// A geographic point. `lat`/`lng` in degrees (lng East-positive), matching
/// horstreporter's convention.
typedef LatLng = ({double lat, double lng});

/// Decodes a Maidenhead grid locator to lat/lng (degrees), ported from
/// `horstreporter/spot.go:locatorToLatLng`. Accepts 2 (field), 4 (square) or 6
/// (subsquare) characters; each precision adds a center offset so the result is the
/// center of the cell. Returns (0, 0) for < 2 chars.
LatLng locatorToLatLng(String locator) {
  final s = locator.toUpperCase();
  if (s.length < 2) return (lat: 0.0, lng: 0.0);

  // Field: A-R → 20° lng, 10° lat, anchored at (-180, -90).
  double lng = (s.codeUnitAt(0) - 65) * 20.0 - 180.0; // 'A' = 65
  double lat = (s.codeUnitAt(1) - 65) * 10.0 - 90.0;

  if (s.length >= 4) {
    // Square: 0-9 → 2° lng, 1° lat.
    lng += (s.codeUnitAt(2) - 48) * 2.0; // '0' = 48
    lat += (s.codeUnitAt(3) - 48) * 1.0;
    if (s.length >= 6) {
      // Subsquare: A-X → 5' lng, 2.5' lat, plus center offset.
      lng += (s.codeUnitAt(4) - 65) * (5.0 / 60.0) + (5.0 / 120.0);
      lat += (s.codeUnitAt(5) - 65) * (2.5 / 60.0) + (2.5 / 120.0);
    } else {
      // 4-char: center of the square.
      lng += 1.0;
      lat += 0.5;
    }
  } else {
    // 2-char: center of the field.
    lng += 10.0;
    lat += 5.0;
  }
  return (lat: lat, lng: lng);
}

/// A normalized AEQD projection result: `x`/`y` on the unit disc (where `|(x, y)| = c`,
/// the angular distance from the center in radians) and `c` itself. `null` if the
/// point is beyond the visible horizon (near-antipode).
typedef AeqdNormalized = ({double x, double y, double c});

/// Azimuthal equidistant projection centered on [lat0], [lon0] (degrees).
///
/// `project()` returns the canvas pixel for a point, given the canvas size, zoom and
/// great-circle horizon. `zoom = 1.0` maps the antipode to the rim; `horizonKm = 20015`
/// (the antipode distance) keeps the whole world. Ported from
/// `horstreporter/static/azimuth-runtime.js` (`projectAeqdNormalized` + `projectToCanvas`).
class Aeqd {
  final double lat0;
  final double lon0;

  const Aeqd(this.lat0, this.lon0);

  /// Forward AEQD to normalized (x, y) + angular distance c (radians). `null` if the
  /// point is beyond `maxVisibleC` (near-antipode, where the projection is singular).
  AeqdNormalized? normalized(double lat, double lon) {
    final lat0r = _degToRad(lat0);
    final lon0r = _degToRad(lon0);
    final lat1r = _degToRad(lat);
    final lon1r = _degToRad(lon);

    final dLon = _wrapPi(lon1r - lon0r);
    final sinLat0 = math.sin(lat0r);
    final cosLat0 = math.cos(lat0r);
    final sinLat = math.sin(lat1r);
    final cosLat = math.cos(lat1r);

    final cosC = (sinLat0 * sinLat + cosLat0 * cosLat * math.cos(dLon)).clamp(-1.0, 1.0);
    final c = math.acos(cosC);
    if (!c.isFinite || c > maxVisibleC) return null;

    final k = c == 0 ? 1.0 : c / math.sin(c);
    final x = k * cosLat * math.sin(dLon);
    final y = k * (cosLat0 * sinLat - sinLat0 * cosLat * math.cos(dLon));
    return (x: x, y: y, c: c);
  }

  /// Canvas pixel for a point, or `null` if beyond the horizon/antipode. Convention
  /// matches horstreporter: y flipped (north up), canvas center at (w/2, h/2),
  /// `scale = radius * zoom / π`.
  ({double x, double y})? project(
    double lat,
    double lon,
    double width,
    double height, {
    double zoom = 1.0,
    double horizonKm = antipodeKm,
  }) {
    final n = normalized(lat, lon);
    if (n == null) return null;
    final distKm = n.c * earthRadiusKm;
    if (distKm > horizonKm) return null;
    final radius = math.min(width, height) * 0.47;
    final scale = (radius * zoom) / math.pi;
    return (x: width / 2 + n.x * scale, y: height / 2 - n.y * scale);
  }
}