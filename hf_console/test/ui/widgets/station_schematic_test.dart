import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/station_schematic.dart';
import 'package:hf_console/store/wiring.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/station_schematic.dart';

import '../../support/fixtures.dart';
import '../../support/station_fixtures.dart';
import '../../support/test_harness.dart';

Future<BusStore> _pump(WidgetTester tester, StationScenario scenario,
    {Size pane = const Size(747, 770), BusStore? store}) async {
  await tester.binding.setSurfaceSize(Size(pane.width + 20, pane.height + 20));
  final s = store ?? (BusStore()..seedScenario(scenario));
  await tester.pumpWidget(TestHarness(
    store: s,
    child: Center(child: SizedBox(width: pane.width, height: pane.height, child: const StationSchematic())),
  ));
  if (scenario == StationScenario.linkDown) s.markDisconnected();
  await tester.pump();
  return s;
}

void main() {
  tearDown(() => AppTheme.setScheme(AppColorScheme.dc));

  testWidgets('a running station shows its live readings', (tester) async {
    await _pump(tester, StationScenario.running);
    for (final t in [
      'FLEX-8400', '14.074.000', 'DATA', 'RX', 'ACOM', 'OPER', 'ATR-1000', 'INLINE', '13.8 V', 'AUTO',
      'Ultrabeam', '20 m Forward', '212°', '145.212.500', 'IDLE', '200°', 'AWAKE',
      '13/13', '1/1', 'TRX ON', 'PA ON', 'ARMED', 'NO PASS',
    ]) {
      expect(find.text(t), findsOneWidget, reason: t);
    }
    // Three plugs (relay word until the bridge publishes apower), the
    // sequencer in the power panel's vocabulary, beam steer.
    expect(find.text('ON'), findsNWidgets(5));
    // No card for this console and no static text: a healthy link is not
    // news (a dead one is the LINK DOWN banner).
    for (final t in ['This console', 'RX ONLY', 'Logger PC', ':1883']) {
      expect(find.text(t), findsNothing, reason: t);
    }
    expect(find.text('RHCP'), findsNWidgets(2)); // PLC #2 and the X-Quad caption
  });

  testWidgets('a plug shows its live draw once the bridge publishes apower', (tester) async {
    final s = BusStore()..seedStation();
    s.applyState('muehle/power/psu-13v8', {'device_online': true, 'power': 'on', 'apower': 64.4});
    await _pump(tester, StationScenario.running, store: s);
    expect(find.text('64 W'), findsOneWidget);
  });

  testWidgets('an unreachable plug leaves its bus unknown, not powered', (tester) async {
    final s = BusStore()..seedStation();
    s.setDeviceOffline('muehle/power/psu-13v8');
    await _pump(tester, StationScenario.running, store: s);
    expect(find.text('13.8 V'), findsNothing);
    expect(find.text('NO LINK'), findsOneWidget); // the PSU card: its plug is unreachable
  });

  testWidgets('TX shows the carrier and forward power', (tester) async {
    await _pump(tester, StationScenario.tx);
    expect(find.text('TX'), findsOneWidget);
    expect(find.text('812'), findsOneWidget);
    expect(find.text('1.29'), findsOneWidget);
    expect(find.text('AO-91'), findsOneWidget);
    expect(find.text('EL 18°'), findsOneWidget);
  });

  testWidgets('faults: bridge down, device lost and the PA trip read as such', (tester) async {
    await _pump(tester, StationScenario.faults);
    expect(find.text('FAULT HIGH SWR'), findsOneWidget);
    expect(find.text('2.90'), findsOneWidget); // the trip reading
    expect(find.text('BYPASS'), findsOneWidget);
    expect(find.text('MANUAL HOLD'), findsOneWidget);
    expect(find.text('OFFLINE'), findsOneWidget); // RCU-06, bridge down
    expect(find.text('NO LINK'), findsOneWidget); // GS-500, device lost
    expect(find.text('no data'), findsOneWidget); // Ultrabeam sub-line
    expect(find.text('12/13'), findsOneWidget);
  });

  testWidgets('standby: antenna side unpowered, PC asleep', (tester) async {
    await _pump(tester, StationScenario.standby);
    expect(find.text('STANDBY'), findsOneWidget);
    expect(find.text('off'), findsNWidgets(4)); // G-450, RCU-06, SPID, GS-500
    expect(find.text('unpowered'), findsOneWidget);
    expect(find.text('ASLEEP'), findsOneWidget);
    expect(find.text('AUTO, IDLE'), findsOneWidget);
  });

  testWidgets('PSU off: the 13.8 V side reads OFF, not as a fault', (tester) async {
    await _pump(tester, StationScenario.psuOff);
    expect(find.text('0 V'), findsOneWidget);
    // FLEX, ACOM (remote-on dead with PLC #1), ATR and IC-9700 value rows +
    // the PSU plug.
    expect(find.text('OFF'), findsNWidgets(5));
    expect(find.text('OFFLINE'), findsNothing);
    expect(find.text('UNREACHABLE'), findsNothing);
    // PLC #1 is dead: its last snapshot must not read as closed relays.
    expect(find.text('TRX ON'), findsNothing);
    expect(find.text('TRX ?'), findsOneWidget);
    expect(find.text('STANDBY'), findsOneWidget); // the deaf IC-9700
  });

  testWidgets('link down: values stay last-known, the PC state is unknown', (tester) async {
    await _pump(tester, StationScenario.linkDown);
    expect(find.text('UNKNOWN'), findsOneWidget);
    expect(find.text('14.074.000'), findsOneWidget);
  });

  testWidgets('tapping a block opens its details; tap again or close dismisses', (tester) async {
    await _pump(tester, StationScenario.running);
    await tester.tap(find.byKey(const ValueKey('schematic-hit-muehle/hf/radio')));
    await tester.pump();
    expect(find.byKey(const ValueKey('schematic-popover')), findsOneWidget);
    expect(find.text('muehle/hf/radio'), findsOneWidget);
    expect(find.text('flexbridge'), findsOneWidget);
    expect(find.text('scmino'), findsWidgets);
    expect(find.text('freq_hz'), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('schematic-popover-close')));
    await tester.pump();
    expect(find.byKey(const ValueKey('schematic-popover')), findsNothing);

    await tester.tap(find.byKey(const ValueKey('schematic-hit-muehle/hf/radio')));
    await tester.pump();
    await tester.tap(find.byKey(const ValueKey('schematic-hit-muehle/hf/radio')));
    await tester.pump();
    expect(find.byKey(const ValueKey('schematic-popover')), findsNothing);
  });

  testWidgets('a tap inside the details keeps them open, a tap on the sheet closes them', (tester) async {
    await _pump(tester, StationScenario.running);
    await tester.tap(find.byKey(const ValueKey('schematic-hit-muehle/hf/pa')));
    await tester.pump();
    await tester.tap(find.text('muehle/hf/pa'));
    await tester.pump();
    expect(find.byKey(const ValueKey('schematic-popover')), findsOneWidget);
    await tester.tapAt(tester.getTopLeft(find.byType(StationSchematic)) + const Offset(4, 740));
    await tester.pump();
    expect(find.byKey(const ValueKey('schematic-popover')), findsNothing);
  });

  testWidgets('host details list the bridges on that host', (tester) async {
    await _pump(tester, StationScenario.faults);
    await tester.tap(find.byKey(const ValueKey('schematic-hit-host:scmino')));
    await tester.pump();
    expect(find.text('ultrabridge'), findsOneWidget);
    expect(find.text('down'), findsOneWidget);
  });

  testWidgets('every tap target renders at least 48 dp, even on a small pane', (tester) async {
    await _pump(tester, StationScenario.running, pane: const Size(520, 540));
    final targets = find.byWidgetPredicate((w) => w.key is ValueKey<String> &&
        (w.key! as ValueKey<String>).value.startsWith('schematic-hit-'));
    expect(targets.evaluate().length, greaterThan(20));
    for (final e in targets.evaluate()) {
      // Global rect: the sheet is scaled by a FittedBox.
      final r = tester.getRect(find.byWidget(e.widget));
      expect(r.width, greaterThanOrEqualTo(47.9), reason: '${e.widget.key}');
      expect(r.height, greaterThanOrEqualTo(47.9), reason: '${e.widget.key}');
    }
  });

  testWidgets('Escape closes the details', (tester) async {
    await _pump(tester, StationScenario.running);
    await tester.tap(find.byKey(const ValueKey('schematic-hit-muehle/hf/radio')));
    await tester.pump();
    expect(find.byKey(const ValueKey('schematic-popover')), findsOneWidget);
    await tester.sendKeyEvent(LogicalKeyboardKey.escape);
    await tester.pump();
    expect(find.byKey(const ValueKey('schematic-popover')), findsNothing);
  });

  testWidgets('the PA arm has its own details', (tester) async {
    final s = BusStore()..seedStation();
    s.applyState('muehle/hf/pa-arm', {'device_online': true, 'enabled': true, 'armed': false, 'error': 'FEED STALE'});
    await _pump(tester, StationScenario.running, store: s);
    expect(find.text('SAFE'), findsOneWidget);
    await tester.tap(find.byKey(const ValueKey('schematic-hit-muehle/hf/pa-arm')));
    await tester.pump();
    expect(find.text('PLC #1 PA arm'), findsOneWidget);
    expect(find.text('FEED STALE'), findsWidgets);
  });

  testWidgets('the PSU block has its own details', (tester) async {
    await _pump(tester, StationScenario.running);
    await tester.tap(find.byKey(const ValueKey('schematic-hit-psu:13v8')));
    await tester.pump();
    expect(find.text('13.8 V TRX PSU'), findsOneWidget);
    expect(find.text('shelly-power-bridge'), findsOneWidget);
  });

  testWidgets('a sleeping PC agent is asleep in the host details, not down', (tester) async {
    await _pump(tester, StationScenario.standby);
    await tester.tap(find.byKey(const ValueKey('schematic-hit-host:bwpc')));
    await tester.pump();
    expect(find.text('asleep'), findsWidgets);
    expect(find.text('down'), findsNothing);
  });

  testWidgets('a dead antenna switch shows its alarm dot and no wiper', (tester) async {
    final s = BusStore()..seedStation();
    s.applyStatus('muehle/hf/ant-switch', 'offline');
    await _pump(tester, StationScenario.running, store: s);
    final m = StationSchematicModel(s);
    expect(m.severity('muehle/hf/ant-switch'), Severity.alarm);
    final w = schematicWires(m);
    expect(w.port, isNull);
    expect(w.switchAlarm, isTrue);
  });

  test('wires follow the scenario', () {
    SchematicWires wires(StationScenario sc) {
      final s = BusStore()..markConnected(scheduleGraceNotify: false);
      s.seedScenario(sc);
      if (sc == StationScenario.linkDown) s.markDisconnected();
      return schematicWires(StationSchematicModel(s));
    }

    final run = wires(StationScenario.running);
    expect((run.port, run.rf, run.carrier, run.psu, run.ant, run.pc), ('port4', true, false, true, true, true));
    expect(run.direction, 'forward');
    expect(wires(StationScenario.tx).carrier, isTrue);
    final off = wires(StationScenario.psuOff);
    expect((off.psu, off.rf, off.port, off.uhfRf), (false, false, null, false));
    final faults = wires(StationScenario.faults);
    expect(faults.port, 'port6');
    expect(faults.direction, isNull, reason: 'RCU-06 bridge down: no arrow from stale state');
    expect(wires(StationScenario.standby).ant, isFalse);
    final down = wires(StationScenario.linkDown);
    expect((down.link, down.psu, down.carrier), (false, false, false));
  });

  test('the drawn switch ports are exactly the console antenna map', () {
    expect(schematicPorts.map((p) => p.port).toSet(), antennaMap.keys.toSet());
  });

  testWidgets('renders without exceptions in every scheme, scenario and pane size', (tester) async {
    for (final scheme in AppColorScheme.values) {
      AppTheme.setScheme(scheme);
      for (final scenario in StationScenario.values) {
        for (final pane in const [Size(747, 770), Size(560, 760), Size(900, 500)]) {
          await _pump(tester, scenario, pane: pane);
          expect(tester.takeException(), isNull, reason: '$scheme $scenario $pane');
        }
      }
    }
  });

  testWidgets('an empty bus renders (fresh connect, nothing retained yet)', (tester) async {
    await _pump(tester, StationScenario.running, store: BusStore());
    expect(tester.takeException(), isNull);
    expect(find.text('FLEX-8400'), findsOneWidget);
  });
}
