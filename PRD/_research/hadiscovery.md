# hadiscovery — research spec (for PRD)

Component: **hadiscovery** (`/Users/ingomar.otter/dev/stationa/hadiscovery/`)
Reviewed: 2026-08-29, branch `hf_console/review-fixes`. All statements below are from the
actual code unless flagged otherwise.

---

## 1. Purpose & role

The station ("Mühle") is an amateur-radio ("ham radio") station: a collection of radio
equipment (transceiver/receiver "radio", power amplifier "PA", antenna tuner, antenna
switch, rotator, antenna controller) that is automated by small "bridge" services. Every
device bridge or logic service occupies an MQTT **slot** at the address
`<site>/<station>/<slot>` (e.g. `muehle/hf/radio`) and publishes a three-plane MQTT
interface: `…/meta` (retained identity/birth certificate), `…/state` (retained JSON state
snapshot), `…/status` (retained `online`/`offline` liveness), and accepts intent on
`…/cmd`.

**Home Assistant** (HA) is an open-source home-automation platform. HA can render UI
entities (sensors, switches, sliders, buttons) for devices that announce themselves via
**MQTT discovery**: a retained JSON config message on a topic under a discovery prefix
(`homeassistant/…`) describing the entity's state topic, command topic, units, etc.

hadiscovery is a **passive, central HA-discovery consumer**. It implements the logic slot
`muehle/hf/discovery` (it owns no physical device; it runs as a systemd service on the
Raspberry Pi "shari"). It:

1. subscribes to every slot's `/meta` announcement (default filter `muehle/+/+/meta`);
2. reads the consumer-neutral **`expose` block** each bridge embeds in its `/meta`
   (a platform-agnostic description of that slot's observable/controllable fields — no
   HA vocabulary);
3. renders HA MQTT-discovery messages for every exposed field and action and publishes
   them (retained) under the HA discovery tree;
4. re-publishes all known discovery when Home Assistant restarts ("HA birth");
5. removes discovery when a slot is decommissioned or its `expose` shrinks.

It never writes any slot's `/cmd` (HA itself does, following the `command_topic` written
into the discovery payloads) and never publishes under any topic except its own
`meta`/`status` and the configured discovery prefix. The architecture intent: bridges
carry zero HA knowledge; HA is one renderer of a neutral surface (InfluxDB, Node-RED,
Prometheus could consume the same `expose`). Live observation: 22 rendered entities
across 5 slots (`ant-ctrl`, `pa`, `antenna-select`, `radio`, `discovery`).

---

## 2. Upstream interface

hadiscovery has **no device, no serial port, no HTTP listener**. Its only upstream is the
MQTT broker (`tcp://192.168.1.50:1883` in this deployment), and downstream of that, each
bridge's retained `/meta`.

### 2.1 What it reads: the `/meta` birth certificate

Each slot publishes (retained) on `<site>/<station>/<slot>/meta` a JSON object. hadiscovery
parses exactly these keys (unknown keys are ignored, forward-compatible):

| key | type | meaning / handling |
|---|---|---|
| `schema` | string | must be exactly `"1.0"` or the meta is rejected (logged, skipped) |
| `role` | string | must be non-empty or the meta is rejected |
| `link` | string | device link type (ignored by hadiscovery beyond parsing) |
| `location` | string | building (e.g. `"bauwagen"`); ignored by hadiscovery's render |
| `host` | string | compute node (e.g. `"shari"`); ignored by render |
| `device` | object | `{model, serial, firmware}` — `model` and `firmware` are fallbacks for the HA device block |
| `capabilities` | object | string-keyed map; used to resolve `options_ref` in `expose` |
| `expose` | object | the neutral field surface (§2.2); optional — a JSON literal `null` is treated as absent |

The slot address is derived **from the topic**, not the payload: the trailing `/meta`
segment is stripped. The topic must be exactly 4 segments with segment 4 == `meta`
(`<site>/<station>/<slot>/meta`), else the message is rejected with
`"meta topic %q is not <site>/<station>/<slot>/meta"`. (This holds because the
subscription filter is `<site>/+/+/meta`.)

### 2.2 The neutral `expose` schema (BEHAVIOR CONTRACT — input format)

`expose` (all parts optional; a slot that omits it is "undiscoverable", §5.6):

```json
"expose": {
  "device":  { "name": "...", "model": "...", "manufacturer": "...",
               "sw_version": "...", "area": "..." },
  "fields":  [ { ... Field ... } ],
  "actions": [ { ... Action ... } ]
}
```

**`expose.device`** (`device`, optional; supplements `meta.device`):

| key | meaning |
|---|---|
| `name` | HA device display name |
| `model` | device model |
| `manufacturer` | manufacturer string |
| `sw_version` | firmware string |
| `area` | Home Assistant area name to suggest |

**Field** (one per entry of `fields[]`):

| key | type | meaning |
|---|---|---|
| `key` | string | **required**: the JSON key in the slot's `/state` snapshot this entity reads; also becomes the HA object id (sanitized) |
| `name` | string | display name of the entity |
| `type` | string | `"number"` \| `"enum"` \| `"boolean"` \| `"string"` |
| `unit` | string | unit string (e.g. `"Hz"`, `"%"`, `"W"`); mapped to an HA `device_class` (§3.3) |
| `class` | string | explicit semantic hint (`frequency`, `power`, …) — **wins over** the unit-derived class |
| `state_class` | string | HA long-term-statistics class: `"measurement"` \| `"total"` \| `"total_increasing"`; passed through verbatim |
| `options` | [string] | inline enum option list |
| `options_ref` | string | key into the slot's `capabilities` map holding the option list (resolved at render time; non-string capability elements are stringified, so an int list `[1,2,3]` renders `["1","2","3"]`) |
| `writable` | bool | true ⇒ the field is a setpoint/command target, not just a sensor |
| `command` | object | required for a writable field: how a write is encoded on `/cmd` (below) |
| `on`, `off` | string | for `boolean`: the *string* payloads the state field actually holds (e.g. `"tx"`/`"rx"`). Absent ⇒ the state holds a real JSON bool |
| `min`, `max`, `step` | number | for writable `number` setpoints |

**Action** (one per `actions[]`): `{ "key", "name", "command" }` — a one-shot button;
`key` becomes the entity object id, `command` is the fixed `/cmd` payload sent on press.

**Command descriptor** (structured, never a consumer template string):

| key | meaning |
|---|---|
| `action` | optional; the `action` word carried in the `/cmd` JSON |
| `value_key` | the JSON key the user-supplied value rides under (per the bus convention usually `"value"`) |
| `value_type` | `"string"` \| `"int"` \| `"float"` — how the user value is coerced |

Three shapes exist: `action`+`value_key` → `{"action":"<action>","<value_key>":<value>}`;
`value_key` only → `{"<value_key>":<value>}`; `action` only → `{"action":"<action>}` (button).

---

## 3. MQTT presence

### 3.1 Subscriptions (QoS 1)

| subscription | handler behavior |
|---|---|
| `muehle/+/+/meta` (config `meta_filter`, default `<site>/+/+/meta`; `+/+/+/meta` when no site is configured) | every message → job queue → engine `OnMeta`. Zero-length payload ⇒ treated as a slot-clear (`OnMetaCleared`). Payload + topic are **copied** out of the paho message before enqueueing (paho payloads are only valid during the handler). |
| `homeassistant/status` | payload `"online"` (whitespace-trimmed) ⇒ HA restarted ⇒ engine re-publishes every known slot's discovery. Any other payload ignored. |

Both subscriptions are (re)established inside the paho OnConnect handler, so they are
re-armed on every reconnect and the broker replays all retained `/meta` into the engine.

### 3.2 Publications

| topic | retained | QoS | payload | when |
|---|---|---|---|---|
| `muehle/hf/discovery/status` | yes | 1 | `"online"` / `"offline"` | `online` on every connect; `offline` registered as the MQTT **last will** (LWT, QoS 1, retained) at connect and published on clean shutdown |
| `muehle/hf/discovery/meta` | yes | 1 | see below | on every connect |
| `<discovery_prefix>/<component>/<nodeID>/<objectID>/config` | yes | 1 | HA discovery JSON (§3.3) | on meta change, HA birth, meta clear (empty payload) |

hadiscovery's own `/meta` (exact shape published by `publishMeta`; note it deliberately
publishes **no** `expose` block, so hadiscovery itself is undiscoverable):

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

`capabilities.renders` and `capabilities.filter` are diagnostic only. `location` and
`host` come from config. MQTT client id defaults to `<site>-<station>-<slot>` =
`muehle-hf-discovery`.

Connection properties (paho v1.5.1 defaults where not set): `CleanSession=false`,
`AutoReconnect=true` (paho default backoff up to 10 min), keep-alive 30 s, connect
timeout 30 s (default), `OrderMatters=true` (default — load-bearing, see §9). Connect is
performed through a ctx-aware wrapper so a SIGTERM during an unreachable-broker connect
interrupts it (paho's `Connect().Wait()` alone ignores context).

### 3.3 Rendered HA discovery (BEHAVIOR CONTRACT — output format)

**Discovery topic layout:** `<discovery_prefix>/<component>/<nodeID>/<objectID>/config`
with `discovery_prefix` default `homeassistant`; `component` ∈ {sensor, binary_sensor,
number, select, button}; `nodeID = sanitize("<site>-<station>-<slot>")` (e.g.
`muehle-hf-radio`); `objectID = sanitize(field.key | action.key)`.
`sanitize` lowercases and replaces every character outside `[a-z0-9_-]` with `_`
(uppercase letters in input therefore become lowercase; `_` passes through).

Examples: `homeassistant/sensor/muehle-hf-radio/freq_hz/config`,
`homeassistant/select/muehle-hf-radio/mode/config`,
`homeassistant/button/muehle-hf-ant-ctrl/retract/config`.

**Field → HA component mapping** (deterministic, order-preserving — fields are walked in
declared order, then actions):

| `expose` input | HA component | notes |
|---|---|---|
| `type:"number"`, `writable:false` | `sensor` | unit→`device_class` (or explicit `class`), `state_class` carried |
| `type:"number"`, `writable:true` | `number` | plus `command_topic`, `command_template`, `min`/`max`/`step`, `mode:"box"`, `retain:true`. Reads current value from the same state field (no separate read entity). |
| `type:"enum"`, `writable:false` | `sensor` | options are informational; not emitted |
| `type:"enum"`, `writable:true` | `select` | `options` = inline `options` if non-empty, else resolve `options_ref` against `capabilities`. **Skipped entirely** (no entity) if no `command` or the resolved options list is empty (HA requires non-empty `options` on a select). `retain:true` |
| `type:"boolean"` | `binary_sensor` | always read-only; see below |
| `type:"string"` | `sensor` | plain `value_template`, no unit/class |
| other/unknown `type` | *(skipped)* | unrenderable, silently dropped |
| each `actions[]` entry | `button` | static `payload_press`, `entity_category:"config"` |
| `writable` without a usable `command` (enum case) | *(skipped)* | per above |

**Common envelope injected on every rendered entity** (from slot context, not from
`expose`):

- `state_topic` = `<addr>/state` (state-reading components)
- `command_topic` = `<addr>/cmd` (writable components and buttons)
- `availability` = `[{"topic":"<addr>/status","payload_available":"online","payload_not_available":"offline"}]`,
  `availability_mode:"all"` — every entity of a slot tracks that slot's own LWT
- `unique_id` = `<nodeID>_<objectID>` (e.g. `muehle-hf-radio_freq_hz`)
- `device` block (below)
- `origin` = `{"name":"hadiscovery","sw_version":<build>}` (`"dev"` unless stamped via
  `-ldflags -X` at build; one HA device per slot, `identifiers:[<nodeID>]`)
- `value_template` = `{{ value_json.<key> }}` for every field entity (except the
  bool-no-on/off case below)

**Boolean rendering:** if the field carries `on` and/or `off`: `value_template` stays the
pass-through `{{ value_json.<key> }}` and `payload_on`/`payload_off` are the given values,
each defaulting to `"ON"`/`"OFF"` when only one is given. Otherwise (state holds a real
bool): `value_template` = `{{ 'ON' if value_json.<key> else 'OFF' }}` with
`payload_on:"ON"`, `payload_off:"OFF"`.

**Command rendering** (the only structured→template step; HA Jinja syntax):

| command shape | writable field → `command_template` | button → `payload_press` |
|---|---|---|
| `action` + `value_key` | `{"action":"<action>","<value_key>":<placeholder>}` | `{"action":"<action>","<value_key>":""}` (unusual; value empty) |
| `value_key` only | `{"<value_key>":<placeholder>}` | `{"<value_key>":""}` |
| `action` only | (n/a for writable) | `{"action":"<action>"}` |

`value_type` → placeholder: `"int"` → `{{ value | int }}`; `"float"` → `{{ value | float }}`;
`"string"`/unset → `"{{ value }}"`. Concrete examples this produces on the live bus:
radio mode select → `{"action":"mode","value":"{{ value }}"}`; ant-ctrl frequency number
→ `{"action":"frequency","freq_hz":{{ value | int }}}`; ant-switch select →
`{"select":"{{ value }}"}`; ant-ctrl retract button → `{"action":"retract"}`. These
payloads follow the bus-wide `/cmd` convention: arguments ride under a `value` (or
role-specific) key, never under a key named after the action.

**Unit → HA `device_class` map** (only when `class` is unset; else `class` passes
through verbatim): `Hz`→`frequency`; `°C` or `degC`→`temperature`; `W` or `Watts`→`power`;
`V` or `Volts`→`voltage`; `A` or `Amps`→`current`; `dBm`→`signal_strength`; anything
else → key omitted.

**Device block fallbacks:**

| HA key | source |
|---|---|
| `identifiers` | `[<nodeID>]` (one HA device per slot) |
| `name` | `expose.device.name`, else `expose.device.model`, else `"<role> <addr>"` |
| `model` | `expose.device.model`, else `meta.device.model` |
| `manufacturer` | `expose.device.manufacturer` (no fallback) |
| `sw_version` | `expose.device.sw_version`, else `meta.device.firmware` |
| `suggested_area` | `expose.device.area`, else the deployment-wide configured `area` (default `"Bauwagen"`; config `area=""` suppresses the fallback entirely) |

**No-expose diagnostic entity** (for a valid `/meta` with no `expose` block): exactly one
`binary_sensor`, object id `online`, name = the slot's `role`, `state_topic` = `<addr>/status`,
`payload_on:"online"`, `payload_off:"offline"`, `entity_category:"diagnostic"`, with the
same availability/device/origin envelope. When the slot later publishes an `expose`, the
diagnostic topic is cleared (it is absent from the new rendered set, so the shrink-clear
in §5.5 removes it).

---

## 4. Command surface

hadiscovery accepts **no commands**. It subscribes only to `/meta` topics and
`homeassistant/status`; it never subscribes to any `/cmd` and never writes one. There is
no HTTP, serial, or stdin interface. Its process-level surface is only:

- `-config <path>` (default `/etc/hadiscovery/config.toml`) — TOML config file
- `-broker <url>` — broker override, wins over the config file when the flag is given

Side effects of note: when HA (or a user pressing an HA-rendered control) sends a
command, **HA** publishes to the slot's `/cmd` following the `command_topic` +
`command_template`/`payload_press` hadiscovery rendered — hadiscovery is not in that path.

---

## 5. Behavior & state machine

### 5.1 Startup

1. Parse flags; load config (a **missing** config at the *default* path is tolerable —
   run on defaults + flags, log `"no config at default path …; using defaults + flags"`;
   a missing/malformed *explicitly requested* file, or a malformed default file, is fatal).
2. Apply derived defaults (`discovery_prefix` → `"homeassistant"`; `meta_filter` →
   `<site>/+/+/meta`, or `+/+/+/meta` if site is empty). `Validate()`: requires
   `mqtt.site`, `mqtt.station`, `mqtt.slot`, `location`, `host`, and a non-empty
   `meta_filter`; else fatal `invalid configuration: …`.
3. Construct the engine (known-slots map empty, noop publisher until the client exists).
4. Create the MQTT client: LWT registered (`<addr>/status` = `"offline"`, QoS 1,
   retained), clean-session false. **Engine's publisher is wired to the client before
   connect** so retained `/meta` replayed on connect reaches the real broker.
5. Connect (ctx-aware). On failure: fatal (`mqtt connect: …`).
6. OnConnect handler (every connect, including auto-reconnects): publish `online`
   (retained) to own `/status`; publish own `/meta` (retained); subscribe
   `meta_filter` and `homeassistant/status` (QoS 1). The broker then replays all
   retained `/meta` messages into the subscription.
7. Log `"running; slot=%s/%s/%s filter=%s prefix=%s area=%s"`; block on context
   (SIGINT/SIGTERM).

### 5.2 The message pipeline (load-bearing, see §9)

Every `/meta` or `homeassistant/status` message is handled on paho's **dispatch
goroutine**: the handler copies topic+payload and pushes a closure onto a **bounded job
queue (capacity 256)**, then returns immediately. A single worker goroutine
(`shared/mqtt.RunJobs`) drains the queue and runs the engine sequentially. Two hard rules
this encodes:

- paho message handlers must **never block or call a blocking Publish** (deadlocked this
  service live — §9);
- engine work must be **serialized and ordered** (its idempotency and shrink-clear logic
  depend on sequential `OnMeta` per slot).

If the queue is full, the incoming job is **dropped** (non-blocking enqueue); recovery
relies on retained `/meta` re-delivery on the next reconnect or the bridge republishing.
The worker is cancelled before disconnect on shutdown.

### 5.3 `OnMeta` (per `/meta` message, on the worker)

1. Zero-length payload ⇒ clear path (§5.6).
2. Parse (`expose.Parse`): reject schema ≠ `"1.0"`, empty role, unparseable JSON, or a
   topic that is not `<a>/<b>/<c>/meta` — log `"[hadiscovery] skip meta <topic>: <err>"`
   and continue (a malformed slot never reaches render).
3. Render the entity set (`ha.Render`); if empty (no `expose`), render the single
   diagnostic entity instead.
4. **Idempotency check:** compare the newly rendered ordered list (topic+payload bytes)
   against the stored set for that slot address. Byte-identical ⇒ **no-op** (no publish,
   no log). This is what makes a bridge's heartbeat re-publish of its retained `/meta`
   churn-free.
5. If changed: log the no-expose fallback line only on an actual transition
   (`slot <addr> role=<role> has no expose block; emitting diagnostic only`) — not on
   identical re-deliveries, to avoid journald spam.
6. **Shrink-clear:** for topics in the previous set but not in the new set, publish an
   empty retained payload (QoS 1) — HA removes those entities.
7. Publish every entity in the new set: retained, QoS 1.
8. Store the new set under the slot address.

Publish failures are logged (`[hadiscovery] publish/clear/re-publish <topic>: <err>`) and
otherwise ignored — no retry, no crash; correctness eventually arrives via the bridge's
next retained `/meta` re-delivery or an HA restart.

### 5.4 `OnHAStatus` (HA birth)

Payload (whitespace-trimmed) == `"online"` ⇒ re-publish **every known slot's** stored
entity set (retained, QoS 1), logging
`[hadiscovery] HA online; re-publishing discovery for N slot(s)` (or
`HA online; no known slots to re-publish`). Note the re-publish uses the *stored* rendered
sets, not a re-render, so it is exact and cheap. Anything other than `"online"` (including
`"offline"`) is ignored.

### 5.5 `OnMetaCleared` (slot decommissioned)

Zero-length retained `/meta` ⇒ look up the slot's stored entity set; publish an empty
retained payload to each of its discovery config topics (HA removes the entities); drop
the slot from the map. A clear for an unknown slot is a no-op.

### 5.6 Error paths summary

| condition | behavior |
|---|---|
| broker unreachable at start | connect retries blocked until ctx cancel; fatal on SIGTERM-timeout (ctx-aware connect returns `context.Canceled` → `log.Fatalf`) |
| connection lost mid-run | paho auto-reconnect (backoff up to 10 min); on reconnect OnConnect re-publishes own meta/status, re-subscribes, retained `/meta` replay re-drives the engine; idempotency keeps this churn-free |
| malformed / rejected meta | logged, skipped |
| subscribe failure | logged (`[mqtt] subscribe failed …`); handler simply not armed (next reconnect retries) |
| publish failure | logged, ignored |
| job queue full (256) | job dropped silently |
| SIGINT/SIGTERM | worker cancelled first, then (if connected) publish `"offline"` to own status and `Disconnect(250)` (250 ms quiesce) |

### 5.7 Timing values

No application-level timers exist. Only: job queue capacity **256**; shutdown quiesce
**250 ms**; systemd `RestartSec=5`; paho defaults — connect timeout 30 s, keep-alive
30 s, reconnect backoff max 10 min.

---

## 6. Configuration

Single TOML file, default path `/etc/hadiscovery/config.toml` (0600, owned by the
service user; **contains the MQTT password directly** — no EnvironmentFile split).
Precedence: explicit CLI flag > config file > built-in default.

| key | default | meaning |
|---|---|---|
| `location` | *(required)* | building, published in own `/meta` (e.g. `bauwagen`) |
| `host` | *(required)* | compute node, published in own `/meta` (e.g. `shari`) |
| `area` | `"Bauwagen"` | HA `suggested_area` fallback for devices whose `expose.device.area` is unset; `""` suppresses the key entirely |
| `[mqtt] broker` | *(required, or -broker)* | e.g. `tcp://192.168.1.50:1883` |
| `[mqtt] client_id` | `<site>-<station>-<slot>` | override only |
| `[mqtt] site` | *(required)* | e.g. `muehle` |
| `[mqtt] station` | *(required)* | e.g. `hf` |
| `[mqtt] slot` | `"discovery"` | own slot name |
| `[mqtt] user` | — | MQTT username (e.g. `hf`) |
| `[mqtt] password` | `""` | secret; set on the device, never via flags |
| `[mqtt] discovery_prefix` | `"homeassistant"` | root of the HA discovery tree |
| `[mqtt] meta_filter` | `<site>/+/+/meta` (or `+/+/+/meta` with no site) | the `/meta` subscription; **must end in `/meta`** — the address is parsed from the topic |

Secrets convention: never on the command line, never in the systemd unit, never in
shell history; the deploy script transfers the seed config through a 0600 temp file and
removes it on the target after install.

---

## 7. Deployment

- Target: **shari**, Raspberry Pi at `192.168.1.139` (SSH user `io`), as systemd service
  `hadiscovery`, service user `hadiscovery` (system user, no login, no home).
- `deploy.sh` (env-var driven, defaults in brackets): cross-compiles
  `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X hadiscovery/internal/ha.Version=${VERSION:-dev}" -o dist/hadiscovery-linux-arm64 ./cmd/hadiscovery`;
  generates the unit and a **seed-once** config (written with umask 077, installed
  `install -o hadiscovery -g hadiscovery -m 0600` **only if** `/etc/hadiscovery/config.toml`
  does not exist; existing configs are never overwritten — the device owns its settings);
  scp's binary/unit/seed to `/tmp` on target; creates user/config dir
  (`/etc/hadiscovery` 0755 service-user-owned); stops service, moves binary to
  `/opt/hadiscovery/hadiscovery` (0755), unit to `/etc/systemd/system/hadiscovery.service`,
  daemon-reload, enable, restart, prints status. MQTT password seeds via
  `MQTT_PASSWORD=… ./deploy.sh` on first deploy (or is edited on-device afterwards with
  `sudo -e /etc/hadiscovery/config.toml`).
- systemd unit (exact):
  - `[Unit]`: `Description=Home Assistant discovery consumer for the station bus (hadiscovery)`, `After=network-online.target`, `Wants=network-online.target`
  - `[Service]`: `Type=simple`; `ExecStart=/opt/hadiscovery/hadiscovery -config /etc/hadiscovery/config.toml`; `Restart=on-failure`; `RestartSec=5`; `User=hadiscovery`/`Group=hadiscovery`; `ConfigurationDirectory=hadiscovery`; `NoNewPrivileges=true`; `ProtectSystem=full`; `ProtectHome=true`; `PrivateTmp=true`
  - `[Install]`: `WantedBy=multi-user.target`
  - No `DeviceAllow`, no `SupplementaryGroups`, no ports — passive outbound-only consumer.
- Dependencies: Go 1.26 module `hadiscovery`; `github.com/eclipse/paho.mqtt.golang
  v1.5.1`; `github.com/pelletier/go-toml/v2`; shared in-repo module
  `codeberg.org/kgbvax/stationa/shared` (via `replace` for standalone builds) providing
  ctx-aware `Connect`, `Enqueue`/`RunJobs` job queue, and topic helpers
  (`SlotBase`, `SiblingTopic`).

---

## 8. Invariants & safety rules

1. **Passive:** never publishes to any slot's `/cmd`; never publishes under any address
   except its own `meta`/`status` and `<discovery_prefix>/…`.
2. **`expose` stays consumer-neutral:** no HA vocabulary (device_class strings, Jinja,
   payload_on/off) may ever appear in a slot's `/meta`; all HA knowledge lives only in
   this service's render layer.
3. **Deterministic render:** entities are emitted in `fields[]` then `actions[]` declared
   order; engine idempotency (byte-compare) depends on that order. No maps/nondeterminism
   in the render path.
4. **Idempotency:** a byte-identical re-delivery of a retained `/meta` must produce zero
   publishes.
5. **No stale discovery:** when an entity set shrinks (meta changed or cleared), the
   dropped topics get empty retained payloads so HA removes them; a decommissioned slot's
   whole set is cleared.
6. **No blocking work in MQTT message handlers** (the live-deadlock rule, §9); engine
   work is serialized on one worker.
7. **Two-layer liveness respected:** each rendered entity's availability is the *slot's*
   `/status` (bridge LWT); hadiscovery's own liveness is its own `/status` LWT.
8. **Credentials only in the 0600 config file** — never flags, unit, or shell history.
9. Rendered field values are never invented by hadiscovery: it only references
   `<addr>/state` keys; bus conventions (freq_hz in integer Hz, canonical mode names
   `cw|usb|lsb|am|fm|data`) are the bridges' contract.

---

## 9. Known defects & fragilities

1. **Live deadlock (fixed, but THE constraint for any reimplementation).** The original
   implementation called the engine's synchronous
   `Publish(topic, qos, retained, payload)` (a `token.Wait()` — blocking) **directly
   inside the paho message handler**. paho delivers handlers inline on its single
   `matchAndDispatch` dispatch goroutine when `OrderMatters=true` (the default); after the
   first burst message the handler blocked awaiting its own outgoing PUBACK while paho's
   read loop blocked pushing the next retained PUBLISH into the full message channel —
   total deadlock: process alive, log frozen after the first retained `/meta`. It hit
   live exactly this way (subscribed `muehle/+/+/meta`, processed one meta, froze) and
   unit tests missed it because they drive `OnMeta` directly with a fake publisher. The
   fix: handlers copy topic/payload and `Enqueue` a closure onto a bounded (256) channel;
   one `RunJobs` worker runs engine work off the paho goroutine (code shared in
   `shared/mqtt` so every stationa component gets the same fix); regression test
   `TestOnMetaDefersEngineWork` in `internal/mqtt/client_test.go`. **A reimplementation
   must keep message handling off the MQTT client's dispatch path and must serialize
   engine work.**
2. **Silent job drop under load:** a full job queue drops the message with no log line.
   Recovery depends on retained `/meta` replay (reconnect) or bridge re-announce. With 256
   slots of backlog this is unlikely, but the drop is invisible.
3. **`number` + `writable` without a `command` is NOT skipped:** the enum branch skips
   unrenderable writable fields, but a writable number with a nil/degenerate command
   still renders a HA `number` with `command_topic` set and an **empty**
   `command_template` (the `omitempty` drops the key) — HA gets a number entity whose
   commands send the raw value to `/cmd` with no JSON wrapper, violating the `/cmd`
   convention. (Fragility; no bridge currently publishes that shape.)
4. **`class` passes through verbatim** as the HA `device_class` — a bridge publishing a
   class HA does not recognize yields an invalid discovery payload; the neutral/HA
   boundary is only convention here.
5. **No re-render on HA birth:** `OnHAStatus` re-publishes the *stored* entity sets; if a
   slot's `/meta` changed while hadiscovery was disconnected from the broker (but the
   broker retained it), the change is picked up only via that retained `/meta` replay on
   reconnect — which does happen — so this is consistent, but the rebirth path itself
   never re-reads `/meta`.
6. **Sanitize is lowercase-only:** two distinct keys differing only by case collide into
   the same HA object id (`Freq` and `freq` → `freq`), the later one overwriting the
   former's discovery topic.
7. **Docs vs code nits:** the spec (`docs/proposals/hadiscovery-spec.md`) sketches
   `NewEngine(prefix, pub)`; code is `NewEngine(prefix, area)` + later `SetPub` (area
   support was added after the spec). The spec's example `expose` block mixes
   flexbridge and ant-ctrl fields (the doc flags this itself). The engine logs skip
   reasons at `log.Printf` (stdlib log — journald INFO); no structured logging.
8. **README lag:** `README.md` says "pending on-device deploy"; `CLAUDE.md` and the live
   system say deployed and running on shari (CLAUDE.md is current).
9. **`OnConnect` publishes directly** (not via the job queue) — this is correct (OnConnect
   runs on the connect goroutine, not the dispatch router) but is a subtle invariant a
   reimplementation must reproduce rather than "fix".

---

## 10. Re-implementation notes

**Must be preserved verbatim (behavior contract):**

- The neutral `expose` schema (§2.2) — input format of every bridge.
- `/meta` parse rules: schema must be `"1.0"`, role required, address from topic with
  the 4-segment `/meta` check, unknown keys ignored.
- The complete field→component mapping, boolean on/off handling, command
  template/payload shapes, value_type coercion strings (exact Jinja:
  `{{ value | int }}`, `{{ value | float }}`, `"{{ value }}"`), unit→device_class map,
  sanitize rules, `unique_id`/`nodeID`/`objectID`/config-topic layout, the shared
  availability envelope, device-block fallback order, `suggested_area` fallback.
- The no-expose diagnostic binary_sensor (object id `online`, name = role,
  entity_category diagnostic).
- Own presence: `meta` payload shape (role `discovery`, link `none`, capabilities
  renders+filter, no expose), retained `status` online/offline LWT.
- Lifecycle semantics: idempotent no-op on identical meta, shrink-clear with empty
  retained payloads, clear-on-empty-meta, HA-birth republish of stored sets on
  `homeassistant/status` = `"online"`, QoS 1 retained everywhere.
- The handler/threading constraint (§9.1): engine work off the MQTT dispatch path,
  serialized; and the ctx-interruptible connect.
- Config file format, seed-once deployment convention, hardened unit shape, 0600 secrets.

**Free to change (implementation detail):**

- Language, MQTT library, JSON library, TOML library; paho specifics (CleanSession,
  auto-reconnect backoff) as long as observable behavior (retained replay on reconnect,
  LWT) is preserved.
- Job-queue capacity (256) and drop-vs-block choice — but the queue must be bounded and
  must never block the dispatch path.
- Log message wording (though the log content itself is the only debugging surface).
- JSON field order inside discovery payloads is not contractual for HA, but note the
  golden tests in this repo byte-compare it; the *idempotency* comparison is on the
  service's own bytes, so any reimplementation only needs self-consistent serialization.
- `origin.sw_version` value (`"dev"` unless stamped).
- The exact systemd hardening directives, provided the no-secrets-on-cmdline,
  dedicated-user, and passive-outbound-only properties survive.