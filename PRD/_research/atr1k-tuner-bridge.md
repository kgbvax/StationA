# Research spec — atr1k-tuner-bridge (ATR-1000 ATU → MQTT bridge)

Source analyzed: `/Users/ingomar.otter/dev/stationa/atr1k-tuner-bridge` (Go), plus the
shared modules it imports (`shared/schema`, `shared/mqtt`), its tests, `deploy.sh`, and
the git history of the live `/cmd` convention fix (commit `3dfe067`, 2026-07-12).
This document is the behavioral contract a re-implementation in any technology stack
must reproduce. Code is truth; README/doc disagreements are called out explicitly.

---

## 0. Glossary (plain-language definitions)

- **Ham radio / HF**: amateur radio; HF ("high frequency") is the 3–30 MHz shortwave
  spectrum used for long-distance communication.
- **ATU / "antenna tuner"**: an *antenna tuner unit* — a box of switchable inductors
  (measured in microhenry, µH) and capacitors (picofarad, pF) inserted in the
  transmitter's feedline that electrically matches an imperfect antenna to the
  transmitter so power is not reflected back.
- **SWR**: *standing-wave ratio*, a dimensionless number ≥ 1.0 measuring impedance
  mismatch; 1.0 is perfect, above ~3 is bad. Often expressed as raw×100 on the wire.
- **Forward power (fwd)**: watts flowing from transmitter toward the antenna.
- **In-line vs bypass**: an ATU relay pair either inserts the matching network into
  the RF path ("in line") or shorts past it ("bypass"). A bypassed tuner is inert.
- **Tune cycle**: the ATU's relay search that finds an L/C combination matching the
  antenna at the current frequency. "Mem" recalls a stored solution; "full" searches.
  **Settling** = a tune cycle is in progress (relays are moving; transmit should pause).
- **MQTT**: a lightweight publish/subscribe protocol; clients publish messages to
  hierarchical topic strings, subscribers receive them via a central **broker**.
- **Retained message**: an MQTT message the broker stores and re-delivers to any
  future subscriber ("late subscriber") — used so a new consumer immediately sees the
  last state.
- **LWT (Last Will and Testament)**: a message the broker publishes on the client's
  behalf if the client disconnects ungracefully — here `offline` on `/status`.
- **QoS**: MQTT delivery guarantee level; QoS 0 = at most once, QoS 1 = at least once.
- **WebSocket**: a TCP protocol providing a full-duplex message channel over an HTTP
  upgrade; "binary WebSocket" = messages carry raw bytes, not text.
- **Slot**: the station model's fixed MQTT address `<site>/<station>/<slot>` (default
  `muehle/hf/tuner`); the four planes `meta`, `state`, `status`, `cmd` hang off it.
- **Bridge/adapter**: this service; a thin translator between one vendor device's
  proprietary protocol and the station's canonical MQTT contract.
- **shari**: the Raspberry Pi (192.168.1.139) at the site that runs all station services.
- **antennaselect**: a sibling logic service (slot `muehle/hf/antenna-select`) that
  decides which antenna/routing to use per band and drives this tuner's `set_inline`.

---

## 1. Purpose & role

atr1k-tuner-bridge is a long-running daemon that connects an **ATR-1000 automatic
antenna tuner** (of the BTR-1000 / N7DDD device family, reached over Wi-Fi via its
built-in binary WebSocket server) to the station's MQTT bus. It:

1. Streams telemetry out of the tuner — SWR, forward power, and the inductor/
   capacitor relay values — plus the tuner's in-line/bypass position and tune-cycle
   status, published as one retained JSON snapshot.
2. Accepts two commands over MQTT — put the tuner in line or bypass, and start a
   tune cycle (memory-recall or full search) — and writes them to the tuner.

It is **read-write**. It implements the station slot `muehle/hf/tuner`. It does not
transmit or drive the radio; the antennaselect reconciler drives `set_inline`
automatically for non-resonant bands (it publishes `set_inline=true` when the
fan-dipole antenna is selected on 30/60/160 m, `false` otherwise). The station's
`hadiscovery` consumer renders Home Assistant entities from this bridge's `expose`
block; the bridge itself carries no Home Assistant vocabulary.

Place in the station: one of ~13 thin adapters that each front one physical or logical
device at a fixed slot address, all talking to one MQTT broker at 192.168.1.50:1883.

---

## 2. Upstream interface — the ATR-1000 binary WebSocket

### 2.1 Transport & connection

- **Transport**: outbound WebSocket (`ws://`, plain — no TLS) to the tuner, default
  endpoint `ws://192.168.1.20:60001` (TCP port 60001). The tuner is the server; the
  bridge always dials out. No handshake beyond the HTTP WebSocket upgrade — no
  authentication, no hello message.
- **Handshake timeout**: 10 s (`websocket.Dialer{HandshakeTimeout: 10 * time.Second}`).
  The dial itself is context-cancellable (SIGTERM interrupts it).
- **Message type**: WebSocket **binary** messages; each message is exactly one frame.
- **Connection-loss detection**: a read error from the WebSocket read loop (any error
  from `ReadMessage`, including clean close). There is **no ping/pong keepalive and
  no idle-read timeout** — a silently dead TCP link is detected only when the OS
  errors the socket (or when a write fails).

### 2.2 Frame format (reverse-engineered protocol)

```
byte 0: 0xFF   flag — every frame, inbound and outbound, starts with 0xFF
byte 1: cmd    command/type byte
byte 2: len   payload length in bytes (single byte)
byte 3..: payload  — multi-byte integers inside the payload are little-endian uint16
```

The length byte is **written but not validated on receipt**: the decoder takes the
payload as `data[3:]` regardless of byte 2. A frame is rejected only if the total
message is shorter than 3 bytes or byte 0 ≠ 0xFF.

Command bytes:

| cmd | Name | Direction | Payload | Meaning |
|-----|------|-----------|---------|---------|
| 1 | Sync | bridge → tuner | none (frame is exactly `FF 01 00`, 3 bytes) | request a full state snapshot from the tuner |
| 2 | Meter | tuner → bridge | SWR raw uint16 LE at offsets 4–5 (frame-relative), forward watts uint16 LE at offsets 6–7 | telemetry; frame must be ≥ 8 bytes long to decode |
| 3 | TuneStatus | bridge → tuner | 1 byte: `0` = bypass, `1` = in line | put the tuner in line / bypass (frame `FF 03 01 00` or `FF 03 01 01`) |
| 4 | TuneMode | bridge → tuner | 1 byte: `0` = Reset, `1` = Mem, `2` = Full, `3` = Fine | start a tune cycle; the bridge only ever sends `1` (mem) or `2` (full) — frame `FF 04 01 01` or `FF 04 01 02` |
| 5 | Relay | tuner → bridge | inductance raw uint16 LE at offsets 6–7, capacitance uint16 LE at offsets 8–10 (pF) | L/C relay telemetry; frame must be ≥ 10 bytes long to decode |

Meter payload decoding (offsets are indices in the whole frame, byte 0 = 0xFF):

- `swr_raw = uint16LE(frame[4:6])`. If `swr_raw >= 100`, published SWR = `swr_raw / 100`
  (example: raw 217 → 2.17). If `swr_raw < 100` it is published **as-is** (not divided —
  see Known defects §9). In practice SWR ≥ 1.0 so raw ≥ 100 always.
- `fwd = uint16LE(frame[6:8])` — forward power in whole watts, published unchanged.

Relay payload decoding:

- `l_uh = uint16LE(frame[6:8]) / 100` — inductance in µH, two implied decimals
  (raw 1234 → 12.34 µH).
- `c_pf = uint16LE(frame[8:10])` — capacitance in pF, published unchanged (raw 56 → 56).

Inbound frames with other cmd bytes (including a tuner-echoed Sync/TuneStatus/
TuneMode), and all malformed frames, are silently discarded (no log, no state change).
Bytes 2–3 of meter/relay frames (before the SWR/inductance field) are ignored.

### 2.3 Sequence on connect

1. Dial with 10 s handshake timeout.
2. On success: mark device online (see §5), log, and **send a Sync frame
   (`FF 01 00`)** asking the tuner for a full state snapshot (the tuner responds with
   Meter and/or Relay frames). Send failure is ignored (`_ =`).
3. Enter read loop: blocking read of one binary message at a time; each message is
   decoded as above and any state change is pushed to the MQTT publisher.
4. Read error → return the error; the outer loop (§5) marks the device offline and
   backs off.

### 2.4 Writes (commands to the tuner)

- Every command write is serialized under a mutex and given a **5 s write deadline**
  (`writeTimeout`). If the WebSocket is not currently connected, the write fails
  immediately with error `atr1k: websocket not connected`.
- Commands sent while the tuner link is down simply fail and are logged (warn) —
  they are **not queued or retried**.

---

## 3. MQTT presence

### 3.1 Connection parameters

| Property | Value |
|----------|-------|
| Broker | `tcp://192.168.1.50:1883` (plain MQTT, paho defaults ⇒ MQTT 3.1.1) |
| Auth | username `hf` (config `mqtt.user`), password from env (never in TOML) |
| Client ID | default `<site>-<station>-<slot>` ⇒ `muehle-hf-tuner`; override `mqtt.client_id` |
| Auto-reconnect | yes (paho `SetAutoReconnect(true)`), retry connect every 5 s (`SetConnectRetryInterval(5s)`), infinite retries |
| LWT | topic `<slot>/status`, payload `offline`, QoS 1, retained — registered at connect |
| On (re)connect | publish `online` to `/status` (retained, QoS 1); re-subscribe `/cmd` (QoS 1); re-publish retained `/meta` |
| Graceful disconnect | on SIGTERM the client is disconnected (500 ms paho quiesce) so the LWT normally does NOT fire; `offline` appears only on a crash/network loss |
| Context-aware connect | the initial connect is interrupted by SIGTERM (bridged through a goroutine + select; otherwise paho's blocking connect would hang systemd's stop until SIGKILL) |

### 3.2 Topics (exact strings, with defaults site=muehle, station=hf, slot=tuner)

| Topic | Direction | Retained | QoS used | Content |
|-------|-----------|----------|----------|---------|
| `muehle/hf/tuner/meta` | bridge → bus | yes | 1 | JSON birth certificate (below) |
| `muehle/hf/tuner/state` | bridge → bus | yes | 1 | JSON state snapshot (below) |
| `muehle/hf/tuner/status` | bridge → bus (LWT) | yes | 1 | plain string `online` or `offline` |
| `muehle/hf/tuner/cmd` | bus → bridge | **no** | subscribed at QoS 1 | JSON command intent (§4) |

The slot address is not hardcoded: topics are `<site>/<station>/<slot>/…` built from
three config fields. `/cmd` is deliberately not retained: a tune is one-shot, and a
stale retained `tune` replayed after a bridge restart could re-key the tuner
unexpectedly. Self-heal comes from consumers re-reading retained `/state`.

QoS policy in the publisher (behavior contract): **retained documents (meta, state)
are published at QoS 1; non-retained would be QoS 0** (no non-retained publishes
exist in this bridge). Publishes are fire-and-forget — delivery tokens are never
awaited and publish errors are never surfaced (paho queues QoS 1 internally).

### 3.3 `/meta` payload (retained, republished on every MQTT (re)connect)

```json
{
  "schema": "1.0",
  "role": "tuner",
  "device": { "model": "ATR-1000" },
  "link": "wifi",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "inline": true,
    "tune_modes": ["mem", "full"]
  },
  "expose": {
    "device": { "name": "HF ATU", "model": "ATR-1000" },
    "fields": [
      { "key": "swr",           "name": "SWR",           "type": "number",  "unit": "ratio", "class": "swr",   "state_class": "measurement" },
      { "key": "fwd",           "name": "Forward Power",  "type": "number",  "unit": "W",    "class": "power", "state_class": "measurement" },
      { "key": "inline",        "name": "In Line",       "type": "boolean", "writable": true,
        "command": { "action": "set_inline", "value_key": "value", "value_type": "bool" } },
      { "key": "l_uh",          "name": "Inductance",    "type": "number",  "unit": "µH" },
      { "key": "c_pf",          "name": "Capacitance",   "type": "number",  "unit": "pF" },
      { "key": "settling",      "name": "Tuning",        "type": "boolean" },
      { "key": "fault",         "name": "Fault",         "type": "string" },
      { "key": "device_online", "name": "Device Online", "type": "boolean" }
    ],
    "actions": [
      { "key": "tune", "name": "Tune",
        "command": { "action": "tune", "value_key": "value", "value_type": "enum" } }
    ]
  }
}
```

Exact constant values: `schema` is always `"1.0"`; `role` is always `"tuner"`;
`capabilities.inline` is always `true` (hardcoded); `capabilities.tune_modes` is
always `["mem", "full"]`; `expose.device.name` is `"HF ATU"`. `device.model`, `link`,
`location`, `host` come from config (defaults `ATR-1000`, `wifi`, `bauwagen`, `shari`).
`expose.device.manufacturer` is deliberately absent. Omit-if-empty applies to
`link`, `location`, `host`. The `value_key: "value"` in both command descriptors is
the station-wide `/cmd` convention (§4) — this is the field that was wrong live and
is now fixed.

### 3.4 `/state` payload (retained, change-deduped)

Published **only when a published field changes** vs. the last published snapshot
(the ATR streams meter frames frequently; dedup keeps the bus quiet at steady SWR).
The envelope adds `ts`; all other fields are the canonical tuner state:

```json
{
  "ts": "2026-07-12T10:30:00Z",
  "inline": true,
  "swr": 2.17,
  "fwd": 750,
  "l_uh": 12.34,
  "c_pf": 56,
  "settling": false,
  "device_online": true
}
```

| Field | JSON type | Units | Semantics |
|-------|-----------|-------|-----------|
| `ts` | string | RFC3339, UTC | publish timestamp |
| `inline` | boolean | — | tuner in the RF path (true) or bypassed (false). Client-side belief: set optimistically on `set_inline`/`tune`; the tuner sends no inbound confirmation (see §9) |
| `swr` | number | ratio | raw/100 when raw ≥ 100; raw otherwise |
| `fwd` | number (uint16) | watts | forward power |
| `l_uh` | number | µH | matching inductance (raw/100) |
| `c_pf` | number (uint16) | pF | matching capacitance |
| `settling` | boolean | — | a tune cycle is in progress; consumers treat it as a transmit-inhibit hint. Cleared by any Relay frame or by timeout |
| `fault` | string, **omitted when empty** | — | `"tune timeout"` after a tune cycle fails to settle in time; cleared by the next Relay frame |
| `device_online` | boolean | — | the **ATU** link is up while the bridge is up. Distinct from `/status` (bridge liveness) |
| `error` | string, **omitted when empty** | — | human-readable device fault when `device_online` is false, e.g. `atr1k: atr1k read: unexpected EOF` |

Field-order in the JSON follows the struct: `ts, inline, swr, fwd, l_uh, c_pf,
settling, fault, device_online, error` (JSON order is incidental, but the presence
semantics are not). Note the shipped doc example shows `"fault": ""` and
`"error": ""` — the code actually **omits** these keys when empty (Go `omitempty`);
the doc lags.

Publish cadence: event-driven only, no periodic heartbeat on `/state`. Triggers:
(1) tuner link established → snapshot with `device_online: true`, last-known
measurements preserved; (2) each decoded Meter frame; (3) each decoded Relay frame;
(4) command acknowledgment state change; (5) tune timeout; (6) tuner link lost →
snapshot with `device_online: false` + `error` set, `settling` forced false, and the
last SWR/fwd/L/C values **preserved** (not zeroed).

Dedup comparison is exact (`==` on every published field including floats) — safe
here because raw device values are passed through unchanged, so a steady reading
produces bit-identical snapshots.

### 3.5 `/status`

Retained plain string. `online` published by the bridge on every MQTT (re)connect;
`offline` set by the broker LWT on ungraceful loss. It reflects **the bridge
process**, never the tuner: while the tuner WebSocket is down and the bridge is
backing off, `/status` stays `online` and only `/state.device_online` is false.
Consumers must AND the two (station-wide two-layer liveness rule).

### 3.6 Subscriptions

Only one: `muehle/hf/tuner/cmd` at QoS 1, re-subscribed on every MQTT reconnect
(not retained, so nothing replays). The paho message handler only enqueues onto a
bounded channel (capacity 8); a single worker goroutine drains and executes
commands serially. If the queue is full, the command is **dropped** with a warning
log (`/cmd queue full, dropping command`) — flooding never blocks paho's dispatch.

---

## 4. Command surface

### 4.1 The `/cmd` payload convention (BEHAVIOR CONTRACT — the live-fixed rule)

Every station bridge accepts `/cmd` JSON of the shape:

```json
{ "action": "<action-word>", "value": <argument> }
```

The argument **always rides under the JSON key `"value"`**, never under a key named
after the action. This bridge originally decoded `set_inline` from a key `"inline"`
and `tune` from a key `"mode"`; the antennaselect reconciler (following the
stationa convention, as ACOM PA bridge's `set_band` already did) published
`{"action":"set_inline","value":true}` and the bridge silently discarded it live
(logged `cmd set_inline: missing inline`). Fixed in commit `3dfe067` (2026-07-12,
verified live on shari). **A re-implementation must accept only the `value` key** —
`value` is also advertised in `/meta.expose` as `value_key` for both commands.

### 4.2 Actions accepted

**`set_inline`** — `{"action":"set_inline","value":true|false}` (JSON boolean).
- Sends TuneStatus frame `FF 03 01 01` (true, in line) or `FF 03 01 00` (false,
  bypass) to the tuner.
- On successful write: optimistically sets local state `inline` to the commanded
  value and publishes a `/state` snapshot (subject to dedup).
- On write failure (tuner offline): warn log; state unchanged.
- Missing or non-boolean `value`: warn log (`cmd set_inline: missing value` /
  `bad value`), no tuner write, no state change.
- This is the soft-binding target antennaselect drives automatically per band.

**`tune`** — `{"action":"tune","value":"mem"}` or `{"action":"tune","value":"full"}`.
- `"mem"` (memory recall, fast) sends TuneMode frame `FF 04 01 01`;
  `"full"` (full search, slower) sends `FF 04 01 02`.
- On successful write: sets local state `inline=true` and `settling=true`, arms a
  **12 s tune timer**, publishes a snapshot.
- Settle: the next Relay frame from the tuner clears `settling`, stops the timer,
  clears any `fault`, and publishes the new L/C values.
- Timeout: if no Relay frame arrives within 12 s, `settling` → false and
  `fault` → `"tune timeout"`; snapshot published. The fault persists until the next
  Relay frame.
- A second `tune` while one is pending: re-arms (restarts) the 12 s timer.
- Unknown mode string (anything other than exactly `mem`/`full`, e.g. `"fine"` or
  `"Fine"`): warn log, no write.
- Missing `value`: warn log, no write.

**Anything else**: any other `action` (including missing action ⇒ `""`), and any
payload that is not valid JSON: warn log, ignored, no state change, no reply topic —
the bridge never publishes to `/cmd` and has no error channel beyond logs.

Semantics note: `value` for `set_inline` is a JSON boolean; for `tune` a JSON string.
Extra keys in the payload are ignored. Multiple `/cmd` messages execute in arrival
order, serialized by the single worker.

---

## 5. Behavior & state machine

### 5.1 Startup sequence

1. Parse flags (`-config`, `-log.level`, `-debug`); load TOML config (defaults if
   file missing; malformed TOML or unreadable file ⇒ exit code 2), apply env
   overrides, then flag overrides; validate (empty `mqtt.site`/`mqtt.station` or
   `tuner.url` ⇒ exit 2).
2. Create device client and bridge; wire the bounded `/cmd` worker channel.
3. Connect MQTT (LWT registered; on connect: publish `online`, subscribe `/cmd`,
   publish `/meta`). If the initial connect fails, the bridge retries internally
   every 5 s forever; SIGTERM interrupts it (exit 0, clean stop).
4. Enter the tuner WebSocket loop (below). Startup does **not** publish `/state`
   until the first telemetry or link-status event.

### 5.2 Tuner WebSocket loop (`wsLoop`)

```
loop forever until SIGTERM:
    dial tuner (10 s handshake timeout)
      on error → mark device offline (error string "atr1k: dial atr1k: …"),
                 sleep backoff, backoff ×1.5 (cap 60 s), retry
      on success → mark device online (error cleared),
                 send Sync frame (FF 01 00),
                 read loop:
                     each binary message → decode Meter/Relay → update state →
                     publish (deduped)
                     read error → exit read loop
    mark device offline (error string "atr1k: atr1k read: …" or write-path error),
                 sleep backoff, backoff ×1.5 (cap 60 s), retry
```

Exact timings:

- **Initial backoff 2 s**, multiplied by 1.5 after each failed attempt,
  **capped at 60 s**. Sequence: 2 s, 3 s, 4.5 s, 6.75 s, … 60 s.
  Note: the backoff is *never reset* after a successful connection — it keeps its
  grown value for the process lifetime (see §9).
- **Tune timeout 12 s** (see §4.2).
- **Write timeout 5 s** per tuner command.
- **MQTT connect retry 5 s**; **MQTT disconnect quiesce 500 ms** at shutdown.
- A SIGTERM at any point closes the WebSocket (unblocking the read loop) and exits
  with code 0. Any other fatal error exits with code 1.

### 5.3 State transitions

State fields and what changes them:

- `device_online/error`: set true/"" on successful dial; set false/"`atr1k: <read
  or dial error>`" on read-loop/dial exit. On going offline, a pending tune is
  aborted (timer stopped, `settling` forced false) — a dropped connection means it
  will not settle — and the last SWR/fwd/L/C values are preserved in the snapshot.
- `inline`: set by `set_inline` command (optimistic, after successful write); forced
  true by a `tune` command; preserved across tuner reconnects.
- `swr`/`fwd`: only from Meter frames. `l_uh`/`c_pf`: only from Relay frames.
- `settling`: true on `tune`; false on Relay frame / timeout / tuner link loss.
- `fault`: `"tune timeout"` from the timer; cleared by any Relay frame; cleared on
  tuner link regain (state rebuilt with empty fault).

### 5.4 Error paths summary

| Event | Effect |
|-------|--------|
| Tuner dial fails | `/state.device_online=false`, error `atr1k: dial atr1k: …`, `/status` stays online, backoff |
| Tuner read errors mid-stream | same, error `atr1k: atr1k read: …`, backoff |
| Command while tuner offline | warn log, no state change, command lost |
| Malformed tuner frame | silently ignored |
| Unknown/invalid `/cmd` | warn log, ignored |
| `/cmd` flood (queue > 8) | commands dropped with warn |
| MQTT broker loss | paho auto-reconnects (5 s interval); on regain: `online`, resubscribe, `/meta` republish. No `/state` republish is forced (see §9) |
| SIGTERM | clean exit 0; MQTT disconnected gracefully (LWT does not fire) |

---

## 6. Configuration

TOML file, default path `/etc/atr1k-tuner-bridge/config.toml` (flag `-config`).
Missing file ⇒ built-in defaults (the binary works with zero configuration).
Malformed file ⇒ startup failure. Unknown TOML keys are silently ignored.
Precedence: defaults < TOML < environment < `-log.level` flag.

| Key | Default | Meaning |
|-----|---------|---------|
| `host` | `shari` | compute node name, published in `/meta.host` |
| `tuner.url` | `ws://192.168.1.20:60001` | ATR-1000 binary WebSocket endpoint |
| `device.model` | `ATR-1000` | identity in `/meta.device.model` and `expose.device.model` |
| `device.link` | `wifi` | transport label in `/meta.link` |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | MQTT broker URL |
| `mqtt.client_id` | `""` ⇒ `muehle-hf-tuner` | MQTT client id (built `<site>-<station>-<slot>`) |
| `mqtt.site` | `muehle` | slot address part 1 (mandatory, non-empty) |
| `mqtt.station` | `hf` | slot address part 2 (mandatory, non-empty) |
| `mqtt.slot` | `tuner` | slot address part 3 |
| `mqtt.location` | `bauwagen` | physical location label in `/meta` |
| `mqtt.user` | `hf` | MQTT username |
| `mqtt.password` | `""` (never in TOML) | MQTT password — supplied via env |
| `log.level` | `info` | `debug`\|`info`\|`warn`\|`error` |

Environment overrides (all optional, non-empty values replace TOML):
`ATR1K_TUNER_BRIDGE_MQTT_BROKER`, `ATR1K_TUNER_BRIDGE_MQTT_CLIENT_ID`,
`ATR1K_TUNER_BRIDGE_MQTT_USER`, `ATR1K_TUNER_BRIDGE_MQTT_PASSWORD`,
`ATR1K_TUNER_BRIDGE_MQTT_SITE`, `ATR1K_TUNER_BRIDGE_MQTT_STATION`,
`ATR1K_TUNER_BRIDGE_MQTT_SLOT`, `ATR1K_TUNER_BRIDGE_TUNER_URL`.

**Secrets**: the MQTT password is loaded from
`ATR1K_TUNER_BRIDGE_MQTT_PASSWORD`, supplied by a systemd `EnvironmentFile`
(`/etc/atr1k-tuner-bridge/atr1k-tuner-bridge.env`, 0600, owned by the service user)
— never in the TOML, unit file, or process command line.

Flags: `-config <path>` (default above); `-log.level` overrides config level
(case-insensitive); `-debug` (default false) logs every WebSocket frame as hex
(`TX to ATR: …` / `RX from ATR: …`).

---

## 7. Deployment

- Target host: Raspberry Pi `shari` at `192.168.1.139`, SSH user `io` (all
  defaults overridable by env vars: `SSH_HOST`, `SSH_USER`, `SERVICE_NAME`,
  `SERVICE_USER`, `INSTALL_DIR`, `BINARY`, plus the config seeding vars
  `TUNER_URL`, `LOCATION`, `HOST_NAME`, `DEVICE_MODEL`, `DEVICE_LINK`,
  `LOG_LEVEL`, `MQTT_BROKER`, `MQTT_SITE`, `MQTT_STATION`, `MQTT_SLOT`,
  `MQTT_USER`, `MQTT_PASSWORD`).
- `./deploy.sh`:
  1. Cross-compiles `GOOS=linux GOARCH=arm64 CGO_ENABLED=0` with
     `-trimpath -ldflags="-s -w"` into `dist/atr1k-tuner-bridge-linux-arm64`.
  2. Generates the TOML seed (password deliberately empty) and the env-file seed
     (password only) in local 0600 temp files, and the systemd unit.
  3. scp's binary, unit, and both seeds to the Pi.
  4. On the Pi: creates a system user `atr1k-tuner-bridge` (nologin, no home) if
     missing; creates config dir 0755 owned by service user; **seeds config and env
     files only if absent** (seed-once — the device owns its settings afterwards;
     subsequent deploys never touch them); stops the service, installs the binary
     to `/opt/atr1k-tuner-bridge/atr1k-tuner-bridge` (0755), installs the unit,
     `daemon-reload`, `enable`, `restart`, prints status.
- systemd unit (`atr1k-tuner-bridge.service`):
  - `Type=simple`, `ExecStart=/opt/atr1k-tuner-bridge/atr1k-tuner-bridge -config /etc/atr1k-tuner-bridge/config.toml`
  - `EnvironmentFile=/etc/atr1k-tuner-bridge/atr1k-tuner-bridge.env`
  - `After=network-online.target`, `Wants=network-online.target`
  - `Restart=on-failure`, `RestartSec=5`
  - `User=`/`Group=atr1k-tuner-bridge`; `ConfigurationDirectory`/`StateDirectory=atr1k-tuner-bridge`
  - Hardening (network-only service, no serial devices, no disk writes):
    `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
    `PrivateDevices`, `ProtectKernelTunables`, `ProtectKernelModules`,
    `ProtectControlGroups`, `RestrictAddressFamilies=AF_INET AF_INET6`,
    `RestrictNamespaces`, `LockPersonality`, `RestrictRealtime`, `RestrictSUIDSGID`,
    `RemoveIPC`, empty `CapabilityBoundingSet`/`AmbientCapabilities`,
    `ReadWritePaths=/var/lib/atr1k-tuner-bridge`.
  - Resource ceilings: `MemoryMax=256M`, `TasksMax=64`.
  - Logs: journal, `SyslogIdentifier=atr1k-tuner-bridge`.
- Runtime dependencies: outbound TCP to the tuner (192.168.1.20:60001) and the
  MQTT broker (192.168.1.50:1883). Nothing else; no udev rules, no local files.

---

## 8. Invariants & safety rules

1. **Two-layer liveness**: `/status` = bridge process liveness (LWT); `/state.device_online`
   = tuner link liveness. They are never conflated: a tuner outage must leave
   `/status` at `online`.
2. **`/cmd` must never be retained**, and the bridge must never publish to `/cmd` —
   especially never re-issue a stale `tune` after restart (a tune can re-key the
   tuner while the transmitter is on).
3. **Command arguments ride under the `value` key only** (§4.1). Legacy per-action
   keys are rejected, not tolerated.
4. Commands must never be executed on paho's message-dispatch goroutine (that
   deadlocks brokers clients); they serialize through a bounded queue with
   drop-on-overflow (never block).
5. Unknown actions/modes and malformed payloads are inert: logged, no device write,
   no state mutation, no crash.
6. A tuner-link loss aborts a pending tune (`settling=false`, timer stopped) — never
   leave `settling=true` while the tuner is unreachable.
7. Last-known measurements (swr/fwd/l_uh/c_pf) are preserved across tuner
   reconnects; only `device_online`/`error` (and a pending `settling`) change.
8. Malformed/unparseable slot addressing must abort startup (exit 2) — a malformed
   topic path must never be published.
9. `/state` is change-deduplicated — a steady tuner stream must not flood the bus.
10. No secrets in the TOML, unit file, or command line (EnvironmentFile only).
11. A SIGTERM is a clean stop: exit 0 (so `systemctl stop` is not a failure), and
    the tuner connection must be closed promptly (the blocking read is interrupted).
12. Writes to the tuner are serialized and time-bounded (5 s) — commands never
    interleave on the wire.

---

## 9. Known defects & fragilities

1. **README endpoint disagreement**: README "Requirements" says the default is
   `ws://192.168.1.111:60001`; code, config.example.toml, deploy.sh and the README's
   own config table say `ws://192.168.1.20:60001` (code is truth: `192.168.1.20`).
2. **SWR raw < 100 is published undivided**: raw 50 would publish SWR 50 instead of
   0.5. Benign in practice (SWR ≥ 1.0 ⇒ raw ≥ 100) but a reimplementation must decide
   explicitly; the historical rule is "divide by 100 only when raw ≥ 100".
3. **`inline` is client-side belief only**: the protocol has no inbound frame
   confirming in-line/bypass (cmd 3 is outbound; the decoder ignores inbound cmds
   other than 2 and 5). If the operator toggles the tuner's front panel, or a
   `tune` is interpreted by the hardware without going in-line, `/state.inline`
   is wrong until the next `set_inline`. Also preserved-as-true across tuner
   reconnects regardless of what the tuner actually did while the link was down.
4. **Frame length byte unchecked on receive**: the decoder uses `data[3:]` and
   ignores byte 2, relying solely on minimum-length checks (≥ 8 for meter, ≥ 10
   for relay). A frame with a bogus length decodes anyway.
5. **Backoff never resets**: the ×1.5/60 s-cap backoff grows monotonically across
   the process lifetime and is not reset after a successful dial. A long-lived
   bridge that has had many tuner dropouts retries slowly forever (up to 60 s)
   even for the next transient blip.
6. **No `/state` republish after broker flush**: on MQTT reconnect only `/meta`
   and `/status` are re-published. If the broker lost the retained `/state` (e.g.
   broker restart), the bridge will not re-publish it until a dedup-defeating
   telemetry change occurs — a tuner at a rock-steady SWR could leave `/state`
   stale/absent indefinitely.
7. **Publish delivery is fire-and-forget**: QoS-1 publish tokens are never awaited;
   a failed publish (broker hiccup mid-queue) is invisible to the bridge.
8. **No WebSocket keepalive/ping**: a half-open tuner TCP link (e.g. Wi-Fi drop
  without RST) blocks the read loop indefinitely; `device_online` stays true until
   the OS errors the socket or a write times out (5 s deadline only applies to
   command writes).
9. **`tune` timer race window**: the 12 s timer callback runs on its own goroutine
   and races a Relay frame under the mutex — handled correctly, but the timer is
   merely *stopped*, not drained, so a just-fired callback can still set
   `fault: "tune timeout"` immediately after a Relay frame cleared settling in a
   new tune cycle (window is microseconds; never observed live).
10. **Doc lags on `fault`/`error` presence**: `docs/atr1k-tuner-bridge-mqtt-api.md`
    shows `"fault": ""` / `"error": ""` in the example; the code omits the keys when
    empty (Go `omitempty`). Consumers must treat absence = empty.
11. **Meter-frame cadence unknown**: the code imposes no read timeout; the tuner's
    push rate (and whether Meter frames pause when idle/no RF) is not documented
    anywhere in this repo — verify against hardware.
12. **TuneMode values 0 (Reset) and 3 (Fine) exist in the protocol but are not
    exposed** over MQTT; `{"action":"tune","value":"fine"}` is rejected. If the
    hardware supports Fine mode, the bridge cannot drive it.

---

## 10. Re-implementation notes

**Must be preserved verbatim (behavior contract):**

- Exact topic strings and their retained/QoS split: retained `/meta`, `/state`,
  `/status` at QoS 1; `/cmd` subscribed QoS 1, never retained.
- `/meta` JSON shape, constant values (`schema:"1.0"`, `role:"tuner"`,
  `capabilities.inline:true`, `tune_modes:["mem","full"]`, expose field list with
  exact `key`/`name`/`type`/`unit`/`class`/`state_class`, `value_key:"value"`).
- `/state` JSON field names and types (`ts`, `inline`, `swr`, `fwd`, `l_uh`, `c_pf`,
  `settling`, `fault` omitempty, `device_online`, `error` omitempty), the exact
  scaling (SWR raw/100 when ≥100, L raw/100, C raw), and change-dedup semantics.
- The `/cmd` convention: `{"action":…,"value":…}` — bool for `set_inline`, exactly
  `"mem"`/`"full"` for `tune` — and inert handling of everything else.
- The binary frame layout (`FF cmd len payload`, LE uint16 fields, offsets 4/6 and
  6/8), the Sync-on-connect (`FF 01 00`), the three outbound command frames, and
  decoding of inbound cmds 2 and 5 only.
- All timing constants: dial handshake 10 s, write deadline 5 s, tune timeout 12 s,
  backoff 2 s ×1.5 cap 60 s, MQTT retry 5 s, cmd queue depth 8 with drop-on-full.
- Two-layer liveness (§8.1), offline-state preservation of measurements (§8.7),
  pending-tune abort on link loss (§8.6), `/meta` + `online` republish on every
  MQTT reconnect, exit-0 clean shutdown.
- Config key names, defaults, env var names, seed-once deploy behavior, and the
  no-secret-in-TOML rule (the systemd unit/hardening details are Linux-specific but
  the isolation intent — dedicated user, no privileged capabilities, resource
  ceilings — should be mirrored).

**Free to change (implementation detail):**

- Go/paho/gorilla specifics; the goroutine+select context bridge for connect; the
  package layout (`cmd/`, `internal/{tuner,bridge,config}`); slog text logging.
- The dedup mechanism (exact float `==`) — any equivalent change-detection is fine
  as long as steady readings produce no repeated publishes.
- Backoff growth curve shape (keep start ≈ 2 s and cap ≈ 60 s; resetting on
  success is an *improvement*, not a violation).
- JSON key ordering and whitespace; logging wording (except that warn logs are the
  only command-error channel).
- The frame-length byte's presence (the hardware protocol requires it; a new
  implementation should validate it even though this one does not).

**Open verification items for the new team (not derivable from this repo):** the
tuner's actual meter push cadence; whether the tuner ever echoes cmd 3 inbound (to
fix defect §9.3); whether idle (no-RF) Meter frames carry SWR raw 0 and how that
should be rendered (raw 0 < 100 ⇒ published as 0).