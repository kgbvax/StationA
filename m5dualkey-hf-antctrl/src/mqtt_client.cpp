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
      _moving(false),
      _dvkCallback(nullptr),
      _radioBridgeOnline(false),
      _radioDeviceOnline(false),
      _dvkStatus{0},
      _dvkId(0) {}

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

  _radioBridgeOnline = false;
  _radioDeviceOnline = false;
  _dvkStatus[0] = '\0';
  _dvkId = 0;
  notifyDvk();
}

bool MqttClientManager::radioLive() {
  return _mqtt.connected() && _radioBridgeOnline && _radioDeviceOnline;
}

void MqttClientManager::notifyDvk() {
  if (_dvkCallback == nullptr) {
    return;
  }
  const bool live = radioLive();
  const DvkState state{live, live && strcmp(_dvkStatus, "playback") == 0, live ? _dvkId : static_cast<uint8_t>(0)};
  _dvkCallback(state);
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
    const bool subscribedRadio = _mqtt.subscribe(AppConfig::TOPIC_RADIO_STATUS) &&
                                 _mqtt.subscribe(AppConfig::TOPIC_RADIO_STATE);
    logf(
        "%lu,MQTT,CONNECTED,host=%s,port=%u,auth=%s,client=%s,sub_status=%s,sub_state=%s,sub_radio=%s\n",
        millis(),
        AppConfig::MQTT_HOST,
        AppConfig::MQTT_PORT,
        hasAuth ? "yes" : "no",
        clientId,
        subscribedStatus ? "yes" : "no",
        subscribedState ? "yes" : "no",
        subscribedRadio ? "yes" : "no");
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

bool MqttClientManager::publishRadioCmd(const char* payload) {
  if (!radioLive()) {
    logf(
        "%lu,MQTT,TX_SKIPPED,topic=%s,payload=%s,reason=%s\n",
        millis(),
        AppConfig::TOPIC_RADIO_CMD,
        payload,
        !_mqtt.connected() ? "not_connected" : (!_radioBridgeOnline ? "bridge_offline" : "device_offline"));
    return false;
  }

  // NOT retained: a DVK play keys the transmitter; it must never replay.
  const bool ok = _mqtt.publish(AppConfig::TOPIC_RADIO_CMD, payload, false);
  logf("%lu,MQTT,TX,topic=%s,payload=%s,result=%s\n", millis(), AppConfig::TOPIC_RADIO_CMD, payload, ok ? "ok" : "failed");
  return ok;
}

bool MqttClientManager::publishDvkStop() {
  // No value: the bridge stops whichever memory /state.dvk_id names.
  return publishRadioCmd("{\"action\":\"dvk_stop\"}");
}

bool MqttClientManager::publishDvkToggle() {
  if (strcmp(_dvkStatus, "playback") == 0) {
    return publishDvkStop();
  }
  if (strcmp(_dvkStatus, "recording") == 0 || strcmp(_dvkStatus, "preview") == 0 ||
      strcmp(_dvkStatus, "disabled") == 0) {
    logf("%lu,MQTT,TX_SKIPPED,topic=%s,reason=dvk_%s\n", millis(), AppConfig::TOPIC_RADIO_CMD, _dvkStatus);
    return false;
  }

  char payload[48];
  snprintf(payload, sizeof(payload), "{\"action\":\"dvk_play_%u\"}", AppConfig::DVK_MEMORY);
  return publishRadioCmd(payload);
}

void MqttClientManager::onRadioState(uint8_t* payload, const unsigned int length) {
  if (length == 0) {
    _radioDeviceOnline = false;
    _dvkStatus[0] = '\0';
    _dvkId = 0;
    notifyDvk();
    return;
  }

  // The radio /state is large (DVK memory list, meters ...): keep only the
  // fields the DVK key needs.
  StaticJsonDocument<64> filter;
  filter["device_online"] = true;
  filter["dvk_status"] = true;
  filter["dvk_id"] = true;

  StaticJsonDocument<256> doc;
  const DeserializationError err =
      deserializeJson(doc, payload, length, DeserializationOption::Filter(filter));
  if (err) {
    logf("%lu,MQTT,RX_PARSE_FAILED,topic=%s,error=%s,len=%u\n", millis(), AppConfig::TOPIC_RADIO_STATE, err.c_str(), length);
    return;
  }

  const bool deviceOnline = doc["device_online"] | false;
  const char* status = doc["dvk_status"] | "";
  const uint8_t id = doc["dvk_id"] | 0;
  const bool changed = deviceOnline != _radioDeviceOnline || strcmp(status, _dvkStatus) != 0 || id != _dvkId;

  _radioDeviceOnline = deviceOnline;
  strlcpy(_dvkStatus, status, sizeof(_dvkStatus));
  _dvkId = id;

  // The radio /state republishes on every meter change; log only DVK changes.
  if (changed) {
    logf("%lu,MQTT,RX_RADIO,device_online=%s,dvk_status=%s,dvk_id=%u,len=%u\n", millis(),
         deviceOnline ? "true" : "false", _dvkStatus[0] != '\0' ? _dvkStatus : "(none)", _dvkId, length);
    notifyDvk();
  }
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

  if (strcmp(topic, AppConfig::TOPIC_RADIO_STATUS) == 0) {
    char status[16];
    const size_t copyLen = (length < sizeof(status) - 1) ? length : (sizeof(status) - 1);
    memcpy(status, payload, copyLen);
    status[copyLen] = '\0';
    _radioBridgeOnline = (strcmp(status, "online") == 0);
    logf("%lu,MQTT,RX_RADIO_STATUS,status=%s\n", millis(), status);
    notifyDvk();
    return;
  }

  if (strcmp(topic, AppConfig::TOPIC_RADIO_STATE) == 0) {
    onRadioState(payload, length);
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
