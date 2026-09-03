# Salvage: ultrabridge.md
> Extracted from PRD/03-components/ultrabridge.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [protocol] RCU-06 serial transport and HDLC framing

**Physical link:** USB-serial adapter, FTDI "Dual RS232" (two ports, the RCU-06 sits on
interface 0). It appears on the host as `/dev/ttyUSBn`.

**Serial line parameters (must be):** 19200 baud, 8 data bits, no parity, 1 stop bit
(8N1), no flow control.

**Port path (must be):** the configured path must be the **stable by-id symlink**
`/dev/serial/by-id/usb-FTDI_Dual_RS232-if00-port0`. The adapter intermittently drops and
**re-enumerates under a different tty name** (observed live: ttyUSB0/ttyUSB2 →
ttyUSB1/ttyUSB3, correlated with whole-USB-bus bounces). The by-id symlink always resolves
to the tty the kernel assigns at that moment. The self-heal (below) depends on it.

The bridge opens the port **without a read timeout**: a read blocks until a byte arrives
or the link drops. The caller enforces request and response deadlines. **All requests are
strictly serialized** — exactly one request/response exchange in flight on the wire at any
time (a device-wide lock). The 2-second poll and MQTT/web commands interleave but never
overlap.

Byte-oriented link with an HDLC-like frame. Constants:

| Name | Value |
|------|-------|
| STX (start of frame) | `0xF5` |
| DLE (escape byte) | `0xF6` |
| ETX (end of frame) | `0xFA` |
| checksum seed | `0x55` |

**Frame layout:** `STX <escaped payload> ETX`, where the unescaped payload is
`Seq(1 byte) | Com(1 byte) | Data(0..n bytes) | Checksum(1 byte)`.

**Checksum (exactly):** start at `0x55`. For each payload byte `b`:
`chk = chk XOR b; chk = chk + 1` (mod 256). The bridge appends the result as the last
payload byte. On receive, recompute over everything except the last byte; a mismatch
rejects the frame.

**Byte stuffing (exactly):** send any payload byte equal to STX, ETX, or DLE as the two
bytes `DLE, (b & 0x7F)`. On receive, restore the byte following a DLE as `(b | 0x80)`.
Note the asymmetry (encode masks bit 7, decode sets bit 7). The round-trip loses payload
bytes ≥ 0x80 that collide with a framing constant after masking. In practice all payload
bytes of this protocol are < 0x80.

**Minimum frame:** the payload holds ≥ 3 bytes (`Seq | Com | Checksum`); shorter frames
are rejected ("packet too short").

**Resync:** the frame reader is a byte-at-a-time state machine that resyncs on STX, so it
tolerates stale/unsolicited bytes. Preserve this — it costs nothing and tolerates
unsolicited debug frames (it is not known whether the RCU-06 firmware ever emits
unsolicited frames; no evidence either way).

## [protocol] Command bytes, reply bytes, and payload offsets

**Seq** is a bridge-side counter starting at 0, incremented per request, wrapped to 7 bits
(`0..127`). The reply carries the same Seq. A reply with a mismatched Seq is discarded and
the reader keeps reading (tolerates stale bytes from a previous timed-out exchange).

Request command bytes:

| Byte | Name | Meaning | Request data | Exchange timeout |
|------|------|---------|--------------|-------------------|
| `0x01` | status_query | read full status | none | 2 s |
| `0x02` | retract | fully retract all elements | none | 5 s |
| `0x03` | change_frequency | tune to frequency + set direction | 3 bytes: freq_kHz LSB, freq_kHz MSB (little-endian uint16), direction byte | 5 s |
| `0x0A` | moving_status | ask how far motors still have to travel | none | 2 s |
| `0x0C` | `modify_element_len` | directly command element lengths | — | defined but never used |

Reply command bytes:

| Byte | Name | Meaning |
|------|------|---------|
| `0x00` | ok | success. Data carries the response payload (status queries) or stays empty |
| `0x14` (20) | error | device error |
| `0x1E` (30) | bad_params | request rejected (for example `change_frequency` with < 3 data bytes) |
| `0x28` (40) | invalid_command | unknown command byte |
| `0x40` (64) | debug | unsolicited debug message (treated as "not ok", ignored) |

**status_query reply payload (≥ 12 bytes, else reject "status payload too short"):**

| Offset | Field | Meaning |
|--------|-------|---------|
| 0 | firmware minor | controller firmware version, low byte |
| 1 | firmware major | controller firmware version, high byte |
| 2 | operation | current operation code (not interpreted) |
| 3–4 | frequency kHz | current tuned frequency, little-endian uint16, in kHz |
| 5 | band index | device-internal band index |
| 6 | orientation | direction byte. The value is the low nibble (`& 0x0F`) |
| 7 | flags1 | not interpreted |
| 8 | flags2 | not interpreted |
| 9 | motor bits | nonzero ⇒ at least one motor moves |
| 10 | min freq MHz | not interpreted |
| 11 | max freq MHz | not interpreted |

**moving_status reply payload (≥ 4 bytes):**

| Offset | Field | Meaning |
|--------|-------|---------|
| 0–1 | total distance mm | little-endian uint16. **Travel distance still left**, in millimeters. 0 ⇒ motors idle |
| 2–3 | progress units | little-endian uint16. Parsed, not interpreted |

**Direction byte mapping (exactly):**

| Wire byte | Meaning | Bus vocabulary |
|-----------|---------|----------------|
| `0x00` | normal | `forward` (aliases `""`, `normal`) |
| `0x01` | 180° | `reverse` (alias `180`) |
| `0x02` | bidirectional | `bidirectional` (alias `bidir`) |

Any other input string is rejected with `invalid mode "<s>" (expected
forward|reverse|bidirectional)`. Unknown wire values map to the label `unknown`.

Dead protocol surface (defined, unused): command `0x0C modify_element_len` and the error
value `unsupported write command`; `ReplyDebug` (0x40) is only ever treated as "not ok".
A trap if these ever carry high-bit bytes (see the DLE asymmetry above).

## [requirement] Connection-loss detection and the EIO self-heal

Hard requirement distilled from a live incident. Not a nice-to-have.

**The incident.** The FTDI adapter randomly disconnects (a USB bus glitch). The kernel
re-enumerates it under a **new tty name**. The long-lived bridge process still held the
file descriptor of the now-deleted device node. Every later write then failed with
`write request: Input/output error` (EIO — the OS error for a broken device link). Before
the fix the bridge kept accepting MQTT commands but was never able to actuate the antenna
until someone restarted it manually. Root cause of the drop is environmental (USB bus
glitches correlate with MQTT dropouts on the same host) — the self-heal masks it, it does
not fix the flaky USB. The by-id path is load-bearing: a config pointing at
`/dev/ttyUSB0` breaks self-heal (a reopen then resolves the *name*, which can be gone or
can name the other port of the dual adapter).

**Requirements (must hold):**

1. The bridge opens the configured port (the stable by-id path) at startup. It retains a
   re-usable *opener* bound to that path and baud. Later code can re-resolve the path
   through that opener.
2. Every exchange runs under the device-wide lock. If the current handle is `nil` (previous
   reopen failed, or adapter absent), the bridge tries the opener first at the start of the
   exchange. The link re-establishes **lazily**, driven by the 2 s poll loop. No separate
   reconnect thread.
3. **Write fault:** if writing a request returns an error (EIO and so on), the bridge
   closes the stale handle (best-effort) and calls the opener — which **re-resolves the
   by-id symlink to the freshly attached tty**. If reopen succeeded, it re-sends the
   request once with a **fresh sequence number**.
4. **Read fault:** the port has no read timeout, so a returned read *error* means a
   port-level fault (link drop, EOF, port closed). This differs from mere absence of data.
   Same recovery: close, reopen through the by-id path, re-send once with a fresh Seq.
5. **Retry bound:** at most **one reopen per exchange**. A second fault within the same
   exchange goes to the caller. The next poll tick retries. This stops a tight reopen loop
   against a persistently broken link.
6. **Failed reopen:** if the adapter is not back yet, the bridge sets the internal handle
   to `nil` and surfaces the error. The next exchange (poll tick) tries the reopen again.
   Net requirement: **recovery within one to two 2-second poll ticks of the adapter
   reappearing, with no manual restart**.
7. **Bus-visible behavior during outage:** `/state` carries `device_online:false` and
   `error:"…"` while `/status` **stays `online`** (the bridge process is healthy). On the
   first successful exchange afterwards the bridge overwrites the state with fresh device
   data, sets `device_online:true`, and clears the error field.

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

**Exactness note:** the inner read returns a non-nil error also when the caller-level
deadline expires. So **a pure response timeout also triggers exactly one reopen + re-send**
before the bridge surfaces `read response: read timeout`. A re-implementation can keep
this behavior — bounded and harmless — or deliberately distinguish timeouts from port
faults. The choice must be a conscious one, because a *slow* device otherwise causes
serial-port churn on every timeout. The 2 s / 5 s timeouts apply to each send try.

**Startup is fail-fast only for the first open**: if the bridge cannot open the port at
boot, the process prints the error to stderr and exits 1 (the service manager restarts it).

Code-check verdict (2026-09-03): implemented and deployed —
`ultrabridge/internal/ub/transport/serial_open.go` + `device.go` (opener bound to by-id
path, one-retry bound, lazy reopen on nil handle), with tests in
`internal/ub/transport/device_reopen_test.go`.

## [requirement] Hardware-less mock device (conformance script)

A re-implementation must provide an in-process mock device usable when `serial_port` is
empty (or by a same-behavior switch). It implements the same framing and command set, with
this exact scripted behavior (it doubles as a conformance script for the framing layer):

- first state: 14000 kHz, band index 4, direction forward, motors idle, min/max freq
  7/30 MHz.
- `retract` succeeds immediately and reports motors moving for 2000 ms afterwards (then
  clears), and resets frequency to 14000 kHz.
- `change_frequency` with < 3 data bytes replies bad_params. Unknown commands reply
  invalid_command.

Code-check verdict (2026-09-03): implemented —
`ultrabridge/internal/ub/transport/mock_client.go` (retractDurationMs = 2000).

## [requirement] The 25 kHz deadband (behavior contract)

The bridge **suppresses** a change_frequency command (silently, treated as success) when
all of the following hold:

- the current known frequency is nonzero,
- `|requested − current| < 25 kHz` (a difference of exactly 25 kHz goes through),
- the direction byte equals the current direction.

Rationale: re-driving the motors for a sub-25 kHz retune only wears them and churns the
bus. 25 kHz matches the debug UI's nudge step, so one nudge still passes. A pure direction
change always bypasses the deadband (a direction flip at the same frequency must reach the
device). When the current frequency is unknown (0), the bridge skips the deadband so the
command always goes through.

Code-check verdict (2026-09-03): implemented —
`ultrabridge/internal/ub/service/controller.go` (deadband in SetFrequency, bypass in
SetMode), tests in `controller_test.go`. Not documented in `ultrabeam-mqtt-api.md` —
consider adding it there.

## [defect] SIGTERM is not trapped (graceful shutdown dead in production)

The code traps only SIGINT and ignores SIGTERM (what systemd sends on `stop`/`restart`).
A service stop then kills the process without the graceful path: no self-published
`offline`, no HTTP drain. The broker LWT covers `/status=offline`, so bus consumers see no
difference, but the graceful-shutdown code is effectively dead in production.

Verdict: still open — `cmd/ultrabridge/main.go:46` registers
`signal.NotifyContext(context.Background(), os.Interrupt)` only; no SIGTERM handler
anywhere in `ultrabridge/`.

## [defect] A response timeout also triggers the reopen+retry path

A response timeout produces a read error (go.bug.st/serial port timeout), so the EIO
self-heal reopens and re-sends once on every pure timeout — contradicting an in-code
comment. Harmless (bounded to one retry), but a *slow* device causes serial-port churn on
every timeout. A rebuild must decide deliberately: keep this or distinguish timeouts from
port faults (see the open decision below).

Verdict: still open — `internal/ub/transport/device.go:115,144` treat any read/write error
as a port fault eligible for one reopen.

## [defect] freq_hz truncation/wrap on MQTT frequency commands

`khz := uint16(cmd.FreqHz / 1000)` — values above 65.535 MHz wrap modulo 65536 (for
example 71 MHz → 5.6 MHz sent), and the command silently drops sub-kHz precision. No
range validation on the MQTT path (unlike the web path's 1..65535 kHz check). Frequency
stays inside the uint16 kHz domain (max 65535 kHz = 65.535 MHz); a rebuild must add an
explicit `0 < freq_hz ≤ 65535000` check.

Verdict: still open — `internal/mqtt/client.go:488` still truncates without a range check.

## [defect] No command acknowledgment channel

Invalid direction, unknown band, rejected params, or timeouts on a `/cmd` only produce log
lines — no ack/nack topic. A rejected command does not set `/state.error`; only transport
failures do. A command sender must check success by diffing `/state`. Strings the bridge
returns to command callers (HTTP 502 bodies and the log, never `/state.error`):
`status query reply com=<n>`, `moving status reply com=<n>`, `retract reply com=<n>`,
`frequency reply com=<n>`, `status payload too short: have=<n> want>=12`,
`moving payload too short: have=<n> want>=4`, `controller rejected params`,
`invalid mode "<s>" (expected forward|reverse|bidirectional)`, `current frequency unknown`.
Strings that CAN appear in `/state.error` (transport-level only): `write request: <err>`,
`read response: <err>`, `reopen serial: <err>`, `timeout waiting for response`,
`device closed`, `EOF`, a context-cancellation error.

Verdict: still open — `internal/mqtt/client.go:488–503` discard controller errors with
`_ =`; no ack topic exists.

## [defect] Command-queue drop semantics

If more than 256 commands wait in the bounded queue, the bridge silently drops new ones. A
dropped command stays invisible. On shutdown, the worker loses queued-but-unexecuted jobs
— for a lost *retract* job the retained `/cmd` was not cleared, so the retract re-executes
on the next start.

Verdict: partially changed — the queue is still bounded at 256 with silent drops
(`internal/mqtt/client.go:124`), but the retract-specific consequence is gone: since the
2026-09-03 outage fixes, **every** command is one-shot (the retained `/cmd` topic is
cleared after the worker has acted on it, executed or rejected — PRD §6.1's
"non-retract commands stay retained and re-execute on every reconnect" contract is
superseded; see `ultrabridge/CLAUDE.md` and `ultrabeam-mqtt-api.md`). Note the residual
race documented in CLAUDE.md: a connection drop in the execution→clear window still leaves
the command retained and replays it once.

## [defect] DLE-escape asymmetry

Encode `b & 0x7F` vs decode `b | 0x80` cannot round-trip payload bytes ≥ 0x80 that collide
with framing constants. Irrelevant for current traffic. A trap if the unused
`modify_element_len` (0x0C) or `debug` payloads ever carry high-bit bytes.

Verdict: still open (inherent to the on-wire protocol) —
`internal/ub/protocol/packet.go:66,82` still use the asymmetric pair.

## [defect] Web UI has no authentication

The HTTP UI has no authentication and production binds `0.0.0.0:8080` — anyone on the LAN
can move the antenna through `/api/*`. Same trust level as the rest of the shack LAN, but
a security-sensitive rebuild must at least document or gate it.

Verdict: still open — no auth of any kind in `internal/web/`.

## [defect] Parsed-but-unused status fields

Per-motor bits, min/max freq, firmware version, flags, and progress units never reach the
bus. So a single stuck motor only shows as generic `moving:true` or device silence.

Verdict: still open (state contract unchanged — `/state` carries only the fields in
`ultrabeam-mqtt-api.md`).

## [defect] First-refresh race

The poll loop starts before the one-shot first refresh. Under slow startup the first tick
can interleave. Harmless (lock-guarded). Recorded as a fact to keep or consciously fix.

Verdict: not re-verified in code (PRD marks it harmless and lock-guarded).

## [decision] Ultrabeam antenna-switch port (3 vs 4) — unresolved on-device

The repo-root documentation and station-integration docs put the Ultrabeam beam on
antenna-switch port 3. The antennaselect seeded config and the console's antenna map put
it on port 4. The live config on shari is authoritative, but the PRD's research never read
it. The bridge itself is agnostic (it tunes the antenna — the switch routes it), but any
re-integration must confirm the port on-device.

Verdict (2026-09-03): still conflicting — root `CLAUDE.md` says `ant/ultrabeam` (port 3)
while `antennaselect/config.example.toml` maps `port4 = "ultrabeam"`. Needs an operator
check against the live shari config.

## [decision] Real-world retract duration vs the 5 s exchange timeout

Whether the 5 s retract exchange timeout is ever exceeded in the field is unknown (the
mock simulates 2 s of motor motion; the repo contains no field data). If real retracts
take longer than 5 s to *acknowledge* (as opposed to complete), the timeout needs
retuning. The device's motors keep moving after the `ok` reply; the 2 s poll's `moving`
flag is the completion signal. Needs field data / a human at the device.

## [decision] Timeout-vs-fault discrimination

The reference treats a response timeout as a port fault (one reopen+retry). Keep this or
distinguish timeouts from EIO deliberately — unresolved. Current behavior is harmless but
causes port churn with a merely slow device.

## [decision] Range-validation design on the MQTT path, coupled to the missing ack channel

An explicit `0 < freq_hz ≤ 65535000` check is required on the MQTT `frequency` action;
whether out-of-range commands are dropped silently or surfaced somehow stays an open
design decision coupled to the missing-acknowledgment defect (there is no ack channel).

## [decision] Unknown device-side behaviors to settle with the hardware

1. **Does the RCU-06 firmware ever emit unsolicited frames?** The frame reader resyncs on
   STX, so it tolerates them, but there is no evidence either way. Preserve STX-resync
   behavior regardless (it costs nothing and tolerates unsolicited debug frames).
2. **Legacy embedded HA discovery reliance:** whether any live consumer still relies on the
   legacy embedded HA-discovery path (`mqtt.publish_ha_discovery`), or whether anything
   still sends the deprecated `mode` command alias, was never confirmed against the live
   shari config; check with the operator before freezing a rebuild.