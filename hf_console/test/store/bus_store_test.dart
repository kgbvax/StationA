import 'dart:convert';
import 'package:clock/clock.dart';
import 'package:fake_async/fake_async.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/wiring.dart' show expectedSlots;

import '../support/fixtures.dart';

void main() {
  group('BusStore.apply', () {
    test('stores meta, state, status and cmd per slot', () {
      final store = BusStore();
      store.apply('muehle/hf/pa/meta', jsonEncode({'schema': '1.0'}), true);
      store.apply('muehle/hf/pa/state', jsonEncode({'mode': 'operate'}), true);
      store.apply('muehle/hf/pa/status', 'online', true);
      store.apply('muehle/hf/pa/cmd', jsonEncode({'action': 'set_mode'}), true);

      final slot = store.slots['muehle/hf/pa']!;
      expect(slot.meta?['schema'], '1.0');
      expect(slot.state?['mode'], 'operate');
      expect(slot.status, 'online');
      expect(slot.cmd?['action'], 'set_mode');
    });

    test('clears a plane on empty retained payload', () {
      final store = BusStore();
      store.apply('muehle/hf/pa/status', 'online', true);
      store.apply('muehle/hf/pa/status', '', true);
      expect(store.slots['muehle/hf/pa']?.status, isNull);
    });

    test('ignores unknown topic suffixes', () {
      final store = BusStore();
      store.apply('muehle/hf/pa/unknown', jsonEncode({'x': 1}), true);
      expect(store.slots['muehle/hf/pa'], isNull);
    });

    group('rejects non-object payloads without throwing (T2)', () {
      for (final bad in ['[1,2]', '42', 'true', 'garbage']) {
        test('drops "$bad" on meta/state/cmd, keeps last-good', () {
          final store = BusStore();
          store.apply('muehle/hf/pa/meta', jsonEncode({'schema': '1.0'}), true);
          store.apply('muehle/hf/pa/state', jsonEncode({'mode': 'operate'}), true);
          store.apply('muehle/hf/pa/cmd', jsonEncode({'action': 'set_mode'}), true);
          final before = store.malformedPayloads;

          store.apply('muehle/hf/pa/meta', bad, true);
          store.apply('muehle/hf/pa/state', bad, true);
          store.apply('muehle/hf/pa/cmd', bad, true);

          final slot = store.slots['muehle/hf/pa']!;
          expect(slot.meta?['schema'], '1.0', reason: 'last-good meta kept');
          expect(slot.state?['mode'], 'operate', reason: 'last-good state kept');
          expect(slot.cmd?['action'], 'set_mode', reason: 'last-good cmd kept');
          expect(store.malformedPayloads, before + 3);
        });
      }

      test('drops a non-string status payload, keeps last-good', () {
        final store = BusStore();
        store.apply('muehle/hf/pa/status', 'online', true);
        final before = store.malformedPayloads;

        store.apply('muehle/hf/pa/status', jsonEncode({'online': true}), true);

        expect(store.slots['muehle/hf/pa']?.status, 'online');
        expect(store.malformedPayloads, before + 1);
      });

      test('the empty-payload clear still works alongside the guards', () {
        final store = BusStore();
        store.apply('muehle/hf/pa/state', jsonEncode({'mode': 'operate'}), true);
        store.apply('muehle/hf/pa/state', '', true);
        expect(store.slots['muehle/hf/pa']?.state, isNull);
        expect(store.malformedPayloads, 0);
      });
    });
  });

  group('Slot online state', () {
    test('isOnline requires both bridge and device online', () {
      final store = BusStore();
      store.apply('muehle/hf/pa/status', 'online', true);
      expect(store.slots['muehle/hf/pa']!.bridgeOnline, isTrue);
      expect(store.slots['muehle/hf/pa']!.isOnline, isFalse);

      store.apply('muehle/hf/pa/state', jsonEncode({'device_online': true}), true);
      expect(store.slots['muehle/hf/pa']!.isOnline, isTrue);
    });

    test('logic slots without device_online key are online once state arrives', () {
      final store = BusStore();
      store.apply('muehle/hf/antenna-select/status', 'online', true);
      expect(store.slots['muehle/hf/antenna-select']!.isOnline, isFalse);

      store.apply('muehle/hf/antenna-select/state', jsonEncode({'mode': 'auto', 'target': 'port6'}), true);
      expect(store.slots['muehle/hf/antenna-select']!.isOnline, isTrue);
    });

    test('physical-device slot with missing device_online stays offline', () {
      final store = BusStore();
      store.apply('muehle/hf/pa/status', 'online', true);
      // no state at all yet
      expect(store.slots['muehle/hf/pa']!.isOnline, isFalse);
    });

    test('session-bearing slot: healthy idle (device_online false) stays online', () {
      // muehle/uhf/radio (icom9700-radio-bridge) repurposes device_online as
      // CI-V control-session liveness — a healthy idle session publishes
      // false BY DESIGN (R16), and session_state is the idle-vs-fault
      // discriminator. It must never read as an unreachable device.
      final store = BusStore();
      store.apply('muehle/uhf/radio/status', 'online', true);
      store.apply('muehle/uhf/radio/state', jsonEncode({
        'session_state': 'idle',
        'audio_demand': false,
        'monitor': false,
        'device_online': false,
        'ts': '2026-09-14T12:00:00Z',
      }), true);
      expect(store.slots['muehle/uhf/radio']!.isOnline, isTrue);
      expect(store.offlineList.where((e) => e.startsWith('muehle/uhf/radio')),
          isEmpty);

      // A faulting session keeps the BRIDGE reachable — the fault surfaces
      // as the slot's error, not as a device-unreachable row.
      store.apply('muehle/uhf/radio/state', jsonEncode({
        'session_state': 'error',
        'error': 'radio: login refused',
        'device_online': false,
        'ts': '2026-09-14T12:00:01Z',
      }), true);
      expect(store.slots['muehle/uhf/radio']!.isOnline, isTrue);
      expect(store.offlineList.where((e) => e.contains('unreachable') && e.contains('uhf/radio')),
          isEmpty);
      // ...while the error still reaches the fault history.
      expect(store.faultHistory.where((r) => r.address == 'muehle/uhf/radio'),
          isNotEmpty);
    });
  });

  group('BusStore.offlineList', () {
    test('lists bridge down and device unreachable separately', () {
      final store = BusStore();
      store.setBridgeOffline('muehle/hf/pa');
      store.setDeviceOffline('muehle/hf/tuner');
      store.setOnline('muehle/hf/radio');

      final offline = store.offlineList;
      expect(offline, contains('muehle/hf/pa: bridge down'));
      expect(offline, contains('muehle/hf/tuner: device unreachable'));
      expect(offline, isNot(contains('muehle/hf/radio')));
    });

    test('reports expected slots silent only after the grace period', () {
      fakeAsync((async) {
        final store = BusStore();
        store.markConnected();
        store.setOnline('muehle/hf/radio');
        // antenna-select and power-seq never publish anything this session.

        // Grace period not yet elapsed: silence not reported.
        async.elapse(const Duration(seconds: 2));
        expect(store.offlineList.where((e) => e.contains('silent')), isEmpty);

        async.elapse(const Duration(seconds: 2));
        final offline = store.offlineList;
        expect(offline, contains('muehle/hf/antenna-select: silent (no state since connect)'));
        expect(offline, contains('muehle/hf/power-seq: silent (no state since connect)'));
        // Slots that DID publish are never reported as silent.
        expect(offline.where((e) => e.startsWith('muehle/hf/radio: silent')), isEmpty);
      });
    });

    test('reports silent expected slots even with zero bus messages', () {
      // The dead-station case the report exists for: a broker delivering
      // nothing under muehle/# must still surface every expected slot.
      fakeAsync((async) {
        final store = BusStore();
        store.markConnected();
        async.elapse(const Duration(seconds: 5));

        final silent = store.offlineList.where((e) => e.contains('silent')).toList();
        expect(silent.length, expectedSlots.length);
      });
    });

    test('notifies on connect and again when the grace period expires', () {
      // On a quiet band no bus message ever rebuilds the UI after the
      // retained flood — the store must push the report itself. The connect
      // notify repaints the linkUp-gated controls the moment the link is up.
      fakeAsync((async) {
        final store = BusStore();
        var notified = 0;
        store.addListener(() => notified++);
        store.markConnected();
        expect(notified, 1);
        async.elapse(const Duration(seconds: 5));
        expect(notified, 2);
      });
    });

    test('a silent report clears when the slot publishes', () {
      fakeAsync((async) {
        final store = BusStore();
        store.markConnected();
        async.elapse(const Duration(seconds: 5));
        expect(
          store.offlineList.where((e) => e.startsWith('muehle/hf/antenna-select: silent')),
          isNotEmpty,
        );

        store.setOnline('muehle/hf/antenna-select');
        expect(
          store.offlineList.where((e) => e.startsWith('muehle/hf/antenna-select: silent')),
          isEmpty,
        );
        expect(store.offlineList.where((e) => e.startsWith('muehle/hf/antenna-select')), isEmpty);
      });
    });

    test('a slot heard from once is never reported silent', () {
      fakeAsync((async) {
        final store = BusStore();
        store.markConnected();
        store.setOnline('muehle/hf/antenna-select');
        store.setBridgeOffline('muehle/hf/antenna-select');
        async.elapse(const Duration(seconds: 10));

        expect(store.offlineList, contains('muehle/hf/antenna-select: bridge down'));
        expect(store.offlineList.where((e) => e.startsWith('muehle/hf/antenna-select: silent')), isEmpty);
      });
    });
  });

  group('BusStore.offlineSince timestamps', () {
    test('silent slots map to connect time; device-link flips to the flip time', () {
      fakeAsync((async) {
        final store = BusStore();
        store.markConnected();
        final connectAt = clock.now();

        store.setOnline('muehle/hf/radio');
        async.elapse(const Duration(minutes: 10));
        store.setDeviceOffline('muehle/hf/radio');
        final dropAt = clock.now();
        expect(store.offlineSince['muehle/hf/radio'], dropAt);
        expect(store.offlineSince['muehle/hf/radio'], isNot(connectAt));

        // power-seq never publishes: it is silent, and its 'silence began'
        // time is exactly the connect time — not a ticking render clock.
        async.elapse(const Duration(seconds: 5));
        expect(store.offlineSince['muehle/hf/power-seq'], connectAt);
      });
    });

    test('a cleared /status stamps at clear time, not at last online time', () {
      fakeAsync((async) {
        final store = BusStore();
        store.setOnline('muehle/hf/switch');
        final onlineAt = store.slots['muehle/hf/switch']!.statusChangedAt;

        async.elapse(const Duration(minutes: 30));
        store.apply('muehle/hf/switch/status', '', true);
        final clearAt = clock.now();

        // The row must carry the clear time, not the going-online floor.
        expect(store.offlineSince['muehle/hf/switch'], clearAt);
        expect(store.slots['muehle/hf/switch']!.statusChangedAt, clearAt);
        expect(onlineAt, isNot(clearAt));
      });
    });
  });

  group('Fault history', () {
    test('records a PA fault with error text', () {
      final store = BusStore();
      store.setPaFault(error: 'HOT SWITCHING ATTEMPT');

      expect(store.faultHistory.length, 1);
      expect(store.faultHistory.first.address, 'muehle/hf/pa');
      expect(store.faultHistory.first.text, 'HOT SWITCHING ATTEMPT');
      expect(store.faultHistory.first.active, isTrue);
    });

    test('clears an active fault when state reports none', () {
      final store = BusStore();
      store.setPaFault();
      store.setPaHealthy();

      expect(store.faultHistory.first.active, isFalse);
    });

    test('reactivates and refreshes timestamp on repeated fault', () {
      final store = BusStore();
      store.setPaFault();
      final firstTs = store.faultHistory.first.ts;

      store.setPaHealthy();
      store.applyState('muehle/hf/pa', {
        'fault': 'other',
        'error': 'HOT SWITCHING ATTEMPT',
        'device_online': true,
        'ts': '2026-08-20T15:00:00.000000',
      });

      expect(store.faultHistory.length, 1);
      expect(store.faultHistory.first.active, isTrue);
      expect(store.faultHistory.first.ts, isNot(firstTs));
      expect(store.faultHistory.first.ts, '2026-08-20T15:00:00.000000');
    });

    test('adds a new record when fault text changes', () {
      final store = BusStore();
      store.setPaFault(error: 'HOT SWITCHING ATTEMPT');
      store.setPaFault(error: 'EXCESSIVE DRIVE POWER');

      expect(store.faultHistory.length, 2);
      expect(store.faultHistory.first.active, isFalse); // old fault
      expect(store.faultHistory.last.active, isTrue); // new fault
    });

    test('caps history at 30 entries', () {
      final store = BusStore();
      for (var i = 0; i < 35; i++) {
        store.applyState('muehle/hf/pa', {
          'fault': 'other',
          'error': 'fault $i',
          'device_online': true,
          'ts': '2026-08-20T${14 + i ~/ 60}:${(i % 60).toString().padLeft(2, '0')}:00.000000',
        });
      }
      expect(store.faultHistory.length, 30);
    });

    test('handles non-string fault, error and ts values', () {
      final store = BusStore();
      store.applyState('muehle/hf/pa', {
        'fault': 1,
        'error': true,
        'device_online': true,
        'ts': 1234567890,
      });

      expect(store.faultHistory.length, 1);
      expect(store.faultHistory.first.text, 'TRUE'); // error prioritized
      expect(store.faultHistory.first.ts, '1234567890');
    });

    test('treats NONE error as no fault', () {
      final store = BusStore();
      store.applyState('muehle/hf/pa', {
        'fault': 'none',
        'error': 'NONE',
        'device_online': true,
        'ts': '2026-08-20T14:30:00.000000',
      });

      expect(store.faultHistory.length, 0);
    });
  });

  group('stateValueAs<T>', () {
    test('returns typed value when type matches', () {
      final store = BusStore();
      store.applyState('muehle/hf/pa', {
        'temp_c': 38.5,
        'swr': 1.1,
        'fault': 'other',
      });

      expect(store.stateValueAs<double>('muehle/hf/pa', 'temp_c'), 38.5);
      expect(store.stateValueAs<int>('muehle/hf/pa', 'temp_c'), isNull); // double != int
      expect(store.stateValueAs<String>('muehle/hf/pa', 'fault'), 'other');
    });

    test('returns null for missing slot or key', () {
      final store = BusStore();
      expect(store.stateValueAs<String>('muehle/hf/pa', 'fault'), isNull);
    });
  });
}
