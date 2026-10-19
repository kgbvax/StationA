import 'package:clock/clock.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/station_schematic.dart';

import '../support/fixtures.dart';
import '../support/station_fixtures.dart';

BusStore _store(StationScenario scenario) {
  final s = BusStore()..markConnected(scheduleGraceNotify: false);
  s.seedScenario(scenario);
  return s;
}

void main() {
  group('quality', () {
    test('a healthy station is ok everywhere, no severity', () {
      final m = StationSchematicModel(_store(StationScenario.running));
      for (final a in [
        'muehle/power/psu-13v8', 'muehle/hf/radio', 'muehle/hf/pa', 'muehle/hf/tuner', 'muehle/hf/ant-switch',
        'muehle/hf/ant-ctrl', 'muehle/hf/rotator', 'muehle/hf/switch', 'muehle/hf/power-seq',
        'muehle/uhf/radio', 'muehle/uhf/az-rotator', 'muehle/uhf/el-rotator', 'muehle/uhf/pol-ctrl',
      ]) {
        expect(m.quality(a), SlotQuality.ok, reason: a);
        expect(m.severity(a), Severity.none, reason: a);
      }
    });

    test('link down makes everything stale, without alarms', () {
      final s = _store(StationScenario.running)..markDisconnected();
      final m = StationSchematicModel(s);
      expect(m.quality('muehle/hf/radio'), SlotQuality.stale);
      expect(m.severity('muehle/hf/radio'), Severity.none);
    });

    test('bridge down and device lost are alarms', () {
      final s = _store(StationScenario.running)
        ..setBridgeOffline('muehle/hf/pa')
        ..setDeviceOffline('muehle/uhf/el-rotator');
      final m = StationSchematicModel(s);
      expect(m.quality('muehle/hf/pa'), SlotQuality.down);
      expect(m.severity('muehle/hf/pa'), Severity.alarm);
      expect(m.quality('muehle/uhf/el-rotator'), SlotQuality.lost);
      expect(m.severity('muehle/uhf/el-rotator'), Severity.alarm);
    });

    test('standby: antenna-side devices are unpowered, the sleeping PC is expected', () {
      final m = StationSchematicModel(_store(StationScenario.standby));
      for (final a in antPoweredSlots) {
        expect(m.quality(a), SlotQuality.unpowered, reason: a);
        expect(m.severity(a), Severity.none, reason: a);
      }
      expect(m.quality(shackPcSlot), SlotQuality.unpowered);
      expect(m.severity(shackPcSlot), Severity.none);
    });

    test('antenna plug confirmed off explains the rotators without standby', () {
      final s = _store(StationScenario.running)
        ..applyState(antennaPlugSlot, {'device_online': true, 'power': 'off'})
        ..setDeviceOffline('muehle/hf/rotator');
      expect(StationSchematicModel(s).quality('muehle/hf/rotator'), SlotQuality.unpowered);
    });

    test('an unknown antenna plug does not excuse a lost rotator', () {
      final s = _store(StationScenario.running)
        ..setBridgeOffline(antennaPlugSlot)
        ..setDeviceOffline('muehle/hf/rotator');
      expect(StationSchematicModel(s).quality('muehle/hf/rotator'), SlotQuality.lost);
    });

    test('PSU off: the 13.8 V devices, tuner included, are unpowered', () {
      final m = StationSchematicModel(_store(StationScenario.psuOff));
      for (final a in psuPoweredSlots) {
        expect(m.quality(a), SlotQuality.unpowered, reason: a);
        expect(m.severity(a), Severity.none, reason: a);
      }
      expect(psuPoweredSlots, contains('muehle/hf/tuner'));
    });

    test('PSU off: firmware-as-bridge slots whose LWT fired read unpowered, not down', () {
      final m = StationSchematicModel(_store(StationScenario.psuOff));
      for (final a in ['muehle/hf/switch', 'muehle/hf/pa-arm', 'muehle/hf/ant-switch']) {
        expect(m.store.slots[a]!.bridgeOnline, isFalse, reason: a);
        expect(m.quality(a), SlotQuality.unpowered, reason: a);
      }
    });

    test('with the PSU on, a dead PLC #1 is down', () {
      final s = _store(StationScenario.running)..applyStatus('muehle/hf/switch', 'offline');
      expect(StationSchematicModel(s).quality('muehle/hf/switch'), SlotQuality.down);
      expect(StationSchematicModel(s).severity('muehle/hf/switch'), Severity.alarm);
    });

    test('PSU off without a confirmed reading is not an excuse', () {
      final s = _store(StationScenario.psuOff)..setBridgeOffline(psuSlot);
      expect(StationSchematicModel(s).psuPower, isNull);
      expect(StationSchematicModel(s).quality('muehle/hf/switch'), SlotQuality.down);
    });

    test('plug power is three-state: a dead plug is unknown, not on', () {
      final s = _store(StationScenario.running);
      expect(StationSchematicModel(s).psuPower, isTrue);
      s.setDeviceOffline(psuSlot); // Shelly unreachable, retained power stays 'on'
      expect(StationSchematicModel(s).psuPower, isNull);
      s.applyState(psuSlot, {'device_online': true});
      expect(StationSchematicModel(s).psuPower, isNull, reason: 'missing key');
    });

    test('ACOM with its remote-on relay off is unpowered, not unreachable', () {
      final s = _store(StationScenario.running)
        ..applyState('muehle/hf/switch', {'device_online': true, 'trx': 'on', 'pa': 'off'})
        ..setDeviceOffline(paSlot);
      expect(StationSchematicModel(s).quality(paSlot), SlotQuality.unpowered);
      s.applyState('muehle/hf/switch', {'device_online': true, 'trx': 'on', 'pa': 'on'});
      expect(StationSchematicModel(s).quality(paSlot), SlotQuality.lost);
    });

    test('a deaf IC-9700 is unpowered only when the PSU is confirmed off', () {
      final off = StationSchematicModel(_store(StationScenario.psuOff));
      expect(off.uhfRadioDeaf, isTrue);
      expect(off.quality(uhfRadioSlot), SlotQuality.unpowered);
      final s = _store(StationScenario.running)
        ..applyState(uhfRadioSlot, {'device_online': false, 'session_state': 'idle', 'monitor': true, 'radio_responding': false});
      final m = StationSchematicModel(s);
      expect(m.uhfRadioDeaf, isTrue);
      expect(m.quality(uhfRadioSlot), SlotQuality.ok);
    });

    test('the IC-9700 idle session (device_online false) is healthy', () {
      final m = StationSchematicModel(_store(StationScenario.running));
      expect(m.quality('muehle/uhf/radio'), SlotQuality.ok);
    });

    test('a closed OscarWatch is not a fault, a dead tracker bridge is', () {
      final s = _store(StationScenario.running)
        ..applyState('muehle/uhf/sat-track', {'device_online': false, 'error': 'tracker closed'});
      expect(StationSchematicModel(s).severity('muehle/uhf/sat-track'), Severity.none);
      s.setBridgeOffline('muehle/uhf/sat-track');
      expect(StationSchematicModel(s).severity('muehle/uhf/sat-track'), Severity.alarm);
    });

    test('a never-heard slot is no data, alarmed only once the silence grace is over', () {
      final start = DateTime(2026, 10, 19, 12);
      withClock(Clock.fixed(start), () {
        final s = BusStore()..markConnected(scheduleGraceNotify: false);
        expect(StationSchematicModel(s).quality('muehle/hf/pa'), SlotQuality.noData);
        expect(StationSchematicModel(s).severity('muehle/hf/pa'), Severity.none);
        withClock(Clock.fixed(start.add(const Duration(seconds: 10))), () {
          expect(StationSchematicModel(s).severity('muehle/hf/pa'), Severity.alarm);
        });
      });
    });
  });

  group('severity from readings', () {
    test('device fault text is an alarm and is reported upper-cased', () {
      final m = StationSchematicModel(_store(StationScenario.faults));
      expect(m.severity('muehle/hf/pa'), Severity.alarm);
      expect(m.problemText('muehle/hf/pa'), 'HIGH SWR');
    });

    test('tuner SWR follows the tuner panel limits', () {
      final s = _store(StationScenario.running);
      Severity at(double swr) {
        s.applyState('muehle/hf/tuner', {'device_online': true, 'inline': true, 'swr': swr, 'fault': ''});
        return StationSchematicModel(s).severity('muehle/hf/tuner');
      }

      expect(at(1.9), Severity.none);
      expect(at(2.0), Severity.warn);
      expect(at(3.0), Severity.alarm);
    });

    test('PA SWR counts only while keyed', () {
      final s = _store(StationScenario.running);
      s.applyState('muehle/hf/pa', {'device_online': true, 'keyed': 'rx', 'swr': 2.5, 'fault': 'none', 'error': ''});
      expect(StationSchematicModel(s).severity('muehle/hf/pa'), Severity.none);
      s.applyState('muehle/hf/pa', {'device_online': true, 'keyed': 'tx', 'swr': 2.5, 'fault': 'none', 'error': ''});
      expect(StationSchematicModel(s).severity('muehle/hf/pa'), Severity.warn);
    });

    test('antenna select manual hold is a warning', () {
      final m = StationSchematicModel(_store(StationScenario.faults));
      expect(m.severity('muehle/hf/antenna-select'), Severity.warn);
    });

    test('carrier covers tx, tune and a keyed PA', () {
      expect(StationSchematicModel(_store(StationScenario.running)).carrier, isFalse);
      expect(StationSchematicModel(_store(StationScenario.tx)).carrier, isTrue);
      final s = _store(StationScenario.running)
        ..applyState('muehle/hf/radio', {'device_online': true, 'tx': 'rx', 'tuning': true});
      expect(StationSchematicModel(s).carrier, isTrue);
    });
  });

  group('hosts', () {
    test('counts bridges per host, one per component', () {
      final m = StationSchematicModel(_store(StationScenario.running));
      final sc = m.hostHealth('scmino');
      expect(sc.total, 13);
      expect(sc.up, 13);
      final sh = m.hostHealth('shari');
      expect((sh.up, sh.total), (1, 1));
    });

    test('a down bridge lowers its host count', () {
      final m = StationSchematicModel(_store(StationScenario.faults));
      final sc = m.hostHealth('scmino');
      expect(sc.up, 12);
      expect(sc.components['ultrabridge'], isFalse);
    });

    test('lastChange is the latest of the status and device flips', () {
      final t0 = DateTime(2026, 10, 19, 12);
      withClock(Clock.fixed(t0), () {
        final s = BusStore()..markConnected(scheduleGraceNotify: false);
        s.setOnline('muehle/hf/pa');
        withClock(Clock.fixed(t0.add(const Duration(minutes: 5))), () {
          s.applyState('muehle/hf/pa', {'device_online': false});
        });
        expect(StationSchematicModel(s).lastChange('muehle/hf/pa'), t0.add(const Duration(minutes: 5)));
      });
    });
  });
}
