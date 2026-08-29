# powerseq — research spec for re-implementation

Source analyzed: `/Users/ingomar.otter/dev/stationa/powerseq` (Go; `cmd/powerseq/main.go`,
`internal/config/config.go`, `internal/seq/seq.go`, `internal/mqtt/client.go`,
`config.example.toml`, `deploy.sh`, `README.md`, `CLAUDE.md`, `docs/powerseq-mqtt-api.md`,
test files `internal/seq/seq_test.go`, `internal/config/config_test.go`).
Code is truth; where docs disagree it is called out below.

---

## 1. Purpose & role

**powerseq** is the *station startup/shutdown sequencer* of the Mühle amateur-radio
station automation ecosystem. Plain-language context a non-ham reader needs:

- **Amateur radio (ham) station**: a collection of radio equipment — a transceiver
  ("TRX", a FLEX-8400 radio used to transmit/receive), a power amplifier ("PA",
  an ACOM 1200S that boosts transmit power), an antenna tuner, antennas and their
  switching/tuning controllers.
- **Mains**: the building's electrical power supply. Two smart plugs ("Shelly"
  devices, each an MQTT-connected switched power outlet) gate the station: the
  *station master mains* (everything) and the *13.8 V PSU* (the DC power supply
  that feeds the control electronics of the radio gear).
- The station is automated over an **MQTT bus** (a publish/subscribe message
  broker at `tcp://192.168.1.50:1883`). Every controllable device or service is a
  **slot** with a topic address of the form `<site>/<station>/<slot>` (default
  site `muehle`; the HF-station slots live under station `hf`). Each slot exposes
  four topics: `/meta` (self description), `/state` (retained JSON snapshot of its
  current condition), `/status` (liveness: literal string `online` or `offline`),
  and `/cmd` (JSON command input).

powerseq is a **logic slot** — it talks to no physical device; it is a pure
MQTT service. Its job: on a single operator one-button command (`start` or
`stop`), it brings the whole station up (or down) **in a fixed, safe order**,
emitting commands to the individual device slots, pausing for settle delays, and
**waiting for explicit liveness/telemetry confirmation** from each dependent
device before proceeding. This matters because powering a radio transmitter
chain in the wrong order can damage hardware (e.g. energizing a PA before the
control logic is up, or removing power in a way that causes damaging electrical
inrush current), and because devices take tens of seconds to boot once their
mains is switched on.

- Its own slot address: `muehle/hf/power-seq` (defaults; configurable).
- It runs as a systemd service on the Raspberry Pi "shari" (`192.168.1.139`).
- It is a **sequencer role** in the station integration model; it is **one
  writer** of the slots it drives but does **not lock them** — any individual
  slot stays directly commandable for troubleshooting while the sequencer is
  idle.
- The sequences are **config-driven, not hard-coded**: the complete startup and
  shutdown step lists live in a TOML config file. The code only implements four
  generic step kinds and the runtime around them.

---

## 2. Upstream interface

There is **no physical device**. The only upstream is the MQTT broker:

- Transport: MQTT 3.1.1 over plain TCP (`tcp://192.168.1.50:1883`), username
  `hf`, password from an environment file (never in config or command line).
- Client ID: defaults to `<site>-<station>-<slot>` → `muehle-hf-power-seq`
  (configurable via `mqtt.client_id`).
- `CleanSession=false` (persistent session) + `AutoReconnect=true` (paho
  auto-reconnect).
- LWT (Last Will and Testament, a message the broker publishes on the client's
  behalf if it disconnects ungracefully): topic `<self>/status`, payload
  `offline`, QoS 1, retained.
- Initial connect is context-aware (a SIGTERM while the broker is unreachable
  interrupts the connect attempt rather than hanging until systemd SIGKILL) —
  provided by a shared helper `shared/mqtt.Connect` which races the paho connect
  token against the process context and disconnects on cancellation.
  (Implementation detail in Go's paho library; the *behavior contract* is:
  shutdown must resolve promptly even when the broker is unreachable.)
- Connection loss is detected two ways: paho's `OnConnectionLost` callback (sets
  an internal `broker-online=false` flag) and the absence of the connection for
  publishes. Every publish is bounded by a **10-second** wait timeout
  (`publishTimeout = 10 * time.Second`) so a dead broker surfaces as an error
  instead of stalling the sequencer forever.

---

## 3. MQTT presence

All own topics under the self base `muehle/hf/power-seq` (default):

### Published

| Topic | Retained | QoS | Payload | When |
|---|---|---|---|---|
| `muehle/hf/power-seq/meta` | yes | 1 | JSON birth certificate (below) | on every (re)connect |
| `muehle/hf/power-seq/state` | yes | 1 | `{ "ts", "phase", "step", "fault"? }` | on every phase/step/fault change, on runner start, and on every (re)connect (republished by the runner) |
| `muehle/hf/power-seq/status` | yes | 1 | literal string `online` or `offline` (not JSON) | `online` on every (re)connect; `offline` via LWT on ungraceful death, or published explicitly on clean shutdown |

**`/meta` payload** (all values derived from config; `controls`/`watches`
derived from the configured sequences):

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

- `controls` = absolute addresses of every slot that any `cmd` step targets
  (both sequences, deduplicated, **sorted alphabetically**).
- `watches` = every slot referenced by any step of either sequence
  (`cmd` + `wait_status` + `wait_state` targets), sorted alphabetically.
- `expose` is fixed regardless of sequence: three read-only fields
  (`phase` enum, `step` string, `fault` string). The operator command surface is
  the one-shot `/cmd`, which is not retained and therefore not exposed as a
  writable field. A separate consumer (hadiscovery) renders these into Home
  Assistant; powerseq itself publishes no Home-Assistant discovery topics (the
  `discovery_prefix` config key is carried but unused by this component).

**`/state` payload** — published on every internal transition:

```json
{ "ts": "2026-07-14T18:42:01Z", "phase": "starting", "step": "psu-on" }
{ "ts": "...", "phase": "running", "step": "" }
{ "ts": "...", "phase": "idle", "step": "", "fault": "psu-on: timeout" }
```

| Field | Type | Semantics |
|---|---|---|
| `ts` | string, RFC 3339, UTC | snapshot timestamp |
| `phase` | string enum | `idle` \| `starting` \| `running` \| `stopping` |
| `step` | string, **always present** (never omitted; empty `""` at idle/running and after completion/fault) | name of the current step while a sequence runs |
| `fault` | string, **omitted when empty** | `"<step>: <reason>"` for the step that aborted the last sequence; cleared on the next honored `start`/`stop` and on a completed shutdown |

The runner publishes an initial idle `/state` on process start (overwriting any
stale retained `/state` left by a previous crash — the sequencer never reads its
own `/state` back) and re-publishes it on broker (re)connect (so a broker that
lost retained messages gets a fresh copy).

**`/status`**: plain string `online`/`offline` (trimmed, case-insensitive on the
consumer side is not relevant here — powerseq is the publisher). `online` is
published on every (re)connect; `offline` on clean shutdown before
disconnecting (plus the LWT for the ungraceful case).

### Subscribed (derived from the configured sequences, never hard-coded)

The `/status` topic of **every** slot referenced by any `cmd`, `wait_status` or
`wait_state` step of either sequence (QoS 1), plus the `/state` topic of every
`wait_state` target (QoS 1), plus the sequencer's own `/cmd`. For the default
sequences this is exactly:

```
muehle/hf/ant-switch/status   (liveness gate for wait-controllers-online)
muehle/hf/pa-arm/status        (liveness gate)
muehle/hf/pa/state             (wait-pa-power-on: field "power" == "on")
muehle/hf/pa/status            (implicit liveness precondition for wait_state)
muehle/hf/power-seq/cmd        (operator one-button; QoS 0 — see §4)
muehle/hf/radio/status         (wait-radio-online gate)
muehle/hf/switch/status        (liveness gate)
muehle/power/master/status     (observability of a controlled slot)
muehle/power/psu-13v8/status   (observability of a controlled slot)
```

- `/status` messages: any payload whose trimmed, case-insensitive value equals
  `online` records the slot as online; anything else records `offline`.
  **Absence is distinct from offline**: a slot that has never published is
  "unseen" and a `wait_status` with `state="offline"` can never pass on it.
- `/state` messages: the whole JSON object is parsed and kept per slot. A
  **malformed** `/state` payload **deletes** the slot's previous snapshot (a
  good→bad transition must not leave a stale value poison the map) and is
  logged at warn level.
- Subscriptions are re-issued on every (re)connect (paho keeps the persistent
  session, but the code re-subscribes unconditionally).
- Message handlers never publish (they only update mutex-guarded maps and,
  for `/cmd`, do a non-blocking channel send) — this avoids the classic
  MQTT-client deadlock of publishing from inside a message callback.

### Commands it issues to other slots (published)

Each `cmd` step publishes to `<site>/<slot>/cmd`, QoS 1, retained by default:

```json
{ "action": "<action>", "value": "<value>" }
```

`value` is **always a JSON string** (the ecosystem's "value-key convention":
arguments ride under the key `value`, and booleans are carried as the strings
`"true"`/`"false"`). Exact commands of the default sequences (§5). A step may
set `retain = false` for a one-shot (non-steady-state) command; default is
`true` — the controlled slots keep the retained intent and re-apply it on their
own reconnects ("self-healing").

---

## 4. Command surface

powerseq accepts exactly one input: a JSON message on its own `/cmd` topic,
which **must not be published retained** by the sender, and which powerseq
subscribes at **QoS 0 deliberately**: with the persistent MQTT session
(`CleanSession=false`), a QoS-1 subscription would let the broker queue a
command issued while powerseq is offline and replay it on reconnect —
re-energizing the station, exactly the hazard the "own /cmd is not retained"
rule exists to prevent. QoS 0 means a command issued while powerseq is down is
simply lost, which is correct for a one-shot operator command.

Accepted payloads (only the `action` key is examined; extra keys are ignored;
a malformed JSON body is logged at warn and dropped):

```json
{ "action": "start" }
{ "action": "stop" }
```

Any other `action` is logged at warn and ignored.

**Busy guard (behavior contract).** Commands are honored only when the phase
permits; otherwise they are dropped with a warn log:

- `start` — honored only when `phase == "idle"` (a prior `fault` is cleared by
  running start again; re-running startup over an already-hot station is safe
  because every step is idempotent steady-state intent). Dropped when
  `phase` is `starting`, `running` or `stopping`.
- `stop` — honored when `phase == "running"` (normal shutdown), or when
  `phase == "idle"` **and** `fault != ""` (resume/finish an interrupted
  shutdown: a startup that faulted half-way left slots energized; `stop` from
  that state runs the full shutdown list as a teardown — the shutdown list has
  no waits in the default config, so it always completes). Dropped when
  `phase == "idle"` with no fault, or while `starting`/`stopping`.

There is **no abort command**: a command arriving while a sequence is in
progress is dropped; aborting an in-progress sequence is explicitly *not*
supported (docs call it "deferred").

The guard is enforced in two places (an implementation-detail worth preserving
in spirit): a fast-path check at command reception (drop before queueing) and
an authoritative atomic re-check when the runner picks the command up (the
phase transition `idle→starting` / `running→stopping` happens under a lock,
closing the race where two rapid commands both pass the fast-path check). The
runner-to-sequencer queue is a channel of capacity 4; a command arriving when
it is full is dropped with a warn log.

There are no CLI commands other than flags (§6); no other RPC surface.

---

## 5. Behavior & state machine

### State machine

States (published as `phase`):

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

- `idle` → `starting` on honored `start`; `starting` → `running` when the last
  startup step completes; `running` → `stopping` on honored `stop`; `stopping`
  → `idle` when the last shutdown step completes.
- Any step failure (wait timeout, broker disconnect mid-sequence, publish
  failure, process-shutdown interruption) → immediately `phase=idle` with
  `fault = "<step-name>: <reason>"` and `step=""`. **No rollback**: slots
  already driven keep whatever the last retained `/cmd` set them to. Recovery
  is by re-running `start` (idempotent re-drive) or `stop` (teardown from
  idle+fault).
- On sequence entry: `step` is reset to `""`, `fault` cleared; the new phase is
  published. On entering each step: `step` set to that step's name, `fault`
  cleared, `/state` published. On completion: `step=""`, `phase` = end phase,
  `/state` published.
- Exact fault reason strings (concatenated as `"<step>: <reason>"`):
  - `timeout` — a wait step exceeded its deadline.
  - `broker disconnected` — the MQTT connection dropped before a cmd step's
    publish or during a wait's poll loop.
  - `publish failed: <error>` — the cmd publish itself failed (including the
    10-second publish wait timeout).
  - `interrupted` — process shutdown (SIGINT/SIGTERM) during a delay or wait
    step, or at a step boundary.
- A single runner executes one sequence at a time; the runner also handles a
  coalesced "republish retained /state" request from the MQTT layer on
  reconnect.

### Step kinds (config-driven; the only behavior primitives)

Each step has a `name` (required, unique within its list) and a `kind`:

1. **`cmd`** — publish `{"action":…,"value":…}` (value always a string) to
   `<site>/<slot>/cmd`, retained unless `retain=false`. Requires the broker
   to be connected (checked immediately before publish); a broker-down or
   failed publish faults the sequence.
2. **`delay`** — sleep for a fixed duration: either a literal integer
   `duration_s` (>0, else a load-time config error) or a symbolic `duration =
   "network"` (resolves to `[timing].network_delay_s`, default 30 s) or
   `"stagger"` (resolves to `[timing].shutdown_stagger_s`, default 2 s).
   Unknown symbolic values are load-time errors. Delay steps are local and
   are NOT gated by the broker connection (they continue during an outage —
   only interruption by process shutdown cancels them).
3. **`wait_status`** — wait until **every** slot in `slots` (logical AND,
   non-empty entries only; an empty `""` entry is a load-time config error
   because it would make the AND unpassable) has `/status` == `state`
   (default `online`; `offline` is also allowed — it requires an *actual*
   `offline` payload, never mere absence).
4. **`wait_state`** — wait until the top-level JSON field `field` of slot
   `<site>/<slot>/state` compares equal (as strings) to `value`, **with an
   implicit precondition that the slot's `/status` is currently `online`**
   — a dead device whose LWT fired cannot satisfy the wait on a stale
   retained `/state`. `value` may be the empty string, in which case the
   wait passes when the field is absent, `null`, or `""` (waiting for a
   field to clear). Observed JSON values are coerced to strings for
   comparison: JSON booleans → `"true"`/`"false"`, JSON numbers → shortest
   decimal representation, strings → as-is, `null`/absent → `""`. A
   `wait_state` targeting the sequencer's own slot is a load-time config
   error (its `/state` is an output, not an input).

**Wait mechanics (exact).** A wait step polls its condition every
`poll_interval_ms` (default 200 ms). Each poll checks, in order: process
context cancelled → fault `interrupted`; broker offline → fault
`broker disconnected`; wall-clock past the deadline → fault `timeout`; then the
condition itself. An optional **hold (debounce)** window `hold_ms` requires the
condition to hold *continuously* for that long before the wait passes; if the
condition breaks mid-hold the window restarts. Omitted `hold_ms` →
`[timing].default_hold_ms` (default 0); an **explicit `hold_ms = 0` means
edge-triggered** (pass as soon as true) even when a default hold is configured.
The deadline is the per-step `timeout_s` if given, else `[timing].step_timeout_s`
(default 120 s); a given `timeout_s` must be > 0.

Because observations are cached retained snapshots, a wait whose condition is
already satisfied on entry passes on its first poll (fast path) — re-running
`start` over a fully-hot station converges almost immediately.

### Default startup sequence (shipped in `config.example.toml`, "model §7.1")

Executed in order on honored `start`:

| # | Step name | Kind | Detail | Precondition / confirmation |
|---|---|---|---|---|
| 1 | `master-on` | cmd | `muehle/power/master/cmd` `{"action":"set_power","value":"on"}` (retained) | none (first step); turns on the station master mains smart plug |
| 2 | `network-delay` | delay | `duration="network"` → 30 s default | lets the network (broker, WiFi of the plug-in devices) come up |
| 3 | `psu-on` | cmd | `muehle/power/psu-13v8/cmd` `{"action":"set_power","value":"on"}` (retained) | turns on the 13.8 V DC power supply |
| 4 | `wait-controllers-online` | wait_status | all of `muehle/hf/switch`, `muehle/hf/pa-arm`, `muehle/hf/ant-switch` `/status` == `online`; timeout 120 s default | confirms the devices booting off the 13.8 V supply are actually up |
| 5 | `trx-on` | cmd | `muehle/hf/switch/cmd` `{"action":"set_trx","value":"on"}` (retained) | powers the transceiver via its remote-on relay |
| 6 | `wait-radio-online` | wait_status | `muehle/hf/radio/status` == `online`; timeout 120 s default | confirms the radio's bridge is up after power-on |
| 7 | `pa-on` | cmd | `muehle/hf/switch/cmd` `{"action":"set_pa","value":"on"}` (retained) | engages the PA-enable relay |
| 8 | `wait-pa-power-on` | wait_state | `muehle/hf/pa/state` field `power` == `"on"` AND `muehle/hf/pa/status` == `online`; timeout 120 s default | confirms the PA actually reports powered; the liveness precondition prevents a dead PA from passing on a stale retained state |
| 9 | `pa-arm-enable` | cmd | `muehle/hf/pa-arm/cmd` `{"action":"set_enabled","value":"true"}` (retained) | arms the PA (permits RF output). Last step → `phase=running` |

Total worst-case time before `running` with all waits timing out at defaults is
≈ 30 s + 3 × 120 s; typical hot-idempotent re-run is < 1 s.

### Default shutdown sequence (on honored `stop`; reverse order with inrush staggers)

| # | Step name | Kind | Detail |
|---|---|---|---|
| 1 | `pa-arm-disable` | cmd | `muehle/hf/pa-arm/cmd` `{"action":"set_enabled","value":"false"}` (retained) |
| 2 | `stagger-1` | delay | `duration="stagger"` → 2 s default |
| 3 | `pa-off` | cmd | `muehle/hf/switch/cmd` `{"action":"set_pa","value":"off"}` (retained) |
| 4 | `stagger-2` | delay | 2 s |
| 5 | `trx-off` | cmd | `muehle/hf/switch/cmd` `{"action":"set_trx","value":"off"}` (retained) |
| 6 | `stagger-3` | delay | 2 s |
| 7 | `psu-off` | cmd | `muehle/power/psu-13v8/cmd` `{"action":"set_power","value":"off"}` (retained) |
| 8 | `stagger-4` | delay | 2 s |
| 9 | `master-off` | cmd | `muehle/power/master/cmd` `{"action":"set_power","value":"off"}` (retained) |

No waits in the default shutdown (deliberately: shutdown must make progress
even if devices are already dead). Completing step 9 → `phase=idle`, fault
cleared. The 2-second staggers exist to stagger the electrical inrush current
of switching inductive loads.

### Connection/reconnection behavior

- On process start: load + validate config (fatal exit 2 on error), connect to
  the broker (fatal exit 1 on failure), start the runner, which immediately
  publishes a retained `idle` `/state` (overwriting a stale retained `/state`
  from a crashed predecessor — this also guarantees that a service restart
  never spuriously replays a sequence: **on boot no `/cmd` is emitted until an
  operator `start` arrives**, even if the whole station is already hot).
- On (re)connect (paho auto-reconnect): publish `/status` `online` (retained),
  publish `/meta` (retained, fire-and-forget), flag broker-online, re-issue
  all subscriptions, and ask the runner to re-publish the retained `/state`.
- On connection loss: set broker-online=false; a running sequence faults at
  the next cmd step or wait poll (`broker disconnected`); delay steps are
  unaffected.
- On clean shutdown (SIGINT/SIGTERM): the runner finishes/interrupts; the
  client publishes `/status` `offline` (retained) and disconnects with a
  250-ms grace. A sequence interrupted by shutdown aborts to
  `phase=idle, fault="<step>: interrupted"`.
- A publish attempt that cannot complete within 10 s returns an error; a
  cmd-step publish error faults the sequence with `publish failed: …`; a
  `/state` publish error is only logged (never fatal).

---

## 6. Configuration

TOML file, default path `/etc/powerseq/config.toml`, overridable with the
`-config` flag. A missing file at the default path is not itself an error
(built-in transport/timing defaults are used) — but note that with no
`[[startup]]`/`[[shutdown]]` steps the subsequent validation **fails**
(exit 2): the sequence has no built-in default and must be configured. A
missing or malformed file at an explicitly-given `-config` path is fatal.
Second flag: `-log.level` (`debug|info|warn|error`), overriding the config's
`[log].level`.

| Key | Default | Meaning |
|---|---|---|
| `host` | `shari` | compute-node label published in `/meta` |
| `location` | `bauwagen` | physical label published in `/meta` |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | broker URI |
| `mqtt.client_id` | `""` → `muehle-hf-power-seq` | MQTT client ID |
| `mqtt.site` / `mqtt.station` / `mqtt.slot` | `muehle` / `hf` / `power-seq` | the sequencer's own slot address; also the prefix used to resolve the site-relative step slots to absolute topics |
| `mqtt.user` | `hf` | broker username |
| `mqtt.password` | `""` | **not used from TOML in production** — supplied via env (below) |
| `mqtt.discovery_prefix` | `homeassistant` | carried but unused by this component (discovery is rendered by a separate consumer) |
| `timing.network_delay_s` | `30` | seconds; value of `delay duration="network"` |
| `timing.step_timeout_s` | `120` | seconds; default wait deadline |
| `timing.shutdown_stagger_s` | `2` | seconds; value of `delay duration="stagger"` |
| `timing.poll_interval_ms` | `200` | ms; wait-poll cadence |
| `timing.default_hold_ms` | `0` | ms; default hold for waits that omit `hold_ms` |
| `log.level` | `info` | verbosity |
| `[[startup]]` / `[[shutdown]]` | none (required) | ordered step lists (schema in §5); at least one step each, load-time validated |

Load-time validation (all fatal, exit 2, fail-fast-never-at-runtime): site/
station/slot/broker non-empty; each step list non-empty; step names present
and unique within their list; `cmd` needs `slot`, `action`, `value` (all
non-empty); `wait_status` needs non-empty `slots`, each entry non-empty,
`state` ∈ {`online`,`offline`} if given, `timeout_s > 0` if given;
`wait_state` needs `slot` and `field` (`value` may be `""`), `timeout_s > 0`
if given; `delay` needs exactly one of `duration_s` (>0) or `duration`
(`network`|`stagger`); unknown `kind` rejected. Additionally (validated when
the sequencer is constructed, also before any MQTT activity): a `wait_state`
on the sequencer's own `<site>/<station>/<slot>` is rejected; an unknown
symbolic delay duration is rejected. Negative/zero timing values fall back
to their defaults at load.

**Env overrides** (used for the secret; overlay `[mqtt]` after TOML load):
`POWERSEQ_MQTT_BROKER`, `POWERSEQ_MQTT_CLIENT_ID`, `POWERSEQ_MQTT_USER`,
`POWERSEQ_MQTT_PASSWORD`, `POWERSEQ_MQTT_SITE`. The password is kept in a
0600 systemd EnvironmentFile (`/etc/powerseq/powerseq.env`), never in the
TOML, the unit file, or the process command line.

---

## 7. Deployment

- Target: Raspberry Pi "shari", `192.168.1.139`, SSH user `io`.
- `./deploy.sh`: cross-compiles (`GOOS=linux GOARCH=arm64 CGO_ENABLED=0`,
  `-trimpath -ldflags="-s -w"`) to `dist/powerseq-linux-arm64`, generates the
  systemd unit and **seed** config/env files locally, scps them to the Pi
  (`/tmp`), then installs remotely: creates a system user `powerseq`
  (no home, `nologin`), installs binary to `/opt/powerseq/powerseq` (0755),
  unit to `/etc/systemd/system/powerseq.service`, and — **seed-once** —
  installs `/etc/powerseq/config.toml` and `/etc/powerseq/powerseq.env`
  (0600, owned by `powerseq`) **only if they do not already exist**;
  existing files are never overwritten (the device owns its own settings).
  Then `daemon-reload`, `enable`, `restart`, print status.
- Environment knobs of deploy.sh (defaults): `SSH_HOST=192.168.1.139`,
  `SSH_USER=io`, `SERVICE_NAME=powerseq`, `SERVICE_USER=powerseq`,
  `INSTALL_DIR=/opt/powerseq`, `CONFIG_DIR=/etc/powerseq`,
  `HOST_NAME=shari`, `LOCATION=bauwagen`, `LOG_LEVEL=info`,
  `MQTT_BROKER=tcp://192.168.1.50:1883`, `MQTT_SITE=muehle`,
  `MQTT_STATION=hf`, `MQTT_SLOT=power-seq`, `MQTT_USER=hf`,
  `MQTT_PASSWORD=` (empty → a commented placeholder is seeded and the deploy
  prints a warning to set it on the device), `NETWORK_DELAY_S=30`,
  `STEP_TIMEOUT_S=120`, `SHUTDOWN_STAGGER_S=2`.
- systemd unit: `Type=simple`, `After/Wants=network-online.target`,
  `ExecStart=/opt/powerseq/powerseq -config /etc/powerseq/config.toml`,
  `EnvironmentFile=/etc/powerseq/powerseq.env`, `Restart=on-failure`,
  `RestartSec=5`, `User/Group=powerseq`,
  `ConfigurationDirectory=powerseq`, `StateDirectory=powerseq`.
  Hardening: `NoNewPrivileges=true`, `ProtectSystem=strict`, `ProtectHome=true`,
  `PrivateTmp=true`, `PrivateDevices=true`, `ProtectKernelTunables=true`,
  `ProtectKernelModules=true`, `ProtectControlGroups=true`,
  `RestrictAddressFamilies=AF_INET AF_INET6`, `RestrictNamespaces=true`,
  `LockPersonality=true`, `RestrictRealtime=true`, `RestrictSUIDSGID=true`,
  `RemoveIPC=true`, `CapabilityBoundingSet=` (empty),
  `AmbientCapabilities=` (empty), `ReadWritePaths=/var/lib/powerseq`,
  `MemoryMax=256M`, `TasksMax=64`, stdout/stderr to journal,
  `SyslogIdentifier=powerseq`, `WantedBy=multi-user.target`.
- Dependencies: MQTT broker (above) with the `hf` user account; the slots it
  drives must exist on the bus; Go module `powerseq` imports
  `codeberg.org/kgbvax/stationa/shared` (via `replace … => ../shared`) for
  the ctx-aware connect helper and slot-address helpers, and
  `github.com/BurntSushi/toml` + `github.com/eclipse/paho.mqtt.golang`.

---

## 8. Invariants & safety rules

**Behavior contract — must be preserved exactly:**

1. **Ordering**: startup order is master mains → network delay → PSU →
   controller-liveness wait → TRX power → radio-online wait → PA enable →
   PA-powered wait → PA arm. Shutdown is the exact reverse with 2-second
   staggers between power removals. PA arming (`set_enabled true`) is *always*
   the last startup action; PA disarm *always* the first shutdown action.
2. **No liveness, no progress**: a `wait_state` never passes unless the
   target's `/status` is *currently* `online` (prevents a dead device passing
   on a stale retained `/state`); a `wait_status` on `offline` requires an
   actual `offline` payload, never absence.
3. **No spurious energization**: own `/cmd` is never retained and subscribed
   at QoS 0 only, so a restart or reconnect can never replay an operator
   `start`. On boot the sequencer emits no commands until an explicit `start`
   arrives. On startup the sequencer republishes its own retained `/state`
   and `/meta` and `/status=online` (which overwrites the LWT `offline`).
4. **No rollback on fault**: a failed sequence leaves driven slots at their
   last retained intent; recovery is an explicit re-run (`start` clears the
   fault) or `stop` from `idle+fault` (teardown).
5. **One writer, no locking**: the sequencer never publishes anything that
   *claims* the controlled slots; while idle, any slot can be commanded
   directly. The retained steady-state `/cmd` intent lets each device
   self-heal to the last intent on its own reconnect.
6. **Broker outage cannot stall or corrupt**: cmd and wait steps fault (never
   wait indefinitely on stale data); all publishes are bounded by 10 s;
   delay steps are purely local.
7. **Single-sequence execution**: exactly one sequence runs at a time; a
   second command during a run is dropped, never queued to run after (queue
   capacity 4 only smooths delivery, and `begin()` re-checks the phase
   atomically).
8. **Config fail-fast**: any malformed, incomplete, or self-referential step
   list is a load-time fatal, never a runtime surprise.
9. Retained controlled `/cmd` uses the value-key convention: the argument is a
   JSON **string** under `"value"` (booleans as `"true"`/`"false"`), matching
   what the target bridges parse.

---

## 9. Known defects & fragilities

- **deploy.sh seed config omits the sequences** (the most significant
  discrepancy found): the TOML that `deploy.sh` seeds on a fresh device
  contains `host`, `location`, `[mqtt]`, `[timing]`, `[log]` — but **no
  `[[startup]]`/`[[shutdown]]` step lists**, which `Validate()` requires. A
  first-deploy service therefore crashes at startup with "at least one
  [[startup]] step is required" and, with `Restart=on-failure`/`RestartSec=5`,
  crash-loops until an operator hand-adds the step lists from
  `config.example.toml`. The example file has them; the seed generator does
  not copy them.
- **No abort of an in-progress sequence**: a `stop` issued during `starting`,
  or a `start` during `stopping`, is silently dropped. An operator who
  mistypes a start and wants to cancel must wait out the current run (up to
  ~30 s + 3 × 120 s worst case with default timeouts) or fault it indirectly.
- **`wait_state` uses only retained-snapshot caching, no freshness bound**:
  the liveness precondition checks current `/status`, but the `/state`
  snapshot itself can be arbitrarily old (a device that is online per its
  bridge LWT but whose device link died without a `/status` change would
  satisfy `wait-pa-power-on` on a stale `state.power == "on"`). The
  two-layer-liveness convention (bridge `/status` AND device-online inside
  `/state`) is not enforced here for the wait payload.
- **`stop` from `idle` without a fault is dropped** — even if the station is
  fully hot because it was started by hand or by a previous process
  lifetime. There is no way to shut the station down through the sequencer
  unless the sequencer itself is in `running` (or `idle+fault`).
- **The fast-path guard logs at warn for normal traffic**: an operator's
  legitimate double-press of `start` produces warn-level noise ("cmd start
  ignored: phase=running").
- **Publishes in `OnConnect` are fire-and-forget** (`/status`, `/meta`): a
  failure there is invisible (no log, no retry beyond paho's own resend);
  only `/state` republish goes through the bounded publisher on the runner.
- **Subscriptions with a persistent session**: paho with `CleanSession=false`
  accumulates subscriptions in the broker session; changing the config (and
  thus the derived subscription set) re-subscribes the new set on top of the
  old session — stale subscriptions for slots no longer referenced are never
  explicitly unsubscribed, so the broker keeps delivering (and powerseq
  discarding) their messages until the session is reset.
- **No metric/heartbeat of its own beyond `/status`**: a wedged-but-connected
  process (e.g. runner deadlock) would not be detected by the bus; nothing
  watchdogs the runner goroutine.
- Docs (`docs/powerseq-mqtt-api.md` §5) say `stop` is honored only from
  `phase=running`; the code (and tests) also honor `stop` from
  `phase=idle` **with** a fault. The code is authoritative (the doc lags).
- README says "~30 s (network)" etc.; actual values are config defaults
  (30 s network, 120 s step timeout, 2 s stagger, 200 ms poll) and fully
  changeable — the sequences and timings live in TOML, not code.

---

## 10. Re-implementation notes

**Preserve verbatim (behavior contract / wire format):**

- All exact topic strings: `muehle/hf/power-seq/{meta,state,status,cmd}` and
  the controlled/waited topics in §3, including the derived-subscription rule
  (status of every referenced slot, state of every wait_state slot, own cmd).
- Payload shapes: `/meta` structure (including `schema:"1.0"`, `role:"sequencer"`,
  `link:"none"`, sorted `controls`/`watches`, fixed `expose` block);
  `/state` `{ts, phase, step, fault?}` with `step` always present and `fault`
  omitted-when-empty; `/status` literal strings; emitted `/cmd`
  `{"action":…,"value":"<string>"}` with retained=true default.
- The exact default startup/shutdown step lists, names, commands, and order
  (§5) — these names appear in `state.step` and fault strings and are
  user-visible.
- The four step kinds and their exact semantics: wait poll cadence 200 ms,
  hold/debounce semantics (omitted vs explicit-0), deadline semantics
  (default 120 s, per-step override, >0), offline-needs-actual-payload,
  wait_state liveness precondition + string coercion rules + clear-on-empty
  + malformed-state-drops-stale, delay symbolic refs (network 30 s /
  stagger 2 s), exactly-one-of duration keys.
- Busy-guard truth table (start: idle only; stop: running or idle+fault),
  two-phase check semantics (reception check + atomic re-check at execution),
  drop-and-log on violation, no abort command, one-sequence-at-a-time.
- Fault taxonomy and string format `"<step>: <reason>"` with reasons
  `timeout`, `broker disconnected`, `publish failed: …`, `interrupted`;
  no-rollback policy; fault cleared by the next honored command.
- Retention/QoS policy: own `/cmd` QoS 0 + not retained; observations QoS 1 +
  persistent session; controlled `/cmd` retained QoS 1; LWT on own `/status`;
  10-second publish bound; republish of `/meta`, `/status=online`, `/state` on
  every reconnect; publish an initial idle `/state` at process start; never
  emit commands at boot.
- Config surface (keys, defaults, validation rules, env overrides, secrets in
  an EnvironmentFile), and the systemd deployment contract (seed-once,
  restart behavior).

**Free to change (implementation detail):**

- Go, paho, TOML library, slog, channel-and-goroutine structure, mutexes —
  any concurrency model that keeps the same observable ordering and the
  "handlers never block on publish" guarantee.
- The polling implementation of waits (an event-driven condition-variable
  approach is fine as long as timings/semantics — poll cadence bound, hold
  window, deadline — are equivalent; the 200 ms poll is a cadence *ceiling*
  in spirit, but note faster reaction changes no published behavior).
- Sorted order of `controls`/`watches` arrays is alphabetical in this
  implementation — a reimplementation should keep sorted output to stay
  byte-comparable, but consumers treat them as sets.
- The single-runner-goroutine design and the coalesced republish channel.
- The `shared/` Go helpers (ctx-aware connect etc.) — the *behavior* (prompt
  shutdown during connect, bounded publishes) is the contract, not the code.
- Log format (stderr text handler) and exact log strings.

**Must fix / decide in the reimplementation (known-defect items):**

- Ship the full seed config including the `[[startup]]`/`[[shutdown]]` step
  lists (or a built-in default sequence) so a fresh deploy boots green.
- Consider whether `stop` from any phase (an explicit abort/cancel) should be
  supported — the current drop behavior is documented intent, but a PRD team
  may want to change it; if changed, it is a deliberate contract change, not a
  bug fix.
- Consider a freshness bound or device-online coupling for `wait_state` (the
  station's two-layer-liveness convention) rather than trusting
  `/status` + last cached `/state`.