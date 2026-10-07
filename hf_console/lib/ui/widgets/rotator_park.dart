import '../../mqtt/mqtt_service.dart';
import '../../store/bus_store.dart';
import '../../store/wiring.dart';

/// PARK for a [RotatorSurface] with a bridge-side park (the sat mount):
/// shared by the sat-rotator panel (UHF) and the VHF map rail (UHF + CAM).
///
/// The bridge owns the park position (spid-ercm-rotator-bridge
/// `control.<axis>.park`, published as /meta `capabilities.limits.park`);
/// the console only sends the intent and labels the key from /meta.

/// The first of the surface's slots that is operable (bridge online AND the
/// console link up), or null — the one slot a park goes to. Park is a
/// mount-level intent, so a second publish would only re-run its stop phase.
String? rotatorParkSlot(BusStore store, RotatorSurface surface) {
  if (surface.parkPayload == null || !store.linkUp) return null;
  for (final slot in surface.stopSlots) {
    if (store.slots['muehle/$slot']?.isOnline ?? false) return slot;
  }
  return null;
}

/// Publishes the park intent to [rotatorParkSlot]; false when none is operable.
bool sendRotatorPark(BusStore store, MqttService mqtt, RotatorSurface surface) {
  final slot = rotatorParkSlot(store, surface);
  if (slot == null) return false;
  mqtt.publish(
    cmdTopic(slot),
    surface.parkPayload!(),
    retain: cmdRetain['muehle/$slot'] ?? false,
  );
  return true;
}

/// The configured park position of one slot, from /meta
/// `capabilities.limits.park`; null while /meta is absent or malformed.
double? slotParkDeg(BusStore store, String slot) {
  final caps = store.slots['muehle/$slot']?.meta?['capabilities'];
  final limits = caps is Map ? caps['limits'] : null;
  final park = limits is Map ? limits['park'] : null;
  return park is num ? park.toDouble() : null;
}

/// "200° / 0°" — every axis's park position, '—' for an axis without /meta.
String rotatorParkLabel(BusStore store, RotatorSurface surface) =>
    surface.stopSlots.map((slot) {
      final deg = slotParkDeg(store, slot);
      return deg == null ? '—' : '${fmtParkDeg(deg)}°';
    }).join(' / ');

/// Whole degrees print without decimals (200 not 200.0).
String fmtParkDeg(double v) =>
    v == v.truncateToDouble() ? v.truncate().toString() : v.toString();
