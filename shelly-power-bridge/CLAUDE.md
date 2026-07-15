# CLAUDE.md — shelly-power-bridge

shelly-power-bridge fronts **Shelly Gen2+ smart plugs** (Plus / Mini / Pro …)
that speak MQTT natively, translating them into the station integration-model
**`power`** slots. It is a **compound bridge**: one process fronts N Shelies,
each becoming a site-level power slot at `muehle/power/<slot>/{meta,state,status,cmd}`.
Each `[[slot]]` runs its own paho client with its own LWT, so a process death
takes every fronted slot offline with no stale-online gap.

The Shelies today are the **station master mains** (`power/master`) and the
**13.8 V PSU** (`power/psu-13v8`). The PSU is a site-level DC rail feeding both
HF and UHF (radio, tuner, ant-ctrl, ant-switch, rotator, the M5 Stamps), which
is why the power layer sits at site level rather than under one station.
Historically the Shelies were controlled only through Home Assistant — an
HA-as-control-path anti-pattern (integration model §9); this bridge brings them
onto the canonical bus so the sequencer (`powerseq`) and operators drive them
directly and HA becomes one writer among many.

The bridge is **read-write**: it observes the Shelly native relay state and
commands the relay over the Gen2+ RPC-over-MQTT topic. Its name follows the
stationa bridge-naming convention `<devtag>-<function>-bridge`
(see `../stationa/docs/conventions/naming.md`).

---

## Commands

```bash
go build ./cmd/shelly-power-bridge                 # local build
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o dist/shelly-power-bridge-linux-arm64 ./cmd/shelly-power-bridge   # Pi cross-compile
go test ./...                                     # unit tests (no network/hardware)
go test ./... -race                               # tests with race detector
go vet ./...                                      # vet
gofmt -s -w .                                     # fmt
./deploy.sh                                       # cross-compile, ship, install as hardened systemd service
```

Run a single test package:
```bash
go test ./internal/bridge/... -run TestHandleCommand
go test ./internal/shelly/...
```

CLI flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `/etc/shelly-power-bridge/config.toml` | Path to TOML config file |
| `-log.level` | (from config) | `debug` \| `info` \| `warn` \| `error`; overrides config |

---

## Architecture

**Data flow:** Shelly Gen2+ native MQTT → `internal/shelly` (wire codec) →
`internal/bridge` (canonical state) → MQTT.

1. `cmd/shelly-power-bridge/main.go` — flags, config load, signal ctx, one
   runtime goroutine **per `[[slot]]`**. Each `runSlot` owns its own paho client
   (with the slot LWT), a single `jobs` worker goroutine, and a heartbeat
   staleness watcher. On (re)connect it publishes online + `/meta` and
   subscribes the Shelly native topics + the canonical `/cmd`. The initial
   paho `Connect()` is ctx-aware (a `tok.Wait()` bridged through a goroutine +
   `select` on `ctx.Done()`) so a SIGTERM while the broker is unreachable can
   interrupt the connect — otherwise systemd must SIGKILL after
   `TimeoutStopSec`.
2. `internal/shelly` — Gen2+ wire codec: `StatusTopic`/`RPCTopic`, `ParseStatus`
   (decodes `<id>/status/switch:0` `output` → canonical `on`/`off`),
   `SwitchSet` (builds the `<id>/rpc` `Switch.Set` payload). No MQTT client.
3. `internal/bridge` — canonical `power` slot model + MQTT publishing: `/meta`
   (role `power`, capabilities `fail_safe` + `feeds`, `expose` `power`
   writable), retained `/state` snapshot with change-dedup, `/cmd` dispatch via
   a `Commander`. `device_online` mirrors Shelly reachability.
4. `internal/config` — TOML config (one `[[slot]]` per Shelly), flags,
   `SHELLY_POWER_BRIDGE_MQTT_*` env overrides.

**Concurrency:** the paho message handlers run on paho's goroutine and **only
enqueue** closures onto a per-slot bounded `jobs` channel — they never publish
(a paho handler must not call a blocking `Publish`; the stationa memory records
a live hadiscovery deadlock). The single `jobs` worker serializes all bridge
state mutation + publishing for the slot. The shelly RPC `Publish` (from
`HandleCommand`) runs in that worker, not on a paho handler, so blocking on the
token is safe. `bridge.SlotBridge.mu` guards the canonical state + dedup
snapshot. The heartbeat watcher enqueues `MarkDeviceOffline` when the Shelly's
`<id>/online` heartbeat is lost.

**Compound-device / multi-client note:** MQTT 3.1.1 allows only one Will per
client, so the compound bridge uses **one paho client per slot** (each with its
own LWT on its own `<slot>/status`). A process death fires every slot's will at
once — no stale-online gap. Each client's id defaults to
`<site>-<station>-<slot>`.

---

## MQTT topics

shelly-power-bridge publishes to the station integration model topics, per
fronted Shelly (`<slot>` = `master` | `psu-13v8` | …):

```
muehle/power/<slot>/meta     retained  birth certificate (capabilities + expose)
muehle/power/<slot>/state    retained  live power state JSON snapshot
muehle/power/<slot>/status   retained  online | offline (LWT — the bridge, not the Shelly)
muehle/power/<slot>/cmd      retained   set_power intent (bus → bridge); self-healing §8
```

`/state` is a single retained JSON document: `power` (`on`/`off`, the **actual**
relay position read back from the Shelly), `device_online`, `error,omitempty`,
plus an RFC3339 `ts`. `/cmd` is **retained** (the self-healing steady-state
exception, model §8): a power slot holds an on/off intent, and the broker
replays the last command on every reconnect so the bridge re-applies it. See
`docs/shelly-power-bridge-mqtt-api.md` for the full on-the-wire contract,
including the Shelly native topics it subscribes (`<id>/status/switch:0`,
`<id>/online`) and commands (`<id>/rpc`).

---

## Configuration and secrets

Config is TOML (`/etc/shelly-power-bridge/config.toml` by default, or
`-config <path>`). It holds one `[[slot]]` per Shelly with its `shelly_id` (the
Gen2 MQTT prefix), `device_model`/`device_serial`, `fail_safe`, and `feeds`.
The MQTT password is **not** in the TOML — it is loaded from an
`EnvironmentFile` so it never appears in the unit file or process command line:

```bash
# /etc/shelly-power-bridge/shelly-power-bridge.env  (0600, owned by shelly-power-bridge user)
SHELLY_POWER_BRIDGE_MQTT_PASSWORD=<password>
```

The systemd unit contains `EnvironmentFile=/etc/shelly-power-bridge/shelly-power-bridge.env`.
Env overrides: `SHELLY_POWER_BRIDGE_MQTT_BROKER`/`_CLIENT_ID`/`_USER`/`_PASSWORD`/
`_SITE`. Per-slot values come from the TOML only. The env prefix is the dir name
uppercased with hyphens → underscores (naming convention).

See `../stationa/docs/conventions/config-and-secrets.md` for the full convention.

---

## Deployment

Target: Raspberry Pi (`shari`, `192.168.1.139`, user `io`).

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
# Set the real Shelly device ids before first deploy:
MASTER_SHELLY_ID=shellyplus1pm-<mac> PSU_SHELLY_ID=shellyplus1pm-<mac> ./deploy.sh
```

The unit is hardened like atr1k-tuner-bridge (network-only):
`ProtectSystem=strict`, `PrivateDevices=true` (no serial devices),
`RestrictAddressFamilies=AF_INET AF_INET6` (covers the outbound MQTT),
`MemoryMax=256M`, `TasksMax=64`. There is no udev rule and no `DeviceAllow` —
the bridge is a broker client only. See `../stationa/docs/conventions/deployment.md`.

---

## Station model and shared conventions

Shared documentation lives in `../stationa/docs/` (the stationa meta-repo,
cloned adjacent to this repo).

| Document | Path |
|---|---|
| Station integration model (three-plane MQTT contract, §4 `power` role, §7.0 power slots) | `../stationa/docs/station-integration-model.md` |
| Config and secrets convention | `../stationa/docs/conventions/config-and-secrets.md` |
| Deployment convention | `../stationa/docs/conventions/deployment.md` |
| Bridge-naming convention (`<devtag>-<function>-bridge`) | `../stationa/docs/conventions/naming.md` |

This project implements the `muehle/power/*` slots and must conform to those
conventions. Component-specific schema lives in
`docs/shelly-power-bridge-mqtt-api.md`.