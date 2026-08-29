# 02 — Interface Specification: The Wire Contract

Status: draft for review. Scope: the complete MQTT bus contract of the Mühle station
automation system. Audience: an engineering team that knows neither amateur radio nor
this project.

> **How to read this document.** The rules let a competent software engineer
> re-implement the station's messaging layer **byte-compatibly** on the MQTT bus. The
> engineer needs no knowledge of amateur radio, MQTT culture, or this project. Every
> requirement is a testable
> statement that uses MUST and MUST NOT. The statement "the bridge MUST republish
> `/state` within 10 s of a field change" is the required register throughout. Exact
> topic strings, exact JSON key spellings, exact types, exact units, exact timings, and
> exact error strings are the subject matter. Approximations are defects. Technology
> names (Go, paho, Flutter, ESPHome, systemd) appear **only** inside clearly marked
> *Reference-implementation notes* and are never requirements.

## 0. Purpose

This document is the normative **wire contract** for the Mühle amateur-radio station
automation system. It gives the complete set of MQTT topics, JSON payload shapes, field
types and units, and command conventions. It also gives the timing and heartbeat rules,
the liveness semantics, and the cross-slot invariants. Together these define how every
station component communicates. The sections after this one establish the shared
conventions (§1), the common payload grammar (§2), and the per-slot reference (§3).
Section 3 is the heart of the document. The later sections add the band/mode
canonicalization tables (§4), the two-layer liveness contract (§5), and the cross-slot
invariants (§6). They also give the open decisions and the unresolved facts (§7).
A re-implementation that satisfies every MUST statement in §3 and §6, using any
technology stack, is conformant. Sibling documents: system overview
([README.md](README.md), [00-system-overview.md](00-system-overview.md)), component
architecture ([01-architecture.md](01-architecture.md)), per-component deep-dives
([03-components/](03-components/)), the operator console ([04-console.md](04-console.md)),
deployment ([05-deployment-ops.md](05-deployment-ops.md)), safety ([06-safety.md](06-safety.md)),
and roadmap ([07-priorities-milestones.md](07-priorities-milestones.md)). The
station-level design rationale lives in `docs/station-integration-model.md` in the
source repository.

### 0.1 Glossary (plain-language definitions, first-use terms)

- **Amateur radio ("ham") station**: a licensed, non-commercial radio installation, here
  the "Mühle" station. Its parts are a transceiver, a power amplifier, an antenna
  tuner, antennas, and the machinery to route and point them.
- **HF / UHF**: high frequency (roughly 3–30 MHz, long-distance "shortwave" bands) and
  ultra-high frequency (roughly 300 MHz–3 GHz). The station has one HF chain and one UHF
  rotator.
- **Transceiver (TRX, "the radio")**: a combined radio transmitter and receiver — here a
  FLEX-8400. "TX" = transmitting, "RX" = receiving.
- **PA (power amplifier)**: a device that boosts the transceiver's output to high power —
  here an ACOM 1200S, up to 1200 W.
- **ATU / antenna tuner**: an impedance-matching network placed in the feed line ("in
  line") or bypassed. Needed when an antenna is not resonant on the operating band —
  here an ATR-1000.
- **SWR (standing-wave ratio)**: a measure of feed-line impedance match. 1.0 is perfect,
  ≥ 3.0 is dangerous (reflected power heats the PA).
- **Rotator**: a motor that turns a mast-mounted directional antenna. Azimuth (az) is the
  horizontal pointing angle in degrees.
- **Ultrabeam**: a motorized tubular beam antenna whose element lengths are motor-tuned
  per band. It supports forward / 180°-reversed / bi-directional radiation patterns and
  full element retraction, driven by an "RCU-06" controller.
- **Antenna switch**: a relay board connecting exactly one of several antennas to the
  radio. Its `off` position shorts all antenna feeds to electrical ground (lightning
  protection).
- **Dummy load**: a heat-dissipating resistor that radiates nothing. Used for testing.
- **Bridge**: a small service that translates one physical device's protocol (serial,
  Ethernet, WiFi relay, and so on) onto the MQTT bus.
- **MQTT**: a lightweight publish/subscribe message protocol. Clients publish messages
  to hierarchical **topics** (slash-separated strings). A central **broker** relays them
  to subscribers.
- **Retained message**: a message the broker stores per topic and re-delivers to every
  future subscriber until overwritten or cleared by a zero-length retained publish.
- **QoS**: how firmly MQTT delivers a message. QoS 0 = at most once. QoS 1 = at least
  once, and QoS 1 messages can arrive twice. All station publishes use QoS 1 unless
  stated otherwise.
- **LWT (last will and testament)**: a message registered with the broker at connect
  time. The broker publishes it on the client's behalf if the client disconnects
  *uncleanly* (crash, network loss, kill -9).
- **Slot**: one component's topic namespace on the bus, an address of the form
  `<site>/<station>/<slot>` — for example `muehle/hf/pa`. This is the unit of station identity.
- **Plane**: one of the four per-slot topic suffixes `meta`, `state`, `status`, `cmd`
  (defined in §1.2).
- **Logic slot**: a slot with no physical device behind it (a pure MQTT service).
- **Band**: the wavelength name of an operating frequency — for example `20m` for the 14 MHz
  amateur band. Canonical table in §4.1.
- **Mode**: the emission mode of a transceiver (CW = Morse on/off keying, USB/LSB =
  upper/lower sideband voice, AM, FM, and `data` for all digital modes).
- **Slice**: one receive channel inside the radio. The radio can run several at the
  same time. The bridge reports the lowest-index active one (§3.3).
- **Key-up / keying line**: the transceiver's transmit-request signal path — the wire
  that switches the amplifier into transmit. The PA
  amplifies only while this line is active, so a relay in this line works as the PA
  arm interlock (§3.7).
- **IARU Region 1**: the amateur-radio band-allocation plan for Europe, Africa, and
  the Middle East. It defines the band edges in §4.1.
- **DX spot**: a report that says another operator heard a distant ("DX") station on
  some frequency.
- **Maidenhead locator / grid square**: a ham-radio geographic encoding. 4 characters
  denote a 2°×1° "square", 6 characters a ~5′×2.5′ "subsquare".
- **Shari**: the Raspberry Pi (192.168.1.139) that hosts the station's services.
- **shack-pc**: the Windows PC in the radio shack that hosts the interactive UHF
  rotator console.

---

## 1. Conventions

### 1.1 Topic grammar

Every station component ("slot") owns exactly one address of the form:

```
<site>/<station>/<slot>
```

- `site` = `muehle` (fixed for this deployment).
- `station` = `hf` (the HF station) or `uhf` (the UHF station) or `power` (the supply
  layer, shared across stations).
- `slot` = the component name, for example `radio`, `pa`, `ant-switch`, `power-seq`.

The full list of slot addresses is normative (§3). A component MUST NOT publish under
any address other than its own. It MUST NOT publish to another slot's `meta`, `state`,
or `status` plane under any circumstances. Another component commands a slot only by
publishing to that slot's `/cmd` topic (§1.4).

Client IDs: every bridge MUST use the MQTT client ID `<site>-<station>-<slot>`
(for example `muehle-hf-radio`), unless a component documents otherwise: the operator console
uses `hf-console-<epoch-milliseconds>` (unique per app launch). The `testui` diagnostic
relay uses `testui`. The traffic capture tool uses
`<site>-<station>-hf-mqtt-capture`. A client ID MUST NOT collide with any slot-derived
bridge client ID unless the client *is* that bridge.

### 1.2 The four planes

Each slot exposes exactly four topics:

| Topic | Plane | Payload | Retained | QoS | Direction |
|---|---|---|---|---|---|
| `<addr>/meta` | meta | JSON "birth certificate" (§2.2) | yes | 1 | bridge → bus. |
| `<addr>/state` | state | ONE full JSON snapshot (§2.1) | yes | 1 | bridge → bus. |
| `<addr>/status` | status | literal string `online` or `offline` (NOT JSON) | yes | 1 | bridge → bus. |
| `<addr>/cmd` | cmd | JSON command (§1.4) | see §1.5 | 1 | bus → bridge. |

Rules (all testable):

- **R1.1** A slot MUST publish its `/meta` on every broker (re)connect.
- **R1.2** A slot MUST publish `online` on its `/status` topic on every broker
  (re)connect, and MUST register `offline` (QoS 1, retained) as its MQTT LWT at connect
  time.
- **R1.3** `/state` MUST be a single JSON object containing the slot's complete
  condition — never per-field topics, never partial updates that need a merge. The
  latest message fully replaces any previous one.
- **R1.4** `/status` MUST be the plain ASCII string `online` or `offline` — not JSON,
  not `true`/`false`.
- **R1.5** A zero-length **retained** publish on any plane topic clears that plane's
  retained value. Consumers MUST treat an empty payload as "cleared/absent", not as an
  empty value. This is the standard MQTT retained-clear idiom, and live slots use it.
  The Ultrabeam controller clears its own retained `/cmd` after executing `retract`
  (§3.4), and the discovery consumer clears decommissioned entities (§3.13).
- **R1.6** Any component that is not a slot (the console, capture tool, testui) MUST
  NOT publish any `meta`/`state`/`status` plane of a slot-derived address. The operator
  console in particular has **no MQTT presence of its own** — no slot, no meta, no
  state, no LWT, no heartbeat. Its only bus footprint is the `/cmd` messages it
  publishes (§3.15).

### 1.3 Plane semantics

- **`meta`** is static identity + capability self-description: who/what this slot is,
  what it can do, and (through the `expose` block, §2.3) which state fields exist and how to
  command them. It changes only when the bridge's software or configuration changes.
- **`state`** is the live condition: telemetry, derived values, and (when applicable) the
  `device_online` device-link flag (§2.1, §5).
- **`status`** is bridge-process liveness only. It says nothing about the device behind
  the bridge (that is `/state.device_online`), and — critically — it can lie after a
  clean shutdown: see §5.3.
- **`cmd`** is intent input. The default payload grammar is §1.4. Retention policy §1.5.

### 1.4 The command convention: `{"action", "value"}`

The default command payload is a JSON object:

```json
{ "action": "<action-word>", "value": "<string argument>" }
```

- **R1.7** The argument MUST ride under the key `value` — never under a key named after
  the action (a live incident on the antenna tuner traced to exactly this mistake).
- **R1.8** The argument MUST be a JSON **string**. A boolean travels as the string
  `"true"` / `"false"`. (Documented exceptions below and in §3 carry other JSON types,
  each declared by the target slot.)
- **R1.9** Button-style commands (no argument) MUST be `{"action": "<word>"}` with no
  `value` key at all. Sending `"value": null` is a defect: historically this payload
  arrived as an empty string, and then bridges dropped it.
- **R1.10** A command recipient MUST ignore unknown actions, unknown keys, and
  unparseable JSON payloads (log-and-drop), and MUST NOT disconnect or crash on them.
  Malformed input never has bus-visible side effects.

**Declared exceptions to the string-`value` rule** (each is normative for its slot.
Details are in §3):

| Slot and action | Exact payload | Deviation |
|---|---|---|
| `hf/ant-ctrl` `frequency` | `{"action":"frequency","freq_hz":<int>}` | integer Hz under key `freq_hz`. |
| `hf/rotator` `set_az` | `{"action":"set_az","az":<number>}` | number under key `az`. |
| `hf/ant-switch` (all) | `{"select":"off"\|"port1"..\|"port6"}` | **no `action` key**. String under `select`. |
| `hf/antenna-select` (all) | `{"request":"<value>"}` | **no `action` key**. String under `request`. |
| `hf/tuner` `set_inline` | `{"action":"set_inline","value":<bool>}` | `value` is a real JSON boolean. |
| `hf/pa-arm` `set_enabled` | `{"action":"set_enabled","value":"true"\|"false"}` | **string required** — a JSON boolean `true` DISARMS (§3.7). |
| `uhf/rotator` `stop` | `{"action":"stop"}` | the only command this slot accepts. |

Every writable `expose` field declares its own command shape through the `command`
descriptor (§2.3), so a re-implementation can derive all of the above mechanically from
each slot's `/meta`.

### 1.5 Retention policy per plane

- **R1.11** Senders MUST always publish `meta`, `state`, `status` retained.
- **R1.12** A sender MUST NOT retain `cmd` **by default**. Commands are one-shot
  intents. A broker replaying a stale command on a device's reconnect is a hazard.
- **R1.13** Exception — a sender MUST publish **idempotent steady-state setpoints**
  retained so that the target device re-applies the last intent after its own restart
  ("self-healing"). The retained-set is exactly:

| Retained `/cmd` topic | Retained actions |
|---|---|
| `muehle/power/master/cmd` | `set_power`. |
| `muehle/power/psu-13v8/cmd` | `set_power`. |
| `muehle/hf/switch/cmd` | `set_pa`, `set_trx`. |
| `muehle/hf/pa-arm/cmd` | `set_enabled`. |
| `muehle/hf/ant-switch/cmd` | `select`. |
| `muehle/hf/antenna-select/cmd` | `request`. |
| `muehle/hf/ant-ctrl/cmd` | `frequency`, `direction`, `band` (**not** `retract`, §3.4). |

- **R1.14** The senders of all other `/cmd` topics MUST NOT retain them: `hf/pa`,
  `hf/rotator`, `hf/tuner`, `hf/power-seq`, `hf/radio`. (The sequencer emits retained
  commands *to* other slots per R1.13 but its own `/cmd` is one-shot, §3.12.)
- **R1.15** A sender MUST NOT leave a one-shot action retained. The Ultrabeam controller,
  after executing `retract`, MUST clear its retained `/cmd` topic with an empty retained
  publish so a restart never re-retracts.
- **R1.16** The sequencer MUST subscribe to its own `/cmd` at QoS 0, and publishers
  MUST NOT retain messages to it. A command issued while the sequencer is down is then
  lost instead of replayed at reconnect (this prevents spurious station energization,
  §3.12).

### 1.6 Data types and units on the bus

- **R1.17** Timestamps: the `ts` field MUST be RFC 3339, UTC (`2026-07-14T18:42:01Z`).
  *Documented deviation:* the `hf/switch` and `hf/pa-arm` slots publish `ts` as
  **process-uptime milliseconds** (a JSON number), not a wall-clock string (§3.6, §3.7).
  Also `hf/ant-switch` derives `ts` from its Home-Assistant instance's clock, which is
  wrong when that instance is down. Consumers MUST treat both deviations as facts. We
  recommend that a re-implementation fix them (§7).
- **R1.18** Frequencies MUST be integers in **Hz** (`freq_hz`), never kHz, never MHz.
- **R1.19** Power in **watts** (`fwd_power_w`, `rfl_power_w`), temperature in **°C**
  (`temp_c`), SWR as a dimensionless **ratio** (`swr`), azimuth in **degrees**
  (`az`, float), inductance in **µH** (`l_uh`), capacitance in **pF** (`c_pf`).
- **R1.20** Band names and modes MUST use the canonical tables in §4, derived from
  `freq_hz`. A band value MUST never be a stored setpoint echoed back.
- **R1.21** JSON booleans are real booleans except where a slot documents the
  string form (R1.8, §3.8).
- **R1.22** Omitted-when-empty: the fields that a slot's table marks as omitted MUST be
  absent from the JSON when they have no value. Such a field is never `null`, `""`, or
  `0`. A consumer MUST distinguish "absent" (unknown) from any present value.

### 1.7 Broker and session topology

- The production broker is Mosquitto at `tcp://192.168.1.50:1883`, MQTT 3.1.1 over plain
  TCP (no TLS). This is the normative production address as of 2026-08-29.
- Accounts: services use the `hf` user. The operator console uses a dedicated
  `console` user with a narrow ACL (subscribe `muehle/#`, publish `muehle/+/cmd`).
- **R1.23** Credentials MUST exist only in 0600 config/env files on the target host.
  Never on a command line, never in a systemd unit, never in shell history.
  (Deployment detail in [05-deployment-ops.md](05-deployment-ops.md).)
- **R1.24 (library-independent constraint)** — **non-blocking handlers**: an incoming
  MQTT message handler MUST NOT block inside the client's receive/dispatch path. It
  MUST NOT in particular synchronously publish and wait for broker acknowledgement
  there. Handlers MUST copy the payload bytes, enqueue a closure onto a bounded queue
  (drop-on-full, never block), and return. A single worker MUST drain the queue and do
  all mutating/publishing work, serialized. *Rationale (live incident):* the discovery
  consumer deadlocked in production — its process stayed alive, and its log froze
  after the first retained message. Its handler published synchronously
  from inside the dispatch
  callback. Queue sizes in the reference implementation are 256 (antennaselect,
  ultrabridge, hadiscovery), 64 (shelly-power-bridge), 32 (flexbridge, pelcobridge2).
  Any bounded size works, blocking does not.
- **R1.25 (library-independent constraint)** — **interruptible connect**: process
  shutdown (SIGTERM) during a broker outage must quickly stop a pending connect. A
  connect that blocks until the supervisor SIGKILLs the process is a
  defect. (This hit the PA bridge live: shutdown hung until systemd escalated.)
- **R1.26 (library-independent constraint)** — **bounded publishes**: every publish
  MUST have a time bound (reference value: 10 s wait timeout) so a dead broker
  surfaces as an error, not a hang.
- Session policy is per-component and normative: device bridges and passive consumers
  can use persistent sessions (`CleanSession=false`). **Logic consumers that re-seed
  state from retained replay (the antenna reconciler) MUST use clean sessions**
  (`CleanSession=true`) so that every (re)connect replays all retained inputs. A
  persistent session delivers nothing on reconnect, and the reconciler can wake up
  blind. See §5.4 for the full session matrix and the flagged divergence.

---

## 2. Common payload grammar

### 2.1 Common `/state` fields

Three fields recur across slots:

| Field | Type | Presence | Semantics |
|---|---|---|---|
| `ts` | string, RFC 3339 UTC (see R1.17 deviations) | always | Snapshot timestamp of this `/state` publication. |
| `device_online` | boolean | always present on physical-device bridges. **Omitted** by logic slots and by the embedded antenna switch. | True iff the bridge now has a working data link to its physical device. The link needs a done handshake and an up serial, WebSocket, or Ethernet line. This is **not** bridge-process liveness (that is `/status`). See §5. |
| `error` | string | omitted when empty | Last error text, verbatim. Empty means healthy. See per-slot tables for exact strings. |

- **R2.1** On loss of the device link a bridge MUST publish a `/state` with
  `device_online: false` promptly (each slot's table gives the detection timeout).
- **R2.2** Deployed bridges publish `device_online: true` **explicitly**. The
  integration-model document instead says the field is "omitted when true". Consumers
  MUST treat both forms the same (**absence = true** for slots that must publish it.
  A missing *snapshot* still means offline, §5.2. Safety-critical consumers are the
  one exception, R5.2a). Whether a re-implementation
  mandates explicit-true is an open decision (§7.3 item 1).
- **R2.3** A bridge MUST clear a present `error` string by republishing `/state`
  without the key (omitted-when-empty), never as `""` or `"none"`. Exception —
  the PA slot publishes the literal `fault:"none"` when clear (§3.9, historical
  quirk). The PA leaves `error` empty or omits it when its fault byte is `0xFF`.
  The word `NONE` is only the firmware message-table label for byte `0xFF`, never
  a value on the wire.

### 2.2 The `/meta` envelope

Every slot publishes, retained, a JSON object of this shape. Per slot, keys other than
`schema` and `role` need not appear. Consumers MUST ignore unknown keys:

```json
{
  "schema": "1.0",
  "role": "<role word>",
  "device": { "model": "...", "serial": "...", "firmware": "..." },
  "link": "ethernet|serial|wifi|rs485|none",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": { "<key>": "<value or array>" },
  "expose": { "device": {...}, "fields": [...], "actions": [...] }
}
```

| Key | Type | Semantics |
|---|---|---|
| `schema` | string | Exactly `"1.0"`. Consumers reject anything else (log-and-skip). |
| `role` | string | The slot's functional role — one of: `radio`, `ant-ctrl`, `ant-switch`, `switch`, `pa-arm`, `reconciler`, `pa`, `rotator`, `tuner`, `sequencer`, `discovery`, `power`. Non-empty, required. The `hf/switch` slot publishes `switch` (deployed value). |
| `device` | object | Physical device identity: `{model, serial, firmware}`. `serial` is a configured label when the device protocol reports none (for example the PA). **Compound-device tie:** when one physical device serves two slots (the PLC serving `hf/switch` and `hf/pa-arm`), both slots MUST carry `device.model` and `device.serial` in exactly the same form. That equality IS the mechanism by which consumers learn that the two slots share one device. |
| `link` | string | How the bridge talks to the device: `ethernet` (radio), `serial` (PA, ant-ctrl), `wifi` (antenna switch, PLC relays, tuner), `rs485` (UHF rotator), `none` (logic slots). No deployed slot publishes `embedded`. The antenna switch is an embedded node (no separate bridge process), but it publishes `wifi` because its own WiFi/MQTT connection is the transport. |
| `location` | string | Physical label, deployment-wide `bauwagen`. |
| `host` | string | Compute node: `shari` for services. `shack-pc` for the interactive UHF rotator console. `embedded` for the PLC slots (the node names itself). The antenna switch omits `host`. |
| `capabilities` | object | Free-form, string-keyed. Per-slot contents are normative in §3. Also the resolution target for `expose.options_ref` (§2.3). |
| `expose` | object | The consumer-neutral field surface, §2.3. Not always present. A slot without it renders only as a liveness dot. |

- **R2.4** A bridge MUST publish `/meta` on every (re)connect, byte-identical to its
  previous publication when nothing changed (consumers byte-compare to stay
  churn-free).
- **R2.5** Consumers derive the slot address **from the topic** (strip the
  trailing `/meta`), never from the payload.

### 2.3 The `expose` sub-schema (consumer-neutral field descriptors)

`expose` describes a slot's observable and controllable surface with **no
Home-Assistant vocabulary** — all rendering knowledge lives in the discovery consumer
(§3.13). Shape:

```json
"expose": {
  "device":  { "name": "...", "model": "...", "manufacturer": "...",
               "sw_version": "...", "area": "..." },
  "fields":  [ { "key": "freq_hz", "name": "Frequency", "type": "number",
                 "unit": "Hz", "class": "frequency", "state_class": "measurement",
                 "writable": true, "min": 1800000, "max": 54000000, "step": 1000,
                 "command": { "action": "frequency", "value_key": "freq_hz",
                              "value_type": "int" } } ],
  "actions": [ { "key": "retract", "name": "Retract",
                 "command": { "action": "retract" } } ]
}
```

**Field descriptor keys:**

| Key | Type | Meaning |
|---|---|---|
| `key` | string, required | The JSON key in this slot's `/state` that the field reads. Becomes the consumer entity id (lowercased. Characters outside `[a-z0-9_-]` → `_`). |
| `name` | string | Display name. |
| `type` | `"number" \| "enum" \| "boolean" \| "string"` | Value kind. Consumers skip unknown types. |
| `unit` | string | Unit string (`"Hz"`, `"%"`, `"W"`, `"°C"`). Consumers map it to a device class (`Hz`→frequency, `°C`/`degC`→temperature, `W`/`Watts`→power, `V`/`Volts`→voltage, `A`/`Amps`→current, `dBm`→signal_strength). |
| `class` | string | Explicit semantic hint. **Wins over** the unit-derived class. |
| `state_class` | `"measurement" \| "total" \| "total_increasing"` | Long-term-statistics class. Consumers pass it through verbatim. |
| `options` | [string] | Inline enum option list. |
| `options_ref` | string | Key into this slot's `capabilities` map holding the option list (resolved at render. The consumer stringifies non-string elements). |
| `writable` | bool | True ⇒ the field is a setpoint (a command target), not just a sensor. |
| `command` | object | Needed for writable fields. See below. |
| `on`, `off` | string | For `boolean` fields whose state value is actually a string (for example `tx`: `"tx"`/`"rx"`): the two payload strings. Absent ⇒ the state holds a real JSON bool. |
| `min`, `max`, `step` | number | For writable `number` setpoints. |

**Command descriptor** (structured — never a consumer template string):

| Key | Meaning |
|---|---|
| `action` | Not always present. The `action` word carried in the `/cmd` JSON. |
| `value_key` | The JSON key the user-supplied value rides under (usually `"value"`). |
| `value_type` | `"string" \| "int" \| "float" \| "bool" \| "boolean" \| "enum"` — this tells the command builder how it must copy the user value onto the wire. Note: the deployed slots spell the boolean type two ways (`bool` on the tuner, `boolean` on the PLC slots) — see §7.3. |

Exactly three shapes exist, and any command builder MUST honor them:

| Shape | Built payload |
|---|---|
| `action` + `value_key` | `{"action":"<action>","<value_key>":<value>}`. |
| `value_key` only | `{"<value_key>":<value>}` (no `action` key). |
| `action` only | `{"action":"<action>"}` (button. **no** value key). |

- **R2.6** Actions with an empty `value_key` are pure buttons. A builder MUST send no
  `value` key at all (see R1.9 — historically a `null` arrived as `""` and then bridges
  dropped it).
- **R2.7** A bridge MUST list the fields in `fields[]` in a stable, deterministic
  order (consumers byte-compare rendered sets for idempotency).

---

## 3. The slot reference

One sub-section per slot. Every table is normative: field spellings, types, units,
presence rules, exact publish triggers, exact heartbeat values, exact error strings. A
re-implementation MUST reproduce them. Where a value is a deploy-time default, the
table states it as "default" with its value.

Slot index (15 addresses):

| # | Address | Role | Kind |
|---|---|---|---|
| 3.1 | `muehle/power/master` | `power` | bridge (Shelly plug). |
| 3.2 | `muehle/power/psu-13v8` | `power` | bridge (Shelly plug). |
| 3.3 | `muehle/hf/radio` | `radio` | bridge (FLEX-8400). |
| 3.4 | `muehle/hf/ant-ctrl` | `ant-ctrl` | bridge (Ultrabeam RCU-06). |
| 3.5 | `muehle/hf/ant-switch` | `ant-switch` | embedded node (ESPHome relay firmware). |
| 3.6 | `muehle/hf/switch` | `switch` | bridge (PLC relays 3, 4). |
| 3.7 | `muehle/hf/pa-arm` | `pa-arm` | bridge (PLC relay 1). |
| 3.8 | `muehle/hf/antenna-select` | `reconciler` | logic slot. |
| 3.9 | `muehle/hf/pa` | `pa` | bridge (ACOM 1200S). |
| 3.10 | `muehle/hf/rotator` | `rotator` | bridge (Yaesu G-450DC through WRC). |
| 3.11 | `muehle/hf/tuner` | `tuner` | bridge (ATR-1000). |
| 3.12 | `muehle/hf/power-seq` | `sequencer` | logic slot. |
| 3.13 | `muehle/hf/discovery` | `discovery` | logic slot (passive). |
| 3.14 | `muehle/uhf/pol-ctrl` | — | **gap: no component exists** (§3.14). |
| 3.15 | `muehle/uhf/rotator` | `rotator` | interactive console (pelcobridge2). |

Non-slot bus participants: the operator console (§3.16), the traffic capture tool and
bus test bench (§3.16), the web console bridge (§3.16).

### 3.1 `muehle/power/master` — station master mains smart plug

A switched mains outlet ("Shelly" device, WiFi) gating **all** station power. One
service process serves both this slot and `power/psu-13v8` (§3.2): **two slots, one
process, two MQTT connections** — each slot has its own connection, LWT, and client ID
(`muehle-power-master`, `muehle-power-psu-13v8`). A process crash flips **both**
`/status` planes offline simultaneously. Consumers CAN treat simultaneous offline of
the two power slots as a supply-layer process death.

**Topics:** `muehle/power/master/{meta,state,status,cmd}` — all retained QoS 1.

**`/state` payload:**

| Field | Type | Unit | Presence | Semantics | Publish trigger |
|---|---|---|---|---|---|
| `ts` | string | RFC 3339 UTC | always | snapshot time | every publish. |
| `power` | string (`"on"`/`"off"`) | — | always | Relay readback from the plug's native announcements. Command effects surface only through readback. Known deployed defect: a heartbeat `true` also optimistically sets `power:"on"` (§7.3) | on change. |
| `device_online` | bool | — | always | plug reachable (heartbeat + online flag) | on change. |
| `error` | string | — | omitted when empty | `"shelly heartbeat lost"` (no heartbeat message for 75 s) or `"shelly online=false"` (plug published online=false) | on change. |

**Liveness mechanics (normative):** the bridge MUST poll the plug's heartbeat ticker
every 10 s. A heartbeat older than **75 s** (or never received — treated as 1 h elapsed
on first check) MUST set `device_online: false` with `error: "shelly heartbeat
lost"`. A heartbeat message carrying `online=false` MUST set `device_online: false`
with `error: "shelly online=false"`.

**`/meta`:** `schema:"1.0"`, `role:"power"`, `device:{model,serial,firmware}` (Shelly
identity), `link:"wifi"`, `location:"bauwagen"`, `host:"shari"`,
`capabilities:{"fail_safe":"off"}` — fail-safe `off` means: on mains restoration after
a power cut the plug's relay is **open** (station stays dark) until commanded on. The
`psu-13v8` slot also carries `capabilities.feeds` (§3.2). `master` has none.
`expose.fields` contains one writable enum field: `power` (options `on`/`off`),
command `{action:"set_power", value_key:"value", value_type:"string"}`.

**`/cmd`:** exactly one action:

| Action | Payload | Retained | Meaning / resulting behavior |
|---|---|---|---|
| `set_power` | `{"action":"set_power","value":"on"\|"off"}` | **yes** (R1.13) | Commands the plug's relay. The bridge is fire-and-observe: it publishes the RPC and waits for the readback. `/state.power` changes only when the plug confirms. The bridge ignores an invalid `value`. |

- **R3.1.1** The retained `set_power` is the self-heal mechanism: after the plug (or
  the bridge) restarts, the device re-applies the retained command, so the last
  steady-state intent converges.
- **R3.1.2** No other component MUST publish to this `/cmd` topic. The allowed senders
  are the sequencer (§3.12) and the operator tools (console, testui). The reconciler
  is not a sender.

### 3.2 `muehle/power/psu-13v8` — 13.8 V DC power-supply plug

Same physical process, protocol, payload grammar, and command surface as §3.1 (all
rules R3.1.x apply verbatim, with the slot address `muehle/power/psu-13v8`). It gates
the 13.8 V DC supply feeding the **control electronics of the whole HF chain**.

Differences from §3.1:

- **`capabilities`** also carries `feeds` — the array of slots this supply
  powers: `["hf/radio","uhf/radio","hf/tuner","hf/ant-ctrl","hf/ant-switch","hf/rotator","hf/switch","hf/pa-arm"]`
  (site-relative names). The bridge omits it entirely when empty. `master` has none.
- **R3.2.1 (consumer rule)** — the operator console renders a root-cause line
  `PSU OFF — HF control chain unpowered` when this slot is bridge-online, with
  confirmed `power:"off"`, and any `muehle/hf/` slot appears offline. A switched-off
  PSU silently kills the whole HF control chain. Confirmed-off only — a missing
  `power` key MUST NOT trigger the line (never invent OFF from absent data).

### 3.3 `muehle/hf/radio` — FLEX-8400 transceiver (flexbridge)

The HF transceiver, reached over Ethernet through the vendor's TCP API (SmartSDR). The
bridge discovers the radio on the LAN, opens a TCP connection, and completes a
handshake before trusting any radio data.

**Topics:** `muehle/hf/radio/{meta,state,status,cmd}` — `meta`/`state`/`status`
retained. `/cmd` published by senders **not retained** (R1.14).

**`/state` payload:**

| Field | Type | Unit | Presence | Semantics | Publish trigger |
|---|---|---|---|---|---|
| `ts` | string | RFC 3339 UTC | always | snapshot time | every publish. |
| `freq_hz` | int64 | Hz | always | Operating frequency of the selected active slice. **0 = unknown** | on change. |
| `band` | string | — | omitted when empty | Canonical band derived from `freq_hz` per §4.1, with 2000 Hz edge hysteresis per slice (guards `gen`-band exits only). Empty during link reconnect. | on change. |
| `mode` | string | — | omitted when unknown | Canonical mode per §4.2 after firmware normalization. | on change. |
| `tx` | string (`"tx"`/`"rx"`) | — | always | Transmit state of the selected slice | on change. |
| `tuning` | bool | — | always | Logical OR of the radio's internal ATU-tune flag and any active tuner cycle reported to the bridge | on change. |
| `drive` | int | % (0–100) | always | Transmit drive (power) setting | on change. |
| `device_online` | bool | — | **always present** | Radio TCP link + handshake done. **R3.3.1:** on link loss the bridge MUST republish a zeroed snapshot with `device_online:false`, `freq_hz:0`, empty band/mode, `tx:"rx"`, `drive:0`. Never leave the last healthy values standing as retained truth. | on change. |
| `dvk_status` | string | — | omitted when empty | Digital voice keyer status: `idle` \| `recording` \| `preview` \| `playback` \| `disabled` | on change. |
| `dvk_id` | int | 1–12 | omitted when 0 | DVK memory slot now playing | on change. |
| `mic_profile` | string | — | omitted when empty | Last-loaded mic profile name (a mic profile is a named, saved microphone-audio setting. The radio reports no active-mic name — the bridge tracks the last loaded profile) | on change. |
| `mic_profiles` | [string] | — | omitted when empty | Sorted list of available mic profile names (queried once per radio connection through a one-shot command). The caret-delimited reply from the vendor protocol is only the input that the bridge parses — it never appears on the bus. The bus form is a JSON array of strings, full-snapshot replacement. | on change. |
| `rx_input` | string | — | omitted when empty | Active receive antenna input (`ant1`/`ant2`/`rx_a`) | on change. |

**Slice selection (normative):** the bridge MUST report the **lowest-index active/TX
slice** deterministically.

**`/meta`:** `role:"radio"`, `device:{model:"FLEX-8400",serial,firmware}`,
`link:"ethernet"`, `location:"bauwagen"`, `host:"shari"`, and
`capabilities:{bands:["160m","80m","60m","40m","30m","20m","17m","15m","12m","10m","6m"], modes:[...canonical modes...], receivers:1, diversity:false, amp_key:true, tune:true, bias_t:false, rx_inputs:["ant1","ant2","rx_a"], tx_outputs:["ant1","ant2"]}`.
The `expose` block declares read-only fields `device_online`, `freq_hz`, `band`,
`mode`, `drive`, `tx` (boolean with `on:"tx"` / `off:"rx"`), `tuning`, `dvk_status`,
`dvk_id`, `mic_profiles`. Writable fields `band` (command `set_band`) and
`mic_profile` (command `set_mic_profile`). And 13 actions `dvk_play_1` …
`dvk_play_12`, `dvk_stop`.

`amp_key` in `capabilities` is the radio's amplifier-keying output line — the wire
that switches an external PA into transmit on command of the radio.

**`/cmd` actions:**

| Action | Payload | Retained | Meaning / resulting behavior |
|---|---|---|---|
| `set_band` | `{"action":"set_band","value":"<band>"}` | no | Band label mapped internally to the radio's native band-stacking register number, in meters (20m→20, 6m→6, 160m→160). A band-stacking register is the radio's per-band saved frequency/mode memory. The bridge sends it as a band-stacking selection to a panadapter (the radio's on-screen spectrum-display session. SmartSDR command `display pan s <pan-handle> band=<n>`). It is not a frequency and not MHz. The bridge rejects any other label (logged, no bus effect). It needs a tracked panadapter. The bridge arms a **750 ms band-transition hold** and absorbs conflicting changes during it. The radio then retunes itself (it restores its persisted per-band frequency). The new `freq_hz`/`band` appear in `/state` as normal on-change telemetry. |
| `dvk_play` | `{"action":"dvk_play","value":"1".."12"}` | no | Play the given DVK memory slot. |
| `dvk_play_<N>` | `{"action":"dvk_play_3"}` (no value) | no | The same command in per-slot form. N validated 1–12. |
| `dvk_stop` | `{"action":"dvk_stop","value":"<id>"}` — value not necessary | no | With a value: stop that memory id (validated 1–12). Without a value (or with an empty string): stop the active memory, resolved from the live `/state` id. If no memory is active, the bridge warns and drops the command. There is no stop-all behavior. |
| `set_mic_profile` | `{"action":"set_mic_profile","value":"<name>"}` | no | Load a mic profile. Validation: non-empty, ≤ 64 chars, no `"`, no `\`, no control characters. If the tracked profile list is non-empty and the name is not in it, the bridge drops the command as a likely typo (an empty list does not block). The bridge rejects invalid names. It updates `mic_profile` **optimistically** (before radio confirmation). |

**Not commands:** `set_freq_hz`, `set_mode`, `set_drive` do not exist. Senders MUST
NOT publish them. (Frequency, mode, and drive change at the radio's own front panel or
at a remote client. The bridge only observes.)

**Publish cadence: on change only.** The deployed bridge republishes `/state` only when
a field value changes. **R3.3.2 (hard requirement derived from a live incident):** a
re-implementation MUST republish `/state` periodically — at least every
**5 s** while the radio link is up — or reach the same freshness another way.
*Rationale:* the PA-arm PLC holds `armed` only while a `radio/state` message arrives
at least every 10 s (§3.7). With change-only publishing, a healthy but idle radio
publishes nothing and silently disarms the station. This starvation is a **known live
fragility of the deployed system**. The periodic heartbeat is a mandatory fix for the
re-implementation, not a nice-to-have.

**Reference-implementation notes:** radio reconnect backoff 2 s × 1.5 (factor) capped
60 s, never reset on success. MQTT retry 5 s. Handshake deadlines 5 s. Device
discovery 3 s / announcement wait 5 s. Command queue depth 32. On clean shutdown the
bridge disconnects within ~500 ms and its LWT does **not** fire (retained `online`
persists — §5.3).

### 3.4 `muehle/hf/ant-ctrl` — Ultrabeam RCU-06 antenna controller (ultrabridge)

Tunes the rotatable Ultrabeam beam's motorized elements for the operating frequency
and drives its pattern direction and retraction. The device is on USB-serial
(RCU-06 binary protocol: STX 0xF5 / DLE 0xF6 / ETX 0xFA framing, checksummed frames,
7-bit sequence numbers, 19200 baud 8N1).

**Topics:** `muehle/hf/ant-ctrl/{meta,state,status,cmd}`. **All four retained** —
including `/cmd` (this slot is the one deliberate full-retention exception. See below).

**`/state` payload:**

| Field | Type | Unit | Presence | Semantics | Publish trigger |
|---|---|---|---|---|---|
| `ts` | string | RFC 3339 UTC | always | snapshot time | every publish. |
| `freq_hz` | int | Hz | always | Frequency the elements are now tuned for (`device kHz × 1000`) | on change (device polled every **2 s**). |
| `band` | string | — | always | Band label derived from the device's kHz table. Outside the table: `band-<device band index>` (device-native label, for example `band-7`) | on change. |
| `direction` | string | — | always | `forward` \| `reverse` \| `bidirectional` — device direction byte 0x00/0x01/0x02. The empty string `""` appears only before the first successful refresh (or while offline), never a pattern word | on change. |
| `moving` | bool | — | always | Elements now motorized (in motion) | on change. |
| `device_online` | bool | — | always | Serial link healthy | on change. |
| `error` | string | — | omitted when empty | Serial exchange failure text (for example I/O error) | on change. |

**Polling (normative):** every **2 s** tick the bridge MUST send a status query and
then a moving-status query (each with a 2 s timeout per exchange). The moving-status
query runs every cycle regardless of the `moving` flag. During a fully dead device
link, each query can use up its full 2 s timeout. The effective tick then stretches
to 4–8 s — the 2 s ticker drops missed beats instead of queueing them. Before the
first successful refresh (or while offline) the bridge holds the zero-value state:
`freq_hz: 0`, `band: ""`, `direction: ""` — never `band-0` or `unknown`. On a serial
I/O error the bridge MUST reopen the device **by stable
device path** (by-id) and retry the exchange exactly once. This is the USB
re-enumeration self-heal. If the retry also fails, the bridge reports the error in
`error`.

**`/meta`:** `role:"ant-ctrl"`, `device:{model:"Ultrabeam RCU-06",serial,firmware}`,
`link:"serial"`, `capabilities:{bands:["20m","17m","15m","12m","10m","6m"],
directions:["forward","reverse","bidirectional"]}`. `expose`: `freq_hz` writable —
`command:{action:"frequency", value_key:"freq_hz", value_type:"int"}`, min 1800000, max
54000000, step 1000. `band` writable (`value_key:"value"`). `direction` writable.
`moving`, `device_online`, `error` read-only. One action `retract`.

**`/cmd` actions:**

| Action | Payload | Retained | Meaning / resulting behavior |
|---|---|---|---|
| `frequency` | `{"action":"frequency","freq_hz":<int>}` | yes | Tune elements to the given frequency. The bridge accepts it only if it differs from the current tuned frequency by more than the **25 kHz deadband** (pure direction changes and an unknown current frequency bypass the deadband). It takes effect through the device's change-frequency exchange. `moving` goes true, then `freq_hz`/`band` update when settled. |
| `band` | `{"action":"band","value":"<band>"}` | yes | Convenience form: tune to the band-center frequency (Hz): 20m→14,175,000. 17m→18,118,000. 15m→21,225,000. 12m→24,940,000. 10m→28,850,000. 6m→51,000,000. |
| `direction` | `{"action":"direction","value":"forward"\|"reverse"\|"bidirectional"}` | yes | Set the radiation pattern. The cmd path also accepts the legacy input aliases `""`/`normal` (forward), `180` (reverse), and `bidir` (bidirectional). `/state` publishes only the three canonical words. (The legacy word `mode` stays a deprecated alias for `direction` — bridges MUST still accept it, but they do not advertise it.) |
| `retract` | `{"action":"retract"}` (no value) | **cleared after execution** | Fully retract the elements (emergency/safe state). This is a one-shot: after execution the bridge MUST clear the retained `/cmd` topic with an empty retained publish (R1.15) so restarts never re-retract. |

- **R3.4.1 (retained-cmd self-heal):** on every (re)connect the bridge MUST re-apply
  the retained `/cmd` — except a cleared one. The bridge never re-applies retract. This is the
  mechanism by which the beam survives bridge restarts without the commander
  re-sending intent.
- **R3.4.2** The console publishes `direction` commands to this topic retained. The
  reconciler publishes `frequency` retained (§3.8). All senders follow the same
  payload grammar above.

### 3.5 `muehle/hf/ant-switch` — 1:6 relay antenna switch (embedded node)

An ESPHome firmware image on a relay board. This is an **embedded node**: the firmware
*is* the bridge. There is no separate service process, and the node's own WiFi/MQTT
connection **is** the liveness layer. Consequently:

- **R3.5.1** This slot's `/status` reflects the **node itself**. There is no separate
  device behind it, so its `/state` carries **no `device_online` field** (consumers
  apply the absence-=-true rule, R2.2/§5.2).
- **R3.5.2** Its `/state.ts` comes from its Home-Assistant instance's clock, which is
  wrong when that instance is down. This is a documented defect. We recommend that a
  re-implementation give the node an independent time source (§7.3).

**Topics:** `muehle/hf/ant-switch/{meta,state,status,cmd}` — all retained QoS 1.
The client ID is `ant-switch`. The node uses a persistent MQTT session and re-subscribes on
reconnect so the retained `/cmd` replays into it (self-heal).

**`/state` payload:**

| Field | Type | Presence | Semantics | Publish trigger |
|---|---|---|---|---|
| `ts` | string | always | Snapshot time (see R3.5.2 caveat) | every publish. |
| `selected` | string (`"off"` \| `"port1"`..`"port6"`) | always | **Actual relay readback, never a command echo.** `off` = all relays open and all antenna feeds shorted to ground. | on change. |
| `settled` | bool | always | False from any commanded change until **200 ms after the latest change** (timer-restart semantics — a subsequent change within the window re-arms it). Conservative: never optimistic. | published `false` at T+0 with the new `selected`, then `true` at T+200 ms. |

So every change produces exactly **two** publishes: `(selected:<new>, settled:false)`
then `settled:true` 200 ms later.

**`/meta`:** `role:"ant-switch"`, `device:{model:<board model>}`, `link:"wifi"`
(the node runs embedded firmware — no separate bridge process exists — but its own
WiFi/MQTT connection is the transport, so `wifi` is the value on the wire). The node
omits `host`. `capabilities:{ports:[1,2,3,4,5,6], off:true, exclusive:true, hot_switch:false}` —
`exclusive` means at most one port selected at a time. `hot_switch:false` means the
relays are not rated for switching with RF power applied (drives the cross-slot
invariants in §6). `expose`: `selected` writable with **no `action`** —
`command:{value_key:"select", value_type:"string"}`. `settled` read-only.

**`/cmd`:** exactly one shape:

| Action | Payload | Retained | Meaning / resulting behavior |
|---|---|---|---|
| (none) | `{"select":"off"\|"port1"..\|"port6"}` | **yes** — the commander (the reconciler, §3.8) publishes it retained | Move the relays to the given position. **Exclusive move is break-before-make:** all relays open, then the selected one closes. The switch **silently ignores** an invalid value (no publish, no error). Idempotent: re-selecting the current port produces no relay chatter and no `settled` flap. |

- **R3.5.3 (power-loss fail-safe):** on power loss all relays open (`off`/grounded is
  the hardware default). On the node's reconnect the retained `/cmd` replays and the
  switch re-converges to the last commanded intent.
- **R3.5.4 (station wiring):** the six positions are a wiring-map matter, not this
  slot's: `off` = grounded, port 1 = dummy load. The Ultrabeam is port 3 **or**
  port 4 and the fan-dipole is port 6 **or** port 2 — both are open decisions
  (§7.1). No source documents anything wired to port 5. The operator console
  nevertheless renders `port5` as a selectable position (labeled "Port 5") and
  publishes `{"select":"port5"}` to the switch when an operator picks it. The
  switch firmware accepts all six positions either way.

### 3.6 `muehle/hf/switch` — PA/TRX remote-on relays (PLC #1, relays 3 and 4)

One M5 Stamp PLC (a small WiFi relay/PLC board) serves **two slots**: this one (relays
3 and 4) and `hf/pa-arm` (relay 1). See §3.7. The two slots run two independent MQTT
connections from the same firmware — one LWT each, and both fire on a crash. Both
slots publish **the same** `device{model:"M5Stamp PLC", serial:"m5stamp-plc-1"}` in
their `/meta` — that equality is the compound-device tie (R2.2).

**Topics:** `muehle/hf/switch/{meta,state,status,cmd}` — `meta`/`state`/`status`
retained. `/cmd` retained (R1.13).

**`/state` payload:**

| Field | Type | Presence | Semantics | Publish trigger |
|---|---|---|---|---|
| `ts` | **number** | always | **Process uptime in milliseconds** — not a wall clock (R1.17 deviation, known to consumers) | every publish. |
| `pa` | string (`"on"`/`"off"`) | always | PA remote-on relay (relay 3) **readback** | on change. |
| `trx` | string (`"on"`/`"off"`) | always | Transceiver remote-on relay (relay 4) **readback** | on change. |
| `device_online` | bool | always | Hardcoded `true` while the firmware runs (the device is the node itself) | always. |

There is **no periodic heartbeat** on this slot's `/state` (contrast §3.7) — the bus
can detect a wedged but connected PLC only through its LWT.

**`/meta`:** `role:"switch"` (deployed value — the integration model calls the role
`switch`, never `relay`), `link:"wifi"`, `host:"embedded"` (the node names itself),
`capabilities:{channels:["pa","trx"], exclusive:false, kind:{pa:"remote_on", trx:"remote_on"}, relay_map:{pa:3, trx:4}}`.

**`/cmd` actions:**

| Action | Payload | Retained | Meaning / resulting behavior |
|---|---|---|---|
| `set_pa` | `{"action":"set_pa","value":"on"\|"off"}` | yes | Energize/de-energize relay 3 (PA remote-on). |
| `set_trx` | `{"action":"set_trx","value":"on"\|"off"}` | yes | Energize/de-energize relay 4 (TRX remote-on). |

- **R3.6.1** The PLC also has **physical buttons** (B = PA, C = TRX, 150 ms debounce)
  that toggle the relays locally and publish the corresponding retained `/cmd`. So
  local and remote control converge on the same topic. Buttons work while offline.
  On reconnect the retained `/cmd` replay **reverts** any local change that contradicts
  the last retained intent (documented, accepted behavior).
- **R3.6.2** On power loss the relays **open** (de-energized = safe). Retained `/cmd`
  replay restores the last intent on reconnect.

### 3.7 `muehle/hf/pa-arm` — PA arm relay (PLC #1, relay 1)

The PA arm interlock: a physical relay in the PA's keying line. `enabled` is the
permission that the operator commanded. `armed` is a **derived** value — the PLC
continuously computes whether amplification is now safe in the light of the radio's
and switch's published state. **`armed` is never commanded. Only `enabled` is.**

**Topics:** `muehle/hf/pa-arm/{meta,state,status,cmd}` — `meta`/`state`/`status`
retained. `/cmd` retained.

**`/state` payload:**

| Field | Type | Presence | Semantics | Publish trigger |
|---|---|---|---|---|
| `ts` | **number** | always | Process uptime in milliseconds (R1.17 deviation) | every publish. |
| `enabled` | bool | always | Last `set_enabled` permission (relay drive permission) | on change. |
| `armed` | bool | always | **Derived**, see formula below | on change (recomputed every 50 ms). |
| `device_online` | bool | always | Hardcoded `true` while firmware runs | always. |
| `error` | string | omitted when empty | Reason `armed` is withheld (see precedence list below). Emitted independent of `enabled` | on change. |

**Arm formula (normative — the station's central RF-safety interlock):**

```
armed = enabled
      AND radio_online        (radio /state.device_online == true; an absent key counts as false — fail-closed, R5.2a)
      AND NOT radio_tuning    (radio /state.tuning != true)
      AND band_safe           (radio /state.band ∈ SAFE_BANDS)
      AND heartbeat_fresh     (a parseable radio /state message arrived within the last 10 s)
      AND antenna_ready       (ant-switch /state.selected is a non-empty value ≠ "off")
```

- `SAFE_BANDS` = `160m, 80m, 60m, 40m, 30m, 20m, 17m, 15m, 12m, 10m, 6m`.
- Recompute loop: every **50 ms**. (Deployed defect: the firmware runs the recompute
  loop only while WiFi is up — during a WiFi outage `armed` freezes rather than
  dropping. A re-implementation MUST drop to `armed:false` on any liveness
  uncertainty, §7.3.)
- **Error-string precedence (exact strings):** `"radio offline"` → `"radio tuning"` →
  `"band not safe"` → `"antenna grounded"` — first matching cause wins. **A heartbeat
  timeout produces NO error string** (armed simply drops silently — the deployed
  behavior consumers must know).
- Any parseable `radio/state` message — regardless of content — refreshes the
  heartbeat clock. The `ant-switch/state.selected` input has **no staleness window**.
- The PLC subscribes to `radio/state` only, never to `radio/status` (the component
  research confirms this). A dead radio bridge therefore stays invisible to the
  `radio_online` input directly. But a dead bridge also stops publishing `/state`, so
  the 10 s heartbeat backstop drops `armed` within one loop pass anyway. For this
  consumer, freshness gives the R5.1 two-layer AND indirectly — not a `/status`
  subscription.

**`/meta`:** `role:"pa-arm"`, `link:"wifi"`,
`capabilities:{fail_safe:"open", heartbeat:true, relay:1}` — fail-safe `open`:
de-energized = PA keying line blocked. Any of the enabling inputs dropping
de-energizes the relay within the 50 ms loop.

**Heartbeat (output):** the PLC MUST republish `/state` at least every **10,000 ms**
(10 s) even when nothing changed (dedup-suppressed if the same content was just
published).

**`/cmd` actions:**

| Action | Payload | Retained | Meaning / resulting behavior |
|---|---|---|---|
| `set_enabled` | `{"action":"set_enabled","value":"true"\|"false"}` | yes | Set the permission. **The value MUST be the string `"true"`/`"false"`** — a JSON boolean `true` DISARMS (the PLC string-matches. Anything that is not the exact string `"true"` clears the permission). |

There is deliberately **no** `arm` or `force` command: arming is only ever the derived
formula, never an operator override.

### 3.8 `muehle/hf/antenna-select` — antenna-selection reconciler (logic slot)

A stateless policy engine that watches the radio and the switch and decides which
antenna the switch has selected right now. It drives the switch and three "follow"
bindings.
The beam controller tracks frequency, the PA is pre-positioned to the band, and the
tuner enters or leaves the RF path. No device, no I/O. It runs on shari.

**Topics:** `muehle/hf/antenna-select/{meta,state,status,cmd}`. Own planes retained
QoS 1. `/cmd` retained (operator holds survive a reconciler restart through retained
replay).

**Subscriptions (normative):** `muehle/hf/radio/state`, `muehle/hf/radio/status`,
`muehle/hf/ant-switch/state`, own `/cmd` — all QoS 1, re-armed on every (re)connect.
The reconciler MUST use a **clean MQTT session** (R5.7) so retained inputs replay on
every reconnect and re-seed its stateless state.

**`/state` payload** — published **only when the resolved decision changes** (deduped,
with no periodic republish):

| Field | Type | Presence | Semantics |
|---|---|---|---|
| `ts` | string | always | Decision time, RFC 3339 UTC. |
| `mode` | string | always | `"auto"` \| `"manual"`. Derived: `manual` iff an operator hold is active. There is no separate mode switch. |
| `target` | string | always | The switch position the reconciler now wants: `"off"` or a wiring-map port key (`port1`..`port6`). This is **intent**. The actual position lives in `ant-switch/state.selected`. |
| `source` | string | always | `"idle"` \| `"operator"` \| `"auto"` — why. |

This slot publishes **no `device_online`** (logic slot).

**`/meta`** (deployed shape): `role:"reconciler"`, `link:"none"`,
`location:"bauwagen"`, `host:"shari"`,
`capabilities:{controls:"ant-switch", follows:{ultrabeam:"ant-ctrl", pa:"pa", tuner:"tuner"}, ladder:["idle","operator","auto"]}` (the
whole `follows` key omitted when config disables all three bindings), and
`expose.fields`: `source` (enum, options_ref `ladder`), `target` (string), `mode`
(string) — all read-only.

**`/cmd`:** one shape, no `action` key:

| Payload | Effect |
|---|---|
| `{"request":"port1".."port6"\|"off"}` | Engage an **operator hold** on that switch position (ladder tier 2). `mode` becomes `manual`. |
| `{"request":"auto"}` | Release the hold. Return to band-policy selection. |
| Empty/absent payload (cleared retained) | The engine treats it as a release. |
| Any other string | **Still engages a hold** with that literal string as target (unvalidated — deployed behavior. We recommend validation in the re-implementation, §7.3). |

**The decision ladder** (evaluated on every input change. Every decision is a pure
function of config + latest inputs):

```
Tier 1  idle:     station inactive            → target "off",  source "idle"
Tier 2  operator: operator hold active         → target request, source "operator"
Tier 3  auto:     band policy (radio.band)    → target port,  source "auto"
```

- **R3.8.1** Tier 1 **wins over everything, including an operator hold** (walk-away
  safety). The engine treats unknown activity as active.
- **R3.8.2** Tier 3 resolves only if the radio passes the two-layer gate
  (radio `/status == online` AND `radio/state.device_online == true`, §5) **and**
  `band` is non-empty. Band resolution: scan `band_policy.bands` in **sorted resource
  order** for the band. The resource maps to a port through the inverted wiring map.
- **R3.8.3** A known-but-unmatched band (for example `160m`, or the out-of-band `gen`)
  resolves to the fallback resource (deployed: `fan-dipole` → `port6`).
- **R3.8.4** An **empty band holds the last selection** (empty target, no command
  emitted) — it is the radio's transient reconnect state, not a tuning intent.
  Radio offline likewise holds the last selection — but a tier-2 operator hold still
  asserts while the radio is offline.

**Deployed wiring map and band policy** (subject to the open port decisions, §7.1):

| Wiring key | Resource | Physical antenna |
|---|---|---|
| `port1` | `dummy-load` | dummy load. |
| `port3` **or** `port4` | `ultrabeam` | Ultrabeam rotatable beam — **open decision, §7.1**. |
| `port6` **or** `port2` | `fan-dipole` | 80/40 m fan dipole — **open decision, §7.1**. |
| `off` | `grounded` | all feeds grounded. |

Band policy: `ultrabeam` serves `["6m","10m","12m","15m","17m","20m"]`.
`fan-dipole` serves `["30m","40m","60m","80m"]`. Fallback `fan-dipole` (160m, `gen`,
and any unmatched band land there — non-resonant, hence the tuner follow).

**Idle grounding (walk-away safety):**

- **R3.8.5** The reconciler infers activity, never sets it directly. A `radio/state`
  message whose `freq_hz` differs from the last seen one, **or** whose `tx == "tx"`,
  sets activity and resets the idle clock. An internal **5-second** ticker re-checks. When
  `now − lastActivity ≥ 30 minutes` (config default `idle.timeout_minutes = 30`),
  the reconciler MUST select `off` (grounded) — landing within ~5 s of expiry.
- **R3.8.6** Re-activation requires a **new** `radio/state` with a changed `freq_hz`
  or `tx=="tx"` — a replay of the same state does not count. An operator command
  does **not** count. A reconciler restart **does** (retained replay always looks
  like a freq change — deployed defect, §7.3).

**Emission rules (all normative):**

| Output | Topic / payload | Retained | Emitted iff |
|---|---|---|---|
| Own state | `<own>/state` `{ts,mode,target,source}` | yes | the decision triple changed. |
| Switch select | `ant-switch/cmd` `{"select":"<port>"}` | yes | target known ∧ target ≠ `ant-switch/state.selected` ∧ radio not transmitting. While the switch position is still unknown, at most one command per target. On later mismatch the reconciler **re-asserts the command on every input change** (it re-converges a manual override of the switch). |
| Band-follow | `ant-ctrl/cmd` `{"action":"frequency","freq_hz":<int>}` | yes | resolved target equals the followed resource's port ∧ radio two-layer online ∧ `freq_hz > 0`. Deduped against last frequency pushed. |
| PA follow | `pa/cmd` `{"action":"set_band","value":"<band>"}` | **no** | PA-follow enabled ∧ radio two-layer online ∧ band non-empty. **Not** gated on antenna selection, activity, or TX (the PA is always in the RF path. Hot-switch protection is hardware). Deduped. |
| Tuner follow | `tuner/cmd` `{"action":"set_inline","value":<bool>}` | **no** | tuner-follow enabled ∧ resource port configured ∧ radio two-layer online ∧ band non-empty. `true` iff resolved target == tuner's resource port ∧ band ∈ `atu_bands` (deployed: `["30m","60m","80m","160m"]`). Else `false`. Deduped. |

- **R3.8.7 (cold-switch deferral)** — if `radio/state.tx == "tx"` when the reconciler
  wants a port change, it **withholds** the select and logs once. It fires when a
  later input change finds the radio back in `rx`. No timeout (deployed. §7.3).
- **R3.8.8** The not-retained PA/tuner commands self-heal by the reconciler
  **re-resolving on the retained `radio/state` replay at its own reconnect** —
  not from retained commands.

### 3.9 `muehle/hf/pa` — ACOM 1200S power amplifier

The HF power amplifier, on RS-232 serial (9600 8N1, vendor binary protocol). Telemetry
is opt-in (the bridge sends an enable-telemetry frame at connect), then the PA streams
72-byte telemetry frames.

**Topics:** `muehle/hf/pa/{meta,state,status,cmd}` — `meta`/`state`/`status` retained.
`/cmd` **not retained**.

**`/state` payload:**

| Field | Type | Unit | Presence | Semantics |
|---|---|---|---|---|
| `ts` | string | RFC 3339 UTC | always | snapshot time. |
| `mode` | string | — | always | `"operate"` \| `"standby"`. Firmware states `OPR`/`OPR+RX`/`OPR+TX` → `operate`. Anything else → `standby`. The deployed protocol never produces the PA's hardware `bypass` state. |
| `band` | string | — | omitted when unknown | Auto-sensed band from the PA's RF sample: one of the 10 labels `160m..6m` **without 60m** (the PA has no 60m position). Or the raw `"UNK"` when the PA reports no band. |
| `keyed` | string | — | always | `"tx"` \| `"rx"` \| `"inhibited"` — the PA amplifies / idles / its own protection holds it off. |
| `fwd_power_w` | number | W | always | Forward power. **Hard-zeroed whenever `keyed != "tx"`.** |
| `rfl_power_w` | number | W | always | Reflected power. Hard-zeroed whenever `keyed != "tx"`. |
| `temp_c` | number | °C | always | PA temperature, rounded to 0.1 °C (firmware sends Kelvin. Raw 0 encodes 0.0, not −273.15). |
| `swr` | number | ratio | always | Standing-wave ratio (raw/100). |
| `fault` | string | — | always | `"none"` \| `"swr"` \| `"temp"` \| `"reflected"` \| `"other"` — bucketized from the firmware fault byte. The component doc ([03-components/acom1200s-pa-bridge.md](03-components/acom1200s-pa-bridge.md)) preserves the **verbatim firmware message table**. |
| `pa_state` | string | — | always | Raw firmware status string (`"STBY"`, `"OPR/RX"`, `"OPR/TX"`, `"OFF"`, …) — diagnostics only. |
| `power` | string | — | always | `"on"` \| `"off"` — **read-only telemetry**: the PA's own powered state, `off` only when `pa_state == OFF` or the serial port goes down. There is deliberately **no `set_power` command** for this slot (the switch relays and the supply layer do all mains switching). |
| `device_online` | bool | — | always | Serial link + telemetry enabled. |
| `error` | string | — | omitted when empty | Verbatim firmware fault message or serial error text when the fault byte is not `0xFF`. When the fault byte is `0xFF` (no fault), the deployed bridge publishes an empty or omitted `error`. `NONE` is only the firmware message-table label for byte `0xFF`, never a value on the wire (R2.3). |

**Publish cadence (normative dedup):** publish every telemetry frame while
`keyed == "tx"` **or** any fault/error/offline condition is active. Otherwise publish
on change of `mode`/`band`/`keyed`/`fault`/`pa_state`/`power`/`device_online`/`error`.
Otherwise a heartbeat republish happens at least every **60 s**.

**`/meta`:** `role:"pa"`, `device:{model:"ACOM 1200S", serial:"acom-1200s",
firmware:<ver>}`, `link:"serial"`,
`capabilities:{bands:[10 bands, no 60m], max_power_w:1200, band_source:"rf_sense", rf_sample:false, key_input:"hardware", alc_out:true, modes:["operate","standby"]}`.
`alc_out` is the automatic-level-control feedback output from the amplifier — it
limits the radio's drive power to keep the PA in its safe range.

**`/cmd` actions** (both fire-and-observe. The PA confirms through telemetry):

| Action | Payload | Retained | Meaning / resulting behavior |
|---|---|---|---|
| `set_mode` | `{"action":"set_mode","value":"operate"\|"standby"}` | no | Switch the PA between operate and standby. |
| `set_band` | `{"action":"set_band","value":"<band>"}` | no | Pre-position the PA's auto-sense band selector. Accepts band labels or bare band numbers, case-insensitive. Rejected (logged, no effect) when the PA's current band index is 0/unknown. Band movement is by relative steps (next/prev) with ≥ 150 ms between steps. |

### 3.10 `muehle/hf/rotator` — HF beam rotator (Yaesu G-450DC through WRC)

The HF mast rotator, driven through an AF6SA "WRC" controller box exposing a WebSocket
JSON API (`ws://192.168.1.108/wsrotor`, 10 s handshake). The bridge also serves two
legacy control protocols locally (see below).

**Topics:** `muehle/hf/rotator/{meta,state,status,cmd}` — `meta`/`state`/`status`
retained. `/cmd` **never retained** (no reconciliation: motion is not a setpoint).

**`/state` payload:**

| Field | Type | Unit | Presence | Semantics |
|---|---|---|---|---|
| `ts` | string | RFC 3339 UTC | always | snapshot time. |
| `az` | number | degrees | always | Current azimuth (float). |
| `target_az` | number | degrees | omitted when the WRC reports none | Commanded target (the WRC's `tdeg`. 0 is indistinguishable from absent — deployed quirk). |
| `moving` | bool | — | always | Derived: the WRC's state string contains `rotat` or `moving`. |
| `rotor_state` | string | — | omitted when empty | Raw WRC state string. |
| `device_online` | bool | — | always | WebSocket link healthy. |
| `error` | string | — | omitted when empty | `"wrc: …"`-prefixed error text. |

Publish trigger: on change of the six-field content (deduped. No periodic republish).

**`/meta`:** `role:"rotator"`, `device:{model:"Yaesu G-450DC"}`, `link:"wifi"`
(WRC reachable over the LAN), `host:"shari"`, `capabilities:{axes:["az"]}`.
`expose`: `az` writable — `command:{action:"set_az", value_key:"az",
value_type:"float"}`, advertised min 0 / max 450 / step 1 (advertised only. **not
enforced** by the bridge). `target_az`, `moving`, `rotor_state`, `device_online`
read-only. Actions `stop`, `fwd`, `rev`.

**`/cmd` actions:**

| Action | Payload | Retained | Meaning / resulting behavior |
|---|---|---|---|
| `set_az` | `{"action":"set_az","az":<number>}` | no | Slew to the given azimuth. The WRC's own protocol requires the angle as a **quoted string**. The bridge performs the conversion. |
| `stop` | `{"action":"stop"}` | no | Halt motion. |
| `fwd` / `rev` | `{"action":"fwd"}` / `{"action":"rev"}` | no | Jog clockwise / counter-clockwise until stopped. |

- **R3.10.1** When the WRC is unreachable, the bridge **drops** commands (no queue, no
  retry) and publishes `device_online:false` with `error`.

**Side servers (reference-implementation notes. Wire behavior normative for clients):**
the bridge also serves a GS-232B-compatible TCP server on `0.0.0.0:7373` (`C`/`C2` →
`+0aaa+0000\r`, `M090`/`W180`/`000` → `\r`, `S` → `\r`, unknown → `?>\r`) and a
PSTRotator UDP responder on `0.0.0.0:12040` (`AZ?` → reply to sender port + 1
`<PST><AZIMUTH>aaa</AZIMUTH></PST>`. `<STOP>`. `<PARK>` no-op. `<AZIMUTH>n</AZIMUTH>`).
Reconnect backoff 2 s × 1.5 → 60 s, never reset.

### 3.11 `muehle/hf/tuner` — ATR-1000 antenna tuner

Impedance-matching unit in the feed line, on a binary WebSocket protocol
(`ws://192.168.1.20:60001`. Frames `FF <cmd> <len> <payload>`. Bridge sends Sync on
connect, then Meter/TuneStatus/TuneMode/Relay frames).

**Topics:** `muehle/hf/tuner/{meta,state,status,cmd}` — `meta`/`state`/`status`
retained. `/cmd` **not retained**.

**`/state` payload:**

| Field | Type | Unit | Presence | Semantics |
|---|---|---|---|---|
| `ts` | string | RFC 3339 UTC | always | snapshot time. |
| `inline` | bool | — | always | ATU in the RF path (`true`) vs bypassed. Client-side belief, set optimistically on command and confirmed/corrected by the device's TuneStatus frames. |
| `swr` | number | ratio | always | Raw/100 when raw ≥ 100, else raw as-is (deployed quirk of the firmware scale). |
| `fwd` | number | W | always | Forward power. |
| `l_uh` | number | µH | always | Tuner inductor setting. |
| `c_pf` | number | pF | always | Tuner capacitor setting. |
| `settling` | bool | — | always | A tune cycle is in progress (cleared by the device's Relay frame). |
| `fault` | string | — | omitted when empty | `"tune timeout"` when the tune timer expires. |
| `device_online` | bool | — | always | WebSocket link healthy. |
| `error` | string | — | omitted when empty | Protocol/serial error text. |

**Publish triggers (normative):** event-driven only — there is no periodic heartbeat
on `/state`. The bridge publishes on: (1) tuner link established, (2) each decoded
Meter frame, (3) each decoded Relay frame, (4) a command-acknowledgment state
change, (5) tune timeout, (6) tuner link lost. The link-lost snapshot publishes
`device_online:false` with `error` set, forces `settling` to `false`, and
**preserves the last measured SWR/fwd/l_uh/c_pf values** — they are stale, not
zeroed. A consumer MUST read them as last-known values, never as fresh ones.

**`/meta`:** `role:"tuner"`, `device:{model:"ATR-1000"}`, `link:"wifi"`,
`capabilities:{inline:true, tune_modes:["mem","full"]}`. `expose`: `swr`, `fwd`,
`l_uh`, `c_pf`, `settling`, `fault`, `device_online` read-only. `inline` writable —
`command:{action:"set_inline", value_key:"value", value_type:"bool"}` (the value is a
**JSON boolean** here). One action `tune` (`value_key:"value"`,
`value_type:"enum"`, options `mem`/`full`).

**`/cmd` actions:**

| Action | Payload | Retained | Meaning / resulting behavior |
|---|---|---|---|
| `set_inline` | `{"action":"set_inline","value":<bool>}` — real JSON bool | no | Put the ATU in the RF path (`true`) or bypass it (`false`). The bridge updates `inline` optimistically. |
| `tune` | `{"action":"tune","value":"mem"\|"full"}` — string value | no | Start a tune cycle: memory-based or full search. `settling:true` immediately. The device's Relay frame ends it. A **12-second** timer faults with `fault:"tune timeout"` if no completion arrives. A second `tune` command re-arms the timer. The bridge rejects an unknown mode. |

- **R3.11.1** The argument key is `value` only (never `inline`, never a key named for
  the action) — this convention comes from a live fix on this very bridge. A regression
  here re-breaks the station's tuner.

### 3.12 `muehle/hf/power-seq` — station startup/shutdown sequencer (powerseq, logic slot)

One-button bring-up/teardown of the whole station in a fixed, safe order, with explicit
liveness confirmation between steps. No device. Sequences are config-driven (TOML). The
behavior primitives are four generic step kinds. Runs on shari.

**Topics:** `muehle/hf/power-seq/{meta,state,status,cmd}` — `meta`/`state`/`status`
retained QoS 1. The sequencer subscribes to its own `/cmd` at **QoS 0**. Senders
MUST NOT publish it retained (R1.16). A command issued while the sequencer is down
disappears, by design, so a restart can never replay an operator `start`.

**`/state` payload** — published on every internal transition, on runner start, and
republished on every broker (re)connect:

| Field | Type | Presence | Semantics |
|---|---|---|---|
| `ts` | string | always | RFC 3339 UTC. |
| `phase` | string enum | always | `idle` \| `starting` \| `running` \| `stopping`. |
| `step` | string | **always present** (empty `""` at idle/running/after completion) | Name of the current step while a sequence runs — step names are user-visible and normative (below). |
| `fault` | string | omitted when empty | `"<step>: <reason>"`. Cleared by the next honored command and by a completed shutdown. |

This slot publishes **no `device_online`** (logic slot).

**`/meta`:** `role:"sequencer"`, `link:"none"`, `location:"bauwagen"`, `host:"shari"`,
`capabilities:{controls:[…sorted slot addresses any cmd step targets…], watches:[…sorted addresses of every slot any step references…]}`,
`expose.fields`: `phase` (enum `idle|starting|running|stopping`), `step` (string),
`fault` (string) — all read-only (the command surface is the one-shot `/cmd`).

**`/cmd`** — the only input. The sequencer examines only the `action` key, ignores
extra keys, and logs malformed JSON before it drops the command:

| Action | Payload | Honored iff | Effect |
|---|---|---|---|
| `start` | `{"action":"start"}` (no value) | `phase == "idle"` (running start again clears a prior `fault` — re-running over a hot station is safe. Every step is idempotent steady-state intent) | Runs the startup sequence (below). |
| `stop` | `{"action":"stop"}` (no value) | `phase == "running"` **or** `phase == "idle"` **with** a fault (teardown of a half-energized station) | Runs the shutdown sequence (below). |

The sequencer logs any other action, or any command while a sequence is in progress,
at warn and drops it. There is **no cancel command** (deliberate. §7.3). The busy
guard runs twice: a fast-path check at reception and an atomic re-check at execution
(the `idle→starting` / `running→stopping` transition happens under a lock).

**Default startup sequence (step names normative — they appear in `state.step` and
fault strings):**

| # | Step name | Kind | Detail |
|---|---|---|---|
| 1 | `master-on` | cmd | `muehle/power/master/cmd` `{"action":"set_power","value":"on"}` (retained). |
| 2 | `network-delay` | delay | symbolic `network` → **30 s** (default `timing.network_delay_s`). |
| 3 | `psu-on` | cmd | `muehle/power/psu-13v8/cmd` `{"action":"set_power","value":"on"}` (retained). |
| 4 | `wait-controllers-online` | wait_status | all of `muehle/hf/switch`, `muehle/hf/pa-arm`, `muehle/hf/ant-switch` `/status == "online"`. Deadline **120 s** (default `timing.step_timeout_s`). |
| 5 | `trx-on` | cmd | `muehle/hf/switch/cmd` `{"action":"set_trx","value":"on"}` (retained). |
| 6 | `wait-radio-online` | wait_status | `muehle/hf/radio/status == "online"`. 120 s. |
| 7 | `pa-on` | cmd | `muehle/hf/switch/cmd` `{"action":"set_pa","value":"on"}` (retained). |
| 8 | `wait-pa-power-on` | wait_state | `muehle/hf/pa/state` field `power == "on"` **and** `muehle/hf/pa/status == "online"` (implicit liveness precondition). 120 s. |
| 9 | `pa-arm-enable` | cmd | `muehle/hf/pa-arm/cmd` `{"action":"set_enabled","value":"true"}` (retained) — last step → `phase=running`. |

**Default shutdown sequence (exact reverse with inrush staggers):**

| # | Step name | Kind | Detail |
|---|---|---|---|
| 1 | `pa-arm-disable` | cmd | `pa-arm/cmd` `{"action":"set_enabled","value":"false"}` (retained). |
| 2 | `stagger-1` | delay | symbolic `stagger` → **2 s** (default `timing.shutdown_stagger_s`). |
| 3 | `pa-off` | cmd | `switch/cmd` `{"action":"set_pa","value":"off"}`. |
| 4 | `stagger-2` | delay | 2 s. |
| 5 | `trx-off` | cmd | `switch/cmd` `{"action":"set_trx","value":"off"}`. |
| 6 | `stagger-3` | delay | 2 s. |
| 7 | `psu-off` | cmd | `power/psu-13v8/cmd` `{"action":"set_power","value":"off"}`. |
| 8 | `stagger-4` | delay | 2 s. |
| 9 | `master-off` | cmd | `power/master/cmd` `{"action":"set_power","value":"off"}` — completes → `phase=idle`, fault cleared. |

The staggers exist to separate the electrical inrush currents of switching inductive
loads. Shutdown deliberately has **no waits** — it must make progress even if
devices are already dead.

**Step kinds (the only behavior primitives):**

1. **`cmd`** — publish `{"action":…,"value":"<string>"}` to `<slot>/cmd`, retained
   (unless the step sets `retain=false`). Requires broker connectivity at publish
   time. Failure faults the sequence. Each publish gets a **10 s** bound.
2. **`delay`** — sleep a literal `duration_s` (>0) or a symbolic `network` (30 s) /
   `stagger` (2 s). Not gated by the broker (purely local).
3. **`wait_status`** — until **every** slot in the list has `/status == state`
   (default `online`. `offline` requires an *actual* `offline` payload — **absence
   never counts as offline**). Poll every **200 ms** (default
   `timing.poll_interval_ms`). Deadline 120 s default, per-step override `timeout_s`
   (>0). The step can also carry a **hold/debounce** `hold_ms`: the condition must
   hold continuously. If the condition breaks mid-hold, the step restarts the window.
   Omitted `hold_ms` → default 0
   (`timing.default_hold_ms`). **Explicit `hold_ms = 0` is edge-triggered**.
4. **`wait_state`** — until the top-level JSON field `field` of `<slot>/state`
   string-compares equal to `value` (JSON booleans coerce to `"true"`/`"false"`,
   numbers to shortest decimal, `null`/absent to `""`. An empty `value` passes when
   the field is absent/null/empty). The wait also carries an implicit precondition:
   the slot's `/status` must be `online` now. A dead device cannot pass a wait
   on a stale retained snapshot.

**Fault taxonomy (exact `"<step>: <reason>"` reasons):** `timeout`, `broker
disconnected`, `publish failed: <error>`, `interrupted` (process shutdown during a
step). **No rollback**: on any fault the sequencer goes `phase=idle` with the fault
string and leaves already-driven slots at their last retained intent. Recovery is an
explicit re-run (`start`) or `stop` from idle+fault. A malformed `/state` payload on a
watched slot **deletes** that slot's cached snapshot (good→bad must not leave stale
truth). The configured sequences derive the subscriptions. The sequencer re-issues
them on every (re)connect.

### 3.13 `muehle/hf/discovery` — Home-Assistant discovery consumer (hadiscovery, logic slot)

A passive renderer: it reads every slot's `/meta`, translates the neutral `expose`
block into Home-Assistant MQTT-discovery messages, and keeps them current. **It never
publishes to any slot's `/cmd`** and never writes under any address except its own
`meta`/`status` and the configured HA discovery prefix (`homeassistant`).

**Topics:** `muehle/hf/discovery/{meta,status}` retained QoS 1 — **no `/state`, no
`/cmd`** (this slot has no telemetry and no command surface. The command surface of
HA-rendered entities belongs to the *bridges'* `/cmd` topics, which HA itself
publishes to).

**Subscriptions:** `muehle/+/+/meta` (config `meta_filter`. The consumer parses the
slot address from the topic. The topic must have exactly 4 segments and end in
`meta`) and `homeassistant/status` — QoS 1, re-armed on every (re)connect so retained
metas replay.

**`/meta` (exact published shape — deliberately **no `expose`**, so the discovery
consumer itself is undiscoverable):**

```json
{ "schema": "1.0", "role": "discovery", "link": "none",
  "location": "bauwagen", "host": "shari",
  "capabilities": { "renders": ["sensor","binary_sensor","number","select","button"],
                    "filter": "muehle/+/+/meta" } }
```

**Behavior contract (normative):**

- **R3.13.1** The consumer logs and skips a `/meta` with `schema != "1.0"`, an empty
  `role`, unparseable JSON, or a malformed topic — it never renders such a meta.
- **R3.13.2** A byte-identical re-delivery of a slot's `/meta` MUST produce **zero**
  publishes (idempotency by byte-comparison of the rendered set).
- **R3.13.3** When a slot's rendered entity set **shrinks** (changed meta), the
  consumer MUST publish empty retained payloads to the dropped topics so HA removes
  them. A zero-length retained `/meta` clears that slot's whole set
  (decommissioning).
- **R3.13.4** On `homeassistant/status` payload `"online"` (whitespace-trimmed), the
  consumer MUST re-publish every known slot's **stored** (not re-rendered) entity
  set.
- **R3.13.5** Discovery topic layout: `<prefix>/<component>/<nodeID>/<objectID>/config`,
  `nodeID = sanitize("<site>-<station>-<slot>")`, `objectID =
  sanitize(field.key|action.key)`, `sanitize` = lowercase, characters outside
  `[a-z0-9_-]` → `_`.
- **R3.13.6** Field→component mapping: read-only `number`→`sensor`, writable
  `number`→`number` (with command_template), read-only `enum`→`sensor`, writable
  `enum`→`select` (skipped entirely if no command or empty options), `boolean`→`binary_sensor`,
  `string`→`sensor`, each `actions[]`→`button`. Unknown types skipped.
  Every entity carries the slot's own `/status` as availability, a `unique_id` of
  `<nodeID>_<objectID>`, a `value_template` of `{{ value_json.<key> }}`, and a device
  block per slot.
- **R3.13.7** A valid `/meta` with **no `expose`** renders exactly one diagnostic
  `binary_sensor` (object id `online`, name = the role, state_topic = the slot's
  `/status`).
- **R3.13.8** Command rendering follows §1.4/§2.3 exactly: `int` → `{{ value | int }}`,
  `float` → `{{ value | float }}`, string → `"{{ value }}"`.

### 3.14 `muehle/uhf/pol-ctrl` — UHF polarization control (documented gap)

**No component implementing this slot exists in the repository.** The integration model
attributes it to a second M5 Stamp PLC ("PLC #2" driving X-Quad antenna polarization
relays). Nobody has written the PLC #2 firmware. The address appears in the model and
in the operator console's expected-slots list, so a console will report it as
`silent (no state since connect)` — which is the truth. A re-implementation MUST treat
this slot as **planned, not specified**: any contract for it is new design work
(§7.3). Reference-implementation note: the m5stamp firmware's slot table reserves it.

### 3.15 `muehle/uhf/rotator` — UHF pan/tilt head console (pelcobridge2)

It is an **interactive terminal application** (not a daemon) that runs on shack-pc and
drives a PTS-303Z/3050DZ pan/tilt head over RS-485 (Pelco-D/P protocol). A human at
the keyboard is central to its safety model. The MQTT attachment is strictly
read-mostly. The component works without it.

**Topics:** `muehle/uhf/rotator/{meta,state,status,cmd}` — `meta`/`state`/`status`
retained QoS 1. `/cmd` subscribed QoS 1, not retained.

**`/state` payload** (published on every engine state change, deduped by a comparison
key that excludes `ts` and `readback_age_s`. The component caches the last payload
and republishes it on broker (re)connect because a quiescent head emits nothing):

| Field | Type | Unit | Presence | Semantics |
|---|---|---|---|---|
| `ts` | string | RFC 3339 UTC | always | publish time. |
| `az`, `el` | number \| null | degrees | always | **True** (offset-corrected) azimuth/elevation, 0.01 rounding. `null` before first readback. |
| `phys_az`, `phys_el` | number \| null | degrees | always | Raw head readback. |
| `readback_valid` | bool | — | always | Both axes have a readback at all (not freshness). |
| `readback_age_s` | number | s | always | Age of the pan readback, 0.1 rounding. |
| `armed` | bool | — | always | Arm state — **arming is TUI-keyboard-only, never remote**. `false` at every process start (never persisted). |
| `az_offset_deg` | number \| null | degrees | always | The arm offset (physical − true). |
| `moving` | bool | — | always | A jog command or a set ladder is active. |
| `target_az`, `target_el` | number \| null | degrees | omitted when none | Current ladder target in **true** degrees. |
| `set_status` | string | — | omitted when `""` | `"setting"` \| `"converged"` \| `"failed"`. |
| `jog_speed` | int | 0–63 | always | Current jog speed byte. |
| `rotctld_clients` | int | count | always | Connected rotctld TCP clients. |
| `device_online` | bool | — | always | A checksum-valid frame received since the link was last known-dead. |
| `link` | string | — | always | `"ok"` when `device_online` else `"down"`. |
| `error` | string | — | omitted when empty | Last link/engine error text. |

**`/meta`:** `role:"rotator"`, `device:{model:"PTS-303Z/3050DZ"}`, `link:"rs485"`,
`host:"shack-pc"`, `capabilities:{axes:["az","el"]}`. `expose`: `az`/`el`
(`unit:"°"`, min 0–360 / 0–90), `target_az`/`target_el`, `moving`, `armed`,
`device_online`. One action `stop`.

**`/cmd`:** the engine accepts exactly one action:

| Action | Payload | Meaning / resulting behavior |
|---|---|---|
| `stop` | `{"action":"stop"}` | Immediate all-stop frame to the head, any state, always honored. **The engine logs and ignores any other action and any unparseable payload. There is NO MQTT path to motion, arming, calibration, or self-test.** |

- **R3.15.1 (safety, non-negotiable):** Remote motion for this slot exists only through its
  local rotctld TCP server (Hamlib text protocol, default `0.0.0.0:4533`). It exists
  **only while a human has armed the head from the local TUI**. Arming has no construction
  path outside the TUI and the engine rejects Arm intents from any non-TUI source.
  Armed state never survives a restart.
- **R3.15.2** MQTT failure is non-fatal for this component: broker down degrades only
  its status indicator.

### 3.16 Non-slot bus participants

**hf_console (operator console).** A Flutter application (Android tablet primary. Also
iOS and a web channel). It is a pure MQTT client plus one HTTP SSE feed. Bus behavior
(normative):

- Subscribes exactly `muehle/#` at QoS 0. Publishes only `muehle/<slot>/cmd` at QoS 1
  with the per-topic retain flags of §1.5. No LWT, no heartbeat, no slot of its own
  (R1.6).
- Its command payloads are exactly those of §3's `/cmd` tables (the `set_az` top-level
  `az`, the `select`/`request` no-action shapes, `pa-arm` string booleans, tuner real
  bool, `dvk_stop` with no value, and so on). The console is the reference sender
  of nearly every command on the bus. The payload table in
  [04-console.md](04-console.md) comes 1:1 from §3.
- It maintains a fixed **expected-slots list** (the 15 addresses of §3, including the
  `uhf/pol-ctrl` gap). Per slot it reports: `"bridge down"` (status ≠ online),
  `"device unreachable"` (bridge up, device down), and after a **3-second** post-connect
  grace `"silent (no state since connect)"` for expected slots never heard from. Each
  entry carries a per-slot timestamp of when the slot went dark.
- A **web console bridge** (`webbridge`, on shari, port 8091) serves the Flutter web
  build and byte-forwards a WebSocket at `/mqtt` to the broker's TCP port. The forward
  is a pure bidirectional copy with no MQTT interpretation. Browsers cannot open raw
  TCP.

**Bus test bench and capture (auxiliary, not station behavior).** A passive capture
tool records all `muehle/hf/#` traffic verbatim to hourly files (publish-silent by
contract). A web test bench (`testui`) can watch and publish arbitrary bus topics.
Its relay enforces contract rules that any replacement tool MUST preserve: publishes
only under the site prefix, and only to the four plane suffixes. The relay **rejects
retained publishes to `/cmd`** (mirroring §1.5 for manual stimulation). Details in
[03-components/](03-components/).

### 3.17 Host liveness nodes (model-only)

The integration model names `muehle/host/shari` and `muehle/host/shack-pc` as host
liveness topics. **No component publishes them.** They are model-level intent, not
deployed contract. A re-implementation MUST NOT depend on them (§7.3).

### 3.18 The logging layer (specified, not yet implemented)

`docs/logging-integration-model.md` specifies more planes — `log/event` and
`spots/event` per slot, and a `qso-log` (contact-log) role — that **no component
implements yet**. They are future scope. A re-implementation MUST treat them as
open design surface, not as contract. It MUST NOT break the four-plane grammar by
pre-allocating those planes. See §7.3.

---

## 4. Band and mode canonicalization

### 4.1 The canonical band table

The band is ALWAYS derived from `freq_hz` (never a stored setpoint echoed back). The
table below is the station's single normative mapping (IARU Region 1 / German
allocations. Edges inclusive):

| Band | Lower edge (Hz) | Upper edge (Hz) |
|---|---|---|
| `160m` | 1,800,000 | 2,000,000. |
| `80m` | 3,500,000 | 4,000,000. |
| `60m` | 5,351,500 | 5,366,500. |
| `40m` | 7,000,000 | 7,300,000. |
| `30m` | 10,100,000 | 10,150,000. |
| `20m` | 14,000,000 | 14,350,000. |
| `17m` | 18,068,000 | 18,168,000. |
| `15m` | 21,000,000 | 21,450,000. |
| `12m` | 24,890,000 | 24,990,000. |
| `10m` | 28,000,000 | 29,700,000. |
| `6m` | 50,000,000 | 54,000,000. |
| `2m` | 144,000,000 | 146,000,000. |
| `70cm` | 430,000,000 | 440,000,000. |
| `23cm` | 1,240,000,000 | 1,300,000,000. |

**Fallback labels:**

- **R4.1** An HF frequency inside the radio's general-coverage range but outside all
  allocations resolves to `gen` (general coverage). Anything else (unmappable)
  resolves to `unknown`, or the slot omits the field (per slot).
- **R4.2** The Ultrabeam controller's device-native band table is narrower. A device
  band outside its label table resolves to `band-<device band index>` (for example
  `band-7`). Consumers comparing radio vs controller bands MUST treat `gen`,
  `unknown`, and `band-*` as **non-comparable** (they can agree on frequency while
  disagreeing on label).
- **R4.3** The radio bridge applies a **2000 Hz hysteresis** at band edges per slice.
  A frequency straddling an edge keeps its current band until it moves ≥ 2000 Hz past
  it (guards `gen`-band exits only).
- **R4.4** The band-center table for commanding the beam controller (§3.4 `band`
  action): 20m → 14,175,000 Hz. 17m → 18,118,000. 15m → 21,225,000. 12m → 24,940,000.
  10m → 28,850,000. 6m → 51,000,000.

### 4.2 The canonical mode table

Exactly six mode words exist on the bus:

`cw` · `usb` · `lsb` · `am` · `fm` · `data`

The radio bridge MUST omit an unknown mode from `/state`, and it never guesses a
mode. The radio bridge normalizes the radio's firmware mode strings per this map:

| Firmware mode | Bus mode |
|---|---|
| `USB`, `USB-D` | `usb`. |
| `LSB`, `LSB-D` | `lsb`. |
| `CW`, `CW-U`, `CW-L`, `CW_U`, `CW_L` | `cw`. |
| `AM`, `SAM` | `am`. |
| `FM`, `NFM`, `DFM` | `fm`. |
| `DIGU`, `DIGL`, `DATA-U`, `DATA-L`, `FDV`, `FDVU`, `FDVL`, `RTTY-U`, `RTTY-L`, `RTTY`, `PKTUSB`, `PKTLSB`, `DSTR`, `FT8`, `FT4`, `PSK31`, `WSPR`, `JS8` | `data`. |

(The reference docs give a longer and a shorter variant of this map — we give the
union here. §7.3 flags the difference. The principle is
normative: every firmware digital mode maps to `data`.)

---

## 5. The liveness contract

### 5.1 Two layers, by definition

Every slot has two independent liveness layers:

1. **Bridge liveness** — `<addr>/status`: literal `online`/`offline`, retained,
   driven by the MQTT LWT (broker-published on an unclean disconnect) and by explicit
   `online` on every (re)connect. It answers the question of whether the bridge
   process is alive and connected to the broker.
2. **Device liveness** — `<addr>/state.device_online` (boolean inside the JSON
   snapshot). It answers the question of whether the bridge has a working data link
   to the physical device right now. A bridge that is up while its serial cable is
   not plugged in shows `status: online` and `device_online: false` simultaneously. This
   is a normal, important state.

### 5.2 What consumers MUST do

- **R5.1** A consumer MUST consider a slot trustworthy for a purpose only when
  **BOTH** layers are up: `/status == "online"` AND `/state.device_online == true`
  (two-layer AND). Keying safety decisions on `/status` alone is a defect — a live
  incident had the antenna reconciler flapping on a bridge-up-but-radio-down window.
- **R5.2** A consumer MUST treat a `/state` snapshot that **lacks the `device_online`
  key** as device-online (logic slots and the embedded switch legitimately omit it) —
  but a slot with **no `/state` snapshot at all** as device-offline.
- **R5.2a (safety-consumer exception)** The PA-arm chain (§3.7) deliberately reads
  this input the other way. In `radio/state` an absent `device_online` key counts as
  device-**offline** (fail-closed, [06-safety.md](06-safety.md) §2.2). This exception
  applies only to safety-critical consumers. Ordinary consumers, and slots that never
  carry the field (logic slots, the embedded switch), keep the absence-=-true rule of
  R5.2. The radio bridge always publishes `device_online` explicitly (R2.2), so the
  exception changes no deployed behavior. It makes the fail-closed reading legal for
  the one consumer that needs it.
- **R5.3** When either layer drops *after* a selection, the reconciler holds its last
  decision (no chatter, no grounding from liveness alone) — grounding comes only from
  the idle timer.
- **R5.4** The sequencer's `wait_state` steps also need the target's
  `/status` to be `online` at that moment before a cached `/state` can satisfy them
  (§3.12). And `wait_status` on `offline` needs an actual `offline` payload —
  absence never counts.

### 5.3 The clean-shutdown caveat (actual behavior, not the ideal)

**R5.5** On a **clean** process shutdown (SIGTERM, service stop), the broker does
**not** fire the LWT. The retained `/status` therefore stays `"online"` indefinitely
after a stopped service. Deployed components differ in what they publish:

- Most bridges publish an explicit retained `offline` before a clean disconnect
  (sequencer, reconciler, discovery, console-side services) — these self-correct.
- Some bridges disconnect within a ~500 ms quiesce with the will suppressed — for
  those, **the retained `online` outlives the process**. This is actual deployed
  behavior, documented so consumers do not "fix" it into a false model.

**R5.6** Consequence for consumers: `/status` alone can never prove that a service
runs. Staleness of `/state` (each bridge's heartbeat cadence, §3) is the
complementary signal. The console's `silent (no state since connect)` report exists
precisely for services that died leaving fresh-looking retained planes. A
re-implementation MUST pick one behavior (publish `offline` on clean shutdown) and
apply it uniformly — flagged as open decision §7.3.

### 5.4 Session matrix and the retained-replay rule

- **R5.7** A consumer that re-seeds stateless logic from retained inputs (the antenna
  reconciler) MUST use a clean MQTT session so every (re)connect replays all retained
  messages. A persistent session does not replay retained messages for existing
  subscriptions — the consumer can wake up blind.
- **R5.8** Bridges CAN use persistent sessions. But any bridge using one MUST
  re-issue its subscriptions on every (re)connect anyway (deployed practice), because
  a persistent broker session accumulates stale subscriptions when config changes.
- **R5.9 (divergence, flagged §7.3):** the integration-model template states
  `CleanSession = No` (persistent) as the station default, while the reconciler
  mandates clean sessions. Both are true as deployed — the matrix is per-component,
  not station-wide. A re-implementation MUST resolve the two-layer rule R5.7 first.

### 5.5 The pa-arm heartbeat chain (cross-layer)

The PA-arm PLC (§3.7) is a **consumer whose input freshness is safety-critical**: it
holds `armed` only while a parseable `radio/state` message arrives at least every
10 s. This creates a hard producer-side requirement on the radio bridge (R3.3.2,
periodic republish ≤ 5 s) and a hard consumer-side rule (R5.1, two-layer AND — for
this PLC the heartbeat backstop supplies the second layer, not a `/status`
subscription, §3.7). The
deployed system violates the producer side (change-only publishing) and (live
evidence) disarms silently on a quiet band — see §7.3 for the fix mandate.

---

## 6. Cross-slot invariants (wire-level, testable)

These are the statements an integration test can observe on the bus.

**I-1 (pa-arm formula).** At any instant, `muehle/hf/pa-arm/state.armed == true` iff
`enabled == true` AND `radio/state.device_online == true` (an absent key counts as
false, R5.2a. The PLC never reads `radio/status` — the 10 s heartbeat below covers
a dead bridge) AND `radio/state.tuning != true` AND `radio/state.band ∈
SAFE_BANDS` (§3.7 list).
A parseable `radio/state` message must also have arrived within the last 10 s. And
`ant-switch/state.selected` is non-empty and ≠ `"off"`.

**I-2 (arm drop is silent on heartbeat timeout).** When the radio heartbeat
10 s window expires, `armed` MUST drop with **no** `error` string published
(deployed behavior. Consumers must not wait for an error to notice).

**I-3 (no select during TX).** The reconciler MUST NOT publish any
`{"select":…}` to `ant-switch/cmd` while the most recent `radio/state.tx == "tx"`
— the reconciler defers the change, and it fires on the return to `rx` (R3.8.7). The
operator console enforces the same rule client-side for **direct** switch drives
(fail-closed: unknown radio state blocks. Only the reconciler path is exempt, because
the reconciler owns ordering).

**I-4 (settled is conservative).** `ant-switch/state.settled == true` implies the
relays have been stable for 200 ms. A consumer MUST NOT assume `selected` is final
while `settled == false`. (No deployed consumer gates on `settled` yet — documented
backlog, §7.3.)

**I-5 (idle grounding overrides everything).** After 30 minutes with no radio
`freq_hz` change and no TX, the reconciler MUST publish `{"select":"off"}` (retained)
even if an operator hold is active. `antenna-select/state` MUST then read
`{mode:<mode>, target:"off", source:"idle"}` where `mode` is `"manual"` if an
operator hold is still active (a hold keeps `mode` manual even when tier 1 wins the
target) and `"auto"` otherwise.

**I-6 (empty band holds).** The reconciler MUST NOT publish any switch command while
`radio/state.band` is empty (link reconnect state) — resolving it to the fallback
chatters the antenna and is forbidden (R3.8.4).

**I-7 (sequencer ordering).** `pa-arm set_enabled "true"` is **always the last** action
of a startup sequence and `set_enabled "false"` **always the first** action of a
shutdown. 2-second staggers separate the power removals in shutdown (R3.12).
Observably: the sequencer cannot set any `pa-arm/cmd` retain value `"true"` while
`phase == "starting"` before every prior step's wait passed.

**I-8 (no spurious energization).** A sequencer restart MUST NOT cause any `/cmd`
publication to any slot until an operator `start` arrives. The sequencer's own `/cmd`
is QoS 0 and un-retained, so offline commands disappear with no replay (R1.16, R3.12).

**I-9 (no rollback).** When a sequence faults, the sequencer MUST NOT publish any
corrective commands. Slots keep their last retained intent (R3.12). Recovery is
operator-issued.

**I-10 (switch exclusivity and fail-safe).** `ant-switch/state.selected` MUST at all
times be one of `off`/`port1..6` (never two ports). On power loss all relays open. On
the node's reconnect the retained `ant-switch/cmd` re-applies (R3.5.3). A retained
`select` value equal to the current readback produces no relay chatter.

**I-11 (beam never tuned off-circuit).** The reconciler MUST publish
`{"action":"frequency",…}` to `ant-ctrl/cmd` only when the resolved antenna target is
the Ultrabeam's port (R3.8 table) — never tune a beam that is not in circuit.

**I-12 (compound-device identity).** `hf/switch/meta.device` and
`hf/pa-arm/meta.device` MUST carry the same `model` and `serial` (R2.2) — the only
bus-visible fact that one PLC serves both slots. A consumer that sees one slot's LWT
fire and not the other's sees a **deliberate** state (two independent connections
from one process: both fire on crash, one can be cleanly restarted alone).

**I-13 (discovery passivity).** `muehle/hf/discovery` MUST NOT publish to any topic
under `muehle/` except its own `meta` and `status` (R3.13) — any `/cmd` publication
from this client is a defect.

**I-14 (UHF rotator stop-only).** `muehle/uhf/rotator` MUST accept exactly
`{"action":"stop"}` on `/cmd`. No MQTT message can move, arm, or calibrate the head
(R3.15.1). Its `/status` going offline MUST NOT imply the head moved.

**I-15 (freq→band derivation).** No slot MUST publish a `band` value that is not
derivable from its own `freq_hz` (or device readback) through §4.1 — bands are never
stored setpoints (R1.20, R4 preamble).

**I-16 (6 m pattern constraint, console-enforced).** While
`radio/state.band == "6m"` and the controller is online and not moving, the operator
console auto-publishes `{"action":"direction","value":"forward"}` to `ant-ctrl/cmd`
once per invalid state if `ant-ctrl/state.direction != "forward"`. The Ultrabeam's
elements support only the forward pattern on 6 m. The console also disables its
180°/BI-DIR buttons on 6 m. (A UI-side guard mirrored from the controller vendor's own
web UI. The bridge itself does not enforce it.)

**I-17 (PSU root-cause visibility).** The console's faults report MUST show the
PSU-root-cause line when §3.2's conditions hold. It MUST NOT show it when the
`power` key is absent (R3.2.1). It MUST never invent OFF from missing data.

**I-18 (radio state freshness).** The radio bridge MUST republish `/state` at least
every 5 s while the link is up (R3.3.2) — the fix side of the pa-arm starvation chain
(I-1, §5.5). Deployed behavior is change-only. A re-implementation MUST add the
heartbeat. Any consumer adding a freshness gate of its own MUST use ≥ 10 s.

**I-19 (handler isolation, any stack).** No MQTT message handler MUST block on a
publish anywhere in the system (R1.24). No connect MUST outlive process shutdown
(R1.25). No publish MUST wait unbounded (R1.26). These are library-independent and
traceable to live incidents.

**I-20 (console silence).** The operator console MUST NOT create any topic under
`muehle/` other than by publishing `/cmd` messages per §1.5's retain table (R1.6).
The bus must be able to run headless without it.

---

## 7. Open decisions and unresolved facts

This document leaves nothing in this section resolved. A re-implementation MUST surface each one to the
product owner rather than silently picking a side. Where two sources disagree, this
document states both with their evidence.

### 7.1 Antenna-switch wiring: two contested ports — requires on-device confirmation

Two port numbers in the wiring map are **not knowable from the repository**. The
Ultrabeam's switch port (3 or 4) and the fan-dipole's switch port (6 or 2) are both
contested facts.

**Ultrabeam — `port3` or `port4`:**

- **Evidence for `port3`:** the repo-root project instructions and the
  station-integration model's wiring map list the Ultrabeam on switch port 3.
  The antennaselect unit tests assert `port3`.
- **Evidence for `port4`:** `antennaselect/config.example.toml`, the antennaselect
  `deploy.sh` **seed** (what a fresh device can get), and the operator console's
  antenna port map all say `port4`.

**Fan-dipole — `port6` or `port2`:**

- **Evidence for `port6`:** the repo-root project instructions, the
  station-integration model's wiring map, `antennaselect/config.example.toml`, and
  the operator console's antenna port map all say `port6` (80/40 m fan dipole).
- **Evidence for `port2`:** the station-integration model's own passive-resource
  list says "fan-dipole … (port 2)". This contradicts the model's wiring map in
  the same document.

**Uncontested:** `port1` is the dummy load in every source. No source documents
anything wired to `port5`. The operator console nevertheless renders `port5` as a
selectable position and publishes `{"select":"port5"}` when an operator picks it.

**The live truth** lives in `/etc/antenna-select/config.toml` on shari (seeded as
`port4`/`port6`, possibly hand-edited since) and is **not readable from the workstation**
(0600, on-device).

Both variants of each port are plausible wiring. **Any re-implementation MUST treat
the wiring map as pure configuration, and MUST confirm both contested ports on the
device before deploying**. If the wiring is wrong, the band policy routes 20 m–6 m
transmissions into the wrong antenna, or into an unwired port (an open/short).
The console then displays a wrong label. Getting a port wrong does not affect the
bus contract — the `selected`/`select` values are port keys either way.

### 7.2 Broker topology: which broker is production?

- Deployed code defaults (all services, console setup screen, webbridge) point at
  **`tcp://192.168.1.50:1883`** (the Home-Assistant Mosquitto add-on). This document
  treats `.50` as production (§1.7).
- Committed-but-**unmerged** work (`feat/shack-local-mqtt-broker`, commit `cd58466`,
  2026-08-26, resume planned ~2026-09-02) introduces a **shack-local broker on shari
  at `192.168.1.139:1883`**, authoritative for `muehle/#` and bridged to `.50`, so
  the station bus survives a shack↔house network drop.
- **Contradicting evidence in the deployed field:** the m5stamp PLC's on-device
  secrets/config point at **`192.168.1.139`** (shari). This means at least one deployed
  component already talks to the shack broker, or the operator configured it in anticipation of
  it. Its config diverges from every Go service.
- The hf_console project docs already *describe* `.139` while its code defaults to
  `.50` — docs and code disagree until the migration merges and deploys.

**Decision needed:** single broker at `.50`, or shack broker at `.139` bridged to
`.50`. The wire contract itself is broker-location-agnostic (addresses are config).
The ACLs, the console account, and the webbridge default all follow the choice.
The re-implementation must reconcile the m5stamp divergence either way.

### 7.3 Source disagreements and unresolved facts

1. **`device_online` form.** Integration model: "omitted when true". Deployed bridges:
  explicit `device_online:true`. Consumers must accept both (R2.2/R5.2). A
  re-implementation must decide: mandate explicit-true (recommended: fewer consumer
  corner cases) or keep the dual rule.
2. **Clean-session divergence.** Integration-model template says persistent sessions.
  The reconciler mandates clean sessions for retained replay (§5.4, R5.9). Resolve to
  a per-component matrix in the re-implementation (R5.7 governs).
3. **Clean-shutdown `/status`.** Some bridges publish `offline` before clean
  disconnect, others suppress the will and leave retained `online` (§5.3). Unify (one
  behavior) in the re-implementation.
4. **Mode-normalization map variants.** The radio-bridge's own code table
  (flexbridge `NormalizeMode`, shorter — for CW it accepts only `CW`, `CW-U`,
  `CW-L`) and the shared docs' table (longer, adds `CW_U`/`CW_L`, `RTTY`, `FT8`,
  `FT4`, `PSK31`, `WSPR`, `JS8`) differ. §4.2 gives the union. Confirm which
  firmware mode strings the FLEX-8400 actually sends and trim the map to observed
  reality.
5. **`hf/switch` and `hf/pa-arm` `ts` is uptime-milliseconds** (§3.6/§3.7) and
  **`hf/ant-switch` `ts` depends on the HA clock** (R3.5.2) — both deviate from the
  RFC 3339 rule (R1.17). Consumers tolerate both today. A re-implementation
  must normalize, and this is a **deliberate contract change** if done (consumers
  keyed on the number type can need updating).
6. **Radio state heartbeat.** Deployed flexbridge is change-only. The pa-arm
  starvation chain (I-1, I-2, §5.5) makes a ≤ 5 s periodic republish a **hard
  requirement** for the re-implementation (I-18). This changes observable bus traffic
  (a quiet band now produces a publish every ≤ 5 s) — accept it and give it contract
  status.
7. **Idle-grounding recovery gaps (live-observed 2026-08-28):** (a) the first key-up
  after grounding transmits into the grounded switch (re-arm is itself a TX, so the
  reconciler defers the select). (b) change-only publishing starves recovery (ties to
  #6). (c) there is no non-TX re-arm path (an operator at the desk who never touches
  the VFO cannot un-ground). (d) a reconciler restart always re-arms ACTIVE and resets
  the 30-minute clock. The auto-ground itself works live. The memory notes document
  the fix directions (operator-presence re-arm, TX-deferral timeout, restart-stable
  idle state) but these are **new design**, not contract.
8. **`pa-arm` recomputation gated on WiFi** — `armed` freezes (instead of dropping)
  during a PLC WiFi outage (§3.7). Safety requires freeze→drop. Fix in the PLC
  firmware re-implementation.
9. **Powerseq deploy seed omits the sequences** — `deploy.sh` seeds a config without
  `[[startup]]`/`[[shutdown]]` lists, which validation requires. A first deploy
  crash-loops until an operator hand-adds them. The re-implementation MUST ship a
  complete seed (or built-in defaults).
10. **The sequencer drops a `stop` from `idle` without a fault** (§3.12): there
    is no way to shut down through the sequencer unless it is in `running` or
    `idle+fault`. Documented intent vs. operator expectation — decide whether an
    explicit cancel command must exist (it is a deliberate contract change,
    not a bug fix).
11. **The console enables its START button during phase `stopping`** (console defect
    list) — the sequencer drops the mid-stop `start`, but the UI must gate it.
12. **The reconciler does not validate operator `request` strings** (any string
    becomes a hold target forwarded verbatim to the switch, §3.8) — validate against
    the wiring map in the re-implementation.
13. **The reconciler does not read `settled`/`tuning`** (I-4) — the
    settled-wait handshake remains backlog for the ant-switch work.
14. **The station left ant-switch ports 2, 3, and 5 without wiring** (under the
    `port4`/`port6` reading of §7.1): the console renders `off, port1, port4,
    port5, port6` as selectable positions — including the unwired `port5`.
    The switch firmware itself supports all six positions.
15. **UHF rotator password source.** The interactive console's docs say the MQTT
    password never comes from its config file. Its code accepts a config-file
    fallback (for a double-clicked GUI process with no environment). Decide which
    contract to keep (§3.15).
16. **Test-bench credential hygiene.** The repository commits a real-looking MQTT
    password in the test bench's checked-in bench config. The deployment path is correct — a
    separate 0600 EnvironmentFile on the device. The checked-in file is not. Rotate the
    credential and purge it from history during re-implementation. Treat all bus
    credentials as compromised if anyone ever shared the repo.
17. **Test bench publishes unauthenticated on the LAN** (bind `0.0.0.0:8090`, no auth
    on its publish endpoint) — anyone on the LAN can command the station through it.
    The replacement tool MUST add authentication or bind to an ops-only interface.
18. **PLC #2 / `uhf/pol-ctrl` does not exist** (§3.14) — contract TBD. The console
    reports it as silent, which is correct today.
19. **Host liveness nodes** (`muehle/host/shari`, `muehle/host/shack-pc`) are
    model-only (§3.17) — build them, or delete them from the model.
20. **Logging layer** (`log/event`, `spots/event`, `qso-log` role): docs define it,
    but it has no deployed code (§3.18) — future scope. The re-implementation MUST NOT
    extend the four-plane grammar until the design exists.
21. **PA `error`/`fault` clear-value inconsistency** (`""` vs `"NONE"` vs `"none"`
    across slots, R2.3) — unify on omitted-when-empty in the re-implementation
    (consumer-side case-insensitive comparison is the deployed mitigation).
22. **The `testui` band table** lists 6 m as 50–53.999999 MHz (a float artifact of
    54 MHz) and 160 m upper edge as 1.999999 MHz — treat §4.1's integer edges
    (2,000,000 / 54,000,000) as authoritative.
23. **Ultrabeam `mode` action** is a deprecated alias of `direction` (§3.4) — keep
    accepting it for old senders, or drop it. Decide in the re-implementation.
24. **Console is not a slot** (R1.6): the bus cannot check whether the operator
    console stays connected. If console presence becomes operationally relevant, a
    `muehle/hf/console` slot (meta/status at minimum) is new design work.
25. **The power bridges' heartbeat handler falsifies `power`** (§3.1, known
    deployed defect): a plug heartbeat `"true"` sets `power:"on"` even while the
    relay is off. The wrong state persists until the next native relay
    announcement. A re-implementation MUST make the heartbeat refresh only
    `device_online` and `error`, and leave `power` untouched.
26. **`value_type` boolean spelling diverges** (§2.3): the tuner's meta publishes
    `bool` while the PLC slots' metas publish `boolean`. Pick one spelling in the
    re-implementation and treat the change as a deliberate contract
    normalization.

---

*End of 02-interface-spec.md. Cross-references: [README.md](README.md) ·
[00-system-overview.md](00-system-overview.md) · [01-architecture.md](01-architecture.md) ·
[03-components/](03-components/) · [04-console.md](04-console.md) ·
[05-deployment-ops.md](05-deployment-ops.md) · [06-safety.md](06-safety.md) ·
[07-priorities-milestones.md](07-priorities-milestones.md)*