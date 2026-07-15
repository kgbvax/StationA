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
`/status`), `wait_state` (wait for a slot's `/state` field; implicit
`/status`-online precondition so a dead device cannot pass on stale retained
`/state`), or `delay` (a literal `duration_s` or a symbolic `network`/`stagger`
ref into `[timing]`). `config.example.toml` ships the model §7.1 sequence as the
default; edit it to change the order, targets, waits, or delays for a different
setup. The subscribed topics and the `/meta` `controls`/`watches` are derived
from the configured sequence, so adding a target in TOML extends subscriptions
with no Go change.

It is **one writer** of those slots but does **not** lock them — any channel
stays directly toggleable for troubleshooting while the sequencer is idle.

Its name follows the stationa bridge-naming convention (logic-slot / service
name `powerseq`; see `../stationa/docs/conventions/naming.md`).

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

**Default sequence** (model §7.1, shipped in `config.example.toml`):
`power/master` set_power on → ~30 s (network) → `power/psu-13v8` set_power on →
wait `hf/switch` + `hf/pa-arm` + `hf/ant-switch` `/status` online → `hf/switch`
set_trx on → wait `hf/radio` `/status` online → `hf/switch` set_pa on → wait
`hf/pa` `state.power` on → `hf/pa-arm` set_enabled true → `phase=running`.
**Shutdown** is the reverse with short staggers for inrush → `phase=idle`. A
step that times out → `fault` + `phase=idle`; the slots driven so far remain in
whatever state the retained `/cmd` last set them (self-healing, no rollback).

**Concurrency:** the paho message handlers run on paho's dispatch goroutine and
only do a quick mutex update + (for `/cmd`) a non-blocking send to the
sequencer's `cmdCh` — they **never publish**, so they never block (the stationa
memory: a paho handler must not call a blocking `Publish`). The runner goroutine
does all the `/cmd` emission + `/state` publishing; blocking the runner in a
liveness wait is safe — it is never a paho handler. `Sequencer.mu` guards the
status/state maps, the broker-online flag, and phase/step/fault.

**Busy guard:** `start` is honored only when `phase=idle`; `stop` when
`phase=running`, or `phase=idle` with a `fault` set (resume an interrupted
shutdown — idempotent re-run). The guard is checked twice: once in `request()`
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
muehle/hf/power-seq/state    retained  { phase, step, fault?, ts }
muehle/hf/power-seq/status   retained  online | offline (LWT — the service)
muehle/hf/power-seq/cmd      not retained   start | stop (operator one-button)
```

`/cmd` is **not retained** (a one-shot operator command; a stale retained
`start` replaying on service restart could re-energize the station
unexpectedly). It is also **subscribed at QoS 0** so the broker never queues a
`/cmd` published while powerseq is offline for replay on reconnect (a
persistent session would otherwise queue a QoS-1 `/cmd`). `/status` and
`/state` keep QoS 1 + the persistent session so their last retained value
replays on reconnect. On (re)connect the runner re-publishes the retained
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
`../stationa/docs/conventions/config-and-secrets.md`.

---

## Deployment

Target: Raspberry Pi (`shari`, `192.168.1.139`, user `io`).

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
```

The unit is hardened like atr1k-tuner-bridge (network-only):
`ProtectSystem=strict`, `PrivateDevices=true` (no serial devices),
`RestrictAddressFamilies=AF_INET AF_INET6`, `MemoryMax=256M`, `TasksMax=64`.
See `../stationa/docs/conventions/deployment.md`.

---

## Station model and shared conventions

Shared documentation lives in `../stationa/docs/` (the stationa meta-repo,
cloned adjacent to this repo).

| Document | Path |
|---|---|
| Station integration model (§4 `sequencer` role, §7.1 `hf/power-seq` slot + the startup/shutdown sequence) | `../stationa/docs/station-integration-model.md` |
| Config and secrets convention | `../stationa/docs/conventions/config-and-secrets.md` |
| Deployment convention | `../stationa/docs/conventions/deployment.md` |
| Bridge-naming convention | `../stationa/docs/conventions/naming.md` |

This project implements the `muehle/hf/power-seq` slot and must conform to those
conventions. Component-specific schema lives in `docs/powerseq-mqtt-api.md`.