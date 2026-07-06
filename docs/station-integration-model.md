# Station Integration Model — Reference

Version 0.6 (draft). This defines the *shape* the configuration takes and, from 0.4, its
transport binding. Resolved in 0.2: hierarchy and tune-routing confirmed; band policy
completed; rotators, the polarization controller, and PA-arm hardware pinned down;
compute hosts added as a first-class node kind. In 0.3: the `station` node reduced to a
structural grouping with a single operator-set `activity` flag. In 0.4: MQTT fixed as the
transport binding, and the design invariants (one-way dependency, optional consumers,
UI-agnostic operator surface) written down. In 0.5: worked on-the-wire payloads for the
`radio` slot added as Appendix A, and band capability declared by name against the
canonical table rather than as per-device ranges. In 0.6: multi-receiver model added
(active TX receiver at fixed `radio/state`, per-receiver sub-topics for `receivers > 1`);
mode normalization responsibility assigned to adapters; host assignments confirmed (shari);
meter data deferred to §12; Appendix A corrected (`drive_pct` → `drive`; `rx_input` flagged
as requiring adapter work).

The guiding idea: the live configuration is the documentation. A component, once
connected, describes itself — who it is, what it can do, and what is currently true
about it — to a well-known address. Anyone who wants to understand the station
subscribes and reads. This document defines the grammar of that self-description so
that a component can be understood with little additional context, and so that a
GenAI-written adapter has a strict target to translate into.

---

## 1. Principles, as three planes

The three original goals collapse into one architecture once every component is
treated as a small control loop with three separate planes that never share a path.

- **Identity and capability** — who I am, what I can do. Static, published on connect.
- **State** — what is currently true about me. Live, retained.
- **Command / intent** — what someone would like to be true. Transient.

From these the goals follow:

- *Configuration is the documentation.* Self-description on the identity and state
  planes means the inventory is always a live read, never a separate document that
  rots. Reference things by name and role, never by IP address or UDP port.
- *Change is constant; avoid tight coupling.* A component publishes state to its own
  address and receives intent at its own address. Consumers react to **state**, never
  to the fact that a command was sent. Swap the box behind an address and nothing
  downstream changes.
- *Integration is cheap.* Cheap adapters stay coherent only against a strict core. The
  canonical vocabulary and slot template are precious and versioned; per-vendor
  adapters are thin and disposable. The cheapness is a consequence of the strictness.

**Plane discipline (the rule most easily violated):** consumers couple to state, not
to intent. A command is fire-and-observe — send it, then watch state to see if it
took. Nobody assumes success because a command was emitted.

---

## 2. Naming and hierarchy

```
site / station / position / <slot>
```

- **site** — the physical property, by its common name (`muehle`).
- **station** — a transmitting entity (`hf`, `uhf`). This is the unit of contention in
  a multi-multi setup: one transmitter per band, shared-resource claims. The station
  node is a structural grouping, not a stateful component: membership is everything
  under its path, and it carries only an operator-set `activity` flag (never inferred).
- **position** — an operator seat. Collapses to one and disappears in a single-op
  shack; reappears only when two operators share a station.
- **slot** — a role at that position (`radio`, `pa`, `ant-switch`, ...).

A physical building that is not a transmitting entity (the Bauwagen) is a
**`location` attribute**, not a path level. A second building is then just another
`location` value; promote it to a real level only if buildings become
contention-relevant (shared feedline, shared rotator).

The address is structural. The device filling a slot is an attribute of the slot, not
part of the address — so an unnamed rotator is still addressable as `.../rotator`.

---

## 3. The slot template

Every slot — device or logic — has the same three-plane shape.

```
<site>/<station>/<slot>

# birth certificate — published on connect, retained, changes rarely
role:          <role>
device:        { model, serial, firmware }   # swappable; absent for logic slots
link:          <transport>                   # informational
location:      <building>
capabilities:  { ... }                       # the discovery contract

# state — retained, last-write-wins, online driven by last-will
online:        bool
<field>:       <type>                         # what is currently true

# intent — transient; the slot reconciles toward it or rejects
<action>:      <type>
```

Rules that hold for all slots:

- **State is retained** (last-known-value) so a late-joining consumer or logger has the
  full picture without polling.
- **Liveness via last-will:** `online` flips false automatically when the component
  drops. Retained state is then known-stale.
- **Never trust retained state for safety.** Retained values can be stale after a
  crash. Safety lives on the hardware plane (Section 6), never on retained messaging
  state.
- **Capability is the discovery contract.** Consumers bind to a declared capability,
  not to a device model. A binding is valid only if both ends declare compatible
  capabilities.
- **Adapters live at the edge.** A per-vendor adapter translates proprietary protocol
  into canonical schema. Where an adapter runs is deployment metadata
  (`host: <name>`), not topology.

Component kinds:

| Kind | Publishes state? | Example |
|---|---|---|
| Active equipment | yes | radio, pa, ant-switch, ultrabeam controller |
| Passive resource | no — referenced by config | fan dipole, mast, dummy load |
| Service / logic | yes | reconciler, arbiter, logger, Horst modules |
| Host / infrastructure | yes (liveness, health) | shack PC, Raspberry Pi (`shari`) |

A passive resource is named and referenced (e.g. by the wiring map) but has no state
of its own. One physical antenna can span both: the Ultrabeam is a passive RF resource
on a switch port **and** an active control slot that follows band and frequency.

A host is a compute node that runs adapters and services; it publishes its own
liveness so the deployment topology is documented and its failure is visible. An
embedded controller (an M5Stack PLC) collapses device, adapter, and host into a single
node — custom firmware speaks the canonical schema directly.

---

## 4. Canonical vocabulary

- **Frequency in Hz is the single source of truth.** `band` is derived from `freq_hz`
  and published for convenience. There is no `set_band` intent; commanders set a
  frequency and band falls out. This removes the class of bug where band and frequency
  disagree because two things set them independently.
- **Bands.** The canonical vocabulary owns the band name→frequency-range table (edges are
  region- and license-dependent; these are DL / IARU R1). A device declares the band
  *names* it supports and references this table rather than re-stating edges. In state,
  `freq_hz` is the truth and `band` is the derived name.
- **Modes:** `cw`, `usb`, `lsb`, `am`, `fm`, `data`. These are canonical names; firmware
  typically uses proprietary variants (`CW-U`, `CW-L`, `DIGU`, `DIGL`, `USB`, `LSB`,
  `FDV`, `RTTY-U`, …). **Normalization is the adapter's responsibility** — consumers see
  only the canonical names above. Adapters must publish a canonical mode or omit the
  field; they must never publish a raw firmware mode string.
- **Roles:** `radio`, `pa`, `tuner`, `ant-switch`, `rotator`, `pol-ctrl`, `preamp`
  (function; may be a slot or a declared capability), `bias-feed` (likewise),
  `reconciler`, `host`. Passive: `ant/*`, `mast/*`, `preamp/*` (masthead LNA).
- **Capability keys** seen so far: `bands`, `modes`, `receivers`, `diversity`,
  `amp_key`, `tune`, `bias_t`, `band_source`, `rf_sample`, `alc_out`, `hot_switch`,
  `ports`, `off`, `exclusive`, `axes`, `polarizations`.

**Multi-receiver radios.** The `receivers` capability declares how many independent
receive paths a radio provides (SDRs, contest transceivers, and multi-receiver rigs may
have two or more). `radio/state` is always the **active TX receiver** — one fixed address
that downstream actuators (PA, antenna-select reconciler) bind to regardless of how many
internal slices, VFOs, or SCUs the hardware has. For a radio with `receivers > 1`, each
receiver also publishes its own state at `<slot>/receiver/N/state` (for logging or
multi-band monitoring consumers), but no core component depends on those sub-topics.
Single-receiver radios publish only `radio/state`; the sub-topics are absent and never
required. The active receiver is defined as the one currently selected for transmit (or,
during RX-only, the primary VFO).

**Function vs realization:** the same logical function (e.g. bias-T for a masthead
preamp) may appear as a standalone slot or be absorbed as a capability of a parent
device. Bind to the function; discover where it lives from capability declarations.
A dumber device simply does not declare the capability, and you add a slot to fill the
gap — nothing downstream changes.

**Wiring map:** the single editable place antenna arrangement lives. It maps switch
ports to named passive resources. Devices never contain it; the reconciler reads it.

---

## 5. Mechanism A — reconciler and the arbitration ladder

Actuators are dumb; policy lives in a reconciler/arbiter. When more than one commander
wants one actuator, a single arbiter resolves them by priority and emits one intent
stream. The actuator receives one command source and reports its state.

**Priority ladder (highest asserting tier wins):**

```
1 idle:      station inactive             → safe default (off / ground)
2 operator:  active hold (dummy, forced)  → that selection
3 auto:      policy(state)                → resolved selection
```

- `mode` is **derived**: `manual` while an operator hold is active, else `auto`. No
  separate auto/manual switch is needed — the presence of a hold *is* manual mode.
- `source` (`idle | operator | auto`) is published so the live config documents *why*
  the actuator is where it is, not just where.

This arbiter is the multi-multi contention mechanism in miniature. The box that ranks
idle over operator over auto for one operator is what later ranks two stations
contending for one shared antenna: more inputs, same ladder.

---

## 6. Mechanism B — the soft/hard straddle

Safety, interlock, and sequencing do **not** live on the loosely-coupled messaging
plane. The messaging plane sets desired state and reports actual state; a hardware
path enforces safe state during transitions and fails safe.

- **Hardware series interlock.** A physical TX-inhibit line runs in series through the
  relevant components; any open link inhibits transmit. Fast, local, fail-safe. The
  messaging layer **mirrors** each link as read-only state for documentation and
  monitoring, and never sits in the enforcement path.
- **Fail-safe defaults.** A relay's default and loss-of-signal state is the safe one
  (PA isolated / TX inhibited). Loss of software, network, or power degrades to
  inaction, not hazard.
- **Software arm, not software gate.** Where there is no hardware trigger for a hazard
  window (e.g. tuning), express the software contribution as a slowly-changing *arm*
  permit combined by hardware AND with the fast hardware key line. Normal transmit
  timing rides the hardware edge; software only arms/disarms. The arm permit is
  heartbeat-driven — loss of heartbeat drops the arm.
- **Ordering contracts.** Some intents carry a required order:
  - *Tune:* disarm the PA and confirm before the tune carrier. Because there is no
    hardware tune line, route the TUNE action through the automation so disarm leads.
  - *Port change:* a cold-switch-only switch (`hot_switch: false`) must have RF
    inhibited and RX confirmed before the port moves, auto or manual.

---

## 7. Site instantiation — Mühle

Two stations, both `location: bauwagen`. Each station node is structural — a path
prefix over its members — and carries one field only:

```
muehle/hf   |   muehle/uhf
role:     station
kind:     { hf | uhf }
location: bauwagen
activity: { active | inactive }    # operator-set, never inferred; read by ladder tier 1
```

Everything else a station might report (band, tx, readiness) already lives on the radio;
deriving it here would only duplicate. The switch's fail-safe-to-ground default covers
power loss independently of this flag.

### 7.1 Station `hf`

**`muehle/hf/radio`** — FLEX-8400, ethernet
```
capabilities: bands [160m..6m, by name]; modes [cw,usb,lsb,am,fm,data];
              receivers 1; diversity false; amp_key true; tune true; bias_t false;
              rx_inputs [ant1,ant2,rx_a]; tx_outputs [ant1,ant2]
state:        online; freq_hz (Hz int); band(derived); mode (canonical); tx {rx|tx};
              tuning (bool); drive (0-100); rx_input (omitempty, adapter work pending)
              — always the active/TX receiver
intent:       set_freq_hz; set_mode; set_drive; select_rx; tune {start|stop}
```
The FLEX-8400 supports up to 4 simultaneous receive slices (a SmartSDR concept). These
map generically to receivers. With `receivers 1` declared, only `radio/state` is
published. If a multi-slice configuration is added later, promote the declaration to
`receivers N` and publish per-receiver state at `radio/receiver/N/state`.

**`muehle/hf/pa`** — ACOM 1200S, ethernet (carries band/mode/telemetry, NOT keying)
```
capabilities: bands [...]; max_power_w <spec>; band_source cat; rf_sample false;
              key_input hardware; alc_out true; modes [operate,standby]
state:        online; mode {operate|standby|bypass}; band; keyed {rx|tx|inhibited};
              fwd_power_w; rfl_power_w; temp_c; fault {none|swr|temp|reflected}
intent:       set_band; set_mode {operate|standby}; clear_fault
```

**`muehle/hf/ant-switch`** — wifi 1-to-5 (dumb actuator)
```
capabilities: ports [1,2,3,4,5]; off true; exclusive true; hot_switch false
state:        online; selected {off|p1..p5}; settled
intent:       select {off|p1..p5}
```

**`muehle/hf/tuner`** — ATR-1000 (BTR-1000 / N7DDC design), wifi — schema TBD, follows
the standard template.

**`muehle/hf/rotator`** — Yaesu G-450DC. Control path: AF6SA WRC, with bridge software
on `shari` presenting it to the automation. `capabilities: axes [az]`.

**`muehle/hf/ultrabeam-ctrl`** — Ultrabeam controller, bridge on `shari`. Follows band
and frequency; reports element/tuning state.

**`muehle/hf/rx-loop-ctrl`** — K9AY controller. Reports loop direction; the loop preamp
is a sub-state of this slot and is dropped by the hardware TX-low line.

**`muehle/hf/pa-arm`** — the PA-enable relay. Publishes `armed`. Arm decision is soft
(derived from `radio.tuning` and band safety); the AND with the hardware key line and
the fail-safe-open default are hardware. Realized as a discrete M5Stack PLC with custom
firmware — an embedded node that is device, adapter, and host in one.

**`muehle/hf/antenna-select`** — reconciler/arbiter (logic slot)
```
subscribes: radio.band, radio.tx, station.activity, operator.request, ant-switch.selected
emits:      ant-switch.select
config:     wiring_map { p1: dummy-load, p2: fan-dipole, p3: ultrabeam, off: grounded }
            band_policy { 6,10,12,15,17,20m: ultrabeam;
                          30,40,60,80m:     fan-dipole }   # 30/60m require the ATU
            priority   { 1 idle, 2 operator, 3 auto }
state:      mode {auto|manual}; target {off|p1..p5}; source {idle|operator|auto}
```

**Passive resources:** `ant/ultrabeam` (port 3), `ant/fan-dipole` 80/40 (port 2),
`ant/dummy-load` (port 1), `ant/k9ay-loop` (RX, via rx-loop-ctrl), `mast/ta16-hf`.

**Adapters:** the `pa`, `tuner`, HF `rotator`, and `ultrabeam-ctrl` bridges run on host
`shari`; `pa-arm` is a self-hosted M5Stack. See §7.3. The PA's CAT band-following
(null-modem + DXE-ACOMSS-KWD cable) lives entirely inside its adapter.

**Soft bindings (reconciler, state → intent):**
- `pa.set_band` ← `radio.band`
- `ultrabeam-ctrl.{band,freq}` ← `radio.{band,freq}`
- `ant-switch.select` ← arbiter(band_policy, wiring_map, ladder)
- `ant-switch.select` → `off` when `station.activity = inactive` (ladder tier 1)
- `pa-arm.armed` ← false while `radio.tuning`, fail-safe open, heartbeat-driven

**Hardware interlock chain (enforces; software mirrors only):**
```
radio (TX low) → rx-loop-ctrl (preamp off) → ultrabeam-ctrl (inhibit if moving) → pa
```
Enforces: RX preamp protected on TX; no high-power TX while the beam moves; PA keys only
when every link is closed.

### 7.2 Station `uhf`

Leaner — most of the HF machinery is absent, which is the model collapsing correctly.

**`muehle/uhf/radio`** — IC-9700. Declares `bias_t: true` (switched, per band). The
masthead preamps are enabled manually by the operator via the radio's bias voltage and
dropped automatically on TX when the radio removes the bias — so preamp protection is
internal to the radio and needs no slot and no external sequencer. Preamp is a
*capability* plus a passive LNA, not an active slot.

**`muehle/uhf/rotator`** — SPID rotator with its own controller, driven over serial by
PSTRotator on host `shack-pc`. `capabilities: axes [az, el]`. Same role name as the HF
rotator but a completely different control stack; a satellite-tracking consumer reads
the axes rather than knowing the hardware.

**`muehle/uhf/pol-ctrl`** — M5Stack PLC with custom firmware. `capabilities:
polarizations [h, v, cl, cr]`. Settable state, operator-driven; no automatic binding.

**Passive resources:** `ant/xquad-2m`, `ant/xquad-70cm`, `mast/ta16-vhf`,
`preamp/2m`, `preamp/70cm` (masthead LNAs, manually enabled, bias supplied and dropped
by the radio).

No PA, no tuner, no antenna switch — band routing is physical via the radio's separate
2 m / 70 cm ports, so there is no antenna-select binding at all.

### 7.3 Hosts and adapters

Compute hosts are first-class infrastructure nodes: each publishes its own `online`
and health, and every adapter or service declares the host it runs on. The set a host
runs is derivable from those declarations — a single source of truth, not duplicated on
the host. If a host drops, its hosted components go offline via their own last-will, and
the host's own last-will marks it down.

```
muehle/host/shari       # Raspberry Pi, Linux
  role: host;  location: bauwagen;  state: online, temp_c, load
muehle/host/shack-pc    # shack PC
  role: host;  location: bauwagen;  state: online
```

| Adapter / service | Fronts | Host |
|---|---|---|
| PA bridge (+ ACOM telemetry) | `hf/pa` | `shari` |
| Tuner bridge | `hf/tuner` | `shari` |
| HF rotator bridge (AF6SA WRC) | `hf/rotator` | `shari` |
| Ultrabeam controller bridge | `hf/ultrabeam-ctrl` | `shari` |
| PSTRotator (serial) | `uhf/rotator` (SPID) | `shack-pc` |
| FLEX bridge (flex2mqtt) | `hf/radio` | `shari` |
| HF antenna-select reconciler | `hf/antenna-select` | `shari` |
| Logging | subscriber, no slot | `shari` |
| pol-ctrl firmware | `uhf/pol-ctrl` | embedded (M5Stack) |
| pa-arm firmware | `hf/pa-arm` | embedded (M5Stack) |
| HA discovery bridge (optional) | consumer of `meta`/`state`/`status` | `shack-pc` |

Embedded controllers collapse device, adapter, and host into one node and are the
cheapest integration point on the station; the model treats them identically to a
bridged device — same three planes, just co-located.

---

## 8. Transport binding (MQTT)

MQTT, plain JSON, no Sparkplug. Sparkplug's birth/death certificates, capability
announcement, and state/command split are already specified above, so adopting its wire
format would pay protobuf and sequence-number costs for machinery we already own — and
its opaque payloads and fixed namespace fight "configuration is documentation." Borrow
the ideas (retained birth certificate, death via Last Will), not the encoding.

Four topic suffixes per slot — one per plane, plus liveness:

```
<addr>/meta     retained       birth certificate: identity + capabilities (rarely changes)
<addr>/state    retained       live state as ONE JSON snapshot document
<addr>/status   retained       online | offline — the Last Will topic
<addr>/cmd      NOT retained   intent
```

- **State is one retained JSON document, not per-field topics.** A late or reconnecting
  subscriber gets the whole atomic snapshot immediately — no polling, no half-updated
  scatter. The field layout of that document is defined by each slot's schema in §7; the
  transport defines only the envelope.
- **Liveness is the Last Will on `status`.** Register a retained `offline` will on
  connect, publish retained `online` once up, publish `offline` yourself on clean
  shutdown. The broker produces `online: false` when a node drops without anything else
  having to notice.
- **`meta` is retained**, so a late subscriber gets the birth certificate automatically;
  the rebirth-request machinery Sparkplug needs is unnecessary.
- **Never retain `cmd` — this is a safety rule, not a style choice.** A retained command
  re-delivers to the next subscriber and on every reconnect, so a stale `tune` or
  `select` would fire itself with no operator behind it. State is retained; intent never
  is. This is the transport-level echo of the plane discipline.

  **Exception: physical actuators with self-healing semantics.** For a physical device
  whose correct position must survive a software restart (e.g. an antenna controller
  tuned to a specific frequency), the `/cmd` topic MAY be retained with the desired
  steady-state. One-shot physical commands (e.g. `retract`) MUST clear the retained
  topic (publish empty payload) immediately after execution so they do not re-execute
  on reconnect. A command is safe to retain only when re-applying it on reconnect
  produces the same physical outcome as the original.

Defaults: QoS 1 everywhere; QoS 2 only for a specific command that must not double-fire
even under broker retry. Client IDs derived from the slot address, so a duplicate
connection is diagnosable. Broker: Mosquitto with a persistent store (so retained `meta`
and `state` survive a restart); EMQX only if a rules engine or clustering is wanted
later, which it isn't yet.

---

## 9. Design invariants

The constraints most likely to erode once the project is open-sourced and others
contribute. Hard rules, not preferences.

- **Dependency points one way only.** Core components — radios, PAs, switches, the
  arbiter, the interlocks — know nothing about any consumer and never publish to a
  consumer-specific tree (no core component publishes under `homeassistant/…`). Consumers
  depend on the canonical bus; the bus never depends on a consumer. Delete every consumer
  and the station runs identically.
- **Consumers are optional modules.** Dashboards, historians, and the Home Assistant
  bridge are separate processes, disabled by default, required by no core component, and
  documented under optional integrations rather than getting-started. Home Assistant is
  the reference consumer, not a privileged one; the same bus grows Grafana, Node-RED, or
  Prometheus consumers the same way — each a thin edge adapter against the same core.
- **The operator surface is a UI-agnostic canonical topic.** Manual control (an operator
  hold into ladder tier 2) is a plain canonical topic any UI can publish — a CLI, a web
  page, an M5Stack button, or the HA bridge. The HA bridge is merely one writer of that
  topic, never its definition, so "manual antenna control" never becomes "manual antenna
  control if you run HA."

---

## 10. Trade-offs and known weak points

Stated plainly rather than left implied.

- **The reconciler is a coordination single point.** If it dies, soft bindings stop
  updating; the station degrades to manual and retained state goes stale. This is
  acceptable *only because* safety is hardware and fail-safe — but it means band-follow,
  arm logic, and antenna selection all stop together. Consider supervision/restart and
  an explicit "reconciler offline" indication so the operator knows they are on manual.
- **Retained state can be acted on while stale.** Mitigated by `online`/last-will and
  heartbeats, but any consumer that acts on retained state must check liveness first.
  This is why no safety decision may depend on it.
- **Band-policy residual.** 30/40/60/80 m now map to the dipole and 6-20 m (incl. 17/12)
  to the Ultrabeam. Two riders remain: 30/60 m on the 80/40 dipole are not resonant, so
  the policy implicitly requires the ATU in-line and tuning on those bands; and 160 m has
  no antenna assigned, so a 160 m frequency resolves to nothing. Decide the 160 m
  no-match default (reject, hold last, or a named fallback).
- **Hosts are now single points too.** `shari` fronts the HF PA, tuner, rotator, and
  Ultrabeam; `shack-pc` fronts the VHF rotator and, most likely, the reconciler and
  logging. A host loss takes its whole cluster offline at once — correctly, via
  last-will, but simultaneously. Host liveness is load-bearing and worth monitoring;
  the reconciler-on-shack-PC assumption compounds the coordination single point above.
- **GenAI-authored adapters vary in quality.** The strict core limits blast radius, but
  a bad adapter can publish plausible-but-wrong state and drive correct bindings to
  wrong outcomes. Adapters need conformance tests against the schema before they are
  trusted, especially for state fields that feed safety mirrors.
- **Idle-over-operator is a deliberate surprise.** Walking away (station inactive)
  overrides an operator dummy-load hold. Defensible for walk-away safety; documented so
  it is chosen behaviour, not emergent.
- **Two TA16 masts.** Distinct passive-resource names (`mast/ta16-hf`, `mast/ta16-vhf`)
  are load-bearing; never reference a mast by model alone.

---

## 11. Open items to confirm

All seven v0.1 items are resolved and folded in (hierarchy confirmed as stations; 23 cm
out of scope; UHF polarization and preamps operator-only; devices identified; `pa-arm`
an M5Stack; band policy set; tune routing through the automation). Residuals surfaced by
those answers:

1. **160 m antenna.** No antenna is assigned to 160 m; confirm whether you operate it
   and, if so, on what.
2. ~~**`shack-pc` roles.**~~ Resolved: shari hosts flex2mqtt (FLEX bridge), the reconciler,
   and logging. shack-pc hosts PSTRotator only.
3. **`pa-arm` build.** Recorded as a discrete M5Stack PLC (from "perhaps another
   M5StackPLC"); confirm once built rather than planned.

---

## 12. Deferred

The reconciler's internal implementation; horstprop Layer-3 modelling; multi-multi
arbitration beyond the single-station ladder; and the mechanical field layouts for the
slots still marked TBD in §7. None of these change the shapes above.

**Live meter data (VITA-49).** The real-time meter stream from the radio (S-meter, SWR,
PA power, temperatures — decoded from VITA-49 datagrams at 10–20 fps) was intentionally
removed from the flex2mqtt bridge: it is high-rate, non-retained, and belongs to a
monitoring/logging consumer rather than the canonical station bus. When added, it will
live at a sub-topic (`<slot>/meter/<group>/<name>`) and be explicitly **not retained**,
consistent with the plane discipline (high-rate telemetry ≠ state).

---

## Appendix A — worked example: the `radio` slot on the wire

Concrete payloads for `muehle/hf/radio`, the reference a new adapter copies. All JSON is
UTF-8. `meta`, `state`, and `status` are retained; `cmd` is not. QoS 1 throughout.

`radio/state` always represents the **active TX receiver**. For a single-receiver radio
(`receivers: 1`) this is the only receiver. For multi-receiver radios, per-receiver
sub-topics (`radio/receiver/N/state`) are additive; downstream actuators (PA, rotator,
antenna-select) bind only to this top-level document.

`muehle/hf/radio/meta` — retained
```json
{
  "schema": "1.0",
  "role": "radio",
  "device": { "model": "FLEX-8400", "serial": "8400-01234", "firmware": "3.8.19" },
  "link": "ethernet",
  "location": "bauwagen",
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

`muehle/hf/radio/state` — retained
```json
{
  "ts": "2026-07-06T12:34:56Z",
  "freq_hz": 14025000,
  "band": "20m",
  "mode": "cw",
  "tx": "rx",
  "tuning": false,
  "drive": 40,
  "rx_input": "ant1"
}
```

`muehle/hf/radio/status` — retained, and the Last Will
```
online
```

`muehle/hf/radio/cmd` — not retained
```json
{ "set_freq_hz": 14074000 }
```

Encoding decisions worth copying:

- `freq_hz` is the canonical integer truth (Hz, not MHz); `band` is the derived name,
  resolved against the canonical table (§4), not re-declared here.
- `mode` is the canonical name (`cw`, `usb`, …). The adapter normalizes the firmware
  string before publishing; consumers never see raw firmware mode names.
- `drive` is the transmit gain as a 0–100 integer (the SmartSDR `drive` field). Not
  `drive_pct` — keep the name short and match the source API.
- `rx_input` is omitempty; an adapter that does not yet parse RX-input selection simply
  omits it rather than guessing. Consumers that need it check for its presence.
- **Liveness is the `/status` topic, never an `online` field in `/state`.** A dead node
  cannot update its own state document to say it is gone — which is exactly why the Last
  Will publishes to a separate topic. `status` is a plain string (`online` / `offline`),
  not JSON, so it maps to Home Assistant availability with no template.
- `/state` is a full snapshot on every publish (last-write-wins); there are no partial
  updates, so any subscriber always holds a complete, consistent view.
- `tune` is absent from operator `cmd` traffic. It reaches `/cmd` only from the sequencer
  after `pa-arm` is disarmed and confirmed (§6); operators publish a tune request to the
  sequencer, never directly here. Everything else (`set_freq_hz`, `set_mode`, `set_drive`,
  `select_rx`) is a direct intent.
