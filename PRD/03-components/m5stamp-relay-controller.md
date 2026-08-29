# 03-components — m5stamp-relay-controller (embedded safety relay controller)

## 0. Purpose

This document specifies the station's **embedded safety relay controller**: a
small mains-powered microcontroller box mounted at the station that closes and
opens four physical relays, and publishes their state to the station's MQTT bus.
It plays two roles at once. First, it asserts the "remote-on" control lines of
the radio (transceiver, "TRX" — the device that transmits and receives) and of
the power amplifier ("PA" — the device that boosts the transmit signal to high
power), allowing software to switch those devices on remotely. Second — and this
is its safety-critical role — it drives the **PA arm relay**, a relay whose
closed contacts form the final "permission" circuit that enables the PA to
transmit at all. The arm relay is **fail-safe-open**: the safe state is the relay
being *de-energized* (contacts open, PA disabled), and the dangerous state
(armed, PA enabled) requires active, continuously-maintained electrical energy.
Any failure — operator permit withdrawn, radio offline, loss of the radio state
feed, a tuning cycle in progress, an unsafe frequency band, a grounded antenna,
loss of supply power, a firmware crash — must open the arm relay and thus
disable the PA. A re-implementation team must rebuild a component that is
indistinguishable from the current one on the bus **and** preserves this
fail-safe property in hardware, not only in software.

Terms used in this document are defined at first use. The bus-wide contract
(addresses, the four planes `meta/state/status/cmd`, the command payload
convention, band/mode vocabulary, liveness model) is specified once in
`02-interface-spec.md` and is not fully restated here; this document adds only
what is specific to this component. The system-level safety architecture that
this component participates in is specified in `06-safety.md`.

---

## 1. Orientation: what the reader needs to know first

- **Amateur radio ("ham radio")** is the licensed hobby of two-way radio
  communication. **HF** (high frequency) means the shortwave bands roughly
  3–30 MHz used for long-distance contacts. The station's HF station consists of
  a FLEX-8400 transceiver, an ACOM 1200S power amplifier, an ATR-1000 antenna
  tuner ("ATU" — matches the antenna's impedance to the feedline), a 1:6 antenna
  switch, and the antennas.
- **MQTT** is a lightweight publish/subscribe protocol: clients publish
  messages to hierarchical text **topics** on a central **broker**; subscribers
  receive messages for topic filters they subscribed to. A **retained**
  message is stored by the broker and delivered to every future subscriber of
  the topic. **LWT** (last will and testament) is a message the broker
  publishes on a client's behalf if that client's connection dies without a
  clean disconnect.
- A **slot** is this station's unit of management: one device's MQTT presence,
  addressed as `<site>/<station>/<slot>` (e.g. `muehle/hf/pa-arm`) with the
  topic suffixes `/meta` (identity birth certificate), `/state` (one retained
  JSON state snapshot), `/status` (plain-string LWT liveness), and `/cmd`
  (commands into the component). See `02-interface-spec.md` §3.
- A **compound device** is one physical device publishing more than one slot;
  the slots share the same `device{model,serial}` in their `/meta`, which is how
  consumers know they are the same physical box. This controller is a compound
  device with exactly two slots.
- A **band** is a named frequency allocation, e.g. `"20m"` (the 14 MHz amateur
  band). A **tuning cycle** is an automatic ATU tune in which the radio
  transmits a carrier into a possibly-mismatched load — unsafe while the PA
  could amplify it.
- A **relay** is an electromechanical switch: an energized coil pulls contacts
  closed; de-energizing the coil lets a spring open them. "Fail-safe-open"
  means the de-energized (no power, crashed CPU, dead supply) state is the open
  contacts — the PA cannot transmit. Loss of energy can only ever *remove* the
  arm permit, never create it.
- A **PLC** (programmable logic controller) is a small industrial-style
  embedded controller. The reference implementation runs on an **M5 Stamp PLC**
  (StamPLC K141): an ESP32-S3 microcontroller board with an I2C GPIO expander
  providing 4 relay outputs and 8 digital inputs, a small color LCD, and three
  front-panel buttons A (left), B (middle), C (right).
- The **grounded antenna position**: the antenna switch has an `"off"` selection
  that connects the antenna feed to ground for lightning protection.
  Transmiting into a grounded feed is destructive, so the arm must drop when
  the switch reports `"off"`.

---

## 2. The two slots and the relay map

The controller is ONE physical box publishing TWO slots. Both slots' `/meta`
carry **identical** `device.model` and `device.serial` — that shared identity IS
the compound-device relationship; there is no other mechanism tying the slots
together.

| Slot | Role | Physical relay (1-based) | Function |
|---|---|---|---|
| `muehle/hf/switch` | `switch` | relay 3, relay 4 | Asserts the PA and TRX **remote-on** control lines. Read-write, operator-controllable. A "remote-on line" is a control wire on the radio/PA that switches the device on remotely (a soft trigger, not a mains cut). |
| `muehle/hf/pa-arm` | `pa-arm` | relay 1 | The PA-enable **arm relay**, fail-safe-open, driven by the safety logic specified in §4. |
| (spare) | — | relay 2 | Unused. |

Requirements:

- **R2.1** The controller SHALL publish exactly these two slots and no others on
  the HF station. It SHALL NOT publish any UHF topic (see §10 for the PLC #2
  gap).
- **R2.2** The slot segment names (`hf/switch`, `hf/pa-arm`) and the site prefix
  (`muehle`) SHALL be exactly as given; they are part of the bus contract (see
  `02-interface-spec.md` §2).
- **R2.3** Relay-to-slot mapping SHALL be fixed: relay 3 = switch channel `pa`,
  relay 4 = switch channel `trx`, relay 1 = the arm relay, relay 2 = unused.
- **R2.4** Both slots' `/meta` SHALL carry the same `device{model,serial}`
  values (in the deployed instance: model `"M5Stamp PLC"`, serial
  `"m5stamp-plc-1"`; these are deployment configuration, see §7).

The switch slot's relays (3 and 4) are **not touched by any safety logic**: they
change only by `/cmd` or by the front-panel buttons; they open on a controller
crash only because the coil drivers die with the CPU.

---

## 3. The armed formula (NORMATIVE — the safety core)

The controller continuously computes:

```
armed = enabled ∧ radio_online ∧ ¬tuning ∧ band_safe ∧ heartbeat_fresh ∧ antenna_ready
```

Each input is derived from a specific, exact source on the bus:

| Input | Meaning | Source (exact topic + JSON field) | Value when never received / missing |
|---|---|---|---|
| `enabled` | Operator/sequencer arm permit — a *software* allow, one of six AND-ed inputs, never sufficient alone | Last `set_enabled` command on `muehle/hf/pa-arm/cmd` (retained) | `false` |
| `radio_online` | The radio's device link is up | `muehle/hf/radio/state` field `device_online` (bool) | `false` (default when the field is absent) |
| `tuning` | The radio is running an ATU tune cycle (transmits a carrier into a mismatched load; the PA must not amplify it) | `muehle/hf/radio/state` field `tuning` (bool) | `false` (default when absent) |
| `band_safe` | The radio's reported frequency band is one the PA covers | `muehle/hf/radio/state` field `band` (string) — must be a member of the closed allow-list, see R3.4 | empty string → **unsafe** |
| `heartbeat_fresh` | A `muehle/hf/radio/state` message arrived within the last **10 000 ms** | Any parseable message on that topic refreshes the heartbeat clock, whatever its content | never-received counts as **stale** |
| `antenna_ready` | An antenna is in circuit (not grounded) | `muehle/hf/ant-switch/state` field `selected` (string) — ready iff `selected ∉ {"", "off"}` | `""` / missing → **not ready** (conservative) |

The state sources are published by other components: `muehle/hf/radio/state` by
the radio bridge (see `03-components/flex-radio-bridge.md`), and
`muehle/hf/ant-switch/state` by the antenna-switch bridge (see
`03-components/waveshare-antswitch.md`). The controller only ever subscribes to
them; it never publishes on them.

Requirements:

- **R3.1** The controller SHALL evaluate the armed formula at least every
  **50 ms** (this is the reference main-loop period). Any period ≤ 200 ms is
  acceptable provided the arm relay still opens within one evaluation cycle
  of the input heartbeat expiring (R3.2, R3.3) — i.e. the worst-case
  arm-drop latency after the last radio message remains bounded at
  heartbeat window + one loop period (~10.05 s at a 50 ms period;
  ~10.2 s at a 200 ms period).
- **R3.2** The arm relay SHALL be energized if and only if `armed` is true;
  any single failing input SHALL de-energize the relay (open = PA disabled)
  within one evaluation cycle of the failure becoming detectable.
- **R3.3** The heartbeat window SHALL be exactly 10 000 ms, measured from the
  arrival time of the last parseable `muehle/hf/radio/state` message. A
  **never-received** feed SHALL count as stale from boot. An **unparseable**
  (invalid JSON) message SHALL be silently dropped and SHALL NOT refresh the
  heartbeat clock.
- **R3.4** `band_safe` SHALL be membership in the closed allow-list — exactly:
  `"160m", "80m", "60m", "40m", "30m", "20m", "17m", "15m", "12m", "10m", "6m"`
  — which is exactly the ACOM 1200S PA's coverage. Any other band (2 m, 70 cm,
  `unknown`, `gen`) or an empty/missing band SHALL be unsafe.
- **R3.5** `antenna_ready` SHALL be true iff the last received
  `ant-switch/state.selected` is a non-empty string other than `"off"`.
  Missing/unknown SHALL be treated conservatively as not-ready. (The reference
  implementation has NO staleness window on this input — see §9, defect D4,
  and Open decision O3.)
- **R3.6** On cold boot, all safety inputs SHALL start in the safe/false state
  (`enabled=false`, `radio_online=false`, `tuning=false`, `band=""`,
  heartbeat=never, `antenna_ready=false`), so the arm cannot close until BOTH a
  fresh radio state AND a non-off antenna-switch state have been received —
  even if a retained `set_enabled` command replays at reconnect.
- **R3.7** The arm relay SHALL NOT be commandable. The only accepted command
  affecting it is the `set_enabled` permit (§6.3); there SHALL be no arm,
  disarm, or force action, and the permit alone SHALL never close the relay.
- **R3.8** Loss of supply power SHALL open the arm relay by physics (unpowered
  coil = open contacts). This is a property of the electrical wiring, not of the
  software; see §11 (Open decision O7) for the electrical-contract gap.

### 3.1 The published `error` string

`muehle/hf/pa-arm/state` carries an optional `error` string (omitted when
empty). The reference implementation produces it with this exact function,
evaluated in this precedence order (first match wins), **independent of
`enabled`**:

1. `"radio offline"` — last radio state had `device_online` false, or none ever received;
2. `"radio tuning"` — last radio state had `tuning` true;
3. `"band not safe"` — last radio `band` not in the allow-list, or empty/never received;
4. `"antenna grounded"` — last `ant-switch/state.selected` is `"off"` or missing/empty.

Known reference defect (see §9, D2): a heartbeat timeout alone drops `armed`
with **no** `error` string, because the error function does not test heartbeat
freshness. A re-implementation SHALL NOT silently reproduce this; it SHALL
either add `"heartbeat stale"` as the highest-precedence error or otherwise
publish an explicit reason for every arm drop (see Open decision O2).

### 3.2 Two distinct 10-second rhythms — do not conflate them

- **Input heartbeat** (10 000 ms): the radio state freshness window feeding the
  armed formula (R3.3). When it expires, the arm relay opens within one loop
  pass — at most ~10.05 s after the last radio message.
- **Output heartbeat** (10 000 ms): the dedup-suppressed periodic republish of
  the retained `muehle/hf/pa-arm/state` (§5.2). It only refreshes the bus
  snapshot so downstream consumers can judge freshness; it never touches any
  relay.

---

## 4. MQTT presence

### 4.1 Connections and liveness

- Protocol: MQTT 3.1.1 over plain TCP, no TLS. Broker address, port, username,
  and password are deployment configuration (§7); the current production
  broker is at `192.168.1.50:1883` with username `hf` (see Open decision O8
  on the in-flight broker migration).
- **R4.1** The controller SHALL maintain **two independent MQTT client
  connections**, one per slot (client IDs `muehle-switch` and `muehle-pa-arm`),
  because MQTT 3.1.1 allows only one last-will per client and each slot needs
  its own `/status` LWT. Consequence: a controller crash fires BOTH slots'
  wills simultaneously, with no window in which one slot looks alive and the
  other dead. (MQTT 5.0 multiple wills, or any equivalent mechanism that
  guarantees both `/status` topics go offline together, satisfies this
  requirement.)
- **R4.2** Each connection SHALL register a **retained**, QoS-1 will on its own
  `<slot>/status` topic with payload `offline`, and SHALL publish retained
  `online` to the same topic on every (re)connect.
- **R4.3** `/status` SHALL be a plain string (`online`/`offline`), not JSON.
- **R4.4** Keep-alive: 30 s. Client RX/TX buffer: at least 1024 bytes (the
  retained `/meta` with its `expose` block exceeds smaller defaults).
- **R4.5** `/status` SHALL be understood as the liveness of this controller for
  this slot. `/state.device_online` is always `true` when published (the
  controller can only publish while it is up) — the two-layer liveness model
  (see `02-interface-spec.md` §3.5) degenerates to LWT-only here, and consumers
  MUST know this. Note the general LWT caveat: the broker fires a will only on
  an *unclean* disconnect, so retained `online` can persist after a clean stop;
  consumers must not trust `/status` alone.
- **R4.6** Session: the reference implementation effectively uses clean
  session = true (its MQTT library's connect-with-will default). Self-healing
  does NOT depend on session state — it relies entirely on retained messages
  (§4.3). A re-implementation may use either, provided R6.4 (retained-command
  replay on reconnect) holds. The shared docs' blanket "clean session = no"
  claim does not match this component; flag resolved as "either, retained
  messages are the contract" (see `02-interface-spec.md` §2 for the general
  discussion).

### 4.2 Published topics — payloads (all retained, exact JSON)

All publishes are of complete JSON documents. `ts` in both slots' `/state` is
**PLC uptime in milliseconds** — a monotonic freshness marker, NOT wall-clock
time; the controller has no real-time clock. This deviates deliberately from
the RFC 3339 `ts` used by the Go bridges (see Open decision O9).

`muehle/hf/switch/meta` — retained, published once per MQTT (re)connect (never
on a timer). Payload:

```json
{"schema":"1.0","role":"switch","device":{"model":"M5Stamp PLC","serial":"m5stamp-plc-1"},"link":"wifi","host":"embedded","capabilities":{"channels":["pa","trx"],"exclusive":false,"kind":{"pa":"remote_on","trx":"remote_on"},"relay_map":{"pa":3,"trx":4}},"expose":{"device":{"name":"M5Stamp PLC","model":"M5Stamp PLC","manufacturer":"M5Stack"},"fields":[{"key":"pa","name":"PA remote-on","type":"enum","options":["on","off"],"writable":true,"command":{"action":"set_pa","value_key":"value","value_type":"string"}},{"key":"trx","name":"TRX remote-on","type":"enum","options":["on","off"],"writable":true,"command":{"action":"set_trx","value_key":"value","value_type":"string"}}]}}
```

`muehle/hf/switch/state` — retained, published on (a) MQTT connect,
(b) immediately after any `/cmd` application, (c) immediately after any
front-panel button toggle. **No periodic republish.** Payload:

```json
{"ts":123456,"pa":"on","trx":"off","device_online":true}
```

- `pa`, `trx`: strings `"on"`/`"off"` — the **actual relay positions read back**
  from relays 3 and 4 (R5.5), never the last commanded values.
- `device_online`: boolean, always `true` (R4.5).

`muehle/hf/pa-arm/meta` — retained, once per (re)connect. Payload:

```json
{"schema":"1.0","role":"pa-arm","device":{"model":"M5Stamp PLC","serial":"m5stamp-plc-1"},"link":"wifi","host":"embedded","capabilities":{"fail_safe":"open","heartbeat":true,"relay":1},"expose":{"device":{"name":"M5Stamp PLC","model":"M5Stamp PLC","manufacturer":"M5Stack"},"fields":[{"key":"armed","name":"Armed","type":"boolean","writable":false},{"key":"enabled","name":"Enabled (arm permit)","type":"boolean","writable":true,"command":{"action":"set_enabled","value_key":"value","value_type":"boolean"}}]}}
```

`muehle/hf/pa-arm/state` — retained. Publish immediately whenever `enabled`,
`armed`, or `error` changes; otherwise at least every **10 000 ms** while
connected (dedup-suppressed against the last published snapshot). Skip
publishing entirely while the pa-arm connection is down. Payload:

```json
{"ts":123456,"enabled":true,"armed":false,"device_online":true,"error":"radio tuning"}
```

- `enabled`: boolean — the operator/sequencer arm permit (last `set_enabled`).
- `armed`: boolean — the **derived** actual arm-relay state; never commanded.
- `error`: string, **omitted when empty**; values and precedence per §3.1.

`muehle/hf/switch/cmd` — retained, published by the controller itself when a
front-panel button toggles a relay, echoing the intent
(`{"action":"set_pa","value":"on"}` etc.) so the retained intent and the local
state cannot diverge while connected. The controller both subscribes to and
publishes this topic; its own echo is harmlessly re-applied on arrival
(idempotent).

### 4.3 Subscriptions and retained-command replay (self-heal)

- **R4.7** The switch connection SHALL subscribe to `muehle/hf/switch/cmd`;
  the pa-arm connection SHALL subscribe to `muehle/hf/pa-arm/cmd`,
  `muehle/hf/radio/state`, and `muehle/hf/ant-switch/state` (absolute topics).
  All subscriptions SHALL be re-established on every (re)connect.
- **R4.8** Both `/cmd` topics are **retained** (the station's sanctioned
  exception for idempotent, self-healing actuator setpoints — see
  `02-interface-spec.md` §4.5). On every reconnect the broker replays the last
  retained command and the controller SHALL re-apply it: this is the
  self-healing steady state. After a controller reboot the switch relays'
  positions are thereby restored, and the `enabled` permit is restored (but
  cannot close the arm until fresh radio + antenna states arrive, per R3.6).
- **R4.9** The controller SHALL NOT subscribe to
  `muehle/power/psu-13v8` (the 13.8 V supply slot). The dependency on that
  supply is purely electrical — the controller boots from it, so its loss kills
  the controller and the arm relay opens by physics (R3.8). There is no
  software-level PSU dependency and no bus-visible "supply lost" input to the
  armed formula. A re-implementation that wants a bus-level PSU guard must add
  it as an explicit, separately-specified extension — silently adding one here
  would change the arm formula's failure behavior.

### 4.4 `ts` and QoS notes

The reference publishes state/meta at QoS 0 (retained), while the station
convention is QoS 1; retention, not the QoS level, is what makes self-heal
work. The LWT is QoS 1 retained. See Open decision O10.

---

## 5. Hardware-facing behavior

- **R5.1 Cold boot** SHALL write all four relays de-energized (open) as the
  FIRST initialization act, before any network activity. Physically, before the
  relay expander is initialized the coils are also unpowered = open. The
  cold-boot state is: arm open, PA/TRX remote-on off.
- **R5.2** The arm relay SHALL be driven from the derived `armed` value
  immediately when it changes (energize to arm, de-energize to open).
- **R5.3** Front-panel button B SHALL toggle the PA remote-on relay (3);
  button C SHALL toggle the TRX remote-on relay (4); button A is unused.
  Buttons SHALL work even when WiFi is down (local control is independent of
  the network).
- **R5.4** Button debouncing: at most one button action per 150 ms.
- **R5.5** Published switch state SHALL report **read-back actual relay
  positions**, not commanded values.
- **R5.6** The arm relay SHALL have NO local override — no button can force or
  drop it; it moves only with `armed`.
- **R5.7 (defect carried as a prohibition)** The reference implementation's
  relay readback has no failure detection (a comment claims fail-safe-on-error
  but the code returns the raw expander read). A re-implementation SHALL treat
  a failed/unverifiable readback conservatively (report `off`/open) OR shall
  demonstrate the read path cannot fail; it SHALL NOT claim fail-safe readback
  without implementing it. (Open decision O6.)
- **R5.8** A local LCD (reference: 240x135 px) SHALL show at most
  1 s-stale connection state (WiFi down / WiFi-only / MQTT) and the four
  actual relay positions as indicator lights. (Local indicators are genuinely
  useful at the bench; exact layout is free.) The reference hardcodes a
  leftover `"TEST"` header label — cosmetic, not a contract.

---

## 6. Command surface

All `/cmd` payloads are JSON of the universal station shape
`{"action":"<word>","value":"<string>"}` — the argument rides under the key
`value` (see `02-interface-spec.md` §4.5).

- **R6.1 (string-"true" convention — LOAD-BEARING)** The `value` field SHALL
  be carried as a **JSON string**. Booleans ride as the strings `"true"` and
  `"false"`. The reference implementation extracts `value` with a
  string-typed default (`doc["value"] | ""`): a sender publishing a JSON
  boolean (`"value": true`) yields an empty string, and for `pa-arm`
  `set_enabled` that **silently sets `enabled=false` — disarming the PA**. This
  is a known live defect: the reference `/meta` `expose` block even declares
  `value_type:"boolean"`, inviting exactly this bug. A re-implementation
  SHALL NOT reproduce the silent disarm. It SHALL either (a) accept JSON
  booleans and the strings `"true"`/`"false"` interchangeably, or (b) reject
  any non-string `value` **loudly** (publish an error or log; never silently
  apply the safe-side-but-wrong interpretation of a mistyped permit grant).
  Whichever is chosen, a command `{"action":"set_enabled","value":true}` (JSON
  boolean) SHALL never result in the permit being set to `false` without any
  indication. (Open decision O4.)

### 6.1 `muehle/hf/switch/cmd`

- `{"action":"set_pa","value":"on"}` — energize relay 3 (PA remote-on).
  `"off"` (or any other value — exact-match "on" only) de-energizes it.
- `{"action":"set_trx","value":"on"/"off"}` — same for relay 4 (TRX remote-on).
- Side effects of a recognized command: drive the relay, read both relays back,
  publish a fresh retained `switch/state` immediately (if connected).
- **R6.2** Unknown actions or unparseable JSON SHALL be silently dropped — no
  reply, no error topic, never a crash.
- **R6.3** The controller SHALL NOT echo commands received over MQTT back to
  `/cmd`; only front-panel button presses are echoed (§4.2). The retained
  `/cmd` content is owned by the last external commander.

### 6.2 Front-panel toggle during an MQTT outage (known behavior)

A button press while disconnected still toggles the physical relay, but the
`/cmd` echo is skipped — so the retained intent on the broker is STALE, and on
reconnect the broker's retained-command replay **overwrites the local change**
(the relay reverts to the pre-outage intent). This is actual reference
behavior, listed as a known defect (§9, D13); a re-implementation must decide
explicitly whether to keep it, and document the choice (Open decision O5).

### 6.3 `muehle/hf/pa-arm/cmd`

- `{"action":"set_enabled","value":"true"}` — set the software arm permit
  true. The `value` MUST be exactly the string `"true"`; anything else —
  `"false"`, `""`, a JSON boolean — sets the permit **false** (subject to
  R6.1's required loud-handling change for non-string values).
- Side effect: only the in-RAM `enabled` flag; `armed` is recomputed on the
  next evaluation cycle (≤ ~50 ms later in the reference), which may change
  relay 1 and publishes a fresh `pa-arm/state` if anything changed.
- There is **no arm/force command** — see R3.7.
- Unknown action or unparseable JSON: silently dropped (R6.2).

---

## 7. Configuration & secrets

There is **no runtime configuration channel** — no serial console protocol, no
config topic. All configuration is compile-time, baked into the firmware image.

Secrets (a gitignored header file, `secrets.h`, with a checked-in template):

| Key | Meaning | Template value |
|---|---|---|
| `WIFI_SSID` | station WiFi SSID | `CHANGE_ME` |
| `WIFI_PASSWORD` | station WiFi password | `CHANGE_ME` |
| `MQTT_HOST` | broker host | `192.168.1.50` (see Open decision O8) |
| `MQTT_PORT` | broker port | `1883` |
| `MQTT_USER` | broker user | `hf` |
| `MQTT_PASSWORD` | broker password | `CHANGE_ME` |
| `OTA_PASSWORD` | network-update password (required for OTA) | `CHANGE_ME` |
| `DEVICE_MODEL` | device identity in BOTH slots' `/meta` | `"M5Stamp PLC"` |
| `DEVICE_SERIAL` | stable device id; also the OTA/mDNS hostname | `"m5stamp-plc-1"` |

Non-secret constants (reference defaults; these are the "tunable defaults"
of this component — changing them changes observable timings, so a
re-implementation SHALL use exactly these values unless a decision below says
otherwise):

| Constant | Default | Meaning |
|---|---|---|
| `RADIO_HEARTBEAT_MS` | 10000 | input-heartbeat window (R3.3) |
| `PA_ARM_HEARTBEAT_MS` | 10000 | pa-arm `/state` republish heartbeat (§4.2) |
| `SAFE_BANDS[]` | 160m, 80m, 60m, 40m, 30m, 20m, 17m, 15m, 12m, 10m, 6m | arm allow-list (R3.4) |
| `MQTT_KEEPALIVE_S` | 30 | MQTT keep-alive |
| `MQTT_BUFFER_SIZE` | 1024 | MQTT client buffer |
| `LOOP_DELAY_MS` | 50 | main-loop period |
| `UI_REFRESH_MS` | 1000 | LCD repaint cadence |
| `UI_BTN_DEBOUNCE_MS` | 150 | button debounce |

Hardcoded, not configurable at all: the two-slot topology, the armed formula,
the error strings and their precedence, all JSON field names, `link:"wifi"`,
`host:"embedded"`, the `capabilities` and `expose` blocks, button-to-relay
assignments.

Requirements:

- **R7.1** Credentials SHALL NOT be committed to the repository; a build
  SHALL fail if the secrets file is absent. Any mechanism that keeps
  credentials off the bus and out of the repo is acceptable (the
  header-file pattern is one instance; note this deliberately differs from
  the Go services' 0600-TOML / systemd EnvironmentFile pattern — see
  `05-deployment-ops.md`).
- **R7.2** The WiFi radio SHALL be configured for reliability over power
  savings (the device is mains/13.8 V powered, not battery).

---

## 8. Runtime behavior

### 8.1 Boot sequence

1. Initialize the relay expander, then **write all four relays open** (R5.1).
2. Initialize the local UI.
3. Start WiFi (station mode, non-blocking).
4. Start the network-update listener; its start hook SHALL de-energize the arm
   relay before rebooting into an update (the PA/TRX remote-on relays are NOT
   dropped at update start in the reference — the window is short and the
   post-update cold boot opens everything anyway; keeping or fixing this is a
   documented choice).
5. RAM state: all safe/false per R3.6.

### 8.2 Main loop (reference: every 50 ms)

1. Service buttons (always — even with WiFi down).
2. **WiFi gate**: if WiFi is down, attempt reconnect at most every 5000 ms,
   repaint the LCD, and **return early**.
3. If WiFi is up: service both slot MQTT connections and the network-update
   listener.
4. Recompute `armed` (only reached when WiFi is up — see §9, defect D1).
5. Publish `pa-arm/state` if anything changed or ≥ 10 s since the last publish.
6. Repaint the LCD at most every 1000 ms.

### 8.3 Reconnection

- **R8.1** While WiFi is up and the broker is unreachable, each of the two
  MQTT clients SHALL attempt reconnect; on success it SHALL perform, in this
  exact order: (1) publish retained `online` to its own `<slot>/status`;
  (2) subscribe its own `<slot>/cmd` — the broker replays the last retained
  command, which the controller re-applies (R4.8); (3) re-publish retained
  `/meta` and `/state`; (4) the pa-arm connection only then subscribes
  `muehle/hf/radio/state` and `muehle/hf/ant-switch/state`, whose retained
  snapshots replay onto the safety inputs at that point. This order is
  bus-observable and part of the contract: the first post-reconnect
  `pa-arm/state` always reflects the pre-replay RAM state (per R3.6's
  safe boot state plus the restored `enabled` permit), and the replayed
  radio/ant-switch snapshots may flip a safety input and trigger an
  immediate follow-up `/state` publish once they land.
- **R8.2** The reference attempts reconnect **every loop pass (~every 50 ms,
  no backoff — ~20 attempts/s per client)**. This is a defect to fix, not
  copy: a re-implementation SHALL use bounded backoff (see Open decision O11).
- **R8.3** During a broker outage with a half-open TCP connection, the 30 s
  keep-alive governs detection; during that window the arm logic keeps running
  locally on stale inputs until the 10 s input heartbeat (fed only by live
  messages) drops the arm — safety holds because the evaluation is local.
- **R8.4** No state is persisted across reboots — everything is rebuilt from
  retained MQTT messages plus safe defaults. There is no flash/NVRAM state.

### 8.4 Deployment (USB first flash, then OTA)

- **R8.5** First flash SHALL be over USB serial (a build + flash script
  auto-detects the serial port; on macOS `/dev/cu.usbserial-*`,
  `/dev/cu.SLAB_USBtoUART`, `/dev/cu.wchusbserial*`; on Linux
  `/dev/ttyUSB*`, `/dev/ttyACM*`). Subsequent updates MAY be delivered
  over the network (OTA, "over-the-air") to the device's mDNS name
  (mDNS — multicast DNS — resolves names ending in `.local` on the local
  network) (`m5stamp-plc-1.local` in the deployed instance), authenticated by the
  OTA password. OTA requires the running firmware to already contain the
  update listener — **the first flash after introducing network updates must
  be USB**; recovery from a bad network update is also via USB.
- **R8.6** Update procedure contract: an update begins by dropping the arm
  relay; the device reboots; the new firmware cold-boots all relays open
  (§8.1); the switch relays' commanded state is lost until the broker's
  retained `/cmd` replays on reconnect; downstream consumers see both slots'
  LWTs fire `offline` during the reboot.
- **R8.7** The controller is an embedded node: it is NOT a systemd service and
  is NOT deployed to the shari host (see `05-deployment-ops.md`).

---

## 9. Known defects & fragilities (carried from the reference implementation)

A re-implementation MUST NOT silently reproduce these; each must be either
fixed or explicitly chosen and documented.

- **D1 — Arm relay frozen during WiFi outage (safety-relevant).** The main
  loop's WiFi gate early-returns BEFORE arm recomputation, so with WiFi down
  the arm relay holds its last physical position indefinitely — no heartbeat
  enforcement, no recomputation. The "any failure drops the arm" contract
  (R3.2) is enforced only while WiFi is up. On WiFi recovery the
  recomputation runs and (radio feed stale) drops the arm within one loop
  pass. **Recommended for re-implementation: evaluate the arm formula
  regardless of link state.** The safe direction is dropping the arm, never
  holding it. (Open decision O1.)
- **D2 — Silent arm drop on heartbeat timeout.** The error function (§3.1)
  does not test heartbeat freshness, so a heartbeat timeout drops `armed`
  with no `error` field (or a stale, misleading one). Operators see the arm
  drop "for no reason". See R3.1's requirement and Open decision O2.
- **D3 — Heartbeat starvation by a change-only radio producer (LIVE INCIDENT,
  hard requirement for the whole system).** The radio bridge publishes
  `muehle/hf/radio/state` only on change, so a healthy radio sitting quietly
  on one frequency stops refreshing the pa-arm input heartbeat and the arm
  drops after 10 s despite the radio being fine; and the operator's first
  transmission attempt ("key-up" — actuating the transmitter) after an
  automatic grounding recovery can occur while the antenna feed is still
  shorted to ground (see §1, grounded antenna position) — transmitting into
  that short is destructive. This is a recorded live
  failure. The fix is a **producer-side requirement**: the radio bridge SHALL
  republish its state at least every 5 s (or provide an equivalent liveness
  mechanism) — see `03-components/flex-radio-bridge.md` and
  `02-interface-spec.md`. The 10 s window here and the producer's cadence MUST
  be coordinated as one decision; a re-implementation of this component
  SHALL NOT unilaterally widen the window (every downstream consumer of the
  10 s freshness figure depends on it).
- **D4 — `antenna_ready` has no staleness window.** A stale antenna-switch
  state stays "ready" forever; if the antenna-switch bridge is down but its
  last retained state names a port, the arm can close on data from a dead slot.
  Mitigation in the reference: none at this component (the ant-switch's LWT
  is visible to other consumers but not consumed here). (Open decision O3.)
- **D5 — JSON-boolean `value` silently disarms.** See R6.1.
- **D6 — No MQTT reconnect backoff.** See R8.2.
- **D7 — Doc-vs-code drift (recorded, resolved for this document):** the
  reference's own API doc claims clean-session=false (code: effectively true)
  and claims `error` is suppressed when `enabled=false` (code: error is
  published based on safety inputs regardless of `enabled`, and the doc
  omits the `antenna grounded` error). This document follows the code.
- **D8 — Local toggles during an MQTT outage are silently reverted** on
  reconnect by retained-command replay (§6.2, Open decision O5).
- **D9 — `device_online` hardcoded `true`** in both `/state`s; two-layer
  liveness degenerates to LWT-only here (R4.5).
- **D10 — Relay readback fail-safe claimed but not implemented** (R5.7, Open
  decision O6).
- **D11 — OTA start drops only the arm relay**, leaving the remote-on relays
  in position through the short update window (§8.1 step 4).
- **D12 — The radio-feed subscription is the pa-arm connection's only path to
  safety inputs**: if that one MQTT connection dies while WiFi stays up, the
  radio feed stops arriving and the arm still drops within 10 s (safety
  holds); but if WiFi dies, see D1.

### 9.1 Reference-implementation notes (non-normative)

The reference implementation is Arduino C++ built with PlatformIO
(platform `espressif32`, board `esp32-s3-devkitc-1`, C++17) for the M5 Stamp
PLC (StamPLC K141: ESP32-S3 + AW9523B I2C GPIO expander at address 0x59
providing the 4 relay outputs and 8 unused digital inputs). Libraries:
`M5StamPLC ^1.0` (relay hardware seam), `M5Unified ^0.2` (LCD/buttons),
`PubSubClient ^2.8` (MQTT — its one-will-per-client limitation is the reason
for the two-connection pattern in R4.1; its 256-byte default buffer is the
reason for the 1024-byte buffer in R4.4), `ArduinoJson ^7.0` (payload
parsing — its `doc["value"] | ""` string-extraction is the mechanism behind
D5). It is flashed via PlatformIO's `esptool` (USB, 921600 baud) or
`espota` (network) environments, wrapped by a `deploy.sh usb|ota` script;
serial monitor 115200 baud exists for development/exception decoding only
(no runtime serial protocol). The platform, language, and libraries are all
implementation detail: any stack that satisfies R2–R8 is conformant.

---

## 10. PLC #2 / `muehle/uhf/pol-ctrl` — attributed but nonexistent (GAP)

The station slot table attributes the slot `muehle/uhf/pol-ctrl` (X-Quad UHF
antenna polarization relay control, "PLC #2") to this component's project.
**The repository contains no PLC #2 firmware**: no second build environment,
no pol-ctrl code, no `uhf/` topics anywhere in the source. Only the two HF
slots of §2 are implemented. This is a documented gap, not a component: either
the slot-table entry is aspirational or the firmware lives elsewhere. A
re-implementation team SHALL treat `muehle/uhf/pol-ctrl` as **not covered**
by this specification and raise it as its own scope decision (see
`00-system-overview.md` and Open decision O12).

---

## 11. Open decisions & unresolved facts

- **O1 — Does the arm evaluation run during link outages?** Reference: NO —
  the arm freezes in its last physical position for the duration of a WiFi
  outage (defect D1). Recommended: evaluate always; the safe direction is
  dropping. Evidence: main-loop WiFi gate before `recomputeArm()`
  (`m5stamp-hf-ctrl/src/main.cpp`). MUST be resolved before build.
- **O2 — Do heartbeat timeout and stale `antenna_ready` produce an `error`
  string?** Reference: heartbeat timeout — NO error (defect D2); ant-switch —
  no staleness window at all (D4). Proposed: add `"heartbeat stale"` (highest
  precedence) and an ant-switch freshness window with its own error string.
  Any added window must be coordinated with the antenna-switch bridge's
  publish cadence (see `03-components/waveshare-antswitch.md`).
- **O3 — `antenna_ready` staleness window size** (if O2 adds one): the
  ant-switch bridge publishes change-only; the same producer-cadence problem
  as D3 applies. The window and the ant-switch producer's heartbeat cadence
  are one decision.
- **O4 — JSON-boolean `value` handling** (R6.1): accept interchangeably or
  reject loudly. Reference silently disarms — forbidden.
- **O5 — Button toggles during an MQTT outage**: keep the
  revert-on-reconnect behavior (§6.2) or reconcile local vs retained intent
  (e.g. republish the new intent on reconnect before applying the replay).
- **O6 — Relay readback failure handling** (R5.7): implement the claimed
  fail-safe (treat unreadable as open) or demonstrate the read path cannot
  fail. Unverified in the reference.
- **O7 — The electrical contract is OUTSIDE this repository.** The physical
  wiring — which relay contacts interrupt the PA's hardware enable line, how
  the arm relay ANDs into the hardware TX-inhibit series interlock chain, the
  remote-on line polarity, contact ratings — exists only in the shack, not in
  the repo or docs. The safety model (see `06-safety.md`) places enforcement
  in this hardware chain, with software only arming/disarming. A
  re-implementation MUST obtain the wiring from the site and SHALL treat
  fail-safe-open (R3.2, R3.8) as a hard requirement verified on the bench
  (pull power with the arm energized and confirm the PA goes unable to
  transmit).
- **O8 — Broker address.** The PRD treats `192.168.1.50:1883` as current
  production per repo-wide defaults, but the checked-in local copy of this
  firmware's `secrets.h` and its API doc say `192.168.1.139` (a planned
  broker migration to the shari host, committed but not fleet-deployed as of
  2026-08-29 — see `05-deployment-ops.md`). The deployed PLC's actual broker
  is whatever its flashed `secrets.h` says and was not verifiable from the
  workstation. Confirm on-site which broker the live PLC is connected to
  before reconstructing.
- **O9 — `ts` form.** This component uses uptime milliseconds; the station
  model specifies RFC 3339 UTC strings for `/state.ts`. Consumers already
  treat this component's `ts` as a freshness marker only. Decide whether the
  rebuilt component keeps uptime ms (documented deviation) or moves to
  RFC 3339 (needs an RTC or NTP on the embedded node).
- **O10 — Publish QoS.** Reference publishes state/meta at QoS 0 (retained);
  the station convention is QoS 1. Retention makes self-heal work either
  way; pick one and conform (`02-interface-spec.md` §2).
- **O11 — Reconnect backoff parameters** (fixing D6): reference retries
  ~20/s/client indefinitely. Choose bounded backoff parameters.
- **O12 — PLC #2 / `muehle/uhf/pol-ctrl`** (§10): does the re-implementation
  scope include a polarization controller firmware, or is the slot-table
  entry dropped? Nothing to reconstruct from — no code exists in the repo.
- **O13 — `enabled=false` + bad safety input:** the reference publishes an
  `error` even when the permit is off (contrary to its own doc; §3.1). Keep
  this (informative) or suppress errors while deliberately disarmed —
  coordinate with the operator console's display logic (`04-console.md`).
- **O14 — Ultrabeam switch-port conflict (inherited, does not directly touch
  this component):** station docs say the Ultrabeam is on antenna-switch port
  3; `antennaselect` config and the console say port 4. This component only
  consumes `selected ∉ {"", "off"}`, so either variant satisfies R3.5 — but
  the on-site port map must be confirmed before any wiring-dependent
  reconstruction; see `03-components/antennaselect.md` and
  `03-components/waveshare-antswitch.md`.