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

journald is the station's consolidated log (`Storage=persistent`, cap 500 M since
2026-09-03). Services write to stderr; the journal collects them. Nothing writes its
own log file — `hf-mqtt-capture` already provides the bus-side record.

## Convention

### 1. `log/slog` everywhere; text handler → stderr

Every Go service builds one root logger at startup:

```go
level := slog.LevelInfo // from the [log] level config key, where one exists
logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: level,
})).With("component", "flexbridge") // constant, per-service
slog.SetDefault(logger)
```

- Text format, not JSON: humans read it via `journalctl`; journald already adds
  timestamps, unit, and PID per line, so the app must not re-emit timestamps.
- stderr, not stdout: systemd captures both, but stderr is the error stream by
  contract and keeps `journald`'s unit attribution intact.

### 2. `component` and `slot` attributes — never format-string-embedded identity

- Each service stamps one constant `component` attr (the service name, equal to the
  systemd unit / slot's bridge name).
- Anything slot- or device-specific goes through a child logger:
  `logger.With("slot", "muehle/hf/psu-13v8")`. Multi-slot services (shelly-power-bridge
  fronts three plugs) MUST create one child logger per slot — a shared logger that
  cannot tell its slots apart in the log is a bug.
- New code passes real attrs (`slog.Warn("cmd rejected", "action", a, "err", err)`);
  pre-formatted `Printf`-style is tolerated only inside the legacy
  `Logger{Infof,Warnf,Debugf}` bridges (see §5).

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

Fatal startup paths log via slog, then exit: `os.Exit(2)` for config errors,
`os.Exit(1)` for connect/run errors (existing convention, unchanged).

### 4. No log files, no rotation, no syslog in the app

The app writes to stderr and nothing else. Rotation, retention, and persistence are
journald's job on shari (`Storage=persistent`, `SystemMaxUse=500M`). The MQTT
traffic record including every published `/state.error` value lives separately in
`hf-mqtt-capture` (`/var/log/hf-mqtt-capture/…`). Never open a log file in a service.

### 5. The `Logger{Infof,Warnf,Debugf}` bridge interface

`flexbridge` (mirrored by atr1k, acom, wrc, shelly, pelcobridge2's mqtt slot) passes a
minimal interface into bridge/device code. Keep it, with two constraints:

- Its adapter wraps the service's slog logger (so `component`/`slot` attrs are already
  attached), and its `Warnf`/`Errorf` implementations MUST call `slog.Warn`/`slog.Error`
  — never map everything to `Info`.
- New code should prefer calling slog directly; the interface exists so device-protocol
  packages stay free of a logging dependency.

### 6. Query recipes (shari)

```bash
# All station services, errors and warnings only, last hour
journalctl -u flexbridge -u ultrabridge -u acom1200s-pa-bridge \
  -u atr1k-tuner-bridge -u wrc-rotator-bridge -u shelly-power-bridge \
  -u antennaselect -u hadiscovery -u powerseq -u hf-mqtt-capture \
  -p warning -S -1h

# One slot's error narrative including context
journalctl -u ultrabridge -S -24h | grep -E 'slot=muehle/hf/ant-ctrl|error'

# What error detail the bus carried (capture recorder)
sudo grep 'error' /var/log/hf-mqtt-capture/$(date +%Y-%m-%d)/$(date +%H).log
```

## Migration notes

- **ultrabridge, antennaselect, hadiscovery, testui** — replace std `log` with slog per
  §1; keep the existing `[mqtt]`/`[reconcile]` style as a short message prefix or a
  `subsys` attr, whichever reads better; re-level their error paths per §3.
- **pelcobridge2** — it is interactive: engine/rotctld events stay in the TUI log pane,
  but `Warn`+ events MUST be mirrored to stderr slog so an operator session also lands
  in the journal. The MQTT slot already logs via `stderrLogger`; migrate it to slog.
- **shelly-power-bridge** — one child logger per slot with `slot` attr (§2).
- **flexbridge** — publishes `device_online` but no `/state.error`; see
  `docs/station-integration-model.md` §3 for the error-field contract it must adopt.

## When to apply

- **New component** → set up the root logger per §1 before any other code runs.
- **Touching an error path** → check its level against §3.
- **Adding a slot to a multi-slot service** → child logger with `slot` attr.

## Acceptance

1. `journalctl -p warning` on shari returns every degraded/failure event of the last
   hour — no error text at `Info` level.
2. Every journal line from a service identifies `component` and, where a slot is
   involved, `slot`.
3. No service writes its own log file.