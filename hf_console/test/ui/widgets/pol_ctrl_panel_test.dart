// pol_ctrl_panel_test.dart — widget tests for the Tier-2 X-Quad polarization
// surface (U11): four-state control (h/v/cl/cr) on the muehle/uhf/pol-ctrl
// slot published by m5stamp-pol-ctrl. Covers the plan's scenarios: the
// retained value-key set_pol payload (KTD13 contrast with the sat rotators'
// one-shot), selection rendering from relay readback (/state.pol — never
// tap optimism, KTD15), the two-layer gating (slot.isOnline AND
// store.linkUp), error surfacing (in-panel ERR + the faults-bar interplay),
// and R15: nothing binds polarization to band, tracking, or any policy.

import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/pol_ctrl_panel.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

const address = 'muehle/uhf/pol-ctrl';

void main() {
  Future<void> pumpPanel(
    WidgetTester tester, {
    required BusStore store,
    required FakeMqttService mqtt,
  }) async {
    await tester.pumpWidget(
        TestHarness(store: store, mqtt: mqtt, child: const PolCtrlPanel()));
    await tester.pumpAndSettle();
  }

  ElevatedButton button(WidgetTester tester, String pol) =>
      tester.widget<ElevatedButton>(find.byKey(ValueKey('pol-btn-$pol')));

  /// The active button paints the accent background (like the antenna
  /// panel's active port); every other state paints a non-accent one.
  bool isActive(WidgetTester tester, String pol) =>
      button(tester, pol).style?.backgroundColor?.resolve(const {}) ==
      AppTheme.accent;

  group('readback rendering (selection is relay truth, not tap optimism)', () {
    testWidgets('renders the current phase name and highlights only it',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'cl');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('CIRCULAR LEFT'), findsOneWidget);
      expect(isActive(tester, 'cl'), isTrue);
      for (final other in ['h', 'v', 'cr']) {
        expect(isActive(tester, other), isFalse);
      }
    });

    testWidgets('every vocabulary value renders its phase name', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);

      const names = {
        'h': 'HORIZONTAL',
        'v': 'VERTICAL',
        'cl': 'CIRCULAR LEFT',
        'cr': 'CIRCULAR RIGHT',
      };
      for (final entry in names.entries) {
        store.setPolCtrl(pol: entry.key);
        await pumpPanel(tester, store: store, mqtt: mqtt);
        expect(find.text(entry.value), findsOneWidget,
            reason: 'pol ${entry.key} must render ${entry.value}');
      }
    });

    testWidgets('no /state.pol renders a dash and no active button',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      // Bridge up, state up, but the snapshot carries no pol key (invalid
      // readback / pre-first-publish) — must not masquerade as any phase.
      store.setOnline(address);
      store.applyState(address, {'device_online': true});

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('—'), findsOneWidget);
      for (final pol in ['h', 'v', 'cl', 'cr']) {
        expect(isActive(tester, pol), isFalse);
      }
    });

    testWidgets('an out-of-vocabulary readback renders raw, not a guess',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'xx');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      // Unknown bus truth must be shown honestly, never silently coerced
      // to a known phase.
      expect(find.text('xx'), findsOneWidget);
      for (final pol in ['h', 'v', 'cl', 'cr']) {
        expect(isActive(tester, pol), isFalse);
      }
    });

    testWidgets('a state republish re-renders the selection (convergence)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'v');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      // The device confirms the change (relay readback republish).
      store.setPolCtrl(pol: 'cr');
      await tester.pumpAndSettle();

      expect(find.text('CIRCULAR RIGHT'), findsOneWidget);
      expect(isActive(tester, 'cr'), isTrue);
      expect(isActive(tester, 'v'), isFalse);
    });
  });

  group('gating (two-layer AND: slot.isOnline && store.linkUp)', () {
    testWidgets('controller online → all four states tappable', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'v');

      await pumpPanel(tester, store: store, mqtt: mqtt);

      for (final pol in ['h', 'v', 'cl', 'cr']) {
        expect(button(tester, pol).onPressed, isNotNull);
      }
      expect(find.text('OFFLINE'), findsNothing);
    });

    testWidgets('device_online false (AW9523 expander failed) → disabled',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'cl');
      store.setDeviceOffline(address);

      await pumpPanel(tester, store: store, mqtt: mqtt);

      for (final pol in ['h', 'v', 'cl', 'cr']) {
        expect(button(tester, pol).onPressed, isNull);
      }
    });

    testWidgets('bridge offline (LWT) → disabled (bridge layer of the AND)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'cl');
      store.setBridgeOffline(address);

      await pumpPanel(tester, store: store, mqtt: mqtt);

      for (final pol in ['h', 'v', 'cl', 'cr']) {
        expect(button(tester, pol).onPressed, isNull);
      }
    });

    testWidgets('linkUp false → inert; stale retained state stays visible but not operable',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'cl');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      store.markDisconnected();
      await tester.pumpAndSettle();

      // Retained state is still present — the exact trap: it must render
      // (operator sees the phase) but not operate.
      expect(find.text('CIRCULAR LEFT'), findsOneWidget);
      for (final pol in ['h', 'v', 'cl', 'cr']) {
        expect(button(tester, pol).onPressed, isNull);
      }

      await tester.tap(find.byKey(const ValueKey('pol-btn-cr')));
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('tap while offline publishes nothing', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'v');
      store.setBridgeOffline(address);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('pol-btn-cr')),
          warnIfMissed: false);
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);
    });
  });

  group('publishing (retained steady state — KTD13 contrast)', () {
    testWidgets('tap publishes the retained value-key set_pol payload',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'v');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('pol-btn-cl')));
      await tester.pumpAndSettle();

      expect(mqtt.publishes.length, 1);
      final rec = mqtt.publishes.first;
      expect(rec.topic, 'muehle/uhf/pol-ctrl/cmd');
      expect(rec.retain, isTrue,
          reason: 'set_pol is desired steady state, re-applied on reconnect '
              '(the deliberate contrast with the sat rotators\' one-shot)');
      expect(jsonDecode(rec.payload), {'action': 'set_pol', 'value': 'cl'});
    });

    testWidgets('each state publishes its own value', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'v');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      for (final pol in ['h', 'v', 'cl', 'cr']) {
        await tester.tap(find.byKey(ValueKey('pol-btn-$pol')));
        await tester.pumpAndSettle();
      }

      expect(mqtt.publishes.length, 4);
      final values =
          mqtt.publishes.map((r) => (jsonDecode(r.payload) as Map)['value']).toList();
      expect(values, ['h', 'v', 'cl', 'cr']);
      for (final rec in mqtt.publishes) {
        expect(rec.topic, 'muehle/uhf/pol-ctrl/cmd');
        expect(rec.retain, isTrue);
      }
    });

    testWidgets('selection does NOT move on tap — only /state moves it (KTD15)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'v');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      await tester.tap(find.byKey(const ValueKey('pol-btn-h')));
      await tester.pumpAndSettle();

      // The publish happened, but the panel must not pretend it succeeded:
      // until the relay readback republishes, V stays the selected truth.
      expect(mqtt.publishes.length, 1);
      expect(find.text('VERTICAL'), findsOneWidget);
      expect(isActive(tester, 'v'), isTrue);
      expect(isActive(tester, 'h'), isFalse);
    });
  });

  group('error surfacing (loud, never silent)', () {
    testWidgets('a rejected command surfaces as an in-panel ERR tag + text',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(
          pol: 'cl', error: "cmd rejected: invalid pol 'c1' (expected h|v|cl|cr)");

      await pumpPanel(tester, store: store, mqtt: mqtt);

      expect(find.text('ERR'), findsOneWidget);
      expect(find.textContaining("invalid pol 'c1'"), findsOneWidget);
    });

    testWidgets('the error also reaches the faults bar via the store',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(
          pol: 'cl', error: "cmd rejected: invalid pol 'c1' (expected h|v|cl|cr)");

      await pumpPanel(tester, store: store, mqtt: mqtt);

      // The store already folds /state.error into the fault history that the
      // faults bar renders — the interplay the firmware contract promises.
      final active = store.faultHistory.where((r) => r.active).toList();
      expect(active, isNotEmpty);
      expect(active.first.address, address);
      expect(active.first.text, contains('INVALID POL'));
    });

    testWidgets('a valid state change clears the error indication',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(
          pol: 'cl', error: "cmd rejected: invalid pol 'c1' (expected h|v|cl|cr)");

      await pumpPanel(tester, store: store, mqtt: mqtt);
      expect(find.text('ERR'), findsOneWidget);

      // Next valid state change republishes /state without the error field.
      store.setPolCtrl(pol: 'cr');
      await tester.pumpAndSettle();

      expect(find.text('ERR'), findsNothing);
      expect(find.text('CIRCULAR RIGHT'), findsOneWidget);
    });

    testWidgets('an error while offline still renders (diagnosis beats gating)',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(
          pol: 'cl', error: "cmd rejected: invalid pol 'c1' (expected h|v|cl|cr)");
      // Expander marked failed, but the retained snapshot still carries the
      // pending rejection — the realistic wire shape (a fresh /state with
      // device_online false, pol, and error in one snapshot).
      store.applyState(address, {
        'pol': 'cl',
        'device_online': false,
        'error': "cmd rejected: invalid pol 'c1' (expected h|v|cl|cr)",
        'ts': '2026-09-13T12:34:56Z',
      });

      await pumpPanel(tester, store: store, mqtt: mqtt);

      // Gating disables the buttons, not the truth: the operator must still
      // see why the controller is complaining.
      expect(find.text('ERR'), findsOneWidget);
      expect(button(tester, 'cl').onPressed, isNull);
    });
  });

  group('operator-driven only (R15 — no automatic binding)', () {
    testWidgets('band changes never publish set_pol', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'v');
      store.setRadio(band: '20m');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      // Radio retunes across bands — the panel rebuilds on every store
      // change; a band-follow binding would publish here.
      store.setRadio(band: '10m');
      store.setRadio(band: '2m');
      await tester.pumpAndSettle();

      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('rotator/tracking motion never publishes set_pol',
        (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'v');
      store.setSatRotator('muehle/uhf/az-rotator', axis: 'az', pos: 45);
      store.setSatRotator('muehle/uhf/el-rotator',
          axis: 'el', pos: 10, moving: true);

      await pumpPanel(tester, store: store, mqtt: mqtt);
      // A pass is being tracked: positions churn and the store notifies on
      // every snapshot. Polarization must not follow.
      store.setSatRotator('muehle/uhf/az-rotator',
          axis: 'az', pos: 120, target: 180, moving: true);
      store.setSatRotator('muehle/uhf/el-rotator',
          axis: 'el', pos: 30, target: 45, moving: true);
      await tester.pumpAndSettle();

      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('the only publish path is a button tap', (tester) async {
      final store = BusStore();
      final mqtt = FakeMqttService(store);
      store.setPolCtrl(pol: 'v');

      await pumpPanel(tester, store: store, mqtt: mqtt);
      // No interaction at all — churn the whole store, never a publish.
      store.setPower();
      store.setPaHealthy();
      store.setAntenna(selected: 'port4');
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);

      // And the one tap that does publish is exactly one set_pol.
      await tester.tap(find.byKey(const ValueKey('pol-btn-h')));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.length, 1);
      expect(jsonDecode(mqtt.publishes.single.payload),
          {'action': 'set_pol', 'value': 'h'});
    });
  });
}