# 03 — Component spec: wrc-rotator-bridge (HF antenna rotator bridge)

This document defines the bridge that connects the Mühle amateur-radio station's HF-band
antenna rotator to the station's MQTT message bus. The rotator is a Yaesu G-450DC motorized
antenna mount. A third-party "WRC" (Web Remote Control) controller unit steers it.

The bridge is a small always-on daemon. It reads a continuous status stream from the WRC over
a WebSocket and republishes the status as one retained JSON state document on the bus. It
accepts turn/stop commands from the bus and forwards the commands to the WRC. It can also run
two more inbound control paths for legacy desktop rotator-control software: a GS-232B TCP
server and a PSTRotator UDP listener. A team that reconstructs this component from this
document alone — with no access to the original code — must build a service with the same
behavior. All topics, field names, types, units, timings, and error behaviors below are
**normative**. Sections explicitly marked "Reference-implementation notes" give
non-normative background about the original build.

**Plain-language vocabulary** (this section defines each term once. Later sections use the
terms freely.):

- **Amateur radio (ham) station**: a licensed private two-way radio installation. This one
  carries the name "Mühle". Its bus address prefix is `muehle`.
- **HF (high frequency)**: the amateur short-wave bands, roughly 3–30 MHz. Operators use them
  for long-distance communication. The directional antennas this rotator turns are HF antennas.
- **UHF (ultra-high frequency)**: the roughly 300 MHz–3 GHz bands. The station's separate UHF
  antenna positioner is a *different* component (§1). This document does not cover it.
- **Rotator**: an electric motor mount that physically turns a directional antenna. The mount
  points the antenna at a chosen compass heading.
- **Azimuth (`az`)**: compass bearing in degrees. 0 = north, 90 = east, 180 = south, 270 = west.
- **CW / CCW**: clockwise / counter-clockwise rotation as seen from above. CW increases
  azimuth. (No relation to "CW" the Morse-code operating mode.)
- **MQTT**: a lightweight publish/subscribe message protocol. Clients publish messages
  addressed to *topics* (hierarchical strings like `muehle/hf/rotator/state`) and subscribe to
  topic filters.
- **Retained message**: an MQTT message the broker stores. The broker re-delivers it to every
  future subscriber of that topic until a newer message replaces it. It acts as "the last
  known value".
- **QoS**: MQTT delivery level. QoS 0 = at-most-once, QoS 1 = at-least-once.
- **LWT (Last Will and Testament)**: a message the broker publishes on a client's behalf if
  that client disconnects *uncleanly* (crash, network loss). The station uses it as the
  process-liveness flag.
- **Slot / plane**: the station models every device as a bus address
  `<site>/<station>/<slot>` (here `muehle/hf/rotator`) with four parallel *planes*: `/meta`
  (identity/capabilities), `/state` (live data), `/status` (process liveness), `/cmd`
  (inbound commands). See `02-interface-spec.md`.
- **Bridge**: a daemon that translates between one physical device and the bus. This
  component is the bridge for the `rotator` slot.
- **WRC**: "Web Remote Control", a third-party rotator controller (by radio amateur AF6SA)
  wired to the rotator's motor and sensors. It is the *upstream device* this bridge talks to.
  It exposes a WebSocket server for monitoring and steering.
- **WebSocket**: a standard protocol that upgrades an HTTP connection into a bidirectional
  message stream.
- **GS-232B**: a classic ASCII command protocol for Yaesu rotator controllers (commands like
  `C`, `M180`, `S`). Most legacy rotator-control software speaks it.
- **PSTRotator**: a popular Windows rotator-control program. It steers rotators with short
  XML-style datagrams over UDP.
- **shari**: the Raspberry Pi single-board computer (192.168.1.139) that runs all station
  services.
- **Service supervisor**: the host's service manager. It starts the daemon at boot, restarts
  it on failure, and delivers the shutdown/stop signal. This is a stack-agnostic term. In the
  reference implementation the supervisor is systemd (see §7 reference-implementation notes).

---

## 1. Role and context

The bridge fills the slot `muehle/hf/rotator`. It is a **read-write** slot. The bridge
publishes rotator status and accepts motion commands. It replaced an older third-party bridge
("rotint"). rotint used a different topic tree (`rotor2mqtt/…`) and carried its MQTT password
on the process command line. See §9 for the one-time migration.

The HF rotator turns the antenna horizontally (azimuth-only). It does not tilt the antenna.
The separate UHF antenna positioner is a *different* component (see
`03-components/pelcobridge2.md`, slot `muehle/uhf/rotator`). Do not conflate the two.

The bridge's four responsibilities:

1. Connect to the WRC's WebSocket (default endpoint `ws://192.168.1.108/wsrotor`). The WRC
   streams the rotator's status continuously as JSON frames. The bridge republishes that
   status — deduplicated — as a single retained JSON document on `muehle/hf/rotator/state`.
2. Accept commands on `muehle/hf/rotator/cmd` (`set_az`, `stop`, `fwd`, `rev`) and forward
   them to the WRC over the same WebSocket.
3. Run a GS-232B TCP server on port 7373 and a PSTRotator UDP listener on port 12040 (for
   legacy desktop software, enabled by default). Motion from either legacy path still
   surfaces in `/state`. All three paths funnel into the same serialized device-write path.
4. Publish a retained `/meta` "birth certificate" that describes identity and capabilities.
   Publish a retained `/status` liveness flag backed by an MQTT Last Will.

Whether a re-implementation must keep the two legacy paths (item 3) is an **open decision** —
see §11.

## 2. Upstream interface — the WRC WebSocket

### 2.1 Transport and connection parameters

**Transport**: plain (unencrypted, unauthenticated) WebSocket to a configurable URL (config
key `rotor.url`, default `ws://192.168.1.108/wsrotor`). The bridge acts as the WebSocket
*client*. There is no HTTP header customization, no subprotocol negotiation, no
authentication, and no application-level handshake. Once the socket is open, the WRC pushes
JSON status frames. The bridge can push JSON command frames at any time.

Normative connection requirements:

- The WebSocket handshake must time out after **10 seconds** if the WRC does not complete the
  upgrade.
- A shutdown signal received during the handshake must stop the dial.
- The bridge must not need any frame from the WRC before it sends its first command frame.

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

| Key | JSON type | Needed | Meaning |
|---|---|---|---|
| `state` | string | yes | controller state string. Observed values: `rotating` while turning. `stopped` or `idle` at rest. |
| `name` | string | no | rotor label. The bridge parses it but does not use it. |
| `az` | number | yes | current azimuth in degrees. |
| `lim1` | number | no | counter-clockwise rotation limit in degrees. The bridge parses it but does not use it. |
| `lim2` | number | no | clockwise rotation limit in degrees. The bridge parses it but does not use it. |
| `tdeg` | number | no | commanded target azimuth in degrees. |
| `fmsg` | string | no | fault message. |

The WRC streams these frames continuously (even when the antenna is stationary). The firmware
sets the exact cadence. The bridge must tolerate a continuous frame rate and must deduplicate
before it publishes (§3.3). The bridge must skip frames with unparsable JSON and log a
warning. It must not close the connection.

### 2.3 Upstream frames (bridge → WRC): command documents

Every command frame is a single JSON object with exactly one key, `az`. Its value is **always
a string**:

| Command | Wire frame | Meaning |
|---|---|---|
| Rotate to azimuth | `{"az":"180"}` | Rotate to 180°. |
| Stop | `{"az":"stop"}` | Stop motion immediately. |
| Jog clockwise | `{"az":"fwd"}` | Continuous CW rotation. It runs until a stop or a limit. |
| Jog counter-clockwise | `{"az":"rev"}` | Continuous CCW rotation. It runs until a stop or a limit. |

**NORMATIVE — string quoting**: the bridge must send the azimuth number as a quoted string
that holds an integer. The string has no decimal point and no exponent (for example `"180"`,
never `180` and never `"180.0"`). The controller firmware ignores numeric JSON values. A send
of `{"az":180}` is a silent no-op. This is the most easily missed requirement of the upstream
protocol.

The bridge must serialize all writes to the WRC: at most one in-flight command frame at a
time across all inbound paths (MQTT, GS-232B, PSTRotator). Commands from different paths must
not interleave on the wire.

### 2.4 Canonicalization rules

- The bridge derives `moving` from the raw `state` string. It lower-cases the string.
  `moving` is `true` if and only if the string contains the substring `rotat` OR the substring
  `moving`. An empty state string gives `false`. (The rule is deliberately fuzzy, so renamed
  firmware strings still map. See §10 for the downside.)
- The bridge takes `target_az` from `tdeg` **only when `tdeg != 0`**. A commanded target of
  exactly 0° (north) is therefore unrepresentable, and the bridge omits it. See §10.
- The bridge preserves the raw `state` string in the published field `rotor_state` for
  diagnostics.
- `fmsg` maps to the published field `error`.
- The bridge keeps the last known `az` across a reconnect. The bridge never zeroes the cached
  azimuth while the process lives.

### 2.5 Connection-loss detection

The read loop exits on any read failure. Loss of the WRC link therefore shows up only as a
WebSocket read error, or when process shutdown closes the socket. The reference
implementation has no ping/pong keepalive and no idle/read timeout. On loss, the bridge marks
the device offline (§5.2) and retries with backoff.

## 3. MQTT presence

MQTT 3.1.1 over plain TCP. Default broker: `tcp://192.168.1.50:1883` (the shack broker). A
planned migration to a broker on shari (192.168.1.139) exists. The team had not deployed it
when the authors wrote this PRD — see `00-system-overview.md`. Username `hf` (used only if configured
non-empty). The password comes through an environment variable, never from the config file.
Default client ID: `muehle-hf-rotator`.

Normative MQTT-session requirements:

- The bridge must register a Last Will at connect time. Topic `<slot>/status`, payload
  `offline`, QoS 1, retained.
- The bridge must auto-reconnect to the broker. It retries every **5 seconds** during a
  broker outage.
- The shutdown signal must interrupt the first broker connect. A shutdown during a hanging
  connect must end in process exit within the 1-second shutdown-latency bound of §5.1. The
  process must not wait for TCP timeouts.
- On every (re)connect the bridge must: publish `online` to `/status` (retained), republish
  `/meta` (retained), and resubscribe `/cmd` at QoS 1.
- The bridge publishes retained topics at QoS 1. It subscribes inbound `/cmd` at QoS 1.

### 3.1 Topics (exact strings, defaults)

| Topic | Direction | Retained | QoS | Cadence |
|---|---|---|---|---|
| `muehle/hf/rotator/meta` | publish | yes | 1 | on every MQTT (re)connect. |
| `muehle/hf/rotator/state` | publish | yes | 1 | only when a published field changes (§3.3). |
| `muehle/hf/rotator/status` | publish (LWT + on connect) | yes | 1 | `online` on every (re)connect. `offline` comes from the broker Last Will on unclean death. |
| `muehle/hf/rotator/cmd` | subscribe | no | 1 | resubscribed on every (re)connect. |

The bridge builds topic strings as `<site>/<station>/<slot>/<plane>` from config (`site=muehle`,
`station=hf`, `slot=rotator` by default).

Publishers do not retain `/cmd`. A rotator move is one-shot intent. A stale azimuth replayed
after a restart can spin the antenna unexpectedly. There is NO desired-state reconciliation
loop. Consumers observe the result in `/state`.

### 3.2 `/meta` payload (retained birth certificate)

The bridge publishes it identically on every MQTT connect. Exact JSON (defaults shown.
`host`, `location`, `device.model`, `link` come from config):

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

- `role` must be the canonical slot role `rotator` — never a device name or model.
- `capabilities.axes` must be `["az"]` (azimuth-only, hard-coded).
- The `expose` block is a consumer-neutral field/action surface. The separate discovery
  component (`03-components/hadiscovery.md`) reads it programmatically to render
  home-automation entities. **This bridge itself must not publish anything under
  `homeassistant/…`.**
- The `expose` record advertises the values `min:0, max:450, step:1` on the `az` field as
  metadata only. The bridge does NOT clamp or validate incoming azimuths against them (§10).
  A consumer must not
  rely on the bridge enforcing this range.
- The bridge publishes no `area` field. The discovery component supplies a deployment-wide
  default area.
- In the reference implementation the `expose.device.name` value (`"HF Rotator"`) and the
  `manufacturer` value (`"Yaesu"`) are hard-coded rather than configurable. Everything else
  in `/meta` comes from config.

### 3.3 `/state` payload (retained live snapshot)

The bridge publishes a single JSON document — and publishes it **only when a published field
changes**:

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
| `ts` | string | — | always | RFC 3339, UTC. The time of *this publish*. (The WRC frame carries no timestamp.) |
| `az` | number | degrees | always | current azimuth. The raw WRC value passes through. |
| `target_az` | number | degrees | omitted when absent/zero | the commanded target from WRC `tdeg`. |
| `moving` | boolean | — | always | derived from `rotor_state` per §2.4. |
| `rotor_state` | string | — | omitted when empty | the raw WRC state string. |
| `device_online` | boolean | — | always | `true` iff the WRC WebSocket is up. `false` when the bridge process lives but the WRC link is down. |
| `error` | string | — | omitted when empty | human-readable fault. WRC `fmsg` when connected. `wrc: <error text>` when the link drops. |

Publish triggers, exactly. (a) A parsed WRC status frame differs from the last published
snapshot in any of the six fields `az`, `target_az`, `moving`, `rotor_state`, `device_online`,
`error`. (b) The WebSocket drops or a reconnect try changes `device_online` or `error`.

A rotator `/state` document must never carry `freq_hz`, `band`, or `mode`. Those are radio
concerns that belong to other slots (see `02-interface-spec.md`).

The bridge publishes `device_online` as an explicit boolean. This matches the deployed form.
(The integration model's phrase "omitted when true" conflicts with deployed behavior across
all bridges. Consumers must treat both forms as the same — see `02-interface-spec.md`
§5 and §11 below.)

### 3.4 `/status` (process liveness)

Plain string payload (not JSON): exactly `online` or `offline`, retained, QoS 1. The bridge
publishes `online` on every MQTT (re)connect. `offline` rides in the MQTT Last Will (§3), so
the broker publishes it only if the bridge dies uncleanly.

**Two-layer liveness is normative.** `/status` reflects the **bridge process only**. If the
WRC vanishes while the bridge runs, `/status` stays `online` and `/state.device_online` flips
to `false`. Consumers MUST AND the two signals. Conversely, the bridge must not report
bridge-process death in `/state`. The bridge must not flip `/status` offline when only the
WRC link drops.

**Actual behavior, not idealized.** On a CLEAN process shutdown the broker does not fire the
Last Will, and the reference implementation never publishes `offline` itself. After a clean
stop the retained `/status` can stay `online` indefinitely until the process next appears.
Consumers MUST NOT trust `/status` alone. (The component's API doc claims the code publishes
`offline` on clean shutdown. The code does not do this. This is a known defect/discrepancy —
§10.)

## 4. Command surface

### 4.1 MQTT `/cmd`

Not retained, QoS 1. Each payload is one JSON object:

| Action | Payload | Behavior |
|---|---|---|
| `set_az` | `{"action":"set_az","az":180}` | Rotate to 180°. Forwards `{"az":"180"}` to the WRC. `az` is a JSON number (any float accepted, no range check). |
| `stop` | `{"action":"stop"}` | Stop motion. Forwards `{"az":"stop"}`. |
| `fwd` | `{"action":"fwd"}` | Continuous jog clockwise. Forwards `{"az":"fwd"}`. Runs until a limit or a `stop`. |
| `rev` | `{"action":"rev"}` | Continuous jog counter-clockwise. Forwards `{"az":"rev"}`. |

Rules:

- **The argument key for `set_az` is `az`**, not the station's generic `value` key (see §11).
  The `/meta` `expose` field descriptor gives the authoritative machine-readable declaration
  (`command.value_key: "az"`). Consumers that read `/meta` need no special case.
- The bridge logs (warning) and ignores unknown actions, `set_az` with a missing `az`, and
  malformed JSON.
- No acknowledgment or reply topic exists. A consumer watches `/state` to see success:
  `moving` → `true`, `az` → target, `target_az` → commanded value.
- A deployment can omit the device writer (read-only mode). The bridge then logs and ignores
  any command.

**Dispatch pipeline (normative).** The MQTT receive-path handler must only enqueue the raw
payload onto a bounded queue of capacity **8**. A single worker must parse and execute the
commands one at a time, strictly FIFO. When the queue is full, the worker drops the excess
command with a warning log. The MQTT receive path must NEVER block on device I/O. (Blocking
the receive path deadlocks the MQTT client library. This constraint holds for any library —
see §8, invariant 5.)

**Failure mode.** If the WRC WebSocket is down when the worker dequeues a command, the write
fails. The bridge logs the failure and **discards the command — no retry, no pending queue
across reconnect**. A `stop` issued during a WRC outage is therefore lost, and the antenna
keeps turning per the controller's own last command. Changing this is a PRD-level decision
(§11).

### 4.2 GS-232B TCP server (legacy path, not mandatory. Default enabled on `0.0.0.0:7373`)

One command per line. The line ends with `\r` or `\n` (either alone). The bridge trims the
input and upper-cases it before matching.

| Line in | Behavior | Response |
|---|---|---|
| `C` or `C2` | query position | `+0aaa+0000\r` where `aaa` is the current azimuth zero-padded to 3 digits, truncated to integer (azimuth 180 → `+0180+0000\r`). The trailing `+0000` is a fixed dummy elevation. (Elevation is the antenna's vertical tilt angle. This azimuth-only rotator does not have one — §1.) |
| `M` + 1–3 digits (for example `M090`) | rotate to that azimuth | `\r` |
| `W` + 1–3 digits + whitespace + integer (for example `W180 000`) | rotate to that azimuth. The bridge parses and ignores the elevation argument | `\r` |
| `S` | stop | `\r` |
| anything else | — | `?>\r` |

Edge behaviors (normative. They match the deployed protocol as legacy clients observed it):

- The `M` and `W` matchers anchor **only at the start of the line**. They prefix-match and
  silently ignore any trailing characters after the captured digits.
  - For `M`: the matcher captures the **first 1–3 leading digits** after the letter and
    ignores everything after them. The bridge treats `M1234` as `M123` (ack `\r`, rotate to
    123°). `M12.5` and `M12X` likewise rotate to 12° and ack. Only `M` followed by **zero**
    digits gets **no response at all** — no `\r`, no error.
  - For `W`: the line must carry the 1–3 azimuth digits plus the whitespace-separated integer
    elevation argument (a `W` line that lacks either gets no response at all). The bridge
    ignores trailing characters after the elevation integer (`W180 000abc` → rotate to 180°,
    ack `\r`).
  - This lenient prefix matching is normative deployed behavior. A re-implementation with
    strict full-line matching (rejecting `M1234`) produces observably different wire behavior.
    Tightening is a behavior-change decision — see §11.
- The digit-less no-response case can hang naive line-oriented clients that wait for an
  acknowledgment — §10.
- The line reader stops at the first `\r` or `\n` and does not read a following `\n`. (A
  `\r\n` pair leaves the `\n` in place. The next read then sees a harmless empty line.)
- The azimuth used in `C` replies is the last WRC status frame. If the process has never
  reached the WRC since start, replies are `+0000+0000`.
- Commands funnel into the same serialized WRC write path as MQTT. Resulting motion surfaces
  in `/state`.

### 4.3 PSTRotator UDP listener (legacy path, not mandatory. Default enabled on `0.0.0.0:12040`)

Datagrams up to **1024 bytes**. The listener matches XML-style content case-insensitively
with tolerant (whitespace-flexible) pattern rules.

| Datagram content | Behavior | Response |
|---|---|---|
| contains `AZ?` (for example `<PST>AZ?</PST>`) | position query | `<PST><AZIMUTH>aaa</AZIMUTH></PST>` (integer azimuth) as a UDP datagram to the **source IP, port + 1** (12041 by default) — the PSTRotator reply convention. |
| contains `<STOP>…</STOP>` | stop motion | none. |
| contains `<PARK>…</PARK>` | logged, ignored (no park support) | none. |
| contains `<AZIMUTH>-?\d+(\.\d+)?</AZIMUTH>` (negatives and decimals allowed. For example `<PST><AZIMUTH>180</AZIMUTH><ELEVATION>45</ELEVATION></PST>` → 180, and the listener ignores elevation) | rotate to that azimuth | none. |
| anything else | logged | none. |

Precedence is normative: `AZ?` first, then `STOP`, then `PARK`, then `AZIMUTH` — so a datagram
that carries both `<STOP>` and `<AZIMUTH>` stops. All drive the same serialized WRC write
path.

## 5. Behavior and state machine

### 5.1 Startup (in order)

1. Load config: built-in defaults ← TOML file (flag `-config`, default path
   `/etc/wrc-rotator-bridge/config.toml`) ← environment overrides ← command-line flag
   overrides. A missing file is acceptable (defaults apply). Malformed config or a validation
   failure exits with code **2**.
2. Validation: `mqtt.site` and `mqtt.station` must be non-empty. `rotor.url` must be
   non-empty. `gs232.port` and `pstrotator.port` must be non-zero when the respective server runs. Violations exit 2.
3. Install a SIGINT/SIGTERM handler (cancellation).
4. Connect MQTT FIRST, with the Last Will registered. On connect: publish retained `online`
   to `/status`, publish retained `/meta`, subscribe `/cmd` (QoS 1). **MQTT connect failure at
   boot is fatal**: exit code 1. The service supervisor (the host's service manager that
   starts the daemon at boot, restarts it on failure, and delivers the shutdown/stop signal)
   restarts it after 5 s.
5. Start the GS-232B TCP server and the PSTRotator UDP listener. A listen failure (for
   example a port already taken) is only logged as an error — the process keeps running
   without that path. It runs without the path for its whole lifetime. Only a restart can
   retry it.
6. Enter the WRC WebSocket loop (§5.2). It runs until shutdown.

**Shutdown.** SIGTERM/SIGINT cancels the run. The bridge nudges the WRC read loop closed. The
MQTT client disconnects with a 500 ms quiesce. Exit code **0** (so a clean service stop
reports success). Any other runtime error exits 1. Exit codes are normative: 0 on
SIGTERM/SIGINT, 2 on config errors, 1 on runtime failure.

**Shutdown latency (normative bound).** A shutdown during a hanging first MQTT connect, the
WebSocket handshake, or a backoff sleep must end the process within **1 second**. The process must not wait for any network timeout or any supervisor kill
timeout. Test: issue the shutdown signal while the broker address is black-holed (and,
separately, during a backoff sleep). Assert process exit within 1 s.

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

Exact values (normative): first backoff **2 s**, growth factor **1.5×**, cap **60 s**
(sequence: 2 s, 3 s, 4.5 s, 6.75 s, … capped at 60 s). The backoff resets ONLY on process
restart. It does not reset after a successful connection. Repeated failures within one
process lifetime therefore drive the interval to the 60 s cap and keep it there (§10).

During all WRC retries the bridge stays connected to MQTT. `/status` stays `online` and
`/meta` stays retained. Only `/state.device_online` reflects the outage. The last known
azimuth survives reconnects (snapshots published during an outage keep the old `az`).

Between a successful re-dial and the first status frame, the bridge rebuilds the device's
cached state with only the old azimuth preserved (`moving=false`, empty `rotor_state`, empty
`target_az`). A published snapshot in that window can look "cleaner" than reality for one
frame (§10).

### 5.3 MQTT reconnect behavior

Auto-reconnect handles broker outages (retry every 5 s). On every (re)connect the bridge
publishes `online` to `/status`, republishes `/meta`, and resubscribes `/cmd` (QoS 1). The
bridge does not receive commands that others issue while the broker link is down (the bridge
retains nothing). The publisher silently loses publishes that run during a
disconnect — it never blocks on delivery and never retries. Retained documents self-heal on the next publish. The next
change after reconnect republishes the current truth.

### 5.4 Error-path summary

| Event | Visible effect | Recovery |
|---|---|---|
| WRC dial fails / WebSocket drops | `/state` gets `device_online:false` and `error:"wrc: …"`. `/status` stays unchanged | backoff retry loop §5.2. |
| WRC sends unparsable JSON | warning log. Skip the frame. Keep the connection. | none needed. |
| `/cmd` malformed / unknown action / `set_az` missing `az` / read-only deploy | warning log. Ignored. | — |
| `/cmd` while WRC down | warning log. Command dropped. | operator re-issues. |
| >8 commands queued | warning log. Command dropped. | operator re-issues. |
| GS-232 / PSTRotator listen fails | error log. That path stays off for the process lifetime. | process restart. |
| GS-232 client disconnects | connection closed. Logged. | — |
| MQTT connect fails at boot | process exit 1 | service supervisor restart, 5 s delay. |
| Bridge crash / broker link loss | broker fires Last Will → `/status` = `offline` (retained) | service supervisor restart. |

## 6. Configuration

Config file: TOML, default path `/etc/wrc-rotator-bridge/config.toml` (flag `-config
<path>`). Layering (normative): built-in defaults ← TOML ← environment ← flags. The bridge
ignores unknown TOML keys (not an error).

| Key | Default | Meaning |
|---|---|---|
| `host` | `shari` | compute-node name. The bridge publishes it in `/meta.host`. |
| `rotor.url` | `ws://192.168.1.108/wsrotor` | WRC WebSocket endpoint (must stay non-empty). |
| `gs232.enabled` | `true` | run the GS-232B TCP server. |
| `gs232.bind` | `0.0.0.0` | GS-232 listen address. |
| `gs232.port` | `7373` | GS-232 listen port (non-zero when enabled). |
| `pstrotator.enabled` | `true` | run the PSTRotator UDP listener. |
| `pstrotator.bind` | `0.0.0.0` | PSTRotator listen address. |
| `pstrotator.port` | `12040` | PSTRotator listen port (non-zero when enabled). |
| `device.model` | `Yaesu G-450DC` | identity string in `/meta.device.model` and `expose.device.model`. |
| `device.link` | `ethernet` | transport label in `/meta.link`. |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | broker URI. |
| `mqtt.client_id` | `""` → `muehle-hf-rotator` | MQTT client ID. |
| `mqtt.user` | `hf` | MQTT username. |
| `mqtt.password` | `""` (overridden by env) | MQTT password — must stay out of the TOML. |
| `mqtt.site` / `mqtt.station` / `mqtt.slot` | `muehle` / `hf` / `rotator` | bus address. Site and station must stay non-empty. |
| `mqtt.location` | `bauwagen` | physical-location label in `/meta.location`. |
| `mqtt.discovery_prefix` | `homeassistant` | legacy key. The bridge parses it but wires it to nothing. |
| `mqtt.publish_ha_discovery` | `false` | legacy key. The bridge parses it but wires it to nothing. |
| `log.level` | `info` | log level: `debug` \| `info` \| `warn` \| `error`. |

Flags: `-config <path>`, `-log.level <level>` (overrides config), `-debug` (log raw WRC
WebSocket frames in both directions).

Environment overrides (all prefixed `WRC_ROTATOR_BRIDGE_`): `_MQTT_BROKER`, `_MQTT_CLIENT_ID`,
`_MQTT_USER`, `_MQTT_PASSWORD` (the secret), `_MQTT_SITE`, `_MQTT_STATION`, `_MQTT_SLOT`,
`_ROTOR_URL`. A non-empty environment value replaces the TOML value.

**Secrets (normative).** The MQTT password lives only in a 0600 environment file
(`/etc/wrc-rotator-bridge/wrc-rotator-bridge.env`, owned by the service user) as
`WRC_ROTATOR_BRIDGE_MQTT_PASSWORD="…"`, loaded by the service supervisor. It MUST NOT appear
in the TOML, the unit definition, or the process command line. See
`docs/conventions/config-and-secrets.md` (as reconstructed in `05-deployment-ops.md`).

## 7. Deployment

- Target host: the Raspberry Pi `shari` at 192.168.1.139.
- The deploy procedure. Cross-compile a static binary for the Pi. Generate the service unit,
  a seed config.toml, and a seed env file (mode 0600, password in a separate temp file).
  Copy them to the Pi. Then over SSH: create a dedicated system service user (no home,
  nologin) if missing. Install the binary under `/opt/wrc-rotator-bridge/`. **Seed-once** —
  install `/etc/wrc-rotator-bridge/config.toml` (0600, service-user owned) and the env file
  only if they do not already exist. Stop the service. Move binary + unit into place. Reload
  the service supervisor. Enable, restart, print status.
- **Seed-once is normative.** After first install the on-device config and env file belong to
  the deployment. A redeploy MUST never overwrite them.
- Service requirements: the service runs as the dedicated unprivileged user. It restarts on
  failure with a 5 s delay. It starts after network availability. Logs go to the system
  journal with identifier `wrc-rotator-bridge`.
- Hardening requirements (the service needs only network access — two inbound listen sockets
  plus outbound TCP). The unit gets no-new-privileges. It sees a read-only filesystem, except
  one writable state directory (`/var/lib/wrc-rotator-bridge`). /tmp stays private. The unit
  gets no device access beyond pseudo-devices. The unit gets no namespaces and no real-time
  scheduling.
  - The unit protects kernel tunables, modules, and cgroups. It restricts address families to
    `AF_INET`/`AF_INET6`. It has private /tmp. It has no SUID/SGID and no IPC.
  - Capability bounding and ambient sets stay empty. The memory limit is 256 MB. The task
    limit is 64.
- **One-time migration from the legacy `rotint` bridge** (run before the first deploy of this
  component, only on a Pi that ran rotint before). On the Pi: create the new service user and
  config directory. Extract the MQTT password from the old service unit's command line **on
  the device**. Write it into the new env file (0600). This repairs the old bridge's
  command-line secret exposure. Stop and disable the old service, and remove its unit. Leave
  the old bridge's files and its user untouched. The step is idempotent. It backs up any
  pre-existing env file to `*.pre-migration`. The old rotint bridge published a different
  topic tree (`rotor2mqtt/…`) plus embedded home-assistant discovery. This component replaces
  it entirely.

Reference-implementation notes (non-normative): the original is a Go daemon supervised by
systemd (`Type=simple`, unit `wrc-rotator-bridge.service`,
`ExecStart=/opt/wrc-rotator-bridge/wrc-rotator-bridge -config /etc/wrc-rotator-bridge/config.toml`,
`EnvironmentFile=/etc/wrc-rotator-bridge/wrc-rotator-bridge.env`, `Restart=on-failure`,
`RestartSec=5`, `After=network-online.target`), deployed by `deploy.sh` cross-compiling with
`GOOS=linux GOARCH=arm64 CGO_ENABLED=0` and stripped symbols. Dependencies:
`gorilla/websocket` v1.5.3, `eclipse/paho.mqtt.golang` v1.5.1, `BurntSushi/toml` v1.6.0, and
the shared in-repo module for topic builders and context-aware MQTT connect. Any stack that
satisfies the normative requirements above is fine.

## 8. Invariants (normative requirements)

1. **No one treats `/cmd` as retained state.** The bridge stores no desired azimuth, and it
   re-applies none on any restart or reconnect. A move is one-shot intent. A re-implementation
   must NOT add desired-state reconciliation that replays the last commanded azimuth.
2. **Two-layer liveness stays two-layer.** `/status` reflects the bridge process only.
   WRC-link state lives exclusively in `/state.device_online`. Never flip `/status` offline
   on a WRC drop. Never report bridge death in `/state`.
3. **`/state` and `/meta` are single retained JSON documents. `/status` is a plain
   `online`/`offline` string.** No per-field topics. The bridge publishes no
   `homeassistant/…` topics.
4. **The bridge serializes all WRC writes** (one in-flight frame at a time across MQTT,
   GS-232B, and PSTRotator paths). **The bridge sends absolute azimuths as quoted strings**
   (`{"az":"180"}`), never JSON numbers — the controller ignores numeric values.
5. **The MQTT receive path must never block on device I/O.** The bridge enqueues incoming
   commands to a bounded queue (capacity 8, drop-on-overflow). A single worker executes the
   commands. Rationale (library-independent): in the reference MQTT library, handlers run on
   the connection's dispatch thread — a handler that blocks or publishes synchronously
   deadlocks the client. This class of failure has occurred live elsewhere in this station.
   Any re-implementation must isolate handler work from the receive path regardless of
   library.
6. **Exit codes: 0 on SIGTERM/SIGINT, 2 on config errors, 1 on runtime failure**. A clean
   shutdown must not look like a crash to the service supervisor.
7. **No secrets in the config file, unit definition, or command line** — only in the 0600
   environment file. The deploy must never overwrite the on-device env file after first
   install.
8. **A rotator never publishes radio fields** (`freq_hz`, `band`, `mode`).
9. **`stop` takes precedence over an embedded azimuth within a single PSTRotator datagram**.
   FIFO arrival at the serialized write path resolves a stop that races a separate set
   command.
10. **Shutdown interruptibility**. SIGTERM during a hanging first MQTT connect, the
    WebSocket handshake, or a backoff sleep must end in process exit within **1 second**
    (the §5.1 bound). Do not rely on service-supervisor kill timeouts.

## 9. Relationship to the rest of the system

- The rotator is a standalone read-write slot. No other component commands it automatically
  today. The console UI (`04-console.md`) lets the operator steer it. Any future automation
  can command it like any consumer (through `/cmd`. The consumer takes `value_key` from
  `/meta.expose`).
- The discovery component (`03-components/hadiscovery.md`) renders home-automation entities
  from `/meta.expose`. This bridge carries no home-automation vocabulary itself.
- `02-interface-spec.md` §5 defines the liveness conventions that all bridges share
  (including the clean-shutdown `/status` caveat), in one place.

## 10. Known defects and fragilities (as observed. A re-implementation must decide per §11 whether to fix)

- **`target_az` of 0 is unrepresentable.** The bridge sets `target_az` only when `tdeg != 0`,
  so it drops a commanded target of exactly 0° (north). The design conflates the notions
  "absent" and "zero".
- **No watchdog on the WRC stream.** There is no read/idle timeout. If the WRC TCP connection
  hangs without closing (half-open, silent device), the bridge reports `device_online:true`
  forever with a frozen azimuth. Only a socket error triggers recovery.
- **The bridge silently drops commands when the WRC is down — including `stop`.** No
  pending-command queue exists for reconnects. A stop issued during an outage never reaches
  the device, and the antenna keeps turning per the controller's own last command.
- **No azimuth range validation anywhere.** The bridge forwards MQTT `set_az` (any float),
  GS-232 (up to `M999`), and PSTRotator (arbitrary decimals and negatives) verbatim. `/meta`
  advertises `min:0 max:450` but nothing enforces it. Out-of-range handling belongs entirely
  to WRC firmware.
- **Doc/code discrepancy on clean-shutdown `offline`.** The component API doc claims the code
  publishes `/status` `offline` on clean shutdown. The code never publishes `offline`, and a
  graceful disconnect fires no Last Will — retained `/status` stays `online` after a clean
  stop (§3.4).
- **Publish errors are invisible.** The publisher fires-and-forgets MQTT publishes (never
  waits on delivery tokens). A failed `/state` publish is silently lost (self-heals only on
  the next change).
- **GS-232 `M`/`W` lenient prefix matching.** The matchers capture only the first 1–3 digits
  and ignore trailing characters (§4.2). So the bridge turns the antenna to 123° for
  `M1234` and gives no rejection. A client that sends a malformed long azimuth then gets an
  ack for a truncated value. Only a digit-less `M` (or a `W` missing its whitespace+integer elevation
  part) gets no reply at all, which can hang naive line-oriented clients that wait for an
  acknowledgment. Tightening to strict full-line matching is a behavior change that needs
  PRD-level sign-off (§11).
- **`moving` detection matches substrings** (`rotat`/`moving`). The rule survives firmware
  renames. But the bridge misclassifies a future state string that merely *contains* those
  substrings (for example `pre-rotating-check`) as moving.
- **Backoff never resets on success** within a process lifetime. Alternating success and
  long failure drives the retry interval to the 60 s cap and keeps it there until restart.
- **Reconnect transient flattening.** Between a successful re-dial and the first status
  frame, published state shows `moving:false`, empty `rotor_state`, empty `target_az` (old
  azimuth preserved). One frame can look "cleaner" than reality.
- **Dead config keys.** The bridge parses `mqtt.publish_ha_discovery` and
  `mqtt.discovery_prefix` but wires them to nothing (embedded home-assistant discovery does
  not exist in this component) — leftovers of the rotint migration.
- **Hard-coded identity strings.** `expose.device.name` ("HF Rotator") and `manufacturer`
  ("Yaesu") are binary constants, not config (minor). Everything else in `/meta` comes from
  config.

## 11. Open decisions and unresolved facts

1. **Must a re-implementation keep the two legacy inbound paths**. The deployed system turns
   the GS-232B TCP server (:7373) and the PSTRotator UDP listener (:12040) on by default. At
   least one of them exists because desktop rotator software in the shack drives the rotator
   through it (the sources do not record which one). Evidence: both come default-on in code
   and config. §4.2–4.3 give the details in full. However, the station bus (the console sends
   /cmd) can steer the same rotator. The legacy paths expose unauthenticated plaintext network
   listeners — a security-relevant surface. The decision is open. (a) Keep both,
   byte-compatible. (b) Keep one (which?). (c) Drop both and go bus-only. If the team drops
   both, the exact GS-232B/PSTRotator protocol details in §4.2–4.3 become informative only.
   The PRD reconstruction must treat "at least one legacy path stays reachable for desktop
   software" as the likely requirement. Pending confirmation: which software operators
   actually use.
2. **`set_az` argument key `az` vs the station-wide `value` convention.** The station-wide
   `/cmd` convention (see `02-interface-spec.md`) puts every command argument under the
   generic key `value` as a string, with one documented exception (the ultrabeam controller's
   frequency). This bridge instead uses a field-specific key `az` that carries a JSON
   *number* — it predates the convention. The `/meta.expose` descriptor (`value_key:"az"`,
   `value_type:"float"`) is the authoritative machine-readable declaration, so well-behaved
   consumers need no special case. Open decision for a re-implementation: preserve the
   deployed `az`-key grammar for compatibility with existing consumers, or migrate to
   `value` (which can break a consumer that hard-codes `az`). Do not change it silently.
3. **`device_online` form.** This bridge publishes the key/value `device_online:true`
   explicitly. The integration-model text says "omitted when true". Consumers must treat both
   forms as the same (absence = true). A re-implementation must pick one and align with the
   system-wide decision recorded in `02-interface-spec.md` §5.
4. **Whether to fix the known defects of §10** is a set of deliberate behavior changes, not
   bug fixes. The reference team flagged each of these as needing PRD-level sign-off before
   "fixing". The list: adding `/cmd` retention or desired-state reconciliation (invariant 1
   forbids it as built). Merging device liveness into `/status` (invariant 2 forbids it).
   Clamping/validating azimuths. Retrying or queueing commands across WRC outages (including
   `stop`). Resetting backoff on success. Adding a stream watchdog. Each fixes a real
   fragility (a lost `stop` during an outage is safety-relevant — the antenna keeps turning)
   but changes observable behavior. The reconstructing team must decide each explicitly and
   record the decision. In particular, the missing stream watchdog and the dropped-`stop`
   behavior need safety review against `06-safety.md`.
5. **Clean-shutdown `/status`.** The component API doc and the code disagree. The doc claims
   a clean shutdown publishes `offline`. The code does not. Retained `online` persists after
   a clean stop. §3.4 documents the code-derived behavior. The open decision. A
   re-implementation must either explicitly publish `offline` before a graceful disconnect
   (which makes the doc true) or keep the current behavior and rely on consumers treating
   `/status` as advisory. System-wide, consumers must not trust `/status` alone either way.
6. **WRC endpoint details.** The reference code and live observation gave the WRC WebSocket
   URL (`ws://192.168.1.108/wsrotor`) and its JSON frame vocabulary. No vendor documentation
   exists in this repo. The exact frame cadence, the meaning of all `state` strings beyond
   `rotating`/`stopped`/`idle`, and the `name`/`lim1`/`lim2` semantics are firmware-set and
   unverified. Code confirms the quoted-string requirement for azimuths
   (`strconv.FormatFloat(az,'f',0,64)`), but its firmware rationale (why the firmware ignores
   numbers) is unknown.
7. **Which legacy desktop software actually uses the GS-232B / PSTRotator paths** in the
   Mühle shack the sources do not record. This feeds decision 1.
8. **GS-232B line-matching strictness.** The deployed matchers prefix-match and ignore
   trailing characters after the captured digits (`M1234` → rotate to 123°, ack, §4.2). §4.2
   documents this as normative deployed behavior. Open decision: keep the lenient prefix
   matching byte-compatible. Or tighten to strict full-line matching (rejecting `M1234` with
   `?>` or no reply). That is a deliberate behavior change that alters what legacy clients
   observe on the wire. Do not change silently.

Reference-implementation notes (non-normative): the original is a Go daemon (module
`wrc-rotator-bridge`, Go 1.26) with packages `cmd/wrc-rotator-bridge` (wiring, restart loop,
bounded command queue), `internal/rotor` (WRC protocol structs, dial with 10 s handshake
timeout, read loop, mutex-guarded writes), `internal/bridge` (state model, `/meta`,
change-dedup, `/cmd` dispatch), `internal/gs232`, `internal/pstrotator`, `internal/config`.
Free-to-change implementation details: language and libraries, package layout, and the
mutex/channel plumbing (any serialization that meets invariants 4–5). The line-reader details
beyond the `\r`/`\n` tolerance rules are free too. So are log wording and destination. A
re-implementation can simply drop the dead discovery config keys
`mqtt.publish_ha_discovery` and `mqtt.discovery_prefix`.