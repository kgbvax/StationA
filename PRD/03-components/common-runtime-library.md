# 03 — common runtime library: cross-cutting plumbing for every component

## 0. Purpose

Every component of the Mühle station automation system — the device bridges
(radio, power amplifier, antenna tuner, rotator, power plugs, …) and the logic
services (antenna-selection policy, power sequencer, discovery renderer) — talks
to the same MQTT bus (MQTT: a lightweight publish/subscribe protocol; a central
**broker** server relays messages routed by hierarchical text topics) and must
obey the same connect/liveness/payload rules. This document specifies the
**common runtime library**: the small set of cross-cutting plumbing behaviors
that every component depends on and that must exist exactly once, identically,
in every component, rather than being re-implemented (and mis-implemented) per
component. It is written **stack-agnostically**: each requirement below is a
behavior contract that any technology must reproduce. The reference
implementation happens to be a small Go module (~130 lines, two packages: an
MQTT-connect wrapper plus a job queue, and a set of topic-string builders plus
one command-payload type) imported by every Go bridge; a re-implementation in
another stack needs the *equivalent behaviors*, not the same code shape. Three
of the contracts here exist because of specific production incidents
(non-cancellable connect, handler deadlock, and a wrong command-payload shape
that shipped live) — they are requirements with incident-derived rationale, not
style preferences. For the bus-wide topic/payload/liveness model itself see
`02-interface-spec.md`; for how components compose on the bus see
`01-architecture.md`.

## 1. Vocabulary

- **Bridge** — a small always-on daemon (in the reference: a systemd service on
  the Raspberry Pi "shari") connecting one physical device — or one logic
  function — to the MQTT bus.
- **Slot** — the bus address of one component: `<site>/<station>/<slot>`, e.g.
  `muehle/hf/radio`. Production site is `muehle`, stations are `hf` and `uhf`.
- **Plane** — one of the four per-slot topics `<slot>/meta` (retained
  self-description), `<slot>/state` (retained JSON snapshot),
  `<slot>/status` (retained liveness string), `<slot>/cmd` (command input).
- **Retained message** — an MQTT message the broker stores and delivers
  immediately to every future subscriber of that topic; the station's whole
  state model is built on retained snapshots.
- **LWT (Last Will and Testament)** — a message the broker publishes on the
  client's behalf if the client disappears *uncleanly* (crash, network loss).
- **QoS 1** — MQTT "at least once" delivery; used for every station publish and
  subscribe.
- **Clean session** — an MQTT flag: with a clean session the broker keeps no
  per-client state across connections; each connect starts with fresh
  subscriptions. A persistent (non-clean) session resumes prior subscriptions
  and queued messages but does **not** re-deliver retained messages for
  subscriptions the session already holds.
- **Dispatch thread** — the messaging client library's internal thread that
  runs inbound-message callbacks. In many MQTT libraries (including the
  reference implementation's) this is a single thread; blocking it stalls all
  further message delivery.
- **SIGTERM** — the Unix process-termination signal; how a service supervisor
  (in the reference: systemd) asks a process to shut down. **SIGKILL** is the
  forced kill that follows if the process does not exit in time.

## 2. Scope and structural rules

### 2.1 What the library is and is not

**SHALL** — The common runtime library provides exactly these behaviors, and
nothing else:

1. Abortable connect (§4).
2. Inbound-message handler isolation via a bounded FIFO job queue with one
   serialized worker (§5).
3. Topic builders for the slot topic grammar (§6).
4. The structural command-payload envelope (§7).
5. The connection-lifecycle pattern components assemble from it: client ID,
   LWT, on-connect birth sequence, clean-session/retained-replay recovery
   (§8), and reconnect-backoff ownership (§9).

**SHALL NOT** — The library is *not* a shared MQTT client: it holds no broker
address, no credentials, no subscriptions, and publishes nothing itself. Every
component constructs and owns its own client, configured from its own config
file. The library is a thin layer each component wraps, so that the two
incident-proven failure modes (§4, §5) and the two string conventions (§6, §7)
exist once and cannot drift per component.

**SHALL NOT** — The library never touches secrets. Broker credentials (user,
password) flow only through per-component configuration (0600 TOML files /
supervisor environment files on the target host). Any re-implementation must
keep credential handling entirely outside shared plumbing.

**SHALL** — Shared plumbing may be common across components; per-device logic
may not. One component must never import another component's internal code —
the only sanctioned cross-component code is the common library. (In the
reference this is enforced by the Go language's module `internal/` visibility
rule; a re-implementation needs the equivalent discipline — per-component code
private, common runtime public.)

### 2.2 Library has no configuration

**SHALL** — The library defines no configuration keys and no defaults. It
receives everything as arguments: the already-configured client (§4), the
caller-created job queue and its capacity (§5), and explicit
`site`/`station`/`slot` strings from the component's config (§6). Buffer sizes,
timeouts, and cadences live in components.

## 3. Incident-derived rationale (why these contracts are requirements)

Three production incidents in the current system are the justification for
three of the contracts. Any re-implementation that "simplifies" them away will
reproduce the incidents:

1. **Uncancellable connect → SIGKILL.** The reference MQTT library's connect
   call blocks until the attempt finishes and ignores cancellation. A bridge
   receiving SIGTERM during a broker outage could not shut down; the supervisor
   had to force-kill it after its stop timeout (hit live by the
   power-amplifier bridge; present but latent in the radio bridge).
2. **Handler deadlock → silent stop.** Inbound-message handlers in the
   reference library run inline on the client's dispatch thread. A handler that
   calls a blocking publish deadlocks the dispatch loop: the bridge silently
   stops consuming messages after the first one (deadlocked the discovery
   renderer live; the antenna-selection reconciler carried the same latent
   bug before deployment).
3. **Wrong command-payload shape.** The antenna-tuner bridge originally parsed
   its command argument from a JSON key named after the action
   (`{"action":"set_inline","set_inline":"true"}`) instead of the station-wide
   `value` key — a live interoperability failure that motivated centralizing
   the envelope (§7).

## 4. Abortable connect

### 4.1 Requirement

**SHALL** — A connect attempt in progress MUST be cancellable: when the
component receives a shutdown signal (SIGTERM or an equivalent cancellation
token), an in-flight connect attempt SHALL return control to the caller
promptly — within a small bound (the reference returns as soon as the
cancellation is observed; a re-implementation SHOULD bound this at ≤ 1 s) —
whether or not the broker is reachable or answering. Component shutdown MUST
NOT depend on broker availability.

**SHALL** — On cancellation, and on connect failure, the half-open connection
SHALL be torn down (the client disconnected) before the error is returned, so
the caller can simply exit.

**SHALL** — The library performs no retry loop of its own. Connect is invoked
once per program start (or once per an outer retry loop the component owns,
§9). Automatic reconnection *after* a successful connect is configured by the
component on its own client (§8).

### 4.2 Normative algorithm

A stack-agnostic connect wrapper SHALL behave as follows:

1. Start the connect attempt on the fully pre-configured client (broker URL,
   credentials, client ID, session flag, last will, handlers are the caller's
   configuration).
2. Await the attempt's outcome **concurrently** with the shutdown/cancellation
   signal — not as a blocking call.
3. Outcome first: on error, tear down the connection and return the error; on
   success, return success.
4. Cancellation first: tear down the connection and return the cancellation
   error.

The point of the wrapper is observable behavior, not mechanism: even if the
underlying client library's connect call is itself uncancellable, the wrapper
SHOULD make the *caller* prompt — abandoning the stuck attempt — because the
process is exiting anyway. Ideally a re-implementation uses an MQTT client
whose connect is genuinely cancellable (see §11 for the reference's
leaked-goroutine trade-off).

## 5. Handler isolation: bounded queue, single serialized worker

### 5.1 The rule

**SHALL** — An inbound MQTT message handler MUST NOT block, MUST NOT publish
(blocking), and MUST NOT do long work on the messaging client's delivery
thread. A handler SHALL do at most cheap state capture and a non-blocking
submit of a work item to the component's job queue. All state mutation and
all publishing for a component SHALL run on the queue's single worker, not on
the delivery thread. This rule is library-independent: whatever MQTT stack the
re-implementation uses, handler work MUST be isolated from the receive path.
Rationale: incident 2 of §3 — a blocked dispatch thread stalls all inbound
delivery, and with a retained-message replay burst immediately after connect
(the station always subscribes to retained topics) the component deadlocks
after the first message.

**SHALL** — Submit SHALL be non-blocking: when the queue is full, the work
item is dropped, and the submit call returns immediately. It SHALL never
block, never error, never panic. A blocking submit would reintroduce the
deadlock of §3 incident 2 through the back door.

**SHALL** — Exactly one worker SHALL drain the queue. Each component (or each
slot/client a multi-slot component fronts) uses one queue and one worker, so
that all state mutation and publishing for that component are serialized
strictly FIFO, in submission order among items that were not dropped. The
reconciler-style consumers depend on this: their idempotency and decision
logic requires strictly sequential, ordered updates. Do not fan out to a pool
of workers.

**SHALL** — The worker SHALL terminate cleanly on either of two independent
exit conditions: (a) the component's cancellation token fires, or (b) the
queue is closed by its owner. The worker SHALL NOT assume cancellation is the
only exit; receiving the "closed" indication SHALL stop it without invoking
an empty/nil work item and without hanging. (In the reference this guards a
real path: an owner that closes the queue on an early-return failure path that
is not a cancellation.)

**SHALL** — A queue item may assume nothing about delivery: there is no
requeue, no acknowledgment, no retry, and no error reporting. Components
SHALL keep queued work items small and side-effect-ordered; a work item that
throws/panics will typically kill the worker and the process.

### 5.2 Queue capacity

**SHALL** — The queue SHALL be bounded. The capacity is a per-component
choice; any non-zero bound is conformant, and the drop-on-full semantics are
identical regardless of capacity. Reference values (defaults each component
chooses): 256 items for the reconciler, the antenna-controller bridge, and the
discovery renderer; 64 for the power-plug bridge; 32 for the radio bridge and
the UHF rotator console.

### 5.3 Why silent drop-on-full is acceptable — and its limits

Dropping work is the deliberate trade-off: the alternative (blocking) stalls
the delivery thread, which is the exact deadlock the queue exists to prevent.
The recovery argument, which a re-implementation MUST keep valid:

- Every input a component subscribes to is a **retained snapshot** topic
  (`…/meta`, `…/state`, `…/status`) whose owner re-announces it — on every
  change, on its own reconnect (retained replay to fresh subscribers), and/or
  on its own periodic heartbeat. So a dropped message is re-armed by the next
  announce of the same snapshot.
- **Limit (a real incident):** this argument holds only for inputs that
  eventually re-announce. A publisher that announces *only on change* never
  re-arms a drop. Live instance: the radio bridge publishes its `/state` only
  when a field changes, which starves the power-amplifier arm relay's 10 s
  heartbeat during radio-idle-but-healthy periods — the arm drops even though
  everything is healthy. Any component whose state is consumed with a
  heartbeat MUST republish periodically (see the pa-arm heartbeat requirement
  in `06-safety.md` and the per-bridge specs), and any consumer whose inputs
  are change-driven (not retained-snapshot-replayed) MUST NOT rely on the
  drop-on-full queue alone for correctness.
- Changing the state model away from single retained snapshots (e.g. to
  per-field or delta topics) would break the recovery argument entirely; the
  retained-snapshot model is a precondition for drop-on-full (see
  `02-interface-spec.md` §3).

### 5.4 Drop telemetry (recommendation)

The reference implementation drops silently — no log line, no counter, no way
for the submitter to know a drop happened (see §11). For a re-implementation
this document **recommends** (SHOULD, not SHALL): the submit primitive
increments a visible drop counter and logs a rate-limited warning on drop,
while remaining strictly non-blocking. A bounded-block-with-timeout submit is
an acceptable alternative ONLY if the delivery thread can still never be
blocked — i.e. the timeout must be effectively zero for handler-side submits.
This is a recommendation rather than a requirement because the
retained-replay recovery argument (§5.3) makes silent drops survivable in the
current model; making it a requirement is an open decision (§12).

## 6. Topic builders (slot topic grammar)

### 6.1 Requirement

**SHALL** — The library SHALL provide pure functions that build the canonical
topic strings from the parts `site`, `station`, `slot` (and, for consumers, an
arbitrary suffix), so no component hand-concatenates topic strings:

| Builder | Exact output | Purpose |
|---|---|---|
| slot base | `<site>/<station>/<slot>` | canonical slot address; e.g. `muehle/hf/radio` |
| meta topic | `<slot>/meta` | retained birth-certificate plane |
| state topic | `<slot>/state` | retained live JSON snapshot plane |
| status topic | `<slot>/status` | retained liveness plane: literal string `online` / `offline`, registered as the MQTT last will |
| cmd topic | `<slot>/cmd` | command-intent input plane |
| sibling topic | `<site>/<station>/<slot>/<suffix>` | any plane of *another* slot — used by consumer/logic components (antenna-select, sequencer, discovery) to subscribe to other components' topics |

Worked exact outputs (these string values are pinned): `muehle/hf/radio`,
`muehle/hf/radio/meta`, `muehle/hf/radio/state`, `muehle/hf/radio/status`,
`muehle/hf/radio/cmd`, and sibling `("muehle","hf","radio","state")` →
`muehle/hf/radio/state`.

**SHALL** — The builders are pure concatenation with `/` separators in the
order shown. They perform **no validation, no trimming, no defaulting**: an
empty part appears verbatim (e.g. `("", "hf", "radio")` → `/hf/radio`).
Defaulting (`site = muehle` etc.) and validation are each component's
config-layer job. A re-implementation MAY add validation as an improvement
(make it a non-silent startup failure), but the outputs for valid inputs are
exact.

### 6.2 Grammar and vocabulary (referenced, defined elsewhere)

The address grammar `site / station / slot`, the rule that the slot segment is
a canonical role name (never a device/product name), and the plane semantics
(retention, payload shapes, two-layer liveness) are bus contracts defined once
in `02-interface-spec.md` §2–§4; this library's builders are the single
implementation of that grammar. The builders presuppose the two-layer liveness
split — `/status` is the bridge-process liveness topic (LWT), the boolean field
`device_online` inside `/state` is device-link liveness — and components SHALL
NOT collapse the two planes into one.

## 7. Command payload envelope (the value-key convention)

### 7.1 Requirement

**SHALL** — The library SHALL provide one payload type for `/cmd` messages
with exactly two fields, both JSON strings:

```json
{ "action": "<action word>", "value": "<argument>" }
```

**SHALL** — A command's argument ALWAYS rides under the JSON key `value`, as
a **string** — never under a key named after the action. Rationale: §3
incident 3 (the wrong live form was `{"action":"set_inline","set_inline":"true"}`).
Centralizing the type in the library makes the convention structural: senders
build it and receivers parse it through the same type, so it cannot drift per
component.

**SHALL** — Where the argument is semantically a number or boolean, it is
still stringified into `value` by the sender and parsed by the receiver:
booleans as `"true"`/`"false"`, numbers as their decimal string (e.g.
`{"action":"set_freq","value":"14000000"}`; frequency is always in integer Hz
across the whole bus, never kHz/MHz).

**SHALL** — `action` is a slot-specific lowercase word (e.g. `set_power`,
`tune`, `set_band`, `select`, `start`) chosen by each component's own
contract; the carrier key is invariantly `value`. The library defines the
envelope only — the set of accepted actions per slot is each component's
specification.

### 7.2 The one sanctioned exception

A component's `expose` block (the consumer-neutral field/command surface in
`/meta`, see `02-interface-spec.md` §3.2) may declare a `command` descriptor
with a `value_key` other than `value`, which re-homes the argument. Exactly
one deployed component uses this: the antenna-controller bridge's frequency
command carries an integer under `freq_hz`:
`{"action":"frequency","freq_hz":14025000}`, exactly as its `expose.command`
declares (`{action:"frequency", value_key:"freq_hz", value_type:"int"}`).
The `expose` descriptor is the authority for which key carries the value. The
library's two-field envelope is the default for everything else; no component
may invent per-action keys outside this exception.

### 7.3 Receiver-side error behavior (bus rule the library serves)

Unknown or unparseable `/cmd` payloads are logged and dropped by the
receiving component; unknown actions are logged and ignored; a component never
crashes on bad intent. Commands are fire-and-observe: consumers confirm via
`/state`, never by assuming emission implies success. Retention rules for
`/cmd` (normally not retained; idempotent actuator setpoints retained;
one-shots must clear their retained topic) are defined in
`02-interface-spec.md` §5.

## 8. Connection lifecycle pattern (assembled by components from the library)

These are component-level requirements the library's contracts serve; they
are restated here because they form the recovery model the library's
connect/queue assume.

1. **Client ID** — derived from the slot address as
   `<site>-<station>-<slot>` (slash → hyphen), e.g. `muehle-hf-antenna-select`
   (config-overridable), so a duplicate connection is diagnosable on the
   broker.
2. **Last will** — registered at every connect: retained QoS-1 `offline` on
   the component's own `<slot>/status` topic.
3. **On-connect birth sequence** — runs on every (re)connect: publish retained
   `online` to `<slot>/status` (QoS 1), re-publish retained `/meta`,
   re-publish retained `/state` snapshot, (re)subscribe to all inputs
   (including `<slot>/cmd` for commandable components).
4. **Connection-lost** — log only; recovery is via automatic reconnect (§9)
   plus the birth sequence.
5. **Clean shutdown ordering** — reference pattern: cancel the worker
   (stopping all queued work) first; then, if the connection is open, publish
   a final retained `offline` on `<slot>/status` and disconnect with a short
   quiesce wait (reference values: immediate/0 ms on error and cancellation
   paths inside the library; 250 ms on the component's graceful-close path —
   these are non-normative shutdown-latency details).

### 8.1 Clean session + retained replay: the recovery requirement

**SHALL** — Every component MUST be able to re-seed its entire input state
from retained messages on every (re)connect. This is the recovery model that
makes drop-on-full (§5.3) and reconciler restarts survivable: a stateless
consumer re-derives everything from the retained replay of its inputs.

- **Stateless logic consumers** (the reconciler is the reference case) SHALL
  use a **clean session**: with a persistent session the broker resumes
  existing subscriptions and does NOT replay retained messages for them, so
  the consumer would wake with empty inputs and never resolve. With a clean
  session, every (re)connect re-delivers the retained `state`/`status`/`meta`
  of every subscribed slot, and messages published during a brief offline
  window are simply missed — acceptable, because the consumer is stateless
  and re-derives from snapshots.
- **Device bridges** may use a persistent session or a clean session, but in
  either case their on-connect handler MUST (re)subscribe and their own
  snapshot republication (birth sequence) MUST make them self-describing to
  fresh subscribers.
- The station integration document states "clean session: No" as the default,
  while the deployed code is split — see §12, open decision 1.

### 8.2 Status-plane realities the library presupposes

- On a **clean process shutdown** the broker does NOT fire the last will (the
  will fires only on unclean loss) — hence requirement 5 above: a component
  publishes `offline` itself before disconnecting. But a process stopped while
  the broker is unreachable (or killed before it can publish) leaves retained
  `online` on its `/status`. **Consumers MUST NOT trust `/status` alone**;
  combine with `device_online` and with staleness checks. See
  `02-interface-spec.md` §4 for the two-layer AND rule.
- `/status` is a plain string (`online`/`offline`, not JSON), retained, QoS 1 —
  so it maps to standard home-automation availability without templating.

## 9. Reconnect backoff ownership

**SHALL** — The library owns no reconnect policy. Each component owns its own:

- **Post-connect loss:** the component configures its client's automatic
  reconnect (library-internal timing in the reference: initial 1 s, max 10 s
  backoff — a non-normative default; any curve is conformant provided the
  birth sequence of §8 item 3 re-runs on every reconnect).
- **Initial connect failure:** components diverge by role (both are
  conformant; pick deliberately): (a) exit the process and let the service
  supervisor restart the unit (the common device-bridge pattern, giving
  supervisor-owned backoff), or (b) configure client-internal initial-connect
  retry (the radio bridge does this with a 5 s retry interval). A component
  with an outer run/retry loop owns its own exponential backoff (e.g. the PA
  bridge's device-link loop: initial 2 s, ×1.5 growth, capped at 60 s — a
  component-level default for its serial device, not a bus rule).
- **Device-link (non-broker) reconnect loops** — e.g. reopening a dropped
  USB-serial adapter — are per-component concerns with per-component backoff;
  see the per-bridge specs under `03-components/`.

## 10. Acceptance tests

A re-implementation's common runtime SHALL pass equivalents of these tests
(mirroring the reference's tests plus its known gaps):

1. Submitting to a full queue does not block (bounded-time assertion).
2. The worker runs queued items in order and exits when the cancellation
   token fires.
3. The worker exits cleanly — no hang, no nil-invocation — when the queue is
   closed independently of cancellation.
4. A cancelled/shutdown-signalled in-flight connect returns within a small
   bound even though the broker never answers, and the half-open connection
   was torn down. (The reference implementation lacks this test — add it.)
5. Topic builders produce the exact strings of §6.1.
6. The command payload type round-trips exactly
   `{"action":"x","value":"y"}` and nothing else.

## 11. Reference-implementation notes (non-normative)

How the current Go implementation happens to do it; none of this is required
behavior:

- **Shape:** a single Go module `shared/` (~130 lines), two packages:
  `mqtt` (the context-aware connect, `Enqueue`, `RunJobs`) and `schema`
  (six string builders + the `CmdPayload` struct). Imported by every Go bridge
  via per-module dependency directives plus a local path override so each
  bridge builds standalone. The module has no `main`, no binary, no service
  unit — it deploys only inside each bridge binary.
- **MQTT stack:** Eclipse Paho Go client v1.5.1 (MQTT 3.1.1, broker
  `tcp://192.168.1.50:1883`). Its known foot-guns are exactly §3 items 1–2;
  the shared module exists to centralize their workarounds.
- **Connect wrapper mechanism:** the connect token's blocking wait is bridged
  through a goroutine + a select against context cancellation, so the caller
  is prompt. **Known defect:** the goroutine blocked in the uncancellable
  wait leaks (one per interrupted connect) until the client library's own TCP
  timeout resolves the attempt — an accepted trade-off with no fix available
  in that library. A new stack should ideally make cancellation cancel the
  actual connect attempt.
- **Queue mechanism:** untyped closures (`func()`) on a buffered channel.
  **Known defects:** silent drops (no log, no counter; the submitter cannot
  distinguish delivered from dropped), no requeue/ack/error surface, and
  minimal test coverage (the context-cancel-during-connect path — the
  library's headline feature — has no automated test).
- **Quiesce values:** the library always disconnects with a 0 ms quiesce on
  error/cancellation paths; components use 250 ms on graceful close. Visible
  only as shutdown latency.
- **Clean-session split in the deployed code:** the reconciler uses clean
  session = true (with the rationale of §8.1); the antenna-controller bridge
  and the discovery renderer explicitly use clean session = false; the radio,
  PA, tuner, and rotator bridges do not set the flag (library default: false)
  but rely on their on-connect resubscribe; the radio bridge additionally
  enables a 5 s client-internal initial-connect retry.

## 12. Open decisions & unresolved facts

1. **Clean session: contradictory sources.** The station integration document
   specifies "Clean session: No — persistent session so subscriptions and
   queued messages survive client restarts" as the transport default. The
   deployed code disagrees with itself: the reconciler sets clean session =
   **true** (with an explicit code comment: a persistent session does not
   replay retained messages for existing subscriptions, so the stateless
   reconciler would wake with empty inputs and never resolve), while the
   antenna-controller bridge and the discovery renderer explicitly set it
   **false**, and the radio/PA/tuner/rotator bridges leave the library default
   (false) with on-connect resubscribe. The normative requirement in §8.1
   (re-seed all input state from retained replay on every reconnect) is what
   must hold; how each component achieves it (clean session vs. explicit
   unsubscribe-then-resubscribe in the on-connect handler) is an open
   decision for the re-implementation team. Evidence: integration model §2
   ("Clean session: No") vs. `antennaselect/internal/mqtt/client.go`
   (clean-session-true rationale comment) vs. `ultrabridge`/`hadiscovery`
   client code (explicit false).
2. **Drop telemetry: recommendation or requirement?** The reference drops
   queued work silently (§5.4, §11). This document recommends visible drop
   counters + rate-limited logging but does not require it, because the
   retained-replay recovery argument covers the current state model. If the
   re-implementation keeps any change-driven (non-replaying) input, drop
   telemetry should be promoted to a requirement — the flexbridge/pa-arm
   heartbeat starvation (§5.3 limit) is the live evidence that change-only
   inputs break the drop-recovery assumption.
3. **Heartbeat republish cadence for change-only publishers.** The live
   incident (radio `/state` published only on change starving the 10 s pa-arm
   heartbeat) is fixed per-bridge in the component specs (each consumed
   state must be re-announced periodically or the consumer must have an
   equivalent liveness mechanism — see `06-safety.md`), but the exact cadence
   (e.g. ≤ 5 s radio-state heartbeat) is specified there, not here; the
   common library takes no position on cadence.
4. **`device_online` form.** The integration model says the field is "omitted
   when true"; deployed bridges (radio, antenna-controller, PA, tuner)
   publish `device_online: true` explicitly. Consumers must treat both forms
   as equivalent (absence = true), or the contract must mandate explicit
   true. This is a bus-contract decision — see `02-interface-spec.md`; it
   affects the library only in that the two-layer liveness model (§6.2) must
   be readable in both forms.
5. **Ultrabeam switch-port conflict (station-wide).** Repo docs and tests say
   the Ultrabeam beam antenna is on antenna-switch port 3; the deployed
   `antennaselect` seed config and the console's antenna map say port 4. The
   live config on shari is authoritative but was not readable when this PRD
   was written; every document touching the wiring must present port 3 vs
   port 4 as an open decision pending on-device confirmation. Not a library
   concern, listed here for cross-reference completeness (see
   `03-components/antenna-switch.md` and `01-architecture.md`).
6. **Broker topology migration.** All deployed components point at
   `tcp://192.168.1.50:1883` (the shack broker). A migration to a broker on
   shari (`192.168.1.139`) exists on an unmerged feature branch (committed
   but NOT deployed as of 2026-08-29). The library takes no position (broker
   address is per-component config); the PRD treats 192.168.1.50 as
   production. See `05-deployment-ops.md`.