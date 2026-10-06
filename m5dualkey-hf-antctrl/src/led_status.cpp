#include "led_status.h"

namespace {
constexpr uint32_t MOVING_FLASH_STEP_MS = 140;
constexpr uint8_t WHITE_LEVEL = 200;

constexpr uint32_t SELF_TEST_STEP_MS = 450;

// Physical LED order is swapped on this unit.
constexpr uint8_t PIXEL_A = 1;
constexpr uint8_t PIXEL_B = 0;
}  // namespace

LedStatus::LedStatus(uint8_t powerPin, uint8_t signalPin, uint8_t ledCount)
    : _powerPin(powerPin),
      _strip(ledCount, signalPin, NEO_GRB + NEO_KHZ800),
      _currentMode(OperationalMode::Unknown),
      _displayState(DisplayState::Mode),
      _lastPulseStepMs(0),
      _pulseLevel(0),
      _pulseDelta(0),
      _selfTestStep(0),
      _selfTestLoopsTotal(0),
      _selfTestLoopsDone(0),
      _lastSelfTestStepMs(0),
      _flashPhase(false),
      _isMoving(false),
      _previewA(false),
      _previewB(false) {}

void LedStatus::applyOff() {
  for (uint8_t i = 0; i < _strip.numPixels(); ++i) {
    _strip.setPixelColor(i, _strip.Color(0, 0, 0));
  }
}

void LedStatus::begin() {
  pinMode(_powerPin, OUTPUT);
  digitalWrite(_powerPin, HIGH);

  _strip.begin();
  _strip.setBrightness(AppConfig::LED_BRIGHTNESS);
  applyOff();
  _strip.show();
}

void LedStatus::applyModeStatic() {
  applyOff();

  if (_previewA && _previewB) {
    // Combo action preview (bidirectional)
    _strip.setPixelColor(PIXEL_A, _strip.Color(255, 100, 0));
    _strip.setPixelColor(PIXEL_B, _strip.Color(255, 100, 0));
    _strip.show();
    return;
  }

  if (_previewA) {
    // Button A action preview (forward)
    _strip.setPixelColor(PIXEL_A, _strip.Color(0, 255, 0));
    _strip.show();
    return;
  }

  if (_previewB) {
    // Button B action preview (reverse)
    _strip.setPixelColor(PIXEL_B, _strip.Color(255, 0, 0));
    _strip.show();
    return;
  }

  // Logical mapping: PIXEL_A -> Button A, PIXEL_B -> Button B
  switch (_currentMode) {
    case OperationalMode::Forward:
      if (_isMoving) {
        _strip.setPixelColor(PIXEL_A, _flashPhase ? _strip.Color(WHITE_LEVEL, WHITE_LEVEL, WHITE_LEVEL) : _strip.Color(0, 0, 0));
        _strip.setPixelColor(PIXEL_B, _flashPhase ? _strip.Color(0, 0, 0) : _strip.Color(WHITE_LEVEL, WHITE_LEVEL, WHITE_LEVEL));
      } else {
        _strip.setPixelColor(PIXEL_A, _strip.Color(0, 255, 0));
      }
      break;
    case OperationalMode::Reverse:
      if (_isMoving) {
        _strip.setPixelColor(PIXEL_A, _flashPhase ? _strip.Color(WHITE_LEVEL, WHITE_LEVEL, WHITE_LEVEL) : _strip.Color(0, 0, 0));
        _strip.setPixelColor(PIXEL_B, _flashPhase ? _strip.Color(0, 0, 0) : _strip.Color(WHITE_LEVEL, WHITE_LEVEL, WHITE_LEVEL));
      } else {
        _strip.setPixelColor(PIXEL_B, _strip.Color(255, 0, 0));
      }
      break;
    case OperationalMode::Bidirectional:
      if (_isMoving) {
        _strip.setPixelColor(PIXEL_A, _flashPhase ? _strip.Color(WHITE_LEVEL, WHITE_LEVEL, WHITE_LEVEL) : _strip.Color(0, 0, 0));
        _strip.setPixelColor(PIXEL_B, _flashPhase ? _strip.Color(0, 0, 0) : _strip.Color(WHITE_LEVEL, WHITE_LEVEL, WHITE_LEVEL));
      } else {
        _strip.setPixelColor(PIXEL_A, _strip.Color(255, 100, 0));
        _strip.setPixelColor(PIXEL_B, _strip.Color(255, 100, 0));
      }
      break;
    case OperationalMode::Unknown:
    default:
      break;
  }

  _strip.show();
}

void LedStatus::applyReversePulseStep() {
  _flashPhase = !_flashPhase;

  applyOff();
  _strip.setPixelColor(PIXEL_A, _flashPhase ? _strip.Color(WHITE_LEVEL, WHITE_LEVEL, WHITE_LEVEL) : _strip.Color(0, 0, 0));
  _strip.setPixelColor(PIXEL_B, _flashPhase ? _strip.Color(0, 0, 0) : _strip.Color(WHITE_LEVEL, WHITE_LEVEL, WHITE_LEVEL));
  _strip.show();
}

void LedStatus::applySelfTestStep() {
  applyOff();

  switch (_selfTestStep % 4) {
    case 0:
      _strip.setPixelColor(PIXEL_A, _strip.Color(0, 255, 0));
      break;
    case 1:
      _strip.setPixelColor(PIXEL_B, _strip.Color(255, 0, 0));
      break;
    case 2:
      _strip.setPixelColor(PIXEL_A, _strip.Color(255, 100, 0));
      _strip.setPixelColor(PIXEL_B, _strip.Color(255, 100, 0));
      break;
    case 3:
    default:
      // All off between cycles to clearly show transitions.
      break;
  }

  _strip.show();
}

void LedStatus::setMode(const OperationalMode mode) {
  if (_displayState == DisplayState::Mode && mode == _currentMode) {
    return;
  }

  _displayState = DisplayState::Mode;
  _currentMode = mode;
  _flashPhase = false;
  _lastPulseStepMs = millis();
  applyModeStatic();
}

void LedStatus::setMoving(const bool moving) {
  if (_isMoving == moving) {
    return;
  }

  _isMoving = moving;
  _flashPhase = false;
  _lastPulseStepMs = millis();

  if (_displayState == DisplayState::Mode) {
    applyModeStatic();
  }
}

void LedStatus::setButtonPreview(const bool buttonAPressed, const bool buttonBPressed) {
  if (_previewA == buttonAPressed && _previewB == buttonBPressed) {
    return;
  }

  _previewA = buttonAPressed;
  _previewB = buttonBPressed;

  if (_displayState == DisplayState::Mode) {
    applyModeStatic();
  }
}

void LedStatus::startModeCycle(const uint8_t loops) {
  if (loops == 0) {
    return;
  }

  _displayState = DisplayState::SelfTest;
  _selfTestStep = 0;
  _selfTestLoopsTotal = loops;
  _selfTestLoopsDone = 0;
  _lastSelfTestStepMs = millis();
  applySelfTestStep();
}

void LedStatus::update(const uint32_t nowMs) {
  if (_displayState == DisplayState::SelfTest) {
    if ((nowMs - _lastSelfTestStepMs) < SELF_TEST_STEP_MS) {
      return;
    }
    _lastSelfTestStepMs = nowMs;

    _selfTestStep = static_cast<uint8_t>((_selfTestStep + 1) % 4);
    if (_selfTestStep == 0) {
      ++_selfTestLoopsDone;
      if (_selfTestLoopsDone >= _selfTestLoopsTotal) {
        _displayState = DisplayState::Mode;
        _flashPhase = false;
        _lastPulseStepMs = nowMs;
        applyModeStatic();
        return;
      }
    }

    applySelfTestStep();
    return;
  }

  if (_isMoving && !_previewA && !_previewB) {
    if ((nowMs - _lastPulseStepMs) >= MOVING_FLASH_STEP_MS) {
      _lastPulseStepMs = nowMs;
      applyReversePulseStep();
    }
    return;
  }

  // Non-animated modes remain static.
}
