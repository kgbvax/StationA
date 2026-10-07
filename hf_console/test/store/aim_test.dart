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
}
