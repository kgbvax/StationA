# 07 — Priorities & Tentative Milestones

Status: draft, owner-reviewed input needed on the open decisions in §7.
Audience: the re-implementation team and whoever plans its schedule.

This document is the *reconstruction plan*: in what order to rebuild the system,
what each phase must demonstrate before the next starts, and which requirements
are non-negotiable. It assumes the reader has skimmed `00-system-overview.md`
and `01-architecture.md`.

---

## 1. How priorities were assigned

The system is a station that switches mains power and routes radio-frequency
energy into antennas. Two facts drive every priority call:

1. **Physical safety comes first.** A wrong antenna switch while transmitting can
   destroy an amplifier in seconds; an ungrounded antenna is a lightning risk.
   Anything whose failure is physical (the PA arm chain, the antenna switch, the
   power sequence) outranks anything whose failure is merely inconvenient
   (dashboards, discovery integrations).
2. **The bus is the platform.** Components never talk to each other directly —
   they integrate only through the message bus (see `01-architecture.md`). So the
   wire contract (`02-interface-spec.md`) plus the small runtime-plumbing rules
   (`03-components/common-runtime-library.md`) must exist and be *testable in
   isolation* before any component that depends on them.

A third rule follows from the station being live: **the re-implementation can be
shadow-tested against the running system.** The existing deployment keeps
publishing the same bus traffic, so a new component can be built to observe the
same broker, be compared message-for-message, and be cut over one slot at a time
(§5.3).

### Priority classes

| Class | Meaning | Examples |
|---|---|---|
| **P0 — safety-critical** | Failure causes physical equipment damage or an unsafe station state | PA arm relay chain, antenna switch cold-switch rule, power sequencing |
| **P1 — core operation** | Failure means the station cannot be operated, but fails safe | radio bridge, power bridge, bus + runtime plumbing, sequencer, antenna policy |
| **P2 — operator experience** | Failure degrades day-to-day usability | operator console, rotator bridges, Home Assistant discovery |
| **P3 — diagnostics & tooling** | Dev/ops support; not on the operating path | bus capture tool, bus monitor/stimulator, design assets |
| **P4 — deferred / optional** | Specified or prototyped, but not on anyone's critical path | logging layer (specified, never implemented), UHF bench tool, polarization firmware |

---

## 2. Component priority table

Dependencies are *hard* (the component cannot function or be tested without
them), not merely historical build order.

| Priority | Component (spec) | Depends on | Why this priority |
|---|---|---|---|
| P0 | PA arm / relay controller (`03-components/m5stamp-relay-controller.md`) | bus, radio slot state, ant-switch state | The one component whose job is to *prevent* transmission when unsafe; fail-safe-open relay, 10 s heartbeat rule |
| P0 | 1:6 antenna switch (`03-components/antenna-switch.md`) | bus, embedded hardware | Cold-switch safety (`settled` guard) is load-bearing; also the actuator everything routes through |
| P0 | Safety spec sign-off (`06-safety.md`) | — | Every phase's acceptance is audited against this document |
| P1 | Common runtime plumbing (`03-components/common-runtime-library.md`) | — | Every other component embeds it; must be first code written |
| P1 | Wire contract + golden tests (`02-interface-spec.md`) | broker | The compliance harness every component must pass (§5.1) |
| P1 | Power bridge, master mains + 13.8 V PSU (`03-components/shelly-power-bridge.md`) | bus | Everything downstream is dead without power; also the simplest bridge — the natural first integration proof |
| P1 | Power sequencer (`03-components/powerseq.md`) | power bridge, switch + arm controllers online, radio + PA slots | The one-button start/stop is the operator's primary entry point; its waits encode the liveness contract |
| P1 | Radio bridge (`03-components/flexbridge.md`) | bus, radio hardware | The radio is the station's purpose; its state feeds the arm chain (P0) |
| P1 | Antenna policy reconciler (`03-components/antennaselect.md`) | radio state, ant-switch, ant-ctrl, PA, tuner slots | Central routing brain incl. idle grounding; live-proven fragilities must be *fixed* here, not copied |
| P1 | Beam antenna controller bridge (`03-components/ultrabridge.md`) | serial hardware, bus | Needed for 6–20 m operation; its USB self-heal is a normative requirement |
| P1 | Amplifier bridge (`03-components/acom1200s-pa-bridge.md`) | serial hardware, bus | TX power; telemetry-only power contract |
| P1 | Tuner bridge (`03-components/atr1k-tuner-bridge.md`) | websocket hardware, bus | Required for 30/60/80/160 m on the fan dipole |
| P2 | Operator console (`04-console.md`) | every slot it displays | The operator's window; MVP subset defined in M4 |
| P2 | HF rotator bridge (`03-components/wrc-rotator-bridge.md`) | WRC hardware, bus | Beam steering; station transmits fine without it |
| P2 | Home Assistant discovery (`03-components/hadiscovery.md`) | every slot's `/meta` | Convenience integration; purely passive |
| P2 | UHF rotator (`03-components/pelcobridge2.md`) | RS-485 head, shack PC | Separate UHF chain; manual-arm safety model must be preserved exactly |
| P3 | Bus capture tool, bus monitor/stimulator (`05-deployment-ops.md` §4) | broker | Dev tooling; the monitor's security defect is a normative fix requirement |
| P4 | Logging layer (per `docs/logging-integration-model.md`, in the existing repo) | bus | Specified but never implemented — treat as new feature work, lowest priority |
| P4 | PLC #2 polarization firmware (`00-system-overview.md` §5) | embedded hardware | Documented gap: attributed in the slot table, no firmware ever existed |

---

## 3. Tentative milestones

Each milestone ends with an acceptance gate (§5). Durations are rough, assuming
a small team (2–3 engineers) working with hardware access; they are *sequence*
promises, not calendar promises.

### M0 — Decisions and verification (before any code)

Goal: retire every open decision that would otherwise force rework.

- Confirm on the live host (`shari`, Raspberry Pi): the deployed antenna wiring
  map (beam antenna on switch port 3 vs 4 — the sources disagree; see
  `06-safety.md` §7), the deployed sequencer step lists, the deployed amplifier
  serial poll rate, and the current broker topology (192.168.1.50 today; the
  shack-local broker migration was committed but never deployed).
- Decide, and write into `02-interface-spec.md`: the `device_online` payload
  form (publish explicit `true` vs. omit-when-true — the re-implementation must
  pick one and require consumers to accept both).
- Decide the radio state heartbeat: the reference radio bridge publishes state
  only on change, which starves the P0 arm heartbeat (a live incident). The
  re-implementation **must** add a periodic republish (≤ 5 s) or equivalent —
  this is a normative change, not a copy.
- Decide the console design direction (the design exploration produced four
  candidates; one content model — see `04-console.md` §6).
- Choose the new technology stack per component class (application services,
  embedded firmware, console app) and confirm it can meet the runtime
  constraints in `03-components/common-runtime-library.md` (abortable connect,
  never-block-in-handler, retained-replay recovery).
- Inventory physical/electrical facts the repo does not contain (relay contact
  ratings, whether switch "off" is grounded by an external relay, the UHF head
  DIP settings) — see each component's "Open decisions".

**Acceptance M0:** every item in `02-interface-spec.md` §7 and `06-safety.md`
§7 is either resolved in writing or explicitly accepted as an assumption with
an owner.

### M1 — Bus foundation (the platform)

Goal: the platform everything else stands on, proven with the simplest real
component.

- Broker stood up (or the existing one reused per the M0 topology decision),
  access credentials per the config/secrets convention
  (`05-deployment-ops.md` §2).
- Common runtime library in the new stack: abortable connect, bounded job
  queue + single serialized worker, topic builders, `{"action","value"}`
  command payload helper.
- **Golden wire tests** (§5.1): a test harness that publishes/records real bus
  traffic and validates payloads against `02-interface-spec.md`.
- Power bridge (both smart plugs) — first real component; proves the whole
  plane pattern (meta/state/status/cmd, two-layer liveness, fire-and-observe
  confirmation) end to end.

**Acceptance M1:** golden tests green; power bridge shadowed against the
incumbent on the live broker with byte-comparable snapshots; a kill -9 of the
power bridge leaves the bus in a state consumers correctly interpret as
offline.

### M2 — Safe-to-transmit chain (P0 hardware layer)

Goal: the station physically cannot transmit unsafely, even with everything
else absent.

- Antenna switch firmware (exclusive selection, readback, 200 ms settle guard,
  boot-all-off + retained-command self-heal).
- Relay controller (arm formula, fail-safe-open, heartbeat window, per-slot
  connections + last-wills) — or interim: keep the incumbent embedded nodes
  running and only re-verify them against `06-safety.md`.
- Power sequencer with its full startup/shutdown step lists (config-driven).

**Acceptance M2 (safety gate — the most important one):** scripted fault
injection on the bench — kill the radio bridge, drop the PSU slot, age the
radio state past 10 s, remove the arm enable — and observe the arm relay
de-energize in every case; no port change ever occurs while RF is present;
relays always boot to the all-off state; sequencer refuses to proceed when a
wait's precondition slot is offline. Signed off against `06-safety.md` line by
line.

### M3 — HF operating core

Goal: a fully operational HF station on the new stack.

- Radio bridge (including the M0 heartbeat fix), beam controller (with the USB
  re-enumeration self-heal), amplifier bridge, tuner bridge.
- Antenna policy reconciler with arbitration ladder and idle grounding —
  with the two live fragilities **fixed as normative requirements**: heartbeat
  starvation and first-key-up-after-ground.

**Acceptance M3:** a live operating session shadowing the incumbent: band
changes route the correct antenna, the tuner engages exactly on the
non-resonant band set, 30-minute idle grounding fires, and TX through the arm
chain works. One slot at a time is cut over to the new stack here (power
first, radio last, see §5.3).

### M4 — Operator console

Goal: the operator stops needing the old console.

- MVP slice first: station power + sequencer, radio (incl. voice keyer +
  mic profile), PA + arm, antenna routing, faults bar, the LINK DOWN banner and
  the cross-panel RF safety guard (fail-closed).
- Then: tuner, beam direction, rotator compass + Mercator DX map with the
  independent spot feed (SSE, dedup, aging, reprojection), offline stamping,
  silent-slot reporting.

**Acceptance M4:** an operator runs a full contest-style session from the new
console alone; the safety guard blocks an unsafe antenna move in a scripted
test; spot overlay matches the reference feed's arithmetic on a fixed
recording.

### M5 — Rotators, discovery, UHF

- HF rotator bridge (quoted-string bearing quirk; decide whether the two legacy
  inbound control paths are kept — open decision in its spec).
- Home Assistant discovery renderer (passive; exposure-driven).
- UHF rotator with its **manual-only arming** preserved exactly (never remote,
  never persisted, motion locked unless armed).

**Acceptance M5:** gpredict/rotctl compatibility test suite passes; discovery
entities appear and clear correctly on a Home Assistant restart.

### M6 — Tooling, hardening, and the deferred layer

- Bus capture tool and bus monitor/stimulator (the monitor **must** fix the
  incumbent's committed-credential and unauthenticated-endpoint defects —
  normative).
- Deployment automation for the whole park (seed-once config, hardened units,
  per `05-deployment-ops.md`).
- Only if wanted: the logging layer (contact-log and spot events) — a
  specified-but-never-implemented feature; treat as greenfield.
- Only if wanted: PLC #2 polarization firmware (currently a documented gap).

**Acceptance M6:** a clean host can be provisioned from scratch by following
`05-deployment-ops.md` alone.

---

## 4. What is deliberately NOT being copied

Requirements derived from live incidents, where the reference implementation
is *wrong* and the PRD mandates a fix (each is normative in the linked spec):

1. **Radio state heartbeat starvation** — the arm chain drops the amplifier
   arm when the radio is idle-but-healthy because state is change-only
   (`03-components/m5stamp-relay-controller.md`, `06-safety.md` §2). The
   re-implementation adds a periodic republish.
2. **First key-up after grounding goes into the short** — the grounding
   recovery race (`03-components/antennaselect.md`).
3. **JSON-boolean command values silently disarm the arm relay** — the command
   value convention must be enforced structurally
   (`03-components/m5stamp-relay-controller.md`).
4. **Bus monitor's committed credential + unauthenticated publish endpoint**
   (`05-deployment-ops.md` §4).
5. **No drop-telemetry in the job queue** — silent job drops are invisible;
   the new runtime library must log them
   (`03-components/common-runtime-library.md`).

---

## 5. Test and cutover strategy

### 5.1 Golden wire tests (M1, continuous)

A harness that, for every slot, records a canonical traffic sample (meta
envelope, state snapshots across transitions, status flips, command round
trips) and validates any candidate implementation against
`02-interface-spec.md`: topic strings, JSON field names/types/units,
retention/QoS flags, heartbeat cadence, last-will behavior. These tests are
written *from the spec, not from the incumbent* — the incumbent's known
defects (§4) must fail them.

### 5.2 Bench hardware-in-the-loop (M2, M5)

The P0 chain is tested with real relays and dummy loads, never simulation
only: fault injection cases are enumerated in M2's acceptance gate. The UHF
head's measured protocol quirks (unusable tilt readback, ignored absolute-set
opcodes — `03-components/pelcobridge2.md`) are only verifiable on the bench.

### 5.3 Shadow-then-cutover deployment (M3 onward)

Because the bus is the only integration surface, a new component can run
*alongside* the incumbent, subscribed to the same broker but publishing under
a shadow topic root, and be diffed message-for-message. Cutover is per slot,
power chain first, the radio last (the slot with the most consumers). Each
cutover keeps the incumbent able to be switched back for one operating session.

### 5.4 Operating-session acceptance (M3, M4)

The final gate for anything operator-facing is a real session: start the
station with one button, operate on several bands across both antennas, end
with the station grounded, and review the capture log afterwards for contract
violations.

---

## 6. Risks

| Risk | Impact | Mitigation |
|---|---|---|
| Port-number wiring ambiguity unresolved | Wrong antenna routed; potential transmitter-into-open-circuit | M0 gate: read the live deployed config on the host; until then treat as unknown and block any physical antenna test |
| Re-implementation library lacks abortable connect / non-blocking handlers | Shutdown hangs under broker outage (live incident class) | M0 stack decision explicitly verifies against `common-runtime-library.md` constraints |
| Radio protocol edge cases (voice keyer commands undocumented in vendor API; two-word panadapter status topic) | Silent feature loss | These are catalogued per component; keep the vendor-undocumented features isolated behind their own tests |
| New stack team lacks ham-radio context | Misread requirements (e.g. why a dummy load exists) | Every doc defines terms at first use; `00-system-overview.md` is the mandatory first read; the station owner reviews M2's safety gate |
| Live station is the test environment | Cutover incident interrupts operation | Shadow-then-cutover (§5.3); never cut over more than one slot per session |
| Specified-but-unimplemented features drift into scope creep | Schedule | Logging layer and PLC #2 are explicitly P4 |

---

## 7. Open decisions blocking this plan

Everything in `02-interface-spec.md` §7 and `06-safety.md` §7 feeds M0. The
items with schedule impact:

1. **Switch port wiring** (beam on port 3 or 4) — blocks M2 acceptance.
2. **Broker topology** (existing 192.168.1.50 vs. planned shack-local broker) —
   blocks M1; a broker move before reconstruction starts would change every
   component's config default.
3. **Radio heartbeat mechanism** (periodic republish vs. consumer-side
   freshness) — changes the radio bridge's contract; decide before M3, but the
   fix direction (producer-side) is already normative here.
4. **Console design direction** — blocks the M4 visual pass only, not the data
   layer.
5. **Keep or drop the two legacy rotator control paths** (GS-232B TCP,
   PSTRotator UDP) — blocks only the HF rotator in M5.