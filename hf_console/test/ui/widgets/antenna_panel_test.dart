import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/antenna_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('AntennaPanel', () {
    testWidgets('shows selected port and mode', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setAntenna(selected: 'port4', settled: true, mode: 'auto');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      expect(find.textContaining('AUTO · Ultrabeam'), findsOneWidget);
      // port2/port3 are not wired at Mühle and must not appear.
      expect(find.textContaining('PORT 2'), findsNothing);
      expect(find.textContaining('PORT 3'), findsNothing);
    });

    testWidgets('publishes antenna-select request in auto mode', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setAntenna(selected: 'off', mode: 'auto');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'PORT 5'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/antenna-select/cmd');
      expect(mqtt.publishes.first.payload, contains('port5'));
    });

    testWidgets('switching to manual publishes manual mode', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setAntenna(selected: 'off', mode: 'auto');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'MANUAL'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/antenna-select/cmd');
      expect(mqtt.publishes.first.payload, contains('manual'));
    });

    testWidgets('in manual mode port buttons drive ant-switch directly', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setAntenna(selected: 'off', mode: 'manual');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'FAN DIPOLE 80/40'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/ant-switch/cmd');
      expect(mqtt.publishes.first.payload, contains('port6'));
    });

    testWidgets('publishes ant-switch directly when selector is offline', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setOnline('muehle/hf/ant-switch');
      store.applyState('muehle/hf/ant-switch', {
        'selected': 'off',
        'settled': true,
        'device_online': true,
      });
      store.setBridgeOffline('muehle/hf/antenna-select');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'DUMMY LOAD'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/ant-switch/cmd');
      expect(mqtt.publishes.first.payload, contains('port1'));
    });
  });
}
