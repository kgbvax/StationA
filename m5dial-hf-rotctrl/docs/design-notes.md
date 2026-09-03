# Salvage: m5-dial-rotator-head.md
> Extracted from PRD/03-components/m5-dial-rotator-head.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [decision] O2 — Publish QoS (still open)
The reference publishes commands at QoS 0 (DV1). The station convention is QoS 1.
Keep the deviation with its visible failure mode, or migrate to an MQTT library
that publishes at QoS 1. The rotator bridge's command queue (capacity 8, silently
drops excess) makes lost intermediate detents harmless. Only a lost final flush
can strand the target pointer away from the needle.

DV1 background: PubSubClient cannot publish above QoS 0. Retention does not
apply (the commands are one-shot), so the loss mode is a dropped command,
visible as a target-pointer versus needle mismatch. A library migration
(e.g. espMqttClient) is the fix candidate.

Code check 2026-09-03: still open — `src/mqtt_client.h` and `src/main.cpp`
still comment "publishes go out at QoS 0 (PubSubClient cannot publish above
QoS 0)". `docs/m5dial-hf-rotctrl-mqtt-api.md` already points here as "tracked
as an open decision in the PRD".

## [decision] O8 — Non-blocking MQTT connect (still open)
The reference blocks the main loop for up to about 1 s per reconnect try
against a silent broker host (DV4). During that freeze, the display stalls and
the device can miss a press. A non-blocking connect state machine, or a connect
on a second task, removes the freeze. Decide whether the ~1 s freeze is
acceptable for v1.

DV4 background: PubSubClient's `connect()` is synchronous. Against a silent
broker host (powered off, no answer to the connection request) the reference
caps the block at about 1 s through the WiFi client's connect timeout. Worst
case during a full broker outage: the loop freezes about 1 s of every 5 s.
A fully non-blocking connect is the open decision.

Code check 2026-09-03: still open — `src/mqtt_client.h` still calls
`net_.setTimeout(1)` (arduino-esp32 core quirk: seconds, not ms) around the
synchronous connect. `docs/m5dial-hf-rotctrl-mqtt-api.md` points here as an
open decision.

## [decision] O4 — Display tearing (needs a bench check)
The reference pushes the full 240x240 frame at up to 30 Hz. Tearing can be
visible on the round panel. The fallback is a 20 Hz frame cap. Needs a bench
check with the physical display.

Code check 2026-09-03: no evidence of a 20 Hz fallback or tearing fix in
`src/config.h`/`src/face.h` (frame period still 33 ms / ~30 Hz); the bench
check is still outstanding.

## [decision] O5 — Touch surface (hardware option, out of scope)
The device has a touch panel. The reference does not use it. A future extension
can add touch presets. Out of scope, but the hardware holds the option.

## [decision] O6 — Blind detent handling (choice is final unless re-opened)
The reference ignores detents while liveness is down (R4.7). The alternative
was a local-only target that publishes when the link returns. That alternative
breaks the no-replay rule (R5.3). The choice is final unless the team re-opens
it with a replay-safe design.

## [decision] O1 — Encoder direction and counts (CLOSED, keep as closed record)
CLOSED (bench test, 2026-09-01). One knob-flat yields exactly 4 quadrature
counts. The raw counts run inverted: a clockwise turn reads negative. The
reference sets its invert constant, so a clockwise turn steps the target up
(R4.6). A serial trace confirmed the counts over a full 0-to-450 sweep while
the needle animated.

Code check 2026-09-03: resolved in code — `src/config.h` carries
`ENCODER_COUNTS_PER_DETENT 4` and `ENCODER_INVERT 1`. Note:
`docs/m5dial-hf-rotctrl-mqtt-api.md` "Known deviations" item 3 still says the
constants are "pending hardware verification on the bench" — stale; update that
doc when folding this in.

## [requirement] Analog face acceptance requirements (R3.1–R3.8)
The live docs describe the face only in overview; these normative details are
stated nowhere else.

- The card MUST be a compass scale of 0 through 360 degrees on one circle. One
  azimuth degree MUST take one degree of arc. North MUST sit at the top.
- The face MUST wrap azimuth onto the compass card for display (azimuth 390 at
  the 30-degree mark). The card carries no second scale and no overlap band;
  only the plaque marks the overlap pass.
- The card MUST print numerals every 30 degrees (0 through 330) and fine ticks
  every 10 degrees, one N mark at north, and E, S, W at the cardinals. Scale
  text under the readout window is exempt — a real meter card leaves its
  cutout window clear.
- The face MUST show a digital readout window with two lines: measured azimuth
  (AZ) and commanded target azimuth (TGT). Both MUST print one decimal place
  (the 22.5-degree detent step carries a half degree). Both MUST wrap for
  display and each MUST truncate, not round, so the display never prints 360.
  A small **+360 hint** MUST sit next to any line whose underlying value is
  360 or above. The needle and the target pointer MUST render behind the
  window.
- The needle MUST move with exponential damping, not in steps. A sudden
  90-degree step of the input MUST settle in less than 3 s. The damping
  constant is implementation choice within that limit (reference: about 0.55 s
  time constant).
- The needle MUST track slow rotation with a visible lag of no more than
  5 degrees. The face must read like a physical meter movement, not like a
  digital readout.
- The target pointer MUST jump at once when the target changes; the device
  damps only the needle. The pointer MUST be a different color from the needle.
- While the rotator moves, the face MUST show the commanded sweep: an arc from
  the measured azimuth to the target azimuth, in the target color, drawn in the
  direction the rotator travels. Both ends of the arc MUST sit at their wrapped
  compass positions. The arc MUST cap its span at 360 screen degrees; the
  +360 hint carries any owed pass beyond one turn.

## [requirement] Repaint discipline and frame budget (R3.12/R3.13)
- The face MUST repaint when the needle has not settled, when any displayed
  value or flag changes, or at least every 1000 ms. It MUST NOT repaint in a
  tight loop.
- The frame rate MUST stay at or under 30 Hz. The full frame is one screen
  push per repaint. (Tearing fallback: see O4 above.)

## [requirement] Timing tunables are normative (reference defaults)
A change to any tunable changes observable timing. The team MUST use exactly
these values unless an open decision says otherwise.

| Tunable | Default |
|---|---|
| detent step | 22.5 degrees (one knob-flat, 16 detents per revolution) |
| publish interval within a burst | 200 ms |
| final flush after the last detent | 400 ms |
| target re-sync idle window | 2000 ms |
| needle damping time constant | about 0.55 s |
| frame period | 33 ms |
| quiescent repaint | 1000 ms |

Code check 2026-09-03: matches `src/config.h`
(`DETENT_DEG 22.5`, `PUBLISH_FLUSH_MS 400`, `TARGET_RESYNC_IDLE_MS 2000`,
`FRAME_MS 33`, `QUIESCENT_REFRESH_MS 1000`).

## [requirement] Power/reliability posture (R7.3)
The device MUST prefer WiFi reliability over power saving. It runs on mains
power.

## [unique] Desk-sim model: deliberate halt latency (stop-vs-detent race)
Beyond the live docs' sim description: the sim model (`src/sim.h`) holds the
halt `/state` back for a short delay (`SIM_HALT_LATENCY_MS`) — on the wire the
stop crosses the broker while the mast coasts — so the stop-versus-detent race
(the press re-baseline path) stays testable on the desk.

## [unique] Reference implementation lineage (non-normative notes)
- Reference hardware: M5Stack Dial — ESP32-S3, GC9A01 round 240x240 display,
  16-detent rotary encoder on GPIO 40/41, buzzer. Libraries: `M5Dial ^1.0`
  (display, encoder, button), `PubSubClient ^2.8` (MQTT), `ArduinoJson ^7.0`.
  The face renders into one full-frame in-RAM sprite of about 115 KB, pushed to
  the display per repaint; if that allocation fails, the device draws direct to
  the display as a fallback.
- DV3: the update start's stop publish is fire-and-forget; it carries no
  confirmation.
- DV5 (rationale worth keeping): the reference does not use the encoder class
  of the M5Dial library — that class attaches interrupts only for pins below
  40, so on the Dial's GPIO 40/41 it silently polls instead, and a polled state
  machine loses whole detents while the display renders. The reference carries
  its own interrupt-driven quadrature decoder instead.