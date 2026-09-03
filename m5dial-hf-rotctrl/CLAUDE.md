# CLAUDE.md — m5dial-hf-rotctrl

`m5dial-hf-rotctrl` is the firmware for an **M5Stack Dial** (ESP32-S3FN8,
1.28" round GC9A01 240×240 display, 16-detent rotary encoder with push
button, FT3267 touch, buzzer, WiFi) mounted in the shack as a **physical
analog control head for the HF rotator**:

- the **face** is a 1970s-style needle meter: a fixed **0…360° compass card**
  (N at 12 o'clock, 1 screen-degree per compass-degree), a **damped** black
  needle (~0.55 s time constant — sweeps like a physical meter movement), a
  thin red **target pointer**, and a liveness ring/badge. Azimuth **wraps on
  the display** (az 390 renders at the 30° mark, same as the real sky); the
  command space stays linear 0…450° (the G-450DC's real range incl. overlap),
  and a small **+360 hint** on the plaque marks when the antenna rides the
  360…450 overlap pass;
- the **knob** is the rotator: every detent publishes a new target azimuth to
  `muehle/hf/rotator/cmd` (22.5°/detent — one knob-flat, one compass step) and the
  needle chases your hand; pressing the knob publishes `stop`.

It is a **consumer + `/cmd` stimulator, NOT a slot** (integration model §9 — the same class as `testui` and `hf_console`).
It publishes **no** `/meta`, `/state`, `/status` of any address, has **no LWT
and no heartbeat** — no MQTT presence of its own. Its entire bus footprint is
two subscriptions (`muehle/hf/rotator/state` + `/status`) and two publish
shapes on `muehle/hf/rotator/cmd`. Delete it and the station runs identically.

This is embedded firmware (Arduino C++ / PlatformIO), not a Go service on
shari. It therefore uses its **own gitignored secrets file** (`src/secrets.h`,
copied from `src/secrets.example.h.x`) — **not** the systemd EnvironmentFile
convention, which is scoped to the Go services (see
`../docs/conventions/config-and-secrets.md`).

---

## Commands

```bash
pio run                       # build firmware (default env = USB)
pio run -e m5dial-rotctrl1-ota -t upload    # build + flash over-the-air
pio run -e m5dial-rotctrl1-sim -t upload    # build + flash the desk-sim image
pio device monitor            # serial monitor (115200)
./deploy.sh                   # build + flash OTA (default; host m5dial-rotctrl-1.local)
./deploy.sh ota [host]        # build + flash OTA, explicit host
./deploy.sh usb [port]        # first flash / recovery only, auto-detects port
./deploy.sh sim [port]        # desk-simulation image (USB only, auto-detects port)
```

First-time setup: copy the secrets template and fill it in:

```bash
cp src/secrets.example.h.x src/secrets.h
$EDITOR src/secrets.h     # WIFI_SSID, WIFI_PASSWORD, MQTT_* (user "dial"), OTA_PASSWORD, DEVICE_*
```

`src/secrets.h` is gitignored; the build includes it via `#include "secrets.h"`
in `main.cpp` (and fails fast if it is missing).

> **No Go toolchain here.** This project has no `go.mod` and no tests. It is
> flashed to the Dial from a workstation with PlatformIO installed; the Dial
> then runs standalone on the station WiFi.
>
> **Wireless OTA is in the first image.** ArduinoOTA is wired from the very
> first build, so only the literal first flash (or a recovery) needs USB —
> every routine update goes over the network via `./deploy.sh` (no-arg
> defaults to OTA). The m5stamp PLC in the field runs pre-OTA firmware and
> its next update needs physical USB; this project exists so the Dial can
> never drift into that state — with one deliberate exception: a
> **sim-flashed Dial** has no radio and needs `./deploy.sh usb` to return to
> the live image (see the desk-simulation section below).
>
> **OTA publishes `stop` first.** An update start publishes
> `{"action":"stop"}` best-effort: the antenna keeps moving through a Dial
> reboot otherwise (motion runs in the bridge/WRC, not in this head), and the
> operator would be blind while the head is down. If MQTT is down at update
> start no stop goes out — do not update during motion with the link down.

---

## Desk-simulation build (`m5dial-rotctrl1-sim`)

`./deploy.sh sim` (USB flash only) builds with `SIM_MODE=1`: the station side
of the wire is replaced by `src/sim.h`, a local mast model. **No WiFi, no
MQTT, no OTA** — the face, knob, coalescer and stop re-baseline logic run the
**production code** against the model, so bench development needs no antenna,
broker or network at all.

The model is protocol-faithful, not just plausible: it takes `set_az`/`stop`
the way the `wrc-rotator-bridge` does (target clamped to 0…450; a stop halts
±1° past the measured azimuth with the cancelled target still reported — the
coast the press re-baseline path exists for) and emits the same `/state` JSON
through the same parser (`applyRotatorState`), with the same change-dedup
discipline (a parked sim is silent, like a parked rotator). It slews at the
real G-450DC rate (~5.7°/s — a 180° turn takes ~32 s; the patience is part of
the test). The face shows a small **SIM** label so a desk face is never
mistaken for the live antenna.

Because the sim image has no radio, USB is the only way in **and** the only
way back: `./deploy.sh usb` returns to the live image.

---

## Architecture

**Data flow:** knob → local target (coalescer) → `set_az` on the rotator's
`/cmd` → the rotator moves → `/state` → damped needle. The knob is a
*relative* input (last target ± Δ per detent), so the boot position of the
encoder never causes a jump, and the target is initialized from the first
`/state` (`target_az` else `az`).

1. `src/main.cpp` — WiFi, MQTT wiring, the knob/publish/face state machines,
   OTA. Loop order matters: input is read first (works with WiFi/MQTT down),
   then coalesce-publish, WiFi gate, `mqtt.loop()`, `ArduinoOTA.handle()`,
   face render.
2. `src/mqtt_client.h` — `DialMqtt`: the non-slot adaptation of
   m5stamp-hf-ctrl's `SlotMqtt`. Keeps buffer-before-connect, keepalive,
   loop-driven reconnect; **removes all slot shape — no will, no status
   publish**. Adds a reconnect throttle so the needle keeps rendering while
   the broker is down.
3. `src/face.h` — the analog meter face. One full-frame 240×240 RGB565 sprite
   (~115 KB, allocated once; direct-draw fallback if allocation fails),
   every layer redrawn per repaint, pushed only when changed.
4. `src/knob.h` — the encoder as whole detents (4 counts/detent, remainder
   carried so detent-boundary wobble is absorbed) and press discrimination
   (a press event on release, any hold duration — press = stop, no modes).
   **Knob counting is edge-driven by our own IRAM ISR quadrature on GPIO
   40/41, NOT `M5Dial.Encoder`**: the M5Dial library's PJRC encoder class
   has a static ESP32 interrupt table that stops at GPIO 39, so on this
   board it silently attaches no interrupt and *polls* instead — and a
   polled state machine loses whole detents whenever the face animates (a
   ~25 ms full-frame SPI push spans a full detent's worth of quadrature
   states). That was the "missed dial change events" bug. `[knob] raw=…`
   on the serial monitor is the verification line: one knob-flat must show
   4 raw counts, every flat.
5. `src/config.h` — non-secret compile-time config (topics, tuning constants).
6. `src/sim.h` — the SIM_MODE local mast model (see the desk-simulation
   section below; compiles to nothing in the live builds).
7. `src/secrets.h` (gitignored) / `src/secrets.example.h.x` — WiFi + MQTT
   credentials + device identity.

**Publishing discipline** (the two rules that must never regress):

- **Coalesce**: the bridge's `/cmd` queue is 8 deep and silently drops on
  overflow. First detent immediately; while turning ≤ one publish per 200 ms;
  final flush 400 ms after the last detent. `stop` flushes a pending target
  first so it never lands behind our own queued move.
- **Never replay**: a pending target is dropped on MQTT disconnect, never
  republished on reconnect. `/cmd` is not retained; a stale replay can spin
  the antenna. The Dial publishes only on fresh operator input or stop.
- **Stop re-baselines from the halt state**: press = stop sets a provisional
  target from the last-reported `az` (the antenna coasts past it); the halt
  `/state` (`moving:false`) carries the true stop point and re-baselines the
  target, bypassing the 2 s turning-guard — otherwise the change-deduped
  `/state` would go silent before any correction could land.

**Liveness (two layers, ANDed):** `/status` = the bridge process (LWT);
`/state.device_online` = the WRC websocket link. The face ANDs them
(green / amber = link down / red = bridge or MQTT down / gray = no data).
Liveness knowledge is **dropped on the MQTT disconnect edge** — the face
shows NO DATA until the resubscribe's retained `/status` + `/state` replay
re-establishes it, so a reconnect never trades on stale pre-disconnect flags.
While liveness is down, **detents are ignored** (queuing unobservable motion
commands while blind is worse than ignoring input); press = `stop` still
publishes whenever MQTT is connected. `/state` is change-deduped upstream —
**a parked rotator is silent by design; silence is never staleness**.

**Concurrency:** single-threaded Arduino `loop()`; PubSubClient callbacks run
synchronously inside `client_.loop()`, and nothing publishes from inside a
callback (fire-and-observe only).

---

## MQTT topics

```
# subscribes (the entire inbound surface)
muehle/hf/rotator/state    retained   { az, target_az?, moving, device_online }
muehle/hf/rotator/status   retained   online | offline (bridge LWT)

# publishes (the entire outbound surface — NOT retained)
muehle/hf/rotator/cmd      -          {"action":"set_az","az":180}
                                      {"action":"stop"}
```

The rotator's command argument key is **`az`, not `value`** — a pre-convention
deviation declared in the slot's `/meta.expose` (`value_key:"az"`). Do not
"fix" it. Publishes go out at QoS 0 (PubSubClient cannot publish higher;
known deviation — see the API doc). Full wire contract:
`docs/m5dial-hf-rotctrl-mqtt-api.md`.

---

## Configuration and secrets

Non-secret config is compile-time in `src/config.h` (topics, detent step,
publish coalescing constants, needle damping, beep constants). Secrets live
in `src/secrets.h` (gitignored):

```cpp
// src/secrets.h
#define WIFI_SSID     "..."
#define WIFI_PASSWORD "..."
#define MQTT_HOST     "192.168.1.139"   // shack broker, LAN address (never 127.0.0.1)
#define MQTT_PORT     1883
#define MQTT_USER     "dial"            // narrow account, NOT the broad "hf" account
#define MQTT_PASSWORD "..."
#define OTA_PASSWORD  "..."
#define DEVICE_MODEL  "M5Stack Dial"
#define DEVICE_SERIAL "m5dial-rotctrl-1"  // → m5dial-rotctrl-1.local for OTA
```

This is the **embedded-firmware secrets pattern** (like the ant-switch ESPHome
`secrets.yaml`), distinct from the Go services' EnvironmentFile. The `dial`
broker account must be added on shari (seed-once broker — hand-edit
`/etc/mosquitto/acl.conf` + `mosquitto_passwd`, restart mosquitto; see
`../mqtt-broker/README.md`).

---

## Station model and shared conventions

Shared documentation lives in `../docs/` (this component is a subdirectory of
the stationa monorepo).

| Document | Path |
|----------|------|
| Station integration model (§9 consumer invariants, §7.1 rotator slot) | `../docs/station-integration-model.md` |
| MQTT broker topology (accounts, ACLs) | `../docs/conventions/mqtt-topology.md` |
| Config and secrets convention (embedded-firmware pattern) | `../docs/conventions/config-and-secrets.md` |
| Bridge-naming convention (devtag `m5dial`) | `../docs/conventions/naming.md` |
| Design + open-decision notes | `../docs/design-notes.md` (salvaged from the PRD, 2026-09-03) |

The wire contract this firmware conforms to is owned by
`../wrc-rotator-bridge/docs/wrc-rotator-bridge-mqtt-api.md`; the Dial-specific
view (including the verification recipe) is
`docs/m5dial-hf-rotctrl-mqtt-api.md`.