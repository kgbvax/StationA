# Requirements — m5btn2 Firmware
Version: 0.1 (draft)
Date: 2026-06-07
Status: Draft

## 1) Goal
Build reliable firmware for an  M5Stack Chain Dual Key  that detects button interactions and provides predictable feedback for integration into larger workflows.

## 2) Scope

### In scope
- Initialize board and runtime using Arduino framework.
- Read two physical buttons reliably.
- Debounce button signals in firmware.
- Detect press/release and short/long press events.
- Output events over serial (USB CDC) for debugging/integration.
- Keep implementation compatible with PlatformIO and M5Unified.
- On presses, send events to certain mqtt topics
- subscripe to state topics are reflect the state by led color

### Out of scope
- Cloud backends and internet services beyond local-network MQTT integration.
- OTA update pipeline.
- Mobile/desktop UI.
- Persistent settings storage.

## 3) Context / Environment
- Build system: PlatformIO
- Framework: Arduino
- Platform: `espressif32`
- MCU: `esp32s3` at 240 MHz
- Flash size: 8 MB
- Board profile: `esp32-s3-devkitc-1` (or compatible M5Stack StampS3 config as needed)
- Library dependency: `m5stack/M5Unified @ ^0.1.16`
- Serial over USB CDC enabled on boot
- the board is described here: https://docs.m5stack.com/en/chain/Chain_DualKey 
- confirmed pin mapping for this implementation:
  - Key1 / Button A: GPIO0
  - Key2 / Button B: GPIO17
  - LED signal: GPIO21
  - LED power enable: GPIO40
- **Superseded (2026-10-03):** the binding below is the pre-stationa `ubctrl/*`
  API. The firmware now uses slot `muehle/hf/ant-ctrl` — see `README.md`.
- Using the followign MQTT Binding:
### Home Assistant / MQTT Binding

#### Topic Prefixes

- Base prefix (configurable): `<prefix>` (default: `ubctrl`)
- Discovery prefix: `homeassistant/...`

#### Published State Topics

- `<prefix>/status/frequency`
  - JSON: `{ "frequency": <kHz>, "band": "...", "mode": "forward|reverse|bidirectional" }`
- `<prefix>/status/motors`
  - JSON: `{ "moving": <bool>, "motor_bits": <int> }`
- `<prefix>/status/availability`
  - String: `online`
- `<prefix>/status/raw`
  - Full state object from `/api/status`

#### Command Topics

- `<prefix>/command/frequency`
  - Payload: integer frequency in kHz (e.g. `14000`)
- `<prefix>/command/mode`
  - Payload: `forward`, `reverse`, or `bidirectional`
  - Compatibility aliases also accepted: `normal`, `180`, `bidir`
- `<prefix>/command/retract`
  - Any payload triggers retract




## 4) Functional Requirements

- **FR-001 Initialization**
  - The firmware shall initialize serial output and board peripherals during `setup()`.
  - Acceptance criteria:
    - On boot, a startup message appears on serial within 2 seconds.
    - No blocking loop in setup beyond initialization.

- **FR-002 Button sampling**
  - The firmware shall sample both button inputs continuously in `loop()`.
  - Acceptance criteria:
    - Both buttons report state transitions (pressed/released) when actuated.
    - No missed transition under normal manual press speed.

- **FR-003 Debounce**
  - The firmware shall debounce each button input.
  - Acceptance criteria:
    - Contact bounce shall not generate multiple press events for a single physical press.
    - Debounce window configurable (default: 30 ms).

- **FR-004 Short press event**
  - The firmware shall emit a short-press event when a button is pressed and released under long-press threshold.
  - Acceptance criteria:
    - Event format includes button ID and event type.
    - Default short press timing: press duration < 600 ms.

- **FR-005 Long press event**
  - The firmware shall emit a long-press event when press duration reaches threshold.
  - Acceptance criteria:
    - Long-press threshold configurable (default: 600 ms).
    - Long-press event emitted once per hold action.

- **FR-006 Dual-button behavior**
  - The firmware shall support independent and simultaneous button operation.
  - Acceptance criteria:
    - Pressing both buttons together does  suppress individual detection.
    - Event output identifies both button together.

- **FR-007 Event output**
  - The firmware shall output events over serial in machine-readable format.
  - Acceptance criteria:
    - Output format is stable and documented (e.g., `BTN_A:SHORT`, `BTN_B:LONG`).
    - Events are timestamped in milliseconds since boot.

- **FR-008 Event output**
  - Pressing button A should send a "forward" to <prefix>/command/mode
  - Pressing button B should send a "reverse" to <prefix>/command/mode
  - Pressing both buttons   should send a "bidirectional" to <prefix>/command/mode

- **FR-009 Status display**
  - The firmware shall subscribe to <prefix>/status/frequency
    - Whem mode is forward, the LED at Button A should be green
    - When mode is reverse, the LED at Button B should light up red
    - When mode is birectional, the LEDS at Button  A and B should light up Orange


## 5) Non-Functional Requirements

- **NFR-001 Responsiveness**
  - End-to-end event latency (press to serial output) should be ≤ 50 ms (excluding long-press threshold timing).

- **NFR-002 Reliability**
  - Firmware shall run continuously for 24 hours without crash/reboot under normal button interaction.

- **NFR-003 Maintainability**
  - Button handling logic shall be separated into reusable functions/classes.
  - Constants (debounce, thresholds) shall be centralized as named configuration values.

- **NFR-004 Resource usage**
  - Implementation shall avoid dynamic memory allocation in the main loop.

## 6) Interfaces & Data Contract

### Input
- Physical buttons: Button A, Button B

### Output
- Serial events (USB CDC), one line per event.
- Recommended event schema:
  - `<millis>,<button_id>,<event_type>`
  - Example: `15320,BTN_A,SHORT_PRESS`

## 7) Constraints
- Must compile under PlatformIO environment `m5stack_chain_dualkey`.
- Must use Arduino framework and remain compatible with `M5Unified`.
- Must not depend on internet connectivity.
- Must keep loop non-blocking (no long `delay()` usage except minimal, justified cases).

## 8) Risks & Edge Cases
- Mechanical bounce causing duplicate events.
- Very fast tapping near debounce threshold.
- Simultaneous button press race conditions.
- USB serial unavailable at power-on (events should still be handled internally).

## 9) Validation Plan

### Build validation
- `pio run` completes without errors/warnings blocking build.

### Functional tests (manual)
- Test single short press per button.
- Test single long press per button.
- Test repeated rapid short presses (10+ per button).
- Test simultaneous press and staggered release.
- Verify no duplicate events from bounce.

### Stability test
- 24h idle + periodic interaction check for lockups/reset.

## 10) Definition of Done
- All FR and NFR acceptance criteria pass.
- Event format documented and stable.
- Code compiles cleanly in target PlatformIO env.
- Basic manual test log captured in project notes.

## 11) Open Questions
- [resolved] Exact pin mapping for Button A and Button B? (A=GPIO0, B=GPIO17)
- [resolved] Final long-press threshold value: 600 ms default
- [resolved] Need combo action (`A+B`): yes, unique combo event using a time window (default 100 ms)
 