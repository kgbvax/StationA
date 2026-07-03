# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build        # local binary -> bin/flexbridge
make pi           # cross-compile for Raspberry Pi arm64 -> bin/flexbridge-linux-arm64
make test         # unit tests (no network required)
make test-race    # tests with race detector
make vet          # go vet
make fmt          # gofmt -s -w .
make run          # build + run with deploy/config.example.toml
```

Run a single test package or test:
```bash
go test ./internal/flexradio/...
go test ./internal/bridge/... -run TestGate
```

## Architecture

flexbridge is a read-only bridge: it never sends commands to the radio — only listens and forwards state to MQTT.

**Data flows:**

1. **UDP discovery** (`flexradio.Discover`) — broadcast to `255.255.255.255:4992`, receives the radio's IP, serial, and model.
2. **TCP connection** (`flexradio.Client`) — port 4992 (SmartSDR TCP/IP API). On connect it runs a handshake: sends `version`, sets the local UDP port for meter streaming, subscribes to `slice/radio/interlock/atu/meter all`. After that, `Client.Run` blocks reading async status lines.
3. **UDP meter stream** (VITA-49) — the radio sends real-time meter datagrams at 10–20 fps to the port registered during handshake. Decoded in `flexradio.ParseVITA49` / `VITAPacket.MeterReadings`.

**Two concurrent goroutines feed the bridge:**
- TCP goroutine calls `Bridge.HandleStatus` (and `Bridge.HandleReply` for the one-shot meter list)
- UDP goroutine calls `Bridge.HandleMeterPacket`

`Bridge` (`internal/bridge/bridge.go`) owns all shared state under `sync.RWMutex`. It deduplicates and throttles meter publishes via `Gate` (`throttle.go`), which gates on per-group minimum intervals and per-unit deadbands.

**TX gating**: TX-chain and audio meters are suppressed while `interlock.Transmitting == false` to avoid publishing full-scale garbage during receive.

**Reconnect loop** (`main.go:radioLoop`): connects, runs until disconnect, then exponential-backoffs and retries. Calls `Bridge.Reset()` between attempts to clear stale state and force republish on reconnect.

**MQTT topics:**
- `flexbridge/<serial>/status` — bridge LWT (online/offline)
- `flexbridge/<serial>/state/...` — retained status fields (frequency, mode, PTT, ATU, etc.)
- `flexbridge/<serial>/meter/<group>/[<slice>/]<object_id>` — non-retained live meter values

**Home Assistant discovery** is published once per connect cycle by `Bridge.PublishDiscovery`. Per-slice entities are published lazily when slices first appear via `Bridge.MaybePublishSliceDiscovery` (slices are dynamic).

## Key packages

| Package | Role |
|---|---|
| `internal/flexradio` | Protocol: discovery, TCP client, frame parser, VITA-49 decoder, meter registry, status parsers, band lookup |
| `internal/bridge` | Radio events → MQTT: state tracking, throttle/dedup gate, discovery payloads |
| `internal/ha` | Home Assistant discovery payload builders and topic helpers |
| `internal/config` | TOML config, flags, `FLEXBRIDGE_*` env overrides |

## Configuration and secrets

Config is TOML (`/etc/flexbridge/config.toml` by default, or `-config <path>`). Secrets can be passed via `FLEXBRIDGE_MQTT_PASSWORD` (and other `FLEXBRIDGE_*` env vars) from a systemd `EnvironmentFile` instead of hard-coding in the TOML.

## Deployment

The deploy target is a Raspberry Pi running as a hardened systemd service (see `deploy/`). Build with `make pi`, copy binary + `deploy/` to the Pi, run `deploy/install.sh`.

## Testing notes

Tests in `internal/flexradio/status_real_test.go` capture real FLEX-8400 firmware output formats observed in production. These are regression guards for parser mismatches that broke the live deployment — keep them faithful to actual firmware output.

The `Gate` clock is injectable via `SetNow` for deterministic throttle tests.
