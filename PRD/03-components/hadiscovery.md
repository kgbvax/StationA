# hadiscovery — Home Assistant discovery renderer

**Component spec** — logic slot `muehle/hf/discovery` (passive; owns no physical device).

This document specifies hadiscovery, the station's central **Home Assistant discovery renderer**. The Mühle station is an amateur-radio installation (amateur radio — "ham radio" — is the licensed hobby of two-way radio communication) whose equipment is automated by small services called **bridges** (one service per physical device, translating between the device's native interface and the station's MQTT message bus). Each bridge occupies a **slot** — a reserved MQTT address of the form `<site>/<station>/<slot>` (e.g. `muehle/hf/radio`) — and publishes a three-plane MQTT interface: `…/meta` (a retained "birth certificate" describing identity and capabilities), `…/state` (a retained JSON snapshot of current values), `…/status` (a retained `online`/`offline` liveness string maintained via MQTT last-will), and it accepts commands on `…/cmd`. A full definition of these planes is in `02-interface-spec.md`.

**Home Assistant (HA)** is a popular open-source home-automation platform. It can auto-learn devices that publish a "discovery payload" — a retained JSON config message on a well-known MQTT topic — describing the entity's state topic, command topic, units, and so on. hadiscovery's job is to be the single, passive translator between the station's own consumer-neutral self-description (the `expose` block each bridge embeds in its `/meta`) and Home Assistant's discovery format. Bridges carry zero HA knowledge; hadiscovery is the only component that knows HA exists.

The re-implementation team SHALL build a service that satisfies every normative statement ("SHALL"/"MUST") in this document. Behavior contracts (topics, payloads, exact strings, timings, error handling) are normative and stack-agnostic. Technology names (Go, paho, TOML, systemd) appear only in clearly-marked reference-implementation notes.

---

## 1. Role and passivity

### 1.1 What the service does

hadiscovery is a **passive, central HA-discovery consumer**. It implements the logic slot `muehle/hf/discovery` (a "logic slot" is a slot with no physical device behind it — the service runs as a daemon on the station's Raspberry Pi control node, "shari"). It:

1. subscribes to the `/meta` birth certificate of **every slot** on the bus (default subscription filter `muehle/+/+/meta`, where `+` is the MQTT single-level wildcard);
2. reads the consumer-neutral **`expose` block** each bridge embeds in its `/meta` — a platform-agnostic description of that slot's observable and controllable fields (defined in §4; it contains no HA vocabulary);
3. renders HA MQTT-discovery messages for every exposed field and action and publishes them retained under the HA discovery tree (§6);
4. re-publishes all known discovery when Home Assistant restarts (§7.4);
5. removes discovery when a slot is decommissioned or its `expose` shrinks (§7.3, §7.5).

Live observation on the deployed system: 22 rendered entities across 5 slots (`ant-ctrl`, `pa`, `antenna-select`, `radio`, `discovery`).

### 1.2 What the service must never do (passivity contract)

The following are hard requirements:

- **R1.1** The service SHALL NOT publish to any slot's `/cmd` topic, ever. When a user presses a control rendered in Home Assistant, **Home Assistant itself** publishes the command to the slot's `/cmd`, following the `command_topic` and `command_template`/`payload_press` strings this service previously rendered. This service is not in the command path.
- **R1.2** The service SHALL NOT publish under any topic except (a) its own `/meta` and `/status`, and (b) topics under the configured discovery prefix (default `homeassistant`). It has no other write surface.
- **R1.3** The service SHALL NOT embed any HA vocabulary (HA `device_class` strings, Jinja template syntax, `payload_on`/`payload_off` values) into any slot's `/meta`. All HA knowledge SHALL live only inside this service's render layer. This is the architectural invariant that makes the `expose` block reusable by other consumers (InfluxDB, Node-RED, Prometheus) without change — see `01-architecture.md`.
- **R1.4** The service has no device, no serial port, no HTTP listener, and no stdin interface. Its only inputs are the MQTT broker and its process-level flags (`-config <path>`, default `/etc/hadiscovery/config.toml`; `-broker <url>`, which overrides the config file's broker when given). Its only upstream is the MQTT broker (deployed production broker: `tcp://192.168.1.50:1883` — see §11 on the planned broker migration).

---

## 2. The consumer-neutral `expose` schema (input contract)

This section defines the input format every bridge publishes and this service consumes. It is a behavior contract on the wire, independent of any implementation language.

### 2.1 The `/meta` birth certificate this service parses

Each slot publishes (retained) on `<site>/<station>/<slot>/meta` a JSON object. The service parses exactly the keys below; **unknown keys SHALL be ignored** (forward compatibility — bridges may add keys and older renderers must keep working):

| key | type | meaning / handling |
|---|---|---|
| `schema` | string | MUST be exactly `"1.0"`, or the whole `/meta` is rejected (logged, skipped — §7.3 step 2) |
| `role` | string | MUST be non-empty, or the `/meta` is rejected |
| `link` | string | device link type; parsed but otherwise ignored by this service |
| `location` | string | building (e.g. `"bauwagen"`); parsed but ignored by the render |
| `host` | string | compute node (e.g. `"shari"`); parsed but ignored by the render |
| `device` | object | `{model, serial, firmware}` — `model` and `firmware` are fallbacks for the HA device block (§6.6) |
| `capabilities` | object | string-keyed map of arbitrary JSON; used to resolve `options_ref` (§2.2) |
| `expose` | object | the neutral field surface (§2.2); optional — a JSON literal `null` is treated exactly like absence |

**Address derivation:** the slot address SHALL be derived from the message **topic**, never from the payload: the trailing `/meta` segment is stripped. A message whose topic is not exactly 4 segments with segment 4 equal to `meta` (i.e. `<site>/<station>/<slot>/meta`) SHALL be rejected with an error like `"meta topic %q is not <site>/<station>/<slot>/meta"` and skipped. (This holds in practice because the subscription filter ends in `/meta`.)

### 2.2 The `expose` block

All parts are optional; a slot that omits the block entirely is "undiscoverable" (§6.7). Top-level shape:

```json
"expose": {
  "device":  { "name": "...", "model": "...", "manufacturer": "...",
               "sw_version": "...", "area": "..." },
  "fields":  [ { "...field descriptor...": "..." } ],
  "actions": [ { "...action descriptor...": "..." } ]
}
```

**`expose.device`** (optional; supplements `meta.device` for the consumer-side device registry): `name` (consumer display name), `model`, `manufacturer`, `sw_version` (firmware string), `area` (a Home Assistant area name to suggest; see §6.6).

**Field** — one entry per element of `fields[]`:

| key | type | meaning |
|---|---|---|
| `key` | string | **required**: the JSON key in the slot's `/state` snapshot that this entity reads; also becomes the HA object id after sanitization (§6.1) |
| `name` | string | display name of the entity |
| `type` | string | `"number"` \| `"enum"` \| `"boolean"` \| `"string"` |
| `unit` | string | unit string (e.g. `"Hz"`, `"%"`, `"W"`); mapped to an HA `device_class` (§6.5) |
| `class` | string | explicit semantic hint (`frequency`, `power`, …) — wins over the unit-derived class |
| `state_class` | string | HA long-term-statistics class: `"measurement"` \| `"total"` \| `"total_increasing"`; passed through verbatim |
| `options` | [string] | inline enum option list |
| `options_ref` | string | key into the slot's `capabilities` map holding the option list; resolved at render time. Non-string capability elements are stringified, so an integer list `[1,2,3]` resolves to `["1","2","3"]` |
| `writable` | bool | `true` means the field is a setpoint/command target, not just a sensor |
| `command` | object | required for a writable field: how a write is encoded on `/cmd` (§2.3) |
| `on`, `off` | string | for `type:"boolean"`: the **string** payloads the state field actually holds (e.g. `"tx"`/`"rx"`). Both absent means the state holds a real JSON boolean |
| `min`, `max`, `step` | number | for writable `number` setpoints |

**Action** — one entry per element of `actions[]`: `{ "key", "name", "command" }`, a one-shot button. `key` becomes the entity object id (sanitized), `name` the display name, `command` the fixed `/cmd` payload sent on press.

### 2.3 The command descriptor (structured, never a consumer template string)

This is the input format; the HA-template rendering of it is §6.4.

| key | meaning |
|---|
| `action` | optional; the `action` word carried in the `/cmd` JSON |
| `value_key` | the JSON key the user-supplied value rides under (per the bus-wide convention usually `"value"` — arguments always ride under a `value` key, never under a key named after the action; see `02-interface-spec.md`) |
| `value_type` | `"string"` \| `"int"` \| `"float"` — how the user value is coerced |

Exactly three shapes exist:

1. `action` + `value_key` → `/cmd` payload `{"action":"<action>","<value_key>":<value>}`
2. `value_key` only → `/cmd` payload `{"<value_key>":<value>}`
3. `action` only → `/cmd` payload `{"action":"<action>"}` (button)

**R2.1** The descriptor SHALL be published by bridges in this structured form; the discovery renderer SHALL be the only place where these become consumer-specific (HA Jinja) template strings.

---

## 3. The service's own MQTT presence

### 3.1 Subscriptions (QoS 1)

| subscription | behavior on message |
|---|---|
| the configured `meta_filter` (default `<site>/+/+/meta`, i.e. `muehle/+/+/meta`; `+/+/+/meta` when no site is configured) | every message goes to the job queue (§7.2) and then to the meta engine. A zero-length payload is treated as a slot-clear (§7.5). Topic and payload are **copied** out of the incoming message before enqueueing, because many MQTT client libraries only guarantee payload validity during the handler. |
| `homeassistant/status` | payload `"online"` (after whitespace trimming) means Home Assistant restarted → the engine re-publishes every known slot's discovery (§7.4). Any other payload (including `"offline"`) is ignored. |

**R3.1** Both subscriptions SHALL be (re)established inside the client's on-connect handler, so they are re-armed on every reconnect; the broker then replays all retained `/meta` messages into the engine. This is how the engine re-learns the world after any disconnect — there is no persistent local state.

### 3.2 Publications

All publications are retained, QoS 1.

| topic | retained | QoS | payload | when |
|---|---|---|---|---|
| `muehle/hf/discovery/status` | yes | 1 | `"online"` / `"offline"` | `"online"` on every connect; `"offline"` registered as the MQTT **last will** (LWT — a message the broker publishes on the client's behalf if it disappears ungracefully) at connect, and published explicitly on clean shutdown |
| `muehle/hf/discovery/meta` | yes | 1 | own birth certificate (below) | on every connect |
| `<discovery_prefix>/<component>/<nodeID>/<objectID>/config` | yes | 1 | HA discovery JSON (§6) | on `/meta` change, HA birth, `/meta` clear (§7) |

**R3.2** The service's own `/meta` SHALL be exactly this shape (with `location` and `host` from config):

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

The service deliberately publishes **no** `expose` block — it is itself undiscoverable (a discovery-of-discovery entity would be circular). `capabilities.renders` and `capabilities.filter` are diagnostic only. There is no `device` block (no device). The MQTT client id SHALL default to `<site>-<station>-<slot>` = `muehle-hf-discovery` (config may override).

### 3.3 Connection properties

- **R3.3** The connect call SHALL be interruptible by process shutdown (SIGINT/SIGTERM): a termination signal received while the broker is unreachable SHALL abort the connect attempt and exit, not hang until the supervisor force-kills it. (In the reference implementation the stock library's connect blocks ignoring the shutdown context — this bit another stationa component live; the fix is a context-aware connect wrapper.)
- **R3.4** A persistent MQTT session (clean-session flag false) and automatic reconnect SHALL be used, so the broker replays retained messages and re-delivers on reconnect. Reference values: keep-alive 30 s, connect timeout 30 s, auto-reconnect backoff up to 10 min.
- **R3.5** The last will (`<addr>/status` = `"offline"`, QoS 1, retained) SHALL be registered at connect time, before the connection completes, so an ungraceful death still marks the slot offline.
- **R3.6** On every connect (including automatic reconnects) the on-connect handler SHALL: publish `"online"` (retained) to its own `/status`, publish its own `/meta` (retained), and establish both subscriptions.

**Liveness caveat (bus-wide fact):** on a *clean* process shutdown the broker does not fire the last will — this service therefore publishes `"offline"` explicitly before disconnecting. Other stationa components do not all do this; consumers of `/status` topics must not assume a retained `"online"` proves a service is running. See `02-interface-spec.md` §liveness.

---

## 4. Rendered output: HA discovery (behavior contract)

### 4.1 Discovery topic layout

**R4.1** Every rendered entity is published retained, QoS 1, on:

```
<discovery_prefix>/<component>/<nodeID>/<objectID>/config
```

- `discovery_prefix`: default `homeassistant` (config `discovery_prefix`).
- `component` ∈ {`sensor`, `binary_sensor`, `number`, `select`, `button`} per §4.2.
- `nodeID = sanitize("<site>-<station>-<slot>")` (e.g. `muehle-hf-radio`).
- `objectID = sanitize(field.key)` or `sanitize(action.key)`.
- `sanitize`: lowercase the input, then replace every character outside `[a-z0-9_-]` with `_`.

Examples (live bus): `homeassistant/sensor/muehle-hf-radio/freq_hz/config`, `homeassistant/select/muehle-hf-radio/mode/config`, `homeassistant/button/muehle-hf-ant-ctrl/retract/config`.

### 4.2 Field → HA component mapping

**R4.2** The mapping SHALL be deterministic and order-preserving: fields are walked in `fields[]` declared order, then actions. No maps or other nondeterministic ordering is permitted in the render path (the engine's idempotency check depends on byte-stable output — §7.3).

| `expose` input | HA component | details |
|---|---|---|
| `type:"number"`, `writable:false` | `sensor` | unit → `device_class` (§4.5) or explicit `class`; `state_class` carried |
| `type:"number"`, `writable:true` | `number` | plus `command_topic`, `command_template` (§4.4), `min`/`max`/`step`, `mode:"box"`, `retain:true`. The current value is read from the same state field — there is no separate read entity |
| `type:"enum"`, `writable:false` | `sensor` | options are informational; not emitted |
| `type:"enum"`, `writable:true` | `select` | `options` = inline `options` if non-empty, else the `options_ref` resolved against `capabilities`. **Skipped entirely (no entity)** if there is no `command` or the resolved options list is empty — HA requires a non-empty `options` list on a select. `retain:true` |
| `type:"boolean"` | `binary_sensor` | always read-only, even if the field is marked writable (§4.3) |
| `type:"string"` | `sensor` | plain `value_template`, no unit/class |
| any other/unknown `type` | *(skipped)* | unrenderable, silently dropped |
| each `actions[]` entry | `button` | static `payload_press` (§4.4), `entity_category:"config"` |

### 4.3 Common envelope injected on every rendered entity

**R4.3** Every entity payload SHALL carry, derived from the slot context (never from `expose`):

- `state_topic` = `<addr>/state` (state-reading components)
- `command_topic` = `<addr>/cmd` (writable components and buttons)
- `availability` = `[{"topic":"<addr>/status","payload_available":"online","payload_not_available":"offline"}]` with `availability_mode:"all"` — every entity of a slot tracks that slot's own last-will topic. (This is the bridge-process liveness plane only; the second, deeper liveness plane `state.device_online` — is the device behind the bridge alive — is inside the `/state` snapshot and shows up as ordinary field entities where the bridge exposes it. See `02-interface-spec.md` §liveness for the two-layer contract.)
- `unique_id` = `<nodeID>_<objectID>` (e.g. `muehle-hf-radio_freq_hz`)
- `device` block (§4.6)
- `origin` = `{"name":"hadiscovery","sw_version":<build>}` (`"dev"` unless stamped at build time)
- `value_template` = `{{ value_json.<key> }}` for every field entity (with one exception below)

**Boolean rendering (exact):**

- If the field carries `on` and/or `off` (the state holds strings such as `"tx"`/`"rx"`): `value_template` stays the pass-through `{{ value_json.<key> }}`, and `payload_on`/`payload_off` are the given values, each defaulting to `"ON"`/`"OFF"` when only one of the pair is given.
- Otherwise (the state holds a real JSON boolean): `value_template` = `{{ 'ON' if value_json.<key> else 'OFF' }}` with `payload_on:"ON"`, `payload_off:"OFF"`.

### 4.4 Command rendering (the only structured → template step)

**R4.4** HA command templates use the Jinja template syntax; the rendered strings SHALL be exactly:

| command shape | writable field → `command_template` | button → `payload_press` |
|---|---|---|
| `action` + `value_key` | `{"action":"<action>","<value_key>":<placeholder>}` | `{"action":"<action>","<value_key>":""}` (unusual; value empty) |
| `value_key` only | `{"<value_key>":<placeholder>}` | `{"<value_key>":""}` |
| `action` only | (not applicable to writable fields) | `{"action":"<action>"}` |

**R4.5** The `<placeholder>` SHALL be by `value_type`, exactly: `"int"` → `{{ value | int }}`; `"float"` → `{{ value | float }}`; `"string"` or unset → `"{{ value }}"` (quoted — the value is rendered as a JSON string).

Concrete live examples: radio mode select → `{"action":"mode","value":"{{ value }}"}`; antenna-controller frequency number → `{"action":"frequency","freq_hz":{{ value | int }}}` (the one documented bus exception to the `value`-key convention, riding under the role-specific `freq_hz` integer key); antenna-switch select → `{"select":"{{ value }}"}`; antenna-controller retract button → `{"action":"retract"}`.

### 4.5 Unit → HA `device_class` map

**R4.6** When the field's `class` is set, it passes through verbatim as the HA `device_class` (even if HA does not recognize it — see §10.4). Otherwise the unit maps as:

| unit string | `device_class` |
|---|---|
| `Hz` | `frequency` |
| `°C` or `degC` | `temperature` |
| `W` or `Watts` | `power` |
| `V` or `Volts` | `voltage` |
| `A` or `Amps` | `current` |
| `dBm` | `signal_strength` |
| anything else | key omitted (no `device_class`) |

### 4.6 Device block fallbacks

**R4.7** The HA `device` block SHALL be built with this fallback order (one HA device per slot):

| HA key | source |
|---|---|
| `identifiers` | `[<nodeID>]` |
| `name` | `expose.device.name`, else `expose.device.model`, else `"<role> <addr>"` |
| `model` | `expose.device.model`, else `meta.device.model` |
| `manufacturer` | `expose.device.manufacturer` (no fallback) |
| `sw_version` | `expose.device.sw_version`, else `meta.device.firmware` |
| `suggested_area` | `expose.device.area`, else the deployment-wide configured `area` (default `"Bauwagen"`; configuring `area=""` suppresses the fallback entirely — no `suggested_area` key is emitted for slots that do not name their own area) |

### 4.7 No-`expose` diagnostic entity

**R4.8** For a valid `/meta` **without** an `expose` block, the renderer SHALL emit exactly one `binary_sensor` so the slot is at least visible in HA as a device with a liveness entity:

- object id: `online`; discovery topic `<prefix>/binary_sensor/<nodeID>/online/config`
- `name` = the slot's `role`
- `state_topic` = `<addr>/status`, `payload_on:"online"`, `payload_off:"offline"`
- `entity_category:"diagnostic"`, plus the same availability/device/origin envelope as any other entity.

When the slot later publishes an `expose`, this diagnostic topic is cleared automatically (it is absent from the new rendered set, so the shrink-clear of §7.3 removes it).

**R4.9** hadiscovery shall never invent field values: rendered entities only *reference* `<addr>/state` keys; the values come from the bridges. Bus conventions (frequency fields named per convention in integer Hz, canonical mode names `cw|usb|lsb|am|fm|data`) are the bridges' contract, not this service's — see `02-interface-spec.md`.

---

## 5. Lifecycle and state machine

### 5.1 Startup sequence

**R5.1** On start, the service SHALL:

1. Parse flags and load config. A **missing** config file at the *default* path is tolerable (run on defaults + flags, log a note); a missing or malformed *explicitly requested* file, or a malformed default file, is fatal. Precedence: explicit CLI flag > config file > built-in default.
2. Apply derived defaults: `discovery_prefix` → `"homeassistant"`; `meta_filter` → `<site>/+/+/meta` (or `+/+/+/meta` if no site is configured). Validate: `site`, `station`, `slot`, `location`, `host`, and a non-empty `meta_filter` are required, else exit fatal with an `invalid configuration: …` message. Note: the `meta_filter` value is not validated to end in `/meta`, but it must — the address is parsed from the topic; a filter without the trailing `/meta` breaks discovery silently.
3. Construct the engine with an empty known-slots map and a no-op publisher.
4. Create the MQTT client: last-will registered (`<addr>/status` = `"offline"`, QoS 1, retained), persistent session. **The engine's publisher SHALL be wired to the client before connecting**, so retained `/meta` replayed by the broker on connect reaches the real broker, not the no-op.
5. Connect (context-interruptible, R3.3). On failure: exit fatal.
6. In the on-connect handler (every connect, including auto-reconnects): publish `"online"` retained, publish own `/meta` retained, subscribe both subscriptions (R3.1). The broker then replays all retained `/meta` into the engine.
7. Log a one-line summary (slot, filter, prefix, area) and block until SIGINT/SIGTERM.

### 5.2 The message pipeline — normative threading constraint

**R5.2 (MUST — derived from a live incident).** The service's message handling SHALL satisfy both:

1. **No blocking work on the MQTT dispatch path.** In the reference MQTT library, incoming-message handlers run inline on the connection's single dispatch goroutine. The original implementation called its synchronous publish (a blocking call) directly inside the message handler; after the first retained `/meta` the handler blocked waiting for its own outgoing PUBACK while the read loop blocked pushing the next retained message into the now-full channel — total deadlock: process alive, log frozen after the first message. It hit live exactly this way, and unit tests missed it because they drove the engine directly with a fake publisher. **Regardless of MQTT library**, the re-implementation SHALL isolate handler work from the receive path: the handler copies topic + payload (payload validity is only guaranteed for the handler's duration), enqueues a closure onto a **bounded job queue** (reference capacity 256), and returns immediately.
2. **Serialized, ordered engine work.** A single worker SHALL drain the queue and run engine work sequentially; the engine's idempotency and shrink-clear logic depend on ordered, non-concurrent processing per slot.

**R5.3** If the job queue is full, the incoming job is dropped (non-blocking enqueue; reference behavior drops silently — see §10.2). Recovery relies on retained `/meta` re-delivery on the next reconnect or the bridge republishing.

**R5.4** On shutdown the worker SHALL be cancelled **before** disconnecting, so no engine work races the disconnect; then (if still connected) publish `"offline"` to own `/status` and disconnect with a 250 ms quiesce.

### 5.3 On each `/meta` message (worker thread)

**R5.5** The engine SHALL process each `/meta` message as follows:

1. Zero-length payload → clear path (§5.5).
2. Parse (§2.1): reject on schema ≠ `"1.0"`, empty `role`, unparseable JSON, or a topic that is not `<a>/<b>/<c>/meta` — log `"[hadiscovery] skip meta <topic>: <err>"` and continue. A malformed slot never reaches render.
3. Render the entity set (§4). If the render is empty (no `expose`), render the single diagnostic entity (§4.7) instead.
4. **Idempotency check:** compare the newly rendered ordered list (discovery topic + payload bytes) against the stored set for that slot address. Byte-identical ⇒ no-op: zero publishes, zero log output. This is what makes a bridge's heartbeat re-publication of its retained `/meta` churn-free.
5. If changed and the slot had no `expose`: log the fallback line `slot <addr> role=<role> has no expose block; emitting diagnostic only` **only on an actual transition**, not on identical re-deliveries (to avoid log spam).
6. **Shrink-clear:** for topics present in the previous set but absent from the new set, publish an empty retained payload (QoS 1) — HA removes those entities.
7. Publish every entity of the new set: retained, QoS 1.
8. Store the new set under the slot address.

**R5.6** Publish failures SHALL be logged (`[hadiscovery] publish/clear/re-publish <topic>: <err>`) and otherwise ignored — no retry, no crash. Correctness eventually arrives via the bridge's next retained `/meta` re-delivery or an HA restart.

### 5.4 On `homeassistant/status` = `"online"` (HA birth)

**R5.7** When Home Assistant restarts it publishes `"online"` to `homeassistant/status`; the service SHALL then re-publish **every known slot's stored entity set** (retained, QoS 1), logging the slot count (or that there are none). The re-publish uses the *stored* rendered sets, not a re-render — exact and cheap. Any payload other than whitespace-trimmed `"online"` is ignored. (Consequence: if a slot's `/meta` changed while this service was broker-disconnected, the change is picked up via the retained `/meta` replay on reconnect, which does happen — the birth path itself never re-reads `/meta`. See §10.5.)

### 5.5 On zero-length retained `/meta` (slot decommissioned)

**R5.8** A zero-length retained `/meta` SHALL cause: look up the slot's stored entity set; publish an empty retained payload to each of its discovery config topics (HA removes the entities); drop the slot from the known map. A clear for an unknown slot is a no-op.

### 5.6 Error paths summary

| condition | required behavior |
|---|---|
| broker unreachable at start | connect blocks until shutdown signal; signal aborts the connect (R3.3); then fatal exit |
| connection lost mid-run | auto-reconnect (backoff up to 10 min reference); on reconnect the on-connect handler re-publishes own meta/status, re-subscribes, and retained `/meta` replay re-drives the engine; idempotency keeps this churn-free |
| malformed / rejected meta | logged, skipped |
| subscribe failure | logged; handler simply not armed; next reconnect retries |
| publish failure | logged, ignored |
| job queue full | job dropped (silently, in the reference — §10.2) |
| SIGINT/SIGTERM | worker cancelled first, then explicit `"offline"` on own status (if connected) and disconnect with 250 ms quiesce |

### 5.7 Timings

No application-level timers exist. Numeric values: job queue capacity **256** (default, tunable — but the queue SHALL be bounded and SHALL never block the dispatch path); shutdown quiesce **250 ms**; supervisor restart delay **5 s** (systemd `RestartSec`); MQTT keep-alive 30 s, connect timeout 30 s, reconnect backoff max 10 min (all reference defaults).

---

## 6. Configuration

Single TOML config file, default path `/etc/hadiscovery/config.toml`, mode 0600, owned by the service user. **It contains the MQTT password directly** (unlike some sibling components that split the secret into an environment file — see `05-deployment-ops.md` for the secrets convention; either is acceptable, the constraint is: never on the command line, never in the service unit, never in shell history).

| key | default | meaning |
|---|---|---|
| `location` | *(required)* | building, published in own `/meta` (deployed: `bauwagen`) |
| `host` | *(required)* | compute node, published in own `/meta` (deployed: `shari`) |
| `area` | `"Bauwagen"` | HA `suggested_area` fallback for devices whose `expose.device.area` is unset; `""` suppresses the key entirely |
| `[mqtt] broker` | *(required, or `-broker` flag)* | e.g. `tcp://192.168.1.50:1883` |
| `[mqtt] client_id` | `<site>-<station>-<slot>` | override only |
| `[mqtt] site` | *(required)* | e.g. `muehle` |
| `[mqtt] station` | *(required)* | e.g. `hf` |
| `[mqtt] slot` | `"discovery"` | own slot name |
| `[mqtt] user` | — | MQTT username (deployed: `hf`) |
| `[mqtt] password` | `""` | secret; set on the device, never via flags |
| `[mqtt] discovery_prefix` | `"homeassistant"` | root of the HA discovery tree |
| `[mqtt] meta_filter` | `<site>/+/+/meta` (or `+/+/+/meta` with no site) | the `/meta` subscription; **must end in `/meta`** — the slot address is parsed from the topic |

---

## 7. Deployment

- **R7.1** Target: the station's control node (Raspberry Pi "shari", 192.168.1.139), as a supervised daemon (reference: systemd service `hadiscovery`, service user `hadiscovery` — a system user with no login and no home directory).
- **R7.2 Seed-once config:** the deploy tooling SHALL install the config file only if it does not already exist on the target (written with restrictive umask, installed 0600, owned by the service user); existing configs are never overwritten — the device owns its settings. The MQTT password is seeded on first deploy and edited on-device afterwards.
- **R7.3 Hardened unit (reference shape):** `Type=simple`; `ExecStart=<binary> -config /etc/hadiscovery/config.toml`; `Restart=on-failure`; `RestartSec=5`; `After=network-online.target` + `Wants=network-online.target`; dedicated user/group; `NoNewPrivileges=true`; `ProtectSystem=full`; `ProtectHome=true`; `PrivateTmp=true`; no device permissions, no supplementary groups, no ports — this is a passive outbound-only consumer.
- **R7.4** Logs go to the system journal (reference: stdlib-style plain-text lines at info level; the log content is the service's only debugging surface — no metrics, no tracing). Reference log prefixes: `[mqtt]`, `[hadiscovery]`.

---

## 8. Invariants and safety rules (summary of normative constraints)

1. **Passive:** never publishes to any `/cmd`; never publishes under any address except its own `meta`/`status` and `<discovery_prefix>/…`. (R1.1, R1.2)
2. **`expose` stays consumer-neutral:** no HA vocabulary ever in a slot's `/meta`; all HA knowledge lives only in this service's render layer. (R1.3)
3. **Deterministic render:** entities emitted in `fields[]`-then-`actions[]` declared order; idempotency (byte-compare) depends on that order; no nondeterministic ordering in the render path. (R4.2, R5.5)
4. **Idempotent:** a byte-identical re-delivery of a retained `/meta` produces zero publishes. (R5.5)
5. **No stale discovery:** when an entity set shrinks (meta changed or cleared), the dropped topics get empty retained payloads so HA removes them; a decommissioned slot's whole set is cleared. (R5.5, R5.8)
6. **No blocking work in MQTT message handlers; engine work serialized on one worker.** (R5.2)
7. **Two-layer liveness respected:** each rendered entity's availability is the *slot's* `/status` (bridge last-will); the service's own liveness is its own `/status` last-will, with an explicit `"offline"` on clean shutdown. (R4.3, R3.5)
8. **Credentials only in the 0600 config file** — never flags, unit, or shell history. (§6, R7.2)
9. **Rendered values are never invented:** entities only reference `<addr>/state` keys; the bridges own the values and the bus conventions. (R4.9)

---

## 9. Known defects and fragilities (as deployed — reproduce or fix, but know they exist)

These are behaviors of the deployed implementation that a re-implementation should be aware of; each is a candidate for a deliberate fix, but the fix must not silently change the contract.

1. **The handler deadlock (fixed in the reference implementation, and THE constraint for any re-implementation).** Described in full in §5.2 / R5.2. The original code published synchronously inside the MQTT message handler and froze the live service after processing exactly one retained `/meta`. The fix (copy + bounded queue + single worker) is shared library code in the reference repo so every stationa consumer gets it; the regression test drives the real client layer, not the engine directly. Any re-implementation MUST keep message handling off the dispatch path and MUST serialize engine work, regardless of library.
2. **Silent job drop under load:** a full job queue drops the message with no log line. Recovery depends on retained `/meta` replay (reconnect) or bridge re-announce. With a 256-capacity queue this is unlikely at this station's scale, but the drop is invisible.
3. **Writable `number` without a `command` is NOT skipped:** the enum branch skips unrenderable writable fields, but a writable number with a nil/degenerate command still renders an HA `number` with `command_topic` set and an **empty** `command_template` (the JSON `omitempty` drops the key) — HA gets a number entity whose commands send the raw value to `/cmd` with no JSON wrapper, violating the `/cmd` convention. No deployed bridge currently publishes that shape, so it has never fired live. A re-implementation should skip such fields (matching the enum branch) or reject them loudly.
4. **`class` passes through verbatim** as the HA `device_class`: a bridge publishing a class HA does not recognize yields an invalid discovery payload. The neutral/HA boundary is only convention here — the neutral `class` string is not validated against HA's vocabulary.
5. **No re-render on HA birth:** `homeassistant/status`=online re-publishes the *stored* entity sets; the rebirth path never re-reads `/meta`. Changes are still picked up via retained replay on reconnect, so the system is consistent, but a re-implementation could re-render instead (trade-off: exactness/cheapness vs. freshness).
6. **`sanitize` is lowercase-only:** two distinct keys differing only by case collide into the same HA object id (`Freq` and `freq` both → `freq`), the later one silently overwriting the former's discovery topic.
7. **`OnConnect` publishes directly**, not via the job queue. This is *correct* — the on-connect handler runs on the connect path, not the dispatch router — but it is a subtle invariant a re-implementation must reproduce (or route through the same queue) rather than "fix".
8. **Docs/code nits in the reference repo:** the internal spec sketch (`docs/proposals/hadiscovery-spec.md`) predates the HA-area support (constructor signature differs from code) and its example `expose` block mixes two bridges' fields; the component README says "pending on-device deploy" while the component is in fact deployed and running on shari (the repo-root CLAUDE.md is current). None of this affects the wire contract.

---

## 10. Reference-implementation notes (non-normative)

- Language/libraries: Go; `github.com/eclipse/paho.mqtt.golang` v1.5.1 (MQTT client); `github.com/pelletier/go-toml/v2` (config); the shared in-repo module `codeberg.org/kgbvax/stationa/shared` provides the context-interruptible connect wrapper, the `Enqueue`/`RunJobs` bounded job queue, and slot-topic helpers.
- paho specifics that are load-bearing in the reference: `OrderMatters=true` (the default) is why the deadlock happened (handlers run inline on the single dispatch goroutine); `CleanSession=false` for retained replay; auto-reconnect with backoff up to 10 min. A different library must preserve the observable behavior (retained replay on reconnect, last-will, handler-context payload lifetime), not these flags.
- Build/deploy: cross-compile `GOOS=linux GOARCH=arm64 CGO_ENABLED=0` with the version stamped via `-ldflags -X`; `deploy.sh` scp's binary, unit, and seed config to the target, stops the service, installs binary under `/opt/hadiscovery/`, unit under `/etc/systemd/system/`, daemon-reloads, enables, restarts.
- Internal layout (for orientation only): `internal/expose` (neutral schema + `/meta` parser, zero HA knowledge), `internal/ha` (the only HA knowledge: render + diagnostic), `internal/engine` (lifecycle: known-slots map, idempotency, shrink-clear, birth republish), `internal/mqtt` (thin bus layer), `internal/config`.
- JSON field order inside discovery payloads is not contractual for HA, but the reference golden tests byte-compare it. The *idempotency* comparison is on the service's own bytes, so a re-implementation only needs self-consistent serialization — but it must be deterministic (R4.2).
- `origin.sw_version` value (`"dev"` unless stamped at build) is free to change.

---

## 11. Open decisions and unresolved facts

- **Broker topology.** The deployed production broker is `192.168.1.50:1883` (the config example and seed default). A migration to a broker running on shari (`192.168.1.139`) exists on an unmerged feature branch in the reference repo — committed but **not deployed** as of 2026-08-29. The re-implementation must treat `192.168.1.50` as current production and take the broker address purely from config; where the new system's broker lives is a deployment decision (see `05-deployment-ops.md`).
- **Writable-number-without-command bug (§9.3):** preserve-as-is or fix by skipping? No deployed bridge publishes the shape, so either is safe today; the research spec flags it as a fragility. Unresolved — a re-implementation should decide explicitly and document it.
- **`class` passthrough (§9.4):** should the renderer validate `class` values against the HA `device_class` vocabulary (dropping/rejecting unknown ones), or keep the verbatim pass-through? The neutral/HA boundary is only convention here. Unresolved.
- **Sanitize case-collision (§9.6):** should `sanitize` disambiguate keys that differ only by case (e.g. suffix `_2`), instead of silently overwriting? Unresolved; no live collision exists (all deployed keys are lowercase).
- **HA-birth republish strategy (§9.5):** republish stored sets (exact, cheap — current) vs. re-render from `/meta` (fresher, needs a re-read path). The current behavior is consistent because retained replay covers changes on reconnect, but the design choice is undocumented upstream. Unresolved.
- **Job-queue overflow visibility (§9.2):** the reference drops silently. A re-implementation could log a drop, apply backpressure, or widen the queue. The requirement (R5.2/R5.3) fixes only "bounded + never blocks the dispatch path"; the overflow policy is open.
- **`meta_filter` validation:** the config validator requires only that `meta_filter` be non-empty; it does not check the trailing `/meta`. A filter without it breaks address parsing silently. Open whether to add validation.
- **README vs CLAUDE.md deployment status:** the component README says "pending on-device deploy"; the repo-root CLAUDE.md and live observation (22 entities, 5 slots) say deployed and running on shari since before 2026-08-29. The code-derived and live evidence wins: the component IS deployed. Noted here per the sources rule.
- **HA instance details unknown to this repo:** the location, discovery-prefix configuration, and MQTT credentials of the actual Home Assistant instance are not in the repo; only the broker credentials (on shari, in the 0600 config) are known. A re-implementation integrating with a fresh HA must configure its discovery prefix to match (default `homeassistant`).