# hadiscovery — MQTT API

hadiscovery is the **central Home Assistant discovery consumer** for the station bus
(integration model §3.1, §9). It is a *passive consumer*: it subscribes to slot `/meta`
announcements, reads each slot's consumer-neutral `expose` block, and renders HA MQTT
discovery. It owns no device, opens no serial port, serves no HTTP, and writes no `/cmd`.

This document describes:

1. hadiscovery's own bus presence (its `/meta` and `/status`).
2. the neutral→HA render mapping (HA-specific; the **neutral** `expose` schema itself is
   defined in `../docs/station-integration-model.md` §3.1 / Appendix C).
3. the discovery topic layout, rebirth, and removal behavior.

The bridges contain **zero HA knowledge**. HA is one renderer of the same neutral surface
that InfluxDB, Node-RED, a dashboard, or Prometheus could equally consume. `meta` is the
single source of truth.

---

## 1. hadiscovery's own bus presence

hadiscovery occupies a **logic slot**. Default address `muehle/hf/discovery`
(configurable via `[mqtt] site/station/slot`).

| Topic | Retained | Payload | Meaning |
|---|---|---|---|
| `muehle/hf/discovery/meta` | yes | JSON birth certificate (below) | announces the consumer |
| `muehle/hf/discovery/status` | yes | `online` / `offline` | LWT; `offline` is the broker-stored will |

Its `/meta` (role `discovery`, link `none`, **no** `device` block):

```json
{
  "schema": "1.0",
  "role": "discovery",
  "link": "none",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "renders": ["sensor", "binary_sensor", "number", "select", "button"],
    "filter": "muehle/+/+/meta"
  }
}
```

`capabilities.renders` is diagnostic — the HA component kinds this consumer can emit.
`capabilities.filter` is the meta subscription it watches.

hadiscovery does **not** publish an `expose` block (it has no field surface to expose).
It is therefore itself undiscoverable; it appears in HA only as a device if a future
diagnostic publisher renders it, not via its own `expose`.

---

## 2. The neutral→HA render mapping

hadiscovery reads each slot's `expose.fields[]` and `expose.actions[]` and renders HA
discovery. This is the **only** place HA-specific knowledge lives. It is a small, finite,
deterministic mapper (~5 type branches) from neutral primitives to HA components.

### 2.1 Field → HA component

| neutral field | → HA component | notes |
|---|---|---|
| `number`, not `writable` | `sensor` | `state_topic=<addr>/state`, `value_template={{ value_json.<key> }}`, unit→`device_class`, `state_class` carried through |
| `number`, `writable` | `number` | + `command_topic=<addr>/cmd`, `command_template` from `command`, `min`/`max`/`step`, `mode: box`, `retain: true` (a HA `number` both displays state and commands) |
| `enum`, not `writable` | `sensor` | `value_template` (options are informational; not emitted on a sensor) |
| `enum`, `writable` | `select` | + `command_topic`, `options` (resolve `options_ref` against `capabilities`, else inline `options`), `command_template` from `command`, `retain: true`. **Skipped** if `writable` but no `command`. |
| `boolean` | `binary_sensor` | see §2.3 |
| `string` | `sensor` | `value_template` only; no unit/class |
| `action` (in `actions[]`) | `button` | `command_topic`, `payload_press` = static JSON of `command` |

**Normalization:** a HA `number`/`select` both reads state from `state_topic` **and**
commands via `command_topic`, so a slot does **not** need separate read/write entities.
This is why ultrabridge's 8 embedded entities collapse to 5 under hadiscovery
(see `../ultrabridge/docs/ultrabeam-mqtt-api.md`).

### 2.2 Shared envelope (injected on every entity)

Every entity hadiscovery renders gets, from the slot context (not from `expose`):

- `state_topic` = `<addr>/state` (for state-reading components)
- `command_topic` = `<addr>/cmd` (for writable components / buttons)
- `availability` = `[{ "topic": "<addr>/status", "payload_available": "online", "payload_not_available": "offline" }]`
- `availability_mode` = `"all"`
- `unique_id` = `<nodeID>_<objectID>`, where `nodeID = sanitize("<site>-<station>-<slot>")` (e.g. `muehle-hf-radio`) and `objectID = sanitize(field.key)`
- `device` block (see §2.4)
- `origin` = `{ "name": "hadiscovery", "sw_version": <build> }`

### 2.3 boolean rendering

- If the field carries `on`/`off` (the state holds **string** payloads, e.g. `tx`/`rx`):
  - `value_template` = `{{ value_json.<key> }}` (pass-through)
  - `payload_on` = the `on` value, `payload_off` = the `off` value (each defaults to `ON`/`OFF` if only one is given).
- Else (the state holds a real bool):
  - `value_template` = `{{ 'ON' if value_json.<key> else 'OFF' }}`
  - `payload_on` = `ON`, `payload_off` = `OFF`

### 2.4 device block

From `expose.device`, falling back to `meta.device`:

| HA key | source |
|---|---|
| `identifiers` | `[<nodeID>]` — one HA device per slot |
| `name` | `expose.device.name`, else `expose.device.model`, else `<role> <addr>` |
| `model` | `expose.device.model`, else `meta.device.model` |
| `manufacturer` | `expose.device.manufacturer` |
| `sw_version` | `expose.device.sw_version`, else `meta.device.firmware` |
| `suggested_area` | `expose.device.area`, else the deployment-wide default (see below) |

`suggested_area` is HA's device-level area hint: it places the device (and all its
entities) into an HA area when the device is first created. HA will **not** override a
manual area assignment made in the UI, so this is a suggestion, not a hard setting. The
value is `expose.device.area` when the slot names one; otherwise hadiscovery fills in a
**deployment-wide default area** so every discovered device lands in a sensible place
without each bridge having to set it. The default is configured by `area` in
`config.toml` (default `"Bauwagen"`); set it to `""` to emit no `suggested_area` at all.

Logic slots may omit `expose.device` entirely; they then get a device named
`<role> <addr>` with just the identifier — and the deployment default area, since they
name no area of their own.

### 2.5 command rendering

The `command` descriptor is structured (never a consumer template string in `meta`).
hadiscovery owns the only structured→template step:

| `command` shape | `command_template` (writable) / `payload_press` (button) |
|---|---|
| `action` + `value_key` | `{"action":"<action>","<value_key>":<placeholder>}` |
| `value_key` only | `{"<value_key>":<placeholder>}` |
| `action` only (button) | `{"action":"<action>"}` |

`value_type` → placeholder (Jinja coercion of the user-supplied value):

| `value_type` | placeholder |
|---|---|
| `int` | `{{ value | int }}` |
| `float` | `{{ value | float }}` |
| `string` / unset | `"{{ value }}"` |

Examples:

- radio mode (enum writable): `{"action":"mode","value":"{{ value }}"}`
- radio/ctrl freq (number writable, int): `{"action":"frequency","freq_hz":{{ value | int }}}`
- ant-switch select (enum writable, no action): `{"select":"{{ value }}"}`
- ultrabeam retract (button): `{"action":"retract"}`

### 2.6 unit → device_class

A field's explicit `class` (a generic semantic hint) wins; otherwise the unit is mapped:

| unit | HA `device_class` |
|---|---|
| `Hz` | `frequency` |
| `°C`, `degC` | `temperature` |
| `W`, `Watts` | `power` |
| `V`, `Volts` | `voltage` |
| `A`, `Amps` | `current` |
| `dBm` | `signal_strength` |
| (other / none) | *(omitted)* |

This mirrors the map that previously lived in `flexbridge/internal/ha/deviceClassFor` — it
is now centralized here, in the single renderer.

---

## 3. Discovery topic layout

Standard HA MQTT discovery:

```
<discovery_prefix>/<component>/<nodeID>/<objectID>/config
```

- `discovery_prefix` defaults to `homeassistant` (HA's default; configurable).
- `component` ∈ `sensor`, `binary_sensor`, `number`, `select`, `button`.
- `nodeID` = `sanitize("<site>-<station>-<slot>")` (e.g. `muehle-hf-radio`).
- `objectID` = `sanitize(field.key)` or `sanitize(action.key)`.

Example:

```
homeassistant/sensor/muehle-hf-radio/freq_hz/config
homeassistant/select/muehle-hf-radio/mode/config
homeassistant/binary_sensor/muehle-hf-radio/tx/config
homeassistant/button/muehle-hf-ant-ctrl/retract/config
```

`sanitize` lowercases and replaces any char outside `[a-z0-9_-]` with `_`.

---

## 4. Rebirth (HA restart)

hadiscovery subscribes to `homeassistant/status`. When Home Assistant (re)starts it
publishes `online` there; hadiscovery then **re-publishes every known slot's discovery
config** (retained) so HA's registry is repopulated. Any non-`online` payload is ignored.

This fixes the drift in the legacy embedded discovery (flexbridge never re-published on HA
restart; ubctrl did). A single central consumer makes rebirth uniform across all slots.

---

## 5. Removal and idempotency

- **Slot decommissioned:** a zero-length payload on a slot's `/meta` (retained clear) ⇒
  hadiscovery publishes an **empty** retained payload to each of that slot's discovery
  config topics, which makes HA remove the entities, then forgets the slot.
- **Field/action removed:** when a slot's `expose` shrinks (re-delivered meta with fewer
  fields), the dropped entities' config topics are cleared (empty retained) and the
  remaining ones re-published.
- **Idempotent:** a byte-identical re-delivery of a retained `/meta` is a **no-op**
  (no churn on the bus). hadiscovery stores the rendered entity set per slot and compares.
- **No expose block:** a slot that publishes `/meta` without `expose` gets a single
  `binary_sensor` diagnostic entity (object id `online`) reading `<addr>/status`, tagged
  `entity_category: diagnostic`, so it is at least visible in HA as a liveness device.

---

## 6. Slots with no expose

Slots are not required to publish `expose`. A slot without it is simply not discovered for
state/control — but hadiscovery still emits the one diagnostic liveness sensor above so
the slot shows up as a device in HA. Once a slot adds an `expose` block, the diagnostic is
replaced by the real entities (the diagnostic topic is cleared).

---

## 7. Live smoke test (pure MQTT, no hardware)

```bash
# from hadiscovery/
go run ./cmd/hadiscovery -config ./config.example.toml   # broker tcp://192.168.1.50:1883

# watch the discovery tree:
mosquitto_sub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'homeassistant/#' -v

# confirm a specific slot:
mosquitto_sub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'homeassistant/sensor/muehle-hf-radio/freq_hz/config' -v

# trigger HA rebirth, confirm republish in hadiscovery logs:
mosquitto_pub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'homeassistant/status' -m 'online' -r
```

---

## 8. Why a central consumer, not embedded discovery

(integration model §9.) The embedded HA discovery in flexbridge/ubctrl was a temporary,
config-gated deviation and had already **drifted** (different nodeIDs, inconsistent
rebirth). The neutral `expose` block + a single passive consumer means:

- **one** surface (`expose` in `meta`), reusable by non-HA consumers;
- **one** renderer of HA knowledge (this package), not N copies across bridges;
- adding a component or field needs **no consumer edit** — publish `expose`, done.

Bridges gate their embedded discovery off with `[mqtt] publish_ha_discovery = false`
(default `false`) once hadiscovery is running; deletion of the embedded code is a
follow-up.