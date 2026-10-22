# CLAUDE.md — antmode

Station antenna-mode logic slot `muehle/hf/ant-mode` (unit `antmode`, runs on scmino). It owns the mode of the HF Ultrabeam (`hf/ant-ctrl`) and the HF rotator (`hf/rotator`, G-450DC, no brake): `normal`, `storm` or `maintenance`.

In storm and maintenance it publishes a retained **lock** in `/state`:
- `ground` → antennaselect grounds the switch,
- then `retract` → ultrabridge retracts the elements,
- then `park_az` → wrc-rotator-bridge parks the boom.

**The bridges enforce the lock and park themselves.** antmode never commands the rotator or the Ultrabeam. Its only command is `power/antennas` `set_power` (power borrow), plus clearing its own retained `/cmd`.

API (authoritative): `docs/ant-mode-mqtt-api.md`. Decision record: `../docs/known-issues.md` "HF antenna mode". Consumer parser: `../shared/antmode` (consumers read the lock only through it).

## Layout

| Path | What |
|---|---|
| `internal/policy` | Pure mode state machine: manual wins, presence, suggestion vs auto entry, auto exit over successful polls only, ack/dismiss. Fake-clock table tests. |
| `internal/heading` | Pure storm heading: wind ± 90°, +360 variants ≤ `max_az`, nearest the readback; stale → fallback. |
| `internal/nina` | BBK NINA client: dashboard → `dwd.*` CAP messages → II code filter; `Active(now)`. A failed poll is an error, never "no warnings". |
| `internal/dwd` | DWD `stationOverviewExtended` client: today's (Europe/Berlin) `days[]` wind. |
| `internal/engine` | Runtime: lock sequencing, power borrow, persistence, `/state` `/meta`, `/cmd` (one-shot, ts gate, startup hold, own-state adoption). Single-threaded on the jobs worker. |
| `internal/store` | Atomic JSON state file. |
| `internal/poll` | Poll goroutine: timeout, interval, backoff, `logging.Streak`. |
| `internal/mqtt` | Paho wiring. Every input is `Enqueue`d; no handler publishes. Clean session; own `/cmd` at QoS 0. |
| `internal/config` | TOML config (`config.example.toml`; unknown keys are an error). |

## Rules

- **Two-layer liveness** for every sibling: `/status` online **and** `/state.device_online` not false.
- **Presence:** manned only while powerseq is `starting`/`running`/`waking`. Offline or silent powerseq = unmanned.
- **`auto_enter` defaults to false** (observe only). Turn it on only after the suggestions have matched WarnWetter for some days (plan Phase 6).
- **Never publish `/state` before the startup hold (3 s) ends.** Without a state file, antmode adopts a locked retained own `/state`; an early `normal` would unlock the bridges.
- **The borrow persists `restore_to` before it publishes `set_power on`.** It restores only while powerseq's phase is unchanged, and then always publishes the retained `set_power off`, even when the plug already reads `off` (shelly-power-bridge replays its retained `/cmd` on reconnect).
- **Park once.** A wind veer never re-aims (G-450 has no brake). Drift is reported only.
- **`/cmd` is one-shot.** Clear every non-empty payload; ignore the empty echo.

## Build / test / deploy

```bash
go test -race ./...
NINA_ARS=<12 digits> DWD_STATION=<id> ./deploy.sh   # first deploy (seed-once /etc/antmode/config.toml); host scmino; unit "antmode"; state in /var/lib/antmode
```

The slot is listed in the station landing-page inventory (`stationportal/internal/inventory/inventory.toml`, `[[slot]]` + `[[software]]`) and in the hf_console host map and wiring (`hf_console/lib/store/station_hosts.dart`, `wiring.dart` `cmdRetain`/`expectedSlots`). Keep them in step if the host or unit changes.
