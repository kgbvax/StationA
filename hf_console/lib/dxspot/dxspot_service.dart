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

class DxSpotService extends ChangeNotifier {
  final Map<String, DxSpot> _byKey = {};
  List<DxSpot> _spots = const [];

  String? _baseUrl;
  String? _locator;
  String? _callsign;
  double? _centerLat;
  double? _centerLng;

  bool _running = false;
  bool _connected = false;
  String? _error;
  int _backoff = _minBackoffSeconds;

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
    }
    notifyListeners();
  }

  List<DxSpot> get spots => _spots;
  double? get centerLat => _centerLat;
  double? get centerLng => _centerLng;
  bool get connected => _connected;
  String? get error => _error;
  bool get active => _centerLat != null; // overlay enabled iff a center exists

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
    final base = (_baseUrl == null || _baseUrl!.isEmpty) ? _defaultBaseUrl : _baseUrl!;
    final qth = (_callsign != null && _callsign!.isNotEmpty) ? _callsign! : _locator!;
    final url = '$base/api/stream?qth=${Uri.encodeComponent(qth)}&minutes=15';
    _source = createDxSpotSource();
    _source!.start(url, onData: _ingest, onDisconnected: _onDisconnected);
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
    // Need a callsign or grid to place/label the spot meaningfully.
    if (locator.isEmpty && (receiver == null || receiver.isEmpty) && (sender == null || sender.isEmpty)) {
      return;
    }

    final spot = DxSpot(
      lat: lat,
      lng: lng,
      snr: (j['snr'] as num?)?.toInt() ?? 0,
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
    _notify();
  }

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