// sat_track.dart — the satellite the station is tracking, from
// muehle/uhf/sat-track.
//
// oscarwatch-sattrack-bridge (scmino) fronts the OscarWatch tracker on the
// shack PC and publishes the focused satellite as one retained /state
// snapshot: identity, look angle from the station, slant range and the
// sub-satellite point (sub_lat/sub_lng/alt_km, derived bridge-side from the
// look angle + station QTH). Satellite keys are JSON null — not omitted — when
// nothing is tracked or the tracker link is down. This model parses that
// snapshot out of the BusStore; nothing here talks MQTT.

import 'dart:math' as math;

import '../dxspot/projection.dart';
import 'bus_store.dart';

const String satTrackSlot = 'muehle/uhf/sat-track';

/// One tracked satellite, positioned. Only built when there is something to
/// draw: a tracked satellite with a sub-satellite point.
class SatTrack {
  final String name;
  final double lat; // sub-satellite point, degrees
  final double lng;
  final double? altKm; // height above the ellipsoid; null → no footprint
  final double? az; // look angle from the station, degrees
  final double? el;
  final double? rangeKm; // slant range
  final bool inRange; // OscarWatch inRange (above the horizon)

  const SatTrack({
    required this.name,
    required this.lat,
    required this.lng,
    this.altKm,
    this.az,
    this.el,
    this.rangeKm,
    this.inRange = false,
  });

  /// Parses a sat-track /state snapshot. Returns null unless a satellite is
  /// tracked, the tracker link is up (device_online) and the bridge resolved
  /// a sub-satellite point.
  static SatTrack? fromState(Map<String, dynamic>? s) {
    if (s == null || s['tracking'] != true || s['device_online'] != true) return null;
    final lat = _num(s['sub_lat']);
    final lng = _num(s['sub_lng']);
    if (lat == null || lng == null) return null;
    final name = s['sat_name'];
    return SatTrack(
      name: name is String && name.isNotEmpty ? name : 'SAT',
      lat: lat,
      lng: lng,
      altKm: _num(s['alt_km']),
      az: _num(s['az']),
      el: _num(s['el']),
      rangeKm: _num(s['range_km']),
      inRange: s['in_range'] == true,
    );
  }

  /// The satellite to draw from [store], or null. Two-layer liveness: our
  /// broker link, the bridge's /status, and the snapshot's device_online —
  /// a frozen position must never render.
  static SatTrack? fromStore(BusStore store) {
    if (!store.linkUp) return null;
    final slot = store.slots[satTrackSlot];
    if (slot == null || !slot.bridgeOnline) return null;
    return fromState(slot.state);
  }

  LatLng get position => (lat: lat, lng: lng);

  /// Footprint radius on the ground (km): the great-circle distance from the
  /// sub-satellite point to where the satellite sits on the horizon (0° el),
  /// Re · acos(Re / (Re + h)). Null without an altitude.
  double? get footprintRadiusKm {
    final h = altKm;
    if (h == null || h <= 0) return null;
    return earthRadiusKm * math.acos(earthRadiusKm / (earthRadiusKm + h));
  }

  /// Map label: name, elevation and slant range when known.
  String get label {
    final parts = <String>[name];
    final e = el;
    if (e != null) parts.add('EL ${e.round()}°');
    final r = rangeKm;
    if (r != null) parts.add('${r.round()} km');
    return parts.join('  ');
  }

  /// Repaint key: changes when anything the painter draws changes.
  String get paintKey => '$name|${lat.toStringAsFixed(3)}|${lng.toStringAsFixed(3)}|'
      '${altKm?.round()}|${el?.round()}|${rangeKm?.round()}|$inRange';

  static double? _num(dynamic v) => v is num ? v.toDouble() : null;
}
