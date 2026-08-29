# hf_console — DATA/DOMAIN LAYER research spec

Component: `hf_console` (Flutter app, non-UI layer) at `/Users/ingomar.otter/dev/stationa/hf_console`.
Research date: 2026-08-29. Branch examined: `hf_console/review-fixes` (HEAD 3401bf9).
Files covered: `lib/mqtt/*`, `lib/store/*`, `lib/dxspot/*`, `lib/main.dart`, `webbridge/main.go`, `deploy.sh`, `tool/prebuild.sh`, `android/app/src/main/AndroidManifest.xml`, `test/support/fixtures.dart`, `pubspec.yaml`, project `CLAUDE.md`.

---

## 1. Purpose & role

hf_console is the **operator console** for the HF (high-frequency, roughly 3–30 MHz amateur-radio) portion of the "Mühle" station. It runs on an Android tablet (also builds for iPhone and, as a web page, in a LAN browser) and gives the operator a single screen that shows the live state of every station subsystem and lets them send commands.

Plain-language glossary (terms used throughout):

- **Ham radio / amateur radio**: licensed, non-commercial two-way radio; operators exchange contacts ("QSOs") worldwide.
- **DX**: "long distance" — a distant station an operator wants to contact. A **DX spot** is a report that some station was heard on some frequency.
- **FT8/FT4**: weak-signal digital modes; spots from these carry an **SNR** (signal-to-noise ratio, dB) figure.
- **Maidenhead locator / grid square**: a geographic encoding used in ham radio. 2 chars = a "field" (20° lng × 10° lat), 4 chars = a "square" (2° × 1°), 6 chars = a "subsquare" (5′ × 2.5′). Example: `JN58sd`.
- **QTH**: ham shorthand for "station location".
- **MQTT**: lightweight pub/sub protocol. A **broker** relays messages on hierarchical **topics** (e.g. `muehle/hf/pa/state`). A **retained** message is stored by the broker and delivered to every future subscriber. **LWT** (last will and testament) is a message the broker publishes on a client's behalf when that client drops ungracefully. **QoS** 0 = at most once, 1 = at least once.
- **SSE (Server-Sent Events)**: a long-lived HTTP response of `data: <text>` lines; blank line ends an event.
- **Slot**: one station subsystem on the MQTT bus, identified by a topic address such as `muehle/hf/pa`.
- **Bridge**: a small daemon that translates one physical device's protocol onto the MQTT bus.
- **AEQD (azimuthal equidistant) projection**: a map projection centered on one point; every other point appears at its true bearing AND true distance from the center.

Architecturally, the app is a **pure MQTT client plus one HTTP(S) SSE feed**:

1. It subscribes to `muehle/#` on the station MQTT broker and mirrors everything it hears into a local in-memory store (`BusStore`).
2. It publishes JSON command payloads to `muehle/<slot>/cmd` topics when the operator taps controls.
3. Independently of MQTT, it opens an SSE feed to the external "horstreporter" DX-spot service and renders spot positions.

It is read-mostly: the console has **no MQTT presence of its own** — no `muehle/.../console` slot, no `/meta`, no `/state`, no LWT, no heartbeat. Its only bus footprint is the `/cmd` messages it publishes and its broker session.

---

## 2. Upstream interface

### 2.1 MQTT broker (primary)

- **Transport**: raw TCP to the broker, port 1883 (no TLS). Protocol MQTT 3.1.1 via the `mqtt_client` Dart package, v10.x (`pubspec.yaml`: `mqtt_client: ^10.11.11`).
- **Broker address**: user-configured at first launch; stored credentials carry host/port. On the examined branch the setup-screen default host is `192.168.1.50` (Home-Assistant broker); the unmerged two-broker work (see §6.4) repoints this to `192.168.1.139` (shack-local broker on the Raspberry Pi "shari"). The project CLAUDE.md already documents the shack broker at `192.168.1.139:1883` — code and docs disagree on this branch; the code default is `192.168.1.50`, port default `1883`, default username `console`.
- **Connection parameters** (`lib/mqtt/mqtt_service.dart`), exact values:
  - `keepAlivePeriod = 20` seconds (MQTT PINGREQ idle ping).
  - `autoReconnect = true`, `resubscribeOnAutoReconnect = true` (library-managed reconnect; on success the `muehle/#` subscription is re-established and retained payloads re-flood).
  - Clean session (`startClean()`), i.e. no broker-side session/queue: messages published while the console is disconnected are lost.
  - Client ID: `'hf-console-' + <epoch milliseconds>` — unique per app launch, so concurrent sessions don't evict each other.
  - Authentication: username + password from secure storage (see §6).
- **Web variant** (`lib/mqtt/client_factory_web.dart`): browsers cannot open TCP sockets, so the web build ignores the configured host/port and connects `MqttBrowserClient` to a **hard-coded** `ws://192.168.1.139:8091/mqtt` with `websocketProtocols = MqttClientConstants.protocolsSingleDefault` (MQTT over WebSocket). The username/password from setup still flow through to the broker inside the MQTT CONNECT packet.
- **Connection-loss detection**: MQTT keepalive (20 s) plus the library's auto-reconnect machinery. The app tracks three callbacks: `onConnected` / `onAutoReconnected` → set a `connected` ValueNotifier true and call `store.markConnected()`; `onDisconnected` / `onAutoReconnect` → set connected false. **Fragility**: if the *initial* `connect()` throws (broker unreachable at launch), the app catches the exception and shows the console in offline mode; there is no app-level retry loop — recovery relies on the library's auto-reconnect, which in `mqtt_client` only arms after a first successful connection. See §9.

### 2.2 horstreporter SSE feed (secondary, independent of MQTT)

- **Service**: horstreporter, an external DX-spot aggregation service (also the station's web spot map). Default base URL `https://horstreporter.kgbvax.net`, user-configurable.
- **Endpoint**: `GET <base>/api/stream?qth=<urlencoded-qth>&minutes=30&surroundings=true` where `<qth>` = the station callsign if configured, else the Maidenhead locator. Built in `DxSpotService.streamUrl()` (`lib/dxspot/dxspot_service.dart`).
- **Protocol**: SSE. Each spot arrives as a **default (unnamed) event**: one `data:` line whose payload is a JSON object (see §3.4). Multiple `data:` lines per event would be joined with `\n`.
- **No keepalives**: the server sends nothing between spots (no `: comment` heartbeat lines). Consequences handled client-side (§2.2.1).
- **Reconnect behavior**: on (re)connect the server republishes the last `minutes=N` window of spots. The client requests `minutes=30`, but the source-file comments say "minutes=15" — a code/comment discrepancy; the URL parameter in code is authoritative: **30**.
- **CORS**: the server sends `Access-Control-Allow-Origin` permitting cross-origin browser access (the web build's page origin is `http://shari:8091`). Native builds are unaffected by CORS.
- **Authentication**: none.

#### 2.2.1 Native SSE client (`lib/dxspot/dxspot_source_io.dart`, dart:io)

- Request headers: `Accept: text/event-stream`, `Accept-Encoding: identity` (prevents gzip so the body can be split by lines incrementally), `Cache-Control: no-cache`.
- `HttpClient.connectionTimeout = 15 s` — bounds the TCP+HTTP connect phase.
- **Idle watchdog = 5 minutes**, rearmed on *every* received line (including comment lines). Because the server sends no keepalives, a half-open TCP connection would never surface as EOF/error; the watchdog force-closes and reports disconnect so backoff can take over. Cost: on a genuinely quiet band the client needlessly reconnects every ≤5 min — harmless, because the server replays the recent-spots window on reconnect.
- Non-200 status → treated as disconnect.
- SSE frame parsing per HTML spec: blank line dispatches the accumulated event; a single leading space after `data:` is stripped; `event:`/`id:`/`retry:` lines and `:` comments are ignored.
- The source does **not** reconnect on its own; it invokes an `onDisconnected` callback once and the service layer owns backoff (so both platforms behave identically).

#### 2.2.2 Browser SSE client (`lib/dxspot/dxspot_source_web.dart`)

- Uses `EventSource` natively; an `onError` is converted into an explicit close + `onDisconnected` (deliberately discarding EventSource's built-in auto-reconnect so the service's backoff governs on both platforms). Losing SSE's `Last-Event-ID` resume is harmless because every new connection re-fetches the history window.

### 2.3 webbridge (web deployment's broker proxy, Go)

`webbridge/main.go` — `hf-console-web` binary:

- Flags: `-listen 0.0.0.0:8091`, `-mqtt-broker <addr>` (deploy default on this branch `192.168.1.50:1883`; two-broker branch default `192.168.1.139:1883`), `-web-root /opt/hf-console-web/build/web`.
- Serves the static Flutter web build at `/` and exposes `/mqtt` as a **byte-forwarding WebSocket-to-TCP proxy**: upgrades the HTTP request to WebSocket (gorilla/websocket, `CheckOrigin` returns true for any origin — LAN trust), dials the broker over raw TCP, then pumps bytes bidirectionally in two goroutines (4096-byte read buffer, frames sent as binary WebSocket messages). It performs **no MQTT framing or interpretation** — the browser's MQTT client speaks raw MQTT packets through the tunnel. Normal/generic WebSocket close errors are silenced; others logged.
- One broker TCP connection per browser WebSocket connection; when either side closes, both are torn down.

---

## 3. MQTT presence

### 3.1 Subscriptions

| Topic filter | QoS | When |
|---|---|---|
| `muehle/#` | at most once (QoS 0) | immediately on every (re)connect, before anything else |

Nothing else is subscribed. All bus traffic arrives through this one filter.

### 3.2 Publications

The app publishes only to command topics: `muehle/<slot>/cmd`, QoS at least once (QoS 1). Whether each topic is published **retained** is fixed per slot in `lib/store/wiring.dart` (`cmdRetain`) and must match the bus-wide policy:

Retained (self-healing steady state — last command persists; a restarted device re-applies it):
- `muehle/power/master`, `muehle/power/psu-13v8`, `muehle/hf/switch`, `muehle/hf/pa-arm`, `muehle/hf/ant-ctrl`, `muehle/hf/ant-switch`, `muehle/hf/antenna-select`

Non-retained (one-shot):
- `muehle/hf/pa`, `muehle/hf/rotator`, `muehle/hf/tuner`, `muehle/hf/power-seq`, `muehle/hf/radio` (DVK play/stop are one-shot)

There is also a `clear(topic)` helper: publishes an **empty payload, retained** — the standard MQTT idiom for deleting a retained message; not currently invoked by any UI code on this branch but part of the service API.

`MqttService.publish` silently drops the message if the client is null or not in the `connected` state (no queueing — see §9).

### 3.3 Inbound payload decoding and the store model (`lib/store/bus_store.dart`, `lib/mqtt/mqtt_service.dart`)

Decode pipeline, per received message:

1. Bytes → UTF-8 string with `allowMalformed: true`.
2. Empty string → payload treated as `null` ("cleared" semantics — a retained empty message clears that plane).
3. Otherwise JSON-decode; on decode failure the raw string is kept (the `/status` plane is a bare string, not JSON).

Topic parsing in `BusStore.apply(topic, payload, retained)`:

- Split the topic on `/`. The **last segment must be one of the four plane names** `meta`, `state`, `status`, `cmd`; everything before it is the slot address (e.g. topic `muehle/hf/pa/state` → address `muehle/hf/pa`, plane `state`). Topics not ending in a known plane are ignored entirely.
- A `Slot` object is created lazily per address.

**Slot model** (all planes are "latest value wins"; each slot holds at most one snapshot per plane):

```
Slot {
  address: String                    // e.g. "muehle/hf/pa"
  meta:   Map<String,dynamic>?       // device description, from <addr>/meta
  state:  Map<String,dynamic>?       // full JSON state snapshot, from <addr>/state
  status: String?                   // bridge liveness, from <addr>/status ("online" or absent)
  cmd:    Map<String,dynamic>?      // last command acknowledged on <addr>/cmd
  statusChangedAt: DateTime?         // local-clock timestamp of last /status change
  deviceChangedAt: DateTime?         // local-clock timestamp of last /state.device_online change
}
```

Cleared-plane handling (empty retained payload): `meta`/`cmd` → nulled; `state` → nulled and `deviceChangedAt` stamped (if there was a state); `status` → nulled and `statusChangedAt` stamped (if there was a status).

**Liveness — two layers, both required** (this mirrors a station-wide convention; the console consumes, it does not publish, either layer):

- `bridgeOnline` = `status == 'online'` (exact string; `/status` carries the bridge's MQTT LWT, `'online'` when the bridge process is alive).
- `deviceOnline`:
  - `state == null` → **false** (no snapshot received yet);
  - `state` present but **no `device_online` key** → **true** (logic-only slots — `antenna-select`, `power-seq`, `hadiscovery` — have no physical device and legitimately omit the key);
  - otherwise → `state['device_online'] == true` (physical-device bridges publish an explicit `device_online` boolean inside `/state`; the bridge's LWT can stay `'online'` while the serial/USB/Ethernet link to the device is down — `/state.device_online` is the only honest device-link signal).
- `isOnline` = `bridgeOnline && deviceOnline`.

**Side effects on every `/state` apply**:

- `deviceChangedAt` stamped when `device_online` flips (or when state goes from null to non-null).
- **Fault history** updated (see below).
- "Hot value" notifiers updated: `BusStore.hot<T>(path)` lazily creates a `ValueNotifier` keyed by `'<address>.<stateKey>'` (e.g. `'muehle/hf/rotator.az'`); on each state apply, every state key that has a live notifier gets its value pushed. Note: on this branch **no widget actually calls `hot()`** — the UI reads via `stateValue`/`stateValueAs` on `BusStore` notifications; `hot()` is a dormant high-frequency fast-path.

**Fault history** (list of `FaultRecord {address, text, ts, active}`, key `'<address>|<text>'`):

- On each state apply, active fault text is derived: if `state.error` is non-empty and not case-insensitively `'NONE'` → the error, uppercased; else if `state.fault` is non-empty and not `'none'` → the fault, uppercased; else no active fault.
- Any previously active record for the same address whose text differs from the new active text is marked inactive (cleared).
- A matching existing record is re-activated and its `ts` refreshed (so a persistent fault stays at the top and visibly current); `ts` comes from `state.ts` if present, else local clock ISO-8601.
- A brand-new fault text is appended. History is capped at **30 records** (oldest dropped).
- Faults are never removed from history, only deactivated.

**Silence reporting and the offline list**:

- `markConnected()` (called by MqttService on every connect/auto-reconnect) records `_connectedAt = now` and schedules a one-shot `notifyListeners` after a **3-second silence grace** (`_silenceGrace = Duration(seconds: 3)`).
- `offlineList` computes, per known slot: `'<address>: bridge down'` if `status != 'online'`; else `'<address>: device unreachable'` if `!deviceOnline`. Additionally, **after** the 3 s grace, every **expected slot** (fixed list, below) that has produced no message at all this session is reported as `'<address>: silent (no state since connect)'` — this catches services that are dead-since-boot or never deployed (a broker with no retained payloads under `muehle/#` delivers zero messages, which is exactly the dead-station case the report exists for).
- `offlineSince` provides per-address timestamps aligned with the same rows: bridge-down → `statusChangedAt`; device-unreachable → `deviceChangedAt ?? statusChangedAt`; silent → the session connect time. Timestamps are best-effort: a retained message arriving at connect counts as a "change", so they floor at connect time, never earlier bus truth.

**Expected slots** (the full fixed list in `wiring.dart` — every deployed slot the console monitors):

```
muehle/power/master, muehle/power/psu-13v8,
muehle/hf/radio, muehle/hf/ant-ctrl, muehle/hf/ant-switch, muehle/hf/switch,
muehle/hf/pa-arm, muehle/hf/antenna-select, muehle/hf/pa, muehle/hf/rotator,
muehle/hf/tuner, muehle/hf/power-seq, muehle/hf/discovery,
muehle/uhf/rotator, muehle/uhf/pol-ctrl
```

(15 slots. TBD/precision slots and host-liveness nodes are deliberately excluded until they exist.)

**State-plane field catalog** — everything the console reads from each slot's `/state` (from UI consumption + test fixtures; the console tolerates missing keys everywhere, substituting defaults shown in §5.4):

- `muehle/hf/radio` (FLEX-8400 transceiver): `freq_hz` (int, Hz — station convention: Hz, never kHz/MHz), `band` (string e.g. `'20m'`), `mode` (canonical: `cw`,`usb`,`lsb`,`am`,`fm`,`data`), `tx` (`'rx'`/`'tx'`), `tuning` (bool), `drive` (int 0–100), `dvk_status` (`'idle'`/…), `dvk_id` (int, 0 = none), `mic_profile` (string, active mic profile name), `mic_profiles` (string, caret-delimited profile list), `device_online`, `ts`.
- `muehle/hf/pa` (ACOM 1200S amplifier): `mode` (`'standby'`/`'operate'`), `keyed` (`'rx'`/`'tx'`), `fault` (`'none'` or a fault word), `error` (string, `''`/`'NONE'` when clear), `temp_c` (double, °C), `fwd_power_w` (double, W), `rfl_power_w` (double, W), `swr` (double, ratio), `pa_state` (device string e.g. `'STBY'`, `'OPR/RX'`, `'OPR/TX'`), `power` (`'on'`/`'off'`), `device_online`, `ts`.
- `muehle/hf/tuner` (ATR-1000 ATU): `inline` (bool — in the RF path vs bypass), `settling` (bool), `fault` (string, `''` clear), `swr` (double), `device_online`, `ts`.
- `muehle/hf/rotator` (Yaesu G-450DC): `az` (double, degrees), `target_az` (double), `moving` (bool), `device_online`, `ts`.
- `muehle/hf/ant-ctrl` (Ultrabeam RCU-06 controller): `direction` (string, e.g. `'forward'`), `moving` (bool), `band` (string, may be `''` = unknown), `device_online`, `ts`.
- `muehle/hf/ant-switch` (1:6 relay antenna switch): `selected` (string: `'off'` | `'port1'` | `'port4'` | `'port5'` | `'port6'` — port2/port3 are unwired at this station and never appear), `settled` (bool), `device_online`, `ts`.
- `muehle/hf/antenna-select` (antenna-selection reconciler, logic slot): `mode` (`'auto'`/`'manual'`), `device_online` (key omitted in practice; see logic-slot rule), `ts`.
- `muehle/hf/switch` (M5 Stamp PLC relays): `pa` (`'on'`/`'off'` — PA remote-on relay), `trx` (`'on'`/`'off'` — transceiver remote-on relay), `device_online`, `ts`.
- `muehle/hf/pa-arm` (PA arm relay): `enabled` (bool), `armed` (bool), `error` (string), `device_online`, `ts`.
- `muehle/hf/power-seq` (startup/shutdown sequencer, logic slot): `phase` (e.g. `'idle'`, `'running'`), `fault` (string, `''` clear), `device_online`, `ts`.
- `muehle/power/master`, `muehle/power/psu-13v8` (Shelly smart plugs): `power` (`'on'`/`'off'`), `device_online`, `ts`.
- `muehle/uhf/*` slots are in `expectedSlots` but not rendered by HF panels on this branch.

**Port→name map** (`antennaMap`, `lib/store/wiring.dart`; source of truth is the antennaselect wiring map):

```
'off' → 'Grounded'   (all antenna ports grounded; TX path disconnected)
'port1' → 'Dummy load'
'port4' → 'Ultrabeam'
'port5' → 'Port 5'
'port6' → 'Fan dipole 80/40'
```

(Note: the repo-root CLAUDE.md table lists Ultrabeam on switch port 3; this app's map says port 4. The app's map is what the PRD must use for the console's behavior.)

### 3.4 horstreporter spot payload (SSE `data:` JSON)

One spot object (per horstreporter's `streamSpot`):

| Field | Type | Meaning |
|---|---|---|
| `lat`, `lng` | number | **the remote (DX) station's position in degrees** — already resolved server-side; NOT the reporter's position |
| `snr` | int | reported signal-to-noise ratio in dB (FT8/FT4 spots) |
| `ageSeconds` | int | how old the report was when the server emitted it |
| `locator` | string | the remote station's Maidenhead locator (may be empty) |
| `band` | string | band label, e.g. `'20m'` |
| `sourceType` | string | `'mqtt'` (FT8/FT4 via MQTT), `'dxcluster'`, `'rbn'`, `'wspr'` |
| `sender`, `receiver` | string, optional | callsigns; present only for `sourceType == 'dxcluster'` |

Console-side ingest rules (exact, in `dxspot_service.dart`):

- `sourceType != 'mqtt'` → **dropped** (the console mirrors horstreporter's azimuthal view, which consumes only FT8/FT4 mqtt spots).
- Missing `lat`/`lng` → dropped. Missing all of locator+receiver+sender → dropped (nothing to place or label).
- SNR gate (mode-aware; mirrors the server's own stream filter): filter modes `'none' | 'ssb' | 'cw'` with thresholds; defaults: mode `'ssb'`, `ssbMinDb = 0` dB, `cwMinDb = -15` dB. `snr >= threshold` passes; mode `'none'` disables gating. The mode is driven by the live radio mode (`muehle/hf/radio/state.mode`): `usb/lsb/am/fm/data → 'ssb'`, `cw → 'cw'`, anything else (including unknown) → `'none'` (gating off).
- Dedup key: `'<locator>|<receiver??''>|<sender??''>|<band>|<sourceType>'`. On repeat: keep the freshest (lowest `ageSeconds`); tie-break by higher `snr`; **always** update the kept spot's `receivedAtMs` (local wall-clock receive time) so an actively re-spotted station doesn't age out.
- Live age of a spot = `ageSeconds + (now - receivedAtMs)/1000`.

### 3.5 Publish cadence / heartbeats

- The console publishes **no periodic messages of any kind** — no heartbeat, no LWT, no status. Its presence on the broker is invisible to other components.
- It reacts to operator input only; inbound traffic cadence is whatever each bridge publishes (each bridge owns its own snapshot/heartbeat policy).

---

## 4. Command surface

All commands go to `muehle/<slot>/cmd` (helper `cmdTopic(slot)`), QoS 1, retain per §3.2. Standard payload shape is `{"action": <string>, "value": <anything>}` (station-wide "value-key" convention: arguments ride under `value`, not under a key named after the action). **Documented deviations below are load-bearing.**

Payload builders (`lib/store/wiring.dart`) and the panel that triggers each:

| Slot topic | Payload (exact) | Notes / retain |
|---|---|---|
| `muehle/power/master/cmd` | `{"action":"set_power","value":"on"\|"off"}` | retained |
| `muehle/power/psu-13v8/cmd` | `{"action":"set_power","value":"on"\|"off"}` | retained |
| `muehle/hf/power-seq/cmd` | `{"action":"start"}` / `{"action":"stop"}` — **no `value` key at all** | one-shot |
| `muehle/hf/switch/cmd` | `{"action":"set_pa","value":"on"\|"off"}`; `{"action":"set_trx","value":"on"\|"off"}` | retained |
| `muehle/hf/pa-arm/cmd` | `{"action":"set_enabled","value":"true"\|"false"}` — **value is a STRING, not a JSON bool** | retained |
| `muehle/hf/rotator/cmd` | `{"action":"set_az","az":<double>}` — **azimuth is a top-level key, NOT under `value`**; `{"action":"stop"}`; `{"action":"fwd"}`; `{"action":"rev"}` | one-shot |
| `muehle/hf/ant-ctrl/cmd` | `{"action":"frequency","freq_hz":<int>}` — **freq_hz top-level, integer Hz**; `{"action":"direction","value":<direction>}`; `{"action":"band","value":<band>}`; `{"action":"retract"}` | retained |
| `muehle/hf/pa/cmd` | `{"action":"set_mode","value":<mode>}`; `{"action":"set_band","value":<band>}` | one-shot |
| `muehle/hf/tuner/cmd` | `{"action":"set_inline","value":<true\|false>}` — **value is a real JSON bool**; `{"action":"tune","value":"mem"\|"full"}` — **value is a STRING** | one-shot |
| `muehle/hf/antenna-select/cmd` | `{"request":"<portName>\|"auto"\|"manual"}` — **no `action`/`value` keys at all** | retained |
| `muehle/hf/ant-switch/cmd` | `{"select":"<portName>"}` — **no `action`/`value` keys** | retained |
| `muehle/hf/radio/cmd` | `{"action":"dvk_play_N"}` (N clamped 1..12 — play DVK memory slot N); `{"action":"dvk_stop","value":"<id>"}` or `"value":""` (stop all); `{"action":"set_band","value":<band>}`; `{"action":"set_mic_profile","value":<profileName>}` | one-shot |

Behavioral notes:

- **Antenna selection routing logic** (`antenna_panel.dart`): when the antenna-select reconciler is online and its mode is not `'manual'`, tapping an antenna sends `{"request": <port>}` to `antenna-select` (policy layer). When mode is `'manual'`, or the reconciler is offline/absent, the tap drives the switch actuator directly via `{"select": <port>}` to `ant-switch`. A "manual/auto" toggle sends `{"request":"manual"\|"auto"}` to `antenna-select` (only when that slot is online).
- **Fail-closed RF-safety guard** (behavior contract): the panel computes `rfSafe` = radio online AND radio `tx == 'rx'` AND radio `tuning != true` AND PA `keyed != 'tx'`. Direct (manual/reconciler-offline) antenna switching is **refused while RF is present** — switching an antenna while power is applied would destroy relay contacts ("hot switching"). Same-side guard: `selectPort` no-ops if the ant-switch slot is not online.
- Publishing when disconnected silently drops the command (§9).

---

## 5. Behavior & state machine

### 5.1 Startup sequence (`lib/main.dart`)

1. Flutter init; on non-web: allow all device orientations, enter `immersiveSticky` full-screen mode (Android; no-op elsewhere).
2. Root state creates, in order: `CredentialStore`, `BusStore`, `MqttService(store)`, `DxSpotService`.
3. `_tryAutoConnect()`: read all stored credentials/settings.
   - Configure + start the DX-spot service immediately (it is independent of the broker): `configure(baseUrl, locator, callsign)` then `start()`. If no station locator is stored, `start()` no-ops (overlay off — compass shows beam heading only).
   - If `mqtt_host`, parseable `mqtt_port`, `mqtt_user`, and non-empty `mqtt_password` all exist → connect. A connect failure is caught and swallowed: the console screen is shown anyway, in offline state (the connected-indicator and faults bar convey it).
   - If credentials are complete → show console screen; otherwise → show the setup screen (broker host/port/user/password + optional station locator + horstreporter URL fields).
4. Setup-screen save: persist all six values (locator uppercased), connect (failure tolerated), `configure + restart` the DX service, then show the console. Once provisioned, the tablet boots straight to the console forever after; the only in-app way back to the locator/URL fields is the DX-overlay gear dialog (broker credentials are not editable in-app after first provision — only by clearing app data).

### 5.2 MQTT session lifecycle

- Connect → onConnected: connected=true, `store.markConnected()` (restarts the 3 s silence grace + schedules a UI refresh), subscribe `muehle/#` QoS 0. The broker then floods all retained `muehle/#` payloads, which populate the store.
- Drop → onDisconnected/onAutoReconnect: connected=false. The library reconnects by itself (keepalive 20 s detects dead links; auto-reconnect retries with its own timing, resubscribing and re-flooding retained state). On success: connected=true, `markConnected()` again (silence grace restarts because a reconnect re-delivers everything).
- All inbound messages go through `_apply` → `BusStore.apply` → `notifyListeners()` on **every** message (the store is a Flutter `ChangeNotifier`; the whole widget tree rebuilds on any bus message — no coalescing in the store itself).

### 5.3 DX-spot service lifecycle (`DxSpotService`, a `ChangeNotifier`)

Constants (exact): max spot age **600 s** (10 min); max spots **80**; backoff start **2 s**, multiply by 1.5 each failure, cap **60 s**; notify throttle **500 ms** (≤ ~2 Hz UI notifications); prune interval **60 s**.

- `configure(baseUrl, locator, callsign)`: sets the AEQD projection center by decoding the locator (`locatorToLatLng`); with no locator, the overlay is inactive and all spots cleared. Does not (re)start the feed.
- `start()`: no-op if already running or inactive (no locator). Arms the 60 s prune timer and connects the SSE source.
- On a spot event: ingest (rules §3.4), set `connected = true`, clear `error`, **reset backoff to 2 s**, rebuild.
- Rebuild: prune spots older than 600 s live-age; sort by live age ascending, tie-break SNR descending; cap at 80; recompute grid squares; notify (throttled to 500 ms, coalescing bursts).
- Prune timer runs even when the feed is quiet so stale dots don't linger on a silent/half-open feed.
- On disconnect (source reports EOF/error/watchdog): connected=false, `error = 'horstreporter feed down'`, notify, schedule reconnect after `backoff` seconds, then `backoff = backoff * 3 ~/ 2` (capped 60).
- On a good event after reconnect, backoff resets to 2 s.
- **Radio-mode coupling**: a `BusStore` listener in `main.dart` watches `muehle/hf/radio/state.mode` and calls `dxSpot.setMode(mode)` on every bus update, which retargets the SNR filter family (§3.4) while keeping the dB thresholds. Changing the filter does not clear existing spots; live spots are re-evaluated on their next broadcast, and a reconnect re-ingests history through the new threshold.

**Grid squares** (aggregate for the overlay, mirroring horstreporter's azimuthal view exactly): spots are grouped by the **first 4 characters** of their (uppercased) locator; per square the **dominant band** = the band with the most spots in that square (first-max wins ties); the **score** = mean of the top quarter of SNR values (sort descending, take `ceil(n/4)` clamped to 1..n, average) — used for opacity; squares with no band data are skipped. Spots with <4-char locators or empty bands don't contribute.

### 5.4 UI default values when keys are absent (the "unknown" contract)

The UI must never fabricate an optimistic state; when a state key is missing it renders from these defaults, which encode "unknown/neutral": PA mode `'standby'`, keyed `'rx'`, fault `'none'`, error `''`, temp 0.0, fwd 0.0, swr 1.0; tuner inline false, settling false, fault `''`, swr 1.0; rotator az 0.0 (target = az), moving false; ant-ctrl direction `'forward'`; antenna `selected` shown only if present (null = unknown, never guessed); power slots `'off'`; radio freq 0, band `''`, mode `''`, tx `'rx'`, drive 0, dvk `'idle'`, dvk_id 0; pa-arm enabled/armed false, error `''`. Note these are defaults for *display*, deliberately distinguishable from real values (e.g. PA relay `'off'` is distinct from the switch's `pa` key being absent → null → "unknown" rendering).

### 5.5 Error paths, exhaustive

- Broker unreachable at launch → connect throws → swallowed; console shows offline; DX overlay unaffected.
- Broker drops mid-session → library auto-reconnect; UI offline indicator until re-established; published commands while disconnected are silently dropped (no queue).
- Malformed/undecodable payload → stored as raw string (harmless for state planes, which then simply don't match key lookups).
- Retained-empty plane message → clears that plane's value (§3.3).
- SSE connect failure/non-200/EOF/error → disconnect → backoff 2 s × 1.5 → 60 s cap; reset on first good event.
- SSE half-open (no data, no EOF) → 5-min idle watchdog forces reconnect.
- Malformed GeoJSON asset → parser returns empty list, never throws (compass renders without coastlines rather than crashing).
- Corrupt/absent stored credentials → setup screen shown.

---

## 6. Configuration

There is no config file. All configuration is **user-entered on first launch and persisted per device**.

### 6.1 Keys (CredentialStore, `lib/store/credential_store.dart`)

| Key | Type (stored) | Meaning | Default when unset |
|---|---|---|---|
| `mqtt_host` | string | broker host | `'192.168.1.50'` in setup-screen default on this branch (`'192.168.1.139'` on the two-broker branch) |
| `mqtt_port` | string (digits) | broker TCP port | `'1883'`; parse failure on save falls back to 1883 |
| `mqtt_user` | string | broker username (dedicated `console` account; narrow ACL: subscribe `muehle/#`, publish `muehle/+/cmd`) | `'console'` |
| `mqtt_password` | string | broker password | none — empty password ⇒ auto-connect skipped, setup screen shown |
| `station_locator` | string, **uppercased on save** | station Maidenhead locator; presence enables the DX overlay; also the AEQD projection center | `''` (overlay off) |
| `horstreporter_base_url` | string | SSE base URL | `'https://horstreporter.kgbvax.net'` (also hard-coded fallback in `streamUrl`) |
| `station_callsign` | string | station callsign, preferred as the SSE `qth=` param when set | read at startup (Android path reads all secure-storage keys), but **no UI on this branch writes it** — dormant key |

Save validation: empty host, empty user, or empty password silently refuses to save (button no-ops). Locator is trimmed + uppercased; URLs trimmed.

### 6.2 Secrets handling

- **Android**: `flutter_secure_storage` → Android Keystore-encrypted storage. **iOS**: Keychain. Release APKs installed with `adb install -r` keep existing credentials across reinstalls (data not wiped).
- **Web**: no secure storage exists in a browser — falls back to `SharedPreferences`, i.e. **plaintext browser-local storage**; the broker password is stored in the clear in the web build. Known accepted trade-off on a LAN-only deployment (see §9).

### 6.3 Hard-coded operational values (not user-configurable)

Web MQTT endpoint `ws://192.168.1.139:8091/mqtt`; keepalive 20 s; silence grace 3 s; SSE `minutes=30`, `surroundings=true`; DX max age 600 s / 80 spots / backoff 2–60 s / throttle 500 ms / prune 60 s; SSE connect timeout 15 s / idle watchdog 5 min; fault-history cap 30; DVK slots 1–12.

### 6.4 Two-broker status (documented because it is in flight)

Work-in-progress on branch `feat/shack-local-mqtt-broker`, commit `cd58466` (2026-08-26), **not merged into the branch examined and, per project memory, not yet deployed** (planned resume ~2026-09-02): a shack-local Mosquitto broker on shari (`192.168.1.139:1883`, authoritative for `muehle/#`) bridged to the untouched Home-Assistant Mosquitto add-on (`192.168.1.50:1883`), so the station keeps its bus when the shack↔house link drops. That commit repoints hf_console's setup-screen default host and webbridge broker default to `192.168.1.139:1883` and adds a `mqtt-broker/` component (mosquitto.conf, ACL, seed-once deploy). The current branch's project CLAUDE.md already *documents* the shack broker at 192.168.1.139, while its code still defaults to 192.168.1.50 — docs and code disagree until the merge lands. Any reimplementation must ask which broker topology is target: single broker at 192.168.1.50, or shack broker at 192.168.1.139 bridged to .50. The console itself is topology-agnostic (host is a config field); only defaults and the webbridge's broker flag differ.

---

## 7. Deployment

### 7.1 Android tablet (primary channel)

- Build: `tool/prebuild.sh` (gate: `flutter analyze` + `flutter test`) then `flutter build apk --release`; sideload `build/app/outputs/flutter-apk/app-release.apk` via ADB to the station tablet (device id `HA2CLVAY`, application id `codeberg.kgbvax.hf_console`; `adb install -r` preserves credentials).
- AndroidManifest requirements that were learned the hard way: the `INTERNET` permission must be in `android/app/src/main/AndroidManifest.xml` (Flutter scaffolding only puts it in debug/profile overlays) and `android:usesCleartextTraffic="true"` is required for the raw, non-TLS MQTT TCP socket. `keepScreenOn` is set on the activity.
- No Play Store; self-sideloaded only.

### 7.2 Web channel (shari)

`./deploy.sh` from `hf_console/`:

1. Runs the prebuild gate; `flutter build web --release --base-href /`.
2. Cross-compiles webbridge: `GOOS=linux GOARCH=arm64 CGO_ENABLED=0` (GOWORK=off) → `dist/hf-console-web-linux-arm64`.
3. Generates a hardened systemd unit and installs onto shari (`io@192.168.1.139` by default; env-overridable: `SSH_HOST`, `SSH_USER`, `SERVICE_NAME=hf-console-web`, `SERVICE_USER=hfconsoleweb`, `INSTALL_DIR=/opt/hf-console-web`, `HTTP_PORT=8091`, `MQTT_BROKER`).
4. Remote install: creates the dedicated `hfconsoleweb` system user, moves the binary to `/opt/hf-console-web/hf-console-web`, replaces `/opt/hf-console-web/build/web` with the Flutter output, installs `/etc/systemd/system/hf-console-web.service`, enables + restarts it.
5. Service reachable at `http://shari:8091/`.

Unit hardening (exact): `Restart=on-failure`, `RestartSec=5`, `NoNewPrivileges=true`, `ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`, `PrivateDevices=true`, kernel/control-group/namespace/realtime/SUID protections, `RestrictAddressFamilies=AF_INET AF_INET6`, empty capability bounding set, `MemoryMax=64M`, `TasksMax=32`, journal logging. ExecStart: `<install>/hf-console-web -listen 0.0.0.0:<port> -mqtt-broker <addr> -web-root <install>/build/web`.

### 7.3 iPhone

Same Flutter app, self-sideloaded IPA (`tool/build-ios.sh`); raw-TCP MQTT bypasses App Transport Security so no ATS exception is needed; credentials go to the iOS Keychain. No server-side deploy involved.

### 7.4 Dependencies

Flutter SDK ≥ 3.12.2 (Dart); packages: `mqtt_client ^10.11.11`, `flutter_secure_storage ^11.0.0`, `provider ^6.1.5+1`, `clock ^1.1.2` (test-clock-injectable time in BusStore), `shared_preferences ^2.5.5`, `typed_data ^1.3.2`. Bundled assets: fonts (Saira Condensed, IBM Plex Sans/Mono — offline, no runtime font fetch), `assets/geo/world.geojson` (Natural Earth 50m admin-0 country outlines, copied from horstreporter's vendored copy, ~3 MB raw), launcher icon. webbridge: Go, `gorilla/websocket`, standalone module (GOWORK=off build).

---

## 8. Invariants & safety rules

Behavior contracts that any reimplementation MUST preserve:

1. **Two-layer liveness**: a slot is online only if `/status == 'online'` AND `/state.device_online` is true; a state snapshot present without a `device_online` key means the slot is a logic slot and is considered device-online; a missing snapshot means device-offline. Never key offline detection on `/status` alone, never on `/state` alone.
2. **Fail-closed hot-switch guard**: never send an antenna-switch/select command while RF may be present (radio `tx == 'tx'`, radio `tuning == true`, or PA `keyed == 'tx'`). The guard applies to *direct* switching (manual mode or reconciler offline); in auto mode the reconciler is trusted to sequence. The guard must fail closed on unknown radio state.
3. **Retain policy fidelity**: each `/cmd` topic's retained-ness is fixed per the table in §3.2 and must match the bus policy — retained commands re-fire on device restart by design; wrongly retaining a one-shot (e.g. PA `set_mode`, rotator `set_az`) would cause a device to replay a stale movement/command on boot.
4. **Payload shape fidelity, including deviations**: `value`-key convention everywhere EXCEPT the documented exceptions (§4): power-seq start/stop have no `value`; rotator `set_az` uses top-level `az`; ant-ctrl `frequency` uses top-level `freq_hz` (integer Hz); `pa-arm.set_enabled` takes a *string* `"true"/"false"`; `tuner.set_inline` takes a *real* JSON bool; `tuner.tune` takes a *string* `"mem"/"full"`; `antenna-select`/`ant-switch` use `request`/`select` keys with no `action`. Getting these wrong is a known live-bug class at this station.
5. **freq_hz is integer Hz** — never kHz/MHz on the bus.
6. **Empty retained payload = cleared plane**; the console must null the corresponding slot plane, not ignore it.
7. **Silence reporting runs on the grace period after connect, not after the first message** — a broker with zero retained `muehle/#` payloads is exactly the dead-station case that must be reported after 3 s.
8. **No optimistic UI**: the console only renders what the bus confirms. Unknown/absent state keys render as the neutral defaults of §5.4, never as the value a control would set.
9. **The console is silent on the bus**: no heartbeat, no LWT, no retained console state, no subscribe beyond `muehle/#`, publish only to `muehle/+/cmd`.
10. **DX overlay independence**: the MQTT feed and the SSE feed are fully independent; loss of one never disables the other.
11. **Only `sourceType == 'mqtt'` spots** are ingested (keeps the console's overlay consistent with horstreporter's own azimuthal view).
12. Ordering within the app: on (re)connect, subscribe happens immediately in `onConnected` so the retained flood populates the store before user interaction; the DX service is started before/regardless of broker connection.

---

## 9. Known defects & fragilities

1. **Initial-connect failure has no app-level retry** (`mqtt_service.dart`/`main.dart`): a failed first `connect()` is swallowed and the console sits offline; recovery depends on `mqtt_client`'s auto-reconnect, which arms only after a successful first connection. A tablet booted before the broker is reachable may stay offline until the app is restarted. (Behavior observed as: "offline start is allowed" — deliberate tolerance, but no retry loop backs it.)
2. **Silent command loss** (`MqttService.publish`): if not connected, publish returns without error, queue, or user feedback — an operator tap while offline is simply dropped.
3. **Web credentials in plaintext**: on the web build the broker password lands in browser SharedPreferences/local storage; acceptable only on the trusted LAN.
4. **webbridge `CheckOrigin` allows all origins** and the bridge is bound to `0.0.0.0` — any LAN page can open a broker tunnel through it (again, LAN-trust model, but note it).
5. **`hot()` fast-path is dormant**: `BusStore.hot()` exists but nothing uses it; every bus message triggers `notifyListeners()` and a full widget-tree rebuild. On a busy bus this is a repaint per message (the DX service has a 500 ms notify throttle; the bus store has none).
6. **`station_callsign` key is read but never written** by any UI on this branch — a dormant feature (callsign preferred over locator as the SSE `qth` parameter).
7. **Comment/code disagreement**: SSE source comments say the server replays `minutes=15`, the URL requests `minutes=30`; and the two values interact with the 5-min idle watchdog (a quiet band forces reconnect roughly every 5 min; the 30-min replay window means up to 30 minutes of spot history re-ingested each time — dedup keeps this correct but it is repeated work).
8. **Doc/code broker-address disagreement** (this branch): project CLAUDE.md documents the shack broker at `192.168.1.139:1883`; the setup default, deploy.sh `MQTT_BROKER` default, and webbridge flag default all still point at `192.168.1.50:1883`. Also, the repo-root CLAUDE.md's antenna table (Ultrabeam on port 3) disagrees with the app's `antennaMap` (port 4) — the app's map matches the deployed switch wiring.
9. **`mqtt_client` keepalive/reconnect quirks**: `resubscribeOnAutoReconnect` is relied upon for re-flooding retained state; if a future client doesn't resubscribe, the store would silently go stale while `connected` shows true.
10. **QoS 0 subscription**: bus traffic is received at-most-once; a lost retained snapshot only heals on the next publish by that bridge (or a reconnect).
11. **`statusChangedAt`/`deviceChangedAt` are best-effort**: they floor at connect time (retained messages count as "changes"), so "when did it go dark" can be understated, never overstated — deliberate, documented in code.
12. **Fault-history `ts` trusts `state.ts`** verbatim when present; a bridge with a bad clock writes misleading fault timestamps into the console's history list.
13. **SSE watchdog cost**: the 5-minute idle timeout guarantees periodic disconnect/reconnect churn on quiet bands (harmless by design — history replay — but a reimplementation should keep both the watchdog and the replay-window dedup, or it will lose spots).
14. **The unused `retained` parameter** is passed into `BusStore.apply` but not used in its logic (harmless; noted so a reimplementation doesn't hunt for meaning that isn't there).

---

## 10. Re-implementation notes

**Must be preserved verbatim (behavior contracts):**

- Topic plane parsing: last `/`-segment ∈ {meta, state, status, cmd}; subscribe `muehle/#` QoS 0; publish `/cmd` QoS 1 with the per-slot retain table (§3.2); empty retained payload = clear.
- The full two-layer liveness semantics, logic-slot rule, and the 15-slot expected list with the 3 s silence grace and the exact offline-row strings/keys (`bridge down`, `device unreachable`, `silent (no state since connect)`).
- Every command payload in §4, byte-for-byte in key names and value types, including the value-key deviations — a different payload shape will be ignored or mis-executed by the deployed bridges, which are NOT being reimplemented.
- The state-field catalog and units of §3.4/§5.4 (integer Hz, string on/off, swr as ratio, etc.) and the neutral defaults for missing keys.
- The fail-closed RF guard and the antenna-select vs ant-switch routing decision (auto + reconciler online → `request` to antenna-select; manual or reconciler offline → `select` to ant-switch, guarded).
- Fault-history semantics: error-over-fault precedence, case-insensitive 'NONE' suppression, uppercasing, per-address auto-clear on mismatch, 30-record cap, key format `address|TEXT`.
- DX feed: URL shape `?qth=&minutes=30&surroundings=true`; only `sourceType=='mqtt'`; dedup key and freshness rules; 600 s max age; 80-spot cap; 2 s ×1.5 → 60 s backoff reset on a good event; 5-min idle watchdog with line-reset; 15 s connect timeout; radio-mode→SNR-family mapping with 0 dB (ssb) / −15 dB (cw) defaults.
- Spot `lat`/`lng` are the REMOTE station's coordinates — do not re-derive position from the spot's locator for dot placement (the locator is used only for labels/grid squares); a reimplementation reusing the payload's lat/lng is correct.
- Grid-square aggregation: 4-char prefix grouping, dominant band, top-quartile-mean score.
- Projection math if pixel-identical maps matter: AEQD with earth radius 6371 km, `radius = min(w,h) × 0.47`, `scale = radius × zoom / π`, y-axis flipped, near-antipode clip at c = π − 0.02, horizon 20015 km; Maidenhead decode/bounds per §"locatorToLatLng"/"locatorToBounds" (field 20°×10° anchored at (−180,−90), square 2°×1°, subsquare 5′×2.5′ with center offsets); Mercator with radius 6378137, tile 256, lat clamp ±85.05112878°, min-zoom = log2(height/256). These are ports of horstreporter's own JS so the two frontends' maps coincide.
- Keepalive 20 s, clean session, unique-per-launch client id.
- Credential keys and their secure-storage placement; locator uppercasing; empty-password ⇒ setup screen.

**Free to change (implementation detail):**

- Flutter, `provider`, `ChangeNotifier`, the `mqtt_client` package, `clock` injection — any state container and MQTT/SSE library that honors the contracts above.
- The conditional-import platform split (`client_factory*.dart`, `dxspot_source*.dart`) — an artifact of Dart browser limitations; any equivalent per-platform transport selection is fine. On web, MQTT-over-WebSocket via the bridge (or any other browser-reachable tunnel) is required because browsers cannot open raw TCP.
- The `hot()` ValueNotifier API (dormant).
- The Go webbridge implementation — any byte-transparent WebSocket↔TCP proxy at `/mqtt` plus static file serving works; the systemd unit and deploy script are the ops interface.
- Widget-tree structure, theme, panel layout (covered by a different reader).
- The 30-record fault cap and 80-spot cap are tuning knobs — but changing them changes observable behavior; document if changed.

**Open questions / things the code does not reveal:**

- Whether the deployed reality is the one-broker (.50) or two-broker (.139) topology (branch unmerged, deploy pending as of 2026-08-29) — §6.4.
- The full set of values `dvk_status` can take beyond `'idle'` (only `'idle'` appears in fixtures; other values come from flexbridge, not this app).
- All values of `antenna-select` `mode` beyond `'auto'`/`'manual'` (fixtures only use these).
- The physical truth of Ultrabeam on port 3 vs port 4 (docs disagree; app says 4).