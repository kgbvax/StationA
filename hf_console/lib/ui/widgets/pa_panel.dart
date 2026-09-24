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
  // Rolling 1-second window of forward-power samples, used to draw the peak
  // (max) and 95th-percentile markers on the FWD meter. Timestamped via
  // package:clock so widget tests advance the window with tester.pump.
  final List<_FwdSample> _fwdSamples = [];

  // Peak-hold ballistics: the markers snap up instantly to the window maxima
  // and then decay linearly at a rate that drains a full-scale (1200 W) peak
  // in ~5 s, instead of vanishing the moment the sample window rolls over.
  static const double _meterFullScale = 1200;
  static const double _peakDecayPerSecond = _meterFullScale / 5; // 240 W/s
  // ~30 fps: the markers glide down instead of stepping. Only runs while a
  // marker stands above the live reading, so the cost is bounded to decays.
  static const Duration _decayInterval = Duration(milliseconds: 33);

  double _lastFwd = 0;
  double _peakHold = 0;
  double _p95Hold = 0;
  Timer? _decayTimer;
  // Anchor for elapsed-time decay: the step is derived from how long the
  // last tick actually took, so timer jitter never changes the drain rate.
  DateTime? _lastDecayAt;

  void _recordFwd(double fwd) {
    final now = clock.now();
    if (_fwdSamples.isNotEmpty && _fwdSamples.last.v == fwd) {
      // Constant value: refresh the timestamp so it stays "present" in the
      // window even when the amp holds a steady power level.
      _fwdSamples.last.t = now;
    } else {
      _fwdSamples.add(_FwdSample(now, fwd));
    }
    final cutoff = now.subtract(const Duration(seconds: 1));
    _fwdSamples.removeWhere((s) => s.t.isBefore(cutoff));
    _peakHold = math.max(_peakHold, _maxOverWindow());
    _p95Hold = math.max(_p95Hold, _p95OverWindow());
    _syncDecayTimer();
  }

  double _maxOverWindow() {
    if (_fwdSamples.isEmpty) return 0;
    return _fwdSamples.map((s) => s.v).reduce((a, b) => a > b ? a : b);
  }

  double _p95OverWindow() {
    if (_fwdSamples.isEmpty) return 0;
    final vals = _fwdSamples.map((s) => s.v).toList()..sort();
    return _percentile(vals, 0.95);
  }

  /// Keep the decay timer running exactly while a held marker still stands
  /// above the live power; once both markers have come down to the reading
  /// the timer stops until the next burst.
  void _syncDecayTimer() {
    if (_peakHold > _lastFwd || _p95Hold > _lastFwd) {
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
      // Decay toward the live reading, never below it — a new transmission
      // takes the marker over immediately.
      _peakHold = math.max(_lastFwd, _peakHold - _peakDecayPerSecond * dt);
      _p95Hold = math.max(_lastFwd, _p95Hold - _peakDecayPerSecond * dt);
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

    _lastFwd = fwd;
    _recordFwd(fwd);
    final maxFwd = _peakHold;
    final p95Fwd = _p95Hold;

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
            title: 'PA · ACOM 1200S',
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
                      value: fwd,
                      max: 1200,
                      unit: 'W FWD',
                      ticks: const [(0, '0'), (500, '500'), (1000, '1000'), (1200, '1200')],
                      fillColor: AppTheme.green,
                      compact: true,
                      // Always non-null: a marker that vanished at zero would
                      // remove its reserved row and jump the layout. At zero
                      // the triangle parks at the origin instead.
                      markerTop: maxFwd / 1200,
                      markerBottom: p95Fwd / 1200,
                      markerTopColor: AppTheme.txt,
                      markerBottomColor: AppTheme.accent,
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
                      value: swr,
                      // SWR cannot go below 1.0 — the scale starts there, so
                      // a perfect match reads as an empty bar.
                      min: 1.0,
                      max: 4.0,
                      unit: 'SWR',
                      ticks: const [(1.0, '1.0'), (1.5, '1.5'), (3.0, '3.0'), (4.0, '4.0')],
                      fillColor: AppTheme.amber,
                      compact: true,
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

/// One forward-power sample with its arrival time.
class _FwdSample {
  DateTime t;
  final double v;
  _FwdSample(this.t, this.v);
}

/// Linear-interpolation percentile (numpy default) over an already-sorted list.
double _percentile(List<double> sorted, double p) {
  if (sorted.isEmpty) return 0;
  if (sorted.length == 1) return sorted.first;
  final idx = p * (sorted.length - 1);
  final lower = idx.floor();
  final upper = idx.ceil();
  if (lower == upper) return sorted[lower];
  final frac = idx - lower;
  return sorted[lower] + frac * (sorted[upper] - sorted[lower]);
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

  /// Optional peak/percentile markers, as fractions of the scale (0..1). A non-null
  /// [markerTop] draws a downward triangle above the bar; [markerBottom] draws
  /// an upward triangle below it. Marker rows are reserved only while a
  /// marker is non-null, so a meter whose markers toggle to null at zero
  /// would jump its layout — pass 0 there and the marker parks at the origin.
  final double? markerTop;
  final double? markerBottom;
  final Color? markerTopColor;
  final Color? markerBottomColor;

  const _Meter({
    required this.value,
    this.min = 0,
    required this.max,
    required this.unit,
    required this.ticks,
    required this.fillColor,
    this.compact = false,
    this.markerTop,
    this.markerBottom,
    this.markerTopColor,
    this.markerBottomColor,
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

    final hasMarkers = markerTop != null || markerBottom != null;
    final markerSpace = hasMarkers ? _markerSize + _markerGap : 0.0;
    final stackHeight = barHeight + 2 * markerSpace;

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
                  if (markerTop != null)
                    Positioned(
                      top: 0,
                      left: _markerLeft(markerTop!, w),
                      child: _TriangleMarker(key: const ValueKey('pa-fwd-peak'), color: markerTopColor ?? AppTheme.txt, pointDown: true, size: _markerSize),
                    ),
                  if (markerBottom != null)
                    Positioned(
                      bottom: 0,
                      left: _markerLeft(markerBottom!, w),
                      child: _TriangleMarker(key: const ValueKey('pa-fwd-p95'), color: markerBottomColor ?? AppTheme.accent, pointDown: false, size: _markerSize),
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
