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
const _historyMinutes = _maxAgeSeconds ~/ 60; // replay exactly the window we keep
// Safety bound on the derived dot list only; the grid squares aggregate the
// whole age window. At ~5 spots/s (qth without surroundings) the 10-min
// store holds well under this, so the bound is not hit in normal operation —
// a small freshest-N cap here was what made the map churn (every new spot
// evicted an old square).
const _maxSpots = 3000;
const _minBackoffSeconds = 2;
const _maxBackoffSeconds = 60;
const _notifyThrottle = Duration(milliseconds: 500);
const _pruneInterval = Duration(seconds: 60); // age out stale spots without new traffic

/// The horstreporter source families the overlay keeps: `mqtt` = PSKReporter
/// (horstreporter ingests only FT8/FT4 from it, mqtt.go pskrModeFeedsBaseline)
/// and `rbn` = Reverse Beacon Network skimmers (CW, plus some RTTY — the wire
/// carries no mode field to tell them apart). WSPR and DX-cluster are not
/// propagation evidence for the operator's bands and are dropped server-side
/// (`include_wspr=false&include_dxcluster=false`).
const Set<String> kDxSourceTypes = {'mqtt', 'rbn'};

/// One DX spot from horstreporter's v1 `streamSpot` (stream_wire.go). `lat`/`lng`
/// are already the *far* station's position. `sender`/`receiver` are present only
/// for `sourceType == "dxcluster"`.
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
  final int receivedAtMs; // wall-clock receive time
  /// Number of reports merged into this key while it stayed in the window.
  /// Drives the dominant-band count, like horstreporter's per-report tally.
  final int reports;

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
    this.reports = 1,
  });

  /// Absolute wall-clock time the report was made (receipt minus server age).
  /// Comparable across receipts — unlike [ageSeconds], which is relative to
  /// each spot's own receipt time.
  int get heardAtMs => receivedAtMs - ageSeconds * 1000;

  /// Callsign to label, if any (dxcluster `receiver` is the DX call); else the grid.
  String get label =>
      (receiver != null && receiver!.isNotEmpty) ? receiver! : ((sender != null && sender!.isNotEmpty) ? sender! : locator);

  /// The spot's true current age in seconds. Use with a single `nowMs` snapshot
  /// for gating/sort/prune.
  int liveAgeSecondsAt(int nowMs) => (nowMs - heardAtMs) ~/ 1000;
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
  final Map<String, int> bestSnr = {};
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

  /// Map a canonical radio mode (the `mode` key of `muehle/hf/radio` or
  /// `muehle/uhf/radio` /state — same vocabulary) onto the SNR family used by
  /// the filter. SSB family = usb/lsb/am/fm/data; CW family
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
  /// Test seam: when set, [DxSpotSource]s come from here instead of the
  /// platform factory, so re-subscribe behaviour is observable in tests.
  @visibleForTesting
  static DxSpotSource Function()? debugSourceFactory;

  final Map<String, DxSpot> _byKey = {};
  List<DxSpot> _spots = const [];
  List<GridSquare> _gridSquares = const [];

  /// `_byKey` changed since [_spots]/[_gridSquares] were last derived. The
  /// derivation (prune + full sort + grid grouping) is O(n log n) over every
  /// key heard in the last 10 minutes, so it runs lazily on read — at most
  /// once per repaint — instead of once per incoming spot. The connect
  /// replays 30 minutes of history as one SSE event per spot; rebuilding on
  /// each of them froze the page for minutes in browsers without a JIT
  /// (Safari's Enhanced Security web process, used for plain-http pages).
  bool _rebuildPending = false;
  int _rebuilds = 0;
  String _squareSig = ''; // _gridSquares content signature (keep the instance if unchanged)

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
  Set<String>? _enabledBands;

  /// The radio slot whose /state `mode` drives the SNR gate ([setMode]): the
  /// HF rig by default, the IC-9700 while the UHF/CAM pages show VHF spots.
  String modeRadio = hfRadioSlot;

  static const hfRadioSlot = 'muehle/hf/radio';
  static const uhfRadioSlot = 'muehle/uhf/radio';

  DxSpotSource? _source;
  Timer? _reconnect;
  Timer? _prune;
  Timer? _notifyPending;
  bool _dirty = false;

  /// Station QTH + endpoint config. Sets the projection center from [locator]; clears
  /// spots if no locator (overlay off). Does NOT (re)start the feed — callers start()
  /// or restart() as appropriate, so there is no double-restart on the change path.
  void configure({String? baseUrl, String? locator, String? callsign}) {
    final feedChanged = baseUrl != _baseUrl || locator != _locator || callsign != _callsign;
    _baseUrl = baseUrl;
    _locator = locator;
    _callsign = callsign;
    if (feedChanged) {
      // Spots of the previous QTH/endpoint must not linger until the age-out.
      _purge();
    }
    if (locator != null && locator.isNotEmpty) {
      final c = locatorToLatLng(locator);
      _centerLat = c.lat;
      _centerLng = c.lng;
    } else {
      _centerLat = null;
      _centerLng = null;
      _purge();
    }
    notifyListeners();
  }

  List<DxSpot> get spots {
    _rebuildIfPending();
    return _spots;
  }

  List<GridSquare> get gridSquares {
    _rebuildIfPending();
    return _gridSquares;
  }

  /// Number of times the spot list has been re-derived from the store.
  @visibleForTesting
  int get rebuildCount => _rebuilds;

  double? get centerLat => _centerLat;
  double? get centerLng => _centerLng;
  bool get connected => _connected;
  String? get error => _error;
  bool get active => _centerLat != null; // overlay enabled iff a center exists
  DxSpotFilter get filter => _filter;

  /// Update the SNR filter. The store keeps every spot; the gate applies when
  /// the visible lists are derived, so a change re-filters the whole window
  /// at once (no re-dial, no 10-minute mix of old and new thresholds).
  void setFilter(DxSpotFilter filter) {
    _filter = filter;
    _rebuildPending = true;
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
      _rebuildPending = true;
      notifyListeners();
    }
  }

  /// Restrict the subscription to these canonical bands (the UHF and CAM
  /// pages ask for {'6m','2m','70cm'}); null/empty = every band the feed carries. The
  /// narrowing happens SERVER-side: horstreporter's `enabled_bands` stream
  /// parameter drops non-matching spots before serialization, so the device
  /// never downloads or parses them. A change re-dials immediately — the SSE
  /// URL is wrong until it does — and the minutes-window replay refills the
  /// map from the narrowed feed. The store is purged with it: without the
  /// purge, spots from the previous band set would linger until the 10-minute
  /// age-out (the feed replay only ADDS spots, it never removes old ones).
  void setBands(Set<String>? bands) {
    final next = (bands == null || bands.isEmpty) ? null : Set.unmodifiable(bands);
    if (_bandsEqual(next, _enabledBands)) return;
    _enabledBands = next;
    _purge();
    notifyListeners();
    if (_running) _connect();
  }

  void _purge() {
    _byKey.clear();
    _spots = const [];
    _gridSquares = const [];
    _squareSig = '';
    _rebuildPending = false;
  }

  static bool _bandsEqual(Set<String>? a, Set<String>? b) {
    if (a == null || a.isEmpty) return b == null || b.isEmpty;
    if (b == null || b.isEmpty) return false;
    return a.length == b.length && a.containsAll(b);
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
      if (_byKey.length != before) _markChanged();
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
    final url = streamUrl(_baseUrl ?? '', qth, bands: _enabledBands);
    _source = debugSourceFactory != null ? debugSourceFactory!() : createDxSpotSource();
    _source!.start(url, onData: _ingest, onDisconnected: _onDisconnected);
  }

  /// Build the SSE URL used by this service. Public so tests can assert the
  /// query parameters without starting a real connection.
  ///
  /// - `qth` is the callsign when one is configured (reports of the own
  ///   station: who heard me / whom did I hear), else the locator (every
  ///   PSKReporter/RBN report to or from a station in that 4-char square).
  ///   No `surroundings`: that widens the match to the 3×3 block of squares
  ///   around the QTH — ~8× the traffic, a regional view rather than ours.
  /// - `minutes` matches the client's age window; older history would only be
  ///   downloaded and pruned.
  /// - WSPR and DX-cluster are dropped server-side ([kDxSourceTypes]).
  /// - [bands] narrows via horstreporter's `enabled_bands` parameter.
  static String streamUrl(String baseUrl, String qth, {Set<String>? bands}) {
    final base = baseUrl.isEmpty ? _defaultBaseUrl : baseUrl;
    var url = '$base/api/stream?qth=${Uri.encodeComponent(qth)}&minutes=$_historyMinutes'
        '&include_wspr=false&include_dxcluster=false';
    if (bands != null && bands.isNotEmpty) {
      url += '&enabled_bands=${bands.join(',')}';
    }
    return url;
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
    // PSKReporter (FT8/FT4) and RBN (CW) only; the server already drops WSPR
    // and DX-cluster, this is a guard.
    if (!kDxSourceTypes.contains(sourceType)) return;

    final snr = (j['snr'] as num?)?.toInt() ?? 0;
    final ageSeconds = (j['ageSeconds'] as num?)?.toInt() ?? 0;
    // The server's history cutoff is not exact (arrival-ordered history), so
    // drop what the prune would remove on the next rebuild anyway.
    if (ageSeconds > _maxAgeSeconds) return;

    // Need a callsign or grid to place/label the spot meaningfully.
    if (locator.isEmpty && (receiver == null || receiver.isEmpty) && (sender == null || sender.isEmpty)) {
      return;
    }

    final spot = DxSpot(
      lat: lat,
      lng: lng,
      snr: snr,
      ageSeconds: ageSeconds,
      locator: locator,
      band: band,
      sourceType: sourceType,
      sender: sender,
      receiver: receiver,
      receivedAtMs: DateTime.now().millisecondsSinceEpoch,
    );

    final key = '$locator|${receiver ?? ''}|${sender ?? ''}|$band|$sourceType';
    final prev = _byKey[key];
    // The latest report wins (its SNR included), compared by absolute heard
    // time: ageSeconds values from different receipts are not comparable. A
    // replayed/late older report of the same key only bumps the report count;
    // it must not make the key look fresh (a reconnect replays the window).
    if (prev == null) {
      _byKey[key] = spot;
    } else if (spot.heardAtMs >= prev.heardAtMs) {
      _byKey[key] = DxSpot(
        lat: spot.lat,
        lng: spot.lng,
        snr: spot.snr,
        ageSeconds: spot.ageSeconds,
        locator: spot.locator,
        band: spot.band,
        sourceType: spot.sourceType,
        sender: spot.sender,
        receiver: spot.receiver,
        receivedAtMs: spot.receivedAtMs,
        reports: prev.reports + 1,
      );
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
        receivedAtMs: prev.receivedAtMs,
        reports: prev.reports + 1,
      );
    }

    _connected = true;
    _error = null;
    _backoff = _minBackoffSeconds; // reset on a good event
    _markChanged();
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
      acc.counts[band] = (acc.counts[band] ?? 0) + s.reports;
      final best = acc.bestSnr[band];
      if (best == null || s.snr > best) acc.bestSnr[band] = s.snr;
      acc.snrs.add(s.snr);
    }
    // Stable order (by locator) and a deterministic dominant-band tie-break
    // (count, then best SNR, then band name): input order is freshness order,
    // and letting it decide made squares flip colour without new data.
    final locs = byLoc.keys.toList()..sort();
    final out = <GridSquare>[];
    for (final loc in locs) {
      final acc = byLoc[loc]!;
      String dominant = '';
      int maxCount = 0;
      int maxSnr = 0;
      acc.counts.forEach((band, count) {
        final snr = acc.bestSnr[band]!;
        if (dominant.isEmpty ||
            count > maxCount ||
            (count == maxCount && (snr > maxSnr || (snr == maxSnr && band.compareTo(dominant) < 0)))) {
          maxCount = count;
          maxSnr = snr;
          dominant = band;
        }
      });
      if (dominant.isEmpty) continue;
      final score = _topQuartileMean(acc.snrs);
      out.add(GridSquare(
        locator: loc,
        dominantBand: dominant,
        snrs: acc.snrs,
        score: score.isFinite ? score : 0.0,
      ));
    }
    return List.unmodifiable(out);
  }

  /// The store changed: mark the derived lists stale and schedule the
  /// throttled repaint. The O(n log n) derivation itself waits for the next
  /// read ([_rebuildIfPending]).
  void _markChanged() {
    _rebuildPending = true;
    _notify();
  }

  /// Prune stale spots (live age > [_maxAgeSeconds]), apply the SNR gate and
  /// derive the visible lists. The grid squares aggregate EVERY spot in the
  /// window (as horstreporter's own map does); only the dot list is bounded.
  /// When the squares come out identical, the previous list instance is kept so
  /// the painters (identity-based shouldRepaint) skip the repaint.
  void _rebuildIfPending() {
    if (!_rebuildPending) return;
    _rebuildPending = false;
    _rebuilds++;
    final now = DateTime.now().millisecondsSinceEpoch;
    _byKey.removeWhere((_, s) => s.liveAgeSecondsAt(now) > _maxAgeSeconds);
    final visible = _byKey.values.where((s) => _filter.allows(s.snr)).toList();
    final squares = _collectGridSquares(visible);
    final sig = _signature(squares);
    if (sig != _squareSig) {
      _squareSig = sig;
      _gridSquares = squares;
    }
    if (visible.length > _maxSpots) {
      visible.sort((a, b) => b.heardAtMs.compareTo(a.heardAtMs));
      _spots = List.unmodifiable(visible.sublist(0, _maxSpots));
    } else {
      _spots = List.unmodifiable(visible);
    }
  }

  /// What the painters draw per square: locator, colour and opacity score.
  static String _signature(List<GridSquare> squares) {
    final b = StringBuffer();
    for (final q in squares) {
      b
        ..write(q.locator)
        ..write(q.dominantBand)
        ..write(q.score.round())
        ..write(';');
    }
    return b.toString();
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