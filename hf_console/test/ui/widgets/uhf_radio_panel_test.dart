// uhf_radio_panel_test.dart — widget tests for the IC-9700 radio surface
// (U7) on the UHF tab: the always-rendering session readout (idle /
// connecting / live / error), the bridge-liveness-only ARM gate (R16
// carve-out: device_online:false is the healthy idle), the R10 armed ∧ live
// gate on PTT + tuning, the pending-confirm semantics of arm/PTT (set on
// tap, cleared by the first newer /state, 5 s no-confirmation timeout), the
// SEL marker from selected_vfo, and the one-shot per-VFO value-key payloads
// (R9) published through the cmdRetain map.

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

/// The panel's mode button row vocabulary (UhfRadioPanel._modes, mirrored
/// here because the static is library-private).
const panelModes = ['cw', 'usb', 'lsb', 'am', 'fm'];

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

  ElevatedButton btn(WidgetTester tester, String key) =>
      tester.widget<ElevatedButton>(find.byKey(ValueKey(key)));

  StatusTag tagOf(WidgetTester tester, String label) =>
      tester.widget<StatusTag>(find.ancestor(
        of: find.text(label),
        matching: find.byType(StatusTag),
      ));

  /// A readback row: the nearest Row ancestor of the VFO label text.
  Finder vfoRow(String label) => find
      .ancestor(of: find.text(label), matching: find.byType(Row))
      .first;

  group('readout rendering (always renders — the panel core value)', () {
    testWidgets('live renders freq/mode/band and meters for BOTH VFOs',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(subMode: 'usb');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('432.100 MHz'), findsOneWidget); // MAIN
      expect(find.text('145.200 MHz'), findsOneWidget); // SUB
      expect(find.text('70CM'), findsOneWidget);
      expect(find.text('2M'), findsOneWidget);
      // Mode appears three times: the VFO readout text + the mode button of
      // each VFO (every VFO renders the full five-button row).
      expect(find.text('FM'), findsNWidgets(3));
      expect(find.text('USB'), findsNWidgets(3));
      // Raw 0-255 meters from the live snapshot.
      expect(find.textContaining('S 34 · PWR 100 · SWR 12 · ALC 0'),
          findsOneWidget);
      // RX chip from the string-enum readback.
      expect(tagOf(tester, 'RX').color, AppTheme.green);
      expect(find.text('LIVE'), findsOneWidget);
    });

    testWidgets('idle renders the session readout with dashes, no meters',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(tagOf(tester, 'IDLE').color, AppTheme.txtMute);
      // Radio-measured fields are omitted at idle — dashes, never zeros.
      expect(find.text('—'), findsWidgets);
      expect(find.textContaining('S 34'), findsNothing);
      expect(find.text('OFFLINE'), findsNothing);
    });

    testWidgets('connecting renders an amber CONNECTING tag', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'connecting');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(tagOf(tester, 'CONNECTING').color, AppTheme.amber);
    });

    testWidgets('error session renders the ERROR tag plus ERR text',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(
          sessionState: 'error', error: 'radio: login refused');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(tagOf(tester, 'ERROR').color, AppTheme.red);
      expect(find.text('ERR'), findsOneWidget);
      expect(find.text('radio: login refused'), findsOneWidget);
    });

    testWidgets('SEL marker tracks selected_vfo for both values',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(selectedVfo: 'main');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(find.text('SEL'), findsOneWidget);
      expect(
          find.descendant(of: vfoRow('MAIN'), matching: find.text('SEL')),
          findsOneWidget);
      expect(
          find.descendant(of: vfoRow('SUB'), matching: find.text('SEL')),
          findsNothing);

      store.setUhfRadio(selectedVfo: 'sub');
      await tester.pumpAndSettle();
      expect(find.text('SEL'), findsOneWidget);
      expect(
          find.descendant(of: vfoRow('SUB'), matching: find.text('SEL')),
          findsOneWidget);
      expect(
          find.descendant(of: vfoRow('MAIN'), matching: find.text('SEL')),
          findsNothing);
    });

    testWidgets('SAT tag renders from /state.satellite', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(satellite: true);

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('SAT'), findsOneWidget);
    });

    testWidgets('a type-confused /state renders dashes, never throws',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setOnline(address);
      store.applyState(address, {
        'ts': '2026-09-15T12:34:56Z',
        'freq_hz': '432100000', // string, not int
        'band': 70, // int, not string
        'mode': 5,
        'tx': true, // bool, not the string enum
        'main': 'oops', // string, not object
        'sub': 42,
        'selected_vfo': 7,
        'satellite': 'yes',
        'session_state': 42,
        'device_online': 'yes',
        'armed': 'yes',
        's_meter': 'x',
      });

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(tester.takeException(), isNull);
      expect(find.text('—'), findsWidgets);
    });
  });

  group('gating (arm on bridge liveness ONLY; PTT + tuning on armed ∧ live)',
      () {
    testWidgets(
        'idle: session readout, PTT + tuning disabled, arm/disarm ENABLED',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      // The arm loop must be reachable from the panel at healthy idle —
      // arm-while-idle is the bridge's connect trigger (R1).
      expect(btn(tester, 'uhf-arm-btn').onPressed, isNotNull);
      expect(btn(tester, 'uhf-ptt-btn').onPressed, isNull);
      for (final m in panelModes) {
        expect(btn(tester, 'uhf-main-mode-$m').onPressed, isNull);
        expect(btn(tester, 'uhf-sub-mode-$m').onPressed, isNull);
      }
      expect(btn(tester, 'uhf-main-freq-set').onPressed, isNull);
      expect(btn(tester, 'uhf-main-step-up').onPressed, isNull);
      expect(btn(tester, 'uhf-sub-freq-set').onPressed, isNull);
      expect(btn(tester, 'uhf-sub-step-down').onPressed, isNull);
    });

    testWidgets('armed + live enables PTT and the tuning controls',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: true);

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('ARMED'), findsOneWidget);
      expect(btn(tester, 'uhf-ptt-btn').onPressed, isNotNull);
      expect(btn(tester, 'uhf-main-mode-usb').onPressed, isNotNull);
      expect(btn(tester, 'uhf-sub-freq-set').onPressed, isNull, reason:
          'SET needs a parseable target; steppers and modes are enabled');
      expect(btn(tester, 'uhf-sub-step-up').onPressed, isNotNull);
    });

    testWidgets(
        'connecting: actions gated, but the arm/disarm toggle stays ENABLED',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'connecting');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(btn(tester, 'uhf-arm-btn').onPressed, isNotNull);
      expect(btn(tester, 'uhf-ptt-btn').onPressed, isNull);
      expect(btn(tester, 'uhf-main-mode-fm').onPressed, isNull);
      expect(btn(tester, 'uhf-main-freq-set').onPressed, isNull);
    });

    testWidgets(
        'unarmed PTT tap is impossible, but a bus rejection still renders ERR',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      // Idle + unarmed: the console cannot key PTT at all — but another
      // producer (the HA bridge account) can, and its rejection must land.
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(btn(tester, 'uhf-ptt-btn').onPressed, isNull);
      await tester.tap(find.byKey(const ValueKey('uhf-ptt-btn')),
          warnIfMissed: false);
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);

      store.setUhfRadio(
          sessionState: 'idle',
          error: 'ptt rejected: not armed',
          ts: '2026-09-15T12:34:57Z');
      await tester.pumpAndSettle();

      expect(find.text('ERR'), findsOneWidget);
      expect(find.text('ptt rejected: not armed'), findsOneWidget);
    });

    testWidgets(
        'healthy idle (device_online:false, /status up): no OFFLINE tag, '
        'no station fault row, arm ENABLED', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle'); // device_online derives false

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('OFFLINE'), findsNothing);
      expect(btn(tester, 'uhf-arm-btn').onPressed, isNotNull);
      // The R16 carve-out in the store: the session slot lists nowhere.
      expect(
          store.offlineList.where((e) => e.contains(address)), isEmpty);
      expect(
          store.faultHistory.where((r) => r.address == address), isEmpty);
    });

    testWidgets(
        '/status offline over a retained idle /state: OFFLINE tag, session '
        'tag suppressed, every action disabled', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');
      store.setBridgeOffline(address);

      await pumpPanel(tester, store: store, mqtt: mqtt);

      // A dead bridge leaves the retained snapshot behind — it must not
      // read as a current IDLE, and nothing may be operable.
      expect(find.text('OFFLINE'), findsOneWidget);
      expect(find.text('IDLE'), findsNothing);
      // The RX/TX chip rides the same liveness gate: a retained live
      // snapshot must not read as current radio state (review finding).
      expect(find.text('RX'), findsNothing);
      expect(find.text('TX'), findsNothing);
      expect(btn(tester, 'uhf-arm-btn').onPressed, isNull);
      expect(btn(tester, 'uhf-ptt-btn').onPressed, isNull);
      expect(btn(tester, 'uhf-main-mode-fm').onPressed, isNull);
      expect(btn(tester, 'uhf-main-step-up').onPressed, isNull);
    });
  });

  group('publishing (one-shot per-VFO value-key payloads, R9)', () {
    testWidgets(
        'arm toggle publishes arm/disarm through the cmdRetain map (no '
        'crash) and the label follows the readback, not the tap', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(find.text('ARM'), findsOneWidget);

      await tester.tap(find.byKey(const ValueKey('uhf-arm-btn')));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      final rec = mqtt.publishes.first;
      expect(rec.topic, 'muehle/uhf/radio/cmd');
      expect(rec.retain, isFalse,
          reason: 'the arm permit must never re-apply after a restart '
              '(fail-disarmed, R11) — one-shot class');
      expect(jsonDecode(rec.payload), {'action': 'arm'});
      // Pending: the toggle is inert until the readback confirms.
      expect(btn(tester, 'uhf-arm-btn').onPressed, isNull);
      // Still ARM — readback is the truth, never tap optimism (KTD15).
      expect(find.text('ARM'), findsOneWidget);

      // The first /state newer than the tap confirms: ARMED + DISARM label.
      store.setUhfRadio(
          sessionState: 'connecting',
          armed: true,
          ts: '2026-09-15T12:34:57Z');
      await tester.pumpAndSettle();
      expect(find.text('ARMED'), findsOneWidget);
      expect(find.text('DISARM'), findsOneWidget);
      expect(btn(tester, 'uhf-arm-btn').onPressed, isNotNull);

      await tester.tap(find.byKey(const ValueKey('uhf-arm-btn')));
      await tester.pumpAndSettle();
      expect(jsonDecode(mqtt.publishes.last.payload), {'action': 'disarm'});
      expect(mqtt.publishes.last.retain, isFalse);
    });

    testWidgets('SET publishes set_freq with the Hz value as a string and '
        'the targeted vfo; the mode row publishes set_mode', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: true, subMode: 'fm');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      await tester.enterText(
          find.byKey(const ValueKey('uhf-main-freq-input')), '435300000');
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('uhf-main-freq-set')));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      expect(jsonDecode(mqtt.publishes.first.payload), {
        'action': 'set_freq',
        'value': '435300000', // string — the bridge decodes Value as string
        'vfo': 'main',
      });
      expect(mqtt.publishes.first.retain, isFalse);

      await tester.tap(find.byKey(const ValueKey('uhf-sub-mode-usb')));
      await tester.pumpAndSettle();
      expect(jsonDecode(mqtt.publishes.last.payload), {
        'action': 'set_mode',
        'value': 'usb',
        'vfo': 'sub',
      });
    });

    testWidgets('steppers edit the field from the typed value, falling back '
        'to the live readback', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: true); // MAIN live at 432100000

      await pumpPanel(tester, store: store, mqtt: mqtt);

      // Empty field: step from the readback, 5 kHz quantum.
      await tester.tap(find.byKey(const ValueKey('uhf-main-step-up')));
      await tester.pumpAndSettle();
      final field =
          tester.widget<TextField>(find.byKey(const ValueKey('uhf-main-freq-input')));
      expect(field.controller!.text, '432105000');

      // Typed field: step from the typed value.
      await tester.enterText(
          find.byKey(const ValueKey('uhf-main-freq-input')), '145200000');
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('uhf-main-step-down')));
      await tester.pumpAndSettle();
      expect(
          tester
              .widget<TextField>(
                  find.byKey(const ValueKey('uhf-main-freq-input')))
              .controller!
              .text,
          '145195000');
      // Steppers only edit — they never publish.
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('an error-free republish clears the ERR indication',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(
          sessionState: 'error', error: 'radio: connection refused');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(find.text('ERR'), findsOneWidget);

      // The next successful activity republishes /state without the error.
      store.setUhfRadio(
          sessionState: 'idle', ts: '2026-09-15T12:36:20Z');
      await tester.pumpAndSettle();

      expect(find.text('ERR'), findsNothing);
      expect(find.text('radio: connection refused'), findsNothing);
    });
  });

  group('pending-confirm semantics (arm and PTT)', () {
    testWidgets(
        'PTT pending clears on the next /state and the toggle follows the '
        'tx readback', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: true); // live, rx

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('uhf-ptt-btn')));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.single.topic, 'muehle/uhf/radio/cmd');
      expect(jsonDecode(mqtt.publishes.single.payload),
          {'action': 'ptt', 'value': 'on'});
      expect(mqtt.publishes.single.retain, isFalse);
      // Pending: the toggle is inert until the readback confirms.
      expect(btn(tester, 'uhf-ptt-btn').onPressed, isNull);
      // Still RX — readback is the truth (mqtt-api.md: tx follows the radio).
      expect(find.text('RX'), findsOneWidget);

      store.setUhfRadio(
          armed: true, tx: 'tx', ts: '2026-09-15T12:34:57Z');
      await tester.pumpAndSettle();

      expect(find.text('TX'), findsOneWidget);
      expect(tagOf(tester, 'TX').color, AppTheme.red);
      expect(btn(tester, 'uhf-ptt-btn').onPressed, isNotNull);

      // The next tap toggles OFF — against the readback, not the last tap.
      await tester.tap(find.byKey(const ValueKey('uhf-ptt-btn')));
      await tester.pumpAndSettle();
      expect(jsonDecode(mqtt.publishes.last.payload),
          {'action': 'ptt', 'value': 'off'});
    });

    testWidgets('PTT pending times out to the no-confirmation ERR at 5 s',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: true, tx: 'tx'); // live, keyed

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('uhf-ptt-btn')));
      await tester.pumpAndSettle();
      expect(find.textContaining('no bus confirmation'), findsNothing);

      // No newer /state ever arrives — the local window reverts to ERR.
      await tester.pump(const Duration(seconds: 5));
      await tester.pumpAndSettle();

      expect(find.text('ERR'), findsOneWidget);
      expect(find.text('no bus confirmation (ptt)'), findsOneWidget);
      // Pending consumed: the toggle is operable again for a retry.
      expect(btn(tester, 'uhf-ptt-btn').onPressed, isNotNull);
    });

    testWidgets('a dead-bridge arm tap times out to the no-confirmation ERR '
        'at 5 s', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('uhf-arm-btn')));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.topic, 'muehle/uhf/radio/cmd');
      expect(jsonDecode(mqtt.publishes.single.payload), {'action': 'arm'});

      // The bridge dies right after the tap: the LWT lands, the arm cmd is
      // never confirmed.
      store.setBridgeOffline(address);
      await tester.pump(const Duration(seconds: 5));
      await tester.pumpAndSettle();

      expect(find.text('OFFLINE'), findsOneWidget);
      expect(find.text('ERR'), findsOneWidget);
      expect(find.text('no bus confirmation (arm)'), findsOneWidget);
    });

    testWidgets('a late confirmation after the timeout clears the ERR tag',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(armed: true);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('uhf-ptt-btn')));
      await tester.pump(const Duration(seconds: 5));
      await tester.pumpAndSettle();
      expect(find.text('no bus confirmation (ptt)'), findsOneWidget);

      // The confirmation straggles in — the newer snapshot is the truth and
      // it carries no error, so the local complaint must go.
      store.setUhfRadio(
          armed: true, tx: 'tx', ts: '2026-09-15T12:34:57Z');
      await tester.pumpAndSettle();

      expect(find.textContaining('no bus confirmation'), findsNothing);
      expect(find.text('ERR'), findsNothing);
      expect(find.text('TX'), findsOneWidget);
    });
  });
}
