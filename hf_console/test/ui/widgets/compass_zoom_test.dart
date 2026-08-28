// compass_zoom_test.dart — pure-Dart tests for the compass zoom-step helpers
// extracted in `lib/ui/widgets/compass_panel.dart`. Mirrors the +/- button
// behavior in horstreporter (`static/app.js:1472-1488`, ±0.2 per click,
// clamp [1.0, 5.0]). No WidgetTester needed — these helpers are pure.

import 'package:flutter_test/flutter_test.dart';

import 'package:hf_console/ui/widgets/compass_panel.dart';

void main() {
  group('clampCompassZoom', () {
    test('clamps below the minimum', () {
      expect(clampCompassZoom(-0.5), kCompassZoomMin);
      expect(clampCompassZoom(0), kCompassZoomMin);
    });

    test('clamps above the maximum', () {
      expect(clampCompassZoom(5.5), kCompassZoomMax);
      expect(clampCompassZoom(100), kCompassZoomMax);
    });

    test('passes through values inside the range', () {
      expect(clampCompassZoom(2.5), 2.5);
    });

    test('NaN and infinity fall back to the default', () {
      expect(clampCompassZoom(double.nan), kCompassZoomDefault);
      expect(clampCompassZoom(double.infinity), kCompassZoomMax);
      expect(clampCompassZoom(double.negativeInfinity), kCompassZoomMin);
    });
  });

  group('zoomStep', () {
    test('adds the step when up is true', () {
      expect(zoomStep(1.5, up: true), 1.7);
      expect(zoomStep(1.0, up: true), 1.2);
    });

    test('subtracts the step when up is false', () {
      expect(zoomStep(1.5, up: false), 1.3);
      expect(zoomStep(5.0, up: false), 4.8);
    });

    test('clamps at the upper bound', () {
      expect(zoomStep(4.95, up: true), kCompassZoomMax);
      expect(zoomStep(kCompassZoomMax, up: true), kCompassZoomMax);
    });

    test('clamps at the lower bound', () {
      expect(zoomStep(1.05, up: false), kCompassZoomMin);
      expect(zoomStep(kCompassZoomMin, up: false), kCompassZoomMin);
    });

    test('uses 0.2 step size (matches horstreporter app.js:1472-1488)', () {
      expect(kCompassZoomStep, 0.2);
    });
  });

  group('canZoomIn / canZoomOut', () {
    test('canZoomIn is false at and above the maximum', () {
      expect(canZoomIn(kCompassZoomMax), isFalse);
      expect(canZoomIn(kCompassZoomMax + 1), isFalse);
    });

    test('canZoomIn is true below the maximum', () {
      expect(canZoomIn(kCompassZoomMax - 0.01), isTrue);
      expect(canZoomIn(kCompassZoomDefault), isTrue);
      expect(canZoomIn(kCompassZoomMin), isTrue);
    });

    test('canZoomOut is false at and below the minimum', () {
      expect(canZoomOut(kCompassZoomMin), isFalse);
      expect(canZoomOut(kCompassZoomMin - 1), isFalse);
    });

    test('canZoomOut is true above the minimum', () {
      expect(canZoomOut(kCompassZoomMin + 0.01), isTrue);
      expect(canZoomOut(kCompassZoomDefault), isTrue);
      expect(canZoomOut(kCompassZoomMax), isTrue);
    });

    test('at the default both buttons are enabled', () {
      expect(canZoomIn(kCompassZoomDefault), isTrue);
      expect(canZoomOut(kCompassZoomDefault), isTrue);
    });
  });
}