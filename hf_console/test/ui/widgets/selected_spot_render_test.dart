import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/wiring.dart';
import 'package:hf_console/ui/widgets/compass_panel.dart';
import 'package:hf_console/ui/widgets/mercator_map_panel.dart';

import '../../support/fake_mqtt_service.dart';
import '../../support/test_harness.dart';

// The selected-station marker: muehle/hf/spots /state carries the station the
// operator keyed in the shack logger (logger-spot-bridge); the maps render a
// pin + callsign (Mercator) / pin or bearing ray + callsign + chip (compass).
// These tests drive the same path the MQTT service uses — BusStore.apply.

void _applySelected(BusStore store, Map<String, dynamic>? selected) {
  final state = <String, dynamic>{
    'ts': '2026-09-15T14:03:12Z',
    'device_online': true,
    if (selected != null) 'selected': selected,
  };
  store.apply('muehle/hf/spots/state', jsonEncode(state), false);
}

void _bringRotorOnline(BusStore store) {
  store.apply('muehle/hf/rotator/status', 'online', true);
  store.apply(
      'muehle/hf/rotator/state',
      jsonEncode({
        'az': 10.0,
        'ts': '2026-09-15T14:03:12Z',
      }),
      false);
}

void main() {
  testWidgets('Mercator map renders with a keyed selection (coordinates path)', (tester) async {
    final store = BusStore();
    _applySelected(store, {
      'call': 'VK9XY',
      'freq_hz': 14024760,
      'band': '20m',
      'mode': 'cw',
      'lat': -11.53,
      'lng': 151.19,
      'azimuth': 62.4,
      'distance_km': 15420.3,
      'source': 'dxlog',
      'ts': DateTime.now().toUtc().toIso8601String(),
    });

    await tester.pumpWidget(TestHarness(store: store, child: const MercatorMapPanel()));
    await tester.pump(const Duration(milliseconds: 600));
    expect(tester.takeException(), isNull);
  });

  testWidgets('Compass renders with a keyed selection (azimuth-only path)', (tester) async {
    final store = BusStore();
    _applySelected(store, {
      'call': 'VK9XY',
      'band': '20m',
      'azimuth': 62.4,
      'distance_km': 15420.3,
      'source': 'dxlog',
      'ts': DateTime.now().toUtc().toIso8601String(),
    });

    await tester.pumpWidget(TestHarness(store: store, child: const CompassPanel()));
    await tester.pump(const Duration(milliseconds: 600));
    expect(tester.takeException(), isNull);
  });

  testWidgets('A stale selection renders dimmed, not thrown out', (tester) async {
    final store = BusStore();
    _applySelected(store, {
      'call': 'VK9XY',
      'azimuth': 62.4,
      'source': 'dxlog',
      // 8 minutes old: past the 5-minute dim, before the 15-minute hide.
      'ts': DateTime.now().toUtc().subtract(const Duration(minutes: 8)).toIso8601String(),
    });

    await tester.pumpWidget(TestHarness(store: store, child: const CompassPanel()));
    await tester.pump(const Duration(milliseconds: 600));
    expect(tester.takeException(), isNull);
  });

  testWidgets('Cleared selection (selected omitted) renders nothing', (tester) async {
    final store = BusStore();
    _applySelected(store, null);

    await tester.pumpWidget(TestHarness(store: store, child: const MercatorMapPanel()));
    await tester.pump(const Duration(milliseconds: 600));
    expect(tester.takeException(), isNull);
  });

  // --- Tap-to-aim: the chip is the actuator, not just a read-out ---

  testWidgets('Tapping the selected chip aims the rotor at the keyed station', (tester) async {
    final store = BusStore();
    final mqtt = FakeMqttService(store);
    _bringRotorOnline(store);
    _applySelected(store, {
      'call': 'VK9XY',
      'band': '20m',
      'azimuth': 62.4,
      'distance_km': 15420.3,
      'source': 'log4om',
      'ts': DateTime.now().toUtc().toIso8601String(),
    });

    await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
    await tester.pump(const Duration(milliseconds: 600));

    // Aimable: the bearing segment carries the target marker.
    expect(find.textContaining('→ 62°'), findsOneWidget);

    await tester.tap(find.textContaining('VK9XY'));
    await tester.pump();

    expect(mqtt.publishes, hasLength(1));
    expect(mqtt.publishes.single.topic, cmdTopic('hf/rotator'));
    expect(mqtt.publishes.single.payload, rotatorAzPayload(62.4));
  });

  testWidgets('Chip tap target is at least 48 dp tall', (tester) async {
    final store = BusStore();
    final mqtt = FakeMqttService(store);
    _bringRotorOnline(store);
    _applySelected(store, {
      'call': 'VK9XY',
      'azimuth': 62.4,
      'source': 'log4om',
      'ts': DateTime.now().toUtc().toIso8601String(),
    });

    await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
    await tester.pump(const Duration(milliseconds: 600));

    final chipFinder = find.ancestor(
      of: find.textContaining('VK9XY'),
      matching: find.byType(GestureDetector),
    ).first;
    expect(tester.getSize(chipFinder).height, greaterThanOrEqualTo(48.0));
  });

  testWidgets('Rotor offline: chip renders without the aim marker and tap publishes nothing', (tester) async {
    final store = BusStore();
    final mqtt = FakeMqttService(store);
    _applySelected(store, {
      'call': 'VK9XY',
      'azimuth': 62.4,
      'source': 'log4om',
      'ts': DateTime.now().toUtc().toIso8601String(),
    });

    await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const CompassPanel()));
    await tester.pump(const Duration(milliseconds: 600));

    expect(find.textContaining('→ 62°'), findsNothing);
    expect(find.textContaining('62°'), findsOneWidget);

    await tester.tap(find.textContaining('VK9XY'), warnIfMissed: false);
    await tester.pump();
    expect(mqtt.publishes, isEmpty);
  });
}
