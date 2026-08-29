# 03-components — shelly-power-bridge

## 0. Purpose

This document describes **shelly-power-bridge**, a small software daemon that fronts
two **smart plugs**. Smart plugs are relays on the **mains** supply (the building AC
power feed) that you can switch on and off over the network. Here the plugs are two *Shelly Plus 1PM* Wi-Fi plugs. One switches the
Mühle amateur-radio station's master mains supply. The other switches its 13.8 V DC
power supply — the rail that powers all the radio equipment. The bridge is a pure
**MQTT-to-MQTT translator**. The plugs are themselves MQTT clients on the station's
shared message broker. The bridge has *no network connection of any kind to the
plugs*. It only subscribes to the plugs' manufacturer-native MQTT topics and
publishes the station's canonical topics. Its job is to make the two supplies first-class,
observable, commandable members of the station bus. Then the startup/shutdown
sequencer (`powerseq`, see `03-components/powerseq.md`) and operator consoles can read
and switch station power directly. In the historical arrangement, only a
home-automation hub had that ability. The bus conventions it implements (three-plane
topics, liveness, retained state) come from `02-interface-spec.md`. This document is
the component-level contract.

Terms used throughout, defined at first use per the PRD style contract:

- **Amateur radio (ham radio)**: the licensed hobby of two-way radio communication.
  The "Mühle station" is such an installation. You need no radio-domain knowledge
  here — the plugs switch mains power for equipment racks.
- **MQTT**: a lightweight publish/subscribe protocol. Clients publish messages to
  slash-separated named **topics**. A central **broker** routes messages between
  publishers and subscribers. The production broker is at `192.168.1.50:1883`
  (MQTT 3.1.1, plain TCP, no TLS). A planned migration to a broker on the host
  "shari" (`192.168.1.139`) exists. We have not deployed it — see §12.
- **Retained message**: the broker stores the last message published on a topic.
  It re-delivers that message to every new subscriber, so a topic reads back its
  latest value.
- **LWT (Last Will and Testament)**: a message a client registers with the broker at
  connect time. The broker publishes it on the client's behalf if the client drops
  without disconnecting cleanly. The broker uses it to announce "process dead."
- **QoS 1**: at-least-once delivery with acknowledgment. All messages in this
  contract use QoS 1.
- **Slot**: the station model's unit of addressing — a topic stem
  `<site>/<station>/<slot>` with four topic suffixes. `/meta` (static identity),
  `/state` (live snapshot), `/status` (bridge-process liveness, LWT-driven), `/cmd`
  (commands into the component). See `02-interface-spec.md` §2.
- **Bridge**: a daemon that couples one physical device (or device family) to the
  bus, and translates between the device-native protocol and the canonical slot
  topics.
- **Shelly Gen2+**: the second-plus generation of Shelly smart-relay devices
  (Plus/Mini/Pro families). They speak MQTT natively as *peers on the same broker*.
  Each publishes its own state under a device-id topic prefix and accepts JSON
  RPC (remote procedure call) commands over an MQTT topic.

---

## 1. Position in the system

### 1.1 The two slots

The bridge fronts exactly two plugs. Each plug becomes one *site-level* power slot.
Site level means the slot address is `muehle/power/<slot>` — the station segment is
literally `power`. The reason: these supplies sit **outside and upstream of both the
HF and UHF station paths**. HF (high frequency) and UHF (ultra high frequency) are
the two radio service bands the station works in. Here they are only names for the
station's two equipment sets, `muehle/hf/...` and `muehle/uhf/...`. The 13.8 V PSU
(power supply unit) feeds equipment in both sets, and the
master mains plug feeds everything. The plugs are not part of any single station's
equipment chain. That is why they live one level above `muehle/hf/...` and
`muehle/uhf/...`.

| Slot address | Physical device | Meaning |
|---|---|---|
| `muehle/power/master` | Shelly Plus 1PM | Station **master mains** — the upstream mains plug for the whole station. Top of the power tree. Its `feeds` list is empty by design. |
| `muehle/power/psu-13v8` | Shelly Plus 1PM | The plug feeding the **13.8 V DC PSU** — a site-level DC rail that feeds the HF and UHF equipment. That equipment: radios, antenna tuner, antenna controller, antenna switch, rotator, relay controllers. It declares a `feeds` list so consumers can reason about what goes dark if the rail drops. |

Both plugs are physically located at the site labeled `bauwagen` and speak Wi-Fi to
the broker. The bridge process runs on the Raspberry Pi host `shari`.

### 1.2 The `feeds` map (exact seeded value)

`muehle/power/psu-13v8`'s `/meta` capabilities carry this exact eight-entry `feeds`
list (station-relative slot addresses), as seeded by deployment:

```
hf/radio, uhf/radio, hf/tuner, hf/ant-ctrl, hf/ant-switch, hf/rotator, hf/switch, hf/pa-arm
```

`muehle/power/master` publishes **no `feeds` key at all** (omitted from the JSON when
empty — it is the top of the tree and feeds everything implicitly).

Two entries in this list do not match the rest of the station's slot registry. They
are open decisions, not settled fact — see §12 for the evidence:

- `uhf/radio` — there is **no `muehle/uhf/radio` slot anywhere in the system**. The
  UHF station's registered slots are `muehle/uhf/pol-ctrl` and `muehle/uhf/rotator`.
  The feed presumably refers to a UHF radio physically powered by the same rail, but
  no bridge fronts it.
- `uhf/pol-ctrl` — the M5 Stamp PLC (programmable logic controller) #2
  polarization controller. The slot registry includes it and the narrative says
  the rail feeds it, but it is **absent** from the
  seeded list. The HF power amplifier (`hf/pa`) is also absent.

The PRD requires the re-implementation to reproduce the seeded value as-is for
parity. It can correct the list only as an explicit, flagged decision (§12).

### 1.3 Compound bridge — normative warning

This is a **compound bridge**: one operating-system process fronts N plugs (N =
number of configured slot entries. 2 in production), each with its **own MQTT client
connection and its own LWT**. The reason is a protocol constraint the station turned
into a requirement: MQTT 3.1.1 allows only one Will per client connection. The
station further requires that **the death of the bridge process must take every
fronted slot offline simultaneously**. All Wills fire at once, with no window in
which one slot reads online while another reads offline.

**Normative warning:** one process fronts both supplies. So a single crash, hang,
out-of-memory kill, or deployment restart of this process takes **both**
`muehle/power/master` and `muehle/power/psu-13v8` offline at the same instant. There
is no way to take one supply's bridge down without taking the other's with it. This
is a deliberate trade-off (one process to deploy and supervise). Any
re-implementation must either preserve it knowingly or reverse it knowingly (for
example, one process per plug). Do not split the difference. The contract is N
clients with N Wills **in one process**. The alternative is one process per slot
with independently-firing Wills, which changes the observable failure coupling.

A second coupling: the MQTT client identifier. The requirement is **one client id
per slot**, default `<site>-<station>-<slot>` (for example `muehle-power-master`,
`muehle-power-psu-13v8`). If all slots use one shared configured client id verbatim,
the broker kicks the older client whenever a new one connects (MQTT session
take-over). Then the two slots fight — see §11 defect 4.

---

## 2. Upstream interface — the Shelly devices

### 2.1 No direct connection (behavior contract)

The bridge must not open any network connection to a plug. The operator
pre-provisions the Shelly devices out of band as MQTT clients on the same broker.
The devices publish under their device-id prefix. All bridge-to-plug communication
flows through publish/subscribe on the broker:

| Native topic | Direction | Payload | Purpose |
|---|---|---|---|
| `<shelly_id>/status/switch:0` | plug → bridge (subscribed, QoS 1) | JSON (§2.2) | Unsolicited relay-state announcement. The single source of truth for the relay's actual position. |
| `<shelly_id>/online` | plug → broker (subscribed, QoS 1) | Literal string `true` or `false` | Plug heartbeat. `true` periodically while the plug has a connection. `false` on the plug's graceful MQTT disconnect. Drives the `device_online` field. |
| `<shelly_id>/rpc` | bridge → plug (published, QoS 1, not retained) | JSON RPC request (§4.2) | Commands the relay. |
| `<shelly_id>/rpc/rb` | plug → bridge | RPC response | **Not subscribed, not used** (see §4.3). |

`<shelly_id>` is the device's Gen2 MQTT prefix — its stable device id, for example
`shellyplus1pm-aabbccddeeff` — and comes from per-slot config.

The bridge is **entirely push-driven**: it must not poll the plug, must not send
status requests, and must not try to configure the plug. Its announce/heartbeat
periods are device-side settings the bridge neither reads nor writes. See §6.4 for
how the bridge bounds a silent plug anyway.

### 2.2 Native status payload

From `<shelly_id>/status/switch:0` the bridge parses exactly these JSON fields and
**ignores everything else** in the payload:

| Field | Type | Meaning | Use |
|---|---|---|---|
| `output` | bool | Actual relay position. `true` = on. | **The only field that affects behavior** — becomes `/state.power`. |
| `apower` | number (watts) | Apparent power draw. | Parsed, then discarded (known gap, §11 defect 2). |
| `aenergy` | number (watt-minutes) | Cumulative energy. | Parsed, then discarded. |
| `voltage` | number (volts) | Mains voltage. | Parsed, then discarded. |
| `current` | number (amps) | Load current. | Parsed, then discarded. |

The bridge logs malformed payloads (invalid JSON) at WARN and drops them with no
state change. A payload that is valid JSON but lacks `output` decodes as `false` →
power `"off"`. The bridge must treat an announcement without an `output` key as a
report of "relay off" (reference-implementation note: this is Go's JSON zero-value
behavior. The contract keeps it).

---

## 3. Canonical MQTT presence

For each slot (`<slot>` = `master` | `psu-13v8`), the bridge owns these topics.
Topic strings, retention, and QoS are normative.

| Topic | Retained | QoS | Direction | Cadence |
|---|---|---|---|---|
| `muehle/power/<slot>/meta` | yes | 1 | bridge → bus | On every MQTT (re)connect. |
| `muehle/power/<slot>/state` | yes | 1 | bridge → bus | Only when the snapshot changes (change-deduped, §5.3). |
| `muehle/power/<slot>/status` | yes | 1 | bridge and broker (LWT) | `online` by the bridge on every (re)connect. `offline` by the broker through the Will on ungraceful death. |
| `muehle/power/<slot>/cmd` | yes (retained on the bus. Written by other components) | 1 (subscription) | bus → bridge | Subscribed by the bridge. The broker replays the last retained command at every (re)connect — the self-heal mechanism, §4.4. |

Note: the `/cmd` retention is the station's documented **exception**. Commands are
normally *not* retained, but idempotent actuator setpoints like power on/off carry
the retained flag, so the intent survives restarts (see `02-interface-spec.md` §1.5).

The bridge must not publish Home Assistant discovery topics. A separate passive
consumer (`03-components/hadiscovery.md`) renders discovery from the `expose` block.

### 3.1 `/meta` payload (exact shape)

Published retained on every (re)connect. The JSON below shows the PSU slot (`feeds`
present). The master slot is the same minus the whole `feeds` key:

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
- `role`: literal `"power"` (the station model's role taxonomy. These are the only
  two `power`-role slots in the system).
- `device.model` / `device.serial`: from per-slot config. `serial` must be the
  stable Gen2 device id (the same string as `shelly_id`).
- `link`: literal `"wifi"` (the plug's own network link, not for example a serial
  link).
- `location`: free-text physical-location label from per-slot config. The bridge
  omits it when empty. Production value: `bauwagen`.
- `host`: compute node the bridge runs on, from global config. Production value:
  `shari`.
- `capabilities.fail_safe`: `"on"` | `"off"` from per-slot config, default `"off"`.
  Semantics: the plug's power-on default after a mains outage. `"off"` means a
  mains blip drops the station rather than re-energizing it unexpectedly (a
  deliberate safety posture, see `06-safety.md`). Informational metadata only: the
  bridge never programs the plug. The plug's actual out-of-band setting must match
  the published value.
- `capabilities.feeds`: list of downstream slot addresses this supply powers
  (station-relative, for example `hf/radio`). **The bridge omits the key entirely
  from the JSON when the list is empty** — the master slot publishes no `feeds`
  key.
- `expose`: the consumer-neutral field surface (see `02-interface-spec.md` §2.3).
  Exactly one field: `power`, writable, enum `on|off`, with the command descriptor
  `{action:"set_power", value_key:"value", value_type:"string"}`. The argument of a
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

- `ts`: string, RFC 3339, UTC — timestamp of this snapshot. Regenerated per
  publish. Never part of dedup.
- `power`: `"on"` | `"off"` — the **actual relay position** as announced by the
  plug, read back from `<shelly_id>/status/switch:0`. Never an open-loop echo of
  the last command (§4.3).
- `device_online`: bool — plug reachability, heartbeat-based (§6.4). This is the
  second, distinct liveness layer. `/status` says whether the *bridge process* is
  alive. `device_online` says whether the *plug behind the bridge* is alive.
  Consumers must check both (see `02-interface-spec.md` §5). Note: the deployed
  bridge publishes `device_online` explicitly as `true`/`false`. The
  integration-model text describes an "omitted when true" form. Consumers must
  treat both forms as the same (absence = true). See §12.
- `error`: string, present only when non-empty — the reason the bridge marked the
  device offline. One of `"shelly heartbeat lost"` or `"shelly online=false"`. When
  the plug recovers, the bridge clears the key and it disappears from the JSON
  again.

### 3.3 `/status` payload

Literal string `online` or `offline`, retained. The bridge publishes `online` in
its connect handler on every (re)connect. The client registers `offline` as the
Will at connect time. Per slot, the Will topic is the slot's `/status` topic,
payload `offline`, QoS 1, retained = true. The broker publishes it if the process
dies or the client drops.

**Actual deployed behavior (not the idealized claim):** on a *clean* process
shutdown (SIGTERM, graceful MQTT DISCONNECT), the broker does **not** fire the
Will, so the retained `/status` stays `online` for a stopped service. Consumers
must not trust `/status` alone to mean "running right now". It means "running at
last announcement, or stopped cleanly." `02-interface-spec.md` §5.3 documents this
asymmetry station-wide. This component inherits the asymmetry. It did not
introduce it.

---

## 4. Command surface

### 4.1 The one command

The bridge accepts exactly one command, on the slot's `/cmd` topic: JSON with keys
`action` (string) and `value` (string):

```json
{ "action": "set_power", "value": "on" }
{ "action": "set_power", "value": "off" }
```

- `set_power` + `on` → publish the `Switch.Set` RPC with the relay-on parameter.
- `set_power` + `off` → same with relay-off.
- There is no other action or value. The bridge logs unknown actions and unknown
  values (for example `value:"sleep"`) at WARN and silently drops them. No error
  topic, no negative acknowledgment of any kind (§11 defect 9). Malformed `/cmd`
  JSON: same (WARN log, drop).
- **Read-only mode.** When the deployment wires no relay-control surface (a nil
  `Commander` in the reference code), the bridge also drops a *valid* command.
  The bridge logs it at WARN with `cmd: no commander configured`. This is a read-only deployment
  affordance. The shipped build always wires the commander, and no config key
  selects this mode.
- The bridge must not amplify the command surface: the only thing it ever sends
  the plug is the exact `Switch.Set` RPC of §4.2.

Many writers can publish `/cmd` (the sequencer, operator consoles, the
home-automation hub as one writer among many). The last retained write wins. The
bridge is the only writer of the plug's RPC topic.

### 4.2 RPC payload (exact bytes)

Published to `<shelly_id>/rpc`, QoS 1, not retained:

```json
{"id":1,"src":"shelly-power-bridge","method":"Switch.Set","params":{"id":0,"on":true}}
```

- `id`: fixed `1` (an RPC call counter that the code never increments — irrelevant
  because the bridge does not use the responses).
- `src`: fixed string `"shelly-power-bridge"` — visible to the plug. Keep it
  identifiable.
- `method`: `Switch.Set`. `params.id`: fixed `0` (switch component 0 — the single
  relay of a Plus 1PM). `params.on`: `true`/`false` per the requested state.

If the bridge's MQTT client is not connected at dispatch time, the command fails
with `"mqtt client not connected"` (WARN log) and the bridge drops it. But `/cmd`
is a retained message, so the broker replays it on the next reconnect. The intent
thus survives bridge or broker restarts (§4.4).

### 4.3 Fire-and-observe confirmation (behavior contract)

The bridge must not update `/state` optimistically after it issues a command. It
must not subscribe to or interpret the RPC response topic `<shelly_id>/rpc/rb`.
Confirmation is **fire-and-observe**: the bridge waits for the plug's own
`<shelly_id>/status/switch:0` announcement to reflect the new relay position and
only then publishes the changed `/state`. Consequences, all normative:

- A command that the broker accepts but the plug does not execute changes nothing
  on `/state`.
- A failed switch (for example a relay fault) shows only as "the announced state
  never changes". Consumers can detect it by comparing the `/cmd` intent against
  `/state.power`. The bridge does not signal it (§11 defect 10).
- `/state.power` always equals the last *observed* relay position, never the last
  *commanded* one.

### 4.4 Retained `/cmd` as steady-state intent (behavior contract)

`/cmd` is a retained message and is the steady-state intent for the plug. On every
bridge
(re)connect — including subscribe time after a restart — the broker replays the
last retained command, and the bridge re-applies it. So the plug converges to the
intended state even after a bridge crash, broker restart, or network gap. The
bridge must not clear or use up (delete) the retained command. This makes the
power slots self-healing without any persisted bridge-side state.

---

## 5. Behavior and state machine

### 5.1 Startup

1. Load the TOML config (TOML: a standard key-value config file format. Path
   from flag, default
   `/etc/shelly-power-bridge/config.toml`), apply environment overrides, and
   validate. A config file that the code cannot read, a TOML decode error, or a
   validation failure → message to stderr, **exit code 2**. A *missing* config
   file is not an error (the code then uses built-in defaults, but validation then
   fails for lack of slot entries — exit 2).
2. Bind a cancellation context to SIGINT/SIGTERM.
3. Spawn one independent worker per configured slot. Slots run fully
   independently. The process exits only when **all** workers return, and the
   code reads slot errors only after that. A healthy worker returns only when
   the shutdown signal cancels the context. So the exit semantics are **wait-all**,
   not fail-fast: if one slot's **first** MQTT connect fails while another slot
   stays connected, the process does **not** exit. It keeps running the healthy
   slot and leaves the failed slot dark until SIGTERM. No exit code 1 occurs,
   and the supervisor never restarts. In practice both slots share one broker,
   so the common failure mode is both slots failing together. Then all workers
   return at once, the process exits with **exit code 1**, and the service
   supervisor restarts the whole process after **5 s** (`Restart=on-failure`,
   `RestartSec=5`). See §12 for the wait-all versus fail-fast decision.
4. The shutdown signal must be able to interrupt the first connect. If SIGTERM
   arrives while the broker is unreachable, the connect stops immediately rather
   than hanging until the supervisor's stop timeout escalates to a force kill.
   (Derived from a live incident in a sibling component. Stated as a
   library-independent requirement.)
5. Per slot, before connecting, build: the serialized state worker (§5.4), the
   heartbeat staleness watcher (§6.4), and the bridge state object with state
   `power:""`, `device_online:false`. The bridge publishes **nothing** until the
   first real event arrives. The very first snapshot always differs from the empty
   last-published one. So the first native announcement or heartbeat publishes
   immediately.

### 5.2 Connect / reconnect (per slot)

On every successful (re)connect, in this order:

1. Publish `online` (retained, QoS 1) to the slot's `/status`.
2. Mark the bridge connected — the bridge can then publish `/state`.
3. Publish `/meta` (retained).
4. Subscribe (QoS 1) to `<shelly_id>/status/switch:0`, `<shelly_id>/online`, and
   the slot's `/cmd`. The broker replays the retained `/cmd` at subscribe time and
   the bridge re-applies it — the self-heal path (§4.4).

MQTT session parameters: `CleanSession=false` (the broker keeps the client's
session state, including subscriptions, across reconnects), auto-reconnect enabled, MQTT
keep-alive default 30 s (a library default. Not load-bearing).

On connection loss: log at WARN, mark the bridge disconnected, and **stop
publishing `/state`** (the Will already announced `offline`. Publishing state into
a dead session is wrong). Hold received state in memory and publish it on the next
change after reconnecting. Re-establishment is automatic, and the bridge adds no
explicit backoff of its own.

### 5.3 Normal operation and change-dedup

- `<shelly_id>/status/switch:0` message → parse it. On success: set `power` from
  `output`, set `device_online=true`, clear `error`, and publish `/state` **if the
  snapshot changed**. A native announcement is proof of reachability, so it also
  *revives* `device_online`. Recovery comes only from this path or a heartbeat
  `true`.
- `<shelly_id>/online` message → payload exactly `true`: record the heartbeat
  timestamp and refresh `device_online=true`. Note: this bullet states the
  recommended fixed behavior, not legacy parity. The legacy true-handler also
  sets `power:"on"` (§11 defect 1). Whether to fix it is an open decision
  (§12 item 4). Payload exactly `false`: mark the
  device offline with error `"shelly online=false"` and publish a changed `/state`
  with `device_online:false`. Any other payload falls into the `false` branch
  (only the literal `true` is special-cased).
- `/cmd` message → parse and dispatch (§4).

**Change-dedup (exact rule):** only three fields drive the comparison — `power`,
`device_online`, `error`. The code regenerates `ts` per publish and never compares
it. The bridge publishes a snapshot only when one of those three changes.
The same consecutive snapshots stay unpublished. Corollary: after a broker
reconnect the retained `/state` copy still serves. The code publishes a snapshot
that changed during the disconnect only when the next event arrives. That is a
freshness gap, not a correctness break — see §11 defect 8.

### 5.4 Serialized state path (library-independent constraint)

All state mutation and all publishing for a slot must flow through a single
serialized path, decoupled from the MQTT receive path. Inbound message handlers
only enqueue work items onto a per-slot bounded queue (reference implementation:
capacity 64) and never publish or block. Rationale: in the reference MQTT library,
incoming handlers run on the connection's dispatch thread. A handler that blocks
or publishes synchronously deadlocks the client — this exact defect took down a
sibling component in production. Any stack must preserve the decoupling, whatever
its threading model.

When the queue is full, the code drops incoming work and never blocks on it
(protecting the receive path is the point of the bound). The bridge recovers a
dropped command through the retained-`/cmd` replay on the next reconnect. It
recovers a dropped telemetry event through the plug's next announcement. The
reference implementation drops silently and without a count. We recommend that a
re-implementation at least count and log drops (§11 defect 6).

### 5.5 Shutdown

On SIGINT/SIGTERM: cancel the context. Stop a pending first connect (§5.1 step
4). Disconnect each slot's client gracefully with a 500 ms quiesce allowance. A
graceful DISCONNECT suppresses the Will, so a clean stop does **not** flip
`/status` to `offline` (it stays retained-`online`, §3.3). Stop the workers. Log
`shelly-power-bridge stopped`. Exit **0**.

### 5.6 Error-path summary

| Condition | Effect |
|---|---|
| Config unreadable (other than missing) / TOML decode error / validation failure | stderr message, exit 2. |
| First MQTT connect failure (all slots) | Every worker errors → process exit 1 → supervisor restart after 5 s. |
| First MQTT connect failure (one slot, another stays connected) | No exit. The process keeps running the healthy slot. The failed slot stays dark until SIGTERM (wait-all semantics, §5.1 step 3). |
| Broker disconnect after the first connect | Log at WARN. `/state` publishing stopped. Auto-reconnect. The broker fires the Will (`/status` = `offline`). |
| Malformed native status / malformed `/cmd` / unknown cmd action or value | Log at WARN, no state change, message dropped. |
| `Switch.Set` publish fails (client disconnected mid-dispatch) | Log at WARN. The retained `/cmd` replay re-drives it after reconnect. |
| Jobs queue full | The code silently drops the incoming event (state can lag until the next event). |

---

## 6. Liveness — the two layers

### 6.1 Layer 1: bridge liveness (`/status`)

LWT per slot, registered at connect: topic `muehle/power/<slot>/status`, payload
`offline`, QoS 1, retained. The bridge republishes `online` on every (re)connect.
Process death fires **every** slot's Will at once (§1.3). Clean shutdown leaves
`online` in place (§3.3).

### 6.2 Layer 2: plug liveness (`/state.device_online`)

Driven by the plug's own heartbeat (`<shelly_id>/online`) and native announcements:
heartbeat `true` or any announcement → online. Heartbeat `false` or staleness →
offline. The two layers must never read as one. The bridge can be perfectly alive
while the plug is dark (Wi-Fi outage). And the plug can announce while the bridge
is down (nobody reads the announcements). Consumers must AND both — see
`02-interface-spec.md` §5 for the station-wide rule and the incident that
motivated it.

### 6.3 Unknown must not read as online

A plug that the process never heard from since start must read
`device_online:false` within the first staleness tick (≤10 s of startup), not stay
at a default-online value. The state starts offline until the plug proves
otherwise.

### 6.4 Heartbeat staleness watcher (exact timing)

- Per slot, a ticker fires every **10 s** (default. Not configurable now).
- On each tick, the watcher checks the time since the last `<shelly_id>/online` =
  `true` heartbeat. If that exceeds **75 s**, the watcher marks the device offline
  with error `"shelly heartbeat lost"` and publishes a changed `/state`. Dedup
  means only one publish. The watcher keeps ticking while stale but adds nothing.
- If no heartbeat has ever arrived since process start, the elapsed time counts as
  **1 hour**. That is, the never-heard case trips the threshold on the first tick
  (§6.3).
- The watcher only marks offline, never online. Recovery needs a native
  announcement or a heartbeat `true`.
- The 75 s threshold is a **default tuned against the plug's own device-side
  heartbeat cadence**, which the bridge does not set or query. The bridge's config
  does not expose it (§11 defect 7). If the plug's announce period is ever
  lengthened past 75 s, the slot flaps offline.

---

## 7. Invariants (testable requirements for any re-implementation)

1. `/state.power` must always equal the actual relay position read back from the
   plug's native announcement, never the last commanded value. No optimistic
   state.
2. The `fail_safe` default must be `off`: after a mains outage the station stays
   off until someone explicitly re-energizes it. The bridge publishes the
   configured value as metadata but never programs the plug. The plug's
   out-of-band power-on default must match the published metadata.
3. Process death must flip every fronted slot to `/status` = `offline`
   simultaneously — one Will per slot, all registered at connect time by the same
   process.
4. `/status` (bridge liveness) and `/state.device_online` (plug reachability) must
   stay distinct fields with distinct drivers.
5. `/cmd` must stay retained and carry steady-state intent: the broker replays it
   and the bridge re-applies it on every reconnect. The bridge never clears it.
6. No command amplification: the bridge acts only on `set_power` with value
   `on|off`, and drops everything else. The only plug-bound message is the exact
   `Switch.Set` RPC of §4.2.
7. The bridge must not publish `/state` while the slot's MQTT client has no
   connection.
8. An unknown plug must not read as online (§6.3).
9. Inbound MQTT handling must stay decoupled from outbound publishing (§5.4). A
   blocked receive handler deadlocks the client. This happened live in a sibling
   component.
10. A secret must never appear in the config file, unit file, or process command
    line. Secrets arrive through an environment file (§8.2).
11. A clean SIGTERM shutdown must disconnect gracefully (Will suppressed,
    `/status` stays `online`) with a bounded quiesce (500 ms). Only ungraceful
    death fires the Will.
12. Exit codes must be: 2 = configuration/validation error, 1 = runtime failure
    (first connect), 0 = clean stop.

---

## 8. Configuration

### 8.1 Keys

TOML file, default path `/etc/shelly-power-bridge/config.toml`, override with
`-config <path>`. A missing file is not an error (defaults apply — and then
validation fails for lack of slots). The code silently ignores unknown keys. Load
order: defaults → TOML → environment overrides → `-log.level` flag
(case-insensitive. It overrides the config). Levels: `debug` | `info` | `warn` |
`error`, default `info`, to stderr, text format.

Global keys:

| Key | Default | Meaning |
|---|---|---|
| `host` | `shari` | Compute-node name, published in `/meta.host`. |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | Broker URI (see §12 on the planned broker migration). |
| `mqtt.client_id` | `""` | Empty = per-slot id `<site>-<station>-<slot>`. If non-empty, the code uses it for **all** slots — a collision trap (§1.3, §11 defect 4). The empty default is the safe, production value. |
| `mqtt.user` | `hf` | MQTT username. |
| `mqtt.password` | `""` | Overridden by environment in practice. The secret is never kept in the TOML. |
| `mqtt.site` | `muehle` | Site prefix of every slot address. Mandatory. |
| `mqtt.discovery_prefix` | `homeassistant` | Carried in config, published nowhere by this bridge (rendered by the separate hadiscovery consumer). Effectively inert here. |
| `log.level` | `info` | As above. |

Per-slot keys (one `[[slot]]` table per plug):

| Key | Default | Meaning |
|---|---|---|
| `station` | — (required) | Middle path segment. Site-level power slots use `power`. |
| `slot` | — (required) | Last path segment: `master`, `psu-13v8`. |
| `location` | `""` | Free-text label → `/meta.location`, omitted when empty. |
| `device_model` | `""` | → `/meta.device.model` and `expose.device.name`/`model`. |
| `device_serial` | — (required) | → `/meta.device.serial`. Must equal the Gen2 device id. |
| `shelly_id` | — (required) | The Gen2 MQTT prefix. It determines all native topics (§2.1). |
| `fail_safe` | `off` | `"on"` or `"off"` only (validated) → `/meta.capabilities.fail_safe`. |
| `feeds` | absent | List of downstream slot addresses → `/meta.capabilities.feeds`, omitted when empty. |

Validation failures (exit 2): no site, no broker, no slot entries, empty
station/slot/shelly_id/device_serial, `fail_safe` not in {`on`,`off`}, a repeated
slot address. Per-slot values come from the TOML only. They have no environment
overrides.

### 8.2 Secrets

Environment overrides applied after TOML (used by the supervisor's environment
file): `SHELLY_POWER_BRIDGE_MQTT_BROKER`, `SHELLY_POWER_BRIDGE_MQTT_CLIENT_ID`,
`SHELLY_POWER_BRIDGE_MQTT_USER`, `SHELLY_POWER_BRIDGE_MQTT_PASSWORD`,
`SHELLY_POWER_BRIDGE_MQTT_SITE`. Empty env values do not override. The MQTT
password lives in `/etc/shelly-power-bridge/shelly-power-bridge.env` (mode 0600,
owned by the service user) as `SHELLY_POWER_BRIDGE_MQTT_PASSWORD=...`. The
service definition injects it. It never appears in the TOML, the unit file, or
the process command line. This is the station-wide convention (see
`05-deployment-ops.md` §3).

---

## 9. Deployment

Target: Raspberry Pi `shari` at `192.168.1.139`, SSH user `io`. The reference
deploy script performs the whole flow (overridable through env vars `SSH_HOST`,
`SSH_USER`, `SERVICE_NAME`, `SERVICE_USER`, `INSTALL_DIR`, `HOST_NAME`,
`LOG_LEVEL`, `MQTT_BROKER`, `MQTT_SITE`, `MQTT_USER`, `MQTT_PASSWORD`,
`MASTER_SHELLY_ID`, `PSU_SHELLY_ID`):

1. Cross-compile a static `linux/arm64` binary into `dist/`.
2. Generate a **seed** config and a **seed** environment (secrets) file locally —
   temp files, 0600 umask. The secret file travels separately from the config.
3. Generate the service unit (§9.1).
4. Copy binary, unit, and seeds to the Pi. On the Pi: create a dedicated system
   user `shelly-power-bridge` (no login, no home) if missing. Create
   `/etc/shelly-power-bridge`. **Seed-once**: install the config and env file only
   if they do not already exist (0600, service-user owned) — later deploys never
   overwrite them, the device owns its settings. Stop the service. Move the binary
   into `/opt/shelly-power-bridge/`. Install the unit, reload the supervisor,
   enable and restart, and print status.
5. The seed config hard-codes the two `[[slot]]` blocks (master, psu-13v8, with
   the `feeds` list of §1.2). The operator must set `MASTER_SHELLY_ID` and
   `PSU_SHELLY_ID` to the real device ids before the first deploy. But note: the
   reference script does NOT actually refuse its placeholder defaults
   (`shellyplus1pm-aabbccddeeff`, `shellyplus1pm-112233445566`) despite its
   documentation claiming it does. It only prints a warning to check. A
   re-implementation must make a first deploy refuse, or ask for confirmation of,
   placeholder ids (§11 defect 5). A first deploy with placeholders silently binds
   to nonexistent devices (all telemetry silent, all slots `device_online:false`).

Runtime dependencies: only network access to the MQTT broker. The plugs must be
pre-provisioned out of band to speak MQTT to the same broker under their device-id
prefix — the bridge cannot configure them.

### 9.1 Service unit (reference settings, normative posture)

- ExecStart: `/opt/shelly-power-bridge/shelly-power-bridge -config /etc/shelly-power-bridge/config.toml`
- EnvironmentFile: `/etc/shelly-power-bridge/shelly-power-bridge.env`
- `Type=simple`, `Restart=on-failure`, `RestartSec=5`, after/wants network-online.
- Runs as dedicated user/group `shelly-power-bridge` (`ConfigurationDirectory` and
  `StateDirectory` = `shelly-power-bridge`).
- Hardening: `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
  `PrivateDevices=true` (network-only service — no devices needed),
  `ProtectKernelTunables/Modules`, `ProtectControlGroups`,
  `RestrictAddressFamilies=AF_INET AF_INET6`, `RestrictNamespaces`,
  `LockPersonality`, `RestrictRealtime`, `RestrictSUIDSGID`, `RemoveIPC`,
  empty capability sets, `ReadWritePaths=/var/lib/shelly-power-bridge`.
- Resource ceilings: `MemoryMax=256M`, `TasksMax=64` — the Pi hosts all station
  services. A leak in one must not starve the others (compound-bridge corollary of
  §1.3: exceeding the ceiling takes **both** slots offline at once).
- Logs to the system journal, identifier `shelly-power-bridge`.

Full deployment conventions: `05-deployment-ops.md`.

---

## 10. Consumers and safety context

- `powerseq` (see `03-components/powerseq.md`) is the primary automated consumer: it
  drives the startup/shutdown sequence over these two slots' `/cmd` topics and reads
  their `/state` to confirm ordered power transitions.
- The operator console (see `04-console.md`) displays both slots and offers switching.
- The `feeds` metadata lets any consumer compute blast radius. If `psu-13v8` drops,
  everything in its `feeds` list goes dark (most of those devices then also lose
  their bridges' device links — a cascading pattern consumers must expect).
- Because `master` is the upstream of `psu-13v8` itself, switching `master` off
  takes the PSU (and all its feeds) dark too. The `fail_safe=off` posture means
  neither supply re-energizes on mains restoration without an explicit command.
  See `06-safety.md` §2 for the station-level rule: humans or the sequencer, never
  the plugs themselves, decide when power returns.

---

## 11. Known defects and fragilities of the legacy system

A re-implementation must treat items 1 and 2 as **decisions to make explicitly**
(fix or preserve for parity), and the rest as known hazards to design against.

1. **Heartbeat `true` falsifies `power` (the heartbeat false-on defect).** The
   `<shelly_id>/online` = `true` handler refreshes the *whole* telemetry snapshot,
   including `power` (it sets it to `"on"`) — not just `device_online`. If the
   relay is actually off, the next periodic heartbeat publishes `/state` with
   `power:"on"` — a wrong value. It stays until the plug's next native status
   announcement corrects it. The announcements are periodic but sparse, so the
   window can be minutes. The legacy comment calls it "a no-op if power
   unchanged," which is only true when the relay is on. We recommend that a
   re-implementation make the heartbeat refresh only `device_online`/`error` and
   leave `power` untouched. Flag the behavioral change if made.
2. **The code discards parsed metering (the metering gap).** The Plus 1PM reports
   `apower`/`aenergy`/`voltage`/`current` in every native announcement. The bridge
   decodes them and publishes none of them — the canonical `/state` has no
   power/energy fields. The parse already exists in the legacy code. So closing
   the gap is cheap. The PRD leaves it an open decision (§12) whether `/state`
   gains metering fields. That is a change to the `/state` schema — coordinate
   with `02-interface-spec.md`.
3. **Committed build artifacts.** A compiled binary sits in the project directory
   in the legacy repo. This is hygiene only, not behavior.
4. **Shared `client_id` is a collision trap.** Setting `mqtt.client_id` non-empty
   makes all slots share one MQTT client id. The broker kicks the older client on
   each connect, and the slots fight over the session. The default (empty) is
   safe. We recommend that a re-implementation suffix a configured id per slot, or
   reject a non-empty shared id.
5. **Deploy script does not refuse placeholder Shelly ids** (§9 step 5) — a
   docs-vs-code disagreement in the legacy repo. The code seeds the placeholders
   unverified.
6. **Silent event drop under load.** The bounded queue (§5.4) drops on overflow,
   unlogged and uncounted. This is acceptable at this scale. Count and log the
   drops.
7. **Staleness threshold coupled to invisible device config.** The 75 s heartbeat
   timeout (§6.4) assumes the plug's device-side heartbeat period. The bridge
   neither queries nor configures it, and the bridge's config does not expose the
   75 s value. Lengthen the plug's period past 75 s and the slot flaps offline.
8. **`/state` is not refreshed on reconnect.** After a broker reconnect the bridge
   republishes `/meta` only. A state change during the disconnect waits for the
   next event. This is a minor freshness gap (in practice the heartbeat path
   usually republishes soon after).
9. **No command feedback.** The bridge drops invalid `/cmd` actions/values with
   only a log line. Nothing on the bus tells the writer that the bridge ignored
   the message.
10. **No RPC response/error consumption.** The bridge ignores the `Switch.Set`
    reply topic and the plug's error fields (deliberately, §4.3). Consumers can
    only detect a failed switch as "announced state never changes."

---

## 12. Open decisions and unresolved facts

1. **The `uhf/radio` mystery feed.** The seeded `feeds` list for `psu-13v8` (the
   same in `config.example.toml` and the deploy script's seed generator) holds
   `uhf/radio`. But no `muehle/uhf/radio` slot exists anywhere in the system — the
   UHF station's registered slots are `uhf/pol-ctrl` and `uhf/rotator` only. The
   legacy narrative says the rail feeds "the radios" of both stations. Variants:
   (a) a UHF radio exists physically but has no bridge/slot, and the feed refers
   to it. (b) The entry is stale or wrong. Evidence points both ways: the entry
   appears in every config artifact (so it is deliberate, not a typo), while the
   slot registry contradicts it. Unresolved — the operator must confirm what the
   13.8 V rail physically feeds. For parity, a re-implementation must reproduce
   the list as-is unless the operator resolves it.
2. **Feeds list omissions.** The seeded list omits `uhf/pol-ctrl` (PLC #2, a
   registered slot the narrative says the rail feeds — though note the PLC #2
   firmware itself is a documented repo gap) and `hf/pa` (the HF power amplifier).
   Possibly the PA takes mains power directly (it is on the `master` tree), and
   the site added PLC #2 after someone wrote that list. Unresolved. The same
   parity guidance as item 1 applies.
3. **Metering in `/state` (defect 2).** Publish `apower`/`aenergy`/`voltage`/`current`
   to `/state` (or a sibling topic), or keep discarding them? The device gives the
   data, and the legacy code already parses it, but the deployed `/state` schema
   has none of it. Any addition changes the `/state` contract. The people that
   add the fields must coordinate with `02-interface-spec.md`.
4. **Heartbeat false-on fix (defect 1).** Preserve the legacy falsifying behavior
   exactly (parity) or fix the handler to refresh only `device_online`? The PRD
   recommends the fix, but it changes observable behavior (heartbeat no longer
   publishes `power:"on"`) and must be a flagged decision, not an accident.
5. **`device_online` wire form.** Deployed bridges publish `device_online:true`
   explicitly. The integration-model document describes an "omitted when true"
   form. The PRD's cross-document position: consumers must treat both forms as the
   same (absence = true), or the bus must mandate explicit-true. Unresolved
   station-wide — see `02-interface-spec.md` §7.3.
6. **Broker topology.** All deployed defaults point at `192.168.1.50:1883` (the
   shack broker). A migration to a broker on shari (`192.168.1.139`) exists on an
   unmerged, undeployed feature branch as of 2026-08-29. This document gives the
   `192.168.1.50` production topology. The migration is a deployment decision,
   not a component contract — see `05-deployment-ops.md`.
7. **Real Shelly device ids.** The example/seed configs carry placeholder ids
   (`shellyplus1pm-aabbccddeeff`, `shellyplus1pm-112233445566`) and example
   serials (`shellyplus1pm-A1`, `shellyplus1pm-B2`). The real production ids live
   only in the seeded config on shari (`/etc/shelly-power-bridge/config.toml`).
   We were unable to read them from the workstation when writing this PRD. A
   re-implementation must get them from the operator or the live host. Do not
   copy the placeholders.
8. **The slot-error exit decision: wait-all or fail-fast.** The legacy process
   waits for all slot workers and only then returns the first collected error
   (§5.1 step 3). So a single failed slot leaves the process running with that
   slot dark. A re-implementation must choose: preserve the wait-all semantics,
   or fail fast on the first slot error. A fail-fast exit changes observable
   behavior — the healthy slot's Will fires and its `/status` flips to
   `offline` on an error that today leaves it untouched. Flag the choice either
   way.

---

## 13. Reference-implementation notes (non-normative)

The legacy component is a Go daemon (`shelly-power-bridge/` in the stationa
monorepo). It uses the paho MQTT client and a shared station library for
connection setup and topic/command helpers. Implementation details that are *not*
part of the contract: Go, paho, goroutine structure, TOML parsing library, log
format. The ctx-aware `Connect()` workaround (a goroutine and select that bridge
the blocking connect against the shutdown signal — the *behavior* of §5.1 step 4
is the contract, not the mechanism). The jobs-channel pattern (any mechanism that
satisfies §5.4 is fine). How the code stores the heartbeat clock and the dedup
state. JSON field order/whitespace (consumers parse JSON. Do not change field
names or types).
Whether the RPC response topic is ever subscribed (today it deliberately is not).
The 10 s tick, 75 s threshold, 500 ms quiesce, 5 s supervisor restart, and queue
depth 64 are contract values (the exact queue depth is a free bound). The
component publishes `Switch.Set` with `src:"shelly-power-bridge"` visible to the
plug. A re-implementation must keep a similarly identifiable source string.