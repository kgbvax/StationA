#include "chain_key.h"

#include "config.h"
#include "logging.h"

ChainKeyManager::ChainKeyManager()
    : _bus(nullptr),
      _keyId(0),
      _busPort(0),
      _lastProbeMs(0),
      _lastHeartbeatMs(0),
      _pressCount(0),
      _longPressCount(0),
      _led{0, 0, 0},
      _ledValid(false) {}

void ChainKeyManager::begin() {
  _port1.begin(&Serial1, 115200, AppConfig::CHAIN_PORT1_RX_PIN, AppConfig::CHAIN_PORT1_TX_PIN);
  _port2.begin(&Serial2, 115200, AppConfig::CHAIN_PORT2_RX_PIN, AppConfig::CHAIN_PORT2_TX_PIN);
  _lastProbeMs = millis() - AppConfig::CHAIN_PROBE_MS;  // probe on the first update()
}

bool ChainKeyManager::probe(Chain& bus, const uint8_t portNumber) {
  if (!bus.isDeviceConnected(2, 20)) {
    return false;
  }

  uint16_t count = 0;
  if (bus.getDeviceNum(&count) != CHAIN_OK || count == 0) {
    return false;
  }

  for (uint16_t id = 1; id <= count; ++id) {
    chain_device_type_t type;
    if (bus.getDeviceType(id, &type) != CHAIN_OK || type != CHAIN_KEY_TYPE_CODE) {
      continue;
    }

    uint8_t ok = 0;
    // Shortest double-click window so a single press reports after ~100 ms.
    bus.setKeyButtonTriggerInterval(id, BUTTON_DOUBLE_CLICK_TIME_100MS, BUTTON_LONG_PRESS_TIME_3S, &ok);
    if (bus.setKeyButtonMode(id, CHAIN_BUTTON_REPORT_MODE, &ok) != CHAIN_OK || !ok) {
      logf("%lu,CHAIN,KEY_MODE_FAILED,port=%u,id=%u\n", millis(), portNumber, id);
      continue;
    }
    bus.setRGBLight(id, AppConfig::CHAIN_KEY_LED_BRIGHTNESS, &ok);

    _bus = &bus;
    _busPort = portNumber;
    _keyId = static_cast<uint8_t>(id);
    _ledValid = false;
    writeLed();
    bus.getEnumPleaseNum();  // drop enum requests that led to this probe
    logf("%lu,CHAIN,KEY_FOUND,port=%u,id=%u,devices=%u\n", millis(), portNumber, id, count);
    return true;
  }
  return false;
}

void ChainKeyManager::lose(const char* reason) {
  logf("%lu,CHAIN,KEY_LOST,port=%u,reason=%s\n", millis(), _busPort, reason);
  _bus = nullptr;
  _keyId = 0;
  _busPort = 0;
  _pressCount = 0;
  _longPressCount = 0;
}

void ChainKeyManager::update(const uint32_t nowMs) {
  if (!present()) {
    if ((nowMs - _lastProbeMs) < AppConfig::CHAIN_PROBE_MS) {
      return;
    }
    _lastProbeMs = nowMs;
    if (!probe(_port2, 2)) {
      probe(_port1, 1);
    }
    _lastHeartbeatMs = nowMs;
    return;
  }

  chain_button_press_type_t type;
  while (_bus->getKeyButtonPressStatus(_keyId, &type)) {
    if (type == CHAIN_BUTTON_PRESS_LONG) {
      if (_longPressCount < 255) ++_longPressCount;
    } else if (_pressCount < 255) {
      ++_pressCount;
    }
  }

  // A device plugged in or re-powered asks to be enumerated: start over.
  if (_bus->getEnumPleaseNum() > 0) {
    lose("re_enumerate");
    _lastProbeMs = nowMs - AppConfig::CHAIN_PROBE_MS;
    return;
  }

  if ((nowMs - _lastHeartbeatMs) >= AppConfig::CHAIN_HEARTBEAT_MS) {
    _lastHeartbeatMs = nowMs;
    if (!_bus->isDeviceConnected(2, 20)) {
      lose("heartbeat");
      _lastProbeMs = nowMs;
    }
  }
}

bool ChainKeyManager::takePress() {
  if (_pressCount == 0) {
    return false;
  }
  --_pressCount;
  return true;
}

bool ChainKeyManager::takeLongPress() {
  if (_longPressCount == 0) {
    return false;
  }
  --_longPressCount;
  return true;
}

void ChainKeyManager::setLed(const uint8_t r, const uint8_t g, const uint8_t b) {
  if (_ledValid && _led[0] == r && _led[1] == g && _led[2] == b) {
    return;
  }
  _led[0] = r;
  _led[1] = g;
  _led[2] = b;
  _ledValid = false;
  writeLed();
}

void ChainKeyManager::writeLed() {
  if (!present() || _ledValid) {
    return;
  }
  uint8_t ok = 0;
  uint8_t rgb[3] = {_led[0], _led[1], _led[2]};
  _ledValid = (_bus->setRGBValue(_keyId, 0, 1, rgb, 3, &ok) == CHAIN_OK && ok);
}
