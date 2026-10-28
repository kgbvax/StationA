# Station Integration Model: Reference

Version 0.10 (draft). This defines the *shape* the configuration takes and, from 0.4, its
transport binding. Resolved in 0.2: hierarchy and tune-routing confirmed; band policy
completed; rotators, the polarization controller, and PA-arm hardware pinned down;
compute hosts added as a first-class node kind. In 0.3: the `station` node reduced to a
structural grouping with a single inferred `activity` flag. In 0.4: MQTT fixed as the
transport binding, and the design invariants (one-way dependency, optional consumers,
UI-agnostic operator surface) written down. In 0.5: worked on-the-wire payloads for the
`radio` slot added as Appendix A, and band capability declared by name against the
canonical table rather than as per-device ranges. In 0.6: multi-receiver model added
(active TX receiver at fixed `radio/state`, per-receiver sub-topics for `receivers > 1`);
mode normalization responsibility assigned to adapters; host assignments confirmed (shari);
meter data deferred to §12; Appendix A corrected (`drive_pct` → `drive`; `rx_input` flagged
as requiring adapter work). In 0.7: the generic/concrete boundary made explicit, with canonical
role `ant-ctrl` introduced and the `ultrabeam-ctrl` slot renamed (a device name had leaked
into an address, violating §2's own rule); the liveness contradiction resolved (`online`
removed from the §3 state template; optional `device_online`/`error` state fields defined);
`/meta` fields made normative, including `host`; a resource→controller map added so
band-follow needs no antenna names in code; adapter conformance checklist added (§8.1);
the HA-discovery deviation in current bridges recorded against §9. In 0.8: a
power-distribution layer added, with canonical roles `power`, `switch`, `sequencer` (§4); a
site-level `power` path analogous to `host` (§2, §7.3); the compound-device pattern made
explicit (§3); PA remote-on moved off the PA slot (`set_power`/RTS removed) onto the power
layer; `pa-arm` realized by an M5 Stamp PLC (closing §11.3); and a `power-seq` sequencer
owning the ordered, delay-and-confirmation startup/shutdown. In 0.9: the
sat-ops rotator mount added, with `uhf/az-rotator` (SPID) and `uhf/el-rotator`
(GS-500 via ERC-M) as two per-axis slots, each on its own serial link, from one
compound bridge (`spid-ercm-rotator-bridge` on shari), free motion with no arming gate and the
posture's exposure review recorded in `known-issues.md`; the stale PSTRotator
host attributions corrected (shack-pc fronts pelcobridge2, the PTS-303Z/3050DZ
pan/tilt head). 2026-09-23: antenna-select tier 0, the operator `manual` stand-down,
outranks idle (§5). In 0.10: the `sat-track` role added (§4) with the
`uhf/sat-track` slot (§7.2) for the satellite the station is tracking, fronted from
OscarWatch's Satellite-link WebSocket by `oscarwatch-sattrack-bridge` on the new host
`scmino` (§7.3). 2026-10-10: the `uhf/rotator` slot (pelcobridge2, the
PTS-303Z/3050DZ pan/tilt head on shack-pc) retired, as it was not in use; UHF az/el is the
sat-ops mount (`uhf/az-rotator` / `uhf/el-rotator`). Also 2026-10-10: `power/antennas`
and the `pc-power` role with `power/shack-pc` (`winpc-power-bridge` on bwpc) added;
sequencer idle standby. 2026-10-09 to 2026-10-18: most bridges and logic slots moved
from shari to scmino (§7.3). 2026-10-18: `power/master` removed (the site master mains
is HA-owned and out of scope), `power/shack-pc-mains` added. 2026-10-19: sequencer idle
shutdown 2 h after the last activity. 2026-10-22: the `ant-mode` role and `hf/ant-mode`
slot (storm/maintenance lock, §4, §7.1), the lock tier above the arbitration ladder and
`source: lock` (§5). 2026-10-23: §7 brought up to the deployed state (hosts, power
slots, antenna-select ladder, radio, PA and IC-9700 contracts); roles `steering`,
`bandmap` and `discovery` listed in §4; `hf/beam-steer` and `hf/spots` added to §7.1.

The guiding idea: the live configuration is the documentation. A component, once
connected, describes itself to a well-known address: who it is, what it can do, and what
is currently true about it. Anyone who wants to understand the station can subscribe and
read. This document defines the grammar of that self-description so
that a component can be understood with little additional context, and so that a
GenAI-written adapter has a strict target to translate into.

---

## 1. Principles, as three planes

The three original goals can be met by one architecture when every component is
treated as a small control loop with three separate planes that never share a path.

- **Identity and capability**: who I am, what I can do. Static, published on connect.
- **State**: what is currently true about me. Live, retained.
- **Command / intent**: what someone would like to be true. Transient.

From these the goals follow:

- *Configuration is the documentation.* Self-description on the identity and state
  planes means the inventory is always read live from those planes, never from a
  separate document that falls out of date. Reference things by name and role, never
  by IP address or UDP port.
- *Change is constant; avoid tight coupling.* A component publishes state to its own
  address and receives intent at its own address. Consumers react to **state**, never
  to the fact that a command was sent. Replacing the device behind an address needs no
  change downstream.
- *Integration is cheap.* Cheap adapters stay coherent only when they follow a strict
  core. The canonical vocabulary and slot template are versioned and changed with care;
  per-vendor adapters are thin and easy to replace. Adapters stay cheap because the core
  already fixes the vocabulary and the slot template, so an adapter translates one
  vendor's protocol into them.

**Plane discipline (an easy rule to violate):** consumers couple to state, not
to intent. A command is fire-and-observe: send it, then watch state to see if it
took. Nobody assumes success because a command was emitted.

---

## 2. Naming and hierarchy

```
site / station / position / <slot>
```

- **site**: the physical property, by its common name (`muehle`).
- **station**: a transmitting entity (`hf`, `uhf`). This is the unit of contention in
  a multi-multi setup: one transmitter per band, shared-resource claims. The station
  node is a structural grouping, not a stateful component: membership is everything
  under its path, and it carries only an inferred `activity` flag (derived by the
  antenna-select reconciler from radio VFO/frequency changes, transmits, operator
  holds and live station holds of receive-only consumers such as bmrec; published as `activity` / `last_activity` on `hf/antenna-select/state`, since
  the station node itself has no topic).
- **position**: an operator seat. A single-op shack has only one, and it is left out of
  the address; it appears again only when two operators share a station.
- **slot**: a role at that position (`radio`, `pa`, `ant-switch`, ...).

A physical building that is not a transmitting entity (the Bauwagen) is a
**`location` attribute**, not a path level. A second building is then another
`location` value; promote it to a real level only if buildings become
contention-relevant (shared feedline, shared rotator).

The address is structural. The device filling a slot is an attribute of the slot, not
part of the address. An unnamed rotator is therefore still addressable as `.../rotator`.

Two rules make this enforceable:

- **The slot segment is a canonical role name (§4), never a device or product name.**
  `muehle/hf/ant-ctrl`, not `muehle/hf/ultrabeam-ctrl` (the Ultrabeam RCU-06 is the
  `device` attribute in that slot's birth certificate). A second instance of the same
  role at one station takes a numeric suffix: `ant-ctrl-2`, `rotator-2`. (Three slots
  carry role `rotator`: `hf/rotator`, and at `uhf` the sat-ops mount's `az-rotator` /
  `el-rotator`. The two `uhf` slots are the two axes of one mount, so they carry an
  axis prefix instead of a numeric suffix, §7.2.)
  Deployed deviation (owner to settle): logic, power and software-source slots are
  named for their function: `antenna-select` (role `reconciler`), `power-seq`
  (`sequencer`), `beam-steer` (`steering`), `spots` (`bandmap`), `psu-13v8` / `antennas` /
  `shack-pc-mains` (`power`), `shack-pc` (`pc-power`). `/meta.role` always carries the
  canonical role.
- **`site`, `station`, `slot`, `location`, and `host` are deployment configuration,
  never code constants.** An adapter must work for any site, station, or slot name via
  its config file alone; the only names an adapter may hard-code are the canonical
  vocabulary itself (roles, modes, field names) and facts about the device it fronts.

**Site-level infrastructure sits outside the station path.** Not every node belongs to a
transmitting station. Two non-station paths exist under the site:

- `site/host/<name>`: compute nodes (§7.3).
- `site/power/<name>`: power-distribution infrastructure that feeds *across* stations
  (a shared 13.8 V PSU that powers both the HF and UHF radios,
  the tuner, controllers, relay boards; antenna power; the shack PC mains; and the shack
  PC's own sleep/wake agent, `power/shack-pc`, role `pc-power`, downstream of its mains
  plug). These are shared upstream resources, so they do
  not live under `hf/` or `uhf/`; like `host`, they are addressed at site level. A
  station-scoped consumer (a sequencer, an operator) commands them like any other slot.

---

## 3. The slot template

Every slot (device or logic) has the same three-plane shape.

```
<site>/<station>/<slot>

# birth certificate — published on connect, retained, changes rarely
schema:        "1.0"                         # meta schema version — REQUIRED (§8.1)
role:          <role>                        # canonical role name (§4) — REQUIRED
device:        { model, serial, firmware }   # swappable; absent for logic slots
link:          <transport>                   # informational; REQUIRED for device slots
location:      <building>                    # REQUIRED, from config
host:          <compute node>                # where the adapter runs (§7.3) — REQUIRED,
                                             # from config; embedded nodes (device, adapter
                                             # and host in one) publish `embedded`
capabilities:  { ... }                       # the discovery contract — REQUIRED
expose:        { ... }                       # OPTIONAL (§3.1): the slot's consumer-neutral
                                             # observable/controllable field surface — what
                                             # fields/actions it publishes, with types/units/
                                             # options/writability/command shapes. Consumed by
                                             # HA, historians, dashboards, etc. No HA vocabulary.

# liveness — its own plane (the /status topic, §8), never a state field
status:        online | offline               # broker last-will

# state — retained, last-write-wins
ts:            <RFC 3339 UTC>                # REQUIRED on every /state snapshot (§8.1)
<field>:       <type>                         # what is currently true
device_online: bool                           # optional, device slots only: false when the
                                              # fronted hardware is unreachable while the
                                              # bridge itself is up. Form still open
                                              # (conventions/runtime-library.md, "device_online
                                              # form"): every deployed Go device bridge
                                              # publishes it explicitly (true or false); the
                                              # ESPHome ant-switch omits it
error:         string                         # optional: human-readable device fault

# intent — transient; the slot reconciles toward it or rejects
<action>:      <type>
```

Rules that hold for all slots:

- **State is retained** (last-known-value) so a late-joining consumer or logger has the
  full picture without polling.
- **Liveness via last-will, on its own topic.** `/status` changes to `offline`
  automatically when the component drops; retained state is then known-stale. Liveness
  is **never** a field inside `/state`: a dead node cannot update its own state
  document. The optional `device_online` state field is a different thing: it reports
  that the *hardware behind a running bridge* is unreachable (serial cable pulled,
  device powered off) while `/status` stays `online`.
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
| Active equipment | yes | radio, pa, ant-switch, antenna controller (`ant-ctrl`) |
| Passive resource | no (referenced by config) | fan dipole, mast, dummy load |
| Service / logic | yes | reconciler, arbiter, logger, Horst modules |
| Host / infrastructure | planned (liveness, health); not yet published, see `docs/known-issues.md` | shack PC (`bwpc`), Raspberry Pis (`shari`, `scmino`) |

A passive resource is named and referenced (e.g. by the wiring map) but has no state
of its own. One physical antenna can span both: the Ultrabeam is a passive RF resource
on a switch port **and** an active control slot that follows band and frequency.

A host is a compute node that runs adapters and services; it publishes its own
liveness so the deployment topology is documented and its failure is visible. No host
publisher exists yet (`docs/known-issues.md`, "Host-liveness nodes are model-only"). An
embedded controller (an M5 Stamp PLC) combines device, adapter, and host in a single
node: its custom firmware speaks the canonical schema directly.

**Compound devices.** One physical device may publish **several slots**, each its own
role and address with its own three planes. The slots are tied as one physical device by
carrying the **same `device { model, serial }`** in their `/meta`: that shared attribute
*is* the compound-device relationship, and no extra mechanism is needed. The function slots
carry control + semantics; a generic-surface slot (e.g. a read-only `digital-input`
mirror) may additionally expose the raw hardware truth for monitoring (§6). Example: a
single M5 Stamp PLC publishes `pa-arm` (safety arm relay) and `switch` (remote-on relays)
as two slots with identical M5Stamp-PLC identity.

---

## 3.1 The `expose` block: consumer-neutral field surface

`expose` is an OPTIONAL block on `/meta` that describes the slot's observable and
controllable field surface in a form **no specific consumer owns**. It answers "what
fields/actions does this slot publish, and which are settable?" once, in the birth
certificate, so that *any* consumer (Home Assistant, a historian like InfluxDB, a
dashboard, Node-RED, Prometheus) can render its own representation without hardcoding
per-role knowledge. This is part of what lets §9 hold: HA is one thin edge adapter
against the same surface as Grafana or Node-RED, and the bus never depends on any of
them.

`expose` carries **no consumer vocabulary** (no `device_class` strings, no Jinja
templates, no `payload_on/off`). Those belong to the consumer. The block describes only
neutral primitives: field `type` (`number`/`enum`/`boolean`/`string`), `unit`, a semantic
`class` hint, enum `options` (inline or by `options_ref` into `capabilities`), `writable`,
a `command` descriptor for how a write is encoded on `/cmd`, and `min/max/step` for
setpoints. `actions` describe one-shot buttons. The full schema is in Appendix C.

A slot that omits `expose` is not discovered by `expose`-driven consumers; its
canonical planes are unaffected. Logic slots omit `device.model` (and may omit `device`
entirely).

```json
"expose": {
  "device":  { "name": "...", "model": "...", "manufacturer": "...",
               "sw_version": "...", "area": "..." },
  "fields":  [ { "key": "freq_hz", "name": "Frequency", "type": "number",
                 "unit": "Hz", "class": "frequency", "state_class": "measurement" },
               { "key": "band", "name": "Band", "type": "enum", "options_ref": "bands",
                 "writable": true,
                 "command": { "action": "set_band", "value_key": "value", "value_type": "string" } } ],
  "actions": [ { "key": "retract", "name": "Retract", "command": { "action": "retract" } } ]
}
```

---

## 4. Canonical vocabulary

- **Frequency in Hz is the single source of truth.** `band` is derived from `freq_hz`
  and published for convenience. On slots that carry `freq_hz` there is no band *setpoint*
  on `/state`: `band` is never a stored value, always derived. The PA (`hf/pa`) has no
  frequency on the bus: its `band` is the amplifier's own band selection (`band_source`),
  set via `set_band`. A radio *may* accept a `set_band` `/cmd` input for
  native band-stacking (the radio restores its own persisted per-band frequency and the
  bridge republishes that `freq_hz` with `band` derived from it), but the canonical tuning
  intent is still `set_freq_hz`: commanders set a frequency and the band is derived from
  it. This is meant to avoid the class of bug where band and frequency disagree
  because two things set them independently.
- **Bands.** The canonical vocabulary owns the band name→frequency-range table (edges are
  region- and license-dependent; these are DL / IARU R1 except 160 m, 80 m, 40 m and
  6 m, which use the wider ITU Region 2 edges, see `conventions/band-mode-reference.md`).
  A device declares the band *names* it supports and references this table rather than
  re-stating edges. The table lives in `conventions/band-mode-reference.md`. A frequency
  outside every allocation derives `gen` (HF general coverage, 1.8–30 MHz) or `unknown`
  (anything else, including 0 Hz, e.g. a radio with no slice yet); no synthesized band
  names.
- **Modes:** `cw`, `usb`, `lsb`, `am`, `fm`, `data`. These are canonical names; firmware
  typically uses proprietary variants (`CW-U`, `CW-L`, `DIGU`, `DIGL`, `USB`, `LSB`,
  `FDV`, `RTTY-U`, …). **Normalization is the adapter's responsibility**: consumers see
  only the canonical names above. Adapters must publish a canonical mode or omit the
  field; they must never publish a raw firmware mode string.
- **Roles:** `radio`, `pa`, `tuner`, `ant-switch`, `ant-ctrl` (controller that tunes or
  steers a passive antenna resource - the antenna itself remains a passive resource),
  `rotator`, `pol-ctrl`, `preamp` (function; may be a slot or a declared capability),
  `bias-feed` (likewise), `reconciler`, `host`, and the power-control roles:
  `power` (a supply switch: smart plug / contactor cutting mains or DC to a bank; one
  on/off, declares `feeds`), `switch` (a generic multi-channel relay switch of
  non-exclusive booleans; each channel declares its `kind`: `remote_on`, `enable`, or
  `spare`; distinct from `ant-switch`, the exclusive 1-of-N selector), `pa-arm` (the
  PA-enable arm relay; safety, heartbeat-driven, fail-safe-open), and `sequencer` (a
  logic slot that runs an ordered, delay- and confirmation-based startup/shutdown over
  other slots' `/cmd`), `sat-track` (the satellite a tracker application is
  following: identity, look angle, derived sub-satellite point; read-only, fronts
  software, not hardware), `ant-mode` (a logic slot owning the station antenna mode
  `normal | storm | maintenance`; its `/state.lock` is read by the enforcing bridges,
  §7.1), `pc-power` (a computer's own sleep/wake agent, e.g. the shack PC at
  `power/shack-pc`; offline = asleep; wake is Wake-on-LAN, not MQTT), `steering` (a
  logic slot that turns a logger bearing into rotator and antenna-direction intents,
  `hf/beam-steer`), `bandmap` (the operator-keyed station from the shack logger,
  `hf/spots`; read-only, fronts software) and `discovery` (a consumer that renders
  `/meta` `expose` into a consumer's discovery format, `hf/discovery`). Passive:
  `ant/*`, `mast/*`, `preamp/*` (masthead LNA).
- **Capability keys** are role-specific; each slot's block in §7 and its component's
  `*-mqtt-api.md` are authoritative. Examples: `bands`, `modes`, `receivers`,
  `diversity`, `amp_key`, `tune`, `bias_t`, `band_source`, `rf_sample`, `alc_out`,
  `max_power_w`, `key_input` (pa), `hot_switch`, `ports`, `off`, `exclusive`, `axes`,
  `limits`, `deadband` (rotator), `directions` (ant-ctrl), `inline`, `tune_modes`
  (tuner), `polarizations`, `feeds` (a `power` slot's downstream slot addresses),
  `channels` + `kind` + `relay_map` (a `switch` slot's relay channels), `fail_safe`
  (`open` | `off` | `on`: for a `pa-arm` relay its de-energized/safe state, `open`; for
  a `power` slot the plug's power-on default after a mains blip - the three Mühle
  Shelly plugs publish `on`, the PSU plug by returning to its last state),
  `heartbeat` (bool), `controls`/`watches` (sequencer), `ladder`/`follows`
  (reconciler).
- **`band_source`** (PA): how the amplifier's band is determined, one of `cat` (band data
  fed from the radio over a CAT link), `rf_sense` (the amp auto-switches by sensing
  the RF drive), or `manual` (only via `/cmd set_band`). Distinct from `rf_sample`
  (whether the amp independently samples/reports RF power).

**Multi-receiver radios.** The `receivers` capability declares how many independent
receive paths a radio provides (SDRs, contest transceivers, and multi-receiver rigs may
have two or more). `radio/state` is always the **active TX receiver**, one fixed address
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
A simpler device does not declare the capability, and you add a slot to fill the gap;
nothing downstream changes.

**Wiring map:** the single editable place antenna arrangement lives. It maps switch
ports to named passive resources. Devices never contain it; the reconciler reads it.
Passive-resource names (`ultrabeam`, `fan-dipole`, …) are free-form site-local
identifiers: they appear **only** in site configuration, never in adapter or
reconciler code.

**Controller map:** the wiring map's companion. A passive resource MAY have an active
controller slot that tunes or steers it (the Ultrabeam has an `ant-ctrl`; the fan
dipole has none). The reconciler's config binds a resource to its controller slot
(antennaselect: `[band_follow] resource = "ultrabeam", slot = "ant-ctrl"`, one binding
today; the PA band-follow and the ATU in-line follow are separate bindings,
`[pa_follow]` and `[tuner_follow]`), and band/frequency-follow is emitted to whatever
slot the binding names; `/meta` `capabilities.follows` publishes the bindings. This
keeps follow logic generic, since which antenna follows the radio is set only in site
configuration.

---

## 5. Mechanism A: reconciler and the arbitration ladder

Actuators are dumb; policy lives in a reconciler/arbiter. When more than one commander
wants one actuator, a single arbiter resolves them by priority and emits one intent
stream. The actuator receives one command source and reports its state.

**Priority ladder (highest asserting tier wins):**

```
0 manual:    operator stand-down ("manual") → no target, no move, no follow (source operator)
1 idle:      station inactive               → safe default (off / ground)
2 operator:  active hold (dummy, forced)    → that selection
3 auto:      policy(state)                  → resolved selection
```

Tier 0 outranks idle: in manual the antenna is never moved automatically, idle
grounding included (an operator who wants it grounded requests `off`).

Above the whole ladder sits the **antenna-mode lock** (`hf/ant-mode`, §7.1): while the
mode is `storm` or `maintenance` the arbiter targets `off` with `source: lock`, even
under antennaselect's manual stand-down. Entering a locked mode is itself an explicit
operator decision (or the unmanned safety action), so it outranks "never move
automatically".

- `mode` is **derived**: `manual` while an operator hold is active, else `auto`. No
  separate auto/manual switch is needed: the presence of a hold is manual mode.
- `source` (`lock | idle | operator | auto`) is published so the live config records the
  reason the actuator is in its current position, in addition to the position itself.

This arbiter is the multi-multi contention mechanism at small scale. The arbiter that
ranks idle over operator over auto for one operator can later rank two stations
contending for one shared antenna, using the same ladder with more inputs.

---

## 6. Mechanism B: the soft/hard straddle

Safety, interlock, and sequencing do **not** live on the loosely-coupled messaging
plane. The messaging plane sets desired state and reports actual state; a hardware
path enforces safe state during transitions and fails safe.

- **Hardware series interlock.** A physical TX-inhibit line runs in series through the
  relevant components; any open link inhibits transmit. The path is local and
  fast, and it fails safe. The messaging layer **mirrors** each link as read-only state
  for documentation and monitoring, and never sits in the enforcement path.
- **Fail-safe defaults.** A relay's default and loss-of-signal state is the safe one
  (PA isolated / TX inhibited). Loss of software, network, or power therefore leaves the
  equipment in that safe, inactive state.
- **Software arm, not software gate.** Where there is no hardware trigger for a hazard
  window (e.g. tuning), express the software contribution as a slowly-changing *arm*
  permit combined by hardware AND with the fast hardware key line. Normal transmit
  timing is determined by the hardware edge; software only arms/disarms. The arm permit
  is heartbeat-driven: loss of heartbeat drops the arm.
- **Ordering contracts.** Some intents carry a required order:
  - *Tune:* disarm the PA and confirm before the tune carrier. Because there is no
    hardware tune line, route the TUNE action through the automation so disarm leads.
    (Partly realized: the `pa-arm` firmware's hazard gates open the arm relay while the
    radio or the tuner reports a tune cycle, §7.1. An ATU `tune` on `hf/tuner/cmd` raises
    `settling` before the carrier, so that disarm leads. A TUNE started on the radio is
    not routed through the automation (flexbridge has no `tune` action), so the gate
    opens only after the radio reports `tuning`. There is no confirm step.)
  - *Port change:* a cold-switch-only switch (`hot_switch: false`) must have RF
    inhibited and RX confirmed before the port moves, auto or manual.
  - *Station power-up/-down:* the supplies and remote-on triggers must come up in an
    order with delays and liveness confirmations: the shack PC mains and the 13.8 V
    supply first (the site master mains is out of scope and never switched), the 13.8 V
    supply before the controllers that boot from it can actuate anything, the radio
    before the PA, the PA powered before the arm permit. This
    ordering lives in a `sequencer` logic slot (§7.1), not in the device bridges; the
    bridges only report state. The sequencer is one writer of the power slots but does
    not lock them, so a channel stays directly toggleable for troubleshooting.

---

## 7. Site instantiation: Mühle

Two stations, both `location: bauwagen`. Each station node is structural (a path
prefix over its members) and carries one field only:

```
muehle/hf   |   muehle/uhf
role:     station
kind:     { hf | uhf }
location: bauwagen
activity: { active | inactive }    # inferred by antenna-select from radio VFO/TX, operator
                                   # holds and station holds (hf/recorder); published as hf/antenna-select/state.activity
                                   # (+ last_activity); read by ladder tier 1 and the powerseq
                                   # idle standby / idle shutdown
```

Everything else a station might report (band, tx, readiness) already lives on the radio;
deriving it here would only duplicate. The switch's fail-safe-to-ground default covers
power loss independently of this flag. The station node is conceptual: no `muehle/hf`
topic is published.

### 7.0 Power infrastructure (site level)

The upstream supplies feed across both stations, so they sit at site level
(`site/power/<name>`, §2), not under `hf/` or `uhf/`. All three are Shelly smart plugs,
formerly operated only through Home Assistant; brought onto the canonical bus by one
`shelly-power-bridge` on `scmino` (since 2026-10-18; a compound bridge, i.e. one process
with three slots: `psu-13v8`, `antennas`, `shack-pc-mains`). HA is
thereafter one writer of these topics and no longer the control path (§9).

The site **master mains** is not a slot: it is an Athom ESPHome plug owned by Home
Assistant, and it also powers the Pis and the network, so StationA never switches it
(out of scope, 2026-10-18).

**`muehle/power/psu-13v8`**: Shelly plug, 13.8 V PSU
```
role: power;  device: { model: "Shelly Plug S" (Gen1), serial };  link: wifi;  host: scmino
capabilities: fail_safe {on}; feeds [hf/radio, uhf/radio, hf/tuner,
              hf/ant-switch, hf/switch, hf/pa-arm]   # what this supply powers
state:        power {on|off}; device_online (Shelly reachability); error (omitempty)
intent:       set_power {on|off}             # /cmd retained
```
The 13.8 V supply (the TRX PSU) is upstream of the HF radio, the UHF radio, the tuner,
the ant-switch, **and the M5 Stamp PLCs**, so it must be on before any of them can
actuate or report. The sequencer (§7.1) waits for the PLC (`hf/switch`) before the
remote-on relays are commanded.

**`muehle/power/antennas`**: Shelly plug, antenna power (2026-10-10)
```
role: power;  device: { model: "Shelly Plug S Gen3", serial };  link: wifi;  host: scmino
capabilities: fail_safe {on}; feeds [hf/ant-ctrl, hf/rotator, uhf/az-rotator, uhf/el-rotator]
state:        power {on|off}; device_online (Shelly reachability); error (omitempty)
intent:       set_power {on|off}             # /cmd retained
```
Powers both rotators (HF and UHF) and the Ultrabeam RCU-06. The sequencer switches it on
in the cold start, off in shutdown, idle standby and idle shutdown (§7.1 power-seq), and
back on at wake.

**`muehle/power/shack-pc-mains`**: Shelly plug, shack PC (bwpc) mains (2026-10-18)
```
role: power;  device: { model: "Shelly Plug S Gen3", serial };  link: wifi;  host: scmino
capabilities: fail_safe {on}; feeds [power/shack-pc, uhf/sat-track]
state:        power {on|off}; device_online (Shelly reachability); error (omitempty)
intent:       set_power {on|off}             # /cmd retained
```
The mains supply under `power/shack-pc`. The sequencer switches it on in the cold start
(then Wake-on-LAN boots the PC) and **never off**, neither in shutdown nor in standby,
because cutting mains powers Windows off without a clean shutdown. Off is a manual
(confirmed) console switch.

**`muehle/power/shack-pc`**: the shack PC bwpc (Windows), `winpc-power-bridge`
```
role: pc-power;  link: ethernet;  host: bwpc
state:        power {awake}; user_idle_s; device_online; error (omitempty)
intent:       sleep                          # /cmd NOT retained (a retained sleep would
                                             #  replay at wake)
```
Offline (`/status`) is normal: the PC is asleep. The agent refuses `sleep` while the
operator has used keyboard/mouse within its threshold. Wake is Wake-on-LAN from the
sequencer (a sleeping PC has no MQTT session, so there is no MQTT wake command).

### 7.1 Station `hf`

**`muehle/hf/radio`**: FLEX-8400, ethernet
```
capabilities: bands [160m..6m, by name]; modes [cw,usb,lsb,am,fm,data];
              receivers 1; diversity false; amp_key true; tune true; bias_t false;
              rx_inputs [ant1,ant2,rx_a]; tx_outputs [ant1,ant2]
state:        freq_hz (Hz int); band(derived); mode (canonical); tx {rx|tx};
              tuning (bool); drive (0-100); device_online (radio link liveness);
              dvk_status {idle|recording|preview|playback|disabled}; dvk_id (1-12, active memory);
              dvk_memories ([{id,name,duration_ms}], the DVK library as named in SmartSDR;
              /state-only, omitted until reported);
              mic_profile (active name, client-side tracked; empty until first load);
              mic_profiles (available names, sorted; /state-only, from `profile mic info`);
              rx_input (omitempty, adapter work pending)
              — always the active/TX receiver
intent:       set_band {label}; set_mode {cw|usb|lsb}; dvk_play_<N> / dvk_play {1..12};
              dvk_stop {id|active}; set_mic_profile {name}   # /cmd NOT retained (one-shot)
```
The role's other intents (`set_freq_hz`, `set_drive`, `select_rx`, `tune {start|stop}`)
are not implemented by flexbridge, which rejects them as unknown actions; ATU tuning is
the `hf/tuner` slot's own `tune` action.
`set_band` drives SmartSDR **native band-stacking**: the bridge sends
`display pan s <pan_handle> band=<wavelength>` (the wavelength is the band number,
e.g. `20m`→`20`), and the radio restores the last-used frequency/mode for that band. `band`
remains a derived `/state` field (§4): after a `set_band` the bridge republishes the
radio's tuned `freq_hz` with `band` derived from it, so band and frequency can never
disagree. Only the FLEX-8400's regular bands (`160m` to `6m`) are supported; XVTR bands
are out of scope.
DVK (Digital Voice Keyer) is a SmartSDR v4+ / SmartSDR+ feature: 12 voice memories keyed
to TX on playback. flexbridge is otherwise read-only; `set_band`, `set_mode` (`cw`/`usb`/
`lsb` only: the caller picks the sideband; added 2026-10-23, wire command not yet
confirmed live), DVK, and mic profiles are its only `/cmd` surfaces: DVK exposed as
one-shot actions (`dvk_play_N` buttons + `dvk_stop`), `set_band` as a writable `band`
setpoint (a select), `set_mic_profile` as a writable `mic_profile` setpoint; `set_mode`
is `/cmd`-only (not in `expose`, where `mode` stays read-only) and applies to the TX
slice (else the active one). All are observed on `/state`: `freq_hz`/`band`/`mode` for
`set_band`, `mode` for `set_mode`, `dvk_status`/`dvk_id` for DVK, `mic_profile`/
`mic_profiles` for mic profiles. Voice modes only; the radio refuses DVK in cw/data.
Mic profiles use SmartSDR's **native** `profile mic load "<name>"`; the radio is the
single source of truth and the bridge keeps no preset layer. `set_mic_profile` loads a profile.
There is **no save** (`profile mic save` is obsolete on SmartSDR v4+, returning malformed;
profile creation uses a file-transfer mechanism out of scope). The available profile list is
queried via the one-shot `profile mic info` command (sent once in the handshake) and
published on `/state.mic_profiles` (a `/state`-only dynamic field, not in `expose.fields`);
SmartSDR does not report an active mic profile, so `/state.mic_profile` is tracked
client-side as the name most recently loaded via `set_mic_profile` (best-effort, empty until
the first load via the bus).
The FLEX-8400 supports up to 4 simultaneous receive slices (a SmartSDR concept). These
map generically to receivers. With `receivers 1` declared, only `radio/state` is
published. If a multi-slice configuration is added later, promote the declaration to
`receivers N` and publish per-receiver state at `radio/receiver/N/state`.

**`muehle/hf/pa`**: ACOM 1200S, serial (USB-serial; carries band/mode/telemetry, NOT keying)
```
capabilities: bands [...]; max_power_w <spec>; band_source rf_sense; rf_sample false;
              key_input hardware; alc_out true; modes [operate,standby]
state:        mode {operate|standby}; band; keyed {rx|tx|inhibited};
              fwd_power_w; rfl_power_w; temp_c; swr; fault {none|swr|temp|reflected|other};
              pa_state (raw firmware mode, diagnostic); device_online; error (fault message);
              power {on|off}              # read-only telemetry now
intent:       set_band; set_mode {operate|standby}   # /cmd NOT retained; no clear_fault
                                                     # (the ACOM protocol has none)
```
The PA's *own* power-on control line (a soft "remote power-on" input) is operated by a
relay on `muehle/hf/switch` (§7.1), not by the PA bridge: the ACOM's RTS wake-line path
was removed (it never worked). The PA slot only **observes** power: `power` is the
*actual* state from telemetry (`off` only when the amp reports OFF); it is no longer a
setpoint. Power-on/off of the PA is a power-distribution-layer concern; `pa.power`
reflects the result. (The `set_power`/RTS extension is retired; see §7.1 `hf/switch`.)

**`muehle/hf/ant-switch`**: wifi 1-to-6 (dumb actuator)
```
capabilities: ports [1,2,3,4,5,6]; off true; exclusive true; hot_switch false
state:        selected {off|port1..port6}; settled
intent:       select {off|port1..port6}
```

**`muehle/hf/tuner`**: ATR-1000 (BTR-1000 / N7DDC design), wifi (binary WebSocket)
```
capabilities: inline true; tune_modes [mem, full]
state:        inline (bool); swr (float, ratio); fwd (W, forward power);
              l_uh (inductance µH, diagnostic); c_pf (capacitance pF, diagnostic);
              settling (bool); fault (string, omitempty); device_online (bool)
intent:       set_inline {true|false}; tune {full|mem}
```
The ATU is engaged in-line only when its served resource is the resolved antenna AND the
band is non-resonant (e.g. 30/60/80/160 m on the fan-dipole); it is bypassed otherwise. The
`set_inline` intent is driven by `antenna-select` (see the soft binding below), gated on
radio online + a known band, and engages only while the tuner's resource is the resolved
target; the reconciler's cold-switch sequencing already withholds a port change during
TX, so the ATU is not re-keyed mid-TX.

**`muehle/hf/rotator`**: Yaesu G-450DC. Control path: AF6SA WRC, with bridge software
(`wrc-rotator-bridge`) on `scmino` (since 2026-10-12; its GS-232 :7373 and PstRotator
:12040 listeners moved with it) presenting it to the automation.
`capabilities: axes [az]`.

**`muehle/hf/ant-ctrl`**: antenna controller (role `ant-ctrl`; device: Ultrabeam
RCU-06), bridge (`ultrabridge`) on `scmino` (since 2026-10-09; RCU-06 USB-serial on
scmino). Controls the `ant/ultrabeam` passive resource: follows band
and frequency; reports element/tuning state.

**`muehle/hf/rx-loop-ctrl`**: K9AY controller (no component or bridge yet, and no
field layout, §12). Reports loop direction; the loop preamp
is a sub-state of this slot and is dropped by the hardware TX-low line.

**`muehle/hf/switch`**: M5 Stamp PLC #1, remote-on relays (compound with `pa-arm`;
same M5Stamp-PLC `device`). wifi, embedded.
```
capabilities: channels [pa, trx]; exclusive false; kind {pa: remote_on, trx: remote_on};
              relay_map {pa: 3, trx: 4}
state:        pa {on|off}; trx {on|off}        # relay positions, read back from relays 3 & 4
intent:       set_pa {on|off}; set_trx {on|off}   # /cmd retained (idempotent)
```
The PA and TRX have pronounced "remote power-on" *control lines* (soft triggers, not
mains cuts); this slot operates them via relays 3 (PA) and 4 (TRX). Closing a relay
asserts the device's remote-on; opening it releases the trigger and the device
soft-shuts. Powering the PA is a power-layer concern, not the PA slot's (see `pa`
above); `pa.power` telemetry reflects the result. Relay 1 of the same PLC is the arm
relay (`pa-arm`); relay 2 is spare.

**`muehle/hf/pa-arm`**: M5 Stamp PLC #1, the PA-enable arm relay (relay 1; compound with
`hf/switch`). Embedded safety node: device, adapter, and host in one (the formerly
planned M5Stack, §11.3, now realized as the M5 Stamp PLC).
```
capabilities: fail_safe open; heartbeat true; relay 1
state:        enabled bool; armed bool; device_online bool; block string (omitempty);
              error string (omitempty); cmd_error string (omitempty)
              # enabled = software arm-permit (last /cmd or PLC button A); armed = relay
              #   readback, closed only when enabled ∧ every interlock gate clear (radio,
              #   band, antenna switch, Ultrabeam, PA, ATU) — fail-safe-open; table in
              #   m5stamp-hf-ctrl/docs/m5stamp-hf-ctrl-mqtt-api.md §4
intent:       set_enabled {true|false}         # /cmd retained (steady-state permit)
```
Arm decision is the embedded firmware's: it subscribes `hf/radio` (`/status` + `/state`:
`device_online`, `tuning`, `tx` and `freq_hz`, from which it derives the band) plus
`/status` + `/state` of `hf/ant-switch`, `hf/ant-ctrl`, `hf/antenna-select`, `hf/pa`
and `hf/tuner`, and `hf/ant-ctrl/cmd`, and computes `armed`; `armed` is never commanded
directly (§6 software-arm-permit AND hardware key). Heartbeat is own liveness plus
radio reachability: radio offline ⇒ `armed` false; loss of 13.8 V ⇒ relay drops open
(fail-safe). An operator/sequencer only sets the `enabled` permit.

**`muehle/hf/power-seq`**: the station startup/shutdown sequencer (logic slot, `powerseq`
on `scmino`, since 2026-10-18).
```
role: sequencer  # logic slot, no device
subscribes: /status of every slot a step references (power/*, hf/switch, hf/radio,
            hf/pa, …); hf/pa/state.power; hf/antenna-select/state (activity,
            last_activity); /state of the idle watches (uhf/radio, uhf/az-rotator,
            uhf/el-rotator); hf/ant-mode/state (power_hold)
state:      phase {idle|starting|running|stopping|entering_standby|standby|waking};
            step <string>; fault (omitempty);
            stop_reason {operator|idle_shutdown} (omitempty)
intent:     start | stop                       # operator one-button; /cmd NOT retained
```
Startup (confirmations + delays, cold start 2026-10-10, master mains dropped
2026-10-18): `power/shack-pc-mains` on → `power/psu-13v8` (TRX PSU) on → 5 s → Wake-on-LAN
to bwpc → wait `hf/switch` `/status` online
(`hf/switch/cmd` is one retained topic: `set_trx` + `set_pa` published while the PLC
boots would collapse to `set_pa`) → `hf/switch` trx on → 1 s → `hf/switch` pa on →
`power/antennas` on → wait `hf/radio` `/status` online → wait
`hf/pa` `power` on (the slot's `/status` must also be online, and its
`/state.device_online` must not be `false` when the field is present, so neither a dead
bridge nor a dead device passes the wait on a stale retained `/state`) → `hf/pa-arm`
set_enabled true (arms when safe). Shutdown is the reverse, with short staggers for
inrush, and leaves `power/shack-pc-mains` on. The sequencer is **one writer** of these slots but does not lock them: any
channel stays directly toggleable for troubleshooting while the sequencer is idle.

**Idle standby.** From `running`, when `hf/antenna-select` reports `activity=inactive`
(it has grounded the antenna after its idle timeout), no idle watch (UHF radio
frequency, UHF rotator position) moved, and `running` has lasted the idle timeout →
`entering_standby`: `power/antennas` off, `power/shack-pc` sleep → `standby`. A
`last_activity` newer than the standby entry (VFO change, TX, operator hold, station hold), a watch
moving, or `start` → `waking`: Wake-on-LAN to bwpc, `power/antennas` on, wait for
`hf/ant-ctrl` + `hf/rotator` online → `running`. antennaselect reconnects the antenna
on the same activity. UHF use counts as activity because the antenna plug also feeds
the UHF rotators and bwpc runs the satellite tracker.

**Idle shutdown.** A station still in `standby` 2 h (`[idle] shutdown_minutes`) after
the last activity (antennaselect `last_activity`, an idle watch, the last start/wake)
runs the shutdown list plus a bwpc sleep → `idle` with `stop_reason=idle_shutdown`.
The PSU, FLEX and PA are then off; only an operator `start` (full cold start) brings
the station back.

**Antenna-mode hold.** While `hf/ant-mode/state.power_hold` is true (below), powerseq
runs no idle transition (standby, wake, idle shutdown) and treats watch changes as a
re-baseline, not activity; operator `start`/`stop` still work. When the hold ends it
starts a fresh settle window and restarts the idle-shutdown clock.

**Restart.** powerseq keeps its phase across a restart (top-level `state_file`, default
`/var/lib/powerseq/state.json`, "" = off; 2026-10-22). `idle`, `running` and `standby`
come back as they were, with the standby and idle-shutdown clocks. A sequence killed
mid-way (`starting`/`stopping`/`entering_standby`/`waking`) comes back as `idle` with
`fault` "<step>: interrupted by a powerseq restart (<phase>)" and is not re-run. A
missing or corrupt state file gives `idle`. A restart never emits a `/cmd`.

This sequence is **config-driven** in `powerseq` (the `[[startup]]` / `[[shutdown]]` /
`[[standby]]` / `[[wake]]` / `[[idle_shutdown]]` step lists in its TOML, each step `cmd` / `wait_status` /
`wait_state` / `delay` / `wol`); the
text above is the default shipped in `config.example.toml`. The
subscribed topics and the `/meta` `controls`/`watches` are derived from the configured
sequence, so a different setup can define a different sequence without a Go change.

**`muehle/hf/antenna-select`**: reconciler/arbiter (logic slot)
```
subscribes: hf/ant-mode/state (lock), radio.{band,tx,freq_hz,device_online}, radio/status,
            operator.request (own /cmd), ant-switch.selected,
            ant-ctrl/state (re-extend after unlock)
            (activity is inferred: a freq_hz change or tx == "tx" on a live radio link, or an
            operator hold (any non-auto /cmd request, whatever the link state), marks active;
            an idle timeout with none of them marks inactive)
emits:      ant-switch.select; <controller>.cmd frequency (freq follow, per band_follow;
            retained); pa.set_band (pa_follow); tuner.set_inline (tuner_follow)
config:     wiring_map  { port1: dummy-load, port4: ultrabeam, port6: fan-dipole, off: grounded }
                        # port4 as seeded (antennaselect config/deploy, PLC ULTRABEAM_PORT,
                        # console); on-device confirmation still open — docs/known-issues.md
            band_follow { resource: ultrabeam, slot: ant-ctrl }   # resource → controller slot
            band_policy { 6,10,12,15,17,20m: ultrabeam;
                          30,40,60,80m:     fan-dipole }   # 30/60/80/160m require the ATU
            priority   { L lock (hf/ant-mode storm|maintenance), 0 manual-standdown,
                         1 idle, 2 operator, 3 auto }
state:      mode {auto|manual}; target {off|port1..port6|""}; source {lock|idle|operator|auto};
            activity {active|inactive}; last_activity (RFC 3339, absent until the first activity)
            # target "" = no target: hold the last selection (manual stand-down, radio offline
            #   or unknown band, a lock not yet grounded)
```

**`muehle/hf/ant-mode`**: station antenna mode (logic slot, `antmode` on `scmino`;
contract: `antmode/docs/ant-mode-mqtt-api.md`)
```
role:       ant-mode   # logic slot, no device
subscribes: hf/power-seq/state (presence), hf/rotator, hf/ant-ctrl, hf/ant-switch,
            hf/radio, power/antennas; polls NINA (DWD warnings by ARS) and DWD
            stationOverviewExtended (day wind direction/gust)
state:      ant_mode {normal|storm|maintenance}; source {manual|auto};
            lock {seq, ground, retract, park_az, nudge} (only when locked);
            phase; power_hold; power_borrow; presence; wind; warnings;
            storm_suggested
intent:     set_ant_mode <mode> | repark | dismiss_suggestion   # one-shot, cleared
```
Storm and maintenance ground the antenna, retract the Ultrabeam and park the HF rotator
(storm: boom broadside to the DWD wind direction, parked once; maintenance: a preset
azimuth for lowering the mast). **The lock is enforced in the bridges, not here**:
`hf/rotator` and `hf/ant-ctrl` read `/state.lock` (via `shared/antmode`), refuse motion on
every inbound path (MQTT, GS-232, PstRotator, HTTP) and execute the park themselves; a
locked snapshot stays enforced while antmode is offline. `hf/ant-ctrl/state.retract_seq`
names the lock `seq` whose retract finished (the persisted `retracted` latch may be left
from an earlier lock). antmode's `retracting` step leaves only on `hf/ant-ctrl` live,
`retract_seq` = `lock.seq`, `retracted` and not `moving` (a stale latch never ends it),
or on the retract timeout (the fallback for an unpowered RCU-06 or an older bridge). The
park itself (wrc-rotator-bridge) and the retract latch (ultrabridge) are persisted
under each bridge's `state_dir`, so a bridge restart neither repeats a finished park
nor forgets the retract. A DWD wind warning (level 1
"Windböen" and up) enters storm automatically only while the station is unmanned
(powerseq not `starting`/`running`/`waking`, after a 60 s dwell; an `idle` that follows
a manned phase without `stopping` (an aborted or killed sequence, which powerseq
reports as `idle` with a `fault`, or a lost state file) stays manned while `hf/radio` is
live, until 30 min after the later of that `idle` and the last TX) and
`auto_enter` is on (off for the observe-only rollout); otherwise it is a suggestion.
`power_hold` tells `hf/power-seq` to hold its idle logic (no standby, wake or idle
shutdown; watch changes re-baseline): while antenna power is borrowed for a park, for
the whole maintenance, and in storm until the park is done or failed. Operator
`start`/`stop` still work. `power_borrow` (not `power_hold`) says the power is borrowed.

**`muehle/hf/beam-steer`**: smart rotation (logic slot, `beamsteer` on `scmino`;
contract: `beamsteer/docs/beam-steer-mqtt-api.md`)
```
role:       steering   # logic slot, no device
input:      PstRotator UDP :12050 (the contest logger's rotator output)
subscribes: hf/rotator, hf/ant-ctrl, hf/radio, hf/ant-mode/state
emits:      hf/rotator set_az | stop (not retained); hf/ant-ctrl direction (retained, ts-stamped)
state:      enabled; inputs; lock (omitempty); pending (omitempty); last
intent:     enable | disable (retained) | aim <bearing> (not retained)
```
Turns a logger bearing into the cheapest way to put an Ultrabeam lobe on the station: flip
the Ultrabeam 180° when the station is behind the beam, else rotate to the nearer lobe;
with the toggle off, requests pass straight through to the rotator. Stands down while
`hf/ant-mode` is locked.

**`muehle/hf/spots`**: the operator-keyed station from DXLog/Log4OM (a software
source, `logger-spot-bridge` on `scmino`, role `bandmap`; read-only, no `/cmd`).
`state: ts; device_online (a logger keyed something recently); selected (omitted when
nothing is keyed)`.

**`muehle/hf/discovery`**: HA discovery consumer (logic slot, `hadiscovery`; see §7.3, §9)
```
subscribes: <site>/+/+/meta  (reads each slot's consumer-neutral `expose` block, §3.1)
emits:      <discovery_prefix>/...  (Home Assistant discovery config; no /cmd, no slot state)
host:       scmino
```

**Passive resources:** `ant/ultrabeam` (port 4 as seeded; on-device confirmation still
open, see `docs/known-issues.md`), `ant/fan-dipole` 80/40 (port 6),
`ant/dummy-load` (port 1), `ant/k9ay-loop` (RX, via rx-loop-ctrl), `mast/ta16-hf`.

**Adapters:** all HF bridges run on host `scmino`: `radio` (flexbridge), `tuner`, HF
`rotator` and `pa` since 2026-10-12, and `ant-ctrl` since 2026-10-09; `pa-arm` + `switch`
are a self-hosted M5 Stamp PLC and `ant-switch` is an ESPHome board. The Shelly power
bridge and the `power-seq` sequencer run on `scmino` (since 2026-10-18). See §7.3. The PA
bridge talks to the ACOM 1200S over USB-serial (Prolific, 9600 8N1) for telemetry,
`/cmd set_band` band-walk commands (sent by the antenna-select PA band-follow, on in the
seeded config (`[pa_follow] enabled = true`; off in the built-in defaults), or by an
operator) and `set_mode`. There is no CAT band-data cable in this
adapter; the amp auto-bands by sensing the RF drive (`band_source: rf_sense`), so
band-follow is hardware RF-sense by default: the `pa.set_band ← radio.band` soft
binding below is an optional software override / pre-position path.

**Soft bindings (reconciler, state → intent):**
- `pa.set_band` ← `radio.band`
- `tuner.set_inline` ← `band_policy` (engage the ATU in-line when the selected resource is
  its resource and the band is non-resonant; bypass otherwise; this closes the §10 residual)
- `ant-ctrl.frequency` ← `radio.freq_hz` (via `band_follow`; only while the Ultrabeam is
  the resolved target; suppressed under the `hf/ant-mode` lock and in manual)
- `ant-switch.select` ← arbiter(band_policy, wiring_map, ladder)
- `ant-switch.select` → `off` when the idle timeout elapses with no VFO change or TX
  and no live station hold (`hf/recorder/state.station_hold`, bmrec's HF lab while it
  records; expiring, see `antennaselect/docs/antenna-select-mqtt-api.md`)
  (ladder tier 1; walk-away lightning protection), except under an operator `manual`
  stand-down (tier 0): in manual the antenna is **never** moved automatically, and idle
  grounding is itself an automatic move
- `ant-switch.select` → `off` while `hf/ant-mode/state.lock.ground` (lock tier, above the
  manual stand-down); `ant-ctrl` and `tuner` follow suspended while locked (PA
  band-follow continues)
- `power-seq` idle logic held ← `hf/ant-mode/state.power_hold`; `power/antennas.set_power`
  ← antmode power borrow (only when `[power] borrow` is on, default off)
- `pa-arm.armed` ← `enabled ∧` all interlock gates clear for 1 s (an Ultrabeam move's own
  timed windows excepted), closed only in RX
  (realized in the M5 Stamp firmware, not the reconciler; fail-safe open). Hazard gates
  open it at once even in TX: radio lost/stale/tuning, band not PA-safe, antenna
  grounded or switch relays travelling, Ultrabeam moving / commanded / retracted /
  off-frequency (while in circuit), PA fault, ATU tune. Readiness gates block closing and
  open it only in RX: band-change hold-off, Ultrabeam settle, reconciler move pending, a
  supervising feed offline. Full table: `m5stamp-hf-ctrl/docs/m5stamp-hf-ctrl-mqtt-api.md` §4
- `power-seq.{start,stop}` ← operator one-button; the sequencer then issues the ordered
  chain `power/shack-pc-mains → power/psu-13v8 → WOL bwpc → hf/switch.trx → hf/switch.pa →
  power/antennas → hf/pa-arm` on start (reverse on stop, leaving `power/shack-pc-mains`
  on), gated on liveness confirmations and delays (§6, §7.1)

**Hardware interlock chain (enforces; software mirrors only):**
```
13.8 V PSU → M5 Stamp PLCs + ant-switch (boot on supply) → radio (remote-on) → pa (remote-on)
radio (TX low) → rx-loop-ctrl (preamp off) → ant-ctrl (inhibit if moving) → pa-arm → pa
```
The first line is the power-on dependency: the supply must be up before the controllers
can actuate or the radios can be triggered. The second is the RF safety chain; `pa-arm`
sits in it (the arm relay ANDs with the hardware key line), fail-safe-open.

### 7.2 Station `uhf`

Leaner than `hf`: most of the HF machinery is absent, and the model works correctly
with those parts left out.

**`muehle/uhf/radio`**: Icom IC-9700, **receive-only** since the 2026-09 pivot. The
RS-BA1 LAN session (control :50001) carries the demand-driven :50003 **RX audio
stream** only. The CI-V data stream is not opened, and **no LAN CI-V commands exist**.
Telemetry is read over the radio's dedicated **serial CI-V** port (read-only)
and broadcast on change. Fronted by **icom9700-radio-bridge** (host `scmino` since
2026-10-18). On scmino `serial.device` is unset by design: the only IC-9700 CI-V
adapter there belongs to `rigctld-ic9700` for `park_radio`, so the serial telemetry
monitor and `power_on` are disabled until a second CI-V port is added (that also needs
a unit change: the unit's `PrivateDevices=true` gives the service a private `/dev`
without real ttys, so its `DeviceAllow` lines alone do not expose one).
```
capabilities: bands [2m,70cm,23cm]; modes [cw,usb,lsb,am,fm,data];
              park {freq_hz, mode} (only when park.rigctld_addr is configured)
state:        ts; session_state {idle|connecting|live|error}   # CAPTURE-session state
              audio_demand (bool, TTL-bounded); monitor (bool);
              device_online (bool = capture liveness; false on healthy idle);
              error (on fault);
              — telemetry, monitor-gated (omitted while monitor off or the radio
              is deaf — never zeroed, never frozen):
              radio_responding; freq_hz (Hz int); band (derived); mode (canonical);
              satellite (read-only); s_meter, swr, alc (raw 0–255, on change);
              tx_power (only when the bench proves the CI-V read)
intent:       audio_on; audio_off; power_on; monitor_on; monitor_off; park_radio
              # /cmd NOT retained (one-shot); nothing keys the radio;
              #   park_radio is the only retune
```
**Receive-only.** The radio cannot be keyed from the bus (no `ptt`, no `arm`, no free
tuning). The one steering action is `park_radio`, a fixed-target retune to the
configured park frequency/mode through the loopback rigctld on scmino. The removal
(and the firmware PTT-off gap that motivated it) is recorded in
`icom9700-radio-bridge/docs/known-issues.md`; any future TX control goes through
serial CI-V and must re-pass the exposure review.
`park_radio` turns satellite mode off, then split off, then sets frequency, then mode,
and reads split, frequency and mode back (an error on mismatch). It goes through the
loopback-only hamlib rigctld `rigctld-ic9700` on scmino:4532
(`icom9700-radio-bridge/rigctld-scmino/`); the bridge opens no serial port for it, and
it is rejected when `park.rigctld_addr` is empty.
The field-for-field wire contract for this slot is
`icom9700-radio-bridge/docs/mqtt-api.md`; where that document and this model's generic
radio example (Appendix A) differ, the module contract wins.

**On-demand session.** The radio's single LAN session stays available for manual
wfview operating: the bridge is an on-demand client and does not hold the session
permanently. It connects only when an `audio_on` demand arrives, never auto-steals the
session, and disconnects after an idle timeout (the audio demand itself is TTL-bounded
so a dead consumer releases it). Telemetry `/state` fields come from the serial
monitor, not the LAN session: the measured fields are **omitted** (never zeroed, never
frozen) while the monitor is off or the radio does not answer, whatever the
capture-session state; `radio_responding` is present whenever the monitor is on. `ts`,
`session_state`, `audio_demand`, `monitor` and `device_online` are always present;
`error` is present on fault. Contention policy: never steal. A refused connect
surfaces as an observed fact in `/state.error`, and whether the radio distinguishes
refusal from eviction is pinned at the bench.

**`device_online` override for this slot.** Here `device_online` means **CAPTURE
session liveness** (redefined 2026-09), not the generic device-reachability of §3: it
reads `true` only while the audio-capture session is live, and a **healthy idle reads
`false`** (network-standby reachability is not reported). `session_state` is the
idle-vs-fault discriminator: `idle` with `/status` online is a healthy parked slot, not
a fault, and consumers must render it so.

**Bias-T, preamps, satellite mode.** The bridge publishes no `bias_t` capability
(dropped in the 2026-09 pivot): the bias voltage is set in the radio's menu and no bus
action exists. The masthead preamps are enabled
manually by the operator and dropped automatically on TX; preamp protection is
internal to the radio and needs no slot and no external sequencer. The radio's native
satellite split is READ-ONLY over the bus (`satellite` telemetry field, read by the
serial monitor): there is no `sat_mode`, `set_preamp` or `set_attenuator` command
anymore (receive-only posture, 2026-09).

**Safety posture.** There is NO TX path: the radio cannot be keyed from the bus (no
`ptt`, no `arm`, no software gate to bypass). Free remote control was removed in the
2026-09 pivot. Since 2026-10-08 the only steering action is `park_radio`, a
fixed-target retune over serial CI-V through the loopback-only `rigctld-ic9700` on
scmino (satellite mode off, split off, frequency, mode; no PTT/power/raw CI-V). This
retune path appears in the exposure review in `docs/known-issues.md` only as a note
(loopback rigctld, not a TX path); it has no reviewed vector of its own. Any
wider control or TX path (serial CI-V per the operator decision) must re-pass that
review and re-prove the firmware unkey behavior. `/meta` declares a **read-only**
`expose` block, with no PTT/arm widgets in HA (the sat-rotator posture; the action set lives
on `/cmd` and in the wire contract).

**`muehle/uhf/rotator`**: *retired 2026-10-10 (not in use).* Formerly the
PTS-303Z/3050DZ pan/tilt head (Pelco-D/P over RS-485), driven by the interactive
**pelcobridge2** TUI + rotctld on `shack-pc` (manual arming, MQTT `/cmd` limited to
`stop`). The component was removed from the repo; the address is free. UHF az/el
rotation is the sat-ops mount below.

**`muehle/uhf/az-rotator`** and **`muehle/uhf/el-rotator`**: the sat-ops
rotator mount (azimuth: SPID, Rot1Prog binary serial; elevation: GS-500 via the
ERC-M controller, GS-232B dialect serial), carrying the co-mounted 2 m / 70 cm
X-Quads. Fronted by one compound bridge (`spid-ercm-rotator-bridge` on host
`shari`, built like shelly-power-bridge: one process, two slots, two MQTT
clients, each with its own LWT). Per slot:

```
role: rotator;  device: { model (SPID | ERC-M / GS-500) } (no firmware key: SPID has
               none, and the ERC-M rFMW probe was dropped 2026-09-17 because it hangs
               the controller);
               link: serial;  host: shari
capabilities: axes ["az"|"el"]; limits {min, max, park}; deadband (°)
state:        ts; az|el (position, ° — omitted while readback invalid, never fabricated);
               target (omitempty, cleared by stop); moving (inferred:
               |target − readback| > deadband); link; device_online (explicit bool);
               error (omitempty)
intent:       goto {degrees under value}; stop;
              park (mount-level: stop, then BOTH axes to their capabilities.limits.park —
              either slot parks the whole mount)     # /cmd one-shot, NOT retained
```

**Two-layer liveness across two slots:** each slot's `/state.device_online` is
its **own serial link** (a dead elevation port takes only `el-rotator`
offline, never `az-rotator`), while `/status` is the shared bridge process
(one LWT per slot, so process death flips both with no stale-online gap).

Motion is **free with no arming gate** (deliberately unlike the retired
`uhf/rotator`'s manual-arming TUI): any connected client (rotctld, PstRotator, GS-232B,
console) may move the mount. Safety is procedural (operator watching); the posture's exposure
vectors are reviewed and recorded per-vector in `docs/known-issues.md` (the
plan's KTD4 register). The slots publish **read-only** `expose` blocks, so
hadiscovery renders state but no HA motion widgets; the motion authority lives
on `/cmd` and the protocol listeners.

Beyond the bus, the same bridge runs the protocol endpoints on `shari`: a
rotctld-compatible TCP server (`:4534`, the pelcobridge2-proven hamlib
dialect, for gpredict-class and terrestrial az-only clients), a PstRotator
native UDP listener (`:12041`) and a GS-232B TCP server (`:4533`, the PstRotator/N1MM
path: a networked PstRotator connects as a GS-232 TCP client), all fed from the same
dispatch core. Dispatch is by axis only (az → SPID, el → GS-500; a wire message that structurally
omits one axis leaves that axis parked; `stop` from any path halts both axes;
targets outside the configured travel limits are refused on every path before
any serial write). The ports are deliberately distinct from the other rotator
systems (wrc-rotator-bridge on scmino: GS-232 `:7373`, PstRotator `:12040`) so a
client pointed at "the rotator" cannot silently reach the wrong antenna system.
`:4533` (the hamlib rotctld default, once the retired pelcobridge2's on shack-pc) is
now this bridge's GS-232B server; the rotctld dialect is on `:4534`. Refusals on these
protocol paths (dead axis, out-of-limits target) are answered to the protocol client and
logged (journald), never routed into `/state.error`. That channel carries `/cmd`
rejections and serial link faults only.

**`muehle/uhf/sat-track`**: the satellite the station is tracking. A software
source, not a device: **oscarwatch-sattrack-bridge** (host `scmino`) dials the
OscarWatch tracker's Satellite-link WebSocket on the shack PC and republishes its
focused satellite. Read-only (no `/cmd`); tracker-agnostic role, so another tracker's
adapter (gpredict, SatPC32) would publish the same shape.
```
role: sat-track;  device: { name, model: OscarWatch };  link: websocket;  host: scmino
capabilities: source (oscarwatch); protocol (Satellite-link version)
state:        ts; device_online (the tracker link); tracking; in_range;
              sat_name; norad_id (string); mode_type (transponder label, not a mode);
              az; el (°); range_km; range_rate_km_s; sunlit;
              sub_lat; sub_lng; alt_km (sub-satellite point, derived from the look
              angle + station QTH — no TLE); uplink_hz; downlink_hz (radio-corrected);
              uplink_mode; downlink_mode (canonical or null); uplink_band; downlink_band;
              beacon_only; doppler; error (omitempty, only while the link is down)
```
Satellite keys are JSON `null` (not omitted) when nothing is tracked or the tracker is
unreachable; a lost link clears the satellite and does not hold its last position.
The look angle comes from the tracker. `uhf/az-rotator` / `uhf/el-rotator` report
where the mount points, and `sat-track` reports where the satellite is.
Wire contract: `oscarwatch-sattrack-bridge/docs/oscarwatch-sattrack-bridge-mqtt-api.md`.

**`muehle/uhf/pol-ctrl`**: M5 Stamp PLC #2 running an ESPHome config (`m5stamp-pol-ctrl`; not yet flashed, see `docs/known-issues.md`). `capabilities:
polarizations [h, v, cl, cr]`. Settable state, operator-driven; no automatic binding.

**Passive resources:** `ant/xquad-2m`, `ant/xquad-70cm`, `mast/ta16-vhf`,
`preamp/2m`, `preamp/70cm` (masthead LNAs, manually enabled, bias supplied and dropped
by the radio).

There is no PA, tuner or antenna switch. Band routing is physical via the radio's
separate 2 m / 70 cm ports, so there is no antenna-select binding.

### 7.3 Hosts and adapters

Compute hosts are first-class infrastructure nodes: each publishes its own `online`
and health, and every adapter or service declares the host it runs on. The set a host
runs is derivable from those declarations, which are the single source of truth; the
host does not carry its own copy. If a host drops, its hosted components go offline via
their own last-will, and the host's own last-will marks it down.

Not yet implemented: no component publishes `muehle/host/*`. These are reserved,
model-only nodes (open decision in `docs/known-issues.md`, "Host-liveness nodes are
model-only"). Today host placement is carried only by each slot's `/meta.host`.

```
muehle/host/shari       # Raspberry Pi, Linux
  role: host;  location: bauwagen;  state: online, temp_c, load
muehle/host/bwpc        # shack PC (Windows, 192.168.1.197): loggers, PstRotator,
                        # OscarWatch, winpc-power-bridge
  role: host;  location: bauwagen;  state: online
muehle/host/scmino      # Raspberry Pi CM5, Debian 12 arm64 — runs most station
                        # services since 2026-10-12
  role: host;  location: bauwagen;  state: online
```

| Adapter / service | Fronts | Host |
|---|---|---|
| PA bridge (+ ACOM telemetry) | `hf/pa` | `scmino` |
| Tuner bridge | `hf/tuner` | `scmino` |
| HF rotator bridge (AF6SA WRC) | `hf/rotator` | `scmino` |
| Antenna controller bridge (`ultrabridge`, Ultrabeam RCU-06) | `hf/ant-ctrl` | `scmino` |
| Sat rotator bridge (`spid-ercm-rotator-bridge`) | `uhf/az-rotator`, `uhf/el-rotator` | `shari` |
| IC-9700 radio bridge (`icom9700-radio-bridge`) | `uhf/radio` (Icom IC-9700) | `scmino` |
| IC-9700 rigctld (`rigctld-ic9700`, loopback :4532) | none (`park_radio` path) | `scmino` |
| Sat-track bridge (`oscarwatch-sattrack-bridge`) | `uhf/sat-track` (OscarWatch on bwpc) | `scmino` |
| FLEX bridge (flexbridge) | `hf/radio` | `scmino` |
| HF antenna-select reconciler | `hf/antenna-select` | `scmino` |
| Antenna mode (`antmode`) | `hf/ant-mode` | `scmino` |
| Beam steering (`beamsteer`, PstRotator UDP :12050) | `hf/beam-steer` | `scmino` |
| Logger spot bridge (`logger-spot-bridge`) | `hf/spots` | `scmino` |
| Bus capture (`hf-mqtt-capture`) | subscriber of `muehle/hf/#`, no slot | `shari` |
| Shelly power bridge (`shelly-power-bridge`) | `power/psu-13v8`, `power/antennas`, `power/shack-pc-mains` | `scmino` |
| Shack-PC power agent (`winpc-power-bridge`) | `power/shack-pc` | `bwpc` |
| M5 Stamp #1 firmware (`m5stamp-hf-ctrl`) | `hf/pa-arm`, `hf/switch` | embedded (M5 Stamp PLC) |
| M5 Stamp #2 firmware (`m5stamp-pol-ctrl`, ESPHome) | `uhf/pol-ctrl` | embedded (M5 Stamp PLC) |
| Antenna switch firmware (`waveshare_relay-antswitch-bridge`, ESPHome) | `hf/ant-switch` | embedded (WaveShare ESP32-S3 relay board) |
| Power sequencer (`powerseq`) | `hf/power-seq` | `scmino` |
| HA discovery bridge (optional, `hadiscovery`) | `hf/discovery` (logic slot); consumer of `meta`/`state`/`status` | `scmino` |
| VHF cam restreamer (`vhfcam-restream`, :8083) | `hf/vhfcam` (`/status` + `/state` only) | `scmino` |
| Station broker (`mqtt-broker`, Mosquitto :1883, `muehle/#` mirror of the HA broker `.50`) | none | `scmino` |
| Console web / testui / stationportal (:8091 / :8090 / :80) | consumers, no slot | `scmino` |

Embedded controllers combine device, adapter, and host in one node and are often the
cheapest integration point on the station, since no separate bridge or host is needed.
The model treats them identically to a bridged device: same three planes, just
co-located.

---

## 8. Transport binding (MQTT)

MQTT, plain JSON, no Sparkplug. Sparkplug's birth/death certificates, capability
announcement, and state/command split are already specified above, so adopting its wire
format would add protobuf and sequence-number costs for machinery we already have, and
its opaque payloads and fixed namespace conflict with "configuration is documentation."
This binding takes two ideas from Sparkplug (a retained birth certificate, death via Last
Will) and uses plain JSON as the encoding.

Four topic suffixes per slot (one per plane, plus liveness):

```
<addr>/meta     retained       birth certificate: identity + capabilities (rarely changes)
<addr>/state    retained       live state as ONE JSON snapshot document
<addr>/status   retained       online | offline — the Last Will topic
<addr>/cmd      NOT retained   intent
```

- **State is one retained JSON document, not per-field topics.** A late or reconnecting
  subscriber gets the whole atomic snapshot immediately, without polling and without
  half-updated state. The field layout of that document is defined by each slot's schema
  in §7; the transport defines only the envelope.
- **Liveness is the Last Will on `status`.** Register a retained `offline` will on
  connect, publish retained `online` once up, publish `offline` yourself on clean
  shutdown. The broker produces `online: false` when a node drops without anything else
  having to notice.
- **`meta` is retained**, so a late subscriber gets the birth certificate automatically;
  the rebirth-request machinery Sparkplug needs is unnecessary.
- **Never retain `cmd`: this is a safety rule.** A retained command re-delivers to the
  next subscriber and on every reconnect, so a stale `tune` or `select` can execute with
  no operator behind it. State is retained; intent never is. This is the transport-level
  counterpart of the plane discipline.

  **Exception: physical actuators with self-healing semantics.** For a physical device
  whose correct position must survive a software restart (e.g. a remote-on relay or a
  power plug that must stay on, or the `uhf/pol-ctrl` polarization), the `/cmd` topic
  MAY be retained with the desired
  steady-state. A command is safe to retain only when re-applying it on reconnect
  produces the same physical outcome as the original, and "re-applying" means the
  single retained value, never a replay of history.

  Three rules bound the re-delivery paths this exception opens (all three were
  motivated live on 2026-09-03, when a bridge came back after ~18 h offline and the
  broker delivered both its queued history and the retained value, physically
  re-driving the antenna). They apply to **one-shot command topics** (topics whose
  payloads are momentary intents, not desired steady state). A topic holding a
  *desired steady state* (a power `set_power`, an arm permit, an idempotent mode) is
  governed by the exception's own precondition above and by rule 2 only: its retained
  value is *supposed to* survive and be re-applied on reconnect, so rules 1 and 3
  must not be applied to it. One guard: a `/cmd` topic is **never** that case.
  Even where its payload resembles desired state (the `muehle/uhf/radio`
  `audio_on` demand, for instance, is TTL-bounded and refreshed by its consumer),
  re-applying it after a bridge restart is the consumer's own refresh decision,
  not the broker's replay; one-shot `/cmd` topics stay under rules 1–3.

  1. **One-shot commands clear the retained topic.** After the consumer has acted on
     a one-shot command (executed *or* rejected), it publishes an empty payload to
     the retained `/cmd` topic so nothing re-delivers on the next reconnect or
     restart. Re-applying a one-shot command after a restart is not the actuator's
     job; the arbiter that owns the command stream re-resolves (§5). A steady-state
     intent topic does the opposite: its retained value is the latest writer's
     intent *by design* (every writer, whether HA, a reconciler or an operator, publishes
     retained so the topic always tracks the current intent, and the consumer
     re-applies it on its own restart).
  2. **A `/cmd` consumer under a persistent session subscribes the command topic at
     QoS 0.** With `CleanSession=false` and a QoS-1 subscription the broker queues
     every `/cmd` published while the consumer is offline and replays the whole
     backlog on reconnect, and retention is irrelevant to that mechanism. QoS 0 stops
     offline queueing; retained delivery is independent of subscription QoS, so the
     steady-state replay still works. (This exempts the command subscription from
     checklist item 2's "QoS 1 on every subscribe". `powerseq` established the
     pattern on its own `/cmd`. It has used a clean session since 2026-10-18 and still
     subscribes `/cmd` at QoS 0. The persistent-session `/cmd` consumers `ultrabridge`,
     `shelly-power-bridge`, `spid-ercm-rotator-bridge`, `icom9700-radio-bridge` and the
     ESPHome `uhf/pol-ctrl` node follow it. The ESPHome ant-switch
     (`waveshare_relay-antswitch-bridge`, `clean_session: false`) still subscribes its
     retained `/cmd` at QoS 1, which is an open deviation from this rule. The other
     `/cmd` consumers use a clean session instead (explicitly in `antennaselect`,
     `beamsteer`, `antmode`, `powerseq` and `winpc-power-bridge`; by paho's default in
     `flexbridge`, `acom1200s-pa-bridge`, `wrc-rotator-bridge` and `atr1k-tuner-bridge`;
     forced by PubSubClient on the M5 Stamp PLC #1), which closes the same path. A retained steady-state input
     whose queued backlog must not replay gets the same treatment: `ultrabridge`
     subscribes `hf/ant-mode/state` at QoS 0 under its persistent session. A QoS-0
     `/cmd` subscription is also permitted under a clean session.)
     Adoption note: switching the subscription to QoS 0 does **not** purge a backlog
     the broker already queued for the session under a previous QoS-1 subscription,
     and does not suppress retained delivery. When adopting, clear the slot's
     retained `/cmd` once (empty retained publish) so no pre-adoption value
     replays.
  3. **One-shot commands carry a producer timestamp and consumers bound their age.**
     A `/cmd` payload on a one-shot topic SHOULD carry an RFC 3339 `ts`; a consumer
     MUST drop a stamped command older than a small bound (tens of seconds) instead
     of executing it. A stamp from the future is equally invalid. Producers that
     predate the field are covered by rules 1 and 2, but the rules leave one path open:
     a command published while the consumer is down is retained on the broker and
     replays **once** on its reconnect (the latest intent, executed once, then cleared
     by rule 1). That is self-heal, bounded by construction, but it is not a closed
     path, and no rule should claim it is.
     Steady-state intent topics are exempt from this rule: an older retained intent
     is still the current intent and is meant to be re-applied.

Defaults: QoS 1 everywhere; QoS 2 only for a specific command that must not double-fire
even under broker retry. Client IDs derived from the slot address, so a duplicate
connection can be diagnosed. Broker: Mosquitto with a persistent store (so retained
`meta` and `state` survive a restart); EMQX only if a rules engine or clustering is
wanted later, and neither is wanted at present.

**Broker topology (as deployed, 2026-10-23):** the station broker is Mosquitto
(`mqtt-broker/`, persistent store) on **scmino** (`192.168.1.178:1883`). It is bridged
both ways to the Home Assistant broker at `192.168.1.50:1883` (HA's own Mosquitto
add-on) as a transitional mirror: `muehle/#` both ways, `homeassistant/status` in,
`homeassistant/+/+/+/config` out. Per `mqtt-broker/CLAUDE.md` its clients so far are the
M5 Stamp PLC #1 and `shelly-power-bridge` with its Shelly plugs; root `CLAUDE.md`
(2026-10-18) records the other services as still on `.50`. Per-service endpoints are
seed-once device config, are not tracked in the repo and are not verified live (most
`deploy.sh` seed defaults name `.50`, some `127.0.0.1`). shari runs no broker.
**Target:** every client moves to the scmino broker, which becomes
authoritative for `muehle/#`, so the station can keep a working bus when the shack↔house
link is down. The bridge then narrows to the split mapping: slot
`state`/`meta`/`status` and the `hadiscovery` discovery config out, HA's birth and
`cmd` in. HA stays the reference consumer (§9). See `conventions/mqtt-topology.md` for
the topic-direction table and ACLs.

### 8.1 Adapter conformance checklist

The strict target this document sets. An adapter is conformant iff every line
holds; a review (human or GenAI) checks these one by one.

1. Publishes exactly the four planes: `/meta`, `/state`, `/status` retained; `/cmd`
   subscribed, and retained only under the §8 self-healing-actuator exception. A
   **one-shot** command topic follows §8 rules 1 and 3 (execute-then-clear, `ts`
   staleness bound); a **steady-state intent** topic follows the exception
   precondition and rule 2 only (its retained value is deliberately never cleared).
   Either way: QoS-0 command subscription when the session is persistent (§8 rule 2).
   Read-only slots omit `/cmd`.
2. QoS 1 on every publish and subscribe (QoS 2 only for a documented must-not-double-fire
   command), except a consumer under a persistent session, which subscribes its `/cmd`
   topic, and any retained steady-state input whose queued backlog must not replay
   (e.g. ultrabridge's `hf/ant-mode/state`), at QoS 0 so the broker cannot queue a
   backlog for offline replay (§8 rule 2). A QoS-0 `/cmd` subscription is also
   permitted under a clean session (e.g. `powerseq`, `winpc-power-bridge`).
3. `/status` is the plain string `online`/`offline`: registered as a retained Last Will,
   published `online` on (re)connect, `offline` on clean shutdown. No liveness field in
   `/state`; hardware reachability, if reported, is the `device_online`/`error` state
   fields (§3).
4. `/state` is one full JSON snapshot per publish (no partial updates, no per-field
   topics), with an RFC 3339 UTC `ts`.
5. `/meta` carries `schema`, canonical `role` (§4), `capabilities`, `location`, `host`;
   device slots add `device{model,…}` and `link`. `location` and `host` come from config.
   An OPTIONAL `expose` block (§3.1, Appendix C) declares the slot's consumer-neutral
   field surface; slots that want to be discovered by HA/historians/dashboards publish it.
6. `site`/`station`/`slot` come from config; no site, station, host, location, or
   passive-resource name appears as a constant in code. Client ID defaults to a
   derivation of the slot address.
7. Vocabulary: `freq_hz` integer Hz; `band` derived from the canonical table with
   `gen`/`unknown` for out-of-allocation frequencies; modes normalized to canonical
   names or the field omitted, never a raw firmware string.
8. No publish under any consumer tree (`homeassistant/…` etc.); see §9 and its
   currently-documented deviation.
9. Config in a single seed-once TOML (0600 when it holds secrets), secrets never on the
   command line; hardened systemd unit (see `conventions/`).
10. **The initial connect must not leave the slot without MQTT.** The first connect
    either retries indefinitely (e.g. paho `SetConnectRetry(true)`) or fails the process
    with a non-zero exit so systemd's `Restart=on-failure` crash-loops it back until the
    broker answers. A component must never continue running with its MQTT plane
    silently disabled: the web UI and serial side staying healthy while the bus never
    hears from the slot again is the failure mode this item forbids (the 2026-09-03
    ultrabridge outage ran ~18 h this way). Mid-run drops stay covered by the
    auto-reconnect; the initial connect is the gap it does not cover.

---

## 9. Design invariants

The constraints most likely to erode once the project is open-sourced and others
contribute. They are hard rules.

- **Dependency points one way only.** Core components (radios, PAs, switches, the
  arbiter, the interlocks) know nothing about any consumer and never publish to a
  consumer-specific tree (no core component publishes under `homeassistant/…`). Consumers
  depend on the canonical bus; the bus never depends on a consumer. Delete every consumer
  and the station runs identically.
- **Consumers are optional modules.** Dashboards, historians, and the Home Assistant
  bridge are separate processes, disabled by default, required by no core component, and
  documented under optional integrations rather than getting-started. Home Assistant is
  the reference consumer and has no privileged status. Grafana, Node-RED or Prometheus
  consumers can be added the same way, each as a thin edge adapter against the same core.
- **The operator surface is a UI-agnostic canonical topic.** Manual control (an operator
  hold into ladder tier 2) is a plain canonical topic any UI can publish (a CLI, a web
  page, an M5Stack button, or the HA bridge). The HA bridge is one writer of that topic
  and never defines it, so "manual antenna control" never depends on running HA.

**Known deviation (accepted, temporary):** flexbridge, ultrabridge and acom1200s-pa-bridge
historically published Home Assistant discovery under `homeassistant/…` from within the
core adapter (wrc-rotator-bridge still accepts the `publish_ha_discovery` key but has no
discovery code behind it). That embedded discovery is now **gated off**
(`publish_ha_discovery = false`, default false), so by default they no longer publish
it. ultrabridge also subscribed `homeassistant/status` to re-announce, and still
subscribes it unconditionally: on HA birth it re-publishes its retained `/state`, and it
publishes discovery again only with the gate on. The invariant stands; the target is a
standalone HA-discovery consumer that reads `/meta` + `/state` and emits discovery for
every slot, at which point the embedded code is deleted.

**Resolution in progress:** the standalone consumer is `hadiscovery` (`muehle/hf/discovery`),
a passive service that reads the consumer-neutral `expose` block (§3.1, Appendix C) from
each slot's `/meta` and renders HA discovery. Components publish `expose` in their own
`/meta` and carry **no** HA knowledge; HA is one renderer of the same surface any other
consumer could read. `hadiscovery` runs on scmino (since 2026-10-12). The embedded
discovery in flexbridge, ultrabridge and acom1200s-pa-bridge is gated off (see above) and
slated for deletion once `hadiscovery` is proven live. Until then the embedded discovery
must remain config-gated, and nothing may depend on it: with discovery disabled (or HA
absent), the canonical planes are unaffected and the station runs identically.

---

## 10. Trade-offs and known weak points

The known limits of the current design are listed below.

- **The reconciler is a coordination single point.** If it dies, soft bindings stop
  updating; the station degrades to manual and retained state goes stale. This is
  acceptable *only because* safety is hardware and fail-safe, but it also means band-follow
  (Ultrabeam frequency, PA band, tuner in-line) and antenna selection all stop together.
  The pa-arm interlock runs in the M5 Stamp PLC #1 firmware and keeps running; with the
  reconciler offline it skips its antenna-move-pending gate. Mitigated: systemd
  `Restart=on-failure` (5 s) restarts it, and hf_console reports `hf/antenna-select` in
  its faults bar when it is offline or silent, greys out AUTO/MANUAL and drives the
  switch directly, so the operator can see that they are on manual.
- **Retained state can be acted on while stale.** Mitigated by `online`/last-will and
  heartbeats, but any consumer that acts on retained state must check liveness first.
  This is why no safety decision may depend on it.
- **Band-policy residual.** 30/40/60/80 m now map to the dipole and 6-20 m (incl. 17/12)
  to the Ultrabeam. Two caveats: 30/60 m (and, per the seeded `tuner_follow.atu_bands`,
  80 m) on the 80/40 dipole need the ATU, so the policy implicitly requires the ATU
  in-line and tuning on those bands; and 160 m has no antenna assigned. **Resolved:**
  unmatched bands (incl. 160 m) route to a named fallback, the fan-dipole (via the ATU).
  flexbridge's no-information band values (`""`, `unknown`: reconnect, 0 Hz) hold the
  last selection, with no PA or tuner follow, instead of reaching the fallback
  (2026-10-10); only a known-but-unmatched band (e.g. 160 m, `gen`) falls back.
  Configured as `band_policy.fallback` in the `antenna-select` reconciler. The ATU is
  engaged in-line for the non-resonant bands by the `tuner.set_inline ← band_policy` soft
  binding (§7.1), driven by the `antenna-select` `[tuner_follow]` block against the
  `hf/tuner` slot (the former "tuner is assumed but never driven" residual is closed).
- **Hosts are now single points too.** `scmino` runs the station broker (bridged to the
  HA broker `.50`). `scmino` also fronts the HF radio, PA, tuner, rotator and Ultrabeam,
  the IC-9700, the Shelly power plugs and the logger and OscarWatch feeds, and it hosts
  every logic slot (`antenna-select`, `beam-steer`, `ant-mode`, `power-seq`,
  `discovery`). `shari` fronts only the sat-ops az/el rotator mount
  (`spid-ercm-rotator-bridge`, whose rotctld, PstRotator and GS-232B listeners also live
  there) and runs the bus logger (`hf-mqtt-capture`). The shack PC (`bwpc`) fronts
  `power/shack-pc` (`winpc-power-bridge`) and runs OscarWatch, the source behind
  `uhf/sat-track`; the UHF pan/tilt head (`pelcobridge2`) was retired 2026-10-10. Root
  `CLAUDE.md` (2026-10-18) records most services as still connecting directly to the HA
  broker (`192.168.1.50`); the scmino broker mirrors `muehle/#` to it. A host loss takes
  its whole cluster offline at once (correctly, via last-will, but simultaneously). The
  station depends on host liveness, which is worth monitoring; the reconciler running on
  `scmino` adds to that host's coordination single point (see §11).
- **GenAI-authored adapters vary in quality.** The strict core limits the damage a bad
  adapter can do, but such an adapter can still publish plausible-but-wrong state and drive correct
  bindings to wrong outcomes. Adapters need conformance tests against the schema before
  they are trusted, especially for state fields that feed safety mirrors.
- **Idle-over-operator is a deliberate surprise.** Walking away (station inactive)
  overrides an operator dummy-load hold. The rationale is walk-away safety, and the
  behaviour is documented here as a design choice. The one exception is an explicit operator
  `manual` stand-down (antenna-select tier 0): in manual the antenna is never moved
  automatically (idle grounding included), so a manual operator grounds via an explicit
  `off` request instead. The antenna-mode lock (`hf/ant-mode` storm/maintenance, §5) is
  the one automatic move that manual does not block: it grounds the switch even in
  manual.
- **Two TA16 masts.** Distinct passive-resource names (`mast/ta16-hf`, `mast/ta16-vhf`)
  are required; never reference a mast by model alone.

---

## 11. Open items to confirm

All seven v0.1 items are resolved and folded in (hierarchy confirmed as stations; 23 cm
out of scope, superseded 2026-09-15: 23 cm is in the IC-9700 slot's band set
(receive-only since the 2026-09 pivot: RX audio capture and read-only telemetry), see
§7.2 and the band/mode reference; UHF polarization and preamps operator-only; devices identified; `pa-arm`
an embedded M5 Stamp PLC; band policy set; tune routing through the automation). Residuals
surfaced by those answers:

1. ~~**160 m antenna.**~~ Resolved: no dedicated 160 m antenna. Unmatched bands (incl.
   160 m) fall back to the fan-dipole via the ATU (`band_policy.fallback` in
   `antenna-select`). Revisit if a resonant 160 m antenna is added.
2. ~~**`shack-pc` roles.**~~ Resolved: the reconciler moved to scmino with flexbridge on 2026-10-12; shari keeps
   logging (`hf-mqtt-capture`) and the sat-ops rotator bridge. shack-pc hosted
   pelcobridge2 only: the interactive TUI + rotctld
   server for the PTS-303Z/3050DZ pan/tilt head (`uhf/rotator`), retired 2026-10-10
   (not in use). Since 2026-10-18 shack-pc (bwpc) fronts `power/shack-pc` through
   `winpc-power-bridge`, and shack-pc runs OscarWatch, the source behind `uhf/sat-track`
   (bridge on scmino). (The former
   PSTRotator/SPID attribution of shack-pc was stale: the SPID/GS-500 sat-ops
   rotators are fronted by `spid-ercm-rotator-bridge` on shari, as
   `uhf/az-rotator` / `uhf/el-rotator`.)
3. ~~**`pa-arm` build.**~~ Resolved: realized as **M5 Stamp PLC #1** (`m5stamp-hf-ctrl`),
   a compound embedded node publishing both `hf/pa-arm` (arm relay 1) and `hf/switch`
   (remote-on relays 3 & 4). Formerly planned as a discrete M5Stack; the M5 Stamp PLC
   supersedes it.

---

## 12. Deferred

The reconciler's internal implementation; horstprop Layer-3 modelling; multi-multi
arbitration beyond the single-station ladder; and the mechanical field layouts for the
slots §7 names without a field layout (`rx-loop-ctrl`, the §7.3 host-liveness nodes). (`uhf/radio`
left this list 2026-09-15: the IC-9700 slot carries a full §7.2 entry and a committed
wire contract, `icom9700-radio-bridge/docs/mqtt-api.md`.)
None of these change the shapes above.

**Live meter data (VITA-49).** The real-time meter stream from the radio (S-meter, SWR,
PA power, temperatures), decoded from VITA-49 datagrams at 10–20 fps, was intentionally
removed from the flexbridge bridge: it is high-rate, non-retained, and belongs to a
monitoring/logging consumer rather than the canonical station bus. When added, it will
live at a sub-topic (`<slot>/meter/<group>/<name>`) and be explicitly **not retained**,
consistent with the plane discipline (high-rate telemetry ≠ state).

---

## Appendix A: worked example of the `radio` slot on the wire

Concrete payloads for `muehle/hf/radio` (the reference a new adapter copies) and,
further down, the `muehle/uhf/radio` instance of the same role (the receive-only
variant). All JSON is
UTF-8. `meta`, `state`, and `status` are retained; `cmd` is not. QoS 1 on every publish.
A `/cmd` subscriber with a persistent session (the `uhf/radio` bridge) subscribes at
QoS 0 (§8 rule 2).

`radio/state` always represents the **active TX receiver**. For a single-receiver radio
(`receivers: 1`) this is the only receiver. For multi-receiver radios, per-receiver
sub-topics (`radio/receiver/N/state`) are additive; downstream actuators (PA, rotator,
antenna-select) bind only to this top-level document.

`muehle/hf/radio/meta`: retained
```json
{
  "schema": "1.0",
  "role": "radio",
  "device": { "model": "FLEX-8400", "serial": "8400-01234", "firmware": "3.8.19" },
  "link": "ethernet",
  "location": "bauwagen",
  "host": "scmino",
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
  },
  "expose": {
    "device": { "name": "FlexRadio FLEX-8400", "model": "FLEX-8400",
                "manufacturer": "FlexRadio Systems", "sw_version": "3.8.19",
                "area": "bauwagen" },
    "fields": [
      { "key": "device_online", "name": "Device online", "type": "boolean" },
      { "key": "freq_hz", "name": "Frequency", "type": "number", "unit": "Hz",
        "class": "frequency", "state_class": "measurement" },
      { "key": "band", "name": "Band", "type": "enum", "options_ref": "bands",
        "writable": true,
        "command": { "action": "set_band", "value_key": "value", "value_type": "string" } },
      { "key": "mode", "name": "Mode", "type": "enum", "options_ref": "modes" },
      { "key": "drive", "name": "Drive", "type": "number", "unit": "%" },
      { "key": "tx", "name": "Transmitting", "type": "boolean", "on": "tx", "off": "rx" },
      { "key": "tuning", "name": "Tuning", "type": "boolean" },
      { "key": "dvk_status", "name": "DVK Status", "type": "string" },
      { "key": "dvk_id", "name": "DVK Memory", "type": "number" },
      { "key": "mic_profile", "name": "Mic Profile", "type": "string", "writable": true,
        "command": { "action": "set_mic_profile", "value_key": "value",
                     "value_type": "string" } }
    ],
    "actions": [
      { "key": "dvk_play_1", "name": "DVK Play 1", "command": { "action": "dvk_play_1" } },
      { "key": "dvk_stop", "name": "DVK Stop", "command": { "action": "dvk_stop" } }
    ]
  }
}
```
`expose.actions` also carries `dvk_play_2` … `dvk_play_12` in the shape of
`dvk_play_1` (13 actions in all); `expose.device.name` is `"FlexRadio "` plus the
discovered model. flexbridge does not publish `host` yet (a deviation from §8.1
item 5); the value shown is what it should carry.

`muehle/hf/radio/state`: retained
```json
{
  "ts": "2026-07-06T12:34:56Z",
  "freq_hz": 14025000,
  "band": "20m",
  "mode": "cw",
  "tx": "rx",
  "tuning": false,
  "drive": 40,
  "device_online": true
}
```

`muehle/hf/radio/status`: retained, and the Last Will
```
online
```

`muehle/hf/radio/cmd`: not retained
```json
{ "action": "set_band", "value": "20m" }
{ "action": "set_mic_profile", "value": "Default ProSet HC6" }
{ "action": "set_mode", "value": "cw" }
{ "action": "dvk_play_3" }
{ "action": "dvk_stop" }
```
(Band-stacking: the radio restores the last-used frequency/mode for `20m`; `band` stays
derived from `freq_hz` on `/state`. A direct `set_freq_hz` is the model's canonical tuning
intent; flexbridge exposes `set_band` as the operator-facing convenience. `set_mic_profile`
drives SmartSDR's native mic profile load; the available list is queried via
`profile mic info` and observed on `/state.mic_profiles`, and the active name is tracked
client-side on `/state.mic_profile` since SmartSDR reports no active mic profile. There is no
save: `profile mic save` is obsolete on SmartSDR v4+. `set_mode` (`cw`/`usb`/`lsb`
only) sends `slice set <idx> mode=<MODE>` to the slice `/state` reports (TX, else
active); hf_console sends it when the operator re-taps the active band (CW → USB above
10 MHz, LSB below; any other mode → CW). The wire command is not yet confirmed live on
the FLEX-8400. DVK: `dvk_play_<N>` (1–12), `dvk_play` + `value`, `dvk_stop` [+ `value`].)

Encoding decisions worth copying:

- `freq_hz` is the canonical integer truth (Hz, not MHz); `band` is the derived name,
  resolved against the canonical table (§4), not re-declared here.
- `mode` is the canonical name (`cw`, `usb`, …). The adapter normalizes the firmware
  string before publishing; consumers never see raw firmware mode names.
- `drive` is the transmit gain as a 0–100 integer (the SmartSDR `drive` field). Not
  `drive_pct`; keep the name short and match the source API.
- `rx_input` is omitempty; an adapter that does not yet parse RX-input selection
  omits it rather than guessing. Consumers that need it check for its presence.
  flexbridge does not parse RX-input selection yet, so it omits `rx_input`.
- **Liveness is the `/status` topic, never an `online` field in `/state`.** A dead node
  cannot update its own state document to say it is gone, which is why the Last
  Will publishes to a separate topic. `status` is a plain string (`online` / `offline`),
  not JSON, so it maps to Home Assistant availability with no template.
- `/state` is a full snapshot on every publish (last-write-wins); there are no partial
  updates, so the last document a subscriber received is a complete, consistent view on
  its own.
- `tune` is not a radio `/cmd` action: flexbridge implements none. ATU tuning is the
  `hf/tuner` slot's one-shot `tune` action, which the console publishes directly. The §6
  ordering (PA disarmed before the tune carrier) is enforced by the `pa-arm` PLC: its hazard
  gates open the arm relay while `hf/radio` reports `tuning` or `hf/tuner` reports a tune
  cycle (`settling`). The role's other intents (`set_freq_hz`, `set_mode`, `set_drive`,
  `select_rx`, `set_band`) are direct intents. flexbridge implements `set_band`,
  `set_mode` (`cw`/`usb`/`lsb`), `set_mic_profile` and the DVK actions, and rejects
  `set_freq_hz`, `set_drive` and `select_rx` as unknown actions. `set_band` is the
  band-stacking convenience (§4; §7.1 `muehle/hf/radio`): it changes the band and lets
  the radio pick the frequency, so `/state` stays frequency-derived rather than carrying
  a band setpoint.

### The same role, receive-only variant: `muehle/uhf/radio` (IC-9700)

The UHF station's `radio` slot instantiates the role **receive-only** (2026-09 pivot):
no TX state, arm/PTT intent or free tuning; the one retune is `park_radio`,
which sets a fixed park frequency/mode (satellite mode and split off first) through
rigctld on scmino. The LAN session carries the demand-driven RX audio stream only,
and telemetry comes from the read-only serial CI-V monitor (on change, omitted while
the monitor is off or the radio is not responding). `device_online` means
**capture-session liveness** with healthy idle `false`. The field-for-field contract is
`icom9700-radio-bridge/docs/mqtt-api.md`. Where it and this appendix's generic example
above differ, that document wins for this slot.

`muehle/uhf/radio/meta`: retained (capabilities + **read-only** `expose`; no PTT/arm widgets)
```json
{
  "schema": "1.0",
  "role": "radio",
  "device": { "model": "Icom IC-9700" },
  "link": "ethernet",
  "location": "bauwagen",
  "host": "scmino",
  "capabilities": {
    "bands": ["2m", "70cm", "23cm"],
    "modes": ["cw", "usb", "lsb", "am", "fm", "data"],
    "park": { "freq_hz": 145212500, "mode": "fm" }
  },
  "expose": {
    "device": { "name": "muehle/uhf/radio", "model": "Icom IC-9700" },
    "fields": [
      { "key": "freq_hz",           "name": "Frequency",        "type": "number", "unit": "Hz", "state_class": "measurement" },
      { "key": "mode",              "name": "Mode",             "type": "enum",   "options_ref": "modes" },
      { "key": "s_meter",           "name": "S-meter",          "type": "number", "state_class": "measurement" },
      { "key": "swr",               "name": "SWR",              "type": "number", "state_class": "measurement" },
      { "key": "alc",               "name": "ALC",              "type": "number", "state_class": "measurement" },
      { "key": "audio_demand",      "name": "Audio capture",    "type": "boolean" },
      { "key": "monitor",           "name": "Serial monitor",   "type": "boolean" },
      { "key": "radio_responding",  "name": "Radio responding", "type": "boolean" },
      { "key": "session_state",     "name": "Capture session",  "type": "string" },
      { "key": "error",             "name": "Last error",       "type": "string" }
    ]
  }
}
```
`capabilities.park` is present only when `park.rigctld_addr` is configured (values
illustrative; the console sends `park_radio` only when it is advertised). The bridge
currently publishes `host` as the radio's LAN name and no `link`, its `expose` names the
device `radio`, and its mode `options_ref` is `capabilities.modes`, which consumers do not
resolve; these are bridge-side deviations from the example above.

`muehle/uhf/radio/state`: retained, monitor on and the radio answering
```json
{
  "ts": "2026-09-15T12:34:56Z",
  "session_state": "live",
  "audio_demand": true,
  "monitor": true,
  "device_online": true,
  "radio_responding": true,
  "freq_hz": 432100000,
  "band": "70cm",
  "mode": "fm",
  "satellite": false,
  "s_meter": 34,
  "swr": 12,
  "alc": 0
}
```

`muehle/uhf/radio/state`: retained, **healthy idle** (`device_online:false` is not a
fault here; the telemetry fields are omitted, never zeroed or frozen)
```json
{
  "ts": "2026-09-15T12:36:20Z",
  "session_state": "idle",
  "audio_demand": false,
  "monitor": false,
  "device_online": false
}
```

`muehle/uhf/radio/status`: retained, and the Last Will
```
online
```

`muehle/uhf/radio/cmd`: not retained (one-shot; cleared with an empty retained
payload after every execute-or-reject)
```json
{ "action": "audio_on" }
{ "action": "monitor_on" }
```
(Exactly six actions exist: `audio_on`, `audio_off`, `power_on`, `monitor_on`,
`monitor_off`, `park_radio`, with no value arguments. There is no `ptt` and no `arm`:
the radio cannot be keyed from the bus. The only retune is `park_radio`: satellite mode
off, split off, then the configured `[park]` frequency and mode, read back, via the
rigctld on scmino. It is rejected when `park.rigctld_addr` is empty. The full action
table lives in `icom9700-radio-bridge/docs/mqtt-api.md`.)

---

## Appendix C: the `expose` schema (normative)

`expose` is an optional block on `/meta` (§3.1) declaring a slot's consumer-neutral
observable/controllable field surface. It is normative: consumers (HA via `hadiscovery`,
historians, dashboards) bind to it, and components implement it exactly. It carries no
consumer-specific vocabulary.

### Top level

| key | required | meaning |
|---|---|---|
| `device` | optional | device-registry block shared by all entities of the slot (see below) |
| `fields` | optional | list of state fields to expose (sensors / setpoints) |
| `actions` | optional | list of one-shot buttons |

### `device`

| key | meaning |
|---|---|
| `name` | device display name (mandatory if `device` present) |
| `model` / `manufacturer` / `sw_version` | device identity (model/serial/firmware live in `meta.device`; `expose.device` may repeat/supplement for the consumer) |
| `area` | suggested area (e.g. "Radio shack") |

Logic slots omit `model`; they may omit `device` entirely (the consumer falls back to
`<role> <addr>`).

### `fields[]`

| key | required | meaning |
|---|---|---|
| `key` | yes | the state-field name (the JSON key in `/state`) |
| `name` | yes | display name |
| `type` | yes | `number` \| `enum` \| `boolean` \| `string` |
| `unit` | no | unit string for `number` (`Hz`, `%`, `°C`, `W`, etc.) |
| `class` | no | semantic hint (`frequency`, `temperature`, `power`, etc.), using generic terms and no HA-specific ones |
| `state_class` | no | `measurement` \| `total` \| `total_increasing` |
| `options` | no* | inline enum option list |
| `options_ref` | no* | key into `capabilities` whose array is the option list (e.g. `"modes"`), so the option list is defined once, in `capabilities` |
| `writable` | no | if true, the field is also a setpoint/command target |
| `command` | no | required when `writable`: how a write is encoded on `/cmd` (see below) |
| `on` / `off` | no | for `boolean`: the payload strings `/state` holds (e.g. `tx`/`rx`). Absent ⇒ state holds a JSON boolean. |
| `min` / `max` / `step` | no | for writable `number` setpoints |

\* `enum` requires exactly one of `options` / `options_ref`.

### `actions[]`

| key | required | meaning |
|---|---|---|
| `key` | yes | action identifier (becomes the entity object id) |
| `name` | yes | display name |
| `command` | yes | the `/cmd` payload to send on press (see below) |

### `command` descriptor

Describes how a write is encoded on `/cmd`, in structured form (never a consumer-specific
template string):

| key | meaning |
|---|---|
| `action` | optional `/cmd` JSON `action` value (e.g. `"frequency"`, `"set_band"`, `"retract"`) |
| `value_key` | the JSON key under which the user-supplied value is placed (e.g. `"freq_hz"`, `"value"`, `"select"`) |
| `value_type` | `string` \| `int` \| `float` (how the consumer coerces the value) |

The consumer renders this into its own command syntax. The three observed shapes:
- `action` + `value_key`: `{"action":"<action>","<value_key>":<value>}`, e.g. band ⇒
  `{"action":"set_band","value":"20m"}` (flexbridge, acom) or direction ⇒
  `{"action":"direction","value":"forward"}` (ultrabridge); frequency ⇒
  `{"action":"frequency","freq_hz":14074000}`
- `value_key` only: `{"<value_key>":<value>}`, e.g. ant-switch ⇒ `{"select":"port2"}`
- `action` only (button): `{"action":"<action>"}`, e.g. `{"action":"retract"}`

### How a consumer maps `expose` (informative, non-normative)

A consumer derives the topic planes from the slot address: `state_topic = <addr>/state`,
`command_topic = <addr>/cmd`, availability = `<addr>/status`. The neutral type maps to the
consumer's entity kinds; e.g. for Home Assistant: `number` writable→`number`, `number`
readonly→`sensor`, `enum` writable→`select`, `enum` readonly→`sensor`, `boolean`→
`binary_sensor`, `action`→`button`. The unit maps to a device class via the consumer's own
taxonomy. The HA-specific mapping lives in `hadiscovery/docs/discovery-mqtt-api.md`;
keeping it there leaves this model consumer-neutral.
