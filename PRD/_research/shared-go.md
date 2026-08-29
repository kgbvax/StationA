# shared (Go module) — research spec for re-implementation

Source analyzed: `/Users/ingomar.otter/dev/stationa/shared` — `go.mod`, `go.sum`,
`mqtt/mqtt.go`, `mqtt/mqtt_test.go`, `schema/schema.go`, `schema/schema_test.go`.
Consumer usage cross-checked in: `acom1200s-pa-bridge/cmd/.../main.go`,
`antennaselect/internal/mqtt/client.go`, `flexbridge/cmd/flexbridge/main.go`,
`shelly-power-bridge/cmd/.../main.go`, `pelcobridge2/internal/mqtt/slot.go`,
`ultrabridge/internal/mqtt/client.go`, `hadiscovery/internal/mqtt/client.go`.
Code is truth. There is no README in the module; all behavior is derived from
code and doc comments, which agree.

---

## 1. Purpose & role

Plain-language context first:

- **Amateur-radio (ham) station**: a set of radio equipment (transceiver, power
  amplifier, antenna controllers, antenna switch, power supplies) automated by
  software. This ecosystem is "stationa".
- **MQTT bus**: a publish/subscribe message broker (a server at
  `tcp://192.168.1.50:1883` that relays messages routed by topic string). Every
  controllable device or service in the station is a **slot** with address
  `<site>/<station>/<slot>` (e.g. `muehle/hf/radio`), exposing four topic
  planes: `/meta`, `/state`, `/status`, `/cmd`.
- **Bridge**: a small daemon connecting one physical device (or one logic
  function) to the MQTT bus. Each bridge is a separate Go program deployed as a
  systemd service on the Raspberry Pi "shari".

The `shared` module (`codeberg.org/kgbvax/stationa/shared`, Go 1.26.5) is the
**common library every Go bridge imports**. It is deliberately tiny — two
packages, ~130 lines total — because its purpose is not to wrap MQTT but to
**centralize two specific, live-proven failure modes** of the underlying MQTT
client library, plus the canonical topic-string and command-payload
conventions, so that no bridge re-implements them (and no bridge gets them
wrong again):

1. `shared/mqtt` — the **context-aware connect** wrapper and the
   **background job-queue** used by message handlers. Both exist to work around
   concrete bugs in the Eclipse Paho Go MQTT client
   (`github.com/eclipse/paho.mqtt.golang` v1.5.1), documented in the project
   memory after they hit production:
   - Paho's `Connect().Wait()` blocks ignoring context cancellation → a bridge
     receiving SIGTERM during a broker outage could not shut down; systemd had
     to SIGKILL it after `TimeoutStopSec` (hit live by `acom1200s-pa-bridge`;
     latent in `flexbridge`).
   - Paho runs inbound message handlers inline on its own dispatch goroutine;
     a handler that calls a blocking `Publish` deadlocks the dispatch loop →
     the bridge silently stops consuming messages (deadlocked `hadiscovery`
     live; `antennaselect` would have on deploy).
2. `shared/schema` — string helpers that build the canonical slot topics, and
   the `CmdPayload` type encoding the station-wide **`/cmd` value-key
   convention**: a command's argument rides under the JSON key `value`, never
   under a key named after the action (`atr1k-tuner-bridge` got this wrong
   live, before the convention was centralized).

The module is **explicitly a thin layer, NOT a shared client** (its own
package comment says so). Every bridge still constructs and owns its own Paho
`Client`, `ClientOptions` (broker URL, credentials, client ID, clean-session,
last-will, reconnect settings), subscriptions, and publishing. A
reimplementation in another language must reproduce the *behavior contracts*
below, not the library split — but the contracts exist because of real
production incidents, so they are all BEHAVIOR CONTRACT unless flagged
otherwise.

Dependencies of the module: only `paho.mqtt.golang v1.5.1` (plus its indirect
`gorilla/websocket v1.5.3`, `golang.org/x/net v0.44.0`,
`golang.org/x/sync v0.17.0`). Each consuming bridge's `go.mod` carries both
`require codeberg.org/kgbvax/stationa/shared` and a
`replace … => ../shared` so each bridge builds standalone without the Go
workspace; the root `go.work` ties the modules together. (IMPLEMENTATION
DETAIL — Go-module plumbing.)

---

## 2. Upstream interface

The "upstream" of this library is not a device; it is the **Eclipse Paho Go
MQTT client** (`github.com/eclipse/paho.mqtt.golang` v1.5.1), which in turn
talks MQTT 3.1.1 over TCP to the broker at `tcp://192.168.1.50:1883`
(username `hf`, password held per-bridge in config/env files — never in this
module; the module never sees credentials or broker addresses).

Facts about the Paho client that the shared module's contracts are built on
(these are properties of the upstream library a reimplementation must
understand, whatever MQTT client it uses):

- `client.Connect()` returns a **token**; `token.Wait()` blocks until the
  connect attempt finishes. **`Wait()` takes no context and ignores any
  external cancellation** — that is the root defect `shared/mqtt.Connect`
  works around.
- Inbound message handlers (the callback registered per subscription) run
  **inline on Paho's internal dispatch goroutine** (the
  `matchAndDispatch` goroutine, since `OrderMatters` defaults to true).
  Paho's own docs state the handler "must not block or call functions within
  this package that may block (e.g. Publish) other than in a new go routine".
  If a handler blocks, the dispatch loop stalls; the broker-side read loop
  then blocks pushing the next inbound PUBLISH into the message channel the
  stalled dispatcher is no longer draining. With a retained-message burst
  immediately after connect (this ecosystem subscribes to several retained
  topics), the bridge deadlocks **after the first message** and consumes
  nothing further.
- `client.Disconnect(quopauseMillis uint)` disconnects, waiting
  `quopause` ms for work to quiesce. The shared code always passes a
  timeout of `0` ms on error/cancel paths (immediate teardown) —
  bridge-owned shutdown paths elsewhere use e.g. 250 ms (antennaselect
  `Close()`).

---

## 3. MQTT presence

**The shared module itself publishes and subscribes to nothing.** It has no
topics. Everything below defines what it *builds for its consumers* and the
conventions it enforces.

### 3.1 Topic helpers (`shared/schema`)

All helpers take the string parts `site`, `station`, `slot` and pure-string
concatenate with `/` — no validation, no trimming, no defaults. If a caller
passes an empty part, the empty part appears verbatim in the topic (e.g.
`SlotBase("", "hf", "radio")` → `"/hf/radio"`). Defaulting is the caller's
job (bridges default site `muehle` etc. in their config layers).

| Function | Returns | Contract |
|---|---|---|
| `SlotBase(site, station, slot)` | `<site>/<station>/<slot>` | canonical slot address; e.g. `SlotBase("muehle","hf","radio")` → `muehle/hf/radio` |
| `MetaTopic(site, station, slot)` | `<slot>/meta` | retained "birth certificate" (self-description) plane; e.g. `muehle/power/master/meta` |
| `StateTopic(site, station, slot)` | `<slot>/state` | retained live JSON snapshot plane |
| `StatusTopic(site, station, slot)` | `<slot>/status` | retained liveness plane: literal string `online` / `offline`, set as the MQTT last-will (LWT) |
| `CmdTopic(site, station, slot)` | `<slot>/cmd` | operator → bridge command input plane |
| `SiblingTopic(site, station, slot, suffix)` | `<site>/<station>/<slot>/<suffix>` | same as the plane helpers but for **any** suffix — used by consumer/logic components (antennaselect, powerseq, hadiscovery) that subscribe to other slots' planes |

All are pure functions; exact outputs are pinned by `schema_test.go`:
`muehle/hf/radio`, `muehle/power/master/{meta,state,status,cmd}`,
`SiblingTopic("muehle","hf","radio","state")` → `muehle/hf/radio/state`.

### 3.2 Retained/QoS conventions the module assumes (from consumer code)

The module doesn't enforce these, but the ecosystem contract built on its
helpers is (BEHAVIOR CONTRACT at the ecosystem level):

- `/state` and `/meta`: single retained JSON snapshot per slot — **not**
  per-field topics. QoS 1.
- `/status`: retained, QoS 1, payload is the literal string `online` or
  `offline` (not JSON). Set as the client's MQTT last-will so a crashed
  bridge is stamped `offline` by the broker. Bridge Liveness (`/status`) and
  device-link liveness (the boolean field `device_online` inside `/state`)
  are **two separate layers** — consumers must AND them.
- `/cmd`: not retained, QoS 1 (per-bridge; the convention is "cmd is intent,
  never retained").

---

## 4. Command surface (the `/cmd` value-key convention)

`shared/schema` exports one type, `CmdPayload`:

```go
type CmdPayload struct {
    Action string `json:"action"`
    Value  string `json:"value"`
}
```

Decoded JSON shape on `<slot>/cmd`:

```json
{"action": "<action word>", "value": "<argument>"}
```

**BEHAVIOR CONTRACT — the value-key convention:**

- The command's argument **always rides under the JSON key `value`**, a
  string. It is **never** under a key named after the action (the wrong form,
  which `atr1k-tuner-bridge` shipped live before this was centralized, was
  e.g. `{"action":"set_inline","set_inline":"true"}`).
- `action` is a slot-specific lowercase word — e.g. `"set_power"`, `"tune"`,
  `"set_band"`, `"select"`, `"start"` — chosen by each bridge's own contract.
  The carrier key is invariantly `value`.
- Both fields are strings. Where the argument is semantically a number or
  boolean, it is still stringified into `value` by the sender and parsed by
  the receiving bridge (e.g. `{"action":"set_freq","value":"14000000"}` for a
  `freq_hz` given in Hz as an integer — the bus-wide rule is that frequency
  is always in Hz, never kHz/MHz).

This module defines the envelope only; the set of accepted `action` words per
slot is each bridge's contract (see the per-bridge research files).

---

## 5. Behavior & state machine

### 5.1 `shared/mqtt.Connect(ctx, client) error` — context-aware connect

Signature: `Connect(ctx context.Context, client paho.Client) error`.

Exact algorithm (BEHAVIOR CONTRACT — the whole point of this function):

1. Start the connect: `tok := client.Connect()` (the Paho client's options —
   broker URL, credentials, client ID, clean session, LWT, auto-reconnect,
   handlers — were configured by the caller; this function only performs the
   connect).
2. Spawn a goroutine that calls `tok.Wait()` and sends `tok.Error()` on a
   buffered (size 1) channel. (If the connect hangs forever, this goroutine
   leaks — accepted trade-off; see §9.)
3. `select` on two cases:
   - **Connect finished**: if the token error is non-nil, call
     `client.Disconnect(0)` (immediate, 0 ms quiesce) and return that error.
     If nil, return `nil` — connected.
   - **`ctx` cancelled first** (e.g. SIGTERM while the broker is unreachable
     or auth is failing): call `client.Disconnect(0)` and return
     `ctx.Err()`. This is the workaround for Paho's `Wait()` ignoring
     context: the *caller's* observable behavior is a prompt return on
     cancellation, even though Paho's internal connect attempt keeps running
     until its own TCP timeout.

Contract a reimplementation must reproduce: **a cancelled context (SIGTERM)
must interrupt an in-flight connect attempt promptly** — the bridge's
shutdown must not depend on the broker being reachable. On either failure
path the half-open client is torn down (disconnect) before the error is
returned, so the caller can just exit. There is **no retry loop here**:
`Connect` is called once per program start (or once per outer retry loop the
bridge owns — e.g. `acom1200s-pa-bridge` retries its whole connect with a
1.5× exponential backoff it scales itself, capped). Auto-reconnect *after* a
successful connect is Paho's `SetAutoReconnect(true)`, configured by each
bridge — see §5.4.

### 5.2 `shared/mqtt.Enqueue(jobs chan<- func(), f func())` — non-blocking job submit

Contract (BEHAVIOR CONTRACT):

- Attempts to send the closure `f` on the caller-owned `jobs` channel
  **non-blocking**: if the channel buffer is full, the job is **silently
  dropped** — Enqueue never blocks, never panics, returns nothing.
- Dropping is the deliberate trade-off: the alternative (blocking) would stall
  Paho's dispatch goroutine, which is the very deadlock this queue exists to
  prevent. Recovery from a drop is by design: the ecosystem's state model is
  retained-snapshot based, so the next periodic announce, the next state
  change, or the retained replay on reconnect re-arms the missed work.
  (Package comment: "The next native announce / cmd re-arms once the worker
  drains.")
- Buffer size is **caller-chosen** and varies by bridge in this codebase:
  256 (antennaselect, ultrabridge, hadiscovery), 64 (shelly-power-bridge), 32
  (flexbridge, pelcobridge2). There is no size in the shared module. Any
  non-zero bound is valid; the semantics are drop-on-full regardless of size.

**Rule for handlers (must never be violated in any reimplementation):**
an inbound MQTT message handler must do, at most, cheap state capture and
`Enqueue(...)`; it must **never** call a blocking publish, never block, never
do long work. This is the rule that was violated in the live hadiscovery
deadlock.

### 5.3 `shared/mqtt.RunJobs(ctx, jobs <-chan func())` — the single worker

Contract (BEHAVIOR CONTRACT):

- Runs a loop on the calling goroutine (callers start it with `go RunJobs(...)`)
  until either exit condition:
  1. `ctx` is done (SIGTERM / caller cancellation), or
  2. the `jobs` channel is **closed** by the owner (receive yields the
     comma-ok `ok == false`).
- Each queued closure is run **synchronously, one at a time, in FIFO order**
  on this single goroutine. Ordering of jobs as enqueued is preserved
  (subject to drops on a full buffer — dropped jobs leave gaps; there is no
  requeue, no ack, no retry, no error handling; a panicking closure would
  kill the worker and the program).
- **All closures for one slot/client share one jobs channel + one worker.**
  This is what serializes state mutation and publishing for the whole
  component — the reconciler-style consumers (antennaselect) depend on
  strictly sequential, ordered updates for their idempotency/decision logic.
- Closing the channel is **optional** — cancelling ctx alone stops the worker.
  The comma-ok receive (rather than a bare receive) is load-bearing: a closed
  channel yields nil values forever, and calling a nil closure panics. The
  real failure mode this guards: a caller that closes the channel on an early
  return that is *not* a ctx cancellation (e.g. a `Connect` failure path) —
  the worker must exit cleanly, not panic, and must not assume ctx.Done() is
  the only exit.
- Shutdown ordering used by consumers (antennaselect's `Close`, the reference
  pattern): cancel ctx first (stops the worker so no reconcile/publish work
  races shutdown), then if the connection is open, publish a final retained
  `offline` on `/status` and `Disconnect(250)`.

**Queue semantics summary** (what a different-stack implementation must
reproduce): a single-threaded, FIFO, bounded job queue fed by non-blocking
submit (drop-on-full, silent), drained by exactly one worker goroutine/task,
terminated by either cancellation or channel close, where all MQTT-message
handling work for the component runs on that worker rather than on the
messaging client's delivery thread.

### 5.4 Reconnect semantics (owned by the bridges, not the module — but part of the pattern the module serves)

`shared/mqtt` does no reconnection. The ecosystem reference pattern (from
antennaselect, ultrabridge, hadiscovery consumers) is:

- Paho option `SetAutoReconnect(true)` — the library reconnects automatically
  after a lost connection, with its own internal retry timing (paho default:
  initial 1 s, max 10 s backoff; not overridden by these bridges).
- `SetCleanSession(true)` — **mandatory for logic consumers**. On every
  (re)connect the broker drops the prior session and re-delivers all retained
  messages for fresh subscriptions, which is how consumers re-seed their
  input state after any outage. (A persistent session would *not* replay
  retained messages for existing subscriptions, and the reconciler would
  wake with empty inputs and never resolve.)
- `SetOnConnectHandler` — runs on every (re)connect; the reference behavior:
  publish retained `online` to own `/status` (QoS 1), re-publish `/meta`,
  re-subscribe to all inputs.
- `SetConnectionLostHandler` — logs only.
- Client ID derives from the slot address: `<site>-<station>-<slot>` (e.g.
  `muehle-hf-antenna-select`) so a duplicate connection is diagnosable on the
  broker.
- LWT: `SetWill(statusTopic, "offline", 1, true)` — retained QoS-1 will on the
  own `/status` topic.

### 5.5 Timing constants that exist in this module

There are **no timeouts, delays, or cadences inside the module itself**
besides the literal `Disconnect(0)` (immediate, 0 ms quiesce) on both of
`Connect`'s failure paths. All backoff (e.g. acom bridge's 1.5× scaling),
heartbeat periods, and reconnect parameters live in the consumers.

---

## 6. Configuration

**The module has no configuration, no config keys, no defaults, and never
touches secrets.** It receives everything as function arguments:

- `Connect(ctx, client)` — the `client` arrives fully configured by the
  bridge (broker URL, credentials, client ID, clean session, LWT, handlers).
- `Enqueue(jobs, f)` / `RunJobs(ctx, jobs)` — the jobs channel and its buffer
  size are created by the bridge.
- All schema helpers take explicit `site/station/slot` strings from the
  bridge's TOML config (bridge-level keys `site`, `station`, `slot`, defaults
  `muehle` / `hf` / per-bridge).

MQTT credentials (user `hf`, per-bridge password) are handled entirely by
consumer bridges in 0600 TOML files / systemd EnvironmentFiles on the target
host, per the repo's config-and-secrets convention. A reimplementation must
keep credential handling out of any shared plumbing.

---

## 7. Deployment

- The module is **library-only**: no `main`, no binary, no systemd unit, no
  deploy script, no target host. It is deployed only inside each bridge
  binary that imports it.
- It lives as subdirectory `shared/` of the stationa monorepo, a Go module of
  its own (`codeberg.org/kgbvax/stationa/shared`), tied into the root
  `go.work` workspace. Every consuming bridge's `go.mod` carries
  `require codeberg.org/kgbvax/stationa/shared` plus a
  `replace codeberg.org/kgbvax/stationa/shared => ../shared` so each bridge
  builds standalone from its own directory without the workspace. (All
  IMPLEMENTATION DETAIL for the Go build system.)
- Consumers are forbidden by Go's `internal/` visibility rule from importing
  another bridge's internals; the shared module is the only sanctioned
  cross-bridge code. A new-stack reimplementation should preserve the
  equivalent rule: shared plumbing may be common; per-device logic may not be
  imported across components.

---

## 8. Invariants & safety rules

1. **A message handler must never block** — never call a blocking publish,
   never do long work, inside the MQTT client's delivery thread. Handlers
   enqueue; the worker publishes. Violation deadlocks the client (live
   incident: hadiscovery; the same latent bug existed in antennaselect).
2. **Enqueue must never block** — on a full queue it drops, silently. A
   blocking submit would reintroduce invariant 1 through the back door.
3. **All state mutation + publishing for one component runs on one worker,
   strictly FIFO** — consumers' idempotency and decision logic depends on
   sequential ordered updates. Do not fan out to a pool.
4. **A cancelled context must interrupt an in-flight connect** — shutdown
   must never require the broker to be reachable. On both connect-failure and
   cancellation the half-open client is disconnected before returning.
5. **The worker must survive its owner closing the jobs channel at any
   point** (including non-cancellation early returns) — exit cleanly, never
   invoke a nil closure.
6. **`/cmd` arguments ride under the `value` key** — always. Any
   reimplementation of any bridge's command parser must accept
   `{"action": "...", "value": "..."}` and must not invent per-action keys.
7. **`/status` is bridge LWT liveness; `/state.device_online` is device-link
   liveness** — the module doesn't implement this, but the topic helpers
   serve a model where consumers must AND both; don't collapse the two planes
   into one.
8. **Retained single-snapshot model**: one retained JSON `/state` per slot,
   not per-field topics. The drop-on-full queue is only safe *because* the
   model is retained-snapshot + replay-on-reconnect; changing to
   per-field/delta topics would break the drop-recovery argument.
9. **No secrets in shared plumbing** — credentials flow only through
   per-bridge config.

---

## 9. Known defects & fragilities

1. **Paho `Connect().Wait()` ignores context — worked around, not fixed.**
   The upstream defect remains: `shared/mqtt.Connect` makes the *caller*
   prompt, but the internal goroutine blocked in `tok.Wait()` **leaks** until
   Paho's own TCP timeout resolves the attempt (goroutine + token leak per
   interrupted connect). Accepted trade-off; there is no way to cancel a
   Paho token's wait. A new stack should ideally make cancellation cancel
   the actual connect attempt.
2. **Silent job drops.** A full queue drops work with no log, no counter, no
   telemetry. If a consumer's inputs are change-driven rather than
   retained-snapshot-driven, a drop can mean a permanently missed transition
   (cf. the project memory on flexbridge's "change-only" `/state` starving
   pa-arm's 10 s heartbeat — a consumer-level instance of exactly this class
   of bug). The queue's safety depends on every input eventually being
   re-announced or replayed from retain.
3. **No requeue/ack/error handling in the worker.** A closure that panics
   kills the worker goroutine (and typically the process). A closure that
   errors has nowhere to report it. Consumers must keep closures trivial.
4. **Enqueue's drop is invisible to the enqueuer** — no boolean return. A
   consumer cannot distinguish delivered from dropped, so it cannot
   compensate (e.g. by re-enqueueing later).
5. **Schema helpers do no validation.** Empty or malformed site/station/slot
   parts pass through into malformed topics verbatim. Defaults and
   validation are every caller's responsibility; the helpers only guarantee
   the join order and separators.
6. **`Value` is always a string.** Numeric/boolean arguments require
   stringification on send and parsing on receive — a standing source of
   per-bridge parsing bugs that the type cannot catch. (Conventional, not
   accidental, but fragile.)
7. **Reconnect timing is whatever the underlying client does.** The module
   imposes no reconnect policy; consumers rely on paho's internal
   auto-reconnect backoff (default 1 s initial / 10 s max) and on the
   clean-session retained replay for recovery. A new-stack client with
   different reconnect behavior would still be contract-compliant as long
   as invariants 1–4 and the retained-replay recovery hold.
8. **Test coverage is minimal** (three tests: Enqueue-doesn't-block,
   RunJobs-runs-and-exits, topic strings). The ctx-cancel-during-Connect
   path — the module's headline feature — has **no test** (it is hard to test
   against a hanging token).

---

## 10. Re-implementation notes

**Must be preserved verbatim (BEHAVIOR CONTRACT):**

- The topic grammar `<site>/<station>/<slot>/{meta|state|status|cmd}` and
  the exact plane semantics (meta = retained self-description; state =
  retained JSON snapshot; status = retained literal `online`/`offline` +
  LWT; cmd = intent input).
- The `/cmd` envelope `{"action": string, "value": string}` with the
  value-key rule (argument under `value`, never under an action-named key).
- The single-worker bounded FIFO job-queue pattern: handler threads only
  enqueue; exactly one worker mutates state and publishes; submit is
  non-blocking with silent drop-on-full; worker exits cleanly on either
  cancellation or queue close; ordering preserved among non-dropped jobs.
- Prompt interruption of an in-flight connect on shutdown (context /
  SIGTERM), with the half-open connection torn down on both failure and
  cancellation paths.
- Per-component serialized publishing (one queue per slot/client), and the
  two-layer liveness split (`/status` vs `/state.device_online`) the helpers
  presuppose.
- Credential-free shared plumbing (no secrets in the library).

**Free to change (IMPLEMENTATION DETAIL):**

- Go, paho.mqtt.golang, goroutines, channels, the `func()` closures (an
  event/message-typed queue would be equally valid), the exact buffer sizes
  (32/64/256 across current bridges), the module split and import mechanism,
  the `Disconnect(0)`/`Disconnect(250)` quiesce millisecond values (visible
  only as shutdown latency), and paho's specific auto-reconnect backoff
  curve.
- The drop-on-full strategy could be upgraded (bounded blocking with
  timeout, or a logged drop counter) **as long as the delivery thread is
  never blocked** — but any change must keep the recovery story valid:
  every input must eventually be re-announced or replayed from retained
  state, since drops are silent and final.

**Minimal acceptance tests a reimplementation should have** (mirroring the
existing ones plus the gaps):

1. Submitting to a full queue does not block (bounded-time assertion).
2. The worker runs queued jobs in order and exits on cancellation.
3. The worker exits cleanly (no nil-call, no hang) when the queue is closed
   independent of cancellation.
4. A cancelled context returns from an in-flight connect within a small
  bound even though the broker never answers. *(Currently untested in the
   Go module — add it.)*
5. Topic builders produce the exact strings of §3.1.
6. CmdPayload round-trips `{"action":"x","value":"y"}` and nothing else.