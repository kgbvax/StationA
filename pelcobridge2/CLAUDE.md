# pelcobridge2 — engineering notes

UHF rotator TUI + rotctld server for the PTS-303Z/3050DZ Pelco head. Evolved
from `../pelcotest/ptest` (keep its invariants); the old `pelcobridge` daemon
was deleted and is not a reference. Shared conventions live in `../CLAUDE.md`
and `../docs/`; this file is what is *not* derivable from the code.

## Non-negotiable invariants

1. **One engine goroutine owns the wire and all state.** No mutexes; everyone
   else speaks through `Request{From, Intent, Reply}`. `Submit` never blocks;
   `Call` carries a ctx timeout (2 s is the convention).
2. **No timer polling.** Engine timers are one-shot gate releases only:
   frame gap after TX, reply-wait around the single outstanding query,
   `settle_ms` quiet window around absolute sets. A timer never transmits.
   The ONE recorded deviation is the TUI hold-to-move stop tick
   (`internal/ui/ui.go` package doc) — Bubble Tea has no key-release events.
3. **At most one outstanding query.** A second is queued until the reply wait
   expires. Readback is garbage while a motor runs — never query while moving.
4. **User queries answer in TRUE degrees.** `gotReadback` applies the arm
   offset to every user-facing pan reply (rotctld `get_pos`, TUI `a`) so
   read-then-set hamlib clients get a consistent coordinate frame; the
   ladder's own verify queries keep raw physical degrees (their targets are
   physical). `GotoPhysZero` bypasses the offset in both directions.
5. **A failed write unwinds, never wedges.** `tx` on write error fails the
   in-flight query (ErrNoFix), kills the ladder (setStat "failed"), clears
   jog, and re-opens the gate — because no timer is armed when nothing went
   out on the wire. Any code path that touches `e.ladder` after a `tx` must
   re-check for nil first (`txSetStep`).
6. **Read errors self-heal.** The transport reader dies on any read error;
   `onReadErr` auto-reopens (2 s cooldown) and starts a fresh reader
   generation. Late errors from a stale generation are ignored by tag.
   `ReopenIntent` (ctrl+r) does the same manually.
7. **Arming is TUI-only, twice enforced** (`ErrSource` gate in the engine AND
   no construction site for `ArmIntent`/`SelfTestIntent` outside the TUI).
   Never persisted; disarmed at every start. **Arming gates the rotctld path
   only**: TUI jog and goto-0 are allowed disarmed (`req.From != SrcTUI &&
   !e.armed` in `internal/control/engine.go`) — that is how the head is
   positioned before arming. MQTT has no motion intents at all: `/cmd` accepts
   only `{"action":"stop"}` (`internal/mqtt/slot.go`).
8. **Set ladder phases**: 1 = set TX due after settle, 2 = verify TX due after
   settle (`txSetStep` MUST advance phase to 2), 3 = verify query in flight.
   Stop/jog/new set cancels the ladder — human wins. The all-stop ALSO
   cancels queued (not yet executed) motion: `stopAll` replies every pending
   request `ErrCancelled`, so nothing drains and starts moving after the
   all-stop frame. The queue itself is capped (`maxPending` 16 → ErrBusy).
9. **Pelco opcodes are untyped consts** — cast at use sites (`byte(x)`).
   Readbacks (0x59/0x5B) are textbook degrees×100; absolute sets (0x4B/0x4D)
   need the quiet line. The engine sends self-check disable (preset set 105)
   once per connect, before anything else uses the line.
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
`DegToWord` would park them at 0°); `S`→`RPRT 0` always; `_`→info line;
`\dump_state`→ protocol v1 `tag=value` + `done` (gpredict sends this at open);
`+` extended prefix; unknown → `RPRT -4`. `internal/rotctld/server_test.go`
pins this byte-exactly — do not change responses without re-verifying against
hamlib's `rotctl_parse.c` / `netrotctl.c`.

## Testing patterns

- `internal/simhead` models the head AS THE ENGINE MEETS IT (silence-required
  sets, garbage-while-moving, D/P adaptive); each quirk toggleable.
- Engine tests need `cfg.Settle` scaled above the simulated travel time —
  the first verify readback is garbage while the head is moving.
- `TestJogMovesAndStops` polls `h.tr.PanDeg()` directly, not the snapshot:
  no readbacks happen while moving, so `PhysAz` is stale mid-jog.
- UI tests: `tea.Batch` returns its inner commands — `runCmd` in
  `internal/ui/ui_test.go` descends into `tea.BatchMsg`.
- Snapshots carry TRUE az (`Norm360(phys − offset)`); `Phys*` are raw.
  Ladder targets in the snapshot are true values (`Norm360(target − offset)`).
- Value-receiver trap: Bubble Tea model methods that mutate state must return
  `(model, tea.Cmd)` — `jog()`/`bumpSpeed()` lost their mutations once, which
  silently broke release-to-stop (the tick's sequence never matched the
  model's). `TestJogHold` pins exactly this.
- Serial/tcp transports guard the port handle with a mutex and snapshot it —
  never hold the mutex across the blocking Read (`internal/serialio/serial.go`).

## Bench-dependent defaults (tune on hardware)

`settle_ms` 2000, `set_tolerance_deg` 0.3, reply wait 400 ms, `jog_hold_ms`
250 — all in `[control]` of the TOML. Windows Terminal auto-repeat behaviour
for hold-to-move is unverified; fallback would be toggle-jog (bench check).