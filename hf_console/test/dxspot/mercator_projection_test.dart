import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/mercator_projection.dart';

void main() {
  group('MercatorProjection', () {
    test('forward and inverse round-trip near the QTH', () {
      const proj = MercatorProjection(
        centerLat: 51.0,
        centerLng: 7.0,
        zoom: 4.0,
        width: 800,
        height: 600,
      );
      const lat = 52.5;
      const lng = 13.5;
      final p = proj.project(lat, lng);
      expect(p, isNotNull);
      final back = proj.unproject(p!.x, p.y);
      expect(back, isNotNull);
      expect(back!.lat, closeTo(lat, 0.0001));
      expect(back.lng, closeTo(lng, 0.0001));
    });

    test('round-trip across the antimeridian', () {
      const proj = MercatorProjection(
        centerLat: 0.0,
        centerLng: 179.0,
        zoom: 3.0,
        width: 800,
        height: 600,
      );
      const lat = 10.0;
      const lng = -179.0; // only 2° away in world space
      final p = proj.project(lat, lng);
      expect(p, isNotNull);
      final back = proj.unproject(p!.x, p.y);
      expect(back, isNotNull);
      // Wrapped longitude comes back near the centre's side.
      expect(back!.lat, closeTo(lat, 0.0001));
      expect(back.lng, closeTo(181.0, 0.0001));
    });

    test('clamps latitudes beyond Web-Mercator poles', () {
      const proj = MercatorProjection(
        centerLat: 0.0,
        centerLng: 0.0,
        zoom: 2.0,
        width: 800,
        height: 600,
      );
      final p = proj.project(90.0, 0.0);
      expect(p, isNotNull);
      final back = proj.unproject(p!.x, p.y);
      expect(back!.lat, lessThanOrEqualTo(85.05112878));
    });

    test('scale grows exponentially with zoom', () {
      const proj1 = MercatorProjection(
        centerLat: 0.0,
        centerLng: 0.0,
        zoom: 1.0,
        width: 512,
        height: 256,
      );
      const proj2 = MercatorProjection(
        centerLat: 0.0,
        centerLng: 0.0,
        zoom: 2.0,
        width: 512,
        height: 256,
      );
      expect(proj2.scale, closeTo(proj1.scale * 2.0, 0.0001));
    });

    test('minZoomForHeight matches horstreporter constraint', () {
      // A 256 px tall viewport just fits the 256 px world at zoom 0.
      expect(MercatorProjection.minZoomForHeight(256), closeTo(0.0, 0.001));
      // 512 px -> zoom 1.
      expect(MercatorProjection.minZoomForHeight(512), closeTo(1.0, 0.001));
    });
  });
}
