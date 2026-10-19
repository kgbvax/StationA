# CLAUDE.md — powerseq

`powerseq` is the station startup/shutdown **sequencer** — a logic slot (no
device) implementing the integration-model `sequencer` role at
`muehle/hf/power-seq` (model §4, §7.1). It subscribes the `/status` of every
slot its sequence references (and the `/state` of every `wait_state` target)
and, on the operator one-button `/cmd` (`start`|`stop`, **not retained**), runs
an ordered sequence over those slots' retained `/cmd` with delays and liveness
confirmations at each step.

The sequence is **config-driven**, not hard-coded: a pair of ordered step lists
(`[[startup]]` / `[[shutdown]]`) in `config.toml` define it. Each step is one of
four kinds — `cmd` (emit a retained `/cmd`), `wait_status` (wait for N slots'
`/status`), `wait_state` (wait for a slot's `/state` field; implicit two-layer
liveness precondition — `/status` online AND `/state.device_online` not `false`
when present — so neither a dead bridge nor a dead device behind a live bridge
can pass on stale retained `/state`), or `delay` (a literal `duration_s` or a symbolic `network`/`stagger`
ref into `[timing]`). `config.example.toml` ships the model §7.1 sequence as the
default; edit it to change the order, targets, waits, or delays for a different
setup. The subscribed topics and the `/meta` `controls`/`watches` are derived
from the configured sequence, so adding a target in TOML extends subscriptions
with no Go change.

It is **one writer** of those slots but does **not** lock them — any channel
stays directly toggleable for troubleshooting while the sequencer is idle.

Its name follows the stationa bridge-naming convention (logic-slot / service
name `powerseq`; see `../docs/conventions/naming.md`).

---

## Commands

```bash
go build ./cmd/powerseq                          # local build
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o dist/powerseq-linux-arm64 ./cmd/powerseq    # Pi cross-compile
go test ./...                                    # unit tests (no network/hardware)
go test ./... -race                              # tests with race detector
go vet ./...                                     # vet
gofmt -s -w .                                    # fmt
./deploy.sh                                      # cross-compile, ship, install as hardened systemd service
./powerseq -check -config config.toml            # validate a config and exit
```

Run a single test:
```bash
go test ./internal/seq/... -run TestStartupOrder
go test ./internal/config/...
```

CLI flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `/etc/powerseq/config.toml` | Path to TOML config file |
| `-log.level` | (from config) | `debug` \| `info` \| `warn` \| `error`; overrides config |

---

## Architecture

**Data flow:** station bus `/status` + `hf/pa/state` → `internal/mqtt`
(observations) → `internal/seq` (state machine) → `internal/mqtt` (`/cmd`
emission + own `/state`).

1. `cmd/powerseq/main.go` — flags, config load, signal ctx, mqtt connect, start
   the sequencer runner goroutine, wait on signal.
2. `internal/seq` — the state machine: a single **runner goroutine** that
   executes one sequence at a time (`runSequence` over the configured startup or
   shutdown step list), emitting retained `/cmd` and waiting on liveness (polling
   a mutex-guarded input map with a step-timeout deadline; optional `hold_ms`
   debounce). Subscriptions and `/meta` `controls`/`watches` are derived from the
   configured sequence. A broker disconnect gates cmd + wait steps (fault, no
   stall). Phase/step/fault are published as a retained `/state` snapshot.
   `Start()`/`Stop()` are non-blocking and phase-guarded.
3. `internal/mqtt` — paho wiring: ctx-aware connect, LWT on
   `power-seq/status`, subscribes the derived `/status` + `/state` topics + own
   `/cmd`, routes observations to the sequencer, publishes `/meta` on
   (re)connect, bounds `Publish` so a broker outage cannot stall the runner.
4. `internal/config` — TOML config (incl. the `[[startup]]`/`[[shutdown]]`
   step lists), flags, `POWERSEQ_MQTT_*` env overrides, sequence validation.

**Default sequence** (shipped in `config.example.toml`, cold start 2026-10-10):
`power/shack-pc-mains` set_power on → `power/psu-13v8` (TRX PSU) set_power on → 5 s → WOL bwpc → wait `hf/switch` online (retained-cmd collapse guard) →
`hf/switch` set_trx on → 1 s → `hf/switch` set_pa on → `power/antennas`
set_power on → wait `hf/radio` online → wait `hf/pa` `state.power` on →
`hf/pa-arm` set_enabled true → `phase=running`.
The site master mains is out of scope (Home Assistant; it powers the Pis and
the network too) — no step touches it.
**Shutdown** is the reverse with short staggers for inrush → `phase=idle`. It
never switches `power/shack-pc-mains` off (a hard power-off of Windows); neither
does standby.

**Idle standby** (optional `[[standby]]`/`[[wake]]` + `[idle]`): from `running`,
when `hf/antenna-select` reports `activity=inactive` (antenna grounded), no
`[[idle.watch]]` (UHF radio freq, UHF rotator position) moved and `running`
lasted `timeout_minutes` → `entering_standby` → antenna power off + shack PC
(bwpc) sleep (`retain=false`) → `standby`. A `last_activity` newer than the
standby entry, a watch moving, or `start` → `waking` → WOL to bwpc + antenna
power on → `running`. A watch change during a sequence or within `[idle]
settle_s` (default 30) of a phase change only re-baselines the watch: the UHF
rotator readbacks step when standby cuts the antenna power (link still up),
which woke every standby ~2 s later until 2026-10-19. A wake logs its reason.
**Idle shutdown** (optional `[idle] shutdown_minutes`,
default example 120, + `[[idle_shutdown]]`): still in `standby` that long after
the last activity (antennaselect `last_activity`, a watch, the last start/wake)
→ `stopping` → shutdown list + bwpc sleep → `idle` with
`/state.stop_reason=idle_shutdown`; only `start` revives it. The idle check
runs on the runner goroutine (1 s ticker), so it serializes with start/stop through `begin()`. Step kind `wol` sends the
magic packet (UDP broadcast; `RestrictAddressFamilies=AF_INET` covers it). A
step that times out → `fault` + `phase=idle`; the slots driven so far remain in
whatever state the retained `/cmd` last set them (self-healing, no rollback).

**Concurrency:** the paho message handlers run on paho's dispatch goroutine and
only do a quick mutex update + (for `/cmd`) a non-blocking send to the
sequencer's `cmdCh` — they **never publish**, so they never block (the stationa
memory: a paho handler must not call a blocking `Publish`). The runner goroutine
does all the `/cmd` emission + `/state` publishing; blocking the runner in a
liveness wait is safe — it is never a paho handler. `Sequencer.mu` guards the
status/state maps, the broker-online flag, and phase/step/fault.

**Busy guard:** `start` is honored when `phase=idle` (startup) or
`phase=standby` (= wake); `stop` when `phase=running`, `phase=standby` or `phase=idle` — the shutdown is an idempotent teardown that
can only de-energize, and it must not depend on the in-memory `fault` latch:
a process restart wipes the latch, and stop is then the only teardown path for
the still-energized slots a faulted sequence left behind (no rollback by
design). The guard is checked twice: once in `request()`
(a fast-path drop so an obviously-busy command never enqueues) and again
authoritatively in the runner's `begin()` (under `mu`, atomically transitioning
phase) — the re-check closes the TOCTOU window between `request()`'s phase
read and the runner's phase transition, so two rapid same-type commands can't
both replay the full sequence. A command that arrives mid-sequence is dropped
(logged).

---

## MQTT topics

```
muehle/hf/power-seq/meta     retained  birth certificate (capabilities + expose)
muehle/hf/power-seq/state    retained  { phase, step, fault?, stop_reason?, ts }
muehle/hf/power-seq/status   retained  online | offline (LWT — the service)
muehle/hf/power-seq/cmd      not retained   start | stop (operator one-button)
```

`/cmd` is **not retained** (a one-shot operator command; a stale retained
`start` replaying on service restart could re-energize the station
unexpectedly). It is also **subscribed at QoS 0** so the broker never queues a
`/cmd` for replay. Every received `/cmd` and every phase change is logged at
Info. The MQTT session is **clean** (`CleanSession=true`): all inputs are
retained and replay on each subscribe. The persistent session it replaced
stopped receiving every QoS-1 message on the HA broker on 2026-10-18 (only
the PLC's QoS-0 bridged topics arrived), so `hf/radio`/`hf/pa` were never seen
and cold starts timed out. A wait timeout now names what was seen, e.g.
`wait-radio-online: timeout (hf/radio status=none state=none)`. On (re)connect the runner re-publishes the retained
`/state` (in addition to `/meta` and `/status=online`) so a broker wipe that
drops retained messages restores an idle sequencer's `/state`. The controlled
slots' `/cmd` (which the sequencer *emits*) **are** retained — power slots and
the remote-on / arm-permit slots hold steady-state intent. See
`docs/powerseq-mqtt-api.md` for the full on-the-wire contract, including the
watched topics it subscribes.

---

## Configuration and secrets

Config is TOML (`/etc/powerseq/config.toml` by default, or `-config <path>`):
the sequencer's own address (site/station/slot) and `[timing]` (network delay,
step timeout, shutdown stagger). The MQTT password is **not** in the TOML — it
is loaded from an `EnvironmentFile`:

```bash
# /etc/powerseq/powerseq.env  (0600, owned by powerseq user)
POWERSEQ_MQTT_PASSWORD=<password>
```

The systemd unit contains `EnvironmentFile=/etc/powerseq/powerseq.env`. Env
overrides: `POWERSEQ_MQTT_BROKER`/`_CLIENT_ID`/`_USER`/`_PASSWORD`/`_SITE`. See
`../docs/conventions/config-and-secrets.md`.

---

## Deployment

Target: Raspberry Pi **scmino** (`192.168.1.178`, user `io`) since 2026-10-18.
The shari copy (`192.168.1.139`) is disabled, not removed (rollback). Never run
both: they share the client_id and the one-button `/cmd`.

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
```

Config is seed-once, with one exception: **config migrations**. When a release
needs a new config section, `migrate-config.sh` (run by `deploy.sh` on the
target) adds it to the existing `/etc/powerseq/config.toml` in place. It is
idempotent, backs the file up first (`config.toml.pre-migrate-<ts>`) and the
migrated file must pass `powerseq -check` with the new binary before the
service is stopped — otherwise the backup is restored and the deploy aborts.
Current migration: idle shutdown (`shutdown_minutes`, env `SHUTDOWN_MINUTES`
default 120, + `[[idle_shutdown]]` = the live `[[shutdown]]` + bwpc sleep).
Add future migrations to that script as their own idempotent block.

The unit is hardened like atr1k-tuner-bridge (network-only):
`ProtectSystem=strict`, `PrivateDevices=true` (no serial devices),
`RestrictAddressFamilies=AF_INET AF_INET6`, `MemoryMax=256M`, `TasksMax=64`.
See `../docs/conventions/deployment.md`.

---

## Station model and shared conventions

Shared documentation lives in `../docs/` (this component is a subdirectory of the stationa monorepo).

| Document | Path |
|---|---|
| Station integration model (§4 `sequencer` role, §7.1 `hf/power-seq` slot + the startup/shutdown sequence) | `../docs/station-integration-model.md` |
| Config and secrets convention | `../docs/conventions/config-and-secrets.md` |
| Deployment convention | `../docs/conventions/deployment.md` |
| Bridge-naming convention | `../docs/conventions/naming.md` |

This project implements the `muehle/hf/power-seq` slot and must conform to those
conventions. Component-specific schema lives in `docs/powerseq-mqtt-api.md`.