import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/map_fit.dart';
import 'package:hf_console/dxspot/mercator_projection.dart';

void main() {
  const muenster = (lat: 51.96, lng: 7.63);
  const w = 800.0;
  const h = 600.0;

  // Both points must land inside the canvas, padding respected.
  void expectFramed(MapView v, dynamic a, dynamic b, {double padding = 64}) {
    final p = MercatorProjection(centerLat: v.lat, centerLng: v.lng, zoom: v.zoom, width: w, height: h);
    for (final pt in [a, b]) {
      final m = p.project(pt.lat, pt.lng)!;
      expect(m.x, inInclusiveRange(padding - 0.5, w - padding + 0.5), reason: '$pt x');
      expect(m.y, inInclusiveRange(padding - 0.5, h - padding + 0.5), reason: '$pt y');
    }
  }

  test('frames the station and a far satellite (~4000 km)', () {
    const sat = (lat: 30.0, lng: -30.0);
    final v = fitPoints(muenster, sat, width: w, height: h);
    expectFramed(v, muenster, sat);
    expect(v.zoom, lessThan(5), reason: 'a satellite thousands of km away needs a wide view');
    expect(v.zoom, greaterThan(1));
  });

  test('frames a near satellite tighter than a far one', () {
    const near = (lat: 52.9, lng: 9.0);
    const far = (lat: 30.0, lng: -30.0);
    final vn = fitPoints(muenster, near, width: w, height: h);
    expectFramed(vn, muenster, near);
    expect(vn.zoom, greaterThan(fitPoints(muenster, far, width: w, height: h).zoom));
  });

  test('a satellite overhead does not zoom past maxZoom', () {
    final v = fitPoints(muenster, muenster, width: w, height: h, maxZoom: 7);
    expect(v.zoom, 7);
    expect(v.lat, closeTo(51.96, 1e-6));
    expect(v.lng, closeTo(7.63, 1e-6));
  });

  test('zoom is quantized down, so small moves do not re-zoom', () {
    final a = fitPoints(muenster, (lat: 40.0, lng: -20.0), width: w, height: h);
    final b = fitPoints(muenster, (lat: 40.05, lng: -20.05), width: w, height: h);
    expect(a.zoom % 0.25, 0);
    expect(b.zoom, a.zoom);
  });

  test('antimeridian pair frames the short way round', () {
    const a = (lat: 10.0, lng: 175.0);
    const b = (lat: 12.0, lng: -175.0);
    final v = fitPoints(a, b, width: w, height: h);
    expect(v.lng.abs(), greaterThan(170), reason: 'centre sits near the date line, not at 0°');
    expect(v.zoom, greaterThan(3));
  });
}
