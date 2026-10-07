#pragma once

#include <Arduino.h>

enum class ButtonId : uint8_t {
  None = 0,
  A,
  B,
  Both,
};

enum class ButtonEventType : uint8_t {
  None = 0,
  Press,
  Release,
  ShortPress,
  LongPress,
  ComboPress,
};

struct ButtonEvent {
  ButtonId button;
  ButtonEventType type;
  uint32_t timestamp;
};

class ButtonManager {
 public:
  ButtonManager(uint8_t pinA, uint8_t pinB, uint32_t debounceMs, uint32_t longPressMs, uint32_t comboWindowMs);

  void begin();
  void update(uint32_t nowMs);
  bool nextEvent(ButtonEvent& outEvent);

 private:
  struct ButtonState {
    uint8_t pin;
    bool rawPressed;
    bool debouncedPressed;
    bool lastStableSample;
    bool longPressFired;
    bool suppressNextShort;
    uint32_t lastRawChangeMs;
    uint32_t pressedSinceMs;
  };

  static constexpr uint8_t QUEUE_SIZE = 16;

  ButtonState _a;
  ButtonState _b;
  uint32_t _debounceMs;
  uint32_t _longPressMs;
  uint32_t _comboWindowMs;

  bool _comboActive;
  uint32_t _comboStartMs;

  ButtonEvent _queue[QUEUE_SIZE];
  uint8_t _head;
  uint8_t _tail;

  bool isPressedRaw(const ButtonState& state) const;
  void processButton(ButtonState& state, ButtonId id, ButtonState& other, uint32_t nowMs);
  void pushEvent(ButtonId id, ButtonEventType type, uint32_t nowMs);
};
