# Research spec: antennaselect — HF antenna-selection reconciler

Source analyzed: `/Users/ingomar.otter/dev/stationa/antennaselect` (Go), plus the shared
modules it imports (`shared/mqtt`, `shared/schema`), the project `CLAUDE.md`,
`config.example.toml`, `deploy.sh`, `docs/antenna-select-mqtt-api.md`, all unit tests, and
the station memory notes on observed live behavior (2026-08-28). Code is truth; this
document flags where the docs and code disagree.

Audience note — every amateur-radio and station-internal term is defined at first use.

---

## 1. Purpose & role

**antennaselect** is a small, stateless, always-running decision service ("reconciler") for
an amateur-radio ("ham") station. An amateur-radio station is a setup of radio transceivers,
antennas and amplifiers used for two-way radio communication on short-wave ("HF", roughly
3–30 MHz, plus the 6 m band) frequencies. This station ("Mühle") has several physical
antennas, all wired to a single 1:6 relay antenna switch (only one antenna is connected to
the radio at a time; the switch's "off" position shorts all antenna feeds to electrical
ground to protect against lightning).

The reconciler has **no device and no I/O of its own** — it is a "logic slot": a pure
policy engine that:

1. watches the radio's current frequency band (the wavelength name of the operating
   frequency, e.g. "20m" for the 14 MHz amateur band) and TX (transmit) state over MQTT
   (a lightweight publish/subscribe messaging protocol; messages go to a broker, clients
   subscribe to hierarchical topic strings like `muehle/hf/radio/state`);
2. decides which of the station's physical antennas should be connected right now, using a
   fixed three-tier priority ladder (idle/ground → operator hold → automatic band policy);
3. commands the antenna-switch bridge ("ant-switch", a separate service driving the relay
   hardware) to move to that antenna;
4. additionally drives three "follow" bindings: the rotatable-beam controller tracks the
   radio frequency ("band-follow"), the power amplifier ("PA") is pre-positioned to the
   radio's band, and the antenna tuner ("ATU", an impedance-matching device needed when an
   antenna is not resonant on the operating band) is switched in/out of the RF path; and
5. implements **auto-grounding**: after 30 minutes with no radio activity it selects the
   switch's "off" (grounded) position, so an unattended station cannot leave an antenna
   hanging live (lightning / walk-away protection).

It runs as a systemd service on the host `shari` (a Raspberry Pi at 192.168.1.139),
talking only to the MQTT broker at 192.168.1.50:1883. Its slot address on the bus is
`muehle/hf/antenna-select`.

**Behavior contract:** everything in sections 3–8 describes behavior a reimplementation must
reproduce exactly. The Go language, the paho MQTT client, the internal package layout and
the jobs-queue mechanics are implementation detail — except that the property those
mechanics exist to guarantee (a message handler must never block on publish) is itself a
contract (see §5.4).

---

## 2. Upstream interface

There is no physical device. The upstream interfaces are:

- **MQTT broker** `tcp://192.168.1.50:1883` (configurable), user `hf`, password kept only
  in the on-device config file. MQTT concepts used: **retained** messages (the broker
  stores the last message per topic and replays it to every new subscriber), **QoS 1**
  (at-least-once delivery), and **LWT** ("last will and testament" — the broker publishes a
  preset message on the topic when a client drops unexpectedly).
- **Sibling slots** it consumes (see §3 for payloads):
  - `radio` (the FLEX-8400 transceiver bridge, "flexbridge") — publishes a retained JSON
    state snapshot on `muehle/hf/radio/state` **only when its content changes** (no periodic
    heartbeat — this is a live-observed property of flexbridge, and it drives several
    defects in §9), and `online`/`offline` on `muehle/hf/radio/status` (its LWT).
  - `ant-switch` (the ESPHome relay-board bridge) — retained state on
    `muehle/hf/ant-switch/state`; accepts `select` commands.
- **Sibling slots it commands**: `ant-ctrl` (ultrabridge, the Ultrabeam RCU-06 rotatable
  beam controller), `pa` (ACOM 1200S amplifier bridge), `tuner` (ATR-1000 ATU bridge).
  Slot names of these three come from config, not code.

Connection loss detection: paho `SetAutoReconnect(true)` plus a connection-lost handler
that only logs. A **clean session** (`CleanSession=true`) is deliberately used: on every
(re)connect the broker drops the prior session and replays all retained input topics,
which is how the stateless reconciler re-seeds its inputs and re-emits follow intents.
(A persistent session would *not* replay retained messages for existing subscriptions and
the reconciler would wake up blind — this is called out in code as load-bearing.)

---

## 3. MQTT presence

Topic base: `muehle/hf/antenna-select` (config `[mqtt]` site/station/slot). All publishes
are QoS 1. Client ID defaults to `<site>-<station>-<slot>` = `muehle-hf-antenna-select`
(configurable via `mqtt.client_id`).

### 3.1 Published topics

**`muehle/hf/antenna-select/status`** — plain string `online` / `offline`, retained, QoS 1.
`online` is published in the on-connect handler; `offline` is published explicitly on clean
shutdown and is registered as the paho **LWT** (QoS 1, retained) so an abrupt death still
flips it. No other heartbeat exists: the reconciler's own state has no periodic publish.

**`muehle/hf/antenna-select/meta`** — retained JSON "birth certificate", published on every
connect. Exact shape (with the Mühle deploy config: band-follow on, PA follow on, tuner
follow on):

```json
{
  "schema": "1.0",
  "role": "reconciler",
  "link": "none",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "controls": "ant-switch",
    "follows": { "ultrabeam": "ant-ctrl", "pa": "pa", "tuner": "tuner" },
    "ladder": ["idle", "operator", "auto"]
  },
  "expose": {
    "device": { "name": "Antenna selector" },
    "fields": [
      { "key": "source", "name": "Source", "type": "enum", "options_ref": "ladder" },
      { "key": "target", "name": "Target", "type": "string" },
      { "key": "mode",   "name": "Mode",   "type": "string" }
    ]
  }
}
```

- `follows` keys: `band_follow.resource` → `band_follow.slot`; if `pa_follow.enabled`,
  `pa_follow.slot` → `pa_follow.slot`; if `tuner_follow.enabled`, `tuner_follow.slot` →
  `tuner_follow.slot`. The whole `follows` key is omitted when all three bindings are
  disabled.
- `expose` is a consumer-neutral field-surface description (a separate Home-Assistant
  discovery consumer renders UI from it); all three `/state` fields are read-only sensors.

**`muehle/hf/antenna-select/state`** — retained JSON snapshot, published **only when the
resolved decision changes** (deduped; no periodic republication):

```json
{ "ts": "2026-07-06T12:34:56Z", "mode": "auto", "target": "port4", "source": "auto" }
```

| Field | Type | Values / semantics |
|---|---|---|
| `ts` | string | RFC 3339, UTC, time of this decision |
| `mode` | string | `auto` \| `manual`. Derived: `manual` iff an operator hold is active, else `auto`. There is no separate mode switch. |
| `target` | string | the port the reconciler currently wants: `off` or a wiring-map port key (`port1`..`port6`). Empty string is the internal "hold last" marker and IS published here: it is the first publish after any process start with unresolved inputs, and it reappears on every transition into hold-last (radio offline, empty band) — update() publishes the triple whenever it changes, including the empty target. |
| `target` ground truth | — | this is *intent*; the switch's actual position lives in `ant-switch/state.selected` |
| `source` | string | *why*: `idle` \| `operator` \| `auto` |

**`muehle/hf/ant-switch/cmd`** (the switch's command topic, not the reconciler's) —
retained, QoS 1, exact payload:

```json
{ "select": "port4" }
```

Note this uses the key `select` (the ant-switch bridge's own contract), **not** the
station-wide `{"action": ..., "value": ...}` convention used for the PA/tuner commands
below. A reimplementation must match each target bridge's exact contract.

**`muehle/hf/ant-ctrl/cmd`** (topic = `<band_follow.slot>/cmd`, `ant-ctrl` at Mühle) —
band-follow intent, retained, QoS 1, exact payload:

```json
{ "action": "frequency", "freq_hz": 14175000 }
```

`freq_hz` is an integer in **Hz** (station-wide convention: never kHz/MHz on the bus). The
value rides under `freq_hz` (ultrabridge's contract), not `value`.

**`muehle/hf/pa/cmd`** (topic = `<pa_follow.slot>/cmd`) — PA band pre-position,
**NOT retained**, QoS 1, exact payload:

```json
{ "action": "set_band", "value": "20m" }
```

Not-retained is deliberate (the PA bridge subscribes un-retained); self-healing comes from
the reconciler re-resolving on the retained `radio/state` replay at its own reconnect, not
from a retained command.

**`muehle/hf/tuner/cmd`** (topic = `<tuner_follow.slot>/cmd`) — ATU in-line/bypass,
**NOT retained**, QoS 1, exact payload:

```json
{ "action": "set_inline", "value": true }
```

`value` is a JSON boolean: `true` = ATU in the RF path, `false` = bypassed.

### 3.2 Subscribed topics (all QoS 1, re-established in the on-connect handler on every
(re)connect; retained messages replay into each subscription at connect)

| Topic | Payload | Fields used |
|---|---|---|
| `muehle/hf/radio/state` | JSON (retained by flexbridge) | `band` (string, canonical band name; `""` during radio-link reconnect), `freq_hz` (integer Hz), `tx` (`"rx"` \| `"tx"`), `device_online` (bool — radio-link liveness) |
| `muehle/hf/radio/status` | plain string `online`/`offline` (LWT) | bridge-process liveness |
| `muehle/hf/ant-switch/state` | JSON (retained) | `selected` (string: `off` or a port key), `settled` (bool — received but **not used**; see §9) |
| `muehle/hf/antenna-select/cmd` | JSON (retained) | `request` (string) — operator hold/release, see §4 |

Malformed JSON on any input topic is logged and dropped; nothing crashes or disconnects.

Publish cadence summary: nothing is periodic except the internal 5 s idle check (§5.3),
which only publishes if the decision changes. Every emitted publish is deduped (§5.5).

---

## 4. Command surface

The component accepts exactly one command topic: `muehle/hf/antenna-select/cmd`
(retained so a hold survives a reconciler restart via retained replay).

Payload: `{"request": "<value>"}`. Behavior by value:

| `request` value | Effect |
|---|---|
| `port1`..`port6` or `off` | engage an **operator hold** (ladder tier 2) on that switch position; `mode` becomes `manual` |
| `auto` | release the hold; return to band-policy selection (tier 3) |
| empty/absent JSON payload (e.g. a cleared retained message) | treated as release |
| any other string | **still engages a hold** with that literal string as target (unvalidated — see §9) |

Side effects of a hold: the reconciler commands the switch to that port (subject to the
TX cold-switch deferral, §5.2) and ignores the band policy until `auto` is received or the
station goes idle. A hold does **not** count as activity for the idle timer (§5.3) and does
**not** override an idle grounding (§5.6).

There is no read/ack protocol on the command; the reconciler's observable acknowledgment
is the retained `antenna-select/state` (`source: "operator"`, `mode: "manual"`) and the
subsequent `ant-switch/cmd` emit.

---

## 5. Behavior & state machine

### 5.1 Startup sequence

1. Load config from `/etc/antenna-select/config.toml` (flag `-config` overrides). A missing
   *default-path* file is tolerable (run on built-in defaults + flags); a missing
   *explicitly requested* file, or any malformed file, is fatal (exit).
2. `-broker` flag overrides `[mqtt].broker`. `Validate()` runs (see §6); failure is fatal.
3. `lastActivity = now` (the idle clock starts fresh at process start — see §9 fragility 5).
4. Two background goroutines start: a single-worker **jobs queue** (buffered channel,
   capacity 256) and the **idle loop** (5 s ticker).
5. Connect to the broker (context-aware connect; SIGTERM during a broker outage interrupts
   it). On connect, in order: publish `status`=`online` (retained), publish `meta`
   (retained), subscribe to the four input topics (QoS 1).
6. Retained messages replay into the subscriptions; each one feeds the reconciler. The
   first resolution (any input) publishes the first `antenna-select/state`.

### 5.2 Decision ladder (evaluated on every input change — the reconciler is stateless;
every decision is a pure function of config + latest inputs)

```
Tier 1  idle:     station activity == "inactive"   → target = "off", source = "idle"
Tier 2  operator: operator hold active             → target = request, source = "operator"
Tier 3  auto:     band policy(radio.band)          → target = port, source = "auto"
```

Exact rules:

- **Tier 1 wins over everything, including an operator hold** (walk-away safety; a
  deliberate, documented decision). Unknown activity (`""`) is treated as `active`.
- **Tier 2**: a hold is active iff `request` is non-empty and not `"auto"`.
- **Tier 3**: resolved only if the radio is "online" (two-layer gate, §5.6) **and**
  `band` is non-empty. Resolution: scan `band_policy.bands` (resource names in **sorted
  order** for determinism) for the band; the resource maps to a port via the inverted
  wiring map. A **known-but-unmatched** band (e.g. `160m`, or the out-of-band marker
  `gen`) maps to `band_policy.fallback` (at Mühle: `fan-dipole` → `port6`).
- **Empty band `""` holds the last selection** (target = empty, no command emitted) — it
  is flexbridge's transient "reconnecting, no slice reported yet" state, not a tuning
  intent. Resolving it to the fallback chattered the antenna on every reconnect cycle;
  this is a regression-tested behavior. Radio offline likewise yields empty target (hold
  last) — but a **tier-2 operator hold still asserts while the radio is offline**.
- `mode` is `manual` whenever a hold is active, even when tier 1 wins the target.

Mühle wiring map and band policy (as deployed; see §9 for the port-number discrepancy):

| Wiring-map key | Resource | Physical antenna |
|---|---|---|
| `port1` | `dummy-load` | dummy load (a heat-dissipating resistor used for testing — radiates nothing) |
| `port4` (example config / deploy seed; `port3` in tests and the repo's top-level docs) | `ultrabeam` | Ultrabeam rotatable beam (a directional antenna on a mast, tuned by the `ant-ctrl` slot) |
| `port6` | `fan-dipole` | 80/40 m fan dipole (a fixed wire antenna resonant on 40 m and 80 m) |
| `off` | `grounded` | not a port — the switch position that shorts antenna feeds to ground |

Band policy: `ultrabeam` serves `["6m","10m","12m","15m","17m","20m"]`; `fan-dipole` serves
`["30m","40m","60m","80m"]`; `fallback = "fan-dipole"` (so 160m, `gen`, and any other
unmatched band land on the fan dipole — non-resonant there, hence the tuner follow, §5.7).

### 5.3 Idle grounding (walk-away safety)

- **Activity** is inferred, never operator-set: a `radio/state` message whose `freq_hz`
  differs from the last-seen `freq_hz`, **or** whose `tx == "tx"`, sets activity =
  `active` and resets the idle clock.
- A 5-second ticker (`idleCheckInterval = 5 * time.Second`) re-checks on the worker: if
  `now - lastActivity >= idle.timeout_minutes` (default 30 minutes), activity is set to
  `inactive` → tier 1 → target `off` → `ant-switch/cmd {"select":"off"}` (retained).
  Grounding therefore lands within ~5 s after the timeout expires.
- **Re-activation** requires a *new* radio/state message with a *changed* `freq_hz` or
  `tx=="tx"` — a replay of the same state does not count. Nothing else (operator command,
  reconciler restart logic, switch state) re-arms activity. The full recovery path and its
  holes are in §9.

### 5.4 Threading / the no-blocking-publish rule (critical, live-learned)

Every MQTT message handler runs on the client library's dispatch goroutine, which must
never block — in particular it must never synchronously publish and wait for the broker
ack. (This deadlocked the station's hadiscovery service live; antennaselect had the same
pattern and would have died on deploy. paho delivers handlers inline on its
`matchAndDispatch` goroutine by default.)

Current code: each handler only parses the payload and **enqueues a closure** onto a
bounded jobs channel (capacity 256); a single worker goroutine drains it and does all
mutate/reconcile/publish work, publishing synchronously (`token.Wait()`), which is safe
there. This serializes all decisions (ordering matters to the dedup logic). If the queue
is full, **the job is dropped** (never block the dispatch goroutine) — the next input
message re-arms. A regression test (`TestOnRadioStateDefersReconcile`) enforces that the
handler returns without waiting for a publish. A reimplementation in any stack must
preserve the property, not necessarily the mechanism.

### 5.5 Emission rules and dedup (all on the worker)

For each resolution:

- **`antenna-select/state`**: published iff the decision (mode/target/source triple)
  changed since the last published one (or it is the first).
- **`ant-switch/cmd {"select": p}`**: emitted iff the reconciler wants a port change *now*
  — i.e. target is known, target differs from `ant-switch/state.selected`, and the radio
  is not transmitting. Additional dedup: while the switch's `selected` is still unknown
  (before the first `ant-switch/state` arrives), only one command is sent per target
  (`lastSelect`); once `selected` is known, the command is re-issued whenever the switch
  reports a position different from the target — so a **manual override of the switch is
  re-asserted on the next input change** (regression-tested: `TestReassertionAfterManualOverride`).
  The same-band-frequency-change case emits no command (regression-tested).
- **Cold-switch TX deferral**: if `radio/state.tx == "tx"` when a port change is wanted,
  the select is **withheld** (`DeferredForTX`), logged once per resolution
  (`port change to "X" deferred: radio is transmitting (cold-switch)`), and fires when a
  later input change finds the radio back in `rx`. There is **no timeout** on the
  deferral (see §9 fragility 4). Rationale: the antenna switch is not rated for
  hot-switching; the reconciler owns ordering, while hard enforcement is a hardware
  interlock chain (`radio → rx-loop-ctrl → ant-ctrl → pa`) — the reconciler is never
  part of the enforcement path.
- **Band-follow** (`ant-ctrl/cmd {"action":"frequency","freq_hz":N}`): emitted iff the
  resolved target equals the followed resource's port AND the radio is online AND
  `freq_hz > 0`, deduped against the last frequency pushed.
- **PA band-follow** (`pa/cmd {"action":"set_band","value":B}`): emitted iff
  `pa_follow.enabled` AND radio online AND band non-empty — **not** gated on antenna
  selection, activity state, or TX (the PA is always in the RF path; PA hot-switch
  protection is hardware). Deduped against the last band pushed. Not retained.
- **Tuner follow** (`tuner/cmd {"action":"set_inline","value":b}`): emitted iff
  `tuner_follow.enabled` AND the tuner resource's port is configured AND radio online AND
  band non-empty. Value: `true` iff resolved target == tuner's resource port AND band is
  in `atu_bands`; else `false` (so leaving a non-resonant band, or selecting another
  antenna, drops the ATU out of line — including when idle-grounded, since target `off`
  ≠ tuner port). Deduped against the last value pushed. Not retained. Because the port
  select itself defers during TX, the ATU is not re-keyed mid-transmission — but note
  the `set_inline` value is computed from the *resolved target*, not the switch's actual
  position, so during a deferred-for-TX window the ATU intent may precede the actual
  switch move.

### 5.6 Two-layer liveness gate (critical, live-learned)

Radio-derived fields (`band`, `freq_hz`, `tx`) may only be trusted when **both** are up:

- `radio/status == "online"` — the flexbridge *process* is alive (broker LWT layer). This
  stays `online` while the bridge is up but the radio link is down.
- `radio/state.device_online == true` — the FLEX radio *link* is up (handshake done).

`RadioOnline = (radioBridgeOnline AND radioDeviceOnline)`, recomputed on either message
under the lock; the gate survives retained-replay in any arrival order (regression-tested
in all four combinations plus both orderings). Earlier code keyed on `/status` alone and
**chattered**: a bridge-up-but-radio-link-down window carries a stale/empty-band
`radio/state`, and the reconciler flapped the antenna to the fallback and back. This gate
and the empty-band-hold rule (§5.2) are the two halves of that fix. If either layer drops
after a selection, the reconciler holds the last selection (no chatter, no grounding from
liveness alone).

### 5.7 Error paths

| Event | Behavior |
|---|---|
| Broker connect fails at startup | process exits with error; systemd `Restart=on-failure`, `RestartSec=5` |
| Broker drops mid-run | paho AutoReconnect; connection-lost handler only logs; on reconnect: `status` online, `meta`, re-subscribe, retained replay re-seeds inputs and re-emits follow intents (clean session guarantees the replay) |
| Malformed JSON input | logged (`[mqtt] bad radio/state: …` / `bad ant-switch/state: …` / `bad operator cmd: …`), message dropped, previous inputs retained |
| Publish failure (broker briefly down) | logged (`[mqtt] publish failed topic=… err=…`), not retried; dedup state (`lastSelect`, `lastFollowFreq`, `lastPaBand`, `lastTunerInline`) has already advanced, so an identical re-resolve will not re-emit — recovery relies on a subsequent input change or the reconnect retained replay |
| Subscribe failure | logged, service continues (retained replay for that topic is lost until reconnect) |
| Jobs queue full (256) | incoming input job dropped (never blocks the MQTT dispatch goroutine); state is lost until the next message on that topic |
| SIGINT/SIGTERM | context cancels (connect is ctx-aware so even a broker outage during shutdown resolves promptly); worker stops first; if the connection is open, `status`=`offline` (retained) is published, then disconnect (250 ms quiesce) |
| Switch never confirms | unknown `selected` ⇒ at most one command per target (`lastSelect`); known `selected` mismatching target ⇒ command re-issued on every input change (no explicit retry timer — retries are input-driven) |

### 5.8 Grounding state machine, exactly

States (implicit, in the `StationActivity` input):

- **ACTIVE** (`StationActivity` = `active` or unknown): ladder tiers 2/3 may select any
  port.
- **GROUNDED-IDLE** (`StationActivity` = `inactive`, set only by the 5 s idle check after
  30 min without a freq change or a TX): target forced to `off`, source `idle`, overriding
  operator holds. The switch's `off` position shorts the open antenna ports to ground.

Transitions:

- ACTIVE → GROUNDED-IDLE: `now - lastActivity ≥ 30 min` (config `idle.timeout_minutes`),
  observed within 5 s. Emits `{"select":"off"}` (unless TX at that instant — deferred).
- GROUNDED-IDLE → ACTIVE: the **only** re-arm is a `radio/state` message with
  `freq_hz` ≠ last-seen `freq_hz` **or** `tx == "tx"`. Then tier 3 resolves the band port
  and a select is emitted — but if the re-arming message was the TX itself, the select is
  TX-deferred and only fires when the radio reports `rx` again (see §9 fragility 2).
- Process restart: `lastActivity` is reborn as `now` and `RadioFreqHz` reborn as 0, so the
  retained `radio/state` replay always looks like a freq change ⇒ **a restart always
  re-arms ACTIVE** and resets the idle clock (see §9 fragility 5).

---

## 6. Configuration

Single TOML file, `/etc/antenna-select/config.toml`, mode 0600 (it contains the MQTT
password), owned by the service user. Precedence: CLI flag > config file > built-in
default. `[priority]` in the example file is documentation only — the ladder order is
fixed in code and not configurable.

| Key | Default | Meaning |
|---|---|---|
| `location` | *(required)* | building/site fact published in `/meta` (deploy seed: `bauwagen`) |
| `host` | *(required)* | compute node published in `/meta` (deploy seed: `shari`) |
| `mqtt.broker` | *(required in effect)* | broker URL, e.g. `tcp://192.168.1.50:1883`; `-broker` flag overrides |
| `mqtt.client_id` | `<site>-<station>-<slot>` | MQTT client ID |
| `mqtt.site` / `mqtt.station` | *(required)* | topic prefix parts (`muehle` / `hf`) |
| `mqtt.slot` | `antenna-select` | this slot's topic segment |
| `mqtt.user` | `hf` | broker username |
| `mqtt.password` | *(secret; empty in repo examples)* | broker password — only ever in the 0600 config, never on a command line |
| `wiring_map` | *(required, no default)* | table of switch port key → resource name; must include `off = "grounded"`; port keys are the switch's own names (`port1`..`port6`) |
| `band_policy.bands` | *(required, no default)* | resource → list of band names it serves |
| `band_policy.fallback` | *(required)* | resource for any band not listed (incl. 160m, `gen`) |
| `band_follow.resource` | `""` (disabled) | wiring-map resource whose controller tracks the radio frequency |
| `band_follow.slot` | `ant-ctrl` | controller slot receiving `frequency` intents |
| `pa_follow.enabled` | `false` (deploy seed: `true`) | enable PA `set_band` follow |
| `pa_follow.slot` | `pa` | PA slot |
| `tuner_follow.enabled` | `false` (deploy seed: `true`) | enable ATU `set_inline` follow |
| `tuner_follow.slot` | `tuner` | tuner slot |
| `tuner_follow.resource` | *(required if enabled)* | wiring-map resource the ATU serves (`fan-dipole`) |
| `tuner_follow.atu_bands` | *(required if enabled)* | non-resonant bands needing the ATU in line (`["30m","60m","80m","160m"]`) |
| `idle.timeout_minutes` | `30` | walk-away grounding timeout, integer minutes, must be positive |

`Validate()` rejects (fatal at startup): missing `mqtt.site`/`mqtt.station`; missing
`location`/`host`; any band-policy resource (or fallback) not present in the wiring map;
missing fallback; any band mapped to two resources wired to *different* ports (same-port
aliases are allowed); `band_follow.resource` not in the wiring map or missing slot;
`pa_follow.enabled` without a slot; `tuner_follow.enabled` without slot/resource, or
resource not in the wiring map; non-positive idle timeout. The `off` wiring-map entry is
excluded from resource→port inversion (it is a position, not a routable resource).

Secret handling: password lives only in the 0600 TOML; systemd unit has no secrets; the
deploy script (below) can seed it on-device without it ever leaving the Pi.

---

## 7. Deployment

- Target host: `shari` (Raspberry Pi, `192.168.1.139`, SSH user `io`). Runs as a
  dedicated system user `antenna-select` (no login, no home), created on first deploy.
- `deploy.sh` (run from the project dir on a workstation):
  1. Cross-compiles `linux/arm64`, `CGO_ENABLED=0`, `-trimpath -ldflags "-s -w"`, output
     `dist/antennaselect-linux-arm64`.
  2. Generates the seed config (umask 077 locally) from env vars + the baked Mühle wiring
     map/band policy, and a systemd unit.
  3. `scp`s binary, unit, and seed to `/tmp` on the target.
  4. Remote install: creates the service user; `install -d /etc/antenna-select` (0755,
     service-user-owned); **seed-once** — installs the config only if none exists, 0600;
     if the seed's password is empty, pulls the shared `hf` MQTT password on-device from
     the first readable of `/etc/acom1200s-pa-bridge/acom1200s-pa-bridge.env`,
     `/etc/flexbridge/flexbridge.env`, `/etc/hadiscovery/hadiscovery.env`,
     `/etc/atr1k-tuner-bridge/atr1k-tuner-bridge.env` and injects it into the installed
     config (password never leaves the Pi); stops the old service; moves the binary to
     `/opt/antennaselect/antennaselect` (0755) and the unit to
     `/etc/systemd/system/antenna-select.service`; `daemon-reload`; `enable`; `restart`;
     prints status.
- Systemd unit (generated): `Description=HF antenna-selection reconciler (antennaselect)`;
  `After=network-online.target` + `Wants=network-online.target`; `Type=simple`;
  `ExecStart=/opt/antennaselect/antennaselect -config /etc/antenna-select/config.toml`;
  `Restart=on-failure`; `RestartSec=5`; `User=Group=antenna-select`;
  `ConfigurationDirectory=antenna-select`; hardening: `NoNewPrivileges=true`,
  `ProtectSystem=full`, `ProtectHome=true`, `PrivateTmp=true` (no `DeviceAllow`/
  `SupplementaryGroups` — no serial device, no listening port).
- After the first deploy, `/etc/antenna-select/config.toml` on the device is the edit
  surface; redeploying never overwrites it (delete it to re-seed).
- Deploy status: built, unit-tested, and **deployed to shari** (memory notes grounding
  observed live on 2026-08-28), though the project CLAUDE.md still says "pending
  deployment" — the CLAUDE.md lags the code/deploy state.

---

## 8. Invariants & safety rules

1. **Never move the antenna switch port while the radio is transmitting** (cold-switch
   discipline). A wanted port change is deferred during `tx=="tx"` and emitted on the
   return to `rx`. Enforcement of RF safety is a hardware interlock; the reconciler owns
   ordering only.
2. **Never trust retained radio state for safety**: radio-derived fields are acted on only
   when the bridge LWT says `online` AND `device_online` is `true`. `/status` alone is
   insufficient by design.
3. **An empty band holds the last selection** — never resolve `""` to the fallback.
4. **Idle overrides an operator hold** (walk-away safety): station-inactive forces
   `off` even in manual mode.
5. **React to state, emit intent**: the reconciler never assumes a `select` took effect;
   it confirms via `ant-switch/state.selected` and re-asserts on mismatch.
6. **`off` is the safe default**: it grounds all antenna feeds (lightning protection), and
   the switch hardware independently fails to ground on power loss.
7. **All decisions serialized on one worker** (ordered, single-threaded reconcile+publish);
   **no MQTT message handler may ever block on a publish** (deadlock invariant, §5.4).
8. **The reconciler is never part of the RF enforcement path** — killing it degrades the
   station to manual operation but cannot make it unsafe.
9. Band-follow may only tune a controller while that controller's antenna is the selected
   target (never tune a beam that is not in circuit).
10. PA/tuner commands are fire-and-forget intents, not retained state; the ATU/PA self-heal
    by the reconciler re-resolving on retained `radio/state` replay at reconnect.
11. `freq_hz` is always an integer in Hz; mode names are canonical station-wide
    (`cw`,`usb`,`lsb`,`am`,`fm`,`data` — not used by this component but part of the
    shared-bus convention it lives in).

---

## 9. Known defects & fragilities

Verified against code and the 2026-08-28 adversarial live review (station memory
"antenna grounding recovery gaps"); auto-grounding itself works live, recovery is the
fragile half:

1. **First key-up after grounding transmits into the grounded switch.** Re-arming
   activity is *itself* a TX (`tx=="tx"`), and the select wanted in that same resolution
   is TX-deferred (§5.5). So the first transmission after an idle ground keys the radio
   into the grounded/short switch port; the select only fires at un-key, when
   `tx=="rx"` arrives. (The PA stays disarmed via a separate `antenna_ready` chain, so
   the amplifier does not key, but the FLEX keys into a short.) Structural to the current
   design: recovery-from-ground has no non-TX trigger (see 3).
2. **Recovery is starved by flexbridge's change-only publishing.** flexbridge publishes
   `radio/state` only on content change (no heartbeat). On a quiet receive period, no
   messages arrive, so (a) the reconciler cannot re-arm until the operator actually
   changes frequency or keys, and (b) the m5stamp `pa-arm` PLC — a *separate* consumer
   requiring a `radio/state` message within its 10 s `RADIO_HEARTBEAT_MS` window to hold
   `armed` — silently drops its arm on a quiet band and cannot re-arm until the next
   state change, even though the antenna has come back. The reconciler cannot fix this
   alone; the documented fix direction is a periodic flexbridge state heartbeat (~5 s
   while the link is live).
3. **No manual re-arm.** An operator `/cmd` request does not count as activity. While
   idle-inactive, tier 1 overrides even an operator hold, so the only un-ground path is a
   radio `freq_hz` change or a TX. An operator at the desk who never touches the VFO
   cannot un-ground the station except by transmitting into the short (see 1). Treating
   an operator command (or fresh retained state) as "presence" is the documented fix
   candidate.
4. **Deferred-for-TX has no timeout.** A frozen `tx=="tx"` (e.g. flexbridge dies mid-TX
   leaving a stale retained state) freezes *all* actuation — idle grounding, operator
   holds, everything — until flexbridge's own reconnect/Reset republishes `tx=="rx"`.
5. **Restart always re-arms the station.** `lastActivity` is born `now` and
   `RadioFreqHz` born 0, so the retained `radio/state` replay at startup always looks
   like a frequency change: a reconciler restart during idle **un-grounds** the station
   (activity flips to `active`) and resets the walk-away clock for another full 30
   minutes, with no operator present. The inverse failure of 2/3.
6. **Ultrabeam port-number discrepancy in the repo.** `config.example.toml` and the
   `deploy.sh` seed map `ultrabeam` to **`port4`**; the unit tests and the repo's
   top-level CLAUDE.md/integration docs say **`port3`**. The live behavior depends on
   whatever `/etc/antenna-select/config.toml` on shari actually says (seeded as `port4`,
   possibly hand-edited since). A reimplementation must treat the port mapping as pure
   configuration and pin down the physical truth before deploying.
7. **`settled` is received but not used.** `ant-switch/state.settled` (movement complete)
   is parsed into the inputs but no logic gates on it — the settled-wait handshake is
   explicitly documented backlog, to land with the ant-switch bridge work. Likewise
   `radio/state.tuning` is not yet an input.
8. **Operator requests are unvalidated.** Any non-empty, non-`auto` string becomes the
   hold target and is forwarded verbatim as `{"select":"<garbage>"}` to the switch; the
   reconciler itself never rejects unknown ports/resources.
9. **Job drops under load.** When the 256-slot jobs queue is full, inputs are silently
   dropped (by design, to protect the MQTT dispatch goroutine). Mitigated by idempotent
   re-resolution on the next message, but a dropped operator command is invisible until
   the next event on that topic.
10. **Publish failures advance dedup state.** Dedup markers (`lastSelect`,
    `lastFollowFreq`, `lastPaBand`, `lastTunerInline`) update before the publish succeeds;
    a failed publish of a given intent will not be retried until a *different* resolution
    or a reconnect replay.
11. **PA `set_band` ignores station activity.** It fires whenever the radio is online and
    reports a band — including while the station is idle-grounded. Harmless to RF safety
    (no carrier) but a reimplementation should know the binding is *not* activity-gated.
12. **Single point of coordination.** If the reconciler dies (and systemd's
    `Restart=on-failure` gives up), band-follow, tuner-follow, and selection stop and the
    station degrades to manual. Accepted only because RF safety is hardware; the project
    notes recommend an explicit "reconciler offline" indication (currently only the LWT
    `status` topic).
13. Docs lag: the project CLAUDE.md still says "pending deployment" though the service
    has since been deployed and grounding observed live (2026-08-28).

---

## 10. Re-implementation notes

**Must be preserved verbatim (behavior contract):**

- Exact topic strings and the four-plane suffix scheme (`meta`/`state`/`status`/`cmd`),
  the slot address `muehle/hf/antenna-select`, client-ID convention, and all target-slot
  topic names (configurable in principle, `radio`/`ant-switch`/`ant-ctrl`/`pa`/`tuner` in
  practice).
- Exact payload shapes and key spellings per topic, including the per-bridge asymmetries:
  `{"select": p}` (retained) for ant-switch; `{"action":"frequency","freq_hz":N}`
  (retained) for the controller; `{"action":"set_band","value":"<band>"}` and
  `{"action":"set_inline","value":<bool>}` (both **not retained**) for PA and tuner;
  `{"ts","mode","target","source"}` for own state; `{"request": …}` for own command.
  QoS 1 everywhere; retained-flag per topic exactly as in §3.1.
- The three-tier ladder with its exact precedence (idle > operator > auto), the derived
  `mode`, and idle overriding operator holds.
- The two-layer radio-liveness AND gate; the empty-band-holds rule; the
  known-but-unmatched → fallback rule; sorted-resource determinism.
- Cold-switch TX deferral and its log message semantics; select confirm/re-assertion via
  `ant-switch/state.selected` (including re-asserting after a manual switch override).
- The idle/activity model: activity = freq change or TX only; 5 s check interval; default
  30-minute grounding; grounding target `off`; re-arm only on freq change or TX.
- All dedup semantics (decision-change publish; lastSelect while switch position unknown;
  per-binding last-value dedup) and the not-retained PA/tuner self-heal model (re-resolve
  on retained `radio/state` replay at own reconnect).
- The no-blocking-publish invariant in whatever concurrency model the new stack uses
  (a serialized decision worker is the simplest equivalent).
- Clean-session/clean-reconnect semantics such that retained inputs replay on every
  reconnect (no persistent-session behavior).
- Config key names and validation rules (§6) so existing on-device config files keep
  working; seed-once deploy behavior; secret handling (password only in 0600 config).

**Free to change (implementation detail):**

- Language, MQTT client library, internal package layout, the jobs-queue mechanism
  (provided the deadlock invariant and input serialization are preserved some other way),
  the idle-loop ticker implementation, log formatting, the systemd unit's cosmetic
  fields, the TOML parsing library.
- The `/meta` `expose` block's *rendering* is consumer-neutral metadata — the keys and
  shape should be preserved (a downstream HA-discovery consumer parses them), but it
  carries no behavior.
- The exact `time.RFC3339` formatting choice; any equal-precision UTC timestamp works.

**Open items a reimplementation should ideally resolve (currently defects, §9):**
periodic radio-state freshness (needs the flexbridge side), a non-TX re-arm path
(operator presence), a TX-deferral timeout, restart-stable idle state, operator-request
validation, and `settled`-gating of switch confirmation.