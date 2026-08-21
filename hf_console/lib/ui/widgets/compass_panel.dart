import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import 'dart:math' as math;
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../../dxspot/dxspot_service.dart';
import '../../dxspot/projection.dart';
import '../theme.dart';
import 'card_container.dart';

class CompassPanel extends StatelessWidget {
  const CompassPanel({super.key});

  @override
  Widget build(BuildContext context) {
    return const _CompassBody();
  }
}

class _CompassBody extends StatelessWidget {
  const _CompassBody();

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();
    final dx = context.watch<DxSpotService>();

    final rotator = store.slots['muehle/hf/rotator'];
    final rotatorOnline = rotator?.isOnline ?? false;
    final az = store.stateValueAs<num>('muehle/hf/rotator', 'az')?.toDouble() ?? 0.0;
    final targetAz = store.stateValueAs<num>('muehle/hf/rotator', 'target_az')?.toDouble() ?? az;
    final moving = store.stateValueAs<bool>('muehle/hf/rotator', 'moving') ?? false;

    final direction = store.stateValueAs<String>('muehle/hf/ant-ctrl', 'direction') ?? 'forward';

    final targetDiff = (targetAz - az).abs();
    final targetVisible = targetDiff > 5.0;

    // DX-overlay status line for the card header: off entirely when no station
    // locator is set; otherwise show spot count / connecting / feed-down.
    final dxTitle = !dx.active
        ? ''
        : (dx.error != null
            ? 'DX ✗'
            : dx.connected
                ? 'DX ${dx.spots.length}'
                : 'DX …');

    final azimuthParts = <String>[
      '${az.round()}°',
      if (rotatorOnline && targetVisible) '→ ${targetAz.round()}°',
      if (moving) 'MOVING',
      if (!rotatorOnline) 'OFFLINE',
    ];
    final azimuthColor = moving
        ? AppTheme.red
        : !rotatorOnline
            ? AppTheme.txtMute
            : AppTheme.accent;

    void sendAz(double value) {
      mqtt.publish(cmdTopic('hf/rotator'), rotatorAzPayload(value), retain: cmdRetain['muehle/hf/rotator']!);
    }

    void sendStop() {
      mqtt.publish(cmdTopic('hf/rotator'), rotatorStopPayload(), retain: false);
    }

    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 12, vertical: 10),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          CardHeader(
            title: dxTitle,
            trailing: Container(
              padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 3),
              decoration: BoxDecoration(
                color: AppTheme.blend(azimuthColor, 0.12),
                border: Border.all(color: azimuthColor),
                borderRadius: BorderRadius.circular(4),
              ),
              child: Text(
                azimuthParts.join(' · '),
                style: AppTheme.mono(12, weight: FontWeight.w700, color: azimuthColor),
              ),
            ),
          ),
        const SizedBox(height: 8),
        Expanded(
          child: LayoutBuilder(
            builder: (context, constraints) {
              final size = math.min(constraints.maxWidth, constraints.maxHeight * 1.15);
              return Center(
                child: SizedBox(
                  width: size,
                  height: size,
                  child: GestureDetector(
                    behavior: HitTestBehavior.opaque,
                    onTapUp: rotatorOnline
                        ? (details) {
                            final local = details.localPosition;
                            final cx = size / 2;
                            final cy = size / 2;
                            final dx = local.dx - cx;
                            final dy = local.dy - cy;
                            final deg = math.atan2(dy, dx) * 180 / math.pi;
                            final tappedAz = ((deg + 90) % 360 + 360) % 360;
                            sendAz(tappedAz);
                          }
                        : null,
                    child: CustomPaint(
                      painter: _CompassPainter(
                        az: az,
                        direction: direction,
                        targetAz: targetAz,
                        spots: dx.spots,
                        centerLat: dx.centerLat,
                        centerLng: dx.centerLng,
                      ),
                    ),
                  ),
                ),
              );
            },
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
    ),
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
  final List<DxSpot> spots;
  final double? centerLat;
  final double? centerLng;

  _CompassPainter({
    required this.az,
    required this.direction,
    required this.targetAz,
    this.spots = const [],
    this.centerLat,
    this.centerLng,
  });

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

    _drawSpots(canvas, cx, cy, r);

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

  /// AEQD-project the DX spots onto the compass disc and draw them, colored by
  /// `sourceType`. Antipode maps to the rim (zoom 1.0 → whole world). Labels are
  /// drawn only for the freshest/strongest few to avoid clutter. No-op without a
  /// projection center (beam-only compass).
  void _drawSpots(Canvas canvas, double cx, double cy, double r) {
    final lat0 = centerLat;
    final lon0 = centerLng;
    if (lat0 == null || lon0 == null || spots.isEmpty) return;

    const zoom = 1.0;
    // Keep dots just inside the tick ring (ticks are r-6..r-12).
    final drawR = math.max(20.0, r - 16);
    final scale = drawR * zoom / math.pi;
    final aeqd = Aeqd(lat0, lon0);
    final nowMs = DateTime.now().millisecondsSinceEpoch;

    // Labels: top 6 by snr among spots fresher than 2 min (live age), one label per
    // callsign. Skip a label whose box overlaps an already-placed label (#10).
    final fresh = spots.where((s) => s.liveAgeSecondsAt(nowMs) < 120).toList()
      ..sort((a, b) => b.snr.compareTo(a.snr));
    final labelSet = <String>{};
    for (final s in fresh) {
      if (labelSet.length >= 6) break;
      labelSet.add(s.label);
    }
    final drawnLabels = <String>{};
    final placedLabelRects = <Rect>[];

    for (final spot in spots) {
      final n = aeqd.normalized(spot.lat, spot.lng);
      if (n == null) continue;
      final px = cx + n.x * scale;
      final py = cy - n.y * scale;
      // Safety clip: anything past the rim is dropped.
      if (math.sqrt((px - cx) * (px - cx) + (py - cy) * (py - cy)) > drawR + 1) continue;

      final color = AppTheme.spotColor(spot.sourceType);
      final rad = 2.0 + (spot.snr.clamp(-10, 40) + 10) / 50.0 * 2.5;

      // Soft halo so dots read against the beam-wedge fill, then the dot itself.
      canvas.drawCircle(Offset(px, py), rad + 2.5, Paint()..color = AppTheme.blend(color, 0.18));
      canvas.drawCircle(Offset(px, py), rad, Paint()..color = color);

      if (spot.liveAgeSecondsAt(nowMs) < 120 &&
          labelSet.contains(spot.label) &&
          drawnLabels.add(spot.label)) {
        final tp = TextPainter(
          text: TextSpan(text: spot.label, style: AppTheme.mono(8, color: AppTheme.txtMute, weight: FontWeight.w600)),
          textDirection: TextDirection.ltr,
        )..layout();
        var lx = (px - cx) >= 0 ? px + rad + 3 : px - rad - 3 - tp.width;
        var ly = py - tp.height / 2;
        // Nudge a colliding label vertically until it clears an already-placed one.
        final rect = Rect.fromLTWH(lx, ly, tp.width, tp.height);
        if (_overlapsAny(rect, placedLabelRects)) {
          final shifted = _shiftLabel(rect, placedLabelRects);
          lx = shifted.left;
          ly = shifted.top;
        }
        // Drop the label if the nudge pushed it out of the disc.
        final placed = Rect.fromLTWH(lx, ly, tp.width, tp.height);
        if (_insideDisc(placed, cx, cy, drawR)) {
          placedLabelRects.add(placed);
          tp.paint(canvas, Offset(lx, ly));
        }
      }
    }
  }

  bool _overlapsAny(Rect r, List<Rect> rects) {
    for (final o in rects) {
      if (r.overlaps(o.inflate(2))) return true;
    }
    return false;
  }

  // Try a few vertical nudges to clear already-placed labels; returns the first
  // non-colliding Rect, or the original if none found.
  Rect _shiftLabel(Rect r, List<Rect> placed) {
    const step = 9.0;
    for (int i = 1; i <= 6; i++) {
      for (final sign in [1.0, -1.0]) {
        final candidate = Rect.fromLTWH(r.left, r.top + sign * i * step, r.width, r.height);
        if (!_overlapsAny(candidate, placed)) return candidate;
      }
    }
    return r;
  }

  bool _insideDisc(Rect r, double cx, double cy, double drawR) {
    final c = r.center;
    return (c.dx - cx) * (c.dx - cx) + (c.dy - cy) * (c.dy - cy) <= drawR * drawR;
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
