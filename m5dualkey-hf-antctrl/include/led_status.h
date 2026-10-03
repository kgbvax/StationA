#pragma once

#include <Arduino.h>
#include <Adafruit_NeoPixel.h>

#include "config.h"

class LedStatus {
 public:
  LedStatus(uint8_t powerPin, uint8_t signalPin, uint8_t ledCount);

  void begin();
  void setMode(OperationalMode mode);
  void setMoving(bool moving);
  void setButtonPreview(bool buttonAPressed, bool buttonBPressed);
  void update(uint32_t nowMs);
  void startModeCycle(uint8_t loops = 1);

 private:
  enum class DisplayState : uint8_t {
    Mode,
    SelfTest,
  };

  uint8_t _powerPin;
  Adafruit_NeoPixel _strip;
  OperationalMode _currentMode;
  DisplayState _displayState;

  uint32_t _lastPulseStepMs;
  int16_t _pulseLevel;
  int16_t _pulseDelta;

  uint8_t _selfTestStep;
  uint8_t _selfTestLoopsTotal;
  uint8_t _selfTestLoopsDone;
  uint32_t _lastSelfTestStepMs;
  bool _flashPhase;
  bool _isMoving;
  bool _previewA;
  bool _previewB;

  void applyOff();
  void applyModeStatic();
  void applyReversePulseStep();
  void applySelfTestStep();
};
