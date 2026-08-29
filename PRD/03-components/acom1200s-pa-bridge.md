# 03.x — acom1200s-pa-bridge: ACOM 1200S power-amplifier bridge

This document specifies `acom1200s-pa-bridge`, the component that connects the station's ACOM 1200S
power amplifier to the MQTT bus. A **power amplifier (PA)** is a device that boosts a radio
transmitter's output signal to high transmit power (the ACOM 1200S delivers up to 1200 W); a
**bridge** is a small daemon translating between a physical device's proprietary protocol and the
station's MQTT message bus; **MQTT** is lightweight publish/subscribe messaging over TCP where
messages are addressed to hierarchical string *topics* and a *retained* message is one the broker
stores and re-delivers to every future subscriber. The bridge occupies the slot
`muehle/hf/pa` — a **slot** being the station's addressing unit, a topic prefix
`<site>/<station>/<slot>` with four topic planes `meta`, `state`, `status`, `cmd` (see
`02-interface-spec.md` §2–§4). It publishes a live JSON snapshot of the amplifier's state, accepts two
commands (operating mode and band selection), and reports liveness at two distinct layers (its own
process, and the serial link to the amplifier). It is deliberately **not** in charge of turning the
amplifier's mains power on or off — that belongs to a different slot (`muehle/hf/switch`, the
remote-on relays).

Terminology used throughout this document:

- **Amateur radio (ham radio)**: the licensed hobby of two-way radio communication. This station
  ("Mühle", site id `muehle`) is an amateur-radio installation; **HF** means shortwave frequencies
  (roughly 3–30 MHz), where the amplifier operates.
- **Transceiver / radio**: the device that generates and receives the radio signal; the amplifier
  boosts what the radio produces on transmit.
- **RF (radio frequency)**: the actual radio signal itself — the high-frequency alternating
  current/field that carries the communication, as opposed to the low-voltage control and data
  signals (serial, MQTT, key lines) around it. The amplifier boosts RF on transmit and senses RF
  to determine its band; "RF routing" means the path the RF signal takes through relays, and "hot
  switching while transmitting" means re-routing it while RF power is present.
- **Drive (drive power)**: the radio's low-power RF input signal fed into the amplifier, which the
  amplifier boosts to full output power (e.g. ~50 W of drive in → up to 1200 W out).
- **SWR (standing wave ratio)**: a dimensionless ratio ≥ 1 measuring how well the antenna system is
  matched; a high SWR means power is reflected back into the amplifier, which can damage it.
- **Keyed / keying**: the act of putting the amplifier into transmit. This amplifier is keyed by a
  **hardware key line** from the radio — never over MQTT.
- **ATU (automatic tuner)**: an antenna-matching unit that the amplifier can work with; the
  amplifier's fault codes include ATU errors.
- **ALC (automatic level control)**: a feedback signal from the amplifier back to the radio that
  limits drive power. The ACOM 1200S has an ALC output.
- **CAT**: "computer-aided transceiver" — a serial link by which a radio reports its frequency/band.
  This bridge carries **no** CAT cable; the amplifier senses its band from the RF drive signal
  itself (`band_source: rf_sense`).
- **LWT (Last Will and Testament)**: a message the MQTT broker publishes on the client's behalf if
  the client disappears without closing the connection (crash, network loss). It is **not**
  published on a clean disconnect.
- **QoS**: MQTT delivery guarantee — QoS 0 = fire-and-forget, QoS 1 = at-least-once.
- **Band**: amateur-radio allocations in the HF range, written canonically here as `160m`, `80m`,
  `40m`, `30m`, `20m`, `17m`, `15m`, `12m`, `10m`, `6m` (the ACOM 1200S has no 60 m band).
- **OPR/RX, OPR/TX**: the amplifier firmware's "operating, receiving" and "operating, transmitting"
  states (raw firmware labels; see §4.2).

---

## 1. Role and responsibilities

The bridge is a Linux daemon (a systemd service) running on the station's Raspberry Pi host
("shari", 192.168.1.139), which connects to the ACOM 1200S over RS-232 (via a USB-serial adapter)
and to the MQTT broker at `192.168.1.50:1883` (user `hf`). Note: a planned migration of the broker
onto shari itself (192.168.1.139) exists on an unmerged feature branch and is NOT deployed —
`192.168.1.50` is current production; see `05-deployment-ops.md` for the decision point.

The bridge SHALL:

1. **Telemetry** — continuously read the amplifier's proprietary binary serial protocol and publish
   a canonical, human-readable JSON snapshot of the amplifier state to the retained topic
   `muehle/hf/pa/state`.
2. **Control** — accept exactly two commands over MQTT, `set_mode` (operate/standby) and
   `set_band`, and translate them into serial frames. **Power is never commanded** (§6.1).
3. **Liveness** — announce its own process liveness via MQTT LWT on `muehle/hf/pa/status`, and
   separately report *device* (serial-link) liveness inside the state snapshot via the
   `device_online` field.

The bridge is a pure observer of the amplifier's power state. An earlier RTS-wake-line `set_power`
mechanism never worked reliably and has been removed; the re-implementation MUST NOT reintroduce it.

---

## 2. Serial interface — ACOM 600S/1200S protocol [NORMATIVE]

### 2.1 Transport

- The amplifier's RS-232 service port is reached through a USB-serial adapter (Prolific chip, USB
  vendor id `067b`), opened by device path. Default path:
  `/dev/serial/by-id/usb-Prolific_Technology_Inc._USB-Serial_Controller_D-if00-port0`
  (a stable-by-id symlink that survives adapter replugs). A udev rule pins this adapter's tty to
  group `dialout` (see §7).
- Serial parameters are protocol constants, **not configurable**: **9600 baud, 8 data bits, no
  parity, 1 stop bit (9600 8N1)**.
- Read timeout on the port: **1 second** (a read returns 0 bytes after 1 s of silence).
- On open, the bridge SHALL reset the serial input/output buffers and then send the
  enable-telemetry command (§2.4).

**Authoritative vendor reference**: the protocol described in this section is documented by the
vendor in the PDF **"ACOM A600S A1200S Serial Protocol V1.3.pdf"** — the file the shipped project's
own engineering notes cite as the source for the frame layout and byte offsets. It is **not present
in the repo**, so the byte-level tables below are what the reference implementation actually puts on
the wire; a re-implementer who wants to resolve the open protocol questions (§10: command ACK
semantics, other frame types, the meaning of the `0x15` payload byte) must obtain that document
from ACOM.

### 2.2 Frame format

All frames, both directions, share this layout:

| Offset | Meaning |
|---|---|
| byte 0 | start byte, always `0x55` |
| byte 1 | message address / message type |
| byte 2 | total frame length **including** this length byte and the trailing checksum byte |
| bytes 3..n−2 | payload |
| last byte | checksum |

**Checksum rule**: the checksum byte is chosen so that the **sum of all bytes in the frame, mod 256,
is zero**. A received frame is valid only if its declared length ≥ 4 and the byte-sum is 0 (mod 256).
The sender computes the checksum as `0 − sum(all other bytes)` (mod 256) and appends it. The
smallest valid frame is 4 bytes.

**Exception [critical quirk]**: the enable-telemetry frame (§2.4) is sent as a fixed 4-byte sequence
with **no checksum byte appended**. Any re-implementation that blindly appends checksums to every
frame would corrupt the enable command.

### 2.3 Receive path: resynchronization and ACK

On the incoming byte stream the bridge SHALL resynchronize byte-exactly as follows:

1. Scan forward one byte at a time until a `0x55` start byte is found.
2. Read the declared length (byte 2). If the length is < 4, treat as noise: discard **one byte** and
   rescan.
3. If fewer bytes than the declared length are buffered, wait for more data (keep the remainder).
4. When a full frame is available, verify the checksum (byte-sum == 0 mod 256).
   - Checksum **fails** → discard one byte and rescan (never discard the whole buffer).
   - Checksum **passes** → handle the frame and consume exactly its declared length.

For every valid frame received, regardless of type, the bridge SHALL send an **ACK frame**:
`{0x55, 0x86, 0x05, <byte-1-of-received-frame>, <checksum>}` — start `0x55`, type `0x86`, length
`0x05`, the received frame's address byte echoed, then the zeroing checksum byte.

Frames with address `0x2F` are **telemetry frames**; all other frame types SHALL be ACKed and
otherwise ignored. (Whether the amplifier sends ACKs back for outbound commands is unknown — see
§10.)

### 2.4 Enable-telemetry command

Sent on port open, and re-sent during silence (§2.6). Exact bytes on the wire:

```
55 92 04 15
```

(start `0x55`, type `0x92` = "enable automatic telemetry", length `0x04`, payload byte `0x15`; the
meaning of `0x15` is undocumented — see §10). **No checksum byte is appended.** Once the amplifier
receives this, it streams 72-byte telemetry frames automatically.

### 2.5 Telemetry frame decode (72 bytes, address `0x2F`)

A telemetry frame SHALL be decoded **only if its total length is exactly 72 bytes**; other lengths
are silently dropped. All multi-byte values are **little-endian**.

| Byte offsets | Type | Meaning |
|---|---|---|
| `[3]` | byte | firmware mode; decode by **high nibble** (`b & 0xF0`) — table below |
| `[16:18]` | uint16 LE | heatsink temperature in **Kelvin**; published value = K − 273.15; raw 0 decodes to 0.0 °C (special case, no −273.15) |
| `[20:22]` | uint16 LE | input (drive) power in **deciwatts** → /10.0 W — parsed but **never published** |
| `[22:24]` | uint16 LE | forward power in W (raw, before averaging per §2.8) |
| `[24:26]` | uint16 LE | reflected power in W |
| `[26:28]` | uint16 LE | SWR raw → /100.0 (e.g. raw 120 → 1.2) |
| `[48:50]` | uint16 LE | frequency in **kHz** — parsed but **never published** (the PA slot does not own bus frequency; the radio slot owns `freq_hz` in Hz) |
| `[66]` | byte | fault byte; `0xFF` = no fault |
| `[69]` | byte | band; decode by **low nibble** (`b & 0x0F`), 1..10 per table below; anything else (0, 11–15) → band label `UNK`, band index 0 (unknown) |

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
| 1 | `160m` | 1 |
| 2 | `80m` | 2 |
| 3 | `40m` | 3 |
| 4 | `30m` | 4 |
| 5 | `20m` | 5 |
| 6 | `17m` | 6 |
| 7 | `15m` | 7 |
| 8 | `12m` | 8 |
| 9 | `10m` | 9 |
| 10 (0x0A) | `6m` | 10 |
| 0, 11–15 | `UNK` | 0 (unknown) |

These ten bands are the amplifier's complete set and are exactly what `set_band` accepts.

**Fault byte → verbatim message.** When the fault byte differs from `0xFF`, the exact string below
is published in the state `error` field:

- `0xFF` → `NONE` (no fault; `error` published empty)
- `0x00` → `HOT SWITCHING ATTEMPT` (attempting to switch RF routing while transmitting)
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
- `0x10` → `5V TOO LOW`; `0x11` → `5V TOO HIGH`
- `0x12` → `26V TOO LOW`; `0x13` → `26V TOO HIGH`
- `0x14` → `ERROR 0x14`
- `0x15` → `PAM1 FAN SPEED TOO LOW`; `0x16` → `PAM2 FAN SPEED TOO LOW`; `0x17` → `LPF FAN SPEED TOO LOW`
  (PAM1/PAM2 = the two power-amplifier modules; LPF = low-pass filter)
- `0x18` → `PAM1 DISSIPATION TOO HIGH`; `0x19` → `PAM2 DISSIPATION TOO HIGH`
- `0x1A` → `PAM1 DISSIPATION WARNING`; `0x1B` → `PAM2 DISSIPATION WARNING`
- `0x1C` → `PAM1 TEMP TOO HIGH`; `0x1D` → `PAM2 TEMP TOO HIGH`
- `0x1E` → `PAM1 EXCESSIVE TEMP`; `0x1F` → `PAM2 EXCESSIVE TEMP`
- `0x20` → `PAM1 HV TOO LOW`; `0x21` → `PAM1 HV TOO HIGH` (HV = high-voltage supply)
- `0x22` → `PAM1 CURRENT NON-ZERO`; `0x23` → `PAM1 IDLE CURRENT TOO LOW`
- `0x24` → `PAM1 CURRENT WARNING`; `0x25` → `PAM1 EXCESSIVE CURRENT`
- `0x26`–`0x29` → `BIAS_1A VOLTAGE ERROR`, `BIAS_1B VOLTAGE ERROR`, `BIAS_1C VOLTAGE ERROR`,
  `BIAS_1D VOLTAGE ERROR`
- `0x2A`–`0x2D` → `BIAS_1A SHOULD BE ZERO`, `BIAS_1B SHOULD BE ZERO`, `BIAS_1C SHOULD BE ZERO`,
  `BIAS_1D SHOULD BE ZERO`
- `0x2E` → `PAM1 GAIN TOO LOW`; `0x2F` → `PAM1 GAIN TOO HIGH`
- `0x30` → `PAM1 HV SHOULD BE ZERO`; `0x31` → `PAM1 CURRENT SHOULD BE ZERO`
- `0x32` → `PAM1 EXCESSIVE TEMP (3)`; `0x33` → `PAM1 TEMP TOO HIGH (3)`
- `0x34`–`0x37` → `BIAS_1A SHOULD BE ZERO (3)` … `BIAS_1D SHOULD BE ZERO (3)`
- `0x38` → `PSU1 EXCESSIVE TEMP` (PSU = power supply unit)
- `0x39` → `PAM1 EXCESSIVE CURRENT (CHECK SWR)`
- `0x40` → `PAM2 HV TOO LOW`; `0x41` → `PAM2 HV TOO HIGH`
- `0x42` → `PAM2 CURRENT NON-ZERO`; `0x43` → `PAM2 IDLE CURRENT TOO LOW`
- `0x44` → `PAM2 CURRENT WARNING`; `0x45` → `PAM2 EXCESSIVE CURRENT`
- `0x46`–`0x49` → `BIAS_2A VOLTAGE ERROR` … `BIAS_2D VOLTAGE ERROR`
- `0x4A`–`0x4D` → `BIAS_2A SHOULD BE ZERO` … `BIAS_2D SHOULD BE ZERO`
- `0x4E` → `PAM2 GAIN TOO LOW`; `0x4F` → `PAM2 GAIN TOO HIGH`
- `0x60` → `PSU1 CONTROL MALFUNCTION`; `0x61` → `PSU2 CONTROL MALFUNCTION`
- `0x62` → `PSU1 EXCESSIVE TEMP`; `0x63` → `PSU2 EXCESSIVE TEMP`
- `0x64` → `DISPLAY COMM ERROR`
- `0x65` → `ATU MODEM TEMP`
- `0x66` → `ATU POWER SWITCH ALARM`; `0x67` → `ATU POWER SWITCH ALARM (ON)`
- `0x68` → `ETHERNET NOT RESPONDING`
- `0x69` → `AUDIO MEMORY ERROR`
- `0x6C` → `LOSS OF AUDIO DATA`; `0x6D` → `LOSS OF ETHERNET DATA`
- `0x6E` → `LOSS OF EEPROM DATA (WARN)`; `0x6F` → `LOSS OF EEPROM DATA (SOFT)`
- `0x70` → `CAT ERROR`
- `0x80` → `ATU NOT RESPONDING / BIAS 1A ERR`
- `0x81` → `ATU-AMP COMM ERROR`; `0x82` → `AMP-ATU COMM ERROR`
- `0x83` → `ASEL NOT RESPONDING` (ASEL = antenna selector)
- `0x84` → `ASEL-AMP COMM ERROR`; `0x85` → `AMP-ASEL COMM ERROR`
- `0x86` → `NO TUNING SETTINGS`; `0x87` → `NO ANTENNA SETTINGS`
- `0x88` → `ATU CANNOT RETUNE (RF PRESENT)`; `0x89` → `ANTENNA CANNOT CHANGE (RF PRESENT)`
- `0x8A` → `ATU TUNING UNSUCCESSFUL`; `0x8B` → `ATU MEMORY FAIL`
- `0xA0` → `ATU DC VOLT TOO HIGH`; `0xA1` → `ATU DC VOLT TOO LOW`
- `0xA2` → `ATU 5V TOO LOW`; `0xA3` → `ATU 5V TOO HIGH`
- `0xA4` → `ANTENNA VOLT TOO HIGH (PWR)`; `0xA5` → `ANTENNA VOLT TOO HIGH (dmg)`
- `0xA6` → `ANTENNA CURRENT TOO HIGH (PWR)`; `0xA7` → `ANTENNA CURRENT TOO HIGH (dmg)`
- `0xA8` → `ANT REFL PWR TOO HIGH (SOFT)`; `0xA9` → `ANT REFL PWR TOO HIGH (HARD)`
- `0xAA` → `ATU INPUT PWR TOO HIGH`; `0xAB` → `ATU INPUT PWR TOO HIGH (dmg)`
- `0xAC` → `ANTENNA SWR TOO HIGH`; `0xAD` → `ANTENNA SWR TOO HIGH (dmg)`
- `0xAE` → `ATU TEMP TOO HIGH`; `0xAF` → `ATU TEMP TOO LOW`
- any other byte → `UNKNOWN ERROR (0xNN)` with uppercase hex

### 2.6 Silence handling and connection-loss detection

Within one open-port session the bridge SHALL behave as follows:

- Any byte received resets the silence timer.
- On each 1 s read timeout with 0 bytes:
  - If silence has lasted **more than 30 s**: the session ends with error text
    `no data received for 30s, restarting monitor` → device marked offline, port closed, restart
    with backoff (§5.2).
  - Else, if silence has lasted **more than 5 s** and the last retry was more than 5 s ago:
    re-send the enable-telemetry command (`55 92 04 15`) and log
    `no data for 5s, re-sending enable telemetry`. This repeats at most once per 5 s until data
    resumes or the 30 s limit trips.
- A read error (port gone, USB replug) ends the session immediately; a shutdown signal also ends the
  session (by closing the port, unblocking the read).

Practical effect: when the amplifier is powered off by the `hf/switch` relays, the port goes silent
and the session exits after 30 s, publishing `device_online:false`. When power returns, the next
restart cycle re-opens the port and re-sends enable-telemetry, so telemetry resumes without operator
action.

### 2.7 Outbound control frames

**Set mode** (sent for the `set_mode` command; checksum byte appended automatically):

- `operate` → `55 81 08 02 00 06 00 <ck>`
- `standby` → `55 81 08 02 00 05 00 <ck>`

Layout: start `0x55`, type `0x81` (amplifier management), length `0x08`, sub-command `0x02` (mode
change), `0x00`, mode byte (`0x06` = OPR/RX, `0x05` = standby), `0x00`, checksum.

**Band-change step**: the ACOM protocol exposes only *relative* band changes (next/previous), so
`set_band` walks the amplifier from its current band index to the target index:

- one "next band" step → `55 81 08 09 00 80 00 <ck>`
- one "previous band" step → `55 81 08 09 00 40 00 <ck>`

Layout: start `0x55`, type `0x81`, length `0x08`, sub-command `0x09` (manual band/antenna change),
`0x00`, direction byte (`0x80` = next, `0x40` = previous), `0x00`, checksum.

**Walk algorithm (exact)**:

1. Resolve the requested target label to the amplifier's band index. Accepted labels: `160m`..`6m`
   or bare numbers `160`, `80`, `40`, `30`, `20`, `17`, `15`, `12`, `10`, `6`; the lookup is
   case-insensitive and whitespace-trimmed. An unknown label SHALL be rejected: log, no serial
   writes.
2. The current band index comes from the last telemetry frame (0 = unknown). If the current band is
   unknown the command SHALL be rejected with `current band unknown, cannot navigate`: log, no
   writes. (Never walk blind from a garbage position.)
3. If target == current: no-op, return success.
4. Otherwise send |target − current| step frames, each in the correct direction, sleeping exactly
   **150 ms between steps**. A write error mid-walk SHALL abort the walk (the amplifier may be left
   between bands — see §8, defect 5).

**Write serialization**: all serial writes (ACKs, enable, mode frames, band steps) SHALL be
serialized so that no two frames ever interleave on the wire, and a band walk is an atomic sequence
of steps. (The reference implementation uses a mutex; only the no-interleaving property is
normative.)

### 2.8 Forward-power averaging

Forward power is noisy frame-to-frame, so it SHALL pass through a time-windowed moving average
before publishing: each decoded frame appends a sample; samples older than the configured window
(`serial.avg_time_ms`, §6) are dropped; the published value is the arithmetic mean (integer
division, truncated) of the remaining samples. The window is clamped to ≥ 1 ms; a window of 1 ms
effectively publishes the raw per-frame value. Reflected power, SWR, and temperature are **not**
averaged.

Note: the deployed configuration seeds 300 ms (§7) while the in-binary default is 1 ms — see §6 and
§10 for this open question.

---

## 3. MQTT presence

Broker: `tcp://192.168.1.50:1883`, user `hf`, password supplied via an environment file, never in
the config (§6). MQTT client id defaults to `<site>-<station>-<slot>` = `muehle-hf-pa` (config may
override). If the broker is unreachable at startup, the bridge SHALL retry the initial connect
every **5 s** forever rather than exit, and a shutdown signal SHALL abort that connect wait within
5 s (§5.3).

### 3.1 Topics — exact strings [NORMATIVE]

| Topic | Direction | Retained | QoS | Payload |
|---|---|---|---|---|
| `muehle/hf/pa/meta` | publish | yes | 1 | JSON birth certificate (§3.3) |
| `muehle/hf/pa/state` | publish | yes | 1 | JSON live snapshot (§3.2) |
| `muehle/hf/pa/status` | publish (incl. LWT) | yes | 1 | plain string `online` / `offline` |
| `muehle/hf/pa/cmd` | **subscribe** | no | 1 | JSON command (§4) |

Every retained publish goes out at QoS 1; a hypothetical non-retained publish would be QoS 0 (all
`/state` and `/meta` publishes in this bridge are retained, hence QoS 1).

**LWT contract**: the client registers a last will of `offline` (QoS 1, retained) on
`muehle/hf/pa/status`, and on every (re)connect publishes `online` (QoS 1, retained) to the same
topic. `/status` reflects the **bridge process only**, never the amplifier. The `/cmd`
subscription is re-established on every MQTT reconnect; `/cmd` is not retained, so commands
published while the bridge is disconnected are lost.

**Two-layer liveness (station-wide rule, see `02-interface-spec.md` §5 and `06-safety.md`)**:
`/status` (process, via LWT) and `/state.device_online` (serial link to the amplifier) are
independent and consumers must check both. Additionally — actual observed behavior, not idealized —
on a **clean** process shutdown the broker does **not** fire the LWT, so the retained `/status`
keeps its last value `online` after a cleanly stopped service. Consumers must not trust `/status`
alone.

### 3.2 `/state` payload [NORMATIVE]

Single JSON document, published retained:

```json
{
  "ts":            "2026-07-10T18:30:00Z",
  "mode":          "operate",
  "band":          "20m",
  "keyed":         "tx",
  "fwd_power_w":   600,
  "rfl_power_w":   3,
  "temp_c":        42.1,
  "swr":           1.2,
  "fault":         "none",
  "pa_state":      "OPR/TX",
  "power":         "on",
  "device_online": true,
  "error":         ""
}
```

Field reference (field names and types exact):

| Field | JSON type | Semantics |
|---|---|---|
| `ts` | string | RFC 3339, UTC, of this publish |
| `mode` | string | canonical: `operate` \| `standby`. `bypass` is never produced (the ACOM protocol has no bypass state). Mapping: `OPR/RX` and `OPR/TX` → `operate`; every other firmware mode (STANDBY, OFF, ATAC, RESET, INIT, DEBUG, SERVICE, MENU, UNKNOWN) → `standby` |
| `band` | string | canonical label (`160m`..`6m`, or `UNK`); **omitted** when empty string |
| `keyed` | string | canonical: `OPR/TX` → `tx`; `OPR/RX` → `rx`; everything else → `inhibited` (the amplifier will not key) |
| `fwd_power_w` | number (uint16) | forward power in W, window-averaged (§2.8); **forced to 0 whenever keyed != `tx`** (see below) |
| `rfl_power_w` | number (uint16) | reflected power in W; **forced to 0 whenever keyed != `tx`** |
| `temp_c` | number | heatsink temperature in °C, rounded to **0.1 °C** |
| `swr` | number | ratio, raw/100; **not** zeroed when not keyed |
| `fault` | string | canonical: `none` \| `swr` \| `temp` \| `reflected` \| `other` — bucket mapping in §3.2.1 |
| `pa_state` | string | raw firmware mode string, diagnostic only (`OPR/RX`, `OPR/TX`, `STANDBY`, `OFF`, …). Raw firmware strings appear **only** here, never in `mode`/`keyed`/`fault` |
| `power` | string | `on` \| `off`, **read-only telemetry**: `off` only when `pa_state == OFF` (firmware fully powered down) or the serial port is lost; every other state, including STANDBY (powered but not transmitting), is `on`. Never commanded — see §6.1 |
| `device_online` | bool | `true` while the serial session has data; `false` when the port is lost (30 s silence or read error) |
| `error` | string | verbatim fault message (§2.5) when the fault byte != `0xFF`; otherwise empty/omitted. Also carries serial-session error text (e.g. `serial: open serial: ...`) when the port is lost |

**Power-meter zeroing rule**: the amplifier can keep reporting the last transmit value after
unkeying (and the averager holds stale samples), so `fwd_power_w` and `rfl_power_w` SHALL be
hard-zeroed in every snapshot where `keyed != "tx"` (i.e. in `rx` and `inhibited`). Displays must
never show stale transmit power while receiving.

**Publish deduplication rule** (the shipped README lags here: it says "published on every telemetry
frame"; the code — and this spec — throttles):

- Publish **every** telemetry frame while `keyed == "tx"`.
- Publish **every** telemetry frame in an error condition: any `fault != none`, non-empty `error`,
  or `device_online == false`.
- Publish when any of these fields change vs. the last published snapshot: `mode`, `band`, `keyed`,
  `fault`, `pa_state`, `power`, `device_online`, `error`.
- Otherwise (idle rx/inhibited, nothing meaningful changed): publish only as a heartbeat, at most
  once every **60 s**, so the retained document's timestamp stays fresh.
- Consequence: numeric-only changes (`temp_c`, `swr`, and the zeroed power meters) do **not**
  trigger a publish in idle states — idle temperature drift reaches the bus at the 60 s heartbeat
  cadence at most.
- On any `device_online` transition the dedup state resets, forcing the next frame to publish.

### 3.2.1 Fault bucket mapping

`0xFF` → `none`. Otherwise the fault byte is bucketed into the canonical `fault` enum:

- `temp`: `0x18`, `0x19`, `0x1A`, `0x1B`, `0x1C`, `0x1D`, `0x1E`, `0x1F`, `0x32`, `0x33`, `0x38`,
  `0x62`, `0x63`, `0x65`, `0xAE`, `0xAF`
- `swr`: `0x0D`, `0x39`, `0xAC`, `0xAD`
- `reflected`: `0x04`, `0x05`, `0xA8`, `0xA9`
- every other byte → `other` (relays, drive power, HV, bias, fans, PSU, comms, CAT, EEPROM,
  ATU DC/antenna faults). The verbatim message always survives in `error`.

### 3.3 `/meta` payload [NORMATIVE]

Retained JSON, published **once per serial-open cycle** — i.e. republished after every serial
reconnect/restart, and only after the serial port has opened successfully; it is not tied to MQTT
(re)connect:

```json
{
  "schema": "1.0",
  "role": "pa",
  "device": { "model": "ACOM 1200S", "serial": "acom-1200s" },
  "link": "serial",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "bands": ["160m","80m","40m","30m","20m","17m","15m","12m","10m","6m"],
    "max_power_w": 1200,
    "band_source": "rf_sense",
    "rf_sample": false,
    "key_input": "hardware",
    "alc_out": true,
    "modes": ["operate","standby"]
  },
  "expose": {
    "device": { "name": "ACOM 1200S", "model": "ACOM 1200S", "manufacturer": "ACOM" },
    "fields": [ ... see below ... ]
  }
}
```

`capabilities` semantics: the amplifier auto-bands by sensing RF drive (`rf_sense`, no CAT cable);
no independent RF sample output (`rf_sample:false`); keyed by a hardware key line
(`key_input:hardware`) so there is no `set_keyed` command; it has an ALC output to the radio
(`alc_out:true`); maximum output 1200 W; `device.serial` is a **configured stable id** (the serial
protocol reports no serial number; an empty config value defaults to `acom-1200s`). `firmware` is
omitted (unknown over serial).

`expose` is the consumer-neutral field surface (rendered into Home Assistant — a home-automation
platform that auto-discovers MQTT devices — by the separate `hadiscovery` service; it contains no
consumer-specific vocabulary). Exact field list:

| key | name | type | extras |
|---|---|---|---|
| `mode` | Mode | enum | `options_ref: modes`, `writable: true`, command `{action: set_mode, value_key: value, value_type: string}` |
| `band` | Band | enum | `options_ref: bands`, `writable: true`, command `{action: set_band, value_key: value, value_type: string}` |
| `power` | Power | enum | `options: ["on","off"]` — **read-only** (no command) |
| `keyed` | Keyed | enum | `options: ["rx","tx","inhibited"]` |
| `fwd_power_w` | Forward Power | number | `unit: "W"`, `class: power`, `state_class: measurement` |
| `rfl_power_w` | Reflected Power | number | `unit: "W"`, `class: power`, `state_class: measurement` |
| `temp_c` | Temperature | number | `unit: "°C"`, `class: temperature`, `state_class: measurement` |
| `swr` | SWR | number | `state_class: measurement` |
| `fault` | Fault | enum | `options: ["none","swr","temp","reflected","other"]` |
| `pa_state` | PA State | string | — |
| `device_online` | Device Online | boolean | — |

`expose.device` deliberately carries **no `area`** — the standalone `hadiscovery` service supplies a
deployment-wide default Home Assistant area. (The top-level `location: bauwagen` is the bus-identity
location, a separate concept.)

### 3.4 Legacy embedded Home Assistant discovery [NON-NORMATIVE]

A legacy Home Assistant discovery emitter exists in the reference implementation, compiled in but
disabled by config (`mqtt.publish_ha_discovery = false`). When enabled it publishes retained
discovery configs under
`homeassistant/select|sensor|binary_sensor/acom-<sanitized-serial>/<object_id>/config` with node id
`acom-<serial>` — a **different** node id than the hadiscovery path's `muehle-hf-pa`, so switching
paths creates duplicate entities. It is slated for deletion; a re-implementation may omit it
entirely if the `hadiscovery` consumer is used.

### 3.5 Cadence summary

| Event | Publishes |
|---|---|
| MQTT (re)connect | `/status` = `online`; re-subscribe `/cmd` |
| Serial port opened (each restart cycle) | `/meta` (full, incl. `expose`); `/state` snapshot with `device_online:true`; legacy discovery if enabled |
| Each telemetry frame | `/state` **if** dedup allows (§3.2) |
| Serial port lost | `/state` with `device_online:false`, `power:"off"`, `error:"serial: <cause>"`; `/status` stays `online` |
| Bridge crash / unclean death | `/status` = `offline` (broker LWT) |
| Idle, nothing changing | `/state` heartbeat every ≤ 60 s |

There is no periodic `/status` republish and no other heartbeat.

---

## 4. Command surface

Subscribed topic: `muehle/hf/pa/cmd` (QoS 1, not retained). Payload JSON follows the station `/cmd`
convention (see `02-interface-spec.md` §6): **the argument rides under the key `value`**, never
under a key named after the action, and is always a JSON string:

```json
{"action": "set_mode", "value": "operate"}
{"action": "set_mode", "value": "standby"}
{"action": "set_band", "value": "20m"}
```

| Action | Accepted values | Effect |
|---|---|---|
| `set_mode` | `operate` \| `standby` | sends the corresponding mode frame (§2.7); any other value is logged and dropped, nothing sent |
| `set_band` | `160m`/`160`, `80m`/`80`, `40m`/`40`, `30m`/`30`, `20m`/`20`, `17m`/`17`, `15m`/`15`, `12m`/`12`, `10m`/`10`, `6m`/`6` (case-insensitive, whitespace-trimmed) | walks the amplifier band-by-band (§2.7). Rejected (logged, nothing sent) if the label is unknown or the current band index is 0/unknown |
| anything else — including `set_power` | — | logged as `unknown action` and ignored. **There is no `set_power` and no `clear_fault`** (the ACOM protocol as implemented exposes no fault-clear transmit command) |

A malformed JSON payload SHALL be logged and ignored. `set_mode` works even before any telemetry
frame has arrived (it needs no band knowledge); `set_band` is rejected until a telemetry frame has
provided a current band index.

**Feedback contract**: commands are fire-and-forget. There is no reply topic and no error topic;
success or failure is observable only through subsequent `/state` telemetry (mode/band change) and
process logs. Command execution errors are logged at warn level and dropped.

**Dispatch architecture [library-independent constraint]**: the MQTT message handler itself must
never block or perform the serial work inline. A `set_band` walk can hold the serial line for over
1 second; a handler that blocks the MQTT client's receive/dispatch path deadlocks the client — this
exact class of bug deadlocked a sibling component live. The bridge SHALL queue the raw payload on a
**bounded queue of capacity 8** consumed by a **single worker**, which serializes execution so no
two band walks interleave. If the queue is full the command SHALL be **dropped** with a
`/cmd queue full, dropping command` warning rather than blocking. (This constraint applies
regardless of the MQTT library used; see `03-components/common-runtime-library.md` for the
station-wide rule derived from this incident.)

---

## 5. Behavior and state machine

### 5.1 Startup sequence (exact order)

1. Parse command-line flags (`-config`, `-log.level`, `-debug`), load config (§6); a config load or
   validation failure SHALL exit with code **2**. The `-debug` flag hex-dumps **all serial I/O**
   (both directions, raw bytes as received/transmitted) to stderr, for operator diagnostics only;
   it changes no bus behavior and no protocol bytes.
2. Create the logger (stderr, text format; levels debug|info|warn|error, default info).
3. Install a signal handler: SIGINT/SIGTERM cancel a root shutdown context.
4. Build the serial device, bridge state, and command worker (before any I/O).
5. Connect MQTT: register LWT (`offline`, retained, QoS 1 on `/status`), retry the initial connect
   every 5 s forever. The process SHALL NOT fail on a broker outage. **The initial connect SHALL be
   abortable by the shutdown signal** (see §5.3). A connect failure not caused by shutdown SHALL
   exit with code **1**.
6. On connect: publish `/status` = `online` (retained, QoS 1), subscribe `/cmd` at QoS 1.
7. Enter the serial restart loop (§5.2).

### 5.2 Serial restart loop

```
loop:
  open port (9600 8N1), reset buffers
  → on error: mark device offline, backoff, retry
  send enable-telemetry (55 92 04 15)   ← the open FAILS if this write errors
  mark device_online = true (publish snapshot)
  publish /meta
  read loop:
    - resync/parse frames (§2.3), ACK each, emit telemetry → dedup → /state
    - >5 s silence: re-send enable every ≥5 s (§2.6)
    - >30 s silence, or read error, or shutdown signal: exit read loop, close port
  mark device_online = false, power = "off", error = "serial: <cause>" (publish snapshot)
  backoff: wait 2 s, then ×1.5 each failure, capped at 60 s
  goto loop (unless shutdown signalled)
```

Backoff constants: initial **2 s**, multiplier **1.5×**, max **60 s**. The loop exits only on the
shutdown signal. (Note: the reference implementation does not reset the backoff after a
long-successful session — see §8, defect 4.)

### 5.3 Shutdown behavior

A shutdown signal (SIGTERM/SIGINT) SHALL interrupt **every** blocking wait in the process:

- If blocked in the initial MQTT connect (e.g. during a broker outage): the connect wait SHALL abort
  and the process SHALL exit **within 5 s of the shutdown signal** even when the broker is
  unreachable — far below the systemd default 90 s stop timeout, which the process MUST NOT depend
  on. A test SHALL verify this bound (send the shutdown signal while the broker is unreachable;
  assert process exit within 5 s). **History [must not regress]**: in the reference MQTT
  library (paho), the connect wait ignored the caller's cancellation, and this exact bridge hung
  live on SIGTERM during a broker outage until systemd force-killed it after the stop timeout. The
  fix routes the wait through a watcher that races the shutdown signal.
- If in the serial read loop: closing the serial port on the shutdown signal SHALL unblock the
  1 s read; the loop returns cancelled.
- If in a backoff sleep: it SHALL return immediately.
- An in-flight band walk is not interrupted mid-walk (worst case ≈ 9 steps × 150 ms ≈ 1.35 s
  remains), then the loop notices the shutdown signal.
- The MQTT client disconnects cleanly (with a short quiesce window, 500 ms in the reference
  implementation); because the disconnect is clean, **the broker does not fire the LWT** — retained
  `/status` keeps its last value `online` after a clean stop (station-wide known behavior; see
  `02-interface-spec.md` §5 and §8, defect 1).

Exit code conventions [NORMATIVE]:

- **0** when the run ended via shutdown signal (a clean service stop is not a failure; this is what
  prevents `systemctl stop` from triggering a restart),
- **1** for other run errors (e.g. MQTT connect failure not caused by shutdown),
- **2** for config load/validation errors.

### 5.4 MQTT outage during operation

During an MQTT outage the bridge SHALL keep reading serial and keep attempting publishes; the
client reconnects automatically and `/status` is re-published `online` on reconnect. Commands
published while the bridge is disconnected are lost (`/cmd` is not retained).

---

## 6. Configuration

TOML file, default path `/etc/acom1200s-pa-bridge/config.toml` (flag `-config <path>` overrides).
A missing config file is **not** an error — built-in defaults are used. Unknown keys are silently
ignored. Precedence: defaults → TOML → environment → `-log.level` flag.

| Key | Default (in binary) | Meaning |
|---|---|---|
| `host` | `shari` | compute node name, published in `/meta.host` |
| `serial.port` | `/dev/serial/by-id/usb-Prolific_Technology_Inc._USB-Serial_Controller_D-if00-port0` | serial device path; by-id symlink survives adapter replugs |
| `serial.avg_time_ms` | `1` | forward-power moving-average window in ms; 1 = raw per-frame. **The deployment script seeds 300 on first install** — see §7 and §10 |
| `device.model` | `ACOM 1200S` | published in `/meta.device.model` |
| `device.serial` | `""` (→ derived `acom-1200s`) | stable configured id; the serial protocol reports no serial number |
| `device.link` | `serial` | transport label in `/meta.link` |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | broker URL |
| `mqtt.client_id` | `""` (→ `muehle-hf-pa`) | MQTT client id; empty derives `<site>-<station>-<slot>` |
| `mqtt.user` | `hf` | MQTT username |
| `mqtt.password` | `""` | **never set in TOML in production** — environment override only |
| `mqtt.site` / `mqtt.station` / `mqtt.slot` | `muehle` / `hf` / `pa` | slot address; site+station are mandatory (validation fails otherwise); slot falls back to `pa` if empty |
| `mqtt.location` | `bauwagen` | physical location label in `/meta` |
| `mqtt.discovery_prefix` | `homeassistant` | legacy HA discovery prefix (only used when legacy discovery is enabled) |
| `mqtt.publish_ha_discovery` | `false` | gate for legacy embedded HA discovery (§3.4) |
| `log.level` | `info` | `debug` \| `info` \| `warn` \| `error` |

Environment overrides (prefix `ACOM1200S_PA_BRIDGE_`): `MQTT_BROKER`, `MQTT_CLIENT_ID`,
`MQTT_USER`, `MQTT_PASSWORD`, `MQTT_SITE`, `MQTT_STATION`, `MQTT_SLOT`, `SERIAL_PORT`. Empty
environment values are ignored (non-empty only).

**Secrets convention** (see `05-deployment-ops.md` and the shared config/secrets doc): the MQTT
password lives in `/etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env` (0600, owned by the service
user), loaded by the systemd unit via `EnvironmentFile=` as
`ACOM1200S_PA_BRIDGE_MQTT_PASSWORD="..."`. It never appears in the TOML, the unit file, or the
process command line.

Validation (exit 2 on failure): `mqtt.site` and `mqtt.station` must be non-empty; `serial.port`
must be non-empty.

### 6.1 Power is never commanded [NORMATIVE safety rule]

The bridge SHALL NOT include any power-command mechanism: no `set_power` action, no
`power_default` config, no host serial control-line (RTS) manipulation. PA mains power is owned by
the `muehle/hf/switch` slot (remote-on relays; see `03-components/m5stamp-relay-controller.md`
§2). The
`power` field in `/state` is read-only telemetry. A re-implementation SHALL reject a `set_power`
command (log + ignore). See also `06-safety.md`.

---

## 7. Deployment

- Target: Raspberry Pi **shari**, `192.168.1.139` (ssh user `io`). MQTT broker at
  `192.168.1.50:1883`.
- The deployment flow: cross-compile a static binary, copy binary + systemd unit + **seed** config
  and env files to the Pi, then over ssh:
  - create system user `acom1200s-pa-bridge` (no login shell, no home) if missing; ensure group
    `dialout` exists and add the user to it;
  - install udev rule
    `/etc/udev/rules.d/99-acom1200s-pa-bridge-serial.rules`:
    `SUBSYSTEM=="tty", SUBSYSTEMS=="usb", ATTRS{idVendor}=="067b", GROUP="dialout", MODE="0660"`
    and trigger udev — pins the Prolific adapter's tty to the serial group;
  - **seed-once**: install `/etc/acom1200s-pa-bridge/config.toml` (0600) and
    `acom1200s-pa-bridge.env` (0600) only if absent — the Pi owns its settings afterwards;
    redeploys never overwrite them;
  - install the binary to `/opt/acom1200s-pa-bridge/acom1200s-pa-bridge` (755), the unit to
    `/etc/systemd/system/acom1200s-pa-bridge.service`, daemon-reload, enable, restart.
- Seed defaults used on *first* deploy differ from in-binary defaults in exactly one place:
  `serial.avg_time_ms = 300` (deploy seed) vs `1` (binary/example config). See §10.
- systemd unit (hardened; serial-specific):
  - `After/Wants=network-online.target`; `Type=simple`;
    `ExecStart=/opt/acom1200s-pa-bridge/acom1200s-pa-bridge -config /etc/acom1200s-pa-bridge/config.toml`;
    `EnvironmentFile=/etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env`;
  - `User=Group=acom1200s-pa-bridge`, `SupplementaryGroups=dialout`,
    `ConfigurationDirectory=StateDirectory=acom1200s-pa-bridge`;
  - `Restart=on-failure`, `RestartSec=5`; no explicit `TimeoutStopSec` (systemd default 90 s);
  - hardening: `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
    `ProtectKernelTunables/Modules/ControlGroups`, `RestrictAddressFamilies=AF_INET AF_INET6`,
    `RestrictNamespaces`, `LockPersonality`, `RestrictRealtime`, `RestrictSUIDSGID`, `RemoveIPC`,
    empty `CapabilityBoundingSet`/`AmbientCapabilities`,
    `ReadWritePaths=/var/lib/acom1200s-pa-bridge`;
  - **no `PrivateDevices`** (must open the serial tty); instead `DeviceAllow=char-ttyUSB rw`,
    `char-ttyACM rw`, `char-tty rw`; `MemoryMax=256M`, `TasksMax=64` (shari is a shared host — one
    leaky bridge must not exhaust the station);
  - logs to the journal under identifier `acom1200s-pa-bridge`.
- **One-time migration from the predecessor service** (idempotent, run **before** the first deploy):
  on the Pi, create the new user and config directory, copy the predecessor `acombridge` config to
  the new path, copy its env file **rewriting the variable prefix `ACOMBRIDGE_` →
  `ACOM1200S_PA_BRIDGE_` on the device** (the secret never leaves the Pi), then stop, disable and
  remove the old service, unit, install, config, udev rule, and (best-effort) the old user.
  Ordering matters: migration first, so the deploy's seed-once keeps the real config and password.

Dependencies: a static binary (no runtime language deps); the MQTT broker reachable; the ACOM on the
Prolific adapter.

---

## 8. Invariants and known defects

### 8.1 Invariants [NORMATIVE — every re-implementation SHALL satisfy these]

1. **The bridge never commands power** (§6.1). `power` in `/state` is read-only telemetry; a
   `set_power` command is rejected (log + ignore).
2. **Raw firmware strings never leak into canonical fields.** `mode`, `keyed`, `fault` contain only
   the canonical enum values; raw firmware modes live only in `pa_state`, verbatim fault text only
   in `error`.
3. **Serial writes are serialized** — ACKs, enable, mode and band frames never interleave on the
   wire; band walks are atomic sequences of steps.
4. **Commands never block the MQTT receive path**: bounded queue (capacity 8), drop on overflow,
   single worker.
5. **`/status` (LWT) tracks only the bridge process**; device link loss is reported exclusively via
   `/state.device_online` (plus `power:"off"`). Consumers must check both layers (station-wide
   rule; see `02-interface-spec.md` §5).
6. **Frequency is never published by this slot** — bus frequency lives on the radio slot as
   `freq_hz` (integer Hz). The amplifier's kHz value is decoded and discarded.
7. **Exit code 0 on shutdown signal** so a service stop is clean and does not restart-loop.
8. **A shutdown signal must interrupt every blocking wait** (broker connect during outage, serial
   read, backoff sleep) — no dependence on force-kill timeouts; the process exits within **5 s** of
   the signal in every state, and a test SHALL verify the broker-outage case (§5.3).
9. **`set_band` is rejected when the current band is unknown** (index 0) rather than walking blind.
10. **Power meters read 0 W whenever the amplifier is not keyed** (`keyed != "tx"`), so displays
    can never show stale transmit power while receiving.
11. **`/meta` is republished after every serial reconnect** so late subscribers always see the birth
    certificate; `/cmd` is resubscribed on every MQTT reconnect.
12. **The enable frame `55 92 04 15` is sent checksum-less** — a checksummed enable frame is
    incorrect.

### 8.2 Known defects of the reference implementation

These are documented actual behaviors of the shipped code. A re-implementation SHOULD fix those
marked "fix", but any behavior change must be a conscious decision because consumers may depend on
the current observable behavior.

1. **Stale retained `/status: online` after clean stop** — LWT fires only on unclean death; on a
   clean stop the broker keeps the last retained `online`. A stopped bridge still looks "online" to
   `/status`-only consumers. (Live station behavior; consumers are required to use the two-layer
   liveness rule. A deliberate clean-stop publish of `offline` would be a fix — but see the
   station-wide discussion in `02-interface-spec.md`.)
2. **Docs lag the code in three places** (code is truth):
   - the in-repo API doc and README say `/state` is "published on every telemetry frame" — the code
     dedups idle frames with a 60 s heartbeat (§3.2);
   - the README claims a "watchdog" re-enables telemetry when the amplifier is power-cycled — the
     amp-power-cycle watchdog was removed with the `set_power` machinery; what remains is the 5 s
     silence re-send inside a live session and the 30 s silence reconnect;
   - a stale source comment still lists `set_power` among dispatched commands — it is rejected as
     unknown.
3. **`avg_time_ms` default mismatch** — in-binary/example = 1 (raw), deploy seed = 300 (300 ms
   averaging). A device deployed from scratch behaves differently from one run with defaults; which
   value the live config on shari carries is unknown from the repo (§10).
4. **Backoff never resets after a good run** — the serial-restart backoff grows on every failure but
   is never reset to 2 s after a long successful session, so a bridge that ran fine for days waits
   up to 60 s to reopen after the next port loss. Since amplifier power-off via `hf/switch` is a
   routine event, this regularly delays telemetry resumption by tens of seconds. (Arguably should
   be fixed; changing it alters recovery timing.)
5. **Band walk can leave the amplifier mid-walk** — a serial write error mid-walk aborts with the
   amplifier between bands; there is no reconciliation or retry (the next telemetry frame reports
   the actual band). Also nothing prevents a band walk while transmitting (the amplifier itself may
   refuse with fault `STOP TRANSMISSION FIRST`, leaving position uncertain).
6. **No command feedback on the bus** — success/failure exists only in logs; bus consumers see
   outcomes only via telemetry deltas (§4).
7. **Degenerate first snapshot** — after a serial open but before the first telemetry frame, the
   bridge publishes a snapshot with `device_online:true` whose canonical fields are the previous
   cycle's values — or, on the very first cycle, empty strings (`mode`, `keyed`, `fault`, `pa_state`
   all `""`, `power` `""`). Momentarily odd JSON for consumers that assume non-empty enums.
8. **Idle numeric staleness** — because dedup ignores numeric changes, `temp_c`/`swr` drift is
   published at most every 60 s when idle.
9. **Enable-frame checksum asymmetry** — by design (vendor quirk), see §2.4 and invariant 12.
10. **Commands lost while the bridge is disconnected** — `/cmd` is not retained; publishes during an
    MQTT outage are dropped silently.
11. **Dead code** — an unused `Reset` operation and an unused last-publish-topic field exist in the
    reference implementation.
12. **Publish errors invisible** — publishes are fire-and-forget into the client's queue; QoS 1
    delivery errors surface nowhere in logs (messages may be queued during a brief outage, not
    lost, but errors are invisible).
13. **History (fixed, must not regress)** — the connect-wait-ignoring-shutdown bug hung this
    bridge's SIGTERM live during a broker outage (§5.3). Any re-implementation must be tested for
    "shutdown signal while broker unreachable exits within 5 s".

---

## 9. Reference-implementation notes [NON-NORMATIVE]

The shipped component is a Go program using the Eclipse paho Go MQTT client, the `go.bug.st/serial`
serial library, and a TOML parser; it is built via the repo's shared module for MQTT plumbing
(publish/queue helpers, topic addressing). These choices are implementation detail. What must be
preserved is everything observable on the wire and the bus: the serial contract (§2), the topic and
payload contracts (§3), the command surface (§4), the state machine and timings (§5), the config
surface (§6) — operators' config files and the systemd EnvironmentFile must keep working — and
the deployment layout and permissions (§7).

Free to change: programming language and libraries; the internal concurrency architecture (only the
observable serialization of serial writes and the non-blocking MQTT dispatch matter); log message
wording (keep semantic content); the hex-dump debug format; the legacy embedded HA discovery
(§3.4); the 500 ms disconnect quiesce window; the backoff reset behavior (defect 4) if the timing
change is a conscious decision.

---

## 10. Open decisions and unresolved facts

- **Which `avg_time_ms` is actually running on shari?** The deployment script seeds
  `serial.avg_time_ms = 300` on first install, while the binary default and the example config say
  `1` (raw per-frame). The seeded config file is device-owned and was not readable from the
  workstation when this PRD was written, so the live value is unknown. Evidence for 300:
  `deploy.sh` seed block; evidence for 1: binary default, `config.example.toml`. Any
  re-implementation must decide whether the normative default is 1 or 300, and confirm the live
  value on the device. This is the "on-device-truth" open question.
- **Does the amplifier ACK outbound command frames (type `0x81`)?** The bridge ACKs every inbound
  frame but ignores all non-telemetry inbound frames, so a command's success is inferred only from
  subsequent telemetry. Whether the vendor protocol defines a command ACK that a re-implementation
  could consume for real command feedback is unknown — the authoritative vendor document,
  "ACOM A600S A1200S Serial Protocol V1.3.pdf" (§2.1), is not in the repo and must be obtained from
  ACOM to settle this.
  Related: command ACK semantics are unobservable in the current system — the re-implementation
  could add them, but that would be new behavior.
- **Are there other inbound frame types?** Only `0x2F` (telemetry) is consumed. Whether the vendor
  protocol defines additional inbound frame types is unknown (vendor document not in repo; see §2.1
  for its exact title).
- **Meaning of the enable-frame payload byte `0x15`** (`55 92 04 15`) — undocumented in the repo;
  the bytes are cargo-culted from the vendor protocol document (§2.1) and must be sent exactly
  as-is.
- **Precise telemetry frame rate** — not documented anywhere; only known to be "high rate even in
  OPR/RX" (a code comment motivating the dedup). A re-implementation should treat the frame rate as
  unspecified and design dedup accordingly.
- **Should a clean stop publish `/status: offline` explicitly?** Current behavior leaves retained
  `online` after a clean stop (defect 1). The station-wide rule says consumers must check both
  liveness layers, which makes the stale value survivable, but a deliberate offline publish on
  clean shutdown would be a cleaner contract — a station-wide decision (see
  `02-interface-spec.md` §5), not one for this component alone.
- **Should the serial backoff reset after a successful session?** (defect 4) — behavior change
  with timing consequences; flagged as a conscious decision for the re-implementation.
- **`device_online` form**: the deployed bridge publishes `device_online:true` explicitly; the
  integration model says "omitted when true". Consumers must treat both forms as equivalent
  (absence = true) or the contract must mandate explicit-true — a station-wide open decision
  (see `02-interface-spec.md` §5), not resolvable from this component alone.
- **Broker topology**: this component's defaults target `tcp://192.168.1.50:1883` (current
  production). A planned migration to a broker on shari (`192.168.1.139`) exists on an unmerged
  feature branch, committed but NOT deployed as of 2026-08-29 — see `05-deployment-ops.md` for the
  migration decision.