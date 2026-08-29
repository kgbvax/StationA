# 01 — System architecture (stack-agnostic)

This document gives the architecture of the Mühle amateur-radio station automation
system ("stationa"). A competent software engineering team that knows nothing
about amateur radio or the original technology stack can use it to rebuild the
system from zero. Any programming language and any messaging library will do.
**Amateur radio (ham radio)** is the licensed hobby of two-way radio communication.
The system automates a complete station: a **transceiver** (the radio, which transmits
and receives), a **power amplifier** (**PA** — boosts transmit power), an **antenna
tuner** (**ATU** — matches the antenna's electrical impedance to the feed line when
the antenna is not naturally resonant on the operating frequency, with **SWR** —
standing-wave ratio — as the mismatch measure), a **rotator** (a motor that turns a directional
antenna), an **antenna switch** (a 1-of-N relay selector that routes the feed line to
one of several antennas), and the antennas themselves. This document is normative:
behavior statements appear as testable rules on the wire protocol and on component
behavior. The original implementation appears only in clearly-marked
"Reference-implementation notes" sub-sections, and only as non-normative background.
The sibling PRD documents are `00-system-overview.md` (context and
vocabulary), `02-interface-spec.md` (exact topic, payload, band and mode tables), and
`03-components/<slug>.md` (one file per component). Further: `04-console.md` (operator
console), `05-deployment-ops.md` (hosts, deploy), `06-safety.md` (RF-safety rules that the
system must never break), `07-priorities-milestones.md`.

---

## 1. Architecture summary

### 1.1 The shape of the system

The system has **no central server**. It consists of exactly one **message broker**
plus a set of small, independent services — called **bridges**. Each bridge fronts
exactly one physical device, or exactly one station-wide logic role, and nothing else.
**MQTT** is a lightweight publish/subscribe protocol: clients publish short messages to
hierarchical text **topics** on a central **broker**. A broker is a server that relays
messages by topic. Any client **subscribes** to topic filters and receives every
matching message. A **retained message** is one that the broker stores, and
re-delivers immediately to every new subscriber of that topic. **LWT (Last Will and
Testament)** is an MQTT feature: the broker publishes a pre-registered message on the
client's behalf if the client disconnects uncleanly. **QoS** (quality of service) 1
means at-least-once delivery.

Components never call each other. Integration happens in exactly two ways:

1. **Observation**: every component publishes what it is and what is now true about
   it, as retained messages at a well-known address. Every consumer subscribes and
   reads.
2. **Command**: a consumer publishes a command message to the component's command
   topic. The component acts on it, or it ignores the command. The consumer then
   watches the component's published state to see what actually happened.

The rules below are normative:

- **REQ-ARCH-1.** The system must consist of one MQTT broker and a set of independent
  bridge services. No component can take a privileged or central position on the
  message path.
- **REQ-ARCH-2.** Each bridge must front exactly one physical device or one logic
  role. §2.3 allows a bridge to front two slots of the *same* physical device, or two
  roles of the same process. A bridge must never front two unrelated devices.
- **REQ-ARCH-3.** Components must integrate only in the two ways above. They must
  (a) subscribe to other components' published state, and (b) publish commands to
  other components' command topics. No component-to-component direct connection,
  RPC, database, or file exchange is part of the architecture.
- **REQ-ARCH-4.** The dependency graph must point one way. Core device bridges and
  logic bridges must know nothing about any consumer, and must never publish under any
  consumer's topic tree. Deleting every consumer (dashboards, home-automation
  rendering, historians) must leave the station unchanged. Consumers must be
  separately deployable processes that no core component needs.
- **REQ-ARCH-5.** Any single bridge must be replaceable, restartable, and rebuildable
  independently, as long as its on-bus behavior (topics, payloads, timings) remains
  conformant. A conforming re-implementation must be indistinguishable from the
  original to a subscriber of `muehle/#`.
- **REQ-ARCH-6.** The live configuration must be the documentation. A live
  subscription to the bus gives the component inventory: the wildcard
  `muehle/+/+/meta` yields every component's self-description, for example. No
  separate inventory document is authoritative.
- **REQ-ARCH-7.** Loss of any one bridge must degrade only the function that the
  bridge provides. Its loss must never make the station *unsafe*. (See §6.3 and
  `06-safety.md`: hardware interlocks enforce RF safety, and the messaging layer only
  mirrors them.)

### 1.2 Why this shape

- **Independent failure.** A crashed bridge takes down one device's automation, not
  the station. The broker is the only shared element. Every component reconnects to
  it autonomously.
- **One service per device.** Each device has exactly one adapter that owns its
  quirks (serial protocol, boot time, error behavior). These quirks stay isolated in
  that adapter.
- **Rebuildability.** Every contract is a topic string plus a JSON payload. A team
  can rewrite one component in isolation and check the rewrite by diffing its bus
  output against the original's.
- **Heterogeneous hosts.** Components run on a Raspberry Pi, a **shack** PC (the
  shack is the room that houses the radio equipment), embedded relay controllers,
  and a tablet. The only thing they share is the broker.

### 1.3 Reference-implementation notes (non-normative)

The deployed system is a set of small programs in the Go language. Each runs as a
hardened service under the `systemd` supervisor on a Raspberry Pi called `shari`
(192.168.1.139). The programs use the Eclipse Paho Go MQTT client library and a
Mosquitto broker at `tcp://192.168.1.50:1883`. Two embedded devices (the antenna
switch and one relay controller) run their own firmware that speaks the same topic
schema directly over wifi. A Flutter application on an Android tablet is a pure
consumer. None of these is a rule. The rules are the behavior contracts in this
document. The broker must support retained messages, persistent storage across
broker restarts, and MQTT last-will messages (MQTT 3.1.1 semantics are enough. The
deployed system uses MQTT 3.1.1 with QoS 1 everywhere).

Deployed deviation from REQ-ARCH-4: the radio bridge and the beam-controller bridge
still carry embedded home-automation discovery code. The configuration key
`publish_ha_discovery` (default `false`) gates it off, with `discovery_prefix`
(default `homeassistant`). The team will delete this code when the passive discovery
consumer is proven live (OD-19). With discovery off, the canonical planes do not
change. A re-implementation can omit the embedded discovery code entirely.

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
  `uhf` (144–440 MHz). The station segment is structural. It carries only an inferred
  `activity` flag (§7.3), and in a multi-operator future it is the unit of contention.
- **slot** — a canonical **role** name at that station. A **role** is a function
  ("radio", "tuner", "sequencer"), not a device or product name: the slot segment is
  `ant-ctrl`, never `ultrabeam`. A second instance of the same role takes a numeric
  suffix (`ant-ctrl-2`).
- A **position** segment (an operator seat) has a place in the grammar. It is not
  needed in the now single-operator station, so it collapses away. It can return only
  if two operators share a station.

The rules below are normative:

- **REQ-SLOT-1.** Slot addresses must follow `<site>/<station>/<slot>`. The four
  planes (§3) attach as suffixes: `<addr>/meta`, `<addr>/state`, `<addr>/status`,
  `<addr>/cmd`.
- **REQ-SLOT-2.** `site`, `station`, `slot`, `location` (building label, for example
  `bauwagen`) and `host` (compute node label) must be deployment configuration, never
  code constants. A bridge must work for any site/station/slot name from its
  configuration file alone. A re-implementation can build in only the canonical
  vocabulary (role names, mode names, field names) and facts about the fronted
  device.
- **REQ-SLOT-3.** Site-level infrastructure that serves *across* stations must sit
  outside the station path: compute nodes at `site/host/<name>`, cross-station power
  supplies at `site/power/<name>`. Consumers command these like any other slot.
- **REQ-SLOT-4.** The MQTT client identifier of a component must default to its slot
  address with `/` replaced by `-` (for example `muehle-hf-radio`). Configuration can
  override it. The broker then makes a second, colliding connection diagnosable.

### 2.2 Full slot table

This is the deployed station inventory. Addresses are the contract. Consumers bind
to them. (Component details: `03-components/<slug>.md`.)

| Slot address | Canonical role | Owning component | Physical device | Host |
|---|---|---|---|---|
| `muehle/power/master` | `power` | shelly-power-bridge (one process, two slots) | Shelly smart plug — station master mains | shari. |
| `muehle/power/psu-13v8` | `power` | shelly-power-bridge | Shelly smart plug — 13.8 V DC power supply (site-level, feeding HF and UHF control electronics) | shari. |
| `muehle/hf/radio` | `radio` | flexbridge | FLEX-8400 transceiver (ethernet) | shari. |
| `muehle/hf/ant-ctrl` | `ant-ctrl` | ultrabridge | Ultrabeam RCU-06 beam-antenna controller (USB-serial) | shari. |
| `muehle/hf/ant-switch` | `ant-switch` | waveshare_relay-antswitch-bridge (embedded firmware) | 1:6 relay antenna switch (wifi) | embedded (the switch itself). |
| `muehle/hf/switch` | `switch` | m5stamp-hf-ctrl (embedded firmware) | M5 Stamp relay controller #1 — relay 4 = transceiver remote-on, relay 3 = PA remote-on | embedded. |
| `muehle/hf/pa-arm` | `pa-arm` | m5stamp-hf-ctrl (same firmware, same device) | M5 Stamp relay controller #1 — relay 1 = the PA arm approval | embedded. |
| `muehle/hf/antenna-select` | `reconciler` | antennaselect | none — logic slot (policy engine) | shari. |
| `muehle/hf/pa` | `pa` | acom1200s-pa-bridge | ACOM 1200S power amplifier (serial connection, telemetry only — the bridge cannot switch it) | shari. |
| `muehle/hf/rotator` | `rotator` | wrc-rotator-bridge | Yaesu G-450DC rotator through AF6SA "WRC" controller (websocket) | shari. |
| `muehle/hf/tuner` | `tuner` | atr1k-tuner-bridge | ATR-1000 antenna tuner (binary WebSocket over wifi) | shari. |
| `muehle/hf/power-seq` | `sequencer` | powerseq | none — logic slot (startup/shutdown sequencer) | shari. |
| `muehle/hf/discovery` | `discovery` (published in its own `/meta` — see note below) | hadiscovery | none — logic slot (passive consumer of other slots' `/meta`) | shari. |
| `muehle/uhf/pol-ctrl` | `pol-ctrl` | *no firmware exists* — documented gap | M5 Stamp relay controller #2 — X-Quad antenna polarization relays | embedded (gap). |
| `muehle/uhf/rotator` | `rotator` | pelcobridge2 | PTS-303Z/3050DZ pan/tilt head (Pelco-D protocol over RS-485) | shack-pc (interactive program, not a service). |
| `muehle/host/shari`, `muehle/host/shack-pc` | `host` | *no component publishes these* — model-only (§10) | — | themselves. |

The canonical role vocabulary is: `radio`, `pa`, `tuner`, `ant-switch`, `ant-ctrl`,
`rotator`, `pol-ctrl`, `reconciler`, `sequencer`, `host`, `power`, `switch`,
`pa-arm`, and `discovery`. `preamp` (the same function, as a slot or a capability), `bias-feed`
(likewise — a **bias feed** feeds DC power over the antenna coax to a masthead device),
and `station` are also in the vocabulary. A re-implementation must not
invent new names without cause. A future logging layer adds `qso-log`, `bandmap`, and
later `scoreboard` (§10 gives it as a spec, and nobody builds it today). A **QSO**
is a two-way radio contact, so `qso-log` is the contact-log role. The
`muehle/hf/discovery` slot is a passive consumer, not an actuator, and consumers are
not core (REQ-ARCH-4). Its own `/meta` nevertheless publishes the role string
`discovery` (see `02-interface-spec.md` §3.13), and the vocabulary admits it for
self-description. See
`03-components/` for its coverage.

The integration model also defines `muehle/hf/rx-loop-ctrl` (a K9AY receive-only loop
antenna's direction controller) in the hardware interlock chain (§6.3). It is not in
the deployed slot table above. See `03-components/` for the per-component coverage.

The `pol-ctrl` slot in the table drives the X-Quad antenna's polarization relays. An
**X-Quad** is a four-element square antenna. **Polarization** is the orientation of the
radio wave's electric field.

The UHF station also has an IC-9700 transceiver that no component automates. No
`uhf/radio` slot exists. The shared 13.8 V power supply feeds it (see the `feeds`
capability in `02-interface-spec.md`).

### 2.3 Compound devices and multi-slot processes

- One physical device can appear under two slots: the M5 Stamp relay controller #1
  owns both `muehle/hf/switch` and `muehle/hf/pa-arm`. The only mechanism that ties
  the two slots to one device is that both carry **the same
  `device {model, serial}`** objects in `/meta` (§3.1). There is no other
  compound-device mechanism.
- One process can own two slots: shelly-power-bridge fronts both power slots. Each
  slot still gets its own full four-plane presence.
- Embedded nodes (the antenna switch, the relay controllers) collapse device,
  adapter, and host into one node. Their firmware speaks the canonical schema
  directly over wifi MQTT.

### 2.4 What is deliberately NOT a slot: passive resources

Antennas, masts, and masthead preamplifiers are **passive resources**. Configuration
names them and references them. They have **no MQTT presence at all** — no topics, no
state, nothing to subscribe to. The station's antennas are:

- `ant/ultrabeam` — a steerable directional beam antenna ("yagi") on a mast. The
  `ant-ctrl` slot tunes it actively.
- `ant/fan-dipole` — a fixed multi-band wire antenna, resonant on the 80 m and 40 m
  bands.
- `ant/dummy-load` — a heat-dissipating test load that radiates nothing (a test
  load).
- `ant/k9ay-loop` — a receive-only loop antenna.
- `ant/xquad-2m`, `ant/xquad-70cm` — UHF quad antennas.
- masts `mast/ta16-hf`, `mast/ta16-vhf`, and masthead preamplifiers `preamp/2m`,
  `preamp/70cm` (a **masthead preamplifier** is a signal booster mounted at the
  antenna).

Passive resources exist in exactly one place: the antenna-selection reconciler's
**wiring map**. This is configuration (never code) that maps antenna-switch port keys
to resource names: `port1: dummy-load`, `port6: fan-dipole`, `off: grounded`, and the
ultrabeam port (see §10 for the port 3 / port 4 conflict). A **controller map**
(configuration `[band_follow] resource + slot`) maps resource name to controller slot
(for example `ultrabeam → ant-ctrl`) so that band-following needs no antenna names in
code. The rules below are normative:

- **REQ-RES-1.** Passive-resource names must appear only in site configuration, never
  in component code.
- **REQ-RES-2.** Components must not publish or subscribe under `ant/*`, `mast/*`, or
  `preamp/*`.
- **REQ-RES-3.** The wiring map must be the single editable place where the antenna
  arrangement lives. Port numbers must be configuration, never hard-coded (see §10,
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

- **REQ-PLANE-1.** Every slot must expose exactly these four suffixes. Read-only
  slots omit `/cmd`. No other sub-topics are part of the contract now. (§10 gives —
  but does not build — a non-retained `event` sub-topic for a future logging layer.)
- **REQ-PLANE-2.** All publishes and subscribes must use QoS 1 (at-least-once). A
  command that must never double-fire can use QoS 2 (exactly-once). None uses QoS 2
  now.
- **REQ-PLANE-3.** Publishers must set the retain flag on `/meta`, `/state` and
  `/status`. They must not set it on `/cmd`, except under the idempotent-actuator
  exception of §5.4.
- **REQ-PLANE-4.** All payloads must be UTF-8 JSON. The one exception is `/status`,
  which is a bare string (§3.3). It is not JSON.

### 3.1 `/meta` — the birth certificate

Retained JSON, re-published on **every (re)connect** (re-birth). Field set:

| Field | Type | Needed | Semantics |
|---|---|---|---|
| `schema` | string | yes | now `"1.0"`. |
| `role` | string | yes | canonical role name (§2.2) — never a device/product name. |
| `device` | object | device slots | `{model, serial?, firmware?}`. Logic slots omit it (or omit `serial`/`firmware`). Two slots of one physical device carry the same `device{model,serial}` (§2.3). |
| `link` | string | device slots | transport hint: `ethernet` \| `serial` \| `wifi` \| `embedded`, `none` for logic slots. |
| `location` | string | yes | physical building label, from configuration (for example `bauwagen`). |
| `host` | string | yes | compute node the adapter runs on, from configuration. Embedded nodes name themselves. |
| `capabilities` | object | yes | the discovery contract (below). |
| `expose` | object | no | consumer-neutral field/action surface (below). |

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

In the example, `diversity` means receiving the same signal on two antennas and
combining them. `bias_t` marks a bias feed — DC power over the antenna coax to a
masthead device (§2.2).

**Capabilities** are the binding contract. Consumers bind to a *declared capability*,
never to a device model. The capability keys include `bands`, `modes`, `ports`,
`exclusive`, `hot_switch`, `axes`, `feeds`, `channels`, `relay_map`, `fail_safe`,
`heartbeat`, `key_input`, `max_power_w`, and `inline`. More keys:
`tune_modes`, `directions`, `ladder`, `band_source`, and others. The same function can
appear as a standalone slot, or a capability key can absorb it. Bind to the function,
and discover where it lives. The exact key set per role is in
`02-interface-spec.md` §3.

**`expose`** is a block that no slot needs to provide. It describes the slot's
observable and controllable fields with zero consumer-specific vocabulary (no
home-automation strings, no templates), so that any consumer can render its own
representation. Per field it declares: `key` (the JSON key in `/state`), `name`,
`type` (`number`|`enum`|`boolean`| `string`), the not-necessary
`unit`/`class`/`state_class`/`options`/`options_ref`/`min`/`max`/
`step`/`on`/`off`, a `writable` flag (a field with `writable: true` is a setpoint),
and for writable fields a **command descriptor**
`{action?, value_key?, value_type?}` that states exactly how `/cmd` encodes a
command to this field (§5.1). A `writable: true` field always carries its command
descriptor. `expose` also declares a top-level `device` block that no slot needs
(`name` when the block is present, and `model`, `manufacturer`, `sw_version`,
`area` that a slot gives only when it has them)
and an `actions[]` list of one-shot buttons (`{key, name, command}` — the
deployed antenna controller declares `retract` this way). Full field tables:
`02-interface-spec.md` §2.3.

### 3.2 `/state` — one retained JSON snapshot

- **REQ-STATE-1 (one-snapshot rule).** A slot's `/state` must be a single complete
  JSON document per publish — never partial updates, never per-field topics. Last
  write wins. A subscriber always holds a complete, consistent view. A late joiner
  gets the whole picture from the retained copy without polling.
- **REQ-STATE-2.** Every snapshot must carry `ts`: an RFC 3339 UTC timestamp string
  (for example `"2026-07-06T12:34:56Z"`) of this publish.
- **REQ-STATE-3 (change-only publishing).** A component must publish `/state` only
  when a field value changes. This is the default cadence — the bus is quiet when
  nothing changes.
- **REQ-STATE-4 (heartbeat exception).** Components that are inputs to
  heartbeat-driven safety logic must additionally republish an unchanged `/state` on
  a fixed period, so that absence is detectable. The PA-arm relay controller
  republishes the same state at least every 10 s. See REQ-TX-3 for the radio-side
  heartbeat rule.
- **REQ-STATE-5.** Device slots must set `device_online` (boolean) to `false` and
  `error` (human-readable string) when the fronted hardware is unreachable but the
  bridge itself is up. The `device_online`-when-true form is an open decision (OD-3).
  Consumers must treat absence and explicit `true` as the same.
- **REQ-STATE-6.** Liveness must never be a field inside `/state`. A dead node cannot
  update its own state document. That is exactly why liveness is a separate topic
  (§3.3).
- **REQ-STATE-7.** `freq_hz` must always be an integer in Hz (never kHz, MHz, or a
  float). `band` must always be the result of checking `freq_hz` against the
  shared band table (`02-interface-spec.md` §4.1). It must never be a stored
  setpoint, so band and frequency can never disagree. The adapter normalizes
  modes to the six canonical names (`cw`, `usb`, `lsb`, `am`, `fm`, `data`).
  Consumers never see raw firmware strings.

Worked example (`muehle/hf/radio/state`):

```json
{ "ts": "2026-07-06T12:34:56Z", "freq_hz": 14025000, "band": "20m",
  "mode": "cw", "tx": "rx", "tuning": false, "drive": 40, "rx_input": "ant1" }
```

Per-slot field sets are in `02-interface-spec.md` §3 and each `03-components/` file.

### 3.3 `/status` — liveness

- **REQ-STATUS-1.** The payload must be the bare string `online` or `offline` — not
  JSON — retained, QoS 1. (This lets home-automation availability integrations map it
  with no template.)
- **REQ-STATUS-2.** A component must register a retained last-will (`offline`) at
  every connect. It must publish retained `online` once up on every (re)connect, and
  must publish retained `offline` itself on clean shutdown. The broker publishes
  `offline` automatically when a node drops uncleanly.
- **REQ-STATUS-3.** A clean shutdown does not always produce the component's own
  final `offline` (see §4.3). Consumers therefore must not treat a retained `online`
  as proof that the component runs.

### 3.4 Plane-adjacent topics (specified, not built)

A future logging layer adds a non-retained `event` sub-topic per logging slot
(`<station>/log/event`, `<station>/spots/event`) with retained
`log/{meta,status,state}`. **No component has this behavior today.** It is future
scope (§10, and `07-priorities-milestones.md`). High-rate streams (audio, spectrum,
IQ data) are explicitly *not* on the bus.

---

## 4. The two-layer liveness model

### 4.1 The two layers

The model has two distinct liveness notions, deliberately on two different planes:

1. **Bridge liveness** — `/status` (the LWT topic). It answers: *is the software
   component connected to the broker?* The component sets `online` at connect, and
   the will sets `offline` on an unclean loss.
2. **Device-link liveness** — the boolean `device_online` field *inside* `/state`
   (device slots only). It answers: *is the hardware behind the running bridge
   reachable?* The bridge stays `online` while, for example, the radio's ethernet
   link is down. Only `device_online` goes `false`, conventionally with a
   human-readable `error` string.

- **REQ-LIVE-1.** A consumer must combine both layers with a logical AND before it
  trusts a device slot's state-derived fields.
  `usable = (/status == "online") AND (device_online is true or absent)`.
  Keying on `/status` alone is a defect with a live incident behind it. The
  antenna-selection reconciler originally keyed on
  `/status` alone. When the radio's link died while the bridge stayed up, the
  reconciler flapped the antenna selection on stale or empty state ("chatter").
  Regression tests cover the fix — the AND gate plus the empty-band-holds rule
  (§7.3) — in all four combinations and both arrival orders of the retained
  messages.
- **REQ-LIVE-2.** A consumer that waits on state must also check that the target's
  `/status` is now `online`. This implicit precondition makes sure that a dead
  device cannot satisfy a wait on a stale retained `/state`. (See §6.1: the
  sequencer's `wait_state` does exactly this.)
- **REQ-LIVE-3.** A `wait_status`-style condition must distinguish *absence* from
  *offline*. Waiting for `offline` needs an actual `offline` payload, and it can
  never pass on a slot that has never published.
- **REQ-LIVE-4.** **Never trust retained state for safety.** Retained values can be
  stale after a crash. Safety lives on the hardware plane (§6.3, `06-safety.md`).
  The messaging layer only mirrors it.

### 4.2 Heartbeats

Consumers, not the broker, enforce the liveness of *inputs*:

- The PA-arm relay firmware drops its arm approval if the radio's `/state` has not
  arrived within **10 s** (constant `RADIO_HEARTBEAT_MS = 10000`).
- The relay firmware republishes its own `/state` at least every **10 s**, even
  when unchanged, so *its* consumers can detect its death.
- **REQ-TX-3 (radio state heartbeat — hard rule from a live incident).** The radio
  bridge must republish `/state` at least every **5 s** while the radio link is
  up — not only on state change. It can instead give a liveness mechanism that
  works just as well. The original bridge published change-only. During quiet
  reception no messages
  arrived, the PA-arm heartbeat silently expired, and the arm dropped even though
  everything was healthy. The receiver-side 10 s window and the ≤5 s republish
  rule are both normative.

### 4.3 The clean-shutdown retained-"online" subtlety

This is actual deployed behavior. A re-implementation must reproduce it, and
consumers must survive it: the broker does not fire the last will on a clean
process shutdown. The model expects the component to publish `offline` itself
before it disconnects. But if a stop skips that final publish (a supervisor stop
that kills it, a bug, a host power cut), the retained `/status` stays `"online"`
for a service that no longer runs. Two rules follow:

- **REQ-LIVE-5.** Consumers must not treat a retained `/status == "online"` as proof
  of a running component. It proves only that *some* recently-live instance
  published it. Freshness comes from heartbeat republishes (§4.2) and from the
  `/state.ts` timestamp.
- **REQ-LIVE-6.** A supervisor that stops a component cleanly must let the component
  publish its own retained `offline` before termination (grace period ≥ 250 ms in
  the reference deployment).

---

## 5. Command model

### 5.1 The value-key convention

Commands are JSON on `<addr>/cmd`, QoS 1, not retained by default. The universal
envelope:

```json
{ "action": "<action>", "value": "<argument>" }
```

- **REQ-CMD-1.** The argument must ride under the JSON key **`value`** — never under
  a key named after the action. The original ATR-1000 tuner bridge shipped
  `{"action":"set_inline","set_inline":"true"}` live. The team centralized this
  convention to end that class of bug.
- **REQ-CMD-2.** `value` must be a JSON **string** on the wire — booleans as
  `"true"` / `"false"`, numbers as their decimal string — and the receiving bridge
  parses it. Two deviations exist on the bus now. (a) The antenna controller's
  `frequency` command carries an integer under `freq_hz` (see REQ-CMD-3). (b) The
  deployed tuner bridge accepts, and the deployed reconciler sends, a JSON boolean
  for `{"action":"set_inline","value":true|false}` (checked in code — see OD-9).
- **REQ-CMD-3 (deviation authority).** The command descriptor in the slot's `/meta`
  `expose` block is the **authority** for which key carries the argument and whether
  an `action` key exists. Four allowed shapes:
  1. `{action, value_key:"value"}` → `{"action":"<action>","value":"<string>"}` (the
     default).
  2. `{action, value_key:"freq_hz", value_type:"int"}` →
     `{"action":"frequency","freq_hz":14025000}` (the antenna controller).
  3. `{value_key:"select", value_type:"string"}` with no `action` →
     `{"select":"port4"}` (the antenna switch).
  4. `{action}` only, with no `value_key` (a button) →
     `{"action":"<action>"}` (the antenna controller's `retract`, declared in
     the `actions[]` list of the `expose` block).
  A re-implementation of a bridge must accept exactly its declared shape.
- **REQ-CMD-4.** A bridge that receives an unknown action, an invalid payload, or
  malformed JSON must log the input and drop it. The bridge must keep running, and
  it must never crash on bad intent.

### 5.2 Fire-and-observe

- **REQ-CMD-5 (plane discipline).** Commands are **fire-and-observe**: the sender
  must not assume that a command succeeded because it emitted it. Confirmation is
  the target's subsequent `/state`. Consumers react to **state**, never to intent.
- **REQ-CMD-6 (no acknowledgment plane).** Commands never get an acknowledgment.
  There is no reply topic and no request/response pattern. The only observable
  acknowledgment is the retained state change. A reconciler that wants confirmation
  re-issues intent when the observed state disagrees with its target (§7.2).

### 5.3 Retention policy

- **REQ-CMD-7 (default).** Senders must not retain `/cmd`. A retained command
  re-delivers on every reconnect, possibly with no operator behind it.
- **REQ-CMD-8 (idempotent-actuator exception).** A sender can retain `/cmd` when
  re-applying the same command on reconnect produces the same physical outcome.
  This covers the steady-state setpoints of idempotent actuators: power plugs
  `set_power`, remote-on relays `set_pa`/`set_trx`, the arm approval `set_enabled`,
  and the antenna-switch `select`. Retention gives these actuators **self-heal**:
  after any reconnect they re-apply the last commanded steady state from the
  retained command.
- **REQ-CMD-9 (one-shot clearing).** If anyone retains a one-shot physical command
  for any reason (for example the antenna controller's `retract`, retained so that
  it survives a link outage), the owner must clear it. The owner must publish an
  **empty retained payload** on the same topic immediately after execution. Then
  the command does not re-fire on reconnect. One-shot commands like the tuner's
  `tune` must never get the retain flag.
- **REQ-CMD-10 (operator one-shots).** Senders must publish a sequencer's own
  `/cmd` not-retained, and the sequencer must subscribe to it at QoS 0 (see §6.1
  for why: a queued start command replayed after a restart can re-energize the
  station). After a restart, a component must not emit any command until an
  explicit operator command arrives.

### 5.4 Per-slot command surfaces (summary)

Exact payloads and semantics per component are in `02-interface-spec.md` §3 and the
`03-components/` files. The deployed surfaces, for orientation:

| Slot | Commands (retained unless noted) |
|---|---|
| `power/master`, `power/psu-13v8` | `set_power {on\|off}` (retained). |
| `hf/radio` | `set_band {label}` (restores the radio's persisted per-band frequency/mode), `dvk_play {1..12}` / `dvk_play_<N>` / `dvk_stop {id\|active}` (voice keyer one-shots — a **voice keyer** is a recorder that replays a stored voice message on transmit), `set_mic_profile {name}`. Nothing else exists: `set_freq_hz`, `set_mode`, and `set_drive` do not exist, and senders must not publish them (`02-interface-spec.md` §3.3). All not retained. |
| `hf/pa` | `set_band`, `set_mode {operate\|standby}` (not retained). |
| `hf/ant-switch` | `select {off\|port1..port6}` (retained, `{"select":"…"}` shape, no `action` key). |
| `hf/tuner` | `set_inline {true\|false}`, `tune {full\|mem}` (not retained). |
| `hf/switch` | `set_pa {on\|off}`, `set_trx {on\|off}` (retained, idempotent). |
| `hf/pa-arm` | `set_enabled {true\|false}` (retained steady-state approval). `armed` — nobody ever commands it (§6.3). |
| `hf/power-seq` | `start`, `stop` (not retained, QoS 0). |
| `hf/ant-ctrl` | `frequency` (int under `freq_hz`), `band`, `direction`, `retract` (retained then cleared per REQ-CMD-9). |
| `hf/antenna-select` | `request {port1..port6\|off\|auto}` (retained operator hold, §7.3). |
| `hf/rotator` | `set_az {degrees}`, `stop`, `fwd`, `rev` (not retained — motion is not a setpoint). |
| `uhf/rotator` | `stop` only — no remote motion exists by design, and arming is a local keyboard act. |
| `hf/discovery` | none (passive consumer). |

---

## 6. Data flows

Three concrete end-to-end walks. Topics and payloads are exact. Timings are the
deployed defaults (all are configuration — `05-deployment-ops.md`, `03-components/`).

### 6.1 Flow A — operator presses START: sequencer → power → radio

The operator (any UI: a dashboard button, a home-automation tile, the console)
publishes one command to the sequencer. The sequencer — a logic slot, no device —
runs a configuration-driven ordered step list. It pauses for settle delays and for
explicit liveness/telemetry confirmation from each dependent device before it goes
on. Ordering matters: energizing a PA (power amplifier) before control logic is up
can damage hardware. Removing power without inrush staggering can also damage
hardware. Devices need tens of seconds to boot after their mains comes up.

Sequence (the exact shipped default. Step names are user-visible in `/state.step`
and fault strings):

1. Sequencer receives `{"action":"start"}` on `muehle/hf/power-seq/cmd` — not
   retained, subscribed at QoS 0 (a command queued across a restart and replayed can
   re-energize the station — REQ-CMD-10). The sequencer honors it only when its
   published `phase` is `idle`. Then `phase` becomes `starting`.
2. `master-on` — publish `{"action":"set_power","value":"on"}` (retained) to
   `muehle/power/master/cmd` (the station master mains smart plug). Everything
   loses and regains power with this plug.
3. `network-delay` — wait 30 s (default `network_delay_s`). The network, broker,
   and the wifi of plug-in devices come up only after master mains returns.
4. `psu-on` — publish `{"action":"set_power","value":"on"}` (retained) to
   `muehle/power/psu-13v8/cmd` (the 13.8 V DC supply feeding all control
   electronics).
5. `wait-controllers-online` — wait until the `/status` of `muehle/hf/switch`,
   `muehle/hf/pa-arm`, and `muehle/hf/ant-switch` are all `online` (they boot on
   the 13.8 V supply), polling every 200 ms, deadline 120 s (default
   `step_timeout_s`). Timeout → fault (below).
6. `trx-on` — publish `{"action":"set_trx","value":"on"}` (retained) to
   `muehle/hf/switch/cmd` (closes relay 4, the transceiver's remote-power-on
   trigger).
7. `wait-radio-online` — wait until `muehle/hf/radio/status` is `online` (deadline
   120 s).
8. `pa-on` — publish `{"action":"set_pa","value":"on"}` (retained) to
   `muehle/hf/switch/cmd` (relay 3, PA remote-on).
9. `wait-pa-power-on` — wait until the PA's `/state` field `power == "on"` **AND**
   its `/status` is now `online` (deadline 120 s). The implicit liveness
   precondition (REQ-LIVE-2) stops a dead PA from passing on a stale retained
   `/state`.
10. `pa-arm-enable` — publish `{"action":"set_enabled","value":"true"}` (retained)
    to `muehle/hf/pa-arm/cmd`. Last step → `phase` becomes `running`.

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

Shutdown is the exact reverse with a 2 s stagger (default `shutdown_stagger_s`)
between power removals, to stagger the electrical **inrush current** (the surge
that appears the moment an inductive load receives power) of switching: `pa-arm set_enabled false` → stagger →
`set_pa off` → stagger → `set_trx off` → stagger → `psu-13v8 off` → stagger →
`master off`. The default shutdown deliberately has no waits — it must make progress
even if devices are already dead. Completing it clears any fault and returns
`phase=idle`.

Error behavior (normative):

- **REQ-SEQ-1.** Any step failure — wait timeout (`timeout`), broker disconnect
  (`broker disconnected`), publish failure including the 10-second publish bound
  (`publish failed: …`), or process shutdown (`interrupted`) — must set
  `phase=idle` immediately, with `fault = "<step>: <reason>"` and `step=""`. The
  sequencer must do **no rollback**: driven slots keep their last retained `/cmd`.
  Recovery is a re-run of `start` (every step is idempotent steady-state intent),
  or a run of `stop` from `idle`-with-fault (teardown of a half-started station).
- **REQ-SEQ-2.** Exactly one sequence must run at a time. The sequencer drops and
  logs commands that arrive during a run. The sequencer honors `start` only from
  `phase=idle`. It honors `stop` from `phase=running`, or from `phase=idle` with a
  fault. There is no command that cancels a run in progress (a deliberate
  contract, see OD-7).
- **REQ-SEQ-3.** On process start, the sequencer must publish a fresh retained idle
  `/state` (this overwrites any stale retained snapshot from a crashed
  predecessor) and must emit **no commands** until an operator `start` arrives.
- **REQ-SEQ-4.** On every (re)connect, the sequencer must republish
  `/status=online`, `/meta`, and its retained `/state`.

### 6.2 Flow B — operator picks a band: antenna-select → switch / beam / tuner / PA

A **band** is the wavelength name of the operating frequency (for example, `20m`
names the 14 MHz amateur allocation. The exact band-edge table is in
`02-interface-spec.md` §4.1). The operator selects a band on the radio (or any UI
publishes the radio's `set_band` command, which restores the radio's persisted
frequency/mode for that band). The radio bridge republishes `/state` with the new
`freq_hz` (integer, Hz) and the derived `band`. Everything else follows by
observation:

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

Exact emission rules (normative. The reconciler is stateless — every decision is a
pure function of configuration plus the latest inputs):

- **REQ-SEL-1 (ladder).** On every input change, the reconciler must resolve one
  switch target by the fixed priority ladder **idle > operator > auto** (§7.3). It
  must publish the target as `{"ts","mode","target","source"}` on
  `muehle/hf/antenna-select/state` — only when the decision triple changes.
- **REQ-SEL-2 (two-layer gate).** The reconciler must act on radio-derived fields
  only when `radio/status == "online"` AND `radio/state.device_online` is true (or
  absent). If either layer drops after a selection, the reconciler must hold the
  last selection (no chatter, no grounding from liveness alone).
- **REQ-SEL-3 (empty band holds).** An empty `band` string must hold the last
  selection, and the reconciler must emit nothing. (It is the radio bridge's
  transient "reconnecting, no slice reported yet" state. A **slice** is the
  radio's named receive channel, a vendor concept. Resolving the empty band to
  the fallback chattered the antenna on every reconnect cycle. Regression-tested.)
- **REQ-SEL-4 (fallback).** A known-but-unmatched band (for example `160m`, or the
  out-of-allocation marker `gen`) must map to the configured fallback resource (at
  Mühle: the fan-dipole, reached through the antenna tuner).
- **REQ-SEL-5 (cold-switch deferral).** The reconciler must withhold a wanted port
  change while `radio/state.tx == "tx"` (the switch is not rated for switching under
  RF power — `hot_switch: false`). It must log the deferral once per deferred
  resolution (the deferral notice re-arms on each new resolution), and emit the
  change on the return to `rx`. The reconciler owns ordering. Hardware interlocks
  give the enforcement (§6.3).
- **REQ-SEL-6 (confirm/re-assert).** The reconciler must treat the switch's reported
  `selected` position as ground truth. While it is unknown, the reconciler sends at
  most one command per target. Once known, any mismatch with the target re-issues
  the command on the next input change (the switch then re-asserts over a manual
  override).
- **REQ-SEL-7 (band-follow).** The reconciler must emit a frequency intent
  `{"action":"frequency","freq_hz":N}` (retained) to the followed resource's
  controller slot (default `ant-ctrl`). It must emit only while that controller's
  resource is the selected port, the radio is two-layer online, and
  `freq_hz > 0`. It must deduplicate against the last frequency pushed. Never tune
  a beam that is not in circuit.
- **REQ-SEL-8 (PA follow).** The reconciler must emit
  `{"action":"set_band","value":"<band>"}` (NOT retained) to the PA slot whenever
  the radio is online and reports a band. This emission is deliberately NOT gated
  on antenna selection, station activity, or TX state — it fires during
  transmission as well. The PA is always in the RF path, and
  its hot-switch protection is hardware. Deduplicated against the last band
  pushed.
- **REQ-SEL-9 (tuner follow).** The reconciler must emit
  `{"action":"set_inline","value":…}` (NOT retained) to the tuner slot when both
  conditions hold. The selected resource must be the tuner's resource, and the
  band must be in the configured non-resonant set (the deployed configuration
  value is `["30m","60m","80m","160m"]`. The configuration must set `atu_bands`
  when the operator enables tuner follow — the deployed component has no
  built-in default). `true` puts the ATU in
  the RF path, `false` bypasses it (including when idle-grounded, since target
  `off` is not the tuner's port). Deduplicated. Self-heal for the non-retained
  PA/tuner commands comes from the reconciler re-resolving on the retained
  radio-state replay at its own reconnect — not from retained commands.

### 6.3 Flow C — transmit safety: the arm-approval formula

This flow is the reason the system is safe to leave automated. **Safety does not
live on the messaging plane.** A physical TX-inhibit line runs in series through the
RF components. Any open link inhibits transmit — fast, local, fail-safe. The
messaging layer only *mirrors* each link as read-only state, and adds one
slowly-changing **arm approval**:

```
armed = enabled ∧ radio_online ∧ ¬radio.tuning ∧ band_safe ∧ heartbeat ∧ antenna_ready
```

The arm-approval relay is **fail-safe open**: the safe state de-energizes its coil,
so loss of software, network, or 13.8 V power removes the approval. The hardware
chain (locally enforced. Software mirrors only):

```
13.8 V PSU → relay controllers + ant-switch (they boot on this supply) → radio (remote-on) → PA (remote-on)
radio TX-low line → rx-loop-ctrl (preamp off) → ant-ctrl (inhibit while moving) → pa-arm → PA
```

Walk:

1. The sequencer's last startup step publishes the retained approval
   `{"action":"set_enabled","value":"true"}` to `muehle/hf/pa-arm/cmd`. This is
   `enabled` — an *approval*, never an arm.
2. The relay-controller firmware (`muehle/hf/pa-arm`) itself computes `armed` —
   nobody ever commands it:
   - `enabled` — the last `set_enabled` approval (retained, idempotent,
     self-healing).
   - `radio_online` — the radio bridge's liveness per the two layers.
   - `¬radio.tuning` — the radio is not running a tuning carrier (a **carrier**
     is an unmodulated RF signal that the radio emits while the tuner matches
     the antenna).
   - `band_safe` — the radio's band is in the firmware's safe-band set.
   - `heartbeat` — a radio `/state` message arrived within **10 s** (§4.2).
   - `antenna_ready` — the antenna switch is on a valid port (not grounded/`off`).
3. The firmware combines the approval with the fast hardware key line (a physical
   AND): normal transmit timing rides the hardware edge. Software only
   arms/disarms.
4. It publishes `/state {"enabled":…, "armed":…}` and republishes an unchanged
   snapshot at least every **10 s** (its own heartbeat).

- **REQ-TX-1.** The actuator firmware must compute `armed` itself. Nobody ever
  commands it.
- **REQ-TX-2.** The arm relay must be fail-safe open (de-energized = no approval).
  The antenna switch's `off` position must short open antenna ports to ground
  (lightning protection). The smart plugs' power-on default must be off
  (`fail_safe: off`). Loss of software, network, or power must degrade to
  inaction, not hazard.
- **REQ-TX-3.** (Stated at §4.2.) The radio bridge must republish `/state` at
  least every 5 s while the link is up. The original change-only publishing
  starved the 10 s heartbeat during idle-but-healthy periods and silently dropped
  the arm — a live incident. The team derived this rule from that incident, and
  it is not negotiable.
- **REQ-TX-4.** The ordering contracts must hold: the PA disarmed **and
  confirmed** before any tuning carrier. RF inhibited and receive confirmed
  before any antenna port change. Arming always the last startup action, and
  disarm always the first shutdown action.
- **REQ-TX-5.** No safety decision must depend on the messaging plane. Killing
  the reconciler or any bridge degrades the station to manual operation, which
  is safe.

A known live defect to carry forward honestly (documented, not fixed, see OD-10).
**To key** means to switch the transmitter on, from Morse-code hand-key practice.
A **key-up** is the start of a transmission, and **un-key** is its end.
Re-arming station activity is itself a transmission (§7.3). Therefore the first
key-up after an idle grounding can happen into the still-grounded (shorted) switch
port. The deferred port change fires only at un-key. The PA stays disarmed
through `antenna_ready`, but the transceiver keys into a short. A
re-implementation must treat a recovery path free of this defect as a design goal.

---

## 7. Coordination patterns

### 7.1 Policy vs actuator: the reconciler split

Actuators are dumb: they apply commands and report state. Policy — *which* actuator
state the operators want, and *why* — lives in a separate **reconciler** (a
stateless, pure decision component, the `reconciler` role). When many commanders
want one actuator, one arbiter resolves by priority and emits one intent stream.

- **REQ-COORD-1.** The reconciler must be a pure function of configuration plus
  the latest observed inputs. The reconciler has no internal mode switch, and no
  command memory beyond the operator hold input.
- **REQ-COORD-2.** The reconciler must publish *why*: its `/state` carries
  `source ∈ {idle, operator, auto}` — the tier that won — so the bus documents the
  reason for every actuator position.
- **REQ-COORD-3.** The reconciler must derive `mode`: `manual` while an operator
  hold is active, else `auto` — never a separately commanded switch.
- **REQ-COORD-4.** The reconciler must not belong to any enforcement path. Its
  death degrades the station to manual. It is a coordination single point: if it
  dies, all soft bindings (antenna selection, band-follow, PA-follow,
  tuner-follow) stop together, and its retained state goes stale. The design
  accepts this only because safety is hardware (§6.3). A supervision/restart
  policy and an explicit "reconciler offline" operator indication are open wishes
  (OD-11).

### 7.2 Sequencer: one writer, no locking

The sequencer owns the ordered startup/shutdown chain (§6.1) but **never locks**
the slots it drives. Any channel stays directly commandable for troubleshooting
while the sequencer is idle, and the retained steady-state commands give each
actuator self-heal (§5.3). Confirmation is by observation: wait steps poll the
targets' actual published liveness and state. The wait uses a 200 ms poll cadence
and a 120 s default deadline. The continuous-hold debounce window is not
necessary: the default 0 means edge-triggered, and an explicit `hold_ms = 0`
always means edge-triggered.

### 7.3 Arbitration ladder and idle grounding

The reconciler's fixed three-tier ladder (the code fixes the order, not the
configuration):

```
Tier 1  idle      station activity == inactive      → target = off (grounded), source = idle
Tier 2  operator an operator hold is active          → target = the held port,  source = operator
Tier 3  auto      band policy(radio.band)            → target = resolved port,  source = auto
```

- The highest asserting tier wins. **Tier 1 wins over an operator hold** —
  deliberate walk-away safety: a forgotten operator hold cannot keep an antenna
  hanging live. An operator hold still asserts while the radio is offline, and
  `mode` stays `manual` while a hold is active, even when tier 1 wins the target.
- **The reconciler infers station activity, and no operator sets it**. A radio
  `/state` message whose `freq_hz` differs from the last-seen value marks the
  station `active` and resets the idle clock. So does a message with
  `tx == "tx"`. The reconciler treats unknown activity as `active`.
- **Idle grounding**: after 30 idle minutes (default `idle.timeout_minutes = 30`),
  checked on a 5 s cadence, the reconciler selects the switch's `off` position.
  That position shorts all antenna feeds to electrical ground — lightning and
  walk-away protection. Grounding lands within ~5 s of the deadline.
- **Re-arm**: the only re-activation trigger is a radio `/state` message with a
  changed `freq_hz` or an actual transmission. Operator commands do not count as
  activity. Known recovery fragilities (all documented live behavior, candidates
  for redesign — OD-10): no manual (non-TX) re-arm path exists. A
  deferred-during-TX select can send the first key-up after grounding into the
  short. The TX deferral has no timeout. A reconciler restart re-births
  `lastActivity = now` and therefore always un-grounds the station and resets the
  30-minute clock.
- **REQ-GROUND-1.** The reconciler must ground the station to `off` after the
  configured idle timeout, with no VFO activity and no transmission. (**VFO** —
  variable-frequency oscillator, the radio's tuning control, hence "frequency
  change".)
- **REQ-GROUND-2.** Grounding must override an operator hold.
- **REQ-GROUND-3.** The station node (`muehle/hf`, `muehle/uhf`) must carry the
  inferred `activity {active|inactive}` flag as its single non-structural field.
  The reconciler derives it. No deployed component publishes the station
  activity topic today: the deployed reconciler holds activity as internal state
  only. Who publishes `<station>/state` is an open scope decision, in the same
  class as the host nodes (OD-12).

### 7.4 Cold-switch ordering contract

- **REQ-COLD-1.** For any switch that declares `hot_switch: false` (the antenna
  switch does), RF-inhibited conditions must come before every port change: the
  reconciler withholds during TX, and hardware interlocks also enforce this.
- **REQ-COLD-2.** A disarm-and-confirm of the PA must come before any tuning
  carrier. There is no hardware tune line, so the automation routes the TUNE
  action: operators publish tune requests to the sequencer, never directly to the
  radio. Disarm always leads the carrier. The tune-request routing is aspirational
  today — the deployed sequencer has no tune path (see OD-20).

---

## 8. Runtime-library constraints (any stack must respect these)

These constraints exist because of production incidents with the reference stack's
MQTT client library. But they are **stack-independent rules**. Whatever messaging
library the new implementation uses, its threading and failure model must satisfy
them. The rationale quotes the live incidents.

1. **REQ-RT-1 (handlers never block or publish).** Inbound-message handler code must
   not block, do long work, or call a blocking publish on the messaging client's
   delivery thread. Handlers must only capture the message and enqueue work.
   *Rationale (live incident):* in the reference library, handlers run inline on the
   connection's dispatch thread. A handler that blocked on publish deadlocked the
   discovery consumer live after the first retained-message burst (the client
   deadlocks after the first message and consumes nothing further). The same
   latent bug existed in the reconciler before deploy.
2. **REQ-RT-2 (serialized single worker).** All state mutation and publishing for
   one component must run on exactly one worker, strictly FIFO, ordered as
   enqueued. The reconciler's deduplication and re-assertion logic depends on
   sequential, ordered updates. Do not fan out to a pool.
3. **REQ-RT-3 (bounded queue, drop-on-full, never block the delivery thread).** The
   handler→worker queue must have a bound. When it is full, the queue **drops the
   work silently** — a blocking submit can reintroduce REQ-RT-1 through the
   back door. This is safe only because of REQ-RT-5 (retained-snapshot replay):
   every dropped input gets a re-arm from the next change, or from the retained
   replay on reconnect.
4. **REQ-RT-4 (connect must be abortable on shutdown).** Process shutdown must
   interrupt an in-flight broker connection try within a stated small bound
   (the reference returns within milliseconds. Pick and pin a bound, for example
   less than 1 s, as an acceptance test), independent of broker reachability.
   On shutdown, the runtime must tear down the
   half-open connection on both the failure path and the cancellation
   path. *Rationale (live
   incident):* the reference library's connect call blocked, and ignored
   cancellation. A service that received a shutdown signal during a broker outage
   hung until the supervisor force-killed it (this hit the PA bridge live). A new
   stack must ideally make cancellation cancel the actual connect try, not just
   the caller's wait.
5. **REQ-RT-5 (retained-replay recovery).** On every (re)connect, a component must
   re-run its full birth sequence — publish retained `online` on `/status`,
   re-publish `/meta`, re-publish its retained `/state` snapshot, and re-subscribe
   to all inputs. The client's session semantics must re-deliver all retained
   input messages on every reconnect. A stateless consumer can then re-seed its
   inputs. See OD-5 for the session-flag nuance, and for the one component that
   deviates. Recovery after any outage is always "replay retained state and
   re-resolve", never "resume a persisted internal model".
6. **REQ-RT-6 (bounded publishes).** A publish must stay bounded in time (the
   sequencer uses a 10-second wait timeout), so a dead broker surfaces as an error
   and does not stall the component forever.
7. **REQ-RT-7 (auto-reconnect).** After a successful connect, connection loss must
   trigger automatic reconnection with backoff (reference defaults: 1 s at the
   start, and 10 s max. The curve itself is an implementation detail. The rule is
   recovery without a person present).
8. **REQ-RT-8 (no credentials in shared plumbing).** Any shared runtime library must
   stay credential-free. Broker addresses, users, and passwords flow only through
   per-component configuration (`05-deployment-ops.md`).
9. **REQ-RT-9 (malformed input never crashes).** The component must log and drop
   malformed JSON on any input, and keep the previous inputs. A consumer that sees a
   malformed `/state` must delete that slot's cached snapshot (a good→bad
   transition must not leave a stale value poisoning decisions).
10. **REQ-RT-10 (worker robustness).** The worker must exit cleanly on
    cancellation or queue close, at any point in the component's lifecycle
    (including non-cancellation early returns), and must never invoke work after
    close.

**Acceptance tests for the messaging layer (normative).** A re-implementation
must pass each test below for its messaging library:

1. A submit to a full queue does not block within a bounded time (REQ-RT-3).
2. The worker runs jobs in order and exits on cancellation (REQ-RT-2, REQ-RT-10).
3. The worker exits cleanly, with no call on a closed worker, when the queue
   closes without cancellation (REQ-RT-10).
4. A cancelled context returns from an in-flight connect within the bound that
   REQ-RT-4 states.
5. The topic builders produce the exact grammar strings of §2.1 and §3.
6. The command envelope round-trips `{"action", "value"}` (REQ-CMD-1).

Known fragilities of the reference implementation: silent queue drops have no
counter and no telemetry, and a job that panics kills the worker. A
re-implementation can do better than the reference on these two points, but must
not do worse.

**Reference-implementation notes (non-normative).** The deployed system centralizes
these in a ~130-line shared library module that every bridge imports. The module
contains a context-aware connect wrapper (it races the library's connect token
against process cancellation, and disconnects the half-open client on either
path). It also contains a bounded jobs-queue with a single worker goroutine
(buffer sizes 32/64/256 depending on bridge). The topic-string builders and the
`{action, value}` payload type live in a sibling schema package. Each bridge
still constructs and owns its own MQTT client, options, and subscriptions. The
shared layer is deliberately a thin set of behavior workarounds, not a client
wrapper. Visibility rules forbid cross-bridge imports of device logic. Shared
plumbing can be common. Per-device logic cannot.

---

## 9. Hosts and processes

| Host | What it is | What runs there |
|---|---|---|
| MQTT broker, `192.168.1.50:1883` | The one shared service. Persistent store (retained messages survive a broker restart). User `hf`, and per-service credentials in on-host config files. | Nothing else — the broker hosts no logic. |
| `shari` (192.168.1.139) | Raspberry Pi single-board computer, the deployment target for all supervised services. | Radio bridge, beam-controller bridge, PA bridge, tuner bridge, HF rotator bridge, smart-plug bridge (both power slots), antenna-selection reconciler, power sequencer, discovery consumer. Each is a separate hardened service process with a dedicated unprivileged user (see `05-deployment-ops.md`). |
| `shack-pc` | The shack PC. | The UHF rotator console **only** — deliberately an interactive terminal program that the operator starts by hand, not a daemon. Its arming is a local keyboard act, never remote. An always-on headless process contradicts its safety model. |
| Embedded devices (wifi) | The antenna switch (relay board with its own firmware) and the M5 Stamp relay controller #1. | Their firmware speaks the canonical four-plane schema directly. Device, adapter, and host collapse into one node. |
| M5 Stamp relay controller #2 | Intended UHF polarization relay controller. | **No firmware exists in the repo** — a documented gap (OD-13), not a component. |
| Tablet (Android) | Operator console. | A pure consumer/UI application. It subscribes to state and publishes canonical `/cmd` topics like any other writer. Never load-bearing. See `04-console.md`. |
| Devices themselves | FLEX-8400 radio, ACOM 1200S PA, ATR-1000 ATU, Ultrabeam RCU-06, WRC rotator controller, Shelly smart plugs. | Speak their native protocols only. Their bridges translate. |

Host liveness: the model defines `muehle/host/shari` and `muehle/host/shack-pc`
nodes (publishing `online`, and on shari `temp_c`, `load`). **No component in the
repo publishes them** — they are model-only (OD-12). Host loss is now visible only
through its bridges' LWTs going `offline` together.

The logging layer's topics (§3.4) exist as a specification, not as code. When
someone builds them, the slots live at the position level (host = the operator's
PC).

---

## 10. Open decisions and unresolved facts

Anything the sources disagree on or leave unknown, with evidence. A re-implementation
must resolve each item explicitly, and not inherit an accident.

1. **OD-1 — Ultrabeam antenna-switch port: 3 or 4**. The sources disagree. The
   port-3 camp: the repo's top-level docs, the integration model §7.1, and the
   reconciler's unit tests. The port-4 camp: the reconciler's example
   configuration, its deploy seed, and the console's antenna map. The console
   code also comments that port 2 and port 3 are not wired at Mühle, which
   strengthens the port-4 camp. The integration model is inconsistent within
   itself about the fan-dipole port: its passive-resource list says port 2, and
   its wiring map says port 6. Therefore "port 6 = fan dipole" is not consistent
   across all sources. Port 1 = dummy load and `off` = grounded do agree. The
   live config file on shari is authoritative, but nobody managed to read it
   from the workstation at the time of writing this PRD. The rule regardless of
   outcome: port numbers are pure configuration (REQ-RES-3).
   **Action: confirm on-device before wiring anything.**
2. **OD-2 — Broker topology.** All deployed defaults point at the broker at
   `192.168.1.50:1883` (now production). Work exists on an unmerged feature branch
   to move components to a broker on shari (`192.168.1.139`). The branch carries
   the work, but it stays **undeployed** as of 2026-08-29. The PRD treats
   192.168.1.50 as production.
   The migration is a decision point (it changes only the `broker` configuration
   value — nothing in the contracts).
3. **OD-3 — `device_online` when true: omitted or explicit.** The integration model
   says the field is "omitted when true". The deployed bridges (radio, beam
   controller, PA, tuner) publish `device_online: true` explicitly. Consumers must
   treat both forms as the same (REQ-LIVE-1), unless the new implementation makes
   one form mandatory — decide and document.
4. **OD-4 — clean-shutdown `/status`.** Retained `"online"` persists after a clean
   stop when the component's own final `offline` publish does not happen (no will
   fires on a graceful disconnect). The actual behavior is on record (§4.3). Open:
   whether the new implementation adds a stronger promise (supervisor-published
   offline, expiry semantics), or keeps consumer-side freshness rules (REQ-LIVE-5).
5. **OD-5 — session semantics.** The ecosystem convention says persistent sessions
   ("clean session: no"), so subscriptions and queued messages survive client
   restarts. But stateless consumers (the reconciler, the discovery consumer)
   deliberately use **clean sessions**, so retained messages replay on every
   reconnect. Without the replay, they can wake with empty inputs and never
   resolve. The sequencer uses a persistent session and re-subscribes
   unconditionally, with a known side effect: stale subscriptions for removed
   config entries are never unsubscribed. The stack-agnostic rule is REQ-RT-5
   (retained inputs re-delivered on every reconnect). Which session flag achieves
   it per component is the decision.
6. **OD-6 — radio state heartbeat period.** REQ-TX-3 demands a periodic republish
   of ≤5 s (the documented fix direction "for example every 5 s"). The receiver
   window is exactly 10 s in the deployed firmware. The exact period is a tunable
   default — pick and pin one. The incident-derived rule is the invariant.
7. **OD-7 — sequencer `stop` semantics.** Docs say the sequencer honors `stop` only
   from `phase=running`. The code (authoritative) also honors it from `idle` **with
   a fault** (teardown of a half-started station). The sequencer drops `stop` from
   `idle` without a fault. So nobody can shut down, through the sequencer, a
   station that an operator started by hand. And no command can cancel a
   sequence in progress. This is a deliberate contract. A new team can change it,
   but that is a contract change, not a fix.
8. **OD-8 — `wait_state` freshness.** The sequencer's liveness precondition checks
   the now `/status`, but the waited `/state` snapshot can be arbitrarily old (a
   device online per LWT, but with a dead device link, can satisfy a wait on a
   stale field). The two-layer liveness convention does not reach wait *payloads*.
   Decide: a freshness bound, or `device_online` coupling for `wait_state`.
9. **OD-9 — `/cmd` value types.** The convention (REQ-CMD-2) says `value` is always
   a JSON string, booleans as `"true"`/`"false"`. Deployed drift, checked in code:
   the reconciler sends and the tuner bridge accepts a JSON **boolean**
   (`{"action":"set_inline","value":true}`), and the tuner's expose descriptor
   declares `value_type: "bool"`. The antenna controller uses an integer under
   `freq_hz`. The antenna switch uses `{"select":"…"}` with no `action` key (both
   allowed by their expose descriptors). Decide: enforce string-only (this changes
   the tuner's contract), or formalize per-descriptor value types (the de-facto
   state now).
10. **OD-10 — grounding recovery design.** Known live defects exist in the
    reconciler's recovery half (§6.3, §7.3). The first key-up after grounding can
    go into the grounded (shorted) port. There is no non-TX re-arm path (an
    operator at the desk cannot un-ground without touching the VFO or
    transmitting). The TX deferral has no timeout (a frozen `tx` freezes all
    actuation). A reconciler restart always un-grounds and resets the idle clock.
    The reconciler never validates operator requests — any non-empty string
    becomes a hold target that it forwards verbatim to the switch. The switch's
    `settled` flag arrives but nothing uses it. Publish failures advance dedup
    state (the reconciler does not retry a failed intent until a different
    resolution or a reconnect).
    The research files list fix directions (operator presence as activity,
    deferral timeout, restart-stable idle state, request validation,
    `settled`-gating). Each is a design decision for the re-implementation, with
    the behavior now as the documented baseline.
11. **OD-11 — reconciler single point.** If the reconciler dies (and the
    supervisor's restart gives up), antenna selection and all follow bindings
    stop, and the station degrades to manual. The design accepts this only
    because RF safety is hardware. Open wishes: a supervision policy and an
    explicit operator-facing "reconciler offline" indication (today only the LWT
    `status` topic exists).
12. **OD-12 — host liveness nodes.** `muehle/host/shari` and `muehle/host/shack-pc`
    appear in the model, but no bridge publishes them. Model-only. shari is itself
    a single point (it fronts the HF PA, tuner, rotator and beam bridges *and*
    hosts the reconciler). Host-liveness publication is an open scope decision.
13. **OD-13 — UHF polarization controller firmware gap.** The slot table
    attributes `muehle/uhf/pol-ctrl` to M5 Stamp relay controller #2. But no
    firmware for controller #2 exists in the repo. It is a documented gap, not a
    component. A re-implementation scope decision.
14. **OD-14 — logging layer.** `docs/logging-integration-model.md` gives
    `log/event`, `spots/event` planes and a `qso-log` role. **No component has
    this behavior today.** Future scope ("specified, not yet built") — describe it
    in milestones, and do not build it as behavior of the system now.
15. **OD-15 — sequencer first-deploy seed defect.** The deployed deploy script
    seeds a config **without** the needed step lists. A first deployment therefore
    crash-loops until an operator hand-adds them from the example config. A
    re-implementation must ship a complete seed, or a built-in default sequence.
16. **OD-16 — legacy component names**. Two deployed bridge names violate the
    naming convention. The radio bridge and the beam-controller bridge names embed
    device hints instead of the `<devtag>-<function>-bridge` pattern, and one PA
    bridge name is deliberately model-specific. The team deferred renames because
    they touch live service units. A rename never changes slot addresses. Names
    can change in a re-implementation, as long as the address map stays the same.
17. **OD-17 — QoS 2.** The convention reserves QoS 2 (exactly-once) for a
    documented must-never-double-fire command. No such command exists now. Nothing
    uses QoS 2, and the decision can wait until such a command appears.
18. **OD-18 — K9AY receive-loop controller.** `hf/rx-loop-ctrl` appears in the
    integration model (receive-loop direction control, its preamp dropped by the
    hardware TX-low line), but not in the deployed slot table or the repo as a
    component. Scope and slot assignment stay open.
19. **OD-19 — embedded discovery code in two bridges.** The deployed radio bridge
    and beam-controller bridge carry embedded home-automation discovery code,
    gated off by `publish_ha_discovery` (default `false`) with `discovery_prefix`
    (default `homeassistant`). This deviates from REQ-ARCH-4 as a disabled
    leftover. The team will delete the code once the passive discovery consumer
    is proven live. With discovery off, the canonical planes do not change, so
    a re-implementation can omit it entirely.
20. **OD-20 — operator tune routing.** The sequencer-routed tune path in
    REQ-COLD-2 is aspirational. The deployed sequencer accepts only `start` and
    `stop`, and its default sequences hold no tune step or tune command.
    No operator-issued tune path exists on the bus today. A re-implementation
    must decide where the operator tune request lands (a sequencer action, a
    dedicated step, or another owner). That decision is a contract addition,
    not a reproduction.