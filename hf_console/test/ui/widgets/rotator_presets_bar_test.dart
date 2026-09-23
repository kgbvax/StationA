// rotator_presets_bar_test.dart — widget tests for the rotator STOP button.
// (The NA / SA / VK / JA region presets were removed; these tests pin their
// absence so they don't creep back in.) The horizontal `RotatorPresetsBar`
// lives in the phone scroll column; the vertical `RotatorPresetsRail`
// overlays the DX map's right edge above the zoom controls (tablet). Both
// share `_presetActions` — these tests guard the publish logic and the
// offline-gating, plus the rail/stepper geometry (an earlier layout put the
// rail low enough to cover the + button).

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/compass_panel.dart';
import 'package:hf_console/ui/widgets/rotator_presets_bar.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('RotatorPresetsBar', () {
    testWidgets('region presets (NA / SA / VK / JA) are gone', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const RotatorPresetsBar()));
      await tester.pumpAndSettle();

      expect(find.text('NA 330'), findsNothing);
      expect(find.text('SA 210'), findsNothing);
      expect(find.text('VK 60'), findsNothing);
      expect(find.text('JA 35'), findsNothing);
      expect(find.text('STOP'), findsOneWidget);
    });

    testWidgets('does not publish STOP when offline', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const RotatorPresetsBar()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'STOP'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('STOP publishes a stop action with retain=false', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 45.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const RotatorPresetsBar()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'STOP'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/rotator/cmd');
      expect(mqtt.publishes.first.payload, contains('stop'));
      expect(mqtt.publishes.first.retain, isFalse);
    });
  });

  group('RotatorPresetsRail (inside CompassPanel)', () {
    testWidgets('rail shows only STOP, no region presets', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      expect(find.text('NA 330'), findsNothing);
      expect(find.text('STOP'), findsOneWidget);
    });

    testWidgets('rail clears the +/- zoom stepper', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      // The rail's bottom button must sit entirely above the + button —
      // regression guard for the overlay geometry.
      final stop = tester.getRect(find.widgetWithText(ElevatedButton, 'STOP'));
      final add = tester.getRect(find.byIcon(Icons.add));
      expect(stop.bottom, lessThanOrEqualTo(add.top));
    });

    testWidgets('hidden when showPresets is false (phone layout)', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0);

      await tester.pumpWidget(
          TestHarness(store: store, mqtt: mqtt, child: const CompassPanel(showPresets: false)));
      await tester.pumpAndSettle();

      expect(find.text('STOP'), findsNothing);
    });
  });
}
