import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/ant_mode.dart';
import 'package:hf_console/store/bus_store.dart';

import '../support/fixtures.dart';

void main() {
  group('AntModeState.parse (the shared/antmode reading rules)', () {
    test('no retained state is normal', () {
      final m = AntModeState.parse(null)!;
      expect(m.mode, 'normal');
      expect(m.locked, isFalse);
      expect(m.lockReason, '');
    });

    test('a full storm snapshot', () {
      final m = AntModeState.parse({
        'ant_mode': 'storm',
        'source': 'auto',
        'lock': {'seq': 17, 'ground': true, 'retract': true, 'park_az': 175, 'nudge': false},
        'phase': 'parked',
        'presence': 'unmanned',
        'wind': {'dir_deg': 265, 'gust_kmh': 72, 'ts': 't', 'stale': false},
        'warnings': [
          {'id': 'dwd.x', 'event': 'STURMBÖEN', 'ii': '52', 'onset': 'a', 'expires': 'b'},
        ],
        'drift_deg': 1,
      })!;
      expect(m.locked, isTrue);
      expect(m.nudgeAllowed, isFalse);
      expect((m.l.seq, m.l.ground, m.l.retract, m.l.parkAz), (17, true, true, 175));
      expect(m.wind!.dirDeg, 265);
      expect(m.wind!.stale, isFalse);
      expect(m.warnings.single.event, 'STURMBÖEN');
      expect(m.driftDeg, 1);
      expect(m.lockReason, 'STORM LOCK · parked 175°');
    });

    test('a locked mode without a lock block reads as a zero lock', () {
      final m = AntModeState.parse({'ant_mode': 'maintenance'})!;
      expect(m.locked, isTrue);
      expect(m.l.parkAz, isNull);
      // The nudge is not granted until the lock says so.
      expect(m.nudgeAllowed, isFalse);
      expect(m.lockReason, 'MAINTENANCE LOCK · parking');
    });

    test('the reason claims only what the phase confirms', () {
      String reason(String phase, {int? park = 175}) => AntModeState.parse({
            'ant_mode': 'storm',
            'lock': {'seq': 1, 'ground': true, 'retract': true, if (park != null) 'park_az': park},
            'phase': phase,
          })!.lockReason;
      expect(reason('parked'), 'STORM LOCK · parked 175°');
      // park_az is the target, published as the rotation starts.
      expect(reason('rotating'), 'STORM LOCK · parking → 175°');
      // It stays on a failed park (rotator offline, power borrow failed).
      expect(reason('failed'), 'STORM LOCK · PARK FAILED');
      expect(reason('failed', park: null), 'STORM LOCK · PARK FAILED');
      expect(reason('grounding', park: null), 'STORM LOCK · parking');
      expect(reason('awaiting_power', park: null), 'STORM LOCK · parking');
    });

    test('maintenance with lock.nudge grants the nudge', () {
      final m = AntModeState.parse({
        'ant_mode': 'maintenance',
        'lock': {'seq': 3, 'ground': true, 'retract': true, 'park_az': 90, 'nudge': true},
      })!;
      expect(m.nudgeAllowed, isTrue);
    });

    test('normal ignores a stray lock', () {
      final m = AntModeState.parse({
        'ant_mode': 'normal',
        'lock': {'seq': 1, 'nudge': true},
      })!;
      expect(m.locked, isFalse);
      expect(m.lock, isNull);
    });

    test('malformed: unknown mode, missing mode, bad lock, park_az out of range', () {
      expect(AntModeState.parse({'ant_mode': 'hurricane'}), isNull);
      expect(AntModeState.parse({'mode': 'storm'}), isNull);
      expect(AntModeState.parse({'ant_mode': 'storm', 'lock': 'yes'}), isNull);
      expect(AntModeState.parse({'ant_mode': 'storm', 'lock': {'park_az': 451}}), isNull);
      expect(AntModeState.parse({'ant_mode': 'storm', 'lock': {'park_az': -1}}), isNull);
    });
  });

  group('BusStore.antMode', () {
    test('defaults to normal before antmode exists', () {
      expect(BusStore().antMode.locked, isFalse);
    });

    test('a locked snapshot holds while antmode is offline', () {
      final store = BusStore()..setAntMode('storm');
      store.applyStatus('muehle/hf/ant-mode', 'offline');
      expect(store.antMode.locked, isTrue);
      expect(store.antMode.mode, 'storm');
    });

    test('a malformed snapshot keeps the last known mode', () {
      final store = BusStore()..setAntMode('storm');
      final before = store.malformedPayloads;
      store.apply('muehle/hf/ant-mode/state', jsonEncode({'ant_mode': 'gale'}), true);
      expect(store.antMode.mode, 'storm');
      expect(store.malformedPayloads, before + 1);
      store.apply('muehle/hf/ant-mode/state', '[1,2]', true);
      expect(store.antMode.mode, 'storm');
    });

    test('a cleared retained state is normal', () {
      final store = BusStore()..setAntMode('maintenance');
      store.apply('muehle/hf/ant-mode/state', '', true);
      expect(store.antMode.locked, isFalse);
    });

    test('other slots never touch the mode', () {
      final store = BusStore()..setAntMode('storm');
      store.applyState('muehle/hf/beam-steer', {'ant_mode': 'normal'});
      expect(store.antMode.mode, 'storm');
    });
  });
}
