// ring_subpaths_test.dart — unit tests for the shared ring→subpath splitter
// that both map painters fill their landmass paths from.
//
// The split rules exist because of a reproduced bug: the Mercator fill split
// on RAW Δlng > 180° (the dateline), but MercatorProjection wraps longitude
// relative to the pan center, so the real seam tears at lng = centerLng ± 180°
// and moves with every pan. On the default QTH-centered view that meridian
// crosses the Chukotka coast and the fill streaked a corrupted land-colored
// band across the whole map.
//
// Beyond "no segment spans half a world", the tests pin FILL PARITY against
// geographic point-in-polygon ground truth at panned centers — the class of
// defect a segment-length bound cannot see. A ring torn at the seam (instead
// of cut) has short in-run segments while its implicit close chords run from
// the seam across the ring interior, dropping whole straddling landmasses
// (all of visible India when the seam crosses Asia) from the fill.

import 'dart:io';
import 'dart:math' as math;

import 'package:flutter_test/flutter_test.dart';

import 'package:hf_console/dxspot/mercator_projection.dart';
import 'package:hf_console/dxspot/projection.dart';
import 'package:hf_console/dxspot/ring_subpaths.dart';
import 'package:hf_console/dxspot/world_geometry.dart';

List<List<LatLng>> _loadRings() {
  final file = File('assets/geo/world.geojson');
  if (!file.existsSync()) {
    // Integration-style tests below call this too; callers skip when null.
    return const [];
  }
  return WorldGeometry.parseRings(file.readAsStringSync());
}

List<List<ProjectedPoint>> _mercatorRuns(
  List<List<LatLng>> rings,
  MercatorProjection proj,
) =>
    projectRingSubpaths(
      rings,
      (lat, lng) {
        final m = proj.project(lat, lng);
        if (m == null) return null;
        return (x: m.x, y: m.y);
      },
      seamCrossing: proj.seamCrossingBetween,
    );

/// Even-odd parity of the painter's subpath construction at (px, py): one
/// horizontal ray cast against every run, implicit close chords included —
/// the exact geometry Canvas.drawPath(evenOdd) fills from the same runs.
bool _insideRuns(List<List<ProjectedPoint>> runs, double px, double py) {
  var inside = false;
  for (final run in runs) {
    for (int i = 0; i < run.length; i++) {
      final a = run[i];
      final b = run[(i + 1) % run.length]; // includes the implicit close
      if ((a.y > py) != (b.y > py)) {
        final x = a.x + (py - a.y) / (b.y - a.y) * (b.x - a.x);
        if (x > px) inside = !inside;
      }
    }
  }
  return inside;
}

/// Geographic ground truth: even-odd point-in-polygon over the RAW rings,
/// cast eastward in longitude. The asset is pre-cut at ±180° (no ring
/// wraps), so raw-space ray casting is exact. GeoJSON hole rings (Caspian)
/// subtract via the same even-odd the painter uses.
bool _insideRings(List<List<LatLng>> rings, double lat, double lng) {
  var inside = false;
  for (final ring in rings) {
    for (int i = 0; i < ring.length; i++) {
      final a = ring[i];
      final b = ring[(i + 1) % ring.length];
      if ((a.lat > lat) != (b.lat > lat)) {
        final x = a.lng + (lat - a.lat) / (b.lat - a.lat) * (b.lng - a.lng);
        if (x > lng) inside = !inside;
      }
    }
  }
  return inside;
}

/// Asserts that every probe's painted parity equals its geographic truth (and
/// that the truth itself matches the pinned expectation — so a broken ground
/// truth helper cannot silently pass a broken renderer, or vice versa).
void _expectFillParity(
  List<List<LatLng>> rings,
  MercatorProjection proj,
  List<(LatLng probe, bool truth)> probes,
) {
  final runs = _mercatorRuns(rings, proj);
  for (final (probe, expected) in probes) {
    final truth = _insideRings(rings, probe.lat, probe.lng);
    expect(truth, expected,
        reason: 'ground truth at (${probe.lat}, ${probe.lng}) changed?');
    final p = proj.project(probe.lat, probe.lng);
    expect(p, isNotNull,
        reason: 'probe (${probe.lat}, ${probe.lng}) must be on the canvas');
    final painted = _insideRuns(runs, p!.x, p.y);
    expect(painted, truth,
        reason:
            'fill parity at (${probe.lat}, ${probe.lng}) must match geographic truth');
  }
}

void main() {
  ProjectedPoint? identityProject(double lat, double lng) =>
      (x: lng.toDouble(), y: lat.toDouble());

  test('a plain ring projects to one run, vertices in order', () {
    final rings = [
      [
        (lat: 0.0, lng: 0.0),
        (lat: 0.0, lng: 1.0),
        (lat: 1.0, lng: 1.0),
        (lat: 1.0, lng: 0.0),
      ],
    ];
    final runs = projectRingSubpaths(rings, identityProject);
    expect(runs, hasLength(1));
    expect(runs.single, hasLength(4));
    expect(runs.single.first.x, 0.0);
    expect(runs.single.last.y, 1.0);
  });

  test('a null projection breaks the run at the undefined vertex', () {
    final rings = [
      [
        (lat: 0.0, lng: 0.0),
        (lat: 0.0, lng: 1.0),
        (lat: 0.0, lng: 2.0), // projects null
        (lat: 0.0, lng: 3.0),
        (lat: 0.0, lng: 4.0),
      ],
    ];
    ProjectedPoint? project(double lat, double lng) =>
        lng == 2.0 ? null : (x: lng, y: lat);
    final runs = projectRingSubpaths(rings, project);
    expect(runs, hasLength(2));
    expect(runs[0].map((p) => p.x), [0.0, 1.0]);
    expect(runs[1].map((p) => p.x), [3.0, 4.0]);
  });

  test('seam cut turns a straddling ring into two pieces closed along the seam', () {
    // A small closed rectangle straddling the seam at lng 8 ± 180 = -172.
    // Consecutive raw Δlng is tiny everywhere — the wrapped deltas flip
    // branch across the seam, the shape a raw-Δlng split can never catch.
    // Tearing this ring at the seam leaves each half closing from its last
    // vertex to the rectangle's start vertex — chords across the interior
    // that drop the straddled middle from the fill. The cut must instead
    // produce two pieces, each first/last vertex on the SAME canvas edge,
    // so each closes along the seam and together they enclose the whole
    // rectangle.
    final rings = [
      [
        (lat: 65.0, lng: -172.8), // wrapped delta +179.2 — right canvas edge
        (lat: 65.0, lng: -171.2), // −179.2 — left edge; seam crossing here
        (lat: 66.0, lng: -171.2),
        (lat: 66.0, lng: -172.8), // crossing back
        (lat: 65.0, lng: -172.8), // closed
      ],
    ];
    const proj = MercatorProjection(
      centerLat: 50.0,
      centerLng: 8.0, // seam at lng -172
      zoom: 2.5,
      width: 800,
      height: 450,
    );
    final runs = _mercatorRuns(rings, proj);
    expect(runs, hasLength(2),
        reason: 'the two seam crossings must cut the ring into two pieces');

    final worldPx = proj.scale * 2 * math.pi * 6378137.0;
    for (final run in runs) {
      for (int i = 1; i < run.length; i++) {
        expect((run[i].x - run[i - 1].x).abs(), lessThan(worldPx * 0.5),
            reason: 'segment crosses the wrap seam');
      }
      // The cut piece's implicit close chord must run ALONG the seam: first
      // and last vertex share one canvas edge.
      expect((run.first.x - run.last.x).abs(), lessThan(1e-6),
          reason: 'piece closes with a chord across the interior, not along the seam');
    }

    // Fill parity: the middle of the straddled rectangle must be enclosed.
    _expectFillParity(rings, proj, [
      ((lat: 65.5, lng: -172.5), true),
      ((lat: 64.5, lng: -171.0), false),
    ]);
  });

  test('real asset: default QTH center — no streaks, every piece closes along the seam', () {
    final rings = _loadRings();
    if (rings.isEmpty) {
      markTestSkipped('assets/geo/world.geojson not present');
      return;
    }

    // Default Mercator view: QTH-centered (the panel's initial state), which
    // puts the seam at lng -172 — on the Chukotka coast. Pre-fix, three real
    // rings straddled it and streaked the fill.
    const proj = MercatorProjection(
      centerLat: 50.0,
      centerLng: 8.0,
      zoom: 2.5,
      width: 800,
      height: 450,
    );
    final runs = _mercatorRuns(rings, proj);
    expect(runs, isNotEmpty);

    final worldPx = proj.scale * 2 * math.pi * 6378137.0;
    for (final run in runs) {
      for (int i = 1; i < run.length; i++) {
        expect((run[i].x - run[i - 1].x).abs(), lessThan(worldPx * 0.5),
            reason: 'segment crosses the wrap seam (streak)');
      }
      // Uncut rings close first→last on the same (duplicated) raw vertex; cut
      // pieces close on the same canvas edge. A plain tear instead leaves the
      // close chord running from the seam to the ring's stored start vertex —
      // across the interior, the wedge defect the review round reproduced.
      expect((run.first.x - run.last.x).abs(), lessThan(1.0),
          reason: 'implicit close chord spans the ring interior');
    }

    // The seam cut must actually have split something: without it the
    // straddlers cannot produce > rings.length runs. This pins the original
    // review finding — a raw-Δlng split yields exactly runs.length here.
    expect(runs.length, greaterThan(rings.length),
        reason: 'the seam cut must split the rings straddling lng -172');
  });

  test('real asset: panned center — straddling landmasses keep their fill (parity vs ground truth)', () {
    final rings = _loadRings();
    if (rings.isEmpty) {
      markTestSkipped('assets/geo/world.geojson not present');
      return;
    }

    // The review round's repro: pan to 39/-98 and zoom out to 1. The seam
    // moves to raw lng 82°E — straight across Russia/Mongolia/China/India.
    // A tear at the seam paints ALL of visible India plus the Altai/Siberia
    // interior as sea; the cut must render them as land.
    const pan = MercatorProjection(
      centerLat: 39.0,
      centerLng: -98.0,
      zoom: 1.0,
      width: 800,
      height: 450,
    );
    _expectFillParity(rings, pan, [
      // West of the seam (wrapped delta near +180, right canvas edge):
      ((lat: 23.77, lng: 78.13), true), // central India, deep inland
      ((lat: 39.74, lng: -104.99), true), // Colorado — same branch, far from the seam
      ((lat: -20.0, lng: 80.0), false), // mid Indian Ocean
      // East of the seam (wrapped delta near -180, left canvas edge):
      ((lat: 51.10, lng: 86.57), true), // Altai
      ((lat: 68.35, lng: 86.57), true), // Siberia
      ((lat: 55.0, lng: 100.0), true), // Mongolian border region
      ((lat: 0.0, lng: -170.0), false), // mid Pacific
      ((lat: 42.0, lng: 50.5), false), // Caspian Sea — hole ring stays a hole
    ]);

    // Seam over Europe/Africa (center -151 puts it at raw lng 29°E) — the
    // other panned view the review measured chord wedges on (65 rings cut).
    const euAfrica = MercatorProjection(
      centerLat: 50.0,
      centerLng: -151.0,
      zoom: 1.0,
      width: 800,
      height: 450,
    );
    _expectFillParity(rings, euAfrica, [
      ((lat: 55.75, lng: 37.62), true), // Moscow — east of the seam
      ((lat: 48.85, lng: 2.35), true), // Paris — west of the seam
      ((lat: 0.5, lng: 37.9), true), // Kenya, straddler-adjacent east side
      ((lat: -30.0, lng: 15.0), false), // South Atlantic
    ]);

    // Zoomed-out tall frame at the default center: Antarctica's ring enters
    // the canvas, and its stored start vertex sits mid-peninsula — a tear
    // there paints the Antarctic Peninsula as sea once it is in frame.
    const antarctica = MercatorProjection(
      centerLat: 50.0,
      centerLng: 8.0,
      zoom: 1.0,
      width: 800,
      height: 800,
    );
    _expectFillParity(rings, antarctica, [
      ((lat: -63.8, lng: -58.4), true), // Antarctic Peninsula
      ((lat: -72.0, lng: 20.0), true), // Queen Maud Land interior
      ((lat: -40.0, lng: 20.0), false), // open South Atlantic
      ((lat: 50.5, lng: 8.5), true), // Germany near the center
    ]);
  });

  test('real asset: accumulated pan centers (unwrapped centerLng) keep the seam cut', () {
    final rings = _loadRings();
    if (rings.isEmpty) {
      markTestSkipped('assets/geo/world.geojson not present');
      return;
    }

    // _panBy accumulates the center longitude unwrapped (unproject returns
    // centerLng + delta, nothing wraps or clamps it), so panning far in one
    // direction reaches centerLng values like 622 (= -98 + 720) or -818
    // (= -98 - 720). The seam-crossing resolver must still find the raw
    // crossing meridian inside the segment's span there — an earlier
    // normalization only tried centerLng ± 180 with a single ±360
    // adjustment, silently disabling the cut past ±2 turns and letting the
    // original full-width land streak return (measured 15,096 hard
    // mismatches at centerLng 406). These centers render the identical view
    // of the 39/-98 case above, so the same probes must hold.
    const east = MercatorProjection(
      centerLat: 39.0,
      centerLng: 622.0, // ≡ -98 after two full eastward turns
      zoom: 1.0,
      width: 800,
      height: 450,
    );
    _expectFillParity(rings, east, [
      ((lat: 23.77, lng: 78.13), true), // central India
      ((lat: 51.10, lng: 86.57), true), // Altai
      ((lat: 68.35, lng: 86.57), true), // Siberia
      ((lat: 42.0, lng: 50.5), false), // Caspian hole
      ((lat: -20.0, lng: 80.0), false), // mid Indian Ocean
    ]);

    const west = MercatorProjection(
      centerLat: 39.0,
      centerLng: -818.0, // ≡ -98 after two full westward turns
      zoom: 1.0,
      width: 800,
      height: 450,
    );
    _expectFillParity(rings, west, [
      ((lat: 23.77, lng: 78.13), true),
      ((lat: 51.10, lng: 86.57), true),
      ((lat: 0.0, lng: -170.0), false), // mid Pacific
    ]);
  });

  test('real asset: raw-Δlng dateline cuts do not exist in the dataset', () {
    // Pins the fact that made the original raw-Δlng split dead code: Natural
    // Earth admin_0 rings are pre-cut at ±180°, so consecutive raw Δlng never
    // approaches 180°.
    final rings = _loadRings();
    if (rings.isEmpty) {
      markTestSkipped('assets/geo/world.geojson not present');
      return;
    }
    var maxRawDelta = 0.0;
    for (final ring in rings) {
      for (int i = 1; i < ring.length; i++) {
        final d = (ring[i].lng - ring[i - 1].lng).abs();
        if (d > maxRawDelta) maxRawDelta = d;
      }
    }
    expect(maxRawDelta, lessThan(180.0),
        reason: 'if this ever exceeds 180° the dataset changed; revisit the split rules');
  });
}