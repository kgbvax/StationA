// ring_subpaths.dart — shared ring→subpath projection for the map painters.
//
// Both the AEQD compass world layer and the Mercator panel fill the Natural
// Earth landmasses as one even-odd Path. That only renders correctly if each
// ring's projected polyline is split wherever the projection is discontinuous:
// a vertex that fails to project (AEQD near-antipode cap, non-finite Mercator
// results) or a wrap seam. The Mercator seam is *not* the raw ±180° dateline —
// Natural Earth rings are pre-cut there with no raw Δlng jump at all — but the
// center-relative longitude wrap inside MercatorProjection.project, which tears
// at lng = centerLng ± 180° and moves with every pan. An unsplit segment across
// the seam is used both as a coastline stroke and as a fill boundary, streaking
// a land-colored band across the whole map; on the default QTH-centered view
// the tear lands on the Chukotka coast (2026-08 review round reproduced a
// full-width corrupted band without any panning).
//
// A seam split must CUT, not tear: the caller interpolates the exact crossing
// vertex and provides it projected onto both sides of the seam, the splitter
// appends it to the ending piece AND starts the next piece from it, and the
// piece a ring's walk starts in gets its two halves (head and tail of the
// vertex sequence) merged back together. Every piece then implicitly closes
// along a straight chord at its canvas edge, enclosing exactly the ring's part
// on that side of the seam. A plain tear instead leaves each half closing from
// its last vertex back to the ring's stored start vertex — a chord across the
// ring interior that drops whole straddling landmasses (all of visible India
// when the seam crosses Asia) from the fill.
//
// Pure Dart (no Canvas) so the split rules unit-test against synthetic rings
// and the bundled asset without a widget harness.

import 'projection.dart';

/// A projected vertex, or `null` where the projection is undefined for that
/// point (AEQD near-antipode cap, non-finite Mercator result).
typedef ProjectedPoint = ({double x, double y});

/// Projects a raw vertex; see [ProjectedPoint].
typedef VertexProjection = ProjectedPoint? Function(double lat, double lng);

/// For a raw segment prev→next that crosses the projection's wrap seam, the
/// interpolated crossing vertex projected onto BOTH sides of the seam: a seam
/// meridian lies on both canvas edges, so the crossing point exists once per
/// side. `prevSide` closes the piece ending at prev, `nextSide` opens the
/// piece starting at next. Returns null when the segment doesn't cross.
typedef SeamCrossing
    = ({ProjectedPoint prevSide, ProjectedPoint nextSide})? Function(
        LatLng prev, LatLng next);

/// Project every ring into contiguous vertex runs (each becomes one subpath
/// of the caller's even-odd fill path).
///
/// Without [seamCrossing] (AEQD has no wrap seam), a run breaks wherever
/// [project] returns null. With it, a segment reported as crossing the seam
/// cuts the ring: the ending piece gains the crossing vertex on prev's side,
/// the next piece starts from it on next's side, and the two halves of the
/// piece the ring's vertex walk starts in are merged (a GeoJSON ring is a
/// cycle, so with a crossing the walk starts mid-piece). Every piece then
/// closes along the seam chord at its canvas edge. Runs shorter than two
/// points are dropped — a one-point "run" draws nothing and cannot affect
/// fill parity.
List<List<ProjectedPoint>> projectRingSubpaths(
  List<List<LatLng>> rings,
  VertexProjection project, {
  SeamCrossing? seamCrossing,
}) {
  final out = <List<ProjectedPoint>>[];
  for (final ring in rings) {
    if (seamCrossing == null) {
      _projectPlainRing(ring, project, out);
    } else {
      _projectCutRing(ring, project, seamCrossing, out);
    }
  }
  return out.where((r) => r.length >= 2).toList(growable: false);
}

/// No-seam projection: runs break only where a vertex fails to project.
void _projectPlainRing(
  List<LatLng> ring,
  VertexProjection project,
  List<List<ProjectedPoint>> out,
) {
  List<ProjectedPoint>? run;
  for (final p in ring) {
    final v = project(p.lat, p.lng);
    if (v == null) {
      run = null;
    } else if (run == null) {
      run = [v];
      out.add(run);
    } else {
      run.add(v);
    }
  }
}

/// Seam-cutting projection: same null breaks, plus the cut described on
/// [projectRingSubpaths].
void _projectCutRing(
  List<LatLng> ring,
  VertexProjection project,
  SeamCrossing seamCrossing,
  List<List<ProjectedPoint>> out,
) {
  List<ProjectedPoint>? run;
  List<ProjectedPoint>? head; // the run that began at the ring's first vertex
  LatLng? prevRaw;
  var crossings = 0;
  for (final p in ring) {
    final v = project(p.lat, p.lng);
    final cross = prevRaw == null ? null : seamCrossing(prevRaw, p);
    if (cross != null) {
      crossings++;
      // Close the piece ending here at the seam vertex on prev's side, then
      // open the next piece at the same vertex on p's side.
      run?.add(cross.prevSide);
      run = [cross.nextSide];
      if (v != null) run.add(v);
      out.add(run);
    } else if (v == null) {
      run = null;
    } else if (run == null) {
      run = [v];
      out.add(run);
      if (crossings == 0) head = run;
    } else {
      run.add(v);
    }
    prevRaw = p;
  }
  // A ring is a cycle, so with crossings the run the walk STARTED in (head)
  // and the run it ENDED in (tail, ending at the duplicated closing vertex)
  // are the two halves of the same seam-side piece. Merge them so that piece
  // also closes along the seam instead of with two chords from the stored
  // start vertex across the ring interior. A closed ring crosses a meridian
  // an even number of times; the parity guard just stays no worse than a
  // plain tear on pathological data.
  if (crossings > 0 &&
      crossings.isEven &&
      head != null &&
      out.isNotEmpty &&
      !identical(out.last, head)) {
    out.last.addAll(head);
    out.remove(head);
  }
}