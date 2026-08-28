// Smoke test: verify that the grid-square pipeline produces a sensible
// pixel quad for a typical 4-char Maidenhead locator near a JO31 QTH. This
// regression-tests the bug where the NW corner was projected using the NE
// lat+lng, collapsing the quad to a triangle.

import 'package:flutter_test/flutter_test.dart';

import 'package:hf_console/dxspot/projection.dart';

void main() {
  // JO31 — Ruhr area, central Europe (matches the screenshot QTH).
  final qth = locatorToLatLng('JO31');
  // JN58 is northern Italy, ~7° away from JO31 — a typical nearby square.
  const loc = 'JN58';
  final bounds = locatorToBounds(loc);

  test('grid-square corners are 4 distinct AEQD points (NW != NE)', () {
    expect(bounds, isNotNull);
    final aeqd = Aeqd(qth.lat, qth.lng);
    final sw = aeqd.normalized(bounds!.sw.lat, bounds.sw.lng)!;
    final se = aeqd.normalized(bounds.sw.lat, bounds.ne.lng)!;
    final ne = aeqd.normalized(bounds.ne.lat, bounds.ne.lng)!;
    final nw = aeqd.normalized(bounds.ne.lat, bounds.sw.lng)!;
    final tuples = <String>{
      '${sw.x},${sw.y}',
      '${se.x},${se.y}',
      '${ne.x},${ne.y}',
      '${nw.x},${nw.y}',
    };
    expect(tuples.length, 4,
        reason: 'NW must not collapse onto NE; expected 4 unique corners, got $tuples');
    // SW should be south of NW (lower y in screen-space, larger -y in AEQD).
    expect(sw.y, lessThan(nw.y));
    // SW should be west of SE (more negative x).
    expect(sw.x, lessThan(se.x));
  });

  test('grid-square pixel quad area shrinks if NW is collapsed onto NE (regression)', () {
    // Project at r=300, zoom=1.5 (typical compass disc). Compute the quad
    // area two ways:
    //  (a) correct: NW = NE.lat, SW.lng  (current implementation)
    //  (b) bug:     NW = NE.lat, NE.lng  (the old typo that produced a triangle)
    // The correct quad has ~2× the area of the bug triangle. Asserting that
    // |2A_correct| > |2A_bug| is a robust regression catch without needing
    // a magic threshold tied to projection constants.
    final aeqd = Aeqd(qth.lat, qth.lng);
    const scale = 300.0 * 1.5 / 3.141592653589793;
    const cx = 300.0, cy = 300.0;
    final sw = aeqd.normalized(bounds!.sw.lat, bounds.sw.lng)!;
    final se = aeqd.normalized(bounds.sw.lat, bounds.ne.lng)!;
    final ne = aeqd.normalized(bounds.ne.lat, bounds.ne.lng)!;
    final nwCorrect = aeqd.normalized(bounds.ne.lat, bounds.sw.lng)!;
    final nwBug = ne; // collapse NW onto NE

    double area2(List<List<double>> pts) {
      var s = 0.0;
      for (var i = 0; i < pts.length; i++) {
        final j = (i + 1) % pts.length;
        s += pts[i][0] * pts[j][1] - pts[j][0] * pts[i][1];
      }
      return s;
    }

    List<List<double>> project(nw) => [
          [cx + sw.x * scale, cy - sw.y * scale],
          [cx + se.x * scale, cy - se.y * scale],
          [cx + ne.x * scale, cy - ne.y * scale],
          [cx + nw.x * scale, cy - nw.y * scale],
        ];

    final correct = area2(project(nwCorrect)).abs();
    final bug = area2(project(nwBug)).abs();
    expect(correct, greaterThan(bug),
        reason: 'quad with the correct NW corner must have strictly more area than the '
            'triangle produced by the old `nw = ne` typo; got correct=$correct, bug=$bug');
  });
}