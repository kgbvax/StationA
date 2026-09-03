# Runtime-library constraints (salvaged from the PRD)
> Salvaged from PRD/03-components/common-runtime-library.md + PRD/01-architecture.md (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [requirement] Shared-library scope rules (what the library must and must not be)

**MUST** — The common runtime library provides exactly these behaviors, and nothing else:
abortable connect; inbound-message handler isolation through a bounded FIFO job queue with
one serialized worker; topic builders for the slot topic grammar; the structural
command-payload envelope; and the connection-lifecycle pattern components assemble from it
(client ID, LWT, on-connect birth sequence, clean-session/retained-replay recovery,
reconnect-backoff ownership).

**MUST NOT** — The library is *not* a shared MQTT client. It holds no broker address, no
credentials, no subscriptions, and publishes nothing itself. Every component constructs and
owns its own client, configured from its own config file.

**MUST NOT** — The library never touches secrets. Broker credentials flow only through
per-component configuration on the target host (0600 TOML files or supervisor environment
files). Any re-implementation must keep credential handling entirely outside shared plumbing.

**MUST** — Shared plumbing can be common across components; per-device logic cannot. One
component must never import another component's internal code — the only sanctioned
cross-component code is the common library.

**MUST** — The library defines no configuration keys and no defaults. It receives everything
as arguments: the already-configured client, the caller-created job queue and its capacity,
and explicit `site`/`station`/`slot` strings. Buffer sizes, timeouts, and cadences live in
components.

## [requirement] Abortable connect: the ≤ 1 s bound

**MUST** — The component must be able to cancel a connect that is in progress. On a shutdown
signal, the in-flight connect call must return control to the caller promptly; a
re-implementation must keep the bound at ≤ 1 s. The return must happen whether or not the
broker is reachable or answering. Component shutdown MUST NOT depend on broker availability.

**MUST** — On cancellation, and on connect failure, the code must tear down the half-open
connection (disconnect the client) before it returns the error.

**MUST** — The library does no retry loop of its own. The component invokes connect once per
program start (or once per an outer retry loop it owns). The component configures automatic
reconnection *after* a successful connect on its own client.

Normative algorithm (stack-agnostic): start the connect on the fully pre-configured client;
await the outcome **concurrently** with the shutdown/cancellation signal — not as a blocking
call; outcome first (error → tear down, return the error; success → return success);
cancellation first (tear down, return the cancellation error). The wrapper must make the
*caller* prompt even if the underlying client library's connect is itself uncancellable —
abandon the stuck connect, because the process exits anyway.

## [requirement] Handler isolation: bounded queue, single serialized worker

**MUST** — An inbound MQTT message handler MUST NOT block on the messaging client's delivery
thread. It MUST NOT publish. It MUST NOT do device input or output. It MUST NOT do long work
there. A handler does at most cheap state capture and a non-blocking submit to the
component's job queue. Inline state capture inside the handler is legal only if it is
provably non-blocking and the queue stays the only publish path. This rule is
library-independent. Rationale: a blocked dispatch thread stalls all inbound delivery; with
the retained-message replay burst immediately after connect the component deadlocks after
the first message.

**MUST** — Submit must be non-blocking. When the queue is full, the queue drops the work
item, and the submit call returns immediately. Submit must never block, never error, never
panic. A blocking submit reintroduces the handler-deadlock incident through the back door.

**MUST** — Exactly one worker must drain the queue; each component (or each slot/client a
multi-slot component fronts) uses one queue and one worker. The worker serializes all state
mutation and publishing strictly FIFO, in submission order among items that were not
dropped. Reconciler-style consumers depend on this: their idempotency and decision logic
require strictly sequential, ordered updates. Do not fan out to a pool of workers.

**MUST** — The worker must stop cleanly on either exit condition: (a) the component's
cancellation token fires, or (b) its owner closes the queue. Receiving "closed" must stop
the worker without invoking an empty/nil work item and without hanging. (Guards a real
path: an owner that closes the queue on an early-return failure path that is not a
cancellation.) A queue item can assume nothing about delivery: no requeue, no
acknowledgment, no retry, no error reporting. Keep queued work items small and
side-effect-ordered; a panicking work item typically kills the worker and the process.

Reference queue capacities (per-component defaults, any non-zero bound conformant): 256 for
the reconciler, the antenna-controller bridge, and the discovery renderer; 64 for the
power-plug bridge; 32 for the radio bridge and the UHF rotator console; the antenna-tuner
bridge runs its own private bounded queue of 8 items with one worker.

## [requirement] Drop-on-full recovery argument and its limit (change-only publishers)

The recovery argument, which any implementation MUST keep valid:

- Every input a component subscribes to is a **retained snapshot** topic (`…/meta`,
  `…/state`, `…/status`). The owner re-announces it on every change, on its own reconnect
  (retained replay to fresh subscribers), and/or on its own periodic heartbeat. So a dropped
  message is re-armed by the next announce of the same snapshot.
- **Limit (a real incident):** the argument holds only for inputs that eventually
  re-announce. A publisher that announces *only on change* never re-arms a drop. Live
  instance: the radio bridge published its `/state` only when a field changed, starving the
  power-amplifier arm relay during radio-idle-but-healthy periods — the PA arm (the
  safety-permit relay that lets the amplifier transmit) dropped even though everything was
  healthy. The arm relay needs a 10 s heartbeat.
- Therefore: any component that serves its state with a heartbeat MUST republish
  periodically; any consumer whose inputs are change-driven (not retained-snapshot-replayed)
  MUST NOT rely on the drop-on-full queue alone for correctness.
- Changing the state model away from single retained snapshots (per-field or delta topics)
  breaks the recovery argument entirely. The retained-snapshot model is a precondition for
  drop-on-full.

Code-check verdict: flexbridge now carries a 5 s `/state` heartbeat
(`Bridge.StateHeartbeat`, `DefaultStateHeartbeat = 5 * time.Second` in
flexbridge/internal/bridge/bridge.go, explicitly sized inside the 10 s pa-arm window) —
fixed in code at flexbridge/cmd/flexbridge/main.go; deployment status should be confirmed.

## [requirement] Re-seed input state from retained replay on every reconnect

**MUST** — Every component MUST be able to re-seed its whole input state from retained
messages on every (re)connect. This is the recovery model that makes drop-on-full and
reconciler restarts survivable: a stateless consumer re-derives everything from the
retained replay of its inputs.

- **Stateless logic consumers** (the reconciler is the reference case) must re-seed all
  input state from retained replay on every (re)connect. The normal way is a **clean
  session**: with a persistent session the broker resumes existing subscriptions and does
  NOT replay retained messages for them, so the consumer can wake with empty inputs and
  never resolve. With a clean session, every (re)connect re-delivers the retained
  `state`/`status`/`meta` of every subscribed slot; messages published during a brief
  offline window are missed — acceptable for a stateless consumer.
- One deployed exception: the power sequencer is a stateless logic consumer on a persistent
  session; it resubscribes unconditionally on every connect, and each subscribe packet makes
  the broker send retained messages for that filter again. Its own `/cmd` stays QoS 0 so the
  broker cannot queue a one-shot start command while it is offline.
- **Device bridges** may use either session mode; their on-connect handler MUST (re)subscribe
  and their birth sequence MUST make them self-describing to fresh subscribers.

## [requirement] Clean-shutdown ordering (reference pattern)

1. Cancel the worker first (stops all queued work).
2. If the connection is open, publish a final retained `offline` on `<slot>/status` and
   disconnect with a short quiesce wait (a fixed wait that lets outbound packets leave
   before the socket closes). Reference values: 0 ms on the error and cancellation paths
   inside the library; 250 ms (reconciler, discovery renderer) or 500 ms (radio bridge,
   power-plug bridge) on the graceful-close path — non-normative shutdown-latency details.

## [requirement] Acceptance tests for any common-runtime re-implementation

1. Submitting to a full queue does not block (bounded-time assertion).
2. The worker runs queued items in order and exits when the cancellation token fires.
3. The worker exits cleanly — no hang, no nil-invocation — when its owner closes the queue
   independently of cancellation.
4. A cancelled/shutdown-signalled in-flight connect returns within ≤ 1 s of the
   cancellation signal even though the broker never answers, and the half-open connection
   was torn down. Hard pass/fail criterion.
5. Topic builders produce the exact strings of the slot grammar (`muehle/hf/radio`,
   `muehle/hf/radio/meta|state|status|cmd`, sibling `("muehle","hf","radio","state")` →
   `muehle/hf/radio/state`).
6. The command payload type round-trips exactly `{"action":"x","value":"y"}` and nothing
   else.

Topic-builder purity constraint (normative): the builders are pure concatenation with `/`
separators, **no validation, no trimming, no defaulting** — an empty part appears verbatim
(e.g. `("", "hf", "radio")` → `/hf/radio`). Defaulting and validation are each component's
config-layer job; a re-implementation may add validation as a non-silent startup failure,
but outputs for valid inputs are exact.

## [defect] Connect wrapper leaks a goroutine per interrupted connect

PRD evidence: the wrapper bridges the uncancellable paho connect wait with a goroutine +
select on ctx.Done; the goroutine blocked in the uncancellable wait leaks (one per
interrupted connect) until the client library's own TCP timeout ends the connect. Accepted
trade-off; the v1 library has no fix. A new stack must ideally make cancellation cancel the
actual connect.

Code-check verdict: still open — shared/mqtt/mqtt.go `Connect` spawns the goroutine with
`tok.Wait()` and no timeout/cleanup path on ctx cancellation.

## [defect] Queue drops have no telemetry beyond a log line

PRD evidence: drops were silent — no counter, no way for the submitter to distinguish
delivered from dropped; recommended fix was a visible drop counter plus rate-limited
warning while remaining strictly non-blocking.

Code-check verdict: partially fixed — shared/mqtt/mqtt.go `Enqueue` now logs
"[mqtt] jobs queue full: dropping job" (2026-09-03, motivated by a dropped retract leaving
its retained cmd armed to re-fire on reconnect); still open: no drop counter, no
rate limiting on the log line.

## [defect] Missing automated tests for two headline behaviors

PRD evidence: the reference's test coverage was minimal; the context-cancel-during-connect
path — the library's headline feature — had no automated test, and neither did the
worker-exits-on-close path.

Code-check verdict: still open — shared/mqtt/mqtt_test.go contains only
TestEnqueueNonBlocking and TestRunJobsRunsClosures (ctx-cancel exit); no test for the
closed-channel exit (acceptance test 3) and none for cancel-during-connect (acceptance
test 4).

## [unique] Incident-derived rationale for the three contracts

Three production incidents justify the contracts; any re-implementation that "simplifies"
them away reproduces the incidents:

1. **Uncancellable connect → SIGKILL.** The MQTT client's connect call blocks until the
   connect finishes and ignores cancellation; a bridge that received SIGTERM during a broker
   outage did not shut down and the supervisor force-killed it after its stop timeout. Hit
   the power-amplifier bridge live; the radio bridge carried the same latent risk.
2. **Handler deadlock → silent stop.** Inbound-message handlers running inline on the
   dispatch thread deadlock the dispatch loop when they call a blocking publish; the bridge
   silently stops consuming messages after the first one. Deadlocked the discovery renderer
   live; the antenna-selection reconciler carried the same latent bug before deployment.
3. **Wrong command-payload shape.** The antenna-tuner bridge originally parsed its command
   argument from a JSON key named after the action
   (`{"action":"set_inline","set_inline":"true"}`) instead of the station-wide `value` key —
   a live interoperability failure that motivated centralizing the envelope in the library.

(Short versions of all three live in shared/mqtt/mqtt.go and shared/schema/schema.go
package comments; the fuller rationale above is the design lineage.)

## [decision] Clean session: contradictory sources, still unresolved

The station integration document named the transport default "Clean session: No —
persistent session". The deployed code disagrees with itself: the reconciler sets clean
session = **true** (a persistent session does not replay retained messages for existing
subscriptions; a stateless reconciler would wake with empty inputs and never resolve), while
the antenna-controller bridge, the discovery renderer, the power sequencer, and the
power-plug bridge explicitly set it **false**, and the radio/PA/tuner/rotator bridges leave
the library default (false) relying on on-connect resubscribe.

What must hold is the §re-seed requirement above; how each component achieves it (clean
session, explicit unsubscribe-then-resubscribe, or persistent session with unconditional
resubscribe) is the open decision. Unresolved fact: a power-sequencer comment claimed the
persistent session itself replays retained values on reconnect — in MQTT the replay comes
from the unconditional resubscribe (each subscribe packet triggers retained delivery), not
from the session flag; relying on resubscribe-driven replay under a persistent session can
be semantically fragile.

Code-check verdict: still open — the split persists in code
(antennaselect/internal/mqtt/client.go:137 `SetCleanSession(true)`; ultrabridge,
hadiscovery, powerseq, shelly-power-bridge, testui all `SetCleanSession(false)`). The
station-integration-model §8 now documents the persistent-session/QoS-0-`/cmd` rules that
came out of the 2026-09-03 offline-replay incident, but the per-component session-flag
choice is still not settled.

## [decision] device_online form: omitted-when-true vs explicit true

The integration model says the field is "omitted when true". Deployed bridges (radio,
antenna-controller, PA, tuner) publish `device_online: true` explicitly. Consumers must
treat both forms as the same (absence = true), or the contract must mandate explicit true.
It is a bus-contract decision; the common library is affected only in that the two-layer
liveness model must be readable in both forms.

Code-check verdict: still open — the model's `/state` template still says "omitted when
true" (docs/station-integration-model.md), while flexbridge now asserts "device_online is
always present (true/false)" (flexbridge/internal/bridge/bridge.go:92). Consumers must
handle both forms until the contract mandates one.

## [decision] Ultrabeam antenna-switch port: 3 or 4 (station-wide)

Repo docs and tests say the Ultrabeam beam antenna is on antenna-switch port 3; the
antennaselect seed config and the console's antenna map say port 4. The live config on
shari is authoritative and was not read before this was recorded. Every document touching
the wiring must present port 3 or port 4 as an open decision pending on-device
confirmation.

Code-check verdict: still open — the contradiction persists in the repo
(docs/station-integration-model.md and root CLAUDE.md say `port3: ultrabeam`;
antennaselect/config.example.toml says `port4 = "ultrabeam"`). Resolution needs the live
shari config or on-device confirmation.

## [decision] When to promote drop telemetry from recommendation to requirement

Silent drop-on-full was survivable in the current model because of the retained-replay
recovery argument; visible drop counters + rate-limited logging were recommended but not
required. The promotion condition: if the re-implementation keeps any change-driven
(non-replaying) input, drop telemetry must be promoted to a requirement — the
flexbridge/pa-arm heartbeat starvation was the live evidence that change-only inputs break
the drop-recovery assumption.

Status note: a log line now exists in shared/mqtt (see the defect section); a counter and
rate limiting do not, so the promotion question is still live for any future change-only
publisher.

## [decision] Heartbeat cadence for change-only publishers (pointer)

Each component spec fixes the live incident and demands: re-announce every consumed state
periodically, or give the consumer another liveness mechanism. The exact cadence (for
example ≤ 5 s radio-state heartbeat) lives in the per-bridge specs and the safety
requirements, not in the common library. The library takes no position on cadence.
(Reference value as implemented: flexbridge 5 s.)

---

# Runtime-library constraints from 01-architecture.md

*(Verbatim from PRD/01-architecture.md.)*
> Extracted from PRD (2026-09-03) before PRD deletion. Prose is verbatim PRD text unless marked.

## [requirement] REQ-RT-1..10 — runtime-library constraints (stack-independent)

These constraints exist because of production incidents with the reference stack's
MQTT client library (Eclipse Paho Go). But they are **stack-independent rules**.
Whatever messaging library an implementation uses, its threading and failure model
must satisfy them. The rationale quotes the live incidents.

1. **REQ-RT-1 (handlers never block or publish).** Inbound-message handler code must
   not block, do long work, or call a blocking publish on the messaging client's
   delivery thread. Handlers must only capture the message and enqueue work.
   *Rationale (live incident):* in the reference library, handlers run inline on the
   connection's dispatch thread. A handler that blocked on publish deadlocked the
   discovery consumer live after the first retained-message burst (the client
   deadlocks after the first message and consumes nothing further). The same
   latent bug existed in the reconciler before deploy.
2. **REQ-RT-2 (serialized single worker).** All state mutation and publishing for
   one component must run on exactly one worker, strictly FIFO, ordered as
   enqueued. The reconciler's deduplication and re-assertion logic depends on
   sequential, ordered updates. Do not fan out to a pool.
3. **REQ-RT-3 (bounded queue, drop-on-full, never block the delivery thread).** The
   handler→worker queue must have a bound. When it is full, the queue **drops the
   work silently** — a blocking submit can reintroduce REQ-RT-1 through the
   back door. This is safe only because of REQ-RT-5 (retained-snapshot replay):
   every dropped input gets a re-arm from the next change, or from the retained
   replay on reconnect.
4. **REQ-RT-4 (connect must be abortable on shutdown).** Process shutdown must
   interrupt an in-flight broker connection try within a stated small bound
   (the reference returns within milliseconds. Pick and pin a bound, for example
   less than 1 s, as an acceptance test), independent of broker reachability.
   On shutdown, the runtime must tear down the half-open connection on both the
   failure path and the cancellation path. *Rationale (live incident):* the
   reference library's connect call blocked, and ignored cancellation. A service
   that received a shutdown signal during a broker outage hung until the
   supervisor force-killed it (this hit the PA bridge live). A new stack must
   ideally make cancellation cancel the actual connect try, not just the caller's
   wait.
5. **REQ-RT-5 (retained-replay recovery).** On every (re)connect, a component must
   re-run its full birth sequence — publish retained `online` on `/status`,
   re-publish `/meta`, re-publish its retained `/state` snapshot, and re-subscribe
   to all inputs. The client's session semantics must re-deliver all retained
   input messages on every reconnect. A stateless consumer can then re-seed its
   inputs. Recovery after any outage is always "replay retained state and
   re-resolve", never "resume a persisted internal model".
6. **REQ-RT-6 (bounded publishes).** A publish must stay bounded in time (the
   sequencer uses a 10-second wait timeout), so a dead broker surfaces as an error
   and does not stall the component forever.
7. **REQ-RT-7 (auto-reconnect).** After a successful connect, connection loss must
   trigger automatic reconnection with backoff (reference defaults: 1 s at the
   start, and 10 s max. The curve itself is an implementation detail. The rule is
   recovery without a person present).
8. **REQ-RT-8 (no credentials in shared plumbing).** Any shared runtime library must
   stay credential-free. Broker addresses, users, and passwords flow only through
   per-component configuration.
9. **REQ-RT-9 (malformed input never crashes).** The component must log and drop
   malformed JSON on any input, and keep the previous inputs. A consumer that sees a
   malformed `/state` must delete that slot's cached snapshot (a good→bad
   transition must not leave a stale value poisoning decisions).
10. **REQ-RT-10 (worker robustness).** The worker must exit cleanly on
    cancellation or queue close, at any point in the component's lifecycle
    (including non-cancellation early returns), and must never invoke work after
    close.

Session-flag nuance behind REQ-RT-5 (from the PRD's open-decisions section): the
ecosystem convention is persistent sessions ("clean session: no"), so subscriptions
and queued messages survive client restarts. But stateless consumers (the
reconciler, the discovery consumer) deliberately use **clean sessions**, so
retained messages replay on every reconnect — without the replay they can wake
with empty inputs and never resolve. The stack-agnostic rule is REQ-RT-5; which
session flag achieves it per component is the decision.

## [requirement] Acceptance tests for the messaging layer (normative)

A re-implementation must pass each test below for its messaging library:

1. A submit to a full queue does not block within a bounded time (REQ-RT-3).
2. The worker runs jobs in order and exits on cancellation (REQ-RT-2, REQ-RT-10).
3. The worker exits cleanly, with no call on a closed worker, when the queue
   closes without cancellation (REQ-RT-10).
4. A cancelled context returns from an in-flight connect within the bound that
   REQ-RT-4 states.
5. The topic builders produce the exact topic grammar strings
   (`<site>/<station>/<slot>` + `/meta`, `/state`, `/status`, `/cmd`).
6. The command envelope round-trips `{"action", "value"}`.

## [defect] Known fragilities of the reference implementation

PRD text: "silent queue drops have no counter and no telemetry, and a job that
panics kills the worker. A re-implementation can do better than the reference on
these two points, but must not do worse."

Code-check verdict (2026-09-03, shared/mqtt/mqtt.go): partially improved. Queue
drops are now **logged** (`[mqtt] jobs queue full: dropping job (worker
saturated)`) — added after the 2026-09-03 audit because a dropped `retract` left
its retained cmd armed to re-fire on reconnect — but there is still **no counter
and no telemetry**. And `RunJobs` invokes each closure with a bare `f()` and **no
`recover()`**: a job that panics still kills the worker goroutine. Both items are
still open against the "must not do worse" bar.

## [unique] Reference-implementation shape (non-normative, for design lineage)

The deployed system centralizes these in a ~130-line shared library module
(`shared/mqtt`) that every bridge imports. It contains a context-aware connect
wrapper (it races the library's connect token against process cancellation, and
disconnects the half-open client on either path) and a bounded jobs-queue with a
single worker goroutine (buffer sizes 32/64/256 depending on bridge). The
topic-string builders and the `{action, value}` payload type live in a sibling
schema package (`shared/schema`). Each bridge still constructs and owns its own
MQTT client, options, and subscriptions. The shared layer is deliberately a thin
set of behavior workarounds, not a client wrapper. Visibility rules forbid
cross-bridge imports of device logic: shared plumbing can be common, per-device
logic cannot.
