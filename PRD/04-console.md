# 04 — Operator console (hf_console)

> **PRD document 04 of the Mühle station-automation reconstruction.**
> Audience: competent software engineers who know **nothing** about amateur radio, MQTT culture, or this repository. This document defines every specialized term at first use. It is a **feature/behavior specification from the operator's perspective**. It carries the exact data dependencies: topics, JSON payload shapes, units, timings, and error behavior. It is not a widget-by-widget description of the reference application.

**Purpose.** The operator console is the single human interface to the Mühle HF station. It is a fixed-mount tablet on the operating desk. It (a) shows the live state of every station subsystem. It (b) turns every operator action into one small JSON command on the station message bus. Most importantly, it (c) never lets stale data, a dead link, or an unknown value masquerade as a healthy station.

The console holds **no station logic of its own**. Sequencing, antenna arbitration, and interlocks live in server-side components (see `03-components/powerseq.md`, `03-components/antennaselect.md`). The console renders, guards operator input at the UI level, and fails closed on RF-safety questions.

The two upstream feeds are the MQTT control bus and an independent HTTP spot feed. This document specifies them, the in-memory domain model, every panel and command, the operator-safety presentation rules, configuration, and the visual design language.

Cross-references:

- `00-system-overview.md`: station glossary and slot table.
- `02-interface-spec.md`: bus planes, the `/cmd` value-key convention, and the liveness model.
- `06-safety.md`: station-wide interlock rationale.
- `05-deployment-ops.md`: shari, the Raspberry Pi that runs all server-side services.

---

## Glossary (plain-language definitions, first use applies throughout)

- **Amateur radio ("ham radio")**: the licensed, non-commercial hobby of two-way radio communication. Operators exchange contacts ("QSOs") worldwide with identifying callsigns.
- **HF**: "high frequency", the 3–30 MHz shortwave radio spectrum for long-distance communication. The Mühle station's HF chain is the subject of this console.
- **MQTT**: a lightweight publish/subscribe protocol. Clients publish messages to hierarchical slash-separated **topic** strings (for example `muehle/hf/pa/state`). A central **broker** relays them to subscribers. The broker stores a **retained** message and re-delivers it to every future subscriber. A zero-length message to the same topic overwrites or clears it. **LWT** (last will and testament) is a message that the broker publishes for a client if that client drops without a clean disconnect. **QoS 0** is at-most-once delivery. **QoS 1** is at-least-once delivery.
- **Slot**: one addressable station component on the bus, for example `muehle/hf/pa` (the power amplifier's bridge). Each slot owns four topics called **planes**: `/meta` (static identity), `/state` (a retained JSON telemetry snapshot, republished whole on every change), `/status` (bridge-process liveness, the bare string "online"/"offline", driven by MQTT LWT), and `/cmd` (commands addressed to the slot).
- **Bridge**: the server-side program on shari that translates one physical device's protocol onto its slot.
- **TRX / transceiver**: the radio itself (a FLEX-8400).
- **PA / power amplifier**: a device that boosts the radio's transmit signal (an ACOM 1200S, up to 1200 W).
- **ATU / antenna tuning unit ("tuner")**: an impedance-matching network placed in the feed line (an ATR-1000). "Inline" = in the RF path. "Bypass" = out of it.
- **SWR (standing-wave ratio)**: feed-line impedance match quality. 1.0 is perfect. 3.0 or more is dangerous. Reflected power heats the PA.
- **Rotator**: the motor that turns the mast-mounted beam antenna. Position is an azimuth in degrees.
- **Ultrabeam**: a motorized tubular beam antenna. Motors tune its element lengths per band (6–20 m). It supports forward / 180°-reverse / bi-directional radiation patterns and full retraction.
- **DVK (digital voice keyer)**: memory slots in the radio that play back recorded audio (for example contest exchanges).
- **FT8/FT4**: narrow-bandwidth weak-signal digital modes. Stations exchange automated contacts. Spotters report these contacts as **spots**.
- **CW (continuous wave)**: Morse-code telegraphy. The bus mode name for it is `cw`.
- **SSB (single sideband)**: the usual voice mode on HF. The bus modes `usb` and `lsb` are its upper- and lower-sideband variants. The SNR gate (R2.9.3) groups these plus `am`/`fm`/`data` as the "SSB family".
- **DX spot**: a report of a distant ("DX" = long-distance) station heard on the air: who, where, band, and signal-to-noise ratio (SNR, in dB).
- **Maidenhead locator / grid square**: a geographic encoding used in amateur radio. 2 characters = a "field" (20° longitude × 10° latitude). 4 characters = a "square" (2° × 1°). 6 characters = a "subsquare" (5′ × 2.5′). Example: `JN58sd`.
- **QTH**: ham shorthand for "station location" (the station's own location).
- **AEQD (azimuthal equidistant projection)**: a map projection centered on one point. Every other point appears there at its true bearing AND true distance from the center — exactly what a beam-antenna compass needs.
- **horstreporter**: the station-owner's own external web tool. It aggregates DX spots from four sources: FT8/FT4 through MQTT, dxcluster (internet chat channels where operators announce stations heard), RBN (a network of automated Morse-code listening robots), and WSPR (a low-power beacon mode). It serves them over **SSE (Server-Sent Events)**. SSE is a long-lived HTTP response stream of `data: <text>` lines. A blank line ends an event.
- **SmartSDR**: the FLEX radio's own software. The radio stores "mic profiles" as named transmit-audio configurations.

---

## 1. Purpose, role, and platforms

### 1.1 Role

The console is a **pure MQTT consumer + commander plus one read-only HTTP feed**:

1. It subscribes to `muehle/#` on the station MQTT broker. It mirrors everything it hears into a local in-memory slot store (§2.3).
2. It publishes JSON command payloads to `muehle/<slot>/cmd` topics when the operator taps controls (§2.8).
3. Independently of MQTT, it opens an SSE feed to horstreporter. It renders DX-spot positions on two map projections (§2.9).

It has **no bus presence of its own**: no slot, no `/meta`, no `/state`, no LWT, no heartbeat, no periodic publish of any kind. Its only footprint is the `/cmd` messages it publishes and its broker session. A reconstruction must not add console presence topics, heartbeats, or retained console state. Another component must never start to depend on the console to stay alive.

The console must not hold station logic. It renders and commands. It never sequences, arbitrates, or caches policy. The single documented exception is the 6 m forward-only correction (§3.5). That is a device-physics guard mirrored from the antenna controller's own web UI.

### 1.2 Platforms (normative)

Three delivery targets of one application, all sharing one behavior contract:

1. **Android tablet** (primary). Fixed-mount on the operating desk, full-screen immersive mode, all orientations allowed (layout, not orientation locks, decides the shape).
2. **Phone (iOS)**. Same application, single-column reflow (breakpoint: shortest side < 600 px).
3. **Web, served from shari over the LAN.** Browsers cannot open raw TCP sockets. So the web build connects to the broker over a WebSocket. A small server-side proxy (`webbridge`) byte-forwards that stream to the broker's TCP port. The proxy does no MQTT interpretation — the browser's MQTT client speaks raw MQTT packets through the tunnel.

> **Reference-implementation note (non-normative).** The reference is a Flutter application: an Android APK sideloaded through ADB, a self-sideloaded iOS IPA, and a Flutter web build.
> The web proxy (`webbridge/main.go`, Go + gorilla/websocket) serves the static Flutter build at `/` and exposes `/mqtt` as a byte-forwarding WebSocket↔TCP proxy on port 8091. Deployment runs it as a hardened systemd service `hf-console-web` on shari under a dedicated no-login user.
> Raw-TCP MQTT from a mobile app does not trip iOS App Transport Security. ATS governs only HTTP/WebSocket sessions. The console needs no ATS exception.
> Any per-platform transport selection with the same behavior is acceptable if the §2 contracts hold.

---

## 2. Domain-layer contracts

### 2.1 MQTT session contract

| Parameter | Required value |
|---|---|
| Protocol | MQTT 3.1.1, no TLS (LAN broker, port 1883 default). |
| Subscription | exactly one: `muehle/#` at **QoS 0**. |
| Subscription timing | immediately on every (re)connect, before anything else, so the retained-payload flood populates the store before user interaction. |
| Session | clean session, no broker-side queue. By design, the console loses messages that arrive while it has no connection. |
| Keep-alive | 20 s. |
| Client ID | unique per app launch (`hf-console-<epoch-milliseconds>`), so concurrent sessions do not evict each other. |
| Auto-reconnect | enabled. On reconnect the console re-establishes the subscription (the retained flood then refreshes the store). |
| Connection-loss detection | keep-alive plus disconnect callbacks must flip one boolean `connected` state. Every surface that cares (banner §4.1, top-bar dot §3.0) listens to that state. |
| Console LWT | none (the console is not a monitored component). |
| Publishes | only `muehle/<slot>/cmd`, at **QoS 1**, retain flag fixed per topic (§2.7). |
| Publish while disconnected | **silently dropped** — no queue, no error dialog. The link-down banner (§4.1) is the operator-facing warning. |

Requirement statements:

- **R2.1.1** The console must subscribe to `muehle/#` and nothing else.
- **R2.1.2** The console must publish only to topics matching `muehle/<slot>/cmd`.
- **R2.1.3** If the first connection try fails (for example the broker is unreachable at app launch), the console must open anyway and render in offline state. The console must swallow the failure. *Known fragility carried forward deliberately:* in the reference the library's auto-reconnect arms only after a first successful connection. So a console started before the broker is reachable can stay offline until an operator restarts it manually. Any reconstruction must give at least one recovery path from a failed first connect (see §7, open decision).
- **R2.1.4** The console must expose a `clear(topic)` capability: publishing a zero-length **retained** message to a topic (the standard MQTT retained-clear idiom). The shipped UI does not invoke it. It exists for tooling and tests.
- **R2.1.5** QoS 0 accepts silent delivery gaps. A gap that carries a plane snapshot leaves that plane stale until the slot publishes again or the console reconnects. The `connected` indicator stays true meanwhile. The reference does no periodic refresh. A reconstruction that wants gap-free state must add an explicit refresh mechanism.

### 2.2 Inbound decoding and plane parsing

Per received message, the console must:

1. Decode bytes as UTF-8 and tolerate malformed byte sequences.
2. Treat an **empty payload as "cleared plane"**: the console sets the corresponding slot plane value to null. This is how a component deletes a retained message.
3. Otherwise JSON-decode. On decode failure keep the raw string. Only the `/status` plane legitimately is a raw string: the bare text "online"/"offline".
4. Parse the topic: the last path segment MUST be one of the four plane names `meta` / `state` / `status` / `cmd`. Everything before it is the slot address (for example `muehle/hf/pa/state` → address `muehle/hf/pa`, plane `state`). The console must ignore topics whose last segment is not a known plane name.
5. The console applies each `/state` snapshot as a **full replacement**, never as a merge.

### 2.3 Local slot store

The console maintains one in-memory record per slot address (latest value wins per plane):

```
Slot {
  address: String                    // e.g. "muehle/hf/pa"
  meta:   object|null                // from <addr>/meta
  state:  object|null                // full JSON snapshot from <addr>/state
  status: String|null                // from <addr>/status ("online"|"offline" or cleared)
  cmd:    object|null                // last command seen on <addr>/cmd
  statusChangedAt: DateTime?          // local-clock stamp of last /status change
  deviceChangedAt: DateTime?          // local-clock stamp of last device_online change
}
```

- When the bus clears `/state` or `/status` (empty retained payload), that is itself a change. The console must stamp the corresponding change time.
- Every bus message must trigger a store change notification. The reference notifies the whole UI per message. A reconstruction can coalesce instead. Then no displayed value can ever fall stale beyond the coalescing window, and the DX service keeps its own 500 ms throttle (§2.9).
- **No optimistic UI**: the console renders only what the bus confirms. Missing state keys render from the neutral defaults of §2.6. They never render from the value that a control sets.

### 2.4 Two-layer liveness

A slot is `isOnline` only if BOTH layers are up (see `02-interface-spec.md` §5):

- **bridgeOnline** = `/status` == the exact string `"online"`. This plane carries the bridge's MQTT LWT.
- **deviceOnline**:
  - `state == null` (no snapshot ever received) → **false**.
  - `state` present but **without a `device_online` key** → **true**. Logic-only slots (the antenna-selection reconciler, the power sequencer, the discovery consumer) have no physical device. They legitimately omit the key.
  - Otherwise → `state.device_online == true`. The bridge process can stay `/status: online` while its serial/USB/Ethernet link to the device is down. `/state.device_online` is the only honest device-link signal.

Requirements:

- **R2.4.1** The console must key offline detection on `/status` plus `/state` together. It must not use `/status` alone, nor `/state` alone.
- **R2.4.2** (station-wide caveat the console must survive) On a **clean** process shutdown the broker does not fire the LWT. So a retained `/status` can stay `"online"` for a stopped service. The console therefore cannot tell "stopped cleanly" from "online" through `/status` alone. This is precisely why the second layer exists. And that is why a slot whose bridge stopped cleanly surfaces later through `device_online`, silent-slot reporting, or missing state rather than `/status`.
- **R2.4.3** `device_online` form: deployed bridges publish an explicit `device_online: true`. The integration-model doc allows omission of the key when true. The console must treat both forms as the same (absence = true, once a state snapshot has arrived). Whether a reconstruction mandates explicit-true is an open decision (§7, and `02-interface-spec.md`).

### 2.5 Offline rows, "went dark" stamps, and the connect grace

**Expected slots.** The console pre-declares the 15 slots that must exist:

```
muehle/power/master     muehle/power/psu-13v8    muehle/hf/radio
muehle/hf/ant-ctrl      muehle/hf/ant-switch     muehle/hf/switch
muehle/hf/pa-arm        muehle/hf/antenna-select  muehle/hf/pa
muehle/hf/rotator       muehle/hf/tuner          muehle/hf/power-seq
muehle/hf/discovery     muehle/uhf/rotator       muehle/uhf/pol-ctrl
```

(Note: `muehle/uhf/pol-ctrl` has **no firmware in the reference repo**. PLC #2 (the station's second small microcontroller relay board) is a documented station gap. So this slot will always appear as silent/offline in the faults bar until that firmware exists. That is correct behavior, not a defect.)

**Offline-row taxonomy.** For every slot ever heard from, the computed offline list must hold, with the exact strings:

| Condition | Row text | Timestamp shown |
|---|---|---|
| `/status != "online"` | `<addr>: bridge down` | `statusChangedAt`. |
| bridge up but device link down | `<addr>: device unreachable` | `deviceChangedAt ?? statusChangedAt`. |

**Silent expected slots (dead-since-boot visibility).**

- **R2.5.1** On every (re)connect the console must record the connect time and start a **3-second grace timer**. The reconnect re-floods retained state. So connect-time silence must not trip the report.
- **R2.5.2** The console must measure the grace from connect, **not from the first received message**. A broker with zero retained payloads delivers nothing at all. That is exactly the dead-station case the report exists for.
- **R2.5.3** After the grace expires, the console must report every expected slot that has produced **no message at all this session**. The report text is `<addr>: silent (no state since connect)`, stamped with the connect time. The report must clear the moment the slot publishes anything.
- **R2.5.4** The grace timer must force a UI refresh after 3 s even if no further bus message arrives (a one-shot timer). So the report appears even on a quiet bus.
- **R2.5.5** Timestamps are best-effort. Retained messages arriving at connect count as "changes". So "when did it go dark" can sit below the true time (floored at connect time), never above it. The UI must render the store's stamp, not the render-time clock.

**Fault history** (from `/state.fault` and `/state.error` on every slot):

- Active fault text per state apply: `error.toUpperCase()` if `error` is non-empty and not case-insensitively `NONE`. Else `fault.toUpperCase()` if non-empty and not `none`. Else no active fault. **Error outranks fault.**
- When a previously active record for the same address has text that no longer matches, the console must mark the record cleared (inactive).
- An exact repeat must re-activate the record AND refresh its timestamp. Repeated faults stay at the top and read as ongoing. Known consequence: a recurring fault's "when" becomes its last-seen time, not its onset.
- Timestamp source: `state.ts` when present, else the local clock, ISO-8601.
- The console never deletes records. It only deactivates them. History caps at **30 records**. It evicts the oldest. Record key: `<address>|<TEXT>`.

**PSU-off root-cause naming.** A switched-off PSU silently kills the whole HF control chain.
When the 13.8 V PSU bridge (`muehle/power/psu-13v8`) is itself online, its `state.power` reads `"off"`, and any `muehle/hf/` slot appears in the offline list.

The faults bar must then add the explanatory top line:

```
muehle/power/psu-13v8: PSU OFF — HF control chain unpowered
```

Confirmed-off only: a missing `power` key is **unknown**. To infer a fault from an absent key is forbidden.

**Faults bar rendering.** Header "FAULTS" + tag (`N ACTIVE` red / `ALL OK` green). Rows: fault-history records plus offline entries. But an address with an active fault suppresses its generic offline line (the fault text says more). Rows carry an HH:MM:SS stamp. Sort: active first, then newest first. **Visible list capped at 4 rows.** Empty state: "No faults or offline devices".

### 2.6 State-field catalog and neutral defaults

The console reads the following `/state` keys (full catalog per slot). The console tolerates missing keys everywhere. Units are normative: **frequency is always integer hertz (`freq_hz`), never kHz or MHz**. Power is in watts. Temperature is in °C. SWR is a ratio. Azimuth is in degrees.

| Slot | Keys read (type) |
|---|---|
| `muehle/hf/radio` | `freq_hz` (int, Hz), `band` (string, for example `"20m"`), `mode` (`cw`,`usb`,`lsb`,`am`,`fm`,`data`), `tx` (`rx`/`tx`), `tuning` (bool), `drive` (int %), `dvk_status` (`idle`/`playback`/`recording`/`preview`/`disabled`), `dvk_id` (int, 0 = none), `mic_profile` (string, active profile name), `mic_profiles` (sorted JSON array of profile-name strings), `device_online`, `ts`. |
| `muehle/hf/pa` | `mode` (`standby`/`operate`), `keyed` (`rx`/`tx`/`inhibited`), `fault` (`none` or word), `error` (`""`/`NONE` when clear), `temp_c` (num), `fwd_power_w` (num 0–1200), `rfl_power_w` (num), `swr` (num 1.0–4.0), `pa_state` (device string), `power` (`on`/`off`), `device_online`, `ts`. |
| `muehle/hf/tuner` | `inline` (bool), `settling` (bool), `fault` (string), `swr` (num), `device_online`, `ts`. |
| `muehle/hf/rotator` | `az` (double), `target_az` (double), `moving` (bool), `device_online`, `ts`. |
| `muehle/hf/ant-ctrl` | `direction` (string), `moving` (bool), `band` (string, can be `""`), `device_online`, `ts`. |
| `muehle/hf/ant-switch` | `selected` (`off`\|`port1`\|`port4`\|`port5`\|`port6`), `settled` (bool), `device_online`, `ts`. |
| `muehle/hf/antenna-select` | `mode` (`auto`/`manual`), `ts` (logic slot — no `device_online` key). |
| `muehle/hf/switch` | `pa` (`on`/`off`), `trx` (`on`/`off`), `device_online`, `ts`. |
| `muehle/hf/pa-arm` | `enabled` (bool), `armed` (bool), `error` (string), `device_online`, `ts`. |
| `muehle/hf/power-seq` | `phase` (for example `idle`, `running`, `starting`, `stopping`), `fault` (string), `ts`. |
| `muehle/power/master`, `muehle/power/psu-13v8` | `power` (`on`/`off`), `device_online`, `ts`. |

Neutral display defaults apply when a key is absent. They encode "unknown/neutral". They are deliberately distinguishable from real values. PA: `mode=standby`, `keyed=rx`, `fault=none`, `error=""`, `temp_c=0`, `fwd_power_w=0`, `swr=1.0`. Tuner: `inline=false`, `settling=false`, `fault=""`, `swr=1.0`. Rotator: `az=0` (target = az), `moving=false`. Ant-ctrl: `direction=forward`. Power slots: `power="off"`. Radio: `freq_hz=0`, `band=""`, `mode=""`, `tx="rx"`, `drive=0`, `dvk_status="idle"`, `dvk_id=0`. Pa-arm: `enabled=false`, `armed=false`, `error=""`. A missing antenna-switch `selected` renders "Unknown", never a port (§3.6).

### 2.7 Per-slot `/cmd` retain policy

The bus does not normally retain `/cmd` topics (one-shot). The commander retains idempotent actuator setpoints. So a restarted device re-applies the last commanded state (self-heal replay). The console must use exactly this fixed map:

| `muehle/<slot>/cmd` | Retained |
|---|---|
| `power/master` | **yes**. |
| `power/psu-13v8` | **yes**. |
| `hf/switch` | **yes**. |
| `hf/pa-arm` | **yes**. |
| `hf/ant-ctrl` | **yes**. |
| `hf/ant-switch` | **yes**. |
| `hf/antenna-select` | **yes**. |
| `hf/pa` | no (one-shot). |
| `hf/rotator` | no (one-shot). |
| `hf/tuner` | no (one-shot). |
| `hf/power-seq` | no (one-shot). |
| `hf/radio` | no (one-shot. DVK play/stop are one-shot too). |

Wrongly retaining a one-shot (for example rotator `set_az`) causes a device to replay a stale movement on boot. Wrongly not retaining a setpoint loses commanded state on restart. This map matches the bus-wide policy (see `02-interface-spec.md`).

### 2.8 Command payload contract

Every operator action is one publish of a JSON payload to `muehle/<slot>/cmd` at QoS 1 with the §2.7 retain flag. The station-wide payload convention is `{"action": <name>, "value": <string argument>}`. The deviations below are **load-bearing**. Deployed bridges parse them as-is. A reconstruction of the console must reproduce them byte-for-byte in key names and value types.

| # | UI source | Topic | Retain | Exact payload |
|---|---|---|---|---|
| 1 | Power panel, MAINS toggle | `muehle/power/master/cmd` | yes | `{"action":"set_power","value":"on"|"off"}`. |
| 2 | Power panel, PSU toggle | `muehle/power/psu-13v8/cmd` | yes | `{"action":"set_power","value":"on"|"off"}`. |
| 3 | Power panel, TRX toggle | `muehle/hf/switch/cmd` | yes | `{"action":"set_trx","value":"on"|"off"}`. |
| 4 | Power panel, PA toggle | `muehle/hf/switch/cmd` | yes | `{"action":"set_pa","value":"on"|"off"}`. |
| 5 | Power panel, START STATION | `muehle/hf/power-seq/cmd` | no | `{"action":"start"}` — **no `value` key**. |
| 6 | Power panel, STOP STATION | `muehle/hf/power-seq/cmd` | no | `{"action":"stop"}`. |
| 7 | Ultrabeam panel, FORWARD | `muehle/hf/ant-ctrl/cmd` | yes | `{"action":"direction","value":"forward"}`. |
| 8 | Ultrabeam panel, 180° | same | yes | `{"action":"direction","value":"reverse"}`. |
| 9 | Ultrabeam panel, BI-DIR | same | yes | `{"action":"direction","value":"bidirectional"}`. |
| 10 | Ultrabeam panel, RETRACT | same | yes | `{"action":"retract"}` — no `value` key. |
| 11 | Ultrabeam 6 m auto-correction (§3.5) | same | yes | `{"action":"direction","value":"forward"}`. |
| 12 | Antenna panel, direct-drive port change | `muehle/hf/ant-switch/cmd` | yes | **deviation: no `action`** — `{"select":"off"|"port1"|"port4"|"port5"|"port6"}`. |
| 13 | Antenna panel, reconciler-path port change | `muehle/hf/antenna-select/cmd` | yes | **deviation: no `action`** — `{"request":"<port token>"}`. |
| 14 | Antenna panel, AUTO / MANUAL | `muehle/hf/antenna-select/cmd` | yes | `{"request":"auto"|"manual"}`. |
| 15 | PA panel, OPERATE | `muehle/hf/pa/cmd` | no | `{"action":"set_mode","value":"operate"}`. |
| 16 | PA panel, STANDBY | same | no | `{"action":"set_mode","value":"standby"}`. |
| 17 | PA ARM panel | `muehle/hf/pa-arm/cmd` | yes | `{"action":"set_enabled","value":"true"|"false"}` — **value is the STRING `"true"`/`"false"`, not a JSON boolean**. |
| 18 | Tuner panel, BYPASS / inline | `muehle/hf/tuner/cmd` | no | `{"action":"set_inline","value":false|true}` — **value is a real JSON boolean**. |
| 19 | Tuner panel, TUNE MEM | same | no | `{"action":"tune","value":"mem"}` — value is a string. |
| 20 | Tuner panel, TUNE FULL | same | no | `{"action":"tune","value":"full"}`. |
| 21 | Compass tap-to-aim | `muehle/hf/rotator/cmd` | no | `{"action":"set_az","az":<double degrees>}` — **argument key is `az`, not `value`**. |
| 22 | Rotator presets NA / SA / VK / JA | same | no | `{"action":"set_az","az":330|210|60|35}`. |
| 23 | Rotator STOP | same | no (retain forced false) | `{"action":"stop"}`. |
| 24 | TRX band buttons | `muehle/hf/radio/cmd` | no | `{"action":"set_band","value":"80m"|"40m"|"20m"|"17m"|"15m"|"12m"|"10m"}`. |
| 25 | TRX DVK buttons DVK1–4 | same | no | `{"action":"dvk_play_1"…"dvk_play_4"}` (id clamped 1–12 in the builder). |
| 26 | TRX DVK STOP | same | no | `{"action":"dvk_stop","value":""}` (empty-string value stops the active memory — there is no stop-all behavior). |
| 27 | Mic-profile row | same | no | `{"action":"set_mic_profile","value":"<profile name>"}`. |

The command-builder library also defines more payloads. No shipped button wires them. They are part of the contract surface for completeness. `{"action":"frequency","freq_hz":<int>}` goes to `ant-ctrl`. The integer Hz value is a top-level key. That is the one documented exception to the value-key convention. The controller's capability descriptor exposes it. `{"action":"band","value":<band>}` goes to `ant-ctrl`. `{"action":"set_band","value":<band>}` goes to `pa`. `{"action":"fwd"}` and `{"action":"rev"}` go to `rotator`. The builder also supports `dvk_stop` with a specific memory id: the `value` field then carries the id as a string. An empty string (or no value) stops the active memory, resolved from the live `/state` id — there is no stop-all behavior.

The console silently drops all commands if the MQTT link is down. The widget level disables every command button when its owning slot is offline (two-layer liveness, §2.4).

**Related hard requirement (from a live incident, re-stated here because the console is the surface where it bites):** the PA arm relay de-energizes if its inputs do not refresh within 10 s. The enabling input is the radio's `/state`, and the radio bridge publishes `/state` only on change. When the radio is idle-but-healthy this starves the heartbeat. The interlock drops out. The operator sees the ARM panel flip to SAFE with no fault. Any reconstruction of the radio bridge must republish radio state at least every 5 s. Or it must give a liveness mechanism with the same effect — see `03-components/flexbridge.md` and `06-safety.md`. The console cannot fix this. It must merely survive it and render it truthfully.

### 2.9 DX-spot feed (independent SSE overlay plane)

The DX overlay is display-only. It runs **independently of the MQTT broker**. Loss of either feed must never disable the other. The overlay is active if and only if the configuration holds a station locator (§5). Without it the compass shows beam heading only. The map panels render without spot data.

#### 2.9.1 Transport

- Endpoint: `GET {baseUrl}/api/stream?qth={qth}&minutes=30&surroundings=true`. Here `qth` = the station callsign if configured, else the Maidenhead locator (both percent-encoded). `baseUrl` defaults to `https://horstreporter.kgbvax.net`. `minutes=30` makes the server replay the last 30 minutes of spots on each (re)connect. *(Code/comment discrepancy in the reference: source comments say `minutes=15`. The URL parameter in code is authoritative: 30.)*
- Protocol: SSE. Each spot arrives as one default (unnamed) event with a single `data:` line. The line payload is a JSON object.
  Parsing follows the HTML spec: a blank line dispatches. The parser strips one leading space. The parser joins multi-line `data:` with `\n`. The parser ignores `:` comments and `event:`/`id:`/`retry:` lines for data purposes. **But any received line resets the idle watchdog**.
- Request headers: `Accept: text/event-stream`, `Accept-Encoding: identity` (prevents gzip so the parser can split the stream by lines), `Cache-Control: no-cache`.

- **No keepalives**: the server sends `data:` only when a spot arrives. It sends no `: comment` heartbeat lines. The client therefore arms an **idle watchdog: 5 minutes without any received line forces the connection closed and a reconnect** (a half-open TCP connection otherwise never surfaces as an error). A **15 s connection timeout** bounds the connect phase. The client treats a non-200 status as a disconnect.
- **Reconnect/backoff**: the service layer owns this, not the transport. So all platforms behave the same way. The first delay is **2 s**. Each failure multiplies it by 1.5, capped at **60 s**. Any good event resets it to 2 s. The browser build must suppress its EventSource built-in auto-reconnect. So the backoff above governs everywhere. Losing SSE's `Last-Event-ID` resume is harmless. Every connection re-fetches the history window anyway.
- Consequences of the watchdog: on a genuinely quiet band the client reconnects every ≤ 5 minutes and re-ingests up to 30 minutes of spot history. Dedup (below) keeps this correct. The client repeats that work by design. Do not remove the watchdog without putting a replacement in its place. A half-open feed otherwise leaves stale dots forever (the 60 s prune timer is the backstop).

#### 2.9.2 Spot payload and ingest rules

One spot per SSE `data:` frame (the `streamSpot` shape):

| Field | Type | Meaning |
|---|---|---|
| `lat`, `lng` | number | **the remote (DX) station's position in degrees**, already resolved server-side — NOT the reporter's position. The console must place dots from these values. The locator serves only labels and grid squares. |
| `snr` | number → int | signal-to-noise ratio, dB (FT8/FT4 spots). |
| `ageSeconds` | number → int | how old the report was when the server emitted it. |
| `locator` | string | the remote station's Maidenhead locator (can be empty). |
| `band` | string | band label, for example `"20m"`. |
| `sourceType` | string | `"mqtt"` (FT8/FT4 through MQTT), `"dxcluster"`, `"rbn"`, `"wspr"`. |
| `sender`, `receiver` | string, can be absent | callsigns, dxcluster only. |

Ingest rules (exact):

- **R2.9.1** Only `sourceType == "mqtt"` spots (FT8/FT4) enter the store. The ingest drops dxcluster, rbn, and wspr spots. Rationale: the console's overlay intentionally mirrors horstreporter's azimuthal view band-for-band.
- **R2.9.2** The ingest drops spots missing `lat`/`lng`. It also drops spots missing all of locator+receiver+sender (nothing to place or label).
- **R2.9.3 Mode-aware SNR gate**, driven live by the radio mode (`muehle/hf/radio` `state.mode`): `usb`/`lsb`/`am`/`fm`/`data` → SSB family, threshold **0 dB**. `cw` → CW family, threshold **−15 dB**. Anything else (unknown/blank) → gating **off**. The ingest drops spots below threshold. Changing the filter does not clear current spots. A reconnect re-ingests history under the new threshold. The UI shows the active gate as a filter chip: `"SNR off"` / `"SSB ≥ 0dB"` / `"CW ≥ -15dB"`.
- **R2.9.4 Dedup key**: `"<locator>|<receiver??''>|<sender??''>|<band>|<sourceType>"`. On repeat the service keeps the freshest spot (lowest server `ageSeconds`). Higher SNR breaks ties.
  The service **always** refreshes the kept spot's local receive time. So an actively re-spotted station does not age out.
  Known limitation: the key ignores lat/lng. A station that reports a moved position under the same locator/sender/band keeps its first coordinates.
- **R2.9.5 Live age** of a spot = `ageSeconds` + seconds since local receipt. Max live age **600 s**. Enforcement happens at ingest and by a **60 s prune timer** that runs even when the feed is quiet.
- **R2.9.6 Cap 80 spots**, sorted by live age ascending, ties broken by SNR descending.
- **R2.9.7 UI notifications throttled to ≤ ~2 Hz (500 ms coalesce)** so a busy band cannot repaint-storm.
- **R2.9.8 Grid-square aggregation** (shared by both maps): the service groups spots by the first 4 characters of their uppercased locator. Per square the **dominant band** = the band with the most spots (first-max wins ties). The **score** = mean of the top quarter of SNR values (sort descending, take `ceil(n/4)` clamped to ≥1, average) — used for opacity. Squares with no band data do not contribute. Spots with <4-char locators do not contribute. Opacity ramp (exact): the ramp starts at 0.45 for a score of 0 dB. It falls to 0.15 at −10 dB. The slope is 0.03/dB below 0 dB and 0.015/dB above. Floor 0.10, cap 0.75.
- **R2.9.9 Fixed band palette** (NOT theme tokens — the same in all color schemes so a spot keeps its color across theme switches and matches the horstreporter web frontend): `160m #8B0000`, `80m #800080`, `60m #4B0082`, `40m #0000FF`, `30m #03B1B1`, `20m #008000`, `17m #808000`, `15m #FFA500`, `12m #00FFFF`, `10m #FF0000`, `6m #FF00FF`, `4m #FF1493`, `2m #008080`, unknown → grey `#555555`.
  Source-type fallback colors (spot with no band): mqtt→green, dxcluster→accent, rbn→amber, wspr→orange, other→muted text color.

#### 2.9.3 Projections and geometry (both maps share one spot service)

**AEQD (compass disc), exact math** (a port of horstreporter's own implementation so the two frontends' maps coincide). Earth radius 6371 km. Disc radius = `min(w,h) × 0.47`. Scale = `radius × zoom / π`. The math flips the y-axis. The implementation clips the projection 0.02 rad shy of the antipode. Horizon at 20015 km. Maidenhead decode: fields 20°×10° anchored at (−180,−90), squares 2°×1°, subsquares 5′×2.5′ with center offsets. The decoder supports 2/4/6-char locators. The cell center is the projected point. The configured station locator sets the AEQD center.

**Web Mercator (map panel), exact math**: standard EPSG:3857, spherical Mercator, earth radius 6378137, 256 px tile. The implementation clamps latitude to ±85.05112878°. It wraps longitude relative to the map center so antimeridian crossings render as a short jump. It detects antimeridian cuts in coastline rings by a |Δlng| > 180° break in the subpath. Minimum zoom = log2(height/256).

**Landmasses**: a bundled static world outline asset (Natural Earth 50m country polygons, ~1600 rings / ~100k vertices, ~3 MB). The app loads this asset lazily once. It never fetches it at runtime. A malformed asset must produce an empty coastline list, never a crash (the compass renders without coastlines rather than failing).

**Performance contract**: interactive zoom on tablet hardware must stay visually smooth. This criterion is deliberately unquantified further. The acceptance procedure is a manual zoom-drag test on the reference tablet with typical world-layer complexity. The animation must not visibly stutter. The reference gets this with a rasterized world-layer cache keyed on center/zoom/size/colors — the caching is implementation detail, the smoothness is the contract.

---

## 3. Panels and features

### 3.0 Pages and layout

Single-screen application with three tab pages — **Station / HF / UHF**. It adds a color-scheme picker (§6), a DX-settings gear (§5.3), an MQTT connection indicator, and an all-online tag. Top-level column: link banner (when down, §4.1) → page content → faults bar (§2.5 rendering).

- **HF page** (default). Tablet/landscape: two columns. Left: DX map container (fills), then Ultrabeam panel, Antenna panel, rotator presets. Right: top bar, then a scroll column of PA panel, PA-arm panel, Tuner panel, TRX/DVK panel, then the faults bar. Right-column width = 44% of viewport when ≥1200 px wide and ≥720 px tall, else 48%. Minimum 420 px (compact: 320 px). Phone (`shortestSide < 600`): single vertical scroll, DX map on top, then all panels in fixed order, then faults. The fixed order is Ultrabeam, Antenna, rotator presets, PA, PA ARM, Tuner, TRX/DVK.
- **Station page**: top bar, Power panel (§3.10) and Climate panel, scrolling.
- **UHF page**: placeholder text "UHF controls are not yet wired."
- The faults bar appears on every page.

**Top-bar indicators** (operator-safety surfaces):

- **Connection indicator**: 10 px dot, green glow + "MQTT" when connected, red + "OFFLINE" when not.
- **Online tag**: pill `● all online` (green) or `● N offline` (red), N = computed offline-row count (§2.5). Note N counts rows, including silent-slot rows. So N can exceed the physical device count.

**Boot sequence** (normative order):

1. Full-screen immersive mode. All orientations allowed.
2. Read all stored credentials/settings (§5). Configure and start the DX-spot service regardless of broker state (it no-ops when the configuration lacks a locator).
3. If host + parseable port + user + non-empty password are all present, the app tries auto-connect. On failure the console opens showing the link-down banner. If any part is absent, the app shows the setup screen instead (§5).
4. A store listener re-evaluates the DX SNR filter family on every bus update of the live radio mode.

### 3.1 TRX / DVK panel (radio)

Reads `muehle/hf/radio` (§2.6). Renders:

- **Readout chip**: frequency in MHz with 3 decimals (from `freq_hz`), mode uppercased (accent), band, TX/RX pill (red TX / green RX), drive %, DVK suffix when not idle: `PLAYBACK · M<n>` (accent) / `RECORDING`, `PREVIEW` (amber) / `DISABLED` (red). Offline → a red-bordered `OFFLINE` chip.
- **Band buttons**: `80m 40m 20m 17m 15m 12m 10m`, active one highlighted, each publishing `set_band` one-shot. (160m/60m/30m/6m are deliberately omitted for this station's antennas. A reconstruction must treat the list as configuration, not law.)
- **DVK buttons** DVK1–DVK4 publish `dvk_play_n`. A button highlights while `dvk_status == "playback"` and `dvk_id` matches. **STOP** (danger styling) publishes `dvk_stop` with empty-string value.
- **Mic-profile row** (three buttons bound by name to SmartSDR mic profiles, persisted on-device under keys `mic_profile_btn1`…`mic_profile_btn3`):
  - Tap a **bound** button → the console activates it (`set_mic_profile <name>`).
  - Tap an **unbound** button → a pick-from-list dialog opens. The list comes from `muehle/hf/radio.state.mic_profiles` (a sorted JSON array of profile-name strings that the bridge queries from the radio on connect). An empty list → informational dialog, no manual entry. Picking binds the button AND activates the profile.
  - **Long-press** → dialog with **Associate** (bind another existing profile without activating) and **Unbind** (bound buttons only).
  - The active name (`state.mic_profile`) renders as a `MIC: <name>` label. It highlights the matching bound button. The name stays empty until the first profile load arrives through the bus — SmartSDR reports no active-mic name, so the bridge tracks "last loaded".
  - There is deliberately no Save/edit. The operator creates and edits profiles in SmartSDR itself.

### 3.2 PA panel (power amplifier)

Reads `muehle/hf/pa` (§2.6) plus cross-slot `muehle/hf/switch.state.pa` (the remote-on relay) and the slot's own liveness. Renders:

- **FWD power meter**, full scale **1200 W**, bar labels `0 / 500 / 1000 / 1200`, green gradient into orange at the hot end. Two ballistic markers ride over the bar: a downward triangle at the **1-second rolling-window peak** (white). An upward triangle at the **rolling 95th percentile** (accent, linear-interpolation percentile). Markers snap up instantly. They then decay linearly at **24 W per 100 ms tick** (full-scale drain in ~5 s), never below the live reading. The decay timer runs only while a marker stands above the live value. A constant reading refreshes its sample timestamp so it stays "present" in the window.
- **SWR meter**, full scale **4.0**, labels `1.0 / 1.5 / 3.0 / 4.0`, amber fill. There is deliberately no reflected-power readout.
- Buttons **OPERATE** (green when active) / **STANDBY** (amber when active). The panel disables them offline. They publish `set_mode`.
- **Status tag priority** (cross-panel RF presentation. A live TX outranks relay bookkeeping): `OFFLINE` (muted) → fault/error text uppercased (red) → `● TX` (red) → `RELAY ?` (amber. The hf/switch relay state is unknown — a missing key must never read as OFF) → `PA RELAY OFF` (amber) → `PA OFF` (amber. The amp's own power telemetry) → `INHIBITED` (amber. The trigger is `keyed == "inhibited"`. It sits after the relay and power checks because a live TX still outranks it) → `OPERATE` (green) / `STANDBY` (amber). Each appends ` · {temp} °C` when temperature > 0.

### 3.3 PA ARM panel

Reads `muehle/hf/pa-arm`: `enabled`, `armed`, `error`. One toggle button labeled **ARM** (when disabled — danger styling) or **SAFE** (when enabled). It publishes `set_enabled` with the inverted value as a string. Tag priority: `OFFLINE` (muted) → error text (red) → `ARMED` (red — the interlock is hot, the amp can key) → `ENABLED` (amber) → `SAFE` (green). Arming goes through a retained command. So a pa-arm bridge restart restores the armed state.

### 3.4 Tuner panel (antenna tuning unit)

Reads `muehle/hf/tuner`: `inline`, `settling`, `fault`, `swr`. Buttons: **BYPASS** (active-amber when not inline), **TUNE MEM**, **TUNE FULL** — both tune buttons read **`TUNING…`**. The panel locks them while `settling` (a second tune queued against a settling tuner just competes with the in-flight one). BYPASS stays enabled while online. Tag: `OFFLINE` → fault uppercased (red) → `TUNING` (amber) → `IN LINE · SWR {x}` — colored by SWR: **≥3.0 red, ≥2.0 amber, else green** (3.5:1 and 1.1:1 must not read alike) → `BYPASS` (amber — a bypassed tuner is a degraded TX path, not neutral info).

### 3.5 Ultrabeam panel (beam direction)

Reads `muehle/hf/ant-ctrl` (`direction`, `moving`, `band`) and `muehle/hf/radio` (`band`). Header pill priority: `OFFLINE` (red) → `MOVING` (red) → `DIRECTION` uppercased (accent) → `BAND MISMATCH · <ctrlBand> ≠ <radioBand>` (red). The mismatch check fires only when the slot is online AND both bands are "comparable". Comparable means: non-empty, not `gen`/`unknown` (the radio bridge's out-of-allocation labels), not starting with `band-` (the antenna controller's), and different. (The two bridges label unknown frequencies differently. They can agree on frequency while disagreeing on label. Pre-first-state silence must not cry wolf. The author tuned the comparability heuristic to the two deployed bridges' vocabularies.)

Buttons: **FORWARD**, **180°** (reverse), **BI-DIR**, **RETRACT** (danger styling).

- All four need the slot online.
- The panel locks the direction buttons while `moving` (rapid presses cannot queue competing motor commands mid-travel).
- **RETRACT stays pressable while moving** — it is the designated emergency action.

**6 m auto-correction** (the one documented console-initiated automatic command, a device-physics guard): on the 6 m band the Ultrabeam's elements support only forward radiation. Therefore (a) the console disables the 180° and BI-DIR buttons while the radio is on 6 m. And (b) if `direction != forward` while the radio is on 6 m and the controller is online and not moving, the console itself auto-publishes `direction=forward` **once per invalid state**. A latch flag prevents republish-per-rebuild. The flag clears when the state resolves or while moving. So the correction re-fires after travel if the state is still invalid. The auto-correction obeys the same moving lockout as manual buttons. It fires without operator confirmation by design.

Personality note: a band-heckling-dragon mascot image sits beside the RETRACT button — a deliberate bit of station personality. Keep it or drop it. Operators recognize it.

### 3.6 Antenna routing panel

Reads `muehle/hf/ant-switch` (`selected`, `settled`), `muehle/hf/antenna-select` (`mode`), plus cross-panel RF state from `muehle/hf/radio` (`tx`, `tuning`, `device_online`) and `muehle/hf/pa` (`keyed`).

**Port→label map** (source: the antenna-selection reconciler's wiring map). Ports 2 and 3 have no wiring at this site, and the UI leaves them out:

```
off → Grounded        port1 → Dummy load      port4 → Ultrabeam
port5 → Port 5         port6 → Fan dipole 80/40
```

> **CONFLICT (open decision, see §7):** the repo-root documentation table places the Ultrabeam on switch **port 3**. The console's map and the reconciler's example config say **port 4**. The deployed wiring on shari is authoritative. But the author lacked read access when writing this PRD. A reconstruction must resolve port 3 vs port 4 on-device before it finalizes the map. The fan dipole's port is also contested (6 or 2 — see §7). The console's behavior is otherwise the same (dummy load = port 1 is consistent everywhere).

Rendered port buttons: `off, port1, port4, port5, port6`. A missing or unknown `selected` value renders **`?` (Unknown)**. It must never render as a port, least of all `off`, because `off` paints a dead bridge as a deliberate grounded-safety state.

Header: `"{modeLabel} · {antennaName}"`. modeLabel is the reconciler mode uppercased if that slot is online and reports a mode. Else modeLabel is **`DIRECT`** (never invent "AUTO" when nobody enforces the policy). Color: red when blocked (grounded or manual override), accent when managed and settled, amber when managed-but-not-settled or unmanaged. More tags follow. While relays are in flight (`settled == false`) an amber **`NO RF`** tag shows plus a pulsing amber dot (700 ms animation). **`RF ON`** (red) appears whenever RF shows up anywhere. **`RF ?`** (amber) appears when direct-drive is active and RF state is unknown.

**Routing command selection** (normative):

- If the reconciler is online and its mode is not `manual`: taps publish `{"request": <port>}` to `antenna-select` — the policy layer, which enforces RF-inhibit ordering itself. This path is **exempt from the console-side RF guard**.
- If the reconciler is in `manual` mode, or offline/absent ("direct drive"): taps publish `{"select": <port>}` directly to `ant-switch`. The console **blocks them unless RF-safe** (§4.3).
- The AUTO / MANUAL mode buttons publish `{"request":"auto"/"manual"}` to `antenna-select`. They stay enabled only when that slot is online.

### 3.7 Rotator compass panel

Reads `muehle/hf/rotator` (`az`, `target_az`, `moving`) and `muehle/hf/ant-ctrl` (`direction`). Draws a circular compass disc:

- Ticks every 30°, majors every 90°. Cardinal labels N (accent) / E, S, W (muted).
- **Beam lobes** from the Ultrabeam direction: forward = one wedge of half-width 30° at `az` (accent). Reverse = one at `(az+180) % 360` (amber). Bidirectional = both, each half-width 45°. Each lobe = filled wedge + radial line + arrowhead at the rim.
- White boom line from center to `az`. Accent center dot.
- **Target line**: semi-transparent accent line at `target_az`. The painter draws it only when `|target_az − az| > 5.0°`. The header chip likewise shows the target (`"123° → 330°"`) only beyond 5°.
- Chrome: top-left DX status badge (`"DX 42"` spot count / `"DX …"` connecting / `"DX ✗"` feed down — hidden entirely when the overlay is off). Top-right azimuth chip built from `["<az>°", "→ <target>°"?, "MOVING"?, "OFFLINE"?]` — red while moving, muted while offline, accent otherwise. A zoom badge. A band-key rail sits on the left. It lists only the bands present in current spots, in canonical order (160m, 80m, 60m, 40m, 30m, 20m, 17m, 15m, 12m, 10m, 6m). Each chip shows a swatch of the fixed band color (§2.9.2). Chips fade in/out over 180 ms.
- **Zoom**: range **[1.0, 5.0]**, default **1.5**, step **0.2**. NaN→default. ±∞ clamps to the bound. The controls disable the buttons within 1e-9 of the bounds. Controls: stacked +/− buttons bottom-right. And a vertical drag on the disc's **right gutter** (right half, inside the disc radius). Drag up = zoom in, ~30 px per 1.0 zoom step. This gesture is available only while the DX overlay is active.
- **Tap-to-aim**: a tap anywhere on the disc (while the rotator slot is online) converts the tap angle to a compass azimuth `((atan2(dy,dx)·180/π + 90) mod 360 + 360) mod 360` and publishes `{"action":"set_az","az":<deg>}`. When the rotator is offline the console disables the disc gesture. The chip reads OFFLINE.
- **DX overlay on the disc**: AEQD-projected landmasses (§2.9.3) plus 4-char grid-square quads. The painter fills each quad with the dominant band color at the SNR opacity and strokes it 0.6 px. Grid squares render at ANY projected size (they must not vanish at low zoom). The only skips are corners off-disc (antipode wrap) or centroid off-disc.

**Rotator presets bar** (separate card): `NA 330`, `SA 210`, `VK 60`, `JA 35` (NA = North America, SA = South America, VK = Australia, JA = Japan — ham country prefixes). Each publishes `set_az`. `STOP` publishes `{"action":"stop"}` (retain forced false). The console disables all five when the rotator slot is offline.

### 3.8 Mercator DX map panel

The map area hosts two interchangeable projections sharing one spot service (§2.9). Icons top-right toggle them (compass = azimuthal, map = Web Mercator). The SNR filter chip sits with them.

**Mercator panel**: pan by drag (the map moves opposite the drag), wheel/scroll zoom. Zoom range **[1.0, 12.0]**, default **2.5**, step **0.5**. The controls disable the buttons within 1e-9 of bounds. A crosshair reset button re-centers on QTH and restores zoom 2.5. The view stays pinned to QTH until the user pans. Paint order, back to front: page-colored background. Land fill + coastline stroke (even-odd fill so GeoJSON hole rings subtract. Antimeridian handling per §2.9.3). Grid-square quads (dominant-band fill at SNR opacity, +0.2-opacity stroke). Spot dots (radius 2.5, band color, 0.6 white contrast stroke). And a QTH marker (accent dot, radius 5, page-color ring).

### 3.9 Station power / sequencer panel

Reads `muehle/power/master.power`, `muehle/power/psu-13v8.power`, `muehle/hf/switch.trx`/`.pa`, `muehle/hf/power-seq.phase`/`.fault`, and each slot's liveness. Layout: **START STATION** (green styling) · **STOP STATION** (danger) · four relay toggles · SEQUENCE readout.
Relay toggles: MAINS, PSU 13.8V, TRX, PA. ON shows green. OFF shows faint. OFFLINE shows red text. Each toggle works only when its slot is online. An offline toggle carries a tooltip "`<name>` offline — relay uncontrollable".

- **START gating**: enabled only when the power-seq slot is online AND `phase` is NOT `running` AND NOT `starting`. (Known defect carried forward: phase `stopping` leaves START enabled — a tap mid-shutdown sends `{"action":"start"}` during the stop sequence. The sequencer tolerates it. A reconstruction must decide whether to guard it. §7.)
- **STOP**: the console enables it whenever the power-seq slot is online.
- Sequence label: `FAULT` (red, if `fault` non-empty) / `ON` (green, running) / `STARTING` (green) / `STOPPING` / `IDLE` (muted).

The sequencer itself (ordered startup: mains → PSU → TRX → PA → arm, shutdown in reverse, paced on real liveness confirmations) is a server component — see `03-components/powerseq.md`. The console only starts/stops it and renders its phase.

### 3.10 Climate panel

A **static placeholder**: hard-coded `HEAT` (on), `COOL` (off), `21.4 °C`, `612 ppm`. No bus topics, no commands. HVAC control connects to nothing anywhere in the station. The panel exists to reserve layout. It must not imply telemetry that does not exist. A reconstruction can drop it or clearly mark it "not wired".

---

## 4. Operator-safety UI (consolidated contract)

These rules are the console's core reason for existing. They are testable requirements. Each traces to a real incident or a deliberate design decision.

- **R4.1 LINK DOWN banner.** While the broker connection is down, a full-width red strip renders at the very top of every page. The copy is exact:

  > `LINK DOWN — DATA STALE · COMMANDS NOT DELIVERED`

  It disappears the instant the link returns. The panels do NOT disable taps one by one. Commands simply do not arrive, and the banner promises exactly that. (Consequence: a user can tap with no per-press feedback. The banner is the only warning. A reconstruction can add per-press feedback. It must not remove the banner.)

- **R4.2 No command while offline.** The console disables every command button unless its owning slot passes two-layer liveness (§2.4).

- **R4.3 Fail-closed cross-panel RF guard (cold-switch).** The console must refuse a direct-drive antenna-switch port change (§3.6 manual/reconciler-offline path) unless the RF check confirms safe. RF reads as "on" if ANY of three independent paths says so: radio `tx == "tx"`, radio `tuning == true`, PA `keyed == "tx"`. RF counts as "safe" only if the radio link is up AND `tx == "rx"` AND `tuning != true` AND `keyed != "tx"`. **Unknown blocks** (fail closed). Confirmed RX allows the change. Rationale: transmit power on an antenna relay arcs its contacts. The reconciler path (auto mode, reconciler online) is exempt because the reconciler arbitrates inhibit ordering itself.

- **R4.4 Never invent OFF (or any definite state) from missing data.** A missing `hf/switch.pa` shows `RELAY ?`, never "PA RELAY OFF". A missing PSU `power` key never triggers the PSU root-cause line. A missing `ant-switch.selected` shows Unknown, never Grounded. An absent selector never shows "AUTO".

- **R4.5 Red means "shouting", not "error", in these states**: the GROUNDED port button renders solid red even while active (grounded = no antenna connected = operating impossible). The MANUAL override button renders solid red while engaged. ARMED renders red (the interlock is hot). RETRACT and danger actions use red-tinted styling.

- **R4.6 Stale data must be loud.** Offline rows carry a stamp of when each slot went dark (§2.5), never render time. The console reports silent expected slots after the 3 s connect grace. In the PA tag a live TX outranks plumbing.

- **R4.7 Motion/settling discipline.** The console locks Ultrabeam direction commands while moving (RETRACT excepted). It locks tuner tune commands while settling. It disables START STATION while the sequencer reports running/starting.

- **R4.8 The DX overlay never interferes with control.** It shows read-only content. Tap = aim, right-gutter vertical drag = zoom. The gestures do not overlap.

- **R4.9 Band colors stay fixed across themes** (§2.9.2) — the horstreporter correspondence is a cross-component visual invariant.

- **R4.10 Payload shapes are contracts** (§2.8): a reconstruction must reproduce the value-key deviations exactly. Deployed bridges parse them as-is. The console project does NOT re-implement them.

---

## 5. Setup and configuration

The user enters all configuration at runtime. The device stores it per-key. There is no config file, no build-time config, no command-line flags.

### 5.1 Stored keys

| Key | Meaning | Default | Storage |
|---|---|---|---|
| `mqtt_host` | Broker host. Form default `192.168.1.50` (see §5.4). | — | secure storage. |
| `mqtt_port` | Broker TCP port (stored as string). Save falls back to `1883` on a parse failure. | — | secure storage. |
| `mqtt_user` | MQTT username — the dedicated low-privilege `console` account (ACL: subscribe `muehle/#`, publish `muehle/+/cmd`). Deliberately NOT the broad `hf` user. The app then embeds no station-wide credentials. | — | secure storage. |
| `mqtt_password` | Broker password — must be non-empty. An empty password skips auto-connect and shows the setup screen. | — | secure storage. |
| `station_locator` | Station Maidenhead locator. The console uppercases it on save. Presence enables the DX overlay and sets the AEQD center. Empty = overlay off. | — | secure storage. |
| `horstreporter_base_url` | SSE feed base URL. | — | secure storage. |
| `station_callsign` | Station callsign. The console reads it at startup. No UI writes it (**dead key** — §7). The console prefers it as the SSE `qth=` parameter over the locator. | — | secure storage. |
| `mic_profile_btn1..3` | Mic-profile button bindings. | unbound | plain device preferences. |

Defaults (`mqtt_host` form default `192.168.1.50` per §5.4, `mqtt_port` `1883`, `mqtt_user` `console`, `horstreporter_base_url` `https://horstreporter.kgbvax.net`) stay in the table above by key.

**Secrets handling (normative):** on mobile the platform's encrypted credential storage must hold the credential keys (Android Keystore / iOS Keychain). On web no such storage exists. The reference falls back to plain browser-local storage. So the broker password sits in the clear on the web channel. This is a known accepted trade-off on a trusted LAN. A reconstruction must either preserve it knowingly or explicitly improve it (for example per-session password entry on web). It must not silently worsen it.

### 5.2 First-run credentials screen

The console shows this screen if and only if any of `mqtt_host` / `mqtt_port` / `mqtt_user` / `mqtt_password` is absent. The password must be non-empty when present. A centered "MÜHLE · HF" card shows fields Host, Port, Username, Password (obscured, eye toggle) and a DX-overlay section the user can skip: Station locator and Horstreporter URL. CONNECT saves all six values (locator uppercased). It tries a connect (failure swallowed — the console opens with the banner). It configures and restarts the DX service. Then it switches to the console. Validation: empty host, empty user, or empty password ⇒ the save silently refuses.

### 5.3 Why the setup screen is unreachable after provisioning, and the gear workaround (normative)

Boot routes to the setup screen only when a credential is absent. Once provisioned, the device boots straight to the console **forever**. There is deliberately no in-app settings page for broker credentials. They live only in secure storage. Changing them needs a storage clear or a reinstall. The reasons are deliberate:

1. The tablet is a fixed-mount appliance. A settings page invites accidental misconfiguration of a safety-relevant connection.
2. The DX-overlay keys came later and needed a reachable editor. So the top-bar gear ("DX overlay settings") opens a modal sheet editing **only** `station_locator` and `horstreporter_base_url` (same defaults, locator uppercased on save). Saving persists both keys and **live-applies** them to the running spot service (configure + restart) without touching the MQTT link.

This lockout is operationally awkward when the broker moves (see §5.4). And it is a reconstruction decision point. But the behavior above is the specified contract. If a reconstruction adds an in-app broker-credential editor, the editor must sit behind a guard (for example a confirmation). That is a deviation to document, not a silent improvement.

### 5.4 Broker topology: default and planned migration

- **Current production**: a single MQTT broker at `192.168.1.50:1883` (the house Home-Assistant broker). The console's setup-screen default and the web proxy's broker flag both point there.
- **Planned, committed but NOT deployed** as of 2026-08-29: a shack-local broker on shari (`192.168.1.139:1883`, authoritative for `muehle/#`) bridged to the house broker. So the station keeps its bus when the shack↔house link drops. That work repoints the console's default host and the web proxy's default broker to `192.168.1.139:1883`.
- The console itself is topology-agnostic: the broker host is a config field. The only web-channel difference is the transport (direct TCP vs the WebSocket proxy). A reconstruction must treat the topology as a deployment decision (see `00-system-overview.md` and §7). It must not hard-code either address into application logic. (The reference web build hard-codes its WebSocket URL — a known LAN-IP brittleness. A reconstruction can improve that.)

> **Doc/code disagreement (evidence):** the console project's own README/CLAUDE docs already describe the shack broker at `192.168.1.139:1883`. But the examined code defaults to `192.168.1.50:1883`. The code is authoritative for current production. The docs describe the pending migration.

### 5.5 Hard-coded values (not user-configurable)

The app ships these values hard-coded. The user cannot change them in the app:

- the world-outline asset, the fixed band palette, the mascot image, and the bundled fonts.
- the 15-slot expected list and the antenna port map (pending the §3.6 conflict).
- the web WebSocket endpoint and keep-alive 20 s.
- the silence grace 3 s, SSE `minutes=30` + `surroundings=true`.
- DX max age 600 s, 80 spots, backoff 2–60 s, throttle 500 ms, prune 60 s.
- the SSE connect timeout 15 s and the idle watchdog 5 min.
- the fault-history cap 30 and the DVK id clamp 1–12.

---

## 6. Theme, visual language, and design lineage

### 6.1 Color schemes

Three user-switchable schemes. The user can switch at the top bar at any time (buttons DC / PA / FO). Exact reference values (a reconstruction can refine the hex values, but dark-first, near-black cards, monospace readouts, and the semantic assignments are the visual contract):

- **dc** (default, dark): page `#0A0C10`, pane `#0F1218`, card `#151923`, land `#232C3E`, lines `#2A3142`/`#3D4558`, text `#DEE3EC` (normal) / `#8C97AB` (muted) / `#5B6579` (faint), accent cyan `#5FB2C9`, green `#5CCB8A`, amber `#D7B04A`, red `#D9685C`, orange `#D99A5F`.
- **paper** (light): warm-grey page `#E5E3DD`, card `#F9F8F5`, ink `#1A1A1A`, accent `#005F99`, green `#1E6B48`, amber `#A36A00`, red `#B52E1D`, orange `#C45F18`.
- **forest** (dark green): page `#151A17`, accent gold `#C8A45C`, green `#6DB88B`.

Semantics are invariant across schemes: accent = active/informational, green = healthy/allowed, amber = degraded/attention, red = danger/offline/blocked/live-TX, orange = hot-end of the power meter.

### 6.2 Typography and controls

- Fonts: a condensed display face for headings, a humanist sans for body, and a monospace face for **all data, readouts, labels and buttons**. The fonts ship as bundled assets. The app never fetches them at runtime (the console must work with no internet beyond the LAN).
- Buttons: flat, 4 px radius, 1 px border. Pane background when inactive. Accent fill with black/white foreground when active. Red-tinted for danger. **Solid red fill** for "dangerActive" states (grounded, manual override, armed).
- Minimum touch target 44×40 (48×48 for icon buttons), with two documented deliberate exceptions (rotator preset buttons, compass zoom stack). A reconstruction must consciously re-decide these.
- A purple-bordered card variant marks the station-power panel.
- Type scale for width: ≥1400 px → ×1.25, ≥1200 px → ×1.1.
- All decorative animations (the 700 ms pulsing dot, the 180 ms fades, the PA marker decay) must respect the platform reduced-motion setting when the platform supplies one. Under reduced motion the console must show a static presentation that stays legible.

### 6.3 Design lineage (sas reference note)

The visual/interaction exploration lives in the repo under `sas/` as static HTML mockups. It is **design input, not software**. Two rounds exist:

1. **Information-architecture directions** (from the design brief — a fixed-mount tablet where every action is one tap, readable in dim light. Explicitly forbidden: neon on black, glassmorphism, thin trendy fonts, "corporate dashboard" looks). The directions: **A "Shop Panel"** (chunky industrial grid), **B "Logbook Strips"** (full-width ruled strips), **C "Split Deck"** (persistent left world-pane + switching right command pane). The brief also fixed console requirements that are behavior contract regardless of visuals: every action one tap. A shared fault strip at the bottom. Offline controls grey out with a reason. Interlocks block dangerous actions with no confirmation dialogs (rejected actions surface as faults). Dark + light themes. (The later mockups relaxed the brief's original "no frequency readout" constraint. They show band/mode/frequency in the top bar — the later artifact wins.)
2. **Four high-fidelity tablet previews**:
   - **Airbus / process-control** — flight-deck aesthetic. Every state color gets a dim tinted "annunciator tile" box. So alarms read as lit instrument tiles.
   - **DCs / dark-dense** — near-black SCADA look (SCADA = industrial control-room software, here describing the visual style), single cyan accent, maximum information density (datablock grids, monospace everywhere).
   - **Exact design-system** — consumer-grade tokenized system (brand purple, 16 px card radii, pill controls). This direction risks drifting toward the forbidden corporate-dashboard look the most.
   - **Hybrid (DCs + at-a-glance grid)** — the synthesis: the DCs palette and typography combined with an at-a-glance grid layout. This is the most complete artifact and the approved high-fidelity reference.

All four previews agree on a **shared content model**. This model is the requirement, because the direction can otherwise change. The content model: top-bar station identity + radio context + band strip. A banner row for beam-direction warnings. Compass with beam-lobe wedges + target line + presets. PA block with meters + OPERATE/STANDBY. Tuner block with TUNE MEM / TUNE FULL / inline state. Antenna port row + AUTO + direction segment + RETRACT. Station-power card with sequencer phase + relay toggles. A climate placeholder. A bottom RX/TX/INHIBITED-style status strip. Interaction rules from the handoff are behavior contract for the console: one-tap actions. Shared fault strip. Offline greying-with-reason. Interlocks-not-confirmation-dialogs. Dashed target line hidden within 5° of target. Sequencer pacing on real liveness confirmations.

Two further handoff rules are **conscious deviations** in the shipped console, not silent drops. First, the handoff disables manual relay toggles while a sequence is in progress. Second, it chains an open-loop supply cascade: mains off forces PSU, TRX, PA, and arm off. The shipped console implements neither rule. Its relay toggles gate on slot liveness only (§3.9). Sequencing and the cascade belong to the server side (see `03-components/powerseq.md` and `06-safety.md`). A reconstruction must record its own choice for both rules.

---

## 7. Open decisions and unresolved facts

Each item lists the evidence for the variants. None is silently resolved.

1. **Ultrabeam switch port: 3 or 4.** Repo-root documentation and the integration model say switch port 3. The console's port map (`hf_console/lib/store/wiring.dart`), the reconciler's example config, and its deploy seed say port 4. The deployed `/etc/antennaselect/config.toml` on shari is authoritative. But the author lacked read access from the workstation when writing this PRD. The team MUST confirm it on-device. The fan dipole's port is also contested: every source above says port 6, but the integration model's own passive-resource list says port 2. Dummy load (port 1) is consistent everywhere.
2. **Broker topology: one broker or two.** Deployed code defaults point at `192.168.1.50:1883`. The team committed the shack-local-broker migration (shari `192.168.1.139:1883` bridged to `.50`) on an unmerged feature branch. Nobody deployed it as of 2026-08-29. The console is topology-agnostic. Only defaults and the web proxy's broker flag differ. A reconstruction must pick the target topology before it fixes defaults.
3. **`device_online` form.** The integration model says writers can omit the key when true. Deployed bridges publish explicit `device_online: true`. Consumers (this console) treat absence-as-true once a state snapshot has arrived. Whether a reconstruction mandates explicit-true everywhere is open. See `02-interface-spec.md`.
4. **Chosen design direction.** The hybrid preview is the approved high-fidelity reference (per the console project docs). The shipped app is the hybrid's split-deck skeleton rendered in the DCs dark visual language. But the shipped Flutter theme's concrete values diverge in detail from every mockup. A reconstruction must decide whether "hybrid preview" or "shipped app" is the visual target of record.
5. **`station_callsign` is a dead key.** The console reads it at startup. When present, the console prefers it as the SSE `qth=` parameter over the locator. But no UI on the examined branch writes it. Either wire an editor for it (for example in the gear sheet) or drop it. The spot feed's `qth` parameter then always carries the locator.
6. **SSE watchdog churn.** The 5-minute idle watchdog guarantees disconnect/reconnect cycles on quiet bands. Each cycle re-ingests up to 30 minutes of replayed history. Dedup makes it correct, but the client repeats the work. The reference source comments even disagree with themselves about the replay window — 15 vs 30. The code URL parameter 30 is authoritative. Options: keep both watchdog and replay window (reference behavior), shrink the replay `minutes`, or negotiate a real keepalive with the spot service. Any change must keep "no stale dots on a half-open feed".
7. **First-connect failure behavior.** The reference swallows a failed first connect. It relies on library auto-reconnect that arms only after a first successful connection. A console booted before the broker is reachable can stay offline until an operator restarts it. A reconstruction must define an app-level retry (for example periodic reconnect attempts). Or it must accept and document the same limitation.
8. **START enabled during phase `stopping`.** The shipped START STATION button gates on `running`/`starting` but not on `stopping`. So a tap mid-shutdown sends `{"action":"start"}` during the stop sequence. The sequencer tolerates it. Whether the console must also gate `stopping` is open.
9. **Web-channel credential storage.** The web build keeps the broker password in plain browser-local storage (no secure storage exists in browsers). The team must decide consciously: preserve as a trusted-LAN trade-off, or replace with per-session entry. Document the decision.
10. **`dvk_status` value set.** Only `idle` appears in the console's own fixtures. The radio bridge owns the full vocabulary (`playback`, `recording`, `preview`, `disabled`). The author lacked a way to enumerate it from the console's code alone.
11. **Climate panel.** Hard-coded placeholder values (21.4 °C, 612 ppm) imply telemetry that does not exist anywhere in the station. Keep it as a labeled placeholder, or drop it.
12. **Faults bar render-time fallback.** The store contract says offline stamps must come from the store, never render time. The shipped UI keeps a render-time clock as a last-resort fallback when a stamp is absent. A reconstruction must define the fallback (or make it impossible). Do not inherit the contradiction silently.
13. **Mic-profile list transport.** The `mic_profiles` field arrives on the bus as a sorted JSON array of profile-name strings. (The caret-delimited form is the vendor protocol reply that the radio bridge parses. The console never sees that form.) SmartSDR reports no active-mic name. So `state.mic_profile` is the bridge's client-side "last loaded" tracking. Any reconstruction of the radio bridge owns this contract. The console only renders it.
14. **`antenna-select` mode value set.** The console fixtures exercise only `auto` and `manual`. Values beyond these two are unknown to this PRD. The antenna panel header uppercases whatever the slot reports while online. So an unexpected value must degrade gracefully there. A reconstruction must define how unknown mode values render.

> Reference-implementation note (non-normative): the shipped app is `hf_console/` (Flutter). Packages include an MQTT client, secure storage, provider, clock. `05-deployment-ops.md` covers the deployment details. These are the Android APK sideload, the iOS self-sideload, and the `webbridge` Go proxy with its hardened systemd unit on shari (port 8091, dedicated no-login user, `MemoryMax=64M`). The release-manifest rule also lives there: the `INTERNET` permission and the cleartext-traffic allowance go into the main manifest. Those details are not normative for behavior.