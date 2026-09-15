// uhf_radio_panel_test.dart — widget tests for the IC-9700 radio surface
// (U7) on the UHF tab. Covers the plan's scenarios: the always-rendering
// session readout (idle/connecting/live/error), the arm toggle that gates
// on the bus link ONLY (arm-while-idle is the on-demand session's connect
// trigger, R1/R14), PTT + tuning gated on armed ∧ live, PTT pending
// resolution (first /state newer than the tap; 5 s no-confirmation
// timeout), the SEL marker, safe rendering of type-confused state, ERR
// surfacing/clearing, and healthy idle producing neither an OFFLINE tag
// nor a station fault row (R16).

import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/status_tag.dart';
import 'package:hf_console/ui/widgets/uhf_radio_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

const address = 'muehle/uhf/radio';

void main() {
  Future<void> pumpPanel(
    WidgetTester tester, {
    required BusStore store,
    required FakeMqttService mqtt,
  }) async {
    await tester.pumpWidget(
        TestHarness(store: store, mqtt: mqtt, child: const UhfRadioPanel()));
    await tester.pumpAndSettle();
  }

  ElevatedButton button(WidgetTester tester, String key) =>
      tester.widget<ElevatedButton>(find.byKey(ValueKey(key)));

  /// The active (dangerActive) button paints the solid semantic background.
  bool bgIs(WidgetTester tester, String key, Color color) =>
      button(tester, key).style?.backgroundColor?.resolve(const {}) == color;

  StatusTag? tag(WidgetTester tester, String label) {
    final found = find.widgetWithText(StatusTag, label);
    return found.evaluate().isEmpty
        ? null
        : tester.widget<StatusTag>(found.first);
  }

  group('readout rendering (live fixture)', () {
    testWidgets('renders freq/mode/band/meters for both VFOs', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(); // live, sub selected (145.800), main 432.100

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('432.100'), findsOneWidget); // MAIN
      expect(find.text('145.800'), findsOneWidget); // SUB
      expect(find.text('70CM'), findsOneWidget);
      expect(find.text('2M'), findsOneWidget);
      // Mode text appears in the readouts AND the mode button row.
      expect(find.text('USB'), findsWidgets);
      expect(find.text('FM'), findsWidgets);
      // Two MHz suffixes — both VFO rows carry a frequency.
      expect(find.text(' MHz'), findsNWidgets(2));
      // Meters: S-meter present (120), the others omitted by the fixture.
      expect(find.text('S 120'), findsOneWidget);
      expect(find.text('LIVE'), findsOneWidget);
      expect(tag(tester, 'LIVE')?.color, AppTheme.green);
    });

    testWidgets('SEL marker tracks selected_vfo for both values',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(selectedVfo: 'sub');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(find.widgetWithText(StatusTag, 'SEL'), findsOneWidget);
      // The one SEL sits in the SUB row (same vertical band as its label —
      // .first: 'SUB' also labels the local target-picker button).
      final selY = tester.getTopLeft(find.widgetWithText(StatusTag, 'SEL')).dy;
      expect(selY - tester.getTopLeft(find.text('SUB').first).dy, lessThan(24));

      store.setUhfRadio(selectedVfo: 'main');
      await tester.pumpAndSettle();
      expect(find.widgetWithText(StatusTag, 'SEL'), findsOneWidget);
      final selY2 = tester.getTopLeft(find.widgetWithText(StatusTag, 'SEL')).dy;
      expect(selY2 - tester.getTopLeft(find.text('MAIN').first).dy, lessThan(24));
    });

    testWidgets('the TX pill renders only while tx == "tx" (string enum)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(tx: 'rx');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(find.widgetWithText(StatusTag, 'TX'), findsNothing);

      store.setUhfRadio(tx: 'tx');
      await tester.pumpAndSettle();
      expect(find.widgetWithText(StatusTag, 'TX'), findsOneWidget);
      expect(bgIs(tester, 'uhf-ptt-toggle', AppTheme.red), isTrue);
    });
  });

  group('session-state readout (the panel always renders)', () {
    testWidgets('idle: session readout, PTT + tuning disabled, arm ENABLED',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('IDLE'), findsOneWidget);
      expect(button(tester, 'uhf-ptt-toggle').onPressed, isNull);
      expect(button(tester, 'uhf-freq-set').onPressed, isNull);
      for (final m in ['cw', 'usb', 'lsb', 'am', 'fm', 'data']) {
        expect(button(tester, 'uhf-mode-$m').onPressed, isNull);
      }
      expect(button(tester, 'uhf-arm-toggle').onPressed, isNotNull);
      // Healthy idle carries no radio-measured fields — no OFFLINE tag
      // (R14/R16), and the meter row is absent entirely.
      expect(find.text('OFFLINE'), findsNothing);
      expect(find.text('S 120'), findsNothing);
    });

    testWidgets('connecting: amber CONNECTING tag, actions gated',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'connecting');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('CONNECTING'), findsOneWidget);
      expect(tag(tester, 'CONNECTING')?.color, AppTheme.amber);
      expect(button(tester, 'uhf-ptt-toggle').onPressed, isNull);
      expect(button(tester, 'uhf-arm-toggle').onPressed, isNotNull);
    });

    testWidgets('error: red ERROR tag + ERR text', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(
          sessionState: 'error', error: 'radio: login refused');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('ERROR'), findsOneWidget);
      expect(tag(tester, 'ERROR')?.color, AppTheme.red);
      expect(tag(tester, 'ERR'), isNotNull);
      expect(find.textContaining('login refused'), findsOneWidget);
    });

    testWidgets('healthy idle produces no station fault row (R16)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      // device_online:false on this slot is CI-V session liveness, not a
      // dead device — the offline list (the faults bar's source) must not
      // carry it.
      expect(store.offlineList.any((s) => s.contains(address)), isFalse);
      expect(
          store.faultHistory
              .where((r) => r.active && r.address == address),
          isEmpty);
    });
  });

  group('arm gate (link-up-only gating is the deliberate deviation)', () {
    testWidgets('arm toggle publishes the one-shot arm payload (non-retained)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle'); // armed:false

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('uhf-arm-toggle')));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      final rec = mqtt.publishes.single;
      expect(rec.topic, '$address/cmd');
      expect(rec.retain, isFalse,
          reason: 'KTD6: a retained arm permit would re-arm after a bridge '
              'restart and defeat fail-disarm');
      expect(jsonDecode(rec.payload), {'action': 'arm'});
    });

    testWidgets('armed readback flips the toggle to a red DISARM',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: true);

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('DISARM'), findsOneWidget);
      expect(bgIs(tester, 'uhf-arm-toggle', AppTheme.red), isTrue);

      await tester.tap(find.byKey(const ValueKey('uhf-arm-toggle')));
      await tester.pumpAndSettle();
      expect(jsonDecode(mqtt.publishes.single.payload), {'action': 'disarm'});
    });

    testWidgets('arm stays enabled while the bridge LWT is down (deviation)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');
      store.setBridgeOffline(address);

      await pumpPanel(tester, store: store, mqtt: mqtt);

      // The link-up-only rule (R14): a dead bridge never executes the
      // one-shot cmd — the faults bar carries the bridge-down row.
      expect(button(tester, 'uhf-arm-toggle').onPressed, isNotNull);
    });

    testWidgets('arm is inert while the whole bus link is down',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      store.markDisconnected();
      await tester.pumpAndSettle();
      expect(button(tester, 'uhf-arm-toggle').onPressed, isNull);

      await tester.tap(find.byKey(const ValueKey('uhf-arm-toggle')),
          warnIfMissed: false);
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);
    });
  });

  group('PTT (armed ∧ live, pending window, readback truth)', () {
    testWidgets('unarmed PTT tap is impossible (button disabled)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: false); // live, unarmed

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(button(tester, 'uhf-ptt-toggle').onPressed, isNull);
      await tester.tap(find.byKey(const ValueKey('uhf-ptt-toggle')),
          warnIfMissed: false);
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('armed+live enables PTT; tap publishes ptt on',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: true);

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(button(tester, 'uhf-ptt-toggle').onPressed, isNotNull);
      await tester.tap(find.byKey(const ValueKey('uhf-ptt-toggle')));
      await tester.pumpAndSettle();

      expect(jsonDecode(mqtt.publishes.single.payload),
          {'action': 'ptt', 'value': 'on'});

      // Resolve the pending window so no timer leaks into teardown.
      store.setUhfRadio(armed: true, tx: 'tx');
      await tester.pumpAndSettle();
    });

    testWidgets('the rejection path still renders ERR when it arrives from '
        'the bus (retained /state.error)', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(
          armed: false,
          error: 'ptt rejected: not armed');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(tag(tester, 'ERR'), isNotNull);
      expect(find.textContaining('not armed'), findsOneWidget);
    });

    testWidgets('PTT pending clears on the next /state (tx flip = success)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: true, tx: 'rx');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('uhf-ptt-toggle')));
      await tester.pump(); // tap frame — pending window opens
      expect(find.text('PTT …'), findsOneWidget);
      expect(button(tester, 'uhf-ptt-toggle').onPressed, isNull);

      // The bridge executes and republishes; the ts moved → pending ends.
      store.setUhfRadio(armed: true, tx: 'tx');
      await tester.pumpAndSettle();

      expect(find.text('PTT …'), findsNothing);
      expect(find.text('PTT OFF'), findsOneWidget);
      expect(find.text('PTT: no bus confirmation'), findsNothing);
      expect(button(tester, 'uhf-ptt-toggle').onPressed, isNotNull);
    });

    testWidgets('PTT pending times out to the no-confirmation ERR at 5 s',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: true, tx: 'rx');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('uhf-ptt-toggle')));
      await tester.pump();

      await tester.pump(const Duration(seconds: 5));
      await tester.pumpAndSettle();

      expect(find.text('PTT: no bus confirmation'), findsOneWidget);
      expect(tag(tester, 'ERR'), isNotNull);
      // No bus confirmation ⇒ no optimistic tx rendering.
      expect(find.widgetWithText(StatusTag, 'TX'), findsNothing);

      // A later /state (bus alive again) clears the timeout rendering.
      store.setUhfRadio(armed: true, tx: 'rx');
      await tester.pumpAndSettle();
      expect(find.text('PTT: no bus confirmation'), findsNothing);
    });

    testWidgets('PTT OFF: readback tx=="tx" makes the next tap publish off',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: true, tx: 'tx');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(find.text('PTT OFF'), findsOneWidget);

      await tester.tap(find.byKey(const ValueKey('uhf-ptt-toggle')));
      await tester.pumpAndSettle();
      expect(jsonDecode(mqtt.publishes.single.payload),
          {'action': 'ptt', 'value': 'off'});

      // Resolve the pending window so no timer leaks into teardown.
      store.setUhfRadio(armed: true, tx: 'rx');
      await tester.pumpAndSettle();
    });
  });

  group('tuning (per-VFO cmds, gated armed ∧ live)', () {
    Future<void> armAndPump(WidgetTester tester, BusStore store,
        FakeMqttService mqtt) async {
      store.setUhfRadio(armed: true);
      await pumpPanel(tester, store: store, mqtt: mqtt);
    }

    testWidgets('set_freq carries the Hz string and the target VFO',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      await armAndPump(tester, store, mqtt);

      await tester.enterText(
          find.byKey(const ValueKey('uhf-freq-input')), '432.100');
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('uhf-freq-set')));
      await tester.pumpAndSettle();

      expect(jsonDecode(mqtt.publishes.single.payload),
          {'action': 'set_freq', 'value': '432100000', 'vfo': 'sub'});
    });

    testWidgets('the local MAIN/SUB picker retargets the next cmd',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      await armAndPump(tester, store, mqtt);

      await tester.tap(find.byKey(const ValueKey('uhf-vfo-target-main')));
      await tester.pumpAndSettle();
      await tester.enterText(
          find.byKey(const ValueKey('uhf-freq-input')), '1296.100');
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('uhf-freq-set')));
      await tester.pumpAndSettle();

      expect(jsonDecode(mqtt.publishes.single.payload),
          {'action': 'set_freq', 'value': '1296100000', 'vfo': 'main'});
    });

    testWidgets('mode buttons publish set_mode; the readback drives active',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: true, subMode: 'fm');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      // SUB (target) is on fm — only the fm button paints active.
      expect(bgIs(tester, 'uhf-mode-fm', AppTheme.accent), isTrue);
      expect(bgIs(tester, 'uhf-mode-usb', AppTheme.accent), isFalse);

      await tester.tap(find.byKey(const ValueKey('uhf-mode-usb')));
      await tester.pumpAndSettle();
      expect(jsonDecode(mqtt.publishes.single.payload),
          {'action': 'set_mode', 'value': 'usb', 'vfo': 'sub'});
    });

    testWidgets('an unparseable freq keeps SET disabled', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      await armAndPump(tester, store, mqtt);

      await tester.enterText(find.byKey(const ValueKey('uhf-freq-input')), 'abc');
      await tester.pumpAndSettle();
      expect(button(tester, 'uhf-freq-set').onPressed, isNull);
      expect(mqtt.publishes, isEmpty);
    });
  });

  group('robustness', () {
    testWidgets('type-confused /state renders dashes, never throws',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setOnline(address);
      store.applyState(address, {
        'session_state': 5,
        'armed': 'yes',
        'tx': 3,
        'main': 'nope',
        'sub': ['not', 'a', 'map'],
        'selected_vfo': 9,
        's_meter': 'x',
        'device_online': true,
        'ts': '2026-09-14T12:00:00Z',
      });

      await pumpPanel(tester, store: store, mqtt: mqtt);

      // Both VFO readouts degrade to the dash (freq AND mode per row); no
      // SEL (selected_vfo is not a known VFO); no crash. A type-confused
      // session renders no tag.
      expect(find.text('—'), findsNWidgets(4));
      expect(find.widgetWithText(StatusTag, 'SEL'), findsNothing);
      expect(button(tester, 'uhf-ptt-toggle').onPressed, isNull);
    });

    testWidgets('an error-free republish clears the ERR rendering',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(error: 'radio: login refused');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(tag(tester, 'ERR'), isNotNull);

      store.setUhfRadio();
      await tester.pumpAndSettle();

      expect(find.widgetWithText(StatusTag, 'ERR'), findsNothing);
      expect(find.textContaining('login refused'), findsNothing);
    });

    testWidgets('a silent slot renders no session tag and no OFFLINE claim '
        'while the bridge is up but stateless', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      // Bridge online (retained /status), never published /state.
      store.applyStatus(address, 'online');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('OFFLINE'), findsNothing);
      expect(find.text('LIVE'), findsNothing);
      expect(button(tester, 'uhf-ptt-toggle').onPressed, isNull);
      expect(button(tester, 'uhf-arm-toggle').onPressed, isNotNull);
    });
  });
}
