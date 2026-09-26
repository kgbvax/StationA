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

      expect(find.textContaining('OFFLINE'), findsOneWidget);
      expect(find.text('OPERATE'), findsOneWidget);
      expect(find.widgetWithText(ElevatedButton, 'OPERATE'), findsOneWidget);
    });

    testWidgets('shows plain device name when healthy', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaHealthy();

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pumpAndSettle();

      // Regular operate/standby shows just the pill name — no suffix.
      expect(find.text('ACOM 1200S'), findsOneWidget);
      expect(find.textContaining('OFFLINE'), findsNothing);
    });

    testWidgets('shows human-readable PA error in tag', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaFault(error: 'HOT SWITCHING ATTEMPT');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pumpAndSettle();

      expect(find.textContaining('HOT SWITCHING ATTEMPT'), findsOneWidget);
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

    testWidgets('draws one held peak marker per meter', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaTransmitting(fwd: 800, rfl: 20);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pump();

      // One peak triangle above each bar; the old percentile marker is gone.
      expect(find.byKey(const ValueKey('pa-fwd-peak')), findsOneWidget);
      expect(find.byKey(const ValueKey('pa-swr-peak')), findsOneWidget);
      expect(find.byKey(const ValueKey('pa-fwd-p95')), findsNothing);
    });

    testWidgets('shows PA RELAY OFF when the remote-on relay is open', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaHealthy();
      // Relay off: hf/switch has the PA relay de-energized.
      store.applyState('muehle/hf/switch', {'pa': 'off', 'trx': 'on', 'device_online': true});
      // Amp's own power telemetry reports OFF (powered-down at the device).
      store.applyState('muehle/hf/pa', {'power': 'off'});
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pumpAndSettle();

      expect(find.textContaining('PA RELAY OFF'), findsOneWidget);
    });

    testWidgets('shows RELAY ? when the hf/switch state is unknown, not a fabricated OFF', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaHealthy();
      // Strip the switch slot entirely: silence must not become an
      // affirmative open-relay claim.
      store.apply('muehle/hf/switch/status', '', true);
      store.apply('muehle/hf/switch/state', '', true);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pumpAndSettle();

      expect(find.textContaining('RELAY ?'), findsOneWidget);
      expect(find.textContaining('PA RELAY OFF'), findsNothing);
    });

    testWidgets('bar releases and peak marker holds, then decays slowly', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaTransmitting(fwd: 800, rfl: 20);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pump();

      final peak = find.byKey(const ValueKey('pa-fwd-peak'));
      double peakX() => tester.getTopLeft(peak).dx;
      final atPeak = peakX();

      // Unkey: live power drops to zero.
      store.applyState('muehle/hf/pa', _rx);
      await tester.pump();

      // The readout (and bar) release instead of snapping to zero.
      expect(find.text('0.0 W FWD'), findsNothing);

      // Held at the peak for 2 s…
      await tester.pump(const Duration(milliseconds: 1500));
      expect(peakX(), atPeak);
      // …by then the bar has released to zero.
      await tester.pump(const Duration(milliseconds: 1500));
      expect(find.text('0.0 W FWD'), findsOneWidget);

      // …then drains slowly (1200 W / 10 s): partway down at 4 s.
      await tester.pump(const Duration(seconds: 1));
      final mid = peakX();
      expect(mid, lessThan(atPeak));

      // 800 W is gone by ~9 s; at zero the marker parks at the origin
      // instead of disappearing — removing it would jump the layout.
      await tester.pump(const Duration(seconds: 6));
      expect(peakX(), lessThan(mid));
      expect(peak, findsOneWidget);
      // Stack keeps its height at zero: compact bar 8 + one 7 px marker row + 1 px gap.
      final stackSize = tester.getSize(find.byKey(const ValueKey('pa-meter-stack')).first);
      expect(stackSize.height, 8.0 + 7.0 + 1.0);
    });

    testWidgets('bar and peak rise instantly', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPaTransmitting(fwd: 200, rfl: 5);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const PaPanel()));
      await tester.pump();
      final low = tester.getTopLeft(find.byKey(const ValueKey('pa-fwd-peak'))).dx;

      store.applyState('muehle/hf/pa', {'fwd_power_w': 800});
      await tester.pump();

      expect(find.text('800 W FWD'), findsOneWidget);
      expect(tester.getTopLeft(find.byKey(const ValueKey('pa-fwd-peak'))).dx, greaterThan(low));
    });
  });
}

const _rx = {
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
};
