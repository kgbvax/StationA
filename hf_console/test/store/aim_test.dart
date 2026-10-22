import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/store/aim.dart';
import 'package:hf_console/store/bus_store.dart';
import 'package:hf_console/store/wiring.dart';

import '../support/fake_mqtt_service.dart';
import '../support/fixtures.dart';

void main() {
  group('publishAim', () {
    BusStore storeWith({
      required bool steerOnline,
      required bool enabled,
      Object? inputs = const {'rotator': true, 'ant_ctrl': true, 'radio': true},
      bool linkUp = true,
    }) {
      final store = BusStore();
      // The realistic shapes: the retained /state (enabled, inputs) survives
      // the bridge going offline (LWT), so it is applied either way.
      store.setOnline('muehle/hf/beam-steer');
      store.applyState('muehle/hf/beam-steer', {'enabled': enabled, if (inputs != null) 'inputs': inputs});
      if (!steerOnline) store.applyStatus('muehle/hf/beam-steer', 'offline');
      if (linkUp) store.markConnected(scheduleGraceNotify: false);
      return store;
    }

    test('AUTO live: an HF aim goes to beamsteer, unretained', () {
      final store = storeWith(steerOnline: true, enabled: true);
      final mqtt = FakeMqttService(store);
      publishAim(mqtt, store, hfRotator, 265);
      expect(mqtt.publishes.single.topic, 'muehle/hf/beam-steer/cmd');
      expect(mqtt.publishes.single.payload, '{"action":"aim","value":265}');
      expect(mqtt.publishes.single.retain, isFalse);
    });

    test('AUTO off: an HF aim goes straight to the rotator', () {
      final store = storeWith(steerOnline: true, enabled: false);
      final mqtt = FakeMqttService(store);
      publishAim(mqtt, store, hfRotator, 265);
      expect(mqtt.publishes.single.topic, 'muehle/hf/rotator/cmd');
      expect(mqtt.publishes.single.payload, contains('set_az'));
    });

    test('beamsteer offline: falls back to the rotator even with AUTO on', () {
      final store = storeWith(steerOnline: false, enabled: true);
      final mqtt = FakeMqttService(store);
      publishAim(mqtt, store, hfRotator, 265);
      expect(mqtt.publishes.single.topic, 'muehle/hf/rotator/cmd');
    });

    test('beamsteer blind (no rotator data): falls back to the rotator', () {
      final store = storeWith(steerOnline: true, enabled: true, inputs: {'rotator': false});
      final mqtt = FakeMqttService(store);
      publishAim(mqtt, store, hfRotator, 265);
      expect(mqtt.publishes.single.topic, 'muehle/hf/rotator/cmd');
    });

    test('beamsteer build without inputs (no aim action): falls back', () {
      final store = storeWith(steerOnline: true, enabled: true, inputs: null);
      final mqtt = FakeMqttService(store);
      publishAim(mqtt, store, hfRotator, 265);
      expect(mqtt.publishes.single.topic, 'muehle/hf/rotator/cmd');
    });

    test('link down: never routes to beamsteer', () {
      final store = storeWith(steerOnline: true, enabled: true, linkUp: false);
      expect(beamSteerActive(store), isFalse);
    });

    test('the VHF dial never routes through beamsteer', () {
      final store = storeWith(steerOnline: true, enabled: true);
      final mqtt = FakeMqttService(store);
      publishAim(mqtt, store, vhfRotator, 120);
      expect(mqtt.publishes.single.topic, 'muehle/uhf/az-rotator/cmd');
    });
  });

  group('publishAim under the antenna mode', () {
    BusStore autoOn() {
      final store = BusStore();
      store.setOnline('muehle/hf/beam-steer');
      store.applyState('muehle/hf/beam-steer', {
        'enabled': true,
        'inputs': {'rotator': true, 'ant_ctrl': true, 'radio': true},
      });
      store.markConnected(scheduleGraceNotify: false);
      return store;
    }

    test('maintenance: an aim (disc, map, chip tap) is not a nudge and sends nothing', () {
      // A tap can name any bearing — one stray tap would slew the brakeless
      // G-450 a full sweep with people at the mast. Only the bounded
      // ±maintenanceNudgeStepDeg keys (publishNudge) move it in maintenance.
      final store = autoOn()
        ..setRotator(az: 176)
        ..setAntMode('maintenance');
      final mqtt = FakeMqttService(store);
      expect(aimRoute(store, hfRotator), AimRoute.nudge);
      expect(publishAim(mqtt, store, hfRotator, 95), isFalse);
      expect(publishAim(mqtt, store, hfRotator, 178), isFalse);
      expect(mqtt.publishes, isEmpty);
    });

    test('storm: nothing is sent', () {
      final store = autoOn()..setAntMode('storm');
      final mqtt = FakeMqttService(store);
      expect(aimRoute(store, hfRotator), AimRoute.refused);
      expect(publishAim(mqtt, store, hfRotator, 95), isFalse);
      expect(mqtt.publishes, isEmpty);
    });

    test('maintenance before the nudge is granted: nothing is sent', () {
      final store = autoOn();
      store.applyStatus('muehle/hf/ant-mode', 'online');
      store.applyState('muehle/hf/ant-mode', {'ant_mode': 'maintenance', 'lock': {'seq': 1, 'ground': true}});
      final mqtt = FakeMqttService(store);
      expect(publishAim(mqtt, store, hfRotator, 95), isFalse);
      expect(mqtt.publishes, isEmpty);
    });

    test('a locked snapshot still refuses with antmode offline', () {
      final store = autoOn()..setAntMode('storm', online: false);
      final mqtt = FakeMqttService(store);
      expect(publishAim(mqtt, store, hfRotator, 95), isFalse);
    });

    test('normal: unchanged routing', () {
      final store = autoOn()..setAntMode('normal');
      final mqtt = FakeMqttService(store);
      expect(publishAim(mqtt, store, hfRotator, 95), isTrue);
      expect(mqtt.publishes.single.topic, 'muehle/hf/beam-steer/cmd');
    });

    test('the sat rotator is not locked by the antenna mode', () {
      final store = autoOn()..setAntMode('storm');
      final mqtt = FakeMqttService(store);
      expect(publishAim(mqtt, store, vhfRotator, 120), isTrue);
      expect(mqtt.publishes.single.topic, 'muehle/uhf/az-rotator/cmd');
    });
  });

  group('publishNudge (maintenance ±step keys)', () {
    BusStore maintenance({double az = 176}) {
      final store = BusStore()
        ..setRotator(az: az)
        ..setAntMode('maintenance');
      store.markConnected(scheduleGraceNotify: false);
      return store;
    }

    test('the step is 5°', () {
      expect(maintenanceNudgeStepDeg, 5.0);
    });

    test('+step: set_az readback + 5 with the override, straight to the rotator', () {
      final store = maintenance();
      final mqtt = FakeMqttService(store);
      expect(canNudge(store, hfRotator), isTrue);
      expect(publishNudge(mqtt, store, hfRotator, maintenanceNudgeStepDeg), isTrue);
      expect(mqtt.publishes.single.topic, 'muehle/hf/rotator/cmd');
      expect(mqtt.publishes.single.payload, '{"action":"set_az","az":181.0,"override":"maintenance"}');
      expect(mqtt.publishes.single.retain, isFalse);
    });

    test('-step: set_az readback - 5', () {
      final store = maintenance();
      final mqtt = FakeMqttService(store);
      expect(publishNudge(mqtt, store, hfRotator, -maintenanceNudgeStepDeg), isTrue);
      expect(mqtt.publishes.single.payload, '{"action":"set_az","az":171.0,"override":"maintenance"}');
    });

    test('a step larger than the nudge is bounded to it', () {
      final store = maintenance();
      final mqtt = FakeMqttService(store);
      expect(publishNudge(mqtt, store, hfRotator, 180), isTrue);
      expect(publishNudge(mqtt, store, hfRotator, -90), isTrue);
      expect(mqtt.publishes.map((p) => p.payload), [
        '{"action":"set_az","az":181.0,"override":"maintenance"}',
        '{"action":"set_az","az":171.0,"override":"maintenance"}',
      ]);
    });

    test('never past the rotator stops (0..450), never across the wrap', () {
      final low = maintenance(az: 2);
      final mLow = FakeMqttService(low);
      publishNudge(mLow, low, hfRotator, -maintenanceNudgeStepDeg);
      expect(mLow.publishes.single.payload, '{"action":"set_az","az":0.0,"override":"maintenance"}');

      final high = maintenance(az: 448);
      final mHigh = FakeMqttService(high);
      publishNudge(mHigh, high, hfRotator, maintenanceNudgeStepDeg);
      expect(mHigh.publishes.single.payload, '{"action":"set_az","az":450.0,"override":"maintenance"}');

      // 358 + 5 is the overlap (363), not 3° the long way round.
      final wrap = maintenance(az: 358);
      final mWrap = FakeMqttService(wrap);
      publishNudge(mWrap, wrap, hfRotator, maintenanceNudgeStepDeg);
      expect(mWrap.publishes.single.payload, '{"action":"set_az","az":363.0,"override":"maintenance"}');
    });

    test('no readback: nothing is sent', () {
      final store = BusStore()
        ..setOnline('muehle/hf/rotator')
        ..setAntMode('maintenance');
      store.markConnected(scheduleGraceNotify: false);
      final mqtt = FakeMqttService(store);
      expect(canNudge(store, hfRotator), isFalse);
      expect(publishNudge(mqtt, store, hfRotator, maintenanceNudgeStepDeg), isFalse);
      expect(mqtt.publishes, isEmpty);
    });

    test('rotator bridge offline or link down: nothing is sent', () {
      final off = maintenance()..applyStatus('muehle/hf/rotator', 'offline');
      expect(canNudge(off, hfRotator), isFalse);
      expect(publishNudge(FakeMqttService(off), off, hfRotator, 5), isFalse);

      final down = BusStore()
        ..setRotator(az: 176)
        ..setAntMode('maintenance');
      expect(canNudge(down, hfRotator), isFalse);
    });

    test('storm, normal, or maintenance before the nudge is granted: nothing is sent', () {
      for (final store in [
        maintenance()..setAntMode('storm'),
        maintenance()..setAntMode('normal'),
        maintenance()
          ..applyState('muehle/hf/ant-mode', {
            'ant_mode': 'maintenance',
            'lock': {'seq': 1, 'ground': true},
          }),
      ]) {
        final mqtt = FakeMqttService(store);
        expect(canNudge(store, hfRotator), isFalse);
        expect(publishNudge(mqtt, store, hfRotator, 5), isFalse);
        expect(mqtt.publishes, isEmpty);
      }
    });

    test('the sat rotator has no nudge', () {
      final store = maintenance();
      expect(canNudge(store, vhfRotator), isFalse);
    });
  });
}
