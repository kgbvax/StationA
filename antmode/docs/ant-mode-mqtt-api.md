# antmode — `muehle/hf/ant-mode` MQTT contract

`antmode` is the logic slot that owns the station **antenna mode**: `normal`, `storm` or
`maintenance`. In storm and maintenance the HF Ultrabeam (`hf/ant-ctrl`) and the HF
rotator (`hf/rotator`, Yaesu G-450DC — no mechanical brake) are parked and locked:

| | storm | maintenance |
|---|---|---|
| Antenna switch | grounded (`off`) | grounded (`off`) |
| Ultrabeam elements | retracted, kept in | retracted, kept in |
| Rotator | parked once at wind ± 90° (fallback preset) | parked at the maintenance preset |
| Motion | refused on every path | refused, except a console nudge (`override:"maintenance"`): hf_console's ±5° keys, a `set_az` of readback ± 5° — tap-to-aim is off |
| Antenna power, if off at entry | borrowed: on → park → back off (also if powerseq switches it off mid-park) | borrowed: on until maintenance ends |
| powerseq idle logic (`power_hold`) | held until parked or failed | held for the whole maintenance |
| Entry | operator, or automatic on a DWD wind warning when unmanned (`auto_enter`, default off: suggestion only) | operator only |
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

- All are **one-shot transition requests**, not steady state: antmode applies the
  request, then clears the retained topic (§8 rule 1). Home Assistant's select publishes
  retained without `ts`; a steady-state reading would replay a stale `normal` on every
  antmode restart and cancel an auto-entered storm. Every non-empty payload is cleared —
  applied, dropped or malformed — with an empty retained publish; the empty payload that
  comes back is ignored. Subscribed at QoS 0 (clean session), so the broker never queues
  one for a stopped antmode; a retained one published meanwhile is applied at start.
- `ts` (RFC 3339) optional; older than 30 s or more than 5 s in the future (clock skew
  allowance) → dropped with a Warn.
- `value` is a JSON string (mode names are case-insensitive).
- A cmd that arrives during the 3 s startup hold (below) is queued and applied when the
  hold ends, on top of any adopted state.
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
  "seq": 17,
  "lock": {"seq": 17, "ground": true, "retract": true, "park_az": 175, "nudge": false},
  "phase": "parked",
  "fault": "",
  "power_hold": false,
  "power_borrow": {"restore_to": "off", "powerseq_phase": "standby", "since": "…"},
  "presence": "unmanned",
  "wind": {"dir_deg": 265, "gust_kmh": 72, "ts": "…", "stale": false},
  "warnings": [{"id": "dwd.2.49.0.0.276.0.DWD.PVW.…", "event": "STURMBÖEN", "ii": "52",
                "onset": "…", "expires": "…"}],
  "storm_suggested": false,
  "reason": "DWD STURMBÖEN active, station unmanned",
  "auto_exit_at": "",
  "drift_deg": 1,
  "warnings_ok": true
}
```

| Field | Meaning |
|---|---|
| `ant_mode` | `normal` \| `storm` \| `maintenance`. Not called `mode` (that is the radio-mode vocabulary, and antennaselect's auto/manual). |
| `source` | `manual` \| `auto` — who entered the current mode. |
| `seq` | The last lock `seq` used (present in every mode, also normal). antmode reads it back from the retained `/state` at start so a stale state file never reuses a `seq`. Consumers use `lock.seq`. |
| `lock` | Present only when `ant_mode` ≠ normal. Built up in safe order: `ground` → `retract` (once the switch reports `off`) → `park_az` (once retracted or after a retract timeout; the bridge waits for a stable readback itself). `seq` changes on every mode entry (including storm ↔ maintenance) and on `repark`; it is opaque — compare for equality only (a fresh state file seeds it from the clock). `nudge` is true in maintenance only. |
| `phase` | `idle` (normal) \| `grounding` \| `retracting` \| `awaiting_power` \| `awaiting_devices` \| `rotating` \| `parked` \| `failed`. See *Sequencing* below. |
| `fault` | Why `failed` (omitempty). `failed` keeps the lock; a late arrival at `park_az` (or, in maintenance, at a nudge target) still moves to `parked`. |
| `power_hold` | powerseq must hold its idle logic: watch changes re-baseline, never activity, and no idle action (standby, wake, idle shutdown). True while a borrow is open, for the whole maintenance (borrowed or not: no power cut and no wake power-up with someone at the mast), and in storm until `parked` or `failed` (a standby must not cut the gear mid-park). |
| `power_borrow` | Present while antmode borrows `power/antennas`: `restore_to`, `powerseq_phase` (at the borrow start), `since`, `taken_over` (omitempty). A display reads this, not `power_hold`, for "antenna power borrowed". antmode adopts it from the retained `/state` when its state file is lost. |
| `presence` | `manned` \| `unmanned` — see *Mode policy* (powerseq phase, with a 60 s dwell and the restart rule). |
| `wind` | DWD MOSMIX day value for the configured station (`stationOverviewExtended` `days[]` entry for today's Europe/Berlin date: `windDirection`/10 °, `windGust`/10 km/h). `ts` = when it was fetched. `stale` when no successful fetch is younger than `wind.stale_minutes` (default 180) or there never was one; the storm park then uses `storm.fallback_az`. |
| `warnings` | Currently **active** (onset ≤ now < expires; no lead time) DWD warnings for the configured ARS whose DWD II event code is in `nina.ii_codes` (default: level 1 "Windböen" and up, plus thunderstorms with gusts — the list and its DWD source are in `config.example.toml`). Filtered by the clock every second, so a warning becomes active at its onset without a new poll. |
| `warnings_ok` | The last NINA poll succeeded. `false` = the warnings are **unknown** (outage): `warnings` then shows the last successful poll, still filtered by the clock. |
| `storm_suggested` | Mode normal, a qualifying warning active that was neither acknowledged (manual `normal`) nor dismissed, and the station manned **or** `auto_enter` off → the console/HA shows a banner. `reason` then explains it. |
| `auto_exit_at` | When an auto-entered storm will return to normal if the polls stay quiet: last quiet poll + remaining hold-off, so it changes only at a poll; the exit happens at the first successful poll at or after it (omitempty; absent while a warning is active, during an outage, and before the first quiet poll). |
| `drift_deg` | Rotator readback minus `park_az` while parked (no re-aim; Warn above a threshold). In maintenance it includes the operator's nudges. |

## Reading rules for consumers (normative)

1. Parse with `shared/antmode.Parse`.
2. **Empty / no retained state → normal.** The station is usable before antmode exists.
3. **Valid state with `ant_mode` ≠ normal → enforce it**, even while `ant-mode/status` is
   offline. A dead owner never unlocks.
4. **Malformed → keep the last known state** and log an Error.
5. `ts` is not checked — this is steady state.
6. Bridges with a persistent MQTT session (ultrabridge) subscribe at **QoS 0**.
7. **Connect hold.** A consumer that moves the antenna (wrc-rotator-bridge,
   ultrabridge, beamsteer) refuses motion until the retained state has arrived, for at
   most 3 s, so a command replayed on connect cannot beat a storm lock: wrc-rotator-bridge
   from the first broker connect, ultrabridge from process start, beamsteer after every
   connect (on a lapse without a replay the last known lock stands). powerseq takes no
   idle action for up to 3 s after every reconnect until the state is re-delivered.

### What each consumer does

| Consumer | While locked |
|---|---|
| wrc-rotator-bridge | Refuses `set_az`/`fwd`/`rev` on MQTT, GS-232 and PstRotator; `stop` always allowed. Executes `park_az` once per `seq` (device online, readback stable ~3 s — wind creep restarts the wait, so the park waits for a lull; re-sent only if it never arrived). An operator stop (any path) during the park cancels it until a new `seq` — antmode then reaches `failed` at `rotate_timeout_s`; `repark` starts a new one. Accepts `set_az` with `"override":"maintenance"` when `lock.nudge` and the target is within `nudge_max_deg` (default 10°) of the readback, linear in rotor coordinates (refused with the WRC offline); and inside the 0–450° travel; an accepted nudge also ends the park for that `seq` — also a nudge before `park_az` is published (grounding/retracting), which antmode cannot tell from a stop: it reaches `failed` at `rotate_timeout_s` (safe; `repark`). A nudge after the bridge sent its park is reported `parked` (see `rotating` below). A finished park (arrived, cancelled, nudged) is recorded in `<state_dir>/park.json` and not repeated after a bridge restart; a park still underway at the restart is re-sent. `/state.lock` = mode name. |
| ultrabridge | Refuses `frequency`/`band`/`direction` (MQTT, HTTP 423); `retract` always allowed. Ensures retracted: on a new `seq` with `lock.retract` (or `retract` appearing on the current `seq`), on device back online (also while a retract is still running: re-issued, `retracted` reads false until it is accepted), at bridge start. `/state.lock`. `/state.retract_seq` = the `seq` whose retract finished (accepted, then a poll ≥ 2 s later read retracted and not moving); absent from a new `seq`, a `retract` appearing, or a back-online re-arm until that retract finishes — unlike `retracted`, a latch left from an earlier lock or the ACK-time snapshot never confirms the current `seq` (antmode's `retracting` gate leaves only on `retract_seq` = `lock.seq`). The `retracted` latch is persisted (`<state_dir>/retract.json`) and survives a bridge restart, locked or not, with the RCU-06 powered or not; a same-band frequency is never swallowed by the 25 kHz deadband between a retract and the next accepted frequency. |
| antennaselect | Lock tier above the manual stand-down: target `off`, source `lock`; band-follow and tuner follow stop (PA band-follow continues). On unlock, follow state is reset so the same frequency is re-sent (re-extends the elements); until `hf/ant-ctrl/state` shows the elements out, an RCU-06 `device_online` false→true edge re-sends it again (ultrabridge drops a frequency it cannot deliver), and so does — once per unlock — `hf/ant-ctrl/state` reporting `device_online`, `retracted` and no `lock` for 15 s (the frequency was refused under a stale lock: mirror replay order). The lock is not activity. |
| beamsteer | `Goto`/`aim` ignored with a reason, pending direction flips dropped, logger STOP not forwarded (it would cancel the park). `/state.lock` = mode name (omitted when unlocked). |
| powerseq | While `power_hold`: watch changes re-baseline, no idle transition (standby, wake, idle shutdown); operator `start`/`stop` still work. When the hold ends: a fresh settle window, and the idle-shutdown clock restarts from the hold end. |
| m5stamp-hf-ctrl (PA arm) | No change: the grounded switch and the retracted Ultrabeam already open the arm. |

## Behaviour (normative for antmode)

### Mode policy

- **Presence:** `manned` iff `hf/power-seq/status` is online and its `/state.phase` is
  `starting`, `running` or `waking`; otherwise `unmanned`. Two refinements:
  - **Dwell:** a manned → unmanned change counts only after it lasted 60 s, so a powerseq
    restart or `/status` blip never auto-enters storm on an operator. A real outage still
    reads unmanned after the dwell. A station first seen unmanned (antmode start) is
    unmanned at once.
  - **Lost phase:** an `idle` that follows a manned phase without `stopping` in between
    (a start that aborted, or a powerseq restart that lost the phase) stays `manned`
    while `hf/radio` is live, until the radio has not transmitted for 30 min (powerseq
    cannot enter standby from `idle`, so this is the only "operator gone" signal left).
    A listen-only operator therefore reads unmanned 30 min after the restart. Since
    1cabf4a (2026-10-22) powerseq keeps its phase across a restart (`state_file`): a
    plain restart restores `running`/`standby`; only a sequence killed mid-way
    (`starting`/`waking` → `idle` + `fault`) or a missing/corrupt state file still shows
    `idle`. antmode is unchanged; the rule could be narrowed to an `idle` with a `fault`
    (`../../docs/known-issues.md` "HF antenna mode").
- **Automatic entry:** with `storm.auto_enter = true` (default **false** — observe only
  until plan Phase 6), mode normal, station unmanned, and a qualifying warning active
  that is not acknowledged → storm, `source: auto`. A dismissed suggestion does not block
  it (dismiss ≠ acknowledge). Evaluated on every poll, every input and once a second.
- **Automatic exit:** only an auto-entered storm. Quiet time accumulates between
  consecutive successful polls that find no qualifying warning; a failed poll neither
  counts nor resets it (the interval around an outage is skipped); any qualifying warning
  resets it to zero. At `storm.exit_holdoff_minutes` (default 60) → normal,
  `source: auto`. The quiet time survives a restart (the restart gap is not counted).
- A warning id leaves the acknowledged/dismissed sets once a successful poll no longer
  lists it as active. DWD issues a new id on escalation (and on every update), so an
  updated warning counts as new.
- **NINA reading:** `GET /dashboard/{ARS}.json` (ARS zero-padded to district level), keep
  ids with prefix `dwd.`, skip `msgType` Cancel; `GET /warnings/{id}.json` (CAP): skip
  Cancel, non-`Actual` and DWD pre-information (`event` "VORABINFORMATION …", which
  reuses II codes 40/55); the II code is `info[].eventCode` `II` of the German block;
  onset falls back to `effective`, then `sent`; a missing `expires` is open-ended. One
  failed request fails the whole poll (unknown, never "no warnings").

### Sequencing (per entry, repark or storm ↔ maintenance)

| Phase | Leaves when | Timeout (config `[park]`) |
|---|---|---|
| `awaiting_power` | `power/antennas` live with `power: on` | `power.devices_timeout_s` (240) from the borrow start → continue at `grounding` (Warn) |
| `awaiting_devices` | `hf/rotator` and `hf/ant-ctrl` both live (two-layer) | same budget → continue at `grounding` (Warn) |
| `grounding` | `hf/ant-switch` live with `selected: "off"` → `lock.retract` | `ground_timeout_s` (60) → retract anyway, but **only once `hf/radio` has not been live and transmitting for `tx_quiet_s` (30)** — longer than a digital-mode RX gap (FT8: ~2 s every 15 s), so the retract never starts between two overs with the Ultrabeam still in circuit |
| `retracting` | `hf/ant-ctrl` live, `retract_seq` = `lock.seq`, `retracted`, not `moving` → `lock.park_az`. A latch from an earlier lock (no or an older `retract_seq`) never ends it, so a repark or a storm ↔ maintenance switch waits for the new seq's re-retract | `retract_timeout_s` (120) → park anyway (Warn; the fallback for an unpowered RCU-06 or an ultrabridge without `retract_seq`) |
| `rotating` | `hf/rotator` live, within `tolerance_deg` (3) of `park_az`, not `moving` → `parked`. Maintenance also, once the bridge's park target has been seen this seq (`target_az` at `park_az`; for `park_az` 0, whose target the bridge omits, a moving rotator with no `target_az`; the evidence is dropped whenever the rotator goes offline): the rotator at rest at a `target_az` that is neither `park_az` nor the target at lock start (a console nudge ended the park) → `parked`, `drift_deg` = the offset | `rotate_timeout_s` (300) → `failed` + `fault` |
| `parked` | — `drift_deg` tracked; Warn once per excursion above `drift_warn_deg` (10); no re-aim | — |

- **Storm heading:** wind direction ± 90° (mod 360) plus the +360 variants up to
  `storm.max_az` (360), whichever is nearest the rotator readback (linear travel; unknown
  readback → the lowest candidate); integer degrees. Stale wind → `storm.fallback_az`.
  Computed once when the elements are in; a wind veer never re-aims (use `repark`).
- **Maintenance:** `park_az` = `maintenance.park_az`.
- A repark or storm ↔ maintenance switch keeps `ground`/`retract` already in force and
  only recomputes `park_az`. Within one `seq`, `park_az` is computed once (a late borrow
  that re-runs the sequence keeps it).
- Unlock (→ normal): `lock` removed, `phase: idle`. Nothing is commanded, apart from
  the restore of a borrow that is still open (below).

### Power borrow (`[power] borrow`, default false)

- At entry (or repark) with `power/antennas` live and `off`, and powerseq online in a
  **stable** phase (`idle`, `standby`, `running` — never during a transition). **Storm
  also later**, once per `seq`, while the park is not done (phase not `parked`, `failed`
  included): an entry that found the power still on or powerseq in a transition (an auto
  entry at `entering_standby`, an operator `stop`) borrows as soon as the power reads `off`
  and powerseq is stable again; the sequence re-runs from `awaiting_power` (ground and
  retract stay, `park_az` is kept). Never later in maintenance: an operator on site may
  switch the power off on purpose.
  1. `restore_to: off` and the powerseq phase are persisted (if that write fails, no
     borrow);
  2. `/state` goes out with `power_hold: true`;
  3. retained `{"action":"set_power","value":"on"}` (QoS 1) to `power/antennas/cmd`,
     retried every second until it is sent.
- **Restore** — retained `set_power off` — when due: storm once `parked` or `failed`;
  maintenance when it ends; any borrow when the mode returns to normal mid-way (abort).
  A storm park that `failed` while the rotator is still `moving` (the bridge waited for
  a lull) keeps the power until it stops, for at most one more `rotate_timeout_s` — the
  brakeless boom is never cut mid-slew.
  - **Takeover:** if powerseq is seen in any phase other than the borrow-start phase,
    `idle` or `standby` during the borrow (`starting`, `waking`, `running`, `stopping`,
    `entering_standby`: an operator-driven sequence), the power is powerseq's for good and
    is left as it is (Warn). `idle`/`standby` alone are not a takeover: their intent is
    antenna power off, and powerseq can come back from a restart in `idle` (a killed
    sequence, or no usable state file).
  - While powerseq is offline (or has no phase) the due restore **waits** (Warn once);
    `power_hold` stays true.
  - A new lock (re-entry, repark, mode switch) takes over an open borrow, also one whose
    restore is still pending (it could not be published): the restore is cancelled and
    `on` re-sent if the power went off meanwhile.
  - The restore is sent even when `power/antennas` already reads `off` (an `on` still in
    flight, a lost Shelly RPC, a Shelly that booted `off` after a power cut):
    shelly-power-bridge replays its retained `/cmd` as the intent on every reconnect, so
    antmode's retained `on` must be overwritten.
  - Once the restore is sent (or skipped on a takeover) the borrow ends; `power_hold`
    then follows the mode rule (false in normal and in a parked or failed storm, true in
    maintenance).
- A borrow (including a due restore) survives a restart and is completed after it — also
  a lost state file (adopted from `/state.power_borrow`).
- A storm park that failed despite its borrow is not borrowed for again in that `seq`
  (no on/off loop); `repark` starts a new one.

### Persistence, startup, restart

- State file (default `/var/lib/antmode/state.json`, systemd `StateDirectory`, 0600,
  atomic temp + rename): mode, source, since, seq, lock, phase, fault, acknowledged and
  dismissed ids, auto-exit quiet time, borrow (with `taken_over`), the `seq` last
  borrowed for, the rotator target at lock start (nudge detection).
- **Startup hold:** for 3 s after the first broker connect antmode only collects the
  retained replays; it publishes no `/state`, decides nothing and queues `/cmd`.
- antmode always subscribes its own retained `/state` during the hold (and ignores it
  after): its top-level `seq` raises the policy `seq` (never reused), and a **locked**
  snapshot (mode, source, since, seq, lock, phase, `power_borrow`) is adopted when there
  is **no (or an unreadable) state file**, or when its `lock.seq` is **newer than the
  file's** `seq` — the file write failed since (a full or read-only disk), so a lost or
  stale file never unlocks the station. A file at least as new wins. A normal or
  malformed snapshot is never adopted. A failed state-file write is an Error streak in
  the journal.
- `/state` is republished on every reconnect and on every change (deduplicated without
  `ts`); `/status` is the LWT.

## `/meta` expose

- `ant_mode`: writable enum `normal|storm|maintenance` → command
  `{"action":"set_ant_mode","value":<v>}` (`value_key: value`, `value_type: string`;
  Home Assistant select).
- Read-only: `storm_suggested` (boolean → binary_sensor), `phase` (enum, the eight
  phases), `source` (enum `manual|auto`), `presence` (enum `manned|unmanned`).
- Actions: `repark` (`{"action":"repark"}`), `dismiss_suggestion`.
- `capabilities`: `modes`, `reads` and `locks` (site-relative slots), `actions`,
  `auto_enter`, `power_borrow`, `ii_codes`, `max_az`, `fallback_az`, `maintenance_az`,
  `exit_holdoff_minutes`. `role: ant-mode`, `link: none`.
