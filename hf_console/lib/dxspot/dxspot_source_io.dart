// dxspot_source_io.dart — native (Android / desktop / test) SSE client via dart:io.
//
// dart:io has no EventSource, so we stream the response body and parse SSE frames
// ourselves. No internal reconnect: on EOF/error we call onDisconnected and let the
// service own backoff. The stream is gzip-encoded (HttpClient's default
// Accept-Encoding + autoUncompress): horstreporter flushes its gzip writer per
// SSE batch (gzip.go), so it decodes incrementally at ~1/5 of the wire bytes.
//
// Liveness: horstreporter writes a `: hb` comment after 25 s without spots
// (stream.go), so silence means a dead link, not a quiet band. A half-open TCP
// socket (NAT/firewall dropped the connection without FIN/RST) would otherwise
// hang forever. We guard two ways:
//   1. HttpClient.connectionTimeout bounds the connect phase (server accepts TCP but
//      never answers the HTTP request).
//   2. An idle watchdog resets on EVERY received line (comments included); after
//      [_idleTimeout] (three missed heartbeats) we force-close and call
//      onDisconnected so backoff takes over. The reconnect replays the
//      `minutes` window, so the overlay refills at once.

import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'dxspot_source.dart';
import 'sse_parser.dart';

const _connectTimeout = Duration(seconds: 15);
const _idleTimeout = Duration(seconds: 90);

class _IoDxSpotSource extends DxSpotSource {
  HttpClient? _client;
  StreamSubscription<String>? _sub;
  Timer? _idleWatchdog;
  void Function(String)? _onData;
  void Function()? _onDisconnected;
  bool _stopped = false;
  late final SseParser _parser = SseParser((d) => _onData?.call(d));

  @override
  void start(
    String url, {
    required void Function(String data) onData,
    required void Function() onDisconnected,
  }) {
    _stopped = false;
    _parser.reset();
    _onData = onData;
    _onDisconnected = onDisconnected;
    _connect(url);
  }

  Future<void> _connect(String url) async {
    if (_stopped) return;
    final client = HttpClient()..connectionTimeout = _connectTimeout;
    _client = client;
    try {
      final req = await client.openUrl('GET', Uri.parse(url));
      req.headers.set('Accept', 'text/event-stream');
      req.headers.set('Cache-Control', 'no-cache');
      final resp = await req.close();
      // stop() may have run while we awaited: never subscribe or arm after it.
      if (_stopped || !identical(_client, client)) {
        client.close(force: true);
        return;
      }
      if (resp.statusCode != 200) {
        resp.drain<void>().catchError((_) {});
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

  // Any received line (including `: hb` comments) means the link is alive →
  // reset the watchdog; framing lives in [SseParser].
  void _handleLine(String line) {
    _armWatchdog(); // silence for _idleTimeout tears down the link
    _parser.addLine(line);
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
    _parser.reset();
  }
}

DxSpotSource createDxSpotSource() => _IoDxSpotSource();