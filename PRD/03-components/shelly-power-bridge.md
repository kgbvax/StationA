# 03-components — shelly-power-bridge

## 0. Purpose

This document specifies **shelly-power-bridge**, a small software daemon that fronts
two **smart plugs** — mains relays that can be switched on and off over the network
(here: two *Shelly Plus 1PM* Wi-Fi plugs, one switching the Mühle amateur-radio
station's master mains supply and one switching its 13.8 V DC power supply — the rail
that powers all the radio equipment). The bridge is a pure **MQTT-to-MQTT translator**:
the plugs are themselves MQTT clients on the station's shared message broker, and the
bridge has *no network connection of any kind to the plugs* — it only subscribes to the
plug's manufacturer-native MQTT topics and publishes the station's canonical topics.
Its job is to make the two supplies first-class, observable, commandable members of the
station bus so that the startup/shutdown sequencer (`powerseq`, see
`03-components/powerseq.md`) and operator consoles can read and switch station power
directly, instead of the historical arrangement where only a home-automation hub could.
The bus conventions it implements (three-plane topics, liveness, retained state) are
defined in `02-interface-spec.md`; this document is the component-level contract.

Terms used throughout, defined at first use per the PRD style contract:

- **Amateur radio (ham radio)**: the licensed hobby of two-way radio communication;
  the "Mühle station" is such an installation. No radio-domain knowledge is needed here —
  the plugs switch mains power for equipment racks.
- **MQTT**: a lightweight publish/subscribe protocol. Clients publish messages to
  slash-separated named **topics**; a central **broker** routes messages between
  publishers and subscribers. The production broker is at `192.168.1.50:1883`
  (MQTT 3.1.1, plain TCP, no TLS). A planned migration to a broker on the host "shari"
  (`192.168.1.139`) exists but is not deployed — see §12.
- **Retained message**: the broker stores the last message published on a topic and
  re-delivers it to every new subscriber, so a topic reads back its latest value.
- **LWT (Last Will and Testament)**: a message a client registers with the broker at
  connect time, which the broker publishes on the client's behalf if the client drops
  without disconnecting cleanly. Used to announce "process dead."
- **QoS 1**: at-least-once delivery with acknowledgment. All messages in this contract
  use QoS 1.
- **Slot**: the station model's unit of addressing — a topic stem
  `<site>/<station>/<slot>` with four topic suffixes: `/meta` (static identity),
  `/state` (live snapshot), `/status` (bridge-process liveness, LWT-driven), `/cmd`
  (commands into the component). See `02-interface-spec.md` §2.
- **Bridge**: a daemon that couples one physical device (or device family) to the bus,
  translating between device-native protocol and the canonical slot topics.
- **Shelly Gen2+**: the second-plus generation of Shelly smart-relay devices
  (Plus/Mini/Pro families), which speak MQTT natively as *peers on the same broker*
  — each publishes its own state under a device-id topic prefix and accepts JSON
  RPC commands over an MQTT topic.

---

## 1. Position in the system

### 1.1 The two slots

The bridge fronts exactly two plugs, each becoming one *site-level* power slot.
Site level means the slot address is `muehle/power/<slot>` — the station segment is
literally `power` — because these supplies sit **outside and upstream of both the HF and
UHF station paths**: the 13.8 V PSU feeds equipment in both stations, and the master
mains plug feeds everything. They are not part of any single station's equipment chain,
which is why they live one level above `muehle/hf/...` and `muehle/uhf/...`.

| Slot address | Physical device | Meaning |
|---|---|---|
| `muehle/power/master` | Shelly Plus 1PM | Station **master mains** — the upstream mains plug for the whole station. Top of the power tree; its `feeds` list is empty by design. |
| `muehle/power/psu-13v8` | Shelly Plus 1PM | The plug feeding the **13.8 V DC PSU** — a site-level DC rail feeding both the HF and UHF equipment (radios, antenna tuner, antenna controller, antenna switch, rotator, relay controllers). It declares a `feeds` list so consumers can reason about what goes dark if the rail drops. |

Both plugs are physically located at the site labeled `bauwagen` and speak Wi-Fi to the
broker; the bridge process runs on the Raspberry Pi host `shari`.

### 1.2 The `feeds` map (exact seeded value)

`muehle/power/psu-13v8`'s `/meta` capabilities carry this exact eight-entry `feeds`
list (station-relative slot addresses), as seeded by deployment:

```
hf/radio, uhf/radio, hf/tuner, hf/ant-ctrl, hf/ant-switch, hf/rotator, hf/switch, hf/pa-arm
```

`muehle/power/master` publishes **no `feeds` key at all** (omitted from the JSON when
empty — it is the top of the tree and feeds everything implicitly).

Two entries in this list do not match the rest of the station's slot registry and are
open decisions, not settled fact — see §12 for the evidence:

- `uhf/radio` — there is **no `muehle/uhf/radio` slot anywhere in the system**; the UHF
  station's registered slots are `muehle/uhf/pol-ctrl` and `muehle/uhf/rotator`. The
  feed presumably refers to a UHF radio physically powered by the same rail, but no
  bridge fronts it.
- `uhf/pol-ctrl` (the M5 Stamp PLC #2 polarization controller, which *is* a registered
  slot and which the narrative says the rail feeds) is **absent** from the seeded list,
  as is the HF power amplifier (`hf/pa`).

The PRD requires the seeded value be reproduced as-is for parity; a re-implementation
may correct the list only as an explicit, flagged decision (§12).

### 1.3 Compound bridge — normative warning

This is a **compound bridge**: one operating-system process fronts N plugs (N = number
of configured slot entries; 2 in production), each with its **own MQTT client
connection and its own LWT**. The reason is a protocol constraint turned into a
requirement: MQTT 3.1.1 allows only one Will per client connection, and the station
requires that **the death of the bridge process must take every fronted slot offline
simultaneously** — all Wills fire at once, with no window in which one slot reads
online while another reads offline.

**Normative warning:** because one process fronts both supplies, a single crash, hang,
out-of-memory kill, or deployment restart of this process takes **both**
`muehle/power/master` and `muehle/power/psu-13v8` offline at the same instant. There is
no way to take one supply's bridge down without taking the other's with it. This is a
deliberate trade-off (one process to deploy and supervise) that any re-implementation
must either preserve knowingly or reverse knowingly (e.g. one process per plug). Do not
split the difference: N clients with N Wills **in one process** is the contract; the
alternative is one process per slot with independently-firing Wills, which changes the
observable failure coupling.

A second coupling: the MQTT client identifier. The requirement is **one client id per
slot**, default `<site>-<station>-<slot>` (e.g. `muehle-power-master`,
`muehle-power-psu-13v8`). If a shared configured client id is used verbatim for all
slots, the broker will kick the older client whenever a new one connects (MQTT session
take-over) and the two slots will fight — see §11 defect 4.

---

## 2. Upstream interface — the Shelly devices

### 2.1 No direct connection (behavior contract)

**The bridge SHALL NOT open any network connection to a plug.** The Shelly devices are
pre-provisioned (by the operator, out of band) as MQTT clients on the same broker,
publishing under their device-id prefix. All bridge-to-plug communication is
publish/subscribe on the broker:

| Native topic | Direction | Payload | Purpose |
|---|---|---|---|
| `<shelly_id>/status/switch:0` | plug → bridge (subscribed, QoS 1) | JSON (§2.2) | Unsolicited relay-state announcement; the single source of truth for the relay's actual position. |
| `<shelly_id>/online` | plug → broker (subscribed, QoS 1) | Literal string `true` or `false` | Plug heartbeat. `true` periodically while the plug is connected; `false` on the plug's graceful MQTT disconnect. Drives the `device_online` field. |
| `<shelly_id>/rpc` | bridge → plug (published, QoS 1, not retained) | JSON RPC request (§4.2) | Commands the relay. |
| `<shelly_id>/rpc/rb` | plug → bridge | RPC response | **Not subscribed, not consumed** (see §4.3). |

`<shelly_id>` is the device's Gen2 MQTT prefix — its stable device id, e.g.
`shellyplus1pm-aabbccddeeff` — and comes from per-slot config.

The bridge is **entirely push-driven**: it SHALL NOT poll the plug, SHALL NOT send
status requests, and SHALL NOT attempt to configure the plug (its announce/heartbeat
periods are device-side settings the bridge neither reads nor writes; see §6.4 for how
the bridge bounds a silent plug anyway).

### 2.2 Native status payload

From `<shelly_id>/status/switch:0` the bridge parses exactly these JSON fields and
**ignores everything else** in the payload:

| Field | Type | Meaning | Use |
|---|---|---|---|
| `output` | bool | Actual relay position; `true` = on. | **The only field that affects behavior** — becomes `/state.power`. |
| `apower` | number (watts) | Apparent power draw. | Parsed, then discarded (known gap, §11 defect 2). |
| `aenergy` | number (watt-minutes) | Cumulative energy. | Parsed, then discarded. |
| `voltage` | number (volts) | Mains voltage. | Parsed, then discarded. |
| `current` | number (amps) | Load current. | Parsed, then discarded. |

Malformed payloads (invalid JSON) SHALL be logged at WARN and dropped with no state
change. A payload that is valid JSON but lacks `output` decodes as `false` → power
`"off"` — the bridge SHALL treat an announcement without an `output` key as a
report of "relay off" (reference-implementation note: this is Go's JSON zero-value
behavior, kept as a contract).

---

## 3. Canonical MQTT presence

For each slot (`<slot>` = `master` | `psu-13v8`), the bridge owns these topics.
Topic strings, retention, and QoS are normative.

| Topic | Retained | QoS | Direction | Cadence |
|---|---|---|---|---|
| `muehle/power/<slot>/meta` | yes | 1 | bridge → bus | On every MQTT (re)connect. |
| `muehle/power/<slot>/state` | yes | 1 | bridge → bus | Only when the snapshot changes (change-deduped, §5.3). |
| `muehle/power/<slot>/status` | yes | 1 | bridge + broker (LWT) | `online` by the bridge on every (re)connect; `offline` by the broker via the Will on ungraceful death. |
| `muehle/power/<slot>/cmd` | yes (retained on the bus; written by other components) | 1 (subscription) | bus → bridge | Subscribed by the bridge; the broker replays the last retained command at every (re)connect — the self-heal mechanism, §4.4. |

Note the `/cmd` retention is the station's documented **exception**: commands are
normally *not* retained, but idempotent actuator setpoints like power on/off are
retained so the intent survives restarts (see `02-interface-spec.md` §5).

The bridge SHALL NOT publish Home Assistant discovery topics; a separate passive
consumer (`03-components/hadiscovery.md`) renders discovery from the `expose` block.

### 3.1 `/meta` payload (exact shape)

Published retained on every (re)connect. For the PSU slot (`feeds` shown); the master
slot is identical minus the entire `feeds` key:

```json
{
  "schema": "1.0",
  "role": "power",
  "device": { "model": "Shelly Plus 1PM", "serial": "shellyplus1pm-<mac>" },
  "link": "wifi",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "fail_safe": "off",
    "feeds": ["hf/radio", "uhf/radio", "hf/tuner", "hf/ant-ctrl",
              "hf/ant-switch", "hf/rotator", "hf/switch", "hf/pa-arm"]
  },
  "expose": {
    "device": { "name": "Shelly Plus 1PM", "model": "Shelly Plus 1PM", "manufacturer": "Shelly" },
    "fields": [
      {
        "key": "power", "name": "Power", "type": "enum", "options": ["on", "off"],
        "writable": true,
        "command": { "action": "set_power", "value_key": "value", "value_type": "string" }
      }
    ]
  }
}
```

Field semantics:

- `schema`: literal `"1.0"`.
- `role`: literal `"power"` (the station model's role taxonomy; these are the only
  two `power`-role slots in the system).
- `device.model` / `device.serial`: from per-slot config. `serial` SHOULD be the stable
  Gen2 device id (the same string as `shelly_id`).
- `link`: literal `"wifi"` (the plug's own network link, as opposed to e.g. serial).
- `location`: free-text physical-location label from per-slot config; omitted when
  empty. Production value: `bauwagen`.
- `host`: compute node the bridge runs on, from global config. Production value:
  `shari`.
- `capabilities.fail_safe`: `"on"` | `"off"` from per-slot config, default `"off"`.
  Semantics: the plug's power-on default after a mains outage — `"off"` means a mains
  blip drops the station rather than re-energizing it unexpectedly (a deliberate
  safety posture, see `06-safety.md`). Informational metadata only: the bridge never
  programs the plug; the plug's actual out-of-band setting must match.
- `capabilities.feeds`: list of downstream slot addresses this supply powers
  (station-relative, e.g. `hf/radio`). **The key SHALL be omitted entirely from the
  JSON when the list is empty** — the master slot publishes no `feeds` key.
- `expose`: the consumer-neutral field surface (see `02-interface-spec.md` §7). Exactly
  one field, `power`, writable, enum `on|off`, with the command descriptor
  `{action:"set_power", value_key:"value", value_type:"string"}` — the argument of a
  `/cmd` message always rides under the JSON key `"value"`, never under a key named
  after the action (station-wide convention).

### 3.2 `/state` payload (exact shape)

Published retained, change-deduped (§5.3):

```json
{
  "ts": "2026-07-14T18:42:01Z",
  "power": "on",
  "device_online": true
}
```

- `ts`: string, RFC 3339, UTC — timestamp of this snapshot. Regenerated per publish;
  never part of dedup.
- `power`: `"on"` | `"off"` — the **actual relay position** as announced by the plug,
  read back from `<shelly_id>/status/switch:0`. Never an open-loop echo of the last
  command (§4.3).
- `device_online`: bool — plug reachability, heartbeat-based (§6.4). This is the
  second, distinct liveness layer: `/status` says whether the *bridge process* is
  alive; `device_online` says whether the *plug behind the bridge* is alive. Consumers
  must check both (see `02-interface-spec.md` §6). Note the deployed bridge publishes
  `device_online` explicitly as `true`/`false`; the integration-model text describes
  an "omitted when true" form — consumers must treat both forms as equivalent
  (absence = true); see §12.
- `error`: string, present only when non-empty — the reason the device was marked
  offline, one of `"shelly heartbeat lost"` or `"shelly online=false"`. When the plug
  recovers the key is cleared and disappears from the JSON again.

### 3.3 `/status` payload

Literal string `online` or `offline`, retained. `online` is published by the bridge in
its connect handler on every (re)connect. `offline` is registered as the Will at
connect time — per slot: Will topic = the slot's `/status` topic, payload `offline`,
QoS 1, retained = true — so the broker publishes it if the process dies or the client
drops.

**Actual deployed behavior (not the idealized claim):** on a *clean* process shutdown
(SIGTERM, graceful MQTT DISCONNECT), the broker does **not** fire the Will, so the
retained `/status` stays `online` for a stopped service. Consumers must not trust
`/status` alone to mean "running right now" — it means "running at last announcement,
or stopped cleanly." This asymmetry is documented station-wide in
`02-interface-spec.md` §6 and is inherited, not introduced, by this component.

---

## 4. Command surface

### 4.1 The one command

Exactly one command is accepted, on the slot's `/cmd` topic, JSON with keys `action`
(string) and `value` (string):

```json
{ "action": "set_power", "value": "on" }
{ "action": "set_power", "value": "off" }
```

- `set_power` + `on` → publish the `Switch.Set` RPC with the relay-on parameter.
- `set_power` + `off` → same with relay-off.
- **No other action or value is accepted.** Unknown actions and unknown values (e.g.
  `value:"sleep"`) SHALL be logged at WARN and silently dropped: no error topic, no
  negative acknowledgment of any kind (§11 defect 9). Malformed `/cmd` JSON: same
  (WARN log, drop).
- The bridge SHALL NOT amplify the command surface: the only thing it ever sends the
  plug is the exact `Switch.Set` RPC of §4.2.

Multiple writers may publish `/cmd` (the sequencer, operator consoles, the
home-automation hub as one writer among many); last retained write wins. The bridge is
the only writer of the plug's RPC topic.

### 4.2 RPC payload (exact bytes)

Published to `<shelly_id>/rpc`, QoS 1, not retained:

```json
{"id":1,"src":"shelly-power-bridge","method":"Switch.Set","params":{"id":0,"on":true}}
```

- `id`: fixed `1` (an RPC call counter that is never incremented — irrelevant since
  responses are not consumed).
- `src`: fixed string `"shelly-power-bridge"` — visible to the plug; keep it
  identifiable.
- `method`: `Switch.Set`; `params.id`: fixed `0` (switch component 0 — the single
  relay of a Plus 1PM); `params.on`: `true`/`false` per the requested state.

If the bridge's MQTT client is not connected at dispatch time, the command fails with
`"mqtt client not connected"` (WARN log) and is dropped — but because `/cmd` is
retained, the broker replays it on the next reconnect, so the intent is not lost
across bridge or broker restarts (§4.4).

### 4.3 Fire-and-observe confirmation (behavior contract)

The bridge SHALL NOT update `/state` optimistically after issuing a command, and SHALL
NOT subscribe to or interpret the RPC response topic `<shelly_id>/rpc/rb`.
Confirmation is **fire-and-observe**: the bridge waits for the plug's own
`<shelly_id>/status/switch:0` announcement to reflect the new relay position and only
then publishes the changed `/state`. Consequences, all normative:

- A command accepted by the broker but not executed by the plug changes nothing on
  `/state`.
- A failed switch (e.g. relay fault) manifests only as "the announced state never
  changes" — detectable by consumers (compare `/cmd` intent vs `/state.power`), not
  signalled by the bridge (§11 defect 10).
- `/state.power` always equals the last *observed* relay position, never the last
  *commanded* one.

### 4.4 Retained `/cmd` as steady-state intent (behavior contract)

`/cmd` is retained and is the steady-state intent for the plug. On every bridge
(re)connect — including subscribe time after a restart — the broker replays the last
retained command and the bridge re-applies it, so the plug converges to the intended
state even after a bridge crash, broker restart, or network gap. The bridge SHALL NOT
clear or consume (delete) the retained command. This makes the power slots
self-healing without any persisted bridge-side state.

---

## 5. Behavior & state machine

### 5.1 Startup

1. Load the TOML config (path from flag, default
   `/etc/shelly-power-bridge/config.toml`), apply environment overrides, validate.
   Config-file unreadable, TOML decode error, or validation failure → message to
   stderr, **exit code 2**. A *missing* config file is not an error (built-in defaults
   are used, but validation then fails for lack of slot entries — exit 2).
2. Bind a cancellation context to SIGINT/SIGTERM.
3. Spawn one independent worker per configured slot. Slots are fully independent; the
   process exits only when all have returned. If any slot's **initial** MQTT connect
   fails, that slot returns an error and the process exits with **exit code 1**; the
   service supervisor then restarts the whole process after **5 s**
   (`Restart=on-failure`, `RestartSec=5`).
4. The initial connect SHALL be interruptible by the shutdown signal: if SIGTERM
   arrives while the broker is unreachable, the connect aborts immediately rather than
   hanging until the supervisor's stop timeout escalates to a force kill. (Derived
   from a live incident in a sibling component; stated as a library-independent
   requirement.)
5. Per slot, before connecting, build: the serialized state worker (§5.4), the heartbeat
   staleness watcher (§6.4), and the bridge state object with initial state
   `power:""`, `device_online:false`. **Nothing is published until the first real
   event arrives** — the very first snapshot always differs from the empty
   last-published one, so the first native announcement or heartbeat publishes
   immediately.

### 5.2 Connect / reconnect (per slot)

On every successful (re)connect, in this order:

1. Publish `online` (retained, QoS 1) to the slot's `/status`.
2. Mark the bridge connected — `/state` publishing is now allowed.
3. Publish `/meta` (retained).
4. Subscribe (QoS 1) to `<shelly_id>/status/switch:0`, `<shelly_id>/online`, and the
   slot's `/cmd`. The retained `/cmd` is replayed by the broker at subscribe time and
   re-applied — the self-heal path (§4.4).

MQTT session parameters: `CleanSession=false`, auto-reconnect enabled, MQTT
keep-alive default 30 s (a library default; not load-bearing).

On connection loss: log WARN; mark the bridge disconnected; **stop publishing
`/state`** (the Will already announced `offline`; publishing state into a dead session
is wrong); hold received state in memory and publish it on the next change after
reconnecting. Re-establishment is automatic; the bridge adds no explicit backoff of
its own.

### 5.3 Normal operation and change-dedup

- `<shelly_id>/status/switch:0` message → parse; on success: set `power` from
  `output`, set `device_online=true`, clear `error`; publish `/state` **if the snapshot
  changed**. A native announcement is proof of reachability, so it also *revives*
  `device_online` — recovery comes only from this path or a heartbeat `true`.
- `<shelly_id>/online` message → payload exactly `true`: record the heartbeat
  timestamp and refresh `device_online=true`; payload exactly `false`: mark the device
  offline with error `"shelly online=false"` (publishing a changed `/state` with
  `device_online:false`). Any other payload falls into the `false` branch (only the
  literal `true` is special-cased).
- `/cmd` message → parse and dispatch (§4).

**Change-dedup (exact rule):** only three fields drive the comparison — `power`,
`device_online`, `error`. `ts` is regenerated per publish and never compared. A
snapshot SHALL be published only when one of those three changes; identical
consecutive snapshots are not republished. Corollary: after a broker reconnect the
retained `/state` copy still serves; a snapshot changed during the disconnect is only
published when the next event arrives (a freshness gap, not a correctness break — §11
defect 8).

### 5.4 Serialized state path (library-independent constraint)

All state mutation and all publishing for a slot SHALL flow through a single
serialized path, decoupled from the MQTT receive path: inbound message handlers only
enqueue work items onto a per-slot bounded queue (reference implementation: capacity
64) and never publish or block. Rationale: in the reference MQTT library, incoming
handlers run on the connection's dispatch thread; a handler that blocks or publishes
synchronously deadlocks the client — this exact defect took down a sibling component
in production. Any stack must preserve the decoupling, whatever its threading model.

When the queue is full, incoming work SHALL be dropped, never blocked on (protecting
the receive path is the point of the bound). A dropped command is recovered by the
retained-`/cmd` replay on the next reconnect; a dropped telemetry event is recovered
by the plug's next announcement. The reference implementation drops silently and
uncounted — a re-implementation SHOULD at least count and log drops (§11 defect 6).

### 5.5 Shutdown

On SIGINT/SIGTERM: cancel the context; abort a pending initial connect (§5.1 step 4);
disconnect each slot's client gracefully with a 500 ms quiesce allowance — a graceful
DISCONNECT suppresses the Will, so a clean stop does **not** flip `/status` to
`offline` (it stays retained-`online`; §3.3); stop the workers; log
`shelly-power-bridge stopped`; exit **0**.

### 5.6 Error-path summary

| Condition | Effect |
|---|---|
| Config unreadable (other than missing) / TOML decode error / validation failure | stderr message, exit 2 |
| Initial MQTT connect failure | slot worker errors → process exit 1 → supervisor restart after 5 s |
| Broker disconnect after initial connect | WARN; `/state` publishing suppressed; auto-reconnect; broker fires Will (`/status` = `offline`) |
| Malformed native status / malformed `/cmd` / unknown cmd action or value | WARN log, no state change, message dropped |
| `Switch.Set` publish fails (client disconnected mid-dispatch) | WARN; retained `/cmd` replay re-drives it after reconnect |
| Jobs queue full | incoming event silently dropped (state may lag until next event) |

---

## 6. Liveness — the two layers

### 6.1 Layer 1: bridge liveness (`/status`)

LWT per slot, registered at connect: topic `muehle/power/<slot>/status`, payload
`offline`, QoS 1, retained. `online` republished by the bridge on every (re)connect.
Process death fires **every** slot's Will at once (§1.3). Clean shutdown leaves
`online` in place (§3.3).

### 6.2 Layer 2: plug liveness (`/state.device_online`)

Driven by the plug's own heartbeat (`<shelly_id>/online`) and native announcements:
heartbeat `true` or any announcement → online; heartbeat `false` or staleness →
offline. The two layers SHALL never be conflated: the bridge can be perfectly alive
while the plug is dark (Wi-Fi outage), and the plug can be announcing while the
bridge is down (announcements unread by anyone). Consumers must AND both — see
`02-interface-spec.md` §6 for the station-wide rule and the incident that motivated it.

### 6.3 Unknown must not read as online

A plug never heard from since process start SHALL be reported `device_online:false`
within the first staleness tick (≤10 s of startup), not stuck at a default-online
value. Initial state is offline until proven otherwise.

### 6.4 Heartbeat staleness watcher (exact timing)

- Per slot, a ticker fires every **10 s** (default; not currently configurable).
- On each tick, if the time since the last `<shelly_id>/online` = `true` heartbeat
  exceeds **75 s**, the device is marked offline with error `"shelly heartbeat lost"`
  and a changed `/state` is published (dedup means only one publish; the watcher keeps
  ticking while stale but adds nothing).
- If no heartbeat has ever been received since process start, the elapsed time is
  treated as **1 hour** — i.e. the never-heard case trips the threshold on the first
  tick (§6.3).
- The watcher only marks offline, never online; recovery requires a native
  announcement or a heartbeat `true`.
- The 75 s threshold is a **default tuned against the plug's own device-side heartbeat
  cadence**, which the bridge does not set or query. It is not configurable from the
  bridge's config (§11 defect 7): if the plug's announce period is ever lengthened
  past 75 s, the slot flaps offline.

---

## 7. Invariants (testable requirements for any re-implementation)

1. `/state.power` SHALL always equal the actual relay position read back from the
   plug's native announcement, never the last commanded value. No optimistic state.
2. `fail_safe` default SHALL be `off`: after a mains outage the station stays off
   until explicitly re-energized. The bridge publishes the configured value as
   metadata but never programs the plug; the plug's out-of-band power-on default
   must match the published metadata.
3. Process death SHALL flip every fronted slot to `/status` = `offline`
   simultaneously — one Will per slot, all registered at connect time by the same
   process.
4. `/status` (bridge liveness) and `/state.device_online` (plug reachability) SHALL
   remain distinct fields with distinct drivers.
5. `/cmd` SHALL be retained and treated as steady-state intent: replayed and
   re-applied on every reconnect; never cleared by the bridge.
6. No command amplification: only `set_power` with value `on|off` SHALL be acted on;
   everything else is dropped. The only plug-bound message SHALL be the exact
   `Switch.Set` RPC of §4.2.
7. `/state` SHALL NOT be published while the slot's MQTT client is disconnected.
8. An unknown plug SHALL NOT read as online (§6.3).
9. Inbound MQTT handling SHALL be decoupled from outbound publishing (§5.4) —
   a blocked receive handler deadlocks the client; this happened live in a sibling
   component.
10. No secret SHALL appear in the config file, unit file, or process command line;
    secrets arrive via an environment file (§8.2).
11. A clean SIGTERM shutdown SHALL disconnect gracefully (Will suppressed, `/status`
    stays `online`) with a bounded quiesce (500 ms); only ungraceful death fires the
    Will.
12. Exit codes SHALL be: 2 = configuration/validation error, 1 = runtime failure
    (initial connect), 0 = clean stop.

---

## 8. Configuration

### 8.1 Keys

TOML file, default path `/etc/shelly-power-bridge/config.toml`, override with
`-config <path>`. A missing file is not an error (defaults apply — and then validation
fails for lack of slots). Unknown keys are silently ignored. Load order: defaults →
TOML → environment overrides → `-log.level` flag (case-insensitive; overrides config).
Levels: `debug` | `info` | `warn` | `error`, default `info`, to stderr, text format.

Global keys:

| Key | Default | Meaning |
|---|---|---|
| `host` | `shari` | Compute-node name, published in `/meta.host`. |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | Broker URI (see §12 on the planned broker migration). |
| `mqtt.client_id` | `""` | Empty = per-slot id `<site>-<station>-<slot>`. If non-empty it is used for **all** slots — a collision trap (§1.3, §11 defect 4); the empty default is the safe, production value. |
| `mqtt.user` | `hf` | MQTT username. |
| `mqtt.password` | `""` | Overridden by environment in practice; the secret is never kept in the TOML. |
| `mqtt.site` | `muehle` | Site prefix of every slot address; mandatory. |
| `mqtt.discovery_prefix` | `homeassistant` | Carried in config, published nowhere by this bridge (rendered by the separate hadiscovery consumer); effectively inert here. |
| `log.level` | `info` | As above. |

Per-slot keys (one `[[slot]]` table per plug):

| Key | Default | Meaning |
|---|---|---|
| `station` | — (required) | Middle path segment; site-level power slots use `power`. |
| `slot` | — (required) | Last path segment: `master`, `psu-13v8`. |
| `location` | `""` | Free-text label → `/meta.location`, omitted when empty. |
| `device_model` | `""` | → `/meta.device.model` and `expose.device.name`/`model`. |
| `device_serial` | — (required) | → `/meta.device.serial`; should equal the Gen2 device id. |
| `shelly_id` | — (required) | The Gen2 MQTT prefix; determines all native topics (§2.1). |
| `fail_safe` | `off` | `"on"` or `"off"` only (validated) → `/meta.capabilities.fail_safe`. |
| `feeds` | absent | List of downstream slot addresses → `/meta.capabilities.feeds`, omitted when empty. |

Validation failures (exit 2): no site, no broker, no slot entries, empty
station/slot/shelly_id/device_serial, `fail_safe` not in {`on`,`off`}, duplicate slot
address. Per-slot values come from the TOML only; they have no environment overrides.

### 8.2 Secrets

Environment overrides applied after TOML (used by the supervisor's environment file):
`SHELLY_POWER_BRIDGE_MQTT_BROKER`, `SHELLY_POWER_BRIDGE_MQTT_CLIENT_ID`,
`SHELLY_POWER_BRIDGE_MQTT_USER`, `SHELLY_POWER_BRIDGE_MQTT_PASSWORD`,
`SHELLY_POWER_BRIDGE_MQTT_SITE`. Empty env values do not override. The MQTT password
lives in `/etc/shelly-power-bridge/shelly-power-bridge.env` (mode 0600, owned by the
service user) as `SHELLY_POWER_BRIDGE_MQTT_PASSWORD=...` and is injected by the
service definition — it never appears in the TOML, the unit file, or the process
command line. This is the station-wide convention (see `05-deployment-ops.md` §3).

---

## 9. Deployment

Target: Raspberry Pi `shari` at `192.168.1.139`, SSH user `io`. The reference deploy
script performs the whole flow (overridable via env vars `SSH_HOST`, `SSH_USER`,
`SERVICE_NAME`, `SERVICE_USER`, `INSTALL_DIR`, `HOST_NAME`, `LOG_LEVEL`, `MQTT_BROKER`,
`MQTT_SITE`, `MQTT_USER`, `MQTT_PASSWORD`, `MASTER_SHELLY_ID`, `PSU_SHELLY_ID`):

1. Cross-compile a static `linux/arm64` binary into `dist/`.
2. Generate a **seed** config and a **seed** environment (secrets) file locally — temp
   files, 0600 umask; the secret file travels separately from the config.
3. Generate the service unit (§9.1).
4. Copy binary, unit, and seeds to the Pi; on the Pi: create a dedicated system user
   `shelly-power-bridge` (no login, no home) if missing; create
   `/etc/shelly-power-bridge`; **seed-once**: install the config and env file only if
   they do not already exist (0600, service-user owned) — later deploys never
   overwrite them, the device owns its settings; stop the service, move the binary
   into `/opt/shelly-power-bridge/`, install the unit, reload the supervisor, enable +
   restart, print status.
5. The seed config hard-codes the two `[[slot]]` blocks (master + psu-13v8, with the
   `feeds` list of §1.2). `MASTER_SHELLY_ID`/`PSU_SHELLY_ID` must be set to the real
   device ids before first deploy — **but note the reference script does NOT actually
   refuse its placeholder defaults** (`shellyplus1pm-aabbccddeeff`,
   `shellyplus1pm-112233445566`) despite its documentation claiming it does; it only
   prints a warning to verify. A re-implementation SHALL make first-deploy refuse or
   require confirmation of placeholder ids (§11 defect 5) — a first deploy with
   placeholders silently binds to nonexistent devices (all telemetry silent, all slots
   `device_online:false`).

Runtime dependencies: only network access to the MQTT broker. The plugs must be
pre-provisioned out of band to speak MQTT to the same broker under their device-id
prefix — the bridge cannot configure them.

### 9.1 Service unit (reference settings, normative posture)

- ExecStart: `/opt/shelly-power-bridge/shelly-power-bridge -config /etc/shelly-power-bridge/config.toml`
- EnvironmentFile: `/etc/shelly-power-bridge/shelly-power-bridge.env`
- `Type=simple`, `Restart=on-failure`, `RestartSec=5`, after/wants network-online.
- Runs as dedicated user/group `shelly-power-bridge`; `ConfigurationDirectory` and
  `StateDirectory` = `shelly-power-bridge`.
- Hardening: `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
  `PrivateDevices=true` (network-only service — no devices needed),
  `ProtectKernelTunables/Modules`, `ProtectControlGroups`,
  `RestrictAddressFamilies=AF_INET AF_INET6`, `RestrictNamespaces`,
  `LockPersonality`, `RestrictRealtime`, `RestrictSUIDSGID`, `RemoveIPC`,
  empty capability sets, `ReadWritePaths=/var/lib/shelly-power-bridge`.
- Resource ceilings: `MemoryMax=256M`, `TasksMax=64` — the Pi hosts all station
  services; a leak in one must not starve the others (compound-bridge corollary of
  §1.3: exceeding the ceiling takes **both** slots offline at once).
- Logs to the system journal, identifier `shelly-power-bridge`.

Full deployment conventions: `05-deployment-ops.md`.

---

## 10. Consumers and safety context

- `powerseq` (see `03-components/powerseq.md`) is the primary automated consumer: it
  drives the startup/shutdown sequence over these two slots' `/cmd` topics and reads
  their `/state` to confirm ordered power transitions.
- The operator console (see `04-console.md`) displays both slots and offers switching.
- The `feeds` metadata lets any consumer compute blast radius: if `psu-13v8` drops,
  everything in its `feeds` list goes dark (most of those devices then also lose their
  bridges' device links — a cascading pattern consumers must expect).
- Because `master` is the upstream of `psu-13v8` itself, switching `master` off takes
  the PSU (and all its feeds) dark too. The `fail_safe=off` posture means neither
  supply re-energizes on mains restoration without an explicit command — see
  `06-safety.md` §2 for the station-level rule that humans or the sequencer, never the
  plugs themselves, decide when power returns.

---

## 11. Known defects & fragilities of the legacy system

A re-implementation must treat items 1 and 2 as **decisions to make explicitly** (fix
or preserve for parity), and the rest as known hazards to design against.

1. **Heartbeat `true` falsifies `power` (the heartbeat false-on defect).** The
   `<shelly_id>/online` = `true` handler refreshes the *whole* telemetry snapshot
   including `power`, setting it to `"on"` — not just `device_online`. If the relay is
   actually off, the next periodic heartbeat publishes `/state` with `power:"on"`, a
   wrong value that persists until the plug's next native status announcement
   corrects it (announcements are periodic but sparse; the window can be minutes).
   Legacy comment calls it "a no-op if power unchanged," which is only true when the
   relay is on. A re-implementation SHOULD make the heartbeat refresh only
   `device_online`/`error` and leave `power` untouched — flag the behavioral change
   if made.
2. **Parsed metering is discarded (the metering gap).** The Plus 1PM reports
   `apower`/`aenergy`/`voltage`/`current` in every native announcement; the bridge
   decodes them and publishes none of them — the canonical `/state` has no
   power/energy fields. The parse already exists in the legacy code, so closing the
   gap is cheap; the PRD leaves it an open decision (§12) whether `/state` gains
   metering fields (which would change the `/state` schema — coordinate with
   `02-interface-spec.md`).
3. **Committed build artifacts.** A compiled binary is checked into the project
   directory in the legacy repo. Hygiene only; not behavior.
4. **Shared `client_id` is a collision trap.** Setting `mqtt.client_id` non-empty makes
   all slots share one MQTT client id; the broker kicks the older client on each
   connect and the slots fight over the session. The default (empty) is safe. A
   re-implementation should suffix a configured id per slot or reject a non-empty
   shared id.
5. **Deploy script does not refuse placeholder Shelly ids** (§9 step 5) — docs-vs-code
   disagreement in the legacy repo; the code seeds placeholders unverified.
6. **Silent event drop under load.** The bounded queue (§5.4) drops on overflow,
   unlogged and uncounted. Acceptable at this scale; count and log the drops.
7. **Staleness threshold coupled to invisible device config.** The 75 s heartbeat
   timeout (§6.4) assumes the plug's device-side heartbeat period; the bridge neither
   queries nor configures it, and the 75 s value is not configurable from the bridge's
   config. Lengthen the plug's period past 75 s and the slot flaps offline.
8. **`/state` is not refreshed on reconnect.** After a broker reconnect the bridge
   republishes `/meta` only; a state change during the disconnect waits for the next
   event. Minor freshness gap (in practice the heartbeat path usually republishes
   soon after).
9. **No command feedback.** Invalid `/cmd` actions/values are dropped with only a log
   line; nothing on the bus tells the writer it was ignored.
10. **No RPC response/error consumption.** The `Switch.Set` reply topic and the
    plug's error fields are ignored (deliberately, §4.3); a failed switch is only
    detectable by consumers as "announced state never changes."

---

## 12. Open decisions & unresolved facts

1. **The `uhf/radio` mystery feed.** The seeded `feeds` list for `psu-13v8` (identical
   in `config.example.toml` and the deploy script's seed generator) contains
   `uhf/radio`, but no `muehle/uhf/radio` slot exists anywhere in the system — the
   UHF station's registered slots are `uhf/pol-ctrl` and `uhf/rotator` only, and the
   legacy narrative says the rail feeds "the radios" of both stations. Variants:
   (a) a UHF radio exists physically but has no bridge/slot, and the feed refers to
   it; (b) the entry is stale/erroneous. Evidence for both: the entry appears in every
   config artifact (so it is deliberate, not a typo), while the slot registry
   contradicts it. Unresolved — needs operator confirmation of what the 13.8 V rail
   physically feeds. A re-implementation should reproduce the list as-is for parity
   unless the operator resolves it.
2. **Feeds list omissions.** The seeded list omits `uhf/pol-ctrl` (PLC #2, a
   registered slot the narrative says the rail feeds — though note the PLC #2 firmware
   itself is a documented repo gap) and `hf/pa` (the HF power amplifier). Possibly the
   PA is mains-powered directly (it is on the `master` tree), and PLC #2 was added to
   the site after the feed list was written. Unresolved; same parity guidance as (1).
3. **Metering in `/state` (defect 2).** Publish `apower`/`aenergy`/`voltage`/`current`
   to `/state` (or a sibling topic), or keep discarding them? The device provides the
   data and the legacy code already parses it, but the deployed `/state` schema has
   none of it. Any addition changes the `/state` contract and must be coordinated
   with `02-interface-spec.md`.
4. **Heartbeat false-on fix (defect 1).** Preserve the legacy falsifying behavior
   exactly (parity) or fix the handler to refresh only `device_online`? The PRD
   recommends the fix, but it changes observable behavior (heartbeat no longer
   publishes `power:"on"`) and must be a flagged decision, not an accident.
5. **`device_online` wire form.** Deployed bridges publish `device_online:true`
   explicitly; the integration-model document describes an "omitted when true" form.
   The PRD's cross-document position: consumers must treat both forms as equivalent
   (absence = true), or the bus must mandate explicit-true. Unresolved station-wide —
   see `02-interface-spec.md` §6.
6. **Broker topology.** All deployed defaults point at `192.168.1.50:1883` (the shack
   broker). A migration to a broker on shari (`192.168.1.139`) exists on an unmerged,
   undeployed feature branch as of 2026-08-29. This document specifies the
   `192.168.1.50` production topology; the migration is a deployment decision, not a
   component contract — see `05-deployment-ops.md`.
7. **Real Shelly device ids.** The example/seed configs carry placeholder ids
   (`shellyplus1pm-aabbccddeeff`, `shellyplus1pm-112233445566`) and example serials
   (`shellyplus1pm-A1`, `shellyplus1pm-B2`). The real production ids live only in the
   seeded config on shari (`/etc/shelly-power-bridge/config.toml`) and were not
   readable from the workstation when this PRD was written. A re-implementation must
   obtain them from the operator or the live host; do not copy the placeholders.

---

## 13. Reference-implementation notes (non-normative)

The legacy component is a Go daemon (`shelly-power-bridge/` in the stationa monorepo)
using the paho MQTT client and a shared station library for connection setup and
topic/command helpers. Implementation details that are *not* part of the contract:
Go, paho, goroutine structure, TOML parsing library, log format, the ctx-aware
`Connect()` workaround (a goroutine + select bridging the blocking connect against the
shutdown signal — the *behavior* of §5.1 step 4 is the contract, the mechanism is not),
the jobs-channel pattern (any mechanism satisfying §5.4 is fine), how the heartbeat
clock and dedup are stored, JSON field order/whitespace (consumers parse JSON; do not
change field names or types), and whether the RPC response topic is ever subscribed
(today it deliberately is not). The 10 s tick, 75 s threshold, 500 ms quiesce, 5 s
supervisor restart, and queue depth 64 are contract values (the exact queue depth is
a free bound). The component publishes `Switch.Set` with `src:"shelly-power-bridge"`
visible to the plug; a re-implementation should keep a similarly identifiable source
string.