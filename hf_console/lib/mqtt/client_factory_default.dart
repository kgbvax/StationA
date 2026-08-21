// client_factory_default.dart — mobile / desktop / test implementation.
import 'package:mqtt_client/mqtt_client.dart';
import 'package:mqtt_client/mqtt_server_client.dart';

MqttClient createMqttClientImpl(String host, int port, String clientId) {
  final client = MqttServerClient(host, clientId);
  client.port = port;
  return client;
}
