# Spec: `hadiscovery` — neutral `expose` in meta + passive HA discovery service

> **Status:** Approved design (2026-07-10); **implemented** — `hadiscovery/` is
> scaffolded and passes `go test ./... -race`, and the bridges are migrated (publish
> `expose`, embedded discovery gated off). What remains is the on-device deploy + live
> smoke. This document is retained as the self-contained handoff spec for the design: a
> fresh Claude Code instance should be able to re-derive the whole change from this file
> plus the existing repo, without any other context.
>
> **Scope of this change:** (1) a new component `hadiscovery/` that reads a consumer-neutral
> `expose` block from slot `meta` and renders Home Assistant MQTT discovery; (2) define the
> `expose` schema in the integration model; (3) migrate the existing components to publish
> `expose` and gate off their embedded HA discovery.
>
> **You (the implementing instance) should:** follow the Sequencing at the end, implement
> each section, and run the Verification steps. Do not improvise the `expose` schema — it is
> normative; implement exactly what is specified here, then update
> `docs/station-integration-model.md` to match.

---

## 1. Context and motivation

The stationa ecosystem runs a three-plane MQTT bus. Each slot at address
`<site>/<station>/<slot>` (e.g. `muehle/hf/radio`) publishes:

| topic | retained | purpose |
|---|---|---|
| `<addr>/meta` | yes | birth certificate: identity + capabilities |
| `<addr>/state` | yes | one JSON state snapshot |
| `<addr>/status` | yes | `online` \| `offline` (LWT) |
| `<addr>/cmd` | no (except idempotent actuators) | intent |

The integration model **§9** (`docs/station-integration-model.md`) states the target:
*"a standalone HA-discovery consumer that reads `/meta` + `/state` and emits discovery for
every slot, at which point the embedded discovery is deleted."* It also names HA a
*reference* consumer, not a privileged one — *"the same bus grows Grafana, Node-RED, or
Prometheus consumers the same way, each a thin edge adapter against the same core."*

**Today's state and problem.** flexbridge and ultrabridge embed HA discovery under
`homeassistant/…` (a temporary, config-gated §9 deviation), and they have already **drifted**:
flexbridge uses nodeID `flexradio-<serial>` + `availability_topic` and does not re-publish on
HA restart; ultrabridge uses `hf-ant-ctrl` + an `availability` list and subscribes
`homeassistant/status`. More fundamentally, today's `meta` carries only `role` +
`capabilities` — it advertises enum lists (`bands`, `modes`, `directions`, `ports`) but **no
field names, units, device_class, writability, or command shapes**. So a consumer cannot
render discovery generically from current `meta` without hardcoding per-role knowledge —
which is not generic and couples every bridge to HA.

**Chosen design (referred to as "3b").** Components declare a **consumer-neutral `expose`**
block in their own `meta`: a platform-agnostic description of their observable/controllable
field surface. A new passive service, **`hadiscovery`**, reads `expose` and renders HA
discovery. Bridges contain **zero HA knowledge**; HA is one renderer of the same neutral
surface that InfluxDB, Node-RED, a dashboard, or Prometheus could equally consume. `meta`
becomes the single source of truth; adding a component or field needs no consumer edit.

This is the only arrangement that satisfies two constraints simultaneously: `meta` is in a
shape reusable by non-HA systems, and the system is maintainable / not brittle (single
source of truth — no duplicated field surfaces across N bridges). The alternative "have
components publish HA-shaped discovery directly" couples every bridge to HA and duplicates
the surface; the alternative "neutral meta + components also self-publish HA" duplicates the
surface (the real brittleness). A neutral reusable `meta` without duplication logically
requires a passive renderer — i.e. this service.

The "indirection" is a small, finite, golden-tested mapper (~5 type branches), not a brittle
pipeline.

### Realistic non-HA consumers of the `expose` surface

- **InfluxDB + Grafana** (via telegraf MQTT input): long-term telemetry history/dashboards
  (freq, drive, PA power, SWR, temps). The neutral surface tells them which fields are
  numeric and their units.
- **Node-RED:** automation flows off state topics; reads `expose` to know each slot's fields
  and controls generically.
- **A custom shack dashboard / web HMI:** auto-renders controls for any slot from its
  `expose` block instead of hardcoding per role.
- **openHAB / ioBroker:** their thing/channel model maps onto the same field types.
- **Prometheus / an archiver / MQTT explorer:** benefit from a structured, typed field list.
- **Other station coordinators:** a future band-change orchestrator reads what each slot
  exposes and commands generically (antennaselect already "follows" ant-ctrl; a neutral
  surface lets such logic be role-agnostic).

---

## 2. The neutral `expose` schema (normative — canonical home is the integration model)

`expose` is an **optional** block added to a slot's `meta` payload alongside the existing
fields. It is **consumer-neutral**: no HA vocabulary (no `device_class` strings, no Jinja
templates, no `payload_on/off`). Example for the radio slot:

```json
"expose": {
  "device": {
    "name": "FLEX-8400",
    "model": "FLEX-8400",
    "manufacturer": "FlexRadio Systems",
    "sw_version": "3.8.19",
    "area": "Radio shack"
  },
  "fields": [
    { "key": "freq_hz", "name": "Frequency", "type": "number", "unit": "Hz",
      "class": "frequency", "state_class": "measurement" },
    { "key": "band",    "name": "Band", "type": "enum", "options_ref": "bands" },
    { "key": "mode",    "name": "Mode", "type": "enum", "options_ref": "modes",
      "writable": true,
      "command": { "action": "mode", "value_key": "value", "value_type": "string" } },
    { "key": "drive",   "name": "Drive", "type": "number", "unit": "%" },
    { "key": "tx",      "name": "Transmitting", "type": "boolean", "on": "tx", "off": "rx" },
    { "key": "tuning",  "name": "Tuning", "type": "boolean" }
  ],
  "actions": [
    { "key": "retract", "name": "Retract", "command": { "action": "retract" } }
  ]
}
```

> **The example above is a schema-vocabulary illustration, not a faithful flexbridge
> meta.** It demonstrates every field shape in one block — including a `writable` enum
> (`mode`) and an `action` (`retract`) — so it deliberately mixes a read-only radio's
> fields with an `ant-ctrl`-style writable/action surface. flexbridge's *actual* `expose`
> is read-only (no `writable`, no `actions`); the `retract` action belongs to `ant-ctrl`.
> See §4 for each slot's faithful migration.

### Field semantics (neutral)

| key | meaning |
|---|---|
| `type` | `number` \| `enum` \| `boolean` \| `string` |
| `unit` | unit string (`Hz`, `%`, `°C`, `W` …); consumer maps unit→its own class taxonomy |
| `class` | optional semantic hint (`frequency`, `temperature`, `power` …) — generic, not HA-specific |
| `state_class` | `measurement` \| `total` \| `total_increasing` (carried through for HA) |
| `options` | inline enum option list; OR `options_ref`: a key into `capabilities` (e.g. `"modes"`) |
| `writable` | if true, the field is a setpoint/command target, not just a sensor |
| `command` | how a write is encoded on `/cmd`: `action` (optional), `value_key` (JSON key for the value), `value_type` (`string`\|`int`\|`float`). Rendered by the consumer into the platform's command syntax. |
| `on`/`off` | for `boolean`: the payload strings the state actually holds (e.g. `tx`/`rx`). Absent ⇒ state holds a real bool. |
| `min`/`max`/`step` | for writable `number` setpoints |

`expose` is **optional** on `meta` (slots that don't publish it are simply not discovered;
the consumer emits one diagnostic sensor for them — see §3). Logic slots omit
`device.model` (and may omit `device` entirely, falling back to role+addr).

---

## 3. New component: `hadiscovery/` (own git repo, nested in stationa, gitignored)

Module `hadiscovery`, go 1.26.2, deps `github.com/eclipse/paho.mqtt.golang v1.5.1` +
`github.com/pelletier/go-toml/v2 v2.4.2` (no serial). Layout mirrors `antennaselect/` (the
non-serial, logic-slot template) per `docs/conventions/`.

```
hadiscovery/
  go.mod, go.sum
  CLAUDE.md, README.md
  config.example.toml
  deploy.sh                       # copy antennaselect/deploy.sh, retarget (non-serial)
  docs/discovery-mqtt-api.md       # own meta shape + the neutral→HA mapping (HA-specific doc)
  cmd/hadiscovery/main.go
  internal/
    config/config.go + config_test.go
    expose/schema.go               # Expose/Field/Action/Command structs (neutral)
    expose/meta.go + meta_test.go  # Parse(metaTopic, payload) -> SlotMeta (reads meta + expose)
    ha/render.go                   # Render: expose -> []Entity; device/availability/unique_id; unit map; sanitize; ConfigTopic
    ha/render_test.go              # golden JSON per field type
    engine/lifecycle.go            # known-slots map, republish on HA birth, clear on meta-cleared, no-expose diagnostic
    engine/lifecycle_test.go
    mqtt/client.go                 # paho: own LWT, own meta (role discovery), subscribe meta filter + homeassistant/status
```

### Key signatures

`internal/config/config.go` (mirrors `antennaselect/internal/config/config.go`):
```go
type MQTT struct {
    Broker, ClientID, Site, Station, Slot string
    User, Password                         string
    DiscoveryPrefix                        string // default "homeassistant"
    MetaFilter                              string // default "<site>/+/+/meta"
}
type Config struct { Location, Host string; MQTT MQTT }
func Default() Config
func Load(path string) (Config, error)   // preserves fs.ErrNotExist
func (c Config) Validate() error        // requires site, station; slot defaults "discovery"
```

`internal/expose/schema.go` — neutral types:
```go
type Expose struct {
    Device  *DeviceBlock      `json:"device,omitempty"`
    Fields  []Field           `json:"fields,omitempty"`
    Actions []Action          `json:"actions,omitempty"`
}
type Field struct {
    Key, Name, Type, Unit, Class, StateClass string
    Options    []string `json:"options,omitempty"`
    OptionsRef string   `json:"options_ref,omitempty"`
    Writable   bool
    Command    *Command `json:"command,omitempty"`
    On, Off     string  `json:"on,omitempty"`  // boolean payloads
    Min, Max    any     `json:"min,omitempty"`
    Step        any     `json:"step,omitempty"`
}
type Action struct { Key, Name string; Command *Command }
type Command struct {
    Action    string `json:"action,omitempty"`
    ValueKey  string `json:"value_key,omitempty"`
    ValueType string `json:"value_type,omitempty"` // string|int|float
}
type DeviceBlock struct {
    Name, Model, Manufacturer, SWVersion, Area string
}
```

`internal/expose/meta.go`:
```go
type SlotMeta struct {
    Addr                       string // "<site>/<station>/<slot>"
    Site, Station, Slot         string
    Schema, Role, Link          string
    Location, Host              string
    Device                     struct { Model, Serial, Firmware string }
    Capabilities               map[string]any
    Expose                     *Expose // nil if absent
}
// Parse splits addr from the meta topic (strips "/meta"), rejects schema != "1.0"
// or empty role. The MetaFilter is "<site>/+/+/meta", so addr is always 3 segments.
func Parse(metaTopic string, payload []byte) (SlotMeta, error)
```

`internal/ha/render.go`:
```go
type Entity struct { Component, ObjectID string; Topic string; Payload []byte }
// Render maps a neutral Field/Action + slot context to HA discovery JSON.
// Injects state_topic/command_topic/availability/unique_id/device; resolves options_ref
// against capabilities; renders command_template from Command; maps unit->device_class.
func Render(prefix string, m SlotMeta) []Entity
func NodeID(m SlotMeta) string          // sanitize("<site>-<station>-<slot>")
func ConfigTopic(prefix, component, nodeID, objectID string) string
func sanitize(s string) string           // [a-z0-9_-], else '_' (matches flexbridge ha.sanitize)
```

`internal/engine/lifecycle.go`:
```go
type Pub interface {
    Publish(topic string, qos byte, retained bool, payload []byte) error
}
type Engine struct {
    prefix string; pub Pub
    mu sync.Mutex; known map[string][]Entity // key = slot addr
}
func NewEngine(prefix string, pub Pub) *Engine
func (e *Engine) OnMeta(metaTopic string, payload []byte)        // parse->Render->publish retained; idempotent (skip if byte-identical); no expose -> one diagnostic sensor
func (e *Engine) OnHAStatus(payload string)                     // "online" -> republish all known
func (e *Engine) OnMetaCleared(metaTopic string)                // empty retained -> publish empty to that slot's discovery topics, drop entry
```

`internal/mqtt/client.go` (mirrors `antennaselect/internal/mqtt/client.go`):
- LWT `<site>/<station>/<slot>/status` = `offline`; OnConnect publishes `online` (retained) +
  own `meta` (role `discovery`, link `none`, no `device`, location/host from cfg,
  `capabilities: {renders:[...], filter: "<meta_filter>"}` — `renders` is the set of HA
  component types it can render (diagnostic only, no consumer binds to it); `filter` echoes
  the configured meta_filter) +
  subscribes `MetaFilter` (default `<site>/+/+/meta`) and `homeassistant/status`.
- meta handler: zero-length payload ⇒ `eng.OnMetaCleared`; else `eng.OnMeta`.
- `homeassistant/status` handler ⇒ `eng.OnHAStatus`.
- clientID `<site>-<station>-<slot>` (default `muehle-hf-discovery`).

`cmd/hadiscovery/main.go`: same shape as `antennaselect/cmd/antennaselect/main.go` —
`config.Default()`, `-config`/`-broker` flags, `flag.Visit`-based `isFlagSet`,
`loadConfig` (default-path-missing tolerable, explicit-missing/malformed fatal),
`Validate`, `engine.NewEngine`, `mqtt.New`, `<-ctx.Done()`.

`deploy.sh`: copy `antennaselect/deploy.sh`, set `SERVICE_NAME=hadiscovery`,
`BINARY=hadiscovery`, `PKG=./cmd/hadiscovery`; drop the wiring-map/band-policy seed
blocks; keep MQTT/location/host seed env; cross-compile
`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 -trimpath -ldflags="-s -w"`; seed-once 0600 config;
hardened systemd unit (NoNewPrivileges, ProtectSystem=full, ProtectHome, PrivateTmp,
ConfigurationDirectory). No serial/HTTP/DeviceAllow. Target: shari (192.168.1.139).

### The neutral→HA render (the whole "translator"; lives in `hadiscovery`, not in any bridge)

~5 deterministic branches:

| neutral field | → HA component | consumer injects |
|---|---|---|
| `number`, not `writable` | `sensor` | `state_topic=<addr>/state`, `value_template={{ value_json.<key> }}`, unit→`device_class` (static unit map), `state_class` |
| `number`, `writable` | `number` | + `command_topic=<addr>/cmd`, `min/max/step`, `command_template` rendered from `command` |
| `enum`, not `writable` | `sensor` | `value_template` |
| `enum`, `writable` | `select` | + `command_topic`, `options` (resolve `options_ref` against `capabilities`), `command_template` from `command` |
| `boolean` | `binary_sensor` | `on`/`off` given ⇒ `value_template={{ value_json.<key> }}` + `payload_on/off`; else `value_template={{ 'ON' if value_json.<key> else 'OFF' }}`, payload ON/OFF |
| `action` | `button` | `command_topic`, `payload_press` = JSON(`command`) |

Shared envelope every entity gets, injected by the consumer:
- `availability` = `[{topic: <addr>/status, payload_available: "online", payload_not_available: "offline"}]`, `availability_mode: "all"`
- `unique_id = <nodeID>_<key>`
- `device` from `expose.device` (fallback to `meta.device`; identifiers `[nodeID]`, name from expose.device.name or `"<role> <addr>"`, manufacturer/model/sw_version/suggested_area from expose.device)
- `origin` = `{name: "hadiscovery", sw_version: <build>, support_url: ""}`
- `nodeID = sanitize(<site>-<station>-<slot>)` (e.g. `muehle-hf-radio`)
- discovery topic `<prefix>/<component>/<nodeID>/<objectID>/config`, default prefix `homeassistant`

`command` rendering (the only structured→template step), deterministic:
- `action` + `value_key`: `{"action":"<action>","<value_key>":<coerced>}` → mode ⇒
  `{"action":"mode","value":"{{ value }}"}`; freq ⇒ `{"action":"frequency","freq_hz":{{ value | int }}}`
- `value_key` only (no action): `{"<value_key>":<coerced>}` → ant-switch ⇒ `{"select":"{{ value }}"}`
- `action` only (button): `{"action":"<action>"}` → `{"action":"retract"}`

`value_type` coercion: `string`→`"{{ value }}"`, `int`→`{{ value | int }}`, `float`→`{{ value | float }}`.

Unit→device_class map (HA-specific, lives in consumer; reuse the map in
`flexbridge/internal/ha/discovery.go` `deviceClassFor`): `Hz`→`frequency`,
`°C`/`degC`→`temperature`, `W`→`power`, `V`→`voltage`, `A`→`current`,
`dBm`→`signal_strength`, else none.

### No-`expose` behavior

If a slot's `meta` has no `expose` block, the consumer emits **one diagnostic `binary_sensor`**
(entity_category `diagnostic`) tied to `<addr>/status` (payload on `online`/off `offline`),
named `"<role>"`, so the slot is visible in HA as a device with a single availability sensor.
Generic — not per-role. Also log at INFO: `slot <addr> role=<role> has no expose block; emitting diagnostic only`.

---

## 4. Component migrations (publish `expose`, gate off embedded discovery)

### flexbridge (`flexbridge/`, role `radio`, read-only)
- `internal/bridge/bridge.go` `publishMeta()` (~line 301): add `Expose` to the `metaPayload`
  struct + populate it. Fields: `freq_hz` (number, Hz, class frequency, state_class
  measurement), `band` (enum, options_ref `bands`), `mode` (enum, options_ref `modes`
  — **read-only**: flexbridge is a read-only bridge with no `/cmd`, so `mode` carries no
  `writable`/`command`), `drive` (number, unit `%`), `tx` (boolean, on `tx` off `rx`),
  `tuning` (boolean). `expose.device` from the existing `d` (Model; firmware→sw_version,
  manufacturer `FlexRadio Systems`, area from config/location). No `actions` (read-only
  bridge).
- Gate off embedded discovery: add `[mqtt] publish_ha_discovery = false` (default false);
  guard the `PublishDiscovery()` call (bridge.go:344) and stop calling it from the connect
  flow. Keep `internal/ha` for now (reversible per §9 "config-gated"); deletion is a
  follow-up once hadiscovery is proven. nodeID changes from `flexradio-<serial>` to
  `muehle-hf-radio` (note in `docs/radio2mqtt-schema.md`).

### ultrabridge (`ultrabridge/`, role `ant-ctrl`, read-write)
- `internal/mqtt/client.go` `PublishMeta()` (~line 158): add `expose` to the meta map.
  Fields: `freq_hz` (number, Hz, frequency, measurement, **writable**, command
  `{action:frequency, value_key:freq_hz, value_type:int}`, min 1800000, max 54000000, step
  1000), `band` (enum, options_ref `bands`, writable, command `{action:band, value_key:value,
  value_type:string}`), `direction` (enum, options_ref `directions`, writable, command
  `{action:direction, value_key:value, value_type:string}`), `moving` (boolean). Action
  `retract` (button, command `{action:retract}`). `expose.device`: manufacturer `Ultrabeam`,
  model `RCU-06`, area `Radio shack`.
  - **Normalization note:** this collapses the current 8 embedded entities (frequency
    sensor + frequency_set number; band sensor + band_set select; direction sensor +
    direction_set select; moving; retract) to 5 HA entities (number freq_hz, select band,
    select direction, binary_sensor moving, button retract) — a HA `number`/`select` both
    displays state and commands, so separate read/write entities are redundant. Call this
    out in `ultrabeam-mqtt-api.md`.
- Gate off embedded discovery: add `publish_ha_discovery = false` (default false); guard
  `PublishDiscovery()` (client.go:205) and the `homeassistant/status` subscription
  (client.go:353-356) behind it. nodeID changes from `hf-ant-ctrl` to `muehle-hf-ant-ctrl`.

### antennaselect (`antennaselect/`, role `reconciler`, logic slot)
- `internal/mqtt/client.go` `publishMeta()` (~line 244): add `expose`. Fields: `source`
  (enum, options_ref `ladder`), `target` (string), `mode` (string) — all read-only sensors.
  `expose.device`: name `Antenna selector`, no model (logic slot). **Confirm exact
  state-field semantics against `antennaselect/docs/antenna-select-mqtt-api.md` before
  populating** (the state payload is `{ts, mode, target, source}`).
- No embedded discovery to remove (none exists).

### antswitchbridge (`antswitchbridge/`, contract-first, role `ant-switch`)
- `docs/ant-switch-mqtt-api.md` §4 meta example: add `expose`. Fields: `selected` (enum,
  options inline `["off","p1","p2","p3","p4","p5"]`, writable, command `{value_key:select,
  value_type:string}` — no action), `settled` (boolean). `expose.device` from `device.model`.
- No code yet; the contract doc is the authoritative surface the future bridge must
  publish. (Options are inlined because `capabilities.ports` is `[1..5]` ints while
  `selected` is `"p1".."p5"` strings — shapes differ.)

---

## 5. Model / shared-doc changes

- `docs/station-integration-model.md`:
  - Add `expose` (optional) to the §3 slot template and the §8.1 normative meta-field list,
    described as *"the slot's consumer-neutral observable/controllable field surface;
    consumed by HA, historians, dashboards, etc."*
  - Update §9 deviation block: the standalone consumer (`hadiscovery`) now exists and reads
    `expose`; embedded discovery in flexbridge/ultrabridge is gated off; the `expose` block is the
    mechanism that lets HA be one of several thin edge adapters (already the model's stated
    direction).
  - Add an appendix defining the `expose` schema: `device`, `fields` (key/name/type/unit/
    class/state_class/options/options_ref/writable/command/on/off/min/max/step), `actions`
    (key/name/command). Keep it **consumer-neutral** in the model (describe types
    number/enum/boolean/string/action + writable + command descriptor); the **HA-specific
    mapping** lives in `hadiscovery/docs/discovery-mqtt-api.md`, not the model.
- `docs/templates/mqtt-schema.md`: add `expose` to the meta template example.
- `hadiscovery/docs/discovery-mqtt-api.md`: the HA mapping (the §3 render table + unit→class
  map), the discovery topic layout, the `homeassistant/status` rebirth behavior, and
  hadiscovery's own meta shape.

---

## 6. stationa meta-repo edits

- `CLAUDE.md`: add row to Projects table (`hadiscovery | hadiscovery/ | central HA discovery
  consumer`) and to Slot assignments (`muehle/hf/discovery | hadiscovery | logic slot — no
  device (runs on shari)`).
- `README.md`: add one-line entry.
- `.gitignore`: add `hadiscovery/`.
- `hadiscovery/`: `git init` (own repo, nested, gitignored by stationa).

---

## 7. Verification

**Unit tests (no network/hardware):**
- `expose/meta_test.go`: parse fixtures for radio/ant-ctrl/reconciler/ant-switch meta incl.
  `expose`; assert addr split, options_ref resolution, absent-expose ⇒ nil.
- `ha/render_test.go`: **golden JSON per field type** — number-readonly, number-writable,
  enum-readonly, enum-writable (options_ref resolved), boolean-default, boolean-custom
  (tx/rx), action/button. Assert exact `value_template`/`command_template`/`payload_on/off`/
  `options`/`unit`/`device_class`/`unique_id`/`device`/`availability`/topic. This locks the
  neutral→HA contract.
- `engine/lifecycle_test.go` (fake `Pub`): (a) `OnMeta` publishes N retained configs to
  correct topics; (b) identical `OnMeta` is a no-op; (c) changed meta republishes; (d)
  `OnHAStatus("online")` republishes all known; (e) `OnMetaCleared` publishes empty payloads
  to the slot's discovery topics and drops the entry; (f) meta without `expose` emits one
  diagnostic sensor.
- `go vet ./... && gofmt -l .` clean; `go test ./...` and `-race`. Run the same in
  flexbridge/ultrabridge/antennaselect after their migrations.

**Live smoke (pure MQTT, no hardware — like antennaselect):**
```bash
# from hadiscovery/
go run ./cmd/hadiscovery -config ./config.example.toml   # broker tcp://192.168.1.50:1883
# observe discovery populated:
mosquitto_sub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'homeassistant/#' -v
# confirm a slot, e.g.:
mosquitto_sub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'homeassistant/sensor/muehle-hf-radio/frequency/config' -v
# trigger HA rebirth, confirm republish in logs:
mosquitto_pub -h 192.168.1.50 -u hf -P "$MQTT_PASSWORD" -t 'homeassistant/status' -m 'online' -r
```
Migrate flexbridge/ultrabridge, redeploy, confirm HA shows one device per slot under
`muehle-hf-*` identifiers and the old `flexradio-*`/`hf-ant-ctrl` entities are gone (gate off).

---

## 8. Sequencing

1. **Model first:** define `expose` schema in `docs/station-integration-model.md` + template
   — canonical before any code.
2. **Scaffold hadiscovery:** go.mod → config → expose (schema+parse+test) → ha (render+golden
   tests) → engine (lifecycle+test) → mqtt client → main → deploy.sh/config.example/CLAUDE.md/
   docs. Stand-alone, runnable against the broker with no migrations.
3. **Migrate flexbridge:** add `expose` to meta, gate embedded discovery off.
4. **Migrate ultrabridge:** add `expose` to meta, gate embedded discovery + `homeassistant/status` off.
5. **Migrate antennaselect:** add `expose` to meta.
6. **antswitchbridge contract:** add `expose` to the contract doc.
7. **stationa meta edits:** CLAUDE.md, README.md, .gitignore, §9 update.
8. `git init hadiscovery`; full live smoke; confirm no duplicate entities.

---

## Critical files (reference)

- New: `hadiscovery/internal/expose/{schema,meta}.go`, `hadiscovery/internal/ha/render.go`,
  `hadiscovery/internal/engine/lifecycle.go`, `hadiscovery/internal/mqtt/client.go`,
  `hadiscovery/cmd/hadiscovery/main.go`, `hadiscovery/deploy.sh`
- Reference patterns to mirror: `antennaselect/internal/config/config.go`,
  `antennaselect/internal/mqtt/client.go`, `antennaselect/deploy.sh`,
  `antennaselect/cmd/antennaselect/main.go`
- Reuse: `flexbridge/internal/ha/discovery.go` `deviceClassFor` (unit→device_class map) and
  `sanitize` (object-id sanitization)
- Migrate: `flexbridge/internal/bridge/bridge.go` (publishMeta + gate PublishDiscovery),
  `ultrabridge/internal/mqtt/client.go` (PublishMeta + gate PublishDiscovery/HA-status sub),
  `antennaselect/internal/mqtt/client.go` (publishMeta)
- Contract: `antswitchbridge/docs/ant-switch-mqtt-api.md`
- Model: `docs/station-integration-model.md` (§3, §8.1, §9, expose appendix),
  `docs/templates/mqtt-schema.md`
- Meta-repo: `CLAUDE.md`, `README.md`, `.gitignore`