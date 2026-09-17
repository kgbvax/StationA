// selected_spot.dart — the operator-keyed station from muehle/hf/spots.
//
// logger-spot-bridge (shack PC) publishes the station the operator has keyed
// in the logging software (DXLog `lookupinfo` / Log4OM outbound CALLSIGN) as
// the retained /state `selected` record: callsign, RF context, map position
// and the beam answer (azimuth + distance_km). This model parses that record
// out of the BusStore snapshot; nothing here talks MQTT.
//
// Terminology (user's ask): this is NOT a spot — a call entered by hand must
// behave exactly like one picked from a spot list. The `spots` slot name is
// historical from the logging-integration draft (role `bandmap` at `…/spots`).
//
// The bridge resolves position and bearing; the console only renders (and
// ages out: dim after 5 min, gone after 15 min — see selectedStaleness).

/// Age buckets the painters/renderers act on. A selection older than
/// [staleAfter] renders dimmed (the operator probably moved on); past
/// [expiredAfter] it disappears.
const Duration _staleAfter = Duration(minutes: 5);
const Duration _expiredAfter = Duration(minutes: 15);

/// Whether the selection is fresh, stale (dim it) or expired (hide it) at
/// [ageSeconds].
enum SelectedStaleness { fresh, stale, expired }

SelectedStaleness stalenessFor(int ageSeconds) {
  if (ageSeconds >= _expiredAfter.inSeconds) return SelectedStaleness.expired;
  if (ageSeconds >= _staleAfter.inSeconds) return SelectedStaleness.stale;
  return SelectedStaleness.fresh;
}

/// Whether the bottom-left selected-station chip is on screen at [nowMs]:
/// a parseable selection that has not expired (dim at 5 min, gone at
/// 15 min). Shared by the compass panel (which renders the chip) and
/// DxMapContainer (which lifts the dragon above it).
bool selectedChipVisible(dynamic raw, int nowMs) {
  final sel = SelectedSpot.fromSelected(raw);
  return sel != null && stalenessFor(sel.ageSecondsAt(nowMs)) != SelectedStaleness.expired;
}

/// One operator-keyed station, exactly as the bridge published it. Nullable
/// doubles mean "the logger did not send / the bridge could not derive" — the
/// painters degrade per field (pin needs lat/lng, the bearing ray needs
/// azimuth).
class SelectedSpot {
  final String call;
  final int? freqHz;
  final String band;
  final String mode;
  final String locator;
  final double? lat;
  final double? lng;
  final double? azimuth; // degrees true, 0..360
  final double? distanceKm;
  final String countryPrefix;
  final String source; // "dxlog" | "log4om" | … (listener name)
  final int tsMs; // selection wall-clock (UTC), for age-out

  const SelectedSpot({
    required this.call,
    this.freqHz,
    this.band = '',
    this.mode = '',
    this.locator = '',
    this.lat,
    this.lng,
    this.azimuth,
    this.distanceKm,
    this.countryPrefix = '',
    this.source = '',
    required this.tsMs,
  });

  /// Parses the `selected` value of the `muehle/hf/spots` /state snapshot.
  /// Returns null when absent, not a map, or carrying no callsign (the
  /// bridge's clear signal is `selected` omitted; an empty call is rejected
  /// here as well, so a half-written state can never render a bare pin).
  static SelectedSpot? fromSelected(dynamic raw) {
    if (raw is! Map) return null;
    final call = (raw['call'] as String?)?.trim() ?? '';
    if (call.isEmpty) return null;

    final ts = DateTime.tryParse((raw['ts'] as String?) ?? '');
    return SelectedSpot(
      call: call.toUpperCase(),
      freqHz: (raw['freq_hz'] as num?)?.toInt(),
      band: (raw['band'] as String?) ?? '',
      mode: (raw['mode'] as String?) ?? '',
      locator: ((raw['locator'] as String?) ?? '').toUpperCase(),
      lat: (raw['lat'] as num?)?.toDouble(),
      lng: (raw['lng'] as num?)?.toDouble(),
      azimuth: (raw['azimuth'] as num?)?.toDouble(),
      distanceKm: (raw['distance_km'] as num?)?.toDouble(),
      countryPrefix: ((raw['country_prefix'] as String?) ?? '').toUpperCase(),
      source: (raw['source'] as String?) ?? '',
      tsMs: ts?.millisecondsSinceEpoch ?? 0,
    );
  }

  /// Age in seconds against a single [nowMs] snapshot (painters take one
  /// `now` per frame so every readout in the frame agrees).
  int ageSecondsAt(int nowMs) =>
      tsMs <= 0 ? _expiredAfter.inSeconds + 1 : ((nowMs - tsMs) ~/ 1000).clamp(0, 1 << 31);

  /// True when the record carries a usable map position (the Mercator pin
  /// and the compass dot both need coordinates).
  bool get hasPosition => lat != null && lng != null;

  /// True when a bearing ray can be drawn even without coordinates.
  bool get hasBearing => azimuth != null;
}
