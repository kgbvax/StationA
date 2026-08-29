# 03 — Component spec: wrc-rotator-bridge (HF antenna rotator bridge)

This document specifies the bridge that connects the Mühle amateur-radio station's HF-band antenna rotator — a Yaesu G-450DC motorized antenna mount, steered through a third-party "WRC" (Web Remote Control) controller unit — to the station's MQTT message bus. The bridge is a small always-on daemon that (a) reads a continuous status stream from the WRC over a WebSocket and republishes it as one retained JSON state document on the bus, (b) accepts turn/stop commands from the bus and forwards them to the WRC, and (c) optionally exposes two additional inbound control paths for legacy desktop rotator-control software (a GS-232B TCP server and a PSTRotator UDP listener). A team reconstructing this component from this document alone — with no access to the original code — must be able to build a behaviorally equivalent service. All topics, field names, types, units, timings, and error behaviors below are **normative**; sections explicitly marked "Reference-implementation notes" are non-normative background about the original build.

**Plain-language vocabulary used throughout** (each term is defined once here; later sections use them freely):

- **Amateur radio (ham) station**: a licensed private two-way radio installation. This one is named "Mühle"; its bus address prefix is `muehle`.
- **HF (high frequency)**: the amateur short-wave bands, roughly 3–30 MHz, used for long-distance communication; the directional antennas this rotator turns are HF antennas.
- **UHF (ultra-high frequency)**: the roughly 300 MHz–3 GHz bands; the station's separate UHF antenna positioner is a *different* component (§1) and is not covered by this document.
- **Rotator**: an electric motor mount that physically turns a directional antenna so it points at a chosen compass heading.
- **Azimuth (`az`)**: compass bearing in degrees; 0 = north, 90 = east, 180 = south, 270 = west.
- **CW / CCW**: clockwise / counter-clockwise rotation as seen from above; CW increases azimuth. (Unrelated to "CW" the Morse-code operating mode.)
- **MQTT**: a lightweight publish/subscribe message protocol. Clients publish messages addressed to *topics* (hierarchical strings like `muehle/hf/rotator/state`) and subscribe to topic filters.
- **Retained message**: an MQTT message the broker stores and re-delivers to every future subscriber of that topic until replaced — it acts as "the last known value".
- **QoS**: MQTT delivery-guarantee level. QoS 0 = at-most-once, QoS 1 = at-least-once.
- **LWT (Last Will and Testament)**: a message the broker publishes on a client's behalf if that client disconnects *uncleanly* (crash, network loss). Used as the process-liveness flag.
- **Slot / plane**: the station models every device as a bus address `<site>/<station>/<slot>` (here `muehle/hf/rotator`) with four parallel *planes*: `/meta` (identity/capabilities), `/state` (live data), `/status` (process liveness), `/cmd` (inbound commands). See `02-interface-spec.md`.
- **Bridge**: a daemon translating between one physical device and the bus. This component is the bridge for the `rotator` slot.
- **WRC**: "Web Remote Control", a third-party rotator controller (by radio amateur AF6SA) wired to the rotator's motor and sensors. It is the *upstream device* this bridge talks to; it exposes a WebSocket server for monitoring and steering.
- **WebSocket**: a standard protocol that upgrades an HTTP connection into a bidirectional message stream.
- **GS-232B**: a classic ASCII command protocol for Yaesu rotator controllers (commands like `C`, `M180`, `S`), spoken by most legacy rotator-control software.
- **PSTRotator**: a popular Windows rotator-control program that steers rotators by sending short XML-style datagrams over UDP.
- **shari**: the Raspberry Pi single-board computer (192.168.1.139) that runs all station services.
- **Service supervisor**: the host's service manager that starts the daemon at boot, restarts it on failure, and delivers the shutdown/stop signal. A stack-agnostic term; in the reference implementation it is systemd (see §7 reference-implementation notes).

---

## 1. Role and context

The bridge fills the slot `muehle/hf/rotator`. It is a **read-write** slot: it publishes rotator status and accepts motion commands. It replaced an older third-party bridge ("rotint") that used a different topic tree (`rotor2mqtt/…`) and carried its MQTT password on the process command line; see §9 for the one-time migration.

The HF rotator is azimuth-only (it turns the antenna horizontally; it does not tilt it). The separate UHF antenna positioner is a *different* component (see `03-components/pelcobridge2.md`, slot `muehle/uhf/rotator`); the two must not be conflated.

The bridge's four responsibilities:

1. Connect to the WRC's WebSocket (default endpoint `ws://192.168.1.108/wsrotor`), which continuously streams the rotator's status as JSON frames, and republish that status — deduplicated — as a single retained JSON document on `muehle/hf/rotator/state`.
2. Accept commands on `muehle/hf/rotator/cmd` (`set_az`, `stop`, `fwd`, `rev`) and forward them to the WRC over the same WebSocket.
3. Optionally (enabled by default) run a GS-232B TCP server on port 7373 and a PSTRotator UDP listener on port 12040 so legacy desktop software can drive the same rotator directly. Motion resulting from either legacy path still surfaces in `/state`, because all three paths funnel into the same serialized device-write path.
4. Publish a retained `/meta` "birth certificate" describing identity and capabilities, and a retained `/status` liveness flag backed by an MQTT Last Will.

Whether the two legacy paths (item 3) are mandatory for a re-implementation is an **open decision** — see §11.

## 2. Upstream interface — the WRC WebSocket

### 2.1 Transport and connection parameters

**Transport**: plain (unencrypted, unauthenticated) WebSocket to a configurable URL (config key `rotor.url`; default `ws://192.168.1.108/wsrotor`). The bridge acts as the WebSocket *client*. There is no HTTP header customization, no subprotocol negotiation, no authentication, and no application-level handshake: once the socket is open, the WRC pushes JSON status frames and the bridge may at any time push JSON command frames.

Normative connection requirements:

- The WebSocket handshake SHALL time out after **10 seconds** if the WRC does not complete the upgrade.
- A shutdown signal received during the handshake SHALL abort the dial.
- The bridge SHALL NOT require any frame from the WRC before sending its first command frame.

### 2.2 Downstream frames (WRC → bridge): status documents

Each WebSocket text message is one JSON status document:

```json
{
  "state": "rotating",
  "name":   "…",
  "az":     123.5,
  "lim1":   0.0,
  "lim2":   450.0,
  "tdeg":   180.0,
  "fmsg":   "…"
}
```

| Key | JSON type | Required | Meaning |
|---|---|---|---|
| `state` | string | yes | controller state string; observed values: `rotating` while turning, `stopped` or `idle` at rest |
| `name` | string | no | rotor label; parsed but unused by the bridge |
| `az` | number | yes | current azimuth in degrees |
| `lim1` | number | no | counter-clockwise rotation limit in degrees; parsed but unused |
| `lim2` | number | no | clockwise rotation limit in degrees; parsed but unused |
| `tdeg` | number | no | commanded target azimuth in degrees |
| `fmsg` | string | no | fault message |

The WRC streams these frames continuously (even when the antenna is stationary); the exact cadence is firmware-determined. The bridge MUST tolerate a continuous frame rate and MUST deduplicate before publishing (§3.3). Frames with unparsable JSON SHALL be skipped with a warning log and SHALL NOT close the connection.

### 2.3 Upstream frames (bridge → WRC): command documents

Every command frame is a single JSON object with exactly one key, `az`, whose value is **always a string**:

| Command | Wire frame | Meaning |
|---|---|---|
| Rotate to azimuth | `{"az":"180"}` | Rotate to 180° |
| Stop | `{"az":"stop"}` | Halt motion immediately |
| Jog clockwise | `{"az":"fwd"}` | Continuous CW rotation until stopped or a limit is hit |
| Jog counter-clockwise | `{"az":"rev"}` | Continuous CCW rotation until stopped or a limit is hit |

**NORMATIVE — string quoting**: the azimuth number MUST be sent as a quoted string containing an integer with no decimal point and no exponent (e.g. `"180"`, never `180` and never `"180.0"`). The controller firmware ignores numeric JSON values; sending `{"az":180}` is a silent no-op. This is the single most easily-missed requirement of the upstream protocol.

All writes to the WRC MUST be serialized: at most one in-flight command frame at a time across all inbound paths (MQTT, GS-232B, PSTRotator). Commands from different paths SHALL NOT interleave on the wire.

### 2.4 Canonicalization rules

- `moving` is derived from the raw `state` string: lower-case it; `moving` is `true` if and only if the string contains the substring `rotat` OR the substring `moving`. An empty state string yields `false`. (Deliberately fuzzy so renamed firmware strings still map; see §10 for the downside.)
- `target_az` is taken from `tdeg` **only when `tdeg != 0`** — consequently a commanded target of exactly 0° (north) is unrepresentable and omitted; see §10.
- The raw `state` string is preserved verbatim in the published field `rotor_state` for diagnostics.
- `fmsg` maps to the published field `error`.
- The last known `az` SHALL be kept across a reconnect; the bridge never zeroes the cached azimuth while the process lives.

### 2.5 Connection-loss detection

Loss of the WRC link is detected ONLY by a WebSocket read error (the read loop exits on any read failure) or by process shutdown closing the socket. There is no ping/pong keepalive and no idle/read timeout in the reference implementation. On loss, the bridge marks the device offline (§5.2) and retries with backoff.

## 3. MQTT presence

MQTT 3.1.1 over plain TCP. Default broker: `tcp://192.168.1.50:1883` (the shack broker; a planned migration to a broker on shari 192.168.1.139 exists but was not deployed when this PRD was written — see `00-system-overview.md`). Username `hf` (used only if configured non-empty); password supplied via an environment variable, never in the config file. Default client ID: `muehle-hf-rotator`.

Normative MQTT-session requirements:

- The bridge SHALL register, at connect time, a Last Will: topic `<slot>/status`, payload `offline`, QoS 1, retained.
- The bridge SHALL auto-reconnect to the broker, retrying every **5 seconds** during a broker outage.
- The initial broker connect SHALL be interruptible by the shutdown signal (i.e. a shutdown request during a hanging initial connect MUST result in process exit within the 1-second shutdown-latency bound of §5.1, without waiting for TCP timeouts).
- On every (re)connect, the bridge SHALL: publish `online` to `/status` (retained), republish `/meta` (retained), and resubscribe `/cmd` at QoS 1.
- Retained topics are published at QoS 1; inbound `/cmd` is subscribed at QoS 1.

### 3.1 Topics (exact strings, defaults)

| Topic | Direction | Retained | QoS | Cadence |
|---|---|---|---|---|
| `muehle/hf/rotator/meta` | publish | yes | 1 | on every MQTT (re)connect |
| `muehle/hf/rotator/state` | publish | yes | 1 | only when a published field changes (§3.3) |
| `muehle/hf/rotator/status` | publish (LWT + on connect) | yes | 1 | `online` on every (re)connect; `offline` via broker Last Will on unclean death |
| `muehle/hf/rotator/cmd` | subscribe | no | 1 | resubscribed on every (re)connect |

Topic strings are built as `<site>/<station>/<slot>/<plane>` from config (`site=muehle`, `station=hf`, `slot=rotator` by default).

`/cmd` is deliberately **not retained** by publishers: a rotator move is one-shot intent, and replaying a stale azimuth after a restart could spin the antenna unexpectedly. There is NO desired-state reconciliation loop; consumers observe the result in `/state`.

### 3.2 `/meta` payload (retained birth certificate)

Published identically on every MQTT connect. Exact JSON (defaults shown; `host`, `location`, `device.model`, `link` come from config):

```json
{
  "schema": "1.0",
  "role": "rotator",
  "device": { "model": "Yaesu G-450DC" },
  "link": "ethernet",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": { "axes": ["az"] },
  "expose": {
    "device": { "name": "HF Rotator", "model": "Yaesu G-450DC", "manufacturer": "Yaesu" },
    "fields": [
      { "key": "az", "name": "Azimuth", "type": "number", "unit": "°", "class": "azimuth",
        "state_class": "measurement", "writable": true, "min": 0, "max": 450, "step": 1,
        "command": { "action": "set_az", "value_key": "az", "value_type": "float" } },
      { "key": "target_az", "name": "Target Azimuth", "type": "number", "unit": "°",
        "class": "azimuth", "state_class": "measurement" },
      { "key": "moving", "name": "Moving", "type": "boolean" },
      { "key": "rotor_state", "name": "Rotor State", "type": "string" },
      { "key": "device_online", "name": "Device Online", "type": "boolean" }
    ],
    "actions": [
      { "key": "stop", "name": "Stop",      "command": { "action": "stop" } },
      { "key": "fwd",  "name": "Rotate CW",  "command": { "action": "fwd" } },
      { "key": "rev",  "name": "Rotate CCW", "command": { "action": "rev" } }
    ]
  }
}
```

Normative rules:

- `role` SHALL be the canonical slot role `rotator` — never a device name or model.
- `capabilities.axes` SHALL be `["az"]` (azimuth-only, hard-coded).
- The `expose` block is a consumer-neutral field/action surface; the separate discovery component (`03-components/hadiscovery.md`) reads it programmatically to render home-automation entities. **This bridge itself SHALL NOT publish anything under `homeassistant/…`.**
- `min:0, max:450, step:1` on the `az` field are advertised metadata only. The bridge does NOT clamp or validate incoming azimuths against them (§10); a consumer MUST NOT rely on the bridge enforcing this range.
- No `area` field is published; the discovery component supplies a deployment-wide default area.
- In the reference implementation the `expose.device.name` (`"HF Rotator"`) and `manufacturer` (`"Yaesu"`) values are hard-coded rather than configurable; everything else in `/meta` is config-driven.

### 3.3 `/state` payload (retained live snapshot)

A single JSON document, published **only when a published field changes** (§3.3):

```json
{
  "ts": "2026-07-06T12:34:56Z",
  "az": 123.5,
  "target_az": 180.0,
  "moving": true,
  "rotor_state": "rotating",
  "device_online": true
}
```

| Field | JSON type | Unit | Presence | Semantics |
|---|---|---|---|---|
| `ts` | string | — | always | RFC 3339, UTC; the time of *this publish* (the WRC frame carries no timestamp) |
| `az` | number | degrees | always | current azimuth, raw WRC value passed through |
| `target_az` | number | degrees | omitted when absent/zero | commanded target from WRC `tdeg` |
| `moving` | boolean | — | always | derived from `rotor_state` per §2.4 |
| `rotor_state` | string | — | omitted when empty | raw WRC state string |
| `device_online` | boolean | — | always | `true` iff the WRC WebSocket is currently up; `false` when the bridge process is alive but the WRC link is down |
| `error` | string | — | omitted when empty | human-readable fault: WRC `fmsg` when connected, or `wrc: <error text>` when the link drops |

Publish triggers, exactly: (a) every parsed WRC status frame differing from the last published snapshot in any of the six fields `az`, `target_az`, `moving`, `rotor_state`, `device_online`, `error`; (b) the WebSocket dropping or a reconnect attempt changing `device_online`/`error`.

A rotator `/state` document SHALL NEVER carry `freq_hz`, `band`, or `mode` — those are radio concerns belonging to other slots (see `02-interface-spec.md`).

`device_online` is published as an explicit boolean, matching the deployed form. (The integration model's phrase "omitted when true" conflicts with deployed behavior across all bridges; consumers must treat both forms as equivalent — see `02-interface-spec.md` §liveness and §11 below.)

### 3.4 `/status` (process liveness)

Plain string payload (not JSON): exactly `online` or `offline`, retained, QoS 1. `online` is published by the bridge on every MQTT (re)connect. `offline` is registered as the MQTT Last Will (§3), so the broker publishes it only if the bridge dies uncleanly.

**Two-layer liveness is normative.** `/status` reflects the **bridge process only**. If the WRC vanishes while the bridge runs, `/status` stays `online` and `/state.device_online` flips to `false`. Consumers MUST AND the two signals. Conversely, the bridge SHALL NOT report bridge-process death in `/state`, and SHALL NOT flip `/status` offline when only the WRC link drops.

**Actual behavior, not idealized**: on a CLEAN process shutdown the broker does not fire the Last Will, and the reference implementation never publishes `offline` itself — so after a clean stop the retained `/status` can remain `online` indefinitely until the process next appears. Consumers MUST NOT trust `/status` alone. (The component's API doc claims `offline` is "published on clean shutdown"; the code does not do this. This is a known defect/discrepancy — §10.)

## 4. Command surface

### 4.1 MQTT `/cmd`

Not retained, QoS 1. Each payload is one JSON object:

| Action | Payload | Behavior |
|---|---|---|
| `set_az` | `{"action":"set_az","az":180}` | Rotate to 180°. Forwards `{"az":"180"}` to the WRC. `az` is a JSON number (any float accepted; no range check). |
| `stop` | `{"action":"stop"}` | Halt motion; forwards `{"az":"stop"}`. |
| `fwd` | `{"action":"fwd"}` | Continuous jog clockwise; forwards `{"az":"fwd"}`. Runs until a limit or a `stop`. |
| `rev` | `{"action":"rev"}` | Continuous jog counter-clockwise; forwards `{"az":"rev"}`. |

Rules:

- **The argument key for `set_az` is `az`**, not the station's generic `value` key (see §11). The authoritative machine-readable declaration is the `/meta` `expose` field descriptor (`command.value_key: "az"`); consumers that read `/meta` need no special case.
- Unknown actions, `set_az` with a missing `az`, and malformed JSON are each logged (warning) and ignored.
- No acknowledgment or reply topic exists. Success is observed by watching `/state`: `moving` → `true`, `az` → target, `target_az` → commanded value.
- The bridge MAY be deployed without a device writer (read-only mode), in which case any command is logged and ignored.

**Dispatch pipeline (normative)**: the MQTT receive-path handler SHALL only enqueue the raw payload onto a bounded queue of capacity **8**; a single worker SHALL parse and execute commands one at a time, strictly FIFO. When the queue is full, the excess command SHALL be dropped with a warning log — the MQTT receive path SHALL NEVER block on device I/O. (Blocking the receive path deadlocks the MQTT client library; this constraint is library-independent — see §8, invariant 5.)

**Failure mode**: if the WRC WebSocket is down when a command is dequeued, the write fails, the failure is logged, and the command is **discarded — no retry, no pending queue across reconnect**. A `stop` issued during a WRC outage is therefore lost, and the antenna keeps turning per the controller's own last command. Changing this is a PRD-level decision (§11).

### 4.2 GS-232B TCP server (legacy path, optional; default enabled on `0.0.0.0:7373`)

One command per line, terminated by `\r` or `\n` (either alone). Input is trimmed and upper-cased before matching.

| Line in | Behavior | Response |
|---|---|---|
| `C` or `C2` | query position | `+0aaa+0000\r` where `aaa` is the current azimuth zero-padded to 3 digits, truncated to integer (e.g. azimuth 180 → `+0180+0000\r`); the trailing `+0000` is a fixed dummy elevation (elevation: the antenna's vertical tilt angle, which this azimuth-only rotator does not have — §1) |
| `M` + 1–3 digits (e.g. `M090`) | rotate to that azimuth | `\r` |
| `W` + 1–3 digits + whitespace + integer (e.g. `W180 000`) | rotate to that azimuth; elevation argument parsed and ignored | `\r` |
| `S` | stop | `\r` |
| anything else | — | `?>\r` |

Edge behaviors (normative, matching the deployed protocol as observed by legacy clients):

- The `M` and `W` matchers are anchored **only at the start of the line**: they prefix-match and silently ignore any trailing characters after the captured digits.
  - For `M`: the **first 1–3 leading digits** after the letter are captured and everything after them is ignored. `M1234` is treated as `M123` (ack `\r`, rotate to 123°); `M12.5` and `M12X` likewise rotate to 12° and ack. Only `M` followed by **zero** digits gets **no response at all** — no `\r`, no error.
  - For `W`: the 1–3 azimuth digits plus the whitespace-separated integer elevation argument are **required** (a `W` line lacking either gets no response at all), but trailing characters after the elevation integer are ignored (`W180 000abc` → rotate to 180°, ack `\r`).
  - This lenient prefix matching is normative deployed behavior; a re-implementation that instead enforced strict full-line matching (rejecting `M1234`) would produce observably different wire behavior. Tightening is a behavior-change decision — see §11.
- The digit-less no-response case is known to be able to hang naive line-oriented clients awaiting acknowledgment — §10.
- The line reader stops at the first `\r` or `\n` and does not consume a following `\n` (a `\r\n` pair leaves the `\n` to be read as a harmless empty line).
- The azimuth used in `C` replies is the last WRC status frame; if the WRC has never been reached since process start, replies are `+0000+0000`.
- Commands funnel into the same serialized WRC write path as MQTT; resulting motion surfaces in `/state`.

### 4.3 PSTRotator UDP listener (legacy path, optional; default enabled on `0.0.0.0:12040`)

Datagrams up to **1024 bytes**; XML-style content matched case-insensitively with tolerant (whitespace-flexible) pattern rules.

| Datagram content | Behavior | Response |
|---|---|---|
| contains `AZ?` (e.g. `<PST>AZ?</PST>`) | position query | `<PST><AZIMUTH>aaa</AZIMUTH></PST>` (integer azimuth) sent as a UDP datagram to the **source IP, port + 1** (12041 by default) — the PSTRotator reply convention |
| contains `<STOP>…</STOP>` | stop motion | none |
| contains `<PARK>…</PARK>` | logged, ignored (no park support) | none |
| contains `<AZIMUTH>-?\d+(\.\d+)?</AZIMUTH>` (negatives and decimals allowed; e.g. `<PST><AZIMUTH>180</AZIMUTH><ELEVATION>45</ELEVATION></PST>` → 180, elevation ignored) | rotate to that azimuth | none |
| anything else | logged | none |

Precedence is normative: `AZ?` first, then `STOP`, then `PARK`, then `AZIMUTH` — so a datagram carrying both `<STOP>` and `<AZIMUTH>` stops. All drive the same serialized WRC write path.

## 5. Behavior & state machine

### 5.1 Startup (in order)

1. Load config: built-in defaults ← TOML file (flag `-config`, default path `/etc/wrc-rotator-bridge/config.toml`) ← environment overrides ← command-line flag overrides. A missing file is acceptable (defaults apply); malformed config or validation failure exits with code **2**.
2. Validation: `mqtt.site` and `mqtt.station` must be non-empty; `rotor.url` must be non-empty; `gs232.port` and `pstrotator.port` must be non-zero when the respective server is enabled. Violations exit 2.
3. Install a SIGINT/SIGTERM handler (cancellation).
4. Connect MQTT FIRST, with the Last Will registered. On connect: publish retained `online` to `/status`, publish retained `/meta`, subscribe `/cmd` (QoS 1). **MQTT connect failure at boot is fatal**: exit code 1; the service supervisor (the host's service manager that starts the daemon at boot, restarts it on failure, and delivers the shutdown/stop signal) restarts it after 5 s.
5. Start the GS-232B TCP server and the PSTRotator UDP listener. A listen failure (e.g. port already taken) is only logged as an error — the process keeps running without that path (for its whole lifetime; a restart is needed to retry).
6. Enter the WRC WebSocket loop (§5.2), which runs until shutdown.

**Shutdown**: SIGTERM/SIGINT cancels the run; the WRC read loop is nudged closed; the MQTT client disconnects with a 500 ms quiesce; exit code **0** (so a clean service stop reports success). Any other runtime error exits 1. Exit codes are normative: 0 on SIGTERM/SIGINT, 2 on config errors, 1 on runtime failure.

**Shutdown latency (normative bound)**: a shutdown signal received during a hanging initial MQTT connect, the WebSocket handshake, or a backoff sleep SHALL result in process exit within **1 second**, without waiting for any network timeout or any supervisor kill timeout. Test: issue the shutdown signal while the broker address is black-holed (and, separately, during a backoff sleep) and assert process exit within 1 s.

### 5.2 WRC WebSocket loop (reconnect behavior)

```
attempt = 0; backoff = 2 s
loop:
    dial WRC (handshake timeout 10 s)
    on success:
        device-online goes true internally; read frames;
        each parsed frame updates state and publishes /state on change
        (the first frame after a reconnect flips /state.device_online back to true)
    on dial or read error (and shutdown not requested):
        publish /state with device_online=false and error="wrc: <error>"
        sleep backoff (interruptible by shutdown)
        backoff = min(backoff * 1.5, 60 s)
        retry
```

Exact values (normative): initial backoff **2 s**, growth factor **1.5×**, cap **60 s** (sequence: 2 s, 3 s, 4.5 s, 6.75 s, … capped at 60 s). The backoff resets ONLY on process restart — it does not reset after a successful connection, so repeated failures within one process lifetime drive the interval to the 60 s cap and keep it there (§10).

During all WRC retries the bridge stays connected to MQTT: `/status` stays `online` and `/meta` stays retained; only `/state.device_online` reflects the outage. The last known azimuth is preserved across reconnects (snapshots published during an outage keep the old `az`).

Between a successful re-dial and the first status frame, the device's cached state is rebuilt with only the old azimuth preserved (`moving=false`, empty `rotor_state`, empty `target_az`) — a published snapshot in that window can look "cleaner" than reality for one frame (§10).

### 5.3 MQTT reconnect behavior

Broker outages are handled by auto-reconnect (retry every 5 s). On every (re)connect: `online` → `/status` (retained), `/meta` republished (retained), `/cmd` resubscribed (QoS 1). Commands issued by others while the broker link is down are simply not received (nothing is retained). Publishes issued while disconnected are lost silently — the publisher never blocks on delivery and never retries; retained documents self-heal on the next publish, since the next change after reconnect republishes current truth.

### 5.4 Error-path summary

| Event | Visible effect | Recovery |
|---|---|---|
| WRC dial fails / WebSocket drops | `/state`: `device_online:false`, `error:"wrc: …"`; `/status` unchanged | backoff retry loop §5.2 |
| WRC sends unparsable JSON | warning log; frame skipped; connection kept | none needed |
| `/cmd` malformed / unknown action / `set_az` missing `az` / read-only deploy | warning log, ignored | — |
| `/cmd` while WRC down | warning log, command dropped | operator re-issues |
| >8 commands queued | warning log, command dropped | operator re-issues |
| GS-232 / PSTRotator listen fails | error log; that path disabled for process lifetime | process restart |
| GS-232 client disconnects | connection closed, logged | — |
| MQTT connect fails at boot | process exit 1 | service supervisor restart, 5 s delay |
| Bridge crash / broker link loss | broker fires Last Will → `/status` = `offline` (retained) | service supervisor restart |

## 6. Configuration

Config file: TOML, default path `/etc/wrc-rotator-bridge/config.toml` (flag `-config <path>`). Layering (normative): built-in defaults ← TOML ← environment ← flags. Unknown TOML keys are ignored (not an error).

| Key | Default | Meaning |
|---|---|---|
| `host` | `shari` | compute-node name, published in `/meta.host` |
| `rotor.url` | `ws://192.168.1.108/wsrotor` | WRC WebSocket endpoint (mandatory non-empty) |
| `gs232.enabled` | `true` | run the GS-232B TCP server |
| `gs232.bind` | `0.0.0.0` | GS-232 listen address |
| `gs232.port` | `7373` | GS-232 listen port (non-zero when enabled) |
| `pstrotator.enabled` | `true` | run the PSTRotator UDP listener |
| `pstrotator.bind` | `0.0.0.0` | PSTRotator listen address |
| `pstrotator.port` | `12040` | PSTRotator listen port (non-zero when enabled) |
| `device.model` | `Yaesu G-450DC` | identity string in `/meta.device.model` and `expose.device.model` |
| `device.link` | `ethernet` | transport label in `/meta.link` |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | broker URI |
| `mqtt.client_id` | `""` → `muehle-hf-rotator` | MQTT client ID |
| `mqtt.user` | `hf` | MQTT username |
| `mqtt.password` | `""` (overridden by env) | MQTT password — must be kept out of the TOML |
| `mqtt.site` / `mqtt.station` / `mqtt.slot` | `muehle` / `hf` / `rotator` | bus address; site and station mandatory |
| `mqtt.location` | `bauwagen` | physical-location label in `/meta.location` |
| `mqtt.discovery_prefix` | `homeassistant` | legacy key; parsed, wired to nothing (dead) |
| `mqtt.publish_ha_discovery` | `false` | legacy key; parsed, wired to nothing (dead) |
| `log.level` | `info` | `debug` \| `info` \| `warn` \| `error` |

Flags: `-config <path>`, `-log.level <level>` (overrides config), `-debug` (log raw WRC WebSocket frames in both directions).

Environment overrides (all prefixed `WRC_ROTATOR_BRIDGE_`): `_MQTT_BROKER`, `_MQTT_CLIENT_ID`, `_MQTT_USER`, `_MQTT_PASSWORD` (the secret), `_MQTT_SITE`, `_MQTT_STATION`, `_MQTT_SLOT`, `_ROTOR_URL`. A non-empty environment value replaces the TOML value.

**Secrets (normative)**: the MQTT password lives only in a 0600 environment file (`/etc/wrc-rotator-bridge/wrc-rotator-bridge.env`, owned by the service user) as `WRC_ROTATOR_BRIDGE_MQTT_PASSWORD="…"`, loaded by the service supervisor. It MUST NOT appear in the TOML, the unit definition, or the process command line. See `docs/conventions/config-and-secrets.md` (as reconstructed in `05-deployment-ops.md`).

## 7. Deployment

- Target host: the Raspberry Pi `shari` at 192.168.1.139.
- The deploy procedure: cross-compile a static binary for the Pi; generate the service unit, a seed config.toml, and a seed env file (mode 0600, password in a separate temp file); copy them to the Pi; then over SSH: create a dedicated system service user (no home, nologin) if missing; install the binary under `/opt/wrc-rotator-bridge/`; **seed-once** — install `/etc/wrc-rotator-bridge/config.toml` (0600, service-user owned) and the env file only if they do not already exist; stop the service, move binary + unit into place, reload the service supervisor, enable, restart, print status.
- **Seed-once is normative**: after first install the on-device config and env file are owned by the deployment; a redeploy MUST never overwrite them.
- Service requirements: run as the dedicated unprivileged user; restart on failure with a 5 s delay; start after network availability; logs to the system journal with identifier `wrc-rotator-bridge`.
- Hardening requirements (the service needs only network access — two inbound listen sockets plus outbound TCP): no-new-privileges; read-only filesystem except one writable state directory (`/var/lib/wrc-rotator-bridge`); private /tmp; no device access beyond pseudo-devices; kernel-tunable/module/cgroup protection; address families restricted to `AF_INET`/`AF_INET6`; no namespaces, no real-time scheduling, no SUID/SGID, no IPC; empty capability bounding and ambient sets; memory limit 256 MB; task limit 64.
- **One-time migration from the legacy `rotint` bridge** (run before the first deploy of this component, only on a Pi that previously ran rotint): on the Pi, create the new service user and config directory; extract the MQTT password from the old service unit's command line **on the device** and write it into the new env file (0600) — repairing the old bridge's command-line secret exposure; stop and disable the old service and remove its unit; leave the old bridge's files and its user untouched. Idempotent; backs up any pre-existing env file to `*.pre-migration`. The old rotint bridge published a different topic tree (`rotor2mqtt/…`) plus embedded home-assistant discovery; this component replaces it entirely.

Reference-implementation notes (non-normative): the original is a Go daemon supervised by systemd (`Type=simple`, unit `wrc-rotator-bridge.service`, `ExecStart=/opt/wrc-rotator-bridge/wrc-rotator-bridge -config /etc/wrc-rotator-bridge/config.toml`, `EnvironmentFile=/etc/wrc-rotator-bridge/wrc-rotator-bridge.env`, `Restart=on-failure`, `RestartSec=5`, `After=network-online.target`), deployed by `deploy.sh` cross-compiling with `GOOS=linux GOARCH=arm64 CGO_ENABLED=0` and stripped symbols. Dependencies: `gorilla/websocket` v1.5.3, `eclipse/paho.mqtt.golang` v1.5.1, `BurntSushi/toml` v1.6.0, and the shared in-repo module for topic builders and context-aware MQTT connect. Any equivalent stack satisfying the normative requirements above is acceptable.

## 8. Invariants (normative requirements)

1. **`/cmd` is never treated as retained state.** No desired azimuth is stored or re-applied on any restart or reconnect; a move is one-shot intent. A re-implementation SHALL NOT add desired-state reconciliation that replays the last commanded azimuth.
2. **Two-layer liveness stays two-layer.** `/status` reflects the bridge process only; WRC-link state lives exclusively in `/state.device_online`. Never flip `/status` offline on a WRC drop; never report bridge death in `/state`.
3. **`/state` and `/meta` are single retained JSON documents; `/status` is a plain `online`/`offline` string.** No per-field topics; no `homeassistant/…` topics from this bridge.
4. **All WRC writes are serialized** (one in-flight frame at a time across MQTT, GS-232B, and PSTRotator paths), and **absolute azimuths are sent as quoted strings** (`{"az":"180"}`), never JSON numbers — the controller ignores numeric values.
5. **The MQTT receive path must never block on device I/O.** Incoming commands are enqueued to a bounded queue (capacity 8, drop-on-overflow) and executed by a single worker. Rationale (library-independent): in the reference MQTT library, handlers run on the connection's dispatch thread — a handler that blocks or publishes synchronously deadlocks the client; this class of failure has occurred live elsewhere in this station. Any re-implementation must isolate handler work from the receive path regardless of library.
6. **Exit codes: 0 on SIGTERM/SIGINT, 2 on config errors, 1 on runtime failure.** Clean shutdown must not look like a crash to the service supervisor.
7. **No secrets in the config file, unit definition, or command line** — only in the 0600 environment file; deploy must never overwrite the on-device env file after first install.
8. **A rotator never publishes radio fields** (`freq_hz`, `band`, `mode`).
9. **`stop` takes precedence over an embedded azimuth within a single PSTRotator datagram**; a stop that races a separate set command is resolved by FIFO arrival at the serialized write path.
10. **Shutdown interruptibility**: SIGTERM during a hanging initial MQTT connect, the WebSocket handshake, or a backoff sleep must result in process exit within **1 second** (the shutdown-latency bound of §5.1) — no reliance on service-supervisor kill timeouts.

## 9. Relationship to the rest of the system

- The rotator is a standalone read-write slot; no other component commands it automatically today. The console UI (`04-console.md`) lets the operator steer it; any future automation would command it like any consumer (via `/cmd`, reading `value_key` from `/meta.expose`).
- The discovery component (`03-components/hadiscovery.md`) renders home-automation entities from `/meta.expose`; this bridge carries no home-automation vocabulary itself.
- Liveness conventions shared by all bridges (including the clean-shutdown `/status` caveat) are defined once in `02-interface-spec.md` §liveness.

## 10. Known defects & fragilities (as observed; a re-implementation must decide per §11 whether to fix)

- **`target_az` of 0 is unrepresentable**: `target_az` is set only when `tdeg != 0`, so a commanded target of exactly 0° (north) is dropped. Conflation of "absent" and "zero".
- **No watchdog on the WRC stream**: no read/idle timeout. If the WRC TCP connection hangs without closing (half-open, silent device), the bridge reports `device_online:true` forever with a frozen azimuth; only a socket error triggers recovery.
- **Commands are silently dropped when the WRC is down — including `stop`.** No pending-command queue exists for reconnects; a stop issued during an outage is lost and the antenna keeps turning per the controller's own last command.
- **No azimuth range validation anywhere**: MQTT `set_az` (any float), GS-232 (up to `M999`), and PSTRotator (arbitrary decimals and negatives) are forwarded verbatim. `/meta` advertises `min:0 max:450` but nothing enforces it; out-of-range handling is left entirely to WRC firmware.
- **Doc/code discrepancy on clean-shutdown `offline`**: the component API doc claims `/status` `offline` is published on clean shutdown; the code never publishes `offline`, and a graceful disconnect fires no Last Will — retained `/status` remains `online` after a clean stop (§3.4).
- **Publish errors are invisible**: the publisher fires-and-forgets MQTT publishes (never waits on delivery tokens); a failed `/state` publish is silently lost (self-heals only on the next change).
- **GS-232 `M`/`W` lenient prefix matching**: the matchers capture only the first 1–3 digits and ignore trailing characters (§4.2), so `M1234` silently turns the antenna to 123° instead of being rejected — a client sending a malformed long azimuth gets an ack for a truncated value. Only a digit-less `M` (or a `W` missing its whitespace+integer elevation part) gets no reply at all, which can hang naive line-oriented clients awaiting acknowledgment. Tightening to strict full-line matching is a behavior change requiring PRD-level sign-off (§11).
- **`moving` detection is substring-based** (`rotat`/`moving`): robust to firmware renames, but any future state string merely *containing* those substrings (e.g. `pre-rotating-check`) would be misclassified as moving.
- **Backoff never resets on success** within a process lifetime; alternating success/long-failure drives the retry interval to the 60 s cap and keeps it there until restart.
- **Reconnect transient flattening**: between a successful re-dial and the first status frame, published state shows `moving:false`, empty `rotor_state`, empty `target_az` (old azimuth preserved) — one frame can look "cleaner" than reality.
- **Dead config keys**: `mqtt.publish_ha_discovery` and `mqtt.discovery_prefix` are parsed but wired to nothing (embedded home-assistant discovery does not exist in this component) — leftovers of the rotint migration.
- **Hard-coded identity strings**: `expose.device.name` ("HF Rotator") and `manufacturer` ("Yaesu") are binary constants, not config (minor; everything else in `/meta` is configurable).

## 11. Open decisions & unresolved facts

1. **Must a re-implementation keep the two legacy inbound paths?** The GS-232B TCP server (:7373) and PSTRotator UDP listener (:12040) are enabled by default in the deployed system, and at least one (which one is not recorded in the sources) exists because desktop rotator software in the shack drives the rotator through it. Evidence: they are default-on in code and config and spec'd in detail (§4.2–4.3). However, the station bus (/cmd via the console) can steer the same rotator, and the legacy paths are unauthenticated plaintext network listeners — a security-relevant surface. Decision required: (a) keep both, byte-compatible; (b) keep one (which?); (c) drop both and require bus-only control. If dropped, the exact GS-232B/PSTRotator protocol details in §4.2–4.3 become informative only. The PRD's reconstruction should treat "at least one legacy path remains reachable for desktop software" as the likely requirement pending confirmation of which software is actually in use.
2. **`set_az` argument key `az` vs the station-wide `value` convention.** The station-wide `/cmd` convention (see `02-interface-spec.md`) puts every command argument under the generic key `value` as a string, with one documented exception (the ultrabeam controller's frequency). This bridge instead uses a field-specific key `az` carrying a JSON *number* — it predates the convention. The `/meta.expose` descriptor (`value_key:"az"`, `value_type:"float"`) is the authoritative machine-readable declaration, so well-behaved consumers need no special case. Open decision for a re-implementation: preserve the deployed `az`-key grammar for compatibility with existing consumers, or migrate to `value` (which would break any consumer that hard-codes `az`). Do not silently change it.
3. **`device_online` form**: this bridge publishes `device_online:true` explicitly; the integration-model text says "omitted when true". Consumers must treat both forms as equivalent (absence = true). A re-implementation should pick one and align with the system-wide decision recorded in `02-interface-spec.md` §liveness.
4. **Whether to fix the known defects of §10** is a set of deliberate behavior changes, not bug fixes. The reference team explicitly flagged these as requiring PRD-level sign-off before "fixing": adding `/cmd` retention or desired-state reconciliation (invariant 1 forbids it as built); merging device liveness into `/status` (invariant 2 forbids it); clamping/validating azimuths; retrying or queueing commands across WRC outages (including `stop`); resetting backoff on success; adding a stream watchdog. Each fixes a real fragility (a lost `stop` during an outage is safety-relevant — the antenna keeps turning) but changes observable behavior. The reconstructing team should decide each explicitly and record the decision; in particular, the missing stream watchdog and the dropped-`stop` behavior deserve safety review against `06-safety.md`.
5. **Clean-shutdown `/status`**: the component API doc and the code disagree (doc claims `offline` is published on clean shutdown; it is not — retained `online` persists after a clean stop). The code-derived behavior is documented in §3.4. Open decision: whether a re-implementation should explicitly publish `offline` before a graceful disconnect (making the doc true), or keep the current behavior and rely on consumers treating `/status` as advisory. System-wide, consumers must not trust `/status` alone either way.
6. **WRC endpoint details**: the WRC WebSocket URL (`ws://192.168.1.108/wsrotor`) and its JSON frame vocabulary were taken from the reference code and live observation; there is no vendor documentation in this repo. The exact frame cadence, the meaning of all `state` strings beyond `rotating`/`stopped`/`idle`, and the `name`/`lim1`/`lim2` semantics are firmware-determined and unverified. The requirement that azimuths be quoted strings is confirmed from code (`strconv.FormatFloat(az,'f',0,64)`) but its firmware rationale (why numbers are ignored) is unknown.
7. **Which legacy desktop software actually uses the GS-232B / PSTRotator paths** in the Mühle shack is not recorded in the sources; this feeds decision 1.
8. **GS-232B line-matching strictness.** The deployed matchers prefix-match and ignore trailing characters after the captured digits (`M1234` → rotate to 123°, ack; §4.2). This is documented in §4.2 as normative deployed behavior. Open decision: keep the lenient prefix matching byte-compatible, or tighten to strict full-line matching (rejecting `M1234` with `?>` or no reply) — a deliberate behavior change that would alter what legacy clients observe on the wire. Do not change silently.

Reference-implementation notes (non-normative): the original is a Go daemon (module `wrc-rotator-bridge`, Go 1.26) with packages `cmd/wrc-rotator-bridge` (wiring, restart loop, bounded command queue), `internal/rotor` (WRC protocol structs, dial with 10 s handshake timeout, read loop, mutex-guarded writes), `internal/bridge` (state model, `/meta`, change-dedup, `/cmd` dispatch), `internal/gs232`, `internal/pstrotator`, `internal/config`. Free-to-change implementation details: language and libraries, package layout, the mutex/channel plumbing (any equivalent serialization satisfying invariants 4–5), the line-reader details beyond the `\r`/`\n` tolerance rules, log wording and destination, and the dead discovery config keys (a re-implementation should simply drop `mqtt.publish_ha_discovery` and `mqtt.discovery_prefix`).