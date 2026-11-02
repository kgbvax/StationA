import 'dart:convert';

import 'package:flutter/gestures.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/wiring.dart';
import 'package:hf_console/ui/widgets/dvk_panel.dart';
import 'package:shared_preferences/shared_preferences.dart';
import '../../support/fake_mqtt_service.dart';
import '../../support/fixtures.dart';
import '../../support/test_harness.dart';

void main() {
  group('DvkPanel', () {
    setUp(() => SharedPreferences.setMockInitialValues({}));

    Future<FakeMqttService> pump(WidgetTester tester, BusStore store) async {
      final mqtt = FakeMqttService(store);
      await tester.binding.setSurfaceSize(const Size(1200, 800));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      await tester.pumpWidget(TestHarness(store: store, mqtt: mqtt, child: const DvkPanel()));
      await tester.pumpAndSettle();
      return mqtt;
    }

    Finder btn(int n) => find.byKey(ValueKey('dvk-btn-$n'));

    /// A plain tap: the double-tap recognizer holds it back for its window.
    Future<void> tapKey(WidgetTester tester, int n) async {
      await tester.tap(btn(n));
      await tester.pump(kDoubleTapTimeout + const Duration(milliseconds: 20));
    }

    Future<void> doubleTapKey(WidgetTester tester, int n) async {
      await tester.tap(btn(n));
      await tester.pump(kDoubleTapMinTime + const Duration(milliseconds: 10));
      await tester.tap(btn(n));
      await tester.pumpAndSettle();
    }

    Future<void> holdKey(WidgetTester tester, int n) async {
      final g = await tester.startGesture(tester.getCenter(btn(n)));
      await tester.pump(DvkPanel.holdDuration + const Duration(milliseconds: 50));
      await g.up();
      await tester.pump();
    }

    /// The radio's /state as the bridge would publish it (tx follows playback).
    void radio(BusStore store, {bool playing = false, int id = 0, String tx = 'rx'}) {
      store.applyState('muehle/hf/radio', {
        ...store.slots['muehle/hf/radio']!.state!,
        'tx': playing ? 'tx' : tx,
        'dvk_status': playing ? 'playback' : 'idle',
        'dvk_id': playing ? id : 0,
      });
    }

    List<Map<String, dynamic>> cmds(FakeMqttService mqtt) => [
          for (final p in mqtt.publishes)
            if (p.topic == 'muehle/hf/radio/cmd') jsonDecode(p.payload) as Map<String, dynamic>,
        ];

    List<String> actions(FakeMqttService mqtt) => [for (final c in cmds(mqtt)) c['action'] as String];

    /// Ends the test with the panel (and the loop's timers) unmounted.
    Future<void> unmount(WidgetTester tester) async {
      await tester.pumpWidget(const SizedBox());
    }

    testWidgets('labels the buttons with the radio memory names', (tester) async {
      final store = BusStore();
      store.setRadio();
      store.applyState('muehle/hf/radio', {
        ...store.slots['muehle/hf/radio']!.state!,
        'dvk_memories': [
          {'id': 1, 'name': 'CQ ET', 'duration_ms': 2720},
          {'id': 2, 'name': 'dl9et', 'duration_ms': 2565},
          {'id': 3, 'name': 'Recording 3', 'duration_ms': 7445},
          {'id': 9, 'name': 'Recording 9', 'duration_ms': 0}, // beyond the 4 buttons
        ],
      });
      final mqtt = await pump(tester, store);

      expect(find.text('CQ ET'), findsOneWidget);
      expect(find.text('dl9et'), findsOneWidget);
      expect(find.text('DVK3'), findsNothing); // only memories 1–2 get buttons
      expect(find.text('Recording 3'), findsNothing);
      expect(find.text('Recording 9'), findsNothing);

      await tapKey(tester, 2);
      expect(mqtt.publishes.single.topic, 'muehle/hf/radio/cmd');
      expect(mqtt.publishes.single.payload, contains('dvk_play_2'));
    });

    testWidgets('another band sends set_band', (tester) async {
      final store = BusStore();
      store.setRadio(freqHz: 3700000, band: '80m', mode: 'lsb');
      final mqtt = await pump(tester, store);

      await tester.tap(find.widgetWithText(ElevatedButton, '40m'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.payload, radioSetBandPayload('40m'));
    });

    testWidgets('the active band toggles SSB <-> CW', (tester) async {
      final store = BusStore();
      store.setRadio(freqHz: 3700000, band: '80m', mode: 'lsb');
      final mqtt = await pump(tester, store);

      await tester.tap(find.widgetWithText(ElevatedButton, '80m'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.topic, 'muehle/hf/radio/cmd');
      expect(mqtt.publishes.single.payload, radioSetModePayload('cw'));
      expect(mqtt.publishes.single.retain, isFalse);

      store.setRadio(freqHz: 3550000, band: '80m', mode: 'cw');
      await tester.pumpAndSettle();
      mqtt.resetPublishes();
      await tester.tap(find.widgetWithText(ElevatedButton, '80m'));
      await tester.pumpAndSettle();
      expect(mqtt.publishes.single.payload, radioSetModePayload('lsb'));
    });

    testWidgets('a plain tap waits out the double-tap window, then plays', (tester) async {
      final store = BusStore()..setRadio();
      final mqtt = await pump(tester, store);

      await tester.tap(btn(1));
      await tester.pump(const Duration(milliseconds: 100));
      expect(mqtt.publishes, isEmpty, reason: 'a double tap must not transmit');
      await tester.pump(kDoubleTapTimeout);
      expect(actions(mqtt), ['dvk_play_1']);
    });

    testWidgets('a double tap opens the menu and transmits nothing', (tester) async {
      final store = BusStore()..setRadio();
      final mqtt = await pump(tester, store);

      await doubleTapKey(tester, 1);
      expect(find.text('DVK button 1'), findsOneWidget);
      expect(mqtt.publishes, isEmpty);
    });

    testWidgets('the menu binds a register; it plays on tap and survives a rebuild', (tester) async {
      final store = BusStore()..setRadio();
      store.applyState('muehle/hf/radio', {
        ...store.slots['muehle/hf/radio']!.state!,
        'dvk_memories': [
          {'id': 5, 'name': 'CQ TEST', 'duration_ms': 2000},
        ],
      });
      final mqtt = await pump(tester, store);

      await doubleTapKey(tester, 1);
      expect(find.byKey(const ValueKey('dvk-register-5')), findsOneWidget);
      await tester.tap(find.byKey(const ValueKey('dvk-register-5')));
      await tester.pump();
      await tester.tap(find.text('Save'));
      await tester.pumpAndSettle();
      expect(find.text('DVK button 1'), findsNothing);
      expect(find.text('CQ TEST'), findsOneWidget);

      await tapKey(tester, 1);
      expect(actions(mqtt), ['dvk_play_5']);

      // A new panel (layout switch, app restart) reads the binding back.
      await unmount(tester);
      mqtt.resetPublishes();
      await pump(tester, store);
      expect(find.text('CQ TEST'), findsOneWidget);
      final prefs = await SharedPreferences.getInstance();
      expect(prefs.getInt('dvk_btn1_memory'), 5);
      expect(prefs.getInt('dvk_btn2_memory'), isNull);
    });

    testWidgets('Cancel keeps the binding', (tester) async {
      final store = BusStore()..setRadio();
      final mqtt = await pump(tester, store);

      await doubleTapKey(tester, 2);
      await tester.tap(find.byKey(const ValueKey('dvk-register-7')));
      await tester.pump();
      await tester.tap(find.text('Cancel'));
      await tester.pumpAndSettle();

      await tapKey(tester, 2);
      expect(actions(mqtt), ['dvk_play_2']);
      final prefs = await SharedPreferences.getInstance();
      expect(prefs.getInt('dvk_btn2_memory'), isNull);
    });

    testWidgets('the interval stepper is bounded 1..30 s and persisted', (tester) async {
      final store = BusStore()..setRadio();
      await pump(tester, store);

      await doubleTapKey(tester, 1);
      expect(find.text('3 s'), findsOneWidget);
      for (var i = 0; i < 5; i++) {
        await tester.tap(find.byKey(const ValueKey('dvk-repeat-minus')));
        await tester.pump();
      }
      expect(find.text('1 s'), findsOneWidget);
      for (var i = 0; i < 40; i++) {
        await tester.tap(find.byKey(const ValueKey('dvk-repeat-plus')));
        await tester.pump();
      }
      expect(find.text('30 s'), findsOneWidget);
      for (var i = 0; i < 25; i++) {
        await tester.tap(find.byKey(const ValueKey('dvk-repeat-minus')));
        await tester.pump();
      }
      expect(find.text('5 s'), findsOneWidget);
      await tester.tap(find.text('Save'));
      await tester.pumpAndSettle();

      final prefs = await SharedPreferences.getInstance();
      expect(prefs.getInt('dvk_btn1_repeat_s'), 5);
    });

    testWidgets('stored bindings are read on start; junk is ignored', (tester) async {
      SharedPreferences.setMockInitialValues({
        'dvk_btn1_memory': 9,
        'dvk_btn2_memory': 99,
        'dvk_btn1_repeat_s': 0,
      });
      final store = BusStore()..setRadio();
      final mqtt = await pump(tester, store);

      expect(find.text('DVK9'), findsOneWidget);
      expect(find.text('DVK2'), findsOneWidget);
      await tapKey(tester, 1);
      expect(actions(mqtt), ['dvk_play_9']);
    });

    testWidgets('offline, a tap or hold sends nothing', (tester) async {
      final store = BusStore()..setRadio();
      final mqtt = await pump(tester, store);
      store.markDisconnected();
      await tester.pump();

      await tapKey(tester, 1);
      await holdKey(tester, 1);
      expect(mqtt.publishes, isEmpty);
      await unmount(tester);
    });

    group('auto repeat', () {
      testWidgets('hold plays now, then counts [3] [2] [1] after the message and replays', (tester) async {
        final store = BusStore()..setRadio();
        final mqtt = await pump(tester, store);

        await holdKey(tester, 1);
        expect(actions(mqtt), ['dvk_play_1']);
        expect(find.text('DVK1'), findsOneWidget, reason: 'name only while the play is starting');

        radio(store, playing: true, id: 1);
        await tester.pump();
        expect(find.text('DVK1'), findsOneWidget, reason: 'name only while on air');

        radio(store);
        await tester.pump();
        expect(find.text('DVK1 [3]'), findsOneWidget);
        await tester.pump(const Duration(seconds: 1));
        expect(find.text('DVK1 [2]'), findsOneWidget);
        await tester.pump(const Duration(seconds: 1));
        expect(find.text('DVK1 [1]'), findsOneWidget);
        await tester.pump(const Duration(seconds: 1));
        expect(actions(mqtt), ['dvk_play_1', 'dvk_play_1']);
        await unmount(tester);
      });

      testWidgets('a tap pauses to [–]; another tap resumes with a play', (tester) async {
        final store = BusStore()..setRadio();
        final mqtt = await pump(tester, store);
        await holdKey(tester, 1);
        radio(store, playing: true, id: 1);
        radio(store);
        await tester.pump();
        expect(find.text('DVK1 [3]'), findsOneWidget);

        await tapKey(tester, 1);
        expect(find.text('DVK1 [–]'), findsOneWidget);
        expect(actions(mqtt), ['dvk_play_1'], reason: 'nothing on air, nothing to stop');
        await tester.pump(const Duration(seconds: 10));
        expect(actions(mqtt), ['dvk_play_1'], reason: 'a paused loop never restarts by itself');

        await tapKey(tester, 1);
        expect(actions(mqtt), ['dvk_play_1', 'dvk_play_1']);
        expect(find.text('DVK1'), findsOneWidget);
        await unmount(tester);
      });

      testWidgets('a tap during the loop\'s own message cuts it, naming the memory', (tester) async {
        final store = BusStore()..setRadio();
        final mqtt = await pump(tester, store);
        await holdKey(tester, 2);
        radio(store, playing: true, id: 2);
        await tester.pump();

        await tapKey(tester, 2);
        expect(actions(mqtt), ['dvk_play_2', 'dvk_stop']);
        expect(cmds(mqtt).last['value'], '2');
        expect(find.text('DVK2 [–]'), findsOneWidget);
        await unmount(tester);
      });

      testWidgets('a hold leaves auto mode', (tester) async {
        final store = BusStore()..setRadio();
        final mqtt = await pump(tester, store);
        await holdKey(tester, 1);
        radio(store, playing: true, id: 1);
        radio(store);
        await tester.pump();
        expect(find.text('DVK1 [3]'), findsOneWidget);

        await holdKey(tester, 1);
        expect(find.text('DVK1'), findsOneWidget);
        await tester.pump(const Duration(seconds: 10));
        expect(actions(mqtt), ['dvk_play_1']);
        await unmount(tester);
      });

      testWidgets('only one button is in auto mode', (tester) async {
        final store = BusStore()..setRadio();
        final mqtt = await pump(tester, store);
        await holdKey(tester, 1);
        radio(store, playing: true, id: 1);
        radio(store);
        await tester.pump();
        expect(find.text('DVK1 [3]'), findsOneWidget);

        await holdKey(tester, 2);
        expect(find.text('DVK1'), findsOneWidget);
        expect(actions(mqtt), ['dvk_play_1', 'dvk_play_2']);
        await unmount(tester);
      });

      testWidgets('a foreign transmit pauses the loop', (tester) async {
        final store = BusStore()..setRadio();
        await pump(tester, store);
        await holdKey(tester, 1);
        radio(store, playing: true, id: 1);
        radio(store);
        await tester.pump();

        radio(store, tx: 'tx'); // operator PTT between plays
        await tester.pump();
        expect(find.text('DVK1 [–]'), findsOneWidget);
        await unmount(tester);
      });

      testWidgets('STOP cuts the loop\'s message and pauses it', (tester) async {
        final store = BusStore()..setRadio();
        final mqtt = await pump(tester, store);
        await holdKey(tester, 1);
        radio(store, playing: true, id: 1);
        await tester.pump();

        await tester.tap(find.widgetWithText(ElevatedButton, 'STOP'));
        await tester.pump();
        expect(find.text('DVK1 [–]'), findsOneWidget);
        expect(actions(mqtt), ['dvk_play_1', 'dvk_stop', 'dvk_stop']);
        expect(cmds(mqtt)[1]['value'], '1', reason: 'the loop names its memory');
        expect(cmds(mqtt)[2]['value'], '', reason: 'the plain stop cuts a hand-started message');
        await unmount(tester);
      });

      testWidgets('STOP in the countdown pauses without a loop stop', (tester) async {
        final store = BusStore()..setRadio();
        final mqtt = await pump(tester, store);
        await holdKey(tester, 1);
        radio(store, playing: true, id: 1);
        radio(store);
        await tester.pump();

        await tester.tap(find.widgetWithText(ElevatedButton, 'STOP'));
        await tester.pump();
        expect(find.text('DVK1 [–]'), findsOneWidget);
        await tester.pump(const Duration(seconds: 10));
        expect(actions(mqtt), ['dvk_play_1', 'dvk_stop']);
        await unmount(tester);
      });

      testWidgets('a loop survives the panel being rebuilt', (tester) async {
        final store = BusStore()..setRadio();
        final mqtt = FakeMqttService(store);
        await tester.binding.setSurfaceSize(const Size(1200, 800));
        addTearDown(() => tester.binding.setSurfaceSize(null));
        Widget panel(Key k) => TestHarness(store: store, mqtt: mqtt, child: DvkPanel(key: k));

        await tester.pumpWidget(panel(const ValueKey('a')));
        await tester.pumpAndSettle();
        await holdKey(tester, 1);
        radio(store, playing: true, id: 1);
        radio(store);
        await tester.pump();
        expect(find.text('DVK1 [3]'), findsOneWidget);

        // Same harness (same controller), new panel state: the layout switch.
        await tester.pumpWidget(panel(const ValueKey('b')));
        await tester.pumpAndSettle();
        expect(find.text('DVK1 [3]'), findsOneWidget);
        await unmount(tester);
      });

      testWidgets('saving the menu of the armed button reaches the loop', (tester) async {
        final store = BusStore()..setRadio();
        final mqtt = await pump(tester, store);
        await holdKey(tester, 1);
        radio(store, playing: true, id: 1);
        radio(store);
        await tester.pump();

        await doubleTapKey(tester, 1);
        await tester.tap(find.byKey(const ValueKey('dvk-register-6')));
        await tester.pump();
        await tester.tap(find.text('Save'));
        await tester.pumpAndSettle();
        expect(find.text('DVK6 [3]'), findsOneWidget, reason: 'a countdown in progress is not restarted');

        await tester.pump(const Duration(seconds: 3));
        expect(actions(mqtt), ['dvk_play_1', 'dvk_play_6']);
        await unmount(tester);
      });
    });

    testWidgets('falls back to DVK<n> before the radio reports names', (tester) async {
      final store = BusStore();
      store.setRadio();
      await pump(tester, store);

      expect(find.text('DVK1'), findsOneWidget);
      expect(find.text('DVK2'), findsOneWidget);
      expect(find.text('DVK3'), findsNothing);
    });
  });

  test('toggledSsbCwMode picks the sideband by frequency', () {
    expect(toggledSsbCwMode('cw', 3550000), 'lsb'); // 80m
    expect(toggledSsbCwMode('cw', 7030000), 'lsb'); // 40m
    expect(toggledSsbCwMode('cw', 10000000), 'usb'); // 10 MHz and up: USB
    expect(toggledSsbCwMode('cw', 14025000), 'usb'); // 20m
    expect(toggledSsbCwMode('cw', null), 'usb');
    expect(toggledSsbCwMode('lsb', 3700000), 'cw');
    expect(toggledSsbCwMode('usb', 14200000), 'cw');
    expect(toggledSsbCwMode('data', 14074000), 'cw');
    expect(toggledSsbCwMode(null, null), 'cw');
  });

  test('memoryNames ignores malformed input', () {
    expect(DvkPanel.memoryNames(null), isEmpty);
    expect(DvkPanel.memoryNames('x'), isEmpty);
    expect(DvkPanel.memoryNames([1, {'id': '1', 'name': 'a'}, {'id': 2}]), isEmpty);
    expect(DvkPanel.memoryNames([{'id': 3, 'name': ' CQ '}, {'id': 4, 'name': '  '}]), {3: 'CQ'});
  });
}
