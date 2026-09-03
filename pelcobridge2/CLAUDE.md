# pelcobridge2 — engineering notes

UHF rotator TUI + rotctld server for the PTS-303Z/3050DZ Pelco head. It evolved
from the `pelcotest/ptest` bench tool and the old `pelcobridge` daemon; both
were deleted and are not references (ptest's serial/assembler invariants live
on in `internal/pelco` and §"Testing patterns"). Shared conventions live in
`../CLAUDE.md` and `../docs/`; this file is what is *not* derivable from the
code.

The vendor's reference material lives in `docs/` here: the PTS-303Z/3050DZ
user manual (.doc), the Chinese serial-command spreadsheet (.xls), and a survey
of open-source implementations for this head family. Treat vendor claims as
untrusted documentation — the bench facts in the code's comments win over the manual where they disagree.

## Non-negotiable invariants

1. **One engine goroutine owns the wire and all state.** No mutexes; everyone
   else speaks through `Request{From, Intent, Reply}`. `Submit` never blocks;
   `Call` carries a ctx timeout (2 s is the convention).
2. **No timer polling.** Engine timers are one-shot gate releases only:
   frame gap after TX, reply-wait around the single outstanding query,
   `settle_ms` quiet window around absolute sets. A timer never transmits
   unprompted motion. TWO recorded deviations, both timer-chained ladder TXes
   (the ladder was started by an explicit operator request; the tick only
   advances it): `tickSettle` TXes the ladder's next set or its verify query,
   and `tickBurst` ends a crawl jog burst with a stop frame
   (`internal/control/engine.go` `onTick`). The remaining deviation is the TUI
   hold-to-move stop tick (`internal/ui/ui.go` package doc) — Bubble Tea has no
   key-release events.
3. **At most one outstanding query.** A second is queued until the reply wait
   expires. Readback is garbage while a motor runs — never query while moving.
   `gotReadback` dispatches into the ladder ONLY when the frame answers the
   outstanding query (`matched`): a wrong-axis frame, a duplicate reply, or
   the late answer to an already-expired query must never be mistaken for a
   check readback — the bogus error would jog the head off garbage data.
4. **User queries answer in TRUE degrees.** `gotReadback` applies the arm
   offset to every user-facing pan reply (rotctld `get_pos`, TUI `a`) so
   read-then-set hamlib clients get a consistent coordinate frame; tilt
   replies mirror the head's INVERTED native scale (`el = 90 − tilt`, bench
   2026-08-30). The ladder's own verify queries keep raw physical degrees
   (their targets are physical — pan offset, tilt native word).
   `GotoPhysZero` bypasses the offset in both directions (tilt step targets
   native 0, the mechanical home = el 90).
5. **A failed write unwinds, never wedges.** `tx` on write error fails the
   in-flight query (ErrNoFix), kills the ladder (setStat "failed"), clears
   jog, and re-opens the gate — because no timer is armed when nothing went
   out on the wire. Any code path that touches `e.ladder` after a `tx` must
   re-check for nil first (`txSetStep`, `tickSettle` case 2, `tickBurst`).
   Every tx-failure path also calls `drain()` — the re-opened gate with no
   armed timer would otherwise strand the queue until unrelated traffic.
6. **Read errors self-heal.** The transport reader dies on any read error;
   `onReadErr` auto-reopens (2 s cooldown) and starts a fresh reader
   generation. Late errors from a stale generation are ignored by tag.
   `ReopenIntent` (ctrl+r) does the same manually.
7. **Arming is TUI-only, twice enforced** (`ErrSource` gate in the engine AND
   no construction site for `ArmIntent`/`SelfTestIntent`/`SelfCheckIntent`
   outside the TUI). Never persisted; disarmed at every start. **Arming gates
   the rotctld path only**: TUI jog, goto-0, and the TUI goto prompt
   (`g` → `GotoAzElIntent`) are allowed disarmed
   (`req.From != SrcTUI && !e.armed` in `internal/control/engine.go`) — that is
   how the head is positioned before arming. MQTT has no motion intents at
   all: `/cmd` accepts only `{"action":"stop"}` (`internal/mqtt/slot.go`).
8. **Set ladder phases**: 1 = next step's set TX due after settle (multi-step
   ladders only), 2 = verify TX due after settle (`txSetStep` MUST advance
   phase to 2), 3 = verify query in flight, 4 = crawl jog burst running
   (`tickBurst` armed). NO automatic re-sends (bench decision 2026-08-30):
   off-target verify or no readback fails the ladder immediately
   (`ladderFail`) — the operator re-issues. A crawl burst is NOT a re-send:
   it is the commanded convergence mode, chosen by flag/TOML (`crawl`), and
   its repetition is the operator's chosen behavior, not engine initiative.
   Stop/jog/new set cancels the ladder — human wins. A manual jog queued
   mid-crawl wins at the NEXT readback: `crawlVerify` first drains (the one
   gate-open moment per cycle — a parked jog runs outright via `execJog`, and
   parked queries get their ErrBusy/ErrMoving answer instead of piling up as
   caller-abandoned zombies), then scans the WHOLE pending queue for a jog
   (the readback can land inside the frame gap, where the drain is a no-op).
   A goto under an active manual jog is refused `ErrMoving`
   (`execSet` — a jog never self-stops, so its verify would be garbage; the
   old silent `jogOp = 0` was the bug). The all-stop ALSO
   cancels queued (not yet executed) motion: `stopAll` replies every pending
   request `ErrCancelled`, so nothing drains and starts moving after the
   all-stop frame. The queue itself is capped (`maxPending` 16 → ErrBusy).
   SetStatus is cleared (`""`) by `execJog`, `stopAll`, and a crawl cancel —
   a jog or e-stop moves the head, so a previous "converged" must not
   outlive the motion that voided it. Out-of-range elevation is refused at
   the intent boundary (`elInRange`, 0..90, both modes, both `handle` and
   `execQueued`): the set ladder clamps at the wire (`SetTiltFrame`), but a
   crawl never builds a set frame and would otherwise chase an unreachable
   native target with bursts into the mechanical limit until the cap.
   **Crawl mode** (`-crawl` / `[control] crawl`, process-wide, every goto
   source funnels through `execSet`): read state first (one query per step,
   phase 3) → off-target → one timed low-speed jog burst at `crawl_speed`
   (phase 4, `tickBurst`) → stop → settle → check query → repeat, until
   within `crawl_tolerance_deg` or `CrawlMaxBursts` (40/axis) fails the
   ladder. Direction math is NATIVE-frame (`jogToward` deliberately does NOT
   swap the tilt pair — both arguments are native; wraparound-shortest for
   pan). simhead ignores the speed byte — crawl tests pick rates where
   rate × burst is a sane travel fraction and burst travel must not
   overshoot the tolerance (no burst-length adaptation yet — that is the
   later "learn deg/s per speed byte" version; `crawlVerify` already logs
   the raw per-burst data it will need: `crawl %c: %+.2f° in %.1fs @ 0x%02X`).
9. **Pelco opcodes are untyped consts** — cast at use sites (`byte(x)`).
   Readbacks (0x59/0x5B) are textbook degrees×100; absolute sets (0x4B/0x4D)
   need the quiet line. The TILT scale is INVERTED relative to elevation
   (bench 2026-08-30): native tilt 0 = antenna at zenith (mechanical home),
   90 = horizon, `el = 90 − tilt` (`pelco.TiltToEl`/`ElToTilt`). Set words,
   readback words, AND the jog opcodes all speak the native scale — `OpUp`
   raises native tilt (LOWERS the antenna), so the engine's `dirOpcode` swaps
   the tilt pair (jog-up sends OpDown). `simhead` models the native frame
   unchanged; only the engine converts. The engine sends self-check disable (preset set 105)
   once per connect, before anything else uses the line, and RE-SENDS it after
   every successful reopen (the head may have power-cycled meanwhile). Preset
   call 105 re-enables the head's periodic self-check — manual maintenance
   toggle, TUI-only (`c` off / `C` on behind a y/n confirm, enable additionally
   disarmed-only; the TUI never short-circuits on the pane's current value —
   the model is a claim, not proof). Both self-test (preset call 125) and its
   factory restore invalidate every readback (`havePan`/`haveEl` cleared) and
   set the modelled self-check back to "on". Self-test mechanics (bench
   knowledge, doc only — no code models them): the pan axis re-homes by
   turning RIGHT past its 0° sensor TWICE (361–720° travel; pre-wind one turn
   LEFT first to shorten the sweep — the TUI confirm prompt says so); the
   tilt axis just drives 90°→0° on its limit switches. The snapshot's `SelfCheck` is a
   canonical tri-state "on"/"off"/"unknown" — and it is LIVENESS-GATED: RS-485
   has no link ACK, so a preset frame leaving the adapter proves nothing; a
   sent-but-unproven claim parks in `selfCheckPend` and lands only once the
   head proves it is alive (a checksum-valid RX frame after the preset went
   out). Any link death (read error, manual reopen) drops the model to
   "unknown" before the reopen is even attempted; `publish()` is the single
   point that maps an empty internal value to "unknown", so consumers render
   the field verbatim (`mqtt` does NO remap).
10. **paho foot-guns** (`shared/mqtt`): connect via `sharedmqtt.Connect`
   (ctx-aware); message handlers only `Enqueue` — one `RunJobs` worker does
   all publishing (hadiscovery deadlocked live on blocking Publish in a
   handler). Connect is non-fatal/background; `[mqtt] enabled=false` works.
   `OnConnect` republishes the last retained `/state` payload (a quiescent
   head emits nothing, so consumers after a broker restart would otherwise
   see no state); the state dedup key excludes `ts` AND `readback_age_s`.
11. **Secrets**: password only via env `PELCOBRIDGE2_MQTT_PASSWORD`; config +
   state files 0600, seed-once; no password flag. An explicitly named missing
   config (flag/env) is an ERROR — only the no-path case falls back to
   defaults (`internal/config/config.go` `Load`).

## rotctld wire contract (verified against hamlib source)

`p`→two `%.2f` lines, NO RPRT on success; `P`→`RPRT 0`, disarmed `RPRT -9`,
parse fail `RPRT -1` (NaN/`inf` included — `ParseFloat` accepts them, and
`DegToWord` would park them at 0°), el outside 0..90 `RPRT -1`. `P` submits
ONE `GotoAzElIntent` for both axes — two sequential Calls would queue the el
behind the pan ladder, whose settle (set mode) or bursts (crawl) outlast the
2 s round-trip, so the client read `RPRT -6` while the head still moved when
the queued el drained (observed in both modes). `S`→`RPRT 0` always; `_`→info
line; `\dump_state`→ protocol v1 `tag=value` + `done` (gpredict sends this at
open); `+` extended prefix; unknown → `RPRT -4`.
`internal/rotctld/server_test.go` pins this byte-exactly — do not change
responses without re-verifying against hamlib's `rotctl_parse.c` /
`netrotctl.c`.

## Testing patterns

- `internal/simhead` models the head AS THE ENGINE MEETS IT (silence-required
  sets, garbage-while-moving); each quirk toggleable. `PanDeg`/`TiltDeg` are
  raw native words; `SetAzEl(az, el)`/`ElDeg()` are test conveniences that
  teleport/read in physical az and TRUE el (mirrored via
  `pelco.ElToTilt`/`TiltToEl`). The engine-test harness `gotoAzEl(trueAz, el)`
  additionally crosses the arm offset — a user-frame goto.
- Engine tests need `cfg.Settle` scaled above the simulated travel time —
  the first verify readback is garbage while the head is moving.
- A second set in one test cannot wait on `SetStatus == "converged"` — the
  first set's status still stands. Wait on the value (`s.El == 30`).
- Self-test/self-check frames must not break a frame-gap or reply window, so
  the engine answers ErrBusy ("retry", not refusal) — tests retry with
  `harness.callNoBusy` (`engine_test.go`).
- `TestJogMovesAndStops` polls `h.tr.PanDeg()` directly, not the snapshot:
  no readbacks happen while moving, so `PhysAz` is stale mid-jog.
- UI tests: `tea.Batch` returns its inner commands — `runCmd` in
  `internal/ui/ui_test.go` descends into `tea.BatchMsg`.
- Snapshots carry TRUE az (`Norm360(phys − offset)`) and TRUE el
  (`90 − native tilt`); `Phys*` are raw. Ladder targets in the snapshot are
  true values (pan `Norm360(target − offset)`, tilt `90 − native target`).
- Value-receiver trap: Bubble Tea model methods that mutate state must return
  `(model, tea.Cmd)` — `jog()`/`bumpSpeed()` lost their mutations once, which
  silently broke release-to-stop (the tick's sequence never matched the
  model's). `TestJogHold` pins exactly this.
- Serial/tcp transports guard the port handle with a mutex and snapshot it —
  never hold the mutex across the blocking Read (`internal/serialio/serial.go`).

## Bench-dependent defaults (tune on hardware)

`settle_ms` 2000, `set_tolerance_deg` 0.3, reply wait 400 ms, `jog_hold_ms`
250 — all in `[control]` of the TOML. Crawl: `crawl_speed` 0x04,
`crawl_tolerance_deg` 4.0, burst 1 s, per-axis burst cap 40 (the last two are
engine-internal, not TOML). The bench check must confirm 0x04 is "very low"
on the real head and that one 1 s burst lands inside the 4° tolerance.
`crawl_speed` 0 is the fillDefaults unset-sentinel and becomes 0x04 — the
usable range is 1–0x3F. UNBENCH-VERIFIED assumption: `OpRight` increases the
native pan word (simhead models it that way; the 2026-08-30 bench confirmed
only the tilt inversion) — the first live crawl should check the pan
direction before trusting a long crawl. Windows Terminal auto-repeat
behaviour for hold-to-move is unverified; fallback would be toggle-jog
(bench check).