// mercator_projection.dart — Web-Mercator (EPSG:3857) math for the DX map panel.
//
// Mirrors the projection used by horstreporter's Leaflet map (`static/map.js`):
// Spherical Mercator with the standard 256-pixel tile size and ±85.05112878°
// latitude clamp. Pure Dart so it unit-tests without Flutter.

import 'dart:math' as math;

import 'projection.dart';

const double _mercatorRadius = 6378137.0;
const double _webMercatorMaxLat = 85.05112878;
const double _tileSize = 256.0;

/// A projected (x, y) point in pixels, or `null` if the input is outside the
/// Mercator domain. `x` increases east, `y` increases south (screen convention).
typedef MercatorPoint = ({double x, double y});

/// Web-Mercator projection anchored on a centre point, zoom and canvas size.
///
/// `zoom = 1` means the whole world fits in 512×256 pixels (tile size × 2^zoom);
/// larger zooms magnify. The centre is specified in degrees and is reprojected
/// on every call so panning can move continuously.
class MercatorProjection {
  final double centerLat;
  final double centerLng;
  final double zoom;
  final double width;
  final double height;

  const MercatorProjection({
    required this.centerLat,
    required this.centerLng,
    required this.zoom,
    required this.width,
    required this.height,
  });

  /// World width/height in metres at zoom 0.
  static double get worldSize => 2.0 * math.pi * _mercatorRadius;

  /// Pixels per metre for the current zoom.
  double get scale => (math.pow(2.0, zoom) * _tileSize) / worldSize;

  double get _centerWorldX => _mercatorRadius * _degToRad(centerLng);
  double get _centerWorldY => _latToY(centerLat);

  /// Project a geographic point to canvas pixels, or `null` if clamped to the
  /// pole edge. Longitude is wrapped relative to the centre so antimeridian
  /// crossings render as a short jump instead of wrapping the long way around.
  MercatorPoint? project(double lat, double lng) {
    final clampedLat = lat.clamp(-_webMercatorMaxLat, _webMercatorMaxLat);
    if (!clampedLat.isFinite) return null;
    final deltaLng = _wrapLng180(lng - centerLng);
    final worldX = _centerWorldX + _mercatorRadius * _degToRad(deltaLng);
    final worldY = _latToY(clampedLat);
    final x = width / 2.0 + (worldX - _centerWorldX) * scale;
    final y = height / 2.0 - (worldY - _centerWorldY) * scale;
    if (!x.isFinite || !y.isFinite) return null;
    return (x: x, y: y);
  }

  /// Convert a screen pixel back to lat/lng. Longitude is returned relative
  /// to the centre (can be outside [-180, 180]) so antimeridian round-trips
  /// stay continuous.
  LatLng? unproject(double x, double y) {
    if (!x.isFinite || !y.isFinite) return null;
    final deltaLngRad = (x - width / 2.0) / scale / _mercatorRadius;
    final worldY = _centerWorldY - (y - height / 2.0) / scale;
    final lat = _yToLat(worldY);
    final lng = centerLng + _radToDeg(deltaLngRad);
    if (!lat.isFinite || !lng.isFinite) return null;
    return (lat: lat, lng: lng);
  }

  /// True if [lat] is inside the Mercator latitude domain.
  static bool latInBounds(double lat) =>
      lat >= -_webMercatorMaxLat && lat <= _webMercatorMaxLat;

  /// Minimum zoom at which the whole world (256 × 2^zoom px tall) fills a
  /// viewport of the given height. Matches horstreporter's `applyMercatorViewConstraints`.
  static double minZoomForHeight(double height) {
    if (height <= 0) return 0.0;
    return math.max(0.0, math.log(height / _tileSize) / math.ln2);
  }

  static double _latToY(double lat) {
    final clamped = lat.clamp(-_webMercatorMaxLat, _webMercatorMaxLat);
    final sinLat = math.sin(_degToRad(clamped));
    return _mercatorRadius * math.log((1.0 + sinLat) / (1.0 - sinLat)) / 2.0;
  }

  static double _yToLat(double y) {
    final t = math.exp(-y / _mercatorRadius);
    final latRad = math.pi / 2.0 - 2.0 * math.atan(t);
    return _radToDeg(latRad).clamp(-_webMercatorMaxLat, _webMercatorMaxLat);
  }

  static double _wrapLng180(double lng) {
    // Wrap a longitude *delta* into (-180, 180] so the short branch is chosen.
    while (lng > 180.0) {
      lng -= 360.0;
    }
    while (lng <= -180.0) {
      lng += 360.0;
    }
    return lng;
  }

  static double _degToRad(double deg) => deg * math.pi / 180.0;
  static double _radToDeg(double rad) => rad * 180.0 / math.pi;
}
