# antenna-select MQTT API

This document describes the MQTT interface of **antennaselect** — the antenna-selection
reconciler. It implements the `antenna-select` logic slot of the station integration model
(`../../docs/station-integration-model.md` §5, §7.1).

> **Status: implemented** (pending on-device deploy). This documents the slot's wire
> surface and decision logic as built — derived from `internal/mqtt` and
> `internal/reconcile`.

antenna-select is a **logic slot** — it has *no device*. It subscribes to state from the
radio, the station, the switch, and the operator; applies a priority ladder over a wiring
map and band policy; and emits one intent stream to the `ant-switch`. It is the miniature
of the multi-multi contention mechanism (model §5): more inputs later, same ladder.

**Plane discipline (model §1):** it reacts to *state* and emits *intent*. It never assumes
a command took effect because it was sent — it watches `ant-switch/state` to confirm.

---

## 1. Topic addressing

```
muehle/hf/antenna-select/{meta,state,status,cmd}
```

Configured via `[mqtt]` in `config.toml` (`slot = "antenna-select"`).

| Suffix | Retained | Direction | Purpose |
|--------|----------|-----------|---------|
| `/meta` | yes | reconciler → bus | birth certificate |
| `/state` | yes | reconciler → bus | resolved decision + why (JSON snapshot) |
| `/status` | yes | broker LWT | liveness |
| `/cmd` | yes | bus → reconciler | operator hold / release (§4) |

---

## 2. `/meta` — birth certificate

```json
{
  "schema": "1.0",
  "role": "reconciler",
  "link": "none",
  "location": "bauwagen",
  "host": "shari",
  "capabilities": {
    "controls": "ant-switch",
    "follows": { "ultrabeam": "ant-ctrl" },
    "ladder": ["idle", "operator", "auto"]
  },
  "expose": {
    "device": { "name": "Antenna selector" },
    "fields": [
      { "key": "source", "name": "Source", "type": "enum", "options_ref": "ladder" },
      { "key": "target", "name": "Target", "type": "string" },
      { "key": "mode",   "name": "Mode",   "type": "string" }
    ]
  }
}
```

`role` is `reconciler` (a logic slot, model §4). `location` and `host` come from config
(deployment facts, model §3/§7.3). `follows` documents the configured controller map
(`[band_follow]`); it is omitted when band-follow is disabled.

### The `expose` block — consumer-neutral field surface

`expose` (integration model §3.1, Appendix C) is the **consumer-neutral** description of
this slot's observable field surface — no consumer vocabulary. The standalone `hadiscovery`
consumer renders Home Assistant discovery from it; other consumers can render theirs from
the same block. The reconciler reacts to state and emits intent (model §1), so all of its
`/state` fields are **read-only sensors** — no writable fields, no actions. `source` is an
enum whose options resolve via `options_ref` into `capabilities.ladder` (the priority
tiers); `target` and `mode` are strings with no fixed capability list to reference.

---

## 3. `/state` — resolved decision

Retained JSON snapshot, QoS 1. Published whenever the resolution changes.

```json
{
  "ts":     "2026-07-06T12:34:56Z",
  "mode":   "auto",
  "target": "p3",
  "source": "auto"
}
```

| Field | Type | Notes |
|-------|------|-------|
| `ts` | string | RFC 3339 UTC |
| `mode` | string | `auto` \| `manual`. **Derived**: `manual` whenever an operator hold is active, else `auto`. There is no separate auto/manual switch — the presence of a hold *is* manual. |
| `target` | string | the port the reconciler currently wants: `off` \| `p1`..`p5` |
| `source` | string | *why* the target is what it is: `idle` \| `operator` \| `auto` (model §5). Published so the live config documents the reason, not just the value. |

`target` is what the reconciler *wants*; the switch's own `selected`/`settled` report what
*is*. Consumers wanting ground truth read `ant-switch/state`.

---

## 4. `/cmd` — operator hold / release

Retained JSON, QoS 1. This is the UI-agnostic operator surface (model §9) — any client
(CLI, web, M5Stack button, HA) may publish it.

```json
{ "request": "p2" }
```

| `request` | Effect |
|-----------|--------|
| `p1`..`p5` \| `off` | engage an **operator hold** (ladder tier 2) on that port |
| `auto` | release the hold; return to band-policy selection (tier 3) |

Retained so an operator hold survives a reconciler restart (self-healing). Note the
deliberate surprise (model §10): an operator hold is still overridden by station-inactive
(tier 1) — walking away wins over a forced selection.

---

## 5. Resolution ladder (model §5)

Highest asserting tier wins. Re-evaluated on every relevant input change:

```
1  idle:      station.activity == inactive   →  target = off,  source = idle
2  operator:  hold present (/cmd != auto)     →  target = request, source = operator
3  auto:      band_policy(radio.band)          →  target = port,  source = auto
```

- **Tier 1 (idle)** is the safe default and overrides everything, including an operator
  hold. The switch's fail-safe-to-ground default covers power loss independently.
- **Tier 3 (auto)** maps the radio's current band to a port via `[band_policy]`; unmatched
  bands (incl. 160m) use the configured `fallback` (see config).

---

## 6. Cold-switch sequencing (model §6 — safety-critical)

The ant-switch is `hot_switch: false`. When the resolved `target` differs from
`ant-switch.state.selected`, the reconciler **must not** move the port under TX:

1. If `radio.state.tx == "tx"`, hold the change (do not emit `select`); wait for RX.
2. Emit `ant-switch/cmd {"select": <target>}`.
3. Confirm via `ant-switch/state.selected`; completion is `settled == true`.

**Implemented today:** step 1 (defer under TX) and confirmation via `selected`. The
reconciler receives `settled` but does not yet gate anything on it — the settled-wait
handshake lands together with the antswitchbridge implementation (see the stationa
compliance review backlog). `radio.state.tuning` is likewise not yet an input.

Enforcement remains **hardware** — the series interlock
`radio → rx-loop-ctrl → ant-ctrl → pa` (model §6). The reconciler owns the *ordering*
only, and never sits in the enforcement path. **Never trust retained state for this** —
check `radio/status` liveness before acting on `radio/state` (model §10).

---

## 7. Soft band-follow — the controller map (model §4, §7.1)

Because the reconciler already holds `radio/state`, it also drives a controlled antenna
to track the radio **when that antenna is the selected one**. Which antenna, and which
controller slot, is site configuration (`[band_follow]` — at Mühle: resource `ultrabeam`,
slot `ant-ctrl`), never code:

- Emit `<band_follow.slot>/cmd {"action":"frequency","freq_hz": <radio.freq_hz>}` on
  radio frequency change while `target == <band_follow.resource>'s port`.

This is the only non-ant-switch binding in scope here. The PA bindings (`pa.set_band`,
`pa-arm`) are the same reconciler's eventual responsibility but are **out of scope** for the
antenna work.

---

## 8. Subscriptions summary

| Topic | Fields used |
|-------|-------------|
| `muehle/hf/radio/state` | `band`, `freq_hz`, `tx` (`tuning`: future input, not yet used) |
| `muehle/hf/radio/status` | liveness gate before acting |
| `muehle/hf` (station node) | `activity` (`active`\|`inactive`) — tier 1 |
| `muehle/hf/antenna-select/cmd` | operator `request` — tier 2 |
| `muehle/hf/ant-switch/state` | `selected` — confirm (`settled`: received, gating is backlog) |

**Emits:** `muehle/hf/ant-switch/cmd` (`select`); `muehle/hf/<band_follow.slot>/cmd`
(`frequency`, while the followed antenna is selected — `ant-ctrl` at Mühle).

**Dependency not built here:** the station `activity` flag needs a publisher (operator/HA
sets `muehle/hf`). If absent, treat as `active` and log — never silently assume inactive.
