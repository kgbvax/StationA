# pa-mqtt-api — ACOM 1200S PA bridge MQTT schema

Reference for the MQTT topics and payloads published by **acombridge**. This is
the on-the-wire contract: what a subscriber sees, what every field means, and
what values to expect.

acombridge implements the `pa` slot of the station integration model
(`../stationa/docs/station-integration-model.md` §7.1). It bridges an ACOM
600S/1200S linear amplifier to MQTT over a USB-serial adapter (Prolific,
9600 8N1). The four planes are:

| Suffix | Retained | Direction | Purpose |
|---|---|---|---|
| `/meta` | yes | bridge → bus | birth certificate: identity + capabilities + `expose` |
| `/state` | yes | bridge → bus | live PA state (single JSON snapshot) |
| `/status` | yes | broker LWT | liveness: `online` / `offline` (the *bridge*) |
| `/cmd` | no | bus → bridge | desired state: `set_mode`, `set_band` |

---

## Topic addressing

```
<site>/<station>/<slot>/<suffix>
```

Configured via `[mqtt]` in `config.toml`:

```toml
site     = "muehle"    # physical site
station  = "hf"        # transmitting entity
slot     = "pa"        # role (default: pa)
location = "bauwagen"  # included in /meta
```

Example for the default Mühle HF station:

```
muehle/hf/pa/meta
muehle/hf/pa/state
muehle/hf/pa/status
muehle/hf/pa/cmd
```

---

## `/status` — liveness

Plain string, retained, QoS 1. Written by the MQTT client LWT, not the bridge.

| Value | When |
|---|---|
| `online` | published on every (re)connect |
| `offline` | broker Last Will on unclean disconnect |

`/status` reflects the **bridge** process, not the amplifier. When the serial
port is lost (30 s silence / read error) the bridge stays up: `/status` stays
`online` while `/state.device_online` goes `false` (see model §3). This lets a
dashboard distinguish "bridge crashed" from "amplifier unplugged / powered off".

---

## `/meta` — birth certificate

Retained JSON, published once per connect cycle (after the serial port opens).

```json
{
  "schema": "1.0",
  "role": "pa",
  "device": {
    "model": "ACOM 1200S",
    "serial": "acom-1200s"
  },
  "link": "serial",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "bands":       ["160m","80m","40m","30m","20m","17m","15m","12m","10m","6m"],
    "max_power_w": 1200,
    "band_source": "rf_sense",
    "rf_sample":   false,
    "key_input":   "hardware",
    "alc_out":     true,
    "modes":       ["operate","standby"]
  },
  "expose": {
    "device": {
      "name": "ACOM 1200S",
      "model": "ACOM 1200S",
      "manufacturer": "ACOM",
      "area": "bauwagen"
    },
    "fields": [
      { "key": "mode",          "name": "Mode",            "type": "enum",   "options_ref": "modes", "writable": true,
        "command": { "action": "set_mode", "value_key": "value", "value_type": "string" } },
      { "key": "band",          "name": "Band",            "type": "enum",   "options_ref": "bands", "writable": true,
        "command": { "action": "set_band", "value_key": "value", "value_type": "string" } },
      { "key": "keyed",         "name": "Keyed",           "type": "enum",   "options": ["rx","tx","inhibited"] },
      { "key": "fwd_power_w",   "name": "Forward Power",   "type": "number", "unit": "W",  "class": "power",       "state_class": "measurement" },
      { "key": "rfl_power_w",   "name": "Reflected Power", "type": "number", "unit": "W",  "class": "power",       "state_class": "measurement" },
      { "key": "temp_c",        "name": "Temperature",     "type": "number", "unit": "°C", "class": "temperature", "state_class": "measurement" },
      { "key": "swr",           "name": "SWR",             "type": "number", "state_class": "measurement" },
      { "key": "fault",         "name": "Fault",           "type": "enum",   "options": ["none","swr","temp","reflected","other"] },
      { "key": "pa_state",      "name": "PA State",        "type": "string" },
      { "key": "device_online", "name": "Device Online",   "type": "boolean" }
    ]
  }
}
```

### The `expose` block — consumer-neutral field surface

`expose` (integration model §3.1, Appendix C) is the **consumer-neutral**
description of this slot's field surface. It carries **no consumer vocabulary**
— no `device_class` strings, no Jinja templates, no `payload_on/off`.
Consumers (the standalone `hadiscovery` service for Home Assistant, and any
future historian/dashboard) render their own representation from these neutral
primitives.

`mode` and `band` are `writable` with a `command` descriptor: a consumer
renders the setpoint and emits the `/cmd` action JSON itself. The `mode`/`band`
enum options resolve via `options_ref` against `capabilities.modes` /
`capabilities.bands` (single source of truth). `pa_state` is a raw diagnostic
string (the firmware mode) kept for dashboards; it is **not** part of the
canonical model — `mode`/`keyed`/`fault` are.

### `/meta` field reference

| Field | Type | Notes |
|---|---|---|
| `schema` | string | Schema version (`"1.0"`) |
| `role` | string | Always `"pa"` |
| `device.model` | string | From config (`device.model`, default `ACOM 1200S`) |
| `device.serial` | string | Configured id; the ACOM protocol reports no serial, so default `acom-1200s` |
| `link` | string | `serial` (the deployed USB-serial transport) |
| `location` / `host` | string | From config — deployment facts |
| `capabilities.bands` | string[] | The amp's own 10 bands (no 60m — the ACOM 1200S has no 60m band) |
| `capabilities.max_power_w` | int | 1200 |
| `capabilities.band_source` | string | `rf_sense` — the amp auto-bands by sensing the RF drive; this serial adapter has no CAT band-data cable |
| `capabilities.rf_sample` | bool | `false` — no independent RF sampling |
| `capabilities.key_input` | string | `hardware` — keyed by a hardware key line, not MQTT |
| `capabilities.alc_out` | bool | `true` — ALC output to the radio |
| `capabilities.modes` | string[] | `["operate","standby"]` |

---

## `/state` — live PA state

Retained JSON snapshot, QoS 1, RFC 3339 UTC `ts`. Published **on every telemetry
frame** — PA telemetry is a continuous stream and the snapshot is the live
state (model §8). The document is always complete.

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
  "device_online": true,
  "error":         ""
}
```

### `/state` field reference

| Field | Type | Unit | Notes |
|---|---|---|---|
| `ts` | string | — | RFC 3339 UTC timestamp of this publish |
| `mode` | string | — | canonical: `operate` \| `standby` (see mapping) |
| `band` | string | — | canonical band label from the amp's band byte |
| `keyed` | string | — | canonical: `rx` \| `tx` \| `inhibited` |
| `fwd_power_w` | uint16 | W | forward power, window-averaged over `serial.avg_time_ms` |
| `rfl_power_w` | uint16 | W | reflected power |
| `temp_c` | float | °C | heatsink temperature (Kelvin → Celsius) |
| `swr` | float | ratio | raw SWR / 100 |
| `fault` | string | — | canonical: `none` \| `swr` \| `temp` \| `reflected` \| `other` |
| `pa_state` | string | — | raw firmware mode (diagnostic): `OPR/RX`, `OPR/TX`, `STANDBY`, `OFF`, … |
| `device_online` | bool | — | `true` while the serial loop has data; `false` when the port is lost |
| `error` | string | — | verbatim fault message when `fault != none`; empty otherwise |

The raw amplifier frequency (kHz) is **not** published — `band` is the PA's
band, and per `band-mode-reference.md` frequency on the bus is always Hz as an
integer (the radio slot owns `freq_hz`; the PA slot does not).

### Firmware → canonical mapping

| firmware mode (`pa_state`) | `mode` | `keyed` |
|---|---|---|
| `OPR/RX` | `operate` | `rx` |
| `OPR/TX` | `operate` | `tx` |
| `STANDBY` / `OFF` / `ATAC` / `RESET` / `INIT` / `DEBUG` / `SERVICE` / `MENU` / `UNKNOWN` | `standby` | `inhibited` |

`bypass` is never produced — the ACOM protocol has no bypass state.

| fault byte family | `fault` |
|---|---|
| `0xFF` (no fault) | `none` |
| temperature codes (`0x18`–`0x1F`, `0x32`,`0x33`,`0x38`,`0x62`,`0x63`,`0x65`,`0xAE`,`0xAF`) | `temp` |
| SWR codes (`0x0D`,`0x39`,`0xAC`,`0xAD`) | `swr` |
| reflected codes (`0x04`,`0x05`,`0xA8`,`0xA9`) | `reflected` |
| everything else (relays, HV, bias, fans, PSU, comms, CAT, EEPROM, ATU DC…) | `other` |

The precise verbatim message for any non-`none` fault is preserved in `error`.

---

## `/cmd` — desired state

**Not retained**, QoS 1. Published by external systems (antenna reconciler,
HA, operator). The bridge subscribes on every (re)connect and dispatches to the
amplifier over serial.

### Command payloads

**set_mode** — set the amplifier operating mode:
```json
{"action": "set_mode", "value": "operate"}
{"action": "set_mode", "value": "standby"}
```
`operate` sends the OPR/RX mode byte; `standby` sends the standby byte. Other
values are rejected. Keying itself is hardware-driven (`key_input: hardware`) —
there is no `set_keyed` command.

**set_band** — walk the amplifier to a band:
```json
{"action": "set_band", "value": "20m"}
```
The ACOM band-change command is **relative** (next/prev only), so the bridge
walks from the current band to the target in 150 ms-spaced steps. The target
must be one of `capabilities.bands`. If the current band is unknown (no
telemetry yet), the command is rejected. Band follows the radio via CAT in
normal operation; this command is for manual override.

Unknown actions are logged and ignored. There is no `clear_fault` command —
the ACOM serial protocol as implemented exposes no fault-clear TX command.

---

## Home Assistant discovery

> **Two paths exist (integration model §9).** The preferred path is the
> standalone `hadiscovery` consumer, which reads this bridge's `expose` block
> from `/meta` and renders HA discovery centrally. The legacy **embedded**
> discovery is retained for reversibility but is **gated off** by
> `[mqtt] publish_ha_discovery = false` (the default). Set it `true` only to
> fall back during migration. Once `hadiscovery` is proven, the embedded
> discovery code will be deleted.
>
> The two paths use **different node IDs**: the embedded path uses
> `acom-<serial>` (below); `hadiscovery` uses `muehle-hf-pa` (the sanitized
> slot address). So switching from embedded to `hadiscovery` moves entities to
> a new device in HA — the old `acom-*` entities must be removed (clear their
> discovery topics) to avoid duplicates.

### Embedded discovery (legacy, gated, default off)

When `publish_ha_discovery = true`, acombridge publishes discovery configs under
`homeassistant/` (the `discovery_prefix` in config). Discovery node ID:
`acom-<sanitized-serial>`.

| Entity | Component | Object ID | `value_template` | Notes |
|---|---|---|---|---|
| Mode | `select` | `mode` | `{{ value_json.mode }}` | options operate/standby; `command_template` wraps into `set_mode` JSON |
| Band | `select` | `band` | `{{ value_json.band }}` | options = capabilities.bands; `command_template` wraps into `set_band` JSON |
| Forward Power | `sensor` | `fwd_power_w` | `{{ value_json.fwd_power_w }}` | W, power, measurement |
| Reflected Power | `sensor` | `rfl_power_w` | `{{ value_json.rfl_power_w }}` | W, power, measurement |
| Temperature | `sensor` | `temp_c` | `{{ value_json.temp_c }}` | °C, temperature, measurement |
| SWR | `sensor` | `swr` | `{{ value_json.swr }}` | measurement |
| Keyed | `sensor` | `keyed` | `{{ value_json.keyed }}` | — |
| Fault | `sensor` | `fault` | `{{ value_json.fault }}` | — |
| PA State | `sensor` | `pa_state` | `{{ value_json.pa_state }}` | diagnostic |
| Device Online | `binary_sensor` | `device_online` | `{{ value_json.device_online }}` | on `true` / off `false` |

The `select` entities use a `command_template` of
`{"action":"set_mode","value":"{{ value }}"}` / `{"action":"set_band",...}`
so HA's selected option is published to `/cmd` in the JSON shape the bridge
expects.

### Standalone discovery via `hadiscovery` (preferred)

With `publish_ha_discovery = false` (default), acombridge publishes only the
`expose` block in `/meta`. The `hadiscovery` service subscribes to
`muehle/+/+/meta`, reads `expose`, and renders HA discovery under node ID
`muehle-hf-pa`:

```
homeassistant/<component>/muehle-hf-pa/<object_id>/config
```

The bridge itself contains no HA knowledge in this path. See
`../stationa/hadiscovery/docs/discovery-mqtt-api.md` for the neutral→HA mapping.

---

## Publish trigger summary

| Event | Topic updated |
|---|---|
| Connect / reconnect | `/status` (`online`); `/meta` (incl. `expose`); embedded HA discovery only if `publish_ha_discovery = true` |
| Each telemetry frame | `/state` (full snapshot) |
| Serial port lost (30 s silence / read error) | `/state` with `device_online:false`, `error:"..."`; `/status` stays `online` |
| Amp power-cycles back (mode `OFF` → live) | watchdog re-arms telemetry; `/state` resumes |
| Disconnect / crash | `/status` → `offline` (broker LWT) |