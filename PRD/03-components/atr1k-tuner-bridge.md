# 03-components — atr1k-tuner-bridge (ATR-1000 antenna-tuner bridge)

This document specifies **atr1k-tuner-bridge**. It is one of the station's thin
adapter services. The long-running daemon connects one physical device — an
**ATR-1000 automatic antenna tuner** — to the station's MQTT message bus. The
bridge translates between the tuner's proprietary binary WebSocket protocol and
the station's canonical JSON-over-MQTT contract. It streams telemetry (SWR,
forward power, inductor and capacitor relay values, in-line/bypass position,
tune-cycle status) out of the tuner as one retained JSON snapshot. It also
accepts two commands: put the tuner in the RF path or bypass it, and start a
tune cycle. A re-implementation team must be able to rebuild this component's
observable behavior exactly from this document alone. The team needs no access
to the original code.

**Reader prerequisites.** This document defines every term at first use. It
assumes the reader has read `00-system-overview.md` for the shape of the
station and `01-architecture.md` for the bus model. `02-interface-spec.md`
defines the bus-level contracts (planes, /cmd convention, liveness)
normatively. This document restates the parts specific to the tuner slot and
links the rest.

---

## 1. Glossary (plain-language definitions)

- **Amateur radio (ham radio)**: the licensed hobby of two-way radio
  communication. **HF** ("high frequency") is the 3–30 MHz shortwave spectrum
  used for long-distance communication.
- **Transmitter / radio**: the device that generates the radio-frequency signal
  that an antenna radiates.
- **Antenna**: the passive structure that converts the transmitter's electrical
  signal into radio waves. A real antenna rarely matches the transmitter
  impedance perfectly, especially across many frequency bands.
- **ATU / antenna tuner**: an *antenna tuner unit* — a box of switchable
  inductors (measured in microhenry, µH) and capacitors (picofarad, pF). The
  feedline (the cable carrying the signal to the antenna) carries it. It
  matches an imperfect antenna electrically to the transmitter so that the
  antenna system does not reflect power back toward the transmitter.
- **SWR (standing-wave ratio)**: a dimensionless number ≥ 1.0. It says how
  badly the antenna system matches the transmitter impedance. 1.0 is perfect.
  Above roughly 3 it is bad — much of the transmit power goes back toward the
  transmitter instead of into the antenna. On the tuner's wire protocol the
  tuner sends it as an integer equal to SWR×100.
- **Forward power (fwd)**: watts of power flowing from the transmitter toward
  the antenna.
- **In-line vs bypass**: the ATU holds a relay pair that either inserts the
  matching network into the RF path ("in line") or shorts past it ("bypass").
  A bypassed tuner is electrically inert.
- **Tune cycle**: the ATU's relay search that finds an inductor/capacitor (L/C)
  combination matching the antenna at the current frequency. **"Mem"** recalls
  a previously stored solution (fast). **"full"** searches from scratch
  (slower). **Settling** means a tune cycle is in progress — the relays move,
  and transmission must pause.
- **MQTT**: a lightweight publish/subscribe protocol. Clients publish messages
  to hierarchical topic strings, and subscribers get them through a central
  **broker** (a server). See `01-architecture.md`.
- **Retained message**: an MQTT message the broker stores and re-delivers to
  any future subscriber. A new consumer immediately sees the last known state.
- **LWT (Last Will and Testament)**: a message the broker publishes on a
  client's behalf if that client disconnects ungracefully — here the string
  `offline` on the `/status` topic.
- **QoS**: MQTT delivery-guarantee level. QoS 0 = at most once, QoS 1 = at
  least once.
- **WebSocket**: a TCP protocol giving a full-duplex message channel over an
  HTTP upgrade. **binary WebSocket** means messages carry raw bytes, not text.
- **Slot**: the station model's fixed MQTT address of the form
  `<site>/<station>/<slot>` (default `muehle/hf/tuner`). The four topic
  *planes* `meta`, `state`, `status`, `cmd` hang off it. See
  `02-interface-spec.md` §2.
- **Bridge / adapter**: this service — a thin translator between one vendor
  device's proprietary protocol and the station's canonical MQTT contract.
- **shari**: the Raspberry Pi single-board computer at `192.168.1.139` on the
  site's LAN that runs all station services.
- **antennaselect**: a sibling *logic* service with no physical device (slot
  `muehle/hf/antenna-select`). It decides which antenna and routing to use per
  band, and it drives this tuner's `set_inline` command automatically.
- **hadiscovery**: a sibling consumer that renders home-automation entities
  from this bridge's `expose` metadata block. This bridge itself carries no
  home-automation vocabulary.

---

## 2. Role in the station

The bridge implements the station slot `muehle/hf/tuner`. It is **read-write**:

1. **Read** — it streams telemetry out of the tuner (SWR, forward power, L/C
   relay values). It also reads the tuner's in-line/bypass position and
   tune-cycle status. It publishes all of this as one retained JSON snapshot on
   `<slot>/state`.
2. **Write** — it accepts two commands on `<slot>/cmd`: put the tuner in line
   or in bypass, and start a tune cycle (memory recall or full search).

It does not transmit and does not drive the radio.

**Band vocabulary used below.** An antenna is **resonant** on a band when it is
electrically matched there, so the transmitter needs no tuner. The
**fan-dipole** is a multi-band wire antenna: several dipole radiators share one
feedpoint. Each amateur band name gives the wavelength in meters. The 30/60/160
m bands are the 10/5/1.8 MHz amateur allocations.

The antennaselect reconciler drives `set_inline` automatically for non-resonant
bands. It publishes `set_inline=true` when the fan-dipole antenna is the
selection on the 30/60/160 m bands, and `false` otherwise (see the antennaselect
component document). The `hadiscovery` consumer renders entities from this
bridge's `expose` block.

**Device context.** The ATR-1000 (BTR-1000 / N7DDC device family) is a
Wi-Fi-enabled automatic antenna tuner. It runs a binary WebSocket server on TCP
port 60001 and continuously pushes meter and relay telemetry frames to any
connected client. The repo holds no vendor documentation for this protocol —
the original team reverse-engineered it by observing the device. The frame
format in §4 is the authoritative description.

**Station-wide facts that apply here** (see `02-interface-spec.md` for detail):

- The MQTT broker in today's production is at `192.168.1.50:1883` (plain MQTT,
  no TLS). A migration to a broker on shari (`192.168.1.139`) exists on an
  unmerged feature branch. Nobody has deployed it. The re-implementation
  targets `192.168.1.50:1883` and must make the broker address configurable
  (§8).
- **Two-layer liveness is real and subtle**: `/status` (bridge process alive,
  through MQTT last-will) and `/state.device_online` (device behind the bridge
  alive) are two different signals. Consumers must check both with a logical
  AND. On a clean process shutdown the broker does NOT fire the last-will. A
  retained `/status` of `online` can persist for a stopped service — consumers
  must not trust `/status` alone.
- The deployed bridge publishes `device_online:true` **explicitly**. The
  integration-model document says "omitted when true". Consumers must treat
  absence as true (see §11, open decision).

---

## 3. Upstream interface — the ATR-1000 binary WebSocket

### 3.1 Transport and connection

The bridge must connect **outbound** to the tuner over a plain (no TLS)
WebSocket at `ws://<tuner-host>:60001`. The tuner is the server. The bridge
always dials out. Default endpoint: `ws://192.168.1.20:60001` (configurable,
§8).

- There is no handshake beyond the HTTP WebSocket upgrade: no authentication,
  no hello message.
- The WebSocket handshake timeout must be **10 s**.
- The process shutdown signal (SIGTERM) must interrupt the dial operation — a
  shutdown during a dial must not hang.
- Messages must be WebSocket **binary** messages. Each message is exactly one
  frame (§3.2).
- **Connection-loss detection**: any error from the socket read loop (including
  a clean close) ends the read loop. The reference implementation has **no
  ping/pong keepalive and no idle-read timeout**. The operating system detects
  a silently dead TCP link only when the OS errors the socket or a write fails.
  See §10.8 for the consequence and §11 for the re-implementation decision.

### 3.2 Frame format (reverse-engineered wire protocol)

Every frame, inbound and outbound:

```
byte 0: 0xFF   flag — every frame starts with 0xFF
byte 1: cmd    command/type byte
byte 2: len    payload length in bytes
byte 3..      payload — multi-byte integers inside the payload are
              little-endian unsigned 16-bit (uint16 LE)
```

Command bytes:

| cmd | Name | Direction | Wire frame | Meaning |
|-----|------|-----------|------------|---------|
| 1 | Sync | bridge → tuner | `FF 01 00` (3 bytes) | request a full state snapshot from the tuner. |
| 2 | Meter | tuner → bridge | `FF 02 len` + payload | telemetry: SWR + forward power. The frame must be ≥ 8 bytes in total to decode. |
| 3 | TuneStatus | bridge → tuner | `FF 03 01 00` (bypass) / `FF 03 01 01` (in line) | put the tuner in line or bypass. |
| 4 | TuneMode | bridge → tuner | `FF 04 01 01` (mem) / `FF 04 01 02` (full) | start a tune cycle. |
| 5 | Relay | tuner → bridge | `FF 05 len` + payload | L/C relay telemetry. The frame must be ≥ 10 bytes in total to decode. |

**Meter frame (cmd 2) decoding.** Offsets are indices in the whole frame
(byte 0 is the 0xFF flag). The decoder ignores bytes 2–3.

- `swr_raw = uint16LE(frame[4:6])`.
  - If `swr_raw >= 100`, the published SWR = `swr_raw / 100`. Example: raw 217
    gives SWR 2.17.
  - If `swr_raw < 100`, the published SWR = `swr_raw` **unchanged** (NOT
    divided). In practice SWR ≥ 1.0, so raw ≥ 100 always. This quirk is a known
    defect (§10.2) — a re-implementation must decide the rule explicitly. The
    historical rule is "divide by 100 only when raw ≥ 100".
- `fwd = uint16LE(frame[6:8])` — forward power in whole watts, published
  unchanged.

**Relay frame (cmd 5) decoding.** The decoder ignores bytes 2–5.

- `l_uh = uint16LE(frame[6:8]) / 100` — inductance in µH with two implied
  decimals (raw 1234 → 12.34 µH).
- `c_pf = uint16LE(frame[8:10])` — capacitance in pF, published unchanged
  (raw 56 → 56).

**Inbound frame handling.**

- The bridge silently discards inbound frames with any other cmd byte
  (including a tuner echo of Sync, TuneStatus, or TuneMode) — no log, no state
  change.
- The bridge silently discards malformed frames (total message shorter than 3
  bytes, or byte 0 ≠ 0xFF).
- The outbound frames carry the length byte (byte 2). The reference
  implementation does **not validate it on receipt**. The decoder takes the
  payload as everything after byte 3 and relies only on the minimum-length
  checks (≥ 8 bytes for Meter, ≥ 10 bytes for Relay). See §10.4. A
  re-implementation must validate the length byte (improvement, not a
  violation).

### 3.3 Sequence on connect

1. Dial the tuner with the 10 s handshake timeout.
2. On success: mark the device online (§6.3), log, and send a **Sync frame
   (`FF 01 00`)** that requests a full state snapshot. The tuner responds with
   Meter and/or Relay frames. The bridge ignores a send failure of this Sync
   (not fatal).
3. Enter the read loop. Do a blocking read of one binary message at a time.
   Decode each message per §3.2, and push any state change to the MQTT
   publisher (deduplicated, §5.2).
4. A read error ends the read loop. The outer loop (§6.2) marks the device
   offline and backs off before redialing.

### 3.4 Writes (commands to the tuner)

- The bridge serializes every command write under a mutex, and every write
  has a **5 s write deadline** — a write that cannot complete within 5 s
  fails.
- If the WebSocket is not connected, the write must fail immediately with
  error `atr1k: websocket not connected`.
- The bridge logs commands that fail (tuner offline, write timeout) at warn
  level and does **not queue or retry them**. The bridge drops them forever.

---

## 4. MQTT presence

### 4.1 Connection parameters

| Property | Required value / behavior |
|----------|--------------------------|
| Broker | `tcp://192.168.1.50:1883` (config `mqtt.broker`) |
| Auth | username `hf` (config `mqtt.user`). The password comes from an environment variable, never from the config file |
| Client ID | default `<site>-<station>-<slot>` ⇒ `muehle-hf-tuner`. Override: `mqtt.client_id` |
| MQTT protocol version | 3.1.1 (the reference library default) |
| Auto-reconnect | enabled. The client retries connect every **5 s**, without an end |
| Last Will | topic `<slot>/status`, payload `offline`, QoS 1, retained — registered at connect time |
| On every (re)connect | publish `online` to `/status` (retained, QoS 1). Re-subscribe `/cmd` (QoS 1). Re-publish retained `/meta` |
| Graceful shutdown | on SIGTERM the client disconnects gracefully (a 500 ms quiesce in the reference library). The Last Will normally does NOT fire. `offline` appears only on a crash or network loss. Consequence: a cleanly stopped service leaves a retained `online` on `/status` — station-wide known behavior |
| Interruptible connect | SIGTERM must interrupt the first connect (otherwise a blocking connect hangs the service manager's stop until it force-kills the process) |

### 4.2 Topics (exact strings, defaults site=muehle, station=hf, slot=tuner)

| Topic | Direction | Retained | QoS | Content |
|-------|-----------|----------|-----|---------|
| `muehle/hf/tuner/meta` | bridge → bus | yes | 1 | JSON birth certificate (§4.3). |
| `muehle/hf/tuner/state` | bridge → bus | yes | 1 | JSON state snapshot (§4.4). |
| `muehle/hf/tuner/status` | bridge → bus (through LWT) | yes | 1 | plain string `online` or `offline`. |
| `muehle/hf/tuner/cmd` | bus → bridge | **no** | subscribed at 1 | JSON command intent (§5). |

The slot address is not hardcoded. The bridge builds topics
`<site>/<station>/<slot>/…` from three config fields (§8). The `mqtt.site` and
`mqtt.station` fields are mandatory and non-empty — an empty value stops startup
(§6.1). An empty or unset `mqtt.slot` never stops startup: the bridge silently
uses the default `tuner` instead. The same `tuner` fallback applies when the
bridge derives the default client ID (§4.1).

`/cmd` is deliberately NOT retained. A tune is one-shot. A stale retained
`tune` replayed after a bridge restart can re-key the tuner (change its relay
configuration) while the transmitter is on. Self-heal comes from consumers
re-reading the retained `/state`.

**Publish behavior.** The bridge publishes the retained documents (`/meta`,
`/state`) at QoS 1. Publishes are fire-and-forget: the bridge never waits for
delivery confirmations and never surfaces publish errors. The reference MQTT
library queues QoS-1 messages internally, so a mid-queue broker hiccup is
invisible to the bridge (§10.7).

### 4.3 `/meta` payload (retained, republished on every MQTT reconnect)

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
      { "key": "fwd",           "name": "Forward Power", "type": "number",  "unit": "W",    "class": "power", "state_class": "measurement" },
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

Normative constant values:

- `schema` is always the string `"1.0"`. `role` is always the string `"tuner"`.
- `capabilities.inline` is always boolean `true` (hardcoded).
  `capabilities.tune_modes` is always exactly `["mem", "full"]`.
- `expose.device.name` is always `"HF ATU"`. `expose.device.manufacturer` is
  deliberately absent.
- `device.model`, `link`, `location`, `host` come from config with defaults
  `ATR-1000`, `wifi`, `bauwagen`, `shari`. The code leaves empty values out of
  the JSON (applies to `link`, `location`, `host`).
- The `value_key: "value"` in both command descriptors is the station-wide
  `/cmd` convention — see §5.1. This exact descriptor text is normative.

### 4.4 `/state` payload (retained, change-deduplicated)

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
| `ts` | string | RFC 3339, UTC | publish timestamp |
| `inline` | boolean | — | tuner in the RF path (true) or bypassed (false). **Client-side belief**: set optimistically on `set_inline`/`tune`. The tuner sends no inbound confirmation (§10.3) |
| `swr` | number | ratio | raw/100 when raw ≥ 100. Raw otherwise (§3.2). |
| `fwd` | number (uint16) | watts | forward power, raw device value unchanged |
| `l_uh` | number | µH | matching inductance (raw/100) |
| `c_pf` | number (uint16) | pF | matching capacitance, raw unchanged |
| `settling` | boolean | — | a tune cycle is in progress. Consumers treat it as a transmit-inhibit hint. Any Relay frame or the 12 s timeout clears it |
| `fault` | string, **omitted when empty** | — | `"tune timeout"` when a tune cycle fails to settle within 12 s. The next Relay frame clears it |
| `device_online` | boolean | — | the **ATU link** is up. Explicitly published (true/false). Distinct from `/status` (bridge liveness). |
| `error` | string, **omitted when empty** | — | human-readable device fault when `device_online` is false, for example `atr1k: atr1k read: unexpected EOF`. |

`fault` and `error` must be **absent from the JSON when empty** — not present
as `""`. (The shipped API doc in the original repo shows `"fault": ""` /
`"error": ""` in its example. That doc lags the code, which omits the keys.
Consumers must treat absence as empty either way.)

**Publish cadence.** Event-driven only. There is NO periodic heartbeat on
`/state`. The bridge must publish `/state` only when a compared field changes
versus the last published snapshot. The ATR streams meter frames often. Dedup keeps the bus quiet at a steady SWR. The dedup comparison in the
reference implementation is exact equality on exactly these nine fields:
`inline`, `swr`, `fwd`, `l_uh`, `c_pf`, `settling`, `fault`, `device_online`,
`error`. The `ts` timestamp is not part of the comparison — the bridge
generates it fresh at each publish. This is safe here because raw device values
pass through unchanged, so a steady reading produces bit-identical field
values. Any change-detection that behaves the same is acceptable as long as
steady readings produce no repeated publishes.

**Publish triggers** (each subject to dedup):

1. Tuner link established → snapshot with `device_online: true`, last-known
   measurements preserved.
2. Each decoded Meter frame.
3. Each decoded Relay frame.
4. Command acknowledgment state change (§5.2).
5. Tune timeout firing (§5.2).
6. Tuner link lost → snapshot with `device_online: false` + `error` set, and
   the last SWR/fwd/L/C values **preserved**, not zeroed. **As-built defect**:
   this event does NOT stop a pending tune. `settling` stays true in this
   snapshot, and the still-armed timer later publishes `settling: false` +
   `fault: "tune timeout"` with `device_online` flipped back to true (§10.13).

Note the gap: an MQTT reconnect triggers only `/meta` and `/status` republish —
NOT `/state` (§10.6).

### 4.5 `/status`

Retained plain string. The bridge publishes `online` on every MQTT
(re)connect. The broker's Last Will sets `offline` on ungraceful loss. It
reflects **the bridge process only, never the tuner**: while the tuner
WebSocket is down and the bridge backs off, `/status` stays `online` and only
`/state.device_online` is false. Consumers must AND the two layers
(station-wide two-layer liveness rule, `02-interface-spec.md`). Also
station-wide: on a clean process shutdown the Will does not fire, so a stopped
service can leave a retained `online` on `/status`.

### 4.6 Subscription and command queue

- Exactly one subscription: `<slot>/cmd` at QoS 1, re-subscribed on every MQTT
  reconnect (it is not retained, so nothing replays on resubscribe).
- Incoming message handling must never execute commands on the MQTT client's
  message-dispatch thread. This is a library-independent constraint, with a
  live incident as rationale. In the reference MQTT library, handlers run on
  the connection's dispatch thread. A handler that blocks or publishes
  synchronously deadlocks the whole client — this happened live on another
  component of this station. The re-implementation MUST isolate handler work
  from the receive path regardless of library.
- The reference design: the handler only enqueues onto a bounded queue of
  capacity **8**. A single worker drains and executes commands serially. When
  the queue is full, the bridge **drops** the command with a warn log
  (`/cmd queue full, dropping command`) — flooding never blocks the dispatch
  thread.

---

## 5. Command surface

### 5.1 The `/cmd` payload convention — normative requirement derived from a live incident

Every station bridge accepts `/cmd` JSON of the shape:

```json
{ "action": "<action-word>", "value": <argument> }
```

The argument **ALWAYS rides under the JSON key `"value"`** — never under a key
named after the action. The type of `value` is action-specific (boolean for
`set_inline`, string for `tune`).

**Incident rationale (the re-implementation must honor this, not just note
it).** This bridge originally decoded `set_inline` from a key `"inline"` and
`tune` from a key `"mode"`. The antennaselect reconciler followed the
station-wide convention, as the ACOM power-amplifier bridge's `set_band`
command already did. It published `{"action":"set_inline","value":true}`, and
this bridge **silently discarded it live** (logging only `cmd set_inline:
missing inline`), so the antenna-tuner routing silently failed to actuate. A
fix landed on 2026-07-12 (commit `3dfe067`), and the live station on shari
confirmed it. A re-implementation MUST accept only the `value` key. The bridge
rejects per-action legacy keys. It does not tolerate them. The convention is
also advertised to consumers in `/meta.expose` as `value_key: "value"` for both
commands (§4.3), so the metadata and the decoder can never disagree again.

### 5.2 Actions accepted

**`set_inline`** — payload `{"action":"set_inline","value":true|false}` (`value` is
a JSON boolean).

- `true` sends TuneStatus frame `FF 03 01 01` (in line). `false` sends
  `FF 03 01 00` (bypass).
- On successful write: the bridge sets the local state field `inline`
  optimistically to the commanded value and publishes a `/state` snapshot
  (subject to dedup).
- On write failure (tuner offline): warn log. State unchanged. The bridge drops
  the command (no retry).
- Missing or non-boolean `value`: warn log (`cmd set_inline: missing value` /
  `cmd set_inline: bad value`). No tuner write. No state change.
- This is the **soft-binding** target that antennaselect drives automatically
  per band. A soft binding is a repeated command stream, not a handshake:
  antennaselect sends the command again whenever its model of the desired
  routing changes. This bridge does not acknowledge or confirm it.

**`tune`** — payload `{"action":"tune","value":"mem"}` or
`{"action":"tune","value":"full"}` (`value` is a JSON string, case-sensitive).

- `"mem"` (memory recall, fast) sends TuneMode frame `FF 04 01 01`.
- `"full"` (full search, slower) sends TuneMode frame `FF 04 01 02`.
- On successful write: the bridge sets local state `inline` true, sets
  `settling` true, arms a **12 s tune timer**, and publishes a snapshot.
- **Settle**: the next Relay frame (cmd 5) from the tuner clears `settling`,
  stops the timer, clears any `fault`, and publishes the new L/C values.
- **Timeout**: when no Relay frame arrives within **12 s**, `settling` → false
  and `fault` → `"tune timeout"`. The bridge publishes a snapshot. The fault
  stays until the next Relay frame.
- A second `tune` during a pending tune re-arms (restarts) the 12 s timer.
- A `value` other than exactly `mem` or `full` (examples: `"fine"`, `"Fine"`,
  `"0"`), or a missing `value`: warn log, no write. (The wire protocol also
  defines TuneMode values 0 = Reset and 3 = Fine, but the bridge does not
  expose them over MQTT — see §10.12.)

**Everything else is inert.** Any other `action` (including missing action ⇒
empty string), and any payload that is not valid JSON: warn log. The bridge
ignores it all: no state change, no device write, no reply topic. The bridge never publishes to
`/cmd` and has no error channel beyond logs.

The bridge ignores extra keys in a payload. The single worker serializes all
`/cmd` messages and executes them in arrival order.

---

## 6. Behavior and state machine

### 6.1 Startup sequence

1. Load configuration (§8): defaults < config file < environment < command-line
   log-level flag. A missing config file is acceptable (built-in defaults make
   the binary work with zero configuration). A malformed or unreadable file is
   a startup failure with **exit code 2**.
2. Validate: an empty `mqtt.site`, `mqtt.station`, or `tuner.url` is a startup
   failure with **exit code 2** (the bridge must never publish a malformed
   topic path).
3. Create the device client and bridge. Wire the bounded `/cmd` worker queue
   (capacity 8, drop-on-overflow).
4. Connect MQTT with the Last Will registered. On connect publish `online`,
   publish `/meta`, subscribe `/cmd`. When the first connect fails, retry
   every 5 s forever. SIGTERM interrupts it (clean exit 0).
5. Enter the tuner WebSocket loop (§6.2). Startup does NOT publish `/state`
   before the first telemetry or link-status event.

### 6.2 Tuner WebSocket loop

```
loop forever until SIGTERM:
    dial tuner (10 s handshake timeout)
      error → mark device offline (error string "atr1k: dial atr1k: …"),
              sleep backoff, backoff ×1.5 (cap 60 s), retry
      success → mark device online (error cleared),
              send Sync frame (FF 01 00),
              read loop:
                  each binary message → decode Meter/Relay → update state →
                  publish (deduplicated)
                  read error → exit read loop
    mark device offline (error "atr1k: atr1k read: …" or a write-path error),
              sleep backoff, backoff ×1.5 (cap 60 s), retry
```

### 6.3 State transitions

- `device_online` / `error`: true/empty after a successful dial. False/`atr1k:
  <read or dial error>` after a read-loop or dial exit. The bridge keeps the
  last SWR/fwd/L/C values in the snapshot. **As-built defect**: this event does
  not stop a pending tune — `settling` stays true until the orphaned timer
  fires (§10.13).
- `inline`: the `set_inline` command sets it (optimistically, after a
  successful write). A `tune` command forces it true. The value stays across
  tuner reconnects, regardless of what the tuner actually did while the link
  was down (§10.3).
- `swr`/`fwd`: only from Meter frames. `l_uh`/`c_pf`: only from Relay frames.
- `settling`: true on `tune`. False on a Relay frame or on timeout. **As-built
  defect**: a tuner link loss does not clear it — the pending timer still fires
  later (§10.13).
- `fault`: `"tune timeout"` from the timer. Any Relay frame clears it. Tuner
  link regain clears it too (state rebuilt with empty fault).

### 6.4 Exact timing constants (all normative defaults)

| Constant | Value | Applies to |
|----------|-------|------------|
| WebSocket handshake timeout | **10 s** | dialing the tuner. |
| Write deadline | **5 s** | each tuner command write. |
| Tune-settle timeout | **12 s** | Relay frame must arrive within this of a `tune`, or `fault: "tune timeout"` results. |
| Reconnect backoff | starts **2 s**, ×1.5 after each failed dial, **capped at 60 s** (2, 3, 4.5, 6.75 s, … 60 s) | tuner dial retries. Note: never reset after success in the reference implementation (§10.5). |
| MQTT connect retry | **5 s**, infinite | broker reconnect. |
| MQTT disconnect quiesce | **500 ms** | graceful shutdown. |
| `/cmd` queue depth | **8** | drop-on-overflow beyond this. |

### 6.5 Shutdown and exit codes

- SIGTERM at any point closes the tuner WebSocket (unblocking the read loop),
  disconnects MQTT gracefully, and exits with **code 0**. A service-manager
  stop is never a failure, and the Last Will does not fire.
- Any other fatal error exits with **code 1**.
- Startup validation / config failures exit with **code 2** (§6.1).

### 6.6 Error-path summary

| Event | Effect |
|-------|--------|
| Tuner dial fails | `/state.device_online=false`, `error` = `atr1k: dial atr1k: …`, `/status` stays `online`, backoff |
| Tuner read errors mid-stream | same, `error` = `atr1k: atr1k read: …`, backoff |
| Command while tuner offline | warn log. No state change. Command lost |
| Malformed tuner frame | silently ignored |
| Unknown/invalid `/cmd` | warn log. Ignored |
| `/cmd` flood (queue > 8) | commands dropped with warn log |
| MQTT broker loss | auto-reconnect every 5 s. On regain publish `online`, resubscribe `/cmd`, republish `/meta`. **The bridge does not force a `/state` republish** (§10.6) |
| SIGTERM | clean exit 0. Last Will does not fire |

---

## 7. Invariants and safety rules

1. **Two-layer liveness.** `/status` shows bridge process liveness (Last
   Will). `/state.device_online` shows tuner link liveness. Consumers must
   never conflate them: a tuner outage must leave `/status` at `online`.
2. **The broker must never retain `/cmd`, and the bridge must never publish
   to `/cmd`** — especially it must never re-issue a stale `tune` after a
   restart. A tune can re-key the tuner (move its relays) while the
   transmitter is on.
3. **Command arguments ride under the `value` key only** (§5.1). The bridge
   rejects per-action legacy keys. It does not tolerate them. This requirement
   exists because the violation silently broke live antenna routing once
   already.
4. **Commands must never execute on the MQTT dispatch thread.** They serialize
   through a bounded queue with drop-on-overflow, never blocking the receive
   path (library-independent — a live dead-lock incident on another component
   is the rationale).
5. **Unknown actions/modes and malformed payloads are inert**: logged, no
   device write, no state mutation, no crash.
6. **A tuner-link loss must stop a pending tune** (`settling=false`, timer
   stopped) — `settling=true` must never persist while the tuner is
   unreachable. The reference implementation violates this rule for up to 12 s
   (§10.13). The re-implementation must follow this rule as written here.
7. **Last-known measurements (swr, fwd, l_uh, c_pf) stay in place across
   tuner reconnects**. Only `device_online`/`error` and (per invariant 6) a
   pending `settling` change.
8. **Malformed slot addressing stops startup** (exit 2) — the bridge must
   never publish a malformed topic path.
9. **`/state` is change-deduplicated** — a steady tuner stream must not flood
   the bus.
10. **No secrets in the config file, unit file, or command line** —
    environment file only (§8.3).
11. **SIGTERM is a clean stop** (exit 0), and the bridge must close the tuner
    connection promptly to interrupt the blocking read.
12. **The bridge serializes writes to the tuner and time-bounds them (5 s)** —
    commands never interleave on the wire.

---

## 8. Configuration

Configuration is a TOML file (default path `/etc/atr1k-tuner-bridge/config.toml`,
command-line flag `-config <path>`). Precedence: built-in defaults < TOML file <
environment variables < `-log.level` flag. The bridge silently ignores unknown
TOML keys. A missing file is acceptable (defaults apply). A malformed file is a
startup failure.

**What binds a re-implementation.** The contract is: the key names, the
defaults, the precedence order, the environment-variable names, the secrets
rule (§8.3), and the seeded-file path (§9.1). The TOML file format itself is a
reference-implementation choice. Any format that keeps those keys and values,
and the deploy contract of §9.1, is acceptable.

### 8.1 Keys and defaults

| Key | Default | Meaning |
|-----|---------|---------|
| `host` | `shari` | compute node name, published in `/meta.host`. |
| `tuner.url` | `ws://192.168.1.20:60001` | ATR-1000 binary WebSocket endpoint. |
| `device.model` | `ATR-1000` | identity in `/meta.device.model` and `expose.device.model`. |
| `device.link` | `wifi` | transport label in `/meta.link`. |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | MQTT broker URL. |
| `mqtt.client_id` | `""` ⇒ `muehle-hf-tuner` | MQTT client id, built as `<site>-<station>-<slot>` when empty. |
| `mqtt.site` | `muehle` | slot address part 1 (mandatory, non-empty). |
| `mqtt.station` | `hf` | slot address part 2 (mandatory, non-empty). |
| `mqtt.slot` | `tuner` | slot address part 3. An empty value silently falls back to this default (§4.2). |
| `mqtt.location` | `bauwagen` | physical location label in `/meta`. |
| `mqtt.user` | `hf` | MQTT username. |
| `mqtt.password` | `""` (never set in TOML) | MQTT password — the environment supplies it. |
| `log.level` | `info` | `debug` \| `info` \| `warn` \| `error` |

### 8.2 Environment overrides

All entries are not needed. A non-empty value replaces the TOML value:
`ATR1K_TUNER_BRIDGE_MQTT_BROKER`, `ATR1K_TUNER_BRIDGE_MQTT_CLIENT_ID`,
`ATR1K_TUNER_BRIDGE_MQTT_USER`, `ATR1K_TUNER_BRIDGE_MQTT_PASSWORD`,
`ATR1K_TUNER_BRIDGE_MQTT_SITE`, `ATR1K_TUNER_BRIDGE_MQTT_STATION`,
`ATR1K_TUNER_BRIDGE_MQTT_SLOT`, `ATR1K_TUNER_BRIDGE_TUNER_URL`.

### 8.3 Secrets rule

The bridge loads the MQTT password from the environment variable
`ATR1K_TUNER_BRIDGE_MQTT_PASSWORD`. A service-manager environment file supplies
that variable (`/etc/atr1k-tuner-bridge/atr1k-tuner-bridge.env`, permission
0600, owned by the service user). The password must never appear in the TOML
file, the service unit, or the process command line.

### 8.4 Command-line flags

- `-config <path>` (default path above).
- `-log.level <level>` (case-insensitive) overrides the configured level.
- `-debug` (default false): logs every WebSocket frame as hex (`TX to ATR: …` /
  `RX from ATR: …`).

---

## 9. Deployment

Reference deployment target: the Raspberry Pi **shari** (`192.168.1.139`, SSH
user `io`). Runtime dependencies: outbound TCP to the tuner
(`192.168.1.20:60001`) and the MQTT broker (`192.168.1.50:1883`). Nothing else
— no serial devices, no udev rules, no local state files beyond what the
service manager provides.

### 9.1 Deployment procedure (deploy script behavior)

1. Cross-compile a static binary for the target architecture
    (linux/arm64, no dynamic linking).
2. Generate three artifacts locally: the TOML config seed, the
   environment-file seed, and the service-manager unit. Keep the TOML seed
   password deliberately empty. Keep only the password in the environment-file
   seed. Put both seeds in 0600 temp files.
3. Copy binary, unit, and both seeds to the target host.
4. On the target host:
   - create a dedicated system service user (no login shell, no home) if
     missing.
   - create the config directory (0755, owned by the service user).
   - **seed the config and environment files only if absent** (seed-once: the
     device owns its settings afterwards. Later deploys never touch them).
   - stop the service. Install the binary to
     `/opt/atr1k-tuner-bridge/atr1k-tuner-bridge` (0755). Install the unit.
     Reload the service manager. Enable the service and restart it. Print
     status.

The deploying machine's environment variables can override all deploy-script
defaults (`SSH_HOST`, `SSH_USER`, `SERVICE_NAME`, `SERVICE_USER`,
`INSTALL_DIR`, `BINARY`, plus the seeding vars `TUNER_URL`, `LOCATION`,
`HOST_NAME`, `DEVICE_MODEL`, `DEVICE_LINK`, `LOG_LEVEL`, `MQTT_BROKER`,
`MQTT_SITE`, `MQTT_STATION`, `MQTT_SLOT`, `MQTT_USER`, `MQTT_PASSWORD`).

### 9.2 Service unit (service-manager requirements)

- `Type=simple`.
- `ExecStart=/opt/atr1k-tuner-bridge/atr1k-tuner-bridge -config /etc/atr1k-tuner-bridge/config.toml`.
- `EnvironmentFile=/etc/atr1k-tuner-bridge/atr1k-tuner-bridge.env`.
- `After=network-online.target`, `Wants=network-online.target`.
- `Restart=on-failure`, `RestartSec=5`.
- The service runs as the dedicated user/group. Configuration and state
  directories carry the service name.
- **Isolation intent (normative regardless of service manager)**: a dedicated
  unprivileged user, no new privileges, private /tmp, no capabilities. The
  file system stays protected read-only, except one writable state directory.
  No device access beyond network sockets. Kernel-tuning, module, and
  control-group interfaces stay protected. Address families stay limited to
  IPv4/IPv6. Resource ceilings: 256 MiB memory and 64 tasks. The exact
  systemd directive names are a reference-implementation detail. The
  isolation level is the requirement.
- Logs go to the system journal, identifier `atr1k-tuner-bridge`.

*(Reference-implementation note: the original unit is a systemd unit with
`NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
`PrivateDevices`, `ProtectKernelTunables`, `ProtectKernelModules`,
`ProtectControlGroups`, `RestrictAddressFamilies=AF_INET AF_INET6`,
`RestrictNamespaces`, `LockPersonality`, `RestrictRealtime`, `RestrictSUIDSGID`,
`RemoveIPC`, empty `CapabilityBoundingSet`/`AmbientCapabilities`,
`ReadWritePaths=/var/lib/atr1k-tuner-bridge`, `MemoryMax=256M`, `TasksMax=64`.)*

---

## 10. Known defects and fragilities (as-built — a re-implementation must know these)

1. **README endpoint disagreement (resolved: 192.168.1.20 wins).** The original
   README "Requirements" section says the default tuner endpoint is
   `ws://192.168.1.111:60001`. The code, `config.example.toml`, `deploy.sh`,
   and the README's own config table all say `ws://192.168.1.20:60001`. Code
   is truth. The correct default is `ws://192.168.1.20:60001`. Treat `.111` as
   a README typo.
2. **The bridge publishes SWR raw < 100 undivided.** A raw value of 50 gives
   SWR 50 instead of 0.5. Benign in practice (SWR ≥ 1.0 ⇒ raw ≥ 100 always),
   but a re-implementation must decide the rule explicitly. The historical
   rule is "divide by 100 only when raw ≥ 100". What raw 0 (idle / no RF) must
   render as is an open hardware question (§11).
3. **`inline` is client-side belief only.** The protocol has no inbound frame
   that confirms in-line/bypass (cmd 3 is outbound only, and the decoder ignores
   inbound cmds other than 2 and 5). If the operator toggles the tuner's front
   panel, or the hardware interprets a `tune` without going in line,
   `/state.inline` is wrong until the next `set_inline`. The value also stays
   true across tuner reconnects, regardless of what the tuner actually did
   while the link was down.
4. **Frame length byte unchecked on receive.** The decoder takes the payload
   as everything after byte 3 and ignores byte 2. It relies only on the
   minimum-length checks (≥ 8 Meter, ≥ 10 Relay). A frame with a bogus length
   decodes anyway. A re-implementation must validate the length byte.
5. **Backoff never resets.** The ×1.5 / 60 s-cap backoff grows monotonically
   for the process lifetime and does not reset after a successful dial. A
   long-lived bridge that has seen many tuner dropouts retries slowly (up to
   60 s) even for the next transient blip. Resetting the backoff on success is
   an improvement, not a violation.
6. **No `/state` republish after broker flush.** On MQTT reconnect only
   `/meta` and `/status` get re-published. If the broker lost the retained
   `/state` (for example after a broker restart), the bridge will not
   re-publish it until a dedup-defeating telemetry change happens. A tuner at
   a rock-steady SWR can leave `/state` stale or absent indefinitely. A
   re-implementation must republish `/state` on every MQTT reconnect
   (improvement).
7. **Publish delivery is fire-and-forget.** Nothing waits for QoS-1 delivery
   confirmations. A failed publish (broker hiccup mid-queue) is invisible to
   the bridge.
8. **No WebSocket keepalive/ping.** A half-open tuner TCP link (for example a
   Wi-Fi drop without a TCP reset) blocks the read loop indefinitely.
   `device_online` stays true until the OS errors the socket or a command
   write hits its 5 s deadline. Adding a keepalive or read-idle timeout is an
   improvement a re-implementation must consider (§11).
9. **`tune` timer race window.** The 12 s timer callback runs on its own
   thread and races a Relay frame under the state mutex. The reference
   implementation handles this correctly, but the code merely stops the timer
   — it does not drain it. A just-fired callback can set `fault: "tune
   timeout"` microseconds after a Relay frame cleared settling in a new tune
   cycle. Never observed live.
10. **Shipped API doc lags on `fault`/`error` presence.** The original
    `docs/atr1k-tuner-bridge-mqtt-api.md` shows `"fault": ""` / `"error": ""`.
    The code omits the keys when empty. Consumers must treat absence = empty.
11. **Meter-frame cadence unknown.** The code imposes no read timeout. No
    document in the repo gives the tuner's push rate, or says whether Meter
    frames pause when idle / no RF. A re-implementation must check this
    against hardware.
12. **TuneMode values 0 (Reset) and 3 (Fine) exist in the wire protocol, but
    the bridge does not expose them** over MQTT. The bridge rejects
    `{"action":"tune","value":"fine"}`. If the hardware supports Fine mode,
    the bridge cannot drive it.
13. **Pending-tune stop on link loss is not wired.** The device package holds
    the stop code — `setOnline(false, …)` stops the 12 s timer and forces
    `settling=false` — but nothing calls it. The link-lost path in the entry
    point calls `bridge.SetDeviceOnline(false, …)`, which changes only
    `device_online` and `error`. As-built: a pending tune at link loss leaves
    `settling=true` in the published snapshot. The still-armed timer fires up
    to 12 s later, and its callback pushes the device's cached state, which
    still says `device_online=true` with no `error`. `/state` then shows
    `settling: false`, `fault: "tune timeout"`, and `device_online: true` —
    the tuner looks reachable while it is down. The next dial failure
    re-publishes `device_online: false`.

---

## 11. Open decisions and unresolved facts

Do NOT silently invent a resolution for these. Each needs either on-hardware
verification or a deliberate design decision by the re-implementation team.

1. **Tuner endpoint address.** The README says `ws://192.168.1.111:60001` in
   one place. The code, config example, deploy script, and the README's own
   config table say `ws://192.168.1.20:60001`. Code is truth
   (`192.168.1.20`). But the team must confirm the live device's actual IP on
   the site LAN on-site before the first deploy. Evidence for both variants:
   original repo README.md §Requirements against `config.example.toml`,
   `deploy.sh`, README config table.
2. **Meter-frame push cadence and idle behavior.** This repo cannot tell how
   often the tuner pushes Meter frames. It also cannot tell whether the tuner
   pauses when there is no RF. This decides whether the change-dedup design alone is enough, or
   whether liveness needs an idle-read timeout. Check against hardware.
3. **SWR raw 0 / raw < 100 rendering.** Idle Meter frames can carry SWR raw 0
   (which the historical rule publishes as 0). The team has not decided
   whether raw < 100 needs division, clamping, or treatment as "no reading".
   The
   historical rule ("divide only when ≥ 100") is the as-built behavior.
4. **No keepalive / half-open link detection.** As-built has none (§10.8).
   Open question: does the re-implementation add a WebSocket ping interval or
   a read-idle timeout? §3.1 gives the as-built behavior. Any addition is an
   improvement that must not change the published contract.
5. **`device_online` form: explicit-true against omit-when-true.** The
   deployed bridge publishes `device_online:true` explicitly. The station
   integration-model document says the code can omit the field when true.
   Consumers must treat absence as true either way. The re-implementation must
   pick one form (explicit-true matches all deployed bridges), and the choice
   must stay consistent station-wide (see `02-interface-spec.md`).
6. **Whether the tuner ever echoes cmd 3 (TuneStatus) inbound.** If it does,
   the echo can confirm in-line/bypass and fix the client-side-belief defect
   (§10.3). The as-built decoder deliberately discards all inbound cmds except
   2 and 5. The team needs a hardware capture to decide this.
7. **Backoff reset on success.** As-built never resets the backoff (§10.5). A
   re-implementation can reset it on a successful dial (improvement — keep the
   start ≈ 2 s and cap ≈ 60 s values). Decide explicitly.
8. **`/state` republish on MQTT reconnect.** As-built does not do it (§10.6).
   Republishing on reconnect is an improvement that closes a stale-snapshot
   window. Decide explicitly.
9. **Broker topology.** The as-built default is `tcp://192.168.1.50:1883`
   (production). A migration to a broker on shari (`192.168.1.139`) exists on
   an unmerged feature branch and is not deployed. The broker address must
   stay config-driven. Which broker is production at cutover is a
   station-level decision (see `05-deployment-ops.md`).
10. **Pending-tune stop on link loss.** The reference implementation does not
   follow invariant 6 (§10.13). The stop code exists but is dead, so `/state`
   flips `device_online` back to true while the tuner is down. Recommended:
   follow invariant 6 as written (stop the pending tune and its timer at link
   loss). Decide explicitly.

---

## 12. Reference-implementation notes (non-normative)

The original is a Go service. It uses the gorilla/websocket client for the
tuner link and the Eclipse paho MQTT client library. It also uses the
station's shared runtime library for connection plumbing and the `/cmd`
`value`-key convention. Package layout: `cmd/atr1k-tuner-bridge` (entry, flag parsing,
wiring the `/cmd` queue), `internal/tuner` (device client, protocol codec),
`internal/bridge` (MQTT presence, `/meta` construction, dedup publisher,
command decode), `internal/config` (TOML + environment + flags). Logging is
Go's structured text logging at levels debug/info/warn/error. The warn log is
the *only* command-error channel — there is no error topic and no reply
topic, which is normative (§5.2), not incidental. The paho-specific
goroutine-and-select bridge makes the first connect interruptible by SIGTERM.
The 500 ms quiesce at shutdown is another library workaround. The
*behavior* to keep is the interruptible clean stop (§6.5) — that is the
requirement. None of these library or layout choices bind the
re-implementation. §3–§9 state every normative behavior stack-agnostically.