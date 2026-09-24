// vhfcam_service.dart — console client for vhfcam-restream's preview server
// (:8083 on shari). Independent of the MQTT bus: the vhfcam slot publishes no
// streaming/audio state (its /state is overlay bookkeeping only), so radio and
// stream health come from the server's own HTTP planes:
//
//   GET  /api/radio-status   IC-9700 audio-chain LEDs (2 s poll, like the page)
//   GET  /hls/live.m3u8      exists only while the preview sink is running —
//                            the process is installed disabled-at-boot and
//                            started ad hoc, so 404 is a *normal* state
//   POST /api/cmd/{action}   audio_on | audio_off | power_on (allowlist; the
//                            server translates these to muehle/uhf/radio/cmd)
//
// Tolerant by design: the cam is an ad-hoc accessory, not a station slot, so
// an unreachable server is a UI state, never a fault-bar entry and never a
// log-worthy error. Failures back the poll off to 10 s to stay polite to the
// Pi. On web the transport stub reports unreachable and the CAM page shows its
// placeholder.

import 'dart:async';
import 'dart:convert';

import 'package:flutter/foundation.dart';

import 'radio_status.dart';
import 'vhfcam_transport_io.dart' if (dart.library.html) 'vhfcam_transport_web.dart';

const defaultVhfcamBaseUrl = 'http://192.168.1.139:8083';

const _pollInterval = Duration(seconds: 2);
const _pollBackoff = Duration(seconds: 10);
const _requestTimeout = Duration(seconds: 3);
const _streamProbeInterval = Duration(seconds: 10);
const _backoffAfterFailures = 3;

/// The exact command set the preview server allowlists; anything else is
/// rejected client-side too.
const vhfcamCommands = {'audio_on', 'audio_off', 'power_on'};

class VhfcamService extends ChangeNotifier {
  String _baseUrl = defaultVhfcamBaseUrl;
  RadioStatus? _status;
  bool serverOnline = false;
  bool streamLive = false;

  Timer? _timer;
  bool _ticking = false;
  int _failures = 0;
  DateTime _lastStreamProbe = DateTime.fromMillisecondsSinceEpoch(0);

  String get baseUrl => _baseUrl;
  RadioStatus? get status => _status;

  /// Live playlist URL for the player.
  String get hlsUrl => '$_baseUrl/hls/live.m3u8';

  /// Re-points the service (gear-sheet live-apply). An empty value restores
  /// the shack default.
  void configure({required String baseUrl}) {
    final trimmed = baseUrl.trim();
    final next = trimmed.isEmpty ? defaultVhfcamBaseUrl : trimmed;
    if (next == _baseUrl) return;
    _baseUrl = next;
    _failures = 0;
    _lastStreamProbe = DateTime.fromMillisecondsSinceEpoch(0);
    notifyListeners();
    _tick(); // re-poll immediately under the new base URL
  }

  /// Starts the poll loop. Idempotent; runs for the app's lifetime (the poll
  /// is a 100-byte JSON every 2 s — cheaper than managing page lifecycle).
  void start() {
    if (_timer != null) return;
    _tick();
  }

  Future<void> _tick() async {
    if (_ticking) return;
    _ticking = true;
    try {
      await _pollStatus();
      await _probeStream();
    } finally {
      _ticking = false;
    }
    notifyListeners();
    _schedule();
  }

  void _schedule() {
    _timer?.cancel();
    final delay = _failures >= _backoffAfterFailures ? _pollBackoff : _pollInterval;
    _timer = Timer(delay, _tick);
  }

  Future<void> _pollStatus() async {
    final (ok, body) = await vhfcamHttpGet('$_baseUrl/api/radio-status', _requestTimeout);
    RadioStatus? parsed;
    if (ok) {
      try {
        parsed = RadioStatus.fromJson(jsonDecode(body) as Map<String, dynamic>);
      } catch (_) {
        parsed = null; // malformed body == unreachable for our purposes
      }
    }
    serverOnline = parsed != null;
    if (parsed != null) _status = parsed;
    _failures = parsed == null ? _failures + 1 : 0;
  }

  /// Existence-probe of the live playlist. Skipped while the server is
  /// unreachable (the sink cannot be up behind a dead server) and rate-limited
  /// to one probe per [_streamProbeInterval] — the poll tick runs at 2 s.
  Future<void> _probeStream() async {
    if (!serverOnline) {
      streamLive = false;
      return;
    }
    final now = DateTime.now();
    // Rate-limit both directions (rising and falling edge) to one probe per
    // interval — a stopped sink must not turn the 2 s poll into a 2 s playlist
    // hammer, and a just-started sink is allowed up to 10 s of detection lag.
    if (now.difference(_lastStreamProbe) < _streamProbeInterval) return;
    _lastStreamProbe = now;
    final (ok, _) = await vhfcamHttpGet(hlsUrl, _requestTimeout);
    streamLive = ok;
  }

  /// Forces an immediate re-probe (feed retry button) without waiting for the
  /// next scheduled tick.
  Future<void> probeNow() => _tick();

  /// Seeds the status the way a successful poll would, without a server
  /// round-trip. Widget tests only — the poll loop never runs under test.
  @visibleForTesting
  void seedStatus({required bool online, bool stream = false, RadioStatus? status}) {
    serverOnline = online;
    streamLive = stream;
    _status = status;
    _failures = 0;
    notifyListeners();
  }

  /// POSTs one of [vhfcamCommands] to the preview server. Returns whether the
  /// server accepted it. The vhfcam slot itself has no /cmd plane — the server
  /// translates this into a muehle/uhf/radio/cmd publish.
  Future<bool> sendCommand(String action) async {
    if (!vhfcamCommands.contains(action) || !serverOnline) return false;
    final ok = await vhfcamHttpPost('$_baseUrl/api/cmd/$action', _requestTimeout);
    // Refresh the LEDs right away instead of on the next tick — the button
    // feedback is the poll result, and 2 s of stale LED reads as a dead button.
    _tick();
    return ok;
  }

  @override
  void dispose() {
    _timer?.cancel();
    _timer = null;
    super.dispose();
  }
}
