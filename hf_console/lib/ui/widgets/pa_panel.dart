import 'dart:async';
import 'dart:math' as math;

import 'package:clock/clock.dart';
import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import '../../store/bus_store.dart';
import '../../mqtt/mqtt_service.dart';
import '../../store/wiring.dart';
import '../theme.dart';
import 'card_container.dart';
import 'pulsing_amber_button.dart';
import 'status_pill.dart';

class PaPanel extends StatefulWidget {
  const PaPanel({super.key});

  @override
  State<PaPanel> createState() => _PaPanelState();
}

class _PaPanelState extends State<PaPanel> {
  // Meter ballistics: instant attack, slow release on the bars, and a held
  // peak marker, so a speech/FT8 envelope reads steadily instead of
  // jumping with every /state update.
  final _fwd = _Ballistics(min: 0, max: 1200);
  final _swr = _Ballistics(min: 1.0, max: 4.0);

  // ~30 fps: bars and markers glide down instead of stepping. Only runs while
  // something still settles, so the cost is bounded to decays.
  static const Duration _decayInterval = Duration(milliseconds: 33);
  Timer? _decayTimer;
  // Anchor for elapsed-time decay: the step is derived from how long the
  // last tick actually took, so timer jitter never changes the drain rate.
  DateTime? _lastDecayAt;

  /// Keep the decay timer running exactly while a bar or marker still
  /// stands above the live reading; once everything has settled the timer
  /// stops until the next burst.
  void _syncDecayTimer() {
    if (_fwd.settling || _swr.settling) {
      if (_decayTimer == null) {
        _lastDecayAt = clock.now();
        _decayTimer = Timer.periodic(_decayInterval, (_) => _decayTick());
      }
    } else {
      _decayTimer?.cancel();
      _decayTimer = null;
      _lastDecayAt = null;
    }
  }

  void _decayTick() {
    if (!mounted) return;
    final now = clock.now();
    final last = _lastDecayAt ?? now;
    _lastDecayAt = now;
    final dt = now.difference(last).inMicroseconds / 1e6;
    setState(() {
      _fwd.tick(dt, now);
      _swr.tick(dt, now);
    });
    _syncDecayTimer();
  }

  @override
  void dispose() {
    _decayTimer?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final store = context.watch<BusStore>();
    final mqtt = context.read<MqttService>();

    final slot = store.slots['muehle/hf/pa'];
    final online = (slot?.isOnline ?? false) && store.linkUp;
    final mode = store.stateValueAs<String>('muehle/hf/pa', 'mode') ?? 'standby';
    final keyed = store.stateValueAs<String>('muehle/hf/pa', 'keyed') ?? 'rx';
    final fault = store.stateValueAs<String>('muehle/hf/pa', 'fault') ?? 'none';
    final error = store.stateValueAs<String>('muehle/hf/pa', 'error') ?? '';
    final fwd = store.stateValueAs<num>('muehle/hf/pa', 'fwd_power_w')?.toDouble() ?? 0.0;
    final swr = store.stateValueAs<num>('muehle/hf/pa', 'swr')?.toDouble() ?? 1.0;

    // Cross-links: the PA remote-on relay (hf/switch) and the amp's own
    // power telemetry both gate operation but live on other slots — without
    // them a dead amp presents as a healthy standby PA. Unknown state must
    // not be asserted as 'off': a silent/cleared hf/switch slot knows nothing
    // about the relay, and claiming it open would fabricate a no-RF verdict.
    final paRelayState = store.stateValueAs<String>('muehle/hf/switch', 'pa');
    final paPower = store.stateValueAs<String>('muehle/hf/pa', 'power');

    final now = clock.now();
    _fwd.sample(fwd, now);
    _swr.sample(swr, now);
    _syncDecayTimer();

    final (suffix, suffixColor) =
        _paState(keyed, fault, error, paRelayState, paPower) ?? ('', null);

    void setMode(String value) {
      if (!online) return;
      mqtt.publish(
        cmdTopic('hf/pa'),
        paSetModePayload(value),
        retain: cmdRetain['muehle/hf/pa']!,
      );
    }

    return CardContainer(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          CardHeader(
            title: 'PA',
            trailing: StatusPill(
              slots: const ['muehle/hf/pa'],
              label: 'ACOM 1200S',
              suffix: suffix.isEmpty ? null : suffix,
              suffixColor: suffixColor,
              stickySuffix: true, // FAULT diagnoses survive a dead link
            ),
          ),
          Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            mainAxisSize: MainAxisSize.min,
            children: [
              Row(
                crossAxisAlignment: CrossAxisAlignment.end,
                children: [
                  Expanded(
                    child: _Meter(
                      value: _fwd.bar,
                      max: 1200,
                      unit: 'W FWD',
                      ticks: const [(0, '0'), (500, '500'), (1000, '1000'), (1200, '1200')],
                      fillColor: AppTheme.green,
                      compact: true,
                      // Always non-null: a marker that vanished at zero would
                      // remove its reserved row and jump the layout. At zero
                      // the triangle parks at the origin instead.
                      marker: _fwd.peakFraction,
                      markerKey: const ValueKey('pa-fwd-peak'),
                    ),
                  ),
                  const SizedBox(width: 10),
                  SizedBox(
                    width: 96,
                    height: 37,
                    child: ElevatedButton(
                      onPressed: online ? () => setMode('operate') : null,
                      style: AppTheme.actionButton(active: mode == 'operate'),
                      child: const Text('OPERATE'),
                    ),
                  ),
                ],
              ),
              const SizedBox(height: 10),
              Row(
                crossAxisAlignment: CrossAxisAlignment.end,
                children: [
                  Expanded(
                    child: _Meter(
                      value: _swr.bar,
                      // SWR cannot go below 1.0 — the scale starts there, so
                      // a perfect match reads as an empty bar.
                      min: 1.0,
                      max: 4.0,
                      unit: 'SWR',
                      ticks: const [(1.0, '1.0'), (1.5, '1.5'), (3.0, '3.0'), (4.0, '4.0')],
                      fillColor: AppTheme.amber,
                      compact: true,
                      marker: _swr.peakFraction,
                      markerKey: const ValueKey('pa-swr-peak'),
                    ),
                  ),
                  const SizedBox(width: 10),
                  SizedBox(
                    width: 96,
                    height: 37,
                    // Standby engaged is the deliberate not-TX-ready state —
                    // amber pulse, not the cyan used for OPERATE.
                    child: PulsingAmberButton(
                      onPressed: online ? () => setMode('standby') : null,
                      engaged: mode == 'standby',
                      // Idle keeps the dim amber tint it always had.
                      idleStyle: AppTheme.actionButton(amber: true),
                      child: const Text('STANDBY'),
                    ),
                  ),
                ],
              ),
            ],
          ),
        ],
      ),
    );
  }

  /// Irregular states only — the pill shows the plain green device name
  /// while the amp is online and unremarkable (operate or standby), so
  /// regular states return null. Offline is the pill's own concern.
  (String, Color)? _paState(String keyed, String fault, String error, String? paRelayState, String? paPower) {
    if (fault.isNotEmpty && fault != 'none') {
      final label = error.isNotEmpty ? error.toUpperCase() : fault.toUpperCase();
      return (label, AppTheme.red);
    }
    // A live transmit outranks relay/power plumbing: an amber 'relay off'
    // suffix must never sit where red 'TX' belongs — the relay bookkeeping is
    // the subordinate fact of the two.
    if (keyed == 'tx') return ('TX', AppTheme.red);
    if (paRelayState == null) return ('RELAY ?', AppTheme.amber);
    if (paRelayState == 'off') return ('PA RELAY OFF', AppTheme.amber);
    if (paPower == 'off') return ('PA OFF', AppTheme.amber);
    if (keyed == 'inhibited') return ('INHIBITED', AppTheme.amber);
    return null;
  }
}

/// Peak-reading meter ballistics for one scale.
///
/// The bar rises instantly and releases exponentially toward the live
/// reading. The peak marker rises instantly, holds for [holdTime], then
/// drains linearly (full scale in [peakDrainTime]) — never below the bar.
class _Ballistics {
  final double min;
  final double max;

  static const double releaseTau = 0.6; // s
  static const Duration holdTime = Duration(seconds: 2);
  static const double peakDrainTime = 10; // s for a full-scale peak

  double _live;
  double bar;
  double peak;
  DateTime? _peakAt;

  _Ballistics({required this.min, required this.max})
      : _live = min,
        bar = min,
        peak = min;

  double get _span => max - min;

  /// Peak position as a fraction of the scale (0..1).
  double get peakFraction => ((peak - min) / _span).clamp(0.0, 1.0);

  /// True while the bar or marker still stands above where it will settle.
  bool get settling => bar > _live || peak > bar;

  void sample(double live, DateTime now) {
    _live = live;
    if (live > bar) bar = live;
    if (live >= peak) {
      peak = live;
      _peakAt = now;
    }
  }

  void tick(double dt, DateTime now) {
    if (bar > _live) {
      bar = _live + (bar - _live) * math.exp(-dt / releaseTau);
      if (bar - _live < 0.005 * _span) bar = _live;
    }
    final held = _peakAt != null && now.difference(_peakAt!) < holdTime;
    if (!held) peak -= _span / peakDrainTime * dt;
    if (peak < bar) peak = bar;
  }
}

class _Meter extends StatelessWidget {
  final double value;
  final double min;
  final double max;
  final String unit;

  /// Scale labels as (value, text). Each label sits at its value's true
  /// position on the bar, so the ticks line up with the fill and markers.
  final List<(double, String)> ticks;
  final Color fillColor;
  final bool compact;

  /// Optional peak marker, as a fraction of the scale (0..1): a downward
  /// triangle above the bar. Its row is reserved only while [marker] is
  /// non-null, so a meter whose marker toggles to null at zero would jump
  /// its layout — pass 0 there and the marker parks at the origin.
  final double? marker;
  final Key? markerKey;

  const _Meter({
    required this.value,
    this.min = 0,
    required this.max,
    required this.unit,
    required this.ticks,
    required this.fillColor,
    this.compact = false,
    this.marker,
    this.markerKey,
  });

  static const double _markerSize = 7;
  static const double _markerGap = 1;

  @override
  Widget build(BuildContext context) {
    final fraction = _fractionOf(value);
    final valueStyle = AppTheme.mono(compact ? 18 : 24, weight: FontWeight.w700);
    final unitStyle = AppTheme.mono(compact ? 11 : 13, color: AppTheme.txtFaint);
    final labelStyle = AppTheme.mono(compact ? 9 : 11, color: AppTheme.txtFaint);
    final barHeight = compact ? 8.0 : 12.0;

    final markerSpace = marker != null ? _markerSize + _markerGap : 0.0;
    final stackHeight = barHeight + markerSpace;

    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        Text.rich(
          TextSpan(
            children: [
              TextSpan(text: value.toStringAsFixed(value < 10 ? 1 : 0), style: valueStyle),
              TextSpan(text: ' $unit', style: unitStyle),
            ],
          ),
        ),
        SizedBox(height: compact ? 2 : 3),
        SizedBox(
          key: const ValueKey('pa-meter-stack'),
          height: stackHeight,
          child: LayoutBuilder(
            builder: (context, constraints) {
              final w = constraints.maxWidth;
              return Stack(
                clipBehavior: Clip.none,
                children: [
                  Positioned(
                    top: markerSpace,
                    left: 0,
                    right: 0,
                    height: barHeight,
                    child: Container(decoration: BoxDecoration(color: AppTheme.pane, borderRadius: BorderRadius.circular(4), border: Border.all(color: AppTheme.cardLine))),
                  ),
                  Positioned(
                    top: markerSpace,
                    left: 0,
                    width: fraction * w,
                    height: barHeight,
                    child: Container(
                      decoration: BoxDecoration(
                        gradient: LinearGradient(colors: [fillColor, fillColor, AppTheme.orange]),
                        borderRadius: BorderRadius.circular(4),
                      ),
                    ),
                  ),
                  if (marker != null)
                    Positioned(
                      top: 0,
                      left: _markerLeft(marker!, w),
                      child: _TriangleMarker(key: markerKey, color: AppTheme.txt, pointDown: true, size: _markerSize),
                    ),
                ],
              );
            },
          ),
        ),
        SizedBox(height: compact ? 2 : 3),
        // Alignment(-1 + 2f) anchors the label's own f-point at f of the
        // width: the first label stays flush left, the last flush right,
        // and a middle label lands within a few px of its true position.
        Stack(
          children: [
            for (final (v, text) in ticks)
              Align(
                alignment: Alignment(-1 + 2 * _fractionOf(v), 0),
                heightFactor: 1,
                child: Text(text, style: labelStyle),
              ),
          ],
        ),
      ],
    );
  }

  double _fractionOf(double v) => ((v - min) / (max - min)).clamp(0.0, 1.0);

  double _markerLeft(double fraction, double width) {
    if (width <= _markerSize) return 0;
    return (fraction * width - _markerSize / 2).clamp(0.0, width - _markerSize);
  }
}

class _TriangleMarker extends StatelessWidget {
  final Color color;
  final bool pointDown;
  final double size;

  const _TriangleMarker({super.key, required this.color, required this.pointDown, this.size = 7});

  @override
  Widget build(BuildContext context) {
    return CustomPaint(
      size: Size(size, size),
      painter: _TrianglePainter(color: color, pointDown: pointDown),
    );
  }
}

class _TrianglePainter extends CustomPainter {
  final Color color;
  final bool pointDown;

  _TrianglePainter({required this.color, required this.pointDown});

  @override
  void paint(Canvas canvas, Size size) {
    final paint = Paint()
      ..color = color
      ..style = PaintingStyle.fill;
    final path = Path();
    if (pointDown) {
      // Apex at the bottom centre, pointing down toward the bar.
      path
        ..moveTo(0, 0)
        ..lineTo(size.width, 0)
        ..lineTo(size.width / 2, size.height);
    } else {
      // Apex at the top centre, pointing up toward the bar.
      path
        ..moveTo(0, size.height)
        ..lineTo(size.width, size.height)
        ..lineTo(size.width / 2, 0);
    }
    path.close();
    canvas.drawPath(path, paint);
  }

  @override
  bool shouldRepaint(covariant _TrianglePainter oldDelegate) =>
      oldDelegate.color != color || oldDelegate.pointDown != pointDown;
}
