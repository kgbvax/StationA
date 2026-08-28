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

    testWidgets('never shows reflected-power readout', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaTransmitting(fwd: 800, rfl: 20);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pumpAndSettle();

      // Reflected power is not displayed at all — neither while transmitting…
      expect(find.textContaining('REFL'), findsNothing);

      // …nor while receiving.
      store.applyState('muehle/hf/pa', {
        'mode': 'operate',
        'keyed': 'rx',
        'fault': 'none',
        'error': '',
        'temp_c': 38.5,
        'fwd_power_w': 0,
        'rfl_power_w': 20,
        'swr': 1.1,
        'pa_state': 'OPR/RX',
        'power': 'on',
        'device_online': true,
        'ts': '2026-08-20T14:30:00.000000',
      });
      // pump, not pumpAndSettle: the peak-hold decay timer is still running.
      await tester.pump();

      expect(find.textContaining('REFL'), findsNothing);
    });

    testWidgets('draws peak and percentile markers while transmitting', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaTransmitting(fwd: 800, rfl: 20);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pumpAndSettle();

      // Two triangle markers: peak above the bar, 95th-percentile below it.
      expect(find.byKey(const ValueKey('pa-fwd-peak')), findsOneWidget);
      expect(find.byKey(const ValueKey('pa-fwd-p95')), findsOneWidget);
    });

    testWidgets('peak markers decay slowly after unkeying', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaTransmitting(fwd: 800, rfl: 20);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pumpAndSettle();

      // Unkey: live power drops to zero, but the held peak marker must not.
      store.applyState('muehle/hf/pa', {
        'mode': 'operate',
        'keyed': 'rx',
        'fault': 'none',
        'error': '',
        'temp_c': 38.5,
        'fwd_power_w': 0,
        'rfl_power_w': 20,
        'swr': 1.1,
        'pa_state': 'OPR/RX',
        'power': 'on',
        'device_online': true,
        'ts': '2026-08-20T14:30:00.000000',
      });
      await tester.pump();

      // ~2 s after unkeying the marker is still on the meter, partway down
      // (800 W drains at 1200 W / 5 s = 240 W per second).
      await tester.pump(const Duration(seconds: 2));
      expect(find.byKey(const ValueKey('pa-fwd-peak')), findsOneWidget);

      // A full-scale peak would take ~5 s; 800 W is gone well before that.
      await tester.pump(const Duration(seconds: 4));
      expect(find.byKey(const ValueKey('pa-fwd-peak')), findsNothing);
      expect(find.byKey(const ValueKey('pa-fwd-p95')), findsNothing);
    });
  });
}
