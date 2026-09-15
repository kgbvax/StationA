import 'package:mqtt_client/mqtt_client.dart';
import 'package:hf_console/mqtt/mqtt_service.dart';
import 'package:hf_console/store/bus_store.dart';

/// A non-network [MqttService] that records every [publish] call for assertions.
class FakeMqttService extends MqttService {
  final List<PublishRecord> publishes = [];

  FakeMqttService([BusStore? store]) : super(store ?? BusStore());

  @override
  Future<void> connect({
    required String host,
    required int port,
    required String username,
    required String password,
    required String clientId,
  }) async {}

  @override
  bool publish(String topic, String payload,
      {required bool retain, MqttQos qos = MqttQos.atLeastOnce}) {
    publishes.add(PublishRecord(topic, payload, retain));
    return true;
  }

  @override
  void dispose() {}

  void resetPublishes() => publishes.clear();
}

class PublishRecord {
  final String topic;
  final String payload;
  final bool retain;

  PublishRecord(this.topic, this.payload, this.retain);

  @override
  String toString() => 'PublishRecord($topic, $payload, retain=$retain)';
}
