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
| Protocol | MQTT 3.1.1 (plain TCP, `tcp://192.168.1.50:1883`) |
| Authentication | Username/password (`hf` user) |
| Clean session | auto (paho default); `/cmd` is re-subscribed on every reconnect |
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
      { "key": "device_online", "name": "Device Online", "type": "boolean" }
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
| `expose` | consumer-neutral field surface (model §3.1, Appendix C). No area — `hadiscovery` supplies the default. `az` is a setpoint (`writable` + command descriptor); `stop`/`fwd`/`rev` are one-shot actions. |

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
  "device_online": true
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

Publish triggers:
- Every parsed WRC status whose `az`/`target_az`/`moving`/`rotor_state`/`error`
  differs from the last snapshot.
- When the WebSocket is lost or regained (`device_online`/`error` change).

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

---

## 8. Home Assistant discovery

The standalone `hadiscovery` consumer is the only path wrc-rotator-bridge uses.
It reads the consumer-neutral `expose` block from `/meta` (§4) and renders HA
discovery. The bridge itself contains **no HA knowledge** and publishes nothing
under `homeassistant/…`. The legacy embedded path (`publish_ha_discovery`)
exists in the config for migration symmetry with sibling bridges but defaults
to `false` and is not wired in this component. See `../../docs/station-integration-model.md`
§9 and `hadiscovery/docs/discovery-mqtt-api.md`.

---

## 9. Typical interaction flows

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