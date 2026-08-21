import 'dart:async';
import 'dart:convert';
import 'package:flutter/foundation.dart';
import 'package:mqtt_client/mqtt_client.dart';
import 'package:typed_data/typed_data.dart';
import '../store/bus_store.dart';
import 'client_factory.dart';

class MqttService {
  final BusStore store;
  final ValueNotifier<bool> connected = ValueNotifier(false);
  MqttClient? _client;
  StreamSubscription? _updates;

  MqttService(this.store);

  Future<void> connect({
    required String host,
    required int port,
    required String username,
    required String password,
    required String clientId,
  }) async {
    final client = createMqttClient(host, port, clientId);
    client.keepAlivePeriod = 20;
    client.autoReconnect = true;
    client.resubscribeOnAutoReconnect = true;
    client.onConnected = () {
      connected.value = true;
      client.subscribe('muehle/#', MqttQos.atMostOnce);
    };
    client.onDisconnected = () {
      connected.value = false;
    };
    client.onAutoReconnect = () {
      connected.value = false;
    };
    client.onAutoReconnected = () {
      connected.value = true;
    };

    final conn = MqttConnectMessage()
        .withClientIdentifier(clientId)
        .authenticateAs(username, password)
        .startClean();
    client.connectionMessage = conn;

    await client.connect();
    _client = client;

    _updates = client.updates!.listen((List<MqttReceivedMessage> messages) {
      for (final msg in messages) {
        final rec = msg.payload as MqttPublishMessage;
        final topic = msg.topic;
        final bytes = rec.payload.message;
        final retained = rec.header!.retain;
        _apply(topic, bytes, retained);
      }
    });
  }

  void _apply(String topic, Uint8Buffer bytes, bool retained) {
    final text = utf8.decode(bytes.toList(), allowMalformed: true);
    if (text.isEmpty) {
      store.apply(topic, null, retained);
      return;
    }
    dynamic payload;
    try {
      payload = jsonDecode(text);
    } catch (_) {
      payload = text;
    }
    store.apply(topic, payload, retained);
  }

  void publish(String topic, String payload, {required bool retain, MqttQos qos = MqttQos.atLeastOnce}) {
    final client = _client;
    if (client == null || client.connectionStatus!.state != MqttConnectionState.connected) return;
    final builder = MqttClientPayloadBuilder();
    builder.addString(payload);
    client.publishMessage(topic, qos, builder.payload!, retain: retain);
  }

  void clear(String topic) {
    publish(topic, '', retain: true, qos: MqttQos.atLeastOnce);
  }

  void dispose() {
    _updates?.cancel();
    _client?.disconnect();
    connected.dispose();
  }
}
