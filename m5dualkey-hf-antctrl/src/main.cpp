#include <Arduino.h>
#include <ArduinoOTA.h>
#include <WiFi.h>

#include "button_manager.h"
#include "chain_key.h"
#include "config.h"
#include "led_status.h"
#include "logging.h"
#include "mqtt_client.h"

ButtonManager g_buttons(
    AppConfig::KEY1_PIN,
    AppConfig::KEY2_PIN,
    AppConfig::DEBOUNCE_MS,
    AppConfig::LONG_PRESS_MS,
    AppConfig::COMBO_WINDOW_MS);

LedStatus g_leds(AppConfig::LED_PWR_PIN, AppConfig::LED_SIG_PIN, AppConfig::LED_COUNT);
MqttClientManager g_mqtt;
ChainKeyManager g_chainKey;

bool g_buttonAPressed = false;
bool g_buttonBPressed = false;
bool g_dualHoldActive = false;
bool g_dualHoldRebootIssued = false;
uint32_t g_dualHoldStartMs = 0;

constexpr uint32_t REBOOT_BOTH_HOLD_MS = 5000;

bool g_otaStarted = false;

// Wireless OTA, so only the first flash (or recovery) needs USB. Started once,
// on the first WiFi association: mDNS needs the interface up. The ESP32 core
// keeps the listener across later WiFi reconnects.
void startOtaOnce() {
  if (g_otaStarted || WiFi.status() != WL_CONNECTED) {
    return;
  }
  g_otaStarted = true;

  ArduinoOTA.setHostname(AppConfig::OTA_HOSTNAME);
  ArduinoOTA.setPassword(SECRET_OTA_PASSWORD);
  ArduinoOTA.onStart([]() {
    // No antenna motion to stop: direction commands are one-shot and the
    // controller finishes them on its own.
    logf("%lu,OTA,START\n", millis());
    g_leds.setMode(OperationalMode::Unknown);
  });
  ArduinoOTA.onEnd([]() { logf("%lu,OTA,END,rebooting\n", millis()); });
  ArduinoOTA.onError([](ota_error_t error) { logf("%lu,OTA,ERROR,code=%u\n", millis(), static_cast<unsigned>(error)); });
  ArduinoOTA.begin();
  logf("%lu,OTA,LISTENING,host=%s.local,ip=%s\n", millis(), AppConfig::OTA_HOSTNAME, WiFi.localIP().toString().c_str());
}

const char* toButtonLabel(const ButtonId id) {
  switch (id) {
    case ButtonId::A:
      return "BTN_A";
    case ButtonId::B:
      return "BTN_B";
    case ButtonId::Both:
      return "BTN_BOTH";
    case ButtonId::None:
    default:
      return "BTN_NONE";
  }
}

const char* toEventLabel(const ButtonEventType type) {
  switch (type) {
    case ButtonEventType::Press:
      return "PRESS";
    case ButtonEventType::Release:
      return "RELEASE";
    case ButtonEventType::ShortPress:
      return "SHORT_PRESS";
    case ButtonEventType::LongPress:
      return "LONG_PRESS";
    case ButtonEventType::ComboPress:
      return "COMBO_PRESS";
    case ButtonEventType::None:
    default:
      return "NONE";
  }
}

void handleModeUpdate(const OperationalMode mode) {
  g_leds.setMode(mode);
  logf("%lu,MODE,%s\n", millis(), modeToPayload(mode));
}

void handleMovingUpdate(const bool moving) {
  g_leds.setMoving(moving);
  logf("%lu,MOTORS,moving=%s\n", millis(), moving ? "true" : "false");
}

// Chain Key LED mirrors the radio's DVK: red = our memory is on the air,
// amber = another memory is playing, dim green = ready, off = radio link down.
void handleDvkUpdate(const DvkState& state) {
  if (!state.live) {
    g_chainKey.setLed(0, 0, 0);
  } else if (state.playing && state.id == AppConfig::DVK_MEMORY) {
    g_chainKey.setLed(255, 0, 0);
  } else if (state.playing) {
    g_chainKey.setLed(255, 100, 0);
  } else {
    g_chainKey.setLed(0, 40, 0);
  }
  logf("%lu,DVK,live=%s,playing=%s,id=%u\n", millis(), state.live ? "true" : "false",
       state.playing ? "true" : "false", state.id);
}

void setup() {
  Serial.begin(115200);

  g_buttons.begin();
  g_leds.begin();
  g_leds.startModeCycle(1);
  g_mqtt.setDvkCallback(handleDvkUpdate);
  g_mqtt.begin(handleModeUpdate, handleMovingUpdate);
  g_chainKey.begin();

  logln("m5dualkey-hf-antctrl startup");
}

void loop() {
  const uint32_t now = millis();

  g_buttons.update(now);

  ButtonEvent event{};
  while (g_buttons.nextEvent(event)) {
    logf("%lu,%s,%s\n", event.timestamp, toButtonLabel(event.button), toEventLabel(event.type));

    if (event.button == ButtonId::A) {
      if (event.type == ButtonEventType::Press) {
        g_buttonAPressed = true;
      } else if (event.type == ButtonEventType::Release) {
        g_buttonAPressed = false;
      }
    } else if (event.button == ButtonId::B) {
      if (event.type == ButtonEventType::Press) {
        g_buttonBPressed = true;
      } else if (event.type == ButtonEventType::Release) {
        g_buttonBPressed = false;
      }
    }

    if (event.type == ButtonEventType::ShortPress) {
      if (event.button == ButtonId::A) {
        g_mqtt.publishDirectionCommand(OperationalMode::Forward);
      } else if (event.button == ButtonId::B) {
        g_mqtt.publishDirectionCommand(OperationalMode::Reverse);
      }
    } else if (event.type == ButtonEventType::ComboPress) {
      g_mqtt.publishDirectionCommand(OperationalMode::Bidirectional);
    }
  }

  const bool bothPressed = g_buttonAPressed && g_buttonBPressed;
  g_leds.setButtonPreview(g_buttonAPressed, g_buttonBPressed);

  if (bothPressed && !g_dualHoldActive) {
    g_dualHoldActive = true;
    g_dualHoldRebootIssued = false;
    g_dualHoldStartMs = now;
  } else if (!bothPressed && g_dualHoldActive) {
    g_dualHoldActive = false;
    g_dualHoldRebootIssued = false;
  }

  if (g_dualHoldActive && !g_dualHoldRebootIssued && (now - g_dualHoldStartMs) >= REBOOT_BOTH_HOLD_MS) {
    g_dualHoldRebootIssued = true;
    logf("%lu,SYSTEM,REBOOT,reason=both_buttons_held_5s\n", now);
    delay(50);
    ESP.restart();
  }

  g_chainKey.update(now);
  while (g_chainKey.takePress()) {
    logf("%lu,CHAIN_KEY,PRESS\n", now);
    g_mqtt.publishDvkToggle();
  }
  while (g_chainKey.takeLongPress()) {
    logf("%lu,CHAIN_KEY,LONG_PRESS\n", now);
    g_mqtt.publishDvkStop();
  }

  g_leds.update(now);

  g_mqtt.loop(now);

  startOtaOnce();
  if (g_otaStarted) {
    ArduinoOTA.handle();
  }
}