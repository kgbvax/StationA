// client_factory_web.dart — browser implementation.
//
// Browsers cannot open raw TCP sockets. The web build is served from shari and
// a small WebSocket bridge at /mqtt forwards bytes to the broker. The MQTT
// credentials (user/pass) from the setup screen still pass through to the
// broker; the host/port fields are ignored on web.
import 'package:mqtt_client/mqtt_client.dart';
import 'package:mqtt_client/mqtt_browser_client.dart';

/// The bridge serves this page itself, so the WebSocket endpoint is this
/// page's own origin — the build follows whatever host serves it instead of
/// hardcoding one station address.
final Uri _bridgeBase = Uri.base;

MqttClient createMqttClientImpl(String host, int port, String clientId) {
  final wsScheme = _bridgeBase.scheme == 'https' ? 'wss' : 'ws';
  final client = MqttBrowserClient(
      '$wsScheme://${_bridgeBase.host}:${_bridgeBase.port}/mqtt', clientId);
  client.websocketProtocols = MqttClientConstants.protocolsSingleDefault;
  return client;
}
