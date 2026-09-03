# CLAUDE.md — ultrabridge

ultrabridge bridges a **UltraBeam RCU-06** antenna controller (serial) to MQTT, exposing
the `muehle/hf/ant-ctrl` slot on the station bus (canonical role `ant-ctrl`; the device
name lives in `/meta.device`, never in the address). It also serves a small web UI.

This slot is the *controller* for one antenna (the remotely-tunable Ultrabeam) — **not**
"the antenna." Physical antennas are passive resources selected by the `ant-switch`
actuator under the `antenna-select` reconciler; see the station integration model §3/§7.1.

---

## Commands

```bash
go build ./cmd/ultrabridge          # local binary
go test ./...                  # all tests
go test -race ./...            # with race detector
go test ./internal/mqtt/...    # MQTT client tests only
go vet ./...                   # vet
gofmt -s -w .                  # format

# Run locally with mock serial (no hardware needed)
go run ./cmd/ultrabridge -http 127.0.0.1:8080

# Deploy to shari
./deploy.sh
```

---

## Architecture

ultrabridge is a **read-write** bridge: it reads antenna state over serial, publishes it to
MQTT, and also subscribes to `/cmd` to move the antenna on request.

**Layers:**

| Layer | Package | Role |
|-------|---------|------|
| Serial transport | `internal/ub/transport` | Open serial port or mock; trace I/O for web UI |
| Controller | `internal/ub/service` | Issue RCU-06 commands, maintain `State` |
| MQTT client | `internal/mqtt` | Publish `meta`/`state`/`status`, subscribe `/cmd`, HA discovery |
| Web UI | `internal/web` | HTTP status page + debug log |
| Config | `internal/config` | TOML file, `Default()`, `Load()`, flag overrides |

**Poll loop** (`main.go`): every 2 seconds calls `ctrl.Refresh()`,
`ctrl.PollMotorStatus()`, then publishes to web and MQTT if state changed.

**`/cmd` handling:** The MQTT client subscribes to `<slot>/cmd` **at QoS 0** (a
persistent-session QoS-1 subscription lets the broker queue a backlog while the bridge
is offline and replay it on reconnect — the 2026-09-03 incident; powerseq subscribes
its own `/cmd` at QoS 0 for the same reason). On receipt it dispatches to the
controller (`frequency`, `direction`, `band`, `retract`; `mode` is accepted as a
deprecated alias for `direction`). **Every** command is one-shot: the retained `/cmd`
topic is cleared after the worker has acted on it (executed or rejected), so a command
normally cannot re-fire on the next (re)connect — best-effort: a connection drop in
the clear window (execution → clear publish) leaves it retained and replays it once.
A command published while the bridge is offline also replays once on reconnect
(retained delivery is QoS-independent). Commands may carry an RFC 3339 `ts`; one older
than 30 s (or from the future) is dropped before reaching the serial device. On an
initial connect failure the process exits non-zero — systemd's `Restart=on-failure`
crash-loops it back; the bridge must never run with its MQTT plane silently disabled.

---

## MQTT topics

```
muehle/hf/ant-ctrl/meta      retained  birth certificate
muehle/hf/ant-ctrl/state     retained  live JSON snapshot
muehle/hf/ant-ctrl/status    retained  online | offline (LWT)
muehle/hf/ant-ctrl/cmd       retained  one-shot command — cleared after execution
```

State fields: `ts`, `freq_hz` (Hz), `band`, `direction` (`forward`/`reverse`/`bidirectional`),
`moving` (bool), `device_online` (bool, `true` while the RCU-06 is reachable, `false` when
not — always present), `error` (string, omitempty).

`direction` is deliberately not called `mode` — on the station bus `mode` is the
canonical radio-mode vocabulary (`cw`/`usb`/…, integration model §4).

The RCU-06 uses kHz internally; ultrabridge multiplies by 1000 before publishing `freq_hz`.

See `ultrabeam-mqtt-api.md` for the full on-the-wire contract.

---

## Configuration

Config is TOML at `/etc/ultrabridge/config.toml` (0600, owned by the `ultrabridge` service user).
The MQTT password is stored in this file — never on the command line.

Key fields:
```toml
http_addr   = "0.0.0.0:8080"
serial_port = "/dev/serial/by-id/usb-FTDI_Dual_RS232-if00-port0"
baud        = 19200
location    = "bauwagen"   # published in /meta
host        = "shari"      # published in /meta

[mqtt]
broker           = "tcp://127.0.0.1:1883"
site             = "muehle"
station          = "hf"
slot             = "ant-ctrl"
discovery_prefix = "homeassistant"
user             = "hf"
password         = "..."
# client_id defaults to "<site>-<station>-<slot>"
```

Leave `serial_port` empty to run with the mock device (no hardware required).

See `../docs/conventions/config-and-secrets.md` for the full convention.

---

## Deployment

```bash
./deploy.sh      # cross-compile arm64, copy to shari, install systemd service
```

The script seeds `/etc/ultrabridge/config.toml` on first deploy only (seed-once). To change
settings after the first deploy, edit the file on shari directly.

```bash
ssh io@192.168.1.139 'journalctl -u ultrabridge -f'
ssh io@192.168.1.139 'sudo systemctl restart ultrabridge'
```

See `../docs/conventions/deployment.md` for the general pattern.

---

## Station model and shared conventions

Shared documentation lives in `../docs/` (this component is a subdirectory of the stationa monorepo).

| Document | Path |
|---|---|
| Station integration model (three-plane MQTT contract) | `../docs/station-integration-model.md` |
| Config and secrets convention | `../docs/conventions/config-and-secrets.md` |
| Deployment convention | `../docs/conventions/deployment.md` |
| Canonical band/mode vocabulary | `../docs/conventions/band-mode-reference.md` |

This project must conform to those conventions. Component-specific schema lives in
`ultrabeam-mqtt-api.md` in this repo.
