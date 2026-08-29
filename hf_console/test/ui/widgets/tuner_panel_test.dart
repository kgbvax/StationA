import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/tuner_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('TunerPanel', () {
    testWidgets('shows OFFLINE when bridge is down', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const TunerPanel()));
      await tester.pumpAndSettle();

      expect(find.text('OFFLINE'), findsOneWidget);
    });

    testWidgets('shows fault in red', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setTuner(inline: true, fault: 'high swr');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const TunerPanel()));
      await tester.pumpAndSettle();

      expect(find.text('HIGH SWR'), findsOneWidget);
    });

    testWidgets('publishes set_inline on BYPASS tap', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setTuner(inline: true);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const TunerPanel()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'BYPASS'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/tuner/cmd');
      expect(mqtt.publishes.first.payload, contains('set_inline'));
      expect(mqtt.publishes.first.payload, contains('false'));
      expect(mqtt.publishes.first.retain, isFalse);
    });

    testWidgets('locks tune taps while settling', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setTuner(settling: true);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const TunerPanel()));
      await tester.pumpAndSettle();

      expect(find.text('TUNING…'), findsNWidgets(2));
      final mem = tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'TUNING…').first);
      expect(mem.onPressed, isNull);

      await tester.tap(find.widgetWithText(ElevatedButton, 'TUNING…').first, warnIfMissed: false);
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);
    });
  });
}
