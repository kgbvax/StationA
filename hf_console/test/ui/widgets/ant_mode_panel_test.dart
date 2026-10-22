import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/ui/theme.dart';
import 'package:hf_console/ui/widgets/ant_mode_panel.dart';

import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

Future<FakeMqttService> _pump(WidgetTester tester, BusStore store) async {
  await tester.binding.setSurfaceSize(const Size(600, 900));
  addTearDown(() => tester.binding.setSurfaceSize(null));
  final mqtt = FakeMqttService(store);
  await tester.pumpWidget(TestHarness(
    store: store,
    mqtt: mqtt,
    child: const SingleChildScrollView(child: AntModePanel()),
  ));
  await tester.pumpAndSettle();
  return mqtt;
}

/// The panel's local HH:MM of an RFC 3339 time.
String _hhmm(String iso) {
  final t = DateTime.parse(iso).toLocal();
  return '${t.hour.toString().padLeft(2, '0')}:${t.minute.toString().padLeft(2, '0')}';
}

/// The single published /cmd without its `ts`, after checking the ts is a
/// fresh UTC stamp (antmode drops one older than 30 s).
Map<String, dynamic> _cmd(FakeMqttService mqtt) {
  final p = jsonDecode(mqtt.publishes.single.payload) as Map<String, dynamic>;
  final ts = DateTime.parse(p.remove('ts') as String);
  expect(ts.isUtc, isTrue);
  expect(DateTime.now().toUtc().difference(ts).inSeconds.abs(), lessThan(5));
  return p;
}

ElevatedButton _button(WidgetTester tester, String key) => tester.widget<ElevatedButton>(find.byKey(ValueKey(key)));

void main() {
  group('AntModePanel', () {
    testWidgets('normal: the current mode is inert, the others ask before sending', (tester) async {
      final store = BusStore()..setAntMode('normal');
      final mqtt = await _pump(tester, store);

      expect(find.byKey(const ValueKey('ant-mode-repark')), findsNothing);
      // The current mode keeps its highlight and is a no-op.
      expect(_button(tester, 'ant-mode-normal').style!.backgroundColor!.resolve({}), AppTheme.accent);
      await tester.tap(find.byKey(const ValueKey('ant-mode-normal')));
      await tester.pumpAndSettle();
      expect(find.byType(AlertDialog), findsNothing);
      expect(mqtt.publishes, isEmpty);

      await tester.tap(find.byKey(const ValueKey('ant-mode-storm')));
      await tester.pumpAndSettle();
      expect(find.text('Enter storm mode?'), findsOneWidget);
      expect(mqtt.publishes, isEmpty, reason: 'nothing before the confirmation');

      await tester.tap(find.byKey(const ValueKey('ant-mode-confirm')));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.topic, 'muehle/hf/ant-mode/cmd');
      expect(_cmd(mqtt), {'action': 'set_ant_mode', 'value': 'storm'});
      expect(mqtt.publishes.single.retain, isTrue);
    });

    testWidgets('cancel sends nothing', (tester) async {
      final store = BusStore()..setAntMode('normal');
      final mqtt = await _pump(tester, store);

      await tester.tap(find.byKey(const ValueKey('ant-mode-maintenance')));
      await tester.pumpAndSettle();
      expect(find.text('Enter maintenance mode?'), findsOneWidget);
      await tester.tap(find.text('Cancel'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('locked: phase, park, drift and wind; REPARK asks, then sends', (tester) async {
      final store = BusStore()..setAntMode('storm');
      store.applyState('muehle/hf/ant-mode', {
        ...store.slots['muehle/hf/ant-mode']!.state!,
        'drift_deg': 2,
      });
      final mqtt = await _pump(tester, store);

      expect(find.text('Ant mode · STORM'), findsOneWidget);
      expect(find.text('PARKED · park 175° · drift +2° · manual · since ${_hhmm('2026-10-22T18:00:00Z')}'), findsOneWidget);
      expect(find.text('wind from 265° · gust 72 km/h · manned'), findsOneWidget);

      await tester.tap(find.byKey(const ValueKey('ant-mode-repark')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('ant-mode-repark-confirm')));
      await tester.pumpAndSettle();
      expect(_cmd(mqtt), {'action': 'repark'});
      expect(mqtt.publishes.single.retain, isTrue);
    });

    testWidgets('back to normal warns that active warnings are acknowledged', (tester) async {
      final store = BusStore()
        ..setAntMode('storm', warnings: [
          {'id': 'dwd.1', 'event': 'STURMBÖEN', 'ii': '52', 'onset': '', 'expires': ''},
        ]);
      final mqtt = await _pump(tester, store);

      expect(find.text('DWD STURMBÖEN'), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('ant-mode-normal')));
      await tester.pumpAndSettle();
      expect(find.textContaining('acknowledged'), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('ant-mode-confirm')));
      await tester.pumpAndSettle();
      expect(_cmd(mqtt), {'action': 'set_ant_mode', 'value': 'normal'});
    });

    testWidgets('storm suggestion: Enter storm confirms, Dismiss sends at once', (tester) async {
      final store = BusStore()..setAntMode('normal', stormSuggested: true, reason: 'DWD STURMBÖEN active, station manned');
      final mqtt = await _pump(tester, store);

      expect(find.byKey(const ValueKey('ant-mode-suggestion')), findsOneWidget);
      expect(find.text('DWD STURMBÖEN active, station manned'), findsWidgets);

      await tester.tap(find.byKey(const ValueKey('ant-mode-dismiss')));
      await tester.pumpAndSettle();
      expect(_cmd(mqtt), {'action': 'dismiss_suggestion'});

      mqtt.resetPublishes();
      await tester.tap(find.byKey(const ValueKey('ant-mode-enter-storm')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('ant-mode-confirm')));
      await tester.pumpAndSettle();
      expect(_cmd(mqtt), {'action': 'set_ant_mode', 'value': 'storm'});
    });

    testWidgets('antmode dies while the dialog is open: confirm sends nothing', (tester) async {
      final store = BusStore()..setAntMode('storm');
      final mqtt = await _pump(tester, store);

      await tester.tap(find.byKey(const ValueKey('ant-mode-normal')));
      await tester.pumpAndSettle();
      expect(find.text('Return to normal?'), findsOneWidget);
      // LWT arrives with the dialog up.
      store.applyStatus('muehle/hf/ant-mode', 'offline');
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('ant-mode-confirm')));
      await tester.pumpAndSettle();
      expect(mqtt.publishes, isEmpty, reason: 'a retained normal would wait for antmode and unlock the storm');
    });

    testWidgets('antmode offline: the lock still shows, commands are off', (tester) async {
      final store = BusStore()..setAntMode('maintenance', online: false);
      await _pump(tester, store);

      // A dead owner never unlocks: the mode is named, still enforced.
      expect(find.text('MAINTENANCE · still enforced by the bridges'), findsOneWidget);
      expect(find.text('Ant mode · OFFLINE'), findsOneWidget);
      expect(_button(tester, 'ant-mode-normal').onPressed, isNull);
      expect(_button(tester, 'ant-mode-storm').onPressed, isNull);
      expect(_button(tester, 'ant-mode-repark').onPressed, isNull);
      expect(find.textContaining('PARKED · park 175°'), findsOneWidget);
    });

    testWidgets('power_hold alone is "powerseq held", never "borrowed"', (tester) async {
      // Maintenance holds powerseq's idle logic for its whole length,
      // borrowed or not.
      final store = BusStore()..setAntMode('maintenance', powerHold: true);
      await _pump(tester, store);
      expect(find.textContaining('borrowed'), findsNothing);
      expect(find.text('powerseq held'), findsOneWidget);
    });

    testWidgets('power_borrow shows the borrow and its restore target', (tester) async {
      final store = BusStore()
        ..setAntMode('storm', phase: 'rotating', powerHold: true, powerBorrow: {
          'restore_to': 'off',
          'powerseq_phase': 'standby',
          'since': '2026-10-22T18:00:01Z',
        });
      await _pump(tester, store);
      expect(find.text('antenna power borrowed · restores off · powerseq held'), findsOneWidget);
    });

    testWidgets('a taken-over borrow is not restored', (tester) async {
      final store = BusStore()
        ..setAntMode('storm', phase: 'rotating', powerHold: true, powerBorrow: {
          'restore_to': 'off',
          'powerseq_phase': 'standby',
          'taken_over': true,
        });
      await _pump(tester, store);
      expect(find.text('antenna power borrowed · powerseq took over · powerseq held'), findsOneWidget);
    });

    testWidgets('a borrow still open back in normal stays visible', (tester) async {
      // Abort to normal with powerseq offline: the restore waits.
      final store = BusStore()
        ..setAntMode('normal', powerHold: true, powerBorrow: {'restore_to': 'off', 'powerseq_phase': 'idle'});
      await _pump(tester, store);
      expect(find.text('antenna power borrowed · restores off · powerseq held'), findsOneWidget);
    });

    testWidgets('normal without a hold or borrow shows no power line', (tester) async {
      final store = BusStore()..setAntMode('normal');
      await _pump(tester, store);
      expect(find.textContaining('borrowed'), findsNothing);
      expect(find.textContaining('powerseq held'), findsNothing);
    });

    testWidgets('a failed park shows the fault', (tester) async {
      final store = BusStore()..setAntMode('storm', phase: 'failed', fault: 'rotator never arrived');
      await _pump(tester, store);
      expect(find.text('Ant mode · PARK FAILED'), findsOneWidget);
      expect(find.text('rotator never arrived'), findsOneWidget);
    });
  });
}
