// bridge_endpoint.dart — the web build's MQTT-over-WebSocket endpoint.
//
// The Go bridge (webbridge/) serves the page itself, so the WebSocket endpoint
// is the page's own origin + /mqtt — the build follows whatever host serves it.
// Pure (no dart:html) so it is unit-testable on the VM.

/// Returns the bridge URL and the port to put on the client. The port must be
/// set as `client.port` too: MqttBrowserClient rebuilds the URL with
/// `client.port` (default 1883), so the URL's own port alone is ignored and
/// the browser would dial the broker's TCP port instead of the bridge.
({String url, int port}) bridgeEndpoint(Uri base) {
  final scheme = base.scheme == 'https' ? 'wss' : 'ws';
  return (url: '$scheme://${base.host}:${base.port}/mqtt', port: base.port);
}
