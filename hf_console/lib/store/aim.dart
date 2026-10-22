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

/// How an aim at [surface] would be handled under the antenna mode.
enum AimRoute {
  /// Normal: beamsteer (AUTO live) or the rotator's plain aim payload.
  normal,

  /// Maintenance with `lock.nudge`: set_az straight to the rotator, tagged
  /// with the maintenance override — never through beamsteer (which is
  /// standing down and must not carry the override).
  nudge,

  /// Storm, or maintenance before the nudge is granted: nothing is sent —
  /// the rotator bridge would refuse it. The UI shows
  /// `store.antMode.lockReason` instead.
  refused,
}

/// The antenna mode locks the HF rotator only; the sat rotators are not
/// parked by it (deferred in the antmode plan).
AimRoute aimRoute(BusStore store, RotatorSurface surface) {
  if (surface.stateSlot != hfRotator.stateSlot) return AimRoute.normal;
  final m = store.antMode;
  if (!m.locked) return AimRoute.normal;
  return m.nudgeAllowed ? AimRoute.nudge : AimRoute.refused;
}

/// Aims [surface] at bearing [deg] (great-circle, degrees true). Returns
/// whether anything was published.
///
/// With AUTO live, an HF aim is a request to put a lobe on that bearing, not
/// to turn the boom there: it goes to beamsteer, which flips the Ultrabeam
/// 180° or turns the mast to the cheaper lobe — the same decision the logger
/// gets. A straight `set_az` would ignore a reversed beam and point the main
/// lobe away from the station. The aim cmd is one-shot (never retained), so
/// the retained AUTO on/off on the same topic stays intact.
///
/// Under an antenna-mode lock (see [aimRoute]) a maintenance nudge goes
/// straight to the rotator with the override; anything else is not sent.
bool publishAim(MqttService mqtt, BusStore store, RotatorSurface surface, double deg) {
  switch (aimRoute(store, surface)) {
    case AimRoute.refused:
      return false;
    case AimRoute.nudge:
      mqtt.publish(cmdTopic(surface.cmdSlot), rotatorAzOverridePayload(deg),
          retain: cmdRetain[surface.stateSlot] ?? false);
      return true;
    case AimRoute.normal:
      break;
  }
  if (surface.stateSlot == hfRotator.stateSlot && beamSteerActive(store)) {
    mqtt.publish(cmdTopic('hf/beam-steer'), beamSteerAimPayload(deg), retain: false);
    return true;
  }
  mqtt.publish(cmdTopic(surface.cmdSlot), surface.aimPayload(deg), retain: cmdRetain[surface.stateSlot] ?? false);
  return true;
}
