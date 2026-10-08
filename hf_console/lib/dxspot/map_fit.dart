// map_fit.dart — the Mercator view (centre + zoom) that frames two points.
//
// Used by the UHF/CAM map to keep the station and the tracked satellite on
// screen together. Pure Dart so it unit-tests without Flutter.

import 'dart:math' as math;

import 'mercator_projection.dart';
import 'projection.dart';

typedef MapView = ({double lat, double lng, double zoom});

const double _r = 6378137.0;
const double _maxLat = 85.05112878;

double _y(double lat) {
  final l = lat.clamp(-_maxLat, _maxLat) * math.pi / 180.0;
  return _r * math.log(math.tan(math.pi / 4 + l / 2));
}

double _lat(double y) =>
    (2 * math.atan(math.exp(y / _r)) - math.pi / 2) * 180.0 / math.pi;

/// The view framing [a] and [b] inside a [width]×[height] canvas with
/// [padding] px of clear margin on every side. The zoom is clamped to
/// [minZoom]..[maxZoom] (a satellite overhead must not zoom to street level)
/// and rounded DOWN to [step] so the view only changes when the framing
/// changes materially — never a re-zoom on every position update.
/// Longitude is wrapped around [a] so an antimeridian pair frames the short way.
MapView fitPoints(
  LatLng a,
  LatLng b, {
  required double width,
  required double height,
  double padding = 64,
  double minZoom = 1,
  double maxZoom = 12,
  double step = 0.25,
}) {
  var dLng = b.lng - a.lng;
  dLng = ((dLng + 180) % 360 + 360) % 360 - 180;
  final bLng = a.lng + dLng;

  final xA = _r * a.lng * math.pi / 180.0;
  final xB = _r * bLng * math.pi / 180.0;
  final yA = _y(a.lat);
  final yB = _y(b.lat);

  final spanX = (xA - xB).abs();
  final spanY = (yA - yB).abs();
  final availW = math.max(1.0, width - 2 * padding);
  final availH = math.max(1.0, height - 2 * padding);

  var zoom = maxZoom;
  if (spanX > 0 || spanY > 0) {
    final scale = math.min(
      spanX > 0 ? availW / spanX : double.infinity,
      spanY > 0 ? availH / spanY : double.infinity,
    );
    // scale = 2^zoom · 256 / worldSize  (see MercatorProjection.scale)
    zoom = math.log(scale * MercatorProjection.worldSize / 256.0) / math.ln2;
  }
  zoom = (zoom / step).floorToDouble() * step;
  zoom = zoom.clamp(minZoom, maxZoom).toDouble();

  var lng = (xA + xB) / 2 / _r * 180.0 / math.pi;
  lng = ((lng + 180) % 360 + 360) % 360 - 180;
  return (lat: _lat((yA + yB) / 2), lng: lng, zoom: zoom);
}
