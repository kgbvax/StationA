import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/ultrabeam_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('UltrabeamPanel', () {
    testWidgets('shows the tuned band in the pill', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUltrabeam(direction: 'forward', band: '20m');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();

      expect(find.text('Ultrabeam · 20M'), findsOneWidget);
    });

    testWidgets('out-of-allocation band labels do not render as the band', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUltrabeam(direction: 'forward', band: 'band-2');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();

      expect(find.text('Ultrabeam'), findsOneWidget);
      expect(find.textContaining('·'), findsNothing);
    });

    testWidgets('shows direction and publishes forward command', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUltrabeam(direction: 'bidirectional');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();

      // Direction reads from the active (accent) button — there is no
      // status pill anymore.
      final bidir = tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'BI-DIR'));
      expect(bidir.style!.backgroundColor!.resolve({}), AppTheme.accent);

      await tester.tap(find.widgetWithText(ElevatedButton, 'FORWARD'));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/hf/ant-ctrl/cmd');
      expect(mqtt.publishes.first.payload, contains('direction'));
      expect(mqtt.publishes.first.payload, contains('forward'));
    });

    testWidgets('locks direction buttons while moving, RETRACT stays live', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUltrabeam(direction: 'forward', moving: true);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();

      // No status pill anymore — travel is signalled by the locked buttons.
      expect(find.text('MOVING'), findsNothing);
      expect(
        tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'FORWARD')).onPressed,
        isNull,
      );
      expect(
        tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'RETRACT')).onPressed,
        isNotNull,
      );
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

    testWidgets('engaged 180° renders amber (TestHarness freezes the pulse)', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUltrabeam(direction: 'reverse');

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();

      // Amber, not accent/red: reverse is irregular, not an error.
      final reverse = tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, '180°'));
      expect(reverse.style!.backgroundColor!.resolve({}), AppTheme.amber);
      // Normal directions keep the cyan accent highlight.
      final forward = tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'FORWARD'));
      expect(forward.style!.backgroundColor!.resolve({}), isNot(AppTheme.accent));
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

    group('AUTO (smart rotation) toggle', () {
      Future<void> pump(WidgetTester tester, BusStore store, FakeMqttService mqtt) async {
        await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
        await tester.pumpAndSettle();
      }

      ElevatedButton smart(WidgetTester tester) =>
          tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, 'AUTO'));

      testWidgets('enables beamsteer with a retained cmd', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        store.setUltrabeam();
        store.setOnline('muehle/hf/beam-steer');
        store.applyState('muehle/hf/beam-steer', {'enabled': false});
        await pump(tester, store, mqtt);

        await tester.tap(find.widgetWithText(ElevatedButton, 'AUTO'));
        await tester.pumpAndSettle();

        expect(mqtt.publishes.length, 1);
        expect(mqtt.publishes.first.topic, 'muehle/hf/beam-steer/cmd');
        expect(mqtt.publishes.first.payload, '{"action":"enable"}');
        expect(mqtt.publishes.first.retain, isTrue);
      });

      testWidgets('reflects enabled state, disables on tap, shows last reason', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        store.setUltrabeam();
        store.setOnline('muehle/hf/beam-steer');
        store.applyState('muehle/hf/beam-steer', {
          'enabled': true,
          'last': {'bearing': 265, 'direction': 'reverse', 'reason': 'behind: switch to 180°'},
        });
        await pump(tester, store, mqtt);

        expect(smart(tester).style!.backgroundColor!.resolve({}), AppTheme.accent);
        expect(find.text('AUTO · behind: switch to 180°'), findsOneWidget);

        await tester.tap(find.widgetWithText(ElevatedButton, 'AUTO'));
        await tester.pumpAndSettle();
        expect(mqtt.publishes.single.payload, '{"action":"disable"}');
      });

      testWidgets('disabled while beamsteer is offline, even with the controller up', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        store.setUltrabeam();
        store.setBridgeOffline('muehle/hf/beam-steer');
        await pump(tester, store, mqtt);

        expect(smart(tester).onPressed, isNull);
        expect(find.textContaining('AUTO ·'), findsNothing);
      });

      testWidgets('the decision text never changes the card height', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        store.setUltrabeam(direction: 'bidirectional', band: '20m');
        store.setOnline('muehle/hf/beam-steer');
        store.applyState('muehle/hf/beam-steer', {'enabled': false});
        await pump(tester, store, mqtt);
        final before = tester.getSize(find.byType(UltrabeamPanel)).height;

        store.applyState('muehle/hf/beam-steer', {
          'enabled': true,
          'last': {'bearing': 230, 'reason': 'in bidirectional back lobe'},
        });
        await tester.pumpAndSettle();

        expect(find.text('AUTO · in bidirectional back lobe'), findsOneWidget);
        expect(tester.getSize(find.byType(UltrabeamPanel)).height, before);
      });

      testWidgets('stays live while the elements move', (tester) async {
        final store = BusStore();
        final mqtt = FakeMqttService(store);
        store.setUltrabeam(moving: true);
        store.setOnline('muehle/hf/beam-steer');
        await pump(tester, store, mqtt);

        expect(smart(tester).onPressed, isNotNull);
      });
    });
  });
}
