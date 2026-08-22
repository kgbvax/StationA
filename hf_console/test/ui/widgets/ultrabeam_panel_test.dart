import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/ultrabeam_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('UltrabeamPanel', () {
    testWidgets('shows direction and publishes forward command', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUltrabeam(direction: 'bidirectional');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();

      expect(find.text('BIDIRECTIONAL'), findsOneWidget);

      await tester.tap(find.widgetWithText(ElevatedButton, 'FORWARD'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/ant-ctrl/cmd');
      expect(mqtt.publishes.first.payload, contains('direction'));
      expect(mqtt.publishes.first.payload, contains('forward'));
    });

    testWidgets('shows MOVING in red while moving', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUltrabeam(direction: 'forward', moving: true);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();

      expect(find.text('MOVING'), findsOneWidget);
    });
  });
}
