---

# ultrabridge — Research Spec (for PRD re-implementation)

Source: code + docs of `/Users/ingomar.otter/dev/stationa/ultrabridge` (Go), read 2026-08-29.
Code is truth; README/API-doc divergences are flagged inline. Audience: an engineer who knows
nothing about amateur radio or this station. Every term is defined at first use.

---

## 1. Purpose & role

**Amateur radio ("ham radio")** is a licensed hobby radio service; operators transmit on
allocated **frequency bands** (e.g. "20m" ≈ 14 MHz, wavelength-derived names). A station's
antenna must be matched to the operating frequency to radiate efficiently.

**ultrabridge** is a software bridge that connects an **Ultrabeam RCU-06 antenna controller**
to the station's MQTT message bus. The Ultrabeam is a *motorized loading-coil antenna*: three
(or more) telescoping aluminum elements whose lengths are adjusted by electric motors so the
antenna resonates on the chosen frequency. The **RCU-06** is the little box that drives those
motors; a computer talks to it over a serial cable (USB-serial adapter).

The bridge:

1. Polls the RCU-06 over serial every 2 s for its current state (tuned frequency, direction,
   whether motors are moving).
2. Publishes that state as a retained JSON snapshot to MQTT topic `muehle/hf/ant-ctrl/state`.
3. Subscribes to `muehle/hf/ant-ctrl/cmd` and executes tuning commands (set frequency, change
   direction, jump to a band center, retract the elements) sent by other station components or
   a human.
4. Serves a small web page (status display, manual controls, serial-traffic debug stream).

**Its place in the station:** the station ("Mühle", a German amateur radio site) is a
distributed system of hardware bridges and logic services speaking MQTT. Each device occupies
a **slot address** `<site>/<station>/<slot>` — here `muehle/hf/ant-ctrl` (site `muehle`,
station `hf`, role `ant-ctrl`). The bus uses four **planes** per slot: `/meta` (identity),
`/state` (current snapshot), `/status` (bridge liveness), `/cmd` (commands in). ultrabridge
implements the `ant-ctrl` slot: the *controller* of one remotely-tunable antenna. Physical
antennas are passive resources (no MQTT); a separate reconciler (`antennaselect`) decides
routing and publishes band-follow commands to this slot. The bridge runs on **shari**, a
Raspberry Pi at 192.168.1.139, as a systemd service; the MQTT broker is at 192.168.1.50:1883.

Deliberate naming rule: the beam-direction vocabulary is `direction`, **never** `mode` — on
this bus `mode` means the radio emission mode (`cw`/`usb`/`lsb`/…), a different concept.

---

## 2. Upstream interface — the RCU-06 serial protocol

### 2.1 Transport

- USB-serial adapter: **FTDI Dual RS232** (two-port; the RCU-06 sits on interface 0). On the
  target host it appears as `/dev/ttyUSBn`.
- The adapter intermittently drops and **re-enumerates under a different tty name**
  (observed live: ttyUSB0/ttyUSB2 → ttyUSB1/ttyUSB3; cause looks like a USB hub/power/cable
  glitch — the MQTT connection dropped at the same moments, i.e. the whole USB bus bounced).
  Therefore the bridge must be configured with the **stable by-id symlink**
  `/dev/serial/by-id/usb-FTDI_Dual_RS232-if00-port0`, which always points at whichever tty
  the kernel currently assigns. The self-heal behavior (§2.4) depends on this.
- Serial line parameters: **19200 baud, 8 data bits, no parity, 1 stop bit (8N1), no flow
  control** (the Go library `go.bug.st/serial` defaults; only baud is set explicitly).
  Baud is configurable (`baud` config key / `-baud` flag).
- The port is opened **without a read timeout**: a read blocks until a byte arrives or the
  link drops. Request/response deadlines are enforced by the caller (see §2.3).
- Requests are strictly serialized (a mutex); there is no concurrent traffic on the wire.

### 2.2 Framing

Byte-oriented link with a simple HDLC-like frame. Constants:

| Name | Value |
|------|-------|
| STX (start of frame) | `0xF5` |
| DLE (escape) | `0xF6` |
| ETX (end of frame) | `0xFA` |
| checksum seed | `0x55` |

**Frame layout:** `STX  <escaped payload>  ETX` where the unescaped payload is
`Seq(1 byte) | Com(1 byte) | Data(0..n bytes) | Checksum(1 byte)`.

- **Checksum:** start at `0x55`; for each payload byte `b`: `chk = chk XOR b; chk = chk + 1`
  (all mod 256). The result is appended as the last payload byte. On receive, the checksum is
  recomputed over everything except the last byte and must match.
- **Byte stuffing:** any payload byte equal to STX, ETX, or DLE is transmitted as the pair
  `DLE, (b & 0x7F)`. On receive, the byte following a DLE is restored as `(b | 0x80)`. Note
  the asymmetry: encode masks bit 7, decode sets bit 7 — lossy round-trip for payload bytes
  ≥ 0x80 that happen to equal a framing constant after masking; in practice the protocol's
  payload bytes are all < 0x80.
- **Minimum frame:** payload ≥ 3 bytes (`Seq | Com | Checksum`), else "packet too short".

### 2.3 Commands, replies, and the request/response model

One request frame out, one reply frame back. **Seq** is a bridge-side counter starting at 0,
incremented per request, wrapped to 7 bits (`(seq + 1) & 0x7F`, i.e. 0..127). The reply carries
the same Seq; a reply with a mismatched Seq is discarded and reading continues (tolerates
stale bytes from a previous timed-out exchange).

Request command bytes:

| Byte | Name | Meaning | Request Data | Bridge timeout |
|------|------|---------|--------------|----------------|
| `0x01` | status_query | read full status | none | 2 s |
| `0x02` | retract | fully retract all elements | none | 5 s |
| `0x03` | change_frequency | tune to frequency + set direction | 3 bytes: freq_kHz LSB, freq_kHz MSB (little-endian uint16), direction byte | 5 s |
| `0x0A` | moving_status | ask how far motors still have to travel | none | 2 s |
| `0x0C` | modify_element_len | directly command element lengths | — | **defined but never used by this bridge** |

Reply command bytes:

| Byte | Name | Meaning |
|------|------|---------|
| `0x00` | ok | success; Data carries the response payload (status queries) or is empty |
| `0x14` (20) | error | device error |
| `0x1E` (30) | bad_params | request rejected, e.g. change_frequency with < 3 data bytes |
| `0x28` (40) | invalid_command | unknown command byte |
| `0x40` (64) | debug | unsolicited debug message (ignored as a non-OK reply) |

**status_query reply payload (≥ 12 bytes, else "status payload too short"):**

| Offset | Field | Meaning |
|--------|-------|---------|
| 0 | firmware minor | controller firmware version, low |
| 1 | firmware major | controller firmware version, high |
| 2 | operation | current operation code (not interpreted) |
| 3–4 | frequency kHz | current tuned frequency, **little-endian uint16, kHz** |
| 5 | band index | device-internal band index (used only for the fallback label, §3.3) |
| 6 | orientation | direction byte; low nibble is the value (`& 0x0F`) |
| 7 | flags1 | not interpreted |
| 8 | flags2 | not interpreted |
| 9 | motor bits | nonzero ⇒ at least one motor is moving |
| 10 | min freq MHz | not interpreted |
| 11 | max freq MHz | not interpreted |

**moving_status reply payload (≥ 4 bytes):**

| Offset | Field | Meaning |
|--------|-------|---------|
| 0–1 | total distance mm | little-endian uint16; **remaining** travel distance in millimeters; 0 ⇒ motors idle |
| 2–3 | progress units | little-endian uint16; not interpreted beyond parse |

**Direction byte:**

| Byte | Wire meaning | Bus vocabulary (also accepted as input aliases) |
|------|-------------|--------------------------------------------------|
| `0x00` | normal | `forward` (also `""`, `normal`) |
| `0x01` | 180° | `reverse` (also `180`) |
| `0x02` | bidirectional | `bidirectional` (also `bidir`) |

Any other input string is rejected with `invalid mode "<s>" (expected forward|reverse|bidirectional)`.
Unknown wire values map to the label `unknown`.

**Tuning model.** The bridge never computes element positions. It sends only the target
frequency (kHz, uint16) plus a direction byte via `change_frequency`; the RCU-06 internally
maps that to per-element motor travel (the `band_index` in the status reply reflects the
device's own band table). There is **no band→element-length table in this bridge**; the only
band logic is (a) deriving a human band label from the kHz value for display, and (b)
mapping a band label to its IARU Region 1 center frequency when a "band" command arrives
(both tables in §3.3). IARU Region 1 = the Europe/Africa/Middle-East allocation plan that
defines band edges and recommended center frequencies. The unused `modify_element_len`
(`0x0C`) command is the protocol's hook for direct element control; the bridge declares it
(and an `unsupported write command` error value) but never exercises it.

### 2.4 Connection loss detection and the EIO self-heal (BEHAVIOR CONTRACT — must be preserved)

**The live failure:** the FTDI adapter randomly disconnects (USB glitch); the kernel
re-enumerates it under a **new tty name**. The long-lived bridge process still holds the file
descriptor of the now-deleted device node; every subsequent write fails with
`write request: Input/output error` (EIO). Before the fix the bridge kept accepting MQTT
commands but could never actuate the antenna until manually restarted
(`systemctl restart ultrabridge`).

**The self-heal design (implemented in `internal/ub/transport`):**

1. At startup the bridge opens the configured port (the stable **by-id path**) and keeps a
   re-usable *opener closure* capturing that path + baud.
2. Every exchange runs under a device-wide mutex. If the current handle is `nil` (a previous
   reopen failed, or the adapter is still absent), the opener is tried first — so the link
   re-establishes lazily, driven by the 2 s poll loop.
3. **Write fault:** if writing a request returns an error (EIO etc.), the bridge closes the
   stale handle (best-effort; it is usually already gone), calls the opener — which
   **re-resolves the by-id symlink to the freshly attached tty** — and, if the reopen
   succeeded, re-sends the request once with a **fresh sequence number**.
4. **Read fault:** the serial port has no read timeout, so a returned *error* (not absence of
   data) means a port-level fault (link drop, EOF, port closed). Same recovery: close,
   reopen via by-id path, re-send once with a fresh seq.
5. **Retry bound:** at most **one reopen per exchange**. A second fault within the same
   exchange is surfaced to the caller; the next poll tick retries. This prevents a tight
   reopen loop against a persistently broken link.
6. **Failed reopen:** if the adapter is not back yet (opener fails, e.g. "no such device"),
   the internal handle is set to `nil`, the error is surfaced, and the *next* exchange
   (poll tick) attempts the reopen again. Net effect: recovery within one to two 2 s poll
   ticks of the adapter reappearing — no manual restart.
7. **What recovery looks like to the bus:** while the link is down, `/state` carries
   `device_online:false` and `error:"…"` (e.g. `write request: Input/output error`, or
   `reopen serial: …` / `open serial port …: …`), while `/status` **stays `online`** — the
   bridge process itself is healthy. On the first successful exchange afterwards, the
   state is overwritten with fresh device data, `device_online:true`, and the error field
   cleared.

Exact exchange algorithm (per call), all under one lock:

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

Note (exactness matters): the inner read returns a non-nil error also when the caller-level
deadline expires (per-byte deadline check inside the reader), so **a pure response timeout
also triggers exactly one reopen + re-send** before surfacing
`read response: read timeout`. A code comment claims read errors are "not a timeout"; the
reader's own deadline check makes that comment inaccurate — observed behavior is: timeout ⇒
one silent reopen+retry, then fail. The 5 s/2 s timeouts apply to each send attempt.

Startup is fail-fast only for the *first* open: if the port cannot be opened at boot the
process prints the error to stderr and exits 1 (systemd restarts it every 5 s).

### 2.5 Mock device (implementation detail, useful for testing)

When `serial_port` is empty the bridge uses an in-process mock implementing the same
protocol: initial state 14000 kHz, band index 4, direction forward, motors idle; retract
succeeds immediately, reports motors moving for 2000 ms afterwards (then clears), and
resets frequency to 14000 kHz; change_frequency with < 3 data bytes replies bad_params;
unknown commands reply invalid_command; min/max freq reported as 7/30 MHz.

---

## 3. MQTT presence

Protocol MQTT 3.1.1 over plain TCP. Connection parameters:

- **Clean session = false** (subscriptions and retained delivery survive restarts);
  **auto-reconnect on**.
- **Client ID:** config `mqtt.client_id`, defaulting to `<site>-<station>-<slot>` →
  `muehle-hf-ant-ctrl` in production. A duplicate connection is diagnosable on the broker.
- **Last Will and Testament (LWT):** a message the broker publishes if the client dies
  without disconnecting. Here: `offline` on the `/status` topic, QoS 1, retained. On every
  successful (re)connect the bridge publishes `online` itself; on clean shutdown it
  publishes `offline` itself before disconnecting.
- Broker URL from config (`tcp://192.168.1.50:1883`), username `hf`, password from config.
- Connect is context-aware (a shutdown signal interrupts a hung connect; without this,
  SIGTERM during broker outage hangs until systemd SIGKILLs — a live station-wide lesson).
- On every (re)connect: publish `/status` = `online`, publish `/meta` (retained), then
  **re-subscribe** to `/cmd` and `homeassistant/status` (QoS 1 each) in case the broker lost
  session state.

### 3.1 Topics (exact strings)

| Topic | Direction | Retained | QoS | Payload |
|-------|-----------|----------|-----|---------|
| `muehle/hf/ant-ctrl/meta` | bridge → bus | yes | 1 | JSON birth certificate |
| `muehle/hf/ant-ctrl/state` | bridge → bus | yes | 1 | JSON state snapshot |
| `muehle/hf/ant-ctrl/status` | bridge → bus | yes | 1 | plain string `online` / `offline` |
| `muehle/hf/ant-ctrl/cmd` | bus → bridge | yes | 1 | JSON command |
| `homeassistant/status` | bridge ← | — | 1 | watched for HA rebirth (`online`) |

Slot parts come from config (`site`, `station`, `slot`); default slot `ant-ctrl`.

### 3.2 `/meta` payload (exact)

Published once per connect cycle. Retained JSON:

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

`location` is omitted when config `location == ""`; `host` omitted when config `host == ""`.
`expose` is a **consumer-neutral field descriptor** (no Home-Assistant-specific vocabulary):
a generic discovery renderer elsewhere in the station (`hadiscovery`) turns it into
home-automation entities. Exact content produced by the code:

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

`options_ref` resolves into `capabilities.bands` / `capabilities.directions` (single source
of truth for enum options).

**Doc lag:** the repo's `ultrabeam-mqtt-api.md` shows `"area": "Radio shack"` inside
`expose.device`; the code deliberately omits `area` (the standalone discovery service
supplies a station-wide default area). The code is truth.

### 3.3 `/state` payload (exact)

Retained JSON, QoS 1, **published only when a value actually changes** (dedup across the
fields listed below; a bare timestamp change does not trigger a publish):

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

| Field | Type | Unit | Semantics |
|-------|------|------|-----------|
| `ts` | string | RFC 3339, UTC | timestamp of this publish |
| `freq_hz` | integer | **Hz** | frequency the beam is tuned to. Device speaks kHz (uint16); the bridge multiplies by 1000. Bus convention: Hz as integer, never kHz/MHz. |
| `band` | string | — | human band label derived from the kHz value (table below); if outside all allocations, `band-<N>` where N is the device-reported band index |
| `direction` | string | — | `forward` \| `reverse` \| `bidirectional` (never called "mode") |
| `moving` | bool | — | `true` while elements physically move. Sources: status-query motor bits ≠ 0, or moving-status remaining distance ≠ 0 |
| `device_online` | bool | — | `true` while the RCU-06 is reachable over serial. **Always present.** Distinguishes "device down" from "no data". Bridge liveness is `/status`, never a state field. |
| `error` | string, omitempty | — | last error message; absent when empty |

**Band-label table from kHz (device units; upper boundary exclusive):**

| Label | Low kHz | High kHz (exclusive) |
|-------|---------|---------------------|
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

**Band-center table (IARU Region 1 mid-band, kHz):** `20m`→14175, `17m`→18118,
`15m`→21225, `12m`→24940, `10m`→28850, `6m`→51000. These are the frequencies a
`band` command tunes to. Options list order for selects:
`["20m","17m","15m","12m","10m","6m"]`.

### 3.4 Cadence and heartbeats

- **Poll loop: every 2 s** — status query, then moving-status query, then (if anything
  changed) publish `/state`. There is **no periodic `/meta` or `/status` heartbeat**; those
  are event-driven (connect/disconnect). `/state` doubles as the bridge's data heartbeat,
  and during a device outage the state itself changes (error transitions), so a consumer
  watching `/status` + `/state.device_online` has full visibility.
- A device outage stretches the effective tick: each of the two queries can consume up to
  its 2 s timeout (with one internal reopen+retry each), so a fully dead link yields roughly
  one publish every ≥ 4–8 s (the 2 s ticker drops missed beats rather than queueing them).

### 3.5 Home Assistant discovery (secondary, off by default)

Two paths exist; the **default** is that this bridge publishes only the `expose` block and a
**separate station service (`hadiscovery`)** renders Home Assistant discovery from it. A
legacy **embedded** discovery (retained configs under
`homeassistant/<component>/hf-ant-ctrl/<object_id>/config`, 8 entities, templates like
`{{ value_json.freq_hz }}`, availability tied to `/status`) still exists, gated by config
`mqtt.publish_ha_discovery = false` (default). When the gate is on, the bridge also
re-publishes all embedded discovery configs whenever `homeassistant/status` announces
`online`; in both modes an HA `online` birth triggers a `/state` republish attempt (it goes
through the same change-dedup check; the retained copy serves HA regardless, so this is
benign). The bridge subscribes to `homeassistant/status` in both modes.

---

## 4. Command surface

### 4.1 MQTT `/cmd` (retained JSON, QoS 1)

Payload parsed as `{"action": string, "value": string, "freq_hz": int64}`. Messages that fail
JSON parsing or lack an `action` are logged and dropped. Commands are not executed on the
MQTT client's callback thread: each is queued (bounded queue of 256; **dropped if full**) and
executed **in arrival order** by a single worker, so commands are serialized against the 2 s
poll loop and against each other. Consequences of retention:

- Every non-retract command stays retained and is **re-delivered and re-executed on every
  bridge reconnect/restart** (intentional self-healing: the antenna re-converges to the last
  commanded state).
- `retract` is one-shot: after executing, the bridge **publishes an empty retained payload**
  to `/cmd` (clearing the retained message) so retract never re-fires on restart.

| Action | Payload | Behavior |
|--------|---------|----------|
| `frequency` | `{"action":"frequency","freq_hz":21225000}` | Convert Hz→kHz by truncating integer division by 1000, keep the current direction, send change_frequency to the device (subject to the 25 kHz deadband, §5.3), then refresh state |
| `direction` | `{"action":"direction","value":"reverse"}` | Change direction at the current frequency; value must be one of the direction aliases (forward/normal/"" / reverse/180 / bidirectional/bidir). Always reaches the device (bypasses deadband). |
| `mode` | `{"action":"mode","value":"…"}` | **Deprecated alias** for `direction` (kept for backward compatibility) |
| `band` | `{"action":"band","value":"15m"}` | Look up the band-center kHz table (value is whitespace-trimmed); unknown band ⇒ log `unknown band %q` and drop. Then behaves like `frequency` at that center, keeping the current direction. |
| `retract` | `{"action":"retract"}` | Send retract to the device (5 s timeout), refresh state, then clear the retained `/cmd` topic |
| anything else | — | logged `unknown cmd action=%q`, dropped |

If the current frequency is unknown (0, e.g. before first successful refresh or while
offline), a `direction` command first triggers a status refresh; if still unknown it fails
with `current frequency unknown` (logged only). **Command-execution errors are never
published back to the bus** — no ack/nack topic exists; a command sender must diff `/state`.
A rejected command (reply not OK) does not set the error field either; only transport-level
failures flip `device_online`/`error` (via the next poll).

### 4.2 Web/HTTP API (secondary, human/debug surface)

Served on the configured HTTP address (default `127.0.0.1:8080`; production `0.0.0.0:8080`).
No authentication. All modifying endpoints are POST; device errors return **502 Bad
Gateway** with the error text; invalid input returns 400.

| Endpoint | Method | Body | Behavior |
|----------|--------|------|----------|
| `/` | GET | — | HTML status/control page (live via SSE; controls disabled while motors move) |
| `/api/status` | GET | — | JSON of the internal state: `frequency_khz` (int), `band_name` (string), `band_index` (int), `mode_name` (string), `motors_moving` (bool), `motor_bits` (int), `updated_at` (RFC3339), `offline` (bool), `last_error` (string, omitted when empty) |
| `/api/events` | GET | — | SSE stream; event name `status`, data = same JSON as `/api/status`; sent on every 2 s poll tick (unconditionally, unlike MQTT) plus one immediate event on connect |
| `/api/refresh` | POST | — | one status-query round trip, then broadcast |
| `/api/retract` | POST | — | same as the MQTT retract but clears nothing |
| `/api/frequency` | POST | form: `frequency` (kHz, integer 1–65535, required), `mode` (optional; defaults to current direction) | set frequency (deadband applies) |
| `/api/mode` | POST | form: `mode` (required) | set direction only (deadband bypassed) |
| `/api/debug` | GET / POST | POST form `enabled` ∈ {1,true,on} / {0,false,off,empty} | toggle serial-traffic tracing; GET returns `{"enabled": bool}` |
| `/api/debug/events` | GET | — | SSE stream; event `trace`, data JSON `{"at":"15:04:05.000","dir":"tx"\|"rx","name":"status_query"\|"ok"…,"com":<int>,"data":"01 0F …","err":""}` — one tx entry before and one rx entry after each serial exchange, only while tracing is on |

The web page's frequency input is clamped to 1..65535 kHz with 25 kHz nudge buttons (±25),
and band buttons POST the band-center kHz values directly to `/api/frequency`.

---

## 5. Behavior & state machine

### 5.1 Startup sequence (exact order)

1. Parse flags; resolve config with precedence **flag > config file > built-in default**. If
   the *default* config path is absent, run on defaults; if a path given explicitly via
   `-config` is missing, or any file is malformed, exit 1.
2. Open transport: `serial_port` empty ⇒ in-process mock (hardware-less dev/test); otherwise
   open the serial port at `baud`. **First open failure is fatal** (print to stderr, exit 1).
3. Wrap the transport in the optional tracer (no-op unless the debug toggle is on).
4. Construct the controller. In-memory state initially zero: frequency 0, band **""**
   (empty string), direction **""** (empty string), until first successful refresh — the
   zero-value `State{}` has empty-string `BandName`/`ModeName`; `band-<N>` and `unknown`
   are computed labels produced only by a successful refresh, so a boot-time offline
   device publishes `band:""`/`direction:""`, not `band-0`/`unknown`.
5. Start the web server's debug hub; build HTTP UI.
6. If `mqtt.broker` configured: connect MQTT (ctx-aware; on failure log `mqtt disabled: …`
   and **continue without MQTT** rather than exiting). On success: publish embedded
   discovery if gated on, bind `/cmd` + `homeassistant/status` subscriptions. The connect
   handler also fires: `/status=online`, `/meta`, re-subscribes.
7. Start the **2 s poll goroutine** (status query, moving status, publish to web + MQTT).
8. Perform one immediate initial refresh (error only logged, not fatal) and publish the
   first state to web + MQTT.
9. Serve HTTP until shutdown. SIGINT triggers graceful shutdown: HTTP server drains (5 s
   budget), MQTT closes (worker stopped first, then `/status=offline` published if
   connected, then disconnect with 250 ms grace). **Only SIGINT is trapped — SIGTERM (what
   systemd sends) is not**, so a systemd stop kills the process without the graceful path and
   the broker publishes the LWT `offline`.

### 5.2 Steady state

Every 2 s: status query → state replaced wholesale (frequency, band label, direction from
orientation low nibble, moving from motor bits, error implicitly cleared) → moving-status
query → moving recomputed from remaining distance, `offline=false`, error cleared → publish
to web (always) and MQTT (only on change). `/cmd` messages interleave via the worker queue;
each command does its own refresh afterwards.

### 5.3 The 25 kHz deadband (BEHAVIOR CONTRACT)

A change_frequency command is **suppressed** (silently, treated as success) when **all** hold:

- the current known frequency is nonzero,
- `|requested − current| < 25 kHz` (exactly 25 kHz goes through),
- the direction byte equals the current direction.

Rationale: re-driving the motors for a sub-25 kHz retune only wears them and churns the bus;
25 kHz matches the UI nudge step so one nudge still passes. A pure direction change
(`direction` command) always bypasses the deadband (a direction flip at the same frequency
must reach the device). When the current frequency is unknown (0), the deadband is skipped
so the command always goes through.

### 5.4 Error paths

| Event | State effect | Bus effect | Recovery |
|-------|--------------|------------|----------|
| Serial write/read EIO | `device_online:false`, `error` = raw message | `/state` publish (if changed) | automatic reopen via by-id path (§2.4), within a tick or two of the adapter returning |
| Response timeout (device silent) | first timeout: one silent reopen+retry; second: `device_online:false`, `error:"read response: read timeout"` or `"timeout waiting for response"` | `/state` publish | poll loop keeps retrying |
| Reply not `ok` (error/bad_params/invalid_command/debug) | state **unchanged**; error returned to caller; does not flip `device_online` | none directly | next poll |
| Serial reopen fails (adapter absent) | handle = nil, error surfaced | `/state` publish | lazy retry on every subsequent exchange/poll |
| MQTT connection lost | — | `/status` → `offline` via broker LWT | paho auto-reconnect; on reconnect full re-init (§3), retained `/cmd` re-delivered ⇒ last command re-applied |
| MQTT connect fails at startup | — | none; bridge runs web-only | none (no retry while running — restart needed) |
| Process crash | — | `/status` → `offline` (LWT) | systemd `Restart=on-failure`, 5 s |

Exact error strings, split by where each can appear (code is truth: only
transport-level `Exchange` errors reach state via `setOffline`; rejected-reply and parse
errors are returned to callers only):

**(a) CAN appear in `/state.error`:** `write request: <err>` (e.g.
`write request: Input/output error`), `read response: <err>` (e.g.
`read response: read timeout`), `reopen serial: <err>`,
`timeout waiting for response`, `device closed`, `EOF`, context-cancellation errors.

**(b) Returned to callers but NEVER stored in `/state.error`** (log / HTTP 502 bodies
only; a rejected command leaves state untouched): `status query reply com=<n>`,
`moving status reply com=<n>`, `retract reply com=<n>`, `frequency reply com=<n>`,
`status payload too short: have=<n> want>=12`,
`moving payload too short: have=<n> want>=4`, `controller rejected params`,
`invalid mode "<s>" (expected forward|reverse|bidirectional)`,
`current frequency unknown`.

### 5.5 Timing constants (complete list)

| Constant | Value |
|----------|-------|
| Poll interval | 2 s |
| status-query / moving-status exchange timeout | 2 s each (per send attempt) |
| retract / change-frequency exchange timeout | 5 s each |
| Frequency deadband | 25 kHz |
| Sequence number space | 0..127, wraps |
| MQTT jobs queue capacity | 256 (drop when full) |
| MQTT disconnect grace | 250 ms |
| HTTP shutdown budget | 5 s |
| systemd `RestartSec` | 5 s |
| Mock retract duration (test only) | 2000 ms |
| Web SSE channel buffers | 8 (status) / 32 (debug); slow consumers drop events |

---

## 6. Configuration

Single TOML file, default path `/etc/ultrabridge/config.toml` (override with `-config`).
Precedence: explicitly-set flag > file > built-in default. (Quirk: a flag explicitly set to
its default value still wins over the file.) Missing default-path file ⇒ run on defaults;
missing explicitly-given file or malformed TOML ⇒ fatal.

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
| `mqtt.discovery_prefix` | string | `homeassistant` | HA discovery tree prefix (legacy path only) |
| `mqtt.publish_ha_discovery` | bool | `false` | gate for legacy embedded HA discovery |
| `mqtt.user` | string | `""` | MQTT username (production `hf`) |
| `mqtt.password` | string | `""` | **secret**, stored in plaintext in this file |

Flags: `-config`, `-http`, `-port`, `-baud`, `-mqtt-broker`, `-mqtt-client-id`, `-mqtt-user`,
`-mqtt-password` (only those explicitly given override the file).

**Secret handling:** the MQTT password lives only in the TOML file, which deploy.sh installs
as `0600` owned by the service user; it never appears in the systemd unit, ExecStart, or the
process table. There is no environment-variable fallback.

---

## 7. Deployment

- Target: **shari**, Raspberry Pi, `io@192.168.1.139`, Linux arm64.
- `deploy.sh` (run from the project dir): cross-compiles
  `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"` into
  `dist/ultrabridge-linux-arm64`; scp binary, generated systemd unit, and a seed config
  (temp file written under `umask 077`) to the target; then remotely:
  - creates system user/group `ultrabridge` (nologin, no home) if missing;
  - ensures group `dialout` exists and adds the service user to it;
  - installs a **udev rule** `/etc/udev/rules.d/99-ultrabridge-serial.rules`:
    `SUBSYSTEM=="tty", SUBSYSTEMS=="usb", ATTRS{idVendor}=="0403", GROUP="dialout", MODE="0660"`
    (FTDI vendor 0403) so the serial device is always group-accessible regardless of distro
    default (plugdev vs dialout);
  - creates `/opt/ultrabridge` and `/etc/ultrabridge` (0755, owned by service user);
  - **seed-once config**: installs `/etc/ultrabridge/config.toml` (0600, service-user owned)
    **only if it does not already exist**; later deploys never touch it (the device owns its
    settings; edit on the device, or delete to re-seed);
  - installs the unit, `daemon-reload`, `enable`, `restart`, prints status.
- systemd unit (exact essentials): `After/Wants=network-online.target`; `Type=simple`;
  `ExecStart=/opt/ultrabridge/ultrabridge -config /etc/ultrabridge/config.toml` (no other
  args — everything comes from the file); `Restart=on-failure`, `RestartSec=5`;
  `User=Group=ultrabridge`, `SupplementaryGroups=dialout`;
  `ConfigurationDirectory=ultrabridge`; hardening: `NoNewPrivileges=true`,
  `ProtectSystem=full`, `ProtectHome=true`, `PrivateTmp=true`;
  device access: `DeviceAllow=char-ttyUSB rw`, `DeviceAllow=char-ttyACM rw`,
  `DeviceAllow=char-tty rw`.
- Logs: `journalctl -u ultrabridge -f`. Ops: `sudo systemctl restart ultrabridge`.
- Runtime dependencies: MQTT broker at `192.168.1.50:1883` (user `hf`); the FTDI adapter on
  USB; network reachability. The HTTP UI listens on all interfaces at :8080 in production.
- `build.sh` additionally builds a Windows amd64 binary (dev convenience only).

---

## 8. Invariants & safety rules

**Must never be violated by a re-implementation:**

1. **Antenna commands are serialized.** Exactly one serial exchange in flight at any time
   (device-wide lock); poll queries and commands interleave but never overlap on the wire.
2. **`freq_hz` on the bus is Hz, integer.** The device speaks kHz (uint16); the ×1000
   conversion happens only inside the bridge. Never publish kHz/MHz.
3. **`direction` is never named `mode`.** Bus `mode` is the radio emission-mode vocabulary;
   the deprecated `mode` *command alias* must remain accepted for backward compatibility.
4. **`device_online` is always present in `/state`** — consumers must distinguish "bridge
   online, device down" from "no data". Bridge liveness is `/status` (LWT) only.
5. **`retract` is one-shot:** after execution the retained `/cmd` is cleared (empty retained
   publish) so it cannot re-fire on restart. All other commands stay retained and are
   re-applied on reconnect — intentional (actuator self-healing).
6. **Retract is the antenna's safe state** (elements fully in). External safety logic (the
   `antennaselect` reconciler) drives grounding/antenna selection; this bridge merely
   executes `retract` faithfully with a 5 s timeout and verifies the reply is `ok`.
7. **A dead device must never take the bridge down.** Serial errors flip
   `device_online:false` and surface in `error`; the process stays up, keeps polling, keeps
   serving MQTT/web, and self-heals (§2.4).
8. **At most one reopen+retry per exchange** — no unbounded retry loops against a broken
   link; the 2 s poll is the recovery driver.
9. **The 25 kHz deadband** (§5.3) with the direction-change and unknown-frequency bypasses,
   exactly as specified.
10. **MQTT handlers must never block** on serial I/O or a blocking Publish inside the
    client's dispatch callback; all command work is serialized through a bounded worker queue
    (commands execute in arrival order). Station-wide lesson from a live deadlock in a
    sibling component.
11. **Connect must be cancellable** (a shutdown signal interrupts a hung MQTT connect).
12. **Secrets never on the command line** — password only in the 0600 TOML file.
13. **Checksum/framing errors** on received packets are rejected; stale-sequence replies are
    discarded rather than misattributed to a new request.
14. Frequency commands are bounded to the uint16 kHz domain (max 65535 kHz = 65.535 MHz);
    the web layer enforces 1..65535, the MQTT layer relies on truncation (see fragilities).

---

## 9. Known defects & fragilities

1. **SIGTERM is not handled** — `signal.NotifyContext(…, os.Interrupt)` traps SIGINT only.
   systemd stops services with SIGTERM, so every `systemctl stop/restart` kills the process
   without the graceful path: no self-published `offline`, no HTTP drain. The broker LWT
   covers `/status=offline`, so bus consumers see no difference, but the graceful-shutdown
   code is effectively dead in production.
2. **A response timeout also triggers the reopen+retry path.** The reader's per-byte
   deadline check returns a non-nil "read timeout" error, which the exchange treats as a
   port fault (close port, reopen by-id, re-send once). The code comment says the opposite
   ("a non-nil read error is a port-level fault, not a timeout"). Harmless (bounded to one
   retry) but a reimplementation should decide deliberately: a *slow* device causes
   serial-port churn on every timeout.
3. **`freq_hz` truncation on MQTT `frequency` commands.** `khz := uint16(cmd.FreqHz / 1000)`:
   values above 65.535 MHz wrap modulo 65536 (e.g. 71 MHz → 5.6 MHz sent), and sub-kHz
   precision is silently dropped. No range validation on the MQTT path (unlike the web
   path's 1..65535 check).
4. **No command acknowledgment channel.** Invalid direction, unknown band, rejected
   params, or timeouts on a `/cmd` are only logged. A rejected command does not set
   `/state.error` (reply-not-OK leaves state untouched); only transport failures do. A
   command sender must diff `/state`.
5. **Jobs queue drop semantics.** If > 256 commands are pending, new ones are silently
   dropped (deliberate, to protect the MQTT dispatch thread); a dropped command is
   invisible. On shutdown the worker stops and queued-but-unexecuted jobs are lost — for a
   lost *retract* job the retained `/cmd` was not cleared, so the retract re-executes on
   the next start.
6. **Poll-tick stretching.** With the device fully absent, each tick can consume up to ~2 s
   of timeout per query (plus one internal reopen+retry each), so the effective poll period
   degrades to ~4–8 s and the 2 s ticker silently drops beats (no queueing). Consumers must
   not assume a hard 2 s cadence.
7. **Parsed-but-unused status fields:** per-motor bits, min/max freq, firmware version,
   flags, progress units never reach the bus — a single stuck motor only shows as generic
   `moving:true` or device silence.
8. **DLE-escape asymmetry** (`b & 0x7F` encode vs `b | 0x80` decode) cannot round-trip
   payload bytes ≥ 0x80 that collide with framing constants. Irrelevant for current
   traffic; a trap if `modify_element_len` (0x0C) or `debug` payloads ever carry high-bit
   bytes.
9. **Web UI has no authentication** and production binds `0.0.0.0:8080` — anyone on the LAN
   can move the antenna via `/api/*`. Same trust level as the rest of the shack LAN, but
   worth stating in a security-sensitive rebuild.
10. **Doc lags (code is truth):** `ultrabeam-mqtt-api.md` still shows `"area": "Radio
    shack"` in `expose.device` (code omits it); README's MQTT example uses user `ham`
    (production user is `hf`); README_API's `/api/status` example omits `band_index`,
    `motor_bits`, `last_error` that the endpoint returns.
11. **Dead protocol surface:** command `0x0C modify_element_len` and the error value
    `unsupported write command` are declared but never used; `ReplyDebug` (0x40) is defined
    but only ever treated as "not ok".
12. **Initial-refresh race:** the 2 s poll goroutine starts before the one-shot initial
    refresh; under slow startup the first tick can interleave. Harmless (mutex-guarded) —
    implementation detail.
13. **FTDI re-enumeration root cause is environmental** (USB bus glitches correlate with
    MQTT dropouts on the same host). The self-heal masks it; it does not fix the flaky USB.
    The stable by-id path is load-bearing — a config pointing at `/dev/ttyUSB0` would break
    self-heal (reopen would reopen the *name*, which may no longer exist or may name the
    other port of the dual adapter).

---

## 10. Re-implementation notes

**Preserve verbatim (the behavior contract):**

- The four topic strings and plane semantics, QoS 1 + retained on all four, LWT
  `offline`/`online` on `/status`, clean-session=false, client-ID derivation.
- Exact `/meta` JSON including `capabilities` and `expose` (field keys, `options_ref`,
  min/max/step 1800000/54000000/1000 — consumers depend on them).
- Exact `/state` field names (`ts`, `freq_hz` in Hz, `band`, `direction`, `moving`,
  `device_online` always present, `error` omitempty), change-only publishing, the
  band-label and band-center tables with their exact kHz boundaries.
- `/cmd` payload grammar (`action`, `value`, `freq_hz`), all five actions including the
  `mode` alias, band-value trimming, retract's retained-topic clearing, re-application of
  retained commands on reconnect, unknown-command dropping.
- Serial framing (STX/DLE/ETX values, escaping, checksum algorithm and seed), command/reply
  byte values, little-endian kHz payloads, status payload layout (≥12/≥4 bytes), direction
  byte mapping, 7-bit sequence numbering with reply matching.
- Exchange timeouts (2 s status/moving, 5 s retract/change), the 2 s poll, the 25 kHz
  deadband with its bypasses.
- **The EIO self-heal exactly as specified in §2.4** — opener bound to the by-id path, lazy
  reopen, one reopen+retry per exchange, nil-handle retry on the next poll, error surfaced
  while the bridge stays up.
- The serialization invariant (one exchange at a time; commands ordered), non-blocking MQTT
  handlers, cancellable connect.
- Config precedence rules, seed-once deployment, 0600 secret handling, systemd hardening,
  the udev group rule, serial_port defaulting to the by-id path in production.

**Free to change (implementation detail):**

- Language/framework/libraries. (Go, paho, go.bug.st/serial are incidental — but note they
  carry the specific pitfalls whose *workarounds* must be reproduced: paho's non-cancellable
  connect and inline handler dispatch; the serial lib's blocking reads.)
- Web UI look, SSE mechanics, debug-trace format (keep endpoint semantics if ops tooling
  depends on them; they are secondary to the bus contract).
- Internal State struct shape; the mock device (keep some hardware-less test double).
- Log wording — except error strings that leak into `/state.error`, which consumers may
  match on.
- The embedded/legacy HA discovery block — already gated off; a clean rebuild can drop it
  (keep `expose` and the `homeassistant/status` birth-triggered state republish attempt).
- `build.sh`'s Windows target and the build flags.

**Things the code cannot tell you (verify with the operator before freezing the PRD):**

- Actual live `serial_port` value and MQTT credentials on shari; whether any consumer
  currently relies on the legacy embedded HA discovery or still sends the `mode` alias.
- Whether the RCU-06 firmware ever emits unsolicited frames (the reader resyncs on STX, so
  it tolerates them, but there is no evidence either way).
- Real-world retract duration and whether the 5 s retract timeout is ever exceeded (the
  mock simulates 2 s; no field data in the repo).