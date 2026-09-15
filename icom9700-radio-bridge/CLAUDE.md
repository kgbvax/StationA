# CLAUDE.md — icom9700-radio-bridge

icom9700-radio-bridge fronts the **Icom IC-9700** as the station's UHF radio: the
canonical `radio` slot `muehle/uhf/radio` on the station bus, controlled over CI-V
via Icom's RS-BA1-style LAN protocol (UDP control :50001 + CI-V data :50002; the
:50003 audio stream is out of scope in v1 — see `docs/civ-research-brief.md`).

The defining design constraint (KTD-2, user-settled): **the radio's single LAN
session stays freely available for manual wfview operating** — the bridge is a
polite on-demand client. It connects only on demand (a pending `/cmd`, an armed
permit, or a safety-driven PTT-off delivery), never auto-steals the session, and
disconnects after the idle timeout. While disconnected, radio-measured `/state`
fields are omitted (not zeroed) and `device_online` reads `false` as a *healthy
idle* state — `session_state` (`idle|connecting|live|error`) is the
idle-vs-fault discriminator.

PTT sits behind a **software arm gate** (KTD-4): `armed` is a bridge-held permit
that drops on session loss and restart (fail-disarmed); a max-TX watchdog bounds
a keyed PTT (KTD-5). The gate is software-only — the IC-9700 has no external
TX-inhibit path here; that accepted posture is recorded in the exposure review.

**Status: in progress — U1 scaffold (this), U2-U7 pending** (see
`../docs/plans/2026-09-14-001-feat-icom9700-radio-bridge-plan.md`). The MQTT
plane, config, deploy shell and the `internal/radio.Manager` seam are landed;
`internal/radio` is an obviously-marked stub (every connect fails with
`ErrNotImplemented` and the loop backs off 2 s → 60 s), so a deployed scaffold
sits politely idle on the bus. Protocol constants live in
`docs/civ-research-brief.md`.

---

## Commands

```bash
make build        # local binary -> bin/icom9700-radio-bridge
make pi           # cross-compile for Raspberry Pi arm64 -> bin/...-linux-arm64
make test         # unit tests (no network required)
make test-race    # tests with race detector
make vet          # go vet
make fmt          # gofmt -s -w .
make run          # build + run with config.example.toml
```

Run a single test package:
```bash
go test ./internal/config/...
```

CLI flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `/etc/icom9700-radio-bridge/config.toml` | Path to TOML config file |
| `-log.level` | (from config) | `debug` \| `info` \| `warn` \| `error`; overrides config |

There is deliberately **no password flag** — no secret ever rides a command
line. An explicitly-passed `-config` path that is missing is **fatal** (exit 2,
wraps `fs.ErrNotExist`); an absent *default* path runs on built-in defaults.

---

## Architecture

**Wiring** (`cmd/icom9700-radio-bridge/main.go`, flexbridge shape): the radio
manager is constructed BEFORE the MQTT connect so the `/cmd` handler exists when
OnConnect fires. The paho handler must not block (a radio command is a UDP
round-trip), so it copies the payload and Enqueues onto a bounded jobs channel
(32); one `sharedmqtt.RunJobs` worker runs `Manager.Execute` serially. LWT
("offline", QoS 1, retained) on `/status`; OnConnect publishes "online" and
re-subscribes `/cmd` at **QoS 0** (one-shot class, KTD-6/R9 — a persistent
session must not replay stale cmds). Deliberately **no paho ConnectRetry**: the
initial connect failure must reach `run()` and exit non-zero (ultrabridge
convention, model §8.1 item 10 — systemd crash-loops until the broker answers;
shelly/powerseq/antennaselect use the same shape). `sharedmqtt.Connect` is
ctx-aware so a SIGTERM during connect is honored.

**Radio loop** (`radioLoop`): `Manager.Connect` → on success a poll tick at
`radio.poll_interval` → on any failure `Disconnect` + backoff (2 s, x1.5, cap
60 s) → retry until ctx is cancelled. U4 wraps this loop with the on-demand
session policy (idle timeout, never-steal contention, safety-driven reconnects);
U6 adds the loss-of-control rules (TX watchdog, MQTT-loss PTT-off, disarm on
session loss).

**Seam:** `internal/radio.Manager` — `Connect(ctx) error`, `Disconnect()`,
`Execute(ctx, payload []byte) error`, `Poll(ctx) error`. U2-U5 fill it; the U1
`Stub` rejects everything with `radio.ErrNotImplemented`.

| Package | Role |
|---|---|
| `internal/radio` | Manager seam + U1 stub (U2 transport, U3 CI-V codec, U4 session manager land here) |
| `internal/config` | TOML config, flags, `ICOM9700_*` env overrides |
| `docs/civ-research-brief.md` | Committed protocol constants (ports, handshake, packet layer, CI-V command table) |

---

## MQTT topics

```
muehle/uhf/radio/meta      retained  birth certificate (capabilities + read-only expose, R8)
muehle/uhf/radio/state     retained  live state JSON snapshot (R5/R6)
muehle/uhf/radio/status    retained  online | offline (LWT — the bridge process)
muehle/uhf/radio/cmd       not retained  one-shot intent (bus -> bridge), value-keyed args
```

Landed in U1: the LWT plane (`/status`) and the `/cmd` subscription (stub-rejected).
Planned (U5): `/meta` + `/state` per R5-R9 — top-level active-TX fields
(`freq_hz`, `band`, `mode`, `tx` as the `"tx"|"rx"` string enum) plus `main`/`sub`
VFO objects, `selected_vfo`, `satellite`, `session_state`, `device_online`
(CI-V control-session liveness — healthy idle reads `false`, R16), `armed`,
meters. `/cmd` actions: `set_freq`, `set_mode`, `set_data`, `set_preamp`,
`set_attenuator`, `set_power`, `sat_mode`, `arm`/`disarm`, `ptt`. The full wire
contract lands in `docs/mqtt-api.md` (U8).

---

## Configuration and secrets

Config is TOML (`/etc/icom9700-radio-bridge/config.toml` by default, or
`-config <path>`). Both secrets are **env-only** — a `password` key anywhere in
the TOML is a hard parse error:

```bash
# /etc/icom9700-radio-bridge/icom9700-radio-bridge.env  (0600, owned by the service user)
ICOM9700_MQTT_PASSWORD=<password>
ICOM9700_CIV_PASSWORD=<radio remote-control login password>
```

The systemd unit contains `EnvironmentFile=.../icom9700-radio-bridge.env`. The
env prefix is `ICOM9700_` (plan-pinned, shorter than the dir-name-derived
convention); the other overridable vars are listed in `config.example.toml`.

Key defaults: `radio_host = "9700.kgbvax.net"`, `[mqtt] broker =
"tcp://bwbroker:1883"` (**KTD-7**: bwbroker DNS indirection — must resolve to
the muehle/#-authoritative broker, never the HA consumer broker; deploy.sh
gates on this), site/station/slot `muehle`/`uhf`/`radio`, `[session]
idle_timeout=120s tx_watchdog=180s max_attempts=3 attempt_spacing=30s`,
`[radio] poll_interval=1s`.

See `../docs/conventions/config-and-secrets.md` for the full convention.

---

## Deployment

Target: Raspberry Pi (current address `192.168.1.140`, user `io`).

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
```

The unit is network-only strict: NoNewPrivileges, ProtectSystem=strict,
PrivateDevices, RestrictAddressFamilies=AF_INET AF_INET6, empty
CapabilityBoundingSet, **MemoryMax=256M** (UDP retransmit buffers and all
radio-silence-growable structures must stay bounded, R18). Seed-once 0600 TOML
+ 0600 env file. Deploy gates (plan Deployment notes) apply before the first
live deploy — exposure review, bwbroker resolution, radio-side prerequisites,
and the unkey-on-session-loss Go/No-Go gate.

See `../docs/conventions/deployment.md` for the general pattern.

---

## Station model and shared conventions

Shared documentation lives in `../docs/` (this component is a subdirectory of the stationa monorepo).

| Document | Path |
|---|---|
| Station integration model (three-plane MQTT contract) | `../docs/station-integration-model.md` |
| Feature plan (KTDs, requirements, units) | `../docs/plans/2026-09-14-001-feat-icom9700-radio-bridge-plan.md` |
| CI-V over LAN protocol brief | `docs/civ-research-brief.md` (this module) |
| Config and secrets convention | `../docs/conventions/config-and-secrets.md` |
| Deployment convention | `../docs/conventions/deployment.md` |
| Logging convention (slog, component/slot attrs) | `../docs/conventions/logging.md` |
| Canonical band/mode vocabulary | `../docs/conventions/band-mode-reference.md` |

This project implements the `muehle/uhf/radio` slot and must conform to those
conventions.
