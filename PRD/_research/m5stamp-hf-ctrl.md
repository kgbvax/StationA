# Research spec — m5stamp-hf-ctrl (M5 Stamp PLC #1 firmware)

Source of truth: the code under `/Users/ingomar.otter/dev/stationa/m5stamp-hf-ctrl/src/`
(`main.cpp`, `config.h`, `mqtt_slot.h`, `relay.h`, `secrets.example.h`), plus
`platformio.ini`, `deploy.sh`, and `docs/m5stamp-hf-ctrl-mqtt-api.md`. Where a
document disagrees with the code, the code wins and the disagreement is noted.

Audience: an engineer who knows nothing about amateur radio, reconstructing this
system's behavior in a completely different technology stack.

Glossary (defined at first use, collected here for reference):

- **Ham radio / HF**: amateur ("ham") radio; HF (high frequency) means the
  shortwave bands roughly 3–30 MHz used for long-distance contacts.
- **TRX**: transceiver — the radio itself (here a FLEX-8400).
- **PA**: power amplifier — a device that boosts the transceiver's transmit
  signal (here an ACOM 1200S).
- **Remote-on line**: a control wire on the TRX/PA that switches the device on
  remotely (as opposed to its front-panel power button).
- **Arm relay**: a relay whose closed contacts form the final "permission"
  circuit that enables the PA to transmit. "Armed" = relay energized/closed;
  "open" = de-energized → PA cannot transmit.
- **Fail-safe-open**: the safe state is the relay being DE-ENERGIZED (open).
  Anything going wrong (power loss, crash, watchdog) removes coil current →
  relay opens → PA disabled. The dangerous state requires active, continuously
  maintained energy.
- **ATU / tuning**: antenna tuner; "tuning" is an automatic tune cycle where
  the radio transmits a carrier into a mismatched load — unsafe while the PA
  could amplify it.
- **Band**: a named frequency allocation, e.g. "20m" (the 14 MHz ham band).
  The ACOM 1200S PA covers 160 m through 6 m.
- **MQTT**: a lightweight publish/subscribe protocol. Messages are published
  to hierarchical string **topics**; a broker distributes them to subscribers.
  **Retained** messages are kept by the broker and delivered to every future
  subscriber of the topic. **LWT** (last will and testament) is a message the
  broker publishes on a topic if the client connection dies without a clean
  disconnect.
- **Slot**: this station's unit of management — one device's MQTT presence,
  addressed as `<site>/<station>/<slot>` with the suffixes `/meta`, `/state`,
  `/status`, `/cmd` (the "three-plane + command" convention of the station
  integration model).
- **Compound device**: one physical device publishing more than one slot; the
  slots share the same `device{model,serial}` in `/meta`, which is how
  consumers know they are the same box.
- **PLC**: programmable logic controller. The M5 Stamp PLC (StamPLC K141) is an
  ESP32-S3 microcontroller board with an AW9523B I2C GPIO expander (address
  0x59) providing 4 relay outputs and 8 digital inputs, a 240x135-pixel color
  LCD, and three front-panel buttons A (left), B (middle), C (right).
- **Grounded antenna position**: the antenna switch has an "off" position that
  connects the antenna feed to ground for lightning protection; transmitting
  into it is destructive, so the arm must drop.
- **OTA**: over-the-air — flashing new firmware over WiFi instead of USB.

---

## 1. Purpose & role

`m5stamp-hf-ctrl` is Arduino C++ / PlatformIO firmware for **M5 Stamp PLC #1**,
a small mains/13.8 V-powered embedded box mounted at the station. It is a
**compound embedded node**: one physical PLC publishes **two** MQTT slots that
share the same `device{model,serial}`:

| Slot | Role | PLC relay (1-based) | Library channel (0-based) | Function |
|---|---|---|---|---|
| `muehle/hf/switch` | `switch` | relay 3, relay 4 | channel 2, channel 3 | Asserts the PA and TRX "remote-on" control lines. Read-write, operator-controllable. |
| `muehle/hf/pa-arm` | `pa-arm` | relay 1 | channel 0 | The PA-enable **arm relay**, fail-safe-open, with the arm (safety) logic embedded in this firmware. |
| (spare) | — | relay 2 | channel 1 | Unused. |

Relay map constants (`src/config.h`): `RELAY_PA_ARM 0`, `RELAY_SPARE 1`,
`RELAY_PA_REMOTE 2`, `RELAY_TRX_REMOTE 3`.

The PLC is the station's **embedded safety node**. It subscribes to the radio's
slot (`muehle/hf/radio/state`, published by `flexbridge`) and the antenna
switch's slot (`muehle/hf/ant-switch/state`, published by the ESPHome
ant-switch bridge) and continuously computes:

```
armed = enabled ∧ radio_online ∧ ¬radio.tuning ∧ band_safe ∧ heartbeat_fresh ∧ antenna_ready
```

It drives relay 1 so that **any** failure (operator permit withdrawn, radio
offline, radio tuning, unsafe/unknown band, radio state feed stale > 10 s,
antenna switch in the grounded "off" position, loss of the 13.8 V supply, PLC
crash) de-energizes the relay → open → PA disabled.

The station's slot table also assigns `muehle/uhf/pol-ctrl` (X-Quad UHF
antenna polarization, "PLC #2") to this project — **but the repository contains
no PLC #2 firmware**: no second PlatformIO environment, no pol-ctrl code, no
uhf topics anywhere in `src/`. Only the two HF slots above are implemented.
Treat "PLC #2 / pol-ctrl" as NOT covered by this codebase (see §9, §10).

The firmware is flashed to the PLC from a workstation (USB serial first flash,
OTA afterwards) and then runs standalone on the station WiFi. It is not a
systemd service and not deployed to the shari host.

---

## 2. Upstream interface

There is no serial/USB device upstream. The PLC's "device" is its own relay
hardware, driven through the official `M5StamPLC` Arduino library (version
`^1.0`):

- Transport to the world: WiFi (station network, SSID/password compile-time in
  the gitignored `src/secrets.h`), then plain TCP MQTT 3.1.1 via PubSubClient
  (`^2.8`) to the broker configured in secrets. In the deployed instance the
  broker is the **shack-local broker on shari at 192.168.1.139:1883** (comment
  in the checked-in-but-gitignored `src/secrets.h` and `docs/…-mqtt-api.md` §1
  both say 192.168.1.139; the root-repo CLAUDE.md and `secrets.example.h` still
  say 192.168.1.50 — the pre-two-broker address; see §9).
- Hardware access (`src/relay.h`, the single integration seam):
  - `M5StamPLC.begin()` — initializes the AW9523B expander and relays.
  - `M5StamPLC.writePlcRelay(channel 0..3, bool)` — energize/de-energize.
  - `M5StamPLC.readPlcRelay(channel 0..3)` — read back actual relay position.
  - `M5StamPLC.readPlcInput(channel 0..7)` — 8 digital inputs (declared,
    currently unused).
  - Front panel: LCD 240x135 (rendered via M5Unified `M5StamPLC.Display`),
    buttons A/B/C via `M5StamPLC.update()` + `BtnB.wasPressed()` / `BtnC.wasPressed()`.
- No serial console protocol is used at runtime; `monitor_speed 115200` exists
  for development and exception decoding only. `CORE_DEBUG_LEVEL=0`.

---

## 3. MQTT presence

### 3.1 Connections

- Protocol: MQTT 3.1.1 over plain TCP (no TLS), username `hf`, password from
  secrets. **Two independent client connections**, one per slot, because MQTT
  3.1.1 allows only one Will per client and each slot needs its own `/status`
  LWT — so a PLC crash fires BOTH slots' wills simultaneously, with no
  stale-online gap.
- Client IDs: `muehle-switch` and `muehle-pa-arm` (format: `<SITE>-<suffix>`).
- Keep-alive: 30 s (`MQTT_KEEPALIVE_S 30`). Client RX/TX buffer: 1024 bytes
  (`MQTT_BUFFER_SIZE 1024`, set because the retained `/meta` with its `expose`
  block exceeds PubSubClient's 256-byte default).
- LWT per slot: topic `<slot>/status`, QoS 1, **retained**, payload `offline`.
  On every (re)connect the client publishes retained `online` to the same
  topic. `/status` thus means "liveness of this PLC for this slot".
- Session: the code calls PubSubClient's 7-argument `connect(id, user, pass,
  willTopic, willQoS, willRetain, willMessage)`, which internally defaults to
  **cleanSession = true**. The doc file and header comments claim "Clean
  session false" — that claim is wrong (see §9). It is behaviorally moot
  because self-heal works via *retained* messages, not session state.

### 3.2 Published topics (all payloads JSON, all retained, publish QoS 0)

Slot `muehle/hf/switch`:

- `muehle/hf/switch/meta` — retained birth certificate, published once per
  MQTT (re)connect (connect-edge), never on a timer (`META_REFRESH_MS` is
  defined but unused — see §9). Payload:
  ```json
  {"schema":"1.0","role":"switch","device":{"model":"M5Stamp PLC","serial":"m5stamp-plc-1"},"link":"wifi","host":"embedded","capabilities":{"channels":["pa","trx"],"exclusive":false,"kind":{"pa":"remote_on","trx":"remote_on"},"relay_map":{"pa":3,"trx":4}},"expose":{"device":{"name":"M5Stamp PLC","model":"M5Stamp PLC","manufacturer":"M5Stack"},"fields":[{"key":"pa","name":"PA remote-on","type":"enum","options":["on","off"],"writable":true,"command":{"action":"set_pa","value_key":"value","value_type":"string"}},{"key":"trx","name":"TRX remote-on","type":"enum","options":["on","off"],"writable":true,"command":{"action":"set_trx","value_key":"value","value_type":"string"}}]}}
  ```
  (`device.model`/`device.serial` come from secrets: `DEVICE_MODEL`
  `"M5Stamp PLC"`, `DEVICE_SERIAL` `"m5stamp-plc-1"` — identical in both
  slots' `/meta`; that shared identity IS the compound-device relationship.)
- `muehle/hf/switch/state` — retained, published on (a) MQTT connect, (b)
  immediately after any `/cmd` application, (c) immediately after any
  front-panel button toggle. **No periodic republish** — on a quiet bus the
  retained snapshot simply ages. Payload:
  ```json
  {"ts":123456,"pa":"on","trx":"off","device_online":true}
  ```
  - `ts`: number — PLC uptime in **milliseconds** (`millis()`; no real-time
    clock; it is a monotonic freshness marker, not wall-clock time).
  - `pa`, `trx`: string `"on"`/`"off"` — the **actual relay positions read
    back** from relays 3 and 4, not the last commanded value.
  - `device_online`: boolean, **hardcoded `true`** — meaningful only while the
    PLC is publishing (if it can publish, it is up).
- `muehle/hf/switch/cmd` — retained, **published by the PLC itself** when a
  front-panel button toggles a relay (see §4.2), payload
  `{"action":"set_pa","value":"on"}` / `{"action":"set_trx","value":"off"}`.
  The PLC both subscribes to and publishes this topic; publishing its own
  toggle keeps the retained intent consistent, and the echoed message is
  harmlessly re-applied on arrival (idempotent).

Slot `muehle/hf/pa-arm`:

- `muehle/hf/pa-arm/meta` — retained, once per (re)connect. Payload:
  ```json
  {"schema":"1.0","role":"pa-arm","device":{"model":"M5Stamp PLC","serial":"m5stamp-plc-1"},"link":"wifi","host":"embedded","capabilities":{"fail_safe":"open","heartbeat":true,"relay":1},"expose":{"device":{"name":"M5Stamp PLC","model":"M5Stamp PLC","manufacturer":"M5Stack"},"fields":[{"key":"armed","name":"Armed","type":"boolean","writable":false},{"key":"enabled","name":"Enabled (arm permit)","type":"boolean","writable":true,"command":{"action":"set_enabled","value_key":"value","value_type":"boolean"}}]}}
  ```
- `muehle/hf/pa-arm/state` — retained. Publish cadence: **immediately whenever
  `enabled`, `armed`, or the blocking `error` string changes**, otherwise at
  least every **10 000 ms** (`PA_ARM_HEARTBEAT_MS 10000`) while connected — a
  dedup-suppressed heartbeat so a quiet bus is not flooded with identical
  snapshots (the last-published values are tracked in
  `lastPublishedEnabled/Armed/Error`). This ~10 s heartbeat is what
  downstream consumers (sequencer, antenna-selection logic, the operator
  console) rely on to judge slot freshness. Payload:
  ```json
  {"ts":123456,"enabled":true,"armed":false,"device_online":true,"error":"radio tuning"}
  ```
  - `ts`: uptime ms (as above).
  - `enabled`: boolean — the operator/sequencer arm permit (last `set_enabled`).
  - `armed`: boolean — **derived** actual state of relay 1; never commanded.
  - `device_online`: boolean, hardcoded `true`.
  - `error`: string, **omitted when empty**. Produced by this exact function,
    evaluated in this precedence order (first match wins), INDEPENDENT of
    `enabled`:
    1. `"radio offline"` — last received `hf/radio/state` had `device_online`
       false (or none ever received);
    2. `"radio tuning"` — last received `tuning` true;
    3. `"band not safe"` — last received `band` not in the allow-list (or
       empty/never received);
    4. `"antenna grounded"` — last received `ant-switch/state.selected` is
       `"off"` or missing/empty.
  - Consequence: if `enabled=false` but a safety input is also bad, `error` IS
    published (contrary to the doc file's claim — see §9). And a **heartbeat
    timeout alone produces NO error string**: the error function does not
    check heartbeat freshness, so the arm can drop from `armed:true` to
    `armed:false` with `error` absent (silent drop). A reimplementation must
    reproduce this exactly or must (better) be explicitly specified otherwise
    — see §10.

### 3.3 Subscribed topics

On the **switch** connection:

- `muehle/hf/switch/cmd` — re-subscribed on every (re)connect; retained
  command replayed by the broker = self-healing steady state. QoS 0 subscribe.

On the **pa-arm** connection:

- `muehle/hf/pa-arm/cmd` — re-subscribed on every (re)connect (self-heal of
  `enabled`).
- `muehle/hf/radio/state` — absolute-topic subscription, re-subscribed on every
  (re)connect. The PLC never publishes here.
- `muehle/hf/ant-switch/state` — absolute-topic subscription, same handling.

All three arrive at one PubSubClient callback and are dispatched by topic
suffix in `handlePaArmCmd`: topic ending `/radio/state` → radio inputs;
`/ant-switch/state` → antenna input; otherwise treated as `/cmd`.

### 3.4 Inputs consumed from other slots (exact fields)

From `muehle/hf/radio/state` (published by flexbridge):

- `device_online` (bool, default `false` if absent) → `radioOnline`
- `tuning` (bool, default `false`) → `radioTuning`
- `band` (string, e.g. `"20m"`; absent → `""`) → `radioBand`
- Receiving ANY parseable `hf/radio/state` message, whatever its content,
  refreshes the heartbeat clock `lastRadioStateMs = millis()`. Unparseable
  JSON is silently dropped and does NOT refresh the clock.

From `muehle/hf/ant-switch/state`:

- `selected` (string; absent → `""`) → `antennaReady = (selected != "" &&
  selected != "off")`. Unknown/missing is treated conservatively as
  not-ready. Note: this input has **no staleness window** — a stale
  `antenna_ready` stays ready forever until a new message arrives.

---

## 4. Command surface

All `/cmd` payloads are JSON: `{"action":"<word>", "value": "<string>"}` — the
station convention that arguments ride under the key `value`. The firmware
parses with ArduinoJson: `action = doc["action"] | ""`, `value = doc["value"] | ""`.
**`value` must be a JSON string**; a JSON boolean (`"value": true`) does not
survive the `| ""` string extraction and yields `""` (see §9 fragility).

### 4.1 `muehle/hf/switch/cmd`

- `{"action":"set_pa","value":"on"}` — set relay 3 (PA remote-on) energized.
- `{"action":"set_pa","value":"off"}` — de-energize relay 3. Any `value` other
  than exactly `"on"` results in off.
- `{"action":"set_trx","value":"on"/"off"}` — same for relay 4 (TRX remote-on).
- Unknown action or unparseable JSON: silently dropped, no reply, no error.
- Side effects of a recognized command: drive the relay, read both relays back,
  publish a fresh retained `switch/state` immediately (if connected). The
  retained `/cmd` itself is whatever the sender published (the PLC does not
  echo commands received over MQTT back to `/cmd`; only button presses are
  echoed).

### 4.2 Front-panel buttons (local control path)

- Button **B** toggles PA remote-on; button **C** toggles TRX remote-on.
  Button A is unused. Debounce: at most one button action per 150 ms
  (`UI_BTN_DEBOUNCE_MS 150`).
- A button press: flips the in-RAM state, drives the relay, then publishes the
  intent to its own retained `switch/cmd` (`{"action":"set_pa","value":"on"}`,
  etc.) and a fresh `switch/state`. If MQTT is disconnected, the relay is still
  toggled but the `/cmd` echo is skipped (`publishSwitchCmd` returns early) —
  the retained intent on the broker is then STALE and, on reconnect, the
  replayed retained `/cmd` **overwrites the local change** (revert to the
  pre-outage intent). Buttons work even with WiFi down (they are handled
  before the WiFi check in the loop).
- The arm relay has **no** local override — no button can force it.

### 4.3 `muehle/hf/pa-arm/cmd`

- `{"action":"set_enabled","value":"true"}` — set the software arm permit to
  true. `value` must be exactly the string `"true"`; anything else (including
  `"false"`, `""`, a JSON boolean) sets the permit to **false**.
- There is **no** arm/force command. `armed` is never commanded; it is only
  derived. The permit is one AND-input of six.
- Side effect: only the in-RAM `enabled` flag; `armed` is recomputed on the
  next main-loop iteration (≤ ~50 ms later), which may change relay 1 and
  publishes a fresh `pa-arm/state` if anything changed.
- Unknown action or unparseable JSON: silently dropped.

---

## 5. Behavior & state machine

### 5.1 Boot sequence (`setup()`)

1. `relayInit()`: `M5StamPLC.begin()`, then **all four relays written
   de-energized (open)**. This is the cold-boot fail-safe state: PA arm open,
   PA/TRX remote-on off. (Physically, before `begin()` completes the relays
   are also unpowered = open.)
2. `uiInit()`: backlight on, LCD rotation 1 (landscape), black screen.
3. `wifiConnect()`: `WiFi.mode(WIFI_STA)`, `WiFi.setSleep(false)` (reliability
   over power — mains-powered), `WiFi.begin(...)` — **non-blocking**.
4. `otaInit()`: ArduinoOTA hostname = `DEVICE_SERIAL` (`m5stamp-plc-1`),
   password = `OTA_PASSWORD` from secrets; `onStart` hook **de-energizes the
   PA arm relay before rebooting into an update**; `onError` logs only and the
   main loop keeps running. OTA is serviced every loop iteration once WiFi is
   up. Note: the switch relays are NOT dropped on OTA start.
5. RAM state: `enabled=false`, `armed=false`, `radioOnline=false`,
   `radioTuning=false`, `radioBand=""`, `lastRadioStateMs=0`,
   `antennaReady=false`, `switchState{pa=false,trx=false}`. Because
   `lastRadioStateMs==0` and `antennaReady=false`, the arm is held off until
   BOTH a radio state AND a non-off ant-switch state have been received — even
   if `enabled` were set by retained-command replay.

### 5.2 Main loop (`loop()`, iterated every 50 ms — `LOOP_DELAY_MS 50`, ~20 Hz)

1. `M5StamPLC.update()` (button debouncing) — always, even with WiFi down.
2. `handleButtons()` — local toggles (§4.2).
3. **WiFi gate**: if `WiFi.status() != WL_CONNECTED`, every 5000 ms call
   `WiFi.reconnect()`, repaint the LCD, delay 50 ms, and **return early**.
   Everything below is skipped during a WiFi outage — including the arm
   recomputation (see §9 for the safety consequence).
4. Drive both slot MQTT state machines (`switchSlot.loop()`, then
   `paArmSlot.loop()`), and `ArduinoOTA.handle()`.
   - `SlotMqtt::loop()`: if connected, service PubSubClient; if not, attempt
     `connect()` **immediately, every loop iteration** (~every 50 ms) — the
     header comment claims "reconnect with a backoff" but none is implemented
     (§9). On success: publish retained `online` to `<slot>/status`,
     subscribe `<slot>/cmd` (retained command replays → self-heal), invoke the
     connect-edge callback.
   - Connect-edge callback for switch: publish retained `meta` + `state`.
   - Connect-edge callback for pa-arm: publish retained `meta` + `state`, then
     re-subscribe `muehle/hf/radio/state` and `muehle/hf/ant-switch/state`.
     Connect-edge fires only on the rising edge (`connectedOnce` latch,
     cleared when disconnected).
5. **Arm recomputation** (every iteration, but only reached when WiFi is up):
   - `heartbeatFresh = (millis() - lastRadioStateMs) < 10000 && lastRadioStateMs != 0`
   - `armed = enabled && radioOnline && !radioTuning && bandSafe(radioBand) && heartbeatFresh && antennaReady`
     with `bandSafe(band) = band ∈ {"160m","80m","60m","40m","30m","20m","17m","15m","12m","10m","6m"}`
     (the ACOM 1200S PA's coverage; empty/unknown band is unsafe).
   - If `armed` changed, immediately drive relay 1 (`relaySet(RELAY_PA_ARM,
     armed)` — energize to arm, de-energize to drop).
   - Then decide whether to publish `pa-arm/state`: publish if `armed` changed,
     or `enabled`/`armed`/`error` differ from the last published snapshot, or
     ≥ 10 000 ms since the last publish. Skip publishing entirely when the
     pa-arm connection is down.
6. Repaint the LCD at most every 1000 ms (`UI_REFRESH_MS 1000`): header "TEST"
   (hardcoded leftover label), top-right connection word — green `MQTT` when
   both WiFi and the switch connection are up, green `WiFi` when only WiFi is
   up, red `----` when WiFi is down — and four square indicators (green when
   on, gray when off) reading the **actual relay positions**: ARM, PA, TRX, SP.
7. `delay(50)`.

### 5.3 Exact heartbeat behavior (the safety-critical part)

There are two distinct 10-second rhythms; do not conflate them:

- **Input heartbeat** (`RADIO_HEARTBEAT_MS 10000`): the arm logic requires a
  `muehle/hf/radio/state` message to have arrived within the last 10 s
  (`lastRadioStateMs != 0` guards the never-received case, which counts as
  stale). What happens when heartbeats stop: on the first loop iteration after
  `millis() - lastRadioStateMs ≥ 10000` — i.e. within at most ~10.05 s of the
  last radio message — `heartbeatFresh` goes false, `armed` is recomputed
  false, and **relay 1 is de-energized (opens) within that same loop pass**,
  dropping the PA enable. The published `pa-arm/state` then shows
  `armed:false` (published immediately as a change; with NO new `error` string,
  because the error function does not test heartbeat freshness — see §3.2).
  When heartbeats stop *because the pa-arm MQTT connection died* but WiFi
  stays up: the arm logic keeps running locally, the radio feed stops
  arriving, and the arm still drops within 10 s — safety holds. When
  heartbeats stop because **WiFi itself is down**: the loop's early return
  skips arm recomputation, so the arm relay **freezes in its last physical
  position for the duration of the outage**; on WiFi recovery the recomputation
  runs and (radio feed still stale) drops it within one loop pass. This
  WiFi-outage hold is arguably a contract violation of "any failure drops the
  relay" — see §9. When heartbeats stop because the **radio's own state is
  merely unchanging** (flexbridge publishes `/state` only on change): the arm
  still drops after 10 s even though the radio is healthy — this is a known
  live incident pattern (§9) and any reimplementation must decide whether the
  producer should heartbeat or the consumer window should grow.
- **Output heartbeat** (`PA_ARM_HEARTBEAT_MS 10000`): the dedup-suppressed
  periodic republish of retained `pa-arm/state` described in §3.2. Downstream
  consumers treat pa-arm as stale if its `/state` grows older than ~10 s (and
  as gone entirely when the LWT fires `status:offline`). Relays are NOT
  affected by the output heartbeat — it only refreshes the bus snapshot.

The **switch slot relays (3 and 4) are never touched by any heartbeat or
safety logic**. They change only by `/cmd` or by buttons; a PLC crash opens
them only because the coil driver dies with the CPU.

### 5.4 Reconnection behavior summary

- WiFi: checked every loop; when down, `WiFi.reconnect()` at most every 5000
  ms; loop early-returns (arm frozen, MQTT down, buttons still live, LCD shows
  `----`).
- Each MQTT client: reconnect attempt every loop (~20 Hz, no backoff) while
  WiFi is up; on reconnect: `status:online` retained, re-subscribe `/cmd`
  (retained command replays → re-applied), pa-arm additionally re-subscribes
  the two state feeds, both `/meta` re-published, both `/state` re-published
  (pa-arm's dedup tracking makes the first post-reconnect publish unconditional
  — connect-edge calls `publishPaArmState` directly).
- Broker-side outage with TCP half-open: PubSubClient keep-alive 30 s governs
  detection; during that window the arm logic still runs locally on stale
  inputs until the 10 s input heartbeat (fed only by live messages) drops the
  arm.

### 5.5 Error paths

- Unparseable `/cmd` JSON → dropped silently. Unparseable radio/ant-switch
  state → dropped silently, heartbeat NOT refreshed.
- `relayGet` readback: comment claims "returns false on any read failure
  (fail-safe: treat as open)" but the implementation is a bare
  `return M5StamPLC.readPlcRelay(ch);` with no error handling — the claimed
  fail-safe readback is not actually implemented (§9).
- OTA error → logged only; loop continues.

---

## 6. Configuration

All configuration is compile-time; the firmware has no runtime configuration
channel (no serial console, no MQTT config topic).

**Secrets — `src/secrets.h` (gitignored; template `src/secrets.example.h`)**
(the embedded-firmware secrets pattern — deliberately NOT the Go services'
systemd EnvironmentFile convention):

| Key | Meaning | Template value | Deployed value (checked-in local copy) |
|---|---|---|---|
| `WIFI_SSID` | station WiFi SSID | `CHANGE_ME` | `foom2m` |
| `WIFI_PASSWORD` | station WiFi password | `CHANGE_ME` | (secret, not recorded) |
| `MQTT_HOST` | broker host | `192.168.1.50` | `192.168.1.139` (shack broker on shari) |
| `MQTT_PORT` | broker port | `1883` | `1883` |
| `MQTT_USER` | broker user | `hf` | `hf` |
| `MQTT_PASSWORD` | broker password | `CHANGE_ME` | (secret, not recorded) |
| `OTA_PASSWORD` | ArduinoOTA update password (required for network uploads) | `CHANGE_ME` | (secret, not recorded) |
| `DEVICE_MODEL` | device identity published in BOTH slots' `/meta` | `"M5Stamp PLC"` | `"M5Stamp PLC"` |
| `DEVICE_SERIAL` | stable device id; also the OTA hostname and mDNS name | `"m5stamp-plc-1"` | `"m5stamp-plc-1"` |

**Non-secret config — `src/config.h`** (all hardcoded constants):

| Constant | Value | Meaning |
|---|---|---|
| `SITE` | `"muehle"` | topic site prefix |
| `SWITCH_SLOT` / `PA_ARM_SLOT` | `"hf/switch"` / `"hf/pa-arm"` | slot addresses |
| `RADIO_SLOT` / `ANT_SWITCH_SLOT` | `"hf/radio"` / `"hf/ant-switch"` | subscribed feeds for arm logic |
| `RELAY_PA_ARM/SPARE/PA_REMOTE/TRX_REMOTE` | 0 / 1 / 2 / 3 | 0-based expander channels for plan relays 1/2/3/4 |
| `RADIO_HEARTBEAT_MS` | 10000 | input-heartbeat window (s) |
| `SAFE_BANDS[]` | 160m,80m,60m,40m,30m,20m,17m,15m,12m,10m,6m | PA-safe band allow-list (ACOM 1200S coverage) |
| `MQTT_BUFFER_SIZE` | 1024 | PubSubClient buffer |
| `MQTT_KEEPALIVE_S` | 30 | MQTT keep-alive |
| `META_REFRESH_MS` | 300000 | **defined but unused** — intended periodic `/meta` republish, never wired |
| `UI_REFRESH_MS` | 1000 | LCD repaint cadence |
| `UI_BTN_DEBOUNCE_MS` | 150 | min time between button actions |
| `PA_ARM_HEARTBEAT_MS` | 10000 | pa-arm `/state` publish heartbeat |
| `LOOP_DELAY_MS` | 50 | main-loop period |

Hardcoded in `main.cpp` (not configurable at all): the two-slot topology, the
arm-logic boolean expression, the error strings and their precedence, the JSON
field names, `link:"wifi"`, `host:"embedded"`, `capabilities` contents, the
`expose` blocks, the `"TEST"` LCD label, button-to-relay assignments.

---

## 7. Deployment

- Target: the physical M5 Stamp PLC #1 on the station bench/shack — flashed
  from a workstation with PlatformIO; NOT deployed to shari, no systemd unit,
  no config file on any host.
- `platformio.ini`: platform `espressif32`, board `esp32-s3-devkitc-1`,
  framework `arduino`, C++17, `CORE_DEBUG_LEVEL=0`. Libraries (`lib_deps`):
  `m5stack/M5StamPLC @ ^1.0`, `m5stack/M5Unified @ ^0.2`,
  `knolleary/PubSubClient @ ^2.8`, `bblanchon/ArduinoJson @ ^7.0`.
- Two build environments:
  - `m5stamp-plc1` (default): USB flashing, `upload_protocol = esptool`,
    `upload_speed = 921600`, monitor 115200 with `esp32_exception_decoder, time`.
  - `m5stamp-plc1-ota`: same but `upload_protocol = espota`,
    `upload_port = m5stamp-plc-1.local` (mDNS name = `DEVICE_SERIAL`).
- `deploy.sh` (the README's claim "no deploy.sh" is stale — the script exists):
  - `./deploy.sh usb [port]` — build + flash over USB; auto-detects the serial
    port (`/dev/cu.usbserial-*`, `/dev/cu.SLAB_USBtoUART`,
    `/dev/cu.wchusbserial*` on macOS; `/dev/ttyUSB*`, `/dev/ttyACM*` on Linux);
    exits 1 if none found.
  - `./deploy.sh ota [host]` — build + flash over the network; default host
    `m5stamp-plc-1.local`; requires the OTA password to match and the running
    firmware to already have the OTA listener (ArduinoOTA), i.e. **the first
    flash after adding OTA support must be USB**; recovery from a bad OTA is
    also via USB.
- Update procedure (behavior contract): an OTA update begins by dropping the
  PA arm relay (`onStart`), then the device reboots into the new firmware,
  which cold-boots with all relays open (§5.1). The switch relays' commanded
  state is lost at reboot until the broker's retained `/cmd` replays on
  reconnect (self-heal). Downstream consumers see both slots' LWTs fire
  `offline` during the reboot.
- Pre-deploy checklist: `src/secrets.h` must exist (build fails without it).

---

## 8. Invariants & safety rules (must hold in any reimplementation)

1. **Fail-safe-open arm**: relay 1 is energized ONLY while `armed` is true;
   `armed` requires ALL of: operator permit `enabled`, radio
   `device_online=true`, `tuning=false`, band in the allow-list, a radio
   `/state` received within the last 10 s, and antenna switch
   `selected ∉ {"", "off"}`. Any single failure de-energizes the relay. Loss
   of supply power opens the relay by physics (coil unpowered).
2. **Cold boot opens all relays** before any network activity; the arm cannot
   close on boot until fresh radio + ant-switch states have arrived (staleness
   starts at "never received" = stale).
3. **The arm relay is never commandable.** The only accepted command is the
   `set_enabled` permit; there is no arm/disarm/force action, and the permit
   alone can never close the relay.
4. **Conservative defaults for all arm inputs**: missing fields read as
   radio-offline / not-tuning / band-unknown(unsafe) / antenna-not-ready.
5. **Per-slot LWT**: two independent MQTT connections, each with a retained
   `offline` will on its own `<slot>/status` and publishing retained `online`
   on connect — a crash marks BOTH slots offline at once. `/status` is PLC
   liveness; `device_online` in `/state` is always `true` when published.
6. **Retained `/cmd` as steady-state intent**: both slots re-subscribe their
   `/cmd` on every reconnect and re-apply the broker-replayed last command
   (self-healing). Local (button) toggles publish their intent back to the
   retained `/cmd` so the two cannot diverge while connected.
7. **State publishes read back actual relay positions**, not commanded values.
8. **pa-arm `/state` heartbeat ≤ 10 s** while connected (change-driven
   otherwise), so downstream freshness logic works; switch `/state` has no
   periodic heartbeat (change-only + retained).
9. **`ts` is uptime ms**, not wall-clock — consumers must treat it as a
   monotonic freshness marker only.
10. **No state is persisted across reboots** — everything is rebuilt from
    retained MQTT messages plus defaults; there is no flash/NVRAM state.
11. **Band allow-list is closed**: `160m,80m,60m,40m,30m,20m,17m,15m,12m,10m,6m`
    — exactly the ACOM 1200S coverage; 2m/70cm/unknown → never armed.
12. **Tuning blocks arming** (an ATU tune cycle transmits into a mismatched
    load; the PA must not amplify it).

---

## 9. Known defects & fragilities

1. **Arm relay frozen during WiFi outage**: the main loop's WiFi check
   early-returns BEFORE `recomputeArm()`, so with WiFi down the arm relay holds
   its last position indefinitely (no heartbeat enforcement, no recomputation).
   The "any failure drops the arm" contract is enforced only while WiFi is up.
   A reimplementation should run the safety evaluation regardless of link state.
2. **Silent arm drop on heartbeat timeout**: `currentPaArmError()` does not
   test heartbeat freshness, so a 10 s input-heartbeat timeout drops
   `armed` with **no `error` field** in the published state (or a stale,
   misleading one). Operators see the arm drop "for no reason".
3. **Heartbeat starvation by a change-only producer** (live incident, recorded
   in project memory): flexbridge publishes `hf/radio/state` only on change,
   so a radio sitting quietly on one frequency stops refreshing the pa-arm
   input heartbeat and the arm drops after 10 s despite a healthy radio; the
   first key-up after an auto-ground recovery goes "into the short". Any
   reimplementation must fix producer cadence or the consumer window — but
   must then coordinate with every consumer of the 10 s freshness figure.
4. **`antenna_ready` has no staleness window**: a stale ant-switch state stays
   "ready" forever; conversely, if the ant-switch bridge is down but its last
   retained state says an antenna is selected, the arm can close on data from
   a dead slot (mitigated only by the LWT being visible to *other* consumers —
   not consumed here).
5. **`value` must be a JSON string**: `/cmd` values are extracted with
   `doc["value"] | ""`. A sender publishing `"value": true` (JSON boolean)
   gets `""`, and for pa-arm `set_enabled` that silently sets `enabled=false`
   (disarms). The expose block even declares `value_type:"boolean"`, inviting
   exactly this bug. Station convention: booleans ride as the strings
   `"true"`/`"false"`.
6. **No MQTT reconnect backoff**: the `SlotMqtt::loop()` comment claims a
   backoff, but `connect()` is retried every loop pass (~every 50 ms, two
   clients) while WiFi is up — broker unreachable means ~20 connect
   attempts/second per slot indefinitely.
7. **Doc vs code: clean session**: `docs/m5stamp-hf-ctrl-mqtt-api.md` §1 and
   `mqtt_slot.h` comments claim CleanSession=false; the code uses PubSubClient's
   7-arg connect whose default is cleanSession=true. Self-heal still works
   because it relies on retained messages, not session persistence, but the
   documented contract is wrong.
8. **Doc vs code: `error` semantics**: the doc says `error` values are
   `radio offline / radio tuning / band not safe` (omits `antenna grounded`,
   which the code does produce) and claims "when `enabled` is false, `armed`
   is false with no error" — the code emits the error based on safety inputs
   regardless of `enabled` (e.g. `enabled:false` + radio offline publishes
   `error:"radio offline"`).
9. **Doc vs deploy facts**: README says "no `deploy.sh`" but the script exists;
   the API doc and local `secrets.h` say broker 192.168.1.139 (shack broker on
   shari) while the root CLAUDE.md and `secrets.example.h` say 192.168.1.50 —
   the broker address is effectively "whatever secrets.h says", and the
   discrepancy reflects an in-flight two-broker migration (per project memory,
   the shack-local broker work was committed but deployment coordination was
   still in progress as of 2026-08-28).
10. **`META_REFRESH_MS` (300 s) is dead config**: no periodic `/meta`
    republish exists; `/meta` is only re-sent on MQTT reconnect. Harmless
    (retained), but the config comment describing a "safety net" cadence is
    false.
11. **`relayGet` fail-safe readback is claimed but not implemented**: comment
    says "Returns false on any read failure (fail-safe: treat as open)"; the
    code is a bare `return M5StamPLC.readPlcRelay(ch);` with no failure
    detection.
12. **Vestigial state**: `SlotMqtt::connected_` is set true on connect and
    never cleared; `connected()` actually queries the client. Harmless but
    misleading.
13. **Local toggles during MQTT outage are silently reverted** on reconnect by
    the broker's retained `/cmd` replay (see §4.2).
14. **`device_online` hardcoded true** in both `/state`s — there is no
    distinction between "PLC up" and anything else; slot-level two-layer
    liveness convention (bridge LWT + device link) degenerates to LWT-only
    here, which consumers must know.
15. **PLC #2 / `muehle/uhf/pol-ctrl` does not exist**: the station slot table
    assigns the X-Quad polarization slot to "m5stamp-hf-ctrl (PLC #2)", but
    this repository contains no second firmware, no pol-ctrl topics, and no
    second build environment. Either the slot table is aspirational or the
    firmware lives elsewhere.
16. **Hardcoded `"TEST"` label** on the LCD header — leftover bench label in
    production UI.
17. **OTA start drops only the arm relay**, leaving PA/TRX remote-on relays in
    their current positions through the update; the device reboots into
    all-open anyway (see §7), so the window is short but nonzero.

---

## 10. Re-implementation notes

**Must be preserved verbatim (behavior contract):**

- Topic strings: `muehle/hf/{switch,pa-arm}/{meta,state,status,cmd}`;
  subscriptions `muehle/hf/radio/state`, `muehle/hf/ant-switch/state`.
- JSON payload field names, types, and units exactly as in §3 (`ts` uptime ms;
  `pa`/`trx` as `"on"`/`"off"` strings; `enabled`/`armed` booleans; `error`
  strings exactly `radio offline`, `radio tuning`, `band not safe`,
  `antenna grounded`, omitted when empty; command words `set_pa`, `set_trx`,
  `set_enabled` with string `value` key).
- Retained flags on all four suffixes of both slots (including `/cmd`) and the
  retained, QoS-1 LWT pattern (`offline` will, `online` on connect), one
  connection per slot so both wills fire together.
- The arm-logic expression, the 10 000 ms input-heartbeat window, the exact
  band allow-list, conservative missing-field defaults, the
  never-received-counts-as-stale rule.
- Fail-safe-open wiring (arm = energized) and cold-boot all-relays-open before
  networking.
- Change-driven publishing with a ≤ 10 s pa-arm state heartbeat; readback-based
  switch state; self-heal via retained `/cmd` replay on reconnect.
- Button behavior: B toggles PA remote-on, C toggles TRX remote-on, echo to
  retained `/cmd`; buttons work offline.

**Free to change (implementation detail):**

- Platform/language (Arduino C++/ESP32 was incidental); the two-PubSubClient
  trick is just how MQTT 3.1.1's one-will-per-client limit was satisfied —
  MQTT 5.0 with multiple wills, or a broker-side session, would be equivalent.
- The lack of reconnect backoff (should be fixed, not copied), the dead
  `META_REFRESH_MS`, the `connected_` vestige, the LCD layout/`"TEST"` label
  (cosmetics, though local indicator lights are genuinely useful at the bench).
- ArduinoOTA specifically — any authenticated OTA mechanism preserving the
  "arm relay drops before the update reboots the device" rule.
- Secrets-in-header-file pattern — any mechanism that keeps credentials off
  the bus and out of the repo is fine.

**Decisions a reimplementation must make explicitly (where this codebase is
arguably wrong — document whatever you choose):**

1. Whether the arm evaluation runs during link outages (here: it does not —
   defect 9.1). Recommended: run it always; the safe direction is dropping
   the arm, never holding it.
2. Whether heartbeat timeout and stale `antenna_ready` produce an `error`
   string (here: heartbeat — no, ant-switch — no window at all).
3. Whether `/cmd` accepts JSON-boolean `value` (here: it silently disarms —
   either accept booleans or reject the command loudly).
4. Whether the radio producer heartbeats (the live starvation incident) —
   coordinate the 10 s window with the producer's actual cadence.