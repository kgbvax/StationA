// client_factory_web.dart — browser implementation.
//
// Browsers cannot open raw TCP sockets. The web build is served from shari and
// a small WebSocket bridge at /mqtt forwards bytes to the broker. The MQTT
// credentials (user/pass) from the setup screen still pass through to the
// broker; the host/port fields are ignored on web.
import 'package:mqtt_client/mqtt_client.dart';
import 'package:mqtt_client/mqtt_browser_client.dart';

const _wsUri = 'ws://192.168.1.139:8091/mqtt';

MqttClient createMqttClientImpl(String host, int port, String clientId) {
  final client = MqttBrowserClient(_wsUri, clientId);
  client.websocketProtocols = MqttClientConstants.protocolsSingleDefault;
  return client;
}
