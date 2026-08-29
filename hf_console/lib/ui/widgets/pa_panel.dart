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
  static const double _peakDecayPerTick = _meterFullScale / 50; // W per 100 ms
  static const Duration _decayInterval = Duration(milliseconds: 100);

  double _lastFwd = 0;
  double _peakHold = 0;
  double _p95Hold = 0;
  Timer? _decayTimer;

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
      _decayTimer ??= Timer.periodic(_decayInterval, (_) => _decayTick());
    } else {
      _decayTimer?.cancel();
      _decayTimer = null;
    }
  }

  void _decayTick() {
    if (!mounted) return;
    setState(() {
      // Decay toward the live reading, never below it — a new transmission
      // takes the marker over immediately.
      _peakHold = math.max(_lastFwd, _peakHold - _peakDecayPerTick);
      _p95Hold = math.max(_lastFwd, _p95Hold - _peakDecayPerTick);
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
    final online = slot?.isOnline ?? false;
    final mode = store.stateValueAs<String>('muehle/hf/pa', 'mode') ?? 'standby';
    final keyed = store.stateValueAs<String>('muehle/hf/pa', 'keyed') ?? 'rx';
    final fault = store.stateValueAs<String>('muehle/hf/pa', 'fault') ?? 'none';
    final error = store.stateValueAs<String>('muehle/hf/pa', 'error') ?? '';
    final temp = store.stateValueAs<num>('muehle/hf/pa', 'temp_c')?.toDouble() ?? 0.0;
    final fwd = store.stateValueAs<num>('muehle/hf/pa', 'fwd_power_w')?.toDouble() ?? 0.0;
    final swr = store.stateValueAs<num>('muehle/hf/pa', 'swr')?.toDouble() ?? 1.0;

    // Cross-links: the PA remote-on relay (hf/switch) and the amp's own
    // power telemetry both gate operation but live on other slots — without
    // them a dead amp presents as a healthy standby PA.
    final paRelayOn = (store.stateValueAs<String>('muehle/hf/switch', 'pa') ?? 'off') == 'on';
    final paPower = store.stateValueAs<String>('muehle/hf/pa', 'power') ?? '';

    _lastFwd = fwd;
    _recordFwd(fwd);
    final maxFwd = _peakHold;
    final p95Fwd = _p95Hold;

    final (tagLabel, tagColor) = _paTag(mode, keyed, fault, error, temp, paRelayOn, paPower, online);

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
            trailing: _Tag(tagLabel, tagColor),
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
                      labels: const ['0', '500', '1000', '1200'],
                      fillColor: AppTheme.green,
                      compact: true,
                      markerTop: maxFwd > 0 ? maxFwd / 1200 : null,
                      markerBottom: p95Fwd > 0 ? p95Fwd / 1200 : null,
                      markerTopColor: AppTheme.txt,
                      markerBottomColor: AppTheme.accent,
                    ),
                  ),
                  const SizedBox(width: 10),
                  SizedBox(
                    width: 96,
                    height: 34,
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
                      max: 4.0,
                      unit: 'SWR',
                      labels: const ['1.0', '1.5', '3.0', '4.0'],
                      fillColor: AppTheme.amber,
                      compact: true,
                    ),
                  ),
                  const SizedBox(width: 10),
                  SizedBox(
                    width: 96,
                    height: 34,
                    child: ElevatedButton(
                      onPressed: online ? () => setMode('standby') : null,
                      style: AppTheme.actionButton(amber: true, active: mode == 'standby'),
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

  (String, Color) _paTag(String mode, String keyed, String fault, String error, double temp, bool paRelayOn, String paPower, bool online) {
    if (!online) return ('OFFLINE', AppTheme.txtMute);
    if (fault.isNotEmpty && fault != 'none') {
      final label = error.isNotEmpty ? error.toUpperCase() : fault.toUpperCase();
      return (label, AppTheme.red);
    }
    if (!paRelayOn) return ('PA RELAY OFF', AppTheme.amber);
    if (paPower == 'off') return ('PA OFF', AppTheme.amber);
    if (keyed == 'tx') return ('● TX', AppTheme.red);
    if (keyed == 'inhibited') return ('INHIBITED', AppTheme.amber);
    final tempLabel = temp > 0 ? ' · ${temp.round()} °C' : '';
    if (mode == 'operate') return ('OPERATE$tempLabel', AppTheme.green);
    return ('STANDBY$tempLabel', AppTheme.amber);
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
  final double max;
  final String unit;
  final List<String> labels;
  final Color fillColor;
  final bool compact;

  /// Optional peak/percentile markers, as fractions of [max] (0..1). A non-null
  /// [markerTop] draws a downward triangle above the bar; [markerBottom] draws
  /// an upward triangle below it.
  final double? markerTop;
  final double? markerBottom;
  final Color? markerTopColor;
  final Color? markerBottomColor;

  const _Meter({
    required this.value,
    required this.max,
    required this.unit,
    required this.labels,
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
    final fraction = (value / max).clamp(0.0, 1.0);
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
        Row(
          mainAxisAlignment: MainAxisAlignment.spaceBetween,
          children: labels.map((l) => Text(l, style: labelStyle)).toList(),
        ),
      ],
    );
  }

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

class _Tag extends StatelessWidget {
  final String label;
  final Color color;

  const _Tag(this.label, this.color);

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 7, vertical: 2),
      decoration: BoxDecoration(
        color: AppTheme.blend(color, 0.12),
        border: Border.all(color: color),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(label, style: AppTheme.mono(11, color: color, weight: FontWeight.w700)),
    );
  }
}
