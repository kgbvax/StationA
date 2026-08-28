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
4. **Arming is TUI-only, twice enforced** (`ErrSource` gate in the engine AND
   no construction site for `ArmIntent`/`SelfTestIntent` outside the TUI).
   Never persisted; disarmed at every start. MQTT has no motion intents at
   all: `/cmd` accepts only `{"action":"stop"}` (`internal/mqtt/slot.go`).
5. **Set ladder phases**: 1 = set TX due after settle, 2 = verify TX due after
   settle (`txSetStep` MUST advance phase to 2), 3 = verify query in flight.
   Stop/jog/new set cancels the ladder — human wins.
6. **Pelco opcodes are untyped consts** — cast at use sites (`byte(x)`).
   Readbacks (0x59/0x5B) are textbook degrees×100; absolute sets (0x4B/0x4D)
   need the quiet line.
7. **paho foot-guns** (`shared/mqtt`): connect via `sharedmqtt.Connect`
   (ctx-aware); message handlers only `Enqueue` — one `RunJobs` worker does
   all publishing (hadiscovery deadlocked live on blocking Publish in a
   handler). Connect is non-fatal/background; `[mqtt] enabled=false` works.
8. **Secrets**: password only via env `PELCOBRIDGE2_MQTT_PASSWORD`; config +
   state files 0600, seed-once; no password flag.

## rotctld wire contract (verified against hamlib source)

`p`→two `%.2f` lines, NO RPRT on success; `P`→`RPRT 0`, disarmed `RPRT -9`,
parse fail `RPRT -1`; `S`→`RPRT 0` always; `_`→info line; `\dump_state`→
protocol v1 `tag=value` + `done` (gpredict sends this at open); `+` extended
prefix; unknown → `RPRT -4`. `internal/rotctld/server_test.go` pins this
byte-exactly — do not change responses without re-verifying against hamlib's
`rotctl_parse.c` / `netrotctl.c`.

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

## Bench-dependent defaults (tune on hardware)

`settle_ms` 2000, `set_tolerance_deg` 0.3, reply wait 400 ms, `jog_hold_ms`
250 — all in `[control]` of the TOML. Windows Terminal auto-repeat behaviour
for hold-to-move is unverified; fallback would be toggle-jog (bench check).