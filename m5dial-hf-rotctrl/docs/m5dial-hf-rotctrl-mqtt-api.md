# m5dial-hf-rotctrl — MQTT interface

**M5Stack Dial HF-rotator control head.** Firmware for an M5Stack Dial
(ESP32-S3FN8, 1.28" round GC9A01 240×240 display, rotary encoder with push,
WiFi) mounted in the shack as a physical analog control head for the HF
rotator slot (`muehle/hf/rotator`, Yaesu G-450DC via `wrc-rotator-bridge`).

This device is a **consumer + `/cmd` stimulator, NOT a slot** (station
integration model §9 — same class as `testui`
and `hf_console`). It publishes no `/meta`, `/state` or `/status` of any
address, has **no LWT and no heartbeat**: it has no MQTT presence of its own.
Its entire bus footprint is two subscriptions and two publish shapes on the
rotator slot. Deleting it changes nothing for any other component.

## Connection

| Property | Value |
|----------|-------|
| Broker | `192.168.1.139:1883` (shack broker on shari, LAN address — never `127.0.0.1`, the Dial is a remote device) |
| User | `dial` (narrow account: read rotator `state`/`status`, write rotator `cmd` — see `mqtt-broker/acl.conf.example`) |
| Client ID | `dial-hf-rotctrl-1` (documented non-slot exception; must not collide with slot-derived bridge IDs) |
| MQTT | 3.1.1, clean session, keepalive 30 s, buffer 1024 |
| Will | **none** (not a slot — no `/status` of its own to take offline) |
| Subscribe QoS | 1 |
| Publish QoS | 0 — PubSubClient cannot publish above QoS 0 (see *Known deviations* below) |

## Subscriptions

| Topic | Retained | Purpose |
|-------|----------|---------|
| `muehle/hf/rotator/state` | yes | the needle + liveness feed |
| `muehle/hf/rotator/status` | yes | bridge liveness (LWT), one of the two liveness layers |

Both are re-subscribed on every MQTT connect edge; the retained replay is the
boot-time state feed (no other request/refresh exists — and none is needed:
the bridge change-dedups `/state`, so **a parked rotator is silent by design;
silence is never read as staleness**).

### `/state` fields consumed

```json
{"ts":"2026-07-06T12:34:56Z","az":123.5,"target_az":180.0,"moving":true,"device_online":true}
```

| Field | Used for |
|-------|----------|
| `az` | needle position (damped ~0.55 s time constant for the meter-movement feel); `AZ` plaque readout |
| `target_az` | target initialization and idle re-sync (below); ignored while the operator is turning |
| `moving` | red pending-sweep arc; the arrival beep edge (`moving` true→false within 1.5° of the target) |
| `device_online` | liveness layer 2 — the WRC websocket link. Bridge `/status` alone is **not** trusted: `/status` = bridge process, `device_online` = rotator link. The face ANDs them (green ring = all up, amber = bridge up + link down, red = bridge/MQTT down, gray = no data yet) |

## Publications

All to `muehle/hf/rotator/cmd` — **NOT retained** (one-shot intent; the
rotator is explicitly not a self-healing actuator, wrc-rotator-bridge
invariant 1: a stale retained target replay can spin the antenna).

| Action | Payload | When |
|--------|---------|------|
| `set_az` | `{"action":"set_az","az":180}` | knob detents (target = last target ± 22.5° per detent — one knob-flat, one compass step) |
| `stop` | `{"action":"stop"}` | knob press (ack beep sounds when it publishes); also published (best-effort) at the start of an OTA firmware update |

**`az`, not `value`:** the rotator's command grammar predates the station-wide
`value`-key convention; the argument key is `az`, declared in the slot's
`/meta.expose` as `value_key:"az", value_type:"float"`. Do not "fix" it.

### Publish rules

- **Coalescing.** The bridge's `/cmd` worker queue is 8 deep and silently drops
  on overflow, so the knob never machine-guns. First detent of a burst
  publishes immediately; while turning, at most one publish per 200 ms
  (always the latest target); a final flush 400 ms after the last detent.
  A `stop` flushes any pending `set_az` **first**, so a stop never lands
  behind our own queued move.
- **No replay.** A pending target is dropped on MQTT disconnect and never
  republished on reconnect. The Dial publishes only in response to fresh
  operator input or a stop. After a broker/WiFi outage the knob does nothing
  until the operator turns it again.
- **Blind-input gating.** Detents are ignored while liveness is down (MQTT
  disconnected, `/status` ≠ `online`, or `device_online` false). Queueing
  unobservable motion commands while blind is worse than ignoring the input.
  Press = `stop` still publishes whenever MQTT is connected (a stop is safe in
  every state — including when the WRC link is down, where the bridge will
  relay it if/when the link returns).
- **Liveness is re-established, never carried over.** On the MQTT disconnect
  edge the Dial drops all liveness knowledge (`statusSeen`/`haveState`) and the
  face shows `NO DATA` until the resubscribe's retained `/status` + `/state`
  replay lands. Without this, a reconnect would trade on stale pre-disconnect
  flags for the replay window — detents would be accepted against a
  possibly-dead slot and the QoS 0 `set_az` publishes would go nowhere.
- **Press = stop re-baselines from the halt state.** The press sets a
  provisional target from the last-reported `az` (the antenna coasts past
  that while the stop travels and brakes); the subsequent halt `/state`
  (`moving:false`) carries the true stop point and re-baselines the target,
  bypassing the 2 s turning-guard (a press right after turning is the normal
  stop case, and the change-deduped `/state` would otherwise go silent
  before any correction).
- **Clamping.** Targets are clamped to the WRC range 0…450° and never wrap
  (wrapping in azimuth space would command a full-circle spin). The *display*
  wraps — the face is a 0…360° compass card (az 390 renders at 30°) with a
  `+360` hint when the underlying value rides the 360…450 overlap pass.

## Liveness matrix → face behavior

| State | Ring | Badge | Detents | Press |
|-------|------|-------|---------|-------|
| All up | green | — (clean face) | step + publish | stop |
| Bridge/MQTT down (`/status` ≠ online or disconnected) | red | `OFFLINE` | ignored | stop (only if MQTT connected) |
| WRC link down (`device_online` false) | amber | `NO LINK` | ignored | stop |
| No `/state` yet (boot, or reconnect before the retained replay) | gray | `NO DATA` | ignored | stop (if connected) |

Known limitation inherited from the bus: the bridge change-dedups `/state`
and `status` is a plain retained LWT, so a *clean* bridge stop can leave a
stale `online` + last-state pair visible until the broker/bridge cycle. The
face cannot distinguish that case; it is a station-level known defect of the
wrc-rotator-bridge, not a Dial defect.

## Known deviations / limitations

1. **Publishes at QoS 0** — PubSubClient cannot publish above QoS 0 (only
   subscribe supports QoS 1; the m5stamp-hf-ctrl firmware has the same
   limitation). A lost *final* set_az flush on a flaky link is possible;
   the face makes it visible (TGT plaque vs needle divergence), and the
   operator nudges the knob again. hf_console publishes at QoS 1 from the
   tablet; if this proves lossy in practice, migrating the firmware to
   espMqttClient is the fix (open decision; see `../docs/design-notes.md`).
2. **OTA stop is best-effort** — if MQTT is down when a firmware update
   starts, no stop goes out. Operator rule: do not update during motion with
   the link down.
3. **Encoder direction/counts** are config constants (`ENCODER_INVERT`,
   `ENCODER_COUNTS_PER_DETENT` in `src/config.h`) — bench-verified 2026-09-01:
   one knob-flat = 4 counts, raw counts run inverted (CW reads negative, the
   invert constant compensates). Closed record in `../docs/design-notes.md`.
4. **Bounded blocking reconnect** — PubSubClient's `connect()` is synchronous
   and, against a *silent* broker host (shari powered off, SYN unanswered —
   a refused connection returns fast), blocks in `select()`. The firmware
   caps the block at ~1 s via `WiFiClient::setTimeout(1)` (the arduino-esp32
   `WiFiClient::setTimeout` takes **seconds**, not ms — core quirk). Worst
   case during a full broker outage: the loop freezes ~1 s of every 5 s
   (face repaint and input reading stall; a short press entirely inside a
   frozen window is lost). A fully non-blocking connect state machine would
   remove even that; open decision (see `../docs/design-notes.md`).

## Verification recipe

With the Dial on the bench and the shack broker reachable
(`$MQTT_PASSWORD` = the `dial` account password):

```bash
# Terminal 1 — watch everything the Dial does
mosquitto_sub -h 192.168.1.139 -u dial -P "$MQTT_PASSWORD" -t 'muehle/hf/rotator/#' -v

# Terminal 2 — simulate the bridge (the real bridge may be stopped, or these
# publishes are simply newer; both are retained)
mosquitto_pub -h 192.168.1.139 -u dial -P "$MQTT_PASSWORD" -t muehle/hf/rotator/status \
  -m online -r
mosquitto_pub -h 192.168.1.139 -u dial -P "$MQTT_PASSWORD" -t muehle/hf/rotator/state \
  -m '{"az":180,"target_az":180,"moving":false,"device_online":true}' -r
```

1. Boot the Dial → needle sweeps 0→180 in ~3 s, ring green, TGT 180.0.
2. Turn the knob → immediate `set_az`, coalesced while turning, final flush
   after ~400 ms; verify against Terminal 1.
3. Press the knob mid-burst → pending target flushes, then `stop`. A long
   hold then release does the same (press = stop, any duration).
4. Publish `status` `offline` → red ring + OFFLINE badge, detents dead, press
   still publishes `stop`.
5. Publish `state` with `device_online:false` → amber ring + NO LINK badge.
6. Cycle the Dial's WiFi → after reconnect **no** `set_az` until a fresh detent.
7. While simulated motion is being commanded, `./deploy.sh ota` → `stop`
   published at update start, then the new image re-subscribes.

## Desk-simulation build (no MQTT at all)

`./deploy.sh sim` flashes a `SIM_MODE=1` image (USB only) for bench
development with **no antenna, no broker and no WiFi**. The station side of
the wire is a local mast model (`src/sim.h`) that speaks this contract
internally: it consumes `set_az`/`stop` the way the `wrc-rotator-bridge` does
(target clamped to 0…450; a stop halts ±1° past the measured azimuth with the
cancelled target still reported) and emits the same `/state` JSON through the
same parser, change-deduped (a parked sim is silent, like a parked rotator).
The face, knob, coalescer and stop re-baseline logic are the production code;
the model slews at the real G-450DC rate (~5.7°/s), so a 180° turn takes ~32 s.
The face carries a small SIM label so a desk face is never mistaken for the
live antenna. The sim image has no OTA listener — returning to the live image
is a `./deploy.sh usb` flash.

## Reference-implementation notes

PlatformIO/Arduino firmware, `m5stack/M5Dial @ ^1.0` (M5GFX/GC9A01, encoder,
BtnA push), `knolleary/PubSubClient @ ^2.8`, `bblanchon/ArduinoJson @ ^7.0`,
`ArduinoOTA` (wireless OTA from the first image; only the very first flash
is USB — see `deploy.sh`). Face rendered into a full-frame 240×240 RGB565
sprite (~115 KB) and pushed only when changed. See the component `CLAUDE.md`
for build and deploy commands.