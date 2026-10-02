import '../mqtt/mqtt_service.dart';
import 'bus_store.dart';
import 'wiring.dart';

const _beamSteer = 'muehle/hf/beam-steer';

/// Smart rotation (beamsteer, the Ultrabeam panel's AUTO) can take an aim:
/// switched on, its bridge up, the console's link up — and beamsteer itself
/// reports live rotator data (`inputs.rotator`). A blind beamsteer ignores
/// every request, and a build without `inputs` predates the `aim` action;
/// in both cases aiming falls back to the rotator directly.
bool beamSteerActive(BusStore store) {
  if (!store.linkUp || !(store.slots[_beamSteer]?.isOnline ?? false)) return false;
  if (!(store.stateValueAs<bool>(_beamSteer, 'enabled') ?? false)) return false;
  final inputs = store.stateValue(_beamSteer, 'inputs');
  return inputs is Map && inputs['rotator'] == true;
}

/// Aims [surface] at bearing [deg] (great-circle, degrees true).
///
/// With AUTO live, an HF aim is a request to put a lobe on that bearing, not
/// to turn the boom there: it goes to beamsteer, which flips the Ultrabeam
/// 180° or turns the mast to the cheaper lobe — the same decision the logger
/// gets. A straight `set_az` would ignore a reversed beam and point the main
/// lobe away from the station. The aim cmd is one-shot (never retained), so
/// the retained AUTO on/off on the same topic stays intact.
void publishAim(MqttService mqtt, BusStore store, RotatorSurface surface, double deg) {
  if (surface.stateSlot == hfRotator.stateSlot && beamSteerActive(store)) {
    mqtt.publish(cmdTopic('hf/beam-steer'), beamSteerAimPayload(deg), retain: false);
    return;
  }
  mqtt.publish(cmdTopic(surface.cmdSlot), surface.aimPayload(deg), retain: cmdRetain[surface.stateSlot] ?? false);
}
