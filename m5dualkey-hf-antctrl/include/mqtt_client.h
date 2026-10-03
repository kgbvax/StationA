#pragma once

#include <Arduino.h>
#include <PubSubClient.h>
#include <WiFi.h>

#include "config.h"

// mode is Unknown and moving is false whenever the ant-ctrl link is not live
// (bridge /status offline, or /state.device_online false, or no data yet).
using ModeUpdateCallback = void (*)(OperationalMode mode);
using MovingUpdateCallback = void (*)(bool moving);

class MqttClientManager {
 public:
  MqttClientManager();

  void begin(ModeUpdateCallback modeCallback, MovingUpdateCallback movingCallback);
  void loop(uint32_t nowMs);
  bool publishDirectionCommand(OperationalMode mode);
  bool isConnected();

 private:
  WiFiClient _wifiClient;
  PubSubClient _mqtt;
  ModeUpdateCallback _modeCallback;
  MovingUpdateCallback _movingCallback;
  uint32_t _lastWiFiAttemptMs;
  uint32_t _lastMqttAttemptMs;

  // Two-layer liveness: /status is the bridge LWT, /state.device_online is the
  // controller link. Both must be up before the LEDs show a direction or a key
  // press is sent.
  bool _bridgeOnline;
  bool _deviceOnline;
  OperationalMode _direction;
  bool _moving;

  static MqttClientManager* _instance;

  bool linkLive();
  void resetLinkState();
  void notify();
  void tryConnectWiFi(uint32_t nowMs);
  void tryConnectMqtt(uint32_t nowMs);
  void onMessage(char* topic, uint8_t* payload, unsigned int length);

  static void mqttCallbackThunk(char* topic, uint8_t* payload, unsigned int length);
};
