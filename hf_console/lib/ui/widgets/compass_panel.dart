import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import 'dart:math' as math;
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';

class CompassPanel extends StatelessWidget {
  final bool showHeader;

  const CompassPanel({super.key, this.showHeader = true});

  @override
  Widget build(BuildContext context) {
    return showHeader
        ? CardContainer(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                CardHeader(
                  title: 'Beam',
                  trailing: Text('rotator + ultrabeam', style: AppTheme.mono(10, color: AppTheme.txtFaint)),
                ),
                const SizedBox(height: 8),
                const Expanded(child: _CompassBody()),
              ],
            ),
          )
        : const _CompassBody();
  }
}

class _CompassBody extends StatelessWidget {
  const _CompassBody();

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final rotator = store.slots['muehle/hf/rotator'];
    final rotatorOnline = rotator?.isOnline ?? false;
    final az = store.stateValueAs<num>('muehle/hf/rotator', 'az')?.toDouble() ?? 0.0;
    final targetAz = store.stateValueAs<num>('muehle/hf/rotator', 'target_az')?.toDouble() ?? az;
    final moving = store.stateValueAs<bool>('muehle/hf/rotator', 'moving') ?? false;

    final antCtrl = store.slots['muehle/hf/ant-ctrl'];
    final antCtrlOnline = antCtrl?.isOnline ?? false;
    final direction = store.stateValueAs<String>('muehle/hf/ant-ctrl', 'direction') ?? 'forward';

    final targetDiff = (targetAz - az).abs();
    final targetVisible = targetDiff > 5.0;
    final statusParts = <String>[
      '${az.round()}°',
      if (rotatorOnline && targetVisible) 'TARGET ${targetAz.round()}°',
      if (moving) 'MOVING',
      if (!rotatorOnline) 'ROTATOR OFFLINE',
      if (!antCtrlOnline) 'ANT-CTRL OFFLINE',
    ];

    void sendAz(double value) {
      mqtt.publish(cmdTopic('hf/rotator'), rotatorAzPayload(value), retain: cmdRetain['muehle/hf/rotator']!);
    }

    void sendStop() {
      mqtt.publish(cmdTopic('hf/rotator'), rotatorStopPayload(), retain: false);
    }

    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Expanded(
          child: LayoutBuilder(
            builder: (context, constraints) {
              final size = math.min(constraints.maxWidth, constraints.maxHeight * 1.15);
              return Center(
                child: SizedBox(
                  width: size,
                  height: size,
                  child: CustomPaint(
                    painter: _CompassPainter(az: az, direction: direction, targetAz: targetAz),
                  ),
                ),
              );
            },
          ),
        ),
        Center(
          child: Container(
            margin: const EdgeInsets.only(top: 6, bottom: 6),
            padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 4),
            decoration: BoxDecoration(
              color: AppTheme.accentDim,
              border: Border.all(color: AppTheme.accent),
              borderRadius: BorderRadius.circular(4),
            ),
            child: Text(
              statusParts.join(' · '),
              style: AppTheme.mono(13, color: AppTheme.accent, weight: FontWeight.w600),
            ),
          ),
        ),
        Padding(
          padding: const EdgeInsets.only(bottom: 8),
          child: Wrap(
            spacing: 4,
            runSpacing: 4,
            alignment: WrapAlignment.center,
            children: [
              _Preset('NA 330', onPressed: rotatorOnline ? () => sendAz(330) : null),
              _Preset('SA 210', onPressed: rotatorOnline ? () => sendAz(210) : null),
              _Preset('VK 60', onPressed: rotatorOnline ? () => sendAz(60) : null),
              _Preset('JA 35', onPressed: rotatorOnline ? () => sendAz(35) : null),
              _Preset('STOP', danger: true, onPressed: rotatorOnline ? sendStop : null),
            ],
          ),
        ),
      ],
    );
  }
}

class _Preset extends StatelessWidget {
  final String label;
  final bool danger;
  final VoidCallback? onPressed;

  const _Preset(this.label, {this.danger = false, this.onPressed});

  @override
  Widget build(BuildContext context) {
    return ElevatedButton(
      onPressed: onPressed,
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
    final r = math.min(cx, cy) - 10;

    canvas.drawCircle(Offset(cx, cy), r, Paint()..color = AppTheme.card);
    canvas.drawCircle(
      Offset(cx, cy),
      r,
      Paint()
        ..color = AppTheme.cardLineHi
        ..style = PaintingStyle.stroke
        ..strokeWidth = 1,
    );

    final tickPaint = Paint()
      ..color = AppTheme.txtFaint
      ..strokeWidth = 1;
    for (int a = 0; a < 360; a += 30) {
      final major = a % 90 == 0;
      final p1 = _pt(cx, cy, a.toDouble(), r);
      final p2 = _pt(cx, cy, a.toDouble(), major ? r - 12.0 : r - 6.0);
      canvas.drawLine(p1, p2, tickPaint..strokeWidth = major ? 2 : 1);
    }

    _label(canvas, 'N', cx, cy, 0.0, r - 20, AppTheme.accent, 14, FontWeight.w700);
    _label(canvas, 'E', cx, cy, 90.0, r - 20, AppTheme.txtFaint, 12, FontWeight.w500);
    _label(canvas, 'S', cx, cy, 180.0, r - 20, AppTheme.txtFaint, 12, FontWeight.w500);
    _label(canvas, 'W', cx, cy, 270.0, r - 20, AppTheme.txtFaint, 12, FontWeight.w500);

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
          ..color = AppTheme.blend(b.color, b.glow ? 0.55 : 0.40)
          ..style = PaintingStyle.fill,
      );
    }

    for (final b in beams) {
      final p1 = _pt(cx, cy, b.ang, 28);
      final p2 = _pt(cx, cy, b.ang, r - 14);
      canvas.drawLine(p1, p2, Paint()..color = b.color..strokeWidth = 3.5..strokeCap = StrokeCap.round);
      _drawArrow(canvas, cx, cy, b.ang, r - 10, b.color);
    }

    final boomStart = _pt(cx, cy, az, 16);
    final boomEnd = _pt(cx, cy, az, r - 2);
    canvas.drawLine(boomStart, boomEnd, Paint()..color = AppTheme.txt..strokeWidth = 2..strokeCap = StrokeCap.round);

    canvas.drawCircle(Offset(cx, cy), 5, Paint()..color = AppTheme.accent);

    final targetDiff = (targetAz - az).abs();
    if (targetDiff > 5.0) {
      final targetStart = _pt(cx, cy, targetAz, 16);
      final targetEnd = _pt(cx, cy, targetAz, r - 6);
      canvas.drawLine(
        targetStart,
        targetEnd,
        Paint()
          ..color = AppTheme.blend(AppTheme.accent, 0.55)
          ..strokeWidth = 1.5
          ..strokeCap = StrokeCap.round,
      );
    }
  }

  List<_Beam> _beams() {
    const half = {'forward': 30.0, 'reverse': 30.0, 'bidirectional': 45.0};
    return direction == 'forward'
        ? [_Beam(az, half['forward']!, AppTheme.accent, false)]
        : direction == 'reverse'
            ? [_Beam((az + 180) % 360, half['reverse']!, AppTheme.amber, true)]
            : [
                _Beam(az, half['bidirectional']!, AppTheme.accent, false),
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
    final left = _pt(tip.dx, tip.dy, angle - 135, 11);
    final right = _pt(tip.dx, tip.dy, angle + 135, 11);
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
