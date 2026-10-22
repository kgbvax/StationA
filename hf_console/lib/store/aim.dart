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

  /// Maintenance with `lock.nudge`: an aim (disc, map or chip tap) sends
  /// nothing — a tap names any bearing, and one stray tap would slew the
  /// brakeless G-450 a full sweep with people at the mast. The rotator moves
  /// only by the bounded ±[maintenanceNudgeStepDeg] keys ([publishNudge]):
  /// set_az straight to the rotator, tagged with the maintenance override —
  /// never through beamsteer (which is standing down and must not carry the
  /// override).
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
/// Under an antenna-mode lock (see [aimRoute]) nothing is sent: storm
/// refuses all motion, and in maintenance only [publishNudge] moves the
/// rotator.
bool publishAim(MqttService mqtt, BusStore store, RotatorSurface surface, double deg) {
  if (aimRoute(store, surface) != AimRoute.normal) return false;
  if (surface.stateSlot == hfRotator.stateSlot && beamSteerActive(store)) {
    mqtt.publish(cmdTopic('hf/beam-steer'), beamSteerAimPayload(deg), retain: false);
    return true;
  }
  mqtt.publish(cmdTopic(surface.cmdSlot), surface.aimPayload(deg), retain: cmdRetain[surface.stateSlot] ?? false);
  return true;
}

/// One maintenance nudge step, degrees. The only motion the console offers
/// while the mast may be worked on: a small, deliberate move from the
/// current readback (antmode plan: "a ±5° nudge with the override").
const double maintenanceNudgeStepDeg = 5.0;

/// The G-450DC's travel (0..450° with the overlap). A nudge target is
/// clamped to it and never wrapped mod 360: 358 + 5 is the overlap at 363,
/// not 3° the long way round.
const double _hfRotatorMinAz = 0.0;
const double _hfRotatorMaxAz = 450.0;

/// Whether a ±step nudge key can act: maintenance with `lock.nudge` on the
/// HF rotator, its bridge online, the console's link up and a numeric
/// readback to step from.
bool canNudge(BusStore store, RotatorSurface surface) {
  if (aimRoute(store, surface) != AimRoute.nudge) return false;
  if (!store.linkUp || !(store.slots[surface.stateSlot]?.isOnline ?? false)) return false;
  return store.stateValueAs<num>(surface.stateSlot, 'az') != null;
}

/// Nudges [surface] by [stepDeg] from its current readback — bounded to
/// ±[maintenanceNudgeStepDeg] and to the rotator's travel — as a set_az
/// tagged with the maintenance override, straight to the rotator. Returns
/// whether anything was published (see [canNudge]).
bool publishNudge(MqttService mqtt, BusStore store, RotatorSurface surface, double stepDeg) {
  if (!canNudge(store, surface) || stepDeg.isNaN || stepDeg == 0) return false;
  final az = store.stateValueAs<num>(surface.stateSlot, 'az')!.toDouble();
  final step = stepDeg.clamp(-maintenanceNudgeStepDeg, maintenanceNudgeStepDeg);
  final target = (az + step).clamp(_hfRotatorMinAz, _hfRotatorMaxAz).roundToDouble();
  mqtt.publish(cmdTopic(surface.cmdSlot), rotatorAzOverridePayload(target),
      retain: cmdRetain[surface.stateSlot] ?? false);
  return true;
}
