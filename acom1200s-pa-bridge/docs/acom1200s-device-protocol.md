# Salvage: acom1200s-pa-bridge.md
> Extracted from PRD/03-components/acom1200s-pa-bridge.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [protocol] Serial transport, frame format, checksum, resync and ACK

### Transport (§2.1)

- The bridge reaches the amplifier's RS-232 service port through a USB-serial adapter (Prolific
  chip, USB vendor id `067b`) and opens it by device path. Default path:
  `/dev/serial/by-id/usb-Prolific_Technology_Inc._USB-Serial_Controller_D-if00-port0`
  (a stable-by-id symlink that survives adapter replugs). A udev rule pins this adapter's tty to
  group `dialout`.
- Serial parameters are protocol constants, **not configurable**: **9600 baud, 8 data bits, no
  parity, 1 stop bit (9600 8N1)**.
- Read timeout on the port: **1 second** (a read returns 0 bytes after 1 s of silence).
- On open, the bridge must reset the serial input/output buffers and then send the
  enable-telemetry command.

**Authoritative vendor reference**: the vendor documents the protocol in the PDF
**"ACOM A600S A1200S Serial Protocol V1.3.pdf"**. **The file is not in the repo** — the byte-level
tables below are what the reference implementation actually puts on the wire. (Code-check
2026-09-03: `acom1200s-pa-bridge/CLAUDE.md` §Protocol reference points at
`docs/ACOM A600S A1200S Serial Protocol V1.3.pdf`, but `acom1200s-pa-bridge/docs/` holds only
`pa-mqtt-api.md` and `future-cat-emulator-band-follow.md` — the pointer is broken and no copy of
the PDF exists anywhere in the repo.)

### Frame format (§2.2)

All frames, both directions, share this layout:

| Offset | Meaning |
|---|---|
| byte 0 | start byte, always `0x55`. |
| byte 1 | message address / message type. |
| byte 2 | total frame length **including** this length byte and the trailing checksum byte. |
| bytes 3..n−2 | payload. |
| last byte | checksum. |

**Checksum rule**: the sender chooses the checksum byte so that the **sum of all bytes in the
frame, mod 256, is zero**. A received frame is valid only if its declared length ≥ 4 and the
byte-sum is 0 (mod 256). The sender computes the checksum as `0 − sum(all other bytes)` (mod 256)
and appends it. The smallest valid frame is 4 bytes.

**Exception [critical quirk]**: the sender transmits the enable-telemetry frame as a fixed
4-byte sequence with **no checksum byte**. Any re-implementation that blindly appends checksums to
every frame corrupts the enable command. (Code: `internal/acom/device.go` `write()` skips the
checksum for `MsgEnableAuto`.)

### Receive path: resynchronization and ACK (§2.3)

On the incoming byte stream the bridge must resynchronize byte-exactly as follows:

1. Scan forward one byte at a time until the stream shows a `0x55` start byte.
2. Read the declared length (byte 2). If the length is < 4, treat the data as noise: remove **one
   byte** and rescan.
3. If the buffer holds fewer bytes than the declared length, wait for more data (keep the
   remainder).
4. When a full frame is available, check the checksum (byte-sum == 0 mod 256).
   - Checksum **fails** → remove one byte and rescan (never remove the whole buffer).
   - Checksum **passes** → handle the frame and remove exactly its declared length from the
     buffer.

For every valid frame, regardless of type, the bridge must send an **ACK frame**:
`{0x55, 0x86, 0x05, <byte-1-of-received-frame>, <checksum>}`. Layout: start `0x55`, type `0x86`,
length `0x05`, the received frame's address byte echoed, then the zeroing checksum byte.

Frames with address `0x2F` are **telemetry frames**. The bridge must ACK all other frame types and
otherwise ignore them. (Whether the amplifier sends ACKs back for outbound commands is unknown —
see the open decisions below.)

### Enable-telemetry command (§2.4)

The bridge sends this frame on port open, and re-sends it during silence. Exact bytes on the wire:

```
55 92 04 15
```

(start `0x55`, type `0x92` = "enable automatic telemetry", length `0x04`, payload byte `0x15`.
The vendor document does not explain `0x15`). **The bridge appends no checksum byte.** Once the
amplifier receives this frame, it streams 72-byte telemetry frames automatically.

### Silence handling and connection-loss detection (§2.6)

Within one open-port session:

- Any byte received resets the silence timer.
- On each 1 s read timeout with no bytes:
  - If silence has lasted **more than 30 s**: the session ends with error text
    `no data received for 30s, restarting monitor` → device marked offline, port closed. The
    bridge restarts with backoff.
  - Else, if silence has lasted **more than 5 s** and the last retry was more than 5 s ago:
    re-send the enable-telemetry command (`55 92 04 15`) and log
    `no data for 5s, re-sending enable telemetry`. This repeats at most once per 5 s until data
    resumes or the 30 s limit trips.
- A read error (port gone, USB replug) ends the session immediately. A shutdown signal also ends
  the session (by closing the port, unblocking the read).

Practical effect: when the operator powers the amplifier off through the `hf/switch` relays, the
port goes silent and the session exits after 30 s and publishes `device_online:false`. When power
returns, the next restart cycle re-opens the port and re-sends enable-telemetry. Telemetry then
resumes without operator action.

## [protocol] Telemetry frame decode — 72-byte `0x2F` frame, offsets, mode and band tables

The bridge decodes a telemetry frame **only if its total length is exactly 72 bytes**. It
silently drops other lengths. All multi-byte values are **little-endian**. (Byte offsets and both
tables below verified against `internal/acom/parser.go` and `internal/acom/decoders.go`,
2026-09-03.)

| Byte offsets | Type | Meaning |
|---|---|---|
| `[3]` | byte | firmware mode. Decode by **high nibble** (`b & 0xF0`) — table below. |
| `[16:18]` | uint16 LE | heatsink temperature in **Kelvin**. Published value = K − 273.15. Raw 0 decodes to 0.0 °C (special case, no −273.15). |
| `[20:22]` | uint16 LE | input (drive) power in **deciwatts** → /10.0 W — parsed but **never published**. |
| `[22:24]` | uint16 LE | forward power in W (raw, before averaging). |
| `[24:26]` | uint16 LE | reflected power in W. |
| `[26:28]` | uint16 LE | SWR raw → /100.0 (raw 120 → 1.2). |
| `[48:50]` | uint16 LE | frequency in **kHz** — parsed but **never published** (the PA slot does not own bus frequency. The radio slot owns `freq_hz` in Hz). |
| `[66]` | byte | fault byte. `0xFF` = no fault. |
| `[69]` | byte | band. Decode by **low nibble** (`b & 0x0F`), 1..10 per table below. Anything else (0, 11–15) → band label `UNK`, band index 0 (unknown). |

**Firmware mode table** (`[3] & 0xF0` → raw string, published verbatim as the `pa_state` field):

| High nibble | Raw mode string |
|---|---|
| `0x10` | `RESET` |
| `0x20` | `INIT` |
| `0x30` | `DEBUG` |
| `0x40` | `SERVICE` |
| `0x50` | `STANDBY` |
| `0x60` | `OPR/RX` |
| `0x70` | `OPR/TX` |
| `0x80` | `ATAC` |
| `0x90` | `MENU` |
| `0xA0` | `OFF` |
| anything else | `UNKNOWN` |

**Band table** (low nibble of `[69]` → canonical label and index):

| Nibble | Label | Index |
|---|---|---|
| 1 | `160m`. | 1 |
| 2 | `80m`. | 2 |
| 3 | `40m`. | 3 |
| 4 | `30m`. | 4 |
| 5 | `20m`. | 5 |
| 6 | `17m`. | 6 |
| 7 | `15m`. | 7 |
| 8 | `12m`. | 8 |
| 9 | `10m`. | 9 |
| 10 (0x0A) | `6m`. | 10 |
| 0, 11–15 | `UNK`. | 0 (unknown) |

These ten bands are the amplifier's complete set and are exactly what `set_band` accepts.

## [protocol] Fault table — fault byte → verbatim message (~90 entries)

When the fault byte differs from `0xFF`, the bridge publishes the exact string below in the state
`error` field (verified present in `internal/acom/decoders.go` `decodeError`):

- `0xFF` → `NONE` (no fault. The bridge publishes `error` empty)
- `0x00` → `HOT SWITCHING ATTEMPT` (a try to switch the RF routing while the amplifier transmits)
- `0x01` → `OUTPUT RELAY CLOSED (SHOULD BE OPEN)`
- `0x02` → `OUTPUT RELAY OPEN (SHOULD BE CLOSED)`
- `0x03` → `DRIVE POWER WRONG TIME`
- `0x04` → `REFLECTED POWER WARNING`
- `0x05` → `EXCESSIVE REFLECTED POWER`
- `0x06` → `DRIVE POWER TOO HIGH`
- `0x07` → `EXCESSIVE DRIVE POWER`
- `0x08` → `HOT SWITCHING ATTEMPT (2)`
- `0x09` → `DRIVE FREQUENCY OUT OF RANGE`
- `0x0A` → `FREQUENCY VIOLATION`
- `0x0B` → `OUTPUT DISBALANCE`
- `0x0C` → `DETECTED RF POWER WRONG TIME`
- `0x0D` → `PA LOAD SWR TOO HIGH`
- `0x0E` → `STOP TRANSMISSION FIRST`
- `0x0F` → `REMOVE DRIVE POWER IMMEDIATELY`
- `0x10` → `5V TOO LOW`. `0x11` → `5V TOO HIGH`
- `0x12` → `26V TOO LOW`. `0x13` → `26V TOO HIGH`
- `0x14` → `ERROR 0x14`
- `0x15` → `PAM1 FAN SPEED TOO LOW`. `0x16` → `PAM2 FAN SPEED TOO LOW`. `0x17` → `LPF FAN SPEED TOO LOW`
  (PAM1/PAM2 = the two power-amplifier modules. LPF = low-pass filter)
- `0x18` → `PAM1 DISSIPATION TOO HIGH`. `0x19` → `PAM2 DISSIPATION TOO HIGH`
- `0x1A` → `PAM1 DISSIPATION WARNING`. `0x1B` → `PAM2 DISSIPATION WARNING`
- `0x1C` → `PAM1 TEMP TOO HIGH`. `0x1D` → `PAM2 TEMP TOO HIGH`
- `0x1E` → `PAM1 EXCESSIVE TEMP`. `0x1F` → `PAM2 EXCESSIVE TEMP`
- `0x20` → `PAM1 HV TOO LOW`. `0x21` → `PAM1 HV TOO HIGH` (HV = high-voltage supply)
- `0x22` → `PAM1 CURRENT NON-ZERO`. `0x23` → `PAM1 IDLE CURRENT TOO LOW`
- `0x24` → `PAM1 CURRENT WARNING`. `0x25` → `PAM1 EXCESSIVE CURRENT`
- `0x26`–`0x29` → `BIAS_1A VOLTAGE ERROR`, `BIAS_1B VOLTAGE ERROR`, `BIAS_1C VOLTAGE ERROR`,
  `BIAS_1D VOLTAGE ERROR`
- `0x2A`–`0x2D` → `BIAS_1A SHOULD BE ZERO`, `BIAS_1B SHOULD BE ZERO`, `BIAS_1C SHOULD BE ZERO`,
  `BIAS_1D SHOULD BE ZERO`
- `0x2E` → `PAM1 GAIN TOO LOW`. `0x2F` → `PAM1 GAIN TOO HIGH`
- `0x30` → `PAM1 HV SHOULD BE ZERO`. `0x31` → `PAM1 CURRENT SHOULD BE ZERO`
- `0x32` → `PAM1 EXCESSIVE TEMP (3)`. `0x33` → `PAM1 TEMP TOO HIGH (3)`
- `0x34`–`0x37` → `BIAS_1A SHOULD BE ZERO (3)` … `BIAS_1D SHOULD BE ZERO (3)`
- `0x38` → `PSU1 EXCESSIVE TEMP` (PSU = power supply unit)
- `0x39` → `PAM1 EXCESSIVE CURRENT (CHECK SWR)`
- `0x40` → `PAM2 HV TOO LOW`. `0x41` → `PAM2 HV TOO HIGH`
- `0x42` → `PAM2 CURRENT NON-ZERO`. `0x43` → `PAM2 IDLE CURRENT TOO LOW`
- `0x44` → `PAM2 CURRENT WARNING`. `0x45` → `PAM2 EXCESSIVE CURRENT`
- `0x46`–`0x49` → `BIAS_2A VOLTAGE ERROR` … `BIAS_2D VOLTAGE ERROR`
- `0x4A`–`0x4D` → `BIAS_2A SHOULD BE ZERO` … `BIAS_2D SHOULD BE ZERO`
- `0x4E` → `PAM2 GAIN TOO LOW`. `0x4F` → `PAM2 GAIN TOO HIGH`
- `0x60` → `PSU1 CONTROL MALFUNCTION`. `0x61` → `PSU2 CONTROL MALFUNCTION`
- `0x62` → `PSU1 EXCESSIVE TEMP`. `0x63` → `PSU2 EXCESSIVE TEMP`
- `0x64` → `DISPLAY COMM ERROR`
- `0x65` → `ATU MODEM TEMP`
- `0x66` → `ATU POWER SWITCH ALARM`. `0x67` → `ATU POWER SWITCH ALARM (ON)`
- `0x68` → `ETHERNET NOT RESPONDING`
- `0x69` → `AUDIO MEMORY ERROR`
- `0x6C` → `LOSS OF AUDIO DATA`. `0x6D` → `LOSS OF ETHERNET DATA`
- `0x6E` → `LOSS OF EEPROM DATA (WARN)`. `0x6F` → `LOSS OF EEPROM DATA (SOFT)`
- `0x70` → `CAT ERROR`
- `0x80` → `ATU NOT RESPONDING / BIAS 1A ERR`
- `0x81` → `ATU-AMP COMM ERROR`. `0x82` → `AMP-ATU COMM ERROR`
- `0x83` → `ASEL NOT RESPONDING` (ASEL = antenna selector)
- `0x84` → `ASEL-AMP COMM ERROR`. `0x85` → `AMP-ASEL COMM ERROR`
- `0x86` → `NO TUNING SETTINGS`. `0x87` → `NO ANTENNA SETTINGS`
- `0x88` → `ATU CANNOT RETUNE (RF PRESENT)`. `0x89` → `ANTENNA CANNOT CHANGE (RF PRESENT)`
- `0x8A` → `ATU TUNING UNSUCCESSFUL`. `0x8B` → `ATU MEMORY FAIL`
- `0xA0` → `ATU DC VOLT TOO HIGH`. `0xA1` → `ATU DC VOLT TOO LOW`
- `0xA2` → `ATU 5V TOO LOW`. `0xA3` → `ATU 5V TOO HIGH`
- `0xA4` → `ANTENNA VOLT TOO HIGH (PWR)`. `0xA5` → `ANTENNA VOLT TOO HIGH (dmg)`
- `0xA6` → `ANTENNA CURRENT TOO HIGH (PWR)`. `0xA7` → `ANTENNA CURRENT TOO HIGH (dmg)`
- `0xA8` → `ANT REFL PWR TOO HIGH (SOFT)`. `0xA9` → `ANT REFL PWR TOO HIGH (HARD)`
- `0xAA` → `ATU INPUT PWR TOO HIGH`. `0xAB` → `ATU INPUT PWR TOO HIGH (dmg)`
- `0xAC` → `ANTENNA SWR TOO HIGH`. `0xAD` → `ANTENNA SWR TOO HIGH (dmg)`
- `0xAE` → `ATU TEMP TOO HIGH`. `0xAF` → `ATU TEMP TOO LOW`
- any other byte → `UNKNOWN ERROR (0xNN)` with uppercase hex

## [protocol] Outbound control frames, band-walk algorithm and write serialization

**Set mode** — the bridge sends these frames for the `set_mode` command and appends the checksum
byte automatically:

- `operate` → `55 81 08 02 00 06 00 <ck>`
- `standby` → `55 81 08 02 00 05 00 <ck>`

Layout: start `0x55`, type `0x81` (amplifier management), length `0x08`, sub-command `0x02` (mode
change), `0x00`, mode byte (`0x06` = OPR/RX, `0x05` = standby), `0x00`, checksum.

**Band-change step**: the ACOM protocol exposes only *relative* band changes (next/previous). So
`set_band` walks the amplifier from its current band index to the target index:

- one "next band" step → `55 81 08 09 00 80 00 <ck>`
- one "previous band" step → `55 81 08 09 00 40 00 <ck>`

Layout: start `0x55`, type `0x81`, length `0x08`, sub-command `0x09` (manual band/antenna change),
`0x00`, direction byte (`0x80` = next, `0x40` = previous), `0x00`, checksum.

**Walk algorithm (exact)**:

1. Resolve the requested target label to the amplifier's band index. Accepted labels: `160m`..`6m`
   or bare numbers `160`, `80`, `40`, `30`, `20`, `17`, `15`, `12`, `10`, `6`. The lookup is
   case-insensitive and whitespace-trimmed. Reject an unknown label: log it, and write nothing to
   the serial port.
2. The current band index comes from the last telemetry frame (0 = unknown). If the current band is
   unknown, reject the command with `current band unknown, cannot navigate`: log it, and write
   nothing. (Never walk blind from a garbage position.)
3. If target == current: no-op, return success.
4. Otherwise send |target − current| step frames, each in the correct direction. Sleep exactly
   **150 ms between steps**. A write error mid-walk must stop the walk (the amplifier can stay
   between bands — see the defect below).

**Write serialization**: the bridge must serialize all serial writes (ACKs, enable, mode frames,
band steps) so that no two frames ever interleave on the wire. A band walk is an atomic sequence
of steps. (The implementation uses a mutex. Only the no-interleaving property is normative.)

## [protocol] Forward-power averaging

Forward power is noisy frame-to-frame, so it passes through a time-windowed moving average before
the bridge publishes it. Each decoded frame appends a sample. The bridge drops samples older than
the configured window (`serial.avg_time_ms`). The published value is the arithmetic mean
(integer division, truncated) of the remaining samples. The code clamps the window to ≥ 1 ms.
A window of 1 ms effectively publishes the raw per-frame value. Reflected power, SWR, and
temperature are **not** averaged.

Note: the deployed configuration carries 300 (deploy seed). The in-binary default is 1 — see the
open decisions below.

## [requirement] Publish-cadence contract — dedup rules the live api doc lags

`docs/pa-mqtt-api.md` still says `/state` is "published **on every telemetry frame**". The code,
and the PRD, throttle (code-check 2026-09-03: dedup confirmed live in
`internal/bridge/bridge.go` `publishState`; the api doc lag is still present):

- Publish **every** telemetry frame while `keyed == "tx"`.
- Publish **every** telemetry frame in an error condition: any `fault != none`, non-empty `error`,
  or `device_online == false`.
- Publish when any of these fields change against the last published snapshot: `mode`, `band`,
  `keyed`, `fault`, `pa_state`, `power`, `device_online`, `error`.
- Otherwise (idle rx/inhibited, nothing meaningful changed): publish only as a heartbeat, at most
  once every **60 s**, so the retained document's timestamp stays fresh.
- Consequence: numeric-only changes (`temp_c`, `swr`, and the zeroed power meters) do **not**
  trigger a publish in idle states. Idle temperature drift reaches the bus at the 60 s heartbeat
  cadence at most.
- On any `device_online` transition the dedup state resets and forces the next frame to publish.

Cadence summary:

| Event | Publishes |
|---|---|
| MQTT (re)connect | `/status` = `online`. Re-subscribe `/cmd` |
| Serial port opened (each restart cycle) | `/meta` (full, incl. `expose`). `/state` snapshot with `device_online:true` |
| Each telemetry frame | `/state` **if** dedup allows |
| Serial port lost | `/state` with `device_online:false`, `power:"off"`, `error:"serial: <cause>"`. `/status` stays `online` |
| Bridge crash / unclean death | `/status` = `offline` (broker LWT) |
| Idle, nothing changing | `/state` heartbeat every ≤ 60 s |

There is no periodic `/status` republish and no other heartbeat. The precise telemetry frame rate
is not documented anywhere; only "high even in OPR/RX" (the motivation for the dedup) — treat it
as unspecified and design dedup accordingly.

## [requirement] Power-meter zeroing rule

The amplifier can keep reporting the last transmit value after unkeying (and the averager holds
stale samples). So the bridge hard-zeros `fwd_power_w` and `rfl_power_w` in every snapshot where
`keyed != "tx"` (that is, in `rx` and `inhibited`). Displays must never show stale transmit power
while the operator receives. SWR is **not** zeroed. (Not stated in `docs/pa-mqtt-api.md`.)

## [requirement] Exit codes and shutdown-interrupts-every-wait

Exit code conventions [NORMATIVE]:

- **0** when the run ended through a shutdown signal (a clean service stop is not a failure. This
  is what prevents `systemctl stop` from triggering a restart),
- **1** for other run errors (for example, an MQTT connect failure not caused by shutdown),
- **2** for config load/validation errors.

A shutdown signal (SIGTERM/SIGINT) must interrupt **every** blocking wait in the process:

- If the process sits in the first MQTT connect (for example, during a broker outage), the connect
  wait must stop. The process must exit **within 5 s of the shutdown signal**, even when the
  broker is unreachable. That is far below the systemd default 90 s stop timeout, which the process
  must not depend on. A test must check this bound (send the shutdown signal while the broker is
  unreachable. Assert process exit within 5 s).
- If the process runs the serial read loop: closing the serial port on the shutdown signal must
  unblock the 1 s read. The loop then returns cancelled.
- If the process sleeps in a backoff: the sleep must return immediately.
- The bridge does not interrupt an in-flight band walk mid-walk (worst case ≈ 9 steps × 150 ms ≈
  1.35 s remains). The loop then notices the shutdown signal.
- The MQTT client disconnects cleanly. Because the disconnect is clean, **the broker does not fire
  the LWT** — retained `/status` keeps its last value `online` after a clean stop (station-wide
  known behavior; consumers must use the two-layer liveness rule).

**History [must not regress]**: in the reference MQTT library (paho), the connect wait ignored the
caller's cancellation. This exact bridge hung live on SIGTERM during a broker outage until systemd
force-killed it after the stop timeout. The fix routes the wait through a watcher that races the
shutdown signal. Any re-implementation test must cover "shutdown signal while broker unreachable
exits within 5 s".

## [defect] Band walk can leave the amplifier mid-walk, and nothing blocks a walk while transmitting

A serial write error mid-walk stops the walk with the amplifier between bands. There is no
reconciliation or retry (the next telemetry frame reports the actual band). Also nothing prevents
a band walk while transmitting (the amplifier itself can refuse with fault `STOP TRANSMISSION
FIRST`, so the position stays uncertain).

Code-check 2026-09-03: **still open** — `internal/acom/device.go` `SetBand` returns
`fmt.Errorf("band step %d/%d: %w", …)` on write error and has no retry/reconciliation; no TX check
before the walk.

## [defect] Degenerate first snapshot after serial open

After a serial open but before the first telemetry frame, the bridge publishes a snapshot with
`device_online:true`. Its canonical fields carry the previous cycle's values. On the very first
cycle they carry empty strings (`mode`, `keyed`, `fault`, `pa_state` all `""`, `power` `""`).
Momentarily odd JSON for consumers that assume non-empty enums.

Code-check 2026-09-03: **still open** — `cmd/…/main.go` calls `b.SetDeviceOnline(true, "")` right
after port open, before the first telemetry frame; `internal/bridge/bridge.go` `SetDeviceOnline`
publishes the current state snapshot as-is.

## [defect] Publish errors invisible; no command feedback on the bus

- The code publishes fire-and-forget into the client's queue. QoS 1 delivery errors surface
  nowhere in logs (the client can hold messages queued during a brief outage, not lost, but the
  errors stay invisible). Code-check 2026-09-03: **still open** — `internal/bridge/bridge.go`
  discards Publish errors (`_ = b.pub.Publish(...)`).
- Commands are fire-and-forget. There is no reply topic and no error topic. You can see success or
  failure only through subsequent `/state` telemetry (mode/band change) and process logs. The
  bridge logs command-execution errors at warn level and drops them. Code-check: **still the
  design**.

## [defect] Stale `set_power` mentions in code comments

A source comment (and the `main.go` package doc) still list `set_power` among dispatched commands
— the bridge rejects it as unknown. Code-check 2026-09-03: **still open** —
`cmd/acom1200s-pa-bridge/main.go` line 4 package doc reads
"dispatches /cmd intent (set_mode, set_band, set_power)". The dispatch comment in
`internal/bridge/bridge.go` is correct ("set_power is intentionally not handled").

## [decision] Which `avg_time_ms` is actually running on shari?

The deployment script seeds `serial.avg_time_ms = 300` on first install. The binary default and
the example config say `1` (raw per-frame). The seeded config file is device-owned. The live value
on shari was unknown when the PRD was written. Evidence for 300: the `deploy.sh` seed block.
Evidence for 1: binary default (`internal/config/config.go`), `config.example.toml`. A
re-implementation must decide whether the normative default is 1 or 300, and must confirm the
live value on the device (`ssh io@192.168.1.139 cat /etc/acom1200s-pa-bridge/config.toml`).
Code-check 2026-09-03: mismatch **still present in the repo** (binary 1, `deploy.sh` seed 300).

## [decision] Vendor protocol open questions (need the ACOM PDF, which is not in the repo)

All four hinge on the vendor document **"ACOM A600S A1200S Serial Protocol V1.3.pdf"**, which must
be obtained from ACOM (its repo path claimed by `acom1200s-pa-bridge/CLAUDE.md` does not exist —
see the transport section above):

- **Does the amplifier ACK outbound command frames (type `0x81`)?** The bridge ACKs every inbound
  frame but ignores all non-telemetry inbound frames. So you can infer a command's success only
  from subsequent telemetry. Whether the vendor protocol defines a command ACK — one that a
  re-implementation can use for real command feedback — is unknown. Command ACK semantics are
  unobservable in the current system; a re-implementation can add them, but that is new behavior.
- **Are there other inbound frame types?** The bridge consumes only `0x2F` (telemetry). Whether the
  vendor protocol defines more inbound frame types is unknown.
- **Meaning of the enable-frame payload byte `0x15`** (`55 92 04 15`) — undocumented. The bytes
  are cargo-culted from the vendor protocol document, and the bridge must send them exactly
  as-is.
- **Precise telemetry frame rate** — not documented anywhere. Only one fact exists: the rate is
  "high even in OPR/RX" (a code comment that motivates the dedup). A re-implementation must treat
  the frame rate as unspecified and design dedup accordingly.

## [decision] Station-wide decisions this component inherits

- **Clean stop and `/status: offline` — publish it explicitly or not?** Current behavior leaves
  retained `online` after a clean stop (the LWT fires only on unclean death; a stopped bridge
  still looks "online" to `/status`-only consumers). The station-wide rule (consumers must check
  both `/status` and `/state.device_online`) makes the stale value survivable. A deliberate
  offline publish on clean shutdown gives a cleaner contract — a station-wide decision
  (`docs/station-integration-model.md`), not one for this component alone. Code-check 2026-09-03:
  **still open** — no explicit offline publish on shutdown anywhere in
  `acom1200s-pa-bridge/cmd/…/main.go`.
- **`device_online` form**: the deployed bridge publishes `device_online:true` explicitly. The
  integration model says "omitted when true". Consumers must treat both forms as the same
  (absence = true), or the contract must mandate explicit-true — a station-wide open decision
  (tracked in the M0 open-decision list), not resolvable from this component alone.

## [unique] Design lineage: power is observed, never commanded

The bridge is a pure observer of the amplifier's power state. An earlier RTS-wake-line `set_power`
mechanism never worked reliably, so the project removed it — along with the amp-power-cycle
"watchdog" that re-armed telemetry after a power cycle. What remains inside a live session is the
5 s silence re-send; the other guard is the 30 s silence reconnect. A re-implementation must not
reintroduce `set_power`, `power_default`, or RTS-line manipulation; the `muehle/hf/switch` slot
owns PA mains power, and `power` in `/state` is read-only telemetry (`off` only when
`pa_state == OFF` or the port is lost).