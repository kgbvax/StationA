import 'dart:math' as math;

import 'package:flutter/material.dart';

/// Polarization symbol in the usual antenna notation.
///
/// - `h` / `v`: a double-headed arrow along the E-field — horizontal or
///   vertical.
/// - `cl` / `cr`: a near-full circle with an arrowhead showing the sense of
///   rotation, IEEE convention (as seen from the transmitter, looking along
///   the direction of propagation): left-hand = counter-clockwise,
///   right-hand = clockwise. The same convention satellite ops use.
///
/// Any other value paints nothing — the panel never draws a guess.
class PolGlyph extends StatelessWidget {
  final String pol;
  final Color color;
  final double size;

  const PolGlyph({super.key, required this.pol, required this.color, this.size = 24});

  static bool supports(String? pol) => const {'h', 'v', 'cl', 'cr'}.contains(pol);

  @override
  Widget build(BuildContext context) {
    return CustomPaint(
      size: Size.square(size),
      painter: _PolGlyphPainter(pol: pol, color: color),
    );
  }
}

class _PolGlyphPainter extends CustomPainter {
  final String pol;
  final Color color;

  _PolGlyphPainter({required this.pol, required this.color});

  @override
  void paint(Canvas canvas, Size size) {
    final s = size.shortestSide;
    final c = size.center(Offset.zero);
    final stroke = Paint()
      ..color = color
      ..style = PaintingStyle.stroke
      ..strokeWidth = math.max(1.5, s * 0.09)
      ..strokeCap = StrokeCap.round;
    final fill = Paint()
      ..color = color
      ..style = PaintingStyle.fill;
    final head = s * 0.26;

    switch (pol) {
      case 'h':
      case 'v':
        final half = s * 0.42;
        final dir = pol == 'h' ? const Offset(1, 0) : const Offset(0, 1);
        final a = c - dir * half;
        final b = c + dir * half;
        // Shaft stops short of the tips so the heads stay sharp.
        canvas.drawLine(a + dir * head * 0.8, b - dir * head * 0.8, stroke);
        _arrowHead(canvas, b, dir, head, fill);
        _arrowHead(canvas, a, -dir, head, fill);
      case 'cl':
      case 'cr':
        final r = s * 0.36;
        final ccw = pol == 'cl';
        // Screen y points down, so a positive sweep is clockwise.
        const gap = 1.1; // radians left open for the arrowhead
        // The arc ends at the top of the circle, so the head lies flat and
        // points left (↺, left-hand) or right (↻, right-hand).
        const end = -math.pi / 2;
        final sweep = (2 * math.pi - gap) * (ccw ? -1 : 1);
        canvas.drawArc(Rect.fromCircle(center: c, radius: r), end - sweep, sweep, false, stroke);
        final tip = c + Offset(math.cos(end), math.sin(end)) * r;
        // Direction of travel at the arc's end.
        final tangent = Offset(ccw ? -1 : 1, 0);
        _arrowHead(canvas, tip + tangent * head * 0.85, tangent, head, fill);
    }
  }

  /// Filled triangle with its apex at [tip], pointing along unit [dir].
  void _arrowHead(Canvas canvas, Offset tip, Offset dir, double len, Paint paint) {
    final normal = Offset(-dir.dy, dir.dx);
    final base = tip - dir * len;
    final path = Path()
      ..moveTo(tip.dx, tip.dy)
      ..lineTo((base + normal * len * 0.6).dx, (base + normal * len * 0.6).dy)
      ..lineTo((base - normal * len * 0.6).dx, (base - normal * len * 0.6).dy)
      ..close();
    canvas.drawPath(path, paint);
  }

  @override
  bool shouldRepaint(covariant _PolGlyphPainter old) => old.pol != pol || old.color != color;
}
