# acombridge

A Go service that bridges an **ACOM 600S / 1200S linear amplifier** to the
station MQTT bus using the station integration model (slot `muehle/hf/pa`). It
reads the amplifier's proprietary serial protocol over a USB-serial adapter,
publishes a canonical PA state snapshot, and accepts `/cmd` intent to switch
operating mode and select the active band.

It is part of the Mühle station automation ecosystem; see the meta-repo at
`../stationa/` for the shared integration model and conventions.

## Requirements

- ACOM 600S or 1200S amplifier connected via a USB-serial adapter (Prolific, vendor `067b`)
- MQTT broker (e.g. Mosquitto at `192.168.1.50:1883`)
- (Optional) Home Assistant — discovery is now rendered by the standalone
  `hadiscovery` consumer from this bridge's `expose` block; the legacy embedded
  discovery is gated off by default (model §9)

## Build & run

```sh
go build ./cmd/acombridge                 # local build
./deploy.sh                               # cross-compile for the Pi and install as a systemd service
```

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `/etc/acombridge/config.toml` | Path to TOML configuration file |
| `-log.level` | (from config) | `debug` \| `info` \| `warn` \| `error` |
| `-debug` | `false` | Hex-dump all serial I/O to stderr |

The service auto-reconnects to the broker and reopens the serial port on
failure (exponential backoff), and re-enables amplifier telemetry when it
detects the amplifier has been switched off and back on (watchdog).

## Configuration

All non-secret settings live in a TOML file (`config.example.toml` is a fully
annotated starting point). The binary works without one — built-in defaults are
used when the file is absent. The MQTT password is **not** in the TOML; it is
loaded from the `ACOMBRIDGE_MQTT_PASSWORD` environment variable (systemd
`EnvironmentFile`), so it never appears in the unit file or process command line.

```sh
sudo mkdir -p /etc/acombridge
sudo cp config.example.toml /etc/acombridge/config.toml
# /etc/acombridge/acombridge.env  (0600):
#   ACOMBRIDGE_MQTT_PASSWORD="..."
```

| Key | Default | Description |
|-----|---------|-------------|
| `mqtt.broker` | `tcp://192.168.1.50:1883` | MQTT broker URL |
| `mqtt.site` / `station` / `slot` | `muehle` / `hf` / `pa` | Station-model slot addressing |
| `mqtt.location` | `bauwagen` | Physical location label (in `/meta`) |
| `mqtt.user` | `hf` | MQTT username |
| `mqtt.password` | _(empty — use EnvironmentFile)_ | MQTT password |
| `mqtt.discovery_prefix` | `homeassistant` | Legacy embedded HA discovery prefix |
| `mqtt.publish_ha_discovery` | `false` | Gate legacy embedded HA discovery (model §9) |
| `serial.port` | `/dev/serial/by-id/…Prolific…` | Serial port device path |
| `serial.avg_time_ms` | `300` | Forward-power moving-average window in ms |
| `device.model` / `serial` / `link` | `ACOM 1200S` / `""` / `serial` | Identity published in `/meta` |
| `host` | `shari` | Compute node (in `/meta`) |
| `log.level` | `info` | Log level |

## MQTT

acombridge publishes the station three-plane topics under `<site>/<station>/<slot>`:

```
muehle/hf/pa/meta      retained  birth certificate (capabilities + expose)
muehle/hf/pa/state     retained  live PA state JSON snapshot
muehle/hf/pa/status    retained  online | offline (LWT — the bridge, not the amp)
muehle/hf/pa/cmd       not retained  set_mode | set_band intent
```

### `/state`

Published on every telemetry frame. Canonical fields only — raw firmware strings
live in the `pa_state` diagnostic and `error` fields, never in `mode`/`keyed`/`fault`.

```json
{
  "ts":            "2026-07-10T18:30:00Z",
  "mode":          "operate",
  "band":          "20m",
  "keyed":         "tx",
  "fwd_power_w":   600,
  "rfl_power_w":   3,
  "temp_c":        42.1,
  "swr":           1.2,
  "fault":         "none",
  "pa_state":      "OPR/TX",
  "device_online": true,
  "error":         ""
}
```

| Field | Unit | Description |
|-------|------|-------------|
| `mode` | — | `operate` \| `standby` (canonical) |
| `band` | — | Canonical band label (`160m` … `6m`; no 60m on the ACOM 1200S) |
| `keyed` | — | `rx` \| `tx` \| `inhibited` (canonical) |
| `fwd_power_w` | W | Forward power, time-averaged |
| `rfl_power_w` | W | Reflected power |
| `temp_c` | °C | Heat-sink temperature |
| `swr` | ratio | Standing wave ratio |
| `fault` | — | `none` \| `swr` \| `temp` \| `reflected` \| `other` (canonical) |
| `pa_state` | — | Raw firmware mode (diagnostic): `OPR/RX`, `OPR/TX`, `STANDBY`, `OFF`, … |
| `device_online` | bool | `true` while the serial loop has data; `false` when the port is lost |
| `error` | — | Verbatim fault message when `fault != none` |

### `/cmd`

```json
{"action": "set_mode", "value": "operate"}
{"action": "set_mode", "value": "standby"}
{"action": "set_band", "value": "20m"}
```

Band changes walk the amplifier from its current band to the target using
next/prev serial commands (the protocol exposes only relative band changes).
See `docs/pa-mqtt-api.md` for the full contract and the firmware→canonical
mapping.

### Home Assistant discovery

Discovery is rendered by the standalone `hadiscovery` consumer from this
bridge's `expose` block in `/meta` (preferred, model §9). The legacy embedded
discovery is retained but gated behind `mqtt.publish_ha_discovery = false`
(default off); set it `true` only as a migration fallback.

## Protocol reference

`docs/ACOM A600S A1200S Serial Protocol V1.3.pdf` — the authoritative source
for frame layout and byte offsets used in `internal/acom/parser.go`.

## Station model and shared conventions

| Document | Path |
|---|---|
| Station integration model (§7.1 PA slot) | `../stationa/docs/station-integration-model.md` |
| Config and secrets convention | `../stationa/docs/conventions/config-and-secrets.md` |
| Deployment convention (serial addendum) | `../stationa/docs/conventions/deployment.md` |
| Canonical band/mode vocabulary | `../stationa/docs/conventions/band-mode-reference.md` |
| PA slot MQTT schema | `docs/pa-mqtt-api.md` |