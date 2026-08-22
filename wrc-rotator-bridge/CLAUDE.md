# CLAUDE.md — wrc-rotator-bridge

wrc-rotator-bridge bridges the **HF antenna rotator** (Yaesu G-450DC steered via an
AF6SA WRC controller) to MQTT using the station integration model (slot
`muehle/hf/rotator`). It dials the WRC's WebSocket status stream, publishes a
canonical rotator state snapshot, dispatches `/cmd` intent (`set_az`, `stop`,
`fwd`, `rev`) back to the rotator, and optionally runs two parallel legacy
control paths: a GS-232B TCP server on port 7373 and a PSTRotator-compatible
UDP listener on port 12040. Either path lets rotator-control software drive the
rotator directly; resulting motion still surfaces in `/state`. It is
**read-write**. The UHF sat rotator is a separate slot (`muehle/uhf/rotator`)
served by pelcobridge — no collision.

---

## Commands

```bash
go build ./cmd/wrc-rotator-bridge                 # local build
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o dist/wrc-rotator-bridge-linux-arm64 ./cmd/wrc-rotator-bridge   # Pi cross-compile
go test ./...                                 # unit tests (no network/hardware)
go test ./... -race                           # tests with race detector
go vet ./...                                  # vet
gofmt -s -w .                                 # fmt
./deploy.sh                                   # cross-compile, ship, install as hardened systemd service
```

Run a single test package:
```bash
go test ./internal/bridge/... -run TestHandleCommand
go test ./internal/gs232/...
go test ./internal/pstrotator/...
```

CLI flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `/etc/wrc-rotator-bridge/config.toml` | Path to TOML config file |
| `-log.level` | (from config) | `debug` \| `info` \| `warn` \| `error`; overrides config |
| `-debug` | `false` | Log WRC WebSocket I/O |

---

## Architecture

**Data flow:** WRC WebSocket → `internal/rotor` → `internal/bridge` → MQTT,
with `internal/gs232` and `internal/pstrotator` as parallel inbound control
paths.

1. `cmd/wrc-rotator-bridge/main.go` — flags, config load, signal ctx, MQTT
   connect+LWT, `/cmd` subscription + bounded worker, WRC WebSocket restart
   loop with exponential backoff, GS-232 server start.
2. `internal/rotor` — WRC WebSocket device: dial (ctx-aware `DialContext`),
   read loop parsing `RotorStatus` into canonical `State`, mutex-guarded
   command writes (`SetAz`/`Stop`/`Jog`), thread-safe `Snapshot`/`CurrentAz`
   for the GS-232 server.
3. `internal/bridge` — canonical rotator state model + MQTT publishing:
   `/meta` (role `rotator`, `axes [az]`, `expose`), retained `/state` snapshot
   with change-dedup, `/cmd` dispatch via a `Commander`.
4. `internal/gs232` — optional GS-232B TCP server: `C`/`C2` position query,
   `Mxxx`/`Wxxx` move, `S` stop. Drives the same `rotor.Device` the bridge
   does; the resulting motion surfaces in `/state`.
5. `internal/pstrotator` — optional PSTRotator-compatible UDP listener on port
   12040 (configurable). Accepts XML datagrams (`AZIMUTH`, `STOP`, `PARK`,
   `AZ?` query) and drives the same `rotor.Device`; the resulting motion
   surfaces in `/state`.
6. `internal/config` — TOML config, flags, `WRC_ROTATOR_BRIDGE_*` env overrides.

**Restart loop** (`wsLoop`): dials the WRC, publishes meta, runs the read loop
until the WebSocket errors or ctx cancels, then marks the device offline and
backoffs-and-retries. `/status` stays `online` while the bridge retries — only
`/state.device_online` flips. The initial paho `Connect()` is made ctx-aware
(bridging `tok.Wait()` through a goroutine + `select` on `ctx.Done()`) so a
SIGTERM while the broker is unreachable can interrupt the connect — otherwise
systemd must SIGKILL after `TimeoutStopSec`.

**Concurrency:** `rotor.Device.writeMu` guards the WebSocket and all writes
(commands from `/cmd` and from GS-232 handlers serialize on it). `rotor.Device.
stateMu` guards the last state snapshot. `bridge.Bridge.mu` guards the
canonical state and the dedup snapshot. The paho `/cmd` handler runs in paho's
goroutine and only enqueues onto a bounded channel — it never blocks (a
`set_az` writes to the WRC). GS-232 handlers run in their own accept goroutines
and reach the device only through the mutex-guarded `Controller` interface.

---

## MQTT topics

wrc-rotator-bridge publishes to the station integration model topics:

```
muehle/hf/rotator/meta     retained  birth certificate (capabilities + expose)
muehle/hf/rotator/state    retained  live rotator state JSON snapshot
muehle/hf/rotator/status   retained  online | offline (LWT — the bridge, not the WRC)
muehle/hf/rotator/cmd      not retained  set_az | stop | fwd | rev intent (bus → bridge)
```

`/state` is a single retained JSON document. Canonical fields: `az`
(degrees), `target_az` (commanded target, omitempty), `moving` (bool),
`rotor_state` (raw WRC state string, diagnostic), `device_online`/`error`. A
rotator carries **no** `freq_hz`/`band`/`mode` — those are radio concerns.
`/cmd` is **not retained**: a rotator move is a one-shot command; re-applying a
stale target on restart could spin the rotator unexpectedly. See
`docs/wrc-rotator-bridge-mqtt-api.md` for the full on-the-wire contract.

---

## Configuration and secrets

Config is TOML (`/etc/wrc-rotator-bridge/config.toml` by default, or
`-config <path>`). The MQTT password is **not** in the TOML — it is loaded from
an `EnvironmentFile` so it never appears in the unit file or process command line:

```bash
# /etc/wrc-rotator-bridge/wrc-rotator-bridge.env  (0600, owned by wrc-rotator-bridge user)
WRC_ROTATOR_BRIDGE_MQTT_PASSWORD=<password>
```

The systemd unit contains `EnvironmentFile=/etc/wrc-rotator-bridge/wrc-rotator-bridge.env`.
Env overrides: `WRC_ROTATOR_BRIDGE_MQTT_BROKER`/`_CLIENT_ID`/`_USER`/`_PASSWORD`/
`_SITE`/`_STATION`/`_SLOT`, `WRC_ROTATOR_BRIDGE_ROTOR_URL`. The GS-232 and
PSTRotator listeners are configured via `[gs232]` / `[pstrotator]` in TOML
(`deploy.sh` seeds them on first deploy).

See `../docs/conventions/config-and-secrets.md` for the full convention.

---

## Deployment

Target: Raspberry Pi (`shari`, `192.168.1.139`, user `io`).

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
```

**Migrating from the legacy `rotint` rotor bridge:** shari's prior rotator service
was the **upstream AF6SA `rotint` project** (`rotor-bridge.service`, binary
`/home/io/rotint/rotor-bridge`), not a stationa bridge — it ran as user `io`, passed
the MQTT password on the command line, had no TOML config, and published the
`rotor2mqtt/…` topic tree plus embedded HA discovery under `homeassistant/…`. This
bridge replaces it with the stationa three-plane schema (`muehle/hf/rotator/*` +
`/meta.expose` consumed by `hadiscovery`).

Run the one-time `./migrate-from-rotint.sh` **before** the first `./deploy.sh`. It
creates the new service user + config dir, extracts the MQTT password from the old
unit's `-password="…"` command line and writes it to
`/etc/wrc-rotator-bridge/wrc-rotator-bridge.env` (0600, owner the new user) **on the
device** so the password never leaves the Pi (this also fixes rotint's pre-existing
command-line secret exposure), then stops + disables `rotor-bridge.service` and
removes its unit. It deliberately leaves `/home/io/rotint` and user `io` in place
(the user's files / the deploy user). Idempotent — safe to re-run. Ordering
matters: migration first, then `./deploy.sh`, whose seed-once seeds `config.toml`
with defaults that match rotint's hardcoded values (WRC `ws://192.168.1.108/wsrotor`,
broker `tcp://192.168.1.50:1883`, user `hf`, GS-232 `0.0.0.0:7373`) and leaves the
migrated env file (the real password) untouched. wrc-rotator-bridge is network-only,
so the migration creates no udev rule and adds no serial group.

The unit is hardened like flexbridge (network-only): `ProtectSystem=strict`,
`PrivateDevices=true` (no serial devices), `RestrictAddressFamilies=AF_INET
AF_INET6` (covers the outbound WRC+MQTT and the inbound GS-232 TCP plus
PSTRotator UDP listens), `MemoryMax=256M`, `TasksMax=64`. There is no udev
rule and no `DeviceAllow` — the bridge makes outbound TCP connections plus two
inbound listen sockets. See `../docs/conventions/deployment.md`.

---

## Station model and shared conventions

Shared documentation lives in `../docs/` (this component is a subdirectory of the stationa monorepo).

| Document | Path |
|---|---|
| Station integration model (three-plane MQTT contract, §7.1 rotator slot) | `../docs/station-integration-model.md` |
| Config and secrets convention | `../docs/conventions/config-and-secrets.md` |
| Deployment convention | `../docs/conventions/deployment.md` |
| Canonical band/mode vocabulary (not used by a rotator) | `../docs/conventions/band-mode-reference.md` |

This project implements the `muehle/hf/rotator` slot and must conform to those
conventions. Component-specific schema lives in `docs/wrc-rotator-bridge-mqtt-api.md`.