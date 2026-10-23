# Logging convention — slog, stderr, component/slot attrs

## Context

Error-handling behavior is already consistent across the Go components (fatal only at
startup; log-and-retry at runtime; LWT on `/status`; error detail in the retained
`/state`), but the *logging* was not. As of 2026-09-03 the modules split three ways:

- `log/slog` text → stderr: flexbridge, acom1200s-pa-bridge, wrc-rotator-bridge,
  atr1k-tuner-bridge, shelly-power-bridge, powerseq
- std `log` with `[prefix]` strings: ultrabridge, antennaselect, hadiscovery, testui
- no logging package: pelcobridge2 (TUI app — engine events went to the on-screen log
  pane only, invisible to the journal)

Even the slog users flattened everything into format strings through the shared
`Logger{Infof,Warnf,Debugf}` interface, so no line was filterable by component, slot,
or severity. On shari, `journalctl -p warning` returned nothing while ~19 % of lines
contained error text — severity filtering was broken because no module logged at
`Error`/`Warn` level.

journald on each service host (shari, scmino) holds the station's service logs (shari:
`Storage=persistent`, cap 500 M since 2026-09-03; scmino: to be recorded). Services
write to stderr; the journal collects them. Nothing writes its own log file, except
winpc-power-bridge on bwpc, which has no journald (§4) — `hf-mqtt-capture` already
provides the `hf` bus-side record.

## Convention

### 1. `log/slog` everywhere; text handler → stderr

Every Go service builds one root logger at startup:

```go
level := slog.LevelInfo // from the [log] level config key, where one exists
logger := slog.New(logging.NewHandler(os.Stderr, &slog.HandlerOptions{
    Level: level,
})).With("component", "flexbridge") // constant, per-service
slog.SetDefault(logger)
```

`logging` is `codeberg.org/kgbvax/stationa/shared/logging`. Under systemd (it sets
`JOURNAL_STREAM`) the handler prefixes each line with its syslog priority (`<3>`
Error, `<4>` Warn, `<6>` Info, `<7>` Debug); journald strips the prefix and files the
line at that priority. A bare `slog.NewTextHandler` lands every line at priority info,
so `journalctl -p warning` never sees it — that was the state from the 2026-09-04
migration until 2026-10-10. Outside the journal (terminal, tests) the handler is the
plain text handler.

- Text format, not JSON: humans read it via `journalctl`; journald already adds
  timestamps, unit, and PID per line, so the app must not re-emit timestamps.
- stderr, not stdout: systemd captures both, but stderr is the error stream by
  contract and keeps `journald`'s unit attribution intact.

### 2. `component` and `slot` attributes — never format-string-embedded identity

- Each service stamps one constant `component` attr (the service name, normally equal
  to the systemd unit / slot's bridge name; exception: antennaselect logs
  `component=antennaselect` under unit `antenna-select`).
- Anything slot- or device-specific goes through a child logger:
  `logger.With("slot", "muehle/power/psu-13v8")`. Multi-slot services (shelly-power-bridge
  fronts three plugs) MUST create one child logger per slot — a shared logger that
  cannot tell its slots apart in the log is a bug.
- New code passes real attrs (`slog.Warn("cmd rejected", "action", a, "err", err)`);
  pre-formatted `Printf`-style is tolerated only inside the legacy
  `Logger{Infof,Warnf,Errorf,Debugf}` bridges (see §5).

### 3. Levels mean something — errors must be filterable

| Level | Use for | Examples |
|-------|---------|----------|
| `Debug` | Protocol/trace detail, per-exchange noise | raw frame dumps, poll ticks |
| `Info` | Lifecycle events, normal operation | connect, reconnect, birth sequence, state published |
| `Warn` | Degraded but recovering — operator should notice | serial reopen after EIO, stale heartbeat, queue drop, device `device_online=false` transition |
| `Error` | Failure that lost state or data, needs intervention | publish failed, malformed JSON dropped, connect attempt failed, config fallback |

**The rule that makes consolidation work:** any line that an operator would grep for
as an error MUST be `Warn` or `Error`. `journalctl -p warning` is the station-wide
error filter; a module that logs its errors at `Info` breaks it.

The converse also holds: a reconnect loop against a device that is simply off must not
flood the filter with one `Warn` per backoff cycle. Use `logging.Streak` (shared/logging):
the first failure of an outage logs at `Warn`, repeats at `Info` with a `repeat` count,
and `Reset()` after a run that proved healthy re-arms the `Warn` (acom, atr1k and
flexbridge reset on their existing "run lasted ≥ 1 min" backoff test). Routine,
self-healing steps inside a run (e.g. re-sending a telemetry-enable) are `Info`.

Fatal startup paths log via slog, then exit: `os.Exit(2)` for config errors,
`os.Exit(1)` for connect/run errors (existing convention, unchanged).

### 4. No log files, no rotation, no syslog in the app

The app writes to stderr and nothing else. Rotation, retention, and persistence are
journald's job on each service host (shari and scmino). shari: `Storage=persistent`,
`SystemMaxUse=500M` (drop-in, since 2026-09-03). scmino: not yet recorded — verify and
apply the same drop-in. The MQTT traffic record of the `hf` station (`muehle/hf/#`,
including every `/state.error` value published there; 72 h retention by default) lives
separately in `hf-mqtt-capture` on shari (`/var/log/hf-mqtt-capture/…`). `uhf` and
`power` slots are not captured. Never open a log file in a service.

Recorded exception: `winpc-power-bridge` (Windows, bwpc) runs hidden (`-H=windowsgui`)
with no journald, so it writes `[log] file` (seeded from config.example.toml as
`winpc-power-bridge.log`, resolved next to the config; built-in default `""` = stderr;
moved to `<file>.1` at start above 5 MB); see
`winpc-power-bridge/CLAUDE.md`.

### 5. The `Logger{Infof,Warnf,Errorf,Debugf}` bridge interface

`flexbridge` (mirrored by atr1k, acom, wrc, shelly, powerseq) passes a minimal
interface into bridge/device code (shelly's and atr1k's tuner-package variants have no
`Errorf`). Keep it, with two constraints:

- Its adapter wraps the service's slog logger (so `component`/`slot` attrs are already
  attached), and its `Warnf`/`Errorf` implementations MUST call `slog.Warn`/`slog.Error`
  — never map everything to `Info`.
- New code should prefer calling slog directly; the interface exists so device-protocol
  packages stay free of a logging dependency.

### 6. Query recipes

Linux station services run on two hosts: scmino (`192.168.1.178`) runs all of them
except `spid-ercm-rotator-bridge` and `hf-mqtt-capture`, which run on shari
(`192.168.1.139`). `winpc-power-bridge` on bwpc logs to a file (§4 exception); the
ESPHome/PlatformIO firmware components (ant-switch, M5 Stamp PLCs, M5 control heads) log
on the device, not to journald.

```bash
# scmino: errors and warnings only, last hour
ssh io@192.168.1.178 journalctl -u flexbridge -u ultrabridge -u acom1200s-pa-bridge \
  -u atr1k-tuner-bridge -u wrc-rotator-bridge -u shelly-power-bridge \
  -u antenna-select -u antmode -u beamsteer -u hadiscovery -u powerseq \
  -u icom9700-radio-bridge -u rigctld-ic9700 -u logger-spot-bridge \
  -u oscarwatch-sattrack-bridge -u vhfcam-restream -u stationportal \
  -u testui -u hf-console-web -p warning -S -1h

# shari: errors and warnings only, last hour
ssh io@192.168.1.139 journalctl -u spid-ercm-rotator-bridge -u hf-mqtt-capture -p warning -S -1h

# One slot's error narrative including context (ultrabridge runs on scmino)
ssh io@192.168.1.178 "journalctl -u ultrabridge -S -24h | grep -E 'slot=muehle/hf/ant-ctrl|error'"

# What error detail the bus carried (capture recorder, on shari; files are named in UTC)
ssh io@192.168.1.139 "sudo grep 'error' /var/log/hf-mqtt-capture/\$(date -u +%Y-%m-%d)/\$(date -u +%H).log"
```

## Migration notes

- **Done 2026-09-04 (28f0670):** ultrabridge, antennaselect, hadiscovery, testui and
  hf-mqtt-capture moved from std `log` to slog; shelly-power-bridge has one child logger
  per slot with `slot` attr (§2). **Done 2026-10-10:** journald priority prefix via
  `shared/logging.NewHandler` (62e9e48, every Linux Go service except hf-console-web,
  whose webbridge still uses std `log`) and `logging.Streak` one-Warn-per-outage
  (625f46d, in the flexbridge/atr1k/acom reconnect loops; the antenna-mode refusal paths
  in wrc-rotator-bridge, ultrabridge, antmode and beamsteer adopted it 2026-10-22).
- **Open: flexbridge** — publishes `device_online` but no `/state.error`; see
  `docs/station-integration-model.md` §3 for the error-field contract it must adopt.
- **Open: component attr** — wrc-rotator-bridge and powerseq stamp no `component` attr
  (their `newLogger` / `mqtt.NewLogger` return `slog.New(h)` without
  `.With("component", …)`); hf-console-web (hf_console/webbridge) still logs through std
  `log` (no component, every line at priority info).

## When to apply

- **New component** → set up the root logger per §1 before any other code runs.
- **Touching an error path** → check its level against §3.
- **Adding a slot to a multi-slot service** → child logger with `slot` attr.

## Acceptance

1. `journalctl -p warning` on each service host (scmino and shari) returns every
   degraded/failure event of the last hour — no error text at `Info` level.
2. Every journal line from a service identifies `component` and, where a slot is
   involved, `slot`.
3. No service writes its own log file, except the recorded winpc-power-bridge
   exception (§4).