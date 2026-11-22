// thin_subpaths_test.dart — the world-layer vertex thinning keeps run ends,
// drops sub-pixel neighbours and sub-pixel islands, and cuts the real asset
// down enough to matter on the tablet.

import 'dart:io';
import 'dart:math' as math;

import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/projection.dart';
import 'package:hf_console/dxspot/ring_subpaths.dart';
import 'package:hf_console/dxspot/world_geometry.dart';

void main() {
  test('keeps first and last vertex, drops neighbours closer than minDist', () {
    final run = <ProjectedPoint>[
      (x: 0, y: 0),
      (x: 0.3, y: 0),
      (x: 0.6, y: 0),
      (x: 1.2, y: 0),
      (x: 5, y: 0),
      (x: 5.2, y: 0.1),
    ];
    final out = thinSubpaths([run], 1.0);
    expect(out, hasLength(1));
    expect(out.single, [
      (x: 0.0, y: 0.0),
      (x: 1.2, y: 0.0),
      (x: 5.0, y: 0.0),
      (x: 5.2, y: 0.1), // last vertex kept although within minDist
    ]);
  });

  test('drops runs whose extent is below minDist in both axes', () {
    final tiny = <ProjectedPoint>[(x: 10, y: 10), (x: 10.4, y: 10.5), (x: 10, y: 10)];
    final thin = <ProjectedPoint>[(x: 0, y: 0), (x: 0.2, y: 3), (x: 0, y: 0)];
    final out = thinSubpaths([tiny, thin], 1.0);
    expect(out, [thin]);
  });

  test('drops runs shorter than two points', () {
    expect(thinSubpaths([<ProjectedPoint>[(x: 1, y: 1)]], 1.0), isEmpty);
  });

  test('real asset: compass at tablet size keeps under a third of the vertices', () {
    final file = File('assets/geo/world.geojson');
    if (!file.existsSync()) {
      markTestSkipped('assets/geo/world.geojson not present');
      return;
    }
    final rings = WorldGeometry.parseRings(file.readAsStringSync());
    final aeqd = Aeqd(52.2, 7.8); // JO32WE
    const r = 250.0, zoom = 1.5; // shack tablet disc, logical px
    final scale = (r - 6) * zoom / math.pi;
    final sub = projectRingSubpaths(rings, (lat, lng) {
      final n = aeqd.normalized(lat, lng);
      if (n == null) return null;
      return (x: r + n.x * scale, y: r - n.y * scale);
    });
    final before = sub.fold<int>(0, (a, s) => a + s.length);
    final after = thinSubpaths(sub, 1.0).fold<int>(0, (a, s) => a + s.length);
    expect(after, lessThan(before / 3), reason: '$before -> $after');
  });
}
