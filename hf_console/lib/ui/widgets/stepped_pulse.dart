import 'dart:async';

import 'package:flutter/animation.dart';
import 'package:flutter/foundation.dart';

/// A repeating 0 → 1 → 0 swing, sampled by a [Timer] instead of on every
/// vsync.
///
/// It replaces `AnimationController.repeat(reverse: true)` for the slow amber
/// highlights. A vsync controller makes the engine draw a full frame at the
/// display rate (60/120 fps) for as long as the highlight is on, and the
/// highlights stay on for hours (PA STANDBY is the ACOM's resting state). On
/// the web build (CanvasKit, no partial repaint) every frame redraws the
/// whole canvas: the 60 fps pulse alone took about 30 % CPU on an idle
/// console.
///
/// The swing samples every [step] (about 11 fps) for the first [settleAfter],
/// while the state is new and the eye is on it, then every [slowStep]
/// (8 fps) for as long as it lasts. The phase carries over the change.
///
/// The phase counts timer ticks, not wall time, so widget tests drive it
/// deterministically with `tester.pump(duration)`. Value at start is 0 (the
/// dim end), as with a controller that repeats from its lower bound.
class SteppedPulse extends ChangeNotifier implements ValueListenable<double> {
  SteppedPulse({
    required this.halfPeriod,
    this.step = const Duration(milliseconds: 90),
    this.slowStep = const Duration(milliseconds: 125),
    this.settleAfter = const Duration(seconds: 10),
    this.curve = Curves.easeInOut,
  });

  /// Time from one end of the swing to the other.
  final Duration halfPeriod;

  /// Sampling interval while the swing is new. Each tick is one rebuild (and
  /// one frame).
  final Duration step;

  /// Sampling interval once the swing has run for [settleAfter].
  final Duration slowStep;

  /// How long the swing samples at [step] before it drops to [slowStep].
  final Duration settleAfter;

  /// Applied to the linear triangle wave.
  final Curve curve;

  Timer? _timer;
  int _elapsedUs = 0;
  double _value = 1.0;

  @override
  double get value => _value;

  bool get isRunning => _timer != null;

  /// The sampling interval in use now ([step], then [slowStep]); null when
  /// stopped.
  Duration? get currentStep => _timer == null ? null : (_slow ? slowStep : step);

  bool get _slow => _elapsedUs >= settleAfter.inMicroseconds;

  /// Starts the swing from the dim end at the fast rate. No-op while running.
  void start() {
    if (_timer != null) return;
    _elapsedUs = 0;
    _value = _sample();
    _schedule(step);
  }

  void _schedule(Duration interval) {
    _timer = Timer.periodic(interval, (t) {
      _elapsedUs += interval.inMicroseconds;
      final v = _sample();
      if (v != _value) {
        _value = v;
        notifyListeners();
      }
      if (interval != slowStep && _slow) {
        t.cancel();
        _schedule(slowStep);
      }
    });
  }

  /// Stops the swing and holds [hold] (1.0 = the full end). Does not notify:
  /// callers stop from build, where the next build reads [value] anyway.
  void stop({double hold = 1.0}) {
    _timer?.cancel();
    _timer = null;
    _value = hold;
  }

  double _sample() {
    final half = halfPeriod.inMicroseconds;
    final phase = _elapsedUs % (2 * half);
    final tri = phase <= half ? phase / half : 2 - phase / half;
    return curve.transform(tri.clamp(0.0, 1.0));
  }

  @override
  void dispose() {
    _timer?.cancel();
    _timer = null;
    super.dispose();
  }
}
