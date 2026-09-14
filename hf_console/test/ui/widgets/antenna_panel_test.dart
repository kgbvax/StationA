import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/antenna_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

// Resolve the effective background color of a labelled action button.
Color buttonBg(WidgetTester tester, String label) {
  final btn = tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, label));
  return btn.style!.backgroundColor!.resolve({})!;
}

void main() {
  group('AntennaPanel', () {
    testWidgets('shows selected port and mode', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setAntenna(selected: 'port4', settled: true, mode: 'auto');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      // port2/port3 are not wired at Mühle and must not appear; port5 is
      // not offered in the console either.
      expect(find.textContaining('PORT 2'), findsNothing);
      expect(find.textContaining('PORT 3'), findsNothing);
      expect(find.textContaining('PORT 5'), findsNothing);
    });

    testWidgets('publishes antenna-select request in auto mode', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setAntenna(selected: 'off', mode: 'auto');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      // port4's map label is 'Ultrabeam'; the button carries the label.
      await tester.tap(find.widgetWithText(ElevatedButton, 'ULTRABEAM'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/antenna-select/cmd');
      expect(mqtt.publishes.first.payload, contains('port4'));
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
      // Direct-drive requires confirmed RX (fail-closed hot-switch guard).
      store.setRadio();

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
      // Direct-drive requires confirmed RX (fail-closed hot-switch guard).
      store.setRadio();

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      await tester.tap(find.widgetWithText(ElevatedButton, 'DUMMY'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/ant-switch/cmd');
      expect(mqtt.publishes.first.payload, contains('port1'));
    });

    testWidgets('grounded state renders the GROUNDED button in solid red', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setAntenna(selected: 'off', settled: true, mode: 'auto');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      expect(buttonBg(tester, 'GROUNDED'), AppTheme.red);
      // The other ports stay chrome-coloured.
      expect(buttonBg(tester, 'ULTRABEAM'), isNot(AppTheme.red));
    });

    testWidgets('non-grounded selection does not render red', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setAntenna(selected: 'port4', settled: true, mode: 'auto');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      expect(buttonBg(tester, 'ULTRABEAM'), isNot(AppTheme.red));
      expect(buttonBg(tester, 'GROUNDED'), isNot(AppTheme.red));
    });

    testWidgets('manual mode renders the MANUAL button in solid red', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setAntenna(selected: 'port4', settled: true, mode: 'manual');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      expect(buttonBg(tester, 'MANUAL'), AppTheme.red);
      expect(buttonBg(tester, 'AUTO'), isNot(AppTheme.red));
    });

    group('cold-switch guard (fail closed)', () {
      // Manual mode = direct drive; the guard applies only there (with the
      // reconciler online it arbitrates the RF-inhibit ordering itself).
      Future<BusStore> seedManual(BusStore store) async {
        store.setAntenna(selected: 'port4', settled: true, mode: 'manual');
        return store;
      }

      testWidgets('blocks direct taps while the radio reports TX', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        await seedManual(store);
        store.setRadio(tx: 'tx');

        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
        await tester.pumpAndSettle();

        expect(find.text('RF ON'), findsNothing);
        expect(
          tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'ULTRABEAM')).onPressed,
          isNull,
        );
        await tester.tap(find.widgetWithText(ElevatedButton, 'ULTRABEAM'), warnIfMissed: false);
        await tester.pumpAndSettle();
        expect(mqtt.publishes, isEmpty);
      });

      testWidgets('blocks direct taps on a tune carrier without the tx bit', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        await seedManual(store);
        store.setRadio(tuning: true);

        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
        await tester.pumpAndSettle();

        await tester.tap(find.widgetWithText(ElevatedButton, 'ULTRABEAM'), warnIfMissed: false);
        await tester.pumpAndSettle();
        expect(mqtt.publishes, isEmpty);
      });

      testWidgets('blocks direct taps when the PA reports keyed', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        await seedManual(store);
        store.setRadio();
        store.setOnline('muehle/hf/pa');
        store.applyState('muehle/hf/pa', {'keyed': 'tx', 'device_online': true});

        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
        await tester.pumpAndSettle();

        await tester.tap(find.widgetWithText(ElevatedButton, 'ULTRABEAM'), warnIfMissed: false);
        await tester.pumpAndSettle();
        expect(mqtt.publishes, isEmpty);
      });

      testWidgets('blocks direct taps when the radio state is unknown — fail closed', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        await seedManual(store);
        // No muehle/hf/radio state at all: unknown must mean block, not rx.

        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
        await tester.pumpAndSettle();

        expect(find.text('RF ?'), findsNothing);
        expect(
          tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'ULTRABEAM')).onPressed,
          isNull,
        );
        await tester.tap(find.widgetWithText(ElevatedButton, 'ULTRABEAM'), warnIfMissed: false);
        await tester.pumpAndSettle();
        expect(mqtt.publishes, isEmpty);
      });

      testWidgets('allows direct taps with confirmed RX — the guard is exclusive, not blanket', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        await seedManual(store);
        store.setRadio(); // online, tx rx

        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
        await tester.pumpAndSettle();

        await tester.tap(find.widgetWithText(ElevatedButton, 'ULTRABEAM'));
        await tester.pumpAndSettle();
        expect(mqtt.publishes.length, 1);
        expect(mqtt.publishes.first.topic, 'muehle/hf/ant-switch/cmd');
      });
    });

    testWidgets('unknown ant-switch state renders Unknown, not red Grounded', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      // Bridge dead since before connect: no retained /state at all.
      store.setBridgeOffline('muehle/hf/ant-switch');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      // No header anymore: an unknown state must not paint Grounded red.
      expect(find.textContaining('Grounded'), findsNothing);
      expect(buttonBg(tester, 'GROUNDED'), isNot(AppTheme.red));
    });
  });
}
