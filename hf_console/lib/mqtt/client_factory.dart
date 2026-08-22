// client_factory.dart — platform-appropriate MQTT client creation.
//
// The web build cannot use MqttServerClient (raw TCP), and importing the
// browser client on mobile/test breaks compilation. Use a conditional import
// so the right implementation is selected at compile time.
import 'package:mqtt_client/mqtt_client.dart';

import 'client_factory_default.dart'
    if (dart.library.html) 'client_factory_web.dart';

MqttClient createMqttClient(String host, int port, String clientId) {
  return createMqttClientImpl(host, port, clientId);
}
