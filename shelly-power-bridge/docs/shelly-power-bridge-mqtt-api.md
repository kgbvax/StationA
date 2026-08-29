# MQTT schema — shelly-power-bridge

This document describes the MQTT interface exposed by **shelly-power-bridge**
(the Shelly Gen2+ smart-plug bridge). shelly-power-bridge implements the
site-level `power` slots of the station integration model
(`../../docs/station-integration-model.md`, §4 `power` role, §7.0).

It fronts Shelly Gen2+ plugs (Plus / Mini / Pro …) that are themselves MQTT
clients on the same broker. The bridge subscribes to their **native** topics
and translates to/from the canonical station topics. It is the authoritative
on-the-wire contract — derived from `internal/bridge/bridge.go` and
`internal/shelly/shelly.go`.

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, `tcp://192.168.1.139:1883`) |
| Authentication | Username/password (`hf` user) |
| Clean session | false (`CleanSession=false`); retained `/cmd` is replayed on every reconnect (self-heal) |
| Auto-reconnect | yes (`SetAutoReconnect`) |
| Client ID | one per `[[slot]]`, `<site>-<station>-<slot>` (e.g. `muehle-power-master`) |

The bridge is **compound**: one process fronts N Shelies, each `[[slot]]`
running its own paho client with its own LWT. MQTT 3.1.1 allows only one Will
per client, so one client per slot is what makes a process death fire every
slot's will at once (no stale-online gap).

The initial connect is ctx-aware: a SIGTERM while the broker is unreachable
interrupts the connect (it does not hang until systemd SIGKILL).

---

## 2. Topic addressing

All canonical topics are addressed as:

```
<site>/<station>/<slot>/<suffix>
```

The site comes from `[mqtt].site` (`muehle`); each `[[slot]]` sets its own
`station` and `slot`. For the site-level power layer these are `station="power"`,
`slot="master"|"psu-13v8"|…`. The two default slots give:

```
muehle/power/master/meta      muehle/power/psu-13v8/meta
muehle/power/master/state      muehle/power/psu-13v8/state
muehle/power/master/status     muehle/power/psu-13v8/status
muehle/power/master/cmd        muehle/power/psu-13v8/cmd
```

Site is **mandatory** (model §2/§8.1) — `Validate` refuses to start if it is
empty, and refuses duplicate slot addresses.

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | component → bus | birth certificate: identity + capabilities + `expose` |
| `/state` | yes | component → bus | live power state (JSON snapshot) |
| `/status` | yes | broker LWT | liveness of **the bridge client**: `online` / `offline` |
| `/cmd` | **yes** | bus → component | desired power intent; self-healing steady-state (model §8 exception) |

`/cmd` is **retained** (the self-healing steady-state exception): a power slot
holds an on/off intent. On reconnect the broker replays the last command and
the bridge re-applies it, so the Shelly converges to the intended state even
after a bridge restart. The Shelly's own native announce is the confirmation
(fire-and-observe).

---

## 3. `/meta` — birth certificate

Retained. Published on every (re)connect.

```json
{
  "schema": "1.0",
  "role": "power",
  "device": { "model": "Shelly Plus 1PM", "serial": "shellyplus1pm-aabbccddeeff" },
  "link": "wifi",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "fail_safe": "off",
    "feeds": ["hf/radio", "uhf/radio", "hf/tuner", "hf/ant-ctrl", "hf/ant-switch", "hf/rotator", "hf/switch", "hf/pa-arm"]
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

- `role` is `power` (model §4).
- `capabilities.fail_safe` is the Shelly power-on default (`off` ⇒ a mains blip
  drops the station; the plug restores OFF).
- `capabilities.feeds` lists the downstream slot addresses this supply powers.
  Empty for the station master mains (top of the tree); populated for the
  13.8 V PSU. Omitted from `/meta` entirely when empty.
- `expose` is the consumer-neutral field surface (model §3.1, Appendix C);
  `hadiscovery` renders HA discovery from it (model §9). `power` is the single
  writable field; its `command` rides the argument under the conventional
  `value` key (stationa /cmd convention).

---

## 4. `/state` — live state snapshot

Retained. Published on change (deduped against the last snapshot; not republished
for unchanged telemetry). Not published while the bridge client is disconnected
(the LWT already covers liveness).

```json
{
  "ts": "2026-07-14T18:42:01Z",
  "power": "on",
  "device_online": true
}
```

On a heartbeat loss / Shelly unreachable:

```json
{
  "ts": "2026-07-14T18:43:02Z",
  "power": "on",
  "device_online": false,
  "error": "shelly heartbeat lost"
}
```

| Field | Type | Meaning |
|-------|------|---------|
| `ts` | string (RFC3339) | snapshot timestamp (UTC) |
| `power` | `on` \| `off` | the **actual** relay position, read back from the Shelly's native announce (not the last command) |
| `device_online` | bool | Shelly reachability: true while its `<id>/online` heartbeat arrives |
| `error` | string, omitempty | reason the device was marked offline |

`power` reflects the **actual** relay position, confirmed by the Shelly's own
native status announce — never an open-loop assumption from the last `/cmd`.

---

## 5. `/status` — liveness

Retained. Driven by the MQTT LWT: `online` while the slot's paho client is
connected, `offline` when the process dies (or the will fires). This is liveness
of the **bridge**, not the Shelly — the Shelly's reachability is `device_online`
in `/state`.

---

## 6. `/cmd` — command

Retained. Accepted payload (the argument rides under `value`):

```json
{ "action": "set_power", "value": "on" }
{ "action": "set_power", "value": "off" }
```

`set_power` drives the Shelly relay (switch 0) over the Gen2+ RPC-over-MQTT
topic `<shelly_id>/rpc`:

```json
{ "id": 1, "src": "shelly-power-bridge", "method": "Switch.Set", "params": { "id": 0, "on": true } }
```

The Shelly's response (on `<shelly_id>/rpc/rb`) is not consumed — the resulting
native status announce is the confirmation (fire-and-observe, model §8).
Unknown actions or values are logged and dropped.

---

## 7. Shelly native topics (subscribed)

The bridge subscribes to the Shelly's native topics (the Shelly is also a
client on the same broker). `<shelly_id>` is the Gen2 device id / MQTT prefix
from the `[[slot]]` config.

| Topic | Direction | Payload | Used for |
|-------|-----------|---------|----------|
| `<shelly_id>/status/switch:0` | Shelly → bridge | `{"output":true,"apower":…}` | canonical `power` + `device_online=true` |
| `<shelly_id>/online` | Shelly → broker | `true` / `false` | heartbeat → `device_online`; staleness watcher marks offline after 75 s without `true` |
| `<shelly_id>/rpc` | bridge → Shelly | `Switch.Set` RPC | drive relay 0 on `set_power` |

A status announce sets `device_online=true` and updates `power`; the `<id>/online`
heartbeat keeps it true. The staleness watcher (10 s tick) marks the device
offline if no heartbeat arrives within 75 s, publishing a `/state` snapshot
with `device_online=false` and an `error` reason.