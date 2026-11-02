# CLAUDE.md — icom9700-radio-bridge

icom9700-radio-bridge fronts the **Icom IC-9700** as the station's UHF radio: the
canonical `radio` slot `muehle/uhf/radio` on the station bus. **Receive-only
posture (2026-09 pivot): there is no LAN CI-V command path** — remote TX control
(arm/PTT/tuning) was removed from the bridge and the console; if control is ever
needed again it will be via serial CI-V, not LAN. What remains:

- **RX audio capture** (demand-driven): an `audio_on` /cmd opens the :50003
  receive-audio stream inside the RS-BA1 session and publishes demodulated audio
  (S16LE 48 kHz mono UDP to every `audio.publish_addr` target — a string or an
  array: vhfcam preview :45031, bmrec recorder :45032); the demand is held per
  `holder` (cmd field, missing = `"default"`, max 8 × 32 chars), each
  TTL-bounded (`audio.demand_ttl`, 60 s, refreshed by that consumer's audio_on
  heartbeats) so a dead consumer never pins the radio session (KTD-2) and one
  consumer's `audio_off` never drops another's hold (2026-10).
  The LAN CI-V data stream is NOT opened (`NoCIVData`) — the session carries
  control-keepalives + audio only.
- **Serial CI-V telemetry** (`internal/civserial`): freq/mode arrive as
  unsolicited transceive broadcasts, meters are polled read-only
  (`serial.meter_interval`), everything broadcasts on MQTT on change. Gated by
  the sticky `monitor_on`/`monitor_off` cmds. `power_on` sends the wake frame
  over serial. Both need `serial.device` (a CI-V port on the bridge's host);
  it is unset on scmino, so both are disabled there (see Deployment).

The defining design constraint (KTD-2, user-settled): **the radio's single LAN
session stays freely available for manual wfview operating** — the bridge is a
polite on-demand client. It connects only on demand (an audio demand), never
auto-steals the session, and disconnects after the idle timeout. While
disconnected, `device_online` reads `false` as a *healthy idle* state —
`session_state` (`idle|connecting|live|error`, redefined 2026-09 as the
CAPTURE-session state) is the idle-vs-fault discriminator. Telemetry `/state`
fields are gated on the serial monitor, not on the LAN session: omitted (not
zeroed) while the monitor is off or the radio does not answer. There is no arm
gate, no PTT, no TX watchdog — the safety layer was removed with the control
plane; the historical record lives in `docs/known-issues.md` (firmware PTT-off
gap) and `../docs/known-issues.md` (exposure review).

**Status: implementation complete** (U1–U8 + the 2026-09 receive-only pivot).
The serial monitor is not in use: it needs a second CI-V port on scmino (see
Deployment), then a bench bring-up (radio menu: CI-V USB Baud 115200, CI-V
Transceive ON). Protocol constants live in
`docs/civ-research-brief.md` and `docs/civ-wire-spec.md`.

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
manager (and, when `serial.device` is configured, the serial monitor) are
constructed BEFORE the MQTT connect so the `/cmd` handler exists when OnConnect
fires. The paho handler must not block, so it copies the payload and Enqueues
onto a bounded jobs channel (256); one `sharedmqtt.RunJobs` worker runs the
action serially. LWT ("offline", QoS 1, retained) on `/status`; OnConnect
publishes "online" and re-subscribes `/cmd` at **QoS 0** (one-shot class,
KTD-6/R9 — a persistent session must not replay stale cmds). Deliberately **no
paho ConnectRetry**: the initial connect failure must reach `run()` and exit
non-zero (ultrabridge convention, model §8.1 item 10 — systemd crash-loops
until the broker answers; shelly/powerseq/antennaselect use the same shape).
`sharedmqtt.Connect` is ctx-aware so a SIGTERM during connect is honored.

**Telemetry**: no LAN polls. The serial monitor (internal/civserial) pushes
on-change updates; the bridge folds them into the retained /state (dedup + a
60 s freshness heartbeat, KTD14).

**Seam:** `internal/radio.Manager` — `Run(ctx)`, `SetAudioDemand(ctx, on)`,
`Snapshot()`, `Notify()`. `internal/bridge.Monitor` — the narrow interface
main.go adapts civserial onto.

| Package | Role |
|---|---|
| `internal/civ` | RS-BA1 UDP transport + CI-V codec + the scripted fake radio (NoCIVData skips the CI-V data stream) |
| `internal/civserial` | read-only serial CI-V telemetry monitor (Port interface + go.bug.st/serial adapter) |
| `internal/radio` | on-demand capture-session manager (idle/connecting/live/error; the audio demand is the only hold) |
| `internal/bridge` | bus surface (meta/state/cmd/status) |
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
Current contract (docs/mqtt-api.md is authoritative): `/state` = capture-session
state + `audio_demand` + `audio_holders` + `monitor` + monitor-gated telemetry (`freq_hz`, `band`,
`mode`, `satellite`, `s_meter`, `swr`, `alc`; `tx_power` only when the bench
proves the read) — omitted while the monitor is off or the radio is deaf, never
zeroed, never frozen. `/cmd` actions: exactly `audio_on`, `audio_off`,
`power_on`, `monitor_on`, `monitor_off`, `park_radio`; the audio pair takes an
optional `holder` (per-consumer demand, see docs/mqtt-api.md).

**park_radio** (2026-10-08): turns satellite mode off, then split off (`S 0 VFOA`, 2026-10-09 — a park left the radio in split, TX on VFO B), then sets mode + frequency
to `[park]` (`freq_hz`, `mode`) and reads split, frequency and mode back (an error if the radio
disagrees — live, a radio in sat mode answered `M` with RPRT 0 and stayed LSB;
leaving sat mode also restores a different main frequency, so the order is
satmode off → split off → frequency → mode: the radio restores a per-band stored mode, here CW, on the band change) through a hamlib **rigctld on scmino** (`park.rigctld_addr`,
`rigctld-scmino/` holds its hardened unit + `deploy.sh`; loopback only, via systemd
`IPAddressAllow` — the bridge runs next to it on scmino and dials `127.0.0.1:4532`; until
2026-10-19 the scmino config still dialled the LAN address, which rigctld refused, so park_radio
failed after the move). `internal/rigctl` can send exactly satmode-off, split-off, mode and
frequency (plus the three readbacks) — no PTT/power/raw CI-V. The bridge itself never opens a serial port
for control; the radio's COM port belongs to other software.

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
"tcp://bwbroker:1883"` (**KTD-7**: bwbroker DNS indirection; deploy.sh gates
only on the name resolving on the target. KTD-7's "never the HA consumer
broker" rule belongs to the split-direction shari design, which was never
deployed; `../docs/conventions/mqtt-topology.md` records that live services
resolve `bwbroker` to `.50`. This bridge's live config is not verified),
site/station/slot `muehle`/`uhf`/`radio`, `[session]
idle_timeout=120s max_attempts=3 attempt_spacing=30s`, `[serial] device=""
baud=115200 meter_interval="500ms" power_on_frame="1a050201"` (device empty =
monitor_*/power_on rejected), `[audio] publish_addr="" demand_ttl="60s"`
(publish_addr: one `"host:port"` or an array, every target validated at
startup; empty = no PCM published).

See `../docs/conventions/config-and-secrets.md` for the full convention.

---

## Deployment

Target: **scmino** at `192.168.1.178` (since 2026-10-18; `deploy.sh` default,
user `io`). `serial.device` is unset there **by design**: the only IC-9700 CI-V
adapter on scmino (Prolific `AMBVb148313`) belongs to `rigctld-ic9700` for
park_radio, and the bridge opens no serial port (2026-10-08 decision), so the
monitor and `power_on` are disabled. Enabling them needs a second CI-V port —
and a unit change too: `PrivateDevices=true` gives the service a private `/dev`
without real ttys, so the `DeviceAllow` lines alone do not expose one.

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
```

The unit is strict: NoNewPrivileges, ProtectSystem=strict,
PrivateDevices + DeviceAllow for the serial device classes
(char-ttyUSB/char-ttyACM/char-serial — the monitor's only device access),
RestrictAddressFamilies=AF_INET AF_INET6, empty CapabilityBoundingSet,
**MemoryMax=256M** (UDP retransmit buffers and all radio-silence-growable
structures must stay bounded, R18). Seed-once 0600 TOML + 0600 env file.
Radio-side prerequisites: CI-V USB Baud 115200, CI-V Transceive ON; the
exposure review and bwbroker resolution notes are recorded in the station
register `../docs/known-issues.md`.

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
