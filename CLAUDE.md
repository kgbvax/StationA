# CLAUDE.md — acombridge

acombridge bridges an **ACOM 600S/1200S linear amplifier** to MQTT using the
station integration model (slot `muehle/hf/pa`). It reads the amplifier's
proprietary serial protocol over a USB-serial adapter (Prolific, 9600 8N1),
publishes a canonical PA state snapshot, and dispatches `/cmd` intent
(`set_mode`, `set_band`) back to the amp. Unlike flexbridge it is **read-write**.

---

## Commands

```bash
go build ./cmd/acombridge                 # local build
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" \
  -o dist/acombridge-linux-arm64 ./cmd/acombridge   # Pi cross-compile
go test ./...                             # unit tests (no network/hardware)
go test ./... -race                       # tests with race detector
go vet ./...                              # vet
gofmt -s -w .                             # fmt
./deploy.sh                               # cross-compile, ship, install as hardened systemd service
```

Run a single test package:
```bash
go test ./internal/acom/...
go test ./internal/bridge/... -run TestState
```

CLI flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `/etc/acombridge/config.toml` | Path to TOML config file |
| `-log.level` | (from config) | `debug` \| `info` \| `warn` \| `error`; overrides config |
| `-debug` | `false` | Hex-dump all serial I/O to stderr |

---

## Architecture

**Data flow:** serial run loop → `internal/acom` → `internal/bridge` → MQTT.

1. `cmd/acombridge/main.go` — flags, config load, signal ctx, MQTT connect+LWT,
   `/cmd` subscription, serial restart loop with exponential backoff, watchdog.
2. `internal/acom` — serial protocol: open port, frame scan + checksum, ACK,
   enable-telemetry, 72-byte telemetry parser, forward-power averager, band
   navigation (next/prev walk), mode/band commands. Owns protocol-level state
   (raw mode for the watchdog, band index for navigation).
3. `internal/bridge` — canonical PA state model + MQTT publishing: `/meta`
   (capabilities + `expose`), retained `/state` snapshot, `/cmd` dispatch via a
   `Commander` interface, gated legacy HA discovery.
4. `internal/config` — TOML config, flags, `ACOMBRIDGE_*` env overrides.
5. `internal/ha` — legacy embedded HA discovery payload builders (gated off by
   default; slated for deletion once `hadiscovery` is proven, model §9).

**Restart loop** (`serialLoop`): opens the port, publishes meta, runs the
telemetry loop until the port errors / 30 s silence / ctx cancel, then marks
the device offline and backoffs-and-retries. `/status` stays `online` while the
bridge retries — only `/state.device_online` flips. **Watchdog** goroutine:
every 3 s, if the amp reports `OFF`, re-sends the enable-telemetry command so
the amp resumes streaming after a power cycle.

**Concurrency:** `acom.Device.mu` guards the serial port and all writes (ACK,
enable, mode/band commands) — fixes the original single-file's ACK-without-lock
race. `acom.Device.stateMu` guards mode/band/online. `bridge.Bridge.mu` guards
the canonical state and the discovery-once flag. The paho `/cmd` handler runs
in paho's goroutine and reaches the device only through the `Commander`
interface (mutex-guarded).

---

## MQTT topics

acombridge publishes to the station integration model topics:

```
muehle/hf/pa/meta      retained  birth certificate (capabilities + expose)
muehle/hf/pa/state     retained  live PA state JSON snapshot
muehle/hf/pa/status    retained  online | offline (LWT — the bridge, not the amp)
muehle/hf/pa/cmd       not retained  set_mode | set_band intent (bus → bridge)
```

`/state` is a single retained JSON document. Canonical fields: `mode`
(`operate`/`standby`), `band`, `keyed` (`rx`/`tx`/`inhibited`), `fwd_power_w`,
`rfl_power_w`, `temp_c`, `swr`, `fault` (`none`/`swr`/`temp`/`reflected`/`other`),
plus the raw diagnostic `pa_state` (firmware mode string) and `device_online`/
`error`. The raw firmware mode is **only** in `pa_state`/`error`, never in the
canonical `mode`/`keyed`/`fault` fields. See `docs/pa-mqtt-api.md` for the full
on-the-wire contract and the firmware→canonical mapping.

---

## Configuration and secrets

Config is TOML (`/etc/acombridge/config.toml` by default, or `-config <path>`).
The MQTT password is **not** in the TOML — it is loaded from an
`EnvironmentFile` so it never appears in the unit file or process command line:

```bash
# /etc/acombridge/acombridge.env  (0600, owned by acombridge user)
ACOMBRIDGE_MQTT_PASSWORD=<password>
```

The systemd unit contains `EnvironmentFile=/etc/acombridge/acombridge.env`.
Env overrides: `ACOMBRIDGE_MQTT_BROKER`/`_CLIENT_ID`/`_USER`/`_PASSWORD`/
`_SITE`/`_STATION`/`_SLOT`, `ACOMBRIDGE_SERIAL_PORT`.

See `../stationa/docs/conventions/config-and-secrets.md` for the full convention.

---

## Deployment

Target: Raspberry Pi (`shari`, `192.168.1.139`, user `io`).

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
```

The unit is hardened like flexbridge **minus `PrivateDevices`** (the bridge
must open `/dev/ttyUSB*`). It keeps `SupplementaryGroups=dialout`, the three
`DeviceAllow=char-tty{USB,ACM} rw`, and the Prolific (`067b`) udev rule that
pins the adapter's tty to the dialout group. See `../stationa/docs/conventions/deployment.md`
(serial addendum) for the serial-specific hardening requirements.

---

## Protocol reference

`docs/ACOM A600S A1200S Serial Protocol V1.3.pdf` — the authoritative source
for frame layout and byte offsets used in `internal/acom/parser.go`.

---

## Station model and shared conventions

Shared documentation lives in `../stationa/docs/` (the stationa meta-repo,
cloned adjacent to this repo).

| Document | Path |
|---|---|
| Station integration model (three-plane MQTT contract, §7.1 PA slot) | `../stationa/docs/station-integration-model.md` |
| Config and secrets convention | `../stationa/docs/conventions/config-and-secrets.md` |
| Deployment convention (serial addendum) | `../stationa/docs/conventions/deployment.md` |
| Canonical band/mode vocabulary | `../stationa/docs/conventions/band-mode-reference.md` |

This project implements the `muehle/hf/pa` slot and must conform to those
conventions. Component-specific schema lives in `docs/pa-mqtt-api.md`.