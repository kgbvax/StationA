# ubctrl MQTT API

This document describes the MQTT interface exposed by **ubctrl** (the UltraBeam
RCU-06 antenna controller) so another application can read antenna status and
send control commands.

It is the authoritative integration contract for the topics, payloads, and
semantics — derived from `internal/mqtt/client.go`.

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, e.g. `tcp://host:1883`) |
| Authentication | Username/password if the broker requires it |
| Clean session | Yes |
| Auto-reconnect | ubctrl reconnects automatically |
| ubctrl client ID | `ubctrl` (configurable via `-mqtt-client-id`) |

> **Use a unique client ID.** Your application **must** connect with a client ID
> different from ubctrl's (`ubctrl`). Two clients sharing one ID will repeatedly
> disconnect each other.

All ubctrl and your app must be connected to **the same broker** for this to work.

---

## 2. Topic conventions

All ubctrl topics are namespaced under a configurable **prefix** (default
`ubctrl`, set via `-mqtt-prefix`). Throughout this document the prefix is shown
as `ubctrl`. If you change the prefix, substitute it everywhere.

```
<prefix>/status/<name>     # published BY ubctrl   (read these)
<prefix>/command/<name>    # consumed BY ubctrl    (publish to these)
```

| Direction | QoS | Retained |
|-----------|-----|----------|
| Status (ubctrl → you) | 0 | **Yes** (last value is retained) |
| Commands (you → ubctrl) | 0 | Publish **non-retained** |

Because status topics are retained, a freshly connected subscriber immediately
receives the current values without waiting for the next change.

---

## 3. Status topics (published by ubctrl)

ubctrl publishes status **only when a value changes** (it de-duplicates
identical snapshots), not on a fixed interval. Do not rely on these as a
periodic heartbeat. A change to any of frequency, band, mode, motion, or
online/error state triggers a republish of all status topics.

### 3.1 `ubctrl/status/frequency`

Primary state. JSON object.

```json
{ "frequency": 21225, "band": "15m", "mode": "forward" }
```

| Field | Type | Description |
|-------|------|-------------|
| `frequency` | integer | Current frequency in **kHz** (1–65535) |
| `band` | string | Band label (see [§5.2](#52-band-labels)) |
| `mode` | string | Beam direction: `forward` \| `reverse` \| `bidirectional` |

### 3.2 `ubctrl/status/motors`

Motor / movement state. JSON object.

```json
{ "moving": false, "motor_bits": 0 }
```

| Field | Type | Description |
|-------|------|-------------|
| `moving` | boolean | `true` while the antenna elements are physically moving |
| `motor_bits` | integer | Raw motor status bitmask from the controller (advanced/diagnostic) |

While `moving` is `true`, the antenna is retuning; avoid issuing new
frequency/band/mode commands until it returns to `false`.

### 3.3 `ubctrl/status/availability`

Plain string (not JSON): `online` or `offline`.

- `online` — ubctrl is communicating with the antenna controller.
- `offline` — the last controller exchange failed (serial error / timeout). See
  `last_error` in [§3.4](#34-ubctrlstatusraw).

> **Caveat — no Last Will (LWT).** ubctrl does not currently register an MQTT
> Last Will. If the ubctrl process or its host dies abruptly, the retained
> `availability` value stays at its last published state (often `online`).
> `offline` is only published when ubctrl itself detects a controller
> communication error while running. If your app needs hard liveness detection,
> treat a stale `updated_at` (see §3.4) as a secondary signal.

### 3.4 `ubctrl/status/raw`

Full state object — superset of the above, useful for diagnostics.

```json
{
  "frequency_khz": 21225,
  "band_name": "15m",
  "band_index": 3,
  "mode_name": "forward",
  "motors_moving": false,
  "motor_bits": 0,
  "updated_at": "2026-06-30T12:29:49.506+02:00",
  "offline": false,
  "last_error": ""
}
```

| Field | Type | Description |
|-------|------|-------------|
| `frequency_khz` | integer | Frequency in kHz |
| `band_name` | string | Band label |
| `band_index` | integer | Raw band index reported by the controller (advanced) |
| `mode_name` | string | `forward` \| `reverse` \| `bidirectional` |
| `motors_moving` | boolean | Same as `status/motors.moving` |
| `motor_bits` | integer | Raw motor bitmask |
| `updated_at` | string (RFC 3339) | Timestamp of the last state refresh |
| `offline` | boolean | `true` when controller comms failed |
| `last_error` | string | Last error message (omitted/empty when none) |

---

## 4. Command topics (consumed by ubctrl)

Publish to these topics to control the antenna. Commands are **fire-and-forget**
— there is no per-command acknowledgement topic. To confirm a command took
effect, watch the relevant `status/...` topic for the resulting change (and
`status/availability` / `last_error` for failures).

### 4.1 `ubctrl/command/frequency`

Set the exact frequency. Payload is the frequency in **kHz** as a decimal
string.

| Payload | Example | Notes |
|---------|---------|-------|
| integer kHz, 1–65535 | `21225` | Mode is preserved (uses the current mode) |

```
publish  ubctrl/command/frequency  "21225"
```

### 4.2 `ubctrl/command/mode`

Set the beam direction. Frequency is preserved.

| Payload | Meaning |
|---------|---------|
| `forward` | Forward (canonical) — aliases: `normal` |
| `reverse` | Reverse (canonical) — aliases: `180` |
| `bidirectional` | Bidirectional (canonical) — aliases: `bidir` |

```
publish  ubctrl/command/mode  "reverse"
```

> Unknown payloads are coerced to `forward`. Prefer the three canonical values.

### 4.3 `ubctrl/command/band`

Jump to the **centre of a band** (IARU Region 1). The current mode is preserved.
Payload is a band label.

| Payload | Tunes to (kHz) |
|---------|----------------|
| `20m` | 14175 |
| `17m` | 18118 |
| `15m` | 21225 |
| `12m` | 24940 |
| `10m` | 28850 |
| `6m`  | 51000 |

```
publish  ubctrl/command/band  "20m"
```

> Any payload not in the table above is ignored (no-op).

### 4.4 `ubctrl/command/retract`

Retract the antenna elements. **Any** payload triggers it (the content is
ignored).

```
publish  ubctrl/command/retract  ""
```

---

## 5. Enumerations

### 5.1 Modes (beam direction)

| Canonical | Aliases accepted on input |
|-----------|---------------------------|
| `forward` | `normal` |
| `reverse` | `180` |
| `bidirectional` | `bidir` |

Status topics always report the **canonical** value.

### 5.2 Band labels

Reported in `status/frequency.band` and `status/raw.band_name`, derived from the
current frequency:

`160m`, `80m`, `40m`, `30m`, `20m`, `17m`, `15m`, `12m`, `10m`, `6m`.

If the frequency falls outside all known amateur bands, the label is
`band-<index>` (e.g. `band-2`). Only `20m`–`6m` are valid inputs for
`command/band`.

---

## 6. Typical interaction flows

**Read current state on startup**
1. Subscribe to `ubctrl/status/#`.
2. The broker immediately delivers the retained `frequency`, `motors`,
   `availability`, and `raw` values.

**Set a frequency and confirm**
1. Publish `ubctrl/command/frequency` = `18118`.
2. Watch `ubctrl/status/motors` → `moving:true` then `moving:false`.
3. Confirm `ubctrl/status/frequency.frequency` == `18118`.

**Change band**
1. Publish `ubctrl/command/band` = `17m`.
2. ubctrl retunes to 18118 kHz (band centre), preserving mode; confirm via
   `status/frequency`.

**Change direction**
1. Publish `ubctrl/command/mode` = `bidirectional`.
2. Confirm via `ubctrl/status/frequency.mode`.

**Detect a fault**
- `ubctrl/status/availability` == `offline`, and
- `ubctrl/status/raw.last_error` contains the error string.

---

## 7. Home Assistant discovery (informational)

ubctrl also publishes Home Assistant MQTT discovery configs (retained, QoS 1)
under the `homeassistant/` prefix, creating a device **"UltraBeam Antenna"**
with these entities. Your application does **not** need these — they use the
same command/status topics documented above — but they are listed for context:

| Discovery topic | HA domain | unique_id | Uses |
|-----------------|-----------|-----------|------|
| `homeassistant/sensor/ubctrl/frequency/config` | sensor | `ubctrl_frequency` | `status/frequency.frequency` |
| `homeassistant/sensor/ubctrl/band/config` | sensor | `ubctrl_band` | `status/frequency.band` |
| `homeassistant/binary_sensor/ubctrl/motors_moving/config` | binary_sensor | `ubctrl_motors_moving` | `status/motors.moving` |
| `homeassistant/number/ubctrl/frequency_set/config` | number | `ubctrl_frequency_set` | cmd `command/frequency` |
| `homeassistant/select/ubctrl/mode/config` | select | `ubctrl_mode` | cmd `command/mode` |
| `homeassistant/select/ubctrl/band/config` | select | `ubctrl_band_set` | cmd `command/band` |
| `homeassistant/button/ubctrl/retract/config` | button | `ubctrl_retract` | cmd `command/retract` |

ubctrl re-publishes discovery whenever Home Assistant announces `online` on
`homeassistant/status`.

---

## 8. Quick reference

**Subscribe (read):**
```
ubctrl/status/frequency        {"frequency":<kHz>,"band":"<band>","mode":"<mode>"}
ubctrl/status/motors           {"moving":<bool>,"motor_bits":<int>}
ubctrl/status/availability      online | offline
ubctrl/status/raw              {full state object}
```

**Publish (control):**
```
ubctrl/command/frequency       "<kHz 1..65535>"
ubctrl/command/mode            "forward" | "reverse" | "bidirectional"
ubctrl/command/band            "20m" | "17m" | "15m" | "12m" | "10m" | "6m"
ubctrl/command/retract         "" (any payload)
```
