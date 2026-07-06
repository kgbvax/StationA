# MQTT schema — [Component Name]

> **Template.** Copy this file to `<project>/docs/<component>-mqtt-api.md` and fill
> in the sections below. Replace all `[...]` placeholders. Delete this note.

This document describes the MQTT interface exposed by **[component]** (the [device/role]
bridge). [component] implements the `[slot]` slot of the station integration model
(`../../docs/station-integration-model.md`).

It is the authoritative on-the-wire contract — derived from `internal/mqtt/client.go`.

---

## 1. Connection

| Property | Value |
|----------|-------|
| Protocol | MQTT 3.1.1 (plain TCP, e.g. `tcp://host:1883`) |
| Authentication | Username/password if the broker requires it |
| Clean session | **No** — subscriptions survive restarts |
| Auto-reconnect | [component] reconnects automatically |
| Client ID | `[client-id]` (configurable via `mqtt.client_id`) |

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

Retained JSON, published once per connect cycle.

```json
{
  "schema": "1.0",
  "role": "[role]",
  "device": {
    "model": "[model]",
    "serial": "[serial-if-known]",
    "firmware": "[firmware-if-known]"
  },
  "link": "[ethernet|serial|wifi]",
  "location": "[bauwagen]",
  "capabilities": {
    "[key]": "[value]"
  }
}
```

| Field | Notes |
|-------|-------|
| `role` | canonical role name (see station-integration-model §4) |
| `device` | omit `serial`/`firmware` if not available |
| `capabilities` | [describe what capabilities this component declares] |

---

## 5. `/state` — live state

Retained JSON snapshot, QoS 1. Published only when a field value changes.

```json
{
  "ts":      "2026-07-06T12:34:56Z",
  "[field]": "[value]"
}
```

| Field | Type | Unit | Notes |
|-------|------|------|-------|
| `ts` | string | — | RFC 3339 UTC timestamp of this publish |
| `[field]` | [type] | [unit] | [description] |

Publish triggers:
- [list what causes a state publish]

---

## 6. `/cmd` — desired state

> **Omit this section if the component is read-only (no /cmd topic).**

[Retained / Not retained] JSON, QoS 1. Published by external systems (reconciler, HA,
operator).

[If retained:] Because `/cmd` is retained, [component] re-applies the last command on
reconnect — providing self-healing behaviour after restarts. One-shot physical commands
(e.g. `retract`) clear the retained topic after execution.

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

## 8. Home Assistant auto-discovery

[component] publishes discovery configs under `homeassistant/` (configurable via
`mqtt.discovery_prefix`). Node ID: `<station>-<slot>` (e.g. `hf-[slot]`).

All entities read from the single `/state` topic using `value_template`.

| Entity | Component | Object ID | `value_template` | Notes |
|--------|-----------|-----------|-----------------|-------|
| [name] | `sensor` | `[id]` | `{{ value_json.[field] }}` | [notes] |

[component] re-publishes discovery whenever Home Assistant announces `online` on
`homeassistant/status`.

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
- `/state` contains `"offline":true` and `"error":"..."` while the bridge is still
  running. `/status` stays `online`.
- If the bridge crashes or loses broker connection, `/status` → `offline` (LWT fires).
