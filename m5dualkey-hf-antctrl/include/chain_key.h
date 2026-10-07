#pragma once

#include <Arduino.h>
#include <M5Chain.h>

// An M5Stack Chain Key on one of the DualKey's HY2.0-4P Chain-bus ports.
// Probes both ports, enumerates the bus, and reports key presses. Survives
// unplug/replug (heartbeat + the bus's enum-please on hot plug).
class ChainKeyManager {
 public:
  ChainKeyManager();

  void begin();
  void update(uint32_t nowMs);

  bool present() const { return _keyId != 0; }

  // Pops one press (single or double) since the last call.
  bool takePress();
  // Pops one long press since the last call.
  bool takeLongPress();

  // Key LED; written to the bus only when the colour changes.
  void setLed(uint8_t r, uint8_t g, uint8_t b);

 private:
  Chain _port1;
  Chain _port2;
  Chain* _bus;
  uint8_t _keyId;
  uint8_t _busPort;
  uint32_t _lastProbeMs;
  uint32_t _lastHeartbeatMs;
  uint8_t _pressCount;
  uint8_t _longPressCount;
  uint8_t _led[3];
  bool _ledValid;

  bool probe(Chain& bus, uint8_t portNumber);
  void lose(const char* reason);
  void writeLed();
};
