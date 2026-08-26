# Station Integration Model — Reference

Version 0.7 (draft). This defines the *shape* the configuration takes and, from 0.4, its
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
as requiring adapter work). In 0.7: the generic/concrete boundary made explicit — canonical
role `ant-ctrl` introduced and the `ultrabeam-ctrl` slot renamed (a device name had leaked
into an address, violating §2's own rule); the liveness contradiction resolved (`online`
removed from the §3 state template; optional `device_online`/`error` state fields defined);
`/meta` fields made normative, including `host`; a resource→controller map added so
band-follow needs no antenna names in code; adapter conformance checklist added (§8.1);
the HA-discovery deviation in current bridges recorded against §9. In 0.8: a
power-distribution layer added — canonical roles `power`, `switch`, `sequencer` (§4); a
site-level `power` path analogous to `host` (§2, §7.3); the compound-device pattern made
explicit (§3); PA remote-on moved off the PA slot (`set_power`/RTS removed) onto the power
layer; `pa-arm` realized by an M5 Stamp PLC (closing §11.3); and a `power-seq` sequencer
owning the ordered, delay-and-confirmation startup/shutdown.

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
  under its path, and it carries only an inferred `activity` flag (derived from radio
  VFO/frequency changes and transmits by the antenna-select reconciler).
- **position** — an operator seat. Collapses to one and disappears in a single-op
  shack; reappears only when two operators share a station.
- **slot** — a role at that position (`radio`, `pa`, `ant-switch`, ...).

A physical building that is not a transmitting entity (the Bauwagen) is a
**`location` attribute**, not a path level. A second building is then just another
`location` value; promote it to a real level only if buildings become
contention-relevant (shared feedline, shared rotator).

The address is structural. The device filling a slot is an attribute of the slot, not
part of the address — so an unnamed rotator is still addressable as `.../rotator`.

Two rules make this enforceable:

- **The slot segment is a canonical role name (§4), never a device or product name.**
  `muehle/hf/ant-ctrl`, not `muehle/hf/ultrabeam-ctrl` — the Ultrabeam RCU-06 is the
  `device` attribute in that slot's birth certificate. A second instance of the same
  role at one station takes a numeric suffix: `ant-ctrl-2`, `rotator-2`. (The two
  existing rotators need no suffix — they live under different stations.)
- **`site`, `station`, `slot`, `location`, and `host` are deployment configuration,
  never code constants.** An adapter must work for any site, station, or slot name via
  its config file alone; the only names an adapter may hard-code are the canonical
  vocabulary itself (roles, modes, field names) and facts about the device it fronts.

**Site-level infrastructure sits outside the station path.** Not every node belongs to a
transmitting station. Two non-station paths exist under the site:

- `site/host/<name>` — compute nodes (§7.3).
- `site/power/<name>` — power-distribution infrastructure that feeds *across* stations
  (the station master mains, a shared 13.8 V PSU that powers both the HF and UHF radios,
  the tuner, controllers, relay boards). These are shared upstream resources, so they do
  not live under `hf/` or `uhf/`; like `host`, they are addressed at site level. A
  station-scoped consumer (a sequencer, an operator) commands them like any other slot.

---

## 3. The slot template

Every slot — device or logic — has the same three-plane shape.

```
<site>/<station>/<slot>

# birth certificate — published on connect, retained, changes rarely
role:          <role>                        # canonical role name (§4) — REQUIRED
device:        { model, serial, firmware }   # swappable; absent for logic slots
link:          <transport>                   # informational; REQUIRED for device slots
location:      <building>                    # REQUIRED, from config
host:          <compute node>                # where the adapter runs (§7.3) — REQUIRED,
                                             # from config; embedded nodes name themselves
capabilities:  { ... }                       # the discovery contract — REQUIRED
expose:        { ... }                       # OPTIONAL (§3.1): the slot's consumer-neutral
                                             # observable/controllable field surface — what
                                             # fields/actions it publishes, with types/units/
                                             # options/writability/command shapes. Consumed by
                                             # HA, historians, dashboards, etc. No HA vocabulary.

# liveness — its own plane (the /status topic, §8), never a state field
status:        online | offline               # broker last-will

# state — retained, last-write-wins
<field>:       <type>                         # what is currently true
device_online: bool                           # optional, device slots only: false when the
                                              # fronted hardware is unreachable while the
                                              # bridge itself is up; omitted when true
error:         string                         # optional: human-readable device fault

# intent — transient; the slot reconciles toward it or rejects
<action>:      <type>
```

Rules that hold for all slots:

- **State is retained** (last-known-value) so a late-joining consumer or logger has the
  full picture without polling.
- **Liveness via last-will, on its own topic.** `/status` flips to `offline`
  automatically when the component drops; retained state is then known-stale. Liveness
  is **never** a field inside `/state` — a dead node cannot update its own state
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
| Passive resource | no — referenced by config | fan dipole, mast, dummy load |
| Service / logic | yes | reconciler, arbiter, logger, Horst modules |
| Host / infrastructure | yes (liveness, health) | shack PC, Raspberry Pi (`shari`) |

A passive resource is named and referenced (e.g. by the wiring map) but has no state
of its own. One physical antenna can span both: the Ultrabeam is a passive RF resource
on a switch port **and** an active control slot that follows band and frequency.

A host is a compute node that runs adapters and services; it publishes its own
liveness so the deployment topology is documented and its failure is visible. An
embedded controller (an M5 Stamp PLC) collapses device, adapter, and host into a single
node — custom firmware speaks the canonical schema directly.

**Compound devices.** One physical device may publish **several slots**, each its own
role and address with its own three planes. The slots are tied as one physical device by
carrying the **same `device { model, serial }`** in their `/meta` — that shared attribute
*is* the compound-device relationship; no extra mechanism is needed. The function slots
carry control + semantics; a generic-surface slot (e.g. a read-only `digital-input`
mirror) may additionally expose the raw hardware truth for monitoring (§6). Example: a
single M5 Stamp PLC publishes `pa-arm` (safety arm relay) and `switch` (remote-on relays)
as two slots with identical M5Stamp-PLC identity.

---

## 3.1 The `expose` block — consumer-neutral field surface

`expose` is an OPTIONAL block on `/meta` that describes the slot's observable and
controllable field surface in a form **no specific consumer owns**. It is the answer to
"what fields/actions does this slot publish, and which are settable?" — stated once, in
the birth certificate, so that *any* consumer (Home Assistant, a historian like InfluxDB,
a dashboard, Node-RED, Prometheus) can render its own representation without hardcoding
per-role knowledge. This is what lets §9 hold: HA is one thin edge adapter against the
same surface as Grafana or Node-RED, and the bus never depends on any of them.

`expose` carries **no consumer vocabulary** — no `device_class` strings, no Jinja
templates, no `payload_on/off`. Those belong to the consumer. The block describes only
neutral primitives: field `type` (`number`/`enum`/`boolean`/`string`), `unit`, a semantic
`class` hint, enum `options` (inline or by `options_ref` into `capabilities`), `writable`,
a `command` descriptor for how a write is encoded on `/cmd`, and `min/max/step` for
setpoints. `actions` describe one-shot buttons. The full schema is in Appendix C.

A slot that omits `expose` is simply not discovered by `expose`-driven consumers; its
canonical planes are unaffected. Logic slots omit `device.model` (and may omit `device`
entirely).

```json
"expose": {
  "device":  { "name": "...", "model": "...", "manufacturer": "...",
               "sw_version": "...", "area": "..." },
  "fields":  [ { "key": "freq_hz", "name": "Frequency", "type": "number",
                 "unit": "Hz", "class": "frequency", "state_class": "measurement" },
               { "key": "mode", "name": "Mode", "type": "enum", "options_ref": "modes",
                 "writable": true,
                 "command": { "action": "mode", "value_key": "value", "value_type": "string" } } ],
  "actions": [ { "key": "retract", "name": "Retract", "command": { "action": "retract" } } ]
}
```

---

## 4. Canonical vocabulary

- **Frequency in Hz is the single source of truth.** `band` is derived from `freq_hz`
  and published for convenience. There is no band *setpoint* on `/state`: `band` is never
  a stored value, always derived. A radio *may* accept a `set_band` `/cmd` input for
  native band-stacking (the radio restores its own persisted per-band frequency and the
  bridge republishes that `freq_hz` with `band` derived from it), but the canonical tuning
  intent is still `set_freq_hz` — commanders set a frequency and band falls out. This
  removes the class of bug where band and frequency disagree because two things set them
  independently.
- **Bands.** The canonical vocabulary owns the band name→frequency-range table (edges are
  region- and license-dependent; these are DL / IARU R1). A device declares the band
  *names* it supports and references this table rather than re-stating edges. In state,
  `freq_hz` is the truth and `band` is the derived name.
- **Modes:** `cw`, `usb`, `lsb`, `am`, `fm`, `data`. These are canonical names; firmware
  typically uses proprietary variants (`CW-U`, `CW-L`, `DIGU`, `DIGL`, `USB`, `LSB`,
  `FDV`, `RTTY-U`, …). **Normalization is the adapter's responsibility** — consumers see
  only the canonical names above. Adapters must publish a canonical mode or omit the
  field; they must never publish a raw firmware mode string.
- **Roles:** `radio`, `pa`, `tuner`, `ant-switch`, `ant-ctrl` (controller that tunes or
  steers a passive antenna resource — the antenna itself remains a passive resource),
  `rotator`, `pol-ctrl`, `preamp` (function; may be a slot or a declared capability),
  `bias-feed` (likewise), `reconciler`, `host`, and the power-control roles:
  `power` (a supply switch — smart plug / contactor cutting mains or DC to a bank; one
  on/off, declares `feeds`), `switch` (a generic multi-channel relay switch of
  non-exclusive booleans; each channel declares its `kind` — `remote_on`, `enable`, or
  `spare`; distinct from `ant-switch`, the exclusive 1-of-N selector), `pa-arm` (the
  PA-enable arm relay; safety, heartbeat-driven, fail-safe-open), and `sequencer` (a
  logic slot that runs an ordered, delay- and confirmation-based startup/shutdown over
  other slots' `/cmd`). Passive: `ant/*`, `mast/*`, `preamp/*` (masthead LNA).
- **Capability keys** seen so far: `bands`, `modes`, `receivers`, `diversity`,
  `amp_key`, `tune`, `bias_t`, `band_source`, `rf_sample`, `alc_out`, `hot_switch`,
  `ports`, `off`, `exclusive`, `axes`, `polarizations`, `feeds` (a `power` slot's
  downstream slot addresses), `channels` + `kind` (a `switch` slot's relay channels),
  `fail_safe` (`open`|`off` — the relay's de-energized/safe state), `heartbeat` (bool).
- **`band_source`** (PA): how the amplifier's band is determined — `cat` (band data
  fed from the radio over a CAT link), `rf_sense` (the amp auto-switches by sensing
  the RF drive), or `manual` (only via `/cmd set_band`). Distinct from `rf_sample`
  (whether the amp independently samples/reports RF power).

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
Passive-resource names (`ultrabeam`, `fan-dipole`, …) are free-form site-local
identifiers — they appear **only** in site configuration, never in adapter or
reconciler code.

**Controller map:** the wiring map's companion. A passive resource MAY have an active
controller slot that tunes or steers it (the Ultrabeam has an `ant-ctrl`; the fan
dipole has none). The reconciler's config maps resource name → controller slot
(`controllers { ultrabeam: ant-ctrl }`), and band/frequency-follow is emitted to
whatever slot the map names. This keeps follow logic generic: which antenna follows
the radio is site configuration, not code.

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
  - *Station power-up/-down:* the supplies and remote-on triggers must come up in an
    order with delays and liveness confirmations — mains before the network settles
    (~30 s), the 13.8 V supply before the controllers that boot from it can actuate
    anything, the radio before the PA, the PA powered before the arm permit. This
    ordering lives in a `sequencer` logic slot (§7.1), not in the device bridges; the
    bridges only report state. The sequencer is one writer of the power slots but does
    not lock them — a channel stays directly toggleable for troubleshooting.

---

## 7. Site instantiation — Mühle

Two stations, both `location: bauwagen`. Each station node is structural — a path
prefix over its members — and carries one field only:

```
muehle/hf   |   muehle/uhf
role:     station
kind:     { hf | uhf }
location: bauwagen
activity: { active | inactive }    # inferred from radio VFO/TX by antenna-select; read by ladder tier 1
```

Everything else a station might report (band, tx, readiness) already lives on the radio;
deriving it here would only duplicate. The switch's fail-safe-to-ground default covers
power loss independently of this flag.

### 7.0 Power infrastructure (site level)

The upstream supplies feed across both stations, so they sit at site level
(`site/power/<name>`, §2) — not under `hf/` or `uhf/`. Both are Shelly smart plugs,
formerly operated only through Home Assistant; brought onto the canonical bus by one
`shelly-power-bridge` on `shari` (one process, two slots — a compound bridge). HA is
thereafter one writer of these topics, not the control path (§9).

**`muehle/power/master`** — Shelly plug, station master mains
```
role: power;  device: { model: "Shelly Plug …", serial };  link: wifi;  host: shari
capabilities: fail_safe {off}                 # Shelly power-on default set to off
state:        power {on|off}                  # actual, read from the Shelly
intent:       set_power {on|off}             # /cmd retained (idempotent steady-state)
```
**`muehle/power/psu-13v8`** — Shelly plug, 13.8 V PSU
```
role: power;  device: { model: "Shelly Plug …", serial };  link: wifi;  host: shari
capabilities: fail_safe {off}; feeds [hf/radio, uhf/radio, hf/tuner, hf/ant-ctrl,
              hf/ant-switch, hf/rotator, hf/switch, hf/pa-arm]   # what this supply powers
state:        power {on|off}
intent:       set_power {on|off}             # /cmd retained
```
The 13.8 V supply is upstream of the HF radio, the UHF radio, the tuner, the ant-ctrl,
the ant-switch, the HF rotator, **and the M5 Stamp PLCs** — so it must be on before any
of them can actuate or report. The sequencer (§7.1) waits on their `/status` before
proceeding.

### 7.1 Station `hf`

**`muehle/hf/radio`** — FLEX-8400, ethernet
```
capabilities: bands [160m..6m, by name]; modes [cw,usb,lsb,am,fm,data];
              receivers 1; diversity false; amp_key true; tune true; bias_t false;
              rx_inputs [ant1,ant2,rx_a]; tx_outputs [ant1,ant2]
state:        online; freq_hz (Hz int); band(derived); mode (canonical); tx {rx|tx};
              tuning (bool); drive (0-100); device_online (radio link liveness);
              dvk_status {idle|recording|preview|playback|disabled}; dvk_id (1-12, active memory);
              mic_profile (active name, client-side tracked; empty until first load);
              mic_profiles (available names, sorted; /state-only, from `profile mic info`);
              rx_input (omitempty, adapter work pending)
              — always the active/TX receiver
intent:       set_freq_hz; set_mode; set_drive; select_rx; tune {start|stop};
              set_band {label}; dvk_play {1..12}; dvk_stop {id|active};
              set_mic_profile {name}   # /cmd NOT retained (one-shot)
```
`set_band` drives SmartSDR **native band-stacking**: the bridge sends
`display pan s <pan_handle> band=<wavelength>` (the wavelength is the band number —
`20m`→`20`), and the radio restores the last-used frequency/mode for that band. `band`
remains a derived `/state` field (§4): after a `set_band` the bridge republishes the
radio's tuned `freq_hz` with `band` derived from it, so band and frequency can never
disagree. Only the FLEX-8400's regular bands (`160m`–`6m`) are supported; XVTR bands
are out of scope.
DVK (Digital Voice Keyer) is a SmartSDR v4+ / SmartSDR+ feature: 12 voice memories keyed
to TX on playback. flexbridge is otherwise read-only; `set_band`, DVK, and mic profiles
are its only `/cmd` surfaces — DVK exposed as one-shot actions (`dvk_play_N` buttons +
`dvk_stop`), `set_band` as a writable `band` setpoint (a select), `set_mic_profile` as a
writable `mic_profile` setpoint — all observed on `/state` (`freq_hz`/`band`/`mode`,
`dvk_status`/`dvk_id`, and `mic_profile`/`mic_profiles` respectively). Voice modes only; the
radio refuses DVK in cw/data.
Mic profiles use SmartSDR's **native** `profile mic load "<name>"` — the radio is the
single source of truth, not a bridge-side preset layer. `set_mic_profile` loads a profile.
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

**`muehle/hf/pa`** — ACOM 1200S, serial (USB-serial; carries band/mode/telemetry, NOT keying)
```
capabilities: bands [...]; max_power_w <spec>; band_source rf_sense; rf_sample false;
              key_input hardware; alc_out true; modes [operate,standby]
state:        online; mode {operate|standby|bypass}; band; keyed {rx|tx|inhibited};
              fwd_power_w; rfl_power_w; temp_c; fault {none|swr|temp|reflected};
              power {on|off}              # read-only telemetry now
intent:       set_band; set_mode {operate|standby}; clear_fault
```
The PA's *own* power-on control line (a soft "remote power-on" input) is operated by a
relay on `muehle/hf/switch` (§7.1), not by the PA bridge: the ACOM's RTS wake-line path
was removed (it never worked). The PA slot only **observes** power — `power` is the
*actual* state from telemetry (`off` only when the amp reports OFF); it is no longer a
setpoint. Power-on/off of the PA is a power-distribution-layer concern; `pa.power`
reflects the result. (The `set_power`/RTS extension is retired; see §7.1 `hf/switch`.)

**`muehle/hf/ant-switch`** — wifi 1-to-6 (dumb actuator)
```
capabilities: ports [1,2,3,4,5,6]; off true; exclusive true; hot_switch false
state:        online; selected {off|port1..port6}; settled
intent:       select {off|port1..port6}
```

**`muehle/hf/tuner`** — ATR-1000 (BTR-1000 / N7DDC design), wifi (binary WebSocket)
```
capabilities: inline true; tune_modes [mem, full]
state:        online; inline (bool); swr (float, ratio); fwd (W, forward power);
              l_uh (inductance µH, diagnostic); c_pf (capacitance pF, diagnostic);
              settling (bool); fault (string, omitempty); device_online (bool)
intent:       set_inline {true|false}; tune {full|mem}
```
The ATU is engaged in-line only when its served resource is the resolved antenna AND the
band is non-resonant (e.g. 30/60/80/160 m on the fan-dipole); it is bypassed otherwise. The
`set_inline` intent is driven by `antenna-select` (see the soft binding below), gated on
radio online + a known band, and engages only while the tuner's resource is the resolved
target — the reconciler's cold-switch sequencing already withholds a port change during
TX, so the ATU is not re-keyed mid-TX.

**`muehle/hf/rotator`** — Yaesu G-450DC. Control path: AF6SA WRC, with bridge software
on `shari` presenting it to the automation. `capabilities: axes [az]`.

**`muehle/hf/ant-ctrl`** — antenna controller (role `ant-ctrl`; device: Ultrabeam
RCU-06), bridge on `shari`. Controls the `ant/ultrabeam` passive resource: follows band
and frequency; reports element/tuning state.

**`muehle/hf/rx-loop-ctrl`** — K9AY controller. Reports loop direction; the loop preamp
is a sub-state of this slot and is dropped by the hardware TX-low line.

**`muehle/hf/switch`** — M5 Stamp PLC #1, remote-on relays (compound with `pa-arm`;
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

**`muehle/hf/pa-arm`** — M5 Stamp PLC #1, the PA-enable arm relay (relay 1; compound with
`hf/switch`). Embedded safety node — device, adapter, and host in one (the formerly
planned M5Stack, §11.3, now realized as the M5 Stamp PLC).
```
capabilities: fail_safe open; heartbeat true; relay 1
state:        enabled bool; armed bool
              # enabled = software arm-permit (last /cmd); armed = enabled ∧ radio_online
              #   ∧ ¬radio.tuning ∧ band_safe ∧ heartbeat — the actual relay, fail-safe-open
intent:       set_enabled {true|false}         # /cmd retained (steady-state permit)
```
Arm decision is the embedded firmware's: it subscribes `hf/radio/state` for `tuning`/`band`
and computes `armed`; `armed` is never commanded directly (§6 software-arm-permit AND
hardware key). Heartbeat is own liveness plus radio reachability — radio offline ⇒
`armed` false; loss of 13.8 V ⇒ relay drops open (fail-safe). An operator/sequencer only
sets the `enabled` permit.

**`muehle/hf/power-seq`** — the station startup/shutdown sequencer (logic slot, `powerseq`
on `shari`).
```
role: sequencer  # logic slot, no device
subscribes: /status of power/master, power/psu-13v8, hf/switch, hf/pa-arm, hf/ant-switch,
            hf/radio, hf/pa; and hf/pa/state.power
state:      phase {idle|starting|running|stopping}; step <string>; fault (omitempty)
intent:     start | stop                       # operator one-button; /cmd NOT retained
```
Startup (confirmations + delays): `power/master` on → ~30 s (network) → `power/psu-13v8`
on → wait `hf/switch`, `hf/pa-arm`, `hf/ant-switch` `/status` online (they boot on 13.8 V)
→ `hf/switch` trx on → wait `hf/radio` `/status` online → `hf/switch` pa on → wait
`hf/pa` `power` on (the slot's `/status` must also be online, so a dead PA cannot pass
on a stale retained `/state`) → `hf/pa-arm` set_enabled true (arms when safe). Shutdown
is the reverse, with short staggers for inrush. The sequencer is **one writer** of these
slots but does not lock them — any channel stays directly toggleable for troubleshooting
while the sequencer is idle.

This sequence is **config-driven** in `powerseq` (the `[[startup]]` / `[[shutdown]]`
step lists in its TOML, each step `cmd` / `wait_status` / `wait_state` / `delay`); the
text above is the default shipped in `config.example.toml`, not hard-coded. The
subscribed topics and the `/meta` `controls`/`watches` are derived from the configured
sequence, so a different setup defines a different sequence with no Go change.

**`muehle/hf/antenna-select`** — reconciler/arbiter (logic slot)
```
subscribes: radio.band, radio.tx, radio.freq_hz, operator.request, ant-switch.selected
            (activity is inferred: a freq_hz change or tx == "tx" marks active; an idle
            timeout with neither marks inactive)
emits:      ant-switch.select; <controller>.cmd (band/freq follow, per controller map)
config:     wiring_map  { port1: dummy-load, port3: ultrabeam, port6: fan-dipole, off: grounded }
            controllers { ultrabeam: ant-ctrl }            # resource → controller slot
            band_policy { 6,10,12,15,17,20m: ultrabeam;
                          30,40,60,80m:     fan-dipole }   # 30/60/80/160m require the ATU
            priority   { 1 idle, 2 operator, 3 auto }
state:      mode {auto|manual}; target {off|port1..port6}; source {idle|operator|auto}
```

**`muehle/hf/discovery`** — HA discovery consumer (logic slot, `hadiscovery`; see §7.3, §9)
```
subscribes: <site>/+/+/meta  (reads each slot's consumer-neutral `expose` block, §3.1)
emits:      <discovery_prefix>/...  (Home Assistant discovery config; no /cmd, no slot state)
host:       shari
```

**Passive resources:** `ant/ultrabeam` (port 3), `ant/fan-dipole` 80/40 (port 2),
`ant/dummy-load` (port 1), `ant/k9ay-loop` (RX, via rx-loop-ctrl), `mast/ta16-hf`.

**Adapters:** the `pa`, `tuner`, HF `rotator`, and `ant-ctrl` bridges run on host
`shari`; `pa-arm` + `switch` are a self-hosted M5 Stamp PLC; the Shelly power bridge and
the `power-seq` sequencer run on `shari`. See §7.3. The PA bridge talks to the ACOM
1200S over USB-serial (Prolific, 9600 8N1) for telemetry and manual `/cmd set_band`
band-walk commands. There is no CAT band-data cable in this adapter; the amp auto-bands
by sensing the RF drive (`band_source: rf_sense`), so band-follow is hardware RF-sense
by default — the `pa.set_band ← radio.band` soft binding below is an optional software
override / pre-position path, not the primary follow mechanism.

**Soft bindings (reconciler, state → intent):**
- `pa.set_band` ← `radio.band`
- `tuner.set_inline` ← `band_policy` (engage the ATU in-line when the selected resource is
  its resource and the band is non-resonant; bypass otherwise — closes the §10 residual)
- `ant-ctrl.{band,freq}` ← `radio.{band,freq}` (via the controller map)
- `ant-switch.select` ← arbiter(band_policy, wiring_map, ladder)
- `ant-switch.select` → `off` when the idle timeout elapses with no VFO change or TX
  (ladder tier 1; walk-away lightning protection)
- `pa-arm.armed` ← `enabled ∧ radio_online ∧ ¬radio.tuning ∧ band_safe ∧ heartbeat ∧
  antenna_ready` (realized in the M5 Stamp firmware, not the reconciler; fail-safe open,
  heartbeat-driven; `antenna_ready` drops the arm when the antenna is grounded/off)
- `power-seq.{start,stop}` ← operator one-button; the sequencer then issues the ordered
  chain `power/master → power/psu-13v8 → hf/switch.trx → hf/switch.pa → hf/pa-arm` on
  start (reverse on stop), gated on liveness confirmations and delays (§6, §7.1)

**Hardware interlock chain (enforces; software mirrors only):**
```
13.8 V PSU → M5 Stamp PLCs + ant-switch (boot on supply) → radio (remote-on) → pa (remote-on)
radio (TX low) → rx-loop-ctrl (preamp off) → ant-ctrl (inhibit if moving) → pa-arm → pa
```
The first line is the power-on dependency: the supply must be up before the controllers
can actuate or the radios can be triggered. The second is the RF safety chain; `pa-arm`
sits in it (the arm relay ANDs with the hardware key line), fail-safe-open.

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

**`muehle/uhf/pol-ctrl`** — M5 Stamp PLC #2 with custom firmware. `capabilities:
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
| Antenna controller bridge (Ultrabeam) | `hf/ant-ctrl` | `shari` |
| PSTRotator (serial) | `uhf/rotator` (SPID) | `shack-pc` |
| FLEX bridge (flexbridge) | `hf/radio` | `shari` |
| HF antenna-select reconciler | `hf/antenna-select` | `shari` |
| Logging | subscriber, no slot | `shari` |
| Shelly power bridge (`shelly-power-bridge`) | `power/master`, `power/psu-13v8` | `shari` |
| M5 Stamp #1 firmware (`m5stamp-hf-ctrl`) | `hf/pa-arm`, `hf/switch` | embedded (M5 Stamp PLC) |
| M5 Stamp #2 firmware | `uhf/pol-ctrl` | embedded (M5 Stamp PLC) |
| Power sequencer (`powerseq`) | `hf/power-seq` | `shari` |
| HA discovery bridge (optional, `hadiscovery`) | consumer of `meta`/`state`/`status` | `shari` |

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

**Broker topology:** the station runs a **shack-local Mosquitto on shari**
(`mqtt-broker/`) as the authoritative broker for `muehle/#`, so the station
keeps a working bus even when the shack↔house link is down. A mosquitto
`bridge` connection replicates `muehle/#` to the Home Assistant broker at
`192.168.1.50:1883` (HA's own Mosquitto add-on, left untouched — it still serves
HA's other MQTT devices). HA is still the reference consumer (§9): the bridge
forwards slot `state`/`meta`/`status` and the discovery config `hadiscovery`
renders out, and forwards HA's birth + `cmd` in. See
`conventions/mqtt-topology.md` for the topic-direction table and ACLs.

### 8.1 Adapter conformance checklist

The strict target this document promises. An adapter is conformant iff every line
holds; a review (human or GenAI) checks these one by one.

1. Publishes exactly the four planes: `/meta`, `/state`, `/status` retained; `/cmd`
   subscribed, and retained only under the §8 self-healing-actuator exception (with
   one-shot commands cleared after execution). Read-only slots omit `/cmd`.
2. QoS 1 on every publish and subscribe (QoS 2 only for a documented must-not-double-fire
   command).
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
   `band-N` fallback; modes normalized to canonical names or the field omitted — never a
   raw firmware string.
8. No publish under any consumer tree (`homeassistant/…` etc.) — see §9 and its
   currently-documented deviation.
9. Config in a single seed-once TOML (0600 when it holds secrets), secrets never on the
   command line; hardened systemd unit (see `conventions/`).

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

**Known deviation (accepted, temporary):** flexbridge and ultrabridge historically published
Home Assistant discovery under `homeassistant/…` from within the core adapter (ultrabridge also
subscribed `homeassistant/status` to re-announce). That embedded discovery is now **gated
off** (`publish_ha_discovery = false`, default false) — by default they no longer publish
it. The invariant stands; the target is a standalone HA-discovery consumer that reads
`/meta` + `/state` and emits discovery for every slot, at which point the embedded code is
deleted.

**Resolution in progress:** the standalone consumer is `hadiscovery` (`muehle/hf/discovery`),
a passive service that reads the consumer-neutral `expose` block (§3.1, Appendix C) from
each slot's `/meta` and renders HA discovery. Components publish `expose` in their own
`/meta` and carry **no** HA knowledge; HA is one renderer of the same surface any other
consumer could read. The embedded discovery in flexbridge/ultrabridge is gated off
(`publish_ha_discovery = false`, default false) and slated for deletion once `hadiscovery`
is proven live. Until then the embedded discovery must remain config-gated and
non-load-bearing: with discovery disabled (or HA absent), the canonical planes are
unaffected and the station runs identically.

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
  to the Ultrabeam. Two riders: 30/60 m on the 80/40 dipole are not resonant, so
  the policy implicitly requires the ATU in-line and tuning on those bands; and 160 m has
  no antenna assigned. **Resolved:** unmatched bands (incl. 160 m) route to a named
  fallback — the fan-dipole (via the ATU). Configured as `band_policy.fallback` in the
  `antenna-select` reconciler. The ATU is engaged in-line for the non-resonant bands by
  the `tuner.set_inline ← band_policy` soft binding (§7.1), driven by the `antenna-select`
  `[tuner_follow]` block against the `hf/tuner` slot (the former "tuner is assumed but
  never driven" residual is closed).
- **Hosts are now single points too.** `shari` fronts the HF PA, tuner, rotator, and
  Ultrabeam, and also hosts the `antenna-select` reconciler and logging; `shack-pc`
  fronts only the VHF rotator (PSTRotator). A host loss takes its whole cluster offline
  at once — correctly, via last-will, but simultaneously. Host liveness is load-bearing
  and worth monitoring; the reconciler running on `shari` compounds that host's
  coordination single point (see §11).
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
an embedded M5 Stamp PLC; band policy set; tune routing through the automation). Residuals
surfaced by those answers:

1. ~~**160 m antenna.**~~ Resolved: no dedicated 160 m antenna. Unmatched bands (incl.
   160 m) fall back to the fan-dipole via the ATU (`band_policy.fallback` in
   `antenna-select`). Revisit if a resonant 160 m antenna is added.
2. ~~**`shack-pc` roles.**~~ Resolved: shari hosts flexbridge (FLEX bridge), the reconciler,
   and logging. shack-pc hosts PSTRotator only.
3. ~~**`pa-arm` build.**~~ Resolved: realized as **M5 Stamp PLC #1** (`m5stamp-hf-ctrl`),
   a compound embedded node publishing both `hf/pa-arm` (arm relay 1) and `hf/switch`
   (remote-on relays 3 & 4). Formerly planned as a discrete M5Stack; the M5 Stamp PLC
   supersedes it.

---

## 12. Deferred

The reconciler's internal implementation; horstprop Layer-3 modelling; multi-multi
arbitration beyond the single-station ladder; and the mechanical field layouts for the
slots still marked TBD in §7 (`rx-loop-ctrl`, `uhf/radio`, the host-liveness nodes).
None of these change the shapes above.

**Live meter data (VITA-49).** The real-time meter stream from the radio (S-meter, SWR,
PA power, temperatures — decoded from VITA-49 datagrams at 10–20 fps) was intentionally
removed from the flexbridge bridge: it is high-rate, non-retained, and belongs to a
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
{ "action": "set_band", "value": "20m" }
{ "action": "set_mic_profile", "value": "Default ProSet HC6" }
```
(Band-stacking: the radio restores the last-used frequency/mode for `20m`; `band` stays
derived from `freq_hz` on `/state`. A direct `set_freq_hz` is the model's canonical tuning
intent; flexbridge exposes `set_band` as the operator-facing convenience. `set_mic_profile`
drives SmartSDR's native mic profile load; the available list is queried via
`profile mic info` and observed on `/state.mic_profiles`, and the active name is tracked
client-side on `/state.mic_profile` since SmartSDR reports no active mic profile. There is no
save — `profile mic save` is obsolete on SmartSDR v4+.)

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
  `select_rx`, `set_band`) is a direct intent. `set_band` is the band-stacking convenience
  (§radio slot): it changes the band and lets the radio pick the frequency, so `/state`
  stays frequency-derived rather than carrying a band setpoint.

---

## Appendix C — the `expose` schema (normative)

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
| `unit` | no | unit string (`Hz`, `%`, `°C`, `W` …) — for `number` |
| `class` | no | semantic hint (`frequency`, `temperature`, `power`, …) — generic, not HA-specific |
| `state_class` | no | `measurement` \| `total` \| `total_increasing` |
| `options` | no* | inline enum option list |
| `options_ref` | no* | key into `capabilities` whose array is the option list (e.g. `"modes"`) — single-sources the enum |
| `writable` | no | if true, the field is a setpoint/command target, not just a sensor |
| `command` | no | required when `writable`: how a write is encoded on `/cmd` (see below) |
| `on` / `off` | no | for `boolean`: the payload strings `/state` actually holds (e.g. `tx`/`rx`). Absent ⇒ state holds a real bool. |
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
| `action` | optional `/cmd` JSON `action` value (e.g. `"frequency"`, `"mode"`, `"retract"`) |
| `value_key` | the JSON key under which the user-supplied value is placed (e.g. `"freq_hz"`, `"value"`, `"select"`) |
| `value_type` | `string` \| `int` \| `float` — how the consumer coerces the value |

The consumer renders this into its own command syntax. The three observed shapes:
- `action` + `value_key`: `{"action":"<action>","<value_key>":<value>}` — e.g. mode ⇒
  `{"action":"mode","value":"cw"}`; frequency ⇒ `{"action":"frequency","freq_hz":14074000}`
- `value_key` only: `{"<value_key>":<value>}` — e.g. ant-switch ⇒ `{"select":"port2"}`
- `action` only (button): `{"action":"<action>"}` — e.g. `{"action":"retract"}`

### How a consumer maps `expose` (informative, non-normative)

A consumer derives the topic planes from the slot address: `state_topic = <addr>/state`,
`command_topic = <addr>/cmd`, availability = `<addr>/status`. The neutral type maps to the
consumer's entity kinds; e.g. for Home Assistant: `number` writable→`number`, `number`
readonly→`sensor`, `enum` writable→`select`, `enum` readonly→`sensor`, `boolean`→
`binary_sensor`, `action`→`button`. The unit maps to a device class via the consumer's own
taxonomy. The HA-specific mapping lives in `hadiscovery/docs/discovery-mqtt-api.md`, not
here — the model stays consumer-neutral.
