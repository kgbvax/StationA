# 07 — Priorities and Tentative Milestones

Status: draft, owner-reviewed input needed on the open decisions in §7.
Audience: the re-implementation team and whoever plans its schedule.

This document is the *reconstruction plan*. It gives the order in which to rebuild the
system, what each phase must show before the next starts, and which requirements are
non-negotiable. It assumes the reader has skimmed `00-system-overview.md` and
`01-architecture.md`.

---

## 1. What drives the priorities

The system is a station that switches mains power and routes radio-frequency energy
into antennas. Two facts drive every priority call:

1. **Physical safety comes first.** A wrong antenna switch while transmitting can
   destroy an amplifier in seconds. An ungrounded antenna is a lightning risk.
   Anything whose failure is physical (the PA arm chain, the antenna switch, the power
   sequence) outranks anything whose failure is merely inconvenient (dashboards,
   discovery integrations).
2. **The bus is the platform.** Components never talk to each other directly — they
   integrate only through the message bus (see `01-architecture.md`). So the wire
   contract (`02-interface-spec.md`) plus the small runtime-plumbing rules
   (`03-components/common-runtime-library.md`) must exist and stay *testable in
   isolation* before any component depends on them.

A third rule follows from the station being live. The re-implementation can run
shadow tests against the running system. The existing deployment keeps publishing
the same bus traffic. A new component can observe the same broker, get a
message-for-message comparison, and cut over one slot at a time (§5.3).

### Priority classes

| Class | Meaning | Examples |
|---|---|---|
| **P0 — safety-critical** | Failure causes physical equipment damage or an unsafe station state. | PA arm relay chain, antenna switch cold-switch rule, power sequencing |
| **P1 — core operation** | Failure means the station cannot operate, but fails safe. | radio bridge, power bridge, bus + runtime plumbing, sequencer, antenna policy |
| **P2 — operator experience** | Failure degrades day-to-day usability. | operator console, rotator bridges, Home Assistant discovery |
| **P3 — diagnostics and tooling** | Dev/ops support. Not on the operating path. | bus capture tool, bus traffic stimulator/checker, design assets |
| **P4 — deferred / not necessary** | Specified or prototyped, but not on anyone's critical path. | logging layer (specified, never implemented), UHF bench tool, polarization firmware |

---

## 2. Component priority table

The table shows *hard* dependencies: the component cannot function, and tests cannot
run, without them. This is not merely historical build order.

| Priority | Component (spec) | Depends on | Why this priority |
|---|---|---|---|
| P0 | PA arm / relay controller (`03-components/m5stamp-relay-controller.md`) | bus, radio slot state, ant-switch state. | The one component whose job is to *stop* transmission when unsafe. Fail-safe-open relay, 10 s heartbeat rule. |
| P0 | 1:6 antenna switch (`03-components/antenna-switch.md`) | bus, embedded hardware. | Cold-switch safety (the `settled` guard) is load-bearing. It is also the actuator that everything routes through. |
| P0 | Safety spec sign-off (`06-safety.md`) | — | The team audits every phase's acceptance against this document. |
| P1 | Common runtime plumbing (`03-components/common-runtime-library.md`) | — | Every other component embeds it. The team writes it first. |
| P1 | Wire contract + golden tests (`02-interface-spec.md`) | broker. | It is the compliance harness every component must pass (§5.1). |
| P1 | Slot state simulator (dev tool, no spec — see M1 in §3) | bus, wire contract. | Drives the M2 safety-gate fault injection before the real radio and PA bridges exist (M3). A dev deliverable, not a station component. |
| P1 | Power bridge, master mains + 13.8 V PSU (`03-components/shelly-power-bridge.md`) | bus. | Everything downstream is dead without power. It is also the simplest bridge — the natural first integration proof. |
| P1 | Power sequencer (`03-components/powerseq.md`) | power bridge, switch + arm controllers online, radio + PA slots. | The one-button start/stop is the operator's primary entry point. Its waits encode the liveness contract. |
| P1 | Radio bridge (`03-components/flexbridge.md`) | bus, radio hardware. | The radio is the station's purpose. Its state feeds the arm chain (P0). |
| P1 | Antenna policy reconciler (`03-components/antennaselect.md`) | radio state, ant-switch, ant-ctrl, PA, tuner slots. | The central routing brain, including idle grounding. The team must *fix* the live-proven fragilities here, not copy them. |
| P1 | Beam antenna controller bridge (`03-components/ultrabridge.md`) | serial hardware, bus. | Needed for 6–20 m operation. Its USB self-heal is a normative requirement. |
| P1 | Amplifier bridge (`03-components/acom1200s-pa-bridge.md`) | serial hardware, bus. | TX power. Telemetry-only power contract. |
| P1 | Tuner bridge (`03-components/atr1k-tuner-bridge.md`) | websocket hardware, bus. | The fan dipole needs it for 30/60/80/160 m. |
| P2 | Operator console (`04-console.md`) | every slot it displays. | The operator's window. M4 defines the MVP subset. |
| P2 | HF rotator bridge (`03-components/wrc-rotator-bridge.md`) | WRC hardware, bus. | Beam steering. The station transmits fine without it. |
| P2 | Home Assistant discovery (`03-components/hadiscovery.md`) | every slot's `/meta`. | Convenience integration. Purely passive. |
| P2 | UHF rotator (`03-components/pelcobridge2.md`) | RS-485 head, shack PC. | Separate UHF chain. The manual-arm safety model must stay exactly the same. |
| P3 | Bus capture tool, bus traffic stimulator/checker (`05-deployment-ops.md` §4) | broker. | Dev tooling. The stimulator/checker's security defect is a normative fix requirement. |
| P4 | Logging layer (per `docs/logging-integration-model.md`, in the existing repo) | bus. | Specified but never implemented — treat it as new feature work, lowest priority. |
| P4 | PLC #2 polarization firmware (`00-system-overview.md` §5) | embedded hardware. | Documented gap: the slot table attributes it there, but no firmware ever existed. |

---

## 3. Tentative milestones

Each milestone ends with an acceptance gate (§5). Durations are rough, assuming a
small team (2–3 engineers) working with hardware access. They are *sequence*
promises, not calendar promises.

### M0 — Decisions and verification (before any code)

Goal: retire every open decision that can otherwise force rework.

- Confirm on the live host (`shari`, Raspberry Pi) the deployed antenna wiring map.
  The beam antenna sits on switch port 3 or 4 — the sources disagree (see
  `06-safety.md` §8). Confirm also the deployed sequencer step lists, the deployed
  amplifier serial poll rate, and the current broker topology. It points at
  192.168.1.50 now. The team committed the shack-local broker migration but never
  deployed it.
- Decide the `device_online` payload form, and write the decision into
  `02-interface-spec.md`. The form is publish-explicit-`true` or omit-when-true. The
  re-implementation must pick one form. Consumers must accept both forms.
- Decide the radio state heartbeat mechanism. The reference radio bridge publishes
  state only on change, which starves the P0 arm heartbeat (a live incident). The
  re-implementation **must** add a periodic republish (≤ 5 s) or a mechanism with the
  same effect — a normative change, not a copy.
- Decide the console design direction. The design exploration produced four
  candidates and one content model (see `04-console.md` §6).
- Choose the new technology stack per component class: application services, embedded
  firmware, console app. Confirm that it can meet the runtime constraints in
  `03-components/common-runtime-library.md` (abortable connect,
  never-block-in-handler, retained-replay recovery).
- Inventory the physical/electrical facts the repo does not hold: relay contact
  ratings, whether an external relay grounds switch "off", the UHF head DIP
  settings. See each component's "Open decisions".
- Decide the operator tune-request routing (`01-architecture.md` OD-20): a
  sequencer action, a dedicated sequencer step, or another owner. No tune path
  exists on the deployed bus, so this is a contract addition.
- Decide who publishes the station-activity topic (`<station>/state` with the
  `activity` flag, `01-architecture.md` §7.3). The grounding ladder in the
  reconciler reads this state, so the team must name the owner before M3.
- Capture the real per-slot device identity strings (model, serial, firmware
  version) from the live bus, and keep them as reference data. The M1 shadow
  comparison checks these fields against this capture.

**Acceptance M0:** every item in the open-decision sections has either a written
resolution or an explicitly accepted assumption with an owner. The gate sweeps the
union of all open-decision sections:

- `00-system-overview.md` §7
- `01-architecture.md` §10 (OD-1 to OD-20), including the tune-routing decision
  (OD-20)
- `02-interface-spec.md` §7
- `04-console.md` §7
- `06-safety.md` §8

The station-activity topic decision (`01-architecture.md` §7.3) is part of this
gate even though it is not a numbered OD item.

### M1 — Bus foundation (the platform)

Goal: the platform everything else stands on, proven with the simplest real
component.

- Stand up a broker, or reuse the existing one per the M0 topology decision. Access
  credentials follow the config/secrets convention (`05-deployment-ops.md` §2).
- Common runtime library in the new stack: abortable connect, bounded job queue +
  single serialized worker, topic builders, `{"action","value"}` command payload
  helper.
- **Golden wire tests** (§5.1): a test harness that publishes/records real bus
  traffic and validates payloads against `02-interface-spec.md`.
- **Slot state simulator** (a dev tool, P1 — not a station component): a small
  test tool that publishes valid `/meta`, `/state`, and `/status` snapshots for
  the radio and PA slots. A script can drive it to stall a slot, flip single
  fields, or go silent. The M2 safety gate uses it for fault injection until
  the real M3 components exist.
- Power bridge (both smart plugs) — the first real component. It proves the whole
  plane pattern (meta/state/status/cmd, two-layer liveness, fire-and-observe
  confirmation) end to end.

**Acceptance M1:** golden tests green. The power bridge runs as a shadow against the
incumbent on the live broker. The comparison checks fields, types, and values
against `02-interface-spec.md`. It is not byte-comparable: the PRD does not fix
JSON key order, and device identity strings come from the M0 reference capture.
A kill -9 of the power bridge leaves the bus in a state that consumers correctly
read as offline.

### M2 — Safe-to-transmit chain (P0 hardware layer)

Goal: the station physically cannot transmit unsafely, even with everything else
absent.

- Antenna switch firmware (exclusive selection, readback, 200 ms settle guard,
  boot-all-off + retained-command self-heal).
- Relay controller (arm formula, fail-safe-open, heartbeat window, per-slot
  connections + last-wills). Interim option: keep the incumbent embedded nodes
  running, and only re-check them against `06-safety.md`.
- Power sequencer with its full startup/shutdown step lists (config-driven).

**Acceptance M2 (safety gate — the most important one):** scripted fault injection on
the bench. The M1 slot state simulator drives the radio and PA slots until the real
M3 bridges exist. The procedure kills the radio-slot publisher, drops the
PSU slot, ages the radio state past 10 s, and removes the arm enable. The arm relay
de-energizes in every case. No port change ever happens while RF is present. Relays
always boot to the all-off state. The sequencer refuses to go on when a wait's
precondition slot is offline. The team signs off against `06-safety.md` line by line.

### M3 — HF operating core

Goal: a fully operational HF station on the new stack.

- Radio bridge (including the M0 heartbeat fix), beam controller (with the USB
  re-enumeration self-heal), amplifier bridge, tuner bridge.
- Antenna policy reconciler with arbitration ladder and idle grounding. The two live
  fragilities are normative requirements to fix here: heartbeat starvation and
  first-key-up-after-ground.

**Acceptance M3:** a live operating session shadowing the incumbent. Band changes
route the correct antenna. The tuner engages exactly on the non-resonant band set.
The 30-minute idle grounding fires. TX through the arm chain works. One slot at a
time cuts over to the new stack here (power first, radio last, see §5.3).

### M4 — Operator console

Goal: the operator stops needing the old console.

- MVP slice first: station power + sequencer, radio (including voice keyer + mic
  profile), PA + arm, antenna routing, faults bar, the LINK DOWN banner, and the
  cross-panel RF safety guard (fail-closed).
- Then: tuner, beam direction, rotator compass + Mercator DX map with the independent
  spot feed (SSE, dedup, aging, reprojection), offline stamping, silent-slot
  reporting.
- **Spot-feed recording fixture**: a captured spot-feed sample from the live feed,
  stored with the expected reprojection output. The team records it once, before
  the overlay work starts. The acceptance test below runs against this fixed
  recording, never against the live feed.

**Acceptance M4:** an operator runs a full contest-style session from the new console
alone. A scripted test tries an unsafe antenna move, and the safety guard blocks it.
The spot overlay matches the expected output of the M4 recording fixture.

### M5 — Rotators, discovery, UHF

- HF rotator bridge (quoted-string bearing quirk). Decide whether to keep the two
  legacy inbound control paths — an open decision in its spec.
- Home Assistant discovery renderer (passive, exposure-driven).
- UHF rotator with its **manual-only arming** preserved exactly (never remote, never
  persisted, motion locked unless armed).
- **gpredict/rotctl compatibility test suite**: the team writes the test-case list
  from the rotctld contract (`03-components/pelcobridge2.md` §9.1) and the
  measured head behavior (`03-components/pelcobridge2.md` §3.5). The suite is a
  deliverable of this milestone, produced before the acceptance run.

**Acceptance M5:** the gpredict/rotctl compatibility test suite produced in this
milestone passes. Discovery entities appear and clear correctly across a Home
Assistant restart.

### M6 — Tooling, hardening, and the deferred layer

- Bus capture tool and bus traffic stimulator/checker. The stimulator/checker
  **must** fix the incumbent's committed-credential and unauthenticated-endpoint
  defects — normative.
- Deployment automation for the whole park (seed-once config, hardened units, per
  `05-deployment-ops.md`).
- Only if wanted: the logging layer (contact-log and spot events) — a
  specified-but-never-implemented feature. Treat it as greenfield.
- Only if wanted: PLC #2 polarization firmware (now a documented gap).

**Acceptance M6:** the team can provision a clean host from scratch by following
`05-deployment-ops.md` alone.

---

## 4. What the re-implementation deliberately does not copy

The reference implementation is *wrong* in these places. The PRD mandates a fix for
each, and each is normative in the linked spec:

1. **Radio state heartbeat starvation** — the arm chain drops the amplifier arm when
   the radio is idle-but-healthy, because state is change-only
   (`03-components/m5stamp-relay-controller.md`, `06-safety.md` §2). The
   re-implementation adds a periodic republish.
2. **First key-up after grounding goes into the short** — the grounding recovery
   race (`03-components/antennaselect.md`).
3. **JSON-boolean command values silently disarm the arm relay** — the command
   value convention needs structural enforcement
   (`03-components/m5stamp-relay-controller.md`).
4. **Bus stimulator/checker's committed credential + unauthenticated publish
   endpoint** (`05-deployment-ops.md` §4).
5. **No drop-telemetry in the job queue** — silent job drops are invisible. The new
   runtime library must log them (`03-components/common-runtime-library.md`).

---

## 5. Test and cutover strategy

### 5.1 Golden wire tests (M1, continuous)

A harness that, for every slot, records a canonical traffic sample (meta envelope,
state snapshots across transitions, status flips, command round trips) and validates
any candidate implementation against `02-interface-spec.md`. The checks: topic
strings, JSON field names/types/units, retention/QoS flags, heartbeat cadence,
last-will behavior. The team writes these tests *from the spec, not from the
incumbent*. The incumbent's known defects (§4) must fail them.

### 5.2 Bench hardware-in-the-loop (M2, M5)

The team tests the P0 chain with real relays and dummy loads, never simulation only.
M2's acceptance gate lists the fault injection cases. The UHF head's measured
protocol quirks — unusable tilt readback, ignored absolute-set opcodes
(`03-components/pelcobridge2.md`) — only bench tests can check them.

### 5.3 Shadow-then-cutover deployment (M3 onward)

The bus is the only integration surface. A new component can therefore run
*alongside* the incumbent. It subscribes to the same broker but publishes under a
shadow topic root, and a diff compares it message-for-message. Cutover is per slot,
power chain first, the radio last (the slot with the most consumers). Each cutover
keeps the incumbent switchable back for one operating session.

### 5.4 Operating-session acceptance (M3, M4)

The final gate for anything operator-facing is a real session. Start the station
with one button. Operate on several bands across both antennas. End with the station
grounded. Review the capture log afterwards for contract violations.

---

## 6. Risks

| Risk | Impact | Mitigation |
|---|---|---|
| Port-number wiring ambiguity unresolved | Wrong antenna routed. Potential transmitter-into-open-circuit. | M0 gate: read the live deployed config on the host. Until then treat it as unknown, and block any physical antenna test. |
| Re-implementation library lacks abortable connect / non-blocking handlers | Shutdown hangs under broker outage (live incident class). | The M0 stack decision explicitly checks against the `common-runtime-library.md` constraints. |
| Radio protocol edge cases (voice keyer commands undocumented in the vendor API, and a two-word panadapter status topic) | Silent feature loss. | The team catalogues these per component. It keeps the vendor-undocumented features isolated behind their own tests. |
| New stack team lacks ham-radio context | Misread requirements (for example, why a dummy load exists). | Every doc defines terms at first use. `00-system-overview.md` is the mandatory first read. The station owner reviews M2's safety gate. |
| The live station is the test environment | A cutover incident interrupts operation. | Shadow-then-cutover (§5.3). Never cut over more than one slot per session. |
| Specified-but-unimplemented features drift into scope creep | Schedule. | Logging layer and PLC #2 are explicitly P4. |

---

## 7. Open decisions blocking this plan

Everything in the M0 gate feeds M0. The gate sweeps the union of the open-decision
sections: `00-system-overview.md` §7, `01-architecture.md` §10 (OD-1 to OD-20),
`02-interface-spec.md` §7, `04-console.md` §7, and `06-safety.md` §8. The items
with schedule impact:

1. **Switch port wiring** (beam on port 3 or 4) — blocks M2 acceptance.
2. **Broker topology** (existing 192.168.1.50, or the planned shack-local broker) —
   blocks M1. A broker move before reconstruction starts changes every component's
   config default.
3. **Radio heartbeat mechanism** (periodic republish, or a consumer-side freshness
   check) — changes the radio bridge's contract. Decide before M3. The fix
   direction (producer-side) is already normative here.
4. **Console design direction** — blocks the M4 visual pass only, not the data
   layer.
5. **Keep or drop the two legacy rotator control paths** (GS-232B TCP,
   PSTRotator UDP) — blocks only the HF rotator in M5.
6. **Operator tune routing** (`01-architecture.md` OD-20) — blocks M3. Until the
   tune request has an owner, the team cannot finalize the sequencer's step lists.
7. **Station-activity topic** (who publishes `<station>/state`,
   `01-architecture.md` §7.3) — blocks M3. The reconciler's grounding ladder
   reads this state.