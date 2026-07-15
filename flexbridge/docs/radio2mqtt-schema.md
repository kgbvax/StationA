# radio2mqtt MQTT Schema

Reference for the MQTT topics and payloads published by flexbridge. This is
the on-the-wire contract: what a subscriber sees, what every field means, and
what values to expect.

flexbridge implements the `radio` slot of the station integration model
(`station-integration-model.md`). The three planes are:

| Suffix | Retained | Direction | Purpose |
|---|---|---|---|
| `/meta` | yes | bridge → bus | birth certificate: identity + capabilities |
| `/state` | yes | bridge → bus | live radio state (JSON snapshot) |
| `/status` | yes | broker LWT | liveness: `online` / `offline` |
| `/cmd` | no | bus → bridge | intent (not yet implemented) |

---

## Topic addressing

```
<site>/<station>/<slot>/<suffix>
```

Configured via `[mqtt]` in `config.toml`:

```toml
site     = "muehle"   # physical site
station  = "hf"       # transmitting entity
slot     = "radio"    # role (default: radio)
location = "bauwagen" # included in /meta; optional
```

Example for the default Mühle HF station:

```
muehle/hf/radio/meta
muehle/hf/radio/state
muehle/hf/radio/status
```

---

## `/status` — liveness

Plain string, retained, QoS 1. Written by the MQTT client, not the bridge.

| Value | When |
|---|---|
| `online` | published on every (re)connect |
| `offline` | broker Last Will on unclean disconnect; bridge publishes on clean shutdown |

---

## `/meta` — birth certificate

Retained JSON, published once per connect cycle (after handshake). A new
subscriber or a late-joiner gets this immediately without polling.

```json
{
  "schema": "1.0",
  "role": "radio",
  "device": {
    "model":    "FLEX-8400",
    "serial":   "1126-1213-8400-3564",
    "firmware": "3.8.19"
  },
  "link":     "ethernet",
  "location": "bauwagen",
  "capabilities": {
    "bands":     ["160m","80m","60m","40m","30m","20m","17m","15m","12m","10m","6m"],
    "modes":     ["cw","usb","lsb","am","fm","data"],
    "receivers": 1,
    "diversity": false,
    "amp_key":   true,
    "tune":      true,
    "bias_t":    false,
    "rx_inputs": ["ant1","ant2","rx_a"],
    "tx_outputs":["ant1","ant2"]
  },
  "expose": {
    "device": {
      "name": "FlexRadio 8400",
      "model": "FLEX-8400",
      "manufacturer": "FlexRadio Systems",
      "sw_version": "3.8.19",
      "area": "bauwagen"
    },
    "fields": [
      { "key": "freq_hz", "name": "Frequency", "type": "number", "unit": "Hz", "class": "frequency", "state_class": "measurement" },
      { "key": "band",    "name": "Band",      "type": "enum", "options_ref": "bands" },
      { "key": "mode",    "name": "Mode",      "type": "enum", "options_ref": "modes" },
      { "key": "drive",   "name": "Drive",     "type": "number", "unit": "%" },
      { "key": "tx",      "name": "Transmitting", "type": "boolean", "on": "tx", "off": "rx" },
      { "key": "tuning",  "name": "Tuning",    "type": "boolean" }
    ]
  }
}
```

### The `expose` block — consumer-neutral field surface

`expose` (integration model §3.1, Appendix C) is the **consumer-neutral** description of
this slot's observable field surface. It carries **no consumer vocabulary** — no
`device_class` strings, no Jinja templates, no `payload_on/off`. Consumers (the standalone
`hadiscovery` service for Home Assistant, and any future historian/dashboard/Prometheus
consumer) render their own representation from these neutral primitives.

flexbridge is **read-only**, so none of its fields are `writable` and it exposes no
`actions` — every field renders as a sensor or binary_sensor. The `band`/`mode` enum
options are not inlined; they resolve via `options_ref` against `capabilities.bands` /
`capabilities.modes` above (single source of truth). The `tx` boolean carries `on`/`off`
because `/state` holds the strings `"tx"`/`"rx"`, not a bool; `tuning` omits them because
`/state` holds a real bool.

The HA-specific rendering (component choice, `value_template`, unit→`device_class` map,
availability, device block) lives in `hadiscovery`, not in this bridge.

### `/meta` field reference

| Field | Type | Notes |
|---|---|---|
| `schema` | string | Schema version (`"1.0"`) |
| `role` | string | Always `"radio"` |
| `device.model` | string | Radio model from `info` command |
| `device.serial` | string | Chassis serial |
| `device.firmware` | string | Firmware version; omitted if not reported |
| `link` | string | Always `"ethernet"` |
| `location` | string | From config; omitted if unset |
| `capabilities.bands` | string[] | Canonical band names this radio covers |
| `capabilities.modes` | string[] | Canonical modes this radio supports |
| `capabilities.receivers` | int | Number of independent receive paths |
| `capabilities.diversity` | bool | Diversity receive available |
| `capabilities.amp_key` | bool | Hardware amplifier key output present |
| `capabilities.tune` | bool | Internal or external tuner present |
| `capabilities.bias_t` | bool | Bias-T for masthead preamp |
| `capabilities.rx_inputs` | string[] | Named RX antenna inputs |
| `capabilities.tx_outputs` | string[] | Named TX antenna outputs |

---

## `/state` — live radio state

Retained JSON snapshot, QoS 1. Published on every state change. The document
is always complete — no partial updates — so a subscriber always holds a
consistent view.

`/state` represents the **active TX receiver**: the slice or VFO currently
selected for transmit (or the single active receiver on a single-receiver
radio). Downstream devices (PA, antenna switch) always read from this topic
regardless of how many internal receivers the radio has.

```json
{
  "ts":     "2026-07-06T12:34:56Z",
  "freq_hz": 14025000,
  "band":   "20m",
  "mode":   "cw",
  "tx":     "rx",
  "tuning": false,
  "drive":  40
}
```

### `/state` field reference

| Field | Type | Unit | Notes |
|---|---|---|---|
| `ts` | string | — | RFC 3339 UTC timestamp of this publish |
| `freq_hz` | integer | Hz | VFO frequency of the active receiver |
| `band` | string | — | Canonical band name derived from `freq_hz`; see band table |
| `mode` | string | — | Canonical mode; see mode table |
| `tx` | string | — | `"tx"` while transmitting, `"rx"` otherwise |
| `tuning` | bool | — | `true` while the ATU or radio is actively tuning |
| `drive` | integer | % | Transmit drive level, 0–100 |

### Mode values

All modes are normalized from firmware strings to the canonical vocabulary
before publishing. Adapters are responsible for this normalization; consumers
always see the canonical form.

| Canonical | Firmware strings mapped here |
|---|---|
| `cw` | `CW`, `CW-U`, `CW-L` |
| `usb` | `USB` |
| `lsb` | `LSB` |
| `am` | `AM`, `SAM` |
| `fm` | `FM`, `NFM`, `DFM` |
| `data` | `DIGU`, `DIGL`, `DATA-U`, `DATA-L`, `FDV`, `FDVU`, `FDVL`, `RTTY-U`, `RTTY-L`, `PKTUSB`, `PKTLSB`, `DSTR` |

Unknown firmware modes are lower-cased and passed through.

### Band values

Derived from `freq_hz` at publish time (IARU Region 1 / DL allocations).

| Band | Low (Hz) | High (Hz) |
|---|---|---|
| `160m` | 1,800,000 | 2,000,000 |
| `80m` | 3,500,000 | 4,000,000 |
| `60m` | 5,060,000 | 5,450,000 |
| `40m` | 7,000,000 | 7,300,000 |
| `30m` | 10,000,000 | 10,200,000 |
| `20m` | 14,000,000 | 14,400,000 |
| `17m` | 18,000,000 | 18,200,000 |
| `15m` | 21,000,000 | 21,500,000 |
| `12m` | 24,890,000 | 25,000,000 |
| `10m` | 28,000,000 | 30,000,000 |
| `6m` | 50,000,000 | 54,000,000 |
| `4m` | 69,000,000 | 71,000,000 |
| `2m` | 144,000,000 | 148,000,000 |
| `1.25m` | 222,000,000 | 225,000,000 |
| `70cm` | 430,000,000 | 440,000,000 |
| `33cm` | 902,000,000 | 928,000,000 |
| `23cm` | 1,240,000,000 | 1,300,000,000 |
| `gen` | 1,800,000 | 30,000,000 (general HF, outside ham allocations) |
| `unknown` | — | anything else, or frequency zero |

---

## Home Assistant auto-discovery

> **Two paths now exist (integration model §9).** The preferred path is the standalone
> `hadiscovery` consumer, which reads this bridge's `expose` block from `/meta` and renders
> HA discovery centrally. The legacy **embedded** discovery below is retained for
> reversibility but is **gated off** by `[mqtt] publish_ha_discovery = false` (the default).
> Set it `true` only to fall back during migration. Once `hadiscovery` is proven, the
> embedded discovery code will be deleted.
>
> The two paths use **different node IDs**: the embedded path uses `flexradio-<serial>`
> (below); `hadiscovery` uses `muehle-hf-radio` (the sanitized slot address). So switching
> from embedded to `hadiscovery` moves entities to a new device in HA — the old
> `flexradio-*` entities must be removed (clear their discovery topics) to avoid duplicates.

### Embedded discovery (legacy, gated, default off)

flexbridge publishes MQTT discovery payloads under `homeassistant/` (the
`discovery_prefix` in config). HA picks these up automatically; no manual
entity configuration is required.

Discovery node ID: `flexradio-<sanitized-serial>` (lower-cased, non-alphanumeric
characters replaced with `_`).

All entities read from the single `/state` topic using `value_template`.

| Entity | Component | Object ID | Template | Unit |
|---|---|---|---|---|
| Frequency | `sensor` | `frequency` | `{{ value_json.freq_hz }}` | Hz |
| Band | `sensor` | `band` | `{{ value_json.band }}` | — |
| Mode | `sensor` | `mode` | `{{ value_json.mode }}` | — |
| Drive | `sensor` | `drive` | `{{ value_json.drive }}` | % |
| Transmitting | `binary_sensor` | `transmitting` | `{{ value_json.tx }}` | payload on: `tx` / off: `rx` |
| Tuning | `binary_sensor` | `tuning` | `{{ value_json.tuning \| lower }}` | payload on: `true` / off: `false` |

Discovery config topics follow the pattern:

```
<discovery_prefix>/<component>/flexradio-<serial>/<object_id>/config
```

### Standalone discovery via `hadiscovery` (preferred)

With `publish_ha_discovery = false` (default), flexbridge publishes only the `expose`
block in `/meta`. The `hadiscovery` service subscribes to `muehle/+/+/meta`, reads
`expose`, and renders HA discovery under node ID `muehle-hf-radio`:

```
homeassistant/<component>/muehle-hf-radio/<object_id>/config
```

The entity set is the same six fields (frequency sensor, band sensor, mode sensor, drive
sensor, transmitting binary_sensor, tuning binary_sensor), but rendered by `hadiscovery`
from the neutral `expose` — flexbridge itself contains no HA knowledge. See
`../stationa/hadiscovery/docs/discovery-mqtt-api.md` for the mapping.

---

## Multi-receiver radios

A radio that declares `receivers > 1` additionally publishes per-receiver
state at:

```
<site>/<station>/<slot>/receiver/<N>/state
```

with the same JSON shape as `/state`. The top-level `/state` still represents
the active TX receiver; the per-receiver sub-topics are for logging and
monitoring consumers only. No core station component (PA, antenna switch)
binds to sub-receiver topics.

The FLEX-8400 with a single active slice declares `receivers: 1` and publishes
only `/state`. If multiple slices are configured in SmartSDR, the TX slice
(or the active slice if none is transmitting) drives `/state`.

---

## Publish trigger summary

| Event | Topic updated |
|---|---|
| Connect / reconnect | `/meta` (incl. `expose`), `/status` (`online`); HA discovery only if `publish_ha_discovery = true` |
| Active/TX slice frequency or mode changes | `/state` |
| Interlock transitions TX ↔ RX | `/state` |
| ATU transitions into or out of Tuning | `/state` |
| Radio drive level changes | `/state` |
| Radio `tuning` flag changes | `/state` |
| Disconnect / crash | `/status` → `offline` (broker LWT) |

State is published only when a field actually changes value; unchanged state
does not produce a publish.
