# Research spec — stationa SYSTEM MODEL (the shared MQTT bus contract)

Source: the shared documentation of the stationa monorepo
(`docs/station-integration-model.md` v0.8 draft, `docs/logging-integration-model.md`,
`docs/conventions/{band-mode-reference,config-and-secrets,deployment,naming}.md`,
`docs/templates/mqtt-schema.md`, `docs/proposals/hadiscovery-spec.md`, `OVERVIEW.md`,
`README.md`, `CLAUDE.md`), cross-checked against `shared/schema/schema.go`,
`shared/mqtt/mqtt.go`, `ultrabridge/internal/mqtt/client.go`,
`antennaselect/config.example.toml`, `powerseq/config.example.toml`,
`m5stamp-hf-ctrl/src/config.h` + `main.cpp`.

This is the **system model** research file: it specifies the station-wide bus
contract that every per-component reimplementation must conform to. A team using a
different technology stack must be able to rebuild each component and have it be
indistinguishable on the bus from the current ones, using this document alone.

Terms are defined at first use. BEHAVIOR CONTRACT items are things any
reimplementation must reproduce exactly. IMPLEMENTATION DETAIL items are how the
current Go/Arduino code happens to do it and may be replaced freely.

---

## 1. Purpose & role

**Amateur radio (ham radio)** is the licensed hobby of two-way radio
communication. A **station** here means a complete radio installation: a
transceiver ("radio"), a power amplifier (**PA**, boosts transmit power), an
antenna tuner (**ATU**, matches antenna impedance to the feedline on frequencies
where the antenna is not resonant), an **antenna switch** (a 1-of-N relay
selector routing the feedline to one of several antennas), **rotators** (motors
that turn directional antennas), and the antennas themselves.

**stationa** is a set of small independent services ("bridges") that automate the
**Mühle** station (callsign DL9ET). Each bridge fronts one piece of hardware —
or one logic role — and mirrors it onto a shared **MQTT bus**. **MQTT** is a
lightweight publish/subscribe protocol: clients publish messages to hierarchical
text topics on a central **broker**; subscribers receive messages for topic
filters they subscribed to. "Retained" means the broker stores the last message
on that topic and delivers it immediately to every new subscriber. **LWT (Last
Will and Testament)** is an MQTT feature where the broker publishes a
pre-registered message on the client's behalf if the client disconnects
uncleanly.

There is no central server: components integrate by subscribing to each other's
state, never by calling one another. The bus itself is the system: every
component publishes what it is and what is currently true about it to a
well-known address; any consumer (the Home Assistant home-automation platform, a
dashboard, a time-series historian) subscribes and reads. **The live
configuration is the documentation** — the inventory is a live read of the bus,
never a separate document that rots.

### 1.1 Physical station layout

- Site **muehle** (a property; German for "mill"), building label `bauwagen`
  (a hut/wagon). Two stations: **hf** (high-frequency, 1.8–54 MHz) and **uhf**
  (144–440 MHz).
- **Compute hosts**: `shari`, a Raspberry Pi at `192.168.1.139` (user `io`),
  runs all Go services as systemd units; `shack-pc`, the shack PC, runs the
  interactive UHF rotator console only.
- **MQTT broker**: Mosquitto at `tcp://192.168.1.50:1883`, user `hf`, persistent
  store (retained topics survive broker restart). Credentials never on command
  lines.
- **HF devices**: FLEX-8400 radio (ethernet), ACOM 1200S PA (USB-serial
  telemetry only), ATR-1000 ATU (wifi binary WebSocket), 1:6 antenna switch
  (wifi, ESPHome firmware on a WaveShare relay board), Ultrabeam RCU-06
  antenna controller (USB-serial FTDI), Yaesu G-450DC rotator (via AF6SA "WRC"
  controller over websocket), M5 Stamp PLC #1 (wifi, custom firmware; relays
  3/4 = PA/TRX remote-on, relay 1 = PA arm), Shelly smart plugs (master mains
  + 13.8 V DC PSU).
- **UHF devices**: IC-9700 radio, PTS-303Z/3050DZ pan/tilt rotator head
  (Pelco-D/P protocol over RS-485), M5 Stamp PLC #2 (X-Quad antenna
  polarization relay control).
- **Antennas are passive resources** (see §4.4): `ant/ultrabeam` (a
  steerable-yagi beam antenna), `ant/fan-dipole` (multi-band wire dipole, 80/40
  m resonant), `ant/dummy-load` (a heat-dissipating test load), `ant/k9ay-loop`
  (receive-only loop), `ant/xquad-2m`, `ant/xquad-70cm` (UHF quad antennas),
  masts `mast/ta16-hf`, `mast/ta16-vhf`, masthead preamplifiers `preamp/2m`,
  `preamp/70cm`.

### 1.2 Component ↔ slot map (BEHAVIOR CONTRACT — the address is the contract)

| Slot address | Component | Device | Host |
|---|---|---|---|
| `muehle/power/master` | shelly-power-bridge (1 process, 2 slots) | Shelly plug — station master mains | shari |
| `muehle/power/psu-13v8` | shelly-power-bridge | Shelly plug — 13.8 V PSU (feeds HF+UHF) | shari |
| `muehle/hf/radio` | flexbridge | FLEX-8400 | shari |
| `muehle/hf/ant-ctrl` | ultrabridge | Ultrabeam RCU-06 | shari |
| `muehle/hf/ant-switch` | waveshare_relay-antswitch-bridge (ESPHome YAML, not Go) | 1:6 relay switch | embedded (wifi) |
| `muehle/hf/switch` | m5stamp-hf-ctrl (PlatformIO firmware) | M5 Stamp PLC #1, relays 3 (PA remote-on) & 4 (TRX remote-on) | embedded |
| `muehle/hf/pa-arm` | m5stamp-hf-ctrl (same PLC, relay 1) | M5 Stamp PLC #1 arm relay | embedded |
| `muehle/hf/antenna-select` | antennaselect | logic slot — no device | shari |
| `muehle/hf/pa` | acom1200s-pa-bridge | ACOM 1200S | shari |
| `muehle/hf/rotator` | wrc-rotator-bridge | Yaesu G-450DC via AF6SA WRC | shari |
| `muehle/hf/tuner` | atr1k-tuner-bridge | ATR-1000 ATU | shari |
| `muehle/hf/power-seq` | powerseq | logic slot — no device | shari |
| `muehle/hf/discovery` | hadiscovery | logic slot — no device (Home Assistant discovery renderer) | shari |
| `muehle/uhf/pol-ctrl` | m5stamp PLC #2 firmware | X-Quad polarization relays | embedded |
| `muehle/uhf/rotator` | pelcobridge2 | PTS-303Z/3050DZ pan/tilt (Pelco-D/P, RS-485) | shack-pc (interactive TUI, NOT a daemon) |
| `muehle/host/shari`, `muehle/host/shack-pc` | (host liveness nodes) | — | themselves |

Embedded nodes (ant-switch, the M5 Stamp PLCs) collapse device, adapter, and
host into one node whose firmware speaks the canonical schema directly over
wifi MQTT.

---

## 2. Upstream interface (the transport the model binds to)

The model is transport-bound to **MQTT 3.1.1, plain JSON payloads, no
Sparkplug/protobuf** (IMPLEMENTATION choice; the bus semantics are the
contract):

- Broker URL pattern `tcp://<host>:1883`; production `tcp://192.168.1.50:1883`.
- **QoS 1** (at-least-once) on every publish and subscribe, except a documented
  must-not-double-fire command may use QoS 2 (none currently does).
- **Clean session: No** — persistent session so subscriptions and queued
  messages survive client restarts.
- **Client ID** derived from the slot address, default
  `<site>-<station>-<slot>` with `/`→`-` (e.g. `muehle-hf-radio`), so a
  duplicate connection is diagnosable. Configurable via `mqtt.client_id`.
- Auto-reconnect with the LWT registered at every (re)connect.
- Mosquitto broker with persistent store so retained `meta`/`state` survive a
  broker restart. EMQX rejected (no rules engine/clustering need).

**IMPLEMENTATION DETAIL — two paho foot-guns encoded in `shared/mqtt`:** (a)
`Connect` must be context-aware: the Go paho library's `Connect().Wait()`
blocks ignoring cancellation, so SIGTERM during a broker outage hangs until
systemd SIGKILL — the shared helper bridges the wait through a goroutine +
select on context cancellation (hit acombridge live; flexbridge latent).
(b) Message handlers must never call blocking `Publish` inline (deadlocks the
dispatch loop — deadlocked hadiscovery live): handlers enqueue closures onto a
bounded jobs channel; one worker serializes state mutation + publishing; if the
buffer is full the job is **dropped** (preferred over blocking). A different
stack must reproduce the *behavior* (prompt shutdown, handler never blocks,
serialized publish), not the mechanism.

---

## 3. MQTT presence — the four planes (BEHAVIOR CONTRACT, core of everything)

Every slot address `<addr> = <site>/<station>/<slot>` (e.g. `muehle/hf/radio`)
has exactly these four topic suffixes, one per plane:

```
<addr>/meta     RETAINED   birth certificate: identity + capabilities
<addr>/state    RETAINED   live state as ONE JSON snapshot document
<addr>/status   RETAINED   plain string "online" | "offline" — the LWT topic
<addr>/cmd      NOT retained (default)  intent, bus → component
```

Read-only slots omit `/cmd`. `/cmd` MAY be retained only for physical actuators
with self-healing semantics (see §5.4). This four-plane layout is the same for
every slot, device or logic, embedded or bridged.

### 3.1 `/meta` — birth certificate

Retained JSON, published once per connect cycle (re-published on every
reconnect). Exact field set:

| Field | Type | Required | Semantics |
|---|---|---|---|
| `schema` | string | yes | currently `"1.0"` |
| `role` | string | yes | canonical role name from §4.2 — **never** a device/product name |
| `device` | object | device slots | `{model, serial?, firmware?}`; logic slots omit `serial`/`firmware` or the whole object. Two slots of one physical device carry **identical `device{model,serial}`** — that shared attribute IS the compound-device tie (no other mechanism) |
| `link` | string | device slots | transport hint: `ethernet`, `serial`, `wifi`, `none` (logic), `embedded` |
| `location` | string | yes | physical building label, from config (never code) |
| `host` | string | yes | compute node the adapter runs on, from config; embedded nodes name themselves |
| `capabilities` | object | yes | discovery contract, see §4.3 |
| `expose` | object | optional | consumer-neutral field surface, see §3.2 |

Worked example (`muehle/hf/radio/meta`, all JSON UTF-8, QoS 1):

```json
{
  "schema": "1.0",
  "role": "radio",
  "device": { "model": "FLEX-8400", "serial": "8400-01234", "firmware": "3.8.19" },
  "link": "ethernet",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "bands": ["160m","80m","60m","40m","30m","20m","17m","15m","12m","10m","6m"],
    "modes": ["cw","usb","lsb","am","fm","data"],
    "receivers": 1,
    "diversity": false,
    "amp_key": true,
    "tune": true,
    "bias_t": false,
    "rx_inputs": ["ant1","ant2","rx_a"],
    "tx_outputs": ["ant1","ant2"]
  }
}
```

### 3.2 `expose` — consumer-neutral field surface (normative; Appendix C of the model)

OPTIONAL block inside `/meta`. Describes the slot's observable/controllable
fields with **zero consumer-specific vocabulary** (no HA `device_class`
strings, no Jinja templates, no `payload_on/off`). Any consumer (Home
Assistant via `hadiscovery`, InfluxDB/Grafana, Node-RED, Prometheus, a
dashboard) renders its own representation from it.

Top level: `device` (optional; `{name (mandatory if device present), model?,
manufacturer?, sw_version?, area?}`), `fields` (list), `actions` (list).

`fields[]` entries:

| key | required | meaning |
|---|---|---|
| `key` | yes | the state-field name (the JSON key in `/state`) |
| `name` | yes | display name |
| `type` | yes | `number` \| `enum` \| `boolean` \| `string` |
| `unit` | no | unit string (`Hz`, `%`, `°C`, `W` …), for `number` |
| `class` | no | generic semantic hint (`frequency`, `temperature`, `power` …) |
| `state_class` | no | `measurement` \| `total` \| `total_increasing` |
| `options` / `options_ref` | one required for `enum` | inline option list, or a key into `capabilities` whose array is the list (single-sources enums, e.g. `"modes"`) |
| `writable` | no | true ⇒ the field is a setpoint/command target, not just a sensor |
| `command` | required when `writable` | `/cmd` encoding descriptor (below) |
| `on` / `off` | no | for `boolean`: the payload strings `/state` actually holds (e.g. `"tx"`/`"rx"`); absent ⇒ state holds a real JSON bool |
| `min`/`max`/`step` | no | for writable `number` setpoints (e.g. freq_hz: min 1800000, max 54000000, step 1000) |

`actions[]`: `{key, name, command}` — one-shot buttons.

`command` descriptor: `{action?, value_key?, value_type?}` where `value_type`
∈ `string`|`int`|`float`. Three observed shapes: `action`+`value_key` →
`{"action":"<action>","<value_key>":<value>}`; `value_key` only →
`{"<value_key>":<value>}`; `action` only (button) → `{"action":"<action>"}`.

Faithful deployed `expose` examples (from code):

- **ultrabridge (`ant-ctrl`, read-write):** fields `freq_hz` (number, Hz,
  class frequency, state_class measurement, writable, command
  `{action:"frequency", value_key:"freq_hz", value_type:"int"}`, min
  1800000, max 54000000, step 1000), `band` (enum, options_ref `bands`,
  writable, command `{action:"band", value_key:"value", value_type:"string"}`),
  `direction` (enum, options_ref `directions`, writable, command
  `{action:"direction", value_key:"value", value_type:"string"}`), `moving`
  (boolean). Action `retract` (command `{action:"retract"}`). Device:
  manufacturer `Ultrabeam`, model `RCU-06`.
- **flexbridge (`radio`, read-only):** `freq_hz`, `band` (options_ref
  `bands`), `mode` (options_ref `modes`, no writable — no `/cmd` on this
  bridge historically, though `/cmd set_band`/DVK/mic-profile exist; see
  component research), `drive` (number, `%`), `tx` (boolean, on `tx`/`rx`),
  `tuning` (boolean). No actions.
- **antennaselect (reconciler):** `source` (enum, options_ref `ladder`),
  `target` (string), `mode` (string) — all read-only.
- **ant-switch (contract doc):** `selected` (enum, options inline
  `["off","port1".."port6"]`, writable, command `{value_key:"select",
  value_type:"string"}` — **no action**), `settled` (boolean).

### 3.3 `/state` — one retained JSON snapshot

- **One full JSON document per publish** — never partial updates, never
  per-field topics. Last-write-wins; a subscriber always holds a complete,
  consistent view; a late joiner gets the whole picture from the retained
  copy without polling.
- Every snapshot carries `ts`: RFC 3339 **UTC** timestamp string (e.g.
  `"2026-07-06T12:34:56Z"`) of this publish.
- Published **only when a field value changes** (template; some components add
  a heartbeat republish — e.g. pa-arm republishes identical state every 10 s
  as a slow heartbeat).
- Optional device-slot fields: `device_online` (bool; `false` when the
  fronted hardware is unreachable while the bridge itself is up; **omitted
  when true** per the model §3 — but see Known defects: some bridges publish
  `device_online: true` explicitly) and `error` (string; human-readable
  device fault).
- **Liveness is NEVER a field inside `/state`.** A dead node cannot update its
  own state document; that is exactly why liveness is a separate topic.

Worked example (`muehle/hf/radio/state`):

```json
{ "ts": "2026-07-06T12:34:56Z", "freq_hz": 14025000, "band": "20m",
  "mode": "cw", "tx": "rx", "tuning": false, "drive": 40, "rx_input": "ant1" }
```

Per-slot state field sets (from model §7; each component research file carries
the full per-slot detail):

- `hf/radio`: `ts, freq_hz (int Hz), band (derived), mode (canonical),
  tx {rx|tx}, tuning (bool), drive (int 0–100), device_online, dvk_status
  {idle|recording|preview|playback|disabled}, dvk_id (1–12), mic_profile,
  mic_profiles (sorted list, /state-only), rx_input (omitempty)`. Always the
  **active TX receiver**. Multi-receiver radios add `radio/receiver/N/state`
  sub-topics (non-load-bearing; none deployed).
- `hf/pa`: `online-era fields: mode {operate|standby|bypass}, band, keyed
  {rx|tx|inhibited}, fwd_power_w, rfl_power_w, temp_c, fault
  {none|swr|temp|reflected}, power {on|off} — read-only telemetry now
  (set_power/RTS retired)`.
- `hf/ant-switch`: `selected {off|port1..port6}, settled (bool)`;
  capabilities `ports [1..6], off true, exclusive true, hot_switch false`.
- `hf/tuner`: `inline (bool), swr (float ratio), fwd (W), l_uh (µH
  diagnostic), c_pf (pF diagnostic), settling (bool), fault (omitempty),
  device_online (bool)`.
- `hf/switch`: `pa {on|off}, trx {on|off}` — relay positions read back from
  relays 3 & 4; capabilities `channels [pa,trx], exclusive false, kind
  {pa: remote_on, trx: remote_on}, relay_map {pa:3, trx:4}`; relay 2 spare.
- `hf/pa-arm`: `enabled (bool), armed (bool)`; capabilities `fail_safe open,
  heartbeat true, relay 1`. `enabled` = last `/cmd` permit; `armed` is
  computed by the firmware, never commanded (see §8.2).
- `hf/power-seq`: `phase {idle|starting|running|stopping}, step (string),
  fault (omitempty)`.
- `hf/antenna-select`: `ts, mode {auto|manual}, target {off|port1..port6},
  source {idle|operator|auto}`.
- `power/master`, `power/psu-13v8`: `power {on|off}` — the actual state read
  from the plug; capabilities `fail_safe {off}`; psu also declares
  `feeds [hf/radio, uhf/radio, hf/tuner, hf/ant-ctrl, hf/ant-switch,
  hf/rotator, hf/switch, hf/pa-arm]`.
- `hf/rx-loop-ctrl` (K9AY receive-loop controller): reports loop direction;
  loop preamp is sub-state, dropped by hardware TX-low line.
- `uhf/rotator`: axes `[az, el]`; `uhf/pol-ctrl`: `polarizations [h, v, cl,
  cr]`.
- Station node `muehle/hf` / `muehle/uhf`: structural path prefix only; a
  single inferred field `activity {active|inactive}` (derived from radio
  VFO-change/transmit by the antenna-select reconciler) plus
  `role: station, kind, location`. Hosts `muehle/host/<name>` publish
  `online, temp_c, load` (shari) / `online` (shack-pc).

### 3.4 `/status` — liveness (BEHAVIOR CONTRACT)

- **Plain string** `online` or `offline` — NOT JSON — retained, QoS 1.
- Register a **retained Last Will** `offline` at connect; publish retained
  `online` once up on every (re)connect; publish `offline` yourself on clean
  shutdown. The broker produces `offline` automatically when a node drops
  uncleanly.
- Being a plain string lets it map to Home Assistant availability with no
  template.

### 3.5 Two-layer liveness model (BEHAVIOR CONTRACT — consumers must AND both)

There are two distinct liveness notions, on two different planes:

1. **Bridge/adapter liveness** — `/status` (LWT). Answers "is the software
   component connected to the broker?"
2. **Device link liveness** — `/state.device_online` (bool, device slots
   only). Answers "is the *hardware behind the running bridge* reachable?"
   (serial cable pulled, device powered off) while `/status` stays `online`.
   Conventionally paired with `/state.error` (string) when false. The model
   §3 says it is "omitted when true"; several bridges instead publish
   `device_online: true` explicitly (flexbridge, ant-ctrl, pa, tuner do).

**Consumers must AND both layers** before acting on state. Known live
failure: antennaselect originally keyed on `/status` alone → chatter when the
device link died while the bridge stayed up. A consumer acting on retained
state must check liveness first; a `wait_state` consumer (powerseq) must ALSO
require the slot's `/status == online` so a dead device cannot pass on a
stale retained `/state`. And **never trust retained state for safety** —
retained values can be stale after a crash; safety lives on the hardware
plane (§8.2).

### 3.6 Additional plane-adjacent topics (logging layer proposal)

`docs/logging-integration-model.md` (draft, NOT yet implemented components)
adds a non-retained **event sub-topic** — the one sanctioned exception beyond
the four planes (same justification as the deferred meter sub-topic):

```
<station>/log/meta      retained   role qso-log birth certificate
<station>/log/status    retained   online|offline LWT
<station>/log/state    retained   rollup: {ts, session_id, qso_count,
                                    last_qso{call,band,mode,submode,freq_hz,ts},
                                    score{contest_id,class{power,assisted,mode,bands,overlay},
                                    qso_count,mult_count,points,score,
                                    breakdown[{band,mode,qsos,points,mults}]}} — score omitted outside contests
<station>/log/event     NOT retained, QoS 1  one JSON per add/replace/delete QSO
<station>/spots/event   NOT retained, QoS 1  DX-spot events (see below)
```

Addressing: the log lives at the **position** level (`muehle/<station>/log`
collapsed, or `muehle/<station>/<position>/log` when multiple operator seats
share a station); host = the position's PC. QSO ("contact") event fields:
`id` (stable string; QoS-1 is at-least-once so consumers dedupe on `id`;
`action: replace|delete` references the original `id`), `action
{add|replace|delete}`, `ts` (RFC 3339 UTC), `call`, `freq_hz` (int Hz),
`band` (derived), `mode` (canonical) + optional `submode` (ADIF `SUBMODE`
token, uppercase — `FT8`, `FT4`, `RTTY`, `PSK31`, `JS8`, `WSPR`, …; present
only when `mode == data`), `rst_sent`/`rst_rcvd`, `exchange_sent`/
`exchange_rcvd`, `gridsquare`, `my_call`, `operator`, `contest_id`,
`source` (`wsjtx`, `n1mm`, …), `adif` (optional raw ADIF record for lossless
passthrough). **ADIF** is the standard amateur-radio logbook interchange
format. Spot event fields: `action {add}`, `ts`, `dxcall`, `freq_hz`, `band`,
`mode?`, `spotter`, `comment`, `source {rbn|cluster|local}`, `status?
  {new|dupe|mult|…}`. New roles: `qso-log`, `bandmap` (future `scoreboard`).
New capability keys: `actions [add,replace,delete]`, `source`, `submodes`,
`contest`. The logger→radio/rotator **write** path is deferred (observe-only
this pass). High-rate streams (IQ, audio, spectrum, S-meter via VITA-49
datagrams at 10–20 fps) are explicitly **not on the bus**; a future meter
lane would be `<slot>/meter/<group>/<name>`, not retained.

---

## 4. Command surface

### 4.1 Slot addressing (BEHAVIOR CONTRACT)

Address grammar: `site / station / position? / slot`.

- **site** — the physical property (`muehle`).
- **station** — a transmitting entity (`hf`, `uhf`); the unit of contention in
  multi-operator setups. Structural only; carries just the inferred
  `activity` flag.
- **position** — an operator seat; collapses away in a single-op shack
  (current state), reappears only when two operators share a station (the
  logging layer is the sanctioned re-use).
- **slot** — a canonical **role** at that position.

Rules:
- **The slot segment is a canonical role name, never a device/product
  name.** `muehle/hf/ant-ctrl`, not `muehle/hf/ultrabeam-ctrl`; the device
  name lives in `/meta.device`. A second instance of the same role at one
  station takes a numeric suffix (`ant-ctrl-2`, `rotator-2`).
- **`site`, `station`, `slot`, `location`, `host` are deployment
  configuration, never code constants.** An adapter must work for any
  site/station/slot name from its config file alone; the only hard-codeable
  names are the canonical vocabulary (roles, modes, field names) and facts
  about the fronted device.
- **Site-level infrastructure sits outside the station path**: `site/host/
  <name>` (compute nodes) and `site/power/<name>` (supplies feeding *across*
  stations — master mains and the shared 13.8 V PSU). They are commanded like
  any other slot.
- A building that is not a transmitting entity (`bauwagen`) is a `location`
  attribute, not a path level.

### 4.2 Canonical roles

`radio`, `pa`, `tuner`, `ant-switch` (exclusive 1-of-N selector), `ant-ctrl`
(controller that tunes/steers a passive antenna resource), `rotator`,
`pol-ctrl`, `preamp` (function; slot or capability), `bias-feed` (likewise),
`reconciler`, `host`, `power` (supply switch: smart plug/contactor, one
on/off, declares `feeds`), `switch` (generic multi-channel relay board of
non-exclusive booleans; each channel declares `kind`: `remote_on`/`enable`/
`spare`), `pa-arm` (safety arm relay; heartbeat-driven, fail-safe-open),
`sequencer` (logic slot running ordered, delay- and confirmation-based
startup/shutdown over other slots' `/cmd`), `station`. Passive (never slots):
`ant/*`, `mast/*`, `preamp/*` (masthead LNA). Logging proposal adds:
`qso-log`, `bandmap`, future `scoreboard`.

### 4.3 Capability keys (the discovery contract)

Seen so far: `bands` (list of canonical band names — devices declare band
*names* and reference the shared table, never re-state edges), `modes`,
`receivers` (int; count of independent receive paths), `diversity`,
`amp_key`, `tune`, `bias_t`, `band_source` (PA: `cat`|`rf_sense`|`manual` —
how the amp bands itself; distinct from `rf_sample`), `rf_sample`, `alc_out`,
`hot_switch` (bool; false ⇒ cold-switch only), `rx_inputs`, `tx_outputs`,
`ports` (ant-switch: `[1..6]`), `off` (bool; an `off` selection exists),
`exclusive` (bool), `axes` (rotator: `[az]` or `[az,el]`), `polarizations`
(`[h,v,cl,cr]`), `feeds` (power slot: list of downstream slot addresses),
`channels` + `kind` (switch slot), `relay_map` (channel→relay number),
`fail_safe` (`open`|`off` — the relay's de-energized/safe state),
`heartbeat` (bool), `key_input` (PA: `hardware`), `max_power_w`,
`inline` (tuner), `tune_modes` (tuner: `[mem, full]`), `directions`
(ant-ctrl), `ladder` (reconciler: the priority-tier option list).
Capability is the binding contract: consumers bind to a declared capability,
not a device model; a binding is valid only if both ends declare compatible
capabilities. The same function may appear as a standalone slot or be
absorbed as a capability — bind to the function, discover where it lives.

### 4.4 What is NOT a slot — passive resources

Antennas, masts, and masthead preamps are **passive resources**: named,
referenced by configuration, with **no MQTT presence at all** — no topics, no
state, nothing to subscribe to. They exist only in the `antennaselect`
**wiring map** (config, never code): `port→resource` (e.g. `port1:
dummy-load`, `port3/port4: ultrabeam`, `port6: fan-dipole`, `off: grounded`).
The physical antenna can *also* have an active controller slot (`ant-ctrl`)
that tunes it — the Ultrabeam is both a passive RF resource on a switch port
and an active control slot that follows band/frequency. One wiring map is
the single editable place the antenna arrangement lives; passive-resource
names (`ultrabeam`, `fan-dipole`, …) are free-form site-local identifiers
that appear **only in site configuration**, never in adapter or reconciler
code. The **controller map** (reconciler config `[band_follow] resource +
slot`, e.g. `ultrabeam → ant-ctrl`) maps resource name → controller slot so
band-follow needs no antenna names in code. Two TA16 masts are distinct
names (`mast/ta16-hf`, `mast/ta16-vhf`) — load-bearing, never referenced by
model alone.

### 4.5 `/cmd` payload convention (BEHAVIOR CONTRACT)

Payload JSON, QoS 1, default **not retained**. The universal shape:

```json
{ "action": "<action>", "value": "<argument>" }
```

- The argument rides under the **`value` key — never under a key named after
  the action**. (atr1k-tuner-bridge got this wrong live.) This is
  structurally encoded in `shared/schema.CmdPayload{Action, Value string}` —
  `value` is ALWAYS a string on the bus (powerseq's step schema confirms:
  "value … ALWAYS a string — the value-key convention; model value_type is
  string|int|float, NO bool"; booleans are sent as `"true"`/`"false"`
  strings, e.g. pa-arm `set_enabled` value `"true"`).
- **Exception:** a `command` descriptor with `value_key` other than `value`
  re-homes the argument — e.g. ultrabridge frequency:
  `{"action":"frequency","freq_hz":14025000}` (int64 under `freq_hz`, exactly
  as its `expose.command` declares). The `expose` block is the authority for
  which key carries the value.
- Unknown/invalid payloads are logged and dropped by bridges (`json.Unmarshal
  error or empty action → log, return`); unknown actions are logged and
  ignored (`unknown cmd action=%q`) — a bridge never crashes on bad intent.
- **Plane discipline:** commands are fire-and-observe. Send the command, then
  watch `/state` to see if it took. Nobody assumes success because a command
  was emitted. Consumers react to **state**, never to intent.
- **Retention exception (self-healing actuators):** `/cmd` MAY be retained
  when re-applying the command on reconnect produces the same physical
  outcome (idempotent steady-state setpoints: power `set_power`,
  switch `set_pa`/`set_trx`, pa-arm `set_enabled`, ant-switch `select`).
  One-shot physical commands (e.g. `retract`) MUST clear the retained topic
  by publishing an **empty retained payload** immediately after execution so
  they do not re-fire on reconnect (ultrabridge does exactly this). Never
  retain a one-shot like `tune` — a retained command re-delivers on every
  reconnect with no operator behind it.

### 4.6 Per-slot command surfaces (summary)

- `power/master`, `power/psu-13v8`: `set_power {on|off}` (retained).
- `hf/radio` (flexbridge, otherwise read-only): `set_freq_hz` (canonical
  tuning intent), `set_mode`, `set_drive`, `select_rx`, `tune {start|stop}`
  (reaches `/cmd` only from the sequencer after the PA is disarmed and
  confirmed — operators publish tune requests to the sequencer, never
  directly), `set_band {label}` (SmartSDR native band-stacking: sends
  `display pan s <pan_handle> band=<wavelength>` where the wavelength is the
  band number, `20m`→`20`; the radio restores its persisted per-band
  frequency/mode; the bridge republishes `freq_hz` with `band` derived — band
  and frequency can never disagree), `dvk_play {1..12}` /
  `dvk_stop {id|active}` (digital voice keyer one-shots, voice modes only),
  `set_mic_profile {name}` (NOT retained; SmartSDR `profile mic load "<name>"`;
  no save — `profile mic save` is obsolete on v4+).
- `hf/pa`: `set_band`, `set_mode {operate|standby}`, `clear_fault`
  (band_source `rf_sense` — the amp auto-bands by sensing RF drive; the
  software `set_band` is a pre-position path so the amp doesn't trip on the
  first TX on a new band).
- `hf/ant-switch`: `select {off|port1..port6}`.
- `hf/tuner`: `set_inline {true|false}`, `tune {full|mem}`.
- `hf/switch`: `set_pa {on|off}`, `set_trx {on|off}` (retained, idempotent).
  Closing relay 3/4 asserts the PA/TRX "remote power-on" control lines
  (soft triggers, not mains cuts); opening releases the trigger and the
  device soft-shuts.
- `hf/pa-arm`: `set_enabled {true|false}` (retained steady-state permit).
  `armed` is NEVER commanded directly.
- `hf/power-seq`: `start` | `stop` (operator one-button; NOT retained).
- `hf/ant-ctrl` (ultrabridge): `frequency` (with int64 `freq_hz` key),
  `band` (value = canonical band name; resolved through the band-centre
  table, kHz), `direction` (value; `"mode"` accepted as a deprecated alias
  for `direction`), `retract` (one-shot; clears retained `/cmd` after
  execution).
- `hf/rotator`: (see component research) az set/park.
- `uhf/rotator` (pelcobridge2): **only `stop`** — no motion path exists from
  the bus; arming is a keyboard act in the TUI, never remote.
- `hf/antenna-select`: operator hold intent (ladder tier 2; e.g. force a
  dummy-load selection).
- `hf/discovery`: none (passive consumer).

---

## 5. Behavior & state machine (system-level)

### 5.1 Startup / connect sequence (every component, BEHAVIOR CONTRACT)

1. Load config (see §7); resolve address `<site>/<station>/<slot>`.
2. Connect to broker with client ID `<site>-<station>-<slot>` (config-
   overridable), clean session = No, auto-reconnect, and register the
   retained LWT `<addr>/status = offline`.
3. On (re)connect: publish retained `online` to `<addr>/status`; publish
   retained `/meta` (re-birth — late subscribers and the discovery consumer
   get it from retention anyway, but republish keeps the connect cycle
   self-describing); publish retained `/state` snapshot; subscribe `/cmd`
   (read-write slots).
4. Consumers subscribe `<addr>/#` or wildcard meta filters
   (`<site>/+/+/meta`) and immediately receive retained meta/state/status.

### 5.2 Arbitration ladder (Mechanism A — the reconciler)

Actuators are dumb; policy lives in a reconciler/arbiter (antennaselect). When
multiple commanders want one actuator, one arbiter resolves by priority and
emits one intent stream:

```
1 idle     station.activity == inactive        → safe default (off / grounded)
2 operator active hold (dummy load, forced)   → that selection
3 auto     policy(state)                      → resolved selection
```

Highest asserting tier wins. `mode` is **derived**: `manual` while an operator
hold is active, else `auto` — there is no separate auto/manual switch. The
reconciler publishes `source {idle|operator|auto}` so the bus documents *why*
the actuator is where it is. **Idle-over-operator is deliberate** (walk-away
safety: a grounded antenna overrides a forgotten operator hold — documented
as chosen behavior, not emergent).

Deployed reconciler bindings (config-driven, `antennaselect/config.example.toml`):

- Subscribes `radio` state (`band`, `tx`, `freq_hz`), operator `/cmd`,
  `ant-switch/selected`. **Activity inference:** a `freq_hz` change or
  `tx == "tx"` marks active; an idle timeout with neither marks inactive.
- `[wiring_map]` (see §4.4), `[band_policy]`:
  `ultrabeam = ["6m","10m","12m","15m","17m","20m"]`,
  `fan-dipole = ["30m","40m","60m","80m"]`,
  `fallback = "fan-dipole"` (unmatched bands incl. 160m route to the
  fan-dipole via the ATU), `[priority] order = ["idle","operator","auto"]`.
- `[band_follow]`: when the selected resource is `ultrabeam`, emit frequency
  intents to slot `ant-ctrl`.
- `[pa_follow]`: pre-position the ACOM to the radio's band (`pa.set_band ←
  radio.band`) whenever the radio is online and reporting a band — NOT gated
  on antenna selection, no TX guard (hot-switch protection is hardware).
- `[tuner_follow]`: engage the ATU in-line when the selected resource is
  `fan-dipole` AND the band ∈ `["30m","60m","80m","160m"]` (non-resonant);
  bypass otherwise. Tuner `/cmd` NOT retained (the reconciler self-heals by
  re-resolving on the retained radio/state replay at reconnect).
- `[idle] timeout_minutes = 30` — after 30 minutes with no VFO change and no
  TX, target = `off` (ladder tier 1, walk-away lightning protection; the
  switch's `off` position shorts the open ports to ground).

### 5.3 Cold-switch ordering contract

A `hot_switch: false` switch (the ant-switch is one) must have RF inhibited
and RX confirmed **before** the port moves — auto or manual. The reconciler
withholds a port change during TX. Tuning: the PA must be disarmed and
confirmed before the tune carrier (there is no hardware tune line, so the
TUNE action is routed through the automation so disarm leads).

### 5.4 Startup/shutdown sequence (Mechanism: `powerseq`, the sequencer)

The sequencer owns ordered, delay- and liveness-confirmation-based
startup/shutdown. **The sequence is config-driven** (`[[startup]]` /
`[[shutdown]]` step lists in TOML; step kinds `cmd` / `wait_status` /
`wait_state` / `delay`); the model text is the default shipped in
`config.example.toml`, not code. Exact shipped default:

Startup: 1) `power/master` `set_power on` → 2) `delay "network"` (~30 s, let
the broker and Shelly WiFi come up after master mains) → 3) `power/psu-13v8`
`set_power on` → 4) `wait_status` online for `hf/switch`, `hf/pa-arm`,
`hf/ant-switch` (they boot on 13.8 V) → 5) `hf/switch` `set_trx on` → 6)
`wait_status` online for `hf/radio` → 7) `hf/switch` `set_pa on` → 8)
`wait_state` `hf/pa` `power == "on"` (with the implicit `/status`-online
precondition, so a dead PA cannot pass on a stale retained `/state`) → 9)
`hf/pa-arm` `set_enabled true` (arms when safe).

Shutdown: the exact reverse with `delay "stagger"` (2 s) between steps for
inrush control: `pa-arm set_enabled false` → stagger → `set_pa off` →
stagger → `set_trx off` → stagger → `psu-13v8 off` → stagger → `master off`.

Timing constants (`[timing]`): `network_delay_s = 30`, `step_timeout_s = 120`
(default deadline for every wait step), `shutdown_stagger_s = 2`,
`poll_interval_ms = 200` (wait re-check), `default_hold_ms = 0` (condition
must hold continuously `hold_ms`; an explicit 0 = edge-triggered).
Sequencer state: `phase {idle|starting|running|stopping}`, `step` (current
step name), `fault` (omitempty). Phase transitions are implicit (entering
first startup step → `starting`; last done → `running`; first shutdown step →
`stopping`; last done → `idle`). **A wait timeout → `phase=idle`,
`fault="<step>: <reason>"`, NO rollback** — driven slots hold their last
retained `/cmd`. The sequencer is **one writer but does not lock**: any
channel stays directly toggleable for troubleshooting while the sequencer is
idle. Controlled-slot `/cmd`s are retained; the sequencer's own `start`/`stop`
are NOT retained.

### 5.5 The soft/hard straddle (Mechanism B — safety)

Safety, interlock, and sequencing do **NOT** live on the messaging plane:

- **Hardware series interlock:** a physical TX-inhibit line runs in series
  through the relevant components; any open link inhibits transmit. Fast,
  local, fail-safe. The messaging layer only *mirrors* each link as read-only
  state — it is never in the enforcement path.
- **Fail-safe defaults:** a relay's default and loss-of-signal state is the
  safe one (PA isolated / TX inhibited). Loss of software, network, or power
  degrades to inaction, not hazard. The ant-switch `off` position shorts
  open ports to ground (lightning protection); Shelly plugs' power-on default
  is off (`fail_safe {off}`); the pa-arm relay is fail-safe **open**
  (`fail_safe {open}` — de-energized = unpowered relay coil = arm permit
  absent).
- **Software arm, not software gate:** the software contribution is a
  slowly-changing **arm permit** combined by hardware AND with the fast
  hardware key line. Normal transmit timing rides the hardware edge;
  software only arms/disarms. The arm permit is heartbeat-driven — loss of
  heartbeat drops the arm.
- Hardware interlock chain (enforces; software mirrors only):

```
13.8 V PSU → M5 Stamp PLCs + ant-switch (boot on supply) → radio (remote-on) → pa (remote-on)
radio (TX low) → rx-loop-ctrl (preamp off) → ant-ctrl (inhibit if moving) → pa-arm → pa
```

### 5.6 The `pa-arm` arm decision (embedded firmware, BEHAVIOR CONTRACT)

The M5 Stamp PLC #1 computes `armed` itself (it is never commanded):

```
armed = enabled ∧ radio_online ∧ ¬radio.tuning ∧ band_safe ∧ heartbeat ∧ antenna_ready
```

- `enabled` = last `set_enabled` permit (retained `/cmd`, idempotent).
- Heartbeat: the radio's `/state` must have been received within
  **10 s** (`RADIO_HEARTBEAT_MS = 10000`, "conservative: 10 s"); pa-arm
  itself republishes its state at least every 10 s (`PA_ARM_HEARTBEAT_MS =
  10000`) as a slow heartbeat so absence is detectable.
- Radio offline ⇒ `armed` false. Loss of 13.8 V ⇒ the relay drops open
  (fail-safe). `antenna_ready` drops the arm when the antenna is
  grounded/off (ant-switch not on a valid port).
- `band_safe` = the radio's band is one the station deems safe (per firmware
  band set; see component research for exact set).

### 5.7 Reconnection & error behavior (contract)

- Broker loss: paho-style auto-reconnect; on reconnect the full birth
  sequence (status `online`, meta, state, resubscribe) re-runs.
- Device link loss with bridge up: `/state.device_online: false` +
  `/state.error: "<human-readable>"`; `/status` stays `online`.
- Bridge crash / unclean broker loss: broker publishes retained `offline`
  on `/status`. All the slot's retained state is then known-stale.
- Serial self-heal (ultrabridge, IMPLEMENTATION-relevant but live-proven):
  FTDI adapters drop and re-enumerate under a new tty; the bridge reopens by
  stable by-id path on EIO (`write request: Input/output error`) rather than
  dying.
- Malformed `/state` seen by a consumer drops that slot's prior snapshot (no
  stale poisoning) — powerseq behavior.

---

## 6. Configuration (conventions)

### 6.1 Config/secrets (BEHAVIOR CONTRACT)

- **Single TOML config file per service**, default
  `/etc/<SERVICE>/config.toml`, owned by the dedicated service user, mode
  **0600** (it can hold the MQTT password in plaintext). TOML via
  `pelletier/go-toml/v2` is the current implementation; the file format and
  precedence are the contract.
- Precedence: **explicit flag > config-file value > built-in default**.
  "Explicit" is detected by whether the flag was actually set (Go
  `flag.Visit`), not by comparing to the default — a flag set to its default
  value still wins over the file.
- Missing-file semantics: default path absent → run on defaults + flags
  (local-dev/mock workflow keeps working); `-config` explicitly set to a
  missing/unreadable path → fatal; present-but-malformed → fatal with the
  parse error.
- Secrets never on the command line, never in `ExecStart`, never in `ps`.
  Two sanctioned patterns: (a) password directly in the 0600 TOML
  (ultrabridge, others); (b) **EnvironmentFile** (flexbridge pattern):
  `EnvironmentFile=/etc/flexbridge/flexbridge.env` (0600) holding
  `FLEXBRIDGE_MQTT_PASSWORD=<pw>`; the app reads the env var and overrides
  the config value. powerseq uses `POWERSEQ_MQTT_PASSWORD` the same way.
- **Env-overload prefix** derived from the component dir name uppercased with
  hyphens→underscores: `atr1k-tuner-bridge` → `ATR1K_TUNER_BRIDGE_*`
  (e.g. `ATR1K_TUNER_BRIDGE_MQTT_PASSWORD`). The systemd EnvironmentFile
  matches. (pelcobridge2 keeps `PELCOBRIDGE2_*`.)
- Common `[mqtt]` keys across services: `broker`, `client_id` (default
  derived from address), `site`, `station`, `slot`, `user`, `password`,
  `discovery_prefix` (default `homeassistant`; legacy embedded discovery
  only), `publish_ha_discovery` (default **false**, legacy gate).
- Identity keys: `location` (building), `host` (compute node) — both
  published in `/meta`, both from config, never code.

### 6.2 Seed-once deploy

`deploy.sh` generates a config from the developer's env vars, transfers it to
a 0600 temp path, and installs it **only if no config exists yet**; an
existing file is left untouched and the transferred copy removed ("config
exists -- leaving it untouched (seed-once)"). The device owns its settings;
an operator can edit the file on the Pi and redeploy binaries freely without
losing changes (delete the file + redeploy to re-seed). Seed generation uses
`umask 077`/`install -m 600` and escapes backslash/double-quote so passwords
with special characters round-trip.

### 6.3 Bridge naming

- Device bridges: `<devtag>-<function>-bridge`, where `<devtag>` is the
  **device family / control interface** (not the narrowest model number) and
  `<function>` is the canonical role with internal hyphens collapsed to one
  token (`ant-switch`⇒`antswitch`, `ant-ctrl`⇒`antctrl`). Examples:
  `flex-radio-bridge` (target), `atr1k-tuner-bridge`,
  `waveshare_relay-antswitch-bridge` (the `_` in `waveshare_relay` is a
  recorded deliberate deviation), `shelly-power-bridge` (family tag, fronts
  two slots — one process may own multiple slots).
- Derived names: env prefix, systemd unit, install dir `/opt/<name>`, service
  user, Go module and binary all equal the dir name.
- Exceptions (logic slots, no device): `antennaselect`, `hadiscovery`,
  `powerseq`. Deviation: `pelcobridge2` (interactive TUI, not a daemon).
- Legacy names not force-renamed (tracked TODO): `flexbridge` →
  `flex-radio-bridge`, `ultrabridge` → `ultrabeam-ant-ctrl-bridge`,
  `acom1200s-pa-bridge` (deliberately model-specific, deviating from the
  family rule).

---

## 7. Deployment

- Target: **shari**, Raspberry Pi (arm64 Linux) at `192.168.1.139`, user
  `io`. Broker `192.168.1.50:1883`.
- Build: cross-compile on the workstation
  `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`
  (static, no shared libs on target); each project's `deploy.sh` wraps build +
  scp + install. Per-module builds resolve the shared module via a
  `replace ../shared` directive, never via `go.work` (dev-only convenience).
- Hardened systemd unit per service:

```ini
[Service]
User=SERVICE / Group=SERVICE      # dedicated unprivileged system user
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
ConfigurationDirectory=SERVICE    # systemd manages /etc/SERVICE
```

- Serial services add `SupplementaryGroups=dialout` and
  `DeviceAllow=char-ttyUSB rw`, `char-ttyACM rw`, `char-tty rw`; a udev rule
  (installed by deploy.sh when `SERIAL_USB_VENDOR` is set, default `0403`
  FTDI) forces FTDI USB-serial adapters to group `dialout`, mode 0660.
- `ExecStart=/opt/SERVICE/SERVICE -config /etc/SERVICE/config.toml` — nothing
  else; no secrets in the unit.
- Management: `ssh io@192.168.1.139`, `journalctl -u <svc> -f`,
  `sudo systemctl restart <svc>`, `sudo -e /etc/<svc>/config.toml`.
- Deviation (documented): `pelcobridge2` on host shack-pc is NOT a service —
  bare binary, started interactively by the operator, no systemd unit, no
  auto-start (arming is a keyboard act; a headless always-on process would
  contradict its safety model). Seed-once 0600 config still applies.

---

## 8. Invariants & safety rules (things that must NEVER happen)

1. **Dependency points one way only.** Core components know nothing about any
   consumer and never publish under a consumer tree (`homeassistant/…`).
   Delete every consumer and the station runs identically. Accepted
   temporary deviation: flexbridge/ultrabridge carry embedded HA discovery,
   gated off (`publish_ha_discovery = false`, default false), slated for
   deletion once `hadiscovery` is proven live; with discovery off (or HA
   absent) the canonical planes are unaffected.
2. **Consumers are optional modules** — separate processes, disabled by
   default, required by no core component. HA is a reference consumer, not a
   privileged one.
3. **The operator surface is a UI-agnostic canonical topic.** Manual control
   is a plain canonical `/cmd` topic any UI can publish. HA is one writer,
   never the definition.
4. **Plane discipline:** consumers couple to state, never to intent. Never
   assume a command succeeded because it was emitted.
5. **Never retain `cmd`** except idempotent self-healing actuator setpoints;
   one-shot commands must clear the retained topic after execution. Never
   retain QSO/spot/meter events.
6. **Liveness is never a `/state` field**; `/status` is the plain-string LWT
   topic. `device_online`/`error` are device-link fields, a different thing.
7. **Never trust retained state for safety.** Safety lives on the hardware
   plane. Consumers must AND both liveness layers and check liveness before
   acting on retained state.
8. **Safety ordering contracts:** PA disarmed and confirmed before a tune
   carrier; RF inhibited and RX confirmed before a cold-switch port change;
   the power-up order mains → ~30 s network → 13.8 V → controllers online →
   TRX on → radio online → PA on → PA power confirmed → arm permit. Bridges
   only report state; the sequencer issues the ordered chain.
9. **Fail-safe defaults everywhere:** relays fail open (arm) / off (power,
   ant-switch to ground); loss of software/network/power degrades to
   inaction, not hazard.
10. **`armed` is never commanded**; the embedded firmware computes it from
    the permit AND radio conditions AND a ≤10 s heartbeat.
11. **Band is never a stored setpoint** — always derived from `freq_hz` against
    the canonical table; this removes the class of bug where band and
    frequency disagree.
12. **Adapters normalize modes** — consumers never see raw firmware mode
    strings. `freq_hz` is always an integer in Hz (never kHz/MHz/float).
13. **Slot names are canonical roles**; site/station/slot/location/host are
    config, never code constants.
14. **The sequencer is one writer but never locks** the power channels.
15. **No safety decision may depend on the messaging plane**; the reconciler
    being down degrades the station to manual, which is acceptable *only
    because* safety is hardware and fail-safe.

---

## 9. Canonical band/mode vocabulary (BEHAVIOR CONTRACT — exact tables)

`freq_hz` (integer, Hz) is the single source of truth; `band` is always
derived. Band edges are DL (Germany) / IARU Region 1 allocations, both edges
inclusive:

| Band | Low (Hz) | High (Hz) |
|------|----------|-----------|
| `160m` | 1,800,000 | 2,000,000 |
| `80m` | 3,500,000 | 4,000,000 |
| `60m` | 5,351,500 | 5,366,500 |
| `40m` | 7,000,000 | 7,300,000 |
| `30m` | 10,100,000 | 10,150,000 |
| `20m` | 14,000,000 | 14,350,000 |
| `17m` | 18,068,000 | 18,168,000 |
| `15m` | 21,000,000 | 21,450,000 |
| `12m` | 24,890,000 | 24,990,000 |
| `10m` | 28,000,000 | 29,700,000 |
| `6m` | 50,000,000 | 54,000,000 |
| `2m` | 144,000,000 | 146,000,000 |
| `70cm` | 430,000,000 | 440,000,000 |
| `23cm` | 1,240,000,000 | 1,300,000,000 (out of automation scope) |

Fallback labels: a frequency in 1.8–30 MHz outside any allocation → `gen`
(recognized, case-insensitive, accepted by band-validation helpers); anything
else outside (VHF/UHF gaps, non-HF, zero/negative) → `unknown`. Model §8.1
also mentions a `band-N` fallback form for unmatched bands.

Band centre frequencies (IARU R1; used by ultrabridge when jumping by band
name, converted to kHz internally ×1000 back to Hz): `20m` 14,175,000;
`17m` 18,118,000; `15m` 21,225,000; `12m` 24,940,000; `10m` 28,850,000;
`6m` 51,000,000.

Canonical modes — exactly six: `cw`, `usb`, `lsb`, `am`, `fm`, `data`.
Adapter-side normalization map (firmware → canonical):
`CW, CW-U, CW-L, CW_U, CW_L → cw`; `USB → usb`; `LSB → lsb`;
`AM, SAM → am`; `FM, DFM → fm`;
`DIGU, DIGL, USB-D, LSB-D, FDV, RTTY, RTTY-U, RTTY-L, FT8, FT4, PSK31, WSPR,
JS8 → data`. Digital-over-sideband modes map to `data` regardless of
sideband — canonical `mode` describes the content, not the RF carrier (a PA
consumer derates for digital duty cycle). Adapters publish a canonical mode or
omit the field; never a raw firmware string. To distinguish digital modes use
the optional `submode` field (ADIF SUBMODE token, uppercase, only when
`mode == data`) — never extend the six.

Band color codes (fixed hex, shared with the horstreporter web frontend — a
deliberate cross-component visual invariant): `160m` `#8B0000`, `80m`
`#800080`, `60m` `#4B0082`, `40m` `#0000FF`, `30m` `#03B1B1`, `20m`
`#008000`, `17m` `#808000`, `15m` `#FFA500`, `12m` `#00FFFF`, `10m`
`#FF0000`, `6m` `#FF00FF`, `4m` `#FF1493`, `2m` `#008080`, unknown/missing
`#555555` (grey fallback; never fail to the component's general muted color).
If a spot has no `band`, fall back on grey and optionally use `sourceType` as
a secondary cue (FT8/FT4 green, dxcluster cyan, RBN amber, WSPR orange) —
band color wins when present.

---

## 10. Known defects & fragilities

1. **Reconciler is a coordination single point.** If antennaselect dies, soft
   bindings (band-follow, tuner-follow, pa-follow, antenna selection) all
   stop together and retained state goes stale; the station degrades to
   manual. Accepted only because safety is hardware; supervision/restart and
   an explicit "reconciler offline" indication are open wishes.
2. **Hosts are single points.** shari fronts the HF PA, tuner, rotator, and
   Ultrabeam bridges AND hosts the reconciler + logging; its loss takes the
   cluster offline simultaneously (correctly, via LWT). Host liveness is
   load-bearing. shack-pc fronts only the VHF rotator.
3. **Retained state can be stale while acted on.** Mitigated by LWT +
   heartbeats, but any consumer acting on retained state must check both
   liveness layers first — several consumers historically keyed on `/status`
   alone (antennaselect chatter, live).
4. **`device_online` convention drift:** model §3 says the field is "omitted
   when true"; actual bridges (flexbridge, ultrabridge, acom, atr1k) publish
   `device_online: true` explicitly. Consumers must treat both forms.
5. **Doc-vs-doc wiring-map drift (code/config is truth):** the integration
   model §7.1 wiring map and CLAUDE.md say `port3: ultrabeam`,
   `port6: fan-dipole`; the model's own passive-resource list says
   "fan-dipole … (port 2)"; `antennaselect/config.example.toml` says
   `port4: ultrabeam`, `port6: fan-dipole`. The deployed config on shari is
   authoritative; the exact port numbers are per-site configuration and must
   not be hard-coded by a reimplementation.
6. **Embedded HA discovery deviation** (flexbridge/ultrabridge) still exists
   in code, gated off; slated for deletion.
7. **GenAI-authored adapter quality** is a stated risk; conformance against
   the §8.1 checklist (the nine-line list this document folds into §3–§8) is
   the mitigation. A bad adapter publishing plausible-but-wrong state can
   drive correct bindings to wrong outcomes.
8. **Idle-over-operator surprise:** walking away overrides an operator
   dummy-load hold — deliberate but surprising.
9. **Two legacy names** (`flexbridge`, `ultrabridge`) violate the naming
   convention; rename deferred (touches live systemd units).
10. **Antenna-grounding recovery gaps** (recorded in project memory, live
    behavior): auto-ground works, but recovery is fragile — flexbridge's
    change-only `/state` publishing starves the pa-arm 10 s heartbeat during
    radio-idle periods (no fresh `/state` ⇒ heartbeat goes stale ⇒ arm
    drops), and antennaselect re-activation can race; the first key-up after
    a ground can occur "into the short" before the switch settles.
11. **Paho foot-guns** (see §2): connect ignoring cancellation, and handlers
    blocking on publish — both hit live. Any reimplementation must ensure
    prompt shutdown during broker outage and non-blocking message dispatch.
12. **Plaintext 0600 secrets** accepted trade-off; systemd-creds/LoadCredential
    is the stronger option if the threat model tightens.
13. **160 m has no resonant antenna** — routes to fan-dipole via the ATU
    (`band_policy.fallback`), which will present a high SWR ("standing wave
    ratio", a mismatch measure) that the tuner must handle.
14. **Logging integration model is a proposal** — `…/log/event` etc. are
    specified but the adapter components are not yet implemented.

---

## 11. Re-implementation notes

**Must be preserved verbatim (any new stack):**

- Topic grammar `<site>/<station>/<slot>/{meta,state,status,cmd}` with the
  retention pattern (meta/state/status retained; cmd not, unless the §5.4
  idempotent exception with one-shot clearing).
- The exact four-plane payloads: `/status` plain string; `/meta` field set
  incl. schema `"1.0"`; `/state` one full JSON snapshot with RFC 3339 UTC
  `ts`; `/cmd` `{action, value}` with value always a string unless
  `expose.command.value_key` re-homes it.
- The slot address map (§1.2) — addresses are the contract; downstream
  consumers bind to them.
- The two-layer liveness model and the AND rule.
- Band/mode tables and normalization (§9), `freq_hz` integer Hz, derived
  band, `gen`/`unknown` fallbacks, band colors.
- The arbitration ladder (idle > operator > auto), derived `mode`,
  published `source`, 30-min idle ground, cold-switch contract.
- The arm formula, the 10 s heartbeat constants, fail-safe-open semantics.
- The power-sequence order and timing (30 s network delay, 120 s step
  timeout, 2 s shutdown stagger, 200 ms poll), timeout ⇒ fault, no rollback.
- Config precedence, 0600 TOML, seed-once, EnvironmentFile secret pattern,
  no secrets on any command line.
- Design invariants (§8) — especially the one-way consumer dependency and
  plane discipline.

**Free to change (implementation detail):**

- Language/framework (current: Go 1.26-ish with paho.mqtt.golang +
  pelletier/go-toml/v2; PlatformIO C++ for the M5 Stamp PLCs; ESPHome YAML
  for the ant-switch; Flutter console client).
- The shared-module mechanism (`shared/schema` helpers, `shared/mqtt`
  ctx-aware connect + jobs queue) — reproduce the behaviors (prompt
  shutdown, non-blocking handlers, serialized publish), not the code shape.
- Go workspaces, per-module `replace` directives, cross-compile details
  (any mechanism producing static arm64 binaries works).
- systemd vs. any supervisor, provided the hardening equivalent
  (dedicated user, NoNewPrivileges, ProtectSystem, ProtectHome, PrivateTmp,
  secrets confinement) is met.
- Legacy bridge names, if the rename is done consistently with the address
  map unchanged.
- Per-component internal architecture, as long as the bus behavior is
  byte-equivalent (a conformance harness can subscribe `muehle/#` and diff
  against the current station).