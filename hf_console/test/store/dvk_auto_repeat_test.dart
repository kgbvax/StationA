import 'dart:convert';

import 'package:fake_async/fake_async.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/dvk_auto_repeat.dart';

import '../support/fake_mqtt_service.dart';
import '../support/fixtures.dart';

const _radio = 'muehle/hf/radio';

/// A controller on a link-up store with an online, idle, receiving radio.
class _Rig {
  final store = BusStore();
  late final FakeMqttService mqtt = FakeMqttService(store);
  late final DvkAutoRepeat auto = DvkAutoRepeat(store, mqtt);

  _Rig() {
    // No grace timer: fakeAsync must see no timer the controller did not start.
    store.markConnected(scheduleGraceNotify: false);
    radio();
  }

  /// The radio's /state as the bridge would publish it.
  void radio({String tx = 'rx', String dvk = 'idle', int id = 0, bool tuning = false}) {
    store.applyStatus(_radio, 'online');
    store.applyState(_radio, {
      'tx': tx,
      'tuning': tuning,
      'dvk_status': dvk,
      'dvk_id': id,
      'device_online': true,
    });
  }

  /// The radio reports memory [id] on air (status + keyed together).
  void playback(int id) => radio(tx: 'tx', dvk: 'playback', id: id);

  /// Everything on `hf/radio/cmd` as the action names, in order.
  List<String> get actions => [
        for (final p in mqtt.publishes)
          if (p.topic == 'muehle/hf/radio/cmd') (jsonDecode(p.payload) as Map)['action'] as String,
      ];

  /// The `value` of every `dvk_stop` on `hf/radio/cmd`, in order. A bridge
  /// resolves an empty one from its own state, which says nothing until the
  /// radio has reported the playback, so the loop must name the memory.
  List<Object?> get stopValues => [
        for (final p in mqtt.publishes)
          if (p.topic == 'muehle/hf/radio/cmd' && (jsonDecode(p.payload) as Map)['action'] == 'dvk_stop')
            (jsonDecode(p.payload) as Map)['value'],
      ];

  void dispose() => auto.dispose();
}

void main() {
  test('commands are published on the radio cmd topic, not retained', () {
    final rig = _Rig();
    rig.auto.enter(1, 1, 3);
    expect(rig.mqtt.publishes, hasLength(1));
    expect(rig.mqtt.publishes.single.topic, 'muehle/hf/radio/cmd');
    expect(rig.mqtt.publishes.single.retain, isFalse);
    rig.dispose();
  });

  test('idle controller has no button and no phase', () {
    final rig = _Rig();
    expect(rig.auto.button, isNull);
    expect(rig.auto.phase, isNull);
    expect(rig.auto.remaining, 0);
    expect(rig.auto.isAuto(1), isFalse);
    rig.dispose();
  });

  group('cycle', () {
    test('play -> playback -> idle -> countdown 3,2,1 -> replay', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 4, 3);
        expect(rig.auto.button, 1);
        expect(rig.auto.isAuto(1), isTrue);
        expect(rig.auto.isAuto(2), isFalse);
        expect(rig.auto.phase, DvkAutoPhase.starting);
        expect(rig.actions, ['dvk_play_4']);

        // The loop's own transmission is expected and must not pause it.
        rig.playback(4);
        expect(rig.auto.phase, DvkAutoPhase.playing);
        async.elapse(const Duration(seconds: 10));
        expect(rig.auto.phase, DvkAutoPhase.playing);
        expect(rig.actions, ['dvk_play_4']);

        rig.radio();
        expect(rig.auto.phase, DvkAutoPhase.countdown);
        expect(rig.auto.remaining, 3);

        async.elapse(const Duration(seconds: 1));
        expect(rig.auto.remaining, 2);
        async.elapse(const Duration(seconds: 1));
        expect(rig.auto.remaining, 1);
        expect(rig.actions, ['dvk_play_4']);

        async.elapse(const Duration(seconds: 1));
        expect(rig.auto.phase, DvkAutoPhase.starting);
        expect(rig.auto.remaining, 0);
        expect(rig.actions, ['dvk_play_4', 'dvk_play_4']);

        // Second cycle runs the same way.
        rig.playback(4);
        rig.radio();
        expect(rig.auto.phase, DvkAutoPhase.countdown);
        expect(rig.auto.remaining, 3);

        rig.auto.exit();
        rig.dispose();
        expect(async.pendingTimers, isEmpty);
      });
    });

    test('a one second repeat interval counts 1 then replays', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 1);
        rig.playback(1);
        rig.radio();
        expect(rig.auto.remaining, 1);
        async.elapse(const Duration(seconds: 1));
        expect(rig.actions, ['dvk_play_1', 'dvk_play_1']);
        rig.auto.exit();
        rig.dispose();
      });
    });

    test('notifies on every visible change', () {
      fakeAsync((async) {
        final rig = _Rig();
        var n = 0;
        rig.auto.addListener(() => n++);
        rig.auto.enter(1, 1, 2);
        rig.playback(1);
        rig.radio();
        final before = n;
        async.elapse(const Duration(seconds: 1));
        expect(n, greaterThan(before), reason: 'the [n] label must repaint each second');
        rig.auto.exit();
        rig.dispose();
      });
    });
  });

  group('foreign transmit', () {
    test('operator TX during the countdown pauses', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        async.elapse(const Duration(seconds: 1));
        expect(rig.auto.phase, DvkAutoPhase.countdown);

        rig.radio(tx: 'tx'); // PTT, nothing on the DVK
        expect(rig.auto.phase, DvkAutoPhase.paused);
        expect(rig.auto.button, 1, reason: 'stays armed');
        expect(rig.auto.remaining, 0);

        rig.radio();
        async.elapse(const Duration(seconds: 30));
        expect(rig.actions, ['dvk_play_1'], reason: 'a pause never resumes by itself');
        expect(rig.auto.phase, DvkAutoPhase.paused);
        expect(async.pendingTimers, isEmpty);
        rig.dispose();
      });
    });

    test('PA keyed during the countdown pauses', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        rig.store.setPaTransmitting();
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.dispose();
      });
    });

    test('an ATU tune carrier during the countdown pauses', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        rig.radio(tuning: true);
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.dispose();
      });
    });

    test('TX still on 1 s after the message ended pauses', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio(tx: 'tx'); // dvk idle, carrier held (PTT aborted the message)
        expect(rig.auto.phase, DvkAutoPhase.countdown);

        async.elapse(const Duration(milliseconds: 999));
        expect(rig.auto.phase, DvkAutoPhase.countdown, reason: 'grace not over yet');
        async.elapse(const Duration(milliseconds: 1));
        expect(rig.auto.phase, DvkAutoPhase.paused);

        async.elapse(const Duration(seconds: 30));
        expect(rig.actions, ['dvk_play_1']);
        expect(async.pendingTimers, isEmpty);
        rig.dispose();
      });
    });

    test('a TX tail shorter than 1 s keeps the loop going', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio(tx: 'tx');
        expect(rig.auto.phase, DvkAutoPhase.countdown);

        async.elapse(const Duration(milliseconds: 400));
        rig.radio(); // tail over
        expect(rig.auto.phase, DvkAutoPhase.countdown);

        async.elapse(const Duration(milliseconds: 600));
        expect(rig.auto.remaining, 2);
        async.elapse(const Duration(seconds: 2));
        expect(rig.actions, ['dvk_play_1', 'dvk_play_1']);
        expect(rig.auto.phase, DvkAutoPhase.starting);
        rig.auto.exit();
        rig.dispose();
      });
    });

    test('a key-up after the tail ended is a fresh rising edge', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio(tx: 'tx');
        async.elapse(const Duration(milliseconds: 200));
        rig.radio();
        async.elapse(const Duration(milliseconds: 200));
        rig.radio(tx: 'tx');
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.dispose();
      });
    });

    test('another memory played by hand pauses a running loop', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        expect(rig.auto.phase, DvkAutoPhase.playing);
        rig.playback(2);
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.dispose();
      });
    });

    test('a hand-started playback during the countdown pauses', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        rig.playback(2);
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.dispose();
      });
    });

    test('another memory on air instead of ours pauses at start', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(2);
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.dispose();
      });
    });

    test('the operator recording a memory pauses the countdown', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        rig.radio(dvk: 'recording', id: 7);
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.dispose();
      });
    });
  });

  group('play guards', () {
    test('enter while TX is up arms without playing', () {
      final rig = _Rig();
      rig.radio(tx: 'tx');
      rig.auto.enter(1, 1, 3);
      expect(rig.auto.button, 1);
      expect(rig.auto.phase, DvkAutoPhase.paused);
      expect(rig.mqtt.publishes, isEmpty);
      rig.dispose();
    });

    test('enter while the radio is offline arms without playing', () {
      final rig = _Rig();
      rig.store.setBridgeOffline(_radio);
      rig.auto.enter(1, 1, 3);
      expect(rig.auto.phase, DvkAutoPhase.paused);
      expect(rig.mqtt.publishes, isEmpty);
      rig.dispose();
    });

    test('enter while the DVK is busy arms without playing', () {
      final rig = _Rig();
      rig.radio(dvk: 'playback', id: 3);
      rig.auto.enter(1, 1, 3);
      expect(rig.auto.phase, DvkAutoPhase.paused);
      expect(rig.mqtt.publishes, isEmpty);
      rig.dispose();
    });

    test('resume while TX is up stays paused and sends nothing', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.auto.pause();
        rig.radio(tx: 'tx');
        rig.auto.resume();
        expect(rig.auto.phase, DvkAutoPhase.paused);
        expect(rig.actions, ['dvk_play_1']);
        expect(async.pendingTimers, isEmpty);
        rig.dispose();
      });
    });

    test('a carrier that appears between ticks stops the replay', () {
      // Tick checks run on the live bus, not on what the countdown began with.
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 2);
        rig.playback(1);
        rig.radio();
        async.elapse(const Duration(seconds: 1));
        // Mutate the store without notifying a rising edge the listener could
        // see: silent write to the slot map, as a stale-snapshot stand-in.
        rig.store.slots[_radio]!.state!['tx'] = 'tx';
        async.elapse(const Duration(seconds: 1));
        expect(rig.actions, ['dvk_play_1']);
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.dispose();
      });
    });
  });

  group('start', () {
    test('no playback within 3 s pauses', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        async.elapse(const Duration(milliseconds: 2999));
        expect(rig.auto.phase, DvkAutoPhase.starting);
        async.elapse(const Duration(milliseconds: 1));
        expect(rig.auto.phase, DvkAutoPhase.paused);
        expect(rig.auto.button, 1);

        async.elapse(const Duration(seconds: 30));
        expect(rig.actions, ['dvk_play_1'], reason: 'no blind retry');
        expect(async.pendingTimers, isEmpty);
        rig.dispose();
      });
    });

    test('playback within 3 s cancels the timeout', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        async.elapse(const Duration(seconds: 2));
        rig.playback(1);
        async.elapse(const Duration(seconds: 5));
        expect(rig.auto.phase, DvkAutoPhase.playing);
        rig.auto.exit();
        rig.dispose();
      });
    });

    test('the tx bit before dvk_status does not pause a starting play', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.radio(tx: 'tx'); // our own playback keying up, status not yet seen
        expect(rig.auto.phase, DvkAutoPhase.starting);
        rig.playback(1);
        expect(rig.auto.phase, DvkAutoPhase.playing);
        rig.auto.exit();
        rig.dispose();
      });
    });
  });

  group('single active button', () {
    test('enter on button 2 exits button 1', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        expect(rig.auto.phase, DvkAutoPhase.countdown);

        rig.auto.enter(2, 2, 5);
        expect(rig.auto.button, 2);
        expect(rig.auto.isAuto(1), isFalse);
        expect(rig.auto.isAuto(2), isTrue);
        expect(rig.actions, ['dvk_play_1', 'dvk_play_2'], reason: 'nothing of button 1 was on air');

        // Button 1's countdown timer is gone: only button 2's play is pending.
        rig.playback(2);
        rig.radio();
        expect(rig.auto.remaining, 5);
        async.elapse(const Duration(seconds: 5));
        expect(rig.actions, ['dvk_play_1', 'dvk_play_2', 'dvk_play_2']);
        rig.auto.exit();
        rig.dispose();
        expect(async.pendingTimers, isEmpty);
      });
    });

    test('entering over a running playback stops it, then arms (carrier still up)', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.auto.enter(2, 2, 3);
        // The stop is published, but the carrier of button 1's message is
        // still in the snapshot: the new loop must not stack a play on it.
        expect(rig.actions, ['dvk_play_1', 'dvk_stop']);
        expect(rig.stopValues, ['1']);
        expect(rig.auto.button, 2);
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.dispose();
      });
    });

    test('entering the armed button again resumes it', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.auto.pause();
        rig.auto.enter(1, 3, 3);
        expect(rig.auto.phase, DvkAutoPhase.starting);
        expect(rig.actions, ['dvk_play_1', 'dvk_play_3']);
        rig.auto.exit();
        rig.dispose();
      });
    });
  });

  group('link and radio availability', () {
    test('link down pauses the countdown', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        expect(rig.auto.phase, DvkAutoPhase.countdown);
        rig.store.markDisconnected();
        expect(rig.auto.phase, DvkAutoPhase.paused);

        rig.store.markConnected(scheduleGraceNotify: false);
        async.elapse(const Duration(seconds: 30));
        expect(rig.actions, ['dvk_play_1'], reason: 'the link coming back does not resume');
        expect(rig.auto.phase, DvkAutoPhase.paused);
        expect(async.pendingTimers, isEmpty);
        rig.dispose();
      });
    });

    test('link down pauses while the play is starting and while it is on air', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.store.markDisconnected();
        expect(rig.auto.phase, DvkAutoPhase.paused);
        expect(async.pendingTimers, isEmpty);

        rig.store.markConnected(scheduleGraceNotify: false);
        rig.auto.resume();
        rig.playback(1);
        expect(rig.auto.phase, DvkAutoPhase.playing);
        rig.store.markDisconnected();
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.dispose();
      });
    });

    test('radio bridge going offline pauses', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        rig.store.setBridgeOffline(_radio);
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.dispose();
      });
    });

    test('radio device going unreachable pauses', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        rig.store.setDeviceOffline(_radio);
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.dispose();
      });
    });
  });

  group('pause / resume / exit', () {
    test('resume plays immediately', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 6, 3);
        rig.playback(6);
        rig.radio();
        rig.radio(tx: 'tx');
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.radio();

        rig.auto.resume();
        expect(rig.auto.phase, DvkAutoPhase.starting);
        expect(rig.actions, ['dvk_play_6', 'dvk_play_6']);

        rig.playback(6);
        expect(rig.auto.phase, DvkAutoPhase.playing);
        rig.auto.exit();
        rig.dispose();
      });
    });

    test('resume is a no-op unless paused', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.resume(); // no button in auto
        expect(rig.mqtt.publishes, isEmpty);
        rig.auto.enter(1, 1, 3);
        rig.auto.resume(); // starting
        expect(rig.actions, ['dvk_play_1']);
        rig.auto.exit();
        rig.dispose();
      });
    });

    test('pause during the countdown stays armed and stops the timer', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        rig.auto.pause();
        expect(rig.auto.phase, DvkAutoPhase.paused);
        expect(rig.auto.button, 1);
        expect(rig.auto.remaining, 0);
        expect(rig.actions, ['dvk_play_1'], reason: 'nothing of ours on air, nothing to stop');
        async.elapse(const Duration(seconds: 30));
        expect(rig.actions, ['dvk_play_1']);
        expect(async.pendingTimers, isEmpty);
        rig.dispose();
      });
    });

    test('pause(stopPlayback) cuts our own message', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 5, 3);
        rig.playback(5);
        rig.auto.pause(stopPlayback: true);
        expect(rig.auto.phase, DvkAutoPhase.paused);
        expect(rig.actions, ['dvk_play_5', 'dvk_stop']);
        expect(rig.stopValues, ['5']);
        expect(async.pendingTimers, isEmpty);
        rig.dispose();
      });
    });

    test('pause(stopPlayback) while the play is starting names the memory', () {
      // The radio has not reported the playback yet, so the bridge cannot
      // resolve an id-less stop and would drop it.
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 5, 3);
        rig.auto.pause(stopPlayback: true);
        expect(rig.auto.phase, DvkAutoPhase.paused);
        expect(rig.actions, ['dvk_play_5', 'dvk_stop']);
        expect(rig.stopValues, ['5']);
        rig.dispose();
      });
    });

    test('pause(stopPlayback) in the countdown does not stop a foreign playback', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        rig.auto.pause(stopPlayback: true);
        expect(rig.actions, ['dvk_play_1']);
        rig.dispose();
      });
    });

    test('pause without a button is a no-op', () {
      final rig = _Rig();
      rig.auto.pause(stopPlayback: true);
      expect(rig.auto.phase, isNull);
      expect(rig.mqtt.publishes, isEmpty);
      rig.dispose();
    });

    test('exit during playing sends dvk_stop and leaves auto', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        expect(rig.auto.phase, DvkAutoPhase.playing);

        rig.auto.exit();
        expect(rig.actions, ['dvk_play_1', 'dvk_stop']);
        expect(rig.stopValues, ['1']);
        expect(rig.auto.button, isNull);
        expect(rig.auto.phase, isNull);
        expect(rig.auto.isAuto(1), isFalse);
        expect(async.pendingTimers, isEmpty);

        // The radio finishing afterwards must not wake anything up.
        rig.radio();
        async.elapse(const Duration(seconds: 30));
        expect(rig.actions, ['dvk_play_1', 'dvk_stop']);
        rig.dispose();
      });
    });

    test('exit while the play is starting also sends dvk_stop', () {
      // The radio may still act on the play; an orphaned message would run
      // with nobody to stop it.
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 7, 3);
        rig.auto.exit();
        expect(rig.actions, ['dvk_play_7', 'dvk_stop']);
        expect(rig.stopValues, ['7'], reason: 'an id-less stop is dropped by the bridge before the playback frame');
        expect(async.pendingTimers, isEmpty);
        rig.dispose();
      });
    });

    test('the stop names the memory that was played, not a later config', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 3, 3);
        rig.playback(3);
        rig.auto.updateConfig(1, 9, 3);
        rig.auto.exit();
        expect(rig.stopValues, ['3']);
        rig.dispose();
      });
    });

    test('exit from the countdown or paused sends nothing', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        rig.auto.exit();
        expect(rig.actions, ['dvk_play_1']);
        expect(async.pendingTimers, isEmpty);

        rig.auto.enter(1, 1, 3);
        rig.auto.pause();
        rig.auto.exit();
        expect(rig.actions, ['dvk_play_1', 'dvk_play_1']);
        expect(rig.auto.button, isNull);
        rig.dispose();
      });
    });

    test('exit without a button is a no-op', () {
      final rig = _Rig();
      rig.auto.exit();
      expect(rig.mqtt.publishes, isEmpty);
      rig.dispose();
    });
  });

  group('a play outliving a bus loss', () {
    test('exit after the link came back still cuts it', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 5, 3);
        rig.playback(5);
        rig.store.markDisconnected();
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.store.markConnected(scheduleGraceNotify: false);
        // The retained snapshot still shows our message on air.
        expect(rig.auto.phase, DvkAutoPhase.paused);

        rig.auto.exit();
        expect(rig.actions, ['dvk_play_5', 'dvk_stop']);
        expect(rig.stopValues, ['5']);
        rig.dispose();
      });
    });

    test('pause(stopPlayback) cuts it, once', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 5, 3);
        rig.playback(5);
        rig.store.markDisconnected();
        rig.store.markConnected(scheduleGraceNotify: false);

        rig.auto.pause(stopPlayback: true);
        rig.auto.pause(stopPlayback: true);
        rig.auto.exit();
        expect(rig.actions, ['dvk_play_5', 'dvk_stop']);
        rig.dispose();
      });
    });

    test('a resume refused while it is still on air does not forget it', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 5, 3);
        rig.playback(5);
        rig.store.markDisconnected();
        rig.store.markConnected(scheduleGraceNotify: false);

        rig.auto.resume(); // DVK busy: stays paused, sends nothing
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.auto.exit();
        expect(rig.actions, ['dvk_play_5', 'dvk_stop']);
        rig.dispose();
      });
    });

    test('a radio bridge outage during the message is covered the same way', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 5, 3);
        rig.playback(5);
        rig.store.setBridgeOffline(_radio);
        expect(rig.auto.phase, DvkAutoPhase.paused);
        rig.playback(5); // back online, still playing
        rig.auto.exit();
        expect(rig.stopValues, ['5']);
        rig.dispose();
      });
    });

    test('a play lost while starting is cut once the radio reports it', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 5, 3);
        rig.store.markDisconnected();
        rig.store.markConnected(scheduleGraceNotify: false);
        rig.playback(5); // the play was acted on after all
        rig.auto.exit();
        expect(rig.actions, ['dvk_play_5', 'dvk_stop']);
        expect(rig.stopValues, ['5']);
        rig.dispose();
      });
    });

    test('nothing to cut once the radio is idle again', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 5, 3);
        rig.playback(5);
        rig.store.markDisconnected();
        rig.store.markConnected(scheduleGraceNotify: false);
        rig.radio(); // message finished meanwhile

        rig.auto.exit();
        expect(rig.actions, ['dvk_play_5']);
        rig.dispose();
      });
    });

    test('a foreign memory on air is left alone', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 5, 3);
        rig.playback(5);
        rig.store.markDisconnected();
        rig.store.markConnected(scheduleGraceNotify: false);
        rig.playback(2); // the operator started another memory

        rig.auto.exit();
        expect(rig.actions, ['dvk_play_5']);
        rig.dispose();
      });
    });

    test('a loss from the countdown leaves nothing to cut', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 5, 3);
        rig.playback(5);
        rig.radio();
        rig.store.markDisconnected();
        rig.store.markConnected(scheduleGraceNotify: false);
        rig.playback(5); // someone else plays the same memory

        rig.auto.exit();
        expect(rig.actions, ['dvk_play_5']);
        rig.dispose();
      });
    });

    test('a new play replaces the lost one', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 5, 3);
        rig.playback(5);
        rig.store.markDisconnected();
        rig.store.markConnected(scheduleGraceNotify: false);
        rig.radio(); // message finished
        rig.auto.resume();
        expect(rig.auto.phase, DvkAutoPhase.starting);
        rig.playback(5);
        rig.radio();
        expect(rig.auto.phase, DvkAutoPhase.countdown);

        // Memory 5 on air again, but by someone else: not ours to cut.
        rig.playback(5);
        rig.auto.exit();
        expect(rig.actions, ['dvk_play_5', 'dvk_play_5']);
        rig.dispose();
      });
    });
  });

  group('updateConfig', () {
    test('applies from the next cycle and never pauses the running message', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);

        rig.auto.updateConfig(1, 5, 2);
        expect(rig.auto.phase, DvkAutoPhase.playing, reason: 'memory 1 is still on air');
        rig.playback(1);
        expect(rig.auto.phase, DvkAutoPhase.playing);

        rig.radio();
        expect(rig.auto.phase, DvkAutoPhase.countdown);
        expect(rig.auto.remaining, 2, reason: 'new interval');
        async.elapse(const Duration(seconds: 2));
        expect(rig.actions, ['dvk_play_1', 'dvk_play_5'], reason: 'new memory');

        rig.playback(5);
        expect(rig.auto.phase, DvkAutoPhase.playing);
        rig.auto.exit();
        rig.dispose();
      });
    });

    test('a countdown in progress is not restarted', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 5);
        rig.playback(1);
        rig.radio();
        async.elapse(const Duration(seconds: 2));
        expect(rig.auto.remaining, 3);
        rig.auto.updateConfig(1, 2, 9);
        expect(rig.auto.remaining, 3);
        async.elapse(const Duration(seconds: 3));
        expect(rig.actions, ['dvk_play_1', 'dvk_play_2']);
        rig.playback(2);
        rig.radio();
        expect(rig.auto.remaining, 9);
        rig.auto.exit();
        rig.dispose();
      });
    });

    test('for a button that is not in auto is ignored', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        rig.playback(1);
        rig.radio();
        rig.auto.updateConfig(2, 9, 9);
        expect(rig.auto.remaining, 3);
        async.elapse(const Duration(seconds: 3));
        expect(rig.actions, ['dvk_play_1', 'dvk_play_1']);
        rig.auto.exit();
        rig.dispose();
      });
    });
  });

  group('disposal', () {
    test('dispose cancels timers and detaches from the store', () {
      fakeAsync((async) {
        final rig = _Rig();
        rig.auto.enter(1, 1, 3);
        expect(async.pendingTimers, isNotEmpty); // start timeout
        rig.playback(1);
        rig.radio();
        expect(async.pendingTimers, isNotEmpty); // countdown tick

        rig.dispose();
        expect(async.pendingTimers, isEmpty);

        // A late bus update reaches no disposed notifier.
        rig.radio(tx: 'tx');
        async.elapse(const Duration(seconds: 30));
        expect(rig.actions, ['dvk_play_1']);
      });
    });
  });

  test('out-of-range memory and interval are clamped', () {
    fakeAsync((async) {
      final rig = _Rig();
      rig.auto.enter(1, 99, 0);
      expect(rig.actions, ['dvk_play_12']);
      rig.playback(12); // the clamped id is what the radio reports
      expect(rig.auto.phase, DvkAutoPhase.playing);
      rig.radio();
      expect(rig.auto.remaining, 1);
      rig.auto.exit();
      rig.dispose();
    });
  });
}
