import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/wiring.dart';
import 'package:hf_console/ui/widgets/compass_panel.dart';
import 'package:hf_console/ui/theme.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('CompassPanel', () {
    testWidgets('shows OFFLINE when rotator bridge is down', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      expect(find.textContaining('OFFLINE'), findsOneWidget);
      // The system offline color is red (StatusPill convention) — the pill
      // must not fall back to a muted grey.
      final chip = tester.widget<Container>(
        find.ancestor(of: find.textContaining('OFFLINE'), matching: find.byType(Container)).first,
      );
      final decoration = chip.decoration! as BoxDecoration;
      expect((decoration.border! as Border).top.color, AppTheme.red);
    });

    testWidgets('shows current azimuth and hides target when within 5°', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0, targetAz: 122.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      expect(find.textContaining('120°'), findsOneWidget);
      expect(find.textContaining('→'), findsNothing);
    });

    testWidgets('shows target azimuth when more than 5° away', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setRotator(az: 120.0, targetAz: 200.0);

      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();

      expect(find.textContaining('120°'), findsOneWidget);
      expect(find.textContaining('→ 200°'), findsOneWidget);
    });

    // Preset-button coverage (NA / SA / VK / JA / STOP) lives in
    // `rotator_presets_bar_test.dart` — the rail overlay the compass card
    // carries on its right edge (above the zoom stepper) plus the phone
    // layout's horizontal bar. The compass card itself owns tap-to-aim +
    // the +/- zoom stepper here.
  });

  group('CompassPanel rotator surface', () {
    testWidgets('rotator: null renders the bare DX compass — no azimuth chip, no presets',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      // HF rotator state is seeded and online: it must not leak into a
      // dial that has no rotator surface (Station page).
      store.setRotator(az: 120.0, targetAz: 200.0);

      await tester.pumpWidget(
        TestHarness(store: store, mqtt: mqtt, child: const CompassPanel(rotator: null)),
      );
      await tester.pumpAndSettle();

      expect(find.textContaining('120°'), findsNothing);
      expect(find.textContaining('OFFLINE'), findsNothing);
      expect(find.widgetWithText(ElevatedButton, 'NA 330'), findsNothing);
    });

    testWidgets('vhf surface reads the az-rotator state shape (az / target)', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator('muehle/uhf/az-rotator', axis: 'az', pos: 175, target: 90);

      await tester.pumpWidget(
        TestHarness(store: store, mqtt: mqtt, child: const CompassPanel(rotator: vhfRotator)),
      );
      await tester.pumpAndSettle();

      expect(find.textContaining('175°'), findsOneWidget);
      expect(find.textContaining('→ 90°'), findsOneWidget);
      // The HF presets rail is an HF big-DX affordance — never on the VHF dial.
      expect(find.widgetWithText(ElevatedButton, 'NA 330'), findsNothing);
    });

    testWidgets('vhf chip aim publishes goto on the sat-bridge contract, unretained',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator('muehle/uhf/az-rotator', axis: 'az', pos: 175, target: 90);
      final now = DateTime.now().toUtc().toIso8601String();
      store.apply(
          'muehle/hf/spots/state',
          jsonEncode({
            'ts': now,
            'device_online': true,
            'selected': {'call': 'VK9XY', 'azimuth': 62.4, 'source': 'log4om', 'ts': now},
          }),
          false);

      await tester.pumpWidget(
        TestHarness(store: store, mqtt: mqtt, child: const CompassPanel(rotator: vhfRotator)),
      );
      await tester.pump(const Duration(milliseconds: 600));

      await tester.tap(find.textContaining('VK9XY'), warnIfMissed: false);
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(mqtt.publishes.first.topic, 'muehle/uhf/az-rotator/cmd');
      expect(mqtt.publishes.first.payload, contains('goto'));
      expect(mqtt.publishes.first.payload, contains('62'));
      expect(mqtt.publishes.first.retain, isFalse);
    });
  });
}
