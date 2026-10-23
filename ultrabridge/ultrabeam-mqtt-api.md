# ultrabridge MQTT API

This document describes the MQTT interface exposed by **ultrabridge** (the bridge to the
UltraBeam RCU-06 controller). ultrabridge implements the `ant-ctrl` slot of the
station integration model (`../docs/station-integration-model.md`) — the canonical
role for a tunable-antenna controller (§4). The device name never appears in the
address or role; it lives in `/meta.device`.

> **This slot is a *controller*, not "the antenna."** ultrabridge controls the element
> tuning and direction of one specific antenna — the remotely-tunable Ultrabeam. The
> physical antennas of the station (`ant/ultrabeam`, `ant/fan-dipole`, `ant/dummy-load`)
> are passive resources selected by the `ant-switch` actuator under the `antenna-select`
> reconciler; they are not this slot. See the integration model §3 and §7.1.

It is the authoritative on-the-wire contract — derived from `internal/mqtt/client.go`.

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, e.g. `tcp://host:1883`) |
| Authentication | Username/password if the broker requires it |
| Clean session | **No** — subscriptions survive ultrabridge restarts |
| Auto-reconnect | ultrabridge reconnects automatically |
| ultrabridge client ID | derived from the slot address, `<site>-<station>-<slot>` (configurable via `mqtt.client_id`) |

---

## 2. Topic addressing

All ultrabridge topics are addressed as:

```
<site>/<station>/<slot>/<suffix>
```

Configured via `[mqtt]` in `config.toml`:

```toml
site    = "muehle"     # physical site
station = "hf"         # transmitting entity
slot    = "ant-ctrl"   # canonical role (default: ant-ctrl)
```

Example for the default Mühle HF station:

```
muehle/hf/ant-ctrl/meta
muehle/hf/ant-ctrl/state
muehle/hf/ant-ctrl/status
muehle/hf/ant-ctrl/cmd
```

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | ultrabridge → bus | birth certificate: identity + capabilities |
| `/state` | yes | ultrabridge → bus | live controller state (JSON snapshot) |
| `/status` | yes | broker LWT | liveness: `online` / `offline` |
| `/cmd` | yes | bus → ultrabridge | desired state / command |

ultrabridge also **subscribes** (QoS 0) to the antenna-mode slot's retained
`<site>/<station>/<ant_mode_slot>/state` (default `muehle/hf/ant-mode/state`), which
locks element extends in storm/maintenance — see §6.1.

---

## 3. `/status` — liveness

Plain string, retained, QoS 1.

| Value | When |
|-------|------|
| `online` | published on every (re)connect |
| `offline` | broker Last Will on unclean disconnect; ultrabridge publishes on clean shutdown |

---

## 4. `/meta` — birth certificate

Retained JSON, published once per connect cycle (after connecting to broker).

```json
{
  "schema": "1.0",
  "role": "ant-ctrl",
  "device": {
    "model": "Ultrabeam RCU-06"
  },
  "link": "serial",
  "location": "bauwagen",
  "host": "scmino",
  "capabilities": {
    "bands": ["20m", "17m", "15m", "12m", "10m", "6m"],
    "directions": ["forward", "reverse", "bidirectional"]
  },
  "expose": {
    "device": {
      "name": "UltraBeam Antenna",
      "model": "RCU-06",
      "manufacturer": "Ultrabeam",
      "area": "Radio shack"
    },
    "fields": [
      { "key": "freq_hz", "name": "Frequency", "type": "number", "unit": "Hz",
        "class": "frequency", "state_class": "measurement", "writable": true,
        "min": 1800000, "max": 54000000, "step": 1000,
        "command": { "action": "frequency", "value_key": "freq_hz", "value_type": "int" } },
      { "key": "band", "name": "Band", "type": "enum", "options_ref": "bands",
        "writable": true,
        "command": { "action": "band", "value_key": "value", "value_type": "string" } },
      { "key": "direction", "name": "Direction", "type": "enum", "options_ref": "directions",
        "writable": true,
        "command": { "action": "direction", "value_key": "value", "value_type": "string" } },
      { "key": "moving", "name": "Moving", "type": "boolean" },
      { "key": "retracted", "name": "Retracted", "type": "boolean" },
      { "key": "device_online", "name": "Device online", "type": "boolean" },
      { "key": "error", "name": "Last error", "type": "string" },
      { "key": "lock", "name": "Antenna mode lock", "type": "string" }
    ],
    "actions": [
      { "key": "retract", "name": "Retract", "command": { "action": "retract" } }
    ]
  }
}
```

`location` and `host` come from config (deployment facts, model §3/§7.3); they are
omitted when not configured.

> `directions` is the Ultrabeam's element-direction capability. It is deliberately
> **not** named `modes`: on the station bus `mode` is the canonical radio-mode
> vocabulary (`cw`, `usb`, `lsb`, …, integration model §4), a different concept.

### The `expose` block — consumer-neutral field surface

`expose` (integration model §3.1, Appendix C) is the **consumer-neutral** description of
this slot's observable/controllable field surface — no consumer vocabulary (no
`device_class`, no Jinja, no `payload_on/off`). The standalone `hadiscovery` consumer
renders Home Assistant discovery from it; other consumers (historians, dashboards,
Prometheus) can render theirs from the same block. ultrabridge is **read-write**, so
`freq_hz`/`band`/`direction` are `writable` setpoints backed by `/cmd` (their `command`
descriptor is the structured form of the `/cmd` payloads in §6), `moving` is a read-only
boolean, and `retract` is a one-shot `action`. `device_online` and `error` are read-only
diagnostics: the bridge stays up (`/status` online) while the RCU-06 is down, so these
surface device health that `/status` alone cannot — `hadiscovery` renders them as a
`binary_sensor` and a `sensor`. `lock` is read-only: the antenna mode is owned by the
`hf/ant-mode` slot (§6.1), never written here. The enum options resolve via `options_ref` into
`capabilities.bands` / `capabilities.directions` (single source of truth).

---

## 5. `/state` — live controller state

Retained JSON snapshot, QoS 1. Published only when a field value changes.

```json
{
  "ts":           "2026-07-06T12:34:56Z",
  "freq_hz":      21225000,
  "band":         "15m",
  "direction":    "forward",
  "moving":       false,
  "retracted":    false,
  "device_online": true
}
```

While the antenna-mode lock is active (§6.1) the snapshot also carries `"lock": "storm"`
(or `"maintenance"`), and — once the bridge's own retract for the current lock has
finished — `"retract_seq": <lock.seq>`.

| Field | Type | Unit | Notes |
|-------|------|------|-------|
| `ts` | string | — | RFC 3339 UTC timestamp of this publish |
| `freq_hz` | integer | Hz | Frequency the beam is tuned to |
| `band` | string | — | Band label derived from `freq_hz`; see §7.2 |
| `direction` | string | — | Beam direction: `forward` \| `reverse` \| `bidirectional` |
| `moving` | bool | — | `true` while elements are physically moving (feeds the interlock mirror, model §6) |
| `retracted` | bool | — | Latched `true` by a successful retract, cleared by the next accepted frequency/direction change. Bridge-side (the RCU-06 reports no retract flag), so it is **persisted** in `<state_dir>/retract.json` and survives a bridge restart — locked or not, RCU-06 powered or not (a front-panel re-tune is not seen; the latch then stays `true` until the next accepted frequency/direction, the safe direction for the PA arm). While an antenna-mode lock with `retract` is active the bridge also re-establishes it after an RCU-06 power cycle (§6.1); on the RCU-06 back-online edge under such a lock it reads `false` until the re-issued retract is accepted. The latch is set on the retract **ACK**: it says "a retract was accepted", not "the elements have stopped" (see `retract_seq`). The PA arm on PLC #1 blocks on it. |
| `device_online` | bool | — | `true` while the RCU-06 is reachable over serial, `false` when it is not (and the bridge itself is still up). Always present so consumers can distinguish "online" from "no data". Bridge liveness is `/status`, never a state field (model §3). |
| `error` | string | — | Last error message; omitted when empty |
| `lock` | string | — | Antenna-mode lock: `storm` \| `maintenance` while `hf/ant-mode` locks the Ultrabeam (extends refused); omitted when unlocked. Part of the change dedup — a lock change alone publishes a snapshot. |
| `retract_seq` | integer | — | The antenna-mode `lock.seq` whose ensure-retract (§6.1) **finished**: the retract was accepted for that seq, then a poll at least 2 s later read `retracted` and the motors stopped. Omitted while unlocked, while the lock has `retract: false`, and from a new seq, `retract` appearing, or an RCU-06 back-online re-arm until that retract finishes. Unlike `retracted`, a latch left from an earlier lock or the ACK-time snapshot never confirms the current seq — antmode should compare it with its own `lock.seq`. Part of the change dedup. Not in `/meta` `expose` (sequencing data, not an operator field). |

---

## 6. `/cmd` — command (one-shot, execute-then-clear)

Retained JSON, QoS 1. **Published by external systems** (the `antenna-select`
reconciler's band-follow binding, HA, or an operator). ultrabridge subscribes and executes
the command on receipt, then **clears the retained topic** by publishing an empty payload.

This replaced the former re-apply-the-last-command "self-heal" (2026-09-03): with a
persistent session the broker replays the full queued command history on reconnect — not
just the last value — and a retained setpoint outlives the operator that wrote it, so a
restart physically re-drove the antenna with nobody behind it. Three defenses bound the
replay paths (model §8 rules 1–3). None closes them completely — see the residual path
at the end:

1. **QoS-0 subscription** — the broker does not queue QoS-0 traffic for the offline
   session, so nothing accumulates while ultrabridge is disconnected (retained delivery
   is independent of subscription QoS). powerseq subscribes its own `/cmd` at QoS 0 for
   the same reason.
2. **Execute-then-clear** — after every action (executed *or* rejected), the retained
   topic is emptied. Re-applying a command after reconnect is no longer
   ultrabridge's job; the `antenna-select` reconciler re-resolves its band-follow
   decisions, as it already does for the PA and tuner. Best-effort by construction:
   the clear is a separate publish after the serial exchange, so a connection drop
   in that window can leave the retained command alive — it replays once on the
   next reconnect and is then cleared.
3. **`ts` staleness gate** — see below.

**Residual replay path (not closed):** a command published while ultrabridge is
offline is retained on the broker and is delivered on its reconnect — independent of
subscription QoS — and executes once, age-unbounded unless the producer stamps `ts`.
No current producer does (the reconciler's band-follow, HA, and the console all
publish ts-less payloads). This is accepted because such a value is by construction
the latest intent anyone wrote, band-follow re-resolves on its own, and the command
is cleared after execution. If a producer starts stamping `ts`, the gate below bounds
it to 30 s.

### Command payloads

Commands MAY carry a `ts` (RFC 3339 producer timestamp). When present, a command older
than 30 s (or stamped in the future) is dropped before it reaches the serial device.
Producers without `ts` are still accepted (existing producers predate the field).

**Set frequency (Hz):**
```json
{"action": "frequency", "freq_hz": 21225000, "ts": "2026-09-03T12:00:00Z"}
```

**Set direction:**
```json
{"action": "direction", "value": "reverse"}
```
Valid values: `forward`, `reverse`, `bidirectional`. (`{"action":"mode",...}` is still
accepted as a deprecated alias for backward compatibility.)

**Jump to band centre:**
```json
{"action": "band", "value": "15m"}
```
Valid bands: `20m`, `17m`, `15m`, `12m`, `10m`, `6m`. Tunes to IARU Region 1
centre frequency (see §7.1), preserving current direction.

**Retract elements:**
```json
{"action": "retract"}
```
One-shot physical command (storm safety). After executing, ultrabridge clears the
retained `/cmd` topic (publishes an empty payload).

All commands — frequency, direction, band, retract, and any rejected or unparseable
payload (including one refused by the antenna-mode lock) — are cleared from the retained
topic after the worker has acted on them, so no command re-executes on the next restart
or reconnect.

### 6.1 Antenna-mode lock (storm / maintenance)

The station antenna mode is owned by the `hf/ant-mode` logic slot (component `antmode`,
contract `../antmode/docs/ant-mode-mqtt-api.md`). ultrabridge reads its retained `/state`
through `shared/antmode.Parse` and **enforces** the lock itself:

| While `ant_mode` is `storm` or `maintenance` | |
|---|---|
| `frequency`, `band`, `direction` (and the `mode` alias) | **refused** — every one of them drives the elements out. The command never reaches the RCU-06; the retained `/cmd` is still cleared. One Warn per refusal streak (reset on unlock). |
| `retract` | always allowed |
| HTTP API | `/api/frequency` and `/api/mode` answer **423 Locked**; `/api/retract` works; the status page shows the lock |
| `lock.retract: true` | the bridge **retracts by itself**: on a new `lock.seq` (or `retract` appearing on the current one), when the RCU-06 comes back online (a power cycle may re-extend; this re-issues a retract still waiting for its motors and clears `retracted` until the new one is accepted), and at bridge start once the lock is known — then re-checks on every 2 s poll until `retracted` and not `moving` on a poll at least 2 s after the retract was accepted — that finished retract is published as `retract_seq`. Failures retry on the next poll (one Warn per streak). |
| `lock.retract: false` | extends refused, **no** retract — antmode grounds the antenna switch first and adds `retract` once the switch reports `off`. |

Reading rules: no retained ant-mode state (or an empty payload) = **normal** (the bridge is
usable before antmode exists); a valid locked snapshot is enforced even while
`hf/ant-mode/status` is offline (a dead owner never unlocks); a malformed payload keeps the
last known lock and logs an Error. `ts` is not checked.

- **Subscription:** QoS 0 (the persistent session would otherwise queue a backlog of lock
  transitions while the bridge is offline), subscribed on every (re)connect just before
  `/cmd`; its message route is registered before the initial connect so a delivery on the
  resumed session is never dropped.
- **Startup hold:** for up to 3 s after start, extends are refused until the first
  ant-mode state arrives, so a retained `/cmd` replayed on connect cannot slip in ahead of
  a storm lock.
- **Leaving the mode** does not re-extend: the elements come back out only through
  antennaselect's band-follow (which re-sends the frequency on unlock; the deadband does
  not swallow a same-band retune between an accepted retract and the next accepted
  frequency — this bypass is persisted with the latch and survives both a bridge restart
  and the back-online clear of `retracted`).
- **Config:** `[mqtt] ant_mode_slot` (default `"ant-mode"`; `""` disables the lock);
  top-level `state_dir` (default `"/var/lib/ultrabridge"`, the systemd StateDirectory;
  `""` keeps the retract latch in memory only).

---

## 7. Enumerations and tables

### 7.1 Band centre frequencies

| Band | Centre (kHz) | Centre (Hz) |
|------|-------------|-------------|
| `20m` | 14175 | 14175000 |
| `17m` | 18118 | 18118000 |
| `15m` | 21225 | 21225000 |
| `12m` | 24940 | 24940000 |
| `10m` | 28850 | 28850000 |
| `6m`  | 51000 | 51000000 |

### 7.2 Band labels from frequency

Derived from `freq_hz` at publish time:

| Band | Low (Hz) | High (Hz) |
|------|----------|-----------|
| `160m` | 1,800,000 | 1,999,999 |
| `80m` | 3,500,000 | 3,999,999 |
| `40m` | 7,000,000 | 7,299,999 |
| `30m` | 10,100,000 | 10,149,999 |
| `20m` | 14,000,000 | 14,349,999 |
| `17m` | 18,068,000 | 18,167,999 |
| `15m` | 21,000,000 | 21,449,999 |
| `12m` | 24,890,000 | 24,989,999 |
| `10m` | 28,000,000 | 29,699,999 |
| `6m`  | 50,000,000 | 53,999,999 |
| `band-<N>` | — | outside known allocations |

---

## 8. Home Assistant auto-discovery

> **Two paths now exist (integration model §9).** The preferred path is the standalone
> `hadiscovery` consumer, which reads this bridge's `expose` block from `/meta` and
> renders HA discovery centrally. The legacy **embedded** discovery below is retained for
> reversibility but is **gated off** by `[mqtt] publish_ha_discovery = false` (the
> default). Set it `true` only to fall back during migration. Once `hadiscovery` is
> proven, the embedded discovery code (and this section's embedded table) will be deleted.
>
> The two paths use **different node IDs**: embedded uses `<station>-<slot>` (`hf-ant-ctrl`);
> `hadiscovery` uses `muehle-hf-ant-ctrl` (the sanitized slot address). Switching moves
> entities to a new HA device — clear the old `hf-ant-ctrl` discovery topics to avoid
> duplicates.

### Standalone discovery via `hadiscovery` (preferred, default)

With `publish_ha_discovery = false` (default), ultrabridge publishes only the `expose` block in
`/meta`. The `hadiscovery` service renders HA discovery under node ID `muehle-hf-ant-ctrl`.
A HA `number`/`select` both displays state from `/state` **and** commands via `/cmd`, so the
read and write surfaces collapse into one entity each — **5 entities** instead of the
embedded path's 8:

| Entity | Component | Object ID | Source |
|--------|-----------|-----------|--------|
| Frequency | `number` | `freq_hz` | state `/state.freq_hz` + cmd `{"action":"frequency","freq_hz":...}` |
| Band | `select` | `band` | state `/state.band` + cmd `{"action":"band","value":...}` (options from `capabilities.bands`) |
| Direction | `select` | `direction` | state `/state.direction` + cmd `{"action":"direction","value":...}` |
| Moving | `binary_sensor` | `moving` | state `/state.moving` |
| Retract | `button` | `retract` | cmd `{"action":"retract"}` |

Discovery topics: `homeassistant/<component>/muehle-hf-ant-ctrl/<object_id>/config`. See
`../hadiscovery/docs/discovery-mqtt-api.md` for the full neutral→HA mapping.

### Embedded discovery (legacy, gated, default off)

ultrabridge publishes discovery configs under `homeassistant/` (configurable via
`mqtt.discovery_prefix`). All entities read from the single `/state` topic using
`value_template`. Node ID: `<station>-<slot>` (e.g. `hf-ant-ctrl`).

> Embedded HA discovery is a **documented deviation** from design invariant §9 (no core
> component publishes under a consumer tree). It is non-load-bearing and gated off by
> default; the target is the standalone `hadiscovery` consumer, at which point this
> section disappears.

| Entity | Component | Object ID | Template | Notes |
|--------|-----------|-----------|----------|-------|
| Frequency | `sensor` | `frequency` | `{{ value_json.freq_hz }}` | unit: Hz |
| Band | `sensor` | `band` | `{{ value_json.band }}` | |
| Direction | `sensor` | `direction` | `{{ value_json.direction }}` | |
| Moving | `binary_sensor` | `moving` | `{{ 'ON' if value_json.moving else 'OFF' }}` | |
| Frequency set | `number` | `frequency_set` | `{{ value_json.freq_hz }}` | cmd via `/cmd` |
| Direction set | `select` | `direction_set` | `{{ value_json.direction }}` | cmd via `/cmd` |
| Band set | `select` | `band_set` | `{{ value_json.band }}` | cmd via `/cmd` |
| Retract | `button` | `retract` | — | payload: `{"action":"retract"}` |

ultrabridge re-publishes embedded discovery whenever Home Assistant announces `online` on
`homeassistant/status` (only when the gate is on; otherwise `hadiscovery` handles rebirth).

---

## 9. Typical interaction flows

**Read current state on startup:**
Subscribe to `<slot>/#`. The broker immediately delivers retained `/meta`,
`/state`, and `/status`.

**Command antenna to 17m, confirm:**
1. Publish retained to `<slot>/cmd`: `{"action":"band","value":"17m"}`
2. Watch `<slot>/state` → `moving:true` then `moving:false`
3. Confirm `freq_hz == 18118000`

**Change direction:**
1. Publish retained to `<slot>/cmd`: `{"action":"direction","value":"bidirectional"}`
2. Confirm via `/state` `direction == "bidirectional"`

**Antenna-mode lock (storm):**
1. antmode publishes `{"ant_mode":"storm","lock":{"seq":17,"ground":true,"retract":false,…}}`
   → `/state.lock == "storm"`; a `band` `/cmd` is refused and cleared.
2. Once the switch is grounded antmode adds `"retract":true` → ultrabridge retracts:
   `moving:true, retracted:true` then `moving:false`, then `retract_seq: 17`.
3. On `normal`, `lock` disappears from `/state`; band-follow re-extends.

**Detect RCU fault:**
- `/state` contains `"device_online":false` and `"error":"..."` while ultrabridge is still
  running. `/status` stays `online`.
- If ultrabridge itself crashes or loses broker connection, `/status` → `offline`
  (LWT fires).
