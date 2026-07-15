# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
go build ./...                       # build all packages
go build -o pelcots .                # build the binary
go vet ./...                         # vet
go test ./...                        # all tests
go test -race ./...                  # race detector (engine + servers are concurrent — run this for changes there)
go test ./internal/engine/ -run TestPlanMoveUnwrap   # a single test
gofmt -w internal/ main.go           # format
```

Run modes: `./pelcots` (interactive TUI), `./pelcots -d` (headless network controller). Flags override `pelcots.yaml`; see `README.md` for the full flag/protocol/config reference.

## Architecture

pelcots drives a Pelco-D PTZ/rotator. The central design choice is a **headless `engine` core** that owns all control logic, with the TUI and daemon as thin layers on top. When changing behavior, the engine is almost always where it belongs — not the UI.

**`internal/engine` — the actor core (`engine.go`).** A single goroutine (`run`) owns *all* mutable state and is the only writer to the port. Every public method (`Submit`, `Jog`, `Goto`, `Reconnect`, `SetWrap`, `SetServer`, …) enqueues a closure onto `reqs` via `do()`; the goroutine executes them serially alongside a poll ticker and the inbound-frame channel. External readers never touch engine fields directly — they call `Snapshot()`, which returns an immutable `State` copy guarded by a mutex and refreshed by `publish()` after each event. This is why there are almost no locks: confine new state to the goroutine and expose it through `State`.
- **Reconnect** swaps the `e.frames` channel reference (and nils it when disconnected). The `select` re-evaluates `e.frames` each iteration, so stale events from a closed port are simply never read — there is deliberately no generation counter.
- The engine also owns the inbound control servers (`setServer`) and publishes position to `control.Pos` on each readback.

**Motion policy (important, easy to break).** Two distinct motion paths:
- *Local TUI keyboard* is **hold-to-move**. Terminals have no key-up event, so a held key is detected via the OS key-repeat stream: each press arms a `releaseMsg` timer (`armRelease` in `internal/ui/model.go`); when repeats stop, the timer fires and calls `eng.StopMotion()`. The TUI must never leave the unit moving without an active key press.
- *Engine/network/daemon* motion (inbound commands, cable-wrap unwrap) runs to completion — this is the explicit external path. Don't apply hold-gating to it.

**Cable-wrap protection (`engine/wrap.go` + `startGoto`/`stepMove`).** For infinite-azimuth rotators. The signed wind accumulator (`e.wrap`) is integrated from *observed* readback (`shortestDelta` between samples), so it stays correct through interrupted/held moves; it is **not** set from the planned delta. `planMove` decides per absolute move: `MoveShort` (within ±limit → precise `SetPan`), `MoveUnwrap` (would over-wrap → drive the long way under closed-loop jog until the planned travel completes, then `Stop`), or `MoveBlock` (no representation within limit → refuse). Wind persists in `pelcots.yaml` across runs because the cable stays physically wound; `z`/`ZeroWrap` resets it.

**Transport (`internal/serialio`).** `Port` wraps an `io.ReadWriteCloser`, so the framing reader (`read`/`extract`, self-healing Pelco-D resync) is identical for serial (`Open`) and TCP serial bridges (`Dial`). Frames are delivered on a channel of `Event` (raw bytes / decoded `Frame` / error).

**Inbound control (`internal/control`).** Two text protocols translate to a common `Command` (`KindStop`/`KindSetPos`/`KindJog`) handed to `engine.Submit`: **rotctld** (newline) and **Yaesu GS-232A** (CR). Queries are answered from the thread-safe `Pos` snapshot the engine publishes. Servers are off by default and bind `127.0.0.1` (least privilege); azimuth↔pan, elevation↔tilt.

**`internal/pelco`.** Pure protocol: fixed 7-byte frames, command builders (`SetPan`/`SetTilt` with pan-wrap + tilt 0–90 clamp, `Jog`, `Stop`, queries), `Direction.Cmd2()`. No I/O — reuse these rather than hand-rolling angle math or frame bytes.

**`internal/config`.** YAML load/save. `Load` returns `Default()` for a missing file (first run works). `main.go` layers explicitly-set flags over the loaded config via `flag.Visit`, and auto-saves on quit (TUI) / on signal + periodically (daemon).

**`internal/ui`.** Bubble Tea `Model` is a pure view/controller: it renders `eng.Snapshot()` and maps keys to engine calls. Adding an editable field means updating the `focus*` iota block and `fieldCount` together — `focus_test.go` asserts the tab-cycle order.

## Conventions

- Pelco-D angles are azimuth = pan (0–359.99°, wraps), elevation = tilt (0–90°, clamped). The unit reports position in hundredths of a degree.
- All TX flows through the engine so it is logged once in the unified trace and the port has a single writer; don't write to the port from elsewhere.

---

## Station model and shared conventions

Shared documentation lives in `../stationa/docs/` (the stationa meta-repo, cloned
adjacent to this repo).

| Document | Path |
|---|---|
| Station integration model (three-plane MQTT contract) | `../stationa/docs/station-integration-model.md` |
| Config and secrets convention | `../stationa/docs/conventions/config-and-secrets.md` |
| Deployment convention | `../stationa/docs/conventions/deployment.md` |
| Canonical band/mode vocabulary | `../stationa/docs/conventions/band-mode-reference.md` |

This project implements the `muehle/hf/rotator` slot. When aligning this bridge with
the station model, the component-specific MQTT schema should be documented in `docs/`
in this repo following the template at `../stationa/docs/templates/mqtt-schema.md`.
