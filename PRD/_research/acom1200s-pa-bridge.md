# Research spec — acom1200s-pa-bridge (ACOM 1200S PA → MQTT bridge)

Source analyzed: `/Users/ingomar.otter/dev/stationa/acom1200s-pa-bridge` (Go), plus the shared modules it
imports (`shared/mqtt`, `shared/schema`). Code is truth; READMEs lag where noted.

This document is written so a team using a different language/framework can reconstruct the component's
**observable behavior** exactly. Sections marked **[CONTRACT]** must be preserved verbatim in any
reimplementation; sections marked **[INCIDENTAL]** describe how the current Go code happens to do it.

A glossary of every domain term at first use:

- **Ham radio / HF**: amateur radio on shortwave frequencies. This station ("Mühle", site id `muehle`) is an amateur-radio installation.
- **Linear amplifier / PA (power amplifier)**: a device that amplifies the radio's transmit signal to high power (the ACOM 1200S delivers up to 1200 W). This is the device being bridged.
- **MQTT**: lightweight publish/subscribe messaging over TCP. A message goes to a *topic* (hierarchical string, e.g. `muehle/hf/pa/state`); subscribers to that topic receive it. A *retained* message is stored by the broker and delivered to every future subscriber. **QoS 0** = fire-and-forget, **QoS 1** = at-least-once.
- **LWT (Last Will and Testament)**: a message the broker publishes on the client's behalf if the client disappears without closing the connection (crash, network loss). Not published on a *clean* disconnect.
- **Bridge**: a small daemon that translates between a physical device's proprietary protocol and MQTT.
- **Slot**: the station's addressing unit — a topic prefix `<site>/<station>/<slot>` (e.g. `muehle/hf/pa`) with four planes `meta`, `state`, `status`, `cmd`.
- **SWR (standing wave ratio)**: a dimensionless ratio ≥ 1 measuring how well the antenna is matched; high SWR means power reflected back into the amplifier (bad).
- **Keyed / keying**: the act of putting the amplifier into transmit (TX). The amplifier is keyed by a **hardware key line** from the radio, never over MQTT.
- **ATU (automatic tuner)**: an antenna-matching unit. The ACOM 1200S may be used with one; its fault codes include ATU errors.
- **ALC (automatic level control)**: a feedback signal from the PA back to the radio to limit drive power. The ACOM has an ALC output.
- **CAT**: "computer-aided transceiver" — a serial link by which a radio reports its frequency/band. This bridge's serial adapter carries **no** CAT cable; the amplifier senses the band from the RF drive itself (`band_source: rf_sense`).
- **Prolific adapter**: a common USB-to-RS232 chip (USB vendor id `067b`) used to reach the amplifier's RS-232 port.
- **OPR/RX, OPR/TX**: the amplifier firmware's "operating, receiving" and "operating, transmitting" states.
- **Home Assistant (HA)**: a home-automation platform that can auto-discover MQTT devices via "discovery" config messages. Discovery is rendered for this slot by a separate service (`hadiscovery`) in the default configuration.

---

## 1. Purpose & role

acom1200s-pa-bridge is a Linux daemon that connects an **ACOM 1200S HF linear amplifier** (via its RS-232
service port and a Prolific USB-serial adapter) to the station's MQTT bus, occupying slot `muehle/hf/pa`.
It does three things:

1. **Telemetry**: continuously reads the amplifier's proprietary binary serial protocol and publishes a
   canonical, human-readable JSON snapshot of the amplifier state to the retained topic
   `muehle/hf/pa/state`.
2. **Control**: accepts two commands over MQTT — `set_mode` (operate/standby) and `set_band` — and
   translates them into serial frames to the amplifier.
3. **Liveness**: announces its own process liveness via MQTT LWT on `muehle/hf/pa/status`, and separately
   reports *device* (serial-link) liveness inside the state snapshot (`device_online`).

It is deliberately a **pure observer of the amplifier's power state**: the amplifier is powered on/off by
a *different* component (the `muehle/hf/switch` slot's remote-on relays). This bridge never sends a power
command, never drives any host serial control line (RTS removed), and has no `set_power` command and no
`power_default` config. The `power` field in `/state` is **telemetry only** (the amplifier's actual power
state, derived from its firmware mode). An earlier RTS-wake-line `set_power` mechanism "never worked
reliably and has been removed" (docs/pa-mqtt-api.md).

Runs as a systemd service on the Raspberry Pi "shari" (192.168.1.139), talking to the MQTT broker at
192.168.1.50:1883 (user `hf`).

---

## 2. Upstream interface — the ACOM 600S/1200S serial protocol [CONTRACT]

### 2.1 Transport

- USB-serial adapter (Prolific, USB vendor id `067b`), opened by device path, by default
  `/dev/serial/by-id/usb-Prolific_Technology_Inc._USB-Serial_Controller_D-if00-port0`
  (stable across replugs; the deployment also installs a udev rule pinning this tty to group `dialout`).
- Serial parameters are protocol constants, **not configurable**: **9600 baud, 8 data bits, no parity,
  1 stop bit (9600 8N1)**.
- Read timeout on the port: **1 second** (a read returns 0 bytes after 1 s of silence).
- On open: input and output buffers are reset, then the enable-telemetry command is sent (see 2.4).
- Protocol reference: vendor doc "ACOM A600S A1200S Serial Protocol V1.3.pdf" (not in repo; the byte
  layout below is what the code implements).

### 2.2 Frame format [CONTRACT]

All frames (both directions) share this layout:

| Offset | Meaning |
|---|---|
| byte 0 | start byte, always `0x55` |
| byte 1 | message address / message type |
| byte 2 | total frame length **including** this length byte and the trailing checksum byte |
| bytes 3..n-2 | payload |
| last byte | checksum |

**Checksum rule**: the checksum byte is chosen so that the **sum of all bytes in the frame, mod 256, is
zero**. Receiver-side a frame is valid only if `len >= 4` and the byte-sum is 0. Sender-side the checksum
is computed as `0 - sum(all other bytes)` (mod 256) and appended. The smallest valid frame is 4 bytes.

**Exception**: the enable-telemetry frame (2.4) is sent as a fixed 4-byte sequence and the code
deliberately does **not** append a checksum to it (see 2.4).

### 2.3 Receive path (telemetry + ACK) [CONTRACT]

Byte-stream resynchronization logic (`processBuffer`), byte-exact:

1. Scan forward one byte at a time until a `0x55` start byte is found.
2. Read the length byte (byte 2). If the declared length `< 4`, treat as noise: discard one byte and
   rescan (this also protects against indexing an empty/short packet).
3. If fewer bytes than the declared length are buffered, wait for more data (keep remainder in buffer).
4. When a full frame is available, verify the checksum (sum of all bytes == 0 mod 256).
   - Checksum **fails** → discard one byte and rescan (never discard the whole buffer).
   - Checksum **passes** → handle the frame and consume exactly its declared length.

For every valid frame received, regardless of type, the bridge sends an **ACK frame**:
`{0x55, 0x86, 0x05, <byte-1-of-received-frame>, <checksum>}` — i.e. start `0x55`, message type `0x86`,
length `0x05` (5 bytes total), the received frame's address byte echoed, then the checksum byte that
zeroes the sum.

Frames with address `0x2F` are **telemetry frames**; all others are ACKed and ignored.

### 2.4 Enable-telemetry command [CONTRACT]

Sent on port open, and re-sent during silence (see 2.6). Exact bytes written:

```
55 92 04 15
```

(start `0x55`, message type `0x92` = "enable automatic telemetry", length `0x04`, payload byte `0x15`).
**No checksum byte is appended** — the sender code skips checksum appending when the frame's type byte is
`0x92`. This is a hardcoded quirk matching the vendor protocol; a reimplementation must send exactly
these 4 bytes. Once the amplifier receives this, it streams 72-byte telemetry frames automatically.

### 2.5 Telemetry frame decode (72 bytes, address `0x2F`) [CONTRACT]

The frame is decoded **only if its total length is exactly 72 bytes**; other lengths are silently
dropped (the parser is lenient — short frames are not fatal). All multi-byte values are **little-endian**.

| Byte offsets | Type | Meaning |
|---|---|---|
| `[3]` | byte | firmware mode; decode by **high nibble** (`b & 0xF0`), table below |
| `[16:18]` | uint16 LE | heatsink temperature in **Kelvin**; published value = K − 273.15; raw 0 decodes to 0.0 °C (special case, no −273.15) |
| `[20:22]` | uint16 LE | input (drive) power in **deciwatts** → /10.0 W (parsed but **never published**) |
| `[22:24]` | uint16 LE | forward power in W (raw, before averaging) |
| `[24:26]` | uint16 LE | reflected power in W |
| `[26:28]` | uint16 LE | SWR raw → /100.0 (e.g. raw 120 → 1.2) |
| `[48:50]` | uint16 LE | frequency in **kHz** (parsed but **never published** — the PA slot does not own bus frequency; the radio slot owns `freq_hz` in Hz) |
| `[66]` | byte | fault byte; `0xFF` = no fault |
| `[69]` | byte | band; decode by **low nibble** (`b & 0x0F`), 1..10 per table below; anything else (0, 11..15) → band label `"UNK"` and band **index 0 (unknown)** |

Firmware mode table (`[3] & 0xF0` → raw string, published verbatim as `pa_state`):

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

Band table (low nibble of `[69]` → canonical label and index):

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

Note: the ACOM 1200S has **no 60m band**; the 10-band list above is the amplifier's complete set and is
also what `set_band` accepts.

Fault byte → verbatim message (this exact string is published in the state `error` field when a fault is
active):

- `0xFF` → `NONE` (no fault; published as empty `error`)
- `0x00` → `HOT SWITCHING ATTEMPT` (attempt to switch RF routing while transmitting)
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
- `0x10` → `5V TOO LOW` / `0x11` → `5V TOO HIGH`
- `0x12` → `26V TOO LOW` / `0x13` → `26V TOO HIGH`
- `0x14` → `ERROR 0x14`
- `0x15` → `PAM1 FAN SPEED TOO LOW` / `0x16` → `PAM2 FAN SPEED TOO LOW` / `0x17` → `LPF FAN SPEED TOO LOW`
  (PAM1/PAM2 = the two power-amplifier modules; LPF = low-pass filter)
- `0x18` → `PAM1 DISSIPATION TOO HIGH` / `0x19` → `PAM2 DISSIPATION TOO HIGH`
- `0x1A` → `PAM1 DISSIPATION WARNING` / `0x1B` → `PAM2 DISSIPATION WARNING`
- `0x1C` → `PAM1 TEMP TOO HIGH` / `0x1D` → `PAM2 TEMP TOO HIGH`
- `0x1E` → `PAM1 EXCESSIVE TEMP` / `0x1F` → `PAM2 EXCESSIVE TEMP`
- `0x20` → `PAM1 HV TOO LOW` / `0x21` → `PAM1 HV TOO HIGH` (HV = high voltage supply)
- `0x22` → `PAM1 CURRENT NON-ZERO` / `0x23` → `PAM1 IDLE CURRENT TOO LOW`
- `0x24` → `PAM1 CURRENT WARNING` / `0x25` → `PAM1 EXCESSIVE CURRENT`
- `0x26`–`0x29` → `BIAS_1A VOLTAGE ERROR`, `BIAS_1B VOLTAGE ERROR`, `BIAS_1C VOLTAGE ERROR`, `BIAS_1D VOLTAGE ERROR`
- `0x2A`–`0x2D` → `BIAS_1A SHOULD BE ZERO`, `BIAS_1B SHOULD BE ZERO`, `BIAS_1C SHOULD BE ZERO`, `BIAS_1D SHOULD BE ZERO`
- `0x2E` → `PAM1 GAIN TOO LOW` / `0x2F` → `PAM1 GAIN TOO HIGH`
- `0x30` → `PAM1 HV SHOULD BE ZERO` / `0x31` → `PAM1 CURRENT SHOULD BE ZERO`
- `0x32` → `PAM1 EXCESSIVE TEMP (3)` / `0x33` → `PAM1 TEMP TOO HIGH (3)`
- `0x34`–`0x37` → `BIAS_1A SHOULD BE ZERO (3)` … `BIAS_1D SHOULD BE ZERO (3)`
- `0x38` → `PSU1 EXCESSIVE TEMP` (PSU = power supply unit)
- `0x39` → `PAM1 EXCESSIVE CURRENT (CHECK SWR)`
- `0x40` → `PAM2 HV TOO LOW` / `0x41` → `PAM2 HV TOO HIGH`
- `0x42` → `PAM2 CURRENT NON-ZERO` / `0x43` → `PAM2 IDLE CURRENT TOO LOW`
- `0x44` → `PAM2 CURRENT WARNING` / `0x45` → `PAM2 EXCESSIVE CURRENT`
- `0x46`–`0x49` → `BIAS_2A VOLTAGE ERROR` … `BIAS_2D VOLTAGE ERROR`
- `0x4A`–`0x4D` → `BIAS_2A SHOULD BE ZERO` … `BIAS_2D SHOULD BE ZERO`
- `0x4E` → `PAM2 GAIN TOO LOW` / `0x4F` → `PAM2 GAIN TOO HIGH`
- `0x60` → `PSU1 CONTROL MALFUNCTION` / `0x61` → `PSU2 CONTROL MALFUNCTION`
- `0x62` → `PSU1 EXCESSIVE TEMP` / `0x63` → `PSU2 EXCESSIVE TEMP`
- `0x64` → `DISPLAY COMM ERROR`
- `0x65` → `ATU MODEM TEMP`
- `0x66` → `ATU POWER SWITCH ALARM` / `0x67` → `ATU POWER SWITCH ALARM (ON)`
- `0x68` → `ETHERNET NOT RESPONDING`
- `0x69` → `AUDIO MEMORY ERROR`
- `0x6C` → `LOSS OF AUDIO DATA` / `0x6D` → `LOSS OF ETHERNET DATA`
- `0x6E` → `LOSS OF EEPROM DATA (WARN)` / `0x6F` → `LOSS OF EEPROM DATA (SOFT)`
- `0x70` → `CAT ERROR`
- `0x80` → `ATU NOT RESPONDING / BIAS 1A ERR`
- `0x81` → `ATU-AMP COMM ERROR` / `0x82` → `AMP-ATU COMM ERROR`
- `0x83` → `ASEL NOT RESPONDING` (ASEL = antenna selector)
- `0x84` → `ASEL-AMP COMM ERROR` / `0x85` → `AMP-ASEL COMM ERROR`
- `0x86` → `NO TUNING SETTINGS` / `0x87` → `NO ANTENNA SETTINGS`
- `0x88` → `ATU CANNOT RETUNE (RF PRESENT)` / `0x89` → `ANTENNA CANNOT CHANGE (RF PRESENT)`
- `0x8A` → `ATU TUNING UNSUCCESSFUL` / `0x8B` → `ATU MEMORY FAIL`
- `0xA0` → `ATU DC VOLT TOO HIGH` / `0xA1` → `ATU DC VOLT TOO LOW`
- `0xA2` → `ATU 5V TOO LOW` / `0xA3` → `ATU 5V TOO HIGH`
- `0xA4` → `ANTENNA VOLT TOO HIGH (PWR)` / `0xA5` → `ANTENNA VOLT TOO HIGH (dmg)`
- `0xA6` → `ANTENNA CURRENT TOO HIGH (PWR)` / `0xA7` → `ANTENNA CURRENT TOO HIGH (dmg)`
- `0xA8` → `ANT REFL PWR TOO HIGH (SOFT)` / `0xA9` → `ANT REFL PWR TOO HIGH (HARD)`
- `0xAA` → `ATU INPUT PWR TOO HIGH` / `0xAB` → `ATU INPUT PWR TOO HIGH (dmg)`
- `0xAC` → `ANTENNA SWR TOO HIGH` / `0xAD` → `ANTENNA SWR TOO HIGH (dmg)`
- `0xAE` → `ATU TEMP TOO HIGH` / `0xAF` → `ATU TEMP TOO LOW`
- any other byte → `UNKNOWN ERROR (0xNN)` (uppercase hex)

### 2.6 Connection-loss detection & silence handling [CONTRACT]

Within one open-port session (`Device.Run`):

- Any byte received resets the silence timer.
- On each read timeout (1 s with 0 bytes):
  - If silence has lasted **> 30 s**: the run ends with error `no data received for 30s, restarting
    monitor` → device marked offline, port closed, restart with backoff (see §5).
  - Else, if silence has lasted **> 5 s** and the last retry was **> 5 s ago**: re-send the
    enable-telemetry command (`55 92 04 15`) and log `no data for 5s, re-sending enable telemetry`. This
    repeats at most once per 5 s until data resumes or the 30 s limit trips.
- A read error (port gone, USB replug) ends the run immediately; context cancellation also ends the run
  (a watcher goroutine closes the port, unblocking the read).

Practical meaning: when the amplifier is powered off by the `hf/switch` relays, the port goes silent and
the loop exits after 30 s, publishing `device_online:false`. When power returns, the next restart cycle
re-opens the port and re-sends enable-telemetry, so telemetry resumes without operator action.

### 2.7 Outbound control frames [CONTRACT]

**Set mode** — sent for `/cmd set_mode`. Frames (checksum byte appended automatically by the sender):

- `operate` → `55 81 08 02 00 06 00 <ck>`
- `standby` → `55 81 08 02 00 05 00 <ck>`

Layout: start `0x55`, type `0x81` (amplifier management), length `0x08`, sub-command `0x02` (mode
change), `0x00`, mode byte (`0x06` = OPR/RX, `0x05` = standby), `0x00`, checksum.

**Band change step** — the ACOM protocol exposes only *relative* band changes (next/previous), so
`set_band` walks the amplifier from its current band index to the target index:

- one "next band" step → `55 81 08 09 00 80 00 <ck>`
- one "previous band" step → `55 81 08 09 00 40 00 <ck>`

Layout: start `0x55`, type `0x81`, length `0x08`, sub-command `0x09` (manual band/antenna change),
`0x00`, direction byte (`0x80` = next, `0x40` = previous), `0x00`, checksum.

Walk algorithm (exact):
1. Resolve the target label to the amplifier's band index (labels `160m`..`6m` or bare numbers `160`,
   `80`, `40`, `30`, `20`, `17`, `15`, `12`, `10`, `6`; lookup is case-insensitive and trims whitespace).
   Unknown label → reject, log, no serial writes.
2. Current index comes from the last telemetry frame (0 = unknown). If unknown → reject with
   `current band unknown, cannot navigate`, log, no writes.
3. If target == current → no-op, return success.
4. Otherwise send |target − current| step frames, each in the correct direction, sleeping **150 ms
   between steps**. A write error mid-walk aborts the walk (the amplifier may be left between bands —
   see §9).

All serial writes (ACK, enable, mode, band steps) are serialized under one mutex, so command walks and
read-loop ACKs never interleave on the wire. [INCIDENTAL: mutex; CONTRACT: no interleaved frames.]

### 2.8 Forward-power averaging [CONTRACT, parameterized]

Forward power is noisy frame-to-frame, so it passes through a time-windowed moving average before
publishing: each decoded frame appends a sample; samples older than `serial.avg_time_ms` are dropped; the
published value is the arithmetic mean (integer division, truncated) of the remaining samples. Window is
clamped to ≥ 1 ms; window = 1 ms effectively publishes the raw per-frame value. Note the deployed
default is **300 ms** (deploy.sh seed) while the in-binary default is 1 ms — see §6 and §9. Reflected
power, SWR, and temperature are **not** averaged.

---

## 3. MQTT presence

Broker: `tcp://192.168.1.50:1883`, user `hf`, password from the EnvironmentFile (never in config file).
Client id: defaults to `<site>-<station>-<slot>` → `muehle-hf-pa` (config may override). Paho settings
[INCIDENTAL]: AutoReconnect on, ConnectRetry on, retry every **5 s** — the client keeps retrying the
initial connect forever until it succeeds or the process is signalled.

### 3.1 Topics — exact strings [CONTRACT]

| Topic | Direction | Retained | QoS | Payload |
|---|---|---|---|---|
| `muehle/hf/pa/meta` | publish | yes | 1 | JSON birth certificate (§3.3) |
| `muehle/hf/pa/state` | publish | yes | 1 | JSON live snapshot (§3.2) |
| `muehle/hf/pa/status` | publish (incl. LWT) | yes | 1 | plain string `online` / `offline` |
| `muehle/hf/pa/cmd` | **subscribe** | no | 1 | JSON command (§4) |

QoS rule [CONTRACT]: every retained publish (meta, state) goes out at QoS 1; a non-retained publish
would be QoS 0 (in this bridge all `/state` and `/meta` publishes are retained, hence QoS 1).

LWT [CONTRACT]: the client registers a last will of `offline` (QoS 1, retained) on `muehle/hf/pa/status`,
and on every (re)connect publishes `online` (QoS 1, retained) to the same topic. `/status` reflects the
**bridge process**, not the amplifier. The `/cmd` subscription is re-established on every reconnect
(`/cmd` is not retained, so commands sent while the bridge is down are lost).

### 3.2 `/state` payload [CONTRACT]

Single JSON document, published retained. Shape:

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

Field reference (all fields exact):

| Field | JSON type | Semantics |
|---|---|---|
| `ts` | string | RFC 3339, UTC, of this publish |
| `mode` | string | canonical: `operate` \| `standby`. `bypass` is never produced (the ACOM protocol has no bypass state). Mapping: `OPR/RX`, `OPR/TX` → `operate`; every other firmware mode (STANDBY, OFF, ATAC, RESET, INIT, DEBUG, SERVICE, MENU, UNKNOWN) → `standby` |
| `band` | string | canonical label (`160m`..`6m`, or `UNK`); **omitted** (omitempty) when empty string |
| `keyed` | string | canonical: `OPR/TX` → `tx`; `OPR/RX` → `rx`; everything else → `inhibited` (the amp will not key) |
| `fwd_power_w` | number (uint16) | forward power W, window-averaged; **forced to 0 whenever keyed != `tx`** (see below) |
| `rfl_power_w` | number (uint16) | reflected power W; **forced to 0 whenever keyed != `tx`** |
| `temp_c` | number | heatsink °C, rounded to **0.1 °C** (the K−273.15 conversion accumulates float noise; rounding keeps payloads clean) |
| `swr` | number | ratio, raw/100; **not** zeroed when not keyed |
| `fault` | string | canonical: `none` \| `swr` \| `temp` \| `reflected` \| `other` — bucket mapping in §3.2.1 |
| `pa_state` | string | raw firmware mode string (diagnostic: `OPR/RX`, `OPR/TX`, `STANDBY`, `OFF`, …). Raw firmware strings appear **only** here, never in `mode`/`keyed`/`fault` |
| `power` | string | `on` \| `off`, **read-only telemetry**: `off` only when `pa_state == OFF` (firmware fully powered down) or the serial port is lost; every other state, including STANDBY (powered but not transmitting), is `on`. Never commanded |
| `device_online` | bool | `true` while the serial loop has data; `false` when the port is lost (30 s silence or read error) |
| `error` | string | verbatim fault message (§2.5) when the fault byte != `0xFF`; otherwise empty/omitted (omitempty). Also carries serial-loop error text (e.g. `serial: open serial: ...`) when the port is lost |

**Power meter zeroing rule [CONTRACT]**: the amp can keep reporting the last transmit value after
unkeying (and the averager holds stale samples), so `fwd_power_w` and `rfl_power_w` are hard-zeroed in
every snapshot where `keyed != "tx"` (i.e. in `rx` and `inhibited`).

**Publish deduplication [CONTRACT]** (this is where the shipped docs lag — docs/pa-mqtt-api.md still
says "published on every telemetry frame"; the code throttles):

- Publish **every** telemetry frame while `keyed == "tx"`.
- Publish **every** telemetry frame in an error condition: any `fault != none`, non-empty `error`, or
  `device_online == false`.
- Publish when any of these fields change vs. the last published snapshot: `mode`, `band`, `keyed`,
  `fault`, `pa_state`, `power`, `device_online`, `error`.
- Otherwise (idle rx/inhibited, nothing meaningful changed): publish only as a heartbeat, at most once
  every **60 s** (`idleHeartbeat` constant), so the retained document's timestamp stays fresh.
- Consequence: numeric-only changes (`temp_c`, `swr`, and zeroed power meters) do **not** trigger a
  publish in idle states — idle temperature drift reaches the bus at the 60 s heartbeat cadence at most.
- On any `device_online` transition the dedup state is reset, forcing the next frame to publish.

### 3.2.1 Fault bucket mapping [CONTRACT]

`0xFF` → `none`. Otherwise the fault byte is bucketed:

- `temp`: `0x18`, `0x19`, `0x1A`, `0x1B`, `0x1C`, `0x1D`, `0x1E`, `0x1F`, `0x32`, `0x33`, `0x38`,
  `0x62`, `0x63`, `0x65`, `0xAE`, `0xAF`
- `swr`: `0x0D`, `0x39`, `0xAC`, `0xAD`
- `reflected`: `0x04`, `0x05`, `0xA8`, `0xA9`
- every other byte → `other` (the long tail: relays, drive power, HV, bias, fans, PSU, comms, CAT,
  EEPROM, ATU DC/antenna faults). The verbatim message always survives in `error`.

### 3.3 `/meta` payload [CONTRACT]

Retained JSON, published **once per serial-open cycle** (i.e. republished after every serial
reconnect/restart — and note: only after the serial port has opened successfully; it is not tied to
MQTT (re)connect):

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

`capabilities` semantics: the amp auto-bands by sensing RF drive (`rf_sense`, no CAT cable); no
independent RF sample output (`rf_sample:false`); keyed by a hardware key line (`key_input:hardware`)
so there is no `set_keyed` command; it does have an ALC output to the radio (`alc_out:true`); max
output 1200 W; `device.serial` is a **configured stable id** (the serial protocol reports no serial
number); empty config serial defaults to `acom-1200s`. `firmware` is omitted (unknown over serial).

`expose` (consumer-neutral field surface, rendered into Home Assistant by the separate `hadiscovery`
service; contains no consumer vocabulary). Exact field list:

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

`expose.device` deliberately carries **no `area`** — the standalone `hadiscovery` supplies a
deployment-wide default HA area. (`location: bauwagen` in the top-level meta is the bus-identity
location, a separate concept.)

### 3.4 Legacy embedded HA discovery [INCIDENTAL, gated off by default]

A legacy Home Assistant discovery emitter is compiled in but disabled by config
(`mqtt.publish_ha_discovery = false`). When enabled, it publishes retained HA discovery configs under
`homeassistant/select|sensor|binary_sensor/acom-<sanitized-serial>/<object_id>/config`, node id
`acom-<serial>` (note: **different** node id than the hadiscovery path's `muehle-hf-pa` — switching
paths creates duplicate HA entities). Entities: selects Mode/Band (command templates wrap HA's option
into `{"action":"set_mode","value":"{{ value }}"}` / set_band on `/cmd`), sensors for power, fwd/rfl
power, temp (device_class temperature), SWR, keyed, fault, pa_state, and a `device_online` binary_sensor
(payload on `true`/off `false`). Emitted at most once per serial-open cycle. This path is slated for
deletion; a reimplementation may omit it entirely if the `hadiscovery` consumer is used.

### 3.5 Heartbeat / cadence summary [CONTRACT]

| Event | Publishes |
|---|---|
| MQTT (re)connect | `/status` = `online`; re-subscribe `/cmd` |
| Serial port opened (each restart cycle) | `/meta` (full, incl. `expose`); `/state` snapshot with `device_online:true` (see §5 ordering); legacy discovery if enabled |
| Each telemetry frame | `/state` **if** dedup allows (§3.2) |
| Serial port lost | `/state` with `device_online:false`, `power:"off"`, `error:"serial: <cause>"`; `/status` stays `online` |
| Bridge crash / unclean death | `/status` = `offline` (broker LWT) |
| Idle, nothing changing | `/state` heartbeat every ≤ 60 s |

There is no periodic `/status` republish and no other heartbeat.

---

## 4. Command surface

Subscribed topic: `muehle/hf/pa/cmd` (QoS 1, not retained). Payload JSON, station `/cmd` convention:
**the argument rides under the key `value`**, never under a key named after the action:

```json
{"action": "set_mode", "value": "operate"}
{"action": "set_mode", "value": "standby"}
{"action": "set_band", "value": "20m"}
```

`value` is always a JSON string in this bridge (the shared payload type has `value: string`).

| Action | Accepted values | Effect |
|---|---|---|
| `set_mode` | `operate` \| `standby` | sends the corresponding mode frame (§2.7); any other value is logged and dropped, nothing sent |
| `set_band` | `160m`/`160`, `80m`/`80`, `40m`/`40`, `30m`/`30`, `20m`/`20`, `17m`/`17`, `15m`/`15`, `12m`/`12`, `10m`/`10`, `6m`/`6` (case-insensitive, whitespace-trimmed) | walks the amp band-by-band (§2.7). Rejected (logged, nothing sent) if the label is unknown or the current band index is 0/unknown |
| anything else (incl. `set_power`) | — | logged as `unknown action` and ignored. **There is no `set_power` and no `clear_fault`** (the ACOM protocol as implemented exposes no fault-clear transmit command) |

Malformed JSON payload → parse error logged, ignored. Commands received before any telemetry frame
still work for `set_mode` (no band knowledge needed) but `set_band` is rejected until a telemetry frame
has provided a current band index.

**Feedback contract [CONTRACT]**: commands are fire-and-forget. There is no reply/ack topic, no error
topic; success or failure is only observable through subsequent `/state` telemetry (mode/band change)
and process logs. Command execution errors are logged at warn level and dropped.

**Dispatch architecture [CONTRACT]**: the MQTT message handler itself must never block (a set_band walk
can hold the serial line for > 1 s; blocking the MQTT client's dispatch goroutine deadlocks the client —
this exact class of bug deadlocked a sibling component live). This bridge queues the raw payload on a
**bounded channel of capacity 8** consumed by a single worker goroutine, which serializes execution so
no two band walks interleave on the wire. If the queue is full the command is **dropped** with a
`/cmd queue full, dropping command` warning rather than blocking.

---

## 5. Behavior & state machine [CONTRACT unless noted]

### 5.1 Startup sequence (exact order)

1. Parse flags (`-config`, `-log.level`, `-debug`), load config (§6); config/validate failure → exit
   code 2.
2. Create logger (stderr, text format; levels debug|info|warn|error, default info).
3. Install signal handler: SIGINT/SIGTERM cancel a root context.
4. Build the serial device, bridge state, and command worker (before any I/O).
5. Connect MQTT: LWT registered (`offline`, retained, QoS 1 on `/status`), AutoReconnect + ConnectRetry
   (5 s retry interval) — i.e. the process does not fail on broker outage; it retries forever. **The
   initial connect is context-aware**: a SIGTERM during a broker outage or auth failure aborts the
   connect promptly (see §5.3). Connect failure that is not ctx-cancellation → exit 1.
6. On connect: publish `/status` = `online` (retained, QoS 1), subscribe `/cmd` QoS 1.
7. Enter the serial restart loop.

### 5.2 Serial restart loop

```
loop:
  open port (9600 8N1), reset buffers
  → on error: mark device offline, backoff, retry
  send enable-telemetry (55 92 04 15)   ← open FAILS if this write errors
  mark device_online = true (publish snapshot)
  publish /meta (and legacy discovery if enabled)
  read loop:
    - resync/parse frames (§2.3), ACK each, emit telemetry → bridge dedup → /state
    - >5 s silence: re-send enable every ≥5 s (§2.6)
    - >30 s silence or read error or ctx cancel: exit read loop, close port
  mark device_online = false, power = "off", error = "serial: <cause>" (publish snapshot)
  backoff: wait 2 s, then ×1.5 each failure, capped at 60 s  (reset happens implicitly on next open —
  NOTE: backoff is NOT reset after a successful run; it only stops growing once a run ends; see §9)
  goto loop (unless ctx cancelled)
```

Backoff constants: initial **2 s**, multiplier **1.5×**, max **60 s**. The loop only exits on ctx
cancellation (SIGINT/SIGTERM).

### 5.3 Shutdown behavior (verified against code; the known live issue is FIXED here)

- SIGTERM/SIGINT → root context cancelled.
- If blocked in the initial MQTT connect: the shared ctx-aware connect wrapper aborts the wait,
  disconnects the client, returns `context.Canceled`. **History**: paho's `Connect().Wait()` ignores the
  caller's context, and this bridge was hit live — during a broker outage SIGTERM hung the process until
  systemd SIGKILLed it after `TimeoutStopSec`. The fix routes the wait through a goroutine + `select` on
  ctx.Done. A reimplementation must guarantee that a shutdown signal interrupts *every* blocking wait,
  especially the initial broker connect during an outage.
- If in the serial read loop: a watcher goroutine closes the serial port on ctx cancel, unblocking the
  1 s read; the loop returns ctx.Canceled.
- If in the backoff sleep: the select on ctx.Done fires immediately.
- Any in-flight band walk is not interrupted mid-walk (it completes its remaining ≤ 9 steps × 150 ms
  worst case ≈ 1.35 s, then the loop notices ctx).
- MQTT client is disconnected with a 500 ms quiesce window; because the disconnect is clean, **the
  broker does not publish the LWT** — `/status` keeps its last retained `online` value after a clean
  stop (see §9 fragility).
- Process exit code: **0** when the run ended via context cancellation (clean systemd stop, so
  `systemctl stop` is not a FAILURE); **1** for other run errors (MQTT connect failure not caused by
  shutdown); **2** for config load/validation errors. systemd unit: `Restart=on-failure`,
  `RestartSec=5` — the exit-0-on-SIGTERM convention is what prevents a stop from becoming a restart.

### 5.4 MQTT outage during operation

Paho AutoReconnect handles it transparently [INCIDENTAL]: the bridge keeps reading serial and calling
publish; messages queue in the client and `/status` gets re-published on reconnect. Commands published
while the bridge is disconnected are lost (no `/cmd` retention).

---

## 6. Configuration

TOML file, default path `/etc/acom1200s-pa-bridge/config.toml` (flag `-config <path>`). Missing file is
**not** an error — built-in defaults are used. Unknown TOML keys are silently ignored. Precedence:
defaults → TOML → environment → `-log.level` flag.

| Key | Default (in binary) | Meaning |
|---|---|---|
| `host` | `shari` | compute node name, published in `/meta.host` |
| `serial.port` | `/dev/serial/by-id/usb-Prolific_Technology_Inc._USB-Serial_Controller_D-if00-port0` | serial device path; by-id symlink survives replugs |
| `serial.avg_time_ms` | `1` | forward-power moving-average window in ms; 1 = raw per-frame. **deploy.sh seeds 300 on first install** (see §9) |
| `device.model` | `ACOM 1200S` | published in `/meta.device.model` |
| `device.serial` | `""` (→ derived `acom-1200s`) | stable configured id; the protocol reports no serial number |
| `device.link` | `serial` | transport label in `/meta.link` |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | broker URL |
| `mqtt.client_id` | `""` (→ `muehle-hf-pa`) | MQTT client id; empty derives `<site>-<station>-<slot>` |
| `mqtt.user` | `hf` | MQTT username |
| `mqtt.password` | `""` | **never set in TOML in production** — env override only |
| `mqtt.site` / `mqtt.station` / `mqtt.slot` | `muehle` / `hf` / `pa` | slot address; site+station are mandatory (validate fails otherwise); slot falls back to `pa` if empty |
| `mqtt.location` | `bauwagen` | physical location label in `/meta` |
| `mqtt.discovery_prefix` | `homeassistant` | legacy HA discovery prefix (only used when discovery enabled) |
| `mqtt.publish_ha_discovery` | `false` | gate for legacy embedded HA discovery |
| `log.level` | `info` | `debug` \| `info` \| `warn` \| `error` |

Environment overrides (prefix `ACOM1200S_PA_BRIDGE_`): `MQTT_BROKER`, `MQTT_CLIENT_ID`, `MQTT_USER`,
`MQTT_PASSWORD`, `MQTT_SITE`, `MQTT_STATION`, `MQTT_SLOT`, `SERIAL_PORT`. Empty env values are ignored
(non-empty only).

**Secrets**: the MQTT password lives in `/etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env` (0600, owned
by the service user), loaded by the systemd unit via `EnvironmentFile=` as
`ACOM1200S_PA_BRIDGE_MQTT_PASSWORD="..."`. It never appears in the TOML, the unit file, or the process
command line. CLI flags: `-config` (path), `-log.level` (overrides config), `-debug` (hex-dump all
serial I/O to stderr, diagnostics only).

Validation (exit 2 on failure): `mqtt.site` and `mqtt.station` must be non-empty; `serial.port` must be
non-empty.

---

## 7. Deployment

- Target: Raspberry Pi **shari**, `192.168.1.139`, ssh user `io`. Broker at `192.168.1.50:1883`.
- `./deploy.sh` (from the repo dir): cross-compiles `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`
  `-trimpath -ldflags="-s -w"` into `dist/`, scp's binary + unit + **seed** config/env to the Pi, then
  over ssh:
  - creates system user `acom1200s-pa-bridge` (nologin, no home) if missing; ensures group `dialout`
    exists and adds the user to it;
  - installs udev rule `/etc/udev/rules.d/99-acom1200s-pa-bridge-serial.rules`:
    `SUBSYSTEM=="tty", SUBSYSTEMS=="usb", ATTRS{idVendor}=="067b", GROUP="dialout", MODE="0660"` and
    triggers udev — pins the Prolific adapter's tty to the serial group;
  - **seed-once**: installs `/etc/acom1200s-pa-bridge/config.toml` (0600) and `acom1200s-pa-bridge.env`
    (0600) only if absent — the Pi owns its settings afterwards; redeploys never overwrite them;
  - installs binary to `/opt/acom1200s-pa-bridge/acom1200s-pa-bridge` (755), unit to
    `/etc/systemd/system/acom1200s-pa-bridge.service`, daemon-reload, enable, restart.
- Seed defaults used on *first* deploy differ from in-binary defaults in exactly one place:
  `serial.avg_time_ms = 300` (deploy) vs `1` (binary/config.example).
- systemd unit (hardened, serial-specific):
  - `After/Wants=network-online.target`; `Type=simple`;
    `ExecStart=/opt/acom1200s-pa-bridge/acom1200s-pa-bridge -config /etc/acom1200s-pa-bridge/config.toml`;
    `EnvironmentFile=/etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env`;
  - `User=Group=acom1200s-pa-bridge`, `SupplementaryGroups=dialout`,
    `ConfigurationDirectory=StateDirectory=acom1200s-pa-bridge`;
  - `Restart=on-failure`, `RestartSec=5`; no explicit `TimeoutStopSec` (systemd default 90 s);
  - hardening: `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
    `ProtectKernelTunables/Modules/ControlGroups`, `RestrictAddressFamilies=AF_INET AF_INET6`,
    `RestrictNamespaces`, `LockPersonality`, `RestrictRealtime`, `RestrictSUIDSGID`, `RemoveIPC`,
    empty `CapabilityBoundingSet`/`AmbientCapabilities`, `ReadWritePaths=/var/lib/acom1200s-pa-bridge`;
  - **no `PrivateDevices`** (must open `/dev/ttyUSB*`); instead `DeviceAllow=char-ttyUSB rw`,
    `char-ttyACM rw`, `char-tty rw`; `MemoryMax=256M`, `TasksMax=64` (shari is a shared host — one leaky
    bridge must not OOM the station);
  - logs to journal, `SyslogIdentifier=acom1200s-pa-bridge` → `journalctl -u acom1200s-pa-bridge -f`.
- `./migrate-from-acombridge.sh` (one-time, idempotent, run **before** first deploy): on the Pi, creates
  the new user/config dir, copies `/etc/acombridge/config.toml` → new config, copies
  `/etc/acombridge/acombridge.env` → new env file **rewriting the var prefix `ACOMBRIDGE_` →
  `ACOM1200S_PA_BRIDGE_` on the device** (secret never leaves the Pi), stops+disables+removes the old
  `acombridge` service/unit/install/config/udev rule, best-effort removes the old user. Ordering matters:
  migration first so deploy's seed-once keeps the real config and password.

Dependencies: static Go binary; MQTT broker reachable; ACOM on the Prolific adapter. Go module deps
(paho.mqtt.golang, go.bug.st/serial, BurntSushi/toml) and the shared stationa module
(`replace … => ../shared`) [INCIDENTAL].

---

## 8. Invariants & safety rules

1. **The bridge never commands power.** No `set_power` action, no `power_default`, no host control-line
   (RTS) manipulation. PA power is owned by the `hf/switch` slot. `power` in `/state` is read-only
   telemetry. Any reimplementation must reject a `set_power` command (log + ignore).
2. **Raw firmware strings never leak into canonical fields.** `mode`, `keyed`, `fault` contain only the
   canonical enum values; raw modes live only in `pa_state`, verbatim fault text only in `error`.
3. **Serial writes are serialized** — ACKs, enable, mode and band frames never interleave on the wire;
   band walks are atomic sequences of steps.
4. **Commands never block the MQTT receive path**: bounded queue, drop on overflow, single worker.
5. **`/status` (LWT) tracks only the bridge process**; device link loss is reported exclusively via
   `/state.device_online` (+ `power:"off"`). Consumers must AND the two layers (station-wide rule).
6. **Frequency is never published by this slot** — bus frequency lives on the radio slot as `freq_hz`
   (integer Hz). The amp's kHz value is decoded and discarded.
7. **Exit code 0 on SIGTERM/SIGINT** so systemd stop is clean and does not restart-loop.
8. **A shutdown signal must interrupt every blocking wait** (broker connect during outage, serial read,
   backoff sleep) — no SIGKILL dependency.
9. **set_band is rejected when the current band is unknown** (index 0) rather than walking blind from a
   garbage position.
10. **Power meters read 0 W whenever the amp is not keyed** (keyed != `tx`), so displays can never show
    stale transmit power while receiving.
11. **Meta is republished after every serial reconnect** so late subscribers always see the birth
    certificate; `/cmd` is resubscribed on every MQTT reconnect.

---

## 9. Known defects & fragilities

1. **Stale retained `/status: online` after clean stop.** The LWT fires only on *unclean* death; on
   SIGTERM the client disconnects cleanly and the broker keeps the last retained `online`. A stopped
   bridge still looks "online" to `/status` consumers until they also check `/state` timestamps. (Live
   station behavior; consumers are required to use the two-layer liveness rule.)
2. **Docs lag the code in three places** (code is truth):
   - `docs/pa-mqtt-api.md` and README say `/state` is "published on every telemetry frame" — the code
     dedups idle frames with a 60 s heartbeat (§3.2).
   - README says the bridge "re-enables amplifier telemetry when it detects the amplifier has been
     switched off and back on **(watchdog)**" — the amp-power-cycle watchdog was removed with the
     `set_power` machinery; what remains is the 5 s-silence enable-telemetry re-send inside a live
     session and the 30 s-silence reconnect. CLAUDE.md correctly says "no watchdog".
   - `cmd/main.go`'s package doc still lists `set_power` among dispatched commands — stale comment;
     `set_power` is rejected as unknown.
3. **`avg_time_ms` default mismatch**: in-binary/default/example = 1 (raw), deploy.sh seed = 300 (300 ms
   averaging). A device deployed from scratch silently behaves differently from one run with defaults.
4. **Backoff never resets after a good run.** `serialLoop` scales `backoff` up on every failure but never
   resets it to 2 s after a long successful session — a bridge that ran fine for days still waits up to
   60 s to reopen after the next port loss. (Amplifier power-off via `hf/switch` is a *routine* event,
   so this regularly delays telemetry resumption by tens of seconds.)
5. **Band walk can leave the amp mid-walk.** A serial write error mid-walk aborts with the amp between
   bands; there is no reconciliation (the next telemetry frame reports the actual band, but nothing
   retries the command). Also, nothing prevents a band walk while the amp is transmitting (the amp
   itself may refuse — fault `STOP TRANSMISSION FIRST` — leaving position uncertain).
6. **No command feedback on the bus.** Command success/failure exists only in logs; bus consumers see
   outcomes only via telemetry deltas.
7. **Degenerate first snapshot.** After a serial open but before the first telemetry frame,
   `SetDeviceOnline(true)` publishes a snapshot whose canonical fields are the previous cycle's values
   — or, on the very first cycle, empty strings (`mode`, `keyed`, `fault`, `pa_state` all `""`, `power`
   `""`) with `device_online:true`. Momentarily invalid-ish JSON for consumers that assume non-empty
   enums.
8. **Idle numeric staleness.** Because dedup ignores numeric changes, `temp_c`/`swr` drift is published
   at most every 60 s when idle (and only when some other trigger fires or the heartbeat lands).
9. **Enable-frame checksum asymmetry.** The enable frame (`55 92 04 15`) is sent without an appended
   checksum (the `0x15` payload is not a zeroing checksum — the frame sum is 0x70). Hardcoded quirk of
   the vendor protocol; any "checksum every frame" reimplementation would corrupt the enable command.
10. **Commands lost while bridge disconnected.** `/cmd` is not retained; publishes during an MQTT
    outage are dropped silently.
11. **Dead code**: `Bridge.Reset()` and `Bridge.lastPubTopic` are unused in production code (reset
    semantics after MQTT reconnect are handled by forced-publish flags instead).
12. **Paho token not awaited on publish**: publishes are fire-and-forget into paho's queue; QoS 1
    delivery errors surface only via `IsConnected()` (which the bridge does not actively monitor) — a
    publish during a brief outage is queued, not lost, but errors are invisible in logs.
13. **History (fixed, but must not regress)**: paho `Connect().Wait()` ignoring the context hung this
    bridge's SIGTERM during a broker outage until systemd SIGKILL — the ctx-aware connect (§5.3) is the
    fix; any reimplementation must be tested for "SIGTERM while broker unreachable exits promptly".

---

## 10. Re-implementation notes

**Must be preserved verbatim (observable on the wire / bus):**

- All exact topic strings, retained/QoS choices, LWT topic+payload, `/cmd` subscription semantics.
- The complete `/meta` JSON shape (field names, order-independent content, capabilities facts,
  expose field list) and the `/state` JSON field names, types, units, enum values, and omitempty
  behavior for `band` and `error`.
- The entire serial contract: frame layout, checksum rule, the byte-exact enable/ACK/mode/band frames
  (including the checksum-less enable frame), 72-byte telemetry offsets, all decode tables (mode, band,
  full fault-message table, fault buckets), 9600 8N1, the resync rules, and the 1 s / 5 s / 5 s / 30 s
  timing constants.
- Timing constants observable on the bus: 150 ms band-step cadence, 2 s → 1.5× → 60 s serial backoff,
  60 s idle state heartbeat, 5 s MQTT connect retry.
- Behavior mappings: mode/keyed/power canonicalization, power-meter zeroing when not keyed, temp
  rounding to 0.1 °C, Kelvin-0 special case, SWR /100, band-index-0 navigation rejection, band labels
  incl. bare-number aliases, `/cmd` value-key payload shape, unknown-action and set_power rejection,
  queue-full drop policy, exit-code conventions (0 on signal, 1 on run error, 2 on config error).
- Publish dedup rules (every frame while tx/fault/offline; on enum-field change; 60 s heartbeat) —
  consumers depend on both the flood-avoidance and the retained-freshness behavior.
- The meta-per-serial-reconnect and status-per-MQTT-reconnect cadences.
- Config keys/defaults/env names (operators' config files and the systemd EnvironmentFile must keep
  working), seed-once deploy semantics, and the file layout/permissions on the target.

**Free to change (implementation detail):**

- Go, paho, go.bug.st/serial, TOML library, slog; goroutine/channel architecture (only the observable
  serialization matters); hex-dump debug logging format; the legacy embedded HA discovery (gated off,
  slated for deletion); `Bridge.Reset`/unused fields; log message wording (keep the semantic content);
  backoff reset behavior — arguably an improvement, but changing it alters recovery timing and must be
  a conscious PRD decision; the exact enable-frame construction path (only the 4 bytes on the wire
  matter).
- The 500 ms MQTT disconnect quiesce window.

**Open questions / things the code does not reveal:**

- Why deploy seeds `avg_time_ms = 300` while the example config advertises 1 — which is actually
  running on shari's seeded config is not determinable from the repo (the seed file is device-owned).
- Whether the amplifier ACKs command frames (0x81) back and whether the bridge should react — the code
  ACKs everything inbound but ignores all non-telemetry frames, so a command's success is inferred
  only from telemetry.
- Whether the vendor protocol defines additional inbound frame types (only `0x2F` is consumed; the
  vendor PDF is not in the repo).
- The `0x15` payload byte of the enable frame has no documented meaning in the repo.
- Precise telemetry frame rate is not documented; the dedup design comment says "high rate even in
  OPR/RX".