# antmode — `muehle/hf/ant-mode` MQTT contract

`antmode` is the logic slot that owns the station **antenna mode**: `normal`, `storm` or
`maintenance`. In storm and maintenance the HF Ultrabeam (`hf/ant-ctrl`) and the HF
rotator (`hf/rotator`, Yaesu G-450DC — no mechanical brake) are parked and locked:

| | storm | maintenance |
|---|---|---|
| Antenna switch | grounded (`off`) | grounded (`off`) |
| Ultrabeam elements | retracted, kept in | retracted, kept in |
| Rotator | parked once at wind ± 90° (fallback preset) | parked at the maintenance preset |
| Motion | refused on every path | refused, except a console nudge (`override:"maintenance"`) |
| Antenna power, if off at entry | borrowed: on → park → back off | borrowed: on until maintenance ends |
| Entry | operator, or automatic on a DWD wind warning when unmanned | operator only |
| Exit | operator; an auto-entered storm also exits 60 min after the last warning | operator only |

Why storm parks the boom broadside to the wind: on the 3 el 6–20 the elements are about
0.67 m² against 0.29 m² of boom. Elements end-on cuts the total force by about 30 %
(drag-corrected) and takes the bending load off the fibreglass element roots.

The **bridges enforce** the lock themselves (wrc-rotator-bridge on MQTT, GS-232 :7373
and PstRotator :12040; ultrabridge on MQTT and HTTP) and **execute the park themselves**
from this slot's `/state`. antmode never commands the rotator or the Ultrabeam directly;
it only commands `hf/antenna-select` grounding indirectly (antennaselect reads the lock)
and `power/antennas` (power borrow).

The consumer-side parser is `codeberg.org/kgbvax/stationa/shared/antmode` — every
consumer reads the lock through it.

---

## Topics

```
muehle/hf/ant-mode/meta     retained  birth certificate
muehle/hf/ant-mode/state    retained  snapshot (the lock contract below)
muehle/hf/ant-mode/status   retained  online | offline (LWT)
muehle/hf/ant-mode/cmd      retained? one-shot — applied, then cleared
```

## `/cmd` (one-shot)

```json
{"action":"set_ant_mode","value":"storm","ts":"2026-10-22T18:00:00Z"}
{"action":"repark","ts":"…"}
{"action":"dismiss_suggestion","ts":"…"}
```

- Both are **one-shot transition requests**, not steady state: antmode applies the
  request, then clears the retained topic (§8 rule 1). Home Assistant's select publishes
  retained without `ts`; a steady-state reading would replay a stale `normal` on every
  antmode restart and cancel an auto-entered storm.
- `ts` (RFC 3339) optional; older than 30 s or in the future → dropped.
- `set_ant_mode` with the current mode is a no-op (does **not** re-park; use `repark`).
- `repark` re-runs the park for the current locked mode (new `seq`; storm recomputes the
  heading from the current wind). Ignored in normal.
- `dismiss_suggestion` clears `storm_suggested` for the warnings currently active.
- A manual `set_ant_mode` always wins over automation. A manual `normal` during an active
  warning **acknowledges** those warning ids: they never auto-enter storm again. A new or
  escalated warning does.

## `/state`

```json
{
  "ts": "2026-10-22T18:00:05Z",
  "ant_mode": "storm",
  "source": "auto",
  "since": "2026-10-22T18:00:00Z",
  "lock": {"seq": 17, "ground": true, "retract": true, "park_az": 175, "nudge": false},
  "phase": "parked",
  "fault": "",
  "power_hold": false,
  "presence": "unmanned",
  "wind": {"dir_deg": 265, "gust_kmh": 72, "ts": "…", "stale": false},
  "warnings": [{"id": "dwd.2.49.0.0.276.0.DWD.PVW.…", "event": "STURMBÖEN", "ii": "52",
                "onset": "…", "expires": "…"}],
  "storm_suggested": false,
  "reason": "DWD STURMBÖEN active, station unmanned",
  "auto_exit_at": "",
  "drift_deg": 1
}
```

| Field | Meaning |
|---|---|
| `ant_mode` | `normal` \| `storm` \| `maintenance`. Not called `mode` (that is the radio-mode vocabulary, and antennaselect's auto/manual). |
| `source` | `manual` \| `auto` — who entered the current mode. |
| `lock` | Present only when `ant_mode` ≠ normal. Built up in safe order: `ground` → `retract` (once the switch reports `off`) → `park_az` (once retracted or after a retract timeout, and the rotator readback is stable). `seq` changes on every mode entry and on `repark`. `nudge` is true in maintenance only. |
| `phase` | `idle` (normal) \| `grounding` \| `retracting` \| `awaiting_power` \| `awaiting_devices` \| `rotating` \| `parked` \| `failed`. |
| `fault` | Why `failed` (omitempty). |
| `power_hold` | antmode is borrowing `power/antennas`; powerseq treats watch changes as re-baseline, never as activity, and takes no idle action. |
| `presence` | `manned` (powerseq `starting`/`running`) \| `unmanned` (any other phase, powerseq offline or unknown). |
| `wind` | DWD MOSMIX day value for the configured station (`stationOverviewExtended` `days[].windDirection`/`windGust`, tenths). `stale` when older than the configured limit or missing. |
| `warnings` | Currently **active** (onset passed, not expired) DWD warnings for the configured ARS whose event code is in the configured list (default: level 1 "Windböen" and up, plus thunderstorms with gusts). |
| `storm_suggested` | A qualifying warning is active, the station is manned, and the mode is normal → the console/HA shows a banner. |
| `auto_exit_at` | When an auto-entered storm will return to normal if no new warning arrives (omitempty). |
| `drift_deg` | Rotator readback minus `park_az` while parked (no re-aim; Warn above a threshold). |

## Reading rules for consumers (normative)

1. Parse with `shared/antmode.Parse`.
2. **Empty / no retained state → normal.** The station is usable before antmode exists.
3. **Valid state with `ant_mode` ≠ normal → enforce it**, even while `ant-mode/status` is
   offline. A dead owner never unlocks.
4. **Malformed → keep the last known state** and log an Error.
5. `ts` is not checked — this is steady state.
6. Bridges with a persistent MQTT session (ultrabridge) subscribe at **QoS 0**.

### What each consumer does

| Consumer | While locked |
|---|---|
| wrc-rotator-bridge | Refuses `set_az`/`fwd`/`rev` on MQTT, GS-232 and PstRotator; `stop` always allowed. Executes `park_az` once per `seq` (device online, readback stable ~3 s; re-sent only if it never arrived). Accepts `set_az` with `"override":"maintenance"` when `lock.nudge`. `/state.lock` = mode name. |
| ultrabridge | Refuses `frequency`/`band`/`direction` (MQTT, HTTP 423); `retract` always allowed. Ensures retracted: on a new `seq` with `lock.retract`, on device back online, at bridge start. `/state.lock`. |
| antennaselect | Lock tier above the manual stand-down: target `off`, source `lock`; band-follow stops. On unlock, follow state is reset so the same frequency is re-sent (re-extends the elements). |
| beamsteer | `Goto` ignored with a reason, pending direction flips dropped, logger STOP not forwarded. `/state.locked`. |
| powerseq | While `power_hold`: watch changes re-baseline, no idle transition. Settle window after the hold ends. |
| m5stamp-hf-ctrl (PA arm) | No change: the grounded switch and the retracted Ultrabeam already open the arm. |

## `/meta` expose

- `ant_mode`: writable enum `normal|storm|maintenance` → command
  `{"action":"set_ant_mode","value":<v>}` (Home Assistant select).
- Read-only: `storm_suggested` (binary_sensor), `phase`, `source`, `presence`.
- Action: `repark`.
