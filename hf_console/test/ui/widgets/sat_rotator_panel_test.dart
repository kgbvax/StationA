// sat_rotator_panel_test.dart — widget tests for the sat-ops rotator surface
// (U8): per-axis readouts, goto, and the STOP e-stop on the two spid/ercm
// slots muehle/uhf/az-rotator + muehle/uhf/el-rotator. Covers the plan's
// scenarios: two-layer gating (slot.isOnline AND store.linkUp), client-side
// travel-limit validation against /meta capabilities, the value-key goto
// payload published non-retained, and the STOP that fires on both slots'
// /cmd whenever any axis is operable.

import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/uhf_park.dart';
import 'package:hf_console/ui/widgets/sat_rotator_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

const azAddress = 'muehle/uhf/az-rotator';
const elAddress = 'muehle/uhf/el-rotator';

void main() {
  Future<void> pumpPanel(
    WidgetTester tester, {
    required BusStore store,
    required FakeMqttService mqtt,
  }) async {
    await tester.pumpWidget(
        TestHarness(store: store, mqtt: mqtt, child: const SatRotatorPanel()));
    await tester.pumpAndSettle();
  }

  ElevatedButton button(WidgetTester tester, Finder finder) =>
      tester.widget<ElevatedButton>(finder);

  group('readouts', () {
    testWidgets('shows position, target and moving indicator per axis',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress,
          axis: 'az', pos: 45.5, target: 90, moving: true);
      store.setSatRotator(elAddress, axis: 'el', pos: 12.0);

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('AZIMUTH'), findsOneWidget);
      expect(find.text('ELEVATION'), findsOneWidget);
      expect(find.text('45.5°'), findsOneWidget);
      expect(find.text('90°'), findsOneWidget);
      expect(find.text('12°'), findsOneWidget);
      expect(find.text('MOVING'), findsOneWidget); // az only
    });

    testWidgets('at rest on target the arrow and target are hidden', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 200, target: 200);
      store.setSatRotator(elAddress, axis: 'el', pos: 1, target: 3);

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('200°'), findsOneWidget); // position only
      expect(find.text('3°'), findsOneWidget); // el still short of its target
    });

    testWidgets('omitted readback renders a dash, not a fabricated position',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az'); // no pos, no target

      await pumpPanel(tester, store: store, mqtt: mqtt);

      // Both axis rows render unconditionally; with no readback anywhere
      // each shows exactly one dash.
      expect(find.text('—'), findsNWidgets(2));
      expect(find.text('45.5°'), findsNothing);
    });

    testWidgets(
        'a type-confused /state payload renders dashes, never a TypeError',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      // The readwrite hf account can publish `az` as a String (or the
      // /meta limits as strings); a hard cast in build would throw on
      // every rebuild — a UI DoS. The safe accessors must degrade to
      // dashes / parse-only validation instead.
      store.setOnline(azAddress);
      store.applyMeta(azAddress, {
        'schema': '1.0',
        'role': 'rotator',
        'capabilities': {
          'axes': ['az'],
          'limits': {'min': '0', 'max': '360'}, // type-confused too
        },
      });
      store.applyState(azAddress, {
        'az': '45',
        'target': '90',
        'moving': true,
        'device_online': true,
        'ts': '2026-09-13T12:34:56Z',
      });

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(tester.takeException(), isNull);
      // az degraded to a dash (el absent) — never the string '45°'.
      expect(find.text('—'), findsNWidgets(2));
      expect(find.text('45°'), findsNothing);
      expect(find.text('90°'), findsNothing);
    });
  });

  group('gating (two-layer AND: slot.isOnline && store.linkUp)', () {
    testWidgets('both axes online → controls enabled', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45);
      store.setSatRotator(elAddress, axis: 'el', pos: 10);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.enterText(
          find.byKey(const ValueKey('sat-az-input')), '120');
      await tester.enterText(find.byKey(const ValueKey('sat-el-input')), '30');
      await tester.pumpAndSettle();

      expect(button(tester, find.byKey(const ValueKey('sat-az-goto'))).onPressed,
          isNotNull);
      expect(button(tester, find.byKey(const ValueKey('sat-el-goto'))).onPressed,
          isNotNull);
      expect(
          button(tester, find.byKey(const ValueKey('sat-stop'))).onPressed, isNotNull);
    });

    testWidgets('el device_online false → el disabled, az still operable',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45);
      store.setSatRotator(elAddress, axis: 'el', pos: 10);
      store.setDeviceOffline(elAddress);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.enterText(
          find.byKey(const ValueKey('sat-az-input')), '120');
      await tester.enterText(find.byKey(const ValueKey('sat-el-input')), '30');
      await tester.pumpAndSettle();

      expect(button(tester, find.byKey(const ValueKey('sat-az-goto'))).onPressed,
          isNotNull);
      expect(
          button(tester, find.byKey(const ValueKey('sat-el-goto'))).onPressed, isNull);
      // One live axis keeps the e-stop live.
      expect(
          button(tester, find.byKey(const ValueKey('sat-stop'))).onPressed, isNotNull);
    });

    testWidgets('az bridge offline → az disabled (bridge layer of the AND)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45);
      store.setBridgeOffline(azAddress);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.enterText(
          find.byKey(const ValueKey('sat-az-input')), '120');
      await tester.pumpAndSettle();

      expect(
          button(tester, find.byKey(const ValueKey('sat-az-goto'))).onPressed, isNull);
    });

    testWidgets('linkUp false → whole panel inert (stale state not operable)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45);
      store.setSatRotator(elAddress, axis: 'el', pos: 10);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      store.markDisconnected();
      await tester.pumpAndSettle();

      // Retained slot state is still present — that is exactly the trap: it
      // must not render operable while the link is down.
      expect(store.slots[azAddress]!.isOnline, isTrue);
      expect(
          button(tester, find.byKey(const ValueKey('sat-az-goto'))).onPressed, isNull);
      expect(
          button(tester, find.byKey(const ValueKey('sat-el-goto'))).onPressed, isNull);
      expect(
          button(tester, find.byKey(const ValueKey('sat-stop'))).onPressed, isNull);

      await tester.tap(find.byKey(const ValueKey('sat-stop')));
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);
    });
  });

  group('goto', () {
    testWidgets('publishes the value-key payload non-retained', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.enterText(find.byKey(const ValueKey('sat-az-input')), '45');
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('sat-az-goto')));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      final rec = mqtt.publishes.first;
      expect(rec.topic, 'muehle/uhf/az-rotator/cmd');
      expect(rec.retain, isFalse,
          reason: 'KTD13: rotator /cmd is one-shot, non-retained');
      expect(jsonDecode(rec.payload), {'action': 'goto', 'value': '45.0'});
    });

    testWidgets(
        'GOTO after a ± step publishes the STEPPED bearing, not the stale '
        'pre-step closure value (review T1)', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.enterText(find.byKey(const ValueKey('sat-az-input')), '45');
      await tester.pumpAndSettle();
      // Step the field to 46 (a programmatic controller write fires no
      // onChanged) and hit GOTO without any other rebuild trigger.
      await tester.tap(find.byKey(const ValueKey('sat-az-step-up')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('sat-az-goto')));
      await tester.pumpAndSettle();

      expect(jsonDecode(mqtt.publishes.single.payload),
          {'action': 'goto', 'value': '46.0'},
          reason: 'the field shows 46; publishing 45 is the stale-closure bug');
    });

    testWidgets('publishes on the el slot with el limits', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(elAddress, axis: 'el', pos: 10);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.enterText(find.byKey(const ValueKey('sat-el-input')), '80');
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('sat-el-goto')));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.single.topic, 'muehle/uhf/el-rotator/cmd');
      expect(jsonDecode(mqtt.publishes.single.payload),
          {'action': 'goto', 'value': '80.0'});
    });

    testWidgets('publish disabled while the input is empty', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      // Input untouched (empty) — GOTO must be disabled.
      expect(
          button(tester, find.byKey(const ValueKey('sat-az-goto'))).onPressed, isNull);
    });

    testWidgets('publish disabled while out of limits; boundaries pass',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45); // limits 0..360

      await pumpPanel(tester, store: store, mqtt: mqtt);

      Future<bool> gotoEnabledFor(String text) async {
        await tester.enterText(
            find.byKey(const ValueKey('sat-az-input')), text);
        await tester.pumpAndSettle();
        return button(tester, find.byKey(const ValueKey('sat-az-goto'))).onPressed !=
            null;
      }

      expect(await gotoEnabledFor('400'), isFalse, reason: 'above max');
      expect(await gotoEnabledFor('-1'), isFalse, reason: 'below min');
      expect(await gotoEnabledFor('abc'), isFalse, reason: 'not a number');
      expect(await gotoEnabledFor('360'), isTrue, reason: 'max is inclusive');
      expect(await gotoEnabledFor('0'), isTrue, reason: 'min is inclusive');
      expect(await gotoEnabledFor('359.9'), isTrue);
    });

    testWidgets('no /meta limits → parse-only validation (bridge R10 backstop)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      // Bridge up, state up, but no /meta yet — the panel cannot know the
      // travel envelope; any finite value stays publishable and the bridge's
      // R10 refusal surfaces via the faults bar.
      store.setOnline(azAddress);
      store.applyState(azAddress, {'az': 45, 'device_online': true});

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.enterText(find.byKey(const ValueKey('sat-az-input')), '999');
      await tester.pumpAndSettle();

      expect(button(tester, find.byKey(const ValueKey('sat-az-goto'))).onPressed,
          isNotNull);
    });

    testWidgets('±1° steppers step from the current input and clamp',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45);
      store.setSatRotator(elAddress, axis: 'el', pos: 10); // limits 0..90

      await pumpPanel(tester, store: store, mqtt: mqtt);

      String inputText(String axis) =>
          tester.widget<TextField>(find.byKey(ValueKey('sat-$axis-input')))
              .controller!
              .text;

      // Empty input + step → steps from the live position.
      await tester.tap(find.byKey(const ValueKey('sat-az-step-up')));
      await tester.pumpAndSettle();
      expect(inputText('az'), '46');

      // Steps from the typed value, not the position.
      await tester.enterText(
          find.byKey(const ValueKey('sat-az-input')), '100');
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('sat-az-step-down')));
      await tester.pumpAndSettle();
      expect(inputText('az'), '99');

      // Fractional values keep their decimals.
      await tester.enterText(
          find.byKey(const ValueKey('sat-el-input')), '10.5');
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('sat-el-step-up')));
      await tester.pumpAndSettle();
      expect(inputText('el'), '11.5');

      // Clamped at the el max (90): stepping up past it parks at 90.
      await tester.enterText(find.byKey(const ValueKey('sat-el-input')), '90');
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('sat-el-step-up')));
      await tester.pumpAndSettle();
      expect(inputText('el'), '90');
      // Clamped at the el min (0).
      await tester.enterText(find.byKey(const ValueKey('sat-el-input')), '0');
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('sat-el-step-down')));
      await tester.pumpAndSettle();
      expect(inputText('el'), '0');
    });
  });

  group('STOP (the operator e-stop)', () {
    testWidgets('publishes stop to BOTH slots non-retained on every tap',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45, moving: true);
      store.setSatRotator(elAddress, axis: 'el', pos: 10, moving: true);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('sat-stop')));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 2);
      expect(mqtt.publishes[0].topic, 'muehle/uhf/az-rotator/cmd');
      expect(mqtt.publishes[1].topic, 'muehle/uhf/el-rotator/cmd');
      for (final rec in mqtt.publishes) {
        expect(jsonDecode(rec.payload), {'action': 'stop'});
        expect(rec.retain, isFalse);
      }
    });

    testWidgets('enabled while exactly one axis is online', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45);
      // el never heard from: bridge offline AND device offline.

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(
          button(tester, find.byKey(const ValueKey('sat-stop'))).onPressed, isNotNull);

      // And the stop still dual-publishes (belt-and-braces against the dead
      // slot path).
      await tester.tap(find.byKey(const ValueKey('sat-stop')));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.length, 2);
    });

    testWidgets('disabled when no axis is operable', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(
          button(tester, find.byKey(const ValueKey('sat-stop'))).onPressed, isNull);
    });
  });

  group('error surfacing (loud, never silent)', () {
    testWidgets('a bridge refusal surfaces as an in-panel ERR tag + text',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress,
          axis: 'az', pos: 45, error: 'goto refused: target outside travel limits');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('ERR'), findsOneWidget);
      expect(find.textContaining('target outside travel limits'), findsOneWidget);
    });

    testWidgets('an error-free republish clears the ERR indication',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress,
          axis: 'az', pos: 45, error: 'goto refused: target outside travel limits');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(find.text('ERR'), findsOneWidget);

      // Next error-free /state republish drops the error field.
      store.setSatRotator(azAddress, axis: 'az', pos: 60);
      await tester.pumpAndSettle();

      expect(find.text('ERR'), findsNothing);
      expect(find.text('60°'), findsOneWidget);
    });
  });

  group('park', () {
    tearDown(() => UhfPark.notifier.value = (az: UhfPark.defaultAz, el: UhfPark.defaultEl));

    testWidgets('sends both axes to the default park position (200° / 3°), non-retained',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45);
      store.setSatRotator(elAddress, axis: 'el', pos: 10);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(find.text('PARK 200° / 3°'), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('sat-park')));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.map((r) => r.topic),
          ['muehle/uhf/az-rotator/cmd', 'muehle/uhf/el-rotator/cmd']);
      expect(mqtt.publishes.map((r) => jsonDecode(r.payload)), [
        {'action': 'goto', 'value': '200.0'},
        {'action': 'goto', 'value': '3.0'},
      ]);
      expect(mqtt.publishes.map((r) => r.retain), [false, false]);
    });

    testWidgets('follows the configured position', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45);
      store.setSatRotator(elAddress, axis: 'el', pos: 10);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      UhfPark.notifier.value = (az: 180.5, el: 0);
      await tester.pumpAndSettle();
      expect(find.text('PARK 180.5° / 0°'), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('sat-park')));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.map((r) => jsonDecode(r.payload)['value']), ['180.5', '0.0']);
    });

    testWidgets('only operable axes are sent; link down disables PARK', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setSatRotator(azAddress, axis: 'az', pos: 45);
      store.setSatRotator(elAddress, axis: 'el', pos: 10);
      store.setDeviceOffline(elAddress);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('sat-park')));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.map((r) => r.topic), ['muehle/uhf/az-rotator/cmd']);

      store.markDisconnected();
      await tester.pumpAndSettle();
      expect(button(tester, find.byKey(const ValueKey('sat-park'))).onPressed, isNull);
    });
  });

  group('UhfPark settings parsing', () {
    test('accepts az 0–360 and el 0–90; anything else falls back to the defaults', () {
      expect(UhfPark.parseAz('200'), 200);
      expect(UhfPark.parseAz('361'), isNull);
      expect(UhfPark.parseEl('3'), 3);
      expect(UhfPark.parseEl('-1'), isNull);
      expect(UhfPark.parseEl('abc'), isNull);

      UhfPark.load({UhfPark.azKey: '90', UhfPark.elKey: 'x'});
      expect(UhfPark.notifier.value, (az: 90.0, el: UhfPark.defaultEl));
      UhfPark.load({});
      expect(UhfPark.notifier.value, (az: 200.0, el: 3.0));
    });
  });
}
