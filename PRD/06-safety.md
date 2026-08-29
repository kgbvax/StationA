# 06 — Safety specification: the never-happen rules

This document consolidates every safety rule of the Mühle amateur-radio station into one auditable place. **Amateur radio ("ham radio")** is the licensed hobby of two-way radio communication. This station ("Mühle", site id `muehle`) is an installation of a radio transceiver, a high-power amplifier, antenna switching, and motorized antennas. The MQTT message bus (**MQTT**: a lightweight publish/subscribe protocol) automates it all. Clients exchange messages through a central *broker* on hierarchical string *topics*. A *retained* message is one that the broker stores and re-delivers to every future subscriber. **LWT**, "last will and testament", is a message the broker publishes on a client's behalf if that client disappears without a clean disconnect. The station switches mains power and up to 1200 W of radio-frequency (RF) energy into antennas. Safety is therefore the highest-priority concern. A wrong ordering can destroy hardware. A transmit into a shorted or grounded feed line can destroy the transmitter within seconds. This document states what must NEVER happen. It states where each protection physically lives. It also states — just as important — what is *not* protected. It is the normative cross-component reference. Per-component detail lives in `03-components/<slug>.md`. The bus wire format is in `02-interface-spec.md`. The system picture is in `00-system-overview.md`.

Terminology used throughout (each term also has its definition at first use in context):

- **Slot**: one component's MQTT address `<site>/<station>/<slot>` (for example `muehle/hf/pa`). It has the four topic suffixes (\"planes\") `/meta` (identity), `/state` (retained JSON snapshot), `/status` (liveness, plain string `online`/`offline`, LWT-backed), and `/cmd` (command input).
- **Bridge**: the program that translates between one physical device and its slot.
- **TRX / transceiver**: the radio itself (a FLEX-8400).
- **PA (power amplifier)**: boosts the transceiver's transmit signal (an ACOM 1200S, up to 1200 W output).
- **ATU / antenna tuner**: an impedance-matching network inserted into the feed line. A **tune cycle** makes the radio transmit a carrier while the tuner searches for a match — into a deliberately mismatched load.
- **SWR (standing-wave ratio)**: mismatch measure, 1.0 = perfect, ≥ 3.0 dangerous (reflected power heats the PA).
- **Band**: a named frequency allocation, for example `20m` (the 14 MHz ham band).
- **Relay**: an electromechanical switch. **Fail-safe-open** means the relay is DE-ENERGIZED in the safe state — the dangerous state needs continuously maintained electrical energy. Any power loss, crash, or watchdog failure then removes it.
- **Cold-switching**: changing an RF relay only while no transmit power flows.
- **Rotator**: a motor that turns a directional antenna.
- **Reconciler / sequencer**: logic components (no device of their own). The reconciler arbitrates antenna selection. The sequencer orders station power-up and power-down.
- **13.8 V PSU**: the DC supply that feeds the devices of the radio chain. The model's `feeds` capability lists the full scope: `hf/radio`, `uhf/radio`, `hf/tuner`, `hf/ant-ctrl`, `hf/ant-switch`, `hf/rotator`, `hf/switch`, `hf/pa-arm`.
- **Keyed / keying / key-up**: switching a transmitter or amplifier into actual RF output. A **key line** is the control signal that does this. A *hardware* key line is a physical wire that carries it.
- **ALC (automatic level control)**: a feedback signal from the PA back to the radio. It limits the radio's drive power so the PA stays in its safe range.
- **Preamp (preamplifier)**: a device that boosts weak received signals before the receiver. The **receive-loop controller** (`rx-loop-ctrl`) drives a receive-only loop antenna. The radio's transmit signal hardware-disables its preamp.
- **Shack**: the station room (ham jargon). The **shack PC** is the PC at the operator's desk.

---

## 1. Safety philosophy

### 1.1 The prime rule: safety never lives in a message

**Safety decisions must never depend on the messaging plane.** The MQTT bus *mirrors* state. It never *enforces* anything. Two physical mechanisms realize every protection in this station. The software layers only arrange ordering around them:

1. **Hardware fail-safes**. The dangerous states all need active, continuously maintained energy:
   - The PA arm relay is **fail-safe-open**. The firmware energizes it only while a computed `armed` condition is true (§2). Loss of software, network, MQTT, or the 13.8 V supply removes coil current → relay opens → PA cannot transmit. A **hardware key line** from the radio keys the PA. Physical series logic combines that line with the arm relay. The software arm enable only *arms*. It never gates the fast keying edge.
   - The antenna switch's `off` position is the safe default. All port relays open. On power loss, the switch hardware independently returns to open. The feed line is then unconnected — the station documents the `off` position as grounded for lightning protection (see §8, open decision 10, for the physical detail).
   - The mains smart plugs fail to **off** on power loss (`fail_safe: off`).
2. **Ordered sequencing with confirmation**. Where a wrong *order* can damage hardware — powering a PA before its control logic, or removing power without stagger — the power sequencer enforces the order. It waits for explicit liveness/telemetry confirmation at each step (§4).

The corollary, which the whole design leans on: **killing any software component degrades the station to manual operation, but it cannot make it unsafe**. The antenna arbitrator (antennaselect) is a coordination single point. Its loss stops automated antenna selection, band-follow, and tuner-follow. This is acceptable *only because* hardware enforces RF safety. A reimplementation that moves any enforcement into software-only paths violates this document's core requirement:

> **REQ-S1**: The system must enforce every RF-interlock and power-interlock rule in hardware or in fail-safe relay logic. The failure or absence of any software component, network link, or MQTT broker must then be unable to produce an unsafe RF or power state.
>
> **REQ-S2**: Consumers must treat every safety-relevant state published on the bus as *advisory mirror only*. A consumer must not assume a command succeeded because the code emitted it (\"plane discipline\"). Commands are fire-and-observe. Consumers react to `/state`, never to intent.
>
> **REQ-S3**: Consumers must never trust retained bus state as fresh for safety purposes. Retained values can be arbitrarily stale after a crash. Consumers must apply the two-layer liveness rule (§1.2) before they act on any state.

### 1.2 The two-layer liveness rule (a prerequisite for every consumer)

Two distinct liveness notions exist, and **consumers must AND-combine both** before they trust any state:

1. `/status` (plain string `online`/`offline`, retained, LWT-backed) — the question: does the bridge process have a connection to the broker?
2. `/state.device_online` (boolean inside the state snapshot, device slots) — the question: can the bridge reach the hardware behind it? A bridge can be up (`/status` online) while its serial/WiFi link to the device is dead.

**Actual deployed behavior that we must document, not idealize**: on a *clean* process shutdown the broker does **not** fire the LWT. Retained `/status` can stay `online` indefinitely for a stopped service. A consumer that keys on `/status` alone acts on dead components. This exact mistake — keying on `/status` alone — made the antenna arbitrator flap the selection live when the radio's device link died while its bridge stayed up. The fix is the AND gate plus an empty-band-holds rule.

> **REQ-S4**: A consumer must treat a slot as trustworthy only when (a) its `/status` is `online` AND (b) for device slots, its `/state.device_online` is true. A state snapshot without the key counts as true for logic slots and for embedded slots where firmware and device are one unit (`muehle/hf/ant-switch` is the deployed example — its state never carries `device_online`). But absence of any state snapshot counts as not-trusted.
>
> **REQ-S5**: A consumer must not infer a device is off/absent from a *missing* field. Unknown blocks action (fail closed) — for example, a missing antenna-switch `selected` renders as \"unknown\", never as the grounded-safe state.

One form divergence exists in deployed code: the model document says `device_online` is \"omitted when true\", but the deployed bridges (radio, ant-ctrl, PA, tuner) publish `device_online: true` explicitly. Consumers must treat both forms as the same (absence = true for device slots). Which form a reimplementation mandates is an open decision (§8).

### 1.3 The hardware interlock chain (what physically enforces RF safety)

The station's RF enforcement chain, as documented in the integration model — software mirrors each link as read-only state but is never in this path:

```
13.8 V PSU → M5 Stamp PLCs + antenna switch (boot on supply)
           → radio (remote-on) → PA (remote-on)
radio (TX-low hardware line) → rx-loop-ctrl (preamp off)
           → ant-ctrl (inhibit while moving) → pa-arm relay → PA
```

- The **PA arm relay** (M5 Stamp PLC #1, relay 1) is the final software-driven link. Its closed contacts are one AND-input of the PA's hardware key circuit.
- Hardware inhibits the **antenna controller** (Ultrabeam) while its motorized elements move. The messaging layer mirrors `moving`, but the inhibit is not a message.
- The hardware key line keys the PA, never MQTT (`key_input: hardware` in `muehle/hf/pa/meta`). There is no `set_keyed` command anywhere.

### 1.4 Messaging-plane robustness constraints (library-independent, incident-derived)

These two constraints are behavior contracts regardless of technology stack. Each comes from a live incident:

> **REQ-S6 (handler isolation)**: An incoming-message handler must never block — in particular it must never synchronously publish and wait for broker acknowledgment on the receive/dispatch path. Handlers must only parse and enqueue work onto a bounded queue drained by a single worker. On overflow the work is **dropped**, never blocking. *Rationale*: in the reference stack, handlers run inline on the MQTT client's dispatch thread. A handler that blocks or publishes synchronously deadlocks the whole client. This deadlocked the station's discovery consumer live, and the antenna arbitrator had the same latent pattern.
>
> **REQ-S7 (prompt shutdown)**: A shutdown signal (SIGTERM-class) must interrupt every blocking wait — *especially* a broker connect at startup during a broker outage. An implementation must not rely on the supervisor's kill-timeout to end a hung connect. *Rationale*: the reference MQTT library's connect call blocks ignoring cancellation. A SIGTERM during a broker outage hung a deployed bridge until the service manager SIGKILLed it.

*Reference-implementation note (non-normative)*: in the current code, a shared Go library (`shared/mqtt`: context-aware connect wrapper, bounded jobs queue) provides these. Any mechanism that reproduces REQ-S6/REQ-S7 is acceptable.

---

## 2. The PA arm chain (the primary RF interlock)

The PA arm is the single software-visible link in the hardware key chain. It lives in dedicated firmware on an embedded PLC (**programmable logic controller** — here a small microcontroller board with relay outputs, \"M5 Stamp PLC #1\"). That firmware publishes two slots: `muehle/hf/switch` (PA/TRX remote-on relays) and `muehle/hf/pa-arm` (the arm relay). The two slots share the same `device{model,serial}` in `/meta` — that shared identity is the \"compound device\" tie.

### 2.1 Fail-safe-open wiring

> **REQ-S8**: The wiring must make the arm relay fail-safe-open. Relay **energized** (closed) = PA allowed. Relay **de-energized** (open) = PA inhibited. The firmware must energize the relay only while the computed `armed` condition (§2.2) is true. It must re-compute that condition at least every 50 ms.
>
> **REQ-S9**: On cold boot the firmware must write **all four relays open** before any network activity. The arm cannot close on boot until *fresh* radio and antenna-switch states have arrived. \"Never received\" counts as stale (unsafe).
>
> **REQ-S10**: Loss of the 13.8 V supply must open the arm by physics (coil unpowered) — no software action needed. Software does not need to see PSU loss to stay safe.
>
> **REQ-S11**: The arm relay must not be commandable. The only accepted command on `muehle/hf/pa-arm/cmd` is the enable `{"action":"set_enabled","value":"true"|"false"}` (retained, value as JSON **string**, per the station `/cmd` convention — the argument always sits under the key `value`, booleans as `"true"`/`"false"`). There is no arm, disarm, or force action. The enable alone can never close the relay. The arm relay also has **no local override**: no physical control on the device can force it closed. This is deliberate.
>
> **REQ-S11a (local controls on the same PLC — known behavior)**: The front-panel buttons B and C of the PLC directly toggle the PA and TRX remote-on relays. They work with WiFi down. A local toggle made during an MQTT outage is silently reverted on reconnect, because the broker replays the retained `/cmd` intent. A rebuild must reproduce this revert behavior or must consciously change it (for example, re-publish the local intent after the replay). Either way, the rebuild must document the choice. The arm relay stays outside this: no button can force it (REQ-S11).
>
> **REQ-S12**: A firmware update must de-energize the arm relay before rebooting. The device then cold-boots with all relays open (REQ-S9) and re-converges from retained MQTT state.

### 2.2 The armed formula — all inputs enumerated

The firmware computes continuously:

```
armed = enabled ∧ radio_online ∧ ¬radio.tuning ∧ band_safe ∧ heartbeat_fresh ∧ antenna_ready
```

| Input | Source (exact field) | Safe value | What it stops physically |
|---|---|---|---|
| `enabled` | `muehle/hf/pa-arm/cmd` `set_enabled` (retained enable, replayed by the broker on the PLC's reconnect — self-heal). | `true` | The operator/sequencer enable. Nobody has said \"PA can transmit\". |
| `radio_online` | `muehle/hf/radio/state` field `device_online` (bool. **Absent → false** — fail-closed). This is the deliberate safety-consumer exception to the general consumer rule (`02-interface-spec.md` §5.2, rule R5.2a). The arm chain reads an absent key as unsafe, not as true. | `true` | Amplifying when the radio that must drive and ALC-limit the PA is unreachable — uncontrolled drive. |
| `radio.tuning` | `muehle/hf/radio/state` field `tuning` (bool. Absent → false). | `false` | A tune cycle transmits a carrier into a mismatched load. The PA must not amplify it (REQ-S25). |
| `band_safe` | `muehle/hf/radio/state` field `band`, allow-list exactly `160m, 80m, 60m, 40m, 30m, 20m, 17m, 15m, 12m, 10m, 6m` (the PLC firmware's `SAFE_BANDS`, 11 bands. Empty/unknown → unsafe). | in list | Amplifying on a band the PA cannot handle (damage to its low-pass filters / out-of-spec operation). The ACOM 1200S covers only 10 bands and has no `60m` filter. The allow-list still contains `60m`, so the arm can close on a band the PA does not cover (§8, open decision). |
| `heartbeat_fresh` | arrival time of any parseable `muehle/hf/radio/state` message. | within **10 000 ms** | Acting on a stale or dead radio feed — the radio state must stay *live*, not retained-old. |
| `antenna_ready` | `muehle/hf/ant-switch/state` field `selected`. | `selected ∉ {"", "off"}` | Transmitting into the grounded/disconnected feed (destroys the PA output stage). Unknown is conservatively not-ready. |

The arm chain does **not** subscribe to `muehle/hf/radio/status`. Instead, the
heartbeat backstop covers the bridge-alive layer (REQ-S4). A dead bridge stops
publishing `/state`. The 10 s `heartbeat_fresh` expiry (§2.3) then drops the arm
within one loop pass. The backstop also covers the clean-shutdown case where the
LWT does not fire and `/status` stays `online` (§1.2). For this consumer,
freshness gives the second liveness layer — not a `/status` subscription.

> **REQ-S13**: Any single false input de-energizes the relay within one evaluation cycle (≤ ~50 ms). Missing fields default per input: an absent `device_online`, an absent band, a never-received heartbeat, and an absent `selected` all count as unsafe. One input defaults the other way: a missing `tuning` field counts as `false` (not tuning), the permissive side. This is a known fail-open gap: a radio state snapshot without the `tuning` key lets the arm close during a real tune cycle. §8 lists the fix as an open decision.

Published state: `muehle/hf/pa-arm/state` carries `enabled` (the enable), `armed` (the derived relay state — never commanded), `device_online` (always `true` when published), and `error`. The `error` string, omitted when empty, comes from this precedence-ordered set: `"radio offline"`, `"radio tuning"`, `"band not safe"`, `"antenna grounded"`.

### 2.3 The 10-second heartbeat requirement (normative — derived from a live incident)

The `heartbeat_fresh` input is the rule most often violated in practice. It is a *producer* obligation consumed by a *different device*:

> **REQ-S14 (hard requirement)**: The radio-state producer must republish a heartbeat `muehle/hf/radio/state` at least every **5 s** while the radio link is live. Acceptance criterion: a healthy-but-idle radio (no frequency change, no transmit) must never starve a 10 s freshness window. Any substitute mechanism (a dedicated heartbeat topic, a wider consumer window) must meet the same measurable criterion. See §8 for the open variant discussion. *Rationale (live incident)*: the deployed radio bridge publishes `/state` only on content change. A radio sitting quietly on one frequency stops refreshing the PLC's 10 s input heartbeat. The arm then silently drops despite a healthy radio. After an automatic antenna grounding (§3.3), the first key-up then goes into the shorted feed. Any reimplementation MUST fix producer cadence or add a matching mechanism. It MUST coordinate the 10 s consumer window with the producer's actual cadence across every consumer.

Two distinct 10-second rhythms exist. Never conflate them:

- **Input heartbeat** (`RADIO_HEARTBEAT_MS = 10000`): radio `/state` must have arrived within 10 s. On expiry, `armed` recomputes false and **the relay opens within the same loop pass**.
- **Output heartbeat** (`PA_ARM_HEARTBEAT_MS = 10000`): the pa-arm firmware republishes its retained `pa-arm/state` at least every 10 s (change-driven otherwise, dedup-suppressed). Downstream freshness logic can then detect absence. This rhythm never touches the relay. The `ts` field in the PLC slots' `/state` is device uptime in milliseconds (a `millis()` counter), never wall-clock time. Consumers must measure freshness from message arrival time, never by comparing `ts` to the wall clock.

A reimplementation must resolve these known behavior gaps explicitly. The parentheses give current firmware behavior. The text gives the recommended direction. Choosing otherwise needs documentation:

- **Arm evaluation during link outage**: the main loop now early-returns before re-evaluating `armed` when WiFi is down. The relay *freezes* in its last position for the outage duration — a contract violation of \"any failure drops the relay\". The arm evaluation must run regardless of link state. The safe direction drops the arm. Never hold the arm.
- **Silent arm drop**: a heartbeat timeout produces NO `error` string. The error function does not test freshness, so operators see the arm drop \"for no reason\". The implementation must publish a distinct error reason for heartbeat staleness.
- **`antenna_ready` staleness**: the antenna-switch input has no freshness window, so a stale \"ready\" stays ready forever. The ant-switch slot's LWT is not used here either. The arm can therefore close on data from a dead slot whose retained state shows a selected antenna. The antenna-ready input must carry a freshness bound and/or the `/status` liveness layer.
- **Boolean payload fragility**: the parser extracts `/cmd` values as JSON strings. A sender publishing `"value": true` — a JSON boolean — yields the empty string. For `set_enabled` that **silently disarms**. The command parser must either accept JSON booleans or reject the command loudly. Silent disarm on a well-formed-looking payload is a defect.

### 2.4 PSU-loss behavior

The 13.8 V PSU feeds the whole radio chain: both PLCs, the antenna switch, the radio, the tuner, the antenna controller, and the rotator (§1 gives the full list). Loss of the supply opens the arm relay by physics (REQ-S10) and the antenna switch to its safe default (§3.1). After power returns, both devices cold-boot safe and re-converge from retained MQTT state (self-heal). The PLC re-applies the retained `set_enabled` enable. But the arm still cannot close until fresh radio and antenna states arrive. The sequencer's startup sequence re-establishes the whole chain in order (§4).

---

## 3. Antenna routing safety

Physical background: the station has one coaxial feed line and a 1-of-6 electromechanical relay antenna switch (only one antenna connected at a time). Physical antennas are **passive resources** with no MQTT presence. They exist only in the antenna arbitrator's wiring-map configuration (port 1 = dummy load, a heat-dissipating non-radiating test resistor. The Ultrabeam rotatable beam — sources contest its port, 3 or 4 (§7.3). The 80/40 m fan dipole wire antenna — sources likewise contest its port, 6 or 2 (§7.3). The `off` position opens all relays and leaves the feed line unconnected — the station documents `off` as grounded for lightning protection, see §8). The antenna arbitrator (slot `muehle/hf/antenna-select`, a stateless reconciler) decides *which* port. The switch (slot `muehle/hf/ant-switch`) dumbly moves. The arm chain (§2) is the backstop if RF flows anyway.

### 3.1 Cold-switch rule (no port change under RF)

The switch declares `hot_switch: false` in its `/meta` capabilities. Its relays have no rating for breaking RF current. The switch itself has **no RF sensing and must not invent TX gating** — it executes any valid command immediately. Sequencing is the commanders' job:

> **REQ-S15**: The antenna arbitrator must withhold any port change while the radio reports transmit (`muehle/hf/radio/state` `tx == "tx"`). It must log the deferral once (`port change to "X" deferred: radio is transmitting (cold-switch)`). It must emit the select when a later radio state reports `rx`.
>
> **REQ-S16**: The arbitrator must stay out of the RF enforcement path. Killing it degrades the station to manual antenna selection, never to an unsafe state.
>
> **REQ-S17**: The switch must enforce exclusivity by construction. A port change first turns ALL port relays off. It then turns on exactly the target relay. There is never an instant with two antennas connected (that can put transmitter power into an unexpected path). Note the corollary: every change passes through a moment of **zero** relays on (break-before-make) — no antenna and no ground connection mid-change. This is acceptable *only* because hot-switching is forbidden anyway.
>
> **REQ-S17a (tuner-follow gap — known)**: The reconciler's tuner-follow command (`muehle/hf/tuner/cmd` `{"action":"set_inline","value":<bool>}`) is also an RF-path relay change. It moves the ATU's internal relays. The deployed reconciler does not gate this command on transmit. Its emit conditions are: tuner-follow enabled, a configured tuner resource, radio online, and a non-empty band. A `set_inline` can therefore go out during a TX-deferred port change (REQ-S15) — the ATU reconfigures while the radio transmits and before the switch moves. A reimplementation must gate `set_inline` on transmit together with the port select, or must accept and document this gap. The arm chain (`radio.tuning`, §2.2) stays the backstop.

### 3.2 The settled guard

The switch reports `settled` (boolean) in its `/state`. The value stays `false` from the moment a command orders a relay move. A conservative worst-case mechanical travel time — **200 ms** (configurable `relay_settle_ms`, default 200) — must elapse since the *most recent* commanded change. Only then does the switch publish `settled: true`. The design rule is never optimistic:

> **REQ-S18**: The switch must hold `settled: false` for at least the relay's worst-case travel time after the latest change (timer restart semantics). It must never publish `settled: true` before that. the firmware must read `selected` back from relay state and must never echo the last received command.
>
> **REQ-S19 (known gap — a reimplementation must close it)**: Consumers must gate RF resumption on `settled`. The deployed arbitrator parses `settled` but does not yet act on it — the settled-wait handshake sits in documented backlog. A reimplementation must use it.

`settled` is a *timed* guard, not a contact feedback signal — see §7.1 for the honest limits of that.

### 3.3 Grounding when idle / off (walk-away lightning protection)

The arbitrator's decision ladder has three tiers with fixed precedence — **idle > operator > auto**:

```
Tier 1  idle:     station activity == inactive → target = "off", source = "idle"
Tier 2  operator: operator hold active           → target = request, source = "operator"
Tier 3  auto:     band policy(radio.band)        → target = port,  source = "auto"
```

> **REQ-S20 (idle grounding)**: After a configurable timeout — default **30 minutes** (`idle.timeout_minutes`, must be positive) — with no radio activity, the arbitrator must select `off` (grounded). Activity is *inferred*: a `radio/state` message whose `freq_hz` differs from the last seen value, or whose `tx == "tx"`, resets the idle clock. The check runs every **5 s**, so grounding lands within ~5 s after expiry. An operator command does NOT count as activity.
>
> **REQ-S21 (idle overrides the operator)**: Tier 1 wins over everything. It wins even over an active operator hold — a forgotten dummy-load hold must not keep an antenna hanging live in an unattended station. This is deliberate, documented behavior (and it surprises operators — surface it in the console UI).
>
> **REQ-S22**: On the other side, loss of radio *liveness* must not re-ground the station by itself. If either liveness layer drops after a selection, the arbitrator holds the last selection in the **auto** path (anti-chatter). Deployed behavior differs for tier 2: an active operator hold keeps asserting its held target (select emitted) while the radio is offline. A rebuild must preserve or change that manual-mode behavior deliberately. The arbitrator also holds for the empty band `\"\"` — never resolve an empty band to the fallback. It is the bridge's \"reconnecting\" transient.
>
> **REQ-S23 (PSU-off / station-off)**: When the 13.8 V PSU is off, the whole radio chain loses power (both PLCs, the antenna switch, the radio, the tuner, the antenna controller, the rotator), and every fail-safe opens. Arm relay open. Antenna switch relays open (`off`). Nothing in software needs to act.

The **first-key-up-after-ground fragility** — a live-observed structural defect with a required fix direction:

- Re-arming activity from grounded-idle needs a *new* radio state with a *changed* `freq_hz` or `tx == "tx"` (REQ-S20). But the re-arming event can itself be the transmission. REQ-S15 then *defers* the port select until un-key. Result: **the first key-up after an idle ground transmits into the grounded/short switch position**. The PA stays disarmed through the independent `antenna_ready` chain (§2.2), so the amplifier does not key — but the FLEX radio keys into a short.
- The change-only radio publishing (§2.3) additionally starves the recovery: on a quiet receive period no fresh state arrives. The station cannot re-arm until the operator actually changes frequency or keys.
- There is **no manual re-arm**: while idle-inactive, an operator hold is overridden (REQ-S21). An operator at the desk who never touches the dial can un-ground the station only by transmitting into the short.

> **REQ-S24 (required fix)**: A reimplementation must give a non-transmit re-arm path — at minimum, treating an operator antenna-select command as activity (\"operator presence\"). The reimplementation can also gate the first post-ground transmit on the switch confirming a valid port with `settled == true` (REQ-S19). The heartbeat fix (REQ-S14) is the other half of the same recovery problem. The deferred-for-TX deferral also needs a bound. A stale frozen `tx == "tx"` — for example a crashed radio bridge leaving retained `tx` — freezes *all* actuation, including grounding, indefinitely. The idle clock must also handle restart stability. Now a reconciler restart always re-arms the station as \"active\" for another full 30 minutes, because the retained state replay looks like a frequency change. This is the inverse failure: an unattended station silently un-grounds.

---

## 4. Power sequencing

The sequencer (slot `muehle/hf/power-seq`) is a logic component. It turns the whole station on/off with one operator command (`{"action":"start"}` / `{"action":"stop"}` on its `/cmd`), in a fixed order with settle delays. It adds explicit liveness and telemetry confirmations to every step. Wrong ordering physically damages hardware. Some failures energize a PA before its control logic is up. Other failures remove power in ways that slam inductive loads with inrush current.

The step lists are **deployment configuration**, not code. The reference implementation stores them in a TOML file on the deployment host. Any declarative format with the same key names and defaults is acceptable. The key names, defaults, and validation rules are normative, so operators' on-device config files keep working across a rebuild. The values below are the shipped defaults and are normative as the reference sequence.

### 4.1 Startup order and the physical rationale of each constraint

| # | Step | Action | Why this position (physical rationale) |
|---|---|---|---|
| 1 | `master-on` | `muehle/power/master/cmd` `{"action":"set_power","value":"on"}` (retained). | Station master mains gates everything downstream. It is first because nothing else has power yet. |
| 2 | `network-delay` | delay, default **30 s**. | After master mains returns, the broker's network and the WiFi of the plug-in devices must come up before anything can get a confirmation. |
| 3 | `psu-on` | `muehle/power/psu-13v8/cmd` `set_power on`. | The 13.8 V supply feeds the devices of the radio chain: both PLCs, the antenna switch, the radio, the tuner, the antenna controller, the rotator. Control must come alive before anything energizes the radio chain. |
| 4 | `wait-controllers-online` | wait until `/status` of `muehle/hf/switch`, `muehle/hf/pa-arm`, `muehle/hf/ant-switch` are all `online` (deadline default 120 s). | Proof that the 13.8 V-fed controllers actually booted — never energize the radio/PA chain on assumption. |
| 5 | `trx-on` | `muehle/hf/switch/cmd` `{"action":"set_trx","value":"on"}` (retained). | Radio remote-on only after its control plane exists. |
| 6 | `wait-radio-online` | wait `muehle/hf/radio/status == online` (120 s). | The radio must be up *before* anyone enables the PA — the radio drives and ALC-limits the PA. |
| 7 | `pa-on` | `muehle/hf/switch/cmd` `{"action":"set_pa","value":"on"}` (retained). | PA remote-on only after the wait confirms the radio chain. |
| 8 | `wait-pa-power-on` | wait `muehle/hf/pa/state` field `power == "on"`, **with implicit precondition** `muehle/hf/pa/status == online`. | The PA bridge's LWT staying online stops a *dead* PA from satisfying the wait on a stale retained state — the two-layer rule applied to sequencing. |
| 9 | `pa-arm-enable` | `muehle/hf/pa-arm/cmd` `{"action":"set_enabled","value":"true"}` (retained). | **Arm is always LAST**: the transmit enable comes only after the whole upstream chain gets confirmation. |

> **REQ-S25**: The startup order must be exactly: master mains → network delay → PSU → controller-liveness wait → TRX power → radio-online wait → PA enable → PA-powered wait → arm enable. PA arming must always be the last startup action. The arm chain (§2) can still refuse to close if its own safety inputs are not met.
>
> **REQ-S26**: A `wait_state` step must never pass unless the target's `/status` is `online` at that moment (stale retained state cannot satisfy a wait). A `wait_status` for `offline` must see an actual `offline` payload — absence is never proof. Wait mechanics: poll every 200 ms. The per-step deadline default is 120 s (settable to more than 0). A continuous-hold debounce window can be present. The system checks the condition against the cached retained snapshot (see §7.5 for the freshness gap in the payload itself).

### 4.2 Shutdown order and inrush staggers

Shutdown is the exact reverse, **disarm first**, with a **2-second** stagger (`shutdown_stagger_s` default 2) between each power removal. That staggers the electrical **inrush current** (the surge when switching inductive loads). The mains circuit then never meets simultaneous surges:

`pa-arm set_enabled false` → stagger 2 s → `set_pa off` → 2 s → `set_trx off` → 2 s →
`psu-13v8 off` → 2 s → `master off`.

> **REQ-S27**: Shutdown must be the exact reverse of startup with the stagger delays. PA disarm must always be the first shutdown action. The default shutdown list has **no waits** — shutdown must make progress even if devices are already dead.
>
> **REQ-S28 (no spurious energization)**: The sequencer's own `/cmd` must stay non-retained. The sequencer must subscribe it such that a command issued while the sequencer is down is simply lost (never broker-queued for replay — a replayed `start` can re-energize the station with no operator present). On process boot the sequencer must emit no commands until an explicit operator `start` arrives, even if the whole station is already hot. Controlled-slot commands (power, relays, arm enable) ARE retained — idempotent steady-state intent that each device re-applies on its own reconnect (self-heal).
>
> **REQ-S29 (busy guard)**: The sequencer honors `start` only from phase `idle`. It honors `stop` only from `running`, or from `idle` *with a fault* (teardown of a half-completed startup — the shutdown list has no waits, so it always completes). The sequencer drops commands that arrive mid-sequence. There is no cancel command. Re-running `start` over a hot station is safe. Every step is idempotent steady-state intent, and satisfied waits pass on first poll.
>
> **REQ-S30 (no rollback on fault)**: On any step failure the sequencer must go to `idle` with `fault = "<step>: <reason>"` (reasons exactly: `timeout`, `broker disconnected`, `publish failed: <error>`, `interrupted`) and must not roll back — driven slots keep their last retained intent. Recovery is an explicit re-run. `start` clears the fault, and `stop` from `idle+fault` tears down. This is deliberate: an automatic rollback of power steps can itself cause the inrush/damage patterns that the staggers exist to stop. §7.5 gives the honest consequence.

---

## 5. TX-safety interlocks

### 5.1 Tune cycles need the PA disarmed

An ATU tune cycle (memory recall `mem` or full search `full`) works by transmitting a carrier into a mismatched load while relays search — the PA must never amplify it. There is no hardware tune line, so automation routes the ordering:

> **REQ-S31**: This rule governs the radio-carrier tune command (the command that makes the radio transmit the tune carrier). Such a tune must go to the radio only after the arm state reads disarmed and the code confirms that disarm (through `pa-arm/state.armed == false`). The automation route (sequencer) is the designed contract. It is not deployed behavior: the shipped sequencer step lists have no tune step. Honest gap (deployed behavior): the operator console's TUNE MEM / TUNE FULL buttons publish the ATU's own tune command (`{"action":"tune",...}`) directly to `muehle/hf/tuner/cmd`, gated only by `settling` (REQ-S34). That path has no disarm-confirmation step. Its protections are the independent ones: REQ-S32 (the arm chain drops on `radio.tuning`), REQ-S33 (one-shot, 12 s timeout), and REQ-S34 (button lock while `settling`).
>
> **REQ-S32 (independent and always)**: The arm chain must drop the arm whenever the radio reports `tuning: true` (§2.2). Even a mistuned process order cannot then amplify a tune carrier.
>
> **REQ-S33**: The tuner's `/cmd` must stay non-retained. The tuner bridge must never re-issue a stale tune after a restart (a replayed tune can re-key the tuner while the transmitter is on). Tune is one-shot. A pending tune that does not settle within **12 s** must flag `fault: "tune timeout"` and clear `settling`. A tuner-link loss must stop a pending tune (never leave `settling: true` while unreachable).
>
> **REQ-S34**: The console's tuner panel must lock the tune buttons while `settling == true` (a second queued tune competes with the in-flight one). It must leave BYPASS available.

### 5.2 Amplifier into dummy-load / bad-load stop mechanisms

The damage modes addressed:

- **Transmit into the grounded/off port** (effectively a short): the arm chain's `antenna_ready` input stops this — `selected ∉ {"", "off"}` (§2.2, error `"antenna grounded"`). The console's grounded-state presentation also stops it (the GROUNDED button renders solid red even when active — grounded means no antenna, operating impossible, must shout). REQ-S15/S17 ordering also stops it.
- **Transmit on a band the PA cannot handle**: `band_safe` (§2.2) stops this. The reconciler's band policy also stops it (160 m, which has no resonant antenna here, routes to the fan dipole through the ATU — a high-SWR path the tuner must handle).
- **Amplified drive into the dummy load** is *not* unsafe (a dummy load is a safe, non-radiating load) — an operator hold can select it deliberately for testing. Idle grounding (REQ-S21) overrides that hold on walk-away.
- **PA hot-switching its internal band filters**: the reconciler's PA band pre-position (`pa/cmd {"action":"set_band","value":"<band>"}`, NOT retained, NOT gated on antenna selection or TX) is a soft convenience path. The ACOM's own hardware protects its relays (it faults `HOT SWITCHING ATTEMPT` / `STOP TRANSMISSION FIRST`). The software does not and need not gate it. The amp auto-bands by RF sensing (`band_source: rf_sense`). The software `set_band` pre-positions so the amp does not trip on the first TX of a new band. The amp rejects the `set_band` walk when its current band is unknown.
- **SWR-level protection**: the ACOM's hardware faults on SWR/reflected power too high (`PA LOAD SWR TOO HIGH`, `EXCESSIVE REFLECTED POWER`, …) — mirrored to the bus in `pa/state.fault` (buckets `none|swr|temp|reflected|other`) with the verbatim text in `error`. The console renders SWR ≥ 3.0 red, ≥ 2.0 amber (a 3.5:1 and a 1.1:1 must not read alike). The bridge hard-zeroes `fwd_power_w`/`rfl_power_w` whenever the amp is not keyed. Displays can then never show stale transmit power while receiving.
- **The bridge never commands PA power**: the PA bridge has no `set_power`. The `switch` slot's remote-on relay exclusively owns PA mains. The PA slot must reject any `set_power` command.

### 5.3 The console's fail-closed cross-panel guard

The operator console (see `04-console.md`) is a pure commander — but it owns one operator-facing RF guard, driven by *other slots'* state:

> **REQ-S35**: A direct-drive antenna-switch port change (published when the reconciler is in manual mode or absent: `muehle/hf/ant-switch/cmd {"select":"<port>"}`) must stay blocked unless the conditions below confirm RF safe: radio link up AND `radio.state.tx == "rx"` AND `radio.state.tuning != true` AND `pa.state.keyed != "tx"`. **Unknown blocks** (fail closed) — a relay moving RF arcs. Only the reconciler path (`antenna-select/cmd {"request":"<port>"}`, reconciler online and in auto mode) is exempt. The reconciler arbitrates RF-inhibit ordering itself (REQ-S15).
>
> **REQ-S36**: The console must disable every command button whose owning slot fails two-layer liveness (§1.2). While the MQTT link is down, it must show a full-width banner — every on-screen value is stale retained state and every tap is silently undelivered (`LINK DOWN — DATA STALE · COMMANDS NOT DELIVERED`).
>
> **REQ-S37**: Presentation fail-closed rules: a missing antenna-switch `selected` renders as Unknown — never as `off`/Grounded (that rendering falsely marks a dead bridge as a deliberate safe state). A missing PA relay state renders `RELAY ?` — never asserted as OFF. A live TX (red `● TX`) outranks relay bookkeeping in the PA status tag. The sequencer START button stays disabled while phase is `running` or `starting`.
>
> **REQ-S38**: The 6 m Ultrabeam guard (device physics: the motorized elements support only the forward pattern on 6 m): on 6 m, reverse/bi-directional buttons must stay disabled. The console must auto-publish `direction=forward` once per invalid state (latched, moving-locked, re-fires after travel if still invalid). RETRACT must stay pressable while elements move (designated emergency action).

---

## 6. UHF rotator safety: manual-only arming

The UHF station's pan/tilt rotator (a mast-mounted camera/antenna head on an RS-485 industrial control bus, driven by the Pelco-D protocol) is deliberately NOT remote-controllable:

> **REQ-S39**: The UHF rotator's motion authority must stay unreachable over the bus. Its slot (`muehle/uhf/rotator`) exposes exactly one bus command: `stop`. Arming the drive (enabling motion at all) is a manual keyboard act inside an interactive terminal UI running on the shack PC — never remote, never automated.
>
> **REQ-S40**: The UHF rotator console must not run as an always-on daemon/service. A headless, auto-started process with motion authority contradicts the safety model. The operator starts the binary interactively. Deployment hardening otherwise follows the station convention: dedicated user, 0600 seed-once config.
>
> **REQ-S41**: The head's periodic self-check (an unprompted re-home of the head) must stay disabled in operation. The component sends the disable frame at every process start and again after every successful port reopen. Only the interactive TUI can enable the self-check again, behind a keyboard confirmation and with the head disarmed. No bus path can enable it. The engine models the state as `"on"`, `"off"`, or `"unknown"`. The model is liveness-gated: a claim lands only when the head answers a frame after the preset frame went out. Any link death returns the model to `"unknown"`. A consumer can therefore see a head whose disable state the component cannot prove.

Related UHF gap: the slot table attributes the X-Quad polarization control (`muehle/uhf/pol-ctrl`, \"PLC #2\") to the PLC firmware project. But **no PLC #2 firmware exists in the repository** — the polarization slot has no implementation and therefore no safety behavior to give (§8).

---

## 7. What is NOT protected (honest gaps)

This section is normative honesty: a reimplementation must know which hazards the current design does not cover. The team accepts items marked \"gap\" as limitations. Reproduce them knowingly or fix them consciously.

### 7.1 Antenna switch: no contact feedback, no fault reporting

- **No per-port contact feedback.** The relay driver (an I²C GPIO expander) drives coils only. No contact-closure input exists. `selected` reports the *driven coil state*, not verified closure — a failed or jammed relay stays invisible, and `settled` goes `true` 200 ms after the command regardless. The fix needs hardware (gap).
- **No fault-reporting path.** The switch firmware has no hardware-unreachable state and publishes no error field. Expander bus failures surface only in device logs. The firmware **silently ignores** invalid `/cmd` payloads — no log, no bus response. A commander gets no feedback about a rejection of its select. This stays a gap.
- **`selected` is best-effort readback** (coil state). REQ-S18's readback-of-relay-state is the strongest available. But it is not contact verification.
- **`ts` on `/state` depends on a Home Assistant time source** — with HA down the timestamp is wrong (unsynced epoch). The board reads its hardware RTC but never uses it for `ts`. This is a fragility. A reimplementation must use NTP or the RTC.
- **Security posture** (embedded device): no OTA password. An unauthenticated web server runs on port 80. A native-API encryption key sits committed in the repo. Broker credentials sit embedded in the compiled firmware image (recoverable from a physically taken device). This is a gap. The station accepts it on a trusted LAN.

### 7.2 Arm-chain readback and evaluation gaps

- **Fail-safe readback claimed but unimplemented**: the PLC firmware's relay readback carries a comment \"returns false on any read failure (fail-safe: treat as open)\". But the implementation is a bare library call with no failure detection. This is a gap. The *state* published can lie about the relay. The *relay itself* still fails safe by wiring.
- **Arm frozen during WiFi outage** (§2.3) — the only window where \"any failure drops the relay\" is not enforced.
- **Silent arm drop on heartbeat timeout** (§2.3) and **`antenna_ready` without staleness** (§2.3) — the arm can hold or drop on data from a dead slot.
- **Silent disarm on JSON-boolean `value`** (§2.3).
- Both PLC slots hardcode `device_online` to `true` — the slot-level two-layer convention degenerates to LWT-only here. Consumers must know this.

### 7.3 The port-number open decision (ultrabeam port 3 vs port 4)

The repo disagrees with itself on which physical switch port carries the Ultrabeam beam. The integration model, root docs, and the arbitrator's tests say **port 3**. The arbitrator's example config and deploy seed — and the console's antenna map — say **port 4**. The live on-device config on the deployment host is authoritative, but it stayed **unreadable** while the authors wrote this PRD. Port 1 (dummy load) is consistent everywhere. The fan dipole's port is itself contested (6 or 2 — the integration model's passive-resource list says port 2). Safety consequence: the wiring map is pure configuration and must never appear in code. The port truth must get on-device confirmation before commissioning (§8). A wrong map sends RF to the wrong antenna. The hardware exclusivity (REQ-S17) and arm chain still hold. But the band policy and band-follow can then tune the wrong antenna.

### 7.4 Reconciler fragilities that touch safety

- **Deferred-for-TX has no timeout** (§3.3) — a frozen `tx == "tx"` freezes all actuation, including grounding.
- **The deferral keys on `tx` alone** — the deployed reconciler does not use `radio.state.tuning` for the cold-switch deferral (REQ-S15). The `tuning` input sits in documented backlog next to `settled` (REQ-S19). A rebuild must use `tuning` or must document why `tx` alone is enough.
- **Restart un-grounds the station** (§3.3) — retained-replay looks like activity.
- **Operator requests unvalidated** — any non-empty, non-`auto` string becomes the hold target. The code forwards it verbatim as `{"select":"<garbage>"}` (the switch then silently ignores it, §7.1 — so the garbage goes nowhere, but the reconciler state claims a manual target that does not exist).
- **Publish failures advance dedup** — the reconciler only retries a failed select emit after a different resolution or a reconnect replay. Recovery relies on input-driven re-assertion against the switch's actual `selected`.
- **Job drops under load** — the bounded queue protects REQ-S6 by dropping. A dropped operator command stays invisible until the next event on that topic.

### 7.5 Sequencer gaps

- **No rollback and no cancel** (REQ-S30 is deliberate, but its consequence is real): a faulted half-startup leaves slots energized until an operator re-runs `start` or issues `stop` from `idle+fault`. There is no way to cancel an in-progress sequence. The worst case is ~30 s + 3 × 120 s with default timeouts.
- **The sequencer drops `stop` from `idle` without a fault.** A station started *by hand* (relays toggled directly) cannot get a shutdown through the sequencer.
- **No runner watchdog exists.** A wedged-but-connected sequencer process (a runner deadlock mid-sequence, for example) keeps `/status` `online`. The bus cannot detect the wedge. The station can stay stuck half-started or half-stopped with a stale phase. A rebuild must add a sequence-progress heartbeat or a watchdog, or must document the absence.
- **`wait_state` freshness gap**: the liveness precondition checks current `/status`, but the waited `/state` snapshot itself has no freshness bound. A device whose bridge is online per LWT but whose device link died *without* a `/status` change can satisfy a wait on a stale `power == "on"`. The two-layer rule is not enforced on the wait payload. This is a gap. A fix can couple the wait to `device_online`.
- **The deploy seed omits the step lists**: the generated first-deploy config has no `[[startup]]`/`[[shutdown]]` sections. The validation logic needs them. A fresh deploy crash-loops until an operator hand-adds them. This is a deploy defect. The seed must ship the full step lists.

### 7.6 Bus-level gaps affecting safety consumers

- **Retained `/status` stays `online` after a clean shutdown** (no will on graceful disconnect) — a stopped service looks alive to `/status`-only consumers (§1.2).
- **Change-only producers**: besides the radio-heartbeat incident (REQ-S14), the antenna switch and PLC `switch` slot publish state only on change. A quiet bus makes retained snapshots age without any staleness signal.
- **Tuner `inline` is client-side belief** — the ATR-1000 protocol has no inbound in-line/bypass confirmation. `/state.inline` can be wrong after a front-panel toggle or link loss. The field is mirror-only. Do not build interlocks on it.
- **Tuner link has no keepalive or half-open detection** — the tuner WebSocket has no ping/pong and no idle-read timeout. A silently dead TCP connection (a WiFi drop without a reset) can leave `/state.device_online` at `true` indefinitely. A 5 s write timeout applies to command writes only. Consumers must not treat tuner `device_online` as a freshness signal.
- **The reconciler is a single point of coordination** — accepted *only* under REQ-S1 (safety is hardware). An explicit \"reconciler offline\" operator indication is an open wish (for now only its LWT).

---

## 8. Open decisions and unresolved facts

Each item lists the evidence for every variant. No resolution is silently invented.

1. **Ultrabeam switch port: 3 or 4?** Variant A (port 3): repo-root CLAUDE.md, `docs/station-integration-model.md` §7.1, antennaselect unit tests. Variant B (port 4): `antennaselect/config.example.toml`, `antennaselect/deploy.sh` seed, and the console's antenna port map (which renders port 4 as \"Ultrabeam\" and leaves out port 3 as unwired). The deployed `/etc/antenna-select/config.toml` on shari is authoritative and stayed unreadable from the workstation while the authors wrote this PRD. Every document touching the wiring must present this as needing on-device confirmation. Port 1 (dummy load) is consistent everywhere. The fan dipole's port is also contested (6 or 2, one integration-model source says port 2) and needs the same on-device confirmation.
2. **Radio heartbeat mechanism** (REQ-S14): periodic `/state` republish while the link is live (research suggests ~5 s, chosen as < half the 10 s consumer window), vs enlarging the consumer window, vs a dedicated heartbeat topic. The reimplementation must coordinate whichever variant it chooses with *every* consumer of the 10 s freshness figure (the arm PLC, the sequencer's freshness logic, console staleness displays). Current code has NO producer heartbeat — the live starvation incident is the evidence this must change.
3. **Error string on heartbeat timeout**: current firmware drops the arm silently (no `error` field — the error function does not test freshness). The REQ in §2.3 says publish a reason. The exact string (`"radio state stale"`, `"heartbeat timeout"`, …) is not decided. Related: whether a stale `antenna_ready` must produce an error, and whether the arm must use the ant-switch `/status` LWT.
4. **Arm evaluation during WiFi outage**: current code freezes the relay (§7.2). The recommendation is compute the arm condition always — drop is the safe direction. This is a deliberate contract change. A reimplementation must make it explicitly.
5. **`/cmd` boolean handling**: accept JSON booleans, or reject loudly — current `set_enabled` silently disarms on `"value": true` (§2.3). The station-wide convention says booleans ride as strings. The console complies (`{"action":"set_enabled","value":"true"}`).
6. **`device_online` form**: the model says \"omitted when true\". The deployed bridges publish explicit `true`. Mandate one form, or give the consumer-side equivalence a spec (the PRD asks consumers to treat absence as true either way).
7. **Broker topology**: deployed code points at `192.168.1.50:1883`. The \"shack broker migration\" to `192.168.1.139` exists on an unmerged feature branch — the PLC firmware's checked-in local secrets already say `.139`. Safety-relevant because every embedded fail-safe node's MQTT reachability depends on the broker that the node code uses at compile time and in configuration. The migration is a commissioning decision point.
8. **Non-TX re-arm path for post-ground recovery** (REQ-S24): operator-command-counts-as-activity vs settled-gated first TX vs both. The current design has neither. The first-key-up-into-the-short incident is the evidence.
9. **TX-deferral timeout value** for the reconciler's cold-switch deferral (none exists today), and **idle-clock restart stability** (a reconciler restart now un-grounds and re-arms the station — is restart-time re-grounding or persisted idle state wanted?).
10. **The physical detail of `off`**: station docs say the `off` position \"shorts the antenna feeds to ground\" (lightning protection). The switch firmware realizes `off` simply as *all relays open*, and its own documentation describes the feed line as \"unconnected\". The repo cannot show whether grounding is a property of the switch hardware's unused-port wiring or of an external grounding element. Check this against the physical installation. The safe-default behavior is the same either way.
11. **PLC #2 / `muehle/uhf/pol-ctrl`**: the slot table assigns the X-Quad polarization relays to \"m5stamp (PLC #2)\", but no such firmware exists in the repo (no second build environment, no pol-ctrl topics). Either the slot table is aspirational, or the firmware lives elsewhere. The firmware can give no polarization safety behavior.
12. **Sequencer `stop`-from-any-phase / cancel command**: the team documented the current drop behavior as intent. Adding a cancel is a deliberate contract change (§7.5). Also undecided: a freshness bound / `device_online` coupling for `wait_state`.
13. **PA band-walk during transmit**: the ACOM bridge does not stop `set_band` while the amp transmits. The amp's own hardware can refuse with `STOP TRANSMISSION FIRST`, leaving its band position uncertain until the next telemetry frame. Whether software must TX-gate `set_band` is open.
14. **Console START button during phase `stopping`**: the deployed UI enables START while the shutdown sequence is still in flight. The sequencer's busy guard drops a `start` sent mid-stop, so no hazard materializes — but the UI does not guard it. Preserve knowingly or fix.
15. **Logging layer** (`docs/logging-integration-model.md`, log/event + spots/event planes) has a spec but no component builds it — future scope, no safety claims. Host-liveness nodes (`muehle/host/shari`, `muehle/host/shack-pc`) appear in the model, but no bridge publishes them — model-only.
16. **Missing `tuning` field default** (REQ-S13): the deployed PLC firmware counts an absent `radio.state.tuning` as `false` (not tuning), the permissive side. A radio state snapshot without the key can then arm during a real tune cycle. Either default the missing field to `true` (fail closed — a deliberate contract change against the deployed firmware) or keep the deployed default and document the fail-open risk.
17. **`60m` in the arm allow-list** (§2.2 `band_safe`): the PLC firmware's `SAFE_BANDS` list contains 11 bands including `60m`. The ACOM 1200S covers 10 bands and has no `60m` filter (`set_band` on the PA bridge accepts the same 10). Either drop `60m` from `band_safe`, or check on hardware and document why `60m` operation with the PA is safe. Until resolved, the arm can close on a band the PA does not cover.
18. **Tuner-follow TX-gating** (REQ-S17a): gate the reconciler's `set_inline` command on transmit together with the port select, or accept the current behavior (in-line intent emitted during a TX-deferred port change) as a documented gap.