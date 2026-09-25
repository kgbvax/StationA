// uhf_radio_panel_test.dart — the receive-only IC-9700 surface (2026-09
// pivot): capture + monitor toggles and the read-only readout. The old
// control surface (arm/disarm, PTT, per-VFO tuning) is GONE — the negative
// tests below pin its absence so it cannot creep back.

import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/widgets/uhf_radio_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

const address = 'muehle/uhf/radio';

ElevatedButton btn(WidgetTester tester, String key) =>
    tester.widget<ElevatedButton>(find.byKey(ValueKey(key)));

Future<void> pumpPanel(
    WidgetTester tester, {
    required BusStore store,
    required FakeMqttService mqtt,
  }) async {
  await tester.pumpWidget(
      TestHarness(store: store, mqtt: mqtt, child: const UhfRadioPanel()));
}

void main() {
  group('readout (read-only, monitor-gated)', () {
    testWidgets('monitor on + responding renders freq, band/mode and meters',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(
          sessionState: 'live',
          audioDemand: true,
          monitor: true,
          responding: true,
          freqHz: 145800000,
          band: '2m',
          mode: 'fm',
          sMeter: 120,
          swr: 12);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.pumpAndSettle();

      expect(find.text('145.800 MHz'), findsOneWidget);
      expect(find.textContaining('2M'), findsOneWidget);
      expect(find.textContaining('FM'), findsOneWidget);
      expect(find.textContaining('S9'), findsOneWidget);
      expect(find.textContaining('SWR 12'), findsOneWidget);
    });

    testWidgets('monitor off hides the readout entirely', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.pumpAndSettle();

      expect(find.text('monitor off'), findsOneWidget);
      expect(find.textContaining('MHz'), findsNothing);
    });

    testWidgets('deaf radio (monitor on, no answer) shows the standby hint',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'live', monitor: true, responding: false);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.pumpAndSettle();

      expect(find.text('radio standby / serial down'), findsOneWidget);
      expect(find.text('STANDBY'), findsOneWidget);
      expect(find.textContaining('MHz'), findsNothing);
    });
  });

  group('toggles (one-shot, pending-confirm)', () {
    testWidgets('capture toggle publishes audio_on with retain=false',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.pumpAndSettle();

      await tester.tap(find.byKey(const ValueKey('uhf-capture-btn')));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.single.topic, 'muehle/uhf/radio/cmd');
      expect(jsonDecode(mqtt.publishes.single.payload),
          {'action': 'audio_on'});
      expect(mqtt.publishes.single.retain, isFalse);
    });

    testWidgets('capture toggle reads the readback, not the tap (KTD15)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.pumpAndSettle();

      // Radio is already capturing (another producer set it): the button
      // must publish audio_OFF despite the label state at build time.
      store.setUhfRadio(
          sessionState: 'live',
          audioDemand: true,
          ts: '2026-09-15T12:34:57Z');
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('uhf-capture-btn')));
      await tester.pumpAndSettle();

      expect(jsonDecode(mqtt.publishes.single.payload),
          {'action': 'audio_off'});
    });

    testWidgets('monitor toggle publishes monitor_on', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.pumpAndSettle();

      await tester.tap(find.byKey(const ValueKey('uhf-monitor-btn')));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.single.topic, 'muehle/uhf/radio/cmd');
      expect(jsonDecode(mqtt.publishes.single.payload),
          {'action': 'monitor_on'});
    });

    testWidgets('an unanswered toggle times out to the no-confirmation ERR',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('uhf-capture-btn')));
      await tester.pumpAndSettle();
      expect(find.text('ERR'), findsNothing);

      // No newer /state ever arrives — the local window reverts to ERR.
      await tester.pump(const Duration(seconds: 5));
      await tester.pumpAndSettle();

      expect(find.text('ERR'), findsOneWidget);
      expect(find.text('no bus confirmation (radio audio)'), findsOneWidget);
    });
  });

  group('status surface', () {
    testWidgets('healthy idle: no OFFLINE tag, capture+monitor off',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.pumpAndSettle();

      expect(find.text('OFFLINE'), findsNothing);
      expect(find.text('IDLE'), findsOneWidget);
      expect(find.text('CAPTURE'), findsNothing);
      expect(find.text('MONITOR'), findsNothing);
      expect(btn(tester, 'uhf-capture-btn').onPressed, isNotNull);
      expect(btn(tester, 'uhf-monitor-btn').onPressed, isNotNull);
    });

    testWidgets('SAT renders on the band line from the read-only satellite field',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(
          sessionState: 'live',
          monitor: true,
          responding: true,
          freqHz: 145800000,
          satellite: true);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.pumpAndSettle();

      expect(find.textContaining('SAT'), findsOneWidget);
    });

    testWidgets('dead bridge: OFFLINE tag, both toggles disabled',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(sessionState: 'idle');
      store.setBridgeOffline(address);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.pumpAndSettle();

      expect(find.text('OFFLINE'), findsOneWidget);
      expect(find.text('IDLE'), findsNothing);
      expect(btn(tester, 'uhf-capture-btn').onPressed, isNull);
      expect(btn(tester, 'uhf-monitor-btn').onPressed, isNull);
    });

    testWidgets('a bus rejection renders the ERR text verbatim',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(
          sessionState: 'idle',
          error: 'monitor unavailable: serial.device not configured');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.pumpAndSettle();

      expect(find.text('ERR'), findsOneWidget);
      expect(find.text('monitor unavailable: serial.device not configured'),
          findsOneWidget);
    });
  });

  group('control surface is GONE (receive-only posture)', () {
    testWidgets('no arm / PTT / tuning controls exist anywhere in the panel',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setUhfRadio(
          sessionState: 'live',
          audioDemand: true,
          monitor: true,
          responding: true,
          freqHz: 432650000);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.pumpAndSettle();

      for (final key in [
        'uhf-arm-btn',
        'uhf-ptt-btn',
        'uhf-main-freq-set',
        'uhf-sub-freq-set',
        'uhf-main-step-up',
        'uhf-main-mode-fm',
      ]) {
        expect(find.byKey(ValueKey(key)), findsNothing,
            reason: 'control widget $key must not exist');
      }
      for (final label in ['ARM', 'DISARM', 'PTT', 'SET']) {
        expect(find.text(label), findsNothing, reason: 'control label $label');
      }
      // And nothing in the app can produce a control payload anymore —
      // that is pinned by the wiring tests (builders deleted).
    });
  });

  test('S-meter raw 0-255 reads as S-units (Icom: S9 = 120, S9+60 = 241)', () {
    expect(sUnits(0), 'S0');
    expect(sUnits(42), 'S3');
    expect(sUnits(120), 'S9');
    expect(sUnits(160), 'S9+20');
    expect(sUnits(241), 'S9+60');
    expect(sUnits(255), 'S9+70');
  });
}
