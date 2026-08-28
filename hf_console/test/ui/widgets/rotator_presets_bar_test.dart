// rotator_presets_bar_test.dart — widget tests for the direction-preset bar
// (NA / SA / VK / JA / STOP) extracted from the compass card. The bar lives
// in its own panel below the antenna panel so the compass disc can use the
// full card height. Same MQTT publish path the compass used — these tests
// guard the move didn't drop the publish logic or the offline-gating.

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
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
}
