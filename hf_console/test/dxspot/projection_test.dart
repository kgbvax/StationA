import 'dart:math' as math;
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/projection.dart';

// Tolerances: locator decode involves /60 arithmetic, AEQD involves acos/sin.
const _locEps = 1e-6;
const _aeqdEps = 1e-9;

void main() {
  group('locatorToLatLng', () {
    test('2-char field returns field center', () {
      // Field JN: J=9 → lng 9*20-180 = 0; N=13 → lat 13*10-90 = 40; +center (10,5).
      final p = locatorToLatLng('JN');
      expect(p.lat, closeTo(45.0, _locEps));
      expect(p.lng, closeTo(10.0, _locEps));
    });

    test('4-char square returns square center', () {
      // JN58: lng = 0 + 5*2 + 1 (center) = 11; lat = 40 + 8*1 + 0.5 = 48.5.
      final p = locatorToLatLng('JN58');
      expect(p.lat, closeTo(48.5, _locEps));
      expect(p.lng, closeTo(11.0, _locEps));
    });

    test('6-char subsquare returns subsquare center', () {
      // JN58sd: lng = 0 + 10 + (s=18)*5/60 + 5/120; lat = 40 + 8 + (d=3)*2.5/60 + 2.5/120.
      final p = locatorToLatLng('JN58sd');
      // lat base = 40 (field) + 8 (square) = 48, plus subsquare offset.
      expect(p.lat, closeTo(48.0 + (3 * 2.5 / 60) + (2.5 / 120), _locEps));
      expect(p.lat, closeTo(48.1458333, 1e-5));
      expect(p.lng, closeTo(11.5416666, 1e-5));
    });

    test('lowercase input is uppercased', () {
      expect(locatorToLatLng('jn58'), locatorToLatLng('JN58'));
    });

    test('too-short locator returns (0, 0)', () {
      final p = locatorToLatLng('J');
      expect(p.lat, 0.0);
      expect(p.lng, 0.0);
    });
  });

  group('Aeqd.normalized — bearing radials', () {
    final aeqd = Aeqd(0.0, 0.0);

    test('due north → +y (up), x ≈ 0', () {
      final n = aeqd.normalized(1.0, 0.0)!;
      expect(n.x, closeTo(0.0, 1e-9));
      expect(n.y, greaterThan(0.0));
    });

    test('due east → +x (right), y ≈ 0', () {
      final n = aeqd.normalized(0.0, 1.0)!;
      expect(n.x, greaterThan(0.0));
      expect(n.y, closeTo(0.0, 1e-9));
    });

    test('due south → −y, due west → −x', () {
      expect(aeqd.normalized(-1.0, 0.0)!.y, lessThan(0.0));
      expect(aeqd.normalized(0.0, -1.0)!.x, lessThan(0.0));
    });

    test('longitude wraps across the ±180° seam', () {
      // Center at -179°, spot at +179°: the short way round is 2° *west*, so the
      // spot projects to the west (negative x), not 358° east.
      final a = Aeqd(0.0, -179.0);
      final n = a.normalized(0.0, 179.0)!;
      expect(n.x, lessThan(0.0));
      expect(n.c, closeTo(2.0 * math.pi / 180.0, 1e-6));
    });
  });

  group('Aeqd — equidistant property |(x,y)| = c', () {
    final aeqd = Aeqd(48.5, 11.0); // JN58
    test('holds for a near spot at a known distance', () {
      // A spot 10° away (great-circle) from the center.
      final n = aeqd.normalized(58.5, 11.0)!; // 10° north → c ≈ 10°
      expect(math.sqrt(n.x * n.x + n.y * n.y), closeTo(n.c, _aeqdEps));
      expect(n.c, closeTo(10.0 * math.pi / 180.0, 1e-6));
    });

    test('holds for an arbitrary off-axis spot', () {
      final n = aeqd.normalized(20.0, -30.0)!;
      expect(math.sqrt(n.x * n.x + n.y * n.y), closeTo(n.c, _aeqdEps));
    });
  });

  group('Aeqd.normalized — horizon clip', () {
    final aeqd = Aeqd(0.0, 0.0);
    test('near-antipode (> maxVisibleC) returns null', () {
      // ~179° away exceeds π-0.02 (≈178.85°), regardless of which side.
      expect(aeqd.normalized(0.0, 179.0), isNull);
      expect(aeqd.normalized(0.0, -179.0), isNull);
    });
    test('just-inside is still visible', () {
      expect(aeqd.normalized(0.0, 170.0), isNotNull);
    });
  });

  group('Aeqd.project — canvas mapping', () {
    final aeqd = Aeqd(0.0, 0.0);
    test('center maps to canvas center', () {
      final p = aeqd.project(0.0, 0.0, 200.0, 200.0)!;
      expect(p.x, closeTo(100.0, 1e-6));
      expect(p.y, closeTo(100.0, 1e-6));
    });

    test('near-antipode lands near the rim at zoom 1.0', () {
      // 178° away (≈3.106 rad, under maxVisibleC) → pixel distance ≈ c/π * radius.
      final p = aeqd.project(0.0, 178.0, 200.0, 200.0, zoom: 1.0)!;
      final dist = math.sqrt((p.x - 100) * (p.x - 100) + (p.y - 100) * (p.y - 100));
      // radius = 200*0.47 = 94; antipode would be at 94; 178°/180° → ~92.9.
      expect(dist, greaterThan(90.0));
      expect(dist, lessThan(94.0));
    });

    test('beyond horizonKm returns null', () {
      // Whole-world horizon is the antipode (~20015 km); a near-antipode spot is
      // within it, but a configured tighter horizon clips it.
      final p = aeqd.project(0.0, 178.0, 200.0, 200.0, horizonKm: 1000.0);
      expect(p, isNull);
    });
  });
}