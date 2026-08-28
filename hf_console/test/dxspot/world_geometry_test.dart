// world_geometry_test.dart — smoke tests for the GeoJSON parser (no Flutter).
//
// The parser is pure Dart (`WorldGeometry.parseRings`), so we can exercise it
// without `rootBundle`. The full integration (asset load + cache) is covered by
// the build/smoke path; the parser is where most of the bugs would be.

import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/world_geometry.dart';

void main() {
  test('parseRings returns empty for non-FeatureCollection', () {
    expect(WorldGeometry.parseRings('{"foo":1}'), isEmpty);
    expect(WorldGeometry.parseRings('not json'), isEmpty);
    expect(WorldGeometry.parseRings('[]'), isEmpty);
  });

  test('parseRings extracts Polygon rings', () {
    const g = '''
{
  "type": "FeatureCollection",
  "features": [
    {
      "type": "Feature",
      "properties": {"name": "Test"},
      "geometry": {
        "type": "Polygon",
        "coordinates": [
          [[0,0],[1,0],[1,1],[0,1],[0,0]]
        ]
      }
    }
  ]
}
''';
    final rings = WorldGeometry.parseRings(g);
    expect(rings, hasLength(1));
    expect(rings.first, hasLength(5));
    expect(rings.first.first.lat, 0.0);
    expect(rings.first.first.lng, 0.0);
  });

  test('parseRings extracts MultiPolygon rings', () {
    const g = '''
{
  "type": "FeatureCollection",
  "features": [
    {
      "type": "Feature",
      "properties": {},
      "geometry": {
        "type": "MultiPolygon",
        "coordinates": [
          [[[10,10],[11,10],[11,11],[10,11],[10,10]]],
          [[[20,20],[21,20],[21,21],[20,21],[20,20]]]
        ]
      }
    }
  ]
}
''';
    final rings = WorldGeometry.parseRings(g);
    expect(rings, hasLength(2));
  });

  test('parseRings ignores degenerate rings (< 3 vertices)', () {
    const g = '''
{
  "type": "FeatureCollection",
  "features": [
    {
      "type": "Feature",
      "geometry": {
        "type": "Polygon",
        "coordinates": [
          [[0,0],[1,1],[0,0]]
        ]
      }
    }
  ]
}
''';
    final rings = WorldGeometry.parseRings(g);
    expect(rings, hasLength(1)); // 3-vertex ring kept (closed triangle)
  });

  test('bundled world.geojson decodes to plausible coastline shape', () {
    // Path relative to the package root; flutter_test sets the CWD there.
    final file = File('assets/geo/world.geojson');
    if (!file.existsSync()) {
      markTestSkipped('assets/geo/world.geojson not present');
      return;
    }
    final raw = file.readAsStringSync();
    final rings = WorldGeometry.parseRings(raw);

    // Natural Earth ne_50m_admin_0_countries has roughly 2000-2500 outer rings +
    // a comparable count of inner-ring (holes) — total somewhere in the 3-8k range.
    expect(rings.length, greaterThan(1000));
    expect(rings.length, lessThan(50000));

    // All coords must be in the valid lat/lng range (no NaN, no garbage).
    for (final ring in rings) {
      for (final p in ring) {
        expect(p.lat, inInclusiveRange(-90.0, 90.0));
        expect(p.lng, inInclusiveRange(-180.0, 180.0));
      }
    }
  });
}