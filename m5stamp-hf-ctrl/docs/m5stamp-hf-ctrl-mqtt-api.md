# MQTT schema — m5stamp-hf-ctrl

This document describes the MQTT interface exposed by **m5stamp-hf-ctrl**, the
M5 Stamp PLC #1 firmware. It implements **two** station integration-model
slots — `muehle/hf/switch` and `muehle/hf/pa-arm` — from one physical PLC (a
compound device; the shared `device{model,serial}` is the relationship, model
§3). It is the authoritative on-the-wire contract — derived from
`src/main.cpp`, `src/mqtt_slot.h`, and `src/config.h`.

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, `192.168.1.139:1883`) |
| Authentication | Username/password (`hf` user) |
| Clean session | false; retained `/cmd` is replayed on every reconnect (self-heal) |
| Auto-reconnect | handled in `loop()` (WiFi + two MQTT clients) |
| Client IDs | one per slot: `muehle-switch`, `muehle-pa-arm` |
| Connections | **two** (one PubSubClient per slot, each with its own LWT) |

Two connections (not one) is deliberate: MQTT 3.1.1 allows one Will per client,
and the PLC publishes two slots that each need their own `<slot>/status` LWT.
One connection per slot means a PLC crash fires **both** wills at once — no
stale-online gap. This mirrors the Go compound bridge's one-client-per-slot
decision.

---

## 2. Topic addressing

All topics are addressed `<site>/<station>/<slot>/<suffix>`. The site is
`muehle` (hardcoded in `config.h`); both slots are under `hf/`.

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | PLC → bus | birth certificate: identity + capabilities + `expose` |
| `/state` | yes | PLC → bus | live state (JSON snapshot) |
| `/status` | yes | broker LWT | liveness of **this PLC, this slot**: `online` / `offline` |
| `/cmd` | **yes** | bus → PLC | desired state (self-healing steady-state, §8) |

`/cmd` is **retained** for both slots: they hold steady-state intent. On
reconnect the broker replays the last command and the firmware re-applies it.

The firmware also **subscribes** `muehle/hf/radio/state` and
`muehle/hf/ant-switch/state` (on the pa-arm connection) for arm-logic inputs; it
never publishes there.

---

## 3. `muehle/hf/switch` — remote-on relays

Role `switch`. Non-exclusive multi-channel relay (relays 3 & 4 of the PLC).

### `/meta`

```json
{
  "schema": "1.0",
  "role": "switch",
  "device": { "model": "M5Stamp PLC", "serial": "m5stamp-plc-1" },
  "link": "wifi",
  "host": "embedded",
  "capabilities": {
    "channels": ["pa", "trx"],
    "exclusive": false,
    "kind": { "pa": "remote_on", "trx": "remote_on" },
    "relay_map": { "pa": 3, "trx": 4 }
  },
  "expose": {
    "device": { "name": "M5Stamp PLC", "model": "M5Stamp PLC", "manufacturer": "M5Stack" },
    "fields": [
      { "key": "pa",  "name": "PA remote-on",  "type": "enum", "options": ["on","off"], "writable": true,
        "command": { "action": "set_pa",  "value_key": "value", "value_type": "string" } },
      { "key": "trx", "name": "TRX remote-on", "type": "enum", "options": ["on","off"], "writable": true,
        "command": { "action": "set_trx", "value_key": "value", "value_type": "string" } }
    ]
  }
}
```

### `/state`

```json
{ "ts": 123456, "pa": "on", "trx": "off", "device_online": true }
```

`pa` / `trx` are the **actual** relay positions, read back from relays 3 & 4
(not the last command). `ts` is PLC uptime ms (no RTC — a monotonic freshness
marker). `device_online` is the PLC itself (true while it is publishing).

### `/cmd`

```json
{ "action": "set_pa",  "value": "on" }
{ "action": "set_trx", "value": "off" }
```

Each drives the named relay and publishes a new `/state` with the readback
position. `set_pa` drives relay 3; `set_trx` drives relay 4. The argument rides
under the conventional `value` key (stationa /cmd convention).

---

## 4. `muehle/hf/pa-arm` — the arm relay

Role `pa-arm`. The PA-enable arm relay (relay 1), **fail-safe-open**, with the
arm logic embedded in the firmware. Same device as `hf/switch` (compound).

### `/meta`

```json
{
  "schema": "1.0",
  "role": "pa-arm",
  "device": { "model": "M5Stamp PLC", "serial": "m5stamp-plc-1" },
  "link": "wifi",
  "host": "embedded",
  "capabilities": { "fail_safe": "open", "heartbeat": true, "relay": 1 },
  "expose": {
    "device": { "name": "M5Stamp PLC", "model": "M5Stamp PLC", "manufacturer": "M5Stack" },
    "fields": [
      { "key": "armed",   "name": "Armed", "type": "boolean", "writable": false },
      { "key": "enabled", "name": "Enabled (arm permit)", "type": "boolean", "writable": true,
        "command": { "action": "set_enabled", "value_key": "value", "value_type": "boolean" } }
    ]
  }
}
```

### `/state`

```json
{ "ts": 123456, "enabled": true, "armed": false, "device_online": true, "error": "radio tuning" }
```

| Field | Type | Meaning |
|-------|------|---------|
| `ts` | number | PLC uptime ms (freshness marker) |
| `enabled` | bool | software arm-permit (last `set_enabled` cmd) |
| `armed` | bool | **derived** actual relay state — see arm logic below; never commanded directly |
| `device_online` | bool | the PLC (true while publishing) |
| `error` | string, omitempty | why the arm is dropped: `radio offline` / `radio feed stale` / `radio tuning` / `band not safe` / `antenna grounded` (first failing check in priority order) |

`error` is omitted when `armed` would be true (i.e. when enabled and all
safety conditions hold). When `enabled` is false, `armed` is false with no
`error` (that is the operator's intent, not a fault).

### `/cmd`

```json
{ "action": "set_enabled", "value": "true" }
{ "action": "set_enabled", "value": "false" }
```

There is **no** `arm` action — `armed` is derived, never commanded (§6
software-arm-permit AND hardware key). `set_enabled` only sets the permit; the
firmware arms when safe and drops automatically when not.

### Arm logic (embedded)

```
armed = enabled ∧ radio_online ∧ ¬radio.tuning ∧ band_safe ∧ heartbeat ∧ antenna_ready
```

| Input | Source | Meaning |
|-------|--------|---------|
| `enabled` | `set_enabled` /cmd | operator/sequencer arm permit |
| `radio_online` | `hf/radio/state` `device_online` | the radio (flexbridge) is up |
| `radio.tuning` | `hf/radio/state` `tuning` | a tune cycle is in progress → drop |
| `band_safe` | `hf/radio/state` `band` ∈ {160m,80m,60m,40m,30m,20m,17m,15m,12m,10m,6m} | a band the PA can amplify |
| `heartbeat` | `hf/radio/state` received within `RADIO_HEARTBEAT_MS` (10 s) | the radio feed is live |
| `antenna_ready` | `hf/ant-switch/state` `selected` ≠ `off` | an antenna is in circuit (not the grounded `off` position) |

The relay is energized **only** when `armed` is true; any failure de-energizes
it → open → PA disabled (fail-safe-open). Loss of 13.8 V (which powers the PLC)
or a PLC crash drops the relay open by hardware. The firmware subscribes
`muehle/hf/radio/state` and `muehle/hf/ant-switch/state` on the pa-arm connection
and dispatches them by topic in `handlePaArmCmd` (`/radio/state` → radio inputs,
`/ant-switch/state` → `antenna_ready`, `/cmd` → set_enabled).

---

## 5. `/status` — liveness (both slots)

Retained, driven by the per-slot MQTT LWT: `online` while that slot's
connection is up, `offline` when the PLC dies (or the will fires). This is
liveness of the **PLC**, not the controlled device — the relay/PA reachability
is `device_online` in `/state`.