#pragma once

#include <Arduino.h>
#include <cstdarg>
#include <cstdio>

#include "config.h"

inline bool serialLogReady() {
  if (!AppConfig::ENABLE_LOGS) {
    return false;
  }

  // With USB CDC builds, this is false when no host is attached/open.
  return static_cast<bool>(Serial);
}

inline void logf(const char* format, ...) {
  if (!serialLogReady() || format == nullptr) {
    return;
  }

  char buffer[256];
  va_list args;
  va_start(args, format);
  vsnprintf(buffer, sizeof(buffer), format, args);
  va_end(args);

  Serial.print(buffer);
}

inline void logln(const char* text) {
  if (!AppConfig::ENABLE_LOGS) {
    return;
  }

  if (!serialLogReady()) {
    return;
  }
  Serial.println(text);
}
