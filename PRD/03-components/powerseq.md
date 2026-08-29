# 03.x — powerseq: station startup/shutdown sequencer

This document specifies `powerseq`, the station's power sequencer — a pure **logic slot** (a bus service with no physical device behind it) occupying the address `muehle/hf/power-seq`. **Amateur radio (ham radio)** is the licensed hobby of two-way radio communication; the Mühle station ("site `muehle`") is such an installation whose HF (shortwave, roughly 3–30 MHz) equipment chain is automated over an **MQTT** bus — a lightweight publish/subscribe protocol in which clients exchange messages addressed to hierarchical string *topics* via a central **broker** (server), and where a *retained* message is one the broker stores and re-delivers to every future subscriber. The station's equipment — a **transceiver** (the FLEX-8400 radio that generates and receives the signal), a **power amplifier** or **PA** (an ACOM 1200S that boosts the transmit signal, up to 1200 W), the antennas and their switching/tuning controllers — is powered through two switched smart-plug outlets ("Shelly" devices, each itself an MQTT-connected slot): the *station master mains* (gates everything) and the *13.8 V PSU* (the DC supply feeding the control electronics of the radio gear). powerseq's single job: on a **one-button operator command** (`start` or `stop`) it brings the whole station up or down **in a fixed, safe order**, commanding the individual device slots, pausing for settle delays, and — critically — **waiting for explicit liveness confirmation from each dependent device before proceeding**. This matters because powering a transmit chain in the wrong order damages hardware (e.g. energizing a PA before its control logic is up), because removing power from inductive loads all at once causes damaging electrical **inrush current**, and because devices take tens of seconds to boot once their mains is switched on.

Terminology used throughout this document:

- **Slot**: the station's addressing unit — a topic prefix `<site>/<station>/<slot>` (e.g. `muehle/hf/power-seq`) with four topic planes: `/meta` (self description), `/state` (retained JSON snapshot of current condition), `/status` (liveness: the literal string `online` or `offline`), and `/cmd` (JSON command input). See `02-interface-spec.md` §2–§4.
- **LWT (Last Will and Testament)**: a message the broker publishes on a client's behalf if that client disappears without closing its connection (crash, network loss). It is *not* published on a clean disconnect — so a retained `/status: online` can outlive a cleanly-stopped service.
- **QoS**: MQTT delivery guarantee — QoS 0 = fire-and-forget (at most once), QoS 1 = at least once.
- **Retained message**: one the broker keeps and delivers to every new subscriber of the topic, until replaced. The station uses retained steady-state *intent* so devices can re-apply their last commanded setting after a reboot (**self-heal**).
- **Two-layer liveness**: the station's convention that a device is only considered truly alive when BOTH its bridge process reports `/status: online` AND (where applicable) the device behind the bridge is confirmed inside `/state` (e.g. a `device_online` field). See `01-architecture.md` and §6.2 below.
- **Inrush current**: the brief surge drawn when an inductive load (power supply, relay-fed circuit) is first energized; simultaneous switching of several such loads can trip breakers or stress components, hence the deliberate 2-second staggers.
- **Logic slot**: a slot implemented entirely in software on the station's Raspberry Pi host ("shari", `192.168.1.139`), with no physical device to talk to; its only I/O is MQTT.

---

## 1. Role and responsibilities

powerseq is a Linux daemon (a system service) running on shari, connecting only to the MQTT broker at `tcp://192.168.1.50:1883` (user `hf`; password from a protected environment file, never in config or command line). Note: a planned migration of the broker onto shari itself (`192.168.1.139`) exists on an unmerged feature branch and is NOT deployed — `192.168.1.50` is current production; see `05-deployment-ops.md` for the decision point.

The sequencer SHALL:

1. **One-button sequencing** — accept exactly one input, a `start` or `stop` JSON command on its own `/cmd` topic, and execute the configured startup or shutdown step list.
2. **Ordered, confirmed power-up** — bring the station up in the order: master mains → network settle → 13.8 V PSU → wait for controllers online → transceiver power → wait for radio online → PA enable → wait for PA powered → PA arm.
3. **Ordered, staggered power-down** — bring the station down in the exact reverse order, with 2-second delays between each power removal to stagger inrush current.
4. **Liveness-gated progress** — never proceed past a device-dependent step until that device has explicitly confirmed itself alive on the bus (§6.2).
5. **State publication** — publish its own progress as a retained `/state` snapshot (`phase`, `step`, `fault`) so any console or operator can see where a sequence stands.
6. **No lock, no claim** — act as *one writer* of the slots it drives but never *lock* or claim them; while idle, every controlled slot remains directly commandable for troubleshooting.
7. **Fail without damage** — on any step failure, stop immediately, publish a precise fault, and leave already-driven slots at their last commanded intent (no automatic rollback — §7).

The sequences are **config-driven, not hard-coded**: the complete startup and shutdown step lists live in a TOML configuration file; the code implements only four generic step kinds (`cmd`, `delay`, `wait_status`, `wait_state`) and the runtime around them (§5).

---

## 2. MQTT presence

All own topics live under the self base `muehle/hf/power-seq` (defaults; configurable via `mqtt.site`/`mqtt.station`/`mqtt.slot`).

### 2.1 Published topics

| Topic | Retained | QoS | Payload | When |
|---|---|---|---|---|
| `muehle/hf/power-seq/meta` | yes | 1 | JSON birth certificate (§2.2) | on every (re)connect |
| `muehle/hf/power-seq/state` | yes | 1 | JSON `{ "ts", "phase", "step", "fault"? }` (§2.3) | on process start, on every phase/step/fault change, and on every (re)connect |
| `muehle/hf/power-seq/status` | yes | 1 | literal string `online` or `offline` (not JSON) | `online` on every (re)connect; `offline` published explicitly on clean shutdown; `offline` via LWT on ungraceful death |

### 2.2 `/meta` payload

All values are derived from configuration; `controls`/`watches` are derived from the configured sequences. The sequencer SHALL publish exactly this shape:

```json
{
  "schema": "1.0",
  "role": "sequencer",
  "link": "none",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "controls": ["muehle/hf/pa-arm", "muehle/hf/switch",
                  "muehle/power/master", "muehle/power/psu-13v8"],
    "watches":  ["muehle/hf/ant-switch", "muehle/hf/pa", "muehle/hf/pa-arm",
                  "muehle/hf/radio", "muehle/hf/switch",
                  "muehle/power/master", "muehle/power/psu-13v8"]
  },
  "expose": {
    "device": { "name": "Station power sequencer" },
    "fields": [
      { "key": "phase", "name": "Phase", "type": "enum",
        "options": ["idle", "starting", "running", "stopping"] },
      { "key": "step", "name": "Step", "type": "string" },
      { "key": "fault", "name": "Fault", "type": "string" }
    ]
  }
}
```

Requirements:

- `controls` SHALL be the absolute addresses of every slot that any `cmd` step of either sequence targets, deduplicated and sorted alphabetically.
- `watches` SHALL be every slot referenced by any step of either sequence (`cmd` + `wait_status` + `wait_state` targets), deduplicated and sorted alphabetically.
- `expose` SHALL be fixed regardless of the configured sequences: exactly three read-only fields (`phase` enum with the four options in the order shown, `step` string, `fault` string). The operator command surface is the one-shot `/cmd`, which is not retained and is therefore not exposed as a writable field.
- A separate consumer (hadiscovery, see `03-components/` index in `README.md`) renders these fields into Home Assistant (a home-automation platform); the sequencer itself SHALL NOT publish any home-automation discovery topics. (The config carries a `discovery_prefix` key; it is unused by this component.)

### 2.3 `/state` payload

The sequencer SHALL publish this snapshot on every internal transition:

```json
{ "ts": "2026-07-14T18:42:01Z", "phase": "starting", "step": "psu-on" }
{ "ts": "...", "phase": "running", "step": "" }
{ "ts": "...", "phase": "idle", "step": "", "fault": "psu-on: timeout" }
```

| Field | Type | Semantics |
|---|---|---|
| `ts` | string, RFC 3339, UTC | snapshot timestamp |
| `phase` | string enum | one of `idle`, `starting`, `running`, `stopping` |
| `step` | string | name of the current step while a sequence runs. **Always present, never omitted**; empty string `""` at `idle`/`running` and after sequence completion or fault |
| `fault` | string | `"<step>: <reason>"` for the step that aborted the last sequence. **Omitted when empty.** Cleared by the next honored `start`/`stop` and by a completed shutdown |

Additional requirements:

- The sequencer SHALL publish an initial retained `idle` `/state` at process start (overwriting any stale retained `/state` left by a crashed predecessor). The sequencer never reads its own `/state` back.
- The sequencer SHALL re-publish the retained `/state` on every broker (re)connect, so a broker that lost retained messages gets a fresh copy.

### 2.4 Subscribed topics (derived from the configured sequences — never hard-coded)

The sequencer SHALL subscribe, at QoS 1, to:

- the `/status` topic of **every** slot referenced by any `cmd`, `wait_status`, or `wait_state` step of either sequence;
- the `/state` topic of every `wait_state` target;
- its own `/cmd` (at QoS 0 — §4).

For the default sequences this is exactly:

```
muehle/hf/ant-switch/status    (liveness gate for wait-controllers-online)
muehle/hf/pa-arm/status        (liveness gate)
muehle/hf/pa/state             (wait-pa-power-on: field "power" == "on")
muehle/hf/pa/status            (implicit liveness precondition for wait_state)
muehle/hf/power-seq/cmd        (operator one-button; QoS 0)
muehle/hf/radio/status         (wait-radio-online gate)
muehle/hf/switch/status        (liveness gate)
muehle/power/master/status     (observability of a controlled slot)
muehle/power/psu-13v8/status   (observability of a controlled slot)
```

- `/status` messages: any payload whose trimmed, case-insensitive value equals `online` records the slot as online; anything else records `offline`. **Absence is distinct from offline**: a slot that has never published anything is "unseen", and a `wait_status` with `state="offline"` can never pass on an unseen slot (§5.2).
- `/state` messages: the whole JSON object is parsed and cached per slot. A **malformed** `/state` payload SHALL **delete** the slot's previous cached snapshot (a good→bad transition must not leave a stale value poisoning the cache) and be logged at warn level.
- All subscriptions SHALL be re-issued on every (re)connect (unconditionally, even with a persistent session).
- Incoming-message handlers SHALL never publish and never block: they only update mutex-guarded maps and, for `/cmd`, perform a non-blocking channel send. (This is a library-independent constraint — in the reference MQTT library, handlers run on the connection's dispatch thread and a handler that blocks or publishes synchronously deadlocks the client; this happened live elsewhere in this station. The re-implementation MUST isolate handler work from the receive path — a queue drained by a single worker — regardless of library.)

### 2.5 Commands issued to other slots

Each `cmd` step publishes to `<site>/<slot>/cmd`, QoS 1, **retained by default**:

```json
{ "action": "<action>", "value": "<value>" }
```

- `value` is **always a JSON string** — the station's value-key convention: command arguments always ride under the key `"value"`, and booleans are carried as the strings `"true"`/`"false"`. See `02-interface-spec.md`.
- Retention default is `true`: the controlled slots keep the retained intent and re-apply it on their own reconnects ("self-healing"). A step MAY set `retain = false` for a one-shot (non-steady-state) command.
- Every publish SHALL be bounded by a **10-second** wait timeout: a publish attempt that cannot complete within 10 s SHALL return an error instead of stalling the sequencer. A cmd-step publish error faults the sequence (§7); a `/state` publish error is only logged, never fatal.

---

## 3. Broker connection

- Transport: MQTT 3.1.1 over plain TCP, default `tcp://192.168.1.50:1883`, username `hf`, password from the environment (never in the config file or command line).
- Client ID: defaults to `<site>-<station>-<slot>` with `-` separators → `muehle-hf-power-seq`.
- Persistent session (`CleanSession=false` in MQTT 3.1.1 terms) with automatic reconnect.
- **LWT**: topic `<self>/status`, payload `offline`, QoS 1, retained — so an ungraceful death (crash, network loss) is visible on the bus. On a **clean** shutdown the sequencer SHALL additionally publish `offline` explicitly before disconnecting, with a 250 ms grace before the actual disconnect. (Consumers still must not trust `/status` alone — see `01-architecture.md` on two-layer liveness.)
- The initial connect attempt SHALL be interruptible: process shutdown (SIGINT/SIGTERM) while the broker is unreachable SHALL resolve promptly (within seconds), not hang until the service manager force-kills the process.
- On (re)connect: publish `/status: online` (retained), publish `/meta` (retained), mark the broker as online, re-issue all subscriptions, and request a re-publish of the retained `/state`.
- On connection loss: mark the broker offline. A running sequence SHALL fault at its next `cmd` step or wait poll with reason `broker disconnected` (§7); `delay` steps are purely local and unaffected.

---

## 4. Command surface — the one-button model

powerseq accepts exactly one input: a JSON message on `muehle/hf/power-seq/cmd`:

```json
{ "action": "start" }
{ "action": "stop" }
```

- Only the `action` key is examined; extra keys are ignored. A malformed JSON body is logged at warn and dropped. Any `action` other than `start` or `stop` is logged at warn and ignored.
- **The sender MUST NOT publish this command retained**, and the sequencer SHALL subscribe to its own `/cmd` at **QoS 0 deliberately**. Rationale (normative, a safety property): with a persistent MQTT session, a QoS-1 subscription would let the broker queue a command issued while the sequencer is offline and replay it on reconnect — re-energizing the station, exactly the hazard this rule exists to prevent. QoS 0 means a command issued while the sequencer is down is simply lost, which is correct for a one-shot operator command.
- There SHALL be no other input surface: no other RPC, no command-line commands beyond config/log flags.

### 4.1 Busy guard — truth table

Commands are honored only when the phase permits; otherwise they are dropped with a warn-level log:

| Current `phase` | `fault` set? | `start` | `stop` |
|---|---|---|---|
| `idle` | no | **honored** (→ `starting`) | **dropped** — see known defect §9 |
| `idle` | yes | **honored** (fault cleared; → `starting`; idempotent re-drive over the partially-hot station) | **honored** (→ `stopping`; teardown of a half-started station) |
| `starting` | — | dropped | **dropped** — there is **no abort** (see §4.2) |
| `running` | — | dropped | **honored** (→ `stopping`) |
| `stopping` | — | dropped | dropped |

- **`start` from `idle` with a fault** is safe and deliberate: every step is idempotent steady-state intent, so re-running startup over an already-hot station converges almost immediately (a wait whose condition is already satisfied on entry passes on its first poll).
- **`stop` from `idle` with a fault** exists to resume an interrupted shutdown: a startup that faulted half-way left slots energized; `stop` from that state runs the full shutdown list as a teardown. Because the default shutdown list contains no waits, it always completes. (The component's own API doc lagged the code here, saying `stop` is honored only from `running`; the behavior in this table is authoritative — see §11.)
- **Guard enforcement, two-phase**: the guard SHALL be checked (a) at command reception as a fast-path drop, and (b) re-checked authoritatively and atomically when the runner picks the command up — the phase transition (`idle→starting` / `running→stopping`) happens under a lock, closing the race where two rapid commands both pass the reception check.
- The command queue between reception and the runner has capacity 4; a command arriving when the queue is full is dropped with a warn log. Exactly one sequence runs at a time; a second command during a run is dropped, never queued to run afterward.

### 4.2 No abort — current contract

There is **no abort command**. A `stop` issued during `starting`, or a `start` during `stopping`, is silently dropped (logged at warn). An operator who wants to cancel must wait out the current run — up to roughly 30 s + 3 × 120 s worst case with default timeouts — or fault it indirectly. This is documented *intent* of the deployed system, not an oversight; whether an abort/cancel capability should be added is an open decision (§11), and changing it is a deliberate contract change, not a bug fix.

---

## 5. Step kinds — the only behavior primitives

Each step has a `name` (required, unique within its list — step names are user-visible in `state.step` and fault strings) and a `kind`, one of exactly four:

### 5.1 `cmd`

Publish `{"action":…, "value":"<string>"}` to `<site>/<slot>/cmd`, retained unless the step sets `retain = false`. The broker connection SHALL be checked immediately before the publish; a broker-down or failed publish (including the 10-second bound) faults the sequence.

### 5.2 `wait_status`

Wait until **every** slot in `slots` (logical AND) has `/status` equal to `state` (default `online`). Requirements:

- Each entry in `slots` must be non-empty (an empty `""` entry is a load-time config error — it would make the AND unpassable).
- `state = "online"` passes when every listed slot's most recent `/status` payload is `online`.
- `state = "offline"` is also allowed, and requires an **actual `offline` payload** — never mere absence. A slot that has never published cannot satisfy an `offline` wait.

### 5.3 `wait_state`

Wait until the top-level JSON field `field` of `<site>/<slot>/state` compares equal, as a string, to `value`. Requirements:

- **Mandatory liveness precondition**: the target slot's `/status` MUST be *currently* `online` for the wait to pass, at the moment of passing. A dead device whose LWT fired cannot satisfy the wait on a stale retained `/state`. This is the station's two-layer-liveness convention applied at the gate where retained snapshots are trusted — see §6.2 for the layer the wait does *not* check.
- `value` may be the empty string `""`: the wait then passes when the field is absent, `null`, or `""` (i.e. waiting for a field to clear).
- Observed JSON values are coerced to strings for comparison: JSON booleans → `"true"`/`"false"`, JSON numbers → shortest decimal representation, strings → as-is, `null`/absent → `""`.
- A `wait_state` targeting the sequencer's own slot is a load-time config error (its `/state` is an output, not an input).

### 5.4 `delay`

Sleep a fixed duration: either a literal integer `duration_s` (> 0, else load-time error) or a symbolic `duration` = `"network"` (resolves to `timing.network_delay_s`, default 30 s) or `"stagger"` (resolves to `timing.shutdown_stagger_s`, default 2 s). Exactly one of the two forms must be given; unknown symbolic values are load-time errors. Delay steps are local and NOT gated by the broker connection — they continue during an outage; only process shutdown interrupts them.

### 5.5 Wait mechanics — poll, hold, deadline (exact)

- A wait step SHALL poll its condition every `poll_interval_ms` (default **200 ms**).
- Each poll SHALL check, in this order: (1) process shutdown requested → fault `interrupted`; (2) broker offline → fault `broker disconnected`; (3) wall-clock past the deadline → fault `timeout`; (4) the condition itself.
- **Hold (debounce) window**: an optional `hold_ms` requires the condition to hold *continuously* for that long before the wait passes; if the condition breaks mid-hold, the window restarts. Omitted `hold_ms` → `timing.default_hold_ms` (default 0). An **explicit `hold_ms = 0` means edge-triggered** (pass as soon as true) even when a nonzero default hold is configured.
- **Deadline**: per-step `timeout_s` if given (must be > 0), else `timing.step_timeout_s` (default **120 s**).
- Because observations are cached retained snapshots, a wait whose condition is already satisfied on entry passes on its first poll (fast path).
- A polling implementation is not mandatory — an event-driven condition approach is acceptable as long as the observable semantics are equivalent (poll cadence is a ceiling in spirit; faster reaction changes no published behavior).

---

## 6. The default sequences

The sequences below ship as `config.example.toml` and implement the station integration model §7.1. They are defaults, not code: the step lists live in TOML and are fully editable (§8). The step names appear verbatim in `state.step` and fault strings and are user-visible.

### 6.1 Startup sequence (on honored `start`)

| # | Step name | Kind | Detail | Purpose / confirmation |
|---|---|---|---|---|
| 1 | `master-on` | cmd | `muehle/power/master/cmd` ← `{"action":"set_power","value":"on"}` (retained) | Energize the station master mains smart plug |
| 2 | `network-delay` | delay | `duration="network"` → 30 s default | Let the network (broker, the plug-in devices' WiFi) come up |
| 3 | `psu-on` | cmd | `muehle/power/psu-13v8/cmd` ← `{"action":"set_power","value":"on"}` (retained) | Energize the 13.8 V DC power supply |
| 4 | `wait-controllers-online` | wait_status | all of `muehle/hf/switch`, `muehle/hf/pa-arm`, `muehle/hf/ant-switch` `/status` == `online`; deadline 120 s default | Confirm the devices booting off the 13.8 V supply are actually up |
| 5 | `trx-on` | cmd | `muehle/hf/switch/cmd` ← `{"action":"set_trx","value":"on"}` (retained) | Power the transceiver via its remote-on relay |
| 6 | `wait-radio-online` | wait_status | `muehle/hf/radio/status` == `online`; deadline 120 s default | Confirm the radio's bridge is up after power-on |
| 7 | `pa-on` | cmd | `muehle/hf/switch/cmd` ← `{"action":"set_pa","value":"on"}` (retained) | Engage the PA-enable relay |
| 8 | `wait-pa-power-on` | wait_state | `muehle/hf/pa/state` field `power` == `"on"` AND `muehle/hf/pa/status` == `online`; deadline 120 s default | Confirm the PA actually reports powered; the liveness precondition prevents a dead PA from passing on a stale retained state |
| 9 | `pa-arm-enable` | cmd | `muehle/hf/pa-arm/cmd` ← `{"action":"set_enabled","value":"true"}` (retained) | Arm the PA (permit RF output). Last step → `phase=running` |

Total worst-case time to `running` with all waits timing out at defaults is ≈ 30 s + 3 × 120 s; a typical hot-idempotent re-run converges in under 1 second.

### 6.2 Two-layer liveness as applied here — and its known gap

The station's two-layer liveness convention says a consumer must require BOTH the bridge `/status` (process alive) AND, where the slot carries it, a device-link indication inside `/state` (e.g. `device_online`). The `wait_state` step kind applies the first layer as a **mandatory precondition** (§5.3) before trusting the retained `/state` snapshot. However, the deployed wait does **not** check the second layer inside the waited `/state` payload: the `power == "on"` snapshot itself can be arbitrarily old, so a device whose *link* died without its bridge's `/status` changing would still satisfy `wait-pa-power-on` on a stale snapshot. This is a known defect, not a contract — see §9 and §11.

### 6.3 Shutdown sequence (on honored `stop`)

Exact reverse of startup, with 2-second staggers between power removals for inrush control:

| # | Step name | Kind | Detail |
|---|---|---|---|
| 1 | `pa-arm-disable` | cmd | `muehle/hf/pa-arm/cmd` ← `{"action":"set_enabled","value":"false"}` (retained) |
| 2 | `stagger-1` | delay | `duration="stagger"` → 2 s default |
| 3 | `pa-off` | cmd | `muehle/hf/switch/cmd` ← `{"action":"set_pa","value":"off"}` (retained) |
| 4 | `stagger-2` | delay | 2 s |
| 5 | `trx-off` | cmd | `muehle/hf/switch/cmd` ← `{"action":"set_trx","value":"off"}` (retained) |
| 6 | `stagger-3` | delay | 2 s |
| 7 | `psu-off` | cmd | `muehle/power/psu-13v8/cmd` ← `{"action":"set_power","value":"off"}` (retained) |
| 8 | `stagger-4` | delay | 2 s |
| 9 | `master-off` | cmd | `muehle/power/master/cmd` ← `{"action":"set_power","value":"off"}` (retained) |

There are deliberately **no waits in the shutdown**: shutdown must make progress even if devices are already dead. Completing step 9 → `phase=idle`, fault cleared.

---

## 7. State machine, faults, and the no-rollback policy

```
        start (phase=idle)              stop (phase=running,
      ┌──────────────────┐             or idle+fault)
      │                  ▼             ┌──────────────►
   ┌───────┐  start  ┌──────────┐  ok  ┌─────────┐  stop   ┌──────────┐  ok  ┌──────┐
   │ idle  ├────────►│ starting ├───► │ running ├─────────►│ stopping ├───► │ idle │
   └───┬───┘         └────┬─────┘     └─────────┘          └────┬─────┘      └──────┘
       │                  │ any step fails                       │
       └──────────────────┴────────────► phase=idle + fault ◄────┘
```

- Transitions: `idle → starting` on an honored `start`; `starting → running` when the last startup step completes; `running → stopping` on an honored `stop`; `stopping → idle` when the last shutdown step completes.
- On sequence entry: `step` reset to `""`, `fault` cleared, new phase published. On entering each step: `step` set to that step's name, `fault` cleared, `/state` published. On completion: `step=""`, end phase, `/state` published.
- **Any step failure** (wait timeout, broker disconnect mid-sequence, publish failure, process-shutdown interruption) SHALL immediately set `phase=idle` with `fault = "<step-name>: <reason>"` and `step=""`.
- Exact fault reason strings (concatenated as `"<step>: <reason>"`):
  - `timeout` — a wait step exceeded its deadline.
  - `broker disconnected` — the MQTT connection dropped before a cmd step's publish or during a wait's poll loop.
  - `publish failed: <error>` — the cmd publish itself failed (including the 10-second publish wait timeout).
  - `interrupted` — process shutdown during a delay or wait step, or at a step boundary.
- **No rollback**: a failed sequence leaves every already-driven slot at whatever its last retained `/cmd` set it to. Recovery is explicit: re-run `start` (fault cleared; idempotent re-drive) or `stop` from `idle+fault` (teardown).
- Exactly one sequence runs at a time; a single runner also handles a coalesced "republish retained `/state`" request from the connection layer on reconnect.

### 7.1 Boot and reconnect behavior — no spurious energization

- On process start: load and validate config (fatal exit 2 on error), connect to the broker (fatal exit 1 on failure), then publish the initial retained `idle` `/state` (overwriting any stale retained `/state` from a crashed predecessor).
- **The sequencer SHALL NOT emit any command to any slot at boot or on (re)connect.** No sequence runs until an explicit operator `start` arrives — even if the whole station is already hot. Combined with the own-`/cmd` QoS-0 / non-retained policy (§4), this guarantees a service restart can never replay an operator command and re-energize the station.
- On (re)connect: publish `/status: online` (retained — this overwrites the LWT `offline`), publish `/meta` (retained), re-issue all subscriptions, re-publish the retained `/state`.

---

## 8. Configuration

TOML file, default path `/etc/powerseq/config.toml`, overridable with the `-config` flag; `-log.level` (`debug|info|warn|error`) overrides the config's `[log].level`. A missing file at the *default* path is not itself an error (built-in transport/timing defaults apply) — but with no `[[startup]]`/`[[shutdown]]` step lists, subsequent validation **fails** (exit 2): there is no built-in default sequence; it must be configured. A missing or malformed file at an explicitly-given `-config` path is fatal.

### 8.1 Keys and defaults

| Key | Default | Meaning |
|---|---|---|
| `host` | `shari` | compute-node label published in `/meta` |
| `location` | `bauwagen` | physical label published in `/meta` |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | broker URI |
| `mqtt.client_id` | `""` → `muehle-hf-power-seq` | MQTT client ID |
| `mqtt.site` / `mqtt.station` / `mqtt.slot` | `muehle` / `hf` / `power-seq` | the sequencer's own slot address; also the prefix resolving site-relative step slots to absolute topics |
| `mqtt.user` | `hf` | broker username |
| `mqtt.password` | `""` | **not used from TOML in production** — supplied via env (§8.3) |
| `mqtt.discovery_prefix` | `homeassistant` | carried but unused by this component |
| `timing.network_delay_s` | `30` | seconds; value of `delay duration="network"` |
| `timing.step_timeout_s` | `120` | seconds; default wait deadline |
| `timing.shutdown_stagger_s` | `2` | seconds; value of `delay duration="stagger"` |
| `timing.poll_interval_ms` | `200` | ms; wait-poll cadence |
| `timing.default_hold_ms` | `0` | ms; default hold for waits omitting `hold_ms` |
| `log.level` | `info` | verbosity |
| `[[startup]]` / `[[shutdown]]` | none (**required**) | ordered step lists (§8.2); at least one step each |

Negative or zero timing values fall back to their defaults at load.

### 8.2 Step-list schema (full)

Each `[[startup]]` / `[[shutdown]]` array entry is ONE step, executed in array order. A step has `name` (string, required, unique within its array) + `kind` + kind-specific keys:

```toml
[[startup]]
name = "master-on"          # REQUIRED, unique within this list
kind = "cmd"                 # discriminator: one of the four kinds

# kind = "cmd" — emit a /cmd to a site-relative slot.
slot   = "power/master"      # site-relative -> <site>/<slot>/cmd
action = "set_power"          # /cmd JSON "action"
value  = "on"                 # /cmd JSON "value" — ALWAYS a string (value-key
                              # convention; booleans as "true"/"false")
retain = true                 # optional, default true

# kind = "wait_status" — wait until every listed slot's /status == state.
slots     = ["hf/switch", "hf/pa-arm"]  # site-relative; logical AND; every
                                        # entry must be non-empty
state     = "online"          # optional, default "online"; "offline" requires
                              # an actual offline payload, never absence
hold_ms   = 500               # optional: omitted -> default_hold_ms; EXPLICIT 0
                              # = edge-triggered even when a default hold is set
timeout_s = 60                # optional: per-step override of step_timeout_s;
                              # must be > 0

# kind = "wait_state" — wait until a slot's /state top-level field == value.
slot      = "hf/pa"           # site-relative; the sequencer's OWN slot is a
                              # load-time error
field     = "power"           # top-level JSON key in the /state snapshot
value     = "on"              # string; observed JSON coerced to string; may be ""
                              # (passes when field is absent/nil/empty)
hold_ms   = 500               # optional (as above)
timeout_s = 60                # optional, must be > 0
# IMPLICIT liveness precondition: the slot's /status MUST also be online.
# A malformed /state drops the slot's prior cached snapshot.

# kind = "delay" — sleep a fixed duration.
duration_s = 30               # literal seconds (integer, > 0)
# OR
duration  = "network"         # symbolic ref into [timing]: "network" | "stagger"
# exactly one of duration_s / duration; unknown symbolic value -> load error;
# a non-positive duration_s -> load error (no silent no-op)
```

### 8.3 Load-time validation (all fatal, exit 2 — fail-fast, never a runtime surprise)

- `site`/`station`/`slot`/`broker` non-empty.
- Each step list non-empty; step names present and unique within their list.
- `cmd`: `slot`, `action`, `value` all non-empty.
- `wait_status`: `slots` present and non-empty as a list, every entry non-empty, `state` ∈ {`online`,`offline`} if given, `timeout_s > 0` if given.
- `wait_state`: `slot` and `field` present (`value` may be `""`), `timeout_s > 0` if given; a `wait_state` on the sequencer's own `<site>/<station>/<slot>` is rejected.
- `delay`: exactly one of `duration_s` (> 0) or `duration` (`network` | `stagger`); unknown symbolic values rejected.
- Unknown `kind` rejected.

### 8.4 Environment overrides and secrets

After TOML load, these overlay `[mqtt]`:

```
POWERSEQ_MQTT_BROKER      POWERSEQ_MQTT_CLIENT_ID
POWERSEQ_MQTT_USER        POWERSEQ_MQTT_PASSWORD   POWERSEQ_MQTT_SITE
```

The password SHALL be kept in a 0600 environment file owned by the service user (`/etc/powerseq/powerseq.env`), never in the TOML, the unit file, or the process command line. See `../docs/conventions/config-and-secrets.md` conventions in `05-deployment-ops.md`.

---

## 9. Deployment contract

- Target: Raspberry Pi "shari" (`192.168.1.139`), SSH user `io`.
- The deploy SHALL cross-compile a static binary, ship it plus a generated service unit and **seed-once** config/env files, and install: binary to `/opt/powerseq/powerseq` (0755), unit to `/etc/systemd/system/powerseq.service`, and `/etc/powerseq/config.toml` + `/etc/powerseq/powerseq.env` (0600, owned by the `powerseq` user) **only if they do not already exist** — existing files are never overwritten (the device owns its own settings; see `05-deployment-ops.md`).
- Service unit: simple type, `After=`/`Wants=network-online.target`, `ExecStart=/opt/powerseq/powerseq -config /etc/powerseq/config.toml`, `EnvironmentFile=/etc/powerseq/powerseq.env`, `Restart=on-failure`, `RestartSec=5`, dedicated system user (no home, nologin).
- Hardening requirements for the unit: `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`, kernel/namespace/restriction guards, `RestrictAddressFamilies=AF_INET AF_INET6`, empty capability bounding/ambient sets, `MemoryMax=256M`, `TasksMax=64`, a writable state directory only, output to the journal with identifier `powerseq`.
- The sequencer's only runtime dependencies are the MQTT broker (with the `hf` account) and the slots it drives existing on the bus.

### 9.1 Reference-implementation notes (non-normative)

The deployed implementation is a Go program using the `eclipse/paho.mqtt.golang` client and `BurntSushi/toml`, sharing station plumbing (context-aware connect helper, slot-address helpers) with the other station services via a shared module. The deploy script cross-compiles with `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 -trimpath -ldflags="-s -w"`. Deploy-script environment knobs (defaults): `SSH_HOST=192.168.1.139`, `SSH_USER=io`, `SERVICE_NAME=powerseq`, `SERVICE_USER=powerseq`, `INSTALL_DIR=/opt/powerseq`, `CONFIG_DIR=/etc/powerseq`, `HOST_NAME=shari`, `LOCATION=bauwagen`, `LOG_LEVEL=info`, `MQTT_BROKER=tcp://192.168.1.50:1883`, `MQTT_SITE=muehle`, `MQTT_STATION=hf`, `MQTT_SLOT=power-seq`, `MQTT_USER=hf`, `MQTT_PASSWORD=` (empty → a commented placeholder is seeded and the deploy prints a warning), `NETWORK_DELAY_S=30`, `STEP_TIMEOUT_S=120`, `SHUTDOWN_STAGGER_S=2`. None of these technology choices are requirements — only the behavior contracts of this document are.

---

## 10. Invariants (normative summary)

1. **Ordering** — Startup order is exactly: master mains → network delay → PSU → controller-liveness wait → TRX power → radio-online wait → PA enable → PA-powered wait → PA arm. Shutdown is the exact reverse with 2-second staggers between power removals. PA arming is *always* the last startup action; PA disarm *always* the first shutdown action.
2. **No liveness, no progress** — a `wait_state` never passes unless the target's `/status` is *currently* `online`; a `wait_status` on `offline` requires an actual `offline` payload, never absence.
3. **No spurious energization** — own `/cmd` is never retained and subscribed at QoS 0 only; on boot and on reconnect the sequencer emits no commands until an explicit `start` arrives; on process start it republishes only its own `/state`, `/meta` and `/status=online`.
4. **No rollback on fault** — a failed sequence leaves driven slots at their last retained intent; recovery is an explicit re-run (`start`) or teardown (`stop` from `idle+fault`).
5. **One writer, no locking** — the sequencer never publishes anything that claims the controlled slots; while idle, any slot can be commanded directly. Retained steady-state `/cmd` intent lets each device self-heal to the last intent on its own reconnect.
6. **Broker outage cannot stall or corrupt** — cmd and wait steps fault promptly (never wait indefinitely on stale data); all publishes are bounded by 10 s; delay steps are purely local.
7. **Single-sequence execution** — exactly one sequence at a time; a second command during a run is dropped, never queued to run after; the phase re-check at execution is atomic.
8. **Config fail-fast** — any malformed, incomplete, or self-referential step list is a load-time fatal, never a runtime surprise.
9. **Value-key convention** — every emitted `/cmd` argument is a JSON **string** under `"value"` (booleans as `"true"`/`"false"`), matching what the target bridges parse.
10. **Handler isolation** — incoming-message handling never publishes or blocks (queue + single worker), regardless of MQTT library.

---

## 11. Known defects, fragilities, and open decisions

### Known defects of the deployed system (a re-implementation MUST resolve or explicitly decide each)

1. **deploy seed config omits the step lists** — the config file that the deploy script seeds on a fresh device contains `host`, `location`, `[mqtt]`, `[timing]`, `[log]` but **no `[[startup]]`/`[[shutdown]]` step lists**, which validation requires. A first-deploy service therefore crashes at startup with "at least one `[[startup]]` step is required" and, with `Restart=on-failure`/`RestartSec=5`, crash-loops until an operator hand-adds the step lists from the example config. The example file has them; the seed generator does not copy them. **A re-implementation MUST ship a full seed config (or a built-in default sequence) so a fresh deploy boots green.** Open decision: which mechanism — seed the full lists, or fall back to a built-in default when absent.
2. **No abort of an in-progress sequence** — `stop` during `starting` is silently dropped (§4.2). Documented intent; changing it is a deliberate contract change. Open decision for the re-implementation team.
3. **`wait_state` trusts an arbitrarily old `/state` snapshot** — the liveness precondition checks current `/status`, but the second liveness layer (a device-link indication inside the waited `/state` payload, e.g. `device_online`) is not checked; a device whose link died without a `/status` change could satisfy `wait-pa-power-on` on a stale `state.power == "on"` (§6.2). Open decision: add a freshness bound or device-online coupling for `wait_state`.
4. **`stop` from `idle` without a fault is dropped** — even if the station is fully hot because it was started by hand or by a previous process lifetime, there is no way to shut it down through the sequencer. Open decision: allow unconditional teardown.
5. **Legitimate operator double-presses log at warn** — the fast-path guard logs a normal duplicate `start` at warn level, producing noise.
6. **Fire-and-forget publishes on connect** — `/status` and `/meta` publishes on (re)connect are unlogged and unbounded-retry-free; a failure there is invisible. Only the `/state` republish goes through the bounded publisher.
7. **Stale subscriptions accumulate** — with a persistent MQTT session, changing the config (and thus the derived subscription set) re-subscribes on top of the old session; subscriptions for slots no longer referenced are never unsubscribed, so the broker keeps delivering (and the sequencer discarding) their messages until the session is reset.
8. **No self-watchdog** — there is no metric or heartbeat beyond `/status`; a wedged-but-connected process (e.g. a deadlocked runner) would not be detected on the bus.
9. **Doc lag** — the component's API doc states `stop` is honored only from `phase=running`; the code (and tests) also honor `stop` from `idle+fault`. This document follows the code (§4.1). The README's "~30 s" style figures are config defaults, fully changeable in TOML.
10. **Related live fragility (cross-component, hard requirement)** — the PA arm relay de-energizes if its enabling inputs (the radio's `/state`) are not refreshed within 10 s, and the radio bridge publishes `/state` only on change — starving that heartbeat when the radio is idle-but-healthy. Any re-implementation of the radio bridge MUST add a periodic heartbeat republish (e.g. every ≤5 s) or equivalent; see `03-components/flexbridge.md` and `06-safety.md`. powerseq's `wait-pa-power-on` gate does not protect against this once startup has completed.

### Open decisions & unresolved facts

- **Abort/cancel support** (defect 2): keep the drop-on-busy contract, or add an explicit abort command that safely runs the shutdown list from wherever startup stands? The current contract is specified in §4.1; any change is deliberate, not a fix.
- **Seed mechanism** (defect 1): full step lists in the seed config vs. built-in defaults — both resolve the crash-loop; the deployed system has neither.
- **Two-layer liveness in `wait_state`** (defect 3): whether to require a device-link indication inside the waited `/state` in addition to bridge `/status` — and, relatedly, the station-wide open question of whether `device_online`-style fields may be omitted-when-true or must always be explicit (see `02-interface-spec.md`).
- **Unconditional `stop`** (defect 4): whether `stop` should be honored from `idle` regardless of fault state, closing the hand-started-station hole.
- **Broker migration**: deployed code targets `192.168.1.50:1883`; a planned migration to a broker on shari (`192.168.1.139`) exists on an unmerged feature branch and is NOT deployed. The sequencer's config makes this a deploy-time setting; the PRD treats `192.168.1.50` as current production (see `05-deployment-ops.md`).
- **Clean-shutdown `/status` caveat**: on a clean process stop, the sequencer publishes `offline` explicitly — but consumers of *other* slots' `/status` must still not trust `/status` alone, since not all station components do this and retained `online` can outlive a stopped service (see `01-architecture.md`). powerseq's own waits require a *currently observed* online record, which is immune to the stale-retained case.