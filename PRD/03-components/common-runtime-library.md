# 03 — common runtime library: cross-cutting plumbing for every component

## 0. Purpose

Every component of the Mühle station automation system talks to the same MQTT
bus. The components are the device bridges (radio, power amplifier, antenna
tuner, rotator, power plugs, and more) and the logic services (the
antenna-selection reconciler, the power sequencer, the discovery renderer —
see §1). MQTT is a
lightweight publish/subscribe protocol. A central **broker** server relays
messages routed by hierarchical text topics. All components must obey the same
connect/liveness/payload rules. This document specifies the **common runtime
library**: the small set of cross-cutting plumbing behaviors that every
component depends on. These behaviors must exist exactly once, in the same
form, in every component. No component re-implements (or mis-implements) them.
Each requirement below is a behavior contract that any technology must
reproduce. The document is **stack-agnostic** by design. The reference
implementation happens to be a small Go module (~130 lines, two packages: an
MQTT-connect wrapper plus a job queue, and a set of topic-string builders plus
one command-payload type) imported by every Go bridge. A re-implementation in
another stack needs the *same behaviors*, not the same code shape. Three of the
contracts here exist because of specific production incidents (non-cancellable
connect, handler deadlock, and a wrong command-payload shape that shipped
live). They are requirements with incident-derived rationale, not style
preferences. For the bus-wide topic/payload/liveness model itself see
`02-interface-spec.md`. For how components compose on the bus see
`01-architecture.md`.

## 1. Vocabulary

- **Bridge** — a small always-on daemon (in the reference: a systemd service on
  the Raspberry Pi "shari") connecting one physical device — or one logic
  function — to the MQTT bus.
- **Slot** — the bus address of one component: `<site>/<station>/<slot>`, for
  example `muehle/hf/radio`. Production site is `muehle`, stations are `hf` and
  `uhf`.
- **Reconciler** — the antenna-selection logic service. It re-derives the
  desired actuator state from the retained snapshots it observes and
  publishes one resolved intent stream. Its decision logic needs strictly
  serialized, ordered updates (§5.1).
- **Plane** — one of the four per-slot topics `<slot>/meta` (retained
  self-description), `<slot>/state` (retained JSON snapshot),
  `<slot>/status` (retained liveness string), `<slot>/cmd` (command input).
- **Retained message** — an MQTT message the broker stores and delivers
  immediately to every future subscriber of that topic. The station builds its
  whole state model on retained snapshots.
- **LWT (Last Will and Testament)** — a message the broker publishes on the
  client's behalf if the client disappears *uncleanly* (crash, network loss).
- **QoS 1** — MQTT "at least once" delivery. It is the default for every
  station publish and subscribe. Sanctioned QoS-0 exceptions exist for
  one-shot command inputs (see `02-interface-spec.md` R1.16) and for passive
  consumers.
- **Clean session** — an MQTT flag. With a clean session the broker keeps no
  per-client state across connections. Each connect starts with fresh
  subscriptions. A persistent (non-clean) session resumes prior subscriptions
  and queued messages. It does **not** re-deliver retained messages for
  subscriptions the session already holds.
- **Dispatch thread** — the messaging client library's internal thread that
  runs inbound-message callbacks. In many MQTT libraries (including the
  reference implementation's) this is a single thread. Blocking it stalls all
  further message delivery.
- **SIGTERM** — the Unix process-termination signal. A service supervisor (in
  the reference: systemd) uses it to ask a process to shut down. **SIGKILL** is
  the forced kill that follows if the process does not exit in time.

## 2. Scope and structural rules

### 2.1 What the library is and is not

**MUST** — The common runtime library provides exactly these behaviors, and
nothing else:

1. Abortable connect (§4).
2. Inbound-message handler isolation through a bounded FIFO job queue with one
   serialized worker (§5).
3. Topic builders for the slot topic grammar (§6).
4. The structural command-payload envelope (§7).
5. The connection-lifecycle pattern components assemble from it: client ID,
   LWT, on-connect birth sequence, clean-session/retained-replay recovery
   (§8), and reconnect-backoff ownership (§9).

**MUST NOT** — The library is *not* a shared MQTT client. It holds no broker
address, no credentials, no subscriptions, and publishes nothing itself. Every
component constructs and owns its own client, configured from its own config
file. The library is a thin layer each component wraps. This puts the two
incident-proven failure modes (§4, §5) and the two string conventions (§6, §7)
in one place, so they cannot drift per component.

**MUST NOT** — The library never touches secrets. Broker credentials (user,
password) flow only through per-component configuration on the target host:
TOML files (a standard human-readable key/value configuration-file format)
with permission 0600 (a Unix file permission: readable and writable only by
the owning service account), or supervisor environment files. Any
re-implementation must keep credential handling entirely outside shared
plumbing.

**MUST** — Shared plumbing can be common across components. Per-device logic
cannot be common. One component must never import another component's internal code —
the only sanctioned cross-component code is the common library. (The Go
language's module `internal/` visibility rule enforces this in the reference.
A re-implementation needs the same discipline — per-component code private,
common runtime public.)

### 2.2 Library has no configuration

**MUST** — The library defines no configuration keys and no defaults. It
receives everything as arguments: the already-configured client (§4), the
caller-created job queue and its capacity (§5), and explicit
`site`/`station`/`slot` strings from the component's config (§6). Buffer sizes,
timeouts, and cadences live in components.

## 3. Incident-derived rationale (why these contracts are requirements)

Three production incidents in the current system justify three of the
contracts. Any re-implementation that "simplifies" them away will reproduce the
incidents:

1. **Uncancellable connect → SIGKILL.** The reference MQTT library's connect
   call blocks until the connect finishes and ignores cancellation. A bridge
   that received SIGTERM during a broker outage did not shut down. The
   supervisor had to force-kill it after its stop timeout. The power-amplifier
   bridge hit this live. The radio bridge carries the same latent risk.
2. **Handler deadlock → silent stop.** Inbound-message handlers in the
   reference library run inline on the client's dispatch thread. A handler that
   calls a blocking publish deadlocks the dispatch loop. The bridge then
   silently stops consuming messages after the first one. This deadlocked the
   discovery renderer live. The antenna-selection reconciler carried the same
   latent bug before deployment.
3. **Wrong command-payload shape.** The antenna-tuner bridge originally parsed
   its command argument from a JSON key named after the action
   (`{"action":"set_inline","set_inline":"true"}`). It did not use the
   station-wide `value` key. That was a live interoperability failure. That
   failure motivated centralizing the envelope (§7).

## 4. Abortable connect

### 4.1 Requirement

**MUST** — The component must be able to cancel a connect that is in
progress. When the component receives a shutdown signal (SIGTERM or a
cancellation token that acts the same way), the in-flight connect call must
return control to the caller promptly. The bound is small. The reference
returns as soon as the code observes the cancellation. A re-implementation
must keep the bound at ≤ 1 s. The return must happen whether or not the broker
is reachable or answering. Component shutdown MUST NOT depend on broker
availability.

**MUST** — On cancellation, and on connect failure, the code must tear down
the half-open connection (disconnect the client) before it returns the error.
The caller can then simply exit.

**MUST** — The library does no retry loop of its own. The component invokes
connect once per program start (or once per an outer retry loop it owns, §9).
The component configures automatic reconnection *after* a successful connect on
its own client (§8).

### 4.2 Normative algorithm

A stack-agnostic connect wrapper must behave as follows:

1. Start the connect on the fully pre-configured client (broker URL,
   credentials, client ID, session flag, last will, handlers are the caller's
   configuration).
2. Await the outcome of the connect **concurrently** with the
   shutdown/cancellation signal — not as a blocking call.
3. Outcome first: on error, tear down the connection and return the error. On
   success, return success.
4. Cancellation first: tear down the connection and return the cancellation
   error.

The point of the wrapper is observable behavior, not mechanism. The underlying
client library's connect call can be itself uncancellable. Even then the
wrapper must make the *caller* prompt — abandon the stuck connect — because
the process exits anyway. Ideally a re-implementation uses an MQTT client whose
connect is genuinely cancellable (see §11 for the reference's leaked-goroutine
trade-off).

## 5. Handler isolation: bounded queue, single serialized worker

### 5.1 The rule

**MUST** — An inbound MQTT message handler MUST NOT block on the messaging
client's delivery thread. It MUST NOT publish. It MUST NOT do device input or
output. It MUST NOT do long work there either. A handler must do at most
cheap state capture and a non-blocking submit of a work item to the
component's job queue. Any work that blocks, publishes, or takes part in
serialized decision logic must run on the queue's single worker, not on the
delivery thread. Inline state capture inside the handler is legal only if it
is provably non-blocking and the queue stays the only publish path. This rule
is library-independent. Whatever MQTT stack the
re-implementation uses, the re-implementation MUST isolate handler work from
the receive path. Rationale: incident 2 of §3 — a blocked dispatch thread
stalls all inbound delivery. With a retained-message replay burst immediately
after connect (the station always subscribes to retained topics) the component
deadlocks after the first message.

**MUST** — Submit must be non-blocking. When the queue is full, the queue
drops the work item, and the submit call returns immediately. Submit must
never block, never error, never panic. A blocking submit can reintroduce the
deadlock of §3 incident 2 through the back door.

**MUST** — Exactly one worker must drain the queue. Each component (or each
slot/client a multi-slot component fronts) uses one queue and one worker. The
worker serializes all state mutation and publishing for that component through
that queue strictly FIFO, in submission order among items that were not
dropped. The reconciler-style consumers depend on this: their idempotency and
decision logic requires strictly sequential, ordered updates. Do not fan out to
a pool of workers.

**MUST** — The worker must stop cleanly on either of two exit conditions.
Condition (a): the component's cancellation token fires. Condition (b): its
owner closes the queue. The worker must not assume cancellation is the only exit.
Receiving the "closed" indication must stop the worker without invoking an
empty/nil work item and without hanging. (In the reference this guards a
real path: an owner that closes the queue on an early-return failure path that
is not a cancellation.)

A queue item can assume nothing about delivery: there is no requeue, no
acknowledgment, no retry, and no error reporting. Components must keep queued
work items small and side-effect-ordered. A work item that
throws/panics will typically kill the worker and the process.

### 5.2 Queue capacity

**MUST** — The queue must have a bound. The capacity is a per-component choice.
Any non-zero bound is conformant, and the drop-on-full semantics are the same
regardless of capacity. Reference values (defaults each component chooses):
256 items for the reconciler, the antenna-controller bridge, and the discovery
renderer. 64 for the power-plug bridge. 32 for the radio bridge and the UHF
rotator console. The antenna-tuner bridge does not use the shared queue: it
runs its own private bounded queue of 8 items with one worker (see §11).

### 5.3 Why silent drop-on-full is acceptable — and its limits

Dropping work is the deliberate trade-off. The alternative (blocking) stalls
the delivery thread — the exact deadlock the queue exists to stop. The
recovery argument, which a re-implementation MUST keep valid:

- Every input a component subscribes to is a **retained snapshot** topic
  (`…/meta`, `…/state`, `…/status`). The owner re-announces it on every
  change, on its own reconnect (retained replay to fresh subscribers), and/or
  on its own periodic heartbeat. So a dropped message is re-armed by the next
  announce of the same snapshot.
- **Limit (a real incident):** this argument holds only for inputs that
  eventually re-announce. A publisher that announces *only on change* never
  re-arms a drop. Live instance: the radio bridge publishes its `/state` only
  when a field changes. That starves the power-amplifier arm relay during
  radio-idle-but-healthy periods — the arm drops even though everything is
  healthy. The arm relay is the safety-permit relay that lets the power
  amplifier transmit. The relay needs a 10 s heartbeat (see `06-safety.md`). Any
  component that serves its state with a heartbeat
  MUST republish periodically (see the pa-arm heartbeat requirement in
  `06-safety.md` and the per-bridge specs). And any consumer whose inputs are
  change-driven (not retained-snapshot-replayed) MUST NOT rely on the
  drop-on-full queue alone for correctness.
- Changing the state model away from single retained snapshots (for example to
  per-field or delta topics) will break the recovery argument entirely. The
  retained-snapshot model is a precondition for drop-on-full (see
  `02-interface-spec.md` §1.3 and §1.5).

### 5.4 Drop telemetry (recommendation)

The reference implementation drops silently — no log line, no counter, no way
for the submitter to know a drop happened (see §11). For a re-implementation
this document **recommends** (recommended, not a requirement): the submit
primitive increments a visible drop counter and logs a rate-limited warning on
drop, while remaining strictly non-blocking. A bounded-block-with-timeout
submit is an acceptable alternative. The condition: the timeout must never
block the delivery thread. For handler-side submits the timeout is therefore
effectively zero. This is a recommendation rather than a requirement
because the retained-replay recovery argument (§5.3) makes silent drops
survivable in the current model. Making it a requirement is an open decision
(§12).

## 6. Topic builders (slot topic grammar)

### 6.1 Requirement

**MUST** — The library must provide pure functions that build the canonical
topic strings from the parts `site`, `station`, `slot` (and, for consumers, an
arbitrary suffix). No component hand-concatenates topic strings:

| Builder | Exact output | Purpose |
|---|---|---|
| slot base | `<site>/<station>/<slot>` | canonical slot address, for example `muehle/hf/radio`. |
| meta topic | `<slot>/meta` | retained birth-certificate plane. |
| state topic | `<slot>/state` | retained live JSON snapshot plane. |
| status topic | `<slot>/status` | retained liveness plane: literal string `online` / `offline`, registered as the MQTT last will. |
| cmd topic | `<slot>/cmd` | command-intent input plane. |
| sibling topic | `<site>/<station>/<slot>/<suffix>` | any plane of *another* slot — used by consumer/logic components (antenna-select, sequencer, discovery) to subscribe to other components' topics. |

Worked exact outputs (this pinning is exact): `muehle/hf/radio`,
`muehle/hf/radio/meta`, `muehle/hf/radio/state`, `muehle/hf/radio/status`,
`muehle/hf/radio/cmd`, and sibling `("muehle","hf","radio","state")` →
`muehle/hf/radio/state`.

**MUST** — The builders are pure concatenation with `/` separators in the
order shown. They do **no validation, no trimming, no defaulting**: an
empty part appears verbatim (for example `("", "hf", "radio")` →
`/hf/radio`). Defaulting (`site = muehle` and the same for the other parts)
and validation are each component's config-layer job. A re-implementation can
add validation as an improvement (make it a non-silent startup failure), but
the outputs for valid inputs are exact.

### 6.2 Grammar and vocabulary (referenced, defined elsewhere)

The address grammar `site / station / slot`, the rule that the slot segment is
a canonical role name (never a device/product name), and the plane semantics
(retention, payload shapes, two-layer liveness) are bus contracts.
`02-interface-spec.md` §1.1–§1.4 defines them once, and §5 of that document
defines the liveness contract. This library's builders are
the single implementation of that grammar. The builders presuppose the
two-layer liveness split. `/status` is the bridge-process liveness topic
(LWT). The boolean field `device_online` inside `/state` is device-link
liveness. Components must not collapse the two planes into one.

## 7. Command payload envelope (the value-key convention)

### 7.1 Requirement

**MUST** — The library must provide one payload type for `/cmd` messages
with exactly two fields, both JSON strings:

```json
{ "action": "<action word>", "value": "<argument>" }
```

**MUST** — The argument ALWAYS rides under the JSON key `value`, as a
**string** — never under an action-named key. Rationale: §3
incident 3 (the wrong live form was `{"action":"set_inline","set_inline":"true"}`).
Centralizing the type in the library makes the convention structural. Senders
build it and receivers parse it through the same type, so it cannot drift per
component.

**MUST** — Where the argument is semantically a number or boolean, the
sender still stringifies it into `value`. The receiver parses it.
Booleans become `"true"`/`"false"`. Numbers keep their decimal string.
Example: `{"action":"set_freq","value":"14000000"}`. Frequency is always in
integer Hz across the whole bus, never kHz or MHz.

**MUST** — `action` is a slot-specific lowercase word (for example
`set_power`, `tune`, `set_band`, `select`, `start`) chosen by each component's
own contract. The carrier key is invariantly `value`. The library defines the
envelope only — the set of accepted actions per slot is each component's
specification.

### 7.2 The sanctioned exceptions

A component's `expose` block (the consumer-neutral field/command surface in
`/meta`, see `02-interface-spec.md` §2.3) can declare a `command` descriptor.
That descriptor is the authority for the payload shape. Exactly three shapes
exist, and a command builder must honor all three:

1. `action` + `value_key` — payload `{"action":"<action>","<value_key>":<value>}`.
   A `value_key` other than `value` re-homes the argument. Deployed case: the
   antenna-controller bridge's frequency command carries an integer under
   `freq_hz`: `{"action":"frequency","freq_hz":14025000}`, exactly as its
   `expose.command` declares
   (`{action:"frequency", value_key:"freq_hz", value_type:"int"}`).
2. `value_key` only — payload `{"<value_key>":"<value>"}` with no `action`
   key. Deployed case: the antenna-switch bridge's select command sends
   `{"select":"port3"}`, declared as `{value_key:"select", value_type:"string"}`
   with no `action` key.
3. `action` only — payload `{"action":"<action>"}` with no value key (a pure
   button, for example `retract`).

The library's two-field envelope (§7.1) is the default for everything else.
A component that declares a `command` descriptor in its `expose` block must
follow that descriptor. No component can invent per-action keys outside a
declared `expose.command` descriptor. See `02-interface-spec.md` §2.3 for the
full descriptor semantics.

### 7.3 Receiver-side error behavior (bus rule the library serves)

The receiving component logs and drops unknown or unparseable `/cmd`
payloads. It logs and ignores unknown actions. A component never crashes on
bad intent. Commands are fire-and-observe: consumers confirm through a read of
the producing component's `/state`, never by assuming emission implies success. `02-interface-spec.md`
§1.5 defines retention rules for `/cmd` (normally not retained. Idempotent
actuator setpoints retained. One-shots must clear their retained topic).

## 8. Connection lifecycle pattern (assembled by components from the library)

These are component-level requirements the library's contracts serve. We
restate them here because they form the recovery model that the library's
connect/queue assume.

1. **Client ID** — derived from the slot address as
   `<site>-<station>-<slot>` (slash → hyphen), for example
   `muehle-hf-antenna-select` (config-overridable). The broker can then
   diagnose a repeated connection.
2. **Last will** — registered at every connect: retained QoS-1 `offline` on
   the component's own `<slot>/status` topic.
3. **On-connect birth sequence** — runs on every (re)connect: publish retained
   `online` to `<slot>/status` (QoS 1), re-publish retained `/meta`,
   re-publish retained `/state` snapshot, (re)subscribe to all inputs
   (including `<slot>/cmd` for commandable components).
4. **Connection-lost** — log only. Recovery comes through automatic reconnect
   (§9) plus the birth sequence.
5. **Clean shutdown ordering** — reference pattern. First cancel the worker
   (this stops all queued work). Then, if the connection is open, publish a
   final retained `offline` on `<slot>/status` and disconnect with a short
   quiesce wait (a fixed wait that lets outbound packets leave before the
   socket closes). Reference values: immediate/0 ms on the error and
   cancellation paths inside the library. 250 ms (reconciler, discovery
   renderer) or 500 ms (radio bridge, power-plug bridge) on the component's
   graceful-close path — these are non-normative shutdown-latency details.

### 8.1 Clean session + retained replay: the recovery requirement

**MUST** — Every component MUST be able to re-seed its whole input state from
retained messages on every (re)connect. This is the recovery model that
makes drop-on-full (§5.3) and reconciler restarts survivable: a stateless
consumer re-derives everything from the retained replay of its inputs.

- **Stateless logic consumers** (the reconciler is the reference case) must
  re-seed all input state from retained replay on every (re)connect. The
  normal way is a **clean session**. With a persistent session the broker
  resumes existing subscriptions and does NOT replay retained messages for
  them. The consumer can wake with empty inputs and never resolve. With a
  clean session, every (re)connect re-delivers the retained
  `state`/`status`/`meta` of every subscribed slot. Messages published during
  a brief offline window are simply missed — acceptable, because the consumer
  is stateless and re-derives from snapshots.
- One deployed exception: the power sequencer is a stateless logic consumer
  that uses a persistent session. It resubscribes unconditionally on every
  connect. Each subscribe packet makes the broker send the retained messages
  for that filter again, so its inputs still re-seed (see
  `03-components/powerseq.md`). Its own `/cmd` stays QoS 0 so the broker
  cannot queue a one-shot start command while it is offline (see
  `02-interface-spec.md` R1.16). Whether a persistent session with
  resubscribe-driven replay is robust is an open question (§12, open
  decision 1).
- **Device bridges** can use a persistent session or a clean session. In
  either case their on-connect handler MUST (re)subscribe, and their own
  snapshot republication (birth sequence) MUST make them self-describing to
  fresh subscribers.
- The station integration document states "clean session: No" as the default,
  while the deployed code is split — see §12, open decision 1.

### 8.2 Status-plane realities the library presupposes

- On a **clean process shutdown** the broker does NOT fire the last will (the
  will fires only on unclean loss) — hence requirement 5 above. A component
  publishes `offline` itself before disconnecting. But a process stopped while
  the broker is unreachable (or killed before it can publish) leaves retained
  `online` on its `/status`. **Consumers MUST NOT trust `/status` alone.**
  Combine it with `device_online` and with staleness checks. See
  `02-interface-spec.md` §5.1 and §5.2 for the two-layer AND rule.
- `/status` is a plain string (`online`/`offline`, not JSON), retained, QoS 1 —
  so it maps to standard home-automation availability without templating.

## 9. Reconnect backoff ownership

**MUST** — The library owns no reconnect policy. Each component owns its own:

- **Post-connect loss:** the component configures its client's automatic
  reconnect (library-internal timing in the reference: first 1 s, max 10 s
  backoff — a non-normative default. Any curve is conformant provided the
  birth sequence of §8 item 3 re-runs on every reconnect).
- **First connect failure:** components diverge by role (both are conformant
  — pick deliberately). Option (a): exit the process and let the service
  supervisor restart the unit, which gives supervisor-owned backoff. The
  power-plug and antenna-controller bridges and the logic services use this.
  Option (b): configure client-internal first-connect retry at a 5 s
  interval. The radio, power-amplifier, tuner, and HF-rotator bridges use
  this.
  A component with an outer run/retry loop owns its own exponential backoff
  (for example the PA bridge's device-link loop: first 2 s, ×1.5 growth,
  capped at 60 s — a component-level default for its serial device, not a bus
  rule).
- **Device-link (non-broker) reconnect loops** — for example reopening a
  dropped USB-serial adapter — are per-component concerns with per-component
  backoff. See the per-bridge specs under `03-components/`.

## 10. Acceptance tests

A re-implementation's common runtime must pass equivalents of these tests
(mirroring the reference's tests plus its known gaps):

1. Submitting to a full queue does not block (bounded-time assertion).
2. The worker runs queued items in order and exits when the cancellation
   token fires.
3. The worker exits cleanly — no hang, no nil-invocation — when its owner
   closes the queue independently of cancellation.
4. A cancelled/shutdown-signalled in-flight connect returns within ≤ 1 s of
   the cancellation signal even though the broker never answers, and the
   half-open connection was torn down. This bound is a hard pass/fail
   criterion. (The reference implementation lacks this test — add it.)
5. Topic builders produce the exact strings of §6.1.
6. The command payload type round-trips exactly
   `{"action":"x","value":"y"}` and nothing else.

## 11. Reference-implementation notes (non-normative)

How the current Go implementation happens to do it. None of this describes
required behavior:

- **Shape:** a single Go module `shared/` (~130 lines), two packages:
  `mqtt` (the context-aware connect, `Enqueue`, `RunJobs`) and `schema`
  (six string builders + the `CmdPayload` struct). Every Go bridge imports it
  through per-module dependency directives plus a local path override, so each
  bridge builds standalone. The module has no `main`, no binary, no service
  unit — it deploys only inside each bridge binary.
- **MQTT stack:** Eclipse Paho Go client v1.5.1 (MQTT 3.1.1, broker
  `tcp://192.168.1.50:1883`). Its known foot-guns are exactly §3 items 1–2.
  The shared module exists to centralize their workarounds.
- **Connect wrapper mechanism:** a goroutine plus a select against context
  cancellation bridges the connect token's blocking wait. The caller stays
  prompt. **Known defect:** the goroutine blocked in the uncancellable wait
  leaks (one per interrupted connect) until the client library's own TCP
  timeout ends the connect. This is an accepted trade-off. That library has no
  fix. A new stack must ideally make cancellation cancel the actual connect.
- **Queue mechanism:** untyped closures (`func()`) on a buffered channel.
  **Known defects:** silent drops (no log, no counter. The submitter cannot
  distinguish delivered from dropped), no requeue/ack/error surface, and
  minimal test coverage (the context-cancel-during-connect path — the
  library's headline feature — has no automated test).
- **Quiesce values:** the library always disconnects with a 0 ms quiesce on
  error/cancellation paths. Components use 250 ms (reconciler, discovery
  renderer) or 500 ms (radio bridge, power-plug bridge) on graceful close.
  This is visible only as shutdown latency.
- **Clean-session split in the deployed code:** the reconciler uses clean
  session = true (with the rationale of §8.1). The antenna-controller
  bridge, the discovery renderer, the power sequencer, and the power-plug
  bridge explicitly use clean session = false. The power sequencer relies on
  its unconditional on-connect resubscribe to re-seed its inputs (§8.1). The
  radio, PA, tuner, and rotator bridges do not set the flag (library default:
  false) but rely on their on-connect resubscribe. The radio,
  power-amplifier, tuner, and HF-rotator bridges additionally enable a 5 s
  client-internal first-connect retry.
- **Divergences from the §5.1 worker rule:** the power-amplifier bridge and
  the HF-rotator bridge handle `/cmd` inline on the dispatch thread. Their
  handlers do blocking device writes under a mutex (serial for the
  power-amplifier bridge, websocket for the rotator bridge). The deployed
  code tolerates this, but it is a latent risk. The power sequencer mutates
  sequencer state inline in its subscribe handlers. The antenna-tuner bridge
  does not use the shared queue: it runs its own bounded channel with a
  capacity of 8 and one worker.

## 12. Open decisions and unresolved facts

1. **Clean session: contradictory sources.** The station integration document
   names the transport default as "Clean session: No — persistent session".
   That choice keeps subscriptions and queued messages across client restarts.
   The deployed code disagrees with itself. The reconciler sets clean session
   = **true**. Its code comment explains: a persistent session does not replay
   retained messages for existing subscriptions. A stateless reconciler then
   wakes with empty inputs and never resolves. The antenna-controller bridge,
   the discovery renderer, the power sequencer, and the power-plug bridge
   explicitly set it **false**. The radio/PA/tuner/rotator bridges leave the
   library default (false) with on-connect resubscribe. The normative
   requirement in §8.1 (re-seed all input state from retained replay on every
   reconnect) is what must hold. How each component achieves it (clean
   session, explicit unsubscribe-then-resubscribe, or a persistent session
   with unconditional resubscribe) is an open decision for the re-implementation
   team. Evidence: integration model §2 ("Clean session: No") against
   `antennaselect/internal/mqtt/client.go` (clean-session-true rationale
   comment), against `ultrabridge`/`hadiscovery` client code (explicit false),
   against `powerseq/internal/mqtt/client.go` (explicit false, with a persistent
   session plus a QoS-0 own-`/cmd` subscription, see R1.16), and against
   `shelly-power-bridge/cmd/shelly-power-bridge/main.go` (explicit false).
   Unresolved fact: the power-sequencer comment claims the persistent session
   itself replays retained values on reconnect. In MQTT the replay comes from
   the unconditional resubscribe (each subscribe packet triggers retained
   delivery), not from the session flag. Relying on resubscribe-driven
   replay under a persistent session can be semantically fragile.
2. **Drop telemetry: recommendation or requirement?** The reference drops
   queued work silently (§5.4, §11). This document recommends visible drop
   counters + rate-limited logging but does not need it, because the
   retained-replay recovery argument covers the current state model. If the
   re-implementation keeps any change-driven (non-replaying) input, the team
   must promote drop telemetry to a requirement. The flexbridge/pa-arm
   heartbeat starvation (§5.3 limit) is the live evidence: change-only inputs
   break the drop-recovery assumption.
3. **Heartbeat republish cadence for change-only publishers.** Each component
   spec fixes the live incident: the radio `/state` published only on change
   starved the 10 s pa-arm heartbeat. Each spec demands: re-announce every
   consumed state periodically, or give the consumer another liveness
   mechanism (see `06-safety.md`). The exact cadence (for example ≤ 5 s
   radio-state heartbeat) appears there, not here. The common library takes
   no position on cadence.
4. **`device_online` form.** The integration model says the field is "omitted
   when true". Deployed bridges (radio, antenna-controller, PA, tuner)
   publish `device_online: true` explicitly. Consumers must treat both forms
   as the same (absence = true), or the contract must mandate explicit
   true. This is a bus-contract decision — see `02-interface-spec.md`. It
   affects the library only in that the two-layer liveness model (§6.2) must
   be readable in both forms.
5. **Ultrabeam switch-port conflict (station-wide).** Repo docs and tests say
   the Ultrabeam beam antenna is on antenna-switch port 3. The deployed
   `antennaselect` seed config and the console's antenna map say port 4. The
   live config on shari is authoritative. Nobody read it before the author
   wrote this PRD. Every document touching the wiring must present port 3 or
   port 4 as an open decision pending on-device confirmation. Not a library
   concern, listed here for cross-reference completeness (see
   `03-components/antenna-switch.md` and `01-architecture.md`).
6. **Broker topology migration.** All deployed components point at
   `tcp://192.168.1.50:1883` (the shack broker). A migration to a broker on
   shari (`192.168.1.139`) exists on an unmerged feature branch (committed
   but NOT deployed as of 2026-08-29). The library takes no position (broker
   address is per-component config). The PRD treats 192.168.1.50 as
   production. See `05-deployment-ops.md`.