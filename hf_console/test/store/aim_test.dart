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

    test('maintenance with the nudge: set_az with the override, straight to the rotator', () {
      final store = autoOn()..setAntMode('maintenance');
      final mqtt = FakeMqttService(store);
      expect(aimRoute(store, hfRotator), AimRoute.nudge);
      expect(publishAim(mqtt, store, hfRotator, 95), isTrue);
      // Not through beamsteer even with AUTO live: it stands down and must
      // never carry the override.
      expect(mqtt.publishes.single.topic, 'muehle/hf/rotator/cmd');
      expect(mqtt.publishes.single.payload, '{"action":"set_az","az":95.0,"override":"maintenance"}');
      expect(mqtt.publishes.single.retain, isFalse);
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
}
