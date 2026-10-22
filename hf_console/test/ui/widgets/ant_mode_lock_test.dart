import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/antenna_panel.dart';
import 'package:hf_console/ui/widgets/compass_panel.dart';
import 'package:hf_console/ui/widgets/faults_bar.dart';
import 'package:hf_console/ui/widgets/rotator_presets_bar.dart';
import 'package:hf_console/ui/widgets/ultrabeam_panel.dart';

import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

/// The antenna-mode lock as the operator sees it across the HF controls:
/// what the bridges would refuse is greyed out with the reason on screen;
/// the emergency actions (RETRACT, STOP) stay live.
void main() {
  VoidCallback? pressed(WidgetTester tester, String label) =>
      tester.widget<ElevatedButton>(find.widgetWithText(ElevatedButton, label)).onPressed;

  group('Ultrabeam under the lock', () {
    Future<FakeMqttService> pump(WidgetTester tester, String mode, {bool retracted = true}) async {
      final store = BusStore()
        ..setUltrabeam(direction: 'forward', band: '20m', retracted: retracted)
        ..setOnline('muehle/hf/beam-steer')
        ..setAntMode(mode);
      final mqtt = FakeMqttService(store);
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();
      return mqtt;
    }

    testWidgets('storm: direction keys and AUTO off, RETRACT live, reason shown', (tester) async {
      final mqtt = await pump(tester, 'storm');
      for (final l in ['FORWARD', '180°', 'BI-DIR', 'AUTO']) {
        expect(pressed(tester, l), isNull, reason: l);
      }
      expect(pressed(tester, 'RETRACT'), isNotNull);
      expect(find.text('STORM LOCK · parked 175° · elements held in'), findsOneWidget);
      expect(find.text('Ultrabeam · LOCKED'), findsOneWidget);

      await tester.tap(find.widgetWithText(ElevatedButton, 'RETRACT'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.payload, '{"action":"retract"}');
    });

    testWidgets('storm before ultrabridge confirms the retract: not "held in"', (tester) async {
      await pump(tester, 'storm', retracted: false);
      expect(find.text('STORM LOCK · parked 175° · elements not confirmed in'), findsOneWidget);
      expect(find.textContaining('held in'), findsNothing);
    });

    testWidgets('normal: the keys are live again', (tester) async {
      await pump(tester, 'normal');
      expect(pressed(tester, 'FORWARD'), isNotNull);
      expect(pressed(tester, 'AUTO'), isNotNull);
      expect(find.byKey(const ValueKey('ultrabeam-lock-reason')), findsNothing);
    });

    testWidgets('the 6 m forward correction is not sent under the lock', (tester) async {
      final store = BusStore()
        ..setRadio(band: '6m')
        ..setUltrabeam(direction: 'reverse')
        ..setAntMode('storm');
      final mqtt = FakeMqttService(store);
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const UltrabeamPanel()));
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);
    });
  });

  group('Antenna switch under the lock', () {
    testWidgets('port keys off with the reason, mode keys live', (tester) async {
      final store = BusStore()
        ..setRadio()
        ..setAntenna(selected: 'off', mode: 'manual')
        ..setAntMode('maintenance');
      final mqtt = FakeMqttService(store);
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const AntennaPanel()));
      await tester.pumpAndSettle();

      for (final l in ['GND', 'DUMMY', 'ULTRABEAM', 'FAN DIPOLE']) {
        expect(pressed(tester, l), isNull, reason: l);
      }
      expect(pressed(tester, 'AUTO'), isNotNull);
      expect(find.text('MAINTENANCE LOCK · grounded'), findsOneWidget);
    });

    testWidgets('switch not yet at off: "grounding…", never "grounded"', (tester) async {
      // antennaselect offline, or holding the switch while RF is on.
      final store = BusStore()
        ..setRadio()
        ..setAntenna(selected: 'port4', mode: 'auto')
        ..setAntMode('storm');
      await tester.pumpWidget(TestHarness(store: store, child: const AntennaPanel()));
      await tester.pumpAndSettle();
      expect(find.text('STORM LOCK · grounding…'), findsOneWidget);
      expect(find.textContaining('grounded'), findsNothing);
    });
  });

  group('HF compass under the lock', () {
    Future<FakeMqttService> pump(WidgetTester tester, String mode) async {
      await tester.binding.setSurfaceSize(const Size(700, 700));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      final store = BusStore()
        ..setRotator(az: 176, targetAz: 175)
        ..setAntMode(mode);
      final mqtt = FakeMqttService(store);
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pumpAndSettle();
      return mqtt;
    }

    testWidgets('storm: tap-to-aim sends nothing and the chip says why; STOP stays live', (tester) async {
      final mqtt = await pump(tester, 'storm');
      expect(find.textContaining('STORM LOCK · parked 175°'), findsOneWidget);

      await tester.tapAt(tester.getCenter(find.byType(CompassPanel)) + const Offset(0, -80));
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);

      expect(find.byType(RotatorPresetsRail), findsOneWidget);
      await tester.tap(find.widgetWithText(ElevatedButton, 'STOP'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.payload, '{"action":"stop"}');
    });

    testWidgets('storm with a failed park: the chip says so, not "parked"', (tester) async {
      await tester.binding.setSurfaceSize(const Size(700, 700));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      final store = BusStore()
        ..setRotator(az: 90, targetAz: 90)
        ..setAntMode('storm', phase: 'failed', fault: 'rotator offline');
      await tester.pumpWidget(TestHarness(store: store, child: const CompassPanel()));
      await tester.pumpAndSettle();
      expect(find.textContaining('STORM LOCK · PARK FAILED'), findsOneWidget);
      expect(find.textContaining('parked'), findsNothing);
    });

    testWidgets('maintenance: a disc tap sends nothing; the ±5° keys nudge with the override', (tester) async {
      final mqtt = await pump(tester, 'maintenance');
      expect(find.textContaining('MAINTENANCE · NUDGE ±5° ONLY'), findsOneWidget);

      // A tap names any bearing — a stray one must not slew the boom.
      await tester.tapAt(tester.getCenter(find.byType(CompassPanel)) + const Offset(0, -80));
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);

      await tester.tap(find.byKey(const ValueKey('rotator-nudge-cw')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('rotator-nudge-ccw')));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.map((p) => p.topic), everyElement('muehle/hf/rotator/cmd'));
      expect(jsonDecode(mqtt.publishes[0].payload), {'action': 'set_az', 'az': 181.0, 'override': 'maintenance'});
      expect(jsonDecode(mqtt.publishes[1].payload), {'action': 'set_az', 'az': 171.0, 'override': 'maintenance'});
    });

    testWidgets('the ±5° keys exist only in maintenance with the nudge', (tester) async {
      for (final mode in ['normal', 'storm']) {
        await pump(tester, mode);
        expect(find.byKey(const ValueKey('rotator-nudge-cw')), findsNothing, reason: mode);
        expect(find.byKey(const ValueKey('rotator-nudge-ccw')), findsNothing, reason: mode);
      }
    });

    testWidgets('maintenance: the selected-station chip does not aim', (tester) async {
      await tester.binding.setSurfaceSize(const Size(700, 700));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      final store = BusStore()
        ..setRotator(az: 176, targetAz: 175)
        ..setAntMode('maintenance');
      final now = DateTime.now().toUtc().toIso8601String();
      store.apply(
          'muehle/hf/spots/state',
          jsonEncode({
            'ts': now,
            'device_online': true,
            'selected': {'call': 'VK2XX', 'azimuth': 60.0, 'source': 'log4om', 'ts': now},
          }),
          false);
      final mqtt = FakeMqttService(store);
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
      await tester.pump(const Duration(milliseconds: 600));
      expect(find.textContaining('VK2XX'), findsOneWidget);
      await tester.tap(find.textContaining('VK2XX'), warnIfMissed: false);
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);
    });
  });

  group('Faults bar', () {
    testWidgets('a lock shows as a banner above the list', (tester) async {
      final store = BusStore()..setAntMode('storm', reason: 'DWD STURMBÖEN active, station unmanned');
      await tester.pumpWidget(TestHarness(store: store, child: const FaultsBar()));
      await tester.pumpAndSettle();
      expect(find.byKey(const ValueKey('faults-lock-banner')), findsOneWidget);
      expect(find.text('STORM LOCK · parked 175° · ANTENNA MOTION REFUSED'), findsOneWidget);
      expect(find.textContaining('DWD STURMBÖEN active, station unmanned'), findsOneWidget);
      expect(find.byKey(const ValueKey('faults-storm-suggestion')), findsNothing);
    });

    testWidgets('a lock still rotating says "parking", not "parked"', (tester) async {
      final store = BusStore()..setAntMode('storm', phase: 'rotating');
      await tester.pumpWidget(TestHarness(store: store, child: const FaultsBar()));
      await tester.pumpAndSettle();
      expect(find.text('STORM LOCK · parking → 175° · ANTENNA MOTION REFUSED'), findsOneWidget);
    });

    testWidgets('a storm suggestion shows as a banner', (tester) async {
      final store = BusStore()
        ..setAntMode('normal', stormSuggested: true, warnings: [
          {'id': 'dwd.1', 'event': 'WINDBÖEN', 'ii': '51', 'onset': '', 'expires': ''},
        ]);
      await tester.pumpWidget(TestHarness(store: store, child: const FaultsBar()));
      await tester.pumpAndSettle();
      expect(find.text('STORM MODE SUGGESTED'), findsOneWidget);
      expect(find.text('DWD WINDBÖEN active'), findsOneWidget);
      expect(find.byKey(const ValueKey('faults-lock-banner')), findsNothing);
    });

    testWidgets('normal without a suggestion: no banner', (tester) async {
      final store = BusStore()..setAntMode('normal');
      await tester.pumpWidget(TestHarness(store: store, child: const FaultsBar()));
      await tester.pumpAndSettle();
      expect(find.byKey(const ValueKey('faults-lock-banner')), findsNothing);
      expect(find.byKey(const ValueKey('faults-storm-suggestion')), findsNothing);
    });
  });
}
