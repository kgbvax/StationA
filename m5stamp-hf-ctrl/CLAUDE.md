# CLAUDE.md — m5stamp-hf-ctrl

`m5stamp-hf-ctrl` is the firmware for **M5 Stamp PLC #1** (StamPLC K141: ESP32-S3
+ AW9523B I²C expander @0x59, 4 relays + 8 digital inputs). It is a **compound
embedded node**: one physical PLC publishes **two** station integration-model
slots that share the same `device{model,serial}` (the compound-device
relationship, model §3):

| Slot | Role | Relays | What it does |
|------|------|--------|--------------|
| `muehle/hf/switch` | `switch` | 3 (PA remote-on), 4 (TRX remote-on) | asserts the PA / TRX "remote-on" control lines; read-write |
| `muehle/hf/pa-arm` | `pa-arm` | 1 (arm) | the PA-enable arm relay, **fail-safe-open**, with the arm logic embedded |

Relay 2 is spare. The PLC is the model's planned embedded safety node (§6,
§11.3, now realized here): it subscribes `hf/radio/state` and `hf/ant-switch/state`
and computes

```
armed = enabled ∧ radio_online ∧ ¬radio.tuning ∧ band_safe ∧ heartbeat ∧ antenna_ready
```

driving relay 1 so that **any** failure drops the relay open → PA disabled.

This is embedded firmware (Arduino C++ / PlatformIO), not a Go service on
shari. It therefore uses its **own gitignored secrets file** (`src/secrets.h`,
copied from `src/secrets.example.h`) — **not** the systemd EnvironmentFile
convention, which is scoped to the Go services (see
`../docs/conventions/config-and-secrets.md`).

---

## Commands

```bash
pio run                       # build firmware (resolves lib_deps via PlatformIO)
pio run -t upload             # build + flash over USB (default env)
pio run -t upload -e m5stamp-plc1-ota            # build + flash over-the-air
pio device monitor            # serial monitor (115200)
./deploy.sh usb [port]        # build + flash over USB, auto-detects port
./deploy.sh ota [host]        # build + flash over-the-air (default host: m5stamp-plc-1.local)
```

First-time: copy the secrets template and fill it in:

```bash
cp src/secrets.example.h src/secrets.h
$EDITOR src/secrets.h     # WIFI_SSID, WIFI_PASSWORD, MQTT_*, OTA_PASSWORD, DEVICE_*
```

`src/secrets.h` is gitignored; the build includes it via `#include "secrets.h"`
in `main.cpp`.

> **No Go toolchain here.** This project has no `go.mod` and no tests. It is
> flashed to the PLC over USB/OTA from a workstation with PlatformIO installed;
> the PLC then runs standalone on the station WiFi. The first flash after adding
> OTA support must be USB, because the previously running firmware has no OTA
> listener.

---

## Architecture

**Data flow:** MQTT bus → PubSubClient → main.cpp (state + arm logic) → relay
HAL (M5StamPLC) → relays; and relays read back → main.cpp → MQTT.

1. `src/main.cpp` — WiFi connect, the two slot state machines, the embedded arm
   logic, and the `/meta` + `/state` publishers for both slots.
2. `src/mqtt_slot.h` — `SlotMqtt`: one MQTT connection per slot, each with its
   own **LWT** on its own `<slot>/status`. Two slots ⇒ two connections, so a PLC
   crash fires **both** slots' wills at once (no stale-online gap) — the embedded
   analog of the Go compound bridge's one-client-per-slot decision (MQTT 3.1.1
   allows one Will per client).
3. `src/relay.h` — thin relay HAL over the `M5StamPLC` library; the single
   integration seam against the hardware library.
4. `src/config.h` — non-secret compile-time config (slot addresses, relay map,
   band allow-list, heartbeat window, MQTT buffer sizes).
5. `src/secrets.h` (gitignored) / `src/secrets.example.h` — WiFi + MQTT
   credentials + device identity.

**Arm logic** (`recomputeArm`): runs every `loop()` iteration. `armed` is
recomputed from `enabled ∧ radio_online ∧ ¬tuning ∧ bandSafe ∧ heartbeat ∧
antenna_ready` and drives relay 1 (energize to arm, de-energize/open on any
drop). The radio inputs (`device_online`, `tuning`, `band`) come from
`hf/radio/state` and `antenna_ready` from `hf/ant-switch/state` (`selected` ≠
`off`), both subscribed on the pa-arm slot's connection (dispatched by topic in
`handlePaArmCmd`). Heartbeat: if no `/state` arrives within
`RADIO_HEARTBEAT_MS` (10 s) the arm drops. `band_safe` checks `band` against the
ACOM 1200S band set (160m..6m). `antenna_ready` drops the arm when the antenna
switch is in the grounded `off` position (walk-away lightning protection).

**Fail-safe-open:** relay 1 is energized ONLY to arm. Power loss, PLC crash, or
any safety failure de-energizes it → open → PA disabled. On cold boot
`relayInit()` opens all relays.

**Concurrency:** single-threaded Arduino `loop()`. Both `SlotMqtt` instances are
driven sequentially each iteration; PubSubClient callbacks run synchronously
within `client_.loop()`, so no locking is needed. State mutation from callbacks
is safe-by-construction (no preemption).

---

## MQTT topics

Per slot:

```
muehle/hf/switch/meta     retained  birth certificate (capabilities + expose)
muehle/hf/switch/state    retained  { pa, trx, device_online, ts }
muehle/hf/switch/status   retained  online | offline (LWT — this PLC, this slot)
muehle/hf/switch/cmd      retained   set_pa | set_trx intent (self-healing §8)

muehle/hf/pa-arm/meta     retained  birth certificate (capabilities + expose)
muehle/hf/pa-arm/state    retained  { enabled, armed, device_online, ts, error? }
muehle/hf/pa-arm/status   retained  online | offline (LWT — this PLC, this slot)
muehle/hf/pa-arm/cmd      retained   set_enabled intent (self-healing §8)
```

`/cmd` is **retained** for both slots (self-healing steady-state, model §8): on
reconnect the broker replays the last command and the firmware re-applies it.
The arm relay is **never** commanded directly — `armed` is derived. See
`docs/m5stamp-hf-ctrl-mqtt-api.md` for the full on-the-wire contract.

The firmware also **subscribes** `muehle/hf/radio/state` and
`muehle/hf/ant-switch/state` (on the pa-arm connection) for the arm-logic
inputs; it does not publish there.

---

## Configuration and secrets

Non-secret config is compile-time in `src/config.h` (slot addresses, relay
map, band allow-list, heartbeat window). Secrets live in `src/secrets.h`
(gitignored):

```cpp
// src/secrets.h
#define WIFI_SSID     "..."
#define WIFI_PASSWORD "..."
#define MQTT_HOST     "192.168.1.139"
#define MQTT_PORT     1883
#define MQTT_USER     "hf"
#define MQTT_PASSWORD "..."
#define DEVICE_MODEL  "M5Stamp PLC"
#define DEVICE_SERIAL "m5stamp-plc-1"
```

This is the **embedded-firmware secrets pattern** (like the ant-switch
ESPHome `secrets.yaml`), distinct from the Go services' EnvironmentFile. See
`../docs/conventions/config-and-secrets.md`.

---

## Station model and shared conventions

Shared documentation lives in `../docs/` (this component is a subdirectory of the stationa monorepo).

| Document | Path |
|---|---|
| Station integration model (§3 compound device, §4 `switch`/`pa-arm` roles, §6 arm safety, §7.1 slot blocks) | `../docs/station-integration-model.md` |
| Config and secrets convention (embedded-firmware pattern) | `../docs/conventions/config-and-secrets.md` |
| Bridge-naming convention | `../docs/conventions/naming.md` |

This firmware implements the `muehle/hf/switch` + `muehle/hf/pa-arm` slots and
must conform to those conventions. Component-specific schema lives in
`docs/m5stamp-hf-ctrl-mqtt-api.md`.