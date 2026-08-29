# 06 — Safety specification: the never-happen rules

This document consolidates every safety rule of the Mühle amateur-radio station into one
auditable place. **Amateur radio ("ham radio")** is the licensed hobby of two-way radio
communication; this station ("Mühle", site id `muehle`) is an installation of a radio
transceiver, a high-power amplifier, antenna switching and motorized antennas, all
automated over an MQTT message bus (**MQTT**: a lightweight publish/subscribe protocol
where clients exchange messages through a central *broker* on hierarchical string
*topics*; a *retained* message is stored by the broker and re-delivered to every future
subscriber; **LWT**, "last will and testament", is a message the broker publishes on a
client's behalf if that client disappears without a clean disconnect). The station
switches mains power and up to 1200 W of radio-frequency (RF) energy into antennas, so
safety is the highest-priority concern: a wrong ordering can destroy hardware, and a
transmit into a shorted or grounded feed line can destroy the transmitter within seconds.
This document states what must NEVER happen, where each protection physically lives, and
— just as important — what is *not* protected. It is the normative cross-component
reference; per-component detail lives in `03-components/<slug>.md`, the bus wire format in
`02-interface-spec.md`, and the system picture in `00-system-overview.md`.

Terminology used throughout (each also defined at first use in context):

- **Slot**: one component's MQTT address `<site>/<station>/<slot>` (e.g. `muehle/hf/pa`)
  with the four topic suffixes ("planes") `/meta` (identity), `/state` (retained JSON
  snapshot), `/status` (liveness, plain string `online`/`offline`, LWT-backed),
  `/cmd` (command input).
- **Bridge**: the program that translates between one physical device and its slot.
- **TRX / transceiver**: the radio itself (a FLEX-8400).
- **PA (power amplifier)**: boosts the transceiver's transmit signal (an ACOM 1200S, up
  to 1200 W output).
- **ATU / antenna tuner**: an impedance-matching network inserted into the feed line;
  a **tune cycle** makes the radio transmit a carrier while the tuner searches for a
  match — into a deliberately mismatched load.
- **SWR (standing-wave ratio)**: mismatch measure, 1.0 = perfect, ≥ 3.0 dangerous
  (reflected power heats the PA).
- **Band**: a named frequency allocation, e.g. `20m` (the 14 MHz ham band).
- **Relay**: an electromechanical switch; **fail-safe-open** means the relay is
  DE-ENERGIZED in the safe state — the dangerous state requires continuously maintained
  electrical energy, so any power loss, crash, or watchdog failure removes it.
- **Cold-switching**: changing an RF relay only while no transmit power flows.
- **Rotator**: a motor that turns a directional antenna.
- **Reconciler / sequencer**: logic components (no device of their own) that,
  respectively, arbitrate antenna selection and order station power-up/power-down.
- **13.8 V PSU**: the DC supply feeding the control electronics of the radio chain.

---

## 1. Safety philosophy

### 1.1 The prime rule: safety never lives in a message

**No safety decision may depend on the messaging plane.** The MQTT bus *mirrors* state;
it never *enforces* anything. Every protection in this station is realized by one of two
physical mechanisms, and the software layers only arrange ordering around them:

1. **Hardware fail-safes.** The dangerous states all require active, continuously
   maintained energy:
   - The PA arm relay is **fail-safe-open**: it is energized only while a computed
     `armed` condition is true (§2). Loss of software, network, MQTT, or the 13.8 V
     supply removes coil current → relay opens → PA cannot transmit. The PA is keyed by
     a **hardware key line** from the radio, combined by physical series logic with the
     arm relay — the software arm permit only *arms*; it never gates the fast keying
     edge.
   - The antenna switch's `off` position is the safe default: all port relays open, and
     the switch hardware independently returns to open (and the antenna feeds are tied
     to electrical ground — lightning protection) on power loss.
   - The mains smart plugs fail to **off** on power loss (`fail_safe: off`).
2. **Ordered sequencing with confirmation.** Where a wrong *order* would damage
   hardware (powering a PA before its control logic, removing power without stagger),
   the power sequencer enforces the order and waits for explicit liveness/telemetry
   confirmation at each step (§4).

The corollary, which the whole design leans on: **killing any software component
degrades the station to manual operation but cannot make it unsafe.** The antenna
arbitrator (antennaselect) is a coordination single point — its loss stops automated
antenna selection, band-follow and tuner-follow — and this is acceptable *only because*
RF safety is enforced in hardware. A reimplementation that moves any enforcement into
software-only paths violates this document's core requirement:

> **REQ-S1**: The system SHALL enforce every RF-interlock and power-interlock rule in
> hardware or in fail-safe relay logic, such that the failure or absence of any
> software component, network link, or MQTT broker cannot produce an unsafe RF or
> power state.
>
> **REQ-S2**: Every safety-relevant state published on the bus SHALL be treated by
> consumers as *advisory mirror only*. A consumer SHALL NOT assume a command succeeded
> because it was emitted ("plane discipline"): commands are fire-and-observe; consumers
> react to `/state`, never to intent.
>
> **REQ-S3**: Retained bus state SHALL never be trusted as fresh for safety purposes.
> Retained values can be arbitrarily stale after a crash. Consumers must apply the
> two-layer liveness rule (§1.2) before acting on any state.

### 1.2 The two-layer liveness rule (a prerequisite for every consumer)

There are two distinct liveness notions and **both must be AND-combined** before any
state is trusted:

1. `/status` (plain string `online`/`offline`, retained, LWT-backed) — *is the bridge
   process connected to the broker?*
2. `/state.device_online` (boolean inside the state snapshot, device slots) — *is the
   hardware behind the bridge reachable?* A bridge can be up (`/status` online) while
   its serial/WiFi link to the device is dead.

**Actual deployed behavior that must be documented, not idealized**: on a *clean*
process shutdown the broker does **not** fire the LWT, so retained `/status` can stay
`online` indefinitely for a stopped service. A consumer that keys on `/status` alone
will act on dead components. (This exact mistake — keying on `/status` alone — made the
antenna arbitrator flap the antenna selection live when the radio's device link died
while its bridge stayed up; the fix is the AND gate plus an empty-band-holds rule.)

> **REQ-S4**: A consumer SHALL treat a slot as trustworthy only when (a) its `/status`
> is `online` AND (b) for device slots, its `/state.device_online` is true (a state
> snapshot without the key counts as true for logic slots, but absence of any state
> snapshot counts as not-trusted).
>
> **REQ-S5**: A consumer SHALL NOT infer a device is off/absent from a *missing* field;
> unknown blocks action (fail closed) — e.g. a missing antenna-switch `selected` renders
> as "unknown", never as the grounded-safe state.

One form divergence exists in deployed code: the model document says `device_online` is
"omitted when true", but the deployed bridges (radio, ant-ctrl, PA, tuner) publish
`device_online: true` explicitly. Consumers must treat both forms as equivalent
(absence = true for device slots); which form a reimplementation mandates is an open
decision (§8).

### 1.3 The hardware interlock chain (what physically enforces RF safety)

The station's RF enforcement chain, as documented in the integration model — software
mirrors each link as read-only state but is never in this path:

```
13.8 V PSU → M5 Stamp PLCs + antenna switch (boot on supply)
           → radio (remote-on) → PA (remote-on)
radio (TX-low hardware line) → rx-loop-ctrl (preamp off)
           → ant-ctrl (inhibit while moving) → pa-arm relay → PA
```

- The **PA arm relay** (M5 Stamp PLC #1, relay 1) is the final software-driven link:
  its closed contacts are one AND-input of the PA's hardware key circuit.
- The **antenna controller** (Ultrabeam) is inhibited by hardware while its motorized
  elements are moving; the messaging layer mirrors `moving` but the inhibit is not a
  message.
- The PA is keyed by the hardware key line, never over MQTT (`key_input: hardware` in
  `muehle/hf/pa/meta`); there is no `set_keyed` command anywhere.

### 1.4 Messaging-plane robustness constraints (library-independent, incident-derived)

These two constraints are behavior contracts regardless of technology stack; each is
derived from a live incident:

> **REQ-S6 (handler isolation)**: An incoming-message handler SHALL never block — in
> particular it SHALL never synchronously publish and wait for broker acknowledgment on
> the receive/dispatch path. Handlers must only parse and enqueue work onto a bounded
> queue drained by a single worker; on overflow the work is **dropped**, never blocking.
> *Rationale*: in the reference stack, handlers run inline on the MQTT client's dispatch
> thread; a handler that blocks or publishes synchronously deadlocks the whole client —
> this deadlocked the station's discovery consumer live, and the antenna arbitrator had
> the same latent pattern.*
>
> **REQ-S7 (prompt shutdown)**: A shutdown signal (SIGTERM-class) SHALL interrupt every
> blocking wait, *especially* an initial broker connect during a broker outage — no
> implementation may rely on the supervisor's kill-timeout to terminate a hung connect.
> *Rationale*: the reference MQTT library's connect call blocks ignoring cancellation;
> a SIGTERM during a broker outage hung a deployed bridge until the service manager
> SIGKILLed it.*

*Reference-implementation note (non-normative)*: in the current code these are provided
by a shared Go library (`shared/mqtt`: context-aware connect wrapper, bounded jobs
queue). Any mechanism reproducing REQ-S6/REQ-S7 is acceptable.

---

## 2. The PA arm chain (the primary RF interlock)

The PA arm is the single software-visible link in the hardware key chain. It lives in
dedicated firmware on an embedded PLC (**programmable logic controller** — here a small
microcontroller board with relay outputs, "M5 Stamp PLC #1"), which publishes two slots:
`muehle/hf/switch` (PA/TRX remote-on relays) and `muehle/hf/pa-arm` (the arm relay).
The two slots share the same `device{model,serial}` in `/meta` — that shared identity is
the "compound device" tie.

### 2.1 Fail-safe-open wiring

> **REQ-S8**: The arm relay SHALL be wired fail-safe-open: relay **energized** (closed)
> = PA permitted; relay **de-energized** (open) = PA inhibited. The relay shall be
> energized only while the computed `armed` condition (§2.2) is true, re-evaluated at
> least every 50 ms.
>
> **REQ-S9**: On cold boot the firmware SHALL write **all four relays open** before any
> network activity. The arm cannot close on boot until *fresh* radio and antenna-switch
> states have arrived; "never received" counts as stale (unsafe).
>
> **REQ-S10**: Loss of the 13.8 V supply SHALL open the arm by physics (coil
> unpowered) — no software action required. Software does not need to detect PSU loss to
> be safe.
>
> **REQ-S11**: The arm relay SHALL NOT be commandable. The only accepted command on
> `muehle/hf/pa-arm/cmd` is the permit `{"action":"set_enabled","value":"true"|"false"}`
> (retained, value as JSON **string**, per the station `/cmd` convention — argument
> always under the key `value`, booleans as `"true"`/`"false"`). There is no arm, disarm
> or force action; the permit alone can never close the relay.
>
> **REQ-S12**: A firmware update SHALL de-energize the arm relay before rebooting; the
> device then cold-boots with all relays open (REQ-S9) and re-converges from retained
> MQTT state.

### 2.2 The armed formula — all inputs enumerated

The firmware continuously evaluates:

```
armed = enabled ∧ radio_online ∧ ¬radio.tuning ∧ band_safe ∧ heartbeat_fresh ∧ antenna_ready
```

| Input | Source (exact field) | Safe value | What it prevents physically |
|---|---|---|---|
| `enabled` | `muehle/hf/pa-arm/cmd` `set_enabled` (retained permit, replayed by the broker on the PLC's reconnect — self-heal) | `true` | The operator/sequencer permit; nobody has said "PA may transmit". |
| `radio_online` | `muehle/hf/radio/state` field `device_online` (bool; absent → false) | `true` | Amplifying when the radio that must drive and ALC-limit the PA is unreachable — uncontrolled drive. |
| `radio.tuning` | `muehle/hf/radio/state` field `tuning` (bool; absent → false) | `false` | A tune cycle transmits a carrier into a mismatched load; the PA must not amplify it (REQ-S25). |
| `band_safe` | `muehle/hf/radio/state` field `band`; allow-list exactly `160m, 80m, 60m, 40m, 30m, 20m, 17m, 15m, 12m, 10m, 6m` (the ACOM 1200S PA's coverage; empty/unknown → unsafe) | in list | Amplifying on a band the PA cannot handle (damage to its low-pass filters / out-of-spec operation). |
| `heartbeat_fresh` | arrival time of any parseable `muehle/hf/radio/state` message | within **10 000 ms** | Acting on a stale or dead radio feed — the radio state must be *live*, not retained-old. |
| `antenna_ready` | `muehle/hf/ant-switch/state` field `selected` | `selected ∉ {"", "off"}` | Transmitting into the grounded/disconnected feed (destroys the PA output stage); unknown is conservatively not-ready. |

> **REQ-S13**: Any single false input de-energizes the relay within one evaluation cycle
> (≤ ~50 ms). All missing fields default to the *unsafe* side (radio offline /
> not-tuning / band unknown / antenna not-ready / never-received heartbeat = stale).

Published state: `muehle/hf/pa-arm/state` carries `enabled` (the permit), `armed` (the
derived relay state — never commanded), `device_online` (always `true` when published),
and `error` — a string, omitted when empty, from this precedence-ordered set:
`"radio offline"`, `"radio tuning"`, `"band not safe"`, `"antenna grounded"`.

### 2.3 The 10-second heartbeat requirement (normative; live-incident-derived)

The `heartbeat_fresh` input is the rule most often violated in practice, because it is
a *producer* obligation consumed by a *different device*:

> **REQ-S14 (hard requirement)**: The radio-state producer SHALL republish a heartbeat
> `muehle/hf/radio/state` at least every **5 s** while the radio link is live (or
> provide an equivalent liveness signal), such that a healthy-but-idle radio never
> starves a 10 s freshness window. *Rationale (live incident)*: the deployed radio
> bridge publishes `/state` only on content change; a radio sitting quietly on one
> frequency stops refreshing the PLC's 10 s input heartbeat, and the arm silently drops
> despite a healthy radio — and after an automatic antenna grounding (§3.3) the first
> key-up then goes into the shorted feed. Any reimplementation MUST fix producer
> cadence or an equivalent mechanism, and MUST coordinate the 10 s consumer window with
> the producer's actual cadence across every consumer.*

Two distinct 10-second rhythms exist and must not be conflated:

- **Input heartbeat** (`RADIO_HEARTBEAT_MS = 10000`): radio `/state` must have arrived
  within 10 s; on expiry `armed` recomputes false and **the relay opens within the same
  loop pass**.
- **Output heartbeat** (`PA_ARM_HEARTBEAT_MS = 10000`): the pa-arm firmware republishes
  its retained `pa-arm/state` at least every 10 s (change-driven otherwise,
  dedup-suppressed) so downstream freshness logic can detect absence. This rhythm never
  touches the relay.

Known behavior gaps a reimplementation must resolve explicitly (current firmware
behavior in parentheses; the recommended direction is given, and choosing otherwise
must be documented):

- **Arm evaluation during link outage**: (currently the main loop early-returns before
  re-evaluating `armed` when WiFi is down, so the relay *freezes* in its last position
  for the outage duration — a contract violation of "any failure drops the relay").
  The arm evaluation SHALL run regardless of link state; the safe direction is
  dropping the arm, never holding it.
- **Silent arm drop**: (a heartbeat timeout produces NO `error` string because the
  error function does not test freshness, so operators see the arm drop "for no
  reason"). The implementation SHALL publish a distinct error reason for heartbeat
  staleness.
- **`antenna_ready` staleness**: (the antenna-switch input has no freshness window — a
  stale "ready" stays ready forever; and the ant-switch slot's LWT is not consumed
  here, so the arm can close on data from a dead slot whose retained state says an
  antenna is selected). The antenna-ready input SHALL carry a freshness bound and/or
  the `/status` liveness layer.
- **Boolean payload fragility**: (`/cmd` values are extracted as JSON strings; a sender
  publishing `"value": true` — a JSON boolean — yields the empty string, and for
  `set_enabled` that **silently disarms**). The command parser SHALL either accept JSON
  booleans or reject the command loudly; silent disarm on a well-formed-looking payload
  is a defect.

### 2.4 PSU-loss behavior

The 13.8 V PSU feeds the PLC (and the antenna switch). Loss of the supply opens the arm
relay by physics (REQ-S10) and the antenna switch to its safe default (§3.1). After
power returns, both devices cold-boot safe and re-converge from retained MQTT state
(self-heal): the PLC re-applies the retained `set_enabled` permit, but the arm still
cannot close until fresh radio and antenna states arrive. The sequencer's startup
sequence re-establishes the whole chain in order (§4).

---

## 3. Antenna routing safety

Physical background: the station has one coaxial feed line and a 1-of-6 electromechanical
relay antenna switch (only one antenna connected at a time). Physical antennas are
**passive resources** with no MQTT presence — they exist only in the antenna arbitrator's
wiring-map configuration (port 1 = dummy load — a heat-dissipating, non-radiating test
resistor; the Ultrabeam rotatable beam; port 6 = the 80/40 m fan dipole wire antenna;
the `off` position grounds the antenna feeds — lightning protection). The antenna
arbitrator (slot `muehle/hf/antenna-select`, a stateless reconciler) decides *which*
port; the switch (slot `muehle/hf/ant-switch`) dumbly moves; the arm chain (§2) is the
backstop if RF flows anyway.

### 3.1 Cold-switch rule (no port change under RF)

The switch declares `hot_switch: false` in its `/meta` capabilities: its relays are not
rated for breaking RF current. The switch itself has **no RF sensing and must not
invent TX gating** — it executes any valid command immediately. Sequencing is the
commanders' job:

> **REQ-S15**: The antenna arbitrator SHALL withhold any port change while the radio
> reports transmit (`muehle/hf/radio/state` `tx == "tx"`), log the deferral once
> (`port change to "X" deferred: radio is transmitting (cold-switch)`), and emit the
> select when a later radio state reports `rx`.
>
> **REQ-S16**: The arbitrator SHALL be excluded from the RF enforcement path: killing it
> degrades the station to manual antenna selection, never to an unsafe state.
>
> **REQ-S17**: The switch SHALL enforce exclusivity by construction: a port change turns
> ALL port relays off first, then exactly the target relay on — there is never an
> instant with two antennas connected (which would put transmitter power into an
> unexpected path). Note the corollary: every change passes through a moment of
> **zero** relays on (break-before-make) — no antenna and no ground connection
> mid-change — which is acceptable *only* because hot-switching is forbidden anyway.

### 3.2 The settled guard

The switch reports `settled` (boolean) in its `/state`: `false` from the moment a relay
move is commanded until a conservative worst-case mechanical travel time —
**200 ms** (configurable `relay_settle_ms`, default 200) — has elapsed since the *most
recent* commanded change; `settled: true` only after. The design rule is never
optimistic:

> **REQ-S18**: The switch SHALL hold `settled: false` for at least the relay's
> worst-case travel time after the latest change (timer restart semantics), and never
> publish `settled: true` before that. `selected` SHALL be read back from relay state,
> never echoed from the last received command.
>
> **REQ-S19 (known gap, must be closed)**: Consumers SHALL gate RF resumption on
> `settled`. The deployed arbitrator parses `settled` but does not yet act on it — the
> settled-wait handshake is documented backlog. A reimplementation must consume it.

`settled` is a *timed* guard, not a contact feedback signal — see §7.1 for the honest
limits of that.

### 3.3 Grounding when idle / off (walk-away lightning protection)

The arbitrator's decision ladder has three tiers with fixed precedence —
**idle > operator > auto**:

```
Tier 1  idle:     station activity == inactive → target = "off", source = "idle"
Tier 2  operator: operator hold active           → target = request, source = "operator"
Tier 3  auto:     band policy(radio.band)        → target = port,  source = "auto"
```

> **REQ-S20 (idle grounding)**: After a configurable timeout — default **30 minutes**
> (`idle.timeout_minutes`, must be positive) — with no radio activity, the arbitrator
> SHALL select `off` (grounded). Activity is *inferred*: a `radio/state` message whose
> `freq_hz` differs from the last seen value, or whose `tx == "tx"`, resets the idle
> clock; the check runs every **5 s**, so grounding lands within ~5 s after expiry.
> An operator command does NOT count as activity.
>
> **REQ-S21 (idle overrides the operator)**: Tier 1 wins over everything, including an
> active operator hold — a forgotten dummy-load hold must not keep an antenna hanging
> live in an unattended station. This is deliberate, documented behavior (and
> surprising to operators — surface it in the console UI).
>
> **REQ-S22**: On the other side, loss of radio *liveness* SHALL NOT re-ground the
> station by itself: if either liveness layer drops after a selection, the arbitrator
> holds the last selection (anti-chatter; also holds for the empty band `""` — never
> resolve an empty band to the fallback, it is the bridge's "reconnecting" transient).
>
> **REQ-S23 (PSU-off / station-off)**: When the 13.8 V PSU is off, the whole HF control
> chain (switch PLC, arm PLC, antenna switch) loses power and every fail-safe opens: arm
> relay open, antenna switch relays open (`off`). Nothing in software needs to act.

The **first-key-up-after-ground fragility** — a live-observed structural defect with a
required fix direction:

- Re-arming activity from grounded-idle requires a *new* radio state with a *changed*
  `freq_hz` or `tx == "tx"` (REQ-S20). But the re-arming event can itself be the
  transmission — and REQ-S15 then *defers* the port select until un-key. Result: **the
  first key-up after an idle ground transmits into the grounded/short switch position.**
  The PA stays disarmed via the independent `antenna_ready` chain (§2.2), so the
  amplifier does not key — but the FLEX radio keys into a short.
- The recovery is additionally starved by the change-only radio publishing (§2.3): on a
  quiet receive period no fresh state arrives, so the station cannot re-arm until the
  operator actually changes frequency or keys.
- There is **no manual re-arm**: while idle-inactive, an operator hold is overridden
  (REQ-S21), so an operator at the desk who never touches the dial cannot un-ground the
  station except by transmitting into the short.

> **REQ-S24 (required fix)**: A reimplementation SHALL provide a non-transmit re-arm
> path — at minimum, treating an operator antenna-select command as activity ("operator
> presence"), and/or gating the first post-ground transmit on the switch confirming a
> valid port with `settled == true` (REQ-S19). The heartbeat fix (REQ-S14) is the other
> half of the same recovery problem. Additionally, the deferred-for-TX deferral needs a
> bound (currently a stale frozen `tx == "tx"` — e.g. a crashed radio bridge leaving
> retained `tx` — freezes *all* actuation, including grounding, indefinitely), and
> restart-stability of the idle clock must be handled (currently a reconciler restart
> always re-arms the station as "active" for another full 30 minutes because the
> retained state replay looks like a frequency change — the inverse failure: an
> unattended station silently un-grounds).

---

## 4. Power sequencing

The sequencer (slot `muehle/hf/power-seq`) is a logic component that turns the whole
station on/off with one operator command (`{"action":"start"}` / `{"action":"stop"}` on
its `/cmd`), in a fixed order with settle delays and explicit liveness/telemetry
confirmations. Wrong ordering physically damages hardware — energizing a PA before its
control logic is up, or removing power in ways that slam inductive loads with inrush
current.

The step lists are **configuration** (a TOML file on the deployment host), not code; the
values below are the shipped defaults and are normative as the reference sequence.

### 4.1 Startup order and the physical rationale of each constraint

| # | Step | Action | Why this position (physical rationale) |
|---|---|---|---|
| 1 | `master-on` | `muehle/power/master/cmd` `{"action":"set_power","value":"on"}` (retained) | Station master mains gates everything downstream; first because nothing else has power yet. |
| 2 | `network-delay` | delay, default **30 s** | After master mains returns, the broker's network and the WiFi of the plug-in devices must come up before anything can be confirmed. |
| 3 | `psu-on` | `muehle/power/psu-13v8/cmd` `set_power on` | The 13.8 V supply feeds the *control electronics* (both PLCs, the antenna switch). Control must be alive before the radio chain is energized. |
| 4 | `wait-controllers-online` | wait until `/status` of `muehle/hf/switch`, `muehle/hf/pa-arm`, `muehle/hf/ant-switch` are all `online` (deadline default 120 s) | Proof the 13.8 V-fed controllers actually booted — never energize the radio/PA chain on assumption. |
| 5 | `trx-on` | `muehle/hf/switch/cmd` `{"action":"set_trx","value":"on"}` (retained) | Radio remote-on only after its control plane exists. |
| 6 | `wait-radio-online` | wait `muehle/hf/radio/status == online` (120 s) | The radio must be up *before* the PA is enabled — the PA is driven and ALC-limited by the radio. |
| 7 | `pa-on` | `muehle/hf/switch/cmd` `{"action":"set_pa","value":"on"}` (retained) | PA remote-on only after the radio chain is confirmed. |
| 8 | `wait-pa-power-on` | wait `muehle/hf/pa/state` field `power == "on"`, **with implicit precondition** `muehle/hf/pa/status == online` | The PA bridge's LWT being online prevents a *dead* PA satisfying the wait on a stale retained state — the two-layer rule applied to sequencing. |
| 9 | `pa-arm-enable` | `muehle/hf/pa-arm/cmd` `{"action":"set_enabled","value":"true"}` (retained) | **Arm is always LAST**: the transmit permit is granted only after the entire chain upstream is confirmed. |

> **REQ-S25**: Startup order SHALL be exactly: master mains → network delay → PSU →
> controller-liveness wait → TRX power → radio-online wait → PA enable → PA-powered
> wait → arm permit. PA arming SHALL always be the last startup action; the arm chain
> (§2) may still refuse to close if its own safety inputs are not met.
>
> **REQ-S26**: A `wait_state` step SHALL never pass unless the target's `/status` is
> *currently* `online` (stale retained state cannot satisfy a wait); a `wait_status`
> for `offline` SHALL require an actual `offline` payload — absence is never proof.
> Wait mechanics: poll every 200 ms; per-step deadline default 120 s (overridable > 0);
> optional continuous-hold debounce window; the condition is evaluated against the
> cached retained snapshot (see §7.5 for the freshness gap in the payload itself).

### 4.2 Shutdown order and inrush staggers

Shutdown is the exact reverse, **disarm first**, with a **2-second** stagger
(`shutdown_stagger_s` default 2) between each power removal to stagger the electrical
**inrush current** (the surge when switching inductive loads) so the mains circuit is
never hit by simultaneous surges:

`pa-arm set_enabled false` → stagger 2 s → `set_pa off` → 2 s → `set_trx off` → 2 s →
`psu-13v8 off` → 2 s → `master off`.

> **REQ-S27**: Shutdown SHALL be the exact reverse of startup with the stagger delays;
> PA disarm SHALL always be the first shutdown action. The default shutdown list has
> **no waits** — shutdown must make progress even if devices are already dead.
>
> **REQ-S28 (no spurious energization)**: The sequencer's own `/cmd` SHALL NOT be
> retained, and the sequencer SHALL subscribe it such that a command issued while the
> sequencer is down is simply lost (never broker-queued for replay — a replayed `start`
> would re-energize the station with no operator present). On process boot the
> sequencer SHALL emit no commands until an explicit operator `start` arrives, even if
> the whole station is already hot. Controlled-slot commands (power, relays, arm
> permit) ARE retained — idempotent steady-state intent that each device re-applies on
> its own reconnect (self-heal).
>
> **REQ-S29 (busy guard)**: `start` SHALL be honored only from phase `idle`; `stop`
> only from `running`, or from `idle` *with a fault* (teardown of a half-completed
> startup — the shutdown list has no waits, so it always completes). Commands arriving
> mid-sequence are dropped; there is no abort command. Re-running `start` over a hot
> station is safe: every step is idempotent steady-state intent and satisfied waits
> pass on first poll.
>
> **REQ-S30 (no rollback on fault)**: On any step failure the sequencer SHALL go to
> `idle` with `fault = "<step>: <reason>"` (reasons exactly: `timeout`,
> `broker disconnected`, `publish failed: <error>`, `interrupted`) and SHALL NOT roll
> back — driven slots keep their last retained intent. Recovery is an explicit re-run
> (`start` clears the fault; `stop` from `idle+fault` tears down). This is deliberate:
> an automatic rollback of power steps can itself cause the inrush/damage patterns the
> staggers exist to prevent. (Honest consequence in §7.5.)

---

## 5. TX-safety interlocks

### 5.1 Tune cycles require the PA disarmed

An ATU tune cycle (memory recall `mem` or full search `full`) works by transmitting a
carrier into a mismatched load while relays search — the PA must never amplify it.
There is no hardware tune line, so ordering is routed through automation:

> **REQ-S31**: A tune SHALL be issued to the radio only after the PA arm is disarmed
> and that disarm is *confirmed* (via `pa-arm/state.armed == false`). Operators publish
> tune requests through the automation path (sequencer), never directly.
>
> **REQ-S32**: Independently and always, the arm chain SHALL drop the arm whenever the
> radio reports `tuning: true` (§2.2) — so even a mistuned process order cannot amplify
> a tune carrier.
>
> **REQ-S33**: The tuner's `/cmd` SHALL NOT be retained, and the tuner bridge SHALL
> never re-issue a stale tune after a restart (a replayed tune can re-key the tuner
> while the transmitter is on). Tune is one-shot; a pending tune that does not settle
> within **12 s** SHALL flag `fault: "tune timeout"` and clear `settling`; a tuner-link
> loss SHALL abort a pending tune (never leave `settling: true` while unreachable).
>
> **REQ-S34**: The console's tuner panel SHALL lock the tune buttons while
> `settling == true` (a second queued tune competes with the in-flight one) while
> leaving BYPASS available.

### 5.2 Amplifier into dummy-load / bad-load prevention

The damage modes addressed:

- **Transmit into the grounded/off port** (effectively a short): prevented by the arm
  chain's `antenna_ready` input — `selected ∉ {"", "off"}` (§2.2, error
  `"antenna grounded"`), by the console's grounded-state presentation (the GROUNDED
  button renders solid red even when active — grounded means no antenna, operating
  impossible, must shout), and by REQ-S15/S17 ordering.
- **Transmit on a band the PA cannot handle**: prevented by `band_safe` (§2.2) and by
  the reconciler's band policy (160 m, which has no resonant antenna here, routes to
  the fan dipole via the ATU — a high-SWR path the tuner must handle).
- **Amplified drive into the dummy load** is *not* unsafe (a dummy load is a safe,
  non-radiating load) — an operator hold may deliberately select it for testing; that
  hold is overridden by idle grounding (REQ-S21) on walk-away.
- **PA hot-switching its internal band filters**: the reconciler's PA band pre-position
  (`pa/cmd {"action":"set_band","value":"<band>"}`, NOT retained, NOT gated on antenna
  selection or TX) is a soft convenience path — the ACOM's own hardware protects its
  relays (it faults `HOT SWITCHING ATTEMPT` / `STOP TRANSMISSION FIRST`); the software
  does not and need not gate it. The amp auto-bands by RF sensing (`band_source:
  rf_sense`); the software `set_band` pre-positions so the amp does not trip on the
  first TX of a new band. The amp's `set_band` walk is rejected when its current band
  is unknown.
- **SWR-level protection**: the ACOM's hardware faults on excessive SWR/reflected
  power (`PA LOAD SWR TOO HIGH`, `EXCESSIVE REFLECTED POWER`, …) — mirrored to the bus
  in `pa/state.fault` (buckets `none|swr|temp|reflected|other`) with the verbatim text
  in `error`. The console renders SWR ≥ 3.0 red, ≥ 2.0 amber (a 3.5:1 and a 1.1:1 must
  not read alike). The bridge hard-zeroes `fwd_power_w`/`rfl_power_w` whenever the amp
  is not keyed, so displays can never show stale transmit power while receiving.
- **The bridge never commands PA power**: the PA bridge has no `set_power`; PA mains is
  owned exclusively by the `switch` slot's remote-on relay. Any `set_power` command to
  the PA slot SHALL be rejected.

### 5.3 The console's fail-closed cross-panel guard

The operator console (see `04-console.md`) is a pure commander — but it owns one
operator-facing RF guard, driven by *other slots'* state:

> **REQ-S35**: A direct-drive antenna-switch port change (published when the reconciler
> is in manual mode or absent: `muehle/hf/ant-switch/cmd {"select":"<port>"}`) SHALL be
> blocked unless RF is confirmed safe: radio link up AND `radio.state.tx == "rx"` AND
> `radio.state.tuning != true` AND `pa.state.keyed != "tx"`. **Unknown blocks** (fail
> closed) — a relay moving RF would arc. Only the reconciler path
> (`antenna-select/cmd {"request":"<port>"}`, reconciler online and in auto mode) is
> exempt, because the reconciler arbitrates RF-inhibit ordering itself (REQ-S15).
>
> **REQ-S36**: The console SHALL disable every command button whose owning slot fails
> two-layer liveness (§1.2), and while the MQTT link is down it SHALL show a full-width
> banner — every on-screen value is stale retained state and every tap is silently
> undelivered (`LINK DOWN — DATA STALE · COMMANDS NOT DELIVERED`).
>
> **REQ-S37**: Presentation fail-closed rules: a missing antenna-switch `selected`
> renders as Unknown — never as `off`/Grounded (which would paint a dead bridge as a
> deliberate safe state); a missing PA relay state renders `RELAY ?` — never asserted
> as OFF; a live TX (red `● TX`) outranks relay bookkeeping in the PA status tag; the
> sequencer START button is disabled while phase is `running` or `starting`.
>
> **REQ-S38**: The 6 m Ultrabeam guard (device physics: the motorized elements support
> only the forward pattern on 6 m): on 6 m, reverse/bi-directional buttons SHALL be
> disabled and the console SHALL auto-publish `direction=forward` once per invalid
> state (latched, moving-locked, re-fires after travel if still invalid). RETRACT
> SHALL remain pressable while elements are moving (designated emergency action).

---

## 6. UHF rotator safety: manual-only arming

The UHF station's pan/tilt rotator (a mast-mounted camera/antenna head on an RS-485
industrial control bus, driven by the Pelco-D/P protocol) is deliberately NOT remote-
controllable:

> **REQ-S39**: The UHF rotator's motion authority SHALL NOT be reachable over the bus.
> Its slot (`muehle/uhf/rotator`) exposes exactly one bus command: `stop`. Arming the
> drive (enabling motion at all) is a manual keyboard act inside an interactive
> terminal UI running on the shack PC — never remote, never automated.
>
> **REQ-S40**: The UHF rotator console SHALL NOT run as an always-on daemon/service.
> A headless, auto-started process with motion authority would contradict the safety
> model; the binary is started interactively by the operator. (Deployment hardening
> otherwise follows the station convention: dedicated user, 0600 seed-once config.)

Related UHF gap: the slot table attributes the X-Quad polarization control
(`muehle/uhf/pol-ctrl`, "PLC #2") to the PLC firmware project, but **no PLC #2 firmware
exists in the repository** — the polarization slot has no implementation and therefore
no safety behavior to specify (§8).

---

## 7. What is NOT protected (honest gaps)

This section is normative honesty: a reimplementation must know which hazards the
current design does not cover. Items marked "gap" are accepted limitations to either
reproduce knowingly or fix consciously.

### 7.1 Antenna switch: no contact feedback, no fault reporting

- **No per-port contact feedback.** The relay driver (an I²C GPIO expander) drives coils
  only; no contact-closure input is wired. `selected` reports the *driven coil state*,
  not verified closure — a failed or jammed relay is invisible, and `settled` goes
  `true` 200 ms after the command regardless. (Gap; fix requires hardware.)
- **No fault-reporting path.** The switch firmware has no hardware-unreachable state
  and publishes no error field; expander bus failures surface only in device logs.
  Invalid `/cmd` payloads are **silently ignored** — no log, no bus response; a
  commander gets no feedback that its select was rejected. (Gap.)
- **`selected` is best-effort readback** (coil state, REQ-S18's readback-of-relay-state
  is the strongest available, but it is not contact verification).
- **`ts` on `/state` depends on a Home Assistant time source** — with HA down the
  timestamp is wrong (unsynced epoch). The board's hardware RTC is read but never used
  for `ts`. (Fragility; a reimplementation should use NTP or the RTC.)
- **Security posture** (embedded device): no OTA password, an unauthenticated web
  server on port 80, a committed native-API encryption key, and broker credentials
  embedded in the compiled firmware image (recoverable from a physically taken device).
  (Gap; accepted on a trusted LAN.)

### 7.2 Arm-chain readback and evaluation gaps

- **Fail-safe readback claimed but unimplemented**: the PLC firmware's relay readback
  carries a comment "returns false on any read failure (fail-safe: treat as open)" but
  the implementation is a bare library call with no failure detection. (Gap — the
  *state* published can lie about the relay; the *relay itself* still fails safe by
  wiring.)
- **Arm frozen during WiFi outage** (§2.3) — the only window where "any failure drops
  the relay" is not enforced.
- **Silent arm drop on heartbeat timeout** (§2.3) and **`antenna_ready` without
  staleness** (§2.3) — the arm can hold or drop on data from a dead slot.
- **Silent disarm on JSON-boolean `value`** (§2.3).
- `device_online` in both PLC slots is hardcoded `true` — the slot-level two-layer
  convention degenerates to LWT-only here; consumers must know.

### 7.3 The port-number open decision (ultrabeam port 3 vs port 4)

The repo disagrees with itself on which physical switch port carries the Ultrabeam
beam: the integration model, root docs and the arbitrator's tests say **port 3**; the
arbitrator's example config and deploy seed — and the console's antenna map — say
**port 4**. The live on-device config on the deployment host is authoritative but was
**not readable** when this PRD was written. Port 1 (dummy load) and port 6 (fan
dipole) are consistent everywhere. Consequence for safety: the wiring map is pure
configuration and must never be hardcoded, and the port truth must be confirmed
on-device before commissioning (§8). A wrong map sends RF to the wrong antenna — the
hardware exclusivity (REQ-S17) and arm chain still hold, but the band policy and
band-follow would tune the wrong antenna.

### 7.4 Reconciler fragilities that touch safety

- **Deferred-for-TX has no timeout** (§3.3) — a frozen `tx == "tx"` freezes all
  actuation, including grounding.
- **Restart un-grounds the station** (§3.3) — retained-replay looks like activity.
- **Operator requests unvalidated** — any non-empty, non-`auto` string becomes the hold
  target and is forwarded verbatim as `{"select":"<garbage>"}` (the switch then
  silently ignores it, §7.1 — so the garbage goes nowhere, but the reconciler state
  claims a manual target that does not exist).
- **Publish failures advance dedup** — a failed select emit is not retried until a
  different resolution or a reconnect replay; recovery relies on input-driven
  re-assertion against the switch's actual `selected`.
- **Job drops under load** — the bounded queue protects REQ-S6 by dropping; a dropped
  operator command is invisible until the next event on that topic.

### 7.5 Sequencer gaps

- **No rollback and no abort** (REQ-S30 is deliberate, but its consequence is real): a
  faulted half-startup leaves slots energized until an operator re-runs `start` or
  issues `stop` from `idle+fault`; there is no way to cancel an in-progress sequence
  (worst case ~30 s + 3 × 120 s with default timeouts).
- **`stop` from `idle` without a fault is dropped** — a station started *by hand*
  (relays toggled directly) cannot be shut down through the sequencer.
- **`wait_state` freshness gap**: the liveness precondition checks current `/status`,
  but the waited `/state` snapshot itself has no freshness bound — a device whose
  bridge is online per LWT but whose device link died *without* a `/status` change can
  satisfy a wait on a stale `power == "on"`. The two-layer rule is not enforced on the
  wait payload. (Gap; fix direction: couple the wait to `device_online`.)
- **Deploy seed omits the step lists**: the generated first-deploy config has no
  `[[startup]]`/`[[shutdown]]` sections, which validation requires — a fresh deploy
  crash-loops until an operator hand-adds them. (Deploy defect; ship the full seed.)

### 7.6 Bus-level gaps affecting safety consumers

- **Retained `/status` stays `online` after a clean shutdown** (no will on graceful
  disconnect) — a stopped service looks alive to `/status`-only consumers (§1.2).
- **Change-only producers**: besides the radio-heartbeat incident (REQ-S14), the
  antenna switch and PLC `switch` slot publish state only on change — a quiet bus
  makes retained snapshots age without any staleness signal.
- **Tuner `inline` is client-side belief** — the ATR-1000 protocol has no inbound
  in-line/bypass confirmation; `/state.inline` can be wrong after a front-panel toggle
  or link loss. (Mirror-only field; do not build interlocks on it.)
- **The reconciler is a single point of coordination** — accepted *only* under REQ-S1
  (safety is hardware); an explicit "reconciler offline" operator indication is an open
  wish (currently only its LWT).

---

## 8. Open decisions & unresolved facts

Each item lists the evidence for every variant; no resolution is silently invented.

1. **Ultrabeam switch port: 3 or 4?** Variant A (port 3): repo-root CLAUDE.md,
   `docs/station-integration-model.md` §7.1, antennaselect unit tests. Variant B
   (port 4): `antennaselect/config.example.toml`, `antennaselect/deploy.sh` seed, and
   the console's antenna port map (which renders port 4 as "Ultrabeam" and omits
   port 3 as unwired). The deployed `/etc/antenna-select/config.toml` on shari is
   authoritative and was not readable from the workstation when this PRD was written.
   Every document touching the wiring must present this as requiring on-device
   confirmation. Port 1 (dummy load) and port 6 (fan dipole) are consistent.
2. **Radio heartbeat mechanism** (REQ-S14): periodic `/state` republish while the link
   is live (research suggests ~5 s, chosen as < half the 10 s consumer window), vs
   enlarging the consumer window, vs. a dedicated heartbeat topic. Whichever is chosen
   must be coordinated with *every* consumer of the 10 s freshness figure (the arm PLC,
   the sequencer's freshness logic, console staleness displays). Current code has NO
   producer heartbeat — the live starvation incident is the evidence this must change.
3. **Error string on heartbeat timeout**: current firmware drops the arm silently
   (no `error` field — the error function does not test freshness). REQ in §2.3 says
   publish a reason; the exact string (`"radio state stale"`, `"heartbeat timeout"`, …)
   is undecided. Related: whether a stale `antenna_ready` should produce an error and
   whether the arm should consume the ant-switch `/status` LWT.
4. **Arm evaluation during WiFi outage**: current code freezes the relay (§7.2); the
   recommendation (evaluate always, drop is the safe direction) is a deliberate
   contract change that a reimplementation must make explicitly.
5. **`/cmd` boolean handling**: accept JSON booleans or reject loudly — current
   `set_enabled` silently disarms on `"value": true` (§2.3). The station-wide
   convention says booleans ride as strings; the console complies
   (`{"action":"set_enabled","value":"true"}`).
6. **`device_online` form**: model says "omitted when true"; deployed bridges publish
   explicit `true`. Mandate one form or specify consumer-side equivalence (the PRD
   requires consumers to treat absence as true either way).
7. **Broker topology**: deployed code points at `192.168.1.50:1883` (the "shack broker
   migration" to `192.168.1.139` exists on an unmerged feature branch — the PLC
   firmware's checked-in local secrets already say `.139`). Safety-relevant because
   every embedded fail-safe node's MQTT reachability depends on the broker it is
   compiled/configured against; the migration is a commissioning decision point.
8. **Non-TX re-arm path for post-ground recovery** (REQ-S24): operator-command-counts-
   as-activity vs settled-gated first TX vs both. Current design has neither; the
   first-key-up-into-the-short incident is the evidence.
9. **TX-deferral timeout value** for the reconciler's cold-switch deferral
   (none exists today), and **idle-clock restart stability** (a reconciler restart
   currently un-grounds and re-arms the station — is restart-time re-grounding or
   persisted idle state wanted?).
10. **The physical detail of `off`**: station docs say the `off` position "shorts the
    antenna feeds to ground" (lightning protection); the switch firmware realizes
    `off` simply as *all relays open* and its own documentation describes the feed line
    as "unconnected". Whether grounding is a property of the switch hardware's
    unused-port wiring or of an external grounding element is not determinable from
    the repo — verify against the physical installation. The safe-default behavior is
    identical either way.
11. **PLC #2 / `muehle/uhf/pol-ctrl`**: the slot table assigns the X-Quad polarization
    relays to "m5stamp (PLC #2)", but no such firmware exists in the repo (no second
    build environment, no pol-ctrl topics). Either the slot table is aspirational or
    the firmware lives elsewhere; no polarization safety behavior can be specified.
12. **Sequencer `stop`-from-any-phase / abort command**: current drop behavior is
    documented intent; adding an abort is a deliberate contract change (§7.5). Also
    undecided: a freshness bound / `device_online` coupling for `wait_state`.
13. **PA band-walk during transmit**: the ACOM bridge does not prevent `set_band`
    while the amp is transmitting (the amp's own hardware may refuse with
    `STOP TRANSMISSION FIRST`, leaving its band position uncertain until the next
    telemetry frame). Whether software should TX-gate `set_band` is open.
14. **Console START button during phase `stopping`**: the deployed UI enables START
    while the shutdown sequence is still in flight (a `start` sent mid-stop is dropped
    by the sequencer's busy guard, so no hazard materializes — but the UI does not
    guard it). Preserve knowingly or fix.
15. **Logging layer** (`docs/logging-integration-model.md`, log/event + spots/event
    planes) is specified but implemented by no component — future scope, no safety
    claims. Host-liveness nodes (`muehle/host/shari`, `muehle/host/shack-pc`) appear in
    the model but no bridge publishes them — model-only.