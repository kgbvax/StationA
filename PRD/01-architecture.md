# 01 — System architecture (stack-agnostic)

This document specifies the architecture of the Mühle amateur-radio station automation
system ("stationa") in a form that a competent software engineering team — knowing
nothing about amateur radio or the original technology stack — can use to re-construct
the system from scratch, in any programming language, with any messaging library.
**Amateur radio (ham radio)** is the licensed hobby of two-way radio communication; the
system automates a complete station: a **transceiver** (the radio, which transmits and
receives), a **power amplifier** (**PA** — boosts transmit power), an **antenna tuner**
(**ATU** — matches the antenna's electrical impedance to the feed line when the antenna
is not naturally resonant on the operating frequency; the mismatch measure is **SWR**,
standing-wave ratio), a **rotator** (a motor that turns a directional antenna), an
**antenna switch** (a 1-of-N relay selector routing the feed line to one of several
antennas), and the antennas themselves. This document is normative: behavior statements
are written as testable requirements on the wire protocol and on component behavior.
Wherever the original implementation is mentioned, it appears only in a clearly-marked
"Reference-implementation notes" sub-section as non-normative background. Sibling PRD
documents: `00-system-overview.md` (context and vocabulary), `02-interface-spec.md`
(exact topic, payload, band and mode tables), `03-components/<slug>.md` (one per
component), `04-console.md` (operator console), `05-deployment.md` (hosts, deploy),
`06-safety.md` (RF-safety invariants), `07-priorities-milestones.md`.

---

## 1. Architecture summary

### 1.1 The shape of the system

The system has **no central server**. It consists of exactly one **message broker**
plus a set of small, independent services — called **bridges** — each of which fronts
exactly one physical device, or exactly one station-wide logic role, and nothing else.
**MQTT** is a lightweight publish/subscribe protocol: clients publish short messages to
hierarchical text **topics** on a central **broker** (a server that relays messages by
topic); any client **subscribes** to topic filters and receives every matching message.
A **retained message** is one the broker stores and re-delivers immediately to every new
subscriber of that topic. **LWT (Last Will and Testament)** is an MQTT feature where the
broker publishes a pre-registered message on the client's behalf if the client disconnects
uncleanly. **QoS** (quality of service) 1 means at-least-once delivery.

Components never call each other. Integration happens in exactly two ways:

1. **Observation**: every component publishes what it is and what is currently true
   about it, as retained messages at a well-known address; every consumer subscribes and
   reads.
2. **Command**: a consumer publishes a command message to the component's command topic;
   the component reacts (or does not); the consumer then watches the component's
   published state to see what actually happened.

Requirements (normative):

- **REQ-ARCH-1.** The system SHALL consist of one MQTT broker and a set of independent
  bridge services; there SHALL be no component with a privileged or central position on
  the message path.
- **REQ-ARCH-2.** Each bridge SHALL front exactly one physical device or one logic role.
  A bridge that fronts two slots of the *same* physical device (or two roles of the same
  process) is permitted (see §2.3), but a bridge shall never front two unrelated devices.
- **REQ-ARCH-3.** Components SHALL integrate only by (a) subscribing to other
  components' published state and (b) publishing commands to other components' command
  topics. No component-to-component direct connection, RPC, database, or file exchange
  is part of the architecture.
- **REQ-ARCH-4.** The dependency graph SHALL point one way: core device bridges and
  logic bridges SHALL know nothing about any consumer and SHALL never publish under any
  consumer's topic tree. Deleting every consumer (dashboards, home-automation rendering,
  historians) SHALL leave the station operating identically. Consumers SHALL be
  separately deployable processes that no core component requires.
- **REQ-ARCH-5.** Any single bridge SHALL be replaceable, restartable, or rebuildable
  independently, as long as its on-bus behavior (topics, payloads, timings) remains
  conformant. A conforming re-implementation SHALL be indistinguishable from the original
  to a subscriber of `muehle/#`.
- **REQ-ARCH-6.** The live configuration SHALL be the documentation: the component
  inventory SHALL be readable as a live subscription to the bus (e.g. the wildcard
  `muehle/+/+/meta` yields every component's self-description). No separate inventory
  document is authoritative.
- **REQ-ARCH-7.** Loss of any one bridge SHALL degrade only the function that bridge
  provides; it shall never make the station *unsafe* (see §6.3 and `06-safety.md`: RF
  safety is enforced by hardware interlocks that the messaging layer only mirrors).

### 1.2 Why this shape

- **Independent failure.** A crashed bridge takes down one device's automation, not the
  station. The broker is the only shared element, and every component reconnects to it
  autonomously.
- **One service per device.** Each device has exactly one adapter that owns its quirks
  (serial protocol, boot time, error behavior); no other component is contaminated by
  them.
- **Rebuildability.** Because every contract is a topic string plus a JSON payload, any
  component can be rewritten in isolation and validated by diffing its bus output
  against the original's.
- **Heterogeneous hosts.** Components run on a Raspberry Pi, a shack PC, embedded
  relay controllers, and a tablet — the only thing they share is the broker.

### 1.3 Reference-implementation notes (non-normative)

The deployed system is a set of small programs in the Go language, each deployed as a
hardened service under the `systemd` supervisor on a Raspberry Pi called `shari`
(192.168.1.139), using the Eclipse Paho Go MQTT client library and a Mosquitto broker at
`tcp://192.168.1.50:1883`. Two embedded devices (the antenna switch and one relay
controller) run their own firmware that speaks the same topic schema directly over
wifi. A Flutter application on an Android tablet is a pure consumer. None of these are
requirements; the requirements are the behavior contracts in this document. The broker
MUST support retained messages, persistent storage across broker restarts, and MQTT
last-will messages (MQTT 3.1.1 semantics suffice; the deployed system uses MQTT 3.1.1
with QoS 1 everywhere).

---

## 2. The slot model

### 2.1 Slot addresses

Every controllable device or logic function on the bus is a **slot** with a topic
address:

```
<site>/<station>/<slot>
```

- **site** — the physical property: `muehle` (the station site).
- **station** — a transmitting entity: `hf` (high-frequency, roughly 1.8–54 MHz) or
  `uhf` (144–440 MHz). The station segment is structural; it carries only an inferred
  `activity` flag (§7.3) and is the unit of contention in a multi-operator future.
- **slot** — a canonical **role** name at that station. A **role** is a function
  ("radio", "tuner", "sequencer"), not a device or product name: the slot segment is
  `ant-ctrl`, never `ultrabeam`. A second instance of the same role takes a numeric
  suffix (`ant-ctrl-2`).
- An optional **position** segment (an operator seat) is reserved in the grammar but
  collapsed away in the current single-operator station; it reappears only if two
  operators share a station.

Requirements:

- **REQ-SLOT-1.** Slot addresses SHALL follow `<site>/<station>/<slot>`; the four
  planes (§3) attach as suffixes: `<addr>/meta`, `<addr>/state`, `<addr>/status`,
  `<addr>/cmd`.
- **REQ-SLOT-2.** `site`, `station`, `slot`, `location` (building label, e.g.
  `bauwagen`) and `host` (compute node label) SHALL be deployment configuration, never
  code constants. A bridge SHALL work for any site/station/slot name from its
  configuration file alone. Only the canonical vocabulary (role names, mode names,
  field names) and facts about the fronted device may be built in.
- **REQ-SLOT-3.** Site-level infrastructure that serves *across* stations SHALL sit
  outside the station path: compute nodes at `site/host/<name>`, cross-station power
  supplies at `site/power/<name>`. These are commanded like any other slot.
- **REQ-SLOT-4.** The MQTT client identifier of a component SHALL default to its slot
  address with `/` replaced by `-` (e.g. `muehle-hf-radio`), overridable by
  configuration, so that a duplicate connection is diagnosable on the broker.

### 2.2 Full slot table

This is the deployed station inventory. Addresses are the contract; consumers bind to
them. (Component details: `03-components/<slug>.md`.)

| Slot address | Canonical role | Owning component | Physical device | Host |
|---|---|---|---|---|
| `muehle/power/master` | `power` | shelly-power-bridge (one process, two slots) | Shelly smart plug — station master mains | shari |
| `muehle/power/psu-13v8` | `power` | shelly-power-bridge | Shelly smart plug — 13.8 V DC power supply (site-level; feeds HF and UHF control electronics) | shari |
| `muehle/hf/radio` | `radio` | flexbridge | FLEX-8400 transceiver (ethernet) | shari |
| `muehle/hf/ant-ctrl` | `ant-ctrl` | ultrabridge | Ultrabeam RCU-06 beam-antenna controller (USB-serial) | shari |
| `muehle/hf/ant-switch` | `ant-switch` | waveshare_relay-antswitch-bridge (embedded firmware) | 1:6 relay antenna switch (wifi) | embedded (the switch itself) |
| `muehle/hf/switch` | `switch` | m5stamp-hf-ctrl (embedded firmware) | M5 Stamp relay controller #1 — relay 4 = transceiver remote-on, relay 3 = PA remote-on | embedded |
| `muehle/hf/pa-arm` | `pa-arm` | m5stamp-hf-ctrl (same firmware, same device) | M5 Stamp relay controller #1 — relay 1 = PA arm permit | embedded |
| `muehle/hf/antenna-select` | `reconciler` | antennaselect | none — logic slot (policy engine) | shari |
| `muehle/hf/pa` | `pa` | acom1200s-pa-bridge | ACOM 1200S power amplifier (serial; telemetry only — the bridge cannot switch it) | shari |
| `muehle/hf/rotator` | `rotator` | wrc-rotator-bridge | Yaesu G-450DC rotator via AF6SA "WRC" controller (websocket) | shari |
| `muehle/hf/tuner` | `tuner` | atr1k-tuner-bridge | ATR-1000 antenna tuner (binary WebSocket over wifi) | shari |
| `muehle/hf/power-seq` | `sequencer` | powerseq | none — logic slot (startup/shutdown sequencer) | shari |
| `muehle/hf/discovery` | discovery renderer | hadiscovery | none — logic slot (passive consumer of other slots' `/meta`) | shari |
| `muehle/uhf/pol-ctrl` | `pol-ctrl` | *no firmware exists* — documented gap | M5 Stamp relay controller #2 — X-Quad antenna polarization relays | embedded (gap) |
| `muehle/uhf/rotator` | `rotator` | pelcobridge2 | PTS-303Z/3050DZ pan/tilt head (Pelco-D/P protocol over RS-485) | shack-pc (interactive program, not a service) |
| `muehle/host/shari`, `muehle/host/shack-pc` | `host` | *no component publishes these* — model-only (§10) | — | themselves |

The canonical role vocabulary (a re-implementation shall not invent new names without
cause): `radio`, `pa`, `tuner`, `ant-switch`, `ant-ctrl`, `rotator`, `pol-ctrl`,
`reconciler`, `sequencer`, `host`, `power`, `switch`, `pa-arm`, `preamp` (function;
slot or capability), `bias-feed` (likewise), `station`. A future logging layer
(specified but not implemented — §10) adds `qso-log`, `bandmap`, and later `scoreboard`.

The integration model also defines `muehle/hf/rx-loop-ctrl` (a K9AY receive-only loop
antenna's direction controller) in the hardware interlock chain (§6.3); it is not in the
deployed slot table above. See `03-components/` for per-component coverage.

### 2.3 Compound devices and multi-slot processes

- One physical device may appear under two slots: the M5 Stamp relay controller #1 owns
  both `muehle/hf/switch` and `muehle/hf/pa-arm`. The only mechanism tying the two slots
  to one device is that both carry **identical `device {model, serial}`** objects in
  `/meta` (§3.1). There is no other compound-device mechanism.
- One process may own two slots: shelly-power-bridge fronts both power slots. Each slot
  still gets its own full four-plane presence.
- Embedded nodes (the antenna switch, the relay controllers) collapse device, adapter,
  and host into one node whose firmware speaks the canonical schema directly over
  wifi MQTT.

### 2.4 What is deliberately NOT a slot: passive resources

Antennas, masts, and masthead preamplifiers are **passive resources**: named, referenced
by configuration, with **no MQTT presence at all** — no topics, no state, nothing to
subscribe to. The station's antennas are:

- `ant/ultrabeam` — a steerable directional beam antenna ("yagi") on a mast, actively
  tuned by the `ant-ctrl` slot;
- `ant/fan-dipole` — a fixed multi-band wire antenna, resonant on the 80 m and 40 m
  bands;
- `ant/dummy-load` — a heat-dissipating test load that radiates nothing (used for
  testing);
- `ant/k9ay-loop` — a receive-only loop antenna;
- `ant/xquad-2m`, `ant/xquad-70cm` — UHF quad antennas;
- masts `mast/ta16-hf`, `mast/ta16-vhf`, and masthead preamplifiers `preamp/2m`,
  `preamp/70cm`.

Passive resources exist in exactly one place: the antenna-selection reconciler's
**wiring map** — configuration (never code) that maps antenna-switch port keys to
resource names (`port1: dummy-load`, `port6: fan-dipole`, `off: grounded`, and the
ultrabeam port — see §10 for the port 3 / port 4 conflict). A **controller map**
(configuration `[band_follow] resource + slot`) maps resource name to controller slot
(e.g. `ultrabeam → ant-ctrl`) so that band-following needs no antenna names in code.
Requirements:

- **REQ-RES-1.** Passive-resource names SHALL appear only in site configuration, never
  in component code.
- **REQ-RES-2.** No component SHALL publish or subscribe under `ant/*`, `mast/*`, or
  `preamp/*`.
- **REQ-RES-3.** The wiring map SHALL be the single editable place where the antenna
  arrangement lives; port numbers SHALL be configuration, never hard-coded (see §10,
  open decision OD-1).

---

## 3. The four planes

Every slot has exactly four topic suffixes — one per **plane** (a plane is one of the
four standardized topic suffixes with fixed semantics):

```
<addr>/meta     RETAINED    birth certificate: identity + capabilities
<addr>/state    RETAINED    live state as ONE JSON snapshot document
<addr>/status   RETAINED    plain string "online" | "offline" — liveness (LWT topic)
<addr>/cmd      NOT retained (default)   intent, bus → component
```

- **REQ-PLANE-1.** Every slot SHALL expose exactly these four suffixes; read-only slots
  omit `/cmd`. No other sub-topics are part of the current contract (a non-retained
  `event` sub-topic for a logging layer is specified but not implemented; §10).
- **REQ-PLANE-2.** All publishes and subscribes SHALL use QoS 1 (at-least-once). A
  command that must never double-fire MAY use QoS 2 (exactly-once); none currently does.
- **REQ-PLANE-3.** `/meta`, `/state` and `/status` SHALL be retained; `/cmd` SHALL NOT
  be retained except under the idempotent-actuator exception of §5.4.
- **REQ-PLANE-4.** All payloads SHALL be UTF-8 JSON, except `/status`, which is a bare
  string (§3.4). The one exception: `/status` is NOT JSON.

### 3.1 `/meta` — the birth certificate

Retained JSON, re-published on **every (re)connect** (re-birth). Field set:

| Field | Type | Required | Semantics |
|---|---|---|---|
| `schema` | string | yes | currently `"1.0"` |
| `role` | string | yes | canonical role name (§2.2) — never a device/product name |
| `device` | object | device slots | `{model, serial?, firmware?}`. Logic slots omit it (or omit `serial`/`firmware`). Two slots of one physical device carry identical `device{model,serial}` (§2.3) |
| `link` | string | device slots | transport hint: `ethernet` \| `serial` \| `wifi` \| `embedded`; `none` for logic slots |
| `location` | string | yes | physical building label, from configuration (e.g. `bauwagen`) |
| `host` | string | yes | compute node the adapter runs on, from configuration; embedded nodes name themselves |
| `capabilities` | object | yes | the discovery contract (below) |
| `expose` | object | no | consumer-neutral field/action surface (below) |

Worked example (`muehle/hf/radio/meta`):

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

**Capabilities** are the binding contract: consumers bind to a *declared capability*
(`bands`, `modes`, `ports`, `exclusive`, `hot_switch`, `axes`, `feeds`, `channels`,
`relay_map`, `fail_safe`, `heartbeat`, `key_input`, `max_power_w`, `inline`,
`tune_modes`, `directions`, `ladder`, `band_source`, …), never to a device model. The
same function may appear as a standalone slot or absorbed as a capability — bind to the
function, discover where it lives. The exact key set per role is in
`02-interface-spec.md` §3.

**`expose`** is an optional block describing the slot's observable and controllable
fields with zero consumer-specific vocabulary (no home-automation strings, no
templates) so that any consumer can render its own representation. It declares, per
field: `key` (the JSON key in `/state`), `name`, `type` (`number`|`enum`|`boolean`|
`string`), optional `unit`/`class`/`state_class`/`options`/`options_ref`/`min`/`max`/
`step`/`on`/`off`, and for writable fields a **command descriptor**
`{action?, value_key?, value_type?}` that states exactly how a command to this field is
encoded on `/cmd` (§5.1). Full field tables: `02-interface-spec.md` §4.

### 3.2 `/state` — one retained JSON snapshot

- **REQ-STATE-1 (one-snapshot rule).** A slot's `/state` SHALL be a single complete
  JSON document per publish — never partial updates, never per-field topics. Last
  write wins; a subscriber always holds a complete, consistent view; a late joiner
  gets the whole picture from the retained copy without polling.
- **REQ-STATE-2.** Every snapshot SHALL carry `ts`: an RFC 3339 UTC timestamp string
  (e.g. `"2026-07-06T12:34:56Z"`) of this publish.
- **REQ-STATE-3 (change-only publishing).** A component SHALL publish `/state` only
  when a field value changes. This is the default cadence — the bus is quiet when
  nothing changes.
- **REQ-STATE-4 (heartbeat exception).** Components that are inputs to heartbeat-driven
  safety logic SHALL additionally republish an unchanged `/state` on a fixed period so
  that absence is detectable: the PA-arm relay controller republishes identical state at
  least every 10 s. See REQ-TX-3 for the radio-side heartbeat requirement.
- **REQ-STATE-5.** Device slots SHALL set `device_online` (boolean) to `false` and
  `error` (human-readable string) when the fronted hardware is unreachable while the
  bridge itself is up. The `device_online`-when-true form is an open decision (OD-4):
  consumers MUST treat absence and explicit `true` as equivalent.
- **REQ-STATE-6.** Liveness SHALL NEVER be a field inside `/state`. A dead node cannot
  update its own state document; that is exactly why liveness is a separate topic
  (§3.4).
- **REQ-STATE-7.** `freq_hz` SHALL always be an integer in Hz (never kHz, MHz, or a
  float); `band` SHALL always be *derived* from `freq_hz` against the shared band
  table (`02-interface-spec.md` §2), never a stored setpoint, so band and frequency can
  never disagree. Modes SHALL be normalized by the adapter to the six canonical names
  (`cw`, `usb`, `lsb`, `am`, `fm`, `data`); consumers never see raw firmware strings.

Worked example (`muehle/hf/radio/state`):

```json
{ "ts": "2026-07-06T12:34:56Z", "freq_hz": 14025000, "band": "20m",
  "mode": "cw", "tx": "rx", "tuning": false, "drive": 40, "rx_input": "ant1" }
```

Per-slot field sets are in `02-interface-spec.md` §5 and each `03-components/` file.

### 3.3 `/status` — liveness

- **REQ-STATUS-1.** The payload SHALL be the bare string `online` or `offline` — not
  JSON — retained, QoS 1. (This lets home-automation availability integrations map it
  with no template.)
- **REQ-STATUS-2.** A component SHALL register a retained last-will (`offline`) at
  every connect, publish retained `online` once up on every (re)connect, and publish
  retained `offline` itself on clean shutdown. The broker publishes `offline`
  automatically when a node drops uncleanly.
- **REQ-STATUS-3.** Because a clean shutdown does not always produce the component's
  own final `offline` (see §4.3), consumers SHALL NOT treat a retained `online` as
  proof that the component is running.

### 3.4 Plane-adjacent topics (specified, not implemented)

A future logging layer adds a non-retained `event` sub-topic per logging slot
(`<station>/log/event`, `<station>/spots/event`) with retained
`log/{meta,status,state}`. **No component implements this today**; it is future scope
(§10, and `07-priorities-milestones.md`). High-rate streams (audio, spectrum, IQ data)
are explicitly *not* on the bus.

---

## 4. The two-layer liveness model

### 4.1 The two layers

There are two distinct liveness notions, deliberately on two different planes:

1. **Bridge liveness** — `/status` (the LWT topic). Answers: *is the software component
   connected to the broker?* Set `online` at connect, `offline` by will on unclean loss.
2. **Device-link liveness** — the boolean `device_online` field *inside* `/state`
   (device slots only). Answers: *is the hardware behind the running bridge reachable?*
   The bridge stays `online` while, say, the radio's ethernet link is down; only
   `device_online` goes `false`, conventionally with a human-readable `error` string.

- **REQ-LIVE-1.** A consumer SHALL combine both layers with a logical AND before
  trusting a device slot's state-derived fields: `usable = (/status == "online") AND
  (device_online is true or absent)`. Keying on `/status` alone is a defect with a live
  incident behind it: the antenna-selection reconciler originally keyed on `/status`
  alone, and when the radio's link died while the bridge stayed up, it flapped the
  antenna selection on stale/empty state ("chatter"). The fix — the AND gate plus the
  empty-band-holds rule (§7.3) — is regression-tested in all four combinations and both
  arrival orders of the retained messages.
- **REQ-LIVE-2.** A consumer that waits on state SHALL require the target's `/status`
  to be currently `online` as an implicit precondition, so a dead device cannot satisfy
  a wait on a stale retained `/state` (the sequencer's `wait_state` does exactly this;
  §6.1).
- **REQ-LIVE-3.** A `wait_status`-style condition SHALL distinguish *absence* from
  *offline*: waiting for `offline` requires an actual `offline` payload and can never
  pass on a slot that has never published.
- **REQ-LIVE-4.** **Never trust retained state for safety.** Retained values can be
  stale after a crash. Safety lives on the hardware plane (§6.3, `06-safety.md`); the
  messaging layer only mirrors it.

### 4.2 Heartbeats

Liveness of *inputs* is enforced by consumers, not by the broker:

- The PA-arm relay firmware drops its arm permit if the radio's `/state` has not
  been received within **10 s** (constant `RADIO_HEARTBEAT_MS = 10000`).
- The relay firmware republishes its own `/state` at least every **10 s** even when
  unchanged, so *its* consumers can detect its death.
- **REQ-TX-3 (radio state heartbeat — hard requirement from a live incident).** The
  radio bridge SHALL republish `/state` periodically at least every **5 s** while the
  radio link is up — not only on change — or provide an equivalent liveness mechanism.
  The original bridge published change-only; during quiet reception no messages
  arrived; the PA-arm heartbeat silently expired and the arm dropped even though
  everything was healthy. The receiver-side 10 s window and the ≤5 s republish
  requirement are both normative.

### 4.3 The clean-shutdown retained-"online" subtlety

Actual deployed behavior, which a re-implementation must reproduce *and* consumers must
survive: **on a clean process shutdown, the broker does not fire the last will.** The
component is expected to publish `offline` itself before disconnecting, but if it is
stopped in a way that skips that final publish (a supervisor stop that kills it, a bug,
a host power cut), the retained `/status` stays `"online"` for a service that is no
longer running. Consequences:

- **REQ-LIVE-5.** Consumers SHALL NOT treat a retained `/status == "online"` as proof
  of a running component; it proves only that *some* recently-live instance published
  it. Freshness comes from heartbeat republishes (§4.2) and from the `/state.ts`
  timestamp.
- **REQ-LIVE-6.** A supervisor stopping a component cleanly SHOULD let the component
  publish its own retained `offline` before termination (grace period ≥ 250 ms in the
  reference deployment).

---

## 5. Command model

### 5.1 The value-key convention

Commands are JSON on `<addr>/cmd`, QoS 1, not retained by default. The universal
envelope:

```json
{ "action": "<action>", "value": "<argument>" }
```

- **REQ-CMD-1.** The argument SHALL ride under the JSON key **`value`** — never under
  a key named after the action. (The original ATR-1000 tuner bridge shipped
  `{"action":"set_inline","set_inline":"true"}` live; this convention was centralized
  to end that class of bug.)
- **REQ-CMD-2.** `value` SHALL be a JSON **string** on the wire — booleans as `"true"`
  / `"false"`, numbers as their decimal string — parsed by the receiving bridge.
  Deviations currently on the bus: (a) the antenna controller's `frequency` command
  carries an integer under `freq_hz` (see REQ-CMD-3); (b) the deployed tuner bridge
  accepts and the deployed reconciler sends a JSON boolean for
  `{"action":"set_inline","value":true|false}` (verified in code; see OD-9).
- **REQ-CMD-3 (deviation authority).** The command descriptor in the slot's `/meta`
  `expose` block is the **authority** for which key carries the argument and whether
  an `action` key exists. Three sanctioned shapes:
  1. `{action, value_key:"value"}` → `{"action":"<action>","value":"<string>"}` (the
     default);
  2. `{action, value_key:"freq_hz", value_type:"int"}` →
     `{"action":"frequency","freq_hz":14025000}` (the antenna controller);
  3. `{value_key:"select", value_type:"string"}` with no `action` →
     `{"select":"port4"}` (the antenna switch).
  A re-implementation of a bridge SHALL accept exactly its declared shape.
- **REQ-CMD-4.** A bridge receiving an unknown action, an invalid payload, or
  malformed JSON SHALL log it, drop it, and keep running — never crash on bad intent.

### 5.2 Fire-and-observe

- **REQ-CMD-5 (plane discipline).** Commands are **fire-and-observe**: the sender
  SHALL NOT assume a command succeeded because it was emitted. Confirmation is the
  target's subsequent `/state`. Consumers react to **state**, never to intent.
- **REQ-CMD-6 (no acknowledgment plane).** Commands are never acknowledged. There is
  no reply topic, no request/response pattern. The only observable acknowledgment is
  the retained state change. A reconciler that wants confirmation re-issues intent
  when the observed state disagrees with its target (§7.2).

### 5.3 Retention policy

- **REQ-CMD-7 (default).** `/cmd` SHALL NOT be retained. A retained command
  re-delivers on every reconnect, possibly with no operator behind it.
- **REQ-CMD-8 (idempotent-actuator exception).** `/cmd` MAY be retained when
  re-applying the same command on reconnect produces the same physical outcome —
  steady-state setpoints of idempotent actuators: power plugs `set_power`, remote-on
  relays `set_pa`/`set_trx`, the arm permit `set_enabled`, the antenna-switch
  `select`. Retention gives these actuators **self-heal**: after any reconnect they
  re-apply the last commanded steady state from the retained command.
- **REQ-CMD-9 (one-shot clearing).** A one-shot physical command that is retained for
  any reason (e.g. the antenna controller's `retract`, retained so it survives a
  link outage) SHALL be cleared by publishing an **empty retained payload** on the
  same topic immediately after execution, so it does not re-fire on reconnect.
  One-shot commands like the tuner's `tune` SHALL NOT be retained at all.
- **REQ-CMD-10 (operator one-shots).** A sequencer's own `/cmd` SHALL be published
  not-retained by senders and subscribed at QoS 0 (see §6.1 for why: a queued start
  command replayed after a restart would re-energize the station). A component restart
  SHALL NOT emit any command until an explicit operator command arrives.

### 5.4 Per-slot command surfaces (summary)

Exact payloads and semantics per component are in `02-interface-spec.md` §6 and the
`03-components/` files. The deployed surfaces, for orientation:

| Slot | Commands (retained unless noted) |
|---|---|
| `power/master`, `power/psu-13v8` | `set_power {on\|off}` (retained) |
| `hf/radio` | `set_band {label}` (restores the radio's persisted per-band frequency/mode), `set_freq_hz`, `set_mode`, `set_drive`, `select_rx`, `tune {start\|stop}` (only the sequencer may emit; operators ask the sequencer), `dvk_play {1..12}` / `dvk_stop {id\|active}` (voice keyer one-shots), `set_mic_profile {name}` (not retained) |
| `hf/pa` | `set_band`, `set_mode {operate\|standby}`, `clear_fault` |
| `hf/ant-switch` | `select {off\|port1..port6}` (retained; `{"select":"…"}` shape, no `action` key) |
| `hf/tuner` | `set_inline {true\|false}`, `tune {full\|mem}` (not retained) |
| `hf/switch` | `set_pa {on\|off}`, `set_trx {on\|off}` (retained, idempotent) |
| `hf/pa-arm` | `set_enabled {true\|false}` (retained steady-state permit); `armed` is NEVER commanded (§6.3) |
| `hf/power-seq` | `start`, `stop` (not retained, QoS 0) |
| `hf/ant-ctrl` | `frequency` (int under `freq_hz`), `band`, `direction`, `retract` (retained then cleared per REQ-CMD-9) |
| `hf/antenna-select` | `request {port1..port6\|off\|auto}` (retained operator hold; §7.3) |
| `hf/rotator` | azimuth set/park |
| `uhf/rotator` | `stop` only — no remote motion exists by design; arming is a local keyboard act |
| `hf/discovery` | none (passive consumer) |

---

## 6. Data flows

Three concrete end-to-end walks. Topics and payloads are exact; timings are the
deployed defaults (all are configuration — `05-deployment.md`, `03-components/`).

### 6.1 Flow A — operator presses START: sequencer → power → radio

The operator (any UI: a dashboard button, a home-automation tile, the console) publishes
one command to the sequencer. The sequencer — a logic slot, no device — runs a
configuration-driven ordered step list, pausing for settle delays and for explicit
liveness/telemetry confirmation from each dependent device before proceeding. Ordering
matters: energizing a PA (power amplifier) before control logic is up, or removing
power without inrush staggering, can damage hardware; devices take tens of seconds to
boot after their mains is switched.

Sequence (exact shipped default; step names are user-visible in `/state.step` and fault
strings):

1. Sequencer receives `{"action":"start"}` on `muehle/hf/power-seq/cmd` — not retained,
   subscribed at QoS 0 (a command queued across a restart and replayed would
   re-energize the station — REQ-CMD-10). Honored only when the sequencer's published
   `phase` is `idle`; phase becomes `starting`.
2. `master-on` — publish `{"action":"set_power","value":"on"}` (retained) to
   `muehle/power/master/cmd` (the station master mains smart plug). Everything loses
   and regains power with this plug.
3. `network-delay` — wait 30 s (default `network_delay_s`): the network, broker, and
   the wifi of plug-in devices come up only after master mains returns.
4. `psu-on` — publish `{"action":"set_power","value":"on"}` (retained) to
   `muehle/power/psu-13v8/cmd` (the 13.8 V DC supply feeding all control electronics).
5. `wait-controllers-online` — wait until the `/status` of `muehle/hf/switch`,
   `muehle/hf/pa-arm`, and `muehle/hf/ant-switch` are all `online` (they boot on the
   13.8 V supply), polling every 200 ms, deadline 120 s (default `step_timeout_s`).
   Timeout → fault (below).
6. `trx-on` — publish `{"action":"set_trx","value":"on"}` (retained) to
   `muehle/hf/switch/cmd` (closes relay 4, the transceiver's remote-power-on trigger).
7. `wait-radio-online` — wait until `muehle/hf/radio/status` is `online` (deadline
   120 s).
8. `pa-on` — publish `{"action":"set_pa","value":"on"}` (retained) to
   `muehle/hf/switch/cmd` (relay 3, PA remote-on).
9. `wait-pa-power-on` — wait until the PA's `/state` field `power == "on"` **AND** its
   `/status` is currently `online` (deadline 120 s) — the implicit liveness
   precondition (REQ-LIVE-2) prevents a dead PA from passing on a stale retained
   `/state`.
10. `pa-arm-enable` — publish `{"action":"set_enabled","value":"true"}` (retained) to
    `muehle/hf/pa-arm/cmd`. Last step → `phase` becomes `running`.

```mermaid
sequenceDiagram
    participant Op as Operator (any UI)
    participant Seq as power-seq (sequencer)
    participant M as power/master (smart plug)
    participant PSU as power/psu-13v8 (smart plug)
    participant SW as hf/switch (relay board)
    participant ARM as hf/pa-arm (relay board)
    participant R as hf/radio (radio bridge)
    participant PA as hf/pa (PA bridge)
    Op->>Seq: /cmd {"action":"start"} (QoS 0, not retained)
    Note over Seq: phase=idle → starting
    Seq->>M: /cmd {"action":"set_power","value":"on"} (retained)
    Note over Seq: delay "network" = 30 s
    Seq->>PSU: /cmd {"action":"set_power","value":"on"} (retained)
    M-->>Seq: /status online (retained, replayed)
    ARM-->>Seq: /status online
    SW-->>Seq: /status online
    Note over Seq: wait-controllers-online passed (poll 200 ms, deadline 120 s)
    Seq->>SW: /cmd {"action":"set_trx","value":"on"} (retained)
    R-->>Seq: /status online
    Note over Seq: wait-radio-online passed
    Seq->>SW: /cmd {"action":"set_pa","value":"on"} (retained)
    PA-->>Seq: /state {"power":"on"} (AND /status online)
    Note over Seq: wait-pa-power-on passed
    Seq->>ARM: /cmd {"action":"set_enabled","value":"true"} (retained)
    Note over Seq: phase=running, step=""
    Seq-->>Op: /state {"phase":"running"} (retained; the UI observes state, never assumes success)
```

Shutdown is the exact reverse with a 2 s stagger (default `shutdown_stagger_s`) between
power removals, to stagger the electrical **inrush current** (the surge when an
inductive load is energized) of switching: `pa-arm set_enabled false` → stagger →
`set_pa off` → stagger → `set_trx off` → stagger → `psu-13v8 off` → stagger →
`master off`. The default shutdown deliberately has no waits — it must make progress
even if devices are already dead. Completing it clears any fault and returns
`phase=idle`.

Error behavior (normative):

- **REQ-SEQ-1.** Any step failure — wait timeout (`timeout`), broker disconnect
  (`broker disconnected`), publish failure including the 10-second publish bound
  (`publish failed: …`), or process shutdown (`interrupted`) — SHALL immediately set
  `phase=idle` with `fault = "<step>: <reason>"` and `step=""`, and perform **no
  rollback**: driven slots keep their last retained `/cmd`. Recovery is re-running
  `start` (every step is idempotent steady-state intent) or running `stop` from
  `idle`-with-fault (teardown of a half-started station).
- **REQ-SEQ-2.** Exactly one sequence SHALL run at a time; commands arriving during a
  run are dropped and logged. `start` is honored only from `phase=idle`; `stop` is
  honored from `phase=running` or from `phase=idle` with a fault. There is no abort
  command (a deliberate contract; see OD-8).
- **REQ-SEQ-3.** On process start the sequencer SHALL publish a fresh retained idle
  `/state` (overwriting any stale retained snapshot from a crashed predecessor) and
  emit **no commands** until an operator `start` arrives.
- **REQ-SEQ-4.** On every (re)connect the sequencer SHALL republish `/status=online`,
  `/meta`, and its retained `/state`.

### 6.2 Flow B — operator picks a band: antenna-select → switch / beam / tuner / PA

A **band** is the wavelength name of the operating frequency (e.g. `20m` names the
14 MHz amateur allocation; the exact band-edge table is in `02-interface-spec.md`
§2). The operator selects a band on the radio (or any UI publishes the radio's
`set_band` command, which restores the radio's persisted frequency/mode for that band).
The radio bridge republishes `/state` with the new `freq_hz` (integer, Hz) and the
derived `band`. Everything else follows by observation:

```mermaid
sequenceDiagram
    participant Op as Operator
    participant R as hf/radio (radio bridge)
    participant AS as hf/antenna-select (reconciler)
    participant SW as hf/ant-switch (switch firmware)
    participant AC as hf/ant-ctrl (beam controller bridge)
    participant PA as hf/pa (PA bridge)
    participant TU as hf/tuner (ATU bridge)
    Op->>R: /cmd {"action":"set_band","value":"20m"}
    R-->>R: tunes; band derived from freq_hz
    R-->>AS: /state {"freq_hz":14025000,"band":"20m","tx":"rx"} (retained)
    Note over AS: ladder tier 3 (auto): band 20m → resource "ultrabeam" → port key
    AS-->>AS: /state {"mode":"auto","target":"portX","source":"auto"} (retained, only on decision change)
    AS->>SW: /cmd {"select":"portX"} (retained; withheld while tx=="tx")
    SW-->>AS: /state {"selected":"portX","settled":true} (retained)
    Note over AS: mismatch ⇒ re-assert on next input change
    AS->>AC: /cmd {"action":"frequency","freq_hz":14025000} (retained; only while ultrabeam is the selected port)
    AS->>PA: /cmd {"action":"set_band","value":"20m"} (NOT retained; not gated on antenna or activity)
    AS->>TU: /cmd {"action":"set_inline","value":false} (NOT retained; false on 20m — resonant)
    Note over AC,PA: each target confirms by republishing /state; nobody assumes success
```

Exact emission rules (normative; the reconciler is stateless — every decision is a pure
function of configuration plus latest inputs):

- **REQ-SEL-1 (ladder).** On every input change the reconciler SHALL resolve, by the
  fixed priority ladder **idle > operator > auto** (§7.3), one switch target and
  publish it as `{"ts","mode","target","source"}` on
  `muehle/hf/antenna-select/state`, only when the decision triple changes.
- **REQ-SEL-2 (two-layer gate).** Radio-derived fields SHALL only be acted on when
  `radio/status == "online"` AND `radio/state.device_online` is true (or absent).
  If either layer drops after a selection, the reconciler SHALL hold the last
  selection (no chatter, no grounding from liveness alone).
- **REQ-SEL-3 (empty band holds).** An empty `band` string SHALL hold the last
  selection and emit nothing. (It is the radio bridge's transient
  "reconnecting, no slice reported yet" state; resolving it to the fallback chattered
  the antenna on every reconnect cycle. Regression-tested.)
- **REQ-SEL-4 (fallback).** A known-but-unmatched band (e.g. `160m`, or the
  out-of-allocation marker `gen`) SHALL map to the configured fallback resource (at
  Mühle: the fan-dipole, reached via the antenna tuner).
- **REQ-SEL-5 (cold-switch deferral).** A wanted port change SHALL be withheld while
  `radio/state.tx == "tx"` (the switch is not rated for switching under RF power —
  `hot_switch: false`), logged once, and emitted on the return to `rx`. The reconciler
  owns ordering; enforcement is a hardware interlock (§6.3).
- **REQ-SEL-6 (confirm/re-assert).** The reconciler SHALL treat the switch's reported
  `selected` position as ground truth: while it is unknown, at most one command per
  target; once known, any mismatch with the target re-issues the command on the next
  input change (a manual override of the switch is thus re-asserted).
- **REQ-SEL-7 (band-follow).** A frequency intent
  `{"action":"frequency","freq_hz":N}` (retained) SHALL be emitted to the followed
  resource's controller slot (default `ant-ctrl`) only while that controller's
  resource is the currently selected port, the radio is two-layer online, and
  `freq_hz > 0`, deduplicated against the last frequency pushed. (Never tune a beam
  that is not in circuit.)
- **REQ-SEL-8 (PA follow).** `{"action":"set_band","value":"<band>"}` (NOT retained)
  SHALL be emitted to the PA slot whenever the radio is online and reports a band —
  deliberately NOT gated on antenna selection or station activity (the PA is always
  in the RF path; its hot-switch protection is hardware). Deduplicated against the
  last band pushed.
- **REQ-SEL-9 (tuner follow).** `{"action":"set_inline","value":…}` (NOT retained)
  SHALL be emitted to the tuner slot when the selected resource is the tuner's
  resource and the band is in the configured non-resonant set (default
  `["30m","60m","80m","160m"]`) — `true` puts the ATU in the RF path, `false` bypasses
  it (including when idle-grounded, since target `off` is not the tuner's port).
  Deduplicated. Self-heal for the non-retained PA/tuner commands comes from the
  reconciler re-resolving on the retained radio-state replay at its own reconnect —
  not from retained commands.

### 6.3 Flow C — transmit safety: the arm-permit formula

This flow is the reason the system is safe to leave automated. **Safety does not live
on the messaging plane.** A physical TX-inhibit line runs in series through the RF
components; any open link inhibits transmit — fast, local, fail-safe. The messaging
layer only *mirrors* each link as read-only state and adds one slowly-changing
**arm permit**:

```
armed = enabled ∧ radio_online ∧ ¬radio.tuning ∧ band_safe ∧ heartbeat ∧ antenna_ready
```

The arm-permit relay is **fail-safe open**: its coil is de-energized in the safe state,
so loss of software, network, or 13.8 V power removes the permit. The hardware chain
(enforced locally; software mirrors only):

```
13.8 V PSU → relay controllers + ant-switch (they boot on this supply) → radio (remote-on) → PA (remote-on)
radio TX-low line → rx-loop-ctrl (preamp off) → ant-ctrl (inhibit while moving) → pa-arm → PA
```

Walk:

1. The sequencer's last startup step publishes the retained permit
   `{"action":"set_enabled","value":"true"}` to `muehle/hf/pa-arm/cmd`. This is
   `enabled` — a *permit*, never an arm.
2. The relay-controller firmware (`muehle/hf/pa-arm`) itself computes `armed` — it is
   **never commanded**:
   - `enabled` — last `set_enabled` permit (retained, idempotent, self-healing);
   - `radio_online` — the radio bridge's liveness per the two layers;
   - `¬radio.tuning` — the radio is not running a tuning carrier;
   - `band_safe` — the radio's band is in the firmware's safe-band set;
   - `heartbeat` — a radio `/state` message arrived within **10 s** (§4.2);
   - `antenna_ready` — the antenna switch is on a valid port (not grounded/`off`).
3. The firmware combines the permit with the fast hardware key line (a physical AND):
   normal transmit timing rides the hardware edge; software only arms/disarms.
4. It publishes `/state {"enabled":…, "armed":…}` and republishes an unchanged
   snapshot at least every **10 s** (its own heartbeat).

- **REQ-TX-1.** `armed` SHALL be computed by the actuator firmware, never commanded.
- **REQ-TX-2.** The arm relay SHALL be fail-safe open (de-energized = permit absent);
  the antenna switch's `off` position SHALL short open antenna ports to ground
  (lightning protection); the smart plugs' power-on default SHALL be off
  (`fail_safe: off`). Loss of software, network, or power SHALL degrade to inaction,
  not hazard.
- **REQ-TX-3.** (stated at §4.2) The radio bridge SHALL republish `/state` at least
  every 5 s while the link is up. The original change-only publishing starved the
  10 s heartbeat during idle-but-healthy periods and silently dropped the arm — a
  live incident; this requirement is derived from it and is not optional.
- **REQ-TX-4.** Ordering contracts SHALL hold: the PA disarmed **and confirmed**
  before any tuning carrier; RF inhibited and receive confirmed before any antenna
  port change; arming always the last startup action and disarm always the first
  shutdown action.
- **REQ-TX-5.** No safety decision SHALL depend on the messaging plane. Killing the
  reconciler or any bridge degrades the station to manual operation, which is safe.

A known live defect to carry forward honestly (documented, not fixed; see OD-10):
because re-arming station activity is itself a transmission (§7.3), the first key-up
after an idle grounding can occur into the still-grounded (shorted) switch port before
the deferred port change fires at un-key. The PA stays disarmed via `antenna_ready`,
but the transceiver keys into a short. A re-implementation shall treat a recovery
path free of this defect as a design goal.

---

## 7. Coordination patterns

### 7.1 Policy vs actuator: the reconciler split

Actuators are dumb: they apply commands and report state. Policy — *which* actuator
state is wanted and *why* — lives in a separate **reconciler** (a stateless, pure
decision component, the `reconciler` role). When multiple commanders want one
actuator, one arbiter resolves by priority and emits one intent stream.

- **REQ-COORD-1.** The reconciler SHALL be a pure function of configuration + latest
  observed inputs: no internal mode switch, no command memory beyond the operator
  hold input.
- **REQ-COORD-2.** The reconciler SHALL publish *why*: its `/state` carries
  `source ∈ {idle, operator, auto}` — the tier that won — so the bus documents the
  reason for every actuator position.
- **REQ-COORD-3.** `mode` SHALL be derived — `manual` while an operator hold is
  active, else `auto` — never a separately commanded switch.
- **REQ-COORD-4.** The reconciler SHALL NOT be part of any enforcement path; its death
  degrades the station to manual. It is a coordination single point: if it dies, all
  soft bindings (antenna selection, band-follow, PA-follow, tuner-follow) stop together
  and its retained state goes stale. This is accepted only because safety is hardware
  (§6.3); a supervision/restart policy and an explicit "reconciler offline" operator
  indication are open wishes (OD-11).

### 7.2 Sequencer: one writer, no locking

The sequencer owns the ordered startup/shutdown chain (§6.1) but **never locks** the
slots it drives: any channel stays directly commandable for troubleshooting while the
sequencer is idle, and the retained steady-state commands give each actuator self-heal
(§5.3). Confirmation is by observation: wait steps poll the targets' actual published
liveness and state (200 ms poll cadence, 120 s default deadline, optional
continuous-hold debounce window — default 0 = edge-triggered; an explicit `hold_ms = 0`
always means edge-triggered).

### 7.3 Arbitration ladder and idle grounding

The reconciler's fixed three-tier ladder (order is fixed in code, not configuration):

```
Tier 1  idle      station activity == inactive      → target = off (grounded), source = idle
Tier 2  operator an operator hold is active          → target = the held port,  source = operator
Tier 3  auto      band policy(radio.band)            → target = resolved port,  source = auto
```

- Highest asserting tier wins. **Tier 1 wins over an operator hold** — deliberate
  walk-away safety: a forgotten operator hold cannot keep an antenna hanging live.
  An operator hold still asserts while the radio is offline, and `mode` stays
  `manual` while a hold is active even when tier 1 wins the target.
- **Station activity is inferred, never operator-set**: a radio `/state` message whose
  `freq_hz` differs from the last-seen value, or whose `tx == "tx"`, marks the station
  `active` and resets the idle clock. Unknown activity is treated as `active`.
- **Idle grounding**: after 30 idle minutes (default `idle.timeout_minutes = 30`),
  checked on a 5 s cadence, the reconciler selects the switch's `off` position, which
  shorts all antenna feeds to electrical ground — lightning / walk-away protection.
  Grounding lands within ~5 s of the deadline.
- **Re-arm**: the only re-activation trigger is a radio `/state` message with a changed
  `freq_hz` or an actual transmission. Operator commands do not count as activity.
  Known recovery fragilities (all documented live behavior, candidates for redesign —
  OD-10): no manual (non-TX) re-arm path exists; a deferred-during-TX select can leave
  the first key-up after grounding going into the short; the TX deferral has no
  timeout; a reconciler restart re-births `lastActivity = now` and therefore always
  un-grounds the station and resets the 30-minute clock.
- **REQ-GROUND-1.** The reconciler SHALL ground the station to `off` after the
  configured idle timeout with no VFO (**VFO** — variable-frequency oscillator; the
  radio's tuning control, hence "frequency change") activity and no transmission.
- **REQ-GROUND-2.** Grounding SHALL override an operator hold.
- **REQ-GROUND-3.** The station node (`muehle/hf`, `muehle/uhf`) SHALL carry the
  inferred `activity {active|inactive}` flag as its single non-structural field,
  derived by the reconciler.

### 7.4 Cold-switch ordering contract

- **REQ-COLD-1.** For any switch declaring `hot_switch: false` (the antenna switch
  does), a port change SHALL be preceded by RF-inhibited conditions: the reconciler
  withholds during TX; hardware interlocks enforce.
- **REQ-COLD-2.** A tuning carrier SHALL be preceded by a disarm-and-confirm of the
  PA. There is no hardware tune line, so the TUNE action is routed through the
  automation: operators publish tune requests to the sequencer, never directly to the
  radio, so disarm always leads the carrier.

---

## 8. Runtime-library constraints (any stack must respect these)

These constraints exist because of production incidents with the reference stack's
MQTT client library, but they are **stack-independent requirements**: whatever
messaging library the new implementation uses, its threading and failure model must
satisfy them. Rationale is quoted from the live incidents.

1. **REQ-RT-1 (handlers never block or publish).** Inbound-message handler code SHALL
   NOT block, do long work, or call a blocking publish on the messaging client's
   delivery thread. Handlers SHALL only capture the message and enqueue work.
   *Rationale (live incident):* in the reference library, handlers run inline on the
   connection's dispatch thread; a handler that blocks on publish deadlocked the
   discovery consumer live after the first retained-message burst (the client
   deadlocks after the first message and consumes nothing further), and the same
   latent bug existed in the reconciler before deploy.
2. **REQ-RT-2 (serialized single worker).** All state mutation and publishing for one
   component SHALL run on exactly one worker, strictly FIFO, ordered as enqueued.
   The reconciler's deduplication and re-assertion logic depends on sequential,
   ordered updates. Do not fan out to a pool.
3. **REQ-RT-3 (bounded queue, drop-on-full, never block the delivery thread).** The
   handler→worker queue SHALL be bounded; when full, the queued work is **dropped
   silently** — a blocking submit would reintroduce REQ-RT-1 through the back door.
   This is safe only because of REQ-RT-5 (retained-snapshot replay): every dropped
   input is re-armed by the next change or by the retained replay on reconnect.
4. **REQ-RT-4 (connect must be abortable on shutdown).** Process shutdown SHALL
   interrupt an in-flight broker connection attempt promptly (a small bounded time),
   independent of broker reachability; the half-open connection SHALL be torn down
   on both the failure and the cancellation paths. *Rationale (live incident):* the
   reference library's connect call blocked ignoring cancellation, so a service
   receiving a shutdown signal during a broker outage hung until the supervisor
   force-killed it (hit the PA bridge live). A new stack should ideally make
   cancellation cancel the actual connect attempt, not just the caller's wait.
5. **REQ-RT-5 (retained-replay recovery).** On every (re)connect a component SHALL
   re-run its full birth sequence — publish retained `online` on `/status`,
   re-publish `/meta`, re-publish its retained `/state` snapshot, re-subscribe to all
   inputs — and the client's session semantics SHALL guarantee that all retained
   input messages are re-delivered on every reconnect (so a stateless consumer
   re-seeds its inputs; see OD-5 for the session-flag nuance and the one component
   that deviates). Recovery after any outage is always "replay retained state and
   re-resolve", never "resume a persisted internal model".
6. **REQ-RT-6 (bounded publishes).** A publish SHALL be bounded (the sequencer uses a
   10-second wait timeout) so a dead broker surfaces as an error rather than stalling
   the component forever.
7. **REQ-RT-7 (auto-reconnect).** After a successful connect, connection loss SHALL
   trigger automatic reconnection with backoff (reference defaults: 1 s initial,
   10 s max — the curve itself is implementation detail; the requirement is
   unattended recovery).
8. **REQ-RT-8 (no credentials in shared plumbing).** Any shared runtime library SHALL
   be credential-free; broker addresses, users, and passwords flow only through
   per-component configuration (`05-deployment.md`).
9. **REQ-RT-9 (malformed input never crashes).** Malformed JSON on any input SHALL be
   logged and dropped; previous inputs retained. A malformed `/state` observed by a
   consumer SHALL delete that slot's cached snapshot (a good→bad transition must not
   leave a stale value poisoning decisions).
10. **REQ-RT-10 (worker robustness).** The worker SHALL exit cleanly on either
    cancellation or queue close, at any point in the component's lifecycle (including
    non-cancellation early returns), and shall never invoke work after close.

**Reference-implementation notes (non-normative).** The deployed system centralizes
these in a ~130-line shared library module every bridge imports: a context-aware
connect wrapper (racing the library's connect token against process cancellation,
disconnecting the half-open client on either path) and a bounded jobs-queue with a
single worker goroutine (buffer sizes 32/64/256 depending on bridge). The topic-string
builders and the `{action, value}` payload type live in a sibling schema package. Each
bridge still constructs and owns its own MQTT client, options, and subscriptions — the
shared layer is deliberately a thin set of behavior workarounds, not a client wrapper.
Cross-bridge imports of device logic are forbidden (visibility rules enforce it);
shared plumbing may be common, per-device logic may not.

---

## 9. Hosts and processes

| Host | What it is | What runs there |
|---|---|---|
| MQTT broker, `192.168.1.50:1883` | The one shared service; persistent store (retained messages survive a broker restart); user `hf`, credentials per-service in on-host config files | Nothing else — the broker hosts no logic |
| `shari` (192.168.1.139) | Raspberry Pi single-board computer, the deployment target for all supervised services | Radio bridge, beam-controller bridge, PA bridge, tuner bridge, HF rotator bridge, smart-plug bridge (both power slots), antenna-selection reconciler, power sequencer, discovery consumer — each a separate hardened service process with a dedicated unprivileged user (see `05-deployment.md`) |
| `shack-pc` | The shack PC | The UHF rotator console **only** — deliberately an interactive terminal program the operator starts by hand, not a daemon: its arming is a local keyboard act, never remote, so an always-on headless process would contradict its safety model |
| Embedded devices (wifi) | The antenna switch (relay board with its own firmware) and the M5 Stamp relay controller #1 | Their firmware speaks the canonical four-plane schema directly; device, adapter, and host collapse into one node |
| M5 Stamp relay controller #2 | Intended UHF polarization relay controller | **No firmware exists in the repo** — a documented gap (OD-13), not a component |
| Tablet (Android) | Operator console | A pure consumer/UI application: subscribes to state, publishes canonical `/cmd` topics like any other writer. Never load-bearing. See `04-console.md` |
| Devices themselves | FLEX-8400 radio, ACOM 1200S PA, ATR-1000 ATU, Ultrabeam RCU-06, WRC rotator controller, Shelly smart plugs | Speak their native protocols only; their bridges translate |

Host liveness: the model defines `muehle/host/shari` and `muehle/host/shack-pc` nodes
(publishing `online`, and on shari `temp_c`, `load`) — **no component in the repo
publishes them**; they are model-only (OD-12). Host loss is currently visible only
through its bridges' LWTs going `offline` together.

The logging layer's topics (§3.4) are specified, not implemented; when built, its
slots live at the position level (host = the operator's PC).

---

## 10. Open decisions & unresolved facts

Anything the sources disagree on or leave unknown, with evidence. A re-implementation
must resolve each explicitly rather than inherit an accident.

1. **OD-1 — Ultrabeam antenna-switch port: 3 or 4.** The repo's top-level docs and
   integration model, the unit tests, and the console's antenna map say the Ultrabeam
   beam is on switch **port 3**; the reconciler's example config and its deploy seed —
   and the console's antenna map in other places — say **port 4**. The live config file
   on shari is authoritative but was not readable from the workstation when this PRD
   was written. Consistent across all sources: port 1 = dummy load, port 6 = fan
   dipole, `off` = grounded. Requirement regardless of outcome: port numbers are pure
   configuration (REQ-RES-3). **Action: confirm on-device before wiring anything.**
2. **OD-2 — Broker topology.** All deployed defaults point at the broker at
   `192.168.1.50:1883` (current production). Work exists on an unmerged feature branch
   to move components to a broker on shari (`192.168.1.139`), committed but **not
   deployed** as of 2026-08-29. The PRD treats 192.168.1.50 as production; the
   migration is a decision point (it changes only the `broker` configuration value —
   nothing in the contracts).
3. **OD-3 — `device_online` when true: omitted or explicit.** The integration model
   says the field is "omitted when true"; the deployed bridges (radio, beam
   controller, PA, tuner) publish `device_online: true` explicitly. Consumers must
   treat both forms as equivalent (REQ-LIVE-1) unless the new implementation mandates
   one form — decide and document.
4. **OD-4 — clean-shutdown `/status`.** Retained `"online"` persists after a clean
   stop when the component's own final `offline` publish is skipped (no will fires on
   a graceful disconnect). Documented actual behavior (§4.3). Open: whether the new
   implementation adds a stronger guarantee (supervisor-published offline, expiry
   semantics) or keeps consumer-side freshness rules (REQ-LIVE-5).
5. **OD-5 — session semantics.** The ecosystem convention says persistent sessions
   ("clean session: no") so subscriptions and queued messages survive client restarts;
   but stateless consumers (the reconciler, the discovery consumer) deliberately use
   **clean sessions** so retained messages replay on every reconnect — without the
   replay they would wake with empty inputs and never resolve. The sequencer uses a
   persistent session and re-subscribes unconditionally (with a known side effect:
   stale subscriptions for removed config entries are never unsubscribed). The
   stack-agnostic requirement is REQ-RT-5 (retained inputs re-delivered on every
   reconnect); which session flag achieves it per component is the decision.
6. **OD-6 — radio state heartbeat period.** REQ-TX-3 mandates a periodic republish
   ≤5 s (the documented fix direction "e.g. every 5 s"); the receiver window is
   exactly 10 s in the deployed firmware. The exact period is a tunable default —
   pick and pin one; the incident-derived requirement is the invariant.
7. **OD-7 — sequencer `stop` semantics.** Docs say `stop` is honored only from
   `phase=running`; code (authoritative) also honors it from `idle` **with a fault**
   (teardown of a half-started station). `stop` from `idle` without a fault is dropped
   — there is no way to shut down a station that was started by hand, through the
   sequencer. And there is no abort of an in-progress sequence. Deliberate contract;
   a new team may change it, but that is a contract change, not a fix.
8. **OD-8 — `wait_state` freshness.** The sequencer's liveness precondition checks
   current `/status`, but the waited `/state` snapshot can be arbitrarily old (a
   device online per LWT but with a dead device link could satisfy a wait on a stale
   field). The two-layer liveness convention is not applied to wait *payloads*.
   Decide: freshness bound, or `device_online` coupling for `wait_state`.
9. **OD-9 — `/cmd` value types.** The convention (REQ-CMD-2) says `value` is always a
   JSON string, booleans as `"true"`/`"false"`. Deployed drift, verified in code: the
   reconciler sends and the tuner bridge accepts a JSON **boolean**
   (`{"action":"set_inline","value":true}`), and the tuner's expose descriptor
   declares `value_type: "bool"`. The antenna controller uses an integer under
   `freq_hz`; the antenna switch uses `{"select":"…"}` with no `action` key (both
   sanctioned by their expose descriptors). Decide: enforce string-only (changing the
   tuner's contract) or formalize per-descriptor value types (current de-facto state).
10. **OD-10 — grounding recovery design.** Known live defects in the reconciler's
    recovery half (§6.3, §7.3): first key-up after grounding can go into the grounded
    (shorted) port; no non-TX re-arm path (an operator at the desk cannot un-ground
    without touching the VFO or transmitting); TX deferral has no timeout (a frozen
    `tx` freezes all actuation); a reconciler restart always un-grounds and resets the
    idle clock; operator requests are unvalidated (any non-empty string becomes a
    hold target forwarded verbatim to the switch); the switch's `settled` flag is
    received but unused; publish failures advance dedup state (a failed intent is not
    retried until a different resolution or reconnect). The research files list fix
    directions (operator presence as activity, deferral timeout, restart-stable idle
    state, request validation, `settled`-gating). Each is a design decision for the
    re-implementation, with the current behavior as the documented baseline.
11. **OD-11 — reconciler single point.** If the reconciler dies (and the supervisor's
    restart gives up), antenna selection and all follow bindings stop and the station
    degrades to manual. Accepted only because RF safety is hardware. Open wishes:
    supervision policy and an explicit operator-facing "reconciler offline"
    indication (today only the LWT `status` topic exists).
12. **OD-12 — host liveness nodes.** `muehle/host/shari` and `muehle/host/shack-pc`
    appear in the model but no bridge publishes them. Model-only. shari is itself a
    single point (it fronts the HF PA, tuner, rotator and beam bridges *and* hosts the
    reconciler); host-liveness publication is an open scope decision.
13. **OD-13 — UHF polarization controller firmware gap.** The slot table attributes
    `muehle/uhf/pol-ctrl` to M5 Stamp relay controller #2, but no firmware for
    controller #2 exists in the repo. It is a documented gap, not a component; a
    re-implementation scope decision.
14. **OD-14 — logging layer.** `docs/logging-integration-model.md` specifies
    `log/event`, `spots/event` planes and a `qso-log` role that **no component
    implements**. Future scope ("specified, not yet implemented") — describe in
    milestones, do not build as current behavior.
15. **OD-15 — sequencer first-deploy seed defect.** The deployed deploy script seeds
    a config **without** the required step lists, so a first deployment crash-loops
    until an operator hand-adds them from the example config. A re-implementation
    must ship a complete seed or a built-in default sequence.
16. **OD-16 — legacy component names.** Two deployed bridge names violate the naming
    convention (the radio bridge and the beam-controller bridge names embed device
    hints rather than the `<devtag>-<function>-bridge` pattern; one PA bridge name is
    deliberately model-specific). Renames were deferred because they touch live
    service units. Slot addresses are unaffected; names are free to change in a
    re-implementation provided the address map is unchanged.
17. **OD-17 — QoS 2.** The convention reserves QoS 2 (exactly-once) for a documented
    must-never-double-fire command; none currently exists. Unused; no decision
    needed until such a command appears.
18. **OD-18 — K9AY receive-loop controller.** `hf/rx-loop-ctrl` appears in the
    integration model (receive-loop direction control, its preamp dropped by the
    hardware TX-low line) but not in the deployed slot table or the repo as a
    component. Scope and slot assignment unresolved.