// dxspot_source_web.dart — browser SSE client via dart:html EventSource.
//
// EventSource gives native SSE parsing + auto-reconnect, but for uniform behaviour
// with the native impl (and so the service can surface feed health), we treat an
// error as a disconnect and let the service close + reopen with backoff. Each new
// connection re-fetches the `minutes=15` history window, so losing EventSource's
// Last-Event-ID resume is harmless.
//
// CORS: a cross-origin EventSource requires horstreporter to send
// `Access-Control-Allow-Origin` for the page origin (http://shari:8091). The native
// (Android) build is unaffected — no CORS outside the browser.
//
// dart:html is the zero-dependency browser SSE API (package:web would add a dep and
// needs js_interop boilerplate for EventSource); these lints are expected here.
// ignore_for_file: avoid_web_libraries_in_flutter, deprecated_member_use

import 'dart:html';

import 'dxspot_source.dart';

class _WebDxSpotSource extends DxSpotSource {
  EventSource? _es;
  bool _stopped = false;
  void Function(String)? _onData;
  void Function()? _onDisconnected;

  @override
  void start(
    String url, {
    required void Function(String data) onData,
    required void Function() onDisconnected,
  }) {
    _stopped = false;
    _onData = onData;
    _onDisconnected = onDisconnected;
    _es = EventSource(url);
    _es!.onMessage.listen((MessageEvent e) {
      if (!_stopped && _onData != null) {
        final d = e.data;
        if (d is String && d.isNotEmpty) _onData!(d);
      }
    });
    _es!.onError.listen((_) {
      _close();
      if (!_stopped && _onDisconnected != null) _onDisconnected!();
    });
  }

  void _close() {
    _es?.close();
    _es = null;
  }

  @override
  void stop() {
    _stopped = true;
    _close();
    _onData = null;
    _onDisconnected = null;
  }
}

DxSpotSource createDxSpotSource() => _WebDxSpotSource();