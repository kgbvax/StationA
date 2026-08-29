# 03-components — ultrabridge: beam antenna controller bridge (Ultrabeam RCU-06)

**Purpose.** This document gives the details of `ultrabridge`, the bridge that connects the
station's motorized beam antenna controller to the MQTT message bus. The controller is the
**Ultrabeam RCU-06**. Amateur radio ("ham radio") is a licensed hobby radio service in which
operators transmit on allocated **frequency bands** with wavelength-derived names. For
example, the "20 m" band sits near 14 MHz. An antenna radiates efficiently only when it
matches the operating frequency. The Ultrabeam is a *motorized loading-coil antenna*.
Several telescoping aluminum elements form it. Electric motors adjust their lengths so the
antenna resonates on the chosen frequency. The **RCU-06** is the controller box that drives
those motors. A computer talks to it over a serial link through a USB-serial adapter. The
bridge (1) polls the RCU-06 every 2 seconds for its state (tuned frequency, direction, motor
activity). (2) It publishes that state as a retained JSON snapshot to the bus. (3) It
subscribes to a command topic and does the tuning commands: set frequency, change direction,
jump to a band center, retract the elements. (4) It serves a small unauthenticated HTTP
status, control, and debug page. A re-implementation of this component must satisfy
every normative requirement below. `01-architecture.md` and `02-interface-spec.md` define the terminology
common to all components (slots, planes, bridge, retained message, last-will, and so on). Each term still carries a brief definition at first use here.

Cross-references: `02-interface-spec.md` (bus schema), `03-components/antennaselect.md`
(the policy service that sends commands to this slot), `03-components/common-runtime-library.md`
(shared MQTT plumbing of the reference implementation), `05-deployment-ops.md` (shari
deployment), `06-safety.md` (antenna grounding and the retract safe state).

---

## 1. Role and place in the station

The station ("Mühle", a German amateur-radio site) is a distributed system of hardware
bridges and logic services. They speak MQTT (a lightweight publish/subscribe protocol). A
**broker** receives messages published to named **topics** and forwards them to subscribers.
Each device occupies a **slot address** `<site>/<station>/<slot>`. This bridge implements the
slot `muehle/hf/ant-ctrl`: site `muehle`, station `hf`, role `ant-ctrl` ("antenna
controller"). A **bridge** is a small always-running service that translates between one
physical device and the bus. Each slot exposes four **planes** as topic suffixes: `/meta`
(identity / capabilities), `/state` (current snapshot, retained so late subscribers get it),
`/status` (bridge-process liveness), and `/cmd` (commands in). A **retained message** is one
the broker stores and delivers to every future subscriber immediately, until overwritten or
cleared.

Key role facts a re-implementation must preserve:

1. The slot is the *controller*, not "the antenna". Physical antennas are passive resources
   with no MQTT presence. A separate reconciler service (`antennaselect`) decides antenna
   routing and publishes band-follow commands to this slot (see
   `03-components/antennaselect.md`).
2. The bridge owns **no band-to-element-length model**. It sends only a target frequency and
   a direction byte to the RCU-06. The device maps those internally to per-element motor
   travel. The bridge carries only this band logic. It derives (a) a human band label
   from the frequency for display. It maps (b) a band label to its recommended center
   frequency when a `band` command arrives (§5.4).
3. Naming rule: beam-direction vocabulary on the bus is `direction`, **never** `mode` — on
   this bus `mode` means the radio emission mode (`cw`/`usb`/`lsb`/…), a different concept.
   The deprecated `mode` command alias must stay accepted for backward compatibility (§6.2).

Deployment facts (normative target): the bridge runs on **shari**, a Raspberry Pi at
192.168.1.139, managed by a systemd service. The MQTT broker is at **192.168.1.50:1883**
(shack broker). A planned migration of components to a broker on shari itself exists. The
station had not deployed it as of 2026-08-29. Treat the broker address as a
deployment-level open decision (see `00-system-overview.md` §7 and §12 here).

---

## 2. Upstream interface — the RCU-06 serial protocol

### 2.1 Transport

- **Physical link:** USB-serial adapter, FTDI "Dual RS232" (two ports, the RCU-06 sits on
  interface 0). It appears on the host as `/dev/ttyUSBn`.
- **Serial line parameters (must be):** 19200 baud, 8 data bits, no parity, 1 stop bit
  (8N1), no flow control. Baud is configurable (config key `baud`, default 19200).
- **Port path (must be):** the configured path must be the **stable by-id symlink**
  `/dev/serial/by-id/usb-FTDI_Dual_RS232-if00-port0`. The adapter intermittently drops and
  **re-enumerates under a different tty name** (observed live: ttyUSB0/ttyUSB2 →
  ttyUSB1/ttyUSB3, correlated with whole-USB-bus bounces). The by-id symlink always resolves
  to the tty the kernel assigns at that moment. The self-heal of §2.4 depends on it. A
  configuration pointing at a raw `/dev/ttyUSBn` name is a defect (see §11 item 13).
- The bridge opens the port **without a read timeout**: a read blocks until a byte arrives
  or the link drops. The caller enforces request and response deadlines (§2.3).
- **The bridge must strictly serialize all requests** — exactly one request/response
  exchange in flight on the wire at any time (a device-wide lock). The 2-second poll and
  MQTT/web commands interleave but never overlap.

### 2.2 Framing

Byte-oriented link with an HDLC-like frame (HDLC = a classic byte-framing protocol with
start/end flags and byte escaping). Constants:

| Name | Value |
|------|-------|
| STX (start of frame) | `0xF5`. |
| DLE (escape byte) | `0xF6`. |
| ETX (end of frame) | `0xFA`. |
| checksum seed | `0x55`. |

**Frame layout:** `STX <escaped payload> ETX`, where the unescaped payload is
`Seq(1 byte) | Com(1 byte) | Data(0..n bytes) | Checksum(1 byte)`.

- **Checksum (must be exactly):** start at `0x55`. For each payload byte `b`:
  `chk = chk XOR b; chk = chk + 1` (mod 256). The bridge appends the result as the last
  payload byte. On receive, the bridge recomputes the checksum over everything except the
  last byte. A mismatch rejects the frame.
- **Byte stuffing (must be exactly):** the bridge sends any payload byte equal to STX, ETX,
  or DLE as the two bytes `DLE, (b & 0x7F)`. On receive, the bridge restores the byte
  following a DLE as `(b | 0x80)`. Note the asymmetry (encode masks bit 7, decode sets bit
  7). The round-trip loses payload bytes ≥ 0x80 that collide with a framing constant after
  masking. In practice all payload bytes of this protocol are < 0x80 (see §11 item 8).
- **Minimum frame:** the payload holds ≥ 3 bytes (`Seq | Com | Checksum`). The bridge
  rejects shorter frames ("packet too short").

### 2.3 Commands, replies, and the request/response model

The model is one request frame out, one reply frame back. **Seq** is a bridge-side counter
starting at 0, incremented per request, wrapped to 7 bits (`0..127`). The reply carries the
same Seq. The bridge discards a reply with a mismatched Seq and keeps reading (this
tolerates stale bytes from a previous timed-out exchange).

Request command bytes:

| Byte | Name | Meaning | Request data | Exchange timeout |
|------|------|---------|--------------|-------------------|
| `0x01` | status_query | read full status. | none. | 2 s. |
| `0x02` | retract | fully retract all elements. | none. | 5 s. |
| `0x03` | change_frequency | tune to frequency + set direction. | 3 bytes: freq_kHz LSB, freq_kHz MSB (little-endian uint16), direction byte. | 5 s. |
| `0x0A` | moving_status | ask how far motors still have to travel. | none. | 2 s. |
| `0x0C` | `modify_element_len` | directly command element lengths. | — | defined but this bridge never uses it. |

Reply command bytes:

| Byte | Name | Meaning |
|------|------|---------|
| `0x00` | ok | success. Data carries the response payload (status queries) or stays empty. |
| `0x14` (20) | error | device error. |
| `0x1E` (30) | bad_params | request rejected (for example `change_frequency` with < 3 data bytes). |
| `0x28` (40) | invalid_command | unknown command byte. |
| `0x40` (64) | debug | unsolicited debug message (treated as "not ok", ignored). |

**status_query reply payload (≥ 12 bytes, else reject "status payload too short"):**

| Offset | Field | Meaning |
|--------|-------|---------|
| 0 | firmware minor | controller firmware version, low byte. |
| 1 | firmware major | controller firmware version, high byte. |
| 2 | operation | current operation code (not interpreted). |
| 3–4 | frequency kHz | current tuned frequency, little-endian uint16, in kHz. |
| 5 | band index | device-internal band index (used only for the fallback band label, §5.4). |
| 6 | orientation | direction byte. The value is the low nibble (`& 0x0F`). |
| 7 | flags1 | not interpreted. |
| 8 | flags2 | not interpreted. |
| 9 | motor bits | nonzero ⇒ at least one motor moves. |
| 10 | min freq MHz | not interpreted. |
| 11 | max freq MHz | not interpreted. |

**moving_status reply payload (≥ 4 bytes):**

| Offset | Field | Meaning |
|--------|-------|---------|
| 0–1 | total distance mm | little-endian uint16. **Travel distance still left**, in millimeters. 0 ⇒ motors idle. |
| 2–3 | progress units | little-endian uint16. Parsed, not interpreted. |

**Direction byte mapping (must be exactly):**

| Wire byte | Meaning | Bus vocabulary (input aliases also accepted) |
|-----------|---------|-----------------------------------------------|
| `0x00` | normal | `forward` (aliases `""`, `normal`). |
| `0x01` | 180° | `reverse` (alias `180`). |
| `0x02` | bidirectional | `bidirectional` (alias `bidir`). |

The bridge rejects any other input string with the error `invalid mode "<s>" (expected
forward|reverse|bidirectional)`. Unknown wire values map to the label `unknown`.

**IARU Region 1** = the Europe/Africa/Middle-East amateur allocation plan. It defines band
edges and recommended center frequencies. The band-center table in §5.4 uses its mid-band
frequencies.

### 2.4 Connection-loss detection and the EIO self-heal — NORMATIVE REQUIREMENT

This behavior is a hard requirement distilled from a live incident. It is not a nice-to-have.

**The incident.** The FTDI adapter randomly disconnects (a USB bus glitch). The kernel
re-enumerates it under a **new tty name**. The long-lived bridge process still held the file
descriptor of the now-deleted device node. Every later write then failed with `write
request: Input/output error` (EIO — the OS error for a broken device link). Before the fix
the bridge kept accepting MQTT commands but was never able to actuate the antenna until
someone restarted it manually.

**Requirements (must hold):**

1. The bridge opens the configured port (the stable by-id path) at startup. It retains a
   re-usable *opener* bound to that path and baud. Later code can re-resolve the path
   through that opener.
2. Every exchange runs under the device-wide lock. If the current handle is `nil` (previous
   reopen failed, or adapter absent), the bridge tries the opener first at the start of the
   exchange. The link then re-establishes **lazily**, driven by the 2 s poll loop. The
   design needs no separate reconnect thread.
3. **Write fault:** if writing a request returns an error (EIO and so on), the bridge closes
   the stale handle (best-effort). It calls the opener — which **re-resolves the by-id
   symlink to the freshly attached tty**. If reopen succeeded, it re-sends the request once
   with a **fresh sequence number**.
4. **Read fault:** the port has no read timeout, so a returned read *error* means a
   port-level fault (link drop, EOF, port closed). This differs from mere absence of data.
   Same recovery: close, reopen through the by-id path, re-send once with a fresh Seq.
5. **Retry bound:** at most **one reopen per exchange**. A second fault within the same
   exchange goes to the caller. The next poll tick retries. This stops a tight reopen loop
   against a persistently broken link.
6. **Failed reopen:** if the adapter is not back yet, the bridge sets the internal handle to
   `nil` and surfaces the error (§5 state fields). The next exchange (poll tick) tries the
   reopen again. Net requirement: **recovery within one to two 2-second poll ticks of the
   adapter reappearing, with no manual restart**.
7. **Bus-visible behavior during outage:** `/state` carries `device_online:false` and
   `error:"…"` (exact string examples in §7.3), while `/status` **stays `online`**.
   The bridge process itself is healthy. On the first successful exchange afterwards the
   bridge overwrites the state with fresh device data, sets `device_online:true`, and clears
   the error field.Exact exchange algorithm (per call, all under one lock):

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
expires. So **a pure response timeout also triggers exactly one reopen + re-send** before
the bridge surfaces `read response: read timeout`. The re-implementation can keep this
behavior — bounded and harmless — or deliberately distinguish timeouts from port faults.
But the choice must be a conscious one, because a *slow* device otherwise causes serial-port
churn on every timeout (see §11 item 2). The 2 s / 5 s timeouts apply to each send try.

**Startup is fail-fast only for the first open**. If the bridge cannot open the port at
boot, the process prints the error to stderr and exits 1. The service manager restarts it
every 5 s.

### 2.5 Hardware-less test double (requirement)

A re-implementation must provide an in-process mock device usable when `serial_port` is
empty (or by a same-behavior switch). It implements the same framing and command set, with
this exact scripted behavior (it doubles as a conformance script for the framing layer):

- first state: 14000 kHz, band index 4, direction forward, motors idle, min/max freq
  7/30 MHz.
- `retract` succeeds immediately and reports motors moving for 2000 ms afterwards (then
  clears), and resets frequency to 14000 kHz.
- `change_frequency` with < 3 data bytes replies bad_params. Unknown commands reply
  invalid_command.

---

## 3. MQTT presence

**MQTT version:** 3.1.1 over plain TCP. Connection requirements:

- **Clean session = false** (subscriptions and retained delivery survive restarts).
  The client enables auto-reconnect.
- **Client ID:** from config `mqtt.client_id`. When unset, it must derive as
  `<site>-<station>-<slot>` → `muehle-hf-ant-ctrl` in production, so a repeated client ID
  stays diagnosable on the broker.
- **QoS (Quality of Service)** — MQTT's delivery level. **QoS 1** means
  at-least-once delivery with acknowledgment (versus QoS 0, fire-and-forget). This bridge
  uses QoS 1 on every publish and subscribe (§3.1). That choice is load-bearing for the
  retained-command re-delivery and last-will semantics below. So a re-implementation on a
  different transport must map QoS 1 to the same at-least-once delivery level.
- **Last Will and Testament (LWT)** — a message the broker publishes on the `/status` topic
  if the client dies without a clean disconnect: payload `offline`, QoS 1, retained. On
  every successful (re)connect the bridge itself publishes `online`. On a clean shutdown it
  publishes `offline` before disconnecting. Caveat (real observed broker behavior,
  station-wide): on a *clean* disconnect the will does not fire. A retained
  `/status = online` can then persist for a stopped service if the bridge misses its own
  `offline` publish. Consumers must never trust `/status` alone (see
  `02-interface-spec.md` §5).
- Broker URL, username (`hf` in production), and password come from config.
- **Connect must be cancellable** by the shutdown signal. Without this, a SIGTERM during
  a broker outage hangs the process until the service manager SIGKILLs it (a live
  station-wide incident — see §10 and `03-components/common-runtime-library.md`).
- On every (re)connect the bridge must: publish `/status` = `online`, publish `/meta`
  (retained), then **re-subscribe** to `/cmd` and `homeassistant/status` (QoS 1 each) in
  case the broker lost session state.

### 3.1 Topics (exact strings)

| Topic | Direction | Retained | QoS | Payload |
|-------|-----------|----------|-----|---------|
| `muehle/hf/ant-ctrl/meta` | bridge → bus | yes | 1 | JSON identity/capabilities document (§3.2). |
| `muehle/hf/ant-ctrl/state` | bridge → bus | yes | 1 | JSON state snapshot (§5). |
| `muehle/hf/ant-ctrl/status` | bridge → bus | yes | 1 | plain string `online` / `offline`. |
| `muehle/hf/ant-ctrl/cmd` | bus → bridge | yes | 1 | JSON command (§6). |
| `homeassistant/status` | bridge ← broker | — | 1 | watched for `online` (rebirth signal, §8). |

Slot parts come from config (`mqtt.site`, `mqtt.station`, `mqtt.slot`). The default slot
is `ant-ctrl`. Production seeds site `muehle`, station `hf`.

### 3.2 `/meta` payload (exact JSON, normative)

The bridge publishes this once per connect cycle, retained. A re-implementation must
publish exactly this shape:

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

The payload omits `location` when config `location == ""`. It omits `host` when
`host == ""`.

**`expose`** is a consumer-neutral field descriptor (no home-automation-specific vocabulary):
a generic discovery service elsewhere in the station (`hadiscovery`,
`03-components/hadiscovery.md`) renders it into home-automation entities. The bridge must
produce this exact content:

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
documented **exception** to the `/cmd` value-key convention. The normal convention is
`{"action":"<name>","value":"<string>"}` (argument always under the string key `value`), but
the `frequency` action carries its integer argument under the key `freq_hz`, exactly as
this descriptor declares it (see `02-interface-spec.md` §commands).

Known doc lag in the source repo (code is truth): the reference repo's API doc showed an
`"area"` field inside `expose.device`. The code deliberately omits it. The standalone
discovery service supplies a station-wide default area.
---

## 4. Poll loop and cadence

1. **The bridge must poll the RCU-06 every 2 s (default tick):** one status query, then one
   moving-status query, then (if anything changed) publish `/state`.
2. **No periodic `/meta` or `/status` heartbeat exists** — those planes run on events
   (connect/disconnect). The `/state` change-publishing doubles as the bridge's data
   heartbeat.
3. **Tick stretching under outage (documented behavior — consumers must not assume a hard
   2 s cadence):** with the device fully absent, each of the two queries can use up to its
   2 s timeout (plus one internal reopen+retry each, §2.4). So a dead link yields
   roughly one publish every 4–8 s. The bridge drops missed beats and does not queue them.
4. The bridge publishes `/state` **only when a value actually changes** (dedup over the
   state fields of §5.1 — a bare timestamp change does not trigger a publish). During a
   device outage the error/device_online transitions themselves change the state, so
   consumers watching `/status` + `/state.device_online` keep full visibility.

### Reference-implementation note (non-normative)

The reference is a Go program. Its poll loop is a goroutine with a 2 s ticker. Its MQTT
client is the Eclipse Paho Go library. These specifics are incidental. But note they carry
the pitfalls whose *workarounds* are normative. Paho's connect call is not cancellable by a
context (hence the wrapper requirement in §3). Its incoming-message handlers run on the
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
| `ts` | string | RFC 3339, UTC | timestamp of this publish. |
| `freq_hz` | integer | **Hz** | The beam's tuned frequency. The device speaks kHz (uint16), and the bridge multiplies by 1000. Station-wide bus convention: frequency in Hz as an integer — never kHz or MHz on the bus. |
| `band` | string | — | human band label derived from the kHz value (§5.4). If outside all allocations, `band-<N>` with N = the device-reported band index. |
| `direction` | string | — | `forward` \| `reverse` \| `bidirectional` (never called "mode"). The label `unknown` goes to unrecognized wire values. |
| `moving` | bool | — | `true` while elements physically move. It must read `true` when the status-query motor bits ≠ 0 **or** the moving-status travel-left distance ≠ 0. |
| `device_online` | bool | — | `true` while the RCU-06 is reachable over serial. **This field is always present**, never omitted. It distinguishes "bridge online, device down" from "no data". Bridge liveness is `/status`, never a state field. |
| `error` | string, omitempty | — | last transport-level error message. Absent when empty. |

### 5.2 Two-layer liveness (normative)

`/status` proves the *bridge process* is alive (the broker's last-will mechanism gives the
proof). The `/state.device_online` field proves the *device behind the bridge* is
reachable.
Consumers must check both. This bridge always publishes `device_online` explicitly
(including `true`). See `02-interface-spec.md` §5 for the station-wide contract,
including the absence-equals-true equivalence question listed in §12 here.

### 5.3 Steady-state refresh semantics

Every 2 s tick: the bridge replaces the state wholesale from the status query (frequency,
band label, direction from the orientation low nibble, moving from motor bits, error
implicitly cleared). Then the moving-status query recomputes `moving` from the travel-left
distance. Then it publishes. Each executed command also triggers its own refresh
afterwards (§6).

### 5.4 Band tables (exact, normative)

**Band label from device kHz value (upper boundary exclusive):**

| Label | Low kHz | High kHz (exclusive) |
|-------|---------|----------------------|
| `160m` | 1800 | 2000. |
| `80m` | 3500 | 4000. |
| `40m` | 7000 | 7300. |
| `30m` | 10100 | 10150. |
| `20m` | 14000 | 14350. |
| `17m` | 18068 | 18168. |
| `15m` | 21000 | 21450. |
| `12m` | 24890 | 24990. |
| `10m` | 28000 | 29700. |
| `6m` | 50000 | 54000. |
| otherwise | — | `band-<device band index>`. |

**Band-center table (IARU Region 1 mid-band frequencies, kHz)** — the frequencies a `band`
command tunes to: `20m`→14175, `17m`→18118, `15m`→21225, `12m`→24940, `10m`→28850,
`6m`→51000. Options list order for UI selects: `["20m","17m","15m","12m","10m","6m"]`.

---

## 6. Command surface

### 6.1 Dispatch model (normative)

`/cmd` payloads parse as `{"action": string, "value": string, "freq_hz": int64}`. The
bridge logs and drops messages that fail JSON parsing or lack an `action`. **Incoming MQTT
message handlers must never execute device work inline**: the bridge enqueues each command
(bounded queue, capacity 256 — **it drops a command when the queue is full**) and a single
worker executes them **in arrival order**. Commands thereby serialize against each other
and against the poll loop. Rationale: in the reference MQTT library, handlers run on the
connection's dispatch thread. A handler that blocks on serial I/O or publishes
synchronously deadlocks the client — this happened live in a sibling component. The
requirement is library-independent: isolate handler work from the receive path regardless
of stack.

**Retention consequences (must preserve):**

- Every non-retract command stays retained on `/cmd`. The broker **re-delivers it and the
  bridge re-executes it on every reconnect/restart** — intentional actuator self-healing.
  The antenna re-converges to the last commanded state.
- `retract` is **one-shot**: after the send step, the bridge publishes an empty retained
  payload to `/cmd`, clearing the retained message. Retract then never re-fires on restart.
  The clear is **unconditional** — it happens even when the retract send itself failed
  (see §6.2 for the exact semantics and why they are safety-relevant).

### 6.2 Actions (normative table)

| Action | Payload | Behavior |
|--------|---------|----------|
| `frequency` | `{"action":"frequency","freq_hz":21225000}` | Convert Hz→kHz by truncating integer division by 1000. Keep the current direction. Send change_frequency to the device (subject to the 25 kHz deadband, §6.3), then refresh state. NOTE: this is the bus's one documented exception to the `value`-key convention (§3.2). |
| `direction` | `{"action":"direction","value":"reverse"}` | Change direction at the current frequency. The value must be one of the direction aliases (§2.3). The command always reaches the device (it bypasses the deadband). |
| `mode` | `{"action":"mode","value":"…"}` | **Deprecated alias** for `direction`. It must stay accepted for backward compatibility. |
| `band` | `{"action":"band","value":"15m"}` | Look up the band-center table (§5.4) — the bridge trims whitespace from the value. For an unknown band it logs `unknown band %q` and drops the command. Otherwise the command behaves like `frequency` at that center, keeping the current direction. |
| `retract` | `{"action":"retract"}` | Send retract to the device (5 s timeout). Refresh state. Then clear the retained `/cmd` topic (§6.1). **The clear is unconditional.** It happens after the retract send regardless of outcome. A non-ok device reply, a rejected/invalid invocation, or a transport-level failure all still clear the retained command. The bridge does not check the retract result before clearing. Consequence: a retract that failed does NOT re-fire after a restart. Any rebuild that instead clears only on success must flag that as a deliberate divergence. The change alters the retry semantics of the antenna's designated safe-state command — a failed retract can then re-execute on every restart. |
| anything else | — | The bridge logs `unknown cmd action=%q` and drops the command. |

**No acknowledgment channel exists** (a known defect, §11 item 4): the bridge never
publishes command-execution errors back to the bus — no ack/nack topic. A rejected command
(reply not OK) leaves `/state` untouched and does not set `error`. Only transport-level
failures flip `device_online`/`error` (through the next poll). A command sender must check
success by diffing `/state`. If the current frequency is unknown (0 — before the first
successful refresh, or while offline), a `direction` command first triggers a status
refresh and, if the frequency still reads unknown, the command fails with
`current frequency unknown` (logged only).

### 6.3 The 25 kHz deadband (normative behavior contract)

The bridge **suppresses** a change_frequency command (silently, treated as success) when
all of the following hold:

- the current known frequency is nonzero,
- `|requested − current| < 25 kHz` (a difference of exactly 25 kHz goes through),
- the direction byte equals the current direction.

Rationale: re-driving the motors for a sub-25 kHz retune only wears them and churns the bus.
25 kHz matches the debug UI's nudge step, so one nudge still passes. A pure direction change
always bypasses the deadband (a direction flip at the same frequency must reach the device).
When the current frequency is unknown (0), the bridge skips the deadband so the command
always goes through.

### 6.4 HTTP debug/control API (secondary surface, normative on endpoint semantics)

The bridge serves a small web interface on the configured HTTP address (default
`127.0.0.1:8080` — the production deployment seeds `0.0.0.0:8080`). There is **no
authentication** (a LAN-trust-level surface, see §11 item 9). All modifying endpoints are
POST. Device errors return **502 Bad Gateway** with the error text. Invalid input returns
400.

| Endpoint | Method | Body | Behavior |
|----------|--------|------|----------|
| `/` | GET | — | HTML status/control page. Live updates travel through server-sent events. Controls stay disabled while motors move. The frequency input clamps to 1..65535 kHz with ±25 kHz nudge buttons. Band buttons POST the band-center kHz values. |
| `/api/status` | GET | — | JSON of internal state: `frequency_khz` (int), `band_name` (string), `band_index` (int), `mode_name` (string), `motors_moving` (bool), `motor_bits` (int), `updated_at` (RFC 3339), `offline` (bool), `last_error` (string, omitted when empty). |
| `/api/events` | GET | — | SSE (server-sent events — a long-lived HTTP stream pushing updates). Event name `status`, data = same JSON as `/api/status`. The stream sends on every 2 s poll tick unconditionally (unlike MQTT's change-only) plus one immediate event on connect. |
| `/api/refresh` | POST | — | One status-query round trip, then a broadcast. |
| `/api/retract` | POST | — | The same as the MQTT retract, but it clears no retained topic. |
| `/api/frequency` | POST | form: `frequency` (kHz, integer 1–65535, needed), `mode` (can stay unset, then the current direction applies) | set frequency (deadband applies). |
| `/api/mode` | POST | form: `mode` (needed) | set direction only (deadband bypassed). |
| `/api/debug` | GET / POST | POST form `enabled` ∈ {1,true,on} / {0,false,off,empty} | toggle serial-traffic tracing. GET returns `{"enabled": bool}`. |
| `/api/debug/events` | GET | — | SSE stream. Event `trace`, data JSON `{"at":"15:04:05.000","dir":"tx"\|"rx","name":"status_query"\|"ok"…,"com":<int>,"data":"01 0F …","err":""}` — one tx entry before and one rx entry after each serial exchange, only while tracing is on. |

This surface is a human/debug surface and is secondary to the bus contract. The
re-implementation's web UI look, SSE mechanics, and trace format are free choices. But they
must keep: a status display, manual frequency/direction/retract control with the same
clamps, and a serial-traffic debug stream. (Reference-implementation note: the reference UI is a
single Go HTML/template + SSE page. The exact wire format above describes it for
ops-tooling compatibility.)

### 6.5 Home Assistant discovery (secondary, off by default)

Two paths exist. The **default** is that this bridge publishes only the `expose` block
(§3.2), and the **separate station service (`hadiscovery`)** renders Home Assistant (an
open-source home-automation platform) discovery from it. A **legacy embedded** discovery —
retained config messages under `homeassistant/<component>/hf-ant-ctrl/<object_id>/config`
(8 entities, value templates like `{{ value_json.freq_hz }}`, availability tied to
`/status`) — still exists in the reference, gated OFF by config
`mqtt.publish_ha_discovery = false`. A clean rebuild can drop the embedded path entirely
(keeping `expose`). In that case the re-publish requirement below does not need
re-implementation. Only the `/state` republish and the `homeassistant/status`
subscription stay. For any rebuild that KEEPS the legacy path (gate on), the following
applies. An `online` on `homeassistant/status` must trigger re-publication of ALL
embedded discovery config messages (all 8 entity configs) **before** the `/state`
republish. Without this, every Home Assistant restart loses the entities.
(Reference-implementation note: the reference does exactly this in its MQTT client's
HA-status handler.) In all modes the bridge must subscribe to `homeassistant/status`.
When it announces `online` (the "rebirth" signal a Home Assistant broker sends after
its own restart), the bridge tries a `/state` republish (it passes through the same
change-dedup check). The retained copy serves subscribers regardless, so this is benign.

---

## 7. Startup, shutdown, and error paths

### 7.1 Startup sequence (exact order, normative)

1. Parse flags. Resolve configuration with precedence **explicitly-set flag > config file >
   built-in default**. If the *default* config path is absent, run on defaults. If the
   `-config` path does not exist, or a file does not parse, exit 1.
2. Open the serial transport. An empty `serial_port` means the in-process mock (§2.5).
   Otherwise open the port at `baud`. **First-open failure is fatal** (print to stderr, exit 1).
3. Wrap the transport in the serial tracer (a no-op unless the debug toggle is on).
4. Construct the controller. The first in-memory state is the zero value: frequency 0,
   band **empty string**, direction **empty string** (until the first successful refresh).
   The labels `band-<N>` (§5.4) and `unknown` (§2.3) appear only as computed values after
   a successful refresh. So if the device is unreachable at boot, the first
   published `/state` carries `freq_hz:0`, `band:""`, `direction:""`, with
   `device_online:false` and the transport error in `error`. It does not carry
   `band-0`/`unknown`.
5. Start the web server's debug hub. Build the HTTP UI.
6. If config defines `mqtt.broker`: connect MQTT (cancellable, §3). **On failure log
   `mqtt disabled: …` and continue without MQTT** rather than exiting. On success: publish
   `/status=online`, `/meta`, bind the `/cmd` + `homeassistant/status` subscriptions.
7. Start the 2 s poll loop (status query, moving status, publish to web + MQTT).
8. Do one immediate first refresh (errors logged only, not fatal) and publish the
   first state to web + MQTT.
9. Serve HTTP until shutdown.

### 7.2 Shutdown

SIGINT triggers graceful shutdown. The HTTP server drains (5 s budget). The command worker
stops first. Then the bridge publishes `/status=offline` if connected. Then the MQTT
connection closes with a 250 ms grace. **Known defect, preserved as a fact.** The code traps
only SIGINT and ignores SIGTERM (what systemd sends on `stop`/`restart`). A service stop
then kills the process without the graceful path, and the broker fires the LWT `offline`. The
re-implementation must trap SIGTERM too. Either way, the bus-visible outcome must stay a
correct `/status=offline`.

### 7.3 Error-path table (normative)

| Event | State effect | Bus effect | Recovery |
|-------|--------------|------------|----------|
| Serial write/read EIO | `device_online:false`, `error` = raw message. | `/state` publish (if changed). | The bridge reopens automatically through the by-id path (§2.4), within a tick or two of the adapter returning. |
| Response timeout (device silent) | first timeout: one silent reopen+retry. Second: `device_online:false`, `error:"read response: read timeout"` or `"timeout waiting for response"`. | `/state` publish. | The poll loop keeps retrying. |
| Reply not `ok` (error/bad_params/invalid_command/debug) | state **unchanged**. The bridge returns the error to the caller. `device_online` does not flip. | none directly. | The next poll. |
| Serial reopen fails (adapter absent) | handle becomes `nil`, the error surfaces. | `/state` publish. | A lazy retry runs on every subsequent exchange/poll. |
| MQTT connection lost | — | `/status` → `offline` through the broker LWT. | Auto-reconnect. On reconnect, full re-init (§3). Retained `/cmd` re-delivery ⇒ the bridge re-applies the last command. |
| MQTT connect fails at startup | — | none. The bridge runs web-only. | none (no retry while running — a restart fixes it). |
| Process crash | — | `/status` → `offline` (LWT). | service manager `Restart=on-failure`, 5 s. |

Exact error strings, split by where each can appear (consumers can match on them):

**(a) Strings that CAN appear in `/state.error`.** The transport-level failures (§6.2)
exclusively populate the error field. Every `/state.error` value originates in
the exchange layer — a rejected reply or invalid parameter never reaches `/state`.

- `write request: <err>` (for example `write request: Input/output error`)
- `read response: <err>` (for example `read response: read timeout`)
- `reopen serial: <err>`
- `timeout waiting for response`
- `device closed` (an exchange that ran after the transport shut down)
- `EOF` (end-of-file on the serial link — link drop)
- a context-cancellation error (shutdown interrupted an in-flight exchange)

**(b) Strings the bridge returns to command callers but NEVER stores in `/state.error`.**
These appear in the bridge log and in HTTP 502 response bodies only. A command that fails
this way leaves `/state` untouched (§6.2):

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
| Poll interval | 2 s. |
| status-query / moving-status exchange timeout | 2 s each (per send try). |
| retract / change-frequency exchange timeout | 5 s each. |
| Frequency deadband | 25 kHz. |
| Sequence number space | 0..127, wraps. |
| Command queue capacity | 256 (drop when full). |
| MQTT disconnect grace | 250 ms. |
| HTTP shutdown budget | 5 s. |
| Service-manager restart delay (`RestartSec`) | 5 s. |
| Mock retract duration (test double only) | 2000 ms. |
| Web SSE channel buffers | 8 (status) / 32 (debug). Slow consumers drop events. |

---

## 8. Configuration

Single TOML (a config-file format of nested `key = value` tables) file, default path
`/etc/ultrabridge/config.toml` (override with `-config`). Precedence: explicitly-set flag >
file > built-in default. Quirk preserved as fact: a flag explicitly set to its default value
still wins over the file. A missing default-path file ⇒ run on defaults. A missing
explicitly-given file or malformed TOML ⇒ fatal.

| Key | Type | Default | Meaning |
|-----|------|---------|---------|
| `http_addr` | string | `127.0.0.1:8080` (deploy seeds `0.0.0.0:8080`) | HTTP listen address. |
| `serial_port` | string | `""` ⇒ mock | serial device path. Production uses `/dev/serial/by-id/usb-FTDI_Dual_RS232-if00-port0`. |
| `baud` | int | `19200` | serial baud. |
| `location` | string | `""` (deploy seeds `bauwagen`) | deployment fact published in `/meta.location`. Omitted when empty. |
| `host` | string | `""` (deploy seeds `shari`) | compute node name published in `/meta.host`. Omitted when empty. |
| `mqtt.broker` | string | `""` ⇒ MQTT disabled | broker URL, for example `tcp://192.168.1.50:1883`. |
| `mqtt.client_id` | string | `""` ⇒ derived `muehle-hf-ant-ctrl` | MQTT client ID. |
| `mqtt.site` | string | `""` (deploy seeds `muehle`) | topic prefix part 1. An empty site or station at MQTT init ⇒ error, MQTT disabled. |
| `mqtt.station` | string | `""` (deploy seeds `hf`) | topic prefix part 2. |
| `mqtt.slot` | string | `ant-ctrl` | topic prefix part 3 (canonical role). |
| `mqtt.discovery_prefix` | string | `homeassistant` | HA discovery tree prefix (legacy embedded path only). |
| `mqtt.publish_ha_discovery` | bool | `false` | gate for legacy embedded HA discovery. |
| `mqtt.user` | string | `""` | MQTT username (production `hf`). |
| `mqtt.password` | string | `""` | **secret**, stored in this file. |

Flags (only those explicitly given override the file): `-config`, `-http`, `-port`, `-baud`,
`-mqtt-broker`, `-mqtt-client-id`, `-mqtt-user`, `-mqtt-password`.

**Secret handling (normative):** the MQTT password lives only in the TOML file. The
deployer installs that file as `0600` (owner read/write only, owned by the service user).
The password never appears in the service unit, the ExecStart command line, or the process
table. There is no
environment-variable fallback. See the `docs/conventions/config-and-secrets.md`
counterparts in `02-interface-spec.md` §config.
---

## 9. Deployment

- **Target:** shari, Raspberry Pi, `io@192.168.1.139`, Linux arm64.
- **Install procedure (the deploy tooling must reproduce it):**
  1. Cross-compile a static binary for `linux/arm64` (reference flags:
     `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`). Copy the
     binary, a generated systemd unit, and a seed config to the target — write the seed
     config to a temp file under `umask 077`.
  2. Create system user/group `ultrabridge` (nologin, no home) if missing. Make sure
     group `dialout` exists, and add the service user to it.
  3. Install a **udev rule** `/etc/udev/rules.d/99-ultrabridge-serial.rules`:
     `SUBSYSTEM=="tty", SUBSYSTEMS=="usb", ATTRS{idVendor}=="0403", GROUP="dialout", MODE="0660"`
     (FTDI USB-serial vendor id 0403). The rule keeps the serial device always
     group-accessible, whatever the distro default is (plugdev vs dialout).
  4. Create `/opt/ultrabridge` (binary directory) 0755, **root-owned**, with the binary
     installed there root-owned mode 755. Create `/etc/ultrabridge` (config directory)
     0755, owned by the service user (owner and group both the service user). The
     unprivileged runtime account must not own the directory containing the executable
     it runs.
  5. **Seed-once config:** install `/etc/ultrabridge/config.toml` (0600, service-user
     owned) **only if it does not already exist**. Later deploys never touch it. The
     device owns its settings. Edit on the device, or delete the file to re-seed.
  6. Install the unit, `daemon-reload`, enable, restart, print status.
- **Service unit essentials (exact):** `After/Wants=network-online.target`. `Type=simple`.
  `ExecStart=/opt/ultrabridge/ultrabridge -config /etc/ultrabridge/config.toml` (no other
  arguments — everything comes from the file). `Restart=on-failure`, `RestartSec=5`.
  `User=Group=ultrabridge`, `SupplementaryGroups=dialout`.
  `ConfigurationDirectory=ultrabridge`. Hardening: `NoNewPrivileges=true`,
  `ProtectSystem=full`, `ProtectHome=true`, `PrivateTmp=true`. Device access:
  `DeviceAllow=char-ttyUSB rw`, `DeviceAllow=char-ttyACM rw`, `DeviceAllow=char-tty rw`.
- **Runtime dependencies:** the MQTT broker at `192.168.1.50:1883` (user `hf`, see the
  broker-topology open decision in §12). It needs the FTDI adapter on USB and network
  reachability. The HTTP UI listens on all interfaces at port 8080 in production.
- **Operations:** logs travel through the journal (`journalctl -u ultrabridge -f`).
  Restart with `systemctl restart ultrabridge`. See `05-deployment-ops.md` for the
  station-wide runbook.

### Reference-implementation note (non-normative)

The reference is a Go program. It uses `go.bug.st/serial` for the serial link and Eclipse
Paho for MQTT, and a `deploy.sh` script deploys it (plus a Windows build in `build.sh` for
developer convenience only). These are incidental choices. Every behavior in this document
stands independently of them. What is NOT incidental: that library's blocking serial
reads and non-cancellable connect motivated the cancellation and timeout requirements in
§2.4/§3. Paho's dispatch-thread handler execution motivated §6.1. The
re-implementation must reproduce the *workarounds* whatever stack it uses.

---

## 10. Invariants and safety rules (must hold without exception)

1. **Antenna commands run serialized.** Exactly one serial exchange is in flight at any
   time (device-wide lock). Poll queries and commands interleave but never overlap on the
   wire.
2. **`freq_hz` on the bus is Hz, integer.** The device speaks kHz (uint16). The ×1000
   conversion happens only inside the bridge. Never publish kHz/MHz.
3. **`direction` is never named `mode` on the bus.** Bus `mode` is the radio emission-mode
   vocabulary. The deprecated `mode` *command alias* must stay accepted.
4. **`device_online` is always present in `/state`** — consumers must distinguish "bridge
   online, device down" from "no data". Bridge liveness is `/status` (LWT) only.
5. **`retract` is one-shot.** After the retract step the bridge clears the retained `/cmd`
   (empty retained publish) — unconditionally, even if the send failed (§6.2) — so it
   cannot re-fire on restart. All other commands stay retained and the bridge re-applies
   them on reconnect — intentional actuator self-healing.
6. **Retract is the antenna's safe state** (elements fully in). External safety logic (the
   `antennaselect` reconciler, `06-safety.md`) drives grounding/antenna selection. This
   bridge merely executes `retract` faithfully with a 5 s timeout and checks the reply is
   `ok`.
7. **A dead device must never take the bridge down.** Serial errors set
   `device_online:false` and surface in `error`. The process stays up, keeps polling,
   keeps serving MQTT/web, and self-heals (§2.4).
8. **At most one reopen+retry per exchange** — no unbounded retry loops against a broken
   link. The 2 s poll is the recovery driver.
9. **The 25 kHz deadband** (§6.3) with the direction-change and unknown-frequency
   bypasses, exactly as defined here.
10. **MQTT handlers must never block** on serial I/O or a blocking publish inside the
    client's dispatch callback. A bounded worker queue serializes all command work and
    executes it in arrival order.
11. **Connect must be cancellable** (a shutdown signal interrupts a hung MQTT connect).
12. **Secrets never on the command line** — the password lives only in the 0600 TOML file.
13. **The bridge rejects checksum/framing errors** on received packets. It discards
    stale-sequence replies rather than attributing them to a new request.
14. **Frequency stays inside the uint16 kHz domain** (max 65535 kHz = 65.535 MHz). The
    HTTP layer enforces 1..65535 kHz. Today the MQTT path relies on truncation. Any
    re-implementation must gain an explicit range check (see §11 item 3).

---

## 11. Known defects and fragilities (facts about the reference, to fix or consciously keep)

1. **The code does not handle SIGTERM** — it traps only SIGINT. Every
   `systemctl stop/restart` (SIGTERM) kills the process without the graceful path: no
   self-published `offline`, no HTTP drain. The broker LWT covers `/status=offline`, so bus
   consumers see no difference. But the graceful-shutdown code is effectively dead in
   production. A rebuild must trap SIGTERM.
2. **A response timeout also triggers the reopen+retry path** (§2.4 exactness note),
   contradicting an in-code comment. Harmless (bounded to one retry) but a
   re-implementation must decide deliberately: a *slow* device causes serial-port churn on
   every timeout.
3. **`freq_hz` truncation on MQTT `frequency` commands:** `khz := uint16(cmd.FreqHz / 1000)`
   — values above 65.535 MHz wrap modulo 65536 (for example 71 MHz → 5.6 MHz sent), and the
   command silently drops sub-kHz precision. No range validation on the MQTT path (unlike
   the web path's 1..65535 check). A rebuild must add the range check.
4. **No command acknowledgment channel.** Invalid direction, unknown band, rejected params,
   or timeouts on a `/cmd` only produce log lines. A rejected command does not set
   `/state.error`. Only transport failures do. A command sender must diff `/state`.
5. **Queue drop semantics:** if more than 256 commands wait in the queue, the bridge
   silently drops new ones. A dropped command stays invisible. On shutdown, the worker loses
   queued-but-unexecuted jobs — for a lost *retract* job the retained `/cmd` was not
   cleared, so the retract re-executes on the next start.
6. **Poll-tick stretching** (§4 item 3): consumers must not assume a hard 2 s cadence.
7. **Parsed-but-unused status fields.** Per-motor bits, min/max freq, firmware version,
   flags, and progress units never reach the bus. So a single stuck motor only shows as
   generic `moving:true` or device silence.
8. **DLE-escape asymmetry** (`b & 0x7F` encode vs `b | 0x80` decode) cannot round-trip
   payload bytes ≥ 0x80 that collide with framing constants. Irrelevant for current
   traffic. A trap if the unused `modify_element_len` (0x0C) or `debug` payloads ever carry
   high-bit bytes.
9. **Web UI has no authentication** and production binds `0.0.0.0:8080` — anyone on the LAN
   can move the antenna through `/api/*`. Same trust level as the rest of the shack LAN,
   but a security-sensitive rebuild must at least document or gate it.
10. **Doc lags in the source repo (code is truth):** the API doc's `/meta` example shows
    `"area": "Radio shack"` in `expose.device` (the code omits it). The README's MQTT example
    uses user `ham` (the production user is `hf`). The API doc's `/api/status` example
    omits `band_index`, `motor_bits`, `last_error`, which the endpoint returns.
11. **Dead protocol surface:** the protocol declares command
    `0x0C modify_element_len` and the error value `unsupported write command`, but nothing
    uses them. The code defines `ReplyDebug` (0x40) but only ever treats it as "not ok".
12. **First-refresh race:** the poll loop starts before the one-shot first refresh.
    Under slow startup the first tick can interleave. Harmless (lock-guarded).
13. **FTDI re-enumeration root cause is environmental** (USB bus glitches correlate with
    MQTT dropouts on the same host). The self-heal masks it. It does not fix the flaky
    USB. The by-id path is load-bearing — a config pointing at `/dev/ttyUSB0` breaks
    self-heal (a reopen then resolves the *name*, which can be gone or can name the other
    port of the dual adapter).

---

## 12. Open decisions and unresolved facts

1. **Ultrabeam switch port (station wiring — this affects the ecosystem around this
   bridge)**. The repo-root documentation and station-integration docs put the Ultrabeam
   beam on antenna-switch port 3. The antennaselect seeded config and the console's
   antenna map put it on port 4. The live config on shari is authoritative, but this
   PRD's research never read it. This bridge itself is agnostic (it tunes the antenna —
   the switch routes it). But any re-integration must confirm the port on-device. See
   `03-components/antennaselect.md` §open-decisions and `00-system-overview.md`.
2. **Broker topology.** Deployed config points at the shack broker 192.168.1.50:1883. A
   migration to a broker on shari (192.168.1.139) exists on an unmerged feature branch,
   committed but NOT deployed as of 2026-08-29. The re-implementation must treat the
   broker URL as configuration and confirm the production target at deploy time. (§3, §8.)
3. **`device_online` published form.** The integration-model doc lets `device_online`
   stay "omitted when true". This bridge (like all deployed bridges) publishes
   `device_online:true` explicitly. The re-implementation must mandate explicit-true
   (this component's contract does: §5.1) — station-wide, the `02-interface-spec.md`
   §5 contract tracks the equivalence question.
4. **Live configuration values on shari.** The repo does not record them. They are: the
   actual `serial_port` string, the MQTT credentials, whether any consumer now relies on
   the legacy embedded HA discovery (§6.5). They also cover whether anything still sends
   the deprecated `mode` command alias. The re-build owner must check them with the operator before
   freezing the re-build. The research spec assumes the seeded values (`bauwagen`,
   `shari`, user `hf`, by-id serial path) but never read the live
   `/etc/ultrabridge/config.toml`.
5. **Does the RCU-06 firmware ever emit unsolicited frames?** The frame reader resyncs on
   STX, so it tolerates them, but there is no evidence either way. The re-implementation
   must preserve STX-resync behavior (it costs nothing and tolerates unsolicited debug
   frames).
6. **Real-world retract duration** and whether the 5 s retract exchange timeout is ever
   exceeded in the field (the mock simulates 2 s of motor motion, and the repo contains no
   field data). If real retracts take longer than 5 s to *acknowledge* (as opposed to
   complete), the timeout needs retuning. The device's motors keep moving after the `ok`
   reply. The 2 s poll's `moving` flag is the completion signal.
7. **Timeout-vs-fault discrimination** (§2.4 exactness note / §11 item 2): the reference
   treats a response timeout as a port fault (one reopen+retry). Keep this or distinguish
   timeouts from EIO deliberately — unresolved. The current behavior is harmless, but it
   causes port churn with a merely slow device.
8. **`frequency` command range validation on the MQTT path** — the reference truncates and
   wraps (§11 item 3). A re-implementation needs an explicit `0 < freq_hz ≤ 65535000`
   check (§10 item 14). Whether the bridge drops out-of-range commands silently or
   surfaces them somehow (there is no ack channel, §11 item 4) stays an open design
   decision coupled to the missing-acknowledgment defect.