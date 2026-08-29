# 03 — antennaselect: the HF antenna-selection reconciler

> Cross-references: [../00-system-overview.md](../00-system-overview.md) for the station
> as a whole. [../01-architecture.md](../01-architecture.md) for the four-plane MQTT bus
> model. [../02-interface-spec.md](../02-interface-spec.md) for the exact per-slot topic
> and payload contracts this component consumes and emits.
> [common-runtime-library.md](common-runtime-library.md) for the shared transport
> behaviors (context-aware connect, non-blocking handlers) every component inherits.

**Purpose.** This document gives the full specification for the **antenna-selection
reconciler** (working name `antennaselect`). It is a small, always-running, *stateless*
policy service for the Mühle amateur-radio station. Amateur radio ("ham radio") is the
licensed hobby of two-way radio communication. A station like Mühle's consists of a radio
transceiver ("radio" — the device a licensed operator uses to transmit and receive), a
power amplifier (**PA** — a device that boosts the radio's transmit signal), an antenna
tuner (**ATU** — an impedance-matching device needed when an antenna is not resonant on
the frequency in use), and several physical antennas. All antennas wire to a single
1-of-6 relay **antenna switch** (only one antenna connects to the radio at a time). The
switch's `off` position shorts all antenna feeds to electrical ground — the station's
lightning and walk-away protection. The reconciler has **no device and no I/O of its
own**. It is a "logic slot". It watches the radio's current operating state on the
station's MQTT message bus. It decides which antenna to connect now, and it commands the
antenna switch (and three "follow" bindings) to make it so. This includes automatic
grounding of the antennas after 30 minutes of radio inactivity. A re-implementation team
must be able to rebuild this component from this document alone, indistinguishable on the
bus from the current one.

Terminology used throughout, defined once here and referenced below:

- **MQTT** — a lightweight publish/subscribe messaging protocol. Clients publish
  messages to hierarchical text topics (`muehle/hf/radio/state`, for example) on a
  central **broker**. Subscribers receive messages for the topic filters they subscribed
  to.
- **Retained message** — a message the broker stores per-topic and replays immediately to
  every new subscriber, so a late joiner sees the latest value without polling.
- **LWT (last will and testament)** — a message the broker publishes on the client's
  behalf if the client disconnects uncleanly. The station uses it for liveness.
- **Slot** — the station's unit of addressability. Every component (device bridge or
  logic service) owns exactly one topic address `<site>/<station>/<slot>` with four
  topic planes (`meta`, `state`, `status`, `cmd`). See
  [../01-architecture.md](../01-architecture.md). This component's slot address is
  `muehle/hf/antenna-select`.
- **Bridge** — a small service fronting one physical device onto the bus.
- **Band** — the wavelength name of the operating frequency (`20m` for the
  14 MHz amateur band, for example). It is always *derived* from the integer frequency in
  Hz (`freq_hz`), never stored independently.
- **TX / RX** — transmit / receive. `tx` fields hold the strings `"tx"` or `"rx"`.
- **RF (radio frequency)** — the radio signal energy itself. An "RF path" is the
  signal route from the radio through the switch to an antenna. "RF safety"
  concerns the hazards of that energy.
- **Key, key-down, key-up, un-key** — ham vocabulary for the transmit control,
  from telegraph practice. To "key" the radio is to start transmitting. A
  key-down starts the transmission. A key-up or an un-key ends it.
- **Slice** — the radio's vendor name for one independent receive channel. The
  radio bridge reports no slice while it reconnects to the radio (§5.4).
- **Carrier** — the transmitted RF signal. "No carrier" means the station
  transmits nothing.
- **SWR** — standing-wave ratio, a measure of impedance mismatch on an antenna feed.
  High SWR means the antenna is non-resonant and needs an ATU.
- **QoS 1** — MQTT at-least-once delivery, used for every publish and subscribe in this
  station.

Everything in §§3–12 is a **normative behavior contract**: a re-implementation must
reproduce it exactly. Section 13 carries the configuration schema. §14 carries the
deployment contract, §15 the invariants, and §16 the known defects with normative fix
requirements derived from live incidents. §18 carries the open decisions.

---

## 1. Role in the system

The station's actuators are deliberately "dumb": the antenna switch, the beam
controller, the PA and the ATU each accept simple commands and report their state.
Policy lives in this one reconciler. It exists to answer one question continuously:
*which antenna must connect*, given everything the bus knows right now? The design
fans out the consequences of that answer to the other components.

Concretely it must:

1. Subscribe to the radio's state (band, frequency, TX flag, link liveness) and the
   antenna switch's reported position.
2. Resolve a single **decision** — a target switch position plus the *reason* for it —
   using a fixed three-tier priority ladder (§5): idle grounding > operator hold >
   band policy.
3. Emit that decision as commands to the antenna switch (§4.1) and as its own state for
   every other consumer (§3.3).
4. Drive three "follow" bindings as consequences of the radio state and the resolved
   target (§8). The rotatable-beam controller tracks the radio frequency. The
   reconciler pre-positions the PA to the radio's band. The reconciler moves the ATU
   into or out of the RF path, depending on whether the selected antenna is resonant.
5. Automatically select the switch's `off` (grounded) position after a configurable
   idle timeout (default 30 minutes) with no radio activity. An unattended station
   therefore cannot leave an antenna hanging live (§6).

The reconciler is **never part of the RF-safety enforcement path**. A hardware
interlock chain enforces safety (TX-inhibit, PA-arm, cold-switch hard limits). The
reconciler owns *ordering and intent* only. Killing it degrades the station
to manual operation but cannot make it unsafe (see [../06-safety.md](../06-safety.md)).

It runs as an always-on service on the host `shari` (a Raspberry Pi at
192.168.1.139). It talks only to the MQTT broker. Its inputs and outputs are exclusively
MQTT topics — there is no HTTP API, no serial device, no local file state.

---

## 2. Upstream interfaces

There is no physical device. The upstream interfaces are:

- **MQTT broker** at `tcp://192.168.1.50:1883` (production, configurable — see §13 and
  the broker-migration open decision in §18). Credentials: username `hf`, password
  held only in the on-device config file, never on any command line.
- **Sibling slots consumed** (exact payload contracts in
  [../02-interface-spec.md](../02-interface-spec.md)):
  - `muehle/hf/radio` — the FLEX-8400 radio bridge ("flexbridge"). It publishes a
    retained JSON state snapshot on `.../state` **only when its content changes** (no
    periodic heartbeat — this property is load-bearing and drives several defects,
    §16). It publishes plain-string `online`/`offline` on `.../status` (its LWT).
  - `muehle/hf/ant-switch` — the 1:6 relay antenna-switch bridge. It publishes retained
    state on `.../state`. It accepts `select` commands.
- **Fixed input slot names (deliberate deviation).** The `radio` and
  `ant-switch` slot segments are hard-coded canonical role names in the component
  (reference: `slotRadio = "radio"`, `slotAntSwitch = "ant-switch"`). No configuration
  key exists for them. This is a deliberate, documented deviation from the station rule
  that slot names are configuration, never code (§13). A rebuild must use exactly these
  two slot segments for its radio and ant-switch subscriptions and commands. A rebuild
  that makes them configurable must default to these values.
- **Sibling slots commanded**: `muehle/hf/ant-ctrl` (the Ultrabeam RCU-06 rotatable-beam
    controller bridge, "ultrabridge"), `muehle/hf/pa` (the ACOM 1200S PA bridge),
    `muehle/hf/tuner` (the ATR-1000 ATU bridge). The slot names of these three come
    from configuration, not code (§13).

### 2.1 Session semantics (normative)

The client must connect with **clean session** enabled. This is deliberate and
load-bearing: on every (re)connect the broker drops the prior session and replays all
retained input topics into the fresh subscriptions. The stateless
reconciler then re-seeds all its inputs and re-emits its follow intents after any outage.
(A persistent session does *not* replay retained messages for existing subscriptions,
and a reconciler on one then wakes up blind.) The client must auto-reconnect on broker
loss with no operator intervention. The reconnect handler re-runs the full birth
sequence (§3.1), and the retained replay does the rest.

---

## 3. MQTT presence

Topic base: `muehle/hf/antenna-select` (assembled from configurable
`site`/`station`/`slot` — §13). Every publish is QoS 1. Client ID defaults to
`<site>-<station>-<slot>` with `/`→`-`, that is `muehle-hf-antenna-select`
(configurable).

### 3.1 Birth sequence (on every (re)connect, in this order)

1. Publish `muehle/hf/antenna-select/status` = plain string `online`, retained, QoS 1.
2. Publish `muehle/hf/antenna-select/meta` (retained, §3.2).
3. Subscribe, QoS 1, to the four input topics of §3.4.

Also, the client must register an LWT of plain string `offline` (retained, QoS 1) with
the broker at every connect. An abrupt process death then flips `/status` without
the process doing anything. On a *clean* shutdown (§10.6) the process publishes
`offline` itself before disconnecting.

**Known caveat inherited from the bus model:** on a clean shutdown the broker does not
fire the LWT. The explicitly-published `offline` does land. But consumers in
general must never trust `/status` alone, and this reconciler itself must not either
(§7).

### 3.2 `/meta` — birth certificate

Retained JSON, re-published on every connect. Exact deployed shape (with all three
follow bindings enabled, as at Mühle):

```json
{
  "schema": "1.0",
  "role": "reconciler",
  "link": "none",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "controls": "ant-switch",
    "follows": { "ultrabeam": "ant-ctrl", "pa": "pa", "tuner": "tuner" },
    "ladder": ["idle", "operator", "auto"]
  },
  "expose": {
    "device": { "name": "Antenna selector" },
    "fields": [
      { "key": "source", "name": "Source", "type": "enum", "options_ref": "ladder" },
      { "key": "target", "name": "Target", "type": "string" },
      { "key": "mode",   "name": "Mode",   "type": "string" }
    ]
  }
}
```

Normative details:

- `role` must be the canonical role name `reconciler`, never a product name.
- `link` must be `none` (a logic slot fronts no device transport).
- `location` and `host` must come from configuration (deploy values: `bauwagen`,
  `shari`), never from code.
- `capabilities.ladder` must always be `["idle", "operator", "auto"]` (fixed — the
  order documents the precedence of §5).
- The client assembles `capabilities.follows` from configuration: entry
  `<resource>: <slot>` for the band-follow resource (if enabled — the key `ultrabeam` in
  the example gives the *passive-resource* identifier, not a band), plus `<slot>:
  <slot>` for the PA follow and `<slot>: <slot>` for the tuner follow (if enabled).
  Omit the whole `follows` key when the configuration disables all three bindings.
- `expose` is a consumer-neutral description of the three read-only `/state` fields
  (see [../02-interface-spec.md](../02-interface-spec.md) §expose for the general
  schema, and a Home-Assistant discovery consumer renders UI purely from it). A
  re-implementation must keep its keys and shape. It carries no behavior.

### 3.3 `/state` — the resolved decision

Retained JSON snapshot on `muehle/hf/antenna-select/state`, published **only when the
resolved decision changes** (deduped — there is no periodic republication). Exact shape:

```json
{ "ts": "2026-07-06T12:34:56Z", "mode": "auto", "target": "port4", "source": "auto" }
```

| Field | Type | Contract |
|---|---|---|
| `ts` | string | RFC 3339, UTC, time of this decision. |
| `mode` | string | `auto` \| `manual`. **Derived**: `manual` iff an operator hold (§4) is active, else `auto`. There is no separate mode switch anywhere. |
| `target` | string | the switch position the reconciler now wants: `off` or a wiring-map port key (`port1`…`port6`). The internal "hold last selection" marker is the empty string. See the empty-target rule below this table. |
| `source` | string | *why*: `idle` \| `operator` \| `auto` — the ladder tier that won (§5). |

`target` is **intent**, not fact: the reconciler always reads the switch's actual
position from `ant-switch/state.selected`, and it re-asserts on mismatch (§8.4).
Publishing rule: emit iff the `(mode, target, source)` triple differs from the last
published one, or it is the first ever.

**Empty-target publish rule.** The reconciler publishes the decision triple on
change even when the target is the empty hold-last marker. `target:""` appears on
`antenna-select/state` in two cases:

1. as the first publish after a process start with unresolved inputs, and
2. on every later transition into hold-last (radio offline, empty band).

A rebuild must keep this normative current behavior to stay indistinguishable on
the bus.

### 3.4 Subscribed topics

All QoS 1. The connect handler re-establishes them on every (re)connect. Retained
messages replay into each subscription at connect.

| Topic | Payload | Fields used |
|---|---|---|
| `muehle/hf/radio/state` | JSON (retained by the radio bridge) | `band` (string, canonical band name — `""` transiently during radio-link reconnect), `freq_hz` (integer, Hz), `tx` (`"rx"` \| `"tx"`), `device_online` (bool — radio-link liveness) |
| `muehle/hf/radio/status` | plain string `online`/`offline` (LWT) | bridge-process liveness. |
| `muehle/hf/ant-switch/state` | JSON (retained) | `selected` (string: `off` or a port key), `settled` (bool — parsed but unused now, see §16). |
| `muehle/hf/antenna-select/cmd` | JSON (retained) | `request` (string) — operator hold/release, §4 |

The handler logs malformed JSON on any input topic and drops it. Nothing must crash or
disconnect. Nothing is periodic except the internal 5 s idle check (§6), which itself
publishes only on decision change.

---

## 4. Command surface (operator input)

The component accepts exactly one command topic: `muehle/hf/antenna-select/cmd`,
**retained** (deliberate, so an operator hold survives a reconciler restart through
retained replay). Payload: `{"request": "<value>"}`. Note this is the reconciler's own
command shape. It differs from the `{"action","value"}` convention used on the bridges
it commands — each topic's exact shape is normative per its owner.

| `request` value | Required effect |
|---|---|
| `port1`…`port6` or `off` | engage an **operator hold** (ladder tier 2) on that switch position. `mode` becomes `manual`. |
| `auto` | release the hold. Return to band-policy selection (tier 3). |
| empty/absent JSON payload (for example, a cleared retained message) | treated as a release. |
| any other non-empty string | still engages a hold with that literal string as target (unvalidated — current behavior, see §16.8). |

Effects of a hold:

- The reconciler commands the switch to that position, subject to the cold-switch TX
  deferral (§9).
- The reconciler ignores the band policy until it receives `auto` or the station goes
  idle.
- A hold does **not** count as activity for the idle timer (§6) and does **not**
  override an idle grounding (§5, §6).

There is no read/ack protocol. The observable acknowledgment is the retained
`antenna-select/state` (`source: "operator"`, `mode: "manual"`), and then the
`ant-switch/cmd` emit.

---

## 5. The decision ladder (normative core)

The reconciler is stateless: every decision is a pure function of configuration plus
the latest inputs. The reconciler re-evaluates the ladder on **every** input change
(any of the four subscriptions, any operator command, every idle-check tick). The
component fixes the precedence and it is **not configurable** (a `[priority]` section
in the example config is documentation only):

```
Tier 1  idle:     station activity == "inactive"  → target = "off",  source = "idle"
Tier 2  operator: operator hold active            → target = request, source = "operator"
Tier 3  auto:     band policy(radio.band)         → target = port,   source = "auto"
```

Exact rules, each normative:

1. **Tier 1 wins over everything, including an operator hold.** This is deliberate
   walk-away safety. A forgotten operator hold must never keep an antenna hanging
   live on an unattended station. The component treats unknown activity state as
   `active` (fail toward the less drastic tier).
2. **Tier 2** is active iff the latest operator `request` is non-empty and not
   `"auto"`.
3. **Tier 3** resolves only if the radio passes the two-layer liveness gate (§7)
   **and** `radio/state.band` is non-empty. Resolution: scan the configured
   `band_policy.bands` in **sorted resource order** (a determinism requirement) for the
   band. The matched resource maps to a switch port through the inverted wiring map
   (§6.3). A **known-but-unmatched** band — `160m`, or the `gen` marker used for
   in-band-but-unallocated frequencies — maps to the configured `band_policy.fallback`
   (at Mühle: `fan-dipole`, whose port — 6 or 2 — is open, §18.1).
4. **An empty band (`""`) holds the last selection** (target = internal empty marker,
   no command emitted). This is the radio bridge's transient "reconnecting, no slice
   reported yet" state, not a tuning intent. Resolving it to the fallback chattered
   the antenna on every reconnect cycle. The reference regression tests cover this
   rule, and it must stay.
5. **Radio offline likewise holds the last selection** (no chatter, no grounding from
   liveness alone) — but a **tier-2 operator hold still asserts while the radio is
   offline** (a hold does not depend on radio liveness).
6. `mode` must be `manual` whenever a hold is active — **even when tier 1 wins the
   target** (an idle-grounded station with a stale hold then reports
   `mode:"manual"`, `source:"idle"`, `target:"off"`).

---

## 6. Idle grounding and activity inference

### 6.1 Activity — inferred, never operator-set

"Station activity" is a derived fact, not a command. The reconciler must treat as
**activity** the arrival of a `radio/state` message whose:

- `freq_hz` differs from the last-seen `freq_hz` (the operator turned the VFO — the
  radio's frequency-tuning knob), **or**
- `tx == "tx"` (the operator transmits).

Either sets activity `active` and resets the idle clock. Nothing else — not an
operator command, not a switch-state message, not anything else — re-arms activity
(current behavior — the holes this leaves are §16.2 and §16.3).

### 6.2 The idle check

An internal timer ticks every **5 seconds** (`idleCheckInterval`, a fixed default, not
configurable). On each tick, if `now - lastActivity >= idle.timeout_minutes`
(configurable — **default 30 minutes**, integer, must be positive), the reconciler
sets activity to `inactive` → ladder tier 1 → target `off` → emit
`ant-switch/cmd {"select":"off"}` (retained). Grounding therefore lands within ~5 s
after the timeout expires. The switch's `off` position (not merely "no antenna")
shorts all open antenna feeds to ground.

### 6.3 Grounding state machine, exactly

Two states live in the activity input:

- **ACTIVE** (`active` or unknown): tiers 2/3 can select any port.
- **GROUNDED-IDLE** (`inactive`, set only by the idle check after the timeout): target
  forced to `off`, `source:"idle"`, overriding operator holds.

Transitions:

- **ACTIVE → GROUNDED-IDLE:** the 5 s idle check finds `now - lastActivity ≥ timeout`
  (default 30 min). It emits `{"select":"off"}` unless the radio transmits at that
  instant (TX deferral, §9).
- **GROUNDED-IDLE → ACTIVE:** the **only** re-arm in the current design is a
  `radio/state` message with `freq_hz` ≠ last-seen `freq_hz` or `tx == "tx"`. Tier 3
  then resolves the band's port, and the reconciler emits a select. But if the
  re-arming message was the TX itself, the select stays TX-deferred and fires only
  when the radio reports `rx` again (the first-key-up fragility, §16.1).
- **Process restart:** the idle clock comes back as "now" and the last-seen frequency
  comes back as 0. The retained `radio/state` replay then always looks like a
  frequency change. **A restart therefore always re-arms ACTIVE** and resets the
  walk-away clock (§16.5).

### 6.4 Recovery — normative fix requirements (derived from live incidents)

Auto-grounding worked live on 2026-08-28. **Recovery is the fragile half**. Any
re-implementation must address the following, which the current design
leaves open (§16 catalogues the defects — these are the required fix directions):

1. **A non-TX re-arm path must exist.** In the current design the only un-ground
   trigger is a radio frequency change or a key-down. An operator at the desk who
   never touches the VFO cannot un-ground the station, except by transmitting into the
   grounded/short port (§16.1). A re-implementation must treat an operator command on
   `antenna-select/cmd` (and/or fresh retained radio state on startup) as operator
   *presence*, re-arming activity without RF.
2. **The radio-state starvation problem is out of this component's sole control but
   load-bearing here:** the radio bridge publishes state only on change. During a
   quiet receive period no messages arrive and the reconciler cannot distinguish
   "idle-but-alive" from "abandoned". The system-level fix — a periodic radio-state
   heartbeat (about every ≤5 s while the link is live) — is a hard requirement on the
   *radio bridge* (see [flexbridge](flexbridge.md) and
   [../06-safety.md](../06-safety.md)). The requirements on this component are:
   (a) a radio-state message whose content equals the last-seen state must not reset
   the idle clock or re-arm activity. An added heartbeat therefore cannot defeat idle
   grounding (§6.1 already defines unchanged `freq_hz` and `tx == "rx"` as no activity).
   (b) The reconciler must stay correct when the radio bridge publishes state only on
   change. The design must not depend on the presence of a heartbeat. (c) The heartbeat
   itself is a requirement on the radio bridge and is out of this component's scope.
3. **Restart-stable idle state.** The reconciler restart always re-arms the station
   and resets the 30-minute clock (§16.5). A re-implementation must persist the idle
   clock (or derive it from the last `radio/state` timestamp) across restarts. Then a
   restart during an unattended idle period does not un-ground the antennas.

**Scope of these fix requirements.** The three items above are deliberate exceptions
to the bus-indistinguishability goal in the Purpose. A rebuild will differ from the
reference service exactly in these behaviors, and the rebuild must include these
fixes. Everything else in §§3–12 must reproduce the reference exactly.

---

## 7. Two-layer liveness gate (normative, live-incident-derived)

The reconciler must only trust radio-derived fields (`band`, `freq_hz`, `tx`) when
**both** liveness layers are up:

- `radio/status == "online"` — the radio *bridge process* is alive (broker LWT layer).
  This stays `online` while the bridge is up even if the radio itself is unreachable.
- `radio/state.device_online == true` — the radio's own network link is up.

Define `radioOnline = (bridgeOnline AND deviceOnline)`. The reconciler recomputes it
whenever either message arrives. The gate must survive retained-replay in any arrival
order (the design evaluates it per message from stored latest values, never per
message in isolation). Rationale, from a live incident: earlier code keyed on
`/status` alone and **chattered the antenna**. A bridge-up-but-radio-link-down window
carries a stale or empty-band `radio/state`, and the reconciler flapped the antenna to
the fallback and back. This AND gate and the empty-band-hold rule (§5.4) are the two
halves of that fix.

If either layer drops *after* a selection, the reconciler must hold the last
selection (no chatter, no grounding from liveness alone).

The `device_online` field's *form* is an open decision (§18.2): the model says the
field is "omitted when true". The deployed bridges publish `device_online:true`
explicitly. Consumers — including this one — must treat absence as `true`.

---

## 8. Cross-slot command fan-out

The reconciler emits four command streams. Every command's exact payload shape,
retention flag, and emission condition is normative. Each target bridge's own contract
is authoritative for its shape (see [../02-interface-spec.md](../02-interface-spec.md)).

### 8.1 Antenna switch selection

Topic: `muehle/hf/ant-switch/cmd` — **retained**, QoS 1. Exact payload (the switch
bridge's own shape — note the key is `select`, *not* the station-wide
`{"action","value"}` convention):

```json
{ "select": "port4" }
```

Value is `off` or a port key. Emission conditions:

- the resolved target has a known value (non-empty),
- the target differs from `ant-switch/state.selected` (or the switch position is
  still unknown), and
- the radio is not transmitting (§9).

Re-assertion semantics (regression-tested, must stay): while the switch's
`selected` is still unknown (before the first `ant-switch/state` arrives), the
reconciler sends at most one command per target. Once `selected` has a known value,
the reconciler re-issues the command on every input change where the switch reports a
position different from the target. So the reconciler **re-asserts a manual override
of the switch automatically**. A same-band frequency change emits no command.

### 8.2 Band-follow (beam controller)

Topic: `<band_follow.slot>/cmd` = `muehle/hf/ant-ctrl/cmd` — **retained**, QoS 1.
Exact payload (the controller bridge's documented exception to the value-key
convention — the argument is an integer under `freq_hz`):

```json
{ "action": "frequency", "freq_hz": 14175000 }
```

`freq_hz` is an integer in **Hz** (station-wide convention: never kHz/MHz on the bus).
Emission conditions: the resolved target equals the followed resource's port (never
tune a beam that is not in circuit — invariant §15.9) AND the radio is online (§7) AND
`freq_hz > 0`, deduped against the last frequency pushed.

### 8.3 PA band pre-position

Topic: `<pa_follow.slot>/cmd` = `muehle/hf/pa/cmd` — **NOT retained**, QoS 1. Exact
payload:

```json
{ "action": "set_band", "value": "20m" }
```

Emission conditions: the binding is on (`pa_follow.enabled = true`), the radio is
online, and `band` is non-empty. The design is **not gated on antenna selection,
activity state, or TX**. The PA is always in the RF path and its hot-switch
protection is hardware. The point of the binding: the amp otherwise bands itself by
sensing RF. The pre-position makes sure the first TX on a new band does not trip it.
Deduped against the last band pushed. Not-retained is deliberate: self-healing comes
from the reconciler re-resolving on the retained `radio/state` replay at its own
reconnect, not from a retained command.

### 8.4 Tuner in-line follow

Topic: `<tuner_follow.slot>/cmd` = `muehle/hf/tuner/cmd` — **NOT retained**, QoS 1.
Exact payload (note: `value` here is a real JSON boolean, the tuner bridge's
contract):

```json
{ "action": "set_inline", "value": true }
```

Emission conditions: the binding is on, the tuners' resource port has a configured
value, the radio is online, and `band` is non-empty. Value:

- `true` iff the resolved target equals the tuner's resource port **and** the band is
  in the configured `atu_bands` — the non-resonant bands needing the tuner in line.
- `false` otherwise — so leaving a non-resonant band, selecting another antenna, *or
  an idle ground* (target `off` ≠ tuner port) drops the ATU out of the RF path.

Deduped against the last value pushed. A consequence to know: the component computes
the `set_inline` value from the *resolved target*, not from the switch's actual
position. During a TX-deferred window (§9), the ATU intent can therefore appear
before the switch has physically moved. But the ATU never re-keys
mid-transmission, because the select itself defers and the tuner only matters once
the antenna is in circuit.

### 8.5 The Mühle binding values (configuration, not code)

| Binding | Config section | Deployed value |
|---|---|---|
| Beam band-follow resource | `band_follow.resource` | `ultrabeam` (the passive-resource identifier), slot `ant-ctrl`. |
| PA follow | `pa_follow.enabled = true` | slot `pa`. |
| Tuner follow | `tuner_follow.enabled = true` | slot `tuner`, resource `fan-dipole`, `atu_bands = ["30m","60m","80m","160m"]`. |

The tuner `atu_bands` policy: the fan dipole is a wire antenna resonant on 40 m and
80 m only. The station serves 30/60/80/160 m on it non-resonant, so the ATU must sit
in line for exactly `["30m","60m","80m","160m"]`. (Note: the deployed `atu_bands` list
includes `80m` even though the dipole is nominally resonant there — the deployed
configuration is authoritative. See §18.5.)

---

## 9. Cold-switch TX deferral

The antenna switch is a relay device rated `hot_switch: false` — it must never move
its contacts while transmit RF flows through them. The reconciler must therefore
**withhold any port change** while `radio/state.tx == "tx"`:

- The reconciler defers the select and logs once per resolution, with a message
  of this form: `port change to "X" deferred: radio is transmitting (cold-switch)`.
- It fires on the next input change that finds the radio back in `rx`.
- Current behavior has **no timeout** on the deferral (§16.4). A re-implementation
  must add one (see §16).

Hard enforcement of RF safety is *hardware* (a TX-inhibit interlock chain
radio → rx-loop-ctrl → ant-ctrl → pa-arm → pa). The reconciler owns ordering only and
is never part of the enforcement path (§15.8).

---

## 10. Concurrency model and error paths

### 10.1 The no-blocking-handler rule (library-independent, live-incident-derived)

In MQTT client libraries generally (and in the reference Go paho client specifically),
incoming-message handlers run on the connection's dispatch thread. A handler that
blocks — in particular one that publishes synchronously and waits for the broker
acknowledgment — deadlocks the whole client. The station's discovery service hit this
live. This reconciler carried the same pattern and was on track to die on deploy.

**Normative, stack-independent requirements:**

1. An incoming-message handler must do only input parsing and enqueueing. It must
   never block and never wait on a publish.
2. The design must serialize all mutate/reconcile/publish work onto a single decision
   worker (ordering matters to the dedup logic). The simplest form in any stack
   is one queue + one worker.
3. The queue must have a bound (reference capacity: **256** jobs). If it is full, the
   design must **drop** the incoming job — never block the dispatch thread. A drop is
   recoverable because the next message on that topic re-arms the same idempotent
   re-resolution (§16.9 catalogues the cost).
4. A regression test must exist that checks a handler returns without waiting for a
   publish (the reference test: `TestOnRadioStateDefersReconcile`).

### 10.2 Startup sequence

1. Load the config (§13). A validation failure of an explicitly requested or malformed
   config is fatal (exit).
2. Initialize the idle clock (`lastActivity = now`) and the last-seen frequency (0).
3. Start the single-worker queue and the 5 s idle ticker.
4. Connect to the broker. The shutdown signal must be able to cancel the connect
   operation even while the broker is unreachable. In the reference Go client,
   `Connect().Wait()` ignores the context — the shared helper bridges it through a
   goroutine. A different stack must only make sure shutdown is prompt during a
   broker outage.
5. On connect, run the birth sequence (§3.1). The retained replay re-seeds all inputs,
   and the first resolution publishes the first `/state`.
6. A failure to connect at startup is fatal: the process exits and the supervisor
   restarts it (§14).

### 10.3 Broker loss mid-run

The connection auto-reconnects, and the connection-lost path only logs. On reconnect,
the birth sequence re-runs. The clean-session retained replay (§2.1) re-seeds every
input and re-emits follow intents (each dedup marker persists, so only genuinely-new
intents re-emit).

### 10.4 Malformed input

The handler logs malformed JSON on any input topic
(`[mqtt] bad radio/state: …`, `bad ant-switch/state: …`, `bad operator cmd: …` or
similar) and drops the message. The previous inputs stay.

### 10.5 Publish failure

The handler logs a failed publish and does not retry it. Dedup state has already
advanced, so the same re-resolution does not re-emit. Recovery relies on a later
input change or the reconnect retained replay (§16.10).

### 10.6 Clean shutdown (SIGINT/SIGTERM)

The shutdown signal cancels the run context (the connect path is cancellable, §10.2).
The worker stops first. Then, if the broker connection is open, the process publishes
`status = offline` (retained) explicitly and the client disconnects after a short
quiesce (reference: 250 ms).

### 10.7 Switch that never confirms

If `selected` stays unknown, the reconciler emits at most one command per target.
Once the reconciler knows `selected` and it mismatches the target, the command
re-issues on every input change. There is no explicit retry timer — retries are
input-driven.

### 10.8 Subscribe failure

The service logs a failed subscribe (`[mqtt] subscribe failed topic=… err=…` in the
reference, or a line with the same meaning) and continues to run. The service loses
the retained replay for that topic until the next reconnect. On reconnect the birth
sequence re-subscribes (§3.1), and the clean-session retained replay then re-seeds
the topic (§2.1).

---

## 11. Emission dedup summary

The design compares all dedup markers on the single worker:

| Marker | Suppresses | Reset by |
|---|---|---|
| last published decision triple | the same `(mode,target,source)` re-publish | any decision change. |
| `lastSelect` | a repeated `ant-switch/cmd` while the switch position is unknown / already matching | any target change or switch-position mismatch. |
| `lastFollowFreq` | a repeated `frequency` intent | new `freq_hz` while beam selected. |
| `lastPaBand` | a repeated `set_band` | new band while radio online. |
| `lastTunerInline` | a repeated `set_inline` | new computed inline value. |

---

## 12. Timing constants (defaults)

| Constant | Value | Configurable |
|---|---|---|
| Idle check interval | 5 s | no (fixed). |
| Idle grounding timeout | 30 minutes | yes — `idle.timeout_minutes`, must be positive. |
| Jobs queue capacity | 256 | no (fixed in reference). |
| Shutdown quiesce | 250 ms | no. |

---

## 13. Configuration schema

Single TOML file at `/etc/antenna-select/config.toml`, mode **0600** (it holds the
MQTT password), owned by the service user. Precedence: CLI flag > config file >
built-in default. The full config/secrets convention is
[../02-interface-spec.md](../02-interface-spec.md) §config and
[../05-deployment-ops.md](../05-deployment-ops.md). The keys specific to this
component:

| Key | Default | Meaning |
|---|---|---|
| `location` | *(required)* | building label in `/meta` (deploy seed: `bauwagen`). |
| `host` | *(required)* | compute node in `/meta` (deploy seed: `shari`). |
| `mqtt.broker` | *(required in effect)* | broker URL (`tcp://192.168.1.50:1883`, for example). The `-broker` flag overrides. |
| `mqtt.client_id` | `<site>-<station>-<slot>` | MQTT client ID. |
| `mqtt.site` / `mqtt.station` | *(required)* | topic prefix parts (`muehle` / `hf`). |
| `mqtt.slot` | `antenna-select` | this slot's topic segment. |
| `mqtt.user` | `hf` | broker username. |
| `mqtt.password` | *(secret — empty in repo examples)* | broker password — only ever in the 0600 config, never on a command line. |
| `wiring_map` | *(required, no default)* | table of switch port key → resource name. It must include `off = "grounded"`. Port keys are the switch's own names `port1`..`port6`. |
| `band_policy.bands` | *(required, no default)* | resource → list of canonical band names it serves. |
| `band_policy.fallback` | *(required)* | resource for any band not listed (incl. `160m`, `gen`). |
| `band_follow.resource` | `""` (disabled) | wiring-map resource whose controller tracks the radio frequency. |
| `band_follow.slot` | `ant-ctrl` | controller slot receiving `frequency` intents. |
| `pa_follow.enabled` | `false` (deploy seed: `true`) | enable PA `set_band` follow. |
| `pa_follow.slot` | `pa` | PA slot. |
| `tuner_follow.enabled` | `false` (deploy seed: `true`) | enable ATU `set_inline` follow. |
| `tuner_follow.slot` | `tuner` | tuner slot. |
| `tuner_follow.resource` | *(required if enabled)* | wiring-map resource the ATU serves (`fan-dipole`). |
| `tuner_follow.atu_bands` | *(required if enabled)* | non-resonant bands needing the ATU in line (`["30m","60m","80m","160m"]`). |
| `idle.timeout_minutes` | `30` | walk-away grounding timeout, integer minutes, must be positive. |

**Antennas are passive resources, not slots.** The wiring map is the single editable
place where the antenna arrangement lives. Resource names (`ultrabeam`, `fan-dipole`,
`dummy-load`, `grounded`) are free-form site-local identifiers appearing only in
configuration, never in code. The physical antennas have **no MQTT presence at all** —
no topics, nothing to subscribe to. The Ultrabeam is doubly represented: a passive RF
resource on a switch port *and* an active controller slot (`ant-ctrl`) that tunes it.
The `[band_follow]` `resource`+`slot` pair maps between the two so band-follow needs
no antenna names in code.

**Deployed Mühle wiring map and band policy** (see §18.1 for the open port-number
decision):

| Wiring-map key | Resource | Physical antenna |
|---|---|---|
| `port1` | `dummy-load` | dummy load — a heat-dissipating test resistor. It radiates nothing. |
| `port3` **or** `port4` *(OPEN — §18.1)* | `ultrabeam` | Ultrabeam rotatable beam (a directional antenna on a mast), tuned by the `ant-ctrl` slot. |
| `port6` **or** `port2` *(OPEN — §18.1)* | `fan-dipole` | 80/40 m fan dipole (a fixed multi-band wire antenna). |
| `off` | `grounded` | not a port — the switch position shorting all feeds to ground. |

Band policy: `ultrabeam` serves `["6m","10m","12m","15m","17m","20m"]` (that is, the
beam covers 6–20 m). `fan-dipole` serves `["30m","40m","60m","80m"]`.
`fallback = "fan-dipole"` (160 m, `gen`, and anything else unmatched land on the fan
dipole — non-resonant, hence the tuner follow, §8.5).

**Validation** (fatal at startup — every rule is a requirement):

- missing `mqtt.site`/`mqtt.station`, or missing `location`/`host` → reject.
- any band-policy resource (or the fallback) not present in the wiring map → reject.
- missing fallback → reject.
- any band mapped to two resources wired to *different* ports → reject (the design
  allows same-port aliases).
- `band_follow.resource` not in the wiring map, or configured without a slot → reject.
- `pa_follow.enabled` without a slot → reject.
- `tuner_follow.enabled` without slot/resource, or resource not in the wiring map →
  reject.
- non-positive idle timeout → reject.

The resource→port inversion excludes the `off = "grounded"` entry (it is a
switch position, not a routable resource).

**Config-file absence semantics** (station-wide convention): a missing *default-path*
file is tolerable (the service then runs on built-in defaults + flags — this keeps a
local mock workflow working). A missing *explicitly requested* file, or any malformed
file, is fatal.

---

## 14. Deployment

Target host: `shari` (Raspberry Pi, arm64 Linux, `192.168.1.139`, SSH user `io`).
It runs as a dedicated unprivileged system user `antenna-select` (no login, no home),
created on first deploy. The component is pure logic over MQTT — no serial device, no
listening port — so the service unit carries no device permissions.

Reference-implementation notes (non-normative mechanism — the observable contract is):

- **Seed-once:** the deploy tooling generates a seed config (mode 0600) from
  environment variables plus the baked Mühle wiring map/band policy. It transfers it
  to the target and installs it **only if no config exists** — the tooling never
  overwrites an existing file. Delete it to re-seed. After first deploy,
  `/etc/antenna-select/config.toml` on the device is the edit surface. If the seed's
  password is empty, the tooling pulls the shared `hf` MQTT password on-device. It
  reads the first readable of the other services' environment files
  (`/etc/acom1200s-pa-bridge/…`, `/etc/flexbridge/…`, `/etc/hadiscovery/…`,
  `/etc/atr1k-tuner-bridge/…`) and injects it into the installed config. The password
  never leaves the Pi.
- **Supervision contract (normative):** the service must run under a supervisor that
  restarts it on failure (reference: systemd `Restart=on-failure`, `RestartSec=5`).
  The supervisor starts it after network availability
  (`After=`/`Wants=network-online.target`) and runs it as the dedicated user with
  hardening of the same level as `NoNewPrivileges`,
  `ProtectSystem`, `ProtectHome`, `PrivateTmp`. The start command must be exactly
  the binary plus its config path — no secrets on any command line.
- **Executable layout (reference):** binary at `/opt/antennaselect/antennaselect`,
  unit `antenna-select.service`, config `/etc/antenna-select/config.toml`.

Deploy status of the reference implementation: built, unit-tested, deployed to shari.
Live operation showed idle grounding working on 2026-08-28. (The project's own README
lagged behind, still saying "pending deployment" — the code/deploy state wins.)

---

## 15. Invariants (things that must never drift)

1. **Never move the antenna switch while the radio transmits** (cold-switch
   discipline, §9). Hardware enforces RF safety. The reconciler owns
   ordering only.
2. **Never trust retained radio state for safety**: the reconculator acts on
   radio-derived fields only when the LWT says `online` AND `device_online` is true
   (§7). `/status` alone does not give enough *by design* — and a clean shutdown
   leaves retained `status` values that only the process's own explicit `offline`
   corrects.
3. **An empty band holds the last selection** — never resolve `""` to the fallback.
4. **Idle overrides an operator hold** (walk-away safety): station-inactive forces
   `off` even in manual mode. Deliberate, documented, surprising — §18.6.
5. **React to state, emit intent**: the reconciler never assumes a `select` took
   effect. It confirms through `ant-switch/state.selected` and re-asserts on
   mismatch.
6. **`off` is the safe default**: it grounds all antenna feeds (lightning protection),
   and the switch hardware independently fails to ground on power loss.
7. **All decisions serialized on one worker. No message handler can ever block on a
   publish** (§10.1 — deadlock invariant).
8. **The reconciler is never part of the RF enforcement path** — killing it degrades
   the station to manual operation but cannot make it unsafe.
9. **Band-follow can tune a controller only while that controller's antenna is the
   selected target** — never tune a beam that is not in circuit.
10. **PA/tuner commands are fire-and-forget intents, not retained state.** The
    ATU/PA self-heal by the reconciler re-resolving on retained `radio/state` replay
    at reconnect.
11. **`freq_hz` is always an integer in Hz, and the component always derives the band,
    never stores it** — the shared-bus convention this component lives in (see
    [../02-interface-spec.md](../02-interface-spec.md) §bands for the canonical
    band-edge table and the `gen`/`unknown` fallback labels).

---

## 16. Known defects and fragilities (live-observed — catalogued, not fixed)

Verified against the reference code and a 2026-08-28 adversarial live review.
Auto-grounding works live. **Recovery is the fragile half**. A re-implementation must
treat §16.1–§16.5 as fix requirements (fix directions in §6.4) and the rest as
behavior to either reproduce or consciously change.

1. **First key-up after grounding transmits into the grounded switch.** Re-arming
   activity is *itself* a TX. The select the reconciler wants in that same
   resolution stays TX-deferred (§9). So the first transmission after an idle ground
   keys the radio into the grounded/short port. The select fires only at un-key, when
   `tx=="rx"` arrives. (The PA stays disarmed through a separate `antenna_ready`
   chain, so the amplifier does not key — but the radio keys into a short.)
   Structural recovery gap: recovery from ground has no non-TX trigger (see 3).
2. **The radio bridge's change-only publishing starves recovery.** With no
   fresh `radio/state` messages during quiet receive, the reconciler cannot
   re-arm until the operator changes frequency or keys. Also, a *separate* consumer
   — the PA-arm relay controller, which needs a `radio/state` message within its
   10 s heartbeat window — silently drops its arm and cannot re-arm. The reconciler
   cannot fix this alone. The documented fix is a periodic radio-state heartbeat
   (about ≤5 s while the link is live) on the radio bridge.
3. **No manual re-arm.** An operator `/cmd` request does not count as activity.
   While idle-inactive, tier 1 overrides even an operator hold. The only un-ground
   path is a radio frequency change or a TX — that is, transmitting into the short
   (see 1).
4. **TX deferral has no timeout.** A frozen `tx=="tx"` (a radio bridge that dies
   mid-TX leaves stale retained state, for example) freezes *all* actuation — idle
   grounding, operator holds, everything — until a fresh `tx=="rx"` arrives.
5. **Restart always re-arms the station.** The idle clock comes back as "now" and the
   last-seen frequency as 0. So the retained replay at startup always looks like a
   frequency change. A restart during unattended idle *un-grounds* the station and
   resets the walk-away clock for another full 30 minutes.
6. **Ultrabeam port-number discrepancy** — see §18.1.
7. **The component receives `settled` but does not use it.** The switch's `settled`
   (movement complete)
   flag arrives, but no logic gates on it. The docs list a settled-wait handshake as
   backlog. Likewise `radio/state.tuning` is not yet an input.
8. **Operator requests go unvalidated.** Any non-empty, non-`auto` string becomes
   the hold target. The reconciler forwards it verbatim as
   `{"select":"<garbage>"}` and never rejects unknown ports/resources.
9. **Job drops under load.** When the 256-slot queue is full, the design silently
   drops inputs (by design, §10.1). A dropped operator command is invisible until
   the next event on that topic.
10. **Publish failures advance dedup state.** The design does not retry a failed
    intent publish. It retries only after a different resolution or a reconnect
    replay (§10.5).
11. **PA `set_band` ignores station activity** — it fires whenever the radio is
    online and reporting a band, including while idle-grounded. Harmless to RF
    safety (no carrier), but the binding is *not* activity-gated.
12. **Single point of coordination.** If the reconciler dies (and the supervisor
    gives up), band-follow, tuner-follow, PA pre-position and selection all stop.
    The station degrades to manual. The project accepts this only because RF safety
    is hardware. An explicit "reconciler offline" indication beyond the LWT is a
    recommended improvement.

---

## 17. Reference-implementation notes (non-normative)

The current implementation is a Go 1.2x service in three internal packages (`config`,
`reconcile`, `mqtt`). It uses the `paho.mqtt.golang` client and
`pelletier/go-toml/v2`. The reference build uses `GOOS=linux GOARCH=arm64
CGO_ENABLED=0` with `-trimpath -ldflags "-s -w"`, and a project-local `deploy.sh`
deploys it. The design needs none of that. What the design needs is the *behavior*: exact
topics, payload shapes and retention flags (§§3–8). Also required: the ladder and its
precedence (§5), the idle model (§6), the liveness AND gate (§7), the TX deferral
(§9), and the handler-isolation and serialized-worker properties (§10.1). Also
required: clean-session replay semantics (§2.1), the config key names and validation
rules (§13 — so existing on-device config files keep working), and the
seed-once/0600-secrets deployment behavior (§14). Free to change: language, MQTT
library, package layout, and log formatting. Also free: the queue mechanism (provided
the deadlock invariant and input serialization survive some other way), the
idle-ticker mechanism, and the exact RFC 3339 formatting (any equal-precision UTC
timestamp works). Regression tests in the reference suite are worth re-implementing:
the handler non-blocking test, the TX-defer test, the re-assertion-after-manual-
override test, and the same-band-no-command test. Also: the two-layer gate in all
four liveness combinations, plus both retained-replay arrival orders.

---

## 18. Open decisions and unresolved facts

1. **Ultrabeam switch port: `port3` or `port4`? (OPEN — needs on-device
   confirmation.)** The repo disagrees with itself: `antennaselect/config.example.toml`
   and the deploy seed map `ultrabeam` → **`port4`**. But the unit tests, the
   repo-top-level CLAUDE.md, and the integration-model docs say **`port3`**. (The
   console UI's antenna map also says `port4`.) The live
   `/etc/antenna-select/config.toml` on shari is authoritative. It was not readable
   from the workstation when the PRD authors wrote this document. The install seeded
   it as `port4`. Possibly someone hand-edited it. `port1 = dummy-load` is consistent
   everywhere. `port6 = fan-dipole` is consistent everywhere except in one source: the
   integration model's own passive-resource list says `ant/fan-dipole` 80/40
   (port 2) and contradicts every other source. Consequence for a
   re-implementation: the port mapping is pure configuration (§13) — nothing can
   hard-code it. The design must confirm every wiring-map port number against
   `/etc/antenna-select/config.toml` on shari before any acceptance test asserts a
   port, not only the ultrabeam port.
2. **`device_online` field form.** The integration model says the field is "omitted
   when true". The deployed bridges publish `device_online: true` explicitly. This
   component must accept both (absence = true, §7). Whether a re-built bus mandates
   explicit-true or omission-when-true is a system-wide open decision (see
   [../02-interface-spec.md](../02-interface-spec.md)).
3. **Broker topology.** The deployed configuration points at the broker at
   `192.168.1.50:1883`. A migration to a broker on shari (`192.168.1.139`) exists on
   an unmerged feature branch, committed but **not deployed** as of 2026-08-29. This
   document treats `192.168.1.50` as production. The broker URL is configuration
   either way (§13).
4. **Clean-shutdown `/status` caveat (system-wide).** On a clean shutdown the broker
   does not fire the LWT. This component publishes `offline` explicitly, which
   covers it. But retained `/status` can in general go stale. That is one reason
   invariant 2 needs the two-layer AND, not `/status` alone.
5. **Tuner `atu_bands` includes `80m`** even though documents describe the fan dipole
   as resonant on 80 m (and `atu_bands` in the config comment lists
   `"30/60/80/160m … non-resonant"`). The deployed configuration is authoritative.
   Whether 80 m genuinely needs the ATU in line on this antenna is a physical fact
   you cannot check from the repo.
6. **Idle-over-operator surprise.** Tier 1 overriding a stale operator hold
   (§5.1, §15.4) is deliberate but surprising to operators who expect a manual hold
   to stick. A re-implementation must either preserve it (documented walk-away
   safety) or consciously redesign with an explicit presence model — not silently
   change precedence.
7. **Unresolved fix scope for the recovery defects** (§16.1–§16.5). The research
   gives fix *directions*: non-TX re-arm through operator presence, a radio-state
   heartbeat on the radio bridge, a TX-deferral timeout, restart-stable idle state.
   The list also holds operator-request validation and `settled`-gating. But it
   gives no committed design. §6.4 gives the minimum normative requirements that a
   rebuild must include. For the items beyond §6.4, the re-implementation team
   must decide each.
8. **"Reconciler offline" indication** beyond the plain `/status` LWT is a
   recommended-but-unimplemented improvement (§16.12) — no design exists yet.