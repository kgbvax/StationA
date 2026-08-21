import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/pa_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('PaPanel', () {
    testWidgets('shows OFFLINE when bridge is down', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pumpAndSettle();

      expect(find.text('OFFLINE'), findsOneWidget);
      expect(find.text('OPERATE'), findsOneWidget);
      expect(find.widgetWithText(ElevatedButton, 'OPERATE'), findsOneWidget);
    });

    testWidgets('shows OPERATE in green when healthy', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaHealthy();

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pumpAndSettle();

      // Tag shows OPERATE with temperature; button also says OPERATE.
      expect(find.textContaining('OPERATE ·'), findsOneWidget);
      expect(find.text('OFFLINE'), findsNothing);
    });

    testWidgets('shows human-readable PA error in tag', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaFault(error: 'HOT SWITCHING ATTEMPT');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pumpAndSettle();

      expect(find.text('HOT SWITCHING ATTEMPT'), findsOneWidget);
    });

    testWidgets('publishes set_mode operate on OPERATE tap', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaHealthy();

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'OPERATE'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/pa/cmd');
      expect(mqtt.publishes.first.payload, contains('set_mode'));
      expect(mqtt.publishes.first.payload, contains('operate'));
      expect(mqtt.publishes.first.retain, isFalse);
    });

    testWidgets('does not publish when offline', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'OPERATE'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes, isEmpty);
    });
  });
}
