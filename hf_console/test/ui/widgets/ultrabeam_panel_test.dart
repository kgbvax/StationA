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

    testWidgets('on 6m, forces a non-forward direction back to forward', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRadio(band: '6m');
      store.setUltrabeam(direction: 'reverse');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/ant-ctrl/cmd');
      expect(mqtt.publishes.first.payload, contains('direction'));
      expect(mqtt.publishes.first.payload, contains('forward'));
    });

    testWidgets('on 6m, forces bi-dir back to forward', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRadio(band: '6m');
      store.setUltrabeam(direction: 'bidirectional');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/ant-ctrl/cmd');
      expect(mqtt.publishes.first.payload, contains('forward'));
    });

    testWidgets('on 6m, 180° and BI-DIR buttons are disabled', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRadio(band: '6m');
      store.setUltrabeam(direction: 'forward');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();

      expect(
        tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'FORWARD')).onPressed,
        isNotNull,
      );
      expect(
        tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, '180°')).onPressed,
        isNull,
      );
      expect(
        tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'BI-DIR')).onPressed,
        isNull,
      );
      // No forced-forward publish fires when the direction is already forward.
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('off 6m, 180° and BI-DIR buttons stay enabled', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRadio(band: '20m');
      store.setUltrabeam(direction: 'forward');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();

      expect(
        tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, '180°')).onPressed,
        isNotNull,
      );
      expect(
        tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'BI-DIR')).onPressed,
        isNotNull,
      );
    });

    testWidgets('RETRACT stays pressable while moving — it is the emergency action', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUltrabeam(direction: 'reverse', moving: true);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();

      final retract = tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'RETRACT'));
      expect(retract.onPressed, isNotNull);
      // Direction changes stay locked, matching ultrabridge's web UI.
      expect(tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'FORWARD')).onPressed, isNull);

      await tester.tap(find.widgetWithText(ElevatedButton, 'RETRACT'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.payload, contains('retract'));
    });

    group('band mismatch', () {
      testWidgets('flags a real ham-band divergence in red', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        store.setRadio(band: '20m');
        store.setUltrabeam(band: '40m');

        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
        await tester.pumpAndSettle();

        expect(find.text('BAND MISMATCH · 40m ≠ 20m'), findsOneWidget);
      });

      testWidgets('does not flag agreed non-ham frequencies', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        // Same frequency, different labels between bridges — that is not a
        // mismatch, and crying wolf here desensitizes the real warning.
        store.setRadio(freqHz: 9950000, band: 'gen');
        store.setUltrabeam(band: 'band-2');

        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
        await tester.pumpAndSettle();

        expect(find.textContaining('BAND MISMATCH'), findsNothing);
        // Direction pill and FORWARD button render the same text.
        expect(find.text('FORWARD'), findsNWidgets(2));
      });

      testWidgets('does not flag a pre-first-state controller (unknown ≠ mismatch)', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        store.setRadio(band: '20m');
        // No ant-ctrl state at all: ctrlBand ''.

        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
        await tester.pumpAndSettle();

        expect(find.textContaining('BAND MISMATCH'), findsNothing);
      });
    });
  });
}
