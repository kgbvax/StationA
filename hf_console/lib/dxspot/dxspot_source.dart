// dxspot_source.dart — platform-neutral SSE source interface.
//
// The hf_console speaks Server-Sent Events directly to horstreporter's /api/stream
// (no shari dependency). Browsers and native need different transports, so — mirroring
// lib/mqtt/client_factory*.dart — a concrete implementation is selected at compile
// time via a conditional import in the consumer (dxspot_service.dart):
//
//   import 'dxspot_source_io.dart' if (dart.library.html) 'dxspot_source_web.dart';
//
// Each implementation file defines `DxSpotSource createDxSpotSource()`. The source does
// NOT reconnect on its own: it calls [onDisconnected] and lets the service own backoff,
// so feed health can be surfaced uniformly on both platforms.

abstract class DxSpotSource {
  /// Opens the SSE stream at [url]. [onData] receives each event's `data:` payload
  /// (one or more `data:` lines joined with `\n`); [onDisconnected] is called once
  /// when the stream ends or errors, so the service can reconnect with backoff.
  void start(
    String url, {
    required void Function(String data) onData,
    required void Function() onDisconnected,
  });

  /// Closes the stream and releases resources. Safe to call when already stopped.
  void stop();
}