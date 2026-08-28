// dxspot_service.dart — owns the horstreporter /api/stream SSE feed for the compass.
//
// A ChangeNotifier the compass panel watches. It builds the SSE URL from the station
// locator/callsign + horstreporter base URL, drives a platform DxSpotSource, and keeps a
// deduped, age-capped, throttled list of recent DX spots plus the AEQD projection
// center (derived from the station locator). No shari dependency: the app talks to
// horstreporter directly.
//
// Reconnect/backoff lives here (not in the source) so feed health is surfaced the same
// way on Android (dart:io) and web (EventSource). Modelled on flexbridge's
// radioLoop/sleepCtx/scaleBackoff: 2s × 1.5 → 60s cap.

import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';

import 'dxspot_source.dart';
import 'dxspot_source_io.dart' if (dart.library.html) 'dxspot_source_web.dart';
import 'projection.dart';

const _defaultBaseUrl = 'https://horstreporter.kgbvax.net';
const _maxAgeSeconds = 600; // drop spots not heard again within 10 min
const _maxSpots = 80;
const _minBackoffSeconds = 2;
const _maxBackoffSeconds = 60;
const _notifyThrottle = Duration(milliseconds: 500);
const _pruneInterval = Duration(seconds: 60); // age out stale spots without new traffic

/// One DX spot from horstreporter's `streamSpot` (server.go:233-244). `lat`/`lng` are
/// already the *remote* station's position. `sender`/`receiver` are present only for
/// `sourceType == "dxcluster"`.
class DxSpot {
  final double lat;
  final double lng;
  final int snr;
  final int ageSeconds;
  final String locator;
  final String band;
  final String sourceType;
  final String? sender;
  final String? receiver;
  final int receivedAtMs; // wall-clock receive time (for aging out)

  DxSpot({
    required this.lat,
    required this.lng,
    required this.snr,
    required this.ageSeconds,
    required this.locator,
    required this.band,
    required this.sourceType,
    this.sender,
    this.receiver,
    required this.receivedAtMs,
  });

  /// Callsign to label, if any (dxcluster `receiver` is the DX call); else the grid.
  String get label =>
      (receiver != null && receiver!.isNotEmpty) ? receiver! : ((sender != null && sender!.isNotEmpty) ? sender! : locator);

  /// Server age plus elapsed since receipt — the spot's true current age in seconds.
  /// Use this (with a single `nowMs` snapshot) for freshness gating/sort/prune so a
  /// spot kept across re-broadcasts doesn't keep a frozen first-seen age.
  int liveAgeSecondsAt(int nowMs) => ageSeconds + ((nowMs - receivedAtMs) ~/ 1000);
}

/// Maidenhead 4-character grid-square aggregate of the spots currently in view.
/// Mirrors `horstreporter/azimuth-runtime.js:499-512` `collectGridSquares`:
/// one entry per 4-char locator prefix, with the dominant band (the band with
/// the most reports in that square) and the snrs used by the opacity ramp. The
/// `locator` is the canonical 4-char key so the painter can look up the bounds
/// via `locatorToBounds(locator)`.
class GridSquare {
  final String locator;
  final String dominantBand;
  final List<int> snrs;
  final double score; // top-quartile mean SNR, used for opacity ramp
  const GridSquare({
    required this.locator,
    required this.dominantBand,
    required this.snrs,
    required this.score,
  });
}

class _SquareAccum {
  final Map<String, int> counts = {};
  final List<int> snrs = [];
}

/// Mode-aware SNR threshold for the DX spot feed. Mirrors the `min_snr_mode` /
/// `ssb_min_db` / `cw_min_db` parameters used by horstreporter's stream filter
/// (`server.go:506-528`). A mode of `'none'` disables SNR gating.
class DxSpotFilter {
  final String mode; // 'none' | 'ssb' | 'cw'
  final int ssbMinDb;
  final int cwMinDb;

  const DxSpotFilter({
    this.mode = 'ssb',
    this.ssbMinDb = 0,
    this.cwMinDb = -15,
  });

  /// Map a canonical radio mode (from `muehle/hf/radio/state.mode`) onto the
  /// SNR family used by the filter. SSB family = usb/lsb/am/fm/data; CW family
  /// = cw. Everything else disables gating.
  static String snrModeFor(String? radioMode) {
    final m = (radioMode ?? '').trim().toLowerCase();
    switch (m) {
      case 'usb':
      case 'lsb':
      case 'am':
      case 'fm':
      case 'data':
        return 'ssb';
      case 'cw':
        return 'cw';
      default:
        return 'none';
    }
  }

  int? get threshold {
    switch (mode) {
      case 'ssb':
        return ssbMinDb;
      case 'cw':
        return cwMinDb;
      default:
        return null;
    }
  }

  bool allows(int snr) {
    final t = threshold;
    if (t == null) return true;
    return snr >= t;
  }
}

class DxSpotService extends ChangeNotifier {
  final Map<String, DxSpot> _byKey = {};
  List<DxSpot> _spots = const [];
  List<GridSquare> _gridSquares = const [];

  String? _baseUrl;
  String? _locator;
  String? _callsign;
  double? _centerLat;
  double? _centerLng;

  bool _running = false;
  bool _connected = false;
  String? _error;
  int _backoff = _minBackoffSeconds;
  DxSpotFilter _filter = const DxSpotFilter();

  DxSpotSource? _source;
  Timer? _reconnect;
  Timer? _prune;
  Timer? _notifyPending;
  bool _dirty = false;

  /// Station QTH + endpoint config. Sets the projection center from [locator]; clears
  /// spots if no locator (overlay off). Does NOT (re)start the feed — callers start()
  /// or restart() as appropriate, so there is no double-restart on the change path.
  void configure({String? baseUrl, String? locator, String? callsign}) {
    _baseUrl = baseUrl;
    _locator = locator;
    _callsign = callsign;
    if (locator != null && locator.isNotEmpty) {
      final c = locatorToLatLng(locator);
      _centerLat = c.lat;
      _centerLng = c.lng;
    } else {
      _centerLat = null;
      _centerLng = null;
      _byKey.clear();
      _spots = const [];
      _gridSquares = const [];
    }
    notifyListeners();
  }

  List<DxSpot> get spots => _spots;
  List<GridSquare> get gridSquares => _gridSquares;
  double? get centerLat => _centerLat;
  double? get centerLng => _centerLng;
  bool get connected => _connected;
  String? get error => _error;
  bool get active => _centerLat != null; // overlay enabled iff a center exists
  DxSpotFilter get filter => _filter;

  /// Update the SNR filter. Changing the filter does NOT clear existing spots,
  /// but a reconnect will re-ingest history with the new threshold; live spots
  /// are re-evaluated on the next broadcast.
  void setFilter(DxSpotFilter filter) {
    _filter = filter;
    notifyListeners();
  }

  /// Convenience: set the filter family from a live radio mode and keep the
  /// current dB thresholds.
  void setMode(String? radioMode) {
    final next = DxSpotFilter(
      mode: DxSpotFilter.snrModeFor(radioMode),
      ssbMinDb: _filter.ssbMinDb,
      cwMinDb: _filter.cwMinDb,
    );
    if (next.mode != _filter.mode ||
        next.ssbMinDb != _filter.ssbMinDb ||
        next.cwMinDb != _filter.cwMinDb) {
      _filter = next;
      notifyListeners();
    }
  }

  void start() {
    if (_running) return;
    if (!active) return; // idle: no locator → beam-only compass
    _running = true;
    _backoff = _minBackoffSeconds;
    // Age out stale spots even when the feed is quiet (prune is otherwise coupled to
    // _ingest, so a silent/half-open feed would leave stale dots on the compass).
    _prune?.cancel();
    _prune = Timer.periodic(_pruneInterval, (_) {
      if (!_running) return;
      final now = DateTime.now().millisecondsSinceEpoch;
      final before = _byKey.length;
      _byKey.removeWhere((_, s) => s.liveAgeSecondsAt(now) > _maxAgeSeconds);
      if (_byKey.length != before) _rebuildAndNotify();
    });
    _connect();
  }

  void stop() {
    _running = false;
    _reconnect?.cancel();
    _reconnect = null;
    _prune?.cancel();
    _prune = null;
    _source?.stop();
    _source = null;
    _connected = false;
    _error = null;
    _notifyPending?.cancel();
    _notifyPending = null;
    _dirty = false;
    notifyListeners();
  }

  void restart() {
    stop();
    start();
  }

  @override
  void dispose() {
    stop();
    super.dispose();
  }

  void _connect() {
    if (!_running || !active) return;
    _source?.stop();
    final qth = (_callsign != null && _callsign!.isNotEmpty) ? _callsign! : _locator!;
    final url = streamUrl(_baseUrl ?? '', qth);
    _source = createDxSpotSource();
    _source!.start(url, onData: _ingest, onDisconnected: _onDisconnected);
  }

  /// Build the SSE URL used by this service. Public so tests can assert the
  /// query parameters without starting a real connection.
  static String streamUrl(String baseUrl, String qth) {
    final base = baseUrl.isEmpty ? _defaultBaseUrl : baseUrl;
    return '$base/api/stream?qth=${Uri.encodeComponent(qth)}&minutes=30&surroundings=true';
  }

  void _onDisconnected() {
    if (!_running) return;
    _source?.stop();
    _source = null;
    _connected = false;
    _error = 'horstreporter feed down';
    _notify();
    final d = Duration(seconds: _backoff);
    _reconnect?.cancel();
    _reconnect = Timer(d, () {
      if (!_running) return;
      _backoff = (_backoff * 3 ~/ 2); // ×1.5
      if (_backoff > _maxBackoffSeconds) _backoff = _maxBackoffSeconds;
      _connect();
    });
  }

  void _ingest(String data) {
    if (!_running) return;
    _processSpotJson(data);
  }

  @visibleForTesting
  void ingest(String data) => _processSpotJson(data);

  void _processSpotJson(String data) {
    dynamic j;
    try {
      j = jsonDecode(data);
    } catch (_) {
      return;
    }
    if (j is! Map) return;
    final lat = (j['lat'] as num?)?.toDouble();
    final lng = (j['lng'] as num?)?.toDouble();
    if (lat == null || lng == null) return;

    final locator = (j['locator'] as String?) ?? '';
    final receiver = j['receiver'] as String?;
    final sender = j['sender'] as String?;
    final band = (j['band'] as String?) ?? '';
    final sourceType = (j['sourceType'] as String?) ?? '';
    // The console DX overlay mirrors horstreporter's own azimuthal view, which
    // only consumes FT8/FT4 spots published as sourceType == "mqtt". Drop the
    // other source families (dxcluster, rbn, wspr) at ingest so the band-key
    // rail and grid-square palette stay consistent with the web frontend.
    if (sourceType != 'mqtt') return;

    final snr = (j['snr'] as num?)?.toInt() ?? 0;
    // Mode-aware SNR gate: only FT8/FT4 (mqtt) spots are filtered this way,
    // matching horstreporter's streamClientFilter.
    if (!_filter.allows(snr)) return;

    // Need a callsign or grid to place/label the spot meaningfully.
    if (locator.isEmpty && (receiver == null || receiver.isEmpty) && (sender == null || sender.isEmpty)) {
      return;
    }

    final spot = DxSpot(
      lat: lat,
      lng: lng,
      snr: snr,
      ageSeconds: (j['ageSeconds'] as num?)?.toInt() ?? 0,
      locator: locator,
      band: band,
      sourceType: sourceType,
      sender: sender,
      receiver: receiver,
      receivedAtMs: DateTime.now().millisecondsSinceEpoch,
    );

    final key = '$locator|${receiver ?? ''}|${sender ?? ''}|$band|$sourceType';
    final prev = _byKey[key];
    // Keep the freshest (lowest ageSeconds); tie-break by stronger snr. Either way the
    // key was heard NOW, so refresh its last-heard time — otherwise an actively
    // re-spotted station whose first spot had the best SNR would never get its
    // receivedAtMs updated and the age-prune would blink it off every 10 min.
    if (prev == null ||
        spot.ageSeconds < prev.ageSeconds ||
        (spot.ageSeconds == prev.ageSeconds && spot.snr > prev.snr)) {
      _byKey[key] = spot;
    } else {
      _byKey[key] = DxSpot(
        lat: prev.lat,
        lng: prev.lng,
        snr: prev.snr,
        ageSeconds: prev.ageSeconds,
        locator: prev.locator,
        band: prev.band,
        sourceType: prev.sourceType,
        sender: prev.sender,
        receiver: prev.receiver,
        receivedAtMs: spot.receivedAtMs,
      );
    }

    _connected = true;
    _error = null;
    _backoff = _minBackoffSeconds; // reset on a good event
    _rebuildAndNotify();
  }

  /// Mean of the top quarter of SNR values. Mirrors
  /// `horstreporter/static/utils.js:639-650` so a square's opacity ramp uses
  /// the same score as the web frontend.
  static double _topQuartileMean(List<int> snrs) {
    if (snrs.isEmpty) return double.nan;
    final sorted = List<int>.from(snrs)..sort((a, b) => b - a);
    final k = (sorted.length / 4).ceil().clamp(1, sorted.length);
    var sum = 0;
    for (var i = 0; i < k; i++) {
      sum += sorted[i];
    }
    return sum / k;
  }

  /// Group spots by 4-character Maidenhead locator prefix and compute each
  /// square's dominant band + top-quartile SNR score. Mirrors
  /// `horstreporter/azimuth-runtime.js:499-512`.
  static List<GridSquare> _collectGridSquares(List<DxSpot> spots) {
    final byLoc = <String, _SquareAccum>{};
    for (final s in spots) {
      final raw = s.locator.toUpperCase();
      if (raw.length < 4) continue;
      final loc = raw.substring(0, 4);
      final band = s.band;
      if (band.isEmpty) continue;
      final acc = byLoc.putIfAbsent(loc, () => _SquareAccum());
      acc.counts[band] = (acc.counts[band] ?? 0) + 1;
      acc.snrs.add(s.snr);
    }
    final out = <GridSquare>[];
    for (final entry in byLoc.entries) {
      String dominant = '';
      int maxCount = 0;
      entry.value.counts.forEach((band, count) {
        if (count > maxCount) {
          maxCount = count;
          dominant = band;
        }
      });
      if (dominant.isEmpty) continue;
      final score = _topQuartileMean(entry.value.snrs);
      out.add(GridSquare(
        locator: entry.key,
        dominantBand: dominant,
        snrs: entry.value.snrs,
        score: score.isFinite ? score : 0.0,
      ));
    }
    return List.unmodifiable(out);
  }

  /// Prune stale spots (live age > [_maxAgeSeconds]), cap to [_maxSpots] by freshness,
  /// then throttle [notifyListeners]. Freshness uses live age (server age + elapsed
  /// since receipt), computed against a single `now` so the comparator is consistent.
  void _rebuildAndNotify() {
    final now = DateTime.now().millisecondsSinceEpoch;
    _byKey.removeWhere((_, s) => s.liveAgeSecondsAt(now) > _maxAgeSeconds);
    final list = _byKey.values.toList()
      ..sort((a, b) {
        final age = a.liveAgeSecondsAt(now).compareTo(b.liveAgeSecondsAt(now));
        return age != 0 ? age : b.snr.compareTo(a.snr);
      });
    _spots = list.length > _maxSpots ? List.unmodifiable(list.sublist(0, _maxSpots)) : List.unmodifiable(list);
    _gridSquares = _collectGridSquares(_spots);
    _notify();
  }


  @visibleForTesting
  static double topQuartileMean(List<int> snrs) => _topQuartileMean(snrs);

  /// Coalesce notifyListeners to ≤ ~2 Hz so a busy FT8 band doesn't repaint-storm.
  void _notify() {
    _dirty = true;
    if (_notifyPending != null) return;
    _notifyPending = Timer(_notifyThrottle, () {
      _notifyPending = null;
      if (_dirty) {
        _dirty = false;
        notifyListeners();
      }
    });
  }
}