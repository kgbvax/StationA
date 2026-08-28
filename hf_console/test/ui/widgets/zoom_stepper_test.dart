// zoom_stepper_test.dart — widget-level tests for the +/- compass zoom
// buttons rendered by `CompassPanel`. Verifies the tap callback reaches the
// state (via the zoom badge in the header) and that the +/- icons disable
// at the zoom bounds. Drives the real `CompassPanel` through the project's
// `TestHarness` so we exercise the actual `_ZoomStepper` widget + its
// `onZoomChanged` plumbing.

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/compass_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('CompassPanel zoom buttons', () {
    testWidgets('+ tap increases the zoom badge by 0.2', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      // Default zoom is 1.5×, displayed as "1.50×".
      expect(find.text('1.50×'), findsOneWidget);

      await tester.tap(find.byIcon(Icons.add));
      await tester.pumpAndSettle();

      expect(find.text('1.70×'), findsOneWidget);
    });

    testWidgets('- tap decreases the zoom badge by 0.2', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      expect(find.text('1.50×'), findsOneWidget);

      await tester.tap(find.byIcon(Icons.remove));
      await tester.pumpAndSettle();

      expect(find.text('1.30×'), findsOneWidget);
    });

    testWidgets('- disables at the minimum zoom', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      // Tap - four times from 1.5× to reach the floor at 1.0×.
      for (var i = 0; i < 4; i++) {
        await tester.tap(find.byIcon(Icons.remove));
        await tester.pumpAndSettle();
      }

      expect(find.text('1.00×'), findsOneWidget);
      // Further taps must be no-ops — the button is `onPressed: null` so
      // tap fires nothing.
      await tester.tap(find.byIcon(Icons.remove), warnIfMissed: false);
      await tester.pumpAndSettle();
      expect(find.text('1.00×'), findsOneWidget);
    });

    testWidgets('+ disables at the maximum zoom', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      // Tap + 18 times from 1.5× to reach the ceiling at 5.0× (1.5 + 18×0.2 = 5.1 → clamped to 5.0).
      for (var i = 0; i < 20; i++) {
        await tester.tap(find.byIcon(Icons.add));
        await tester.pumpAndSettle();
      }

      expect(find.text('5.00×'), findsOneWidget);
      await tester.tap(find.byIcon(Icons.add), warnIfMissed: false);
      await tester.pumpAndSettle();
      expect(find.text('5.00×'), findsOneWidget);
    });
  });
}