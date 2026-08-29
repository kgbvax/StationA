# 03-components — ultrabridge: beam antenna controller bridge (Ultrabeam RCU-06)

**Purpose.** This document specifies `ultrabridge`, the software bridge that connects the
station's motorized beam antenna controller — the **Ultrabeam RCU-06** — to the station's MQTT
message bus. Amateur radio ("ham radio") is a licensed hobby radio service in which operators
transmit on allocated **frequency bands** with wavelength-derived names (e.g. "20 m" ≈ 14
MHz). An antenna radiates efficiently only when it is matched to the operating frequency. The
Ultrabeam is a *motorized loading-coil antenna*: several telescoping aluminum elements whose
lengths are adjusted by electric motors so the antenna resonates on the chosen frequency. The
**RCU-06** is the controller box that drives those motors; a computer talks to it over a
serial link through a USB-serial adapter. The bridge (1) polls the RCU-06 every 2 seconds for
its state (tuned frequency, direction, motor activity), (2) publishes that state as a
retained JSON snapshot to MQTT, (3) subscribes to a command topic and executes tuning
commands (set frequency, change direction, jump to a band center, retract the elements), and
(4) serves a small unauthenticated HTTP status/control/debug page. A re-implementation of
this component SHALL satisfy every normative requirement below. Terminology common to all
components (slots, planes, bridge, retained message, last-will, etc.) is defined in
`01-architecture.md` and `02-interface-spec.md`; each term is still briefly redefined at
first use here.

Cross-references: `02-interface-spec.md` (bus schema), `03-components/antennaselect.md`
(the policy service that sends commands to this slot), `03-components/common-runtime-library.md`
(shared MQTT plumbing of the reference implementation), `05-deployment-ops.md` (shari
deployment), `06-safety.md` (antenna grounding and the retract safe state).

---

## 1. Role and place in the station

The station ("Mühle", a German amateur-radio site) is a distributed system of hardware
bridges and logic services that speak MQTT (a lightweight publish/subscribe protocol; a
**broker** receives messages published to named **topics** and forwards them to subscribers).
Each device occupies a **slot address** `<site>/<station>/<slot>`. This bridge implements the
slot `muehle/hf/ant-ctrl` — site `muehle`, station `hf`, role `ant-ctrl` ("antenna
controller"). A **bridge** is a small always-running service that translates between one
physical device and the bus. Each slot exposes four **planes** as topic suffixes: `/meta`
(identity / capabilities), `/state` (current snapshot, retained so late subscribers get it),
`/status` (bridge-process liveness), and `/cmd` (commands in). A **retained message** is one
the broker stores and delivers to every future subscriber immediately, until overwritten or
cleared.

Key role facts a re-implementation SHALL preserve:

1. The slot is the *controller*, not "the antenna". Physical antennas are passive resources
   with no MQTT presence; a separate reconciler service (`antennaselect`) decides antenna
   routing and publishes band-follow commands to this slot (see
   `03-components/antennaselect.md`).
2. The bridge owns **no band-to-element-length model**. It sends only a target frequency and
   a direction byte to the RCU-06, which internally maps that to per-element motor travel.
   The only band logic in the bridge is (a) deriving a human band label from the frequency
   for display and (b) mapping a band label to its recommended center frequency when a
   `band` command arrives (§5.4).
3. Naming rule: beam-direction vocabulary on the bus is `direction`, **never** `mode` — on
   this bus `mode` means the radio emission mode (`cw`/`usb`/`lsb`/…), a different concept.
   A deprecated `mode` command alias SHALL remain accepted for backward compatibility (§6.2).

Deployment facts (normative target): the bridge runs on **shari**, a Raspberry Pi at
192.168.1.139, managed by a systemd service; the MQTT broker is at **192.168.1.50:1883**
(shack broker). A planned migration of components to a broker on shari itself exists but was
NOT deployed as of 2026-08-29 — treat the broker address as a deployment-level open decision
(see `00-system-overview.md` §broker and §12 here).

---

## 2. Upstream interface — the RCU-06 serial protocol

### 2.1 Transport

- **Physical link:** USB-serial adapter, FTDI "Dual RS232" (two ports; the RCU-06 sits on
  interface 0). It appears on the host as `/dev/ttyUSBn`.
- **Serial line parameters (SHALL be):** 19200 baud, 8 data bits, no parity, 1 stop bit
  (8N1), no flow control. Baud is configurable (config key `baud`, default 19200).
- **Port path (SHALL be):** the configured path must be the **stable by-id symlink**
  `/dev/serial/by-id/usb-FTDI_Dual_RS232-if00-port0`. The adapter intermittently drops and
  **re-enumerates under a different tty name** (observed live: ttyUSB0/ttyUSB2 →
  ttyUSB1/ttyUSB3, correlated with whole-USB-bus bounces). The by-id symlink always resolves
  to whichever tty the kernel currently assigns; the self-heal of §2.4 depends on it. A
  configuration pointing at a raw `/dev/ttyUSBn` name is a defect (see §11 item 13).
- The port is opened **without a read timeout**: a read blocks until a byte arrives or the
  link drops; request/response deadlines are enforced by the caller (§2.3).
- **All requests SHALL be strictly serialized** — exactly one request/response exchange in
  flight on the wire at any time (a device-wide lock). The 2-second poll and MQTT/web
  commands interleave but never overlap.

### 2.2 Framing

Byte-oriented link with an HDLC-like frame (HDLC = a classic byte-framing protocol with
start/end flags and byte escaping). Constants:

| Name | Value |
|------|-------|
| STX (start of frame) | `0xF5` |
| DLE (escape byte) | `0xF6` |
| ETX (end of frame) | `0xFA` |
| checksum seed | `0x55` |

**Frame layout:** `STX <escaped payload> ETX`, where the unescaped payload is
`Seq(1 byte) | Com(1 byte) | Data(0..n bytes) | Checksum(1 byte)`.

- **Checksum (SHALL be exactly):** start at `0x55`; for each payload byte `b`:
  `chk = chk XOR b; chk = chk + 1` (mod 256). The result is appended as the last payload
  byte. On receive, the checksum is recomputed over everything except the last byte and the
  frame SHALL be rejected on mismatch.
- **Byte stuffing (SHALL be exactly):** any payload byte equal to STX, ETX, or DLE is
  transmitted as the two bytes `DLE, (b & 0x7F)`. On receive, the byte following a DLE is
  restored as `(b | 0x80)`. Note the asymmetry (encode masks bit 7, decode sets bit 7): the
  round-trip is lossy for payload bytes ≥ 0x80 that collide with a framing constant after
  masking. In practice all payload bytes of this protocol are < 0x80 (see §11 item 8).
- **Minimum frame:** payload ≥ 3 bytes (`Seq | Com | Checksum`); shorter frames are rejected
  ("packet too short").

### 2.3 Commands, replies, and the request/response model

The model is one request frame out, one reply frame back. **Seq** is a bridge-side counter
starting at 0, incremented per request, wrapped to 7 bits (`0..127`). The reply carries the
same Seq; a reply with a mismatched Seq SHALL be discarded and reading continue (this
tolerates stale bytes from a previous timed-out exchange).

Request command bytes:

| Byte | Name | Meaning | Request data | Exchange timeout |
|------|------|---------|--------------|-------------------|
| `0x01` | status_query | read full status | none | 2 s |
| `0x02` | retract | fully retract all elements | none | 5 s |
| `0x03` | change_frequency | tune to frequency + set direction | 3 bytes: freq_kHz LSB, freq_kHz MSB (little-endian uint16), direction byte | 5 s |
| `0x0A` | moving_status | ask how far motors still have to travel | none | 2 s |
| `0x0C` | modify_element_len | directly command element lengths | — | defined but never used by this bridge |

Reply command bytes:

| Byte | Name | Meaning |
|------|------|---------|
| `0x00` | ok | success; Data carries the response payload (status queries) or is empty |
| `0x14` (20) | error | device error |
| `0x1E` (30) | bad_params | request rejected (e.g. change_frequency with < 3 data bytes) |
| `0x28` (40) | invalid_command | unknown command byte |
| `0x40` (64) | debug | unsolicited debug message (treated as "not ok", ignored) |

**status_query reply payload (≥ 12 bytes, else reject "status payload too short"):**

| Offset | Field | Meaning |
|--------|-------|---------|
| 0 | firmware minor | controller firmware version, low byte |
| 1 | firmware major | controller firmware version, high byte |
| 2 | operation | current operation code (not interpreted) |
| 3–4 | frequency kHz | current tuned frequency, little-endian uint16, in kHz |
| 5 | band index | device-internal band index (used only for the fallback band label, §5.4) |
| 6 | orientation | direction byte; the value is the low nibble (`& 0x0F`) |
| 7 | flags1 | not interpreted |
| 8 | flags2 | not interpreted |
| 9 | motor bits | nonzero ⇒ at least one motor is moving |
| 10 | min freq MHz | not interpreted |
| 11 | max freq MHz | not interpreted |

**moving_status reply payload (≥ 4 bytes):**

| Offset | Field | Meaning |
|--------|-------|---------|
| 0–1 | total distance mm | little-endian uint16; **remaining** travel distance in millimeters; 0 ⇒ motors idle |
| 2–3 | progress units | little-endian uint16; parsed, not interpreted |

**Direction byte mapping (SHALL be exactly):**

| Wire byte | Meaning | Bus vocabulary (input aliases also accepted) |
|-----------|---------|-----------------------------------------------|
| `0x00` | normal | `forward` (aliases `""`, `normal`) |
| `0x01` | 180° | `reverse` (alias `180`) |
| `0x02` | bidirectional | `bidirectional` (alias `bidir`) |

Any other input string is rejected with the error `invalid mode "<s>" (expected
forward|reverse|bidirectional)`. Unknown wire values map to the label `unknown`.

**IARU Region 1** = the Europe/Africa/Middle-East amateur allocation plan that defines band
edges and recommended center frequencies; the band-center table in §5.4 uses its mid-band
frequencies.

### 2.4 Connection-loss detection and the EIO self-heal — NORMATIVE REQUIREMENT

This behavior is a hard requirement derived from a live incident, not an optional nicety.

**The incident.** The FTDI adapter randomly disconnects (a USB bus glitch); the kernel
re-enumerates it under a **new tty name**. The long-lived bridge process still held the file
descriptor of the now-deleted device node; every subsequent write failed with `write
request: Input/output error` (EIO — the OS error for a broken device link). Before the fix
the bridge kept accepting MQTT commands but could never actuate the antenna until manually
restarted.

**Requirements (SHALL):**

1. The bridge opens the configured port (the stable by-id path) at startup and retains a
   re-usable *opener* bound to that path and baud, so the path can be re-resolved later.
2. Every exchange runs under the device-wide lock. If the current handle is `nil` (previous
   reopen failed, or adapter absent), the opener is tried first at the start of the
   exchange — the link re-establishes **lazily**, driven by the 2 s poll loop; no separate
   reconnect thread is required.
3. **Write fault:** if writing a request returns an error (EIO etc.), the bridge closes the
   stale handle (best-effort), calls the opener — which **re-resolves the by-id symlink to
   the freshly attached tty** — and, if reopen succeeded, re-sends the request once with a
   **fresh sequence number**.
4. **Read fault:** since the port has no read timeout, a returned read *error* (as opposed
   to absence of data) means a port-level fault (link drop, EOF, port closed). Same
   recovery: close, reopen via the by-id path, re-send once with a fresh Seq.
5. **Retry bound:** at most **one reopen per exchange**. A second fault within the same
   exchange is surfaced to the caller; the next poll tick retries. This prevents a tight
   reopen loop against a persistently broken link.
6. **Failed reopen:** if the adapter is not back yet, the internal handle is set to `nil`,
   the error is surfaced (§5 state fields), and the next exchange (poll tick) attempts the
   reopen again. Net requirement: **recovery within one to two 2-second poll ticks of the
   adapter reappearing, with no manual restart.**
7. **Bus-visible behavior during outage:** `/state` carries `device_online:false` and
   `error:"…"` (exact string examples in §7.3), while `/status` **stays `online`** — the
   bridge process itself is healthy. On the first successful exchange afterwards the state
   is overwritten with fresh device data, `device_online:true`, and the error field
   cleared.

Exact exchange algorithm (per call, all under one lock):

```
if closed -> error "device closed"
if handle == nil: reopen via opener; on failure return "reopen serial: <err>"
loop:
  seq = counter; counter = (counter+1) & 0x7F
  write EncodePacket(seq, com, data)
    on error: if not yet retried and reopen() ok -> mark retried, continue loop
              else return "write request: <err>"
  deadline = now + timeout
  loop:
    if ctx cancelled -> return ctx err
    if now > deadline -> return "timeout waiting for response"
    read one framed packet (byte-at-a-time state machine; STX resyncs, DLE unescapes,
      checksum-verified; a read *error* is a port fault)
      on packet: if seq matches -> return it; else keep reading (foreign/stale packet)
      on read error: if not yet retried and reopen() ok -> mark retried, break to outer
                     loop (re-send with fresh seq)
                   if EOF -> return EOF
                   else return "read response: <err>"
```

Exactness note: the inner read returns a non-nil error also when the caller-level deadline
expires, so **a pure response timeout also triggers exactly one reopen + re-send** before
surfacing `read response: read timeout`. The re-implementation may keep this behavior (it is
bounded and harmless) or deliberately distinguish timeouts from port faults — but it MUST be
a conscious decision, because a *slow* device otherwise causes serial-port churn on every
timeout (see §11 item 2). The 2 s / 5 s timeouts apply to each send attempt.

**Startup is fail-fast only for the first open:** if the port cannot be opened at boot the
process prints the error to stderr and exits 1 (the service manager restarts it every 5 s).

### 2.5 Hardware-less test double (requirement)

A re-implementation SHALL provide an in-process mock device usable when `serial_port` is
empty (or by an equivalent switch), implementing the same framing and command set, with this
exact scripted behavior (it doubles as a conformance script for the framing layer):

- initial state: 14000 kHz, band index 4, direction forward, motors idle, min/max freq
  7/30 MHz;
- `retract` succeeds immediately, reports motors moving for 2000 ms afterwards (then
  clears), and resets frequency to 14000 kHz;
- `change_frequency` with < 3 data bytes replies bad_params; unknown commands reply
  invalid_command.

---

## 3. MQTT presence

**MQTT version:** 3.1.1 over plain TCP. Connection requirements:

- **Clean session = false** (subscriptions and retained delivery survive restarts);
  auto-reconnect enabled.
- **Client ID:** from config `mqtt.client_id`; when unset it SHALL be derived as
  `<site>-<station>-<slot>` → `muehle-hf-ant-ctrl` in production, so a duplicate connection
  is diagnosable on the broker.
- **QoS (Quality of Service)** — MQTT's delivery-guarantee level. **QoS 1** means
  at-least-once delivery with acknowledgment (versus QoS 0, fire-and-forget). This bridge
  uses QoS 1 on every publish and subscribe (§3.1); it is load-bearing for the
  retained-command re-delivery and last-will semantics below, so a re-implementation on a
  different transport SHALL map QoS 1 to an equivalent at-least-once delivery guarantee.
- **Last Will and Testament (LWT)** — a message the broker publishes on the `/status` topic
  if the client dies without disconnecting cleanly: payload `offline`, QoS 1, retained. On
  every successful (re)connect the bridge publishes `online` itself; on a clean shutdown it
  publishes `offline` itself before disconnecting. Caveat (real observed broker behavior,
  station-wide): on a *clean* disconnect the will does not fire, so a retained
  `/status = online` can persist for a stopped service if the bridge's own `offline` publish
  is missed — consumers must never trust `/status` alone (see `02-interface-spec.md`
  §liveness).
- Broker URL, username (`hf` in production), and password from config.
- **Connect must be cancellable** by the shutdown signal: without this, a SIGTERM during a
  broker outage hangs the process until the service manager SIGKILLs it (a live
  station-wide incident — see §10 and `03-components/common-runtime-library.md`).
- On every (re)connect the bridge SHALL: publish `/status` = `online`, publish `/meta`
  (retained), then **re-subscribe** to `/cmd` and `homeassistant/status` (QoS 1 each) in case
  the broker lost session state.

### 3.1 Topics (exact strings)

| Topic | Direction | Retained | QoS | Payload |
|-------|-----------|----------|-----|---------|
| `muehle/hf/ant-ctrl/meta` | bridge → bus | yes | 1 | JSON identity/capabilities document (§3.2) |
| `muehle/hf/ant-ctrl/state` | bridge → bus | yes | 1 | JSON state snapshot (§5) |
| `muehle/hf/ant-ctrl/status` | bridge → bus | yes | 1 | plain string `online` / `offline` |
| `muehle/hf/ant-ctrl/cmd` | bus → bridge | yes | 1 | JSON command (§6) |
| `homeassistant/status` | bridge ← broker | — | 1 | watched for `online` (rebirth signal, §8) |

Slot parts come from config (`mqtt.site`, `mqtt.station`, `mqtt.slot`); default slot
`ant-ctrl`. Production seeds site `muehle`, station `hf`.

### 3.2 `/meta` payload (exact JSON, normative)

Published once per connect cycle, retained. A re-implementation SHALL publish exactly this
shape:

```json
{
  "schema": "1.0",
  "role": "ant-ctrl",
  "device": { "model": "Ultrabeam RCU-06" },
  "link": "serial",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "bands": ["20m", "17m", "15m", "12m", "10m", "6m"],
    "directions": ["forward", "reverse", "bidirectional"]
  },
  "expose": { }
}
```

`location` is omitted when config `location == ""`; `host` omitted when `host == ""`.

**`expose`** is a consumer-neutral field descriptor (no home-automation-specific vocabulary):
a generic discovery service elsewhere in the station (`hadiscovery`,
`03-components/hadiscovery.md`) renders it into home-automation entities. Exact content the
bridge SHALL produce:

```json
"expose": {
  "device": {
    "name": "UltraBeam Antenna",
    "model": "RCU-06",
    "manufacturer": "Ultrabeam"
  },
  "fields": [
    { "key": "freq_hz", "name": "Frequency", "type": "number", "unit": "Hz",
      "class": "frequency", "state_class": "measurement", "writable": true,
      "min": 1800000, "max": 54000000, "step": 1000,
      "command": { "action": "frequency", "value_key": "freq_hz", "value_type": "int" } },
    { "key": "band", "name": "Band", "type": "enum", "options_ref": "bands",
      "writable": true,
      "command": { "action": "band", "value_key": "value", "value_type": "string" } },
    { "key": "direction", "name": "Direction", "type": "enum", "options_ref": "directions",
      "writable": true,
      "command": { "action": "direction", "value_key": "value", "value_type": "string" } },
    { "key": "moving", "name": "Moving", "type": "boolean" },
    { "key": "device_online", "name": "Device online", "type": "boolean" },
    { "key": "error", "name": "Last error", "type": "string" }
  ],
  "actions": [
    { "key": "retract", "name": "Retract", "command": { "action": "retract" } }
  ]
}
```

`options_ref` resolves to `capabilities.bands` / `capabilities.directions` (single source of
truth for enum options). Note the `freq_hz` command descriptor: it is the station's single
documented **exception** to the `/cmd` value-key convention — the normal convention is
`{"action":"<name>","value":"<string>"}` (argument always under the string key `value`), but
the `frequency` action carries its integer argument under the key `freq_hz`, exactly as
declared by this descriptor (see `02-interface-spec.md` §commands).

Known doc lag in the source repo (code is truth): the reference repo's API doc showed an
`"area"` field inside `expose.device`; the code deliberately omits it — the standalone
discovery service supplies a station-wide default area.

---

## 4. Poll loop and cadence

1. **The bridge SHALL poll the RCU-06 every 2 s (default tick):** one status query, then one
   moving-status query, then (if anything changed) publish `/state`.
2. **No periodic `/meta` or `/status` heartbeat exists** — those planes are event-driven
   (connect/disconnect). `/state` change-publishing doubles as the bridge's data heartbeat.
3. **Tick stretching under outage (documented behavior, consumers must not assume a hard
   2 s cadence):** with the device fully absent, each of the two queries can consume up to
   its 2 s timeout (plus one internal reopen+retry each, §2.4), so a dead link yields
   roughly one publish every 4–8 s; missed beats are dropped, not queued.
4. `/state` SHALL be published **only when a value actually changes** (dedup over the state
   fields of §5.1; a bare timestamp change does not trigger a publish). During a device
   outage the error/device_online transitions themselves change the state, so consumers
   watching `/status` + `/state.device_online` retain full visibility.

### Reference-implementation note (non-normative)

The reference is a Go program; the poll loop is a goroutine with a 2 s ticker, and the MQTT
client is the Eclipse Paho Go library. These specifics are incidental — but note they carry
the pitfalls whose *workarounds* are normative: Paho's connect call is not cancellable by a
context (hence the wrapper requirement in §3), and its incoming-message handlers run on the
connection's dispatch thread (hence §6.1's queue requirement). See
`03-components/common-runtime-library.md` for the shared plumbing the reference uses.

---

## 5. `/state` payload (exact contract)

Retained JSON, QoS 1, change-only publishing:

```json
{
  "ts": "2026-07-06T12:34:56Z",
  "freq_hz": 21225000,
  "band": "15m",
  "direction": "forward",
  "moving": false,
  "device_online": true
}
```

### 5.1 Field definitions

| Field | Type | Unit | Semantics (normative) |
|-------|------|------|-----------------------|
| `ts` | string | RFC 3339, UTC | timestamp of this publish |
| `freq_hz` | integer | **Hz** | frequency the beam is tuned to. The device speaks kHz (uint16); the bridge multiplies by 1000. Station-wide bus convention: frequency in Hz as an integer — never kHz or MHz on the bus. |
| `band` | string | — | human band label derived from the kHz value (§5.4); if outside all allocations, `band-<N>` with N = the device-reported band index |
| `direction` | string | — | `forward` \| `reverse` \| `bidirectional` (never called "mode"); `unknown` for unrecognized wire values |
| `moving` | bool | — | `true` while elements physically move. SHALL be `true` when the status-query motor bits ≠ 0 **or** the moving-status remaining distance ≠ 0 |
| `device_online` | bool | — | `true` while the RCU-06 is reachable over serial. **SHALL always be present**, never omitted. Distinguishes "bridge online, device down" from "no data". Bridge liveness is `/status`, never a state field. |
| `error` | string, omitempty | — | last transport-level error message; absent when empty |

### 5.2 Two-layer liveness (normative)

`/status` proves the *bridge process* is alive (via the broker's last-will mechanism);
`/state.device_online` proves the *device behind the bridge* is reachable. Consumers MUST
check both. This bridge always publishes `device_online` explicitly (including `true`). See
`02-interface-spec.md` §liveness for the station-wide contract, including the
absence-equals-true equivalence question listed in §12 here.

### 5.3 Steady-state refresh semantics

Every 2 s tick: the state is replaced wholesale from the status query (frequency, band label,
direction from the orientation low nibble, moving from motor bits, error implicitly cleared);
then the moving-status query recomputes `moving` from remaining distance; then publish. Each
executed command also triggers its own refresh afterwards (§6).

### 5.4 Band tables (exact, normative)

**Band label from device kHz value (upper boundary exclusive):**

| Label | Low kHz | High kHz (exclusive) |
|-------|---------|----------------------|
| `160m` | 1800 | 2000 |
| `80m` | 3500 | 4000 |
| `40m` | 7000 | 7300 |
| `30m` | 10100 | 10150 |
| `20m` | 14000 | 14350 |
| `17m` | 18068 | 18168 |
| `15m` | 21000 | 21450 |
| `12m` | 24890 | 24990 |
| `10m` | 28000 | 29700 |
| `6m` | 50000 | 54000 |
| otherwise | — | `band-<device band index>` |

**Band-center table (IARU Region 1 mid-band frequencies, kHz)** — the frequencies a `band`
command tunes to: `20m`→14175, `17m`→18118, `15m`→21225, `12m`→24940, `10m`→28850,
`6m`→51000. Options list order for UI selects: `["20m","17m","15m","12m","10m","6m"]`.

---

## 6. Command surface

### 6.1 Dispatch model (normative)

`/cmd` payloads are parsed as `{"action": string, "value": string, "freq_hz": int64}`.
Messages that fail JSON parsing or lack an `action` are logged and dropped. **Incoming MQTT
message handlers SHALL never execute device work inline**: each command is enqueued (bounded
queue, capacity 256; **a command is dropped if the queue is full**) and executed **in
arrival order** by a single worker, so commands serialize against each other and against the
poll loop. Rationale: in the reference MQTT library, handlers run on the connection's
dispatch thread, and a handler that blocks on serial I/O or publishes synchronously
deadlocks the client — this happened live in a sibling component. The requirement is
library-independent: isolate handler work from the receive path regardless of stack.

**Retention consequences (SHALL preserve):**

- Every non-retract command stays retained on `/cmd` and is **re-delivered and re-executed
  on every bridge reconnect/restart** — intentional actuator self-healing: the antenna
  re-converges to the last commanded state.
- `retract` is **one-shot**: after attempting it, the bridge publishes an empty retained
  payload to `/cmd`, clearing the retained message so retract never re-fires on restart.
  The clear is **unconditional** — it happens even when the retract attempt itself failed
  (see §6.2 for the exact semantics and why they are safety-relevant).

### 6.2 Actions (normative table)

| Action | Payload | Behavior |
|--------|---------|----------|
| `frequency` | `{"action":"frequency","freq_hz":21225000}` | Convert Hz→kHz by truncating integer division by 1000, keep the current direction, send change_frequency to the device (subject to the 25 kHz deadband, §6.3), then refresh state. NOTE: this is the bus's one documented exception to the `value`-key convention (§3.2). |
| `direction` | `{"action":"direction","value":"reverse"}` | Change direction at the current frequency; value must be one of the direction aliases (§2.3). Always reaches the device (bypasses deadband). |
| `mode` | `{"action":"mode","value":"…"}` | **Deprecated alias** for `direction`; SHALL remain accepted for backward compatibility. |
| `band` | `{"action":"band","value":"15m"}` | Look up the band-center table (§5.4; value is whitespace-trimmed); unknown band ⇒ log `unknown band %q` and drop. Then behaves like `frequency` at that center, keeping the current direction. |
| `retract` | `{"action":"retract"}` | Send retract to the device (5 s timeout), refresh state, then clear the retained `/cmd` topic (§6.1). **The clear is unconditional**: it happens after the retract attempt regardless of outcome — a non-ok device reply, a rejected/invalid invocation, or a transport-level failure all still clear the retained command; the retract result is not checked before clearing. Consequence: a retract that failed does NOT re-fire after a restart. Any rebuild that instead clears only on success MUST flag that as a deliberate divergence, because it changes the retry semantics of the antenna's designated safe-state command (a failed retract would then re-execute on every restart). |
| anything else | — | logged `unknown cmd action=%q`, dropped |

**No acknowledgment channel exists** (a known defect, §11 item 4): command-execution errors
are never published back to the bus — no ack/nack topic. A rejected command (reply not OK)
leaves `/state` untouched and does not set `error`; only transport-level failures flip
`device_online`/`error` (via the next poll). A command sender MUST verify success by
diffing `/state`. If the current frequency is unknown (0 — before the first successful
refresh, or while offline), a `direction` command first triggers a status refresh and, if
still unknown, fails with `current frequency unknown` (logged only).

### 6.3 The 25 kHz deadband (normative behavior contract)

A change_frequency command SHALL be **suppressed** (silently, treated as success) when all
of the following hold:

- the current known frequency is nonzero,
- `|requested − current| < 25 kHz` (a difference of exactly 25 kHz goes through),
- the direction byte equals the current direction.

Rationale: re-driving the motors for a sub-25 kHz retune only wears them and churns the bus;
25 kHz matches the debug UI's nudge step so one nudge still passes. A pure direction change
always bypasses the deadband (a direction flip at the same frequency must reach the device).
When the current frequency is unknown (0), the deadband is skipped so the command always
goes through.

### 6.4 HTTP debug/control API (secondary surface, normative on endpoint semantics)

A small web interface SHALL be served on the configured HTTP address (default
`127.0.0.1:8080`; the production deployment seeds `0.0.0.0:8080`). There is **no
authentication** (a LAN-trust-level surface; see §11 item 9). All modifying endpoints are
POST. Device errors return **502 Bad Gateway** with the error text; invalid input returns
400.

| Endpoint | Method | Body | Behavior |
|----------|--------|------|----------|
| `/` | GET | — | HTML status/control page (live via server-sent events; controls disabled while motors move; frequency input clamped to 1..65535 kHz with ±25 kHz nudge buttons; band buttons POST the band-center kHz values) |
| `/api/status` | GET | — | JSON of internal state: `frequency_khz` (int), `band_name` (string), `band_index` (int), `mode_name` (string), `motors_moving` (bool), `motor_bits` (int), `updated_at` (RFC 3339), `offline` (bool), `last_error` (string, omitted when empty) |
| `/api/events` | GET | — | SSE (server-sent events — a long-lived HTTP stream pushing updates) ; event name `status`, data = same JSON as `/api/status`; sent on every 2 s poll tick unconditionally (unlike MQTT's change-only) plus one immediate event on connect |
| `/api/refresh` | POST | — | one status-query round trip, then broadcast |
| `/api/retract` | POST | — | same as the MQTT retract but clears no retained topic |
| `/api/frequency` | POST | form: `frequency` (kHz, integer 1–65535, required), `mode` (optional; defaults to current direction) | set frequency (deadband applies) |
| `/api/mode` | POST | form: `mode` (required) | set direction only (deadband bypassed) |
| `/api/debug` | GET / POST | POST form `enabled` ∈ {1,true,on} / {0,false,off,empty} | toggle serial-traffic tracing; GET returns `{"enabled": bool}` |
| `/api/debug/events` | GET | — | SSE stream; event `trace`, data JSON `{"at":"15:04:05.000","dir":"tx"\|"rx","name":"status_query"\|"ok"…,"com":<int>,"data":"01 0F …","err":""}` — one tx entry before and one rx entry after each serial exchange, only while tracing is on |

This surface is a human/debug surface and is secondary to the bus contract; the re-implementation's
web UI look, SSE mechanics, and trace format are free to change as long as a status display,
manual frequency/direction/retract control with the same clamps, and a serial-traffic debug
stream exist. (Reference-implementation note: the reference UI is a single Go
HTML/template + SSE page; exact wire format above describes it for ops-tooling compatibility.)

### 6.5 Home Assistant discovery (secondary, off by default)

Two paths exist. The **default** is that this bridge publishes only the `expose` block
(§3.2) and the **separate station service (`hadiscovery`)** renders Home Assistant (an
open-source home-automation platform) discovery from it. A **legacy embedded** discovery —
retained config messages under `homeassistant/<component>/hf-ant-ctrl/<object_id>/config`
(8 entities, value templates like `{{ value_json.freq_hz }}`, availability tied to
`/status`) — still exists in the reference, gated OFF by config
`mqtt.publish_ha_discovery = false`. A clean rebuild MAY drop the embedded path entirely
(keeping `expose`) — in that case the re-publish requirement below does not need
re-implementation: only the `/state` republish and the `homeassistant/status`
subscription remain. But for any rebuild that KEEPS the legacy path (gate on): an
`online` on `homeassistant/status` SHALL trigger re-publication of ALL embedded
discovery config messages (all 8 entity configs) **before** the `/state` republish
attempt — without this, the entities are lost after every Home Assistant restart
(reference-implementation note: the reference does exactly this in its MQTT client's
HA-status handler). In all modes the bridge SHALL subscribe to `homeassistant/status`
and, when it announces `online` (the "rebirth" signal a Home Assistant broker sends after
its own restart), attempt a `/state` republish (it passes through the same change-dedup
check; the retained copy serves subscribers regardless, so this is benign).

---

## 7. Startup, shutdown, and error paths

### 7.1 Startup sequence (exact order, normative)

1. Parse flags; resolve configuration with precedence **explicitly-set flag > config file >
   built-in default**. If the *default* config path is absent, run on defaults; if a path
   given explicitly via `-config` is missing, or any file is malformed, exit 1.
2. Open the serial transport: `serial_port` empty ⇒ in-process mock (§2.5); otherwise open
   the port at `baud`. **First-open failure is fatal** (print to stderr, exit 1).
3. Wrap the transport in the optional serial tracer (no-op unless the debug toggle is on).
4. Construct the controller. The initial in-memory state is the zero value: frequency 0,
   band **empty string**, direction **empty string** (until the first successful refresh).
   The labels `band-<N>` (§5.4) and `unknown` (§2.3) are computed values that appear only
   after a successful refresh — so if the device is unreachable at boot, the first
   published `/state` carries `freq_hz:0`, `band:""`, `direction:""` with
   `device_online:false` and the transport error in `error`, not `band-0`/`unknown`.
5. Start the web server's debug hub; build the HTTP UI.
6. If `mqtt.broker` is configured: connect MQTT (cancellable, §3); **on failure log
   `mqtt disabled: …` and continue without MQTT** rather than exiting. On success: publish
   `/status=online`, `/meta`, bind the `/cmd` + `homeassistant/status` subscriptions.
7. Start the 2 s poll loop (status query, moving status, publish to web + MQTT).
8. Perform one immediate initial refresh (errors logged only, not fatal) and publish the
   first state to web + MQTT.
9. Serve HTTP until shutdown.

### 7.2 Shutdown

SIGINT triggers graceful shutdown: the HTTP server drains (5 s budget); the command worker
stops first; then `/status=offline` is published if connected; then the MQTT connection
closes with a 250 ms grace. **Known defect preserved as fact:** only SIGINT is trapped —
SIGTERM (what systemd sends on `stop`/`restart`) is not, so a service stop kills the process
without the graceful path and the broker fires the LWT `offline`. The re-implementation
SHOULD trap SIGTERM too; either way, the bus-visible outcome must remain a correct
`/status=offline`.

### 7.3 Error-path table (normative)

| Event | State effect | Bus effect | Recovery |
|-------|--------------|------------|----------|
| Serial write/read EIO | `device_online:false`, `error` = raw message | `/state` publish (if changed) | automatic reopen via by-id path (§2.4), within a tick or two of the adapter returning |
| Response timeout (device silent) | first timeout: one silent reopen+retry; second: `device_online:false`, `error:"read response: read timeout"` or `"timeout waiting for response"` | `/state` publish | poll loop keeps retrying |
| Reply not `ok` (error/bad_params/invalid_command/debug) | state **unchanged**; error returned to caller; does not flip `device_online` | none directly | next poll |
| Serial reopen fails (adapter absent) | handle = nil, error surfaced | `/state` publish | lazy retry on every subsequent exchange/poll |
| MQTT connection lost | — | `/status` → `offline` via broker LWT | auto-reconnect; on reconnect full re-init (§3), retained `/cmd` re-delivered ⇒ last command re-applied |
| MQTT connect fails at startup | — | none; bridge runs web-only | none (no retry while running — restart needed) |
| Process crash | — | `/status` → `offline` (LWT) | service manager `Restart=on-failure`, 5 s |

Exact error strings, split by where each can appear (consumers may match on them):

**(a) Strings that CAN appear in `/state.error`.** The error field is populated
exclusively by transport-level failures (§6.2): every `/state.error` value originates in
the exchange layer — a rejected reply or invalid parameter never reaches state.

- `write request: <err>` (e.g. `write request: Input/output error`)
- `read response: <err>` (e.g. `read response: read timeout`)
- `reopen serial: <err>`
- `timeout waiting for response`
- `device closed` (an exchange attempted after the transport was shut down)
- `EOF` (end-of-file on the serial link — link drop)
- a context-cancellation error (shutdown interrupted an in-flight exchange)

**(b) Strings returned to command callers but NEVER stored in `/state.error`.** These
appear in the bridge log and in HTTP 502 response bodies only; a command that fails this
way leaves `/state` untouched (§6.2):

- `status query reply com=<n>`
- `moving status reply com=<n>`
- `retract reply com=<n>`
- `frequency reply com=<n>`
- `status payload too short: have=<n> want>=12`
- `moving payload too short: have=<n> want>=4`
- `controller rejected params`
- `invalid mode "<s>" (expected forward|reverse|bidirectional)`
- `current frequency unknown`

### 7.4 Timing constants (complete normative list)

| Constant | Value |
|----------|-------|
| Poll interval | 2 s |
| status-query / moving-status exchange timeout | 2 s each (per send attempt) |
| retract / change-frequency exchange timeout | 5 s each |
| Frequency deadband | 25 kHz |
| Sequence number space | 0..127, wraps |
| Command queue capacity | 256 (drop when full) |
| MQTT disconnect grace | 250 ms |
| HTTP shutdown budget | 5 s |
| Service-manager restart delay (`RestartSec`) | 5 s |
| Mock retract duration (test double only) | 2000 ms |
| Web SSE channel buffers | 8 (status) / 32 (debug); slow consumers drop events |

---

## 8. Configuration

Single TOML (a config-file format of nested `key = value` tables) file, default path
`/etc/ultrabridge/config.toml` (override with `-config`). Precedence: explicitly-set flag >
file > built-in default. Quirk preserved as fact: a flag explicitly set to its default value
still wins over the file. Missing default-path file ⇒ run on defaults; missing
explicitly-given file or malformed TOML ⇒ fatal.

| Key | Type | Default | Meaning |
|-----|------|---------|---------|
| `http_addr` | string | `127.0.0.1:8080` (deploy seeds `0.0.0.0:8080`) | HTTP listen address |
| `serial_port` | string | `""` ⇒ mock | serial device path; production: `/dev/serial/by-id/usb-FTDI_Dual_RS232-if00-port0` |
| `baud` | int | `19200` | serial baud |
| `location` | string | `""` (deploy seeds `bauwagen`) | deployment fact published in `/meta.location`; omitted when empty |
| `host` | string | `""` (deploy seeds `shari`) | compute node name published in `/meta.host`; omitted when empty |
| `mqtt.broker` | string | `""` ⇒ MQTT disabled | broker URL, e.g. `tcp://192.168.1.50:1883` |
| `mqtt.client_id` | string | `""` ⇒ derived `muehle-hf-ant-ctrl` | MQTT client ID |
| `mqtt.site` | string | `""` (deploy seeds `muehle`) | topic prefix part 1; empty site or station at MQTT init ⇒ error, MQTT disabled |
| `mqtt.station` | string | `""` (deploy seeds `hf`) | topic prefix part 2 |
| `mqtt.slot` | string | `ant-ctrl` | topic prefix part 3 (canonical role) |
| `mqtt.discovery_prefix` | string | `homeassistant` | HA discovery tree prefix (legacy embedded path only) |
| `mqtt.publish_ha_discovery` | bool | `false` | gate for legacy embedded HA discovery |
| `mqtt.user` | string | `""` | MQTT username (production `hf`) |
| `mqtt.password` | string | `""` | **secret**, stored in this file |

Flags (only those explicitly given override the file): `-config`, `-http`, `-port`, `-baud`,
`-mqtt-broker`, `-mqtt-client-id`, `-mqtt-user`, `-mqtt-password`.

**Secret handling (normative):** the MQTT password lives only in the TOML file, which the
deployer installs as `0600` (owner read/write only) owned by the service user; it never
appears in the service unit, the ExecStart command line, or the process table. There is no
environment-variable fallback. See `docs/conventions/config-and-secrets.md` equivalents in
`02-interface-spec.md` §config.

---

## 9. Deployment

- **Target:** shari, Raspberry Pi, `io@192.168.1.139`, Linux arm64.
- **Install procedure (SHALL be reproduced by the deploy tooling):**
  1. Cross-compile a static binary for `linux/arm64` (reference flags:
     `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`); copy the
     binary, a generated systemd unit, and a seed config (temp file written under
     `umask 077`) to the target.
  2. Create system user/group `ultrabridge` (nologin, no home) if missing; ensure group
     `dialout` exists and add the service user to it.
  3. Install a **udev rule** `/etc/udev/rules.d/99-ultrabridge-serial.rules`:
     `SUBSYSTEM=="tty", SUBSYSTEMS=="usb", ATTRS{idVendor}=="0403", GROUP="dialout", MODE="0660"`
     (FTDI USB-serial vendor id 0403) so the serial device is always group-accessible
     regardless of distro default (plugdev vs dialout).
  4. Create `/opt/ultrabridge` (binary directory) 0755, **root-owned**, with the binary
     installed there root-owned mode 755; create `/etc/ultrabridge` (config directory)
     0755, owned by the service user (owner and group both the service user). The
     unprivileged runtime account SHALL NOT own the directory containing the executable
     it runs.
  5. **Seed-once config:** install `/etc/ultrabridge/config.toml` (0600, service-user owned)
     **only if it does not already exist**; later deploys never touch it. The device owns
     its settings; edit on the device, or delete the file to re-seed.
  6. Install the unit, `daemon-reload`, enable, restart, print status.
- **Service unit essentials (exact):** `After/Wants=network-online.target`; `Type=simple`;
  `ExecStart=/opt/ultrabridge/ultrabridge -config /etc/ultrabridge/config.toml` (no other
  arguments — everything comes from the file); `Restart=on-failure`, `RestartSec=5`;
  `User=Group=ultrabridge`, `SupplementaryGroups=dialout`;
  `ConfigurationDirectory=ultrabridge`; hardening: `NoNewPrivileges=true`,
  `ProtectSystem=full`, `ProtectHome=true`, `PrivateTmp=true`; device access:
  `DeviceAllow=char-ttyUSB rw`, `DeviceAllow=char-ttyACM rw`, `DeviceAllow=char-tty rw`.
- **Runtime dependencies:** the MQTT broker at `192.168.1.50:1883` (user `hf`; see the
  broker-topology open decision in §12); the FTDI adapter on USB; network reachability. The
  HTTP UI listens on all interfaces at port 8080 in production.
- **Operations:** logs via the journal (`journalctl -u ultrabridge -f`); restart via
  `systemctl restart ultrabridge`. See `05-deployment-ops.md` for the station-wide runbook.

### Reference-implementation note (non-normative)

The reference is written in Go, uses `go.bug.st/serial` for the serial link and Eclipse Paho
for MQTT, and is deployed with a `deploy.sh` script (plus a Windows build in `build.sh` for
developer convenience only). These are incidental choices; every behavior in this document
is specified independently of them. What is NOT incidental: that library's blocking serial
reads and non-cancellable connect motivated the cancellation and timeout requirements in
§2.4/§3, and Paho's dispatch-thread handler execution motivated §6.1 — the re-implementation
must reproduce the *workarounds* whatever stack it uses.

---

## 10. Invariants and safety rules (SHALL never be violated)

1. **Antenna commands are serialized.** Exactly one serial exchange in flight at any time
   (device-wide lock); poll queries and commands interleave but never overlap on the wire.
2. **`freq_hz` on the bus is Hz, integer.** The device speaks kHz (uint16); the ×1000
   conversion happens only inside the bridge. Never publish kHz/MHz.
3. **`direction` is never named `mode` on the bus.** Bus `mode` is the radio emission-mode
   vocabulary; the deprecated `mode` *command alias* must remain accepted.
4. **`device_online` is always present in `/state`** — consumers must distinguish "bridge
   online, device down" from "no data". Bridge liveness is `/status` (LWT) only.
5. **`retract` is one-shot:** after the retract attempt the retained `/cmd` is cleared
   (empty retained publish) — unconditionally, even if the attempt failed (§6.2) — so it
   cannot re-fire on restart. All other commands stay retained and are
   re-applied on reconnect — intentional actuator self-healing.
6. **Retract is the antenna's safe state** (elements fully in). External safety logic (the
   `antennaselect` reconciler, `06-safety.md`) drives grounding/antenna selection; this
   bridge merely executes `retract` faithfully with a 5 s timeout and verifies the reply is
   `ok`.
7. **A dead device must never take the bridge down.** Serial errors flip
   `device_online:false` and surface in `error`; the process stays up, keeps polling, keeps
   serving MQTT/web, and self-heals (§2.4).
8. **At most one reopen+retry per exchange** — no unbounded retry loops against a broken
   link; the 2 s poll is the recovery driver.
9. **The 25 kHz deadband** (§6.3) with the direction-change and unknown-frequency bypasses,
   exactly as specified.
10. **MQTT handlers must never block** on serial I/O or a blocking publish inside the
    client's dispatch callback; all command work is serialized through a bounded worker
    queue, executed in arrival order.
11. **Connect must be cancellable** (a shutdown signal interrupts a hung MQTT connect).
12. **Secrets never on the command line** — password only in the 0600 TOML file.
13. **Checksum/framing errors** on received packets are rejected; stale-sequence replies are
    discarded rather than misattributed to a new request.
14. **Frequency is bounded to the uint16 kHz domain** (max 65535 kHz = 65.535 MHz); the HTTP
    layer enforces 1..65535 kHz. The MQTT path currently relies on truncation and MUST gain
    an explicit range check in any re-implementation (see §11 item 3).

---

## 11. Known defects and fragilities (facts about the reference, to fix or consciously keep)

1. **SIGTERM is not handled** — only SIGINT is trapped. Every `systemctl stop/restart`
   (SIGTERM) kills the process without the graceful path: no self-published `offline`, no
   HTTP drain. The broker LWT covers `/status=offline`, so bus consumers see no difference,
   but the graceful-shutdown code is effectively dead in production. A rebuild should trap
   SIGTERM.
2. **A response timeout also triggers the reopen+retry path** (§2.4 exactness note),
   contradicting an in-code comment. Harmless (bounded to one retry) but a reimplementation
   should decide deliberately: a *slow* device causes serial-port churn on every timeout.
3. **`freq_hz` truncation on MQTT `frequency` commands:** `khz := uint16(cmd.FreqHz / 1000)` —
   values above 65.535 MHz wrap modulo 65536 (e.g. 71 MHz → 5.6 MHz sent) and sub-kHz
   precision is silently dropped. No range validation on the MQTT path (unlike the web
   path's 1..65535 check). A rebuild must add the range check.
4. **No command acknowledgment channel.** Invalid direction, unknown band, rejected params,
   or timeouts on a `/cmd` are only logged. A rejected command does not set
   `/state.error`; only transport failures do. A command sender must diff `/state`.
5. **Queue drop semantics:** if more than 256 commands are pending, new ones are silently
   dropped; a dropped command is invisible. On shutdown, queued-but-unexecuted jobs are
   lost — for a lost *retract* job the retained `/cmd` was not cleared, so the retract
   re-executes on the next start.
6. **Poll-tick stretching** (§4 item 3): consumers must not assume a hard 2 s cadence.
7. **Parsed-but-unused status fields:** per-motor bits, min/max freq, firmware version,
   flags, progress units never reach the bus — a single stuck motor only shows as generic
   `moving:true` or device silence.
8. **DLE-escape asymmetry** (`b & 0x7F` encode vs `b | 0x80` decode) cannot round-trip
   payload bytes ≥ 0x80 that collide with framing constants. Irrelevant for current
   traffic; a trap if the unused `modify_element_len` (0x0C) or `debug` payloads ever carry
   high-bit bytes.
9. **Web UI has no authentication** and production binds `0.0.0.0:8080` — anyone on the LAN
   can move the antenna via `/api/*`. Same trust level as the rest of the shack LAN, but a
   security-sensitive rebuild must at least document or gate it.
10. **Doc lags in the source repo (code is truth):** the API doc's `/meta` example shows
    `"area": "Radio shack"` in `expose.device` (code omits it); the README's MQTT example
    uses user `ham` (production user is `hf`); the API doc's `/api/status` example omits
    `band_index`, `motor_bits`, `last_error` which the endpoint returns.
11. **Dead protocol surface:** command `0x0C modify_element_len` and the error value
    `unsupported write command` are declared but never used; `ReplyDebug` (0x40) is defined
    but only ever treated as "not ok".
12. **Initial-refresh race:** the poll loop starts before the one-shot initial refresh;
    under slow startup the first tick can interleave. Harmless (lock-guarded).
13. **FTDI re-enumeration root cause is environmental** (USB bus glitches correlate with MQTT
    dropouts on the same host). The self-heal masks it; it does not fix the flaky USB. The
    by-id path is load-bearing — a config pointing at `/dev/ttyUSB0` would break self-heal
    (the reopen would reopen the *name*, which may no longer exist or may name the other
    port of the dual adapter).

---

## 12. Open decisions and unresolved facts

1. **Ultrabeam switch port (station wiring, affects the ecosystem around this bridge).** The
   repo-root documentation and station-integration docs say the Ultrabeam beam is on
   antenna-switch port 3; the antennaselect seeded config and the console's antenna map say
   port 4. The live config on shari is authoritative but was not readable when this PRD was
   written. This bridge itself is agnostic (it tunes the antenna; the switch routes it), but
   any re-integration must confirm the port on-device. See
   `03-components/antennaselect.md` §open-decisions and `00-system-overview.md`.
2. **Broker topology.** Deployed config points at the shack broker 192.168.1.50:1883. A
   migration to a broker on shari (192.168.1.139) exists on an unmerged feature branch,
   committed but NOT deployed as of 2026-08-29. The re-implementation must treat the broker
   URL as configuration and confirm the production target at deploy time. (§3, §8.)
3. **`device_online` published form.** The integration-model doc says `device_online` may be
   "omitted when true"; this bridge (like all deployed bridges) publishes
   `device_online:true` explicitly. The re-implementation should mandate explicit-true
   (this component's contract does: §5.1) — station-wide, the equivalence question is
   tracked in `02-interface-spec.md` §liveness.
4. **Live configuration values on shari** — actual `serial_port` string, MQTT credentials,
   whether any consumer currently relies on the legacy embedded HA discovery (§6.5), and
   whether anything still sends the deprecated `mode` command alias — are not recorded in
   the repo and must be verified with the operator before freezing the re-build. The
   research spec assumes the seeded values (`bauwagen`, `shari`, user `hf`, by-id serial
   path) but could not read the live `/etc/ultrabridge/config.toml`.
5. **Does the RCU-06 firmware ever emit unsolicited frames?** The frame reader resyncs on
   STX so it tolerates them, but there is no evidence either way. The re-implementation
   should preserve STX-resync behavior (it costs nothing and tolerates unsolicited debug
   frames).
6. **Real-world retract duration** and whether the 5 s retract exchange timeout is ever
   exceeded in the field (the mock simulates 2 s of motor motion; the repo contains no field
   data). If real retracts take longer than 5 s to *acknowledge* (as opposed to complete),
   the timeout needs retuning — the device's motors keep moving after the `ok` reply, and
   the 2 s poll's `moving` flag is the completion signal.
7. **Timeout-vs-fault discrimination** (§2.4 exactness note / §11 item 2): the reference
   treats a response timeout as a port fault (one reopen+retry). Keep this or distinguish
   timeouts from EIO deliberately — unresolved; the current behavior is harmless but
   causes port churn with a merely slow device.
8. **`frequency` command range validation on the MQTT path** — the reference truncates and
   wraps (§11 item 3). This PRD requires an explicit 0 < freq_hz ≤ 65535000 check in a
   re-implementation (§10 item 14); whether out-of-range commands should be silently
   dropped or surfaced somehow (there is no ack channel, §11 item 4) is an open design
   decision coupled to the missing-acknowledgment defect.