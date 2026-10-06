#pragma once

#include <Arduino.h>

#if __has_include("secrets.h")
#include "secrets.h"
#else
#error "include/secrets.h missing - copy include/secrets.example.h.x to include/secrets.h and fill in"
#endif

namespace AppConfig {

// Chain DualKey hardware mapping
static constexpr uint8_t KEY1_PIN = 0;
static constexpr uint8_t KEY2_PIN = 17;
static constexpr uint8_t LED_PWR_PIN = 40;
static constexpr uint8_t LED_SIG_PIN = 21;
static constexpr uint8_t LED_COUNT = 2;

// Input timings
static constexpr uint32_t DEBOUNCE_MS = 30;
static constexpr uint32_t LONG_PRESS_MS = 600;
static constexpr uint32_t COMBO_WINDOW_MS = 100;

// Chain Key on a HY2.0-4P Chain-bus port (UART). Both ports are probed.
static constexpr int8_t CHAIN_PORT1_RX_PIN = 48;
static constexpr int8_t CHAIN_PORT1_TX_PIN = 47;
static constexpr int8_t CHAIN_PORT2_RX_PIN = 5;
static constexpr int8_t CHAIN_PORT2_TX_PIN = 6;
static constexpr uint32_t CHAIN_PROBE_MS = 5000;
static constexpr uint32_t CHAIN_HEARTBEAT_MS = 10000;
static constexpr uint8_t CHAIN_KEY_LED_BRIGHTNESS = 60;

// DVK memory the Chain Key plays (flexbridge dvk_play_<N>).
static constexpr uint8_t DVK_MEMORY = 2;

// Connectivity timing
static constexpr uint32_t WIFI_RETRY_MS = 5000;
static constexpr uint32_t MQTT_RETRY_MS = 5000;

// Diagnostics
static constexpr bool ENABLE_LOGS = true;

// Wi-Fi / MQTT credentials live in gitignored include/secrets.h
// (copy include/secrets.example.h.x -> include/secrets.h and fill in).
static constexpr const char* WIFI_SSID = SECRET_WIFI_SSID;
static constexpr const char* WIFI_PASSWORD = SECRET_WIFI_PASSWORD;
static constexpr const char* MQTT_HOST = SECRET_MQTT_HOST;
static constexpr uint16_t MQTT_PORT = SECRET_MQTT_PORT;
static constexpr const char* MQTT_USERNAME = SECRET_MQTT_USERNAME;
static constexpr const char* MQTT_PASSWORD = SECRET_MQTT_PASSWORD;

// Wireless OTA (ArduinoOTA, password from secrets.h). Hostname -> <name>.local.
static constexpr const char* OTA_HOSTNAME = "m5dualkey-antctrl-1";

// MQTT client. The client ID is a non-slot exception: this device owns no
// slot, so it must not collide with a slot-derived bridge ID.
static constexpr const char* MQTT_CLIENT_ID_PREFIX = "m5dualkey-hf-antctrl-";
// PubSubClient drops oversize messages silently. hf/radio/state carries the
// DVK memory list and needs several KB; set it BEFORE connect().
static constexpr uint16_t MQTT_BUFFER_SIZE = 8192;

// Slot muehle/hf/ant-ctrl (ultrabridge, Ultrabeam RCU-06). See
// ../ultrabridge/ultrabeam-mqtt-api.md.
static constexpr const char* TOPIC_ANT_CTRL_STATE = "muehle/hf/ant-ctrl/state";
static constexpr const char* TOPIC_ANT_CTRL_STATUS = "muehle/hf/ant-ctrl/status";
static constexpr const char* TOPIC_ANT_CTRL_CMD = "muehle/hf/ant-ctrl/cmd";

// Slot muehle/hf/radio (flexbridge, FLEX-8400) — DVK playback. See
// ../flexbridge/docs/radio2mqtt-schema.md.
static constexpr const char* TOPIC_RADIO_STATE = "muehle/hf/radio/state";
static constexpr const char* TOPIC_RADIO_STATUS = "muehle/hf/radio/status";
static constexpr const char* TOPIC_RADIO_CMD = "muehle/hf/radio/cmd";

}  // namespace AppConfig

enum class OperationalMode : uint8_t {
  Unknown = 0,
  Forward,
  Reverse,
  Bidirectional,
};

inline const char* modeToPayload(const OperationalMode mode) {
  switch (mode) {
    case OperationalMode::Forward:
      return "forward";
    case OperationalMode::Reverse:
      return "reverse";
    case OperationalMode::Bidirectional:
      return "bidirectional";
    case OperationalMode::Unknown:
    default:
      return "unknown";
  }
}

inline OperationalMode modeFromPayload(const char* payload) {
  if (payload == nullptr) {
    return OperationalMode::Unknown;
  }
  if (strcmp(payload, "forward") == 0 || strcmp(payload, "normal") == 0) {
    return OperationalMode::Forward;
  }
  if (strcmp(payload, "reverse") == 0 || strcmp(payload, "180") == 0) {
    return OperationalMode::Reverse;
  }
  if (strcmp(payload, "bidirectional") == 0 || strcmp(payload, "bidir") == 0) {
    return OperationalMode::Bidirectional;
  }
  return OperationalMode::Unknown;
}
