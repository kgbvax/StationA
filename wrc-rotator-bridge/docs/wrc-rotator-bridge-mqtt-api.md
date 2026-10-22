# MQTT schema — wrc-rotator-bridge

This document describes the MQTT interface exposed by **wrc-rotator-bridge** (the HF
rotator bridge). wrc-rotator-bridge implements the `rotator` slot of the station
integration model (`../../docs/station-integration-model.md`).

It bridges the HF antenna rotator (Yaesu G-450DC) steered via an AF6SA WRC
controller (a WebSocket at `ws://192.168.1.108/wsrotor`) to the bus. It is the
authoritative on-the-wire contract — derived from `internal/bridge/bridge.go`
and `internal/rotor/protocol.go`.

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, `tcp://192.168.1.139:1883`) |
| Authentication | Username/password (`hf` user) |
| Clean session | auto (paho default); `hf/ant-mode/state` and `/cmd` are re-subscribed on every reconnect (ant-mode first) |
| Auto-reconnect | yes (`SetAutoReconnect`, `SetConnectRetry`) |
| Client ID | `muehle-hf-rotator` (configurable via `mqtt.client_id`) |

The initial connect is ctx-aware: a SIGTERM while the broker is unreachable
interrupts the connect (it does not hang until systemd SIGKILL).

---

## 2. Topic addressing

All topics are addressed as:

```
<site>/<station>/<slot>/<suffix>
```

Configured via `[mqtt]` in `config.toml`:

```toml
site    = "muehle"      # physical site
station = "hf"          # transmitting entity
slot    = "rotator"     # role
```

```
muehle/hf/rotator/meta
muehle/hf/rotator/state
muehle/hf/rotator/status
muehle/hf/rotator/cmd
```

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | component → bus | birth certificate: identity + capabilities + `expose` |
| `/state` | yes | component → bus | live state (JSON snapshot) |
| `/status` | yes | broker LWT | liveness: `online` / `offline` |
| `/cmd` | **no** | bus → component | desired state / command (one-shot) |

`/cmd` is **not retained** (model §8): a rotator move is a one-shot command,
and re-applying a stale azimuth on bridge restart could spin the rotator
unexpectedly. The rotator reports its actual position in `/state`; there is no
self-healing steady-state to preserve.

---

## 3. `/status` — liveness

Plain string, retained, QoS 1.

| Value | When |
|-------|------|
| `online` | published on every (re)connect |
| `offline` | broker Last Will on unclean disconnect; published on clean shutdown |

This is the **bridge's** liveness, not the WRC's. If the WRC becomes
unreachable while the bridge is up, `/status` stays `online` and `/state`
carries `device_online:false` (§5).

---

## 4. `/meta` — birth certificate

Retained JSON, published once per connect cycle.

```json
{
  "schema": "1.0",
  "role": "rotator",
  "device": { "model": "Yaesu G-450DC" },
  "link": "ethernet",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": { "axes": ["az"] },
  "expose": {
    "device": { "name": "HF Rotator", "model": "Yaesu G-450DC", "manufacturer": "Yaesu" },
    "fields": [
      { "key": "az", "name": "Azimuth", "type": "number", "unit": "°", "class": "azimuth",
        "state_class": "measurement", "writable": true, "min": 0, "max": 450, "step": 1,
        "command": { "action": "set_az", "value_key": "az", "value_type": "float" } },
      { "key": "target_az", "name": "Target Azimuth", "type": "number", "unit": "°",
        "class": "azimuth", "state_class": "measurement" },
      { "key": "moving", "name": "Moving", "type": "boolean" },
      { "key": "rotor_state", "name": "Rotor State", "type": "string" },
      { "key": "device_online", "name": "Device Online", "type": "boolean" },
      { "key": "lock", "name": "Antenna Mode Lock", "type": "string" }
    ],
    "actions": [
      { "key": "stop", "name": "Stop", "command": { "action": "stop" } },
      { "key": "fwd", "name": "Rotate CW", "command": { "action": "fwd" } },
      { "key": "rev", "name": "Rotate CCW", "command": { "action": "rev" } }
    ]
  }
}
```

| Field | Notes |
|-------|-------|
| `role` | `rotator` — canonical role name (model §4), never a device name |
| `device` | `model` only; the WRC reports no serial/firmware |
| `link` | transport to the controller (`ethernet` for the WebSocket path) |
| `location` / `host` | from config — deployment facts, never code constants |
| `capabilities` | `axes: ["az"]` — azimuth-only rotator (model §7.1) |
| `expose` | consumer-neutral field surface (model §3.1, Appendix C). No area — `hadiscovery` supplies the default. `az` is a setpoint (`writable` + command descriptor); `stop`/`fwd`/`rev` are one-shot actions. `lock` is read-only: the mode is set on `hf/ant-mode`, never here. |

---

## 5. `/state` — live state

Retained JSON snapshot, QoS 1. Published only when a published field changes
(the WRC streams status frequently; a stationary rotator produces no churn).

```json
{
  "ts": "2026-07-06T12:34:56Z",
  "az": 123.5,
  "target_az": 180.0,
  "moving": true,
  "rotor_state": "rotating",
  "device_online": true,
  "lock": "storm"
}
```

| Field | Type | Unit | Notes |
|-------|------|------|-------|
| `ts` | string | — | RFC 3339 UTC timestamp of this publish |
| `az` | number | ° | current azimuth |
| `target_az` | number | ° | commanded target azimuth (from WRC `tdeg`); omitted when none |
| `moving` | boolean | — | true while the WRC reports the rotator is turning |
| `rotor_state` | string | — | raw WRC state string (diagnostic, e.g. `rotating`/`stopped`); omitted when empty |
| `device_online` | boolean | — | false when the WRC WebSocket is down while the bridge is up |
| `error` | string | — | human-readable WRC fault (from WRC `fmsg`); omitted when none |
| `lock` | string | — | antenna-mode lock: `storm` \| `maintenance` while motion is locked (§9); **omitted** when unlocked |

Publish triggers:
- Every parsed WRC status whose `az`/`target_az`/`moving`/`rotor_state`/`error`
  differs from the last snapshot.
- When the WebSocket is lost or regained (`device_online`/`error` change).
- When the antenna-mode lock label changes (within ~1 s of the new
  `hf/ant-mode/state`). A republished lock with the same label publishes nothing.

A rotator carries no `freq_hz`/`band`/`mode` — those are radio concerns.

---

## 6. `/cmd` — desired state

Not retained, QoS 1. Published by external systems (reconciler, HA via
`hadiscovery`, operator).

### Command payloads

**set_az** — rotate to an azimuth:
```json
{"action": "set_az", "az": 180}
```

**set_az with the maintenance override** — an operator nudge during a
maintenance lock (§9). Without a lock it is a plain `set_az`:
```json
{"action": "set_az", "az": 180, "override": "maintenance"}
```

**stop** — halt motion:
```json
{"action": "stop"}
```

**fwd** / **rev** — rotate CW / CCW (continuous jog):
```json
{"action": "fwd"}
{"action": "rev"}
```

Unknown actions are logged and ignored. `set_az` without `az` is ignored.
While the antenna-mode lock is set, `set_az`/`fwd`/`rev` are refused (§9); a
refusal is not acknowledged on the bus — observe `/state.lock`.

---

## 7. GS-232B inbound server (optional)

When `[gs232] enabled = true` (default), wrc-rotator-bridge also listens for
GS-232B clients on `bind:port` (default `0.0.0.0:7373`). This is a parallel
control path orthogonal to the MQTT contract: it drives the same rotator the
bridge does, and the resulting motion surfaces in `/state` so the bus stays
coherent.

| Command | Meaning | Response |
|---------|---------|----------|
| `C` / `C2` | query position | `+0aaa+0000\r` (aaa = current azimuth) |
| `Mxxx` | move to azimuth xxx | `\r` |
| `Wxxx yyy` | set azimuth xxx (elevation yyy ignored) | `\r` |
| `S` | stop | `\r` |
| other | — | `?>\r` |

Commands are `\r`- or `\n`-terminated.

While the antenna-mode lock is set (§9), `M` and `W` are refused — the rotator
does not move, but the client still gets its `\r` ack (GS-232B has no error
reply that clients handle). `S` and `C`/`C2` work as normal.

## 8. PSTRotator UDP listener (optional)

When `[pstrotator] enabled = true` (default), wrc-rotator-bridge also listens
for PSTRotator XML datagrams on `bind:port` (default `0.0.0.0:12040`). This is
another parallel control path orthogonal to the MQTT contract: it drives the
same rotator the bridge does, and the resulting motion surfaces in `/state`.

| Datagram | Meaning | Response |
|----------|---------|----------|
| `<PST><AZIMUTH>180</AZIMUTH></PST>` | rotate to 180° | none |
| `<PST><AZIMUTH>180</AZIMUTH><ELEVATION>45</ELEVATION></PST>` | rotate to 180° (elevation ignored) | none |
| `<PST><STOP>1</STOP></PST>` | halt motion | none |
| `<PST><PARK>1</PARK></PST>` | park | none (no park support; logged) |
| `<PST>AZ?</PST>` | query position | `<PST><AZIMUTH>aaa</AZIMUTH></PST>` on source IP, port+1 |

Configure PSTRotator to send its UDP control output to the bridge host on port
`12040` (or the configured `pstrotator.port`).

While the antenna-mode lock is set (§9), `<AZIMUTH>` is refused silently (UDP
has no reply for it anyway). `<STOP>` and `AZ?` work as normal. `<PARK>` stays
ignored — the lock park is driven by `hf/ant-mode`, not by PSTRotator.

---

## 9. Antenna-mode lock (`hf/ant-mode`)

The station antenna mode (`normal` | `storm` | `maintenance`) is owned by the
`antmode` logic slot. Its contract is `../../antmode/docs/ant-mode-mqtt-api.md`;
the bridge reads it with the shared parser `shared/antmode`. The bridge
subscribes to `<site>/<station>/<ant_mode_slot>/state` (default
`muehle/hf/ant-mode/state`, config `[mqtt] ant_mode_slot`; an empty value
disables the lock).

**Reading rules.** No retained ant-mode state (or an empty payload) = normal,
so the bridge works before antmode is deployed. A valid locked snapshot is
enforced even while `hf/ant-mode/status` is `offline` — a dead owner never
unlocks. A malformed payload keeps the last known lock and logs an Error. `ts`
is not checked.

**Startup hold.** Until the first ant-mode state arrives, motion is refused
while the bridge connects to the broker and for 3 s after the MQTT connection
is up (an empty or `normal` state ends the hold early). The window runs from
the connect, not from process start, so a slow broker cannot use it up. This
stops a command from slipping in ahead of the retained lock. `stop` is never
held.

**Refusal (storm or maintenance).** One gate sits in front of the WRC for all
three inbound paths:

| Path | Refused | Always allowed |
|------|---------|----------------|
| MQTT `/cmd` | `set_az`, `fwd`, `rev` | `stop` |
| GS-232 :7373 | `Mxxx`, `Wxxx yyy` (still acked `\r`) | `S`, `C`/`C2` |
| PstRotator :12040 | `<AZIMUTH>` | `<STOP>`, `AZ?` |

A refusal streak logs one Warn, then Info with a `repeat` count; a new lock
label warns again. In maintenance with `lock.nudge:true`, a `/cmd` `set_az`
carrying `"override":"maintenance"` is accepted (intent tagging for the
console, not security). The override is never accepted in storm, and never on
GS-232 or PstRotator.

**Park.** The bridge executes `lock.park_az` itself — antmode never commands
the rotator:

- Once per `lock.seq`: when `park_az` is present, the WRC is online, and the
  azimuth readback has held within 1° of where it settled, with
  `moving:false`, for 3 s. A readback already within 3° of `park_az` counts as
  parked and sends nothing. A boom creeping in gusts (readback leaving the 1°
  band with `moving:false`) restarts the 3 s window, so the park waits for a
  lull — deliberate: turning the brakeless G-450 in gusts is the worst case.
  The bridge has no deadline of its own; antmode's `rotate_timeout_s` bounds
  it (`failed`).
- Arrival = readback within 3° of `park_az` with `moving:false`.
- No re-aim for drift: once arrived, the bridge does not chase wind creep
  (antmode reports `drift_deg`). A republished lock with the same `seq` and
  `park_az` does nothing.
- Re-sent only if it never arrived and the WRC came back online after a loss
  (reconnect, power cycle), or if the WRC write itself failed.
- An operator `stop` (any path) during an underway park cancels it until a new
  `seq` (antmode `repark`).
- An accepted maintenance nudge ends the park for that `seq`, sent or not, and
  also when it comes before `park_az` is published (`lock.nudge` is set from
  maintenance entry, while antmode still grounds and retracts): the operator
  has taken the azimuth over, so neither the later `park_az` nor a WRC
  reconnect turns the boom back to the preset. A park write in flight lands
  before the nudge. antmode does not see the nudge: unless the boom is within
  tolerance of `park_az` it reaches `failed` at `rotate_timeout_s`, as after a
  stop.
- A finished park (arrived, cancelled by stop, ended by a nudge) is recorded in
  `<state_dir>/park.json` (default `/var/lib/wrc-rotator-bridge`, the systemd
  `StateDirectory`). After a bridge restart a retained lock with the same mode,
  `seq` and `park_az` is not parked again — park once means once, also across
  restarts. A nudge record matches on mode and `seq` alone (whatever
  `park_az`, also one published after the nudge). A park that was underway (sent, not arrived) at the restart is sent
  again. Without the record (first start, `state_dir = ""`, unreadable file)
  the retained lock is parked once more; a rotator already at `park_az` does
  not move.
- `normal` clears the park state; the rotator stays where it is.

---

## 10. Home Assistant discovery

The standalone `hadiscovery` consumer is the only path wrc-rotator-bridge uses.
It reads the consumer-neutral `expose` block from `/meta` (§4) and renders HA
discovery. The bridge itself contains **no HA knowledge** and publishes nothing
under `homeassistant/…`. The legacy embedded path (`publish_ha_discovery`)
exists in the config for migration symmetry with sibling bridges but defaults
to `false` and is not wired in this component. See `../../docs/station-integration-model.md`
§9 and `hadiscovery/docs/discovery-mqtt-api.md`.

---

## 11. Typical interaction flows

**Read current state on startup:** subscribe to `muehle/hf/rotator/#`. The
broker immediately delivers retained `/meta`, `/state`, and `/status`.

**Rotate to an azimuth:**
1. Publish `{"action":"set_az","az":180}` to `muehle/hf/rotator/cmd`.
2. The bridge writes `{"az":"180"}` to the WRC WebSocket.
3. Observe `/state.az` move toward 180 and `/state.moving` go true, then false
   when it arrives (fire-and-observe — model §1 plane discipline).

**Stop:** publish `{"action":"stop"}` to `/cmd`; observe `/state.moving` go false.

**Detect the WRC is down:** `/state` carries `"device_online":false` and an
`"error":"..."` while the bridge is still running. `/status` stays `online`.
If the bridge crashes or loses the broker, `/status` → `offline` (LWT fires).
Liveness itself is never a `/state` field — see model §3.

**Storm lock:** `/state.lock` becomes `storm`; the bridge drives to
`lock.park_az` once the readback is stable and refuses every move until
`hf/ant-mode` returns to `normal`. `stop` still works.
