// world_geometry.dart — loads Natural Earth 50m coastlines as a flat list of rings.
//
// The asset is copied verbatim from horstreporter's `static/vendor/world.geojson`
// (Natural Earth ne_50m_admin_0_countries, public domain). We only need the outlines
// for AEQD re-projection in the compass painter — the per-feature properties
// (country name, ISO codes, etc.) are discarded.
//
// Loading is one-shot: the first call decodes + flattens all Polygon / MultiPolygon
// shapes into a flat `List<List<LatLng>>`; subsequent reads return the cached list.
// The cache lives for the lifetime of the process; the file is ~3 MB raw and the
// decoded list is ~30k rings.
//
// Pure parser (no Flutter dependency on the load path) lives in [_parse] so the
// unit test can exercise it without `rootBundle`.

import 'dart:convert';

import 'package:flutter/services.dart' show rootBundle;

import 'projection.dart';

/// Singleton lazy-loader for the bundled world coastline outlines. See file-level
/// comment. `load()` is idempotent and reentrant; multiple callers in the same frame
/// share the same future.
class WorldGeometry {
  WorldGeometry._();
  static final WorldGeometry instance = WorldGeometry._();

  static const String _assetPath = 'assets/geo/world.geojson';

  List<List<LatLng>>? _rings;
  Future<List<List<LatLng>>>? _pending;

  /// Decoded coastline rings. First call kicks off the load; subsequent calls return
  /// the cached list.
  Future<List<List<LatLng>>> load() {
    if (_rings != null) return Future.value(_rings);
    return _pending ??= _loadAndParse();
  }

  Future<List<List<LatLng>>> _loadAndParse() async {
    final raw = await rootBundle.loadString(_assetPath);
    final list = parseRings(raw);
    _rings = list;
    _pending = null;
    return list;
  }

  /// Pure-Dart parser — exposed for unit tests. Walks the GeoJSON FeatureCollection
  /// and flattens Polygon / MultiPolygon coordinate arrays into a list of rings.
  /// Each ring is a closed polygon (last vertex == first vertex), as GeoJSON
  /// requires. Per-feature `properties` are ignored. Malformed input returns an
  /// empty list (never throws) so a corrupted asset doesn't take down the
  /// compass.
  static List<List<LatLng>> parseRings(String geojson) {
    dynamic j;
    try {
      j = jsonDecode(geojson);
    } catch (_) {
      return const [];
    }
    if (j is! Map) return const [];
    final features = j['features'];
    if (features is! List) return const [];

    final out = <List<LatLng>>[];
    for (final f in features) {
      if (f is! Map) continue;
      final geom = f['geometry'];
      if (geom is! Map) continue;
      final type = geom['type'];
      final coords = geom['coordinates'];
      if (coords is! List) continue;
      if (type == 'Polygon') {
        _flattenPolygon(coords, out);
      } else if (type == 'MultiPolygon') {
        for (final poly in coords) {
          if (poly is List) _flattenPolygon(poly, out);
        }
      }
    }
    return out;
  }

  /// A Polygon is `List<List<List<num>>>` — outer = rings (first is outer, rest are
  /// holes), middle = vertices, inner = [lng, lat]. We keep all rings; the painter
  /// treats holes as just more outlines to draw (we don't fill, only stroke).
  static void _flattenPolygon(dynamic polygon, List<List<LatLng>> out) {
    if (polygon is! List) return;
    for (final ring in polygon) {
      if (ring is! List) continue;
      final pts = <LatLng>[];
      for (final v in ring) {
        if (v is! List || v.length < 2) continue;
        final lng = (v[0] as num).toDouble();
        final lat = (v[1] as num).toDouble();
        pts.add((lat: lat, lng: lng));
      }
      if (pts.length >= 3) out.add(pts);
    }
  }
}