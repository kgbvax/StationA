#pragma once

#include <Arduino.h>
#include <PubSubClient.h>
#include <WiFi.h>

#include "config.h"

// mode is Unknown and moving is false whenever the ant-ctrl link is not live
// (bridge /status offline, or /state.device_online false, or no data yet).
using ModeUpdateCallback = void (*)(OperationalMode mode);
using MovingUpdateCallback = void (*)(bool moving);

// DVK view of muehle/hf/radio. live = bridge /status online AND
// /state.device_online; playing/id only meaningful while live.
struct DvkState {
  bool live;
  bool playing;
  uint8_t id;
};
using DvkUpdateCallback = void (*)(const DvkState& state);

class MqttClientManager {
 public:
  MqttClientManager();

  void begin(ModeUpdateCallback modeCallback, MovingUpdateCallback movingCallback);
  void loop(uint32_t nowMs);
  bool publishDirectionCommand(OperationalMode mode);
  // Chain Key: play DVK memory AppConfig::DVK_MEMORY, or stop whatever DVK
  // memory is playing. Never sent while the radio link is not live.
  bool publishDvkToggle();
  bool publishDvkStop();
  void setDvkCallback(DvkUpdateCallback callback) { _dvkCallback = callback; }
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

  DvkUpdateCallback _dvkCallback;
  bool _radioBridgeOnline;
  bool _radioDeviceOnline;
  char _dvkStatus[16];
  uint8_t _dvkId;

  static MqttClientManager* _instance;

  bool linkLive();
  void resetLinkState();
  void notify();
  bool radioLive();
  void notifyDvk();
  void onRadioState(uint8_t* payload, unsigned int length);
  bool publishRadioCmd(const char* payload);
  void tryConnectWiFi(uint32_t nowMs);
  void tryConnectMqtt(uint32_t nowMs);
  void onMessage(char* topic, uint8_t* payload, unsigned int length);

  static void mqttCallbackThunk(char* topic, uint8_t* payload, unsigned int length);
};
