#include "mqtt_client.h"

#include <ArduinoJson.h>
#include <cstring>

#include "logging.h"

MqttClientManager* MqttClientManager::_instance = nullptr;

MqttClientManager::MqttClientManager()
    : _mqtt(_wifiClient),
      _modeCallback(nullptr),
      _movingCallback(nullptr),
      _lastWiFiAttemptMs(0),
      _lastMqttAttemptMs(0),
      _bridgeOnline(false),
      _deviceOnline(false),
      _direction(OperationalMode::Unknown),
      _moving(false) {}

void MqttClientManager::begin(ModeUpdateCallback modeCallback, MovingUpdateCallback movingCallback) {
  _modeCallback = modeCallback;
  _movingCallback = movingCallback;
  _instance = this;

  WiFi.mode(WIFI_STA);
  _mqtt.setServer(AppConfig::MQTT_HOST, AppConfig::MQTT_PORT);
  _mqtt.setBufferSize(AppConfig::MQTT_BUFFER_SIZE);
  _mqtt.setCallback(MqttClientManager::mqttCallbackThunk);
}

bool MqttClientManager::linkLive() {
  return _mqtt.connected() && _bridgeOnline && _deviceOnline;
}

void MqttClientManager::resetLinkState() {
  // Forget everything from the previous session; the retained /status and
  // /state replay after subscribe is the fresh feed.
  _bridgeOnline = false;
  _deviceOnline = false;
  _direction = OperationalMode::Unknown;
  _moving = false;
  notify();
}

void MqttClientManager::notify() {
  const bool live = linkLive();
  if (_modeCallback != nullptr) {
    _modeCallback(live ? _direction : OperationalMode::Unknown);
  }
  if (_movingCallback != nullptr) {
    _movingCallback(live && _moving);
  }
}

void MqttClientManager::tryConnectWiFi(const uint32_t nowMs) {
  if (WiFi.status() == WL_CONNECTED) {
    return;
  }

  if ((nowMs - _lastWiFiAttemptMs) < AppConfig::WIFI_RETRY_MS) {
    return;
  }

  _lastWiFiAttemptMs = nowMs;
  WiFi.begin(AppConfig::WIFI_SSID, AppConfig::WIFI_PASSWORD);
}

void MqttClientManager::tryConnectMqtt(const uint32_t nowMs) {
  if (WiFi.status() != WL_CONNECTED || _mqtt.connected()) {
    return;
  }

  if ((nowMs - _lastMqttAttemptMs) < AppConfig::MQTT_RETRY_MS) {
    return;
  }

  _lastMqttAttemptMs = nowMs;
  resetLinkState();

  char clientId[48];
  const uint32_t chip = static_cast<uint32_t>(ESP.getEfuseMac() & 0xFFFFFF);
  snprintf(clientId, sizeof(clientId), "%s%06lx", AppConfig::MQTT_CLIENT_ID_PREFIX, static_cast<unsigned long>(chip));

  const bool hasAuth = (AppConfig::MQTT_USERNAME != nullptr && AppConfig::MQTT_USERNAME[0] != '\0');
  const bool connected = hasAuth ? _mqtt.connect(clientId, AppConfig::MQTT_USERNAME, AppConfig::MQTT_PASSWORD)
                                 : _mqtt.connect(clientId);

  if (connected) {
    const bool subscribedStatus = _mqtt.subscribe(AppConfig::TOPIC_ANT_CTRL_STATUS);
    const bool subscribedState = _mqtt.subscribe(AppConfig::TOPIC_ANT_CTRL_STATE);
    logf(
        "%lu,MQTT,CONNECTED,host=%s,port=%u,auth=%s,client=%s,sub_status=%s,sub_state=%s\n",
        millis(),
        AppConfig::MQTT_HOST,
        AppConfig::MQTT_PORT,
        hasAuth ? "yes" : "no",
        clientId,
        subscribedStatus ? "yes" : "no",
        subscribedState ? "yes" : "no");
  } else {
    logf(
        "%lu,MQTT,CONNECT_FAILED,state=%d,host=%s,port=%u\n",
        millis(),
        _mqtt.state(),
        AppConfig::MQTT_HOST,
        AppConfig::MQTT_PORT);
  }
}

void MqttClientManager::loop(const uint32_t nowMs) {
  const bool wasConnected = _mqtt.connected();

  tryConnectWiFi(nowMs);
  tryConnectMqtt(nowMs);

  if (_mqtt.connected()) {
    _mqtt.loop();
  }

  if (wasConnected && !_mqtt.connected()) {
    logf("%lu,MQTT,DISCONNECTED,state=%d\n", millis(), _mqtt.state());
    resetLinkState();
  }
}

bool MqttClientManager::publishDirectionCommand(const OperationalMode mode) {
  const char* direction = modeToPayload(mode);

  if (!linkLive()) {
    logf(
        "%lu,MQTT,TX_SKIPPED,topic=%s,direction=%s,reason=%s\n",
        millis(),
        AppConfig::TOPIC_ANT_CTRL_CMD,
        direction,
        !_mqtt.connected() ? "not_connected" : (!_bridgeOnline ? "bridge_offline" : "device_offline"));
    return false;
  }

  char payload[64];
  snprintf(payload, sizeof(payload), "{\"action\":\"direction\",\"value\":\"%s\"}", direction);

  // NOT retained: one-shot intent. A retained /cmd would replay on the
  // bridge's next reconnect with nobody behind it (ultrabridge API §6).
  const bool ok = _mqtt.publish(AppConfig::TOPIC_ANT_CTRL_CMD, payload, false);
  logf(
      "%lu,MQTT,TX,topic=%s,payload=%s,result=%s\n",
      millis(),
      AppConfig::TOPIC_ANT_CTRL_CMD,
      payload,
      ok ? "ok" : "failed");
  return ok;
}

bool MqttClientManager::isConnected() {
  return _mqtt.connected();
}

void MqttClientManager::onMessage(char* topic, uint8_t* payload, unsigned int length) {
  if (topic == nullptr) {
    return;
  }

  if (strcmp(topic, AppConfig::TOPIC_ANT_CTRL_STATUS) == 0) {
    char status[16];
    const size_t copyLen = (length < sizeof(status) - 1) ? length : (sizeof(status) - 1);
    memcpy(status, payload, copyLen);
    status[copyLen] = '\0';
    _bridgeOnline = (strcmp(status, "online") == 0);
    logf("%lu,MQTT,RX_STATUS,status=%s\n", millis(), status);
    notify();
    return;
  }

  if (strcmp(topic, AppConfig::TOPIC_ANT_CTRL_STATE) != 0) {
    return;
  }

  if (length == 0) {
    // Retained /state cleared: no data.
    _deviceOnline = false;
    _direction = OperationalMode::Unknown;
    _moving = false;
    notify();
    return;
  }

  StaticJsonDocument<512> doc;
  const DeserializationError err = deserializeJson(doc, payload, length);
  if (err) {
    logf("%lu,MQTT,RX_PARSE_FAILED,topic=%s,error=%s\n", millis(), topic, err.c_str());
    return;
  }

  _deviceOnline = doc["device_online"] | false;
  _direction = modeFromPayload(doc["direction"] | static_cast<const char*>(nullptr));
  _moving = doc["moving"] | false;
  logf(
      "%lu,MQTT,RX_STATE,device_online=%s,direction=%s,moving=%s\n",
      millis(),
      _deviceOnline ? "true" : "false",
      modeToPayload(_direction),
      _moving ? "true" : "false");
  notify();
}

void MqttClientManager::mqttCallbackThunk(char* topic, uint8_t* payload, unsigned int length) {
  if (_instance != nullptr) {
    _instance->onMessage(topic, payload, length);
  }
}
