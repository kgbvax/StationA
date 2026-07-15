# CLAUDE.md — atr1k-tuner-bridge

atr1k-tuner-bridge bridges the **ATR-1000 ATU** (BTR-1000 / N7DDC family) to MQTT
using the station integration model (slot `muehle/hf/tuner`). It dials the
tuner's binary WebSocket status stream, publishes a canonical tuner state
snapshot (SWR, forward power, L/C relay state, in-line/bypass, settling, fault),
and dispatches `/cmd` intent (`set_inline`, `tune`) back to the tuner. It is
**read-write**. The ATU sits in the HF TX path; this slot does not drive TX
itself — the antenna-selection reconciler engages the ATU in-line for
non-resonant bands via the `tuner.set_inline` soft binding (integration model
§7.1, §10 residual; see `antennaselect` `[tuner_follow]`).

This bridge is the ATR-1000 / N7DDC family member; its name follows the
stationa bridge-naming convention `<devtag>-<function>-bridge`
(see `../stationa/docs/conventions/naming.md`).

---

## Commands

```bash
go build ./cmd/atr1k-tuner-bridge                 # local build
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o dist/atr1k-tuner-bridge-linux-arm64 ./cmd/atr1k-tuner-bridge   # Pi cross-compile
go test ./...                                     # unit tests (no network/hardware)
go test ./... -race                               # tests with race detector
go vet ./...                                      # vet
gofmt -s -w .                                     # fmt
./deploy.sh                                       # cross-compile, ship, install as hardened systemd service
```

Run a single test package:
```bash
go test ./internal/bridge/... -run TestHandleCommand
go test ./internal/tuner/...
```

CLI flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `/etc/atr1k-tuner-bridge/config.toml` | Path to TOML config file |
| `-log.level` | (from config) | `debug` \| `info` \| `warn` \| `error`; overrides config |
| `-debug` | `false` | Log ATR-1000 WebSocket I/O (frame hex) |

---

## Architecture

**Data flow:** ATR-1000 binary WebSocket → `internal/tuner` → `internal/bridge`
→ MQTT.

1. `cmd/atr1k-tuner-bridge/main.go` — flags, config load, signal ctx, MQTT
   connect+LWT, `/cmd` subscription + bounded worker, ATR-1000 WebSocket restart
   loop with exponential backoff; re-publishes `/meta` on every (re)connect.
2. `internal/tuner` — ATR-1000 binary WebSocket device: ctx-aware
   `DialContext`, read loop decoding meter/relay frames into canonical `State`,
   mutex-guarded command writes (`SetInline`/`Tune`), tune-timeout timer →
   fault, thread-safe `Snapshot`. The binary frame protocol lives in
   `protocol.go` (copied from the `bwflex` repo's tuner skeleton).
3. `internal/bridge` — canonical tuner state model + MQTT publishing: `/meta`
   (role `tuner`, capabilities + `expose`), retained `/state` snapshot with
   change-dedup, `/cmd` dispatch via a `Commander`.
4. `internal/config` — TOML config, flags, `ATR1K_TUNER_BRIDGE_*` env overrides.

**Restart loop** (`wsLoop`): dials the ATU, publishes meta, runs the read loop
until the WebSocket errors or ctx cancels, then marks the device offline and
backoffs-and-retries. `/status` stays `online` while the bridge retries — only
`/state.device_online` flips. The initial paho `Connect()` is made ctx-aware
(bridging `tok.Wait()` through a goroutine + `select` on `ctx.Done()`) so a
SIGTERM while the broker is unreachable can interrupt the connect — otherwise
systemd must SIGKILL after `TimeoutStopSec`.

**Concurrency:** `tuner.Device.writeMu` guards the WebSocket and all writes
(commands serialize on it). `tuner.Device.mu` guards the state snapshot and the
tune timer. `bridge.Bridge.mu` guards the canonical state and the dedup
snapshot. The paho `/cmd` handler runs in paho's goroutine and only enqueues
onto a bounded channel — it never blocks (a `tune` writes to the ATU). The tune
timer fires in its own goroutine and reaches state only through `mu`.

---

## MQTT topics

atr1k-tuner-bridge publishes to the station integration model topics:

```
muehle/hf/tuner/meta     retained  birth certificate (capabilities + expose)
muehle/hf/tuner/state    retained  live tuner state JSON snapshot
muehle/hf/tuner/status   retained  online | offline (LWT — the bridge, not the ATU)
muehle/hf/tuner/cmd      not retained  set_inline | tune intent (bus → bridge)
```

`/state` is a single retained JSON document. Canonical fields: `inline`
(bool), `swr` (ratio), `fwd` (watts), `l_uh` (µH), `c_pf` (pF), `settling`
(bool — a tune cycle is in progress), `fault` (string, omitempty),
`device_online`/`error`. A tuner carries **no** `freq_hz`/`band`/`mode` — those
are radio concerns. `/cmd` is **not retained**: a tune is a one-shot command;
re-applying a stale `tune` on restart could re-key the ATU unexpectedly. See
`docs/atr1k-tuner-bridge-mqtt-api.md` for the full on-the-wire contract.

---

## Configuration and secrets

Config is TOML (`/etc/atr1k-tuner-bridge/config.toml` by default, or
`-config <path>`). The MQTT password is **not** in the TOML — it is loaded from
an `EnvironmentFile` so it never appears in the unit file or process command
line:

```bash
# /etc/atr1k-tuner-bridge/atr1k-tuner-bridge.env  (0600, owned by atr1k-tuner-bridge user)
ATR1K_TUNER_BRIDGE_MQTT_PASSWORD=<password>
```

The systemd unit contains `EnvironmentFile=/etc/atr1k-tuner-bridge/atr1k-tuner-bridge.env`.
Env overrides: `ATR1K_TUNER_BRIDGE_MQTT_BROKER`/`_CLIENT_ID`/`_USER`/`_PASSWORD`/
`_SITE`/`_STATION`/`_SLOT`, `ATR1K_TUNER_BRIDGE_TUNER_URL`. The env prefix is
the dir name uppercased with hyphens → underscores (naming convention).

See `../stationa/docs/conventions/config-and-secrets.md` for the full convention.

---

## Deployment

Target: Raspberry Pi (`shari`, `192.168.1.139`, user `io`).

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
```

The unit is hardened like wrcrotorbridge (network-only): `ProtectSystem=strict`,
`PrivateDevices=true` (no serial devices), `RestrictAddressFamilies=AF_INET
AF_INET6` (covers the outbound ATU WS + MQTT), `MemoryMax=256M`, `TasksMax=64`.
There is no udev rule and no `DeviceAllow` — the bridge makes outbound TCP
connections only. See `../stationa/docs/conventions/deployment.md`.

---

## Station model and shared conventions

Shared documentation lives in `../stationa/docs/` (the stationa meta-repo,
cloned adjacent to this repo).

| Document | Path |
|---|---|
| Station integration model (three-plane MQTT contract, §7.1 tuner slot) | `../stationa/docs/station-integration-model.md` |
| Config and secrets convention | `../stationa/docs/conventions/config-and-secrets.md` |
| Deployment convention | `../stationa/docs/conventions/deployment.md` |
| Bridge-naming convention (`<devtag>-<function>-bridge`) | `../stationa/docs/conventions/naming.md` |
| Canonical band/mode vocabulary (not used by a tuner) | `../stationa/docs/conventions/band-mode-reference.md` |

This project implements the `muehle/hf/tuner` slot and must conform to those
conventions. Component-specific schema lives in
`docs/atr1k-tuner-bridge-mqtt-api.md`.