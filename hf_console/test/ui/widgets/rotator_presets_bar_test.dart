// rotator_presets_bar_test.dart — widget tests for the direction presets
// (NA / SA / VK / JA / STOP). The horizontal `RotatorPresetsBar` lives in the
// phone scroll column; the vertical `RotatorPresetsRail` overlays the DX
// map's right edge above the zoom controls (tablet). Both share
// `_presetActions` — these tests guard the publish logic and the
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
    testWidgets('publishes set_az on preset button tap', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const RotatorPresetsBar()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'NA 330'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/rotator/cmd');
      expect(mqtt.publishes.first.payload, contains('set_az'));
      expect(mqtt.publishes.first.payload, contains('330'));
    });

    testWidgets('does not publish presets when offline', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const RotatorPresetsBar()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'NA 330'));
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
    testWidgets('publishes set_az on preset button tap', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'NA 330'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/rotator/cmd');
      expect(mqtt.publishes.first.payload, contains('set_az'));
      expect(mqtt.publishes.first.payload, contains('330'));
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

      expect(find.text('NA 330'), findsNothing);
    });
  });
}
