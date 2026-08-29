# hf_console — operator-facing UI layer (research spec for PRD)

Component: `hf_console/` — Flutter app (Android tablet APK, iPhone IPA, web channel).
Scope of this document: the **operator-facing UI layer and everything it drives** —
`lib/ui/**` (screens, widgets, theme), `lib/main.dart`, plus the supporting services the
widgets read from and publish through (`lib/store/*`, `lib/mqtt/*`, `lib/dxspot/*`).
The reader of this document covers ONLY the operator-facing features; everything here is
written so an engineer who has never seen amateur radio or this codebase can reconstruct
the **behavior** exactly in a different technology stack.

> README.md of the project is the stock Flutter template ("A new Flutter project.") and
> lags badly. The project CLAUDE.md and the code are authoritative; this document is
> built from the code.

---

## 1. Purpose & role

**hf_console is the single console an operator uses to run the whole HF ("high
frequency", 3–30 MHz amateur shortwave) station from a fixed-mount Android tablet on
the operating desk** (an iPhone layout and a browser channel also exist). It is a pure
MQTT *consumer + commander*: it holds no station logic of its own. Every fact it
displays arrives as retained JSON snapshots on an MQTT bus; every operator action is a
single small JSON command published to a `/cmd` topic. The station's behavior (arming,
sequencing, antenna arbitration) lives in server-side bridges; the console's job is
readability, one-tap reachability, and — critically — **operator-safety presentation**:
it must never let stale data or a dead link masquerade as a healthy station, and it must
fail closed on RF-safety questions.

Plain-language glossary (terms defined here are used throughout):

- **Ham/amateur radio**: non-commercial radio hobby; operators exchange contacts
  ("QSOs") worldwide using callsigns.
- **MQTT**: lightweight publish/subscribe protocol. Topics are slash-hierarchical
  strings; a broker relays messages; "retained" messages are the last message on a
  topic, re-delivered to every new subscriber.
- **Slot**: one addressable station component on the bus, e.g. `muehle/hf/pa` (the
  power amplifier bridge). The Mühle station's three-plane convention gives each slot
  four topics: `/meta` (static identity), `/state` (retained JSON telemetry snapshot),
  `/status` (bridge liveness, `online`/`offline` via MQTT Last-Will), `/cmd` (commands).
- **Bridge**: the server-side program (running on the Raspberry Pi "shari",
  192.168.1.139) that translates between one physical device and its slot.
- **TRX/transceiver**: the radio itself (FLEX-8400).
- **PA (power amplifier)**: ACOM 1200S, boosts the radio's output to up to 1200 W.
- **ATU/tuner (antenna tuning unit)**: ATR-1000, an impedance-matching inductor/capacitor
  network placed in the feed line ("in line") or bypassed.
- **Rotator**: motor that turns the mast-mounted beam antenna; azimuth in degrees.
- **Ultrabeam**: a motorized tubular triband beam whose element lengths are motor-tuned
  per band; supports forward / 180° (reverse) / bi-directional radiation patterns and
  full retraction.
- **SWR (standing-wave ratio)**: feed-line impedance match quality, 1.0 = perfect;
  ≥3.0 is dangerous (reflected power heats the PA).
- **DVK (digital voice keyer)**: memory slots in the radio that play back recorded
  audio (e.g. contest exchanges).
- **FT8/FT4**: narrow-bandwidth digital modes; stations exchange automated contacts
  reported as "spots".
- **DX spot**: a report that a distant ("DX") station was heard: who, where, band,
  SNR. "DX" is ham shorthand for long-distance.
- **Maidenhead locator**: geographic grid reference, e.g. `JN58sd` — 2 chars = 20°×10°
  "field", 4 chars = 2°×1° "square", 6 chars = 5′×2.5′ "subsquare".
- **horstreporter**: the station-owner's own web tool that aggregates DX spots
  (FT8/FT4 via MQTT, dxcluster, RBN, WSPR) and serves them over Server-Sent Events.
  The console's DX overlay intentionally mirrors horstreporter's visuals band-for-band.
- **QTH**: the station's own location ("home").
- **AEQD**: azimuthal equidistant map projection — true bearing AND true distance from
  QTH, i.e. exactly what a beam-antenna compass needs.

Place in the station: it is the only human interface to the HF chain
(master mains → 13.8 V PSU → TRX/PA remote-on relays → radio → PA → tuner → antenna
switch → Ultrabeam controller → rotator) plus the station startup/shutdown sequencer.

---

## 2. Upstream interface

The console talks to two independent upstream services. It runs NO server and owns no
devices.

### 2.1 MQTT broker (control/data plane)

- **Transport**: raw TCP MQTT 3.1.1 via `mqtt_client` v10.x on Android/iOS
  (`MqttServerClient`). On web: MQTT over WebSocket via `MqttBrowserClient`
  (`client_factory_web.dart`), which **hard-codes** `ws://192.168.1.139:8091/mqtt` and
  ignores the host/port fields; the browser talks to the `webbridge` Go program on
  shari that byte-forwards the WebSocket to the broker's TCP port (no MQTT framing in
  the bridge, pure bidirectional copy).
- **Broker address**: entered by the operator at setup. Defaults in the setup form:
  host `192.168.1.50`, port `1883`, user `console`. (Project docs describe a shack-local
  broker on shari `192.168.1.139:1883` bridged to the house/HA broker at `192.168.1.50`;
  the two-broker migration was, at time of writing, committed but not yet deployed, so
  the form default is still the house broker. The `console` MQTT account is a dedicated
  low-privilege user — ACL: subscribe `muehle/#`, publish `muehle/+/cmd` — deliberately
  NOT the broad `hf` user.)
- **Client identity**: `clientId = 'hf-console-<unix-ms>'` (fresh per process),
  `startClean()` (clean session — no broker-side session persistence), keep-alive 20 s,
  auto-reconnect on, resubscribe-on-auto-reconnect on.
- **Subscription**: exactly one — `muehle/#` at QoS 0 (`atMostOnce`). All four planes
  (`meta`, `state`, `status`, `cmd`) of every slot arrive through it.
- **Connection-loss detection**: `mqtt_client` keep-alive/disconnect callbacks flip a
  `ValueNotifier<bool> connected`; every UI surface that cares (banner, top-bar dot)
  listens to that notifier. There is no console-side LWT — the console is not a
  monitored component.
- **Publishes**: only `muehle/<slot>/cmd` topics (see §4), QoS 1 (`atLeastOnce`), with
  per-topic retain flags. `publish()` **silently drops** the message when the client is
  null or not in `connected` state — no queueing, no error surfaced in code (the
  link-down banner is the operator-facing warning). A `clear(topic)` helper publishes an
  empty retained payload (used by tests; not exercised by the shipped UI).

### 2.2 horstreporter SSE feed (DX-spot overlay plane)

- **Transport**: HTTPS Server-Sent Events (native build: dart:io HTTP stream with
  hand-rolled SSE frame parsing; web build: `EventSource`).
- **URL** (built by `DxSpotService.streamUrl`):
  `GET {baseUrl}/api/stream?qth={qth}&minutes=30&surroundings=true`
  where `qth` = station callsign if configured, else the Maidenhead locator, and
  `baseUrl` defaults to `https://horstreporter.kgbvax.net`. `minutes=30` makes the
  server replay the last 30 minutes of spots on each (re)connect.
- **Event payload**: one JSON object per SSE `data:` frame — the `streamSpot` shape:
  `lat` (num, degrees, remote station), `lng` (num), `snr` (num → int, dB),
  `ageSeconds` (num → int), `locator` (String, Maidenhead), `band` (String, e.g. `"20m"`),
  `sourceType` (String: `"mqtt"` | `"dxcluster"` | `"rbn"` | `"wspr"`),
  `sender`/`receiver` (String?, dxcluster-only).
- **No keepalives**: the server sends `data:` only when a spot arrives. The native
  client therefore arms an **idle watchdog: 5 minutes without any received line forces
  the connection closed and triggers reconnect**; connect phase is bounded by a
  **15 s connection timeout**. Headers sent: `Accept: text/event-stream`,
  `Accept-Encoding: identity`, `Cache-Control: no-cache`. SSE parsing follows the HTML
  spec (blank line dispatches; one leading space stripped; multi-line `data:` joined
  with `\n`; `:` comments and `event:`/`id:`/`retry:` ignored for data purposes but any
  line resets the watchdog).
- **The overlay is independent of the broker**: it starts whenever a station locator is
  configured, even if MQTT is down, and stops/starts on its own backoff.

---

## 3. MQTT presence

The console **publishes nothing except `/cmd` messages** and **subscribes to
`muehle/#`**. It creates no topics of its own. What it *ingests* (the exact state keys
the UI reads, with defaults when missing) is listed per panel in §4/§5. Ingestion
semantics (`bus_store.dart`):

- Topic split: last path segment is the plane (`meta`/`state`/`status`/`cmd`), the rest
  is the slot address. Unknown plane names are ignored (silently).
- Payload decoding: UTF-8 (malformed bytes allowed), JSON-decoded; non-JSON text kept
  as a raw string (only `/status` legitimately is one: `"online"`/`"offline"`).
- **Empty (or null) payload = cleared plane** — retained-clear semantics. Clearing
  `/state` or `/status` stamps the change time (see §5.4).
- `/state` snapshots are full replacements, not merges.

### 3.1 Monitored "expected slots" (dead-since-boot visibility)

The console pre-declares the slots that must exist. If a slot has never been heard
from since the MQTT link came up, it is reported (see §5.4). The list (exact strings,
`wiring.dart: expectedSlots`):

```
muehle/power/master, muehle/power/psu-13v8, muehle/hf/radio, muehle/hf/ant-ctrl,
muehle/hf/ant-switch, muehle/hf/switch, muehle/hf/pa-arm, muehle/hf/antenna-select,
muehle/hf/pa, muehle/hf/rotator, muehle/hf/tuner, muehle/hf/power-seq,
muehle/hf/discovery, muehle/uhf/rotator, muehle/uhf/pol-ctrl
```

### 3.2 Retain policy per `/cmd` topic (`wiring.dart: cmdRetain`)

Retained (steady-state, self-healing — a bridge re-reading them after restart restores
the commanded state): `muehle/power/master`, `muehle/power/psu-13v8`, `muehle/hf/switch`,
`muehle/hf/pa-arm`, `muehle/hf/ant-ctrl`, `muehle/hf/ant-switch`, `muehle/hf/antenna-select`.
Non-retained (one-shot): `muehle/hf/pa`, `muehle/hf/rotator`, `muehle/hf/tuner`,
`muehle/hf/power-seq`, `muehle/hf/radio`.

---

## 4. Command surface

Every operator action is one `publish(topic, jsonPayload, retain: cmdRetain[topic])`.
`cmdTopic(slot)` = `muehle/{slot}/cmd`. Canonical payload shape is
`{"action": <action>, "value": <value>}` — with documented per-slot deviations that a
reimplementation MUST reproduce byte-for-byte (they are consumed by already-deployed
bridges):

| # | UI source | Topic (retain) | Exact JSON payload | Effect |
|---|-----------|----------------|--------------------|--------|
| 1 | Power panel, MAINS toggle | `muehle/power/master/cmd` (retained) | `{"action":"set_power","value":"on"\|"off"}` | Shelly mains plug on/off |
| 2 | Power panel, PSU toggle | `muehle/power/psu-13v8/cmd` (retained) | `{"action":"set_power","value":"on"\|"off"}` | 13.8 V PSU plug on/off |
| 3 | Power panel, TRX toggle | `muehle/hf/switch/cmd` (retained) | `{"action":"set_trx","value":"on"\|"off"}` | Radio remote-on relay |
| 4 | Power panel, PA toggle | `muehle/hf/switch/cmd` (retained) | `{"action":"set_pa","value":"on"\|"off"}` | PA remote-on relay |
| 5 | Power panel, START STATION | `muehle/hf/power-seq/cmd` (one-shot) | `{"action":"start"}` (no value key) | Runs the ordered startup sequence (powerseq) |
| 6 | Power panel, STOP STATION | `muehle/hf/power-seq/cmd` (one-shot) | `{"action":"stop"}` | Runs the shutdown sequence |
| 7 | Ultrabeam panel, FORWARD | `muehle/hf/ant-ctrl/cmd` (retained) | `{"action":"direction","value":"forward"}` | Elements to forward pattern |
| 8 | Ultrabeam panel, 180° | same | `{"action":"direction","value":"reverse"}` | 180° pattern |
| 9 | Ultrabeam panel, BI-DIR | same | `{"action":"direction","value":"bidirectional"}` | Both-way pattern |
| 10 | Ultrabeam panel, RETRACT | same | `{"action":"retract"}` (no value key) | Fully retract elements (emergency) |
| 11 | Ultrabeam auto-correction (6m) | same | `{"action":"direction","value":"forward"}` | Auto-published by UI, see §5.2 |
| 12 | Antenna panel, port (manual or selector-offline path) | `muehle/hf/ant-switch/cmd` (retained) | **deviation: value-key-only, no `action`** — `{"select":"off"\|"port1"\|"port4"\|"port5"\|"port6"}` | Drive relay switch directly |
| 13 | Antenna panel, port (auto + selector online) | `muehle/hf/antenna-select/cmd` (retained) | **deviation: no `action`** — `{"request":"<same port token>"}` | Ask the reconciler to route (it enforces RF-inhibit ordering) |
| 14 | Antenna panel, AUTO / MANUAL | `muehle/hf/antenna-select/cmd` (retained) | `{"request":"auto"\|"manual"}` | Reconciler policy mode |
| 15 | PA panel, OPERATE | `muehle/hf/pa/cmd` (one-shot) | `{"action":"set_mode","value":"operate"}` | Amp to operate |
| 16 | PA panel, STANDBY | same | `{"action":"set_mode","value":"standby"}` | Amp to standby |
| 17 | PA ARM panel, ARM/SAFE | `muehle/hf/pa-arm/cmd` (retained) | `{"action":"set_enabled","value":"true"\|"false"}` — **value is a STRING "true"/"false", not a JSON bool** | Enables/disables the PA arm interlock slot |
| 18 | Tuner panel, BYPASS / inline | `muehle/hf/tuner/cmd` (one-shot) | `{"action":"set_inline","value":false\|true}` — **value is a real JSON bool** | ATU in/out of the feed line |
| 19 | Tuner panel, TUNE MEM | same | `{"action":"tune","value":"mem"}` — **value is a STRING** | Memory-based tune |
| 20 | Tuner panel, TUNE FULL | same | `{"action":"tune","value":"full"}` | Full search tune |
| 21 | Compass tap-to-aim | `muehle/hf/rotator/cmd` (one-shot) | `{"action":"set_az","az":<double degrees>}` — **argument key is `az`, not `value`** | Slew beam to azimuth |
| 22 | Rotator presets NA/SA/VK/JA | same | `{"action":"set_az","az":330\|210\|60\|35}` | Same, preset |
| 23 | Rotator STOP | same | `{"action":"stop"}` (retain forced false) | Halt motion |
| 24 | TRX band buttons | `muehle/hf/radio/cmd` (one-shot) | `{"action":"set_band","value":"80m"\|"40m"\|"20m"\|"17m"\|"15m"\|"12m"\|"10m"}` | Radio band change (QSY) |
| 25 | TRX DVK1–4 buttons | same | `{"action":"dvk_play_1"…​"dvk_play_4"}` (id clamped 1–12 in the builder) | Play voice memory slot n |
| 26 | TRX DVK STOP | same | `{"action":"dvk_stop","value":""}` | Stop playback (empty-string value) |
| 27 | Mic profile row | same | `{"action":"set_mic_profile","value":"<profile name>"}` | Load a SmartSDR mic profile |

Guarding of each of these is per-panel (§5). All are dropped silently if the MQTT link
is down. All are disabled at the widget level when their owning slot is offline
(two-layer liveness, §5.1).

---

## 5. Behavior & state machine — panel by panel

### 5.0 App boot, pages, and layout

Boot sequence (`main.dart`):

1. `immersiveSticky` full-screen (Android-only no-op elsewhere); all orientations
   allowed — layout, not orientation locks, decides the shape.
2. Root state reads all stored credentials (see §6). Configures + starts the DX-spot
   service **regardless of broker state** (it no-ops when no locator is set).
3. If host+port+user+non-empty password are all present, attempts auto-connect. Failure
   is swallowed — the console still opens and shows the link-down banner. If ANY of
   those is missing, the **SetupScreen** is shown instead (and stays the home until
   credentials are saved).
4. A store listener re-evaluates the DX SNR filter on every bus update from the live
   radio mode (`muehle/hf/radio` `state.mode`).

**Page structure** (single screen, `ConsoleScreen`): a top-level column of
[LinkStatusBanner] + [page content] + [FaultsBar — only on non-HF pages]. Three pages
via tabs **Station / HF / UHF** plus a color-scheme picker (DC / PA / FO), a DX-settings
gear, an MQTT connection indicator, and an all-online tag.

- **HF page** (default): tablets/landscape — two columns. Left: DX map container on top
  (fills), then Ultrabeam panel, Antenna panel, Rotator presets bar. Right: top bar,
  then a scroll column of PA panel, PA-arm panel, Tuner panel, TRX/DVK panel, then the
  FaultsBar. Right column width = 44% of viewport when ≥1200 px wide and ≥720 px tall,
  else 48%; minimum width 420 px (compact: 320 px). Phones (`shortestSide < 600`):
  single vertical scroll — DX map on top (flex 5), then all panels in order Ultrabeam,
  Antenna, Rotator presets, PA, PA-arm, Tuner, DVK, Faults (flex 6).
- **Station page**: top bar, then Power panel and Climate panel, scrolling.
- **UHF page**: placeholder text "UHF controls are not yet wired." (centered, muted).
- **FaultsBar** appears on every page (embedded in the HF page's columns; full-width
  footer on Station/UHF).

Top-bar indicators (operator-safety surfaces):

- **Connection indicator**: 10 px dot, green glow + "MQTT" when connected, red + "OFFLINE".
- **Online tag**: pill reading `● all online` (green) or `● N offline` (red), where N =
  the computed offline list length (§5.4).

### 5.1 Link-status banner (operator-safety state #1)

While `MqttService.connected` is false, a full-width red strip renders at the very top
of every page:

> `LINK DOWN — DATA STALE · COMMANDS NOT DELIVERED`

red 18%-blend background, red mono text, letter-spacing 0.14. It is unmissable by
design: everything else on screen is stale retained state and every tap is silently
lost, and the copy promises exactly that (taps are not disabled panel-by-panel —
commands are simply not delivered). The banner disappears the instant the link returns.

### 5.2 Ultrabeam panel (antenna controller)

Reads slot `muehle/hf/ant-ctrl` (`direction` string default `"forward"`, `moving` bool
default false) and `muehle/hf/radio` (`band` string) and the controller's own `band`.
Header pill, in priority order: `OFFLINE` (red) → `MOVING` (red) →
`direction.toUpperCase()` (accent) → **`BAND MISMATCH · <ctrlBand> ≠ <radioBand>`**
(red). The mismatch check is deliberately conservative: it fires only when the slot is
online AND both bands are "comparable" — non-empty, not `gen`/`unknown` (flexbridge's
out-of-allocation labels) and not starting with `band-` (ultrabridge's) — and differ.
The two bridges label unknown frequencies differently and can agree on frequency while
disagreeing on label; pre-first-state silence must not cry wolf.

Buttons: **FORWARD** (active when direction==forward), **180°** (reverse),
**BI-DIR**, **RETRACT** (danger styling). Guards:

- All four require the slot online.
- Direction buttons are locked while `moving` (rapid presses can't queue competing
  motor commands mid-travel).
- **RETRACT stays pressable while moving** — it is the designated emergency action.
- **6m auto-correction** (operator-safety behavior, driven by the radio's band on
  ANOTHER slot): on 6 m the Ultrabeam's elements support only forward — 180° and
  BI-DIR buttons are disabled (greyed) on 6 m, and if `direction != forward` while
  the radio is on 6 m and the controller is online and not moving, the UI itself
  auto-publishes `direction=forward` **once per invalid state** (a latch flag
  `_forwardForced` prevents republish-per-rebuild; the flag clears when the state
  resolves or while moving, so the correction re-fires after travel if the state is
  still invalid). The auto-correction obeys the same moving lockout as manual buttons.

Right of RETRACT sits "Horst-Kevin", a 64 dp band-heckling-dragon mascot image
(`assets/img/hk-removebg.png`, tooltip "Horst-Kevin — band-heckling dragon") — a
deliberate bit of personality in the otherwise sober console.

### 5.3 Antenna / routing panel

Reads `muehle/hf/ant-switch` (`selected` string, `settled` bool),
`muehle/hf/antenna-select` (`mode` string), plus cross-panel RF state from
`muehle/hf/radio` (`tx` string, `tuning` bool, `device_online`) and `muehle/hf/pa`
(`keyed` string). Port→label map (source of truth: antennaselect wiring; port2/port3
unwired at this site and omitted from the UI):

```
off → Grounded, port1 → Dummy load, port4 → Ultrabeam,
port5 → Port 5, port6 → Fan dipole 80/40
```

Rendered port buttons: `off, port1, port4, port5, port6`. A missing/unknown `selected`
renders **`?` (Unknown)** — never as a port, least of all `off`, which would paint a
dead bridge as a deliberate grounded-safety state (regression-tested).

Header: `"{modeLabel} · {antName}"` where modeLabel is the reconciler mode uppercased
if the selector slot is online and reports a mode, else **`DIRECT`** (no fabricated
"auto" when nobody enforces the policy). Color: red when blocked (grounded or manual
override), accent when managed+settled, amber when managed+not-settled or unmanaged.
Extras: while relays are in flight (`settled == false`) an amber **`NO RF`** tag plus a
pulsing amber dot (700 ms animation); **`RF ON`** (red) whenever RF is detected
anywhere; **`RF ?`** (amber) when direct-drive is active and RF state is unknown.

**Cross-panel cold-switch RF guard** (operator-safety state — driven by OTHER slots):
RF is "on" if ANY of three independent paths says so — radio `tx == "tx"`, radio
`tuning == true`, PA `keyed == "tx"`. RF is "safe" only if the radio link is up AND
`tx == "rx"` AND `tuning != true` AND `keyed != "tx"` — i.e. **unknown blocks
(fail-closed)**, confirmed-RX allows.

Routing commands:

- If reconciler is in `manual` mode, or the selector slot is offline/absent
  ("direct drive"): taps publish `{"select": port}` to `ant-switch` — and are
  **blocked unless RF-safe** (fail closed; a relay moving RF would arc).
- If reconciler is online in auto mode: taps publish `{"request": port}` to
  `antenna-select` — the reconciler path is **exempt from the console-side RF guard**
  because the reconciler arbitrates RF-inhibit ordering itself.
- AUTO / MANUAL mode buttons publish `{"request": "auto"/"manual"}` to
  `antenna-select` (only when the selector slot is online).

Red-state rendering (operator-safety states): the **GROUNDED** button renders in solid
red even while it is the active selection (grounded = no antenna connected = operating
impossible, must shout); the **MANUAL** button renders solid red while manual override
is engaged.

### 5.4 Liveness, faults, and offline reporting (operator-safety states #2–#5)

**Two-layer liveness** (a station-wide invariant the console renders correctly): a slot
is `isOnline` only if BOTH (a) `/status` == `"online"` (bridge process alive — LWT), and
(b) `/state.device_online == true` (device link up). Logic slots (no physical device —
`antenna-select`, `power-seq`, `hadiscovery`) omit `device_online`; a state snapshot
without the key counts as online once it has arrived; `state == null` counts as
offline. The store stamps, per slot, when `/status` last changed
(`statusChangedAt`), when `device_online` flipped (`deviceChangedAt`), and when the
MQTT link came up (`_connectedAt`). An empty retained payload on `/status` or `/state`
is itself a change and is stamped.

**Offline rows with "when did it go dark" stamps** (operator-safety state #3): the
computed offline list contains, for every slot ever heard from:

- `"{addr}: bridge down"` when `/status != "online"` — stamped with `statusChangedAt`;
- `"{addr}: device unreachable"` when the bridge is up but the device link is down —
  stamped with `deviceChangedAt ?? statusChangedAt`.

**Silent expected slots after a connect grace period** (operator-safety state #4):
after each (re)connect a **3-second grace** timer runs (the reconnect re-floods
retained state, so connect-time silence must not trip the report; the grace is also
deliberately NOT "after first message", because a broker with zero retained payloads
delivers nothing yet is exactly the dead-station case). After the grace, every expected
slot (§3.1) never heard from at all is reported as `"{addr}: silent (no state since
connect)"`, stamped with the connect time. A one-shot `Timer(3 s)` forces a UI rebuild
so the report appears even if the band is quiet enough that no further bus message
arrives. The report clears when the slot publishes anything.

**Fault history** (from `/state.fault` and `/state.error` on every slot): active fault
text = `error.toUpperCase()` if error is non-empty and not `NONE`, else
`fault.toUpperCase()` if non-empty and not `none` — error outranks fault. Timestamp
from `state.ts` (fallback: now, ISO-8601). A previously active record for the address
whose text no longer matches is marked cleared; an exact repeat re-activates the record
AND refreshes its timestamp (repeated faults stay at the top and read as ongoing).
History caps at **30** records (oldest evicted).

**PSU-off root-cause naming** (operator-safety state #5): when the 13.8 V PSU bridge is
itself online, its `state.power` is confirmed `"off"`, and any `muehle/hf/` slot
appears in the offline list — a switched-off PSU silently kills the whole HF control
chain (hf/switch, pa-arm, ant-switch, everything on 13.8 V) — the bar adds the
explanatory top line `muehle/power/psu-13v8: PSU OFF — HF control chain unpowered`.
Confirmed-off only: a missing `power` key is unknown, and inferring a fault from an
absent key is explicitly forbidden (regression-tested).

**FaultsBar rendering**: shows "FAULTS" header + tag (`N ACTIVE` red / `ALL OK`
green). Rows: history records plus offline entries, **except** that an address with an
active fault suppresses its generic offline line (the fault text says more). Rows
carry an HH:MM:SS stamp. Sort: active first, then newest first. **Visible list capped
at 4 rows.** Empty state: "No faults or offline devices".

### 5.5 Rotator compass panel

Reads `muehle/hf/rotator` (`az` double degrees, `target_az` double, `moving` bool) and
`muehle/hf/ant-ctrl` (`direction`). Draws a circular compass disc:

- Ticks every 30°, majors every 90° (12 px vs 6 px); cardinal labels N (accent, 14) /
  E, S, W (muted, 12).
- **Beam lobes** from the Ultrabeam direction: forward = one wedge of half-width 30° at
  `az` (accent); reverse = one at `(az+180) % 360` (amber); bidirectional = both, each
  half-width 45°. Filled wedge (40–55% blend) + 3.5 px radial line + arrowhead at rim.
- White boom line from center to `az`, 5 px accent center dot.
- **Target line**: a semi-transparent accent line at `target_az`, drawn only when
  `|target_az − az| > 5.0°`; the header chip likewise shows the target
  (`"123° → 330°"`) only beyond 5°.
- Chrome: top-left DX status badge (`"DX 42"` count / `"DX …"` connecting / `"DX ✗"`
  feed down — hidden entirely when the overlay is off); top-right azimuth chip
  `parts.join(' · ')` from `["123°", "→ 330°"?, "MOVING"?, "OFFLINE"?]` — red while
  moving, muted while offline, accent otherwise; a zoom badge (`"1.50×"`); a band-key
  rail on the left listing only bands present in current spots, in canonical order
  160m,80m,60m,40m,30m,20m,17m,15m,12m,10m,6m, each chip a 7 px swatch of the fixed
  horstreporter band color (fades in/out over 180 ms).
- **Zoom** (mirrors horstreporter exactly): range **[1.0, 5.0]**, default **1.5**, step
  **0.2**, NaN→default, ±inf clamps to the bound, buttons disabled within 1e-9 of the
  bounds. Discoverable UI: stacked +/− buttons bottom-right (40×28, 48-dp rule
  relaxed deliberately for the two-button stack). Power gesture: vertical drag on the
  disc's **right gutter** (right half, inside the disc radius) adjusts zoom, drag up =
  zoom in, ~30 px per 1.0 zoom step — only available while the DX overlay is active.
- **Tap-to-aim**: a tap anywhere on the disc (while the rotator slot is online)
  converts the tap angle to a compass azimuth
  (`((atan2(dy,dx)·180/π + 90) mod 360 + 360) mod 360`) and publishes
  `{"action":"set_az","az":<deg>}`. When the rotator is offline the disc gesture is
  disabled and the chip reads OFFLINE.
- **DX overlay on the disc**: AEQD-projected landmasses (bundled Natural Earth 50m
  country polygons, `assets/geo/world.geojson` — ~3 MB raw, ~1.6k rings / ~100k
  vertices, loaded lazily once, malformed input → empty list, never throws), plus
  4-char-Maidenhead grid-square quads filled with the dominant band color at
  `gridSnrOpacity` (below), stroked 0.6 px. Squares are drawn at ANY projected size
  (a regression the user flagged live: squares must not vanish at low zoom); the only
  skips are corners off-disc (antipode wrap) or centroid off-disc. Scale:
  `r · zoom / π`; AEQD is clipped 0.02 rad shy of the antipode.
- A rasterized world-layer cache (keyed on cx, cy, r, zoom, center, ring count, fill +
  stroke colors) keeps zoom drags at a single GPU blit — implementation detail, but
  the *perf contract* (interactive zoom on a tablet) must be preserved.

**Rotator presets bar** (separate card below): `NA 330`, `SA 210`, `VK 60`, `JA 35`
(NA = North America, SA = South America, VK = Australia, JA = Japan — ham country
prefixes), all publishing `set_az`; `STOP` publishes `{"action":"stop"}` (retain
forced false). All five disabled when the rotator slot is offline — the same gating as
tap-to-aim. Buttons 44×32 (compressed row).

### 5.6 DX map container (projection switch) and Mercator panel

The map area hosts two interchangeable projections sharing one DxSpotService, toggled
by icons top-right (explore = azimuthal compass, map = Web Mercator), plus a filter
label chip: `"SNR off"` when gating is disabled, else `"SSB ≥ 0dB"` / `"CW ≥ -15dB"`.

**Mercator panel**: pan by drag (drag moves the map opposite), wheel/scroll zoom,
zoom range **[1.0, 12.0]**, default **2.5**, step **0.5**, buttons disabled within
1e-9 of bounds; a crosshair "reset" button re-centers on QTH and restores zoom 2.5.
Until the user pans, the view is pinned to QTH. Paints: page-colored background, land
fill + coastline stroke (even-odd fill so GeoJSON hole rings subtract; antimeridian
cuts detected by |Δlng| > 180° breaking the subpath), grid-square quads (dominant-band
fill at SNR opacity, +0.2-opacity stroke), spot dots (radius 2.5, band color, 0.6 white
stroke for contrast), and a QTH marker (accent dot, radius 5, page-color ring).
Projection is standard EPSG:3857 (spherical Mercator, 256 px tile, lat clamp
±85.05112878°, longitude wrapped relative to center so antimeridian crossings render
as a short jump).

### 5.7 DxSpotService (data behind both maps)

- **Only `sourceType == "mqtt"` spots (FT8/FT4) are ingested** — dxcluster/rbn/wspr are
  dropped at ingest so the band-key and grid palette match horstreporter's azimuthal
  view. Spots also need a callsign or locator to be placeable/labelable.
- **Mode-aware SNR gate** driven live by the radio mode: `usb/lsb/am/fm/data` → SSB
  family, threshold **0 dB**; `cw` → CW family, threshold **−15 dB**; anything else
  (unknown/blank) → gating OFF. Spots below threshold are dropped. Filter changes do
  not clear current spots; a reconnect re-ingests history under the new threshold.
- **Dedup/refresh key**: `"{locator}|{receiver}|{sender}|{band}|{sourceType}"`; keeps
  the freshest (lowest server age, tie-break higher SNR), and ALWAYS refreshes the
  last-heard time (so an actively re-spotted station doesn't blink off).
- **Age cap 600 s** of *live* age (server `ageSeconds` + elapsed since receipt),
  enforced on ingest and by a 60 s prune timer (so a silent feed can't leave stale
  dots). **Cap 80 spots**, sorted by live age then SNR descending.
- **Reconnect/backoff**: 2 s initial, ×1.5 per failure, cap 60 s, reset to 2 s on any
  good event. A disconnect sets error "horstreporter feed down" (shown as `DX ✗`).
- **Notify throttled to ≤ ~2 Hz** (500 ms coalesce) so a busy FT8 band can't
  repaint-storm.
- **Grid squares**: spots grouped by 4-char locator prefix (spots with a band only);
  dominant band = most-reported band; opacity score = mean of the top quarter of SNRs
  (ties: `ceil(n/4)` clamped ≥1), feeding `gridSnrOpacity`: 0 dB → 0.45, −10 dB →
  0.15, slope 0.03/dB below zero and 0.015/dB above, floor 0.10, cap 0.75.
- The overlay is **active iff a station locator is configured** (which also sets the
  AEQD center via Maidenhead decode: 2/4/6-char supported, centers of cells); without
  it the compass is beam-only and the map panels render without overlay data.

### 5.8 PA panel (ACOM 1200S)

Reads `muehle/hf/pa`: `mode` (default standby), `keyed` (rx/tx/inhibited, default rx),
`fault` (default none), `error`, `temp_c` (num °C), `fwd_power_w` (num, 0–1200 W),
`swr` (num, 1.0–4.0), `power`; plus cross-slot `muehle/hf/switch.state.pa` (remote-on
relay) and the panel's own liveness. Renders:

- **FWD power meter**, full scale **1200 W**, bar labels `0 / 500 / 1000 / 1200`, green
  gradient into orange at the hot end. Two ballistic markers over the bar: a downward
  triangle at the **1-second rolling-window peak** (white) and an upward triangle at
  the **rolling 95th percentile** (accent, linear-interpolation percentile). Markers
  snap up instantly and then decay linearly at **24 W per 100 ms tick** (full-scale
  1200 W drains in ~5 s), never below the live reading; the decay timer runs only
  while a marker stands above the live value. A constant reading refreshes its
  sample timestamp so it stays "present" in the window.
- **SWR meter**, full scale **4.0**, labels `1.0 / 1.5 / 3.0 / 4.0`, amber fill. (There
  is deliberately no reflected-power readout — regression-tested.)
- Buttons: **OPERATE** (green when active) and **STANDBY** (amber when active), both
  96×34, disabled offline, publishing `set_mode`.
- **Status tag priority** (cross-panel RF-safety presentation, driven by other slots):
  `OFFLINE` (muted) → fault/error text uppercased (red) → `● TX` (red; a live transmit
  outranks relay bookkeeping) → `RELAY ?` (amber; hf/switch state unknown — a missing
  key must never be asserted as OFF) → `PA RELAY OFF` (amber) → `PA OFF` (amber; amp's
  own power telemetry) → `INHIBITED` (amber) → `OPERATE` (green) / `STANDBY` (amber),
  each with ` · {temp} °C` appended when temp > 0.

### 5.9 PA ARM panel

Reads `muehle/hf/pa-arm`: `enabled` (bool), `armed` (bool), `error` (string). One
toggle button labeled **ARM** (when disabled — danger styling) or **SAFE** (when
enabled), publishing `set_enabled` with the inverted value. Tag priority: `OFFLINE`
(muted) → error text (red) → `ARMED` (red — the interlock is hot, the amp can be keyed)
→ `ENABLED` (amber) → `SAFE` (green).

### 5.10 Tuner panel (ATR-1000)

Reads `muehle/hf/tuner`: `inline` (bool), `settling` (bool), `fault`, `swr` (num).
Buttons: **BYPASS** (active-amber when not inline), **TUNE MEM**, **TUNE FULL** — the
two tune buttons read **`TUNING…`** and are locked while `settling` (a second tune
queued against a settling tuner just competes with the in-flight one). BYPASS always
enabled while online. Tag: `OFFLINE` → fault uppercased (red) → `TUNING` (amber) →
`IN LINE · SWR {x}` — colored by SWR: **≥3.0 red, ≥2.0 amber, else green** (3.5:1 and
1.1:1 must not read alike) → `BYPASS` (amber — a bypassed tuner is a degraded TX path,
not neutral info).

### 5.11 TRX / DVK panel (FLEX-8400)

Reads `muehle/hf/radio`: `freq_hz` (int, Hz), `band`, `mode`, `tx` (rx/tx), `drive`
(int %), `dvk_status` (idle/playback/recording/preview/disabled), `dvk_id` (int).

- **Readout chip**: `14.074 MHz` (3 decimals) · mode uppercased (accent) · band · TX/RX
  pill (red TX / green RX) · drive% · DVK suffix when not idle:
  `PLAYBACK · M{n}` (accent) / `RECORDING`, `PREVIEW` (amber) / `DISABLED` (red).
  Offline → a red-bordered `OFFLINE` chip.
- **Band buttons**: `80 40 20 17 15 12 10` (labels rendered `80m`…), active one
  highlighted, publishing `set_band` one-shot.
- **DVK buttons** `DVK1–DVK4` publish `dvk_play_n`; active highlight while
  `dvk_status == "playback"` and `dvk_id` matches. **STOP** (danger) publishes
  `dvk_stop` with empty-string value.
- **Mic profile row**: three buttons bound (by name, persisted on-device) to SmartSDR
  mic profiles. Tap a bound button → `set_mic_profile <name>`. Tap an unbound button →
  pick-from-list dialog (the list is `muehle/hf/radio.state.mic_profiles`, an array of
  strings the bridge queries from the radio on connect — populated shortly after the
  radio comes online; empty list → an informational dialog, no manual entry) then
  binds the button AND activates the profile. Long-press → dialog with **Associate**
  (bind another existing profile without activating) and **Unbind** (bound buttons
  only). The active name (`state.mic_profile`) renders as a `MIC: <name>` label and
  highlights the matching bound button; it is empty until the first profile is loaded
  via the bus (SmartSDR reports no active-mic name; the bridge tracks "last loaded").
  Bindings persist in `SharedPreferences` under keys `mic_profile_btn1`…`mic_profile_btn3`.
  There is deliberately no Save — profiles are created/edited in SmartSDR itself.

### 5.12 Power panel (Station page)

Reads `muehle/power/master.power`, `muehle/power/psu-13v8.power`,
`muehle/hf/switch.trx`/`.pa`, `muehle/hf/power-seq.phase`/`.fault`, and each slot's
liveness. Layout: **START STATION** (green styling, 76×52 two-line label) · **STOP
STATION** (danger) · four relay toggles (MAINS, PSU 13.8V, TRX, PA — 34×18 switch
pills, 120 ms thumb animation, ON green / OFF faint / OFFLINE red text with a tooltip
"<name> offline — relay uncontrollable"; each toggle gated on its slot being online) ·
SEQUENCE readout.

**Start-button gating** (operator-safety state): START is enabled only when the
power-seq slot is online AND `phase` is NOT `running` and NOT `starting`. (Note: phase
`stopping` leaves START enabled — see §9.) STOP is enabled whenever power-seq is
online. Sequence label: `FAULT` (red, if `fault` non-empty) / `ON` (green, running) /
`STARTING` (green) / `STOPPING` / `IDLE` (muted).

### 5.13 Climate panel

A **static placeholder** — renders hard-coded `HEAT` (on), `COOL` (off), `21.4 °C`,
`612 ppm`. No bus topics, no commands. (HVAC control is not yet wired anywhere in the
station.)

### 5.14 Setup flows and credential handling

**First-launch setup screen** (shown iff any of mqtt_host / mqtt_port / mqtt_user /
mqtt_password is missing — password must be non-empty): a centered card "MÜHLE · HF"
with fields Host (default `192.168.1.50`), Port (`1883`), Username (`console`),
Password (obscured, eye toggle), and an optional "DX overlay" section — Station locator
(e.g. `JN58sd`, upper-cased on save) and Horstreporter URL (default
`https://horstreporter.kgbvax.net`). CONNECT saves all six values, attempts a connect
(swallowing failure — the console opens and shows the banner), then configures and
restarts the DX service, then switches to the console. Validation: host, user and
password must be non-empty or the save is a silent no-op.

**Why the setup screen is unreachable after credentials are set, and how config is
reached**: boot only routes to SetupScreen when a credential is missing; a provisioned
tablet boots straight to the console and there is deliberately no "settings" page for
broker credentials (they live only in secure storage; clearing them requires clearing
app storage / reinstalling). The DX-overlay keys — locator and horstreporter URL —
were added later and needed a reachable editor, so the **top-bar gear ("DX overlay
settings", tune icon) opens `showDxConfigSheet`**, a modal dialog editing ONLY
`station_locator` and `horstreporter_base_url` (same defaults; locator upper-cased on
save). Save persists both keys and **live-applies** them to the running DxSpotService
(configure + restart) without touching the MQTT link. Broker credentials cannot be
changed from inside the app.

### 5.15 Theme and visual language

Three color schemes, user-switchable at the top bar at any time (DC / PA / FO buttons):

- **dc** (default, "dark"): near-black page `#0A0C10`, pane `#0F1218`, card `#151923`,
  land `#232C3E`, lines `#2A3142`/`#3D4558`, text `#DEE3EC`/`#8C97AB`/`#5B6579`
  (normal/muted/faint), accent cyan `#5FB2C9`, green `#5CCB8A`, amber `#D7B04A`,
  red `#D9685C`, orange `#D99A5F`.
- **paper** (light): warm-grey page `#E5E3DD`, card `#F9F8F5`, ink text `#1A1A1A`,
  accent `#005F99`, green `#1E6B48`, amber `#A36A00`, red `#B52E1D`, orange `#C45F18`.
- **forest** (dark green): page `#151A17`, accent gold `#C8A45C`, green `#6DB88B`…

Typography: **SairaCondensed** for display, **IBMPlexSans** body, **IBMPlexMono** for
all data/labels/buttons — all bundled as assets (no runtime font fetch); every
`AppTheme.*` text style adds +1 to the requested point size (a quirk to preserve or
drop consciously). Buttons: flat, 4 px radius, 1 px border, pane background when
inactive, accent fill + black/white foreground when active, red-tinted for danger,
solid red fill for "dangerActive" states (grounded, manual, armed); minimum touch
target 44×40 (48×48 for icon buttons) with two documented deliberate exceptions
(rotator presets 44×32, compass zoom stack 40×28). A purple-bordered "power" card
variant marks the power panel.

**Fixed horstreporter band palette** (NOT theme tokens — identical in all three
schemes, so a spot keeps its color across theme switches and matches the horstreporter
web frontend): `160m #8B0000`, `80m #800080`, `60m #4B0082`, `40m #0000FF`,
`30m #03B1B1`, `20m #008000`, `17m #808000`, `15m #FFA500`, `12m #00FFFF`,
`10m #FF0000`, `6m #FF00FF`, `4m #FF1493`, `2m #008080`, unknown → grey `#555555`.
Source-type fallback colors (when a spot has no band): mqtt→green, dxcluster→accent,
rbn→amber, wspr→orange, other→txtMute.

Type scale for width: ≥1400 px → ×1.25, ≥1200 px → ×1.1 (helper exists; the widgets
mostly size text directly).

### 5.16 Design lineage (sas/)

The approved high-fidelity reference is `sas/tablet_console_hybrid_preview.html` —
"hybrid (DCs + at-a-glance grid)": the **DCs** direction's color scheme and controls
(near-black cards, cyan/green/amber/red semantics, monospace readouts) combined with
the at-a-glance console-grid layout from a design handoff. Other previews explored but
not chosen: `tablet_console_airbus_preview.html` (Airbus/process-control look),
`tablet_console_dcs_preview.html` (dark-dense), `tablet_console_exact_preview.html`
(exact design-system). `sas/hf_console_redesign_plan.md` defines the brief — a
fixed-mount tablet console where every action is one tap, readable in dim light,
"no neon-on-black, glassmorphism, or corporate-dashboard look" — and three
information-architecture directions: **A "Shop Panel"** (chunky industrial grid),
**B "Logbook Strips"** (full-width ruled strips, paper-logbook voice), **C "Split
Deck"** (persistent left world-pane + page-switching right command pane). The shipped
HF layout is direction C's split-deck skeleton (map left, controls right) rendered in
the DCs dark visual language. The plan's fault-strip and offline-state recommendations
became the FaultsBar and the per-panel offline gating.

---

## 6. Configuration

All configuration is user-entered at runtime and stored per-device; there is no config
file, no build-time config, no CLI flags.

| Key | Meaning | Default | Storage |
|-----|---------|---------|---------|
| `mqtt_host` | Broker host | form default `192.168.1.50` | secure storage |
| `mqtt_port` | Broker port (stored as string) | `1883` | secure storage |
| `mqtt_user` | MQTT username (dedicated `console` account) | `console` | secure storage |
| `mqtt_password` | MQTT password (must be non-empty) | — | secure storage |
| `station_locator` | Station Maidenhead locator; sets DX-overlay center; empty = overlay off | — | secure storage |
| `horstreporter_base_url` | SSE feed base URL | `https://horstreporter.kgbvax.net` | secure storage |
| `station_callsign` | Optional; used as `qth=` in the SSE URL in preference to the locator | — | secure storage |
| `mic_profile_btn1..3` | Mic-profile button bindings | unbound | SharedPreferences (plain) |

Secrets handling: on Android the keys live in `flutter_secure_storage` → Android
Keystore; on iOS → Keychain (no sharing entitlement needed). **On web there is no
secure storage**: the store falls back to plain `SharedPreferences` — i.e. the MQTT
password sits in browser localStorage on the web channel. The web client also ignores
host/port entirely (hard-coded WebSocket URL), but the credentials still pass through
to the broker over the bridge.

Hard-coded (not config): world.geojson asset, band palette, `assets/img/hk-removebg.png`
mascot, fonts, the 15 expected slots, the antenna port map, web `ws://…` URL.

---

## 7. Deployment

Three channels from one Flutter codebase:

1. **Android tablet APK** (primary): `tool/prebuild.sh` gate (flutter analyze +
   flutter test), then `flutter build apk --release`; sideload
   `build/app/outputs/flutter-apk/app-release.apk` (via `adb install -r`, which keeps
   stored credentials). Note from project memory: Flutter scaffolding puts INTERNET
   permission only in debug/profile manifests — `src/main/AndroidManifest.xml` must
   carry `INTERNET` (and cleartext allowance for raw-TCP MQTT to a LAN IP) or the
   release APK has no socket authority.
2. **iPhone IPA** (self-sideloaded, Xcode-managed signing, no App Store): phone layout
   via the `shortestSide < 600` branch; raw-TCP MQTT is exempt from iOS ATS (ATS
   governs only NSURLSession/HTTP/WS), so no `NSAllowsArbitraryLoads`; the HTTPS
   horstreporter feed is ATS-safe. `tool/build-ios.sh` runs the prebuild gate,
   regenerates launcher icons, builds the IPA.
3. **Web channel on shari**: `./deploy.sh` (env-configurable: SSH target default
   `io@192.168.1.139`, service `hf-console-web`, user `hfconsoleweb`, install dir
   `/opt/hf-console-web`, port 8091, broker `192.168.1.50:1883`). It runs the prebuild
   gate, builds `flutter build web --release --base-href /`, cross-compiles the Go
   `webbridge` (linux/arm64, CGO off), generates and installs a **hardened systemd
   unit** (`NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
   `PrivateDevices`, kernel/namespace/realtime/SUID restrictions, empty capability
   bounding set, `MemoryMax=64M`, `TasksMax=32`, `Restart=on-failure`/5 s), creates the
   dedicated no-login service user, swaps the static tree atomically via /tmp, enables
   + restarts. Serves `http://shari:8091/`; `/mqtt` is the byte-forwarding WebSocket
   endpoint to the broker (any origin accepted — LAN-only service).

The console itself is never a systemd service on shari (only its web channel is).

---

## 8. Invariants & safety rules (must NEVER be violated)

1. **Fail closed on RF**: a direct-drive antenna-switch port change requires the radio
   link up AND `tx == "rx"` AND `tuning != true` AND PA `keyed != "tx"`. Unknown radio
   state blocks the move. Only the reconciler (antenna-select, online, auto mode) path
   is exempt — it enforces inhibit ordering itself.
2. **Never fabricate OFF from missing data**: a missing `hf/switch.pa` shows `RELAY ?`,
   never "PA RELAY OFF"; a missing PSU `power` key never triggers the PSU root-cause
   line; a missing `ant-switch.selected` shows Unknown, never Grounded; an absent
   selector never shows "AUTO".
3. **No command while offline**: every command button is disabled unless its owning
   slot passes two-layer liveness (bridge online AND device online).
4. **Ultrabeam motion discipline**: direction commands locked while elements are
   moving; RETRACT always available while online (emergency); on 6 m only forward is
   possible — the console itself force-publishes forward when the radio is on 6 m and
   the direction is invalid (once per invalid state).
5. **PA arm is an interlock**: ARMED renders red; arming goes through a retained
   `set_enabled` so a pa-arm bridge restart restores the armed state.
6. **Sequence discipline**: START STATION is disabled while the sequencer reports
   running/starting. STOP remains available whenever the sequencer is online.
7. **Stale data must be loud**: link down ⇒ full-width banner; the offline/faults bar
   stamps when each slot went dark (never render-time); silent expected slots are
   reported after the 3 s grace.
8. **A live TX outranks plumbing** in the PA tag (`● TX` red beats "relay off" amber).
9. **Payload shapes are contracts**: the value-key deviations (`{"select":…}` /
   `{"request":…}` with no `action`; `pa-arm` string booleans; `tuner` real bool and
   string tune modes; `rotator` `az` key; `dvk_stop` empty-string value) must be
   reproduced exactly — deployed bridges parse them as-is.
10. **The DX overlay never interferes with control**: it is read-only display; the
    compass right-gutter zoom gesture and tap-to-aim do not overlap (tap = aim,
    right-gutter vertical drag = zoom).
11. **Band colors are fixed** across themes (horstreporter correspondence is a
    cross-component visual invariant).
12. **No station logic in the console**: it renders and commands; it never sequences,
    arbitrates, or caches policy decisions of its own (the single exception — the 6 m
    force-forward correction — is a documented device-physics guard, mirrored from
    ultrabridge's own web UI).

---

## 9. Known defects & fragilities

- **README.md is a stock Flutter template** — useless; CLAUDE.md + code are truth.
- **Web channel stores the MQTT password in plain localStorage** (no secure storage in
  browsers) and hard-codes `ws://192.168.1.139:8091/mqtt` — LAN-IP brittleness if shari
  ever changes address; the broker host/port fields are ignored on web.
- **Setup-screen lockout**: broker credentials can never be changed in-app once set
  (gear edits only DX keys). Intended, but operationally awkward if the broker moves
  (the two-broker migration will require clearing app data or a reinstall).
- **START STATION is enabled during phase `stopping`** — a user tapping START while the
  shutdown sequence is still in flight sends `{"action":"start"}` mid-stop. powerseq
  presumably tolerates it, but the UI does not guard the `stopping` phase.
- **`publish()` drops silently when disconnected** — intentional (banner warns), but a
  user can tap with no feedback at all beyond the banner; there is no per-press
  toast/flash.
- **FaultsBar render-time fallback**: `offlineSince[address] ?? now` — if the store
  ever omits a key the row silently shows a render-time clock (the store comment says
  UI "must not fall back to render time"; the code does exactly that as a last
  resort). Also `_clockTime` accepts any `at.year > 2000`, so pre-2000 timestamps fall
  back to the wall clock.
- **FaultRecord reactivation refreshes `ts`** — a recurring fault's "when" becomes its
  last-seen time, not its first; the row's original onset is lost.
- **6 m force-forward fires without operator confirmation** — an automatic command
  published by the UI. It is latched (once per invalid state) and moving-locked, but a
  reimplementation must handle the case where the publish races the controller catching
  up (state may re-trigger after travel).
- **bandMismatch comparability is a heuristic** (excludes `gen`, `unknown`, `band-*`)
  tuned to the two deployed bridges' label vocabularies; a third labeling scheme would
  produce false mismatches.
- **ClimatePanel is a mock** (hard-coded 21.4 °C / 612 ppm / HEAT on) — it implies
  telemetry that does not exist.
- **UHF page is a placeholder**; the two UHF slots appear only in the offline/silent
  list.
- **Mercator painter's world-layer cache is `static`** (shared across panel instances,
  keyed on center/zoom/size/theme) — fine for one panel, a leak hazard if two
  Mercator panels ever coexist; the compass's `WorldLayerCache` is instance-scoped and
  explicitly disposed (a smoke test for Picture disposal was being iterated on at
  time of writing: `test/tmp_picture_dispose_smoke_test.dart`, untracked).
- **Dedup key ignores lat/lng**, so a station reporting a moved position under the same
  locator/sender/band keeps its first coordinates unless age/SNR improves.
- **The `_OnlineTag` counts "offline" rows**, including silent-slot rows — the number
  can exceed the number of physical devices (15 expected slots).
- **Fault texts are upper-cased strings from the bus** — any bridge publishing a
  verbose error produces a long un-wrapped red pill/row (PA tag has no length cap).
- **Band buttons omit 160m/60m/30m/6m** even though the radio supports them and the
  band palette includes them — deliberate for this station's antennas, but a
  reimplementation should treat the list as configuration, not law.

---

## 10. Re-implementation notes

**Must be preserved verbatim (behavior contract):**

- Exact MQTT topic strings, payload JSON shapes INCLUDING the value-key deviations and
  types (§4 table), per-topic retain flags, and QoS (sub 0, pub 1).
- Two-layer liveness semantics (bridge `/status` AND device `/state.device_online`;
  logic slots online-once-state-arrives) and the offline-row taxonomy
  (`bridge down` / `device unreachable` / `silent (no state since connect)`), with the
  3 s connect grace and the one-shot post-grace re-render.
- All fail-closed guards: RF cold-switch guard, unknown≠off presentation rules,
  while-moving lockouts, settling lockout, START gating, 6 m forward-only +
  auto-correction, RETRACT-during-motion.
- The full operator-safety presentation set: link banner copy, red grounded/manual/
  armed/dangerActive styling, RF ON / RF ? / NO RF tags, PSU root-cause line, fault
  history dedup/clear/reactivate/cap-30, faults bar 4-row cap and active-first
  newest-first sort.
- DX overlay: SSE URL and query params, mqtt-only spot ingestion, mode-driven SNR
  thresholds (0 dB SSB-family / −15 dB CW / off when unknown), 600 s live-age, 80-spot
  cap, 2 s×1.5→60 s backoff, 15 s connect timeout + 5 min idle watchdog (native),
  grid-square aggregation (4-char prefix, dominant band, top-quartile mean), the exact
  opacity ramp, the exact fixed band palette, AEQD/Mercator math (clipping, lat clamp,
  wrapping, scale `r·zoom/π`), zoom ranges/steps/defaults (compass 1.0–5.0/0.2/1.5;
  Mercator 1.0–12.0/0.5/2.5), tap-to-aim angle math, >5° target visibility.
- Setup flow semantics: setup-screen-only-when-creds-missing, gear sheet editing only
  the two DX keys with live apply, upper-cased locator.
- Credential storage semantics (secure storage native; the web fallback is a known
  trade-off to either preserve or explicitly improve).

**Free to change (implementation detail):**

- Flutter/Dart, `mqtt_client`, provider/ChangeNotifier plumbing, ValueNotifier "hot"
  path optimization, `package:clock` test seams.
- The rasterized world-layer caches, painter structure, animation controllers —
  provided interactive zoom stays smooth on tablet hardware and reduced-motion is
  respected.
- Exact pixel metrics (padding, bar heights, 96×34 buttons) — except the ≥48 dp touch
  target rule and the two documented exceptions should be consciously re-decided.
- The +1 font-size quirk in the text-style helpers; exact hex values of the three
  theme palettes may be refined, but dark-first, near-black cards, mono readouts, and
  the fixed band palette are the visual contract.
- Phone/tablet breakpoints (600 px shortest side, 1200/720 compact split) may be tuned
  if the one-tap-at-everything layout contract holds.
- The webbridge (a standard MQTT-over-WebSocket broker endpoint would remove the need
  for byte-forwarding and the hard-coded URL).
- The Horst-Kevin dragon mascot is a brand flourish — keep or drop, but it is
  user-recognized.