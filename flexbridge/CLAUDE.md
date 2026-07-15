# CLAUDE.md — flexbridge

flexbridge is a **read-only** bridge: it observes a FlexRadio 6000-series radio over the
SmartSDR TCP/IP API and UDP meter stream, and publishes state to MQTT. It never sends
commands to the radio.

---

## Commands

```bash
make build        # local binary -> bin/flexbridge
make pi           # cross-compile for Raspberry Pi arm64 -> bin/flexbridge-linux-arm64
make test         # unit tests (no network required)
make test-race    # tests with race detector
make vet          # go vet
make fmt          # gofmt -s -w .
make run          # build + run with config.example.toml
```

Run a single test package or test:
```bash
go test ./internal/flexradio/...
go test ./internal/bridge/... -run TestGate
```

---

## Architecture

**Data flows:**

1. **UDP discovery** (`flexradio.Discover`) — broadcast to `255.255.255.255:4992`,
   receives the radio's IP, serial, and model.
2. **TCP connection** (`flexradio.Client`) — port 4992 (SmartSDR TCP/IP API). On connect
   runs a handshake: sends `version`, sets local UDP port for meter streaming, subscribes
   to `slice/radio/interlock/atu/meter all`. After that, `Client.Run` blocks reading
   async status lines.
3. **UDP meter stream** (VITA-49) — the radio sends real-time meter datagrams at 10–20 fps
   to the port registered during handshake. Decoded in `flexradio.ParseVITA49` /
   `VITAPacket.MeterReadings`.

Two concurrent goroutines feed the bridge:
- TCP goroutine calls `Bridge.HandleStatus` (and `Bridge.HandleReply` for the one-shot meter list)
- UDP goroutine calls `Bridge.HandleMeterPacket`

`Bridge` (`internal/bridge/bridge.go`) owns all shared state under `sync.RWMutex`.

**Reconnect loop** (`cmd/flexbridge/main.go:radioLoop`): connects, runs until disconnect, then
exponential-backoffs and retries. Calls `Bridge.Reset()` between attempts to clear stale
state and force republish on reconnect.

---

## MQTT topics

flexbridge publishes to the station integration model topics:

```
muehle/hf/radio/meta      retained  birth certificate (capabilities JSON)
muehle/hf/radio/state     retained  live state JSON snapshot
muehle/hf/radio/status    retained  online | offline (LWT)
```

The `site`, `station`, and `slot` values are configurable via `config.toml`.

**State is a single retained JSON document** (not per-field topics). Fields:
`ts`, `freq_hz` (Hz integer), `band` (derived), `mode` (canonical: `cw`/`usb`/`lsb`/`am`/`fm`/`data`),
`tx` (`rx`/`tx`), `tuning` (bool), `drive` (0–100).

**flexbridge is read-only** — it publishes no `/cmd` topic and does not subscribe to commands.

See `docs/radio2mqtt-schema.md` for the full on-the-wire contract.

---

## Key packages

| Package | Role |
|---|---|
| `internal/flexradio` | Protocol: discovery, TCP client, frame parser, VITA-49 decoder, meter registry, status parsers, band lookup |
| `internal/bridge` | Radio events → MQTT: state tracking, discovery payloads |
| `internal/ha` | Home Assistant discovery payload builders and topic helpers |
| `internal/config` | TOML config, flags, `FLEXBRIDGE_*` env overrides |

---

## Configuration and secrets

Config is TOML (`/etc/flexbridge/config.toml` by default, or `-config <path>`). The MQTT
password is **not** in the TOML — it is loaded from an `EnvironmentFile` so it never
appears in the unit file or process command line:

```bash
# /etc/flexbridge/flexbridge.env  (0600, owned by flexbridge user)
FLEXBRIDGE_MQTT_PASSWORD=<password>
```

The systemd unit contains `EnvironmentFile=/etc/flexbridge/flexbridge.env`.

See `../docs/conventions/config-and-secrets.md` for the full convention.

---

## Deployment

Target: Raspberry Pi (`shari`, `192.168.1.139`, user `io`).

```bash
./deploy.sh              # cross-compile, ship, install as a hardened systemd service
```

See `../docs/conventions/deployment.md` for the general pattern and systemd
hardening requirements.

---

## Testing notes

Tests in `internal/flexradio/status_real_test.go` capture real FLEX-8400 firmware output
formats observed in production. These are regression guards for parser mismatches that
broke the live deployment — keep them faithful to actual firmware output.

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
`docs/radio2mqtt-schema.md` in this repo.
