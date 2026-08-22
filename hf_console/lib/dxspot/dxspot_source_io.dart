// dxspot_source_io.dart — native (Android / desktop / test) SSE client via dart:io.
//
// dart:io has no EventSource, so we stream the response body and parse SSE frames
// ourselves. No internal reconnect: on EOF/error we call onDisconnected and let the
// service own backoff. `Accept-Encoding: identity` keeps the long-lived stream as
// plain text (gzip on a streaming body is awkward to decode incrementally).
//
// Liveness: horstreporter's live SSE loop sends NO keepalive comments — it only
// writes `data:` when a spot arrives (server.go:392-410). So a half-open TCP socket
// (NAT/firewall dropped the connection without FIN/RST) would otherwise hang
// forever: the response stream never completes or errors, onDone/onError never
// fire, and the service never reconnects. We guard two ways:
//   1. HttpClient.connectionTimeout bounds the connect phase (server accepts TCP but
//      never answers the HTTP request).
//   2. An idle watchdog resets on EVERY received line; if nothing arrives for
//      [_idleTimeout] we force-close and call onDisconnected so backoff takes over.
//      horstreporter republishes the last `minutes=15` of spots on reconnect, so a
//      periodic reconnect on a genuinely quiet band is a harmless refresh (the
//      overlay keeps showing recent activity) while still recovering a dead link.

import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'dxspot_source.dart';

const _connectTimeout = Duration(seconds: 15);
const _idleTimeout = Duration(minutes: 5);

class _IoDxSpotSource extends DxSpotSource {
  HttpClient? _client;
  StreamSubscription<String>? _sub;
  Timer? _idleWatchdog;
  void Function(String)? _onData;
  void Function()? _onDisconnected;
  bool _stopped = false;
  final StringBuffer _data = StringBuffer(); // accumulated `data:` lines for the current event

  @override
  void start(
    String url, {
    required void Function(String data) onData,
    required void Function() onDisconnected,
  }) {
    _stopped = false;
    _data.clear();
    _onData = onData;
    _onDisconnected = onDisconnected;
    _connect(url);
  }

  Future<void> _connect(String url) async {
    if (_stopped) return;
    _client = HttpClient()..connectionTimeout = _connectTimeout;
    try {
      final req = await _client!.openUrl('GET', Uri.parse(url));
      req.headers.set('Accept', 'text/event-stream');
      req.headers.set('Accept-Encoding', 'identity');
      req.headers.set('Cache-Control', 'no-cache');
      final resp = await req.close();
      if (resp.statusCode != 200) {
        _disconnected();
        return;
      }
      final lines = resp.transform(utf8.decoder).transform(const LineSplitter());
      _sub = lines.listen(
        _handleLine,
        onDone: _disconnected,
        onError: (_) => _disconnected(),
        cancelOnError: true,
      );
      // Arm the watchdog once the stream is live; it resets on every line below.
      _armWatchdog();
    } catch (_) {
      _disconnected();
    }
  }

  void _disconnected() {
    _idleWatchdog?.cancel();
    _idleWatchdog = null;
    if (!_stopped && _onDisconnected != null) _onDisconnected!();
  }

  void _armWatchdog() {
    _idleWatchdog?.cancel();
    _idleWatchdog = Timer(_idleTimeout, _disconnected);
  }

  // SSE frame parsing (HTML spec): blank line dispatches the event; one leading
  // space after `data:` is stripped; multiple `data:` lines join with `\n`. Lines
  // starting with `:` are comments; `event:`/`id:`/`retry:` are ignored here. Any
  // received line (including comments) means the link is alive → reset the watchdog.
  void _handleLine(String line) {
    _armWatchdog(); // reset on every line; silence for _idleTimeout tears down the link
    if (line.isEmpty) {
      if (_data.isNotEmpty) {
        final d = _onData;
        if (d != null) d(_data.toString().trimRight());
        _data.clear();
      }
      return;
    }
    if (line.startsWith('data:')) {
      final v = line.substring(5);
      _data.writeln(v.startsWith(' ') ? v.substring(1) : v);
    }
  }

  @override
  void stop() {
    _stopped = true;
    _idleWatchdog?.cancel();
    _idleWatchdog = null;
    _sub?.cancel();
    _client?.close(force: true);
    _sub = null;
    _client = null;
    _data.clear();
  }
}

DxSpotSource createDxSpotSource() => _IoDxSpotSource();