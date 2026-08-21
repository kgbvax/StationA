import 'dart:convert';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';

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
