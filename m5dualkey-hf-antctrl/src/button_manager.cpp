#include "button_manager.h"

ButtonManager::ButtonManager(uint8_t pinA, uint8_t pinB, uint32_t debounceMs, uint32_t longPressMs, uint32_t comboWindowMs)
    : _a{pinA, false, false, false, false, false, 0, 0},
      _b{pinB, false, false, false, false, false, 0, 0},
      _debounceMs(debounceMs),
      _longPressMs(longPressMs),
      _comboWindowMs(comboWindowMs),
      _comboActive(false),
      _comboStartMs(0),
      _head(0),
      _tail(0) {}

void ButtonManager::begin() {
  pinMode(_a.pin, INPUT);
  pinMode(_b.pin, INPUT);
}

bool ButtonManager::isPressedRaw(const ButtonState& state) const {
  // Chain DualKey buttons are active-low.
  return !digitalRead(state.pin);
}

void ButtonManager::pushEvent(const ButtonId id, const ButtonEventType type, const uint32_t nowMs) {
  const uint8_t nextHead = static_cast<uint8_t>((_head + 1) % QUEUE_SIZE);
  if (nextHead == _tail) {
    // Queue full: drop oldest.
    _tail = static_cast<uint8_t>((_tail + 1) % QUEUE_SIZE);
  }
  _queue[_head] = ButtonEvent{id, type, nowMs};
  _head = nextHead;
}

void ButtonManager::processButton(ButtonState& state, const ButtonId id, ButtonState& other, const uint32_t nowMs) {
  const bool raw = isPressedRaw(state);

  if (raw != state.lastStableSample) {
    state.lastStableSample = raw;
    state.lastRawChangeMs = nowMs;
  }

  if ((nowMs - state.lastRawChangeMs) < _debounceMs) {
    return;
  }

  if (state.debouncedPressed == raw) {
    if (state.debouncedPressed && !state.longPressFired && (nowMs - state.pressedSinceMs) >= _longPressMs) {
      state.longPressFired = true;
      pushEvent(id, ButtonEventType::LongPress, nowMs);
    }
    return;
  }

  // Debounced edge
  state.debouncedPressed = raw;
  if (state.debouncedPressed) {
    state.longPressFired = false;
    state.suppressNextShort = false;
    state.pressedSinceMs = nowMs;
    pushEvent(id, ButtonEventType::Press, nowMs);

    if (other.debouncedPressed && ((nowMs - other.pressedSinceMs) <= _comboWindowMs)) {
      _comboActive = true;
      _comboStartMs = nowMs;
      state.suppressNextShort = true;
      other.suppressNextShort = true;
      pushEvent(ButtonId::Both, ButtonEventType::ComboPress, nowMs);
    }
  } else {
    pushEvent(id, ButtonEventType::Release, nowMs);

    if (!state.longPressFired && !state.suppressNextShort) {
      pushEvent(id, ButtonEventType::ShortPress, nowMs);
    }

    state.suppressNextShort = false;

    if (_comboActive && !other.debouncedPressed) {
      _comboActive = false;
    }
  }
}

void ButtonManager::update(const uint32_t nowMs) {
  processButton(_a, ButtonId::A, _b, nowMs);
  processButton(_b, ButtonId::B, _a, nowMs);

  if (_comboActive && !_a.debouncedPressed && !_b.debouncedPressed) {
    _comboActive = false;
  }
}

bool ButtonManager::nextEvent(ButtonEvent& outEvent) {
  if (_tail == _head) {
    return false;
  }
  outEvent = _queue[_tail];
  _tail = static_cast<uint8_t>((_tail + 1) % QUEUE_SIZE);
  return true;
}
