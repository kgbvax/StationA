// client_factory_web.dart — browser implementation.
//
// Browsers cannot open raw TCP sockets. The web build is served from scmino and
// a small WebSocket bridge at /mqtt forwards bytes to the broker. The MQTT
// credentials (user/pass) from the setup screen still pass through to the
// broker; the host/port fields are ignored on web.
import 'package:mqtt_client/mqtt_client.dart';
import 'package:mqtt_client/mqtt_browser_client.dart';

import 'bridge_endpoint.dart';

MqttClient createMqttClientImpl(String host, int port, String clientId) {
  final ep = bridgeEndpoint(Uri.base);
  final client = MqttBrowserClient(ep.url, clientId);
  // Load-bearing: MqttBrowserClient replaces the URL's port with client.port
  // (default 1883) — without this the browser dials ws://<host>:1883/mqtt.
  client.port = ep.port;
  client.websocketProtocols = MqttClientConstants.protocolsSingleDefault;
  return client;
}
