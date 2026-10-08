// overlay_geometry.dart — great-circle overlays (rotator beam, satellite
// footprint and station line) projected onto the Web-Mercator map.
//
// Pure Dart, like mercator_projection.dart. The painter strokes/fills what
// this returns; it only has to clip to `projection.worldBounds` and repeat
// each shape at the world shifts returned alongside it.
//
// What a great circle does on a Mercator map, and how it is handled here:
//  * it crosses the antimeridian seam (centerLng ± 180°, which moves with
//    every pan) → points are projected continuously (nearest world copy to the
//    previous point) and the shape is repeated at ±n world widths, so the part
//    past the seam re-enters on the other side with no jump;
//  * it runs over a pole → latitudes up to ±89.5° are projected for real
//    (not clamped to ±85.05°, which would squash the path onto the map edge);
//    the clip to the world rectangle cuts it where it leaves the map;
//  * it is long (a zoomed-out view shows the whole earth) → the sampling is
//    by arc length, never by a fixed count: at most [kOverlayStepDeg] between
//    samples. Two points inside the Mercator domain (colatitude ≥ 4.95°) are
//    then too close to be a chord across the pole, which is what would draw a
//    streak across the map.

import 'dart:math' as math;

import 'mercator_projection.dart';
import 'projection.dart';

/// Largest arc between neighbouring samples, degrees.
const double kOverlayStepDeg = 2.0;

/// Great circles meet again at the antipode; a ray stops just short of it.
const double kMaxReachKm = 19990.0;

const double _kmPerDeg = earthRadiusKm * math.pi / 180.0;

/// Points of the great circle that leaves [from] on [bearingDeg], 0…[km] km
/// out (the first point is [from]), at most [kOverlayStepDeg] apart.
List<LatLng> greatCircleSamples(LatLng from, double bearingDeg, double km, {int minSteps = 8}) {
  final steps = math.max(minSteps, (km / (kOverlayStepDeg * _kmPerDeg)).ceil());
  return [for (var i = 0; i <= steps; i++) destinationPoint(from, bearingDeg, km * i / steps)];
}

/// Points of the circle of [km] radius around [center]; the last equals the
/// first. Stepped by bearing, so the arc between neighbours is at most
/// [kOverlayStepDeg].
List<LatLng> circleSamples(LatLng center, double km) {
  final steps = math.max(36, (360.0 / kOverlayStepDeg).ceil());
  return [for (var i = 0; i <= steps; i++) destinationPoint(center, 360.0 * i / steps, km)];
}

/// A projected polyline, continuous across the wrap seam, plus the world
/// shifts (canvas px, multiples of the world width) at which it shows up
/// inside the world rectangle. Draw it once per shift, clipped to
/// `projection.worldBounds`.
class ProjectedLine {
  final List<MercatorPoint> points;
  final List<double> shifts;
  const ProjectedLine(this.points, this.shifts);

  bool get isEmpty => points.length < 2 || shifts.isEmpty;
}

/// Projects [samples] as one continuous line.
ProjectedLine projectLine(MercatorProjection proj, List<LatLng> samples) {
  final pts = <MercatorPoint>[];
  for (final s in samples) {
    final p = proj.projectNear(s.lat, s.lng, pts.isEmpty ? null : pts.last.x);
    if (p != null) pts.add(p);
  }
  if (pts.length < 2) return const ProjectedLine([], []);
  var lo = pts.first.x, hi = pts.first.x;
  for (final p in pts) {
    lo = math.min(lo, p.x);
    hi = math.max(hi, p.x);
  }
  return ProjectedLine(pts, worldShifts(proj, lo, hi));
}

/// Canvas x offsets (multiples of the world width) at which something that
/// spans [minX]…[maxX] on the nearest world copy overlaps the world rectangle.
List<double> worldShifts(MercatorProjection proj, double minX, double maxX) {
  final w = proj.worldPixels;
  final b = proj.worldBounds;
  final first = ((b.left - maxX) / w).ceil();
  final last = ((b.right - minX) / w).floor();
  return [for (var k = first; k <= last; k++) k * w];
}

/// How far from [qth] a beam has to reach to cover the viewport: the
/// farthest of a coarse grid of viewport points (a zoomed-out view has its
/// farthest point on an edge or in the middle, not at a corner — the
/// antipode), with a margin, never beyond [kMaxReachKm].
double beamReachKm(MercatorProjection proj, LatLng qth) {
  var reach = 50.0;
  const nx = 8, ny = 6;
  for (var i = 0; i <= nx; i++) {
    for (var j = 0; j <= ny; j++) {
      final ll = proj.unproject(proj.width * i / nx, proj.height * j / ny);
      if (ll != null) reach = math.max(reach, distanceKm(qth, ll));
    }
  }
  return math.min(reach * 1.1, kMaxReachKm);
}

/// A great-circle ray of [km] from [qth] on [bearingDeg].
ProjectedLine beamRay(MercatorProjection proj, LatLng qth, double bearingDeg, double km) =>
    projectLine(proj, greatCircleSamples(qth, bearingDeg, km));

/// The beam wedge (az ± [half], out to [reachKm]) as a mesh of quads, each
/// four corners on one world copy plus its shifts. A mesh rather than an
/// outline polygon: over a pole or across the seam the outline is not a
/// simple polygon in Mercator, but every small cell of the mesh is.
///
/// Fill them into one path with the non-zero rule so shared edges leave no
/// hairline. Cells off the canvas are dropped; so are cells that straddle a
/// pole (a hole too small to see: the Mercator domain ends 4.95° short of it).
List<ProjectedLine> beamWedge(MercatorProjection proj, LatLng qth, double az, double half, double reachKm) {
  final cols = math.max(4, (2 * half / kOverlayStepDeg).ceil());
  final rows = math.max(8, (reachKm / (kOverlayStepDeg * _kmPerDeg * 1.0)).ceil());
  final w = proj.worldPixels;

  // Projected lattice, wrapped into the world rectangle.
  final grid = List.generate(rows + 1, (_) => List<MercatorPoint?>.filled(cols + 1, null));
  for (var i = 0; i <= rows; i++) {
    for (var j = 0; j <= cols; j++) {
      final ll = destinationPoint(qth, az - half + 2 * half * j / cols, reachKm * i / rows);
      grid[i][j] = proj.projectNear(ll.lat, ll.lng);
    }
  }

  final quads = <ProjectedLine>[];
  for (var i = 0; i < rows; i++) {
    for (var j = 0; j < cols; j++) {
      final corners = [grid[i][j], grid[i + 1][j], grid[i + 1][j + 1], grid[i][j + 1]];
      if (corners.any((c) => c == null)) continue;
      final x0 = corners[0]!.x;
      var spansPole = false;
      final q = <MercatorPoint>[];
      for (final c in corners) {
        // Same world copy as the first corner.
        final x = c!.x + w * ((x0 - c.x) / w).roundToDouble();
        if ((x - x0).abs() > w / 4) spansPole = true;
        q.add((x: x, y: c.y));
      }
      if (spansPole) continue;
      var lo = q.first.x, hi = q.first.x, top = q.first.y, bottom = q.first.y;
      for (final p in q) {
        lo = math.min(lo, p.x);
        hi = math.max(hi, p.x);
        top = math.min(top, p.y);
        bottom = math.max(bottom, p.y);
      }
      if (bottom < 0 || top > proj.height) continue;
      final shifts = [
        for (final s in worldShifts(proj, lo, hi))
          if (hi + s >= 0 && lo + s <= proj.width) s
      ];
      if (shifts.isEmpty) continue;
      quads.add(ProjectedLine(q, shifts));
    }
  }
  return quads;
}
