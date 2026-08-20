import 'package:flutter/material.dart';
import 'dart:math' as math;
import '../theme.dart';
import 'card_container.dart';

class CompassPanel extends StatelessWidget {
  const CompassPanel({super.key});

  @override
  Widget build(BuildContext context) {
    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          CardHeader(
            title: 'Beam',
            trailing: Text('rotator + ultrabeam', style: AppTheme.mono(10, color: AppTheme.txtFaint)),
          ),
          const SizedBox(height: 8),
          Expanded(
            child: LayoutBuilder(
              builder: (context, constraints) {
                final size = math.min(constraints.maxWidth, constraints.maxHeight * 1.2);
                return Center(
                  child: SizedBox(
                    width: size,
                    height: size,
                    child: CustomPaint(
                      painter: _CompassPainter(az: 247, direction: 'reverse', targetAz: 67),
                    ),
                  ),
                );
              },
            ),
          ),
          const SizedBox(height: 6),
          Center(
            child: Text(
              'SLEWING → 067°',
              style: AppTheme.mono(10.5, color: AppTheme.cyan, weight: FontWeight.w600),
            ),
          ),
          const SizedBox(height: 6),
          Wrap(
            spacing: 4,
            runSpacing: 4,
            alignment: WrapAlignment.center,
            children: const [
              _Preset('045'),
              _Preset('090'),
              _Preset('180'),
              _Preset('270'),
              _Preset('STOP', danger: true),
            ],
          ),
        ],
      ),
    );
  }
}

class _Preset extends StatelessWidget {
  final String label;
  final bool danger;

  const _Preset(this.label, {this.danger = false});

  @override
  Widget build(BuildContext context) {
    return ElevatedButton(
      onPressed: () {},
      style: AppTheme.actionButton(danger: danger),
      child: Text(label),
    );
  }
}

class _CompassPainter extends CustomPainter {
  final double az;
  final String direction;
  final double targetAz;

  _CompassPainter({required this.az, required this.direction, required this.targetAz});

  @override
  void paint(Canvas canvas, Size size) {
    final cx = size.width / 2;
    final cy = size.height / 2;
    final r = math.min(cx, cy) - 8;

    // face
    canvas.drawCircle(Offset(cx, cy), r, Paint()..color = const Color(0xFF101216));
    canvas.drawCircle(
      Offset(cx, cy),
      r,
      Paint()
        ..color = AppTheme.txtMute
        ..style = PaintingStyle.stroke
        ..strokeWidth = 1,
    );

    // ticks
    final tickPaint = Paint()
      ..color = AppTheme.txtMute
      ..strokeWidth = 1;
    for (int a = 0; a < 360; a += 30) {
      final major = a % 90 == 0;
      final p1 = _pt(cx, cy, a.toDouble(), r);
      final p2 = _pt(cx, cy, a.toDouble(), major ? r - 10.0 : r - 5.0);
      canvas.drawLine(p1, p2, tickPaint..strokeWidth = major ? 1.5 : 1);
    }

    // labels
    _label(canvas, 'N', cx, cy, 0.0, r - 18, AppTheme.cyan, 12, FontWeight.w700);
    _label(canvas, 'E', cx, cy, 90.0, r - 18, AppTheme.txtMute, 11, FontWeight.w500);
    _label(canvas, 'S', cx, cy, 180.0, r - 18, AppTheme.txtMute, 11, FontWeight.w500);
    _label(canvas, 'W', cx, cy, 270.0, r - 18, AppTheme.txtMute, 11, FontWeight.w500);

    // lobes
    final beams = _beams();
    for (final b in beams) {
      final path = Path();
      path.moveTo(cx, cy);
      final a0 = b.ang - b.half;
      final a1 = b.ang + b.half;
      path.arcTo(
        Rect.fromCircle(center: Offset(cx, cy), radius: r - 4),
        (a0 - 90) * math.pi / 180,
        (a1 - a0) * math.pi / 180,
        false,
      );
      path.close();
      canvas.drawPath(
        path,
        Paint()
          ..color = b.color.withAlpha(b.glow ? 242 : 153)
          ..style = PaintingStyle.fill
          ..maskFilter = b.glow ? const MaskFilter.blur(BlurStyle.normal, 5) : null,
      );
    }

    // bearing arrows
    for (final b in beams) {
      final solidColor = Color.fromARGB(255, b.color.r.toInt(), b.color.g.toInt(), b.color.b.toInt());
      final p1 = _pt(cx, cy, b.ang, 22);
      final p2 = _pt(cx, cy, b.ang, r - 12);
      canvas.drawLine(p1, p2, Paint()..color = solidColor..strokeWidth = 3..strokeCap = StrokeCap.round);
      _drawArrow(canvas, cx, cy, b.ang, r - 8, solidColor);
    }

    // boom needle
    final boomStart = _pt(cx, cy, az, 14);
    final boomEnd = _pt(cx, cy, az, r - 2);
    canvas.drawLine(boomStart, boomEnd, Paint()..color = AppTheme.txt..strokeWidth = 2..strokeCap = StrokeCap.round);

    // hub
    canvas.drawCircle(Offset(cx, cy), 5, Paint()..color = AppTheme.cyan);

    // target line
    final targetStart = _pt(cx, cy, targetAz, 14);
    final targetEnd = _pt(cx, cy, targetAz, r - 6);
    canvas.drawLine(
      targetStart,
      targetEnd,
      Paint()
        ..color = const Color(0x801E7A7A)
        ..strokeWidth = 1.5
        ..strokeCap = StrokeCap.round,
    );

    // azimuth readout
    final tp = TextPainter(
      text: TextSpan(text: '${az.toInt()}°', style: AppTheme.mono(22, color: AppTheme.cyan, weight: FontWeight.w700)),
      textDirection: TextDirection.ltr,
    );
    tp.layout();
    tp.paint(canvas, Offset(cx + r - tp.width - 8, cy - r + 8));
  }

  List<_Beam> _beams() {
    const half = {'forward': 30.0, 'reverse': 30.0, 'bidirectional': 45.0};
    return direction == 'forward'
        ? [_Beam(az, half['forward']!, AppTheme.cyan, false)]
        : direction == 'reverse'
            ? [_Beam((az + 180) % 360, half['reverse']!, AppTheme.amber, true)]
            : [
                _Beam(az, half['bidirectional']!, AppTheme.cyan, false),
                _Beam((az + 180) % 360, half['bidirectional']!, AppTheme.amber, true),
              ];
  }

  Offset _pt(double cx, double cy, double angle, double radius) {
    final rad = (angle - 90) * math.pi / 180;
    return Offset(cx + radius * math.cos(rad), cy + radius * math.sin(rad));
  }

  void _label(Canvas c, String text, double cx, double cy, double angle, double r, Color color, double size, FontWeight w) {
    final p = _pt(cx, cy, angle, r);
    final tp = TextPainter(
      text: TextSpan(text: text, style: AppTheme.mono(size, color: color, weight: w)),
      textDirection: TextDirection.ltr,
      textAlign: TextAlign.center,
    );
    tp.layout();
    tp.paint(c, Offset(p.dx - tp.width / 2.0, p.dy - tp.height / 2.0));
  }

  void _drawArrow(Canvas c, double cx, double cy, double angle, double r, Color color) {
    final tip = _pt(cx, cy, angle, r);
    final path = Path();
    path.moveTo(tip.dx, tip.dy);
    final left = _pt(tip.dx, tip.dy, angle - 135, 10);
    final right = _pt(tip.dx, tip.dy, angle + 135, 10);
    path.lineTo(left.dx, left.dy);
    path.lineTo(right.dx, right.dy);
    path.close();
    c.drawPath(path, Paint()..color = color);
  }

  @override
  bool shouldRepaint(covariant CustomPainter oldDelegate) => true;
}

class _Beam {
  final double ang;
  final double half;
  final Color color;
  final bool glow;
  _Beam(this.ang, this.half, this.color, this.glow);
}
