# MQTT schema — atr1k-tuner-bridge

This document describes the MQTT interface exposed by **atr1k-tuner-bridge**
(the ATR-1000 ATU bridge). atr1k-tuner-bridge implements the `tuner` slot of the
station integration model (`../../docs/station-integration-model.md`).

It bridges the ATR-1000 ATU (BTR-1000 / N7DDC family) — reached over its binary
WebSocket at `ws://192.168.1.20:60001` — to the bus. It is the authoritative
on-the-wire contract — derived from `internal/bridge/bridge.go` and
`internal/tuner/protocol.go`.

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, `tcp://192.168.1.50:1883`) |
| Authentication | Username/password (`hf` user) |
| Clean session | auto (paho default); `/cmd` is re-subscribed on every reconnect |
| Auto-reconnect | yes (`SetAutoReconnect`, `SetConnectRetry`) |
| Client ID | `muehle-hf-tuner` (configurable via `mqtt.client_id`) |

The initial connect is ctx-aware: a SIGTERM while the broker is unreachable
interrupts the connect (it does not hang until systemd SIGKILL).

---

## 2. Topic addressing

All topics are addressed as:

```
<site>/<station>/<slot>/<suffix>
```

The slot address is **not** hardcoded — it is built from the three `[mqtt]`
fields, with built-in defaults so the bridge works with no config file:

| Key | Default | Meaning |
|------|---------|---------|
| `site` | `muehle` | physical site |
| `station` | `hf` | transmitting entity |
| `slot` | `tuner` | role |

```toml
site    = "muehle"      # physical site
station = "hf"          # transmitting entity
slot    = "tuner"       # role
```

The defaults give `muehle/hf/tuner`:

```
muehle/hf/tuner/meta
muehle/hf/tuner/state
muehle/hf/tuner/status
muehle/hf/tuner/cmd
```

To place the slot elsewhere, set the fields in `config.toml`, override via the
`ATR1K_TUNER_BRIDGE_MQTT_SITE` / `_STATION` / `_SLOT` env vars, or seed them via
`deploy.sh`'s `MQTT_SITE` / `MQTT_STATION` / `MQTT_SLOT`. Site and station are
**mandatory** (model §2/§8.1) — `Validate` refuses to start if either is empty,
so a malformed path can never be published.

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | component → bus | birth certificate: identity + capabilities + `expose` |
| `/state` | yes | component → bus | live state (JSON snapshot) |
| `/status` | yes | broker LWT | liveness: `online` / `offline` |
| `/cmd` | **no** | bus → component | desired state / command (one-shot) |

`/cmd` is **not retained** (model §8): a tune is a one-shot command, and
re-applying a stale `tune` on bridge restart could re-key the ATU unexpectedly.
Self-heal comes from the consumer re-reading the retained `/state`, not from a
retained `/cmd`.

---

## 3. `/meta` — birth certificate

Retained JSON, republished on every (re)connect.

```json
{
  "schema": "1.0",
  "role": "tuner",
  "device": { "model": "ATR-1000" },
  "link": "wifi",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "inline": true,
    "tune_modes": ["mem", "full"]
  },
  "expose": {
    "device": { "name": "HF ATU", "model": "ATR-1000" },
    "fields": [
      { "key": "swr",           "name": "SWR",            "type": "number",  "unit": "ratio", "class": "swr",   "state_class": "measurement" },
      { "key": "fwd",           "name": "Forward Power",  "type": "number",  "unit": "W",    "class": "power", "state_class": "measurement" },
      { "key": "inline",        "name": "In Line",        "type": "boolean", "writable": true,
        "command": { "action": "set_inline", "value_key": "value", "value_type": "bool" } },
      { "key": "l_uh",          "name": "Inductance",     "type": "number",  "unit": "µH" },
      { "key": "c_pf",          "name": "Capacitance",    "type": "number",  "unit": "pF" },
      { "key": "settling",      "name": "Tuning",         "type": "boolean" },
      { "key": "fault",         "name": "Fault",          "type": "string" },
      { "key": "device_online", "name": "Device Online", "type": "boolean" }
    ],
    "actions": [
      { "key": "tune", "name": "Tune",
        "command": { "action": "tune", "value_key": "value", "value_type": "enum" } }
    ]
  }
}
```

`capabilities.inline` advertises that the ATU can be put in line/bypass;
`capabilities.tune_modes` lists the tune modes the `tune` action accepts. The
`expose` block is consumer-neutral (model §3.1, Appendix C); `hadiscovery`
renders HA entities from it — this bridge carries no HA vocabulary.

---

## 4. `/state` — live snapshot

Retained single JSON document, republished only when a published field changes
(change-dedup; model §8). `ts` is RFC3339 UTC.

```json
{
  "ts": "2026-07-12T10:30:00Z",
  "inline": true,
  "swr": 1.3,
  "fwd": 85,
  "l_uh": 12.34,
  "c_pf": 56,
  "settling": false,
  "fault": "",
  "device_online": true,
  "error": ""
}
```

| Field | Type | Meaning |
|-------|------|---------|
| `inline` | bool | tuner in the RF path (true) or bypassed (false) |
| `swr` | number | SWR ratio. Raw ATR value ≥100 is divided by 100 (217 → 2.17). |
| `fwd` | number | forward power, watts |
| `l_uh` | number | matching inductance, µH (raw /100) |
| `c_pf` | number | matching capacitance, pF |
| `settling` | bool | a tune cycle is in progress (TX-inhibit hint; clears when the relays update or on timeout) |
| `fault` | string | non-empty on a tune timeout (`"tune timeout"`); cleared on the next relay update |
| `device_online` | bool | the ATR is reachable while the bridge is up (distinct from `/status`, which is the bridge LWT) |
| `error` | string | human-readable device-side fault when `device_online` is false |

The ATR streams meter frames frequently; dedup keeps the bus quiet while SWR is
steady. A tuner carries **no** `freq_hz`/`band`/`mode` — those are radio
concerns.

---

## 5. `/status` — liveness

Retained plain string, registered as the MQTT Last Will:

```
muehle/hf/tuner/status   online   (retained, published on connect)
                         offline  (retained, set by the broker LWT on disconnect)
```

`/status` reflects the **bridge**, not the ATU. If the WebSocket to the ATU
drops while the bridge keeps retrying, `/status` stays `online` and only
`/state.device_online` flips to `false`.

---

## 6. `/cmd` — intent (bus → bridge)

Not retained, QoS 1. Two actions:

### set_inline

Put the tuner in line (true) or bypass (false). Drives the ATR `TuneStatus`
command. The argument rides under the conventional `value` key (matching
acombridge's `set_band`).

```json
{ "action": "set_inline", "value": true }
```

This is the soft-binding target the `antennaselect` reconciler drives for
non-resonant bands (`tuner.set_inline ← band_policy`, integration model §7.1,
§10 residual): with `antennaselect` `[tuner_follow]` enabled, the reconciler
publishes `set_inline=true` when the fan-dipole is selected on 30/60/160 m and
`set_inline=false` otherwise.

### tune

Start a tune cycle. `value` is `mem` (memory recall, fast) or `full` (full tune,
slower search). Drives the ATR `TuneMode` command. While tuning, `/state.
settling` is `true`; it clears when the relays update (settled) or on a 12 s
timeout (→ `fault: "tune timeout"`).

```json
{ "action": "tune", "value": "full" }
{ "action": "tune", "value": "mem" }
```

Unknown actions or modes are logged and ignored.

---

## 7. ATR-1000 binary protocol (summary)

Frames are `[0]=0xFF flag | [1]=cmd | [2]=payload len | [3..] payload` (uint16
LE). See `internal/tuner/protocol.go`.

| cmd | name | direction | payload |
|-----|------|-----------|---------|
| 1 | Sync | out | none — request a full state snapshot |
| 2 | Meter | in | SWR (raw/100 when ≥100) + forward watts |
| 3 | TuneStatus | out | `[3]=0` bypass / `1` in line |
| 4 | TuneMode | out | `[3]=0` Reset / `1` Mem / `2` Full / `3` Fine |
| 5 | Relay | in | L (µH, raw/100) + C (pF) — tune settled |