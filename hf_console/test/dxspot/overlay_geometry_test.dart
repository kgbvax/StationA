// overlay_geometry_test.dart — the rotator beam / satellite great circles on
// the Mercator map: zoomed out they run to the far side of the earth, over the
// poles and across the antimeridian seam.

import 'dart:math' as math;

import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/mercator_projection.dart';
import 'package:hf_console/dxspot/overlay_geometry.dart';
import 'package:hf_console/dxspot/projection.dart';

const _muenster = (lat: 51.962, lng: 7.626);

MercatorProjection _proj({
  double lat = 51.962,
  double lng = 7.626,
  double zoom = 1.0,
  double width = 900,
  double height = 600,
}) =>
    MercatorProjection(centerLat: lat, centerLng: lng, zoom: zoom, width: width, height: height);

double _maxStepDeg(List<LatLng> pts) {
  var m = 0.0;
  for (var i = 1; i < pts.length; i++) {
    m = math.max(m, distanceKm(pts[i - 1], pts[i]) / (earthRadiusKm * math.pi / 180));
  }
  return m;
}

void main() {
  group('projectNear', () {
    test('keeps a path continuous across the antimeridian seam', () {
      final p = _proj(); // seam at lng 187.626 = -172.374
      final a = p.projectNear(10, -170)!; // just west of the seam, in raw lng
      final b = p.projectNear(10, 175, a.x)!; // 175° = -185° → across the seam
      expect((b.x - a.x).abs(), lessThan(p.worldPixels / 10));
      // Without a reference the point lands inside the world rectangle.
      final w = p.worldBounds;
      final c = p.projectNear(10, 175)!;
      expect(c.x, inInclusiveRange(w.left, w.right));
    });

    test('projects latitudes beyond the Mercator limit instead of clamping them', () {
      final p = _proj();
      final w = p.worldBounds;
      final edge = p.projectNear(85.05, 0)!;
      final pole = p.projectNear(89.0, 0)!;
      expect(edge.y, closeTo(w.top, 0.5));
      expect(pole.y, lessThan(w.top - 50));
      expect(p.projectNear(-89.0, 0)!.y, greaterThan(w.bottom + 50));
    });

    test('agrees with project() inside the Mercator domain', () {
      final p = _proj(zoom: 3);
      for (final ll in [(lat: 40.0, lng: -100.0), (lat: -33.0, lng: 151.0), (lat: 60.0, lng: 8.0)]) {
        final a = p.project(ll.lat, ll.lng)!;
        final b = p.projectNear(ll.lat, ll.lng)!;
        expect(b.x, closeTo(a.x, 1e-6));
        expect(b.y, closeTo(a.y, 1e-6));
      }
    });
  });

  group('sampling', () {
    test('great circle samples are at most 2° of arc apart, however long', () {
      for (final km in [20.0, 3000.0, kMaxReachKm]) {
        for (final brg in [0.0, 45.0, 180.0, 290.0]) {
          final pts = greatCircleSamples(_muenster, brg, km);
          expect(_maxStepDeg(pts), lessThanOrEqualTo(kOverlayStepDeg + 1e-6), reason: '$km km @ $brg°');
          expect(distanceKm(pts.first, _muenster), lessThan(0.01));
        }
      }
    });

    test('circle samples close the ring within 2° steps', () {
      final pts = circleSamples((lat: 87.0, lng: 120.0), 3200);
      expect(_maxStepDeg(pts), lessThanOrEqualTo(kOverlayStepDeg + 1e-6));
      expect(distanceKm(pts.first, pts.last), lessThan(1.0));
    });
  });

  group('beamReachKm', () {
    test('zoomed out the beam reaches across the earth, not 5000 km', () {
      expect(beamReachKm(_proj(zoom: 1), _muenster), greaterThan(15000));
    });

    test('never past the antipode', () {
      expect(beamReachKm(_proj(zoom: 1), _muenster), lessThanOrEqualTo(kMaxReachKm));
    });

    test('a caller can cap it', () {
      expect(beamReachKm(_proj(zoom: 1), _muenster, maxKm: 4000), 4000);
      // A cap beyond what the viewport needs changes nothing.
      expect(beamReachKm(_proj(zoom: 10), _muenster, maxKm: 4000), lessThan(300));
    });

    test('zoomed in it stays local', () {
      expect(beamReachKm(_proj(zoom: 10), _muenster), lessThan(300));
    });
  });

  group('lines', () {
    test('a ray over the pole has no horizontal streak inside the map', () {
      final p = _proj();
      final w = p.worldBounds;
      final line = beamRay(p, _muenster, 0, kMaxReachKm);
      var inside = 0;
      for (var i = 1; i < line.points.length; i++) {
        final a = line.points[i - 1], b = line.points[i];
        final aIn = a.y >= w.top && a.y <= w.bottom, bIn = b.y >= w.top && b.y <= w.bottom;
        if (aIn && bIn) {
          inside++;
          expect((b.x - a.x).abs(), lessThan(p.worldPixels / 8), reason: 'segment $i');
        }
      }
      expect(inside, greaterThan(10));
    });

    test('a ray across the seam is one continuous line drawn in two world copies', () {
      final p = _proj(lng: 100); // seam at lng -80
      final line = beamRay(p, _muenster, 290, 9000); // north-west across the Atlantic into America
      for (var i = 1; i < line.points.length; i++) {
        expect((line.points[i].x - line.points[i - 1].x).abs(), lessThan(p.worldPixels / 4));
      }
      final w = p.worldBounds;
      expect(line.points.map((q) => q.x).reduce(math.min), lessThan(w.left), reason: 'runs past the seam');
      expect(line.shifts.length, 2);
    });

    test('a line wholly inside the map has the one shift', () {
      final line = beamRay(_proj(zoom: 6), _muenster, 90, 200);
      expect(line.shifts, [0.0]);
    });
  });

  group('beamWedge', () {
    test('zoomed out the wedge covers the far side and every cell is small', () {
      final p = _proj();
      final cells = beamWedge(p, _muenster, 0, 20, beamReachKm(p, _muenster));
      expect(cells, isNotEmpty);
      final w = p.worldPixels;
      var lowest = double.negativeInfinity;
      for (final c in cells) {
        expect(c.points, hasLength(4));
        expect(c.shifts, isNotEmpty);
        for (final q in c.points) {
          expect((q.x - c.points.first.x).abs(), lessThan(w / 4));
          lowest = math.max(lowest, q.y);
        }
      }
      // A beam on az 0 comes down the far side of the earth: well south of Münster.
      expect(lowest, greaterThan(p.project(0, 0)!.y));
    });

    test('zoomed in only the cells on the canvas are kept', () {
      final p = _proj(zoom: 9);
      final all = beamWedge(p, _muenster, 90, 30, beamReachKm(p, _muenster));
      for (final c in all) {
        final xs = [for (final q in c.points) q.x];
        expect(c.shifts.any((s) => xs.reduce(math.max) + s >= 0 && xs.reduce(math.min) + s <= p.width), isTrue);
      }
    });
  });
}
