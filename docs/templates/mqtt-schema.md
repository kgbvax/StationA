# MQTT schema — [Component Name]

> **Template.** Copy this file to `<project>/docs/<component>-mqtt-api.md` and fill
> in the sections below. Replace all `[...]` placeholders. Delete this note.

This document describes the MQTT interface exposed by **[component]** (the [device/role]
bridge). [component] implements the `[slot]` slot of the station integration model
(`../../docs/station-integration-model.md`).

It is the authoritative on-the-wire contract — derived from `[the source file(s) that build the topics and payloads, e.g. internal/mqtt/client.go or internal/bridge/bridge.go]`.

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, e.g. `tcp://host:1883`) |
| Authentication | Username/password if the broker requires it |
| Clean session | [**Yes** or **No**: state which]. A stateless consumer whose inputs are all retained uses **Yes**: retained planes replay on every subscribe (antennaselect, antmode, beamsteer, and powerseq since 76b4cb6, after a resumed persistent session on the HA broker stopped delivering QoS-1 messages live on 2026-10-18). If **No** (persistent session), the `/cmd` subscription MUST be QoS 0 so the broker cannot queue a backlog for offline replay (model §8 rule 2). The per-component choice is still open (`../../docs/conventions/runtime-library.md`, "Clean session") |
| Auto-reconnect | [component] reconnects automatically — and survives a broker outage at boot: the initial connect retries indefinitely, or the process exits non-zero so systemd restarts it (§8.1 item 10) |
| Client ID | derived from the slot address, e.g. `[site]-[station]-[slot]` (configurable via `mqtt.client_id`) |

---

## 2. Topic addressing

All topics are addressed as:

```
<site>/<station>/<slot>/<suffix>
```

Configured via `[mqtt]` in `config.toml`:

```toml
site    = "muehle"     # physical site
station = "hf"         # transmitting entity
slot    = "[slot]"     # role (default: [default-slot])
```

Example for the Mühle HF station:

```
muehle/hf/[slot]/meta
muehle/hf/[slot]/state
muehle/hf/[slot]/status
muehle/hf/[slot]/cmd      # omit if component is read-only
```

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | component → bus | birth certificate: identity + capabilities |
| `/state` | yes | component → bus | live state (JSON snapshot) |
| `/status` | yes | broker LWT | liveness: `online` / `offline` |
| `/cmd` | [yes/no] | bus → component | desired state / command (omit if read-only) |

---

## 3. `/status` — liveness

Plain string, retained, QoS 1.

| Value | When |
|-------|------|
| `online` | published on every (re)connect |
| `offline` | broker Last Will on unclean disconnect; published on clean shutdown |

---

## 4. `/meta` — birth certificate

Retained JSON, published on every (re)connect and again whenever the identity or capabilities change (e.g. after the device handshake).

```json
{
  "schema": "1.0",
  "role": "[role]",
  "device": {
    "model": "[model]",
    "serial": "[serial-if-known]",
    "firmware": "[firmware-if-known]"
  },
  "link": "[ethernet|serial|wifi|none]",
  "location": "[bauwagen]",
  "host": "[compute-node]",
  "capabilities": {
    "[key]": "[value]"
  },
  "expose": {
    "device":  { "name": "[name]", "model": "[model]", "manufacturer": "[mfr]",
                 "sw_version": "[firmware-if-known]", "area": "[area]" },
    "fields":  [
      { "key": "[state-field]", "name": "[Name]", "type": "[number|enum|boolean|string]",
        "unit": "[Hz|...]", "class": "[frequency|...]", "state_class": "[measurement|...]",
        "options_ref": "[capabilities-key]", "writable": true,
        "command": { "action": "[action]", "value_key": "[cmd-key]", "value_type": "[string|int|float]" } }
    ],
    "actions": [
      { "key": "[action]", "name": "[Name]", "command": { "action": "[action]" } }
    ]
  }
}
```

| Field | Notes |
|-------|-------|
| `role` | canonical role name (see station-integration-model §4) — never a device name |
| `device` | device slots only; omit `serial`/`firmware` if not available. Logic slots omit `device` (model §3/§3.1; every current logic slot does) |
| `link` | device slots: the transport (`ethernet`, `serial`, `wifi`, `websocket`, …); logic slots: `none` |
| `location` / `host` | from config — deployment facts, never code constants |
| `capabilities` | [describe what capabilities this component declares] |
| `expose` | OPTIONAL (model §3.1, Appendix C): the slot's consumer-neutral field surface. Omit entirely if the slot does not want to be discovered by `expose`-driven consumers. `options_ref` points into `capabilities` so enum lists stay single-sourced. No consumer-specific vocabulary (no `device_class`, no templates, no `payload_on/off`) — consumers render their own representation from this. |

---

## 5. `/state` — live state

Retained JSON snapshot, QoS 1. Published on every field change and on every (re)connect[; plus a periodic heartbeat every N s if a consumer keys liveness on message recency, e.g. flexbridge's 5 s heartbeat for the PA-arm interlock].

```json
{
  "ts":      "2026-07-06T12:34:56Z",
  "[field]": "[value]"
}
```

| Field | Type | Unit | Notes |
|-------|------|------|-------|
| `ts` | string | — | RFC 3339 UTC timestamp of this publish |
| `device_online` | bool | — | device slots only: `false` while the fronted hardware is unreachable and the bridge is up. Every deployed Go device bridge publishes it explicitly (`true` or `false`); the ESPHome ant-switch omits it. Consumers MUST treat a missing field as `true` (model §3; runtime-library.md "device_online form") |
| `error` | string | — | optional: human-readable device fault |
| `[field]` | [type] | [unit] | [description] |

Publish triggers:
- [list what causes a state publish]

---

## 6. `/cmd` — desired state

> **Omit this section if the component is read-only (no /cmd topic).**

[Retained | Not retained] JSON. Published by external systems (reconciler, HA, operator). Under a
persistent session the **subscription** is QoS 0 (§8 rule 2 — see §1); publishes stay
QoS 1.

Pick exactly one regime per topic (model §8):

- **Not retained (default, model §8)** — a one-shot command published without the
  retain flag. Nothing replays after a restart or reconnect. Use this unless the slot
  is a physical actuator whose position must survive a software restart.

The next two regimes are the §8 self-healing-actuator exception (a retained
steady-state intent, or a retained one-shot that is cleared after execution):

- **Steady-state intent** — the payload names a desired state to converge to (a
  power, a permit, an idempotent mode). Retain it; every writer publishes retained so
  the topic always tracks the latest intent, and [component] re-applies it on every
  reconnect/restart. Never clear it, never age-gate it — an older retained intent is
  still the current intent.
- **One-shot command** — the payload is a momentary intent (a `retract`, a jog).
  Retain it for delivery robustness, but [component] publishes an **empty retained
  payload** after every execution or rejection (§8 rule 1) so nothing re-fires on the
  next reconnect. Payloads SHOULD carry an RFC 3339 `ts`; [component] MUST drop a
  stamped command older than a small bound (tens of seconds) or stamped in the future
  beyond a small clock-skew allowance (§8 rule 3). Residual replay path: a command
  published while [component] is offline replays **once** on reconnect (latest
  intent, executed once, then cleared).

### Command payloads

**[Action name]:**
```json
{"action": "[action]", "[param]": "[value]"}
```

[Describe each action.]

---

## 7. Enumerations and tables

### [Table name]

| Value | Description |
|-------|-------------|
| `[value]` | [description] |

---

## 8. Home Assistant discovery

Two paths now exist (integration model §9). **The standalone `hadiscovery` consumer is
the preferred path** for new components; the legacy embedded path is retained only as a
config-gated fallback during migration.

### 8.1 Preferred — standalone `hadiscovery` consumer

`hadiscovery` (`muehle/hf/discovery`, runs on `scmino`) is a passive service that reads each
slot's consumer-neutral `expose` block from `/meta` (§4 above; model §3.1, Appendix C) and
renders HA discovery. The component itself contains **no HA knowledge** — it only publishes
`expose` in its `/meta`. `hadiscovery` owns the neutral→HA mapping, the discovery topic
layout, and the `homeassistant/status` rebirth behavior; see
`hadiscovery/docs/discovery-mqtt-api.md` for that mapping.

- Discovery node ID: `<site>-<station>-<slot>` (e.g. `muehle-hf-[slot]`); HA device
  `identifiers` = `[<node ID>]` — one HA device per slot.
- Discovery topic: `<discovery_prefix>/<component>/<node ID>/<object ID>/config`.
- No component-side config is required for this path — just publish `expose`.

To adopt: populate the `expose` block in `/meta` (§4) and ensure `hadiscovery` is running.
This is the only path new components should use.

### 8.2 Legacy — embedded discovery (config-gated, off by default)

> **Deprecated deviation from design invariant §9.** Retained only as a migration
> fallback; slated for deletion once `hadiscovery` is proven live. A component that ships
> embedded discovery MUST gate it behind `[mqtt] publish_ha_discovery = false` (default
> false) so that with it off (or HA absent) the canonical planes are unaffected and the
> station runs identically. Omit this subsection for new components.

When `publish_ha_discovery = true`, [component] publishes discovery configs under
`homeassistant/` (configurable via `mqtt.discovery_prefix`). Node ID: `<station>-<slot>`
(e.g. `hf-[slot]`); HA device `identifiers` differ from the `hadiscovery` path.

All entities read from the single `/state` topic using `value_template`.

| Entity | Component | Object ID | `value_template` | Notes |
|--------|-----------|-----------|-----------------|-------|
| [name] | `sensor` | `[id]` | `{{ value_json.[field] }}` | [notes] |

[component] re-publishes discovery whenever Home Assistant announces `online` on
`homeassistant/status` (this subscription is also gated behind `publish_ha_discovery`).

> **Switching paths moves entities to a new HA device.** The two paths use different node
> IDs and `identifiers`, so they are separate HA devices, not a rename. When migrating
> from embedded to `hadiscovery`, clear the old embedded discovery topics (publish an empty
> retained payload to each old `.../config` topic) so HA does not keep ghost entities
> alongside the new `muehle-hf-[slot]` device.

---

## 9. Typical interaction flows

**Read current state on startup:**
Subscribe to `<slot>/#`. The broker immediately delivers retained `/meta`, `/state`,
and `/status`.

**[Common command scenario]:**
1. [Step 1]
2. [Step 2]
3. [Step 3 — confirm via /state]

**Detect component fault:**
- `/state` contains `"device_online":false` and `"error":"..."` while the bridge is
  still running (the fronted hardware is unreachable). `/status` stays `online`.
- If the bridge crashes or loses broker connection, `/status` → `offline` (LWT fires).
- Liveness itself is never a `/state` field — see model §3.
