import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/power_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('PowerPanel', () {
    testWidgets('shows relay states and publishes toggle commands', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower(master: true, psu: false, trx: true, pa: false);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PowerPanel()));
      await tester.pumpAndSettle();

      expect(find.text('MAINS'), findsOneWidget);
      expect(find.text('PSU 13.8V'), findsOneWidget);
      expect(find.text('TRX'), findsOneWidget);
      expect(find.text('PA'), findsOneWidget);

      await tester.tap(find.widgetWithText(ElevatedButton, 'STOP\nSTATION'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/power-seq/cmd');
      expect(mqtt.publishes.first.payload, contains('stop'));
    });

    testWidgets('START STATION is disabled while the sequence runs', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower(); // seeds power-seq phase 'running'

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PowerPanel()));
      await tester.pumpAndSettle();

      final start = tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'START\nSTATION'));
      expect(start.onPressed, isNull);
      await tester.tap(find.widgetWithText(ElevatedButton, 'START\nSTATION'), warnIfMissed: false);
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('START STATION publishes the start cmd when idle', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPower();
      store.applyState('muehle/hf/power-seq', {'phase': 'idle', 'fault': '', 'device_online': true});

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PowerPanel()));
      await tester.pumpAndSettle();

      final start = tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'START\nSTATION'));
      expect(start.onPressed, isNotNull);
      await tester.tap(find.widgetWithText(ElevatedButton, 'START\nSTATION'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/power-seq/cmd');
      expect(mqtt.publishes.first.payload, contains('start'));
    });
  });
}
