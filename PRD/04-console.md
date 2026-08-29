# 04 — Operator console (hf_console)

> **PRD document 04 of the Mühle station-automation reconstruction.**
> Audience: a competent software engineering team that knows **nothing** about amateur radio,
> MQTT culture, or this repository. Every specialized term is defined at first use. This
> document is a **feature/behavior specification from the operator's perspective** with exact
> data dependencies — topics, JSON payload shapes, units, timings, and error behavior. It is
> not a widget-by-widget description of the reference application.

**Purpose.** The operator console is the single human interface to the Mühle HF station: a
fixed-mount tablet on the operating desk that (a) shows the live state of every station
subsystem, (b) turns every operator action into one small JSON command on the station
message bus, and (c) — most importantly — never lets stale data, a dead link, or an unknown
value masquerade as a healthy station. The console holds **no station logic of its own**:
sequencing, antenna arbitration, and interlocks live in server-side components (see
`03-components/powerseq.md`, `03-components/antennaselect.md`); the console renders, guards
operator input at the UI level, and fails closed on RF-safety questions. This document
specifies the console's two upstream feeds (the MQTT control bus and an independent HTTP
spot feed), its in-memory domain model, every panel and command, the operator-safety
presentation rules, configuration, and the visual design language.

Cross-references: `00-system-overview.md` (station glossary and slot table),
`02-interface-spec.md` (bus planes, `/cmd` value-key convention, liveness model),
`06-safety.md` (station-wide interlock rationale), `05-deployment-ops.md` (shari, the
Raspberry Pi that runs all server-side services).

---

## Glossary (plain-language definitions, first use applies throughout)

- **Amateur radio ("ham radio")**: the licensed, non-commercial hobby of two-way radio
  communication; operators exchange contacts ("QSOs") worldwide using identifying
  callsigns.
- **HF**: "high frequency", the 3–30 MHz shortwave radio spectrum used for long-distance
  communication. The Mühle station's HF chain is the subject of this console.
- **MQTT**: a lightweight publish/subscribe protocol. Clients publish messages to
  hierarchical slash-separated **topic** strings (e.g. `muehle/hf/pa/state`); a central
  **broker** relays them to subscribers. A **retained** message is stored by the broker
  and re-delivered to every future subscriber until overwritten or cleared by publishing a
  zero-length message to the same topic. **LWT** (last will and testament) is a message the
  broker publishes on a client's behalf if that client drops without disconnecting cleanly.
  **QoS 0** = at-most-once delivery; **QoS 1** = at-least-once delivery.
- **Slot**: one addressable station component on the bus, e.g. `muehle/hf/pa` (the power
  amplifier's bridge). Each slot owns four topics called **planes**: `/meta` (static
  identity), `/state` (a retained JSON telemetry snapshot, republished whole on every
  change), `/status` (bridge-process liveness, the bare string `online`/`offline`, driven
  by MQTT LWT), and `/cmd` (commands addressed to the slot).
- **Bridge**: the server-side program on shari that translates one physical device's
  protocol onto its slot.
- **TRX / transceiver**: the radio itself (a FLEX-8400).
- **PA / power amplifier**: a device that boosts the radio's transmit signal (an ACOM
  1200S, up to 1200 W).
- **ATU / antenna tuning unit ("tuner")**: an impedance-matching network placed in the
  feed line (an ATR-1000); "inline" = in the RF path, "bypass" = out of it.
- **SWR (standing-wave ratio)**: feed-line impedance match quality; 1.0 is perfect, ≥3.0
  is dangerous (reflected power heats the PA).
- **Rotator**: the motor that turns the mast-mounted beam antenna; position is an
  azimuth in degrees.
- **Ultrabeam**: a motorized tubular triband beam antenna whose element lengths are
  motor-tuned per band; it supports forward / 180°-reverse / bi-directional radiation
  patterns and full retraction.
- **DVK (digital voice keyer)**: memory slots in the radio that play back recorded audio
  (e.g. contest exchanges).
- **FT8/FT4**: narrow-bandwidth weak-signal digital modes; stations exchange automated
  contacts that are reported as **spots**.
- **DX spot**: a report that a distant ("DX" = long-distance) station was heard — who,
  where, band, and signal-to-noise ratio (SNR, in dB).
- **Maidenhead locator / grid square**: a geographic encoding used in amateur radio.
  2 characters = a "field" (20° longitude × 10° latitude); 4 characters = a "square"
  (2° × 1°); 6 characters = a "subsquare" (5′ × 2.5′). Example: `JN58sd`.
- **QTH**: ham shorthand for "station location" (the station's own location).
- **AEQD (azimuthal equidistant projection)**: a map projection centered on one point
  where every other point appears at its true bearing AND true distance from the center —
  exactly what a beam-antenna compass needs.
- **horstreporter**: the station-owner's own external web tool that aggregates DX spots
  (FT8/FT4 via MQTT, dxcluster, RBN, WSPR) and serves them over
  **SSE (Server-Sent Events)** — a long-lived HTTP response stream of `data: <text>` lines
  where a blank line ends an event.
- **SmartSDR**: the FLEX radio's own software; "mic profiles" are named transmit-audio
  configurations stored in the radio.

---

## 1. Purpose, role, and platforms

### 1.1 Role

The console is a **pure MQTT consumer + commander plus one read-only HTTP feed**:

1. It subscribes to `muehle/#` on the station MQTT broker and mirrors everything it hears
   into a local in-memory slot store (§2.3).
2. It publishes JSON command payloads to `muehle/<slot>/cmd` topics when the operator taps
   controls (§2.8).
3. Independently of MQTT, it opens an SSE feed to horstreporter and renders DX-spot
   positions on two map projections (§2.9).

It has **no bus presence of its own**: no slot, no `/meta`, no `/state`, no LWT, no
heartbeat, no periodic publish of any kind. Its only footprint is the `/cmd` messages it
publishes and its broker session. A reconstruction SHALL NOT add console presence topics,
heartbeats, or retained console state — other components must not come to depend on the
console being alive.

The console SHALL NOT implement station logic. It renders and commands; it never
sequences, arbitrates, or caches policy. The single documented exception is the 6 m
forward-only correction (§3.5), a device-physics guard mirrored from the antenna
controller's own web UI.

### 1.2 Platforms (normative)

Three delivery targets of one application, all sharing one behavior contract:

1. **Android tablet** (primary). Fixed-mount on the operating desk, full-screen immersive
   mode, all orientations allowed (layout, not orientation locks, decides the shape).
2. **Phone (iOS)**. Same application, single-column reflow (breakpoint: shortest side
   < 600 px).
3. **Web, served from shari over the LAN.** Browsers cannot open raw TCP sockets, so the
   web build connects to the broker over a WebSocket that a small server-side proxy
   (`webbridge`) byte-forwards to the broker's TCP port. The proxy performs no MQTT
   interpretation — the browser's MQTT client speaks raw MQTT packets through the tunnel.

> **Reference-implementation note (non-normative).** The reference is a Flutter
> application (Android APK sideloaded via ADB, self-sideloaded iOS IPA, Flutter web
> build). The web proxy (`webbridge/main.go`, Go + gorilla/websocket) serves the static
> Flutter build at `/` and exposes `/mqtt` as a byte-forwarding WebSocket↔TCP proxy on
> port 8091, deployed as a hardened systemd service `hf-console-web` on shari under a
> dedicated no-login user. Raw-TCP MQTT from a mobile app does not trip iOS App Transport
> Security (which governs only HTTP/WebSocket sessions), so no ATS exception is needed.
> Any equivalent per-platform transport selection is acceptable provided the §2 contracts
> hold.

---

## 2. Domain-layer contracts

### 2.1 MQTT session contract

| Parameter | Required value |
|---|---|
| Protocol | MQTT 3.1.1, no TLS (LAN broker, port 1883 default) |
| Subscription | exactly one: `muehle/#` at **QoS 0** |
| Subscription timing | immediately on every (re)connect, before anything else, so the retained-payload flood populates the store before user interaction |
| Session | clean session (no broker-side queue; messages published while the console is disconnected are lost by design) |
| Keep-alive | 20 s |
| Client ID | `hf-console-<epoch-milliseconds>` — unique per app launch so concurrent sessions do not evict each other |
| Auto-reconnect | enabled; on reconnect the `muehle/#` subscription SHALL be re-established (the retained flood then refreshes the store) |
| Connection-loss detection | keep-alive + disconnect callbacks SHALL flip a single boolean `connected` state; every surface that cares (banner §4.1, top-bar dot §3.0) listens to that state |
| Console LWT | none (the console is not a monitored component) |
| Publishes | only `muehle/<slot>/cmd`, at **QoS 1**, retain flag fixed per topic (§2.7) |
| Publish while disconnected | **silently dropped** — no queue, no error dialog. The link-down banner (§4.1) is the operator-facing warning |

Requirement statements:

- **R2.1.1** The console SHALL subscribe to `muehle/#` and nothing else.
- **R2.1.2** The console SHALL publish only to topics matching `muehle/<slot>/cmd`.
- **R2.1.3** If the initial connection attempt fails (e.g. broker unreachable at app
  launch), the console SHALL open anyway and render in offline state; the failure SHALL be
  swallowed. *Known fragility carried forward deliberately:* in the reference the library's
  auto-reconnect arms only after a first successful connection, so a console started before
  the broker is reachable may stay offline until manually restarted. Any reconstruction
  SHALL provide at least one recovery path from a failed initial connect (see §7, open
  decision).
- **R2.1.4** The console SHALL expose a `clear(topic)` capability: publishing a
  zero-length **retained** message to a topic (the standard MQTT retained-clear idiom).
  The shipped UI does not invoke it; it exists for tooling/tests.

### 2.2 Inbound decoding and plane parsing

Per received message, the console SHALL:

1. Decode bytes as UTF-8, tolerating malformed byte sequences.
2. Treat an **empty payload as "cleared plane"**: the corresponding slot plane value is set
   to null. This is how a component deletes a retained message.
3. Otherwise JSON-decode; on decode failure keep the raw string (only the `/status` plane
   legitimately is one: the bare text `online`/`offline`).
4. Parse the topic: the last path segment MUST be one of the four plane names
   `meta` / `state` / `status` / `cmd`; everything before it is the slot address
   (e.g. `muehle/hf/pa/state` → address `muehle/hf/pa`, plane `state`). Topics whose last
   segment is not a known plane name SHALL be ignored entirely.
5. `/state` snapshots are **full replacements**, never merges.

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

- Clearing `/state` or `/status` (empty retained payload) is itself a change and SHALL
  stamp the corresponding change time.
- Every bus message SHALL trigger a store change notification (the reference notifies the
  whole UI per message; a reconstruction MAY coalesce, provided no displayed value is ever
  stale beyond the coalescing window and the DX service keeps its own 500 ms throttle,
  §2.9).
- **No optimistic UI**: the console renders only what the bus confirms. Missing state keys
  render from the neutral defaults of §2.6, never from the value a control would set.

### 2.4 Two-layer liveness

A slot is `isOnline` only if BOTH layers are up (see `02-interface-spec.md` §liveness):

- **bridgeOnline** = `/status` == the exact string `"online"`. This plane carries the
  bridge's MQTT LWT.
- **deviceOnline**:
  - `state == null` (no snapshot ever received) → **false**;
  - `state` present but **without a `device_online` key** → **true** — logic-only slots
    (the antenna-selection reconciler, the power sequencer, the discovery consumer) have
    no physical device and legitimately omit the key;
  - otherwise → `state.device_online == true`. The bridge process can stay
    `/status: online` while its serial/USB/Ethernet link to the device is down;
    `/state.device_online` is the only honest device-link signal.

Requirements:

- **R2.4.1** The console SHALL never key offline detection on `/status` alone nor on
  `/state` alone.
- **R2.4.2** (station-wide caveat the console MUST survive) On a **clean** process
  shutdown the broker does not fire the LWT, so a retained `/status` can stay `"online"`
  for a stopped service. The console therefore cannot distinguish "stopped cleanly" from
  "online" via `/status` alone — this is precisely why the second layer exists, and why a
  slot whose bridge stopped cleanly will eventually surface through `device_online`,
  silent-slot reporting, or missing state rather than `/status`.
- **R2.4.3** `device_online` form: deployed bridges publish an explicit
  `device_online: true`; the integration-model doc says the key may be omitted when true.
  The console SHALL treat both forms as equivalent (absence = true, once a state snapshot
  has arrived). Whether a reconstruction mandates explicit-true is an open decision
  (§7, and `02-interface-spec.md`).

### 2.5 Offline rows, "went dark" stamps, and the connect grace

**Expected slots.** The console pre-declares the 15 slots that must exist:

```
muehle/power/master     muehle/power/psu-13v8    muehle/hf/radio
muehle/hf/ant-ctrl      muehle/hf/ant-switch     muehle/hf/switch
muehle/hf/pa-arm        muehle/hf/antenna-select  muehle/hf/pa
muehle/hf/rotator       muehle/hf/tuner          muehle/hf/power-seq
muehle/hf/discovery     muehle/uhf/rotator       muehle/uhf/pol-ctrl
```

(Note: `muehle/uhf/pol-ctrl` has **no firmware in the reference repo** — PLC #2 is a
documented station gap — so this slot will always appear as silent/offline in the faults
bar until that firmware exists. That is correct behavior, not a defect.)

**Offline-row taxonomy.** For every slot ever heard from, the computed offline list
SHALL contain, with the exact strings:

| Condition | Row text | Timestamp shown |
|---|---|---|
| `/status != "online"` | `<addr>: bridge down` | `statusChangedAt` |
| bridge up but device link down | `<addr>: device unreachable` | `deviceChangedAt ?? statusChangedAt` |

**Silent expected slots (dead-since-boot visibility).**

- **R2.5.1** On every (re)connect the console SHALL record the connect time and start a
  **3-second grace timer** (the reconnect re-floods retained state, so connect-time
  silence must not trip the report).
- **R2.5.2** The grace SHALL be measured from connect, **not from the first received
  message** — a broker with zero retained payloads delivers nothing at all, which is
  exactly the dead-station case the report exists for.
- **R2.5.3** After the grace expires, every expected slot that has produced **no message
  at all this session** SHALL be reported as `<addr>: silent (no state since connect)`,
  stamped with the connect time. The report SHALL clear the moment the slot publishes
  anything.
- **R2.5.4** The grace timer SHALL force a UI refresh after 3 s even if no further bus
  message arrives (a one-shot timer), so the report appears even on a quiet bus.
- **R2.5.5** Timestamps are best-effort: retained messages arriving at connect count as
  "changes", so "when did it go dark" can be understated (floored at connect time), never
  overstated. The UI SHALL render the store's stamp, not the render-time clock.

**Fault history** (from `/state.fault` and `/state.error` on every slot):

- Active fault text per state apply: `error.toUpperCase()` if `error` is non-empty and
  not case-insensitively `NONE`; else `fault.toUpperCase()` if non-empty and not `none`;
  else no active fault. **Error outranks fault.**
- A previously active record for the same address whose text no longer matches SHALL be
  marked cleared (inactive).
- An exact repeat SHALL re-activate the record AND refresh its timestamp (repeated faults
  stay at the top and read as ongoing). Known consequence: a recurring fault's "when"
  becomes its last-seen time, not its onset.
- Timestamp source: `state.ts` when present, else the local clock, ISO-8601.
- Records are never deleted, only deactivated. History caps at **30 records** (oldest
  evicted). Record key: `<address>|<TEXT>`.

**PSU-off root-cause naming.** When the 13.8 V PSU bridge (`muehle/power/psu-13v8`) is
itself online, its `state.power` is confirmed `"off"`, and any `muehle/hf/` slot appears in
the offline list — a switched-off PSU silently kills the whole HF control chain — the
faults bar SHALL add the explanatory top line:

```
muehle/power/psu-13v8: PSU OFF — HF control chain unpowered
```

Confirmed-off only: a missing `power` key is **unknown**, and inferring a fault from an
absent key is forbidden.

**Faults bar rendering.** "FAULTS" header + tag (`N ACTIVE` red / `ALL OK` green). Rows:
fault-history records plus offline entries, except that an address with an active fault
suppresses its generic offline line (the fault text says more). Rows carry an HH:MM:SS
stamp. Sort: active first, then newest first. **Visible list capped at 4 rows.** Empty
state: "No faults or offline devices".

### 2.6 State-field catalog and neutral defaults

The console reads the following `/state` keys (full catalog per slot; the console
tolerates missing keys everywhere). Units are normative: **frequency is always integer
hertz (`freq_hz`), never kHz or MHz**; power in watts; temperature in °C; SWR as a ratio;
azimuth in degrees.

| Slot | Keys read (type) |
|---|---|
| `muehle/hf/radio` | `freq_hz` (int, Hz), `band` (string, e.g. `"20m"`), `mode` (`cw`,`usb`,`lsb`,`am`,`fm`,`data`), `tx` (`rx`/`tx`), `tuning` (bool), `drive` (int %), `dvk_status` (`idle`/`playback`/`recording`/`preview`/`disabled`), `dvk_id` (int, 0 = none), `mic_profile` (string, active profile name), `mic_profiles` (caret-delimited string list), `device_online`, `ts` |
| `muehle/hf/pa` | `mode` (`standby`/`operate`), `keyed` (`rx`/`tx`), `fault` (`none` or word), `error` (`""`/`NONE` when clear), `temp_c` (num), `fwd_power_w` (num 0–1200), `rfl_power_w` (num), `swr` (num 1.0–4.0), `pa_state` (device string), `power` (`on`/`off`), `device_online`, `ts` |
| `muehle/hf/tuner` | `inline` (bool), `settling` (bool), `fault` (string), `swr` (num), `device_online`, `ts` |
| `muehle/hf/rotator` | `az` (double), `target_az` (double), `moving` (bool), `device_online`, `ts` |
| `muehle/hf/ant-ctrl` | `direction` (string), `moving` (bool), `band` (string, may be `""`), `device_online`, `ts` |
| `muehle/hf/ant-switch` | `selected` (`off`\|`port1`\|`port4`\|`port5`\|`port6`), `settled` (bool), `device_online`, `ts` |
| `muehle/hf/antenna-select` | `mode` (`auto`/`manual`), `ts` (logic slot — no `device_online` key) |
| `muehle/hf/switch` | `pa` (`on`/`off`), `trx` (`on`/`off`), `device_online`, `ts` |
| `muehle/hf/pa-arm` | `enabled` (bool), `armed` (bool), `error` (string), `device_online`, `ts` |
| `muehle/hf/power-seq` | `phase` (e.g. `idle`, `running`, `starting`, `stopping`), `fault` (string), `ts` |
| `muehle/power/master`, `muehle/power/psu-13v8` | `power` (`on`/`off`), `device_online`, `ts` |

Neutral display defaults when a key is absent (encodes "unknown/neutral", deliberately
distinguishable from real values): PA `mode=standby`, `keyed=rx`, `fault=none`, `error=""`,
`temp_c=0`, `fwd_power_w=0`, `swr=1.0`; tuner `inline=false`, `settling=false`, `fault=""`,
`swr=1.0`; rotator `az=0` (target = az), `moving=false`; ant-ctrl `direction=forward`;
power slots `power="off"`; radio `freq_hz=0`, `band=""`, `mode=""`, `tx="rx"`, `drive=0`,
`dvk_status="idle"`, `dvk_id=0`; pa-arm `enabled=false`, `armed=false`, `error=""`.
A missing antenna-switch `selected` renders "Unknown", never a port (§3.6).

### 2.7 Per-slot `/cmd` retain policy

`/cmd` topics are normally NOT retained (one-shot); idempotent actuator setpoints are
retained by their commander so a restarted device re-applies the last commanded state
(self-heal replay). The console SHALL use exactly this fixed map:

| Slot (topic `muehle/<slot>/cmd`) | Retained |
|---|---|
| `power/master` | **yes** |
| `power/psu-13v8` | **yes** |
| `hf/switch` | **yes** |
| `hf/pa-arm` | **yes** |
| `hf/ant-ctrl` | **yes** |
| `hf/ant-switch` | **yes** |
| `hf/antenna-select` | **yes** |
| `hf/pa` | no (one-shot) |
| `hf/rotator` | no (one-shot) |
| `hf/tuner` | no (one-shot) |
| `hf/power-seq` | no (one-shot) |
| `hf/radio` | no (one-shot; DVK play/stop are one-shot) |

Wrongly retaining a one-shot (e.g. rotator `set_az`) would cause a device to replay a
stale movement on boot; wrongly not retaining a setpoint would lose commanded state on
restart. This map matches the bus-wide policy (see `02-interface-spec.md`).

### 2.8 Command payload contract

Every operator action is one publish of a JSON payload to `muehle/<slot>/cmd` at QoS 1
with the §2.7 retain flag. The station-wide payload convention is
`{"action": <name>, "value": <string argument>}`. The deviations below are
**load-bearing**: deployed bridges parse them as-is and a reconstruction of the console
MUST reproduce them byte-for-byte in key names and value types.

| # | UI source | Topic | Retain | Exact payload |
|---|---|---|---|---|
| 1 | Power panel, MAINS toggle | `muehle/power/master/cmd` | yes | `{"action":"set_power","value":"on"|"off"}` |
| 2 | Power panel, PSU toggle | `muehle/power/psu-13v8/cmd` | yes | `{"action":"set_power","value":"on"|"off"}` |
| 3 | Power panel, TRX toggle | `muehle/hf/switch/cmd` | yes | `{"action":"set_trx","value":"on"|"off"}` |
| 4 | Power panel, PA toggle | `muehle/hf/switch/cmd` | yes | `{"action":"set_pa","value":"on"|"off"}` |
| 5 | Power panel, START STATION | `muehle/hf/power-seq/cmd` | no | `{"action":"start"}` — **no `value` key** |
| 6 | Power panel, STOP STATION | `muehle/hf/power-seq/cmd` | no | `{"action":"stop"}` |
| 7 | Ultrabeam panel, FORWARD | `muehle/hf/ant-ctrl/cmd` | yes | `{"action":"direction","value":"forward"}` |
| 8 | Ultrabeam panel, 180° | same | yes | `{"action":"direction","value":"reverse"}` |
| 9 | Ultrabeam panel, BI-DIR | same | yes | `{"action":"direction","value":"bidirectional"}` |
| 10 | Ultrabeam panel, RETRACT | same | yes | `{"action":"retract"}` — no `value` key |
| 11 | Ultrabeam 6 m auto-correction (§3.5) | same | yes | `{"action":"direction","value":"forward"}` |
| 12 | Antenna panel, direct-drive port change | `muehle/hf/ant-switch/cmd` | yes | **deviation: no `action`** — `{"select":"off"|"port1"|"port4"|"port5"|"port6"}` |
| 13 | Antenna panel, reconciler-path port change | `muehle/hf/antenna-select/cmd` | yes | **deviation: no `action`** — `{"request":"<port token>"}` |
| 14 | Antenna panel, AUTO / MANUAL | `muehle/hf/antenna-select/cmd` | yes | `{"request":"auto"|"manual"}` |
| 15 | PA panel, OPERATE | `muehle/hf/pa/cmd` | no | `{"action":"set_mode","value":"operate"}` |
| 16 | PA panel, STANDBY | same | no | `{"action":"set_mode","value":"standby"}` |
| 17 | PA ARM panel | `muehle/hf/pa-arm/cmd` | yes | `{"action":"set_enabled","value":"true"|"false"}` — **value is the STRING `"true"`/`"false"`, not a JSON boolean** |
| 18 | Tuner panel, BYPASS / inline | `muehle/hf/tuner/cmd` | no | `{"action":"set_inline","value":false|true}` — **value is a real JSON boolean** |
| 19 | Tuner panel, TUNE MEM | same | no | `{"action":"tune","value":"mem"}` — value is a string |
| 20 | Tuner panel, TUNE FULL | same | no | `{"action":"tune","value":"full"}` |
| 21 | Compass tap-to-aim | `muehle/hf/rotator/cmd` | no | `{"action":"set_az","az":<double degrees>}` — **argument key is `az`, not `value`** |
| 22 | Rotator presets NA / SA / VK / JA | same | no | `{"action":"set_az","az":330|210|60|35}` |
| 23 | Rotator STOP | same | no (retain forced false) | `{"action":"stop"}` |
| 24 | TRX band buttons | `muehle/hf/radio/cmd` | no | `{"action":"set_band","value":"80m"|"40m"|"20m"|"17m"|"15m"|"12m"|"10m"}` |
| 25 | TRX DVK buttons DVK1–4 | same | no | `{"action":"dvk_play_1"…"dvk_play_4"}` (id clamped 1–12 in the builder) |
| 26 | TRX DVK STOP | same | no | `{"action":"dvk_stop","value":""}` (empty-string value stops all) |
| 27 | Mic-profile row | same | no | `{"action":"set_mic_profile","value":"<profile name>"}` |

The command-builder library also defines (not wired to any shipped button, part of the
contract surface for completeness): `{"action":"frequency","freq_hz":<int>}` to
`ant-ctrl` (integer Hz, top-level key — the one documented exception to the value-key
convention, exposed by the controller's capability descriptor), `{"action":"band","value":<band>}`
to `ant-ctrl`, `{"action":"set_band","value":<band>}` to `pa`, `{"action":"fwd"}` /
`{"action":"rev"}` to `rotator`.

All commands are silently dropped if the MQTT link is down, and every command button is
disabled at the widget level when its owning slot is offline (two-layer liveness, §2.4).

**Related hard requirement (from a live incident, re-stated here because the console is
the surface where it bites):** the PA arm relay de-energizes if its enabling inputs — the
radio's `/state` — are not refreshed within 10 s, and the radio bridge publishes `/state`
only on change. When the radio is idle-but-healthy this starves the heartbeat and the
interlock drops out; the operator sees the ARM panel flip to SAFE with no fault. Any
reconstruction of the radio bridge MUST republish radio state at least every 5 s (or
provide an equivalent liveness mechanism) — see `03-components/flexbridge.md` and
`06-safety.md`. The console cannot fix this; it must merely survive and truthfully render
it.

### 2.9 DX-spot feed (independent SSE overlay plane)

The DX overlay is an optional, read-only display layer that runs **independently of the
MQTT broker**. Loss of either feed SHALL never disable the other. The overlay is active
if and only if a station locator is configured (§5); without it the compass shows beam
heading only and the map panels render without spot data.

#### 2.9.1 Transport

- Endpoint: `GET {baseUrl}/api/stream?qth={qth}&minutes=30&surroundings=true` where
  `qth` = the station callsign if configured, else the Maidenhead locator (both
  percent-encoded); `baseUrl` defaults to `https://horstreporter.kgbvax.net`.
  `minutes=30` makes the server replay the last 30 minutes of spots on each (re)connect.
  *(Code/comment discrepancy in the reference: source comments say `minutes=15`; the URL
  parameter in code is authoritative: 30.)*
- Protocol: SSE. Each spot arrives as one default (unnamed) event with a single `data:`
  line whose payload is a JSON object. Parsing follows the HTML spec: blank line dispatches;
  one leading space stripped; multi-line `data:` joined with `\n`; `:` comments and
  `event:`/`id:`/`retry:` lines ignored for data purposes — **but any received line resets
  the idle watchdog**.
- Request headers: `Accept: text/event-stream`, `Accept-Encoding: identity` (prevents
  gzip so the stream can be split by lines), `Cache-Control: no-cache`.
- **No keepalives**: the server sends `data:` only when a spot arrives and no `: comment`
  heartbeat lines. The client therefore arms an **idle watchdog: 5 minutes without any
  received line forces the connection closed and a reconnect** (a half-open TCP
  connection would otherwise never surface as an error). Connect phase is bounded by a
  **15 s connection timeout**. Non-200 status is treated as a disconnect.
- **Reconnect/backoff** (owned by the service layer, not the transport, so all platforms
  behave identically): initial delay **2 s**, multiplied by 1.5 per failure, capped at
  **60 s**; reset to 2 s on any good event. The browser build SHALL suppress its
  EventSource's built-in auto-reconnect so this backoff governs everywhere. Losing SSE's
  `Last-Event-ID` resume is harmless because every connection re-fetches the history
  window.
- Consequences of the watchdog: on a genuinely quiet band the client reconnects every
  ≤ 5 minutes and re-ingests up to 30 minutes of spot history. Dedup (below) keeps this
  correct; it is repeated work by design. Do not remove the watchdog without replacing it
  — a half-open feed otherwise leaves stale dots forever (the 60 s prune timer is the
  backstop).

#### 2.9.2 Spot payload and ingest rules

One spot per SSE `data:` frame (the `streamSpot` shape):

| Field | Type | Meaning |
|---|---|---|
| `lat`, `lng` | number | **the remote (DX) station's position in degrees**, already resolved server-side — NOT the reporter's position. The console SHALL place dots from these values; the locator is used only for labels and grid squares. |
| `snr` | number → int | signal-to-noise ratio, dB (FT8/FT4 spots) |
| `ageSeconds` | number → int | how old the report was when the server emitted it |
| `locator` | string | the remote station's Maidenhead locator (may be empty) |
| `band` | string | band label, e.g. `"20m"` |
| `sourceType` | string | `"mqtt"` (FT8/FT4 via MQTT), `"dxcluster"`, `"rbn"`, `"wspr"` |
| `sender`, `receiver` | string, optional | callsigns; dxcluster only |

Ingest rules (exact):

- **R2.9.1** Only `sourceType == "mqtt"` spots (FT8/FT4) SHALL be ingested; dxcluster,
  rbn, and wspr are dropped at ingest. Rationale: the console's overlay intentionally
  mirrors horstreporter's azimuthal view band-for-band.
- **R2.9.2** Spots missing `lat`/`lng`, or missing all of locator+receiver+sender, SHALL
  be dropped (nothing to place or label).
- **R2.9.3 Mode-aware SNR gate**, driven live by the radio mode (`muehle/hf/radio`
  `state.mode`): `usb`/`lsb`/`am`/`fm`/`data` → SSB family, threshold **0 dB**; `cw` →
  CW family, threshold **−15 dB**; anything else (unknown/blank) → gating **off**. Spots
  below threshold are dropped. Changing the filter does not clear current spots; a
  reconnect re-ingests history under the new threshold. The UI shows the active gate as a
  filter chip: `"SNR off"` / `"SSB ≥ 0dB"` / `"CW ≥ -15dB"`.
- **R2.9.4 Dedup key**: `"<locator>|<receiver??''>|<sender??''>|<band>|<sourceType>"`.
  On repeat: keep the freshest (lowest server `ageSeconds`); tie-break by higher SNR;
  **always** refresh the kept spot's local receive time so an actively re-spotted station
  doesn't age out. (Known limitation: the key ignores lat/lng, so a station reporting a
  moved position under the same locator/sender/band keeps its first coordinates.)
- **R2.9.5 Live age** of a spot = `ageSeconds + seconds since local receipt`. Max live
  age **600 s**, enforced on ingest and by a **60 s prune timer** that runs even when the
  feed is quiet.
- **R2.9.6 Cap 80 spots**, sorted by live age ascending, tie-break SNR descending.
- **R2.9.7 UI notifications throttled to ≤ ~2 Hz (500 ms coalesce)** so a busy band can't
  repaint-storm.
- **R2.9.8 Grid-square aggregation** (shared by both maps): spots grouped by the first
  4 characters of their uppercased locator; per square the **dominant band** = the band
  with the most spots (first-max wins ties); the **score** = mean of the top quarter of
  SNR values (sort descending, take `ceil(n/4)` clamped to ≥1, average) — used for opacity.
  Squares with no band data, or spots with <4-char locators, don't contribute. Opacity
  ramp (exact): score 0 dB → 0.45; −10 dB → 0.15; slope 0.03/dB below 0 dB and 0.015/dB
  above; floor 0.10, cap 0.75.
- **R2.9.9 Fixed band palette** (NOT theme tokens — identical in all color schemes so a
  spot keeps its color across theme switches and matches the horstreporter web frontend):
  `160m #8B0000`, `80m #800080`, `60m #4B0082`, `40m #0000FF`, `30m #03B1B1`,
  `20m #008000`, `17m #808000`, `15m #FFA500`, `12m #00FFFF`, `10m #FF0000`,
  `6m #FF00FF`, `4m #FF1493`, `2m #008080`, unknown → grey `#555555`. Source-type
  fallback colors (spot with no band): mqtt→green, dxcluster→accent, rbn→amber,
  wspr→orange, other→muted text color.

#### 2.9.3 Projections and geometry (both maps share one spot service)

**AEQD (compass disc), exact math** (a port of horstreporter's own implementation so the
two frontends' maps coincide): earth radius 6371 km; disc radius = `min(w,h) × 0.47`;
scale = `radius × zoom / π`; y-axis flipped; projection clipped 0.02 rad shy of the
antipode; horizon at 20015 km. Maidenhead decode: fields 20°×10° anchored at
(−180,−90), squares 2°×1°, subsquares 5′×2.5′ with center offsets (2/4/6-char supported;
the cell center is the projected point). The configured station locator sets the AEQD
center.

**Web Mercator (map panel), exact math**: standard EPSG:3857, spherical Mercator, earth
radius 6378137, 256 px tile; latitude clamped to ±85.05112878°; longitude wrapped
relative to the map center so antimeridian crossings render as a short jump; antimeridian
cuts in coastline rings detected by a |Δlng| > 180° break in the subpath; minimum zoom =
log2(height/256).

**Landmasses**: a bundled static world outline asset (Natural Earth 50m country polygons,
~1600 rings / ~100k vertices, ~3 MB), loaded lazily once, never fetched at runtime. A
malformed asset SHALL produce an empty coastline list, never a crash (the compass renders
without coastlines rather than failing).

**Performance contract**: interactive zoom on tablet hardware SHALL stay visually smooth;
the reference achieves this with a rasterized world-layer cache keyed on center/zoom/size/
colors — the caching is implementation detail, the smoothness is the contract.

---

## 3. Panels and features

### 3.0 Pages and layout

Single-screen application with three tab pages — **Station / HF / UHF** — plus a
color-scheme picker (§6), a DX-settings gear (§5.3), an MQTT connection indicator, and an
all-online tag. Top-level column: link banner (when down, §4.1) → page content → faults
bar (§2.5 rendering).

- **HF page** (default). Tablet/landscape: two columns. Left: DX map container (fills),
  then Ultrabeam panel, Antenna panel, rotator presets. Right: top bar, then a scroll
  column of PA panel, PA-arm panel, Tuner panel, TRX/DVK panel, then the faults bar.
  Right-column width = 44% of viewport when ≥1200 px wide and ≥720 px tall, else 48%;
  minimum 420 px (compact: 320 px). Phone (`shortestSide < 600`): single vertical scroll,
  DX map on top, then all panels in fixed order, then faults.
- **Station page**: top bar, Power panel (§3.10) and Climate panel, scrolling.
- **UHF page**: placeholder text "UHF controls are not yet wired."
- The faults bar appears on every page.

**Top-bar indicators** (operator-safety surfaces):

- **Connection indicator**: 10 px dot, green glow + "MQTT" when connected, red +
  "OFFLINE" when not.
- **Online tag**: pill `● all online` (green) or `● N offline` (red), N = computed
  offline-row count (§2.5). Note N counts rows, including silent-slot rows, so it can
  exceed the physical device count.

**Boot sequence** (normative order):

1. Full-screen immersive mode; all orientations allowed.
2. Read all stored credentials/settings (§5); configure and start the DX-spot service
   regardless of broker state (it no-ops when no locator is configured).
3. If host + parseable port + user + non-empty password are all present, attempt
   auto-connect; failure is swallowed and the console opens showing the link-down banner.
   If any is missing, show the setup screen instead (§5).
4. A store listener re-evaluates the DX SNR filter family on every bus update of the
   live radio mode.

### 3.1 TRX / DVK panel (radio)

Reads `muehle/hf/radio` (§2.6). Renders:

- **Readout chip**: frequency in MHz with 3 decimals (from `freq_hz`), mode uppercased
  (accent), band, TX/RX pill (red TX / green RX), drive %, DVK suffix when not idle:
  `PLAYBACK · M<n>` (accent) / `RECORDING`, `PREVIEW` (amber) / `DISABLED` (red).
  Offline → a red-bordered `OFFLINE` chip.
- **Band buttons**: `80m 40m 20m 17m 15m 12m 10m`, active one highlighted, each
  publishing `set_band` one-shot. (160m/60m/30m/6m are deliberately omitted for this
  station's antennas — a reconstruction should treat the list as configuration, not law.)
- **DVK buttons** DVK1–DVK4 publishing `dvk_play_n`; active highlight while
  `dvk_status == "playback"` and `dvk_id` matches. **STOP** (danger styling) publishes
  `dvk_stop` with empty-string value.
- **Mic-profile row** (three buttons bound by name to SmartSDR mic profiles, persisted
  on-device under keys `mic_profile_btn1`…`mic_profile_btn3`):
  - Tap a **bound** button → activate it (`set_mic_profile <name>`).
  - Tap an **unbound** button → pick-from-list dialog populated from
    `muehle/hf/radio.state.mic_profiles` (a caret-delimited string list the bridge
    queries from the radio on connect; empty list → informational dialog, no manual
    entry). Picking binds the button AND activates the profile.
  - **Long-press** → dialog with **Associate** (bind another existing profile without
    activating) and **Unbind** (bound buttons only).
  - The active name (`state.mic_profile`) renders as a `MIC: <name>` label and highlights
    the matching bound button; it is empty until the first profile is loaded via the bus —
    SmartSDR reports no active-mic name, so the bridge tracks "last loaded".
  - There is deliberately no Save/edit: profiles are created and edited in SmartSDR
    itself.

### 3.2 PA panel (power amplifier)

Reads `muehle/hf/pa` (§2.6) plus cross-slot `muehle/hf/switch.state.pa` (the remote-on
relay) and the slot's own liveness. Renders:

- **FWD power meter**, full scale **1200 W**, bar labels `0 / 500 / 1000 / 1200`,
  green gradient into orange at the hot end. Two ballistic markers over the bar:
  a downward triangle at the **1-second rolling-window peak** (white) and an upward
  triangle at the **rolling 95th percentile** (accent, linear-interpolation percentile).
  Markers snap up instantly, then decay linearly at **24 W per 100 ms tick** (full-scale
  drain in ~5 s), never below the live reading; the decay timer runs only while a marker
  stands above the live value. A constant reading refreshes its sample timestamp so it
  stays "present" in the window.
- **SWR meter**, full scale **4.0**, labels `1.0 / 1.5 / 3.0 / 4.0`, amber fill. There is
  deliberately no reflected-power readout.
- Buttons **OPERATE** (green when active) / **STANDBY** (amber when active), disabled
  offline, publishing `set_mode`.
- **Status tag priority** (cross-panel RF presentation; a live TX outranks relay
  bookkeeping): `OFFLINE` (muted) → fault/error text uppercased (red) → `● TX` (red) →
  `RELAY ?` (amber; the hf/switch relay state is unknown — a missing key must never be
  asserted as OFF) → `PA RELAY OFF` (amber) → `PA OFF` (amber; the amp's own power
  telemetry) → `INHIBITED` (amber) → `OPERATE` (green) / `STANDBY` (amber). Each appends
  ` · {temp} °C` when temperature > 0.

### 3.3 PA ARM panel

Reads `muehle/hf/pa-arm`: `enabled`, `armed`, `error`. One toggle button labeled **ARM**
(when disabled — danger styling) or **SAFE** (when enabled), publishing `set_enabled`
with the inverted value as a string. Tag priority: `OFFLINE` (muted) → error text (red) →
`ARMED` (red — the interlock is hot, the amp can be keyed) → `ENABLED` (amber) → `SAFE`
(green). Arming goes through a retained command so a pa-arm bridge restart restores the
armed state.

### 3.4 Tuner panel (antenna tuning unit)

Reads `muehle/hf/tuner`: `inline`, `settling`, `fault`, `swr`. Buttons: **BYPASS**
(active-amber when not inline), **TUNE MEM**, **TUNE FULL** — both tune buttons read
**`TUNING…`** and are locked while `settling` (a second tune queued against a settling
tuner just competes with the in-flight one). BYPASS is always enabled while online. Tag:
`OFFLINE` → fault uppercased (red) → `TUNING` (amber) → `IN LINE · SWR {x}` — colored by
SWR: **≥3.0 red, ≥2.0 amber, else green** (3.5:1 and 1.1:1 must not read alike) →
`BYPASS` (amber — a bypassed tuner is a degraded TX path, not neutral info).

### 3.5 Ultrabeam panel (beam direction)

Reads `muehle/hf/ant-ctrl` (`direction`, `moving`, `band`) and `muehle/hf/radio` (`band`).
Header pill priority: `OFFLINE` (red) → `MOVING` (red) → `DIRECTION` uppercased (accent)
→ `BAND MISMATCH · <ctrlBand> ≠ <radioBand>` (red). The mismatch check fires only when
the slot is online AND both bands are "comparable" — non-empty, not `gen`/`unknown` (the
radio bridge's out-of-allocation labels), not starting with `band-` (the antenna
controller's) — and differ. (The two bridges label unknown frequencies differently and
can agree on frequency while disagreeing on label; pre-first-state silence must not cry
wolf. The comparability heuristic is tuned to the two deployed bridges' vocabularies.)

Buttons: **FORWARD**, **180°** (reverse), **BI-DIR**, **RETRACT** (danger styling).

- All four require the slot online.
- Direction buttons are locked while `moving` (rapid presses can't queue competing motor
  commands mid-travel).
- **RETRACT stays pressable while moving** — it is the designated emergency action.

**6 m auto-correction** (the one documented console-initiated automatic command, a
device-physics guard): on the 6 m band the Ultrabeam's elements support only forward
radiation. Therefore (a) the 180° and BI-DIR buttons are disabled while the radio is on
6 m, and (b) if `direction != forward` while the radio is on 6 m and the controller is
online and not moving, the console itself auto-publishes `direction=forward` **once per
invalid state**: a latch flag prevents republish-per-rebuild and clears when the state
resolves or while moving, so the correction re-fires after travel if the state is still
invalid. The auto-correction obeys the same moving lockout as manual buttons. It fires
without operator confirmation by design.

Personality note: a band-heckling-dragon mascot image sits beside the RETRACT button —
a deliberate bit of station personality; keep or drop, but it is user-recognized.

### 3.6 Antenna routing panel

Reads `muehle/hf/ant-switch` (`selected`, `settled`), `muehle/hf/antenna-select`
(`mode`), plus cross-panel RF state from `muehle/hf/radio` (`tx`, `tuning`,
`device_online`) and `muehle/hf/pa` (`keyed`).

**Port→label map** (source: the antenna-selection reconciler's wiring map; ports 2 and 3
are unwired at this site and omitted from the UI):

```
off → Grounded        port1 → Dummy load      port4 → Ultrabeam
port5 → Port 5         port6 → Fan dipole 80/40
```

> **CONFLICT (open decision, see §7):** the repo-root documentation table places the
> Ultrabeam on switch **port 3**; the console's map and the reconciler's example config
> say **port 4**. The deployed wiring on shari is authoritative but was not readable when
> this PRD was written. A reconstruction MUST resolve port 3 vs port 4 on-device before
> finalizing the map; the console's behavior is otherwise identical (fan dipole = port 6
> and dummy load = port 1 are consistent everywhere).

Rendered port buttons: `off, port1, port4, port5, port6`. A missing or unknown `selected`
value renders **`?` (Unknown)** — never as a port, least of all `off`, which would paint a
dead bridge as a deliberate grounded-safety state.

Header: `"{modeLabel} · {antennaName}"` where modeLabel is the reconciler mode uppercased
if that slot is online and reports a mode, else **`DIRECT`** (never fabricate "AUTO"
when nobody enforces the policy). Color: red when blocked (grounded or manual override),
accent when managed and settled, amber when managed-but-not-settled or unmanaged.
Additional tags: while relays are in flight (`settled == false`) an amber **`NO RF`** tag
plus a pulsing amber dot (700 ms animation); **`RF ON`** (red) whenever RF is detected
anywhere; **`RF ?`** (amber) when direct-drive is active and RF state is unknown.

**Routing command selection** (normative):

- If the reconciler is online and its mode is not `manual`: taps publish
  `{"request": <port>}` to `antenna-select` — the policy layer, which enforces
  RF-inhibit ordering itself. This path is **exempt from the console-side RF guard**.
- If the reconciler is in `manual` mode, or offline/absent ("direct drive"): taps
  publish `{"select": <port>}` directly to `ant-switch` — and are **blocked unless
  RF-safe** (§4.3).
- The AUTO / MANUAL mode buttons publish `{"request":"auto"/"manual"}` to
  `antenna-select`, enabled only when that slot is online.

### 3.7 Rotator compass panel

Reads `muehle/hf/rotator` (`az`, `target_az`, `moving`) and `muehle/hf/ant-ctrl`
(`direction`). Draws a circular compass disc:

- Ticks every 30°, majors every 90°; cardinal labels N (accent) / E, S, W (muted).
- **Beam lobes** from the Ultrabeam direction: forward = one wedge of half-width 30° at
  `az` (accent); reverse = one at `(az+180) % 360` (amber); bidirectional = both, each
  half-width 45°. Filled wedge + radial line + arrowhead at the rim.
- White boom line from center to `az`; accent center dot.
- **Target line**: semi-transparent accent line at `target_az`, drawn only when
  `|target_az − az| > 5.0°`; the header chip likewise shows the target (`"123° → 330°"`)
  only beyond 5°.
- Chrome: top-left DX status badge (`"DX 42"` spot count / `"DX …"` connecting /
  `"DX ✗"` feed down — hidden entirely when the overlay is off); top-right azimuth chip
  assembled from `["<az>°", "→ <target>°"?, "MOVING"?, "OFFLINE"?]` — red while moving,
  muted while offline, accent otherwise; a zoom badge; a band-key rail on the left
  listing only bands present in current spots, in canonical order
  (160m, 80m, 60m, 40m, 30m, 20m, 17m, 15m, 12m, 10m, 6m), each chip a swatch of the
  fixed band color (§2.9.2), fading in/out over 180 ms.
- **Zoom**: range **[1.0, 5.0]**, default **1.5**, step **0.2**; NaN→default; ±∞ clamps
  to the bound; buttons disabled within 1e-9 of the bounds. Controls: stacked +/−
  buttons bottom-right, and a vertical drag on the disc's **right gutter** (right half,
  inside the disc radius): drag up = zoom in, ~30 px per 1.0 zoom step, available only
  while the DX overlay is active.
- **Tap-to-aim**: a tap anywhere on the disc (while the rotator slot is online) converts
  the tap angle to a compass azimuth
  `((atan2(dy,dx)·180/π + 90) mod 360 + 360) mod 360` and publishes
  `{"action":"set_az","az":<deg>}`. When the rotator is offline the disc gesture is
  disabled and the chip reads OFFLINE.
- **DX overlay on the disc**: AEQD-projected landmasses (§2.9.3) plus 4-char grid-square
  quads filled with the dominant band color at the SNR opacity, stroked 0.6 px. Grid
  squares are drawn at ANY projected size (they must not vanish at low zoom); the only
  skips are corners off-disc (antipode wrap) or centroid off-disc.

**Rotator presets bar** (separate card): `NA 330`, `SA 210`, `VK 60`, `JA 35`
(NA = North America, SA = South America, VK = Australia, JA = Japan — ham country
prefixes), each publishing `set_az`; `STOP` publishes `{"action":"stop"}` (retain forced
false). All five are disabled when the rotator slot is offline.

### 3.8 Mercator DX map panel

The map area hosts two interchangeable projections sharing one spot service (§2.9),
toggled by icons top-right (compass = azimuthal, map = Web Mercator), plus the SNR filter
chip.

**Mercator panel**: pan by drag (the map moves opposite the drag), wheel/scroll zoom;
zoom range **[1.0, 12.0]**, default **2.5**, step **0.5**, buttons disabled within 1e-9
of bounds; a crosshair reset button re-centers on QTH and restores zoom 2.5. Until the
user pans, the view is pinned to QTH. Paints, back to front: page-colored background,
land fill + coastline stroke (even-odd fill so GeoJSON hole rings subtract; antimeridian
handling per §2.9.3), grid-square quads (dominant-band fill at SNR opacity, +0.2-opacity
stroke), spot dots (radius 2.5, band color, 0.6 white contrast stroke), and a QTH marker
(accent dot, radius 5, page-color ring).

### 3.9 Station power / sequencer panel

Reads `muehle/power/master.power`, `muehle/power/psu-13v8.power`,
`muehle/hf/switch.trx`/`.pa`, `muehle/hf/power-seq.phase`/`.fault`, and each slot's
liveness. Layout: **START STATION** (green styling) · **STOP STATION** (danger) · four
relay toggles (MAINS, PSU 13.8V, TRX, PA — ON green / OFF faint / OFFLINE red text with
a tooltip "`<name>` offline — relay uncontrollable"; each toggle gated on its slot being
online) · SEQUENCE readout.

- **START gating**: enabled only when the power-seq slot is online AND `phase` is NOT
  `running` AND NOT `starting`. (Known defect carried forward: phase `stopping` leaves
  START enabled — a tap mid-shutdown sends `{"action":"start"}` during the stop sequence.
  The sequencer tolerates it; a reconstruction should decide whether to guard it. §7.)
- **STOP**: enabled whenever the power-seq slot is online.
- Sequence label: `FAULT` (red, if `fault` non-empty) / `ON` (green, running) /
  `STARTING` (green) / `STOPPING` / `IDLE` (muted).

The sequencer itself (ordered startup: mains → PSU → TRX → PA → arm, shutdown in reverse,
paced on real liveness confirmations) is a server component — see
`03-components/powerseq.md`; the console only starts/stops it and renders its phase.

### 3.10 Climate panel

A **static placeholder**: hard-coded `HEAT` (on), `COOL` (off), `21.4 °C`, `612 ppm`.
No bus topics, no commands. HVAC control is not wired anywhere in the station; the panel
exists to reserve layout and shall not imply telemetry that does not exist (a
reconstruction may omit it or clearly mark it "not wired").

---

## 4. Operator-safety UI (consolidated contract)

These rules are the console's core reason for existing. They are testable requirements;
each traces to a real incident or a deliberate design decision.

- **R4.1 LINK DOWN banner.** While the broker connection is down, a full-width red strip
  renders at the very top of every page with the exact copy:

  > `LINK DOWN — DATA STALE · COMMANDS NOT DELIVERED`

  It disappears the instant the link returns. Taps are NOT disabled panel-by-panel —
  commands are simply not delivered, and the banner promises exactly that. (Consequence:
  a user can tap with no per-press feedback; the banner is the only warning. A
  reconstruction may add per-press feedback but SHALL NOT remove the banner.)

- **R4.2 No command while offline.** Every command button is disabled unless its owning
  slot passes two-layer liveness (§2.4).

- **R4.3 Fail-closed cross-panel RF guard (cold-switch).** A direct-drive antenna-switch
  port change (§3.6 manual/reconciler-offline path) SHALL be refused unless RF is
  confirmed safe. RF is "on" if ANY of three independent paths says so: radio
  `tx == "tx"`, radio `tuning == true`, PA `keyed == "tx"`. RF is "safe" only if the
  radio link is up AND `tx == "rx"` AND `tuning != true` AND `keyed != "tx"` —
  **unknown blocks** (fail closed); confirmed-RX allows. Rationale: switching an antenna
  relay while transmit power is applied arcs the contacts. The reconciler path (auto
  mode, reconciler online) is exempt because the reconciler arbitrates inhibit ordering
  itself.

- **R4.4 Never fabricate OFF (or any definite state) from missing data.** A missing
  `hf/switch.pa` shows `RELAY ?`, never "PA RELAY OFF"; a missing PSU `power` key never
  triggers the PSU root-cause line; a missing `ant-switch.selected` shows Unknown, never
  Grounded; an absent selector never shows "AUTO".

- **R4.5 Red means "shouting", not "error", in these states**: the GROUNDED port button
  renders solid red even while active (grounded = no antenna connected = operating
  impossible); the MANUAL override button renders solid red while engaged; ARMED renders
  red (the interlock is hot); RETRACT and danger actions use red-tinted styling.

- **R4.6 Stale data must be loud.** Offline rows are stamped with when each slot went
  dark (§2.5), never with render-time; silent expected slots are reported after the 3 s
  connect grace; a live TX outranks plumbing in the PA tag.

- **R4.7 Motion/settling discipline.** Ultrabeam direction commands are locked while
  moving (RETRACT excepted); tuner tune commands are locked while settling; START STATION
  is disabled while the sequencer reports running/starting.

- **R4.8 The DX overlay never interferes with control.** It is read-only display; tap =
  aim, right-gutter vertical drag = zoom; the gestures do not overlap.

- **R4.9 Band colors are fixed across themes** (§2.9.2) — the horstreporter
  correspondence is a cross-component visual invariant.

- **R4.10 Payload shapes are contracts** (§2.8): the value-key deviations must be
  reproduced exactly — deployed bridges parse them as-is and are NOT being reimplemented
  as part of the console.

---

## 5. Setup and configuration

All configuration is user-entered at runtime and stored per-device. There is no config
file, no build-time config, no command-line flags.

### 5.1 Stored keys

| Key | Meaning | Default | Storage |
|---|---|---|---|
| `mqtt_host` | Broker host | form default `192.168.1.50` (see §5.4) | secure storage |
| `mqtt_port` | Broker TCP port (stored as string) | `1883` (parse failure on save falls back to 1883) | secure storage |
| `mqtt_user` | MQTT username — the dedicated low-privilege `console` account (ACL: subscribe `muehle/#`, publish `muehle/+/cmd`; deliberately NOT the broad `hf` user, so no station-wide credentials are embedded in the app) | `console` | secure storage |
| `mqtt_password` | Broker password — must be non-empty; empty ⇒ auto-connect skipped, setup screen shown | — | secure storage |
| `station_locator` | Station Maidenhead locator; uppercased on save; presence enables the DX overlay and sets the AEQD center; empty = overlay off | — | secure storage |
| `horstreporter_base_url` | SSE feed base URL | `https://horstreporter.kgbvax.net` | secure storage |
| `station_callsign` | Optional station callsign; preferred as the SSE `qth=` parameter over the locator | — | secure storage (**dead key** — read at startup but no UI writes it; §7) |
| `mic_profile_btn1..3` | Mic-profile button bindings | unbound | plain device preferences |

**Secrets handling (normative):** on mobile the credential keys SHALL live in the
platform's encrypted credential storage (Android Keystore / iOS Keychain). On web no such
storage exists; the reference falls back to plain browser-local storage — i.e. the broker
password sits in the clear on the web channel. This is a known accepted trade-off on a
trusted LAN; a reconstruction MUST either preserve it knowingly or explicitly improve it
(e.g. per-session password entry on web), and SHALL NOT silently worsen it.

### 5.2 First-run credentials screen

Shown if and only if any of `mqtt_host` / `mqtt_port` / `mqtt_user` / `mqtt_password` is
missing (password must be non-empty). A centered "MÜHLE · HF" card with fields Host,
Port, Username, Password (obscured, eye toggle) and an optional DX-overlay section:
Station locator and Horstreporter URL. CONNECT saves all six values (locator
uppercased), attempts a connect (failure swallowed — the console opens with the banner),
configures and restarts the DX service, then switches to the console. Validation: empty
host, empty user, or empty password ⇒ the save silently refuses.

### 5.3 Why the setup screen is unreachable after provisioning, and the gear workaround (normative)

Boot routes to the setup screen only when a credential is missing. Once provisioned, the
device boots straight to the console **forever**, and there is deliberately no in-app
settings page for broker credentials: they live only in secure storage, and changing them
requires clearing app storage or reinstalling. The reasons are deliberate:

1. The tablet is a fixed-mount appliance; a settings page invites accidental
   misconfiguration of a safety-relevant connection.
2. The DX-overlay keys were added later and needed a reachable editor — so the top-bar
   gear ("DX overlay settings") opens a modal sheet editing **only** `station_locator`
   and `horstreporter_base_url` (same defaults, locator uppercased on save). Saving
   persists both keys and **live-applies** them to the running spot service (configure +
   restart) without touching the MQTT link.

This lockout is operationally awkward when the broker moves (see §5.4) and is a
reconstruction decision point — but the behavior above is the specified contract; if a
reconstruction adds an in-app broker-credential editor it MUST be guarded (e.g.
confirmation) and is a deviation to be documented, not a silent improvement.

### 5.4 Broker topology: default and planned migration

- **Current production**: a single MQTT broker at `192.168.1.50:1883` (the house
  Home-Assistant broker). The console's setup-screen default and the web proxy's broker
  flag both point there.
- **Planned, committed but NOT deployed** as of 2026-08-29: a shack-local broker on
  shari (`192.168.1.139:1883`, authoritative for `muehle/#`) bridged to the house
  broker, so the station keeps its bus when the shack↔house link drops. That work
  repoints the console's default host and the web proxy's default broker to
  `192.168.1.139:1883`.
- The console itself is topology-agnostic: the broker host is a config field, and the
  only web-channel difference is the transport (direct TCP vs the WebSocket proxy). A
  reconstruction SHALL treat the topology as a deployment decision (see `00-system-overview.md`
  and §7) and SHALL NOT hard-code either address into application logic. (The reference
  web build hard-codes its WebSocket URL — a known LAN-IP brittleness; improving it is
  allowed.)

> **Doc/code disagreement (evidence):** the console project's own README/CLAUDE docs
> already describe the shack broker at `192.168.1.139:1883`, while the examined code
> defaults to `192.168.1.50:1883`. The code is authoritative for current production; the
> docs describe the pending migration.

### 5.5 Hard-coded values (not user-configurable)

The world-outline asset, the fixed band palette, the mascot image, bundled fonts, the
15-slot expected list, the antenna port map (pending the §3.6 conflict), the web
WebSocket endpoint, keep-alive 20 s, silence grace 3 s, SSE `minutes=30` +
`surroundings=true`, DX max age 600 s / 80 spots / backoff 2–60 s / throttle 500 ms /
prune 60 s, SSE connect timeout 15 s / idle watchdog 5 min, fault-history cap 30, DVK id
clamp 1–12.

---

## 6. Theme, visual language, and design lineage

### 6.1 Color schemes

Three user-switchable schemes, switchable at the top bar at any time (buttons DC / PA /
FO). Exact reference values (a reconstruction may refine the hex values, but dark-first,
near-black cards, monospace readouts, and the semantic assignments are the visual
contract):

- **dc** (default, dark): page `#0A0C10`, pane `#0F1218`, card `#151923`, land
  `#232C3E`, lines `#2A3142`/`#3D4558`, text `#DEE3EC` (normal) / `#8C97AB` (muted) /
  `#5B6579` (faint), accent cyan `#5FB2C9`, green `#5CCB8A`, amber `#D7B04A`, red
  `#D9685C`, orange `#D99A5F`.
- **paper** (light): warm-grey page `#E5E3DD`, card `#F9F8F5`, ink `#1A1A1A`, accent
  `#005F99`, green `#1E6B48`, amber `#A36A00`, red `#B52E1D`, orange `#C45F18`.
- **forest** (dark green): page `#151A17`, accent gold `#C8A45C`, green `#6DB88B`.

Semantics are invariant across schemes: accent = active/informational, green =
healthy/allowed, amber = degraded/attention, red = danger/offline/blocked/live-TX,
orange = hot-end of the power meter.

### 6.2 Typography and controls

- Fonts: a condensed display face for headings, a humanist sans for body, and a
  monospace face for **all data, readouts, labels and buttons** — bundled as assets,
  never fetched at runtime (the console must work with no internet beyond the LAN).
- Buttons: flat, 4 px radius, 1 px border, pane background when inactive, accent fill with
  black/white foreground when active, red-tinted for danger, **solid red fill** for
  "dangerActive" states (grounded, manual override, armed).
- Minimum touch target 44×40 (48×48 for icon buttons), with two documented deliberate
  exceptions (rotator preset buttons, compass zoom stack) — a reconstruction should
  consciously re-decide these.
- A purple-bordered card variant marks the station-power panel.
- Type scale for width: ≥1400 px → ×1.25, ≥1200 px → ×1.1.

### 6.3 Design lineage (sas reference note)

The visual/interaction exploration lives in the repo under `sas/` as static HTML
mockups; it is **design input, not software**. Two rounds exist:

1. **Information-architecture directions** (from the design brief — a fixed-mount tablet
   where every action is one tap, readable in dim light; explicitly forbidden: neon on
   black, glassmorphism, thin trendy fonts, "corporate dashboard" looks):
   **A "Shop Panel"** (chunky industrial grid), **B "Logbook Strips"** (full-width ruled
   strips), **C "Split Deck"** (persistent left world-pane + switching right command
   pane). The brief also fixed console requirements that are behavior contract
   regardless of visuals: every action one tap; a shared fault strip at the bottom;
   offline controls grey out with a reason; interlocks block dangerous actions with no
   confirmation dialogs (rejected actions surface as faults); dark + light themes.
   (The brief's original "no frequency readout" constraint was relaxed by the later
   mockups, which show band/mode/frequency in the top bar — the later artifact wins.)
2. **Four high-fidelity tablet previews**:
   - **Airbus / process-control** — flight-deck aesthetic; every state color gets a dim
     tinted "annunciator tile" box so alarms read as lit instrument tiles.
   - **DCs / dark-dense** — near-black SCADA look, single cyan accent, maximum
     information density (datablock grids, monospace everywhere).
   - **Exact design-system** — consumer-grade tokenized system (brand purple, 16 px card
     radii, pill controls); the direction most at risk of drifting toward the forbidden
     corporate-dashboard feel.
   - **Hybrid (DCs + at-a-glance grid)** — the synthesis: the DCs palette and typography
     combined with an at-a-glance grid layout; the most complete artifact and the
     approved high-fidelity reference.

All four previews agree on a **shared content model**, which is the requirement, since
the direction may otherwise change: top-bar station identity + radio context + band
strip; a banner row for beam-direction warnings; compass with beam-lobe wedges + target
line + presets; PA block with meters + OPERATE/STANDBY; tuner block with TUNE MEM /
TUNE FULL / inline state; antenna port row + AUTO + direction segment + RETRACT;
station-power card with sequencer phase + relay toggles; a climate placeholder; a
bottom RX/TX/INHIBITED-style status strip. Interaction rules from the handoff are
behavior contract for the console: one-tap actions, shared fault strip, offline
greying-with-reason, interlocks-not-confirmation-dialogs, dashed target line hidden
within 5° of target, sequencer pacing on real liveness confirmations.

---

## 7. Open decisions & unresolved facts

Each item lists the evidence for the variants; none is silently resolved.

1. **Ultrabeam switch port: 3 or 4.** Repo-root documentation and the integration model
   say switch port 3; the console's port map (`hf_console/lib/store/wiring.dart`), the
   reconciler's example config, and its deploy seed say port 4. The deployed
   `/etc/antennaselect/config.toml` on shari is authoritative but was not readable from
   the workstation when this PRD was written. MUST be confirmed on-device. Fan dipole
   (port 6) and dummy load (port 1) are consistent everywhere.
2. **Broker topology: one broker or two.** Deployed code defaults point at
   `192.168.1.50:1883`; the shack-local-broker migration (shari `192.168.1.139:1883`
   bridged to `.50`) is committed on an unmerged feature branch and not deployed as of
   2026-08-29. The console is topology-agnostic; only defaults and the web proxy's
   broker flag differ. Reconstruction must pick the target topology before fixing
   defaults.
3. **`device_online` form.** The integration model says the key may be "omitted when
   true"; deployed bridges publish explicit `device_online: true`. Consumers (this
   console) treat absence-as-true once a state snapshot has arrived. Whether a
   reconstruction mandates explicit-true everywhere is open; see `02-interface-spec.md`.
4. **Chosen design direction.** The hybrid preview is the approved high-fidelity
   reference (per the console project docs), and the shipped app is the hybrid's
   split-deck skeleton rendered in the DCs dark visual language — but the shipped
   Flutter theme's concrete values diverge in detail from every mockup. A reconstruction
   must decide whether "hybrid preview" or "shipped app" is the visual target of record.
5. **`station_callsign` is a dead key.** It is read at startup and would be preferred as
   the SSE `qth=` parameter over the locator, but no UI on the examined branch writes
   it. Either wire an editor for it (e.g. in the gear sheet) or drop it; the spot feed's
   `qth` parameter then always carries the locator.
6. **SSE watchdog churn.** The 5-minute idle watchdog guarantees
   disconnect/reconnect cycles on quiet bands, each re-ingesting up to 30 minutes of
   replayed history (dedup makes it correct but it is repeated work; the reference
   source comments even disagree with themselves about the replay window — 15 vs 30;
   the code URL parameter 30 is authoritative). Options: keep both watchdog and replay
   window (reference behavior), shrink the replay `minutes`, or negotiate a real
   keepalive with the spot service. Any change must preserve "no stale dots on a
   half-open feed".
7. **Initial-connect failure behavior.** The reference swallows a failed first connect
   and relies on library auto-reconnect that arms only after a first successful
   connection — a console booted before the broker is reachable may stay offline until
   restarted. A reconstruction must define an app-level retry (e.g. periodic reconnect
   attempts) or accept and document the same limitation.
8. **START enabled during phase `stopping`.** The shipped START STATION button is gated
   on `running`/`starting` but not `stopping`, so a tap mid-shutdown sends
   `{"action":"start"}` during the stop sequence. The sequencer tolerates it; whether
   the console should also gate `stopping` is open.
9. **Web-channel credential storage.** Plain browser-local storage for the broker
   password on the web build (no secure storage exists in browsers) — preserve as a
   trusted-LAN trade-off or replace with per-session entry; must be a conscious
   decision, documented.
10. **`dvk_status` value set.** Only `idle` appears in the console's own fixtures; the
    full vocabulary (`playback`, `recording`, `preview`, `disabled`) comes from the
    radio bridge and was not enumerable from the console's code alone.
11. **Climate panel.** Hard-coded placeholder values (21.4 °C, 612 ppm) imply telemetry
    that does not exist anywhere in the station. Keep as a labeled placeholder, or omit.
12. **Faults bar render-time fallback.** The store contract says offline stamps must come
    from the store, never render time; the shipped UI keeps a render-time clock as a
    last-resort fallback when a stamp is missing. A reconstruction should define the
    fallback (or make it impossible) rather than inherit the contradiction silently.
13. **Mic-profile list transport.** The `mic_profiles` field arrives as a caret-delimited
    string (profiles contain spaces); SmartSDR reports no active-mic name, so
    `state.mic_profile` is the bridge's client-side "last loaded" tracking. Any
    reconstruction of the radio bridge owns this contract; the console only renders it.

> Reference-implementation note (non-normative): the shipped app is
> `hf_console/` (Flutter; packages include an MQTT client, secure storage, provider,
> clock). Deployment details — the Android APK sideload, the iOS self-sideload, the
> `webbridge` Go proxy and its hardened systemd unit on shari (port 8091, dedicated
> no-login user, `MemoryMax=64M`), and the release-manifest requirement that the
> `INTERNET` permission and cleartext-traffic allowance live in the main manifest —
> are covered in `05-deployment-ops.md` and are not normative for behavior.