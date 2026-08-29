# Research spec — shelly-power-bridge

Source analyzed (code is truth; this file derived from it): `shelly-power-bridge/` in the
stationa monorepo — `cmd/shelly-power-bridge/main.go`, `internal/shelly/shelly.go`,
`internal/bridge/bridge.go`, `internal/bridge/publish.go`, `internal/config/config.go`,
`deploy.sh`, `config.example.toml`, `docs/shelly-power-bridge-mqtt-api.md`,
`CLAUDE.md`, `README.md`, plus the shared modules `shared/schema` and `shared/mqtt`
it imports. The README/CLAUDE.md and code agree on everything material; disagreements
are called out inline below.

---

## 1. Purpose & role

A **smart plug** (here: a Shelly Gen2+ "Plus 1PM" Wi-Fi relay with power metering) is a
mains relay that can be switched on/off over the network. This component —
**shelly-power-bridge** — is a software daemon that fronts two such plugs on behalf of a
ham-radio station ("Mühle") automation system, exposing them on a shared MQTT message
bus so that other station software (a startup/shutdown sequencer called `powerseq`,
operator consoles) can read and switch the station's power supplies directly.

Glossary of terms used throughout:

- **Ham radio / station**: the amateur-radio installation this serves. No radio-domain
  knowledge is needed; the plugs just switch mains power.
- **MQTT**: a lightweight publish/subscribe protocol. Clients publish messages to
  named **topics** (slash-separated strings) and subscribe to topics. A central
  **broker** (at `192.168.1.50:1883`, plain TCP, MQTT 3.1.1) routes messages.
  **Retained** messages: the broker stores the last message per topic and re-delivers
  it to any new subscriber. **LWT** (Last Will and Testament): a message the broker
  publishes on a client's behalf if that client drops without disconnecting.
  **QoS 1**: at-least-once delivery with acknowledgment.
- **Slot**: the station model's unit of addressing — a topic stem
  `<site>/<station>/<slot>`, here always site `muehle`. Each slot has four topic
  suffixes: `/meta` (static identity), `/state` (live snapshot), `/status`
  (bridge liveness, LWT-driven), `/cmd` (commands into the component).
- **Shelly Gen2+**: Shelly devices of the second-plus generation (Plus/Mini/Pro). They
  are *themselves MQTT clients on the same broker*, natively publishing their state and
  accepting RPC commands over MQTT. The bridge is a translator between the Shelly's
  native topics and the station's canonical slot topics.

The bridge fronts exactly two plugs today, each becoming one site-level slot:

| Slot address | Physical device | Meaning |
|---|---|---|
| `muehle/power/master` | Shelly Plus 1PM (master mains plug) | Station master mains — the upstream supply for the whole station. Top of the power tree; `feeds` empty. |
| `muehle/power/psu-13v8` | Shelly Plus 1PM (PSU plug) | A 13.8 V DC power-supply rail feeding both the HF and UHF radio equipment (radios, antenna tuner, antenna controller, antenna switch, rotator, two relay controllers). Declares `feeds` so consumers know what goes dark if this supply drops. |

It is a **compound bridge**: one process fronts N plugs (N = number of `[[slot]]`
config entries, here 2), each with its *own* MQTT client connection and LWT. This is a
behavior contract: MQTT 3.1.1 allows one Will per client, and the requirement is that a
process death must take **every** fronted slot offline simultaneously (all Wills fire at
once) with no stale-online gap.

The bridge is **read-write**: it observes the plug's native relay-state announcements
and commands the relay. Historically these plugs were controlled only via Home
Assistant (a home-automation hub); this bridge moves them onto the canonical bus so HA
is just one writer among many.

---

## 2. Upstream interface — the Shelly devices

**There is no direct network connection to the plug.** The bridge never opens a socket
to the Shelly; all communication is via the shared MQTT broker. The Shelly is a peer
MQTT client on the same broker. "Talking to the Shelly" = subscribing to and publishing
its native topics. This is a behavior contract.

Transport specifics (contract):

- Broker URI: from config, default `tcp://192.168.1.50:1883` (plain MQTT 3.1.1, no TLS).
- Authentication: MQTT username/password (default user `hf`; password via environment,
  never in the config file).
- Per slot: one MQTT client, client id defaulting to `<site>-<station>-<slot>`
  (e.g. `muehle-power-master`, `muehle-power-psu-13v8`), or a shared configured
  `client_id` with per-slot suffix NOT applied (see Known defects #4).
- `CleanSession=false`, auto-reconnect enabled, keepalive default of the paho client
  library (30 s — implementation detail of the library, not set explicitly).

Native topics per plug, with `<shelly_id>` = the device's Gen2 MQTT prefix (its device
id, e.g. `shellyplus1pm-aabbccddeeff`), from config:

| Topic | Direction | Payload | Purpose |
|---|---|---|---|
| `<shelly_id>/status/switch:0` | Shelly → bridge (subscribed, QoS 1) | JSON, see below | Relay-state announcement; the single source of truth for the plug's actual relay position. |
| `<shelly_id>/online` | Shelly → broker (subscribed, QoS 1) | Literal string `true` or `false` | Heartbeat. `true` periodically while the plug is connected; `false` published on the plug's graceful MQTT disconnect. The bridge uses it for `device_online` liveness. |
| `<shelly_id>/rpc` | bridge → Shelly (published, QoS 1, not retained) | JSON RPC request | Commands the relay. |
| `<shelly_id>/rpc/rb` | Shelly → bridge | RPC responses | **Not subscribed, not consumed.** Confirmation is fire-and-observe: the resulting `<shelly_id>/status/switch:0` announcement is treated as the confirmation, not the RPC response. |

`<shelly_id>/status/switch:0` payload fields parsed (all others in the payload are
ignored):

- `output` (bool) — actual relay position; `true` = on. This is the only field that
  affects bridge behavior.
- `apower` (number, watts), `aenergy` (number, watt-minutes total), `voltage` (number,
  volts), `current` (number, amps) — **parsed but discarded**. The canonical `/state`
  carries no metering (see Known defects #2).

Command payload published to `<shelly_id>/rpc` (exact bytes):

```json
{"id":1,"src":"shelly-power-bridge","method":"Switch.Set","params":{"id":0,"on":true}}
```

- `id` is a fixed 1 (an RPC call counter that is never incremented — irrelevant since
  responses are not consumed).
- `src` is the fixed string `"shelly-power-bridge"`.
- `method` is `Switch.Set`; `params.id` is fixed 0 (switch component 0 — the single
  relay of a Plus 1PM); `params.on` is `true`/`false` per the requested state.

Polling vs push: **entirely push.** The bridge never polls the Shelly and never
requests status; it only reacts to the plug's unsolicited announcements. How often the
Shelly publishes `<id>/status/switch:0` and `<id>/online` is device-side configuration
(Shelly `announce_period` / heartbeat settings) — the bridge imposes nothing. Its
staleness detection (§5) is what bounds how long a silent plug can be believed.

Malformed `<id>/status/switch:0` payloads (bad JSON) are logged at WARN and dropped; no
state changes. A missing `output` key decodes as `false` → power `"off"` (Go JSON
zero-value semantics — an empty/garbage-free payload without `output` reports "off";
only invalid JSON errors).

Connection loss to the *broker* is detected by the MQTT client layer (auto-reconnect;
LWT fires broker-side). Loss of the *Shelly* is detected by heartbeat staleness (§5).

---

## 3. MQTT presence

For each slot (`<slot>` = `master` | `psu-13v8`; site `muehle`, station `power`):

| Topic | Retained | QoS | Direction | Cadence |
|---|---|---|---|---|
| `muehle/power/<slot>/meta` | yes | 1 | bridge → bus | Published on every MQTT (re)connect. |
| `muehle/power/<slot>/state` | yes | 1 | bridge → bus | Published only when the snapshot changes (change-deduped). |
| `muehle/power/<slot>/status` | yes | 1 | LWT + connect | `"online"` published by the bridge on every (re)connect; `"offline"` published by the *broker* via the Will if the process dies / client drops. |
| `muehle/power/<slot>/cmd` | **yes** (retained on the bus; written by other components) | 1 (subscription) | bus → bridge | The bridge subscribes; the broker replays the last retained command on every (re)connect — this is the self-healing mechanism, see §4/§5. |

The bridge does **not** publish any Home Assistant discovery topics itself; a separate
component (`hadiscovery`) renders discovery from the `expose` block in `/meta`.

### `/meta` payload (exact shape, retained)

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
- `role`: literal `"power"` (station model's role taxonomy).
- `device.model` / `device.serial`: from per-slot config (`device_model`,
  `device_serial`; serial should be the stable Gen2 device id, i.e. the same string as
  `shelly_id`).
- `link`: literal `"wifi"`.
- `location`: from per-slot config (`bauwagen` = the physical building label; free text).
- `host`: from global config (`shari` — the Raspberry Pi the bridge runs on).
- `capabilities.fail_safe`: `"on"` | `"off"` from config, default `"off"`. Semantics:
  the plug's power-on default after a mains outage; `"off"` means a mains blip drops
  the station rather than re-energizing it unexpectedly. Informational for consumers;
  the bridge never programs the plug.
- `capabilities.feeds`: list of downstream slot addresses this supply powers
  (addresses relative like `hf/radio`). **Omitted entirely from the JSON when empty**
  (the station-master slot publishes no `feeds` key).
- `expose`: the consumer-neutral field surface. Exactly one field, `power`, writable,
  enum `on|off`, with the command descriptor `{action:"set_power", value_key:"value",
  value_type:"string"}` — the argument of a `/cmd` message always rides under the JSON
  key `"value"`, never under a key named after the action.

### `/state` payload (exact shape, retained)

```json
{
  "ts": "2026-07-14T18:42:01Z",
  "power": "on",
  "device_online": true
}
```

- `ts`: string, RFC3339, UTC — timestamp of this snapshot.
- `power`: `"on"` | `"off"` — the **actual** relay position as announced by the plug
  (read back from `<id>/status/switch:0`), never an open-loop echo of the last command.
- `device_online`: bool — plug reachability (heartbeat-based, §5). Distinct from
  `/status`, which is bridge liveness.
- `error`: string, present only when non-empty (e.g. `"shelly heartbeat lost"` or
  `"shelly online=false"`) — reason the device was marked offline. Note: when the plug
  recovers, `error` is cleared and the key disappears again.

Only three fields drive dedup: `power`, `device_online`, `error`. `ts` is regenerated
per publish and never part of the comparison. A snapshot is published only when one of
those three changes; identical consecutive snapshots are not republished.

### `/status` payload

Literal string `online` or `offline`, retained. `online` is published by the bridge in
its MQTT connect handler; `offline` is registered as the Will at connect time (QoS 1,
retained) so the broker publishes it if the process dies.

### `/cmd` — subscribed, retained

See §4. The broker's retained replay of the last command on every reconnect is a
behavior contract (self-healing): it makes the plug converge to the intended state even
after a bridge or broker restart.

### LWT registration (exact)

Per slot, at client construction: Will topic = the slot's `/status` topic, payload
`"offline"`, QoS 1, retained = true.

---

## 4. Command surface

Exactly one command is accepted, via the slot's `/cmd` topic:

```json
{ "action": "set_power", "value": "on" }
{ "action": "set_power", "value": "off" }
```

- Payload is JSON with keys `action` (string) and `value` (string). The argument always
  rides under `value` (station-wide convention; not under a `set_power`-named key).
- `set_power` + `on` → publish the `Switch.Set` RPC with `params.on=true` to
  `<shelly_id>/rpc` (QoS 1, not retained).
- `set_power` + `off` → same with `params.on=false`.
- **No other action or value is accepted.** Unknown actions and unknown values (e.g.
  `value:"sleep"`) are logged at WARN and silently dropped — no error message is
  published anywhere, no negative acknowledgment of any kind.
- Side effects of a successful command: only the RPC publish. The bridge does NOT
  update `/state` optimistically; it waits for the plug's own
  `<id>/status/switch:0` announcement to reflect the new relay position, and then
  publishes the changed snapshot. A command that is accepted by the broker but not
  executed by the plug therefore changes nothing on `/state` (fire-and-observe).
- If the MQTT client is not connected at dispatch time, `SetPower` fails with
  `"mqtt client not connected"`, logged at WARN; the command is otherwise dropped.
  However, because `/cmd` is retained, the broker replays it on the next reconnect, so
  the intent is not lost across disconnects.
- Malformed `/cmd` JSON: logged WARN, dropped.
- The command handler can be absent (`Commander == nil`, read-only deployment): then any
  valid command is logged WARN (`"cmd: no commander configured"`) and dropped. In the
  shipped deployment a Commander is always wired; this is a testing affordance.

The bridge itself is the only publisher of the RPC topic; multiple writers may write
`/cmd` (the sequencer, operators, Home Assistant) — last retained write wins.

---

## 5. Behavior & state machine

### Startup

1. Parse flags (`-config`, `-log.level`), load TOML config + env overrides, validate.
   On config error: print to stderr, exit code **2**. On validation error: same.
2. Create a signal-bound context (SIGINT, SIGTERM).
3. Spawn one goroutine per `[[slot]]` (`runSlot`). Slots are fully independent; the
   process exits only when all have returned, and `run()` reads the error channel
   only after `wg.Wait()`. A healthy slot goroutine returns only on ctx cancel
   (`<-ctx.Done()`), so if ONE slot's initial MQTT connect fails while another
   stays connected, the process does NOT exit — it hangs running the healthy
   slot with the failed slot dark until SIGTERM (no exit 1, no systemd restart).
   In practice both slots share one broker, so the common failure is both
   failing together: all goroutines return, the process exits with code **1**,
   and systemd restarts the whole process (`Restart=on-failure`, `RestartSec=5`).
4. Per slot, before connecting: build the per-slot jobs worker (a single goroutine that
   serializes ALL state mutation and publishing for that slot, fed by a bounded channel
   of capacity **64**; when the buffer is full, incoming work is **dropped**, never
   blocked on — see Known defects #6), the heartbeat staleness watcher, and the bridge
   state object (initial state: `power:""`, `device_online:false` — nothing is
   published until an event arrives; note the very first snapshot is always a "change"
   against the empty last-published, so the first real event publishes).

### Connect / reconnect (per slot)

- Initial connect is context-aware: if SIGTERM arrives while the broker is unreachable,
  the connect is aborted immediately (returns `context.Canceled`), rather than hanging
  until systemd's stop timeout SIGKILLs the process. This is a behavior contract.
- On every successful (re)connect, in this order:
  1. Publish `online` (retained, QoS 1) to the slot's `/status`.
  2. Mark the bridge connected (publishing to `/state` is now allowed).
  3. Publish `/meta` (retained).
  4. Subscribe (QoS 1) to `<shelly_id>/status/switch:0`, `<shelly_id>/online`, and the
     slot's `/cmd`. The retained `/cmd` is replayed by the broker at subscribe time and
     re-applied — this is the self-heal path.
- On connection loss: log WARN; mark the bridge disconnected. `/state` publishing stops
  while disconnected (the LWT already announces `offline`); state received while
  disconnected is held in memory and published on the next change after reconnecting.
  Auto-reconnect handles re-establishment with no explicit backoff logic in the bridge
  (the paho client's built-in reconnect behavior; initial reconnect delay is a library
  default — implementation detail).

### Normal operation (per slot)

- `<id>/status/switch:0` message → parse; on success: set `power` from `output`, set
  `device_online=true`, clear `error`; publish `/state` if the snapshot changed. A
  native announcement is proof the plug is reachable, so it also revives `device_online`.
- `<id>/online` message → payload exactly `"true"`: record heartbeat time, and enqueue a
  telemetry event that refreshes `device_online=true` (a no-op if `power` unchanged);
  payload exactly `"false"`: mark device offline with error `"shelly online=false"`
  (publishes a changed `/state` with `device_online:false`). Any other payload is
  ignored by the online path (not `"true"` and not `"false"` → treated as the
  `"false"` branch, since the code tests only for `"true"` — see Known defects #1 for
  the side effect of the `"true"` branch).
- `/cmd` message → parse and dispatch (§4).

### Heartbeat staleness watcher (exact timing)

- A ticker fires every **10 s** (per slot). On each tick, if the time since the last
  `<id>/online`=`true` heartbeat exceeds **75 s**, the device is marked offline with
  error `"shelly heartbeat lost"` and a changed `/state` is published.
- If no heartbeat has ever been received since process start, the elapsed time is
  treated as **1 hour** — i.e. a never-heard plug is reported `device_online:false`
  within the first 10-second tick, not stuck at a default-online value.
- The watcher keeps firing every 10 s while stale, but the dedup means only one
  `/state` publish results (unless `power`/`error` changes again). Recovery requires
  an actual native announcement or heartbeat `true` — the watcher only marks offline,
  never online.
- The 75 s threshold is chosen against the plug's own heartbeat cadence (device-side
  config); the bridge does not set or query it.

### Shutdown

- On SIGINT/SIGTERM: the signal context cancels; the initial connect (if still
  pending) aborts; each slot disconnects its client with a 500 ms quiesce allowance
  (graceful DISCONNECT, which suppresses the Will — so a clean stop does NOT flip
  `/status` to `offline`); the jobs workers stop; the process logs
  `"shelly-power-bridge stopped"` and exits **0**.

### Error paths summary

| Condition | Effect |
|---|---|
| Config file unreadable (other than missing) / TOML decode error | stderr message, exit 2 |
| Validation failure (no site, no broker, no slots, empty station/slot/shelly_id/device_serial, bad fail_safe, duplicate slot address) | stderr message, exit 2 |
| Initial MQTT connect failure (all slots) | slot goroutines return error → process exits 1 → systemd restart after 5 s. One slot only: process does NOT exit (wg.Wait blocks on the healthy slot) — failed slot stays dark until SIGTERM |
| Broker disconnect after initial connect | WARN log, `/state` publish suppressed, auto-reconnect; Will fires broker-side (`/status` = `offline`) |
| Malformed native status / online not `"true"` handling / malformed `/cmd` / unknown cmd action or value | WARN log, no state change, message dropped |
| `Switch.Set` publish fails (client disconnected mid-dispatch) | WARN log, nothing published; retained `/cmd` replay re-drives it later |
| Jobs buffer full (64 outstanding) | incoming events silently dropped (state may lag until the next event) |

---

## 6. Configuration

TOML file, default path `/etc/shelly-power-bridge/config.toml`, override with
`-config <path>`. A **missing** file is not an error (built-in defaults are used — but
validation then fails for lack of slots). Unknown TOML keys are silently ignored.
Load order: defaults → TOML → environment overrides → `-log.level` flag override.

Global keys:

| Key | Default | Meaning |
|---|---|---|
| `host` | `shari` | Compute node name, published in `/meta.host`. |
| `mqtt.broker` | `tcp://192.168.1.50:1883` | Broker URI. |
| `mqtt.client_id` | `""` | If empty, each slot's client id is `<site>-<station>-<slot>`. If set, it is used for **all** slots (shared-prefix hazard, see Known defects #4). |
| `mqtt.user` | `hf` | MQTT username. |
| `mqtt.password` | `""` | Overridden by env in practice; the secret is not kept in the TOML. |
| `mqtt.site` | `muehle` | Site prefix of every slot address. Mandatory. |
| `mqtt.discovery_prefix` | `homeassistant` | Carried in config, published nowhere by this bridge (used conceptually by the hadiscovery consumer; effectively inert here). |
| `log.level` | `info` | `debug` \| `info` \| `warn` \| `error` (stderr, text format). |

Per-slot keys (one `[[slot]]` table per plug):

| Key | Default | Meaning |
|---|---|---|
| `station` | — (required) | Middle path segment; site-level power slots use `power`. |
| `slot` | — (required) | Last path segment (`master`, `psu-13v8`). |
| `location` | `""` (optional) | Free-text location label, published in `/meta.location` (omitted if empty). |
| `device_model` | `""` | Published in `/meta.device.model` and `expose.device.name`/`model`. |
| `device_serial` | — (required) | Published in `/meta.device.serial`; should equal the Gen2 device id. |
| `shelly_id` | — (required) | The Gen2 MQTT prefix; determines all native topics (§2). |
| `fail_safe` | `off` | `"on"` or `"off"` only (validated); published in `/meta.capabilities.fail_safe`. |
| `feeds` | absent | List of downstream slot addresses; published in `/meta.capabilities.feeds`, omitted when empty. |

Environment overrides (applied after TOML; used by the systemd EnvironmentFile):
`SHELLY_POWER_BRIDGE_MQTT_BROKER`, `SHELLY_POWER_BRIDGE_MQTT_CLIENT_ID`,
`SHELLY_POWER_BRIDGE_MQTT_USER`, `SHELLY_POWER_BRIDGE_MQTT_PASSWORD`,
`SHELLY_POWER_BRIDGE_MQTT_SITE`. Only these; per-slot values come from the TOML only.
Empty env values do not override.

Secrets: the MQTT password lives in
`/etc/shelly-power-bridge/shelly-power-bridge.env` (0600, service-user owned) as
`SHELLY_POWER_BRIDGE_MQTT_PASSWORD=...` and is injected by the systemd unit's
`EnvironmentFile=`. It never appears in the TOML, the unit file, or the process command
line. This is contract (station-wide convention).

Flags: `-config` (default above); `-log.level` (overrides config, case-insensitive).

---

## 7. Deployment

Target: Raspberry Pi `shari` at `192.168.1.139`, SSH user `io`. `./deploy.sh` does the
whole flow (overridable via env vars `SSH_HOST`, `SSH_USER`, `SERVICE_NAME`,
`SERVICE_USER`, `INSTALL_DIR`, `HOST_NAME`, `LOG_LEVEL`, `MQTT_BROKER`, `MQTT_SITE`,
`MQTT_USER`, `MQTT_PASSWORD`, `MASTER_SHELLY_ID`, `PSU_SHELLY_ID`):

1. Cross-compile `linux/arm64` (`CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`) into
   `dist/`.
2. Generate a **seed** config and a **seed** EnvironmentFile locally (temp files,
   0600 umask; the secret file travels separately from the config).
3. Generate the systemd unit (below).
4. `scp` the binary, unit, and seeds to the Pi; remote script: create a dedicated
   system user `shelly-power-bridge` (no login, no home) if missing; create
   `/etc/shelly-power-bridge` (0755, service-user owned); **seed-once**: install the
   config and env file only if they do not already exist (0600, service-user owned) —
   later deploys never overwrite them, the device owns its settings; stop the service,
   move the binary into `/opt/shelly-power-bridge/`, install the unit, `daemon-reload`,
   enable + restart, print status.
5. The seed config hard-codes the two `[[slot]]` blocks (master + psu-13v8, `feeds`
   list fixed as in §1). `MASTER_SHELLY_ID`/`PSU_SHELLY_ID` must be set to the real
   device ids — the default placeholder ids are accepted by the script (despite its
   comment claiming it refuses placeholders — README/deploy-doc vs. code disagreement;
   the script only *prints a warning to verify*).

Systemd unit (exact settings):

- `ExecStart=/opt/shelly-power-bridge/shelly-power-bridge -config /etc/shelly-power-bridge/config.toml`
- `EnvironmentFile=/etc/shelly-power-bridge/shelly-power-bridge.env`
- `Type=simple`, `Restart=on-failure`, `RestartSec=5`
- `After/Wants=network-online.target`, `WantedBy=multi-user.target`
- Runs as dedicated user/group `shelly-power-bridge`; `ConfigurationDirectory` and
  `StateDirectory` = `shelly-power-bridge`
- Hardening: `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
  `PrivateDevices=true` (network-only service, no serial devices),
  `ProtectKernelTunables/Modules`, `ProtectControlGroups`,
  `RestrictAddressFamilies=AF_INET AF_INET6`, `RestrictNamespaces`,
  `LockPersonality`, `RestrictRealtime`, `RestrictSUIDSGID`, `RemoveIPC`,
  `CapabilityBoundingSet=` (empty), `AmbientCapabilities=` (empty),
  `ReadWritePaths=/var/lib/shelly-power-bridge`
- Resource ceilings: `MemoryMax=256M`, `TasksMax=64` (the Pi hosts all station
  services; a leak must not OOM the whole station)
- Logs to journal, identifier `shelly-power-bridge`.

Dependencies at runtime: only network access to the MQTT broker. The Shelly plugs must
be pre-provisioned (by the operator, out of band) to speak MQTT to the same broker
under their device-id prefix — the bridge cannot configure them.

---

## 8. Invariants & safety rules

Behavior contract — a reimplementation must preserve all of these:

1. **`power` in `/state` is always the actual relay position read back from the plug's
   native announcement**, never the last commanded value. No optimistic state.
2. **`fail_safe` default is `off`**: after a mains outage the station stays OFF until
   explicitly re-energized. The bridge never programs the plug's power-on behavior, but
   it publishes the configured value so consumers (the sequencer) can reason about it.
   (The plug's own power-on default is set out of band and must match this metadata.)
3. **Process death must flip every slot to `offline` simultaneously** — one Will per
   slot, all registered at connect time by the same process.
4. **Two-layer liveness must remain distinct**: `/status` = bridge liveness (LWT);
   `/state.device_online` = plug reachability (heartbeat). Never conflate them.
5. **`/cmd` is retained and is the steady-state intent**: on any bridge/broker restart
   the last command is replayed and re-applied, so the plug converges to the intended
   state. The bridge must not clear or consume the retained command.
6. **No command amplification**: only `set_power` `on|off` is acted on; everything else
   is dropped. The bridge must never invent commands to the plug beyond the exact
   `Switch.Set` RPC of §2.
7. **`/state` is not published while the slot's MQTT client is disconnected** (the LWT
   already carries the failure; publishing state into a dead session is wrong/pointless).
8. **Unknown state must not read as online**: a plug never heard from since startup is
   reported `device_online:false` within the first staleness tick (10 s).
9. Commands and state flow through a serialized per-slot path — the MQTT receive path
   must never block on publishing (a blocked receive handler deadlocks the client's
   dispatch loop; this exact defect took down a sibling component in production). In any
   stack: decouple inbound message handling from outbound publishing.
10. Never place the MQTT password (or any secret) in the config TOML, unit file, or
    command line; EnvironmentFile only.
11. A clean shutdown (SIGTERM) must disconnect gracefully (Will suppressed, `/status`
    stays `online`); only an ungraceful death fires the Will.

---

## 9. Known defects & fragilities

1. **Heartbeat `true` falsifies `power`.** The `<id>/online` `"true"` handler enqueues
   `HandleTelemetry("on")` — which sets `power:"on"` *and* `device_online:true`, not
   just device_online. If the relay is actually OFF, a periodic heartbeat publishes
   `/state` with `power:"on"` — a wrong state that persists until the next native
   status announcement corrects it (announcements are periodic but sparse; the window
   can be minutes). The code comment calls it a "no-op if power unchanged", which is
   only true when the relay is on. A reimplementation should make the heartbeat refresh
   only `device_online`/`error`, leaving `power` untouched.
2. **Parsed metering is discarded.** `apower`, `aenergy`, `voltage`, `current` are
   decoded in the wire codec but never published anywhere; the canonical `/state` has
   no power/energy fields. If the PRD wants metering (a Plus 1PM provides it), it is a
   deliberate gap to close — the parse already exists.
3. **Committed build artifacts.** A macOS/arm64 binary is checked in at the project
   root (`shelly-power-bridge`) and in `dist/`. Hygiene issue only.
4. **Shared `client_id` is a collision trap.** If `mqtt.client_id` is set (non-empty),
   every slot's client uses the *same* id; two slots on one broker then fight over the
   session (MQTT kicks the older client), despite the one-client-per-slot design. The
   default (empty) is safe; a reimplementation should either suffix the configured id
   per slot or reject a non-empty shared id.
5. **deploy.sh does not actually refuse placeholder Shelly ids.** Its header and the
   project CLAUDE.md claim it "will refuse to seed a placeholder id", but the code seeds
   the defaults (`shellyplus1pm-aabbccddeeff`, `shellyplus1pm-112233445566`) without
   check. Docs-vs-code disagreement; first deploy can silently bind to nonexistent
   devices (all telemetry silent, all slots `device_online:false`).
6. **Silent event drop under load.** The per-slot jobs queue is 64 deep and drops on
   overflow (deliberately — to protect the MQTT receive path). A dropped command is
   recovered by the retained-`/cmd` replay only if a reconnect happens; a dropped
   telemetry event is recovered by the plug's next announcement. Acceptable at this
   scale, but the drop is unlogged and uncounted.
7. **Staleness threshold is coupled to invisible device config.** The 75 s heartbeat
   timeout assumes the plug's own heartbeat/announce period (device-side, not queried
   or configured by the bridge). If someone lengthens the plug's period past 75 s, the
   slot flaps offline. The 75 s value is not configurable from the bridge's config.
8. **`/state` is not refreshed on reconnect.** After a broker reconnect the bridge
   republishes `/meta` and the retained broker copy of `/state` still serves; if the
   state changed while disconnected the new snapshot is only published when the next
   event arrives (heartbeat/announcement/cmd replay). Combined with #1's heartbeat path,
   in practice a heartbeat usually re-publishes soon after reconnect. Minor freshness
   gap, not a correctness break.
9. **Unknown `/cmd` writers get no feedback.** Invalid actions/values are dropped with
   only a log line. Nothing on the bus tells the writer it was ignored.
10. **No RPC response/error consumption.** The `Switch.Set` reply (`<id>/rpc/rb`) and
    the plug's error fields are ignored; a failed switch (e.g. relay fault) manifests
    only as "announced state never changes" — detectable by consumers, not signalled.

---

## 10. Re-implementation notes

**Must be preserved verbatim (the bus contract):**

- All topic strings: canonical `muehle/power/{master,psu-13v8}/{meta,state,status,cmd}`
  and native `<shelly_id>/{status/switch:0,online,rpc}`.
- All payload JSON field names, types, and spellings (`schema`, `role`, `device.model`,
  `device.serial`, `link`, `location`, `host`, `capabilities.fail_safe`,
  `capabilities.feeds`, `expose.*` with `command.action/value_key/value_type`, `ts`,
  `power`, `device_online`, `error`, and `/cmd` `action`/`value`).
- Retained flags and QoS 1 on meta/state/status; retained `/cmd` subscription;
  LWT payload `offline` (retained) vs connect publish `online`.
- The exact `Switch.Set` RPC payload shape (`id:1`, `src:"shelly-power-bridge"`,
  `method:"Switch.Set"`, `params:{id:0,on:<bool>}`) — the `src` string is visible to
  the plug and should stay identifiable.
- Fire-and-observe command confirmation (state only from the plug's own announcement).
- One MQTT client + one Will per slot; simultaneous offline on process death.
- Heartbeat semantics: 10 s check tick, 75 s staleness threshold, never-heard =
   offline within the first tick, `online`=`false` message marks offline immediately,
   recovery only from native announcements/heartbeat `true`.
- Change-dedup on (`power`,`device_online`,`error`); no `/state` publish while
   disconnected; `/meta` republished on every reconnect.
- Command rejection behavior (only `set_power`/`on|off`; drop everything else).
- Timing constants: 5 s systemd restart, 500 ms disconnect quiesce, jobs queue depth
   64 (as a bound, exact depth is free).
- Exit codes (2 config, 1 runtime, 0 clean stop) and the seed-once/secrets deployment
   posture.

**Free to change (implementation detail):**

- Language, MQTT client library, logging library/format, goroutine structure.
- The paho-specific ctx-aware connect workaround and the jobs-channel pattern — any
   mechanism that satisfies invariants #7, #9 and the prompt-connect-on-SIGTERM
   behavior is fine.
- How the heartbeat clock is stored; how dedup is implemented.
- JSON field order and whitespace in payloads (consumers parse JSON; do not change
   field names/types).
- Whether the RPC response topic is subscribed (today it is deliberately not).

**Recommendations beyond parity (fix the known defects):** make the heartbeat refresh
`device_online` only (defect #1); consider publishing the already-parsed metering to
`/state` or a sibling field (defect #2); suffix per-slot client ids if a shared prefix
is configured (defect #4); reject or seed-refuse placeholder Shelly ids (defect #5);
log jobs drops (defect #6). Each changes the observable behavior slightly — flag any
such change explicitly in the PRD so parity with the legacy system is a decision, not
an accident.