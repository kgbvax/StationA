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
    "follows": { "ultrabeam": "ant-ctrl", "pa": "pa", "tuner": "tuner" },
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
(deployment facts, model §3/§7.3). `follows` documents the radio-follow bindings this
reconciler drives: the controller map (`[band_follow]`: resource → controller slot) and,
when enabled, the PA band-follow (`[pa_follow]`: `pa` → `pa`, §7.1) and the tuner in-line
follow (`[tuner_follow]`: `tuner` → `tuner`, §7.1). It is omitted entirely when all three
are disabled.

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
  "target": "port4",
  "source": "auto"
}
```

| Field | Type | Notes |
|-------|------|-------|
| `ts` | string | RFC 3339 UTC |
| `mode` | string | `auto` \| `manual`. **Derived**: `manual` whenever an operator hold is active, else `auto`. There is no separate auto/manual switch — the presence of a hold *is* manual. |
| `target` | string | the port the reconciler currently wants: `off` \| `port1`..`port6` |
| `source` | string | *why* the target is what it is: `idle` \| `operator` \| `auto` (model §5). Published so the live config documents the reason, not just the value. |

`target` is what the reconciler *wants*; the switch's own `selected`/`settled` report what
*is*. Consumers wanting ground truth read `ant-switch/state`.

---

## 4. `/cmd` — operator hold / release

Retained JSON, QoS 1. This is the UI-agnostic operator surface (model §9) — any client
(CLI, web, M5Stack button, HA) may publish it.

```json
{ "request": "port2" }
```

| `request` | Effect |
|-----------|--------|
| `port1`..`port6` \| `off` | engage an **operator hold** (ladder tier 2) on that port |
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
  bands (incl. 160m, and the out-of-band `gen` marker) use the configured `fallback` (see
  config). An **empty band** (`""` — flexbridge's transient "no slice reported yet" /
  reconnect-Reset state) is *not* treated as unmatched: the reconciler **holds the last
  selection** rather than chattering to the fallback. Only a known-but-unmatched band
  reaches the fallback; the empty/transient case holds. Auto is skipped entirely (target
  empty → hold) when the radio is not online (see §6 / §10).

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
act on `radio/state` only when the radio is online (model §10): `radio/status` (broker
LWT) must be `online` *and* `radio/state.device_online` must be `true`. `/status` alone is
the bridge-process liveness, not the radio-link liveness — it stays `online` while
flexbridge is up but the radio link is down, so it cannot gate on its own.
`device_online` (which flexbridge always publishes) is the radio-link signal; the
reconciler trusts radio-derived fields only when both are up.

---

## 7. Soft band-follow — the controller map (model §4, §7.1)

Because the reconciler already holds `radio/state`, it also drives a controlled antenna
to track the radio **when that antenna is the selected one**. Which antenna, and which
controller slot, is site configuration (`[band_follow]` — at Mühle: resource `ultrabeam`,
slot `ant-ctrl`), never code:

- Emit `<band_follow.slot>/cmd {"action":"frequency","freq_hz": <radio.freq_hz>}` on
  radio frequency change while `target == <band_follow.resource>'s port`.

### 7.1 PA band-follow — `pa.set_band ← radio.band` (model §7.1 soft binding)

The reconciler also pre-positions the **PA** to the radio's band. The ACOM 1200S auto-bands
by sensing the RF drive (`band_source: rf_sense`), which by ACOM design **trips the amp on
the 1st transmit on a new band** — the amp is still on the old band when RF appears. Pushing
the band during RX, before the operator transmits, avoids that trip.

This binding is **not** the controller map: the PA is always in the RF path, so it is **not
gated on antenna selection** — it fires whenever the radio is online and reporting a band.
No TX guard is applied; PA hot-switch protection is handled in hardware (the same series
interlock the antenna cold-switch relies on, model §6). Gated by `[pa_follow].enabled`
(default `true` on shari; `false` in the built-in defaults).

- Emit `<pa_follow.slot>/cmd {"action":"set_band","value": <radio.band>}` on radio band
  change while `radio.status == online`. **Not retained** (QoS 1) — the PA `/cmd` contract
  is not retained (see `../acombridge/docs/pa-mqtt-api.md`); self-heal comes from this
  reconciler re-resolving on the retained `radio/state` replay at its own reconnect, not
  from a retained `pa/cmd`. Deduped against the last band pushed.

`pa-arm` remains out of scope here (no MQTT keying path — the ACOM is keyed in hardware,
`key_input: hardware`). The `set_band` vocabulary matches acombridge's existing `/cmd`
contract; the PA bridge needs no change for this feature — it already dispatches `set_band`
and dedups `current == target`.

**Edge cases (v1):** if the amp is OFF when the band changes, `set_band` is rejected by
acombridge (`current band unknown`) and the amp later auto-bands by RF sense on its next TX
(the trip returns for that one transition); re-emitting on PA online is a future
enhancement. 60m is not an ACOM band, so a 60m `set_band` is rejected by acombridge and
logged once (deduped here); harmless.

### 7.2 Tuner in-line follow — `tuner.set_inline ← band_policy` (model §7.1 soft binding)

The reconciler also engages the **ATU** (ATR-1000, slot `hf/tuner`) in-line for the
non-resonant bands, and bypasses it otherwise — so leaving a non-resonant band drops the
ATU out of line. This closes the model §10 residual (30/60/80/160 m on the fan-dipole were
routed to a non-resonant antenna with the ATU *assumed* but never driven).

Unlike the PA binding, this **is** gated on antenna selection: the ATU only matters when
its served resource (`[tuner_follow].resource`, `fan-dipole` at Mühle) is the resolved
target. The ATU engages when the resource is selected **and** the band is in
`[tuner_follow].atu_bands` (`30m`, `60m`, `80m`, `160m` at Mühle); it is bypassed for any other
selection or band. Gated on radio online + a known band (§10). The reconciler's cold-switch
sequencing already withholds a port change during TX, so the ATU is not re-keyed mid-TX.

- Emit `<tuner_follow.slot>/cmd {"action":"set_inline","value": <bool>}` on radio band or
  selection change while `radio.status == online`. **Not retained** (QoS 1) — self-heal
  comes from this reconciler re-resolving on the retained `radio/state` replay at its own
  reconnect, not from a retained `tuner/cmd`. Deduped against the last inline value pushed.
  `true` = ATU in line, `false` = bypass.

Gated by `[tuner_follow].enabled` (default `true` on shari; `false` in the built-in
defaults). The `set_inline` vocabulary matches atr1k-tuner-bridge's `/cmd` contract.

---

## 8. Subscriptions summary

| Topic | Fields used |
|-------|-------------|
| `muehle/hf/radio/state` | `band`, `freq_hz`, `tx`, `device_online` (radio-link liveness) (`tuning`: future input, not yet used). `freq_hz` change or `tx == "tx"` marks the station active (idle-timeout input) |
| `muehle/hf/radio/status` | bridge liveness (LWT) — one half of the radio-online gate |
| `muehle/hf/antenna-select/cmd` | operator `request` — tier 2 |
| `muehle/hf/ant-switch/state` | `selected` — confirm (`settled`: received, gating is backlog) |

**Emits:** `muehle/hf/ant-switch/cmd` (`select`); `muehle/hf/<band_follow.slot>/cmd`
(`frequency`, while the followed antenna is selected — `ant-ctrl` at Mühle);
`muehle/hf/<pa_follow.slot>/cmd` (`set_band`, not retained — `pa` at Mühle, while the
radio is online and reporting a band; gated by `[pa_follow].enabled`);
`muehle/hf/<tuner_follow.slot>/cmd` (`set_inline`, not retained — `tuner` at Mühle, while
the tuner's resource is selected and the band is non-resonant; gated by
`[tuner_follow].enabled`).

**Idle timeout (walk-away safety, §10):** the reconciler infers `activity` itself — a
`freq_hz` change or `tx == "tx"` marks the station `active`; after `[idle].timeout_minutes`
(default 30m) with neither, it marks `inactive` and resolves `target = off` (tier 1). The
switch's `off` position shorts the open ports to ground (lightning protection). There is no
dedicated override command, but **an operator hold is presence**: a non-empty, non-`auto`
`{"request": …}` on `/cmd` resets the idle clock and marks the station `active`, so a hold
works as a manual re-arm even while the radio link is down or silent (previously tier 1
overrode every hold in exactly that state). The walk-away re-ground still fires
`[idle].timeout_minutes` after the last hold or radio activity. A release (`auto`) resets
nothing.
